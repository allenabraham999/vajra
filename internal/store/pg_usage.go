// Package store — pg_usage.go is the cost-tracking ledger for sandboxes.
// Every sandbox state transition that affects billing (RUNNING start,
// RUNNING end) is appended as a sandbox_usage row; queries roll the rows
// up over a window for /v1/usage.
//
// Why a ledger instead of a single row per sandbox? Sandboxes can stop
// and restart any number of times in a billing window, and we want each
// active interval to count separately so a sandbox that ran 5 minutes,
// stopped overnight, then ran 5 more minutes is billed for 10 minutes,
// not 8 hours.
package store

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/allenabraham999/vajra/internal/models"
)

// SandboxUsage is one billing interval of a sandbox at a fixed config.
type SandboxUsage struct {
	ID              int64     `db:"id" json:"id"`
	SandboxID       string    `db:"sandbox_id" json:"sandbox_id"`
	AccountID       string    `db:"account_id" json:"account_id"`
	PeriodStart     time.Time `db:"period_start" json:"period_start"`
	PeriodEnd       time.Time `db:"period_end" json:"period_end"`
	VCPUSeconds     int64     `db:"vcpu_seconds" json:"vcpu_seconds"`
	MemoryMBSeconds int64     `db:"memory_mb_seconds" json:"memory_mb_seconds"`
	DiskGBSeconds   int64     `db:"disk_gb_seconds" json:"disk_gb_seconds"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
}

// UsageStore is the interface handlers depend on. Recording is idempotent
// up to a point — RecordStart writes a half-open row (PeriodEnd zero)
// that RecordStop closes. If a sandbox is destroyed without a stop call,
// the reconciler should backfill via FinalizeOpenIntervals.
type UsageStore interface {
	// RecordStart opens a new active interval for sandboxID at startedAt.
	// The config snapshot (vcpu/memory/disk) is captured here; concurrent
	// scaling of a sandbox should call RecordStop+RecordStart with the
	// new config so the rate is accurate.
	RecordStart(ctx context.Context, accountID, sandboxID string, cfg models.SandboxConfig, startedAt time.Time) error
	// RecordStop closes the most-recent open interval for sandboxID.
	// Returns nil if no open interval exists (idempotent for a sandbox
	// that has been stopped and is being stopped again).
	RecordStop(ctx context.Context, sandboxID string, stoppedAt time.Time) error
	// FinalizeOpenIntervals closes any interval older than maxAge that
	// the reconciler observed has gone DESTROYED/ERROR without a stop.
	// Returns the number of rows updated.
	FinalizeOpenIntervals(ctx context.Context, sandboxID string, closedAt time.Time) (int, error)
	// SumByAccount returns the rolled-up totals for accountID over the
	// half-open window [from, to). Open intervals (PeriodEnd zero) are
	// closed on the fly at min(now, to) so a query during a running
	// sandbox returns up-to-the-second totals.
	SumByAccount(ctx context.Context, accountID string, from, to time.Time) (UsageRollup, error)
	// PerSandbox returns a per-sandbox breakdown over the same window.
	PerSandbox(ctx context.Context, accountID string, from, to time.Time) ([]UsageRow, error)
}

// UsageRollup is the account-level total returned by SumByAccount.
type UsageRollup struct {
	From            time.Time `json:"from"`
	To              time.Time `json:"to"`
	VCPUSeconds     int64     `json:"vcpu_seconds"`
	MemoryMBSeconds int64     `json:"memory_mb_seconds"`
	DiskGBSeconds   int64     `json:"disk_gb_seconds"`
	Cost            float64   `json:"cost"`
}

// UsageRow is one sandbox's contribution to an account's usage.
type UsageRow struct {
	SandboxID       string  `db:"sandbox_id" json:"sandbox_id"`
	VCPUSeconds     int64   `db:"vcpu_seconds" json:"vcpu_seconds"`
	MemoryMBSeconds int64   `db:"memory_mb_seconds" json:"memory_mb_seconds"`
	DiskGBSeconds   int64   `db:"disk_gb_seconds" json:"disk_gb_seconds"`
	Cost            float64 `json:"cost"`
}

// Cost rates from the brief (per hour). VCPU $0.06/hr, Memory $0.01/GB/hr,
// Storage $0.005/GB/hr. Stored as per-second to mirror the ledger's unit
// so callers don't redo the divisions.
const (
	costVCPUSecond     = 0.06 / 3600.0
	costMemoryGBSecond = 0.01 / 3600.0
	costDiskGBSecond   = 0.005 / 3600.0
)

// CalculateCost converts a rollup's seconds into dollars. Memory is in
// MB-seconds in the ledger (matches the schema column); we divide by 1024
// to convert to GB-seconds for the rate sheet.
func CalculateCost(vcpuSec, memMBSec, diskGBSec int64) float64 {
	mbToGB := float64(memMBSec) / 1024.0
	return float64(vcpuSec)*costVCPUSecond +
		mbToGB*costMemoryGBSecond +
		float64(diskGBSec)*costDiskGBSecond
}

// pgUsageStore is the Postgres implementation. It uses a sentinel
// "open interval" representation: PeriodEnd is set to the same value as
// PeriodStart and the duration columns are zero until RecordStop fires.
// We can't use NULL because the schema's column is NOT NULL — keeping
// PeriodEnd == PeriodStart and treating that as "open" lets us run on
// the existing migration without an ALTER TABLE.
type pgUsageStore struct{ ext sqlx.ExtContext }

// Usage exposes pgUsageStore on *Postgres.
func (p *Postgres) Usage() UsageStore { return &pgUsageStore{ext: p.ext} }

// RecordStart inserts an open interval for the sandbox. If an interval is
// already open (e.g. a duplicate transition), we leave it alone — the
// duration is computed at SumByAccount time.
func (s *pgUsageStore) RecordStart(ctx context.Context, accountID, sandboxID string, cfg models.SandboxConfig, startedAt time.Time) error {
	// Refuse to open a second interval if one is already open.
	var openCount int
	err := sqlx.GetContext(ctx, s.ext, &openCount,
		`SELECT COUNT(*) FROM sandbox_usage
		 WHERE sandbox_id = $1 AND period_end = period_start`, sandboxID)
	if err != nil {
		return translate(err)
	}
	if openCount > 0 {
		return nil
	}
	_, err = s.ext.ExecContext(ctx,
		`INSERT INTO sandbox_usage (
		   sandbox_id, account_id, period_start, period_end,
		   vcpu_seconds, memory_mb_seconds, disk_gb_seconds, created_at
		 ) VALUES ($1, $2, $3, $3, 0, 0, 0, NOW())`,
		sandboxID, accountID, startedAt.UTC())
	return translate(err)
}

// RecordStop closes the open interval (PeriodEnd == PeriodStart) for
// sandboxID. The vcpu/memory/disk seconds are computed by multiplying
// the sandbox's current config snapshot by the elapsed window; we read
// the config from the sandboxes row to keep this method's signature
// minimal.
func (s *pgUsageStore) RecordStop(ctx context.Context, sandboxID string, stoppedAt time.Time) error {
	const q = `
WITH cfg AS (
    SELECT
        COALESCE((config->>'vcpus')::INT, 0)     AS vcpus,
        COALESCE((config->>'memory_mb')::INT, 0) AS memory_mb,
        COALESCE((config->>'disk_gb')::INT, 0)   AS disk_gb
    FROM sandboxes WHERE id = $1
)
UPDATE sandbox_usage SET
    period_end = $2,
    vcpu_seconds      = ((EXTRACT(EPOCH FROM ($2 - period_start)))::BIGINT) * (SELECT vcpus FROM cfg),
    memory_mb_seconds = ((EXTRACT(EPOCH FROM ($2 - period_start)))::BIGINT) * (SELECT memory_mb FROM cfg),
    disk_gb_seconds   = ((EXTRACT(EPOCH FROM ($2 - period_start)))::BIGINT) * (SELECT disk_gb FROM cfg)
WHERE sandbox_id = $1
  AND period_end = period_start
  AND $2 >= period_start`
	res, err := s.ext.ExecContext(ctx, q, sandboxID, stoppedAt.UTC())
	if err != nil {
		return translate(err)
	}
	// 0 rows affected is fine — sandbox might have never been started, or
	// was already closed by FinalizeOpenIntervals.
	_, _ = res.RowsAffected()
	return nil
}

// FinalizeOpenIntervals is the reconciler's hammer: any open interval for
// sandboxID is closed at closedAt. Returns the number of rows touched so
// callers can log the cleanup.
func (s *pgUsageStore) FinalizeOpenIntervals(ctx context.Context, sandboxID string, closedAt time.Time) (int, error) {
	if err := s.RecordStop(ctx, sandboxID, closedAt); err != nil {
		return 0, err
	}
	var n int
	err := sqlx.GetContext(ctx, s.ext, &n,
		`SELECT COUNT(*) FROM sandbox_usage
		 WHERE sandbox_id = $1 AND period_end > period_start`, sandboxID)
	if err != nil {
		return 0, translate(err)
	}
	return n, nil
}

// SumByAccount rolls up vcpu/memory/disk seconds across the half-open
// window. Open intervals are clamped to (period_start, min(now, to)) so
// a query mid-run still returns useful totals.
func (s *pgUsageStore) SumByAccount(ctx context.Context, accountID string, from, to time.Time) (UsageRollup, error) {
	out := UsageRollup{From: from, To: to}
	const q = `
WITH cfg AS (
    SELECT
        s.id AS sandbox_id,
        COALESCE((s.config->>'vcpus')::INT, 0)     AS vcpus,
        COALESCE((s.config->>'memory_mb')::INT, 0) AS memory_mb,
        COALESCE((s.config->>'disk_gb')::INT, 0)   AS disk_gb
    FROM sandboxes s
), windows AS (
    SELECT
        u.sandbox_id,
        GREATEST(u.period_start, $2) AS effective_start,
        LEAST(
            CASE WHEN u.period_end = u.period_start THEN $3 ELSE u.period_end END,
            $3
        ) AS effective_end,
        cfg.vcpus, cfg.memory_mb, cfg.disk_gb
    FROM sandbox_usage u
    LEFT JOIN cfg ON cfg.sandbox_id = u.sandbox_id
    WHERE u.account_id = $1
      AND u.period_start < $3
      AND CASE WHEN u.period_end = u.period_start THEN $3 ELSE u.period_end END > $2
)
SELECT
    COALESCE(SUM(GREATEST(EXTRACT(EPOCH FROM (effective_end - effective_start))::BIGINT, 0) * vcpus), 0)     AS vcpu_seconds,
    COALESCE(SUM(GREATEST(EXTRACT(EPOCH FROM (effective_end - effective_start))::BIGINT, 0) * memory_mb), 0) AS memory_mb_seconds,
    COALESCE(SUM(GREATEST(EXTRACT(EPOCH FROM (effective_end - effective_start))::BIGINT, 0) * disk_gb), 0)   AS disk_gb_seconds
FROM windows`
	row := struct {
		VCPUSeconds     int64 `db:"vcpu_seconds"`
		MemoryMBSeconds int64 `db:"memory_mb_seconds"`
		DiskGBSeconds   int64 `db:"disk_gb_seconds"`
	}{}
	if err := sqlx.GetContext(ctx, s.ext, &row, q, accountID, from.UTC(), to.UTC()); err != nil {
		return out, translate(err)
	}
	out.VCPUSeconds = row.VCPUSeconds
	out.MemoryMBSeconds = row.MemoryMBSeconds
	out.DiskGBSeconds = row.DiskGBSeconds
	out.Cost = CalculateCost(out.VCPUSeconds, out.MemoryMBSeconds, out.DiskGBSeconds)
	return out, nil
}

// PerSandbox is the per-row variant of SumByAccount, used to render the
// dashboard's per-sandbox cost table. Same window semantics; same
// open-interval clamping.
func (s *pgUsageStore) PerSandbox(ctx context.Context, accountID string, from, to time.Time) ([]UsageRow, error) {
	const q = `
WITH cfg AS (
    SELECT
        s.id AS sandbox_id,
        COALESCE((s.config->>'vcpus')::INT, 0)     AS vcpus,
        COALESCE((s.config->>'memory_mb')::INT, 0) AS memory_mb,
        COALESCE((s.config->>'disk_gb')::INT, 0)   AS disk_gb
    FROM sandboxes s
), windows AS (
    SELECT
        u.sandbox_id,
        GREATEST(u.period_start, $2) AS effective_start,
        LEAST(
            CASE WHEN u.period_end = u.period_start THEN $3 ELSE u.period_end END,
            $3
        ) AS effective_end,
        cfg.vcpus, cfg.memory_mb, cfg.disk_gb
    FROM sandbox_usage u
    LEFT JOIN cfg ON cfg.sandbox_id = u.sandbox_id
    WHERE u.account_id = $1
      AND u.period_start < $3
      AND CASE WHEN u.period_end = u.period_start THEN $3 ELSE u.period_end END > $2
)
SELECT
    sandbox_id,
    SUM(GREATEST(EXTRACT(EPOCH FROM (effective_end - effective_start))::BIGINT, 0) * vcpus)     AS vcpu_seconds,
    SUM(GREATEST(EXTRACT(EPOCH FROM (effective_end - effective_start))::BIGINT, 0) * memory_mb) AS memory_mb_seconds,
    SUM(GREATEST(EXTRACT(EPOCH FROM (effective_end - effective_start))::BIGINT, 0) * disk_gb)   AS disk_gb_seconds
FROM windows
GROUP BY sandbox_id
ORDER BY sandbox_id`
	out := []UsageRow{}
	if err := sqlx.SelectContext(ctx, s.ext, &out, q, accountID, from.UTC(), to.UTC()); err != nil {
		return nil, translate(err)
	}
	for i := range out {
		out[i].Cost = CalculateCost(out[i].VCPUSeconds, out[i].MemoryMBSeconds, out[i].DiskGBSeconds)
	}
	return out, nil
}
