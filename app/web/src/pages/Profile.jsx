import { useState } from 'react'
import { useAuth } from '../auth/AuthProvider.jsx'
import { api } from '../lib/api.js'
import { Badge, Button } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { Avatar, fullName } from '../components/Avatar.jsx'
import { ProfileFields, profileComplete } from '../components/ProfileFields.jsx'
import { ChangePassword } from './Settings.jsx'
import { useRefresh } from '../lib/useRefresh.jsx'

// Profile — everything about the signed-in person, in one place: who they are (name
// and avatar, app/profile.go), how they sign in (their password), and the API
// tokens that act as them. Settings is for how the app behaves; this is for the
// account. A shared-session guest never sees it (App.jsx's GUEST_HIDDEN): they act
// as the host, and the host's account is not theirs to change.

function Section({ title, hint, children }) {
  return (
    <div className="space-y-3 rounded-xl border bg-surface p-4">
      <div>
        <div className="text-sm font-semibold">{title}</div>
        {hint && <div className="text-xs text-muted">{hint}</div>}
      </div>
      {children}
    </div>
  )
}

function fmtDate(iso) {
  try { return new Date(iso).toLocaleDateString([], { year: 'numeric', month: 'long', day: 'numeric' }) } catch { return '' }
}

export default function Profile() {
  const { user, setUser, logout, refresh } = useAuth()
  // Refresh re-reads the account: a name changed in another tab, a role an admin moved.
  useRefresh(() => refresh())
  const [p, setP] = useState(() => ({ firstName: user?.firstName || '', lastName: user?.lastName || '', avatar: user?.avatar || '' }))
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState(null) // { tone, text }
  const dirty = p.firstName !== (user?.firstName || '') || p.lastName !== (user?.lastName || '') || p.avatar !== (user?.avatar || '')

  const save = async (e) => {
    e.preventDefault()
    setBusy(true); setMsg(null)
    try {
      const u = await api.updateProfile(p)
      setUser(u)
      setMsg({ tone: 'ok', text: 'Saved.' })
    } catch (x) {
      setMsg({ tone: 'err', text: x.message })
    } finally {
      setBusy(false)
    }
  }

  if (!user) return null
  return (
    <div className="max-w-2xl space-y-4">
      <div className="flex items-center gap-4 rounded-xl border bg-surface p-4">
        <Avatar avatar={user.avatar} name={fullName(user)} size={64} />
        <div className="min-w-0 flex-1">
          <div className="truncate text-lg font-semibold">{fullName(user)}</div>
          <div className="flex flex-wrap items-center gap-2 text-xs text-muted">
            <span>{user.username}</span>
            <Badge tone={user.role === 'admin' ? 'primary' : 'muted'}>{user.role}</Badge>
            {user.createdAt && <span>· member since {fmtDate(user.createdAt)}</span>}
          </div>
        </div>
        <Button variant="outline" size="sm" onClick={logout}><Icon.Logout size={15} /> Sign out</Button>
      </div>

      <Section title="Name and avatar" hint="What colleagues see beside your work, on shared boards and in shared sessions. Your names are stored encrypted.">
        <form onSubmit={save} className="space-y-3">
          <ProfileFields value={p} onChange={(v) => { setP(v); setMsg(null) }} />
          <div className="flex items-center gap-2">
            <Button type="submit" disabled={busy || !dirty || !profileComplete(p)}>{busy ? 'Saving…' : 'Save'}</Button>
            {dirty && <Button type="button" variant="ghost" onClick={() => setP({ firstName: user.firstName || '', lastName: user.lastName || '', avatar: user.avatar || '' })}>Undo changes</Button>}
            {msg && <span className={`text-xs ${msg.tone === 'ok' ? 'text-success' : 'text-danger'}`}>{msg.text}</span>}
          </div>
        </form>
      </Section>

      <Section title="Username" hint="How you sign in. It cannot be changed — an administrator can create a new account if you need another.">
        <div className="rounded-lg border bg-bg px-3 py-2 font-mono text-sm">{user.username}</div>
      </Section>

      <ChangePassword />

      <Section title="API tokens" hint="Tokens let scripts and the dbcanvas CLI act as you, with the scope you give each one.">
        <Button variant="outline" size="sm" onClick={() => { location.hash = 'api' }}>
          <Icon.Code size={15} /> Manage your API tokens
        </Button>
      </Section>
    </div>
  )
}
