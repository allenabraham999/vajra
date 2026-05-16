package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Pool tunables. Min/max are upper/lower bounds on the dynamic target —
// the loop oscillates inside that band based on observed hit/miss rate.
const (
	DefaultPoolMinSize        = 5
	DefaultPoolMaxSize        = 15
	DefaultPoolFirstCID       = uint32(100)
	DefaultPoolReplenishEvery = 1 * time.Second
	DefaultPoolAdjustEvery    = 30 * time.Second
	DefaultPoolRotateEvery    = 60 * time.Second
	DefaultPoolStaleAfter     = 10 * time.Minute
	DefaultPoolWarmConcurrent = 3
	// DefaultPoolMemReserveFrac is the fraction of host memory that must
	// remain free for the pool to start another warm-up. Below this, the
	// replenish loop logs and waits.
	DefaultPoolMemReserveFrac = 0.20
)

// PoolStats is the JSON shape returned by GET /pool/stats.
type PoolStats struct {
	MinSize      int     `json:"min_size"`
	MaxSize      int     `json:"max_size"`
	TargetSize   int     `json:"target_size"`
	Available    int     `json:"available"`
	Warming      int     `json:"warming"`
	TotalHits    int64   `json:"total_hits"`
	TotalMisses  int64   `json:"total_misses"`
	TotalCreated int64   `json:"total_created"`
	HitRatePct   float64 `json:"hit_rate_pct"`
	Template     string  `json:"template"`
}

// PooledSandbox is a fully-restored, paused microVM the pool owns until
// AssignFromPool hands it to a caller. The Sandbox snapshot it carries is
// the seed for the AdoptSandbox call on assignment — paths inside it
// match the rewritten config.json that CH bound at restore time.
type PooledSandbox struct {
	ID              string
	Dir             string
	APISocket       string
	VsockSocketPath string
	CID             uint32
	RootfsPath      string
	TemplateHash    string
	Config          SandboxConfig
	CreatedAt       time.Time
}

// PoolManager keeps a small dynamic pool of pre-restored, paused
// sandboxes so create requests hit warm capacity instead of paying the
// full ~1.5 s overlay+restore overhead.
//
// Ownership: pool members are NOT in SandboxManager until AssignFromPool
// adopts them. That means the heartbeat usage accounting excludes them,
// while the host's vsock + memory footprint is real. The trade-off is
// intentional — under-reporting usage trades a small scheduling lie for
// a much faster create path; the alternative (registering pool members)
// would have them fail health checks because the paused guest can't
// answer the vsock probe.
type PoolManager struct {
	sandboxes *SandboxManager
	cache     *ImageCache
	vmm       VMM
	logger    *slog.Logger
	root      string

	minSize         int
	maxSize         int
	templateHash    string
	config          SandboxConfig
	replenishEvery  time.Duration
	adjustEvery     time.Duration
	rotateEvery     time.Duration
	staleAfter      time.Duration
	warmConcurrency int
	memReserveFrac  float64

	totalHits    atomic.Int64
	totalMisses  atomic.Int64
	totalCreated atomic.Int64

	cidCounter atomic.Uint32

	mu           sync.Mutex
	warm         []*PooledSandbox
	warming      int
	freeCIDs     []uint32
	targetSize   int
	recentHits   int
	recentMisses int
	windowStart  time.Time

	wg      sync.WaitGroup
	stop    chan struct{}
	stopped atomic.Bool
}

// ErrPoolEmpty is returned by AssignFromPool when no warm sandbox is ready.
var ErrPoolEmpty = errors.New("pool: no warm sandbox available")

// NewPoolManager builds a pool sized between minSize and maxSize. Pass
// minSize <= 0 for DefaultPoolMinSize; pass maxSize <= 0 for the larger of
// minSize and DefaultPoolMaxSize.
func NewPoolManager(
	minSize, maxSize int,
	templateHash string,
	cfg SandboxConfig,
	sandboxes *SandboxManager,
	logger *slog.Logger,
) *PoolManager {
	if minSize <= 0 {
		minSize = DefaultPoolMinSize
	}
	if maxSize <= 0 {
		maxSize = DefaultPoolMaxSize
	}
	if maxSize < minSize {
		maxSize = minSize
	}
	if logger == nil {
		logger = slog.Default()
	}
	p := &PoolManager{
		sandboxes:       sandboxes,
		cache:           sandboxes.Cache(),
		vmm:             sandboxes.VMM(),
		logger:          logger,
		root:            sandboxes.Root(),
		minSize:         minSize,
		maxSize:         maxSize,
		templateHash:    templateHash,
		config:          cfg,
		replenishEvery:  DefaultPoolReplenishEvery,
		adjustEvery:     DefaultPoolAdjustEvery,
		rotateEvery:     DefaultPoolRotateEvery,
		staleAfter:      DefaultPoolStaleAfter,
		warmConcurrency: DefaultPoolWarmConcurrent,
		memReserveFrac:  DefaultPoolMemReserveFrac,
		targetSize:      minSize,
		windowStart:     time.Now(),
		stop:            make(chan struct{}),
	}
	p.cidCounter.Store(DefaultPoolFirstCID)
	return p
}

// Start launches the background pool loops. Calling more than once
// returns immediately. The pool warms in the background — Start does not
// block on minSize warm members being ready, so the agent can serve cold
// creates while the pool catches up.
func (p *PoolManager) Start(ctx context.Context) {
	if p.stopped.Load() {
		return
	}
	p.wg.Add(1)
	go p.replenishLoop(ctx)
	p.wg.Add(1)
	go p.adjustLoop(ctx)
	p.wg.Add(1)
	go p.rotateLoop(ctx)
}

// Stop terminates the loops and destroys every warm member. Safe to call
// multiple times. Blocks until all warming goroutines have finished —
// pool members never outlive a clean Stop.
func (p *PoolManager) Stop() {
	if p.stopped.Swap(true) {
		return
	}
	close(p.stop)
	p.wg.Wait()
	p.destroyAll()
}

// Shutdown is a synonym for Stop; matches the spec terminology used in
// the agent's SIGTERM handler.
func (p *PoolManager) Shutdown() { p.Stop() }

// AssignFromPool hands a warm member to the caller. Returns ErrPoolEmpty
// if no warm sandbox is ready — the caller falls back to a cold create.
// The pool removes the member from its tracking under the lock, so two
// simultaneous Assign calls cannot return the same sandbox.
func (p *PoolManager) AssignFromPool() (*PooledSandbox, error) {
	p.mu.Lock()
	if len(p.warm) == 0 {
		p.recentMisses++
		p.mu.Unlock()
		p.totalMisses.Add(1)
		return nil, ErrPoolEmpty
	}
	// FIFO: hand out the oldest so warm dwell time stays bounded.
	ps := p.warm[0]
	p.warm = p.warm[1:]
	p.recentHits++
	p.mu.Unlock()
	p.totalHits.Add(1)
	return ps, nil
}

// MakeSandbox builds a SandboxManager-ready Sandbox value from a pooled
// member. The caller (the create handler) calls this after ResumeVM so
// the sandbox is RUNNING and Healthy before being adopted.
func (p *PoolManager) MakeSandbox(ps *PooledSandbox) *Sandbox {
	now := time.Now().UTC()
	return &Sandbox{
		ID:              ps.ID,
		State:           SandboxStateRunning,
		TemplateHash:    ps.TemplateHash,
		VsockCID:        ps.CID,
		APISocket:       ps.APISocket,
		VsockSocketPath: ps.VsockSocketPath,
		RootfsPath:      ps.RootfsPath,
		Config:          ps.Config,
		CreatedAt:       ps.CreatedAt,
		UpdatedAt:       now,
		Healthy:         true,
		LastHealthAt:    now,
		FromPool:        true,
	}
}

// Release is kept for API parity with the older pool; pool members
// are never returned to the warm list after a destroy, so this is just
// a CID-recycling hook.
func (p *PoolManager) Release(cid uint32) {
	if cid < DefaultPoolFirstCID {
		return
	}
	p.mu.Lock()
	p.freeCIDs = append(p.freeCIDs, cid)
	p.mu.Unlock()
}

// Stats returns a point-in-time snapshot.
func (p *PoolManager) Stats() PoolStats {
	p.mu.Lock()
	available := len(p.warm)
	warming := p.warming
	target := p.targetSize
	p.mu.Unlock()
	hits := p.totalHits.Load()
	misses := p.totalMisses.Load()
	rate := 0.0
	if total := hits + misses; total > 0 {
		rate = 100.0 * float64(hits) / float64(total)
	}
	return PoolStats{
		MinSize:      p.minSize,
		MaxSize:      p.maxSize,
		TargetSize:   target,
		Available:    available,
		Warming:      warming,
		TotalHits:    hits,
		TotalMisses:  misses,
		TotalCreated: p.totalCreated.Load(),
		HitRatePct:   rate,
		Template:     p.templateHash,
	}
}

func (p *PoolManager) replenishLoop(ctx context.Context) {
	defer p.wg.Done()
	// Kick once immediately so WarmUp is observable inside the first
	// second instead of waiting for the first tick.
	p.replenishOnce(ctx)
	ticker := time.NewTicker(p.replenishEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case <-ticker.C:
			p.replenishOnce(ctx)
			p.shrinkExcess(ctx)
		}
	}
}

func (p *PoolManager) adjustLoop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.adjustEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case <-ticker.C:
			p.adjustTargetSize()
		}
	}
}

func (p *PoolManager) rotateLoop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.rotateEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case <-ticker.C:
			p.rotateStale(ctx)
		}
	}
}

// replenishOnce launches warm-up goroutines until warm+warming reaches
// the dynamic target, capped at maxSize and DefaultPoolWarmConcurrent
// inflight at once. The actual restore happens in a goroutine; this
// function only schedules and returns quickly so the loop can react.
func (p *PoolManager) replenishOnce(ctx context.Context) {
	for {
		if p.shouldStop() {
			return
		}
		p.mu.Lock()
		warmCount := len(p.warm)
		warming := p.warming
		target := p.targetSize
		if target > p.maxSize {
			target = p.maxSize
		}
		needed := target - warmCount - warming
		if needed <= 0 || warmCount+warming >= p.maxSize || warming >= p.warmConcurrency {
			p.mu.Unlock()
			return
		}
		if !p.hostHasMemoryHeadroomLocked() {
			p.mu.Unlock()
			p.logger.Warn("pool: skipping warm-up; host low on memory",
				"warm", warmCount, "warming", warming, "target", target)
			return
		}
		p.warming++
		p.mu.Unlock()
		p.logger.Info("pool: replenishing",
			"warm", warmCount, "warming", warming+1, "target", target)
		p.wg.Add(1)
		go p.warmOne(ctx)
	}
}

// warmOne runs a single pre-warm in the background. On success the
// member is appended to warm; on failure the warming counter still
// decrements so the loop will retry.
func (p *PoolManager) warmOne(ctx context.Context) {
	defer p.wg.Done()
	t0 := time.Now()
	ps, err := p.buildPooled(ctx)
	p.mu.Lock()
	p.warming--
	if err != nil {
		p.mu.Unlock()
		p.logger.Warn("pool: warm-up failed", "err", err)
		return
	}
	p.warm = append(p.warm, ps)
	p.mu.Unlock()
	p.totalCreated.Add(1)
	p.logger.Info("pool: pre-warmed sandbox",
		"id", ps.ID, "elapsed_ms", time.Since(t0).Milliseconds())
}

// buildPooled materialises a single pool member end-to-end:
//   qcow2 overlay → hardlink snapshot → rewrite config → CH restore (paused).
// The new VM is fully restored but stays paused; AssignFromPool's caller
// resumes it. On any failure the partial sandbox directory is removed
// and the CID is returned to the free list.
func (p *PoolManager) buildPooled(ctx context.Context) (*PooledSandbox, error) {
	if !p.cache.HasTemplate(p.templateHash) {
		return nil, fmt.Errorf("template %s not in cache", p.templateHash)
	}
	layout := p.cache.Layout(p.templateHash)
	if err := p.cache.EnsureRootfsBacking(p.templateHash); err != nil {
		return nil, fmt.Errorf("ensure rootfs backing: %w", err)
	}

	cid := p.acquireCID()
	id := "pool-" + newSandboxID()
	dir := filepath.Join(p.root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.recycleCID(cid)
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	overlay := filepath.Join(dir, "rootfs.qcow2")
	if err := createRootfsOverlay(layout.RootfsBackingPath, overlay, p.logger); err != nil {
		_ = os.RemoveAll(dir)
		p.recycleCID(cid)
		return nil, fmt.Errorf("create rootfs overlay: %w", err)
	}
	snapshotDir, vsockSocketPath, err := prepareSandboxSnapshot(layout, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		p.recycleCID(cid)
		return nil, fmt.Errorf("prepare snapshot: %w", err)
	}
	socketPath, err := p.vmm.RestoreVMPaused(ctx, id, snapshotDir)
	if err != nil {
		_ = os.RemoveAll(dir)
		p.recycleCID(cid)
		return nil, fmt.Errorf("restore paused: %w", err)
	}
	p.cache.Touch(p.templateHash)
	return &PooledSandbox{
		ID:              id,
		Dir:             dir,
		APISocket:       socketPath,
		VsockSocketPath: vsockSocketPath,
		CID:             cid,
		RootfsPath:      overlay,
		TemplateHash:    p.templateHash,
		Config:          p.config,
		CreatedAt:       time.Now().UTC(),
	}, nil
}

// shrinkExcess drops warm members past the current target. Triggered on
// every replenish tick so a freshly-lowered target reflects on disk
// without waiting for the rotate loop.
func (p *PoolManager) shrinkExcess(ctx context.Context) {
	p.mu.Lock()
	excess := len(p.warm) - p.targetSize
	if excess <= 0 {
		p.mu.Unlock()
		return
	}
	victims := make([]*PooledSandbox, 0, excess)
	for i := 0; i < excess; i++ {
		// FIFO: oldest first so the survivors are the freshest.
		victims = append(victims, p.warm[i])
	}
	p.warm = p.warm[excess:]
	p.mu.Unlock()
	for _, v := range victims {
		p.destroyMember(ctx, v, "shrink")
	}
}

// adjustTargetSize moves targetSize between min and max based on the
// last 30s of pool traffic. Misses pull the target up, idle silence
// pulls it down. Hits with no misses hold the target where it is.
func (p *PoolManager) adjustTargetSize() {
	p.mu.Lock()
	hits := p.recentHits
	misses := p.recentMisses
	prev := p.targetSize
	switch {
	case misses > 0:
		p.targetSize += misses + 2
	case hits == 0:
		p.targetSize--
	}
	if p.targetSize < p.minSize {
		p.targetSize = p.minSize
	}
	if p.targetSize > p.maxSize {
		p.targetSize = p.maxSize
	}
	p.recentHits = 0
	p.recentMisses = 0
	p.windowStart = time.Now()
	target := p.targetSize
	warm := len(p.warm)
	p.mu.Unlock()
	if target != prev {
		p.logger.Info("pool: adjust",
			"target", target, "warm", warm, "hits", hits, "misses", misses,
		)
	} else {
		p.logger.Debug("pool: adjust (no change)",
			"target", target, "warm", warm, "hits", hits, "misses", misses,
		)
	}
}

// rotateStale destroys warm members older than staleAfter so the pool
// doesn't accumulate dirty kernel state. Replacements come on the next
// replenish tick.
func (p *PoolManager) rotateStale(ctx context.Context) {
	now := time.Now()
	p.mu.Lock()
	kept := p.warm[:0]
	stale := make([]*PooledSandbox, 0)
	for _, ps := range p.warm {
		if now.Sub(ps.CreatedAt) > p.staleAfter {
			stale = append(stale, ps)
			continue
		}
		kept = append(kept, ps)
	}
	p.warm = kept
	p.mu.Unlock()
	for _, ps := range stale {
		p.destroyMember(ctx, ps, "stale")
	}
}

// destroyMember tears down a warm member: stop CH cleanly, remove the
// sandbox dir, return the CID to the free list. Best-effort: errors are
// logged, never raised — the loops keep moving.
func (p *PoolManager) destroyMember(ctx context.Context, ps *PooledSandbox, reason string) {
	destroyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.vmm.DestroyVM(destroyCtx, ps.APISocket); err != nil {
		p.logger.Warn("pool: destroy failed", "id", ps.ID, "reason", reason, "err", err)
	}
	if err := os.RemoveAll(ps.Dir); err != nil {
		p.logger.Warn("pool: remove dir failed", "id", ps.ID, "err", err)
	}
	p.recycleCID(ps.CID)
	p.logger.Info("pool: destroyed", "id", ps.ID, "reason", reason)
}

func (p *PoolManager) destroyAll() {
	p.mu.Lock()
	victims := p.warm
	p.warm = nil
	p.mu.Unlock()
	ctx := context.Background()
	for _, ps := range victims {
		p.destroyMember(ctx, ps, "shutdown")
	}
}

func (p *PoolManager) acquireCID() uint32 {
	p.mu.Lock()
	if n := len(p.freeCIDs); n > 0 {
		cid := p.freeCIDs[n-1]
		p.freeCIDs = p.freeCIDs[:n-1]
		p.mu.Unlock()
		return cid
	}
	p.mu.Unlock()
	for {
		v := p.cidCounter.Add(1) - 1
		if v < DefaultPoolFirstCID {
			// Counter wrapped or someone reset it; clamp upward.
			p.cidCounter.Store(DefaultPoolFirstCID + 1)
			return DefaultPoolFirstCID
		}
		return v
	}
}

func (p *PoolManager) recycleCID(cid uint32) {
	if cid < DefaultPoolFirstCID {
		return
	}
	p.mu.Lock()
	p.freeCIDs = append(p.freeCIDs, cid)
	p.mu.Unlock()
}

func (p *PoolManager) shouldStop() bool {
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

// hostHasMemoryHeadroomLocked checks /proc/meminfo for free memory >= the
// configured reserve fraction of total. Caller must hold p.mu. On
// non-Linux hosts (the dev macOS) the check is skipped: tests and dev
// runs always have headroom for our purposes.
func (p *PoolManager) hostHasMemoryHeadroomLocked() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	total, free, ok := readMemInfo()
	if !ok {
		return true
	}
	if total <= 0 {
		return true
	}
	frac := float64(free) / float64(total)
	return frac >= p.memReserveFrac
}

// readMemInfo parses MemTotal and MemAvailable from /proc/meminfo in KB.
// MemAvailable (not MemFree) is used because Linux page cache makes
// MemFree pessimistic — MemAvailable is the kernel's own estimate of
// what's actually claimable without swap.
func readMemInfo() (totalKB, availKB int64, ok bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB, _ = strconv.ParseInt(fields[1], 10, 64)
		case "MemAvailable:":
			availKB, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	}
	if totalKB == 0 {
		return 0, 0, false
	}
	return totalKB, availKB, true
}
