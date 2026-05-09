package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultSandboxRoot is the per-host directory holding sandbox-specific
// state (rootfs overlays, saved snapshots).
const DefaultSandboxRoot = "/var/lib/vajra/sandboxes"

// FirstUserCID is the first vsock CID we hand out. Linux reserves 0-2
// (HYPERVISOR, LOCAL, HOST), so we start at 3.
const FirstUserCID uint32 = 3

// GuestExecPort is the vsock port the guest agent listens on for the
// JSON-line exec protocol. Kept fixed across all sandboxes so the host
// doesn't need to negotiate.
const GuestExecPort uint32 = 5252

// SandboxState mirrors the agent's local view of a microVM. It is a
// strict subset of models.SandboxState — the agent does not model
// PENDING/CREATING/etc, since those exist only at the master layer.
type SandboxState string

const (
	SandboxStateCreating  SandboxState = "CREATING"
	SandboxStateRunning   SandboxState = "RUNNING"
	SandboxStatePaused    SandboxState = "PAUSED"
	SandboxStateStopped   SandboxState = "STOPPED"
	SandboxStateDestroyed SandboxState = "DESTROYED"
	SandboxStateError     SandboxState = "ERROR"
)

// SandboxConfig is the resource shape requested for a sandbox.
type SandboxConfig struct {
	VCPUs    int   `json:"vcpus"`
	MemoryMB int   `json:"memory_mb"`
	DiskGB   int   `json:"disk_gb"`
}

// Sandbox is the agent's record for a single microVM. Mutable fields are
// guarded by SandboxManager.mu; callers receive a deep copy via
// SandboxManager.Get/List.
//
// VsockSocketPath is the absolute path of the host-side vsock socket that
// CH bind()s for this sandbox — the same path written into the rewritten
// snapshot config.json by prepareSandboxSnapshot. It must be set from the
// rewritten path, never derived from the API socket: under the per-sandbox
// snapshot scheme the two are unrelated, and re-deriving would point health
// probes at a path that no process is listening on.
type Sandbox struct {
	ID              string        `json:"id"`
	State           SandboxState  `json:"state"`
	TemplateHash    string        `json:"template_hash"`
	VsockCID        uint32        `json:"vsock_cid"`
	APISocket       string        `json:"api_socket"`
	VsockSocketPath string        `json:"vsock_socket"`
	RootfsPath      string        `json:"rootfs_path"`
	StateDir        string        `json:"state_dir"`
	Config          SandboxConfig `json:"config"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	Healthy         bool          `json:"healthy"`
	LastHealthAt    time.Time     `json:"last_health_at"`
	FromPool        bool          `json:"from_pool"`
	// Error is set when the async create goroutine fails. GetSandbox
	// returns this verbatim so the master poller can surface a useful
	// message to operators.
	Error string `json:"error,omitempty"`
}

// CreateRequest captures what callers (master, pool) supply to
// SandboxManager.CreateSandbox. ID is optional; if empty the manager
// generates a random one.
type CreateRequest struct {
	ID           string
	TemplateHash string
	Config       SandboxConfig
}

// ExecResult is the outcome of a guest-side command invocation.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// VsockDialer abstracts the host→guest vsock connection so tests can swap
// in a Unix-socket fake. The default implementation talks to Cloud
// Hypervisor's hybrid vsock socket via the textual CONNECT protocol.
type VsockDialer interface {
	Dial(ctx context.Context, hostSocket string, port uint32) (io.ReadWriteCloser, error)
}

// VMM is the subset of vmm.VMManager used by SandboxManager. Pulling it
// behind an interface lets unit tests stub out cloud-hypervisor entirely.
type VMM interface {
	RestoreVM(ctx context.Context, vmID, snapshotPath string) (string, error)
	SnapshotVM(ctx context.Context, socketPath, destDir string, resume bool) error
	DestroyVM(ctx context.Context, socketPath string) error
}

// SandboxManager owns the lifecycle of every sandbox running on this host.
// It is safe for concurrent use; the lock is held only during map
// mutations and quick state transitions, never across VMM RPCs.
type SandboxManager struct {
	root     string
	cache    *ImageCache
	vmm      VMM
	dialer   VsockDialer
	logger   *slog.Logger
	socketDir string

	nextCID atomic.Uint32

	mu        sync.RWMutex
	sandboxes map[string]*Sandbox
}

// NewSandboxManager constructs a manager. root is the per-host directory
// for sandbox state (overlays, saved snapshots); socketDir mirrors what
// the underlying VMManager uses so vsock paths resolve consistently.
// Pass nil for logger to use slog.Default and nil for dialer to use the
// hybrid-vsock CONNECT dialer.
func NewSandboxManager(
	root, socketDir string,
	cache *ImageCache,
	vm VMM,
	dialer VsockDialer,
	logger *slog.Logger,
) *SandboxManager {
	if logger == nil {
		logger = slog.Default()
	}
	if dialer == nil {
		dialer = &hybridVsockDialer{}
	}
	m := &SandboxManager{
		root:      root,
		cache:     cache,
		vmm:       vm,
		dialer:    dialer,
		logger:    logger,
		socketDir: socketDir,
		sandboxes: map[string]*Sandbox{},
	}
	m.nextCID.Store(FirstUserCID)
	return m
}

// AllocateCID hands out the next vsock CID. Exposed so the pool can
// pre-assign CIDs before the underlying VM is restored.
func (m *SandboxManager) AllocateCID() uint32 {
	return m.nextCID.Add(1) - 1
}

// BeginCreate registers a new sandbox in CREATING state and returns the
// placeholder. The heavy work (CoW rootfs, snapshot prep, CH restore)
// must be driven by FinishCreate — usually from a goroutine kicked off
// by the HTTP handler so master sees the placeholder in ListSandboxes
// while the VM is still coming up. Synchronous callers (the warm pool,
// integration tests) can use CreateSandbox to get the old all-in-one
// behavior.
//
// Validation that's cheap and synchronous (template hash, cache hit, ID
// allocation, CID allocation) happens here so callers get an immediate
// error for bad inputs instead of a "completed with error" sandbox.
func (m *SandboxManager) BeginCreate(req CreateRequest) (*Sandbox, error) {
	if req.TemplateHash == "" {
		return nil, errors.New("sandbox: template hash required")
	}
	if !m.cache.HasTemplate(req.TemplateHash) {
		return nil, fmt.Errorf("sandbox: template %s not in cache", req.TemplateHash)
	}
	id := req.ID
	if id == "" {
		id = newSandboxID()
	}
	now := time.Now().UTC()
	sb := &Sandbox{
		ID:           id,
		State:        SandboxStateCreating,
		TemplateHash: req.TemplateHash,
		VsockCID:     m.AllocateCID(),
		Config:       req.Config,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	m.mu.Lock()
	if _, exists := m.sandboxes[id]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox: %s already exists", id)
	}
	m.sandboxes[id] = sb
	m.mu.Unlock()
	return cloneSandbox(sb), nil
}

// FinishCreate runs the heavy create work for a sandbox previously
// registered by BeginCreate: CoW-copy the rootfs, materialize a
// per-sandbox snapshot directory with rewritten paths, restore from it,
// and flip the in-memory entry from CREATING to RUNNING. On failure the
// entry is moved to ERROR (with Error populated) and any partial files
// are cleaned up; the entry remains in the map so master's poller can
// observe the failure.
//
// The snapshot must be per-sandbox because the original template's
// config.json carries paths captured at snapshot time — relative disk
// paths that resolve against CH's CWD and a hardcoded vsock socket. Two
// concurrent sandboxes cannot share either: they would race on the same
// rootfs and bind() the same vsock socket. The fix is to copy the
// snapshot dir, rewrite config.json with absolute per-sandbox paths, and
// hand CH the rewritten copy.
func (m *SandboxManager) FinishCreate(ctx context.Context, id string) error {
	sb, err := m.lookup(id)
	if err != nil {
		return err
	}
	if sb.State != SandboxStateCreating {
		return fmt.Errorf("sandbox: %s not in CREATING (got %s)", id, sb.State)
	}
	templateHash := sb.TemplateHash
	layout := m.cache.Layout(templateHash)
	dir := filepath.Join(m.root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.markCreateFailed(id, dir, fmt.Errorf("make dir: %w", err))
		return fmt.Errorf("sandbox: make dir: %w", err)
	}
	overlay := filepath.Join(dir, "rootfs.raw")
	if err := reflinkOrCopy(layout.RootfsPath, overlay); err != nil {
		m.markCreateFailed(id, dir, fmt.Errorf("copy rootfs: %w", err))
		return fmt.Errorf("sandbox: copy rootfs: %w", err)
	}
	snapshotDir, vsockSocketPath, err := prepareSandboxSnapshot(layout, dir)
	if err != nil {
		m.markCreateFailed(id, dir, fmt.Errorf("prepare snapshot: %w", err))
		return fmt.Errorf("sandbox: prepare snapshot: %w", err)
	}
	socketPath, err := m.vmm.RestoreVM(ctx, id, snapshotDir)
	if err != nil {
		m.markCreateFailed(id, dir, fmt.Errorf("restore: %w", err))
		return fmt.Errorf("sandbox: restore: %w", err)
	}
	now := time.Now().UTC()
	m.mu.Lock()
	if cur, ok := m.sandboxes[id]; ok {
		cur.State = SandboxStateRunning
		cur.RootfsPath = overlay
		cur.VsockSocketPath = vsockSocketPath
		cur.APISocket = socketPath
		cur.Healthy = true
		cur.LastHealthAt = now
		cur.UpdatedAt = now
	}
	m.mu.Unlock()
	m.cache.Touch(templateHash)
	m.logger.Info("sandbox created",
		"id", id,
		"template", templateHash,
		"cid", sb.VsockCID,
	)
	return nil
}

// CreateSandbox is the synchronous BeginCreate+FinishCreate helper used
// by the warm pool and integration tests. The HTTP handler does not call
// this — it splits the two halves around the request boundary so it can
// return 202 immediately.
func (m *SandboxManager) CreateSandbox(ctx context.Context, req CreateRequest) (*Sandbox, error) {
	sb, err := m.BeginCreate(req)
	if err != nil {
		return nil, err
	}
	if err := m.FinishCreate(ctx, sb.ID); err != nil {
		return nil, err
	}
	return m.Get(sb.ID)
}

// markCreateFailed flips a CREATING sandbox to ERROR with the given
// reason and removes its on-disk state. The entry stays in the map so
// the master poller can observe the failure on its next GetSandbox tick;
// a later DestroySandbox cleans the map entry.
func (m *SandboxManager) markCreateFailed(id, dir string, cause error) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	m.mu.Lock()
	if cur, ok := m.sandboxes[id]; ok {
		cur.State = SandboxStateError
		cur.Error = cause.Error()
		cur.UpdatedAt = time.Now().UTC()
	}
	m.mu.Unlock()
	m.logger.Error("sandbox create failed", "id", id, "err", cause)
}

// AdoptSandbox registers a Sandbox that some other component (typically
// the pool) materialized. The manager assumes ownership of the lifecycle
// and the caller must not touch the value after handing it off.
func (m *SandboxManager) AdoptSandbox(sb *Sandbox) {
	sb.UpdatedAt = time.Now().UTC()
	m.mu.Lock()
	m.sandboxes[sb.ID] = sb
	m.mu.Unlock()
}

// StopSandbox pauses the VM and snapshots its state to disk so a later
// StartSandbox can resume it. The VMM process is then torn down — only
// the saved state survives until Start.
func (m *SandboxManager) StopSandbox(ctx context.Context, id string) error {
	sb, err := m.lookup(id)
	if err != nil {
		return err
	}
	if sb.State == SandboxStateStopped {
		return nil
	}
	if sb.State != SandboxStateRunning && sb.State != SandboxStatePaused {
		return fmt.Errorf("sandbox: cannot stop in state %s", sb.State)
	}
	stateDir := filepath.Join(m.root, id, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("sandbox: state dir: %w", err)
	}
	if err := m.vmm.SnapshotVM(ctx, sb.APISocket, stateDir, false); err != nil {
		m.markError(id)
		return fmt.Errorf("sandbox: snapshot: %w", err)
	}
	if err := m.vmm.DestroyVM(ctx, sb.APISocket); err != nil {
		m.logger.Warn("destroy after snapshot failed; continuing", "id", id, "err", err)
	}
	m.mu.Lock()
	if cur, ok := m.sandboxes[id]; ok {
		cur.State = SandboxStateStopped
		cur.StateDir = stateDir
		cur.APISocket = ""
		cur.UpdatedAt = time.Now().UTC()
	}
	m.mu.Unlock()
	m.logger.Info("sandbox stopped", "id", id)
	return nil
}

// StartSandbox brings a stopped sandbox back to RUNNING by restoring from
// its saved state and resuming. Errors leave the sandbox in ERROR.
func (m *SandboxManager) StartSandbox(ctx context.Context, id string) error {
	sb, err := m.lookup(id)
	if err != nil {
		return err
	}
	if sb.State == SandboxStateRunning {
		return nil
	}
	if sb.State != SandboxStateStopped {
		return fmt.Errorf("sandbox: cannot start in state %s", sb.State)
	}
	if sb.StateDir == "" {
		return fmt.Errorf("sandbox: no saved state for %s", id)
	}
	socketPath, err := m.vmm.RestoreVM(ctx, id, sb.StateDir)
	if err != nil {
		m.markError(id)
		return fmt.Errorf("sandbox: restore: %w", err)
	}
	m.mu.Lock()
	if cur, ok := m.sandboxes[id]; ok {
		cur.State = SandboxStateRunning
		cur.APISocket = socketPath
		// VsockSocketPath is stable across restart: the saved snapshot
		// references the same per-sandbox vsock.sock the original create
		// rewrote into config.json, so CH bind()s the same path again.
		cur.UpdatedAt = time.Now().UTC()
		cur.Healthy = true
		cur.LastHealthAt = time.Now().UTC()
	}
	m.mu.Unlock()
	m.logger.Info("sandbox started", "id", id)
	return nil
}

// DestroySandbox terminates the VMM (if any), deletes the sandbox's
// on-disk state, and removes it from the registry. Idempotent: a missing
// sandbox is treated as already destroyed.
func (m *SandboxManager) DestroySandbox(ctx context.Context, id string) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.sandboxes, id)
	m.mu.Unlock()

	if sb.APISocket != "" {
		if err := m.vmm.DestroyVM(ctx, sb.APISocket); err != nil {
			m.logger.Warn("destroy vm failed; cleaning files anyway", "id", id, "err", err)
		}
	}
	dir := filepath.Join(m.root, id)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("sandbox: remove dir: %w", err)
	}
	m.logger.Info("sandbox destroyed", "id", id)
	return nil
}

// HealthCheck pings the guest agent on the vsock health port. A nil error
// means the guest responded; the manager's own health bookkeeping is
// updated as a side effect so HealthChecker can call this without
// duplicating state.
func (m *SandboxManager) HealthCheck(ctx context.Context, id string) error {
	sb, err := m.lookup(id)
	if err != nil {
		return err
	}
	if sb.State != SandboxStateRunning {
		return fmt.Errorf("sandbox: not running")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := m.dialer.Dial(dialCtx, sb.VsockSocketPath, GuestExecPort)
	healthy := err == nil
	if conn != nil {
		_ = conn.Close()
	}
	m.mu.Lock()
	if cur, ok := m.sandboxes[id]; ok {
		cur.Healthy = healthy
		cur.LastHealthAt = time.Now().UTC()
	}
	m.mu.Unlock()
	if !healthy {
		return fmt.Errorf("sandbox: health probe failed: %w", err)
	}
	return nil
}

// Get returns a deep copy of the sandbox record, or an error if no such
// sandbox is registered.
func (m *SandboxManager) Get(id string) (*Sandbox, error) {
	sb, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	return cloneSandbox(sb), nil
}

// List returns deep copies of every registered sandbox.
func (m *SandboxManager) List() []*Sandbox {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Sandbox, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		out = append(out, cloneSandbox(sb))
	}
	return out
}

func (m *SandboxManager) lookup(id string) (*Sandbox, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sb, ok := m.sandboxes[id]
	if !ok {
		return nil, fmt.Errorf("sandbox: %s not found", id)
	}
	return sb, nil
}

func (m *SandboxManager) markError(id string) {
	m.mu.Lock()
	if cur, ok := m.sandboxes[id]; ok {
		cur.State = SandboxStateError
		cur.UpdatedAt = time.Now().UTC()
	}
	m.mu.Unlock()
}

func cloneSandbox(s *Sandbox) *Sandbox {
	c := *s
	return &c
}

func newSandboxID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "sb-" + hex.EncodeToString(b[:])
}

// prepareSandboxSnapshot stages a per-sandbox copy of the template's
// snapshot directory under sandboxDir/snapshot and rewrites config.json
// so every path resolves on this host without ambiguity:
//
//   - disk paths → the per-sandbox CoW rootfs (sandboxDir/rootfs.raw)
//   - vsock socket → sandboxDir/vsock.sock (avoids cross-sandbox bind() races)
//   - kernel/initramfs/firmware → absolute paths inside the template cache
//
// CH writes config.json relative to wherever the original VM ran, so a
// fresh restore on a different host (or a second sandbox on the same host)
// fails with NotFound on the disk or EADDRINUSE on the vsock unless we
// rewrite first. Returns the per-sandbox snapshot directory and the
// absolute path of the rewritten vsock socket — callers must store the
// latter on the Sandbox so health probes, exec, file I/O, and forwards
// dial the path CH actually bound, not a stale derived one.
func prepareSandboxSnapshot(layout TemplateLayout, sandboxDir string) (string, string, error) {
	dst := filepath.Join(sandboxDir, "snapshot")
	if err := copyDirReflink(layout.SnapshotDir, dst); err != nil {
		return "", "", fmt.Errorf("copy snapshot dir: %w", err)
	}
	vsockSocketPath := filepath.Join(sandboxDir, "vsock.sock")
	if err := rewriteSnapshotConfig(filepath.Join(dst, "config.json"), sandboxDir, layout); err != nil {
		return "", "", err
	}
	return dst, vsockSocketPath, nil
}

// rewriteSnapshotConfig parses the snapshot's config.json as a generic
// map (so unknown CH fields survive the round trip), patches the few
// path-bearing fields, and writes the result back to the same file.
func rewriteSnapshotConfig(configPath, sandboxDir string, layout TemplateLayout) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config.json: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config.json: %w", err)
	}
	rootfs := filepath.Join(sandboxDir, "rootfs.raw")
	if disks, ok := cfg["disks"].([]any); ok {
		for _, d := range disks {
			disk, ok := d.(map[string]any)
			if !ok {
				continue
			}
			disk["path"] = rootfs
		}
	}
	if vsock, ok := cfg["vsock"].(map[string]any); ok {
		vsock["socket"] = filepath.Join(sandboxDir, "vsock.sock")
	}
	if payload, ok := cfg["payload"].(map[string]any); ok {
		for _, key := range []string{"kernel", "initramfs", "firmware"} {
			v, ok := payload[key].(string)
			if !ok || v == "" || filepath.IsAbs(v) {
				continue
			}
			payload[key] = filepath.Join(layout.Dir, v)
		}
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config.json: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0o644); err != nil {
		return fmt.Errorf("write config.json: %w", err)
	}
	return nil
}

// copyDirReflink mirrors src into dst, using reflinkOrCopy on every
// regular file so the (often hundreds-of-MB) memory image inside a CH
// snapshot is shared via copy-on-write where the filesystem supports it.
// Symlinks and special files are skipped — CH snapshots only contain
// regular files.
func copyDirReflink(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDirReflink(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if !e.Type().IsRegular() {
			continue
		}
		if err := reflinkOrCopy(srcPath, dstPath); err != nil {
			return fmt.Errorf("copy %s: %w", srcPath, err)
		}
	}
	return nil
}

// reflinkOrCopy first tries `cp --reflink=auto` (so on btrfs/xfs/apfs the
// copy is O(1) via shared extents) and falls back to a byte copy when the
// reflink flag is not understood by the local cp. We accept the cost of
// shelling out because the Go stdlib has no reflink primitive.
func reflinkOrCopy(src, dst string) error {
	cmd := exec.Command("cp", "--reflink=auto", src, dst)
	if err := cmd.Run(); err == nil {
		return nil
	}
	return plainCopy(src, dst)
}

func plainCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy: %w", err)
	}
	return out.Close()
}

// hybridVsockDialer speaks Cloud Hypervisor's textual hybrid-vsock
// protocol over the host-side Unix socket: the host writes
// "CONNECT <port>\n", reads "OK <hostport>\n", and from then on the
// connection is a plain bidirectional byte stream wired through to the
// guest's listener on that port.
type hybridVsockDialer struct{}

// Dial implements VsockDialer.
func (hybridVsockDialer) Dial(ctx context.Context, hostSocket string, port uint32) (io.ReadWriteCloser, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", hostSocket)
	if err != nil {
		return nil, fmt.Errorf("dial unix %s: %w", hostSocket, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read CONNECT reply: %w", err)
	}
	if len(line) < 2 || line[:2] != "OK" {
		_ = conn.Close()
		return nil, fmt.Errorf("vsock CONNECT rejected: %q", line)
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn keeps the bufio.Reader used to parse the CONNECT handshake
// in front of the wire — without it, any bytes that arrived alongside the
// "OK ..." line would be lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

// Read pulls bytes through the buffered reader.
func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func readJSONLine(r io.Reader, v any) error {
	br := bufio.NewReader(r)
	line, err := br.ReadBytes('\n')
	if err != nil && (err != io.EOF || len(line) == 0) {
		return err
	}
	return json.Unmarshal(line, v)
}
