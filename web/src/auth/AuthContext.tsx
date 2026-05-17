import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  ApiError,
  auth,
  getAuthToken,
  nodes,
  setAuthToken,
  setAuthRefreshHandler,
} from '../api/client'

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

// localStorage keys for the persisted session. The JWT itself is owned
// by the API client (see setAuthToken); the email and expiry are kept
// here so a page reload can rehydrate the user identity and re-arm the
// proactive refresh timer without a round-trip.
const EMAIL_KEY = 'vajra.email'
const EXPIRES_KEY = 'vajra.token_expires'

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
  // rehydrated is set when this mount restored a token from localStorage
  // (a page reload). It gates the one-shot token-validation effect below.
  const rehydrated = useRef(false)

  const [state, setState] = useState<AuthState>(() => {
    const oauth = consumeOAuthHash()
    if (oauth) {
      setAuthToken(oauth.token)
      if (oauth.email) localStorage.setItem(EMAIL_KEY, oauth.email)
      if (oauth.expiresAt) localStorage.setItem(EXPIRES_KEY, oauth.expiresAt)
      justOAuth.current = true
      return {
        email: oauth.email,
        token: oauth.token,
        expiresAt: oauth.expiresAt,
        isAdmin: false,
      }
    }
    // Rehydrate a prior session: the API client already restored the JWT
    // from localStorage at module load, so getAuthToken() is the source
    // of truth for whether a session survived the reload.
    const token = getAuthToken()
    if (token) rehydrated.current = true
    return {
      email: localStorage.getItem(EMAIL_KEY),
      token,
      expiresAt: token ? localStorage.getItem(EXPIRES_KEY) : null,
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

  // After a reload that rehydrated a stored token, confirm the token is
  // still good before the user keeps acting on it. A successful refresh
  // re-mints the JWT and re-arms the expiry timer; a 401/403 means the
  // stored token expired, so clear the session and let the router bounce
  // to /login. A network error leaves the session intact — the user
  // stays signed in and the next real request retries the token.
  useEffect(() => {
    if (!rehydrated.current) return
    rehydrated.current = false
    void (async () => {
      try {
        const r = await auth.refresh()
        setAuthToken(r.token)
        localStorage.setItem(EXPIRES_KEY, r.expires_at)
        const isAdmin = await detectAdmin()
        setState((s) => ({
          ...s,
          token: r.token,
          expiresAt: r.expires_at,
          isAdmin,
        }))
      } catch (e) {
        if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
          setAuthToken(null)
          localStorage.removeItem(EMAIL_KEY)
          localStorage.removeItem(EXPIRES_KEY)
          setState({ email: null, token: null, expiresAt: null, isAdmin: false })
        }
        // Network/server error: keep the session and let later requests
        // retry rather than logging the user out on a transient blip.
      }
    })()
  }, [])

  const login = useCallback(async (email: string, password: string) => {
    const r = await auth.login(email, password)
    setAuthToken(r.token)
    localStorage.setItem(EMAIL_KEY, email)
    localStorage.setItem(EXPIRES_KEY, r.expires_at)
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
    localStorage.removeItem(EMAIL_KEY)
    localStorage.removeItem(EXPIRES_KEY)
    setState({ email: null, token: null, expiresAt: null, isAdmin: false })
  }, [])

  // refreshInFlight coalesces concurrent refreshes: the dashboard polls
  // several endpoints at once, so a lapsed token would otherwise fire a
  // burst of identical refresh calls.
  const refreshInFlight = useRef<Promise<string | null> | null>(null)

  // refresh re-mints the session JWT. Returns the new token, or null if
  // the session is unrecoverable — in which case it clears auth state so
  // the router bounces the user to /login instead of looping on 401s.
  const refresh = useCallback((): Promise<string | null> => {
    if (refreshInFlight.current) return refreshInFlight.current
    const p = (async (): Promise<string | null> => {
      try {
        const r = await auth.refresh()
        setAuthToken(r.token)
        localStorage.setItem(EXPIRES_KEY, r.expires_at)
        setState((s) => ({ ...s, token: r.token, expiresAt: r.expires_at }))
        return r.token
      } catch {
        setAuthToken(null)
        localStorage.removeItem(EMAIL_KEY)
        localStorage.removeItem(EXPIRES_KEY)
        setState({ email: null, token: null, expiresAt: null, isAdmin: false })
        return null
      } finally {
        refreshInFlight.current = null
      }
    })()
    refreshInFlight.current = p
    return p
  }, [])

  // Expose refresh to the API client so a 401 mid-poll triggers a
  // re-mint + replay rather than a hard logout.
  useEffect(() => {
    setAuthRefreshHandler(state.token ? refresh : null)
    return () => setAuthRefreshHandler(null)
  }, [state.token, refresh])

  // Proactively re-mint the token once half its lifetime has elapsed, so
  // a tab left open through a long operation never reaches expiry. Each
  // successful refresh moves expiresAt forward and re-arms this timer.
  // On a page reload the timer re-arms from the rehydrated expiresAt.
  useEffect(() => {
    if (!state.token || !state.expiresAt) return
    const msLeft = new Date(state.expiresAt).getTime() - Date.now()
    const delay = Math.min(Math.max(msLeft / 2, 30_000), 12 * 60 * 60 * 1000)
    const timer = window.setTimeout(() => void refresh(), delay)
    return () => window.clearTimeout(timer)
  }, [state.token, state.expiresAt, refresh])

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
