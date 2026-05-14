package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

type recordingNotifier struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingNotifier) NotifyUnhealthy(_ context.Context, id, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, id+":"+reason)
}

func (r *recordingNotifier) snap() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

// TestHealthCheckerDoesNotNotifyOnProbeFailure pins the contract that a
// failed vsock probe is only logged — the health loop never fires
// NotifyUnhealthy. The VM may still be running fine; signalling "unhealthy"
// here used to let the reconciler destroy working sandboxes.
func TestHealthCheckerDoesNotNotifyOnProbeFailure(t *testing.T) {
	mgr, _, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	if _, err := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// noopDialer always errors, so probes will fail.
	notifier := &recordingNotifier{}
	hc := NewHealthChecker(mgr, notifier, 10*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hc.Start(ctx)

	// Wait for several probe cycles to elapse.
	time.Sleep(60 * time.Millisecond)
	hc.Stop()

	if events := notifier.snap(); len(events) != 0 {
		t.Fatalf("expected no unhealthy notifications, got %d: %v", len(events), events)
	}
}
