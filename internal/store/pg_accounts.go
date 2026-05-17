package store

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/allenabraham999/vajra/internal/models"
)

type pgAccountStore struct{ ext sqlx.ExtContext }

// accountColumns is the SELECT projection. credits_remaining, is_admin,
// suspended and last_login are read here but never written by Create —
// new rows inherit the column DEFAULTs (demo credit grant, non-admin,
// not suspended, NULL last_login).
const accountColumns = `id, email, password_hash, created_at, credits_remaining, is_admin, suspended, last_login`

func (s *pgAccountStore) Create(ctx context.Context, a *models.Account) error {
	const q = `INSERT INTO accounts (id, email, password_hash, created_at)
	           VALUES (:id, :email, :password_hash, :created_at)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, a)
	return translate(err)
}

// SetAdmin flips the is_admin flag for one account.
func (s *pgAccountStore) SetAdmin(ctx context.Context, id string, isAdmin bool) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE accounts SET is_admin = $2 WHERE id = $1`, id, isAdmin)
	return expectAffected(res, err)
}

// SetSuspended flips the suspended flag for one account.
func (s *pgAccountStore) SetSuspended(ctx context.Context, id string, suspended bool) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE accounts SET suspended = $2 WHERE id = $1`, id, suspended)
	return expectAffected(res, err)
}

// UpdateLastLogin stamps last_login for one account.
func (s *pgAccountStore) UpdateLastLogin(ctx context.Context, id string, ts time.Time) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE accounts SET last_login = $2 WHERE id = $1`, id, ts.UTC())
	return expectAffected(res, err)
}

// UpdatePassword replaces the stored bcrypt hash for one account.
func (s *pgAccountStore) UpdatePassword(ctx context.Context, id, passwordHash string) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE accounts SET password_hash = $2 WHERE id = $1`, id, passwordHash)
	return expectAffected(res, err)
}

// DecrementCredits subtracts amount from credits_remaining, flooring the
// balance at -CreditOverdraftUSD so a meter that keeps charging a tenant
// past zero cannot drive the balance arbitrarily negative.
func (s *pgAccountStore) DecrementCredits(ctx context.Context, accountID string, amount float64) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE accounts
		    SET credits_remaining = GREATEST(credits_remaining - $2, $3)
		  WHERE id = $1`,
		accountID, amount, -CreditOverdraftUSD)
	return expectAffected(res, err)
}

// IncrementCredits adds amount to credits_remaining. Called when a Stripe
// payment clears.
func (s *pgAccountStore) IncrementCredits(ctx context.Context, accountID string, amount float64) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE accounts SET credits_remaining = credits_remaining + $2 WHERE id = $1`,
		accountID, amount)
	return expectAffected(res, err)
}

func (s *pgAccountStore) GetByID(ctx context.Context, id string) (*models.Account, error) {
	var a models.Account
	err := sqlx.GetContext(ctx, s.ext, &a,
		`SELECT `+accountColumns+` FROM accounts WHERE id = $1`, id)
	if err != nil {
		return nil, translate(err)
	}
	return &a, nil
}

func (s *pgAccountStore) GetByEmail(ctx context.Context, email string) (*models.Account, error) {
	var a models.Account
	err := sqlx.GetContext(ctx, s.ext, &a,
		`SELECT `+accountColumns+` FROM accounts WHERE email = $1`, email)
	if err != nil {
		return nil, translate(err)
	}
	return &a, nil
}

func (s *pgAccountStore) List(ctx context.Context, opts ListOpts) ([]*models.Account, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Account{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+accountColumns+` FROM accounts
		 ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgAccountStore) Delete(ctx context.Context, id string) error {
	res, err := s.ext.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, id)
	return expectAffected(res, err)
}

type pgAPIKeyStore struct{ ext sqlx.ExtContext }

const apiKeyColumns = `id, account_id, key_hash, name, permissions, created_at, expires_at`

func (s *pgAPIKeyStore) Create(ctx context.Context, k *models.APIKey) error {
	const q = `INSERT INTO api_keys (` + apiKeyColumns + `)
	           VALUES (:id, :account_id, :key_hash, :name, :permissions, :created_at, :expires_at)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, k)
	return translate(err)
}

func (s *pgAPIKeyStore) GetByID(ctx context.Context, accountID, id string) (*models.APIKey, error) {
	var k models.APIKey
	err := sqlx.GetContext(ctx, s.ext, &k,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE account_id = $1 AND id = $2`,
		accountID, id)
	if err != nil {
		return nil, translate(err)
	}
	return &k, nil
}

func (s *pgAPIKeyStore) GetByHash(ctx context.Context, keyHash string) (*models.APIKey, error) {
	var k models.APIKey
	err := sqlx.GetContext(ctx, s.ext, &k,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE key_hash = $1`, keyHash)
	if err != nil {
		return nil, translate(err)
	}
	return &k, nil
}

func (s *pgAPIKeyStore) ListByAccount(ctx context.Context, accountID string, opts ListOpts) ([]*models.APIKey, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.APIKey{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE account_id = $1
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, accountID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgAPIKeyStore) Delete(ctx context.Context, accountID, id string) error {
	res, err := s.ext.ExecContext(ctx,
		`DELETE FROM api_keys WHERE account_id = $1 AND id = $2`, accountID, id)
	return expectAffected(res, err)
}
