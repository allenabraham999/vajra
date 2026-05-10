import { useEffect, useMemo, useState } from 'react'
import { ShieldCheck } from 'lucide-react'
import api from '../api/client'
import type { Cluster, Node, Sandbox } from '../api/types'
import PageHeader from '../components/PageHeader'
import StateBadge from '../components/StateBadge'
import ProgressBar from '../components/ProgressBar'
import { useToast } from '../components/Toast'
import { memMB } from '../utils/format'

export default function AdminPage() {
  const toast = useToast()
  const [clusters, setClusters] = useState<Cluster[]>([])
  const [nodes, setNodes] = useState<Node[]>([])
  const [sandboxes, setSandboxes] = useState<Sandbox[]>([])

  useEffect(() => {
    let alive = true
    async function load() {
      try {
        const [c, n, s] = await Promise.all([
          api.clusters.list(),
          api.nodes.list(),
          api.sandboxes.list(),
        ])
        if (!alive) return
        setClusters(c)
        setNodes(n)
        setSandboxes(s)
      } catch (err) {
        if (alive) toast.error(err instanceof Error ? err.message : 'admin load failed')
      }
    }
    load()
    const t = setInterval(load, 8000)
    return () => {
      alive = false
      clearInterval(t)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const totals = useMemo(() => {
    const cap = nodes.reduce(
      (acc, n) => ({
        cpu: acc.cpu + n.capacity.total_cpu,
        mem: acc.mem + n.capacity.total_memory_mb,
        disk: acc.disk + n.capacity.total_disk_gb,
      }),
      { cpu: 0, mem: 0, disk: 0 },
    )
    const used = nodes.reduce(
      (acc, n) => ({
        cpu: acc.cpu + n.used_resources.used_cpu,
        mem: acc.mem + n.used_resources.used_memory_mb,
        disk: acc.disk + n.used_resources.used_disk_gb,
      }),
      { cpu: 0, mem: 0, disk: 0 },
    )
    return { cap, used }
  }, [nodes])

  const running = sandboxes.filter((s) => s.state === 'RUNNING').length

  const byCluster = useMemo(() => {
    const map = new Map<string, Node[]>()
    nodes.forEach((n) => {
      const arr = map.get(n.cluster_id) ?? []
      arr.push(n)
      map.set(n.cluster_id, arr)
    })
    return map
  }, [nodes])

  return (
    <>
      <PageHeader
        title="Admin"
        description="Operator-only view of accounts, clusters, and capacity."
        actions={<ShieldCheck size={14} className="text-zinc-500" />}
      />
      <div className="p-6 space-y-6">
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
          <Tile label="Sandboxes running" value={running} />
          <Tile label="Nodes online" value={nodes.filter((n) => n.state === 'ACTIVE').length} />
          <Tile label="Cluster vCPU" value={`${totals.used.cpu} / ${totals.cap.cpu}`} />
          <Tile label="Cluster memory" value={`${memMB(totals.used.mem)} / ${memMB(totals.cap.mem)}`} />
        </div>

        <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 overflow-hidden">
          <div className="px-4 py-3 border-b border-zinc-900">
            <h2 className="text-sm font-medium">Clusters</h2>
          </div>
          {clusters.length === 0 ? (
            <div className="p-8 text-center text-xs text-zinc-500">No clusters.</div>
          ) : (
            <table className="w-full text-sm">
              <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                <tr className="border-b border-zinc-900">
                  <th className="text-left font-medium px-4 py-2">Name</th>
                  <th className="text-left font-medium px-4 py-2">Region</th>
                  <th className="text-left font-medium px-4 py-2">State</th>
                  <th className="text-left font-medium px-4 py-2">Nodes</th>
                  <th className="text-left font-medium px-4 py-2 w-1/4">CPU utilization</th>
                </tr>
              </thead>
              <tbody>
                {clusters.map((c) => {
                  const ns = byCluster.get(c.id) ?? []
                  const cap = ns.reduce((a, n) => a + n.capacity.total_cpu, 0)
                  const used = ns.reduce((a, n) => a + n.used_resources.used_cpu, 0)
                  return (
                    <tr key={c.id} className="border-b border-zinc-900/50">
                      <td className="px-4 py-2 font-medium">{c.name}</td>
                      <td className="px-4 py-2 text-zinc-400 text-xs font-mono">{c.region}</td>
                      <td className="px-4 py-2"><StateBadge state={c.state} /></td>
                      <td className="px-4 py-2 text-zinc-400 text-xs font-mono">{ns.length}</td>
                      <td className="px-4 py-2">
                        <ProgressBar value={used} max={cap} label={`${used} / ${cap}`} />
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          )}
        </div>

        <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 overflow-hidden">
          <div className="px-4 py-3 border-b border-zinc-900">
            <h2 className="text-sm font-medium">All sandboxes</h2>
          </div>
          <table className="w-full text-sm">
            <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
              <tr className="border-b border-zinc-900">
                <th className="text-left font-medium px-4 py-2">Name</th>
                <th className="text-left font-medium px-4 py-2">Account</th>
                <th className="text-left font-medium px-4 py-2">State</th>
                <th className="text-left font-medium px-4 py-2">Resources</th>
                <th className="text-left font-medium px-4 py-2">Node</th>
              </tr>
            </thead>
            <tbody>
              {sandboxes.map((sb) => (
                <tr key={sb.id} className="border-b border-zinc-900/50">
                  <td className="px-4 py-2 font-medium">{sb.name}</td>
                  <td className="px-4 py-2 font-mono text-xs text-zinc-500">
                    {sb.account_id.slice(0, 10)}…
                  </td>
                  <td className="px-4 py-2"><StateBadge state={sb.state} /></td>
                  <td className="px-4 py-2 text-zinc-400 text-xs font-mono">
                    {sb.config.vcpus} / {memMB(sb.config.memory_mb)} / {sb.config.disk_gb} GB
                  </td>
                  <td className="px-4 py-2 font-mono text-xs text-zinc-500">
                    {sb.node_id?.slice(0, 8) ?? '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </>
  )
}

function Tile({ label, value }: { label: string; value: number | string }) {
  return (
    <div className="rounded-lg border border-zinc-900 bg-zinc-900/40 p-4">
      <div className="text-[11px] text-zinc-500 uppercase tracking-wider font-mono">{label}</div>
      <div className="mt-2 text-2xl font-semibold tabular-nums">{value}</div>
    </div>
  )
}
