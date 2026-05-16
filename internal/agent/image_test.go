package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestImageCachePullVerifies(t *testing.T) {
	body := []byte("rootfs-bytes")
	want := sha256.Sum256(body)
	wantHex := hex.EncodeToString(want[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := NewImageCache(dir, 0, nil)
	if err := c.PullTemplate(context.Background(), wantHex, srv.URL); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !c.HasTemplate(wantHex) {
		t.Fatalf("expected template to be present")
	}
	got, err := os.ReadFile(c.Layout(wantHex).RootfsPath)
	if err != nil {
		t.Fatalf("read rootfs: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("rootfs mismatch")
	}
}

func TestImageCachePullDetectsHashMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not-the-bytes-you-promised"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := NewImageCache(dir, 0, nil)
	err := c.PullTemplate(context.Background(), strings.Repeat("a", 64), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("expected hash mismatch error, got %v", err)
	}
	// Partial file must be cleaned up so a retry doesn't see stale bytes.
	if _, err := os.Stat(filepath.Join(dir, strings.Repeat("a", 64), "rootfs.raw.part")); !os.IsNotExist(err) {
		t.Fatalf("expected .part to be removed; stat err = %v", err)
	}
}

func TestImageCacheConcurrentPullsCoalesce(t *testing.T) {
	body := []byte("hello")
	want := sha256.Sum256(body)
	hash := hex.EncodeToString(want[:])

	var fetches int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		fetches++
		mu.Unlock()
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := NewImageCache(t.TempDir(), 0, nil)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.PullTemplate(context.Background(), hash, srv.URL); err != nil {
				t.Errorf("pull: %v", err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if fetches > 5 {
		t.Fatalf("unexpectedly many fetches: %d", fetches)
	}
}

func TestImageCacheEvictsLRU(t *testing.T) {
	dir := t.TempDir()
	c := NewImageCache(dir, 100, nil)
	// Two templates of 80 bytes each; total exceeds 100-byte budget.
	for _, h := range []string{"aaaa", "bbbb"} {
		full := filepath.Join(dir, h)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(full, "rootfs.raw"), make([]byte, 80), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// Touch "bbbb" recently; "aaaa" should evict first.
	c.Touch("aaaa")
	c.Touch("bbbb") // touched second → newer
	// Force "aaaa" to be older.
	c.atimes["aaaa"] = c.atimes["aaaa"].Add(-1)
	n, err := c.EvictLRU()
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if n == 0 {
		t.Fatalf("expected at least one eviction, got 0")
	}
	if _, err := os.Stat(filepath.Join(dir, "aaaa")); !os.IsNotExist(err) {
		t.Fatalf("expected aaaa to be evicted; stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bbbb")); err != nil {
		t.Fatalf("expected bbbb to survive; stat err = %v", err)
	}
}

// buildTestBundle produces a zstd-compressed tar of the given files —
// the same wire format master's downloadTemplate endpoint emits.
func buildTestBundle(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return buf.Bytes()
}

func fullTestBundle(rootfs []byte) map[string][]byte {
	return map[string][]byte{
		"rootfs.raw":             rootfs,
		"vmlinux":                []byte("kernel"),
		"snapshot/config.json":   []byte("{}"),
		"snapshot/memory-ranges": []byte("ranges"),
		"snapshot/state.json":    []byte("state"),
	}
}

func TestPullTemplateBundle(t *testing.T) {
	rootfs := []byte("rootfs-content")
	sum := sha256.Sum256(rootfs)
	hash := hex.EncodeToString(sum[:])
	bundle := buildTestBundle(t, fullTestBundle(rootfs))

	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/x-tar-zst")
		_, _ = w.Write(bundle)
	}))
	defer srv.Close()

	c := NewImageCache(t.TempDir(), 0, nil)
	if err := c.PullTemplateBundle(context.Background(), hash, srv.URL, "tmpl-123", "secret-token"); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !c.HasTemplate(hash) {
		t.Fatalf("template not present after pull")
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("auth header = %q, want Bearer secret-token", gotAuth)
	}
	if gotPath != "/internal/templates/tmpl-123/download" {
		t.Fatalf("request path = %q", gotPath)
	}
	layout := c.Layout(hash)
	for _, p := range []string{
		layout.RootfsPath,
		layout.KernelPath,
		filepath.Join(layout.SnapshotDir, "config.json"),
		filepath.Join(layout.SnapshotDir, "memory-ranges"),
		filepath.Join(layout.SnapshotDir, "state.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s extracted: %v", p, err)
		}
	}
}

func TestPullTemplateBundleHashMismatch(t *testing.T) {
	bundle := buildTestBundle(t, fullTestBundle([]byte("actual-content")))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bundle)
	}))
	defer srv.Close()

	c := NewImageCache(t.TempDir(), 0, nil)
	wrongHash := strings.Repeat("a", 64)
	err := c.PullTemplateBundle(context.Background(), wrongHash, srv.URL, "tmpl-1", "tok")
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("expected hash mismatch error, got %v", err)
	}
	if c.HasTemplate(wrongHash) {
		t.Fatalf("template must not be committed on hash mismatch")
	}
}

func TestPullTemplateBundleHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `template "python-test" not staged on master: rootfs missing`, http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewImageCache(t.TempDir(), 0, nil)
	err := c.PullTemplateBundle(context.Background(), strings.Repeat("b", 64), srv.URL, "tmpl-1", "tok")
	if err == nil {
		t.Fatalf("expected error on HTTP 404")
	}
	// The agent surfaces master's reason verbatim so the create failure
	// is actionable rather than an opaque "not in cache".
	if !strings.Contains(err.Error(), "not staged on master") {
		t.Fatalf("error should carry master's reason, got: %v", err)
	}
}

func TestPullTemplateBundleRejectsUnsafePaths(t *testing.T) {
	bundle := buildTestBundle(t, map[string][]byte{"../escape": []byte("evil")})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bundle)
	}))
	defer srv.Close()

	c := NewImageCache(t.TempDir(), 0, nil)
	err := c.PullTemplateBundle(context.Background(), strings.Repeat("c", 64), srv.URL, "tmpl-1", "tok")
	if err == nil {
		t.Fatalf("expected rejection of path-traversal tar entry")
	}
}
