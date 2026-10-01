import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { record, Replayer, EventType } from 'rrweb'
import 'rrweb/dist/style.css'
import { Icon } from '../components/Icons.jsx'
import { useSession } from './SessionProvider.jsx'
import { maskSecrets } from '../lib/secretRegistry.js'
import { startIframeCanvasSampler } from './mirrorCanvas.js'

// Mirror.jsx — "Mirror everything" (app/sharemirror.go).
//
// Following moves everyone to the driver's page, stack and panel, but each browser
// still draws its own copy, so anything that is not followed state — the context menu
// the driver opened, the window they are dragging, a dialog, a hover, what they are
// typing — never reaches anyone else, and a guest loses track of what they are being
// shown. Mirroring sends the screen instead of the state.
//
// MirrorRecorder runs in the driver's browser: rrweb records the page (a snapshot of
// the DOM, then every change to it, the pointer, scrolls, inputs, and canvases a few
// times a second — a VNC desktop is one) and the batches go out over the session's
// socket. MirrorView runs everywhere else: it replays that stream, live and
// read-only, over the viewer's own workspace. The viewer's own workspace keeps
// following underneath, so turning the mirror off lands them where the driver is.
//
// What a guest may not see is kept out at the source, in the driver's browser:
//   - [data-mirror-private] is recorded as an empty box: the host-only pages
//     (Settings, API tokens, Users), the account and notification menus, and the
//     driver's own session panel — every viewer has their own.
//   - With Hide secrets on, the masked secret fields are masked in the stream too,
//     and every secret value they were handed is blanked wherever else it is drawn
//     (lib/secretRegistry.js). Password inputs are always masked.

const PRIVATE = '[data-mirror-private]'
const SECRET = '[data-secret]'
const BATCH_MS = 80

// MirrorRecorder records only while there is someone to send to.
export function MirrorRecorder() {
  const s = useSession()
  const p = s.presence
  const myId = s.me?.guestId ?? null
  const watched = !!p && (
    (p.host?.online && myId !== 0) ||
    (p.guests || []).some((g) => g.online && g.state === 'admitted' && g.id !== myId))
  const on = s.active && s.mirror && s.isDriver && s.connected && watched
  const hide = !!p?.hideSecrets
  const api = useRef(s)
  api.current = s

  useEffect(() => {
    if (!on) return undefined
    let buf = []
    let timer = null
    const flush = () => {
      clearTimeout(timer)
      timer = null
      if (buf.length) { api.current.sendMirror(buf); buf = [] }
    }
    const stop = record({
      emit(e) {
        buf.push(e)
        // A snapshot goes at once: a viewer is waiting on it.
        if (e.type === EventType.FullSnapshot) flush()
        else if (!timer) timer = setTimeout(flush, BATCH_MS)
      },
      blockSelector: PRIVATE,
      // rrweb hands maskInputFn only the inputs maskInputOptions selects, so with Hide
      // secrets on every text field goes through it, to blank a known secret typed
      // or pasted into one.
      maskInputOptions: hide ? { password: true, text: true, textarea: true, search: true, url: true, email: true } : { password: true },
      ...(hide ? {
        maskTextSelector: '*',
        maskTextFn: (text, el) => (el?.closest?.(SECRET) ? '•'.repeat(text.length) : maskSecrets(text)),
        maskInputFn: (text, el) => (el?.type === 'password' || el?.closest?.(SECRET) ? '•'.repeat(text.length) : maskSecrets(text)),
      } : {}),
      recordCanvas: true,
      sampling: { canvas: 3, mousemove: 40, scroll: 100, input: 'last' },
      dataURLOptions: { type: 'image/webp', quality: 0.6 },
      inlineStylesheet: true,
    })
    // rrweb samples only the top page's canvases; a VNC desktop is a canvas inside a
    // browser window's iframe, so those are sampled here (session/mirrorCanvas.js).
    const canvases = startIframeCanvasSampler((e) => {
      buf.push(e)
      if (!timer) timer = setTimeout(flush, BATCH_MS)
    }, { fps: 3, blocked: PRIVATE })
    const unsub = api.current.onMirror((m) => {
      if (m.t === 'mirror-resync') { canvases.resync(); record.takeFullSnapshot(true) }
    })
    return () => {
      unsub()
      canvases.stop()
      stop?.()
      flush()
    }
  }, [on, hide])

  return null
}

// MirrorView is the driver's screen, scaled to fit the space the viewer's workspace
// has (everything but their own session panel), with a strip that says whose it is.
export function MirrorView({ right = 0, onShowPanel }) {
  const s = useSession()
  const stage = useRef(null)
  const root = useRef(null)
  const [size, setSize] = useState(null) // the driver's viewport, { width, height }
  const [box, setBox] = useState({ width: 0, height: 0 })
  const [waiting, setWaiting] = useState(true)
  const api = useRef(s)
  api.current = s

  useEffect(() => {
    if (!s.mirroring) return undefined
    let rp = null
    // Each snapshot (it opens with a Meta event) starts a fresh replay: a resync, a
    // new driver, or the driver reloading.
    const begin = (events) => {
      try { rp?.destroy() } catch { /* already gone */ }
      if (root.current) root.current.innerHTML = ''
      rp = new Replayer([], {
        root: root.current,
        liveMode: true,
        mouseTail: false,
        UNSAFE_replayCanvas: true,
        triggerFocus: false,
        showWarning: false,
        // The replay shows where the driver's page IS, so no animation of its own: a
        // fade-in replayed from its first frame can sit at opacity 0. And it must not
        // grow scrollbars of its own on top of the driver's.
        insertStyleRules: [
          '*, *::before, *::after { animation: none !important; transition: none !important; }',
          'html, body { overflow: hidden !important; }',
        ],
      })
      rp.on('resize', (d) => setSize({ width: d.width, height: d.height }))
      rp.startLive(events[0].timestamp - 200)
      for (const e of events) {
        if (e.type === EventType.Meta) setSize({ width: e.data.width, height: e.data.height })
        rp.addEvent(e)
      }
      setWaiting(false)
    }
    const unsub = api.current.onMirror((m) => {
      if (m.t !== 'mirror' || !Array.isArray(m.data)) return
      const i = m.data.findIndex((e) => e.type === EventType.Meta)
      if (i >= 0) begin(m.data.slice(i))
      else if (rp) for (const e of m.data) rp.addEvent(e)
      else api.current.requestResync()
    })
    setWaiting(true)
    api.current.requestResync()
    return () => {
      unsub()
      try { rp?.destroy() } catch { /* */ }
      rp = null
    }
  }, [s.mirroring])

  // A new driver means a new screen to wait for.
  const driver = s.controllerName
  useEffect(() => { if (s.mirroring) { setWaiting(true); api.current.requestResync() } }, [driver]) // eslint-disable-line react-hooks/exhaustive-deps

  useLayoutEffect(() => {
    const el = stage.current
    if (!el) return undefined
    const ro = new ResizeObserver(() => setBox({ width: el.clientWidth, height: el.clientHeight }))
    ro.observe(el)
    return () => ro.disconnect()
  }, [s.mirroring])

  if (!s.mirroring) return null
  const k = size && box.width ? Math.min(box.width / size.width, box.height / size.height, 1.5) : 1
  return (
    <div className="fixed inset-y-0 left-0 z-[9000] flex flex-col bg-bg" style={{ right }} data-mirror-view>
      <div className="flex shrink-0 items-center gap-2 border-b bg-primary/15 px-3 py-1.5 text-xs text-primary">
        <Icon.Monitor size={14} />
        <span className="flex-1 truncate">
          <b>{driver || 'The driver'}</b>&apos;s screen, mirrored{size ? ` · ${size.width}×${size.height}` : ''} — you see exactly what they see, and cannot click it.
        </span>
        {onShowPanel && (
          <button onClick={onShowPanel} className="rounded px-2 py-0.5 font-medium hover:bg-primary/15">
            <Icon.Chat size={13} className="mr-1 inline" />Session
          </button>
        )}
        <button onClick={() => s.setFollowing(false)} title="Back to your own view of the workspace. Tick Follow in the session panel to come back."
          className="rounded px-2 py-0.5 font-medium hover:bg-primary/15">
          Stop mirroring
        </button>
      </div>
      <div ref={stage} className="relative min-h-0 flex-1 overflow-hidden bg-black/70">
        {waiting && (
          <div className="absolute inset-0 flex items-center justify-center text-sm text-white/80">
            Waiting for {driver || 'the driver'}&apos;s screen…
          </div>
        )}
        <div
          className="pointer-events-none absolute left-1/2 top-1/2"
          style={size ? { width: size.width, height: size.height, transform: `translate(-50%, -50%) scale(${k})` } : undefined}
        >
          <div ref={root} className="dbc-mirror h-full w-full" />
        </div>
      </div>
    </div>
  )
}
