package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/serf/serf"
)

// ── Backend registry ──────────────────────────────────────────────────────────

type srvTarget struct {
	addr    string
	port    uint32
	backend string
	key     string
}

type registry struct {
	mu       sync.Mutex
	backends map[string]srvTarget
	seq      atomic.Int64
}

func newRegistry() *registry {
	return &registry{backends: make(map[string]srvTarget)}
}

func (r *registry) nextKey(b string) string {
	return fmt.Sprintf("%s-%d", b, r.seq.Add(1))
}

func (r *registry) add(nodeName, addr string, port uint32, backend string) srvTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := srvTarget{addr: addr, port: port, backend: backend, key: r.nextKey(backend)}
	r.backends[nodeName] = b
	return b
}

func (r *registry) get(nodeName string) (srvTarget, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.backends[nodeName]
	return b, ok
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

// ── Node catalog ──────────────────────────────────────────────────────────────

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
}

func (n *nodeCatalog) remove(name string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.nodes[name]
	if !ok {
		return false
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
