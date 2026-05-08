// Package master is the control-plane logic for vajra-master. It is
// stateless: every decision is recomputed from the database on each call.
// The scheduler and reconciler in this package are the two long-lived
// pieces of behaviour that don't sit behind an HTTP handler.
package master

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// heartbeatStaleAfter bounds how long ago a node's last heartbeat may be
// before we consider it unschedulable. The agent pushes heartbeats every
// 30s in production, so 90s tolerates two missed beats.
const heartbeatStaleAfter = 90 * time.Second

// SchedRequest is what handlers pass to the scheduler. Region is optional;
// the empty string means "any active cluster".
type SchedRequest struct {
	AccountID string
	VCPUs     int
	MemoryMB  int
	DiskGB    int
	Region    string
}

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
	// now is overridable so tests can pin a deterministic clock for the
	// stale-heartbeat check. Production uses time.Now.
	now func() time.Time
}

// NewScheduler builds a dbScheduler. quotaProvider may be nil, in which
// case DefaultQuota applies to every account.
func NewScheduler(s store.Store, quotaProvider QuotaProvider) *dbScheduler {
	return &dbScheduler{store: s, quota: quotaProvider, now: time.Now}
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
	for _, n := range nodes {
		if n.State != models.NodeStateActive {
			continue
		}
		if n.LastHeartbeat.Before(cutoff) {
			continue
		}
		freeCPU := n.Capacity.TotalCPU - n.UsedResources.UsedCPU
		freeMemMB := n.Capacity.TotalMemoryMB - n.UsedResources.UsedMemoryMB
		freeDiskGB := n.Capacity.TotalDiskGB - n.UsedResources.UsedDiskGB
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
