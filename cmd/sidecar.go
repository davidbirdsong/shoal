package main

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"time"

	"github.com/davidbirdsong/shoal/pkg/child"
	"github.com/davidbirdsong/shoal/pkg/cluster"
	"github.com/davidbirdsong/shoal/pkg/node"
	shoalproto "github.com/davidbirdsong/shoal/pkg/proto"
	"github.com/hashicorp/serf/serf"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

const gossipBasePort = 7946

// ── Sidecar ───────────────────────────────────────────────────────────────────

type sidecar struct {
	reg     *registry
	nodes   *nodeCatalog
	haproxy haproxyClient
	logger  zerolog.Logger
}

func (s *sidecar) drainByNode(nodeName string) {
	b, ok := s.reg.get(nodeName)
	if !ok {
		s.logger.Debug().Str("nodeName", nodeName).Msg("leaving server not in registry")
		return
	}
	if len(b.backend) == 0 {
		return
	}

	logger := s.logger.With().Str("backend", b.backend).
		Str("node_name", nodeName).Str("key", b.key).Logger()

	if err := s.haproxy.DrainServer(b.backend, b.key); err != nil {
		logger.Warn().Err(err).Msg("failed to set drain")
		return
	}
	logger.Info().Msg("draining server")
	return
}

func (s *sidecar) removeByNode(nodeName string) {
	b, ok := s.reg.remove(nodeName)
	_ = s.nodes.remove(nodeName)
	if !ok {
		return
	}
	logger := s.logger.With().Str("backend", b.key).Str("node", nodeName).Logger()
	if err := s.haproxy.RemoveServer(b.backend, b.key); err != nil {
		logger.Err(err).Msg("remove server failed")
	} else {
		logger.Info().Msg("removed backend")
	}
}

func unusableIP(i net.IP) bool {
	return len(i) == 0 || i.IsUnspecified()
}

// handleAnnounce is called for both "announce" queries and "solicit" responses —
// both carry an AnnounceRequest payload.
func (s *sidecar) handleAnnounce(nodeName string, payload []byte) {
	logger := s.logger.With().Str("announce_node", nodeName).Logger()
	req, err := shoalproto.UnmarshalAnnounceRequest(payload)
	if err != nil {
		logger.Err(err).Msg("announce: unmarshal failed")
		return
	}
	if req.State != cluster.StateReady {
		logger.Debug().Str("state", req.State).Msg("announce: ignoring non-ready node")
		return
	}
	m, ok := s.nodes.getMember(nodeName)
	if !ok {
		logger.Warn().Msg("announce before join")
		return
	}
	if unusableIP(m.Addr) {
		logger.Warn().Msg("announce has unusable IP")
	}

	addr := m.Addr.String()
	b := s.reg.add(nodeName, addr, req.Port, req.Backend)
	bl := logger.With().Str("backend", b.backend).Str("addr", addr).Uint32("port", req.Port).Logger()
	if err := s.haproxy.AddServer(b.backend, b.key, addr, req.Port); err != nil {
		bl.Err(err).Msg("announce: add server failed")
	} else {
		bl.Info().Msg("registered backend")
	}
}

func (s *sidecar) onAnnounceQuery(q *serf.Query) error {
	s.handleAnnounce(q.SourceNode(), q.Payload)
	resp, _ := shoalproto.MarshalAnnounceResponse(
		&shoalproto.AnnounceResponse{Accepted: true},
	)
	return q.Respond(resp)
}

func (s *sidecar) onMemberLeaving(members []serf.Member) {
	logger := s.logger.Info()
	for _, m := range members {
		logger.Str("node", m.Name).Msg("member leaving, set to drain")
		s.drainByNode(m.Name)
	}
}

func (s *sidecar) onMemberGone(members []serf.Member) {
	logger := s.logger.Info()
	for _, m := range members {
		logger.Str("node", m.Name).Msg("member gone")
		s.removeByNode(m.Name)
	}
}

func (s *sidecar) onMemberJoin(members []serf.Member) {
	for _, m := range members {
		l := s.logger.With().Str("member", m.Name).Logger()
		l.Debug().
			Str("status", m.Status.String()).
			Str("addr", m.Addr.String()).
			Msg("member join event all members run through")
		if m.Status == serf.StatusAlive {
			if len(m.Addr) == 0 {
				l.Warn().Msg("REFUSING to add nil addr member")
				continue
			}
			if m.Addr.IsUnspecified() {
				l.Warn().Msg("REFUSING to add unspecified addr member")
				continue
			}

			s.nodes.add(m.Name, m)
		}
	}
}

func (s *sidecar) solicit(ctx context.Context, n *node.Node) {
	ticker := time.NewTicker(sidecarSolicitInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			resp, err := n.Serf.Query(cluster.QuerySolicit, nil, &serf.QueryParam{
				FilterTags: map[string]string{cluster.TagKeyRole: cluster.RoleTask},
			})
			if err != nil {
				s.logger.Err(err).Msg("solicit query failed")
				continue
			}
			for r := range resp.ResponseCh() {
				s.handleAnnounce(r.From, r.Payload)
			}
		}
	}
}

func (s *sidecar) handlers() node.EventHandlers {
	return node.EventHandlers{
		OnMemberJoin:   s.onMemberJoin,
		OnMemberFailed: s.onMemberGone,
		OnMemberLeave:  s.onMemberLeaving,
		OnQuery: map[string]func(*serf.Query) error{
			cluster.QueryAnnounce: s.onAnnounceQuery,
		},
	}
}

// ── Cobra command ─────────────────────────────────────────────────────────────

var (
	sidecarJoin            []string
	sidecarHAProxySocket   string
	sidecarHAProxyBackend  string
	sidecarSnapshotDir     string
	sidecarSolicitInterval time.Duration
)

func init() {
	f := sidecarCmd.Flags()
	f.StringSliceVar(&sidecarJoin, "join", nil, "seed addresses to join (required)")
	f.StringVar(&sidecarHAProxySocket, "haproxy-socket", "/var/run/haproxy/admin.sock", "HAProxy runtime UNIX socket path")
	f.StringVar(&sidecarHAProxyBackend, "haproxy-backend", "shoal", "HAProxy backend pool name")
	f.StringVar(&sidecarSnapshotDir, "snapshot-dir", "./serf-snapshots", "serf snapshot directory")
	f.DurationVar(&sidecarSolicitInterval, "solicit-interval", 30*time.Second,
		"interval between solicit queries to tasks")
}

func runSidecar(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	logger := zerolog.Ctx(ctx).With().Str("role", "sidecar").Logger()

	s := &sidecar{
		reg:     newRegistry(),
		nodes:   newNodeCatalog(),
		haproxy: &haproxySocketClient{socketPath: sidecarHAProxySocket},
		logger:  logger,
	}
	logger.Debug().Str("haproxy_sock", sidecarHAProxySocket).Msg("running haproxy sidecar")

	bindAddr, err := ec2PrivateIP(ctx)
	if err != nil {
		logger.Warn().Err(err).Msg("could not fetch EC2 private IP; memberlist will choose advertise addr")
	}

	n, err := node.New(node.NodeConfig{
		NodeName: nodeName,
		Role:     cluster.RoleSidecar,
		// AdvertiseAddr: advertiseAddr,
		BindAddr:  bindAddr,
		BindPort:  gossipBasePort,
		JoinAddrs: ensurePort(sidecarJoin, fmt.Sprintf("%d", gossipBasePort)),
		Logger:    logger,
	})
	if err != nil {
		logger.Fatal().Err(err)
		return nil
	}
	m := n.Serf.Memberlist()
	logger.Debug().Str("local_node", m.LocalNode().String()).Msg("node as seen by serf")

	sc := startConfig{
		nodename: nodeName,
		backend:  taskBackend,
	}
	sc.taskArgs, err = child.ArgsFromCobra(cmd, args)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed extracing worker args")
		return nil
	}
	sc.joinArgs = ensurePort(cmdVarJoinAddr, fmt.Sprintf("%d", gossipBasePort))

	logger.Debug().Strs("haproxy_cmd", args).Msg("starting haproxy")
	worker := exec.Command(args[0], args[1:]...)
	bgTail := pipeWorkers(worker, logger)
	if err := worker.Start(); err != nil {
		logger.Fatal().Err(err).Msg("starting haproxy worker")
		return nil
	}
	bgTail()
	defer func() {
		if err := n.Serf.Leave(); err != nil {
			logger.Warn().Err(err).Msg("on serf leave")
		}

		if err := worker.Cancel(); err != nil {
			logger.Warn().Err(err).Msg("worker shutdown")
		}
		worker.Wait()
	}()

	go s.solicit(ctx, n)
	err = n.Run(ctx, s.handlers())
	if err != nil {
	}
	logger.Fatal().Err(err)

	return nil
}
