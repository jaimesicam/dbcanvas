import { useCallback, useEffect, useState } from 'react'
import { Shell, Splash } from './AuthScreens.jsx'
import { AuthProvider } from './AuthProvider.jsx'
import Root from '../Root.jsx'
import { Button, Field, inputCls } from '../components/ui.jsx'
import { shareApi } from '../lib/shareApi.js'
import { joinToken } from '../lib/guest.js'

// GuestScreens — what a person who opened a share link sees before they are in, and
// after the session is over (app/share.go).
//
//   join     name + email, both required, neither verified
//   lobby    waiting for the host to admit them; the page polls
//   in       the ordinary app, as the host, with the guest shell (App.jsx)
//   closed   denied, removed, left, or the session ended
//
// The link is the whole address of the session: a reload on /join/<token> lands
// back where the guest was, because the cookie says who they are.

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
  if (phase === 'lobby') return <LobbyScreen token={token} info={info} onLeft={() => { setClosed('left'); setPhase('closed') }} />
  if (phase === 'closed') return <ClosedScreen reason={closed} info={info} />
  return (
    <AuthProvider>
      <Root onSessionEnded={(reason) => { setClosed(reason); setPhase('closed') }} />
    </AuthProvider>
  )
}

function JoinScreen({ token, info, onJoined }) {
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async (e) => {
    e.preventDefault()
    setErr(''); setBusy(true)
    try {
      await shareApi.join(token, name.trim(), email.trim())
      await onJoined()
    } catch (x) {
      setErr(x.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Shell title="Join a shared session" subtitle={`${info?.hostName || 'The host'} is sharing ${info?.stackName ? `“${info.stackName}”` : 'a session'}`}>
      <form onSubmit={submit} className="space-y-3">
        <Field label="Your name">
          <input className={inputCls} value={name} onChange={(e) => setName(e.target.value)} maxLength={80} autoFocus required />
        </Field>
        <Field label="Your email">
          <input className={inputCls} type="email" value={email} onChange={(e) => setEmail(e.target.value)} maxLength={254} required />
        </Field>
        <p className="text-xs text-muted">
          The host sees both before letting you in. The link works until {fmtTime(info?.expiresAt)}.
        </p>
        {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
        <Button type="submit" variant="primary" className="w-full" disabled={busy || !name.trim() || !email.trim()}>
          {busy ? 'Joining…' : 'Ask to join'}
        </Button>
      </form>
    </Shell>
  )
}

function LobbyScreen({ token, info, onLeft }) {
  const leave = async () => {
    try { await shareApi.leave(token) } catch { /* the cookie is gone either way */ }
    onLeft()
  }
  return (
    <Shell title="Waiting for the host" subtitle={`${info?.hostName || 'The host'} will let you in shortly`}>
      <div className="space-y-4 text-sm">
        <div className="flex items-center gap-3">
          <div className="h-5 w-5 shrink-0 animate-spin rounded-full border-2 border-surface2 border-t-primary" />
          <span>You are in the lobby as <span className="font-medium">{info?.guest?.name}</span>.</span>
        </div>
        <p className="text-xs text-muted">This page opens the session by itself once you are admitted. Keep it open.</p>
        <Button variant="outline" className="w-full" onClick={leave}>Leave</Button>
      </div>
    </Shell>
  )
}

const CLOSED_TEXT = {
  denied: ['Not admitted', 'The host did not let you in to this session.'],
  removed: ['Removed from the session', 'The host removed you from this session.'],
  left: ['You left the session', 'You can close this tab.'],
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
