// Package agent — logbuf.go provides the in-memory, per-sandbox log
// retention behind the dashboard logs viewer. LogTap is an slog.Handler
// that tees every record carrying a sandbox identifier into a LogBuffer,
// which the GET /sandbox/{id}/logs handler later replays.
package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// LogEntry is one structured log line surfaced to the per-sandbox logs
// viewer. Source is "agent" for entries the agent itself emitted and
// "guest" for lines read off the microVM console.
type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
}

// logBufferPerSandbox caps how many recent entries are retained for a
// single sandbox. The buffer is a ring: once full, the oldest entry is
// dropped. 1000 lines is plenty for a dashboard tail without letting a
// chatty sandbox pin unbounded memory.
const logBufferPerSandbox = 1000

// LogBuffer is an in-memory, per-sandbox ring of recent agent log
// entries. It is written by LogTap (on the logging path) and read by the
// agent's GET /sandbox/{id}/logs handler. Safe for concurrent use.
type LogBuffer struct {
	mu    sync.Mutex
	max   int
	bySbx map[string][]LogEntry
}

// NewLogBuffer returns an empty LogBuffer retaining logBufferPerSandbox
// entries per sandbox.
func NewLogBuffer() *LogBuffer {
	return &LogBuffer{max: logBufferPerSandbox, bySbx: map[string][]LogEntry{}}
}

// Append records one entry for sandboxID, evicting the oldest entry once
// the per-sandbox cap is reached.
func (b *LogBuffer) Append(sandboxID string, e LogEntry) {
	if b == nil || sandboxID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ring := append(b.bySbx[sandboxID], e)
	if len(ring) > b.max {
		ring = ring[len(ring)-b.max:]
	}
	b.bySbx[sandboxID] = ring
}

// Tail returns up to n most-recent entries for sandboxID, oldest first.
// n <= 0 returns every retained entry. The result is a copy — callers
// may retain or mutate it freely.
func (b *LogBuffer) Tail(sandboxID string, n int) []LogEntry {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ring := b.bySbx[sandboxID]
	if n > 0 && len(ring) > n {
		ring = ring[len(ring)-n:]
	}
	out := make([]LogEntry, len(ring))
	copy(out, ring)
	return out
}

// Drop discards all retained entries for sandboxID. Called when a
// sandbox is destroyed so the buffer does not leak memory for the
// lifetime of the agent process.
func (b *LogBuffer) Drop(sandboxID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.bySbx, sandboxID)
	b.mu.Unlock()
}

// sandboxIDKeys are the attribute keys that, when present on a log
// record, associate that record with a sandbox. The agent layer logs
// with "id"/"sandbox_id" while the vmm shim uses "vm_id", so LogTap
// watches all three.
var sandboxIDKeys = map[string]bool{"id": true, "sandbox_id": true, "vm_id": true}

// LogTap is an slog.Handler that forwards every record to an inner
// handler (the agent's normal JSON stdout handler) and, additionally,
// copies any record carrying a sandbox-identifying attribute into a
// LogBuffer so it can be replayed by the per-sandbox logs viewer.
type LogTap struct {
	inner slog.Handler
	buf   *LogBuffer
	attrs []slog.Attr
}

// NewLogTap wraps inner so records are both emitted normally and teed
// into buf. Install it with slog.New(NewLogTap(jsonHandler, buf)).
func NewLogTap(inner slog.Handler, buf *LogBuffer) *LogTap {
	return &LogTap{inner: inner, buf: buf}
}

// Enabled delegates to the inner handler.
func (t *LogTap) Enabled(ctx context.Context, lvl slog.Level) bool {
	return t.inner.Enabled(ctx, lvl)
}

// Handle tees the record into the LogBuffer when it carries a sandbox
// id, then delegates to the inner handler.
func (t *LogTap) Handle(ctx context.Context, rec slog.Record) error {
	if t.buf != nil {
		if id, msg := t.extract(rec); id != "" {
			t.buf.Append(id, LogEntry{
				Timestamp: rec.Time,
				Source:    "agent",
				Level:     levelName(rec.Level),
				Message:   msg,
			})
		}
	}
	return t.inner.Handle(ctx, rec)
}

// WithAttrs accumulates attrs so a logger built via .With("id", x) still
// resolves a sandbox id at Handle time.
func (t *LogTap) WithAttrs(as []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(t.attrs)+len(as))
	merged = append(merged, t.attrs...)
	merged = append(merged, as...)
	return &LogTap{inner: t.inner.WithAttrs(as), buf: t.buf, attrs: merged}
}

// WithGroup delegates to the inner handler; the tap itself is grouping-
// agnostic since it only inspects flat sandbox-id attributes.
func (t *LogTap) WithGroup(name string) slog.Handler {
	return &LogTap{inner: t.inner.WithGroup(name), buf: t.buf, attrs: t.attrs}
}

// extract pulls the sandbox id (if any) and renders a human-readable
// message — the record message plus its non-id attributes — for the
// buffer. WithAttrs values are considered before record attributes.
func (t *LogTap) extract(rec slog.Record) (string, string) {
	id := ""
	var fields []string
	consider := func(a slog.Attr) {
		if sandboxIDKeys[a.Key] {
			if s := a.Value.String(); s != "" && id == "" {
				id = s
			}
			return
		}
		fields = append(fields, a.Key+"="+a.Value.String())
	}
	for _, a := range t.attrs {
		consider(a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		consider(a)
		return true
	})
	msg := rec.Message
	if len(fields) > 0 {
		msg += " " + strings.Join(fields, " ")
	}
	return id, msg
}

// levelName maps an slog.Level onto the INFO/WARN/ERROR vocabulary the
// logs viewer colour-codes.
func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	default:
		return "INFO"
	}
}
