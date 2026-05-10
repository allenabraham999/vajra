// Package master — handlers_archive.go owns the archive/rehydrate REST
// surface. Archive is the cold-storage path for a stopped sandbox: the
// agent compresses the on-disk state into a .tar.zst (optionally
// uploaded to S3) and master flips the row to ARCHIVED. Rehydrate is
// the inverse — pull the archive back, register a STOPPED sandbox, and
// the user can /start it.
package master

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// archiveDispatchTimeout caps the long-running archive RPC. Compression of
// a couple of GB at zstd default levels takes seconds; an S3 round-trip
// can take longer on a slow network. 10 minutes gives us headroom.
const archiveDispatchTimeout = 10 * time.Minute

// archiveResponse is what the master returns to clients. Path is the
// archive locator (filesystem or s3://...); clients hold onto it and pass
// it back on rehydrate when the agent's default lookup wouldn't suffice
// (e.g. rehydrating onto a freshly provisioned host).
type archiveResponse struct {
	OperationID string `json:"operation_id"`
	ID          string `json:"id"`
	Path        string `json:"path"`
	Location    string `json:"location"`
	SizeBytes   int64  `json:"size_bytes"`
}

// rehydrateRequest is the body of POST /v1/sandboxes/{id}/rehydrate. Path
// is the archive locator returned from a prior archive; if empty the
// agent falls back to its configured store. NodeID overrides where the
// rehydrated sandbox lands; otherwise the original node is reused if
// still ACTIVE, falling through to the scheduler.
type rehydrateRequest struct {
	ArchivePath string `json:"archive_path,omitempty"`
	NodeID      string `json:"node_id,omitempty"`
}

// archiveSandbox runs ArchiveSandbox on the agent and flips the DB row to
// ARCHIVED. The agent stops the sandbox first if needed, compresses the
// on-disk state into a tar.zst, optionally uploads to S3, and removes
// the local sandbox dir.
func (h *Handlers) archiveSandbox(w http.ResponseWriter, r *http.Request) {
	accountID, sb, node, ok := h.resolveSandboxAndNode(w, r)
	if !ok {
		return
	}
	if sb.State != models.SandboxStateStopped && sb.State != models.SandboxStateRunning && sb.State != models.SandboxStatePaused {
		writeErr(w, http.StatusConflict, "sandbox state "+string(sb.State)+" not eligible for archive")
		return
	}

	opID, _ := h.Tracker.Start(r.Context(), accountID, sb.ID, models.OperationTypeArchive)

	// FSM: STOPPED → ARCHIVING → ARCHIVED. Walk the row through the
	// archival mid-state best-effort; if the sandbox is RUNNING we let
	// the agent stop it and rely on the final UpdateState to resync.
	if sb.State == models.SandboxStateRunning || sb.State == models.SandboxStatePaused {
		_ = h.Store.Sandboxes().UpdateState(r.Context(), accountID, sb.ID, models.SandboxStateStopping)
		_ = h.Store.Sandboxes().UpdateState(r.Context(), accountID, sb.ID, models.SandboxStateStopped)
	}
	if err := h.Store.Sandboxes().UpdateState(r.Context(), accountID, sb.ID, models.SandboxStateArchiving); err != nil {
		h.log().Warn("archiveSandbox: mid-state", "err", err, "sandbox_id", sb.ID)
	}

	dispatchCtx, cancel := context.WithTimeout(r.Context(), archiveDispatchTimeout)
	defer cancel()
	res, err := h.Pool.ClientFor(node).ArchiveSandbox(dispatchCtx, sb.ID)
	if err != nil {
		_ = h.Store.Sandboxes().UpdateState(r.Context(), accountID, sb.ID, models.SandboxStateError)
		_ = h.Tracker.Complete(r.Context(), opID, err)
		h.log().Error("archiveSandbox: dispatch", "err", err, "sandbox_id", sb.ID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := h.Store.Sandboxes().UpdateState(r.Context(), accountID, sb.ID, models.SandboxStateArchived); err != nil {
		h.log().Error("archiveSandbox: final-state", "err", err)
	}
	_ = h.Tracker.Complete(r.Context(), opID, nil)

	writeJSON(w, http.StatusOK, archiveResponse{
		OperationID: opID,
		ID:          sb.ID,
		Path:        res.Path,
		Location:    res.Location,
		SizeBytes:   res.SizeBytes,
	})
}

// rehydrateSandbox pulls an archived sandbox back to a node and flips the
// row to STOPPED. The caller may specify a target node; otherwise we keep
// the original node assignment if it's still ACTIVE, and fall back to the
// scheduler if it isn't.
func (h *Handlers) rehydrateSandbox(w http.ResponseWriter, r *http.Request) {
	accountID, ok := RequireAccount(w, r)
	if !ok {
		return
	}
	sb, err := h.loadSandbox(r, accountID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "sandbox not found")
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	if sb.State != models.SandboxStateArchived {
		writeErr(w, http.StatusConflict, "sandbox state "+string(sb.State)+" not eligible for rehydrate")
		return
	}
	var body rehydrateRequest
	if r.ContentLength > 0 {
		if err := decodeBody(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	node, clusterID, err := h.pickRehydrateNode(r.Context(), sb, body.NodeID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	opID, _ := h.Tracker.Start(r.Context(), accountID, sb.ID, models.OperationTypeArchive)

	dispatchCtx, cancel := context.WithTimeout(r.Context(), archiveDispatchTimeout)
	defer cancel()
	if err := h.Pool.ClientFor(node).RehydrateSandbox(dispatchCtx, sb.ID, body.ArchivePath); err != nil {
		_ = h.Tracker.Complete(r.Context(), opID, err)
		h.log().Error("rehydrateSandbox: dispatch", "err", err, "sandbox_id", sb.ID)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := h.Store.Sandboxes().UpdatePlacement(r.Context(), sb.ID, clusterID, node.ID); err != nil {
		h.log().Error("rehydrateSandbox: placement", "err", err)
	}
	if err := h.Store.Sandboxes().UpdateState(r.Context(), accountID, sb.ID, models.SandboxStateStopped); err != nil {
		h.log().Error("rehydrateSandbox: final-state", "err", err)
	}
	_ = h.Tracker.Complete(r.Context(), opID, nil)

	out, _ := h.Store.Sandboxes().GetByID(r.Context(), accountID, sb.ID)
	writeJSON(w, http.StatusAccepted, sandboxWithOp{Sandbox: out, OperationID: opID})
}

// pickRehydrateNode returns the node that should host the rehydrated
// sandbox, plus the cluster_id to record on the sandbox row. Preference:
// explicit override → original node (if still ACTIVE) → scheduler-picked.
func (h *Handlers) pickRehydrateNode(ctx context.Context, sb *models.Sandbox, override string) (*models.Node, string, error) {
	if override != "" {
		node, err := h.Store.Nodes().GetByID(ctx, override)
		if err != nil {
			return nil, "", err
		}
		return node, node.ClusterID, nil
	}
	if sb.NodeID != nil && *sb.NodeID != "" {
		node, err := h.Store.Nodes().GetByID(ctx, *sb.NodeID)
		if err == nil && node.State == models.NodeStateActive {
			return node, node.ClusterID, nil
		}
	}
	req := SchedRequest{
		AccountID: sb.AccountID,
		VCPUs:     sb.Config.VCPUs,
		MemoryMB:  sb.Config.MemoryMB,
		DiskGB:    sb.Config.DiskGB,
	}
	cluster, node, err := h.Scheduler.Schedule(ctx, req)
	if err != nil {
		return nil, "", err
	}
	return node, cluster.ID, nil
}
