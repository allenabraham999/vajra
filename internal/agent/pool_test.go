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

// newFastPool wires a per-template PoolManager with tight tick intervals
// so tests run in well under a second. The returned hash is the seeded
// system template; cacheDir lets a test seed additional templates. cfg
// fields left zero fall back to PoolConfig defaults.
func newFastPool(t *testing.T, cfg PoolConfig) (pool *PoolManager, mgr *SandboxManager, vm *fakeVMM, sysHash, cacheDir string) {
	t.Helper()
	mgr, vm, cacheDir = newTestManager(t)
	sysHash = seedTemplate(t, cacheDir, []byte("system-rootfs"))
	cfg.SystemTemplate = sysHash
	cfg.Sandbox = SandboxConfig{VCPUs: 2, MemoryMB: 512}
	pool = NewPoolManager(cfg, mgr, nil)
	pool.replenishEvery = 20 * time.Millisecond
	pool.adjustEvery = 50 * time.Millisecond
	pool.rotateEvery = 50 * time.Millisecond
	pool.staleAfter = 100 * time.Millisecond
	pool.memReserveFrac = 0 // never block on memory in tests
	return pool, mgr, vm, sysHash, cacheDir
}

// templateStat returns the per-template stats row for hash, or ok=false.
func templateStat(p *PoolManager, hash string) (TemplatePoolStats, bool) {
	for _, ts := range p.Stats().Templates {
		if ts.TemplateHash == hash {
			return ts, true
		}
	}
	return TemplatePoolStats{}, false
}

func templateAvailable(p *PoolManager, hash string) int {
	ts, _ := templateStat(p, hash)
	return ts.Available
}

func templateTarget(p *PoolManager, hash string) int {
	ts, _ := templateStat(p, hash)
	return ts.TargetSize
}

func TestPoolWarmUp(t *testing.T) {
	pool, _, vm, sysHash, _ := newFastPool(t, PoolConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	waitFor(t, 2*time.Second, func() bool {
		return templateAvailable(pool, sysHash) >= 3
	})
	if got := vm.restores(); got < 3 {
		t.Fatalf("expected >=3 restore calls, got %d", got)
	}
}

func TestPoolAssign(t *testing.T) {
	pool, mgr, _, sysHash, _ := newFastPool(t, PoolConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	waitFor(t, 2*time.Second, func() bool { return templateAvailable(pool, sysHash) >= 3 })
	ps, err := pool.AssignFromPool(sysHash, "tmpl-sys")
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
	if stats := pool.Stats(); stats.TotalHits != 1 {
		t.Fatalf("expected TotalHits=1, got %d", stats.TotalHits)
	}
	// Pool should refill back to target.
	waitFor(t, 2*time.Second, func() bool { return templateAvailable(pool, sysHash) >= 3 })
}

func TestPoolMiss(t *testing.T) {
	pool, _, _, sysHash, _ := newFastPool(t, PoolConfig{})
	// Don't Start: the pool stays empty so AssignFromPool returns ErrPoolEmpty.
	if _, err := pool.AssignFromPool(sysHash, ""); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("expected ErrPoolEmpty, got %v", err)
	}
	if got := pool.Stats().TotalMisses; got != 1 {
		t.Fatalf("expected TotalMisses=1, got %d", got)
	}
}

func TestPoolReplenish(t *testing.T) {
	pool, _, _, sysHash, _ := newFastPool(t, PoolConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	waitFor(t, 2*time.Second, func() bool { return templateAvailable(pool, sysHash) >= 2 })
	first, err := pool.AssignFromPool(sysHash, "")
	if err != nil {
		t.Fatalf("assign1: %v", err)
	}
	second, err := pool.AssignFromPool(sysHash, "")
	if err != nil {
		t.Fatalf("assign2: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("two assigns returned the same pool member: %s", first.ID)
	}
	waitFor(t, 2*time.Second, func() bool { return templateAvailable(pool, sysHash) >= 2 })
}

// TestPerTemplatePoolWarming verifies that a miss on an unseen template
// stands up its own pool and that the warmer fills it independently of
// the system template's pool.
func TestPerTemplatePoolWarming(t *testing.T) {
	pool, _, _, sysHash, cacheDir := newFastPool(t, PoolConfig{})
	customHash := seedTemplate(t, cacheDir, []byte("custom-rootfs"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	// System template warms on its own.
	waitFor(t, 2*time.Second, func() bool { return templateAvailable(pool, sysHash) >= 1 })

	// The custom template has no pool until the first miss creates one.
	if _, ok := templateStat(pool, customHash); ok {
		t.Fatalf("custom template should have no pool before first request")
	}
	if _, err := pool.AssignFromPool(customHash, "tmpl-custom"); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("first custom assign should miss, got %v", err)
	}
	// The miss seeded a pool; the warmer now fills it.
	waitFor(t, 3*time.Second, func() bool { return templateAvailable(pool, customHash) >= 1 })

	// Both templates now have independent warm pools.
	if templateAvailable(pool, sysHash) < 1 {
		t.Fatalf("system pool drained while custom pool warmed")
	}
	ps, err := pool.AssignFromPool(customHash, "tmpl-custom")
	if err != nil {
		t.Fatalf("custom assign after warm should hit: %v", err)
	}
	if ps.TemplateHash != customHash {
		t.Fatalf("assigned member has wrong template: got %s want %s", ps.TemplateHash, customHash)
	}
}

// TestPoolAdaptiveSizingUpScales verifies that observed hit traffic walks
// a template's target up through the medium and high tiers.
func TestPoolAdaptiveSizingUpScales(t *testing.T) {
	pool, _, _, _, cacheDir := newFastPool(t, PoolConfig{HitThreshMed: 3, HitThreshHigh: 10})
	hash := seedTemplate(t, cacheDir, []byte("busy-template"))
	// Stand up the pool (non-system, so no SystemMinTarget floor).
	_, _ = pool.AssignFromPool(hash, "tmpl-busy")
	if got := templateTarget(pool, hash); got != DefaultPoolNewTemplateTarget {
		t.Fatalf("new pool target = %d, want %d", got, DefaultPoolNewTemplateTarget)
	}

	setHitWindow := func(n int) {
		pool.mu.Lock()
		tp := pool.pools[hash]
		now := time.Now()
		tp.hitWindow = tp.hitWindow[:0]
		for i := 0; i < n; i++ {
			tp.hitWindow = append(tp.hitWindow, now)
		}
		tp.lastHitAt = now
		pool.mu.Unlock()
	}

	setHitWindow(5) // medium tier
	pool.adjustAll()
	if got := templateTarget(pool, hash); got != poolTargetMedium {
		t.Fatalf("after 5 hits target = %d, want %d", got, poolTargetMedium)
	}

	setHitWindow(12) // high tier
	pool.adjustAll()
	if got := templateTarget(pool, hash); got != poolTargetHigh {
		t.Fatalf("after 12 hits target = %d, want %d", got, poolTargetHigh)
	}
}

// TestPoolAdaptiveSizingDrains verifies that an idle template pool's
// target collapses and the empty pool is garbage-collected.
func TestPoolAdaptiveSizingDrains(t *testing.T) {
	pool, _, _, _, cacheDir := newFastPool(t, PoolConfig{})
	hash := seedTemplate(t, cacheDir, []byte("idle-template"))
	_, _ = pool.AssignFromPool(hash, "tmpl-idle")

	// Idle past the half-drain mark: target collapses to 1.
	pool.mu.Lock()
	pool.pools[hash].lastHitAt = time.Now().Add(-40 * time.Minute)
	pool.mu.Unlock()
	pool.adjustAll()
	if got := templateTarget(pool, hash); got != 1 {
		t.Fatalf("after 40m idle target = %d, want 1", got)
	}

	// Idle past the full-drain mark: target hits 0 and the empty,
	// non-system pool is garbage-collected.
	pool.mu.Lock()
	pool.pools[hash].lastHitAt = time.Now().Add(-2 * time.Hour)
	pool.mu.Unlock()
	pool.adjustAll()
	if _, ok := templateStat(pool, hash); ok {
		t.Fatalf("idle drained pool should have been garbage-collected")
	}
}

// TestPoolGlobalCapRespected verifies that total warm VMs across all
// template pools never exceeds the per-node global cap, with the system
// template winning the contention.
func TestPoolGlobalCapRespected(t *testing.T) {
	const cap = 3
	pool, _, _, sysHash, cacheDir := newFastPool(t, PoolConfig{GlobalCap: cap})
	// Two extra templates competing for the same node budget.
	h2 := seedTemplate(t, cacheDir, []byte("competitor-a"))
	h3 := seedTemplate(t, cacheDir, []byte("competitor-b"))
	_, _ = pool.AssignFromPool(h2, "tmpl-a")
	_, _ = pool.AssignFromPool(h3, "tmpl-b")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	// The cap must hold at every observation.
	deadline := time.Now().Add(2 * time.Second)
	maxSeen := 0
	for time.Now().Before(deadline) {
		w := pool.Stats().TotalWarm
		if w > cap {
			t.Fatalf("total warm VMs %d exceeded global cap %d", w, cap)
		}
		if w > maxSeen {
			maxSeen = w
		}
		time.Sleep(10 * time.Millisecond)
	}
	if maxSeen != cap {
		t.Fatalf("pool never filled to the cap: maxSeen=%d cap=%d", maxSeen, cap)
	}
	// The system template, being highest priority, should own the budget.
	if got := templateAvailable(pool, sysHash); got == 0 {
		t.Fatalf("system template starved under cap pressure")
	}
}

// TestUbuntuNobleAlwaysMinThree is the regression guard for the system
// template: however idle it gets, its target never drains below 3 and
// its pool is never garbage-collected.
func TestUbuntuNobleAlwaysMinThree(t *testing.T) {
	pool, _, _, sysHash, _ := newFastPool(t, PoolConfig{})

	// Force maximum idleness — well past the full-drain threshold.
	pool.mu.Lock()
	pool.pools[sysHash].lastHitAt = time.Now().Add(-6 * time.Hour)
	pool.pools[sysHash].hitWindow = nil
	pool.mu.Unlock()
	pool.adjustAll()

	if got := templateTarget(pool, sysHash); got < 3 {
		t.Fatalf("system template target drained to %d; must stay >= 3", got)
	}
	if _, ok := templateStat(pool, sysHash); !ok {
		t.Fatalf("system template pool was garbage-collected; must persist")
	}
}

func TestConcurrentAssign(t *testing.T) {
	pool, _, _, sysHash, _ := newFastPool(t, PoolConfig{GlobalCap: 20})
	// Pin a wide system pool: disable the adaptive loop so the manual
	// target survives, then warm 12 members for the concurrent drain.
	pool.adjustEvery = time.Hour
	pool.mu.Lock()
	pool.pools[sysHash].target = 12
	pool.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	waitFor(t, 4*time.Second, func() bool { return templateAvailable(pool, sysHash) >= 10 })

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	seen := make(chan string, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ps, err := pool.AssignFromPool(sysHash, "")
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
	pool, _, vm, sysHash, _ := newFastPool(t, PoolConfig{})
	pool.staleAfter = 60 * time.Millisecond
	pool.rotateEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Shutdown()

	waitFor(t, 2*time.Second, func() bool { return templateAvailable(pool, sysHash) >= 2 })
	startRestores := vm.restores()
	startDestroys := vm.destroys()
	// Wait long enough for at least one full rotate sweep on the warmed members.
	time.Sleep(250 * time.Millisecond)
	if after := vm.destroys(); after <= startDestroys {
		t.Fatalf("expected stale rotate to destroy older members; destroys: before=%d after=%d",
			startDestroys, after)
	}
	if after := vm.restores(); after <= startRestores {
		t.Fatalf("expected replacement restores after stale rotate; restores: before=%d after=%d",
			startRestores, after)
	}
}

func TestStartupRace(t *testing.T) {
	pool, _, _, sysHash, _ := newFastPool(t, PoolConfig{})
	// Don't Start the pool yet: an Assign here must miss without blocking.
	_, err := pool.AssignFromPool(sysHash, "")
	if !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("expected ErrPoolEmpty on pre-start assign, got %v", err)
	}
}

func TestShutdownCleansUp(t *testing.T) {
	pool, _, vm, sysHash, _ := newFastPool(t, PoolConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	waitFor(t, 2*time.Second, func() bool { return templateAvailable(pool, sysHash) >= 3 })
	pool.Shutdown()
	if got := pool.Stats().TotalWarm; got != 0 {
		t.Fatalf("expected TotalWarm=0 after shutdown, got %d", got)
	}
	if vm.destroys() == 0 {
		t.Fatalf("expected shutdown to destroy warm members; destroyCalls=0")
	}
}

func TestCIDRecycling(t *testing.T) {
	pool, _, _, _, _ := newFastPool(t, PoolConfig{})
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
