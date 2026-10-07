import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { useSession } from './SessionProvider.jsx'

// DrawLayer — drawing on the shared screen (app/sharedraw.go).
//
// Everyone in a session can draw on what they are all looking at: a freehand line,
// or a line of text put down where they click; and rub out what they drew — the host,
// anything. The marks are the hub's, so a guest who arrives late sees them too.
//
// A mark is stored in fractions of the frame it was drawn on, never in pixels, so it
// lands on the same spot of a screen of another size. The frame is the workspace —
// everything left of the session panel — and inside a mirror it is the driver's
// workspace within their mirrored screen (MirrorView puts a DrawLayer there, sized to
// the frame the driver publishes as follow state, `drawFrame`). Line width and text
// size are fractions of the frame's height, so a mark keeps its weight when scaled.
//
// The layer catches the pointer only while this browser is drawing; otherwise it is
// glass, and the workspace under it works as usual. It is kept out of the mirror
// stream ([data-mirror-private]): every viewer draws the marks for themselves.

export const DRAW_COLORS = ['#ef4444', '#f59e0b', '#22c55e', '#3b82f6', '#a855f7', '#ec4899', '#111827', '#ffffff']
const WIDTH = { s: 0.003, m: 0.006, l: 0.012 }
const TEXT_SIZE = { s: 0.022, m: 0.032, l: 0.05 }
const SEND_MS = 70
const ERASER_PROBES = [[0, 0], [8, 0], [-8, 0], [0, 8], [0, -8], [6, 6], [-6, -6], [6, -6], [-6, 6]]

const clamp = (v) => Math.min(1, Math.max(0, v))
const newId = () => `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`

// A line through the points, smoothed: each point is a control point and the curve
// passes through the midpoints between them.
function linePath(points, w, h) {
  const p = points.map(([x, y]) => [x * w, y * h])
  if (p.length === 1) return `M${p[0][0]} ${p[0][1]}L${p[0][0]} ${p[0][1]}`
  let d = `M${p[0][0]} ${p[0][1]}`
  for (let i = 1; i < p.length - 1; i++) {
    const mx = (p[i][0] + p[i + 1][0]) / 2
    const my = (p[i][1] + p[i + 1][1]) / 2
    d += `Q${p[i][0]} ${p[i][1]} ${mx} ${my}`
  }
  const last = p[p.length - 1]
  return `${d}L${last[0]} ${last[1]}`
}

export function Mark({ m, w, h, erasable }) {
  if (m.kind === 'pen') {
    const d = linePath(m.points || [], w, h)
    const sw = Math.max(1, m.width * h)
    return (
      <g data-mark={m.id}>
        <title>{m.name}</title>
        <path d={d} fill="none" stroke={m.color} strokeWidth={sw} strokeLinecap="round" strokeLinejoin="round" />
        {erasable && <path d={d} fill="none" stroke="transparent" strokeWidth={Math.max(20, sw + 12)} strokeLinecap="round" pointerEvents="stroke" />}
      </g>
    )
  }
  const size = Math.max(8, m.size * h)
  // A halo in the opposite tone keeps a line of text legible on any background.
  const halo = m.color === '#ffffff' ? '#111827' : '#ffffff'
  return (
    <g data-mark={m.id}>
      <title>{m.name}</title>
      <text
        x={m.x * w} y={m.y * h} fontSize={size} fill={m.color} dominantBaseline="text-before-edge"
        stroke={halo} strokeWidth={size * 0.14} strokeLinejoin="round" paintOrder="stroke"
        fontWeight="600" style={{ whiteSpace: 'pre', userSelect: 'none' }}
        pointerEvents={erasable ? 'bounding-box' : 'none'}
      >
        {m.text}
      </text>
    </g>
  )
}

// DrawLayer fills its parent. publishFrame: this layer is the frame others' mirrors
// size theirs to (the workspace layer, not the one inside a mirror).
export function DrawLayer({ publishFrame = false }) {
  const s = useSession()
  const ref = useRef(null)
  const [box, setBox] = useState({ w: 0, h: 0 })
  const [draft, setDraft] = useState(null) // the line being drawn
  const [typing, setTyping] = useState(null) // { x, y, text }
  const live = useRef(null)
  const erasing = useRef(false)
  const api = useRef(s)
  api.current = s

  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return undefined
    const measure = () => setBox({ w: el.offsetWidth, h: el.offsetHeight })
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  const { publishFollow } = s
  useEffect(() => {
    if (publishFrame && box.w && box.h) publishFollow({ drawFrame: { w: box.w, h: box.h } })
  }, [publishFrame, box.w, box.h, publishFollow])

  const active = s.drawing && s.canDraw
  const { tool, color, size } = s.pen
  const myId = s.me?.guestId ?? 0

  // Leaving drawing mode, or switching tool, puts down what was being typed.
  const commitText = (t = typing) => {
    if (t && t.text.trim()) {
      api.current.drawMark({ id: newId(), kind: 'text', color, size: TEXT_SIZE[size], x: t.x, y: t.y, text: t.text.trim().slice(0, 500) })
    }
    setTyping(null)
  }
  useEffect(() => { if (!active || tool !== 'text') commitText() }, [active, tool]) // eslint-disable-line react-hooks/exhaustive-deps

  const at = (e) => {
    const r = ref.current.getBoundingClientRect()
    return [clamp((e.clientX - r.left) / r.width), clamp((e.clientY - r.top) / r.height)]
  }

  // The eraser is a disc, not a point: what is under the pointer or a few pixels
  // around it goes, so a thin line is not a game of precision.
  const eraseAt = (e) => {
    const ids = new Set()
    for (const [dx, dy] of ERASER_PROBES) {
      for (const el of document.elementsFromPoint(e.clientX + dx, e.clientY + dy)) {
        const id = el.closest?.('[data-mark]')?.getAttribute('data-mark')
        if (!id || ids.has(id)) continue
        const m = api.current.marks.find((x) => x.id === id)
        if (m && (api.current.isHost || m.by === myId)) ids.add(id)
      }
    }
    if (ids.size) api.current.eraseMarks([...ids])
  }

  const onPointerDown = (e) => {
    if (!active || e.button !== 0) return
    if (tool === 'text' && e.target.closest('input')) return
    e.preventDefault()
    const p = at(e)
    if (tool === 'text') {
      commitText()
      setTyping({ x: p[0], y: p[1], text: '' })
      return
    }
    ref.current.setPointerCapture?.(e.pointerId)
    if (tool === 'eraser') {
      erasing.current = true
      eraseAt(e)
      return
    }
    const mark = { id: newId(), kind: 'pen', color, width: WIDTH[size], points: [p] }
    live.current = { mark, sent: Date.now() }
    setDraft(mark)
    api.current.drawMark(mark)
  }
  const onPointerMove = (e) => {
    if (erasing.current) { eraseAt(e); return }
    const l = live.current
    if (!l) return
    const p = at(e)
    const last = l.mark.points[l.mark.points.length - 1]
    if (Math.hypot(p[0] - last[0], p[1] - last[1]) < 0.0015 || l.mark.points.length >= 4000) return
    l.mark = { ...l.mark, points: [...l.mark.points, p] }
    setDraft(l.mark)
    // Everyone else sees the line grow, a few times a second.
    if (Date.now() - l.sent >= SEND_MS) {
      l.sent = Date.now()
      api.current.drawMark(l.mark)
    }
  }
  const onPointerUp = () => {
    erasing.current = false
    const l = live.current
    if (!l) return
    live.current = null
    api.current.drawMark(l.mark)
    setDraft(null)
  }

  const cursor = !active ? undefined : tool === 'text' ? 'text' : tool === 'eraser' ? 'cell' : 'crosshair'
  const { w, h } = box
  return (
    <div
      ref={ref}
      className="absolute inset-0"
      style={{ pointerEvents: active ? 'auto' : 'none', cursor, touchAction: active ? 'none' : undefined }}
      onPointerDown={onPointerDown} onPointerMove={onPointerMove} onPointerUp={onPointerUp} onPointerCancel={onPointerUp}
      aria-hidden={!active}
    >
      {w > 0 && (
        <svg width={w} height={h} viewBox={`0 0 ${w} ${h}`} className="absolute left-0 top-0 overflow-visible" style={{ pointerEvents: 'none' }}>
          {s.marks.map((m) => (draft && m.id === draft.id ? null : (
            <Mark key={m.id} m={m} w={w} h={h} erasable={active && tool === 'eraser' && (s.isHost || m.by === myId)} />
          )))}
          {draft && <Mark m={draft} w={w} h={h} />}
        </svg>
      )}
      {typing && (
        <input
          autoFocus
          value={typing.text}
          maxLength={500}
          onChange={(e) => setTyping({ ...typing, text: e.target.value })}
          onKeyDown={(e) => {
            if (e.key === 'Enter') { e.preventDefault(); commitText() }
            if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); setTyping(null) }
          }}
          onBlur={() => commitText()}
          placeholder="Type, then Enter"
          className="absolute rounded border border-dashed border-current bg-transparent px-0.5 font-semibold outline-none placeholder:opacity-60"
          style={{
            left: typing.x * w, top: typing.y * h, color,
            fontSize: Math.max(8, TEXT_SIZE[size] * h), lineHeight: 1.1,
            width: `${Math.max(8, typing.text.length + 2)}ch`, maxWidth: Math.max(80, (1 - typing.x) * w),
          }}
        />
      )}
    </div>
  )
}

// WorkspaceDrawing is the layer over this browser's own workspace (not a mirror),
// with the toolbar while drawing. right is the width the session panel takes.
export function WorkspaceDrawing({ right = 0 }) {
  const s = useSession()
  if (!s.active || s.mirroring) return s.active && s.drawing ? <DrawToolbar right={right} /> : null
  return (
    <>
      <div data-mirror-private className="pointer-events-none fixed left-0 top-0 bottom-0" style={{ right, zIndex: 60 }}>
        <DrawLayer publishFrame />
      </div>
      {s.drawing && <DrawToolbar right={right} />}
    </>
  )
}

function ToolBtn({ on, title, onClick, children }) {
  return (
    <button
      type="button" title={title} aria-pressed={on} onClick={onClick}
      className={`rounded-md p-1.5 ${on ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2 hover:text-fg'}`}
    >
      {children}
    </button>
  )
}

// DrawToolbar floats at the top of the workspace while this browser draws.
export function DrawToolbar({ right = 0 }) {
  const s = useSession()
  const { tool, color, size } = s.pen
  const setPen = (patch) => s.setPen({ ...s.pen, ...patch })

  useEffect(() => {
    const onKey = (e) => {
      if (e.key === 'Escape' && !e.target.closest?.('input, textarea')) s.setDrawing(false)
    }
    addEventListener('keydown', onKey)
    return () => removeEventListener('keydown', onKey)
  }, [s.setDrawing]) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div data-mirror-private className="pointer-events-none fixed left-0 top-16 flex justify-center px-4" style={{ right, zIndex: 9100 }}>
      <div className="pointer-events-auto flex flex-wrap items-center gap-1 rounded-xl border bg-surface/95 px-2 py-1 text-xs shadow-lg backdrop-blur" role="toolbar" aria-label="Drawing tools">
        {!s.canDraw ? (
          <span className="px-1 text-muted">The host has turned drawing off for you.</span>
        ) : (
          <>
            <ToolBtn on={tool === 'pen'} title="Pen — draw a line" onClick={() => setPen({ tool: 'pen' })}><Icon.Pencil size={16} /></ToolBtn>
            <ToolBtn on={tool === 'text'} title="Text — click where it goes, type, then Enter" onClick={() => setPen({ tool: 'text' })}><Icon.Text size={16} /></ToolBtn>
            <ToolBtn on={tool === 'eraser'} title={s.isHost ? 'Eraser — rub out any drawing' : 'Eraser — rub out your own drawings'} onClick={() => setPen({ tool: 'eraser' })}><Icon.Eraser size={16} /></ToolBtn>
            <span className="mx-1 h-5 w-px bg-border" />
            {DRAW_COLORS.map((c) => (
              <button
                key={c} type="button" title={c} aria-pressed={color === c} onClick={() => setPen({ color: c, tool: tool === 'eraser' ? 'pen' : tool })}
                className={`h-5 w-5 rounded-full border ${color === c ? 'ring-2 ring-primary ring-offset-1 ring-offset-surface' : ''}`}
                style={{ background: c }}
              />
            ))}
            <span className="mx-1 h-5 w-px bg-border" />
            {['s', 'm', 'l'].map((z) => (
              <ToolBtn key={z} on={size === z} title={{ s: 'Thin', m: 'Medium', l: 'Thick' }[z]} onClick={() => setPen({ size: z })}>
                <span className="block rounded-full bg-current" style={{ width: { s: 4, m: 7, l: 11 }[z], height: { s: 4, m: 7, l: 11 }[z] }} />
              </ToolBtn>
            ))}
            <span className="mx-1 h-5 w-px bg-border" />
            <button type="button" className="rounded-md px-2 py-1 text-muted hover:bg-surface2 hover:text-fg" onClick={() => s.clearMarks(false)}>Clear mine</button>
            {s.isHost && (
              <button type="button" className="rounded-md px-2 py-1 text-danger hover:bg-danger/10" onClick={() => s.clearMarks(true)}>Clear all</button>
            )}
          </>
        )}
        <button type="button" title="Stop drawing (Esc)" className="ml-1 rounded-md px-2 py-1 font-medium text-primary hover:bg-primary/10" onClick={() => s.setDrawing(false)}>Done</button>
      </div>
    </div>
  )
}
