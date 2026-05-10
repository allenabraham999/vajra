package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestMigrateSandbox_RoundTrip wires source and target SandboxManagers
// behind real HTTP servers, runs MigrateSandbox source→target, and
// verifies (a) the source dir is gone (b) the target rehydrated and
// has the same marker file (c) the source manager dropped its entry.
func TestMigrateSandbox_RoundTrip(t *testing.T) {
	source, _, sourceCache := newTestManager(t)
	target, _, _ := newTestManager(t)

	hash := seedTemplate(t, sourceCache, []byte("rootfs-migrate"))

	sb, err := source.CreateSandbox(context.Background(), CreateRequest{
		TemplateHash: hash,
		Config:       SandboxConfig{VCPUs: 1, MemoryMB: 256},
	})
	if err != nil {
		t.Fatalf("source create: %v", err)
	}
	if err := source.StopSandbox(context.Background(), sb.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	markerPath := filepath.Join(source.Root(), sb.ID, "marker.txt")
	if err := os.WriteFile(markerPath, []byte("hello-migrate"), 0o644); err != nil {
		t.Fatalf("marker: %v", err)
	}

	// Spin up a target HTTP server with the same handler the agent uses.
	srv := NewServer(":0", target, nil, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandbox/receive", srv.handleReceive)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	res, err := source.MigrateSandbox(context.Background(), sb.ID, ts.URL, "")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.BytesSent <= 0 {
		t.Fatalf("expected non-zero bytes_sent, got %d", res.BytesSent)
	}

	// Source side: dir gone, manager entry gone.
	if _, err := source.Get(sb.ID); err == nil {
		t.Fatalf("expected source manager to drop sandbox entry")
	}
	if _, err := os.Stat(filepath.Join(source.Root(), sb.ID)); !os.IsNotExist(err) {
		t.Fatalf("expected source dir gone, stat err = %v", err)
	}

	// Target side: sandbox registered in STOPPED, marker present.
	got, err := target.Get(sb.ID)
	if err != nil {
		t.Fatalf("target get: %v", err)
	}
	if got.State != SandboxStateStopped {
		t.Fatalf("target state: expected STOPPED, got %s", got.State)
	}
	targetMarker := filepath.Join(target.Root(), sb.ID, "marker.txt")
	body, err := os.ReadFile(targetMarker)
	if err != nil {
		t.Fatalf("read target marker: %v", err)
	}
	if string(body) != "hello-migrate" {
		t.Fatalf("target marker content: %q", body)
	}
}

// TestReceiveSandbox_RejectsExisting ensures the target side refuses to
// overwrite a sandbox that's already registered (defense against split
// brain or replay).
func TestReceiveSandbox_RejectsExisting(t *testing.T) {
	target, _, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs-existing"))

	sb, err := target.CreateSandbox(context.Background(), CreateRequest{
		TemplateHash: hash,
		Config:       SandboxConfig{VCPUs: 1, MemoryMB: 128},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Pretend a tar comes in for the same ID — should fail at registration
	// before we touch the existing dir.
	if _, err := target.ReceiveSandbox(context.Background(), sb.ID, nopReader{}); err == nil {
		t.Fatalf("expected rejection — sandbox already registered")
	}
}

// TestMigrateSandbox_RejectsEmptyTarget guards against accidentally
// invoking migrate with no destination.
func TestMigrateSandbox_RejectsEmptyTarget(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	if _, err := mgr.MigrateSandbox(context.Background(), "sb-x", "", ""); err == nil {
		t.Fatalf("expected error on empty target_addr")
	}
}

type nopReader struct{}

func (nopReader) Read(p []byte) (int, error) { return 0, nil }
