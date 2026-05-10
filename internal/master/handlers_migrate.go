// Package master — handlers_migrate.go owns offline sandbox migration.
// Master coordinates: validate target, dispatch the source agent's
// /sandbox/{id}/migrate (which streams the on-disk dir to the target's
// /sandbox/receive), then update the placement row so subsequent calls
// route to the new node.
package master

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// migrateRequest is the body of POST /v1/sandboxes/{id}/migrate.
type migrateRequest struct {
	TargetNodeID string `json:"target_node_id"`
}

// migrateResponse is the JSON body returned to admins.
type migrateResponse struct {
	OperationID string `json:"operation_id"`
	ID          string `json:"id"`
	SourceNode  string `json:"source_node_id"`
	TargetNode  string `json:"target_node_id"`
	BytesSent   int64  `json:"bytes_sent"`
}

// migrateSandbox runs the offline migration. Admin-only because moving
// other accounts' sandboxes is an operator action; non-admin callers see
// the same 403 the rest of the admin surface returns.
func (h *Handlers) migrateSandbox(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing sandbox id")
		return
	}
	var body migrateRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.TargetNodeID == "" {
		writeErr(w, http.StatusBadRequest, "target_node_id is required")
		return
	}
	sb, err := h.Store.Sandboxes().GetByIDUnscoped(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeErr(w, translateStoreErr(err), "lookup failed")
		return
	}
	if sb.NodeID == nil || *sb.NodeID == "" {
		writeErr(w, http.StatusConflict, "sandbox has no source placement")
		return
	}
	if *sb.NodeID == body.TargetNodeID {
		writeErr(w, http.StatusBadRequest, "target node is the current node")
		return
	}
	if sb.State != models.SandboxStateStopped && sb.State != models.SandboxStateRunning && sb.State != models.SandboxStatePaused {
		writeErr(w, http.StatusConflict, "sandbox state "+string(sb.State)+" not eligible for migrate")
		return
	}

	source, err := h.Store.Nodes().GetByID(r.Context(), *sb.NodeID)
	if err != nil {
		writeErr(w, translateStoreErr(err), "source node lookup failed")
		return
	}
	target, err := h.Store.Nodes().GetByID(r.Context(), body.TargetNodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "target node not found")
			return
		}
		writeErr(w, translateStoreErr(err), "target node lookup failed")
		return
	}
	if target.State != models.NodeStateActive {
		writeErr(w, http.StatusConflict, "target node not ACTIVE")
		return
	}

	opID, _ := h.Tracker.Start(r.Context(), sb.AccountID, sb.ID, models.OperationTypeMigrate)

	dispatchCtx, cancel := context.WithTimeout(r.Context(), archiveDispatchTimeout)
	defer cancel()
	targetURL := fmt.Sprintf("http://%s:%d", target.IP, DefaultAgentPort)
	res, err := h.Pool.ClientFor(source).MigrateSandbox(dispatchCtx, sb.ID, targetURL, h.AgentSharedSecret)
	if err != nil {
		_ = h.Store.Sandboxes().UpdateState(r.Context(), sb.AccountID, sb.ID, models.SandboxStateError)
		_ = h.Tracker.Complete(r.Context(), opID, err)
		h.log().Error("migrateSandbox: dispatch", "err", err, "sandbox_id", sb.ID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	clusterID := target.ClusterID
	if err := h.Store.Sandboxes().UpdatePlacement(r.Context(), sb.ID, clusterID, target.ID); err != nil {
		h.log().Error("migrateSandbox: placement", "err", err)
	}
	// Migration always returns the sandbox to STOPPED — the source agent
	// stops the sandbox before shipping, and the target re-registers in
	// STOPPED. A subsequent /start re-restores from the saved snapshot.
	if err := h.Store.Sandboxes().UpdateState(r.Context(), sb.AccountID, sb.ID, models.SandboxStateStopped); err != nil {
		h.log().Error("migrateSandbox: final-state", "err", err)
	}
	_ = h.Tracker.Complete(r.Context(), opID, nil)

	writeJSON(w, http.StatusOK, migrateResponse{
		OperationID: opID,
		ID:          sb.ID,
		SourceNode:  source.ID,
		TargetNode:  target.ID,
		BytesSent:   res.BytesSent,
	})
}
