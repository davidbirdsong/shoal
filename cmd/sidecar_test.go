package main

import (
	"net"
	"sync"
	"testing"

	"github.com/davidbirdsong/shoal/pkg/cluster"
	shoalproto "github.com/davidbirdsong/shoal/pkg/proto"
	"github.com/hashicorp/serf/serf"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Mock HAProxy ──────────────────────────────────────────────────────────────

type mockHAProxy struct {
	mu      sync.Mutex
	added   []string
	removed []string
	addErr  error
	delErr  error
}

func (m *mockHAProxy) AddServer(backend, key, addr string, port uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.added = append(m.added, key)
	return m.addErr
}

func (m *mockHAProxy) RemoveServer(backend, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, key)
	return m.delErr
}

func (m *mockHAProxy) DrainServer(backend, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.delErr
}

func (m *mockHAProxy) ShowServers(backend string) ([]ServerState, error) {
	return nil, nil
}

func newTestSidecar(h haproxyClient) *sidecar {
	return &sidecar{
		reg:     newRegistry(),
		nodes:   newNodeCatalog(),
		haproxy: h,
		logger:  zerolog.Nop(),
	}
}

// seedNode adds a member to the sidecar's node catalog so that handleAnnounce
// passes the catalog gate. addr must be a valid non-zero IPv4 address.
func seedNode(s *sidecar, name, addr string) {
	s.nodes.add(name, serf.Member{Name: name, Addr: net.ParseIP(addr)})
}

// ── Registry tests ────────────────────────────────────────────────────────────

func TestRegistryAddRemove(t *testing.T) {
	tests := []struct {
		name         string
		nodeName     string
		addr         string
		port         uint32
		backend      string
		removeNode   string
		wantFound    bool
		doubleRemove bool
	}{
		{
			name:         "add then remove same node",
			nodeName:     "node-1",
			addr:         "10.0.0.1",
			port:         5000,
			backend:      "webservers",
			removeNode:   "node-1",
			wantFound:    true,
			doubleRemove: true,
		},
		{
			name:       "remove node never added",
			nodeName:   "node-2",
			addr:       "10.0.0.2",
			port:       5001,
			backend:    "webservers",
			removeNode: "nonexistent",
			wantFound:  false,
		},
		{
			name:       "remove different node than added",
			nodeName:   "node-3",
			addr:       "10.0.0.3",
			port:       5002,
			backend:    "webservers",
			removeNode: "node-4",
			wantFound:  false,
		},
		{
			name:       "addr and port preserved on add",
			nodeName:   "node-5",
			addr:       "172.16.0.1",
			port:       9999,
			backend:    "api",
			removeNode: "node-5",
			wantFound:  true,
		},
		{
			name:       "zero port accepted",
			nodeName:   "node-6",
			addr:       "10.0.0.6",
			port:       0,
			backend:    "webservers",
			removeNode: "node-6",
			wantFound:  true,
		},
		{
			name:       "backend preserved on add",
			nodeName:   "node-7",
			addr:       "10.0.0.7",
			port:       8080,
			backend:    "grpc",
			removeNode: "node-7",
			wantFound:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRegistry()
			b := r.add(tt.nodeName, tt.addr, tt.port, tt.backend)

			require.NotEmpty(t, b.key)
			assert.Equal(t, tt.addr, b.addr)
			assert.Equal(t, tt.port, b.port)
			assert.Equal(t, tt.backend, b.backend)

			got, found := r.remove(tt.removeNode)
			assert.Equal(t, tt.wantFound, found)
			if tt.wantFound {
				assert.Equal(t, b.key, got.key)
			}

			if tt.doubleRemove {
				_, found2 := r.remove(tt.removeNode)
				assert.False(t, found2, "second remove should return false")
			}
		})
	}
}

func TestRegistryKeysUnique(t *testing.T) {
	r := newRegistry()
	b1 := r.add("node-1", "10.0.0.1", 5001, "webservers")
	b2 := r.add("node-2", "10.0.0.2", 5002, "webservers")
	assert.NotEqual(t, b1.key, b2.key, "backend keys must be unique across nodes")
}

func TestRegistryConcurrent(t *testing.T) {
	r := newRegistry()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.add("concurrent-node", "10.0.0.1", uint32(5000+i), "webservers")
		}(i)
	}
	wg.Wait()
}

// ── Sidecar method tests ──────────────────────────────────────────────────────

// announcePayload builds a ready-state AnnounceRequest payload.
// Address is omitted — the sidecar resolves it from nodeCatalog.
func announcePayload(t *testing.T, port uint32, backend string) []byte {
	t.Helper()
	b, err := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
		Port:    port,
		Backend: backend,
		State:   cluster.StateReady,
	})
	require.NoError(t, err)
	return b
}

func TestHandleAnnounce_Ready(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)
	seedNode(s, "task-node-1", "10.0.0.1")

	s.handleAnnounce("task-node-1", announcePayload(t, 9000, "webservers"))

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Len(t, mock.added, 1)
}

func TestHandleAnnounce_NonReady(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)

	for _, state := range []string{cluster.StateStarting} {
		payload, _ := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
			Port:    9000,
			Backend: "webservers",
			State:   state,
		})
		s.handleAnnounce("task-node-1", payload)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Empty(t, mock.added, "non-ready states must not trigger AddServer")
}

func TestHandleAnnounce_InvalidPayload(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)
	s.handleAnnounce("task-node-1", []byte("not-proto")) // must not panic
	assert.Empty(t, mock.added)
}

func TestRemoveByNode_Registered(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)
	seedNode(s, "task-node-1", "10.0.0.1")

	s.handleAnnounce("task-node-1", announcePayload(t, 9000, "webservers"))
	s.removeByNode("task-node-1")

	mock.mu.Lock()
	defer mock.mu.Unlock()
	require.Len(t, mock.removed, 1)
	assert.Equal(t, mock.added[0], mock.removed[0], "removed key must match added key")
}

func TestRemoveByNode_NotRegistered(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)
	s.removeByNode("ghost-node") // must not panic, no RemoveServer call
	assert.Empty(t, mock.removed)
}

func TestOnMemberGone(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)

	for _, name := range []string{"a", "b"} {
		seedNode(s, name, "10.0.0.1")
		s.handleAnnounce(name, announcePayload(t, 9000, "webservers"))
	}

	s.onMemberGone([]serf.Member{{Name: "a"}, {Name: "b"}})

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Len(t, mock.removed, 2)
}

func TestDuplicateAnnounce(t *testing.T) {
	mock := &mockHAProxy{}
	s := newTestSidecar(mock)

	seedNode(s, "node-1", "10.0.0.1")
	payload := announcePayload(t, 9000, "webservers")
	s.handleAnnounce("node-1", payload)
	s.handleAnnounce("node-1", payload) // re-announce same node

	// Both calls should succeed; registry overwrites the entry.
	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Len(t, mock.added, 2)
}
