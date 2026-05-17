import { useEffect, useMemo, useState } from 'react'
import { PackageSearch, Plus } from 'lucide-react'
import api from '../api/client'
import type { PoolStats, Template, Snapshot } from '../api/types'
import PageHeader from '../components/PageHeader'
import EmptyState from '../components/EmptyState'
import Modal from '../components/Modal'
import Spinner from '../components/Spinner'
import { useToast } from '../components/Toast'
import { formatRelative, shortHash } from '../utils/format'

// PoolSummary is the warm-pool rollup for a single template, aggregated
// across every node in /v1/pool/stats.
interface PoolSummary {
  available: number
  target_size: number
  hits_last_hour: number
}

export default function TemplatesPage() {
  const toast = useToast()
  const [items, setItems] = useState<Template[]>([])
  const [snaps, setSnaps] = useState<Snapshot[]>([])
  const [pool, setPool] = useState<PoolStats | null>(null)
  const [openCreate, setOpenCreate] = useState(false)
  const [openPromote, setOpenPromote] = useState(false)
  const [openBuild, setOpenBuild] = useState(false)

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

  // Warm-pool stats refresh on a light interval so the column tracks the
  // pool without the user reloading the page.
  useEffect(() => {
    let alive = true
    const tick = () =>
      api.pool
        .stats()
        .then((p) => alive && setPool(p))
        .catch(() => {})
    tick()
    const t = setInterval(tick, 10_000)
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [])

  // Roll the per-node, per-template pool rows up into one summary keyed by
  // both template_hash and template_id, so a template matches on either.
  const poolByTemplate = useMemo(() => {
    const m = new Map<string, PoolSummary>()
    const add = (key: string, available: number, target: number, hits: number) => {
      if (!key) return
      const cur = m.get(key) ?? { available: 0, target_size: 0, hits_last_hour: 0 }
      cur.available += available
      cur.target_size += target
      cur.hits_last_hour += hits
      m.set(key, cur)
    }
    for (const node of pool?.nodes ?? []) {
      for (const t of node.templates ?? []) {
        add(t.template_hash, t.available, t.target_size, t.hits_last_hour)
        add(t.template_id, t.available, t.target_size, t.hits_last_hour)
      }
    }
    return m
  }, [pool])

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
              onClick={() => setOpenBuild(true)}
              className="inline-flex items-center gap-1.5 rounded-md bg-teal-500 hover:bg-teal-400 text-zinc-950 shadow-md shadow-teal-500/20 hover:shadow-teal-500/40 transition-all duration-200 hover:scale-[1.02] px-3 py-1.5 text-sm font-medium"
            >
              <Plus size={14} /> New Template
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
            action={
              <button
                onClick={() => setOpenBuild(true)}
                className="inline-flex items-center gap-1.5 rounded-md bg-teal-500 hover:bg-teal-400 text-zinc-950 shadow-md shadow-teal-500/20 hover:shadow-teal-500/40 transition-all duration-200 hover:scale-[1.02] px-3 py-1.5 text-sm font-medium"
              >
                <Plus size={14} /> Build your first template
              </button>
            }
          />
        ) : (
          <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 overflow-hidden">
            <table className="w-full text-sm">
              <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                <tr className="border-b border-zinc-900 bg-zinc-950/40">
                  <th className="text-left font-medium px-4 py-2.5">Name</th>
                  <th className="text-left font-medium px-4 py-2.5">Version</th>
                  <th className="text-left font-medium px-4 py-2.5">Hash</th>
                  <th className="text-left font-medium px-4 py-2.5">Warm pool</th>
                  <th className="text-left font-medium px-4 py-2.5">Created</th>
                </tr>
              </thead>
              <tbody>
                {items.map((t) => (
                  <tr key={t.id} className="border-b border-zinc-900/50 hover:bg-zinc-800/50 transition-colors">
                    <td className="px-4 py-2.5 font-medium">{t.name}</td>
                    <td className="px-4 py-2.5 text-zinc-400 text-xs font-mono">
                      {t.version || '—'}
                    </td>
                    <td className="px-4 py-2.5 font-mono text-xs text-zinc-500">
                      {shortHash(t.hash, 16)}
                    </td>
                    <td className="px-4 py-2.5">
                      <WarmPoolCell
                        summary={
                          poolByTemplate.get(t.hash) ?? poolByTemplate.get(t.id)
                        }
                      />
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
        <div className="mt-3 text-right">
          <button
            onClick={() => setOpenCreate(true)}
            className="text-xs text-zinc-500 hover:text-zinc-300 underline underline-offset-2"
          >
            Advanced: register an existing image by path
          </button>
        </div>
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

      <BuildTemplateModal
        open={openBuild}
        onClose={() => setOpenBuild(false)}
        onCompleted={() => {
          setOpenBuild(false)
          load()
        }}
      />
    </>
  )
}

// Build steps shown while a custom-template build is running. The backend
// reports a single BUILDING status, so this is an at-a-glance outline of
// what the builder is doing rather than a live per-step tracker.
const buildSteps = [
  'Copying the Ubuntu base image',
  'Running your setup commands',
  'Booting & snapshotting the VM',
]

function BuildTemplateModal({
  open,
  onClose,
  onCompleted,
}: {
  open: boolean
  onClose: () => void
  onCompleted: () => void
}) {
  const toast = useToast()
  const [name, setName] = useState('')
  const [version, setVersion] = useState('1.0.0')
  const [setup, setSetup] = useState(
    'apt-get update\napt-get install -y python3-pip\n',
  )
  const [status, setStatus] = useState<string>('')
  const [busy, setBusy] = useState(false)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setStatus('PENDING')
    try {
      // The setup script travels in the `dockerfile` field for wire
      // compatibility; the builder runs it inside the base rootfs.
      const accepted = await api.templates.build({ name, version, dockerfile: setup })
      // Poll until terminal state.
      const deadline = Date.now() + 10 * 60 * 1000
      while (Date.now() < deadline) {
        const b = await api.templates.buildStatus(accepted.build_id)
        setStatus(b.status)
        if (b.status === 'COMPLETED') {
          toast.success(`Template "${name}" built`)
          onCompleted()
          return
        }
        if (b.status === 'FAILED') {
          toast.error(b.error || 'Build failed')
          return
        }
        await new Promise((r) => setTimeout(r, 2000))
      }
      toast.error('Build timed out')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Build failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="Build Custom Template" size="lg">
      <form onSubmit={submit} className="space-y-3">
        <p className="text-xs text-zinc-500 leading-relaxed">
          Builds a new template from the Ubuntu 24.04 base image: your setup
          commands run inside it, then the VM is booted and snapshotted. The
          finished template restores in milliseconds like any other.
        </p>
        <div className="grid grid-cols-2 gap-3">
          <Field label="Name">
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="my-python-env"
              className={inputCls}
              required
            />
          </Field>
          <Field label="Version">
            <input value={version} onChange={(e) => setVersion(e.target.value)} className={inputCls} required />
          </Field>
        </div>
        <Field label="Setup commands">
          <textarea
            value={setup}
            onChange={(e) => setSetup(e.target.value)}
            rows={8}
            className={`${inputCls} font-mono text-xs`}
            required
          />
        </Field>
        <p className="text-xs text-zinc-600">
          Shell commands run as root inside the image — install packages with{' '}
          <code className="text-zinc-400">apt-get install -y …</code>,{' '}
          <code className="text-zinc-400">pip install …</code>, or fetch files.
        </p>
        {busy && (
          <div className="rounded-md border border-zinc-800 bg-zinc-950/50 p-3 space-y-1.5">
            <div className="flex items-center gap-2 text-xs text-teal-300">
              <Spinner size={13} />
              Building — this takes 2–4 minutes
            </div>
            <ul className="text-[11px] text-zinc-500 pl-5 list-disc space-y-0.5">
              {buildSteps.map((s) => (
                <li key={s}>{s}</li>
              ))}
            </ul>
          </div>
        )}
        {status && !busy && (
          <div className="text-xs text-zinc-400">
            Status: <span className="font-mono text-teal-300">{status}</span>
          </div>
        )}
        <div className="flex justify-end gap-2 pt-2 border-t border-zinc-800">
          <button
            type="button"
            onClick={onClose}
            className="rounded-md border border-zinc-800 px-3 py-1.5 text-sm hover:bg-zinc-800"
          >
            {busy ? 'Close' : 'Cancel'}
          </button>
          <button
            type="submit"
            disabled={busy}
            className="rounded-md bg-teal-500 hover:bg-teal-400 disabled:bg-zinc-800 disabled:text-zinc-500 text-zinc-950 px-3 py-1.5 text-sm font-medium flex items-center gap-1.5"
          >
            {busy && <Spinner size={14} />}
            Build template
          </button>
        </div>
      </form>
    </Modal>
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
            className="rounded-md bg-teal-500 hover:bg-teal-400 disabled:bg-zinc-800 disabled:text-zinc-500 text-zinc-950 px-3 py-1.5 text-sm font-medium flex items-center gap-1.5"
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

// WarmPoolCell shows the total warm VMs ready for a template across all
// nodes. Hovering reveals hits/hr and the configured target size.
function WarmPoolCell({ summary }: { summary?: PoolSummary }) {
  if (!summary || (summary.available === 0 && summary.target_size === 0)) {
    return <span className="text-zinc-600 text-xs">—</span>
  }
  return (
    <span
      className="inline-flex items-center gap-1.5 cursor-default"
      title={`${summary.hits_last_hour} hits/hr · target ${summary.target_size}`}
    >
      <span
        className={`size-1.5 rounded-full ${
          summary.available > 0
            ? 'bg-teal-400 animate-pulse'
            : 'bg-zinc-600'
        }`}
      />
      <span className="tabular-nums text-sm text-teal-300 font-medium">
        {summary.available}
      </span>
      <span className="text-[11px] text-zinc-600">/ {summary.target_size}</span>
    </span>
  )
}

const inputCls =
  'w-full rounded-md bg-zinc-950 border border-zinc-800 px-2.5 py-1.5 text-sm focus:border-teal-600 focus:outline-none focus:ring-2 focus:ring-teal-600/30'

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <label className="block text-xs text-zinc-400 mb-1">{label}</label>
      {children}
    </div>
  )
}
