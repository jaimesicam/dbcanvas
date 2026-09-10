import { useEffect, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Button, Field, Toggle, inputCls } from '../components/ui.jsx'
import { Help } from '../components/Tooltip.jsx'
import { HELP } from '../lib/help.js'
import { useAuth } from '../auth/AuthProvider.jsx'
import { api } from '../lib/api.js'
import { useSettings } from '../settings/SettingsProvider.jsx'
import { LOOKS, THEMES } from '../theme/ThemeProvider.jsx'
import { TABS_DEFAULT, TABS_MAX, TABS_MIN, clampTabs } from '../lib/tabs.js'

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

// Tooltips are on for everybody who has not said otherwise: DBCanvas asks for a lot of
// decisions before anything deploys, and the bubbles are where most of the answers live.
// The switch is for the other case — somebody who knows this app and would rather the
// "?" beside every label were not there.
//
// What it covers is this app's own explanations (components/Tooltip.jsx), which is where
// every one of them lives. A native `title` on an icon-only button is not one of those:
// it is that control's NAME, the only one it has, so it stays either way and the hint
// below says so rather than letting "Hidden" promise silence it cannot deliver.
export const TOOLTIP_MODES = [
  { id: 'on', label: 'Shown', hint: 'Hover or focus a "?" for what a control is for, what happens if you leave it alone, and when to change it.' },
  { id: 'off', label: 'Hidden', hint: 'No hover explanations, and the "?" buttons go with them. The short hint under a field stays, and so does the browser\'s own label on a button that is only an icon.' },
]

const NODE_LIBRARIES = [
  { id: 'menu', label: 'Canvas menu', hint: 'Right-click the canvas to add a node, and it lands where you clicked. Categories are the first level of the menu.' },
  { id: 'docked', label: 'Docked library', hint: 'Keeps the Infrastructure Library column on the left of the designer, with its search box and recently-used list. The canvas menu keeps working either way.' },
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

// TabLimit is how many pages the main window will keep open at once.
//
// A draft plus an explicit Save, like the two instance-wide rows below, rather
// than the instant apply the pick-one rows use: a number field commits a value on
// every keystroke, so typing 12 would save 1 first — and a 1 the server clamps to
// the floor is a worse answer than the one still being typed.
export function TabLimit({ value, onSave }) {
  const [draft, setDraft] = useState(value)
  const [busy, setBusy] = useState(false)
  const [saved, setSaved] = useState(false)
  const [err, setErr] = useState('')

  // Re-seed when the account's value arrives (the provider starts on a default).
  useEffect(() => { setDraft(value) }, [value])

  const next = clampTabs(draft)
  const dirty = next !== value

  // The save is taken raw rather than through the page's set(), which swallows the
  // error into the banner at the top: a row with its own Save button should report
  // its own failure, and not claim "Saved" for a write that did not land.
  const apply = async () => {
    setErr(''); setBusy(true); setSaved(false)
    try {
      await onSave(next)
      setDraft(next)
      setSaved(true)
      setTimeout(() => setSaved(false), 2000)
    } catch (e) {
      setErr(e.message)
      setDraft(value)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Row
      title="Tabs"
      hint="How many pages the main window keeps open at once. Applies to the next page you open."
    >
      <div className="flex flex-wrap items-center gap-2">
        {/* inputCls carries w-full, so the width has to come from a wrapper. */}
        <div className="w-24">
          <input
            type="number" min={TABS_MIN} max={TABS_MAX} step="1" disabled={busy}
            className={`${inputCls} disabled:cursor-not-allowed disabled:opacity-55`}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onBlur={() => setDraft(next)}
          />
        </div>
        <span className="text-sm text-muted">tabs</span>
        <Help text={HELP.maxTabs} />
        <Button onClick={apply} disabled={!dirty || busy}>{busy ? 'Saving…' : 'Save'}</Button>
        {saved && <span className="flex items-center gap-1 text-xs text-success"><Icon.Check size={14} /> Saved</span>}
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
      <div className="text-xs text-muted">
        Default {TABS_DEFAULT}, between {TABS_MIN} and {TABS_MAX}. Every open tab stays mounted so it
        keeps its scroll position, its filters and whatever it was in the middle of — which is why
        there is a ceiling at all: it is a memory budget, not a preference. At the limit a page
        simply does not open, and the main window says so rather than evicting a tab that might be
        running something. Lowering it never closes a tab you already have open.
      </div>
    </Row>
  )
}

// LookPreview renders a card, a field and a button in the look it is offering.
// The `data-look` re-points every shape token its subtree reads (index.css), so
// this is not a picture of the look — it is the app's own primitives at small
// size, and it cannot fall out of date with them.
function LookPreview({ look, on }) {
  return (
    <div data-look={look.id} className="rounded-xl bg-bg p-2.5">
      {/* A card on the app's background rather than a bare panel, because half of
          what separates the looks is how a card sits on the page. The mock parts
          wear the same ui-* hooks the real primitives do (components/ui.jsx), so
          the preview picks up the look's chrome — a title bar, a ruled heading, a
          pill button — and not only its radius. */}
      <div className="ui-card rounded-lg border bg-surface shadow-lg">
        <div className="ui-card-head flex items-center justify-between gap-2 border-b px-2.5 py-1.5">
          {/* The look's name IS the heading, so its display face shows on a real
              word rather than on a specimen nobody would read. */}
          <h3 className="text-xs font-semibold text-fg">{look.label}</h3>
          {on && <Icon.Check size={14} />}
        </div>
        <div className="space-y-1.5 p-2.5">
          <div className="ui-input rounded-md border bg-bg px-2 py-1">
            <div className="h-1 w-2/3 rounded-full bg-muted/40" />
          </div>
          <div className="flex items-center gap-1.5">
            <span className="ui-badge rounded-full bg-success/15 px-2 py-0.5 text-[9px] font-medium text-success">running</span>
            <span className="ui-btn ml-auto rounded-lg bg-primary px-2 py-1 text-[9px] font-medium text-primary-fg">Deploy</span>
            <span className="ui-btn rounded-lg border px-2 py-1 text-[9px] text-muted">Cancel</span>
          </div>
        </div>
      </div>
    </div>
  )
}

// LookOptions is the look grid. Exported for the render smoke test, which is the
// only thing that renders all four at once outside this page.
// TooltipOptions is the Tooltips row. Exported like LookOptions, and for the same
// reason: the page itself needs an authenticated session to render, so a row worth a
// render check has to be reachable without one.
export function TooltipOptions({ value, onPick }) {
  return (
    <Row title="Tooltips" hint="The hover explanations on labels, fields and menu rows, across the whole app.">
      <div className="grid gap-2 sm:grid-cols-2">
        {TOOLTIP_MODES.map((m) => {
          const on = value === m.id
          return (
            <button key={m.id} onClick={() => onPick(m.id)}
              className={`flex items-start gap-2.5 rounded-lg border p-3 text-left transition ${on ? 'border-primary bg-primary/10' : 'hover:bg-surface2'}`}>
              <span className={`mt-0.5 ${on ? 'text-primary' : 'text-muted'}`}>
                <Icon.Help size={16} />
              </span>
              <span className="min-w-0">
                <span className="flex items-center gap-1.5 text-sm font-medium">
                  {m.label}
                  {m.id === 'on' && <span className="text-[10px] font-normal text-muted">(default)</span>}
                  {on && <Icon.Check size={14} />}
                </span>
                <span className="block text-xs text-muted">{m.hint}</span>
              </span>
            </button>
          )
        })}
      </div>
    </Row>
  )
}

export function LookOptions({ value, onPick }) {
  return (
    <div className="grid gap-2 sm:grid-cols-2">
      {LOOKS.map((l) => {
        const on = value === l.id
        return (
          <button key={l.id} onClick={() => onPick(l.id)} aria-pressed={on}
            className={`rounded-lg border p-3 text-left transition ${on ? 'border-primary bg-primary/10' : 'hover:bg-surface2'}`}>
            <LookPreview look={l} on={on} />
            <span className="mt-2 block text-xs text-muted">{l.hint}</span>
          </button>
        )
      })}
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

      <Row title="Adding nodes" hint="How the Stack Designer offers the Infrastructure Library.">
        <div className="grid gap-2 sm:grid-cols-2">
          {NODE_LIBRARIES.map((m) => {
            const on = (settings.nodeLibrary || 'menu') === m.id
            return (
              <button key={m.id} onClick={() => set({ nodeLibrary: m.id })}
                className={`flex items-start gap-2.5 rounded-lg border p-3 text-left transition ${on ? 'border-primary bg-primary/10' : 'hover:bg-surface2'}`}>
                <span className={`mt-0.5 ${on ? 'text-primary' : 'text-muted'}`}>
                  <Icon.Grid size={16} />
                </span>
                <span className="min-w-0">
                  <span className="flex items-center gap-1.5 text-sm font-medium">
                    {m.label}
                    {m.id === 'menu' && <span className="text-[10px] font-normal text-muted">(default)</span>}
                    {on && <Icon.Check size={14} />}
                  </span>
                  <span className="block text-xs text-muted">{m.hint}</span>
                </span>
              </button>
            )
          })}
        </div>
      </Row>

      <TooltipOptions value={settings.tooltips || 'on'} onPick={(id) => set({ tooltips: id })} />

      <TabLimit value={clampTabs(settings.maxTabs)} onSave={(n) => save({ maxTabs: n })} />

      <ChangePassword />

      <UploadLimit />

      <TokenLifetime />

      <Row title="Theme" hint="The colour palette. Applied now and whenever you sign in, on any browser.">
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

      <Row
        title="Look"
        hint="The shape of the UI — corners, border weight, elevation and how tightly rows are packed. Independent of the theme above: every look works in every colour."
      >
        <LookOptions value={settings.look || 'modern'} onPick={(id) => set({ look: id })} />
      </Row>
    </div>
  )
}
