package master

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

// waitForGitCloneStatus polls the in-memory store until the sandbox row's
// git_clone_status reaches want or the deadline expires.
func waitForGitCloneStatus(t *testing.T, h *testHarness, accountID, id, want string, timeout time.Duration) *models.Sandbox {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *models.Sandbox
	for time.Now().Before(deadline) {
		sb, err := h.store.Sandboxes().GetByID(context.Background(), accountID, id)
		if err == nil {
			last = sb
			if sb.GitCloneStatus == want {
				return sb
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("sandbox %s git_clone_status did not reach %q within %s; last=%+v", id, want, timeout, last)
	return nil
}

// gitCloneAgent installs an agent stand-in that accepts the create
// asynchronously, reports the sandbox RUNNING on every GET, and routes
// exec calls to execFn. Each exec command string is appended to *cmds
// under mu so the test can assert on the post-create hook's behaviour.
func (h *testHarness) gitCloneAgent(mu *sync.Mutex, cmds *[]string, execFn func() (int, string, string)) {
	h.agentHandler = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox/create":
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sb", "state": "CREATING"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sandbox/"):
			id := strings.TrimPrefix(r.URL.Path, "/sandbox/")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "state": "RUNNING"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			var body struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(h.agentBody, &body)
			mu.Lock()
			*cmds = append(*cmds, body.Command)
			mu.Unlock()
			code, stdout, stderr := execFn()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"exit_code": code, "stdout": stdout, "stderr": stderr,
			})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// TestCreateSandboxWithGitURL verifies a create request carrying a
// git_url is accepted, the git fields are persisted, and the clone is
// marked pending on the returned row.
func TestCreateSandboxWithGitURL(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	resp, body := h.req(t, "POST", "/v1/sandboxes", key, map[string]any{
		"name": "demo", "source": "image", "template_id": "tmpl-1",
		"vcpus": 2, "memory_mb": 1024, "disk_gb": 10,
		"git_url": "https://github.com/octocat/Hello-World", "git_branch": "master",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d body %s", resp.StatusCode, body)
	}
	var got sandboxWithOp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if got.Sandbox == nil {
		t.Fatalf("no sandbox in response: %s", body)
	}
	if got.GitURL != "https://github.com/octocat/Hello-World" {
		t.Fatalf("git_url not persisted: %q", got.GitURL)
	}
	if got.GitBranch != "master" {
		t.Fatalf("git_branch = %q, want master", got.GitBranch)
	}
	if got.GitCloneStatus != gitClonePending {
		t.Fatalf("git_clone_status = %q, want %q", got.GitCloneStatus, gitClonePending)
	}
}

// TestCreateSandboxBadGitURL rejects a malformed git_url at the request
// boundary with 400 — a bad URL is a client error, not a clone failure.
func TestCreateSandboxBadGitURL(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	resp, body := h.req(t, "POST", "/v1/sandboxes", key, map[string]any{
		"name": "demo", "source": "image", "template_id": "tmpl-1",
		"vcpus": 2, "memory_mb": 1024, "disk_gb": 10,
		"git_url": "not-a-real-url",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for bad git_url, got %d body %s", resp.StatusCode, body)
	}
}

// TestGitCloneInSandbox exercises the post-create hook end to end: with
// the exec flow mocked, the hook must wait for RUNNING, run the three
// clone steps (install git → clone → chown), embed the token in the
// clone URL, and mark git_clone_status=done.
func TestGitCloneInSandbox(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	var mu sync.Mutex
	var cmds []string
	h.gitCloneAgent(&mu, &cmds, func() (int, string, string) { return 0, "", "" })

	resp, body := h.req(t, "POST", "/v1/sandboxes", key, map[string]any{
		"name": "demo", "source": "image", "template_id": "tmpl-1",
		"vcpus": 2, "memory_mb": 1024, "disk_gb": 10,
		"git_url":    "https://github.com/octocat/Hello-World",
		"git_branch": "main", "git_token": "ghp_secrettoken",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d body %s", resp.StatusCode, body)
	}
	var got sandboxWithOp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	sb := waitForGitCloneStatus(t, h, accountID, got.ID, gitCloneDone, 5*time.Second)
	if sb.GitCloneError != "" {
		t.Fatalf("git_clone_error should be empty on success, got %q", sb.GitCloneError)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(cmds, "\n")
	for _, want := range []string{
		"apt-get install -y git",
		"git clone -b 'main'",
		"chown -R user:user /workspace",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("exec commands missing %q; got:\n%s", want, joined)
		}
	}
	// The token must be embedded in the clone URL so private repos clone
	// non-interactively.
	if !strings.Contains(joined, "ghp_secrettoken@github.com") {
		t.Fatalf("clone command should embed the token in the URL; got:\n%s", joined)
	}
}

// TestGitCloneFailureKeepsSandboxRunning pins the failure contract: a
// clone that exits non-zero records git_clone_status=failed with the
// reason, redacts the token from the stored error, and never flips the
// sandbox out of RUNNING.
func TestGitCloneFailureKeepsSandboxRunning(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")
	c := h.seedCluster(t)
	h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)

	var mu sync.Mutex
	var cmds []string
	// Every exec fails; stderr echoes the token the way git itself would.
	h.gitCloneAgent(&mu, &cmds, func() (int, string, string) {
		return 128, "", "fatal: could not read from 'https://ghp_secrettoken@github.com'"
	})

	resp, body := h.req(t, "POST", "/v1/sandboxes", key, map[string]any{
		"name": "demo", "source": "image", "template_id": "tmpl-1",
		"vcpus": 2, "memory_mb": 1024, "disk_gb": 10,
		"git_url":   "https://github.com/octocat/private",
		"git_token": "ghp_secrettoken",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d body %s", resp.StatusCode, body)
	}
	var got sandboxWithOp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	sb := waitForGitCloneStatus(t, h, accountID, got.ID, gitCloneFailed, 5*time.Second)
	if sb.State != models.SandboxStateRunning {
		t.Fatalf("sandbox should stay RUNNING after a failed clone, got %s", sb.State)
	}
	if sb.GitCloneError == "" {
		t.Fatalf("git_clone_error should carry the failure reason")
	}
	if strings.Contains(sb.GitCloneError, "ghp_secrettoken") {
		t.Fatalf("git_clone_error must not leak the token: %q", sb.GitCloneError)
	}
	if !strings.Contains(sb.GitCloneError, "***") {
		t.Fatalf("git_clone_error should show the redaction marker: %q", sb.GitCloneError)
	}
}
