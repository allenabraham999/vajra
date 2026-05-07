// Package main is the entry point for vajra-agent.
//
// vajra-agent runs on each bare metal host as a daemon, managing local
// Cloud Hypervisor microVMs. It receives commands from vajra-master and
// pushes state changes back via event-driven updates.
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

	slog.Info("vajra-agent starting", "version", "0.0.1")

	<-ctx.Done()
	slog.Info("vajra-agent shutting down")
}
