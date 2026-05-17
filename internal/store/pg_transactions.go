// Package store — pg_transactions.go is the Postgres ledger of Stripe
// credit purchases. See TransactionStore for the contract.
package store

import (
	"context"

	"github.com/jmoiron/sqlx"

	"github.com/allenabraham999/vajra/internal/models"
)

type pgTransactionStore struct{ ext sqlx.ExtContext }

const transactionColumns = `id, account_id, amount_usd, stripe_session_id, status, created_at`

// Create inserts a new transaction row, typically in 'pending' state when
// a Stripe Checkout session opens.
func (s *pgTransactionStore) Create(ctx context.Context, t *models.Transaction) error {
	const q = `INSERT INTO transactions (` + transactionColumns + `)
	           VALUES (:id, :account_id, :amount_usd, :stripe_session_id, :status, :created_at)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, t)
	return translate(err)
}

// MarkCompleted flips a pending transaction to 'completed'. The
// status = 'pending' guard in the WHERE clause is what makes the webhook
// idempotent: a redelivered Stripe event matches zero rows and reports
// false, so the caller skips crediting the account a second time.
func (s *pgTransactionStore) MarkCompleted(ctx context.Context, stripeSessionID string) (bool, error) {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE transactions SET status = $2
		  WHERE stripe_session_id = $1 AND status = $3`,
		stripeSessionID, models.TransactionCompleted, models.TransactionPending)
	if err != nil {
		return false, translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListByAccount returns an account's transactions, most recent first.
// limit is clamped to a sane range so a stray query parameter cannot ask
// for an unbounded scan.
func (s *pgTransactionStore) ListByAccount(ctx context.Context, accountID string, limit int) ([]*models.Transaction, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := []*models.Transaction{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+transactionColumns+` FROM transactions
		  WHERE account_id = $1
		  ORDER BY created_at DESC LIMIT $2`,
		accountID, limit)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}
