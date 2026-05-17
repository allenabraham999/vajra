import { useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { Boxes, Server } from 'lucide-react'
import {
  Bar,
  BarChart,
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import api from '../api/client'
import type {
  AdminNode,
  PoolNodeStats,
  PoolStats,
  PoolTemplateStats,
  Sandbox,
  Template,
} from '../api/types'
import PageHeader from '../components/PageHeader'
import EmptyState from '../components/EmptyState'
import { useAuth } from '../auth/AuthContext'
import { formatRelative } from '../utils/format'

const DAY_MS = 24 * 60 * 60 * 1000
const HOUR_MS = 60 * 60 * 1000

// Chart palette — kept in sync with the dashboard's teal/amber accents.
const TEAL = '#14b8a6'
const TEAL_LIGHT = '#2dd4bf'
const AMBER = '#f59e0b'

// Shared recharts tooltip styling for the dark theme.
const tooltipProps = {
  contentStyle: {
    background: '#18181b',
    border: '1px solid #3f3f46',
    borderRadius: 8,
    fontSize: 12,
  },
  labelStyle: { color: '#a1a1aa' },
  itemStyle: { color: '#e4e4e7' },
}

// formatMs renders a boot duration compactly.
function formatMs(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—'
  if (ms < 1000) return `${Math.round(ms)} ms`
  return `${(ms / 1000).toFixed(ms < 10000 ? 2 : 1)} s`
}

// median is the p50 of a numeric sample.
function median(xs: number[]): number {
  if (xs.length === 0) return NaN
  const s = [...xs].sort((a, b) => a - b)
  const m = Math.floor(s.length / 2)
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2
}

function within24h(iso: string): boolean {
  const t = new Date(iso).getTime()
  return Number.isFinite(t) && Date.now() - t <= DAY_MS
}

function clockTime(ts: number): string {
  return new Date(ts).toTimeString().slice(0, 8)
}

// Histogram bucket edges for the boot-time distribution chart.
const BUCKETS = [
  { label: '0–50ms', lo: 0, hi: 50 },
  { label: '50–100ms', lo: 50, hi: 100 },
  { label: '100–200ms', lo: 100, hi: 200 },
  { label: '200–500ms', lo: 200, hi: 500 },
  { label: '500ms+', lo: 500, hi: Infinity },
]

interface TickerEvent {
  key: string
  ts: number
  text: string
  kind: 'create' | 'destroy' | 'pool'
}

// bootText describes a sandbox create for the live ticker.
function bootText(s: Sandbox): string {
  if (typeof s.time_to_running_ms === 'number' && s.pool_hit !== undefined) {
    return `${s.name} created (${Math.round(s.time_to_running_ms)}ms, ${
      s.pool_hit ? 'pool hit' : 'cold'
    })`
  }
  return `${s.name} created`
}

export default function MetricsPage() {
  const { isAdmin } = useAuth()
  const [pool, setPool] = useState<PoolStats | null>(null)
  const [sandboxes, setSandboxes] = useState<Sandbox[]>([])
  const [templates, setTemplates] = useState<Template[]>([])
  const [nodes, setNodes] = useState<AdminNode[]>([])
  const [events, setEvents] = useState<TickerEvent[]>([])
  const [loaded, setLoaded] = useState(false)

  const prevStates = useRef<Map<string, string>>(new Map())
  // Total warm VMs across the whole cluster on the previous poll — drives
  // the "pool grew/shrank" ticker line.
  const prevPoolAvail = useRef<number | null>(null)
  // Per-(node,template) snapshot of the previous poll, keyed by
  // `${node_id}::${template_hash}`. Used to flash a row when its available
  // count or hits/hr ticks up between polls.
  const prevPoolRows = useRef<Map<string, { available: number; hits: number }>>(
    new Map(),
  )
  // Keys of rows that changed on the latest poll — drives the flash class.
  const [flashed, setFlashed] = useState<Set<string>>(new Set())
  const seeded = useRef(false)
  const evtSeq = useRef(0)

  // Sum the warm VMs across every node/template in a pool snapshot.
  function totalWarm(p: PoolStats | null): number {
    if (!p) return 0
    if (typeof p.global?.total_warm_vms === 'number') {
      return p.global.total_warm_vms
    }
    let n = 0
    for (const node of p.nodes ?? [])
      for (const t of node.templates ?? []) n += t.available
    return n
  }

  // poolRowMap flattens a pool snapshot to a per-(node,template) map so
  // consecutive polls can be diffed for the flash animation.
  function poolRowMap(
    p: PoolStats | null,
  ): Map<string, { available: number; hits: number }> {
    const m = new Map<string, { available: number; hits: number }>()
    for (const node of p?.nodes ?? []) {
      for (const t of node.templates ?? []) {
        m.set(`${node.node_id}::${t.template_hash}`, {
          available: t.available,
          hits: t.hits_last_hour,
        })
      }
    }
    return m
  }

  // Live poll — pool + sandboxes every 2s drives every section except
  // node health. The charts derive from this same data via useMemo, so a
  // single poll keeps the whole page in sync without extra fetches.
  useEffect(() => {
    let alive = true
    async function tick() {
      const [p, sb] = await Promise.all([
        api.pool.stats().catch(() => null),
        api.sandboxes.list().catch(() => [] as Sandbox[]),
      ])
      if (!alive) return
      setPool(p)
      setSandboxes(sb)
      setLoaded(true)

      if (!seeded.current) {
        // First poll: seed the ticker from recent history, without
        // emitting a "created" event for every pre-existing sandbox.
        const recent = [...sb]
          .sort((a, b) => +new Date(b.created_at) - +new Date(a.created_at))
          .slice(0, 8)
        setEvents(
          recent.map((s) => ({
            key: `seed-${s.id}`,
            ts: new Date(s.created_at).getTime(),
            text: bootText(s),
            kind: 'create' as const,
          })),
        )
        prevStates.current = new Map(sb.map((s) => [s.id, s.state]))
        prevPoolAvail.current = p ? totalWarm(p) : null
        prevPoolRows.current = poolRowMap(p)
        seeded.current = true
        return
      }

      // Subsequent polls: diff against the previous snapshot.
      const fresh: TickerEvent[] = []
      for (const s of sb) {
        const prev = prevStates.current.get(s.id)
        if (prev === undefined) {
          fresh.push({
            key: `e${evtSeq.current++}`,
            ts: Date.now(),
            text: bootText(s),
            kind: 'create',
          })
        } else if (prev !== 'DESTROYED' && s.state === 'DESTROYED') {
          fresh.push({
            key: `e${evtSeq.current++}`,
            ts: Date.now(),
            text: `${s.name} destroyed`,
            kind: 'destroy',
          })
        }
      }
      if (p) {
        const warm = totalWarm(p)
        if (prevPoolAvail.current !== null && warm !== prevPoolAvail.current) {
          const from = prevPoolAvail.current
          fresh.push({
            key: `e${evtSeq.current++}`,
            ts: Date.now(),
            text: `Pool ${warm > from ? 'grew' : 'shrank'}: ${from} → ${warm} ready`,
            kind: 'pool',
          })
        }
        // Flash any pool row whose available or hits/hr ticked up.
        const nextRows = poolRowMap(p)
        const lit = new Set<string>()
        for (const [key, cur] of nextRows) {
          const prev = prevPoolRows.current.get(key)
          if (prev && (cur.available > prev.available || cur.hits > prev.hits)) {
            lit.add(key)
          }
        }
        prevPoolRows.current = nextRows
        prevPoolAvail.current = warm
        if (lit.size) {
          setFlashed(lit)
          // Clear the flash so the CSS transition can replay next time.
          window.setTimeout(() => setFlashed(new Set()), 900)
        }
      }
      prevStates.current = new Map(sb.map((s) => [s.id, s.state]))
      if (fresh.length) setEvents((prev) => [...fresh, ...prev].slice(0, 12))
    }
    tick()
    const t = setInterval(tick, 2000)
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [])

  // Templates rarely change — fetch once for the recent-creates table.
  useEffect(() => {
    api.templates.list().then(setTemplates).catch(() => {})
  }, [])

  // Node health is admin-only and polled less aggressively.
  useEffect(() => {
    if (!isAdmin) return
    let alive = true
    const load = () =>
      api.admin
        .nodes()
        .then((n) => alive && setNodes(n))
        .catch(() => {})
    load()
    const t = setInterval(load, 30_000)
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [isAdmin])

  const templateName = useMemo(() => {
    const m = new Map<string, string>()
    for (const t of templates) m.set(t.id, t.name)
    return m
  }, [templates])

  // Hero stats — prefer the last 24h, but fall back to all-time so the
  // page always shows real numbers even on a quiet day.
  const stats = useMemo(() => {
    const recent = sandboxes.filter((s) => within24h(s.created_at))
    const set = recent.length > 0 ? recent : sandboxes
    const timed = (hit: boolean) =>
      set
        .filter(
          (s) => typeof s.time_to_running_ms === 'number' && s.pool_hit === hit,
        )
        .map((s) => s.time_to_running_ms as number)
    const poolTimes = timed(true)
    const coldTimes = timed(false)
    const definedHit = set.filter((s) => s.pool_hit !== undefined)
    const hitCount = definedHit.filter((s) => s.pool_hit).length
    const hitRate =
      definedHit.length > 0
        ? (hitCount / definedHit.length) * 100
        : pool?.global?.hit_rate_pct ?? 0
    return {
      p50Pool: median(poolTimes),
      p50Cold: median(coldTimes),
      poolSamples: poolTimes.length,
      coldSamples: coldTimes.length,
      hitRate,
      hitCount,
      hitTotal: definedHit.length,
      total24h: recent.length,
      totalAll: sandboxes.length,
      using24h: recent.length > 0,
    }
  }, [sandboxes, pool])

  // Boot-time histogram — bucketed counts split by pool hit vs cold.
  const histogram = useMemo(() => {
    const recent = sandboxes.filter((s) => within24h(s.created_at))
    const set = recent.length > 0 ? recent : sandboxes
    const rows = BUCKETS.map((b) => ({ label: b.label, poolHit: 0, cold: 0 }))
    for (const s of set) {
      const ms = s.time_to_running_ms
      if (typeof ms !== 'number') continue
      const idx = BUCKETS.findIndex((b) => ms >= b.lo && ms < b.hi)
      if (idx < 0) continue
      if (s.pool_hit) rows[idx].poolHit++
      else rows[idx].cold++
    }
    return rows
  }, [sandboxes])

  const histoTotal = histogram.reduce((a, r) => a + r.poolHit + r.cold, 0)

  // 24h creation timeline — one bucket per hour.
  const timeline = useMemo(() => {
    const top = new Date()
    top.setMinutes(0, 0, 0)
    const buckets = Array.from({ length: 24 }, (_, i) => {
      const d = new Date(top.getTime() - (23 - i) * HOUR_MS)
      return {
        start: d.getTime(),
        label: `${String(d.getHours()).padStart(2, '0')}:00`,
        count: 0,
      }
    })
    for (const s of sandboxes) {
      const t = new Date(s.created_at).getTime()
      if (!Number.isFinite(t)) continue
      const idx = Math.floor((t - buckets[0].start) / HOUR_MS)
      if (idx >= 0 && idx < buckets.length) buckets[idx].count++
    }
    return buckets
  }, [sandboxes])

  const recentCreates = useMemo(
    () =>
      [...sandboxes]
        .sort((a, b) => +new Date(b.created_at) - +new Date(a.created_at))
        .slice(0, 10),
    [sandboxes],
  )

  // poolNodes is every node carrying a non-empty template list — these are
  // the rows rendered by the Live Pool Status section.
  const poolNodes = useMemo<PoolNodeStats[]>(
    () => (pool?.nodes ?? []).filter((n) => (n.templates ?? []).length > 0),
    [pool],
  )
  const hasPool = poolNodes.length > 0
  const g = pool?.global

  // --- render ---------------------------------------------------------

  if (!loaded) {
    return (
      <>
        <PageHeader
          title="Metrics"
          description="Live sandbox performance — boot times, pool health, and throughput."
        />
        <div className="p-6 space-y-8">
          <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
            {[0, 1, 2, 3].map((i) => (
              <Skeleton key={i} className="h-32 rounded-xl" />
            ))}
          </div>
          <Skeleton className="h-40 rounded-lg" />
          <Skeleton className="h-72 rounded-lg" />
        </div>
      </>
    )
  }

  if (sandboxes.length === 0) {
    return (
      <>
        <PageHeader
          title="Metrics"
          description="Live sandbox performance — boot times, pool health, and throughput."
        />
        <div className="p-6">
          <EmptyState
            icon={<Boxes size={32} />}
            title="No metrics yet"
            description="Create your first sandbox to see live boot times, pool hit rate, and throughput here."
            action={
              <Link
                to="/sandboxes"
                className="inline-flex items-center gap-1.5 rounded-md bg-teal-500 hover:bg-teal-400 text-zinc-950 shadow-md shadow-teal-500/20 hover:shadow-teal-500/40 transition-all duration-200 hover:scale-[1.02] px-3 py-1.5 text-sm font-medium"
              >
                Create your first sandbox
              </Link>
            }
          />
        </div>
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Metrics"
        description="Live sandbox performance — boot times, pool health, and throughput."
      />
      <div className="p-6 space-y-8">
        {/* SECTION 1 — Hero performance numbers */}
        <section className="grid grid-cols-2 lg:grid-cols-4 gap-4">
          <HeroCard
            label="P50 Boot Time"
            value={stats.p50Pool}
            suffix="ms"
            subtitle={`Warm pool hits · ${stats.using24h ? 'last 24h' : 'all time'}`}
            live
          />
          <HeroCard
            label="P50 Cold Restore"
            value={stats.p50Cold}
            suffix="ms"
            subtitle="Snapshot restore when pool is empty"
          />
          <HeroCard
            label="Pool Hit Rate"
            value={stats.hitRate}
            suffix="%"
            subtitle={`${stats.hitCount} of ${stats.hitTotal} creates hit the warm pool`}
            live
          />
          <HeroCard
            label="Sandboxes Created"
            value={stats.total24h}
            subtitle={`Last 24h · ${stats.totalAll} all-time`}
          />
        </section>

        {/* SECTION 2 — Live pool status + event ticker */}
        <section>
          <SectionTitle live>Live Pool Status</SectionTitle>
          <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 shadow-lg shadow-black/20 p-4">
            {/* Global summary across the whole cluster */}
            {g && (
              <div className="grid grid-cols-3 gap-3 mb-4">
                <PoolStat
                  label="Warm VMs"
                  value={`${g.total_warm_vms} / ${g.total_capacity}`}
                />
                <PoolStat
                  label="Hit rate (24h)"
                  value={`${(g.hit_rate_pct ?? 0).toFixed(1)}%`}
                  accent
                />
                <PoolStat
                  label="Hits / misses (24h)"
                  value={`${g.total_hits_24h} / ${g.total_misses_24h}`}
                />
              </div>
            )}

            {hasPool ? (
              <div className="space-y-4">
                {poolNodes.map((node) => (
                  <div key={node.node_id}>
                    <div className="flex items-center justify-between text-[11px] mb-1.5">
                      <span className="font-mono text-zinc-400 flex items-center gap-1.5">
                        <Server size={11} className="text-zinc-600" />
                        {node.node_id}
                      </span>
                      <span className="text-zinc-600 font-mono">
                        capacity {node.capacity}
                      </span>
                    </div>
                    <div className="space-y-2">
                      {(node.templates ?? []).map((t) => (
                        <PoolTemplateRow
                          key={t.template_hash}
                          tpl={t}
                          flash={flashed.has(
                            `${node.node_id}::${t.template_hash}`,
                          )}
                        />
                      ))}
                    </div>
                  </div>
                ))}
              </div>
            ) : (
              <div className="text-sm text-zinc-500">
                No pre-warm pool configured on the cluster.
              </div>
            )}

            <div className="mt-4 border-t border-zinc-900 pt-3">
              <div className="text-[11px] text-zinc-500 uppercase tracking-wider font-mono mb-2">
                Live events
              </div>
              {events.length === 0 ? (
                <div className="text-xs text-zinc-600 py-2">
                  Waiting for sandbox activity…
                </div>
              ) : (
                <ul className="space-y-1 font-mono text-xs">
                  {events.map((e) => (
                    <li
                      key={e.key}
                      className="flex gap-2 animate-fade-in"
                    >
                      <span className="text-zinc-600 shrink-0">
                        {clockTime(e.ts)}
                      </span>
                      <span className="text-zinc-600">→</span>
                      <span className={tickerColor(e.kind)}>{e.text}</span>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          </div>
        </section>

        {/* SECTION 3 — Boot-time distribution */}
        <section>
          <SectionTitle>Boot Time Distribution</SectionTitle>
          <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 shadow-lg shadow-black/20 p-4">
            <div className="flex items-center gap-4 mb-3 text-[11px]">
              <LegendDot color={TEAL} label="Pool hit" />
              <LegendDot color={AMBER} label="Cold restore" />
            </div>
            {histoTotal === 0 ? (
              <div className="h-[200px] grid place-items-center text-sm text-zinc-600">
                No boot times recorded yet.
              </div>
            ) : (
              <ResponsiveContainer width="100%" height={240}>
                <BarChart
                  data={histogram}
                  margin={{ top: 4, right: 8, bottom: 0, left: -16 }}
                >
                  <CartesianGrid
                    strokeDasharray="3 3"
                    stroke="#27272a"
                    vertical={false}
                  />
                  <XAxis
                    dataKey="label"
                    tick={{ fill: '#71717a', fontSize: 11 }}
                    tickLine={false}
                    axisLine={{ stroke: '#27272a' }}
                  />
                  <YAxis
                    allowDecimals={false}
                    tick={{ fill: '#71717a', fontSize: 11 }}
                    tickLine={false}
                    axisLine={false}
                  />
                  <Tooltip {...tooltipProps} cursor={{ fill: 'rgba(20,184,166,0.06)' }} />
                  <Bar
                    dataKey="poolHit"
                    name="Pool hit"
                    stackId="a"
                    fill={TEAL}
                    isAnimationActive={false}
                  />
                  <Bar
                    dataKey="cold"
                    name="Cold restore"
                    stackId="a"
                    fill={AMBER}
                    radius={[3, 3, 0, 0]}
                    isAnimationActive={false}
                  />
                </BarChart>
              </ResponsiveContainer>
            )}
          </div>
        </section>

        {/* SECTION 4 — Creation timeline */}
        <section>
          <SectionTitle>Sandbox Creation — Last 24h</SectionTitle>
          <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 shadow-lg shadow-black/20 p-4">
            <ResponsiveContainer width="100%" height={220}>
              <LineChart
                data={timeline}
                margin={{ top: 4, right: 12, bottom: 0, left: -16 }}
              >
                <CartesianGrid
                  strokeDasharray="3 3"
                  stroke="#27272a"
                  vertical={false}
                />
                <XAxis
                  dataKey="label"
                  tick={{ fill: '#71717a', fontSize: 11 }}
                  tickLine={false}
                  axisLine={{ stroke: '#27272a' }}
                  interval={3}
                />
                <YAxis
                  allowDecimals={false}
                  tick={{ fill: '#71717a', fontSize: 11 }}
                  tickLine={false}
                  axisLine={false}
                />
                <Tooltip {...tooltipProps} cursor={{ stroke: '#3f3f46' }} />
                <Line
                  type="monotone"
                  dataKey="count"
                  name="Sandboxes created"
                  stroke={TEAL_LIGHT}
                  strokeWidth={2}
                  dot={false}
                  activeDot={{ r: 4, fill: TEAL_LIGHT }}
                  isAnimationActive={false}
                />
              </LineChart>
            </ResponsiveContainer>
          </div>
        </section>

        {/* SECTION 5 — Node health (admin only) */}
        {isAdmin && (
          <section>
            <SectionTitle>Node Health</SectionTitle>
            <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 shadow-lg shadow-black/20 overflow-hidden">
              {nodes.length === 0 ? (
                <div className="p-6 text-center text-sm text-zinc-500 flex items-center justify-center gap-2">
                  <Server size={14} /> No nodes reporting.
                </div>
              ) : (
                <table className="w-full text-sm">
                  <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                    <tr className="border-b border-zinc-900 bg-zinc-950/40">
                      <th className="text-left font-medium px-4 py-2.5">Node</th>
                      <th className="text-left font-medium px-4 py-2.5">CPU</th>
                      <th className="text-left font-medium px-4 py-2.5">Memory</th>
                      <th className="text-right font-medium px-4 py-2.5">
                        Sandboxes
                      </th>
                      <th className="text-right font-medium px-4 py-2.5">
                        Health
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {nodes.map((n) => (
                      <tr
                        key={n.id}
                        className="border-b border-zinc-900/60 hover:bg-zinc-800/50 transition-colors"
                      >
                        <td className="px-4 py-2.5 font-medium">
                          {n.hostname}
                          <div className="text-[11px] text-zinc-600 font-mono">
                            {n.ip}
                          </div>
                        </td>
                        <td className="px-4 py-2.5 w-44">
                          <UsageBar
                            used={n.used_resources.used_cpu}
                            total={n.capacity.total_cpu}
                            unit="vCPU"
                          />
                        </td>
                        <td className="px-4 py-2.5 w-44">
                          <UsageBar
                            used={n.used_resources.used_memory_mb}
                            total={n.capacity.total_memory_mb}
                            unit="MB"
                          />
                        </td>
                        <td className="px-4 py-2.5 text-right tabular-nums text-zinc-300">
                          {n.sandbox_count}
                        </td>
                        <td className="px-4 py-2.5 text-right">
                          <HealthBadge healthy={n.healthy} state={n.state} />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          </section>
        )}

        {/* SECTION 6 — Recent creates with timing */}
        <section>
          <SectionTitle live>Recent Creates</SectionTitle>
          <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 shadow-lg shadow-black/20 overflow-hidden">
            <table className="w-full text-sm">
              <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                <tr className="border-b border-zinc-900 bg-zinc-950/40">
                  <th className="text-left font-medium px-4 py-2.5">Sandbox</th>
                  <th className="text-left font-medium px-4 py-2.5">Template</th>
                  <th className="text-right font-medium px-4 py-2.5">
                    Boot time
                  </th>
                  <th className="text-right font-medium px-4 py-2.5">Type</th>
                  <th className="text-right font-medium px-4 py-2.5">Created</th>
                </tr>
              </thead>
              <tbody>
                {recentCreates.map((s) => (
                  <tr
                    key={s.id}
                    className="border-b border-zinc-900/60 hover:bg-zinc-800/50 transition-colors"
                  >
                    <td className="px-4 py-2.5 font-medium">
                      {s.name}
                      <div className="text-[11px] text-zinc-600 font-mono">
                        {s.id.slice(0, 12)}
                      </div>
                    </td>
                    <td className="px-4 py-2.5 text-zinc-400 text-xs">
                      {templateName.get(s.template_id) ??
                        `${s.template_id.slice(0, 12)}…`}
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums text-teal-300">
                      {typeof s.time_to_running_ms === 'number'
                        ? formatMs(s.time_to_running_ms)
                        : '—'}
                    </td>
                    <td className="px-4 py-2.5 text-right">
                      <SourceBadge poolHit={s.pool_hit} />
                    </td>
                    <td className="px-4 py-2.5 text-right text-zinc-500 text-xs">
                      {formatRelative(s.created_at)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      </div>
    </>
  )
}

// --- presentational components ----------------------------------------

// useCountUp eases a number from its previous value to the target,
// re-running only when the target actually changes.
function useCountUp(target: number, duration = 700): number {
  const [val, setVal] = useState(0)
  const from = useRef(0)
  const raf = useRef(0)
  useEffect(() => {
    if (!Number.isFinite(target)) {
      setVal(0)
      return
    }
    const start = performance.now()
    const begin = from.current
    const step = (now: number) => {
      const t = Math.min(1, (now - start) / duration)
      const eased = 1 - Math.pow(1 - t, 3)
      setVal(begin + (target - begin) * eased)
      if (t < 1) raf.current = requestAnimationFrame(step)
      else from.current = target
    }
    cancelAnimationFrame(raf.current)
    raf.current = requestAnimationFrame(step)
    return () => cancelAnimationFrame(raf.current)
  }, [target, duration])
  return val
}

function HeroCard({
  label,
  value,
  suffix = '',
  decimals = 0,
  subtitle,
  live = false,
}: {
  label: string
  value: number
  suffix?: string
  decimals?: number
  subtitle: string
  live?: boolean
}) {
  const animated = useCountUp(value)
  const ok = Number.isFinite(value)
  return (
    <div className="relative group rounded-xl border border-zinc-800 bg-gradient-to-br from-zinc-900/90 to-zinc-950 shadow-lg shadow-black/30 p-5 transition-all duration-200 hover:-translate-y-0.5 hover:border-teal-500/40 hover:shadow-teal-500/10">
      {live && (
        <span className="absolute top-4 right-4 flex size-2">
          <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-teal-400 opacity-60" />
          <span className="relative inline-flex size-2 rounded-full bg-teal-400" />
        </span>
      )}
      <div className="text-[11px] text-zinc-500 uppercase tracking-wider font-mono">
        {label}
      </div>
      <div className="mt-2 text-4xl font-semibold tabular-nums text-teal-300">
        {ok ? animated.toFixed(decimals) : '—'}
        {ok && suffix && (
          <span className="text-2xl text-teal-400/60 ml-0.5">{suffix}</span>
        )}
      </div>
      <div className="mt-1.5 text-[11px] text-zinc-500">{subtitle}</div>
    </div>
  )
}

function SectionTitle({
  children,
  live = false,
}: {
  children: React.ReactNode
  live?: boolean
}) {
  return (
    <h2 className="text-sm font-medium mb-3 flex items-center gap-2">
      {children}
      {live && (
        <span className="inline-flex items-center gap-1 text-[10px] font-mono uppercase tracking-wider text-teal-400">
          <span className="size-1.5 rounded-full bg-teal-400 animate-pulse" />
          live
        </span>
      )}
    </h2>
  )
}

function Skeleton({ className = '' }: { className?: string }) {
  return <div className={`animate-pulse bg-zinc-800/40 ${className}`} />
}

function LegendDot({ color, label }: { color: string; label: string }) {
  return (
    <span className="inline-flex items-center gap-1.5 text-zinc-500">
      <span
        className="size-2.5 rounded-sm"
        style={{ background: color }}
      />
      {label}
    </span>
  )
}

// PoolStat is one cell of the cluster-wide warm-pool summary grid.
function PoolStat({
  label,
  value,
  accent = false,
}: {
  label: string
  value: string
  accent?: boolean
}) {
  return (
    <div className="rounded-md border border-zinc-800 bg-zinc-950/40 px-3 py-2">
      <div className="text-[10px] text-zinc-500 uppercase tracking-wider font-mono">
        {label}
      </div>
      <div
        className={`mt-0.5 text-lg font-semibold tabular-nums ${
          accent ? 'text-teal-300' : 'text-zinc-200'
        }`}
      >
        {value}
      </div>
    </div>
  )
}

// PoolTemplateRow renders one template's warm-pool state on one node: a
// stacked bar of available vs in-use against target_size, plus hits/hr.
// `flash` lights up the row briefly when its pool grew or got a fresh hit.
function PoolTemplateRow({
  tpl,
  flash,
}: {
  tpl: PoolTemplateStats
  flash: boolean
}) {
  // The bar spans the larger of target_size or the live available+in-use,
  // so an over-target pool never overflows its track.
  const span = Math.max(tpl.target_size, tpl.available + tpl.in_use, 1)
  const availPct = (tpl.available / span) * 100
  const inUsePct = (tpl.in_use / span) * 100
  // Target marker — only shown when the pool can still grow into it.
  const targetPct = Math.min(100, (tpl.target_size / span) * 100)
  return (
    <div
      className={`rounded-md border px-3 py-2 transition-colors duration-700 ${
        flash
          ? 'border-teal-500/50 bg-teal-500/10'
          : 'border-zinc-800/70 bg-zinc-900/30'
      }`}
    >
      <div className="flex items-center justify-between text-xs mb-1.5">
        <span className="font-medium text-zinc-200">{tpl.template_name}</span>
        <span className="text-zinc-500 font-mono tabular-nums">
          <span className="text-teal-300">{tpl.available}</span> ready ·{' '}
          <span className="text-amber-300">{tpl.in_use}</span> in use · target{' '}
          {tpl.target_size}
        </span>
      </div>
      <div className="relative flex h-2.5 w-full overflow-hidden rounded-full bg-zinc-800">
        <div
          className="bg-teal-500 transition-all duration-500"
          style={{ width: `${availPct}%` }}
        />
        <div
          className="bg-amber-500 transition-all duration-500"
          style={{ width: `${inUsePct}%` }}
        />
        {targetPct < 100 && (
          <div
            className="absolute top-0 bottom-0 w-px bg-zinc-400/70"
            style={{ left: `${targetPct}%` }}
            title={`target ${tpl.target_size}`}
          />
        )}
      </div>
      <div className="mt-1 flex items-center justify-between text-[10px] text-zinc-500 font-mono">
        <span>
          {tpl.warming > 0 ? `${tpl.warming} warming · ` : ''}
          <span className={flash ? 'text-teal-300' : ''}>
            {tpl.hits_last_hour} hits/hr
          </span>
        </span>
        <span>
          {tpl.last_hit_at
            ? `last hit ${formatRelative(tpl.last_hit_at)}`
            : 'no hits yet'}
        </span>
      </div>
    </div>
  )
}

function tickerColor(kind: TickerEvent['kind']): string {
  if (kind === 'create') return 'text-teal-300'
  if (kind === 'destroy') return 'text-zinc-400'
  return 'text-amber-300'
}

function SourceBadge({ poolHit }: { poolHit?: boolean }) {
  if (poolHit === undefined) {
    return <span className="text-zinc-600 text-xs">—</span>
  }
  return poolHit ? (
    <span className="inline-flex items-center rounded px-1.5 py-0.5 text-[11px] font-medium bg-teal-500/10 text-teal-300 border border-teal-500/20">
      Pool hit
    </span>
  ) : (
    <span className="inline-flex items-center rounded px-1.5 py-0.5 text-[11px] font-medium bg-amber-500/10 text-amber-300 border border-amber-500/20">
      Cold
    </span>
  )
}

function UsageBar({
  used,
  total,
  unit,
}: {
  used: number
  total: number
  unit: string
}) {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0
  const hot = pct >= 85
  return (
    <div>
      <div className="h-1.5 w-full rounded-full bg-zinc-800 overflow-hidden">
        <div
          className={`h-full rounded-full transition-all duration-500 ${
            hot ? 'bg-amber-500' : 'bg-teal-500'
          }`}
          style={{ width: `${pct}%` }}
        />
      </div>
      <div className="mt-1 text-[10px] text-zinc-600 font-mono tabular-nums">
        {Math.round(used)} / {total} {unit}
      </div>
    </div>
  )
}

function HealthBadge({ healthy, state }: { healthy: boolean; state: string }) {
  return healthy ? (
    <span className="inline-flex items-center rounded px-1.5 py-0.5 text-[11px] font-medium bg-emerald-500/15 text-emerald-400 border border-emerald-500/30">
      Healthy
    </span>
  ) : (
    <span className="inline-flex items-center rounded px-1.5 py-0.5 text-[11px] font-medium bg-red-500/15 text-red-400 border border-red-500/30">
      {state === 'ACTIVE' ? 'Stale' : state}
    </span>
  )
}
