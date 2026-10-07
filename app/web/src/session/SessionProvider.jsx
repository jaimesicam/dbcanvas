import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { shareApi } from '../lib/shareApi.js'
import { useAuth } from '../auth/AuthProvider.jsx'
import { playSound } from '../lib/sessionSounds.js'
import { recorder } from './recorder.js'

// SessionProvider — this browser's side of a shared session (app/share.go,
// app/sharehub.go).
//
// The host and every guest run the same provider over the same socket. It holds
// what the hub says — who is here, who drives, the chat, the shared terminals — and
// gives pages three things to do with it:
//
//   publishFollow(patch)  the driver says where they are (page, stack, viewport…)
//   publishCursor(point)  the driver's pointer, in canvas coordinates
//   follow / cursor       what a follower reads to go where the driver is
//
// and, with the host's "Mirror everything" on, the driver's screen itself
// (session/Mirror.jsx): sendMirror(events) from the driver, onMirror(fn) for
// everyone else, and requestResync() when a viewer needs a fresh snapshot.
//
// and the drawings on the screen (session/DrawLayer.jsx, app/sharedraw.go): marks,
// what everyone drew; drawMark / eraseMarks / clearMarks to change them; drawing, the
// pen (tool, colour, size) and canDraw for this browser's own toolbar.
//
// and one thing it does on its own: when the driver's browser changes something on
// the server, it tells everyone else to re-read ('dbcanvas:invalidate', which App
// turns into the active tab's Refresh).
//
// It also chimes (lib/sessionSounds.js) for what someone should not miss while looking
// at the canvas: a message from someone else, a guest in the lobby or asking for
// control, control handed to you, the clock, the end — and counts the messages that
// arrived while the panel was hidden (unread, markRead).
//
// Outside a session every value is inert, so a page may call these unconditionally.

const SessionCtx = createContext(null)

const INERT = {
  active: false, sid: null, me: null, presence: null, messages: [], isDriver: true, isHost: false,
  isGuest: false, following: false, follow: null, cursor: null, terms: [], browsers: [], ended: null, requests: [],
  connected: false, link: '', unread: 0,
  publishFollow: () => {}, publishUI: () => {}, publishCursor: () => {}, sendChat: () => {},
  requestControl: () => {}, releaseControl: () => {}, takeControl: async () => {}, giveControl: async () => {},
  admit: async () => {}, deny: async () => {}, remove: async () => {}, mute: async () => {}, end: async () => {},
  start: async () => { throw new Error('no session') }, openSharedTerm: async () => {}, closeSharedTerm: async () => {},
  shareBrowser: () => {},
  mirror: false, mirroring: false, setMirror: async () => {}, sendMirror: () => {}, onMirror: () => () => {}, requestResync: () => {},
  notice: () => {}, dismissRequest: () => {}, reset: () => {}, markRead: () => {}, newLink: async () => {},
  marks: [], drawing: false, setDrawing: () => {}, pen: { tool: 'pen', color: '#ef4444', size: 'm' }, setPen: () => {},
  canDraw: false, drawMark: () => {}, eraseMarks: () => {}, clearMarks: () => {},
  setGuestsDraw: async () => {}, setGuestDraw: async () => {}, recording: null,
}

export function useSession() {
  return useContext(SessionCtx) || INERT
}

// useFollowedState is useState for a piece of UI that followers should see change
// with the driver: which tab a node's Properties show, which pane is open. The driver's
// value is published under `key` with the rest of the follow state (so a guest who
// arrives later lands on it too); while following, the value is the driver's.
//
// Key it by what it belongs to — `tab:${nodeId}` — so two panels never share one.
// Outside a session it is plain useState.
export function useFollowedState(key, initial) {
  const s = useSession()
  const theirs = s.following ? s.follow?.ui?.[key] : undefined
  const [value, setValue] = useState(() => (theirs !== undefined ? theirs : initial))
  const valueRef = useRef(value)
  valueRef.current = value
  useEffect(() => {
    if (theirs !== undefined) setValue(theirs)
  }, [theirs])
  // Whoever drives says where they are as soon as the panel shows, or as soon as they
  // take control, not only on their next click.
  const { active, isDriver, publishUI } = s
  useEffect(() => {
    if (active && isDriver) publishUI(key, valueRef.current)
  }, [active, isDriver, key, publishUI])
  const set = useCallback((next) => {
    const v = typeof next === 'function' ? next(valueRef.current) : next
    valueRef.current = v
    setValue(v)
    publishUI(key, v)
  }, [key, publishUI])
  return [value, set]
}

// Exported for the render checks: mount a component as a guest, a driver, a host.
export const SessionContext = SessionCtx

const MUTATING = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])

export function SessionProvider({ children }) {
  const { user, guest } = useAuth()
  const [sid, setSid] = useState(guest?.sessionId || null)
  const [link, setLink] = useState('')
  const [me, setMe] = useState(null)
  const [presence, setPresence] = useState(null)
  const [messages, setMessages] = useState([])
  const [follow, setFollowData] = useState(null)
  const [cursor, setCursor] = useState(null)
  const [terms, setTerms] = useState([])
  // Shared browser windows (browser/BrowserProvider.jsx): { id, link, title, path, opener }.
  const [browsers, setBrowsers] = useState([])
  const [ended, setEnded] = useState(null)
  const [requests, setRequests] = useState([])
  const [connected, setConnected] = useState(false)
  const [unread, setUnread] = useState(0)
  // What is drawn on the screen, oldest first, and this browser's own pen.
  const [marks, setMarks] = useState([])
  const [drawing, setDrawing] = useState(false)
  const [pen, setPen] = useState({ tool: 'pen', color: '#ef4444', size: 'm' })

  const wsRef = useRef(null)
  const followRef = useRef({})
  const followTimer = useRef(null)
  const lastCursor = useRef(0)
  // The mirror stream is far too busy for React state: it goes straight to whoever
  // subscribed (the recorder for resyncs, the replayer for events).
  const mirrorSubs = useRef(new Set())
  const emitMirror = (m) => { for (const fn of mirrorSubs.current) fn(m) }

  const isGuest = !!guest
  const isHost = !!user && !isGuest && !!sid
  const controller = presence?.controller ?? 0
  const myId = me ? me.guestId : isGuest ? guest.guestId : 0
  const isDriver = !!sid && controller === myId
  const driverRef = useRef(isDriver)
  driverRef.current = isDriver
  const myIdRef = useRef(myId)
  myIdRef.current = myId
  const hostRef = useRef(isHost)
  hostRef.current = isHost

  // A host finds their live session again after a reload.
  useEffect(() => {
    if (isGuest || !user) return
    let alive = true
    shareApi.live().then((list) => { if (alive && list?.length && !sid) setSid(list[0].id) }).catch(() => {})
    return () => { alive = false }
  }, [isGuest, user]) // eslint-disable-line react-hooks/exhaustive-deps

  const send = useCallback((msg) => {
    const ws = wsRef.current
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg))
  }, [])

  const localNotice = useCallback((body) => {
    setMessages((ms) => [...ms, { id: `local-${Date.now()}`, kind: 'notice', authorKind: 'system', body, createdAt: new Date().toISOString() }])
  }, [])

  // The socket, with a reconnect until the session ends.
  useEffect(() => {
    if (!sid || ended) return undefined
    let live = true
    let timer = null
    const connect = () => {
      if (!live) return
      const ws = new WebSocket(shareApi.wsURL(sid))
      wsRef.current = ws
      ws.onopen = () => {
        setConnected(true)
        // Where the driver is, again on every (re)connect: anything published before
        // the socket was open was dropped, and a guest arriving must not wait for
        // the driver's next click to find them.
        if (driverRef.current) ws.send(JSON.stringify({ t: 'follow', data: followRef.current }))
      }
      ws.onclose = () => {
        setConnected(false)
        if (live) timer = setTimeout(connect, 2000)
      }
      ws.onmessage = (e) => {
        let m
        try { m = JSON.parse(e.data) } catch { return }
        switch (m.t) {
          case 'hello':
            setMe(m.you)
            setMessages(m.history || [])
            setTerms(m.terms || [])
            setBrowsers(m.browsers || [])
            setMarks(m.marks || [])
            if (m.follow) setFollowData(m.follow)
            break
          case 'draw':
            setMarks((ms) => upsertMark(ms, m.mark))
            break
          case 'draw-erase': {
            const gone = new Set(m.ids || [])
            setMarks((ms) => ms.filter((x) => !gone.has(x.id)))
            break
          }
          case 'presence':
            setPresence(m)
            break
          case 'message': {
            const msg = m.message
            setMessages((ms) => [...ms.slice(-499), msg])
            const mine = msg.authorKind !== 'system' && msg.guestId === myIdRef.current
            if (msg.kind === 'chat' && !mine) { playSound('message'); setUnread((n) => n + 1) }
            else if (msg.kind === 'join' && !mine) playSound('join')
            else if (msg.kind === 'expiry-warning') playSound('warning')
            break
          }
          case 'follow':
            setFollowData(m.data)
            break
          case 'cursor':
            setCursor({ ...m.data, from: m.from, at: Date.now() })
            break
          case 'mirror':
          case 'mirror-resync':
            emitMirror(m)
            break
          case 'invalidate':
            window.dispatchEvent(new CustomEvent('dbcanvas:invalidate', { detail: m.data || {} }))
            break
          case 'control-request':
            if (hostRef.current) playSound('request')
            setRequests((rs) => (rs.some((r) => r.guestId === m.guestId) ? rs : [...rs, { guestId: m.guestId, name: m.name }]))
            break
          case 'terminal-open':
            setTerms((ts) => (ts.some((t) => t.id === m.terminal.id) ? ts : [...ts, m.terminal]))
            break
          case 'browser-open':
            setBrowsers((bs) => (bs.some((b) => b.id === m.browser.id) ? bs : [...bs, m.browser]))
            break
          case 'browser-nav':
            setBrowsers((bs) => bs.map((b) => (b.id === m.browser.id ? { ...b, path: m.browser.path } : b)))
            break
          case 'browser-close':
            setBrowsers((bs) => bs.filter((b) => b.id !== m.browser.id))
            break
          case 'terminal-close':
            setTerms((ts) => ts.filter((t) => t.id !== m.id))
            break
          case 'error':
            localNotice(m.error)
            break
          case 'end':
            // The session is over: so is its recording, and what was drawn on it.
            if (recorder.get().sid === sid) recorder.stop()
            setMarks([])
            setDrawing(false)
            playSound('end')
            setEnded(m.reason || 'ended')
            live = false
            break
          default:
        }
      }
    }
    connect()
    return () => {
      live = false
      if (timer) clearTimeout(timer)
      wsRef.current?.close()
      wsRef.current = null
    }
  }, [sid, ended, localNotice])

  // A guest arriving in the lobby, for the host; control arriving, for whoever gets it.
  const waitingCount = presence?.guests?.filter((g) => g.state === 'waiting').length ?? 0
  const lastWaiting = useRef(0)
  useEffect(() => {
    if (isHost && waitingCount > lastWaiting.current) playSound('lobby')
    lastWaiting.current = waitingCount
  }, [isHost, waitingCount])
  const lastController = useRef(null)
  useEffect(() => {
    if (!presence) return
    if (lastController.current !== null && lastController.current !== controller && controller === myId) playSound('control')
    lastController.current = controller
  }, [presence, controller, myId])

  // Control requests are answered by control moving; a stale one is dropped.
  useEffect(() => {
    if (controller) setRequests((rs) => rs.filter((r) => r.guestId !== controller))
  }, [controller])

  // Becoming the driver: say where you are straight away, so nobody waits for your
  // next click to find you.
  useEffect(() => {
    if (isDriver && sid) send({ t: 'follow', data: followRef.current })
  }, [isDriver, sid, send])

  // The driver's writes, seen from the one place they all pass: re-read, everyone.
  useEffect(() => {
    if (!sid) return undefined
    const orig = window.fetch
    window.fetch = async (input, init = {}) => {
      const res = await orig(input, init)
      try {
        const method = (init.method || (input instanceof Request ? input.method : 'GET')).toUpperCase()
        const url = new URL(typeof input === 'string' ? input : input.url || String(input), location.href)
        if (res.ok && MUTATING.has(method) && url.pathname.startsWith('/api/') &&
            !url.pathname.startsWith('/api/share/') && !url.pathname.startsWith('/api/join/') && driverRef.current) {
          send({ t: 'invalidate', data: { method, path: url.pathname } })
        }
      } catch { /* observing only */ }
      return res
    }
    return () => { window.fetch = orig }
  }, [sid, send])

  const publishFollow = useCallback((patch) => {
    followRef.current = { ...followRef.current, ...patch }
    if (!driverRef.current) return
    clearTimeout(followTimer.current)
    followTimer.current = setTimeout(() => send({ t: 'follow', data: followRef.current }), 80)
  }, [send])

  // publishUI is publishFollow for one key of the followed UI state (useFollowedState):
  // merged into `ui`, so one panel's tab does not wipe another's.
  const publishUI = useCallback((key, v) => {
    publishFollow({ ui: { ...(followRef.current.ui || {}), [key]: v } })
  }, [publishFollow])

  const publishCursor = useCallback((pt) => {
    if (!driverRef.current) return
    const now = Date.now()
    if (now - lastCursor.current < 60) return
    lastCursor.current = now
    send({ t: 'cursor', data: pt })
  }, [send])

  const hostCall = useCallback(async (fn) => {
    try { await fn() } catch (e) { localNotice(e.message) }
  }, [localNotice])

  // Who may draw: the host always; a guest unless the host turned it off for
  // everyone or for them (app/sharedraw.go, which decides — this only draws the UI).
  const canDraw = !!sid && !ended && (isHost || (!!presence?.guestsDraw &&
    !presence?.guests?.find((g) => g.id === myId)?.drawOff))
  useEffect(() => { if (!canDraw) setDrawing(false) }, [canDraw])

  const mirror = !!presence?.mirror
  const value = useMemo(() => ({
    marks, drawing, setDrawing, pen, setPen, canDraw,
    recording: presence?.recording || null,
    // drawMark draws, or redraws a line as it grows; the hub tells everyone else.
    drawMark: (mark) => {
      setMarks((ms) => upsertMark(ms, { ...mark, by: myIdRef.current, name: me?.name || '' }))
      send({ t: 'draw', data: mark })
    },
    eraseMarks: (ids) => {
      if (!ids.length) return
      const gone = new Set(ids)
      setMarks((ms) => ms.filter((x) => !gone.has(x.id) || (!hostRef.current && x.by !== myIdRef.current)))
      send({ t: 'draw-erase', data: { ids } })
    },
    // clearMarks: your own drawings, or — the host only — everyone's.
    clearMarks: (all) => send({ t: 'draw-clear', data: { all: !!all } }),
    setGuestsDraw: (on) => hostCall(() => shareApi.setGuestsDraw(sid, on)),
    setGuestDraw: (gid, on) => hostCall(() => shareApi.setGuestDraw(sid, gid, on)),
    mirror,
    // mirroring: this browser shows the driver's screen rather than its own.
    mirroring: !!sid && !ended && mirror && !isDriver,
    setMirror: (on) => hostCall(() => shareApi.setMirror(sid, on)),
    sendMirror: (events) => send({ t: 'mirror', data: events }),
    onMirror: (fn) => { mirrorSubs.current.add(fn); return () => mirrorSubs.current.delete(fn) },
    requestResync: () => send({ t: 'mirror-resync' }),
    active: !!sid && !ended, sid, me, presence, messages, follow, cursor, terms, browsers, ended, requests, connected, link, unread,
    // Everyone who is not driving follows the driver, always: a viewer who wandered
    // off on their own was a viewer lost to the session.
    isDriver: !sid || isDriver, isHost, isGuest, following: !!sid && !isDriver,
    controllerName: controller === 0 ? presence?.host?.name : presence?.guests?.find((g) => g.id === controller)?.name,
    publishFollow, publishUI, publishCursor,
    notice: localNotice,
    markRead: () => setUnread(0),
    sendChat: (body) => send({ t: 'chat', body }),
    requestControl: () => send({ t: 'control-request' }),
    releaseControl: () => send({ t: 'control-release' }),
    takeControl: () => hostCall(() => shareApi.control(sid, 'host')),
    giveControl: (gid) => hostCall(() => shareApi.control(sid, gid)),
    dismissRequest: (gid) => setRequests((rs) => rs.filter((r) => r.guestId !== gid)),
    admit: (gid) => hostCall(() => shareApi.admit(sid, gid)),
    deny: (gid) => hostCall(() => shareApi.deny(sid, gid)),
    remove: (gid) => hostCall(() => shareApi.remove(sid, gid)),
    mute: (gid, muted) => hostCall(() => shareApi.mute(sid, gid, muted)),
    end: () => hostCall(() => shareApi.end(sid)),
    // newLink replaces the link — the way to invite back a guest who left — and
    // returns the new one, shown once like the first.
    newLink: async () => {
      try {
        const r = await shareApi.newLink(sid)
        setLink(r.url)
        return r.url
      } catch (e) { localNotice(e.message); return '' }
    },
    // start begins a session — from a stack, or from anywhere with stackId null — and
    // returns its link; the link is shown once.
    start: async (stackId, minutes, hideSecrets, mirrorOn) => {
      const r = await shareApi.start(stackId, minutes, hideSecrets, mirrorOn)
      setEnded(null); setMessages([]); setPresence(null); setTerms([]); setBrowsers([]); setRequests([]); setMarks([])
      setLink(r.url)
      setSid(r.session.id)
      return r.url
    },
    // A host who ends a session goes back to working alone.
    reset: () => { setSid(null); setEnded(null); setLink(''); setPresence(null); setMessages([]); setTerms([]); setUnread(0) },
    // t is browser-open | browser-nav | browser-close; the opener keeps its own list
    // in step, since the hub tells everyone but the sender.
    shareBrowser: (t, b) => {
      send({ t, data: b })
      if (t === 'browser-open') setBrowsers((bs) => (bs.some((x) => x.id === b.id) ? bs : [...bs, b]))
      if (t === 'browser-close') setBrowsers((bs) => bs.filter((x) => x.id !== b.id))
      if (t === 'browser-nav') setBrowsers((bs) => bs.map((x) => (x.id === b.id ? { ...x, path: b.path } : x)))
    },
    openSharedTerm: (spec) => shareApi.openTerm(sid, spec),
    closeSharedTerm: (tid) => shareApi.closeTerm(sid, tid).catch(() => {}),
  }), [marks, drawing, pen, canDraw, mirror, sid, ended, me, presence, messages, follow, cursor, terms, browsers, requests, connected, link, unread, isDriver, isHost, isGuest,
    controller, publishFollow, publishUI, publishCursor, localNotice, send, hostCall])

  return <SessionCtx.Provider value={value}>{children}</SessionCtx.Provider>
}

// upsertMark replaces a mark by id — a line redrawn as it grows — or adds it on top.
function upsertMark(ms, mark) {
  const i = ms.findIndex((x) => x.id === mark.id)
  if (i < 0) return [...ms, mark]
  const next = ms.slice()
  next[i] = mark
  return next
}
