import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Camera } from 'lucide-react'
import api from '../api/client'
import type { Sandbox, Snapshot } from '../api/types'
import PageHeader from '../components/PageHeader'
import EmptyState from '../components/EmptyState'
import Modal from '../components/Modal'
import Spinner from '../components/Spinner'
import { useToast } from '../components/Toast'
import { formatBytes, formatRelative } from '../utils/format'

export default function SnapshotsPage() {
  const toast = useToast()
  const nav = useNavigate()
  const [snaps, setSnaps] = useState<Snapshot[]>([])
  const [sandboxes, setSandboxes] = useState<Sandbox[]>([])
  const [loaded, setLoaded] = useState(false)
  const [busyId, setBusyId] = useState<string | null>(null)
  const [promoting, setPromoting] = useState<Snapshot | null>(null)

  const load = useCallback(async () => {
    try {
      const [s, sb] = await Promise.all([
        api.snapshots.list(),
        api.sandboxes.list().catch(() => [] as Sandbox[]),
      ])
      setSnaps(Array.isArray(s) ? s : [])
      setSandboxes(sb)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to load snapshots')
    } finally {
      setLoaded(true)
    }
  }, [toast])

  useEffect(() => {
    load()
    const t = setInterval(load, 10_000)
    return () => clearInterval(t)
  }, [load])

  // Map sandbox id → name so the table can show a human source label.
  const sandboxName = useMemo(() => {
    const m = new Map<string, string>()
    for (const sb of sandboxes) m.set(sb.id, sb.name)
    return m
  }, [sandboxes])

  async function restore(snap: Snapshot) {
    const base = sandboxName.get(snap.sandbox_id) ?? snap.sandbox_id.slice(0, 8)
    const name = window.prompt('Name for the restored sandbox?', `${base}-restored`)
    if (!name) return
    setBusyId(snap.id)
    try {
      const sb = await api.snapshots.restore(snap.id, name)
      toast.success('Sandbox restored from snapshot')
      nav(`/sandboxes/${sb.id}`)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Restore failed')
    } finally {
      setBusyId(null)
    }
  }

  async function remove(snap: Snapshot) {
    if (!window.confirm('Delete this snapshot? This cannot be undone.')) return
    setBusyId(snap.id)
    try {
      await api.snapshots.delete(snap.id)
      toast.success('Snapshot deleted')
      setSnaps((prev) => prev.filter((s) => s.id !== snap.id))
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Delete failed')
    } finally {
      setBusyId(null)
    }
  }

  return (
    <>
      <PageHeader
        title="Snapshots"
        description="Saved point-in-time states across all your sandboxes."
      />
      <div className="p-6">
        {!loaded ? (
          <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 p-8 text-center text-sm text-zinc-500">
            <Spinner size={16} className="inline mr-2" />
            Loading snapshots…
          </div>
        ) : snaps.length === 0 ? (
          <EmptyState
            icon={<Camera size={32} />}
            title="No snapshots yet"
            description="Take a snapshot of any sandbox to save its state — you can restore it into a fresh sandbox or promote it to a reusable template."
          />
        ) : (
          <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 shadow-lg shadow-black/20 overflow-hidden">
            <table className="w-full text-sm">
              <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                <tr className="border-b border-zinc-900 bg-zinc-950/40">
                  <th className="text-left font-medium px-4 py-2.5">Snapshot</th>
                  <th className="text-left font-medium px-4 py-2.5">Source sandbox</th>
                  <th className="text-right font-medium px-4 py-2.5">Size</th>
                  <th className="text-left font-medium px-4 py-2.5">Created</th>
                  <th className="text-right font-medium px-4 py-2.5">Actions</th>
                </tr>
              </thead>
              <tbody>
                {snaps.map((s) => (
                  <tr
                    key={s.id}
                    className="border-b border-zinc-900/60 hover:bg-zinc-800/50 transition-colors"
                  >
                    <td className="px-4 py-2.5 font-mono text-xs text-zinc-300">
                      {s.id.slice(0, 16)}
                    </td>
                    <td className="px-4 py-2.5">
                      {sandboxName.has(s.sandbox_id) ? (
                        <span className="text-zinc-200">
                          {sandboxName.get(s.sandbox_id)}
                        </span>
                      ) : (
                        <span className="font-mono text-xs text-zinc-500">
                          {s.sandbox_id.slice(0, 12)}
                        </span>
                      )}
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums text-xs text-zinc-300">
                      {formatBytes(s.size_bytes)}
                    </td>
                    <td className="px-4 py-2.5 text-zinc-500 text-xs">
                      {formatRelative(s.created_at)}
                    </td>
                    <td className="px-4 py-2.5 text-right space-x-1.5 whitespace-nowrap">
                      <RowButton
                        label="Restore"
                        busy={busyId === s.id}
                        onClick={() => restore(s)}
                      />
                      <RowButton
                        label="Promote"
                        busy={busyId === s.id}
                        onClick={() => setPromoting(s)}
                      />
                      <RowButton
                        label="Delete"
                        busy={busyId === s.id}
                        danger
                        onClick={() => remove(s)}
                      />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <PromoteModal
        snapshot={promoting}
        onClose={() => setPromoting(null)}
        onPromoted={() => {
          setPromoting(null)
          toast.success('Snapshot promoted to template')
        }}
      />
    </>
  )
}

function RowButton({
  label,
  onClick,
  busy,
  danger,
}: {
  label: string
  onClick: () => void
  busy: boolean
  danger?: boolean
}) {
  return (
    <button
      onClick={onClick}
      disabled={busy}
      className={`rounded border px-2 py-0.5 text-[11px] font-medium transition-colors disabled:opacity-40 ${
        danger
          ? 'border-red-900 text-red-300 hover:bg-red-950/50'
          : 'border-zinc-800 text-zinc-300 hover:bg-zinc-800'
      }`}
    >
      {label}
    </button>
  )
}

// PromoteModal turns a snapshot into a reusable template via the existing
// /v1/snapshots/{id}/promote endpoint.
function PromoteModal({
  snapshot,
  onClose,
  onPromoted,
}: {
  snapshot: Snapshot | null
  onClose: () => void
  onPromoted: () => void
}) {
  const toast = useToast()
  const [name, setName] = useState('')
  const [version, setVersion] = useState('1.0.0')
  const [busy, setBusy] = useState(false)

  // Reset the form whenever a different snapshot is selected.
  useEffect(() => {
    if (snapshot) {
      setName('')
      setVersion('1.0.0')
    }
  }, [snapshot])

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (!snapshot || busy) return
    setBusy(true)
    try {
      await api.snapshots.promote(snapshot.id, name.trim(), version.trim() || '1.0.0')
      onPromoted()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Promote failed')
    } finally {
      setBusy(false)
    }
  }

  const inputCls =
    'w-full rounded-md bg-zinc-950 border border-zinc-800 px-2.5 py-1.5 text-sm focus:border-teal-600 focus:outline-none focus:ring-2 focus:ring-teal-600/30'

  return (
    <Modal
      open={snapshot != null}
      onClose={onClose}
      title="Promote snapshot to template"
      size="md"
    >
      <form onSubmit={submit} className="space-y-3">
        <p className="text-xs text-zinc-500 leading-relaxed">
          Registers a reusable template that points at this snapshot, so new
          sandboxes can be launched directly from it.
        </p>
        <div>
          <label className="block text-xs text-zinc-400 mb-1">Template name</label>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="my-template"
            className={inputCls}
            required
          />
        </div>
        <div>
          <label className="block text-xs text-zinc-400 mb-1">Version</label>
          <input
            value={version}
            onChange={(e) => setVersion(e.target.value)}
            className={inputCls}
            required
          />
        </div>
        <div className="flex justify-end gap-2 pt-2 border-t border-zinc-800">
          <button
            type="button"
            onClick={onClose}
            className="rounded-md border border-zinc-800 px-3 py-1.5 text-sm hover:bg-zinc-800"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={busy || !name.trim()}
            className="rounded-md bg-teal-500 hover:bg-teal-400 disabled:bg-zinc-800 disabled:text-zinc-500 text-zinc-950 px-3 py-1.5 text-sm font-medium flex items-center gap-1.5"
          >
            {busy && <Spinner size={14} />}
            Promote
          </button>
        </div>
      </form>
    </Modal>
  )
}
