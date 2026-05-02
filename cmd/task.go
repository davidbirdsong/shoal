package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
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
	taskBackend      string

	cmdVarJoinAddr []string
)

func init() {
	f := taskCmd.Flags()
	f.StringVar(&taskSnapshotDir, "snapshot-dir", "./serf-snapshots", "serf snapshot directory")
	f.DurationVar(&taskDrainTimeout, "drain-timeout", 30*time.Second, "max time to drain before hard exit")
	// TODO: check f.StringSliceVar and compare
	f.StringArrayVar(&cmdVarJoinAddr, "join", []string{""}, "specify join addresses")
	f.StringVar(&taskBackend, "backend", "<none_backend>", "backend to register in haproxy")
}

// taskRunner holds live state across the three lifecycle phases.
type taskRunner struct {
	logger       zerolog.Logger
	worker       *exec.Cmd
	node         *node.Node
	port         int
	backend      string
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
	backend  string
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
	bgTail := pipeWorkers(worker, logger)

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
	bgTail()
	defer worker.Wait()

	logger.Info().Int("pid", worker.Process.Pid).Msg("worker started")
	mustHostname := func() string {
		hostname, err := os.Hostname()
		if err != nil {
			panic(err)
		}
		return hostname
	}

	// TODO: replace with connect-probe loop or pipe-based ready signal.
	time.Sleep(time.Second)
	logger.Info().Msg("worker assumed ready (stub)")
	nodeCfg := node.NodeConfig{
		NodeName: fmt.Sprintf("%s-%x", mustHostname(), rand.Uint32()&0xfffff), // 5 hex chars
		Role:     cluster.RoleTask,
		Tags:     map[string]string{cluster.TagKeyState: cluster.StateStarting},
		// SnapshotDir: taskSnapshotDir,
		JoinAddrs: cfg.joinArgs,
		Logger:    logger,
	}

	n, err := makeNewNode(nodeCfg, logger)
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
		backend:      cfg.backend,
	}, nil
}

// announce sends an AnnounceRequest to all sidecar nodes and logs their responses.
func (t *taskRunner) announce() {
	payload, err := shoalproto.MarshalAnnounceRequest(
		&shoalproto.AnnounceRequest{
			Port:    uint32(t.port),
			State:   cluster.StateReady,
			Backend: t.backend,
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
			t.logger.Info().Msg("registered with sidecar")
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

// drain tells the cluster we're leaving
// and waits for graceful drain and kills worker
func (t *taskRunner) drain(_ context.Context) {
	t.node.Serf.Leave()
	time.Sleep(t.drainTimeout)

	if t.worker.ProcessState == nil {
		t.worker.Process.Signal(syscall.SIGTERM) //nolint:errcheck
	}

	<-t.nodeErr
	t.logger.Info().Msg("clean shutdown complete")
}

func isRetryable(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, syscall.ECONNREFUSED.Error()) ||
		strings.Contains(s, syscall.ETIMEDOUT.Error()) ||
		strings.Contains(s, "i/o timeout")
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) ||
		strings.Contains(err.Error(), syscall.EADDRINUSE.Error())
}

func makeNewNode(nCfg node.NodeConfig, logger zerolog.Logger) (*node.Node, error) {
	retryInterval := time.Second * 3
	retryTimeout := time.Minute * 10
	deadline := time.Now().Add(retryTimeout)
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()

	for port := gossipBasePort + 1; port < 8000; port++ {
		l := logger.With().Int("serf_bind_port", port).Logger()
		nCfg.BindPort = port

	retry:
		for {
			l.Debug().Msg("attempting node.New")
			n, err := node.New(nCfg)
			switch {
			case err == nil:
				l.Info().Msg("serf bind success")
				return n, nil
			case isAddrInUse(err):
				l.Debug().Err(err).Msg("port in use, trying next")
				break retry
			case isRetryable(err):
				if time.Now().After(deadline) {
					return nil, fmt.Errorf("create node: %w", err)
				}
				l.Debug().Err(err).Msg("retryable, waiting")
				<-ticker.C
			default:
				l.Debug().Str("err_type", fmt.Sprintf("%T", err)).Err(err).Msg("node .New result")
				return nil, fmt.Errorf("create node: %w", err)
			}
		}
	}
	return nil, fmt.Errorf("no spare ports open for gossip")
}

func runTask(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	logger := zerolog.Ctx(ctx).With().Str("role", "task").Logger()

	var err error
	sc := startConfig{}
	sc.taskArgs, err = child.ArgsFromCobra(cmd, args)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed extracing worker args")
		return nil
	}
	sc.joinArgs = ensurePort(cmdVarJoinAddr, fmt.Sprintf("%d", gossipBasePort))

	t, err := startTask(ctx, sc)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed starting task")
		return err
	}

	t.run(ctx)
	t.drain(ctx)
	return nil
}

func ensurePort(addrs []string, defaultPort string) []string {
	out := make([]string, len(addrs))
	for i, a := range addrs {
		if _, _, err := net.SplitHostPort(a); err != nil {
			out[i] = net.JoinHostPort(a, defaultPort)
		} else {
			out[i] = a
		}
	}
	return out
}
