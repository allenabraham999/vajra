import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Boxes, Cpu, Server, Plus, AlertTriangle } from 'lucide-react'
import api from '../api/client'
import type { Sandbox, Node } from '../api/types'
import StateBadge from '../components/StateBadge'
import PageHeader from '../components/PageHeader'
import { formatRelative } from '../utils/format'
import CreateSandboxModal from '../pages/CreateSandboxModal'

interface Stat {
  label: string
  value: string | number
  hint?: string
  icon: typeof Boxes
}

export default function Dashboard() {
  const nav = useNavigate()
  const [sandboxes, setSandboxes] = useState<Sandbox[]>([])
  const [nodes, setNodes] = useState<Node[]>([])
  const [openCreate, setOpenCreate] = useState(false)

  useEffect(() => {
    let alive = true
    async function load() {
      try {
        const [s, n] = await Promise.all([
          api.sandboxes.list(),
          api.nodes.list().catch(() => [] as Node[]),
        ])
        if (!alive) return
        setSandboxes(s)
        setNodes(n)
      } catch {
        /* ignore */
      }
    }
    load()
    const t = setInterval(load, 5000)
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [])

  const stats = useMemo<Stat[]>(() => {
    const running = sandboxes.filter((s) => s.state === 'RUNNING').length
    const stopped = sandboxes.filter((s) => s.state === 'STOPPED').length
    const errored = sandboxes.filter((s) => s.state === 'ERROR').length
    const active = nodes.filter((n) => n.state === 'ACTIVE').length
    const offline = nodes.filter((n) => n.state === 'OFFLINE').length
    const totalCPU = sandboxes
      .filter((s) => s.state === 'RUNNING')
      .reduce((acc, s) => acc + s.config.vcpus, 0)
    const totalMem = sandboxes
      .filter((s) => s.state === 'RUNNING')
      .reduce((acc, s) => acc + s.config.memory_mb, 0)
    return [
      {
        label: 'Sandboxes',
        value: sandboxes.length,
        hint: `${running} running · ${stopped} stopped`,
        icon: Boxes,
      },
      {
        label: 'Active CPU',
        value: totalCPU,
        hint: `${(totalMem / 1024).toFixed(1)} GB memory in use`,
        icon: Cpu,
      },
      {
        label: 'Nodes',
        value: nodes.length || '—',
        hint: nodes.length ? `${active} active · ${offline} offline` : 'admin only',
        icon: Server,
      },
      {
        label: 'Errors',
        value: errored,
        hint: errored ? 'sandboxes need attention' : 'all clean',
        icon: AlertTriangle,
      },
    ]
  }, [sandboxes, nodes])

  const recent = useMemo(
    () =>
      [...sandboxes]
        .sort(
          (a, b) =>
            new Date(b.updated_at).getTime() - new Date(a.updated_at).getTime(),
        )
        .slice(0, 10),
    [sandboxes],
  )

  return (
    <>
      <PageHeader
        title="Overview"
        description="Cluster-wide snapshot of your sandboxes and nodes."
        actions={
          <button
            onClick={() => setOpenCreate(true)}
            className="inline-flex items-center gap-1.5 rounded-md bg-emerald-500 hover:bg-emerald-400 text-zinc-950 px-3 py-1.5 text-sm font-medium"
          >
            <Plus size={14} /> New sandbox
          </button>
        }
      />
      <div className="p-6 space-y-6">
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
          {stats.map((s) => (
            <div
              key={s.label}
              className="rounded-lg border border-zinc-900 bg-zinc-900/40 p-4"
            >
              <div className="flex items-center justify-between">
                <span className="text-[11px] text-zinc-500 uppercase tracking-wider font-mono">
                  {s.label}
                </span>
                <s.icon size={14} className="text-zinc-600" />
              </div>
              <div className="mt-2 text-2xl font-semibold tabular-nums">{s.value}</div>
              {s.hint && <div className="mt-1 text-[11px] text-zinc-500">{s.hint}</div>}
            </div>
          ))}
        </div>

        <div className="rounded-lg border border-zinc-900 bg-zinc-900/40">
          <div className="px-4 py-3 border-b border-zinc-900 flex items-center justify-between">
            <h2 className="text-sm font-medium">Recent activity</h2>
            <button
              onClick={() => nav('/sandboxes')}
              className="text-xs text-zinc-500 hover:text-zinc-200"
            >
              View all →
            </button>
          </div>
          {recent.length === 0 ? (
            <div className="p-8 text-center text-sm text-zinc-500">
              No sandboxes yet. Create one to get started.
            </div>
          ) : (
            <table className="w-full text-sm">
              <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                <tr className="border-b border-zinc-900">
                  <th className="text-left font-medium px-4 py-2">Name</th>
                  <th className="text-left font-medium px-4 py-2">State</th>
                  <th className="text-left font-medium px-4 py-2">Resources</th>
                  <th className="text-left font-medium px-4 py-2">Updated</th>
                </tr>
              </thead>
              <tbody>
                {recent.map((sb) => (
                  <tr
                    key={sb.id}
                    className="border-b border-zinc-900/60 hover:bg-zinc-900/40 cursor-pointer"
                    onClick={() => nav(`/sandboxes/${sb.id}`)}
                  >
                    <td className="px-4 py-2.5 font-medium">{sb.name}</td>
                    <td className="px-4 py-2.5">
                      <StateBadge state={sb.state} />
                    </td>
                    <td className="px-4 py-2.5 text-zinc-400 font-mono text-xs">
                      {sb.config.vcpus} vCPU · {sb.config.memory_mb} MB · {sb.config.disk_gb} GB
                    </td>
                    <td className="px-4 py-2.5 text-zinc-500 text-xs">
                      {formatRelative(sb.updated_at)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      <CreateSandboxModal
        open={openCreate}
        onClose={() => setOpenCreate(false)}
        onCreated={(sb) => {
          setOpenCreate(false)
          nav(`/sandboxes/${sb.id}`)
        }}
      />
    </>
  )
}
