import { useState } from 'react'
import { useAuth } from './AuthProvider.jsx'
import { useTheme, THEMES } from '../theme/ThemeProvider.jsx'
import { Button, Field, inputCls } from '../components/ui.jsx'

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

function Shell({ title, subtitle, children }) {
  return (
    <div className="relative flex h-full items-center justify-center bg-bg p-4">
      <ThemeSwatches />
      <div className="w-full max-w-sm animate-fade-in rounded-2xl border bg-surface p-6 shadow-xl">
        <div className="mb-5 flex items-center gap-3">
          <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-primary text-lg font-bold text-primary-fg">
            D
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
      await setup(username, password)
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
        <Field label="Username">
          <input className={inputCls} value={username} onChange={(e) => setUsername(e.target.value)} autoFocus />
        </Field>
        <Field label="Password" hint="At least 8 characters.">
          <input type="password" className={inputCls} value={password} onChange={(e) => setPassword(e.target.value)} />
        </Field>
        <Field label="Confirm password">
          <input type="password" className={inputCls} value={confirm} onChange={(e) => setConfirm(e.target.value)} />
        </Field>
        <Button type="submit" size="lg" className="w-full" disabled={busy}>
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
  const [error, setError] = useState('')
  const [success, setSuccess] = useState('')
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
      const res = await register(username, password)
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
          <Field label="Username" hint="3–32 characters.">
            <input className={inputCls} value={username} onChange={(e) => setUsername(e.target.value)} autoFocus />
          </Field>
          <Field label="Password" hint="At least 8 characters.">
            <input type="password" className={inputCls} value={password} onChange={(e) => setPassword(e.target.value)} />
          </Field>
          <Button type="submit" size="lg" className="w-full" disabled={busy}>
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
