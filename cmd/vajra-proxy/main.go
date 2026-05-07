// Package main is the entry point for vajra-proxy.
//
// vajra-proxy is the reverse proxy fronting sandboxes. It handles dynamic
// routing for sandbox port forwarding, browser terminals over WebSocket,
// and TLS termination.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("vajra-proxy starting", "version", "0.0.1")

	<-ctx.Done()
	slog.Info("vajra-proxy shutting down")
}
