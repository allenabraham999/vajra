package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestServer builds a *Server backed by the same in-test SandboxManager
// + fakeVMM used by sandbox_test.go.
func newTestServer(t *testing.T) (*Server, *SandboxManager, *fakeVMM, string) {
	t.Helper()
	mgr, vm, cacheDir := newTestManager(t)
	srv := NewServer(":0", mgr, nil, nil)
	return srv, mgr, vm, cacheDir
}

// rt drives one request through the server's mux without binding a port.
func rt(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.routes(mux)
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.middleware(mux).ServeHTTP(w, req)
	return w
}

func TestServerCreateAndGetSandbox(t *testing.T) {
	srv, mgr, _, cacheDir := newTestServer(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))

	w := rt(t, srv, "POST", "/sandbox/create", CreateRequestBody{
		TemplateHash: hash,
		Config:       SandboxConfig{VCPUs: 1, MemoryMB: 256},
	})
	// Async create: agent registers the placeholder synchronously and
	// returns 202 with state=CREATING. The CoW + restore work runs in a
	// goroutine; we poll the manager below to confirm the transition.
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var created Sandbox
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ID == "" || created.State != SandboxStateCreating {
		t.Fatalf("unexpected placeholder: %+v", created)
	}

	// GET should reach the placeholder immediately.
	w = rt(t, srv, "GET", "/sandbox/"+created.ID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from GET, got %d", w.Code)
	}

	// Wait for the async goroutine to land. This is a unit-test timeout,
	// not the production poller — keep it tight so a real regression
	// doesn't hide behind a long sleep.
	waitForState(t, mgr, created.ID, SandboxStateRunning, 2*time.Second)
}

// waitForState polls mgr.Get until the sandbox reaches want or the
// deadline passes. Failing fast surfaces async create regressions.
func waitForState(t *testing.T, mgr *SandboxManager, id string, want SandboxState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sb, err := mgr.Get(id)
		if err == nil && sb.State == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	sb, _ := mgr.Get(id)
	t.Fatalf("sandbox %s did not reach %s within %s; last=%+v", id, want, timeout, sb)
}

func TestServerStopStartDestroy(t *testing.T) {
	srv, mgr, _, cacheDir := newTestServer(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if w := rt(t, srv, "POST", "/sandbox/"+sb.ID+"/stop", nil); w.Code != http.StatusNoContent {
		t.Fatalf("stop status %d: %s", w.Code, w.Body.String())
	}
	if w := rt(t, srv, "POST", "/sandbox/"+sb.ID+"/start", nil); w.Code != http.StatusNoContent {
		t.Fatalf("start status %d: %s", w.Code, w.Body.String())
	}
	if w := rt(t, srv, "DELETE", "/sandbox/"+sb.ID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("destroy status %d: %s", w.Code, w.Body.String())
	}
}

func TestServerHealthAndMetrics(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	if w := rt(t, srv, "GET", "/health", nil); w.Code != http.StatusOK {
		t.Fatalf("expected health 200, got %d", w.Code)
	}
	w := rt(t, srv, "GET", "/metrics", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected metrics 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "vajra_agent_requests_total") {
		t.Fatalf("expected requests_total counter in metrics output")
	}
}

func TestServerCreateRejectsUnknownTemplate(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	w := rt(t, srv, "POST", "/sandbox/create", CreateRequestBody{TemplateHash: "nope"})
	if w.Code < 400 {
		t.Fatalf("expected error status, got %d", w.Code)
	}
}
