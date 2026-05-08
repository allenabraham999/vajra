// Package agent implements the per-host node daemon. The agent owns the
// local Cloud Hypervisor processes, the template image cache, the warm
// pool of pre-restored VMs, and the HTTP control surface that vajra-master
// drives.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// DefaultCacheDir is where ImageCache stores templates by content hash.
// Each hash gets its own subdirectory holding rootfs.raw, vmlinux, and a
// snapshot/ subdirectory used by VMM restore.
const DefaultCacheDir = "/var/lib/vajra/cache"

// TemplateLayout is the on-disk layout of a cached template directory.
// Paths are absolute and only valid for the host that produced them.
type TemplateLayout struct {
	Hash        string
	Dir         string
	RootfsPath  string
	KernelPath  string
	SnapshotDir string
}

// ImageCache manages the local template cache. Template directories are
// keyed by the SHA256 of the rootfs so multiple sandboxes from the same
// template share a single base image. The cache enforces a soft size
// budget by evicting the least recently used templates.
type ImageCache struct {
	dir        string
	maxBytes   int64
	httpClient *http.Client
	logger     *slog.Logger

	mu       sync.Mutex
	atimes   map[string]time.Time // hash -> last-touched time
	inflight map[string]chan struct{}
}

// NewImageCache returns a cache rooted at dir. maxBytes is the soft cap
// triggering EvictLRU; pass 0 to disable eviction. Pass nil for logger to
// use slog.Default. The cache directory is created on first write.
func NewImageCache(dir string, maxBytes int64, logger *slog.Logger) *ImageCache {
	if logger == nil {
		logger = slog.Default()
	}
	return &ImageCache{
		dir:        dir,
		maxBytes:   maxBytes,
		httpClient: &http.Client{Timeout: 5 * time.Minute},
		logger:     logger,
		atimes:     map[string]time.Time{},
		inflight:   map[string]chan struct{}{},
	}
}

// WithHTTPClient lets tests substitute a fake transport. Returns the
// receiver for chaining.
func (c *ImageCache) WithHTTPClient(h *http.Client) *ImageCache {
	c.httpClient = h
	return c
}

// Layout returns the on-disk layout for a template hash. The directory
// may or may not exist — call HasTemplate first to check.
func (c *ImageCache) Layout(hash string) TemplateLayout {
	dir := filepath.Join(c.dir, hash)
	return TemplateLayout{
		Hash:        hash,
		Dir:         dir,
		RootfsPath:  filepath.Join(dir, "rootfs.raw"),
		KernelPath:  filepath.Join(dir, "vmlinux"),
		SnapshotDir: filepath.Join(dir, "snapshot"),
	}
}

// HasTemplate reports whether a template with the given hash is present
// locally and minimally usable (rootfs file exists). A successful check
// also bumps the LRU access time.
func (c *ImageCache) HasTemplate(hash string) bool {
	layout := c.Layout(hash)
	if _, err := os.Stat(layout.RootfsPath); err != nil {
		return false
	}
	c.touch(hash)
	return true
}

// Touch updates the LRU access time for a hash. Callers should invoke this
// every time a sandbox is launched from a template so the cache knows the
// template is hot.
func (c *ImageCache) Touch(hash string) { c.touch(hash) }

func (c *ImageCache) touch(hash string) {
	c.mu.Lock()
	c.atimes[hash] = time.Now()
	c.mu.Unlock()
}

// PullTemplate downloads the rootfs from sourceURL into the cache,
// verifying that its SHA256 matches expectHash. Concurrent pulls of the
// same hash share a single download. The caller is expected to populate
// the kernel and snapshot directory separately (or as part of a multi-file
// fetch flow that wraps this).
//
// If the template is already cached, PullTemplate is a no-op.
func (c *ImageCache) PullTemplate(ctx context.Context, expectHash, sourceURL string) error {
	if expectHash == "" {
		return errors.New("imagecache: empty expectHash")
	}
	if c.HasTemplate(expectHash) {
		return nil
	}
	// Coalesce concurrent pulls.
	c.mu.Lock()
	if ch, ok := c.inflight[expectHash]; ok {
		c.mu.Unlock()
		select {
		case <-ch:
			if c.HasTemplate(expectHash) {
				return nil
			}
			return fmt.Errorf("imagecache: concurrent pull of %s failed", expectHash)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	c.inflight[expectHash] = done
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.inflight, expectHash)
		c.mu.Unlock()
		close(done)
	}()

	layout := c.Layout(expectHash)
	if err := os.MkdirAll(layout.Dir, 0o755); err != nil {
		return fmt.Errorf("create template dir: %w", err)
	}
	tmpPath := layout.RootfsPath + ".part"
	if err := c.fetchAndVerify(ctx, sourceURL, tmpPath, expectHash); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, layout.RootfsPath); err != nil {
		return fmt.Errorf("commit template: %w", err)
	}
	c.touch(expectHash)
	c.logger.Info("template pulled",
		"hash", expectHash,
		"source", sourceURL,
		"dir", layout.Dir,
	)
	return nil
}

func (c *ImageCache) fetchAndVerify(ctx context.Context, sourceURL, tmpPath, expectHash string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch template: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("fetch template: HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)
	if _, err := io.Copy(mw, resp.Body); err != nil {
		_ = f.Close()
		return fmt.Errorf("copy template body: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != expectHash {
		return fmt.Errorf("imagecache: hash mismatch: want %s got %s", expectHash, got)
	}
	return nil
}

// RegisterTemplate records a template that was placed into the cache by an
// out-of-band path (e.g. an admin pre-staging files). It checks that the
// rootfs exists and seeds the LRU table. Returns ErrNotFound if the rootfs
// is missing.
func (c *ImageCache) RegisterTemplate(hash string) error {
	if !c.HasTemplate(hash) {
		return fmt.Errorf("imagecache: template %s not present on disk", hash)
	}
	return nil
}

// EvictLRU removes templates oldest-access-first until total size is at or
// under the soft cap. A maxBytes of 0 disables eviction. Returns the number
// of templates evicted.
func (c *ImageCache) EvictLRU() (int, error) {
	if c.maxBytes <= 0 {
		return 0, nil
	}
	entries, total, err := c.listEntries()
	if err != nil {
		return 0, err
	}
	if total <= c.maxBytes {
		return 0, nil
	}
	c.mu.Lock()
	for i := range entries {
		if t, ok := c.atimes[entries[i].hash]; ok {
			entries[i].atime = t
		}
	}
	c.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].atime.Before(entries[j].atime)
	})
	evicted := 0
	for _, e := range entries {
		if total <= c.maxBytes {
			break
		}
		if err := os.RemoveAll(e.path); err != nil {
			c.logger.Warn("evict failed", "hash", e.hash, "err", err)
			continue
		}
		c.mu.Lock()
		delete(c.atimes, e.hash)
		c.mu.Unlock()
		total -= e.size
		evicted++
		c.logger.Info("template evicted", "hash", e.hash, "freed_bytes", e.size)
	}
	return evicted, nil
}

type cacheEntry struct {
	hash  string
	path  string
	size  int64
	atime time.Time
}

func (c *ImageCache) listEntries() ([]cacheEntry, int64, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("read cache dir: %w", err)
	}
	var out []cacheEntry
	var total int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(c.dir, e.Name())
		size, err := dirSize(full)
		if err != nil {
			return nil, 0, err
		}
		info, _ := e.Info()
		mt := time.Time{}
		if info != nil {
			mt = info.ModTime()
		}
		out = append(out, cacheEntry{
			hash:  e.Name(),
			path:  full,
			size:  size,
			atime: mt,
		})
		total += size
	}
	return out, total, nil
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
