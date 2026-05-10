// Package agent — server_archive.go contains the HTTP handlers for the
// archive, rehydrate, migrate, and receive endpoints. Kept separate from
// server.go so that file stays focused on the live-sandbox surface.
package agent

import (
	"errors"
	"net/http"
)

// ArchiveResponse is what POST /sandbox/{id}/archive returns to the master.
type ArchiveResponse struct {
	*ArchiveResult
}

// RehydrateRequestBody captures the optional explicit archive path master
// passes through (e.g. an "s3://bucket/key" URL recorded at archive time).
// When empty the agent's ArchiveManager resolves the location from its
// configured store.
type RehydrateRequestBody struct {
	ArchivePath string `json:"archive_path,omitempty"`
}

// MigrateRequestBody is the body master POSTs to the source agent to
// initiate a migration. AuthToken is forwarded as the Bearer credential
// on the source→target POST so the target's InternalAuth middleware
// accepts the receive call without a separate config plumbing step.
type MigrateRequestBody struct {
	TargetAddr string `json:"target_addr"`
	AuthToken  string `json:"auth_token,omitempty"`
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	if s.archives == nil {
		writeErr(w, http.StatusServiceUnavailable, "archive manager not configured")
		return
	}
	id := r.PathValue("id")
	res, err := s.archives.ArchiveSandbox(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRehydrate(w http.ResponseWriter, r *http.Request) {
	if s.archives == nil {
		writeErr(w, http.StatusServiceUnavailable, "archive manager not configured")
		return
	}
	id := r.PathValue("id")
	var body RehydrateRequestBody
	if r.ContentLength > 0 {
		if err := decodeBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	sb, err := s.archives.RehydrateSandbox(r.Context(), id, body.ArchivePath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) handleMigrate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body MigrateRequestBody
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.TargetAddr == "" {
		writeErr(w, http.StatusBadRequest, "target_addr is required")
		return
	}
	res, err := s.sandboxes.MigrateSandbox(r.Context(), id, body.TargetAddr, body.AuthToken)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleReceive is the migration target endpoint. It expects the source
// to set X-Vajra-Sandbox-ID and stream a tar body. Response is the freshly
// registered Sandbox in STOPPED state.
func (s *Server) handleReceive(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Vajra-Sandbox-ID")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "X-Vajra-Sandbox-ID header required")
		return
	}
	defer r.Body.Close()
	sb, err := s.sandboxes.ReceiveSandbox(r.Context(), id, r.Body)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

// statusFromError is a small helper for archive/rehydrate handlers that
// want to translate sentinel errors into HTTP statuses without importing
// the full master translateStoreErr machinery.
func statusFromError(err error) int {
	if errors.Is(err, errSandboxMissing) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

var _ = statusFromError // reserved for future use; keeps the helper near the handlers.
