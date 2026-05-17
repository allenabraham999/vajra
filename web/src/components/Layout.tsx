import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
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
import Bolt from './Bolt'

interface NavItem {
  to: string
  label: string
  icon: typeof Boxes
  admin?: boolean
  external?: boolean
}

const items: NavItem[] = [
  { to: '/dashboard', label: 'Overview', icon: LayoutDashboard },
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
  const location = useLocation()

  const navItems = items.filter((item) => !item.admin || isAdmin)

  return (
    <div className="flex h-full min-h-screen bg-zinc-950 text-zinc-100">
      <aside className="w-56 shrink-0 border-r border-zinc-900 bg-zinc-950/80 flex flex-col">
        <div className="px-4 py-5 border-b border-zinc-900">
          <div className="flex items-center gap-2.5">
            <Bolt size={22} />
            <div className="flex flex-col leading-none">
              <span className="text-[15px] font-semibold font-mono tracking-[0.18em] text-zinc-100">
                VAJRA
              </span>
              <span className="text-[9px] text-zinc-500 font-mono uppercase tracking-[0.25em] mt-1">
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
                className="flex items-center gap-2.5 px-2.5 py-2 rounded-md text-sm text-zinc-400 hover:text-zinc-100 hover:bg-zinc-900/80 transition-all duration-200"
              >
                <Icon size={15} />
                <span>{label}</span>
              </a>
            ) : (
              <NavLink
                key={to}
                to={to}
                end={to === '/dashboard'}
                className={({ isActive }) =>
                  `flex items-center gap-2.5 px-2.5 py-2 rounded-md text-sm transition-all duration-200 ${
                    isActive
                      ? 'bg-zinc-800/80 text-zinc-50 shadow-[inset_2px_0_0_0_#14b8a6]'
                      : 'text-zinc-400 hover:text-zinc-100 hover:bg-zinc-900/80'
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
            className="flex w-full items-center gap-2 px-2.5 py-2 rounded-md text-sm text-zinc-400 hover:text-zinc-100 hover:bg-zinc-900/80 transition-all duration-200"
          >
            <LogOut size={15} />
            <span>Sign out</span>
          </button>
        </div>
      </aside>

      <main className="flex-1 flex flex-col min-w-0">
        <header className="flex items-center justify-end h-12 border-b border-zinc-900 px-4 gap-3 bg-zinc-950/60 backdrop-blur">
          <span className="text-xs text-zinc-500 font-mono">
            {email ?? 'anonymous'}
          </span>
          <span className="size-6 rounded-full bg-gradient-to-br from-zinc-800 to-zinc-900 ring-1 ring-zinc-800 grid place-items-center text-[11px] uppercase text-zinc-300">
            {email?.[0] ?? '?'}
          </span>
        </header>
        <div key={location.pathname} className="flex-1 overflow-auto animate-fade-in">
          <Outlet />
        </div>
      </main>
    </div>
  )
}
