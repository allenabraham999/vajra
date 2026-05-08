// Package master — handlers_snapshot.go: snapshot-rooted endpoints
// (restore, clone, promote-to-template). Each one fans out to the
// scheduler + dispatcher just like createSandbox; they're factored
// here so the create surface can keep its own narrative.
package master

import (
	"errors"
	"net/http"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// restoreSnapshotRequest is the body of POST /v1/snapshots/{id}/restore.
type restoreSnapshotRequest struct {
	Name     string `json:"name"`
	VCPUs    int    `json:"vcpus"`
	MemoryMB int    `json:"memory_mb"`
	DiskGB   int    `json:"disk_gb"`
}

// restoreSnapshot creates a new sandbox from a snapshot. We translate
// into a createSandboxRequest with source=snapshot so the create path
// (with its quota + scheduler pipeline) is the single source of truth.
func (h *Handlers) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	snapshotID := pathID(r)
	if snapshotID == "" {
		writeErr(w, http.StatusBadRequest, "missing snapshot id")
		return
	}
	var body restoreSnapshotRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Confirm ownership early.
	if _, err := h.Store.Snapshots().GetByID(r.Context(), accountID, snapshotID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "snapshot not found")
			return
		}
		h.log().Error("restoreSnapshot: load", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Hand off to createSandbox by reformulating the request body. We
	// do not actually re-enter the HTTP path; we just call into the
	// shared internals to keep behaviour identical.
	create := &createSandboxRequest{
		Name:       body.Name,
		Source:     "snapshot",
		SnapshotID: snapshotID,
		VCPUs:      body.VCPUs,
		MemoryMB:   body.MemoryMB,
		DiskGB:     body.DiskGB,
	}
	h.runCreate(w, r, accountID, create)
}

// cloneSnapshot is currently an alias of restore: it copies the source
// sandbox's resource config and creates a fresh sandbox from the
// snapshot. Document as alias-for-now.
func (h *Handlers) cloneSnapshot(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	snapshotID := pathID(r)
	if snapshotID == "" {
		writeErr(w, http.StatusBadRequest, "missing snapshot id")
		return
	}
	var body restoreSnapshotRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	snap, err := h.Store.Snapshots().GetByID(r.Context(), accountID, snapshotID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "snapshot not found")
			return
		}
		h.log().Error("cloneSnapshot: load snapshot", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	srcSandbox, err := h.Store.Sandboxes().GetByID(r.Context(), accountID, snap.SandboxID)
	if err != nil {
		// If the source sandbox is gone, fall back to whatever the
		// caller passed (possibly zero, which createSandbox will
		// reject). This is fine; cloneSnapshot is best-effort sugar.
		srcSandbox = nil
	}
	if body.VCPUs == 0 && srcSandbox != nil {
		body.VCPUs = srcSandbox.Config.VCPUs
	}
	if body.MemoryMB == 0 && srcSandbox != nil {
		body.MemoryMB = srcSandbox.Config.MemoryMB
	}
	if body.DiskGB == 0 && srcSandbox != nil {
		body.DiskGB = srcSandbox.Config.DiskGB
	}
	if body.Name == "" && srcSandbox != nil {
		body.Name = srcSandbox.Name + "-clone"
	}

	create := &createSandboxRequest{
		Name:       body.Name,
		Source:     "snapshot",
		SnapshotID: snapshotID,
		VCPUs:      body.VCPUs,
		MemoryMB:   body.MemoryMB,
		DiskGB:     body.DiskGB,
	}
	h.runCreate(w, r, accountID, create)
}

// promoteSnapshot creates a Template row that points at the snapshot's
// storage path. This is metadata-only: production promotion would ask
// the agent to repackage the snapshot into the content-addressable
// template cache, but the agent doesn't expose that yet.
func (h *Handlers) promoteSnapshot(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	snapshotID := pathID(r)
	if snapshotID == "" {
		writeErr(w, http.StatusBadRequest, "missing snapshot id")
		return
	}
	snap, err := h.Store.Snapshots().GetByID(r.Context(), accountID, snapshotID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "snapshot not found")
			return
		}
		h.log().Error("promoteSnapshot: load", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	templateID, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	tmpl := &models.Template{
		ID:           templateID,
		AccountID:    accountID,
		Name:         "snapshot-" + snap.ID,
		Version:      "1",
		Hash:         "sha256:" + snap.ID,
		RootfsPath:   snap.StoragePath,
		KernelPath:   "",
		SnapshotPath: snap.StoragePath,
		CreatedAt:    h.now().UTC(),
	}
	if err := h.Store.Templates().Create(r.Context(), tmpl); err != nil {
		h.log().Error("promoteSnapshot: persist", "err", err)
		writeErr(w, translateStoreErr(err), "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, tmpl)
}

// runCreate is the shared back-end of createSandbox and the
// snapshot-restore endpoints. It enforces precondition checks the HTTP
// path already did, then schedules + dispatches.
//
// This duplicates the body of createSandbox(); we factor it out because
// the snapshot endpoints arrive with a synthesised request object
// rather than a JSON body.
func (h *Handlers) runCreate(w http.ResponseWriter, r *http.Request, accountID string, body *createSandboxRequest) {
	if err := body.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	h.executeCreate(w, r, accountID, body)
}
