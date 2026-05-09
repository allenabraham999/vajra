// Package agent — exec.go owns guest-side command execution. The wire
// protocol is a single JSON-line request followed by a single JSON-line
// response on vsock port GuestExecPort, served by the guest-agent binary
// in scripts/guest-agent. The shape is intentionally untyped (no "op"
// field) so SandboxManager can talk to existing guest builds without a
// version handshake.
package agent

import (
	"context"
	"fmt"
	"time"
)

// Vsock port assignments. These mirror the constants in
// scripts/guest-agent/main.go — a fresh connection on the host targets
// the guest's listener for that op. Any change here must be mirrored in
// the guest agent.
const (
	// GuestFilesPort serves file upload/download/list (see files.go).
	GuestFilesPort uint32 = 5253
	// GuestTerminalPort serves PTY sessions (see proxy/terminal.go).
	GuestTerminalPort uint32 = 5254
	// GuestForwardPort serves bytes-into-localhost-port (see forward.go).
	GuestForwardPort uint32 = 5255
)

// DefaultExecTimeout caps a single ExecCommand if the caller passes
// timeout <= 0. 30s matches the guest's default and is plenty for the
// short shell calls SDKs typically issue.
const DefaultExecTimeout = 30 * time.Second

// ExecCommand opens a vsock conn to GuestExecPort, sends a JSON-line
// `{command, timeout_ms}`, reads back an ExecResult, and returns it.
// On any wire error the underlying connection is closed by the deferred
// Close — there's no streaming side-channel to clean up.
//
// The timeout bounds both the dial and the round-trip wait. The caller's
// ctx may shorten the wait further; whichever fires first wins.
func (m *SandboxManager) ExecCommand(ctx context.Context, id, command string, timeout time.Duration) (*ExecResult, error) {
	sb, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	if sb.State != SandboxStateRunning {
		return nil, fmt.Errorf("sandbox: not running (state %s)", sb.State)
	}
	if timeout <= 0 {
		timeout = DefaultExecTimeout
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := m.dialer.Dial(dialCtx, sb.VsockSocketPath, GuestExecPort)
	if err != nil {
		return nil, fmt.Errorf("sandbox: dial vsock: %w", err)
	}
	defer conn.Close()

	req := execWireRequest{Command: command, TimeoutMS: timeout.Milliseconds()}
	if err := writeJSONLine(conn, req); err != nil {
		return nil, fmt.Errorf("sandbox: send exec: %w", err)
	}
	var res ExecResult
	if err := readJSONLine(conn, &res); err != nil {
		return nil, fmt.Errorf("sandbox: read exec: %w", err)
	}
	return &res, nil
}

// execWireRequest is the JSON shape the guest expects on port 5252.
// Kept private so callers stay on the ExecCommand surface.
type execWireRequest struct {
	Command   string `json:"command"`
	TimeoutMS int64  `json:"timeout_ms"`
}
