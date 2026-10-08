import { useLayoutEffect, useRef, useState } from 'react'
import { Icon } from './Icons.jsx'
import { fmtBytes } from '../lib/dashApi.js'

// LiveOverlay.jsx — the canvas's Live view in full: the details of a node, beside its card, for the
// card that is selected and any that are pinned (lib/useLiveStates.js polls for the figures). Every
// card carries a strip of its own (components/LiveCard.jsx) — role, health, three bars — so this is
// what you open for the rest, not something every node shows at once: seven of these at once was
// a canvas covered in panels. The details body is shared with the Properties panel's Live tab.
//
// The popups are drawn in screen space over the canvas, not inside its zoomed layer, so their text
// stays readable at any zoom; a leader line ties each one back to its card. Zoomed out, a popup
// drops to the essentials (role, and a bar each for CPU, memory and disk). A popup can be dragged
// out of the way, pinned so it stays while another card is selected, or closed.
//
// Trouble comes first. The header's dot is green, amber when the engine reports a replication
// problem (a stopped channel, a peer it cannot reach — app/livehealth.go), and red when the
// database does not answer or the container is not running; the problems themselves are listed
// under the role. A node that is deployed but not running gets a popup that says only that.
//
// Where a popup opens: the first of a few spots around its card — right, below, left, above; for a
// cluster's member, a slot in a row under its frame first — that covers no card, no popup already
// placed and no edge of the canvas. When every spot covers something, the one that covers least.
// Dragging moves a popup from wherever it was placed.

const POP_W = 196
const POP_W_COMPACT = 152
const GAP = 14

// Green below 70%, amber to 90%, red above — and the number is always printed beside the bar,
// so the colour is never the only way to tell.
export const tone = (pct) => (pct >= 90 ? 'var(--danger)' : pct >= 70 ? 'var(--warning)' : 'var(--success)')

export const rate = (v) => (v == null ? '—' : `${fmtBytes(v)}/s`)
export const iops = (v) => (v == null ? '—' : v >= 1000 ? `${(v / 1000).toFixed(1)}k` : `${Math.round(v)}`)

// swapView: the bar and text for swap. A host with no swap at all says so — "0 B" alone would
// read as "fine" when it really means "the OOM killer, not swap, is what happens next".
export function swapView(d) {
  if (d.swapUsed == null && !d.hostSwapTotal) return null
  const used = d.swapUsed || 0
  const cap = d.swapMax || d.hostSwapTotal || 0
  if (!cap) return { pct: 0, text: 'no swap', title: 'The host has no swap configured: memory pressure ends in the OOM killer, not in swapping.' }
  return { pct: (used / cap) * 100, text: fmtBytes(used), title: `${fmtBytes(used)} swapped out of ${fmtBytes(cap)}${d.swapMax ? ' (container limit)' : ' (host swap)'}` }
}

export const ROLE_LABEL = {
  primary: 'PRIMARY', replica: 'REPLICA', secondary: 'SECONDARY', member: 'MEMBER',
  arbiter: 'ARBITER', router: 'ROUTER', standalone: 'STANDALONE',
}

export function Bar({ label, pct, text, hist }) {
  const p = Math.max(0, Math.min(100, pct || 0))
  return (
    <div className="flex items-center gap-1.5">
      <span className="w-8 shrink-0 text-[10px] font-semibold text-muted">{label}</span>
      <div className="relative h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-surface2">
        <div className="absolute inset-y-0 left-0 rounded-full transition-[width] duration-500" style={{ width: `${p}%`, background: tone(p) }} />
      </div>
      {hist && <Spark values={hist} />}
      <span className="shrink-0 text-right font-mono text-[10px] tabular-nums text-fg" style={{ minWidth: 34 }}>{text}</span>
    </div>
  )
}

// Spark is the last minute or so of a percentage, on a fixed 0–100 scale so a flat 3% does not
// get stretched into a dramatic line.
function Spark({ values }) {
  if (values.length < 2) return null
  const w = 36, h = 12
  const pts = values.map((v, i) => `${(i / (values.length - 1)) * w},${h - (Math.min(100, v) / 100) * h}`).join(' ')
  return (
    <svg width={w} height={h} className="shrink-0" aria-hidden="true">
      <polyline points={pts} fill="none" stroke="var(--primary)" strokeWidth="1.2" strokeLinejoin="round" />
    </svg>
  )
}

export const warnStyle = { background: 'color-mix(in srgb, var(--warning) 20%, transparent)', color: 'var(--warning)' }
export const dangerStyle = { background: 'color-mix(in srgb, var(--danger) 18%, transparent)', color: 'var(--danger)' }

// health is the popup's one-word verdict, for the header dot: down, warn or ok.
export function health(data) {
  if (data.state !== 'running' || data.role?.down) return 'down'
  if (data.role?.problems?.length) return 'warn'
  return 'ok'
}
export const DOT = { down: 'var(--danger)', warn: 'var(--warning)', ok: 'var(--success)' }

// NOT_RUNNING is what a deployed node that is not running says instead of its figures.
export const NOT_RUNNING = {
  stopped: ['NODE STOPPED', 'The container is stopped. Start it from the node’s menu.'],
  error: ['DEPLOY FAILED', 'The node failed to deploy; its log says why.'],
  unreachable: ['NOT RESPONDING', 'The deployment says running, but the container does not answer.'],
}

export function RoleChip({ role, mismatch }) {
  if (!role) return null
  if (role.down) {
    return <span title={role.error || 'The database did not answer.'} className="rounded px-1.5 py-px text-[10px] font-bold tracking-wide" style={dangerStyle}>DB DOWN</span>
  }
  if (role.error) {
    return <span title={role.error} className="rounded bg-surface2 px-1.5 py-px text-[10px] font-semibold text-muted">ROLE ?</span>
  }
  const lead = role.role === 'primary' || (role.role === 'standalone' && role.access === 'rw') || role.role === 'router'
  return (
    <span className="flex items-center gap-1">
      <span className="rounded px-1.5 py-px text-[10px] font-bold tracking-wide"
        style={lead
          ? { background: 'color-mix(in srgb, var(--primary) 16%, transparent)', color: 'var(--primary)' }
          : { background: 'var(--surface2)', color: 'var(--fg)' }}>
        {ROLE_LABEL[role.role] || role.role.toUpperCase()}{role.access ? ` · ${role.access.toUpperCase()}` : ''}
      </span>
      {mismatch && (
        <span title={`The design says ${mismatch}; the server says otherwise — a failover, or a role changed by hand.`}
          className="rounded px-1 py-px text-[10px] font-semibold" style={warnStyle}>
          ≠ design
        </span>
      )}
    </span>
  )
}

// roleDetail is the one line under the chip: the engine's own word for its state, lag, fan-out.
export function roleDetail(r) {
  if (!r || r.error || r.down) return ''
  const bits = []
  if (r.state) bits.push(r.state)
  if (r.lagSec != null) bits.push(`lag ${r.lagSec < 10 ? r.lagSec.toFixed(1) : Math.round(r.lagSec)}s`)
  if (r.replicas != null) bits.push(`${r.replicas} replica${r.replicas === 1 ? '' : 's'}`)
  return bits.join(' · ')
}

// designMismatch says what the design expected when the live role contradicts it. Only a
// member the design marks as primary, or explicitly not, can disagree.
export function designMismatch(node, frame, role) {
  if (!frame || !role || role.error || !node.role) return ''
  const livePrimary = role.role === 'primary'
  const liveFollower = role.role === 'replica' || role.role === 'secondary'
  if (node.role === 'primary' && liveFollower) return 'primary'
  if (node.role !== 'primary' && node.role !== 'arbitrator' && livePrimary) return node.role
  return ''
}

// Problems lists what the engine says is wrong with this node's replication, one line each. Zoomed
// out it is a count; the lines are in its tooltip.
const PROBLEMS_SHOWN = 4

export function Problems({ list, compact }) {
  if (!list?.length) return null
  const all = list.join('\n')
  if (compact) {
    return <span title={all} className="rounded px-1.5 py-px text-[10px] font-bold" style={warnStyle}>⚠ {list.length}</span>
  }
  const more = list.length - PROBLEMS_SHOWN
  return (
    <div className="space-y-0.5" title={all}>
      {list.slice(0, PROBLEMS_SHOWN).map((p) => (
        <div key={p} className="line-clamp-2 text-[10px] leading-tight" style={{ color: 'var(--warning)' }}>⚠ {p}</div>
      ))}
      {more > 0 && <div className="text-[10px] text-muted">+{more} more</div>}
    </div>
  )
}

function Header({ node, data, pinned, onPin, onClose, onDragStart }) {
  return (
    <div className="flex cursor-grab items-center gap-1.5 border-b px-2 py-1 active:cursor-grabbing"
      onPointerDown={(e) => { e.stopPropagation(); onDragStart(e) }}>
      <span className="h-1.5 w-1.5 shrink-0 rounded-full" style={{ background: DOT[health(data)] }} />
      <span className="min-w-0 flex-1 truncate text-[11px] font-semibold text-fg">{node.label}</span>
      <button title={pinned ? 'Unpin: close it when another card is selected' : 'Pin: keep it open while you select other cards'}
        aria-label={`${pinned ? 'Unpin' : 'Pin'} live details for ${node.label}`} aria-pressed={!!pinned}
        onPointerDown={(e) => e.stopPropagation()} onClick={onPin}
        className={`shrink-0 rounded ${pinned ? 'text-primary' : 'text-muted hover:text-fg'}`}>
        <Icon.Pin size={12} />
      </button>
      <button title="Close" aria-label={`Close live panel for ${node.label}`}
        onPointerDown={(e) => e.stopPropagation()} onClick={onClose}
        className="shrink-0 rounded text-muted hover:text-fg">
        <Icon.Close size={12} />
      </button>
    </div>
  )
}

function Popup({ node, frame, data, compact, pos, pinned, onPin, onClose, onDragStart }) {
  const frameStyle = { left: pos.x, top: pos.y, width: compact ? POP_W_COMPACT : POP_W }
  return (
    <div className="pointer-events-auto absolute rounded-lg border bg-surface/95 shadow-lg backdrop-blur" style={frameStyle}
      onPointerDown={(e) => e.stopPropagation()}
      onContextMenu={(e) => e.stopPropagation()}>
      <Header node={node} data={data} pinned={pinned} onPin={onPin} onClose={onClose} onDragStart={onDragStart} />
      <div className="px-2 py-1.5">
        <LiveDetailsBody node={node} frame={frame} data={data} compact={compact} />
      </div>
    </div>
  )
}

// LiveDetailsBody is everything Live knows about one node: its role and what the engine reports
// wrong with it, then CPU (with its last minute), memory, disk, I/O wait, swap, network, read and
// write IOPS, and disk throughput. The canvas's details popup and the Properties panel's Live tab
// both draw this, so the two never disagree.
export function LiveDetailsBody({ node, frame, data, compact }) {
  if (data.state !== 'running') {
    const [label, text] = NOT_RUNNING[data.state] || [String(data.state || '').toUpperCase(), '']
    return (
      <div className="space-y-1">
        <span title={text} className="rounded px-1.5 py-px text-[10px] font-bold tracking-wide" style={dangerStyle}>{label}</span>
        {!compact && text && <div className="text-[10px] text-muted">{text}</div>}
      </div>
    )
  }
  const r = data.role
  const d = data.disk
  const fsPct = d && d.fsTotal ? (d.fsUsed / d.fsTotal) * 100 : null
  const memText = data.memLimit ? `${fmtBytes(data.memUsed)}/${fmtBytes(data.memLimit)}` : fmtBytes(data.memUsed)
  const detail = roleDetail(r)
  const swap = swapView(data)
  const iowaitText = data.iowait == null ? '—' : `${data.iowait < 10 ? data.iowait.toFixed(1) : Math.round(data.iowait)}%`
  return (
    <div className="space-y-1">
      {r && (
        <div className="space-y-0.5">
          <span className="flex flex-wrap items-center gap-1">
            <RoleChip role={r} mismatch={designMismatch(node, frame, r)} />
            {compact && <Problems list={r.problems} compact />}
          </span>
          {!compact && detail && <div className="truncate text-[10px] text-muted" title={detail}>{detail}</div>}
          {!compact && r.down && r.error && (
            <div className="line-clamp-2 text-[10px] leading-tight" style={{ color: 'var(--danger)' }} title={r.error}>{r.error}</div>
          )}
          {!compact && <Problems list={r.problems} />}
        </div>
      )}
      <Bar label="CPU" pct={data.cpuPercent} text={`${Math.round(data.cpuPercent || 0)}%`} hist={compact ? null : data.cpuHist} />
      <Bar label="MEM" pct={data.memPercent} text={compact ? `${Math.round(data.memPercent || 0)}%` : memText} />
      {d && (
        <Bar label="DISK" pct={fsPct} text={compact ? `${Math.round(fsPct || 0)}%` : fmtBytes(d.dataBytes)} />
      )}
      {data.iowait != null && (
        <div title="I/O wait: the share of the last 10 seconds some task in this node was stalled on I/O (pressure stall info).">
          <Bar label="IOW" pct={data.iowait} text={iowaitText} />
        </div>
      )}
      {swap && !compact && (
        <div title={swap.title}><Bar label="SWAP" pct={swap.pct} text={swap.text} /></div>
      )}
      {!compact && (
        <>
          {d && (
            <div className="truncate text-[10px] text-muted" title={`${d.path} holds ${fmtBytes(d.dataBytes)}; the filesystem under it is ${fmtBytes(d.fsUsed)} of ${fmtBytes(d.fsTotal)} used`}>
              {d.path} · fs {Math.round(fsPct || 0)}% of {fmtBytes(d.fsTotal)}
            </div>
          )}
          <div className="flex items-center justify-between gap-1 font-mono text-[10px] tabular-nums text-fg">
            <span className="font-sans font-semibold text-muted">NET</span>
            <span title="Network in">↓ {rate(data.netIn)}</span>
            <span title="Network out">↑ {rate(data.netOut)}</span>
          </div>
          <div className="flex items-center justify-between gap-1 font-mono text-[10px] tabular-nums text-fg"
            title={data.readIops == null && data.writeIops == null ? 'Operation counts need two samples, and a kernel that reports them per container (cgroup v2).' : undefined}>
            <span className="font-sans font-semibold text-muted">IOPS</span>
            <span title="Read operations per second">R {iops(data.readIops)}</span>
            <span title="Write operations per second">W {iops(data.writeIops)}</span>
          </div>
          <div className="flex items-center justify-between gap-1 font-mono text-[10px] tabular-nums text-fg">
            <span className="font-sans font-semibold text-muted">I/O</span>
            <span title="Disk read">R {rate(data.diskRead)}</span>
            <span title="Disk write">W {rate(data.diskWrite)}</span>
          </div>
        </>
      )}
    </div>
  )
}

// popupHeight estimates a popup's height for placement — close enough to keep popups apart; a
// measured height would move them as their contents load, which is worse than a few pixels' slack.
function popupHeight(data, compact) {
  if (data.state !== 'running') return 26 + 12 + 18 + (compact ? 0 : 28)
  const hasRole = !!data.role
  const probs = !compact && data.role?.problems?.length
    ? Math.min(data.role.problems.length, PROBLEMS_SHOWN) * 26 + (data.role.problems.length > PROBLEMS_SHOWN ? 14 : 0)
    : 0
  const downLine = !compact && data.role?.down && data.role?.error ? 26 : 0
  const detail = hasRole && !compact && roleDetail(data.role)
  const bars = 2 + (data.disk ? 1 : 0) + (data.iowait != null ? 1 : 0) + (!compact && swapView(data) ? 1 : 0)
  const iopsRow = 16
  return 26 + 12 + (hasRole ? 20 : 0) + (detail ? 14 : 0) + probs + downLine + bars * 18 + (compact ? 0 : (data.disk ? 14 : 0) + 32 + iopsRow)
}

const overlap = (a, b) =>
  Math.max(0, Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x)) * Math.max(0, Math.min(a.y + a.h, b.y + b.h) - Math.max(a.y, b.y))

// closest is the point of rect r nearest to (x, y) — where a leader line meets a card or popup.
const closest = (r, x, y) => ({ x: Math.min(Math.max(x, r.x), r.x + r.w), y: Math.min(Math.max(y, r.y), r.y + r.h) })

// LiveOverlay places a Popup for each node in show (the selected card and the pinned ones) that has
// live data. Every card is an obstacle, shown or not. sizes: { node: [w, h], member: [w, h] } — the
// card sizes in canvas units; blocks are other world-space rectangles to keep clear (the cluster
// tables).
export default function LiveOverlay({ nodes, frames, view, live, sizes, show, pinned, blocks = [], offsets, onClose, onPin, onMove }) {
  const drag = useRef(null)
  const box = useRef(null)
  const [area, setArea] = useState({ w: 0, h: 0 })
  useLayoutEffect(() => {
    const el = box.current?.parentElement
    if (!el) return undefined
    const measure = () => setArea({ w: el.clientWidth, h: el.clientHeight })
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])
  const compact = view.z < 0.7
  const w = compact ? POP_W_COMPACT : POP_W
  const frameById = Object.fromEntries(frames.map((f) => [f.id, f]))
  const sx = (x) => view.x + x * view.z
  const sy = (y) => view.y + y * view.z
  const cardRect = (n) => {
    const [cw, ch] = n.frameId ? sizes.member : sizes.node
    return { x: sx(n.x), y: sy(n.y), w: cw * view.z, h: ch * view.z }
  }

  // What a popup should not cover: every card, every frame's title bar, the cluster tables, popups
  // already placed.
  const obstacles = nodes.map(cardRect)
  for (const f of frames) obstacles.push({ x: sx(f.x), y: sy(f.y), w: f.w * view.z, h: 28 * view.z })
  for (const b of blocks) obstacles.push({ x: sx(b.x), y: sy(b.y), w: b.w * view.z, h: b.h * view.z })

  const order = nodes.filter((n) => live[n.id] && show.has(n.id)).sort((a, b) => a.x - b.x || a.y - b.y)
  const items = []
  for (const n of order) {
    const data = live[n.id]
    const f = n.frameId ? frameById[n.frameId] : null
    const card = cardRect(n)
    const h = popupHeight(data, compact)
    // Docked beside its own card: right, left, below, above — the first that covers nothing.
    const cands = [
      { x: card.x + card.w + GAP, y: card.y - 4 },
      { x: card.x - w - GAP, y: card.y - 4 },
      { x: card.x, y: card.y + card.h + GAP },
      { x: card.x, y: card.y - h - GAP },
    ]
    let best = null
    for (const c of cands) {
      const r = { x: c.x, y: c.y, w, h }
      let cost = 0
      for (const o of obstacles) cost += overlap(r, o)
      if (area.w) {
        const inside = overlap(r, { x: 0, y: 0, w: area.w, h: area.h })
        cost += (w * h - inside) * 2 // off the canvas is worse than over a card
      }
      if (!best || cost < best.cost) best = { r, cost }
      if (cost === 0) break
    }
    obstacles.push(best.r)
    const off = offsets[n.id] || { x: 0, y: 0 }
    const pos = { x: best.r.x + off.x, y: best.r.y + off.y }
    const pr = { x: pos.x, y: pos.y, w, h }
    const to = closest(pr, card.x + card.w / 2, card.y + card.h / 2)
    const from = closest(card, to.x, to.y)
    items.push({ n, f, data, pos, from, to })
  }

  function startDrag(e, id) {
    const off = offsets[id] || { x: 0, y: 0 }
    drag.current = { id, sx: e.clientX, sy: e.clientY, ox: off.x, oy: off.y }
    const move = (ev) => {
      const d = drag.current
      if (d) onMove(d.id, { x: d.ox + ev.clientX - d.sx, y: d.oy + ev.clientY - d.sy })
    }
    const up = () => {
      drag.current = null
      removeEventListener('pointermove', move)
      removeEventListener('pointerup', up)
    }
    addEventListener('pointermove', move)
    addEventListener('pointerup', up)
  }

  return (
    <div ref={box} className="pointer-events-none absolute inset-0 z-10 overflow-hidden">
      <svg className="absolute inset-0 h-full w-full" aria-hidden="true">
        {items.map(({ n, from, to }) => (
          <line key={n.id} x1={from.x} y1={from.y} x2={to.x} y2={to.y}
            stroke="var(--muted)" strokeOpacity="0.55" strokeWidth="1" strokeDasharray="3 3" />
        ))}
      </svg>
      {items.map(({ n, f, data, pos }) => (
        <Popup key={n.id} node={n} frame={f} data={data} compact={compact} pos={pos}
          pinned={pinned.has(n.id)} onPin={() => onPin(n.id)}
          onClose={() => onClose(n.id)} onDragStart={(e) => startDrag(e, n.id)} />
      ))}
    </div>
  )
}
