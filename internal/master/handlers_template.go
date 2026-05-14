// Package master — handlers_template.go: template metadata CRUD plus the
// asynchronous "Dockerfile → Template" build surface. Templates are
// content-addressable VM images; uploading the bytes is a job for the
// agent's image cache, but the metadata row lives here so the scheduler
// can resolve template_id → hash + paths.
package master

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
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

// maxDockerfileBytes caps the inbound Dockerfile body. 256 KiB is large
// enough for any realistic recipe yet bounds the memory the master will
// hold on a malicious upload.
const maxDockerfileBytes = 256 * 1024

// buildTemplate accepts a multipart-or-JSON request carrying the
// Dockerfile, name, and version, enqueues a Build row, fires the async
// pipeline, and returns 202 with the build id so the caller can poll
// GET /v1/templates/builds/{id}.
//
// Two encodings are supported:
//
//	multipart/form-data — fields: dockerfile (file or text), name, version
//	application/json     — body: {"dockerfile": "...", "name": "...", "version": "..."}
//
// JSON makes the SDK happy; multipart matches what `curl -F` users expect.
func (h *Handlers) buildTemplate(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	if h.Builder == nil {
		writeErr(w, http.StatusServiceUnavailable, "builder not configured")
		return
	}
	name, version, dockerfile, err := parseBuildBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	b, err := h.Builder.Enqueue(r.Context(), accountID, name, version, dockerfile)
	if err != nil {
		h.log().Error("buildTemplate: enqueue", "err", err)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	h.Builder.Start(b)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"build_id":         b.ID,
		"status":           b.Status,
		"template_name":    b.TemplateName,
		"template_version": b.TemplateVer,
		"created_at":       b.CreatedAt,
	})
}

// getBuild returns one build by ID, account-scoped. Used for status
// polling after POST /v1/templates/build.
func (h *Handlers) getBuild(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing build id")
		return
	}
	b, err := h.Store.Builds().GetByID(r.Context(), accountID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "build not found")
			return
		}
		h.log().Error("getBuild", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Strip the raw Dockerfile from the polling response — it's redundant
	// once the row is persisted and inflates every poll.
	b.Dockerfile = ""
	writeJSON(w, http.StatusOK, b)
}

// listBuilds returns the calling account's builds, newest first.
func (h *Handlers) listBuilds(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	out, err := h.Store.Builds().ListByAccount(r.Context(), accountID, parseListOpts(r))
	if err != nil {
		h.log().Error("listBuilds", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, b := range out {
		b.Dockerfile = ""
	}
	writeJSON(w, http.StatusOK, out)
}

// parseBuildBody handles both encodings (JSON and multipart). It returns
// (name, version, dockerfile, err).
func parseBuildBody(r *http.Request) (string, string, string, error) {
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(maxDockerfileBytes + 4096); err != nil {
			return "", "", "", err
		}
		name := r.FormValue("name")
		version := r.FormValue("version")
		dockerfile := r.FormValue("dockerfile")
		if dockerfile == "" {
			file, _, err := r.FormFile("dockerfile")
			if err == nil {
				defer file.Close()
				lr := io.LimitReader(file, maxDockerfileBytes+1)
				buf, rerr := io.ReadAll(lr)
				if rerr != nil {
					return "", "", "", rerr
				}
				if len(buf) > maxDockerfileBytes {
					return "", "", "", errors.New("dockerfile exceeds 256 KiB")
				}
				dockerfile = string(buf)
			}
		}
		return name, version, dockerfile, nil
	default:
		// JSON path. Cap the body so a malicious upload can't OOM master.
		r.Body = http.MaxBytesReader(nil, r.Body, maxDockerfileBytes+4096)
		var body struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			Dockerfile string `json:"dockerfile"`
		}
		if err := decodeBody(r, &body); err != nil {
			return "", "", "", err
		}
		if len(body.Dockerfile) > maxDockerfileBytes {
			return "", "", "", errors.New("dockerfile exceeds 256 KiB")
		}
		return body.Name, body.Version, body.Dockerfile, nil
	}
}
