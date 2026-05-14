package store

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/allenabraham999/vajra/internal/models"
)

type pgBuildStore struct{ ext sqlx.ExtContext }

const buildColumns = `id, account_id, template_name, template_ver, status, dockerfile, template_id, error, created_at, completed_at`

func (s *pgBuildStore) Create(ctx context.Context, b *models.Build) error {
	const q = `INSERT INTO builds (` + buildColumns + `)
	           VALUES (:id, :account_id, :template_name, :template_ver, :status, :dockerfile, :template_id, :error, :created_at, :completed_at)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, b)
	return translate(err)
}

func (s *pgBuildStore) GetByID(ctx context.Context, accountID, id string) (*models.Build, error) {
	var b models.Build
	err := sqlx.GetContext(ctx, s.ext, &b,
		`SELECT `+buildColumns+` FROM builds WHERE account_id = $1 AND id = $2`,
		accountID, id)
	if err != nil {
		return nil, translate(err)
	}
	return &b, nil
}

func (s *pgBuildStore) ListByAccount(ctx context.Context, accountID string, opts ListOpts) ([]*models.Build, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Build{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+buildColumns+` FROM builds WHERE account_id = $1
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, accountID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgBuildStore) UpdateStatus(ctx context.Context, id string, status models.BuildStatus, templateID, errMsg *string, completedAt *time.Time) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE builds SET status = $1, template_id = $2, error = $3, completed_at = $4 WHERE id = $5`,
		string(status), templateID, errMsg, completedAt, id)
	return expectAffected(res, err)
}
