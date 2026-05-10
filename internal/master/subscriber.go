// Package master — subscriber.go: long-running goroutine that reacts
// to events published by agents on the bus. Wired only when NATS_URL
// is set; otherwise NoopBus.Subscribe is a no-op and the existing HTTP
// path stays in charge.
package master

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/allenabraham999/vajra/internal/cache"
	"github.com/allenabraham999/vajra/internal/events"
	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// Subscriber bridges the event bus into Postgres + Redis. Heartbeat
// events update Redis immediately so the scheduler reads fresh
// capacity, and we batch-flush usage rows to Postgres every 30s
// rather than writing per-heartbeat — the scheduler's hot path no
// longer needs the DB to be in sync.
type Subscriber struct {
	bus    events.EventBus
	store  store.Store
	cache  cache.Cache
	logger *slog.Logger

	// flushInterval bounds how long an in-memory heartbeat lingers
	// before being persisted. 30s matches the brief.
	flushInterval time.Duration

	mu      sync.Mutex
	pending map[string]*pendingHeartbeat
}

// pendingHeartbeat is the latest in-memory snapshot per node. Replaces
// the per-heartbeat DB write — we flush whatever is in here every
// flushInterval.
type pendingHeartbeat struct {
	usage     models.NodeUsage
	timestamp time.Time
	version   string
}

// NewSubscriber builds a Subscriber. Caller must Subscribe() before
// Run() to actually receive events; main wires those in order.
func NewSubscriber(bus events.EventBus, st store.Store, c cache.Cache, logger *slog.Logger) *Subscriber {
	if logger == nil {
		logger = slog.Default()
	}
	if c == nil {
		c = cache.NewNoopCache()
	}
	return &Subscriber{
		bus:           bus,
		store:         st,
		cache:         c,
		logger:        logger,
		flushInterval: 30 * time.Second,
		pending:       make(map[string]*pendingHeartbeat),
	}
}

// Subscribe wires the subjects we care about. Called once from main
// before Run.
func (s *Subscriber) Subscribe() error {
	if err := s.bus.Subscribe(events.SubjectNodeHeartbeat, s.onHeartbeat); err != nil {
		return err
	}
	if err := s.bus.Subscribe(events.SubjectSandboxStateChanged, s.onStateChanged); err != nil {
		return err
	}
	if err := s.bus.Subscribe(events.SubjectNodeUnhealthy, s.onUnhealthy); err != nil {
		return err
	}
	return nil
}

// Run blocks until ctx is cancelled, flushing batched heartbeats to
// Postgres every flushInterval. NATS itself dispatches each subscribed
// message on its own goroutine, so this loop only handles the timer.
func (s *Subscriber) Run(ctx context.Context) {
	t := time.NewTicker(s.flushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flush(context.Background())
			return
		case <-t.C:
			s.flush(ctx)
		}
	}
}

// onHeartbeat updates Redis immediately and queues the row for the
// next flush. The handler runs on a NATS goroutine so we keep it tight.
func (s *Subscriber) onHeartbeat(_ string, payload []byte) {
	var ev events.NodeHeartbeatEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		s.logger.Debug("subscriber: unmarshal heartbeat", "err", err)
		return
	}
	if ev.NodeID == "" {
		return
	}
	ts := time.Unix(ev.Timestamp, 0).UTC()
	if ev.Timestamp == 0 {
		ts = time.Now().UTC()
	}
	usage := models.NodeUsage{
		UsedCPU:      ev.UsedCPU,
		UsedMemoryMB: ev.UsedMemoryMB,
		UsedDiskGB:   ev.UsedDiskGB,
	}
	// Best-effort: load the row so we know capacity, then write the
	// resources blob. Cache miss here just means scheduler falls
	// through to Postgres next time.
	if node, err := s.store.Nodes().GetByID(context.Background(), ev.NodeID); err == nil {
		node.UsedResources = usage
		node.LastHeartbeat = ts
		payload := nodeResourcesPayload{
			TotalCPU:      node.Capacity.TotalCPU,
			UsedCPU:       usage.UsedCPU,
			TotalMemoryMB: node.Capacity.TotalMemoryMB,
			UsedMemoryMB:  usage.UsedMemoryMB,
			TotalDiskGB:   node.Capacity.TotalDiskGB,
			UsedDiskGB:    usage.UsedDiskGB,
			LastHeartbeat: ts,
		}
		if raw, err := json.Marshal(payload); err == nil {
			_ = s.cache.Set(context.Background(), cache.NodeResourcesKey(ev.NodeID), string(raw), cache.NodeResourcesTTL)
		}
	}
	s.mu.Lock()
	s.pending[ev.NodeID] = &pendingHeartbeat{usage: usage, timestamp: ts, version: ev.Version}
	s.mu.Unlock()
}

// onStateChanged keeps Redis aligned when an agent emits a state
// change directly. Master-driven changes already cache through
// writeSandboxStateCache; this is the corner case where the agent
// transitions a sandbox without master knowing.
func (s *Subscriber) onStateChanged(_ string, payload []byte) {
	var ev events.SandboxStateChangedEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	if ev.SandboxID == "" || ev.NewState == "" {
		return
	}
	if !models.SandboxState(ev.NewState).Valid() {
		return
	}
	_ = s.cache.Set(context.Background(), cache.SandboxStateKey(ev.SandboxID), ev.NewState, cache.SandboxStateTTL)
}

// onUnhealthy logs the agent's distress signal and triggers a single
// reconciliation pass for the affected sandbox by marking it ERROR.
// The reconciler will pick it up from there.
func (s *Subscriber) onUnhealthy(_ string, payload []byte) {
	var ev events.NodeUnhealthyEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	s.logger.Warn("subscriber: node reported unhealthy",
		"node_id", ev.NodeID, "sandbox_id", ev.SandboxID, "error", ev.Error)
	if ev.SandboxID == "" {
		return
	}
	sb, err := s.store.Sandboxes().GetByIDUnscoped(context.Background(), ev.SandboxID)
	if err != nil {
		return
	}
	if err := s.store.Sandboxes().UpdateState(context.Background(), sb.AccountID, sb.ID, models.SandboxStateError); err != nil {
		s.logger.Debug("subscriber: mark error", "err", err, "sandbox_id", sb.ID)
	}
	_ = s.cache.Set(context.Background(), cache.SandboxStateKey(sb.ID), string(models.SandboxStateError), cache.SandboxStateTTL)
}

// flush persists every pending heartbeat to Postgres in one tick. We
// do this in a single pass without batching across nodes because the
// store interface doesn't have a bulk-update method; the volume is
// bounded by the number of nodes which is small.
func (s *Subscriber) flush(ctx context.Context) {
	s.mu.Lock()
	pending := s.pending
	s.pending = make(map[string]*pendingHeartbeat)
	s.mu.Unlock()
	for nodeID, hb := range pending {
		if err := s.store.Nodes().UpdateUsage(ctx, nodeID, hb.usage); err != nil {
			s.logger.Debug("subscriber: flush usage", "node_id", nodeID, "err", err)
			continue
		}
		if err := s.store.Nodes().UpdateHeartbeat(ctx, nodeID, hb.timestamp); err != nil {
			s.logger.Debug("subscriber: flush heartbeat", "node_id", nodeID, "err", err)
		}
		if hb.version != "" {
			LogAgentVersionMismatch(s.logger, nodeID, hb.version)
		}
	}
}
