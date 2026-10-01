import { useCallback, useEffect, useState } from 'react'
import { Shell, Splash } from './AuthScreens.jsx'
import { AuthProvider } from './AuthProvider.jsx'
import Root from '../Root.jsx'
import { Button, Field, inputCls } from '../components/ui.jsx'
import { shareApi } from '../lib/shareApi.js'
import { joinToken } from '../lib/guest.js'
import { useDialog } from '../components/Dialog.jsx'
import { Avatar, AvatarPicker, randomAvatar } from '../components/Avatar.jsx'

// GuestScreens — what a person who opened a share link sees before they are in, and
// after the session is over (app/share.go).
//
//   join     sign in with an account (verified), or a name + email (not verified)
//   lobby    waiting for the host to admit them; the page polls
//   in       the ordinary app, as the host, with the guest shell (App.jsx)
//   closed   denied, removed, left, or the session ended
//
// The link is the whole address of the session: a reload on /join/<token> lands
// back where the guest was, because the cookie says who they are. Leaving is final
// for that link: the cookie stays, marked as left, and coming back takes a new
// invitation link from the host (app/share.go, handleShareNewLink).

function fmtTime(iso) {
  try { return new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) } catch { return '' }
}

export function GuestGate() {
  const token = joinToken()
  const [phase, setPhase] = useState('loading') // loading | join | lobby | in | closed
  const [info, setInfo] = useState(null)
  const [closed, setClosed] = useState('')

  const check = useCallback(async () => {
    try {
      const s = await shareApi.joinStatus(token)
      setInfo(s)
      if (s.state === 'admitted') setPhase('in')
      else if (s.state === 'waiting') setPhase('lobby')
      else { setClosed(s.state); setPhase('closed') }
    } catch (e) {
      if (e.status === 404) {
        // Not joined yet — or a dead link, which the info call tells apart.
        try {
          const i = await shareApi.joinInfo(token)
          setInfo(i)
          setPhase('join')
        } catch {
          setClosed('invalid'); setPhase('closed')
        }
      } else {
        setClosed('invalid'); setPhase('closed')
      }
    }
  }, [token])

  useEffect(() => { check() }, [check])

  // The lobby polls: an admit or a deny arrives within two seconds.
  useEffect(() => {
    if (phase !== 'lobby') return undefined
    const t = setInterval(check, 2000)
    return () => clearInterval(t)
  }, [phase, check])

  if (phase === 'loading') return <Splash />
  if (phase === 'join') return <JoinScreen token={token} info={info} onJoined={check} />
  if (phase === 'lobby') return <LobbyScreen token={token} info={info} />
  if (phase === 'closed') return <ClosedScreen reason={closed} info={info} />
  return (
    <AuthProvider>
      <Root onSessionEnded={(reason) => { setClosed(reason); setPhase('closed') }} />
    </AuthProvider>
  )
}

// JoinScreen offers two ways in. A colleague with a DBCanvas account signs in with
// it — or, when this browser is already signed in, joins as that account in one
// click — and the host sees who they really are. Anyone else joins as a guest with a
// name, an email and an avatar of their choosing. Either way the host admits them
// from the lobby, and either way they work in the host's workspace, not their own.
function JoinScreen({ token, info, onJoined }) {
  const acct = info?.account
  const [mode, setMode] = useState(acct && !acct.isHost ? 'account' : 'guest') // guest | account
  const [other, setOther] = useState(false) // an account other than the signed-in one
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [avatar, setAvatar] = useState(randomAvatar)
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const run = async (fn) => {
    setErr(''); setBusy(true)
    try {
      await fn()
      await onJoined()
    } catch (x) {
      setErr(x.message)
    } finally {
      setBusy(false)
    }
  }
  const asGuest = (e) => { e.preventDefault(); run(() => shareApi.join(token, name.trim(), email.trim(), avatar)) }
  const asAccount = (e) => { e.preventDefault(); run(() => shareApi.joinAccount(token, { username: username.trim(), password })) }
  const asSignedIn = () => run(() => shareApi.joinAccount(token, { useSession: true }))
  const tab = (id, label) => (
    <button type="button" onClick={() => { setMode(id); setErr('') }}
      className={`rounded-md py-1.5 text-sm font-medium transition ${mode === id ? 'bg-surface text-fg shadow' : 'text-muted'}`}>{label}</button>
  )
  return (
    <Shell title="Join a shared session" subtitle={`${info?.hostName || 'The host'} is sharing ${info?.stackName ? `“${info.stackName}”` : 'their workspace'}`}>
      {acct?.isHost ? (
        <div className="mb-3 rounded-lg border border-primary/30 bg-primary/10 px-3 py-2 text-xs text-primary">
          This is your own session — you host it from DBCanvas itself. <a className="font-medium underline" href="/">Open DBCanvas</a>
        </div>
      ) : null}
      <div className="mb-4 grid grid-cols-2 gap-1 rounded-lg bg-surface2 p-1">
        {tab('account', 'I have an account')}
        {tab('guest', 'Join as a guest')}
      </div>
      {mode === 'account' ? (
        acct && !acct.isHost && !other ? (
          <div className="space-y-3">
            <div className="flex items-center gap-3 rounded-lg border bg-bg px-3 py-2.5">
              <Avatar avatar={acct.avatar} name={acct.name} size={40} />
              <div className="min-w-0 flex-1">
                <div className="truncate text-sm font-medium">{acct.name}</div>
                <div className="truncate text-xs text-muted">signed in as {acct.username}</div>
              </div>
            </div>
            {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
            <Button variant="primary" className="w-full" disabled={busy} onClick={asSignedIn}>
              {busy ? 'Joining…' : `Ask to join as ${acct.name.split(' ')[0] || acct.username}`}
            </Button>
            <button type="button" className="w-full text-center text-xs text-muted hover:text-fg" onClick={() => setOther(true)}>
              Not you? Sign in with another account
            </button>
          </div>
        ) : (
          <form onSubmit={asAccount} className="space-y-3">
            <Field label="Username">
              <input className={inputCls} value={username} onChange={(e) => setUsername(e.target.value)} autoFocus autoComplete="username" />
            </Field>
            <Field label="Password">
              <input className={inputCls} type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" />
            </Field>
            <p className="text-xs text-muted">
              The host sees your name and avatar, and that it is really you. This joins the session only — it does not sign this browser in.
            </p>
            {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
            <Button type="submit" variant="primary" className="w-full" disabled={busy || !username.trim() || !password}>
              {busy ? 'Joining…' : 'Sign in and ask to join'}
            </Button>
          </form>
        )
      ) : (
        <form onSubmit={asGuest} className="space-y-3">
          <Field label="Your name">
            <input className={inputCls} value={name} onChange={(e) => setName(e.target.value)} maxLength={80} autoFocus required />
          </Field>
          <Field label="Your email">
            <input className={inputCls} type="email" value={email} onChange={(e) => setEmail(e.target.value)} maxLength={254} required />
          </Field>
          <div className="space-y-1.5">
            <div className="flex items-center gap-2 text-xs font-medium text-muted">Avatar <Avatar avatar={avatar} name={name} size={20} /></div>
            <AvatarPicker value={avatar} onChange={setAvatar} size={32} />
          </div>
          <p className="text-xs text-muted">
            The host sees your name and email before letting you in. The link works until {fmtTime(info?.expiresAt)}.
          </p>
          {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
          <Button type="submit" variant="primary" className="w-full" disabled={busy || !name.trim() || !email.trim()}>
            {busy ? 'Joining…' : 'Ask to join'}
          </Button>
        </form>
      )}
    </Shell>
  )
}

function LobbyScreen({ token, info }) {
  const [dialog, ask] = useDialog()
  const [err, setErr] = useState('')
  const leave = async () => {
    const yes = await ask.confirm({
      title: 'Leave the lobby?',
      body: 'The host will not be able to admit you. To come back you will need a new invitation link.',
      confirmLabel: 'Leave', danger: true,
    })
    if (!yes) return
    try {
      await shareApi.leave(token)
    } catch (e) {
      setErr(`Could not leave: ${e.message}`)
      return
    }
    location.replace('/?left=1')
  }
  return (
    <Shell title="Waiting for the host" subtitle={`${info?.hostName || 'The host'} will let you in shortly`}>
      <div className="space-y-4 text-sm">
        <div className="flex items-center gap-3">
          <div className="h-5 w-5 shrink-0 animate-spin rounded-full border-2 border-surface2 border-t-primary" />
          <span>You are in the lobby as <span className="font-medium">{info?.guest?.name}</span>.</span>
        </div>
        <p className="text-xs text-muted">This page opens the session by itself once you are admitted. Keep it open.</p>
        {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
        <Button variant="outline" className="w-full" onClick={leave}>Leave</Button>
      </div>
      {dialog}
    </Shell>
  )
}

const CLOSED_TEXT = {
  denied: ['Not admitted', 'The host did not let you in to this session.'],
  removed: ['Removed from the session', 'The host removed you from this session. To come back, ask the host for a new invitation link.'],
  left: ['You left the session', 'This link no longer lets you back in. To rejoin, ask the host for a new invitation link. You can close this tab.'],
  ended: ['The session has ended', 'The host ended the session, or it reached its time limit.'],
  expired: ['The session has ended', 'The session reached its time limit.'],
  revoked: ['The session has ended', 'The host revoked the link.'],
  invalid: ['This link does not work', 'It has expired, was revoked, or was never valid. Ask the host for a new one.'],
}

function ClosedScreen({ reason }) {
  const [title, body] = CLOSED_TEXT[reason] || CLOSED_TEXT.ended
  return (
    <Shell title={title} subtitle="DBCanvas shared session">
      <p className="text-sm text-muted">{body}</p>
    </Shell>
  )
}
