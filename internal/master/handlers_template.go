// Package master — handlers_template.go: template metadata CRUD.
// Templates are content-addressable VM images; uploading the bytes is
// a job for the agent's image cache, but the metadata row lives here
// so the scheduler can resolve template_id → hash + paths.
package master

import (
	"net/http"

	"github.com/allenabraham999/vajra/internal/models"
)

// listTemplates returns templates owned by the calling account.
//
// TODO: surface "public" templates owned by a system account (e.g.
// VAJRA_PUBLIC_ACCOUNT_ID) once that concept is wired into the schema.
func (h *Handlers) listTemplates(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	out, err := h.Store.Templates().ListByAccount(r.Context(), accountID, parseListOpts(r))
	if err != nil {
		h.log().Error("listTemplates", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createTemplateRequest is the body of POST /v1/templates.
type createTemplateRequest struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Hash         string `json:"hash"`
	RootfsPath   string `json:"rootfs_path"`
	KernelPath   string `json:"kernel_path"`
	SnapshotPath string `json:"snapshot_path"`
}

// createTemplate persists a template metadata row. The bytes referenced
// by the paths are the agent's responsibility — we don't validate they
// exist on disk here.
func (h *Handlers) createTemplate(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	var body createTemplateRequest
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name == "" || body.Version == "" || body.Hash == "" {
		writeErr(w, http.StatusBadRequest, "name, version, hash are required")
		return
	}
	id, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	tmpl := &models.Template{
		ID: id, AccountID: accountID,
		Name: body.Name, Version: body.Version, Hash: body.Hash,
		RootfsPath: body.RootfsPath, KernelPath: body.KernelPath,
		SnapshotPath: body.SnapshotPath, CreatedAt: h.now().UTC(),
	}
	if err := h.Store.Templates().Create(r.Context(), tmpl); err != nil {
		h.log().Error("createTemplate", "err", err)
		writeErr(w, translateStoreErr(err), "create failed")
		return
	}
	writeJSON(w, http.StatusCreated, tmpl)
}
