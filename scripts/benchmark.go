// benchmark.go drives internal/vmm and the running vajra-agent to measure
// two end-to-end paths back to back:
//
//   - Cold path: RestoreVM (start cloud-hypervisor + --restore + Resume),
//     timed start of RestoreVM through Resume returning, followed by
//     DestroyVM. Replaces the bash benchmark whose timing was dominated
//     by sleep + ch-remote process spawns.
//   - Pool path (--pool flag): POST /sandbox/create against the local
//     vajra-agent, which assigns a warm member from the pre-warm pool.
//     Timed from request send through the agent returning the running
//     sandbox JSON, followed by DELETE /sandbox/{id} to clean up so the
//     next iteration starts from a refilled pool.
//
// Usage:
//   go run scripts/benchmark.go [-snapshot /path] [-n 10] [-bin /path]
//   go run scripts/benchmark.go --pool --agent-url http://localhost:9000 --template <hash> [-n 10]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/allenabraham999/vajra/internal/vmm"
)

func main() {
	var (
		snapshotPath = flag.String("snapshot", "/tmp/ch-snapshot", "path to snapshot directory (cold path)")
		iterations   = flag.Int("n", 10, "number of iterations per path")
		binary       = flag.String("bin", vmm.DefaultBinaryPath, "cloud-hypervisor binary path")
		socketDir    = flag.String("socket-dir", vmm.DefaultSocketDir, "directory for VMM API sockets")
		quiet        = flag.Bool("quiet", true, "silence vmm logger info messages during runs")
		usePool      = flag.Bool("pool", false, "also benchmark the warm-pool assign path")
		agentURL     = flag.String("agent-url", "http://localhost:9000", "vajra-agent base URL (pool path)")
		templateHash = flag.String("template", "", "template hash for pool create requests")
	)
	flag.Parse()

	if *iterations < 1 {
		fmt.Fprintln(os.Stderr, "n must be >= 1")
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	level := slog.LevelInfo
	if *quiet {
		level = slog.LevelWarn
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Cold path runs unconditionally — gating the snapshot path checks
	// happen here so --pool-only users don't need a snapshot to exist.
	var coldDurations []time.Duration
	if !*usePool || flagPassed("snapshot") {
		if _, err := os.Stat(*snapshotPath); err != nil {
			fmt.Fprintf(os.Stderr, "snapshot path %q not usable: %v\n", *snapshotPath, err)
			os.Exit(2)
		}
		if _, err := os.Stat(*binary); err != nil {
			fmt.Fprintf(os.Stderr, "cloud-hypervisor binary %q not found: %v\n", *binary, err)
			os.Exit(2)
		}
		mgr := vmm.NewVMManager(logger).WithBinary(*binary).WithSocketDir(*socketDir)
		fmt.Printf("vajra benchmark: cold path, %d iterations, snapshot=%s\n", *iterations, *snapshotPath)
		fmt.Println("---")
		coldDurations = runCold(ctx, mgr, *snapshotPath, *iterations)
		fmt.Println("---")
		printStats("Cold create", coldDurations)
	}

	if *usePool {
		if *templateHash == "" {
			fmt.Fprintln(os.Stderr, "--pool requires --template <hash>")
			os.Exit(2)
		}
		fmt.Println()
		fmt.Printf("vajra benchmark: pool path, %d iterations, agent=%s, template=%s\n",
			*iterations, *agentURL, *templateHash)
		fmt.Println("---")
		poolDurations := runPool(ctx, *agentURL, *templateHash, *iterations)
		fmt.Println("---")
		printStats("Pool assign", poolDurations)
	}

	if *usePool && coldDurations != nil {
		fmt.Println()
		fmt.Println("=== Summary ===")
		printSummary("Cold create", coldDurations)
		printSummary("Pool assign", runPoolDurations)
	}
}

// runPoolDurations is captured so the summary block at the bottom can
// reuse the same slice without re-running iterations.
var runPoolDurations []time.Duration

func runCold(ctx context.Context, mgr *vmm.VMManager, snapshotPath string, n int) []time.Duration {
	durations := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "interrupted; aborting")
			break
		}
		d, err := runColdOnce(ctx, mgr, snapshotPath, i)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cold run %02d FAILED: %v\n", i, err)
			os.Exit(1)
		}
		durations = append(durations, d)
		fmt.Printf("cold %02d: %6.2f ms\n", i, ms(d))
	}
	return durations
}

// runColdOnce measures a single RestoreVM + DestroyVM cycle. The clock
// starts immediately before RestoreVM (which itself spawns CH, polls the
// API socket, and issues vm.resume) and stops the moment Resume returns.
// DestroyVM time is excluded from the reported duration but still happens
// inline so the next iteration starts from a clean slate.
func runColdOnce(ctx context.Context, mgr *vmm.VMManager, snapshotPath string, idx int) (time.Duration, error) {
	vmID := fmt.Sprintf("bench-%d-%d", os.Getpid(), idx)
	t0 := time.Now()
	socketPath, err := mgr.RestoreVM(ctx, vmID, snapshotPath)
	if err != nil {
		return 0, fmt.Errorf("restore: %w", err)
	}
	elapsed := time.Since(t0)
	destroyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := mgr.DestroyVM(destroyCtx, socketPath); err != nil {
		return 0, fmt.Errorf("destroy: %w", err)
	}
	return elapsed, nil
}

func runPool(ctx context.Context, agentURL, template string, n int) []time.Duration {
	durations := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "interrupted; aborting")
			break
		}
		d, id, err := runPoolOnce(ctx, agentURL, template)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pool run %02d FAILED: %v\n", i, err)
			os.Exit(1)
		}
		durations = append(durations, d)
		fmt.Printf("pool %02d: %6.2f ms (sandbox=%s)\n", i, ms(d), id)
		if err := destroyViaAgent(ctx, agentURL, id); err != nil {
			fmt.Fprintf(os.Stderr, "cleanup of %s failed: %v\n", id, err)
		}
		// Give the pool a moment to refill so successive iterations all
		// hit the warm path. The replenish loop ticks every 1 s; this
		// pause is just a yield, not a sleep-tied wait.
		time.Sleep(50 * time.Millisecond)
	}
	runPoolDurations = durations
	return durations
}

// runPoolOnce POSTs /sandbox/create to the local agent and times the
// round trip. The agent's handler consults the pool, resumes the warm
// member if any, and returns the sandbox JSON inline.
func runPoolOnce(ctx context.Context, agentURL, template string) (time.Duration, string, error) {
	body := map[string]any{
		"template_hash": template,
		"config":        map[string]int{"vcpus": 2, "memory_mb": 512},
		"from_pool":     true,
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentURL+"/sandbox/create", bytes.NewReader(buf))
	if err != nil {
		return 0, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	t0 := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(t0)

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return 0, "", fmt.Errorf("agent HTTP %d: %s", resp.StatusCode, string(bytes.TrimSpace(raw)))
	}
	var sb struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sb); err != nil {
		return 0, "", fmt.Errorf("decode: %w", err)
	}
	return elapsed, sb.ID, nil
}

func destroyViaAgent(ctx context.Context, agentURL, id string) error {
	if id == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, agentURL+"/sandbox/"+id, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("agent HTTP %d on destroy", resp.StatusCode)
	}
	return nil
}

// flagPassed reports whether the named flag was explicitly set on the
// command line (as opposed to defaulted). Used so --pool-only invocations
// don't reject a missing snapshot directory.
func flagPassed(name string) bool {
	seen := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

func printStats(label string, d []time.Duration) {
	if len(d) == 0 {
		return
	}
	sorted := make([]time.Duration, len(d))
	copy(sorted, d)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum time.Duration
	for _, v := range sorted {
		sum += v
	}
	avg := sum / time.Duration(len(sorted))

	fmt.Printf("%s\n", label)
	fmt.Printf("  count: %d\n", len(sorted))
	fmt.Printf("  min  : %6.2f ms\n", ms(sorted[0]))
	fmt.Printf("  avg  : %6.2f ms\n", ms(avg))
	fmt.Printf("  p50  : %6.2f ms\n", ms(percentile(sorted, 0.50)))
	fmt.Printf("  p95  : %6.2f ms\n", ms(percentile(sorted, 0.95)))
	fmt.Printf("  p99  : %6.2f ms\n", ms(percentile(sorted, 0.99)))
	fmt.Printf("  max  : %6.2f ms\n", ms(sorted[len(sorted)-1]))
}

func printSummary(label string, d []time.Duration) {
	if len(d) == 0 {
		fmt.Printf("%-13s (no samples)\n", label)
		return
	}
	sorted := make([]time.Duration, len(d))
	copy(sorted, d)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum time.Duration
	for _, v := range sorted {
		sum += v
	}
	avg := sum / time.Duration(len(sorted))
	fmt.Printf("%-13s avg=%6.2fms p50=%6.2fms p95=%6.2fms p99=%6.2fms\n",
		label, ms(avg), ms(percentile(sorted, 0.50)),
		ms(percentile(sorted, 0.95)), ms(percentile(sorted, 0.99)))
}

// percentile returns the nearest-rank percentile from a pre-sorted slice.
// p is in [0, 1]. For len(sorted)=10, p=0.95 → index 9 (the max), which is
// the conventional behaviour for small samples.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
