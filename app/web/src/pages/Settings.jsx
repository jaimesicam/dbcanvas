import { useEffect, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Button, inputCls } from '../components/ui.jsx'
import { useAuth } from '../auth/AuthProvider.jsx'
import { useSettings } from '../settings/SettingsProvider.jsx'
import { THEMES } from '../theme/ThemeProvider.jsx'

// Settings — per-user preferences, saved to the account (not the browser) as soon as they are
// changed. Terminal mode applies to consoles opened from here on; existing sessions keep their
// place (right-click a session to dock/undock it).

const TERMINAL_MODES = [
  { id: 'docked', label: 'Docked', hint: 'Opens as a tab in the bottom terminal dock.' },
  { id: 'undocked', label: 'Undocked', hint: 'Opens in its own floating, movable window.' },
]

const DEPLOY_BACKENDS = [
  { id: 'docker', label: 'Docker', hint: 'Provisions every node as a Docker container on the local daemon.' },
  { id: 'vagrant', label: 'Vagrant (hybrid)', hint: 'Runs OS/DB nodes (PostgreSQL, MySQL/PXC, MongoDB, Valkey, ProxySQL, HAProxy) as VirtualBox VMs; the Intranet and image-only infra (PMM, Keycloak, etc.) stay on Docker in the same stack. Requires vagrant + VirtualBox on the host.' },
]

// Upload limits are entered in whole units rather than bytes — nobody types
// 4294967296. The server stores and enforces bytes; these are only the display.
const SIZE_UNITS = [
  { id: 'MiB', bytes: 1024 ** 2 },
  { id: 'GiB', bytes: 1024 ** 3 },
  { id: 'TiB', bytes: 1024 ** 4 },
]

// splitSize picks the largest unit the byte count divides into exactly, so a
// value round-trips through the form unchanged instead of drifting.
function splitSize(bytes) {
  for (const u of [...SIZE_UNITS].reverse()) {
    if (bytes >= u.bytes && bytes % u.bytes === 0) return { value: bytes / u.bytes, unit: u.id }
  }
  return { value: Math.max(1, Math.round(bytes / SIZE_UNITS[0].bytes)), unit: 'MiB' }
}

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

// UploadLimit is the one instance-wide setting on this page: the ceiling on a
// file drop onto a node (StackDesigner). Everyone sees it — you cannot work
// within a limit you cannot see — but only an admin can move it.
function UploadLimit() {
  const { system, saveSystem } = useSettings()
  const { user } = useAuth()
  const isAdmin = user?.role === 'admin'

  const [draft, setDraft] = useState(() => splitSize(system.maxUploadBytes))
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [saved, setSaved] = useState(false)

  // Re-seed once the server's value arrives (the provider starts on a default).
  useEffect(() => { setDraft(splitSize(system.maxUploadBytes)) }, [system.maxUploadBytes])

  const unit = SIZE_UNITS.find((u) => u.id === draft.unit) || SIZE_UNITS[1]
  const bytes = Math.round(draft.value * unit.bytes)
  const dirty = bytes !== system.maxUploadBytes

  const apply = async () => {
    setErr(''); setBusy(true); setSaved(false)
    try {
      await saveSystem({ maxUploadBytes: bytes })
      setSaved(true)
      setTimeout(() => setSaved(false), 2000)
    } catch (e) {
      setErr(e.message)
      setDraft(splitSize(system.maxUploadBytes))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Row
      title="Node file uploads"
      hint="The largest single drop of host files onto a deployed node, in the Stack Designer. Instance-wide — it applies to every user."
    >
      {/* inputCls carries w-full, so the width has to come from a wrapper —
          setting w-28 on the control itself loses to it in the stylesheet. */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="w-24">
          <input
            type="number" min="1" step="1" disabled={!isAdmin || busy}
            className={`${inputCls} disabled:cursor-not-allowed disabled:opacity-55`}
            value={draft.value}
            onChange={(e) => setDraft({ ...draft, value: Math.max(1, Number(e.target.value) || 1) })}
          />
        </div>
        <div className="w-24">
          <select
            disabled={!isAdmin || busy}
            className={`${inputCls} disabled:cursor-not-allowed disabled:opacity-55`}
            value={draft.unit}
            onChange={(e) => setDraft({ ...draft, unit: e.target.value })}
          >
            {SIZE_UNITS.map((u) => <option key={u.id} value={u.id}>{u.id}</option>)}
          </select>
        </div>
        {isAdmin && (
          <Button onClick={apply} disabled={!dirty || busy}>{busy ? 'Saving…' : 'Save'}</Button>
        )}
        {saved && <span className="flex items-center gap-1 text-xs text-success"><Icon.Check size={14} /> Saved</span>}
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
      <div className="text-xs text-muted">
        {isAdmin
          ? 'Default 4 GiB. Between 1 MiB and 1 TiB — the upload streams to the node rather than being held in memory, so the practical bound is temporary disk space, not RAM.'
          : 'Set by an administrator. Ask one to raise it if you need to copy something larger.'}
      </div>
    </Row>
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

      <Row title="Deployment" hint="How a stack's nodes are provisioned when you deploy. Applies to the next deploy of each stack.">
        <div className="grid gap-2 sm:grid-cols-2">
          {DEPLOY_BACKENDS.map((m) => {
            const on = settings.deploymentBackend === m.id
            return (
              <button key={m.id} onClick={() => set({ deploymentBackend: m.id })}
                className={`flex items-start gap-2.5 rounded-lg border p-3 text-left transition ${on ? 'border-primary bg-primary/10' : 'hover:bg-surface2'}`}>
                <span className={`mt-0.5 ${on ? 'text-primary' : 'text-muted'}`}>
                  <Icon.Server size={16} />
                </span>
                <span className="min-w-0">
                  <span className="flex items-center gap-1.5 text-sm font-medium">
                    {m.label}
                    {m.id === 'docker' && <span className="text-[10px] font-normal text-muted">(default)</span>}
                    {on && <Icon.Check size={14} />}
                  </span>
                  <span className="block text-xs text-muted">{m.hint}</span>
                </span>
              </button>
            )
          })}
        </div>
      </Row>

      <UploadLimit />

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
