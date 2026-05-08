// Package agent — snapshot.go is the host-side helper for taking a
// snapshot of a running sandbox into an arbitrary directory. It powers
// the master-driven snapshot flow: master picks the destination path,
// asks the agent for a snapshot, and the agent reports back the size.
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// errSandboxMissing is returned by SnapshotIntoDir when the requested
// sandbox ID isn't registered with the manager. Kept as a sentinel so
// handlers can translate it to HTTP 404.
var errSandboxMissing = errors.New("sandbox: not found")

// SnapshotResult is the on-disk artifact of a Snapshot call. SizeBytes
// is the total bytes written under Path (via filepath.WalkDir).
type SnapshotResult struct {
	Path      string
	SizeBytes int64
}

// SnapshotIntoDir asks the underlying VMM to snapshot the sandbox into
// destDir, walks destDir to compute total bytes written, and returns
// both. Resume is set to true so the caller's sandbox continues running
// after the snapshot — matches the master contract documented in
// internal/master/dispatcher.go.
func (m *SandboxManager) SnapshotIntoDir(ctx context.Context, id, destDir string) (*SnapshotResult, error) {
	sb, err := m.lookup(id)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errSandboxMissing, id)
	}
	if sb.State != SandboxStateRunning && sb.State != SandboxStatePaused {
		return nil, fmt.Errorf("snapshot: cannot snapshot in state %s", sb.State)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("snapshot: mkdir: %w", err)
	}
	if err := m.vmm.SnapshotVM(ctx, sb.APISocket, destDir, true); err != nil {
		return nil, fmt.Errorf("snapshot: vmm: %w", err)
	}
	size, _ := dirSize(destDir) // best-effort; snapshot succeeded either way
	return &SnapshotResult{Path: destDir, SizeBytes: size}, nil
}

