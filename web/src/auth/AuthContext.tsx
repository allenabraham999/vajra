import { createContext, useCallback, useContext, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { auth, setAuthToken } from '../api/client'

interface AuthState {
  email: string | null
  token: string | null
  expiresAt: string | null
}

interface AuthContextValue extends AuthState {
  login: (email: string, password: string) => Promise<void>
  register: (email: string, password: string) => Promise<{ apiKey: string }>
  logout: () => void
}

const AuthContext = createContext<AuthContextValue | null>(null)

const SESSION_EMAIL_KEY = 'vajra.email'

export function AuthProvider({ children }: { children: ReactNode }) {
  // Email persists across reloads for UX; the JWT does not.
  const [state, setState] = useState<AuthState>(() => ({
    email: sessionStorage.getItem(SESSION_EMAIL_KEY),
    token: null,
    expiresAt: null,
  }))

  const login = useCallback(async (email: string, password: string) => {
    const r = await auth.login(email, password)
    setAuthToken(r.token)
    sessionStorage.setItem(SESSION_EMAIL_KEY, email)
    setState({ email, token: r.token, expiresAt: r.expires_at })
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
    setState({ email: null, token: null, expiresAt: null })
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
