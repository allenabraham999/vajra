// Package master — handlers_sandbox.go is the sandbox CRUD + lifecycle
// surface (create / list / get / stop / start / destroy / exec /
// snapshot). Every endpoint is account-scoped via the auth middleware.
//
// Synchronous lifecycle: this milestone runs the dispatcher RPC inline
// with the request, returning 201 on success or a 5xx on failure. That
// keeps the wire contract simple while async/operation-poll wiring is
// still being designed; the OperationTracker is still updated so
// observability is in place when we flip the switch.
package master

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// dispatchTimeout caps a single sandbox-lifecycle agent call. Snapshot
// takes longest in practice; 60s is generous but bounded.
const dispatchTimeout = 60 * time.Second

// createSandboxRequest is the body of POST /v1/sandboxes.
type createSandboxRequest struct {
	Name       string `json:"name"`
	Source     string `json:"source"` // "image" | "snapshot"
	TemplateID string `json:"template_id,omitempty"`
	SnapshotID string `json:"snapshot_id,omitempty"`
	VCPUs      int    `json:"vcpus"`
	MemoryMB   int    `json:"memory_mb"`
	DiskGB     int    `json:"disk_gb"`
	Region     string `json:"region,omitempty"`
}

// validate returns a user-facing error for malformed input.
func (req *createSandboxRequest) validate() error {
	if req.Name == "" {
		return fmt.Errorf("name is required")
	}
	switch req.Source {
	case "image":
		if req.TemplateID == "" {
			return fmt.Errorf("template_id required for source=image")
		}
	case "snapshot":
		if req.SnapshotID == "" {
			return fmt.Errorf("snapshot_id required for source=snapshot")
		}
	default:
		return fmt.Errorf(`source must be "image" or "snapshot"`)
	}
	if req.VCPUs <= 0 || req.MemoryMB <= 0 || req.DiskGB <= 0 {
		return fmt.Errorf("vcpus, memory_mb, disk_gb must be positive")
	}
	return nil
}

// createSandbox runs validate → quota → schedule → create row → dispatch.
func (h *Handlers) createSandbox(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	var body createSandboxRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := body.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	h.executeCreate(w, r, accountID, &body)
}

// executeCreate runs the full create pipeline for a (possibly
// synthesised) request body. createSandbox calls in after JSON
// decoding; the snapshot-restore endpoints call in with a body they
// constructed themselves.
func (h *Handlers) executeCreate(w http.ResponseWriter, r *http.Request, accountID string, body *createSandboxRequest) {
	// Resolve the source artifact so we can both 404 early and pull the
	// template hash the agent needs. For snapshot-sourced sandboxes we
	// load the snapshot row (and stamp the resulting sandbox to the
	// snapshot's node so the restore happens locally).
	templateID, templateHash, snapshot, err := h.resolveSource(r.Context(), accountID, body)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "source not found")
			return
		}
		h.log().Error("createSandbox: resolve source", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	cluster, node, err := h.Scheduler.Schedule(r.Context(), SchedRequest{
		AccountID: accountID,
		VCPUs:     body.VCPUs, MemoryMB: body.MemoryMB, DiskGB: body.DiskGB,
		Region: body.Region,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrQuotaExceeded):
			writeErr(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, ErrNoCluster), errors.Is(err, ErrNoCapacity):
			writeErr(w, http.StatusServiceUnavailable, err.Error())
		default:
			h.log().Error("createSandbox: schedule", "err", err)
			writeErr(w, http.StatusInternalServerError, "scheduling failed")
		}
		return
	}
	// Snapshot-sourced sandboxes pin to the originating node so the
	// restore can reuse local state. Override the scheduler choice.
	if snapshot != nil {
		hostNode, err := h.Store.Nodes().GetByID(r.Context(), snapshot.NodeID)
		if err != nil {
			h.log().Error("createSandbox: load snapshot node", "err", err)
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		node = hostNode
	}

	sandboxID, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := h.now().UTC()
	sb := &models.Sandbox{
		ID: sandboxID, Name: body.Name, AccountID: accountID,
		NodeID: &node.ID, ClusterID: &cluster.ID,
		TemplateID: templateID,
		State:      models.SandboxStatePending,
		Config: models.SandboxConfig{
			VCPUs: body.VCPUs, MemoryMB: body.MemoryMB, DiskGB: body.DiskGB,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := h.Store.Sandboxes().Create(r.Context(), sb); err != nil {
		h.log().Error("createSandbox: insert", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	opID, _ := h.Tracker.Start(r.Context(), accountID, sandboxID, models.OperationTypeCreate)

	// Move PENDING → CREATING before the dispatcher call so an outside
	// observer never sees us stuck in PENDING.
	if err := h.Store.Sandboxes().UpdateState(r.Context(), accountID, sandboxID, models.SandboxStateCreating); err != nil {
		h.log().Error("createSandbox: state transition", "err", err)
	}

	dispatchCtx, cancel := context.WithTimeout(r.Context(), dispatchTimeout)
	defer cancel()
	_, dispatchErr := h.Pool.ClientFor(node).CreateSandbox(dispatchCtx, CreateSandboxRequest{
		ID:           sandboxID,
		TemplateHash: templateHash,
		Config: SandboxConfig{
			VCPUs: body.VCPUs, MemoryMB: body.MemoryMB, DiskGB: body.DiskGB,
		},
	})
	if dispatchErr != nil {
		_ = h.Store.Sandboxes().UpdateState(r.Context(), accountID, sandboxID, models.SandboxStateError)
		_ = h.Tracker.Complete(r.Context(), opID, dispatchErr)
		h.log().Error("createSandbox: dispatch", "err", dispatchErr, "sandbox_id", sandboxID)
		writeErr(w, http.StatusBadGateway, "agent dispatch failed: "+dispatchErr.Error())
		return
	}
	if err := h.Store.Sandboxes().UpdateState(r.Context(), accountID, sandboxID, models.SandboxStateRunning); err != nil {
		h.log().Error("createSandbox: post-dispatch state", "err", err)
	}
	_ = h.Tracker.Complete(r.Context(), opID, nil)

	out, err := h.Store.Sandboxes().GetByID(r.Context(), accountID, sandboxID)
	if err != nil {
		h.log().Error("createSandbox: refetch", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, sandboxWithOp{Sandbox: out, OperationID: opID})
}

// sandboxWithOp augments a sandbox row with the operation id so the
// caller can poll for completion (once async lifecycle lands).
type sandboxWithOp struct {
	*models.Sandbox
	OperationID string `json:"operation_id,omitempty"`
}

// resolveSource validates and loads the artifact a create request is
// referencing. Returns templateID + hash + (optional) snapshot row.
func (h *Handlers) resolveSource(ctx context.Context, accountID string, body *createSandboxRequest) (string, string, *models.Snapshot, error) {
	if body.Source == "image" {
		tmpl, err := h.Store.Templates().GetByID(ctx, accountID, body.TemplateID)
		if err != nil {
			return "", "", nil, err
		}
		return tmpl.ID, tmpl.Hash, nil, nil
	}
	// snapshot source: load the snapshot, then resolve the originating
	// sandbox's template so the agent has a hash to match against.
	snap, err := h.Store.Snapshots().GetByID(ctx, accountID, body.SnapshotID)
	if err != nil {
		return "", "", nil, err
	}
	srcSandbox, err := h.Store.Sandboxes().GetByID(ctx, accountID, snap.SandboxID)
	if err != nil {
		return "", "", nil, err
	}
	tmpl, err := h.Store.Templates().GetByID(ctx, accountID, srcSandbox.TemplateID)
	if err != nil {
		return "", "", nil, err
	}
	return tmpl.ID, tmpl.Hash, snap, nil
}

// listSandboxes returns the calling account's sandboxes, paginated.
func (h *Handlers) listSandboxes(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	out, err := h.Store.Sandboxes().ListByAccount(r.Context(), accountID, parseListOpts(r))
	if err != nil {
		h.log().Error("listSandboxes", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// getSandbox returns one sandbox by ID.
func (h *Handlers) getSandbox(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	sb, err := h.loadSandbox(r, accountID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.log().Error("getSandbox", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

// loadSandbox fetches the path-id sandbox; callers translate the error
// (ErrNotFound → 404, anything else → 500) at the HTTP boundary.
func (h *Handlers) loadSandbox(r *http.Request, accountID string) (*models.Sandbox, error) {
	id := pathID(r)
	if id == "" {
		return nil, errors.New("missing sandbox id")
	}
	sb, err := h.Store.Sandboxes().GetByID(r.Context(), accountID, id)
	if err != nil {
		return nil, err
	}
	return sb, nil
}

// resolveSandboxAndNode is shared by every action endpoint: load the
// sandbox by ID (account-scoped), then load the node it lives on so we
// can pick an AgentClient. Writes an HTTP error and returns ok=false on
// any failure path.
func (h *Handlers) resolveSandboxAndNode(w http.ResponseWriter, r *http.Request) (string, *models.Sandbox, *models.Node, bool) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return "", nil, nil, false
	}
	sb, err := h.loadSandbox(r, accountID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return "", nil, nil, false
	}
	if sb.NodeID == nil || *sb.NodeID == "" {
		writeErr(w, http.StatusConflict, "sandbox has no placement")
		return "", nil, nil, false
	}
	node, err := h.Store.Nodes().GetByID(r.Context(), *sb.NodeID)
	if err != nil {
		h.log().Error("loadNode", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return "", nil, nil, false
	}
	return accountID, sb, node, true
}
