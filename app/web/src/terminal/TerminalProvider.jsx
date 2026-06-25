import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { Icon } from '../components/Icons.jsx'

// A top-level terminal manager. Because the provider (and its dock) live above
// the page switch, xterm instances + their WebSockets stay mounted across
// navigation — sessions are not reset when you leave and return to a page.
//
// Each session lives in exactly one place at a time: a tab in the bottom dock, or
// its own floating window. Detaching/attaching does NOT re-create xterm — the
// session's persistent host <div> (with xterm opened into it) is re-parented into
// the correct slot via appendChild, so scrollback and the live socket survive.

const TermCtx = createContext(null)
export const useTerminals = () => useContext(TermCtx)

const LAYOUT_KEY = 'dbcanvas-term-layout'
const loadLayout = () => {
  try { return { height: 300, ...JSON.parse(localStorage.getItem(LAYOUT_KEY) || '{}') } }
  catch { return { height: 300 } }
}

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

    // Persistent host div for the xterm — moved between dock/float, never recreated.
    const host = document.createElement('div')
    host.style.cssText = 'position:absolute;inset:0;padding:4px'

    termsRef.current.set(id, { term, fit, ws, host, opened: false })
    setSessions((ss) => [...ss, { id, title: tabTitle, status: 'connecting', floating: false }])
    setActiveId(id)
    setOpen(true)
  }, [setStatus])

  const closeTerminal = useCallback((id) => {
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

  const detachTerminal = useCallback((id) => {
    setSessions((ss) => {
      const floatingCount = ss.filter((s) => s.floating).length
      return ss.map((s) => (s.id === id
        ? { ...s, floating: true, float: s.float || { x: 120 + (floatingCount % 6) * 28, y: 96 + (floatingCount % 6) * 28, w: 580, h: 320 } }
        : s))
    })
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
    sessions, activeId, open, setActiveId, setOpen, openTerminal, closeTerminal,
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
  const { sessions, activeId, open, setActiveId, setOpen, closeTerminal, detachTerminal, attachTerminal, setFloat, termsRef } = useTerminals()
  const [layout, setLayout] = useState(loadLayout)
  const areaRef = useRef(null)
  const floatRefs = useRef(new Map()) // id -> body slot element
  const drag = useRef(null)

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

  // dragging: docked height handle, or a floating window's move.
  useEffect(() => {
    const onMove = (e) => {
      const d = drag.current
      if (!d) return
      if (d.kind === 'height') {
        setLayout((l) => ({ ...l, height: Math.min(Math.max(140, d.h0 + (d.y0 - e.clientY)), window.innerHeight - 80) }))
      } else if (d.kind === 'fmove') {
        setFloat(d.id, { x: d.fx + (e.clientX - d.x0), y: d.fy + (e.clientY - d.y0) })
      }
    }
    const onUp = () => { drag.current = null }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => { removeEventListener('pointermove', onMove); removeEventListener('pointerup', onUp) }
  }, [setFloat])

  if (sessions.length === 0) return null

  const statusDot = (status) => `h-1.5 w-1.5 rounded-full ${status === 'connected' ? 'bg-success' : status === 'error' || status === 'closed' ? 'bg-danger' : 'bg-warning'}`

  return (
    <>
      {/* floating per-tab windows */}
      {floating.map((s) => {
        const f = s.float || { x: 120, y: 96, w: 580, h: 320 }
        return (
          <div key={s.id} className="fixed z-40 flex flex-col rounded-lg border bg-surface shadow-2xl"
            style={{ left: f.x, top: f.y, width: f.w, height: f.h, resize: 'both', overflow: 'hidden' }}>
            <div
              className="flex items-center gap-1.5 border-b bg-surface2 px-2 py-1"
              style={{ cursor: 'move' }}
              onPointerDown={(e) => { if (e.target.closest('button')) return; drag.current = { kind: 'fmove', id: s.id, x0: e.clientX, y0: e.clientY, fx: f.x, fy: f.y } }}
            >
              <span className={statusDot(s.status)} />
              <span className="min-w-0 flex-1 truncate text-xs text-fg">{s.title}</span>
              <button title="Dock" onClick={() => attachTerminal(s.id)} className="rounded p-1 text-muted hover:bg-surface hover:text-fg"><Icon.Frame size={13} /></button>
              <button title="Close" onClick={() => closeTerminal(s.id)} className="rounded px-1.5 text-muted hover:text-danger">✕</button>
            </div>
            <div ref={floatSlot(s.id)} className="relative flex-1 overflow-hidden bg-[#0e1117]" />
          </div>
        )
      })}

      {/* bottom dock (docked sessions) */}
      {docked.length > 0 && !open && (
        <button onClick={() => setOpen(true)}
          className="fixed bottom-3 right-3 z-40 flex items-center gap-2 rounded-lg border bg-surface px-3 py-2 text-sm shadow-lg hover:bg-surface2">
          <Icon.Nodes size={16} /> Terminals ({docked.length})
        </button>
      )}
      {docked.length > 0 && open && (
        <div className="fixed inset-x-0 bottom-0 z-40 flex flex-col border bg-surface shadow-2xl" style={{ height: layout.height }}>
          <div
            onPointerDown={(e) => { drag.current = { kind: 'height', y0: e.clientY, h0: layout.height } }}
            className="h-1.5 w-full cursor-ns-resize bg-border/60 hover:bg-primary"
          />
          <div className="flex items-center gap-1 border-b bg-surface2 px-2 py-1">
            <div className="flex min-w-0 flex-1 gap-1 overflow-x-auto">
              {docked.map((s) => (
                <div key={s.id} onClick={() => setActiveId(s.id)}
                  className={`flex shrink-0 cursor-pointer items-center gap-1.5 rounded-md px-2 py-1 text-xs ${s.id === activeId ? 'bg-surface text-fg shadow' : 'text-muted hover:bg-surface'}`}>
                  <span className={statusDot(s.status)} />
                  <span className="max-w-[140px] truncate">{s.title}</span>
                  <button title="Detach into a window" onClick={(e) => { e.stopPropagation(); detachTerminal(s.id) }} className="rounded text-muted hover:text-fg"><Icon.External size={12} /></button>
                  <button title="Close" onClick={(e) => { e.stopPropagation(); closeTerminal(s.id) }} className="rounded hover:text-danger">✕</button>
                </div>
              ))}
            </div>
            <button title="Minimize" onClick={() => setOpen(false)} className="rounded px-1.5 text-muted hover:bg-surface hover:text-fg">—</button>
          </div>
          <div ref={areaRef} className="relative flex-1 overflow-hidden bg-[#0e1117]" />
        </div>
      )}
    </>
  )
}
