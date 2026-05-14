import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Boxes, Play, Square, Trash2, Plus } from 'lucide-react'
import api from '../api/client'
import type { Sandbox } from '../api/types'
import StateBadge from '../components/StateBadge'
import PageHeader from '../components/PageHeader'
import EmptyState from '../components/EmptyState'
import { useToast } from '../components/Toast'
import { formatRelative, memMB } from '../utils/format'
import CreateSandboxModal from './CreateSandboxModal'

export default function SandboxesPage() {
  const nav = useNavigate()
  const toast = useToast()
  const [items, setItems] = useState<Sandbox[]>([])
  const [openCreate, setOpenCreate] = useState(false)
  const [busyId, setBusyId] = useState<string | null>(null)

  async function load() {
    try {
      const r = await api.sandboxes.list()
      setItems(r)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'List failed')
    }
  }

  useEffect(() => {
    load()
    const t = setInterval(load, 5000)
    return () => clearInterval(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function action(id: string, kind: 'stop' | 'start' | 'destroy') {
    setBusyId(id)
    try {
      if (kind === 'stop') await api.sandboxes.stop(id)
      else if (kind === 'start') await api.sandboxes.start(id)
      else {
        if (!window.confirm('Destroy this sandbox? This cannot be undone.')) return
        await api.sandboxes.destroy(id)
      }
      toast.success(`Sandbox ${kind}ed`)
      load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `${kind} failed`)
    } finally {
      setBusyId(null)
    }
  }

  return (
    <>
      <PageHeader
        title="Sandboxes"
        description="Isolated microVMs you've created on the cluster."
        actions={
          <button
            onClick={() => setOpenCreate(true)}
            className="inline-flex items-center gap-1.5 rounded-md bg-teal-500 hover:bg-teal-400 text-zinc-950 shadow-md shadow-teal-500/20 hover:shadow-teal-500/40 transition-all duration-200 hover:scale-[1.02] px-3 py-1.5 text-sm font-medium"
          >
            <Plus size={14} /> Create
          </button>
        }
      />
      <div className="p-6">
        {items.length === 0 ? (
          <EmptyState
            icon={<Boxes size={32} />}
            title="No sandboxes yet"
            description="Spin up an isolated Linux microVM in under a second from any registered template."
            action={
              <button
                onClick={() => setOpenCreate(true)}
                className="inline-flex items-center gap-1.5 rounded-md bg-teal-500 hover:bg-teal-400 text-zinc-950 shadow-md shadow-teal-500/20 hover:shadow-teal-500/40 transition-all duration-200 hover:scale-[1.02] px-3 py-1.5 text-sm font-medium"
              >
                <Plus size={14} /> Create your first sandbox
              </button>
            }
          />
        ) : (
          <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 shadow-lg shadow-black/20 overflow-hidden">
            <table className="w-full text-sm">
              <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                <tr className="border-b border-zinc-900 bg-zinc-950/40">
                  <th className="text-left font-medium px-4 py-2.5">Name</th>
                  <th className="text-left font-medium px-4 py-2.5">State</th>
                  <th className="text-left font-medium px-4 py-2.5">Resources</th>
                  <th className="text-left font-medium px-4 py-2.5">Node</th>
                  <th className="text-left font-medium px-4 py-2.5">Created</th>
                  <th className="text-right font-medium px-4 py-2.5">Actions</th>
                </tr>
              </thead>
              <tbody>
                {items.map((sb) => (
                  <tr
                    key={sb.id}
                    className="border-b border-zinc-900/60 hover:bg-zinc-800/50 transition-colors cursor-pointer"
                    onClick={() => nav(`/sandboxes/${sb.id}`)}
                  >
                    <td className="px-4 py-2.5 font-medium">
                      {sb.name}
                      <div className="text-[11px] text-zinc-600 font-mono">{sb.id.slice(0, 16)}</div>
                    </td>
                    <td className="px-4 py-2.5">
                      <StateBadge state={sb.state} />
                    </td>
                    <td className="px-4 py-2.5 text-zinc-300 text-xs font-mono">
                      {sb.config.vcpus} vCPU · {memMB(sb.config.memory_mb)} · {sb.config.disk_gb} GB
                    </td>
                    <td className="px-4 py-2.5 text-zinc-500 text-xs font-mono">
                      {sb.node_id ? sb.node_id.slice(0, 8) : '—'}
                    </td>
                    <td className="px-4 py-2.5 text-zinc-500 text-xs">
                      {formatRelative(sb.created_at)}
                    </td>
                    <td
                      className="px-4 py-2.5 text-right space-x-1.5"
                      onClick={(e) => e.stopPropagation()}
                    >
                      {sb.state === 'RUNNING' && (
                        <ActionButton
                          onClick={() => action(sb.id, 'stop')}
                          busy={busyId === sb.id}
                          title="Stop"
                          icon={<Square size={12} />}
                        />
                      )}
                      {sb.state === 'STOPPED' && (
                        <ActionButton
                          onClick={() => action(sb.id, 'start')}
                          busy={busyId === sb.id}
                          title="Start"
                          icon={<Play size={12} />}
                        />
                      )}
                      {!isTerminal(sb.state) && (
                        <ActionButton
                          onClick={() => action(sb.id, 'destroy')}
                          busy={busyId === sb.id}
                          title="Destroy"
                          icon={<Trash2 size={12} />}
                          danger
                        />
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
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

function isTerminal(s: string) {
  return s === 'DESTROYED' || s === 'DESTROYING'
}

interface ActionBtnProps {
  onClick: () => void
  busy: boolean
  title: string
  icon: React.ReactNode
  danger?: boolean
}

function ActionButton({ onClick, busy, title, icon, danger }: ActionBtnProps) {
  return (
    <button
      onClick={onClick}
      disabled={busy}
      title={title}
      className={`inline-flex items-center gap-1 rounded border px-2 py-1 text-[11px] font-medium transition-colors disabled:opacity-50 ${
        danger
          ? 'border-red-900 text-red-300 hover:bg-red-950/50'
          : 'border-zinc-800 text-zinc-300 hover:bg-zinc-800'
      }`}
    >
      {icon}
      {title}
    </button>
  )
}
