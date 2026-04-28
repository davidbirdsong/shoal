package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/davidbirdsong/shoal/pkg/child"
	"github.com/davidbirdsong/shoal/pkg/cluster"
	"github.com/davidbirdsong/shoal/pkg/node"
	shoalproto "github.com/davidbirdsong/shoal/pkg/proto"
	"github.com/hashicorp/serf/serf"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

// task flags
var (
	taskSnapshotDir  string
	taskDrainTimeout time.Duration
	taskWorker       []string

	cmdVarJoinAddr []string
)

func init() {
	f := taskCmd.Flags()
	f.StringVar(&taskSnapshotDir, "snapshot-dir", "./serf-snapshots", "serf snapshot directory")
	f.DurationVar(&taskDrainTimeout, "drain-timeout", 30*time.Second, "max time to drain before hard exit")
	f.StringArrayVar(&cmdVarJoinAddr, "join", []string{""}, "specify join addresses")
}

// taskRunner holds live state across the three lifecycle phases.
type taskRunner struct {
	logger       zerolog.Logger
	worker       *exec.Cmd
	node         *node.Node
	port         int
	drainTimeout time.Duration
	nodeErr      <-chan error // result of n.Run goroutine; set in run, consumed in drain
}

const (
	tmplAddr   = "{addr}"
	tmplPort   = "{port}"
	tmplFdPort = "{fdport}"
)

var knownPlaceholders = [3]string{tmplAddr, tmplPort, tmplFdPort}

type cmdLineInterp struct {
	port   int
	fdport int
	addr   string
}
type tokenTransform func(string) (string, error)

func (c cmdLineInterp) applyFormat(tmpl string) (string, error) {
	if err := validateFormat(tmpl); err != nil {
		return "", err
	}
	return strings.NewReplacer(
		tmplAddr, c.addr,
		tmplPort, fmt.Sprintf("%d", c.port),
		tmplFdPort, fmt.Sprintf("%d", c.fdport),
	).Replace(tmpl), nil
}

func interPolate(args []string, t tokenTransform) ([]string, error) {
	new := make([]string, len(args))
	var e error
	for i := 0; i < len(args); i++ {
		new[i], e = t(args[i])
		if e != nil {
			return []string{}, e
		}

	}
	return new, nil
}

func validateFormat(tmpl string) error {
	// find all {word} tokens and verify each is known
	for _, m := range regexp.MustCompile(`\{[^}]+\}`).FindAllString(tmpl, -1) {
		if !slices.Contains(knownPlaceholders[:], m) {
			return fmt.Errorf("unknown placeholder %q in format string", m)
		}
	}
	return nil
}

type startConfig struct {
	taskArgs []string
	joinArgs []string
}

type boundListener struct {
	Files []*os.File
	Fd    int
	Addr  *net.TCPAddr
}

func getListenerFiles(logger zerolog.Logger) (boundListener, error) {
	var bL boundListener
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		logger.Err(err).
			Msg("failed to bind ephemeral port")
		return bL, err
	}

	defer ln.Close()
	bL.Addr = ln.Addr().(*net.TCPAddr)
	logger.Debug().Int("port", bL.Addr.Port).Msg("bound ephemeral port")

	// TODO: pass fd directly so worker can call accept() without knowing the port.
	// For now the port is passed via env and the fd is available as fd 3.
	lnFile, err := ln.(*net.TCPListener).File()
	if err != nil {
		logger.Err(err).Msg("getting listener fd")
		return bL, err
	}
	bL.Files = []*os.File{lnFile}
	bL.Fd = int(lnFile.Fd())
	return bL, nil
}

// startTask binds an ephemeral port, launches the worker subprocess, creates the serf
// node, and advances it to the READY state.
func startTask(ctx context.Context, cfg startConfig) (*taskRunner, error) {
	logger := *zerolog.Ctx(ctx)

	bL, err := getListenerFiles(logger)
	if err != nil {
		return nil, err
	}

	args, err := interPolate(cfg.taskArgs,
		cmdLineInterp{fdport: bL.Fd}.applyFormat,
	)
	if err != nil {
		return nil, err
	}

	worker := exec.Command(args[0], args[1:]...)
	worker.Stdout = os.Stdout
	worker.Stderr = os.Stderr
	worker.ExtraFiles = bL.Files
	worker.Env = os.Environ()
	/*
		worker.Env = append(os.Environ(),
			fmt.Sprintf("SHOAL_PORT=%d", boundAddr.Port),
			fmt.Sprintf("SHOAL_ADDR=%s", boundAddr.IP),
			"SHOAL_LISTENER_FD=3",
		)
	*/
	if err := worker.Start(); err != nil {
		return nil, fmt.Errorf("start worker: %w", err)
	}
	logger.Info().Int("pid", worker.Process.Pid).Msg("worker started")

	// TODO: replace with connect-probe loop or pipe-based ready signal.
	time.Sleep(time.Second)
	logger.Info().Msg("worker assumed ready (stub)")

	n, err := node.New(node.NodeConfig{
		Role:        cluster.RoleTask,
		Tags:        map[string]string{cluster.TagKeyState: cluster.StateStarting},
		SnapshotDir: taskSnapshotDir,
		JoinAddrs:   []string{"foo"},
		Logger:      logger,
	})
	if err != nil {
		worker.Process.Kill() //nolint:errcheck
		return nil, fmt.Errorf("create node: %w", err)
	}

	if err := n.Serf.SetTags(map[string]string{
		cluster.TagKeyRole:  cluster.RoleTask,
		cluster.TagKeyState: cluster.StateReady,
	}); err != nil {
		return nil, fmt.Errorf("set tags ready: %w", err)
	}

	return &taskRunner{
		logger:       logger,
		worker:       worker,
		node:         n,
		port:         bL.Addr.Port,
		drainTimeout: taskDrainTimeout,
	}, nil
}

// announce sends an AnnounceRequest to all sidecar nodes and logs their responses.
func (t *taskRunner) announce() {
	payload, err := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
		Port:  uint32(t.port),
		State: cluster.StateReady,
	})
	if err != nil {
		t.logger.Error().Err(err).Msg("announce: marshal failed")
		return
	}
	resp, err := t.node.Serf.Query(cluster.QueryAnnounce, payload, &serf.QueryParam{
		FilterTags: map[string]string{cluster.TagKeyRole: cluster.RoleSidecar},
	})
	if err != nil {
		t.logger.Error().Err(err).Msg("announce: query failed")
		return
	}
	for r := range resp.ResponseCh() {
		ack, err := shoalproto.UnmarshalAnnounceResponse(r.Payload)
		if err != nil {
			t.logger.Error().Err(err).Str("from", r.From).Msg("announce: unmarshal response failed")
			continue
		}
		if ack.Accepted {
			t.logger.Info().Str("backend", ack.BackendKey).Msg("registered with sidecar")
		} else {
			t.logger.Warn().Str("from", r.From).Str("reason", ack.Reason).Msg("announce rejected")
		}
	}
}

// solicited responds to a QuerySolicit from a sidecar with our current announce payload.
func (t *taskRunner) solicited(q *serf.Query) error {
	payload, err := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
		Port:  uint32(t.port),
		State: cluster.StateReady,
	})
	if err != nil {
		return fmt.Errorf("solicit: marshal: %w", err)
	}
	return q.Respond(payload)
}

// run starts the serf event loop, announces to sidecars, and blocks until the worker
// exits or the context is cancelled.
func (t *taskRunner) run(ctx context.Context) {
	handlers := node.EventHandlers{
		OnQuery: map[string]func(*serf.Query) error{
			cluster.QuerySolicit: t.solicited,
		},
	}

	nodeErr := make(chan error, 1)
	go func() { nodeErr <- t.node.Run(ctx, handlers) }()
	t.nodeErr = nodeErr

	t.announce()

	workerDone := make(chan error, 1)
	go func() { workerDone <- t.worker.Wait() }()

	select {
	case <-ctx.Done():
		t.logger.Err(ctx.Err()).Msg("received signal, draining")
	case err := <-workerDone:
		t.logger.Err(err).Msg("worker exited")
	}
}

// waitDepart sends the depart query and waits for a sidecar ack or the drain timeout.
func (t *taskRunner) waitDepart(payload []byte) error {
	resp, err := t.node.Serf.Query(cluster.QueryDepart, payload, &serf.QueryParam{
		FilterTags: map[string]string{cluster.TagKeyRole: cluster.RoleSidecar},
	})
	if err != nil {
		return err
	}

	timer := time.NewTimer(t.drainTimeout)
	defer timer.Stop()

	for {
		select {
		case r, ok := <-resp.ResponseCh():
			if !ok {
				return nil
			}
			ack, _ := shoalproto.UnmarshalDepartResponse(r.Payload)
			if ack != nil && ack.Accepted {
				t.logger.Info().Str("from", r.From).Msg("sidecar acknowledged depart")
				return nil
			}
		case <-timer.C:
			t.logger.Warn().Msg("drain timeout reached, forcing exit")
			return nil
		}
	}
}

// drain sets DRAINING state, sends the depart query, signals the worker, and waits
// for the serf node to shut down.
func (t *taskRunner) drain(_ context.Context) {
	t.node.Serf.SetTags(map[string]string{ //nolint:errcheck
		cluster.TagKeyRole:  cluster.RoleTask,
		cluster.TagKeyState: cluster.StateDraining,
	})

	payload, _ := shoalproto.MarshalDepartRequest(&shoalproto.DepartRequest{
		Port:           uint32(t.port),
		TimeoutSeconds: uint32(t.drainTimeout.Seconds()),
	})

	if err := t.waitDepart(payload); err != nil {
		t.logger.Error().Err(err).Msg("depart query failed")
	}

	if t.worker.ProcessState == nil {
		t.worker.Process.Signal(syscall.SIGTERM) //nolint:errcheck
	}

	<-t.nodeErr
	t.logger.Info().Msg("clean shutdown complete")
}

func runTask(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	log := zerolog.Ctx(ctx).With().Str("role", "task").Logger()

	var err error
	sc := startConfig{}
	sc.taskArgs, err = child.ArgsFromCobra(cmd, args)
	if err != nil {
		log.Fatal().Err(err).Msg("failed extracing worker args")
		return nil
	}
	sc.joinArgs = cmdVarJoinAddr

	t, err := startTask(ctx, sc)
	if err != nil {
		log.Fatal().Err(err).Msg("failed starting task")
		return err
	}

	t.run(ctx)
	t.drain(ctx)
	return nil
}
