import { useEffect, useMemo, useState } from 'react'
import api from '../api/client'
import type { Node, Sandbox } from '../api/types'
import PageHeader from '../components/PageHeader'
import ProgressBar from '../components/ProgressBar'
import { memMB } from '../utils/format'

// Boot-time numbers from bible.md, surfaced for the demo.
const BOOT_STATS = {
  ec2_avg_ms: 161,
  ec2_p50_ms: 158,
  ec2_p95_ms: 176,
  ec2_p99_ms: 176,
  baremetal_target_ms: 100,
}

export default function MetricsPage() {
  const [sandboxes, setSandboxes] = useState<Sandbox[]>([])
  const [nodes, setNodes] = useState<Node[]>([])

  useEffect(() => {
    let alive = true
    async function load() {
      const [s, n] = await Promise.all([
        api.sandboxes.list().catch(() => []),
        api.nodes.list().catch(() => []),
      ])
      if (!alive) return
      setSandboxes(s)
      setNodes(n)
    }
    load()
    const t = setInterval(load, 5000)
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [])

  const active = useMemo(
    () => sandboxes.filter((s) => s.state === 'RUNNING').length,
    [sandboxes],
  )

  return (
    <>
      <PageHeader
        title="Metrics"
        description="Boot-time benchmarks and live cluster utilization."
      />
      <div className="p-6 space-y-6">
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
          <Tile label="Active sandboxes" value={active} />
          <Tile label="Snapshot restore (p50)" value={`${BOOT_STATS.ec2_p50_ms} ms`} subtle="EC2 nested virt" />
          <Tile label="Snapshot restore (p95)" value={`${BOOT_STATS.ec2_p95_ms} ms`} subtle="EC2 nested virt" />
          <Tile label="Bare-metal target" value={`${BOOT_STATS.baremetal_target_ms} ms`} subtle="no nested overhead" />
        </div>

        <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 p-4">
          <h2 className="text-sm font-medium mb-1">Boot time distribution</h2>
          <p className="text-[11px] text-zinc-500 mb-3">
            10 consecutive Cloud Hypervisor restore + destroy cycles, EC2 c8i.large with nested
            virtualization. From bible.md.
          </p>
          <div className="grid grid-cols-1 md:grid-cols-2 gap-x-8 gap-y-2 text-sm">
            <Metric label="min" value="152.75 ms" />
            <Metric label="avg" value={`${BOOT_STATS.ec2_avg_ms} ms`} />
            <Metric label="p50" value={`${BOOT_STATS.ec2_p50_ms} ms`} />
            <Metric label="p95" value={`${BOOT_STATS.ec2_p95_ms} ms`} />
            <Metric label="p99" value={`${BOOT_STATS.ec2_p99_ms} ms`} />
            <Metric label="max" value={`${BOOT_STATS.ec2_p99_ms} ms`} />
          </div>
          <p className="text-[11px] text-zinc-500 mt-4">
            6× faster than the legacy Incus container baseline (~1000 ms).
          </p>
        </div>

        <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 p-4">
          <h2 className="text-sm font-medium mb-3">Node capacity utilization</h2>
          {nodes.length === 0 ? (
            <p className="text-xs text-zinc-500">No node visibility (admin-only endpoint).</p>
          ) : (
            <div className="space-y-3">
              {nodes.map((n) => (
                <div key={n.id} className="grid grid-cols-1 md:grid-cols-3 gap-4 items-center">
                  <div>
                    <div className="text-sm font-medium">{n.hostname}</div>
                    <div className="text-[11px] text-zinc-500 font-mono">{n.ip}</div>
                  </div>
                  <ProgressBar
                    value={n.used_resources.used_cpu}
                    max={n.capacity.total_cpu}
                    label={`CPU ${n.used_resources.used_cpu}/${n.capacity.total_cpu}`}
                  />
                  <ProgressBar
                    value={n.used_resources.used_memory_mb}
                    max={n.capacity.total_memory_mb}
                    label={`Mem ${memMB(n.used_resources.used_memory_mb)}/${memMB(n.capacity.total_memory_mb)}`}
                  />
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </>
  )
}

function Tile({ label, value, subtle }: { label: string; value: number | string; subtle?: string }) {
  return (
    <div className="rounded-lg border border-zinc-900 bg-zinc-900/40 p-4">
      <div className="text-[11px] text-zinc-500 uppercase tracking-wider font-mono">{label}</div>
      <div className="mt-2 text-2xl font-semibold tabular-nums">{value}</div>
      {subtle && <div className="mt-1 text-[11px] text-zinc-500">{subtle}</div>}
    </div>
  )
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between border-b border-zinc-900/60 py-1">
      <span className="text-[11px] text-zinc-500 uppercase tracking-wider font-mono">{label}</span>
      <span className="font-mono text-zinc-200 tabular-nums">{value}</span>
    </div>
  )
}
