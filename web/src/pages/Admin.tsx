import { useCallback, useEffect, useState } from 'react'
import {
  AlertTriangle,
  Boxes,
  LayoutDashboard,
  ScrollText,
  Server,
  ShieldCheck,
  Users,
} from 'lucide-react'
import api from '../api/client'
import type {
  AdminAccount,
  AdminLogEntry,
  AdminNode,
  AdminOverview,
  AdminSandbox,
} from '../api/types'
import { useAuth } from '../auth/AuthContext'
import PageHeader from '../components/PageHeader'
import StateBadge from '../components/StateBadge'
import ProgressBar from '../components/ProgressBar'
import Modal from '../components/Modal'
import Spinner from '../components/Spinner'
import EmptyState from '../components/EmptyState'
import { useToast } from '../components/Toast'
import { formatRelative, memMB } from '../utils/format'

type TabId = 'overview' | 'nodes' | 'sandboxes' | 'accounts' | 'logs'

const TABS: { id: TabId; label: string; icon: typeof Boxes }[] = [
  { id: 'overview', label: 'Cluster Overview', icon: LayoutDashboard },
  { id: 'nodes', label: 'Nodes', icon: Server },
  { id: 'sandboxes', label: 'Sandboxes', icon: Boxes },
  { id: 'accounts', label: 'Accounts', icon: Users },
  { id: 'logs', label: 'Logs', icon: ScrollText },
]

// errMsg unwraps an unknown thrown value into a display string.
function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : 'request failed'
}

// humanizeSeconds renders an age in seconds as a compact duration.
function humanizeSeconds(s: number): string {
  if (s < 60) return `${Math.floor(s)}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h`
  return `${Math.floor(h / 24)}d`
}

export default function AdminPage() {
  const { isAdmin } = useAuth()
  const [tab, setTab] = useState<TabId>('overview')
  // logSandbox is set by the Sandboxes tab's "view logs" action so the
  // Logs tab opens pre-filtered to that sandbox.
  const [logSandbox, setLogSandbox] = useState('')

  const viewSandboxLogs = useCallback((id: string) => {
    setLogSandbox(id)
    setTab('logs')
  }, [])

  if (!isAdmin) {
    return (
      <>
        <PageHeader title="Admin" description="Operator-only console." />
        <div className="p-6">
          <EmptyState
            icon={<ShieldCheck size={28} />}
            title="Admin access required"
            description="Your account is not an operator. Ask an admin to grant access."
          />
        </div>
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Admin"
        description="Operator console — nodes, sandboxes, accounts, and logs across all tenants."
        actions={<ShieldCheck size={14} className="text-teal-400" />}
      />
      <div className="px-6 pt-4">
        <div className="flex gap-1 border-b border-zinc-900">
          {TABS.map(({ id, label, icon: Icon }) => (
            <button
              key={id}
              onClick={() => setTab(id)}
              className={`flex items-center gap-1.5 px-3 py-2 text-xs font-medium border-b-2 -mb-px transition-colors ${
                tab === id
                  ? 'border-teal-500 text-zinc-50'
                  : 'border-transparent text-zinc-500 hover:text-zinc-200'
              }`}
            >
              <Icon size={14} />
              {label}
            </button>
          ))}
        </div>
      </div>
      <div className="p-6">
        {tab === 'overview' && <OverviewTab />}
        {tab === 'nodes' && <NodesTab />}
        {tab === 'sandboxes' && <SandboxesTab onViewLogs={viewSandboxLogs} />}
        {tab === 'accounts' && <AccountsTab />}
        {tab === 'logs' && <LogsTab initialSandbox={logSandbox} />}
      </div>
    </>
  )
}

// ---- shared bits ----

function Card({ title, children }: { title?: string; children: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 overflow-hidden">
      {title && (
        <div className="px-4 py-3 border-b border-zinc-900">
          <h2 className="text-sm font-medium">{title}</h2>
        </div>
      )}
      {children}
    </div>
  )
}

function Tile({ label, value, hint }: { label: string; value: string | number; hint?: string }) {
  return (
    <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 shadow-lg shadow-black/20 p-4">
      <div className="text-[11px] text-zinc-500 uppercase tracking-wider font-mono">{label}</div>
      <div className="mt-2 text-2xl font-semibold tabular-nums">{value}</div>
      {hint && <div className="mt-1 text-[11px] text-zinc-500">{hint}</div>}
    </div>
  )
}

function ActionBtn({
  onClick,
  children,
  tone = 'default',
  disabled,
}: {
  onClick: () => void
  children: React.ReactNode
  tone?: 'default' | 'danger'
  disabled?: boolean
}) {
  const toneCls =
    tone === 'danger'
      ? 'hover:bg-red-500/10 hover:text-red-300 hover:border-red-500/40'
      : 'hover:bg-zinc-800 hover:text-zinc-100'
  return (
    <button
      disabled={disabled}
      onClick={onClick}
      className={`rounded border border-zinc-700/70 px-2 py-1 text-[11px] font-medium text-zinc-400 transition-colors disabled:opacity-30 disabled:cursor-not-allowed ${toneCls}`}
    >
      {children}
    </button>
  )
}

const TH = 'text-left font-medium px-4 py-2'
const TR = 'border-b border-zinc-900/50'
const TD = 'px-4 py-2'

function Loading() {
  return (
    <div className="flex items-center justify-center gap-2 py-16 text-sm text-zinc-500">
      <Spinner size={16} /> Loading…
    </div>
  )
}

// ---- Overview tab ----

function OverviewTab() {
  const toast = useToast()
  const [data, setData] = useState<AdminOverview | null>(null)

  useEffect(() => {
    let alive = true
    async function load() {
      try {
        const d = await api.admin.overview()
        if (alive) setData(d)
      } catch (e) {
        if (alive) toast.error(errMsg(e))
      }
    }
    load()
    const t = setInterval(load, 8000)
    return () => {
      alive = false
      clearInterval(t)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  if (!data) return <Loading />

  return (
    <div className="space-y-6">
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
        <Tile
          label="Nodes"
          value={`${data.active_nodes} / ${data.total_nodes}`}
          hint="active / total"
        />
        <Tile
          label="Sandboxes"
          value={`${data.running_sandboxes} / ${data.total_sandboxes}`}
          hint="running / total"
        />
        <Tile label="Accounts" value={data.total_accounts} hint="registered tenants" />
        <Tile
          label="Spend today"
          value={`$${data.spend_today.toFixed(2)}`}
          hint="metered cost since 00:00 UTC"
        />
      </div>

      <Card title="Cluster capacity">
        <div className="p-4 space-y-4">
          <ProgressBar
            value={data.used.vcpu}
            max={data.capacity.vcpu}
            label={`vCPU — ${data.used.vcpu} / ${data.capacity.vcpu}`}
          />
          <ProgressBar
            value={data.used.memory_mb}
            max={data.capacity.memory_mb}
            label={`Memory — ${memMB(data.used.memory_mb)} / ${memMB(data.capacity.memory_mb)}`}
          />
          <ProgressBar
            value={data.used.disk_gb}
            max={data.capacity.disk_gb}
            label={`Disk — ${data.used.disk_gb} / ${data.capacity.disk_gb} GB`}
          />
        </div>
      </Card>

      <Card title={`Active alerts (${data.alerts.length})`}>
        {data.alerts.length === 0 ? (
          <div className="p-6 text-center text-xs text-emerald-400">
            All clear — no active alerts.
          </div>
        ) : (
          <ul className="divide-y divide-zinc-900">
            {data.alerts.map((a, i) => (
              <li key={i} className="flex items-center gap-3 px-4 py-2.5">
                <AlertTriangle
                  size={15}
                  className={a.level === 'critical' ? 'text-red-400' : 'text-amber-400'}
                />
                <span
                  className={`text-[10px] font-mono uppercase tracking-wider rounded px-1.5 py-0.5 ring-1 ring-inset ${
                    a.level === 'critical'
                      ? 'bg-red-500/10 text-red-300 ring-red-500/30'
                      : 'bg-amber-500/10 text-amber-300 ring-amber-500/30'
                  }`}
                >
                  {a.level}
                </span>
                <span className="text-sm text-zinc-300">{a.message}</span>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  )
}

// ---- Nodes tab ----

function NodesTab() {
  const toast = useToast()
  const [nodes, setNodes] = useState<AdminNode[] | null>(null)
  const [busy, setBusy] = useState('')

  const load = useCallback(async () => {
    try {
      setNodes(await api.admin.nodes())
    } catch (e) {
      toast.error(errMsg(e))
    }
  }, [toast])

  useEffect(() => {
    load()
    const t = setInterval(load, 8000)
    return () => clearInterval(t)
  }, [load])

  async function act(
    id: string,
    label: string,
    fn: () => Promise<unknown>,
    confirmMsg?: string,
  ) {
    if (confirmMsg && !window.confirm(confirmMsg)) return
    setBusy(id)
    try {
      await fn()
      toast.success(`${label} ok`)
      await load()
    } catch (e) {
      toast.error(errMsg(e))
    } finally {
      setBusy('')
    }
  }

  if (!nodes) return <Loading />

  return (
    <Card title={`Nodes (${nodes.length})`}>
      {nodes.length === 0 ? (
        <div className="p-8 text-center text-xs text-zinc-500">No nodes registered.</div>
      ) : (
        <table className="w-full text-sm">
          <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
            <tr className="border-b border-zinc-900">
              <th className={TH}>Hostname</th>
              <th className={TH}>State</th>
              <th className={TH}>Cluster</th>
              <th className={TH}>vCPU</th>
              <th className={TH}>Memory</th>
              <th className={TH}>Sandboxes</th>
              <th className={TH}>Heartbeat</th>
              <th className={TH}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {nodes.map((n) => {
              const isBusy = busy === n.id
              return (
                <tr key={n.id} className={TR}>
                  <td className={`${TD} font-medium`}>{n.hostname}</td>
                  <td className={TD}>
                    <StateBadge state={n.state} />
                  </td>
                  <td className={`${TD} font-mono text-xs text-zinc-500`}>{n.cluster_id}</td>
                  <td className={`${TD} w-36`}>
                    <ProgressBar
                      value={n.used_resources.used_cpu}
                      max={n.capacity.total_cpu}
                      label={`${n.used_resources.used_cpu}/${n.capacity.total_cpu}`}
                    />
                  </td>
                  <td className={`${TD} w-36`}>
                    <ProgressBar
                      value={n.used_resources.used_memory_mb}
                      max={n.capacity.total_memory_mb}
                      label={memMB(n.capacity.total_memory_mb)}
                    />
                  </td>
                  <td className={`${TD} font-mono text-xs text-zinc-400`}>{n.sandbox_count}</td>
                  <td className={`${TD} text-xs ${n.healthy ? 'text-zinc-400' : 'text-red-400'}`}>
                    {formatRelative(n.last_heartbeat)}
                  </td>
                  <td className={TD}>
                    <div className="flex gap-1.5">
                      <ActionBtn
                        disabled={isBusy || n.state !== 'ACTIVE'}
                        onClick={() => act(n.id, 'drain', () => api.admin.drainNode(n.id))}
                      >
                        Drain
                      </ActionBtn>
                      <ActionBtn
                        disabled={isBusy || n.state !== 'ACTIVE'}
                        onClick={() => act(n.id, 'cordon', () => api.admin.cordonNode(n.id))}
                      >
                        Cordon
                      </ActionBtn>
                      <ActionBtn
                        disabled={isBusy || (n.state !== 'DRAINING' && n.state !== 'CORDONED')}
                        onClick={() => act(n.id, 'uncordon', () => api.admin.uncordonNode(n.id))}
                      >
                        Uncordon
                      </ActionBtn>
                      <ActionBtn
                        tone="danger"
                        disabled={isBusy}
                        onClick={() =>
                          act(
                            n.id,
                            'terminate',
                            () => api.admin.terminateNode(n.id),
                            `Terminate node ${n.hostname}? This destroys the EC2 instance.`,
                          )
                        }
                      >
                        Terminate
                      </ActionBtn>
                    </div>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      )}
    </Card>
  )
}

// ---- Sandboxes tab ----

const SANDBOX_STATES = [
  'PENDING',
  'CREATING',
  'RUNNING',
  'PAUSED',
  'STOPPED',
  'ARCHIVED',
  'DESTROYED',
  'ERROR',
]
const PAGE_SIZE = 50

function SandboxesTab({ onViewLogs }: { onViewLogs: (id: string) => void }) {
  const toast = useToast()
  const [rows, setRows] = useState<AdminSandbox[] | null>(null)
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(0)
  const [stateFilter, setStateFilter] = useState('')
  const [accountFilter, setAccountFilter] = useState('')
  const [busy, setBusy] = useState('')

  const load = useCallback(async () => {
    try {
      const r = await api.admin.sandboxes({
        state: stateFilter || undefined,
        account: accountFilter || undefined,
        limit: PAGE_SIZE,
        offset: page * PAGE_SIZE,
      })
      setRows(r.sandboxes)
      setTotal(r.total)
    } catch (e) {
      toast.error(errMsg(e))
    }
  }, [stateFilter, accountFilter, page, toast])

  useEffect(() => {
    load()
    const t = setInterval(load, 8000)
    return () => clearInterval(t)
  }, [load])

  async function act(id: string, label: string, fn: () => Promise<unknown>, confirmMsg?: string) {
    if (confirmMsg && !window.confirm(confirmMsg)) return
    setBusy(id)
    try {
      await fn()
      toast.success(`${label} ok`)
      await load()
    } catch (e) {
      toast.error(errMsg(e))
    } finally {
      setBusy('')
    }
  }

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <select
          value={stateFilter}
          onChange={(e) => {
            setStateFilter(e.target.value)
            setPage(0)
          }}
          className="rounded-md bg-zinc-950 border border-zinc-800 px-2.5 py-1.5 text-xs focus:border-teal-500 focus:outline-none"
        >
          <option value="">All states</option>
          {SANDBOX_STATES.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </select>
        <input
          value={accountFilter}
          onChange={(e) => {
            setAccountFilter(e.target.value)
            setPage(0)
          }}
          placeholder="Filter by account email…"
          className="rounded-md bg-zinc-950 border border-zinc-800 px-2.5 py-1.5 text-xs w-56 focus:border-teal-500 focus:outline-none"
        />
        <span className="text-[11px] text-zinc-500 ml-auto">
          {total} sandbox{total === 1 ? '' : 'es'}
        </span>
      </div>

      <Card>
        {!rows ? (
          <Loading />
        ) : rows.length === 0 ? (
          <div className="p-8 text-center text-xs text-zinc-500">No sandboxes match.</div>
        ) : (
          <table className="w-full text-sm">
            <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
              <tr className="border-b border-zinc-900">
                <th className={TH}>Name</th>
                <th className={TH}>Account</th>
                <th className={TH}>State</th>
                <th className={TH}>Node</th>
                <th className={TH}>Resources</th>
                <th className={TH}>Age</th>
                <th className={TH}>Actions</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((sb) => {
                const isBusy = busy === sb.id
                return (
                  <tr key={sb.id} className={TR}>
                    <td className={`${TD} font-medium`}>
                      {sb.name}
                      <div className="font-mono text-[10px] text-zinc-600">{sb.id.slice(0, 16)}</div>
                    </td>
                    <td className={`${TD} text-xs text-zinc-400`}>{sb.account_email || '—'}</td>
                    <td className={TD}>
                      <StateBadge state={sb.state} />
                    </td>
                    <td className={`${TD} font-mono text-xs text-zinc-500`}>
                      {sb.node_hostname || '—'}
                    </td>
                    <td className={`${TD} text-xs font-mono text-zinc-400`}>
                      {sb.config.vcpus} vCPU · {memMB(sb.config.memory_mb)}
                    </td>
                    <td className={`${TD} text-xs text-zinc-400`}>
                      {humanizeSeconds(sb.age_seconds)}
                    </td>
                    <td className={TD}>
                      <div className="flex gap-1.5">
                        <ActionBtn
                          disabled={isBusy || sb.state !== 'RUNNING'}
                          onClick={() =>
                            act(sb.id, 'stop', () => api.admin.stopSandbox(sb.id))
                          }
                        >
                          Stop
                        </ActionBtn>
                        <ActionBtn
                          tone="danger"
                          disabled={isBusy || sb.state === 'DESTROYED'}
                          onClick={() =>
                            act(
                              sb.id,
                              'destroy',
                              () => api.admin.destroySandbox(sb.id),
                              `Destroy sandbox ${sb.name}?`,
                            )
                          }
                        >
                          Destroy
                        </ActionBtn>
                        <ActionBtn onClick={() => onViewLogs(sb.id)}>Logs</ActionBtn>
                      </div>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </Card>

      {total > PAGE_SIZE && (
        <div className="flex items-center justify-end gap-2 text-xs">
          <ActionBtn disabled={page === 0} onClick={() => setPage((p) => p - 1)}>
            Prev
          </ActionBtn>
          <span className="text-zinc-500">
            Page {page + 1} of {Math.ceil(total / PAGE_SIZE)}
          </span>
          <ActionBtn
            disabled={(page + 1) * PAGE_SIZE >= total}
            onClick={() => setPage((p) => p + 1)}
          >
            Next
          </ActionBtn>
        </div>
      )}
    </div>
  )
}

// ---- Accounts tab ----

function AccountsTab() {
  const toast = useToast()
  const [accounts, setAccounts] = useState<AdminAccount[] | null>(null)
  const [busy, setBusy] = useState('')
  const [creditTarget, setCreditTarget] = useState<AdminAccount | null>(null)
  const [pwResult, setPwResult] = useState<{ email: string; password: string } | null>(null)

  const load = useCallback(async () => {
    try {
      setAccounts(await api.admin.accounts())
    } catch (e) {
      toast.error(errMsg(e))
    }
  }, [toast])

  useEffect(() => {
    load()
    const t = setInterval(load, 10000)
    return () => clearInterval(t)
  }, [load])

  async function act(id: string, label: string, fn: () => Promise<unknown>) {
    setBusy(id)
    try {
      await fn()
      toast.success(`${label} ok`)
      await load()
    } catch (e) {
      toast.error(errMsg(e))
    } finally {
      setBusy('')
    }
  }

  async function resetPassword(a: AdminAccount) {
    if (!window.confirm(`Reset password for ${a.email}?`)) return
    setBusy(a.id)
    try {
      const r = await api.admin.resetPassword(a.id)
      setPwResult({ email: a.email, password: r.temporary_password })
    } catch (e) {
      toast.error(errMsg(e))
    } finally {
      setBusy('')
    }
  }

  return (
    <>
      <Card title={`Accounts (${accounts?.length ?? 0})`}>
        {!accounts ? (
          <Loading />
        ) : accounts.length === 0 ? (
          <div className="p-8 text-center text-xs text-zinc-500">No accounts.</div>
        ) : (
          <table className="w-full text-sm">
            <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
              <tr className="border-b border-zinc-900">
                <th className={TH}>Email</th>
                <th className={TH}>Credits</th>
                <th className={TH}>Sandboxes</th>
                <th className={TH}>Last login</th>
                <th className={TH}>Role</th>
                <th className={TH}>Actions</th>
              </tr>
            </thead>
            <tbody>
              {accounts.map((a) => {
                const isBusy = busy === a.id
                return (
                  <tr key={a.id} className={TR}>
                    <td className={`${TD} font-medium`}>
                      {a.email}
                      {a.suspended && (
                        <span className="ml-2 text-[10px] font-mono uppercase rounded px-1.5 py-0.5 bg-red-500/10 text-red-300 ring-1 ring-inset ring-red-500/30">
                          suspended
                        </span>
                      )}
                    </td>
                    <td className={`${TD} font-mono tabular-nums ${a.credits < 0 ? 'text-red-400' : 'text-zinc-300'}`}>
                      ${a.credits.toFixed(2)}
                    </td>
                    <td className={`${TD} font-mono text-xs text-zinc-400`}>{a.total_sandboxes}</td>
                    <td className={`${TD} text-xs text-zinc-400`}>
                      {a.last_login ? formatRelative(a.last_login) : 'never'}
                    </td>
                    <td className={TD}>
                      {a.is_admin ? (
                        <span className="text-[10px] font-mono uppercase rounded px-1.5 py-0.5 bg-teal-500/10 text-teal-300 ring-1 ring-inset ring-teal-500/30">
                          admin
                        </span>
                      ) : (
                        <span className="text-[11px] text-zinc-500">user</span>
                      )}
                    </td>
                    <td className={TD}>
                      <div className="flex flex-wrap gap-1.5">
                        <ActionBtn disabled={isBusy} onClick={() => setCreditTarget(a)}>
                          + Credits
                        </ActionBtn>
                        <ActionBtn
                          disabled={isBusy}
                          tone={a.suspended ? 'default' : 'danger'}
                          onClick={() =>
                            act(a.id, a.suspended ? 'unsuspend' : 'suspend', () =>
                              api.admin.suspendAccount(a.id, !a.suspended),
                            )
                          }
                        >
                          {a.suspended ? 'Unsuspend' : 'Suspend'}
                        </ActionBtn>
                        <ActionBtn disabled={isBusy} onClick={() => resetPassword(a)}>
                          Reset password
                        </ActionBtn>
                        <ActionBtn
                          disabled={isBusy}
                          onClick={() =>
                            act(a.id, a.is_admin ? 'demote' : 'promote', () =>
                              api.admin.promoteAccount(a.id, !a.is_admin),
                            )
                          }
                        >
                          {a.is_admin ? 'Revoke admin' : 'Make admin'}
                        </ActionBtn>
                      </div>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </Card>

      {creditTarget && (
        <AddCreditsModal
          account={creditTarget}
          onClose={() => setCreditTarget(null)}
          onDone={() => {
            setCreditTarget(null)
            load()
          }}
        />
      )}

      <Modal open={!!pwResult} onClose={() => setPwResult(null)} title="Password reset" size="sm">
        {pwResult && (
          <div className="space-y-3">
            <p className="text-xs text-zinc-400">
              Temporary password for <span className="text-zinc-200">{pwResult.email}</span>.
              Share it securely — it is shown only once.
            </p>
            <div className="rounded-md bg-zinc-950 border border-zinc-800 px-3 py-2.5 font-mono text-sm text-teal-300 select-all">
              {pwResult.password}
            </div>
          </div>
        )}
      </Modal>
    </>
  )
}

function AddCreditsModal({
  account,
  onClose,
  onDone,
}: {
  account: AdminAccount
  onClose: () => void
  onDone: () => void
}) {
  const toast = useToast()
  const [custom, setCustom] = useState('')
  const [busy, setBusy] = useState(false)

  async function apply(amount: number) {
    if (!amount || Number.isNaN(amount)) {
      toast.error('Enter a non-zero amount')
      return
    }
    setBusy(true)
    try {
      const r = await api.admin.addCredits(account.id, amount)
      toast.success(`Balance: $${r.credits.toFixed(2)}`)
      onDone()
    } catch (e) {
      toast.error(errMsg(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open onClose={onClose} title={`Add credits — ${account.email}`} size="sm">
      <div className="space-y-4">
        <p className="text-xs text-zinc-400">
          Current balance: <span className="font-mono text-zinc-200">${account.credits.toFixed(2)}</span>
        </p>
        <div className="flex gap-2">
          {[10, 50, 100].map((amt) => (
            <button
              key={amt}
              disabled={busy}
              onClick={() => apply(amt)}
              className="flex-1 rounded-md border border-zinc-800 bg-zinc-900/60 py-2 text-sm font-medium hover:border-teal-500/50 hover:text-teal-300 disabled:opacity-40"
            >
              +${amt}
            </button>
          ))}
        </div>
        <div className="flex gap-2">
          <input
            type="number"
            value={custom}
            onChange={(e) => setCustom(e.target.value)}
            placeholder="Custom amount"
            className="flex-1 rounded-md bg-zinc-950 border border-zinc-800 px-3 py-2 text-sm focus:border-teal-500 focus:outline-none"
          />
          <button
            disabled={busy}
            onClick={() => apply(parseFloat(custom))}
            className="rounded-md bg-gradient-to-r from-teal-500 to-teal-600 hover:from-teal-400 hover:to-teal-500 text-zinc-950 font-medium px-4 text-sm disabled:opacity-40"
          >
            Apply
          </button>
        </div>
      </div>
    </Modal>
  )
}

// ---- Logs tab ----

const LOG_LEVELS = ['', 'DEBUG', 'INFO', 'WARN', 'ERROR']

function levelColor(level: string): string {
  switch (level.toUpperCase()) {
    case 'ERROR':
      return 'text-red-400'
    case 'WARN':
    case 'WARNING':
      return 'text-amber-400'
    case 'DEBUG':
      return 'text-zinc-600'
    default:
      return 'text-sky-300'
  }
}

function LogsTab({ initialSandbox }: { initialSandbox: string }) {
  const toast = useToast()
  const [source, setSource] = useState<'master' | 'agent'>('master')
  const [level, setLevel] = useState('')
  const [sandboxId, setSandboxId] = useState(initialSandbox)
  const [tail, setTail] = useState(200)
  const [entries, setEntries] = useState<AdminLogEntry[]>([])
  const [paused, setPaused] = useState(false)

  useEffect(() => {
    let alive = true
    async function load() {
      try {
        const r = await api.admin.logs({
          source,
          level: level || undefined,
          sandbox_id: sandboxId || undefined,
          tail,
        })
        if (alive) setEntries(r.entries)
      } catch (e) {
        if (alive) toast.error(errMsg(e))
      }
    }
    load()
    if (paused) return
    const t = setInterval(load, 2000)
    return () => {
      alive = false
      clearInterval(t)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [source, level, sandboxId, tail, paused])

  const inputCls =
    'rounded-md bg-zinc-950 border border-zinc-800 px-2.5 py-1.5 text-xs focus:border-teal-500 focus:outline-none'

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex rounded-md border border-zinc-800 overflow-hidden">
          {(['master', 'agent'] as const).map((s) => (
            <button
              key={s}
              onClick={() => setSource(s)}
              className={`px-3 py-1.5 text-xs font-medium ${
                source === s ? 'bg-zinc-800 text-zinc-50' : 'text-zinc-500 hover:text-zinc-200'
              }`}
            >
              {s}
            </button>
          ))}
        </div>
        <select value={level} onChange={(e) => setLevel(e.target.value)} className={inputCls}>
          {LOG_LEVELS.map((l) => (
            <option key={l} value={l}>
              {l || 'All levels'}
            </option>
          ))}
        </select>
        <input
          value={sandboxId}
          onChange={(e) => setSandboxId(e.target.value)}
          placeholder="Filter by sandbox id…"
          className={`${inputCls} w-56`}
        />
        <select
          value={tail}
          onChange={(e) => setTail(parseInt(e.target.value, 10))}
          className={inputCls}
        >
          {[100, 200, 500, 1000].map((n) => (
            <option key={n} value={n}>
              tail {n}
            </option>
          ))}
        </select>
        <button
          onClick={() => setPaused((p) => !p)}
          className={`${inputCls} ${paused ? 'text-amber-300' : 'text-emerald-300'}`}
        >
          {paused ? 'Paused' : 'Live · 2s'}
        </button>
      </div>

      <div className="rounded-lg border border-zinc-900 bg-zinc-950 font-mono text-xs h-[60vh] overflow-auto">
        {entries.length === 0 ? (
          <div className="p-8 text-center text-zinc-600">No log entries.</div>
        ) : (
          entries.map((e, i) => (
            <div
              key={i}
              className="flex gap-2 px-3 py-1 border-b border-zinc-900/40 hover:bg-zinc-900/40"
            >
              <span className="text-zinc-600 shrink-0">
                {new Date(e.time).toLocaleTimeString()}
              </span>
              <span className={`shrink-0 w-12 ${levelColor(e.level)}`}>{e.level}</span>
              <span className="text-zinc-300 break-all">
                {e.msg}
                {e.attrs &&
                  Object.entries(e.attrs).map(([k, v]) => (
                    <span key={k} className="text-zinc-600">
                      {' '}
                      {k}=<span className="text-zinc-500">{v}</span>
                    </span>
                  ))}
              </span>
            </div>
          ))
        )}
      </div>
    </div>
  )
}
