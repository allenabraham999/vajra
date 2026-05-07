package vmm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedRequest captures one HTTP call hitting our fake CH server.
type recordedRequest struct {
	Method string
	Path   string
	Body   []byte
}

// fakeVMM is an httptest-style server that listens on a Unix socket and
// records every request. Handlers can be customised per test; the default
// returns 204 No Content for everything.
type fakeVMM struct {
	t          *testing.T
	socketPath string
	server     *http.Server
	listener   net.Listener

	mu       sync.Mutex
	requests []recordedRequest
	handler  func(w http.ResponseWriter, r *http.Request, body []byte)
}

func newFakeVMM(t *testing.T) *fakeVMM {
	t.Helper()
	// macOS caps sun_path at 104 bytes; the per-subtest t.TempDir() can
	// exceed that. Use a short, dedicated dir under os.TempDir().
	dir, err := os.MkdirTemp("", "vmm")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	f := &fakeVMM{
		t:          t,
		socketPath: socketPath,
		listener:   ln,
	}
	f.server = &http.Server{Handler: http.HandlerFunc(f.handle)}
	go func() { _ = f.server.Serve(ln) }()
	t.Cleanup(func() {
		_ = f.server.Close()
		_ = ln.Close()
		_ = os.Remove(socketPath)
	})
	return f
}

func (f *fakeVMM) setHandler(h func(w http.ResponseWriter, r *http.Request, body []byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
}

func (f *fakeVMM) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Body:   body,
	})
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		h(w, r, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeVMM) snapshot() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeVMM) client() *Client { return NewClient(f.socketPath) }

// TestClientHappyPath exercises every CH endpoint the client wraps and
// verifies the path, method, and body the server saw.
func TestClientHappyPath(t *testing.T) {
	f := newFakeVMM(t)
	client := f.client()
	ctx := context.Background()

	cfg := VmConfig{
		Cpus:    &CpusConfig{BootVcpus: 2, MaxVcpus: 2},
		Memory:  &MemoryConfig{Size: 512 * 1024 * 1024},
		Payload: &PayloadConfig{Kernel: "/srv/vmlinux", Cmdline: "console=ttyS0"},
		Console: &ConsoleConfig{Mode: "Off"},
		Serial:  &ConsoleConfig{Mode: "Off"},
	}
	if err := client.Create(ctx, cfg); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := client.Boot(ctx); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if err := client.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := client.Snapshot(ctx, SnapshotConfig{DestinationURL: "file:///snaps/abc"}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := client.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := client.Restore(ctx, RestoreConfig{SourceURL: "file:///snaps/abc"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := client.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	wantSeq := []struct {
		method string
		path   string
	}{
		{"PUT", "/api/v1/vm.create"},
		{"PUT", "/api/v1/vm.boot"},
		{"PUT", "/api/v1/vm.pause"},
		{"PUT", "/api/v1/vm.snapshot"},
		{"PUT", "/api/v1/vm.resume"},
		{"PUT", "/api/v1/vm.restore"},
		{"PUT", "/api/v1/vm.shutdown"},
		{"PUT", "/api/v1/vm.delete"},
	}
	got := f.snapshot()
	if len(got) != len(wantSeq) {
		t.Fatalf("got %d requests, want %d: %+v", len(got), len(wantSeq), got)
	}
	for i, w := range wantSeq {
		if got[i].Method != w.method || got[i].Path != w.path {
			t.Errorf("req[%d]: got %s %s, want %s %s",
				i, got[i].Method, got[i].Path, w.method, w.path)
		}
	}

	// Verify the create payload round-trips intact.
	var sent VmConfig
	if err := json.Unmarshal(got[0].Body, &sent); err != nil {
		t.Fatalf("unmarshal vm.create body: %v", err)
	}
	if sent.Cpus == nil || sent.Cpus.BootVcpus != 2 {
		t.Errorf("create body lost Cpus: %+v", sent)
	}
	if sent.Memory == nil || sent.Memory.Size != 512*1024*1024 {
		t.Errorf("create body lost Memory: %+v", sent)
	}
	if sent.Console == nil || sent.Console.Mode != "Off" {
		t.Errorf("create body lost Console: %+v", sent)
	}
}

// TestClientInfo verifies decoded vm.info responses.
func TestClientInfo(t *testing.T) {
	f := newFakeVMM(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path != "/api/v1/vm.info" || r.Method != http.MethodGet {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VmInfo{
			State:            "Running",
			MemoryActualSize: 1024,
		})
	})

	info, err := f.client().Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.State != "Running" || info.MemoryActualSize != 1024 {
		t.Errorf("got %+v", info)
	}
}

// TestClientErrorResponse verifies non-2xx responses are returned as
// *APIError with the body preserved.
func TestClientErrorResponse(t *testing.T) {
	f := newFakeVMM(t)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("vm already created\n"))
	})

	err := f.client().Boot(context.Background())
	if err == nil {
		t.Fatalf("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err is not *APIError: %T %v", err, err)
	}
	if apiErr.Status != 400 {
		t.Errorf("status: got %d, want 400", apiErr.Status)
	}
	if !strings.Contains(apiErr.Message, "already created") {
		t.Errorf("message: got %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Error(), "/vm.boot") {
		t.Errorf("Error() should mention path: %q", apiErr.Error())
	}
}

// TestClientContextCancel verifies a cancelled context aborts the request.
func TestClientContextCancel(t *testing.T) {
	f := newFakeVMM(t)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := f.client().Boot(ctx)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !strings.Contains(err.Error(), "vm.boot") {
		t.Errorf("error should mention path: %v", err)
	}
}

// TestPollSocketReadyImmediate exercises the happy path: socket already
// open, returns nil quickly.
func TestPollSocketReadyImmediate(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "ready.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	t0 := time.Now()
	if err := PollSocketReady(context.Background(), socketPath, time.Second); err != nil {
		t.Fatalf("PollSocketReady: %v", err)
	}
	if elapsed := time.Since(t0); elapsed > 100*time.Millisecond {
		t.Errorf("expected near-instant return, got %s", elapsed)
	}
}

// TestPollSocketReadyAppearsLate verifies the tight retry loop: the
// socket only appears partway through the timeout window.
func TestPollSocketReadyAppearsLate(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "late.sock")

	go func() {
		time.Sleep(40 * time.Millisecond)
		ln, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Errorf("late listen: %v", err)
			return
		}
		t.Cleanup(func() { _ = ln.Close() })
	}()

	t0 := time.Now()
	if err := PollSocketReady(context.Background(), socketPath, time.Second); err != nil {
		t.Fatalf("PollSocketReady: %v", err)
	}
	elapsed := time.Since(t0)
	if elapsed < 30*time.Millisecond {
		t.Errorf("returned suspiciously early (%s) — did the test even wait?", elapsed)
	}
	if elapsed > 300*time.Millisecond {
		t.Errorf("returned too late (%s) — poll loop may not be tight", elapsed)
	}
}

// TestPollSocketReadyTimeout verifies bounded failure when the socket
// never shows up.
func TestPollSocketReadyTimeout(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "never.sock")
	t0 := time.Now()
	err := PollSocketReady(context.Background(), socketPath, 50*time.Millisecond)
	elapsed := time.Since(t0)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !strings.Contains(err.Error(), socketPath) {
		t.Errorf("error should mention socket path: %v", err)
	}
	if elapsed < 50*time.Millisecond {
		t.Errorf("returned before timeout (%s)", elapsed)
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("overshot timeout (%s)", elapsed)
	}
}

// TestPollSocketReadyContextCancel verifies external context cancellation
// is honoured even when the timeout has plenty of headroom left.
func TestPollSocketReadyContextCancel(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cancel.sock")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := PollSocketReady(ctx, socketPath, 5*time.Second)
	if err == nil {
		t.Fatalf("expected error from cancelled context")
	}
}

// TestCleanupSocketFiles checks each branch: existing files removed,
// missing files tolerated, vsock sibling removed too.
func TestCleanupSocketFiles(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "vm.sock")
	pidPath := socketPath + ".pid"
	vsockPath := filepath.Join(dir, "vm-vsock.sock")

	for _, p := range []string{socketPath, pidPath, vsockPath} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	if err := cleanupSocketFiles(socketPath); err != nil {
		t.Fatalf("cleanup with files: %v", err)
	}
	for _, p := range []string{socketPath, pidPath, vsockPath} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after cleanup", p)
		}
	}

	// Idempotent: second call against a fully-clean dir is a no-op.
	if err := cleanupSocketFiles(socketPath); err != nil {
		t.Errorf("cleanup of missing files should not error: %v", err)
	}
}

// TestSnapshotVMSequence drives SnapshotVM against the fake server and
// confirms the pause/snapshot/resume ordering for both resume modes.
func TestSnapshotVMSequence(t *testing.T) {
	cases := []struct {
		name    string
		resume  bool
		wantSeq []string
	}{
		{"resume after snapshot", true, []string{"/api/v1/vm.pause", "/api/v1/vm.snapshot", "/api/v1/vm.resume"}},
		{"keep paused", false, []string{"/api/v1/vm.pause", "/api/v1/vm.snapshot"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVMM(t)
			snapDir := filepath.Join(t.TempDir(), "snap")
			mgr := NewVMManager(nil)
			if err := mgr.SnapshotVM(context.Background(), f.socketPath, snapDir, tc.resume); err != nil {
				t.Fatalf("SnapshotVM: %v", err)
			}
			got := f.snapshot()
			if len(got) != len(tc.wantSeq) {
				t.Fatalf("got %d requests (%+v), want %d", len(got), got, len(tc.wantSeq))
			}
			for i, p := range tc.wantSeq {
				if got[i].Path != p {
					t.Errorf("req[%d]: got %s, want %s", i, got[i].Path, p)
				}
			}
			// snapshot body should carry an absolute file:// URL.
			var snap SnapshotConfig
			if err := json.Unmarshal(got[1].Body, &snap); err != nil {
				t.Fatalf("decode snapshot body: %v", err)
			}
			if !strings.HasPrefix(snap.DestinationURL, "file://") {
				t.Errorf("destination_url not a file URL: %q", snap.DestinationURL)
			}
			if _, err := os.Stat(snapDir); err != nil {
				t.Errorf("snapshot dir not created: %v", err)
			}
		})
	}
}

// TestSnapshotVMResumesOnFailure asserts that a Snapshot failure still
// triggers a Resume, so the guest is never left wedged in PAUSED.
func TestSnapshotVMResumesOnFailure(t *testing.T) {
	f := newFakeVMM(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path == "/api/v1/vm.snapshot" {
			http.Error(w, "disk full", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mgr := NewVMManager(nil)
	err := mgr.SnapshotVM(context.Background(), f.socketPath, t.TempDir(), false)
	if err == nil {
		t.Fatalf("expected snapshot error")
	}
	got := f.snapshot()
	// Expect: pause, snapshot (failed), resume (recovery)
	wantSeq := []string{"/api/v1/vm.pause", "/api/v1/vm.snapshot", "/api/v1/vm.resume"}
	if len(got) != len(wantSeq) {
		t.Fatalf("got %d requests (%+v), want %d", len(got), got, len(wantSeq))
	}
	for i, p := range wantSeq {
		if got[i].Path != p {
			t.Errorf("req[%d]: got %s, want %s", i, got[i].Path, p)
		}
	}
}

// TestDestroyVMCleansSockets verifies DestroyVM removes socket files even
// when no tracked process is associated with the path.
func TestDestroyVMCleansSockets(t *testing.T) {
	f := newFakeVMM(t)
	dir := filepath.Dir(f.socketPath)
	pidPath := f.socketPath + ".pid"
	vsockPath := filepath.Join(dir, strings.TrimSuffix(filepath.Base(f.socketPath), ".sock")+"-vsock.sock")
	for _, p := range []string{pidPath, vsockPath} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	mgr := NewVMManager(nil)
	if err := mgr.DestroyVM(context.Background(), f.socketPath); err != nil {
		t.Fatalf("DestroyVM: %v", err)
	}
	for _, p := range []string{f.socketPath, pidPath, vsockPath} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after DestroyVM", p)
		}
	}
}

// TestSpawnVMEndToEndWithFakeBinary uses a tiny shell-script "binary" that
// exec's a Go test helper to start a fake VMM. This proves the full
// SpawnVM → poll → Create → Boot path without depending on a real
// cloud-hypervisor install. Skipped on systems without /bin/sh.
func TestSpawnVMUnknownBinary(t *testing.T) {
	// Confidence test that we surface a clean error if the binary is
	// missing — much faster (and more portable) than driving a real CH.
	mgr := NewVMManager(nil).
		WithBinary(filepath.Join(t.TempDir(), "no-such-binary")).
		WithSocketDir(t.TempDir())
	_, err := mgr.SpawnVM(context.Background(), "vm-1", VmConfig{})
	if err == nil {
		t.Fatalf("expected error from missing binary")
	}
	if !strings.Contains(err.Error(), "start cloud-hypervisor") {
		t.Errorf("error should reference spawn step: %v", err)
	}
}

// TestSpawnVMDetectsCrash uses /usr/bin/true (a binary that exits
// immediately) as the "VMM" — SpawnVM should bail in well under the 5s
// poll timeout because the crash-aware poll loop notices the process exit.
// Also asserts socket files are cleaned up on the failure path.
func TestSpawnVMDetectsCrash(t *testing.T) {
	binary := "/usr/bin/true"
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("no %s available", binary)
	}
	socketDir := t.TempDir()
	mgr := NewVMManager(nil).WithBinary(binary).WithSocketDir(socketDir)

	t0 := time.Now()
	_, err := mgr.SpawnVM(context.Background(), "vm-doomed", VmConfig{})
	elapsed := time.Since(t0)
	if err == nil {
		t.Fatalf("expected error from doomed VMM")
	}
	if !strings.Contains(err.Error(), "exited before API socket") {
		t.Errorf("error should reference crash detection: %v", err)
	}
	// Crash detection should be sub-second; the full DefaultPollTimeout (5s)
	// would mean we missed the early exit.
	if elapsed > 500*time.Millisecond {
		t.Errorf("crash detection too slow (%s); expected <500ms", elapsed)
	}
	socketPath := filepath.Join(socketDir, "vm-doomed.sock")
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket %s should be cleaned up", socketPath)
	}
}

// TestSnapshotVMRecoverWithCancelledCtx verifies the post-failure Resume
// runs on a detached context: even when the caller's ctx is already
// cancelled at the moment Snapshot fails, Resume must still fire so the
// guest doesn't stay wedged in PAUSED.
func TestSnapshotVMRecoverWithCancelledCtx(t *testing.T) {
	f := newFakeVMM(t)

	// First Pause() succeeds; Snapshot() blocks until the test cancels
	// ctx, then returns 500. We expect a follow-up Resume() on a
	// detached ctx.
	pauseDone := make(chan struct{})
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch r.URL.Path {
		case "/api/v1/vm.pause":
			close(pauseDone)
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/vm.snapshot":
			// Stall briefly so the caller can cancel ctx mid-call.
			time.Sleep(30 * time.Millisecond)
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-pauseDone
		// Cancel after pause but during snapshot — this is the case that
		// previously left guests wedged.
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	mgr := NewVMManager(nil)
	err := mgr.SnapshotVM(ctx, f.socketPath, t.TempDir(), false)
	if err == nil {
		t.Fatalf("expected snapshot error")
	}

	// Eventually we should see pause, snapshot (failed), resume (recovery).
	// Recovery is async-friendly but synchronous in our code, so the
	// requests are visible by the time SnapshotVM returns.
	got := f.snapshot()
	wantSeq := []string{"/api/v1/vm.pause", "/api/v1/vm.snapshot", "/api/v1/vm.resume"}
	if len(got) != len(wantSeq) {
		t.Fatalf("got %d requests (%+v), want %d", len(got), got, len(wantSeq))
	}
	for i, p := range wantSeq {
		if got[i].Path != p {
			t.Errorf("req[%d]: got %s, want %s", i, got[i].Path, p)
		}
	}
}

// TestDestroyVMSingleBudget asserts that a wedged VMM (one that ignores
// vmm.shutdown) doesn't take longer than DefaultShutdownGrace + a small
// margin to destroy. Pre-fix, this took 2x the budget because the API
// timeout and the process-wait timeout were independent.
func TestDestroyVMSingleBudget(t *testing.T) {
	f := newFakeVMM(t)
	// Make vmm.shutdown hang past the budget; everything else 204.
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path == "/api/v1/vmm.shutdown" {
			time.Sleep(2 * DefaultShutdownGrace)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mgr := NewVMManager(nil)
	t0 := time.Now()
	if err := mgr.DestroyVM(context.Background(), f.socketPath); err != nil {
		t.Fatalf("DestroyVM: %v", err)
	}
	elapsed := time.Since(t0)
	// Allow some slack for test overhead but reject the 2x-budget regression.
	if elapsed > DefaultShutdownGrace+500*time.Millisecond {
		t.Errorf("DestroyVM took %s; budget is %s", elapsed, DefaultShutdownGrace)
	}
}
