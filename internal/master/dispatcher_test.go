package master

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

// newTestClient returns an AgentClient pointed at srv with a tight retry
// policy so tests don't waste seconds on backoff.
func newTestClient(t *testing.T, srv *httptest.Server, secret string) *AgentClient {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse srv url: %v", err)
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse srv port: %v", err)
	}
	c := NewAgentClient(&models.Node{ID: "node-test", IP: host}, secret, nil)
	// Override baseURL/port to point at the test server (httptest picks a
	// random port; DefaultAgentPort wouldn't match).
	c.baseURL = fmt.Sprintf("http://%s:%d", host, port)
	c.retry = RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
	return c
}

func TestAgentClientCreateSandboxHappy(t *testing.T) {
	wantBody := CreateSandboxResponse{ID: "sb-1", State: "RUNNING"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("got method %s, want POST", r.Method)
		}
		if r.URL.Path != "/sandbox/create" {
			t.Errorf("got path %s, want /sandbox/create", r.URL.Path)
		}
		var got CreateSandboxRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID != "sb-1" || got.TemplateHash != "abc" || got.Config.VCPUs != 2 {
			t.Errorf("unexpected request: %+v", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(wantBody)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "secret")
	got, err := c.CreateSandbox(context.Background(), CreateSandboxRequest{
		ID:           "sb-1",
		TemplateHash: "abc",
		Config:       SandboxConfig{VCPUs: 2, MemoryMB: 1024, DiskGB: 4},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if got.ID != wantBody.ID || got.State != wantBody.State {
		t.Errorf("got %+v, want %+v", got, wantBody)
	}
}

func TestAgentClientRetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(CreateSandboxResponse{ID: "sb-2", State: "RUNNING"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "")
	got, err := c.CreateSandbox(context.Background(), CreateSandboxRequest{ID: "sb-2"})
	if err != nil {
		t.Fatalf("CreateSandbox after retry: %v", err)
	}
	if got.ID != "sb-2" {
		t.Errorf("got id %q, want sb-2", got.ID)
	}
	if calls.Load() != 2 {
		t.Errorf("got %d calls, want 2 (one 502, one success)", calls.Load())
	}
}

func TestAgentClientNoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "")
	_, err := c.CreateSandbox(context.Background(), CreateSandboxRequest{ID: "sb-3"})
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if calls.Load() != 1 {
		t.Errorf("got %d calls, want 1 (no retry on 4xx)", calls.Load())
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err %v should mention status 400", err)
	}
}

func TestAgentClientAuthorizationHeader(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type %q, want application/json", ct)
		}
		switch r.URL.Path {
		case "/sandbox/create":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(CreateSandboxResponse{ID: "x", State: "RUNNING"})
		case "/sandbox/x":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(SandboxView{ID: "x", State: "RUNNING"})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "topsecret")
	if _, err := c.CreateSandbox(context.Background(), CreateSandboxRequest{ID: "x"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.GetSandbox(context.Background(), "x"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := c.DestroySandbox(context.Background(), "x"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	for i, h := range seen {
		if h != "Bearer topsecret" {
			t.Errorf("call %d: Authorization %q, want Bearer topsecret", i, h)
		}
	}
}

func TestAgentClientLifecycleMethods(t *testing.T) {
	type call struct {
		method string
		path   string
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, call{r.Method, r.URL.Path})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "")
	if err := c.StopSandbox(context.Background(), "abc"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := c.StartSandbox(context.Background(), "abc"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.DestroySandbox(context.Background(), "abc"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	want := []call{
		{http.MethodPost, "/sandbox/abc/stop"},
		{http.MethodPost, "/sandbox/abc/start"},
		{http.MethodDelete, "/sandbox/abc"},
	}
	if len(calls) != len(want) {
		t.Fatalf("got %d calls, want %d (%v)", len(calls), len(want), calls)
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("call %d: got %v, want %v", i, calls[i], w)
		}
	}
}

func TestAgentClientExecCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandbox/sb-x/exec" {
			t.Errorf("got path %s", r.URL.Path)
		}
		var got struct {
			Command   string `json:"command"`
			TimeoutMS int64  `json:"timeout_ms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Command != "echo hi" {
			t.Errorf("got cmd %q", got.Command)
		}
		if got.TimeoutMS != 1500 {
			t.Errorf("got timeout %d", got.TimeoutMS)
		}
		_ = json.NewEncoder(w).Encode(ExecResult{ExitCode: 0, Stdout: "hi\n", Stderr: ""})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "")
	res, err := c.ExecCommand(context.Background(), "sb-x", "echo hi", 1500*time.Millisecond)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "hi\n" {
		t.Errorf("got %+v", res)
	}
}

func TestAgentClientContextCancellationAbortsRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "")
	// Make backoff long enough that ctx must time out before MaxAttempts.
	c.retry = RetryPolicy{MaxAttempts: 5, BaseDelay: 200 * time.Millisecond, MaxDelay: 200 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.CreateSandbox(ctx, CreateSandboxRequest{ID: "doomed"})
	if err == nil {
		t.Fatal("expected context error")
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Errorf("expected fast cancellation, took %v", elapsed)
	}
	if !strings.Contains(err.Error(), "context") &&
		!strings.Contains(err.Error(), "deadline") &&
		!strings.Contains(err.Error(), "canceled") {
		t.Errorf("expected context error, got %v", err)
	}
}

func TestAgentClientGetSandbox(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("got method %s", r.Method)
		}
		if r.URL.Path != "/sandbox/abc" {
			t.Errorf("got path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(SandboxView{ID: "abc", State: "RUNNING"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "")
	got, err := c.GetSandbox(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "abc" || got.State != "RUNNING" {
		t.Errorf("got %+v", got)
	}
}

func TestAgentClientSnapshotPendingContract(t *testing.T) {
	// Verifies the snapshot dispatcher's wire contract even though the
	// agent endpoint isn't live yet — once the agent ships, this test
	// will exercise the real path unchanged.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandbox/sb-1/snapshot" {
			t.Errorf("got path %s", r.URL.Path)
		}
		var got snapshotRequestBody
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Name != "snap" || got.DestPath != "/tmp/snap" {
			t.Errorf("got %+v", got)
		}
		_ = json.NewEncoder(w).Encode(SnapshotResult{
			SnapshotPath: "/tmp/snap/snap.bin",
			SizeBytes:    4096,
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "")
	got, err := c.SnapshotSandbox(context.Background(), "sb-1", "snap", "/tmp/snap")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got.SnapshotPath != "/tmp/snap/snap.bin" || got.SizeBytes != 4096 {
		t.Errorf("got %+v", got)
	}
}

func TestAgentClientListSandboxes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/sandbox/list" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]AgentSandboxView{
			{ID: "a", State: "RUNNING"},
			{ID: "b", State: "STOPPED"},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "")
	got, err := c.ListSandboxes(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].State != "STOPPED" {
		t.Errorf("got %+v", got)
	}
}

func TestAgentPoolClientForCachesAndReplaces(t *testing.T) {
	pool := NewAgentPool("s", nil)
	n := &models.Node{ID: "n1", IP: "10.0.0.1"}
	c1 := pool.ClientFor(n)
	c2 := pool.ClientFor(n)
	if c1 != c2 {
		t.Error("expected cached client for same node")
	}
	// Same ID, new IP — should rebuild.
	n2 := &models.Node{ID: "n1", IP: "10.0.0.2"}
	c3 := pool.ClientFor(n2)
	if c3 == c1 {
		t.Error("expected new client when IP changes")
	}
	if c3.BaseURL() != fmt.Sprintf("http://10.0.0.2:%d", DefaultAgentPort) {
		t.Errorf("baseURL = %s", c3.BaseURL())
	}
}

