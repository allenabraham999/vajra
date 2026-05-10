package cache

import (
	"fmt"
	"time"
)

// Pre-baked key formatters and TTLs so the rest of the codebase doesn't
// hand-roll keys that drift in shape over time. Centralising here keeps
// every consumer aligned and makes the namespace easy to audit.

// SandboxStateTTL is short — sandbox state changes drive the user's
// view of the world, so a stale read shouldn't survive long.
const SandboxStateTTL = 30 * time.Second

// NodeResourcesTTL is short because the scheduler reads it on the hot
// path. Heartbeats refresh it every 5s, so a 10s TTL guarantees the
// scheduler never sees state more than two beats stale.
const NodeResourcesTTL = 10 * time.Second

// AccountSandboxCountTTL is 60s — the count is incremented/decremented
// on every create/destroy, so the only drift comes from missed writes
// (e.g. a master crash mid-create). 60s is short enough that the next
// quota check after a crash repopulates from the database.
const AccountSandboxCountTTL = 60 * time.Second

// TemplateTTL is generous — template rows are append-mostly. Five
// minutes is enough to amortise the read cost without making cache
// invalidation a problem.
const TemplateTTL = 5 * time.Minute

// SandboxStateKey returns the cache key holding a sandbox's state.
func SandboxStateKey(sandboxID string) string {
	return fmt.Sprintf("sandbox:%s:state", sandboxID)
}

// NodeResourcesKey returns the cache key holding a node's used/total
// CPU/memory/disk JSON blob.
func NodeResourcesKey(nodeID string) string {
	return fmt.Sprintf("node:%s:resources", nodeID)
}

// AccountSandboxCountKey returns the cache key holding the account's
// active-sandbox count for fast quota checks.
func AccountSandboxCountKey(accountID string) string {
	return fmt.Sprintf("account:%s:sandbox_count", accountID)
}

// TemplateKey returns the cache key holding a template's JSON metadata.
func TemplateKey(templateID string) string {
	return fmt.Sprintf("template:%s", templateID)
}
