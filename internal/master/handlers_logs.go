// Package master — handlers_logs.go serves the per-sandbox logs viewer.
//
// GET /v1/sandboxes/{id}/logs returns a point-in-time merge of three
// streams: master control-plane events (the operations audit log), the
// owning agent's log tail, and any captured guest console output.
//
// GET /v1/sandboxes/{id}/logs/stream is a WebSocket that re-collects the
// same merge on a short interval and pushes newly-seen entries to the
// browser. Like the terminal endpoint it authenticates from ?token=
// because a browser WebSocket cannot set an Authorization header.
package master

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// LogEntry is one line in a sandbox's merged log stream. Source is one
// of "master", "agent", or "guest"; Level is INFO, WARN, or ERROR.
type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
}

// logsResponse is the JSON body of GET /v1/sandboxes/{id}/logs.
type logsResponse struct {
	Entries []LogEntry `json:"entries"`
}

const (
	// defaultLogTail is the entry count returned when ?tail= is absent.
	defaultLogTail = 500
	// maxLogTail caps ?tail= so a caller cannot ask master to merge an
	// unbounded history.
	maxLogTail = 5000
	// logAgentTimeout bounds the call to the owning agent's logs
	// endpoint; kept short so a slow agent never stalls the viewer.
	logAgentTimeout = 5 * time.Second
	// logStreamPoll is how often the WebSocket handler re-collects logs
	// and pushes newly-seen entries to the browser.
	logStreamPoll = 1500 * time.Millisecond
)

// listSandboxLogs serves GET /v1/sandboxes/{id}/logs. It merges master
// events with the owning agent's log tail and any guest console output,
// then returns the most-recent ?tail= entries matching ?source=.
func (h *Handlers) listSandboxLogs(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	sb, err := h.loadSandbox(r, accountID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	source := logSourceFilter(r.URL.Query().Get("source"))
	tail := parseTail(r.URL.Query().Get("tail"))

	entries := h.collectLogs(r.Context(), accountID, sb, source, tail)
	writeJSON(w, http.StatusOK, logsResponse{Entries: entries})
}

// streamSandboxLogs serves GET /v1/sandboxes/{id}/logs/stream — a
// WebSocket carrying live log entries. It authenticates from ?token=.
func (h *Handlers) streamSandboxLogs(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if id == "" {
		http.Error(w, "missing sandbox id", http.StatusBadRequest)
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	accountID, status := resolveToken(r.Context(), h.Signer, h.Store.APIKeys(), token)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	sb, err := h.Store.Sandboxes().GetByID(r.Context(), accountID, id)
	if err != nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	source := logSourceFilter(r.URL.Query().Get("source"))

	ws, err := wsUpgrade(w, r)
	if err != nil {
		h.log().Warn("logs stream: ws upgrade", "err", err, "sandbox_id", id)
		return
	}
	defer ws.close()
	h.log().Info("logs stream: opened", "sandbox_id", id, "account_id", accountID)
	h.bridgeLogStream(r.Context(), ws, accountID, sb, source)
}

// collectLogs gathers and merges the requested log sources for sb,
// returning at most tail entries ordered oldest-first.
func (h *Handlers) collectLogs(ctx context.Context, accountID string, sb *models.Sandbox, source string, tail int) []LogEntry {
	var entries []LogEntry
	if source == "all" || source == "master" {
		entries = append(entries, h.masterEvents(ctx, accountID, sb)...)
	}
	if source == "all" || source == "agent" || source == "guest" {
		entries = append(entries, h.agentEvents(ctx, sb, source, tail)...)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Timestamp.Before(entries[j].Timestamp)
	})
	if tail > 0 && len(entries) > tail {
		entries = entries[len(entries)-tail:]
	}
	return entries
}

// masterEvents renders the control-plane history for sb from the
// operations audit log: a start line per operation plus a completion or
// failure line once it finishes.
func (h *Handlers) masterEvents(ctx context.Context, accountID string, sb *models.Sandbox) []LogEntry {
	out := []LogEntry{{
		Timestamp: sb.CreatedAt,
		Source:    "master",
		Level:     "INFO",
		Message:   "sandbox created (state " + string(sb.State) + ")",
	}}
	ops, err := h.Store.Operations().ListBySandbox(ctx, accountID, sb.ID, store.ListOpts{Limit: maxLogTail})
	if err != nil {
		h.log().Warn("listSandboxLogs: operations", "err", err, "sandbox_id", sb.ID)
		return out
	}
	for _, op := range ops {
		out = append(out, LogEntry{
			Timestamp: op.StartedAt,
			Source:    "master",
			Level:     "INFO",
			Message:   string(op.Type) + " started",
		})
		if op.CompletedAt == nil {
			continue
		}
		e := LogEntry{Timestamp: *op.CompletedAt, Source: "master"}
		if op.Status == models.OperationStatusFailed {
			e.Level = "ERROR"
			e.Message = string(op.Type) + " failed"
			if op.Error != nil && *op.Error != "" {
				e.Message += ": " + *op.Error
			}
		} else {
			e.Level = "INFO"
			e.Message = string(op.Type) + " completed"
		}
		out = append(out, e)
	}
	return out
}

// agentEvents fetches the owning agent's log tail for sb and filters it
// to the requested source. An unreachable agent yields a single
// synthetic WARN line rather than failing the whole request.
func (h *Handlers) agentEvents(ctx context.Context, sb *models.Sandbox, source string, tail int) []LogEntry {
	if sb.NodeID == nil || *sb.NodeID == "" {
		return nil
	}
	node, err := h.Store.Nodes().GetByID(ctx, *sb.NodeID)
	if err != nil {
		h.log().Warn("listSandboxLogs: load node", "err", err, "sandbox_id", sb.ID)
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, logAgentTimeout)
	defer cancel()
	raw, err := h.Pool.ClientFor(node).SandboxLogs(callCtx, sb.ID, tail)
	if err != nil {
		return []LogEntry{{
			Timestamp: time.Now().UTC(),
			Source:    "agent",
			Level:     "WARN",
			Message:   "agent logs unavailable: " + err.Error(),
		}}
	}
	out := make([]LogEntry, 0, len(raw))
	for _, e := range raw {
		if source != "all" && e.Source != source {
			continue
		}
		out = append(out, LogEntry{
			Timestamp: e.Timestamp,
			Source:    e.Source,
			Level:     e.Level,
			Message:   e.Message,
		})
	}
	return out
}

// bridgeLogStream is the server→client pump for the logs WebSocket. It
// re-collects logs every logStreamPoll and sends any entry not yet sent
// on this connection as a JSON array text frame. A reader goroutine
// detects the client close so the pump can stop.
func (h *Handlers) bridgeLogStream(ctx context.Context, ws *wsConn, accountID string, sb *models.Sandbox, source string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The logs stream is server→client only, but we must still drain
	// inbound frames so a client close (or a dropped socket) cancels the
	// pump promptly. The reader never writes, so no write lock is needed.
	go func() {
		for {
			frame, err := ws.readFrame()
			if err != nil {
				cancel()
				return
			}
			if frame.Opcode == wsOpcodeClose {
				cancel()
				return
			}
		}
	}()

	ticker := time.NewTicker(logStreamPoll)
	defer ticker.Stop()
	sent := map[string]bool{}
	for {
		// Reload the sandbox each tick so late scheduling or a migration
		// (a changed node placement) is picked up without reconnecting.
		if fresh, err := h.Store.Sandboxes().GetByID(ctx, accountID, sb.ID); err == nil {
			sb = fresh
		}
		var fresh []LogEntry
		for _, e := range h.collectLogs(ctx, accountID, sb, source, maxLogTail) {
			k := logEntryKey(e)
			if sent[k] {
				continue
			}
			sent[k] = true
			fresh = append(fresh, e)
		}
		if len(fresh) > 0 {
			if payload, err := json.Marshal(fresh); err == nil {
				if werr := ws.writeText(payload); werr != nil {
					return
				}
			}
		}
		// Bound the dedupe set; on overflow drop it and let the browser
		// (which also dedupes) absorb the one-off resend.
		if len(sent) > 4*maxLogTail {
			sent = map[string]bool{}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// logEntryKey is the dedupe identity of an entry across stream polls.
func logEntryKey(e LogEntry) string {
	return e.Source + "|" + e.Timestamp.UTC().Format(time.RFC3339Nano) + "|" + e.Message
}

// logSourceFilter normalises the ?source= query value; anything
// unrecognised falls back to "all".
func logSourceFilter(s string) string {
	switch s {
	case "master", "agent", "guest":
		return s
	default:
		return "all"
	}
}

// parseTail clamps the ?tail= query value to (0, maxLogTail], defaulting
// to defaultLogTail when absent or invalid.
func parseTail(s string) int {
	if s == "" {
		return defaultLogTail
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return defaultLogTail
	}
	if n > maxLogTail {
		return maxLogTail
	}
	return n
}
