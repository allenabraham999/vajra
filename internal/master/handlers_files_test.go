package master

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
)

// seedRunningSandbox is a small helper for the file-handler tests:
// register an account, seed a cluster/node/template, and stamp a
// RUNNING sandbox onto the test node.
func (h *testHarness) seedRunningSandbox(t *testing.T) (accountID, key, sandboxID string) {
	t.Helper()
	accountID, key = h.register(t, "files@example.com", "supersecret")
	c := h.seedCluster(t)
	n := h.seedNode(t, c.ID)
	h.seedTemplate(t, accountID)
	sandboxID = "sb-files-1"
	clusterID := c.ID
	nodeID := n.ID
	now := time.Now().UTC()
	sb := &models.Sandbox{
		ID: sandboxID, Name: "demo", AccountID: accountID,
		ClusterID: &clusterID, NodeID: &nodeID,
		TemplateID: "tmpl-1",
		State:      models.SandboxStateRunning,
		Config:     models.SandboxConfig{VCPUs: 1, MemoryMB: 256, DiskGB: 1},
		CreatedAt:  now, UpdatedAt: now,
	}
	if err := h.store.Sandboxes().Create(context.Background(), sb); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
	return
}

func TestUploadFileHappy(t *testing.T) {
	h := newTestHarness(t)
	_, key, sandboxID := h.seedRunningSandbox(t)

	// Override the agent server's behaviour for upload.
	uploaded := make(chan []byte, 1)
	uploadedPath := make(chan string, 1)
	h.agentSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/files/upload") {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		uploadedPath <- r.Header.Get("X-Vajra-Path")
		body, _ := io.ReadAll(r.Body)
		uploaded <- body
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})

	body := []byte("hello world")
	req, _ := http.NewRequest("POST", h.httpSrv.URL+"/v1/sandboxes/"+sandboxID+"/files/upload", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Vajra-Path", "/tmp/hi.txt")
	req.Header.Set("X-Vajra-Mode", "420")
	req.ContentLength = int64(len(body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if got := <-uploadedPath; got != "/tmp/hi.txt" {
		t.Errorf("agent saw path %q", got)
	}
	if got := <-uploaded; !bytes.Equal(got, body) {
		t.Errorf("agent saw body %q, want %q", got, body)
	}
}

func TestUploadFileMissingPath(t *testing.T) {
	h := newTestHarness(t)
	_, key, sandboxID := h.seedRunningSandbox(t)

	body := []byte("hello")
	req, _ := http.NewRequest("POST", h.httpSrv.URL+"/v1/sandboxes/"+sandboxID+"/files/upload", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", resp.StatusCode)
	}
}

func TestDownloadFileHappy(t *testing.T) {
	h := newTestHarness(t)
	_, key, sandboxID := h.seedRunningSandbox(t)

	h.agentSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/files/download") {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		body := []byte("agent-bytes")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Vajra-Mode", "420")
		w.Header().Set("Content-Length", "11")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})

	req, _ := http.NewRequest("GET", h.httpSrv.URL+"/v1/sandboxes/"+sandboxID+"/files/download?path=/tmp/x", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "agent-bytes" {
		t.Errorf("body = %q", body)
	}
	if got := resp.Header.Get("X-Vajra-Mode"); got != "420" {
		t.Errorf("mode header = %q", got)
	}
}

func TestListFilesHappy(t *testing.T) {
	h := newTestHarness(t)
	_, key, sandboxID := h.seedRunningSandbox(t)

	h.agentSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/files/list") {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries": []map[string]any{
				{"name": "a", "size": 10, "mode": 420, "is_dir": false, "mod_time": "2026-05-08T00:00:00Z"},
			},
		})
	})

	req, _ := http.NewRequest("GET", h.httpSrv.URL+"/v1/sandboxes/"+sandboxID+"/files/list?dir=/tmp", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Entries) != 1 || out.Entries[0]["name"] != "a" {
		t.Fatalf("bad entries: %+v", out.Entries)
	}
}
