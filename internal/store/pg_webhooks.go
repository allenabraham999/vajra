package store

import (
	"context"

	"github.com/jmoiron/sqlx"

	"github.com/allenabraham999/vajra/internal/models"
)

type pgWebhookStore struct{ ext sqlx.ExtContext }

const webhookColumns = `id, account_id, url, secret, events, active, created_at`

func (s *pgWebhookStore) Create(ctx context.Context, w *models.Webhook) error {
	const q = `INSERT INTO webhooks (` + webhookColumns + `)
	           VALUES (:id, :account_id, :url, :secret, :events, :active, :created_at)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, w)
	return translate(err)
}

func (s *pgWebhookStore) GetByID(ctx context.Context, accountID, id string) (*models.Webhook, error) {
	var w models.Webhook
	err := sqlx.GetContext(ctx, s.ext, &w,
		`SELECT `+webhookColumns+` FROM webhooks WHERE account_id = $1 AND id = $2`,
		accountID, id)
	if err != nil {
		return nil, translate(err)
	}
	return &w, nil
}

func (s *pgWebhookStore) ListByAccount(ctx context.Context, accountID string, opts ListOpts) ([]*models.Webhook, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Webhook{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+webhookColumns+` FROM webhooks WHERE account_id = $1
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, accountID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// ListActiveByEvent returns every active webhook for accountID whose
// events JSONB array contains the given event name. The @> JSONB
// containment operator uses GIN if one is added later; without it this
// is a sequential scan, which is fine while webhook counts are small.
func (s *pgWebhookStore) ListActiveByEvent(ctx context.Context, accountID, event string) ([]*models.Webhook, error) {
	out := []*models.Webhook{}
	q := `SELECT ` + webhookColumns + ` FROM webhooks
	      WHERE account_id = $1 AND active = TRUE
	        AND events @> to_jsonb($2::text)
	      ORDER BY created_at DESC`
	if err := sqlx.SelectContext(ctx, s.ext, &out, q, accountID, event); err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgWebhookStore) Delete(ctx context.Context, accountID, id string) error {
	res, err := s.ext.ExecContext(ctx,
		`DELETE FROM webhooks WHERE account_id = $1 AND id = $2`, accountID, id)
	return expectAffected(res, err)
}
