// Package main is the entry point for vajra-master.
//
// vajra-master is the stateless control plane API server. All state lives
// in PostgreSQL so multiple replicas can run behind a load balancer.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/allenabraham999/vajra/internal/master"
	"github.com/allenabraham999/vajra/internal/store"
)

// minJWTSecretLen guards against accidentally running with a trivial
// HMAC secret. 32 bytes ≈ 256 bits of entropy, the conventional floor.
const minJWTSecretLen = 32

// config bundles every env-var-derived knob master needs.
type config struct {
	DSN               string
	JWTSecret         string
	AgentSharedSecret string
	ListenAddr        string
	MigrationsDir     string
	ReconcileInterval time.Duration
	AdminAccountID    string
	Version           master.VersionInfo
}

// loadConfig reads the runtime config from process env. Required vars
// missing or trivially short cause a clean exit so the operator sees
// the problem immediately.
func loadConfig() (*config, error) {
	cfg := &config{
		DSN:               os.Getenv("DATABASE_URL"),
		JWTSecret:         os.Getenv("JWT_SECRET"),
		AgentSharedSecret: os.Getenv("AGENT_SHARED_SECRET"),
		ListenAddr:        getenvDefault("LISTEN_ADDR", ":8080"),
		MigrationsDir:     getenvDefault("MIGRATIONS_DIR", "./migrations"),
		AdminAccountID:    os.Getenv("ADMIN_ACCOUNT_ID"),
		Version: master.VersionInfo{
			Version: getenvDefault("VAJRA_VERSION", "dev"),
			Commit:  getenvDefault("VAJRA_COMMIT", "unknown"),
			BuiltAt: getenvDefault("VAJRA_BUILT_AT", ""),
		},
	}
	interval := getenvDefault("RECONCILE_INTERVAL", "60s")
	d, err := time.ParseDuration(interval)
	if err != nil {
		return nil, fmt.Errorf("RECONCILE_INTERVAL %q: %w", interval, err)
	}
	cfg.ReconcileInterval = d

	if cfg.DSN == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if len(cfg.JWTSecret) < minJWTSecretLen {
		return nil, fmt.Errorf("JWT_SECRET must be at least %d bytes", minJWTSecretLen)
	}
	if cfg.AgentSharedSecret == "" {
		return nil, errors.New("AGENT_SHARED_SECRET is required")
	}
	return cfg, nil
}

// getenvDefault returns os.Getenv(key) or fallback when unset.
func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	st, err := store.New(ctx, store.DefaultConfig(cfg.DSN))
	if err != nil {
		logger.Error("store: connect", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	if err := runMigrations(st, cfg.MigrationsDir, logger); err != nil {
		logger.Error("migrations", "err", err)
		os.Exit(1)
	}

	signer := master.NewJWTSigner([]byte(cfg.JWTSecret))
	pool := master.NewAgentPool(cfg.AgentSharedSecret, logger)
	scheduler := master.NewScheduler(st, nil)
	tracker := master.NewOperationTracker(st)
	handlers := master.NewHandlers(st, signer, scheduler, pool, tracker)
	handlers.Logger = logger
	handlers.Version = cfg.Version

	reconciler := master.NewReconciler(st, pool.AsAgentLister(), logger, cfg.ReconcileInterval)
	go reconciler.Run(ctx)

	srv := master.NewServer(master.ServerConfig{
		Addr:           cfg.ListenAddr,
		Logger:         logger,
		InternalSecret: cfg.AgentSharedSecret,
		AdminAccountID: cfg.AdminAccountID,
	}, handlers)

	logger.Info("vajra-master starting",
		"version", cfg.Version.Version,
		"commit", cfg.Version.Commit,
		"addr", cfg.ListenAddr,
	)
	if err := srv.ListenAndServe(ctx); err != nil {
		logger.Error("server", "err", err)
		os.Exit(1)
	}
	logger.Info("vajra-master shutdown complete")
}

// runMigrations applies schema migrations on startup. The dir is a
// filesystem path; we prepend the file:// scheme if the caller didn't
// already.
func runMigrations(st *store.Postgres, dir string, logger *slog.Logger) error {
	source := dir
	if !strings.Contains(source, "://") {
		source = "file://" + source
	}
	mig, err := store.NewMigrator(st.DB().DB, source)
	if err != nil {
		return fmt.Errorf("open migrator: %w", err)
	}
	defer mig.Close()
	if err := mig.Up(); err != nil {
		return fmt.Errorf("up: %w", err)
	}
	v, dirty, err := mig.Version()
	if err != nil {
		return err
	}
	logger.Info("migrations applied", "version", v, "dirty", dirty)
	return nil
}
