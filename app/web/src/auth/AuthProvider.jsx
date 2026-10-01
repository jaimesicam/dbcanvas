import { createContext, useContext, useCallback, useEffect, useState } from 'react'
import { api } from '../lib/api.js'

// phase ∈ {loading, setup, anon, authed}
const AuthContext = createContext(null)

export function AuthProvider({ children }) {
  const [phase, setPhase] = useState('loading')
  const [user, setUser] = useState(null)
  // A shared-session guest (app/share.go): who they are, when the status says this
  // tab is one. user is then the host they act as.
  const [guest, setGuest] = useState(null)

  const refresh = useCallback(async () => {
    try {
      const s = await api.status()
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

  const register = useCallback(async (username, password, profile) => {
    return api.register(username, password, profile)
  }, [])

  const logout = useCallback(async () => {
    try {
      await api.logout()
    } finally {
      await refresh()
    }
  }, [refresh])

  return (
    <AuthContext.Provider value={{ phase, user, guest, refresh, setup, login, register, logout, setUser }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth() {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
