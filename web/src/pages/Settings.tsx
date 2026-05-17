import { Link } from 'react-router-dom'
import { ChevronRight, KeyRound, Webhook } from 'lucide-react'
import { useAuth } from '../auth/AuthContext'
import PageHeader from '../components/PageHeader'

// SettingsPage is a light account hub: it shows who you're signed in as
// and links out to the integration surfaces (Webhooks, API Keys).
export default function SettingsPage() {
  const { email, isAdmin } = useAuth()

  return (
    <>
      <PageHeader
        title="Settings"
        description="Account details and integrations."
      />
      <div className="p-6 space-y-6 max-w-2xl">
        <section className="rounded-lg border border-zinc-800 bg-zinc-900/40 shadow-lg shadow-black/20 p-4">
          <h2 className="text-sm font-medium mb-3">Account</h2>
          <dl className="divide-y divide-zinc-900">
            <Row label="Email" value={email ?? '—'} />
            <Row label="Role" value={isAdmin ? 'Administrator' : 'Member'} />
          </dl>
        </section>

        <section>
          <h2 className="text-sm font-medium mb-2">Integrations</h2>
          <div className="space-y-2">
            <SettingsLink
              to="/webhooks"
              icon={<Webhook size={16} />}
              title="Webhooks"
              desc="Outbound HTTP notifications for sandbox lifecycle events."
            />
            <SettingsLink
              to="/api-keys"
              icon={<KeyRound size={16} />}
              title="API Keys"
              desc="Programmatic access tokens for the Vajra API."
            />
          </div>
        </section>

        <p className="text-xs text-zinc-600">
          More account settings are coming soon.
        </p>
      </div>
    </>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between py-2 text-sm">
      <dt className="text-zinc-500">{label}</dt>
      <dd className="font-mono text-zinc-200">{value}</dd>
    </div>
  )
}

function SettingsLink({
  to,
  icon,
  title,
  desc,
}: {
  to: string
  icon: React.ReactNode
  title: string
  desc: string
}) {
  return (
    <Link
      to={to}
      className="flex items-center gap-3 rounded-lg border border-zinc-800 bg-zinc-900/40 px-4 py-3 hover:border-teal-500/30 hover:bg-zinc-900/70 transition-colors group"
    >
      <span className="text-teal-400 shrink-0">{icon}</span>
      <span className="flex-1 min-w-0">
        <span className="block text-sm font-medium text-zinc-200">{title}</span>
        <span className="block text-xs text-zinc-500">{desc}</span>
      </span>
      <ChevronRight
        size={16}
        className="text-zinc-600 group-hover:text-teal-400 transition-colors"
      />
    </Link>
  )
}
