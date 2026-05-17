import { useEffect, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { ArrowLeft, Play, Square, Trash2, Camera, Upload, Download, Copy } from 'lucide-react'
import api, { ApiError, getAuthToken } from '../api/client'
import type { ExecResult, FileEntry, Sandbox, Snapshot } from '../api/types'
import StateBadge from '../components/StateBadge'
import Spinner from '../components/Spinner'
import { useToast } from '../components/Toast'
import { formatBytes, formatRelative, formatUptime, memMB } from '../utils/format'
import { Terminal } from 'xterm'
import { FitAddon } from 'xterm-addon-fit'
import 'xterm/css/xterm.css'

type Tab = 'terminal' | 'exec' | 'files' | 'snapshots'

export default function SandboxDetailPage() {
  const { id = '' } = useParams()
  const nav = useNavigate()
  const toast = useToast()

  const [sandbox, setSandbox] = useState<Sandbox | null>(null)
  const [tab, setTab] = useState<Tab>('exec')
  const [busy, setBusy] = useState(false)

  async function load() {
    try {
      const r = await api.sandboxes.get(id)
      setSandbox(r)
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        toast.error('Sandbox not found')
        nav('/sandboxes')
      }
    }
  }

  useEffect(() => {
    load()
    const t = setInterval(load, 4000)
    return () => clearInterval(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id])

  async function action(kind: 'stop' | 'start' | 'destroy') {
    if (!sandbox) return
    setBusy(true)
    try {
      if (kind === 'stop') await api.sandboxes.stop(id)
      else if (kind === 'start') await api.sandboxes.start(id)
      else {
        if (!window.confirm('Destroy this sandbox? This cannot be undone.')) {
          setBusy(false)
          return
        }
        await api.sandboxes.destroy(id)
        toast.success('Destroyed')
        nav('/sandboxes')
        return
      }
      toast.success(`${kind} succeeded`)
      load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `${kind} failed`)
    } finally {
      setBusy(false)
    }
  }

  if (!sandbox) {
    return (
      <div className="p-12 grid place-items-center text-zinc-500">
        <Spinner size={20} />
      </div>
    )
  }

  return (
    <>
      <div className="border-b border-zinc-900 px-6 py-4">
        <button
          onClick={() => nav('/sandboxes')}
          className="text-xs text-zinc-500 hover:text-zinc-200 inline-flex items-center gap-1 mb-2"
        >
          <ArrowLeft size={12} /> Sandboxes
        </button>
        <div className="flex items-start justify-between gap-4">
          <div>
            <div className="flex items-center gap-3">
              <h1 className="text-base font-semibold tracking-tight">{sandbox.name}</h1>
              <StateBadge state={sandbox.state} />
            </div>
            <div className="mt-1 flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-zinc-500 font-mono">
              <span>id {sandbox.id.slice(0, 16)}</span>
              <span>tmpl {sandbox.template_id.slice(0, 12)}</span>
              <span>node {sandbox.node_id?.slice(0, 12) ?? '—'}</span>
              <span>uptime {formatUptime(sandbox.created_at)}</span>
              <span>
                {sandbox.config.vcpus} vCPU · {memMB(sandbox.config.memory_mb)} · {sandbox.config.disk_gb} GB
              </span>
            </div>
          </div>
          <div className="flex gap-2">
            {sandbox.state === 'RUNNING' && !!sandbox.node_id && (
              <Btn onClick={() => action('stop')} busy={busy} icon={<Square size={12} />} label="Stop" />
            )}
            {sandbox.state === 'STOPPED' && (
              <Btn onClick={() => action('start')} busy={busy} icon={<Play size={12} />} label="Start" />
            )}
            <Btn
              onClick={() => action('destroy')}
              busy={busy}
              icon={<Trash2 size={12} />}
              label="Destroy"
              danger
            />
          </div>
        </div>
      </div>

      <div className="border-b border-zinc-900 px-4">
        <div className="flex gap-1">
          {(['terminal', 'exec', 'files', 'snapshots'] as Tab[]).map((t) => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={`px-3 py-2 text-xs uppercase tracking-wider font-mono transition-colors ${
                tab === t
                  ? 'text-teal-300 border-b-2 border-teal-500'
                  : 'text-zinc-500 hover:text-zinc-200 border-b-2 border-transparent'
              }`}
            >
              {t}
            </button>
          ))}
        </div>
      </div>

      <div className="p-6">
        {tab === 'terminal' && <TerminalTab sandboxId={id} state={sandbox.state} />}
        {tab === 'exec' && <ExecTab sandboxId={id} />}
        {tab === 'files' && <FilesTab sandboxId={id} />}
        {tab === 'snapshots' && <SnapshotsTab sandboxId={id} sandboxName={sandbox.name} />}
      </div>
    </>
  )
}

function Btn({
  onClick,
  busy,
  icon,
  label,
  danger,
}: {
  onClick: () => void
  busy: boolean
  icon: React.ReactNode
  label: string
  danger?: boolean
}) {
  return (
    <button
      onClick={onClick}
      disabled={busy}
      className={`inline-flex items-center gap-1.5 rounded border px-2.5 py-1 text-xs font-medium transition-colors disabled:opacity-50 ${
        danger
          ? 'border-red-900 text-red-300 hover:bg-red-950/50'
          : 'border-zinc-800 text-zinc-200 hover:bg-zinc-800'
      }`}
    >
      {busy ? <Spinner size={12} /> : icon}
      {label}
    </button>
  )
}

// --- TERMINAL ---

function TerminalTab({ sandboxId, state }: { sandboxId: string; state: string }) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const [connected, setConnected] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!containerRef.current) return
    const term = new Terminal({
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 13,
      cursorBlink: true,
      theme: {
        background: '#0a0a0a',
        foreground: '#e4e4e7',
        cursor: '#22c55e',
        black: '#0a0a0a',
        green: '#22c55e',
      },
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(containerRef.current)
    fit.fit()
    termRef.current = term

    const onResize = () => fit.fit()
    window.addEventListener('resize', onResize)

    if (state !== 'RUNNING') {
      term.writeln(`\x1b[33m[sandbox is ${state} — start it to open a terminal]\x1b[0m`)
      return () => {
        window.removeEventListener('resize', onResize)
        term.dispose()
      }
    }

    // The terminal route lives outside the Authorization-header auth
    // middleware (a browser WebSocket can't set headers), so the JWT
    // rides in the query string. cols/rows seed the guest PTY size.
    const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
    const token = getAuthToken() ?? ''
    const url =
      `${proto}://${window.location.host}/v1/sandboxes/${sandboxId}/terminal` +
      `?token=${encodeURIComponent(token)}&cols=${term.cols}&rows=${term.rows}`
    const ws = new WebSocket(url)
    ws.binaryType = 'arraybuffer'
    wsRef.current = ws
    const encoder = new TextEncoder()

    ws.onopen = () => {
      setConnected(true)
      term.writeln('\x1b[32m[connected]\x1b[0m')
      // initial resize
      ws.send(JSON.stringify({ resize: [term.rows, term.cols] }))
    }
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        term.write(ev.data)
      } else {
        const arr = new Uint8Array(ev.data as ArrayBuffer)
        term.write(arr)
      }
    }
    ws.onerror = () => setError('WebSocket error')
    ws.onclose = () => {
      setConnected(false)
      term.writeln('\r\n\x1b[31m[disconnected]\x1b[0m')
    }

    // Keystrokes go out as binary frames; master treats binary frames
    // as raw PTY input and reserves text frames for control messages
    // (resize). Sending input as text would let it be mistaken for one.
    const dataDisp = term.onData((d) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(encoder.encode(d))
    })
    const resizeDisp = term.onResize(({ rows, cols }) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ resize: [rows, cols] }))
      }
    })

    return () => {
      window.removeEventListener('resize', onResize)
      dataDisp.dispose()
      resizeDisp.dispose()
      ws.close()
      term.dispose()
    }
  }, [sandboxId, state])

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between text-xs">
        <span className="text-zinc-500">
          {connected ? (
            <span className="text-teal-400 font-mono">● connected</span>
          ) : (
            <span className="text-zinc-500 font-mono">○ disconnected</span>
          )}
        </span>
        {error && <span className="text-red-400">{error}</span>}
      </div>
      <div
        ref={containerRef}
        className="h-[60vh] rounded-lg border border-zinc-900 bg-zinc-950 p-2 overflow-hidden"
      />
    </div>
  )
}

// --- EXEC ---

function ExecTab({ sandboxId }: { sandboxId: string }) {
  const toast = useToast()
  const [cmd, setCmd] = useState('uname -a')
  const [busy, setBusy] = useState(false)
  const [history, setHistory] = useState<{ cmd: string; res: ExecResult; t: string }[]>([])

  async function run() {
    if (!cmd.trim() || busy) return
    setBusy(true)
    try {
      const res = await api.sandboxes.exec(sandboxId, cmd)
      setHistory((h) => [{ cmd, res, t: new Date().toISOString() }, ...h])
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'exec failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex gap-2">
        <input
          value={cmd}
          onChange={(e) => setCmd(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.ctrlKey || e.metaKey || !e.shiftKey)) {
              e.preventDefault()
              run()
            }
          }}
          placeholder="bash -c 'echo hello'"
          className="flex-1 rounded-md bg-zinc-950 border border-zinc-800 px-3 py-2 text-sm font-mono focus:border-teal-600 focus:outline-none focus:ring-2 focus:ring-teal-600/30"
        />
        <button
          onClick={run}
          disabled={busy || !cmd.trim()}
          className="rounded-md bg-teal-500 hover:bg-teal-400 disabled:bg-zinc-800 disabled:text-zinc-500 text-zinc-950 px-4 py-2 text-sm font-medium flex items-center gap-1.5"
        >
          {busy && <Spinner size={14} />} Run
        </button>
      </div>

      <div className="space-y-3">
        {history.length === 0 ? (
          <div className="text-center text-xs text-zinc-500 py-8 border border-dashed border-zinc-800 rounded-lg">
            Run a command to see output here.
          </div>
        ) : (
          history.map((h, i) => (
            <div key={i} className="rounded-lg border border-zinc-900 bg-zinc-950/60 overflow-hidden">
              <div className="flex items-center justify-between px-3 py-2 border-b border-zinc-900 text-xs font-mono">
                <div className="flex items-center gap-2">
                  <span className="text-teal-400">$</span>
                  <span className="text-zinc-300">{h.cmd}</span>
                </div>
                <div className="flex items-center gap-2 text-zinc-500">
                  <span>{formatRelative(h.t)}</span>
                  <span
                    className={`rounded px-1.5 py-0.5 ${
                      h.res.exit_code === 0
                        ? 'bg-teal-500/15 text-teal-300'
                        : 'bg-red-500/15 text-red-300'
                    }`}
                  >
                    exit {h.res.exit_code}
                  </span>
                </div>
              </div>
              {h.res.stdout && (
                <pre className="px-3 py-2 text-xs font-mono text-zinc-200 whitespace-pre-wrap break-all border-b border-zinc-900/50">
                  {h.res.stdout}
                </pre>
              )}
              {h.res.stderr && (
                <pre className="px-3 py-2 text-xs font-mono text-red-300 whitespace-pre-wrap break-all bg-red-950/20">
                  {h.res.stderr}
                </pre>
              )}
            </div>
          ))
        )}
      </div>
    </div>
  )
}

// --- FILES ---

function FilesTab({ sandboxId }: { sandboxId: string }) {
  const toast = useToast()
  const [path, setPath] = useState('/tmp')
  const [items, setItems] = useState<FileEntry[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)

  async function load(p = path) {
    setLoading(true)
    setError(null)
    try {
      const r = await api.sandboxes.listFiles(sandboxId, p)
      setItems(r ?? [])
    } catch (err) {
      setItems([])
      setError(err instanceof Error ? err.message : 'failed to list files')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load(path)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, sandboxId])

  async function onUpload(e: React.ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0]
    if (!file) return
    const target = path.endsWith('/') ? path + file.name : path + '/' + file.name
    try {
      await api.sandboxes.uploadFile(sandboxId, target, file)
      toast.success(`Uploaded ${file.name}`)
      load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'upload failed')
    } finally {
      if (fileInputRef.current) fileInputRef.current.value = ''
    }
  }

  async function onDelete(full: string, name: string) {
    if (!window.confirm(`Delete ${name}? This cannot be undone.`)) return
    try {
      await api.sandboxes.deleteFile(sandboxId, full)
      toast.success(`Deleted ${name}`)
      load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'delete failed')
    }
  }

  return (
    <div className="space-y-3">
      <FileBreadcrumb path={path} onNavigate={setPath} />
      <div className="flex items-center gap-2">
        <input
          value={path}
          onChange={(e) => setPath(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && load()}
          className="flex-1 rounded-md bg-zinc-950 border border-zinc-800 px-3 py-1.5 text-sm font-mono focus:border-teal-600 focus:outline-none"
        />
        <button
          onClick={() => load()}
          className="rounded-md border border-zinc-800 px-3 py-1.5 text-xs hover:bg-zinc-800"
        >
          Refresh
        </button>
        <input ref={fileInputRef} type="file" hidden onChange={onUpload} />
        <button
          onClick={() => fileInputRef.current?.click()}
          className="inline-flex items-center gap-1.5 rounded-md bg-teal-500 hover:bg-teal-400 text-zinc-950 shadow-md shadow-teal-500/20 hover:shadow-teal-500/40 transition-all duration-200 hover:scale-[1.02] px-3 py-1.5 text-sm font-medium"
        >
          <Upload size={14} /> Upload
        </button>
      </div>

      <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 overflow-hidden">
        {loading ? (
          <div className="p-8 text-center text-zinc-500">
            <Spinner size={16} />
          </div>
        ) : error ? (
          <div className="p-8 text-center text-xs text-red-400 font-mono">
            {error}
          </div>
        ) : items.length === 0 ? (
          <div className="p-8 text-center text-xs text-zinc-500">
            No files in {path}
          </div>
        ) : (
          <table className="w-full text-sm">
            <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
              <tr className="border-b border-zinc-900">
                <th className="text-left font-medium px-4 py-2">Name</th>
                <th className="text-left font-medium px-4 py-2">Size</th>
                <th className="text-left font-medium px-4 py-2">Modified</th>
                <th className="text-right font-medium px-4 py-2"></th>
              </tr>
            </thead>
            <tbody>
              {items.map((f) => {
                const full = (path.endsWith('/') ? path : path + '/') + f.name
                return (
                  <tr key={f.name} className="border-b border-zinc-900/50 hover:bg-zinc-800/50 transition-colors">
                    <td
                      className={`px-4 py-2 font-mono text-xs ${
                        f.is_dir ? 'text-teal-300 cursor-pointer' : 'text-zinc-200'
                      }`}
                      onClick={() => {
                        if (f.is_dir) setPath(full)
                      }}
                    >
                      {f.is_dir ? `${f.name}/` : f.name}
                    </td>
                    <td className="px-4 py-2 text-zinc-500 text-xs font-mono">
                      {f.is_dir ? '—' : formatBytes(f.size)}
                    </td>
                    <td className="px-4 py-2 text-zinc-500 text-xs">
                      {formatRelative(f.mod_time)}
                    </td>
                    <td className="px-4 py-2 text-right whitespace-nowrap space-x-1.5">
                      {!f.is_dir && (
                        <>
                          <a
                            href={api.sandboxes.downloadFileURL(sandboxId, full)}
                            download
                            className="inline-flex items-center gap-1 rounded border border-zinc-800 px-2 py-0.5 text-[11px] hover:bg-zinc-800"
                          >
                            <Download size={11} /> Download
                          </a>
                          <button
                            onClick={() => onDelete(full, f.name)}
                            className="inline-flex items-center gap-1 rounded border border-red-900 text-red-300 px-2 py-0.5 text-[11px] hover:bg-red-950/50"
                          >
                            <Trash2 size={11} /> Delete
                          </button>
                        </>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}

// FileBreadcrumb renders the current directory as clickable path
// segments so the user can jump back up the tree.
function FileBreadcrumb({
  path,
  onNavigate,
}: {
  path: string
  onNavigate: (p: string) => void
}) {
  const parts = path.split('/').filter(Boolean)
  return (
    <div className="flex flex-wrap items-center gap-1 text-xs font-mono text-zinc-500">
      <button onClick={() => onNavigate('/')} className="hover:text-teal-300">
        root
      </button>
      {parts.map((part, i) => {
        const sub = '/' + parts.slice(0, i + 1).join('/')
        return (
          <span key={sub} className="flex items-center gap-1">
            <span className="text-zinc-700">/</span>
            <button
              onClick={() => onNavigate(sub)}
              className={i === parts.length - 1 ? 'text-zinc-200' : 'hover:text-teal-300'}
            >
              {part}
            </button>
          </span>
        )
      })}
    </div>
  )
}

// --- SNAPSHOTS ---

function SnapshotsTab({ sandboxId, sandboxName }: { sandboxId: string; sandboxName: string }) {
  const toast = useToast()
  const [items, setItems] = useState<Snapshot[]>([])
  const [busy, setBusy] = useState(false)

  async function load() {
    try {
      const r = await api.sandboxes.listSnapshots(sandboxId)
      setItems(r ?? [])
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'list snapshots failed')
    }
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sandboxId])

  async function takeSnapshot() {
    setBusy(true)
    try {
      const s = await api.sandboxes.snapshot(sandboxId)
      toast.success(`Snapshot ${s.id.slice(0, 8)}… captured`)
      load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'snapshot failed')
    } finally {
      setBusy(false)
    }
  }

  async function restore(id: string) {
    const name = window.prompt('Name for the restored sandbox?', `${sandboxName}-restored`)
    if (!name) return
    try {
      const sb = await api.snapshots.restore(id, name)
      toast.success(`Restored as ${sb.name}`)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'restore failed')
    }
  }

  async function clone(id: string) {
    const name = window.prompt('Name for the cloned sandbox?', `${sandboxName}-clone`)
    if (!name) return
    try {
      const sb = await api.snapshots.clone(id, name)
      toast.success(`Cloned as ${sb.name}`)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'clone failed')
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <button
          onClick={takeSnapshot}
          disabled={busy}
          className="inline-flex items-center gap-1.5 rounded-md bg-teal-500 hover:bg-teal-400 disabled:bg-zinc-800 disabled:text-zinc-500 text-zinc-950 px-3 py-1.5 text-sm font-medium"
        >
          {busy ? <Spinner size={14} /> : <Camera size={14} />}
          New snapshot
        </button>
      </div>

      <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 overflow-hidden">
        {items.length === 0 ? (
          <div className="p-8 text-center text-xs text-zinc-500">No snapshots yet.</div>
        ) : (
          <table className="w-full text-sm">
            <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
              <tr className="border-b border-zinc-900">
                <th className="text-left font-medium px-4 py-2">ID</th>
                <th className="text-left font-medium px-4 py-2">Size</th>
                <th className="text-left font-medium px-4 py-2">Node</th>
                <th className="text-left font-medium px-4 py-2">Created</th>
                <th className="text-right font-medium px-4 py-2">Actions</th>
              </tr>
            </thead>
            <tbody>
              {items.map((s) => (
                <tr key={s.id} className="border-b border-zinc-900/50 hover:bg-zinc-800/50 transition-colors">
                  <td className="px-4 py-2 font-mono text-xs flex items-center gap-1">
                    {s.id.slice(0, 16)}
                    <button
                      onClick={() => {
                        navigator.clipboard.writeText(s.id)
                        toast.success('Copied')
                      }}
                      className="text-zinc-600 hover:text-zinc-200"
                    >
                      <Copy size={11} />
                    </button>
                  </td>
                  <td className="px-4 py-2 text-zinc-300 text-xs font-mono">
                    {formatBytes(s.size_bytes)}
                  </td>
                  <td className="px-4 py-2 text-zinc-500 text-xs font-mono">
                    {s.node_id.slice(0, 8)}
                  </td>
                  <td className="px-4 py-2 text-zinc-500 text-xs">
                    {formatRelative(s.created_at)}
                  </td>
                  <td className="px-4 py-2 text-right space-x-1.5">
                    <button
                      onClick={() => restore(s.id)}
                      className="rounded border border-zinc-800 px-2 py-0.5 text-[11px] hover:bg-zinc-800"
                    >
                      Restore
                    </button>
                    <button
                      onClick={() => clone(s.id)}
                      className="rounded border border-zinc-800 px-2 py-0.5 text-[11px] hover:bg-zinc-800"
                    >
                      Clone
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}
