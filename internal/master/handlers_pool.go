// Package master — handlers_pool.go re-exposes every node's per-template
// pre-warm pools at GET /v1/pool/stats so the dashboard never has to know
// agent addresses. The pools live in vajra-agent; master probes each node
// concurrently, resolves template names, and returns an aggregated view.
package master

import (
	"context"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// poolStatsTimeout bounds the per-node pool-stats probes. Probes run
// concurrently, so this is the worst-case wall time even when several
// stale nodes are unreachable — the dashboard polls every few seconds
// and must not stall on a dead agent.
const poolStatsTimeout = 3 * time.Second

// AgentPoolStats mirrors the agent's GET /pool/stats body (NodePoolStats):
// the node's per-template warm pools plus its warm-VM budget. An old
// agent that still serves the flat schema decodes to a zero Capacity and
// nil Templates, which the master treats as "not pool-capable".
type AgentPoolStats struct {
	Capacity    int                      `json:"capacity"`
	TotalWarm   int                      `json:"total_warm"`
	TotalHits   int64                    `json:"total_hits"`
	TotalMisses int64                    `json:"total_misses"`
	Templates   []AgentTemplatePoolStats `json:"templates"`
}

// AgentTemplatePoolStats is one template's pool snapshot from an agent.
type AgentTemplatePoolStats struct {
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

// poolStatsResponse is the GET /v1/pool/stats body: a fleet-wide summary
// plus a per-node, per-template breakdown.
type poolStatsResponse struct {
	Global poolGlobalStats `json:"global"`
	Nodes  []poolNodeStats `json:"nodes"`
}

type poolGlobalStats struct {
	TotalWarmVMs  int     `json:"total_warm_vms"`
	TotalCapacity int     `json:"total_capacity"`
	TotalHits     int64   `json:"total_hits_24h"`
	TotalMisses   int64   `json:"total_misses_24h"`
	HitRatePct    float64 `json:"hit_rate_pct"`
}

type poolNodeStats struct {
	NodeID    string                 `json:"node_id"`
	Capacity  int                    `json:"capacity"`
	Templates []poolTemplateStatsOut `json:"templates"`
}

type poolTemplateStatsOut struct {
	TemplateName string     `json:"template_name"`
	TemplateHash string     `json:"template_hash"`
	TemplateID   string     `json:"template_id,omitempty"`
	Available    int        `json:"available"`
	Warming      int        `json:"warming"`
	TargetSize   int        `json:"target_size"`
	InUse        int        `json:"in_use"`
	HitsLastHour int        `json:"hits_last_hour"`
	LastHitAt    *time.Time `json:"last_hit_at,omitempty"`
}

// getPoolStats serves GET /v1/pool/stats. It never errors — an
// unreachable agent just drops out of the node list — so the dashboard
// degrades cleanly.
func (h *Handlers) getPoolStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := RequireAccount(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, h.buildPoolStats(r.Context()))
}

// buildPoolStats probes every active node's per-template pools
// concurrently and aggregates them. Template names are resolved from the
// store (cached per call). Probes that fail are skipped.
func (h *Handlers) buildPoolStats(ctx context.Context) poolStatsResponse {
	resp := poolStatsResponse{Nodes: []poolNodeStats{}}
	nodes, err := h.Store.Nodes().List(ctx, store.ListOpts{Limit: 200})
	if err != nil {
		h.log().Warn("getPoolStats: list nodes", "err", err)
		return resp
	}
	probeCtx, cancel := context.WithTimeout(ctx, poolStatsTimeout)
	defer cancel()

	type nodeResult struct {
		node  *models.Node
		stats *AgentPoolStats
	}
	results := make(chan nodeResult, len(nodes))
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
				results <- nodeResult{node: n}
				return
			}
			results <- nodeResult{node: n, stats: ps}
		}(n)
	}

	nameCache := map[string]string{}
	for i := 0; i < probes; i++ {
		r := <-results
		if r.stats == nil {
			continue
		}
		ns := poolNodeStats{
			NodeID:    r.node.ID,
			Capacity:  r.stats.Capacity,
			Templates: make([]poolTemplateStatsOut, 0, len(r.stats.Templates)),
		}
		for _, t := range r.stats.Templates {
			ns.Templates = append(ns.Templates, poolTemplateStatsOut{
				TemplateName: h.resolveTemplateName(ctx, nameCache, t.TemplateID, t.TemplateHash),
				TemplateHash: t.TemplateHash,
				TemplateID:   t.TemplateID,
				Available:    t.Available,
				Warming:      t.Warming,
				TargetSize:   t.TargetSize,
				InUse:        t.InUse,
				HitsLastHour: t.HitsLastHour,
				LastHitAt:    t.LastHitAt,
			})
			resp.Global.TotalWarmVMs += t.Available
		}
		sort.Slice(ns.Templates, func(a, b int) bool {
			return ns.Templates[a].TemplateName < ns.Templates[b].TemplateName
		})
		resp.Global.TotalCapacity += r.stats.Capacity
		resp.Global.TotalHits += r.stats.TotalHits
		resp.Global.TotalMisses += r.stats.TotalMisses
		resp.Nodes = append(resp.Nodes, ns)
	}
	sort.Slice(resp.Nodes, func(a, b int) bool {
		return resp.Nodes[a].NodeID < resp.Nodes[b].NodeID
	})
	if total := resp.Global.TotalHits + resp.Global.TotalMisses; total > 0 {
		raw := 100.0 * float64(resp.Global.TotalHits) / float64(total)
		resp.Global.HitRatePct = math.Round(raw*10) / 10
	}
	return resp
}

// resolveTemplateName maps a pool's (id, hash) to a human template name,
// memoised for the life of one request. ID is tried first (exact), then
// the content hash. An unknown template resolves to "".
func (h *Handlers) resolveTemplateName(ctx context.Context, cache map[string]string, id, hash string) string {
	key := id + "|" + hash
	if n, ok := cache[key]; ok {
		return n
	}
	name := ""
	if id != "" {
		if t, err := h.Store.Templates().GetByIDUnscoped(ctx, id); err == nil {
			name = t.Name
		}
	}
	if name == "" && hash != "" {
		if t, err := h.Store.Templates().GetByHash(ctx, hash); err == nil {
			name = t.Name
		}
	}
	cache[key] = name
	return name
}
