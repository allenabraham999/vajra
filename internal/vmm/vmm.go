package vmm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultBinaryPath is where setup-host.sh installs cloud-hypervisor.
	DefaultBinaryPath = "/usr/local/bin/cloud-hypervisor"
	// DefaultSocketDir is the per-host directory holding API sockets.
	DefaultSocketDir = "/tmp/vajra/sockets"
	// SocketPollInterval is the dial cadence used by PollSocketReady. 5ms
	// is fast enough that polling rather than sleeping is the dominant
	// reason restore drops from 312ms (shell prototype) to ~160ms.
	SocketPollInterval = 5 * time.Millisecond
	// DefaultPollTimeout bounds how long we wait for the API socket on
	// spawn/restore before giving up.
	DefaultPollTimeout = 5 * time.Second
	// DefaultShutdownGrace is the total wall-clock budget DestroyVM gives
	// the VMM to exit cleanly (vmm.shutdown + process exit combined)
	// before SIGKILL.
	DefaultShutdownGrace = 3 * time.Second
)

// VMManager owns cloud-hypervisor child processes and their API sockets. A
// single VMManager is meant to be shared across all sandboxes on a host.
type VMManager struct {
	binaryPath string
	socketDir  string
	logger     *slog.Logger

	// procs maps socket path -> *managedProc. We need it so DestroyVM
	// can SIGKILL a wedged VMM and so leaks during failed spawns can be
	// reaped.
	procs sync.Map
}

// NewVMManager returns a VMManager using DefaultBinaryPath and
// DefaultSocketDir. Pass nil for logger to use slog.Default.
func NewVMManager(logger *slog.Logger) *VMManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &VMManager{
		binaryPath: DefaultBinaryPath,
		socketDir:  DefaultSocketDir,
		logger:     logger,
	}
}

// WithBinary overrides the cloud-hypervisor binary path. Returns the
// receiver for chaining.
func (m *VMManager) WithBinary(path string) *VMManager {
	m.binaryPath = path
	return m
}

// WithSocketDir overrides the directory where API sockets are placed.
func (m *VMManager) WithSocketDir(dir string) *VMManager {
	m.socketDir = dir
	return m
}

// SpawnVM starts a fresh cloud-hypervisor process for vmID, waits for the
// API socket to accept connections, then issues vm.create + vm.boot. On
// any failure (including the child process exiting before the socket is
// ready) the child is killed and socket files are removed, so callers
// don't have to clean up partial state.
func (m *VMManager) SpawnVM(ctx context.Context, vmID string, cfg VmConfig) (string, error) {
	socketPath, proc, err := m.startProcess(ctx, vmID, nil)
	if err != nil {
		return "", err
	}
	if err := pollSocketReadyOrExit(ctx, socketPath, DefaultPollTimeout, proc.done); err != nil {
		m.killProcess(socketPath)
		return "", fmt.Errorf("wait for VMM socket: %w", err)
	}
	client := NewClient(socketPath)
	if err := client.Create(ctx, cfg); err != nil {
		m.killProcess(socketPath)
		return "", fmt.Errorf("create vm: %w", err)
	}
	if err := client.Boot(ctx); err != nil {
		m.killProcess(socketPath)
		return "", fmt.Errorf("boot vm: %w", err)
	}
	m.logger.Info("vm running", "vm_id", vmID, "socket", socketPath)
	return socketPath, nil
}

// RestoreVM starts cloud-hypervisor with --restore source_url=<snapshot>,
// waits for the API socket, then resumes the restored (paused) VM.
// snapshotPath may be a filesystem path or a "file://..." URL.
//
// We deliberately do NOT pass --kernel here. The snapshot directory's
// config.json already carries the full VM config including the kernel path;
// passing --kernel makes CH boot a fresh VM instead of restoring, leaving the
// guest in Running state and causing Resume to fail with
// InvalidStateTransition(Running, Running).
func (m *VMManager) RestoreVM(ctx context.Context, vmID, snapshotPath string) (string, error) {
	sourceURL := snapshotPath
	if !strings.HasPrefix(sourceURL, "file://") {
		abs, err := filepath.Abs(snapshotPath)
		if err != nil {
			return "", fmt.Errorf("resolve snapshot path: %w", err)
		}
		sourceURL = "file://" + abs
	}
	extra := []string{"--restore", "source_url=" + sourceURL}

	t0 := time.Now()
	socketPath, proc, err := m.startProcess(ctx, vmID, extra)
	if err != nil {
		return "", err
	}
	if err := pollSocketReadyOrExit(ctx, socketPath, DefaultPollTimeout, proc.done); err != nil {
		m.killProcess(socketPath)
		return "", fmt.Errorf("wait for VMM socket after restore: %w", err)
	}
	client := NewClient(socketPath)
	if err := client.Resume(ctx); err != nil {
		m.killProcess(socketPath)
		return "", fmt.Errorf("resume restored vm: %w", err)
	}
	m.logger.Info("vm restored",
		"vm_id", vmID,
		"socket", socketPath,
		"snapshot", snapshotPath,
		"elapsed_ms", time.Since(t0).Milliseconds(),
	)
	return socketPath, nil
}

// SnapshotVM pauses the VM, snapshots it into destDir, and either resumes
// or leaves it paused based on resume. destDir must exist or be createable.
//
// If the snapshot fails we always attempt to Resume even when the caller's
// ctx is cancelled — otherwise the guest stays wedged in PAUSED forever.
// The recovery hop runs on a detached context with its own short timeout.
func (m *VMManager) SnapshotVM(ctx context.Context, socketPath, destDir string, resume bool) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}
	abs, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve snapshot dir: %w", err)
	}
	client := NewClient(socketPath)
	if err := client.Pause(ctx); err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}
	if err := client.Snapshot(ctx, SnapshotConfig{DestinationURL: "file://" + abs}); err != nil {
		m.bestEffortResume(ctx, client, socketPath, err)
		return fmt.Errorf("snapshot vm: %w", err)
	}
	if resume {
		if err := client.Resume(ctx); err != nil {
			return fmt.Errorf("resume after snapshot: %w", err)
		}
	}
	return nil
}

// bestEffortResume runs Resume on a detached context so a cancelled or
// expired caller ctx doesn't block recovery from a failed snapshot.
func (m *VMManager) bestEffortResume(ctx context.Context, client *Client, socketPath string, snapErr error) {
	recoverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultRequestTimeout)
	defer cancel()
	if err := client.Resume(recoverCtx); err != nil {
		m.logger.Error("snapshot failed AND recovery resume failed; vm stuck in PAUSED",
			"socket", socketPath,
			"snapshot_err", snapErr,
			"resume_err", err,
		)
	}
}

// DestroyVM gracefully shuts down the VMM behind socketPath, SIGKILLs the
// child if it doesn't exit within DefaultShutdownGrace (a single shared
// budget covering both the API call and the process exit), and removes
// all socket files associated with the VM. Safe against sockets whose
// process we don't track — it will still clean up.
func (m *VMManager) DestroyVM(ctx context.Context, socketPath string) error {
	deadline := time.Now().Add(DefaultShutdownGrace)
	apiCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	client := NewClient(socketPath)
	if err := client.VmmShutdown(apiCtx); err != nil {
		// Common case: VMM already gone or socket dead. Log and continue
		// to SIGKILL/cleanup; we don't propagate this as a failure.
		m.logger.Warn("vmm.shutdown failed; will SIGKILL if process is alive",
			"socket", socketPath, "err", err)
	}

	if procAny, ok := m.procs.LoadAndDelete(socketPath); ok {
		proc := procAny.(*managedProc)
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		if !proc.waitTimeout(remaining) {
			m.logger.Warn("cloud-hypervisor did not exit; sending SIGKILL",
				"pid", proc.cmd.Process.Pid)
			proc.kill()
		}
	}
	return cleanupSocketFiles(socketPath)
}

// startProcess spawns cloud-hypervisor with --api-socket plus extra args.
// It does not wait for readiness — callers should follow up with the
// crash-aware pollSocketReadyOrExit using proc.done.
func (m *VMManager) startProcess(ctx context.Context, vmID string, extraArgs []string) (string, *managedProc, error) {
	if err := os.MkdirAll(m.socketDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create socket dir: %w", err)
	}
	socketPath := filepath.Join(m.socketDir, vmID+".sock")
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("remove stale socket: %w", err)
	}
	args := []string{
		"--api-socket", "path=" + socketPath,
		"--console", "off",
		"--serial", "off",
	}
	args = append(args, extraArgs...)

	// exec.CommandContext is intentionally NOT used here: a cancelled ctx
	// would SIGKILL the child, which we'd rather do explicitly via
	// DestroyVM/killProcess so we control cleanup ordering.
	cmd := exec.Command(m.binaryPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start cloud-hypervisor: %w", err)
	}
	proc := newManagedProc(cmd)
	m.procs.Store(socketPath, proc)
	m.logger.Info("spawned cloud-hypervisor",
		"vm_id", vmID,
		"socket", socketPath,
		"pid", cmd.Process.Pid,
		"args", args,
	)
	return socketPath, proc, nil
}

// killProcess force-kills the child for socketPath (if tracked) and
// removes its socket files. Used as the unwind path on spawn failures.
func (m *VMManager) killProcess(socketPath string) {
	if procAny, ok := m.procs.LoadAndDelete(socketPath); ok {
		procAny.(*managedProc).kill()
	}
	_ = cleanupSocketFiles(socketPath)
}

// managedProc wraps an *exec.Cmd with a single, owned Wait() goroutine.
// Wait() is not safe to call concurrently — funnelling all observers
// through `done` keeps that invariant while still letting any number of
// callers wait or kill.
type managedProc struct {
	cmd  *exec.Cmd
	done chan struct{} // closed by the reaper after Wait() returns
}

// newManagedProc starts the reaper goroutine. The caller must have
// already invoked cmd.Start().
func newManagedProc(cmd *exec.Cmd) *managedProc {
	p := &managedProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		_ = cmd.Wait()
	}()
	return p
}

// waitTimeout returns true if the process exits within timeout.
func (p *managedProc) waitTimeout(timeout time.Duration) bool {
	if timeout <= 0 {
		select {
		case <-p.done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

// kill sends SIGKILL (no-op if already exited) and blocks until the
// reaper goroutine has finished — guaranteeing the single-Wait invariant.
func (p *managedProc) kill() {
	_ = p.cmd.Process.Kill()
	<-p.done
}

// cleanupSocketFiles removes the API socket plus common siblings (pid file,
// vsock companion). os.ErrNotExist is treated as success since sockets may
// already have been cleaned up by the VMM on exit.
func cleanupSocketFiles(socketPath string) error {
	base := strings.TrimSuffix(socketPath, filepath.Ext(socketPath))
	candidates := []string{
		socketPath,
		socketPath + ".pid",
		base + "-vsock.sock",
	}
	var firstErr error
	for _, p := range candidates {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			if firstErr == nil {
				firstErr = fmt.Errorf("remove %s: %w", p, err)
			}
		}
	}
	return firstErr
}

// PollSocketReady connects to socketPath every SocketPollInterval until a
// dial succeeds (the API server is accepting) or timeout elapses. This is
// the hot-path replacement for the "sleep 0.05" dance in the shell
// prototype and is what brings cold-start overhead under control.
//
// The function honours both ctx cancellation and timeout — whichever fires
// first wins. Safe to call against a path that does not yet exist.
func PollSocketReady(ctx context.Context, socketPath string, timeout time.Duration) error {
	return pollSocketReadyOrExit(ctx, socketPath, timeout, nil)
}

// pollSocketReadyOrExit is PollSocketReady plus an extra "exited" channel
// that, when closed, aborts the wait immediately. Used by SpawnVM/RestoreVM
// so a crashed cloud-hypervisor doesn't burn the full 5s timeout.
//
// Pass exited=nil to disable crash detection (the public PollSocketReady
// path). A nil channel select case never fires, so this is zero-cost.
func pollSocketReadyOrExit(ctx context.Context, socketPath string, timeout time.Duration, exited <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var lastErr error
	for {
		select {
		case <-exited:
			return fmt.Errorf("cloud-hypervisor exited before API socket %s was ready", socketPath)
		default:
		}
		conn, err := net.DialTimeout("unix", socketPath, SocketPollInterval)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err

		if pollCtx.Err() != nil {
			return fmt.Errorf("socket %s not ready after %s: %w",
				socketPath, timeout, lastErr)
		}
		timer := time.NewTimer(SocketPollInterval)
		select {
		case <-exited:
			timer.Stop()
			return fmt.Errorf("cloud-hypervisor exited before API socket %s was ready", socketPath)
		case <-pollCtx.Done():
			timer.Stop()
			return fmt.Errorf("socket %s not ready after %s: %w",
				socketPath, timeout, lastErr)
		case <-timer.C:
		}
	}
}
