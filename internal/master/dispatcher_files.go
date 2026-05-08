// Package master — dispatcher_files.go adds the file ops to AgentClient.
// Master's handlers_files.go calls these to proxy file traffic from the
// SDK / dashboard down to the agent's HTTP-over-vsock endpoints.
package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// FileEntry mirrors the per-row shape the agent emits in its list
// response. Defined here (in addition to internal/agent) so the master
// package has zero compile-time dependency on agent.
type FileEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	IsDir   bool   `json:"is_dir"`
	ModTime string `json:"mod_time"`
}

// FileListResponse is the JSON body of GET /sandbox/{id}/files/list.
type FileListResponse struct {
	Entries []FileEntry `json:"entries"`
}

// UploadFile streams body to the agent's upload endpoint. size is the
// exact byte count expected by the guest; mismatching it leaves the
// guest waiting on a stalled stream.
func (c *AgentClient) UploadFile(ctx context.Context, sandboxID, path string, mode uint32, size int64, body io.Reader) error {
	if path == "" {
		return errors.New("path is required")
	}
	u := c.baseURL + "/sandbox/" + url.PathEscape(sandboxID) + "/files/upload"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return fmt.Errorf("build upload: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Vajra-Path", path)
	req.Header.Set("X-Vajra-Mode", strconv.FormatUint(uint64(mode), 10))
	req.ContentLength = size
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agent upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &httpError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	return nil
}

// DownloadFileResult is the response of DownloadFile. Body must be
// closed by the caller.
type DownloadFileResult struct {
	Size int64
	Mode uint32
	Body io.ReadCloser
}

// DownloadFile opens a streaming GET to the agent's download endpoint.
// The body is the file content; size + mode are echoed back via headers.
func (c *AgentClient) DownloadFile(ctx context.Context, sandboxID, path string) (*DownloadFileResult, error) {
	if path == "" {
		return nil, errors.New("path is required")
	}
	q := url.Values{}
	q.Set("path", path)
	u := c.baseURL + "/sandbox/" + url.PathEscape(sandboxID) + "/files/download?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build download: %w", err)
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent download: %w", err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, &httpError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	mode64, _ := strconv.ParseUint(resp.Header.Get("X-Vajra-Mode"), 10, 32)
	return &DownloadFileResult{
		Size: resp.ContentLength,
		Mode: uint32(mode64),
		Body: resp.Body,
	}, nil
}

// ListFiles issues GET /sandbox/{id}/files/list?dir=… and returns the
// decoded entries.
func (c *AgentClient) ListFiles(ctx context.Context, sandboxID, dir string) ([]FileEntry, error) {
	q := url.Values{}
	if dir != "" {
		q.Set("dir", dir)
	}
	u := c.baseURL + "/sandbox/" + url.PathEscape(sandboxID) + "/files/list"
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build list: %w", err)
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &httpError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	var out FileListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list: %w", err)
	}
	return out.Entries, nil
}
