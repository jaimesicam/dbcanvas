import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Icon } from '../components/Icons.jsx'

// WindowManager — every floating window in DBCanvas, managed in one place.
//
// Terminals, browser windows (node web UIs) and the file managers each used to draw
// their own frame, with their own drag code and their own idea of z-order — a
// browser window could climb over a dialog, a terminal could not be resized from its
// edges, and a file manager was a modal that blocked the page behind it. Now each of
// them renders a <Window>, and this provider owns what a window manager owns:
//
//   - geometry: move by the title bar, resize from any edge or corner;
//   - stacking: the window you touch comes to the front, always below dialogs and
//     menus (the whole layer is one z-index; windows are ranked inside it);
//   - minimize, maximize (also a double-click on the title bar), close;
//   - snapping: drag to the left or right edge for half the screen, into a corner
//     for a quarter, to the top edge to maximize — with a preview of where it lands;
//   - tile, cascade and minimize-all, from the taskbar;
//   - a taskbar listing every window, plus what other providers put on it (the
//     terminal dock's toggle).
//
// The content stays with whoever owns it. A window is a frame: its body is the
// owner's children, kept mounted while minimized (hidden, not unmounted), so a
// terminal's socket and an iframe's page survive it. Geometry is this browser's
// own; in a shared session the mirror (session/Mirror.jsx) is what shows it.

const Ctx = createContext(null)
const ApiCtx = createContext(null)
export const useWindowManager = () => useContext(Ctx)
// useWindowApi is the manager's commands alone — stable, so a caller that only opens,
// focuses or arranges windows is not re-rendered on every frame of a drag.
export const useWindowApi = () => useContext(ApiCtx)

export const TASKBAR_H = 36
const SNAP_EDGE = 14 // px from a screen edge that snaps
const SNAP_CORNER = 80 // px from a corner, along the edge, that makes it a quarter
const MIN_W = 320
const MIN_H = 180

// area is where windows live: the viewport above the taskbar, less what the shell
// keeps for itself (insets: the top bar, the session panel).
const NO_INSET = { top: 0, right: 0 }
const areaOf = (bar, inset = NO_INSET) => ({
  x: 0, y: inset.top, w: innerWidth - inset.right, h: innerHeight - inset.top - (bar ? TASKBAR_H : 0),
})

// snapRect is the rectangle a snap zone stands for.
function snapRect(zone, a) {
  const hw = Math.round(a.w / 2), hh = Math.round(a.h / 2)
  switch (zone) {
    case 'max': return { x: a.x, y: a.y, w: a.w, h: a.h }
    case 'left': return { x: a.x, y: a.y, w: hw, h: a.h }
    case 'right': return { x: a.x + hw, y: a.y, w: a.w - hw, h: a.h }
    case 'tl': return { x: a.x, y: a.y, w: hw, h: hh }
    case 'tr': return { x: a.x + hw, y: a.y, w: a.w - hw, h: hh }
    case 'bl': return { x: a.x, y: a.y + hh, w: hw, h: a.h - hh }
    case 'br': return { x: a.x + hw, y: a.y + hh, w: a.w - hw, h: a.h - hh }
    default: return null
  }
}

// zoneAt is the snap zone under the pointer while dragging, if any.
function zoneAt(px, py, a) {
  const left = px <= a.x + SNAP_EDGE, right = px >= a.x + a.w - SNAP_EDGE
  const top = py <= a.y + SNAP_EDGE
  if (left || right) {
    if (py <= a.y + SNAP_CORNER) return left ? 'tl' : 'tr'
    if (py >= a.y + a.h - SNAP_CORNER) return left ? 'bl' : 'br'
    return left ? 'left' : 'right'
  }
  if (top) return 'max'
  return null
}

// clampRect keeps a window's title bar reachable: a window can be pushed mostly off
// the side, never above the top or below the taskbar.
function clampRect(r, a) {
  const KEEP = 80
  return {
    ...r,
    w: Math.max(MIN_W, Math.min(r.w, a.w)),
    h: Math.max(MIN_H, Math.min(r.h, a.h)),
    x: Math.min(Math.max(r.x, KEEP - r.w), a.w - KEEP),
    y: Math.min(Math.max(r.y, a.y), a.y + a.h - 32),
  }
}

let cascadeN = 0

// dragCursor holds one cursor over the whole page while a window is moved or resized.
// The pointer crosses terminals, buttons and text on the way, each with a cursor of its
// own, so setting it on <body> is not enough: index.css forces it on every element
// while html[data-wm-drag] is set. null puts the page's own cursors back.
const RESIZE_CURSOR = { n: 'ns-resize', s: 'ns-resize', e: 'ew-resize', w: 'ew-resize', ne: 'nesw-resize', sw: 'nesw-resize', nw: 'nwse-resize', se: 'nwse-resize' }
function dragCursor(cursor) {
  const el = document.documentElement
  if (cursor) {
    el.style.setProperty('--wm-drag-cursor', cursor)
    el.dataset.wmDrag = ''
  } else {
    el.style.removeProperty('--wm-drag-cursor')
    delete el.dataset.wmDrag
  }
}

export function WindowManagerProvider({ children }) {
  // wins: id -> { id, title, icon, rect, min, max, snap, order }
  //   rect is where the window is when it is neither maximized nor snapped; snap is
  //   the zone it is snapped to; order is its place in the stack (higher = front).
  const [wins, setWins] = useState({})
  const [extras, setExtras] = useState({}) // key -> { label, icon, active, onClick, title }
  // The shell's say: start is the taskbar's leading element (the desktop's Start
  // button), always keeps the taskbar up with nothing open, inset is kept clear.
  const [shell, setShellState] = useState({ start: null, status: null, always: false, inset: NO_INSET })
  const insetRef = useRef(NO_INSET)
  insetRef.current = shell.inset
  const [preview, setPreview] = useState(null) // the snap preview rect while dragging
  const [dragging, setDragging] = useState(false)
  const [, setVp] = useState(0)
  const order = useRef(0)
  const layer = useRef(null)
  const [layerEl, setLayerEl] = useState(null)
  const winsRef = useRef(wins)
  winsRef.current = wins

  const bar = shell.always || Object.keys(wins).length > 0 || Object.keys(extras).length > 0
  const barRef = useRef(bar)
  barRef.current = bar
  const area = useCallback(() => areaOf(barRef.current, insetRef.current), [])
  const setShell = useCallback((p) => setShellState((s) => {
    const n = { ...s, ...p }
    return n.start === s.start && n.status === s.status && n.always === s.always && n.inset.top === s.inset.top && n.inset.right === s.inset.right ? s : n
  }), [])

  // The viewport changing size re-lays maximized and snapped windows.
  useEffect(() => {
    const on = () => setVp((n) => n + 1)
    addEventListener('resize', on)
    return () => removeEventListener('resize', on)
  }, [])

  // The shell's reserved space changed (the desktop's top bar came up, the session
  // panel opened): every window is brought back inside what is left.
  useEffect(() => {
    setWins((ws) => {
      const a = areaOf(barRef.current, shell.inset)
      // Pulled in, and narrowed or shortened to fit when it ran past the new edge.
      const fit = (r) => clampRect({
        ...r,
        w: r.x + r.w > a.x + a.w ? Math.max(MIN_W, a.x + a.w - Math.max(r.x, a.x)) : r.w,
        h: r.y + r.h > a.y + a.h ? Math.max(MIN_H, a.y + a.h - Math.max(r.y, a.y)) : r.h,
      }, a)
      return Object.fromEntries(Object.entries(ws).map(([k, w]) => [k, { ...w, rect: fit(w.rect) }]))
    })
  }, [shell.inset])

  // Docked panels below the windows (the terminal dock) sit above the taskbar.
  useLayoutEffect(() => {
    document.documentElement.style.setProperty('--wm-taskbar', bar ? `${TASKBAR_H}px` : '0px')
  }, [bar])

  const patch = useCallback((id, p) => setWins((ws) => (ws[id] ? { ...ws, [id]: { ...ws[id], ...(typeof p === 'function' ? p(ws[id]) : p) } } : ws)), [])

  const register = useCallback((id, spec) => {
    setWins((ws) => {
      if (ws[id]) return { ...ws, [id]: { ...ws[id], title: spec.title, icon: spec.icon } }
      const a = areaOf(true, insetRef.current)
      const w = Math.min(spec.w || 720, a.w - 40)
      const h = Math.min(spec.h || 460, a.h - 40)
      const n = cascadeN++ % 8
      const rect = clampRect({
        x: spec.x ?? Math.round(Math.max(20, (a.w - w) / 2 - 120 + n * 32)),
        y: spec.y ?? Math.round(a.y + Math.max(12, (a.h - h) / 2 - 90 + n * 32)),
        w, h,
      }, a)
      return { ...ws, [id]: { id, title: spec.title, icon: spec.icon, rect, min: false, max: !!spec.max, snap: null, order: ++order.current } }
    })
  }, [])
  const unregister = useCallback((id) => setWins((ws) => {
    if (!ws[id]) return ws
    const { [id]: _, ...rest } = ws
    return rest
  }), [])

  const focus = useCallback((id) => {
    const w = winsRef.current[id]
    if (!w) return
    const top = Math.max(0, ...Object.values(winsRef.current).map((x) => x.order))
    if (w.order === top && !w.min) return
    patch(id, { order: ++order.current, min: false })
  }, [patch])
  const minimize = useCallback((id) => patch(id, { min: true }), [patch])
  const toggleMax = useCallback((id) => patch(id, (w) => ({ max: !(w.max || w.snap), snap: null, min: false })), [patch])
  const maximize = useCallback((id, on = true) => patch(id, { max: on, snap: null, min: false }), [patch])

  // The window at the front that is not minimized — what the taskbar shows as active.
  const front = useMemo(() => {
    let best = null
    for (const w of Object.values(wins)) if (!w.min && (!best || w.order > best.order)) best = w
    return best?.id ?? null
  }, [wins])

  const minimizeAll = useCallback(() => setWins((ws) => Object.fromEntries(Object.entries(ws).map(([k, w]) => [k, { ...w, min: true }]))), [])

  // tile lays the open windows out in a grid; cascade stacks them diagonally.
  const tile = useCallback(() => setWins((ws) => {
    const list = Object.values(ws).filter((w) => !w.min).sort((a, b) => a.order - b.order)
    if (!list.length) return ws
    const a = area()
    const cols = Math.ceil(Math.sqrt(list.length))
    const rows = Math.ceil(list.length / cols)
    const out = { ...ws }
    list.forEach((w, i) => {
      const c = i % cols, r = Math.floor(i / cols)
      const cw = Math.floor(a.w / cols), rh = Math.floor(a.h / rows)
      out[w.id] = { ...w, max: false, snap: null, rect: { x: a.x + c * cw, y: a.y + r * rh, w: cw, h: rh } }
    })
    return out
  }), [area])
  const cascade = useCallback(() => setWins((ws) => {
    const list = Object.values(ws).filter((w) => !w.min).sort((a, b) => a.order - b.order)
    const a = area()
    const w = Math.min(900, Math.round(a.w * 0.6)), h = Math.min(600, Math.round(a.h * 0.65))
    const out = { ...ws }
    list.forEach((x, i) => { out[x.id] = { ...x, max: false, snap: null, rect: clampRect({ x: 40 + i * 32, y: a.y + 20 + i * 32, w, h }, a) } })
    return out
  }), [area])

  const setExtra = useCallback((key, item) => setExtras((es) => {
    if (!item) { const { [key]: _, ...rest } = es; return rest }
    return { ...es, [key]: item }
  }), [])

  useLayoutEffect(() => { setLayerEl(layer.current) }, [])

  const api = useMemo(() => ({
    register, unregister, patch, focus, minimize, maximize, toggleMax, tile, cascade, minimizeAll, setExtra,
    area, setPreview, setDragging, setShell,
  }), [register, unregister, patch, focus, minimize, maximize, toggleMax, tile, cascade, minimizeAll, setExtra, area, setShell])
  const value = useMemo(() => ({ ...api, wins, extras, front, layerEl, start: shell.start, status: shell.status, desktop: shell.always }),
    [api, wins, extras, front, layerEl, shell.start, shell.status, shell.always])

  return (
    <ApiCtx.Provider value={api}>
    <Ctx.Provider value={value}>
      {children}
      {/* One stacking context for every window: above the page and the terminal
          dock, below dialogs and menus (z-50 and up). The dock comes over it while
          it is in use — see dockUp in terminal/TerminalProvider.jsx. */}
      <div ref={layer} className={`pointer-events-none fixed inset-0 ${dragging ? 'wm-dragging' : ''}`} style={{ zIndex: 45 }} />
      {preview && (
        <div className="pointer-events-none fixed rounded-lg border-2 border-primary/70 bg-primary/10 transition-all"
          style={{ zIndex: 46, left: preview.x, top: preview.y, width: preview.w, height: preview.h }} />
      )}
      {bar && <Taskbar />}
    </Ctx.Provider>
    </ApiCtx.Provider>
  )
}

// Window is one managed window. Its owner keeps the content and says what closing
// means; the frame, the geometry and the stacking are the manager's.
//
//   id         stable for the window's life
//   title      the title bar and taskbar label
//   icon       an element for both
//   size       { w, h } (and optionally x, y) for its first placement
//   header     extra title-bar controls (a browser window's toolbar), placed after
//              the title; a pointer-down on a button or input in it never drags
//   onClose    the close button; omitted, there is none
//   canClose   false greys the close button out, with closeTitle as the reason
//   menu       extra entries for the title bar's right-click menu: [{ label, onClick }]
//   bodyClass  classes for the body
//   private    the whole window is an empty box in a shared session's mirror
const NO_WM = { register: () => {}, unregister: () => {}, wins: {}, layerEl: null, area: () => areaOf(false) }

export function Window({ id, title, icon, size, header, onClose, canClose = true, closeTitle, menu, bodyClass = '', private: priv, children }) {
  const wm = useWindowManager() || NO_WM // outside the provider (a render check) it draws nothing
  const { register, unregister } = wm
  const w = wm.wins[id]
  const drag = useRef(null)
  const [ctx, setCtx] = useState(null)

  useEffect(() => {
    register(id, { title, icon, ...size })
  }, [id, title]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => unregister(id), [id, unregister])
  // A window closed mid-drag must not leave the page's cursor grabbing.
  useEffect(() => () => { if (drag.current) dragCursor(null) }, [])

  const a = wm.area()
  const geo = !w ? null : w.max ? snapRect('max', a) : w.snap ? snapRect(w.snap, a) : w.rect

  useEffect(() => {
    const move = (e) => {
      const d = drag.current
      if (!d) return
      const dx = e.clientX - d.sx, dy = e.clientY - d.sy
      if (d.kind === 'move') {
        if (!d.moved && Math.abs(dx) + Math.abs(dy) < 4) return
        let base = d.rect
        if (!d.moved && d.fromSnap) {
          // Dragging a maximized or snapped window out restores its size, under the
          // pointer at the same proportion along the title bar.
          const fx = (d.sx - d.geo.x) / d.geo.w
          base = { ...d.rect, x: d.sx - fx * d.rect.w, y: d.sy - 12 }
          d.rect = base
          wm.patch(id, { max: false, snap: null })
        }
        // Grabbing from the first real movement, not the press — a click on the
        // title bar should not flicker the cursor.
        if (!d.moved) dragCursor('grabbing')
        d.moved = true
        const zone = zoneAt(e.clientX, e.clientY, wm.area())
        d.zone = zone
        wm.setPreview(zone ? snapRect(zone, wm.area()) : null)
        wm.patch(id, { rect: clampRect({ ...base, x: base.x + dx, y: base.y + dy }, wm.area()) })
      } else {
        const r = { ...d.rect }
        const ed = d.edge
        if (ed.includes('e')) r.w = Math.max(MIN_W, d.rect.w + dx)
        if (ed.includes('s')) r.h = Math.max(MIN_H, d.rect.h + dy)
        if (ed.includes('w')) { const nw = Math.max(MIN_W, d.rect.w - dx); r.x = d.rect.x + d.rect.w - nw; r.w = nw }
        if (ed.includes('n')) { const nh = Math.max(MIN_H, d.rect.h - dy); r.y = Math.max(0, d.rect.y + d.rect.h - nh); r.h = d.rect.y + d.rect.h - r.y }
        wm.patch(id, { rect: r })
      }
    }
    const up = () => {
      const d = drag.current
      if (!d) return
      drag.current = null
      wm.setDragging(false)
      wm.setPreview(null)
      dragCursor(null)
      document.body.style.userSelect = ''
      if (d.kind === 'move' && d.moved && d.zone) {
        if (d.zone === 'max') wm.patch(id, { max: true, snap: null })
        else wm.patch(id, { snap: d.zone })
      }
    }
    addEventListener('pointermove', move)
    addEventListener('pointerup', up)
    // A drag the browser takes away (a touch turned into a scroll, the window losing the
    // pointer) ends the same way, or the cursor would stay grabbing.
    addEventListener('pointercancel', up)
    return () => { removeEventListener('pointermove', move); removeEventListener('pointerup', up); removeEventListener('pointercancel', up) }
  }, [id, wm])

  if (!w || !wm.layerEl) return null

  const start = (kind, edge) => (e) => {
    if (e.button !== 0) return
    if (kind === 'move' && e.target.closest('button, input, select, textarea, a, [data-no-drag]')) return
    if (kind === 'resize' && (w.max || w.snap)) {
      // Resizing a snapped window starts from where it is on screen.
      wm.patch(id, { rect: geo, max: false, snap: null })
    }
    e.preventDefault()
    if (kind === 'resize') dragCursor(RESIZE_CURSOR[edge])
    document.body.style.userSelect = 'none'
    wm.setDragging(true)
    drag.current = { kind, edge, sx: e.clientX, sy: e.clientY, rect: (kind === 'resize' && (w.max || w.snap)) ? geo : w.rect,
      geo, fromSnap: !!(w.max || w.snap) }
  }

  const fullish = w.max
  const btn = 'flex h-6 w-6 items-center justify-center rounded text-muted hover:bg-surface hover:text-fg'
  const ctxItems = [
    ...(menu || []),
    { label: w.max ? 'Restore' : 'Maximize', onClick: () => wm.toggleMax(id) },
    { label: 'Minimize', onClick: () => wm.minimize(id) },
    { label: 'Snap left', onClick: () => wm.patch(id, { snap: 'left', max: false }) },
    { label: 'Snap right', onClick: () => wm.patch(id, { snap: 'right', max: false }) },
    ...(onClose && canClose ? [{ label: 'Close', onClick: onClose, danger: true }] : []),
  ]
  const edges = ['n', 's', 'e', 'w', 'ne', 'nw', 'se', 'sw']
  const edgeStyle = {
    n: 'top-0 inset-x-2 h-1.5 cursor-ns-resize', s: 'bottom-0 inset-x-2 h-1.5 cursor-ns-resize',
    e: 'right-0 inset-y-2 w-1.5 cursor-ew-resize', w: 'left-0 inset-y-2 w-1.5 cursor-ew-resize',
    ne: 'right-0 top-0 h-3 w-3 cursor-nesw-resize', nw: 'left-0 top-0 h-3 w-3 cursor-nwse-resize',
    se: 'right-0 bottom-0 h-3 w-3 cursor-nwse-resize', sw: 'left-0 bottom-0 h-3 w-3 cursor-nesw-resize',
  }
  const active = wm.front === id

  return createPortal(
    <div
      data-wm-window={id}
      data-mirror-private={priv ? '' : undefined}
      className={`pointer-events-auto absolute flex flex-col overflow-hidden border bg-surface shadow-2xl ${fullish ? '' : 'rounded-lg'} ${active ? 'border-primary/40' : ''}`}
      style={{ left: geo.x, top: geo.y, width: geo.w, height: geo.h, zIndex: w.order, visibility: w.min ? 'hidden' : 'visible' }}
      aria-hidden={w.min}
      onPointerDownCapture={() => wm.focus(id)}
    >
      <div
        className={`flex shrink-0 items-center gap-1.5 border-b px-2 py-1 ${active ? 'bg-surface2' : 'bg-surface'}`}
        style={{ cursor: 'default', touchAction: 'none' }}
        onPointerDown={start('move')}
        onDoubleClick={(e) => { if (!e.target.closest('button, input, select, textarea, a')) wm.toggleMax(id) }}
        onContextMenu={(e) => { e.preventDefault(); setCtx({ x: e.clientX, y: e.clientY }) }}
      >
        {icon && <span className="shrink-0 text-muted">{icon}</span>}
        <span className={`truncate text-xs font-medium ${header ? 'mr-2 max-w-[220px] shrink-0' : 'min-w-0 flex-1'}`}>{title}</span>
        {header}
        <button className={btn} title="Minimize" onClick={() => wm.minimize(id)}><Icon.Minus size={13} /></button>
        <button className={btn} title={w.max || w.snap ? 'Restore' : 'Maximize'} onClick={() => wm.toggleMax(id)}>
          {w.max || w.snap ? <Icon.Minimize size={12} /> : <Icon.Maximize size={12} />}
        </button>
        {onClose && (
          <button className={`${btn} hover:!text-danger disabled:cursor-not-allowed disabled:opacity-30`} title={canClose ? 'Close' : closeTitle}
            disabled={!canClose} onClick={onClose}>✕</button>
        )}
      </div>
      <div className={`relative min-h-0 flex-1 ${bodyClass}`}>{children}</div>
      {!fullish && edges.map((ed) => (
        <div key={ed} className={`absolute z-10 ${edgeStyle[ed]}`} style={{ touchAction: 'none' }} onPointerDown={start('resize', ed)} />
      ))}
      {ctx && <MenuAt at={ctx} items={ctxItems} onClose={() => setCtx(null)} />}
    </div>,
    wm.layerEl,
  )
}

function MenuAt({ at, items, onClose }) {
  return createPortal(
    <>
      <div className="fixed inset-0 z-[80]" onPointerDown={onClose} onContextMenu={(e) => { e.preventDefault(); onClose() }} />
      <div className="fixed z-[81] min-w-[170px] rounded-md border bg-surface py-1 text-xs shadow-xl"
        style={{ left: Math.min(at.x, innerWidth - 180), top: Math.min(at.y, innerHeight - 30 * items.length - 12) }}>
        {items.map((it) => (
          <button key={it.label} onClick={() => { it.onClick(); onClose() }}
            className={`block w-full px-3 py-1.5 text-left ${it.danger ? 'text-muted hover:bg-danger/10 hover:text-danger' : 'text-fg hover:bg-surface2'}`}>
            {it.label}
          </button>
        ))}
      </div>
    </>,
    document.body,
  )
}

// Taskbar is the strip along the bottom: a button for every window (click to bring
// it forward, again to minimize it), what other providers put there, and the
// arrangement commands.
function Taskbar() {
  const wm = useWindowManager()
  const list = Object.values(wm.wins).sort((a, b) => a.id.localeCompare(b.id, undefined, { numeric: true }))
  const extras = Object.entries(wm.extras || {})
  const [menu, setMenu] = useState(null)
  return (
    <div data-wm-taskbar className="fixed inset-x-0 bottom-0 z-[44] flex items-center gap-1 border-t bg-surface/95 px-2 backdrop-blur"
      style={{ height: TASKBAR_H }}>
      {wm.start}
      <div className="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto">
        {extras.map(([k, x]) => (
          <button key={k} onClick={x.onClick} title={x.title}
            className={`flex shrink-0 items-center gap-1.5 rounded-md px-2.5 py-1 text-xs ${x.active ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2 hover:text-fg'}`}>
            {x.icon}{x.label}
          </button>
        ))}
        {extras.length > 0 && list.length > 0 && <span className="mx-1 h-5 w-px shrink-0 bg-border" />}
        {list.map((w) => {
          const on = wm.front === w.id
          return (
            <button key={w.id} title={w.title}
              onClick={() => (on ? wm.minimize(w.id) : wm.focus(w.id))}
              onContextMenu={(e) => { e.preventDefault(); setMenu({ x: e.clientX, y: e.clientY - 120, id: w.id }) }}
              className={`flex max-w-[200px] shrink-0 items-center gap-1.5 rounded-md border-b-2 px-2.5 py-1 text-xs ${
                on ? 'border-primary bg-surface2 text-fg' : w.min ? 'border-transparent text-muted/70 hover:bg-surface2' : 'border-transparent text-muted hover:bg-surface2 hover:text-fg'}`}>
              {w.icon && <span className="shrink-0">{w.icon}</span>}
              <span className="truncate">{w.title}</span>
            </button>
          )
        })}
      </div>
      {wm.status}
      {list.length > 0 && (
        <div className="flex shrink-0 items-center gap-0.5 border-l pl-1.5">
          <button onClick={wm.tile} title="Tile the open windows" className="rounded px-2 py-1 text-xs text-muted hover:bg-surface2 hover:text-fg">Tile</button>
          <button onClick={wm.cascade} title="Cascade the open windows" className="rounded px-2 py-1 text-xs text-muted hover:bg-surface2 hover:text-fg">Cascade</button>
          <button onClick={wm.minimizeAll} title="Minimize every window" className="rounded px-2 py-1 text-xs text-muted hover:bg-surface2 hover:text-fg">{wm.desktop ? 'Show desktop' : 'Show page'}</button>
        </div>
      )}
      {menu && (
        <MenuAt at={menu} onClose={() => setMenu(null)} items={[
          { label: 'Bring to front', onClick: () => wm.focus(menu.id) },
          { label: 'Minimize', onClick: () => wm.minimize(menu.id) },
          { label: 'Maximize', onClick: () => wm.maximize(menu.id) },
        ]} />
      )}
    </div>
  )
}

// useTaskbarItem puts one entry of the caller's on the taskbar while item is set.
export function useTaskbarItem(key, item) {
  const wm = useWindowManager()
  const setExtra = wm?.setExtra
  const sig = item ? `${item.label}|${item.active}|${item.title}` : ''
  const ref = useRef(item)
  ref.current = item
  useEffect(() => {
    if (!setExtra) return undefined
    if (!ref.current) { setExtra(key, null); return undefined }
    setExtra(key, { ...ref.current, onClick: () => ref.current?.onClick() })
    return () => setExtra(key, null)
  }, [key, sig, setExtra]) // eslint-disable-line react-hooks/exhaustive-deps
}
