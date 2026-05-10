import type { NodeState, SandboxState, OperationStatus, ClusterState } from '../api/types'

type AnyState = SandboxState | NodeState | OperationStatus | ClusterState | string

const colors: Record<string, string> = {
  // sandbox
  RUNNING: 'bg-emerald-500/15 text-emerald-300 ring-emerald-500/30',
  CREATING: 'bg-amber-500/15 text-amber-300 ring-amber-500/30',
  PENDING: 'bg-zinc-500/15 text-zinc-300 ring-zinc-500/30',
  PAUSING: 'bg-amber-500/15 text-amber-300 ring-amber-500/30',
  PAUSED: 'bg-sky-500/15 text-sky-300 ring-sky-500/30',
  STOPPING: 'bg-amber-500/15 text-amber-300 ring-amber-500/30',
  STOPPED: 'bg-zinc-600/20 text-zinc-300 ring-zinc-500/30',
  ARCHIVING: 'bg-amber-500/15 text-amber-300 ring-amber-500/30',
  ARCHIVED: 'bg-zinc-600/20 text-zinc-300 ring-zinc-500/30',
  DESTROYING: 'bg-amber-500/15 text-amber-300 ring-amber-500/30',
  DESTROYED: 'bg-zinc-700/30 text-zinc-400 ring-zinc-600/40',
  ERROR: 'bg-red-500/15 text-red-300 ring-red-500/30',

  // node
  REGISTERING: 'bg-amber-500/15 text-amber-300 ring-amber-500/30',
  ACTIVE: 'bg-emerald-500/15 text-emerald-300 ring-emerald-500/30',
  DRAINING: 'bg-amber-500/15 text-amber-300 ring-amber-500/30',
  QUARANTINED: 'bg-red-500/15 text-red-300 ring-red-500/30',
  OFFLINE: 'bg-zinc-700/30 text-zinc-400 ring-zinc-600/40',
  DECOMMISSIONED: 'bg-zinc-700/30 text-zinc-400 ring-zinc-600/40',

  // operations
  IN_PROGRESS: 'bg-amber-500/15 text-amber-300 ring-amber-500/30',
  COMPLETED: 'bg-emerald-500/15 text-emerald-300 ring-emerald-500/30',
  FAILED: 'bg-red-500/15 text-red-300 ring-red-500/30',
}

export default function StateBadge({ state }: { state: AnyState }) {
  const cls = colors[state] ?? 'bg-zinc-600/20 text-zinc-300 ring-zinc-500/30'
  return (
    <span
      className={`inline-flex items-center rounded px-1.5 py-0.5 text-[11px] font-medium font-mono uppercase tracking-wide ring-1 ring-inset ${cls}`}
    >
      {state}
    </span>
  )
}
