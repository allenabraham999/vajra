package master

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

// fakeAgents is the in-memory AgentLister used by reconciler tests.
type fakeAgents struct {
	mu              sync.Mutex
	listFns         map[string]func() ([]AgentSandbox, error)
	destroyCalls    []destroyCall
	destroyErr      error
}

type destroyCall struct {
	NodeID    string
	SandboxID string
}

func (f *fakeAgents) ListSandboxes(_ context.Context, node *models.Node) ([]AgentSandbox, error) {
	f.mu.Lock()
	fn := f.listFns[node.ID]
	f.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn()
}

func (f *fakeAgents) DestroySandbox(_ context.Context, node *models.Node, sandboxID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyCalls = append(f.destroyCalls, destroyCall{NodeID: node.ID, SandboxID: sandboxID})
	return f.destroyErr
}

// reconcilerSetup wires a Reconciler with the captured logger so tests
// can assert on log records too.
func reconcilerSetup(t *testing.T, fs *fakeStore, agents *fakeAgents) (*Reconciler, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := NewReconciler(fs, agents, logger, time.Second)
	return r, buf
}

func TestReconcile_Orphan(t *testing.T) {
	fs := newFakeStore()
	node := &models.Node{ID: "n1", State: models.NodeStateActive}
	fs.nodes.all = []*models.Node{node}
	fs.sandboxes.byNode = map[string][]*models.Sandbox{"n1": {}}

	agents := &fakeAgents{
		listFns: map[string]func() ([]AgentSandbox, error){
			"n1": func() ([]AgentSandbox, error) {
				return []AgentSandbox{{ID: "orphan1", State: "RUNNING"}}, nil
			},
		},
	}

	r, logs := reconcilerSetup(t, fs, agents)
	r.tickOnce(context.Background())

	if len(agents.destroyCalls) != 1 {
		t.Fatalf("expected 1 destroy call, got %d", len(agents.destroyCalls))
	}
	if agents.destroyCalls[0].SandboxID != "orphan1" || agents.destroyCalls[0].NodeID != "n1" {
		t.Fatalf("unexpected destroy: %+v", agents.destroyCalls[0])
	}
	if !strings.Contains(logs.String(), `"op":"orphan"`) {
		t.Fatalf("expected orphan log entry; got: %s", logs.String())
	}
}

func TestReconcile_Ghost(t *testing.T) {
	fs := newFakeStore()
	node := &models.Node{ID: "n1", State: models.NodeStateActive}
	fs.nodes.all = []*models.Node{node}
	ghost := mkSandbox("ghost1", "acct", models.SandboxStateRunning, 1, 512)
	fs.sandboxes.byNode = map[string][]*models.Sandbox{"n1": {ghost}}
	fs.sandboxes.byID = map[string]*models.Sandbox{"ghost1": ghost}

	agents := &fakeAgents{
		listFns: map[string]func() ([]AgentSandbox, error){
			"n1": func() ([]AgentSandbox, error) { return nil, nil },
		},
	}

	r, logs := reconcilerSetup(t, fs, agents)
	r.tickOnce(context.Background())

	if len(fs.sandboxes.updateStateCalls) != 1 {
		t.Fatalf("expected 1 UpdateState, got %d", len(fs.sandboxes.updateStateCalls))
	}
	got := fs.sandboxes.updateStateCalls[0]
	if got.AccountID != "acct" || got.ID != "ghost1" || got.State != models.SandboxStateDestroyed {
		t.Fatalf("unexpected UpdateState: %+v", got)
	}
	if !strings.Contains(logs.String(), `"op":"ghost"`) {
		t.Fatalf("expected ghost log entry; got: %s", logs.String())
	}
}

func TestReconcile_StateMismatch_RunningToStopped(t *testing.T) {
	fs := newFakeStore()
	node := &models.Node{ID: "n1", State: models.NodeStateActive}
	fs.nodes.all = []*models.Node{node}
	sb := mkSandbox("s1", "acct", models.SandboxStateRunning, 1, 512)
	fs.sandboxes.byNode = map[string][]*models.Sandbox{"n1": {sb}}
	fs.sandboxes.byID = map[string]*models.Sandbox{"s1": sb}

	agents := &fakeAgents{
		listFns: map[string]func() ([]AgentSandbox, error){
			"n1": func() ([]AgentSandbox, error) {
				return []AgentSandbox{{ID: "s1", State: "STOPPED"}}, nil
			},
		},
	}

	r, logs := reconcilerSetup(t, fs, agents)
	r.tickOnce(context.Background())

	if len(fs.sandboxes.updateStateCalls) != 1 {
		t.Fatalf("expected 1 UpdateState, got %d", len(fs.sandboxes.updateStateCalls))
	}
	got := fs.sandboxes.updateStateCalls[0]
	if got.State != models.SandboxStateStopped {
		t.Fatalf("expected STOPPED, got %s", got.State)
	}
	if !strings.Contains(logs.String(), `"op":"state_mismatch"`) {
		t.Fatalf("expected state_mismatch log; got: %s", logs.String())
	}
}

func TestReconcile_StateMismatch_ErrorWithLiveAgentEntryGetsDestroyed(t *testing.T) {
	fs := newFakeStore()
	node := &models.Node{ID: "n1", State: models.NodeStateActive}
	fs.nodes.all = []*models.Node{node}
	// DB has already concluded the sandbox failed (e.g. create poller hit
	// timeout, or a prior reconcile noticed a crash). Agent still has it
	// listed as RUNNING and is reporting capacity for it. Reconciler must
	// nudge the agent to drop the stale entry so usage frees up.
	sb := mkSandbox("s1", "acct", models.SandboxStateError, 1, 512)
	fs.sandboxes.byNode = map[string][]*models.Sandbox{"n1": {sb}}
	fs.sandboxes.byID = map[string]*models.Sandbox{"s1": sb}

	agents := &fakeAgents{
		listFns: map[string]func() ([]AgentSandbox, error){
			"n1": func() ([]AgentSandbox, error) {
				return []AgentSandbox{{ID: "s1", State: "RUNNING"}}, nil
			},
		},
	}

	r, _ := reconcilerSetup(t, fs, agents)
	r.tickOnce(context.Background())

	if len(agents.destroyCalls) != 1 {
		t.Fatalf("expected 1 DestroySandbox call, got %d", len(agents.destroyCalls))
	}
	got := agents.destroyCalls[0]
	if got.NodeID != "n1" || got.SandboxID != "s1" {
		t.Fatalf("unexpected destroy target: %+v", got)
	}
	if len(fs.sandboxes.updateStateCalls) != 0 {
		t.Fatalf("expected no UpdateState — DB row already terminal; got %d", len(fs.sandboxes.updateStateCalls))
	}
}

func TestReconcile_StateMismatch_NotInScopeIsIgnored(t *testing.T) {
	fs := newFakeStore()
	node := &models.Node{ID: "n1", State: models.NodeStateActive}
	fs.nodes.all = []*models.Node{node}
	// DB says Stopped, agent says Running — we don't auto-transition this
	// pair (agent could lie or be transitional), so no UpdateState.
	sb := mkSandbox("s1", "acct", models.SandboxStateStopped, 1, 512)
	fs.sandboxes.byNode = map[string][]*models.Sandbox{"n1": {sb}}
	fs.sandboxes.byID = map[string]*models.Sandbox{"s1": sb}

	agents := &fakeAgents{
		listFns: map[string]func() ([]AgentSandbox, error){
			"n1": func() ([]AgentSandbox, error) {
				return []AgentSandbox{{ID: "s1", State: "RUNNING"}}, nil
			},
		},
	}

	r, _ := reconcilerSetup(t, fs, agents)
	r.tickOnce(context.Background())

	if len(fs.sandboxes.updateStateCalls) != 0 {
		t.Fatalf("expected no UpdateState, got %d", len(fs.sandboxes.updateStateCalls))
	}
}

func TestReconcile_OneNodeListErrorDoesNotAbortPass(t *testing.T) {
	fs := newFakeStore()
	n1 := &models.Node{ID: "n1", State: models.NodeStateActive}
	n2 := &models.Node{ID: "n2", State: models.NodeStateActive}
	fs.nodes.all = []*models.Node{n1, n2}

	// n2 has a ghost sandbox we expect to be cleaned up even though n1 errored.
	ghost := mkSandbox("ghost-n2", "acct", models.SandboxStateRunning, 1, 512)
	fs.sandboxes.byNode = map[string][]*models.Sandbox{
		"n2": {ghost},
	}
	fs.sandboxes.byID = map[string]*models.Sandbox{"ghost-n2": ghost}

	agents := &fakeAgents{
		listFns: map[string]func() ([]AgentSandbox, error){
			"n1": func() ([]AgentSandbox, error) { return nil, errors.New("boom") },
			"n2": func() ([]AgentSandbox, error) { return nil, nil },
		},
	}

	r, logs := reconcilerSetup(t, fs, agents)
	r.tickOnce(context.Background())

	// Despite n1's failure, n2's ghost was reconciled.
	if len(fs.sandboxes.updateStateCalls) != 1 {
		t.Fatalf("expected n2 to still reconcile (1 UpdateState), got %d", len(fs.sandboxes.updateStateCalls))
	}
	if !strings.Contains(logs.String(), "node pass failed") {
		t.Fatalf("expected logged failure for n1; got: %s", logs.String())
	}
}

func TestReconcile_SkipsNonActiveNodes(t *testing.T) {
	fs := newFakeStore()
	fs.nodes.all = []*models.Node{
		{ID: "n1", State: models.NodeStateDraining},
		{ID: "n2", State: models.NodeStateOffline},
	}
	listed := false
	agents := &fakeAgents{
		listFns: map[string]func() ([]AgentSandbox, error){
			"n1": func() ([]AgentSandbox, error) { listed = true; return nil, nil },
			"n2": func() ([]AgentSandbox, error) { listed = true; return nil, nil },
		},
	}
	r, _ := reconcilerSetup(t, fs, agents)
	r.tickOnce(context.Background())
	if listed {
		t.Fatalf("expected non-active nodes to be skipped, but ListSandboxes was called")
	}
}

func TestReconcile_GhostAlreadyDestroyedSkipped(t *testing.T) {
	fs := newFakeStore()
	node := &models.Node{ID: "n1", State: models.NodeStateActive}
	fs.nodes.all = []*models.Node{node}
	dead := mkSandbox("dead", "acct", models.SandboxStateDestroyed, 1, 512)
	fs.sandboxes.byNode = map[string][]*models.Sandbox{"n1": {dead}}
	fs.sandboxes.byID = map[string]*models.Sandbox{"dead": dead}

	agents := &fakeAgents{
		listFns: map[string]func() ([]AgentSandbox, error){
			"n1": func() ([]AgentSandbox, error) { return nil, nil },
		},
	}
	r, _ := reconcilerSetup(t, fs, agents)
	r.tickOnce(context.Background())
	if len(fs.sandboxes.updateStateCalls) != 0 {
		t.Fatalf("expected no UpdateState for already-destroyed sandbox; got %+v", fs.sandboxes.updateStateCalls)
	}
}
