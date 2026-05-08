package master

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestCreateShareReturnsToken creates a share and asserts the token has
// the expected shape (32 hex bytes = 64 chars) and the link URL is
// returned when port is set.
func TestCreateShareReturnsToken(t *testing.T) {
	h := newTestHarness(t)
	_, key, sandboxID := h.seedRunningSandbox(t)
	h.server.handlers.PublicBaseDomain = "vajra.dev"

	resp, body := h.req(t, "POST", "/v1/sandboxes/"+sandboxID+"/share", key,
		map[string]any{"port": 8080, "expires_in_seconds": 3600})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d body %s", resp.StatusCode, body)
	}
	var out createShareResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Token) != 64 {
		t.Errorf("token len = %d", len(out.Token))
	}
	if !strings.HasPrefix(out.URL, "https://8080-"+sandboxID+".vajra.dev/?token=") {
		t.Errorf("url = %q", out.URL)
	}
	if out.Port == nil || *out.Port != 8080 {
		t.Errorf("port = %v", out.Port)
	}
}

// TestValidateShareHappy plumbs a created token through the
// /internal/proxy/validate-share endpoint and confirms the 200 response.
func TestValidateShareHappy(t *testing.T) {
	h := newTestHarness(t)
	_, key, sandboxID := h.seedRunningSandbox(t)

	_, body := h.req(t, "POST", "/v1/sandboxes/"+sandboxID+"/share", key,
		map[string]any{"port": 8080})
	var out createShareResponse
	_ = json.Unmarshal(body, &out)

	q := url.Values{}
	q.Set("sandbox_id", sandboxID)
	q.Set("token", out.Token)
	q.Set("port", "8080")
	req, _ := http.NewRequest("GET", h.httpSrv.URL+"/internal/proxy/validate-share?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer internal-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// TestValidateShareWrongSandbox makes a token for sandbox A and tries
// to use it against sandbox B; expect 403.
func TestValidateShareWrongSandbox(t *testing.T) {
	h := newTestHarness(t)
	_, key, sandboxID := h.seedRunningSandbox(t)
	_, body := h.req(t, "POST", "/v1/sandboxes/"+sandboxID+"/share", key,
		map[string]any{})
	var out createShareResponse
	_ = json.Unmarshal(body, &out)

	q := url.Values{}
	q.Set("sandbox_id", "different-sandbox")
	q.Set("token", out.Token)
	req, _ := http.NewRequest("GET", h.httpSrv.URL+"/internal/proxy/validate-share?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer internal-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// TestValidateShareUnknownToken hits the validator with garbage; expect 404.
func TestValidateShareUnknownToken(t *testing.T) {
	h := newTestHarness(t)
	_, _, sandboxID := h.seedRunningSandbox(t)

	q := url.Values{}
	q.Set("sandbox_id", sandboxID)
	q.Set("token", "00000000000000000000000000000000")
	req, _ := http.NewRequest("GET", h.httpSrv.URL+"/internal/proxy/validate-share?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer internal-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// TestRevokeShare creates, revokes, then checks validate fails 410.
func TestRevokeShare(t *testing.T) {
	h := newTestHarness(t)
	_, key, sandboxID := h.seedRunningSandbox(t)

	_, body := h.req(t, "POST", "/v1/sandboxes/"+sandboxID+"/share", key,
		map[string]any{})
	var out createShareResponse
	_ = json.Unmarshal(body, &out)

	resp, _ := h.req(t, "DELETE", "/v1/sandboxes/"+sandboxID+"/share/"+out.ID, key, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d", resp.StatusCode)
	}

	q := url.Values{}
	q.Set("sandbox_id", sandboxID)
	q.Set("token", out.Token)
	req, _ := http.NewRequest("GET", h.httpSrv.URL+"/internal/proxy/validate-share?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer internal-secret")
	v, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	defer v.Body.Close()
	if v.StatusCode != http.StatusGone {
		t.Fatalf("validate status = %d", v.StatusCode)
	}
}

// TestProxyRouteHappy hits the internal proxy-route endpoint and gets
// back the agent base URL the proxy can use.
func TestProxyRouteHappy(t *testing.T) {
	h := newTestHarness(t)
	_, _, sandboxID := h.seedRunningSandbox(t)
	h.server.handlers.AgentSharedSecret = "agent-secret"

	q := url.Values{}
	q.Set("sandbox_id", sandboxID)
	req, _ := http.NewRequest("GET", h.httpSrv.URL+"/internal/proxy/route?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer internal-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out proxyRouteResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.SandboxID != sandboxID || out.State != "RUNNING" || out.AgentSecret != "agent-secret" {
		t.Fatalf("response mismatch: %+v", out)
	}
	if !strings.HasPrefix(out.AgentBaseURL, "http://127.0.0.1:") {
		t.Errorf("agent base URL = %q", out.AgentBaseURL)
	}
}

// TestListShares creates two shares and confirms list returns both.
func TestListShares(t *testing.T) {
	h := newTestHarness(t)
	_, key, sandboxID := h.seedRunningSandbox(t)
	_, _ = h.req(t, "POST", "/v1/sandboxes/"+sandboxID+"/share", key, map[string]any{})
	_, _ = h.req(t, "POST", "/v1/sandboxes/"+sandboxID+"/share", key, map[string]any{})
	resp, body := h.req(t, "GET", "/v1/sandboxes/"+sandboxID+"/shares", key, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out []map[string]any
	_ = json.Unmarshal(body, &out)
	if len(out) != 2 {
		t.Fatalf("len = %d", len(out))
	}
}
