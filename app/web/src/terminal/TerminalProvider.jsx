import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { Icon } from '../components/Icons.jsx'

// A top-level terminal manager. Because the provider (and its dock) live above
// the page switch, xterm instances + their WebSockets stay mounted across
// navigation — sessions are not reset when you leave and return to a page.

const TermCtx = createContext(null)
export const useTerminals = () => useContext(TermCtx)

const LAYOUT_KEY = 'dbcanvas-term-layout'
const loadLayout = () => {
  try { return { docked: true, height: 300, float: { x: 80, y: 80, w: 700, h: 360 }, ...JSON.parse(localStorage.getItem(LAYOUT_KEY) || '{}') } }
  catch { return { docked: true, height: 300, float: { x: 80, y: 80, w: 700, h: 360 } } }
}

const XTERM_THEME = {
  background: '#0e1117', foreground: '#e6eaf2', cursor: '#6366f1',
  selectionBackground: '#33415580',
}

export function TerminalProvider({ children }) {
  const [sessions, setSessions] = useState([]) // [{id,title,status}]
  const [activeId, setActiveId] = useState(null)
  const [open, setOpen] = useState(false)
  const termsRef = useRef(new Map()) // id -> {term, fit, ws}
  const counter = useRef(0)

  const setStatus = useCallback((id, status) => {
    setSessions((ss) => ss.map((s) => (s.id === id ? { ...s, status } : s)))
  }, [])

  const openTerminal = useCallback(({ stackId, nodeId, title }) => {
    // A fresh session id every call → multiple concurrent terminals per node.
    const n = ++counter.current
    const id = `${stackId}:${nodeId}#${n}`
    const tabTitle = `${title || nodeId} · ${n}`
    const term = new Terminal({
      fontSize: 13, cursorBlink: true, convertEol: false,
      fontFamily: 'ui-monospace, "JetBrains Mono", monospace', theme: XTERM_THEME,
    })
    const fit = new FitAddon()
    term.loadAddon(fit)

    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const ws = new WebSocket(`${proto}://${location.host}/api/stacks/${stackId}/nodes/${nodeId}/term`)
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

    termsRef.current.set(id, { term, fit, ws, opened: false })
    setSessions((ss) => [...ss, { id, title: tabTitle, status: 'connecting' }])
    setActiveId(id)
    setOpen(true)
  }, [setStatus])

  const closeTerminal = useCallback((id) => {
    const t = termsRef.current.get(id)
    if (t) { try { t.ws.close() } catch { /* */ } try { t.term.dispose() } catch { /* */ } termsRef.current.delete(id) }
    setSessions((ss) => {
      const rest = ss.filter((s) => s.id !== id)
      setActiveId((cur) => (cur === id ? (rest[0]?.id ?? null) : cur))
      if (rest.length === 0) setOpen(false)
      return rest
    })
  }, [])

  const value = { sessions, activeId, open, setActiveId, setOpen, openTerminal, closeTerminal, termsRef }
  return (
    <TermCtx.Provider value={value}>
      {children}
      <TerminalDock />
    </TermCtx.Provider>
  )
}

function TerminalDock() {
  const { sessions, activeId, open, setActiveId, setOpen, closeTerminal, termsRef } = useTerminals()
  const [layout, setLayout] = useState(loadLayout)
  const areaRef = useRef(null)
  const drag = useRef(null)

  useEffect(() => {
    try { localStorage.setItem(LAYOUT_KEY, JSON.stringify(layout)) } catch { /* */ }
  }, [layout])

  // fit the active terminal whenever the area resizes or the active tab changes
  const fitActive = useCallback(() => {
    const t = termsRef.current.get(activeId)
    if (t && t.opened) requestAnimationFrame(() => { try { t.fit.fit() } catch { /* */ } })
  }, [activeId, termsRef])

  useEffect(() => {
    if (!areaRef.current) return
    const ro = new ResizeObserver(() => fitActive())
    ro.observe(areaRef.current)
    return () => ro.disconnect()
  }, [fitActive, open, layout.docked])

  useEffect(() => { fitActive() }, [activeId, open, layout, fitActive])

  // mount an xterm instance into its persistent div exactly once
  const mount = (id, el) => {
    const t = termsRef.current.get(id)
    if (!t || !el || t.opened) return
    t.term.open(el)
    t.opened = true
    requestAnimationFrame(() => { try { t.fit.fit() } catch { /* */ } })
  }

  // dragging: 'move' (detached header) | 'resize-h' (docked height)
  useEffect(() => {
    const onMove = (e) => {
      const d = drag.current
      if (!d) return
      if (d.kind === 'height') {
        const h = Math.min(Math.max(140, d.h0 + (d.y0 - e.clientY)), window.innerHeight - 80)
        setLayout((l) => ({ ...l, height: h }))
      } else if (d.kind === 'move') {
        setLayout((l) => ({ ...l, float: { ...l.float, x: d.fx + (e.clientX - d.x0), y: d.fy + (e.clientY - d.y0) } }))
      }
    }
    const onUp = () => { drag.current = null }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => { removeEventListener('pointermove', onMove); removeEventListener('pointerup', onUp) }
  }, [])

  if (sessions.length === 0) return null

  // minimized: a small restore pill
  if (!open) {
    return (
      <button
        onClick={() => setOpen(true)}
        className="fixed bottom-3 right-3 z-40 flex items-center gap-2 rounded-lg border bg-surface px-3 py-2 text-sm shadow-lg hover:bg-surface2"
      >
        <Icon.Nodes size={16} /> Terminals ({sessions.length})
      </button>
    )
  }

  const detached = !layout.docked
  const containerStyle = detached
    ? { position: 'fixed', left: layout.float.x, top: layout.float.y, width: layout.float.w, height: layout.float.h, resize: 'both', overflow: 'hidden' }
    : { position: 'fixed', left: 0, right: 0, bottom: 0, height: layout.height }

  return (
    <div className="z-40 flex flex-col border bg-surface shadow-2xl" style={containerStyle}>
      {/* docked height handle */}
      {!detached && (
        <div
          onPointerDown={(e) => { drag.current = { kind: 'height', y0: e.clientY, h0: layout.height } }}
          className="h-1.5 w-full cursor-ns-resize bg-border/60 hover:bg-primary"
        />
      )}
      {/* header / tabs */}
      <div
        className="flex items-center gap-1 border-b bg-surface2 px-2 py-1"
        onPointerDown={detached ? (e) => {
          if (e.target.closest('button')) return
          drag.current = { kind: 'move', x0: e.clientX, y0: e.clientY, fx: layout.float.x, fy: layout.float.y }
        } : undefined}
        style={detached ? { cursor: 'move' } : undefined}
      >
        <div className="flex min-w-0 flex-1 gap-1 overflow-x-auto">
          {sessions.map((s) => (
            <div
              key={s.id}
              onClick={() => setActiveId(s.id)}
              className={`flex shrink-0 cursor-pointer items-center gap-1.5 rounded-md px-2 py-1 text-xs ${s.id === activeId ? 'bg-surface text-fg shadow' : 'text-muted hover:bg-surface'}`}
            >
              <span className={`h-1.5 w-1.5 rounded-full ${s.status === 'connected' ? 'bg-success' : s.status === 'error' || s.status === 'closed' ? 'bg-danger' : 'bg-warning'}`} />
              <span className="max-w-[140px] truncate">{s.title}</span>
              <button onClick={(e) => { e.stopPropagation(); closeTerminal(s.id) }} className="rounded hover:text-danger">✕</button>
            </div>
          ))}
        </div>
        <button title={detached ? 'Dock' : 'Detach'} onClick={() => setLayout((l) => ({ ...l, docked: !l.docked }))}
          className="rounded p-1 text-muted hover:bg-surface hover:text-fg"><Icon.Frame size={14} /></button>
        <button title="Minimize" onClick={() => setOpen(false)} className="rounded px-1.5 text-muted hover:bg-surface hover:text-fg">—</button>
      </div>
      {/* terminal area */}
      <div ref={areaRef} className="relative flex-1 overflow-hidden bg-[#0e1117]">
        {sessions.map((s) => (
          <div
            key={s.id}
            ref={(el) => mount(s.id, el)}
            className="absolute inset-0 p-1"
            style={{ display: s.id === activeId ? 'block' : 'none' }}
          />
        ))}
      </div>
    </div>
  )
}
