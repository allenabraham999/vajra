// Package agent — migrate.go implements offline sandbox migration. The
// source agent stops the sandbox, streams a tar of its on-disk dir to the
// target's POST /sandbox/receive endpoint, the target unpacks it, and the
// source then deletes its local copy. Master coordinates the DB update
// (node_id, cluster_id) once both sides report success.
package agent

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MigrateResult is the report a source agent returns to master after
// successfully shipping a sandbox to the target. Master uses TargetURL
// only to confirm the call landed on the expected node.
type MigrateResult struct {
	ID         string    `json:"id"`
	TargetURL  string    `json:"target_url"`
	BytesSent  int64     `json:"bytes_sent"`
	MigratedAt time.Time `json:"migrated_at"`
}

// migrateUserAgent identifies migration POSTs in the target's access logs.
const migrateUserAgent = "vajra-agent-migrator/1"

// MigrateSandbox stops the sandbox locally, streams the resulting on-disk
// dir as a tar to targetAddr/sandbox/receive, and on a 2xx response wipes
// the local copy. authToken is sent verbatim as the Bearer credential —
// callers should pass the agent shared secret so the target's
// InternalAuth middleware accepts the request.
//
// targetAddr is the base URL of the target agent ("http://10.0.1.5:9000").
// If empty, the migration is rejected before any state mutation.
func (m *SandboxManager) MigrateSandbox(ctx context.Context, id, targetAddr, authToken string) (*MigrateResult, error) {
	if id == "" {
		return nil, errors.New("migrate: sandbox id required")
	}
	if targetAddr == "" {
		return nil, errors.New("migrate: target address required")
	}
	sb, err := m.Get(id)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if sb.State == SandboxStateRunning || sb.State == SandboxStatePaused {
		if err := m.StopSandbox(ctx, id); err != nil {
			return nil, fmt.Errorf("migrate: stop: %w", err)
		}
	}
	sandboxDir := filepath.Join(m.root, id)
	if _, err := os.Stat(sandboxDir); err != nil {
		return nil, fmt.Errorf("migrate: stat sandbox dir: %w", err)
	}
	target := strings.TrimRight(targetAddr, "/") + "/sandbox/receive"
	pr, pw := io.Pipe()
	counter := &countingReader{r: pr}
	streamErr := make(chan error, 1)
	go func() {
		err := writeTarStream(sandboxDir, pw)
		if err != nil {
			_ = pw.CloseWithError(err)
		} else {
			_ = pw.Close()
		}
		streamErr <- err
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, counter)
	if err != nil {
		_ = pr.Close()
		return nil, fmt.Errorf("migrate: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set("X-Vajra-Sandbox-ID", id)
	req.Header.Set("User-Agent", migrateUserAgent)
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("migrate: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("migrate: target returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := <-streamErr; err != nil {
		return nil, fmt.Errorf("migrate: stream tar: %w", err)
	}
	if err := os.RemoveAll(sandboxDir); err != nil {
		m.logger.Warn("migrate: remove source dir failed", "id", id, "err", err)
	}
	m.removeEntry(id)
	m.logger.Info("sandbox migrated", "id", id, "target", target, "bytes", counter.n)
	return &MigrateResult{
		ID:         id,
		TargetURL:  target,
		BytesSent:  counter.n,
		MigratedAt: time.Now().UTC(),
	}, nil
}

// ReceiveSandbox is the target-side handler body for POST /sandbox/receive.
// It reads a tar stream from r, unpacks it into root/<id>, and registers
// the sandbox in STOPPED state so a follow-up StartSandbox call reactivates
// it. The id must match the source's X-Vajra-Sandbox-ID header (the HTTP
// handler enforces this before invoking).
func (m *SandboxManager) ReceiveSandbox(_ context.Context, id string, r io.Reader) (*Sandbox, error) {
	if id == "" {
		return nil, errors.New("receive: sandbox id required")
	}
	if _, err := m.Get(id); err == nil {
		return nil, fmt.Errorf("receive: sandbox %s already registered", id)
	}
	sandboxDir := filepath.Join(m.root, id)
	if _, err := os.Stat(sandboxDir); err == nil {
		return nil, fmt.Errorf("receive: sandbox dir %s already exists", sandboxDir)
	}
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return nil, fmt.Errorf("receive: mkdir root: %w", err)
	}
	if err := os.MkdirAll(sandboxDir, 0o755); err != nil {
		return nil, fmt.Errorf("receive: mkdir sandbox: %w", err)
	}
	if err := extractTarStream(r, sandboxDir); err != nil {
		_ = os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("receive: extract: %w", err)
	}
	stateDir := filepath.Join(sandboxDir, "state")
	if _, err := os.Stat(stateDir); err != nil {
		stateDir = ""
	}
	now := time.Now().UTC()
	sb := &Sandbox{
		ID:         id,
		State:      SandboxStateStopped,
		VsockCID:   m.AllocateCID(),
		RootfsPath: filepath.Join(sandboxDir, "rootfs.qcow2"),
		StateDir:   stateDir,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	m.AdoptSandbox(sb)
	m.logger.Info("sandbox received", "id", id, "dir", sandboxDir)
	return m.Get(id)
}

// writeTarStream streams srcDir into w as an uncompressed tar. The wire
// format is plain tar (not zstd) — migration is point-to-point and the
// network is the bottleneck, so paying the CPU for compression on both
// ends is rarely a win at gigabit-class link speeds.
func writeTarStream(srcDir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	prefix := filepath.Clean(srcDir)
	walkErr := filepath.Walk(prefix, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(prefix, path)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = filepath.Base(prefix)
		}
		mode := info.Mode()
		if mode&os.ModeSymlink != 0 || mode&os.ModeDevice != 0 || mode&os.ModeNamedPipe != 0 || mode&os.ModeSocket != 0 {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, f); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	})
	if walkErr != nil {
		return walkErr
	}
	return tw.Close()
}

// extractTarStream unpacks a plain (uncompressed) tar stream into destDir.
// Mirrors extractTarZst but skips the zstd decoder; the migration wire
// format is uncompressed tar.
func extractTarStream(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)
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
			// Source produces only dir+regular entries; ignore other types.
		}
	}
}

// countingReader wraps an io.Reader and tallies the bytes read so the
// migrator can report transfer size without a second pass over the data.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
