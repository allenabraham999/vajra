// Package main is the entry point for vajra-agent.
//
// vajra-agent runs on each bare metal host as a daemon, managing local
// Cloud Hypervisor microVMs. It receives commands from vajra-master and
// pushes state changes back via event-driven updates.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/allenabraham999/vajra/internal/agent"
	"github.com/allenabraham999/vajra/internal/vmm"
)

// agentVersion is the build-time version stamp. -ldflags overridable so
// release pipelines can inject a real semver / commit; defaults to "dev"
// for ad-hoc builds.
var agentVersion = "dev"

// config bundles every env-var the agent reads. Filled by loadConfig at
// startup so the rest of main is a straight-line wiring exercise.
type config struct {
	listenAddr   string
	nodeID       string
	clusterID    string
	masterURL    string
	apiKey       string
	cacheDir     string
	sandboxRoot  string
	archiveDir   string
	socketDir    string
	cacheMaxBytes int64
	poolMinSize  int
	poolTemplate string
	heartbeatInterval time.Duration
	healthInterval    time.Duration
	chBinary     string
	totalCPU     int
	totalMemMB   int
	totalDiskGB  int
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("invalid config", "err", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("vajra-agent starting",
		"version", "0.0.1",
		"node_id", cfg.nodeID,
		"listen", cfg.listenAddr,
	)

	if err := run(ctx, cfg, logger); err != nil {
		slog.Error("vajra-agent failed", "err", err)
		os.Exit(1)
	}
	slog.Info("vajra-agent shutting down cleanly")
}

func run(ctx context.Context, cfg config, logger *slog.Logger) error {
	vm := vmm.NewVMManager(logger).
		WithBinary(cfg.chBinary).
		WithSocketDir(cfg.socketDir)
	cache := agent.NewImageCache(cfg.cacheDir, cfg.cacheMaxBytes, logger)
	sandboxes := agent.NewSandboxManager(cfg.sandboxRoot, cfg.socketDir, cache, vm, nil, logger)
	archives := agent.NewArchiveManager(sandboxes, agent.ArchiveOptions{
		ArchiveDir: cfg.archiveDir,
	}, logger)

	var pool *agent.PoolManager
	if cfg.poolMinSize > 0 && cfg.poolTemplate != "" {
		pool = agent.NewPoolManager(
			cfg.poolMinSize,
			cfg.poolTemplate,
			agent.SandboxConfig{VCPUs: 2, MemoryMB: 512, DiskGB: 4},
			sandboxes,
			logger,
		)
		pool.Start(ctx)
		defer pool.Stop()
	}

	var master *agent.MasterClient
	if cfg.masterURL != "" {
		master = agent.NewMasterClient(cfg.masterURL, cfg.nodeID, cfg.apiKey, logger)
	}
	health := agent.NewHealthChecker(sandboxes, masterAsNotifier(master), cfg.healthInterval, logger)
	health.Start(ctx)
	defer health.Stop()

	if master != nil {
		if err := registerWithMaster(ctx, master, cfg); err != nil {
			logger.Warn("master registration failed; continuing", "err", err)
		}
		go heartbeatLoop(ctx, master, sandboxes, cfg, logger)
	}

	srv := agent.NewServer(cfg.listenAddr, sandboxes, pool, logger)
	srv.SetArchiveManager(archives)
	if err := srv.ListenAndServe(ctx); err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// masterAsNotifier adapts *MasterClient (or nil) to the MasterNotifier
// interface the health checker expects. Returning nil when master is
// unconfigured keeps the checker in log-only mode.
func masterAsNotifier(m *agent.MasterClient) agent.MasterNotifier {
	if m == nil {
		return nil
	}
	return m
}

func registerWithMaster(ctx context.Context, master *agent.MasterClient, cfg config) error {
	hostname, _ := os.Hostname()
	ip, err := primaryIP()
	if err != nil {
		return fmt.Errorf("detect ip: %w", err)
	}
	req := agent.RegisterRequest{
		NodeID:    cfg.nodeID,
		Hostname:  hostname,
		IP:        ip,
		ClusterID: cfg.clusterID,
	}
	req.Capacity.TotalCPU = cfg.totalCPU
	req.Capacity.TotalMemoryMB = cfg.totalMemMB
	req.Capacity.TotalDiskGB = cfg.totalDiskGB

	regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return master.Register(regCtx, req)
}

func heartbeatLoop(ctx context.Context, master *agent.MasterClient, sandboxes *agent.SandboxManager, cfg config, logger *slog.Logger) {
	// Send one heartbeat immediately so master can schedule onto this
	// node within milliseconds of agent startup rather than waiting up
	// to a full interval. Without this primer, a fresh agent looks
	// stale to the scheduler (last_heartbeat is whatever the DB held
	// from the previous run, often well past heartbeatStaleAfter).
	sendHeartbeat(ctx, master, sandboxes, cfg, logger)

	ticker := time.NewTicker(cfg.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sendHeartbeat(ctx, master, sandboxes, cfg, logger)
	}
}

func sendHeartbeat(ctx context.Context, master *agent.MasterClient, sandboxes *agent.SandboxManager, cfg config, logger *slog.Logger) {
	req := agent.HeartbeatRequest{
		NodeID:    cfg.nodeID,
		Timestamp: time.Now().UTC(),
	}
	used := computeUsage(sandboxes.List())
	req.Usage.UsedCPU = used.cpu
	req.Usage.UsedMemoryMB = used.memMB
	req.Usage.UsedDiskGB = used.diskGB
	req.SandboxCount = used.count
	req.Version = agentVersion

	hbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := master.Heartbeat(hbCtx, req); err != nil {
		logger.Warn("heartbeat failed", "err", err)
	}
}

type usageTotals struct {
	cpu, memMB, diskGB, count int
}

func computeUsage(sandboxes []*agent.Sandbox) usageTotals {
	var u usageTotals
	for _, sb := range sandboxes {
		// DESTROYED is terminal-clean; ERROR is terminal-failed. Neither
		// is using KVM/RAM/disk in a way that should block scheduling
		// new sandboxes onto the same node, so both are excluded from
		// heartbeat usage. (The dirs may linger until master issues a
		// destroy, but that's a cleanup concern, not a capacity one.)
		if sb.State == agent.SandboxStateDestroyed || sb.State == agent.SandboxStateError {
			continue
		}
		u.cpu += sb.Config.VCPUs
		u.memMB += sb.Config.MemoryMB
		u.diskGB += sb.Config.DiskGB
		u.count++
	}
	return u
}

// primaryIP returns the first non-loopback IPv4 address on the host. We
// prefer IPv4 because the Node model stores a single string and most
// internal traffic in the demo cluster is v4.
func primaryIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.IsLoopback() {
				continue
			}
			if v4 := ipNet.IP.To4(); v4 != nil {
				return v4.String(), nil
			}
		}
	}
	return "", errors.New("no non-loopback ipv4 address found")
}

func loadConfig() (config, error) {
	cfg := config{
		listenAddr:        envOr("VAJRA_AGENT_LISTEN_ADDR", agent.DefaultListenAddr),
		nodeID:            envOr("VAJRA_AGENT_NODE_ID", ""),
		clusterID:         envOr("VAJRA_AGENT_CLUSTER_ID", ""),
		masterURL:         os.Getenv("VAJRA_AGENT_MASTER_URL"),
		apiKey:            os.Getenv("VAJRA_AGENT_API_KEY"),
		cacheDir:          envOr("VAJRA_AGENT_CACHE_DIR", agent.DefaultCacheDir),
		sandboxRoot:       envOr("VAJRA_AGENT_SANDBOX_ROOT", agent.DefaultSandboxRoot),
		archiveDir:        envOr("VAJRA_AGENT_ARCHIVE_DIR", agent.DefaultArchiveDir),
		socketDir:         envOr("VAJRA_AGENT_SOCKET_DIR", vmm.DefaultSocketDir),
		chBinary:          envOr("VAJRA_AGENT_CH_BINARY", vmm.DefaultBinaryPath),
		poolTemplate:      os.Getenv("VAJRA_AGENT_POOL_TEMPLATE"),
		heartbeatInterval: envDuration("VAJRA_AGENT_HEARTBEAT_INTERVAL", 5*time.Second),
		healthInterval:    envDuration("VAJRA_AGENT_HEALTH_INTERVAL", agent.DefaultHealthInterval),
	}
	cfg.cacheMaxBytes = envInt64("VAJRA_AGENT_CACHE_MAX_BYTES", 50*1024*1024*1024)
	cfg.poolMinSize = envInt("VAJRA_AGENT_POOL_MIN_SIZE", 0)
	cfg.totalCPU = envInt("VAJRA_AGENT_TOTAL_CPU", 0)
	cfg.totalMemMB = envInt("VAJRA_AGENT_TOTAL_MEMORY_MB", 0)
	cfg.totalDiskGB = envInt("VAJRA_AGENT_TOTAL_DISK_GB", 0)

	if cfg.nodeID == "" {
		host, err := os.Hostname()
		if err != nil {
			return cfg, fmt.Errorf("derive node id from hostname: %w", err)
		}
		cfg.nodeID = "node-" + host
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
