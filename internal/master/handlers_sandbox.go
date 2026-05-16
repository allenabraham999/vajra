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

	"github.com/allenabraham999/vajra/internal/cache"
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
// matches the pessimistic restore time on a warm node.
//
// autoscaleCreatePollTimeout is the longer ceiling we use after the
// autoscaler has launched a fresh node. The first sandbox on a brand
// new node pays the full cold-cache cost (template pull + qcow2 backing
// warm-up + first snapshot restore), which routinely exceeds 60s.
// Firing the short timeout there used to mark the row ERROR while the
// agent was actually mid-restore; the reconciler then saw DB=ERROR /
// agent=RUNNING, classified the VM as a ghost, and destroyed working
// sandboxes. We use this longer cap on the autoscale path and rely on
// reconcilerRecoverRunningSandbox in the reconciler to defend against
// any remaining races.
const (
	createPollInterval         = 1 * time.Second
	createPollTimeout          = 60 * time.Second
	autoscaleCreatePollTimeout = 5 * time.Minute
)

// createSandboxRequest is the body of POST /v1/sandboxes.
//
// AutoStopMinutes / AutoArchiveMinutes are optional. Omitting them or
// passing zero falls back to models.DefaultAutoStopMinutes /
// DefaultAutoArchiveMinutes. Passing a negative number disables the
// corresponding policy (encoded as 0 in the DB row).
type createSandboxRequest struct {
	Name               string `json:"name"`
	Source             string `json:"source"` // "image" | "snapshot"
	TemplateID         string `json:"template_id,omitempty"`
	SnapshotID         string `json:"snapshot_id,omitempty"`
	VCPUs              int    `json:"vcpus"`
	MemoryMB           int    `json:"memory_mb"`
	DiskGB             int    `json:"disk_gb"`
	Region             string `json:"region,omitempty"`
	AutoStopMinutes    *int   `json:"auto_stop_minutes,omitempty"`
	AutoArchiveMinutes *int   `json:"auto_archive_minutes,omitempty"`
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

	schedReq := SchedRequest{
		AccountID: accountID,
		VCPUs:     body.VCPUs, MemoryMB: body.MemoryMB, DiskGB: body.DiskGB,
		Region: body.Region,
	}
	cluster, node, err := h.Scheduler.Schedule(r.Context(), schedReq)
	if err != nil && errors.Is(err, ErrNoCapacity) {
		// At this point the user would normally see a 503. We diverge
		// based on what the autoscaler can actually do for this request:
		//
		//   1. Request exceeds the largest node we know how to launch →
		//      400. No amount of waiting fixes it.
		//   2. Snapshot-pinned create → still 503, because the user
		//      asked for a specific node that's full and we can't
		//      migrate the snapshot to a fresh box.
		//   3. Autoscaler enabled → take the async path: persist the
		//      sandbox in CREATING, kick a background goroutine that
		//      scales + retries Schedule + dispatches, return 201 now.
		//   4. Autoscaler disabled but request could fit somewhere →
		//      503 with a friendlier "waiting for autoscaler" message
		//      so the user knows they're not stuck on an oversize ask.
		if ExceedsAnyNodeCapacity(body.VCPUs, body.MemoryMB) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf(
				"requested resources exceed maximum node capacity (max %d vCPU, %d MB)",
				maxNodeVCPUs(), maxNodeMemoryMB()))
			return
		}
		if snapshot == nil && h.Autoscaler != nil && h.Autoscaler.Config.Enabled {
			h.startAsyncCreate(w, r, accountID, body, templateID, templateHash)
			return
		}
		writeErr(w, http.StatusServiceUnavailable,
			"all nodes at capacity, waiting for auto-scaler")
		return
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrQuotaExceeded):
			writeErr(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, ErrNoCluster):
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
	autoStop := models.DefaultAutoStopMinutes
	if body.AutoStopMinutes != nil {
		autoStop = *body.AutoStopMinutes
		if autoStop < 0 {
			autoStop = 0
		}
	}
	autoArchive := models.DefaultAutoArchiveMinutes
	if body.AutoArchiveMinutes != nil {
		autoArchive = *body.AutoArchiveMinutes
		if autoArchive < 0 {
			autoArchive = 0
		}
	}
	sb := &models.Sandbox{
		ID: sandboxID, Name: body.Name, AccountID: accountID,
		NodeID: &node.ID, ClusterID: &cluster.ID,
		TemplateID: templateID,
		State:      models.SandboxStatePending,
		Config: models.SandboxConfig{
			VCPUs: body.VCPUs, MemoryMB: body.MemoryMB, DiskGB: body.DiskGB,
		},
		AutoStopMinutes:    autoStop,
		AutoArchiveMinutes: autoArchive,
		LastActivity:       now,
		CreatedAt:          now, UpdatedAt: now,
	}
	if err := h.Store.Sandboxes().Create(r.Context(), sb); err != nil {
		h.log().Error("createSandbox: insert", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.incrAccountSandboxCount(r.Context(), accountID)
	h.writeSandboxStateCache(r.Context(), sandboxID, models.SandboxStatePending)
	opID, _ := h.Tracker.Start(r.Context(), accountID, sandboxID, models.OperationTypeCreate)

	// Move PENDING → CREATING before the dispatcher call so an outside
	// observer never sees us stuck in PENDING.
	if err := h.Store.Sandboxes().UpdateState(r.Context(), accountID, sandboxID, models.SandboxStateCreating); err != nil {
		h.log().Error("createSandbox: state transition", "err", err)
	}
	h.writeSandboxStateCache(r.Context(), sandboxID, models.SandboxStateCreating)
	h.publishStateChange(r.Context(), sb, models.SandboxStatePending, models.SandboxStateCreating)

	agent := h.Pool.ClientFor(node)
	dispatchCtx, cancel := context.WithTimeout(r.Context(), dispatchTimeout)
	defer cancel()
	createResp, dispatchErr := agent.CreateSandbox(dispatchCtx, CreateSandboxRequest{
		ID:           sandboxID,
		TemplateHash: templateHash,
		TemplateID:   templateID,
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
		// fromPool reflects whether the agent served this create from a
		// warm pre-warm pool member; the poller stamps it on the row
		// once the sandbox reaches RUNNING.
		fromPool := createResp != nil && createResp.FromPool
		go h.pollAgentCreate(accountID, sandboxID, opID, agent, createPollTimeout, fromPool)
	}

	out, err := h.Store.Sandboxes().GetByID(r.Context(), accountID, sandboxID)
	if err != nil {
		h.log().Error("createSandbox: refetch", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, sandboxWithOp{Sandbox: out, OperationID: opID})
}

// startAsyncCreate handles the "no capacity + autoscale enabled" branch.
// We persist a CREATING sandbox row with no node placement, return 201
// to the caller immediately, and let a background goroutine wait for
// the autoscaler to register a fresh node, then schedule + dispatch.
// The user polls GET /v1/sandboxes/{id} until state is RUNNING.
//
// The async path skips snapshot-sourced requests — those pin to a
// specific node by design (snapshot bytes are local), so a freshly
// launched box can't host them. Caller already filters that out.
func (h *Handlers) startAsyncCreate(
	w http.ResponseWriter, r *http.Request, accountID string,
	body *createSandboxRequest, templateID, templateHash string,
) {
	sandboxID, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := h.now().UTC()
	autoStop := models.DefaultAutoStopMinutes
	if body.AutoStopMinutes != nil {
		autoStop = *body.AutoStopMinutes
		if autoStop < 0 {
			autoStop = 0
		}
	}
	autoArchive := models.DefaultAutoArchiveMinutes
	if body.AutoArchiveMinutes != nil {
		autoArchive = *body.AutoArchiveMinutes
		if autoArchive < 0 {
			autoArchive = 0
		}
	}
	sb := &models.Sandbox{
		ID: sandboxID, Name: body.Name, AccountID: accountID,
		TemplateID: templateID,
		State:      models.SandboxStateCreating,
		Config: models.SandboxConfig{
			VCPUs: body.VCPUs, MemoryMB: body.MemoryMB, DiskGB: body.DiskGB,
		},
		AutoStopMinutes:    autoStop,
		AutoArchiveMinutes: autoArchive,
		LastActivity:       now,
		CreatedAt:          now, UpdatedAt: now,
	}
	if err := h.Store.Sandboxes().Create(r.Context(), sb); err != nil {
		h.log().Error("createSandbox: async insert", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.incrAccountSandboxCount(r.Context(), accountID)
	h.writeSandboxStateCache(r.Context(), sandboxID, models.SandboxStateCreating)
	opID, _ := h.Tracker.Start(r.Context(), accountID, sandboxID, models.OperationTypeCreate)

	h.log().Info("createSandbox: queued for autoscale",
		"account_id", accountID, "sandbox_id", sandboxID,
		"vcpus", body.VCPUs, "memory_mb", body.MemoryMB)

	go h.driveAsyncCreate(accountID, sandboxID, opID, *body, templateHash)

	writeJSON(w, http.StatusCreated, sandboxWithOp{Sandbox: sb, OperationID: opID})
}

// driveAsyncCreate is the background half of startAsyncCreate. It calls
// HandleNoCapacity to block until a fresh node registers, re-runs the
// scheduler so the new node gets scored like any other, updates the
// sandbox row with the placement, and dispatches to the agent. Any
// failure flips the sandbox to ERROR so the user's poll resolves
// instead of spinning forever.
func (h *Handlers) driveAsyncCreate(
	accountID, sandboxID, opID string,
	body createSandboxRequest, templateHash string,
) {
	ctx := context.Background()
	fail := func(err error) {
		h.log().Error("createSandbox: async drive failed",
			"sandbox_id", sandboxID, "err", err)
		if uerr := h.Store.Sandboxes().UpdateState(ctx, accountID, sandboxID, models.SandboxStateError); uerr != nil {
			h.log().Error("createSandbox: async error update", "err", uerr, "sandbox_id", sandboxID)
		}
		h.writeSandboxStateCache(ctx, sandboxID, models.SandboxStateError)
		_ = h.Tracker.Complete(ctx, opID, err)
	}

	if _, err := h.Autoscaler.HandleNoCapacity(ctx, body, accountID); err != nil {
		fail(fmt.Errorf("autoscale: %w", err))
		return
	}

	schedReq := SchedRequest{
		AccountID: accountID,
		VCPUs:     body.VCPUs, MemoryMB: body.MemoryMB, DiskGB: body.DiskGB,
		Region: body.Region,
	}
	cluster, node, err := h.Scheduler.Schedule(ctx, schedReq)
	if err != nil {
		fail(fmt.Errorf("post-scale schedule: %w", err))
		return
	}
	if err := h.Store.Sandboxes().UpdatePlacement(ctx, sandboxID, cluster.ID, node.ID); err != nil {
		fail(fmt.Errorf("update placement: %w", err))
		return
	}

	agent := h.Pool.ClientFor(node)
	dispatchCtx, cancel := context.WithTimeout(ctx, dispatchTimeout)
	defer cancel()
	createResp, dispatchErr := agent.CreateSandbox(dispatchCtx, CreateSandboxRequest{
		ID:           sandboxID,
		TemplateHash: templateHash,
		TemplateID:   body.TemplateID,
		Config: SandboxConfig{
			VCPUs: body.VCPUs, MemoryMB: body.MemoryMB, DiskGB: body.DiskGB,
		},
	})
	fromPool := createResp != nil && createResp.FromPool
	if dispatchErr != nil {
		// Run the same probe-then-reconcile dance as the sync path. We
		// have no HTTP response to write, so on a hard failure we just
		// flip to ERROR here instead.
		probeCtx, pcancel := context.WithTimeout(ctx, dispatchReconcileTimeout)
		defer pcancel()
		sbView, probeErr := agent.GetSandbox(probeCtx, sandboxID)
		if probeErr == nil && sbView != nil {
			if mapped, ok := mapAgentState(sbView.State); ok {
				switch mapped {
				case models.SandboxStateRunning:
					_ = h.Store.Sandboxes().UpdateState(ctx, accountID, sandboxID, models.SandboxStateRunning)
					h.writeSandboxStateCache(ctx, sandboxID, models.SandboxStateRunning)
					if sb, _ := h.Store.Sandboxes().GetByID(ctx, accountID, sandboxID); sb != nil {
						h.recordBootMetrics(ctx, accountID, sandboxID, sb.CreatedAt, false)
					}
					_ = h.Tracker.Complete(ctx, opID, nil)
					return
				case models.SandboxStateCreating:
					h.pollAgentCreate(accountID, sandboxID, opID, agent, autoscaleCreatePollTimeout, false)
					return
				}
			}
		}
		fail(fmt.Errorf("agent dispatch: %w", dispatchErr))
		return
	}
	h.pollAgentCreate(accountID, sandboxID, opID, agent, autoscaleCreatePollTimeout, fromPool)
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
				// The dispatch errored, so we never saw the agent's
				// from_pool flag — record the boot conservatively as a
				// cold (non-pool) create.
				h.recordBootMetrics(probeCtx, accountID, sandboxID, sb.CreatedAt, false)
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
			go h.pollAgentCreate(accountID, sandboxID, opID, agent, createPollTimeout, false)
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
// HTTP handler. timeout caps how long we'll wait before declaring
// failure ourselves; if the agent never finishes we mark ERROR rather
// than letting the row hang in CREATING forever. Synchronous-path
// callers pass createPollTimeout (60s, warm node); the autoscale path
// passes autoscaleCreatePollTimeout (5min, cold cache).
//
// fromPool carries the agent's from_pool flag from the create dispatch
// so the RUNNING transition can stamp it on the row alongside the boot
// time.
func (h *Handlers) pollAgentCreate(accountID, sandboxID, opID string, agent *AgentClient, timeout time.Duration, fromPool bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(createPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			err := fmt.Errorf("create poll timeout after %s", timeout)
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
				h.writeSandboxStateCache(context.Background(), sandboxID, models.SandboxStateRunning)
				if sb, _ := h.Store.Sandboxes().GetByID(context.Background(), accountID, sandboxID); sb != nil {
					h.recordBootMetrics(context.Background(), accountID, sandboxID, sb.CreatedAt, fromPool)
					h.recordUsageStart(context.Background(), accountID, sandboxID, sb.Config)
					h.publishSandboxCreated(context.Background(), sb)
					h.publishStateChange(context.Background(), sb, models.SandboxStateCreating, models.SandboxStateRunning)
				}
				_ = h.Tracker.Complete(context.Background(), opID, nil)
				return
			case models.SandboxStateError:
				if uerr := h.Store.Sandboxes().UpdateState(context.Background(), accountID, sandboxID, models.SandboxStateError); uerr != nil {
					h.log().Error("createSandbox: poll error update", "err", uerr, "sandbox_id", sandboxID)
				}
				// Prefer the agent's own failure reason (e.g. a template
				// that could not be distributed) over a generic message.
				reason := "agent reported ERROR"
				if sbView.Error != "" {
					reason = sbView.Error
				}
				h.log().Error("createSandbox: agent reported sandbox ERROR",
					"sandbox_id", sandboxID, "reason", reason)
				_ = h.Tracker.Complete(context.Background(), opID, errors.New(reason))
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

// recordBootMetrics stamps how long a create took to reach RUNNING and
// whether the agent served it from the pre-warm pool. The boot duration
// is wall-clock from createdAt (when master accepted the create) to now.
// Best-effort: a failed write never blocks the lifecycle, and the metric
// is purely for the dashboard's boot-times view.
func (h *Handlers) recordBootMetrics(ctx context.Context, accountID, sandboxID string, createdAt time.Time, poolHit bool) {
	ms := h.now().UTC().Sub(createdAt).Milliseconds()
	if ms < 0 {
		ms = 0
	}
	if err := h.Store.Sandboxes().RecordBootMetrics(ctx, accountID, sandboxID, ms, poolHit); err != nil {
		h.log().Warn("RecordBootMetrics failed", "err", err, "sandbox_id", sandboxID)
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

// bootTimeRow is one entry in the GET /v1/sandboxes/boot-times response:
// a recent sandbox create with how long it took to reach RUNNING and
// whether the pre-warm pool served it.
type bootTimeRow struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	CreatedAt       time.Time `json:"created_at"`
	TimeToRunningMS int64     `json:"time_to_running_ms"`
	PoolHit         bool      `json:"pool_hit"`
}

// bootTimesMaxRows caps the boot-times response. The dashboard renders a
// "last 20 creates" table; anything older is noise for that view.
const bootTimesMaxRows = 20

// bootTimes returns the account's most recent sandbox creates that have a
// recorded boot time, newest first. Sandboxes still creating — or created
// before boot metrics were tracked — are skipped. Powers the Metrics
// page's recent-boot-times table.
func (h *Handlers) bootTimes(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	// ListByAccount already orders by created_at DESC; pull a generous
	// window and keep the first bootTimesMaxRows that have a boot time.
	all, err := h.Store.Sandboxes().ListByAccount(r.Context(), accountID, store.ListOpts{Limit: 200})
	if err != nil {
		h.log().Error("bootTimes", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]bootTimeRow, 0, bootTimesMaxRows)
	for _, sb := range all {
		if sb.TimeToRunningMS == nil {
			continue
		}
		row := bootTimeRow{
			ID:              sb.ID,
			Name:            sb.Name,
			CreatedAt:       sb.CreatedAt,
			TimeToRunningMS: *sb.TimeToRunningMS,
		}
		if sb.PoolHit != nil {
			row.PoolHit = *sb.PoolHit
		}
		out = append(out, row)
		if len(out) >= bootTimesMaxRows {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// getSandbox returns one sandbox by ID. The Postgres row is the source
// of truth; the Redis state cache only seeds the response when the row
// shows a stale state (e.g. master_A wrote RUNNING via cache while
// master_B still has CREATING in Postgres because heartbeat-driven
// replication hadn't landed yet). This is best-effort — a cache miss
// or parse error is fine, the row is returned as-is.
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
	if v, cerr := h.getCache().Get(r.Context(), cache.SandboxStateKey(sb.ID)); cerr == nil {
		cached := models.SandboxState(v)
		if cached.Valid() && cached != sb.State {
			sb.State = cached
		}
	}
	// Repopulate cache so subsequent reads hit Redis even after a TTL
	// expiry. Best-effort.
	h.writeSandboxStateCache(r.Context(), sb.ID, sb.State)
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
