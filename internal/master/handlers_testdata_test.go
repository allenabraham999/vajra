package master

import (
	"context"
	"sync"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// handlerStore is a test-only store.Store backed by simple in-memory
// maps. The scheduler/reconciler tests use a thinner fakeStore that
// covers a few substores; the handler suite exercises all of them, so
// we keep this implementation separate to avoid bloating the existing
// fake.
//
// Concurrency is guarded by one big sync.Mutex. That's fine for tests
// (which never race) and keeps the fake easy to reason about.
type handlerStore struct {
	mu sync.Mutex

	accounts    map[string]*models.Account
	emailIdx    map[string]string // email → accountID
	apiKeys     map[string]*models.APIKey
	keyHashIdx  map[string]string // keyHash → keyID
	clusters    map[string]*models.Cluster
	nodes       map[string]*models.Node
	sandboxes   map[string]*models.Sandbox
	snapshots   map[string]*models.Snapshot
	templates   map[string]*models.Template
	operations  map[string]*models.Operation
	shareLinks  map[string]*models.ShareLink
	tokenIdx    map[string]string // token_hash → share id

	pingErr error
}

// newHandlerStore returns an empty handlerStore ready for use.
func newHandlerStore() *handlerStore {
	return &handlerStore{
		accounts:   map[string]*models.Account{},
		emailIdx:   map[string]string{},
		apiKeys:    map[string]*models.APIKey{},
		keyHashIdx: map[string]string{},
		clusters:   map[string]*models.Cluster{},
		nodes:      map[string]*models.Node{},
		sandboxes:  map[string]*models.Sandbox{},
		snapshots:  map[string]*models.Snapshot{},
		templates:  map[string]*models.Template{},
		operations: map[string]*models.Operation{},
		shareLinks: map[string]*models.ShareLink{},
		tokenIdx:   map[string]string{},
	}
}

func (h *handlerStore) Accounts() store.AccountStore     { return &hsAccount{h: h} }
func (h *handlerStore) APIKeys() store.APIKeyStore       { return &hsAPIKey{h: h} }
func (h *handlerStore) Clusters() store.ClusterStore     { return &hsCluster{h: h} }
func (h *handlerStore) Nodes() store.NodeStore           { return &hsNode{h: h} }
func (h *handlerStore) Sandboxes() store.SandboxStore    { return &hsSandbox{h: h} }
func (h *handlerStore) Snapshots() store.SnapshotStore   { return &hsSnapshot{h: h} }
func (h *handlerStore) Templates() store.TemplateStore   { return &hsTemplate{h: h} }
func (h *handlerStore) Operations() store.OperationStore { return &hsOperation{h: h} }
func (h *handlerStore) ShareLinks() store.ShareLinkStore { return &hsShareLink{h: h} }
func (h *handlerStore) Ping(context.Context) error       { return h.pingErr }
func (h *handlerStore) Close() error                     { return nil }

// WithTx in the fake just runs fn against the same store — there is no
// real transaction, but rollback semantics are not tested separately.
func (h *handlerStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return fn(h)
}

type hsAccount struct{ h *handlerStore }

func (s *hsAccount) Create(_ context.Context, a *models.Account) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if _, ok := s.h.emailIdx[a.Email]; ok {
		return store.ErrConflict
	}
	cp := *a
	s.h.accounts[a.ID] = &cp
	s.h.emailIdx[a.Email] = a.ID
	return nil
}
func (s *hsAccount) GetByID(_ context.Context, id string) (*models.Account, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if a, ok := s.h.accounts[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, store.ErrNotFound
}
func (s *hsAccount) GetByEmail(_ context.Context, email string) (*models.Account, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	id, ok := s.h.emailIdx[email]
	if !ok {
		return nil, store.ErrNotFound
	}
	a := s.h.accounts[id]
	cp := *a
	return &cp, nil
}
func (s *hsAccount) List(context.Context, store.ListOpts) ([]*models.Account, error) { return nil, nil }
func (s *hsAccount) Delete(_ context.Context, id string) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	delete(s.h.accounts, id)
	return nil
}

type hsAPIKey struct{ h *handlerStore }

func (s *hsAPIKey) Create(_ context.Context, k *models.APIKey) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if _, ok := s.h.keyHashIdx[k.KeyHash]; ok {
		return store.ErrConflict
	}
	cp := *k
	s.h.apiKeys[k.ID] = &cp
	s.h.keyHashIdx[k.KeyHash] = k.ID
	return nil
}
func (s *hsAPIKey) GetByID(_ context.Context, accountID, id string) (*models.APIKey, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if k, ok := s.h.apiKeys[id]; ok && k.AccountID == accountID {
		cp := *k
		return &cp, nil
	}
	return nil, store.ErrNotFound
}
func (s *hsAPIKey) GetByHash(_ context.Context, hash string) (*models.APIKey, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	id, ok := s.h.keyHashIdx[hash]
	if !ok {
		return nil, store.ErrNotFound
	}
	k := s.h.apiKeys[id]
	cp := *k
	return &cp, nil
}
func (s *hsAPIKey) ListByAccount(_ context.Context, accountID string, _ store.ListOpts) ([]*models.APIKey, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	out := []*models.APIKey{}
	for _, k := range s.h.apiKeys {
		if k.AccountID == accountID {
			cp := *k
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *hsAPIKey) Delete(_ context.Context, accountID, id string) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if k, ok := s.h.apiKeys[id]; ok && k.AccountID == accountID {
		delete(s.h.apiKeys, id)
		delete(s.h.keyHashIdx, k.KeyHash)
		return nil
	}
	return store.ErrNotFound
}

type hsCluster struct{ h *handlerStore }

func (s *hsCluster) Create(_ context.Context, c *models.Cluster) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	cp := *c
	s.h.clusters[c.ID] = &cp
	return nil
}
func (s *hsCluster) GetByID(_ context.Context, id string) (*models.Cluster, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if c, ok := s.h.clusters[id]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, store.ErrNotFound
}
func (s *hsCluster) List(context.Context, store.ListOpts) ([]*models.Cluster, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	out := []*models.Cluster{}
	for _, c := range s.h.clusters {
		cp := *c
		out = append(out, &cp)
	}
	return out, nil
}
func (s *hsCluster) UpdateState(_ context.Context, id string, state models.ClusterState) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if c, ok := s.h.clusters[id]; ok {
		c.State = state
		return nil
	}
	return store.ErrNotFound
}
func (s *hsCluster) Delete(context.Context, string) error { return nil }

type hsNode struct{ h *handlerStore }

func (s *hsNode) Create(_ context.Context, n *models.Node) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	cp := *n
	s.h.nodes[n.ID] = &cp
	return nil
}
func (s *hsNode) GetByID(_ context.Context, id string) (*models.Node, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if n, ok := s.h.nodes[id]; ok {
		cp := *n
		return &cp, nil
	}
	return nil, store.ErrNotFound
}
func (s *hsNode) ListByCluster(_ context.Context, clusterID string, _ store.ListOpts) ([]*models.Node, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	out := []*models.Node{}
	for _, n := range s.h.nodes {
		if n.ClusterID == clusterID {
			cp := *n
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *hsNode) List(context.Context, store.ListOpts) ([]*models.Node, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	out := []*models.Node{}
	for _, n := range s.h.nodes {
		cp := *n
		out = append(out, &cp)
	}
	return out, nil
}
func (s *hsNode) UpdateState(_ context.Context, id string, st models.NodeState) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if n, ok := s.h.nodes[id]; ok {
		n.State = st
		return nil
	}
	return store.ErrNotFound
}
func (s *hsNode) UpdateUsage(_ context.Context, id string, u models.NodeUsage) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if n, ok := s.h.nodes[id]; ok {
		n.UsedResources = u
		return nil
	}
	return store.ErrNotFound
}
func (s *hsNode) UpdateHeartbeat(_ context.Context, id string, ts time.Time) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if n, ok := s.h.nodes[id]; ok {
		n.LastHeartbeat = ts
		return nil
	}
	return store.ErrNotFound
}
func (s *hsNode) UpdateConfig(_ context.Context, id, hostname, ip string, capacity models.NodeCapacity, state models.NodeState) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if n, ok := s.h.nodes[id]; ok {
		n.Hostname = hostname
		n.IP = ip
		n.Capacity = capacity
		n.State = state
		return nil
	}
	return store.ErrNotFound
}
func (s *hsNode) Delete(context.Context, string) error { return nil }

type hsSandbox struct{ h *handlerStore }

func (s *hsSandbox) Create(_ context.Context, sb *models.Sandbox) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	cp := *sb
	s.h.sandboxes[sb.ID] = &cp
	return nil
}
func (s *hsSandbox) GetByID(_ context.Context, accountID, id string) (*models.Sandbox, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if sb, ok := s.h.sandboxes[id]; ok && sb.AccountID == accountID {
		cp := *sb
		return &cp, nil
	}
	return nil, store.ErrNotFound
}
func (s *hsSandbox) GetByIDUnscoped(_ context.Context, id string) (*models.Sandbox, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if sb, ok := s.h.sandboxes[id]; ok {
		cp := *sb
		return &cp, nil
	}
	return nil, store.ErrNotFound
}
func (s *hsSandbox) ListByAccount(_ context.Context, accountID string, _ store.ListOpts) ([]*models.Sandbox, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	out := []*models.Sandbox{}
	for _, sb := range s.h.sandboxes {
		if sb.AccountID == accountID {
			cp := *sb
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *hsSandbox) ListByNode(_ context.Context, nodeID string, _ store.ListOpts) ([]*models.Sandbox, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	out := []*models.Sandbox{}
	for _, sb := range s.h.sandboxes {
		if sb.NodeID != nil && *sb.NodeID == nodeID {
			cp := *sb
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *hsSandbox) ListByState(context.Context, models.SandboxState, store.ListOpts) ([]*models.Sandbox, error) {
	return nil, nil
}
func (s *hsSandbox) UpdateState(_ context.Context, accountID, id string, state models.SandboxState) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if sb, ok := s.h.sandboxes[id]; ok && sb.AccountID == accountID {
		sb.State = state
		sb.UpdatedAt = time.Now().UTC()
		return nil
	}
	return store.ErrNotFound
}
func (s *hsSandbox) UpdatePlacement(_ context.Context, id string, clusterID, nodeID string) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if sb, ok := s.h.sandboxes[id]; ok {
		c := clusterID
		n := nodeID
		sb.ClusterID = &c
		sb.NodeID = &n
		return nil
	}
	return store.ErrNotFound
}
func (s *hsSandbox) Delete(_ context.Context, accountID, id string) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if sb, ok := s.h.sandboxes[id]; ok && sb.AccountID == accountID {
		delete(s.h.sandboxes, id)
		return nil
	}
	return store.ErrNotFound
}

type hsSnapshot struct{ h *handlerStore }

func (s *hsSnapshot) Create(_ context.Context, sn *models.Snapshot) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	cp := *sn
	s.h.snapshots[sn.ID] = &cp
	return nil
}
func (s *hsSnapshot) GetByID(_ context.Context, accountID, id string) (*models.Snapshot, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if sn, ok := s.h.snapshots[id]; ok && sn.AccountID == accountID {
		cp := *sn
		return &cp, nil
	}
	return nil, store.ErrNotFound
}
func (s *hsSnapshot) ListBySandbox(_ context.Context, accountID, sandboxID string, _ store.ListOpts) ([]*models.Snapshot, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	out := []*models.Snapshot{}
	for _, sn := range s.h.snapshots {
		if sn.AccountID == accountID && sn.SandboxID == sandboxID {
			cp := *sn
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *hsSnapshot) ListByAccount(_ context.Context, accountID string, _ store.ListOpts) ([]*models.Snapshot, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	out := []*models.Snapshot{}
	for _, sn := range s.h.snapshots {
		if sn.AccountID == accountID {
			cp := *sn
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *hsSnapshot) ListByNode(context.Context, string, store.ListOpts) ([]*models.Snapshot, error) {
	return nil, nil
}
func (s *hsSnapshot) Delete(_ context.Context, accountID, id string) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if sn, ok := s.h.snapshots[id]; ok && sn.AccountID == accountID {
		delete(s.h.snapshots, id)
		return nil
	}
	return store.ErrNotFound
}

type hsTemplate struct{ h *handlerStore }

func (s *hsTemplate) Create(_ context.Context, t *models.Template) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	cp := *t
	s.h.templates[t.ID] = &cp
	return nil
}
func (s *hsTemplate) GetByID(_ context.Context, accountID, id string) (*models.Template, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if t, ok := s.h.templates[id]; ok && t.AccountID == accountID {
		cp := *t
		return &cp, nil
	}
	return nil, store.ErrNotFound
}
func (s *hsTemplate) GetByHash(_ context.Context, hash string) (*models.Template, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	for _, t := range s.h.templates {
		if t.Hash == hash {
			cp := *t
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}
func (s *hsTemplate) ListByAccount(_ context.Context, accountID string, _ store.ListOpts) ([]*models.Template, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	out := []*models.Template{}
	for _, t := range s.h.templates {
		if t.AccountID == accountID {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *hsTemplate) Delete(context.Context, string, string) error { return nil }

type hsOperation struct{ h *handlerStore }

func (s *hsOperation) Create(_ context.Context, op *models.Operation) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	cp := *op
	s.h.operations[op.ID] = &cp
	return nil
}
func (s *hsOperation) GetByID(_ context.Context, accountID, id string) (*models.Operation, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if op, ok := s.h.operations[id]; ok && op.AccountID == accountID {
		cp := *op
		return &cp, nil
	}
	return nil, store.ErrNotFound
}
func (s *hsOperation) ListBySandbox(context.Context, string, string, store.ListOpts) ([]*models.Operation, error) {
	return nil, nil
}
func (s *hsOperation) ListByAccount(context.Context, string, store.ListOpts) ([]*models.Operation, error) {
	return nil, nil
}
func (s *hsOperation) UpdateStatus(_ context.Context, id string, status models.OperationStatus, errMsg *string, completedAt *time.Time) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if op, ok := s.h.operations[id]; ok {
		op.Status = status
		op.Error = errMsg
		op.CompletedAt = completedAt
		return nil
	}
	return store.ErrNotFound
}

type hsShareLink struct{ h *handlerStore }

func (s *hsShareLink) Create(_ context.Context, sl *models.ShareLink) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if _, exists := s.h.tokenIdx[sl.TokenHash]; exists {
		return store.ErrConflict
	}
	cp := *sl
	s.h.shareLinks[sl.ID] = &cp
	s.h.tokenIdx[sl.TokenHash] = sl.ID
	return nil
}
func (s *hsShareLink) GetByID(_ context.Context, accountID, id string) (*models.ShareLink, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if sl, ok := s.h.shareLinks[id]; ok && sl.AccountID == accountID {
		cp := *sl
		return &cp, nil
	}
	return nil, store.ErrNotFound
}
func (s *hsShareLink) GetByHash(_ context.Context, tokenHash string) (*models.ShareLink, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	id, ok := s.h.tokenIdx[tokenHash]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *s.h.shareLinks[id]
	return &cp, nil
}
func (s *hsShareLink) ListBySandbox(_ context.Context, accountID, sandboxID string, _ store.ListOpts) ([]*models.ShareLink, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	out := []*models.ShareLink{}
	for _, sl := range s.h.shareLinks {
		if sl.AccountID == accountID && sl.SandboxID == sandboxID {
			cp := *sl
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *hsShareLink) Revoke(_ context.Context, accountID, id string, at time.Time) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if sl, ok := s.h.shareLinks[id]; ok && sl.AccountID == accountID {
		t := at
		sl.RevokedAt = &t
		return nil
	}
	return store.ErrNotFound
}
