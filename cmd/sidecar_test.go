package main

import (
	"sync"
	"testing"

	"github.com/davidbirdsong/shoal/pkg/cluster"
	shoalproto "github.com/davidbirdsong/shoal/pkg/proto"
	"github.com/hashicorp/serf/serf"
	"github.com/rs/zerolog"
)

// ── Mock HAProxy ──────────────────────────────────────────────────────────────

type mockHAProxy struct {
	mu      sync.Mutex
	added   []string
	removed []string
	addErr  error
	delErr  error
}

func (m *mockHAProxy) AddServer(key, addr string, port uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.added = append(m.added, key)
	return m.addErr
}

func (m *mockHAProxy) RemoveServer(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, key)
	return m.delErr
}

func (m *mockHAProxy) ShowServers() ([]ServerState, error) {
	return nil, nil
}

func nopZerolog() zerolog.Logger { return zerolog.Nop() }

func newTestSidecar(h haproxyClient) *sidecar {
	return &sidecar{
		reg:     newRegistry(),
		nodes:   newNodeCatalog(),
		haproxy: h,
		logger:  zerolog.Nop(),
	}
}

// ── Registry tests ────────────────────────────────────────────────────────────

func TestRegistryAddRemove(t *testing.T) {
	r := newRegistry()

	b := r.add("node-1", "10.0.0.1", 5000)
	if b.addr != "10.0.0.1" || b.port != 5000 || b.key == "" {
		t.Fatalf("unexpected backend: %+v", b)
	}

	got, ok := r.remove("node-1")
	if !ok {
		t.Fatal("expected remove to find node-1")
	}
	if got.key != b.key {
		t.Errorf("removed key %q, want %q", got.key, b.key)
	}

	_, ok = r.remove("node-1")
	if ok {
		t.Error("second remove should return false")
	}
}

func TestRegistryRemoveAbsent(t *testing.T) {
	r := newRegistry()
	_, ok := r.remove("nonexistent")
	if ok {
		t.Error("remove of absent node should return false")
	}
}

func TestRegistryKeysUnique(t *testing.T) {
	r := newRegistry()
	b1 := r.add("node-1", "10.0.0.1", 5001)
	b2 := r.add("node-2", "10.0.0.2", 5002)
	if b1.key == b2.key {
		t.Errorf("duplicate backend keys: %q", b1.key)
	}
}

func TestRegistryConcurrent(t *testing.T) {
	r := newRegistry()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "node-" + zerolog.GlobalLevel().String() // just a unique-ish string per goroutine
			_ = name
			r.add("concurrent-node", "10.0.0.1", uint32(5000+i))
		}(i)
	}
	wg.Wait()
}

// ── Sidecar method tests ──────────────────────────────────────────────────────

func TestHandleAnnounce_Ready(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)

	payload, _ := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
		Addr:  "10.0.0.5",
		Port:  9000,
		State: cluster.StateReady,
	})
	s.handleAnnounce("task-node-1", payload)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.added) != 1 {
		t.Fatalf("expected 1 AddServer call, got %d", len(mock.added))
	}
}

func TestHandleAnnounce_NonReady(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)

	for _, state := range []string{cluster.StateStarting, cluster.StateDraining} {
		payload, _ := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
			Addr:  "10.0.0.5",
			Port:  9000,
			State: state,
		})
		s.handleAnnounce("task-node-1", payload)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.added) != 0 {
		t.Errorf("expected no AddServer calls for non-ready states, got %d", len(mock.added))
	}
}

func TestHandleAnnounce_InvalidPayload(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)
	s.handleAnnounce("task-node-1", []byte("not-proto")) // must not panic
	if len(mock.added) != 0 {
		t.Error("expected no AddServer call on bad payload")
	}
}

func TestRemoveByNode_Registered(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)

	payload, _ := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
		Addr:  "10.0.0.5",
		Port:  9000,
		State: cluster.StateReady,
	})
	s.handleAnnounce("task-node-1", payload)
	s.removeByNode("task-node-1")

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.removed) != 1 {
		t.Fatalf("expected 1 RemoveServer call, got %d", len(mock.removed))
	}
	if mock.removed[0] != mock.added[0] {
		t.Errorf("removed key %q != added key %q", mock.removed[0], mock.added[0])
	}
}

func TestRemoveByNode_NotRegistered(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)
	s.removeByNode("ghost-node") // must not panic, no RemoveServer call
	if len(mock.removed) != 0 {
		t.Error("expected no RemoveServer call for unknown node")
	}
}

func TestOnMemberGone(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)

	// Register two nodes.
	for _, name := range []string{"a", "b"} {
		payload, _ := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
			Addr: "10.0.0.1", Port: 9000, State: cluster.StateReady,
		})
		s.handleAnnounce(name, payload)
	}

	s.onMemberGone([]serf.Member{{Name: "a"}, {Name: "b"}})

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.removed) != 2 {
		t.Errorf("expected 2 RemoveServer calls, got %d", len(mock.removed))
	}
}

func TestDuplicateAnnounce(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)

	payload, _ := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
		Addr: "10.0.0.5", Port: 9000, State: cluster.StateReady,
	})

	s.handleAnnounce("node-1", payload)
	s.handleAnnounce("node-1", payload) // re-announce same node

	// Both calls should succeed; registry overwrites the entry.
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.added) != 2 {
		t.Errorf("expected 2 AddServer calls, got %d", len(mock.added))
	}
}
