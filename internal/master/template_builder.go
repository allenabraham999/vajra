// Package master — template_builder.go: the production BuildRunner.
//
// It shells out to scripts/build-custom-template.sh, which derives a new
// template from the known-good base image: copy the base rootfs, run the
// caller's setup script inside it under chroot, boot it, snapshot it, and
// lay the (rootfs, kernel, snapshot) triple out under the agent image
// cache. See the script header for the full pipeline.
//
// The agent only ever *restores* templates from a Cloud Hypervisor
// snapshot (internal/agent/sandbox.go) — there is no cold-boot path. So a
// usable template must be that full triple, which is why the build boots
// and snapshots a real VM rather than just producing a rootfs.
package master

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// buildScratchDir is where build-custom-template.sh stages its work. Only
// this directory is swept by the janitor — the content-addressable image
// cache is owned by the agent's LRU and is never touched here.
const buildScratchDir = "/var/lib/vajra/build-custom"

// ScriptBuildRunner is the production BuildRunner. Builds are serialised:
// the build script claims a fixed nbd device, so at most one build may run
// on a node at a time.
type ScriptBuildRunner struct {
	scriptPath string
	baseHash   string
	logger     logger
	mu         sync.Mutex
}

// NewScriptBuildRunner wires a ScriptBuildRunner. scriptPath is the path to
// build-custom-template.sh; baseHash is the template the build derives from.
func NewScriptBuildRunner(scriptPath, baseHash string, lg logger) *ScriptBuildRunner {
	if abs, err := filepath.Abs(scriptPath); err == nil {
		scriptPath = abs
	}
	return &ScriptBuildRunner{scriptPath: scriptPath, baseHash: baseHash, logger: lg}
}

// Run executes the build script with the caller's setup commands and
// returns the artifact it laid out. setup is the shell script the caller
// wants run inside the rootfs; the version argument is unused (the script
// derives identity from the rootfs hash). Builds are serialised — see mu.
func (r *ScriptBuildRunner) Run(ctx context.Context, setup, name, _ string) (*BuildArtifact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if strings.TrimSpace(setup) == "" {
		return nil, fmt.Errorf("setup script is empty")
	}
	if r.baseHash == "" {
		return nil, fmt.Errorf("no base template configured (set VAJRA_TEMPLATE_BASE_HASH)")
	}
	if _, err := os.Stat(r.scriptPath); err != nil {
		return nil, fmt.Errorf("build script unavailable: %w", err)
	}

	// Stage the caller's setup script to a temp file the build script
	// installs into the rootfs and runs under chroot. Always removed on
	// return — the master-side "cleans up on failure" guarantee.
	setupFile, err := writeTempSetup(setup)
	if err != nil {
		return nil, err
	}
	defer os.Remove(setupFile)

	buildID, err := randomHex(8)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "bash", r.scriptPath, name, setupFile)
	cmd.Env = append(os.Environ(), "SRC_HASH="+r.baseHash, "BUILD_ID="+buildID)
	// Run the script in its own process group so a cancelled build is torn
	// down as a unit — otherwise an orphaned build VM (a grandchild) keeps
	// the output pipe open and Wait blocks until it exits on its own.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// On ctx cancel/timeout, SIGTERM the whole group: the script's cleanup
	// trap tears down the build VM and mounts. SIGKILL only after WaitDelay.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 45 * time.Second

	out, runErr := r.runStreaming(cmd, buildID)
	if runErr != nil {
		return nil, fmt.Errorf("template build failed: %w\n%s", runErr, lastLines(out, 25))
	}
	art := parseBuildOutput(out)
	if art.Hash == "" || art.RootfsPath == "" || art.SnapshotPath == "" {
		return nil, fmt.Errorf("build script produced no template artifact\n%s", lastLines(out, 25))
	}
	r.log("template build completed", "build_id", buildID, "hash", art.Hash)
	return art, nil
}

// runStreaming runs cmd, scanning its combined output line by line so each
// PHASE marker is logged as the build progresses, and returns the full
// captured output.
func (r *ScriptBuildRunner) runStreaming(cmd *exec.Cmd, buildID string) (string, error) {
	pr, pw := io.Pipe()
	// Stdout and Stderr share one writer, so os/exec hands the child a
	// single fd: combined output, no interleaving mid-line.
	cmd.Stdout = pw
	cmd.Stderr = pw

	var buf strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			buf.WriteString(line)
			buf.WriteByte('\n')
			if phase, ok := strings.CutPrefix(line, "PHASE:"); ok {
				r.log("template build phase", "build_id", buildID, "phase", phase)
			}
		}
	}()

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		<-done
		return buf.String(), err
	}
	err := cmd.Wait()
	_ = pw.Close()
	<-done
	return buf.String(), err
}

// writeTempSetup writes the caller's setup script to a temp file.
func writeTempSetup(setup string) (string, error) {
	f, err := os.CreateTemp("", "vajra-setup-*.sh")
	if err != nil {
		return "", fmt.Errorf("stage setup script: %w", err)
	}
	if _, err := f.WriteString(setup); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write setup script: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// parseBuildOutput extracts the KEY=VALUE artifact lines the build script
// prints on success.
func parseBuildOutput(out string) *BuildArtifact {
	a := &BuildArtifact{}
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "NEW_TEMPLATE_HASH":
			a.Hash = val
		case "ROOTFS_PATH":
			a.RootfsPath = val
		case "KERNEL_PATH":
			a.KernelPath = val
		case "SNAPSHOT_PATH":
			a.SnapshotPath = val
		}
	}
	return a
}

// lastLines returns the trailing n lines of s — used to surface the tail
// of a failed build's log without dumping the whole thing.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// log dispatches to the configured logger when present.
func (r *ScriptBuildRunner) log(msg string, args ...any) {
	if r.logger != nil {
		r.logger.Info(msg, args...)
	}
}

// StartBuildDirJanitor periodically removes stale scratch directories left
// by interrupted template builds. It only ever touches buildScratchDir —
// never the image cache.
func StartBuildDirJanitor(ctx context.Context, lg logger) {
	go func() {
		sweepStaleBuildDirs(lg)
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sweepStaleBuildDirs(lg)
			}
		}
	}()
}

// sweepStaleBuildDirs removes build scratch dirs older than two hours. A
// build in progress always has a fresh dir, so the age cutoff never races
// a live build.
func sweepStaleBuildDirs(lg logger) {
	entries, err := os.ReadDir(buildScratchDir)
	if err != nil {
		return // absent until the first build — nothing to sweep
	}
	cutoff := time.Now().Add(-2 * time.Hour)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		dir := filepath.Join(buildScratchDir, e.Name())
		// A hard-killed build can leave root-owned files behind, so sudo.
		if err := exec.Command("sudo", "-n", "rm", "-rf", dir).Run(); err != nil {
			if lg != nil {
				lg.Warn("build janitor: could not remove stale dir", "dir", dir, "err", err)
			}
			continue
		}
		if lg != nil {
			lg.Info("build janitor: removed stale build dir", "dir", dir)
		}
	}
}
