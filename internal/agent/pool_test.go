package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// waitFor polls cond every 5ms until it returns true or the deadline
// elapses. Used in pool tests where the fill-up happens in a background
// goroutine.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

func TestPoolReplenishesToTarget(t *testing.T) {
	mgr, _, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	pool := NewPoolManager(3, hash, SandboxConfig{}, mgr, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()

	waitFor(t, 2*time.Second, func() bool {
		return pool.Stats().Available >= 3
	})
	if got := pool.Stats().Total; got < 3 {
		t.Fatalf("expected total >= 3, got %d", got)
	}
}

func TestPoolAssignThenRefills(t *testing.T) {
	mgr, _, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	pool := NewPoolManager(2, hash, SandboxConfig{}, mgr, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop()

	waitFor(t, 2*time.Second, func() bool { return pool.Stats().Available >= 2 })
	id, err := pool.AssignFromPool()
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := mgr.Get(id); err != nil {
		t.Fatalf("assigned id should exist in sandbox manager: %v", err)
	}
	stats := pool.Stats()
	if stats.InUse != 1 {
		t.Fatalf("expected InUse=1, got %d", stats.InUse)
	}
	// Pool should refill back to target.
	waitFor(t, 2*time.Second, func() bool { return pool.Stats().Available >= 2 })

	pool.Release(id)
	if got := pool.Stats().InUse; got != 0 {
		t.Fatalf("expected InUse=0 after release, got %d", got)
	}
}

func TestPoolReportsEmpty(t *testing.T) {
	mgr, _, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	pool := NewPoolManager(1, hash, SandboxConfig{}, mgr, nil)
	// Don't Start: pool stays empty so AssignFromPool returns ErrPoolEmpty.
	if _, err := pool.AssignFromPool(); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("expected ErrPoolEmpty, got %v", err)
	}
}
