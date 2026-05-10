// Package agent — archive.go implements offline archive/rehydrate of a
// stopped sandbox. ArchiveSandbox stops the sandbox if running, tars +
// zstd-compresses the entire sandbox dir (rootfs overlay + saved snapshot)
// into /var/lib/vajra/archives/{id}.tar.zst, optionally uploads to S3 if
// VAJRA_S3_BUCKET is configured, and removes the local sandbox dir.
// RehydrateSandbox is the inverse: pull the archive (S3 or local), expand
// it back into the sandbox dir, and register the sandbox in STOPPED state
// so a follow-up StartSandbox call can re-restore it.
package agent

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/klauspost/compress/zstd"
)

// DefaultArchiveDir is the per-host directory holding offline archives. It
// is kept distinct from DefaultSandboxRoot so the agent can sweep stale
// sandbox dirs without touching archived bytes.
const DefaultArchiveDir = "/var/lib/vajra/archives"

// ArchiveLocationLocal / ArchiveLocationS3 tag where the archive lives.
const (
	ArchiveLocationLocal = "local"
	ArchiveLocationS3    = "s3"
)

// ArchiveResult is what ArchiveSandbox returns to the caller (and ultimately
// to the master). Path is the absolute filesystem path when Location is
// "local"; for S3 it is "s3://<bucket>/<key>" so callers can round-trip the
// value through Rehydrate.
type ArchiveResult struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	Location   string    `json:"location"`
	SizeBytes  int64     `json:"size_bytes"`
	ArchivedAt time.Time `json:"archived_at"`
}

// ArchiveOptions tunes behaviour. When S3Bucket is non-empty the archive is
// uploaded after local compression and the local copy is then removed.
// Defaults are pulled from VAJRA_S3_BUCKET / VAJRA_S3_PREFIX / VAJRA_S3_REGION
// in NewArchiveManager.
type ArchiveOptions struct {
	ArchiveDir string
	S3Bucket   string
	S3Prefix   string
	S3Region   string
}

// ArchiveManager owns the host-side archive store. It is intentionally
// stateless beyond its config: every call resolves paths from disk so the
// agent can be restarted without losing the catalog.
type ArchiveManager struct {
	sandboxes *SandboxManager
	opts      ArchiveOptions
	logger    *slog.Logger
	s3        *s3.Client
}

// NewArchiveManager wires an archive manager to a sandbox manager and
// fills in defaults from the environment. The S3 client is constructed
// lazily on first use so a missing AWS config doesn't break local-only
// deployments.
func NewArchiveManager(sandboxes *SandboxManager, opts ArchiveOptions, logger *slog.Logger) *ArchiveManager {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.ArchiveDir == "" {
		opts.ArchiveDir = DefaultArchiveDir
	}
	if opts.S3Bucket == "" {
		opts.S3Bucket = os.Getenv("VAJRA_S3_BUCKET")
	}
	if opts.S3Prefix == "" {
		opts.S3Prefix = os.Getenv("VAJRA_S3_PREFIX")
	}
	if opts.S3Region == "" {
		opts.S3Region = os.Getenv("VAJRA_S3_REGION")
	}
	return &ArchiveManager{sandboxes: sandboxes, opts: opts, logger: logger}
}

// ArchiveSandbox stops the sandbox if running, compresses its on-disk state
// into a single .tar.zst, optionally uploads to S3, and removes the local
// sandbox dir. The sandbox entry is also removed from the manager — once
// archived it is no longer "live" on this host.
func (a *ArchiveManager) ArchiveSandbox(ctx context.Context, id string) (*ArchiveResult, error) {
	if id == "" {
		return nil, errors.New("archive: sandbox id required")
	}
	sb, err := a.sandboxes.Get(id)
	if err != nil {
		return nil, fmt.Errorf("archive: %w", err)
	}
	if sb.State == SandboxStateRunning || sb.State == SandboxStatePaused {
		if err := a.sandboxes.StopSandbox(ctx, id); err != nil {
			return nil, fmt.Errorf("archive: stop: %w", err)
		}
	}
	sandboxDir := filepath.Join(a.sandboxes.root, id)
	info, err := os.Stat(sandboxDir)
	if err != nil {
		return nil, fmt.Errorf("archive: stat sandbox dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("archive: %s is not a directory", sandboxDir)
	}
	if err := os.MkdirAll(a.opts.ArchiveDir, 0o755); err != nil {
		return nil, fmt.Errorf("archive: mkdir archive dir: %w", err)
	}
	localPath := filepath.Join(a.opts.ArchiveDir, id+".tar.zst")
	if err := writeTarZst(sandboxDir, localPath); err != nil {
		_ = os.Remove(localPath)
		return nil, fmt.Errorf("archive: compress: %w", err)
	}
	size, err := fileSize(localPath)
	if err != nil {
		return nil, fmt.Errorf("archive: stat output: %w", err)
	}
	res := &ArchiveResult{
		ID:         id,
		Path:       localPath,
		Location:   ArchiveLocationLocal,
		SizeBytes:  size,
		ArchivedAt: time.Now().UTC(),
	}
	if a.opts.S3Bucket != "" {
		key := a.s3Key(id)
		if err := a.uploadS3(ctx, localPath, key); err != nil {
			return nil, fmt.Errorf("archive: s3 upload: %w", err)
		}
		_ = os.Remove(localPath)
		res.Path = "s3://" + a.opts.S3Bucket + "/" + key
		res.Location = ArchiveLocationS3
	}
	if err := os.RemoveAll(sandboxDir); err != nil {
		a.logger.Warn("archive: remove sandbox dir failed", "id", id, "err", err)
	}
	a.sandboxes.removeEntry(id)
	a.logger.Info("sandbox archived",
		"id", id, "path", res.Path, "location", res.Location, "size_bytes", res.SizeBytes)
	return res, nil
}

// RehydrateSandbox is the inverse of ArchiveSandbox. archivePath may be a
// local filesystem path or an "s3://bucket/key" URL; if empty, the manager
// derives the location from its configured options. After expansion the
// sandbox is registered in STOPPED state with its saved snapshot path
// pointing at <sandboxDir>/state, ready for a follow-up StartSandbox call.
func (a *ArchiveManager) RehydrateSandbox(ctx context.Context, id, archivePath string) (*Sandbox, error) {
	if id == "" {
		return nil, errors.New("rehydrate: sandbox id required")
	}
	if _, err := a.sandboxes.Get(id); err == nil {
		return nil, fmt.Errorf("rehydrate: sandbox %s already registered", id)
	}
	sandboxDir := filepath.Join(a.sandboxes.root, id)
	if _, err := os.Stat(sandboxDir); err == nil {
		return nil, fmt.Errorf("rehydrate: sandbox dir %s already exists", sandboxDir)
	}
	src, cleanup, err := a.openArchive(ctx, id, archivePath)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if err := os.MkdirAll(a.sandboxes.root, 0o755); err != nil {
		return nil, fmt.Errorf("rehydrate: mkdir root: %w", err)
	}
	if err := os.MkdirAll(sandboxDir, 0o755); err != nil {
		return nil, fmt.Errorf("rehydrate: mkdir sandbox: %w", err)
	}
	if err := extractTarZst(src, sandboxDir); err != nil {
		_ = os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("rehydrate: extract: %w", err)
	}
	stateDir := filepath.Join(sandboxDir, "state")
	if _, err := os.Stat(stateDir); err != nil {
		stateDir = ""
	}
	now := time.Now().UTC()
	sb := &Sandbox{
		ID:         id,
		State:      SandboxStateStopped,
		VsockCID:   a.sandboxes.AllocateCID(),
		RootfsPath: filepath.Join(sandboxDir, "rootfs.qcow2"),
		StateDir:   stateDir,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	a.sandboxes.AdoptSandbox(sb)
	a.logger.Info("sandbox rehydrated", "id", id, "dir", sandboxDir)
	return a.sandboxes.Get(id)
}

// openArchive resolves an archive reference to a local file open for reading.
// archivePath wins when set; otherwise the manager probes its configured
// store (S3 first if a bucket is set, then the local archive dir).
func (a *ArchiveManager) openArchive(ctx context.Context, id, archivePath string) (io.ReadCloser, func(), error) {
	if archivePath == "" {
		if a.opts.S3Bucket != "" {
			archivePath = "s3://" + a.opts.S3Bucket + "/" + a.s3Key(id)
		} else {
			archivePath = filepath.Join(a.opts.ArchiveDir, id+".tar.zst")
		}
	}
	if strings.HasPrefix(archivePath, "s3://") {
		bucket, key, err := parseS3URL(archivePath)
		if err != nil {
			return nil, func() {}, err
		}
		tmp, err := os.CreateTemp("", "vajra-archive-*.tar.zst")
		if err != nil {
			return nil, func() {}, fmt.Errorf("rehydrate: tempfile: %w", err)
		}
		tmp.Close()
		if err := a.downloadS3(ctx, bucket, key, tmp.Name()); err != nil {
			_ = os.Remove(tmp.Name())
			return nil, func() {}, fmt.Errorf("rehydrate: s3 download: %w", err)
		}
		f, err := os.Open(tmp.Name())
		if err != nil {
			_ = os.Remove(tmp.Name())
			return nil, func() {}, fmt.Errorf("rehydrate: open temp: %w", err)
		}
		return f, func() { _ = f.Close(); _ = os.Remove(tmp.Name()) }, nil
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, func() {}, fmt.Errorf("rehydrate: open archive: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}

func (a *ArchiveManager) s3Key(id string) string {
	if a.opts.S3Prefix == "" {
		return "archives/" + id + ".tar.zst"
	}
	return strings.TrimRight(a.opts.S3Prefix, "/") + "/" + id + ".tar.zst"
}

func (a *ArchiveManager) ensureS3(ctx context.Context) (*s3.Client, error) {
	if a.s3 != nil {
		return a.s3, nil
	}
	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if a.opts.S3Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(a.opts.S3Region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	a.s3 = s3.NewFromConfig(cfg)
	return a.s3, nil
}

func (a *ArchiveManager) uploadS3(ctx context.Context, localPath, key string) error {
	client, err := a.ensureS3(ctx)
	if err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	uploader := manager.NewUploader(client)
	_, err = uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: &a.opts.S3Bucket,
		Key:    &key,
		Body:   f,
	})
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

func (a *ArchiveManager) downloadS3(ctx context.Context, bucket, key, destPath string) error {
	client, err := a.ensureS3(ctx)
	if err != nil {
		return err
	}
	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer f.Close()
	downloader := manager.NewDownloader(client)
	_, err = downloader.Download(ctx, f, &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	return nil
}

func parseS3URL(u string) (string, string, error) {
	const prefix = "s3://"
	if !strings.HasPrefix(u, prefix) {
		return "", "", fmt.Errorf("not an s3 url: %s", u)
	}
	rest := strings.TrimPrefix(u, prefix)
	idx := strings.IndexByte(rest, '/')
	if idx < 0 {
		return "", "", fmt.Errorf("s3 url missing key: %s", u)
	}
	return rest[:idx], rest[idx+1:], nil
}

// writeTarZst streams srcDir into a single tar+zstd file at outPath.
// File modes and modification times are preserved; symlinks/special files
// are skipped (CH snapshot dirs only carry regular files in practice).
func writeTarZst(srcDir, outPath string) error {
	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	defer out.Close()
	enc, err := zstd.NewWriter(out)
	if err != nil {
		return fmt.Errorf("zstd writer: %w", err)
	}
	defer enc.Close()
	tw := tar.NewWriter(enc)
	defer tw.Close()
	rootInfo, err := os.Lstat(srcDir)
	if err != nil {
		return fmt.Errorf("lstat root: %w", err)
	}
	prefix := filepath.Clean(srcDir)
	walkErr := filepath.Walk(prefix, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(prefix, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		if rel == "." {
			rel = filepath.Base(prefix)
			_ = rootInfo
		}
		mode := info.Mode()
		if mode&os.ModeSymlink != 0 || mode&os.ModeDevice != 0 || mode&os.ModeNamedPipe != 0 || mode&os.ModeSocket != 0 {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("tar header %s: %w", path, err)
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write header %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		if _, err := io.Copy(tw, f); err != nil {
			_ = f.Close()
			return fmt.Errorf("copy %s: %w", path, err)
		}
		return f.Close()
	})
	if walkErr != nil {
		return walkErr
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close zstd: %w", err)
	}
	return nil
}

// extractTarZst expands a tar+zstd stream into destDir. Existing files are
// overwritten; missing parent directories are created with mode 0755 so
// archives produced by writeTarZst restore round-trip even when the
// directory headers came after their children (rare but legal).
func extractTarZst(src io.Reader, destDir string) error {
	dec, err := zstd.NewReader(src)
	if err != nil {
		return fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()
	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") {
			return fmt.Errorf("unsafe path in archive: %q", hdr.Name)
		}
		target := filepath.Join(destDir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0o700); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o7777)
			if err != nil {
				return fmt.Errorf("open %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %s: %w", target, err)
			}
		default:
			// CH snapshot/rootfs dirs don't carry symlinks or specials in
			// practice; skip silently to keep the extractor minimal.
		}
	}
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
