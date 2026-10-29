import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { Icon } from '../components/Icons.jsx'
import { useSettings } from '../settings/SettingsProvider.jsx'
import { useSession } from '../session/SessionProvider.jsx'
import { shareApi } from '../lib/shareApi.js'
import { Window, useWindowManager, useTaskbarItem } from '../wm/WindowManager.jsx'

// A top-level terminal manager. Because the provider (and its dock) live above
// the page switch, xterm instances + their WebSockets stay mounted across
// navigation — sessions are not reset when you leave and return to a page.
//
// Each session lives in exactly one place at a time: a tab in the bottom dock, or
// a tab in a floating window — a managed one (wm/WindowManager.jsx), so it moves, snaps,
// minimizes to the taskbar and stacks like every other window. A floating window holds
// one session until another is dragged onto it: dragging a window by its title bar, or a
// tab by itself, onto a terminal window adds it there as tabs, and onto the dock docks
// it. A tab dragged onto nothing becomes a window of its own, as Detach does.
// Detaching/attaching does NOT re-create xterm — the
// session's persistent host <div> (with xterm opened into it) is re-parented into
// the correct slot via appendChild, so scrollback and the live socket survive.

const TermCtx = createContext(null)
export const useTerminals = () => useContext(TermCtx)

const LAYOUT_KEY = 'dbcanvas-term-layout'
const loadLayout = () => {
  try { return { height: 300, ...JSON.parse(localStorage.getItem(LAYOUT_KEY) || '{}') } }
  catch { return { height: 300 } }
}

// floatRect is a new floating window's first size; the window manager places it.
const floatRect = () => ({ w: 640, h: 360 })

const XTERM_THEME = {
  background: '#0e1117', foreground: '#e6eaf2', cursor: '#6366f1',
  selectionBackground: '#33415580',
}

export function TerminalProvider({ children }) {
  const [sessions, setSessions] = useState([]) // [{id,title,status,floating,win,float}]
  const [activeId, setActiveId] = useState(null)
  // win is the floating window a session is a tab of; winActive is each window's shown tab.
  const [winActive, setWinActive] = useState({}) // win -> session id
  const sessionsRef = useRef(sessions)
  sessionsRef.current = sessions
  const winCounter = useRef(0)
  const newWin = () => `w${++winCounter.current}`
  const [open, setOpen] = useState(false)
  const termsRef = useRef(new Map()) // id -> {term, fit, ws, host, opened}
  const counter = useRef(0)
  const { settings } = useSettings() // terminalMode: where a new session opens
  const undocked = settings.terminalMode === 'undocked'

  const setStatus = useCallback((id, status) => {
    setSessions((ss) => ss.map((s) => (s.id === id ? { ...s, status } : s)))
  }, [])

  // A shared session (session/SessionProvider.jsx) changes where a terminal comes
  // from: opened by the driver or the host, it is one shell the whole session sees
  // (app/shareterm.go), and every browser — the opener's too — attaches to it when
  // the hub announces it. Outside a session a terminal is this browser's alone.
  const share = useSession()
  const shareRef = useRef(share)
  shareRef.current = share

  // attach creates a session from a socket URL: the xterm, its host div, the dock tab.
  const attach = useCallback(({ id, title, url, shared }) => {
    const term = new Terminal({
      fontSize: 13, cursorBlink: true, convertEol: false,
      fontFamily: 'ui-monospace, "JetBrains Mono", monospace', theme: XTERM_THEME,
    })
    const fit = new FitAddon()
    term.loadAddon(fit)

    const ws = new WebSocket(url)
    ws.binaryType = 'arraybuffer'
    const enc = new TextEncoder()

    ws.onmessage = (e) => term.write(new Uint8Array(e.data))
    ws.onopen = () => {
      setStatus(id, 'connected')
      try { ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows })) } catch { /* */ }
    }
    ws.onclose = () => { setStatus(id, 'closed'); term.write('\r\n\x1b[33m[session closed]\x1b[0m\r\n') }
    ws.onerror = () => setStatus(id, 'error')

    term.onData((d) => { if (ws.readyState === WebSocket.OPEN) ws.send(enc.encode(d)) })
    term.onResize(({ cols, rows }) => { if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'resize', cols, rows })) })

    // Persistent host div for the xterm — moved between dock/float, never recreated.
    const host = document.createElement('div')
    host.style.cssText = 'position:absolute;inset:0;padding:4px'

    termsRef.current.set(id, { term, fit, ws, host, opened: false, shared })
    // The user's terminalMode setting decides where the session lands: a dock tab (default) or
    // its own floating window, cascaded like a detached one.
    setSessions((ss) => [...ss, {
      id, title, status: 'connecting', floating: undocked, shared: !!shared,
      ...(undocked ? { float: floatRect(), win: newWin() } : {}),
    }])
    if (!undocked) {
      setActiveId(id)
      setOpen(true)
    }
  }, [setStatus, undocked])

  // pod, when given, opens the console one layer further in: inside a container of a
  // pod in the Kubernetes cluster this node runs, rather than in the node itself
  // ({ namespace, name, container, shell } — see app/k3dpods.go). Same socket, same
  // protocol; the server decides what to exec.
  const openTerminal = useCallback(({ stackId, nodeId, title, user, pod }) => {
    const sh = shareRef.current
    if (sh.active) {
      if (!sh.isDriver && !sh.isHost) {
        sh.notice('Ask the host for control to open a terminal.')
        return
      }
      sh.openSharedTerm({
        stackId, nodeId, title: title || nodeId, user,
        namespace: pod?.name ? (pod.namespace || 'default') : '', pod: pod?.name || '',
        container: pod?.container || '', shell: pod?.name ? (pod.shell || 'auto') : '',
      }).catch((e) => sh.notice(e.message))
      return // the hub's terminal-open opens it here, and everywhere else
    }
    // A fresh session id every call → multiple concurrent terminals per node.
    const n = ++counter.current
    const id = `${stackId}:${nodeId}#${n}`
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const params = new URLSearchParams()
    if (user) params.set('user', user)
    if (pod?.name) {
      params.set('namespace', pod.namespace || 'default')
      params.set('pod', pod.name)
      params.set('container', pod.container || '')
      params.set('shell', pod.shell || 'auto')
    }
    const qs = params.toString()
    const q = qs ? `?${qs}` : ''
    attach({ id, title: `${title || nodeId} · ${n}`, url: `${proto}://${location.host}/api/stacks/${stackId}/nodes/${nodeId}/term${q}` })
  }, [attach])

  // Shared terminals a viewer closed for themselves, so the list below does not
  // reopen them.
  const dismissed = useRef(new Set())

  const dropTerminal = useCallback((id) => {
    const t = termsRef.current.get(id)
    if (t) {
      if (t._ro) { try { t._ro.disconnect() } catch { /* */ } }
      try { t.ws.close() } catch { /* */ }
      try { t.term.dispose() } catch { /* */ }
      try { t.host.remove() } catch { /* */ }
      termsRef.current.delete(id)
    }
    setSessions((ss) => {
      const rest = ss.filter((s) => s.id !== id)
      setActiveId((cur) => (cur === id ? (rest.find((s) => !s.floating)?.id ?? null) : cur))
      if (rest.every((s) => s.floating)) setOpen(false)
      return rest
    })
  }, [])

  // A shared terminal is closed by whoever has control, for everyone. Anyone else —
  // the host too, while a guest drives — cannot (the server refuses it as well); they
  // can minimize it to the dock instead.
  const closeTerminal = useCallback((id) => {
    const t = termsRef.current.get(id)
    const sh = shareRef.current
    if (t?.shared && sh.active) {
      if (!sh.isDriver) { sh.notice('Only whoever has control can close a shared terminal.'); return }
      sh.closeSharedTerm(t.shared)
      return
    }
    dropTerminal(id)
  }, [dropTerminal])
  // canClose is what the close buttons ask.
  const canClose = useCallback((s) => !s.shared || !share.active || share.isDriver, [share.active, share.isDriver])

  // The session's shared terminals, kept in step with the hub: a viewer for each one
  // open, and gone when it closes (or when the session does).
  useEffect(() => {
    const open = new Set(share.active ? share.terms.map((t) => t.id) : [])
    for (const t of share.active ? share.terms : []) {
      const id = `share:${t.id}`
      if (!termsRef.current.has(id) && !dismissed.current.has(t.id)) {
        attach({ id, title: `${t.title} · shared`, url: shareApi.termURL(share.sid, t.id), shared: t.id })
      }
    }
    for (const [id, t] of termsRef.current) {
      if (t.shared && !open.has(t.shared)) dropTerminal(id)
    }
  }, [share.active, share.sid, share.terms, attach, dropTerminal])

  // moveLast re-places the sessions in ids at the end of the list, changed by fn — the
  // end, so a tab moved into the dock or a window lands after the ones already there.
  const moveLast = (ss, ids, fn) => [...ss.filter((s) => !ids.includes(s.id)), ...ss.filter((s) => ids.includes(s.id)).map(fn)]

  // detachTerminal gives a session a window of its own; at, when given, is where it opens.
  const detachTerminal = useCallback((id, at) => {
    const win = newWin()
    setSessions((ss) => ss.map((s) => (s.id === id
      ? { ...s, floating: true, win, float: { ...floatRect(), ...(s.float?.max ? { max: true } : {}), ...(at || {}) } }
      : s)))
    setWinActive((wa) => ({ ...wa, [win]: id }))
  }, [])

  const attachTerminal = useCallback((id) => {
    setSessions((ss) => moveLast(ss, [id], (s) => ({ ...s, floating: false, win: null })))
    setActiveId(id)
    setOpen(true)
  }, [])

  // dockWindow docks every tab of a floating window, keeping the one it showed in front.
  const dockWindow = useCallback((win, show) => {
    setSessions((ss) => {
      const ids = ss.filter((s) => s.floating && s.win === win).map((s) => s.id)
      return moveLast(ss, ids, (s) => ({ ...s, floating: false, win: null }))
    })
    if (show) setActiveId(show)
    setOpen(true)
  }, [])

  // placeTab moves session id next to the tab target (before it, or after when after is set),
  // into whichever place target is in — the dock or a window — and shows it there. The
  // order of the sessions list is the order of every tab strip.
  const placeTab = useCallback((id, target, after) => {
    const to = sessionsRef.current.find((s) => s.id === target)
    if (id === target || !to) return
    setSessions((ss) => {
      const me = ss.find((s) => s.id === id)
      const rest = ss.filter((s) => s.id !== id)
      const at = rest.findIndex((s) => s.id === target)
      if (!me || at < 0) return ss
      rest.splice(at + (after ? 1 : 0), 0, { ...me, floating: to.floating, win: to.win })
      return rest
    })
    if (to.floating) setWinActive((wa) => ({ ...wa, [to.win]: id }))
    else { setActiveId(id); setOpen(true) }
  }, [])

  // tabInto makes the sessions in ids tabs of the floating window win, showing show.
  const tabInto = useCallback((ids, win, show) => {
    setSessions((ss) => moveLast(ss, ids, (s) => ({ ...s, floating: true, win })))
    setWinActive((wa) => ({ ...wa, [win]: show || ids[0] }))
  }, [])

  const setFloat = useCallback((id, patch) => {
    setSessions((ss) => ss.map((s) => (s.id === id ? { ...s, float: { ...s.float, ...patch } } : s)))
  }, [])

  // Keep the active tab pointed at a docked session (detaching the active one, or
  // a list change, shouldn't leave the dock with a floating/empty active tab).
  useEffect(() => {
    const act = sessions.find((s) => s.id === activeId)
    if (!act || act.floating) {
      const firstDocked = sessions.find((s) => !s.floating)
      if (firstDocked && firstDocked.id !== activeId) setActiveId(firstDocked.id)
      else if (!firstDocked && activeId !== null) setActiveId(null)
    }
  }, [sessions, activeId])

  const value = {
    sessions, activeId, open, setActiveId, setOpen, openTerminal, closeTerminal, canClose,
    detachTerminal, attachTerminal, dockWindow, tabInto, placeTab, winActive, setWinActive, setFloat, termsRef,
  }
  return (
    <TermCtx.Provider value={value}>
      {children}
      <TerminalLayer />
    </TermCtx.Provider>
  )
}

// dropTargetAt is the terminal place under (x, y) that a drag would drop into: a floating
// terminal window ({ win }) or the dock ({ dock: true }). The topmost thing there decides —
// a terminal window behind another window is not a target. skipWin is the window being
// dragged, which is under the pointer itself.
export function dropTargetAt(x, y, skipWin) {
  for (const el of document.elementsFromPoint(x, y)) {
    const w = el.closest('[data-wm-window]')
    const id = w?.getAttribute('data-wm-window') || ''
    if (skipWin && id === `term:${skipWin}`) continue
    if (id.startsWith('term:')) return { win: id.slice(5) }
    if (el.closest('[data-terminal-dock]')) return { dock: true }
    return null
  }
  return null
}

function TerminalLayer() {
  const {
    sessions, activeId, open, setActiveId, setOpen, closeTerminal, canClose, detachTerminal, attachTerminal,
    dockWindow, tabInto, placeTab, winActive, setWinActive, setFloat, termsRef,
  } = useTerminals()
  const closeTitle = (s) => (canClose(s) ? 'Close' : 'Only whoever has control can close a shared terminal')
  const [layout, setLayout] = useState(loadLayout)
  const [menu, setMenu] = useState(null) // { x, y, id } for the right-click context menu
  // dockUp lifts the dock over the windows. It normally sits under them (a window is
  // something you put in front of things), which hid a terminal you had just opened
  // whenever a window — Database Stacks, say — reached down over the bottom of the
  // screen. So showing it, or a new session in it, brings it to the front, and
  // touching a window sends it back.
  //
  // Under the terminalsOnTop setting (the default) touching a window no longer sends it
  // back: the dock, like a floating terminal, leaves the front only when it is hidden.
  // The dock and the window layer are separate stacking contexts, so it cannot slot in
  // between terminal and non-terminal windows — it stays over all of them.
  const [dockUp, setDockUp] = useState(false)
  const { settings } = useSettings()
  const onTop = settings.terminalsOnTop !== 'off'
  const onTopRef = useRef(onTop)
  onTopRef.current = onTop
  const areaRef = useRef(null)
  const floatRefs = useRef(new Map()) // win -> body slot element
  const slotRefs = useRef(new Map()) // win -> its ref callback, kept so React does not re-run it every render
  const drag = useRef(null)
  const wm = useWindowManager()
  // dropOn is the place a drag in progress would drop into ('dock' or a window's win), for
  // its highlight; tabDrag is a tab being dragged, drawn under the pointer.
  const [dropOn, setDropOn] = useState(null)
  const [tabDrag, setTabDrag] = useState(null) // { id, title, x, y }
  const [insertAt, setInsertAt] = useState(null) // { id, after }: where a dragged tab would land in a strip
  const tabPress = useRef(null)

  // Right-click actions, shared by docked tabs and floating windows.
  const openMenu = (id) => (e) => { e.preventDefault(); e.stopPropagation(); setMenu({ x: e.clientX, y: e.clientY, id }) }
  // Maximize is offered only for docked tabs: detach into a maximized window.
  const maximize = (s) => { detachTerminal(s.id); setFloat(s.id, { max: true }) }
  const minimize = (s) => {
    if (s.floating) wm.minimize(`term:${s.win}`)
    else setOpen(false)
  }

  useEffect(() => {
    try { localStorage.setItem(LAYOUT_KEY, JSON.stringify(layout)) } catch { /* */ }
  }, [layout])

  const docked = sessions.filter((s) => !s.floating)
  const floating = sessions.filter((s) => s.floating)
  // The floating windows, in the order their first tab was opened, each with its tabs.
  const windows = []
  for (const s of floating) {
    let w = windows.find((x) => x.win === s.win)
    if (!w) windows.push(w = { win: s.win, tabs: [] })
    w.tabs.push(s)
  }
  const shownIn = (w) => w.tabs.find((s) => s.id === winActive[w.win]) || w.tabs[0]
  const shownId = new Map(windows.map((w) => [w.win, shownIn(w).id]))

  // Place each session's persistent host div into its current slot (dock area or
  // its floating window), opening xterm into it the first time it is attached.
  useEffect(() => {
    for (const s of sessions) {
      const t = termsRef.current.get(s.id)
      if (!t) continue
      const slot = s.floating ? floatRefs.current.get(s.win) : (open ? areaRef.current : null)
      if (!slot) continue
      if (t.host.parentElement !== slot) slot.appendChild(t.host)
      if (!t.opened) { try { t.term.open(t.host) } catch { /* */ } t.opened = true }
      const shown = s.floating ? shownId.get(s.win) === s.id : s.id === activeId
      t.host.style.display = shown ? 'block' : 'none'
      if (shown) requestAnimationFrame(() => { try { t.fit.fit() } catch { /* */ } })
    }
  })

  // Resize the docked active terminal when the dock area resizes.
  useEffect(() => {
    if (!areaRef.current) return
    const ro = new ResizeObserver(() => {
      const t = termsRef.current.get(activeId)
      if (t && t.opened) requestAnimationFrame(() => { try { t.fit.fit() } catch { /* */ } })
    })
    ro.observe(areaRef.current)
    return () => ro.disconnect()
  }, [activeId, open, termsRef])

  // Body-slot ref for a floating window — also fits the terminal it shows as the window resizes.
  const floatSlot = (win) => {
    if (!slotRefs.current.has(win)) {
      let ro = null
      slotRefs.current.set(win, (el) => {
        if (ro) { ro.disconnect(); ro = null }
        if (!el) { floatRefs.current.delete(win); slotRefs.current.delete(win); return }
        floatRefs.current.set(win, el)
        ro = new ResizeObserver(() => {
          for (const t of termsRef.current.values()) {
            if (t.opened && t.host.parentElement === el && t.host.style.display !== 'none') {
              try { t.fit.fit() } catch { /* */ }
            }
          }
        })
        ro.observe(el)
      })
    }
    return slotRefs.current.get(win)
  }

  // dropAt moves the sessions in ids (the tabs of window from, or a single tab) to what is
  // under (x, y), and reports whether there was anything there.
  const dropAt = (x, y, ids, from, show) => {
    const to = dropTargetAt(x, y, from)
    if (!to) return false
    if (to.dock) {
      if (from) dockWindow(from, show)
      else ids.forEach(attachTerminal)
    } else if (to.win !== from) {
      tabInto(ids, to.win, show)
    }
    return true
  }

  // A tab dragged out of the dock or a window's tab strip. It starts as a press, so a click
  // still just selects the tab; past a few pixels it is a drag.
  useEffect(() => {
    const move = (e) => {
      const p = tabPress.current
      if (!p) return
      if (!p.moved && Math.abs(e.clientX - p.sx) + Math.abs(e.clientY - p.sy) < 6) return
      p.moved = true
      const slot = tabSlotAt(e.clientX, e.clientY)
      setInsertAt(slot && slot.id !== p.id ? slot : null)
      setTabDrag({ id: p.id, title: p.title, x: e.clientX, y: e.clientY })
      if (slot) { setDropOn(null); return }
      const to = dropTargetAt(e.clientX, e.clientY)
      const own = to && (to.dock ? !p.from : to.win === p.from)
      setDropOn(!to || own ? null : to.dock ? 'dock' : to.win)
    }
    const up = (e) => {
      const p = tabPress.current
      if (!p) return
      tabPress.current = null
      document.body.style.userSelect = ''
      setDropOn(null)
      setTabDrag(null)
      setInsertAt(null)
      if (!p.moved || e.type === 'pointercancel') return
      // Onto a tab strip: into that place, at that position — which, in its own strip, is
      // rearranging the tabs.
      const slot = tabSlotAt(e.clientX, e.clientY)
      if (slot) { placeTab(p.id, slot.id, slot.after); return }
      const to = dropTargetAt(e.clientX, e.clientY)
      if (to && (to.dock ? !p.from : to.win === p.from)) return // back where it came from
      if (!dropAt(e.clientX, e.clientY, [p.id], null, p.id)) {
        // Onto nothing: a window of its own, its title bar under the pointer.
        detachTerminal(p.id, { x: Math.max(0, e.clientX - 80), y: Math.max(0, e.clientY - 14) })
      }
    }
    addEventListener('pointermove', move)
    addEventListener('pointerup', up)
    addEventListener('pointercancel', up)
    return () => { removeEventListener('pointermove', move); removeEventListener('pointerup', up); removeEventListener('pointercancel', up) }
  })
  // tabSlotAt is the position in a tab strip under (x, y): beside the tab there (after it when
  // the pointer is past its middle), or after the last tab on the strip's empty end.
  const tabSlotAt = (x, y) => {
    const el = document.elementFromPoint(x, y)
    const t = el?.closest('[data-term-tab]')
    if (t) {
      const r = t.getBoundingClientRect()
      return { id: t.getAttribute('data-term-tab'), after: x > r.x + r.width / 2 }
    }
    const last = el?.closest('[data-term-strip]')?.querySelector('[data-term-tab]:last-of-type')
    return last ? { id: last.getAttribute('data-term-tab'), after: true } : null
  }
  const pressTab = (s) => (e) => {
    if (e.button !== 0 || e.target.closest('button')) return
    e.stopPropagation()
    document.body.style.userSelect = 'none'
    tabPress.current = { id: s.id, title: s.title, from: s.floating ? s.win : null, sx: e.clientX, sy: e.clientY, moved: false }
  }

  // dragging the dock's height handle; floating windows are the window manager's.
  useEffect(() => {
    const onMove = (e) => {
      const d = drag.current
      if (!d) return
      setLayout((l) => ({ ...l, height: Math.min(Math.max(140, d.h0 + (d.y0 - e.clientY)), window.innerHeight - 80) }))
    }
    const onUp = () => { drag.current = null }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => { removeEventListener('pointermove', onMove); removeEventListener('pointerup', onUp) }
  }, [])

  useEffect(() => { if (open) setDockUp(true) }, [open, activeId, onTop])
  useEffect(() => {
    const on = (e) => {
      const t = e.target instanceof Element ? e.target : null
      if (!t) return
      if (t.closest('[data-terminal-dock]')) setDockUp(true)
      else if (t.closest('[data-wm-window]') && !onTopRef.current) setDockUp(false)
    }
    addEventListener('pointerdown', on, true)
    return () => removeEventListener('pointerdown', on, true)
  }, [])

  // The dock's toggle lives on the taskbar.
  // A dock that is open but under a window is brought forward, not hidden — hiding
  // what you cannot see is a click that seems to do nothing.
  const dockShown = open && dockUp
  useTaskbarItem('terminals', docked.length ? {
    label: `Terminals (${docked.length})`, icon: <Icon.Terminal size={14} />, active: dockShown,
    title: dockShown ? 'Hide the terminal dock' : 'Show the terminal dock',
    onClick: () => (open && !dockUp ? setDockUp(true) : setOpen((o) => !o)),
  } : null)

  if (sessions.length === 0) return null

  const statusDot = (status) => `h-1.5 w-1.5 rounded-full ${status === 'connected' ? 'bg-success' : status === 'error' || status === 'closed' ? 'bg-danger' : 'bg-warning'}`
  const dropRing = <div className="pointer-events-none absolute inset-0 z-20 rounded-b-lg border-2 border-primary/70 bg-primary/10" />

  // tab is one tab of the dock or of a window's strip: select on click, drag to move, the
  // detach and close buttons.
  const tab = (s, active, onSelect) => (
    <div key={s.id} data-no-drag data-term-tab={s.id} onPointerDown={pressTab(s)} onClick={onSelect} onContextMenu={openMenu(s.id)}
      style={{ touchAction: 'none' }}
      className={`relative flex shrink-0 cursor-pointer items-center gap-1.5 rounded-md px-2 py-1 text-xs ${tabDrag?.id === s.id ? 'opacity-40' : ''} ${active ? 'bg-surface text-fg shadow' : 'text-muted hover:bg-surface'}`}>
      <span className={statusDot(s.status)} />
      <span className="max-w-[140px] truncate">{s.title}</span>
      <button title="Detach into a window" onClick={(e) => { e.stopPropagation(); detachTerminal(s.id) }} className="rounded text-muted hover:text-fg"><Icon.External size={12} /></button>
      <button title={closeTitle(s)} disabled={!canClose(s)} onClick={(e) => { e.stopPropagation(); closeTerminal(s.id) }} className="rounded hover:text-danger disabled:cursor-not-allowed disabled:opacity-30">✕</button>
      {insertAt?.id === s.id && (
        <span className="pointer-events-none absolute inset-y-0.5 w-0.5 rounded bg-primary" style={insertAt.after ? { right: -3 } : { left: -3 }} />
      )}
    </div>
  )

  return (
    <>
      {/* floating windows, each one or more tabs */}
      {windows.map((w) => {
        const s = shownIn(w)
        const many = w.tabs.length > 1
        const closable = w.tabs.filter(canClose)
        return (
          <Window key={w.win} id={`term:${w.win}`} title={many ? `${s.title} (${w.tabs.length} tabs)` : s.title}
            size={{ ...floatRect(), ...w.tabs[0].float, max: !!w.tabs[0].float?.max }} onTop
            icon={<span className="flex items-center gap-1.5"><span className={statusDot(s.status)} /><Icon.Terminal size={13} /></span>}
            onClose={() => closable.forEach((x) => closeTerminal(x.id))} canClose={closable.length > 0}
            closeTitle={closeTitle(s)}
            menu={[
              { label: many ? 'Dock all tabs' : 'Dock', onClick: () => dockWindow(w.win, s.id) },
              ...(many ? [{ label: 'Detach this tab', onClick: () => detachTerminal(s.id) }] : []),
            ]}
            onDragOver={(x, y) => {
              const to = dropTargetAt(x, y, w.win)
              setDropOn(to ? (to.dock ? 'dock' : to.win) : null)
              return !!to
            }}
            onDrop={(x, y) => { setDropOn(null); dropAt(x, y, w.tabs.map((t) => t.id), w.win, s.id) }}
            header={(
              <span className="flex min-w-0 flex-1 justify-end">
                <button title={many ? 'Dock all tabs' : 'Dock'} onClick={() => dockWindow(w.win, s.id)} className="rounded p-1 text-muted hover:bg-surface hover:text-fg"><Icon.Frame size={13} /></button>
              </span>
            )}
            bodyClass="flex flex-col bg-[#0e1117]">
            {many && (
              <div data-term-strip className="flex shrink-0 gap-1 overflow-x-auto border-b bg-surface2 px-2 py-1">
                {w.tabs.map((t) => tab(t, t.id === s.id, () => setWinActive((wa) => ({ ...wa, [w.win]: t.id }))))}
              </div>
            )}
            <div ref={floatSlot(w.win)} className="relative min-h-0 flex-1 overflow-hidden" />
            {dropOn === w.win && dropRing}
          </Window>
        )
      })}

      {/* bottom dock (docked sessions) */}
      {docked.length > 0 && open && (
        <div data-terminal-dock className="fixed inset-x-0 flex flex-col border bg-surface shadow-2xl"
          // 46 is just over the window layer (45) and under dialogs and menus (50+).
          style={{ zIndex: dockUp ? 46 : 40, height: layout.height, bottom: 'var(--wm-taskbar, 0px)' }}>
          <div
            onPointerDown={(e) => { drag.current = { kind: 'height', y0: e.clientY, h0: layout.height } }}
            className="h-1.5 w-full cursor-ns-resize bg-border/60 hover:bg-primary"
          />
          <div className="flex items-center gap-1 border-b bg-surface2 px-2 py-1">
            <div data-term-strip className="flex min-w-0 flex-1 gap-1 overflow-x-auto">
              {docked.map((s) => tab(s, s.id === activeId, () => setActiveId(s.id)))}
            </div>
            <button title="Minimize" onClick={() => setOpen(false)} className="rounded px-1.5 text-muted hover:bg-surface hover:text-fg">—</button>
          </div>
          <div ref={areaRef} className="relative flex-1 overflow-hidden bg-[#0e1117]" />
          {dropOn === 'dock' && dropRing}
        </div>
      )}

      {/* a tab being dragged, under the pointer */}
      {tabDrag && (
        <div className="pointer-events-none fixed z-[60] flex items-center gap-1.5 rounded-md border bg-surface px-2 py-1 text-xs text-fg shadow-xl"
          style={{ left: tabDrag.x + 10, top: tabDrag.y + 8 }}>
          <Icon.Terminal size={12} /><span className="max-w-[180px] truncate">{tabDrag.title}</span>
        </div>
      )}

      {/* right-click context menu */}
      {menu && (() => {
        const s = sessions.find((x) => x.id === menu.id)
        if (!s) return null
        const run = (fn) => () => { fn(s); setMenu(null) }
        const item = (label, glyph, onClick, danger) => (
          <button onClick={onClick}
            className={`flex w-full items-center gap-2 px-3 py-1.5 text-left ${danger ? 'text-muted hover:bg-danger/10 hover:text-danger' : 'text-fg hover:bg-surface2'}`}>
            <span className="w-4 text-center text-muted">{glyph}</span>{label}
          </button>
        )
        return (
          <>
            <div className="fixed inset-0 z-50" onClick={() => setMenu(null)} onContextMenu={(e) => { e.preventDefault(); setMenu(null) }} />
            <div className="fixed z-50 min-w-[160px] rounded-md border bg-surface py-1 text-xs shadow-xl"
              style={{ left: Math.min(menu.x, window.innerWidth - 172), top: Math.min(menu.y, window.innerHeight - 160) }}>
              {!s.floating && item('Maximize', '⛶', run(maximize))}
              {item('Detach into a window', '⧉', run((x) => detachTerminal(x.id)))}
              {s.floating && item('Dock', '▁', run((x) => attachTerminal(x.id)))}
              {item('Minimize', '—', run(minimize))}
              {canClose(s) && item('Close', '✕', run((x) => closeTerminal(x.id)), true)}
            </div>
          </>
        )
      })()}
    </>
  )
}
