// Package master — handlers_sandbox_actions.go contains the per-sandbox
// lifecycle action endpoints (stop/start/destroy/exec/snapshot/list-
// snapshots). They share the same shape: load sandbox + node, run the
// dispatch, persist the result; the helpers here keep that flow tidy.
package master

import (
	"context"
	"errors"
	"net/http"
	"path"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// execRequest is the body of POST /v1/sandboxes/{id}/exec.
type execRequest struct {
	Command   string `json:"command"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
}

// execSandbox runs a command inside the sandbox and returns the result.
// Sandbox must be in RUNNING state — we do not auto-start.
func (h *Handlers) execSandbox(w http.ResponseWriter, r *http.Request) {
	_, sb, node, ok := h.resolveSandboxAndNode(w, r)
	if !ok {
		return
	}
	if sb.State != models.SandboxStateRunning {
		writeErr(w, http.StatusConflict, "sandbox not running")
		return
	}
	var body execRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Command == "" {
		writeErr(w, http.StatusBadRequest, "command is required")
		return
	}
	timeout := time.Duration(body.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout+5*time.Second)
	defer cancel()
	res, err := h.Pool.ClientFor(node).ExecCommand(ctx, sb.ID, body.Command, timeout)
	if err != nil {
		h.log().Error("execSandbox: dispatch", "err", err, "sandbox_id", sb.ID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	h.touchActivity(r.Context(), sb.ID)
	writeJSON(w, http.StatusOK, res)
}

// stopSandbox transitions a RUNNING sandbox through STOPPING to STOPPED.
func (h *Handlers) stopSandbox(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, lifecycleParams{
		opType:  models.OperationTypeStop,
		precond: func(s models.SandboxState) bool { return s == models.SandboxStateRunning },
		mid:     models.SandboxStateStopping,
		final:   models.SandboxStateStopped,
		dispatch: func(ctx context.Context, c *AgentClient, id string) error {
			return c.StopSandbox(ctx, id)
		},
	})
}

// startSandbox transitions a STOPPED sandbox through CREATING to RUNNING.
//
// The state machine doesn't model a STOPPED→CREATING reverse leg, so
// this endpoint does not preflight a transition and instead trusts the
// agent's StartSandbox call. We mark the row as RUNNING on success.
func (h *Handlers) startSandbox(w http.ResponseWriter, r *http.Request) {
	accountID, sb, node, ok := h.resolveSandboxAndNode(w, r)
	if !ok {
		return
	}
	if sb.State != models.SandboxStateStopped {
		writeErr(w, http.StatusConflict, "sandbox not stopped")
		return
	}
	opID, _ := h.Tracker.Start(r.Context(), accountID, sb.ID, models.OperationTypeStart)

	dispatchCtx, cancel := context.WithTimeout(r.Context(), dispatchTimeout)
	defer cancel()
	if err := h.Pool.ClientFor(node).StartSandbox(dispatchCtx, sb.ID); err != nil {
		_ = h.Store.Sandboxes().UpdateState(r.Context(), accountID, sb.ID, models.SandboxStateError)
		_ = h.Tracker.Complete(r.Context(), opID, err)
		h.log().Error("startSandbox: dispatch", "err", err, "sandbox_id", sb.ID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := h.Store.Sandboxes().UpdateState(r.Context(), accountID, sb.ID, models.SandboxStateRunning); err != nil {
		h.log().Error("startSandbox: state", "err", err)
	}
	h.recordUsageStart(r.Context(), accountID, sb.ID, sb.Config)
	h.touchActivity(r.Context(), sb.ID)
	_ = h.Tracker.Complete(r.Context(), opID, nil)

	out, _ := h.Store.Sandboxes().GetByID(r.Context(), accountID, sb.ID)
	writeJSON(w, http.StatusAccepted, sandboxWithOp{Sandbox: out, OperationID: opID})
}

// destroySandbox tears down a sandbox; the row stays around as
// DESTROYED for audit history.
func (h *Handlers) destroySandbox(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, lifecycleParams{
		opType:  models.OperationTypeDestroy,
		precond: func(s models.SandboxState) bool { return s != models.SandboxStateDestroyed && s != models.SandboxStateError },
		mid:     models.SandboxStateDestroying,
		final:   models.SandboxStateDestroyed,
		dispatch: func(ctx context.Context, c *AgentClient, id string) error {
			return c.DestroySandbox(ctx, id)
		},
		// destroy can come from any non-terminal state, so the FSM
		// might reject the mid-state transition — fall through.
		allowMidStateFailure: true,
	})
}

// lifecycleParams bundles per-action parameters for the shared
// lifecycle path.
type lifecycleParams struct {
	opType   models.OperationType
	precond  func(models.SandboxState) bool
	mid      models.SandboxState
	final    models.SandboxState
	dispatch func(context.Context, *AgentClient, string) error
	// allowMidStateFailure: if true, ignore an FSM rejection on the
	// mid-state UpdateState (used by destroy which can run from any
	// non-terminal state).
	allowMidStateFailure bool
}

// lifecycleAction is the canonical body of stop/destroy: precond → mid
// state → dispatch → final state, with operation tracking around it.
func (h *Handlers) lifecycleAction(w http.ResponseWriter, r *http.Request, p lifecycleParams) {
	accountID, sb, node, ok := h.resolveSandboxAndNode(w, r)
	if !ok {
		return
	}
	if !p.precond(sb.State) {
		writeErr(w, http.StatusConflict, "sandbox state "+string(sb.State)+" not eligible")
		return
	}
	opID, _ := h.Tracker.Start(r.Context(), accountID, sb.ID, p.opType)

	if err := h.Store.Sandboxes().UpdateState(r.Context(), accountID, sb.ID, p.mid); err != nil && !p.allowMidStateFailure {
		h.log().Error("lifecycle: mid-state", "err", err, "op", p.opType)
		_ = h.Tracker.Complete(r.Context(), opID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	dispatchCtx, cancel := context.WithTimeout(r.Context(), dispatchTimeout)
	defer cancel()
	if err := p.dispatch(dispatchCtx, h.Pool.ClientFor(node), sb.ID); err != nil {
		_ = h.Store.Sandboxes().UpdateState(r.Context(), accountID, sb.ID, models.SandboxStateError)
		_ = h.Tracker.Complete(r.Context(), opID, err)
		h.log().Error("lifecycle: dispatch", "err", err, "op", p.opType, "sandbox_id", sb.ID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := h.Store.Sandboxes().UpdateState(r.Context(), accountID, sb.ID, p.final); err != nil {
		h.log().Error("lifecycle: final-state", "err", err, "op", p.opType)
	}
	h.writeSandboxStateCache(r.Context(), sb.ID, p.final)
	h.publishStateChange(r.Context(), sb, sb.State, p.final)
	if p.final == models.SandboxStateDestroyed {
		h.invalidateSandboxStateCache(r.Context(), sb.ID)
		h.decrAccountSandboxCount(r.Context(), accountID)
		h.publishSandboxDestroyed(r.Context(), sb)
	}
	if p.final == models.SandboxStateStopped || p.final == models.SandboxStateDestroyed {
		h.recordUsageStop(r.Context(), sb.ID)
	}
	_ = h.Tracker.Complete(r.Context(), opID, nil)

	out, _ := h.Store.Sandboxes().GetByID(r.Context(), accountID, sb.ID)
	writeJSON(w, http.StatusAccepted, sandboxWithOp{Sandbox: out, OperationID: opID})
}

// snapshotRequest is the body of POST /v1/sandboxes/{id}/snapshot.
type snapshotRequest struct {
	Name string `json:"name"`
}

// snapshotSandbox tells the agent to snapshot a sandbox's state and
// records the resulting Snapshot row. The agent picks the on-disk
// destination path; we suggest one based on the snapshot ID.
func (h *Handlers) snapshotSandbox(w http.ResponseWriter, r *http.Request) {
	accountID, sb, node, ok := h.resolveSandboxAndNode(w, r)
	if !ok {
		return
	}
	// The dashboard's "New snapshot" button posts no JSON body, so the
	// body is optional here; an empty one just means "use a default
	// name". decodeBody would otherwise fail the request with
	// "decode body: EOF".
	var body snapshotRequest
	if err := decodeBodyOptional(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name == "" {
		body.Name = "snapshot-" + h.now().UTC().Format("20060102-150405")
	}
	if sb.State != models.SandboxStateRunning && sb.State != models.SandboxStatePaused && sb.State != models.SandboxStateStopped {
		writeErr(w, http.StatusConflict, "sandbox state "+string(sb.State)+" not eligible")
		return
	}

	snapshotID, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	destPath := path.Join("/var/lib/vajra/snapshots", snapshotID)

	opID, _ := h.Tracker.Start(r.Context(), accountID, sb.ID, models.OperationTypeSnapshot)

	dispatchCtx, cancel := context.WithTimeout(r.Context(), dispatchTimeout)
	defer cancel()
	res, err := h.Pool.ClientFor(node).SnapshotSandbox(dispatchCtx, sb.ID, body.Name, destPath)
	if err != nil {
		_ = h.Tracker.Complete(r.Context(), opID, err)
		h.log().Error("snapshotSandbox: dispatch", "err", err, "sandbox_id", sb.ID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	snap := &models.Snapshot{
		ID: snapshotID, SandboxID: sb.ID, AccountID: accountID,
		NodeID: node.ID, StoragePath: res.SnapshotPath, SizeBytes: res.SizeBytes,
		CreatedAt: h.now().UTC(),
	}
	if err := h.Store.Snapshots().Create(r.Context(), snap); err != nil {
		h.log().Error("snapshotSandbox: persist", "err", err)
		_ = h.Tracker.Complete(r.Context(), opID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	_ = h.Tracker.Complete(r.Context(), opID, nil)
	writeJSON(w, http.StatusCreated, snap)
}

// listSandboxSnapshots returns every snapshot taken from a sandbox.
func (h *Handlers) listSandboxSnapshots(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing sandbox id")
		return
	}
	// Confirm ownership of the sandbox first so we don't leak snapshot
	// existence across accounts.
	if _, err := h.Store.Sandboxes().GetByID(r.Context(), accountID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeErr(w, translateStoreErr(err), "lookup failed")
		return
	}
	out, err := h.Store.Snapshots().ListBySandbox(r.Context(), accountID, id, parseListOpts(r))
	if err != nil {
		h.log().Error("listSandboxSnapshots", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
