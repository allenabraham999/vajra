// Package store is the persistence layer for vajra-master. The Store
// interface is the only thing handlers and the scheduler should depend on;
// the Postgres implementation lives in postgres.go and is swappable for
// fakes/mocks in tests.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

// ErrNotFound is returned when a record does not exist OR exists but is
// owned by a different account. Collapsing the two prevents the database
// layer from leaking which IDs are taken across account boundaries.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned when an insert violates a uniqueness constraint
// (duplicate email, duplicate API key hash, etc.).
var ErrConflict = errors.New("store: conflict")

// ListOpts is shared pagination input. Limit <= 0 means "use the
// implementation default"; Offset <= 0 means "from the start". Order is
// always created_at DESC unless a method comment says otherwise.
type ListOpts struct {
	Limit  int
	Offset int
}

// Store is the top-level data-access aggregate. Handlers depend on Store,
// not on the concrete *Postgres type, so the master process can be tested
// against an in-memory fake.
//
// Implementations must be safe for concurrent use.
type Store interface {
	Accounts() AccountStore
	APIKeys() APIKeyStore
	Clusters() ClusterStore
	Nodes() NodeStore
	Sandboxes() SandboxStore
	Snapshots() SnapshotStore
	Templates() TemplateStore
	Operations() OperationStore

	// Ping verifies a working database connection.
	Ping(ctx context.Context) error

	// WithTx runs fn inside a single SQL transaction. The Store passed to
	// fn shares the transaction across every substore call. Returning a
	// non-nil error rolls back; returning nil commits. Nested WithTx is
	// not supported and will return an error.
	WithTx(ctx context.Context, fn func(Store) error) error

	// Close releases pooled connections. Safe to call multiple times.
	Close() error
}

// AccountStore is the persistence interface for accounts. There is no
// account-scoping for accounts themselves; access control happens above
// this layer.
type AccountStore interface {
	Create(ctx context.Context, a *models.Account) error
	GetByID(ctx context.Context, id string) (*models.Account, error)
	GetByEmail(ctx context.Context, email string) (*models.Account, error)
	List(ctx context.Context, opts ListOpts) ([]*models.Account, error)
	Delete(ctx context.Context, id string) error
}

// APIKeyStore manages API key records. GetByHash is the auth hot path —
// implementations should keep a tight, indexed lookup on key_hash.
type APIKeyStore interface {
	Create(ctx context.Context, k *models.APIKey) error
	GetByID(ctx context.Context, accountID, id string) (*models.APIKey, error)
	GetByHash(ctx context.Context, keyHash string) (*models.APIKey, error)
	ListByAccount(ctx context.Context, accountID string, opts ListOpts) ([]*models.APIKey, error)
	Delete(ctx context.Context, accountID, id string) error
}

// ClusterStore is administrative; clusters are not account-scoped.
type ClusterStore interface {
	Create(ctx context.Context, c *models.Cluster) error
	GetByID(ctx context.Context, id string) (*models.Cluster, error)
	List(ctx context.Context, opts ListOpts) ([]*models.Cluster, error)
	UpdateState(ctx context.Context, id string, state models.ClusterState) error
	Delete(ctx context.Context, id string) error
}

// NodeStore is administrative; nodes are not account-scoped. UpdateUsage
// and UpdateHeartbeat are called frequently from the agent reconciler and
// should be cheap.
type NodeStore interface {
	Create(ctx context.Context, n *models.Node) error
	GetByID(ctx context.Context, id string) (*models.Node, error)
	ListByCluster(ctx context.Context, clusterID string, opts ListOpts) ([]*models.Node, error)
	List(ctx context.Context, opts ListOpts) ([]*models.Node, error)
	UpdateState(ctx context.Context, id string, state models.NodeState) error
	UpdateUsage(ctx context.Context, id string, usage models.NodeUsage) error
	UpdateHeartbeat(ctx context.Context, id string, ts time.Time) error
	// UpdateConfig refreshes the mutable identity fields written at
	// register-time: hostname, IP, capacity, and lifecycle state. Used by
	// the agent re-registration path so a node coming back on a new IP
	// or with a different capacity profile updates in place rather than
	// orphaning sandboxes via delete+recreate.
	UpdateConfig(ctx context.Context, id, hostname, ip string, capacity models.NodeCapacity, state models.NodeState) error
	Delete(ctx context.Context, id string) error
}

// SandboxStore reads and writes sandbox records. Every account-facing
// method takes accountID and filters by it; system-internal methods that
// the scheduler/reconciler use (ListByNode, ListByState) skip that filter
// — those are explicitly out of band from user requests.
type SandboxStore interface {
	Create(ctx context.Context, s *models.Sandbox) error
	GetByID(ctx context.Context, accountID, id string) (*models.Sandbox, error)
	ListByAccount(ctx context.Context, accountID string, opts ListOpts) ([]*models.Sandbox, error)
	ListByNode(ctx context.Context, nodeID string, opts ListOpts) ([]*models.Sandbox, error)
	ListByState(ctx context.Context, state models.SandboxState, opts ListOpts) ([]*models.Sandbox, error)
	UpdateState(ctx context.Context, accountID, id string, state models.SandboxState) error
	UpdatePlacement(ctx context.Context, id string, clusterID, nodeID string) error
	Delete(ctx context.Context, accountID, id string) error
}

// SnapshotStore is account-scoped. ListByNode is for agent reconciliation
// (which snapshot blobs are owned by which account on this node).
type SnapshotStore interface {
	Create(ctx context.Context, s *models.Snapshot) error
	GetByID(ctx context.Context, accountID, id string) (*models.Snapshot, error)
	ListBySandbox(ctx context.Context, accountID, sandboxID string, opts ListOpts) ([]*models.Snapshot, error)
	ListByAccount(ctx context.Context, accountID string, opts ListOpts) ([]*models.Snapshot, error)
	ListByNode(ctx context.Context, nodeID string, opts ListOpts) ([]*models.Snapshot, error)
	Delete(ctx context.Context, accountID, id string) error
}

// TemplateStore is account-scoped. GetByHash bypasses scoping by design:
// content-addressable hashes are global identifiers, and looking one up
// reveals nothing about ownership.
type TemplateStore interface {
	Create(ctx context.Context, t *models.Template) error
	GetByID(ctx context.Context, accountID, id string) (*models.Template, error)
	GetByHash(ctx context.Context, hash string) (*models.Template, error)
	ListByAccount(ctx context.Context, accountID string, opts ListOpts) ([]*models.Template, error)
	Delete(ctx context.Context, accountID, id string) error
}

// OperationStore tracks async operations. UpdateStatus is unscoped because
// it's called by internal workers that already proved authorization when
// they enqueued the operation.
type OperationStore interface {
	Create(ctx context.Context, op *models.Operation) error
	GetByID(ctx context.Context, accountID, id string) (*models.Operation, error)
	ListBySandbox(ctx context.Context, accountID, sandboxID string, opts ListOpts) ([]*models.Operation, error)
	ListByAccount(ctx context.Context, accountID string, opts ListOpts) ([]*models.Operation, error)
	UpdateStatus(ctx context.Context, id string, status models.OperationStatus, errMsg *string, completedAt *time.Time) error
}
