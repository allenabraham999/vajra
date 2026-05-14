package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// DefaultRequestTimeout is the per-request hard ceiling used by NewClient.
// 30s accommodates cold-cache restore on freshly-launched nodes where a
// single /vm.info round-trip can stall while CH faults in a ~500 MB
// memory-ranges file from cold EBS; warm-path calls return in <10ms.
const DefaultRequestTimeout = 30 * time.Second

// apiBase is a placeholder host. The Unix-socket transport ignores host and
// port; the path portion is what matters.
const apiBase = "http://unix/api/v1"

// Client speaks the Cloud Hypervisor REST API over a Unix socket. It is
// safe for concurrent use; the underlying http.Client manages its own
// connection pool keyed by socket path.
type Client struct {
	socketPath string
	http       *http.Client
}

// NewClient returns a Client connected to socketPath with the default 5s
// request timeout.
func NewClient(socketPath string) *Client {
	return NewClientWithTimeout(socketPath, DefaultRequestTimeout)
}

// NewClientWithTimeout returns a Client with a custom per-request timeout.
// The timeout is enforced both via http.Client.Timeout and any context the
// caller passes in (whichever fires first wins).
func NewClientWithTimeout(socketPath string, timeout time.Duration) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
		// CH's API server is single-threaded; one idle conn is plenty.
		MaxIdleConns:    1,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
		},
	}
}

// SocketPath returns the Unix socket this client talks to.
func (c *Client) SocketPath() string { return c.socketPath }

// Create issues PUT /api/v1/vm.create.
func (c *Client) Create(ctx context.Context, cfg VmConfig) error {
	return c.do(ctx, http.MethodPut, "/vm.create", cfg, nil)
}

// Boot issues PUT /api/v1/vm.boot.
func (c *Client) Boot(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/vm.boot", nil, nil)
}

// Pause issues PUT /api/v1/vm.pause, freezing vCPUs in place.
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/vm.pause", nil, nil)
}

// Resume issues PUT /api/v1/vm.resume.
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/vm.resume", nil, nil)
}

// Snapshot issues PUT /api/v1/vm.snapshot. The VM must be paused first.
func (c *Client) Snapshot(ctx context.Context, cfg SnapshotConfig) error {
	return c.do(ctx, http.MethodPut, "/vm.snapshot", cfg, nil)
}

// Restore issues PUT /api/v1/vm.restore on a freshly started VMM that has
// no VM yet. After Restore the VM is paused; call Resume to run it.
func (c *Client) Restore(ctx context.Context, cfg RestoreConfig) error {
	return c.do(ctx, http.MethodPut, "/vm.restore", cfg, nil)
}

// Shutdown issues PUT /api/v1/vm.shutdown — graceful guest shutdown.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/vm.shutdown", nil, nil)
}

// Delete issues PUT /api/v1/vm.delete, removing the VM definition from the
// VMM (the VMM process itself keeps running).
func (c *Client) Delete(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/vm.delete", nil, nil)
}

// Info issues GET /api/v1/vm.info.
func (c *Client) Info(ctx context.Context) (*VmInfo, error) {
	var info VmInfo
	if err := c.do(ctx, http.MethodGet, "/vm.info", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// VmmPing issues GET /api/v1/vmm.ping. Useful as a readiness check.
func (c *Client) VmmPing(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/vmm.ping", nil, nil)
}

// VmmShutdown issues PUT /api/v1/vmm.shutdown, asking cloud-hypervisor to
// exit cleanly.
func (c *Client) VmmShutdown(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/vmm.shutdown", nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cloud-hypervisor %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{
			Status:  resp.StatusCode,
			Method:  method,
			Path:    path,
			Message: string(bytes.TrimSpace(msg)),
		}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// APIError captures a non-2xx response from the Cloud Hypervisor API.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

// Error implements error.
func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("cloud-hypervisor %s %s: HTTP %d", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("cloud-hypervisor %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
}
