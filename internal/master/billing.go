// Package master — billing.go is the usage meter. On a fixed interval it
// sweeps every RUNNING sandbox, prices the slice of time the tick covers,
// appends it to the per-day usage rollup, and decrements the owning
// account's prepaid credit balance.
//
// The meter holds no state between ticks: every Tick re-reads the world
// from the store, so it is safe to stop and restart. A single master
// replica should own it — running it in more than one process would
// double-bill — which main enforces by starting it from one place.
package master

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/allenabraham999/vajra/internal/models"
	"github.com/allenabraham999/vajra/internal/store"
)

// Billing defaults. vCPU and memory are the only metered dimensions for
// the prepaid meter — disk is bundled. A bad env var falls back to these
// rather than silently disabling billing.
const (
	defaultBillingInterval   = 10 * time.Second
	defaultVCPUHourlyUSD     = 0.06
	defaultMemoryGBHourlyUSD = 0.01
)

// BillingConfig is the env-derived billing configuration: the metering
// cadence and rates, plus the nested Stripe settings.
type BillingConfig struct {
	Enabled           bool
	Interval          time.Duration
	VCPUHourlyUSD     float64
	MemoryGBHourlyUSD float64
	Stripe            StripeConfig
}

// BillingMeter prices running sandboxes and deducts credits on a fixed
// interval.
type BillingMeter struct {
	store             store.Store
	interval          time.Duration
	vcpuHourlyUSD     float64
	memoryGBHourlyUSD float64
	logger            *slog.Logger
	now               func() time.Time
}

// NewBillingMeter builds a meter. A non-positive interval or rate falls
// back to the brief's defaults so a misconfigured env var cannot quietly
// turn billing off.
func NewBillingMeter(s store.Store, interval time.Duration, vcpuHourlyUSD, memoryGBHourlyUSD float64) *BillingMeter {
	if interval <= 0 {
		interval = defaultBillingInterval
	}
	if vcpuHourlyUSD <= 0 {
		vcpuHourlyUSD = defaultVCPUHourlyUSD
	}
	if memoryGBHourlyUSD <= 0 {
		memoryGBHourlyUSD = defaultMemoryGBHourlyUSD
	}
	return &BillingMeter{
		store:             s,
		interval:          interval,
		vcpuHourlyUSD:     vcpuHourlyUSD,
		memoryGBHourlyUSD: memoryGBHourlyUSD,
		logger:            slog.Default(),
		now:               time.Now,
	}
}

// WithLogger overrides the meter's logger, returning the meter so it can
// be configured inline at construction.
func (b *BillingMeter) WithLogger(l *slog.Logger) *BillingMeter {
	if l != nil {
		b.logger = l
	}
	return b
}

// Run ticks the meter until ctx is cancelled. Launch it in its own
// goroutine from main.
func (b *BillingMeter) Run(ctx context.Context) {
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := b.Tick(ctx); err != nil {
				b.logger.Error("billing tick failed", "err", err)
			}
		case <-ctx.Done():
			b.logger.Info("billing meter stopped")
			return
		}
	}
}

// Tick prices one interval of every RUNNING sandbox and settles it: the
// usage is appended to the daily rollup and the cost is deducted from
// each account's balance. Exported so tests can drive a single
// deterministic tick.
func (b *BillingMeter) Tick(ctx context.Context) error {
	sandboxes, err := b.store.Sandboxes().ListByState(ctx, models.SandboxStateRunning, store.ListOpts{Limit: 1000})
	if err != nil {
		return fmt.Errorf("list running sandboxes: %w", err)
	}
	if len(sandboxes) == 0 {
		return nil
	}

	// fraction is the slice of an hour this tick covers (10s ≈ 0.00278h).
	fraction := b.interval.Hours()
	day := b.now().UTC()

	// Aggregate per account so each account takes exactly one Accumulate
	// and one DecrementCredits per tick, regardless of how many sandboxes
	// it is running.
	type slice struct{ vcpuHours, memoryGBHours, cost float64 }
	perAccount := make(map[string]*slice)
	for _, sb := range sandboxes {
		gb := float64(sb.Config.MemoryMB) / 1024.0
		vcpuHours := float64(sb.Config.VCPUs) * fraction
		memoryGBHours := gb * fraction
		cost := float64(sb.Config.VCPUs)*b.vcpuHourlyUSD*fraction + gb*b.memoryGBHourlyUSD*fraction
		s := perAccount[sb.AccountID]
		if s == nil {
			s = &slice{}
			perAccount[sb.AccountID] = s
		}
		s.vcpuHours += vcpuHours
		s.memoryGBHours += memoryGBHours
		s.cost += cost
	}

	var totalCost float64
	for accountID, s := range perAccount {
		if err := b.store.Usage().Accumulate(ctx, accountID, day, s.vcpuHours, s.memoryGBHours, s.cost); err != nil {
			b.logger.Error("billing: accumulate usage failed", "account_id", accountID, "err", err)
			continue
		}
		if err := b.store.Accounts().DecrementCredits(ctx, accountID, s.cost); err != nil {
			b.logger.Error("billing: decrement credits failed", "account_id", accountID, "err", err)
			continue
		}
		totalCost += s.cost
		b.logger.Info("billing tick settled",
			"account_id", accountID,
			"cost_usd", s.cost,
			"vcpu_hours", s.vcpuHours,
			"memory_gb_hours", s.memoryGBHours,
		)
	}
	b.logger.Info("billing tick complete",
		"accounts", len(perAccount),
		"sandboxes", len(sandboxes),
		"total_cost_usd", totalCost,
	)
	return nil
}
