package master

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// newBillingHandlers builds a Handlers wired to an in-memory store with a
// discard logger — enough to exercise the billing endpoints directly.
func newBillingHandlers(hs *handlerStore) *Handlers {
	h := NewHandlers(hs, NewJWTSigner([]byte("0123456789abcdef0123456789abcdef")), nil, nil, nil)
	h.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return h
}

// fakeStripeGateway is a StripeGateway that records calls and returns
// canned results, so the billing handlers can be tested without Stripe.
type fakeStripeGateway struct {
	checkoutResult CheckoutResult
	checkoutErr    error
	checkoutCalls  int
	lastAccountID  string
	lastAmountUSD  float64

	webhookEvent StripeWebhookEvent
	webhookErr   error
}

func (f *fakeStripeGateway) CreateCheckoutSession(_ context.Context, accountID string, amountUSD float64) (CheckoutResult, error) {
	f.checkoutCalls++
	f.lastAccountID = accountID
	f.lastAmountUSD = amountUSD
	if f.checkoutErr != nil {
		return CheckoutResult{}, f.checkoutErr
	}
	return f.checkoutResult, nil
}

func (f *fakeStripeGateway) VerifyWebhook(_ []byte, _ string) (StripeWebhookEvent, error) {
	if f.webhookErr != nil {
		return StripeWebhookEvent{}, f.webhookErr
	}
	return f.webhookEvent, nil
}

// TestBillingMeterDeductsCredits: a running sandbox plus one meter tick
// must lower the account balance and record the slice of usage.
func TestBillingMeterDeductsCredits(t *testing.T) {
	ctx := context.Background()
	hs := newHandlerStore()
	if err := hs.Accounts().Create(ctx, &models.Account{ID: "acc-1", Email: "a@b.c", CreditsRemaining: 100}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := hs.Sandboxes().Create(ctx, &models.Sandbox{
		ID: "sb-1", AccountID: "acc-1", State: models.SandboxStateRunning,
		Config: models.SandboxConfig{VCPUs: 1, MemoryMB: 512},
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	meter := NewBillingMeter(hs, 10*time.Second, 0.06, 0.01)
	if err := meter.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	got, err := hs.Accounts().GetByID(ctx, "acc-1")
	if err != nil {
		t.Fatalf("reload account: %v", err)
	}
	delta := 100 - got.CreditsRemaining
	if delta <= 0 {
		t.Fatalf("credits not decremented: balance still %v", got.CreditsRemaining)
	}
	// One 10s tick of 1 vCPU + 0.5 GB is a fraction of a cent; bound it so
	// a future rate bug that 100x's the charge is caught.
	if delta > 0.01 {
		t.Fatalf("charge unexpectedly large: $%.6f for one 10s tick", delta)
	}

	daily, err := hs.Usage().DailySummary(ctx, "acc-1",
		time.Now().Add(-48*time.Hour), time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("daily summary: %v", err)
	}
	if len(daily) != 1 || daily[0].CostUSD <= 0 || daily[0].VCPUHours <= 0 {
		t.Fatalf("usage not accumulated into rollup: %+v", daily)
	}
}

// TestBillingMeterStopsOnZero: an account already at zero may go slightly
// negative as a running sandbox keeps billing, but the balance must never
// sink past the -$5 overdraft floor.
func TestBillingMeterStopsOnZero(t *testing.T) {
	ctx := context.Background()
	hs := newHandlerStore()
	if err := hs.Accounts().Create(ctx, &models.Account{ID: "acc-0", Email: "z@b.c", CreditsRemaining: 0}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	// An oversized sandbox so a single tick costs far more than the floor:
	// the clamp must engage rather than letting the balance free-fall.
	if err := hs.Sandboxes().Create(ctx, &models.Sandbox{
		ID: "sb-big", AccountID: "acc-0", State: models.SandboxStateRunning,
		Config: models.SandboxConfig{VCPUs: 100000, MemoryMB: 0},
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	meter := NewBillingMeter(hs, 10*time.Second, 0.06, 0.01)
	for i := 0; i < 5; i++ {
		if err := meter.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		got, err := hs.Accounts().GetByID(ctx, "acc-0")
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got.CreditsRemaining < -store.CreditOverdraftUSD {
			t.Fatalf("balance ran past overdraft floor: %v < %v",
				got.CreditsRemaining, -store.CreditOverdraftUSD)
		}
	}
	got, _ := hs.Accounts().GetByID(ctx, "acc-0")
	if got.CreditsRemaining != -store.CreditOverdraftUSD {
		t.Fatalf("expected clamp at floor %v, got %v", -store.CreditOverdraftUSD, got.CreditsRemaining)
	}
}

// TestStripeCheckoutCreatesSession: POST /v1/billing/checkout must call
// the gateway, return the hosted URL, and persist a pending transaction.
func TestStripeCheckoutCreatesSession(t *testing.T) {
	ctx := context.Background()
	hs := newHandlerStore()
	if err := hs.Accounts().Create(ctx, &models.Account{ID: "acc-1", Email: "a@b.c", CreditsRemaining: 50}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	gw := &fakeStripeGateway{checkoutResult: CheckoutResult{
		SessionID: "cs_test_123",
		URL:       "https://checkout.stripe.test/cs_test_123",
	}}
	h := newBillingHandlers(hs)
	h.Stripe = gw
	h.StripeCfg = StripeConfig{Enabled: true}

	req := httptest.NewRequest(http.MethodPost, "/v1/billing/checkout", strings.NewReader(`{"amount_usd":50}`))
	req = req.WithContext(WithAccountID(req.Context(), "acc-1"))
	rec := httptest.NewRecorder()
	h.createCheckout(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.URL != "https://checkout.stripe.test/cs_test_123" {
		t.Fatalf("url: got %q", resp.URL)
	}
	if gw.checkoutCalls != 1 || gw.lastAccountID != "acc-1" || gw.lastAmountUSD != 50 {
		t.Fatalf("gateway misused: calls=%d account=%q amount=%v",
			gw.checkoutCalls, gw.lastAccountID, gw.lastAmountUSD)
	}
	txns, err := hs.Transactions().ListByAccount(ctx, "acc-1", 10)
	if err != nil || len(txns) != 1 {
		t.Fatalf("expected one transaction: err=%v txns=%+v", err, txns)
	}
	if txns[0].Status != models.TransactionPending || txns[0].StripeSessionID != "cs_test_123" {
		t.Fatalf("unexpected pending transaction: %+v", txns[0])
	}
}

// TestStripeCheckoutRejectsBadAmount: amounts outside the $10–$1000 band
// are a 400 and never reach the gateway.
func TestStripeCheckoutRejectsBadAmount(t *testing.T) {
	ctx := context.Background()
	hs := newHandlerStore()
	_ = hs.Accounts().Create(ctx, &models.Account{ID: "acc-1", Email: "a@b.c"})
	gw := &fakeStripeGateway{}
	h := newBillingHandlers(hs)
	h.Stripe = gw
	h.StripeCfg = StripeConfig{Enabled: true}

	for _, amt := range []string{`{"amount_usd":5}`, `{"amount_usd":5000}`} {
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/checkout", strings.NewReader(amt))
		req = req.WithContext(WithAccountID(req.Context(), "acc-1"))
		rec := httptest.NewRecorder()
		h.createCheckout(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("amount %s: expected 400, got %d", amt, rec.Code)
		}
	}
	if gw.checkoutCalls != 0 {
		t.Fatalf("gateway should not be called for invalid amounts")
	}
}

// TestStripeWebhookAddsCredits: a verified checkout.session.completed
// event credits the account once and is idempotent on redelivery.
func TestStripeWebhookAddsCredits(t *testing.T) {
	ctx := context.Background()
	hs := newHandlerStore()
	if err := hs.Accounts().Create(ctx, &models.Account{ID: "acc-1", Email: "a@b.c", CreditsRemaining: 50}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := hs.Transactions().Create(ctx, &models.Transaction{
		ID: "txn-1", AccountID: "acc-1", AmountUSD: 50,
		StripeSessionID: "cs_test_paid", Status: models.TransactionPending,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed transaction: %v", err)
	}
	gw := &fakeStripeGateway{webhookEvent: StripeWebhookEvent{
		Type: "checkout.session.completed", SessionID: "cs_test_paid",
		AccountID: "acc-1", AmountTotalCents: 5000,
	}}
	h := newBillingHandlers(hs)
	h.Stripe = gw
	h.StripeCfg = StripeConfig{Enabled: true}

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", strings.NewReader("{}"))
		req.Header.Set("Stripe-Signature", "t=1,v1=stub")
		rec := httptest.NewRecorder()
		h.stripeWebhook(rec, req)
		return rec
	}

	if rec := post(); rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body %s", rec.Code, rec.Body.String())
	}
	got, _ := hs.Accounts().GetByID(ctx, "acc-1")
	if got.CreditsRemaining != 100 {
		t.Fatalf("credits: got %v want 100", got.CreditsRemaining)
	}
	txns, _ := hs.Transactions().ListByAccount(ctx, "acc-1", 10)
	if len(txns) != 1 || txns[0].Status != models.TransactionCompleted {
		t.Fatalf("transaction not marked completed: %+v", txns)
	}

	// Stripe delivers at-least-once: a redelivery must not credit again.
	if rec := post(); rec.Code != http.StatusOK {
		t.Fatalf("redelivery status: got %d", rec.Code)
	}
	got2, _ := hs.Accounts().GetByID(ctx, "acc-1")
	if got2.CreditsRemaining != 100 {
		t.Fatalf("redelivery double-credited: balance %v", got2.CreditsRemaining)
	}
}

// TestStripeWebhookRejectsInvalidSignature: a payload with a bogus
// Stripe-Signature is rejected with 400 and never settles.
func TestStripeWebhookRejectsInvalidSignature(t *testing.T) {
	hs := newHandlerStore()
	h := newBillingHandlers(hs)
	// The live gateway runs the real stripe-go signature verification.
	h.Stripe = NewLiveStripeGateway(StripeConfig{
		Enabled: true, SecretKey: "sk_test_x", WebhookSecret: "whsec_testsecret",
	})
	h.StripeCfg = StripeConfig{Enabled: true}

	req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook",
		strings.NewReader(`{"type":"checkout.session.completed"}`))
	req.Header.Set("Stripe-Signature", "t=12345,v1=deadbeefdeadbeefdeadbeef")
	rec := httptest.NewRecorder()
	h.stripeWebhook(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid signature, got %d body %s", rec.Code, rec.Body.String())
	}
}
