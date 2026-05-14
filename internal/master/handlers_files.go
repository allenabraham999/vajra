// Package master — handlers_files.go is the file-ops surface:
//
//	POST /v1/sandboxes/{id}/files/upload   (octet-stream body, X-Vajra-Path header)
//	GET  /v1/sandboxes/{id}/files/download?path=…
//	GET  /v1/sandboxes/{id}/files/list?dir=…
//
// Each handler resolves the sandbox + node, then proxies to the agent
// via the dispatcher's file methods. The wire shape is documented next
// to dispatcher_files.go and the agent's server_files.go.
package master

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/allenabraham999/vajra/internal/store"
)

// MaxUploadSize caps a single upload at 1 GiB. Mirrors the cap inside
// scripts/guest-agent/files.go so the SDK fails fast at master rather
// than streaming a gigabyte before the guest rejects it.
const MaxUploadSize int64 = 1 << 30

// uploadFile streams the request body straight through to the agent.
// We rely on Content-Length to size the upload — clients that don't
// set it are rejected with 411.
func (h *Handlers) uploadFile(w http.ResponseWriter, r *http.Request) {
	_, sb, node, ok := h.resolveSandboxAndNode(w, r)
	if !ok {
		return
	}
	path := r.Header.Get("X-Vajra-Path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "X-Vajra-Path is required")
		return
	}
	if r.ContentLength < 0 {
		writeErr(w, http.StatusLengthRequired, "Content-Length is required")
		return
	}
	if r.ContentLength > MaxUploadSize {
		writeErr(w, http.StatusRequestEntityTooLarge, "upload too large")
		return
	}
	mode64, _ := strconv.ParseUint(r.Header.Get("X-Vajra-Mode"), 10, 32)

	dispatchCtx, cancel := requestContextWithTimeout(r, dispatchTimeout)
	defer cancel()
	if err := h.Pool.ClientFor(node).UploadFile(dispatchCtx, sb.ID, path, uint32(mode64), r.ContentLength, r.Body); err != nil {
		h.log().Error("uploadFile: dispatch", "err", err, "sandbox_id", sb.ID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	h.touchActivity(r.Context(), sb.ID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// downloadFile opens a streaming GET against the agent and copies the
// response body straight to the client. Headers (size, mode) are echoed
// through so the SDK can render a save dialog with the right metadata.
func (h *Handlers) downloadFile(w http.ResponseWriter, r *http.Request) {
	_, sb, node, ok := h.resolveSandboxAndNode(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}
	dispatchCtx, cancel := requestContextWithTimeout(r, dispatchTimeout)
	defer cancel()
	res, err := h.Pool.ClientFor(node).DownloadFile(dispatchCtx, sb.ID, path)
	if err != nil {
		h.log().Error("downloadFile: dispatch", "err", err, "sandbox_id", sb.ID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer res.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	if res.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(res.Size, 10))
	}
	w.Header().Set("X-Vajra-Mode", strconv.FormatUint(uint64(res.Mode), 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, res.Body); err != nil {
		h.log().Warn("downloadFile: copy", "err", err)
	}
	h.touchActivity(r.Context(), sb.ID)
}

// listFiles returns the JSON entries the agent emits.
func (h *Handlers) listFiles(w http.ResponseWriter, r *http.Request) {
	_, sb, node, ok := h.resolveSandboxAndNode(w, r)
	if !ok {
		return
	}
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		dir = "/"
	}
	dispatchCtx, cancel := requestContextWithTimeout(r, dispatchTimeout)
	defer cancel()
	entries, err := h.Pool.ClientFor(node).ListFiles(dispatchCtx, sb.ID, dir)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "directory not found")
			return
		}
		h.log().Error("listFiles: dispatch", "err", err, "sandbox_id", sb.ID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
