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

	mu          sync.Mutex
	restoreCalls   int
	snapshotCalls  int
	destroyCalls   int
	failRestore    bool
	failSnapshot   bool
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

var (
	errFakeRestore  = errFake("fake restore error")
	errFakeSnapshot = errFake("fake snapshot error")
)

type errFake string

func (e errFake) Error() string { return string(e) }

// ===== test helpers =====

// seedTemplate creates a fake template directory under cacheDir keyed by a
// SHA256 of bodyBytes (the rootfs payload). The template directory is
// minimally populated with rootfs.raw so HasTemplate returns true.
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
// and guest ends of a net.Pipe into the manager.
type fixedDialer struct{ conn net.Conn }

func (f *fixedDialer) Dial(_ context.Context, _ string, _ uint32) (io.ReadWriteCloser, error) {
	return f.conn, nil
}
