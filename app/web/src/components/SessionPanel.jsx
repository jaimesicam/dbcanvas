import { useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { Icon } from './Icons.jsx'
import { Button } from './ui.jsx'
import { useDialog } from './Dialog.jsx'
import { useSession } from '../session/SessionProvider.jsx'
import { shareApi } from '../lib/shareApi.js'
import { joinToken } from '../lib/guest.js'
import { EMOJI_GROUPS, onlyEmoji, withEmoji } from '../lib/emoji.js'
import { setSoundsEnabled, soundsEnabled, subscribeSounds } from '../lib/sessionSounds.js'

// SessionPanel — the shared session, beside the workspace (app/share.go).
//
// One panel for the host and the guests, drawn differently by role: the host admits
// and removes, mutes, gives and takes control, and ends the session; a guest asks for
// control, hands it back, and leaves. Everybody chats, sees who is here and who
// drives, and can stop following to look around on their own.
//
// Chat is text. Every message is rendered as a React text node — never as HTML — so a
// guest's "<script>" is shown, not run. Emoji are text too: the picker inserts them at
// the caret, and typed emoticons (":)", ":+1:") become emoji on send (lib/emoji.js).
// Enter sends, Shift+Enter starts a new line; the bell in the chat header turns the
// alert sounds off for this browser (lib/sessionSounds.js).

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

function initials(name) {
  const parts = String(name || '?').trim().split(/[\s._@-]+/).filter(Boolean)
  return ((parts[0]?.[0] || '?') + (parts.length > 1 ? parts[parts.length - 1][0] : '')).toUpperCase()
}

// A steady colour per name, so a face is recognisable down a long chat.
const AVATAR_TONES = ['bg-sky-600', 'bg-emerald-600', 'bg-amber-600', 'bg-rose-600', 'bg-violet-600', 'bg-teal-600', 'bg-fuchsia-600', 'bg-orange-600']
function tone(name) {
  let h = 0
  for (const ch of String(name || '')) h = (h * 31 + ch.codePointAt(0)) >>> 0
  return AVATAR_TONES[h % AVATAR_TONES.length]
}

function Avatar({ name, host, size = 'md' }) {
  const dim = size === 'sm' ? 'h-6 w-6 text-[10px]' : 'h-8 w-8 text-xs'
  return (
    <span className={`flex shrink-0 select-none items-center justify-center rounded-full font-semibold text-white ${dim} ${host ? 'bg-primary' : tone(name)}`} aria-hidden>
      {initials(name)}
    </span>
  )
}

// Messages from one author a few minutes apart read as one block, under one name.
const GROUP_MS = 3 * 60 * 1000
function groupStart(prev, m) {
  if (!prev || prev.kind !== 'chat') return true
  if (prev.authorKind !== m.authorKind || prev.guestId !== m.guestId || prev.author !== m.author) return true
  return (Date.parse(m.createdAt) || 0) - (Date.parse(prev.createdAt) || 0) > GROUP_MS
}

const EVENT_ICON = { control: '🎮', join: '👋', leave: '🚪', lobby: '🛎️', 'expiry-warning': '⏰', end: '🏁', start: '▶️', browser: '🌐', link: '🔗', action: '🛠️' }

export default function SessionPanel({ onClose }) {
  const s = useSession()
  const p = s.presence
  const { left, text } = useCountdown(p?.expiresAt)
  const [draft, setDraft] = useState('')
  const [copied, setCopied] = useState(false)
  const [picker, setPicker] = useState(false)
  const [dialog, ask] = useDialog()
  const sound = useSyncExternalStore(subscribeSounds, soundsEnabled, () => true)
  const listRef = useRef(null)
  const inputRef = useRef(null)
  const { markRead } = s

  // Follow the newest message — unless the reader scrolled up to look at an old one.
  const pinned = useRef(true)
  useEffect(() => {
    const el = listRef.current
    if (el && pinned.current) el.scrollTop = el.scrollHeight
    markRead()
  }, [s.messages.length, markRead])

  // The box grows with what is typed, up to a few lines.
  useEffect(() => {
    const el = inputRef.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(el.scrollHeight, 132)}px`
  }, [draft])

  const send = (e) => {
    e?.preventDefault()
    const body = withEmoji(draft.trim())
    if (!body) return
    s.sendChat(body)
    setDraft('')
    setPicker(false)
    pinned.current = true
  }
  const onKey = (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) send(e)
    if (e.key === 'Escape') setPicker(false)
  }
  const insert = (emoji) => {
    const el = inputRef.current
    const at = el ? el.selectionStart ?? draft.length : draft.length
    const end = el ? el.selectionEnd ?? at : at
    const next = draft.slice(0, at) + emoji + draft.slice(end)
    if (next.length > 2000) return
    setDraft(next)
    requestAnimationFrame(() => { el?.focus(); el?.setSelectionRange(at + emoji.length, at + emoji.length) })
  }
  const newLink = async () => {
    const yes = await ask.confirm({
      title: 'Issue a new invitation link?',
      body: 'The current link stops working for anyone not already in the session. Guests who are in stay in.',
      confirmLabel: 'Issue new link', icon: <Icon.Link size={15} />,
    })
    if (yes) await s.newLink()
  }
  const endSession = async () => {
    const yes = await ask.confirm({
      title: 'End the session for everyone?',
      body: 'Every guest is disconnected and shared terminals close. The transcript is kept on the stack.',
      confirmLabel: 'End session', danger: true,
    })
    if (yes) s.end()
  }
  const removeGuest = async (g) => {
    const yes = await ask.confirm({
      title: `Remove ${g.name}?`,
      body: 'They are disconnected at once. Coming back takes a new invitation link.',
      confirmLabel: 'Remove', danger: true,
    })
    if (yes) s.remove(g.id)
  }
  const copy = async () => {
    try { await navigator.clipboard.writeText(s.link); setCopied(true); setTimeout(() => setCopied(false), 1500) } catch { /* */ }
  }
  // Leaving is final for this link (app/share.go), so it is confirmed, and then the
  // tab goes to DBCanvas's sign-in page. A leave the server did not take keeps the
  // guest where they are, told why, rather than looking as if it worked.
  const leave = async () => {
    const yes = await ask.confirm({
      title: 'Leave the shared session?',
      body: 'You stop seeing the host\u2019s workspace. To come back you will need a new invitation link from the host.',
      confirmLabel: 'Leave', danger: true, icon: <Icon.Logout size={15} />,
    })
    if (!yes) return
    try {
      await shareApi.leave(joinToken())
    } catch (e) {
      s.notice(`Could not leave: ${e.message}`)
      return
    }
    location.replace('/?left=1')
  }

  const guests = p?.guests || []
  const waiting = guests.filter((g) => g.state === 'waiting')
  const inRoom = guests.filter((g) => g.state === 'admitted')
  const controller = p?.controller ?? 0
  const low = left > 0 && left < 10 * 60 * 1000

  if (s.ended && s.isHost) {
    return (
      <aside className="flex w-[22rem] shrink-0 flex-col border-l bg-surface">
        <div className="flex items-center gap-2 border-b px-3 py-2">
          <Icon.Share size={16} />
          <span className="flex-1 text-sm font-semibold">Session ended</span>
        </div>
        <div className="space-y-3 p-3 text-sm">
          <p className="text-muted">Every guest was disconnected. Download the transcript now — one started from a stack is also kept in that stack&apos;s Share dialog.</p>
          <div className="flex gap-2">
            <a className="text-primary hover:underline" href={shareApi.transcriptURL(s.sid)}>Download transcript</a>
            <span className="text-muted">·</span>
            <a className="text-primary hover:underline" href={shareApi.transcriptURL(s.sid, 'json')}>JSON</a>
          </div>
          <Button variant="outline" onClick={s.reset}>Close</Button>
        </div>
        {dialog}
      </aside>
    )
  }

  return (
    <aside data-mirror-private className="flex w-[22rem] shrink-0 flex-col border-l bg-surface" aria-label="Shared session">
      {/* header */}
      <div className="flex items-center gap-2 border-b px-3 py-2">
        <Icon.Share size={16} />
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-semibold">Shared session</div>
          <div className="text-[13px] text-muted">
            {s.isGuest ? `${p?.host?.name || 'The host'}'s workspace` : `${inRoom.length} guest${inRoom.length === 1 ? '' : 's'}`}
            {' · '}
            <span className={low ? 'font-medium text-warning' : ''}>ends in {text}</span>
            {!s.connected && <span className="text-warning"> · reconnecting…</span>}
          </div>
        </div>
        {onClose && <button title="Hide the panel" onClick={onClose} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg"><Icon.Close size={14} /></button>}
      </div>

      {/* control */}
      <div className="space-y-2 border-b px-3 py-2.5 text-[13px]">
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
        {s.isHost ? (
          <label className="flex cursor-pointer items-start gap-1.5" title="Everyone sees exactly the driver's screen — menus, drags, dialogs, windows — instead of their own copy of the page">
            <input type="checkbox" className="mt-0.5" checked={s.mirror} onChange={(e) => s.setMirror(e.target.checked)} />
            <span>
              Mirror everything
              <span className="block text-xs text-muted">
                {s.mirror ? 'Everyone following sees exactly the driver’s screen.' : 'Show everyone the driver’s screen itself — context menus, drags, dialogs — not just the same page.'}
              </span>
            </span>
          </label>
        ) : s.mirror && (
          <div className="flex items-center gap-1.5 text-xs text-muted">
            <Icon.Monitor size={13} />
            {s.isDriver ? 'Mirror is on: everyone sees your screen as you see it.' : s.following ? 'Mirroring the driver’s screen.' : 'Mirror is on — tick Follow to see the driver’s screen.'}
          </div>
        )}
        <div className="flex flex-wrap gap-1.5">
          {s.isGuest && !s.isDriver && <Button variant="outline" onClick={s.requestControl}>Request control</Button>}
          {s.isGuest && s.isDriver && <Button variant="outline" onClick={s.releaseControl}>Hand control back</Button>}
          {s.isHost && controller !== 0 && <Button variant="outline" onClick={s.takeControl}>Take control back</Button>}
          {s.isGuest && <Button variant="subtle" onClick={leave}>Leave</Button>}
          {s.isHost && <Button variant="danger" onClick={endSession}>End session</Button>}
        </div>
        {s.isHost && s.requests.map((r) => (
          <div key={r.guestId} className="flex items-center gap-1.5 rounded-md bg-primary/10 px-2 py-1.5">
            <span className="flex-1"><span className="font-medium">{r.name}</span> asked for control</span>
            <button className="rounded px-1.5 py-0.5 font-medium text-primary hover:bg-primary/15" onClick={() => s.giveControl(r.guestId)}>Grant</button>
            <button className="rounded px-1.5 py-0.5 text-muted hover:bg-surface2" onClick={() => s.dismissRequest(r.guestId)}>Deny</button>
          </div>
        ))}
        {s.isHost && (s.link ? (
          <div className="flex items-center gap-1.5 rounded-md border bg-bg px-2 py-1">
            <Icon.Link size={13} className="shrink-0 text-muted" />
            <span className="min-w-0 flex-1 truncate font-mono text-xs" title={s.link}>{s.link}</span>
            <button onClick={copy} className="shrink-0 font-medium text-primary hover:underline">{copied ? 'Copied' : 'Copy link'}</button>
            <span className="text-muted">·</span>
            <button onClick={newLink} title="Replace the link; the old one stops working for anyone not already in" className="shrink-0 text-muted hover:text-fg hover:underline">New link</button>
          </div>
        ) : (
          <button onClick={newLink} className="flex w-full items-center gap-1.5 rounded-md border border-dashed px-2 py-1.5 text-left text-muted hover:bg-surface2 hover:text-fg">
            <Icon.Link size={13} /> Issue a new invitation link
          </button>
        ))}
      </div>

      {/* people */}
      <div className="max-h-56 overflow-y-auto border-b px-3 py-2 text-[13px]">
        {s.isHost && waiting.length > 0 && (
          <div className="mb-2 space-y-1">
            <div className="font-medium text-muted">Lobby</div>
            {waiting.map((g) => (
              <div key={g.id} className="flex items-center gap-1.5 rounded-md bg-warning/10 px-2 py-1.5">
                <div className="min-w-0 flex-1">
                  <div className="truncate font-medium">{g.name}</div>
                  <div className="truncate text-xs text-muted">{g.email} · {g.remoteAddr}</div>
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
                  <IconBtn title="Remove from the session" onClick={() => removeGuest(g)}><Icon.Close size={12} /></IconBtn>
                </>
              )}
            </Person>
          ))}
        </div>
      </div>

      {/* chat */}
      <div className="flex items-center gap-2 border-b px-3 py-1.5">
        <Icon.Chat size={14} className="text-muted" />
        <span className="flex-1 text-[13px] font-semibold">Chat</span>
        <button
          onClick={() => setSoundsEnabled(!sound)}
          title={sound ? 'Alert sounds on — click to mute' : 'Alert sounds off — click to turn on'}
          aria-pressed={sound}
          className={`rounded p-1 hover:bg-surface2 ${sound ? 'text-fg' : 'text-muted'}`}
        >
          {sound ? <Icon.Bell size={15} /> : <Icon.BellOff size={15} />}
        </button>
      </div>
      <div
        ref={listRef}
        onScroll={(e) => { const el = e.currentTarget; pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40 }}
        className="flex-1 overflow-y-auto px-3 py-3 text-sm leading-relaxed"
        role="log" aria-live="polite" aria-label="Session chat"
      >
        {s.messages.map((m, i) => {
          if (m.kind !== 'chat') {
            return (
              <div key={m.id} className={`my-2 flex items-start justify-center gap-1.5 text-center text-xs ${m.kind === 'expiry-warning' ? 'font-medium text-warning' : m.kind === 'action' ? 'text-fg/80' : 'text-muted'}`}>
                <span className={`inline-flex max-w-full items-start gap-1.5 rounded-full px-2.5 py-1 ${m.kind === 'expiry-warning' ? 'bg-warning/10' : 'bg-surface2/70'}`}>
                  <span aria-hidden>{EVENT_ICON[m.kind] || 'ℹ️'}</span>
                  <span className="break-words text-left">
                    {m.kind === 'action' ? <><span className="font-medium">{m.author}</span>: {m.body}</> : m.body}
                    <span className="ml-1.5 whitespace-nowrap opacity-70">{when(m.createdAt)}</span>
                  </span>
                </span>
              </div>
            )
          }
          const mine = !!s.me && m.authorKind !== 'system' && m.guestId === s.me.guestId
          const host = m.authorKind === 'host'
          const first = groupStart(s.messages[i - 1], m)
          const big = onlyEmoji(m.body)
          return (
            <div key={m.id} className={`flex gap-2 ${first ? 'mt-3' : 'mt-0.5'} ${mine ? 'flex-row-reverse' : ''}`}>
              <div className="w-8 shrink-0">{first && <Avatar name={m.author} host={host} />}</div>
              <div className={`flex min-w-0 max-w-[85%] flex-col ${mine ? 'items-end' : 'items-start'}`}>
                {first && (
                  <div className={`mb-0.5 flex items-baseline gap-1.5 ${mine ? 'flex-row-reverse' : ''}`}>
                    <span className={`text-[13px] font-semibold ${host ? 'text-primary' : 'text-fg'}`}>{mine ? 'You' : m.author}</span>
                    {host && <span className="rounded bg-primary/15 px-1 text-[11px] font-medium text-primary">host</span>}
                    <span className="text-[11px] text-muted">{when(m.createdAt)}</span>
                  </div>
                )}
                <div
                  title={first ? undefined : when(m.createdAt)}
                  className={big
                    ? 'whitespace-pre-wrap break-words text-3xl leading-tight'
                    : `whitespace-pre-wrap break-words rounded-2xl px-3 py-1.5 text-fg ${mine ? 'rounded-tr-md bg-primary/15' : 'rounded-tl-md bg-surface2'}`}
                >
                  {m.body}
                </div>
              </div>
            </div>
          )
        })}
        {s.messages.length === 0 && (
          <div className="flex h-full flex-col items-center justify-center gap-1 text-center text-muted">
            <span className="text-2xl" aria-hidden>💬</span>
            <span>No messages yet. Say hello 👋</span>
          </div>
        )}
      </div>
      <form onSubmit={send} className="relative border-t p-2">
        {picker && (
          <div className="absolute bottom-full left-2 right-2 mb-1 max-h-64 overflow-y-auto rounded-lg border bg-surface p-2 shadow-lg" role="dialog" aria-label="Emoji">
            {EMOJI_GROUPS.map((g) => (
              <div key={g.name} className="mb-1.5 last:mb-0">
                <div className="mb-0.5 px-1 text-[11px] font-medium uppercase tracking-wide text-muted">{g.name}</div>
                <div className="grid grid-cols-8 gap-0.5">
                  {g.items.map((e) => (
                    <button key={e} type="button" onClick={() => insert(e)} className="rounded-md p-1 text-xl leading-none hover:bg-surface2" title={e}>{e}</button>
                  ))}
                </div>
              </div>
            ))}
            <div className="mt-1 border-t px-1 pt-1.5 text-[11px] text-muted">Tip: <span className="font-mono">:)</span> <span className="font-mono">:D</span> <span className="font-mono">:+1:</span> <span className="font-mono">:tada:</span> turn into emoji when sent.</div>
          </div>
        )}
        <div className="flex items-end gap-1.5 rounded-xl border bg-bg px-1.5 py-1 focus-within:border-primary/60 focus-within:ring-2 focus-within:ring-primary/20">
          <button type="button" onClick={() => setPicker((v) => !v)} title="Emoji" aria-expanded={picker}
            className={`mb-0.5 rounded-md p-1.5 hover:bg-surface2 ${picker ? 'text-primary' : 'text-muted'}`}>
            <Icon.Smile size={18} />
          </button>
          <textarea
            ref={inputRef} rows={1}
            value={draft} onChange={(e) => setDraft(e.target.value)} onKeyDown={onKey} maxLength={2000}
            placeholder="Message everyone…"
            aria-label="Message everyone"
            className="max-h-32 min-h-[2.25rem] min-w-0 flex-1 resize-none bg-transparent px-1 py-1.5 text-sm leading-snug outline-none placeholder:text-muted"
          />
          <button type="submit" disabled={!draft.trim()} title="Send (Enter)"
            className="mb-0.5 rounded-lg bg-primary p-1.5 text-primary-fg transition hover:opacity-90 disabled:opacity-40">
            <Icon.Send size={16} />
          </button>
        </div>
        <div className="mt-1 flex justify-between px-1 text-[11px] text-muted">
          <span>Enter to send · Shift+Enter for a new line</span>
          {draft.length > 1800 && <span className={draft.length >= 2000 ? 'text-danger' : ''}>{draft.length}/2000</span>}
        </div>
      </form>
      {dialog}
    </aside>
  )
}

function Person({ name, sub, badge, online, driving, muted, children }) {
  return (
    <div className="group flex items-center gap-2 rounded-md px-1 py-1">
      <span className="relative shrink-0">
        <Avatar name={name} host={badge === 'host'} size="sm" />
        <span className={`absolute -bottom-0.5 -right-0.5 h-2.5 w-2.5 rounded-full border-2 border-surface ${online ? 'bg-success' : 'bg-muted'}`} title={online ? 'online' : 'offline'} />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-1 truncate">
          <span className="truncate font-medium">{name || '…'}</span>
          <span className={`rounded px-1 text-[11px] ${badge === 'host' ? 'bg-primary/15 text-primary' : 'bg-surface2 text-muted'}`}>{badge}</span>
          {driving && <span className="rounded bg-success/15 px-1 text-[11px] text-success">driving</span>}
          {muted && <span className="rounded bg-warning/15 px-1 text-[11px] text-warning">muted</span>}
        </div>
        {sub && <div className="truncate text-xs text-muted">{sub}</div>}
      </div>
      <div className="flex shrink-0 items-center gap-0.5 opacity-60 group-hover:opacity-100">{children}</div>
    </div>
  )
}

function IconBtn({ title, onClick, children }) {
  return <button title={title} onClick={onClick} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">{children}</button>
}
