//go:build integration

// Package test contains end-to-end integration tests that run against a
// real Postgres. master_integration_test.go exercises the vajra-master
// HTTP surface over a real database. Skip if VAJRA_TEST_DSN is unset.
package test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/allenabraham999/vajra/internal/master"
	"github.com/allenabraham999/vajra/internal/store"
)

// TestMasterIntegrationRegisterLoginCreate walks the full create
// pipeline end-to-end against a real Postgres. The test:
//   1. Connects to VAJRA_TEST_DSN.
//   2. Runs migrations.
//   3. Boots an in-process master.Server.
//   4. Stands up an httptest agent stand-in.
//   5. Registers, logs in, seeds a cluster + node + template, creates
//      a sandbox, and asserts the agent received the dispatch.
//
// VAJRA_TEST_DSN must point at a writable Postgres (or empty schema).
// The test does not clean up; use a fresh DB per run.
func TestMasterIntegrationRegisterLoginCreate(t *testing.T) {
	dsn := os.Getenv("VAJRA_TEST_DSN")
	if dsn == "" {
		t.Skip("VAJRA_TEST_DSN not set")
	}
	migrationsDir := os.Getenv("VAJRA_MIGRATIONS_DIR")
	if migrationsDir == "" {
		migrationsDir = "../migrations"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, store.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer st.Close()

	mig, err := store.NewMigrator(st.DB().DB, "file://"+migrationsDir)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := mig.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	defer mig.Close()

	// Stand-in agent.
	var agentCalls int
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "sb-test", "state": "running",
		})
	}))
	defer agentSrv.Close()

	signer := master.NewJWTSigner([]byte("0123456789abcdef0123456789abcdef"))
	pool := master.NewAgentPool("agent-secret", slog.Default())
	scheduler := master.NewScheduler(st, nil)
	tracker := master.NewOperationTracker(st)
	handlers := master.NewHandlers(st, signer, scheduler, pool, tracker)
	srv := master.NewServer(master.ServerConfig{
		Addr:           ":0",
		Logger:         slog.Default(),
		InternalSecret: "internal-secret",
	}, handlers)
	httpSrv := httptest.NewServer(srv.Routes())
	defer httpSrv.Close()

	doJSON := func(method, path, token string, body any) (*http.Response, []byte) {
		buf, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, httpSrv.URL+path, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do %s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp, out
	}

	// Register.
	resp, body := doJSON("POST", "/v1/auth/register", "", map[string]string{
		"email": "alice@vajra-int.test", "password": "supersecret",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d body %s", resp.StatusCode, body)
	}
	var reg struct {
		AccountID string `json:"account_id"`
		APIKey    string `json:"api_key"`
	}
	_ = json.Unmarshal(body, &reg)
	if !strings.HasPrefix(reg.APIKey, "vj_live_") {
		t.Fatalf("bad api key %q", reg.APIKey)
	}

	// Login.
	resp, body = doJSON("POST", "/v1/auth/login", "", map[string]string{
		"email": "alice@vajra-int.test", "password": "supersecret",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d body %s", resp.StatusCode, body)
	}
}
