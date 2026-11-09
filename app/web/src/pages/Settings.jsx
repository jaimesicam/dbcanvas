import { useEffect, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Button, Field, Toggle, inputCls } from '../components/ui.jsx'
import { Help } from '../components/Tooltip.jsx'
import { HELP } from '../lib/help.js'
import { useAuth } from '../auth/AuthProvider.jsx'
import { api } from '../lib/api.js'
import { useSettings } from '../settings/SettingsProvider.jsx'
import { LOOKS, THEMES } from '../theme/ThemeProvider.jsx'
import { TABS_DEFAULT, TABS_MAX, TABS_MIN, clampTabs, hashPage, hashSection } from '../lib/tabs.js'

// Settings — per-user preferences, saved to the account (not the browser) as soon as they are
// changed. Terminal mode applies to consoles opened from here on; existing sessions keep their
// place (right-click a session to dock/undock it).

const TERMINAL_MODES = [
  { id: 'docked', label: 'Docked', hint: 'Opens as a tab in the bottom terminal dock.' },
  { id: 'undocked', label: 'Undocked', hint: 'Opens in its own floating, movable window.' },
]

// Terminals stay in front by default: a console you are typing into should not vanish
// because you clicked a page or a file manager to read something off it. Exported for
// the render checks.
export const TERMINAL_ON_TOP_MODES = [
  { id: 'on', label: 'Keep terminals and file managers on top', hint: 'Terminal windows, the terminal dock and file managers stay in front of every other window, even while another one is selected. Minimize one (or hide the dock) to get it out of the way.' },
  { id: 'off', label: 'Stack normally', hint: 'Whichever window you touch comes to the front, terminals and file managers included.' },
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

// ChangePassword — your own password, on the Profile page (pages/Profile.jsx).
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
      hint="The longest expiry anyone may give an API token, administrators included. Only an administrator can create a token that never expires. Instance-wide."
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

// InternalWrites unlocks the databases the Database Explorer otherwise exposes
// read-only: PMM Server's own PostgreSQL and its Query Analytics ClickHouse.
//
// It is here rather than in the Explorer because of what it is — a decision about the
// whole installation, made once and knowingly — and it is deliberately not the only
// lock: a query tab still has to be armed for writes before one is sent. What the
// setting buys is that "what happens to PMM when its inventory is wrong" becomes a
// scenario you can test at all, without it ever becoming one you reach by accident.
export function InternalWrites() {
  const { system, saveSystem } = useSettings()
  const { user } = useAuth()
  const isAdmin = user?.role === 'admin'
  const on = !!system.internalWrites

  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [armed, setArmed] = useState(false)

  useEffect(() => {
    if (!armed) return undefined
    const t = setTimeout(() => setArmed(false), 4000)
    return () => clearTimeout(t)
  }, [armed])

  const apply = async (next) => {
    setErr(''); setBusy(true)
    try {
      await saveSystem({ internalWrites: next })
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy(false)
      setArmed(false)
    }
  }

  return (
    <Row
      title="Writes to internal databases"
      hint="Whether the Database Explorer may write to PMM Server's own PostgreSQL and ClickHouse. Instance-wide, and off by default."
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium ${
          on ? 'bg-danger/15 text-danger' : 'bg-muted/15 text-muted'}`}>
          {on ? <Icon.StatusWarn size={13} /> : <Icon.Check size={13} />}
          {on ? 'Writes allowed' : 'Read only'}
        </span>
        <Help text={HELP.internalWrites} />
        {isAdmin && (on
          ? <Button variant="subtle" onClick={() => apply(false)} disabled={busy}>{busy ? 'Saving…' : 'Make read-only again'}</Button>
          : armed
            ? <Button variant="danger" onClick={() => apply(true)} disabled={busy}>{busy ? 'Saving…' : 'Yes — allow writes'}</Button>
            : <Button variant="outline" onClick={() => setArmed(true)} disabled={busy}>Allow writes…</Button>)}
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
      <div className={`text-xs ${on ? 'text-danger' : 'text-muted'}`}>
        {on
          ? 'PMM\u2019s internal databases can be written to from the Database Explorer, once a query tab is armed for it. Modifying PMM internal data may corrupt or break the PMM installation \u2014 which is the point when you are testing that, and a hazard the rest of the time.'
          : isAdmin
            ? 'PMM\u2019s internal PostgreSQL and ClickHouse are exposed for inspection only, and the databases themselves refuse writes on those sessions. Turn this on to test what a broken PMM inventory does; turn it off again afterwards.'
            : 'Set by an administrator. PMM\u2019s internal databases are read-only for you.'}
      </div>
    </Row>
  )
}

// OpenRegistration is whether a new sign-up can use its account at once (app/auth.go),
// instead of waiting for an administrator to approve it. Instance-wide and off by default.
export function OpenRegistration() {
  const { system, saveSystem } = useSettings()
  const { user } = useAuth()
  const isAdmin = user?.role === 'admin'
  const on = !!system.openRegistration
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  const apply = async (next) => {
    setErr(''); setBusy(true)
    try {
      await saveSystem({ openRegistration: next })
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Row
      title="Registration"
      hint="Whether an account somebody registers can be used at once, or waits for an administrator to approve it. Instance-wide, and approval is the default."
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium ${
          on ? 'bg-warning/15 text-warning' : 'bg-muted/15 text-muted'}`}>
          {on ? <Icon.StatusWarn size={13} /> : <Icon.Check size={13} />}
          {on ? 'Open: no approval needed' : 'Approval required'}
        </span>
        <Help text={HELP.openRegistration} />
        {isAdmin && (
          <Button variant={on ? 'subtle' : 'outline'} onClick={() => apply(!on)} disabled={busy}>
            {busy ? 'Saving…' : on ? 'Require approval' : 'Open registration'}
          </Button>
        )}
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
      <div className="text-xs text-muted">
        {on
          ? 'Anyone who can reach the sign-in page can create an account and use it straight away, as a user (not an administrator). Administrators are notified of each one.'
          : isAdmin
            ? 'A new account waits under Users until an administrator approves it.'
            : 'Set by an administrator.'}
      </div>
    </Row>
  )
}

// GuestLogin is whether the sign-in page offers "Continue as guest" (app/guestlogin.go): a guest
// account of its own for each guest, with the user role. Instance-wide and off by default. Also
// whether turning it off keeps existing guests, and how long an unused guest account is kept.
export function GuestLogin() {
  const { system, saveSystem } = useSettings()
  const { user } = useAuth()
  const isAdmin = user?.role === 'admin'
  const on = !!system.guestLogin
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  const apply = async (next) => {
    setErr(''); setBusy(true)
    try {
      await saveSystem(typeof next === 'object' ? next : { guestLogin: next })
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Row
      title="Guest sign-in"
      hint="Whether people can use DBCanvas without an account, each as a guest account of their own. Instance-wide, and off by default."
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium ${
          on ? 'bg-warning/15 text-warning' : 'bg-muted/15 text-muted'}`}>
          {on ? <Icon.StatusWarn size={13} /> : <Icon.Close size={13} />}
          {on ? 'Guests can sign in' : 'Off'}
        </span>
        <Help text={HELP.guestLogin} />
        {isAdmin && (
          <Button variant={on ? 'subtle' : 'outline'} onClick={() => apply(!on)} disabled={busy}>
            {busy ? 'Saving…' : on ? (system.guestKeepAccess ? 'Turn off (existing guests stay)' : 'Turn off (signs guests out)') : 'Allow guest sign-in'}
          </Button>
        )}
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
      <div className="text-xs text-muted">
        {on
          ? 'Anyone who can reach the sign-in page can press \u201cContinue as guest\u201d and use DBCanvas as a regular user, without a password. Each guest gets an account of its own, and its stacks are its own.'
          : isAdmin
            ? 'Only people with an account can sign in.'
            : 'Set by an administrator.'}
      </div>
      {isAdmin && system.exposed && (
        // app/exposure.go: reachable beyond this machine, a guest is anybody on the network, and
        // a guest can deploy privileged containers on this Docker host.
        <div className="space-y-1.5 rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">
          <div>
            DBCanvas is published on <span className="font-mono">{system.publishHost}</span>, so it is reachable beyond this
            machine. Anyone who can reach the sign-in page could continue as a guest, and a guest can deploy privileged
            containers on this Docker host — in effect root on it. Prefer <span className="font-mono">APP_HOST=127.0.0.1</span> and
            an SSH tunnel for people who need it.
          </div>
          <label className="flex items-center gap-2 text-fg">
            <input type="checkbox" checked={!!system.guestExposedAck} disabled={busy}
              onChange={(e) => apply({ guestExposedAck: e.target.checked })} />
            <span>I understand: allow guest sign-in on this exposed installation</span>
          </label>
        </div>
      )}
      {isAdmin && (
        <div className="flex flex-wrap items-center gap-4 text-sm">
          <label className="flex items-center gap-2">
            <input type="checkbox" checked={!!system.guestKeepAccess} disabled={busy}
              onChange={(e) => apply({ guestKeepAccess: e.target.checked })} />
            <span>Keep existing guests when turning it off</span>
          </label>
          <label className="flex items-center gap-2">
            <span>Remove guest accounts unused for</span>
            <input type="number" min="0" max="3650" className={`${inputCls} w-20`} disabled={busy}
              defaultValue={system.guestIdleDays ?? 7} key={system.guestIdleDays ?? 7}
              onBlur={(e) => Number(e.target.value) !== (system.guestIdleDays ?? 7) && apply({ guestIdleDays: Math.max(0, Number(e.target.value) || 0) })} />
            <span>days (0 keeps them)</span>
          </label>
        </div>
      )}
    </Row>
  )
}

// SecurityGroups is the matrix of which pages each security group may use
// (app/secgroups.go): Administrators, Regular Users, Guests. Administrators only — the
// server enforces it on every API request, token and dbcanvas-cli included.
export function SecurityGroups() {
  const [view, setView] = useState(null) // { features, groups, allowed, locked }
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState('')
  const { refresh } = useAuth()

  useEffect(() => { api.securityGroups().then(setView, (e) => setErr(e.message)) }, [])

  const toggle = async (group, feature, next) => {
    setErr(''); setBusy(`${group}/${feature}`)
    try {
      setView(await api.saveSecurityGroups({ [group]: { [feature]: next } }))
      // An administrator who changed their own group sees the sidebar follow.
      if (group === 'admin') refresh()
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy('')
    }
  }

  return (
    <Row
      title="Security groups"
      hint="Which pages each group may use. Enforced by the server for the web UI, the API and dbcanvas-cli alike."
    >
      <div className="flex items-center gap-2 text-xs text-muted">
        <Help text={HELP.securityGroups} />
        <span>Administrators are accounts with the admin role, Regular Users the rest, and Guests every guest account.</span>
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
      {!view ? (!err && <div className="text-xs text-muted">Loading…</div>) : (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left text-xs text-muted">
                <th className="py-1.5 pr-2 font-medium">Page</th>
                {view.groups.map((g) => <th key={g.id} className="px-2 py-1.5 text-center font-medium">{g.label}</th>)}
              </tr>
            </thead>
            <tbody>
              {view.features.map((f) => (
                <tr key={f.id} className="border-t">
                  <td className="py-1.5 pr-2">
                    {f.label}
                    {f.requires && <span className="block text-xs text-muted">needs {view.features.find((x) => x.id === f.requires)?.label}</span>}
                  </td>
                  {view.groups.map((g) => {
                    const on = !!view.allowed[g.id]?.[f.id]
                    const locked = !!view.locked[g.id]?.[f.id]
                    return (
                      <td key={g.id} className="px-2 py-1.5 text-center">
                        <input
                          type="checkbox"
                          className="h-4 w-4 disabled:opacity-50"
                          aria-label={`${g.label}: ${f.label}`}
                          title={locked ? (g.id === 'admin' ? 'Administrators always keep this' : 'Only administrators can have this') : undefined}
                          checked={on}
                          disabled={locked || !!busy}
                          onChange={(e) => toggle(g.id, f.id, e.target.checked)}
                        />
                      </td>
                    )
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Row>
  )
}

// GroupLimits sets each security group's limits (app/grouppolicy.go): the longest a stack may
// live, how many stacks and deployed nodes one account may have, and node types it may not
// deploy. Empty or 0 is no limit. Saved per group on blur or change.
export function GroupLimits() {
  const [view, setView] = useState(null) // { policies: {group: policy}, ttls }
  const [err, setErr] = useState('')
  const [draft, setDraft] = useState({}) // group → denied types as typed

  useEffect(() => { api.groupPolicies().then(setView, (e) => setErr(e.message)) }, [])

  const save = async (group, patch) => {
    setErr('')
    try {
      setView(await api.saveGroupPolicies({ [group]: { ...view.policies[group], ...patch } }))
    } catch (e) {
      setErr(e.message)
    }
  }
  const groups = [['admin', 'Administrators'], ['user', 'Regular Users'], ['guest', 'Guests']]

  return (
    <Row title="Security group limits" hint="What each group's accounts may deploy. Empty or 0 means no limit.">
      <div className="flex items-center gap-2 text-xs text-muted">
        <Help text={HELP.groupLimits} />
        <span>Enforced by the server when a stack is created, composed, extended, validated and deployed.</span>
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
      {!view ? (!err && <div className="text-xs text-muted">Loading…</div>) : (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left text-xs text-muted">
                <th className="py-1.5 pr-2 font-medium">Group</th>
                <th className="px-2 py-1.5 font-medium">Longest lifetime</th>
                <th className="px-2 py-1.5 font-medium">Stacks</th>
                <th className="px-2 py-1.5 font-medium">Deployed nodes</th>
                <th className="px-2 py-1.5 font-medium">Node types it may not deploy</th>
              </tr>
            </thead>
            <tbody>
              {groups.map(([g, label]) => {
                const p = view.policies[g] || {}
                const denied = draft[g] ?? (p.deniedNodeTypes || []).join(', ')
                return (
                  <tr key={g} className="border-t">
                    <td className="py-1.5 pr-2">{label}</td>
                    <td className="px-2 py-1.5">
                      <select className={inputCls} aria-label={`${label}: longest lifetime`} value={p.maxTtl || ''}
                        onChange={(e) => save(g, { maxTtl: e.target.value })}>
                        <option value="">no limit</option>
                        {(view.ttls || []).map((t) => <option key={t} value={t}>{t}</option>)}
                      </select>
                    </td>
                    {['maxStacks', 'maxContainers'].map((k) => (
                      <td key={k} className="px-2 py-1.5">
                        <input type="number" min="0" className={`${inputCls} w-20`} aria-label={`${label}: ${k}`}
                          defaultValue={p[k] || 0} key={`${g}-${k}-${p[k] || 0}`}
                          onBlur={(e) => Number(e.target.value) !== (p[k] || 0) && save(g, { [k]: Math.max(0, Number(e.target.value) || 0) })} />
                      </td>
                    ))}
                    <td className="px-2 py-1.5">
                      <input className={inputCls} aria-label={`${label}: denied node types`} placeholder="none — e.g. k3d, kubeadm"
                        value={denied} onChange={(e) => setDraft((d) => ({ ...d, [g]: e.target.value }))}
                        onBlur={() => {
                          const list = denied.split(',').map((x) => x.trim()).filter(Boolean)
                          setDraft((d) => { const n = { ...d }; delete n[g]; return n })
                          if (list.join(',') !== (p.deniedNodeTypes || []).join(',')) save(g, { deniedNodeTypes: list })
                        }} />
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </Row>
  )
}

// GuestSessions is the switch for shared sessions (app/share.go): whether a stack's
// owner may share a live session through a link, and the longest a link may last.
// Instance-wide and off by default — a link admits people who have no account here.
export function GuestSessions() {
  const { system, saveSystem } = useSettings()
  const { user } = useAuth()
  const isAdmin = user?.role === 'admin'
  const on = !!system.allowGuestSessions
  const [minutes, setMinutes] = useState(system.maxGuestMinutes ?? 120)
  const [days, setDays] = useState(system.sessionRetentionDays ?? 90)
  const [recDays, setRecDays] = useState(system.recordingRetentionDays ?? 30)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  useEffect(() => { setMinutes(system.maxGuestMinutes ?? 120) }, [system.maxGuestMinutes])
  useEffect(() => { setDays(system.sessionRetentionDays ?? 90) }, [system.sessionRetentionDays])
  useEffect(() => { setRecDays(system.recordingRetentionDays ?? 30) }, [system.recordingRetentionDays])

  const apply = async (patch) => {
    setErr(''); setBusy(true)
    try { await saveSystem(patch) } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }
  const dirty = Number(minutes) !== system.maxGuestMinutes
  const daysDirty = Number(days) !== system.sessionRetentionDays
  const recDirty = Number(recDays) !== system.recordingRetentionDays

  return (
    <Row
      title="Shared sessions"
      hint="Whether a stack's owner may share a live session with guests through a link, and how long a link may last. Instance-wide, and off by default."
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium ${
          on ? 'bg-primary/15 text-primary' : 'bg-muted/15 text-muted'}`}>
          {on ? <Icon.Check size={13} /> : <Icon.Close size={13} />}
          {on ? 'Allowed' : 'Off'}
        </span>
        <Help text={HELP.guestSessions} />
        {isAdmin && (
          <Button variant={on ? 'subtle' : 'outline'} onClick={() => apply({ allowGuestSessions: !on })} disabled={busy}>
            {busy ? 'Saving…' : on ? 'Turn off (ends live sessions)' : 'Allow shared sessions'}
          </Button>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="text-muted">Longest link</span>
        <input
          type="number" min="5" max="120" step="5" disabled={!isAdmin || busy}
          value={minutes} onChange={(e) => setMinutes(e.target.value)}
          className="w-20 rounded-lg border bg-bg px-2 py-1 text-sm"
        />
        <span className="text-muted">minutes (5 to 120)</span>
        {isAdmin && dirty && (
          <Button variant="primary" onClick={() => apply({ maxGuestMinutes: Number(minutes) })} disabled={busy}>Save</Button>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="text-muted">Keep ended sessions</span>
        <input
          type="number" min="0" max="3650" step="1" disabled={!isAdmin || busy}
          value={days} onChange={(e) => setDays(e.target.value)}
          className="w-20 rounded-lg border bg-bg px-2 py-1 text-sm"
        />
        <span className="text-muted">days (0 keeps them)</span>
        <Help text={HELP.sessionRetention} />
        {isAdmin && daysDirty && (
          <Button variant="primary" onClick={() => apply({ sessionRetentionDays: Math.max(0, Math.round(Number(days) || 0)) })} disabled={busy}>Save</Button>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="text-muted">Purge recordings after</span>
        <input
          type="number" min="1" max="3650" step="1" disabled={!isAdmin || busy}
          value={recDays} onChange={(e) => setRecDays(e.target.value)}
          className="w-20 rounded-lg border bg-bg px-2 py-1 text-sm"
        />
        <span className="text-muted">days (the default; a host can move a recording&apos;s date)</span>
        <Help text={HELP.recordingRetention} />
        {isAdmin && recDirty && (
          <Button variant="primary" onClick={() => apply({ recordingRetentionDays: Math.max(1, Math.round(Number(recDays) || 30)) })} disabled={busy}>Save</Button>
        )}
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
      <div className="text-xs text-muted">
        {system.publicUrl
          ? <>Links are built on <span className="font-mono text-fg">{system.publicUrl}</span> (PUBLIC_URL).</>
          : 'Links use the address your browser is on. Set PUBLIC_URL in .env to the address colleagues use, and APP_HOST to an interface they can reach.'}
      </div>
    </Row>
  )
}

// LiveWatch is how often the server samples every deployed stack for its history, timeline and
// alerts (app/livewatch.go). Instance-wide: each pass execs into every database node.
export function LiveWatch() {
  const { system, saveSystem } = useSettings()
  const { user } = useAuth()
  const isAdmin = user?.role === 'admin'
  const [sec, setSec] = useState(system.liveWatchSeconds ?? 30)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  useEffect(() => { setSec(system.liveWatchSeconds ?? 30) }, [system.liveWatchSeconds])
  const apply = async (v) => {
    setErr(''); setBusy(true)
    try { await saveSystem({ liveWatchSeconds: v }) } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }
  const on = (system.liveWatchSeconds ?? 30) > 0
  return (
    <Row
      title="Stack history and alerts"
      hint="How often the server samples every deployed stack: the trend lines of the last day, the timeline of role changes and failovers, and the alerts in the notification bell. Off means none of these are recorded."
    >
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium ${on ? 'bg-primary/15 text-primary' : 'bg-muted/15 text-muted'}`}>
          {on ? `Every ${system.liveWatchSeconds}s` : 'Off'}
        </span>
        <span className="text-muted">Sample every</span>
        <input type="number" min="0" max="600" step="5" disabled={!isAdmin || busy}
          value={sec} onChange={(e) => setSec(e.target.value)}
          className="w-20 rounded-lg border bg-bg px-2 py-1 text-sm" />
        <span className="text-muted">seconds (15 to 600; 0 switches it off)</span>
        {isAdmin && Number(sec) !== system.liveWatchSeconds && (
          <Button variant="primary" onClick={() => apply(Math.max(0, Math.round(Number(sec) || 0)))} disabled={busy}>Save</Button>
        )}
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
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

// TerminalOnTopOptions is the Terminal stacking choice, exported for the render checks.
export function TerminalOnTopOptions({ value, onPick }) {
  return (
    <div className="grid gap-2 sm:grid-cols-2">
      {TERMINAL_ON_TOP_MODES.map((m) => {
        const on = value === m.id
        return (
          <button key={m.id} onClick={() => onPick(m.id)} aria-pressed={on}
            className={`flex items-start gap-2.5 rounded-lg border p-3 text-left transition ${on ? 'border-primary bg-primary/10' : 'hover:bg-surface2'}`}>
            <span className={`mt-0.5 ${on ? 'text-primary' : 'text-muted'}`}>
              <Icon.Terminal size={16} />
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
  )
}

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

// Keeping Scratchboard boards is off by default (app/scratchboard.go): what gets pasted there is
// often a certificate, a Secret or a connection string. Switching it off deletes every board the
// account kept, so that choice takes a second click.
export const SCRATCHBOARD_MODES = [
  { id: 'off', label: 'Temporary only', hint: 'Every board lives in this browser tab and is gone when it reloads. Nothing is sent to the server.' },
  { id: 'on', label: 'Allow keeping boards', hint: 'Pin a board on the Scratchboard to keep it: it is saved to your account, encrypted, in the DBCanvas database. Unpinned boards stay temporary.' },
]

// ScratchboardOptions is the Scratchboard row, exported for the render checks.
export function ScratchboardOptions({ value, onPick }) {
  const [armed, setArmed] = useState(false)
  useEffect(() => {
    if (!armed) return
    const t = setTimeout(() => setArmed(false), 4000)
    return () => clearTimeout(t)
  }, [armed])
  const pick = (id) => {
    if (id === value) return
    if (id === 'off' && !armed) { setArmed(true); return }
    setArmed(false)
    onPick(id)
  }
  return (
    <Row title="Scratchboard" hint="Whether Scratchboard boards can be kept across reloads.">
      <div className="grid gap-2 sm:grid-cols-2">
        {SCRATCHBOARD_MODES.map((m) => {
          const on = value === m.id
          const confirm = armed && m.id === 'off'
          return (
            <button key={m.id} onClick={() => pick(m.id)} aria-pressed={on}
              className={`flex items-start gap-2.5 rounded-lg border p-3 text-left transition ${confirm ? 'border-danger bg-danger/10' : on ? 'border-primary bg-primary/10' : 'hover:bg-surface2'}`}>
              <span className={`mt-0.5 ${on ? 'text-primary' : 'text-muted'}`}>
                {m.id === 'on' ? <Icon.Pin size={16} /> : <Icon.Pencil size={16} />}
              </span>
              <span className="min-w-0">
                <span className="flex items-center gap-1.5 text-sm font-medium">
                  {m.label}
                  {m.id === 'off' && <span className="text-[10px] font-normal text-muted">(default)</span>}
                  {on && <Icon.Check size={14} />}
                </span>
                <span className={`block text-xs ${confirm ? 'text-danger' : 'text-muted'}`}>
                  {confirm ? 'Click again to switch it off and delete every board you kept. This cannot be undone.' : m.hint}
                </span>
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
  const { user } = useAuth()
  const isAdmin = user?.role === 'admin'
  const [err, setErr] = useState('')
  // Four tabs instead of one long page: how it looks, how you work, who can get in, and what
  // the installation allows. The URL names the tab (#settings/access), so a link opens it; with
  // none, the tab last used in this browser.
  const tabs = [
    ['appearance', 'Appearance'],
    ['workspace', 'Workspace'],
    ['access', 'Access & security'],
    ['system', 'System'],
  ]
  const known = (id) => tabs.some(([t]) => t === id)
  const [tab, setTab] = useState(() => {
    if (hashPage() === 'settings' && known(hashSection())) return hashSection()
    try {
      const v = localStorage.getItem('dbcanvas.settingsTab')
      if (known(v)) return v
    } catch { /* not kept */ }
    return 'appearance'
  })
  const pickTab = (id) => {
    setTab(id)
    try { localStorage.setItem('dbcanvas.settingsTab', id) } catch { /* not kept */ }
    // replaceState, not location.hash: it fires no hashchange, so the shell does not treat it
    // as navigation, and the back button is not filled with tab clicks.
    if (hashPage() === 'settings') history.replaceState(null, '', `#settings/${id}`)
  }
  // A link to another tab while Settings is already open.
  useEffect(() => {
    const onHash = () => {
      if (hashPage() === 'settings' && known(hashSection())) setTab(hashSection())
    }
    addEventListener('hashchange', onHash)
    return () => removeEventListener('hashchange', onHash)
  }, [])
  const two = 'grid items-start gap-4 lg:grid-cols-2'

  return (
    <div className="max-w-6xl space-y-4">
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
      {!loaded && <div className="text-xs text-muted">Loading your settings…</div>}

      <div className="flex gap-1 overflow-x-auto border-b text-sm" role="tablist" aria-label="Settings">
        {tabs.map(([id, text]) => (
          <button key={id} type="button" role="tab" aria-selected={tab === id} onClick={() => pickTab(id)}
            className={`whitespace-nowrap rounded-t-md px-3 py-1.5 ${tab === id ? 'border border-b-0 bg-bg font-semibold text-fg' : 'text-muted hover:text-fg'}`}>
            {text}
          </button>
        ))}
      </div>

      {tab === 'appearance' && (
        <div className={two}>
          <div className="space-y-4">
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
            <TooltipOptions value={settings.tooltips || 'on'} onPick={(id) => set({ tooltips: id })} />
          </div>

          <Row
            title="Look"
            hint="The shape of the UI — corners, border weight, elevation and how tightly rows are packed. Independent of the theme above: every look works in every colour."
          >
            <LookOptions value={settings.look || 'glass'} onPick={(id) => set({ look: id })} />
          </Row>
        </div>
      )}

      {tab === 'workspace' && (
        <div className={two}>
          <div className="space-y-4">
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

          <Row title="Terminal stacking" hint="Whether terminals and file managers stay in front when you select another window.">
            <TerminalOnTopOptions value={settings.terminalsOnTop || 'on'} onPick={(id) => set({ terminalsOnTop: id })} />
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

          </div>
          <div className="space-y-4">
          <TabLimit value={clampTabs(settings.maxTabs)} onSave={(n) => save({ maxTabs: n })} />

          <ScratchboardOptions value={settings.scratchboard || 'off'} onPick={(id) => set({ scratchboard: id })} />

          <Row title="Password" hint="Your password, name and avatar are on your Profile now.">
            <Button variant="outline" size="sm" onClick={() => { location.hash = 'profile' }}>
              <Icon.Users size={15} /> Open your profile
            </Button>
          </Row>
          </div>
        </div>
      )}

      {tab === 'access' && (
        <div className={two}>
          <div className="space-y-4">
            <OpenRegistration />
            <GuestLogin />
            <GuestSessions />
            <TokenLifetime />
          </div>
          <div className="space-y-4">
            {isAdmin && <SecurityGroups />}
            {isAdmin && <GroupLimits />}
          </div>
        </div>
      )}

      {tab === 'system' && (
        <div className={two}>
          <div className="space-y-4">
            <UploadLimit />
            <InternalWrites />
          </div>
          <div className="space-y-4">
            <LiveWatch />
          </div>
        </div>
      )}
    </div>
  )
}
