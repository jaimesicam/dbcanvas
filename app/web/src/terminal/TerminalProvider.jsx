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
// its own floating window — a managed one (wm/WindowManager.jsx), so it moves, snaps,
// minimizes to the taskbar and stacks like every other window. Detaching/attaching does NOT re-create xterm — the
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
  const [sessions, setSessions] = useState([]) // [{id,title,status,floating,float}]
  const [activeId, setActiveId] = useState(null)
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
      ...(undocked ? { float: floatRect() } : {}),
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

  const detachTerminal = useCallback((id) => {
    setSessions((ss) => ss.map((s) => (s.id === id ? { ...s, floating: true, float: s.float || floatRect() } : s)))
  }, [])

  const attachTerminal = useCallback((id) => {
    setSessions((ss) => ss.map((s) => (s.id === id ? { ...s, floating: false } : s)))
    setActiveId(id)
    setOpen(true)
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
    detachTerminal, attachTerminal, setFloat, termsRef,
  }
  return (
    <TermCtx.Provider value={value}>
      {children}
      <TerminalLayer />
    </TermCtx.Provider>
  )
}

function TerminalLayer() {
  const { sessions, activeId, open, setActiveId, setOpen, closeTerminal, canClose, detachTerminal, attachTerminal, setFloat, termsRef } = useTerminals()
  const closeTitle = (s) => (canClose(s) ? 'Close' : 'Only whoever has control can close a shared terminal')
  const [layout, setLayout] = useState(loadLayout)
  const [menu, setMenu] = useState(null) // { x, y, id } for the right-click context menu
  // dockUp lifts the dock over the windows. It normally sits under them (a window is
  // something you put in front of things), which hid a terminal you had just opened
  // whenever a window — Database Stacks, say — reached down over the bottom of the
  // screen. So showing it, or a new session in it, brings it to the front, and
  // touching a window sends it back.
  const [dockUp, setDockUp] = useState(false)
  const areaRef = useRef(null)
  const floatRefs = useRef(new Map()) // id -> body slot element
  const drag = useRef(null)
  const wm = useWindowManager()

  // Right-click actions, shared by docked tabs and floating windows.
  const openMenu = (id) => (e) => { e.preventDefault(); e.stopPropagation(); setMenu({ x: e.clientX, y: e.clientY, id }) }
  // Maximize is offered only for docked tabs: detach into a maximized window.
  const maximize = (s) => { detachTerminal(s.id); setFloat(s.id, { max: true }) }
  const minimize = (s) => {
    if (s.floating) wm.minimize(`term:${s.id}`)
    else setOpen(false)
  }

  useEffect(() => {
    try { localStorage.setItem(LAYOUT_KEY, JSON.stringify(layout)) } catch { /* */ }
  }, [layout])

  const docked = sessions.filter((s) => !s.floating)
  const floating = sessions.filter((s) => s.floating)

  // Place each session's persistent host div into its current slot (dock area or
  // its floating window), opening xterm into it the first time it is attached.
  useEffect(() => {
    for (const s of sessions) {
      const t = termsRef.current.get(s.id)
      if (!t) continue
      const slot = s.floating ? floatRefs.current.get(s.id) : (open ? areaRef.current : null)
      if (!slot) continue
      if (t.host.parentElement !== slot) slot.appendChild(t.host)
      if (!t.opened) { try { t.term.open(t.host) } catch { /* */ } t.opened = true }
      t.host.style.display = (!s.floating && s.id !== activeId) ? 'none' : 'block'
      requestAnimationFrame(() => { try { t.fit.fit() } catch { /* */ } })
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

  // Body-slot ref for a floating window — also fits the terminal as the window resizes.
  const floatSlot = (id) => (el) => {
    const t = termsRef.current.get(id)
    if (el) {
      floatRefs.current.set(id, el)
      if (t && !t._ro) {
        t._ro = new ResizeObserver(() => { try { t.fit.fit() } catch { /* */ } })
        t._ro.observe(el)
      }
    } else {
      floatRefs.current.delete(id)
      if (t && t._ro) { try { t._ro.disconnect() } catch { /* */ } t._ro = null }
    }
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

  useEffect(() => { if (open) setDockUp(true) }, [open, activeId])
  useEffect(() => {
    const on = (e) => {
      const t = e.target instanceof Element ? e.target : null
      if (!t) return
      if (t.closest('[data-terminal-dock]')) setDockUp(true)
      else if (t.closest('[data-wm-window]')) setDockUp(false)
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

  return (
    <>
      {/* floating per-tab windows */}
      {floating.map((s) => (
        <Window key={s.id} id={`term:${s.id}`} title={s.title} size={{ ...floatRect(), max: !!s.float?.max }}
          icon={<span className="flex items-center gap-1.5"><span className={statusDot(s.status)} /><Icon.Terminal size={13} /></span>}
          onClose={() => closeTerminal(s.id)} canClose={canClose(s)} closeTitle={closeTitle(s)}
          menu={[{ label: 'Dock', onClick: () => attachTerminal(s.id) }]}
          header={(
            <span className="flex min-w-0 flex-1 justify-end">
              <button title="Dock" onClick={() => attachTerminal(s.id)} className="rounded p-1 text-muted hover:bg-surface hover:text-fg"><Icon.Frame size={13} /></button>
            </span>
          )}
          bodyClass="bg-[#0e1117]">
          <div ref={floatSlot(s.id)} className="absolute inset-0 overflow-hidden" />
        </Window>
      ))}

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
            <div className="flex min-w-0 flex-1 gap-1 overflow-x-auto">
              {docked.map((s) => (
                <div key={s.id} onClick={() => setActiveId(s.id)} onContextMenu={openMenu(s.id)}
                  className={`flex shrink-0 cursor-pointer items-center gap-1.5 rounded-md px-2 py-1 text-xs ${s.id === activeId ? 'bg-surface text-fg shadow' : 'text-muted hover:bg-surface'}`}>
                  <span className={statusDot(s.status)} />
                  <span className="max-w-[140px] truncate">{s.title}</span>
                  <button title="Detach into a window" onClick={(e) => { e.stopPropagation(); detachTerminal(s.id) }} className="rounded text-muted hover:text-fg"><Icon.External size={12} /></button>
                  <button title={closeTitle(s)} disabled={!canClose(s)} onClick={(e) => { e.stopPropagation(); closeTerminal(s.id) }} className="rounded hover:text-danger disabled:cursor-not-allowed disabled:opacity-30">✕</button>
                </div>
              ))}
            </div>
            <button title="Minimize" onClick={() => setOpen(false)} className="rounded px-1.5 text-muted hover:bg-surface hover:text-fg">—</button>
          </div>
          <div ref={areaRef} className="relative flex-1 overflow-hidden bg-[#0e1117]" />
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
              style={{ left: Math.min(menu.x, window.innerWidth - 172), top: Math.min(menu.y, window.innerHeight - 108) }}>
              {!s.floating && item('Maximize', '⛶', run(maximize))}
              {item('Minimize', '—', run(minimize))}
              {canClose(s) && item('Close', '✕', run((x) => closeTerminal(x.id)), true)}
            </div>
          </>
        )
      })()}
    </>
  )
}
