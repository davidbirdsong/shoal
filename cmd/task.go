package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
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
	f.StringArrayVar(&taskWorker, "worker", nil, "worker command and arguments")
}

func runTask(_ *cobra.Command, _ []string) error {
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
		With().Timestamp().Str("role", "task").Logger()

	if taskJoin == "" {
		return fmt.Errorf("--join is required")
	}
	if len(taskWorker) == 0 {
		return fmt.Errorf("--worker is required")
	}

	// ── Phase 1: STARTING ────────────────────────────────────────────────────

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("bind ephemeral port: %w", err)
	}
	boundAddr := ln.Addr().(*net.TCPAddr)
	log.Info().Int("port", boundAddr.Port).Msg("bound ephemeral port")

	// TODO: pass fd directly so worker can call accept() without knowing the port.
	// For now the port is passed via env and the fd is available as fd 3.
	lnFile, err := ln.(*net.TCPListener).File()
	if err != nil {
		return fmt.Errorf("get listener fd: %w", err)
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
		return fmt.Errorf("start worker: %w", err)
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
		return fmt.Errorf("create node: %w", err)
	}

	if err := n.Serf.SetTags(map[string]string{
		cluster.TagKeyRole:  cluster.RoleTask,
		cluster.TagKeyState: cluster.StateReady,
	}); err != nil {
		return fmt.Errorf("set tags ready: %w", err)
	}

	// ── Phase 2: READY ───────────────────────────────────────────────────────

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	announce := func() {
		payload, err := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
			Addr:  boundAddr.IP.String(),
			Port:  uint32(boundAddr.Port),
			State: cluster.StateReady,
		})
		if err != nil {
			log.Error().Err(err).Msg("announce: marshal failed")
			return
		}
		resp, err := n.Serf.Query(cluster.QueryAnnounce, payload, &serf.QueryParam{
			FilterTags: map[string]string{cluster.TagKeyRole: cluster.RoleSidecar},
		})
		if err != nil {
			log.Error().Err(err).Msg("announce: query failed")
			return
		}
		for r := range resp.ResponseCh() {
			ack, err := shoalproto.UnmarshalAnnounceResponse(r.Payload)
			if err != nil {
				log.Error().Err(err).Str("from", r.From).Msg("announce: unmarshal response failed")
				continue
			}
			if ack.Accepted {
				log.Info().Str("backend", ack.BackendKey).Msg("registered with sidecar")
			} else {
				log.Warn().Str("from", r.From).Str("reason", ack.Reason).Msg("announce rejected")
			}
		}
	}

	announce()

	handlers := node.EventHandlers{
		OnQuery: map[string]func(*serf.Query) error{
			cluster.QuerySolicit: func(q *serf.Query) error {
				payload, err := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
					Addr:  boundAddr.IP.String(),
					Port:  uint32(boundAddr.Port),
					State: cluster.StateReady,
				})
				if err != nil {
					return fmt.Errorf("solicit: marshal: %w", err)
				}
				return q.Respond(payload)
			},
		},
	}

	runErr := make(chan error, 1)
	go func() { runErr <- n.Run(ctx, handlers) }()

	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Wait() }()

	select {
	case sig := <-sigs:
		log.Info().Str("signal", sig.String()).Msg("received signal, draining")
	case err := <-workerDone:
		log.Info().Err(err).Msg("worker exited")
	}

	// ── Phase 3: DRAINING ────────────────────────────────────────────────────

	n.Serf.SetTags(map[string]string{ //nolint:errcheck
		cluster.TagKeyRole:  cluster.RoleTask,
		cluster.TagKeyState: cluster.StateDraining,
	})

	payload, _ := shoalproto.MarshalDepartRequest(&shoalproto.DepartRequest{
		Addr:           boundAddr.IP.String(),
		Port:           uint32(boundAddr.Port),
		TimeoutSeconds: uint32(taskDrainTimeout.Seconds()),
	})

	resp, err := n.Serf.Query(cluster.QueryDepart, payload, &serf.QueryParam{
		FilterTags: map[string]string{cluster.TagKeyRole: cluster.RoleSidecar},
	})
	if err != nil {
		log.Error().Err(err).Msg("depart query failed")
	} else {
		drainTimer := time.NewTimer(taskDrainTimeout)
		defer drainTimer.Stop()
		for {
			select {
			case r, ok := <-resp.ResponseCh():
				if !ok {
					goto departed
				}
				ack, _ := shoalproto.UnmarshalDepartResponse(r.Payload)
				if ack != nil && ack.Accepted {
					log.Info().Str("from", r.From).Msg("sidecar acknowledged depart")
					goto departed
				}
			case <-drainTimer.C:
				log.Warn().Msg("drain timeout reached, forcing exit")
				goto departed
			}
		}
	}

departed:
	cancel()
	<-runErr

	if worker.ProcessState == nil {
		worker.Process.Signal(syscall.SIGTERM) //nolint:errcheck
	}

	log.Info().Msg("clean shutdown complete")
	return nil
}
