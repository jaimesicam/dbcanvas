import { useEffect, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Button, Field, Toggle, inputCls } from '../components/ui.jsx'
import { Help } from '../components/Tooltip.jsx'
import { HELP } from '../lib/help.js'
import { useAuth } from '../auth/AuthProvider.jsx'
import { api } from '../lib/api.js'
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

// ChangePassword — your own password, from Settings.
//
// The current password is required by the server even though the user is signed in,
// and the form says why: this is the check that stops a stolen session becoming a
// lockout. Token revocation is opt-in, because a routine rotation should not break a
// CI job while a leak-driven change should be able to take everything with it.
export function ChangePassword() {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [revokeTokens, setRevokeTokens] = useState(false)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [done, setDone] = useState('')

  // Checked here as well as on the server so the mismatch is caught before a round
  // trip; the server does not see the confirmation field at all.
  const mismatch = confirm !== '' && next !== confirm
  const tooShort = next !== '' && next.length < 8
  const ready = current !== '' && next.length >= 8 && next === confirm && !busy

  const submit = async (e) => {
    e.preventDefault()
    setErr(''); setDone(''); setBusy(true)
    try {
      const res = await api.changePassword(current, next, revokeTokens)
      setCurrent(''); setNext(''); setConfirm(''); setRevokeTokens(false)
      setDone(res?.tokensRevoked
        ? `Password changed. Other sessions signed out, and ${res.tokensRevoked} API token${res.tokensRevoked === 1 ? '' : 's'} revoked.`
        : 'Password changed. Every other session was signed out; your API tokens still work.')
    } catch (e2) {
      setErr(e2.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Row title="Password" hint="Changing it signs out every other browser and device. This one stays signed in.">
      <form onSubmit={submit} className="space-y-3">
        <div className="grid gap-3 sm:grid-cols-3">
          <Field label="Current password" help={HELP.currentPassword}>
            <input type="password" autoComplete="current-password" className={inputCls}
              value={current} onChange={(e) => setCurrent(e.target.value)} required />
          </Field>
          <Field label="New password" help={HELP.newPassword}
            hint={tooShort ? 'At least 8 characters.' : undefined}>
            <input type="password" autoComplete="new-password" className={inputCls}
              value={next} onChange={(e) => setNext(e.target.value)} required minLength={8} />
          </Field>
          <Field label="Confirm new password"
            hint={mismatch ? 'These do not match.' : undefined}>
            <input type="password" autoComplete="new-password" className={inputCls}
              value={confirm} onChange={(e) => setConfirm(e.target.value)} required />
          </Field>
        </div>

        <div className="flex items-center gap-1.5">
          <Toggle checked={revokeTokens} onChange={setRevokeTokens}
            label="Also revoke my API tokens" />
          <Help text={HELP.revokeTokensOnPasswordChange} />
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <Button type="submit" disabled={!ready}>{busy ? 'Changing…' : 'Change password'}</Button>
          <Help text={HELP.passwordSessions} />
        </div>

        {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
        {done && (
          <div className="flex items-start gap-1.5 rounded-lg border border-success/30 bg-success/15 px-3 py-2 text-xs text-success">
            <Icon.Check size={14} /> <span>{done}</span>
          </div>
        )}
        <div className="text-xs text-muted">
          Forgotten it entirely, with nobody able to sign in? An administrator runs{' '}
          <code className="rounded bg-surface2 px-1">dbcanvas_reset_password</code> inside the app
          container — see Configuration &amp; commands.
        </div>
      </form>
    </Row>
  )
}

// TokenLifetime is the other instance-wide setting: the longest expiry a
// non-administrator may give an API token. Same shape as UploadLimit — everyone can
// see the limit they are working under, only an admin can move it.
function TokenLifetime() {
  const { system, saveSystem } = useSettings()
  const { user } = useAuth()
  const isAdmin = user?.role === 'admin'

  const [draft, setDraft] = useState(system.maxTokenDays ?? 90)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [saved, setSaved] = useState(false)

  useEffect(() => { setDraft(system.maxTokenDays ?? 90) }, [system.maxTokenDays])

  const dirty = draft !== system.maxTokenDays

  const apply = async () => {
    setErr(''); setBusy(true); setSaved(false)
    try {
      await saveSystem({ maxTokenDays: draft })
      setSaved(true)
      setTimeout(() => setSaved(false), 2000)
    } catch (e) {
      setErr(e.message)
      setDraft(system.maxTokenDays ?? 90)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Row
      title="API token lifetime"
      hint="The longest expiry anyone who is not an administrator may give an API token. Instance-wide."
    >
      <div className="flex flex-wrap items-center gap-2">
        <div className="w-24">
          <input
            type="number" min="1" max="365" step="1" disabled={!isAdmin || busy}
            className={`${inputCls} disabled:cursor-not-allowed disabled:opacity-55`}
            value={draft}
            onChange={(e) => setDraft(Math.min(365, Math.max(1, Number(e.target.value) || 1)))}
          />
        </div>
        <span className="text-sm text-muted">days</span>
        <Help text={HELP.maxTokenDays} />
        {isAdmin && (
          <Button onClick={apply} disabled={!dirty || busy}>{busy ? 'Saving…' : 'Save'}</Button>
        )}
        {saved && <span className="flex items-center gap-1 text-xs text-success"><Icon.Check size={14} /> Saved</span>}
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
      <div className="text-xs text-muted">
        {isAdmin
          ? 'Default 90 days, between 1 and 365. A request for longer is shortened to this rather than refused, so lowering it never breaks the create form — and it does not shorten tokens that already exist. Only an administrator can create a token that never expires.'
          : 'Set by an administrator. Tokens you create are capped at this, whatever you ask for.'}
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

      <ChangePassword />

      <UploadLimit />

      <TokenLifetime />

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
