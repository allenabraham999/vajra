package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultAPIURL is used when neither the --api-url flag nor the config
// file specifies a base URL. Matches the dev master listener.
const defaultAPIURL = "http://localhost:8080"

// httpTimeout is the per-request budget for control-plane calls. File
// transfers override this with a longer per-call timeout.
const httpTimeout = 60 * time.Second

// apiError carries the JSON error body the master returns:
//
//	{"error": "...", "status": "<code>"}
//
// Wrapping it as a typed error lets RunE handlers distinguish API
// failures from transport errors when needed.
type apiError struct {
	Status  int
	Message string
}

// Error implements the error interface.
func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("api error: status %d", e.Status)
	}
	return fmt.Sprintf("api error %d: %s", e.Status, e.Message)
}

// Client is the thin HTTP wrapper around vajra-master. One instance per
// CLI invocation; not safe for concurrent reuse with mutated baseURL.
type Client struct {
	baseURL string
	apiKey  string
	jwt     string
	http    *http.Client
}

// resolveClient merges --api-url/--api-key flags with the on-disk config
// and returns a ready-to-use Client. Falls back to defaultAPIURL when no
// URL is configured.
func resolveClient() (*Client, *Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	c := &Client{
		baseURL: firstNonEmpty(gFlags.apiURL, cfg.APIURL, defaultAPIURL),
		apiKey:  firstNonEmpty(gFlags.apiKey, cfg.APIKey),
		jwt:     cfg.JWT,
		http:    &http.Client{Timeout: httpTimeout},
	}
	c.baseURL = strings.TrimRight(c.baseURL, "/")
	return c, cfg, nil
}

// firstNonEmpty returns the first non-empty argument, or "" if all are.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// authHeader returns the Bearer token to send. Prefers the API key
// (long-lived, stored on disk) over the JWT (1h TTL).
func (c *Client) authHeader() string {
	if c.apiKey != "" {
		return "Bearer " + c.apiKey
	}
	if c.jwt != "" {
		return "Bearer " + c.jwt
	}
	return ""
}

// do issues a request with JSON encoding/decoding. body may be nil.
// out may be nil to discard the response body.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	return c.doRaw(ctx, method, path, nil, encodeJSON(body), out)
}

// encodeJSON returns a *bytes.Reader for the JSON-encoded body, or nil
// when body is nil. Failure to marshal panics — body shapes are typed
// at the call site, so a marshal error is a programmer bug.
func encodeJSON(body any) *bytes.Reader {
	if body == nil {
		return nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		panic(fmt.Errorf("encode body: %w", err))
	}
	return bytes.NewReader(b)
}

// doRaw is the lower-level transport. headers may be nil; body may be
// nil; out is JSON-decoded only when non-nil and the response is 2xx.
func (c *Client) doRaw(ctx context.Context, method, path string, headers map[string]string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h := c.authHeader(); h != "" {
		req.Header.Set("Authorization", h)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return parseAPIError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// parseAPIError reads the master's error body and returns a typed
// *apiError. Falls back to a generic message when the body isn't the
// expected shape.
func parseAPIError(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &body); err != nil || body.Error == "" {
		body.Error = strings.TrimSpace(string(raw))
	}
	return &apiError{Status: resp.StatusCode, Message: body.Error}
}

// streamGet issues a GET that returns a binary response (file download).
// Caller must close resp.Body.
func (c *Client) streamGet(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if h := c.authHeader(); h != "" {
		req.Header.Set("Authorization", h)
	}
	// Streaming downloads need a longer per-call budget than control-plane
	// requests. Use a dedicated client; the cli-level Timeout would cap
	// the entire transfer.
	httpc := &http.Client{Timeout: 0}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, parseAPIError(resp)
	}
	return resp, nil
}

// streamPut issues a PUT/POST with a streaming body and headers (for
// uploads). Returns nil on 2xx.
func (c *Client) streamPut(ctx context.Context, method, path string, headers map[string]string, contentLength int64, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", "application/octet-stream")
	if h := c.authHeader(); h != "" {
		req.Header.Set("Authorization", h)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	httpc := &http.Client{Timeout: 0}
	resp, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return parseAPIError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// requireAuth fails fast if neither API key nor JWT is available. Used
// at the top of every authed RunE so we don't fire a 401 against the
// server just to print the same message.
func requireAuth(c *Client) error {
	if c.authHeader() == "" {
		return errors.New("not logged in — run `vajra login` or set --api-key")
	}
	return nil
}

// withCtx returns a context with the standard CLI timeout.
func withCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), httpTimeout)
}
