// Package master — handlers_autoscale.go: admin-only autoscaler
// surface. Two endpoints:
//
//   - GET  /v1/admin/autoscale          → status snapshot
//   - POST /v1/admin/autoscale/trigger  → force a scale-up
//
// Both gated by requireAdmin. When the autoscaler is unconfigured we
// return a stable empty status rather than 503 so the dashboard can
// render a "disabled" badge without erroring.
package master

import (
	"net/http"
)

// getAutoscaleStatus reports the autoscaler's current state. Returns a
// disabled-style payload when the autoscaler isn't wired so the
// dashboard call never fails.
func (h *Handlers) getAutoscaleStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if h.Autoscaler == nil {
		writeJSON(w, http.StatusOK, AutoscaleStatus{Enabled: false})
		return
	}
	st, err := h.Autoscaler.Status(r.Context())
	if err != nil {
		h.log().Error("autoscale status", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// triggerAutoscale forces an immediate scale-up. Useful in
// demos/tests; production should let HandleNoCapacity do the work.
func (h *Handlers) triggerAutoscale(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if h.Autoscaler == nil {
		writeErr(w, http.StatusServiceUnavailable, "autoscaler not configured")
		return
	}
	if err := h.Autoscaler.Trigger(r.Context()); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "triggered"})
}
