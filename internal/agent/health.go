package agent

import (
	"context"
	"log/slog"
	"time"
)

// DefaultHealthInterval is how often HealthChecker probes each running
// sandbox via vsock.
const DefaultHealthInterval = 30 * time.Second

// MasterNotifier is the agent's outbound channel to vajra-master for
// asynchronous events. Implementations should be cheap to call from a
// background goroutine; failures must not block the health loop.
type MasterNotifier interface {
	NotifyUnhealthy(ctx context.Context, sandboxID string, reason string)
}

// HealthChecker periodically pings every running sandbox over vsock.
// Probe failures are logged but do NOT change sandbox state — the vsock
// guest agent can be unresponsive (or slow) while the VM itself is
// healthy, and the reconciler must not destroy working sandboxes on the
// strength of a missed ping.
type HealthChecker struct {
	sandboxes *SandboxManager
	notifier  MasterNotifier
	interval  time.Duration
	logger    *slog.Logger

	stop chan struct{}
	done chan struct{}
}

// NewHealthChecker constructs a checker. interval <= 0 falls back to
// DefaultHealthInterval. Pass nil for notifier to log-only mode.
func NewHealthChecker(
	sandboxes *SandboxManager,
	notifier MasterNotifier,
	interval time.Duration,
	logger *slog.Logger,
) *HealthChecker {
	if interval <= 0 {
		interval = DefaultHealthInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &HealthChecker{
		sandboxes: sandboxes,
		notifier:  notifier,
		interval:  interval,
		logger:    logger,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Start runs the probe loop until ctx is cancelled or Stop is called.
// Calling Start more than once panics.
func (h *HealthChecker) Start(ctx context.Context) {
	go h.run(ctx)
}

// Stop signals the loop to exit and waits for it.
func (h *HealthChecker) Stop() {
	select {
	case <-h.stop:
		return
	default:
		close(h.stop)
	}
	<-h.done
}

func (h *HealthChecker) run(ctx context.Context) {
	defer close(h.done)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		h.probeAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-h.stop:
			return
		case <-ticker.C:
		}
	}
}

func (h *HealthChecker) probeAll(ctx context.Context) {
	for _, sb := range h.sandboxes.List() {
		if sb.State != SandboxStateRunning {
			continue
		}
		id := sb.ID
		if err := h.sandboxes.HealthCheck(ctx, id); err != nil {
			h.logger.Warn("health probe failed, VM may still be running", "sandbox_id", id, "err", err)
			// Don't mark as unhealthy — just log. The vsock guest agent
			// can stall while the VM itself is fine; flipping state here
			// would cause the reconciler to destroy working sandboxes.
		}
	}
}
