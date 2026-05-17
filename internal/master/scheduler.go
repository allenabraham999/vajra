// Package master is the control-plane logic for vajra-master. It is
// stateless: every decision is recomputed from the database on each call.
// The scheduler and reconciler in this package are the two long-lived
// pieces of behaviour that don't sit behind an HTTP handler.
package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/allenabraham999/vajra/internal/cache"
	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// heartbeatStaleAfter bounds how long ago a node's last heartbeat may be
// before we consider it unschedulable. The agent pushes heartbeats every
// 30s in production, so 90s tolerates two missed beats.
const heartbeatStaleAfter = 90 * time.Second

// poolProbeTimeout bounds the per-node pre-warm-pool probe PickNode runs
// when a request carries a template hash. Probes fire concurrently, so
// this is the worst-case wall time even if one agent is unreachable.
const poolProbeTimeout = 1 * time.Second

// SchedRequest is what handlers pass to the scheduler. Region is optional;
// the empty string means "any active cluster". TemplateHash, when set,
// lets PickNode steer the create onto a node with a matching warm
// pre-warm pool so the create can be served as an instant pool hit.
type SchedRequest struct {
	AccountID    string
	VCPUs        int
	MemoryMB     int
	DiskGB       int
	Region       string
	TemplateHash string
}

// NodePoolProber probes a node's per-template warm pools. For the given
// template hash it returns available (warm members ready right now) and
// poolCapacity (the node's total warm-VM budget — >0 only for nodes that
// run a per-template pool at all). PickNode prefers a node with warm
// members for the exact template; failing that, a pool-capable node, so
// a new template's pool gets seeded somewhere it can actually warm.
// ok=false means the probe failed and the node gets no pool affinity.
type NodePoolProber func(ctx context.Context, node *models.Node, templateHash string) (available, poolCapacity int, ok bool)

// Quota is a per-account ceiling enforced before scheduling. Values are
// hard caps; CheckQuota rejects a request that would push the account to
// or past the limit.
type Quota struct {
	MaxSandboxes int
	MaxVCPUs     int
	MaxMemoryMB  int
}

// DefaultQuota is the placeholder ceiling used when no account-specific
// quota is configured. Numbers are intentionally generous for the demo.
var DefaultQuota = Quota{MaxSandboxes: 50, MaxVCPUs: 200, MaxMemoryMB: 256 * 1024}

// ErrNoCluster is returned when no cluster is in ACTIVE state, or when a
// region was requested and no active cluster matches it.
var ErrNoCluster = errors.New("scheduler: no eligible cluster")

// ErrNoCapacity is returned when every active node in the chosen cluster
// either has a stale heartbeat or insufficient free CPU/RAM/disk.
var ErrNoCapacity = errors.New("scheduler: no node has capacity")

// ErrQuotaExceeded is returned when the request would push the account
// past its sandbox-count, vCPU sum, or memory sum quota.
var ErrQuotaExceeded = errors.New("scheduler: account quota exceeded")

// Scheduler is the narrow interface handlers depend on so they can be
// tested with a fake.
type Scheduler interface {
	Schedule(ctx context.Context, req SchedRequest) (*models.Cluster, *models.Node, error)
	PickCluster(ctx context.Context, req SchedRequest) (*models.Cluster, error)
	PickNode(ctx context.Context, cluster *models.Cluster, req SchedRequest) (*models.Node, error)
	CheckQuota(ctx context.Context, accountID string, req SchedRequest) error
}

// QuotaProvider returns the quota for accountID. Implementations may look
// the value up from a config table; nil falls back to DefaultQuota.
type QuotaProvider func(accountID string) Quota

// dbScheduler is the Store-backed Scheduler. It holds no per-request
// state — every call hits the store fresh — so the master process stays
// stateless and horizontally scalable.
type dbScheduler struct {
	store store.Store
	quota QuotaProvider
	cache cache.Cache
	log   *slog.Logger
	// poolProbe, when set, lets PickNode check each node's pre-warm pool.
	// nil leaves scheduling purely resource-fit.
	poolProbe NodePoolProber
	// now is overridable so tests can pin a deterministic clock for the
	// stale-heartbeat check. Production uses time.Now.
	now func() time.Time
}

// NewScheduler builds a dbScheduler. quotaProvider may be nil, in which
// case DefaultQuota applies to every account.
func NewScheduler(s store.Store, quotaProvider QuotaProvider) *dbScheduler {
	return &dbScheduler{store: s, quota: quotaProvider, now: time.Now, cache: cache.NewNoopCache(), log: slog.Default()}
}

// WithCache wires a cache.Cache into the scheduler. Used by main to
// hand the same Redis client to both handlers and scheduler. Returns
// the receiver for chaining.
func (d *dbScheduler) WithCache(c cache.Cache) *dbScheduler {
	if c != nil {
		d.cache = c
	}
	return d
}

// WithLogger overrides the scheduler's logger.
func (d *dbScheduler) WithLogger(l *slog.Logger) *dbScheduler {
	if l != nil {
		d.log = l
	}
	return d
}

// WithPoolProber wires a per-node pre-warm-pool probe into the scheduler
// so PickNode can prefer a node holding a warm member that matches the
// request's template. nil leaves scheduling purely resource-fit. Returns
// the receiver for chaining.
func (d *dbScheduler) WithPoolProber(p NodePoolProber) *dbScheduler {
	d.poolProbe = p
	return d
}

// nodeResourcesPayload is the JSON shape we store in Redis under
// node:{id}:resources. Keep stable so heartbeat writers and PickNode
// readers stay aligned.
type nodeResourcesPayload struct {
	TotalCPU      int       `json:"total_cpu"`
	UsedCPU       int       `json:"used_cpu"`
	TotalMemoryMB int       `json:"total_mem_mb"`
	UsedMemoryMB  int       `json:"used_mem_mb"`
	TotalDiskGB   int       `json:"total_disk_gb"`
	UsedDiskGB    int       `json:"used_disk_gb"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

// Schedule composes the full pipeline: quota check, cluster pick, node
// pick. Both the cluster and node are returned so the caller can persist
// the placement.
func (d *dbScheduler) Schedule(ctx context.Context, req SchedRequest) (*models.Cluster, *models.Node, error) {
	if err := d.CheckQuota(ctx, req.AccountID, req); err != nil {
		return nil, nil, err
	}
	cluster, err := d.PickCluster(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	node, err := d.PickNode(ctx, cluster, req)
	if err != nil {
		return nil, nil, err
	}
	return cluster, node, nil
}

// PickCluster returns the first ACTIVE cluster matching the request's
// region preference. If req.Region is set we only consider clusters in
// that region — we never silently fall through to a different region,
// because callers ask for a region for a reason (data residency,
// latency). If req.Region is empty any ACTIVE cluster is acceptable.
func (d *dbScheduler) PickCluster(ctx context.Context, req SchedRequest) (*models.Cluster, error) {
	clusters, err := d.store.Clusters().List(ctx, store.ListOpts{})
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	// Filter to ACTIVE up front; sort by ID for deterministic tie-break.
	active := make([]*models.Cluster, 0, len(clusters))
	for _, c := range clusters {
		if c.State == models.ClusterStateActive {
			active = append(active, c)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })

	if req.Region != "" {
		for _, c := range active {
			if c.Region == req.Region {
				return c, nil
			}
		}
		// Region preference must be honored when set — do not fall through.
		return nil, ErrNoCluster
	}
	if len(active) == 0 {
		return nil, ErrNoCluster
	}
	return active[0], nil
}

// PickNode returns the best-fit ACTIVE node in cluster whose heartbeat is
// fresh and that has enough free CPU, memory, and disk for req.
//
// Scoring follows the brief literally: score is the sum of remaining
// resources after placement, and the highest-scoring node wins. That is
// effectively worst-fit (leave the most slack), not best-fit. The reason
// we pick worst-fit here is to spread load so no single node becomes a
// hotspot for vsock/disk contention; switching to best-fit (lowest score)
// would tighten packing density at the cost of tail-latency variance.
// Tie-broken by node ID lex order so the scheduler is deterministic.
func (d *dbScheduler) PickNode(ctx context.Context, cluster *models.Cluster, req SchedRequest) (*models.Node, error) {
	nodes, err := d.store.Nodes().ListByCluster(ctx, cluster.ID, store.ListOpts{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	cutoff := d.now().Add(-heartbeatStaleAfter)

	type scored struct {
		node  *models.Node
		score int
	}
	candidates := make([]scored, 0, len(nodes))
	// fresh collects every ACTIVE, heartbeat-fresh node regardless of
	// free capacity — the pool-affinity pass below considers it, because
	// a pool hit reuses an already-running warm VM rather than claiming
	// fresh capacity.
	fresh := make([]*models.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.State != models.NodeStateActive {
			continue
		}
		// Prefer Redis-cached usage when available — heartbeats refresh
		// it every 5s, so this dodges any lag between the heartbeat
		// handler's UpdateUsage write and the next ListByCluster read.
		// Cache miss / parse error / stale entry → fall through to the
		// row we already loaded from Postgres. Hit also gives us a
		// fresher heartbeat timestamp than what's on the node row.
		usedCPU := n.UsedResources.UsedCPU
		usedMemMB := n.UsedResources.UsedMemoryMB
		usedDiskGB := n.UsedResources.UsedDiskGB
		hb := n.LastHeartbeat
		if d.cache != nil {
			if raw, err := d.cache.Get(ctx, cache.NodeResourcesKey(n.ID)); err == nil {
				var p nodeResourcesPayload
				if jerr := json.Unmarshal([]byte(raw), &p); jerr == nil {
					usedCPU = p.UsedCPU
					usedMemMB = p.UsedMemoryMB
					usedDiskGB = p.UsedDiskGB
					if !p.LastHeartbeat.IsZero() {
						hb = p.LastHeartbeat
					}
				}
			}
		}
		if hb.Before(cutoff) {
			continue
		}
		fresh = append(fresh, n)
		freeCPU := n.Capacity.TotalCPU - usedCPU
		freeMemMB := n.Capacity.TotalMemoryMB - usedMemMB
		freeDiskGB := n.Capacity.TotalDiskGB - usedDiskGB
		if freeCPU < req.VCPUs || freeMemMB < req.MemoryMB || freeDiskGB < req.DiskGB {
			continue
		}
		// Score = remaining resources AFTER placement. Memory is divided
		// by 1024 so MB doesn't dominate CPU/disk units — keeps the
		// dimensions roughly comparable in a single sum.
		score := (freeCPU - req.VCPUs) +
			(freeMemMB-req.MemoryMB)/1024 +
			(freeDiskGB - req.DiskGB)
		candidates = append(candidates, scored{node: n, score: score})
	}
	// Pool affinity: prefer a node that can serve this template from a
	// warm pre-warm member, falling back to a pool-capable node so a new
	// template's pool gets seeded where it can actually warm.
	candidateIDs := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		candidateIDs[c.node.ID] = true
	}
	if node := d.pickPoolNode(ctx, fresh, candidateIDs, req); node != nil {
		return node, nil
	}
	if len(candidates) == 0 {
		return nil, ErrNoCapacity
	}
	// Highest score first; tie-break by ID lex for determinism.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].node.ID < candidates[j].node.ID
	})
	return candidates[0].node, nil
}

// pickPoolNode picks the node best placed to serve req via the pre-warm
// pool. It probes every heartbeat-fresh node concurrently for
// req.TemplateHash and applies two tiers:
//
//	tier 1 — a node with warm members ready: placed there even if it
//	         would fail the resource-fit filter, since a pool hit reuses
//	         an already-running VM rather than claiming fresh capacity.
//	tier 2 — no warm members anywhere, but a pool-capable node that also
//	         has real capacity: placed there (a cold create) so that
//	         node seeds and warms a pool for next time.
//
// Returns nil — and the caller falls back to plain resource-fit — when
// the request carries no template, no prober is wired, or neither tier
// matches. hasCapacity is the set of node IDs that passed the resource
// filter; tier 2 only ever picks from it.
func (d *dbScheduler) pickPoolNode(ctx context.Context, fresh []*models.Node, hasCapacity map[string]bool, req SchedRequest) *models.Node {
	if req.TemplateHash == "" || d.poolProbe == nil || len(fresh) == 0 {
		return nil
	}
	type probed struct {
		node      *models.Node
		available int
		capacity  int
	}
	probeCtx, cancel := context.WithTimeout(ctx, poolProbeTimeout)
	defer cancel()
	results := make(chan probed, len(fresh))
	for _, n := range fresh {
		go func(n *models.Node) {
			avail, capacity, ok := d.poolProbe(probeCtx, n, req.TemplateHash)
			if !ok {
				results <- probed{}
				return
			}
			results <- probed{node: n, available: avail, capacity: capacity}
		}(n)
	}
	var warmBest, capableBest probed
	for range fresh {
		p := <-results
		if p.node == nil {
			continue
		}
		if p.available > 0 {
			if warmBest.node == nil || p.available > warmBest.available ||
				(p.available == warmBest.available && p.node.ID < warmBest.node.ID) {
				warmBest = p
			}
		}
		if p.capacity > 0 && hasCapacity[p.node.ID] {
			if capableBest.node == nil || p.capacity > capableBest.capacity ||
				(p.capacity == capableBest.capacity && p.node.ID < capableBest.node.ID) {
				capableBest = p
			}
		}
	}
	if warmBest.node != nil {
		d.log.Info("scheduler: pool-affinity placement",
			"node", warmBest.node.ID, "template", req.TemplateHash,
			"available", warmBest.available, "tier", "warm")
		return warmBest.node
	}
	if capableBest.node != nil {
		d.log.Info("scheduler: pool-affinity placement",
			"node", capableBest.node.ID, "template", req.TemplateHash,
			"tier", "pool-capable")
		return capableBest.node
	}
	return nil
}

// CheckQuota enforces both interpretations of "quota": a count of active
// sandboxes and a sum of their requested resources. We list up to 1000
// sandboxes for the account — past that the account is so large it should
// have a custom QuotaProvider with hard limits below 1000 anyway.
func (d *dbScheduler) CheckQuota(ctx context.Context, accountID string, req SchedRequest) error {
	q := DefaultQuota
	if d.quota != nil {
		q = d.quota(accountID)
	}
	sandboxes, err := d.store.Sandboxes().ListByAccount(ctx, accountID, store.ListOpts{Limit: 1000})
	if err != nil {
		return fmt.Errorf("list sandboxes for quota: %w", err)
	}
	activeCount := 0
	usedVCPUs := 0
	usedMemMB := 0
	for _, s := range sandboxes {
		if s.State == models.SandboxStateDestroyed || s.State == models.SandboxStateError {
			continue
		}
		activeCount++
		usedVCPUs += s.Config.VCPUs
		usedMemMB += s.Config.MemoryMB
	}
	if activeCount >= q.MaxSandboxes {
		return fmt.Errorf("%w: sandbox count %d >= max %d", ErrQuotaExceeded, activeCount, q.MaxSandboxes)
	}
	if usedVCPUs+req.VCPUs > q.MaxVCPUs {
		return fmt.Errorf("%w: vCPUs %d + %d > max %d", ErrQuotaExceeded, usedVCPUs, req.VCPUs, q.MaxVCPUs)
	}
	if usedMemMB+req.MemoryMB > q.MaxMemoryMB {
		return fmt.Errorf("%w: memory %dMB + %dMB > max %dMB", ErrQuotaExceeded, usedMemMB, req.MemoryMB, q.MaxMemoryMB)
	}
	return nil
}
