import { useEffect, useState } from 'react'
import Modal from '../components/Modal'
import Spinner from '../components/Spinner'
import { useToast } from '../components/Toast'
import api from '../api/client'
import type { Sandbox, Snapshot, Template } from '../api/types'

interface Props {
  open: boolean
  onClose: () => void
  onCreated: (sb: Sandbox) => void
  prefillSnapshot?: { id: string; sandbox_name: string }
}

const VCPU_OPTS = [1, 2, 4, 8]
const MEM_OPTS = [
  { label: '512 MB', value: 512 },
  { label: '1 GB', value: 1024 },
  { label: '2 GB', value: 2048 },
  { label: '4 GB', value: 4096 },
  { label: '8 GB', value: 8192 },
]
const DISK_OPTS = [1, 3, 5, 10]
const AUTO_STOP_OPTS = [
  { label: 'Never', value: 0 },
  { label: '5 minutes', value: 5 },
  { label: '15 minutes (default)', value: 15 },
  { label: '30 minutes', value: 30 },
  { label: '1 hour', value: 60 },
  { label: '4 hours', value: 240 },
]
const AUTO_ARCHIVE_OPTS = [
  { label: 'Never', value: 0 },
  { label: '1 hour', value: 60 },
  { label: '6 hours', value: 360 },
  { label: '24 hours (default)', value: 1440 },
  { label: '3 days', value: 4320 },
  { label: '7 days', value: 10080 },
]

export default function CreateSandboxModal({ open, onClose, onCreated, prefillSnapshot }: Props) {
  const toast = useToast()
  const [name, setName] = useState('')
  const [source, setSource] = useState<'image' | 'snapshot'>(
    prefillSnapshot ? 'snapshot' : 'image',
  )
  const [templates, setTemplates] = useState<Template[]>([])
  const [snapshots, setSnapshots] = useState<Snapshot[]>([])
  const [templateId, setTemplateId] = useState<string>('')
  const [snapshotId, setSnapshotId] = useState<string>(prefillSnapshot?.id ?? '')
  const [vcpus, setVcpus] = useState(2)
  const [memoryMB, setMemoryMB] = useState(1024)
  const [diskGB, setDiskGB] = useState(3)
  const [autoStop, setAutoStop] = useState(15)
  const [autoArchive, setAutoArchive] = useState(1440)
  const [gitUrl, setGitUrl] = useState('')
  const [gitBranch, setGitBranch] = useState('main')
  const [gitToken, setGitToken] = useState('')
  const [showToken, setShowToken] = useState(false)
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!open) return
    setName(prefillSnapshot ? `${prefillSnapshot.sandbox_name}-restored` : '')
    setGitUrl('')
    setGitBranch('main')
    setGitToken('')
    setShowToken(false)
    api.templates.list().then((ts) => {
      setTemplates(ts)
      if (ts.length > 0 && !templateId) setTemplateId(ts[0].id)
    }).catch(() => {})
    if (prefillSnapshot) {
      setSnapshotId(prefillSnapshot.id)
      setSource('snapshot')
    }
    // populate snapshots list across all sandboxes for picker convenience
    api.sandboxes.list().then(async (sbs) => {
      const lists = await Promise.all(
        sbs.map((sb) => api.sandboxes.listSnapshots(sb.id).catch(() => [])),
      )
      setSnapshots(lists.flat())
    }).catch(() => {})
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (busy) return
    if (!name.trim()) {
      toast.error('Name is required')
      return
    }
    if (source === 'image' && !templateId) {
      toast.error('Pick a template')
      return
    }
    if (source === 'snapshot' && !snapshotId) {
      toast.error('Pick a snapshot')
      return
    }
    setBusy(true)
    try {
      const sb = await api.sandboxes.create({
        name: name.trim(),
        source,
        template_id: source === 'image' ? templateId : undefined,
        snapshot_id: source === 'snapshot' ? snapshotId : undefined,
        vcpus,
        memory_mb: memoryMB,
        disk_gb: diskGB,
        auto_stop_minutes: autoStop,
        auto_archive_minutes: autoArchive,
        git_url: gitUrl.trim() || undefined,
        git_branch: gitUrl.trim() ? gitBranch.trim() || 'main' : undefined,
        git_token:
          gitUrl.trim() && gitToken.trim() ? gitToken.trim() : undefined,
      })
      toast.success(`Sandbox "${sb.name}" created`)
      onCreated(sb)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Create failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="Create sandbox" size="md">
      <form onSubmit={submit} className="space-y-4">
        <Field label="Name">
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="my-dev-box"
            className={inputCls}
            autoFocus
          />
        </Field>

        <Field label="Source">
          <div className="flex rounded-md border border-zinc-800 overflow-hidden">
            {(['image', 'snapshot'] as const).map((s) => (
              <button
                type="button"
                key={s}
                onClick={() => setSource(s)}
                className={`flex-1 py-1.5 text-xs uppercase tracking-wider font-mono ${
                  source === s
                    ? 'bg-teal-500/15 text-teal-300'
                    : 'text-zinc-500 hover:text-zinc-200'
                }`}
              >
                {s}
              </button>
            ))}
          </div>
        </Field>

        {source === 'image' ? (
          <Field label="Template">
            <select
              value={templateId}
              onChange={(e) => setTemplateId(e.target.value)}
              className={inputCls}
            >
              <option value="">— select —</option>
              {templates.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name} {t.version ? `(${t.version})` : ''}
                </option>
              ))}
            </select>
            {templates.length === 0 && (
              <p className="text-[11px] text-zinc-500 mt-1">
                No templates registered. Add one from the Templates page.
              </p>
            )}
          </Field>
        ) : (
          <Field label="Snapshot">
            <select
              value={snapshotId}
              onChange={(e) => setSnapshotId(e.target.value)}
              className={inputCls}
            >
              <option value="">— select —</option>
              {snapshots.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.id.slice(0, 12)}… (sandbox {s.sandbox_id.slice(0, 8)}…)
                </option>
              ))}
            </select>
          </Field>
        )}

        <div className="grid grid-cols-3 gap-3">
          <Field label="vCPU">
            <select value={vcpus} onChange={(e) => setVcpus(+e.target.value)} className={inputCls}>
              {VCPU_OPTS.map((v) => (
                <option key={v} value={v}>{v}</option>
              ))}
            </select>
          </Field>
          <Field label="Memory">
            <select
              value={memoryMB}
              onChange={(e) => setMemoryMB(+e.target.value)}
              className={inputCls}
            >
              {MEM_OPTS.map((m) => (
                <option key={m.value} value={m.value}>
                  {m.label}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Disk">
            <select value={diskGB} onChange={(e) => setDiskGB(+e.target.value)} className={inputCls}>
              {DISK_OPTS.map((v) => (
                <option key={v} value={v}>
                  {v} GB
                </option>
              ))}
            </select>
          </Field>
        </div>

        <div className="grid grid-cols-2 gap-3">
          <Field label="Auto-stop (idle)">
            <select
              value={autoStop}
              onChange={(e) => setAutoStop(+e.target.value)}
              className={inputCls}
            >
              {AUTO_STOP_OPTS.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Auto-archive (stopped)">
            <select
              value={autoArchive}
              onChange={(e) => setAutoArchive(+e.target.value)}
              className={inputCls}
            >
              {AUTO_ARCHIVE_OPTS.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </select>
          </Field>
        </div>

        <div className="space-y-3 border-t border-zinc-800 pt-3">
          <p className="text-xs text-zinc-400">
            Git repository <span className="text-zinc-600">(optional)</span>
          </p>
          <Field label="Repository URL">
            <input
              value={gitUrl}
              onChange={(e) => setGitUrl(e.target.value)}
              placeholder="https://github.com/user/repo"
              className={inputCls}
            />
          </Field>
          {gitUrl.trim() && (
            <>
              <Field label="Branch">
                <input
                  value={gitBranch}
                  onChange={(e) => setGitBranch(e.target.value)}
                  placeholder="main"
                  className={inputCls}
                />
              </Field>
              <button
                type="button"
                onClick={() => setShowToken((v) => !v)}
                className="text-[11px] text-teal-400 hover:text-teal-300"
              >
                {showToken ? '− Hide token' : '+ Private repo? Add GitHub token'}
              </button>
              {showToken && (
                <Field label="GitHub token">
                  <input
                    type="password"
                    value={gitToken}
                    onChange={(e) => setGitToken(e.target.value)}
                    placeholder="ghp_…"
                    autoComplete="off"
                    className={inputCls}
                  />
                </Field>
              )}
              <p className="text-[11px] text-zinc-500">
                Cloned into <span className="font-mono">/workspace</span> once
                the sandbox is running.
              </p>
            </>
          )}
        </div>

        <div className="flex items-center justify-end gap-2 pt-2 border-t border-zinc-800">
          <button
            type="button"
            onClick={onClose}
            className="rounded-md border border-zinc-800 px-3 py-1.5 text-sm text-zinc-300 hover:bg-zinc-800"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={busy}
            className="rounded-md bg-teal-500 hover:bg-teal-400 disabled:bg-zinc-800 disabled:text-zinc-500 text-zinc-950 px-3 py-1.5 text-sm font-medium flex items-center gap-1.5"
          >
            {busy && <Spinner size={14} />}
            Create
          </button>
        </div>
      </form>
    </Modal>
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
