import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuth } from '../auth/AuthContext'
import { useToast } from '../components/Toast'
import Spinner from '../components/Spinner'
import { Copy, KeyRound } from 'lucide-react'

type Mode = 'login' | 'register'

export default function LoginPage() {
  const { login, register } = useAuth()
  const toast = useToast()
  const nav = useNavigate()

  const [mode, setMode] = useState<Mode>('login')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [issuedKey, setIssuedKey] = useState<string | null>(null)

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
      <div className="min-h-screen grid place-items-center bg-zinc-950 px-4">
        <div className="w-full max-w-md rounded-lg border border-zinc-800 bg-zinc-900 p-6 shadow-2xl">
          <div className="flex items-center gap-2 mb-3">
            <div className="size-8 rounded bg-emerald-500/20 grid place-items-center">
              <KeyRound size={16} className="text-emerald-400" />
            </div>
            <h2 className="text-base font-semibold">Save your API key</h2>
          </div>
          <p className="text-xs text-zinc-400 mb-4">
            We won't show this again. Store it somewhere safe — you can use it as
            <span className="font-mono"> Authorization: Bearer </span>against
            the master API.
          </p>
          <div className="rounded-md border border-zinc-800 bg-zinc-950 p-2.5 flex items-center justify-between gap-2 mb-4">
            <code className="font-mono text-xs text-emerald-300 break-all">{issuedKey}</code>
            <button
              onClick={() => {
                navigator.clipboard.writeText(issuedKey)
                toast.success('Copied to clipboard')
              }}
              className="shrink-0 rounded bg-zinc-800 hover:bg-zinc-700 px-2 py-1.5 text-zinc-200"
            >
              <Copy size={14} />
            </button>
          </div>
          <button
            onClick={() => nav('/', { replace: true })}
            className="w-full rounded-md bg-emerald-500 hover:bg-emerald-400 text-zinc-950 font-medium py-2 text-sm"
          >
            Continue to dashboard
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen grid place-items-center bg-zinc-950 px-4">
      <div className="w-full max-w-md">
        <div className="flex items-center gap-2 justify-center mb-6">
          <div className="size-9 rounded bg-gradient-to-br from-emerald-500 to-emerald-700 grid place-items-center font-bold text-zinc-950">
            V
          </div>
          <div className="text-lg font-semibold tracking-tight">Vajra</div>
        </div>

        <div className="rounded-lg border border-zinc-800 bg-zinc-900 p-6 shadow-2xl">
          <div className="flex border-b border-zinc-800 mb-4 -mx-6 -mt-6 px-6">
            <button
              type="button"
              onClick={() => setMode('login')}
              className={`flex-1 py-3 text-sm transition-colors ${
                mode === 'login'
                  ? 'text-zinc-100 border-b-2 border-emerald-500'
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
                  ? 'text-zinc-100 border-b-2 border-emerald-500'
                  : 'text-zinc-500 hover:text-zinc-200'
              }`}
            >
              Create account
            </button>
          </div>

          <form onSubmit={submit} className="space-y-3">
            <div>
              <label className="block text-xs text-zinc-400 mb-1">Email</label>
              <input
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                required
                autoComplete="email"
                className="w-full rounded-md bg-zinc-950 border border-zinc-800 px-3 py-2 text-sm focus:border-emerald-600 focus:outline-none focus:ring-2 focus:ring-emerald-600/30"
              />
            </div>
            <div>
              <label className="block text-xs text-zinc-400 mb-1">Password</label>
              <input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
                minLength={mode === 'register' ? 8 : undefined}
                autoComplete={mode === 'login' ? 'current-password' : 'new-password'}
                className="w-full rounded-md bg-zinc-950 border border-zinc-800 px-3 py-2 text-sm focus:border-emerald-600 focus:outline-none focus:ring-2 focus:ring-emerald-600/30"
              />
              {mode === 'register' && (
                <p className="text-[11px] text-zinc-500 mt-1">Minimum 8 characters.</p>
              )}
            </div>

            <button
              type="submit"
              disabled={busy}
              className="w-full rounded-md bg-emerald-500 hover:bg-emerald-400 disabled:bg-zinc-800 disabled:text-zinc-500 text-zinc-950 font-medium py-2 text-sm flex items-center justify-center gap-2"
            >
              {busy && <Spinner size={14} />}
              {mode === 'login' ? 'Sign in' : 'Create account'}
            </button>
          </form>
        </div>

        <p className="text-center text-[11px] text-zinc-600 mt-4 font-mono">
          API @ <span className="text-zinc-500">{import.meta.env.VITE_API_URL || 'localhost:8080'}</span>
        </p>
      </div>
    </div>
  )
}
