import { useLayoutEffect, useRef, useState } from 'react'

// TimeChart — a dependency-free SVG timeline chart (line or stacked-area) for the Stalk
// Summary. One y-axis only. Grid/axis/text use the app's theme CSS vars; the categorical
// series palette is the validated dataviz reference palette, picked light/dark by surface
// luminance so it adapts to any theme. Legend + hover tooltip carry series identity (never
// color alone), satisfying the palette's relief requirement.

// Validated categorical palette (dataviz reference) — fixed order, never cycled.
const LIGHT = ['#2a78d6', '#1baf7a', '#eda100', '#008300', '#4a3aa7', '#e34948', '#e87ba4', '#eb6834']
const DARK = ['#3987e5', '#199e70', '#c98500', '#008300', '#9085e9', '#e66767', '#d55181', '#d95926']

function isDarkSurface() {
  if (typeof document === 'undefined') return false
  const bg = getComputedStyle(document.documentElement).getPropertyValue('--bg').trim().replace('#', '')
  if (bg.length < 6) return false
  const r = parseInt(bg.slice(0, 2), 16), g = parseInt(bg.slice(2, 4), 16), b = parseInt(bg.slice(4, 6), 16)
  return 0.299 * r + 0.587 * g + 0.114 * b < 128
}

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
function fmtTime(t) {
  const d = new Date(t * 1000)
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false })
}
// fmtDay prefixes the date. A capture or a log excerpt routinely spans days — a mongod
// writes diagnostic.data continuously and a 200,000-line tail reaches back as far as the
// server was quiet — and an axis labelled 03:14:02 in that window names several different
// moments without saying which.
function fmtDay(t) {
  const d = new Date(t * 1000)
  return `${String(d.getDate()).padStart(2, '0')} ${MONTHS[d.getMonth()]}`
}
// fmtAxis labels one tick.
//
// The date is on the FIRST tick always, and on any tick that starts a new day. Not on every
// tick — five copies of "18 Aug" is noise that squeezes out the times — and not only when
// the window crosses midnight, which was the first attempt and left a zoomed-in chart
// labelled 07:22:02 with nothing anywhere on it saying which day that was. An axis has to
// answer "when" on its own, because a chart gets read, screenshotted and pasted into a
// ticket without the header that sat above it.
function fmtAxis(t, i, prev) {
  const dated = i === 0 || (prev != null && new Date(t * 1000).toDateString() !== new Date(prev * 1000).toDateString())
  return dated ? `${fmtDay(t)} ${fmtTime(t).slice(0, 5)}` : fmtTime(t)
}
function fmtNum(v) {
  if (v === null || v === undefined || Number.isNaN(v)) return '—'
  const a = Math.abs(v)
  if (a >= 1e9) return (v / 1e9).toFixed(1) + 'B'
  if (a >= 1e6) return (v / 1e6).toFixed(1) + 'M'
  if (a >= 1e3) return (v / 1e3).toFixed(1) + 'k'
  if (a >= 100) return v.toFixed(0)
  if (a >= 1) return v.toFixed(1)
  if (a === 0) return '0'
  return v.toFixed(2)
}
// fmtBytes renders a byte figure human-readable (KB/MB/GB, base-1024).
function fmtBytes(v) {
  if (v === null || v === undefined || Number.isNaN(v)) return '—'
  const a = Math.abs(v)
  if (a < 1024) return v.toFixed(0) + ' B'
  const u = ['KB', 'MB', 'GB', 'TB']
  let n = v, i = -1
  do { n /= 1024; i++ } while (Math.abs(n) >= 1024 && i < u.length - 1)
  return n.toFixed(1) + ' ' + u[i]
}
const isBytes = (unit) => unit === 'B' || unit === 'B/s'

// lines: [{ key, label, color }]  color = palette slot index (0..7).
// kind: 'line' | 'stacked'. points: [{ t, v:{key:num} }].
// `onZoom(from, to)` makes the chart draggable: select a stretch of time and the caller
// re-reads the source for exactly that window. Optional, and only the FTDC Summary passes
// it — a chart that silently changes what it shows when you drag across it would be a
// surprise everywhere else.
export default function TimeChart({ points, lines, unit = '', kind = 'line', height = 168, onZoom = null }) {
  const wrapRef = useRef(null)
  const [w, setW] = useState(560)
  const [hover, setHover] = useState(null)
  const [drag, setDrag] = useState(null) // { from, to } in timestamps, while dragging

  useLayoutEffect(() => {
    if (!wrapRef.current) return
    const ro = new ResizeObserver((entries) => {
      for (const e of entries) setW(Math.max(240, Math.floor(e.contentRect.width)))
    })
    ro.observe(wrapRef.current)
    return () => ro.disconnect()
  }, [])

  const pal = isDarkSurface() ? DARK : LIGHT
  const colorOf = (i) => pal[i % pal.length]
  const pad = { l: 44, r: 12, t: 10, b: 22 }
  const H = height
  const iw = Math.max(10, w - pad.l - pad.r)
  const ih = Math.max(10, H - pad.t - pad.b)

  if (!points || points.length === 0) {
    return <div ref={wrapRef} className="text-xs text-muted py-6 text-center">no data points</div>
  }

  const ts = points.map((p) => p.t)
  const tMin = Math.min(...ts), tMax = Math.max(...ts)
  const tSpan = tMax - tMin || 1
  const x = (t) => pad.l + ((t - tMin) / tSpan) * iw

  // y domain: stacked → max stack sum; line → max across all plotted keys.
  let yMax = 0
  for (const p of points) {
    if (kind === 'stacked') {
      let s = 0
      for (const ln of lines) s += Math.max(p.v[ln.key] || 0, 0)
      yMax = Math.max(yMax, s)
    } else {
      for (const ln of lines) yMax = Math.max(yMax, p.v[ln.key] || 0)
    }
  }
  yMax = niceMax(yMax)
  const y = (v) => pad.t + ih - (v / yMax) * ih

  // Gap detection: break the line where dt is much larger than the typical step.
  const dts = []
  for (let i = 1; i < points.length; i++) dts.push(points[i].t - points[i - 1].t)
  const medDt = median(dts) || 1
  const gap = Math.max(medDt * 4, 3)
  const segments = [] // arrays of indices with no big gaps
  let seg = [0]
  for (let i = 1; i < points.length; i++) {
    if (points[i].t - points[i - 1].t > gap) { segments.push(seg); seg = [] }
    seg.push(i)
  }
  segments.push(seg)

  // Build paths.
  const linePaths = []
  const areaPaths = []
  if (kind === 'stacked') {
    // cumulative stack per point
    const cum = points.map(() => 0)
    for (let li = 0; li < lines.length; li++) {
      const ln = lines[li]
      for (const s of segments) {
        if (s.length === 0) continue
        let top = '', bot = ''
        for (let j = 0; j < s.length; j++) {
          const idx = s[j]
          const base = cum[idx]
          const val = Math.max(points[idx].v[ln.key] || 0, 0)
          const px = x(points[idx].t)
          top += `${j === 0 ? 'M' : 'L'}${px},${y(base + val)} `
        }
        for (let j = s.length - 1; j >= 0; j--) {
          const idx = s[j]
          bot += `L${x(points[idx].t)},${y(cum[idx])} `
        }
        areaPaths.push({ d: top + bot + 'Z', color: colorOf(ln.color) })
      }
      for (const idx of points.map((_, i) => i)) cum[idx] += Math.max(points[idx].v[ln.key] || 0, 0)
    }
  } else {
    for (const ln of lines) {
      for (const s of segments) {
        if (s.length === 0) continue
        let d = ''
        s.forEach((idx, j) => { d += `${j === 0 ? 'M' : 'L'}${x(points[idx].t)},${y(points[idx].v[ln.key] || 0)} ` })
        linePaths.push({ d, color: colorOf(ln.color) })
      }
    }
  }

  const sparse = points.length <= 14
  const yTicks = ticks(0, yMax, 4)
  const xTickN = Math.min(w < 360 ? 3 : 5, points.length)
  const xTicks = []
  for (let i = 0; i < xTickN; i++) xTicks.push(tMin + (tSpan * i) / (xTickN - 1 || 1))

  // Screen x → timestamp, clamped to the drawn range so a drag off the edge selects to
  // the end rather than to nothing.
  function tsAt(clientX) {
    const rect = wrapRef.current.getBoundingClientRect()
    const px = ((clientX - rect.left) / rect.width) * w
    return Math.min(tMax, Math.max(tMin, tMin + ((px - pad.l) / iw) * tSpan))
  }

  function onMove(e) {
    const tt = tsAt(e.clientX)
    let best = 0, bd = Infinity
    for (let i = 0; i < points.length; i++) { const d = Math.abs(points[i].t - tt); if (d < bd) { bd = d; best = i } }
    setHover(best)
    if (drag) setDrag({ ...drag, to: tt })
  }

  function onDown(e) {
    if (!onZoom || e.button !== 0) return
    setDrag({ from: tsAt(e.clientX), to: tsAt(e.clientX) })
  }

  function onUp() {
    if (!drag) return
    const a = Math.min(drag.from, drag.to)
    const b = Math.max(drag.from, drag.to)
    setDrag(null)
    // A click is not a zoom. Ten seconds is the floor because FTDC samples once a second
    // and a window narrower than that holds nothing to draw.
    if (onZoom && b - a >= 10) onZoom(a, b)
  }

  const showLegend = lines.length >= 1
  const hv = hover != null ? points[hover] : null
  const fmtV = isBytes(unit) ? fmtBytes : fmtNum
  const suffix = (v) => (unit === '%' ? v + '%' : v)

  return (
    <div ref={wrapRef} className="relative w-full">
      <svg width={w} height={H} role="img"
        style={{ display: 'block', maxWidth: '100%', cursor: onZoom ? 'col-resize' : 'default' }}
        onMouseMove={onMove} onMouseLeave={() => { setHover(null); setDrag(null) }}
        onMouseDown={onDown} onMouseUp={onUp}>
        {/* gridlines + y labels */}
        {yTicks.map((t, i) => (
          <g key={i}>
            <line x1={pad.l} x2={w - pad.r} y1={y(t)} y2={y(t)} stroke="var(--grid)" strokeWidth="1" />
            <text x={pad.l - 6} y={y(t) + 3} textAnchor="end" fontSize="10" fill="var(--muted)">{fmtV(t)}</text>
          </g>
        ))}
        {/* the stretch being dragged out for a zoom */}
        {drag && Math.abs(drag.to - drag.from) > 0 && (
          <rect x={x(Math.min(drag.from, drag.to))} y={pad.t}
            width={Math.max(1, Math.abs(x(drag.to) - x(drag.from)))} height={ih}
            fill="var(--primary)" fillOpacity="0.18" stroke="var(--primary)" strokeWidth="1" />
        )}
        {/* x labels */}
        {xTicks.map((t, i) => (
          <text key={i} x={x(t)} y={H - 6} textAnchor={i === 0 ? 'start' : i === xTicks.length - 1 ? 'end' : 'middle'}
            fontSize="10" fill="var(--muted)">{fmtAxis(t, i, i > 0 ? xTicks[i - 1] : null)}</text>
        ))}
        {/* areas (stacked) with a 2px surface gap between fills */}
        {areaPaths.map((p, i) => (
          <path key={i} d={p.d} fill={p.color} fillOpacity="0.82" stroke="var(--surface)" strokeWidth="0.75" />
        ))}
        {/* lines */}
        {linePaths.map((p, i) => (
          <path key={i} d={p.d} fill="none" stroke={p.color} strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
        ))}
        {/* markers for sparse line series */}
        {sparse && kind !== 'stacked' && lines.map((ln) => points.map((p, i) => (
          <circle key={`${ln.key}-${i}`} cx={x(p.t)} cy={y(p.v[ln.key] || 0)} r="2.5" fill={colorOf(ln.color)} stroke="var(--surface)" strokeWidth="1" />
        )))}
        {/* hover crosshair */}
        {hv && (
          <line x1={x(hv.t)} x2={x(hv.t)} y1={pad.t} y2={pad.t + ih} stroke="var(--muted)" strokeWidth="1" strokeDasharray="3 3" />
        )}
      </svg>

      {hv && (
        <div className="pointer-events-none absolute z-10 rounded-lg border bg-surface px-2 py-1.5 text-[11px] shadow-lg"
          style={{ left: Math.min(Math.max(x(hv.t) / w * 100, 4), 74) + '%', top: 4 }}>
          <div className="mb-0.5 font-medium text-fg">{`${fmtDay(hv.t)} ${fmtTime(hv.t)}`}</div>
          {lines.map((ln) => (
            <div key={ln.key} className="flex items-center gap-1.5 whitespace-nowrap">
              <span className="inline-block h-2 w-2 rounded-sm" style={{ background: colorOf(ln.color) }} />
              <span className="text-muted">{ln.label}</span>
              <span className="ml-auto font-mono text-fg">{suffix(fmtV(hv.v[ln.key] || 0))}</span>
            </div>
          ))}
        </div>
      )}

      {showLegend && (
        <div className="mt-1 flex flex-wrap gap-x-3 gap-y-1">
          {lines.map((ln) => (
            <span key={ln.key} className="inline-flex items-center gap-1.5 text-[11px] text-muted">
              <span className="inline-block h-2 w-2 rounded-sm" style={{ background: colorOf(ln.color) }} />
              {ln.label}
            </span>
          ))}
          {unit && <span className="ml-auto text-[11px] text-muted">{unit === '%' ? '%' : `(${unit})`}</span>}
        </div>
      )}
    </div>
  )
}

function niceMax(v) {
  if (v <= 0) return 1
  const p = Math.pow(10, Math.floor(Math.log10(v)))
  const n = v / p
  const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10
  return step * p
}
function ticks(min, max, n) {
  const out = []
  for (let i = 0; i <= n; i++) out.push(min + ((max - min) * i) / n)
  return out
}
function median(a) {
  if (!a.length) return 0
  const s = [...a].sort((x, y) => x - y)
  return s[Math.floor(s.length / 2)]
}
