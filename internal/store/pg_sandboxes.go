package store

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/allenabraham999/vajra/internal/models"
)

type pgSandboxStore struct{ ext sqlx.ExtContext }

const sandboxColumns = `id, name, account_id, node_id, cluster_id, template_id, state, config, created_at, updated_at`

func (s *pgSandboxStore) Create(ctx context.Context, sb *models.Sandbox) error {
	const q = `INSERT INTO sandboxes (` + sandboxColumns + `)
	           VALUES (:id, :name, :account_id, :node_id, :cluster_id, :template_id, :state, :config, :created_at, :updated_at)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, sb)
	return translate(err)
}

func (s *pgSandboxStore) GetByID(ctx context.Context, accountID, id string) (*models.Sandbox, error) {
	var sb models.Sandbox
	err := sqlx.GetContext(ctx, s.ext, &sb,
		`SELECT `+sandboxColumns+` FROM sandboxes WHERE account_id = $1 AND id = $2`,
		accountID, id)
	if err != nil {
		return nil, translate(err)
	}
	return &sb, nil
}

// GetByIDUnscoped is the system-internal lookup used by proxy-route /
// reconciler. The caller has already proved authorization out-of-band;
// here we just return the row regardless of account.
func (s *pgSandboxStore) GetByIDUnscoped(ctx context.Context, id string) (*models.Sandbox, error) {
	var sb models.Sandbox
	err := sqlx.GetContext(ctx, s.ext, &sb,
		`SELECT `+sandboxColumns+` FROM sandboxes WHERE id = $1`, id)
	if err != nil {
		return nil, translate(err)
	}
	return &sb, nil
}

func (s *pgSandboxStore) ListByAccount(ctx context.Context, accountID string, opts ListOpts) ([]*models.Sandbox, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Sandbox{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+sandboxColumns+` FROM sandboxes WHERE account_id = $1
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, accountID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgSandboxStore) ListByNode(ctx context.Context, nodeID string, opts ListOpts) ([]*models.Sandbox, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Sandbox{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+sandboxColumns+` FROM sandboxes WHERE node_id = $1
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, nodeID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgSandboxStore) ListByState(ctx context.Context, state models.SandboxState, opts ListOpts) ([]*models.Sandbox, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Sandbox{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+sandboxColumns+` FROM sandboxes WHERE state = $1
		 ORDER BY created_at ASC LIMIT $2 OFFSET $3`, string(state), limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgSandboxStore) UpdateState(ctx context.Context, accountID, id string, state models.SandboxState) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE sandboxes SET state = $1, updated_at = NOW()
		 WHERE account_id = $2 AND id = $3`, string(state), accountID, id)
	return expectAffected(res, err)
}

func (s *pgSandboxStore) UpdatePlacement(ctx context.Context, id string, clusterID, nodeID string) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE sandboxes SET cluster_id = $1, node_id = $2, updated_at = NOW()
		 WHERE id = $3`, nilIfEmpty(clusterID), nilIfEmpty(nodeID), id)
	return expectAffected(res, err)
}

func (s *pgSandboxStore) Delete(ctx context.Context, accountID, id string) error {
	res, err := s.ext.ExecContext(ctx,
		`DELETE FROM sandboxes WHERE account_id = $1 AND id = $2`, accountID, id)
	return expectAffected(res, err)
}

type pgSnapshotStore struct{ ext sqlx.ExtContext }

const snapshotColumns = `id, sandbox_id, account_id, node_id, storage_path, size_bytes, created_at`

func (s *pgSnapshotStore) Create(ctx context.Context, sn *models.Snapshot) error {
	const q = `INSERT INTO snapshots (` + snapshotColumns + `)
	           VALUES (:id, :sandbox_id, :account_id, :node_id, :storage_path, :size_bytes, :created_at)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, sn)
	return translate(err)
}

func (s *pgSnapshotStore) GetByID(ctx context.Context, accountID, id string) (*models.Snapshot, error) {
	var sn models.Snapshot
	err := sqlx.GetContext(ctx, s.ext, &sn,
		`SELECT `+snapshotColumns+` FROM snapshots WHERE account_id = $1 AND id = $2`,
		accountID, id)
	if err != nil {
		return nil, translate(err)
	}
	return &sn, nil
}

func (s *pgSnapshotStore) ListBySandbox(ctx context.Context, accountID, sandboxID string, opts ListOpts) ([]*models.Snapshot, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Snapshot{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+snapshotColumns+` FROM snapshots
		 WHERE account_id = $1 AND sandbox_id = $2
		 ORDER BY created_at DESC LIMIT $3 OFFSET $4`,
		accountID, sandboxID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgSnapshotStore) ListByAccount(ctx context.Context, accountID string, opts ListOpts) ([]*models.Snapshot, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Snapshot{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+snapshotColumns+` FROM snapshots WHERE account_id = $1
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, accountID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgSnapshotStore) ListByNode(ctx context.Context, nodeID string, opts ListOpts) ([]*models.Snapshot, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Snapshot{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+snapshotColumns+` FROM snapshots WHERE node_id = $1
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, nodeID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgSnapshotStore) Delete(ctx context.Context, accountID, id string) error {
	res, err := s.ext.ExecContext(ctx,
		`DELETE FROM snapshots WHERE account_id = $1 AND id = $2`, accountID, id)
	return expectAffected(res, err)
}

type pgOperationStore struct{ ext sqlx.ExtContext }

const operationColumns = `id, account_id, sandbox_id, type, status, started_at, completed_at, error`

func (s *pgOperationStore) Create(ctx context.Context, op *models.Operation) error {
	const q = `INSERT INTO operations (` + operationColumns + `)
	           VALUES (:id, :account_id, :sandbox_id, :type, :status, :started_at, :completed_at, :error)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, op)
	return translate(err)
}

func (s *pgOperationStore) GetByID(ctx context.Context, accountID, id string) (*models.Operation, error) {
	var op models.Operation
	err := sqlx.GetContext(ctx, s.ext, &op,
		`SELECT `+operationColumns+` FROM operations WHERE account_id = $1 AND id = $2`,
		accountID, id)
	if err != nil {
		return nil, translate(err)
	}
	return &op, nil
}

func (s *pgOperationStore) ListBySandbox(ctx context.Context, accountID, sandboxID string, opts ListOpts) ([]*models.Operation, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Operation{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+operationColumns+` FROM operations
		 WHERE account_id = $1 AND sandbox_id = $2
		 ORDER BY started_at DESC LIMIT $3 OFFSET $4`,
		accountID, sandboxID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgOperationStore) ListByAccount(ctx context.Context, accountID string, opts ListOpts) ([]*models.Operation, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Operation{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+operationColumns+` FROM operations WHERE account_id = $1
		 ORDER BY started_at DESC LIMIT $2 OFFSET $3`, accountID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgOperationStore) UpdateStatus(ctx context.Context, id string, status models.OperationStatus, errMsg *string, completedAt *time.Time) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE operations SET status = $1, error = $2, completed_at = $3 WHERE id = $4`,
		string(status), errMsg, completedAt, id)
	return expectAffected(res, err)
}
