import { RoleChip, health, NOT_RUNNING, ROLE_LABEL, tone, iops, rate, designMismatch, warnStyle, dangerStyle, perSec, connText } from './LiveOverlay.jsx'
import { fmtBytes } from '../lib/dashApi.js'

// LiveCard.jsx — the Live view as part of the canvas itself, rather than panels over it.
//
// With Live on, every card grows a strip: what the node is now (its role as the server reports it,
// or why it is not serving), anything wrong with its replication, and a bar each for CPU, memory
// and disk. Nothing floats, so nothing lands on another card; the full figures open beside a card
// when it is selected (components/LiveOverlay.jsx) or in the Properties panel's Live details tab.
//
// A replicated cluster can instead carry a table under its frame — one row per member, the shape
// of SHOW REPLICA STATUS across the whole cluster — and then its members' cards stay plain.
//
// The strips make cards taller and the tables make clusters longer, so the canvas's geometry
// follows the mode (StackDesigner's card sizes, and its Fix layout for whatever that pushes into
// something else). The heights below are what the layout reserves; the strips are built to them.

// Extra height a card's strip takes: a cluster member's (116px wide, so the bars stack) and a
// standalone node's (212px, so they sit side by side).
export const LIVE_MEMBER_EXTRA = 62
export const LIVE_NODE_EXTRA = 48

// The cluster table: its gap under the frame, header and row heights, and the narrowest it gets —
// a three-member frame is ~400px and the columns need more, so it may be wider than its frame.
export const TABLE_GAP = 8
const TABLE_HEAD = 22
const TABLE_ROW = 24
export const TABLE_MIN_W = 760
export const tableHeight = (members) => TABLE_HEAD + members * TABLE_ROW + 2
export const tableWidth = (frame) => Math.max(frame.w || 0, TABLE_MIN_W)

// problemCount is how many things are wrong with a node: one when it is down, else the problems
// its engine reports.
export function problemCount(data) {
  if (!data) return 0
  if (data.state !== 'running' || data.role?.down) return 1
  return data.role?.problems?.length || 0
}

// The ring a card gets for its health, so a sick card is found at any zoom.
export function healthRing(data) {
  if (!data) return null
  const h = health(data)
  if (h === 'down') return '0 0 0 2px color-mix(in srgb, var(--danger) 55%, transparent)'
  if (h === 'warn') return '0 0 0 2px color-mix(in srgb, var(--warning) 55%, transparent)'
  return null
}
export const healthDot = (data) => (!data ? null : { down: 'var(--danger)', warn: 'var(--warning)', ok: 'var(--success)' }[health(data)])

const SHORT = { primary: 'PRI', replica: 'REP', secondary: 'SEC', member: 'MEMBER', arbiter: 'ARB', router: 'ROUTER', standalone: 'SOLO' }

// shortRole is a role chip that fits a 116px member card.
function ShortRole({ data, node, frame }) {
  const r = data.role
  const chip = (text, style, title) => (
    <span title={title} className="shrink-0 rounded px-1 py-px text-[9px] font-bold tracking-wide" style={style}>{text}</span>
  )
  if (data.state !== 'running') {
    const [label, text] = NOT_RUNNING[data.state] || [String(data.state || '').toUpperCase(), '']
    return chip(label.split(' ')[0] === 'NODE' ? 'STOPPED' : label.split(' ')[0], dangerStyle, text)
  }
  if (!r) return null
  if (r.down) return chip('DB DOWN', dangerStyle, r.error)
  if (r.error) return chip('ROLE ?', { background: 'var(--surface2)', color: 'var(--muted)' }, r.error)
  const lead = r.role === 'primary' || (r.role === 'standalone' && r.access === 'rw')
  const mismatch = designMismatch(node, frame, r)
  return chip(`${SHORT[r.role] || r.role.toUpperCase()}${r.access ? '·' + r.access.toUpperCase() : ''}${mismatch ? ' ≠' : ''}`,
    lead ? { background: 'color-mix(in srgb, var(--primary) 16%, transparent)', color: 'var(--primary)' } : { background: 'var(--surface2)', color: 'var(--fg)' },
    `${ROLE_LABEL[r.role] || r.role}${r.access ? ' · ' + r.access.toUpperCase() : ''}${mismatch ? ` — the design says ${mismatch}` : ''}`)
}

// meta is the short line beside the role: the first problem, the reason a database is down, lag,
// or how many replicas a primary feeds — and, for a node with no database role, its network.
function metaOf(data) {
  const r = data.role
  if (data.state !== 'running') return { text: (NOT_RUNNING[data.state] || [])[1] || '', tone: 'danger' }
  if (r?.down) return { text: r.error || 'the database is not answering', tone: 'danger' }
  if (r?.problems?.length) return { text: r.problems[0], tone: 'warning', all: r.problems.join('\n') }
  if (r?.lagSec != null) return { text: `lag ${r.lagSec < 10 ? r.lagSec.toFixed(1) : Math.round(r.lagSec)}s` }
  if (r?.replicas != null) return { text: `${r.replicas} replica${r.replicas === 1 ? '' : 's'}` }
  if (r?.state) return { text: r.state }
  if (!r && (data.netIn != null || data.netOut != null)) return { text: `net ↓ ${rate(data.netIn)} ↑ ${rate(data.netOut)}` }
  return { text: '' }
}

function figures(data) {
  const fsPct = data.disk && data.disk.fsTotal ? (data.disk.fsUsed / data.disk.fsTotal) * 100 : null
  return [
    { label: 'CPU', pct: data.cpuPercent || 0, title: 'CPU' },
    { label: 'MEM', pct: data.memPercent || 0, title: data.memLimit ? `${fmtBytes(data.memUsed)} of ${fmtBytes(data.memLimit)}` : 'Memory' },
    { label: 'DISK', pct: fsPct, title: data.disk ? `${data.disk.path}: ${fmtBytes(data.disk.dataBytes)}; filesystem ${Math.round(fsPct || 0)}% of ${fmtBytes(data.disk.fsTotal)}` : 'No data directory' },
  ]
}

const pctText = (p) => (p == null ? '—' : `${Math.round(p)}%`)

function Track({ pct, h = 4 }) {
  const p = Math.max(0, Math.min(100, pct || 0))
  return (
    <div className="min-w-0 flex-1 overflow-hidden rounded-full bg-surface2" style={{ height: h }}>
      {pct != null && <div className="h-full rounded-full transition-[width] duration-500" style={{ width: `${p}%`, background: tone(p) }} />}
    </div>
  )
}

// LiveStrip is the card's strip. member: the narrow cluster-member card (bars stacked); otherwise
// the standalone card (bars side by side). data is absent for a node Live has nothing on yet.
export function LiveStrip({ data, node, frame, member }) {
  if (!data) {
    return (
      <div className="flex items-center px-2 text-[10px] text-muted" style={{ height: member ? LIVE_MEMBER_EXTRA : LIVE_NODE_EXTRA }}>
        not running
      </div>
    )
  }
  const meta = metaOf(data)
  const metaStyle = meta.tone ? { color: `var(--${meta.tone})` } : undefined
  const bars = figures(data)
  if (member) {
    return (
      <div className="flex flex-col gap-[3px] px-2 pb-1.5" style={{ height: LIVE_MEMBER_EXTRA }}>
        <div className="flex h-4 items-center gap-1">
          <ShortRole data={data} node={node} frame={frame} />
          <span className={`min-w-0 flex-1 truncate text-right text-[9px] ${meta.tone ? '' : 'text-muted'}`} style={metaStyle} title={meta.all || meta.text}>{meta.text}</span>
        </div>
        {bars.map((b) => (
          <div key={b.label} className="flex h-2.5 items-center gap-1" title={b.title}>
            <span className="w-[22px] shrink-0 font-mono text-[8px] font-semibold text-muted">{b.label}</span>
            <Track pct={b.pct} h={3} />
            <span className="w-[24px] shrink-0 text-right font-mono text-[8px] tabular-nums text-fg">{pctText(b.pct)}</span>
          </div>
        ))}
      </div>
    )
  }
  return (
    <div className="flex flex-col gap-1.5 px-3 pb-2" style={{ height: LIVE_NODE_EXTRA }}>
      <div className="flex h-[18px] items-center gap-1.5">
        {data.state !== 'running'
          ? <ShortRole data={data} node={node} frame={frame} />
          : data.role && <RoleChip role={data.role} mismatch={designMismatch(node, frame, data.role)} />}
        <span className={`min-w-0 flex-1 truncate text-[10px] ${meta.tone ? '' : 'text-muted'}`} style={metaStyle} title={meta.all || meta.text}>
          {meta.tone === 'warning' ? '⚠ ' : ''}{meta.text}
        </span>
      </div>
      <div className="grid grid-cols-3 gap-2">
        {bars.map((b) => (
          <div key={b.label} title={b.title}>
            <div className="mb-0.5 flex justify-between font-mono text-[9px] font-semibold text-muted">
              <span>{b.label}</span><span className="text-fg">{pctText(b.pct)}</span>
            </div>
            <Track pct={b.pct} />
          </div>
        ))}
      </div>
    </div>
  )
}

// ProblemBadge is the count on a card's or a cluster's header.
export function ProblemBadge({ count, title }) {
  if (!count) return null
  return <span title={title} className="shrink-0 rounded px-1 py-px text-[9px] font-bold" style={warnStyle}>⚠ {count}</span>
}

const ROLE_ORDER = { primary: 0, standalone: 1, secondary: 2, replica: 2, member: 3, arbiter: 4 }

// ClusterTable is a cluster's members as rows, placed under its frame by the canvas: role and
// source, lag, the three bars, read and write IOPS, the database's QPS / TPS and connections, and
// what is wrong.
export function ClusterTable({ frame, members, live, onSelect, selectedId }) {
  const rows = members.map((n) => ({ n, d: live[n.id] }))
    .sort((a, b) => (ROLE_ORDER[a.d?.role?.role] ?? 5) - (ROLE_ORDER[b.d?.role?.role] ?? 5) || a.n.label.localeCompare(b.n.label))
  const th = 'px-1.5 text-left font-semibold'
  return (
    <div className="overflow-hidden rounded-lg border bg-surface text-[10px] shadow-sm" style={{ width: tableWidth(frame) }}
      onPointerDown={(e) => e.stopPropagation()}>
      <table className="w-full border-collapse">
        <thead>
          <tr className="bg-surface2 text-[9px] tracking-wide text-muted" style={{ height: TABLE_HEAD }}>
            <th className={th}>MEMBER</th><th className={th}>ROLE</th><th className={th}>LAG</th>
            <th className={th}>CPU</th><th className={th}>MEM</th><th className={th}>DISK</th>
            <th className={th} title="Read / write operations per second">IOPS R / W</th>
            <th className={th} title="Statements / transactions a second, as the database counts them">QPS / TPS</th>
            <th className={th} title="Client connections open / the most allowed">CONN</th><th className={th}>STATUS</th>
          </tr>
        </thead>
        <tbody>
          {rows.map(({ n, d }) => {
            const meta = d ? metaOf(d) : { text: 'not running', tone: '' }
            const bars = d ? figures(d) : []
            const sick = d && health(d) !== 'ok'
            return (
              <tr key={n.id} onClick={() => onSelect?.(n.id)}
                className={`cursor-pointer border-t ${selectedId === n.id ? 'bg-primary/10' : sick ? 'bg-warning/5' : 'hover:bg-surface2'}`}
                style={{ height: TABLE_ROW }}>
                <td className="px-1.5 font-semibold text-fg">
                  <span className="mr-1 inline-block h-1.5 w-1.5 rounded-full align-middle" style={{ background: healthDot(d) || 'var(--muted)' }} />
                  {n.label}
                </td>
                <td className="px-1.5">{d ? <ShortRole data={d} node={n} frame={frame} /> : '—'}</td>
                <td className="px-1.5 font-mono tabular-nums">{d?.role?.lagSec != null ? `${d.role.lagSec < 10 ? d.role.lagSec.toFixed(1) : Math.round(d.role.lagSec)}s` : '—'}</td>
                {[0, 1, 2].map((i) => (
                  <td key={i} className="px-1.5" title={bars[i]?.title}>
                    {bars[i] ? (
                      <div className="flex items-center gap-1">
                        <div className="w-8"><Track pct={bars[i].pct} h={3} /></div>
                        <span className="font-mono tabular-nums">{pctText(bars[i].pct)}</span>
                      </div>
                    ) : '—'}
                  </td>
                ))}
                <td className="px-1.5 font-mono tabular-nums">{d ? `${iops(d.readIops)} / ${iops(d.writeIops)}` : '—'}</td>
                <td className="px-1.5 font-mono tabular-nums">{d?.role?.load ? `${perSec(d.qps)} / ${perSec(d.tps)}` : '—'}</td>
                <td className="px-1.5 font-mono tabular-nums" title={d?.role?.load?.active != null ? `${d.role.load.active} running something now` : undefined}>{d?.role?.load ? connText(d.role.load) : '—'}</td>
                <td className="max-w-[160px] truncate px-1.5" title={meta.all || meta.text}
                  style={meta.tone ? { color: `var(--${meta.tone})` } : undefined}>
                  {meta.tone === 'warning' ? '⚠ ' : ''}{meta.text || 'ok'}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

// footprints are the world-space rectangles the canvas's objects take with Live as it is: a free
// card at its live height, a frame with its table under it. What Fix layout keeps apart.
export function footprints(nodes, frames, sizes, tablesFor) {
  const out = []
  for (const n of nodes) {
    if (n.frameId) continue
    out.push({ kind: 'node', id: n.id, x: n.x, y: n.y, w: sizes.node[0], h: sizes.node[1] })
  }
  for (const f of frames) {
    const t = tablesFor(f)
    const h = t ? f.h + TABLE_GAP + tableHeight(t) : f.h
    const w = t ? Math.max(f.w, TABLE_MIN_W) : f.w
    out.push({ kind: 'frame', id: f.id, x: f.x, y: f.y, w, h })
  }
  return out
}

const LAYOUT_GAP = 24

const hits = (a, b, gap = 0) => a.x < b.x + b.w + gap && b.x < a.x + a.w + gap && a.y < b.y + b.h + gap && b.y < a.y + a.h + gap

// overlaps counts the pairs of footprints that touch.
export function countOverlaps(boxes) {
  let n = 0
  for (let i = 0; i < boxes.length; i++) for (let j = i + 1; j < boxes.length; j++) if (hits(boxes[i], boxes[j])) n++
  return n
}

// fixLayout returns {id: {dx, dy}} moves that pull overlapping footprints apart. Things grow
// downwards — a taller card, a frame with a table under it — so the lower of two overlapping boxes
// moves down, and a box only moves sideways when it shares a row with the one it hit and sits to
// its right (two cards placed side by side). Top to bottom, so a pushed box can push the next one;
// running it again on its result moves nothing.
export function fixLayout(boxes) {
  const now = boxes.map((b) => ({ ...b }))
  now.sort((a, b) => a.y - b.y || a.x - b.x)
  for (let pass = 0; pass < 50; pass++) {
    let moved = false
    for (let i = 0; i < now.length; i++) {
      for (let j = 0; j < now.length; j++) {
        if (i === j) continue
        const a = now[i], b = now[j]
        if (!hits(a, b, LAYOUT_GAP - 1)) continue
        // b is the one that moves: the lower one, or on a tie the one further right.
        if (b.y < a.y || (b.y === a.y && b.x <= a.x)) continue
        const sameRow = Math.abs(b.y - a.y) < Math.min(a.h, b.h) / 2 && b.x >= a.x + a.w / 2
        if (sameRow) b.x = a.x + a.w + LAYOUT_GAP
        else b.y = a.y + a.h + LAYOUT_GAP
        moved = true
      }
    }
    if (!moved) break
  }
  const out = {}
  for (const b of now) {
    const o = boxes.find((x) => x.kind === b.kind && x.id === b.id)
    const dx = Math.round(b.x - o.x), dy = Math.round(b.y - o.y)
    if (dx || dy) out[`${b.kind}:${b.id}`] = { dx, dy }
  }
  return out
}
