package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	// pq registers the "postgres" driver as a side-effect import.
	_ "github.com/lib/pq"
)

// Config is the wiring needed to build a *Postgres. Zero values for the
// pool tunables fall back to library defaults; callers should set them
// for any non-trivial deployment.
type Config struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// DefaultConfig returns sensible pool tunables for vajra-master. The
// numbers are picked to match a small Postgres (max_connections ~ 100)
// shared across a handful of master replicas.
func DefaultConfig(dsn string) Config {
	return Config{
		DSN:             dsn,
		MaxOpenConns:    25,
		MaxIdleConns:    10,
		ConnMaxLifetime: 30 * time.Minute,
		ConnMaxIdleTime: 5 * time.Minute,
	}
}

// Postgres is the sqlx-backed Store implementation. The same struct is
// reused for transaction-bound stores: ext points at either the *sqlx.DB
// (root) or a *sqlx.Tx (within WithTx). db is non-nil only on the root
// store, which is how Ping/Close/WithTx detect "am I a transaction".
type Postgres struct {
	db  *sqlx.DB
	ext sqlx.ExtContext
}

// New opens a Postgres connection pool, applies the pool config, and
// verifies connectivity with a Ping. Callers must Close the returned
// store on shutdown.
func New(ctx context.Context, cfg Config) (*Postgres, error) {
	if cfg.DSN == "" {
		return nil, errors.New("store: empty DSN")
	}
	db, err := sqlx.ConnectContext(ctx, "postgres", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	}
	return &Postgres{db: db, ext: db}, nil
}

// NewWithDB wraps an existing *sqlx.DB without touching its pool config.
// Useful for tests that construct their own *sql.DB / *sqlx.DB.
func NewWithDB(db *sqlx.DB) *Postgres {
	return &Postgres{db: db, ext: db}
}

// DB returns the underlying *sqlx.DB. Returns nil when called on a
// transaction-bound Postgres. Use sparingly; preferring the substore
// methods keeps the Store interface honest.
func (p *Postgres) DB() *sqlx.DB { return p.db }

func (p *Postgres) Accounts() AccountStore     { return &pgAccountStore{ext: p.ext} }
func (p *Postgres) APIKeys() APIKeyStore       { return &pgAPIKeyStore{ext: p.ext} }
func (p *Postgres) Clusters() ClusterStore     { return &pgClusterStore{ext: p.ext} }
func (p *Postgres) Nodes() NodeStore           { return &pgNodeStore{ext: p.ext} }
func (p *Postgres) Sandboxes() SandboxStore    { return &pgSandboxStore{ext: p.ext} }
func (p *Postgres) Snapshots() SnapshotStore   { return &pgSnapshotStore{ext: p.ext} }
func (p *Postgres) Templates() TemplateStore   { return &pgTemplateStore{ext: p.ext} }
func (p *Postgres) Operations() OperationStore { return &pgOperationStore{ext: p.ext} }
func (p *Postgres) ShareLinks() ShareLinkStore { return &pgShareLinkStore{ext: p.ext} }

// Ping verifies a working database connection. Errors on a tx-bound store.
func (p *Postgres) Ping(ctx context.Context) error {
	if p.db == nil {
		return errors.New("store: ping not available on transaction-bound store")
	}
	return p.db.PingContext(ctx)
}

// Close releases pooled connections. Safe to call multiple times and on
// transaction-bound stores (no-op).
func (p *Postgres) Close() error {
	if p.db == nil {
		return nil
	}
	return p.db.Close()
}

// WithTx runs fn inside a single Postgres transaction. The Store passed
// to fn shares the transaction across every substore call. Returning a
// non-nil error rolls back; returning nil commits. Nested WithTx is
// rejected — composing inner/outer commits with sql.Tx is a footgun and
// vajra has no current need for it.
func (p *Postgres) WithTx(ctx context.Context, fn func(Store) error) error {
	if p.db == nil {
		return errors.New("store: nested transactions not supported")
	}
	tx, err := p.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	txStore := &Postgres{ext: tx}
	if err := fn(txStore); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(err, fmt.Errorf("store: rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Compile-time check that *Postgres satisfies Store.
var _ Store = (*Postgres)(nil)

// translate maps low-level SQL errors to package-level sentinels so
// callers can use errors.Is(err, ErrNotFound) without depending on
// driver internals.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		switch pqErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", ErrConflict, pqErr.Message)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w: %s", ErrConflict, pqErr.Message)
		}
	}
	return err
}

// expectAffected returns ErrNotFound when an UPDATE/DELETE matched no
// rows, so handlers can return 404 instead of silently treating the call
// as a success against the wrong account.
func expectAffected(res sql.Result, err error) error {
	if err != nil {
		return translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// applyListDefaults caps and defaults the pagination opts. Limit caps at
// 1000 to keep a runaway list from oom-ing master.
func applyListDefaults(opts ListOpts) (limit, offset int) {
	limit = opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	offset = opts.Offset
	if offset < 0 {
		offset = 0
	}
	return
}

// nilIfEmpty returns nil when s is "" so empty strings serialize to SQL
// NULL via sqlx. Used for nullable foreign keys (sandbox.node_id, etc.)
// where "" is not a valid ID.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
