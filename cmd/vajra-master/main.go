// Package main is the entry point for vajra-master.
//
// vajra-master is the stateless control plane API server. All state lives
// in PostgreSQL so multiple replicas can run behind a load balancer.
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

	slog.Info("vajra-master starting", "version", "0.0.1")

	<-ctx.Done()
	slog.Info("vajra-master shutting down")
}
