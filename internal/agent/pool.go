package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Pool tunables. The pre-warm pool is per-template: each template hash
// gets its own warm set, sized adaptively from observed hit traffic and
// bounded by a per-node global cap so a busy node can't overcommit.
const (
	DefaultPoolFirstCID       = uint32(100)
	DefaultPoolReplenishEvery = 1 * time.Second
	DefaultPoolAdjustEvery    = 60 * time.Second
	DefaultPoolRotateEvery    = 60 * time.Second
	DefaultPoolStaleAfter     = 10 * time.Minute
	DefaultPoolWarmConcurrent = 3
	// DefaultPoolMemReserveFrac is the fraction of host memory that must
	// remain free for the pool to start another warm-up. Below this, the
	// replenish loop logs and waits.
	DefaultPoolMemReserveFrac = 0.20

	// Adaptive-sizing defaults — all overridable via env (see PoolConfig).
	DefaultPoolNewTemplateTarget = 2          // brand-new template pool
	DefaultPoolTemplateMax       = 5          // ceiling on one pool's target
	DefaultPoolSystemMinTarget   = 3          // system template never drains below this
	DefaultPoolHitThresholdMed   = 3          // hits/window → target 3
	DefaultPoolHitThresholdHigh  = 10         // hits/window → target 5
	DefaultPoolAdaptiveWindow    = time.Hour  // rolling hit/miss window
	DefaultPoolDrainAfterIdle    = time.Hour  // no hits this long → target 0
	poolDrainHalfIdle            = 30 * time.Minute // no hits this long → target 1
	poolTargetMedium             = 3
	poolTargetHigh               = 5
)

// ErrPoolEmpty is returned by AssignFromPool when no warm sandbox is ready
// for the requested template — the caller falls back to a cold create.
var ErrPoolEmpty = errors.New("pool: no warm sandbox available")

// PoolConfig carries the per-node pre-warm-pool tunables. Values at or
// below zero fall back to the documented defaults via withDefaults.
type PoolConfig struct {
	// SystemTemplate is the default template (ubuntu-noble) kept warm at
	// all times — its pool is seeded on startup and never drains below
	// SystemMinTarget or gets garbage-collected.
	SystemTemplate string
	// GlobalCap bounds total warm VMs across every template pool on this
	// node. Derived from host CPU (total_cpu * 1.5) by the agent.
	GlobalCap int
	// NewTemplate is the target a freshly-seen template pool starts at.
	NewTemplate int
	// PerTemplateMax caps any single pool's adaptive target.
	PerTemplateMax int
	// SystemMinTarget is the floor on the system template's target.
	SystemMinTarget int
	// HitThreshMed / HitThreshHigh are the hits-in-window cutoffs that
	// step a pool's target up to 3 and 5 respectively.
	HitThreshMed  int
	HitThreshHigh int
	// AdaptiveWindow is the rolling window over which hits/misses count.
	AdaptiveWindow time.Duration
	// DrainAfterIdle is how long a pool may go without a hit before its
	// target collapses to 0 and the pool is garbage-collected.
	DrainAfterIdle time.Duration
	// Sandbox is the resource shape every warmed VM is built with.
	Sandbox SandboxConfig
}

func (c PoolConfig) withDefaults() PoolConfig {
	if c.GlobalCap <= 0 {
		c.GlobalCap = 6
	}
	if c.NewTemplate <= 0 {
		c.NewTemplate = DefaultPoolNewTemplateTarget
	}
	if c.PerTemplateMax <= 0 {
		c.PerTemplateMax = DefaultPoolTemplateMax
	}
	if c.SystemMinTarget <= 0 {
		c.SystemMinTarget = DefaultPoolSystemMinTarget
	}
	if c.HitThreshMed <= 0 {
		c.HitThreshMed = DefaultPoolHitThresholdMed
	}
	if c.HitThreshHigh <= 0 {
		c.HitThreshHigh = DefaultPoolHitThresholdHigh
	}
	if c.AdaptiveWindow <= 0 {
		c.AdaptiveWindow = DefaultPoolAdaptiveWindow
	}
	if c.DrainAfterIdle <= 0 {
		c.DrainAfterIdle = DefaultPoolDrainAfterIdle
	}
	if c.Sandbox.VCPUs == 0 {
		c.Sandbox.VCPUs = 2
	}
	if c.Sandbox.MemoryMB == 0 {
		c.Sandbox.MemoryMB = 512
	}
	if c.Sandbox.DiskGB == 0 {
		c.Sandbox.DiskGB = 4
	}
	return c
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

// templatePool is the warm pool for a single template hash plus the
// bookkeeping the adaptive sizer reads. Every field is guarded by
// PoolManager.mu — templatePool carries no lock of its own.
type templatePool struct {
	hash         string
	id           string // registry template ID, for stats display
	warm         []*PooledSandbox
	warming      int
	target       int
	hitWindow    []time.Time // hit timestamps, pruned to AdaptiveWindow
	missWindow   []time.Time
	lastHitAt    time.Time
	createdAt    time.Time
	lifetimeHits int64
}

// TemplatePoolStats is one template's pool snapshot inside NodePoolStats.
type TemplatePoolStats struct {
	TemplateHash string     `json:"template_hash"`
	TemplateID   string     `json:"template_id,omitempty"`
	Available    int        `json:"available"`
	Warming      int        `json:"warming"`
	TargetSize   int        `json:"target_size"`
	InUse        int        `json:"in_use"`
	HitsLastHour int        `json:"hits_last_hour"`
	TotalHits    int64      `json:"total_hits"`
	LastHitAt    *time.Time `json:"last_hit_at,omitempty"`
}

// NodePoolStats is the GET /pool/stats body: this node's per-template
// warm pools plus the node-wide warm-VM budget.
type NodePoolStats struct {
	Capacity    int                 `json:"capacity"`
	TotalWarm   int                 `json:"total_warm"`
	TotalHits   int64               `json:"total_hits"`
	TotalMisses int64               `json:"total_misses"`
	Templates   []TemplatePoolStats `json:"templates"`
}

// PoolManager keeps a per-template set of pre-restored, paused sandboxes
// so create requests hit warm capacity instead of paying the full
// overlay+restore overhead. Each template hash has its own templatePool;
// the manager warms, drains, rotates and adaptively resizes them under a
// single mutex.
//
// Ownership: pool members are NOT in SandboxManager until AssignFromPool
// adopts them. Heartbeat usage accounting excludes them, while the host's
// vsock + memory footprint is real — an intentional trade of a small
// scheduling lie for a much faster create path.
type PoolManager struct {
	sandboxes *SandboxManager
	cache     *ImageCache
	vmm       VMM
	logger    *slog.Logger
	root      string

	cfg PoolConfig

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

	mu       sync.Mutex
	pools    map[string]*templatePool
	freeCIDs []uint32

	wg      sync.WaitGroup
	stop    chan struct{}
	stopped atomic.Bool
}

// NewPoolManager builds a per-template pool manager. The system template,
// when set, is seeded immediately so it warms from process start.
func NewPoolManager(cfg PoolConfig, sandboxes *SandboxManager, logger *slog.Logger) *PoolManager {
	if logger == nil {
		logger = slog.Default()
	}
	cfg = cfg.withDefaults()
	p := &PoolManager{
		sandboxes:       sandboxes,
		cache:           sandboxes.Cache(),
		vmm:             sandboxes.VMM(),
		logger:          logger,
		root:            sandboxes.Root(),
		cfg:             cfg,
		replenishEvery:  DefaultPoolReplenishEvery,
		adjustEvery:     DefaultPoolAdjustEvery,
		rotateEvery:     DefaultPoolRotateEvery,
		staleAfter:      DefaultPoolStaleAfter,
		warmConcurrency: DefaultPoolWarmConcurrent,
		memReserveFrac:  DefaultPoolMemReserveFrac,
		pools:           make(map[string]*templatePool),
		stop:            make(chan struct{}),
	}
	p.cidCounter.Store(DefaultPoolFirstCID)
	if cfg.SystemTemplate != "" {
		p.pools[cfg.SystemTemplate] = newTemplatePool(cfg.SystemTemplate, "", cfg.SystemMinTarget)
	}
	return p
}

// newTemplatePool returns a pool seeded with target and clocks set to now
// so a brand-new pool isn't immediately treated as idle.
func newTemplatePool(hash, id string, target int) *templatePool {
	now := time.Now()
	return &templatePool{
		hash:      hash,
		id:        id,
		target:    target,
		lastHitAt: now,
		createdAt: now,
	}
}

// Start launches the background pool loops. Calling more than once
// returns immediately. The pool warms in the background — Start does not
// block on warm members being ready, so the agent can serve cold creates
// while the pools fill.
func (p *PoolManager) Start(ctx context.Context) {
	if p.stopped.Load() {
		return
	}
	p.mu.Lock()
	seeded := len(p.pools)
	p.mu.Unlock()
	p.logger.Info("pool: warming",
		"system_template", p.cfg.SystemTemplate,
		"seeded_pools", seeded,
		"global_cap", p.cfg.GlobalCap,
		"per_template_max", p.cfg.PerTemplateMax)
	p.wg.Add(1)
	go p.replenishLoop(ctx)
	p.wg.Add(1)
	go p.adjustLoop(ctx)
	p.wg.Add(1)
	go p.rotateLoop(ctx)
}

// Stop terminates the loops and destroys every warm member across all
// template pools. Safe to call multiple times. Blocks until all warming
// goroutines have finished.
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

// AssignFromPool hands a warm member for templateHash to the caller, or
// ErrPoolEmpty when none is ready. A miss on a template with no pool
// stands one up (at the new-template target) so the warmer starts
// filling it; templateID is recorded for stats display when known.
func (p *PoolManager) AssignFromPool(templateHash, templateID string) (*PooledSandbox, error) {
	if templateHash == "" {
		return nil, ErrPoolEmpty
	}
	now := time.Now()
	p.mu.Lock()
	tp := p.pools[templateHash]
	if tp == nil {
		tp = newTemplatePool(templateHash, templateID, p.cfg.NewTemplate)
		p.pools[templateHash] = tp
		tp.missWindow = append(tp.missWindow, now)
		p.mu.Unlock()
		p.totalMisses.Add(1)
		p.logger.Info("pool: new template pool",
			"template", templateHash, "target", tp.target)
		return nil, ErrPoolEmpty
	}
	if tp.id == "" && templateID != "" {
		tp.id = templateID
	}
	if len(tp.warm) == 0 {
		tp.missWindow = append(tp.missWindow, now)
		p.mu.Unlock()
		p.totalMisses.Add(1)
		return nil, ErrPoolEmpty
	}
	// FIFO: hand out the oldest so warm dwell time stays bounded.
	ps := tp.warm[0]
	tp.warm = tp.warm[1:]
	tp.hitWindow = append(tp.hitWindow, now)
	tp.lastHitAt = now
	tp.lifetimeHits++
	p.mu.Unlock()
	p.totalHits.Add(1)
	return ps, nil
}

// MakeSandbox builds a SandboxManager-ready Sandbox from a pooled member.
// The caller calls this after ResumeVM so the sandbox is RUNNING and
// Healthy before being adopted.
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

// Release recycles a pooled member's CID after the caller destroys it.
// Pool members are never returned to the warm list once handed out.
func (p *PoolManager) Release(cid uint32) {
	p.recycleCID(cid)
}

// Stats returns a point-in-time snapshot of every template pool plus the
// node-wide warm-VM budget.
func (p *PoolManager) Stats() NodePoolStats {
	// in_use: pool-sourced sandboxes still running, bucketed by template.
	inUse := map[string]int{}
	for _, sb := range p.sandboxes.List() {
		if sb.FromPool && sb.State == SandboxStateRunning {
			inUse[sb.TemplateHash]++
		}
	}
	now := time.Now()
	p.mu.Lock()
	out := NodePoolStats{
		Capacity:  p.cfg.GlobalCap,
		Templates: make([]TemplatePoolStats, 0, len(p.pools)),
	}
	for hash, tp := range p.pools {
		p.pruneWindowsLocked(tp, now)
		ts := TemplatePoolStats{
			TemplateHash: hash,
			TemplateID:   tp.id,
			Available:    len(tp.warm),
			Warming:      tp.warming,
			TargetSize:   tp.target,
			InUse:        inUse[hash],
			HitsLastHour: len(tp.hitWindow),
			TotalHits:    tp.lifetimeHits,
		}
		if tp.lifetimeHits > 0 {
			t := tp.lastHitAt
			ts.LastHitAt = &t
		}
		out.TotalWarm += len(tp.warm)
		out.Templates = append(out.Templates, ts)
	}
	p.mu.Unlock()
	sort.Slice(out.Templates, func(i, j int) bool {
		return out.Templates[i].TemplateHash < out.Templates[j].TemplateHash
	})
	out.TotalHits = p.totalHits.Load()
	out.TotalMisses = p.totalMisses.Load()
	return out
}

func (p *PoolManager) replenishLoop(ctx context.Context) {
	defer p.wg.Done()
	// Kick once immediately so warm-up is observable inside the first
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
			p.adjustAll()
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

// replenishOnce drives every template pool toward its target, highest
// priority first, never exceeding warmConcurrency inflight restores or
// the node-wide GlobalCap on warm VMs. When the cap blocks a needed
// warm-up it drains one member from a strictly-lower-priority pool to
// free a slot. The actual restore runs in a goroutine.
func (p *PoolManager) replenishOnce(ctx context.Context) {
	for iter := 0; iter < 64; iter++ {
		if p.shouldStop() {
			return
		}
		p.mu.Lock()
		totalWarm, totalWarming := 0, 0
		for _, tp := range p.pools {
			totalWarm += len(tp.warm)
			totalWarming += tp.warming
		}
		if totalWarming >= p.warmConcurrency {
			p.mu.Unlock()
			return
		}
		needy := p.neediestPoolLocked()
		if needy == nil {
			p.mu.Unlock()
			return
		}
		if totalWarm+totalWarming >= p.cfg.GlobalCap {
			// At the node cap. Free a slot by draining the oldest member
			// of a strictly-lower-priority pool; if there is none we
			// simply can't grow this tick.
			victim := p.evictionVictimLocked(needy)
			if victim == nil {
				p.mu.Unlock()
				return
			}
			v := victim.warm[0]
			victim.warm = victim.warm[1:]
			victimHash := victim.hash
			p.mu.Unlock()
			p.logger.Info("pool: evict for higher-priority template",
				"victim_template", victimHash, "needed_for", needy.hash)
			p.destroyMember(ctx, v, "evict")
			continue
		}
		if !p.hostHasMemoryHeadroomLocked() {
			p.mu.Unlock()
			p.logger.Warn("pool: skipping warm-up; host low on memory",
				"warm", totalWarm, "warming", totalWarming)
			return
		}
		needy.warming++
		hash := needy.hash
		p.mu.Unlock()
		p.logger.Info("pool: replenishing", "template", hash)
		p.wg.Add(1)
		go p.warmOne(ctx, hash)
	}
}

// neediestPoolLocked returns the highest-priority pool that is below its
// target (warm+warming < target), or nil when every pool is satisfied.
// Caller holds p.mu.
func (p *PoolManager) neediestPoolLocked() *templatePool {
	var best *templatePool
	bestRank := -1
	for _, tp := range p.pools {
		if len(tp.warm)+tp.warming >= tp.target {
			continue
		}
		if r := p.poolRankLocked(tp); r > bestRank {
			best, bestRank = tp, r
		}
	}
	return best
}

// evictionVictimLocked returns the lowest-priority pool that has a warm
// member to spare and ranks strictly below needy, or nil. Caller holds
// p.mu.
func (p *PoolManager) evictionVictimLocked(needy *templatePool) *templatePool {
	needyRank := p.poolRankLocked(needy)
	var victim *templatePool
	victimRank := -1
	for _, tp := range p.pools {
		if tp == needy || len(tp.warm) == 0 {
			continue
		}
		r := p.poolRankLocked(tp)
		if r >= needyRank {
			continue
		}
		if victim == nil || r < victimRank {
			victim, victimRank = tp, r
		}
	}
	return victim
}

// poolRankLocked scores a pool's scheduling priority: the system template
// always outranks the rest, and within each tier more recent hits rank
// higher. Caller holds p.mu.
func (p *PoolManager) poolRankLocked(tp *templatePool) int {
	rank := len(tp.hitWindow)
	if tp.hash == p.cfg.SystemTemplate {
		rank += 1_000_000
	}
	return rank
}

// warmOne runs a single pre-warm for templateHash in the background. On
// success the member is appended to that pool; if the pool was
// garbage-collected mid-build the orphan VM is destroyed.
func (p *PoolManager) warmOne(ctx context.Context, templateHash string) {
	defer p.wg.Done()
	t0 := time.Now()
	ps, err := p.buildPooled(ctx, templateHash)
	p.mu.Lock()
	tp := p.pools[templateHash]
	if tp != nil {
		tp.warming--
	}
	if err != nil {
		p.mu.Unlock()
		p.logger.Warn("pool: warm-up failed", "template", templateHash, "err", err)
		return
	}
	if tp == nil {
		p.mu.Unlock()
		p.destroyMember(ctx, ps, "orphan")
		return
	}
	tp.warm = append(tp.warm, ps)
	p.mu.Unlock()
	p.totalCreated.Add(1)
	p.logger.Info("pool: pre-warmed sandbox",
		"id", ps.ID, "template", templateHash,
		"elapsed_ms", time.Since(t0).Milliseconds())
}

// buildPooled materialises a single pool member for templateHash:
//
//	qcow2 overlay → hardlink snapshot → rewrite config → CH restore (paused).
//
// The new VM is fully restored but stays paused. On any failure the
// partial sandbox directory is removed and the CID returned to the free
// list.
func (p *PoolManager) buildPooled(ctx context.Context, templateHash string) (*PooledSandbox, error) {
	if !p.cache.HasTemplate(templateHash) {
		return nil, fmt.Errorf("template %s not in cache", templateHash)
	}
	layout := p.cache.Layout(templateHash)
	if err := p.cache.EnsureRootfsBacking(templateHash); err != nil {
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
	p.cache.Touch(templateHash)
	return &PooledSandbox{
		ID:              id,
		Dir:             dir,
		APISocket:       socketPath,
		VsockSocketPath: vsockSocketPath,
		CID:             cid,
		RootfsPath:      overlay,
		TemplateHash:    templateHash,
		Config:          p.cfg.Sandbox,
		CreatedAt:       time.Now().UTC(),
	}, nil
}

// shrinkExcess drops warm members past each pool's current target.
// Triggered on every replenish tick so a freshly-lowered target reflects
// on disk without waiting for the rotate loop.
func (p *PoolManager) shrinkExcess(ctx context.Context) {
	p.mu.Lock()
	var victims []*PooledSandbox
	for _, tp := range p.pools {
		excess := len(tp.warm) - tp.target
		if excess <= 0 {
			continue
		}
		// FIFO: oldest first so the survivors are the freshest.
		victims = append(victims, tp.warm[:excess]...)
		tp.warm = tp.warm[excess:]
	}
	p.mu.Unlock()
	for _, v := range victims {
		p.destroyMember(ctx, v, "shrink")
	}
}

// adjustAll re-evaluates every pool's adaptive target from the last hour
// of hit traffic and garbage-collects fully-drained idle pools. Runs once
// per adjustEvery tick.
func (p *PoolManager) adjustAll() {
	now := time.Now()
	var gc []string
	p.mu.Lock()
	for hash, tp := range p.pools {
		p.pruneWindowsLocked(tp, now)
		prev := tp.target
		tp.target = p.computeTargetLocked(tp, now)
		if tp.target != prev {
			p.logger.Info("pool: adaptive resize",
				"template", hash, "target", tp.target, "prev", prev,
				"hits_window", len(tp.hitWindow), "warm", len(tp.warm))
		}
		// GC a non-system pool only once it is fully drained, idle to a
		// zero target, and has nothing inflight.
		if hash != p.cfg.SystemTemplate &&
			tp.target == 0 && len(tp.warm) == 0 && tp.warming == 0 {
			gc = append(gc, hash)
		}
	}
	for _, hash := range gc {
		delete(p.pools, hash)
		p.logger.Info("pool: garbage-collected idle template pool", "template", hash)
	}
	p.mu.Unlock()
}

// computeTargetLocked applies the adaptive-sizing rules for one pool:
// idle pools drain, busy pools grow, and the system template is floored
// at SystemMinTarget so it never starves. Caller holds p.mu.
func (p *PoolManager) computeTargetLocked(tp *templatePool, now time.Time) int {
	hits := len(tp.hitWindow)
	idle := now.Sub(tp.lastHitAt)
	var target int
	switch {
	case idle >= p.cfg.DrainAfterIdle:
		target = 0
	case idle >= poolDrainHalfIdle:
		target = 1
	case hits >= p.cfg.HitThreshHigh:
		target = poolTargetHigh
	case hits >= p.cfg.HitThreshMed:
		target = poolTargetMedium
	default:
		target = p.cfg.NewTemplate
	}
	if tp.hash == p.cfg.SystemTemplate && target < p.cfg.SystemMinTarget {
		target = p.cfg.SystemMinTarget
	}
	if target > p.cfg.PerTemplateMax {
		target = p.cfg.PerTemplateMax
	}
	return target
}

// pruneWindowsLocked drops hit/miss timestamps older than the adaptive
// window so the counts reflect only recent traffic. Caller holds p.mu.
func (p *PoolManager) pruneWindowsLocked(tp *templatePool, now time.Time) {
	cutoff := now.Add(-p.cfg.AdaptiveWindow)
	tp.hitWindow = pruneTimestamps(tp.hitWindow, cutoff)
	tp.missWindow = pruneTimestamps(tp.missWindow, cutoff)
}

// pruneTimestamps returns ts with every entry at or before cutoff dropped.
// The slice is assumed roughly time-ordered (appends only), so a single
// forward scan suffices.
func pruneTimestamps(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	if i == 0 {
		return ts
	}
	return append(ts[:0], ts[i:]...)
}

// rotateStale destroys warm members older than staleAfter across every
// pool so the pools don't accumulate dirty kernel state. Replacements
// come on the next replenish tick.
func (p *PoolManager) rotateStale(ctx context.Context) {
	now := time.Now()
	var stale []*PooledSandbox
	p.mu.Lock()
	for _, tp := range p.pools {
		kept := tp.warm[:0]
		for _, ps := range tp.warm {
			if now.Sub(ps.CreatedAt) > p.staleAfter {
				stale = append(stale, ps)
				continue
			}
			kept = append(kept, ps)
		}
		tp.warm = kept
	}
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
	var victims []*PooledSandbox
	for _, tp := range p.pools {
		victims = append(victims, tp.warm...)
		tp.warm = nil
	}
	p.pools = make(map[string]*templatePool)
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
