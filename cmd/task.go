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

	"github.com/davidbirdsong/shoal/pkg/cluster"
	"github.com/davidbirdsong/shoal/pkg/node"
	shoalproto "github.com/davidbirdsong/shoal/pkg/proto"
	"github.com/hashicorp/serf/serf"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

// task flags
var (
	taskJoin         string
	taskSnapshotDir  string
	taskDrainTimeout time.Duration
	taskWorker       []string
)

func init() {
	f := taskCmd.Flags()
	f.StringVar(&taskJoin, "join", "", "sidecar address to join (required)")
	f.StringVar(&taskSnapshotDir, "snapshot-dir", "./serf-snapshots", "serf snapshot directory")
	f.DurationVar(&taskDrainTimeout, "drain-timeout", 30*time.Second, "max time to drain before hard exit")
}

// taskRunner holds live state across the three lifecycle phases.
type taskRunner struct {
	log          zerolog.Logger
	boundAddr    *net.TCPAddr
	worker       *exec.Cmd
	node         *node.Node
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

// startTask binds an ephemeral port, launches the worker subprocess, creates the serf
// node, and advances it to the READY state.
func startTask(_ context.Context, log zerolog.Logger) (*taskRunner, error) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("bind ephemeral port: %w", err)
	}
	boundAddr := ln.Addr().(*net.TCPAddr)
	log.Info().Int("port", boundAddr.Port).Msg("bound ephemeral port")

	// TODO: pass fd directly so worker can call accept() without knowing the port.
	// For now the port is passed via env and the fd is available as fd 3.
	lnFile, err := ln.(*net.TCPListener).File()
	if err != nil {
		return nil, fmt.Errorf("get listener fd: %w", err)
	}
	ln.Close()

	worker := exec.Command(taskWorker[0], taskWorker[1:]...)
	worker.Stdout = os.Stdout
	worker.Stderr = os.Stderr
	worker.ExtraFiles = []*os.File{lnFile} // fd 3
	worker.Env = append(os.Environ(),
		fmt.Sprintf("SHOAL_PORT=%d", boundAddr.Port),
		fmt.Sprintf("SHOAL_ADDR=%s", boundAddr.IP),
		"SHOAL_LISTENER_FD=3",
	)
	if err := worker.Start(); err != nil {
		return nil, fmt.Errorf("start worker: %w", err)
	}
	lnFile.Close()
	log.Info().Int("pid", worker.Process.Pid).Msg("worker started")

	// TODO: replace with connect-probe loop or pipe-based ready signal.
	time.Sleep(time.Second)
	log.Info().Msg("worker assumed ready (stub)")

	n, err := node.New(node.NodeConfig{
		Role:        cluster.RoleTask,
		Tags:        map[string]string{cluster.TagKeyState: cluster.StateStarting},
		SnapshotDir: taskSnapshotDir,
		JoinAddrs:   []string{taskJoin},
		Logger:      log,
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
		log:          log,
		boundAddr:    boundAddr,
		worker:       worker,
		node:         n,
		drainTimeout: taskDrainTimeout,
	}, nil
}

// announce sends an AnnounceRequest to all sidecar nodes and logs their responses.
func (t *taskRunner) announce() {
	payload, err := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
		Addr:  t.boundAddr.IP.String(),
		Port:  uint32(t.boundAddr.Port),
		State: cluster.StateReady,
	})
	if err != nil {
		t.log.Error().Err(err).Msg("announce: marshal failed")
		return
	}
	resp, err := t.node.Serf.Query(cluster.QueryAnnounce, payload, &serf.QueryParam{
		FilterTags: map[string]string{cluster.TagKeyRole: cluster.RoleSidecar},
	})
	if err != nil {
		t.log.Error().Err(err).Msg("announce: query failed")
		return
	}
	for r := range resp.ResponseCh() {
		ack, err := shoalproto.UnmarshalAnnounceResponse(r.Payload)
		if err != nil {
			t.log.Error().Err(err).Str("from", r.From).Msg("announce: unmarshal response failed")
			continue
		}
		if ack.Accepted {
			t.log.Info().Str("backend", ack.BackendKey).Msg("registered with sidecar")
		} else {
			t.log.Warn().Str("from", r.From).Str("reason", ack.Reason).Msg("announce rejected")
		}
	}
}

// solicited responds to a QuerySolicit from a sidecar with our current announce payload.
func (t *taskRunner) solicited(q *serf.Query) error {
	payload, err := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
		Addr:  t.boundAddr.IP.String(),
		Port:  uint32(t.boundAddr.Port),
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
		t.log.Err(ctx.Err()).Msg("received signal, draining")
	case err := <-workerDone:
		t.log.Err(err).Msg("worker exited")
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
				t.log.Info().Str("from", r.From).Msg("sidecar acknowledged depart")
				return nil
			}
		case <-timer.C:
			t.log.Warn().Msg("drain timeout reached, forcing exit")
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
		Addr:           t.boundAddr.IP.String(),
		Port:           uint32(t.boundAddr.Port),
		TimeoutSeconds: uint32(t.drainTimeout.Seconds()),
	})

	if err := t.waitDepart(payload); err != nil {
		t.log.Error().Err(err).Msg("depart query failed")
	}

	if t.worker.ProcessState == nil {
		t.worker.Process.Signal(syscall.SIGTERM) //nolint:errcheck
	}

	<-t.nodeErr
	t.log.Info().Msg("clean shutdown complete")
}

func runTask(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	log := zerolog.Ctx(ctx).With().Str("role", "task").Logger()

	if taskJoin == "" {
		return fmt.Errorf("--join is required")
	}
	if len(taskWorker) == 0 {
		return fmt.Errorf("--worker is required")
	}

	t, err := startTask(ctx, log)
	if err != nil {
		return err
	}

	t.run(ctx)
	t.drain(ctx)
	return nil
}
