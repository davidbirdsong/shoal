package node

import (
	"errors"
	"testing"

	"github.com/hashicorp/serf/serf"
	"github.com/rs/zerolog"
)

func nopNode() *Node {
	return &Node{logger: zerolog.Nop()}
}

// memberEvent constructs a serf.MemberEvent for the given type and node names.
func memberEvent(t serf.EventType, names ...string) serf.MemberEvent {
	members := make([]serf.Member, len(names))
	for i, n := range names {
		members[i] = serf.Member{Name: n}
	}
	return serf.MemberEvent{Type: t, Members: members}
}

func TestDispatchMemberJoin(t *testing.T) {
	var got []string
	h := EventHandlers{
		OnMemberJoin: func(m []serf.Member) {
			for _, mm := range m {
				got = append(got, mm.Name)
			}
		},
	}
	nopNode().dispatch(memberEvent(serf.EventMemberJoin, "a", "b"), h)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("got %v", got)
	}
}

func TestDispatchMemberLeave(t *testing.T) {
	called := false
	h := EventHandlers{OnMemberLeave: func([]serf.Member) { called = true }}
	nopNode().dispatch(memberEvent(serf.EventMemberLeave, "x"), h)
	if !called {
		t.Error("OnMemberLeave not called")
	}
}

func TestDispatchMemberFailed(t *testing.T) {
	called := false
	h := EventHandlers{OnMemberFailed: func([]serf.Member) { called = true }}
	nopNode().dispatch(memberEvent(serf.EventMemberFailed, "x"), h)
	if !called {
		t.Error("OnMemberFailed not called")
	}
}

func TestDispatchMemberUpdate(t *testing.T) {
	called := false
	h := EventHandlers{OnMemberUpdate: func([]serf.Member) { called = true }}
	nopNode().dispatch(memberEvent(serf.EventMemberUpdate, "x"), h)
	if !called {
		t.Error("OnMemberUpdate not called")
	}
}

func TestDispatchNilHandlersSafe(t *testing.T) {
	// All nil handlers — must not panic.
	h := EventHandlers{}
	n := nopNode()
	n.dispatch(memberEvent(serf.EventMemberJoin, "x"), h)
	n.dispatch(memberEvent(serf.EventMemberLeave, "x"), h)
	n.dispatch(memberEvent(serf.EventMemberFailed, "x"), h)
	n.dispatch(memberEvent(serf.EventMemberUpdate, "x"), h)
}

func TestDispatchQuery_KnownName(t *testing.T) {
	called := false
	h := EventHandlers{
		OnQuery: map[string]func(*serf.Query) error{
			"announce": func(q *serf.Query) error {
				called = true
				return nil
			},
		},
	}
	// Construct a Query with only exported fields — Respond is not called in the handler.
	q := &serf.Query{Name: "announce", Payload: []byte("payload")}
	nopNode().dispatch(q, h)
	if !called {
		t.Error("announce handler not called")
	}
}

func TestDispatchQuery_UnknownName(t *testing.T) {
	// Unknown query name — must not panic, no handler called.
	called := false
	h := EventHandlers{
		OnQuery: map[string]func(*serf.Query) error{
			"announce": func(q *serf.Query) error { called = true; return nil },
		},
	}
	q := &serf.Query{Name: "unknown"}
	nopNode().dispatch(q, h)
	if called {
		t.Error("handler should not be called for unknown query name")
	}
}

func TestDispatchQuery_NilMap(t *testing.T) {
	h := EventHandlers{OnQuery: nil}
	q := &serf.Query{Name: "announce"}
	nopNode().dispatch(q, h) // must not panic
}

func TestDispatchQuery_HandlerError(t *testing.T) {
	// Handler returns an error — dispatch should log it and not panic.
	called := false
	h := EventHandlers{
		OnQuery: map[string]func(*serf.Query) error{
			"bad": func(q *serf.Query) error {
				called = true
				return errors.New("handler error")
			},
		},
	}
	q := &serf.Query{Name: "bad"}
	nopNode().dispatch(q, h)
	if !called {
		t.Error("handler not called")
	}
}
