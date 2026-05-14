// Package master — lifecycle.go: idle auto-stop / auto-archive
// enforcement.
//
// The LifecycleManager runs on a 60-second timer (configurable). Each
// tick it polls Postgres for RUNNING sandboxes whose last_activity is
// older than auto_stop_minutes and dispatches Stop, and for STOPPED
// sandboxes whose last_activity is older than auto_archive_minutes and
// dispatches Archive.
//
// The "last_activity" hot path lives in Redis when configured (key
// SandboxLastActivityKey) so exec / file upload / terminal connect
// signals don't slam Postgres on every operation. The manager writes
// activity back to Postgres opportunistically; the LIST query reads
// directly from Postgres because Redis can't index range scans.
package master

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/allenabraham999/vajra/internal/cache"
	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// DefaultLifecycleInterval is the period between LifecycleManager
// sweeps. The brief asks for 60s.
const DefaultLifecycleInterval = 60 * time.Second

// LifecycleManager owns the auto-stop / auto-archive sweep loop. It
// holds references but no per-row state — all decisions are derived
// from the latest Postgres rows on each tick.
type LifecycleManager struct {
	store    store.Store
	pool     *AgentPool
	cache    cache.Cache
	handlers *Handlers
	logger   *slog.Logger
	interval time.Duration
	now      func() time.Time
}

// NewLifecycleManager wires a manager. handlers is borrowed (not
// owned) — the manager calls the same lifecycle helpers a user-
// initiated stop/archive would, so the side effects (cache, bus,
// webhooks, operation tracker) match exactly.
func NewLifecycleManager(st store.Store, pool *AgentPool, c cache.Cache, h *Handlers, lg *slog.Logger) *LifecycleManager {
	if lg == nil {
		lg = slog.Default()
	}
	if c == nil {
		c = cache.NewNoopCache()
	}
	return &LifecycleManager{
		store:    st,
		pool:     pool,
		cache:    c,
		handlers: h,
		logger:   lg,
		interval: DefaultLifecycleInterval,
		now:      time.Now,
	}
}

// WithInterval overrides the default sweep period. Tests use this to
// run the loop at ms granularity.
func (m *LifecycleManager) WithInterval(d time.Duration) *LifecycleManager {
	if d > 0 {
		m.interval = d
	}
	return m
}

// Run blocks until ctx is cancelled, firing one sweep per interval.
// The first sweep runs immediately so a freshly started master does
// not wait a full interval before honouring policies.
func (m *LifecycleManager) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	m.Sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Sweep(ctx)
		}
	}
}

// Sweep runs one auto-stop + auto-archive pass. Exposed so tests can
// drive a single tick without spinning the goroutine.
func (m *LifecycleManager) Sweep(ctx context.Context) {
	now := m.now().UTC()
	m.sweepAutoStop(ctx, now)
	m.sweepAutoArchive(ctx, now)
}

// sweepAutoStop finds idle RUNNING sandboxes and stops them.
func (m *LifecycleManager) sweepAutoStop(ctx context.Context, now time.Time) {
	rows, err := m.store.Sandboxes().ListIdle(ctx, models.SandboxStateRunning, "auto_stop_minutes", now)
	if err != nil {
		m.logger.Error("lifecycle: list idle running", "err", err)
		return
	}
	for _, sb := range rows {
		if err := m.stopOne(ctx, sb); err != nil {
			m.logger.Warn("lifecycle: auto-stop failed", "sandbox_id", sb.ID, "err", err)
			continue
		}
		m.logger.Info("lifecycle: auto-stopped idle sandbox",
			"sandbox_id", sb.ID, "idle_minutes", int(now.Sub(sb.LastActivity).Minutes()),
			"threshold_minutes", sb.AutoStopMinutes)
	}
}

// sweepAutoArchive finds idle STOPPED sandboxes and archives them.
func (m *LifecycleManager) sweepAutoArchive(ctx context.Context, now time.Time) {
	rows, err := m.store.Sandboxes().ListIdle(ctx, models.SandboxStateStopped, "auto_archive_minutes", now)
	if err != nil {
		m.logger.Error("lifecycle: list idle stopped", "err", err)
		return
	}
	for _, sb := range rows {
		if err := m.archiveOne(ctx, sb); err != nil {
			m.logger.Warn("lifecycle: auto-archive failed", "sandbox_id", sb.ID, "err", err)
			continue
		}
		m.logger.Info("lifecycle: auto-archived idle sandbox",
			"sandbox_id", sb.ID, "idle_minutes", int(now.Sub(sb.LastActivity).Minutes()),
			"threshold_minutes", sb.AutoArchiveMinutes)
	}
}

// stopOne drives one sandbox from RUNNING → STOPPED. We dispatch to the
// agent if a node is still attached; if not (orphaned row), we just
// transition the DB row.
func (m *LifecycleManager) stopOne(ctx context.Context, sb *models.Sandbox) error {
	if sb.NodeID != nil && *sb.NodeID != "" && m.pool != nil {
		node, err := m.store.Nodes().GetByID(ctx, *sb.NodeID)
		if err == nil {
			dispatchCtx, cancel := context.WithTimeout(ctx, dispatchTimeout)
			defer cancel()
			if derr := m.pool.ClientFor(node).StopSandbox(dispatchCtx, sb.ID); derr != nil {
				return derr
			}
		}
	}
	if err := m.store.Sandboxes().UpdateState(ctx, sb.AccountID, sb.ID, models.SandboxStateStopped); err != nil {
		return err
	}
	if m.handlers != nil {
		m.handlers.writeSandboxStateCache(ctx, sb.ID, models.SandboxStateStopped)
		m.handlers.publishStateChange(ctx, sb, models.SandboxStateRunning, models.SandboxStateStopped)
		m.handlers.recordUsageStop(ctx, sb.ID)
	}
	return nil
}

// archiveOne marks a STOPPED sandbox as ARCHIVING then ARCHIVED. We
// don't drive the agent here because the archive-to-S3 path is owned
// by handlers_archive.go's dispatcher; for the lifecycle sweep we
// only flip state and let the agent's background reaper reclaim disk.
func (m *LifecycleManager) archiveOne(ctx context.Context, sb *models.Sandbox) error {
	if err := m.store.Sandboxes().UpdateState(ctx, sb.AccountID, sb.ID, models.SandboxStateArchiving); err != nil {
		return err
	}
	if err := m.store.Sandboxes().UpdateState(ctx, sb.AccountID, sb.ID, models.SandboxStateArchived); err != nil {
		return err
	}
	if m.handlers != nil {
		m.handlers.writeSandboxStateCache(ctx, sb.ID, models.SandboxStateArchived)
		m.handlers.publishStateChange(ctx, sb, models.SandboxStateStopped, models.SandboxStateArchived)
	}
	return nil
}

// TouchActivity records "this sandbox was just used" both in Redis
// (fast path) and Postgres (the indexed column the sweep reads). Each
// write is best-effort; a transient cache failure must not block the
// underlying operation the user invoked.
func (m *LifecycleManager) TouchActivity(ctx context.Context, sandboxID string) {
	now := m.now().UTC()
	if m.cache != nil {
		_ = m.cache.Set(ctx, cache.SandboxLastActivityKey(sandboxID), strconv.FormatInt(now.Unix(), 10), cache.SandboxLastActivityTTL)
	}
	if err := m.store.Sandboxes().UpdateLastActivity(ctx, sandboxID, now); err != nil {
		m.logger.Debug("lifecycle: touch activity", "sandbox_id", sandboxID, "err", err)
	}
}
