interface Props {
  value: number
  max: number
  label?: string
  tone?: 'default' | 'warn' | 'danger'
}

export default function ProgressBar({ value, max, label, tone = 'default' }: Props) {
  const pct = max > 0 ? Math.min(100, Math.round((value / max) * 100)) : 0
  const computed = pct >= 90 ? 'danger' : pct >= 70 ? 'warn' : tone
  const color =
    computed === 'danger'
      ? 'bg-red-500'
      : computed === 'warn'
        ? 'bg-amber-500'
        : 'bg-emerald-500'
  return (
    <div className="w-full">
      {label !== undefined && (
        <div className="flex justify-between text-[11px] text-zinc-400 mb-1 font-mono">
          <span>{label}</span>
          <span>{pct}%</span>
        </div>
      )}
      <div className="h-1.5 w-full rounded bg-zinc-800 overflow-hidden">
        <div className={`h-full ${color} transition-all`} style={{ width: `${pct}%` }} />
      </div>
    </div>
  )
}
