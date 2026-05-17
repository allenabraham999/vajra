package models

import "time"

// Transaction status values. A purchase starts pending, then the Stripe
// webhook moves it to completed (payment cleared) or it is left to expire.
const (
	TransactionPending   = "pending"
	TransactionCompleted = "completed"
	TransactionFailed    = "failed"
)

// Transaction is one credit purchase made through Stripe Checkout. The
// row is written in TransactionPending when the checkout session opens
// and flipped to TransactionCompleted by the signed Stripe webhook.
type Transaction struct {
	ID              string    `db:"id" json:"id"`
	AccountID       string    `db:"account_id" json:"account_id"`
	AmountUSD       float64   `db:"amount_usd" json:"amount_usd"`
	StripeSessionID string    `db:"stripe_session_id" json:"stripe_session_id"`
	Status          string    `db:"status" json:"status"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
}
