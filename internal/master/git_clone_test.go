package master

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

// stubGitCloneToTar swaps the master-side clone for fn for the duration
// of the test, restoring the real implementation on cleanup. The git
// auto-clone tests use this so they never run real git or touch the
// network — defaultGitCloneToTar is exercised directly by
// TestDefaultGitCloneToTar instead.
func stubGitCloneToTar(t *testing.T, fn func(context.Context, gitCloneSpec) (string, func(), error)) {
	t.Helper()
	orig := gitCloneToTar
	gitCloneToTar = fn
	t.Cleanup(func() { gitCloneToTar = orig })
}

// fakeCloneTar returns a gitCloneToTar stub that produces a throwaway
// tarball on disk — enough for execGitClone to open, stat and upload.
func fakeCloneTar(t *testing.T) func(context.Context, gitCloneSpec) (string, func(), error) {
	t.Helper()
	return func(_ context.Context, _ gitCloneSpec) (string, func(), error) {
		dir, err := os.MkdirTemp("", "vajra-fake-clone-")
		if err != nil {
			return "", nil, err
		}
		tarPath := filepath.Join(dir, "repo.tar")
		if err := os.WriteFile(tarPath, []byte("fake-tarball-bytes"), 0o644); err != nil {
			_ = os.RemoveAll(dir)
			return "", nil, err
		}
		return tarPath, func() { _ = os.RemoveAll(dir) }, nil
	}
}

// makeLocalGitRepo creates a throwaway git repository on disk with one
// commit on the given branch and returns a file:// URL for it. Lets
// TestDefaultGitCloneToTar exercise the real master-side clone without
// touching the network.
func makeLocalGitRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=vajra-test", "GIT_AUTHOR_EMAIL=test@vajra.dev",
			"GIT_COMMITTER_NAME=vajra-test", "GIT_COMMITTER_EMAIL=test@vajra.dev")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("-c", "init.defaultBranch="+branch, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("vajra test repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "initial commit")
	return "file://" + dir
}

// gitCloneAgent installs an agent stand-in that accepts the create
// asynchronously, reports the sandbox RUNNING on every GET, and routes
// exec calls to execFn. Each exec command string is appended to *cmds
// under mu so the test can assert on the post-create hook's behaviour.
// File uploads (the tarball transfer) are accepted with 204.
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
			// Covers POST /sandbox/{id}/files/upload — the tarball transfer.
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// TestCreateSandboxWithGitURL verifies a create request carrying a
// git_url is accepted, the git fields are persisted, and the clone is
// marked pending on the returned row.
func TestCreateSandboxWithGitURL(t *testing.T) {
	h := newTestHarness(t)
	stubGitCloneToTar(t, fakeCloneTar(t)) // keep the spawned hook off the network
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

// TestGitCloneInSandbox exercises the post-create hook end to end. The
// repo is vsock-only with no network, so the guest must NOT run apt-get
// or git clone — master clones the repo and the guest only receives the
// tarball plus one local extract command. The hook must wait for
// RUNNING, transfer the repo, and mark git_clone_status=done.
func TestGitCloneInSandbox(t *testing.T) {
	h := newTestHarness(t)
	stubGitCloneToTar(t, fakeCloneTar(t))
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
		"git_branch": "main",
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
	for _, want := range []string{"-xmf", "-C /workspace", "chown -R cloud:cloud /workspace"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("extract command missing %q; got:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "apt-get") || strings.Contains(joined, "git clone") {
		t.Fatalf("guest must not run apt-get / git clone (no network); got:\n%s", joined)
	}
}

// TestGitCloneFailureKeepsSandboxRunning pins the failure contract: a
// clone that fails on master records git_clone_status=failed with the
// reason, redacts the token from the stored error, never touches the
// guest, and never flips the sandbox out of RUNNING.
func TestGitCloneFailureKeepsSandboxRunning(t *testing.T) {
	h := newTestHarness(t)
	// The clone fails on master; the error echoes the token the way git
	// itself would, so the redaction path is exercised.
	stubGitCloneToTar(t, func(_ context.Context, git gitCloneSpec) (string, func(), error) {
		return "", nil, fmt.Errorf(
			"git clone: fatal: could not read from 'https://%s@github.com/octocat/private'", git.Token)
	})
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

	mu.Lock()
	defer mu.Unlock()
	if len(cmds) != 0 {
		t.Fatalf("guest must not be touched when master's clone fails; got %v", cmds)
	}
}

// TestDefaultGitCloneToTar exercises the real master-side clone against a
// throwaway local repository: a good clone yields a tarball holding the
// working tree, and a missing branch fails cleanly.
func TestDefaultGitCloneToTar(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repoURL := makeLocalGitRepo(t, "main")

	tarPath, cleanup, err := defaultGitCloneToTar(context.Background(),
		gitCloneSpec{URL: repoURL, Branch: "main"})
	if err != nil {
		t.Fatalf("clone+tar: %v", err)
	}
	defer cleanup()

	info, err := os.Stat(tarPath)
	if err != nil {
		t.Fatalf("stat tarball: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("tarball is empty")
	}
	// The tarball must hold the cloned working tree.
	dest := t.TempDir()
	if out, err := exec.Command("tar", "-xf", tarPath, "-C", dest).CombinedOutput(); err != nil {
		t.Fatalf("extract: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("cloned tree missing README.md: %v", err)
	}

	// A missing branch fails master's clone cleanly.
	if _, _, err := defaultGitCloneToTar(context.Background(),
		gitCloneSpec{URL: repoURL, Branch: "no-such-branch"}); err == nil {
		t.Fatal("expected error cloning a non-existent branch")
	}
}

// TestGitCloneURL pins the token-into-userinfo behaviour: a token is
// embedded so private repos clone non-interactively, and a blank token
// or unparseable URL is returned unchanged.
func TestGitCloneURL(t *testing.T) {
	cases := []struct {
		name, raw, token, want string
	}{
		{"no token", "https://github.com/octocat/Hello-World", "",
			"https://github.com/octocat/Hello-World"},
		{"with token", "https://github.com/octocat/private", "ghp_secrettoken",
			"https://ghp_secrettoken@github.com/octocat/private"},
		{"unparseable url", "::::not a url", "ghp_secrettoken", "::::not a url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitCloneURL(tc.raw, tc.token); got != tc.want {
				t.Fatalf("gitCloneURL(%q, token) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
