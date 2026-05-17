import { useEffect, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { useAuth } from '../auth/AuthContext'
import { useToast } from '../components/Toast'
import Spinner from '../components/Spinner'
import Bolt from '../components/Bolt'
import { Copy, KeyRound } from 'lucide-react'
import { apiBase, auth as authApi } from '../api/client'

type Mode = 'login' | 'register'

export default function LoginPage({ initialMode = 'login' }: { initialMode?: Mode }) {
  const { login, register } = useAuth()
  const toast = useToast()
  const nav = useNavigate()

  const [mode, setMode] = useState<Mode>(initialMode)
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [issuedKey, setIssuedKey] = useState<string | null>(null)
  const [googleEnabled, setGoogleEnabled] = useState(false)

  useEffect(() => {
    let cancelled = false
    authApi
      .config()
      .then((cfg) => {
        if (!cancelled) setGoogleEnabled(cfg.google_oauth_enabled)
      })
      .catch(() => {
        // Probe failure is non-fatal — falls back to email/password only.
      })
    return () => {
      cancelled = true
    }
  }, [])

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    try {
      if (mode === 'login') {
        await login(email, password)
        nav('/', { replace: true })
      } else {
        const r = await register(email, password)
        setIssuedKey(r.apiKey)
      }
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'Authentication failed'
      toast.error(msg)
    } finally {
      setBusy(false)
    }
  }

  if (issuedKey) {
    return (
      <div className="min-h-screen grid place-items-center bg-zinc-950 px-4 relative overflow-hidden">
        <BackgroundGlow />
        <div className="w-full max-w-md rounded-xl border border-zinc-800 bg-zinc-900/70 backdrop-blur p-6 shadow-2xl animate-slide-up relative">
          <div className="flex items-center gap-2.5 mb-3">
            <div className="size-9 rounded-md bg-teal-500/15 ring-1 ring-teal-500/30 grid place-items-center">
              <KeyRound size={17} className="text-teal-300" />
            </div>
            <h2 className="text-base font-semibold tracking-tight">Save your API key</h2>
          </div>
          <p className="text-xs text-zinc-400 mb-4 leading-relaxed">
            We won't show this again. Store it somewhere safe — you can use it as
            <span className="font-mono"> Authorization: Bearer </span>against
            the master API.
          </p>
          <div className="rounded-md border border-zinc-800 bg-zinc-950 p-2.5 flex items-center justify-between gap-2 mb-4">
            <code className="font-mono text-xs text-teal-300 break-all">{issuedKey}</code>
            <button
              onClick={() => {
                navigator.clipboard.writeText(issuedKey)
                toast.success('Copied to clipboard')
              }}
              className="shrink-0 rounded bg-zinc-800 hover:bg-zinc-700 px-2 py-1.5 text-zinc-200 transition-colors"
            >
              <Copy size={14} />
            </button>
          </div>
          <button
            onClick={() => nav('/', { replace: true })}
            className="w-full rounded-md bg-gradient-to-r from-teal-500 to-teal-600 hover:from-teal-400 hover:to-teal-500 text-zinc-950 font-medium py-2 text-sm shadow-[0_0_20px_-4px_rgba(20,184,166,0.6)] hover:shadow-[0_0_28px_-2px_rgba(20,184,166,0.8)] transition-all duration-200 hover:scale-[1.02]"
          >
            Continue to dashboard
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen grid place-items-center bg-zinc-950 px-4 relative overflow-hidden">
      <BackgroundGlow />
      <div className="w-full max-w-sm animate-fade-in relative">
        <div className="flex justify-center mb-7">
          <Link
            to="/"
            className="flex items-center gap-2.5 transition-opacity hover:opacity-80"
          >
            <Bolt size={28} />
            <span className="text-lg font-mono font-semibold tracking-[0.22em] text-zinc-100">
              VAJRA
            </span>
          </Link>
        </div>

        <div className="rounded-xl border border-zinc-800 bg-zinc-900/70 backdrop-blur p-6 shadow-2xl animate-slide-up">
          <div className="flex border-b border-zinc-800 mb-5 -mx-6 -mt-6 px-6">
            <button
              type="button"
              onClick={() => setMode('login')}
              className={`flex-1 py-3 text-sm transition-colors ${
                mode === 'login'
                  ? 'text-zinc-100 border-b-2 border-teal-500'
                  : 'text-zinc-500 hover:text-zinc-200'
              }`}
            >
              Sign in
            </button>
            <button
              type="button"
              onClick={() => setMode('register')}
              className={`flex-1 py-3 text-sm transition-colors ${
                mode === 'register'
                  ? 'text-zinc-100 border-b-2 border-teal-500'
                  : 'text-zinc-500 hover:text-zinc-200'
              }`}
            >
              Create account
            </button>
          </div>

          <form onSubmit={submit} className="space-y-3.5">
            <div>
              <label className="block text-xs text-zinc-400 mb-1.5">Email</label>
              <input
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                required
                autoComplete="email"
                className="w-full rounded-md bg-zinc-950 border border-zinc-800 px-3 py-2.5 text-sm focus:border-teal-500 focus:outline-none focus:ring-2 focus:ring-teal-500/30 transition-all"
              />
            </div>
            <div>
              <label className="block text-xs text-zinc-400 mb-1.5">Password</label>
              <input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
                minLength={mode === 'register' ? 8 : undefined}
                autoComplete={mode === 'login' ? 'current-password' : 'new-password'}
                className="w-full rounded-md bg-zinc-950 border border-zinc-800 px-3 py-2.5 text-sm focus:border-teal-500 focus:outline-none focus:ring-2 focus:ring-teal-500/30 transition-all"
              />
              {mode === 'register' && (
                <p className="text-[11px] text-zinc-500 mt-1">Minimum 8 characters.</p>
              )}
            </div>

            <button
              type="submit"
              disabled={busy}
              className="w-full rounded-md bg-gradient-to-r from-teal-500 to-teal-600 hover:from-teal-400 hover:to-teal-500 disabled:from-zinc-800 disabled:to-zinc-800 disabled:text-zinc-500 disabled:shadow-none text-zinc-950 font-medium py-2.5 text-sm flex items-center justify-center gap-2 shadow-[0_0_20px_-4px_rgba(20,184,166,0.6)] hover:shadow-[0_0_28px_-2px_rgba(20,184,166,0.8)] transition-all duration-200 hover:scale-[1.02] disabled:hover:scale-100"
            >
              {busy && <Spinner size={14} />}
              {mode === 'login' ? 'Sign in' : 'Create account'}
            </button>
          </form>

          {googleEnabled && (
            <>
              <div className="relative my-4 flex items-center">
                <div className="flex-1 border-t border-zinc-800" />
                <span className="mx-3 text-[11px] uppercase tracking-[0.2em] text-zinc-500">or</span>
                <div className="flex-1 border-t border-zinc-800" />
              </div>
              <button
                type="button"
                onClick={() => {
                  window.location.href = `${apiBase}/v1/auth/google`
                }}
                className="w-full rounded-md bg-white hover:bg-zinc-100 active:bg-zinc-200 text-zinc-900 font-medium py-2.5 text-sm flex items-center justify-center gap-2.5 transition-colors duration-150 hover:scale-[1.01]"
              >
                <GoogleIcon size={16} />
                {mode === 'login' ? 'Sign in with Google' : 'Sign up with Google'}
              </button>
            </>
          )}
        </div>

        <p className="text-center text-[11px] text-zinc-600 mt-5 font-mono">
          API @ <span className="text-zinc-500">{import.meta.env.VITE_API_URL || 'localhost:8080'}</span>
        </p>
      </div>
    </div>
  )
}

function GoogleIcon({ size = 16 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 48 48" aria-hidden="true">
      <path
        fill="#FFC107"
        d="M43.611 20.083H42V20H24v8h11.303c-1.649 4.657-6.08 8-11.303 8-6.627 0-12-5.373-12-12s5.373-12 12-12c3.059 0 5.842 1.154 7.961 3.039l5.657-5.657C34.046 6.053 29.268 4 24 4 12.955 4 4 12.955 4 24s8.955 20 20 20 20-8.955 20-20c0-1.341-.138-2.65-.389-3.917z"
      />
      <path
        fill="#FF3D00"
        d="M6.306 14.691l6.571 4.819C14.655 15.108 18.961 12 24 12c3.059 0 5.842 1.154 7.961 3.039l5.657-5.657C34.046 6.053 29.268 4 24 4 16.318 4 9.656 8.337 6.306 14.691z"
      />
      <path
        fill="#4CAF50"
        d="M24 44c5.166 0 9.86-1.977 13.409-5.192l-6.19-5.238C29.211 35.091 26.715 36 24 36c-5.202 0-9.619-3.317-11.283-7.946l-6.522 5.025C9.505 39.556 16.227 44 24 44z"
      />
      <path
        fill="#1976D2"
        d="M43.611 20.083H42V20H24v8h11.303c-.792 2.237-2.231 4.166-4.087 5.571.001-.001.002-.001.003-.002l6.19 5.238C36.971 39.205 44 34 44 24c0-1.341-.138-2.65-.389-3.917z"
      />
    </svg>
  )
}

function BackgroundGlow() {
  return (
    <>
      <div className="pointer-events-none absolute -top-32 left-1/2 -translate-x-1/2 size-[520px] rounded-full bg-teal-500/10 blur-[120px]" />
      <div className="pointer-events-none absolute bottom-0 right-0 size-[360px] rounded-full bg-teal-700/5 blur-[100px]" />
    </>
  )
}
