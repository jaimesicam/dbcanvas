import { useEffect, useRef, useState } from 'react'
import { Icon } from './Icons.jsx'
import { Button } from './ui.jsx'
import { useSession } from '../session/SessionProvider.jsx'
import { shareApi } from '../lib/shareApi.js'
import { joinToken } from '../lib/guest.js'

// SessionPanel — the shared session, beside the workspace (app/share.go).
//
// One panel for the host and the guests, drawn differently by role: the host admits
// and removes, mutes, gives and takes control, and ends the session; a guest asks for
// control, hands it back, and leaves. Everybody chats, sees who is here and who
// drives, and can stop following to look around on their own.
//
// Chat is text. Every message is rendered as a React text node — never as HTML — so a
// guest's "<script>" is shown, not run.

function useCountdown(iso) {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const t = setTimeout(() => setNow(Date.now()), 1000 - (Date.now() % 1000))
    return () => clearTimeout(t)
  })
  const left = Math.max(0, (Date.parse(iso || '') || 0) - now)
  const m = Math.floor(left / 60000)
  const s = Math.floor((left % 60000) / 1000)
  return { left, text: `${m}:${String(s).padStart(2, '0')}` }
}

function when(iso) {
  try { return new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) } catch { return '' }
}

export default function SessionPanel({ onClose }) {
  const s = useSession()
  const p = s.presence
  const { left, text } = useCountdown(p?.expiresAt)
  const [draft, setDraft] = useState('')
  const [copied, setCopied] = useState(false)
  const listRef = useRef(null)

  useEffect(() => {
    const el = listRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [s.messages.length])

  const send = (e) => {
    e.preventDefault()
    const body = draft.trim()
    if (!body) return
    s.sendChat(body)
    setDraft('')
  }
  const copy = async () => {
    try { await navigator.clipboard.writeText(s.link); setCopied(true); setTimeout(() => setCopied(false), 1500) } catch { /* */ }
  }
  const leave = async () => {
    try { await shareApi.leave(joinToken()) } catch { /* */ }
    location.reload()
  }

  const guests = p?.guests || []
  const waiting = guests.filter((g) => g.state === 'waiting')
  const inRoom = guests.filter((g) => g.state === 'admitted')
  const controller = p?.controller ?? 0
  const low = left > 0 && left < 10 * 60 * 1000

  if (s.ended && s.isHost) {
    return (
      <aside className="flex w-80 shrink-0 flex-col border-l bg-surface">
        <div className="flex items-center gap-2 border-b px-3 py-2">
          <Icon.Share size={16} />
          <span className="flex-1 text-sm font-semibold">Session ended</span>
        </div>
        <div className="space-y-3 p-3 text-sm">
          <p className="text-muted">Every guest was disconnected. The transcript is kept on the stack.</p>
          <div className="flex gap-2">
            <a className="text-primary hover:underline" href={shareApi.transcriptURL(s.sid)}>Download transcript</a>
            <span className="text-muted">·</span>
            <a className="text-primary hover:underline" href={shareApi.transcriptURL(s.sid, 'json')}>JSON</a>
          </div>
          <Button variant="outline" onClick={s.reset}>Close</Button>
        </div>
      </aside>
    )
  }

  return (
    <aside className="flex w-80 shrink-0 flex-col border-l bg-surface" aria-label="Shared session">
      {/* header */}
      <div className="flex items-center gap-2 border-b px-3 py-2">
        <Icon.Share size={16} />
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-semibold">Shared session</div>
          <div className="text-xs text-muted">
            {s.isGuest ? `${p?.host?.name || 'The host'}'s workspace` : `${inRoom.length} guest${inRoom.length === 1 ? '' : 's'}`}
            {' · '}
            <span className={low ? 'font-medium text-warning' : ''}>ends in {text}</span>
            {!s.connected && <span className="text-warning"> · reconnecting…</span>}
          </div>
        </div>
        {onClose && <button title="Hide the panel" onClick={onClose} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg"><Icon.Close size={14} /></button>}
      </div>

      {/* control */}
      <div className="space-y-2 border-b px-3 py-2 text-xs">
        <div className="flex items-center gap-1.5">
          <Icon.Pointer size={13} className="text-primary" />
          <span className="flex-1">
            <span className="font-medium">{s.isDriver ? 'You have' : `${s.controllerName || '…'} has`}</span> control
          </span>
          {!s.isDriver && (
            <label className="flex cursor-pointer items-center gap-1 text-muted">
              <input type="checkbox" checked={s.following} onChange={(e) => s.setFollowing(e.target.checked)} /> Follow
            </label>
          )}
        </div>
        <div className="flex flex-wrap gap-1.5">
          {s.isGuest && !s.isDriver && <Button variant="outline" onClick={s.requestControl}>Request control</Button>}
          {s.isGuest && s.isDriver && <Button variant="outline" onClick={s.releaseControl}>Hand control back</Button>}
          {s.isHost && controller !== 0 && <Button variant="outline" onClick={s.takeControl}>Take control back</Button>}
          {s.isGuest && <Button variant="subtle" onClick={leave}>Leave</Button>}
          {s.isHost && <Button variant="danger" onClick={() => { if (confirm('End the session for everyone?')) s.end() }}>End session</Button>}
        </div>
        {s.isHost && s.requests.map((r) => (
          <div key={r.guestId} className="flex items-center gap-1.5 rounded-md bg-primary/10 px-2 py-1.5">
            <span className="flex-1"><span className="font-medium">{r.name}</span> asked for control</span>
            <button className="rounded px-1.5 py-0.5 font-medium text-primary hover:bg-primary/15" onClick={() => s.giveControl(r.guestId)}>Grant</button>
            <button className="rounded px-1.5 py-0.5 text-muted hover:bg-surface2" onClick={() => s.dismissRequest(r.guestId)}>Deny</button>
          </div>
        ))}
        {s.isHost && s.link && (
          <div className="flex items-center gap-1.5 rounded-md border bg-bg px-2 py-1">
            <span className="min-w-0 flex-1 truncate font-mono text-[11px]" title={s.link}>{s.link}</span>
            <button onClick={copy} className="shrink-0 text-primary hover:underline">{copied ? 'Copied' : 'Copy link'}</button>
          </div>
        )}
      </div>

      {/* people */}
      <div className="max-h-56 overflow-y-auto border-b px-3 py-2 text-xs">
        {s.isHost && waiting.length > 0 && (
          <div className="mb-2 space-y-1">
            <div className="font-medium text-muted">Lobby</div>
            {waiting.map((g) => (
              <div key={g.id} className="flex items-center gap-1.5 rounded-md bg-warning/10 px-2 py-1.5">
                <div className="min-w-0 flex-1">
                  <div className="truncate font-medium">{g.name}</div>
                  <div className="truncate text-muted">{g.email} · {g.remoteAddr}</div>
                </div>
                <button className="rounded px-1.5 py-0.5 font-medium text-primary hover:bg-primary/15" onClick={() => s.admit(g.id)}>Admit</button>
                <button className="rounded px-1.5 py-0.5 text-muted hover:bg-surface2" onClick={() => s.deny(g.id)}>Deny</button>
              </div>
            ))}
          </div>
        )}
        <div className="space-y-1">
          <Person name={p?.host?.name} badge="host" online={p?.host?.online} driving={controller === 0} />
          {inRoom.map((g) => (
            <Person key={g.id} name={g.name} sub={s.isHost ? g.email : ''} badge="guest" online={g.online}
              driving={controller === g.id} muted={g.muted}>
              {s.isHost && (
                <>
                  {controller !== g.id && <IconBtn title="Give control" onClick={() => s.giveControl(g.id)}><Icon.Pointer size={12} /></IconBtn>}
                  <IconBtn title={g.muted ? 'Unmute' : 'Mute'} onClick={() => s.mute(g.id, !g.muted)}><Icon.Chat size={12} /></IconBtn>
                  <IconBtn title="Remove from the session" onClick={() => { if (confirm(`Remove ${g.name}?`)) s.remove(g.id) }}><Icon.Close size={12} /></IconBtn>
                </>
              )}
            </Person>
          ))}
        </div>
      </div>

      {/* chat */}
      <div ref={listRef} className="flex-1 space-y-1.5 overflow-y-auto px-3 py-2 text-xs">
        {s.messages.map((m) => (m.kind === 'chat' ? (
          <div key={m.id}>
            <span className={`font-medium ${m.authorKind === 'host' ? 'text-primary' : 'text-fg'}`}>{m.author}</span>
            {m.authorKind === 'host' && <span className="ml-1 rounded bg-primary/15 px-1 text-[10px] text-primary">host</span>}
            <span className="ml-1.5 text-[10px] text-muted">{when(m.createdAt)}</span>
            <div className="whitespace-pre-wrap break-words text-fg">{m.body}</div>
          </div>
        ) : (
          <div key={m.id} className={`break-words ${m.kind === 'action' ? 'text-fg/80' : 'text-muted'} ${m.kind === 'expiry-warning' ? 'font-medium text-warning' : ''}`}>
            <span className="text-[10px]">{when(m.createdAt)} </span>
            {m.kind === 'action' ? <><span className="font-medium">{m.author}</span>: {m.body}</> : m.body}
          </div>
        )))}
        {s.messages.length === 0 && <div className="text-muted">No messages yet.</div>}
      </div>
      <form onSubmit={send} className="flex gap-1.5 border-t p-2">
        <input
          value={draft} onChange={(e) => setDraft(e.target.value)} maxLength={2000}
          placeholder="Message everyone…"
          className="min-w-0 flex-1 rounded-lg border bg-bg px-2 py-1.5 text-sm"
        />
        <Button type="submit" variant="primary" disabled={!draft.trim()}>Send</Button>
      </form>
    </aside>
  )
}

function Person({ name, sub, badge, online, driving, muted, children }) {
  return (
    <div className="group flex items-center gap-1.5 rounded-md px-1 py-1">
      <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${online ? 'bg-success' : 'bg-muted/50'}`} title={online ? 'online' : 'offline'} />
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-1 truncate">
          <span className="truncate font-medium">{name || '…'}</span>
          <span className={`rounded px-1 text-[10px] ${badge === 'host' ? 'bg-primary/15 text-primary' : 'bg-surface2 text-muted'}`}>{badge}</span>
          {driving && <span className="rounded bg-success/15 px-1 text-[10px] text-success">driving</span>}
          {muted && <span className="rounded bg-warning/15 px-1 text-[10px] text-warning">muted</span>}
        </div>
        {sub && <div className="truncate text-muted">{sub}</div>}
      </div>
      <div className="flex shrink-0 items-center gap-0.5 opacity-60 group-hover:opacity-100">{children}</div>
    </div>
  )
}

function IconBtn({ title, onClick, children }) {
  return <button title={title} onClick={onClick} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">{children}</button>
}
