// Package agent — publisher.go: helper that pushes agent events onto
// the optional NATS bus. When NATS_URL is unset the bus is a NoopBus
// and Publish is a no-op — the agent's existing HTTP heartbeat path
// stays in charge of telling master about node state.
package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/allenabraham999/vajra/internal/events"
)

// Publisher wraps an events.EventBus with the helpers the agent
// actually needs (heartbeat, sandbox state, unhealthy). Callers don't
// touch the bus directly so we can swap implementations cleanly.
type Publisher struct {
	bus    events.EventBus
	nodeID string
	logger *slog.Logger
}

// NewPublisher returns a Publisher wired against bus. nodeID stamps
// every event so master can attribute them; logger is used only for
// debug output on serialisation failures.
func NewPublisher(bus events.EventBus, nodeID string, logger *slog.Logger) *Publisher {
	if bus == nil {
		bus = events.NewNoopBus()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Publisher{bus: bus, nodeID: nodeID, logger: logger}
}

// PublishHeartbeat publishes a SubjectNodeHeartbeat event. Called from
// the heartbeat loop once per HeartbeatInterval. Errors are logged but
// not fatal — the master subscriber will reconcile on the next tick.
func (p *Publisher) PublishHeartbeat(ctx context.Context, usage NodeUsageSnapshot, version string) {
	if p == nil {
		return
	}
	ev := events.NodeHeartbeatEvent{
		NodeID:       p.nodeID,
		UsedCPU:      usage.UsedCPU,
		UsedMemoryMB: usage.UsedMemoryMB,
		UsedDiskGB:   usage.UsedDiskGB,
		SandboxCount: usage.SandboxCount,
		Version:      version,
		Timestamp:    time.Now().UTC().Unix(),
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		p.logger.Debug("publisher: marshal heartbeat", "err", err)
		return
	}
	if err := p.bus.Publish(ctx, events.SubjectNodeHeartbeat, raw); err != nil {
		p.logger.Debug("publisher: heartbeat", "err", err)
	}
}

// PublishStateChanged publishes a sandbox state-change event.
func (p *Publisher) PublishStateChanged(ctx context.Context, sandboxID, accountID, oldState, newState string) {
	if p == nil {
		return
	}
	ev := events.SandboxStateChangedEvent{
		SandboxID: sandboxID,
		AccountID: accountID,
		OldState:  oldState,
		NewState:  newState,
		Timestamp: time.Now().UTC().Unix(),
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_ = p.bus.Publish(ctx, events.SubjectSandboxStateChanged, raw)
}

// PublishUnhealthy publishes a node-unhealthy event. The agent's
// MasterClient.NotifyUnhealthy continues to run alongside in HTTP mode
// so older masters keep working; this is purely additive.
func (p *Publisher) PublishUnhealthy(ctx context.Context, sandboxID, reason string) {
	if p == nil {
		return
	}
	ev := events.NodeUnhealthyEvent{
		NodeID:    p.nodeID,
		SandboxID: sandboxID,
		Error:     reason,
		Timestamp: time.Now().UTC().Unix(),
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_ = p.bus.Publish(ctx, events.SubjectNodeUnhealthy, raw)
}

// Close drains the underlying bus.
func (p *Publisher) Close() error {
	if p == nil || p.bus == nil {
		return nil
	}
	return p.bus.Close()
}

// NodeUsageSnapshot is a small bag the heartbeat loop fills and hands
// to PublishHeartbeat. Mirrors the existing computeUsage struct in
// cmd/vajra-agent/main.go without forcing that package to import
// events directly.
type NodeUsageSnapshot struct {
	UsedCPU      int
	UsedMemoryMB int
	UsedDiskGB   int
	SandboxCount int
}
