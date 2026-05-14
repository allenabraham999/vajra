// Package master — handlers_docs.go: OpenAPI spec + Swagger UI.
//
// The spec lives on disk at docs/openapi.yaml (default) and is read
// lazily on first request — keeping the bytes off the heap until
// someone actually asks for them. Subsequent requests reuse the cached
// bytes; a process restart re-reads the latest copy.
package master

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// swaggerUIHTML is a minimal page that loads Swagger UI from a CDN and
// points it at /v1/docs/openapi.yaml. Inline rather than templated so
// the response can be served as a static byte slice with one syscall.
const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8" />
<title>Vajra API — Swagger UI</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css" />
<style>body { margin: 0; }</style>
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
window.onload = function () {
  window.ui = SwaggerUIBundle({
    url: "/v1/docs/openapi.yaml",
    dom_id: "#swagger-ui",
    deepLinking: true,
    presets: [SwaggerUIBundle.presets.apis],
    layout: "BaseLayout",
    docExpansion: "list",
    persistAuthorization: true,
  });
};
</script>
</body>
</html>
`

// docsState caches the on-disk YAML so the second request doesn't pay
// the disk-read cost. The path is read from the VAJRA_OPENAPI_PATH env
// var (set in main) or falls back to "docs/openapi.yaml" relative to
// CWD — matching the production layout. A sync.Mutex + loaded flag
// (rather than sync.Once) keeps the cache resettable from tests.
var docsState struct {
	mu     sync.Mutex
	loaded bool
	bytes  []byte
	err    error
	path   string
}

// OpenAPIPath is the resolved on-disk path of the spec file. main() can
// set this before the first request lands so the env override is
// observed. Defaults to docs/openapi.yaml.
var OpenAPIPath = "docs/openapi.yaml"

// resetDocsCache forces the next request to re-read the file. Test-only;
// kept in this file so the docsState fields remain unexported.
func resetDocsCache() {
	docsState.mu.Lock()
	defer docsState.mu.Unlock()
	docsState.loaded = false
	docsState.bytes = nil
	docsState.err = nil
}

// docsSwaggerUI serves the Swagger UI HTML page at GET /v1/docs.
func (h *Handlers) docsSwaggerUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(swaggerUIHTML))
}

// docsOpenAPISpec serves the YAML spec at GET /v1/docs/openapi.yaml.
// Returns 500 if the file can't be read; the error is logged once per
// process start because the cache latches the failure until reset.
func (h *Handlers) docsOpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	docsState.mu.Lock()
	if !docsState.loaded {
		path := OpenAPIPath
		if env := os.Getenv("VAJRA_OPENAPI_PATH"); env != "" {
			path = env
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			docsState.err = err
		} else {
			docsState.path = abs
			b, rerr := os.ReadFile(abs)
			docsState.bytes = b
			docsState.err = rerr
		}
		docsState.loaded = true
	}
	err := docsState.err
	body := docsState.bytes
	path := docsState.path
	docsState.mu.Unlock()
	if err != nil {
		h.log().Error("docs: read openapi.yaml", "err", err, "path", path)
		writeErr(w, http.StatusInternalServerError, "spec unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
