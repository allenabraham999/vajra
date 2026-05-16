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
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/allenabraham999/vajra/internal/agent"
	"github.com/allenabraham999/vajra/internal/events"
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
	poolMaxSize  int
	poolTemplate string
	heartbeatInterval time.Duration
	healthInterval    time.Duration
	chBinary     string
	totalCPU     int
	totalMemMB   int
	totalDiskGB  int
	natsURL      string
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
		"capacity_cpu", cfg.totalCPU,
		"capacity_mem_mb", cfg.totalMemMB,
		"capacity_disk_gb", cfg.totalDiskGB,
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
	// Synchronously build the qcow2 backing for the configured pool
	// template so the first sandbox out of the pool doesn't pay the 3-6s
	// raw→qcow2 conversion tax. Best-effort: if the template isn't on
	// disk yet we just warn and let the async prewarm pick it up later.
	if cfg.poolTemplate != "" {
		if err := cache.EnsureRootfsBacking(cfg.poolTemplate); err != nil {
			logger.Warn("prewarm pool template", "template", cfg.poolTemplate, "err", err)
		}
	}
	// Pre-warm rootfs.qcow2 backing for every template already on disk so
	// the first sandbox doesn't pay the raw→qcow2 conversion tax (3-6s on
	// a ~10G rootfs). Async so HTTP serving starts immediately.
	go prewarmCache(cache, cfg.cacheDir, logger)
	sandboxes := agent.NewSandboxManager(cfg.sandboxRoot, cfg.socketDir, cache, vm, nil, logger)
	archives := agent.NewArchiveManager(sandboxes, agent.ArchiveOptions{
		ArchiveDir: cfg.archiveDir,
	}, logger)

	var pool *agent.PoolManager
	if cfg.poolTemplate != "" {
		pool = agent.NewPoolManager(
			cfg.poolMinSize,
			cfg.poolMaxSize,
			cfg.poolTemplate,
			agent.SandboxConfig{VCPUs: 2, MemoryMB: 512, DiskGB: 4},
			sandboxes,
			logger,
		)
		// WarmUp runs in the background — the agent serves HTTP
		// immediately so cold creates work while the pool fills.
		pool.Start(ctx)
		defer pool.Shutdown()
	}

	var master *agent.MasterClient
	if cfg.masterURL != "" {
		master = agent.NewMasterClient(cfg.masterURL, cfg.nodeID, cfg.apiKey, logger)
	}

	// Optional NATS bus. When NATS_URL is unset we wire NoopBus and the
	// HTTP heartbeat path below stays in charge — full backward compat.
	var bus events.EventBus = events.NewNoopBus()
	if cfg.natsURL != "" {
		nb, err := events.NewNATSBus(cfg.natsURL, logger)
		if err != nil {
			logger.Warn("nats connect failed; falling back to http only", "err", err)
		} else {
			bus = nb
			defer nb.Close()
		}
	}
	publisher := agent.NewPublisher(bus, cfg.nodeID, logger)

	health := agent.NewHealthChecker(sandboxes, masterAsNotifier(master), cfg.healthInterval, logger)
	health.Start(ctx)
	defer health.Stop()

	if master != nil {
		if err := registerWithMaster(ctx, master, cfg); err != nil {
			logger.Warn("master registration failed; continuing", "err", err)
		}
		go heartbeatLoop(ctx, master, publisher, sandboxes, cfg, logger)
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

func heartbeatLoop(ctx context.Context, master *agent.MasterClient, publisher *agent.Publisher, sandboxes *agent.SandboxManager, cfg config, logger *slog.Logger) {
	// Send one heartbeat immediately so master can schedule onto this
	// node within milliseconds of agent startup rather than waiting up
	// to a full interval. Without this primer, a fresh agent looks
	// stale to the scheduler (last_heartbeat is whatever the DB held
	// from the previous run, often well past heartbeatStaleAfter).
	sendHeartbeat(ctx, master, publisher, sandboxes, cfg, logger)

	ticker := time.NewTicker(cfg.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sendHeartbeat(ctx, master, publisher, sandboxes, cfg, logger)
	}
}

func sendHeartbeat(ctx context.Context, master *agent.MasterClient, publisher *agent.Publisher, sandboxes *agent.SandboxManager, cfg config, logger *slog.Logger) {
	used := computeUsage(sandboxes.List())

	// Always publish to NATS when wired — the master subscriber
	// updates Redis immediately and batches the DB write. This is the
	// primary control-plane signal when the bus is configured.
	publisher.PublishHeartbeat(ctx, agent.NodeUsageSnapshot{
		UsedCPU:      used.cpu,
		UsedMemoryMB: used.memMB,
		UsedDiskGB:   used.diskGB,
		SandboxCount: used.count,
	}, agentVersion)

	// Keep the HTTP heartbeat too. NATS isn't a guaranteed delivery
	// channel for vital health signals; the HTTP write to Postgres is
	// the canonical truth. When NATS is disabled this is the only
	// path. (Future optimisation: drop HTTP heartbeats entirely once
	// the subscriber is proven in production.)
	req := agent.HeartbeatRequest{
		NodeID:    cfg.nodeID,
		Timestamp: time.Now().UTC(),
	}
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

// prewarmCache walks the cache directory and ensures rootfs.qcow2 exists
// for every hash present, so the first sandbox per template doesn't wait
// for the raw→qcow2 conversion. Skips entries whose name doesn't look
// like a content hash and logs (not fatal) on individual failures.
func prewarmCache(cache *agent.ImageCache, dir string, logger *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Warn("prewarm: read cache dir", "dir", dir, "err", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !cache.HasTemplate(e.Name()) {
			continue
		}
		start := time.Now()
		if err := cache.EnsureRootfsBacking(e.Name()); err != nil {
			logger.Warn("prewarm: EnsureRootfsBacking", "hash", e.Name(), "err", err)
			continue
		}
		logger.Info("prewarm: qcow2 backing ready", "hash", e.Name(), "elapsed_ms", time.Since(start).Milliseconds())
	}
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
		natsURL:           os.Getenv("NATS_URL"),
	}
	cfg.cacheMaxBytes = envInt64("VAJRA_AGENT_CACHE_MAX_BYTES", 50*1024*1024*1024)
	cfg.poolMinSize = envInt("VAJRA_AGENT_POOL_MIN_SIZE", 0)
	cfg.poolMaxSize = envInt("VAJRA_AGENT_POOL_MAX_SIZE", 0)
	// Host capacity advertised to the scheduler. The VAJRA_AGENT_TOTAL_*
	// vars are operator overrides; when unset (or non-positive) we
	// auto-detect from the host so a node always advertises the capacity
	// it actually has. A hardcoded override silently mis-sizes the node
	// whenever the box is resized — a stale "2" left over from a
	// c8i.large on a since-upgraded 4-vCPU host makes the scheduler call
	// the node full after a single 2-vCPU sandbox and autoscale a fresh
	// EC2 for every subsequent create instead of bin-packing.
	cfg.totalCPU = envInt("VAJRA_AGENT_TOTAL_CPU", 0)
	if cfg.totalCPU <= 0 {
		cfg.totalCPU = detectCPU()
	}
	cfg.totalMemMB = envInt("VAJRA_AGENT_TOTAL_MEMORY_MB", 0)
	if cfg.totalMemMB <= 0 {
		cfg.totalMemMB = detectMemoryMB()
	}
	cfg.totalDiskGB = envInt("VAJRA_AGENT_TOTAL_DISK_GB", 0)
	if cfg.totalDiskGB <= 0 {
		cfg.totalDiskGB = detectDiskGB(cfg.sandboxRoot)
	}

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

// detectCPU returns the host's logical CPU count. Used when the
// VAJRA_AGENT_TOTAL_CPU override is unset so a node always advertises
// the capacity it actually has rather than a stale hardcoded value.
func detectCPU() int {
	return runtime.NumCPU()
}

// detectMemoryMB returns total host RAM in MB, read from /proc/meminfo's
// MemTotal. Returns 0 when the file can't be read or parsed (e.g.
// non-Linux dev hosts) so the caller can fall back to an explicit value.
func detectMemoryMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return int(kb / 1024)
		}
	}
	return 0
}

// detectDiskGB returns the total size, in GB, of the filesystem backing
// path. Falls back to the root filesystem when path does not exist yet
// (the sandbox root is created lazily). Returns 0 if neither resolves.
func detectDiskGB(path string) int {
	for _, p := range []string{path, "/"} {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err != nil {
			continue
		}
		total := st.Blocks * uint64(st.Bsize)
		return int(total / (1024 * 1024 * 1024))
	}
	return 0
}
