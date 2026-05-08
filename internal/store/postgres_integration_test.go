//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

// integrationDSN points at the docker-compose Postgres by default. Override
// via VAJRA_TEST_DSN if you're running the test against another database.
func integrationDSN() string {
	if v := os.Getenv("VAJRA_TEST_DSN"); v != "" {
		return v
	}
	return "postgres://vajra:vajra@localhost:5432/vajra?sslmode=disable"
}

// migrationsDir resolves the absolute path to the repository's migrations
// directory regardless of where `go test` is invoked from.
func migrationsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd() // .../internal/store
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(wd, "..", "..", "migrations")
}

// setupStore opens a fresh Store with a clean schema. We Down + Up to
// guarantee a known state and so the test is idempotent across runs.
func setupStore(t *testing.T) *Postgres {
	t.Helper()
	ctx := context.Background()
	cfg := DefaultConfig(integrationDSN())
	st, err := New(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unavailable, skipping integration test: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mig, err := NewMigrator(st.DB().DB, "file://"+migrationsDir(t))
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}
	t.Cleanup(func() { _ = mig.Close() })

	if err := mig.Down(); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := mig.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return st
}

func TestIntegration_AccountAndAPIKey(t *testing.T) {
	st := setupStore(t)
	ctx := context.Background()

	acc := &models.Account{
		ID:           "acc-1",
		Email:        "alice@example.com",
		PasswordHash: "hash",
		CreatedAt:    time.Now().UTC(),
	}
	if err := st.Accounts().Create(ctx, acc); err != nil {
		t.Fatalf("create account: %v", err)
	}

	got, err := st.Accounts().GetByEmail(ctx, acc.Email)
	if err != nil {
		t.Fatalf("get by email: %v", err)
	}
	if got.ID != acc.ID {
		t.Fatalf("got %q, want %q", got.ID, acc.ID)
	}

	// Duplicate email must be a conflict.
	dup := *acc
	dup.ID = "acc-2"
	if err := st.Accounts().Create(ctx, &dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict on duplicate email, got %v", err)
	}

	// Account-scoped API key with permissions JSONB roundtrip.
	key := &models.APIKey{
		ID:          "key-1",
		AccountID:   acc.ID,
		KeyHash:     "kh",
		Name:        "default",
		Permissions: models.Permissions{"sandbox:read", "sandbox:write"},
		CreatedAt:   time.Now().UTC(),
	}
	if err := st.APIKeys().Create(ctx, key); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	roundtrip, err := st.APIKeys().GetByHash(ctx, "kh")
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if len(roundtrip.Permissions) != 2 {
		t.Fatalf("permissions roundtrip lost data: %+v", roundtrip.Permissions)
	}

	// Reading another account's key by id must return ErrNotFound, not the row.
	if _, err := st.APIKeys().GetByID(ctx, "other-account", key.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-account read, got %v", err)
	}
}

func TestIntegration_SandboxLifecycle(t *testing.T) {
	st := setupStore(t)
	ctx := context.Background()

	mustCreate(t, st, "acc", "alice@x.com")
	cluster := &models.Cluster{ID: "cl-1", Name: "us-east-1a", Region: "us-east-1", State: models.ClusterStateActive, CreatedAt: time.Now().UTC()}
	if err := st.Clusters().Create(ctx, cluster); err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	node := &models.Node{
		ID: "node-1", ClusterID: cluster.ID, Hostname: "h1", IP: "10.0.0.1",
		State: models.NodeStateActive, LastHeartbeat: time.Now().UTC(),
	}
	if err := st.Nodes().Create(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	tmpl := &models.Template{
		ID: "tmpl-1", AccountID: "acc", Name: "ubuntu", Version: "22.04",
		Hash: "sha256:abc", RootfsPath: "/r", KernelPath: "/k", SnapshotPath: "/s",
		CreatedAt: time.Now().UTC(),
	}
	if err := st.Templates().Create(ctx, tmpl); err != nil {
		t.Fatalf("create template: %v", err)
	}

	now := time.Now().UTC()
	sb := &models.Sandbox{
		ID: "sb-1", Name: "first", AccountID: "acc",
		TemplateID: tmpl.ID, State: models.SandboxStatePending,
		Config:    models.SandboxConfig{VCPUs: 2, MemoryMB: 1024, DiskGB: 10},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.Sandboxes().Create(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	// Placement update — schedules onto cluster+node.
	if err := st.Sandboxes().UpdatePlacement(ctx, sb.ID, cluster.ID, node.ID); err != nil {
		t.Fatalf("update placement: %v", err)
	}
	got, err := st.Sandboxes().GetByID(ctx, "acc", sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.NodeID == nil || *got.NodeID != node.ID {
		t.Fatalf("placement not persisted: %+v", got)
	}

	// State transition.
	if err := st.Sandboxes().UpdateState(ctx, "acc", sb.ID, models.SandboxStateCreating); err != nil {
		t.Fatalf("update state: %v", err)
	}
	got, _ = st.Sandboxes().GetByID(ctx, "acc", sb.ID)
	if got.State != models.SandboxStateCreating {
		t.Fatalf("state did not persist: %s", got.State)
	}

	// Cross-account read isolation.
	if _, err := st.Sandboxes().GetByID(ctx, "other", sb.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound cross-account, got %v", err)
	}

	// Listing by node finds it; listing by state finds it.
	byNode, err := st.Sandboxes().ListByNode(ctx, node.ID, ListOpts{})
	if err != nil || len(byNode) != 1 {
		t.Fatalf("ListByNode = %d, %v", len(byNode), err)
	}
	byState, err := st.Sandboxes().ListByState(ctx, models.SandboxStateCreating, ListOpts{})
	if err != nil || len(byState) != 1 {
		t.Fatalf("ListByState = %d, %v", len(byState), err)
	}
}

func TestIntegration_TransactionRollsBack(t *testing.T) {
	st := setupStore(t)
	ctx := context.Background()
	mustCreate(t, st, "acc", "tx@x.com")

	want := errors.New("intentional")
	err := st.WithTx(ctx, func(s Store) error {
		// Insert is visible inside the tx but must vanish on rollback.
		c := &models.Cluster{ID: "cl-tx", Name: "tx", Region: "r", State: models.ClusterStateActive, CreatedAt: time.Now().UTC()}
		if err := s.Clusters().Create(ctx, c); err != nil {
			t.Fatalf("create cluster in tx: %v", err)
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected sentinel back from WithTx, got %v", err)
	}
	if _, err := st.Clusters().GetByID(ctx, "cl-tx"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rollback didn't take effect: %v", err)
	}
}

func TestIntegration_TransactionCommits(t *testing.T) {
	st := setupStore(t)
	ctx := context.Background()
	mustCreate(t, st, "acc", "tx2@x.com")

	if err := st.WithTx(ctx, func(s Store) error {
		c := &models.Cluster{ID: "cl-cmt", Name: "cmt", Region: "r", State: models.ClusterStateActive, CreatedAt: time.Now().UTC()}
		return s.Clusters().Create(ctx, c)
	}); err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if _, err := st.Clusters().GetByID(ctx, "cl-cmt"); err != nil {
		t.Fatalf("commit didn't persist: %v", err)
	}
}

// mustCreate is a tiny helper for tests that need an account row to satisfy
// foreign keys but don't otherwise care about the value.
func mustCreate(t *testing.T, st Store, id, email string) {
	t.Helper()
	if err := st.Accounts().Create(context.Background(), &models.Account{
		ID: id, Email: email, PasswordHash: "h", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
}
