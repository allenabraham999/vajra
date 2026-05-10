package models

import (
	"errors"
	"testing"
	"time"
)

// allSandboxStates is every state declared in sandbox.go. It must be kept in
// sync with the constants — the tests below iterate over it to build an
// exhaustive transition matrix.
var allSandboxStates = []SandboxState{
	SandboxStatePending,
	SandboxStateCreating,
	SandboxStateRunning,
	SandboxStatePausing,
	SandboxStatePaused,
	SandboxStateStopping,
	SandboxStateStopped,
	SandboxStateArchiving,
	SandboxStateArchived,
	SandboxStateDestroying,
	SandboxStateDestroyed,
	SandboxStateError,
}

// validForward mirrors the FSM in sandbox.go: the canonical chain from
// CLAUDE.md plus the realistic shortcuts (RUNNING → STOPPING/DESTROYING,
// STOPPED → RUNNING/DESTROYING). Transitions to ERROR are tested
// separately.
var validForward = map[SandboxState][]SandboxState{
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
}

func validForwardContains(from, to SandboxState) bool {
	for _, t := range validForward[from] {
		if t == to {
			return true
		}
	}
	return false
}

func TestSandboxTransition_AllPairs(t *testing.T) {
	type tc struct {
		name    string
		from    SandboxState
		to      SandboxState
		wantErr bool
	}
	var cases []tc

	// 1. Every valid forward transition succeeds.
	for from, tos := range validForward {
		for _, to := range tos {
			cases = append(cases, tc{
				name: string(from) + "_to_" + string(to) + "_valid",
				from: from, to: to, wantErr: false,
			})
		}
	}

	// 2. Every non-ERROR state can transition to ERROR.
	for _, from := range allSandboxStates {
		if from == SandboxStateError {
			continue
		}
		cases = append(cases, tc{
			name: string(from) + "_to_ERROR_valid",
			from: from, to: SandboxStateError, wantErr: false,
		})
	}

	// 3. Every other from→to pair is invalid (including same-state no-ops,
	//    backwards moves, skips, and any move out of a terminal state).
	for _, from := range allSandboxStates {
		for _, to := range allSandboxStates {
			if to == SandboxStateError && from != SandboxStateError {
				continue // covered above
			}
			if validForwardContains(from, to) {
				continue // covered above
			}
			cases = append(cases, tc{
				name: string(from) + "_to_" + string(to) + "_invalid",
				from: from, to: to, wantErr: true,
			})
		}
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sb := &Sandbox{State: c.from}
			err := sb.Transition(c.to)
			if (err != nil) != c.wantErr {
				t.Fatalf("Transition(%s → %s): err=%v, wantErr=%v", c.from, c.to, err, c.wantErr)
			}
			if err == nil && sb.State != c.to {
				t.Fatalf("Transition(%s → %s): post-state=%s, want %s", c.from, c.to, sb.State, c.to)
			}
		})
	}
}

func TestSandboxTransition_RefreshesUpdatedAt(t *testing.T) {
	sb := &Sandbox{State: SandboxStatePending, UpdatedAt: time.Time{}}
	before := time.Now().UTC()
	if err := sb.Transition(SandboxStateCreating); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sb.UpdatedAt.Before(before) {
		t.Fatalf("UpdatedAt was not refreshed: %v", sb.UpdatedAt)
	}
}

func TestSandboxTransition_InvalidLeavesSandboxUnchanged(t *testing.T) {
	original := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sb := &Sandbox{State: SandboxStateRunning, UpdatedAt: original}
	err := sb.Transition(SandboxStateArchived) // skip is invalid
	if err == nil {
		t.Fatal("expected error for invalid transition")
	}
	if sb.State != SandboxStateRunning {
		t.Fatalf("state changed after invalid transition: %s", sb.State)
	}
	if !sb.UpdatedAt.Equal(original) {
		t.Fatalf("UpdatedAt mutated after invalid transition: %v", sb.UpdatedAt)
	}
}

func TestSandboxTransition_ErrorIsFullyTerminal(t *testing.T) {
	// ERROR is the only fully-terminal state: nothing leaves it, not even
	// →ERROR (no-op). DESTROYED is "almost terminal" — per CLAUDE.md, any
	// state may transition to ERROR — and that case is covered exhaustively
	// by TestSandboxTransition_AllPairs.
	for _, target := range allSandboxStates {
		sb := &Sandbox{State: SandboxStateError}
		if err := sb.Transition(target); err == nil {
			t.Fatalf("expected error transitioning out of ERROR → %s", target)
		}
	}
}

func TestSandboxTransition_WrapsErrInvalidTransition(t *testing.T) {
	sb := &Sandbox{State: SandboxStateRunning}
	err := sb.Transition(SandboxStateArchived) // skip — invalid
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("error is not wrapping ErrInvalidTransition: %v", err)
	}
}

func TestSandboxState_Valid(t *testing.T) {
	for _, s := range allSandboxStates {
		if !s.Valid() {
			t.Errorf("expected %q to be Valid", s)
		}
	}
	for _, bad := range []SandboxState{"", "running", "Pending", "ZOMBIE"} {
		if bad.Valid() {
			t.Errorf("expected %q to be invalid", bad)
		}
	}
}

func TestCanTransition_MatchesTransition(t *testing.T) {
	// CanTransition is the public predicate; it must agree with Transition's
	// success/failure on every pair.
	for _, from := range allSandboxStates {
		for _, to := range allSandboxStates {
			sb := &Sandbox{State: from}
			err := sb.Transition(to)
			got := err == nil
			want := CanTransition(from, to)
			if got != want {
				t.Fatalf("CanTransition(%s → %s)=%v but Transition err=%v", from, to, want, err)
			}
		}
	}
}
