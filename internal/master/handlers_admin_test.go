package master

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/allenabraham999/vajra/internal/models"
)

// registerAdmin registers an account through the API and flips its
// is_admin column directly in the store, so its API key passes
// requireAdmin.
func (h *testHarness) registerAdmin(t *testing.T, email, password string) (accountID, apiKey string) {
	t.Helper()
	accountID, apiKey = h.register(t, email, password)
	if err := h.store.Accounts().SetAdmin(context.Background(), accountID, true); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	return accountID, apiKey
}

// TestAdminEndpointsRequireAdmin verifies the /v1/admin/* surface is shut
// to anonymous callers (401) and to authenticated non-admins (403), and
// open to an account whose is_admin column is set (200).
func TestAdminEndpointsRequireAdmin(t *testing.T) {
	h := newTestHarness(t)
	_, userKey := h.register(t, "user@example.com", "supersecret")
	_, adminKey := h.registerAdmin(t, "admin@example.com", "supersecret")

	// Read endpoints (GET) and a write endpoint all share requireAdmin.
	endpoints := []struct {
		method, path string
	}{
		{"GET", "/v1/admin/cluster/overview"},
		{"GET", "/v1/admin/nodes"},
		{"GET", "/v1/admin/sandboxes"},
		{"GET", "/v1/admin/accounts"},
		{"GET", "/v1/admin/logs"},
		{"POST", "/v1/admin/nodes/some-node/drain"},
	}

	for _, ep := range endpoints {
		// No credentials → 401.
		resp, _ := h.req(t, ep.method, ep.path, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s anonymous: want 401, got %d", ep.method, ep.path, resp.StatusCode)
		}
		// Authenticated non-admin → 403.
		resp, body := h.req(t, ep.method, ep.path, userKey, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s non-admin: want 403, got %d body %s",
				ep.method, ep.path, resp.StatusCode, body)
		}
	}

	// An admin reaches the surface: the overview returns 200 with a body.
	resp, body := h.req(t, "GET", "/v1/admin/cluster/overview", adminKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin overview: want 200, got %d body %s", resp.StatusCode, body)
	}
	var ov adminOverview
	if err := json.Unmarshal(body, &ov); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if ov.TotalAccounts != 2 {
		t.Errorf("overview total_accounts: want 2, got %d", ov.TotalAccounts)
	}
}

// TestAdminDrainNode verifies the drain and cordon node actions persist
// the new lifecycle state, and that an unknown node id 404s.
func TestAdminDrainNode(t *testing.T) {
	h := newTestHarness(t)
	_, adminKey := h.registerAdmin(t, "admin@example.com", "supersecret")
	cluster := h.seedCluster(t)
	node := h.seedNode(t, cluster.ID)

	// Drain → DRAINING.
	resp, body := h.req(t, "POST", "/v1/admin/nodes/"+node.ID+"/drain", adminKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain: want 200, got %d body %s", resp.StatusCode, body)
	}
	got, err := h.store.Nodes().GetByID(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("reload node: %v", err)
	}
	if got.State != models.NodeStateDraining {
		t.Errorf("after drain: want state DRAINING, got %s", got.State)
	}

	// Cordon → CORDONED.
	resp, body = h.req(t, "POST", "/v1/admin/nodes/"+node.ID+"/cordon", adminKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cordon: want 200, got %d body %s", resp.StatusCode, body)
	}
	got, err = h.store.Nodes().GetByID(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("reload node: %v", err)
	}
	if got.State != models.NodeStateCordoned {
		t.Errorf("after cordon: want state CORDONED, got %s", got.State)
	}

	// Unknown node → 404.
	resp, _ = h.req(t, "POST", "/v1/admin/nodes/no-such-node/drain", adminKey, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("drain unknown node: want 404, got %d", resp.StatusCode)
	}
}

// TestAdminAddCredits verifies the manual credit-adjustment endpoint
// moves an account's prepaid balance and rejects bad input.
func TestAdminAddCredits(t *testing.T) {
	h := newTestHarness(t)
	_, adminKey := h.registerAdmin(t, "admin@example.com", "supersecret")
	targetID, _ := h.register(t, "target@example.com", "supersecret")

	before, err := h.store.Accounts().GetByID(context.Background(), targetID)
	if err != nil {
		t.Fatalf("load target: %v", err)
	}

	// Add $50.
	resp, body := h.req(t, "POST", "/v1/admin/accounts/"+targetID+"/credits", adminKey,
		map[string]float64{"amount": 50})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add credits: want 200, got %d body %s", resp.StatusCode, body)
	}
	var out struct {
		AccountID string  `json:"account_id"`
		Added     float64 `json:"added"`
		Credits   float64 `json:"credits"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Credits != before.CreditsRemaining+50 {
		t.Errorf("response credits: want %v, got %v", before.CreditsRemaining+50, out.Credits)
	}

	// The store reflects the new balance.
	after, err := h.store.Accounts().GetByID(context.Background(), targetID)
	if err != nil {
		t.Fatalf("reload target: %v", err)
	}
	if after.CreditsRemaining != before.CreditsRemaining+50 {
		t.Errorf("stored credits: want %v, got %v",
			before.CreditsRemaining+50, after.CreditsRemaining)
	}

	// Zero amount → 400.
	resp, _ = h.req(t, "POST", "/v1/admin/accounts/"+targetID+"/credits", adminKey,
		map[string]float64{"amount": 0})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("zero amount: want 400, got %d", resp.StatusCode)
	}

	// Unknown account → 404.
	resp, _ = h.req(t, "POST", "/v1/admin/accounts/no-such-account/credits", adminKey,
		map[string]float64{"amount": 25})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown account: want 404, got %d", resp.StatusCode)
	}
}
