import { createContext, useContext, useCallback, useEffect, useState } from 'react'
import { api } from '../lib/api.js'

// phase ∈ {loading, anon, authed}. anon is signed out: there are no passwords, and a
// browser signs in through the one-time link the dbcanvas command opens. For a
// shared-session guest it means their credential stopped working.
// Exported for the browser smoke check (smoke/browser.jsx), which mounts the stack editor
// without signing in.
export const AuthContext = createContext(null)

export function AuthProvider({ children }) {
  const [phase, setPhase] = useState('loading')
  const [user, setUser] = useState(null)
  // A shared-session guest (app/share.go): who they are, when the status says this
  // tab is one. user is then the host they act as.
  const [guest, setGuest] = useState(null)

  const refresh = useCallback(async () => {
    try {
      const s = await api.status()
      if (s.authenticated) {
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

  const logout = useCallback(async () => {
    try {
      await api.logout()
    } finally {
      await refresh()
    }
  }, [refresh])

  return (
    <AuthContext.Provider value={{ phase, user, guest, refresh, logout, setUser }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth() {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
