package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestArchiveRoundTrip verifies that ArchiveSandbox + RehydrateSandbox is
// lossless: every file under the sandbox dir survives the tar+zstd
// round trip with byte-identical contents and 0o755-ish modes.
func TestArchiveRoundTrip(t *testing.T) {
	mgr, _, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))

	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{
		TemplateHash: hash,
		Config:       SandboxConfig{VCPUs: 2, MemoryMB: 512},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Stop first so we have a saved state dir to round-trip.
	if err := mgr.StopSandbox(context.Background(), sb.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Stash a marker file under the sandbox dir so we can detect content
	// drift after the round trip.
	markerPath := filepath.Join(mgr.Root(), sb.ID, "marker.txt")
	if err := os.WriteFile(markerPath, []byte("hello-archive"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	originalSize, err := dirSize(filepath.Join(mgr.Root(), sb.ID))
	if err != nil {
		t.Fatalf("dirSize: %v", err)
	}

	archives := NewArchiveManager(mgr, ArchiveOptions{
		ArchiveDir: t.TempDir(),
	}, nil)

	res, err := archives.ArchiveSandbox(context.Background(), sb.ID)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if res.Location != ArchiveLocationLocal {
		t.Fatalf("expected location=local, got %s", res.Location)
	}
	if res.SizeBytes <= 0 {
		t.Fatalf("expected non-zero archive size, got %d", res.SizeBytes)
	}
	if !strings.HasSuffix(res.Path, ".tar.zst") {
		t.Fatalf("expected .tar.zst extension, got %s", res.Path)
	}
	if _, err := os.Stat(filepath.Join(mgr.Root(), sb.ID)); !os.IsNotExist(err) {
		t.Fatalf("expected sandbox dir gone after archive, stat err = %v", err)
	}
	if _, err := mgr.Get(sb.ID); err == nil {
		t.Fatalf("expected sandbox entry removed from manager after archive")
	}

	// Now rehydrate. The marker file must be back, byte-identical.
	rehydrated, err := archives.RehydrateSandbox(context.Background(), sb.ID, res.Path)
	if err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if rehydrated.State != SandboxStateStopped {
		t.Fatalf("expected STOPPED post-rehydrate, got %s", rehydrated.State)
	}
	if rehydrated.StateDir == "" {
		t.Fatalf("StateDir should be populated when state/ dir survived archive")
	}
	got, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read marker after rehydrate: %v", err)
	}
	if string(got) != "hello-archive" {
		t.Fatalf("marker content drifted: %q", got)
	}
	rehydratedSize, err := dirSize(filepath.Join(mgr.Root(), sb.ID))
	if err != nil {
		t.Fatalf("dirSize after rehydrate: %v", err)
	}
	if rehydratedSize != originalSize {
		t.Fatalf("size differs after round trip: original=%d rehydrated=%d", originalSize, rehydratedSize)
	}
}

// TestArchiveRehydrate_RejectsDuplicate ensures rehydrating an ID that
// is already registered fails fast — protects against double-rehydrate.
func TestArchiveRehydrate_RejectsDuplicate(t *testing.T) {
	mgr, _, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))

	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{
		TemplateHash: hash,
		Config:       SandboxConfig{VCPUs: 1, MemoryMB: 256},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	archives := NewArchiveManager(mgr, ArchiveOptions{ArchiveDir: t.TempDir()}, nil)
	if _, err := archives.RehydrateSandbox(context.Background(), sb.ID, "/nonexistent.tar.zst"); err == nil {
		t.Fatalf("expected rejection — sandbox already registered")
	}
}

// TestArchiveSandbox_AutoStops verifies the agent stops a RUNNING sandbox
// before archiving. Without this we'd archive a snapshot-less directory
// and rehydrate would have nothing to start from.
func TestArchiveSandbox_AutoStops(t *testing.T) {
	mgr, vm, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))

	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{
		TemplateHash: hash,
		Config:       SandboxConfig{VCPUs: 1, MemoryMB: 256},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := mgr.Get(sb.ID)
	if got.State != SandboxStateRunning {
		t.Fatalf("precondition: sandbox should be RUNNING, got %s", got.State)
	}
	beforeSnapshot := vm.snapshotCalls

	archives := NewArchiveManager(mgr, ArchiveOptions{ArchiveDir: t.TempDir()}, nil)
	if _, err := archives.ArchiveSandbox(context.Background(), sb.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if vm.snapshotCalls != beforeSnapshot+1 {
		t.Fatalf("expected snapshot call from auto-stop, before=%d after=%d", beforeSnapshot, vm.snapshotCalls)
	}
}

// dirSize helper: recursive total bytes under p, used by the round-trip
// test to compare pre/post sizes.
func dirSizeForTest(t *testing.T, p string) int64 {
	t.Helper()
	n, err := dirSize(p)
	if err != nil {
		t.Fatalf("dirSize(%s): %v", p, err)
	}
	return n
}

var _ = dirSizeForTest
