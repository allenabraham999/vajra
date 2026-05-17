// Package master — handlers_billing.go is the prepaid-credit billing
// surface: Stripe checkout, the signature-verified webhook, the
// transaction history, and the usage-summary dashboard payload.
package master

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// Checkout amount bounds (USD). The minimum stops dust transactions; the
// maximum is a sanity cap for the demo.
const (
	minCheckoutUSD = 10.0
	maxCheckoutUSD = 1000.0
	// webhookBodyLimit caps the Stripe webhook body so a bogus unauthen-
	// ticated POST cannot stream master out of memory.
	webhookBodyLimit = 1 << 20 // 1 MiB
)

// vcpuRate / memRate return the meter's billing rates, falling back to the
// defaults so GET /v1/usage/summary reports a sensible burn even when the
// rates were never set from config.
func (h *Handlers) vcpuRate() float64 {
	if h.VCPUHourlyUSD > 0 {
		return h.VCPUHourlyUSD
	}
	return defaultVCPUHourlyUSD
}

func (h *Handlers) memRate() float64 {
	if h.MemoryGBHourlyUSD > 0 {
		return h.MemoryGBHourlyUSD
	}
	return defaultMemoryGBHourlyUSD
}

// round4 trims a USD/hours figure to 4 decimal places so the JSON the
// dashboard renders is free of float-noise digits.
func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

// billingConfigResponse is GET /v1/billing/config — a public probe the
// dashboard uses to decide whether to show the "Add Funds" button.
type billingConfigResponse struct {
	StripeEnabled  bool   `json:"stripe_enabled"`
	PublishableKey string `json:"publishable_key,omitempty"`
}

// getBillingConfig reports whether Stripe checkout is wired. Public: it
// exposes only the publishable key, which is safe to reveal by design.
func (h *Handlers) getBillingConfig(w http.ResponseWriter, _ *http.Request) {
	resp := billingConfigResponse{StripeEnabled: h.Stripe != nil && h.StripeCfg.Enabled}
	if resp.StripeEnabled {
		resp.PublishableKey = h.StripeCfg.PublishableKey
	}
	writeJSON(w, http.StatusOK, resp)
}

// checkoutRequest is the POST /v1/billing/checkout body.
type checkoutRequest struct {
	AmountUSD float64 `json:"amount_usd"`
}

// createCheckout opens a Stripe Checkout session for a credit top-up,
// records a pending transaction keyed on the session ID, and returns the
// hosted-page URL for the dashboard to redirect to.
func (h *Handlers) createCheckout(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	if h.Stripe == nil {
		writeErr(w, http.StatusServiceUnavailable, "billing is not enabled")
		return
	}
	var req checkoutRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.AmountUSD < minCheckoutUSD || req.AmountUSD > maxCheckoutUSD {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("amount_usd must be between $%.0f and $%.0f", minCheckoutUSD, maxCheckoutUSD))
		return
	}

	result, err := h.Stripe.CreateCheckoutSession(r.Context(), accountID, req.AmountUSD)
	if err != nil {
		h.log().Error("billing: create checkout session", "account_id", accountID, "err", err)
		writeErr(w, http.StatusBadGateway, "could not start checkout")
		return
	}

	id, err := randomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	txn := &models.Transaction{
		ID:              id,
		AccountID:       accountID,
		AmountUSD:       req.AmountUSD,
		StripeSessionID: result.SessionID,
		Status:          models.TransactionPending,
		CreatedAt:       h.now().UTC(),
	}
	if err := h.Store.Transactions().Create(r.Context(), txn); err != nil {
		h.log().Error("billing: record pending transaction", "account_id", accountID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.log().Info("billing: checkout session created",
		"account_id", accountID,
		"amount_usd", req.AmountUSD,
		"session_id", result.SessionID,
	)
	writeJSON(w, http.StatusOK, map[string]string{"url": result.URL})
}

// stripeWebhook receives Stripe events. It is intentionally unauthenticated
// — the Stripe-Signature header, verified against the signing secret, is
// the authentication. On a cleared payment it credits the account exactly
// once: the pending-status guard inside MarkCompleted makes Stripe's
// at-least-once redelivery a harmless no-op.
func (h *Handlers) stripeWebhook(w http.ResponseWriter, r *http.Request) {
	if h.Stripe == nil {
		writeErr(w, http.StatusServiceUnavailable, "billing is not enabled")
		return
	}
	payload, err := io.ReadAll(io.LimitReader(r.Body, webhookBodyLimit))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read body")
		return
	}
	event, err := h.Stripe.VerifyWebhook(payload, r.Header.Get("Stripe-Signature"))
	if err != nil {
		h.log().Warn("billing: webhook signature rejected", "err", err)
		writeErr(w, http.StatusBadRequest, "invalid signature")
		return
	}
	if !event.Completed() {
		h.log().Info("billing: webhook ignored", "type", event.Type)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	amountUSD := float64(event.AmountTotalCents) / 100.0
	credited := false
	err = h.Store.WithTx(r.Context(), func(s store.Store) error {
		ok, err := s.Transactions().MarkCompleted(r.Context(), event.SessionID)
		if err != nil {
			return err
		}
		if !ok {
			// Unknown or already-completed session — idempotent no-op.
			return nil
		}
		credited = true
		return s.Accounts().IncrementCredits(r.Context(), event.AccountID, amountUSD)
	})
	if err != nil {
		h.log().Error("billing: webhook settlement failed",
			"account_id", event.AccountID, "session_id", event.SessionID, "err", err)
		writeErr(w, http.StatusInternalServerError, "settlement failed")
		return
	}
	if credited {
		h.log().Info("billing: payment received",
			"account_id", event.AccountID,
			"session_id", event.SessionID,
			"amount_usd", amountUSD,
		)
	} else {
		h.log().Info("billing: webhook already processed", "session_id", event.SessionID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// listTransactions returns the caller's most recent credit purchases.
func (h *Handlers) listTransactions(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	txns, err := h.Store.Transactions().ListByAccount(r.Context(), accountID, 50)
	if err != nil {
		h.log().Error("billing: list transactions", "account_id", accountID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transactions": txns})
}

// usageSummary is GET /v1/usage/summary — the billing dashboard payload.
type usageSummary struct {
	CreditsRemaining  float64           `json:"credits_remaining"`
	TotalSpend30d     float64           `json:"total_spend_30d"`
	VCPUHours30d      float64           `json:"vcpu_hours_30d"`
	MemoryGBHours30d  float64           `json:"memory_gb_hours_30d"`
	CurrentHourlyBurn float64           `json:"current_hourly_burn"`
	DailySpend        []dailySpendPoint `json:"daily_spend"`
	PerSandbox        []sandboxCost     `json:"per_sandbox"`
}

// dailySpendPoint is one bar of the 30-day spend chart.
type dailySpendPoint struct {
	Date   string  `json:"date"`
	Amount float64 `json:"amount"`
}

// sandboxCost is one row of the top-spenders table.
type sandboxCost struct {
	Name      string  `json:"name"`
	VCPUHours float64 `json:"vcpu_hours"`
	Cost      float64 `json:"cost"`
}

// getUsageSummary aggregates the billing dashboard's numbers: the prepaid
// balance, 30-day spend + usage from the meter's daily rollup, the live
// hourly burn of running sandboxes, and the top 10 sandboxes by cost.
func (h *Handlers) getUsageSummary(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	usage := h.Store.Usage()
	if usage == nil {
		writeErr(w, http.StatusServiceUnavailable, "usage tracking unavailable")
		return
	}
	ctx := r.Context()
	now := h.now().UTC()
	from := now.AddDate(0, 0, -30)

	summary := usageSummary{DailySpend: []dailySpendPoint{}, PerSandbox: []sandboxCost{}}

	acc, err := h.Store.Accounts().GetByID(ctx, accountID)
	if err != nil {
		h.log().Error("usage summary: load account", "account_id", accountID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	summary.CreditsRemaining = round4(acc.CreditsRemaining)

	// 30-day spend + usage from the billing meter's daily rollup.
	daily, err := usage.DailySummary(ctx, accountID, from, now)
	if err != nil {
		h.log().Error("usage summary: daily rollup", "account_id", accountID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, d := range daily {
		summary.TotalSpend30d += d.CostUSD
		summary.VCPUHours30d += d.VCPUHours
		summary.MemoryGBHours30d += d.MemoryGBHours
		summary.DailySpend = append(summary.DailySpend, dailySpendPoint{
			Date:   d.Day.Format("2006-01-02"),
			Amount: round4(d.CostUSD),
		})
	}
	summary.TotalSpend30d = round4(summary.TotalSpend30d)
	summary.VCPUHours30d = round4(summary.VCPUHours30d)
	summary.MemoryGBHours30d = round4(summary.MemoryGBHours30d)

	// Live hourly burn: the combined hourly cost of running sandboxes.
	sandboxes, err := h.Store.Sandboxes().ListByAccount(ctx, accountID, store.ListOpts{Limit: 1000})
	if err != nil {
		h.log().Error("usage summary: list sandboxes", "account_id", accountID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	names := make(map[string]string, len(sandboxes))
	for _, sb := range sandboxes {
		names[sb.ID] = sb.Name
		if sb.State == models.SandboxStateRunning {
			summary.CurrentHourlyBurn += float64(sb.Config.VCPUs)*h.vcpuRate() +
				(float64(sb.Config.MemoryMB)/1024.0)*h.memRate()
		}
	}
	summary.CurrentHourlyBurn = round4(summary.CurrentHourlyBurn)

	// Per-sandbox breakdown from the interval ledger, top 10 by cost.
	rows, err := usage.PerSandbox(ctx, accountID, from, now)
	if err != nil {
		h.log().Error("usage summary: per-sandbox", "account_id", accountID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	costs := make([]sandboxCost, 0, len(rows))
	for _, row := range rows {
		name := names[row.SandboxID]
		if name == "" {
			name = row.SandboxID
		}
		costs = append(costs, sandboxCost{
			Name:      name,
			VCPUHours: round4(float64(row.VCPUSeconds) / 3600.0),
			Cost:      round4(row.Cost),
		})
	}
	sort.Slice(costs, func(i, j int) bool { return costs[i].Cost > costs[j].Cost })
	if len(costs) > 10 {
		costs = costs[:10]
	}
	summary.PerSandbox = costs

	writeJSON(w, http.StatusOK, summary)
}
