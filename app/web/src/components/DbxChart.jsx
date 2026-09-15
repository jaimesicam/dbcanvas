import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { Icon } from './Icons.jsx'
import { Badge } from './ui.jsx'
import {
  AGGREGATIONS, CHART_TYPES, buildChartData, download, fmtNumber, isNumericCol,
  suggestChart,
} from '../lib/dbxApi.js'

// DbxChart — the Chart view over a result set.
//
// Dependency-free SVG, like TimeChart: the app has no charting library and this
// feature does not add one. What a chart here needs — a handful of marks, an axis, a
// legend and a tooltip — is less code than the adapter layer that decides whether a
// library's licence is compatible with GPL-3.0.
//
// The rule the panel is built around: a chart is a *view of the result*, never a
// change to the query. Grouping, Top N and sorting all happen in the browser over
// rows the database already returned, and when any of them is on, the panel says so
// in the header — so a bar labelled 4,812 is never silently a sum of rows the user
// thought was a value.

// The validated categorical palette, same as TimeChart: fixed order, never cycled,
// and paired with a legend so series identity never rests on colour alone.
const LIGHT = ['#2a78d6', '#1baf7a', '#eda100', '#008300', '#4a3aa7', '#e34948', '#e87ba4', '#eb6834']
const DARK = ['#3987e5', '#199e70', '#c98500', '#008300', '#9085e9', '#e66767', '#d55181', '#d95926']

function isDarkSurface() {
  if (typeof document === 'undefined') return false
  const bg = getComputedStyle(document.documentElement).getPropertyValue('--bg').trim().replace('#', '')
  if (bg.length < 6) return false
  const r = parseInt(bg.slice(0, 2), 16)
  const g = parseInt(bg.slice(2, 4), 16)
  const b = parseInt(bg.slice(4, 6), 16)
  return 0.299 * r + 0.587 * g + 0.114 * b < 128
}

export function DbxChart({ columns = [], rows = [], name = 'chart' }) {
  const [spec, setSpec] = useState(null)
  const colKey = useMemo(() => columns.map((c) => `${c.name}:${c.semanticType}`).join('|'), [columns])

  // The suggestion is applied as the builder's starting state rather than rendered
  // as a separate "suggested chart". It is a proposal in controls the user can see
  // and change, which is the honest way to suggest something.
  useEffect(() => { setSpec(suggestChart(columns)) }, [colKey]) // eslint-disable-line react-hooks/exhaustive-deps

  const s = spec || suggestChart(columns)
  const data = useMemo(() => buildChartData(columns, rows, s), [columns, rows, s])
  const patch = (p) => setSpec({ ...s, ...p })

  if (!columns.length) {
    return <div className="flex h-full items-center justify-center p-8 text-sm text-muted">Run a query to chart its result.</div>
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <Builder columns={columns} spec={s} patch={patch} rows={rows.length} />
      {data.warning && (
        <div className="flex items-center gap-2 border-b bg-warning/5 px-3 py-1.5 text-xs text-warning">
          <Icon.StatusWarn size={13} />
          <span>{data.warning}</span>
        </div>
      )}
      <div className="min-h-0 flex-1 p-3">
        {s.type === 'table' || s.x == null ? (
          <div className="flex h-full items-center justify-center text-sm text-muted">
            Pick a chart type and a category column to draw one.
          </div>
        ) : (
          <Plot spec={s} data={data} columns={columns} name={name} />
        )}
      </div>
    </div>
  )
}

function Builder({ columns, spec, patch, rows }) {
  const sel = 'ui-input rounded-md border bg-bg px-1.5 py-1 text-xs outline-none focus:border-primary'
  const colOpts = (pred) => columns.map((c, i) => ({ c, i })).filter(({ c }) => (pred ? pred(c) : true))
  const needsY = !['histogram'].includes(spec.type)
  const isCat = ['bar', 'hbar', 'pie', 'donut', 'line', 'area'].includes(spec.type)
  return (
    <div className="flex flex-wrap items-end gap-2 border-b bg-surface px-3 py-2">
      <Ctl label="Chart">
        <select className={sel} value={spec.type} onChange={(e) => patch({ type: e.target.value })}>
          {CHART_TYPES.map((t) => <option key={t.id} value={t.id}>{t.label}</option>)}
        </select>
      </Ctl>
      <Ctl label={spec.type === 'scatter' ? 'X' : spec.type === 'histogram' ? 'Value' : 'Category'}>
        <select className={sel} value={spec.x ?? ''} onChange={(e) => patch({ x: e.target.value === '' ? null : Number(e.target.value) })}>
          <option value="">—</option>
          {colOpts(spec.type === 'histogram' ? isNumericCol : null).map(({ c, i }) => (
            <option key={i} value={i}>{c.name}</option>
          ))}
        </select>
      </Ctl>
      {needsY && (
        <Ctl label={spec.type === 'scatter' ? 'Y' : 'Value'}>
          <select className={sel} value={spec.y ?? ''} onChange={(e) => patch({ y: e.target.value === '' ? null : Number(e.target.value) })}>
            <option value="">count of rows</option>
            {colOpts(isNumericCol).map(({ c, i }) => <option key={i} value={i}>{c.name}</option>)}
          </select>
        </Ctl>
      )}
      {isCat && (
        <Ctl label="Series">
          <select className={sel} value={spec.series ?? ''} onChange={(e) => patch({ series: e.target.value === '' ? null : Number(e.target.value) })}>
            <option value="">none</option>
            {colOpts().map(({ c, i }) => <option key={i} value={i}>{c.name}</option>)}
          </select>
        </Ctl>
      )}
      {needsY && (
        <Ctl label="Aggregate" hint="applied here, not in the query">
          <select className={sel} value={spec.agg} onChange={(e) => patch({ agg: e.target.value })}>
            {AGGREGATIONS.map((a) => <option key={a.id} value={a.id}>{a.label}</option>)}
          </select>
        </Ctl>
      )}
      {spec.type === 'histogram' ? (
        <Ctl label="Bins">
          <input type="number" min="2" max="100" className={`${sel} w-16`} value={spec.bins ?? 20}
            onChange={(e) => patch({ bins: Number(e.target.value) })} />
        </Ctl>
      ) : (
        <>
          <Ctl label="Sort">
            <select className={sel} value={spec.sort} onChange={(e) => patch({ sort: e.target.value })}>
              <option value="none">result order</option>
              <option value="x">by category</option>
              <option value="value-desc">by value, high first</option>
              <option value="value-asc">by value, low first</option>
            </select>
          </Ctl>
          <Ctl label="Top N" hint="0 = all">
            <input type="number" min="0" max="500" className={`${sel} w-16`} value={spec.topN ?? 0}
              onChange={(e) => patch({ topN: Number(e.target.value) })} />
          </Ctl>
        </>
      )}
      <div className="flex-1" />
      <Badge tone="muted">{fmtNumber(rows)} rows in</Badge>
    </div>
  )
}

function Ctl({ label, hint, children }) {
  return (
    <label className="flex flex-col gap-0.5">
      <span className="text-[10px] font-medium uppercase tracking-wide text-muted">{label}</span>
      {children}
      {hint && <span className="text-[10px] text-muted">{hint}</span>}
    </label>
  )
}

// useSize measures the chart's box so the SVG can be laid out in real pixels. It is a
// layout effect so the first paint is already the right size rather than a flash at
// the default.
function useSize(ref, fallback = { w: 720, h: 360 }) {
  const [size, setSize] = useState(fallback)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el || typeof ResizeObserver === 'undefined') return undefined
    const ro = new ResizeObserver(() => setSize({ w: el.clientWidth || fallback.w, h: el.clientHeight || fallback.h }))
    ro.observe(el)
    setSize({ w: el.clientWidth || fallback.w, h: el.clientHeight || fallback.h })
    return () => ro.disconnect()
  }, [ref]) // eslint-disable-line react-hooks/exhaustive-deps
  return size
}

function Plot({ spec, data, columns, name }) {
  const box = useRef(null)
  const svgRef = useRef(null)
  const { w, h } = useSize(box)
  const [hover, setHover] = useState(null)
  const palette = isDarkSurface() ? DARK : LIGHT
  const series = data.series.length ? data.series : ['']
  const colorOf = (s) => palette[Math.max(0, series.indexOf(s)) % palette.length]

  const exportSVG = () => {
    if (!svgRef.current) return
    const xml = new XMLSerializer().serializeToString(svgRef.current)
    download(`${name}.svg`, `<?xml version="1.0" encoding="UTF-8"?>\n${xml}`, 'image/svg+xml')
  }

  if (!data.points.length) {
    return <div className="flex h-full items-center justify-center text-sm text-muted">Nothing to plot from this result.</div>
  }

  const pie = spec.type === 'pie' || spec.type === 'donut'
  return (
    <div className="flex h-full min-h-0 flex-col gap-2">
      <div className="flex items-center gap-2">
        {data.series.length > 0 && (
          <div className="flex flex-wrap items-center gap-2">
            {series.map((s) => (
              <span key={s} className="flex items-center gap-1 text-[11px] text-muted">
                <span className="inline-block h-2 w-2 rounded-sm" style={{ background: colorOf(s) }} />
                {s || '—'}
              </span>
            ))}
          </div>
        )}
        {data.clientSide && (
          <span className="text-[11px] text-muted" title="Grouping, Top N and sorting are applied in the browser over the rows the query returned.">
            client-side view of the result
          </span>
        )}
        <div className="flex-1" />
        <button onClick={exportSVG} className="rounded-md border px-2 py-1 text-xs hover:bg-surface2" title="Download this chart as SVG">
          <Icon.External size={12} className="inline" /> SVG
        </button>
      </div>
      <div ref={box} className="relative min-h-0 flex-1">
        {w > 0 && (
          pie
            ? <PieChart svgRef={svgRef} spec={spec} data={data} w={w} h={h} colorOf={colorOf} onHover={setHover} />
            : <XYChart svgRef={svgRef} spec={spec} data={data} w={w} h={h} colorOf={colorOf} series={series} onHover={setHover} />
        )}
        {hover && (
          <div
            className="pointer-events-none absolute z-10 rounded-md border bg-surface px-2 py-1 text-[11px] shadow-lg"
            style={{ left: Math.min(hover.px + 12, w - 170), top: Math.max(0, hover.py - 34) }}
          >
            <div className="font-medium">{hover.x}</div>
            <div className="text-muted">
              {hover.series ? `${hover.series}: ` : ''}
              <span className="tabular-nums text-fg">{fmtNumber(hover.y)}</span>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}

// niceTicks picks round axis values. A y-axis labelled 0 / 2,500 / 5,000 is readable;
// one labelled 0 / 2,381 / 4,762 is the same information rendered unreadably.
function niceTicks(min, max, count = 5) {
  if (!Number.isFinite(min) || !Number.isFinite(max)) return [0]
  if (min === max) return [min]
  const span = max - min
  const raw = span / count
  const mag = 10 ** Math.floor(Math.log10(raw))
  const norm = raw / mag
  const step = (norm >= 5 ? 10 : norm >= 2 ? 5 : norm >= 1 ? 2 : 1) * mag
  const out = []
  for (let v = Math.ceil(min / step) * step; v <= max + step / 2; v += step) out.push(Number(v.toFixed(10)))
  return out.length ? out : [min, max]
}

// XYChart and PieChart take `svgRef` as an ordinary prop rather than through
// forwardRef — the ref exists so the Export button can serialise the SVG, which is a
// use of the element and not a handle on the component.
function XYChart({ spec, data, w, h, colorOf, series, onHover, svgRef }) {
  const horizontal = spec.type === 'hbar'
  const pad = horizontal
    ? { t: 12, r: 18, b: 28, l: Math.min(180, Math.max(70, ...data.points.map((p) => String(p.x).length * 6.2))) }
    : { t: 12, r: 18, b: 52, l: 62 }
  const iw = Math.max(10, w - pad.l - pad.r)
  const ih = Math.max(10, h - pad.t - pad.b)

  const vals = data.points.map((p) => p.y).filter((v) => v !== null && Number.isFinite(v))
  const scatter = spec.type === 'scatter'
  let vMin = vals.length ? Math.min(0, ...vals) : 0
  let vMax = vals.length ? Math.max(...vals) : 1
  if (scatter) vMin = vals.length ? Math.min(...vals) : 0
  if (vMin === vMax) { vMax = vMin + 1 }
  const ticks = niceTicks(vMin, vMax)
  const tMin = Math.min(vMin, ...ticks)
  const tMax = Math.max(vMax, ...ticks)
  const vPos = (v) => (v - tMin) / (tMax - tMin || 1)

  // Categories in the order the data arrived (which the builder's Sort controls).
  const cats = []
  for (const p of data.points) if (!cats.includes(p.x)) cats.push(p.x)
  const band = (horizontal ? ih : iw) / Math.max(1, cats.length)
  const catPos = (x) => cats.indexOf(x) * band + band / 2

  const axis = 'var(--border)'
  const grid = 'var(--grid)'
  const text = 'var(--muted)'

  const marks = []
  if (spec.type === 'bar' || spec.type === 'hbar') {
    const per = Math.max(1, series.length)
    const bw = Math.max(1, (band * 0.72) / per)
    data.points.forEach((p, i) => {
      if (p.y === null) return
      const si = Math.max(0, series.indexOf(p.series))
      const base = catPos(p.x) - (band * 0.72) / 2 + si * bw
      if (horizontal) {
        const len = vPos(p.y) * iw
        marks.push(
          <rect
            key={i} x={pad.l} y={pad.t + base} width={Math.max(0, len)} height={Math.max(1, bw - 1)}
            fill={colorOf(p.series)} rx="1"
            onMouseMove={(e) => onHover({ x: p.x, y: p.y, series: p.series, px: e.nativeEvent.offsetX, py: e.nativeEvent.offsetY })}
            onMouseLeave={() => onHover(null)}
          />,
        )
      } else {
        const len = vPos(p.y) * ih
        marks.push(
          <rect
            key={i} x={pad.l + base} y={pad.t + ih - len} width={Math.max(1, bw - 1)} height={Math.max(0, len)}
            fill={colorOf(p.series)} rx="1"
            onMouseMove={(e) => onHover({ x: p.x, y: p.y, series: p.series, px: e.nativeEvent.offsetX, py: e.nativeEvent.offsetY })}
            onMouseLeave={() => onHover(null)}
          />,
        )
      }
    })
  } else if (spec.type === 'line' || spec.type === 'area') {
    series.forEach((s) => {
      const pts = data.points.filter((p) => p.series === s && p.y !== null)
      if (!pts.length) return
      const coords = pts.map((p) => [pad.l + catPos(p.x), pad.t + ih - vPos(p.y) * ih])
      const d = coords.map(([x, y], i) => `${i ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)}`).join(' ')
      if (spec.type === 'area') {
        marks.push(
          <path key={`a-${s}`} d={`${d} L${coords[coords.length - 1][0].toFixed(1)},${(pad.t + ih).toFixed(1)} L${coords[0][0].toFixed(1)},${(pad.t + ih).toFixed(1)} Z`}
            fill={colorOf(s)} opacity="0.18" />,
        )
      }
      marks.push(<path key={`l-${s}`} d={d} fill="none" stroke={colorOf(s)} strokeWidth="1.8" strokeLinejoin="round" />)
      coords.forEach(([x, y], i) => marks.push(
        <circle key={`p-${s}-${i}`} cx={x} cy={y} r="3.5" fill={colorOf(s)}
          onMouseMove={(e) => onHover({ x: pts[i].x, y: pts[i].y, series: s, px: e.nativeEvent.offsetX, py: e.nativeEvent.offsetY })}
          onMouseLeave={() => onHover(null)} />,
      ))
    })
  } else if (scatter) {
    // A scatter's X is a number, not a category, so it gets its own linear scale.
    const xs = data.points.map((p) => Number(p.x)).filter((v) => Number.isFinite(v))
    const xMin = xs.length ? Math.min(...xs) : 0
    const xMax = xs.length ? Math.max(...xs) : 1
    data.points.forEach((p, i) => {
      const xv = Number(p.x)
      if (!Number.isFinite(xv) || p.y === null) return
      const x = pad.l + ((xv - xMin) / (xMax - xMin || 1)) * iw
      const y = pad.t + ih - vPos(p.y) * ih
      marks.push(
        <circle key={i} cx={x} cy={y} r="3.5" fill={colorOf(p.series)} opacity="0.75"
          onMouseMove={(e) => onHover({ x: p.x, y: p.y, series: p.series, px: e.nativeEvent.offsetX, py: e.nativeEvent.offsetY })}
          onMouseLeave={() => onHover(null)} />,
      )
    })
  } else { // histogram — bars over bins
    const bw = Math.max(1, band * 0.9)
    data.points.forEach((p, i) => {
      const len = vPos(p.y) * ih
      marks.push(
        <rect key={i} x={pad.l + catPos(p.x) - bw / 2} y={pad.t + ih - len} width={bw} height={Math.max(0, len)}
          fill={colorOf('')} rx="1"
          onMouseMove={(e) => onHover({ x: p.x, y: p.y, series: '', px: e.nativeEvent.offsetX, py: e.nativeEvent.offsetY })}
          onMouseLeave={() => onHover(null)} />,
      )
    })
  }

  // Label every category only while they fit; otherwise label every nth, so the
  // axis stays readable instead of turning into a grey smear.
  const every = Math.max(1, Math.ceil(cats.length / Math.floor((horizontal ? ih : iw) / (horizontal ? 18 : 64))))

  return (
    <svg ref={svgRef} width={w} height={h} className="select-none" style={{ fontFamily: 'var(--ui-sans, sans-serif)' }}>
      <rect x="0" y="0" width={w} height={h} fill="transparent" />
      {ticks.map((t) => {
        const p = vPos(t)
        return horizontal ? (
          <g key={t}>
            <line x1={pad.l + p * iw} y1={pad.t} x2={pad.l + p * iw} y2={pad.t + ih} stroke={grid} strokeWidth="1" />
            <text x={pad.l + p * iw} y={h - 10} fontSize="10" fill={text} textAnchor="middle">{fmtNumber(t)}</text>
          </g>
        ) : (
          <g key={t}>
            <line x1={pad.l} y1={pad.t + ih - p * ih} x2={pad.l + iw} y2={pad.t + ih - p * ih} stroke={grid} strokeWidth="1" />
            <text x={pad.l - 6} y={pad.t + ih - p * ih + 3} fontSize="10" fill={text} textAnchor="end">{fmtNumber(t)}</text>
          </g>
        )
      })}
      <line x1={pad.l} y1={pad.t + ih} x2={pad.l + iw} y2={pad.t + ih} stroke={axis} strokeWidth="1" />
      <line x1={pad.l} y1={pad.t} x2={pad.l} y2={pad.t + ih} stroke={axis} strokeWidth="1" />
      {marks}
      {!scatter && cats.map((c, i) => (i % every ? null : (
        horizontal ? (
          <text key={c} x={pad.l - 6} y={pad.t + catPos(c) + 3} fontSize="10" fill={text} textAnchor="end">
            {String(c).length > 26 ? `${String(c).slice(0, 25)}…` : c}
          </text>
        ) : (
          <text key={c} x={pad.l + catPos(c)} y={pad.t + ih + 14} fontSize="10" fill={text} textAnchor="end"
            transform={`rotate(-35 ${pad.l + catPos(c)} ${pad.t + ih + 14})`}>
            {String(c).length > 18 ? `${String(c).slice(0, 17)}…` : c}
          </text>
        )
      )))}
    </svg>
  )
}

function PieChart({ spec, data, w, h, colorOf, onHover, svgRef }) {
  const total = data.points.reduce((a, p) => a + (p.y || 0), 0)
  const r = Math.max(20, Math.min(w, h) / 2 - 24)
  const cx = w / 2
  const cy = h / 2
  const inner = spec.type === 'donut' ? r * 0.55 : 0
  let angle = -Math.PI / 2
  const palette = data.points.map((p) => p.x)
  const arcs = data.points.map((p, i) => {
    const frac = total ? (p.y || 0) / total : 0
    const a0 = angle
    const a1 = angle + frac * Math.PI * 2
    angle = a1
    const large = a1 - a0 > Math.PI ? 1 : 0
    const p0 = [cx + r * Math.cos(a0), cy + r * Math.sin(a0)]
    const p1 = [cx + r * Math.cos(a1), cy + r * Math.sin(a1)]
    const i0 = [cx + inner * Math.cos(a1), cy + inner * Math.sin(a1)]
    const i1 = [cx + inner * Math.cos(a0), cy + inner * Math.sin(a0)]
    const d = inner
      ? `M${p0} A${r},${r} 0 ${large} 1 ${p1} L${i0} A${inner},${inner} 0 ${large} 0 ${i1} Z`
      : `M${cx},${cy} L${p0} A${r},${r} 0 ${large} 1 ${p1} Z`
    return (
      <path
        key={i} d={d} fill={colorOf(palette[i])} stroke="var(--surface)" strokeWidth="1"
        onMouseMove={(e) => onHover({ x: p.x, y: p.y, series: `${(frac * 100).toFixed(1)}%`, px: e.nativeEvent.offsetX, py: e.nativeEvent.offsetY })}
        onMouseLeave={() => onHover(null)}
      />
    )
  })
  return (
    <svg ref={svgRef} width={w} height={h} className="select-none">
      {arcs}
      {inner > 0 && (
        <text x={cx} y={cy + 4} fontSize="13" fill="var(--fg)" textAnchor="middle">{fmtNumber(total)}</text>
      )}
    </svg>
  )
}

export default DbxChart
export { XYChart, PieChart, niceTicks }
