package master

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// opTestStore is a Store implementation whose only working substore is
// Operations(). Every other accessor panics — operation tests must not
// touch them, so a panic here would surface a regression early.
type opTestStore struct {
	ops *opMemStore
}

func newOpTestStore() *opTestStore { return &opTestStore{ops: newOpMemStore()} }

func (s *opTestStore) Accounts() store.AccountStore   { panic("opTestStore: Accounts not implemented") }
func (s *opTestStore) APIKeys() store.APIKeyStore     { panic("opTestStore: APIKeys not implemented") }
func (s *opTestStore) Clusters() store.ClusterStore   { panic("opTestStore: Clusters not implemented") }
func (s *opTestStore) Nodes() store.NodeStore         { panic("opTestStore: Nodes not implemented") }
func (s *opTestStore) Sandboxes() store.SandboxStore  { panic("opTestStore: Sandboxes not implemented") }
func (s *opTestStore) Snapshots() store.SnapshotStore { panic("opTestStore: Snapshots not implemented") }
func (s *opTestStore) Templates() store.TemplateStore { panic("opTestStore: Templates not implemented") }
func (s *opTestStore) ShareLinks() store.ShareLinkStore {
	panic("opTestStore: ShareLinks not implemented")
}
func (s *opTestStore) Operations() store.OperationStore {
	return s.ops
}
func (s *opTestStore) Usage() store.UsageStore    { panic("opTestStore: Usage not implemented") }
func (s *opTestStore) Ping(context.Context) error { return nil }
func (s *opTestStore) WithTx(context.Context, func(store.Store) error) error {
	return errors.New("WithTx not implemented")
}
func (s *opTestStore) Close() error { return nil }

// opMemStore implements store.OperationStore in memory. Concurrency-safe
// because the dispatcher might wrap operations in goroutines — we want
// the fake to behave like Postgres in that respect.
type opMemStore struct {
	mu  sync.Mutex
	ops map[string]*models.Operation
}

func newOpMemStore() *opMemStore { return &opMemStore{ops: make(map[string]*models.Operation)} }

func (m *opMemStore) Create(_ context.Context, op *models.Operation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.ops[op.ID]; dup {
		return store.ErrConflict
	}
	cp := *op
	m.ops[op.ID] = &cp
	return nil
}

func (m *opMemStore) GetByID(_ context.Context, accountID, id string) (*models.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[id]
	if !ok || op.AccountID != accountID {
		return nil, store.ErrNotFound
	}
	cp := *op
	return &cp, nil
}

func (m *opMemStore) ListBySandbox(context.Context, string, string, store.ListOpts) ([]*models.Operation, error) {
	return nil, errors.New("ListBySandbox not implemented")
}

func (m *opMemStore) ListByAccount(context.Context, string, store.ListOpts) ([]*models.Operation, error) {
	return nil, errors.New("ListByAccount not implemented")
}

func (m *opMemStore) UpdateStatus(_ context.Context, id string, status models.OperationStatus, errMsg *string, completedAt *time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[id]
	if !ok {
		return store.ErrNotFound
	}
	op.Status = status
	op.Error = errMsg
	op.CompletedAt = completedAt
	return nil
}

func TestOperationTrackerStartCreatesInProgress(t *testing.T) {
	s := newOpTestStore()
	tr := NewOperationTracker(s)

	id, err := tr.Start(context.Background(), "acct-1", "sb-1", models.OperationTypeCreate)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(id) != 32 {
		t.Errorf("expected 32-char hex id, got %q (len %d)", id, len(id))
	}
	got, err := s.ops.GetByID(context.Background(), "acct-1", id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != models.OperationStatusInProgress {
		t.Errorf("status = %q, want IN_PROGRESS", got.Status)
	}
	if got.SandboxID != "sb-1" || got.AccountID != "acct-1" || got.Type != models.OperationTypeCreate {
		t.Errorf("unexpected row: %+v", got)
	}
	if got.StartedAt.IsZero() {
		t.Error("StartedAt should be set")
	}
	if got.CompletedAt != nil {
		t.Error("CompletedAt should be nil at start")
	}
}

func TestOperationTrackerCompleteSuccess(t *testing.T) {
	s := newOpTestStore()
	tr := NewOperationTracker(s)
	id, err := tr.Start(context.Background(), "acct-1", "sb-1", models.OperationTypeStop)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tr.Complete(context.Background(), id, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, err := s.ops.GetByID(context.Background(), "acct-1", id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != models.OperationStatusCompleted {
		t.Errorf("status = %q, want COMPLETED", got.Status)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt should be non-nil")
	}
	if got.Error != nil {
		t.Errorf("Error should be nil, got %q", *got.Error)
	}
}

func TestOperationTrackerCompleteFailure(t *testing.T) {
	s := newOpTestStore()
	tr := NewOperationTracker(s)
	id, err := tr.Start(context.Background(), "acct-1", "sb-1", models.OperationTypeDestroy)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tr.Complete(context.Background(), id, errors.New("boom")); err != nil {
		t.Fatalf("Complete(err): %v", err)
	}
	got, err := s.ops.GetByID(context.Background(), "acct-1", id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != models.OperationStatusFailed {
		t.Errorf("status = %q, want FAILED", got.Status)
	}
	if got.Error == nil || *got.Error != "boom" {
		t.Errorf("Error = %v, want %q", got.Error, "boom")
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt should be non-nil even on failure")
	}
}

func TestOperationTrackerCompleteTruncatesError(t *testing.T) {
	s := newOpTestStore()
	tr := NewOperationTracker(s)
	id, err := tr.Start(context.Background(), "acct-1", "sb-1", models.OperationTypeSnapshot)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	huge := strings.Repeat("x", 5000)
	if err := tr.Complete(context.Background(), id, errors.New(huge)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, err := s.ops.GetByID(context.Background(), "acct-1", id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Error == nil {
		t.Fatal("Error should be set")
	}
	if len(*got.Error) != errorMessageMaxBytes {
		t.Errorf("Error length = %d, want %d", len(*got.Error), errorMessageMaxBytes)
	}
}

func TestOperationTrackerStartUniqueIDs(t *testing.T) {
	s := newOpTestStore()
	tr := NewOperationTracker(s)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id, err := tr.Start(context.Background(), "acct", "sb", models.OperationTypeCreate)
		if err != nil {
			t.Fatalf("Start[%d]: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
}
