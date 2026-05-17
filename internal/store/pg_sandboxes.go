package store

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/allenabraham999/vajra/internal/models"
)

type pgSandboxStore struct{ ext sqlx.ExtContext }

const sandboxColumns = `id, name, account_id, node_id, cluster_id, template_id, state, config, auto_stop_minutes, auto_archive_minutes, last_activity, created_at, updated_at, time_to_running_ms, pool_hit, git_url, git_branch, git_clone_status, git_clone_error`

func (s *pgSandboxStore) Create(ctx context.Context, sb *models.Sandbox) error {
	const q = `INSERT INTO sandboxes (` + sandboxColumns + `)
	           VALUES (:id, :name, :account_id, :node_id, :cluster_id, :template_id, :state, :config, :auto_stop_minutes, :auto_archive_minutes, :last_activity, :created_at, :updated_at, :time_to_running_ms, :pool_hit, :git_url, :git_branch, :git_clone_status, :git_clone_error)`
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

// ListAll returns sandboxes across every account, newest first. Only the
// admin panel calls it; tenant requests never reach this method.
func (s *pgSandboxStore) ListAll(ctx context.Context, opts ListOpts) ([]*models.Sandbox, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Sandbox{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+sandboxColumns+` FROM sandboxes
		 ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
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

// RecordBootMetrics stamps how long a sandbox took to reach RUNNING and
// whether the create was served from the pre-warm pool. Account-scoped;
// called once per sandbox when it first transitions to RUNNING.
func (s *pgSandboxStore) RecordBootMetrics(ctx context.Context, accountID, id string, timeToRunningMs int64, poolHit bool) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE sandboxes SET time_to_running_ms = $1, pool_hit = $2
		 WHERE account_id = $3 AND id = $4`,
		timeToRunningMs, poolHit, accountID, id)
	return expectAffected(res, err)
}

// UpdateGitClone records the post-create git auto-clone status (and
// failure reason, if any) on the sandbox row. Account-scoped; called by
// the git-clone hook as it moves through cloning → done/failed.
func (s *pgSandboxStore) UpdateGitClone(ctx context.Context, accountID, id, status, errMsg string) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE sandboxes SET git_clone_status = $1, git_clone_error = $2, updated_at = NOW()
		 WHERE account_id = $3 AND id = $4`,
		status, errMsg, accountID, id)
	return expectAffected(res, err)
}

func (s *pgSandboxStore) UpdatePlacement(ctx context.Context, id string, clusterID, nodeID string) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE sandboxes SET cluster_id = $1, node_id = $2, updated_at = NOW()
		 WHERE id = $3`, nilIfEmpty(clusterID), nilIfEmpty(nodeID), id)
	return expectAffected(res, err)
}

func (s *pgSandboxStore) UpdateLastActivity(ctx context.Context, id string, ts time.Time) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE sandboxes SET last_activity = $1 WHERE id = $2`, ts, id)
	return expectAffected(res, err)
}

// ListIdle returns rows in `state` whose last_activity is older than the
// per-row threshold column (`auto_stop_minutes` or `auto_archive_minutes`).
// The column name is validated against a small allow-list so callers
// can't smuggle SQL through; anything else returns an empty slice.
func (s *pgSandboxStore) ListIdle(ctx context.Context, state models.SandboxState, thresholdColumn string, now time.Time) ([]*models.Sandbox, error) {
	switch thresholdColumn {
	case "auto_stop_minutes", "auto_archive_minutes":
	default:
		return nil, nil
	}
	out := []*models.Sandbox{}
	q := `SELECT ` + sandboxColumns + ` FROM sandboxes
	      WHERE state = $1 AND ` + thresholdColumn + ` > 0
	        AND last_activity < ($2::timestamptz - (` + thresholdColumn + ` || ' minutes')::interval)
	      ORDER BY last_activity ASC LIMIT 200`
	if err := sqlx.SelectContext(ctx, s.ext, &out, q, string(state), now); err != nil {
		return nil, translate(err)
	}
	return out, nil
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
