package store

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/allenabraham999/vajra/internal/models"
)

type pgClusterStore struct{ ext sqlx.ExtContext }

const clusterColumns = `id, name, region, state, created_at`

func (s *pgClusterStore) Create(ctx context.Context, c *models.Cluster) error {
	const q = `INSERT INTO clusters (` + clusterColumns + `)
	           VALUES (:id, :name, :region, :state, :created_at)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, c)
	return translate(err)
}

func (s *pgClusterStore) GetByID(ctx context.Context, id string) (*models.Cluster, error) {
	var c models.Cluster
	err := sqlx.GetContext(ctx, s.ext, &c,
		`SELECT `+clusterColumns+` FROM clusters WHERE id = $1`, id)
	if err != nil {
		return nil, translate(err)
	}
	return &c, nil
}

func (s *pgClusterStore) List(ctx context.Context, opts ListOpts) ([]*models.Cluster, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Cluster{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+clusterColumns+` FROM clusters
		 ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgClusterStore) UpdateState(ctx context.Context, id string, state models.ClusterState) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE clusters SET state = $1 WHERE id = $2`, string(state), id)
	return expectAffected(res, err)
}

func (s *pgClusterStore) Delete(ctx context.Context, id string) error {
	res, err := s.ext.ExecContext(ctx, `DELETE FROM clusters WHERE id = $1`, id)
	return expectAffected(res, err)
}

type pgNodeStore struct{ ext sqlx.ExtContext }

const nodeColumns = `id, cluster_id, hostname, ip, state, capacity, used_resources, last_heartbeat`

func (s *pgNodeStore) Create(ctx context.Context, n *models.Node) error {
	const q = `INSERT INTO nodes (` + nodeColumns + `)
	           VALUES (:id, :cluster_id, :hostname, :ip, :state, :capacity, :used_resources, :last_heartbeat)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, n)
	return translate(err)
}

func (s *pgNodeStore) GetByID(ctx context.Context, id string) (*models.Node, error) {
	var n models.Node
	err := sqlx.GetContext(ctx, s.ext, &n,
		`SELECT `+nodeColumns+` FROM nodes WHERE id = $1`, id)
	if err != nil {
		return nil, translate(err)
	}
	return &n, nil
}

func (s *pgNodeStore) ListByCluster(ctx context.Context, clusterID string, opts ListOpts) ([]*models.Node, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Node{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+nodeColumns+` FROM nodes WHERE cluster_id = $1
		 ORDER BY hostname ASC LIMIT $2 OFFSET $3`, clusterID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgNodeStore) List(ctx context.Context, opts ListOpts) ([]*models.Node, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Node{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+nodeColumns+` FROM nodes
		 ORDER BY hostname ASC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgNodeStore) UpdateState(ctx context.Context, id string, state models.NodeState) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE nodes SET state = $1 WHERE id = $2`, string(state), id)
	return expectAffected(res, err)
}

func (s *pgNodeStore) UpdateUsage(ctx context.Context, id string, usage models.NodeUsage) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE nodes SET used_resources = $1 WHERE id = $2`, usage, id)
	return expectAffected(res, err)
}

func (s *pgNodeStore) UpdateHeartbeat(ctx context.Context, id string, ts time.Time) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE nodes SET last_heartbeat = $1 WHERE id = $2`, ts, id)
	return expectAffected(res, err)
}

func (s *pgNodeStore) UpdateConfig(ctx context.Context, id, hostname, ip string, capacity models.NodeCapacity, state models.NodeState) error {
	res, err := s.ext.ExecContext(ctx,
		`UPDATE nodes SET hostname = $1, ip = $2, capacity = $3, state = $4 WHERE id = $5`,
		hostname, ip, capacity, string(state), id)
	return expectAffected(res, err)
}

func (s *pgNodeStore) Delete(ctx context.Context, id string) error {
	res, err := s.ext.ExecContext(ctx, `DELETE FROM nodes WHERE id = $1`, id)
	return expectAffected(res, err)
}

type pgTemplateStore struct{ ext sqlx.ExtContext }

const templateColumns = `id, account_id, name, version, hash, rootfs_path, kernel_path, snapshot_path, created_at`

func (s *pgTemplateStore) Create(ctx context.Context, t *models.Template) error {
	const q = `INSERT INTO templates (` + templateColumns + `)
	           VALUES (:id, :account_id, :name, :version, :hash, :rootfs_path, :kernel_path, :snapshot_path, :created_at)`
	_, err := sqlx.NamedExecContext(ctx, s.ext, q, t)
	return translate(err)
}

func (s *pgTemplateStore) GetByID(ctx context.Context, accountID, id string) (*models.Template, error) {
	var t models.Template
	err := sqlx.GetContext(ctx, s.ext, &t,
		`SELECT `+templateColumns+` FROM templates WHERE account_id = $1 AND id = $2`,
		accountID, id)
	if err != nil {
		return nil, translate(err)
	}
	return &t, nil
}

func (s *pgTemplateStore) GetByHash(ctx context.Context, hash string) (*models.Template, error) {
	var t models.Template
	err := sqlx.GetContext(ctx, s.ext, &t,
		`SELECT `+templateColumns+` FROM templates WHERE hash = $1
		 ORDER BY created_at ASC LIMIT 1`, hash)
	if err != nil {
		return nil, translate(err)
	}
	return &t, nil
}

func (s *pgTemplateStore) ListByAccount(ctx context.Context, accountID string, opts ListOpts) ([]*models.Template, error) {
	limit, offset := applyListDefaults(opts)
	out := []*models.Template{}
	err := sqlx.SelectContext(ctx, s.ext, &out,
		`SELECT `+templateColumns+` FROM templates WHERE account_id = $1
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, accountID, limit, offset)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

func (s *pgTemplateStore) Delete(ctx context.Context, accountID, id string) error {
	res, err := s.ext.ExecContext(ctx,
		`DELETE FROM templates WHERE account_id = $1 AND id = $2`, accountID, id)
	return expectAffected(res, err)
}
