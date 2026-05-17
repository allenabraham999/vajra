// Package master — git_clone.go is the post-create git auto-clone hook.
//
// When a sandbox create request carries a git_url, master clones that
// repository into /workspace once the sandbox reaches RUNNING.
//
// Vajra microVMs are vsock-only and have no outbound network, so the
// clone cannot run inside the guest. Instead master — which does have
// internet — clones the repo into a local temp directory, packs the
// working tree into a tarball, streams the tarball into the sandbox over
// vsock, and runs one in-guest command to extract it into /workspace.
// Every guest-side step (tar, chown) is local, so the missing network
// never matters.
//
// The hook is spawned as a background goroutine and is fully decoupled
// from the create dispatch: it watches the sandbox DB row, so it works
// the same for the synchronous, dispatch-reconcile, and autoscale create
// paths.
//
// The clone is best-effort. A failure is recorded on the sandbox row
// (git_clone_status=failed, git_clone_error=<reason>) and surfaced in the
// dashboard, but it never flips the sandbox out of RUNNING — the user
// still gets a working box, just an empty /workspace. A bad or
// unreachable git_url fails master's own clone cleanly; the reason is
// reported to the sandbox owner and never crashes master.
package master

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

// git_clone_status column values. Empty string means no clone was asked
// for; the others track the hook's progress.
const (
	gitClonePending = "pending" // requested, sandbox not yet RUNNING
	gitCloneCloning = "cloning" // exec'ing the clone steps now
	gitCloneDone    = "done"    // repo cloned into /workspace
	gitCloneFailed  = "failed"  // clone failed; see git_clone_error
)

// Timeouts for the hook. These only bound how long the background
// goroutine lives — a failed clone never blocks the sandbox lifecycle.
const (
	// gitCloneReadyTimeout caps the wait for the sandbox to reach RUNNING.
	// Sized for the autoscale cold path (fresh node + template pull),
	// which can take minutes before the box is up.
	gitCloneReadyTimeout = 6 * time.Minute
	// gitCloneReadyPoll is how often the hook re-reads the sandbox row
	// while waiting for RUNNING.
	gitCloneReadyPoll = 250 * time.Millisecond
	// gitCloneStepTimeout bounds each network/IO-bound step: master's
	// `git clone`, the vsock tarball upload, and the in-guest extract.
	// Generous because a large repo on a cold node is slow.
	gitCloneStepTimeout = 5 * time.Minute
)

// gitCloneTarGuestPath is where master drops the repo tarball inside the
// sandbox before extracting it; /tmp is writable on every template.
const gitCloneTarGuestPath = "/tmp/vajra-git-clone.tar"

// gitCloneSpec carries the optional git auto-clone parameters from a
// create request through to the post-create hook. The token is held only
// in the driving goroutine's memory and is never persisted or logged, so
// private-repo credentials don't land in the database.
type gitCloneSpec struct {
	URL    string
	Branch string
	Token  string
}

// enabled reports whether the create request asked for a git auto-clone.
func (g gitCloneSpec) enabled() bool { return g.URL != "" }

// initialStatus is the git_clone_status to stamp on a freshly created
// sandbox row: "pending" when a clone was requested, "" otherwise.
func (g gitCloneSpec) initialStatus() string {
	if g.enabled() {
		return gitClonePending
	}
	return ""
}

// gitSpec extracts the git auto-clone parameters from a create request,
// defaulting the branch to "main". Returns a disabled spec when the
// request carried no git_url.
func (req *createSandboxRequest) gitSpec() gitCloneSpec {
	if strings.TrimSpace(req.GitURL) == "" {
		return gitCloneSpec{}
	}
	branch := strings.TrimSpace(req.GitBranch)
	if branch == "" {
		branch = "main"
	}
	return gitCloneSpec{
		URL:    strings.TrimSpace(req.GitURL),
		Branch: branch,
		Token:  strings.TrimSpace(req.GitToken),
	}
}

// runGitCloneHook is the post-create git auto-clone hook. It waits for
// the sandbox to reach RUNNING, then transfers the requested repository
// into /workspace. Spawned as a goroutine from the create path; it owns
// its own context and never returns to a handler.
func (h *Handlers) runGitCloneHook(accountID, sandboxID string, git gitCloneSpec) {
	ctx := context.Background()

	sb, ok := h.waitForGitCloneTarget(ctx, accountID, sandboxID)
	if !ok {
		// The sandbox never reached RUNNING (failed create or timeout) —
		// there is nothing to clone into. Leave git_clone_status at
		// "pending"; the sandbox's own ERROR state tells the real story.
		h.log().Warn("git clone hook: sandbox never reached RUNNING",
			"sandbox_id", sandboxID, "git_url", git.URL)
		return
	}
	if sb.NodeID == nil || *sb.NodeID == "" {
		h.recordGitClone(ctx, accountID, sandboxID, gitCloneFailed, "sandbox has no node placement")
		return
	}
	node, err := h.Store.Nodes().GetByID(ctx, *sb.NodeID)
	if err != nil {
		h.log().Error("git clone hook: load node", "err", err, "sandbox_id", sandboxID)
		h.recordGitClone(ctx, accountID, sandboxID, gitCloneFailed, "internal error resolving sandbox node")
		return
	}

	h.recordGitClone(ctx, accountID, sandboxID, gitCloneCloning, "")
	if err := h.execGitClone(ctx, h.Pool.ClientFor(node), sandboxID, git); err != nil {
		msg := redactGitToken(err.Error(), git.Token)
		h.log().Warn("git clone hook failed",
			"sandbox_id", sandboxID, "git_url", git.URL, "err", msg)
		h.recordGitClone(ctx, accountID, sandboxID, gitCloneFailed, msg)
		return
	}
	h.recordGitClone(ctx, accountID, sandboxID, gitCloneDone, "")
	h.log().Info("git clone hook complete", "sandbox_id", sandboxID, "git_url", git.URL)
}

// waitForGitCloneTarget polls the sandbox row until it reaches RUNNING
// (ok=true) or settles into a terminal non-running state / the deadline
// expires (ok=false).
func (h *Handlers) waitForGitCloneTarget(ctx context.Context, accountID, sandboxID string) (*models.Sandbox, bool) {
	deadline := h.now().Add(gitCloneReadyTimeout)
	ticker := time.NewTicker(gitCloneReadyPoll)
	defer ticker.Stop()
	for {
		sb, err := h.Store.Sandboxes().GetByID(ctx, accountID, sandboxID)
		if err == nil {
			switch sb.State {
			case models.SandboxStateRunning:
				return sb, true
			case models.SandboxStatePending, models.SandboxStateCreating:
				// Still booting — keep waiting.
			default:
				// ERROR / STOPPED / DESTROYED / … — it will never be a
				// clone target.
				return nil, false
			}
		}
		if h.now().After(deadline) {
			return nil, false
		}
		<-ticker.C
	}
}

// execGitClone transfers the requested repository into the sandbox's
// /workspace. Because Vajra microVMs have no outbound network, the clone
// runs on master (gitCloneToTar), is streamed into the guest over vsock,
// and unpacked by a single local in-guest command. The master-side temp
// directory is always removed.
func (h *Handlers) execGitClone(ctx context.Context, agent *AgentClient, sandboxID string, git gitCloneSpec) error {
	tarPath, cleanup, err := gitCloneToTar(ctx, git)
	if err != nil {
		return err
	}
	defer cleanup()

	// Stream the tarball into the guest over vsock.
	f, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("open tarball: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat tarball: %w", err)
	}
	uploadCtx, cancel := context.WithTimeout(ctx, gitCloneStepTimeout)
	defer cancel()
	if err := agent.UploadFile(uploadCtx, sandboxID, gitCloneTarGuestPath, 0o644, info.Size(), f); err != nil {
		return fmt.Errorf("upload repo to sandbox: %w", err)
	}

	// Extract into /workspace and hand the tree to the guest user. tar,
	// rm and chown are all local — no guest network needed. Pool VMs are
	// restored from a snapshot, so the guest clock is frozen at snapshot
	// time (hours stale); -m (don't restore archive mtimes) plus
	// --warning=no-timestamp stop tar from failing every file with a
	// "timestamp in the future" warning and exiting non-zero.
	extract := fmt.Sprintf(
		"mkdir -p /workspace && tar --warning=no-timestamp -xmf %[1]s -C /workspace && rm -f %[1]s && chown -R user:user /workspace",
		gitCloneTarGuestPath)
	res, err := agent.ExecCommand(ctx, sandboxID, extract, gitCloneStepTimeout)
	if err != nil {
		return fmt.Errorf("extract repo: %w", err)
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		return fmt.Errorf("extract repo: exited %d: %s", res.ExitCode, detail)
	}
	return nil
}

// gitCloneToTar performs the master-side half of the auto-clone: it
// clones the repository into a temporary directory and packs the working
// tree into a tar archive, returning the archive path and a cleanup func
// the caller must invoke. It is a package var purely so tests can
// substitute a stub — production always uses defaultGitCloneToTar.
var gitCloneToTar = defaultGitCloneToTar

// defaultGitCloneToTar is the real gitCloneToTar. A bad URL or missing
// branch fails the `git clone` here; the error is returned for the hook
// to record on the sandbox row and never crashes master.
func defaultGitCloneToTar(ctx context.Context, git gitCloneSpec) (string, func(), error) {
	workDir, err := os.MkdirTemp("", "vajra-git-clone-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(workDir) }

	// Clone on master. GIT_TERMINAL_PROMPT=0 makes a private repo with no
	// token fail fast instead of blocking on a credential prompt.
	repoDir := filepath.Join(workDir, "repo")
	cloneCtx, cancel := context.WithTimeout(ctx, gitCloneStepTimeout)
	defer cancel()
	clone := exec.CommandContext(cloneCtx, "git", "clone", "--depth", "1",
		"--branch", git.Branch, gitCloneURL(git.URL, git.Token), repoDir)
	clone.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := clone.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// Pack the working tree (the shallow .git included) into a tarball.
	tarPath := filepath.Join(workDir, "repo.tar")
	pack := exec.CommandContext(ctx, "tar", "-cf", tarPath, "-C", repoDir, ".")
	if out, err := pack.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("tar repo: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return tarPath, cleanup, nil
}

// gitCloneURL injects the access token into the URL as userinfo so a
// private repository clones non-interactively. A blank token, or a URL
// that does not parse, returns the input unchanged.
func gitCloneURL(rawURL, token string) string {
	if token == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	u.User = url.User(token)
	return u.String()
}

// redactGitToken strips the access token out of a string before it is
// persisted or logged — git's own error output can echo the clone URL,
// token and all.
func redactGitToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// recordGitClone persists the git clone status (and failure reason, if
// any) on the sandbox row. Best-effort: a failed write is logged but
// never blocks the hook.
func (h *Handlers) recordGitClone(ctx context.Context, accountID, sandboxID, status, errMsg string) {
	if err := h.Store.Sandboxes().UpdateGitClone(ctx, accountID, sandboxID, status, errMsg); err != nil {
		h.log().Warn("UpdateGitClone failed",
			"err", err, "sandbox_id", sandboxID, "status", status)
	}
}
