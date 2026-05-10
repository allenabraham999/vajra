import { useEffect, useState } from 'react'
import { Server } from 'lucide-react'
import api from '../api/client'
import type { Node } from '../api/types'
import PageHeader from '../components/PageHeader'
import EmptyState from '../components/EmptyState'
import StateBadge from '../components/StateBadge'
import ProgressBar from '../components/ProgressBar'
import { useToast } from '../components/Toast'
import { formatRelative, memMB } from '../utils/format'

export default function NodesPage() {
  const toast = useToast()
  const [items, setItems] = useState<Node[]>([])
  const [busyId, setBusyId] = useState<string | null>(null)

  async function load() {
    try {
      const r = await api.nodes.list()
      setItems(r)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'list nodes failed')
    }
  }

  useEffect(() => {
    load()
    const t = setInterval(load, 10_000)
    return () => clearInterval(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function drain(id: string) {
    if (!window.confirm('Drain this node? Running sandboxes will be marked for migration.')) return
    setBusyId(id)
    try {
      await api.nodes.drain(id)
      toast.success('Drain initiated')
      load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'drain failed')
    } finally {
      setBusyId(null)
    }
  }

  return (
    <>
      <PageHeader
        title="Nodes"
        description="Bare-metal hosts running sandboxes. Drain a node to evacuate workloads."
      />
      <div className="p-6">
        {items.length === 0 ? (
          <EmptyState
            icon={<Server size={32} />}
            title="No nodes registered"
            description="Run vajra-agent on a host with KVM enabled to register a new node."
          />
        ) : (
          <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 overflow-hidden">
            <table className="w-full text-sm">
              <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                <tr className="border-b border-zinc-900 bg-zinc-950/40">
                  <th className="text-left font-medium px-4 py-2.5">Hostname</th>
                  <th className="text-left font-medium px-4 py-2.5">State</th>
                  <th className="text-left font-medium px-4 py-2.5 w-1/4">CPU</th>
                  <th className="text-left font-medium px-4 py-2.5 w-1/4">Memory</th>
                  <th className="text-left font-medium px-4 py-2.5">Heartbeat</th>
                  <th className="text-right font-medium px-4 py-2.5"></th>
                </tr>
              </thead>
              <tbody>
                {items.map((n) => (
                  <tr key={n.id} className="border-b border-zinc-900/50 hover:bg-zinc-900/40">
                    <td className="px-4 py-2.5">
                      <div className="font-medium">{n.hostname}</div>
                      <div className="text-[11px] text-zinc-500 font-mono">{n.ip}</div>
                    </td>
                    <td className="px-4 py-2.5">
                      <StateBadge state={n.state} />
                    </td>
                    <td className="px-4 py-2.5">
                      <ProgressBar
                        value={n.used_resources.used_cpu}
                        max={n.capacity.total_cpu}
                        label={`${n.used_resources.used_cpu} / ${n.capacity.total_cpu} vCPU`}
                      />
                    </td>
                    <td className="px-4 py-2.5">
                      <ProgressBar
                        value={n.used_resources.used_memory_mb}
                        max={n.capacity.total_memory_mb}
                        label={`${memMB(n.used_resources.used_memory_mb)} / ${memMB(n.capacity.total_memory_mb)}`}
                      />
                    </td>
                    <td className="px-4 py-2.5 text-zinc-500 text-xs">
                      {formatRelative(n.last_heartbeat)}
                    </td>
                    <td className="px-4 py-2.5 text-right">
                      {n.state !== 'DRAINING' && n.state !== 'DECOMMISSIONED' && (
                        <button
                          onClick={() => drain(n.id)}
                          disabled={busyId === n.id}
                          className="rounded border border-zinc-800 px-2 py-1 text-[11px] hover:bg-zinc-800 disabled:opacity-50"
                        >
                          Drain
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  )
}
