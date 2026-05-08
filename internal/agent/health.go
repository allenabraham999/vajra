package agent

import (
	"context"
	"log/slog"
	"sync"
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

// HealthChecker periodically pings every running sandbox over vsock. A
// missed probe flips the sandbox to UNHEALTHY and fires NotifyUnhealthy
// once per state transition (not on every subsequent failed probe).
type HealthChecker struct {
	sandboxes *SandboxManager
	notifier  MasterNotifier
	interval  time.Duration
	logger    *slog.Logger

	mu           sync.Mutex
	lastNotified map[string]bool
	stop         chan struct{}
	done         chan struct{}
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
		sandboxes:    sandboxes,
		notifier:     notifier,
		interval:     interval,
		logger:       logger,
		lastNotified: map[string]bool{},
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
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
		err := h.sandboxes.HealthCheck(ctx, sb.ID)
		h.mu.Lock()
		wasNotified := h.lastNotified[sb.ID]
		h.mu.Unlock()
		if err != nil && !wasNotified {
			h.logger.Warn("sandbox unhealthy", "id", sb.ID, "err", err)
			if h.notifier != nil {
				h.notifier.NotifyUnhealthy(ctx, sb.ID, err.Error())
			}
			h.mu.Lock()
			h.lastNotified[sb.ID] = true
			h.mu.Unlock()
		} else if err == nil && wasNotified {
			h.logger.Info("sandbox recovered", "id", sb.ID)
			h.mu.Lock()
			delete(h.lastNotified, sb.ID)
			h.mu.Unlock()
		}
	}
}
