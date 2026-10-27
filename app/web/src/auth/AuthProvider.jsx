import { createContext, useContext, useCallback, useEffect, useState } from 'react'
import { api } from '../lib/api.js'

// phase ∈ {loading, setup, anon, authed}
// Exported for the browser smoke check (smoke/browser.jsx), which mounts the stack editor
// without signing in.
export const AuthContext = createContext(null)

export function AuthProvider({ children }) {
  const [phase, setPhase] = useState('loading')
  const [user, setUser] = useState(null)
  // A shared-session guest (app/share.go): who they are, when the status says this
  // tab is one. user is then the host they act as.
  const [guest, setGuest] = useState(null)
  // Whether a new sign-up is approved at once (app/auth.go), for the registration form.
  const [openRegistration, setOpenRegistration] = useState(false)

  const refresh = useCallback(async () => {
    try {
      const s = await api.status()
      setOpenRegistration(!!s.openRegistration)
      if (!s.initialized) {
        setUser(null)
        setPhase('setup')
      } else if (s.authenticated) {
        setUser(s.user)
        setGuest(s.guest || null)
        setPhase('authed')
      } else {
        setUser(null)
        setPhase('anon')
      }
    } catch {
      setUser(null)
      setPhase('anon')
    }
  }, [])

  useEffect(() => {
    refresh()
  }, [refresh])

  const setup = useCallback(async (username, password, profile) => {
    await api.setup(username, password, profile)
    await refresh()
  }, [refresh])

  const login = useCallback(async (username, password) => {
    await api.login(username, password)
    await refresh()
  }, [refresh])

  // With open registration the server signs the new account in, so the status is read
  // again to enter the app.
  const register = useCallback(async (username, password, profile) => {
    const res = await api.register(username, password, profile)
    if (res?.status === 'approved') await refresh()
    return res
  }, [refresh])

  const logout = useCallback(async () => {
    try {
      await api.logout()
    } finally {
      await refresh()
    }
  }, [refresh])

  return (
    <AuthContext.Provider value={{ phase, user, guest, openRegistration, refresh, setup, login, register, logout, setUser }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth() {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
