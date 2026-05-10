import { useEffect, useState } from 'react'
import { PackageSearch, Plus } from 'lucide-react'
import api from '../api/client'
import type { Template, Snapshot } from '../api/types'
import PageHeader from '../components/PageHeader'
import EmptyState from '../components/EmptyState'
import Modal from '../components/Modal'
import Spinner from '../components/Spinner'
import { useToast } from '../components/Toast'
import { formatRelative, shortHash } from '../utils/format'

export default function TemplatesPage() {
  const toast = useToast()
  const [items, setItems] = useState<Template[]>([])
  const [snaps, setSnaps] = useState<Snapshot[]>([])
  const [openCreate, setOpenCreate] = useState(false)
  const [openPromote, setOpenPromote] = useState(false)

  async function load() {
    try {
      const r = await api.templates.list()
      setItems(r)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'list templates failed')
    }
    // pull all snapshots for the promote modal
    api.sandboxes.list().then(async (sbs) => {
      const lists = await Promise.all(
        sbs.map((s) => api.sandboxes.listSnapshots(s.id).catch(() => [])),
      )
      setSnaps(lists.flat())
    }).catch(() => {})
  }

  useEffect(() => {
    load()
  }, [])

  return (
    <>
      <PageHeader
        title="Templates"
        description="Content-addressable VM images keyed by SHA256 of the rootfs."
        actions={
          <div className="flex gap-2">
            <button
              onClick={() => setOpenPromote(true)}
              className="inline-flex items-center gap-1.5 rounded-md border border-zinc-800 px-3 py-1.5 text-sm hover:bg-zinc-800"
            >
              From snapshot
            </button>
            <button
              onClick={() => setOpenCreate(true)}
              className="inline-flex items-center gap-1.5 rounded-md bg-emerald-500 hover:bg-emerald-400 text-zinc-950 px-3 py-1.5 text-sm font-medium"
            >
              <Plus size={14} /> Register
            </button>
          </div>
        }
      />
      <div className="p-6">
        {items.length === 0 ? (
          <EmptyState
            icon={<PackageSearch size={32} />}
            title="No templates registered"
            description="Templates are immutable rootfs + kernel + snapshot bundles, identified by SHA256 hash."
          />
        ) : (
          <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 overflow-hidden">
            <table className="w-full text-sm">
              <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                <tr className="border-b border-zinc-900 bg-zinc-950/40">
                  <th className="text-left font-medium px-4 py-2.5">Name</th>
                  <th className="text-left font-medium px-4 py-2.5">Version</th>
                  <th className="text-left font-medium px-4 py-2.5">Hash</th>
                  <th className="text-left font-medium px-4 py-2.5">Created</th>
                </tr>
              </thead>
              <tbody>
                {items.map((t) => (
                  <tr key={t.id} className="border-b border-zinc-900/50 hover:bg-zinc-900/40">
                    <td className="px-4 py-2.5 font-medium">{t.name}</td>
                    <td className="px-4 py-2.5 text-zinc-400 text-xs font-mono">
                      {t.version || '—'}
                    </td>
                    <td className="px-4 py-2.5 font-mono text-xs text-zinc-500">
                      {shortHash(t.hash, 16)}
                    </td>
                    <td className="px-4 py-2.5 text-zinc-500 text-xs">
                      {formatRelative(t.created_at)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <CreateTemplateModal
        open={openCreate}
        onClose={() => setOpenCreate(false)}
        onCreated={() => {
          setOpenCreate(false)
          load()
        }}
      />

      <PromoteSnapshotModal
        open={openPromote}
        onClose={() => setOpenPromote(false)}
        snapshots={snaps}
        onPromoted={() => {
          setOpenPromote(false)
          load()
        }}
      />
    </>
  )
}

function CreateTemplateModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated: () => void
}) {
  const toast = useToast()
  const [name, setName] = useState('')
  const [version, setVersion] = useState('')
  const [hash, setHash] = useState('')
  const [rootfs, setRootfs] = useState('')
  const [kernel, setKernel] = useState('')
  const [snap, setSnap] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    try {
      await api.templates.create({
        name,
        version,
        hash,
        rootfs_path: rootfs,
        kernel_path: kernel,
        snapshot_path: snap || undefined,
      })
      toast.success(`Template "${name}" registered`)
      onCreated()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'create failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="Register template" size="md">
      <form onSubmit={submit} className="space-y-3">
        <Field label="Name">
          <input value={name} onChange={(e) => setName(e.target.value)} className={inputCls} required />
        </Field>
        <Field label="Version">
          <input
            value={version}
            onChange={(e) => setVersion(e.target.value)}
            placeholder="1.0.0"
            className={inputCls}
          />
        </Field>
        <Field label="SHA256 hash">
          <input
            value={hash}
            onChange={(e) => setHash(e.target.value)}
            placeholder="64 hex characters"
            className={`${inputCls} font-mono`}
            required
          />
        </Field>
        <Field label="Rootfs path">
          <input
            value={rootfs}
            onChange={(e) => setRootfs(e.target.value)}
            placeholder="/var/lib/vajra/cache/<hash>/rootfs.raw"
            className={`${inputCls} font-mono`}
            required
          />
        </Field>
        <Field label="Kernel path">
          <input
            value={kernel}
            onChange={(e) => setKernel(e.target.value)}
            placeholder="/var/lib/vajra/cache/<hash>/vmlinux"
            className={`${inputCls} font-mono`}
            required
          />
        </Field>
        <Field label="Snapshot path (optional)">
          <input
            value={snap}
            onChange={(e) => setSnap(e.target.value)}
            placeholder="/var/lib/vajra/cache/<hash>/snapshot/"
            className={`${inputCls} font-mono`}
          />
        </Field>
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
            disabled={busy}
            className="rounded-md bg-emerald-500 hover:bg-emerald-400 disabled:bg-zinc-800 disabled:text-zinc-500 text-zinc-950 px-3 py-1.5 text-sm font-medium flex items-center gap-1.5"
          >
            {busy && <Spinner size={14} />}
            Register
          </button>
        </div>
      </form>
    </Modal>
  )
}

function PromoteSnapshotModal({
  open,
  onClose,
  snapshots,
  onPromoted,
}: {
  open: boolean
  onClose: () => void
  snapshots: Snapshot[]
  onPromoted: () => void
}) {
  const toast = useToast()
  const [snapId, setSnapId] = useState('')
  const [name, setName] = useState('')
  const [version, setVersion] = useState('1.0.0')
  const [busy, setBusy] = useState(false)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (!snapId) return toast.error('Pick a snapshot')
    setBusy(true)
    try {
      await api.snapshots.promote(snapId, name, version)
      toast.success('Promoted to template')
      onPromoted()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'promote failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="Promote snapshot to template" size="md">
      <form onSubmit={submit} className="space-y-3">
        <Field label="Snapshot">
          <select value={snapId} onChange={(e) => setSnapId(e.target.value)} className={inputCls}>
            <option value="">— select —</option>
            {snapshots.map((s) => (
              <option key={s.id} value={s.id}>
                {s.id.slice(0, 16)}… ({s.sandbox_id.slice(0, 8)})
              </option>
            ))}
          </select>
        </Field>
        <Field label="Template name">
          <input value={name} onChange={(e) => setName(e.target.value)} className={inputCls} required />
        </Field>
        <Field label="Version">
          <input
            value={version}
            onChange={(e) => setVersion(e.target.value)}
            className={inputCls}
            required
          />
        </Field>
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
            disabled={busy || !snapId}
            className="rounded-md bg-emerald-500 hover:bg-emerald-400 disabled:bg-zinc-800 disabled:text-zinc-500 text-zinc-950 px-3 py-1.5 text-sm font-medium flex items-center gap-1.5"
          >
            {busy && <Spinner size={14} />}
            Promote
          </button>
        </div>
      </form>
    </Modal>
  )
}

const inputCls =
  'w-full rounded-md bg-zinc-950 border border-zinc-800 px-2.5 py-1.5 text-sm focus:border-emerald-600 focus:outline-none focus:ring-2 focus:ring-emerald-600/30'

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <label className="block text-xs text-zinc-400 mb-1">{label}</label>
      {children}
    </div>
  )
}
