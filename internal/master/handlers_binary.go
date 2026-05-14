// Package master — handlers_binary.go serves the agent and cloud-hypervisor
// binaries from a local directory so autoscaler-launched nodes can fetch
// them over an authenticated channel without needing public S3 or an IAM
// instance profile. Mounted under /internal/binaries/{name} behind the
// pre-shared-secret InternalAuthMiddleware.
package master

import (
	"net/http"
	"path/filepath"
	"strings"
)

// allowedBinaries is the closed allow-list of names the endpoint will
// serve. Anything else returns 404 even if the file exists, so the
// endpoint can't be turned into an arbitrary file reader.
var allowedBinaries = map[string]bool{
	"vajra-agent":      true,
	"cloud-hypervisor": true,
}

func (h *Handlers) serveBinary(w http.ResponseWriter, r *http.Request) {
	if h.BinaryDir == "" {
		http.Error(w, "binary serving not configured", http.StatusNotFound)
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if !allowedBinaries[name] {
		http.Error(w, "unknown binary", http.StatusNotFound)
		return
	}
	path := filepath.Join(h.BinaryDir, name)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}
