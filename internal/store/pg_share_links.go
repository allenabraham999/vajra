package store

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/allenabraham999/vajra/internal/models"
)

// pgShareLinkStore is the Postgres-backed ShareLinkStore. Mirrors the
// pattern in pg_accounts.go: ext is *sqlx.DB at the root and *sqlx.Tx
// inside a WithTx callback.
type pgShareLinkStore struct{ ext sqlx.ExtContext }

const shareLinkColumns = `id, account_id, sandbox_id, token_hash, port, created_at, expires_at, revoked_at`

// Create inserts a share link. Conflict on token_hash is unlikely
// (256-bit randomness) but bubbles up via translate as ErrConflict.
func (s *pgShareLinkStore) Create(ctx context.Context, sl *models.ShareLink) error {
	const q = `INSERT INTO share_links (` + shareLinkColumns + `)
	           VALUES (:id, :account_id, :sandbox_id, :token_hash, :port, :created_at, :expires_at, :revoked_at)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, sl)
	return translate(err)
}

// GetByID is account-scoped — never returns another account's share.
func (s *pgShareLinkStore) GetByID(ctx context.Context, accountID, id string) (*models.ShareLink, error) {
	var sl models.ShareLink
	err := sqlx.GetContext(ctx, s.ext, &sl,
		`SELECT `+shareLinkColumns+` FROM share_links WHERE account_id = $1 AND id = $2`,
		accountID, id)
	if err != nil {
		return nil, translate(err)
	}
	return &sl, nil
}

// GetByHash is the proxy's hot path: look up a share by token hash.
// Cross-account by design — the proxy doesn't know the account ID at
// the time of validation.
func (s *pgShareLinkStore) GetByHash(ctx context.Context, tokenHash string) (*models.ShareLink, error) {
	var sl models.ShareLink
	err := sqlx.GetContext(ctx, s.ext, &sl,
		`SELECT `+shareLinkColumns+` FROM share_links WHERE token_hash = $1`, tokenHash)
	if err != nil {
		return nil, translate(err)
	}
	return &sl, nil
}

// ListBySandbox returns the calling account's shares for one sandbox.
func (s *pgShareLinkStore) ListBySandbox(ctx context.Context, accountID, sandboxID string, opts ListOpts) ([]*models.ShareLink, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.ShareLink{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+shareLinkColumns+` FROM share_links
		 WHERE account_id = $1 AND sandbox_id = $2
		 ORDER BY created_at DESC LIMIT $3 OFFSET $4`,
		accountID, sandboxID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// Revoke marks a share as revoked. Idempotent — revoking an already-
// revoked share is a no-op (we still update revoked_at to the latest
// timestamp so the audit log shows the most recent attempt).
func (s *pgShareLinkStore) Revoke(ctx context.Context, accountID, id string, at time.Time) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE share_links SET revoked_at = $3 WHERE account_id = $1 AND id = $2`,
		accountID, id, at)
	return expectAffected(res, err)
}
