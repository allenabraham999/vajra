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

// dispatchReconcileTimeout bounds the GetSandbox probe master fires after
// a CreateSandbox dispatch failure. Short on purpose: we only want a
// best-effort answer to "did the agent actually create it?"; if the agent
// doesn't reply quickly we fall back to ERROR and let the reconciler sort
// it out on its next pass.
const dispatchReconcileTimeout = 5 * time.Second

// createPollInterval and createPollTimeout govern the background poller
// master spawns after dispatching a create. The agent's CreateSandbox is
// async and returns 202 immediately with state=CREATING; the poller
// drives the DB row to RUNNING (or ERROR) when the agent's goroutine
// finishes. 1s ticks keep the user-visible latency low; the 60s ceiling
// matches the pessimistic restore time on cold paths.
const (
	createPollInterval = 1 * time.Second
	createPollTimeout  = 60 * time.Second
)

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

	agent := h.Pool.ClientFor(node)
	dispatchCtx, cancel := context.WithTimeout(r.Context(), dispatchTimeout)
	defer cancel()
	_, dispatchErr := agent.CreateSandbox(dispatchCtx, CreateSandboxRequest{
		ID:           sandboxID,
		TemplateHash: templateHash,
		Config: SandboxConfig{
			VCPUs: body.VCPUs, MemoryMB: body.MemoryMB, DiskGB: body.DiskGB,
		},
	})
	if dispatchErr != nil {
		if !h.handleDispatchError(w, agent, accountID, sandboxID, opID, dispatchErr) {
			return
		}
		// Falls through: the agent reports CREATING/RUNNING and we've
		// already updated the DB / kicked the poller. Refetch and return
		// the current row to the user.
	} else {
		// Happy path: agent accepted the create asynchronously. Leave
		// the DB row in CREATING and let the poller drive it forward.
		go h.pollAgentCreate(accountID, sandboxID, opID, agent)
	}

	out, err := h.Store.Sandboxes().GetByID(r.Context(), accountID, sandboxID)
	if err != nil {
		h.log().Error("createSandbox: refetch", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, sandboxWithOp{Sandbox: out, OperationID: opID})
}

// handleDispatchError resolves the four cases that can land us here when
// agent.CreateSandbox returns non-nil. It returns ok=true when the
// caller should keep going (refetch + 201) and ok=false when an HTTP
// error has already been written and the handler must return.
//
//	probe RUNNING  → DB → RUNNING, complete tracker, ok=true (transport
//	                 lost the 202 but the agent finished anyway)
//	probe CREATING → fire poller, ok=true (agent received the request,
//	                 still working)
//	probe 404      → DB → DESTROYED, fail tracker, write 502, ok=false
//	                 (agent has no record; nothing to reconcile to)
//	probe other    → DB → ERROR, fail tracker, write 502, ok=false
//	                 (we genuinely can't tell what's running)
func (h *Handlers) handleDispatchError(
	w http.ResponseWriter,
	agent *AgentClient, accountID, sandboxID, opID string,
	dispatchErr error,
) bool {
	probeCtx, cancel := context.WithTimeout(context.Background(), dispatchReconcileTimeout)
	defer cancel()
	sbView, probeErr := agent.GetSandbox(probeCtx, sandboxID)
	if probeErr == nil && sbView != nil {
		mapped, mapOK := mapAgentState(sbView.State)
		switch {
		case mapOK && mapped == models.SandboxStateRunning:
			if err := h.Store.Sandboxes().UpdateState(probeCtx, accountID, sandboxID, models.SandboxStateRunning); err != nil {
				h.log().Error("createSandbox: reconcile-after-dispatch update", "err", err, "sandbox_id", sandboxID)
			}
			if sb, _ := h.Store.Sandboxes().GetByID(probeCtx, accountID, sandboxID); sb != nil {
				h.recordUsageStart(probeCtx, accountID, sandboxID, sb.Config)
			}
			_ = h.Tracker.Complete(probeCtx, opID, nil)
			h.log().Warn("createSandbox: dispatch reported error but agent has sandbox running",
				"sandbox_id", sandboxID, "dispatch_err", dispatchErr)
			return true
		case mapOK && mapped == models.SandboxStateCreating:
			// Agent got the request and is still working. Spawn the
			// poller and let it drive the DB row to RUNNING.
			h.log().Warn("createSandbox: dispatch reported error but agent still creating",
				"sandbox_id", sandboxID, "dispatch_err", dispatchErr)
			go h.pollAgentCreate(accountID, sandboxID, opID, agent)
			return true
		}
	}
	if isAgentNotFound(probeErr) {
		// Agent has no record of this sandbox — the create never took
		// effect (request lost in transit, or agent crashed before
		// BeginCreate ran). Mark DESTROYED so the row matches reality
		// and the user sees it as a failed create rather than a stuck
		// CREATING row that the reconciler will eventually rewrite.
		if err := h.Store.Sandboxes().UpdateState(probeCtx, accountID, sandboxID, models.SandboxStateDestroyed); err != nil {
			h.log().Error("createSandbox: dispatch destroyed update", "err", err, "sandbox_id", sandboxID)
		}
		_ = h.Tracker.Complete(probeCtx, opID, dispatchErr)
		h.log().Error("createSandbox: agent has no record after dispatch failure",
			"sandbox_id", sandboxID, "dispatch_err", dispatchErr)
		writeErr(w, http.StatusBadGateway, "agent dispatch failed: "+dispatchErr.Error())
		return false
	}
	_ = h.Store.Sandboxes().UpdateState(probeCtx, accountID, sandboxID, models.SandboxStateError)
	_ = h.Tracker.Complete(probeCtx, opID, dispatchErr)
	h.log().Error("createSandbox: dispatch", "err", dispatchErr, "sandbox_id", sandboxID, "probe_err", probeErr)
	writeErr(w, http.StatusBadGateway, "agent dispatch failed: "+dispatchErr.Error())
	return false
}

// pollAgentCreate drives a CREATING sandbox to its terminal state
// (RUNNING or ERROR) by polling the agent. Called as a goroutine from
// executeCreate, so it owns its own context and never returns to the
// HTTP handler. createPollTimeout caps how long we'll wait before
// declaring failure ourselves; if the agent never finishes we mark
// ERROR rather than letting the row hang in CREATING forever.
func (h *Handlers) pollAgentCreate(accountID, sandboxID, opID string, agent *AgentClient) {
	ctx, cancel := context.WithTimeout(context.Background(), createPollTimeout)
	defer cancel()
	ticker := time.NewTicker(createPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			err := fmt.Errorf("create poll timeout after %s", createPollTimeout)
			if uerr := h.Store.Sandboxes().UpdateState(context.Background(), accountID, sandboxID, models.SandboxStateError); uerr != nil {
				h.log().Error("createSandbox: poll timeout update", "err", uerr, "sandbox_id", sandboxID)
			}
			_ = h.Tracker.Complete(context.Background(), opID, err)
			h.log().Error("createSandbox: poll timeout", "sandbox_id", sandboxID)
			return
		case <-ticker.C:
			sbView, err := agent.GetSandbox(ctx, sandboxID)
			if err != nil {
				if isAgentNotFound(err) {
					// Agent forgot about the sandbox mid-create —
					// e.g. the goroutine panicked and was reaped.
					// Surface as ERROR; the periodic reconciler can
					// later mark it DESTROYED if needed.
					if uerr := h.Store.Sandboxes().UpdateState(context.Background(), accountID, sandboxID, models.SandboxStateError); uerr != nil {
						h.log().Error("createSandbox: poll missing update", "err", uerr, "sandbox_id", sandboxID)
					}
					_ = h.Tracker.Complete(context.Background(), opID, err)
					h.log().Error("createSandbox: agent forgot sandbox during poll", "sandbox_id", sandboxID)
					return
				}
				// Transient probe failure — keep ticking.
				h.log().Debug("createSandbox: poll transient error",
					"sandbox_id", sandboxID, "err", err)
				continue
			}
			mapped, ok := mapAgentState(sbView.State)
			if !ok {
				continue
			}
			switch mapped {
			case models.SandboxStateRunning:
				if uerr := h.Store.Sandboxes().UpdateState(context.Background(), accountID, sandboxID, models.SandboxStateRunning); uerr != nil {
					h.log().Error("createSandbox: poll running update", "err", uerr, "sandbox_id", sandboxID)
				}
				if sb, _ := h.Store.Sandboxes().GetByID(context.Background(), accountID, sandboxID); sb != nil {
					h.recordUsageStart(context.Background(), accountID, sandboxID, sb.Config)
				}
				_ = h.Tracker.Complete(context.Background(), opID, nil)
				return
			case models.SandboxStateError:
				if uerr := h.Store.Sandboxes().UpdateState(context.Background(), accountID, sandboxID, models.SandboxStateError); uerr != nil {
					h.log().Error("createSandbox: poll error update", "err", uerr, "sandbox_id", sandboxID)
				}
				_ = h.Tracker.Complete(context.Background(), opID, fmt.Errorf("agent reported ERROR"))
				return
			}
			// CREATING (or any other intermediate) — keep waiting.
		}
	}
}

// isAgentNotFound reports whether err is the agent's "no such sandbox"
// signal. The dispatcher wraps non-2xx responses in *httpError so we
// recognise the 404 the agent's handleGet returns when the manager has
// no entry for the requested ID.
func isAgentNotFound(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.Status == http.StatusNotFound
	}
	return false
}

// recordUsageStart opens a sandbox_usage interval for billing. Best-
// effort: a failed insert never blocks the lifecycle, but we log so
// operators notice persistent mis-tracking. Skip when the store has
// no Usage backend (test fakes return nil).
func (h *Handlers) recordUsageStart(ctx context.Context, accountID, sandboxID string, cfg models.SandboxConfig) {
	usage := h.Store.Usage()
	if usage == nil {
		return
	}
	if err := usage.RecordStart(ctx, accountID, sandboxID, cfg, h.now().UTC()); err != nil {
		h.log().Warn("usage RecordStart failed", "err", err, "sandbox_id", sandboxID)
	}
}

// recordUsageStop closes any open sandbox_usage interval. See
// recordUsageStart on best-effort error handling.
func (h *Handlers) recordUsageStop(ctx context.Context, sandboxID string) {
	usage := h.Store.Usage()
	if usage == nil {
		return
	}
	if err := usage.RecordStop(ctx, sandboxID, h.now().UTC()); err != nil {
		h.log().Warn("usage RecordStop failed", "err", err, "sandbox_id", sandboxID)
	}
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
