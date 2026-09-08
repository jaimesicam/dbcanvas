import { createContext, useContext, useCallback, useEffect, useState } from 'react'
import { api } from '../lib/api.js'

// phase ∈ {loading, setup, anon, authed}
const AuthContext = createContext(null)

export function AuthProvider({ children }) {
  const [phase, setPhase] = useState('loading')
  const [user, setUser] = useState(null)

  const refresh = useCallback(async () => {
    try {
      const s = await api.status()
      if (!s.initialized) {
        setUser(null)
        setPhase('setup')
      } else if (s.authenticated) {
        setUser(s.user)
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

  const setup = useCallback(async (username, password) => {
    await api.setup(username, password)
    await refresh()
  }, [refresh])

  const login = useCallback(async (username, password) => {
    await api.login(username, password)
    await refresh()
  }, [refresh])

  const register = useCallback(async (username, password) => {
    return api.register(username, password)
  }, [])

  const logout = useCallback(async () => {
    try {
      await api.logout()
    } finally {
      await refresh()
    }
  }, [refresh])

  return (
    <AuthContext.Provider value={{ phase, user, refresh, setup, login, register, logout }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth() {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
