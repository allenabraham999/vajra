package master

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// DefaultReconcileInterval is the tick spacing if the caller passes zero
// to NewReconciler. 60s matches the target cadence in CLAUDE.md and is
// much slower than the agent's own heartbeat (so we don't race them).
const DefaultReconcileInterval = 60 * time.Second

// AgentSandbox is the minimal view of a sandbox the agent reports. We
// keep this struct narrow on purpose — anything richer should go through
// the agent's typed API, not the reconcile loop.
type AgentSandbox struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// AgentLister is the subset of agent calls the reconciler depends on.
// The HTTP-backed implementation lives elsewhere; tests use a fake.
type AgentLister interface {
	ListSandboxes(ctx context.Context, node *models.Node) ([]AgentSandbox, error)
	DestroySandbox(ctx context.Context, node *models.Node, sandboxID string) error
}

// Reconciler walks every active node on a fixed interval and brings the
// database back into agreement with what the agent actually reports. It
// handles three drift cases — orphan, ghost, state mismatch — described
// in the methods below.
type Reconciler struct {
	store    store.Store
	agents   AgentLister
	logger   *slog.Logger
	interval time.Duration
	// running guards against overlapping reconcile passes. One pass that
	// runs long must not be re-entered by the next tick — that would
	// double up DestroySandbox calls and racy UpdateState writes.
	running sync.Mutex
}

// NewReconciler builds a Reconciler. If interval is zero,
// DefaultReconcileInterval is used. logger may be nil — slog.Default()
// is substituted.
func NewReconciler(s store.Store, agents AgentLister, logger *slog.Logger, interval time.Duration) *Reconciler {
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		store:    s,
		agents:   agents,
		logger:   logger,
		interval: interval,
	}
}

// Run drives the reconcile loop until ctx is cancelled. Each tick is
// wrapped in a recover so a panic in one node's pass doesn't kill the
// loop — the master keeps running, and the next tick retries.
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tickOnce(ctx)
		}
	}
}

// tickOnce runs one reconcile pass. Exported as a hook for tests so they
// can drive the loop deterministically without spinning a real ticker.
func (r *Reconciler) tickOnce(ctx context.Context) {
	// TryLock — if a previous tick is still running, skip this one.
	if !r.running.TryLock() {
		r.logger.Warn("reconcile: previous tick still running, skipping")
		return
	}
	defer r.running.Unlock()

	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("reconcile: panic recovered", "panic", fmt.Sprintf("%v", rec))
		}
	}()

	r.reconcile(ctx)
}

// reconcile is the body of a tick: list nodes, then process each one.
// Failure on any single node is logged and skipped — other nodes still
// reconcile in the same pass.
func (r *Reconciler) reconcile(ctx context.Context) {
	nodes, err := r.store.Nodes().List(ctx, store.ListOpts{Limit: 1000})
	if err != nil {
		r.logger.Error("reconcile: list nodes", "err", err)
		return
	}
	for _, node := range nodes {
		if node.State != models.NodeStateActive {
			continue
		}
		if err := r.reconcileNode(ctx, node); err != nil {
			// A failure on one node never aborts the whole pass.
			r.logger.Error("reconcile: node pass failed",
				"node_id", node.ID, "err", err)
			continue
		}
	}
}

// reconcileNode compares the agent's sandbox list against the DB for one
// node and resolves three classes of drift.
func (r *Reconciler) reconcileNode(ctx context.Context, node *models.Node) error {
	agentSandboxes, err := r.agents.ListSandboxes(ctx, node)
	if err != nil {
		return fmt.Errorf("list agent sandboxes: %w", err)
	}
	dbSandboxes, err := r.store.Sandboxes().ListByNode(ctx, node.ID, store.ListOpts{Limit: 1000})
	if err != nil {
		return fmt.Errorf("list db sandboxes: %w", err)
	}

	agentByID := make(map[string]AgentSandbox, len(agentSandboxes))
	for _, s := range agentSandboxes {
		agentByID[s.ID] = s
	}
	dbByID := make(map[string]*models.Sandbox, len(dbSandboxes))
	for _, s := range dbSandboxes {
		dbByID[s.ID] = s
	}

	r.handleOrphans(ctx, node, agentByID, dbByID)
	r.handleGhosts(ctx, node, agentByID, dbByID)
	r.handleMismatches(ctx, node, agentByID, dbByID)
	return nil
}

// handleOrphans: agent has the sandbox, DB does not. Best-effort destroy
// on the agent side — if the call fails, log and move on; the next tick
// will retry.
func (r *Reconciler) handleOrphans(ctx context.Context, node *models.Node, agentByID map[string]AgentSandbox, dbByID map[string]*models.Sandbox) {
	for id := range agentByID {
		if _, ok := dbByID[id]; ok {
			continue
		}
		r.logger.Warn("reconcile: orphan sandbox",
			"op", "orphan",
			"node_id", node.ID,
			"sandbox_id", id,
		)
		if err := r.agents.DestroySandbox(ctx, node, id); err != nil {
			r.logger.Error("reconcile: destroy orphan failed",
				"op", "orphan", "node_id", node.ID, "sandbox_id", id, "err", err)
		}
	}
}

// handleGhosts: DB has the sandbox (in a non-DESTROYED state), agent does
// not. Mark the DB row DESTROYED. UpdateState is account-scoped, so we
// pull AccountID from the row we already have rather than refetching.
func (r *Reconciler) handleGhosts(ctx context.Context, node *models.Node, agentByID map[string]AgentSandbox, dbByID map[string]*models.Sandbox) {
	for id, sb := range dbByID {
		if _, ok := agentByID[id]; ok {
			continue
		}
		if sb.State == models.SandboxStateDestroyed {
			continue
		}
		r.logger.Warn("reconcile: ghost sandbox",
			"op", "ghost",
			"node_id", node.ID,
			"sandbox_id", id,
			"db_state", string(sb.State),
		)
		if err := r.store.Sandboxes().UpdateState(ctx, sb.AccountID, id, models.SandboxStateDestroyed); err != nil {
			// Some DB-level transitions are illegal (e.g. PENDING →
			// DESTROYED skips intermediate states). The store layer
			// validates with the FSM, so we surface the error and let
			// the next tick try again — by then the row may have moved
			// through STOPPING/STOPPED on its own.
			if !errors.Is(err, store.ErrNotFound) {
				r.logger.Error("reconcile: ghost update failed",
					"op", "ghost", "node_id", node.ID, "sandbox_id", id, "err", err)
			}
		}
	}
}

// handleMismatches: both sides know the sandbox but disagree on state.
// We only resolve the (RUNNING, STOPPED) pair right now; other mappings
// (PAUSED, ARCHIVED, ERROR) are deliberately deferred — they need a
// fuller agent-state vocabulary that the agent doesn't expose yet.
func (r *Reconciler) handleMismatches(ctx context.Context, node *models.Node, agentByID map[string]AgentSandbox, dbByID map[string]*models.Sandbox) {
	for id, sb := range dbByID {
		agentSb, ok := agentByID[id]
		if !ok {
			continue
		}
		if sb.State == models.SandboxStateDestroyed {
			continue // terminal — never overwrite.
		}
		mapped, ok := mapAgentState(agentSb.State)
		if !ok {
			continue // unknown agent state — leave DB alone.
		}
		if mapped == sb.State {
			continue
		}
		r.logger.Info("reconcile: state mismatch",
			"op", "state_mismatch",
			"node_id", node.ID,
			"sandbox_id", id,
			"db_state", string(sb.State),
			"agent_state", agentSb.State,
		)
		// Only act on the DB→STOPPED transition for now; everything else
		// is left for the next pass once we extend the mapper.
		if sb.State == models.SandboxStateRunning && mapped == models.SandboxStateStopped {
			if err := r.store.Sandboxes().UpdateState(ctx, sb.AccountID, id, models.SandboxStateStopped); err != nil {
				r.logger.Error("reconcile: mismatch update failed",
					"op", "state_mismatch", "node_id", node.ID, "sandbox_id", id, "err", err)
			}
		}
	}
}

// mapAgentState narrows the agent's free-form state string to the
// SandboxState constants we care about for reconciliation and create
// polling. Returns ok=false for any unrecognised value so callers can
// skip safely.
func mapAgentState(s string) (models.SandboxState, bool) {
	switch s {
	case "CREATING", "creating":
		return models.SandboxStateCreating, true
	case "RUNNING", "running":
		return models.SandboxStateRunning, true
	case "STOPPED", "stopped":
		return models.SandboxStateStopped, true
	case "ERROR", "error":
		return models.SandboxStateError, true
	default:
		return "", false
	}
}
