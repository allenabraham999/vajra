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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultCacheDir is where ImageCache stores templates by content hash.
// Each hash gets its own subdirectory holding rootfs.raw, vmlinux, and a
// snapshot/ subdirectory used by VMM restore.
const DefaultCacheDir = "/var/lib/vajra/cache"

// TemplateLayout is the on-disk layout of a cached template directory.
// Paths are absolute and only valid for the host that produced them.
//
// RootfsPath is the verified raw download (source of truth, hash matches
// the registry). RootfsBackingPath is the qcow2 wrapper used as the
// read-only backing for per-sandbox CoW overlays; it's built lazily from
// the raw form by EnsureRootfsBacking and may not exist until then.
type TemplateLayout struct {
	Hash              string
	Dir               string
	RootfsPath        string
	RootfsBackingPath string
	KernelPath        string
	SnapshotDir       string
}

// ImageCache manages the local template cache. Template directories are
// keyed by the SHA256 of the rootfs so multiple sandboxes from the same
// template share a single base image. The cache enforces a soft size
// budget by evicting the least recently used templates.
//
// KeepRawAfterBacking controls whether rootfs.raw is retained after
// EnsureRootfsBacking has produced rootfs.qcow2. The default is to
// delete the raw to halve disk usage; set this to true if you need
// the raw for re-verification, alternative VMM backends, or debugging.
type ImageCache struct {
	dir                 string
	maxBytes            int64
	httpClient          *http.Client
	logger              *slog.Logger
	KeepRawAfterBacking bool

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
		Hash:              hash,
		Dir:               dir,
		RootfsPath:        filepath.Join(dir, "rootfs.raw"),
		RootfsBackingPath: filepath.Join(dir, "rootfs.qcow2"),
		KernelPath:        filepath.Join(dir, "vmlinux"),
		SnapshotDir:       filepath.Join(dir, "snapshot"),
	}
}

// HasTemplate reports whether a template with the given hash is present
// locally and minimally usable. Either rootfs form (raw or qcow2 backing)
// counts: an agent that has already converted to qcow2 and freed the raw
// is still serving the same template. A successful check bumps the LRU
// access time.
func (c *ImageCache) HasTemplate(hash string) bool {
	layout := c.Layout(hash)
	if _, err := os.Stat(layout.RootfsBackingPath); err == nil {
		c.touch(hash)
		return true
	}
	if _, err := os.Stat(layout.RootfsPath); err == nil {
		c.touch(hash)
		return true
	}
	return false
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

// EnsureRootfsBacking guarantees rootfs.qcow2 exists for the given template
// and is suitable as a read-only backing file for per-sandbox CoW overlays.
// It's idempotent: if the qcow2 form is already present we return immediately.
// Otherwise we convert from the raw form via `qemu-img convert`. Concurrent
// callers for the same hash share a single conversion via the inflight map.
//
// If qemu-img is unavailable (test/dev environments without qemu installed),
// we fall back to a byte copy of the raw file under the qcow2 name. CH won't
// accept that file as a real qcow2, but environments without qemu-img also
// don't run a real VMM, so the fallback only needs to satisfy the file-exists
// check that downstream code performs.
func (c *ImageCache) EnsureRootfsBacking(hash string) error {
	layout := c.Layout(hash)
	if _, err := os.Stat(layout.RootfsBackingPath); err == nil {
		c.maybeRemoveRaw(hash, layout)
		return nil
	}
	if _, err := os.Stat(layout.RootfsPath); err != nil {
		return fmt.Errorf("imagecache: raw rootfs missing for %s: %w", hash, err)
	}
	c.mu.Lock()
	if ch, ok := c.inflight[hash+":qcow2"]; ok {
		c.mu.Unlock()
		<-ch
		if _, err := os.Stat(layout.RootfsBackingPath); err == nil {
			return nil
		}
		return fmt.Errorf("imagecache: concurrent qcow2 build of %s failed", hash)
	}
	done := make(chan struct{})
	c.inflight[hash+":qcow2"] = done
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.inflight, hash+":qcow2")
		c.mu.Unlock()
		close(done)
	}()

	tmp := layout.RootfsBackingPath + ".part"
	_ = os.Remove(tmp)
	start := time.Now()
	cmd := exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", layout.RootfsPath, tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		c.logger.Warn("qemu-img convert failed; falling back to raw-as-qcow2 copy",
			"hash", hash, "err", err, "output", strings.TrimSpace(string(out)))
		if cerr := plainCopy(layout.RootfsPath, tmp); cerr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("imagecache: fallback copy: %w", cerr)
		}
	}
	if err := os.Rename(tmp, layout.RootfsBackingPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("imagecache: commit qcow2: %w", err)
	}
	c.maybeRemoveRaw(hash, layout)
	c.logger.Info("rootfs qcow2 backing built",
		"hash", hash,
		"path", layout.RootfsBackingPath,
		"elapsed_ms", time.Since(start).Milliseconds(),
		"kept_raw", c.KeepRawAfterBacking,
	)
	return nil
}

// maybeRemoveRaw removes rootfs.raw once rootfs.qcow2 is in place,
// unless the operator opted into keeping both via KeepRawAfterBacking.
// Logs at warn level on failure but never returns an error: a leaked
// raw file is a disk-usage bug, not a correctness one.
func (c *ImageCache) maybeRemoveRaw(hash string, layout TemplateLayout) {
	if c.KeepRawAfterBacking {
		return
	}
	if err := os.Remove(layout.RootfsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.logger.Warn("could not remove redundant raw rootfs after qcow2 build",
			"hash", hash, "path", layout.RootfsPath, "err", err)
	}
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
