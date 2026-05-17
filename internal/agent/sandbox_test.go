package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeVMM stands in for vmm.VMManager. It records calls and returns
// synthetic socket paths so SandboxManager logic can be exercised on a
// host without cloud-hypervisor.
type fakeVMM struct {
	socketDir string

	mu            sync.Mutex
	restoreCalls  int
	snapshotCalls int
	destroyCalls  int
	pauseCalls    int
	resumeCalls   int
	failRestore   bool
	failSnapshot  bool
	failResume    bool
}

func newFakeVMM(t *testing.T) *fakeVMM {
	t.Helper()
	return &fakeVMM{socketDir: t.TempDir()}
}

func (f *fakeVMM) RestoreVM(_ context.Context, vmID, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restoreCalls++
	if f.failRestore {
		return "", errFakeRestore
	}
	return filepath.Join(f.socketDir, vmID+".sock"), nil
}

func (f *fakeVMM) RestoreVMPaused(_ context.Context, vmID, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restoreCalls++
	if f.failRestore {
		return "", errFakeRestore
	}
	return filepath.Join(f.socketDir, vmID+".sock"), nil
}

func (f *fakeVMM) SnapshotVM(_ context.Context, _, _ string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotCalls++
	if f.failSnapshot {
		return errFakeSnapshot
	}
	return nil
}

func (f *fakeVMM) DestroyVM(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyCalls++
	return nil
}

func (f *fakeVMM) PauseVM(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseCalls++
	return nil
}

func (f *fakeVMM) ResumeVM(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls++
	if f.failResume {
		return errFakeResume
	}
	return nil
}

// restores / destroys are lock-safe accessors so tests can read the call
// counters while the pool's background loops are still mutating them.
func (f *fakeVMM) restores() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restoreCalls
}

func (f *fakeVMM) destroys() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.destroyCalls
}

var (
	errFakeRestore  = errFake("fake restore error")
	errFakeSnapshot = errFake("fake snapshot error")
	errFakeResume   = errFake("fake resume error")
)

type errFake string

func (e errFake) Error() string { return string(e) }

// ===== test helpers =====

// seedTemplate creates a fake template directory under cacheDir keyed by a
// SHA256 of bodyBytes (the rootfs payload). The template is populated with
// rootfs.raw plus a snapshot/config.json mimicking what cloud-hypervisor
// emits — relative disk and kernel paths, and a hardcoded vsock socket —
// so tests exercise the path-rewrite logic the same way real templates do.
func seedTemplate(t *testing.T, cacheDir string, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	dir := filepath.Join(cacheDir, hash)
	if err := os.MkdirAll(filepath.Join(dir, "snapshot"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rootfs.raw"), body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := map[string]any{
		"disks": []any{
			map[string]any{"path": "noble-server-cloudimg-amd64.raw", "readonly": false},
		},
		"vsock": map[string]any{
			"cid":    3,
			"socket": "/tmp/ch-vsock.sock",
		},
		"payload": map[string]any{
			"kernel":  "vmlinux",
			"cmdline": "console=hvc0 root=/dev/vda1",
		},
		// An unmodelled field that must survive the round trip — proves the
		// rewriter doesn't truncate config.json down to known keys.
		"console": map[string]any{"mode": "Off"},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot", "config.json"), data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return hash
}

func newTestManager(t *testing.T) (*SandboxManager, *fakeVMM, string) {
	t.Helper()
	cacheDir := t.TempDir()
	root := t.TempDir()
	vm := newFakeVMM(t)
	cache := NewImageCache(cacheDir, 0, nil)
	mgr := NewSandboxManager(root, vm.socketDir, cache, vm, &noopDialer{}, nil)
	return mgr, vm, cacheDir
}

type noopDialer struct{}

func (noopDialer) Dial(_ context.Context, _ string, _ uint32) (io.ReadWriteCloser, error) {
	return nil, errFake("noop dialer cannot connect")
}

// ===== tests =====

func TestSandboxLifecycle(t *testing.T) {
	mgr, vm, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))

	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{
		TemplateHash: hash,
		Config:       SandboxConfig{VCPUs: 2, MemoryMB: 512},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sb.State != SandboxStateRunning {
		t.Fatalf("expected RUNNING, got %s", sb.State)
	}
	if sb.VsockCID < FirstUserCID {
		t.Fatalf("CID below reserved range: %d", sb.VsockCID)
	}
	if vm.restoreCalls != 1 {
		t.Fatalf("expected 1 restore call, got %d", vm.restoreCalls)
	}
	if _, err := os.Stat(sb.RootfsPath); err != nil {
		t.Fatalf("rootfs overlay missing: %v", err)
	}

	// Stop persists state and tears down the VMM.
	if err := mgr.StopSandbox(context.Background(), sb.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	got, err := mgr.Get(sb.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != SandboxStateStopped {
		t.Fatalf("expected STOPPED, got %s", got.State)
	}
	if got.StateDir == "" {
		t.Fatalf("stop should populate StateDir")
	}
	if vm.snapshotCalls != 1 {
		t.Fatalf("expected snapshot call, got %d", vm.snapshotCalls)
	}

	// Start restores from saved state.
	if err := mgr.StartSandbox(context.Background(), sb.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	got, _ = mgr.Get(sb.ID)
	if got.State != SandboxStateRunning {
		t.Fatalf("expected RUNNING after start, got %s", got.State)
	}
	if vm.restoreCalls != 2 {
		t.Fatalf("expected 2 restore calls, got %d", vm.restoreCalls)
	}

	// Destroy cleans up files and registry.
	if err := mgr.DestroySandbox(context.Background(), sb.ID); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := mgr.Get(sb.ID); err == nil {
		t.Fatalf("expected sandbox to be gone after destroy")
	}
	if _, err := os.Stat(sb.RootfsPath); !os.IsNotExist(err) {
		t.Fatalf("expected overlay to be gone, stat err = %v", err)
	}
}

func TestSandboxCreateRejectsUnknownTemplate(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	_, err := mgr.CreateSandbox(context.Background(), CreateRequest{
		TemplateHash: "deadbeef",
	})
	if err == nil {
		t.Fatalf("expected error for missing template")
	}
}

func TestSandboxCreateRewritesSnapshotConfig(t *testing.T) {
	mgr, _, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))

	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sandboxDir := filepath.Join(mgr.root, sb.ID)
	rewritten, err := os.ReadFile(filepath.Join(sandboxDir, "snapshot", "config.json"))
	if err != nil {
		t.Fatalf("read rewritten config: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(rewritten, &got); err != nil {
		t.Fatalf("parse rewritten config: %v", err)
	}

	disks, ok := got["disks"].([]any)
	if !ok || len(disks) == 0 {
		t.Fatalf("disks missing from rewritten config: %v", got)
	}
	disk0 := disks[0].(map[string]any)
	wantDisk := filepath.Join(sandboxDir, "rootfs.qcow2")
	if disk0["path"] != wantDisk {
		t.Errorf("disk path: got %q, want %q", disk0["path"], wantDisk)
	}
	if !filepath.IsAbs(disk0["path"].(string)) {
		t.Errorf("disk path not absolute: %q", disk0["path"])
	}
	// Unmodelled fields on the disk must survive the round trip.
	if disk0["readonly"] != false {
		t.Errorf("disk readonly key lost: %v", disk0)
	}

	vsock := got["vsock"].(map[string]any)
	wantVsock := filepath.Join(sandboxDir, "vsock.sock")
	if vsock["socket"] != wantVsock {
		t.Errorf("vsock socket: got %q, want %q", vsock["socket"], wantVsock)
	}
	// CID is unrelated to paths and must be preserved.
	if vsock["cid"] == nil {
		t.Errorf("vsock cid lost: %v", vsock)
	}

	payload := got["payload"].(map[string]any)
	wantKernel := filepath.Join(cacheDir, hash, "vmlinux")
	if payload["kernel"] != wantKernel {
		t.Errorf("kernel path: got %q, want %q", payload["kernel"], wantKernel)
	}
	if !filepath.IsAbs(payload["kernel"].(string)) {
		t.Errorf("kernel path not absolute: %q", payload["kernel"])
	}
	// cmdline is unrelated to paths and must round-trip.
	if payload["cmdline"] != "console=hvc0 root=/dev/vda1" {
		t.Errorf("cmdline mutated: %q", payload["cmdline"])
	}
	// Unmodelled top-level field must survive.
	if got["console"] == nil {
		t.Errorf("unmodelled console field dropped: %v", got)
	}

	// The per-sandbox rootfs the disk path points at must actually exist.
	if _, err := os.Stat(disk0["path"].(string)); err != nil {
		t.Errorf("rewritten disk path does not exist: %v", err)
	}

	// The original template snapshot must NOT have been mutated — multiple
	// sandboxes sharing one template must each get a fresh copy.
	originalRaw, err := os.ReadFile(filepath.Join(cacheDir, hash, "snapshot", "config.json"))
	if err != nil {
		t.Fatalf("read original config: %v", err)
	}
	var original map[string]any
	if err := json.Unmarshal(originalRaw, &original); err != nil {
		t.Fatalf("parse original: %v", err)
	}
	origDiskPath := original["disks"].([]any)[0].(map[string]any)["path"]
	if origDiskPath != "noble-server-cloudimg-amd64.raw" {
		t.Errorf("template snapshot mutated; disk path is now %q", origDiskPath)
	}
}

func TestSandboxCreateRollsBackOnRestoreFailure(t *testing.T) {
	mgr, vm, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	vm.failRestore = true

	_, err := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})
	if err == nil {
		t.Fatalf("expected restore failure")
	}
	entries, err := os.ReadDir(mgr.root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected sandbox dir cleaned on rollback; got %d entries", len(entries))
	}
}

func TestSandboxStopMarksErrorOnSnapshotFailure(t *testing.T) {
	mgr, vm, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	vm.failSnapshot = true
	if err := mgr.StopSandbox(context.Background(), sb.ID); err == nil {
		t.Fatalf("expected snapshot failure")
	}
	got, _ := mgr.Get(sb.ID)
	if got.State != SandboxStateError {
		t.Fatalf("expected ERROR after snapshot failure, got %s", got.State)
	}
}

func TestExecCommandRoundTrip(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()

	mgr, _, cacheDir := newTestManager(t)
	mgr.dialer = &fixedDialer{conn: host}
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Stand in for the guest agent: read the request line, write a result.
	go func() {
		buf := make([]byte, 4096)
		n, _ := guest.Read(buf)
		var req struct {
			Command   string `json:"command"`
			TimeoutMS int64  `json:"timeout_ms"`
		}
		_ = json.Unmarshal(bytes.TrimSpace(buf[:n]), &req)
		resp, _ := json.Marshal(ExecResult{ExitCode: 0, Stdout: "echo:" + req.Command})
		_, _ = guest.Write(append(resp, '\n'))
	}()

	res, err := mgr.ExecCommand(context.Background(), sb.ID, "ls", 2*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "echo:ls" {
		t.Fatalf("unexpected exec result: %+v", res)
	}
}

// fixedDialer always returns the supplied conn, so a test can wire host
// and guest ends of a net.Pipe into the manager. The hostSocket the
// caller passed in is recorded so tests can assert the dialer was
// pointed at the per-sandbox vsock path, not a stale derived one.
type fixedDialer struct {
	conn       net.Conn
	mu         sync.Mutex
	lastSocket string
}

func (f *fixedDialer) Dial(_ context.Context, hostSocket string, _ uint32) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	f.lastSocket = hostSocket
	f.mu.Unlock()
	return f.conn, nil
}

// TestSandboxVsockSocketPathMatchesRewrittenConfig pins the regression
// from the agent log: health/exec/files/forward dialed an old derived
// path while CH actually bound a per-sandbox path inside the snapshot.
// The Sandbox struct must carry the rewritten path so all callers dial
// the path that exists.
func TestSandboxVsockSocketPathMatchesRewrittenConfig(t *testing.T) {
	mgr, _, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))

	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sandboxDir := filepath.Join(mgr.root, sb.ID)
	wantVsock := filepath.Join(sandboxDir, "vsock.sock")
	if sb.VsockSocketPath != wantVsock {
		t.Fatalf("VsockSocketPath = %q, want %q", sb.VsockSocketPath, wantVsock)
	}

	// And the value must match what the rewritten config.json points at —
	// otherwise CH and the host would still disagree on where to bind.
	rewritten, err := os.ReadFile(filepath.Join(sandboxDir, "snapshot", "config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(rewritten, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	configVsock := cfg["vsock"].(map[string]any)["socket"]
	if configVsock != sb.VsockSocketPath {
		t.Fatalf("config.json vsock socket %q != sb.VsockSocketPath %q", configVsock, sb.VsockSocketPath)
	}

	// VsockSocketPath must NOT be derived from the API socket — that's
	// exactly the bug the agent log surfaced.
	apiDerived := sb.APISocket
	if apiDerived != "" && sb.VsockSocketPath == apiDerived[:len(apiDerived)-len(filepath.Ext(apiDerived))]+"-vsock.sock" {
		t.Fatalf("VsockSocketPath looks derived from APISocket: %q", sb.VsockSocketPath)
	}
}

// TestSandboxStartPreservesVsockSocketPath guards against StartSandbox
// stomping the per-sandbox path with a re-derived one when the VM is
// restored after a Stop. The path is invariant across restart.
func TestSandboxStartPreservesVsockSocketPath(t *testing.T) {
	mgr, _, cacheDir := newTestManager(t)
	hash := seedTemplate(t, cacheDir, []byte("rootfs"))

	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	original := sb.VsockSocketPath

	if err := mgr.StopSandbox(context.Background(), sb.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := mgr.StartSandbox(context.Background(), sb.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	got, _ := mgr.Get(sb.ID)
	if got.VsockSocketPath != original {
		t.Fatalf("VsockSocketPath changed across restart: was %q, now %q", original, got.VsockSocketPath)
	}
}

// TestHealthCheckDialsPerSandboxVsockPath proves the manager hands the
// per-sandbox path to the dialer. Without the rename + rewrite, this
// would dial the old "<socketDir>/<id>-vsock.sock" path.
func TestHealthCheckDialsPerSandboxVsockPath(t *testing.T) {
	mgr, _, cacheDir := newTestManager(t)
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	dialer := &fixedDialer{conn: host}
	mgr.dialer = dialer

	hash := seedTemplate(t, cacheDir, []byte("rootfs"))
	sb, err := mgr.CreateSandbox(context.Background(), CreateRequest{TemplateHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := mgr.HealthCheck(context.Background(), sb.ID); err != nil {
		t.Fatalf("health: %v", err)
	}
	dialer.mu.Lock()
	got := dialer.lastSocket
	dialer.mu.Unlock()
	if got != sb.VsockSocketPath {
		t.Fatalf("health dialed %q, want VsockSocketPath %q", got, sb.VsockSocketPath)
	}
}
