import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { shareApi } from '../lib/shareApi.js'
import { useAuth } from '../auth/AuthProvider.jsx'

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
// and one thing it does on its own: when the driver's browser changes something on
// the server, it tells everyone else to re-read ('dbcanvas:invalidate', which App
// turns into the active tab's Refresh).
//
// Outside a session every value is inert, so a page may call these unconditionally.

const SessionCtx = createContext(null)

const INERT = {
  active: false, sid: null, me: null, presence: null, messages: [], isDriver: true, isHost: false,
  isGuest: false, following: false, follow: null, cursor: null, terms: [], browsers: [], ended: null, requests: [],
  connected: false, link: '',
  setFollowing: () => {}, publishFollow: () => {}, publishCursor: () => {}, sendChat: () => {},
  requestControl: () => {}, releaseControl: () => {}, takeControl: async () => {}, giveControl: async () => {},
  admit: async () => {}, deny: async () => {}, remove: async () => {}, mute: async () => {}, end: async () => {},
  start: async () => { throw new Error('no session') }, openSharedTerm: async () => {}, closeSharedTerm: async () => {},
  shareBrowser: () => {},
  notice: () => {}, dismissRequest: () => {}, reset: () => {},
}

export function useSession() {
  return useContext(SessionCtx) || INERT
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
  const [following, setFollowing] = useState(true)

  const wsRef = useRef(null)
  const followRef = useRef({})
  const followTimer = useRef(null)
  const lastCursor = useRef(0)

  const isGuest = !!guest
  const isHost = !!user && !isGuest && !!sid
  const controller = presence?.controller ?? 0
  const myId = me ? me.guestId : isGuest ? guest.guestId : 0
  const isDriver = !!sid && controller === myId
  const driverRef = useRef(isDriver)
  driverRef.current = isDriver

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
            if (m.follow) setFollowData(m.follow)
            break
          case 'presence':
            setPresence(m)
            break
          case 'message':
            setMessages((ms) => [...ms.slice(-499), m.message])
            break
          case 'follow':
            setFollowData(m.data)
            break
          case 'cursor':
            setCursor({ ...m.data, from: m.from, at: Date.now() })
            break
          case 'invalidate':
            window.dispatchEvent(new CustomEvent('dbcanvas:invalidate', { detail: m.data || {} }))
            break
          case 'control-request':
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

  const value = useMemo(() => ({
    active: !!sid && !ended, sid, me, presence, messages, follow, cursor, terms, browsers, ended, requests, connected, link,
    isDriver: !sid || isDriver, isHost, isGuest, following: following && !isDriver,
    controllerName: controller === 0 ? presence?.host?.name : presence?.guests?.find((g) => g.id === controller)?.name,
    setFollowing,
    publishFollow, publishCursor,
    notice: localNotice,
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
    // start begins a session on a stack and returns its link; the link is shown once.
    start: async (stackId, minutes, hideSecrets) => {
      const r = await shareApi.start(stackId, minutes, hideSecrets)
      setEnded(null); setMessages([]); setPresence(null); setTerms([]); setBrowsers([]); setRequests([])
      setLink(r.url)
      setSid(r.session.id)
      return r.url
    },
    // A host who ends a session goes back to working alone.
    reset: () => { setSid(null); setEnded(null); setLink(''); setPresence(null); setMessages([]); setTerms([]) },
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
  }), [sid, ended, me, presence, messages, follow, cursor, terms, browsers, requests, connected, link, isDriver, isHost, isGuest,
    following, controller, publishFollow, publishCursor, localNotice, send, hostCall])

  return <SessionCtx.Provider value={value}>{children}</SessionCtx.Provider>
}
