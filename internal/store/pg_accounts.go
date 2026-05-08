package store

import (
	"context"

	"github.com/jmoiron/sqlx"

	"github.com/allenabraham999/vajra/internal/models"
)

type pgAccountStore struct{ ext sqlx.ExtContext }

const accountColumns = `id, email, password_hash, created_at`

func (s *pgAccountStore) Create(ctx context.Context, a *models.Account) error {
	const q = `INSERT INTO accounts (` + accountColumns + `)
	           VALUES (:id, :email, :password_hash, :created_at)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, a)
	return translate(err)
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
