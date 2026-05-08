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

func TestHealthCheckerNotifiesOnceWhenUnhealthy(t *testing.T) {
	mgr, _, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// noopDialer always errors, so probes will fail.
	notifier := &recordingNotifier{}
	hc := NewHealthChecker(mgr, notifier, 10*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hc.Start(ctx)

	// Wait for at least three probe cycles.
	time.Sleep(60 * time.Millisecond)
	hc.Stop()

	events := notifier.snap()
	if len(events) != 1 {
		t.Fatalf("expected exactly one unhealthy notification, got %d: %v", len(events), events)
	}
	if got, _ := mgr.Get(sb.ID); got.Healthy {
		t.Fatalf("expected sandbox marked unhealthy")
	}
}
