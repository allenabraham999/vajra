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
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
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
