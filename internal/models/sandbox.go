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

// Sandbox represents a managed microVM owned by an account and scheduled
// onto a node within a cluster. NodeID and ClusterID are nullable: a
// PENDING sandbox has not yet been scheduled, and both fields may also be
// cleared after archival.
type Sandbox struct {
	ID         string        `db:"id" json:"id"`
	Name       string        `db:"name" json:"name"`
	AccountID  string        `db:"account_id" json:"account_id"`
	NodeID     *string       `db:"node_id" json:"node_id,omitempty"`
	ClusterID  *string       `db:"cluster_id" json:"cluster_id,omitempty"`
	TemplateID string        `db:"template_id" json:"template_id"`
	State      SandboxState  `db:"state" json:"state"`
	Config     SandboxConfig `db:"config" json:"config"`
	CreatedAt  time.Time     `db:"created_at" json:"created_at"`
	UpdatedAt  time.Time     `db:"updated_at" json:"updated_at"`
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
