import type { NodeState, SandboxState, OperationStatus, ClusterState } from '../api/types'

type AnyState = SandboxState | NodeState | OperationStatus | ClusterState | string

interface BadgeStyle {
  cls: string
  pulse?: boolean
  dot?: string
}

const styles: Record<string, BadgeStyle> = {
  // sandbox
  RUNNING: { cls: 'bg-emerald-500/10 text-emerald-300 ring-emerald-500/30', dot: 'bg-emerald-400' },
  CREATING: { cls: 'bg-amber-500/10 text-amber-300 ring-amber-500/30', pulse: true, dot: 'bg-amber-400' },
  PENDING: { cls: 'bg-zinc-500/15 text-zinc-300 ring-zinc-500/30', dot: 'bg-zinc-400' },
  PAUSING: { cls: 'bg-amber-500/10 text-amber-300 ring-amber-500/30', pulse: true, dot: 'bg-amber-400' },
  PAUSED: { cls: 'bg-sky-500/10 text-sky-300 ring-sky-500/30', dot: 'bg-sky-400' },
  STOPPING: { cls: 'bg-amber-500/10 text-amber-300 ring-amber-500/30', pulse: true, dot: 'bg-amber-400' },
  STOPPED: { cls: 'bg-zinc-600/20 text-zinc-300 ring-zinc-500/30', dot: 'bg-zinc-400' },
  ARCHIVING: { cls: 'bg-amber-500/10 text-amber-300 ring-amber-500/30', pulse: true, dot: 'bg-amber-400' },
  ARCHIVED: { cls: 'bg-zinc-600/20 text-zinc-300 ring-zinc-500/30', dot: 'bg-zinc-400' },
  DESTROYING: { cls: 'bg-amber-500/10 text-amber-300 ring-amber-500/30', pulse: true, dot: 'bg-amber-400' },
  DESTROYED: { cls: 'bg-zinc-700/30 text-zinc-400 ring-zinc-600/40', dot: 'bg-zinc-500' },
  ERROR: { cls: 'bg-red-500/10 text-red-300 ring-red-500/40', dot: 'bg-red-400' },

  // node
  REGISTERING: { cls: 'bg-amber-500/10 text-amber-300 ring-amber-500/30', pulse: true, dot: 'bg-amber-400' },
  ACTIVE: { cls: 'bg-emerald-500/10 text-emerald-300 ring-emerald-500/30', dot: 'bg-emerald-400' },
  DRAINING: { cls: 'bg-amber-500/10 text-amber-300 ring-amber-500/30', pulse: true, dot: 'bg-amber-400' },
  CORDONED: { cls: 'bg-red-500/10 text-red-300 ring-red-500/40', dot: 'bg-red-400' },
  QUARANTINED: { cls: 'bg-red-500/10 text-red-300 ring-red-500/40', dot: 'bg-red-400' },
  OFFLINE: { cls: 'bg-zinc-700/30 text-zinc-400 ring-zinc-600/40', dot: 'bg-zinc-500' },
  DECOMMISSIONED: { cls: 'bg-zinc-700/30 text-zinc-400 ring-zinc-600/40', dot: 'bg-zinc-500' },

  // operations
  IN_PROGRESS: { cls: 'bg-amber-500/10 text-amber-300 ring-amber-500/30', pulse: true, dot: 'bg-amber-400' },
  COMPLETED: { cls: 'bg-emerald-500/10 text-emerald-300 ring-emerald-500/30', dot: 'bg-emerald-400' },
  FAILED: { cls: 'bg-red-500/10 text-red-300 ring-red-500/40', dot: 'bg-red-400' },
}

const fallback: BadgeStyle = { cls: 'bg-zinc-600/20 text-zinc-300 ring-zinc-500/30', dot: 'bg-zinc-400' }

export default function StateBadge({ state }: { state: AnyState }) {
  const style = styles[state] ?? fallback
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-[11px] font-medium font-mono uppercase tracking-wider ring-1 ring-inset ${style.cls} ${style.pulse ? 'animate-pulse-soft' : ''}`}
    >
      {style.dot && (
        <span
          className={`size-1.5 rounded-full ${style.dot} ${style.pulse ? 'animate-pulse-soft' : ''}`}
        />
      )}
      {state}
    </span>
  )
}
