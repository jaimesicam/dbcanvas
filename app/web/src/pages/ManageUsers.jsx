import { useCallback, useEffect, useMemo, useState } from 'react'
import { createPortal } from 'react-dom'
import { api } from '../lib/api.js'
import { useAuth } from '../auth/AuthProvider.jsx'
import { Card, Button, Badge, Field, inputCls } from '../components/ui.jsx'
import { useDialog } from '../components/Dialog.jsx'
import { Icon } from '../components/Icons.jsx'
import { useRefresh } from '../lib/useRefresh.jsx'
import { Avatar, fullName, randomAvatar } from '../components/Avatar.jsx'
import { ProfileFields, profileComplete } from '../components/ProfileFields.jsx'

// The filters sit on one line: inputCls is full-width, which a filter must not be.
const selectCls = inputCls.replace('w-full', 'w-auto')

const STATUS_TONE = { approved: 'success', pending: 'warning', rejected: 'danger', disabled: 'muted' }

const SORTS = {
  default: { label: 'Pending first, newest', fn: null },
  name: { label: 'Name', fn: (a, b) => sortName(a).localeCompare(sortName(b)) },
  username: { label: 'Username', fn: (a, b) => a.username.localeCompare(b.username) },
  lastSeen: { label: 'Last active', fn: (a, b) => (lastActive(b) || '').localeCompare(lastActive(a) || '') },
  registered: { label: 'Registered', fn: (a, b) => (b.createdAt || '').localeCompare(a.createdAt || '') },
}

const sortName = (u) => (u.firstName || u.lastName ? `${u.lastName} ${u.firstName}` : u.username).toLowerCase()
// The latest sign of life: a session used, or else the last sign-in.
const lastActive = (u) => [u.lastSeenAt, u.lastLoginAt].filter(Boolean).sort().pop() || ''

function fmtDate(iso) {
  if (!iso) return '—'
  const d = new Date(iso)
  if (isNaN(d)) return '—'
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

function fmtAgo(iso) {
  if (!iso) return ''
  const d = new Date(iso)
  if (isNaN(d)) return ''
  const s = (Date.now() - d.getTime()) / 1000
  if (s < 90) return 'just now'
  if (s < 3600) return `${Math.round(s / 60)} min ago`
  if (s < 86400) return `${Math.round(s / 3600)} h ago`
  if (s < 30 * 86400) return `${Math.round(s / 86400)} d ago`
  return fmtDate(iso)
}

// A user agent, as a person would say it: "Firefox on macOS".
function describeAgent(ua) {
  if (!ua) return 'Not recorded'
  if (/dbcanvas-cli|curl|Go-http-client/i.test(ua)) return ua.split(' ')[0]
  const browser = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : 'Browser'
  const os = /Windows/.test(ua) ? 'Windows' : /Mac OS X|Macintosh/.test(ua) ? 'macOS' : /Android/.test(ua) ? 'Android' : /iPhone|iPad/.test(ua) ? 'iOS' : /Linux/.test(ua) ? 'Linux' : ''
  return os ? `${browser} on ${os}` : browser
}

export default function ManageUsers() {
  const { user: me } = useAuth()
  const [users, setUsers] = useState([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [busyId, setBusyId] = useState(null)
  const [query, setQuery] = useState('')
  const [status, setStatus] = useState('all')
  const [role, setRole] = useState('all')
  const [sort, setSort] = useState('default')
  // Which dialog is open: { kind: 'create' | 'edit' | 'reset' | 'sessions', user }.
  const [open, setOpen] = useState(null)
  const [dialog, ask] = useDialog()

  const load = useCallback(async () => {
    setError('')
    try {
      const list = await api.listUsers()
      setUsers(list)
    } catch (err) {
      setError(err.message)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])
  useRefresh(load)

  async function act(fn, id) {
    setBusyId(id)
    setError('')
    try {
      await fn()
      await load()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusyId(null)
    }
  }

  const changeStatus = (id, action) => act(() => api.setUserStatus(id, action), id)

  async function remove(u) {
    const ok = await ask.confirm({
      title: `Delete ${u.username}?`,
      body: 'The account is deleted together with its stacks, sessions and templates. This cannot be undone.',
      confirmLabel: 'Delete account',
      danger: true,
    })
    if (ok) act(() => api.deleteUser(u.id), u.id)
  }

  async function changeRole(u, next) {
    const promote = next === 'admin'
    const ok = await ask.confirm({
      title: promote ? `Make ${u.username} an administrator?` : `Make ${u.username} a regular user?`,
      body: promote
        ? 'They will be able to approve, disable and delete accounts, reset passwords, and change instance settings.'
        : 'They lose administrator access on their next request; their sessions stay signed in.',
      confirmLabel: promote ? 'Make administrator' : 'Make regular user',
      danger: !promote,
    })
    if (ok) act(() => api.setUserRole(u.id, next), u.id)
  }

  async function signOutEveryone() {
    const ok = await ask.confirm({
      title: 'Sign everyone out?',
      body: 'Ends every session on this instance except this one, administrators included. Passwords and API tokens are not changed.',
      confirmLabel: 'Sign everyone out',
      danger: true,
    })
    if (ok) act(() => api.endAllSessions(), 'all')
  }

  async function clearAllHistory() {
    const ok = await ask.confirm({
      title: 'Clear everyone\'s sign-in history?',
      body: 'Forgets every account\'s last sign-in, and each session\'s address, browser and times. Nobody is signed out; activity from now on is recorded again.',
      confirmLabel: 'Clear history',
      danger: true,
    })
    if (ok) act(() => api.clearAllSignInHistory(), 'all')
  }

  const shown = useMemo(() => {
    const q = query.trim().toLowerCase()
    const list = users.filter((u) =>
      (status === 'all' || u.status === status) &&
      (role === 'all' || u.role === role) &&
      (!q || [u.username, u.firstName, u.lastName, u.email, `${u.firstName} ${u.lastName}`]
        .some((v) => v && v.toLowerCase().includes(q))))
    const fn = SORTS[sort]?.fn
    return fn ? [...list].sort(fn) : list
  }, [users, query, status, role, sort])

  const pendingCount = users.filter((u) => u.status === 'pending').length
  const filtered = shown.length !== users.length
  const close = () => setOpen(null)
  const closeAndReload = () => { setOpen(null); load() }

  return (
    <Card
      title="Manage users"
      subtitle={`${users.length} accounts · ${pendingCount} awaiting approval`}
      action={
        <div className="flex gap-1.5">
          <Button size="sm" onClick={() => setOpen({ kind: 'create' })}>
            <Icon.Plus size={14} /> <span className="ml-1">Add user</span>
          </Button>
          <Button variant="outline" size="sm" disabled={busyId === 'all'} onClick={signOutEveryone}>
            Sign out everyone
          </Button>
          <Button variant="outline" size="sm" disabled={busyId === 'all'} onClick={clearAllHistory}>
            Clear sign-in history
          </Button>
          <Button variant="outline" size="sm" onClick={load}>
            Refresh
          </Button>
        </div>
      }
    >
      {error && <div className="mb-3 rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{error}</div>}

      <div className="mb-3 flex flex-wrap items-center gap-2">
        <div className="relative min-w-[12rem] flex-1">
          <span className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-muted"><Icon.Search size={14} /></span>
          <input className={`${inputCls} pl-8`} placeholder="Search name, username or email" value={query}
            onChange={(e) => setQuery(e.target.value)} aria-label="Search users" />
        </div>
        <select className={selectCls} value={status} onChange={(e) => setStatus(e.target.value)} aria-label="Filter by status">
          <option value="all">All statuses</option>
          <option value="pending">Pending</option>
          <option value="approved">Approved</option>
          <option value="disabled">Disabled</option>
          <option value="rejected">Rejected</option>
        </select>
        <select className={selectCls} value={role} onChange={(e) => setRole(e.target.value)} aria-label="Filter by role">
          <option value="all">All roles</option>
          <option value="admin">Administrators</option>
          <option value="user">Users</option>
        </select>
        <select className={selectCls} value={sort} onChange={(e) => setSort(e.target.value)} aria-label="Sort by">
          {Object.entries(SORTS).map(([id, s]) => <option key={id} value={id}>Sort: {s.label}</option>)}
        </select>
      </div>

      {loading ? (
        <div className="py-10 text-center text-muted">Loading…</div>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="border-b text-xs text-muted">
              <tr>
                <th className="px-3 py-2 text-left font-medium">User</th>
                <th className="px-3 py-2 text-left font-medium">Role</th>
                <th className="px-3 py-2 text-left font-medium">Status</th>
                <th className="px-3 py-2 text-left font-medium">Last active</th>
                <th className="px-3 py-2 text-left font-medium">Registered</th>
                <th className="px-3 py-2 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((u) => {
                const isYou = u.id === me?.id
                const busy = busyId === u.id
                const active = lastActive(u)
                return (
                  <tr key={u.id} className={`border-b ${u.status === 'pending' ? 'bg-warning/5' : ''}`}>
                    <td className="px-3 py-2.5">
                      <div className="flex items-center gap-2.5">
                        <Avatar avatar={u.avatar} name={fullName(u)} size={32} />
                        <span className="min-w-0">
                          <span className="block font-medium text-fg">
                            {u.firstName || u.lastName ? fullName(u) : <span className="text-muted">No name yet</span>}
                            {isYou && <span className="ml-1 text-xs text-muted">(you)</span>}
                          </span>
                          <span className="block text-xs text-muted">{u.username}{u.email ? ` · ${u.email}` : ''}</span>
                        </span>
                      </div>
                    </td>
                    <td className="px-3 py-2.5">
                      <Badge tone={u.role === 'admin' ? 'primary' : 'muted'}>{u.role}</Badge>
                    </td>
                    <td className="px-3 py-2.5">
                      <div className="flex flex-wrap gap-1">
                        <Badge tone={STATUS_TONE[u.status]}>{u.status}</Badge>
                        {u.mustChangePassword && <Badge tone="warning">temporary password</Badge>}
                      </div>
                    </td>
                    <td className="px-3 py-2.5">
                      {active ? (
                        <span title={new Date(active).toLocaleString()}>{fmtAgo(active)}</span>
                      ) : (
                        <span className="text-muted">{u.invitePending ? 'Invite not used yet' : 'No sign-in recorded'}</span>
                      )}
                      {u.sessions > 0 && (
                        <button className="block text-xs text-primary hover:underline" onClick={() => setOpen({ kind: 'sessions', user: u })}>
                          {u.sessions} {u.sessions === 1 ? 'session' : 'sessions'}
                        </button>
                      )}
                    </td>
                    <td className="px-3 py-2.5 text-muted">{fmtDate(u.createdAt)}</td>
                    <td className="px-3 py-2.5">
                      <div className="flex items-center justify-end gap-1.5">
                        {u.status === 'pending' && (
                          <>
                            <Button size="sm" variant="primary" disabled={busy} onClick={() => changeStatus(u.id, 'approve')}>
                              Approve
                            </Button>
                            <Button size="sm" variant="outline" disabled={busy} onClick={() => changeStatus(u.id, 'reject')}>
                              Reject
                            </Button>
                          </>
                        )}
                        {u.status === 'approved' && !isYou && (
                          <Button size="sm" variant="outline" disabled={busy} onClick={() => changeStatus(u.id, 'disable')}>
                            Disable
                          </Button>
                        )}
                        {(u.status === 'disabled' || u.status === 'rejected') && (
                          <Button size="sm" variant="outline" disabled={busy} onClick={() => changeStatus(u.id, 'approve')}>
                            Re-approve
                          </Button>
                        )}
                        {!isYou && u.status === 'approved' && (
                          <Button size="sm" variant="outline" disabled={busy} onClick={() => changeRole(u, u.role === 'admin' ? 'user' : 'admin')}>
                            {u.role === 'admin' ? 'Make user' : 'Make admin'}
                          </Button>
                        )}
                        <Button size="sm" variant="ghost" disabled={busy} onClick={() => setOpen({ kind: 'edit', user: u })} title="Edit profile">
                          <Icon.Pencil size={15} />
                        </Button>
                        <Button size="sm" variant="ghost" disabled={busy} onClick={() => setOpen({ kind: 'sessions', user: u })} title="Sessions">
                          <Icon.Monitor size={16} />
                        </Button>
                        {!isYou && (
                          <Button size="sm" variant="ghost" disabled={busy} onClick={() => setOpen({ kind: 'reset', user: u })} title="Reset password">
                            <Icon.Key size={16} />
                          </Button>
                        )}
                        {!isYou && (
                          <Button size="sm" variant="ghost" disabled={busy} onClick={() => remove(u)} title="Delete user">
                            <Icon.Trash size={16} />
                          </Button>
                        )}
                      </div>
                    </td>
                  </tr>
                )
              })}
              {shown.length === 0 && (
                <tr>
                  <td colSpan={6} className="px-3 py-8 text-center text-muted">
                    {filtered ? 'No users match these filters' : 'No users'}
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      )}
      {open?.kind === 'create' && <CreateUserDialog onClose={closeAndReload} />}
      {open?.kind === 'edit' && <EditUserDialog user={open.user} onClose={close} onSaved={closeAndReload} />}
      {open?.kind === 'reset' && <ResetPasswordDialog user={open.user} onClose={closeAndReload} />}
      {open?.kind === 'sessions' && <SessionsDialog user={open.user} isYou={open.user.id === me?.id} onClose={closeAndReload} />}
      {dialog}
    </Card>
  )
}

// Modal is the frame every dialog on this page is drawn in, above everything (see
// components/Dialog.jsx for why the portal).
function Modal({ title, subtitle, icon, onClose, wide, children }) {
  useEffect(() => {
    const onKey = (e) => { if (e.key === 'Escape') { e.stopPropagation(); onClose() } }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [onClose])
  const box = (
    <div className="fixed inset-0 flex items-center justify-center bg-black/40 p-4 animate-fade-in" style={{ zIndex: 9500 }} onMouseDown={onClose}>
      <div role="dialog" aria-modal="true" aria-label={title}
        className={`max-h-full w-full overflow-y-auto rounded-2xl border bg-surface p-5 shadow-xl ${wide ? 'max-w-2xl' : 'max-w-md'}`}
        onMouseDown={(e) => e.stopPropagation()}>
        <div className="mb-4 flex items-start gap-3">
          <span className="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-primary/15 text-primary">{icon}</span>
          <div className="min-w-0 flex-1">
            <h2 className="text-base font-semibold">{title}</h2>
            {subtitle && <p className="mt-1 text-sm text-muted">{subtitle}</p>}
          </div>
        </div>
        {children}
      </div>
    </div>
  )
  return createPortal(box, document.body)
}

const ErrorBox = ({ children }) =>
  children ? <div className="mb-3 rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{children}</div> : null

function Tabs({ value, onChange, options }) {
  return (
    <div className="mb-4 grid grid-cols-2 gap-1 rounded-lg bg-surface2 p-1">
      {options.map(([id, label]) => (
        <button key={id} type="button" onClick={() => onChange(id)}
          className={`rounded-md py-1.5 text-sm font-medium transition ${value === id ? 'bg-surface text-fg shadow' : 'text-muted'}`}>
          {label}
        </button>
      ))}
    </div>
  )
}

// useBusy runs an action with a busy flag and the error captured for display.
function useBusy() {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const run = async (fn) => {
    setError('')
    setBusy(true)
    try {
      await fn()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return { busy, error, setError, run }
}

// LinkBox shows a password link once it exists, with a copy button.
function LinkBox({ link, username }) {
  const [copied, setCopied] = useState(false)
  const [failed, setFailed] = useState(false)
  async function copy() {
    try {
      await navigator.clipboard.writeText(link.url)
      setCopied(true)
    } catch {
      setFailed(true)
    }
  }
  return (
    <div className="space-y-1.5">
      <div className="flex gap-1.5">
        <input readOnly className={`${inputCls} font-mono text-xs`} value={link.url} onFocus={(e) => e.target.select()} />
        <Button type="button" variant="outline" onClick={copy} title="Copy link">
          {copied ? <Icon.Check size={14} /> : <Icon.Copy size={14} />}
        </Button>
      </div>
      <p className="text-xs text-muted">
        {failed && 'Could not copy — select the link and copy it by hand. '}
        Expires {new Date(link.expiresAt).toLocaleString()}. Send it to {username} privately — anyone with it can set the password.
      </p>
    </div>
  )
}

function PasswordPair({ password, setPassword, confirm, setConfirm, autoFocus, label = 'Password' }) {
  return (
    <>
      <Field label={label} hint="At least 8 characters.">
        <input type="password" autoComplete="new-password" className={inputCls} value={password}
          onChange={(e) => setPassword(e.target.value)} autoFocus={autoFocus} />
      </Field>
      <Field label={`Confirm ${label.toLowerCase()}`}>
        <input type="password" autoComplete="new-password" className={inputCls} value={confirm}
          onChange={(e) => setConfirm(e.target.value)} />
      </Field>
    </>
  )
}

// CreateUserDialog creates an approved account (app/useradmin.go). Its owner either
// gets a link to choose a password, or a temporary one the admin typed, which they
// must change when they first sign in.
function CreateUserDialog({ onClose }) {
  const [profile, setProfile] = useState(() => ({ firstName: '', lastName: '', email: '', avatar: randomAvatar() }))
  const [username, setUsername] = useState('')
  const [role, setRole] = useState('user')
  const [mode, setMode] = useState('link')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [created, setCreated] = useState(null) // { user, url?, expiresAt? }
  const { busy, error, setError, run } = useBusy()

  function onSubmit(e) {
    e.preventDefault()
    if (mode === 'password' && password !== confirm) {
      setError('Passwords do not match.')
      return
    }
    run(async () => {
      setCreated(await api.createUser({ ...profile, username, role, password: mode === 'password' ? password : '' }))
    })
  }

  if (created) {
    return (
      <Modal title={`${created.user.username} was created`} icon={<Icon.Check size={16} />} onClose={onClose}>
        {created.url ? (
          <div className="space-y-3">
            <p className="text-sm text-muted">The account cannot be signed in to until its owner chooses a password with this link.</p>
            <LinkBox link={created} username={created.user.username} />
          </div>
        ) : (
          <p className="text-sm text-muted">
            Give {created.user.username} the temporary password. They will be asked to choose their own when they first sign in.
          </p>
        )}
        <div className="mt-5 flex justify-end">
          <Button type="button" onClick={onClose}>Done</Button>
        </div>
      </Modal>
    )
  }

  const ready = profileComplete(profile) && username.trim().length >= 3 && (mode === 'link' || password)
  return (
    <Modal title="Add user" subtitle="The account is approved straight away." icon={<Icon.Plus size={16} />} onClose={onClose}>
      <ErrorBox>{error}</ErrorBox>
      <form onSubmit={onSubmit} className="space-y-3">
        <ProfileFields value={profile} onChange={setProfile} autoFocus />
        <div className="grid grid-cols-2 gap-2">
          <Field label="Username" hint="3–32 characters.">
            <input className={inputCls} value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" />
          </Field>
          <Field label="Role">
            <select className={inputCls} value={role} onChange={(e) => setRole(e.target.value)}>
              <option value="user">User</option>
              <option value="admin">Administrator</option>
            </select>
          </Field>
        </div>
        <div className="pt-1">
          <Tabs value={mode} onChange={(m) => { setMode(m); setError('') }}
            options={[['link', 'Send a set-password link'], ['password', 'Set a temporary password']]} />
          {mode === 'link' ? (
            <p className="text-sm text-muted">You get a link, valid for 7 days, for them to choose their own password.</p>
          ) : (
            <div className="space-y-3">
              <PasswordPair password={password} setPassword={setPassword} confirm={confirm} setConfirm={setConfirm} label="Temporary password" />
              <p className="text-xs text-muted">They must change it when they first sign in.</p>
            </div>
          )}
        </div>
        <div className="flex justify-end gap-2 pt-2">
          <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
          <Button type="submit" disabled={busy || !ready}>{busy ? 'Creating…' : 'Create account'}</Button>
        </div>
      </form>
    </Modal>
  )
}

// EditUserDialog corrects an account's username and profile.
function EditUserDialog({ user, onClose, onSaved }) {
  const [profile, setProfile] = useState({ firstName: user.firstName || '', lastName: user.lastName || '', email: user.email || '', avatar: user.avatar || '' })
  const [username, setUsername] = useState(user.username)
  const { busy, error, run } = useBusy()
  const renamed = username.trim() !== user.username

  function onSubmit(e) {
    e.preventDefault()
    run(async () => {
      await api.updateUser(user.id, { ...profile, username })
      onSaved()
    })
  }

  return (
    <Modal title={`Edit ${user.username}`} icon={<Icon.Pencil size={15} />} onClose={onClose}>
      <ErrorBox>{error}</ErrorBox>
      <form onSubmit={onSubmit} className="space-y-3">
        <ProfileFields value={profile} onChange={setProfile} autoFocus />
        <Field label="Username" hint={renamed ? `They will sign in as ${username.trim()} from now on; they are told so.` : '3–32 characters.'}>
          <input className={inputCls} value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" />
        </Field>
        <div className="flex justify-end gap-2 pt-2">
          <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
          <Button type="submit" disabled={busy || username.trim().length < 3}>{busy ? 'Saving…' : 'Save'}</Button>
        </div>
      </form>
    </Modal>
  )
}

// ResetPasswordDialog gets another account's owner back in, two ways: hand them a
// one-time link so they pick the password themselves, or set one — temporary by
// default, so the admin does not go on knowing it. Either signs the account out.
function ResetPasswordDialog({ user, onClose }) {
  const [mode, setMode] = useState('link')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [requireChange, setRequireChange] = useState(true)
  const [revokeTokens, setRevokeTokens] = useState(false)
  const [done, setDone] = useState('')
  const [link, setLink] = useState(null) // { url, expiresAt }
  const { busy, error, setError, run } = useBusy()

  function onSetPassword(e) {
    e.preventDefault()
    if (password !== confirm) {
      setError('Passwords do not match.')
      return
    }
    run(async () => {
      await api.setUserPassword(user.id, password, revokeTokens, requireChange)
      setDone(`The password for ${user.username} was changed, and every session they had was signed out.` +
        (revokeTokens ? ' Their API tokens were revoked.' : '') +
        (requireChange ? ' They must choose their own when they next sign in.' : ''))
    })
  }

  return (
    <Modal title={`Reset password for ${user.username}`} subtitle="Either way, every session the account has is signed out."
      icon={<Icon.Key size={16} />} onClose={onClose}>
      <Tabs value={mode} onChange={(m) => { setMode(m); setError('') }} options={[['link', 'Send a reset link'], ['set', 'Set the password']]} />
      <ErrorBox>{error}</ErrorBox>
      {mode === 'link' ? (
        <div className="space-y-3">
          <p className="text-sm text-muted">
            A one-time link {user.username} opens to choose a new password. It works once, for 24 hours
            {user.invitePending ? ' (7 days, as their invite is still unused)' : ''}; a new link replaces this one.
          </p>
          {link && <LinkBox link={link} username={user.username} />}
          <div className="flex justify-end gap-2 pt-2">
            <Button type="button" variant="outline" onClick={onClose}>Close</Button>
            <Button type="button" disabled={busy} onClick={() => run(async () => setLink(await api.createResetLink(user.id)))}>
              {busy ? 'Creating…' : link ? 'Create a new link' : 'Create link'}
            </Button>
          </div>
        </div>
      ) : done ? (
        <div className="space-y-3">
          <div className="rounded-lg border border-success/30 bg-success/15 px-3 py-2 text-sm text-success">{done}</div>
          <div className="flex justify-end">
            <Button type="button" onClick={onClose}>Done</Button>
          </div>
        </div>
      ) : (
        <form onSubmit={onSetPassword} className="space-y-3">
          <PasswordPair password={password} setPassword={setPassword} confirm={confirm} setConfirm={setConfirm} autoFocus label="New password" />
          <label className="flex items-start gap-2 text-sm">
            <input type="checkbox" className="mt-0.5" checked={requireChange} onChange={(e) => setRequireChange(e.target.checked)} />
            <span>Require a new password at next sign-in <span className="block text-xs text-muted">So that only they know their password.</span></span>
          </label>
          <label className="flex items-start gap-2 text-sm">
            <input type="checkbox" className="mt-0.5" checked={revokeTokens} onChange={(e) => setRevokeTokens(e.target.checked)} />
            <span>Also revoke their API tokens <span className="block text-xs text-muted">Do this if the account may have been compromised.</span></span>
          </label>
          <div className="flex justify-end gap-2 pt-2">
            <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={busy || !password}>{busy ? 'Saving…' : 'Set password'}</Button>
          </div>
        </form>
      )}
    </Modal>
  )
}

// SessionsDialog lists where an account is signed in, ends sessions without touching
// the password, and clears the sign-in history without ending any. On your own
// account the session you are using is marked and kept: the rest can go.
function SessionsDialog({ user, isYou, onClose }) {
  const [list, setList] = useState(null)
  const [cleared, setCleared] = useState(false)
  const { busy, error, run } = useBusy()
  const [dialog, ask] = useDialog()

  async function clearHistory() {
    const ok = await ask.confirm({
      title: `Clear ${user.username}'s sign-in history?`,
      body: 'Forgets their last sign-in, and each session\'s address, browser and times. They stay signed in.',
      confirmLabel: 'Clear history',
      danger: true,
    })
    if (ok) run(async () => { await api.clearSignInHistory(user.id); setCleared(true); await load() })
  }
  const load = useCallback(() => api.listUserSessions(user.id).then(setList, () => setList([])), [user.id])
  useEffect(() => { load() }, [load])

  return (
    <Modal wide title={`Sessions for ${user.username}`} icon={<Icon.Monitor size={16} />} onClose={onClose}
      subtitle={user.lastLoginAt && !cleared ? `Last signed in ${new Date(user.lastLoginAt).toLocaleString()}.` : 'No sign-in recorded.'}>
      <ErrorBox>{error}</ErrorBox>
      {!list ? (
        <div className="py-6 text-center text-muted">Loading…</div>
      ) : list.length === 0 ? (
        <div className="py-6 text-center text-muted">No active sessions.</div>
      ) : (
        <div className="max-h-[50vh] overflow-auto">
          <table className="w-full text-sm">
            <thead className="sticky top-0 border-b bg-surface text-xs text-muted">
              <tr>
                <th className="px-2 py-1.5 text-left font-medium">Browser</th>
                <th className="px-2 py-1.5 text-left font-medium">Address</th>
                <th className="px-2 py-1.5 text-left font-medium">Signed in</th>
                <th className="px-2 py-1.5 text-left font-medium">Last active</th>
                <th className="px-2 py-1.5" />
              </tr>
            </thead>
            <tbody>
              {list.map((s) => (
                <tr key={s.id} className="border-b">
                  <td className="px-2 py-2" title={s.userAgent}>{describeAgent(s.userAgent)}</td>
                  <td className="px-2 py-2 font-mono text-xs">{s.ip || '—'}</td>
                  <td className="px-2 py-2 text-muted">{s.createdAt ? fmtDate(s.createdAt) : '—'}</td>
                  <td className="px-2 py-2">{fmtAgo(s.lastSeenAt) || '—'}</td>
                  <td className="px-2 py-2 text-right">
                    {s.current ? (
                      <Badge tone="primary">This browser</Badge>
                    ) : (
                      <Button size="sm" variant="outline" disabled={busy} onClick={() => run(async () => { await api.endUserSession(user.id, s.id); await load() })}>
                        Sign out
                      </Button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <div className="mt-5 flex justify-end gap-2">
        <Button type="button" variant="ghost" className="mr-auto" disabled={busy} onClick={clearHistory}>Clear sign-in history</Button>
        <Button type="button" variant="outline" onClick={onClose}>Close</Button>
        {list?.some((s) => !s.current) && (
          <Button type="button" variant="danger" disabled={busy} onClick={() => run(async () => { await api.endUserSessions(user.id); await load() })}>
            {isYou ? 'Sign out other sessions' : 'Sign out everywhere'}
          </Button>
        )}
      </div>
      {dialog}
    </Modal>
  )
}
