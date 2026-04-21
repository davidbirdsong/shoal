//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/davidbirdsong/shoal/pkg/cluster"
	shoalproto "github.com/davidbirdsong/shoal/pkg/proto"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// haproxyContainer starts a HAProxy container with the deploy config mounted
// and the runtime socket exposed via a tmpdir bind-mount.
func haproxyContainer(t *testing.T) (socketPath string, cleanup func()) {
	t.Helper()

	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..")
	cfgPath := filepath.Join(repoRoot, "deploy", "haproxy", "haproxy.cfg")

	socketDir := t.TempDir()

	req := testcontainers.ContainerRequest{
		Image: "haproxy:alpine",
		Mounts: testcontainers.Mounts(
			testcontainers.BindMount(cfgPath, "/usr/local/etc/haproxy/haproxy.cfg"),
			testcontainers.BindMount(socketDir, "/var/run/haproxy"),
		),
		// Override cmd: run haproxy directly (no shoal sidecar needed for these tests).
		Cmd:        []string{"haproxy", "-f", "/usr/local/etc/haproxy/haproxy.cfg"},
		WaitingFor: wait.ForLog("Proxy shoal started").WithStartupTimeout(15 * time.Second),
	}

	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start haproxy container: %v", err)
	}

	// Give the socket a moment to appear.
	socketPath = filepath.Join(socketDir, "admin.sock")
	for range 20 {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	return socketPath, func() { c.Terminate(ctx) } //nolint:errcheck
}

func newTestClient(t *testing.T, socketPath string) *haproxySocketClient {
	t.Helper()
	return &haproxySocketClient{socketPath: socketPath, backend: "shoal"}
}

func TestIntegration_AddAndShow(t *testing.T) {
	socketPath, cleanup := haproxyContainer(t)
	defer cleanup()

	h := newTestClient(t, socketPath)

	if err := h.AddServer("task-1", "127.0.0.1", 9001); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	servers, err := h.ShowServers()
	if err != nil {
		t.Fatalf("ShowServers: %v", err)
	}

	found := false
	for _, s := range servers {
		if s.Name == "task-1" {
			found = true
			if s.OpState != "2" {
				t.Errorf("expected op_state=2 (UP), got %q", s.OpState)
			}
		}
	}
	if !found {
		t.Errorf("task-1 not found in servers: %+v", servers)
	}
}

func TestIntegration_RemoveServer(t *testing.T) {
	socketPath, cleanup := haproxyContainer(t)
	defer cleanup()

	h := newTestClient(t, socketPath)

	if err := h.AddServer("task-2", "127.0.0.1", 9002); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := h.RemoveServer("task-2"); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}

	servers, err := h.ShowServers()
	if err != nil {
		t.Fatalf("ShowServers: %v", err)
	}
	for _, s := range servers {
		if s.Name == "task-2" {
			t.Errorf("task-2 should have been removed, still present: %+v", s)
		}
	}
}

func TestIntegration_AnnounceFlow(t *testing.T) {
	socketPath, cleanup := haproxyContainer(t)
	defer cleanup()

	h := newTestClient(t, socketPath)
	reg := newRegistry()
	s := &sidecar{reg: reg, haproxy: h, log: nopZerolog()}

	payload, err := shoalproto.MarshalAnnounceRequest(&shoalproto.AnnounceRequest{
		Addr:  "127.0.0.1",
		Port:  9003,
		State: cluster.StateReady,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	s.handleAnnounce("integration-task", payload)

	servers, err := h.ShowServers()
	if err != nil {
		t.Fatalf("ShowServers: %v", err)
	}
	found := false
	for _, srv := range servers {
		if srv.Addr == "127.0.0.1" && srv.Port == 9003 {
			found = true
		}
	}
	if !found {
		t.Errorf("announced backend not found in HAProxy servers: %+v", servers)
	}
}
