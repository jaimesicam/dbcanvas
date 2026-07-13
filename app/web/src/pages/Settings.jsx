import { useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { useSettings } from '../settings/SettingsProvider.jsx'
import { THEMES } from '../theme/ThemeProvider.jsx'

// Settings — per-user preferences, saved to the account (not the browser) as soon as they are
// changed. Terminal mode applies to consoles opened from here on; existing sessions keep their
// place (right-click a session to dock/undock it).

const TERMINAL_MODES = [
  { id: 'docked', label: 'Docked', hint: 'Opens as a tab in the bottom terminal dock.' },
  { id: 'undocked', label: 'Undocked', hint: 'Opens in its own floating, movable window.' },
]

function Row({ title, hint, children }) {
  return (
    <div className="space-y-2 rounded-xl border bg-surface p-4">
      <div>
        <div className="text-sm font-semibold">{title}</div>
        <div className="text-xs text-muted">{hint}</div>
      </div>
      {children}
    </div>
  )
}

export default function Settings() {
  const { settings, save, loaded } = useSettings()
  const [err, setErr] = useState('')

  const set = async (patch) => {
    setErr('')
    try { await save(patch) } catch (e) { setErr(e.message) }
  }

  return (
    <div className="max-w-2xl space-y-4">
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
      {!loaded && <div className="text-xs text-muted">Loading your settings…</div>}

      <Row title="Terminal" hint="Where a node console opens when you launch one.">
        <div className="grid gap-2 sm:grid-cols-2">
          {TERMINAL_MODES.map((m) => {
            const on = settings.terminalMode === m.id
            return (
              <button key={m.id} onClick={() => set({ terminalMode: m.id })}
                className={`flex items-start gap-2.5 rounded-lg border p-3 text-left transition ${on ? 'border-primary bg-primary/10' : 'hover:bg-surface2'}`}>
                <span className={`mt-0.5 ${on ? 'text-primary' : 'text-muted'}`}>
                  <Icon.Nodes size={16} />
                </span>
                <span className="min-w-0">
                  <span className="flex items-center gap-1.5 text-sm font-medium">
                    {m.label}
                    {m.id === 'docked' && <span className="text-[10px] font-normal text-muted">(default)</span>}
                    {on && <Icon.Check size={14} />}
                  </span>
                  <span className="block text-xs text-muted">{m.hint}</span>
                </span>
              </button>
            )
          })}
        </div>
      </Row>

      <Row title="Theme" hint="Applied now and whenever you sign in, on any browser.">
        <div className="grid gap-2 sm:grid-cols-3">
          {THEMES.map((t) => {
            const on = settings.theme === t.id
            return (
              <button key={t.id} onClick={() => set({ theme: t.id })}
                className={`flex items-center gap-2 rounded-lg border p-2.5 text-left transition ${on ? 'border-primary bg-primary/10' : 'hover:bg-surface2'}`}>
                <span className="h-5 w-5 shrink-0 rounded-full border" style={{ background: t.swatch }} />
                <span className="flex-1 truncate text-sm font-medium">{t.label}</span>
                {on && <Icon.Check size={16} />}
              </button>
            )
          })}
        </div>
      </Row>
    </div>
  )
}
