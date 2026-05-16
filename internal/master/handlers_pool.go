// Package master — handlers_pool.go re-exposes a node's pre-warm pool
// stats at GET /v1/pool/stats so the dashboard never has to know agent
// addresses. The pool itself lives in vajra-agent; master just probes a
// node and forwards the snapshot.
package master

import (
	"context"
	"net/http"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// poolStatsTimeout bounds the per-node pool-stats probes. Probes run
// concurrently, so this is the worst-case wall time even when several
// stale nodes are unreachable — the dashboard polls every few seconds
// and must not stall on a dead agent.
const poolStatsTimeout = 3 * time.Second

// AgentPoolStats mirrors the agent's PoolStats JSON (GET /pool/stats on a
// vajra-agent). Field tags match the agent wire format exactly.
type AgentPoolStats struct {
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

// getPoolStats returns pre-warm pool stats for the cluster. The pool is a
// per-agent structure, so master probes known nodes and returns the
// first one that reports a configured pool. With no reachable pool it
// returns a zero-value response (still 200) so the dashboard degrades
// cleanly rather than erroring.
func (h *Handlers) getPoolStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := RequireAccount(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, h.collectPoolStats(r.Context()))
}

// collectPoolStats probes active nodes for pool stats, preferring a node
// with a configured pool (non-empty Template). Probes run concurrently
// so a handful of stale, unreachable nodes cost one timeout in total
// rather than one each. It never errors — an unreachable agent just
// drops out of the result set.
func (h *Handlers) collectPoolStats(ctx context.Context) AgentPoolStats {
	nodes, err := h.Store.Nodes().List(ctx, store.ListOpts{Limit: 200})
	if err != nil {
		h.log().Warn("getPoolStats: list nodes", "err", err)
		return AgentPoolStats{}
	}
	probeCtx, cancel := context.WithTimeout(ctx, poolStatsTimeout)
	defer cancel()
	// Buffered to len(nodes) so a goroutine never blocks on send even
	// when we return early after finding a configured pool.
	results := make(chan *AgentPoolStats, len(nodes))
	probes := 0
	for _, n := range nodes {
		if n.State != models.NodeStateActive {
			continue
		}
		probes++
		go func(n *models.Node) {
			ps, perr := h.Pool.ClientFor(n).PoolStats(probeCtx)
			if perr != nil {
				h.log().Debug("getPoolStats: probe", "node", n.ID, "err", perr)
				results <- nil
				return
			}
			results <- ps
		}(n)
	}
	var fallback AgentPoolStats
	var gotAny bool
	for i := 0; i < probes; i++ {
		ps := <-results
		if ps == nil {
			continue
		}
		if ps.Template != "" {
			return *ps
		}
		if !gotAny {
			fallback, gotAny = *ps, true
		}
	}
	return fallback
}
