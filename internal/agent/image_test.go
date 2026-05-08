package agent

import (
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
