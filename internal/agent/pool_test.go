package agent

import (
	"context"
	"errors"
	"sync"
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

// newFastPool wires a PoolManager with tight tick intervals so tests run
// in under a second per case. Min/max bracket the dynamic target; the
// caller can adjust the manager fields after construction.
func newFastPool(t *testing.T, min, max int) (*PoolManager, *SandboxManager, *fakeVMM) {
	t.Helper()
	mgr, vm, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	pool := NewPoolManager(min, max, hash, SandboxConfig{VCPUs: 2, MemoryMB: 512}, mgr, nil)
	pool.replenishEvery = 20 * time.Millisecond
	pool.adjustEvery = 50 * time.Millisecond
	pool.rotateEvery = 50 * time.Millisecond
	pool.staleAfter = 100 * time.Millisecond
	pool.memReserveFrac = 0 // never block on memory in tests
	return pool, mgr, vm
}

func TestPoolWarmUp(t *testing.T) {
	pool, _, vm := newFastPool(t, 3, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	waitFor(t, 2*time.Second, func() bool {
		return pool.Stats().Available >= 3
	})
	if got := vm.restoreCalls; got < 3 {
		t.Fatalf("expected >=3 restore calls, got %d", got)
	}
}

func TestPoolAssign(t *testing.T) {
	pool, mgr, _ := newFastPool(t, 3, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	waitFor(t, 2*time.Second, func() bool { return pool.Stats().Available >= 3 })
	ps, err := pool.AssignFromPool()
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if ps.CID < DefaultPoolFirstCID {
		t.Fatalf("pool CID below reserved start: %d", ps.CID)
	}
	// Adopt the sandbox the way the server handler would.
	sb := pool.MakeSandbox(ps)
	mgr.AdoptSandbox(sb)
	got, err := mgr.Get(sb.ID)
	if err != nil || got.State != SandboxStateRunning {
		t.Fatalf("adopted sandbox should be RUNNING: %v %v", got, err)
	}
	if !got.FromPool {
		t.Fatalf("adopted sandbox should be flagged FromPool")
	}
	stats := pool.Stats()
	if stats.TotalHits != 1 {
		t.Fatalf("expected TotalHits=1, got %d", stats.TotalHits)
	}
	// Pool should refill back to target.
	waitFor(t, 2*time.Second, func() bool { return pool.Stats().Available >= 3 })
}

func TestPoolMiss(t *testing.T) {
	pool, _, _ := newFastPool(t, 1, 1)
	// Don't Start: pool stays empty so AssignFromPool returns ErrPoolEmpty.
	if _, err := pool.AssignFromPool(); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("expected ErrPoolEmpty, got %v", err)
	}
	if got := pool.Stats().TotalMisses; got != 1 {
		t.Fatalf("expected TotalMisses=1, got %d", got)
	}
}

func TestPoolReplenish(t *testing.T) {
	pool, _, _ := newFastPool(t, 2, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	waitFor(t, 2*time.Second, func() bool { return pool.Stats().Available >= 2 })
	// Drain.
	first, err := pool.AssignFromPool()
	if err != nil {
		t.Fatalf("assign1: %v", err)
	}
	second, err := pool.AssignFromPool()
	if err != nil {
		t.Fatalf("assign2: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("two assigns returned the same pool member: %s", first.ID)
	}
	waitFor(t, 2*time.Second, func() bool { return pool.Stats().Available >= 2 })
}

func TestPoolDynamicGrow(t *testing.T) {
	pool, _, _ := newFastPool(t, 1, 10)
	pool.adjustEvery = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	// Force misses by assigning when pool is empty.
	for i := 0; i < 5; i++ {
		_, _ = pool.AssignFromPool()
	}
	waitFor(t, 2*time.Second, func() bool { return pool.Stats().TargetSize > 1 })
}

func TestPoolDynamicShrink(t *testing.T) {
	pool, _, _ := newFastPool(t, 1, 10)
	// Set a higher initial target than min; with no traffic, adjust should pull it down.
	pool.mu.Lock()
	pool.targetSize = 5
	pool.mu.Unlock()
	pool.adjustEvery = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	waitFor(t, 3*time.Second, func() bool { return pool.Stats().TargetSize == pool.minSize })
}

func TestPoolMaxSizeCap(t *testing.T) {
	pool, _, _ := newFastPool(t, 5, 5)
	// Push for growth by injecting misses; max should still cap at 5.
	for i := 0; i < 20; i++ {
		_, _ = pool.AssignFromPool()
	}
	pool.adjustTargetSize()
	if got := pool.Stats().TargetSize; got > 5 {
		t.Fatalf("target exceeded max: got %d, max=5", got)
	}
}

func TestConcurrentAssign(t *testing.T) {
	pool, _, _ := newFastPool(t, 10, 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	waitFor(t, 3*time.Second, func() bool { return pool.Stats().Available >= 10 })

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	seen := make(chan string, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ps, err := pool.AssignFromPool()
			if err != nil {
				return
			}
			seen <- ps.ID
		}()
	}
	wg.Wait()
	close(seen)
	ids := map[string]bool{}
	for id := range seen {
		if ids[id] {
			t.Fatalf("two goroutines got the same pool member: %s", id)
		}
		ids[id] = true
	}
	if len(ids) == 0 {
		t.Fatalf("no successful assigns out of %d goroutines", N)
	}
}

func TestStaleRotation(t *testing.T) {
	pool, _, vm := newFastPool(t, 2, 5)
	pool.staleAfter = 60 * time.Millisecond
	pool.rotateEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	waitFor(t, 2*time.Second, func() bool { return pool.Stats().Available >= 2 })
	startRestores := vm.restoreCalls
	startDestroys := vm.destroyCalls
	// Wait long enough for at least one full rotate sweep on the warmed members.
	time.Sleep(250 * time.Millisecond)
	if vm.destroyCalls <= startDestroys {
		t.Fatalf("expected stale rotate to destroy older members; destroys: before=%d after=%d",
			startDestroys, vm.destroyCalls)
	}
	if vm.restoreCalls <= startRestores {
		t.Fatalf("expected replacement restores after stale rotate; restores: before=%d after=%d",
			startRestores, vm.restoreCalls)
	}
}

func TestStartupRace(t *testing.T) {
	pool, _, _ := newFastPool(t, 1, 5)
	// Don't Start the pool yet: an Assign here must miss without blocking.
	_, err := pool.AssignFromPool()
	if !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("expected ErrPoolEmpty on pre-start assign, got %v", err)
	}
}

func TestShutdownCleansUp(t *testing.T) {
	pool, _, vm := newFastPool(t, 3, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	waitFor(t, 2*time.Second, func() bool { return pool.Stats().Available >= 3 })
	pool.Shutdown()
	if pool.Stats().Available != 0 {
		t.Fatalf("expected Available=0 after shutdown, got %d", pool.Stats().Available)
	}
	if vm.destroyCalls == 0 {
		t.Fatalf("expected shutdown to destroy warm members; destroyCalls=0")
	}
}

func TestCIDRecycling(t *testing.T) {
	pool, _, _ := newFastPool(t, 0, 5)
	cid := pool.acquireCID()
	if cid < DefaultPoolFirstCID {
		t.Fatalf("CID below reserved range: %d", cid)
	}
	pool.recycleCID(cid)
	// Next acquire must reuse from the free list before bumping the counter.
	reused := pool.acquireCID()
	if reused != cid {
		t.Fatalf("recycled CID not reused: got %d want %d", reused, cid)
	}
}
