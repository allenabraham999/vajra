package master

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/klauspost/compress/zstd"
)

// stageTemplateDir writes a minimal but complete template directory and
// returns the per-hash subdirectory path.
func stageTemplateDir(t *testing.T, hash string) (root, dir string) {
	t.Helper()
	root = t.TempDir()
	dir = filepath.Join(root, hash)
	if err := os.MkdirAll(filepath.Join(dir, "snapshot"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, body := range templateDirFiles() {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root, dir
}

func templateDirFiles() map[string]string {
	return map[string]string{
		"rootfs.raw":             "ROOTFS-BYTES",
		"vmlinux":                "KERNEL",
		"snapshot/config.json":   "{}",
		"snapshot/memory-ranges": "RANGES",
		"snapshot/state.json":    "STATE",
	}
}

// readBundle decompresses and untars a template bundle into a name→body map.
func readBundle(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	zr, err := zstd.NewReader(r)
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = string(body)
	}
	return out
}

func TestResolveTemplateBundleComplete(t *testing.T) {
	_, dir := stageTemplateDir(t, "h1")
	files, err := resolveTemplateBundle(dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(files) != 5 {
		t.Fatalf("expected 5 files, got %d", len(files))
	}
	if files[0].tarName != "rootfs.raw" {
		t.Fatalf("first entry = %q, want rootfs.raw", files[0].tarName)
	}
}

func TestResolveTemplateBundleMissingRootfs(t *testing.T) {
	_, dir := stageTemplateDir(t, "h2")
	if err := os.Remove(filepath.Join(dir, "rootfs.raw")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := resolveTemplateBundle(dir); err == nil {
		t.Fatalf("expected error for missing rootfs")
	}
}

func TestResolveTemplateBundleQcowFallback(t *testing.T) {
	_, dir := stageTemplateDir(t, "h3")
	if err := os.Remove(filepath.Join(dir, "rootfs.raw")); err != nil {
		t.Fatalf("remove raw: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rootfs.qcow2"), []byte("QCOW"), 0o644); err != nil {
		t.Fatalf("write qcow: %v", err)
	}
	files, err := resolveTemplateBundle(dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if files[0].tarName != "rootfs.qcow2" {
		t.Fatalf("expected qcow2 fallback, got %q", files[0].tarName)
	}
}

func TestResolveTemplateBundleMissingDir(t *testing.T) {
	if _, err := resolveTemplateBundle(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatalf("expected error for missing directory")
	}
}

func TestStreamTemplateBundleRoundTrip(t *testing.T) {
	_, dir := stageTemplateDir(t, "h4")
	files, err := resolveTemplateBundle(dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var buf bytes.Buffer
	if err := streamTemplateBundle(&buf, files); err != nil {
		t.Fatalf("stream: %v", err)
	}
	got := readBundle(t, &buf)
	for name, body := range templateDirFiles() {
		if got[name] != body {
			t.Fatalf("entry %s = %q, want %q", name, got[name], body)
		}
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(got))
	}
}

// TestDownloadTemplateEndpoint exercises the full internal route end to
// end: a staged template directory is served as a tar.zst that an agent
// can decompress.
func TestDownloadTemplateEndpoint(t *testing.T) {
	h := newTestHarness(t)
	const hash = "abc123templatehash"
	root, _ := stageTemplateDir(t, hash)
	h.server.handlers.TemplatesDir = root

	tmpl := &models.Template{
		ID: "tmpl-dl-1", AccountID: "acct-1", Name: "python-test",
		Version: "1.0", Hash: hash, CreatedAt: time.Now().UTC(),
	}
	if err := h.store.Templates().Create(context.Background(), tmpl); err != nil {
		t.Fatalf("create template: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet,
		h.httpSrv.URL+"/internal/templates/tmpl-dl-1/download", nil)
	req.Header.Set("Authorization", "Bearer internal-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-tar-zst" {
		t.Fatalf("content-type = %q", ct)
	}
	got := readBundle(t, resp.Body)
	for name, body := range templateDirFiles() {
		if got[name] != body {
			t.Fatalf("entry %s = %q, want %q", name, got[name], body)
		}
	}
}

func TestDownloadTemplateUnknownID(t *testing.T) {
	h := newTestHarness(t)
	h.server.handlers.TemplatesDir = t.TempDir()

	req, _ := http.NewRequest(http.MethodGet,
		h.httpSrv.URL+"/internal/templates/does-not-exist/download", nil)
	req.Header.Set("Authorization", "Bearer internal-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestDownloadTemplateNotStaged covers the "build registered a template
// row but never staged its image files" case — the agent surfaces this
// body verbatim as the distribution-failure reason.
func TestDownloadTemplateNotStaged(t *testing.T) {
	h := newTestHarness(t)
	h.server.handlers.TemplatesDir = t.TempDir() // empty: nothing staged

	tmpl := &models.Template{
		ID: "tmpl-unstaged", AccountID: "acct-1", Name: "ghost-template",
		Version: "1.0", Hash: "nostagedhash", CreatedAt: time.Now().UTC(),
	}
	if err := h.store.Templates().Create(context.Background(), tmpl); err != nil {
		t.Fatalf("create template: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet,
		h.httpSrv.URL+"/internal/templates/tmpl-unstaged/download", nil)
	req.Header.Set("Authorization", "Bearer internal-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("not staged on master")) {
		t.Fatalf("body should explain the staging gap, got: %s", body)
	}
	if !bytes.Contains(body, []byte("ghost-template")) {
		t.Fatalf("body should name the template, got: %s", body)
	}
}
