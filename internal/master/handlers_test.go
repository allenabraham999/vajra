package master

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// testHarness wires a handlerStore + Server into an httptest.Server for
// end-to-end HTTP exercise. agentSrv stands in for a node agent so the
// dispatcher's outbound calls land on a real listener.
type testHarness struct {
	store     *handlerStore
	server    *Server
	httpSrv   *httptest.Server
	agentSrv  *httptest.Server
	agentCalls int
	agentBody []byte
	jwtSecret []byte

	// agentHandler, if non-nil, replaces the default agent stand-in for
	// a given test. Tests that need custom agent behaviour (e.g. dispatch
	// timeout while GET still works) install it before issuing requests.
	agentHandler func(w http.ResponseWriter, r *http.Request)
}

// newTestHarness builds a fully wired control plane for a single test.
func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	st := newHandlerStore()
	signer := NewJWTSigner([]byte("0123456789abcdef0123456789abcdef"))

	h := &testHarness{
		store:     st,
		jwtSecret: []byte("0123456789abcdef0123456789abcdef"),
	}

	// Stand-in agent: accept any POST with a 200, capturing the body.
	h.agentSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.agentCalls++
		body, _ := io.ReadAll(r.Body)
		h.agentBody = body
		if h.agentHandler != nil {
			h.agentHandler(w, r)
			return
		}
		switch {
		case r.URL.Path == "/sandbox/create":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":            "sb-test",
				"state":         "running",
				"template_hash": "deadbeef",
			})
		case strings.HasSuffix(r.URL.Path, "/exec"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"exit_code": 0, "stdout": "ok", "stderr": "",
			})
		case strings.HasSuffix(r.URL.Path, "/snapshot"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"snapshot_path": "/var/lib/vajra/snapshots/foo",
				"size_bytes":    1024,
			})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(h.agentSrv.Close)

	pool := newRoutedAgentPool(h.agentSrv.URL)
	scheduler := NewScheduler(st, nil)
	tracker := NewOperationTracker(st)
	handlers := NewHandlers(st, signer, scheduler, pool, tracker)
	handlers.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := NewServer(ServerConfig{
		Addr:           ":0",
		Logger:         handlers.Logger,
		InternalSecret: "internal-secret",
		AdminAccountID: "admin-account",
	}, handlers)
	h.server = srv
	h.httpSrv = httptest.NewServer(srv.Routes())
	t.Cleanup(h.httpSrv.Close)
	return h
}

// newRoutedAgentPool constructs an AgentPool routed at testURL. The
// trick: ClientFor builds wantBase from node.IP + DefaultAgentPort,
// so we'll set node.IP later from the parsed httptest URL. Until then
// the pool is a stock pool, just with the agent shared secret set.
func newRoutedAgentPool(testURL string) *AgentPool {
	_ = testURL // referenced for documentation; pool is stock here.
	return NewAgentPool("agent-secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// node returns a *models.Node configured to route through the test
// agent. seedNode stamps it into the store and pins the AgentPool so
// outbound dispatcher calls land on the httptest agent server.
func (h *testHarness) seedNode(t *testing.T, clusterID string) *models.Node {
	t.Helper()
	n := &models.Node{
		ID: "test-node", ClusterID: clusterID, Hostname: "test", IP: "127.0.0.1",
		State: models.NodeStateActive,
		Capacity: models.NodeCapacity{
			TotalCPU: 100, TotalMemoryMB: 1024 * 1024, TotalDiskGB: 10000,
		},
		LastHeartbeat: time.Now().UTC(),
	}
	if err := h.store.Nodes().Create(context.Background(), n); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	parsed, _ := url.Parse(h.agentSrv.URL)
	h.server.handlers.Pool.OverrideClient("test-node", &AgentClient{
		nodeID:  "test-node",
		baseURL: "http://" + parsed.Host,
		secret:  "agent-secret",
		http:    &http.Client{Timeout: 5 * time.Second},
		retry:   DefaultRetryPolicy(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return n
}

func (h *testHarness) seedCluster(t *testing.T) *models.Cluster {
	t.Helper()
	c := &models.Cluster{
		ID: "test-cluster", Name: "test", Region: "us-east",
		State: models.ClusterStateActive, CreatedAt: time.Now().UTC(),
	}
	if err := h.store.Clusters().Create(context.Background(), c); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
	return c
}

// req is a tiny convenience for issuing requests against the harness.
func (h *testHarness) req(t *testing.T, method, path, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	r, err := http.NewRequest(method, h.httpSrv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// register creates an account through the API and returns the issued
// raw API key plus the account ID.
func (h *testHarness) register(t *testing.T, email, password string) (accountID, apiKey string) {
	t.Helper()
	resp, body := h.req(t, "POST", "/v1/auth/register", "", map[string]string{
		"email": email, "password": password,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d body %s", resp.StatusCode, body)
	}
	var out registerResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	return out.AccountID, out.APIKey
}

func TestRegisterHappy(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")
	if accountID == "" || !strings.HasPrefix(key, APIKeyPrefix) {
		t.Fatalf("unexpected register response: id=%q key=%q", accountID, key)
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	h := newTestHarness(t)
	h.register(t, "dup@example.com", "supersecret")
	resp, _ := h.req(t, "POST", "/v1/auth/register", "", map[string]string{
		"email": "dup@example.com", "password": "supersecret",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
}

func TestRegisterBadInput(t *testing.T) {
	h := newTestHarness(t)
	resp, _ := h.req(t, "POST", "/v1/auth/register", "", map[string]string{
		"email": "bad", "password": "short",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	h := newTestHarness(t)
	h.register(t, "alice@example.com", "supersecret")
	resp, _ := h.req(t, "POST", "/v1/auth/login", "", map[string]string{
		"email": "alice@example.com", "password": "wrongwrong",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestLoginHappy(t *testing.T) {
	h := newTestHarness(t)
	h.register(t, "alice@example.com", "supersecret")
	resp, body := h.req(t, "POST", "/v1/auth/login", "", map[string]string{
		"email": "alice@example.com", "password": "supersecret",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body %s", resp.StatusCode, body)
	}
	var out loginResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if out.Token == "" || time.Until(out.ExpiresAt) <= 0 {
		t.Fatalf("bad login response: %+v", out)
	}
}

func TestListSandboxesUnauthed(t *testing.T) {
	h := newTestHarness(t)
	resp, _ := h.req(t, "GET", "/v1/sandboxes", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestListSandboxesAccountScoped(t *testing.T) {
	h := newTestHarness(t)
	_, aliceKey := h.register(t, "alice@example.com", "supersecret")
	bobID, _ := h.register(t, "bob@example.com", "supersecret")

	// Inject a sandbox that belongs to bob.
	bobSandbox := &models.Sandbox{
		ID: "sb-bob", Name: "bobs", AccountID: bobID,
		State: models.SandboxStateRunning, CreatedAt: time.Now().UTC(),
	}
	_ = h.store.Sandboxes().Create(context.Background(), bobSandbox)

	resp, body := h.req(t, "GET", "/v1/sandboxes", aliceKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got []*models.Sandbox
	_ = json.Unmarshal(body, &got)
	if len(got) != 0 {
		t.Fatalf("alice should see no sandboxes, got %d", len(got))
	}
}

// seedTemplate inserts a template owned by accountID and returns it.
func (h *testHarness) seedTemplate(t *testing.T, accountID string) *models.Template {
	t.Helper()
	tmpl := &models.Template{
		ID: "tmpl-1", AccountID: accountID, Name: "ubuntu",
		Version: "1.0", Hash: "deadbeef",
		RootfsPath: "/r", KernelPath: "/k", SnapshotPath: "/s",
		CreatedAt: time.Now().UTC(),
	}
	if err := h.store.Templates().Create(context.Background(), tmpl); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	return tmpl
}

func TestCreateSandboxHappy(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	resp, body := h.req(t, "POST", "/v1/sandboxes", key, map[string]any{
		"name": "demo", "source": "image", "template_id": "tmpl-1",
		"vcpus": 2, "memory_mb": 1024, "disk_gb": 10,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d body %s", resp.StatusCode, body)
	}
	if h.agentCalls == 0 {
		t.Fatalf("expected agent dispatch but agentCalls=0")
	}
	// Verify the dispatched body included the template hash.
	if !bytes.Contains(h.agentBody, []byte("deadbeef")) {
		t.Fatalf("agent body missing template hash: %s", h.agentBody)
	}
}

func TestCreateSandboxQuotaExceeded(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	// Override quota to 0 sandboxes so the very first request is rejected.
	h.server.handlers.Scheduler = NewScheduler(h.store, func(string) Quota {
		return Quota{MaxSandboxes: 0, MaxVCPUs: 0, MaxMemoryMB: 0}
	})
	resp, _ := h.req(t, "POST", "/v1/sandboxes", key, map[string]any{
		"name": "demo", "source": "image", "template_id": "tmpl-1",
		"vcpus": 2, "memory_mb": 1024, "disk_gb": 10,
	})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", resp.StatusCode)
	}
}

func TestCreateSandboxNoNode(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")
	h.seedCluster(t)
	h.seedTemplate(t, accountID)
	// Note: no node seeded.

	resp, _ := h.req(t, "POST", "/v1/sandboxes", key, map[string]any{
		"name": "demo", "source": "image", "template_id": "tmpl-1",
		"vcpus": 2, "memory_mb": 1024, "disk_gb": 10,
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}

// waitForSandboxState polls the in-memory store until the sandbox row
// reaches want or the deadline expires. Used to verify the async create
// poller actually drives the DB transition.
func waitForSandboxState(t *testing.T, h *testHarness, accountID, id string, want models.SandboxState, timeout time.Duration) *models.Sandbox {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *models.Sandbox
	for time.Now().Before(deadline) {
		sb, err := h.store.Sandboxes().GetByID(context.Background(), accountID, id)
		if err == nil {
			last = sb
			if sb.State == want {
				return sb
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("sandbox %s did not reach %s within %s; last=%+v", id, want, timeout, last)
	return nil
}

// TestCreateSandboxAsyncPollerDrivesToRunning exercises the async path
// end-to-end: agent returns 202 + CREATING, then GETs flip from CREATING
// to RUNNING. Master must return 201 quickly with CREATING, then the
// background poller must drive the DB row to RUNNING within a few
// seconds.
func TestCreateSandboxAsyncPollerDrivesToRunning(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	// First few GETs return CREATING; subsequent GETs return RUNNING.
	// The atomic counter makes the test deterministic regardless of how
	// many ticks the poller fires before flipping.
	var getCalls int32
	h.agentHandler = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox/create":
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    "sb-test", // ignored by master, which uses its own ID
				"state": "CREATING",
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sandbox/"):
			n := atomic.AddInt32(&getCalls, 1)
			state := "CREATING"
			if n >= 2 {
				state = "RUNNING"
			}
			id := strings.TrimPrefix(r.URL.Path, "/sandbox/")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    id,
				"state": state,
			})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}

	resp, body := h.req(t, "POST", "/v1/sandboxes", key, map[string]any{
		"name": "demo", "source": "image", "template_id": "tmpl-1",
		"vcpus": 2, "memory_mb": 1024, "disk_gb": 10,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d body %s", resp.StatusCode, body)
	}
	var got sandboxWithOp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Sandbox == nil || got.Sandbox.State != models.SandboxStateCreating {
		t.Fatalf("expected CREATING in response, got %+v", got.Sandbox)
	}

	// Poll the DB until the background poller flips the row to RUNNING.
	waitForSandboxState(t, h, accountID, got.Sandbox.ID, models.SandboxStateRunning, 5*time.Second)
}

// TestCreateSandboxDispatchFailUnknown covers the case the user reported
// in production: master's dispatch fails (timeout, network error) AND
// the agent doesn't actually have the sandbox. We must not leave the
// row in CREATING (the periodic reconciler would later rewrite it to
// DESTROYED anyway); persist DESTROYED immediately so the DB matches
// reality and return 502.
func TestCreateSandboxDispatchFailUnknown(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	h.agentHandler = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox/create":
			http.Error(w, "simulated upstream timeout", http.StatusInternalServerError)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sandbox/"):
			http.Error(w, "no such sandbox", http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}

	resp, _ := h.req(t, "POST", "/v1/sandboxes", key, map[string]any{
		"name": "demo", "source": "image", "template_id": "tmpl-1",
		"vcpus": 2, "memory_mb": 1024, "disk_gb": 10,
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
	// Locate the sandbox in the store — POST returned 502 with no body
	// id, but the row was created before dispatch and is account-scoped.
	rows, err := h.store.Sandboxes().ListByAccount(context.Background(), accountID, store.ListOpts{Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 sandbox row, got %d", len(rows))
	}
	if rows[0].State != models.SandboxStateDestroyed {
		t.Fatalf("DB state = %s, want DESTROYED", rows[0].State)
	}
}

// TestCreateSandboxReconcilesAfterDispatchFailure pins the second half
// of the regression: the agent finished CreateSandbox successfully but
// master's HTTP call timed out / 5xx'd. Master used to mark ERROR even
// though the agent reported RUNNING; now it must probe GetSandbox and
// adopt the agent's truth so the DB stays consistent with what's
// actually running on the host.
func TestCreateSandboxReconcilesAfterDispatchFailure(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	// Custom agent: POST /sandbox/create always 500s (dispatch fails
	// after retries), but GET /sandbox/{id} reports RUNNING — the
	// scenario where the agent succeeded, the response just didn't make
	// it back to master.
	h.agentHandler = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox/create":
			http.Error(w, "simulated upstream timeout", http.StatusInternalServerError)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sandbox/"):
			id := strings.TrimPrefix(r.URL.Path, "/sandbox/")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    id,
				"state": "RUNNING",
			})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}

	resp, body := h.req(t, "POST", "/v1/sandboxes", key, map[string]any{
		"name": "demo", "source": "image", "template_id": "tmpl-1",
		"vcpus": 2, "memory_mb": 1024, "disk_gb": 10,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201 after reconcile, got %d body %s", resp.StatusCode, body)
	}
	var got sandboxWithOp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Sandbox == nil || got.Sandbox.State != models.SandboxStateRunning {
		t.Fatalf("expected RUNNING in DB after reconcile, got %+v", got.Sandbox)
	}

	// And the persisted row must agree — confirms the UpdateState on
	// the reconcile branch actually fired.
	stored, err := h.store.Sandboxes().GetByID(context.Background(), accountID, got.Sandbox.ID)
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if stored.State != models.SandboxStateRunning {
		t.Fatalf("DB state = %s, want RUNNING", stored.State)
	}
}

func TestDeleteSandboxOtherAccount(t *testing.T) {
	h := newTestHarness(t)
	_, aliceKey := h.register(t, "alice@example.com", "supersecret")
	bobID, _ := h.register(t, "bob@example.com", "supersecret")

	// Bob's sandbox.
	nodeID := "test-node"
	bobSb := &models.Sandbox{
		ID: "sb-bob", Name: "bob", AccountID: bobID,
		NodeID: &nodeID, State: models.SandboxStateRunning,
	}
	_ = h.store.Sandboxes().Create(context.Background(), bobSb)

	resp, _ := h.req(t, "DELETE", "/v1/sandboxes/sb-bob", aliceKey, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 (account-scoped), got %d", resp.StatusCode)
	}
}

func TestGetSandboxNotFound(t *testing.T) {
	h := newTestHarness(t)
	_, aliceKey := h.register(t, "alice@example.com", "supersecret")
	resp, _ := h.req(t, "GET", "/v1/sandboxes/does-not-exist", aliceKey, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for missing sandbox, got %d", resp.StatusCode)
	}
}

func TestGetSandboxOtherAccount(t *testing.T) {
	h := newTestHarness(t)
	_, aliceKey := h.register(t, "alice@example.com", "supersecret")
	bobID, _ := h.register(t, "bob@example.com", "supersecret")
	nodeID := "test-node"
	bobSb := &models.Sandbox{
		ID: "sb-bob-get", Name: "bob", AccountID: bobID,
		NodeID: &nodeID, State: models.SandboxStateRunning,
	}
	_ = h.store.Sandboxes().Create(context.Background(), bobSb)

	resp, _ := h.req(t, "GET", "/v1/sandboxes/sb-bob-get", aliceKey, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 (account-scoped), got %d", resp.StatusCode)
	}
}

func TestHealthOK(t *testing.T) {
	h := newTestHarness(t)
	resp, body := h.req(t, "GET", "/health", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got healthResponse
	_ = json.Unmarshal(body, &got)
	if got.Status != "ok" || got.DB != "ok" {
		t.Fatalf("unexpected body: %+v", got)
	}
}

func TestInternalNodesRegisterUnauthed(t *testing.T) {
	h := newTestHarness(t)
	h.seedCluster(t)
	resp, _ := h.req(t, "POST", "/internal/nodes/register", "", map[string]any{
		"hostname": "host1", "ip": "10.0.0.1", "cluster_id": "test-cluster",
		"capacity": map[string]int{"total_cpu": 4, "total_memory_mb": 1024, "total_disk_gb": 100},
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestInternalNodesRegisterAuthed(t *testing.T) {
	h := newTestHarness(t)
	h.seedCluster(t)
	resp, body := h.req(t, "POST", "/internal/nodes/register", "internal-secret", map[string]any{
		"hostname": "host1", "ip": "10.0.0.1", "cluster_id": "test-cluster",
		"capacity": map[string]int{"total_cpu": 4, "total_memory_mb": 1024, "total_disk_gb": 100},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d body %s", resp.StatusCode, body)
	}
}

func TestInternalNodesHeartbeat(t *testing.T) {
	h := newTestHarness(t)
	c := h.seedCluster(t)
	h.seedNode(t, c.ID)

	hb := map[string]any{
		"node_id":   "test-node",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"usage": map[string]int{
			"used_cpu": 4, "used_memory_mb": 2048, "used_disk_gb": 5,
		},
		"sandbox_count": 2,
	}
	resp, _ := h.req(t, "POST", "/internal/nodes/test-node/heartbeat", "internal-secret", hb)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", resp.StatusCode)
	}
	got, err := h.store.Nodes().GetByID(context.Background(), "test-node")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.UsedResources.UsedCPU != 4 {
		t.Fatalf("usage not updated: %+v", got.UsedResources)
	}
}
