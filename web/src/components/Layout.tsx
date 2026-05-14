import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import {
  Boxes,
  LayoutDashboard,
  KeyRound,
  Server,
  PackageSearch,
  Receipt,
  ShieldCheck,
  Activity,
  LogOut,
  Webhook,
  BookOpen,
} from 'lucide-react'
import { useAuth } from '../auth/AuthContext'

interface NavItem {
  to: string
  label: string
  icon: typeof Boxes
  admin?: boolean
  external?: boolean
}

const items: NavItem[] = [
  { to: '/', label: 'Overview', icon: LayoutDashboard },
  { to: '/sandboxes', label: 'Sandboxes', icon: Boxes },
  { to: '/templates', label: 'Templates', icon: PackageSearch },
  { to: '/webhooks', label: 'Webhooks', icon: Webhook },
  { to: '/nodes', label: 'Nodes', icon: Server, admin: true },
  { to: '/api-keys', label: 'API Keys', icon: KeyRound },
  { to: '/usage', label: 'Usage', icon: Receipt },
  { to: '/metrics', label: 'Metrics', icon: Activity },
  { to: '/v1/docs', label: 'API Docs', icon: BookOpen, external: true },
  { to: '/admin', label: 'Admin', icon: ShieldCheck, admin: true },
]

export default function Layout() {
  const { email, isAdmin, logout } = useAuth()
  const nav = useNavigate()

  const navItems = items.filter((item) => !item.admin || isAdmin)

  return (
    <div className="flex h-full min-h-screen bg-zinc-950 text-zinc-100">
      <aside className="w-56 shrink-0 border-r border-zinc-900 bg-zinc-950/80 flex flex-col">
        <div className="px-4 py-4 border-b border-zinc-900">
          <div className="flex items-center gap-2">
            <div className="size-7 rounded bg-gradient-to-br from-emerald-500 to-emerald-700 grid place-items-center font-bold text-zinc-950">
              V
            </div>
            <div className="flex flex-col">
              <span className="text-sm font-semibold tracking-tight">Vajra</span>
              <span className="text-[10px] text-zinc-500 font-mono uppercase tracking-wider">
                sandbox cloud
              </span>
            </div>
          </div>
        </div>
        <nav className="flex-1 px-2 py-3 space-y-0.5">
          {navItems.map(({ to, label, icon: Icon, external }) =>
            external ? (
              <a
                key={to}
                href={to}
                target="_blank"
                rel="noreferrer"
                className="flex items-center gap-2.5 px-2.5 py-2 rounded-md text-sm text-zinc-400 hover:text-zinc-100 hover:bg-zinc-900 transition-colors"
              >
                <Icon size={15} />
                <span>{label}</span>
              </a>
            ) : (
              <NavLink
                key={to}
                to={to}
                end={to === '/'}
                className={({ isActive }) =>
                  `flex items-center gap-2.5 px-2.5 py-2 rounded-md text-sm transition-colors ${
                    isActive
                      ? 'bg-zinc-800 text-zinc-50'
                      : 'text-zinc-400 hover:text-zinc-100 hover:bg-zinc-900'
                  }`
                }
              >
                <Icon size={15} />
                <span>{label}</span>
              </NavLink>
            ),
          )}
        </nav>
        <div className="px-2 py-3 border-t border-zinc-900">
          <button
            onClick={() => {
              logout()
              nav('/login', { replace: true })
            }}
            className="flex w-full items-center gap-2 px-2.5 py-2 rounded-md text-sm text-zinc-400 hover:text-zinc-100 hover:bg-zinc-900 transition-colors"
          >
            <LogOut size={15} />
            <span>Sign out</span>
          </button>
        </div>
      </aside>

      <main className="flex-1 flex flex-col min-w-0">
        <header className="flex items-center justify-end h-12 border-b border-zinc-900 px-4 gap-3">
          <span className="text-xs text-zinc-500 font-mono">
            {email ?? 'anonymous'}
          </span>
          <span className="size-6 rounded-full bg-zinc-800 grid place-items-center text-[11px] uppercase">
            {email?.[0] ?? '?'}
          </span>
        </header>
        <div className="flex-1 overflow-auto">
          <Outlet />
        </div>
      </main>
    </div>
  )
}
