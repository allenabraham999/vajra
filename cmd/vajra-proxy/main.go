// Package main is the entry point for vajra-proxy.
//
// vajra-proxy is the reverse proxy fronting sandboxes. It handles dynamic
// routing for sandbox port forwarding, browser terminals over WebSocket,
// and (optionally) TLS termination if VAJRA_PROXY_TLS_CERT/KEY are set.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/allenabraham999/vajra/internal/proxy"
)

// envOr reads env[name], returning fallback when unset/empty.
func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	listenAddr := envOr("VAJRA_PROXY_LISTEN", proxy.DefaultListenAddr)
	baseDomain := envOr("VAJRA_PROXY_BASE_DOMAIN", proxy.DefaultBaseDomain)
	masterURL := strings.TrimRight(envOr("VAJRA_MASTER_URL", "http://localhost:8080"), "/")
	internalSecret := os.Getenv("VAJRA_INTERNAL_SECRET")

	resolver := &proxy.HTTPResolver{
		BaseURL: masterURL,
		Token:   internalSecret,
	}
	shareValidator := &proxy.HTTPShareValidator{
		BaseURL: masterURL,
		Token:   internalSecret,
	}
	srv, err := proxy.NewServer(proxy.Config{
		ListenAddr: listenAddr,
		BaseDomain: baseDomain,
		Logger:     logger,
		Resolver:   resolver,
		Shares:     shareValidator,
	})
	if err != nil {
		logger.Error("vajra-proxy: bad config", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("vajra-proxy starting",
		"version", "0.1.0",
		"addr", listenAddr,
		"base_domain", baseDomain,
		"master", masterURL,
	)
	if err := srv.ListenAndServe(ctx); err != nil {
		logger.Error("vajra-proxy: serve", "err", err)
		os.Exit(1)
	}
	// Allow inflight log writes to drain.
	time.Sleep(50 * time.Millisecond)
}
