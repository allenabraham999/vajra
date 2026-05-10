package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// MasterClient is the agent's outbound HTTP client to vajra-master. The
// agent calls Register once on startup, Heartbeat every HeartbeatInterval,
// and NotifyUnhealthy whenever a sandbox stops responding.
type MasterClient struct {
	baseURL string
	nodeID  string
	apiKey  string
	http    *http.Client
	logger  *slog.Logger
}

// NewMasterClient builds a client targetting baseURL (e.g.
// "http://master:8080"). nodeID is included in every request so the master
// can attribute events without re-authenticating per call. Pass nil for
// logger to use slog.Default.
func NewMasterClient(baseURL, nodeID, apiKey string, logger *slog.Logger) *MasterClient {
	if logger == nil {
		logger = slog.Default()
	}
	return &MasterClient{
		baseURL: baseURL,
		nodeID:  nodeID,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 10 * time.Second},
		logger:  logger,
	}
}

// RegisterRequest is the body of POST /internal/nodes/register.
type RegisterRequest struct {
	NodeID    string `json:"node_id"`
	Hostname  string `json:"hostname"`
	IP        string `json:"ip"`
	ClusterID string `json:"cluster_id"`
	Capacity  struct {
		TotalCPU      int `json:"total_cpu"`
		TotalMemoryMB int `json:"total_memory_mb"`
		TotalDiskGB   int `json:"total_disk_gb"`
	} `json:"capacity"`
}

// HeartbeatRequest is the body of POST /internal/nodes/{id}/heartbeat.
type HeartbeatRequest struct {
	NodeID    string    `json:"node_id"`
	Timestamp time.Time `json:"timestamp"`
	Usage     struct {
		UsedCPU      int `json:"used_cpu"`
		UsedMemoryMB int `json:"used_memory_mb"`
		UsedDiskGB   int `json:"used_disk_gb"`
	} `json:"usage"`
	SandboxCount int    `json:"sandbox_count"`
	Version      string `json:"version,omitempty"`
}

// Register tells master that this agent is online. Master responds with a
// 200 once it has persisted the node row.
func (c *MasterClient) Register(ctx context.Context, req RegisterRequest) error {
	if c == nil || c.baseURL == "" {
		return nil
	}
	return c.postJSON(ctx, "/internal/nodes/register", req)
}

// Heartbeat updates master with current usage.
func (c *MasterClient) Heartbeat(ctx context.Context, req HeartbeatRequest) error {
	if c == nil || c.baseURL == "" {
		return nil
	}
	return c.postJSON(ctx, "/internal/nodes/"+c.nodeID+"/heartbeat", req)
}

// NotifyUnhealthy implements MasterNotifier. Master is expected to mark
// the sandbox as ERROR (or whatever it does); we don't block on the call.
func (c *MasterClient) NotifyUnhealthy(ctx context.Context, sandboxID, reason string) {
	if c == nil || c.baseURL == "" {
		return
	}
	body := map[string]string{
		"node_id":    c.nodeID,
		"sandbox_id": sandboxID,
		"reason":     reason,
	}
	if err := c.postJSON(ctx, "/internal/sandboxes/"+sandboxID+"/unhealthy", body); err != nil {
		c.logger.Warn("notify unhealthy failed", "id", sandboxID, "err", err)
	}
}

func (c *MasterClient) postJSON(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("master %s: HTTP %d: %s", path, resp.StatusCode, bytes.TrimSpace(body))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
