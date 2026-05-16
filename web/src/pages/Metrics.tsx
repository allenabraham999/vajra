import { useEffect, useMemo, useState } from 'react'
import { Zap } from 'lucide-react'
import api from '../api/client'
import type { BootTime, PoolStats } from '../api/types'
import PageHeader from '../components/PageHeader'
import { formatRelative } from '../utils/format'

// formatMs renders a boot duration: milliseconds under a second, seconds
// with one or two decimals above it.
function formatMs(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—'
  if (ms < 1000) return `${Math.round(ms)} ms`
  const s = ms / 1000
  return `${s.toFixed(s < 10 ? 2 : 1)} s`
}

export default function MetricsPage() {
  const [pool, setPool] = useState<PoolStats | null>(null)
  const [boots, setBoots] = useState<BootTime[]>([])
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    let alive = true
    async function load() {
      const [p, b] = await Promise.all([
        api.pool.stats().catch(() => null),
        api.sandboxes.bootTimes().catch(() => [] as BootTime[]),
      ])
      if (!alive) return
      setPool(p)
      setBoots(b)
      setLoaded(true)
    }
    load()
    const t = setInterval(load, 5000)
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [])

  const avgBootMs = useMemo(() => {
    if (boots.length === 0) return null
    const sum = boots.reduce((acc, b) => acc + b.time_to_running_ms, 0)
    return sum / boots.length
  }, [boots])

  const hasPool = pool != null && pool.template !== ''

  return (
    <>
      <PageHeader
        title="Metrics"
        description="Pre-warm pool performance and sandbox boot times."
      />
      <div className="p-6 space-y-8">
        {/* Pool Performance */}
        <section>
          <h2 className="text-sm font-medium mb-3">Pool Performance</h2>
          <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
            <StatCard
              label="Pool Size"
              value={hasPool ? pool!.target_size : '—'}
              hint={hasPool ? `${pool!.min_size}–${pool!.max_size} range` : 'no pool configured'}
            />
            <StatCard
              label="Available"
              value={hasPool ? pool!.available : '—'}
              hint={
                hasPool
                  ? pool!.warming > 0
                    ? `${pool!.warming} warming`
                    : 'ready now'
                  : undefined
              }
            />
            <StatCard
              label="Hit Rate"
              value={hasPool ? `${pool!.hit_rate_pct.toFixed(0)}%` : '—'}
              hint={
                hasPool
                  ? `${pool!.total_hits} hits · ${pool!.total_misses} misses`
                  : undefined
              }
            />
            <StatCard
              label="Avg Boot Time"
              value={avgBootMs != null ? formatMs(avgBootMs) : '—'}
              hint={
                boots.length > 0
                  ? `last ${boots.length} ${boots.length === 1 ? 'sandbox' : 'sandboxes'}`
                  : 'no recent boots'
              }
            />
          </div>
        </section>

        {/* Recent Sandbox Boot Times */}
        <section>
          <h2 className="text-sm font-medium mb-3">Recent Sandbox Boot Times</h2>
          <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 shadow-lg shadow-black/20">
            {!loaded ? (
              <div className="p-8 text-center text-sm text-zinc-500">Loading…</div>
            ) : boots.length === 0 ? (
              <div className="p-8 text-center text-sm text-zinc-500">
                No sandbox boots recorded yet. Create a sandbox to see its boot time here.
              </div>
            ) : (
              <table className="w-full text-sm">
                <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                  <tr className="border-b border-zinc-900">
                    <th className="text-left font-medium px-4 py-2">Name</th>
                    <th className="text-left font-medium px-4 py-2">Created</th>
                    <th className="text-right font-medium px-4 py-2">Boot time</th>
                    <th className="text-right font-medium px-4 py-2">Source</th>
                  </tr>
                </thead>
                <tbody>
                  {boots.map((b) => (
                    <tr
                      key={b.id}
                      className="border-b border-zinc-900/60 hover:bg-zinc-800/50 transition-colors"
                    >
                      <td className="px-4 py-2.5 font-medium">{b.name}</td>
                      <td className="px-4 py-2.5 text-zinc-500 text-xs">
                        {formatRelative(b.created_at)}
                      </td>
                      <td className="px-4 py-2.5 text-right font-mono tabular-nums text-teal-300">
                        {formatMs(b.time_to_running_ms)}
                      </td>
                      <td className="px-4 py-2.5 text-right">
                        <SourceBadge poolHit={b.pool_hit} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </section>

        {/* System Performance */}
        <section>
          <h2 className="text-sm font-medium mb-2">System Performance</h2>
          <div className="flex items-center gap-2 text-xs text-zinc-500">
            <Zap size={13} className="text-teal-400 shrink-0" />
            <span>
              Cloud Hypervisor restore: ~160 ms p50 on EC2, ~115 ms on bare metal —
              6× faster than container-based sandbox baselines.
            </span>
          </div>
        </section>
      </div>
    </>
  )
}

// StatCard is a clean metric tile with a large teal number.
function StatCard({
  label,
  value,
  hint,
}: {
  label: string
  value: number | string
  hint?: string
}) {
  return (
    <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 shadow-lg shadow-black/20 p-4 hover:border-teal-500/30 transition-colors">
      <div className="text-[11px] text-zinc-500 uppercase tracking-wider font-mono">
        {label}
      </div>
      <div className="mt-2 text-3xl font-semibold tabular-nums text-teal-300">
        {value}
      </div>
      {hint && <div className="mt-1 text-[11px] text-zinc-500">{hint}</div>}
    </div>
  )
}

// SourceBadge marks whether a create was served from the warm pool.
function SourceBadge({ poolHit }: { poolHit: boolean }) {
  return poolHit ? (
    <span className="inline-flex items-center rounded px-1.5 py-0.5 text-[11px] font-medium bg-teal-500/10 text-teal-300 border border-teal-500/20">
      Pool
    </span>
  ) : (
    <span className="inline-flex items-center rounded px-1.5 py-0.5 text-[11px] font-medium bg-zinc-800 text-zinc-400 border border-zinc-700">
      Cold
    </span>
  )
}
