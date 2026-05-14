import { useEffect, useState } from 'react'
import { Webhook as WebhookIcon, Plus, Trash2 } from 'lucide-react'
import api from '../api/client'
import type { Webhook, WebhookEventName } from '../api/types'
import PageHeader from '../components/PageHeader'
import EmptyState from '../components/EmptyState'
import Modal from '../components/Modal'
import Spinner from '../components/Spinner'
import { useToast } from '../components/Toast'
import { formatRelative } from '../utils/format'

const ALL_EVENTS: WebhookEventName[] = [
  'sandbox.created',
  'sandbox.running',
  'sandbox.stopped',
  'sandbox.destroyed',
  'sandbox.error',
  'sandbox.archived',
]

export default function WebhooksPage() {
  const toast = useToast()
  const [items, setItems] = useState<Webhook[]>([])
  const [openCreate, setOpenCreate] = useState(false)

  async function load() {
    try {
      const r = await api.webhooks.list()
      setItems(r)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'list webhooks failed')
    }
  }

  useEffect(() => {
    load()
  }, [])

  async function onDelete(id: string) {
    if (!confirm('Delete this webhook?')) return
    try {
      await api.webhooks.delete(id)
      toast.success('Webhook deleted')
      load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'delete failed')
    }
  }

  async function onTest(id: string) {
    try {
      const r = await api.webhooks.test(id)
      if (r.delivered) toast.success('Delivered ✓')
      else toast.error('Receiver did not return 2xx (after retries)')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'test fire failed')
    }
  }

  return (
    <>
      <PageHeader
        title="Webhooks"
        description="HMAC-signed POSTs sent to your receiver when sandbox lifecycle events fire."
        actions={
          <button
            onClick={() => setOpenCreate(true)}
            className="inline-flex items-center gap-1.5 rounded-md bg-teal-500 hover:bg-teal-400 text-zinc-950 shadow-md shadow-teal-500/20 hover:shadow-teal-500/40 transition-all duration-200 hover:scale-[1.02] px-3 py-1.5 text-sm font-medium"
          >
            <Plus size={14} /> Add webhook
          </button>
        }
      />
      <div className="p-6">
        {items.length === 0 ? (
          <EmptyState
            icon={<WebhookIcon size={32} />}
            title="No webhooks configured"
            description="Wire vajra-master to Slack, your incident pipeline, or any HTTPS receiver. Payloads are HMAC-SHA256 signed (X-Vajra-Signature)."
          />
        ) : (
          <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 overflow-hidden">
            <table className="w-full text-sm">
              <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
                <tr className="border-b border-zinc-900 bg-zinc-950/40">
                  <th className="text-left font-medium px-4 py-2.5">URL</th>
                  <th className="text-left font-medium px-4 py-2.5">Events</th>
                  <th className="text-left font-medium px-4 py-2.5">Active</th>
                  <th className="text-left font-medium px-4 py-2.5">Created</th>
                  <th className="text-right font-medium px-4 py-2.5">Actions</th>
                </tr>
              </thead>
              <tbody>
                {items.map((w) => (
                  <tr key={w.id} className="border-b border-zinc-900/50 hover:bg-zinc-800/50 transition-colors">
                    <td className="px-4 py-2.5 font-mono text-xs">{w.url}</td>
                    <td className="px-4 py-2.5 text-xs text-zinc-400">
                      {w.events.join(', ')}
                    </td>
                    <td className="px-4 py-2.5 text-xs">
                      {w.active ? 'yes' : 'no'}
                    </td>
                    <td className="px-4 py-2.5 text-xs text-zinc-500">
                      {formatRelative(w.created_at)}
                    </td>
                    <td className="px-4 py-2.5 text-right">
                      <button
                        onClick={() => onTest(w.id)}
                        className="text-xs text-teal-400 hover:underline mr-3"
                      >
                        Test
                      </button>
                      <button
                        onClick={() => onDelete(w.id)}
                        className="text-zinc-500 hover:text-red-400 inline-flex items-center gap-1"
                      >
                        <Trash2 size={14} />
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <CreateWebhookModal
        open={openCreate}
        onClose={() => setOpenCreate(false)}
        onCreated={() => {
          setOpenCreate(false)
          load()
        }}
      />
    </>
  )
}

function CreateWebhookModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated: () => void
}) {
  const toast = useToast()
  const [url, setUrl] = useState('https://')
  const [selected, setSelected] = useState<Set<WebhookEventName>>(
    new Set(['sandbox.created', 'sandbox.error']),
  )
  const [busy, setBusy] = useState(false)
  const [secret, setSecret] = useState<string | null>(null)

  function toggle(e: WebhookEventName) {
    const next = new Set(selected)
    if (next.has(e)) next.delete(e)
    else next.add(e)
    setSelected(next)
  }

  async function submit(ev: React.FormEvent) {
    ev.preventDefault()
    if (selected.size === 0) {
      toast.error('Pick at least one event')
      return
    }
    setBusy(true)
    try {
      const w = await api.webhooks.create(url, Array.from(selected))
      setSecret(w.secret || '')
      toast.success('Webhook created')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'create failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="Add webhook" size="md">
      {secret ? (
        <div className="space-y-3">
          <p className="text-sm text-zinc-300">
            Save this signing secret — it will not be shown again:
          </p>
          <pre className="rounded-md border border-teal-800 bg-zinc-950 p-3 font-mono text-xs text-teal-300 break-all whitespace-pre-wrap">
            {secret}
          </pre>
          <div className="flex justify-end">
            <button
              type="button"
              onClick={() => {
                setSecret(null)
                setUrl('https://')
                setSelected(new Set(['sandbox.created', 'sandbox.error']))
                onCreated()
              }}
              className="rounded-md bg-teal-500 hover:bg-teal-400 text-zinc-950 shadow-md shadow-teal-500/20 hover:shadow-teal-500/40 transition-all duration-200 hover:scale-[1.02] px-3 py-1.5 text-sm font-medium"
            >
              Done
            </button>
          </div>
        </div>
      ) : (
        <form onSubmit={submit} className="space-y-3">
          <div>
            <label className="block text-xs text-zinc-400 mb-1">Receiver URL</label>
            <input
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              className="w-full rounded-md bg-zinc-950 border border-zinc-800 px-2.5 py-1.5 text-sm font-mono focus:border-teal-600 focus:outline-none"
              required
            />
          </div>
          <div>
            <label className="block text-xs text-zinc-400 mb-1">Events</label>
            <div className="grid grid-cols-2 gap-1.5">
              {ALL_EVENTS.map((e) => (
                <label
                  key={e}
                  className="flex items-center gap-2 rounded border border-zinc-800 px-2 py-1.5 text-xs cursor-pointer hover:bg-zinc-900"
                >
                  <input
                    type="checkbox"
                    checked={selected.has(e)}
                    onChange={() => toggle(e)}
                  />
                  <span className="font-mono">{e}</span>
                </label>
              ))}
            </div>
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
              className="rounded-md bg-teal-500 hover:bg-teal-400 disabled:bg-zinc-800 disabled:text-zinc-500 text-zinc-950 px-3 py-1.5 text-sm font-medium flex items-center gap-1.5"
            >
              {busy && <Spinner size={14} />}
              Create
            </button>
          </div>
        </form>
      )}
    </Modal>
  )
}
