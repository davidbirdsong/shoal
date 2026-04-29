package main

import (
	"context"
	"fmt"
	"time"

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

// handleAnnounce is called for both "announce" queries and "solicit" responses —
// both carry an AnnounceRequest payload.
func (s *sidecar) handleAnnounce(nodeName string, payload []byte) {
	logger := s.logger.With().Str("node", nodeName).Logger()
	req, err := shoalproto.UnmarshalAnnounceRequest(payload)
	if err != nil {
		logger.Err(err).Msg("announce: unmarshal failed")
		return
	}
	if req.State != cluster.StateReady {
		logger.Debug().Str("state", req.State).Msg("announce: ignoring non-ready node")
		return
	}

	addr := s.nodes.getIP(nodeName)
	b := s.reg.add(nodeName, addr, req.Port, req.Backend)
	bl := logger.With().Str("backend", b.key).Str("addr", addr).Uint32("port", req.Port).Logger()
	if err := s.haproxy.AddServer(req.Backend, b.key, addr, req.Port); err != nil {
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

func (s *sidecar) onDepartQuery(q *serf.Query) error {
	req, err := shoalproto.UnmarshalDepartRequest(q.Payload)
	if err != nil {
		return fmt.Errorf("depart: unmarshal: %w", err)
	}
	addr := s.nodes.getIP(q.SourceNode())

	s.logger.Debug().Str("node", q.SourceNode()).Str("addr", addr).
		Int("port", int(req.Port)).Int("timeout", int(req.TimeoutSeconds)).
		Msg("depart received")
	s.removeByNode(q.SourceNode())
	resp, _ := shoalproto.MarshalDepartResponse(&shoalproto.DepartResponse{Accepted: true})
	return q.Respond(resp)
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
		s.logger.Debug().Str("member", m.Name).
			Str("status", m.Status.String()).
			Msg("member join event")
		if m.Status != serf.StatusAlive {
			s.nodes.remove(m.Name)
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
			s.logger.Debug().Msg("soliciting task announces")
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
		OnMemberLeave:  s.onMemberGone,
		OnQuery: map[string]func(*serf.Query) error{
			cluster.QueryAnnounce: s.onAnnounceQuery,
			cluster.QueryDepart:   s.onDepartQuery,
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

func runSidecar(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	logger := zerolog.Ctx(ctx).With().Str("role", "sidecar").Logger()

	s := &sidecar{
		reg:     newRegistry(),
		nodes:   newNodeCatalog(),
		haproxy: &haproxySocketClient{socketPath: sidecarHAProxySocket},
		logger:  logger,
	}

	n, err := node.New(node.NodeConfig{
		Role:        cluster.RoleSidecar,
		BindPort:    gossipBasePort,
		SnapshotDir: sidecarSnapshotDir,
		JoinAddrs:   sidecarJoin,
		Logger:      logger,
	})
	if err != nil {
		return err
	}

	go s.solicit(ctx, n)
	return n.Run(ctx, s.handlers())
}
