package models

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidTransition is returned by Sandbox.Transition when the requested
// state change is not permitted by the state machine. Wrap-friendly so
// callers can use errors.Is to distinguish FSM rejection from other errors.
var ErrInvalidTransition = errors.New("invalid sandbox state transition")

// SandboxState is the lifecycle state of a sandbox.
//
// The valid forward chain is:
//
//	PENDING → CREATING → RUNNING → PAUSING → PAUSED → STOPPING → STOPPED →
//	ARCHIVING → ARCHIVED → DESTROYING → DESTROYED
//
// In addition, any non-ERROR state may transition to ERROR. ERROR is fully
// terminal; DESTROYED has only the →ERROR exit per CLAUDE.md.
type SandboxState string

const (
	SandboxStatePending    SandboxState = "PENDING"
	SandboxStateCreating   SandboxState = "CREATING"
	SandboxStateRunning    SandboxState = "RUNNING"
	SandboxStatePausing    SandboxState = "PAUSING"
	SandboxStatePaused     SandboxState = "PAUSED"
	SandboxStateStopping   SandboxState = "STOPPING"
	SandboxStateStopped    SandboxState = "STOPPED"
	SandboxStateArchiving  SandboxState = "ARCHIVING"
	SandboxStateArchived   SandboxState = "ARCHIVED"
	SandboxStateDestroying SandboxState = "DESTROYING"
	SandboxStateDestroyed  SandboxState = "DESTROYED"
	SandboxStateError      SandboxState = "ERROR"
)

// Valid reports whether s is a known SandboxState constant. Use this to
// reject misspelled or lowercased states from external input before they
// reach the FSM.
func (s SandboxState) Valid() bool {
	_, ok := validSandboxTransitions[s]
	return ok
}

// SandboxConfig holds resource limits applied to the underlying microVM.
// Persisted as a JSONB column.
type SandboxConfig struct {
	VCPUs    int `json:"vcpus"`
	MemoryMB int `json:"memory_mb"`
	DiskGB   int `json:"disk_gb"`
}

// Value implements driver.Valuer so SandboxConfig can be written to a JSONB
// column via sqlx/database/sql.
func (c SandboxConfig) Value() (driver.Value, error) {
	return json.Marshal(c)
}

// Scan implements sql.Scanner for reads from a JSONB column.
func (c *SandboxConfig) Scan(src any) error {
	return scanJSON(src, c)
}

// DefaultAutoStopMinutes is the idle window before a RUNNING sandbox is
// automatically stopped by the LifecycleManager. 15 minutes mirrors the
// Daytona / E2B default and keeps unattended sandboxes from leaking cost.
const DefaultAutoStopMinutes = 15

// DefaultAutoArchiveMinutes is the idle window before a STOPPED sandbox is
// archived to cold storage. 24h (1440 min) is generous enough that a
// developer who leaves a sandbox stopped overnight does not lose it.
const DefaultAutoArchiveMinutes = 1440

// Sandbox represents a managed microVM owned by an account and scheduled
// onto a node within a cluster. NodeID and ClusterID are nullable: a
// PENDING sandbox has not yet been scheduled, and both fields may also be
// cleared after archival.
//
// AutoStopMinutes and AutoArchiveMinutes drive the LifecycleManager: a
// RUNNING sandbox idle for AutoStopMinutes is stopped, a STOPPED sandbox
// idle for AutoArchiveMinutes is archived. A value of 0 disables the
// corresponding policy.
type Sandbox struct {
	ID                 string        `db:"id" json:"id"`
	Name               string        `db:"name" json:"name"`
	AccountID          string        `db:"account_id" json:"account_id"`
	NodeID             *string       `db:"node_id" json:"node_id,omitempty"`
	ClusterID          *string       `db:"cluster_id" json:"cluster_id,omitempty"`
	TemplateID         string        `db:"template_id" json:"template_id"`
	State              SandboxState  `db:"state" json:"state"`
	Config             SandboxConfig `db:"config" json:"config"`
	AutoStopMinutes    int           `db:"auto_stop_minutes" json:"auto_stop_minutes"`
	AutoArchiveMinutes int           `db:"auto_archive_minutes" json:"auto_archive_minutes"`
	LastActivity       time.Time     `db:"last_activity" json:"last_activity"`
	CreatedAt          time.Time     `db:"created_at" json:"created_at"`
	UpdatedAt          time.Time     `db:"updated_at" json:"updated_at"`
	// TimeToRunningMS is the wall-clock milliseconds from create accepted
	// to RUNNING, stamped once when the sandbox first reaches RUNNING.
	// Nil until then, and on rows created before the boot-metrics
	// migration.
	TimeToRunningMS *int64 `db:"time_to_running_ms" json:"time_to_running_ms,omitempty"`
	// PoolHit is true when the create was served from a warm pre-warm
	// pool member rather than a cold snapshot restore. Nil until the
	// sandbox reaches RUNNING.
	PoolHit *bool `db:"pool_hit" json:"pool_hit,omitempty"`
	// Git auto-clone fields. GitURL / GitBranch echo the create request
	// when it asked for a repository to be cloned into /workspace.
	// GitCloneStatus tracks the post-create clone hook — one of "",
	// "pending", "cloning", "done", "failed" — and GitCloneError carries
	// the failure reason when it is "failed". The access token used for
	// private repos is never persisted.
	GitURL         string `db:"git_url" json:"git_url,omitempty"`
	GitBranch      string `db:"git_branch" json:"git_branch,omitempty"`
	GitCloneStatus string `db:"git_clone_status" json:"git_clone_status,omitempty"`
	GitCloneError  string `db:"git_clone_error" json:"git_clone_error,omitempty"`
}

// validSandboxTransitions defines the allowed forward transitions. The
// canonical chain in CLAUDE.md (PENDING → CREATING → RUNNING → PAUSING →
// PAUSED → STOPPING → STOPPED → ARCHIVING → ARCHIVED → DESTROYING →
// DESTROYED) is preserved, but a few realistic shortcuts are added so the
// API can serve the lifecycle users actually invoke without forcing them
// through every intermediate state:
//
//   - RUNNING → STOPPING:   stop without pausing first
//   - RUNNING → DESTROYING: delete a running sandbox
//   - STOPPED → RUNNING:    start a stopped sandbox (re-restore on agent)
//   - STOPPED → DESTROYING: delete a stopped sandbox without archival
//   - ARCHIVED → STOPPED:   rehydrate an archived sandbox back to disk
//
// Transitions to ERROR are handled separately in CanTransition.
var validSandboxTransitions = map[SandboxState][]SandboxState{
	SandboxStatePending:    {SandboxStateCreating},
	SandboxStateCreating:   {SandboxStateRunning},
	SandboxStateRunning:    {SandboxStatePausing, SandboxStateStopping, SandboxStateDestroying},
	SandboxStatePausing:    {SandboxStatePaused},
	SandboxStatePaused:     {SandboxStateStopping},
	SandboxStateStopping:   {SandboxStateStopped},
	SandboxStateStopped:    {SandboxStateArchiving, SandboxStateRunning, SandboxStateDestroying},
	SandboxStateArchiving:  {SandboxStateArchived},
	SandboxStateArchived:   {SandboxStateDestroying, SandboxStateStopped},
	SandboxStateDestroying: {SandboxStateDestroyed},
	SandboxStateDestroyed:  nil,
	SandboxStateError:      nil,
}

// CanTransition reports whether a sandbox in state from may move to state to.
// Unknown states (anything not in the constants above) cannot transition
// anywhere — Transition will reject the move and the caller is expected to
// validate state values via SandboxState.Valid() at trust boundaries.
func CanTransition(from, to SandboxState) bool {
	if to == SandboxStateError {
		return from != SandboxStateError
	}
	allowed, ok := validSandboxTransitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// Transition validates and applies a state change, refreshing UpdatedAt on
// success. On rejection it returns an error wrapping ErrInvalidTransition
// and leaves the sandbox unchanged.
func (s *Sandbox) Transition(next SandboxState) error {
	if !CanTransition(s.State, next) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, s.State, next)
	}
	s.State = next
	s.UpdatedAt = time.Now().UTC()
	return nil
}
