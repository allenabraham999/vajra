package master

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

// _ keeps the store import alive for any test that may need to reference
// store.ErrNotFound without forcing a separate import-shuffle.
var _ = struct{}{}

// --- Feature 1: Custom Images (Dockerfile → Template) -------------------

// TestBuildTemplateJSONHappy enqueues a build via JSON, runs the manager
// synchronously, and asserts the resulting template was created with the
// hash the runner returned.
func TestBuildTemplateJSONHappy(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")

	// Install a deterministic Builder that runs synchronously so the
	// test doesn't race the goroutine.
	h.server.handlers.Builder = NewBuildManager(h.store, NewHashBuildRunner(), nil)

	resp, body := h.req(t, "POST", "/v1/templates/build", key, map[string]any{
		"name":       "my-env",
		"version":    "1.0.0",
		"dockerfile": "FROM ubuntu:24.04\nRUN echo hi\n",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d body %s", resp.StatusCode, body)
	}
	var got struct {
		BuildID         string `json:"build_id"`
		Status          string `json:"status"`
		TemplateName    string `json:"template_name"`
		TemplateVersion string `json:"template_version"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.BuildID == "" || got.Status != string(models.BuildStatusPending) {
		t.Fatalf("unexpected response: %+v", got)
	}

	// Drive the build inline so we can assert the terminal state.
	b, err := h.store.Builds().GetByID(context.Background(), accountID, got.BuildID)
	if err != nil {
		t.Fatalf("load build: %v", err)
	}
	h.server.handlers.Builder.RunSync(context.Background(), b)

	pollResp, pollBody := h.req(t, "GET", "/v1/templates/builds/"+got.BuildID, key, nil)
	if pollResp.StatusCode != http.StatusOK {
		t.Fatalf("poll want 200, got %d body %s", pollResp.StatusCode, pollBody)
	}
	var finished models.Build
	_ = json.Unmarshal(pollBody, &finished)
	if finished.Status != models.BuildStatusCompleted {
		t.Fatalf("status = %s, want COMPLETED (err=%v)", finished.Status, finished.Error)
	}
	if finished.TemplateID == nil || *finished.TemplateID == "" {
		t.Fatalf("expected template_id to be set")
	}
	tmpl, err := h.store.Templates().GetByID(context.Background(), accountID, *finished.TemplateID)
	if err != nil {
		t.Fatalf("template not registered: %v", err)
	}
	if tmpl.Name != "my-env" || tmpl.Version != "1.0.0" {
		t.Fatalf("template name/version mismatch: %+v", tmpl)
	}
}

// TestBuildTemplateRejectsBadInput covers the malformed-name path.
func TestBuildTemplateRejectsBadInput(t *testing.T) {
	h := newTestHarness(t)
	_, key := h.register(t, "alice@example.com", "supersecret")
	h.server.handlers.Builder = NewBuildManager(h.store, NewHashBuildRunner(), nil)

	resp, _ := h.req(t, "POST", "/v1/templates/build", key, map[string]any{
		"name":       "Bad Name With Spaces",
		"version":    "1.0.0",
		"dockerfile": "FROM scratch\n",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

// TestBuildTemplateNoBuilderConfigured asserts the 503 when the build
// manager isn't wired in (e.g. master started without it).
func TestBuildTemplateNoBuilderConfigured(t *testing.T) {
	h := newTestHarness(t)
	_, key := h.register(t, "alice@example.com", "supersecret")
	h.server.handlers.Builder = nil

	resp, _ := h.req(t, "POST", "/v1/templates/build", key, map[string]any{
		"name":       "ok",
		"version":    "1.0.0",
		"dockerfile": "FROM scratch\n",
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}

// --- Feature 2: Auto-stop / Auto-archive --------------------------------

// TestLifecycleAutoStopRunningSandbox seeds a RUNNING sandbox whose
// last_activity is well past auto_stop_minutes and asserts the sweep
// transitions it to STOPPED and dispatches to the agent.
func TestLifecycleAutoStopRunningSandbox(t *testing.T) {
	h := newTestHarness(t)
	accountID, _ := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	node := h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	// Count the dispatched stops so we can assert agent invocation.
	var stops int32
	h.agentHandler = func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stop") {
			atomic.AddInt32(&stops, 1)
		}
		w.WriteHeader(http.StatusNoContent)
	}

	now := time.Now().UTC()
	sb := &models.Sandbox{
		ID: "sb-idle", Name: "idle", AccountID: accountID,
		NodeID: &node.ID, ClusterID: &c.ID, TemplateID: "tmpl-1",
		State:           models.SandboxStateRunning,
		Config:          models.SandboxConfig{VCPUs: 1, MemoryMB: 512, DiskGB: 1},
		AutoStopMinutes: 15,
		LastActivity:    now.Add(-30 * time.Minute),
		CreatedAt:       now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	if err := h.store.Sandboxes().Create(context.Background(), sb); err != nil {
		t.Fatalf("seed: %v", err)
	}

	lm := NewLifecycleManager(h.store, h.server.handlers.Pool, nil, h.server.handlers, nil)
	lm.Sweep(context.Background())

	got, _ := h.store.Sandboxes().GetByID(context.Background(), accountID, "sb-idle")
	if got.State != models.SandboxStateStopped {
		t.Fatalf("state = %s, want STOPPED", got.State)
	}
	if atomic.LoadInt32(&stops) == 0 {
		t.Fatalf("expected agent stop dispatch")
	}
}

// TestLifecycleAutoArchiveStoppedSandbox covers the second leg: a
// STOPPED sandbox idle past auto_archive_minutes should land in
// ARCHIVED.
func TestLifecycleAutoArchiveStoppedSandbox(t *testing.T) {
	h := newTestHarness(t)
	accountID, _ := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	node := h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	now := time.Now().UTC()
	sb := &models.Sandbox{
		ID: "sb-old", Name: "old", AccountID: accountID,
		NodeID: &node.ID, ClusterID: &c.ID, TemplateID: "tmpl-1",
		State:              models.SandboxStateStopped,
		Config:             models.SandboxConfig{VCPUs: 1, MemoryMB: 512, DiskGB: 1},
		AutoArchiveMinutes: 60,
		LastActivity:       now.Add(-2 * time.Hour),
		CreatedAt:          now.Add(-3 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
	}
	if err := h.store.Sandboxes().Create(context.Background(), sb); err != nil {
		t.Fatalf("seed: %v", err)
	}

	lm := NewLifecycleManager(h.store, h.server.handlers.Pool, nil, h.server.handlers, nil)
	lm.Sweep(context.Background())

	got, _ := h.store.Sandboxes().GetByID(context.Background(), accountID, "sb-old")
	if got.State != models.SandboxStateArchived {
		t.Fatalf("state = %s, want ARCHIVED", got.State)
	}
}

// TestLifecycleDoesNotTouchActiveSandbox confirms a recently-active
// sandbox is left alone.
func TestLifecycleDoesNotTouchActiveSandbox(t *testing.T) {
	h := newTestHarness(t)
	accountID, _ := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	node := h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	now := time.Now().UTC()
	sb := &models.Sandbox{
		ID: "sb-busy", Name: "busy", AccountID: accountID,
		NodeID: &node.ID, ClusterID: &c.ID, TemplateID: "tmpl-1",
		State:           models.SandboxStateRunning,
		Config:          models.SandboxConfig{VCPUs: 1, MemoryMB: 512, DiskGB: 1},
		AutoStopMinutes: 15,
		LastActivity:    now.Add(-time.Minute),
		CreatedAt:       now.Add(-time.Hour), UpdatedAt: now,
	}
	if err := h.store.Sandboxes().Create(context.Background(), sb); err != nil {
		t.Fatalf("seed: %v", err)
	}

	lm := NewLifecycleManager(h.store, h.server.handlers.Pool, nil, h.server.handlers, nil)
	lm.Sweep(context.Background())

	got, _ := h.store.Sandboxes().GetByID(context.Background(), accountID, "sb-busy")
	if got.State != models.SandboxStateRunning {
		t.Fatalf("state = %s, want RUNNING (busy)", got.State)
	}
}

// TestCreateSandboxRespectsAutoStopOverride pins the wire contract:
// passing auto_stop_minutes / auto_archive_minutes in the request body
// persists those values on the sandbox row.
func TestCreateSandboxRespectsAutoStopOverride(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	resp, body := h.req(t, "POST", "/v1/sandboxes", key, map[string]any{
		"name": "demo", "source": "image", "template_id": "tmpl-1",
		"vcpus": 2, "memory_mb": 1024, "disk_gb": 10,
		"auto_stop_minutes":    5,
		"auto_archive_minutes": 60,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d body %s", resp.StatusCode, body)
	}
	var got struct {
		ID                 string `json:"id"`
		AutoStopMinutes    int    `json:"auto_stop_minutes"`
		AutoArchiveMinutes int    `json:"auto_archive_minutes"`
	}
	_ = json.Unmarshal(body, &got)
	if got.AutoStopMinutes != 5 || got.AutoArchiveMinutes != 60 {
		t.Fatalf("policies not persisted: %+v", got)
	}
}

// --- Feature 3: Webhooks ------------------------------------------------

// TestWebhookCreateAndList covers the round trip including the secret-
// returned-once contract.
func TestWebhookCreateAndList(t *testing.T) {
	h := newTestHarness(t)
	_, key := h.register(t, "alice@example.com", "supersecret")

	resp, body := h.req(t, "POST", "/v1/webhooks", key, map[string]any{
		"url":    "https://example.invalid/hook",
		"events": []string{"sandbox.created", "sandbox.running"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d body %s", resp.StatusCode, body)
	}
	var created models.Webhook
	_ = json.Unmarshal(body, &created)
	if created.ID == "" || created.Secret == "" {
		t.Fatalf("secret should be present on create: %+v", created)
	}

	resp, body = h.req(t, "GET", "/v1/webhooks", key, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list want 200, got %d", resp.StatusCode)
	}
	var list []models.Webhook
	_ = json.Unmarshal(body, &list)
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list mismatch: %+v", list)
	}
	if list[0].Secret != "" {
		t.Fatalf("secret should be redacted on list, got %q", list[0].Secret)
	}
}

// TestWebhookDispatchSignsBodyAndRetries spins a fake receiver that
// rejects the first attempt and accepts the second; the manager must
// retry once and the receiver must observe a valid HMAC signature on
// the accepted attempt.
func TestWebhookDispatchSignsBodyAndRetries(t *testing.T) {
	h := newTestHarness(t)
	accountID, _ := h.register(t, "alice@example.com", "supersecret")

	var attempts int32
	var lastSig, lastEvent string
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		body, _ := io.ReadAll(r.Body)
		if n == 1 {
			http.Error(w, "transient", http.StatusBadGateway)
			return
		}
		lastBody = body
		lastSig = r.Header.Get(SignatureHeader)
		lastEvent = r.Header.Get(EventHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	// Persist the webhook directly so we control the secret.
	hook := &models.Webhook{
		ID: "wh-1", AccountID: accountID, URL: srv.URL, Secret: "topsecret",
		Events: models.WebhookEvents{"sandbox.running"},
		Active: true, CreatedAt: time.Now().UTC(),
	}
	if err := h.store.Webhooks().Create(context.Background(), hook); err != nil {
		t.Fatalf("seed webhook: %v", err)
	}

	wm := NewWebhookManager(h.store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithBackoff([]time.Duration{1 * time.Millisecond})
	sent, err := wm.DispatchSync(context.Background(), accountID, "sandbox.running", map[string]string{"id": "sb-x"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if sent != 1 {
		t.Fatalf("sent=%d, want 1", sent)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("attempts=%d, want 2 (one retry after 502)", attempts)
	}
	if lastEvent != "sandbox.running" {
		t.Fatalf("event header missing: %q", lastEvent)
	}
	if !strings.HasPrefix(lastSig, "sha256=") {
		t.Fatalf("signature header malformed: %q", lastSig)
	}
	sig := strings.TrimPrefix(lastSig, "sha256=")
	if !VerifySignature("topsecret", lastBody, sig) {
		t.Fatalf("signature did not verify")
	}
}

// TestWebhookTestEndpoint fires a synthetic delivery and asserts the
// `delivered` flag.
func TestWebhookTestEndpoint(t *testing.T) {
	h := newTestHarness(t)
	_, key := h.register(t, "alice@example.com", "supersecret")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	resp, body := h.req(t, "POST", "/v1/webhooks", key, map[string]any{
		"url":    srv.URL,
		"events": []string{"sandbox.created"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create want 201, got %d body %s", resp.StatusCode, body)
	}
	var created models.Webhook
	_ = json.Unmarshal(body, &created)

	// Install the dispatcher with a tight backoff so the test is fast.
	h.server.handlers.Webhooks = NewWebhookManager(h.store, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithBackoff([]time.Duration{1 * time.Millisecond})

	resp, body = h.req(t, "POST", "/v1/webhooks/"+created.ID+"/test", key, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test fire want 200, got %d body %s", resp.StatusCode, body)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if got["delivered"] != true {
		t.Fatalf("delivered=%v, want true; body=%s", got["delivered"], body)
	}
}

// TestWebhookDispatchUnknownEventNoOp confirms the dispatch site is
// tolerant of unknown event names.
func TestWebhookDispatchUnknownEventNoOp(t *testing.T) {
	st := newHandlerStore()
	wm := NewWebhookManager(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n := wm.Dispatch(context.Background(), "acct", "not.an.event", nil); n != 0 {
		t.Fatalf("expected 0 deliveries, got %d", n)
	}
}

// TestWebhookCreateRejectsBadEvent ensures validation on the create path.
func TestWebhookCreateRejectsBadEvent(t *testing.T) {
	h := newTestHarness(t)
	_, key := h.register(t, "alice@example.com", "supersecret")
	resp, _ := h.req(t, "POST", "/v1/webhooks", key, map[string]any{
		"url": "https://example.invalid/", "events": []string{"foo.bar"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

// --- Feature 4: OpenAPI Docs --------------------------------------------

// TestDocsSwaggerUIServed checks GET /v1/docs returns the HTML page
// without requiring auth.
func TestDocsSwaggerUIServed(t *testing.T) {
	h := newTestHarness(t)
	resp, body := h.req(t, "GET", "/v1/docs", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("swagger-ui")) {
		t.Fatalf("expected Swagger UI markup in body, got: %s", body)
	}
}

// TestDocsOpenAPISpecServed asserts the YAML body is returned. We point
// OpenAPIPath at the repo-relative location so the test runs from any
// CWD inside the module.
func TestDocsOpenAPISpecServed(t *testing.T) {
	prev := OpenAPIPath
	OpenAPIPath = mustResolveOpenAPIPath(t)
	resetDocsCache()
	t.Cleanup(func() {
		OpenAPIPath = prev
		resetDocsCache()
	})

	h := newTestHarness(t)
	resp, body := h.req(t, "GET", "/v1/docs/openapi.yaml", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d body %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("openapi: 3.0")) {
		t.Fatalf("expected OpenAPI 3.0 header, got: %s", body[:200])
	}
}

// mustResolveOpenAPIPath finds docs/openapi.yaml by walking up from the
// test's CWD. Returns the absolute path or fails the test. Since the
// test runs from internal/master/, the spec is two levels up.
func mustResolveOpenAPIPath(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for dir := cwd; dir != "/"; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "docs", "openapi.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Fatalf("docs/openapi.yaml not found from %s", cwd)
	return ""
}
