import { useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import {
  ArrowRight,
  BookOpen,
  Zap,
  Lock,
  Package,
  Cloud,
  Code,
  CreditCard,
  Check,
  Terminal,
  Server,
  Boxes,
  Cpu,
  Bot,
  GitBranch,
  Layers,
} from 'lucide-react'
import Bolt from '../components/Bolt'

const GITHUB_URL = 'https://github.com/allenabraham999/vajra'
const DOCS_URL = '/v1/docs'

// GithubIcon — lucide-react dropped brand icons, so the GitHub mark is
// inlined here as an SVG.
function GithubIcon({ size = 16, className = '' }: { size?: number; className?: string }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="currentColor"
      className={className}
      aria-hidden="true"
    >
      <path d="M12 .5C5.37.5 0 5.87 0 12.5c0 5.3 3.44 9.8 8.21 11.39.6.11.82-.26.82-.58 0-.29-.01-1.04-.02-2.05-3.34.73-4.04-1.61-4.04-1.61-.55-1.39-1.34-1.76-1.34-1.76-1.09-.75.08-.73.08-.73 1.21.09 1.84 1.24 1.84 1.24 1.07 1.84 2.81 1.31 3.5 1 .11-.78.42-1.31.76-1.61-2.67-.3-5.47-1.33-5.47-5.93 0-1.31.47-2.38 1.24-3.22-.12-.3-.54-1.52.12-3.17 0 0 1.01-.32 3.3 1.23a11.5 11.5 0 0 1 6 0c2.29-1.55 3.3-1.23 3.3-1.23.66 1.65.24 2.87.12 3.17.77.84 1.24 1.91 1.24 3.22 0 4.61-2.81 5.62-5.49 5.92.43.37.81 1.1.81 2.22 0 1.6-.01 2.89-.01 3.29 0 .32.22.7.83.58A12.01 12.01 0 0 0 24 12.5C24 5.87 18.63.5 12 .5z" />
    </svg>
  )
}

// Landing is the marketing entry point shown at "/" for unauthenticated
// visitors. It is purely presentational — no auth or API calls.
export default function Landing() {
  return (
    <div className="min-h-screen bg-zinc-950 text-zinc-100 antialiased">
      <NavBar />
      <Hero />
      <Performance />
      <Features />
      <Architecture />
      <UseCases />
      <Pricing />
      <CallToAction />
      <Footer />
    </div>
  )
}

/* ----------------------------------------------------------------------
 * Reveal — wraps content so it fades/slides in once it enters the viewport.
 * -------------------------------------------------------------------- */
function Reveal({
  children,
  className = '',
  delay = 0,
}: {
  children: ReactNode
  className?: string
  delay?: number
}) {
  const ref = useRef<HTMLDivElement>(null)
  const [visible, setVisible] = useState(false)

  useEffect(() => {
    const el = ref.current
    if (!el) return
    const obs = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) {
          setVisible(true)
          obs.disconnect()
        }
      },
      { threshold: 0.12, rootMargin: '0px 0px -40px 0px' },
    )
    obs.observe(el)
    return () => obs.disconnect()
  }, [])

  return (
    <div
      ref={ref}
      className={`reveal ${visible ? 'is-visible' : ''} ${className}`}
      style={{ animationDelay: `${delay}ms` }}
    >
      {children}
    </div>
  )
}

/* ----------------------------------------------------------------------
 * Navigation
 * -------------------------------------------------------------------- */
function NavBar() {
  return (
    <header className="sticky top-0 z-50 border-b border-zinc-900/80 bg-zinc-950/80 backdrop-blur-md">
      <div className="mx-auto flex h-16 max-w-6xl items-center justify-between px-5">
        <Link to="/" className="flex items-center gap-2.5">
          <Bolt size={24} />
          <span className="font-mono text-[15px] font-semibold tracking-[0.22em] text-zinc-100">
            VAJRA
          </span>
        </Link>

        <nav className="hidden items-center gap-7 md:flex">
          <a href="#performance" className="text-sm text-zinc-400 transition-colors hover:text-zinc-100">
            Performance
          </a>
          <a href="#features" className="text-sm text-zinc-400 transition-colors hover:text-zinc-100">
            Features
          </a>
          <a href="#pricing" className="text-sm text-zinc-400 transition-colors hover:text-zinc-100">
            Pricing
          </a>
          <a
            href={DOCS_URL}
            className="text-sm text-zinc-400 transition-colors hover:text-zinc-100"
          >
            Docs
          </a>
          <a
            href={GITHUB_URL}
            target="_blank"
            rel="noreferrer"
            className="flex items-center gap-1.5 text-sm text-zinc-400 transition-colors hover:text-zinc-100"
          >
            <GithubIcon size={15} />
            GitHub
          </a>
        </nav>

        <div className="flex items-center gap-2.5">
          <Link
            to="/login"
            className="rounded-md px-3 py-1.5 text-sm text-zinc-300 transition-colors hover:text-zinc-100"
          >
            Sign in
          </Link>
          <Link
            to="/signup"
            className="rounded-md bg-gradient-to-r from-teal-400 to-teal-500 px-3.5 py-1.5 text-sm font-medium text-zinc-950 shadow-[0_0_20px_-6px_rgba(20,184,166,0.8)] transition-all duration-200 hover:scale-[1.03] hover:from-teal-300 hover:to-teal-400"
          >
            Get Started
          </Link>
        </div>
      </div>
    </header>
  )
}

/* ----------------------------------------------------------------------
 * Hero
 * -------------------------------------------------------------------- */
function Hero() {
  return (
    <section className="relative overflow-hidden">
      {/* Background: drifting grid + teal glows */}
      <div
        className="animate-grid pointer-events-none absolute inset-0 opacity-[0.07]"
        style={{
          backgroundImage:
            'linear-gradient(#fff 1px, transparent 1px), linear-gradient(90deg, #fff 1px, transparent 1px)',
          backgroundSize: '48px 48px',
        }}
      />
      <div className="pointer-events-none absolute -top-40 left-1/4 size-[620px] -translate-x-1/2 rounded-full bg-teal-500/10 blur-[140px]" />
      <div className="pointer-events-none absolute -right-20 top-40 size-[420px] rounded-full bg-teal-600/10 blur-[130px]" />
      <div className="pointer-events-none absolute inset-x-0 bottom-0 h-40 bg-gradient-to-b from-transparent to-zinc-950" />

      <div className="relative mx-auto grid max-w-6xl items-center gap-14 px-5 py-20 lg:grid-cols-2 lg:py-28">
        {/* Left: copy */}
        <div className="animate-fade-in">
          <div className="mb-5 text-sm font-medium uppercase tracking-wider text-teal-400">
            Verified on bare metal · Open source-friendly
          </div>

          <h1 className="text-5xl font-bold leading-[1.08] tracking-tight md:text-6xl">
            Sandbox cloud for{' '}
            <span className="bg-gradient-to-r from-teal-300 to-teal-500 bg-clip-text text-transparent">
              AI agents.
            </span>
          </h1>

          <p className="mt-5 max-w-2xl text-xl leading-relaxed text-zinc-400">
            Hardware-isolated microVMs. Built for autonomous agents that need to
            write, run, and verify code in sub-second time.
          </p>

          <div className="mt-8 flex flex-wrap items-center gap-3">
            <Link
              to="/signup"
              className="group flex items-center gap-2 rounded-lg bg-gradient-to-r from-teal-400 to-teal-500 px-5 py-3 text-sm font-semibold text-zinc-950 shadow-[0_0_28px_-6px_rgba(20,184,166,0.85)] transition-all duration-200 hover:scale-[1.03] hover:from-teal-300 hover:to-teal-400"
            >
              Get Started
              <ArrowRight size={16} className="transition-transform group-hover:translate-x-0.5" />
            </Link>
            <a
              href={GITHUB_URL}
              target="_blank"
              rel="noreferrer"
              className="flex items-center gap-2 rounded-lg border border-zinc-700 px-5 py-3 text-sm font-medium text-zinc-200 transition-all duration-200 hover:border-zinc-500 hover:bg-zinc-900"
            >
              <GithubIcon size={16} />
              View on GitHub
            </a>
          </div>

          <div className="mt-9">
            <CodeSnippet />
          </div>
        </div>

        {/* Right: animated dashboard preview */}
        <div className="animate-fade-in lg:pl-4">
          <DashboardPreview />
        </div>
      </div>
    </section>
  )
}

function CodeSnippet() {
  return (
    <div className="overflow-hidden rounded-xl border border-zinc-800 bg-zinc-900/70 shadow-2xl backdrop-blur">
      <div className="flex items-center gap-2 border-b border-zinc-800 px-4 py-2.5">
        <span className="size-2.5 rounded-full bg-red-500/70" />
        <span className="size-2.5 rounded-full bg-yellow-500/70" />
        <span className="size-2.5 rounded-full bg-green-500/70" />
        <span className="ml-2 font-mono text-[11px] text-zinc-500">quickstart.py</span>
      </div>
      <pre className="overflow-x-auto px-4 py-4 font-mono text-[12.5px] leading-relaxed">
        <code>
          <span className="text-fuchsia-400">from</span>
          <span className="text-zinc-300"> vajra </span>
          <span className="text-fuchsia-400">import</span>
          <span className="text-teal-300"> Client</span>
          {'\n\n'}
          <span className="text-zinc-200">client</span>
          <span className="text-zinc-500"> = </span>
          <span className="text-teal-300">Client</span>
          <span className="text-zinc-500">(</span>
          <span className="text-zinc-400">api_key</span>
          <span className="text-zinc-500">=</span>
          <span className="text-amber-300">"vj_live_..."</span>
          <span className="text-zinc-500">)</span>
          {'\n'}
          <span className="text-zinc-200">sandbox</span>
          <span className="text-zinc-500"> = client.sandboxes.</span>
          <span className="text-teal-300">create</span>
          <span className="text-zinc-500">(</span>
          <span className="text-zinc-400">template</span>
          <span className="text-zinc-500">=</span>
          <span className="text-amber-300">"ubuntu-noble"</span>
          <span className="text-zinc-500">)</span>
          {'\n'}
          <span className="text-zinc-200">result</span>
          <span className="text-zinc-500"> = sandbox.</span>
          <span className="text-teal-300">exec</span>
          <span className="text-zinc-500">(</span>
          <span className="text-amber-300">{"\"python -c 'print(\\\"hello\\\")'\""}</span>
          <span className="text-zinc-500">)</span>
        </code>
      </pre>
    </div>
  )
}

function DashboardPreview() {
  const rows = [
    { name: 'agent-run-4f2a', tmpl: 'ubuntu-noble', status: 'running', ms: '27ms' },
    { name: 'ci-build-9c10', tmpl: 'python-3.12', status: 'running', ms: '24ms' },
    { name: 'sbx-eval-71be', tmpl: 'node-22', status: 'booting', ms: '—' },
    { name: 'review-2d8f', tmpl: 'ubuntu-noble', status: 'running', ms: '31ms' },
  ]
  return (
    <div className="animate-float overflow-hidden rounded-xl border border-zinc-800 bg-zinc-900/80 shadow-[0_30px_80px_-30px_rgba(20,184,166,0.35)] backdrop-blur">
      <div className="flex items-center gap-2 border-b border-zinc-800 bg-zinc-950/60 px-4 py-2.5">
        <span className="size-2.5 rounded-full bg-red-500/70" />
        <span className="size-2.5 rounded-full bg-yellow-500/70" />
        <span className="size-2.5 rounded-full bg-green-500/70" />
        <span className="ml-2 font-mono text-[11px] text-zinc-500">vajra · sandboxes</span>
      </div>

      <div className="p-4">
        <div className="mb-3 flex items-center justify-between">
          <div>
            <div className="text-sm font-semibold text-zinc-100">Sandboxes</div>
            <div className="font-mono text-[10.5px] text-zinc-500">
              3 running · 1 booting · ap-south-1
            </div>
          </div>
          <div className="rounded-md bg-teal-500/90 px-2.5 py-1 text-[11px] font-medium text-zinc-950">
            + New sandbox
          </div>
        </div>

        <div className="space-y-1.5">
          {rows.map((r) => (
            <div
              key={r.name}
              className="flex items-center gap-3 rounded-lg border border-zinc-800/80 bg-zinc-950/50 px-3 py-2.5"
            >
              <span
                className={`size-2 shrink-0 rounded-full ${
                  r.status === 'running'
                    ? 'bg-teal-400 shadow-[0_0_8px_rgba(45,212,191,0.8)]'
                    : 'animate-pulse-soft bg-amber-400'
                }`}
              />
              <span className="min-w-0 flex-1 truncate font-mono text-[12px] text-zinc-200">
                {r.name}
              </span>
              <span className="hidden font-mono text-[11px] text-zinc-500 sm:inline">
                {r.tmpl}
              </span>
              <span
                className={`rounded px-1.5 py-0.5 font-mono text-[10.5px] ${
                  r.status === 'running'
                    ? 'bg-teal-500/10 text-teal-300'
                    : 'bg-amber-500/10 text-amber-300'
                }`}
              >
                {r.ms}
              </span>
            </div>
          ))}
        </div>

        <div className="mt-3 grid grid-cols-3 gap-2">
          {[
            ['p50 boot', '26ms'],
            ['uptime', '99.98%'],
            ['vCPU-hrs', '14.2k'],
          ].map(([label, value]) => (
            <div key={label} className="rounded-lg border border-zinc-800/80 bg-zinc-950/50 px-2.5 py-2">
              <div className="text-[10px] uppercase tracking-wide text-zinc-500">{label}</div>
              <div className="font-mono text-sm text-teal-300">{value}</div>
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}

/* ----------------------------------------------------------------------
 * Performance — hero stat numbers
 * -------------------------------------------------------------------- */
function Performance() {
  const stats = [
    { value: '28ms', label: 'Pool hit · RAM-to-RAM restore' },
    { value: '115ms', label: 'Cold restore · disk snapshot to RAM' },
    { value: '3-5s', label: 'Fresh cold boot · one-time, then pool warms' },
    { value: 'Adaptive', label: 'Per-template pool sizing in real time' },
  ]
  return (
    <section id="performance" className="scroll-mt-20 border-y border-zinc-900 bg-zinc-950">
      <div className="mx-auto max-w-6xl px-5 py-16">
        <Reveal className="mb-10 text-center">
          <h2 className="text-sm font-medium uppercase tracking-[0.25em] text-teal-400">
            Performance
          </h2>
          <p className="mt-2 text-2xl font-semibold tracking-tight text-zinc-100 sm:text-3xl">
            Built for speed at every tier.
          </p>
        </Reveal>
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {stats.map((s, i) => (
            <Reveal
              key={s.label}
              delay={i * 90}
              className="rounded-xl border border-zinc-800 bg-gradient-to-b from-zinc-900/80 to-zinc-950 p-6 text-center"
            >
              <div className="bg-gradient-to-r from-teal-200 to-teal-500 bg-clip-text font-mono text-4xl font-bold tracking-tight text-transparent sm:text-5xl">
                {s.value}
              </div>
              <div className="mt-2 text-xs text-zinc-400 sm:text-sm">{s.label}</div>
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  )
}

/* ----------------------------------------------------------------------
 * Features — 3x2 grid
 * -------------------------------------------------------------------- */
function Features() {
  const features = [
    {
      icon: Zap,
      title: 'Instant Sandbox Boots',
      body: 'Pre-warmed pool of microVMs ready to assign. Sub-30ms creates verified live.',
    },
    {
      icon: Lock,
      title: 'Hardware Isolation',
      body: 'Cloud Hypervisor + KVM. Each sandbox gets its own Linux kernel. Stronger than containers.',
    },
    {
      icon: Package,
      title: 'Templates & Snapshots',
      body: 'Build custom images. Snapshot running sandboxes. Restore in milliseconds.',
    },
    {
      icon: Cloud,
      title: 'Auto-Scaling',
      body: 'EC2 + bare metal hybrid. Launches instances on demand. Scales to millions.',
    },
    {
      icon: Code,
      title: 'Full API + SDK',
      body: 'REST API, Python SDK, CLI. Same sandbox in 3 lines of code.',
    },
    {
      icon: CreditCard,
      title: 'Real Billing',
      body: 'Stripe integration. Per-second metering. Free credits to start.',
    },
  ]
  return (
    <section id="features" className="scroll-mt-20 bg-zinc-950">
      <div className="mx-auto max-w-6xl px-5 py-20">
        <Reveal className="mb-12 text-center">
          <h2 className="text-sm font-medium uppercase tracking-[0.25em] text-teal-400">
            Features
          </h2>
          <p className="mt-2 text-2xl font-semibold tracking-tight text-zinc-100 sm:text-3xl">
            Everything you need to run untrusted code.
          </p>
        </Reveal>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {features.map((f, i) => (
            <Reveal
              key={f.title}
              delay={(i % 3) * 90}
              className="group rounded-xl border border-zinc-800 bg-zinc-900/40 p-6 transition-all duration-300 hover:-translate-y-1 hover:border-teal-500/40 hover:bg-zinc-900/80"
            >
              <div className="mb-4 grid size-11 place-items-center rounded-lg bg-teal-500/10 ring-1 ring-teal-500/25 transition-colors group-hover:bg-teal-500/20">
                <f.icon size={20} className="text-teal-300" />
              </div>
              <h3 className="text-base font-semibold text-zinc-100">{f.title}</h3>
              <p className="mt-2 text-sm leading-relaxed text-zinc-400">{f.body}</p>
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  )
}

/* ----------------------------------------------------------------------
 * Architecture
 * -------------------------------------------------------------------- */
function Architecture() {
  const nodes = [
    { icon: Server, title: 'Master', body: 'Stateless control plane. All state in PostgreSQL.' },
    { icon: Boxes, title: 'Cluster', body: 'Two-tier scheduler picks the cluster, then the node.' },
    { icon: Cpu, title: 'Agents', body: 'Bare-metal hosts running Cloud Hypervisor.' },
    { icon: Bolt, title: 'microVMs', body: 'Isolated Linux VMs restored from snapshots.' },
  ]
  return (
    <section className="border-y border-zinc-900 bg-gradient-to-b from-zinc-950 via-zinc-900/30 to-zinc-950">
      <div className="mx-auto max-w-6xl px-5 py-20">
        <Reveal className="mb-12 text-center">
          <h2 className="text-sm font-medium uppercase tracking-[0.25em] text-teal-400">
            Architecture
          </h2>
          <p className="mt-2 text-2xl font-semibold tracking-tight text-zinc-100 sm:text-3xl">
            A stateless control plane over bare-metal agents.
          </p>
          <p className="mx-auto mt-3 max-w-2xl text-sm leading-relaxed text-zinc-400">
            The master schedules work across clusters of bare-metal agents. Each
            agent boots isolated microVMs from snapshots — no shared kernel, no
            cold-start tax.
          </p>
        </Reveal>

        <Reveal className="flex flex-col items-stretch gap-3 md:flex-row md:items-center">
          {nodes.map((n, i) => (
            <div key={n.title} className="flex flex-1 flex-col items-center gap-3 md:flex-row">
              <div className="w-full rounded-xl border border-zinc-800 bg-zinc-900/70 p-5 text-center">
                <div className="mx-auto mb-3 grid size-12 place-items-center rounded-lg bg-teal-500/10 ring-1 ring-teal-500/25">
                  {n.icon === Bolt ? (
                    <Bolt size={22} />
                  ) : (
                    <n.icon size={22} className="text-teal-300" />
                  )}
                </div>
                <div className="text-sm font-semibold text-zinc-100">{n.title}</div>
                <div className="mt-1.5 text-xs leading-relaxed text-zinc-400">{n.body}</div>
              </div>
              {i < nodes.length - 1 && (
                <ArrowRight
                  size={20}
                  className="shrink-0 rotate-90 text-zinc-600 md:rotate-0"
                />
              )}
            </div>
          ))}
        </Reveal>
      </div>
    </section>
  )
}

/* ----------------------------------------------------------------------
 * Use cases
 * -------------------------------------------------------------------- */
function UseCases() {
  const cases = [
    {
      icon: Bot,
      title: 'AI Agents',
      body: 'Persistent sandboxes for autonomous agents.',
    },
    {
      icon: Terminal,
      title: 'Code Execution',
      body: 'Run user-submitted code safely.',
    },
    {
      icon: Layers,
      title: 'Development Environments',
      body: 'Cloud workspaces with isolation.',
    },
    {
      icon: GitBranch,
      title: 'CI/CD',
      body: 'Ephemeral build and test environments.',
    },
  ]
  return (
    <section className="bg-zinc-950">
      <div className="mx-auto max-w-6xl px-5 py-20">
        <Reveal className="mb-12 text-center">
          <h2 className="text-sm font-medium uppercase tracking-[0.25em] text-teal-400">
            Use Cases
          </h2>
          <p className="mt-2 text-2xl font-semibold tracking-tight text-zinc-100 sm:text-3xl">
            One platform, many workloads.
          </p>
        </Reveal>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {cases.map((c, i) => (
            <Reveal
              key={c.title}
              delay={i * 80}
              className="rounded-xl border border-zinc-800 bg-zinc-900/40 p-6"
            >
              <c.icon size={22} className="text-teal-300" />
              <h3 className="mt-4 text-base font-semibold text-zinc-100">{c.title}</h3>
              <p className="mt-1.5 text-sm leading-relaxed text-zinc-400">{c.body}</p>
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  )
}

/* ----------------------------------------------------------------------
 * Pricing
 * -------------------------------------------------------------------- */
function Pricing() {
  const tiers = [
    {
      name: 'Free',
      price: '$200',
      unit: 'in free credits',
      blurb: 'No card needed. Start building today.',
      features: ['1 vCPU sandboxes', 'Full REST API + SDK access', 'Community support'],
      cta: 'Get Started',
      to: '/signup',
      highlight: false,
    },
    {
      name: 'Pay as you go',
      price: '$0.06',
      unit: 'per vCPU-hour',
      blurb: 'Plus $0.01 per GB-hour. Per-second metering.',
      features: [
        'Unlimited sandboxes',
        'Snapshots & custom templates',
        'Auto-scaling across regions',
        'Email support',
      ],
      cta: 'Get Started',
      to: '/signup',
      highlight: true,
    },
    {
      name: 'Enterprise',
      price: 'Custom',
      unit: 'contact us',
      blurb: 'Dedicated capacity and white-glove onboarding.',
      features: ['Dedicated bare metal', 'SSO + audit logs', '99.95% uptime SLA', 'Priority support'],
      cta: 'Contact Sales',
      to: 'mailto:marko@vajra.dev',
      highlight: false,
    },
  ]
  return (
    <section id="pricing" className="scroll-mt-20 border-y border-zinc-900 bg-zinc-950">
      <div className="mx-auto max-w-6xl px-5 py-20">
        <Reveal className="mb-12 text-center">
          <h2 className="text-sm font-medium uppercase tracking-[0.25em] text-teal-400">
            Pricing
          </h2>
          <p className="mt-2 text-2xl font-semibold tracking-tight text-zinc-100 sm:text-3xl">
            Start free. Scale when you ship.
          </p>
        </Reveal>
        <div className="grid gap-5 lg:grid-cols-3">
          {tiers.map((t, i) => (
            <Reveal
              key={t.name}
              delay={i * 100}
              className={`relative flex flex-col rounded-2xl border p-7 ${
                t.highlight
                  ? 'border-teal-500/50 bg-gradient-to-b from-teal-500/[0.07] to-zinc-900/40 shadow-[0_0_50px_-20px_rgba(20,184,166,0.6)]'
                  : 'border-zinc-800 bg-zinc-900/40'
              }`}
            >
              {t.highlight && (
                <span className="absolute -top-3 left-1/2 -translate-x-1/2 rounded-full bg-gradient-to-r from-teal-400 to-teal-500 px-3 py-0.5 text-[11px] font-semibold text-zinc-950">
                  Most popular
                </span>
              )}
              <div className="text-sm font-medium text-zinc-300">{t.name}</div>
              <div className="mt-3 flex items-baseline gap-1.5">
                <span className="text-4xl font-bold tracking-tight text-zinc-100">{t.price}</span>
                <span className="text-sm text-zinc-500">{t.unit}</span>
              </div>
              <p className="mt-2 text-sm text-zinc-400">{t.blurb}</p>
              <ul className="mt-6 flex-1 space-y-2.5">
                {t.features.map((f) => (
                  <li key={f} className="flex items-start gap-2.5 text-sm text-zinc-300">
                    <Check size={16} className="mt-0.5 shrink-0 text-teal-400" />
                    {f}
                  </li>
                ))}
              </ul>
              {t.to.startsWith('mailto:') ? (
                <a
                  href={t.to}
                  className="mt-7 rounded-lg border border-zinc-700 py-2.5 text-center text-sm font-medium text-zinc-200 transition-colors hover:border-zinc-500 hover:bg-zinc-900"
                >
                  {t.cta}
                </a>
              ) : (
                <Link
                  to={t.to}
                  className={`mt-7 rounded-lg py-2.5 text-center text-sm font-semibold transition-all duration-200 ${
                    t.highlight
                      ? 'bg-gradient-to-r from-teal-400 to-teal-500 text-zinc-950 hover:scale-[1.02] hover:from-teal-300 hover:to-teal-400'
                      : 'border border-zinc-700 text-zinc-200 hover:border-zinc-500 hover:bg-zinc-900'
                  }`}
                >
                  {t.cta}
                </Link>
              )}
            </Reveal>
          ))}
        </div>
      </div>
    </section>
  )
}

/* ----------------------------------------------------------------------
 * Closing call to action
 * -------------------------------------------------------------------- */
function CallToAction() {
  return (
    <section className="relative overflow-hidden bg-zinc-950">
      <div className="pointer-events-none absolute left-1/2 top-1/2 size-[520px] -translate-x-1/2 -translate-y-1/2 rounded-full bg-teal-500/10 blur-[140px]" />
      <div className="relative mx-auto max-w-6xl px-5 py-24 text-center">
        <Reveal>
          <Bolt size={44} className="mx-auto" />
          <h2 className="mt-6 text-3xl font-semibold tracking-tight text-zinc-100 sm:text-4xl">
            Ship sandboxes that boot before you blink.
          </h2>
          <p className="mx-auto mt-4 max-w-xl text-base text-zinc-400">
            Spin up your first hardware-isolated microVM with $200 in free
            credits. No credit card required.
          </p>
          <div className="mt-8 flex flex-wrap justify-center gap-3">
            <Link
              to="/signup"
              className="group flex items-center gap-2 rounded-lg bg-gradient-to-r from-teal-400 to-teal-500 px-6 py-3 text-sm font-semibold text-zinc-950 shadow-[0_0_28px_-6px_rgba(20,184,166,0.85)] transition-all duration-200 hover:scale-[1.03] hover:from-teal-300 hover:to-teal-400"
            >
              Get Started Free
              <ArrowRight size={16} className="transition-transform group-hover:translate-x-0.5" />
            </Link>
            <a
              href={DOCS_URL}
              className="flex items-center gap-2 rounded-lg border border-zinc-700 px-6 py-3 text-sm font-medium text-zinc-200 transition-all duration-200 hover:border-zinc-500 hover:bg-zinc-900"
            >
              <BookOpen size={16} />
              Read the Docs
            </a>
          </div>
        </Reveal>
      </div>
    </section>
  )
}

/* ----------------------------------------------------------------------
 * Footer
 * -------------------------------------------------------------------- */
function Footer() {
  return (
    <footer className="border-t border-zinc-900 bg-zinc-950">
      <div className="mx-auto max-w-6xl px-5 py-12">
        <div className="flex flex-col items-center justify-between gap-6 sm:flex-row">
          <div className="flex items-center gap-2.5">
            <Bolt size={20} />
            <span className="font-mono text-sm font-semibold tracking-[0.22em] text-zinc-200">
              VAJRA
            </span>
          </div>
          <nav className="flex flex-wrap items-center justify-center gap-x-6 gap-y-2 text-sm text-zinc-400">
            <a href={DOCS_URL} className="transition-colors hover:text-zinc-100">
              Docs
            </a>
            <a href={DOCS_URL} className="transition-colors hover:text-zinc-100">
              API
            </a>
            <a
              href={GITHUB_URL}
              target="_blank"
              rel="noreferrer"
              className="transition-colors hover:text-zinc-100"
            >
              GitHub
            </a>
            <Link to="/login" className="transition-colors hover:text-zinc-100">
              Sign in
            </Link>
          </nav>
        </div>
        <div className="mt-8 flex flex-col items-center justify-between gap-2 border-t border-zinc-900 pt-6 text-center sm:flex-row sm:text-left">
          <p className="text-xs text-zinc-500">
            Vajra © 2026 — Built for the Kortix engineering lead trial.
          </p>
          <p className="font-mono text-xs text-zinc-600">
            Built with Go, React &amp; Cloud Hypervisor
          </p>
        </div>
      </div>
    </footer>
  )
}
