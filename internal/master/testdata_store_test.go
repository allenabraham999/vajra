package master

import (
	"context"
	"errors"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// fixedNow is a stable clock used by tests so the heartbeat-staleness
// comparison is deterministic.
var fixedNow = time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)

func mkCluster(id, region string, state models.ClusterState) *models.Cluster {
	return &models.Cluster{ID: id, Region: region, State: state}
}

func mkNode(id, clusterID string, state models.NodeState, lastBeat time.Time, totalCPU, totalMemMB, totalDiskGB, usedCPU, usedMemMB, usedDiskGB int) *models.Node {
	return &models.Node{
		ID: id, ClusterID: clusterID, State: state, LastHeartbeat: lastBeat,
		Capacity:      models.NodeCapacity{TotalCPU: totalCPU, TotalMemoryMB: totalMemMB, TotalDiskGB: totalDiskGB},
		UsedResources: models.NodeUsage{UsedCPU: usedCPU, UsedMemoryMB: usedMemMB, UsedDiskGB: usedDiskGB},
	}
}

func mkSandbox(id, accountID string, state models.SandboxState, vcpus, memMB int) *models.Sandbox {
	return &models.Sandbox{
		ID: id, AccountID: accountID, State: state,
		Config: models.SandboxConfig{VCPUs: vcpus, MemoryMB: memMB},
	}
}

func newSchedulerForTest(fs *fakeStore, qp QuotaProvider) *dbScheduler {
	d := NewScheduler(fs, qp)
	d.now = func() time.Time { return fixedNow }
	return d
}

// fakeStore is a minimal store.Store used by scheduler/reconciler tests.
// Substores are returned via embedded pointers so each test can set just
// the data it needs; methods we don't exercise return errUnimplemented.
type fakeStore struct {
	clusters  *fakeClusterStore
	nodes     *fakeNodeStore
	sandboxes *fakeSandboxStore
}

var errUnimplemented = errors.New("fake: method not implemented for these tests")

func newFakeStore() *fakeStore {
	return &fakeStore{
		clusters:  &fakeClusterStore{},
		nodes:     &fakeNodeStore{},
		sandboxes: &fakeSandboxStore{},
	}
}

func (f *fakeStore) Accounts() store.AccountStore     { return nil }
func (f *fakeStore) APIKeys() store.APIKeyStore       { return nil }
func (f *fakeStore) Clusters() store.ClusterStore     { return f.clusters }
func (f *fakeStore) Nodes() store.NodeStore           { return f.nodes }
func (f *fakeStore) Sandboxes() store.SandboxStore    { return f.sandboxes }
func (f *fakeStore) Snapshots() store.SnapshotStore   { return nil }
func (f *fakeStore) Templates() store.TemplateStore   { return nil }
func (f *fakeStore) Operations() store.OperationStore { return nil }
func (f *fakeStore) ShareLinks() store.ShareLinkStore { return nil }
func (f *fakeStore) Usage() store.UsageStore          { return nil }
func (f *fakeStore) Builds() store.BuildStore         { return nil }
func (f *fakeStore) Webhooks() store.WebhookStore     { return nil }
func (f *fakeStore) Ping(context.Context) error       { return nil }
func (f *fakeStore) WithTx(context.Context, func(store.Store) error) error {
	return errUnimplemented
}
func (f *fakeStore) Close() error { return nil }

// fakeClusterStore lets tests inject the List response and an error.
type fakeClusterStore struct {
	list    []*models.Cluster
	listErr error
}

func (f *fakeClusterStore) Create(context.Context, *models.Cluster) error {
	return errUnimplemented
}
func (f *fakeClusterStore) GetByID(context.Context, string) (*models.Cluster, error) {
	return nil, errUnimplemented
}
func (f *fakeClusterStore) List(context.Context, store.ListOpts) ([]*models.Cluster, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}
func (f *fakeClusterStore) UpdateState(context.Context, string, models.ClusterState) error {
	return errUnimplemented
}
func (f *fakeClusterStore) Delete(context.Context, string) error { return errUnimplemented }

// fakeNodeStore stores nodes per cluster and globally.
type fakeNodeStore struct {
	byCluster map[string][]*models.Node
	all       []*models.Node
	listErr   error
}

func (f *fakeNodeStore) Create(context.Context, *models.Node) error  { return errUnimplemented }
func (f *fakeNodeStore) GetByID(context.Context, string) (*models.Node, error) {
	return nil, errUnimplemented
}
func (f *fakeNodeStore) ListByCluster(_ context.Context, clusterID string, _ store.ListOpts) ([]*models.Node, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byCluster[clusterID], nil
}
func (f *fakeNodeStore) List(context.Context, store.ListOpts) ([]*models.Node, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.all, nil
}
func (f *fakeNodeStore) UpdateState(context.Context, string, models.NodeState) error {
	return errUnimplemented
}
func (f *fakeNodeStore) UpdateUsage(context.Context, string, models.NodeUsage) error {
	return errUnimplemented
}
func (f *fakeNodeStore) UpdateHeartbeat(context.Context, string, time.Time) error {
	return errUnimplemented
}
func (f *fakeNodeStore) UpdateConfig(context.Context, string, string, string, models.NodeCapacity, models.NodeState) error {
	return errUnimplemented
}
func (f *fakeNodeStore) Delete(context.Context, string) error { return errUnimplemented }

// fakeSandboxStore is in-memory and supports just the calls the
// scheduler and reconciler use (plus UpdateState for reconciler tests).
// Update calls are recorded so tests can assert on them.
type fakeSandboxStore struct {
	byAccount map[string][]*models.Sandbox
	byNode    map[string][]*models.Sandbox
	byID      map[string]*models.Sandbox
	listErr   error

	updateStateErr   error
	updateStateCalls []sandboxStateUpdate
}

type sandboxStateUpdate struct {
	AccountID string
	ID        string
	State     models.SandboxState
}

func (f *fakeSandboxStore) Create(context.Context, *models.Sandbox) error { return errUnimplemented }
func (f *fakeSandboxStore) GetByID(_ context.Context, accountID, id string) (*models.Sandbox, error) {
	if sb, ok := f.byID[id]; ok && sb.AccountID == accountID {
		return sb, nil
	}
	return nil, store.ErrNotFound
}
func (f *fakeSandboxStore) GetByIDUnscoped(_ context.Context, id string) (*models.Sandbox, error) {
	if sb, ok := f.byID[id]; ok {
		return sb, nil
	}
	return nil, store.ErrNotFound
}
func (f *fakeSandboxStore) ListByAccount(_ context.Context, accountID string, _ store.ListOpts) ([]*models.Sandbox, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byAccount[accountID], nil
}
func (f *fakeSandboxStore) ListByNode(_ context.Context, nodeID string, _ store.ListOpts) ([]*models.Sandbox, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byNode[nodeID], nil
}
func (f *fakeSandboxStore) ListByState(context.Context, models.SandboxState, store.ListOpts) ([]*models.Sandbox, error) {
	return nil, errUnimplemented
}
func (f *fakeSandboxStore) UpdateState(_ context.Context, accountID, id string, state models.SandboxState) error {
	f.updateStateCalls = append(f.updateStateCalls, sandboxStateUpdate{
		AccountID: accountID, ID: id, State: state,
	})
	if f.updateStateErr != nil {
		return f.updateStateErr
	}
	if sb, ok := f.byID[id]; ok {
		sb.State = state
	}
	return nil
}
func (f *fakeSandboxStore) UpdatePlacement(context.Context, string, string, string) error {
	return errUnimplemented
}
func (f *fakeSandboxStore) UpdateLastActivity(context.Context, string, time.Time) error {
	return errUnimplemented
}
func (f *fakeSandboxStore) ListIdle(context.Context, models.SandboxState, string, time.Time) ([]*models.Sandbox, error) {
	return nil, errUnimplemented
}
func (f *fakeSandboxStore) Delete(context.Context, string, string) error { return errUnimplemented }
