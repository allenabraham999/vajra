import { NavLink, Outlet, useLocation } from 'react-router-dom'
import {
  Boxes,
  LayoutDashboard,
  KeyRound,
  Server,
  PackageSearch,
  Receipt,
  ShieldCheck,
  Activity,
  Camera,
  BookOpen,
} from 'lucide-react'
import { useAuth } from '../auth/AuthContext'
import Bolt from './Bolt'
import UserMenu from './UserMenu'

interface NavItem {
  to: string
  label: string
  icon: typeof Boxes
  admin?: boolean
  external?: boolean
}

// The sidebar is grouped into sections; a divider is drawn between them.
// Webhooks now lives in the top-right user menu, not here.
const sections: NavItem[][] = [
  [
    { to: '/dashboard', label: 'Overview', icon: LayoutDashboard },
    { to: '/sandboxes', label: 'Sandboxes', icon: Boxes },
    { to: '/snapshots', label: 'Snapshots', icon: Camera },
    { to: '/templates', label: 'Templates', icon: PackageSearch },
    { to: '/nodes', label: 'Nodes', icon: Server, admin: true },
  ],
  [
    { to: '/api-keys', label: 'API Keys', icon: KeyRound },
    { to: '/usage', label: 'Usage', icon: Receipt },
    { to: '/metrics', label: 'Metrics', icon: Activity },
  ],
  [
    { to: '/v1/docs', label: 'Docs', icon: BookOpen, external: true },
    { to: '/admin', label: 'Admin', icon: ShieldCheck, admin: true },
  ],
]

export default function Layout() {
  const { isAdmin } = useAuth()
  const location = useLocation()

  // Drop admin-only items the caller can't see, then drop any section
  // that emptied out so we never render a dangling divider.
  const visibleSections = sections
    .map((s) => s.filter((item) => !item.admin || isAdmin))
    .filter((s) => s.length > 0)

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
        <nav className="flex-1 px-2 py-3 overflow-y-auto">
          {visibleSections.map((section, i) => (
            <div key={i}>
              {i > 0 && <div className="my-2 border-t border-zinc-900" />}
              <div className="space-y-0.5">
                {section.map((item) => (
                  <NavRow key={item.to} item={item} />
                ))}
              </div>
            </div>
          ))}
        </nav>
      </aside>

      <main className="flex-1 flex flex-col min-w-0">
        {/* relative z-40 lifts this header's stacking context (created by
            backdrop-blur) above the page content, so the UserMenu dropdown
            overlays page content instead of being painted behind it. Stays
            below the z-50 Modal overlay. */}
        <header className="relative z-40 flex items-center justify-end h-12 border-b border-zinc-900 px-4 gap-3 bg-zinc-950/60 backdrop-blur">
          <UserMenu />
        </header>
        <div key={location.pathname} className="flex-1 overflow-auto animate-fade-in">
          <Outlet />
        </div>
      </main>
    </div>
  )
}

// NavRow renders one sidebar entry — an external link opens in a new tab,
// everything else is an in-app NavLink with the teal active indicator.
function NavRow({ item }: { item: NavItem }) {
  const { to, label, icon: Icon, external } = item
  if (external) {
    return (
      <a
        href={to}
        target="_blank"
        rel="noreferrer"
        className="flex items-center gap-2.5 px-2.5 py-2 rounded-md text-sm text-zinc-400 hover:text-zinc-100 hover:bg-zinc-900/80 transition-all duration-200"
      >
        <Icon size={15} />
        <span>{label}</span>
      </a>
    )
  }
  return (
    <NavLink
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
  )
}
