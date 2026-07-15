import { createContext, useCallback, useContext, useEffect, useState } from 'react'
import { api } from '../lib/api.js'
import { useTheme } from '../theme/ThemeProvider.jsx'

// Per-user UI preferences, stored server-side (see app/settings.go) so they follow the account
// rather than the browser. Mounted inside App — i.e. only once authenticated.
//
// Theme: the account's theme is the source of truth. It is applied on load, and every theme
// change (Settings page or the topbar picker) is saved back, so the choice survives a new
// browser. ThemeProvider's localStorage copy stays as the pre-load/offline fallback, which is
// what paints the login screen and avoids a flash before this fetch lands.

const DEFAULTS = { terminalMode: 'docked', theme: 'dark', deploymentBackend: 'docker' }
const SettingsCtx = createContext(null)

export function SettingsProvider({ children }) {
  const { theme, setTheme } = useTheme()
  const [settings, setSettings] = useState(DEFAULTS)
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    let stale = false
    api.settings()
      .then((s) => {
        if (stale) return
        setSettings(s)
        if (s.theme && s.theme !== theme) setTheme(s.theme)
      })
      .catch(() => { /* keep defaults; the settings page will surface save errors */ })
      .finally(() => { if (!stale) setLoaded(true) })
    return () => { stale = true }
    // Runs once on mount: `theme` is only the pre-load fallback to compare against.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // save applies a patch optimistically (the UI reacts at once) and persists it. On failure the
  // previous settings are restored and the error is re-thrown for the caller to display.
  const save = useCallback(async (patch) => {
    const prev = settings
    const next = { ...settings, ...patch }
    setSettings(next)
    if (next.theme !== prev.theme) setTheme(next.theme)
    try {
      setSettings(await api.saveSettings(next))
    } catch (e) {
      setSettings(prev)
      if (prev.theme !== next.theme) setTheme(prev.theme)
      throw e
    }
  }, [settings, setTheme])

  return (
    <SettingsCtx.Provider value={{ settings, save, loaded }}>
      {children}
    </SettingsCtx.Provider>
  )
}

// useSettings is safe outside the provider (returns the defaults and a no-op save), so
// components that may render before/without it don't need a guard.
export function useSettings() {
  return useContext(SettingsCtx) ?? { settings: DEFAULTS, save: async () => {}, loaded: false }
}
