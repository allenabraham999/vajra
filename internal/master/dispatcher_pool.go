package master

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/allenabraham999/vajra/internal/models"
)

// AgentPool is a thin keyed cache of *AgentClient. The pool itself is safe
// for concurrent use; clients within it are immutable so no per-key
// locking is needed once instantiated. Handlers reach the dispatcher
// through `pool.ClientFor(node).CreateSandbox(ctx, ...)`.
type AgentPool struct {
	mu      sync.RWMutex
	secret  string
	logger  *slog.Logger
	clients map[string]*AgentClient // key = node ID
	// pinned client IDs bypass the baseURL-drift rebuild. Used by
	// OverrideClient so tests (or unusual deployments) can route a
	// node through a custom URL.
	pinned map[string]bool
}

// NewAgentPool returns an empty pool. sharedSecret is forwarded to every
// AgentClient as its Bearer token; auth.go owns rotating it.
func NewAgentPool(sharedSecret string, logger *slog.Logger) *AgentPool {
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentPool{
		secret:  sharedSecret,
		logger:  logger,
		clients: make(map[string]*AgentClient),
		pinned:  make(map[string]bool),
	}
}

// ClientFor returns (and caches) the *AgentClient for node n. If the node
// IP changes between calls (a node was re-imaged or its address rotated),
// the cached client is replaced with a fresh one.
func (p *AgentPool) ClientFor(node *models.Node) *AgentClient {
	if node == nil {
		return nil
	}
	// Fast path: cached client whose baseURL still matches the current IP.
	wantBase := fmt.Sprintf("http://%s:%d", node.IP, DefaultAgentPort)
	p.mu.RLock()
	if c, ok := p.clients[node.ID]; ok && (p.pinned[node.ID] || c.baseURL == wantBase) {
		p.mu.RUnlock()
		return c
	}
	p.mu.RUnlock()

	// Slow path: build (or rebuild) the client under the write lock.
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[node.ID]; ok && (p.pinned[node.ID] || c.baseURL == wantBase) {
		return c
	}
	c := NewAgentClient(node, p.secret, p.logger)
	p.clients[node.ID] = c
	return c
}

// Forget drops the cached client for nodeID so the next ClientFor call
// rebuilds it. Useful when a node has been quarantined or decommissioned
// and we want to be sure no stale connection lingers.
func (p *AgentPool) Forget(nodeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.clients, nodeID)
}

// OverrideClient pins a hand-built *AgentClient for a node ID so
// ClientFor returns it verbatim regardless of node.IP. Intended for
// tests (and the rare in-process integration use case) where the
// agent's reachable URL doesn't follow the http://<node.IP>:9000 rule.
// Pass nil to drop a previous override.
func (p *AgentPool) OverrideClient(nodeID string, c *AgentClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c == nil {
		delete(p.clients, nodeID)
		delete(p.pinned, nodeID)
		return
	}
	p.clients[nodeID] = c
	p.pinned[nodeID] = true
}

// AgentListerAdapter exposes per-node ListSandboxes / DestroySandbox to
// callers (most notably the reconciler) without forcing this file to
// import internal/agent. The reconciler's AgentLister interface lives in
// reconciler.go, written in parallel; this adapter is shaped to satisfy
// it directly:
//
//	ListSandboxes(ctx, *models.Node) ([]AgentSandbox, error)
//	DestroySandbox(ctx, *models.Node, sandboxID string) error
//
// We adapt the wire shape (AgentSandboxView) into AgentSandbox at the
// boundary so the reconciler stays oblivious to dispatcher internals.
//
// TODO: if reconciler.go's AgentLister signature changes, update
// AgentListerAdapter to match. Today the two are kept in sync by hand.
type AgentListerAdapter struct {
	pool *AgentPool
}

// AsAgentLister returns an adapter the reconciler can call into.
func (p *AgentPool) AsAgentLister() *AgentListerAdapter {
	return &AgentListerAdapter{pool: p}
}

// ListSandboxes calls into the per-node agent's list endpoint. Returns
// the agent's view of every sandbox it currently tracks.
func (a *AgentListerAdapter) ListSandboxes(ctx context.Context, node *models.Node) ([]AgentSandbox, error) {
	views, err := a.pool.ClientFor(node).ListSandboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes on %s: %w", node.ID, err)
	}
	out := make([]AgentSandbox, 0, len(views))
	for _, v := range views {
		out = append(out, AgentSandbox{ID: v.ID, State: v.State})
	}
	return out, nil
}

// DestroySandbox tells the given node's agent to destroy a sandbox, used
// by the reconciler to clean up orphan sandboxes on the agent that master
// has already torn down.
func (a *AgentListerAdapter) DestroySandbox(ctx context.Context, node *models.Node, sandboxID string) error {
	if err := a.pool.ClientFor(node).DestroySandbox(ctx, sandboxID); err != nil {
		return fmt.Errorf("destroy %s on %s: %w", sandboxID, node.ID, err)
	}
	return nil
}
