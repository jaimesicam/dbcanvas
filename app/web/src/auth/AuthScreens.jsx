import { useEffect, useState } from 'react'
import { useAuth } from './AuthProvider.jsx'
import { useTheme, THEMES } from '../theme/ThemeProvider.jsx'
import { Button, Field, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { ProfileFields, profileComplete } from '../components/ProfileFields.jsx'
import { randomAvatar } from '../components/Avatar.jsx'
import { api } from '../lib/api.js'

export function Splash() {
  return (
    <div className="flex h-full items-center justify-center bg-bg">
      <div className="h-10 w-10 animate-spin rounded-full border-2 border-surface2 border-t-primary" />
    </div>
  )
}

function ThemeSwatches() {
  const { theme, setTheme } = useTheme()
  return (
    <div className="absolute right-4 top-4 flex gap-1.5">
      {THEMES.map((t) => (
        <button
          key={t.id}
          title={t.label}
          onClick={() => setTheme(t.id)}
          className={`h-5 w-5 rounded-full border-2 transition ${theme === t.id ? 'border-fg scale-110' : 'border-transparent'}`}
          style={{ background: t.swatch }}
        />
      ))}
    </div>
  )
}

export function Shell({ title, subtitle, children }) {
  return (
    <div className="relative flex h-full items-center justify-center bg-bg p-4">
      <ThemeSwatches />
      <div className="w-full max-w-md animate-fade-in rounded-2xl border bg-surface p-6 shadow-xl">
        <div className="mb-5 flex items-center gap-3">
          <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-primary text-primary-fg">
            <Icon.Brand size={24} />
          </div>
          <div>
            <h1 className="text-lg font-semibold text-fg">{title}</h1>
            {subtitle && <p className="text-sm text-muted">{subtitle}</p>}
          </div>
        </div>
        {children}
      </div>
    </div>
  )
}

function Banner({ kind, children }) {
  if (!children) return null
  const tones =
    kind === 'success'
      ? 'bg-success/15 text-success border-success/30'
      : 'bg-danger/15 text-danger border-danger/30'
  return <div className={`mb-3 rounded-lg border px-3 py-2 text-sm ${tones}`}>{children}</div>
}

export function SetupScreen() {
  const { setup } = useAuth()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [profile, setProfile] = useState(() => ({ firstName: '', lastName: '', email: '', avatar: randomAvatar() }))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function onSubmit(e) {
    e.preventDefault()
    setError('')
    if (password !== confirm) {
      setError('Passwords do not match.')
      return
    }
    setBusy(true)
    try {
      await setup(username, password, profile)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Shell title="Welcome to DBCanvas" subtitle="Create the administrator account">
      <Banner kind="error">{error}</Banner>
      <form onSubmit={onSubmit} className="space-y-3">
        <ProfileFields value={profile} onChange={setProfile} autoFocus />
        <Field label="Username">
          <input className={inputCls} value={username} onChange={(e) => setUsername(e.target.value)} />
        </Field>
        <Field label="Password" hint="At least 8 characters.">
          <input type="password" className={inputCls} value={password} onChange={(e) => setPassword(e.target.value)} />
        </Field>
        <Field label="Confirm password">
          <input type="password" className={inputCls} value={confirm} onChange={(e) => setConfirm(e.target.value)} />
        </Field>
        <Button type="submit" size="lg" className="w-full" disabled={busy || !profileComplete(profile)}>
          {busy ? 'Creating…' : 'Create administrator'}
        </Button>
      </form>
    </Shell>
  )
}

export function AuthScreen() {
  const { login, register } = useAuth()
  const [tab, setTab] = useState('signin')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [profile, setProfile] = useState(() => ({ firstName: '', lastName: '', email: '', avatar: randomAvatar() }))
  const [error, setError] = useState('')
  // A shared-session guest who pressed Leave lands here with ?left=1
  // (components/SessionPanel.jsx), and somebody who just used a reset link with
  // ?reset=1 (ResetPasswordScreen); say so once, then tidy the address.
  const [success, setSuccess] = useState(() => {
    try {
      const q = new URLSearchParams(location.search)
      const left = q.get('left') === '1'
      const reset = q.get('reset') === '1'
      if (!left && !reset) return ''
      q.delete('left')
      q.delete('reset')
      history.replaceState(null, '', location.pathname + (q.toString() ? `?${q}` : '') + location.hash)
      return left
        ? 'You left the shared session. To rejoin, ask the host for a new invitation link.'
        : 'Your password was changed. Sign in with the new one.'
    } catch { return '' }
  })
  const [busy, setBusy] = useState(false)

  function switchTab(next) {
    setTab(next)
    setError('')
    setSuccess('')
  }

  async function onSignin(e) {
    e.preventDefault()
    setError('')
    setSuccess('')
    setBusy(true)
    try {
      await login(username, password)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function onRegister(e) {
    e.preventDefault()
    setError('')
    setSuccess('')
    setBusy(true)
    try {
      const res = await register(username, password, profile)
      setSuccess(res.message || 'Account created.')
      setUsername('')
      setPassword('')
      setTab('signin')
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Shell title="DBCanvas" subtitle="Sign in to the interaction lab">
      <div className="mb-4 grid grid-cols-2 gap-1 rounded-lg bg-surface2 p-1">
        <button
          onClick={() => switchTab('signin')}
          className={`rounded-md py-1.5 text-sm font-medium transition ${tab === 'signin' ? 'bg-surface text-fg shadow' : 'text-muted'}`}
        >
          Sign in
        </button>
        <button
          onClick={() => switchTab('register')}
          className={`rounded-md py-1.5 text-sm font-medium transition ${tab === 'register' ? 'bg-surface text-fg shadow' : 'text-muted'}`}
        >
          Register
        </button>
      </div>

      <Banner kind="error">{error}</Banner>
      <Banner kind="success">{success}</Banner>

      {tab === 'signin' ? (
        <form onSubmit={onSignin} className="space-y-3">
          <Field label="Username">
            <input className={inputCls} value={username} onChange={(e) => setUsername(e.target.value)} autoFocus />
          </Field>
          <Field label="Password">
            <input type="password" className={inputCls} value={password} onChange={(e) => setPassword(e.target.value)} />
          </Field>
          <Button type="submit" size="lg" className="w-full" disabled={busy}>
            {busy ? 'Signing in…' : 'Sign in'}
          </Button>
        </form>
      ) : (
        <form onSubmit={onRegister} className="space-y-3">
          <ProfileFields value={profile} onChange={setProfile} autoFocus />
          <Field label="Username" hint="3–32 characters.">
            <input className={inputCls} value={username} onChange={(e) => setUsername(e.target.value)} />
          </Field>
          <Field label="Password" hint="At least 8 characters.">
            <input type="password" className={inputCls} value={password} onChange={(e) => setPassword(e.target.value)} />
          </Field>
          <Button type="submit" size="lg" className="w-full" disabled={busy || !profileComplete(profile)}>
            {busy ? 'Creating…' : 'Create account'}
          </Button>
          <p className="text-center text-xs text-muted">
            New accounts require administrator approval before first sign-in.
          </p>
        </form>
      )}
    </Shell>
  )
}

// resetToken is the token when this tab is on a password reset link
// (/reset-password/<token>, app/useradmin.go), else ''.
export function resetToken() {
  const p = location.pathname
  return p.startsWith('/reset-password/') ? decodeURIComponent(p.slice('/reset-password/'.length).split('/')[0] || '') : ''
}

// ResetPasswordScreen is where a reset link an administrator sent lands. It comes
// before sign-in — the person has no working password, which is the point — and
// sends them to the sign-in screen once the new one is set.
export function ResetPasswordScreen({ token }) {
  const [info, setInfo] = useState(null) // { username, firstName, expiresAt, purpose }
  const [dead, setDead] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    api.resetLinkInfo(token).then(setInfo, (err) => setDead(err.message))
  }, [token])

  async function onSubmit(e) {
    e.preventDefault()
    setError('')
    if (password !== confirm) {
      setError('Passwords do not match.')
      return
    }
    setBusy(true)
    try {
      await api.useResetLink(token, password)
      location.replace('/?reset=1')
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  if (dead) {
    return (
      <Shell title="Reset password" subtitle="This link cannot be used">
        <Banner kind="error">{dead}</Banner>
        <Button size="lg" className="w-full" onClick={() => location.replace('/')}>Go to sign in</Button>
      </Shell>
    )
  }
  if (!info) return <Splash />
  // An invite is the link an admin created the account with: nobody is resetting
  // anything, they are choosing a first password.
  const invite = info.purpose === 'invite'
  return (
    <Shell title={invite ? `Welcome${info.firstName ? `, ${info.firstName}` : ''}` : 'Reset password'}
      subtitle={invite ? `Choose a password for your account, ${info.username}` : `Choose a new password for ${info.username}`}>
      <Banner kind="error">{error}</Banner>
      <form onSubmit={onSubmit} className="space-y-3">
        <Field label="New password" hint="At least 8 characters.">
          <input type="password" autoComplete="new-password" className={inputCls} value={password}
            onChange={(e) => setPassword(e.target.value)} autoFocus />
        </Field>
        <Field label="Confirm new password">
          <input type="password" autoComplete="new-password" className={inputCls} value={confirm}
            onChange={(e) => setConfirm(e.target.value)} />
        </Field>
        <Button type="submit" size="lg" className="w-full" disabled={busy || !password}>
          {busy ? 'Saving…' : 'Set password'}
        </Button>
        <p className="text-center text-xs text-muted">
          {invite ? 'The link works once. Then sign in as ' + info.username + '.' : 'The link works once. Every session this account has is signed out.'}
        </p>
      </form>
    </Shell>
  )
}

// ForcePasswordChange is all an account sees after an administrator set its password
// for it: choose your own, or sign out. The server refuses everything else meanwhile.
export function ForcePasswordChange() {
  const { user, refresh, logout } = useAuth()
  const [current, setCurrent] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function onSubmit(e) {
    e.preventDefault()
    setError('')
    if (password !== confirm) {
      setError('Passwords do not match.')
      return
    }
    setBusy(true)
    try {
      await api.changePassword(current, password, false)
      await refresh()
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <Shell title="Choose your password" subtitle={`An administrator set a temporary password for ${user?.username}`}>
      <Banner kind="error">{error}</Banner>
      <form onSubmit={onSubmit} className="space-y-3">
        <Field label="Temporary password" hint="The one you just signed in with.">
          <input type="password" autoComplete="current-password" className={inputCls} value={current}
            onChange={(e) => setCurrent(e.target.value)} autoFocus />
        </Field>
        <Field label="New password" hint="At least 8 characters.">
          <input type="password" autoComplete="new-password" className={inputCls} value={password}
            onChange={(e) => setPassword(e.target.value)} />
        </Field>
        <Field label="Confirm new password">
          <input type="password" autoComplete="new-password" className={inputCls} value={confirm}
            onChange={(e) => setConfirm(e.target.value)} />
        </Field>
        <Button type="submit" size="lg" className="w-full" disabled={busy || !current || !password}>
          {busy ? 'Saving…' : 'Set password and continue'}
        </Button>
        <button type="button" onClick={logout} className="block w-full text-center text-xs text-muted hover:text-fg">
          Sign out instead
        </button>
      </form>
    </Shell>
  )
}
