package master

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

// writeFakeScript drops an executable stand-in build script into a temp
// dir so ScriptBuildRunner tests never touch real Docker/qemu/CH.
func writeFakeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-build.sh")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake script: %v", err)
	}
	return p
}

// TestScriptBuildRunnerReportsScriptFailure: a non-zero script exit must
// surface as an error that carries the script's own output.
func TestScriptBuildRunnerReportsScriptFailure(t *testing.T) {
	script := writeFakeScript(t, "#!/bin/bash\necho 'boom: base rootfs missing' >&2\nexit 3\n")
	r := NewScriptBuildRunner(script, "sha256:base", nil)

	_, err := r.Run(context.Background(), "apt-get install -y jq", "tpl", "1.0.0")
	if err == nil {
		t.Fatal("expected an error from a failing build script")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should surface the script output, got: %v", err)
	}
}

// TestScriptBuildRunnerCleansUpOnFailure: even when the build fails, the
// temp file holding the caller's setup script must be removed.
func TestScriptBuildRunnerCleansUpOnFailure(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "setup-path")
	t.Setenv("VAJRA_TEST_SETUP_MARKER", marker)
	// The fake script records the setup-file path it was handed, then fails.
	script := writeFakeScript(t,
		"#!/bin/bash\nprintf '%s' \"$2\" > \"$VAJRA_TEST_SETUP_MARKER\"\nexit 1\n")
	r := NewScriptBuildRunner(script, "sha256:base", nil)

	if _, err := r.Run(context.Background(), "echo hi", "tpl", "1.0.0"); err == nil {
		t.Fatal("expected an error from a failing build script")
	}
	recorded, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("fake script did not record the setup path: %v", err)
	}
	if _, err := os.Stat(string(recorded)); !os.IsNotExist(err) {
		t.Fatalf("temp setup file %q should have been removed; stat err = %v", recorded, err)
	}
}

// TestScriptBuildRunnerRespectsTimeout: a build that overruns its context
// must be cancelled rather than run forever.
func TestScriptBuildRunnerRespectsTimeout(t *testing.T) {
	script := writeFakeScript(t, "#!/bin/bash\nsleep 30\n")
	r := NewScriptBuildRunner(script, "sha256:base", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := r.Run(ctx, "echo hi", "tpl", "1.0.0")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Run did not honor the context timeout; took %s", elapsed)
	}
}

// TestScriptBuildRunnerParsesArtifact: the happy path — the runner parses
// the KEY=VALUE lines the build script prints into a BuildArtifact.
func TestScriptBuildRunnerParsesArtifact(t *testing.T) {
	script := writeFakeScript(t, "#!/bin/bash\n"+
		"echo PHASE:COPYING\n"+
		"echo NEW_TEMPLATE_HASH=sha256:abc123\n"+
		"echo ROOTFS_PATH=/var/lib/vajra/cache/sha256:abc123/rootfs.qcow2\n"+
		"echo KERNEL_PATH=/var/lib/vajra/cache/sha256:abc123/vmlinux\n"+
		"echo SNAPSHOT_PATH=/var/lib/vajra/cache/sha256:abc123/snapshot\n"+
		"echo PHASE:DONE\n")
	r := NewScriptBuildRunner(script, "sha256:base", nil)

	art, err := r.Run(context.Background(), "echo hi", "tpl", "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if art.Hash != "sha256:abc123" {
		t.Fatalf("hash = %q, want sha256:abc123", art.Hash)
	}
	if !strings.HasSuffix(art.RootfsPath, "/rootfs.qcow2") || art.SnapshotPath == "" {
		t.Fatalf("artifact paths not parsed: %+v", art)
	}
}

// TestSnapshotPromotionUsesRequestNameVersion: promoting a snapshot must
// honour the name/version in the request body (the bug fix), and reject a
// missing name.
func TestSnapshotPromotionUsesRequestNameVersion(t *testing.T) {
	h := newTestHarness(t)
	accountID, key := h.register(t, "alice@example.com", "supersecret")

	snap := &models.Snapshot{
		ID: "snap-1", SandboxID: "sb-1", AccountID: accountID, NodeID: "node-1",
		StoragePath: "/var/lib/vajra/snapshots/snap-1",
		CreatedAt:   time.Now().UTC(),
	}
	if err := h.store.Snapshots().Create(context.Background(), snap); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	resp, body := h.req(t, "POST", "/v1/snapshots/snap-1/promote", key, map[string]any{
		"name": "my-promoted-env", "version": "3.2.1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("promote: want 201, got %d body %s", resp.StatusCode, body)
	}
	var tmpl models.Template
	if err := json.Unmarshal(body, &tmpl); err != nil {
		t.Fatalf("decode template: %v", err)
	}
	if tmpl.Name != "my-promoted-env" || tmpl.Version != "3.2.1" {
		t.Fatalf("promotion ignored request name/version: name=%q version=%q", tmpl.Name, tmpl.Version)
	}

	// A blank name is rejected rather than silently defaulted.
	bad, _ := h.req(t, "POST", "/v1/snapshots/snap-1/promote", key, map[string]any{
		"version": "1.0.0",
	})
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("promote without name: want 400, got %d", bad.StatusCode)
	}
}

// TestExistingTemplatesUnaffected: the classic register + list template
// path is untouched by the build-runner rework.
func TestExistingTemplatesUnaffected(t *testing.T) {
	h := newTestHarness(t)
	_, key := h.register(t, "alice@example.com", "supersecret")

	resp, body := h.req(t, "POST", "/v1/templates", key, map[string]any{
		"name": "ubuntu-noble", "version": "1.0.0", "hash": "sha256:basehash",
		"rootfs_path":   "/var/lib/vajra/cache/sha256:basehash/rootfs.qcow2",
		"kernel_path":   "/var/lib/vajra/cache/sha256:basehash/vmlinux",
		"snapshot_path": "/var/lib/vajra/cache/sha256:basehash/snapshot",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register template: want 201, got %d body %s", resp.StatusCode, body)
	}

	listResp, listBody := h.req(t, "GET", "/v1/templates", key, nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list templates: want 200, got %d", listResp.StatusCode)
	}
	var tmpls []models.Template
	if err := json.Unmarshal(listBody, &tmpls); err != nil {
		t.Fatalf("decode templates: %v", err)
	}
	for _, tm := range tmpls {
		if tm.Name == "ubuntu-noble" && tm.Hash == "sha256:basehash" {
			return
		}
	}
	t.Fatalf("registered template missing from listing: %s", listBody)
}
