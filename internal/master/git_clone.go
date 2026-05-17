// Package master — git_clone.go is the post-create git auto-clone hook.
//
// When a sandbox create request carries a git_url, master clones that
// repository into /workspace once the sandbox reaches RUNNING. The hook
// is spawned as a background goroutine and is fully decoupled from the
// create dispatch: it watches the sandbox DB row, so it works the same
// for the synchronous, dispatch-reconcile, and autoscale create paths.
//
// The clone is best-effort. A failure is recorded on the sandbox row
// (git_clone_status=failed, git_clone_error=<reason>) and surfaced in the
// dashboard, but it never flips the sandbox out of RUNNING — the user
// still gets a working box, just an empty /workspace.
package master

import (
	"context"
	"fmt"
	"net/url"
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
	// gitCloneStepTimeout bounds a single network-bound exec step
	// (apt-get install, git clone) — both can be slow on a cold box.
	gitCloneStepTimeout = 5 * time.Minute
	// gitChownTimeout bounds the final chown — local and fast.
	gitChownTimeout = 30 * time.Second
)

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
// the sandbox to reach RUNNING, then clones the requested repository into
// /workspace via the agent exec flow. Spawned as a goroutine from the
// create path; it owns its own context and never returns to a handler.
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

// execGitClone runs the three-step clone inside the sandbox via the agent
// exec flow: install git, clone the repo into /workspace, then hand the
// tree to the unprivileged user. A non-zero exit on any step aborts and
// returns an error naming the step that failed.
func (h *Handlers) execGitClone(ctx context.Context, agent *AgentClient, sandboxID string, git gitCloneSpec) error {
	steps := []struct {
		label   string
		command string
		timeout time.Duration
	}{
		{"install git", "apt-get install -y git", gitCloneStepTimeout},
		{"git clone", gitCloneCommand(git), gitCloneStepTimeout},
		{"chown workspace", "chown -R user:user /workspace", gitChownTimeout},
	}
	for _, step := range steps {
		res, err := agent.ExecCommand(ctx, sandboxID, step.command, step.timeout)
		if err != nil {
			return fmt.Errorf("%s: %w", step.label, err)
		}
		if res.ExitCode != 0 {
			detail := strings.TrimSpace(res.Stderr)
			if detail == "" {
				detail = strings.TrimSpace(res.Stdout)
			}
			return fmt.Errorf("%s: exited %d: %s", step.label, res.ExitCode, detail)
		}
	}
	return nil
}

// gitCloneCommand builds the `git clone` shell command. The branch and
// URL are single-quote escaped because the guest agent runs the command
// via /bin/sh -c.
func gitCloneCommand(git gitCloneSpec) string {
	return fmt.Sprintf("git clone -b %s %s /workspace",
		shellSingleQuote(git.Branch),
		shellSingleQuote(gitCloneURL(git.URL, git.Token)))
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

// shellSingleQuote wraps s in single quotes for safe interpolation into a
// /bin/sh -c command, escaping any embedded single quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
