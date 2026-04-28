package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/davidbirdsong/shoal/pkg/cluster"
	"github.com/davidbirdsong/shoal/pkg/node"
	shoalproto "github.com/davidbirdsong/shoal/pkg/proto"
	"github.com/hashicorp/serf/serf"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

// ── HAProxy client ────────────────────────────────────────────────────────────

// ServerState represents one server entry from HAProxy's runtime state.
type ServerState struct {
	Name    string
	Addr    string
	Port    uint32
	OpState string // "2" = UP, "0" = DOWN/MAINT
}

// haproxyClient abstracts HAProxy runtime socket operations.
type haproxyClient interface {
	AddServer(key, addr string, port uint32) error
	RemoveServer(key string) error
	ShowServers() ([]ServerState, error)
}

type haproxySocketClient struct {
	socketPath string
	backend    string
}

// query sends a command and returns the full response.
func (h *haproxySocketClient) query(cmd string) (string, error) {
	conn, err := net.Dial("unix", h.socketPath)
	if err != nil {
		return "", fmt.Errorf("haproxy: dial %s: %w", h.socketPath, err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return "", err
	}
	var sb strings.Builder
	if _, err := bufio.NewReader(conn).WriteTo(&sb); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// send sends a command and discards the response.
func (h *haproxySocketClient) send(cmd string) error {
	_, err := h.query(cmd)
	return err
}

func (h *haproxySocketClient) AddServer(key, addr string, port uint32) error {
	if err := h.send(fmt.Sprintf("add server %s/%s %s:%d", h.backend, key, addr, port)); err != nil {
		return err
	}
	return h.send(fmt.Sprintf("set server %s/%s state ready", h.backend, key))
}

func (h *haproxySocketClient) RemoveServer(key string) error {
	return h.send(fmt.Sprintf("del server %s/%s", h.backend, key))
}

// ShowServers returns the current server list for the configured backend.
// HAProxy's "show servers state <backend>" response columns (1-indexed):
//
//	1=be_id 2=be_name 3=srv_id 4=srv_name 5=srv_addr 6=srv_op_state ...  12=srv_port
func (h *haproxySocketClient) ShowServers() ([]ServerState, error) {
	out, err := h.query(fmt.Sprintf("show servers state %s", h.backend))
	if err != nil {
		return nil, err
	}
	var servers []ServerState
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 12 {
			continue
		}
		port64, _ := strconv.ParseUint(fields[11], 10, 32)
		servers = append(servers, ServerState{
			Name:    fields[3],
			Addr:    fields[4],
			Port:    uint32(port64),
			OpState: fields[5],
		})
	}
	return servers, nil
}

// ── Backend registry ──────────────────────────────────────────────────────────

type srvTarget struct {
	addr string
	port uint32
	key  string
}

type nodeCatalog struct {
	mu    sync.Mutex
	nodes map[string]serf.Member
	seq   atomic.Int64
}

func newNodeCatalog() *nodeCatalog {
	return &nodeCatalog{nodes: make(map[string]serf.Member)}
}

func (n *nodeCatalog) add(name string, m serf.Member) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nodes[name] = m
	return
}

func (n *nodeCatalog) remove(name string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.nodes[name]
	if !ok {
		return ok
	}
	delete(n.nodes, name)
	return true
}

func (n *nodeCatalog) getMember(name string) serf.Member {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.nodes[name]
}

func (n *nodeCatalog) getIP(name string) string {
	m := n.getMember(name)
	if m.Name != name {
		return "unknown"
	}
	return m.Addr.String()
}

type registry struct {
	mu       sync.Mutex
	backends map[string]srvTarget
	seq      atomic.Int64
}

func newRegistry() *registry {
	return &registry{backends: make(map[string]srvTarget)}
}

func (r *registry) nextKey() string {
	return fmt.Sprintf("task-%d", r.seq.Add(1))
}

func (r *registry) add(nodeName, addr string, port uint32) srvTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := srvTarget{addr: addr, port: port, key: r.nextKey()}
	r.backends[nodeName] = b
	return b
}

func (r *registry) remove(nodeName string) (srvTarget, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.backends[nodeName]
	if ok {
		delete(r.backends, nodeName)
	}
	return b, ok
}

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
	if err := s.haproxy.RemoveServer(b.key); err != nil {
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
	b := s.reg.add(nodeName, addr,
		req.Port)
	bl := logger.With().Str("backend", b.key).Str("addr", addr).Uint32("port", req.Port).Logger()
	if err := s.haproxy.AddServer(b.key, addr, req.Port); err != nil {
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
	logPort := int(req.Port)
	logTimeout := int(req.TimeoutSeconds)
	addr := s.nodes.getIP(q.SourceNode())

	s.logger.Debug().Str("node", q.SourceNode()).Str("addr", addr).Int("port", logPort).
		Int("timeout", logTimeout).Msg("depart received")
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

func runSidecar(_ *cobra.Command, _ []string) error {
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).
		With().Timestamp().Str("role", "sidecar").Logger()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigs; logger.Info().Msg("shutting down"); cancel() }()

	s := &sidecar{
		reg:     newRegistry(),
		nodes:   newNodeCatalog(),
		haproxy: &haproxySocketClient{socketPath: sidecarHAProxySocket, backend: sidecarHAProxyBackend},
		logger:  logger,
	}

	n, err := node.New(node.NodeConfig{
		Role:        cluster.RoleSidecar,
		SnapshotDir: sidecarSnapshotDir,
		JoinAddrs:   sidecarJoin,
		Logger:      logger,
	})
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}

	go s.solicit(ctx, n)

	return n.Run(ctx, s.handlers())
}
