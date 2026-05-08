//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"strings"
	"time"
)

// execRequest is the JSON-line wire shape on the exec port. Untyped (no
// "op" field) so it stays backward compatible with the original
// SandboxManager.ExecCommand call.
type execRequest struct {
	Command   string `json:"command"`
	TimeoutMS int64  `json:"timeout_ms"`
}

// execResult mirrors agent.ExecResult.
type execResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// errorResult is sent when we couldn't start the command at all.
type errorResult struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error"`
}

// serveExec reads a single JSON-line request, runs it under /bin/sh, and
// writes a single JSON-line response. The connection is single-shot to
// keep the protocol stateless.
func serveExec(c net.Conn, l *prefixLogger) {
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		l.Printf("read: %v", err)
		return
	}
	var req execRequest
	if len(strings.TrimSpace(string(line))) == 0 {
		// Empty line — treat as a host-side liveness probe.
		_, _ = c.Write([]byte(`{"exit_code":0,"stdout":"","stderr":""}` + "\n"))
		return
	}
	if err := json.Unmarshal(line, &req); err != nil {
		writeJSON(c, errorResult{ExitCode: 1, Error: "decode: " + err.Error()})
		return
	}
	if req.Command == "" {
		writeJSON(c, errorResult{ExitCode: 1, Error: "empty command"})
		return
	}
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	res := runShell(ctx, req.Command)
	writeJSON(c, res)
}

// runShell executes command via /bin/sh -c and captures stdout/stderr.
// On context cancellation the child receives SIGKILL via cmd.Wait —
// that's the standard exec.CommandContext semantics.
func runShell(ctx context.Context, command string) execResult {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := execResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else {
			// Failed to start. Use a sentinel exit code that maps
			// to "command not found" semantics.
			res.ExitCode = 127
			res.Stderr += err.Error()
		}
	}
	return res
}

// writeJSON encodes v plus a trailing newline. Any encode error here
// only matters in tests; on a real vsock the only realistic failure is a
// half-closed peer, in which case there is nothing to do but close.
func writeJSON(c net.Conn, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		return
	}
	buf = append(buf, '\n')
	_, _ = c.Write(buf)
}
