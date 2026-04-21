package node

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/davidbirdsong/shoal/pkg/cluster"
	"github.com/hashicorp/memberlist"
	"github.com/hashicorp/serf/serf"
	"github.com/rs/zerolog"
)

// NodeConfig configures a serf cluster node.
type NodeConfig struct {
	// Role is cluster.RoleSidecar or cluster.RoleTask. Required.
	Role string

	// Tags are additional serf node tags merged with the role tag.
	// Do not include cluster.TagKeyRole — it is set from Role.
	Tags map[string]string

	// SnapshotDir is the directory for serf snapshot files.
	// Created if absent. Defaults to "./serf-snapshots".
	SnapshotDir string

	// JoinAddrs are seed addresses to join on startup.
	// Empty means bootstrap a new single-node cluster.
	JoinAddrs []string

	// BindAddr is the address memberlist binds on.
	// Empty defaults to all interfaces.
	BindAddr string

	// Logger receives internal log output. Pass zerolog.Nop() to discard.
	Logger zerolog.Logger
}

// Node wraps a serf.Serf instance with its event channel.
type Node struct {
	Serf    *serf.Serf
	log     zerolog.Logger
	eventCh <-chan serf.Event
}

// New creates a serf node from cfg and optionally joins existing cluster members.
func New(cfg NodeConfig) (*Node, error) {
	if cfg.SnapshotDir == "" {
		cfg.SnapshotDir = "./serf-snapshots"
	}
	if err := os.MkdirAll(cfg.SnapshotDir, 0o755); err != nil {
		return nil, fmt.Errorf("node: create snapshot dir: %w", err)
	}

	// serf and memberlist accept a stdlib *log.Logger; bridge via zerolog's io.Writer impl.
	stdlog := log.New(cfg.Logger, "", 0)

	mlCfg := memberlist.DefaultLANConfig()
	mlCfg.Logger = stdlog
	if cfg.BindAddr != "" {
		mlCfg.BindAddr = cfg.BindAddr
	}
	mlCfg.ProtocolVersion = memberlist.ProtocolVersionMax

	serfCfg := serf.DefaultConfig()
	serfCfg.MemberlistConfig = mlCfg
	serfCfg.Logger = stdlog

	tags := map[string]string{
		cluster.TagKeyRole:  cfg.Role,
		cluster.TagKeyState: cluster.StateStarting,
	}
	for k, v := range cfg.Tags {
		tags[k] = v
	}
	serfCfg.Tags = tags

	serfCfg.SnapshotPath = filepath.Join(cfg.SnapshotDir, serfCfg.NodeName)

	eventCh := make(chan serf.Event, 64)
	serfCfg.EventCh = eventCh

	s, err := serf.Create(serfCfg)
	if err != nil {
		return nil, fmt.Errorf("node: serf create: %w", err)
	}

	if len(cfg.JoinAddrs) > 0 {
		if _, err := s.Join(cfg.JoinAddrs, true); err != nil {
			s.Shutdown() //nolint:errcheck
			return nil, fmt.Errorf("node: join %v: %w", cfg.JoinAddrs, err)
		}
	}

	return &Node{Serf: s, log: cfg.Logger, eventCh: eventCh}, nil
}

// EventHandlers is a dispatch table for serf events.
// Nil fields are silently ignored.
type EventHandlers struct {
	OnMemberJoin   func([]serf.Member)
	OnMemberLeave  func([]serf.Member)
	OnMemberFailed func([]serf.Member)
	OnMemberUpdate func([]serf.Member)

	// OnQuery maps serf query names to handler functions.
	// Unrecognised query names are ignored (no response sent).
	OnQuery map[string]func(*serf.Query) error
}

// Run reads the node's event channel and dispatches to h until ctx is cancelled.
// On cancellation it calls Leave then Shutdown and returns ctx.Err().
func (n *Node) Run(ctx context.Context, h EventHandlers) error {
	for {
		select {
		case <-ctx.Done():
			n.Serf.Leave()    //nolint:errcheck
			n.Serf.Shutdown() //nolint:errcheck
			return ctx.Err()

		case e, ok := <-n.eventCh:
			if !ok {
				return nil
			}
			n.dispatch(e, h)
		}
	}
}

func (n *Node) dispatch(e serf.Event, h EventHandlers) {
	switch e.EventType() {
	case serf.EventMemberJoin:
		if h.OnMemberJoin != nil {
			h.OnMemberJoin(e.(serf.MemberEvent).Members)
		}
	case serf.EventMemberLeave:
		if h.OnMemberLeave != nil {
			h.OnMemberLeave(e.(serf.MemberEvent).Members)
		}
	case serf.EventMemberFailed:
		if h.OnMemberFailed != nil {
			h.OnMemberFailed(e.(serf.MemberEvent).Members)
		}
	case serf.EventMemberUpdate:
		if h.OnMemberUpdate != nil {
			h.OnMemberUpdate(e.(serf.MemberEvent).Members)
		}
	case serf.EventQuery:
		q := e.(*serf.Query)
		fn, ok := h.OnQuery[q.Name]
		if !ok {
			return
		}
		if err := fn(q); err != nil {
			n.log.Error().Err(err).Str("query", q.Name).Msg("query handler error")
		}
	}
}
