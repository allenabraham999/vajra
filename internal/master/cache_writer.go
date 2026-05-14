// Package master — cache_writer.go: helpers that keep Redis aligned
// with Postgres on the write paths handlers care about. Every helper
// is best-effort — a cache failure NEVER blocks the underlying state
// change. Callers in this package use these so we have one place to
// audit cache invalidation behaviour.
package master

import (
	"context"
	"encoding/json"
	"time"

	"github.com/allenabraham999/vajra/internal/cache"
	"github.com/allenabraham999/vajra/internal/events"
	"github.com/allenabraham999/vajra/internal/models"
)

// writeSandboxStateCache persists the sandbox state in Redis with the
// canonical TTL. Best-effort; logs at debug on miss.
func (h *Handlers) writeSandboxStateCache(ctx context.Context, sandboxID string, state models.SandboxState) {
	c := h.getCache()
	if err := c.Set(ctx, cache.SandboxStateKey(sandboxID), string(state), cache.SandboxStateTTL); err != nil {
		h.log().Debug("cache: write sandbox state", "id", sandboxID, "err", err)
	}
}

// invalidateSandboxStateCache deletes the cached state — used after a
// sandbox is destroyed so Postgres is the only source of truth.
func (h *Handlers) invalidateSandboxStateCache(ctx context.Context, sandboxID string) {
	c := h.getCache()
	if err := c.Delete(ctx, cache.SandboxStateKey(sandboxID)); err != nil {
		h.log().Debug("cache: delete sandbox state", "id", sandboxID, "err", err)
	}
}

// writeNodeResourcesCache snapshots the node usage row into Redis under
// the documented JSON shape. Called from the heartbeat handler so the
// scheduler can read fresh capacity without a DB round-trip.
func (h *Handlers) writeNodeResourcesCache(ctx context.Context, n *models.Node) {
	if n == nil {
		return
	}
	payload := nodeResourcesPayload{
		TotalCPU:      n.Capacity.TotalCPU,
		UsedCPU:       n.UsedResources.UsedCPU,
		TotalMemoryMB: n.Capacity.TotalMemoryMB,
		UsedMemoryMB:  n.UsedResources.UsedMemoryMB,
		TotalDiskGB:   n.Capacity.TotalDiskGB,
		UsedDiskGB:    n.UsedResources.UsedDiskGB,
		LastHeartbeat: n.LastHeartbeat,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		h.log().Debug("cache: marshal node resources", "id", n.ID, "err", err)
		return
	}
	if err := h.getCache().Set(ctx, cache.NodeResourcesKey(n.ID), string(raw), cache.NodeResourcesTTL); err != nil {
		h.log().Debug("cache: write node resources", "id", n.ID, "err", err)
	}
}

// incrAccountSandboxCount is called on a successful sandbox create.
// Best-effort: a cache failure here means the next quota check will
// look slightly low, the create that follows will COUNT(*) again, and
// reality reasserts itself within AccountSandboxCountTTL.
func (h *Handlers) incrAccountSandboxCount(ctx context.Context, accountID string) {
	if _, err := h.getCache().Incr(ctx, cache.AccountSandboxCountKey(accountID)); err != nil {
		h.log().Debug("cache: incr sandbox count", "account_id", accountID, "err", err)
	}
}

// decrAccountSandboxCount is called on destroy. See incrAccountSandboxCount.
func (h *Handlers) decrAccountSandboxCount(ctx context.Context, accountID string) {
	if _, err := h.getCache().Decr(ctx, cache.AccountSandboxCountKey(accountID)); err != nil {
		h.log().Debug("cache: decr sandbox count", "account_id", accountID, "err", err)
	}
}

// publishStateChange fires a SubjectSandboxStateChanged event. Best-
// effort; the bus's NoopBus implementation makes this a no-op when
// NATS isn't configured.
//
// Also fans the change out to subscribed webhooks. The mapping from
// SandboxState → webhook event is best-effort: terminal states
// (RUNNING, STOPPED, DESTROYED, ARCHIVED, ERROR) emit a corresponding
// webhook; transient states (CREATING, STOPPING, etc.) do not.
func (h *Handlers) publishStateChange(ctx context.Context, sb *models.Sandbox, oldState, newState models.SandboxState) {
	if sb == nil {
		return
	}
	payload := events.SandboxStateChangedEvent{
		SandboxID: sb.ID,
		AccountID: sb.AccountID,
		OldState:  string(oldState),
		NewState:  string(newState),
		Timestamp: time.Now().UTC().Unix(),
	}
	raw, err := json.Marshal(payload)
	if err == nil {
		if err := h.getBus().Publish(ctx, events.SubjectSandboxStateChanged, raw); err != nil {
			h.log().Debug("bus: publish state change", "id", sb.ID, "err", err)
		}
	}
	if h.Webhooks != nil {
		if ev := sandboxStateToWebhookEvent(newState); ev != "" {
			h.Webhooks.Dispatch(ctx, sb.AccountID, ev, sb)
		}
	}
}

// sandboxStateToWebhookEvent maps a sandbox state into the webhook
// event name customers subscribe to. Returns "" for transitions that
// do not fire a webhook (intermediate / pausing states).
func sandboxStateToWebhookEvent(s models.SandboxState) string {
	switch s {
	case models.SandboxStateRunning:
		return string(models.WebhookEventSandboxRunning)
	case models.SandboxStateStopped:
		return string(models.WebhookEventSandboxStopped)
	case models.SandboxStateArchived:
		return string(models.WebhookEventSandboxArchived)
	case models.SandboxStateDestroyed:
		return string(models.WebhookEventSandboxDestroyed)
	case models.SandboxStateError:
		return string(models.WebhookEventSandboxError)
	}
	return ""
}

// publishSandboxCreated fires a SubjectSandboxCreated event after a
// sandbox reaches RUNNING and dispatches the sandbox.created webhook.
func (h *Handlers) publishSandboxCreated(ctx context.Context, sb *models.Sandbox) {
	if sb == nil || sb.NodeID == nil {
		return
	}
	payload := events.SandboxCreatedEvent{
		SandboxID:  sb.ID,
		AccountID:  sb.AccountID,
		NodeID:     *sb.NodeID,
		TemplateID: sb.TemplateID,
		Timestamp:  time.Now().UTC().Unix(),
	}
	raw, err := json.Marshal(payload)
	if err == nil {
		if err := h.getBus().Publish(ctx, events.SubjectSandboxCreated, raw); err != nil {
			h.log().Debug("bus: publish sandbox created", "id", sb.ID, "err", err)
		}
	}
	if h.Webhooks != nil {
		h.Webhooks.Dispatch(ctx, sb.AccountID, string(models.WebhookEventSandboxCreated), sb)
	}
}

// publishSandboxDestroyed fires a SubjectSandboxDestroyed event.
func (h *Handlers) publishSandboxDestroyed(ctx context.Context, sb *models.Sandbox) {
	if sb == nil {
		return
	}
	nodeID := ""
	if sb.NodeID != nil {
		nodeID = *sb.NodeID
	}
	payload := events.SandboxDestroyedEvent{
		SandboxID: sb.ID,
		AccountID: sb.AccountID,
		NodeID:    nodeID,
		Timestamp: time.Now().UTC().Unix(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if err := h.getBus().Publish(ctx, events.SubjectSandboxDestroyed, raw); err != nil {
		h.log().Debug("bus: publish sandbox destroyed", "id", sb.ID, "err", err)
	}
}
