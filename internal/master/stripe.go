// Package master — stripe.go wraps the Stripe SDK behind a small gateway
// interface. The billing handlers depend on StripeGateway, not on
// stripe-go directly, so tests can substitute a fake and never make a
// network call.
package master

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/stripe/stripe-go/v76"
	"github.com/stripe/stripe-go/v76/checkout/session"
	"github.com/stripe/stripe-go/v76/webhook"
)

// eventCheckoutCompleted is the only Stripe event the webhook acts on: a
// Checkout session whose payment has fully cleared.
const eventCheckoutCompleted = "checkout.session.completed"

// StripeConfig is the env-derived Stripe configuration. Enabled gates the
// checkout/webhook surface; the rest comes straight from the VAJRA_STRIPE_*
// vars.
type StripeConfig struct {
	Enabled        bool
	SecretKey      string
	PublishableKey string
	WebhookSecret  string
	SuccessURL     string
	CancelURL      string
}

// CheckoutResult is what a created Checkout session yields the handler:
// the hosted-page URL to redirect the browser to, and the session ID we
// persist on the pending transaction.
type CheckoutResult struct {
	SessionID string
	URL       string
}

// StripeWebhookEvent is the verified, decoded subset of a Stripe webhook
// the handler needs. For events other than a completed checkout only Type
// is populated.
type StripeWebhookEvent struct {
	Type             string
	SessionID        string
	AccountID        string
	AmountTotalCents int64
}

// Completed reports whether this event is a cleared checkout payment.
func (e StripeWebhookEvent) Completed() bool { return e.Type == eventCheckoutCompleted }

// StripeGateway is the seam between the billing handlers and Stripe. The
// live implementation talks to the Stripe API; tests substitute a fake.
type StripeGateway interface {
	// CreateCheckoutSession opens a hosted Checkout session for amountUSD
	// of credit, tagged with accountID so the webhook can credit the
	// correct account.
	CreateCheckoutSession(ctx context.Context, accountID string, amountUSD float64) (CheckoutResult, error)
	// VerifyWebhook checks the Stripe-Signature header against the signing
	// secret and decodes the payload. A signature mismatch returns a
	// non-nil error, and the handler must then reject the request.
	VerifyWebhook(payload []byte, signatureHeader string) (StripeWebhookEvent, error)
}

// LiveStripeGateway is the production StripeGateway, backed by stripe-go.
type LiveStripeGateway struct {
	cfg StripeConfig
}

// NewLiveStripeGateway wires a gateway against the live Stripe API. It
// sets the process-wide stripe.Key: there is exactly one Stripe account
// per master deployment, so a global key is sufficient and matches how
// stripe-go is designed to be used.
func NewLiveStripeGateway(cfg StripeConfig) *LiveStripeGateway {
	stripe.Key = cfg.SecretKey
	return &LiveStripeGateway{cfg: cfg}
}

// CreateCheckoutSession opens a one-off ("payment" mode) Checkout session
// for amountUSD of account credit.
func (g *LiveStripeGateway) CreateCheckoutSession(_ context.Context, accountID string, amountUSD float64) (CheckoutResult, error) {
	amountCents := int64(math.Round(amountUSD * 100))
	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
				Currency: stripe.String(string(stripe.CurrencyUSD)),
				ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
					Name: stripe.String("Vajra account credits"),
				},
				UnitAmount: stripe.Int64(amountCents),
			},
			Quantity: stripe.Int64(1),
		}},
		SuccessURL: stripe.String(g.cfg.SuccessURL + "&session_id={CHECKOUT_SESSION_ID}"),
		CancelURL:  stripe.String(g.cfg.CancelURL),
	}
	// Metadata rides the session and comes back on the webhook, which is
	// how the webhook handler knows which account to credit.
	params.AddMetadata("account_id", accountID)
	params.AddMetadata("amount_usd", strconv.FormatFloat(amountUSD, 'f', 2, 64))

	sess, err := session.New(params)
	if err != nil {
		return CheckoutResult{}, fmt.Errorf("stripe: new checkout session: %w", err)
	}
	return CheckoutResult{SessionID: sess.ID, URL: sess.URL}, nil
}

// VerifyWebhook validates the Stripe-Signature header against the signing
// secret, then decodes the event. An invalid signature surfaces as an
// error so the handler returns 400 and the payload is never trusted.
func (g *LiveStripeGateway) VerifyWebhook(payload []byte, signatureHeader string) (StripeWebhookEvent, error) {
	event, err := webhook.ConstructEvent(payload, signatureHeader, g.cfg.WebhookSecret)
	if err != nil {
		return StripeWebhookEvent{}, fmt.Errorf("stripe: webhook signature: %w", err)
	}
	out := StripeWebhookEvent{Type: string(event.Type)}
	if !out.Completed() || event.Data == nil {
		return out, nil
	}
	var sess stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
		return out, fmt.Errorf("stripe: decode checkout session: %w", err)
	}
	out.SessionID = sess.ID
	out.AmountTotalCents = sess.AmountTotal
	out.AccountID = sess.Metadata["account_id"]
	return out, nil
}
