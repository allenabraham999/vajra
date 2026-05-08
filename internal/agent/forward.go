// Package agent — forward.go is the host-side helper for the
// guest-agent's TCP forwarding port (5255). DialForward opens a vsock
// connection, writes the JSON handshake, validates the "ok" reply, and
// returns the upgraded byte stream — at that point the connection
// behaves like a TCP socket directly to localhost:port inside the VM.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// DialForwardTimeout caps the dial + handshake. Once the bridge is up
// we don't impose a deadline — the proxy controls long-running streams.
const DialForwardTimeout = 10 * time.Second

// forwardWireRequest is the JSON line written before the connection
// flips into raw bytes. Mirrors scripts/guest-agent/forward.go.
type forwardWireRequest struct {
	Port int    `json:"port"`
	Host string `json:"host,omitempty"`
}

// DialForward opens a vsock connection to the guest's forward port,
// completes the {port,host} handshake, and returns the upgraded byte
// stream. The returned ReadWriteCloser is buffered on the read side so
// any bytes sent right after "ok\n" aren't lost.
//
// Closing the returned stream tears the bridge down; the guest will
// observe EOF on its half and tear down the localhost connection it
// established.
func (m *SandboxManager) DialForward(ctx context.Context, sandboxID string, port int, host string) (io.ReadWriteCloser, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("forward: invalid port %d", port)
	}
	sb, err := m.lookup(sandboxID)
	if err != nil {
		return nil, err
	}
	if sb.State != SandboxStateRunning {
		return nil, fmt.Errorf("sandbox: not running (state %s)", sb.State)
	}
	dialCtx, cancel := context.WithTimeout(ctx, DialForwardTimeout)
	defer cancel()
	conn, err := m.dialer.Dial(dialCtx, sb.VsockSocket, GuestForwardPort)
	if err != nil {
		return nil, fmt.Errorf("forward: dial vsock: %w", err)
	}
	if err := writeJSONLine(conn, forwardWireRequest{Port: port, Host: host}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("forward: send handshake: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("forward: read handshake reply: %w", err)
	}
	line = strings.TrimSpace(line)
	if line != "ok" {
		_ = conn.Close()
		// The guest writes "error: ..." on rejection; surface it.
		return nil, fmt.Errorf("forward: guest rejected: %s", line)
	}
	return &bridgedConn{rwc: conn, br: br}, nil
}

// DialTerminal opens a vsock connection to the guest's terminal port and
// completes the {command, cols, rows} handshake. The returned stream is
// the framed PTY pipe (host→guest = type+length+payload, guest→host =
// raw output bytes). The proxy/terminal handler is the only caller.
func (m *SandboxManager) DialTerminal(ctx context.Context, sandboxID string, opts TerminalOpts) (io.ReadWriteCloser, error) {
	sb, err := m.lookup(sandboxID)
	if err != nil {
		return nil, err
	}
	if sb.State != SandboxStateRunning {
		return nil, fmt.Errorf("sandbox: not running (state %s)", sb.State)
	}
	dialCtx, cancel := context.WithTimeout(ctx, DialForwardTimeout)
	defer cancel()
	conn, err := m.dialer.Dial(dialCtx, sb.VsockSocket, GuestTerminalPort)
	if err != nil {
		return nil, fmt.Errorf("terminal: dial vsock: %w", err)
	}
	header := terminalWireRequest{
		Command: opts.Command,
		Args:    opts.Args,
		Cols:    opts.Cols,
		Rows:    opts.Rows,
		Env:     opts.Env,
	}
	if err := writeJSONLine(conn, header); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("terminal: send handshake: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("terminal: read handshake reply: %w", err)
	}
	line = strings.TrimSpace(line)
	if line != "ok" {
		_ = conn.Close()
		return nil, fmt.Errorf("terminal: guest rejected: %s", line)
	}
	return &bridgedConn{rwc: conn, br: br}, nil
}

// TerminalOpts carries the handshake parameters. Cols/Rows = 0 leaves
// the guest with the kernel default until a resize frame is sent.
type TerminalOpts struct {
	Command string
	Args    []string
	Cols    uint16
	Rows    uint16
	Env     []string
}

// terminalWireRequest mirrors scripts/guest-agent/terminal.go.
type terminalWireRequest struct {
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Cols    uint16   `json:"cols,omitempty"`
	Rows    uint16   `json:"rows,omitempty"`
	Env     []string `json:"env,omitempty"`
}

// bridgedConn wraps a vsock conn whose first bytes (post-handshake) may
// already be in a bufio.Reader. Reads pass through the buffered reader
// to avoid losing those bytes; writes go straight to the underlying
// connection.
type bridgedConn struct {
	rwc io.ReadWriteCloser
	br  *bufio.Reader
}

// Read pulls bytes through the buffered reader.
func (c *bridgedConn) Read(p []byte) (int, error) { return c.br.Read(p) }

// Write writes verbatim to the underlying vsock connection.
func (c *bridgedConn) Write(p []byte) (int, error) { return c.rwc.Write(p) }

// Close tears down the underlying connection.
func (c *bridgedConn) Close() error { return c.rwc.Close() }

// trimJSONLine pulls one JSON envelope off the bufio reader and decodes
// it into v. Currently unused — kept for future helpers that want to
// peek at a typed reply without consuming the rest of the stream.
func trimJSONLine(br *bufio.Reader, v any) error {
	line, err := br.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}
