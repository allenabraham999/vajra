// Package master — handlers_admin.go: admin-only endpoints (clusters,
// nodes, drain, usage). Gated by the AdminAccountID placeholder until
// accounts grow a role column. See Handlers.requireAdmin.
package master

import (
	"net/http"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

// listClusters returns every cluster known to master.
func (h *Handlers) listClusters(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	out, err := h.Store.Clusters().List(r.Context(), parseListOpts(r))
	if err != nil {
		h.log().Error("listClusters", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// listNodes returns every node known to master.
func (h *Handlers) listNodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	out, err := h.Store.Nodes().List(r.Context(), parseListOpts(r))
	if err != nil {
		h.log().Error("listNodes", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// drainNode flips a node into DRAINING. New scheduling skips it; in-
// flight sandboxes keep running until they're stopped or migrated.
func (h *Handlers) drainNode(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing node id")
		return
	}
	if err := h.Store.Nodes().UpdateState(r.Context(), id, models.NodeStateDraining); err != nil {
		h.log().Error("drainNode", "err", err)
		writeErr(w, translateStoreErr(err), "drain failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "draining", "node_id": id})
}

// usageReport is the response shape of GET /v1/usage. The fields are
// stubs in this milestone — there is no rolled-up sandbox_usage reader
// in the store yet, so we report active sandbox configs as a proxy.
type usageReport struct {
	From                 time.Time `json:"from"`
	To                   time.Time `json:"to"`
	SandboxCount         int       `json:"sandbox_count"`
	TotalVCPUSeconds     int64     `json:"total_vcpu_seconds"`
	TotalMemoryMBSeconds int64     `json:"total_memory_mb_seconds"`
	TotalDiskGBSeconds   int64     `json:"total_disk_gb_seconds"`
	Note                 string    `json:"note,omitempty"`
}

// getUsage stub-reports usage for the calling account. Rolled-up
// totals from sandbox_usage are pending; for now we sum config across
// non-DESTROYED sandboxes scaled by the request window.
func (h *Handlers) getUsage(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	from, _ := time.Parse(time.RFC3339, q.Get("from"))
	to, _ := time.Parse(time.RFC3339, q.Get("to"))
	if to.IsZero() {
		to = h.now().UTC()
	}
	if from.IsZero() {
		from = to.Add(-24 * time.Hour)
	}
	windowSec := int64(to.Sub(from).Seconds())
	if windowSec < 0 {
		writeErr(w, http.StatusBadRequest, "to must be after from")
		return
	}

	sandboxes, err := h.Store.Sandboxes().ListByAccount(r.Context(), accountID, parseListOpts(r))
	if err != nil {
		h.log().Error("getUsage: list sandboxes", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	report := usageReport{From: from, To: to, Note: "rolled-up usage pending; this is a stub"}
	for _, sb := range sandboxes {
		if sb.State == models.SandboxStateDestroyed || sb.State == models.SandboxStateError {
			continue
		}
		report.SandboxCount++
		report.TotalVCPUSeconds += int64(sb.Config.VCPUs) * windowSec
		report.TotalMemoryMBSeconds += int64(sb.Config.MemoryMB) * windowSec
		report.TotalDiskGBSeconds += int64(sb.Config.DiskGB) * windowSec
	}
	writeJSON(w, http.StatusOK, report)
}

// healthResponse is the body of GET /health.
type healthResponse struct {
	Status string `json:"status"`
	DB     string `json:"db"`
}

// getHealth pings the database and reports overall liveness.
func (h *Handlers) getHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := requestContextWithTimeout(r, 2*time.Second)
	defer cancel()
	resp := healthResponse{Status: "ok", DB: "ok"}
	if err := h.Store.Ping(ctx); err != nil {
		resp.Status = "degraded"
		resp.DB = "error"
		writeJSON(w, http.StatusServiceUnavailable, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// getVersion serves the build provenance.
func (h *Handlers) getVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.Version)
}
