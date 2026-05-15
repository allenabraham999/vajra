import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { useNavigate } from 'react-router-dom'
import { auth, nodes, setAuthToken } from '../api/client'

interface AuthState {
  email: string | null
  token: string | null
  expiresAt: string | null
  isAdmin: boolean
}

interface AuthContextValue extends AuthState {
  login: (email: string, password: string) => Promise<void>
  register: (email: string, password: string) => Promise<{ apiKey: string }>
  logout: () => void
}

const AuthContext = createContext<AuthContextValue | null>(null)

const SESSION_EMAIL_KEY = 'vajra.email'

async function detectAdmin(): Promise<boolean> {
  try {
    await nodes.list()
    return true
  } catch {
    return false
  }
}

// consumeOAuthHash reads the URL fragment the master writes after a
// successful Google callback (#token=…&expires=…&email=…), then strips
// it from history so a refresh doesn't replay the token. Returns null
// when the page wasn't loaded via an OAuth redirect.
function consumeOAuthHash(): { token: string; email: string | null; expiresAt: string | null } | null {
  const hash = window.location.hash
  if (!hash || !hash.includes('token=')) return null
  // The fragment may start with "#" or "#/auth?" — strip the "#" and any
  // leading path segment so URLSearchParams sees a clean query string.
  let raw = hash.replace(/^#/, '')
  const qIdx = raw.indexOf('?')
  if (qIdx >= 0) raw = raw.slice(qIdx + 1)
  const params = new URLSearchParams(raw)
  const token = params.get('token')
  if (!token) return null
  const email = params.get('email')
  const expiresUnix = params.get('expires')
  const expiresAt = expiresUnix
    ? new Date(parseInt(expiresUnix, 10) * 1000).toISOString()
    : null
  window.history.replaceState(null, '', window.location.pathname + window.location.search)
  return { token, email, expiresAt }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const navigate = useNavigate()
  const justOAuth = useRef(false)

  const [state, setState] = useState<AuthState>(() => {
    const oauth = consumeOAuthHash()
    if (oauth) {
      setAuthToken(oauth.token)
      if (oauth.email) sessionStorage.setItem(SESSION_EMAIL_KEY, oauth.email)
      justOAuth.current = true
      return {
        email: oauth.email,
        token: oauth.token,
        expiresAt: oauth.expiresAt,
        isAdmin: false,
      }
    }
    return {
      email: sessionStorage.getItem(SESSION_EMAIL_KEY),
      token: null,
      expiresAt: null,
      isAdmin: false,
    }
  })

  // After an OAuth-initiated mount, fire the admin probe and bounce the
  // user to /sandboxes (where the post-login app lives).
  useEffect(() => {
    if (!justOAuth.current) return
    justOAuth.current = false
    void detectAdmin().then((isAdmin) => {
      setState((s) => ({ ...s, isAdmin }))
    })
    navigate('/sandboxes', { replace: true })
  }, [navigate])

  const login = useCallback(async (email: string, password: string) => {
    const r = await auth.login(email, password)
    setAuthToken(r.token)
    sessionStorage.setItem(SESSION_EMAIL_KEY, email)
    const isAdmin = await detectAdmin()
    setState({ email, token: r.token, expiresAt: r.expires_at, isAdmin })
  }, [])

  const register = useCallback(async (email: string, password: string) => {
    const r = await auth.register(email, password)
    // After register, immediately log in to get a JWT for the dashboard.
    await login(email, password)
    return { apiKey: r.api_key }
  }, [login])

  const logout = useCallback(() => {
    setAuthToken(null)
    sessionStorage.removeItem(SESSION_EMAIL_KEY)
    setState({ email: null, token: null, expiresAt: null, isAdmin: false })
  }, [])

  const value = useMemo<AuthContextValue>(
    () => ({ ...state, login, register, logout }),
    [state, login, register, logout],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
