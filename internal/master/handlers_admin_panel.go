// Package master — handlers_admin_panel.go: the cluster admin panel's
// node, sandbox and log endpoints. Every handler is gated by requireAdmin
// (is_admin column / VAJRA_ADMIN_EMAIL allowlist). The account-management
// half of the panel lives in handlers_admin_accounts.go.
package master

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// adminNodeStaleAfter is how long a node may go without a heartbeat
// before the panel flags it unhealthy. The agent heartbeats well inside
// this window; 90s tolerates one missed beat plus jitter.
const adminNodeStaleAfter = 90 * time.Second

// adminListLimit is the ceiling for the cross-account scans the panel
// runs (nodes, sandboxes, accounts). It matches the store's own cap.
const adminListLimit = 1000

// ---------- Cluster overview ----------

// adminAlert is one operator-facing warning on the overview tab.
type adminAlert struct {
	Level   string `json:"level"` // "warning" | "critical"
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// adminResourceTotals is a vCPU/memory/disk triple.
type adminResourceTotals struct {
	VCPU     int `json:"vcpu"`
	MemoryMB int `json:"memory_mb"`
	DiskGB   int `json:"disk_gb"`
}

// adminOverview is the GET /v1/admin/cluster/overview payload.
type adminOverview struct {
	TotalNodes       int                 `json:"total_nodes"`
	ActiveNodes      int                 `json:"active_nodes"`
	TotalSandboxes   int                 `json:"total_sandboxes"`
	RunningSandboxes int                 `json:"running_sandboxes"`
	TotalAccounts    int                 `json:"total_accounts"`
	SpendToday       float64             `json:"spend_today"`
	Capacity         adminResourceTotals `json:"capacity"`
	Used             adminResourceTotals `json:"used"`
	Alerts           []adminAlert        `json:"alerts"`
}

// adminClusterOverview aggregates the headline numbers and active alerts
// for the panel's Overview tab.
func (h *Handlers) adminClusterOverview(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	ctx := r.Context()
	nodes, err := h.Store.Nodes().List(ctx, store.ListOpts{Limit: adminListLimit})
	if err != nil {
		h.log().Error("adminOverview: nodes", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	sandboxes, err := h.Store.Sandboxes().ListAll(ctx, store.ListOpts{Limit: adminListLimit})
	if err != nil {
		h.log().Error("adminOverview: sandboxes", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	accounts, err := h.Store.Accounts().List(ctx, store.ListOpts{Limit: adminListLimit})
	if err != nil {
		h.log().Error("adminOverview: accounts", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	out := adminOverview{
		TotalNodes:    len(nodes),
		TotalAccounts: len(accounts),
		Alerts:        []adminAlert{},
	}
	now := h.now()
	for _, n := range nodes {
		out.Capacity.VCPU += n.Capacity.TotalCPU
		out.Capacity.MemoryMB += n.Capacity.TotalMemoryMB
		out.Capacity.DiskGB += n.Capacity.TotalDiskGB
		out.Used.VCPU += n.UsedResources.UsedCPU
		out.Used.MemoryMB += n.UsedResources.UsedMemoryMB
		out.Used.DiskGB += n.UsedResources.UsedDiskGB
		switch {
		case n.State == models.NodeStateActive:
			out.ActiveNodes++
			if now.Sub(n.LastHeartbeat) > adminNodeStaleAfter {
				out.Alerts = append(out.Alerts, adminAlert{
					Level: "critical", Kind: "node_down",
					Message: "node " + n.Hostname + " has not sent a heartbeat in over 90s",
				})
			}
		case n.State == models.NodeStateOffline || n.State == models.NodeStateDecommissioned:
			out.Alerts = append(out.Alerts, adminAlert{
				Level: "critical", Kind: "node_down",
				Message: "node " + n.Hostname + " is " + strings.ToLower(string(n.State)),
			})
		case n.State == models.NodeStateDraining || n.State == models.NodeStateCordoned:
			out.Alerts = append(out.Alerts, adminAlert{
				Level: "warning", Kind: "node_unschedulable",
				Message: "node " + n.Hostname + " is " + strings.ToLower(string(n.State)) + " — not accepting new sandboxes",
			})
		}
	}

	errored := 0
	for _, sb := range sandboxes {
		if sb.State == models.SandboxStateDestroyed {
			continue
		}
		out.TotalSandboxes++
		if sb.State == models.SandboxStateRunning {
			out.RunningSandboxes++
		}
		if sb.State == models.SandboxStateError {
			errored++
		}
	}
	if errored > 0 {
		out.Alerts = append(out.Alerts, adminAlert{
			Level: "warning", Kind: "sandbox_error",
			Message: plural(errored, "sandbox", "sandboxes") + " in ERROR state",
		})
	}
	suspended := 0
	for _, a := range accounts {
		if a.Suspended {
			suspended++
		}
	}
	if suspended > 0 {
		out.Alerts = append(out.Alerts, adminAlert{
			Level: "warning", Kind: "account_suspended",
			Message: plural(suspended, "account", "accounts") + " suspended",
		})
	}
	out.SpendToday = h.spendSinceMidnight(ctx, accounts, now)
	writeJSON(w, http.StatusOK, out)
}

// spendSinceMidnight sums metered cost for every account from 00:00 UTC
// to now. Best-effort: a per-account rollup error is logged and skipped
// so one bad row never blanks the whole overview.
func (h *Handlers) spendSinceMidnight(ctx context.Context, accounts []*models.Account, now time.Time) float64 {
	usage := h.Store.Usage()
	if usage == nil {
		return 0
	}
	midnight := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	total := 0.0
	for _, a := range accounts {
		rollup, err := usage.SumByAccount(ctx, a.ID, midnight, now)
		if err != nil {
			h.log().Warn("adminOverview: spend rollup", "account_id", a.ID, "err", err)
			continue
		}
		total += rollup.Cost
	}
	return total
}

// plural renders "1 node" / "3 nodes".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// ---------- Nodes ----------

// adminNodeView is one row of the panel's Nodes tab: the node plus the
// derived fields the table renders.
type adminNodeView struct {
	models.Node
	SandboxCount int  `json:"sandbox_count"`
	Healthy      bool `json:"healthy"`
}

// adminListNodes returns every node enriched with its live sandbox count
// and a heartbeat-freshness flag.
func (h *Handlers) adminListNodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	ctx := r.Context()
	nodes, err := h.Store.Nodes().List(ctx, store.ListOpts{Limit: adminListLimit})
	if err != nil {
		h.log().Error("adminListNodes", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	sandboxes, err := h.Store.Sandboxes().ListAll(ctx, store.ListOpts{Limit: adminListLimit})
	if err != nil {
		h.log().Error("adminListNodes: sandboxes", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	counts := map[string]int{}
	for _, sb := range sandboxes {
		if sb.State == models.SandboxStateDestroyed || sb.NodeID == nil {
			continue
		}
		counts[*sb.NodeID]++
	}
	now := h.now()
	out := make([]adminNodeView, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, adminNodeView{
			Node:         *n,
			SandboxCount: counts[n.ID],
			Healthy:      now.Sub(n.LastHeartbeat) <= adminNodeStaleAfter,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	writeJSON(w, http.StatusOK, out)
}

// adminSetNodeState is the shared body of the drain/cordon/uncordon
// actions: confirm the node exists, then write the requested state.
func (h *Handlers) adminSetNodeState(w http.ResponseWriter, r *http.Request, state models.NodeState) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing node id")
		return
	}
	if _, err := h.Store.Nodes().GetByID(r.Context(), id); err != nil {
		writeErr(w, translateStoreErr(err), "node not found")
		return
	}
	if err := h.Store.Nodes().UpdateState(r.Context(), id, state); err != nil {
		h.log().Error("adminSetNodeState", "err", err, "state", state)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.log().Info("admin: node state changed", "node_id", id, "state", state)
	writeJSON(w, http.StatusOK, map[string]string{"node_id": id, "state": string(state)})
}

// adminDrainNode flips a node to DRAINING — existing sandboxes keep
// running, the scheduler places nothing new.
func (h *Handlers) adminDrainNode(w http.ResponseWriter, r *http.Request) {
	h.adminSetNodeState(w, r, models.NodeStateDraining)
}

// adminCordonNode marks a node unschedulable without otherwise touching
// its workload.
func (h *Handlers) adminCordonNode(w http.ResponseWriter, r *http.Request) {
	h.adminSetNodeState(w, r, models.NodeStateCordoned)
}

// adminUncordonNode returns a drained/cordoned node to ACTIVE so it
// accepts new sandboxes again.
func (h *Handlers) adminUncordonNode(w http.ResponseWriter, r *http.Request) {
	h.adminSetNodeState(w, r, models.NodeStateActive)
}

// adminTerminateNode terminates the EC2 instance behind a node and
// removes the node row. Only autoscaled (vajra:managed) instances can be
// terminated this way; everything else returns a 400 telling the
// operator to decommission the host by hand.
func (h *Handlers) adminTerminateNode(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing node id")
		return
	}
	if h.Autoscaler == nil || !h.Autoscaler.Config.Enabled {
		writeErr(w, http.StatusBadRequest,
			"terminate is only available for autoscaled nodes (autoscaler not configured)")
		return
	}
	if err := h.Autoscaler.TerminateNode(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "node not found")
			return
		}
		h.log().Error("adminTerminateNode", "err", err, "node_id", id)
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"node_id": id, "status": "terminated"})
}

// ---------- Sandboxes ----------

// adminSandboxView is one row of the panel's Sandboxes tab.
type adminSandboxView struct {
	models.Sandbox
	AccountEmail string `json:"account_email"`
	NodeHostname string `json:"node_hostname,omitempty"`
	AgeSeconds   int64  `json:"age_seconds"`
}

// adminListSandboxes returns sandboxes across every account, joined to
// their owner email and node hostname. Supports ?state=, ?account=
// (email substring), ?node= (node id) filters plus limit/offset paging
// applied after filtering.
func (h *Handlers) adminListSandboxes(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	ctx := r.Context()
	sandboxes, err := h.Store.Sandboxes().ListAll(ctx, store.ListOpts{Limit: adminListLimit})
	if err != nil {
		h.log().Error("adminListSandboxes", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	emails := h.accountEmailIndex(ctx)
	hostnames := h.nodeHostnameIndex(ctx)

	q := r.URL.Query()
	wantState := strings.ToUpper(strings.TrimSpace(q.Get("state")))
	wantAccount := strings.ToLower(strings.TrimSpace(q.Get("account")))
	wantNode := strings.TrimSpace(q.Get("node"))

	now := h.now()
	filtered := make([]adminSandboxView, 0, len(sandboxes))
	for _, sb := range sandboxes {
		if wantState != "" && string(sb.State) != wantState {
			continue
		}
		email := emails[sb.AccountID]
		if wantAccount != "" && !strings.Contains(strings.ToLower(email), wantAccount) {
			continue
		}
		nodeID := ""
		if sb.NodeID != nil {
			nodeID = *sb.NodeID
		}
		if wantNode != "" && nodeID != wantNode {
			continue
		}
		filtered = append(filtered, adminSandboxView{
			Sandbox:      *sb,
			AccountEmail: email,
			NodeHostname: hostnames[nodeID],
			AgeSeconds:   int64(now.Sub(sb.CreatedAt).Seconds()),
		})
	}

	total := len(filtered)
	opts := parseListOpts(r)
	page := paginate(filtered, opts.Offset, opts.Limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"sandboxes": page,
		"total":     total,
		"limit":     opts.Limit,
		"offset":    opts.Offset,
	})
}

// paginate returns the [offset, offset+limit) slice of s, clamped to its
// bounds. A non-positive limit means "no limit" (return from offset on).
func paginate(s []adminSandboxView, offset, limit int) []adminSandboxView {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(s) {
		return []adminSandboxView{}
	}
	end := len(s)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return s[offset:end]
}

// adminStopSandbox stops any account's RUNNING sandbox.
func (h *Handlers) adminStopSandbox(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	sb, ok := h.resolveAdminSandbox(w, r)
	if !ok {
		return
	}
	if sb.State != models.SandboxStateRunning {
		writeErr(w, http.StatusConflict, "sandbox state "+string(sb.State)+" not eligible for stop")
		return
	}
	if node := h.sandboxNode(r.Context(), sb); node != nil {
		ctx, cancel := context.WithTimeout(r.Context(), dispatchTimeout)
		defer cancel()
		if err := h.Pool.ClientFor(node).StopSandbox(ctx, sb.ID); err != nil {
			h.log().Error("adminStopSandbox: dispatch", "err", err, "sandbox_id", sb.ID)
			writeErr(w, http.StatusBadGateway, "agent stop failed: "+err.Error())
			return
		}
	}
	if err := h.Store.Sandboxes().UpdateState(r.Context(), sb.AccountID, sb.ID, models.SandboxStateStopped); err != nil {
		h.log().Error("adminStopSandbox: state", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.writeSandboxStateCache(r.Context(), sb.ID, models.SandboxStateStopped)
	h.log().Info("admin: sandbox stopped", "sandbox_id", sb.ID, "account_id", sb.AccountID)
	writeJSON(w, http.StatusOK, map[string]string{"sandbox_id": sb.ID, "state": string(models.SandboxStateStopped)})
}

// adminDestroySandbox force-destroys any account's sandbox. The agent
// dispatch is best-effort — the row is flipped to DESTROYED even if the
// node is unreachable, so a wedged sandbox can always be cleared.
func (h *Handlers) adminDestroySandbox(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	sb, ok := h.resolveAdminSandbox(w, r)
	if !ok {
		return
	}
	if sb.State == models.SandboxStateDestroyed {
		writeErr(w, http.StatusConflict, "sandbox already destroyed")
		return
	}
	if node := h.sandboxNode(r.Context(), sb); node != nil {
		ctx, cancel := context.WithTimeout(r.Context(), dispatchTimeout)
		defer cancel()
		if err := h.Pool.ClientFor(node).DestroySandbox(ctx, sb.ID); err != nil {
			h.log().Warn("adminDestroySandbox: dispatch failed, forcing row to DESTROYED",
				"err", err, "sandbox_id", sb.ID)
		}
	}
	if err := h.Store.Sandboxes().UpdateState(r.Context(), sb.AccountID, sb.ID, models.SandboxStateDestroyed); err != nil {
		h.log().Error("adminDestroySandbox: state", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.invalidateSandboxStateCache(r.Context(), sb.ID)
	h.log().Info("admin: sandbox destroyed", "sandbox_id", sb.ID, "account_id", sb.AccountID)
	writeJSON(w, http.StatusOK, map[string]string{"sandbox_id": sb.ID, "state": string(models.SandboxStateDestroyed)})
}

// resolveAdminSandbox loads the {id} sandbox without account scoping and
// writes a 400/404 on failure.
func (h *Handlers) resolveAdminSandbox(w http.ResponseWriter, r *http.Request) (*models.Sandbox, bool) {
	id := pathID(r)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing sandbox id")
		return nil, false
	}
	sb, err := h.Store.Sandboxes().GetByIDUnscoped(r.Context(), id)
	if err != nil {
		writeErr(w, translateStoreErr(err), "sandbox not found")
		return nil, false
	}
	return sb, true
}

// sandboxNode resolves a sandbox's node, or nil when it is unplaced or
// the node row has gone away.
func (h *Handlers) sandboxNode(ctx context.Context, sb *models.Sandbox) *models.Node {
	if sb.NodeID == nil || *sb.NodeID == "" {
		return nil
	}
	node, err := h.Store.Nodes().GetByID(ctx, *sb.NodeID)
	if err != nil {
		return nil
	}
	return node
}

// accountEmailIndex returns an account-id → email map for join columns.
func (h *Handlers) accountEmailIndex(ctx context.Context) map[string]string {
	out := map[string]string{}
	accounts, err := h.Store.Accounts().List(ctx, store.ListOpts{Limit: adminListLimit})
	if err != nil {
		h.log().Warn("accountEmailIndex", "err", err)
		return out
	}
	for _, a := range accounts {
		out[a.ID] = a.Email
	}
	return out
}

// nodeHostnameIndex returns a node-id → hostname map for join columns.
func (h *Handlers) nodeHostnameIndex(ctx context.Context) map[string]string {
	out := map[string]string{}
	nodes, err := h.Store.Nodes().List(ctx, store.ListOpts{Limit: adminListLimit})
	if err != nil {
		h.log().Warn("nodeHostnameIndex", "err", err)
		return out
	}
	for _, n := range nodes {
		out[n.ID] = n.Hostname
	}
	return out
}

// ---------- Logs ----------

// adminLogs streams recent master log records from the in-process ring
// buffer. Query params: source (master|agent), tail (line count),
// level (minimum level), sandbox_id (substring filter).
func (h *Handlers) adminLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	q := r.URL.Query()
	tail, _ := strconv.Atoi(q.Get("tail"))
	source := strings.TrimSpace(q.Get("source"))
	if source == "" {
		source = "master"
	}
	entries := []AdminLogEntry{}
	if h.LogBuffer != nil {
		entries = h.LogBuffer.Tail(LogQuery{
			Tail:      tail,
			Level:     q.Get("level"),
			Source:    source,
			SandboxID: q.Get("sandbox_id"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source":  source,
		"count":   len(entries),
		"entries": entries,
	})
}
