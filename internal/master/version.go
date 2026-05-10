// Package master — version.go centralizes build provenance. Variables
// here are designed to be overridden at link time via -ldflags so a
// single binary can self-report its commit, version tag, and build
// timestamp without an external file.
//
// Example: go build -ldflags="-X github.com/allenabraham999/vajra/internal/master.Version=v0.5.0 \
//   -X github.com/allenabraham999/vajra/internal/master.Commit=$(git rev-parse --short HEAD) \
//   -X github.com/allenabraham999/vajra/internal/master.BuiltAt=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
//   ./cmd/vajra-master
package master

import (
	"log/slog"
	"sync"
)

// Version, Commit, and BuiltAt are -ldflags-overridable build metadata.
// Defaults match a "developer build" so a binary built without ldflags
// still reports something useful at GET /version.
var (
	Version = "dev"
	Commit  = "unknown"
	BuiltAt = "unknown"
)

// BuildInfo bundles the version triple. Ready to be served as JSON or
// stamped onto a Handlers value.
func BuildInfo() VersionInfo {
	return VersionInfo{Version: Version, Commit: Commit, BuiltAt: BuiltAt}
}

// agentVersionMismatchOnce ensures we log the warning for a given agent
// at most once per master process; otherwise a 5s heartbeat interval
// would flood the logs with the same line indefinitely.
var (
	agentVersionMismatchMu  sync.Mutex
	agentVersionMismatchSet = map[string]struct{}{}
)

// LogAgentVersionMismatch emits a single warning per node when the agent's
// reported version drifts from the master's. The set is in-memory so a
// master restart resets it; that's fine for an observability signal.
func LogAgentVersionMismatch(logger *slog.Logger, nodeID, agentVersion string) {
	if logger == nil {
		logger = slog.Default()
	}
	if agentVersion == "" || agentVersion == Version {
		return
	}
	key := nodeID + "|" + agentVersion
	agentVersionMismatchMu.Lock()
	if _, seen := agentVersionMismatchSet[key]; seen {
		agentVersionMismatchMu.Unlock()
		return
	}
	agentVersionMismatchSet[key] = struct{}{}
	agentVersionMismatchMu.Unlock()
	logger.Warn("agent version mismatch",
		"node_id", nodeID,
		"agent_version", agentVersion,
		"master_version", Version,
	)
}
