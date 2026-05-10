import { useEffect, useMemo, useState } from 'react'
import { DollarSign } from 'lucide-react'
import api, { ApiError } from '../api/client'
import type { Sandbox, UsageResponse, UsageRow } from '../api/types'
import PageHeader from '../components/PageHeader'
import { memMB } from '../utils/format'

const FREE_CREDITS_USD = 200
const VCPU_RATE_PER_HOUR = 0.06
const MEM_RATE_PER_GB_HOUR = 0.01
const STORAGE_RATE_PER_GB_HOUR = 0.005

interface DailyUsage {
  date: string
  cost: number
}

export default function UsagePage() {
  const [data, setData] = useState<UsageResponse | null>(null)
  const [sandboxes, setSandboxes] = useState<Sandbox[]>([])

  useEffect(() => {
    api.usage
      .get()
      .then(setData)
      .catch((err) => {
        // The server's /v1/usage is a stub; if it 404s we'll synthesise locally.
        if (err instanceof ApiError && (err.status === 404 || err.status === 501)) {
          setData(null)
        }
      })
    api.sandboxes.list().then(setSandboxes).catch(() => {})
  }, [])

  const local = useMemo<UsageResponse>(() => {
    const now = Date.now()
    const rows: UsageRow[] = sandboxes.map((sb) => {
      const start = new Date(sb.created_at).getTime()
      const isLive = sb.state === 'RUNNING' || sb.state === 'PAUSED' || sb.state === 'STOPPED'
      const end = isLive ? now : new Date(sb.updated_at).getTime()
      const hours = Math.max(0, (end - start) / 3_600_000)
      const cpuCost = sb.config.vcpus * hours * VCPU_RATE_PER_HOUR
      const memCost = (sb.config.memory_mb / 1024) * hours * MEM_RATE_PER_GB_HOUR
      const diskCost = sb.config.disk_gb * hours * STORAGE_RATE_PER_GB_HOUR
      const cost = cpuCost + memCost + diskCost
      return {
        sandbox_id: sb.id,
        sandbox_name: sb.name,
        vcpus: sb.config.vcpus,
        memory_mb: sb.config.memory_mb,
        disk_gb: sb.config.disk_gb,
        duration_hours: hours,
        cost_usd: cost,
      }
    })
    const totalCost = rows.reduce((a, r) => a + r.cost_usd, 0)
    const vcpuHours = rows.reduce((a, r) => a + r.vcpus * r.duration_hours, 0)
    const memGBHours = rows.reduce((a, r) => a + (r.memory_mb / 1024) * r.duration_hours, 0)
    const diskGBHours = rows.reduce((a, r) => a + r.disk_gb * r.duration_hours, 0)
    return {
      rows,
      total_cost_usd: totalCost,
      vcpu_hours: vcpuHours,
      memory_gb_hours: memGBHours,
      storage_gb_hours: diskGBHours,
    }
  }, [sandboxes])

  const view = data ?? local

  const daily = useMemo<DailyUsage[]>(() => {
    const buckets = new Map<string, number>()
    const today = new Date()
    today.setHours(0, 0, 0, 0)
    for (let i = 29; i >= 0; i--) {
      const d = new Date(today)
      d.setDate(d.getDate() - i)
      buckets.set(d.toISOString().slice(0, 10), 0)
    }
    sandboxes.forEach((sb) => {
      const start = new Date(sb.created_at)
      const end =
        sb.state === 'DESTROYED' || sb.state === 'ERROR'
          ? new Date(sb.updated_at)
          : new Date()
      const cur = new Date(start)
      cur.setHours(0, 0, 0, 0)
      while (cur <= end) {
        const key = cur.toISOString().slice(0, 10)
        if (buckets.has(key)) {
          const dayStart = new Date(cur)
          const dayEnd = new Date(cur)
          dayEnd.setDate(dayEnd.getDate() + 1)
          const lo = Math.max(start.getTime(), dayStart.getTime())
          const hi = Math.min(end.getTime(), dayEnd.getTime())
          const hours = Math.max(0, (hi - lo) / 3_600_000)
          const cost =
            sb.config.vcpus * hours * VCPU_RATE_PER_HOUR +
            (sb.config.memory_mb / 1024) * hours * MEM_RATE_PER_GB_HOUR +
            sb.config.disk_gb * hours * STORAGE_RATE_PER_GB_HOUR
          buckets.set(key, (buckets.get(key) ?? 0) + cost)
        }
        cur.setDate(cur.getDate() + 1)
      }
    })
    return Array.from(buckets.entries()).map(([date, cost]) => ({ date, cost }))
  }, [sandboxes])

  const remaining = Math.max(0, FREE_CREDITS_USD - view.total_cost_usd)
  const maxDaily = Math.max(0.001, ...daily.map((d) => d.cost))

  return (
    <>
      <PageHeader
        title="Usage & Billing"
        description="Resource consumption + estimated cost. Currently running on free credits."
      />
      <div className="p-6 space-y-6">
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
          <BillingTile label="Credits remaining" value={`$${remaining.toFixed(2)}`} subtle={`of $${FREE_CREDITS_USD.toFixed(0)} free`} />
          <BillingTile label="Total spend" value={`$${view.total_cost_usd.toFixed(2)}`} />
          <BillingTile label="vCPU-hours" value={view.vcpu_hours.toFixed(2)} subtle={`@ $${VCPU_RATE_PER_HOUR}/hr`} />
          <BillingTile label="Memory GB-hours" value={view.memory_gb_hours.toFixed(2)} subtle={`@ $${MEM_RATE_PER_GB_HOUR}/GB/hr`} />
        </div>

        <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 p-4">
          <div className="flex items-center justify-between mb-3">
            <h2 className="text-sm font-medium">Daily spend (last 30 days)</h2>
            <span className="text-[11px] text-zinc-500 font-mono">
              max ${maxDaily.toFixed(2)}
            </span>
          </div>
          <div className="flex items-end gap-px h-32">
            {daily.map((d) => (
              <div
                key={d.date}
                className="flex-1 flex flex-col justify-end group"
                title={`${d.date}: $${d.cost.toFixed(4)}`}
              >
                <div
                  className="bg-emerald-500/40 group-hover:bg-emerald-400 transition-colors rounded-t-sm"
                  style={{ height: `${Math.max(2, (d.cost / maxDaily) * 100)}%` }}
                />
              </div>
            ))}
          </div>
          <div className="flex justify-between text-[10px] text-zinc-600 font-mono mt-1">
            <span>{daily[0]?.date.slice(5)}</span>
            <span>{daily[daily.length - 1]?.date.slice(5)}</span>
          </div>
        </div>

        <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 overflow-hidden">
          <div className="flex items-center justify-between px-4 py-3 border-b border-zinc-900">
            <h2 className="text-sm font-medium">Per-sandbox</h2>
            <DollarSign size={14} className="text-zinc-500" />
          </div>
          {view.rows.length === 0 ? (
            <div className="p-8 text-center text-xs text-zinc-500">
              No usage yet. Create a sandbox to start tracking.
            </div>
          ) : (
            <table className="w-full text-sm">
              <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                <tr className="border-b border-zinc-900">
                  <th className="text-left font-medium px-4 py-2">Sandbox</th>
                  <th className="text-left font-medium px-4 py-2">Resources</th>
                  <th className="text-right font-medium px-4 py-2">Hours</th>
                  <th className="text-right font-medium px-4 py-2">Cost</th>
                </tr>
              </thead>
              <tbody>
                {view.rows.map((r) => (
                  <tr key={r.sandbox_id} className="border-b border-zinc-900/50 hover:bg-zinc-900/40">
                    <td className="px-4 py-2 font-medium">{r.sandbox_name}</td>
                    <td className="px-4 py-2 text-zinc-400 text-xs font-mono">
                      {r.vcpus} vCPU · {memMB(r.memory_mb)} · {r.disk_gb} GB
                    </td>
                    <td className="px-4 py-2 text-right text-zinc-300 font-mono text-xs tabular-nums">
                      {r.duration_hours.toFixed(2)}
                    </td>
                    <td className="px-4 py-2 text-right text-emerald-300 font-mono text-xs tabular-nums">
                      ${r.cost_usd.toFixed(4)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </>
  )
}

function BillingTile({ label, value, subtle }: { label: string; value: string; subtle?: string }) {
  return (
    <div className="rounded-lg border border-zinc-900 bg-zinc-900/40 p-4">
      <div className="text-[11px] text-zinc-500 uppercase tracking-wider font-mono">{label}</div>
      <div className="mt-2 text-2xl font-semibold tabular-nums">{value}</div>
      {subtle && <div className="mt-1 text-[11px] text-zinc-500">{subtle}</div>}
    </div>
  )
}
