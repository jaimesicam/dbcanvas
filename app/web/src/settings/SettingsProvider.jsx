import { createContext, useCallback, useContext, useEffect, useState } from 'react'
import { api } from '../lib/api.js'
import { useTheme } from '../theme/ThemeProvider.jsx'
import { TABS_DEFAULT } from '../lib/tabs.js'

// Per-user UI preferences, stored server-side (see app/settings.go) so they follow the account
// rather than the browser. Mounted inside App — i.e. only once authenticated.
//
// Appearance (theme + look): the account's copy is the source of truth. Both are applied on
// load, and every change (Settings page or the topbar picker) is saved back, so the choice
// survives a new browser. ThemeProvider's localStorage copies stay as the pre-load/offline
// fallback, which is what paints the login screen and avoids a flash before this fetch lands.

const DEFAULTS = {
  terminalMode: 'docked', theme: 'dark', look: 'modern',
  deploymentBackend: 'docker', nodeLibrary: 'menu', maxTabs: TABS_DEFAULT,
}
// Instance-wide, not per user (app/syssettings.go). Kept here so the one fetch
// serves both the settings page and the designer, which needs the upload
// ceiling to refuse an over-size drop before sending it. Mirror of
// defaultSystemSettings() — the server's value wins as soon as it lands.
// sshForwarding and experimental are env-derived and read-only
// (SSH_FORWARDING_HOST, EXPERIMENTAL); both off until the server says otherwise, so
// the designer never offers a tunnel command the installation cannot produce, and a
// feature that is not ready never flashes into a menu before the fetch lands.
const SYSTEM_DEFAULTS = {
  maxUploadBytes: 4 * 1024 * 1024 * 1024, maxTokenDays: 90,
  sshForwarding: { enabled: false }, experimental: false,
}
const SettingsCtx = createContext(null)

export function SettingsProvider({ children }) {
  const { theme, setTheme, look, setLook } = useTheme()
  const [settings, setSettings] = useState(DEFAULTS)
  const [system, setSystem] = useState(SYSTEM_DEFAULTS)
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    let stale = false
    api.settings()
      .then((s) => {
        if (stale) return
        setSettings(s)
        if (s.theme && s.theme !== theme) setTheme(s.theme)
        if (s.look && s.look !== look) setLook(s.look)
      })
      .catch(() => { /* keep defaults; the settings page will surface save errors */ })
      .finally(() => { if (!stale) setLoaded(true) })
    api.systemSettings()
      .then((s) => { if (!stale) setSystem(s) })
      .catch(() => { /* keep the mirrored defaults */ })
    return () => { stale = true }
    // Runs once on mount: `theme`/`look` are only the pre-load fallbacks to compare against.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // save applies a patch optimistically (the UI reacts at once) and persists it. On failure the
  // previous settings are restored and the error is re-thrown for the caller to display.
  const save = useCallback(async (patch) => {
    const prev = settings
    const next = { ...settings, ...patch }
    setSettings(next)
    if (next.theme !== prev.theme) setTheme(next.theme)
    if (next.look !== prev.look) setLook(next.look)
    try {
      setSettings(await api.saveSettings(next))
    } catch (e) {
      setSettings(prev)
      if (prev.theme !== next.theme) setTheme(prev.theme)
      if (prev.look !== next.look) setLook(prev.look)
      throw e
    }
  }, [settings, setTheme, setLook])

  // saveSystem persists an instance-wide patch. Unlike save() it is NOT optimistic:
  // a non-admin's PUT is refused with 403, and briefly showing a limit that the
  // server never accepted would be worse than a short wait.
  const saveSystem = useCallback(async (patch) => {
    setSystem(await api.saveSystemSettings({ ...system, ...patch }))
  }, [system])

  return (
    <SettingsCtx.Provider value={{ settings, save, system, saveSystem, loaded }}>
      {children}
    </SettingsCtx.Provider>
  )
}

// useSettings is safe outside the provider (returns the defaults and a no-op save), so
// components that may render before/without it don't need a guard.
export function useSettings() {
  return useContext(SettingsCtx) ?? {
    settings: DEFAULTS, save: async () => {},
    system: SYSTEM_DEFAULTS, saveSystem: async () => {},
    loaded: false,
  }
}
