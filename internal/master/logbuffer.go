// Package master — logbuffer.go is the in-process ring buffer behind the
// admin panel's "Logs" tab. master logs to stdout as JSON; that stream is
// not readable from inside the process, so LogBuffer tees every slog
// record into a bounded in-memory ring that GET /v1/admin/logs reads back.
//
// This is the one deliberate exception to master's stateless rule: the
// buffer is purely diagnostic, per-replica, and lost on restart — it
// holds no sandbox state and never feeds a scheduling or billing
// decision, so multiple replicas behind a load balancer still "just work"
// (each simply shows its own recent lines).
package master

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// defaultLogBufferSize is how many recent records LogBuffer keeps. ~1k
// lines is a few minutes of master chatter — enough to debug a live
// incident from the dashboard without growing the heap meaningfully.
const defaultLogBufferSize = 1000

// defaultAdminLogTail is the page size when a caller omits ?tail=.
const defaultAdminLogTail = 200

// AdminLogEntry is one captured log record, flattened for JSON transport to
// the admin dashboard.
type AdminLogEntry struct {
	Time  time.Time         `json:"time"`
	Level string            `json:"level"`
	Msg   string            `json:"msg"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// LogBuffer is a fixed-capacity ring of the most recent AdminLogEntry values.
// Safe for concurrent use by the slog handler (writer) and the admin
// logs handler (reader).
type LogBuffer struct {
	mu      sync.Mutex
	entries []AdminLogEntry
	cap     int
}

// NewLogBuffer returns a LogBuffer holding up to size entries. A
// non-positive size falls back to defaultLogBufferSize.
func NewLogBuffer(size int) *LogBuffer {
	if size <= 0 {
		size = defaultLogBufferSize
	}
	return &LogBuffer{cap: size, entries: make([]AdminLogEntry, 0, size)}
}

// append adds e, evicting the oldest entry once the ring is full.
func (b *LogBuffer) append(e AdminLogEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) >= b.cap {
		copy(b.entries, b.entries[1:])
		b.entries[len(b.entries)-1] = e
		return
	}
	b.entries = append(b.entries, e)
}

// LogQuery filters a Tail read. The zero value returns the most recent
// defaultAdminLogTail entries unfiltered.
type LogQuery struct {
	Tail      int    // max entries returned (most recent); <=0 → defaultAdminLogTail
	Level     string // minimum level DEBUG|INFO|WARN|ERROR; "" → all levels
	Source    string // "agent" keeps only node-tagged records; else all
	SandboxID string // substring match against the message and attr values
}

// Tail returns the most recent entries matching q, oldest-first.
func (b *LogBuffer) Tail(q LogQuery) []AdminLogEntry {
	if q.Tail <= 0 {
		q.Tail = defaultAdminLogTail
	}
	minRank := levelRank(q.Level)
	wantAgent := strings.EqualFold(strings.TrimSpace(q.Source), "agent")
	needle := strings.TrimSpace(q.SandboxID)

	b.mu.Lock()
	src := make([]AdminLogEntry, len(b.entries))
	copy(src, b.entries)
	b.mu.Unlock()

	out := make([]AdminLogEntry, 0, q.Tail)
	for _, e := range src {
		if q.Level != "" && levelRank(e.Level) < minRank {
			continue
		}
		if wantAgent && !isAgentEntry(e) {
			continue
		}
		if needle != "" && !entryContains(e, needle) {
			continue
		}
		out = append(out, e)
	}
	if len(out) > q.Tail {
		out = out[len(out)-q.Tail:]
	}
	return out
}

// levelRank maps a slog level name to a comparable rank. Unknown names
// rank lowest so an unrecognised filter never hides everything.
func levelRank(level string) int {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "ERROR":
		return 3
	case "WARN", "WARNING":
		return 2
	case "INFO":
		return 1
	case "DEBUG":
		return 0
	default:
		return 0
	}
}

// isAgentEntry reports whether a record concerns a node agent — the
// dispatcher and reconciler tag every such line with node_id, so the
// "agent" source filter keys on that attribute.
func isAgentEntry(e AdminLogEntry) bool {
	_, ok := e.Attrs["node_id"]
	return ok
}

// entryContains reports whether needle appears in the message or any
// attribute value — used to filter logs down to one sandbox.
func entryContains(e AdminLogEntry, needle string) bool {
	if strings.Contains(e.Msg, needle) {
		return true
	}
	for _, v := range e.Attrs {
		if strings.Contains(v, needle) {
			return true
		}
	}
	return false
}

// bufHandler is a slog.Handler that captures every record into a
// LogBuffer before delegating to an inner handler (the real JSON stdout
// writer). It is installed once at startup; see NewLogBufferHandler.
type bufHandler struct {
	inner slog.Handler
	buf   *LogBuffer
	attrs []slog.Attr
}

// NewLogBufferHandler wraps inner so every record it processes is also
// captured in buf. The returned handler is safe for concurrent use.
func NewLogBufferHandler(inner slog.Handler, buf *LogBuffer) slog.Handler {
	return &bufHandler{inner: inner, buf: buf}
}

// Enabled defers to the inner handler.
func (h *bufHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle captures the record then forwards it to the inner handler.
func (h *bufHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := make(map[string]string, r.NumAttrs()+len(h.attrs))
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.buf.append(AdminLogEntry{
		Time:  r.Time,
		Level: r.Level.String(),
		Msg:   r.Message,
		Attrs: attrs,
	})
	return h.inner.Handle(ctx, r)
}

// WithAttrs threads the preset attrs through both the inner handler and
// the captured copy, so a logger built with .With(...) still records
// those fields in the buffer.
func (h *bufHandler) WithAttrs(as []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(as))
	merged = append(merged, h.attrs...)
	merged = append(merged, as...)
	return &bufHandler{inner: h.inner.WithAttrs(as), buf: h.buf, attrs: merged}
}

// WithGroup defers to the inner handler. Group nesting is not reflected
// in the flattened buffer entry — acceptable for a diagnostic view.
func (h *bufHandler) WithGroup(name string) slog.Handler {
	return &bufHandler{inner: h.inner.WithGroup(name), buf: h.buf, attrs: h.attrs}
}
