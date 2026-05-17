import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useNavigate } from 'react-router-dom'
import { ChevronDown, LogOut, Settings, Webhook } from 'lucide-react'
import { useAuth } from '../auth/AuthContext'

// UserMenu is the top-right account dropdown. It surfaces the signed-in
// email plus the settings-flavoured links (Webhooks, Settings) that no
// longer sit in the sidebar, and the sign-out action.
export default function UserMenu() {
  const { email, logout } = useAuth()
  const nav = useNavigate()
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

  // Close the menu on any outside click or on Escape.
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  const go = (to: string) => {
    setOpen(false)
    nav(to)
  }

  return (
    <div ref={ref} className="relative">
      <button
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-2 rounded-md px-1.5 py-1 hover:bg-zinc-900/80 transition-colors"
      >
        <span className="size-6 rounded-full bg-gradient-to-br from-teal-600 to-teal-800 ring-1 ring-teal-700/50 grid place-items-center text-[11px] uppercase text-teal-50 font-medium">
          {email?.[0] ?? '?'}
        </span>
        <span className="text-xs text-zinc-400 font-mono hidden sm:block max-w-[180px] truncate">
          {email ?? 'anonymous'}
        </span>
        <ChevronDown
          size={13}
          className={`text-zinc-500 transition-transform ${open ? 'rotate-180' : ''}`}
        />
      </button>

      {open && (
        <div className="absolute right-0 mt-1.5 w-60 rounded-lg border border-zinc-800 bg-zinc-900/95 shadow-[0_20px_60px_-15px_rgba(0,0,0,0.8)] backdrop-blur py-1 z-50 animate-fade-in">
          <div className="px-3 py-2 border-b border-zinc-800">
            <div className="text-[10px] text-zinc-500 uppercase tracking-wider font-mono">
              Signed in as
            </div>
            <div className="text-xs text-zinc-200 font-mono truncate">
              {email ?? 'anonymous'}
            </div>
          </div>
          <MenuItem
            icon={<Webhook size={14} />}
            label="Webhooks"
            onClick={() => go('/webhooks')}
          />
          <MenuItem
            icon={<Settings size={14} />}
            label="Settings"
            onClick={() => go('/settings')}
          />
          <div className="my-1 border-t border-zinc-800" />
          <MenuItem
            icon={<LogOut size={14} />}
            label="Sign out"
            danger
            onClick={() => {
              setOpen(false)
              logout()
              nav('/login', { replace: true })
            }}
          />
        </div>
      )}
    </div>
  )
}

function MenuItem({
  icon,
  label,
  onClick,
  danger,
}: {
  icon: ReactNode
  label: string
  onClick: () => void
  danger?: boolean
}) {
  return (
    <button
      onClick={onClick}
      className={`flex w-full items-center gap-2.5 px-3 py-2 text-sm transition-colors ${
        danger
          ? 'text-zinc-400 hover:text-red-300 hover:bg-red-950/40'
          : 'text-zinc-300 hover:text-zinc-100 hover:bg-zinc-800/70'
      }`}
    >
      {icon}
      {label}
    </button>
  )
}
