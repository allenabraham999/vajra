import { useEffect, useState } from 'react'
import { KeyRound, Plus, Trash2, Copy } from 'lucide-react'
import api from '../api/client'
import type { APIKey } from '../api/types'
import PageHeader from '../components/PageHeader'
import EmptyState from '../components/EmptyState'
import Modal from '../components/Modal'
import Spinner from '../components/Spinner'
import { useToast } from '../components/Toast'
import { formatRelative } from '../utils/format'

export default function ApiKeysPage() {
  const toast = useToast()
  const [items, setItems] = useState<APIKey[]>([])
  const [openCreate, setOpenCreate] = useState(false)
  const [issuedKey, setIssuedKey] = useState<{ name: string; key: string } | null>(null)

  async function load() {
    try {
      const r = await api.apiKeys.list()
      setItems(r)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'list keys failed')
    }
  }

  useEffect(() => {
    load()
  }, [])

  async function remove(id: string) {
    if (!window.confirm('Revoke this API key? Active SDKs using it will stop working.')) return
    try {
      await api.apiKeys.delete(id)
      toast.success('Key revoked')
      load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'delete failed')
    }
  }

  return (
    <>
      <PageHeader
        title="API Keys"
        description="Long-lived bearer tokens used by SDKs and automation."
        actions={
          <button
            onClick={() => setOpenCreate(true)}
            className="inline-flex items-center gap-1.5 rounded-md bg-emerald-500 hover:bg-emerald-400 text-zinc-950 px-3 py-1.5 text-sm font-medium"
          >
            <Plus size={14} /> New key
          </button>
        }
      />

      <div className="p-6">
        {items.length === 0 ? (
          <EmptyState
            icon={<KeyRound size={32} />}
            title="No API keys yet"
            description="Issue a key to authenticate from the CLI or your own client code."
            action={
              <button
                onClick={() => setOpenCreate(true)}
                className="inline-flex items-center gap-1.5 rounded-md bg-emerald-500 hover:bg-emerald-400 text-zinc-950 px-3 py-1.5 text-sm font-medium"
              >
                <Plus size={14} /> Create your first key
              </button>
            }
          />
        ) : (
          <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 overflow-hidden">
            <table className="w-full text-sm">
              <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                <tr className="border-b border-zinc-900 bg-zinc-950/40">
                  <th className="text-left font-medium px-4 py-2.5">Name</th>
                  <th className="text-left font-medium px-4 py-2.5">ID</th>
                  <th className="text-left font-medium px-4 py-2.5">Created</th>
                  <th className="text-right font-medium px-4 py-2.5"></th>
                </tr>
              </thead>
              <tbody>
                {items.map((k) => (
                  <tr key={k.id} className="border-b border-zinc-900/50 hover:bg-zinc-900/40">
                    <td className="px-4 py-2.5 font-medium">{k.name}</td>
                    <td className="px-4 py-2.5 font-mono text-xs text-zinc-500">
                      vj_live_{k.id.slice(0, 8)}…
                    </td>
                    <td className="px-4 py-2.5 text-zinc-500 text-xs">
                      {formatRelative(k.created_at)}
                    </td>
                    <td className="px-4 py-2.5 text-right">
                      <button
                        onClick={() => remove(k.id)}
                        className="inline-flex items-center gap-1 rounded border border-red-900 text-red-300 px-2 py-1 text-[11px] hover:bg-red-950/50"
                      >
                        <Trash2 size={11} /> Revoke
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <CreateKeyModal
        open={openCreate}
        onClose={() => setOpenCreate(false)}
        onCreated={(k) => {
          setOpenCreate(false)
          setIssuedKey(k)
          load()
        }}
      />

      <Modal
        open={!!issuedKey}
        onClose={() => setIssuedKey(null)}
        title={`API key: ${issuedKey?.name}`}
        size="md"
      >
        <p className="text-xs text-zinc-400 mb-3">
          Save this somewhere safe — we won't show it again.
        </p>
        <div className="rounded-md border border-zinc-800 bg-zinc-950 p-2.5 flex items-center justify-between gap-2 mb-4">
          <code className="font-mono text-xs text-emerald-300 break-all">{issuedKey?.key}</code>
          <button
            onClick={() => {
              if (issuedKey) {
                navigator.clipboard.writeText(issuedKey.key)
                toast.success('Copied')
              }
            }}
            className="shrink-0 rounded bg-zinc-800 hover:bg-zinc-700 px-2 py-1.5 text-zinc-200"
          >
            <Copy size={14} />
          </button>
        </div>
        <button
          onClick={() => setIssuedKey(null)}
          className="w-full rounded-md bg-emerald-500 hover:bg-emerald-400 text-zinc-950 font-medium py-2 text-sm"
        >
          Done
        </button>
      </Modal>
    </>
  )
}

function CreateKeyModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated: (k: { name: string; key: string }) => void
}) {
  const toast = useToast()
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (!name.trim()) return toast.error('Name required')
    setBusy(true)
    try {
      const r = await api.apiKeys.create(name.trim())
      onCreated({ name: r.name, key: r.key })
      setName('')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'create failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="Create API key" size="sm">
      <form onSubmit={submit} className="space-y-3">
        <div>
          <label className="block text-xs text-zinc-400 mb-1">Name</label>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="ci-prod"
            autoFocus
            className="w-full rounded-md bg-zinc-950 border border-zinc-800 px-2.5 py-1.5 text-sm focus:border-emerald-600 focus:outline-none"
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
            disabled={busy}
            className="rounded-md bg-emerald-500 hover:bg-emerald-400 disabled:bg-zinc-800 disabled:text-zinc-500 text-zinc-950 px-3 py-1.5 text-sm font-medium flex items-center gap-1.5"
          >
            {busy && <Spinner size={14} />}
            Create
          </button>
        </div>
      </form>
    </Modal>
  )
}
