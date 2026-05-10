// Package master implements the vajra-master control plane. The dispatcher
// in this file is master's outbound HTTP client to a single node agent: it
// translates control-plane decisions (create, stop, destroy, exec...) into
// HTTP calls against the agent process running on each host.
package master

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

// DefaultAgentPort is the TCP port master assumes every agent listens on.
// The schema doesn't yet record a per-node port, so we hardcode the value
// from agent.DefaultListenAddr (":9000"). When models.Node grows a Port
// field, swap NewAgentClient over to it.
const DefaultAgentPort = 9000

// agentRequestTimeout caps a single HTTP attempt (excluding retry waits).
// Sized for the slowest synchronous handler we still serve inline: a
// CreateSandbox call that does CoW + CH restore + state polling can take
// 5-15s on cold paths and we want headroom over that. Operations that
// legitimately exceed this (large snapshot exports) must pass a context
// with their own deadline; this is just the per-attempt guardrail so a
// hung agent can't block a goroutine forever.
const agentRequestTimeout = 60 * time.Second

// RetryPolicy controls exponential-backoff retries inside (*AgentClient).do.
// Only network errors and 5xx responses are retried; 4xx is surfaced
// immediately because retrying a malformed request will never help.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// DefaultRetryPolicy is the policy applied when NewAgentClient is given a
// zero-value RetryPolicy. Three attempts with 100ms→200ms→400ms (capped at
// 500ms) backoff matches the brief.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    500 * time.Millisecond,
	}
}

// AgentClient is master's outbound HTTP client to one node agent. Instances
// are immutable once constructed and safe for concurrent use; the AgentPool
// caches one per node ID.
type AgentClient struct {
	nodeID  string
	baseURL string
	secret  string // shared agent-master secret used in Authorization
	http    *http.Client
	retry   RetryPolicy
	logger  *slog.Logger
}

// NewAgentClient builds a client for a single node. baseURL is derived as
// "http://<node.IP>:<DefaultAgentPort>" until the schema grows a port
// column. The shared secret is sent verbatim as a Bearer token; auth.go
// owns issuing/validating it.
func NewAgentClient(node *models.Node, sharedSecret string, logger *slog.Logger) *AgentClient {
	if logger == nil {
		logger = slog.Default()
	}
	id := ""
	ip := ""
	if node != nil {
		id = node.ID
		ip = node.IP
	}
	return &AgentClient{
		nodeID:  id,
		baseURL: fmt.Sprintf("http://%s:%d", ip, DefaultAgentPort),
		secret:  sharedSecret,
		http:    &http.Client{Timeout: agentRequestTimeout},
		retry:   DefaultRetryPolicy(),
		logger:  logger.With("node_id", id),
	}
}

// NodeID returns the node this client targets. Useful for logging.
func (c *AgentClient) NodeID() string { return c.nodeID }

// BaseURL returns the http://host:port prefix this client sends to. Useful
// for tests and diagnostics.
func (c *AgentClient) BaseURL() string { return c.baseURL }

// SandboxConfig is the resource shape master sends to the agent. We define
// our own copy rather than importing internal/agent so the master package
// has zero dependency on agent (avoids awkward coupling even if it isn't
// a true import cycle).
type SandboxConfig struct {
	VCPUs    int `json:"vcpus"`
	MemoryMB int `json:"memory_mb"`
	DiskGB   int `json:"disk_gb"`
}

// CreateSandboxRequest is the JSON body of POST /sandbox/create.
type CreateSandboxRequest struct {
	ID           string        `json:"id,omitempty"`
	TemplateHash string        `json:"template_hash"`
	Config       SandboxConfig `json:"config"`
	FromPool     bool          `json:"from_pool,omitempty"`
}

// CreateSandboxResponse mirrors the agent's Sandbox JSON. Master only
// inspects the fields it needs to drive the FSM — extra fields are
// preserved by the agent but silently ignored here.
type CreateSandboxResponse struct {
	ID              string `json:"id"`
	State           string `json:"state"`
	TemplateHash    string `json:"template_hash,omitempty"`
	VsockCID        uint32 `json:"vsock_cid,omitempty"`
	APISocket       string `json:"api_socket,omitempty"`
	VsockSocketPath string `json:"vsock_socket,omitempty"`
	RootfsPath      string `json:"rootfs_path,omitempty"`
	FromPool        bool   `json:"from_pool,omitempty"`
}

// SandboxView is the minimal slice of a Sandbox the reconciler needs.
// Keeping this small avoids leaking transient fields like socket paths into
// master's reconciliation logic.
type SandboxView struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// AgentSandboxView is the per-row shape ListSandboxes returns. Same shape
// as SandboxView for now, declared separately so each can grow fields
// independently of the other (the list endpoint is likely to surface usage
// counters that GetSandbox does not).
type AgentSandboxView struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// ExecResult is the outcome of a guest-side command invocation.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// SnapshotResult is the JSON body returned by the (forthcoming) snapshot
// endpoint on the agent.
type SnapshotResult struct {
	SnapshotPath string `json:"snapshot_path"`
	SizeBytes    int64  `json:"size_bytes"`
}

// ArchiveResult is the body returned by the agent's POST /sandbox/{id}/archive.
// Path is a local filesystem path when Location=="local" and an
// "s3://bucket/key" URL when Location=="s3".
type ArchiveResult struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	Location   string    `json:"location"`
	SizeBytes  int64     `json:"size_bytes"`
	ArchivedAt time.Time `json:"archived_at"`
}

// MigrateResult is the body returned by the agent's POST /sandbox/{id}/migrate
// after a successful transfer.
type MigrateResult struct {
	ID         string    `json:"id"`
	TargetURL  string    `json:"target_url"`
	BytesSent  int64     `json:"bytes_sent"`
	MigratedAt time.Time `json:"migrated_at"`
}

// rehydrateRequestBody mirrors agent.RehydrateRequestBody.
type rehydrateRequestBody struct {
	ArchivePath string `json:"archive_path,omitempty"`
}

// migrateRequestBody mirrors agent.MigrateRequestBody.
type migrateRequestBody struct {
	TargetAddr string `json:"target_addr"`
	AuthToken  string `json:"auth_token,omitempty"`
}

// execRequestBody is the JSON body of POST /sandbox/{id}/exec.
type execRequestBody struct {
	Command   string `json:"command"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
}

// snapshotRequestBody is the JSON body of POST /sandbox/{id}/snapshot.
type snapshotRequestBody struct {
	Name     string `json:"name"`
	DestPath string `json:"dest_path"`
}

// CreateSandbox issues POST /sandbox/create.
func (c *AgentClient) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (*CreateSandboxResponse, error) {
	out := &CreateSandboxResponse{}
	if err := c.do(ctx, http.MethodPost, "/sandbox/create", req, out); err != nil {
		return nil, fmt.Errorf("agent CreateSandbox: %w", err)
	}
	return out, nil
}

// GetSandbox issues GET /sandbox/{id} and returns a minimal view.
func (c *AgentClient) GetSandbox(ctx context.Context, id string) (*SandboxView, error) {
	out := &SandboxView{}
	if err := c.do(ctx, http.MethodGet, "/sandbox/"+id, nil, out); err != nil {
		return nil, fmt.Errorf("agent GetSandbox(%s): %w", id, err)
	}
	return out, nil
}

// StopSandbox issues POST /sandbox/{id}/stop. The agent returns 204; we
// don't decode a body.
func (c *AgentClient) StopSandbox(ctx context.Context, id string) error {
	if err := c.do(ctx, http.MethodPost, "/sandbox/"+id+"/stop", nil, nil); err != nil {
		return fmt.Errorf("agent StopSandbox(%s): %w", id, err)
	}
	return nil
}

// StartSandbox issues POST /sandbox/{id}/start.
func (c *AgentClient) StartSandbox(ctx context.Context, id string) error {
	if err := c.do(ctx, http.MethodPost, "/sandbox/"+id+"/start", nil, nil); err != nil {
		return fmt.Errorf("agent StartSandbox(%s): %w", id, err)
	}
	return nil
}

// DestroySandbox issues DELETE /sandbox/{id}.
func (c *AgentClient) DestroySandbox(ctx context.Context, id string) error {
	if err := c.do(ctx, http.MethodDelete, "/sandbox/"+id, nil, nil); err != nil {
		return fmt.Errorf("agent DestroySandbox(%s): %w", id, err)
	}
	return nil
}

// ExecCommand issues POST /sandbox/{id}/exec. The timeout is converted to
// milliseconds for the agent body but the surrounding ctx still bounds the
// HTTP call itself.
func (c *AgentClient) ExecCommand(ctx context.Context, id, cmd string, timeout time.Duration) (*ExecResult, error) {
	body := execRequestBody{Command: cmd, TimeoutMS: timeout.Milliseconds()}
	out := &ExecResult{}
	if err := c.do(ctx, http.MethodPost, "/sandbox/"+id+"/exec", body, out); err != nil {
		return nil, fmt.Errorf("agent ExecCommand(%s): %w", id, err)
	}
	return out, nil
}

// SnapshotSandbox issues POST /sandbox/{id}/snapshot.
//
// NOTE: agent endpoint pending; matches contract documented in master
// dispatcher.go. The agent team will add a /sandbox/{id}/snapshot handler
// that accepts {name, dest_path} and returns {snapshot_path, size_bytes}.
// Until that lands, calls here will 404.
func (c *AgentClient) SnapshotSandbox(ctx context.Context, id, name, destPath string) (*SnapshotResult, error) {
	body := snapshotRequestBody{Name: name, DestPath: destPath}
	out := &SnapshotResult{}
	if err := c.do(ctx, http.MethodPost, "/sandbox/"+id+"/snapshot", body, out); err != nil {
		return nil, fmt.Errorf("agent SnapshotSandbox(%s): %w", id, err)
	}
	return out, nil
}

// ListSandboxes issues GET /sandbox/list and returns all sandboxes the
// agent currently tracks. The reconciler uses this to detect drift
// between master's database and the agent's in-memory state. The agent
// shipped this endpoint as part of the networking-and-access milestone.
func (c *AgentClient) ListSandboxes(ctx context.Context) ([]AgentSandboxView, error) {
	var out []AgentSandboxView
	if err := c.do(ctx, http.MethodGet, "/sandbox/list", nil, &out); err != nil {
		return nil, fmt.Errorf("agent ListSandboxes: %w", err)
	}
	return out, nil
}

// ArchiveSandbox issues POST /sandbox/{id}/archive on the agent.
func (c *AgentClient) ArchiveSandbox(ctx context.Context, id string) (*ArchiveResult, error) {
	out := &ArchiveResult{}
	if err := c.do(ctx, http.MethodPost, "/sandbox/"+id+"/archive", nil, out); err != nil {
		return nil, fmt.Errorf("agent ArchiveSandbox(%s): %w", id, err)
	}
	return out, nil
}

// RehydrateSandbox issues POST /sandbox/{id}/rehydrate on the agent.
// archivePath may be empty to let the agent resolve from its configured
// store, or an "s3://..." / local path recorded at archive time.
func (c *AgentClient) RehydrateSandbox(ctx context.Context, id, archivePath string) error {
	body := rehydrateRequestBody{ArchivePath: archivePath}
	if err := c.do(ctx, http.MethodPost, "/sandbox/"+id+"/rehydrate", body, nil); err != nil {
		return fmt.Errorf("agent RehydrateSandbox(%s): %w", id, err)
	}
	return nil
}

// MigrateSandbox issues POST /sandbox/{id}/migrate on the source agent.
// targetAddr is the base URL of the destination agent ("http://10.0.1.5:9000").
func (c *AgentClient) MigrateSandbox(ctx context.Context, id, targetAddr, authToken string) (*MigrateResult, error) {
	body := migrateRequestBody{TargetAddr: targetAddr, AuthToken: authToken}
	out := &MigrateResult{}
	if err := c.do(ctx, http.MethodPost, "/sandbox/"+id+"/migrate", body, out); err != nil {
		return nil, fmt.Errorf("agent MigrateSandbox(%s): %w", id, err)
	}
	return out, nil
}

// httpError is returned by do when the agent responds with a non-2xx
// status. It captures the status code and a truncated body so callers can
// distinguish 4xx (don't retry) from 5xx (retry).
type httpError struct {
	Status int
	Body   string
}

// Error implements error.
func (e *httpError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("agent returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("agent returned HTTP %d: %s", e.Status, e.Body)
}

// retryable reports whether err is a transient failure worth retrying.
// 5xx and any non-HTTP transport error counts; 4xx and context-driven
// errors do not.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var he *httpError
	if errors.As(err, &he) {
		return he.Status >= 500
	}
	// Network/transport errors fall through to here — retry them.
	return true
}

// do executes one HTTP request with retry/backoff. body is JSON-encoded if
// non-nil; out is JSON-decoded from the response body if non-nil. Auth and
// content-type headers are always set. Backs off between attempts and
// honors ctx.Done().
func (c *AgentClient) do(ctx context.Context, method, path string, body any, out any) error {
	policy := c.retry
	if policy.MaxAttempts <= 0 {
		policy = DefaultRetryPolicy()
	}
	var lastErr error
	delay := policy.BaseDelay
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := c.doOnce(ctx, method, path, body, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable(err) || attempt == policy.MaxAttempts {
			return err
		}
		c.logger.Debug("agent request retry",
			"method", method, "path", path,
			"attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > policy.MaxDelay {
			delay = policy.MaxDelay
		}
	}
	return lastErr
}

// doOnce performs a single HTTP attempt — no retry. Split out so do can
// loop cleanly without nested defers leaking response bodies between
// attempts.
func (c *AgentClient) doOnce(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agent transport: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Cap body to keep error logs sensible; an agent shouldn't be
		// returning a megabyte of HTML, but defense-in-depth.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &httpError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if resp.StatusCode == http.StatusNoContent || out == nil {
		// Drain so the connection is reusable.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		// If the body is empty (e.g. an unexpected 200 with no body),
		// io.EOF is fine — only complain about parse failures.
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
