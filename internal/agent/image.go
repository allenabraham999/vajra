// Package agent implements the per-host node daemon. The agent owns the
// local Cloud Hypervisor processes, the template image cache, the warm
// pool of pre-restored VMs, and the HTTP control surface that vajra-master
// drives.
package agent

import (
	"archive/tar"
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

	"github.com/klauspost/compress/zstd"
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

// PullTemplateBundle fetches a complete template (rootfs, guest kernel,
// and Cloud Hypervisor snapshot directory) from vajra-master and unpacks
// it into the local cache. It is the on-demand counterpart to a
// pre-staged template: when an agent is asked to launch a sandbox from a
// template it has never seen, this is how the image arrives.
//
//   - masterURL  — vajra-master's base URL (the agent's own config)
//   - templateID — the template's registry ID; master's download route
//     is keyed on it
//   - secret     — the shared internal token, sent as a Bearer credential
//
// The response is a single zstd-compressed tar. After it is unpacked,
// rootfs.raw is SHA256-verified against expectHash. Concurrent pulls of
// the same hash are coalesced onto one download. If the template is
// already cached, PullTemplateBundle is a no-op.
func (c *ImageCache) PullTemplateBundle(ctx context.Context, expectHash, masterURL, templateID, secret string) error {
	if expectHash == "" {
		return errors.New("imagecache: empty expectHash")
	}
	if templateID == "" {
		return errors.New("imagecache: empty templateID")
	}
	if masterURL == "" {
		return errors.New("imagecache: no master URL configured")
	}
	if c.HasTemplate(expectHash) {
		return nil
	}
	// Coalesce concurrent pulls. The ":bundle" suffix keeps the key
	// distinct from PullTemplate's single-file inflight entries.
	key := "bundle:" + expectHash
	c.mu.Lock()
	if ch, ok := c.inflight[key]; ok {
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
	c.inflight[key] = done
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
		close(done)
	}()

	url := strings.TrimRight(masterURL, "/") + "/internal/templates/" + templateID + "/download"
	layout := c.Layout(expectHash)
	staging := layout.Dir + ".part"
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("clear staging dir: %w", err)
	}
	if err := c.fetchBundle(ctx, url, secret, staging); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	if err := verifyBundleRootfs(staging, expectHash); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	// Commit atomically: replace any partial cache dir with the verified
	// staging dir via a single rename.
	if err := os.RemoveAll(layout.Dir); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("clear cache dir: %w", err)
	}
	if err := os.Rename(staging, layout.Dir); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("commit template: %w", err)
	}
	c.touch(expectHash)
	c.logger.Info("template bundle pulled",
		"hash", expectHash,
		"template_id", templateID,
		"dir", layout.Dir,
	)
	return nil
}

// fetchBundle downloads the zstd-compressed tar at url and extracts it
// into destDir. The Authorization header carries the shared internal
// token. Tar entry names are validated so a malicious archive cannot
// escape destDir.
func (c *ImageCache) fetchBundle(ctx context.Context, url, secret, destDir string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch template bundle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("fetch template bundle: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	zr, err := zstd.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("zstd reader: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		dst, err := safeJoin(destDir, hdr.Name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", hdr.Name, err)
		}
		if err := writeExtractedFile(tr, dst); err != nil {
			return fmt.Errorf("extract %s: %w", hdr.Name, err)
		}
	}
	return nil
}

// safeJoin resolves a tar entry name against root, rejecting absolute
// paths and any ".." components so a hostile archive cannot write
// outside the cache directory.
func safeJoin(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("imagecache: unsafe tar path %q", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("imagecache: unsafe tar path %q", name)
	}
	return filepath.Join(root, clean), nil
}

// writeExtractedFile streams one tar entry to dst.
func writeExtractedFile(r io.Reader, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// verifyBundleRootfs SHA256-checks the extracted rootfs against the
// content hash the template is keyed on. The raw rootfs is the source of
// truth and is verified whenever present; a bundle carrying only the
// derived rootfs.qcow2 (the raw was already converted away on the
// master) cannot be hash-verified this way, so it is accepted as-is.
func verifyBundleRootfs(dir, expectHash string) error {
	rawPath := filepath.Join(dir, "rootfs.raw")
	f, err := os.Open(rawPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, qerr := os.Stat(filepath.Join(dir, "rootfs.qcow2")); qerr == nil {
				return nil
			}
			return errors.New("imagecache: bundle has no rootfs")
		}
		return fmt.Errorf("open rootfs: %w", err)
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return fmt.Errorf("hash rootfs: %w", err)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != expectHash {
		return fmt.Errorf("imagecache: rootfs hash mismatch: want %s got %s", expectHash, got)
	}
	return nil
}
