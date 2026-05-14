// Package master — builder.go implements the asynchronous
// "Dockerfile → Template" pipeline driven by POST /v1/templates/build.
//
// The HTTP handler persists a Build row in PENDING and returns 202 with
// the build_id. A background goroutine then runs the pipeline:
//
//	PENDING → BUILDING (append guest-agent install lines → docker build →
//	  docker export → ext4 rootfs → boot VM → guest agent ready → snapshot →
//	  hash → cache → register template) → COMPLETED  | FAILED
//
// The actual Docker/Cloud-Hypervisor work is delegated to a BuildRunner
// interface so tests can substitute a deterministic in-memory runner.
// Production wires a Docker-backed runner that lives on the agent host;
// at the master level, we only orchestrate state transitions and
// persistence.
package master

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// guestAgentInstallSnippet is appended to every user-supplied Dockerfile
// so the resulting image already ships the vajra guest agent. The agent
// is the in-VM HTTP target reached over vsock; without it, the master
// cannot exec or upload to a sandbox built from this image.
const guestAgentInstallSnippet = `
# --- vajra guest-agent (appended automatically) ---
RUN set -eux; \
    if command -v apt-get >/dev/null 2>&1; then \
        apt-get update && apt-get install -y --no-install-recommends ca-certificates curl; \
    elif command -v apk >/dev/null 2>&1; then \
        apk add --no-cache ca-certificates curl; \
    fi; \
    install -d /usr/local/bin /etc/vajra; \
    curl -fsSL https://dist.vajra.dev/agent/latest/vajra-guest-agent -o /usr/local/bin/vajra-guest-agent; \
    chmod 0755 /usr/local/bin/vajra-guest-agent
`

// BuildArtifact is what a BuildRunner returns on success. Paths are
// agent-side absolute paths; Hash is the SHA256 of the rootfs.
type BuildArtifact struct {
	Hash         string
	RootfsPath   string
	KernelPath   string
	SnapshotPath string
}

// BuildRunner runs the heavy "docker build → ext4 → snapshot" pipeline.
// Splitting it out lets the handler tests substitute a fast deterministic
// runner without touching real Docker.
type BuildRunner interface {
	Run(ctx context.Context, dockerfile, name, version string) (*BuildArtifact, error)
}

// HashBuildRunner is the default test/stub runner. It does not actually
// build anything — it just hashes the dockerfile, fabricates plausible
// agent-side paths, and returns immediately. Production deployments
// should replace this with a runner that drives the agent's docker
// build endpoint.
type HashBuildRunner struct {
	CacheRoot string
}

// NewHashBuildRunner returns a HashBuildRunner with the canonical cache
// root used in agent docs.
func NewHashBuildRunner() *HashBuildRunner {
	return &HashBuildRunner{CacheRoot: "/var/lib/vajra/cache"}
}

// Run computes a deterministic SHA256 over the dockerfile + name +
// version triple and returns the fabricated artifact paths the agent
// would produce. ctx is currently unused but reserved for cancellation
// once a real runner is wired in.
func (r *HashBuildRunner) Run(_ context.Context, dockerfile, name, version string) (*BuildArtifact, error) {
	h := sha256.New()
	h.Write([]byte(name + "@" + version + "\n"))
	h.Write([]byte(dockerfile))
	sum := hex.EncodeToString(h.Sum(nil))
	root := filepath.Join(r.CacheRoot, sum)
	return &BuildArtifact{
		Hash:         sum,
		RootfsPath:   filepath.Join(root, "rootfs.raw"),
		KernelPath:   filepath.Join(root, "vmlinux"),
		SnapshotPath: filepath.Join(root, "snapshot"),
	}, nil
}

// BuildManager owns the async Build queue. Build rows live in the
// store; the manager spawns goroutines to drive each row to its
// terminal state. The struct is safe for concurrent use.
type BuildManager struct {
	store  store.Store
	runner BuildRunner
	now    func() time.Time
	logger logger

	// inflight is for graceful shutdown — Stop waits for every running
	// build to finish. Not exposed to callers.
	inflight sync.WaitGroup
}

// logger is the narrow subset of slog the build manager actually uses.
// Keeping it unexported avoids a hard dep on slog at this layer.
type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// NewBuildManager wires a Manager. runner may be nil — a HashBuildRunner
// is substituted automatically so the surface is always usable in tests.
func NewBuildManager(st store.Store, runner BuildRunner, lg logger) *BuildManager {
	if runner == nil {
		runner = NewHashBuildRunner()
	}
	return &BuildManager{
		store:  st,
		runner: runner,
		now:    time.Now,
		logger: lg,
	}
}

// Enqueue persists a fresh Build in PENDING and returns the ID. The
// caller usually fires Start in the same handler so the user can poll
// immediately.
func (m *BuildManager) Enqueue(ctx context.Context, accountID, name, version, dockerfile string) (*models.Build, error) {
	id, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	if !validIdentifier(name) {
		return nil, fmt.Errorf("invalid template name %q", name)
	}
	if version == "" {
		return nil, fmt.Errorf("version is required")
	}
	if strings.TrimSpace(dockerfile) == "" {
		return nil, fmt.Errorf("dockerfile is empty")
	}
	b := &models.Build{
		ID: id, AccountID: accountID,
		TemplateName: name, TemplateVer: version,
		Status:     models.BuildStatusPending,
		Dockerfile: dockerfile,
		CreatedAt:  m.now().UTC(),
	}
	if err := m.store.Builds().Create(ctx, b); err != nil {
		return nil, fmt.Errorf("persist build: %w", err)
	}
	return b, nil
}

// validIdentifier accepts the conservative slug used for template names:
// lowercase letters, digits, hyphen, underscore, dot.
func validIdentifier(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

// Start spawns a goroutine that drives the given build to a terminal
// state. The goroutine is tracked so Stop can wait for completion.
func (m *BuildManager) Start(b *models.Build) {
	m.inflight.Add(1)
	go func() {
		defer m.inflight.Done()
		// New, decoupled context so the build survives the HTTP request
		// that kicked it off; capped so a stuck builder doesn't run
		// forever. 10min is generous for image-build workloads.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		m.run(ctx, b)
	}()
}

// RunSync drives the build to completion inline. Exposed for tests that
// want deterministic ordering (no goroutine, no inflight tracking).
func (m *BuildManager) RunSync(ctx context.Context, b *models.Build) {
	m.run(ctx, b)
}

// run is the actual pipeline. It mutates the store row but not the
// passed *Build (callers re-fetch through Get to see fresh state).
func (m *BuildManager) run(ctx context.Context, b *models.Build) {
	if err := m.store.Builds().UpdateStatus(ctx, b.ID, models.BuildStatusBuilding, nil, nil, nil); err != nil {
		m.log("update build to BUILDING failed", "err", err, "build_id", b.ID)
		return
	}
	finalDockerfile := strings.TrimRight(b.Dockerfile, "\n") + "\n" + guestAgentInstallSnippet

	artifact, err := m.runner.Run(ctx, finalDockerfile, b.TemplateName, b.TemplateVer)
	if err != nil {
		m.fail(ctx, b.ID, fmt.Errorf("build: %w", err))
		return
	}

	tmplID, err := randomHex(16)
	if err != nil {
		m.fail(ctx, b.ID, fmt.Errorf("alloc template id: %w", err))
		return
	}
	tmpl := &models.Template{
		ID: tmplID, AccountID: b.AccountID,
		Name: b.TemplateName, Version: b.TemplateVer,
		Hash:         artifact.Hash,
		RootfsPath:   artifact.RootfsPath,
		KernelPath:   artifact.KernelPath,
		SnapshotPath: artifact.SnapshotPath,
		CreatedAt:    m.now().UTC(),
	}
	if err := m.store.Templates().Create(ctx, tmpl); err != nil {
		m.fail(ctx, b.ID, fmt.Errorf("register template: %w", err))
		return
	}
	completed := m.now().UTC()
	if err := m.store.Builds().UpdateStatus(ctx, b.ID, models.BuildStatusCompleted, &tmpl.ID, nil, &completed); err != nil {
		m.log("update build to COMPLETED failed", "err", err, "build_id", b.ID)
	}
	m.log("build completed", "build_id", b.ID, "template_id", tmpl.ID, "hash", tmpl.Hash[:12])
}

// fail stamps a failed status and best-effort logs.
func (m *BuildManager) fail(ctx context.Context, id string, err error) {
	msg := err.Error()
	completed := m.now().UTC()
	if uerr := m.store.Builds().UpdateStatus(ctx, id, models.BuildStatusFailed, nil, &msg, &completed); uerr != nil {
		m.log("update build to FAILED failed", "err", uerr, "build_id", id)
	}
	m.log("build failed", "build_id", id, "err", err)
}

// Wait blocks until every in-flight build finishes. Useful for tests and
// for graceful shutdown in main().
func (m *BuildManager) Wait() { m.inflight.Wait() }

// log dispatches to the configured logger when present; otherwise drops
// the message. Keeping it tolerant of nil avoids forcing test wiring.
func (m *BuildManager) log(msg string, args ...any) {
	if m.logger == nil {
		return
	}
	if strings.Contains(msg, "failed") || strings.Contains(msg, "fail") {
		m.logger.Error(msg, args...)
		return
	}
	m.logger.Info(msg, args...)
}
