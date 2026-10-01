import { useState } from 'react'
import { Button, Field, inputCls } from './ui.jsx'
import { Avatar, AvatarPicker } from './Avatar.jsx'
import { api } from '../lib/api.js'

// ProfileFields — a first name, a last name, an email address and an avatar
// (app/profile.go): the same fields wherever an account is described — creating the
// first administrator, registering, the Profile page and the profile dialog.

export function ProfileFields({ value, onChange, autoFocus }) {
  const set = (k) => (e) => onChange({ ...value, [k]: typeof e === 'string' ? e : e.target.value })
  return (
    <div className="space-y-3">
      <div className="grid grid-cols-2 gap-2">
        <Field label="First name">
          <input className={inputCls} value={value.firstName} onChange={set('firstName')} maxLength={60} autoFocus={autoFocus} autoComplete="given-name" />
        </Field>
        <Field label="Last name">
          <input className={inputCls} value={value.lastName} onChange={set('lastName')} maxLength={60} autoComplete="family-name" />
        </Field>
      </div>
      <Field label="Email address">
        <input className={inputCls} type="email" value={value.email || ''} onChange={set('email')} maxLength={254} autoComplete="email" />
      </Field>
      <div className="space-y-1.5">
        <div className="flex items-center gap-2 text-xs font-medium text-muted">
          Avatar
          <Avatar avatar={value.avatar} name={`${value.firstName} ${value.lastName}`} size={20} />
        </div>
        <AvatarPicker value={value.avatar} onChange={set('avatar')} size={32} />
      </div>
    </div>
  )
}

export const profileComplete = (p) => !!(p.firstName?.trim() && p.lastName?.trim() && /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(p.email?.trim() || ''))

// ProfileDialog edits the signed-in account's profile. firstTime words it as the
// one-off request an account from before profiles gets after signing in.
export function ProfileDialog({ user, firstTime, onSaved, onClose }) {
  const [p, setP] = useState({ firstName: user?.firstName || '', lastName: user?.lastName || '', email: user?.email || '', avatar: user?.avatar || '' })
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const save = async (e) => {
    e.preventDefault()
    setErr(''); setBusy(true)
    try {
      const u = await api.updateProfile(p)
      onSaved?.(u)
      onClose()
    } catch (x) {
      setErr(x.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="fixed inset-0 z-[90] flex items-center justify-center bg-black/40 p-4" onMouseDown={firstTime ? undefined : onClose}>
      <form onSubmit={save} onMouseDown={(e) => e.stopPropagation()}
        className="w-full max-w-md space-y-4 rounded-2xl border bg-surface p-5 shadow-xl">
        <div>
          <h2 className="text-base font-semibold">{firstTime ? 'Tell us who you are' : 'Your profile'}</h2>
          <p className="text-xs text-muted">
            {firstTime
              ? 'Your name, email and avatar are what colleagues see beside your work and in shared sessions. You can change them any time on your Profile.'
              : 'Shown beside your work and in shared sessions. Your name and email are stored encrypted.'}
          </p>
        </div>
        <ProfileFields value={p} onChange={setP} autoFocus />
        {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" onClick={onClose}>{firstTime ? 'Later' : 'Cancel'}</Button>
          <Button type="submit" variant="primary" disabled={busy || !profileComplete(p)}>{busy ? 'Saving…' : 'Save'}</Button>
        </div>
      </form>
    </div>
  )
}
