import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { DollarSign, Plus } from 'lucide-react'
import api from '../api/client'
import type {
  BillingConfigResponse,
  DailySpendPoint,
  TransactionResponse,
  UsageSummary,
} from '../api/types'
import PageHeader from '../components/PageHeader'
import Modal from '../components/Modal'
import { useToast } from '../components/Toast'

const PRESET_AMOUNTS = [25, 50, 100, 250]
const MIN_AMOUNT = 10
const MAX_AMOUNT = 1000

const EMPTY_SUMMARY: UsageSummary = {
  credits_remaining: 0,
  total_spend_30d: 0,
  vcpu_hours_30d: 0,
  memory_gb_hours_30d: 0,
  current_hourly_burn: 0,
  daily_spend: [],
  per_sandbox: [],
}

// num coerces a possibly-bad server value to a finite number.
function num(v: unknown): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : 0
}

// normalizeSummary defends the UI against a partial or stale payload.
function normalizeSummary(s: UsageSummary | null): UsageSummary {
  if (!s || typeof s !== 'object') return EMPTY_SUMMARY
  return {
    credits_remaining: num(s.credits_remaining),
    total_spend_30d: num(s.total_spend_30d),
    vcpu_hours_30d: num(s.vcpu_hours_30d),
    memory_gb_hours_30d: num(s.memory_gb_hours_30d),
    current_hourly_burn: num(s.current_hourly_burn),
    daily_spend: Array.isArray(s.daily_spend) ? s.daily_spend : [],
    per_sandbox: Array.isArray(s.per_sandbox) ? s.per_sandbox : [],
  }
}

// zeroFill expands the server's sparse daily_spend (days with usage only)
// into a contiguous 30-day series so the chart always shows 30 bars.
function zeroFill(points: DailySpendPoint[]): DailySpendPoint[] {
  const byDate = new Map(points.map((p) => [p.date, num(p.amount)]))
  const today = new Date()
  today.setHours(0, 0, 0, 0)
  const out: DailySpendPoint[] = []
  for (let i = 29; i >= 0; i--) {
    const d = new Date(today)
    d.setDate(d.getDate() - i)
    const key = d.toISOString().slice(0, 10)
    out.push({ date: key, amount: byDate.get(key) ?? 0 })
  }
  return out
}

export default function UsagePage() {
  const toast = useToast()
  const [summary, setSummary] = useState<UsageSummary>(EMPTY_SUMMARY)
  const [config, setConfig] = useState<BillingConfigResponse | null>(null)
  const [transactions, setTransactions] = useState<TransactionResponse[]>([])
  const [tab, setTab] = useState<'sandboxes' | 'transactions'>('sandboxes')
  const [modalOpen, setModalOpen] = useState(false)

  const refreshSummary = useCallback(() => {
    api.usage
      .summary()
      .then((s) => setSummary(normalizeSummary(s)))
      .catch(() => {})
  }, [])

  const refreshTransactions = useCallback(() => {
    api.billing
      .transactions()
      .then((r) => setTransactions(Array.isArray(r.transactions) ? r.transactions : []))
      .catch(() => {})
  }, [])

  // Initial load, plus a 10s poll so the credits tile reflects the meter.
  useEffect(() => {
    refreshSummary()
    refreshTransactions()
    api.billing.config().then(setConfig).catch(() => {})
    const id = window.setInterval(refreshSummary, 10_000)
    return () => window.clearInterval(id)
  }, [refreshSummary, refreshTransactions])

  // Handle the post-checkout redirect, then strip the query params so a
  // refresh doesn't re-fire the toast.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search)
    if (params.get('paid') === '1') {
      toast.success('Payment successful! Credits added.')
      // The webhook usually lands within a couple of seconds — pull fresh
      // numbers shortly after so the credits tile catches up.
      window.setTimeout(refreshSummary, 3000)
      window.setTimeout(refreshTransactions, 3000)
    } else if (params.get('cancelled') === '1') {
      toast.info('Payment cancelled')
    } else {
      return
    }
    window.history.replaceState(null, '', window.location.pathname)
  }, [toast, refreshSummary, refreshTransactions])

  const daily = useMemo(() => zeroFill(summary.daily_spend), [summary.daily_spend])
  const maxDaily = Math.max(0.0001, ...daily.map((d) => d.amount))

  return (
    <>
      <PageHeader
        title="Usage & Billing"
        description="Resource consumption, spend, and prepaid account credits."
      />
      <div className="p-6 space-y-6">
        <div className="flex items-center justify-end">
          {config?.stripe_enabled && (
            <button
              onClick={() => setModalOpen(true)}
              className="flex items-center gap-1.5 rounded-md bg-teal-600 hover:bg-teal-500 px-3 py-1.5 text-sm font-medium text-white transition-colors"
            >
              <Plus size={14} /> Add Funds
            </button>
          )}
        </div>

        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-5 gap-4">
          <BillingTile
            label="Credits remaining"
            value={`$${summary.credits_remaining.toFixed(2)}`}
            subtle="prepaid balance"
          />
          <BillingTile
            label="Total spend (30d)"
            value={`$${summary.total_spend_30d.toFixed(2)}`}
          />
          <BillingTile
            label="vCPU-hours (30d)"
            value={summary.vcpu_hours_30d.toFixed(2)}
            subtle="@ $0.06/hr"
          />
          <BillingTile
            label="Memory GB-hours (30d)"
            value={summary.memory_gb_hours_30d.toFixed(2)}
            subtle="@ $0.01/GB/hr"
          />
          <BillingTile
            label="Current burn"
            value={`$${summary.current_hourly_burn.toFixed(4)}/hr`}
            subtle="running sandboxes"
          />
        </div>

        <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 p-4">
          <div className="flex items-center justify-between mb-3">
            <h2 className="text-sm font-medium">Daily spend (last 30 days)</h2>
            <span className="text-[11px] text-zinc-500 font-mono">
              max ${maxDaily.toFixed(4)}
            </span>
          </div>
          <div className="flex items-end gap-px h-32">
            {daily.map((d) => (
              <div
                key={d.date}
                className="flex-1 flex flex-col justify-end group"
                title={`${d.date}: $${d.amount.toFixed(4)}`}
              >
                <div
                  className="bg-teal-500/40 group-hover:bg-teal-400 transition-colors rounded-t-sm"
                  style={{ height: `${Math.max(2, (d.amount / maxDaily) * 100)}%` }}
                />
              </div>
            ))}
          </div>
          <div className="flex justify-between text-[10px] text-zinc-600 font-mono mt-1">
            <span>{daily[0]?.date.slice(5)}</span>
            <span>{daily[daily.length - 1]?.date.slice(5)}</span>
          </div>
        </div>

        <div className="rounded-lg border border-zinc-900 bg-zinc-900/30 overflow-hidden">
          <div className="flex items-center gap-1 px-4 pt-3 border-b border-zinc-900">
            <TabButton active={tab === 'sandboxes'} onClick={() => setTab('sandboxes')}>
              Per-sandbox
            </TabButton>
            <TabButton active={tab === 'transactions'} onClick={() => setTab('transactions')}>
              Transactions
            </TabButton>
            <div className="ml-auto pb-2">
              <DollarSign size={14} className="text-zinc-500" />
            </div>
          </div>
          {tab === 'sandboxes' ? (
            <PerSandboxTable
              summary={summary}
              stripeEnabled={!!config?.stripe_enabled}
              onAddFunds={() => setModalOpen(true)}
            />
          ) : (
            <TransactionsTable transactions={transactions} />
          )}
        </div>
      </div>

      <AddFundsModal open={modalOpen} onClose={() => setModalOpen(false)} />
    </>
  )
}

function PerSandboxTable({
  summary,
  stripeEnabled,
  onAddFunds,
}: {
  summary: UsageSummary
  stripeEnabled: boolean
  onAddFunds: () => void
}) {
  if (summary.per_sandbox.length === 0) {
    return (
      <div className="p-10 text-center">
        <p className="text-sm font-medium text-zinc-300">No usage yet</p>
        <p className="text-xs text-zinc-500 mt-1 max-w-sm mx-auto">
          Spin up a sandbox to start metering vCPU and memory. Usage is billed
          against your prepaid account balance.
        </p>
        {stripeEnabled && (
          <button
            onClick={onAddFunds}
            className="mt-4 inline-flex items-center gap-1.5 rounded-md bg-teal-500 hover:bg-teal-400 text-zinc-950 shadow-md shadow-teal-500/20 hover:shadow-teal-500/40 transition-all duration-200 hover:scale-[1.02] px-3 py-1.5 text-sm font-medium"
          >
            <Plus size={14} /> Add funds to start using paid features
          </button>
        )}
      </div>
    )
  }
  return (
    <table className="w-full text-sm">
      <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
        <tr className="border-b border-zinc-900">
          <th className="text-left font-medium px-4 py-2">Sandbox</th>
          <th className="text-right font-medium px-4 py-2">vCPU-hours</th>
          <th className="text-right font-medium px-4 py-2">Cost</th>
        </tr>
      </thead>
      <tbody>
        {summary.per_sandbox.map((r, i) => (
          <tr
            key={`${r.name}-${i}`}
            className="border-b border-zinc-900/50 hover:bg-zinc-800/50 transition-colors"
          >
            <td className="px-4 py-2 font-medium">{r.name}</td>
            <td className="px-4 py-2 text-right text-zinc-300 font-mono text-xs tabular-nums">
              {num(r.vcpu_hours).toFixed(4)}
            </td>
            <td className="px-4 py-2 text-right text-teal-300 font-mono text-xs tabular-nums">
              ${num(r.cost).toFixed(4)}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function TransactionsTable({ transactions }: { transactions: TransactionResponse[] }) {
  if (transactions.length === 0) {
    return (
      <div className="p-8 text-center text-xs text-zinc-500">
        No transactions yet. Use “Add Funds” to top up your balance.
      </div>
    )
  }
  return (
    <table className="w-full text-sm">
      <thead className="text-[11px] text-zinc-500 uppercase tracking-wider">
        <tr className="border-b border-zinc-900">
          <th className="text-left font-medium px-4 py-2">Date</th>
          <th className="text-right font-medium px-4 py-2">Amount</th>
          <th className="text-right font-medium px-4 py-2">Status</th>
        </tr>
      </thead>
      <tbody>
        {transactions.map((t) => (
          <tr
            key={t.id}
            className="border-b border-zinc-900/50 hover:bg-zinc-800/50 transition-colors"
          >
            <td className="px-4 py-2 text-zinc-300 text-xs">
              {new Date(t.created_at).toLocaleString()}
            </td>
            <td className="px-4 py-2 text-right font-mono text-xs tabular-nums">
              ${num(t.amount_usd).toFixed(2)}
            </td>
            <td className="px-4 py-2 text-right">
              <StatusBadge status={t.status} />
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function StatusBadge({ status }: { status: string }) {
  const styles: Record<string, string> = {
    completed: 'bg-emerald-500/15 text-emerald-400 border-emerald-500/30',
    pending: 'bg-amber-500/15 text-amber-400 border-amber-500/30',
    failed: 'bg-red-500/15 text-red-400 border-red-500/30',
  }
  const cls = styles[status] ?? 'bg-zinc-700/30 text-zinc-400 border-zinc-700'
  return (
    <span
      className={`inline-block rounded border px-2 py-0.5 text-[10px] font-medium uppercase tracking-wider ${cls}`}
    >
      {status}
    </span>
  )
}

function AddFundsModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const toast = useToast()
  const [amount, setAmount] = useState<number>(50)
  const [submitting, setSubmitting] = useState(false)

  const valid = amount >= MIN_AMOUNT && amount <= MAX_AMOUNT

  const submit = async () => {
    if (!valid || submitting) return
    setSubmitting(true)
    try {
      const { url } = await api.billing.checkout(amount)
      window.location.href = url
    } catch {
      toast.error('Could not start checkout. Please try again.')
      setSubmitting(false)
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="Add funds to your account" size="sm">
      <div className="space-y-4">
        <p className="text-xs text-zinc-400">
          Pick an amount to add to your prepaid balance. You’ll be redirected
          to Stripe to complete the payment.
        </p>
        <div className="grid grid-cols-4 gap-2">
          {PRESET_AMOUNTS.map((a) => (
            <button
              key={a}
              onClick={() => setAmount(a)}
              className={`rounded-md border px-2 py-2 text-sm font-medium transition-colors ${
                amount === a
                  ? 'border-teal-500 bg-teal-500/15 text-teal-300'
                  : 'border-zinc-800 bg-zinc-900/50 text-zinc-300 hover:border-zinc-700'
              }`}
            >
              ${a}
            </button>
          ))}
        </div>
        <div>
          <label className="text-[11px] text-zinc-500 uppercase tracking-wider font-mono">
            Custom amount (USD)
          </label>
          <input
            type="number"
            min={MIN_AMOUNT}
            max={MAX_AMOUNT}
            value={amount}
            onChange={(e) => setAmount(Number(e.target.value))}
            className="mt-1 w-full rounded-md border border-zinc-800 bg-zinc-950 px-3 py-2 text-sm text-zinc-100 focus:border-teal-500 focus:outline-none"
          />
          {!valid && (
            <p className="mt-1 text-[11px] text-amber-400">
              Amount must be between ${MIN_AMOUNT} and ${MAX_AMOUNT}.
            </p>
          )}
        </div>
        <button
          onClick={submit}
          disabled={!valid || submitting}
          className="w-full rounded-md bg-teal-600 hover:bg-teal-500 disabled:opacity-40 disabled:cursor-not-allowed px-3 py-2 text-sm font-medium text-white transition-colors"
        >
          {submitting ? 'Redirecting…' : 'Continue to checkout'}
        </button>
      </div>
    </Modal>
  )
}

function TabButton({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: ReactNode
}) {
  return (
    <button
      onClick={onClick}
      className={`px-3 pb-2 text-sm font-medium border-b-2 transition-colors ${
        active
          ? 'border-teal-500 text-zinc-100'
          : 'border-transparent text-zinc-500 hover:text-zinc-300'
      }`}
    >
      {children}
    </button>
  )
}

function BillingTile({
  label,
  value,
  subtle,
}: {
  label: string
  value: string
  subtle?: string
}) {
  return (
    <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 shadow-lg shadow-black/20 p-4">
      <div className="text-[11px] text-zinc-500 uppercase tracking-wider font-mono">
        {label}
      </div>
      <div className="mt-2 text-2xl font-semibold tabular-nums">{value}</div>
      {subtle && <div className="mt-1 text-[11px] text-zinc-500">{subtle}</div>}
    </div>
  )
}
