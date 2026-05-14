// Package main is the entry point for vajra-master.
//
// vajra-master is the stateless control plane API server. All state lives
// in PostgreSQL so multiple replicas can run behind a load balancer.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/allenabraham999/vajra/internal/cache"
	"github.com/allenabraham999/vajra/internal/events"
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
	PublicBaseDomain  string
	RateLimitRPS      int
	Version           master.VersionInfo
	RedisURL          string
	NATSURL           string
	Autoscale         master.AutoscaleConfig
	GoogleOAuth       master.GoogleOAuthConfig
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
		PublicBaseDomain:  os.Getenv("PUBLIC_BASE_DOMAIN"),
		Version: master.BuildInfo(),
	}
	// Allow env-var overrides on top of the ldflags-stamped values so
	// dev builds can spoof the version triple without rebuilding.
	if v := os.Getenv("VAJRA_VERSION"); v != "" {
		cfg.Version.Version = v
	}
	if v := os.Getenv("VAJRA_COMMIT"); v != "" {
		cfg.Version.Commit = v
	}
	if v := os.Getenv("VAJRA_BUILT_AT"); v != "" {
		cfg.Version.BuiltAt = v
	}
	if v := os.Getenv("VAJRA_RATE_LIMIT_RPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("VAJRA_RATE_LIMIT_RPS %q: %w", v, err)
		}
		cfg.RateLimitRPS = n
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

	cfg.RedisURL = os.Getenv("REDIS_URL")
	cfg.NATSURL = os.Getenv("NATS_URL")

	// Google OAuth is fully optional. ClientID + ClientSecret + RedirectURL
	// must all be present for the /v1/auth/google* endpoints to do
	// anything; DashboardURL is where the callback bounces the browser
	// after a successful exchange (defaults to "/" if unset).
	cfg.GoogleOAuth = master.GoogleOAuthConfig{
		ClientID:     os.Getenv("VAJRA_GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("VAJRA_GOOGLE_CLIENT_SECRET"),
		RedirectURL:  os.Getenv("VAJRA_GOOGLE_REDIRECT_URL"),
		DashboardURL: os.Getenv("VAJRA_DASHBOARD_URL"),
	}
	// Autoscaler env vars come in short and long flavours so operators
	// can pick whichever they prefer. Short names (the ones in the
	// brief) win when set; the longer forms are kept so existing .envs
	// keep working. InstanceType defaults to empty: that lets the
	// autoscaler size each new node from the request ladder
	// (instanceTypeForResources). Set it explicitly only to pin a type.
	cfg.Autoscale = master.AutoscaleConfig{
		Enabled:       os.Getenv("VAJRA_AUTOSCALE_ENABLED") == "true",
		AMI:           os.Getenv("VAJRA_AUTOSCALE_AMI"),
		InstanceType:  os.Getenv("VAJRA_AUTOSCALE_INSTANCE_TYPE"),
		Region:        getenvDefault("VAJRA_AUTOSCALE_REGION", "us-east-1"),
		SecurityGroup: firstNonEmpty("VAJRA_AUTOSCALE_SG", "VAJRA_AUTOSCALE_SECURITY_GROUP"),
		KeyPair:       firstNonEmpty("VAJRA_AUTOSCALE_KEY", "VAJRA_AUTOSCALE_KEY_PAIR"),
		SubnetID:      firstNonEmpty("VAJRA_AUTOSCALE_SUBNET", "VAJRA_AUTOSCALE_SUBNET_ID"),
		MasterURL:     os.Getenv("VAJRA_AUTOSCALE_MASTER_URL"),
		AgentSecret:   cfg.AgentSharedSecret,
		ClusterID:     getenvDefault("VAJRA_AUTOSCALE_CLUSTER_ID", "cluster-1"),
		S3Bucket:      os.Getenv("VAJRA_AUTOSCALE_S3_BUCKET"),
	}
	if v := os.Getenv("VAJRA_AUTOSCALE_MIN_NODES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Autoscale.MinNodes = n
		}
	}
	if v := os.Getenv("VAJRA_AUTOSCALE_MAX_NODES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Autoscale.MaxNodes = n
		}
	}
	if v := firstNonEmpty("VAJRA_AUTOSCALE_COOLDOWN", "VAJRA_AUTOSCALE_COOLDOWN_MINS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Autoscale.CooldownMins = n
		}
	}
	if v := os.Getenv("VAJRA_AUTOSCALE_ROOT_VOLUME_GB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Autoscale.RootVolumeGB = n
		}
	}
	return cfg, nil
}

// firstNonEmpty returns the value of the first env var in keys whose
// value is non-empty, or "" when all are unset. Used to support short
// (brief-style) and long (legacy) names for the same setting.
func firstNonEmpty(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
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

	// Migrations run on a dedicated *sql.DB that's closed before the
	// main store pool is opened. Sharing the pool with the migrator
	// means mig.Close() (which closes the underlying driver) tears
	// down the connection the server is about to use.
	if err := runMigrations(cfg.DSN, cfg.MigrationsDir, logger); err != nil {
		logger.Error("migrations", "err", err)
		os.Exit(1)
	}

	st, err := store.New(ctx, store.DefaultConfig(cfg.DSN))
	if err != nil {
		logger.Error("store: connect", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// Optional Redis cache. Empty REDIS_URL → NoopCache; everything
	// short-circuits to a miss and callers fall through to Postgres.
	var c cache.Cache = cache.NewNoopCache()
	if cfg.RedisURL != "" {
		rc, err := cache.NewRedisCache(cfg.RedisURL)
		if err != nil {
			logger.Warn("redis connect failed; using noop cache", "err", err)
		} else {
			c = rc
			defer rc.Close()
			logger.Info("redis cache enabled", "url", cfg.RedisURL)
		}
	}

	// Optional NATS event bus. Empty NATS_URL → NoopBus.
	var bus events.EventBus = events.NewNoopBus()
	if cfg.NATSURL != "" {
		nb, err := events.NewNATSBus(cfg.NATSURL, logger)
		if err != nil {
			logger.Warn("nats connect failed; using noop bus", "err", err)
		} else {
			bus = nb
			defer nb.Close()
			logger.Info("nats event bus enabled", "url", cfg.NATSURL)
		}
	}

	signer := master.NewJWTSigner([]byte(cfg.JWTSecret))
	pool := master.NewAgentPool(cfg.AgentSharedSecret, logger)
	scheduler := master.NewScheduler(st, nil).WithCache(c).WithLogger(logger)
	tracker := master.NewOperationTracker(st)
	handlers := master.NewHandlers(st, signer, scheduler, pool, tracker)
	handlers.Logger = logger
	handlers.Version = cfg.Version
	handlers.AgentSharedSecret = cfg.AgentSharedSecret
	handlers.PublicBaseDomain = cfg.PublicBaseDomain
	handlers.BinaryDir = os.Getenv("VAJRA_BINARY_DIR")
	handlers.Cache = c
	handlers.Bus = bus
	handlers.GoogleOAuth = cfg.GoogleOAuth
	if cfg.GoogleOAuth.Enabled() {
		logger.Info("google oauth enabled", "redirect_url", cfg.GoogleOAuth.RedirectURL)
	}

	// Optional autoscaler. Disabled by default — handler check
	// (Autoscaler != nil && Config.Enabled) keeps existing 503 path
	// when VAJRA_AUTOSCALE_ENABLED is unset.
	if cfg.Autoscale.Enabled {
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Autoscale.Region))
		if err != nil {
			logger.Warn("aws config failed; autoscaler disabled", "err", err)
		} else {
			ec2c := ec2.NewFromConfig(awsCfg)
			scaler := master.NewAutoscaler(cfg.Autoscale, ec2c, st, c, scheduler, logger)
			handlers.Autoscaler = scaler
			go scaler.RunScaleDown(ctx)
			logger.Info("autoscaler enabled", "min", scaler.Config.MinNodes, "max", scaler.Config.MaxNodes)
		}
	}

	// NATS subscriber: drive Redis from agent heartbeats, batch DB
	// writes. Only meaningful when NATS_URL is set; with NoopBus the
	// Subscribe calls succeed silently but never receive anything.
	subscriber := master.NewSubscriber(bus, st, c, logger)
	if err := subscriber.Subscribe(); err != nil {
		logger.Warn("nats subscribe failed", "err", err)
	}
	go subscriber.Run(ctx)

	reconciler := master.NewReconciler(st, pool.AsAgentLister(), logger, cfg.ReconcileInterval)
	go reconciler.Run(ctx)

	// Builder, webhooks, and lifecycle sweeps. All three are optional
	// — they default to no-op behaviour when their backing store rows
	// are empty.
	handlers.Builder = master.NewBuildManager(st, nil, logger)
	handlers.Webhooks = master.NewWebhookManager(st, logger)
	handlers.Lifecycle = master.NewLifecycleManager(st, pool, c, handlers, logger)
	go handlers.Lifecycle.Run(ctx)

	srv := master.NewServer(master.ServerConfig{
		Addr:           cfg.ListenAddr,
		Logger:         logger,
		InternalSecret: cfg.AgentSharedSecret,
		AdminAccountID: cfg.AdminAccountID,
		RateLimitRPS:   cfg.RateLimitRPS,
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

// runMigrations applies schema migrations on startup. It opens its own
// dedicated *sql.DB from the DSN, runs migrations, and closes that
// connection — the main store pool must NOT be shared with the
// migrator because mig.Close() closes the underlying database driver,
// which would tear down the server's pool. The dir is a filesystem
// path; we prepend the file:// scheme if the caller didn't already.
func runMigrations(dsn, dir string, logger *slog.Logger) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open migrator db: %w", err)
	}
	defer db.Close()

	source := dir
	if !strings.Contains(source, "://") {
		source = "file://" + source
	}
	mig, err := store.NewMigrator(db, source)
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
