// Package agent — files.go is the host-side file API: upload, download,
// list. Each call opens a fresh vsock connection to GuestFilesPort,
// writes a typed JSON request, and either streams payload bytes through
// or reads them back. The wire shape is documented next to the
// scripts/guest-agent serveFiles handler.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// DefaultFileTimeout caps a single file op when the caller passes
// timeout <= 0. Big enough to swallow a slow vsock + a couple-hundred-MB
// transfer; small enough that a hung guest agent doesn't block forever.
const DefaultFileTimeout = 5 * time.Minute

// FileEntry mirrors the per-row shape the guest emits in its list
// response. Mode is the POSIX permission bits only (no type bits).
type FileEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    uint32    `json:"mode"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// fileWireRequest is the typed envelope sent on the files port. Op
// dispatches the guest handler; the remaining fields are populated per
// op.
type fileWireRequest struct {
	Op   string `json:"op"`
	Path string `json:"path,omitempty"`
	Dir  string `json:"dir,omitempty"`
	Size int64  `json:"size,omitempty"`
	Mode uint32 `json:"mode,omitempty"`
}

// fileWireResponse is the envelope the guest replies with. Different
// ops fill in different combinations.
type fileWireResponse struct {
	OK      bool        `json:"ok,omitempty"`
	Error   string      `json:"error,omitempty"`
	Size    int64       `json:"size,omitempty"`
	Mode    uint32      `json:"mode,omitempty"`
	Entries []FileEntry `json:"entries,omitempty"`
}

// FileUploadRequest is what callers hand to FileUpload.
type FileUploadRequest struct {
	Path    string        // destination path inside the VM (absolute)
	Mode    uint32        // POSIX permission bits, 0 → 0o644
	Size    int64         // exact byte count expected from Body
	Body    io.Reader     // file contents
	Timeout time.Duration // 0 → DefaultFileTimeout
}

// FileUpload streams a file into the guest VM. The guest writes to a
// sibling tempfile and renames so a partial upload never leaves a
// half-written file in place.
func (m *SandboxManager) FileUpload(ctx context.Context, sandboxID string, req FileUploadRequest) error {
	if req.Path == "" {
		return errors.New("file: path is required")
	}
	if req.Size < 0 {
		return errors.New("file: negative size")
	}
	if req.Body == nil {
		return errors.New("file: nil body")
	}
	conn, cleanup, err := m.dialFiles(ctx, sandboxID, req.Timeout)
	if err != nil {
		return err
	}
	defer cleanup()

	header := fileWireRequest{
		Op:   "upload",
		Path: req.Path,
		Size: req.Size,
		Mode: req.Mode,
	}
	if err := writeJSONLine(conn, header); err != nil {
		return fmt.Errorf("file: send header: %w", err)
	}
	// Stream exactly Size bytes. CopyN guards against a short body
	// silently succeeding on the guest side (which would leave the
	// guest waiting on a stalled stream).
	if _, err := io.CopyN(conn, req.Body, req.Size); err != nil {
		return fmt.Errorf("file: send body: %w", err)
	}
	var resp fileWireResponse
	if err := readJSONLine(conn, &resp); err != nil {
		return fmt.Errorf("file: read response: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("file: guest error: %s", resp.Error)
	}
	if !resp.OK {
		return errors.New("file: guest reported not OK")
	}
	return nil
}

// FileDownloadResult holds the bytes (lazily streamed) and metadata for
// a download. The caller MUST close Body before the guest connection
// can be reused — failing to do so leaks the underlying socket.
type FileDownloadResult struct {
	Size int64
	Mode uint32
	Body io.ReadCloser
}

// FileDownload opens a guest connection, requests one path, and returns
// a streaming reader. Body is a LimitReader over the connection bounded
// by the size the guest announced; closing it closes the connection.
func (m *SandboxManager) FileDownload(ctx context.Context, sandboxID, path string, timeout time.Duration) (*FileDownloadResult, error) {
	if path == "" {
		return nil, errors.New("file: path is required")
	}
	conn, cleanup, err := m.dialFiles(ctx, sandboxID, timeout)
	if err != nil {
		return nil, err
	}
	// On any error before we hand control to the caller, run cleanup;
	// on success, hand cleanup off to the returned ReadCloser.
	success := false
	defer func() {
		if !success {
			cleanup()
		}
	}()

	if err := writeJSONLine(conn, fileWireRequest{Op: "download", Path: path}); err != nil {
		return nil, fmt.Errorf("file: send request: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("file: read header: %w", err)
	}
	var resp fileWireResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("file: decode header: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("file: guest error: %s", resp.Error)
	}
	body := &downloadBody{
		Reader: io.LimitReader(br, resp.Size),
		closer: cleanup,
	}
	success = true
	return &FileDownloadResult{Size: resp.Size, Mode: resp.Mode, Body: body}, nil
}

// FileList returns the entries inside a directory in the guest VM.
func (m *SandboxManager) FileList(ctx context.Context, sandboxID, dir string, timeout time.Duration) ([]FileEntry, error) {
	conn, cleanup, err := m.dialFiles(ctx, sandboxID, timeout)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	if err := writeJSONLine(conn, fileWireRequest{Op: "list", Dir: dir}); err != nil {
		return nil, fmt.Errorf("file: send request: %w", err)
	}
	var resp fileWireResponse
	if err := readJSONLine(conn, &resp); err != nil {
		return nil, fmt.Errorf("file: read response: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("file: guest error: %s", resp.Error)
	}
	if resp.Entries == nil {
		return []FileEntry{}, nil
	}
	return resp.Entries, nil
}

// dialFiles is the shared setup: lookup sandbox, validate state, dial
// vsock with the appropriate deadline. Returns the connection and a
// cleanup that the caller must run (typically as a defer).
func (m *SandboxManager) dialFiles(ctx context.Context, sandboxID string, timeout time.Duration) (io.ReadWriteCloser, func(), error) {
	sb, err := m.lookup(sandboxID)
	if err != nil {
		return nil, nil, err
	}
	if sb.State != SandboxStateRunning {
		return nil, nil, fmt.Errorf("sandbox: not running (state %s)", sb.State)
	}
	if timeout <= 0 {
		timeout = DefaultFileTimeout
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	conn, err := m.dialer.Dial(dialCtx, sb.VsockSocketPath, GuestFilesPort)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("sandbox: dial vsock: %w", err)
	}
	cleanup := func() {
		_ = conn.Close()
		cancel()
	}
	return conn, cleanup, nil
}

// downloadBody is an io.ReadCloser whose Close tears down the
// underlying vsock connection. The embedded io.Reader is set to an
// io.LimitReader bounded by the size announced by the guest, so callers
// can't accidentally read past their file into a subsequent JSON envelope.
type downloadBody struct {
	io.Reader
	closer func()
	closed bool
}

// Close runs the cleanup func exactly once. The underlying connection
// is closed inside cleanup; any read error from the wrapped reader is
// returned to the caller via Read, not here.
func (b *downloadBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	if b.closer != nil {
		b.closer()
	}
	return nil
}
