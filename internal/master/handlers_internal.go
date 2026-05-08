// Package master — handlers_internal.go: endpoints meant for node
// agents, gated by the InternalAuthMiddleware (pre-shared secret).
// Nodes are peers of the control plane, not customers, so they
// authenticate with a static secret and never carry an account ID.
package master

import (
	"errors"
	"net/http"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// nodeRegisterRequest matches the agent's RegisterRequest. Capacity is
// flattened by the agent, so we mirror the same shape rather than
// embedding NodeCapacity directly.
type nodeRegisterRequest struct {
	NodeID    string `json:"node_id"`
	Hostname  string `json:"hostname"`
	IP        string `json:"ip"`
	ClusterID string `json:"cluster_id"`
	Capacity  struct {
		TotalCPU      int `json:"total_cpu"`
		TotalMemoryMB int `json:"total_memory_mb"`
		TotalDiskGB   int `json:"total_disk_gb"`
	} `json:"capacity"`
}

// nodeRegisterResponse returns the canonical node ID — useful when the
// agent did not specify one.
type nodeRegisterResponse struct {
	ID string `json:"id"`
}

// registerNode handles POST /internal/nodes/register. If the node ID
// is supplied and exists we update it; otherwise we insert a new row.
func (h *Handlers) registerNode(w http.ResponseWriter, r *http.Request) {
	var body nodeRegisterRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := models.ValidateNodeIP(body.IP); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ClusterID == "" {
		writeErr(w, http.StatusBadRequest, "cluster_id is required")
		return
	}
	cluster, err := h.Store.Clusters().GetByID(r.Context(), body.ClusterID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusBadRequest, "cluster not found")
			return
		}
		h.log().Error("registerNode: cluster lookup", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if cluster.State != models.ClusterStateActive {
		writeErr(w, http.StatusBadRequest, "cluster is not active")
		return
	}

	capacity := models.NodeCapacity{
		TotalCPU:      body.Capacity.TotalCPU,
		TotalMemoryMB: body.Capacity.TotalMemoryMB,
		TotalDiskGB:   body.Capacity.TotalDiskGB,
	}

	if body.NodeID != "" {
		if existing, err := h.Store.Nodes().GetByID(r.Context(), body.NodeID); err == nil {
			// Re-registration path: persist hostname/IP/capacity along
			// with state. Usage is preserved (UpdateUsage is the
			// agent's separate write path).
			if err := h.Store.Nodes().UpdateConfig(r.Context(), existing.ID, body.Hostname, body.IP, capacity, models.NodeStateActive); err != nil {
				h.log().Error("registerNode: update config", "err", err)
				writeErr(w, http.StatusInternalServerError, "internal error")
				return
			}
			writeJSON(w, http.StatusOK, nodeRegisterResponse{ID: existing.ID})
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			h.log().Error("registerNode: lookup", "err", err)
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	id := body.NodeID
	if id == "" {
		var err error
		if id, err = randomHex(16); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	node := &models.Node{
		ID:        id,
		ClusterID: body.ClusterID,
		Hostname:  body.Hostname,
		IP:        body.IP,
		State:     models.NodeStateActive,
		Capacity:  capacity,
		// UsedResources zero-valued; agent will heartbeat updates.
		LastHeartbeat: h.now().UTC(),
	}
	if err := h.Store.Nodes().Create(r.Context(), node); err != nil {
		h.log().Error("registerNode: create", "err", err)
		writeErr(w, translateStoreErr(err), "create failed")
		return
	}
	writeJSON(w, http.StatusCreated, nodeRegisterResponse{ID: id})
}

// nodeHeartbeatRequest matches the agent's HeartbeatRequest. The
// "usage" field name in our wire spec is "used_resources" in the
// internal models — accept the agent's name verbatim.
type nodeHeartbeatRequest struct {
	NodeID    string    `json:"node_id"`
	Timestamp time.Time `json:"timestamp"`
	Usage     struct {
		UsedCPU      int `json:"used_cpu"`
		UsedMemoryMB int `json:"used_memory_mb"`
		UsedDiskGB   int `json:"used_disk_gb"`
	} `json:"usage"`
	SandboxCount int `json:"sandbox_count"`
}

// nodeHeartbeat handles POST /internal/nodes/{id}/heartbeat.
func (h *Handlers) nodeHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing node id")
		return
	}
	var body nodeHeartbeatRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.NodeID != "" && body.NodeID != id {
		writeErr(w, http.StatusBadRequest, "path id does not match body node_id")
		return
	}
	usage := models.NodeUsage{
		UsedCPU:      body.Usage.UsedCPU,
		UsedMemoryMB: body.Usage.UsedMemoryMB,
		UsedDiskGB:   body.Usage.UsedDiskGB,
	}
	if err := h.Store.Nodes().UpdateUsage(r.Context(), id, usage); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "node not found")
			return
		}
		h.log().Error("nodeHeartbeat: usage", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	ts := body.Timestamp
	if ts.IsZero() {
		ts = h.now().UTC()
	}
	if err := h.Store.Nodes().UpdateHeartbeat(r.Context(), id, ts); err != nil {
		h.log().Error("nodeHeartbeat: ts", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// nodeEventRequest is the body of POST /internal/nodes/{id}/event.
type nodeEventRequest struct {
	Type      string         `json:"type"`
	SandboxID string         `json:"sandbox_id"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// nodeEvent processes a state-change or unhealthy event from an agent.
// We accept both /internal/nodes/{id}/event and the agent's existing
// /internal/sandboxes/{id}/unhealthy alias.
func (h *Handlers) nodeEvent(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing node id")
		return
	}
	var body nodeEventRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.SandboxID == "" {
		writeErr(w, http.StatusBadRequest, "sandbox_id is required")
		return
	}
	switch body.Type {
	case "unhealthy":
		h.markSandboxState(w, r, id, body.SandboxID, models.SandboxStateError)
	case "state_change":
		raw, _ := body.Payload["state"].(string)
		next := models.SandboxState(raw)
		if !next.Valid() {
			writeErr(w, http.StatusBadRequest, "invalid target state")
			return
		}
		h.markSandboxState(w, r, id, body.SandboxID, next)
	default:
		writeErr(w, http.StatusBadRequest, "unknown event type")
	}
}

// sandboxUnhealthyAlias is a convenience shim for the agent's existing
// POST /internal/sandboxes/{id}/unhealthy. The agent ships with that
// path baked in; this alias translates into nodeEvent without changing
// the agent client.
func (h *Handlers) sandboxUnhealthyAlias(w http.ResponseWriter, r *http.Request) {
	sandboxID := pathID(r)
	if sandboxID == "" {
		writeErr(w, http.StatusBadRequest, "missing sandbox id")
		return
	}
	// Body shape on the agent: {node_id, sandbox_id, reason}. We only
	// need node_id; the rest is logged.
	var body struct {
		NodeID    string `json:"node_id"`
		SandboxID string `json:"sandbox_id"`
		Reason    string `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.NodeID == "" {
		writeErr(w, http.StatusBadRequest, "node_id is required")
		return
	}
	h.log().Warn("agent reported unhealthy sandbox",
		"node_id", body.NodeID, "sandbox_id", sandboxID, "reason", body.Reason)
	h.markSandboxState(w, r, body.NodeID, sandboxID, models.SandboxStateError)
}

// markSandboxState resolves a sandbox by node + ID (no account context
// in internal calls) and updates its state. Used by both nodeEvent and
// sandboxUnhealthyAlias.
func (h *Handlers) markSandboxState(w http.ResponseWriter, r *http.Request, nodeID, sandboxID string, target models.SandboxState) {
	// SandboxStore.GetByID is account-scoped; in an internal context
	// we don't have an account. List by node and find the row by ID.
	rows, err := h.Store.Sandboxes().ListByNode(r.Context(), nodeID, store.ListOpts{Limit: 1000})
	if err != nil {
		h.log().Error("markSandboxState: list", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	var sb *models.Sandbox
	for _, row := range rows {
		if row.ID == sandboxID {
			sb = row
			break
		}
	}
	if sb == nil {
		writeErr(w, http.StatusNotFound, "sandbox not found on node")
		return
	}
	if err := h.Store.Sandboxes().UpdateState(r.Context(), sb.AccountID, sb.ID, target); err != nil {
		h.log().Error("markSandboxState: update", "err", err)
		writeErr(w, translateStoreErr(err), "update failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
