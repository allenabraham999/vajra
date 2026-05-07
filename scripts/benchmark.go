// benchmark.go drives internal/vmm to measure end-to-end snapshot restore
// latency: RestoreVM (start cloud-hypervisor + --restore + Resume) followed
// by DestroyVM, repeated N times. Replaces the bash benchmark whose timing
// was dominated by sleep + ch-remote process spawns.
//
// Usage:
//   go run scripts/benchmark.go [-snapshot /path] [-n 10] [-bin /usr/local/bin/cloud-hypervisor]
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/allenabraham999/vajra/internal/vmm"
)

func main() {
	var (
		snapshotPath = flag.String("snapshot", "/tmp/ch-snapshot", "path to snapshot directory")
		iterations   = flag.Int("n", 10, "number of restore/destroy iterations")
		binary       = flag.String("bin", vmm.DefaultBinaryPath, "cloud-hypervisor binary path")
		kernel       = flag.String("kernel", vmm.DefaultKernelPath, "kernel path (required by CH CLI even on --restore)")
		socketDir    = flag.String("socket-dir", vmm.DefaultSocketDir, "directory for VMM API sockets")
		quiet        = flag.Bool("quiet", true, "silence vmm logger info messages during runs")
	)
	flag.Parse()

	if *iterations < 1 {
		fmt.Fprintln(os.Stderr, "n must be >= 1")
		os.Exit(2)
	}
	if _, err := os.Stat(*snapshotPath); err != nil {
		fmt.Fprintf(os.Stderr, "snapshot path %q not usable: %v\n", *snapshotPath, err)
		os.Exit(2)
	}
	if _, err := os.Stat(*binary); err != nil {
		fmt.Fprintf(os.Stderr, "cloud-hypervisor binary %q not found: %v\n", *binary, err)
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *quiet {
		level = slog.LevelWarn
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	mgr := vmm.NewVMManager(logger).WithBinary(*binary).WithSocketDir(*socketDir).WithKernel(*kernel)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("vajra benchmark: %d iterations, snapshot=%s\n", *iterations, *snapshotPath)
	fmt.Println("---")

	durations := make([]time.Duration, 0, *iterations)
	for i := 0; i < *iterations; i++ {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "interrupted; aborting")
			break
		}
		d, err := runOnce(ctx, mgr, *snapshotPath, i)
		if err != nil {
			fmt.Fprintf(os.Stderr, "run %02d FAILED: %v\n", i, err)
			os.Exit(1)
		}
		durations = append(durations, d)
		fmt.Printf("run %02d: %6.2f ms\n", i, ms(d))
	}

	if len(durations) == 0 {
		os.Exit(1)
	}
	fmt.Println("---")
	printStats(durations)
}

// runOnce measures a single RestoreVM + DestroyVM cycle. The clock starts
// immediately before RestoreVM (which itself spawns cloud-hypervisor, polls
// the API socket, and issues vm.resume) and stops the moment Resume returns.
// DestroyVM time is excluded from the reported duration but still happens
// inline so the next iteration starts from a clean slate.
func runOnce(ctx context.Context, mgr *vmm.VMManager, snapshotPath string, idx int) (time.Duration, error) {
	vmID := fmt.Sprintf("bench-%d-%d", os.Getpid(), idx)

	t0 := time.Now()
	socketPath, err := mgr.RestoreVM(ctx, vmID, snapshotPath)
	if err != nil {
		return 0, fmt.Errorf("restore: %w", err)
	}
	elapsed := time.Since(t0)

	// Use a detached context for cleanup so a cancelled benchmark still
	// tears down cloud-hypervisor instead of leaking the child process.
	destroyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := mgr.DestroyVM(destroyCtx, socketPath); err != nil {
		return 0, fmt.Errorf("destroy: %w", err)
	}
	return elapsed, nil
}

// printStats emits avg/p50/p95/p99/min/max in milliseconds. Percentiles use
// the nearest-rank method, which is well-defined for tiny samples (n=10).
func printStats(d []time.Duration) {
	sorted := make([]time.Duration, len(d))
	copy(sorted, d)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum time.Duration
	for _, v := range sorted {
		sum += v
	}
	avg := sum / time.Duration(len(sorted))

	fmt.Printf("count: %d\n", len(sorted))
	fmt.Printf("min  : %6.2f ms\n", ms(sorted[0]))
	fmt.Printf("avg  : %6.2f ms\n", ms(avg))
	fmt.Printf("p50  : %6.2f ms\n", ms(percentile(sorted, 0.50)))
	fmt.Printf("p95  : %6.2f ms\n", ms(percentile(sorted, 0.95)))
	fmt.Printf("p99  : %6.2f ms\n", ms(percentile(sorted, 0.99)))
	fmt.Printf("max  : %6.2f ms\n", ms(sorted[len(sorted)-1]))
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
