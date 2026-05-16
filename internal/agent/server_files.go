// Package agent — server_files.go is the HTTP surface that exposes the
// guest file API to vajra-master. Master-side handlers in
// internal/master/handlers_files.go proxy through these endpoints; the
// SDK does not call them directly.
package agent

import (
	"errors"
	"io"
	"net/http"
	"strconv"
)

// MaxUploadHeader bounds the size of the X-Vajra-Path header. A guest
// path longer than this is almost certainly malicious or misconfigured;
// rejecting cheaply prevents a 500 deeper down the stack.
const MaxUploadHeader = 4096

// handleFileUpload streams the request body into the guest VM. Path /
// mode / size are carried in headers so the request body is the bare
// file content. Master sets `X-Vajra-Path`, `X-Vajra-Mode`, and
// `Content-Length` on the proxied request.
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.Header.Get("X-Vajra-Path")
	if path == "" || len(path) > MaxUploadHeader {
		writeErr(w, http.StatusBadRequest, "X-Vajra-Path is required")
		return
	}
	mode, _ := strconv.ParseUint(r.Header.Get("X-Vajra-Mode"), 10, 32)
	if r.ContentLength < 0 {
		writeErr(w, http.StatusBadRequest, "Content-Length is required")
		return
	}
	err := s.sandboxes.FileUpload(r.Context(), id, FileUploadRequest{
		Path: path,
		Mode: uint32(mode),
		Size: r.ContentLength,
		Body: r.Body,
	})
	if err != nil {
		s.logger.Warn("file upload failed", "sandbox", id, "err", err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleFileDownload streams a file from the guest VM into the response
// body. The `path` query string parameter selects the source path;
// `X-Vajra-Mode` and `Content-Length` describe the result.
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "path query is required")
		return
	}
	res, err := s.sandboxes.FileDownload(r.Context(), id, path, 0)
	if err != nil {
		s.logger.Warn("file download failed", "sandbox", id, "err", err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer res.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(res.Size, 10))
	w.Header().Set("X-Vajra-Mode", strconv.FormatUint(uint64(res.Mode), 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, res.Body); err != nil {
		// Headers are already flushed — nothing left to do but log.
		s.logger.Warn("file download copy", "sandbox", id, "err", err)
	}
}

// handleFileList returns the entries inside a directory in the VM as
// JSON. The `dir` query parameter is the absolute path inside the
// sandbox; missing or empty falls back to "/".
func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		dir = "/"
	}
	entries, err := s.sandboxes.FileList(r.Context(), id, dir, 0)
	if err != nil {
		s.logger.Warn("file list failed", "sandbox", id, "err", err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// handleFileDelete removes a single file inside the guest VM. The
// `path` query parameter is the absolute path; the guest rejects
// directories so a stray DELETE can't wipe a subtree.
func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "path query is required")
		return
	}
	if err := s.sandboxes.FileDelete(r.Context(), id, path, 0); err != nil {
		s.logger.Warn("file delete failed", "sandbox", id, "err", err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSandboxList returns every sandbox the agent currently tracks.
// Master's reconciler relies on this to detect drift between the DB and
// the actual on-host state. The shape matches dispatcher.AgentSandbox.
func (s *Server) handleSandboxList(w http.ResponseWriter, _ *http.Request) {
	all := s.sandboxes.List()
	type view struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	out := make([]view, 0, len(all))
	for _, sb := range all {
		out = append(out, view{ID: sb.ID, State: string(sb.State)})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSandboxSnapshot is the agent-side counterpart to master's
// SnapshotSandbox dispatch. It tells SandboxManager to write a snapshot
// to the directory the master suggested and reports back the on-disk
// path and size. Snapshot-from-master flows depend on this; until now
// it was a 404.
func (s *Server) handleSandboxSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name     string `json:"name"`
		DestPath string `json:"dest_path"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.DestPath == "" {
		writeErr(w, http.StatusBadRequest, "dest_path is required")
		return
	}
	res, err := s.sandboxes.SnapshotIntoDir(r.Context(), id, body.DestPath)
	if err != nil {
		if errors.Is(err, errSandboxMissing) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
			return
		}
		s.logger.Warn("snapshot failed", "sandbox", id, "err", err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot_path": res.Path,
		"size_bytes":    res.SizeBytes,
	})
}

