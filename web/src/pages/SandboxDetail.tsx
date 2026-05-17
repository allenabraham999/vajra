import { useEffect, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { ArrowLeft, Play, Square, Trash2, Camera, Upload, Download, Copy, Pause, Eraser } from 'lucide-react'
import api, { ApiError, getAuthToken, type LogEntry, type LogSource } from '../api/client'
import type { ExecResult, FileEntry, Sandbox, Snapshot } from '../api/types'
import StateBadge from '../components/StateBadge'
import Spinner from '../components/Spinner'
import { useToast } from '../components/Toast'
import { formatBytes, formatRelative, formatUptime, memMB } from '../utils/format'
import { copyToClipboard } from '../utils/clipboard'
import { Terminal } from 'xterm'
import { FitAddon } from 'xterm-addon-fit'
import 'xterm/css/xterm.css'

type Tab = 'terminal' | 'exec' | 'files' | 'snapshots' | 'logs'

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
        <GitCloneBanner sandbox={sandbox} />
      </div>

      <div className="border-b border-zinc-900 px-4">
        <div className="flex gap-1">
          {(['terminal', 'exec', 'files', 'snapshots', 'logs'] as Tab[]).map((t) => (
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
        {tab === 'logs' && <LogsTab sandboxId={id} />}
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

// GitCloneBanner surfaces the post-create git auto-clone hook: a spinner
// while the repo is cloning, a warning when it failed, a subtle
// confirmation once it finished. Renders nothing when no repo was
// requested at create time.
function GitCloneBanner({ sandbox }: { sandbox: Sandbox }) {
  if (!sandbox.git_url) return null
  const repo = <span className="font-mono text-zinc-200">{sandbox.git_url}</span>

  if (sandbox.git_clone_status === 'failed') {
    return (
      <div className="mt-3 rounded-md border border-amber-900/60 bg-amber-950/30 px-3 py-2 text-xs text-amber-300">
        <span className="font-medium">Git clone failed:</span>{' '}
        <span className="font-mono break-all">
          {sandbox.git_clone_error || 'unknown error'}
        </span>
      </div>
    )
  }
  if (sandbox.git_clone_status === 'done') {
    return (
      <div className="mt-3 rounded-md border border-teal-900/60 bg-teal-950/20 px-3 py-2 text-xs text-teal-300">
        Cloned {repo} into <span className="font-mono">/workspace</span>
      </div>
    )
  }
  // '' | 'pending' | 'cloning' — the clone is still in flight.
  return (
    <div className="mt-3 flex items-center gap-2 rounded-md border border-zinc-800 bg-zinc-900/50 px-3 py-2 text-xs text-zinc-300">
      <Spinner size={12} />
      <span>
        Cloning {repo} into <span className="font-mono">/workspace</span>…
      </span>
    </div>
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
                      onClick={async () => {
                        const ok = await copyToClipboard(s.id)
                        if (ok) toast.success('Copied!')
                        else toast.error('Copy failed')
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

// --- LOGS ---

type LogFilter = LogSource | 'all'

// SOURCE_META drives the per-source badge in the log viewer: a single
// letter (M/A/G) tinted to tell master, agent, and guest lines apart.
const SOURCE_META: Record<LogSource, { badge: string; cls: string }> = {
  master: { badge: 'M', cls: 'bg-teal-500/15 text-teal-300' },
  agent: { badge: 'A', cls: 'bg-sky-500/15 text-sky-300' },
  guest: { badge: 'G', cls: 'bg-violet-500/15 text-violet-300' },
}

// levelClass colours a line by severity: INFO zinc, WARN amber, ERROR red.
function levelClass(level: string): string {
  if (level === 'ERROR') return 'text-red-300'
  if (level === 'WARN') return 'text-amber-300'
  return 'text-zinc-300'
}

// logKey is the dedupe identity of an entry — the same key the master
// stream handler uses — so the REST backlog and the live socket can
// overlap without producing visible duplicates.
function logKey(e: LogEntry): string {
  return `${e.source}|${e.timestamp}|${e.message}`
}

function LogsTab({ sandboxId }: { sandboxId: string }) {
  const toast = useToast()
  const [entries, setEntries] = useState<LogEntry[]>([])
  const [filter, setFilter] = useState<LogFilter>('all')
  const [paused, setPaused] = useState(false)
  const [connected, setConnected] = useState(false)

  // allRef accumulates every entry regardless of pause state; entries
  // (the rendered list) is only refreshed from it while not paused.
  const allRef = useRef<LogEntry[]>([])
  const seenRef = useRef<Set<string>>(new Set())
  const pausedRef = useRef(false)
  const scrollRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    pausedRef.current = paused
  }, [paused])

  // ingest merges new entries, dedupes, keeps chronological order, and
  // (unless paused) flushes the result to the rendered list.
  function ingest(incoming: LogEntry[]) {
    let changed = false
    for (const e of incoming) {
      const k = logKey(e)
      if (seenRef.current.has(k)) continue
      seenRef.current.add(k)
      allRef.current.push(e)
      changed = true
    }
    if (!changed) return
    allRef.current.sort(
      (a, b) => new Date(a.timestamp).getTime() - new Date(b.timestamp).getTime(),
    )
    if (!pausedRef.current) setEntries([...allRef.current])
  }

  useEffect(() => {
    let cancelled = false
    // Initial backlog over plain HTTP, then a live tail over WebSocket.
    api.sandboxes
      .logs(sandboxId, 'all', 500)
      .then((r) => {
        if (!cancelled) ingest(r.entries ?? [])
      })
      .catch(() => {})

    // The stream route sits outside the Authorization-header middleware
    // (a browser WebSocket can't set headers), so the JWT rides in the
    // query string — same pattern as the terminal.
    const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
    const token = getAuthToken() ?? ''
    const url =
      `${proto}://${window.location.host}/v1/sandboxes/${sandboxId}/logs/stream` +
      `?token=${encodeURIComponent(token)}`
    const ws = new WebSocket(url)
    ws.onopen = () => setConnected(true)
    ws.onmessage = (ev) => {
      if (typeof ev.data !== 'string') return
      try {
        ingest(JSON.parse(ev.data) as LogEntry[])
      } catch {
        /* ignore a malformed frame */
      }
    }
    ws.onerror = () => setConnected(false)
    ws.onclose = () => setConnected(false)

    return () => {
      cancelled = true
      ws.close()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sandboxId])

  // Auto-scroll to the newest line as the list grows, unless paused.
  useEffect(() => {
    if (paused) return
    const el = scrollRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [entries, paused])

  function resume() {
    setEntries([...allRef.current])
    setPaused(false)
  }

  function clear() {
    allRef.current = []
    seenRef.current = new Set()
    setEntries([])
  }

  function download() {
    const text = allRef.current
      .map((e) => `${e.timestamp} [${e.source.toUpperCase()}] ${e.level} ${e.message}`)
      .join('\n')
    const blob = new Blob([text + '\n'], { type: 'text/plain' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = `sandbox-${sandboxId.slice(0, 12)}-logs.log`
    a.click()
    URL.revokeObjectURL(a.href)
    toast.success('Logs downloaded')
  }

  const shown = entries.filter((e) => filter === 'all' || e.source === filter)
  const buffered = allRef.current.length - entries.length

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-1">
          {(['all', 'master', 'agent', 'guest'] as LogFilter[]).map((f) => (
            <button
              key={f}
              onClick={() => setFilter(f)}
              className={`rounded px-2 py-1 text-[11px] uppercase tracking-wider font-mono transition-colors ${
                filter === f
                  ? 'bg-teal-500/15 text-teal-300'
                  : 'text-zinc-500 hover:text-zinc-200'
              }`}
            >
              {f}
            </button>
          ))}
        </div>
        <div className="flex items-center gap-2">
          <span className="text-xs font-mono">
            {connected ? (
              <span className="text-teal-400">● live</span>
            ) : (
              <span className="text-zinc-500">○ offline</span>
            )}
          </span>
          <LogBtn
            onClick={() => (paused ? resume() : setPaused(true))}
            icon={paused ? <Play size={12} /> : <Pause size={12} />}
            label={paused ? 'Resume' : 'Pause'}
          />
          <LogBtn onClick={clear} icon={<Eraser size={12} />} label="Clear" />
          <LogBtn onClick={download} icon={<Download size={12} />} label="Download" />
        </div>
      </div>

      <div
        ref={scrollRef}
        className="h-[60vh] overflow-y-auto rounded-lg border border-zinc-900 bg-zinc-950 p-3 font-mono text-xs leading-relaxed"
      >
        {shown.length === 0 ? (
          <div className="grid h-full place-items-center text-zinc-600">
            No log entries{filter !== 'all' ? ` for ${filter}` : ''} yet.
          </div>
        ) : (
          shown.map((e, i) => {
            const meta = SOURCE_META[e.source] ?? SOURCE_META.agent
            return (
              <div key={i} className="flex gap-2 py-0.5 hover:bg-zinc-900/40">
                <span className="shrink-0 text-zinc-600">
                  {new Date(e.timestamp).toLocaleTimeString()}
                </span>
                <span
                  className={`shrink-0 rounded px-1 font-bold ${meta.cls}`}
                  title={e.source}
                >
                  {meta.badge}
                </span>
                <span className={`whitespace-pre-wrap break-all ${levelClass(e.level)}`}>
                  {e.message}
                </span>
              </div>
            )
          })
        )}
      </div>
      {paused && (
        <div className="text-[11px] font-mono text-amber-400">
          paused — {buffered > 0 ? `${buffered} new line(s) buffered` : 'no new lines'}
        </div>
      )}
    </div>
  )
}

// LogBtn is the small bordered action button shared by the logs toolbar.
function LogBtn({
  onClick,
  icon,
  label,
}: {
  onClick: () => void
  icon: React.ReactNode
  label: string
}) {
  return (
    <button
      onClick={onClick}
      className="inline-flex items-center gap-1 rounded border border-zinc-800 px-2 py-1 text-xs text-zinc-300 hover:bg-zinc-800 transition-colors"
    >
      {icon}
      {label}
    </button>
  )
}
