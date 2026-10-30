import { useMemo, useState } from 'react'
import { Icon } from './Icons.jsx'
import { toneTile, NodeCode } from './ui.jsx'
import { engineOf, engineTone } from '../lib/engines.js'
import { metaOf, figures, Track, pctText, problemCount } from './LiveCard.jsx'
import { health, ROLE_LABEL, NOT_RUNNING, perSec, connText } from './LiveOverlay.jsx'

// NodeTable.jsx — a stack's nodes as rows: the Table side of Database Stacks' Canvas / Table
// switch (StackDesigner). The same design and the same Live figures as the canvas, read down a
// column instead of across a drawing: which node is struggling, what its address is, what it
// talks to.
//
// Rows are grouped by cluster (a frame), in the order the frames sit on the canvas, then the
// nodes in no cluster. The canvas's lines are what a table would lose, so they are a column:
// "→ x" for a line drawn from this node, "← x" for one drawn to it. A line to or from a whole
// cluster is said once, on the cluster's header, rather than on every member.
//
// The figures are Live view's (lib/useLiveStates.js); with Live off the design columns stay and
// the figures are dashes. Selecting a row selects the node, so the Properties panel beside the
// table is the same one the canvas opens; right-click is the canvas's node menu.

const COLS_KEY = 'dbcanvas.nodeTable.cols'
const GROUP_KEY = 'dbcanvas.nodeTable.group'

// OPTIONAL are the columns the Columns menu can hide. Node, Type, Role and Status always show, and
// Status comes straight after Role: last, it was the column scrolled out of sight behind Properties.
const OPTIONAL = [
  ['ip', 'IP'], ['links', 'Connects to'], ['cpu', 'CPU'], ['mem', 'Memory'], ['disk', 'Disk'],
  ['load', 'QPS / TPS'], ['conn', 'Connections'], ['lag', 'Lag'],
]

const loadJSON = (k, d) => { try { const v = JSON.parse(localStorage.getItem(k)); return v ?? d } catch { return d } }
const saveJSON = (k, v) => { try { localStorage.setItem(k, JSON.stringify(v)) } catch { /* private window */ } }

const ROLE_ORDER = { primary: 0, standalone: 1, secondary: 2, replica: 2, member: 3, arbiter: 4, router: 5 }
const STATE_TONE = { running: 'success', provisioning: 'warning', pending: 'muted', stopped: 'muted', error: 'danger' }

// verdict is a row's one-word state, for the dot, the tint and the Needs attention filter.
function verdict(dep, data) {
  if (data) return health(data) // down | warn | ok
  if (!dep) return 'none'
  if (dep.state === 'error') return 'down'
  if (dep.state === 'running') return 'ok'
  return 'busy'
}
const DOT = { down: 'var(--danger)', warn: 'var(--warning)', ok: 'var(--success)', busy: 'var(--warning)', none: 'var(--muted)' }

// roleText is the role as the server reports it with Live on, else as the design says.
function roleText(node, data, designRole) {
  const r = data?.role
  if (r && !r.down && !r.error && r.role) {
    const name = ROLE_LABEL[r.role] || r.role.toUpperCase()
    return { text: name[0] + name.slice(1).toLowerCase() + (r.access ? ` · ${r.access}` : ''), lead: r.role === 'primary' || (r.role === 'standalone' && r.access === 'rw') }
  }
  return { text: designRole || '', lead: false }
}

// statusOf is the Status cell: the Live verdict when there is one, else the deployment's state.
function statusOf(dep, data) {
  if (data) {
    const m = metaOf(data)
    if (data.state !== 'running') return { text: (NOT_RUNNING[data.state] || [data.state])[0].toLowerCase(), tone: 'danger', all: m.text }
    return { text: m.tone ? m.text : 'ok', tone: m.tone || 'success', all: m.all || m.text }
  }
  if (!dep) return { text: 'not deployed', tone: 'muted' }
  return { text: dep.state, tone: STATE_TONE[dep.state] || 'muted' }
}

export default function NodeTable({
  nodes, frames, edges, depByNode, live, liveOn, selectedId, typeLabel, versionOf, memberSub, frameSub,
  onSelect, onMenu, onConsole, webLinks, onOpenWeb, onShowOnCanvas, onLiveOn,
}) {
  const [q, setQ] = useState('')
  const [only, setOnly] = useState('all') // all | db | attention
  const [hidden, setHidden] = useState(() => new Set(loadJSON(COLS_KEY, [])))
  const [grouped, setGrouped] = useState(() => loadJSON(GROUP_KEY, true))
  const [colsOpen, setColsOpen] = useState(false)
  const [folded, setFolded] = useState(() => new Set())
  const show = (k) => !hidden.has(k)
  const toggleCol = (k) => setHidden((h) => { const n = new Set(h); n.has(k) ? n.delete(k) : n.add(k); saveJSON(COLS_KEY, [...n]); return n })

  const byId = useMemo(() => {
    const m = new Map()
    for (const n of nodes) m.set(n.id, n.label || n.id)
    for (const f of frames) m.set(f.id, f.label || f.id)
    return m
  }, [nodes, frames])

  // linksOf lists the canvas's lines at one end (a node or a frame), as "→ x" / "← x".
  const linksOf = useMemo(() => {
    const out = new Map()
    const add = (id, s) => { if (!out.has(id)) out.set(id, []); out.get(id).push(s) }
    const kind = (e) => (e.type === 'async' ? ' (async)' : '')
    for (const e of edges) {
      const a = e.from?.node, b = e.to?.node
      if (!a || !b) continue
      if (e.type === 'bidir') { add(a, `↔ ${byId.get(b) || b}`); add(b, `↔ ${byId.get(a) || a}`); continue }
      add(a, `→ ${byId.get(b) || b}${kind(e)}`)
      add(b, `← ${byId.get(a) || a}${kind(e)}`)
    }
    return (id) => out.get(id) || []
  }, [edges, byId])

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase()
    const pos = (o) => [o?.y ?? 0, o?.x ?? 0]
    const keep = (n) => {
      const dep = depByNode[n.id], data = live?.[n.id]
      if (only === 'db' && !engineOf(n.type)) return false
      if (only === 'attention' && !['down', 'warn'].includes(verdict(dep, data))) return false
      if (!needle) return true
      const frame = n.frameId ? byId.get(n.frameId) : ''
      return [n.label, n.type, typeLabel(n.type), data?.ip, frame].some((s) => (s || '').toLowerCase().includes(needle))
    }
    const sortMembers = (list) => [...list].sort((a, b) =>
      (ROLE_ORDER[live?.[a.id]?.role?.role] ?? 9) - (ROLE_ORDER[live?.[b.id]?.role?.role] ?? 9) || (a.x ?? 0) - (b.x ?? 0))
    const byPos = (a, b) => { const [ay, ax] = pos(a), [by, bx] = pos(b); return ay - by || ax - bx }
    if (!grouped) return [{ nodes: [...nodes].filter(keep).sort((a, b) => (a.label || '').localeCompare(b.label || '')) }]
    const groups = [...frames].sort(byPos).map((f) => {
      const members = nodes.filter((n) => n.frameId === f.id)
      return { frame: f, members, nodes: sortMembers(members.filter(keep)) }
    }).filter((g) => g.nodes.length || (!needle && only === 'all'))
    const loose = nodes.filter((n) => !n.frameId).filter(keep).sort(byPos)
    if (loose.length) groups.push({ loose: true, nodes: loose })
    return groups
  }, [nodes, frames, depByNode, live, q, only, grouped, byId, typeLabel])

  const counts = useMemo(() => ({
    all: nodes.length,
    db: nodes.filter((n) => engineOf(n.type)).length,
    attention: nodes.filter((n) => ['down', 'warn'].includes(verdict(depByNode[n.id], live?.[n.id]))).length,
  }), [nodes, depByNode, live])

  const optCols = OPTIONAL.filter(([k]) => show(k))
  // span is the columns before Actions: Node, Type, Role, the optional ones, Status.
  const span = 4 + optCols.length

  const chip = (id, label, n, warn) => (
    <button key={id} type="button" aria-pressed={only === id} onClick={() => setOnly(id)}
      className={`ui-btn rounded-full border px-3 py-1 text-xs ${only === id
        ? 'border-primary/40 bg-primary/10 font-medium text-primary'
        : warn && n ? 'border-warning/40 bg-warning/10 text-warning' : 'bg-surface text-fg hover:bg-surface2'}`}>
      {label} {n}
    </button>
  )

  const bar = (pct) => (
    <div className="flex w-[84px] items-center gap-1.5">
      <div className="w-10"><Track pct={pct} h={4} /></div>
      <span className="font-mono text-[11px] tabular-nums">{pctText(pct)}</span>
    </div>
  )

  const row = (n, frame) => {
    const dep = depByNode[n.id], data = live?.[n.id]
    const v = verdict(dep, data)
    const st = statusOf(dep, data)
    const role = roleText(n, data, frame ? memberSub(frame, n) : '')
    const bars = data ? figures(data) : []
    const links = linksOf(n.id)
    const web = dep?.state === 'running' ? webLinks(n) : []
    const sel = selectedId === n.id
    const problems = data?.role?.problems || []
    const lag = data?.role?.lagSec
    return [
      <tr key={n.id} onClick={() => onSelect(n.id)} onDoubleClick={() => onShowOnCanvas(n.id)}
        onContextMenu={(e) => { e.preventDefault(); onSelect(n.id); onMenu(e, n.id) }}
        title="Click to select · double-click to show it on the canvas · right-click for its menu"
        className={`cursor-pointer border-t ${sel ? 'bg-primary/10' : v === 'down' ? 'bg-danger/5 hover:bg-danger/10' : v === 'warn' ? 'bg-warning/5 hover:bg-warning/10' : 'hover:bg-surface2/70'}`}>
        <td className="px-3 py-2">
          <div className={`flex items-center gap-2 ${frame ? 'pl-4' : ''}`}>
            <span className="h-2 w-2 shrink-0 rounded-full" style={{ background: DOT[v] }} />
            <span className="font-semibold text-fg">{n.label}</span>
          </div>
        </td>
        <td className="px-3 py-2">
          <div className="flex items-center gap-2">
            <span data-engine-tile={engineOf(n.type) ? '' : undefined} role="img" aria-label={typeLabel(n.type)}
              className="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg border" style={toneTile(engineTone(n.type))}>
              <NodeCode type={n.type} label={typeLabel(n.type)} size={28} />
            </span>
            <span className="flex flex-col leading-tight">
              <span className="text-fg/90">{typeLabel(n.type)}</span>
              {versionOf(n) && versionOf(n) !== typeLabel(n.type) && <span className="text-[11px] text-muted">{versionOf(n)}</span>}
            </span>
          </div>
        </td>
        <td className="px-3 py-2">
          {role.text && <span className={`rounded-full px-2 py-0.5 text-xs ${role.lead ? 'bg-primary/15 font-medium text-primary' : 'bg-surface2 text-fg/90'}`}>{role.text}</span>}
        </td>
        <td className="max-w-[240px] truncate px-3 py-2 text-xs" title={st.all || st.text} style={{ color: `var(--${st.tone === 'muted' ? 'muted' : st.tone})` }}>
          {st.tone === 'warning' ? '⚠ ' : ''}{st.text}
        </td>
        {show('ip') && <td className="px-3 py-2 font-mono text-xs tabular-nums text-muted">{data?.ip || '—'}</td>}
        {show('links') && <td className="max-w-[220px] truncate px-3 py-2 text-xs text-fg/80" title={links.join('\n')}>{links.join(' · ') || '—'}</td>}
        {show('cpu') && <td className="px-3 py-2" title={bars[0]?.title}>{bars.length ? bar(bars[0].pct) : '—'}</td>}
        {show('mem') && <td className="px-3 py-2" title={bars[1]?.title}>{bars.length ? bar(bars[1].pct) : '—'}</td>}
        {show('disk') && <td className="px-3 py-2" title={bars[2]?.title}>{bars.length && bars[2].pct != null ? bar(bars[2].pct) : '—'}</td>}
        {show('load') && <td className="px-3 py-2 text-right font-mono text-xs tabular-nums">{data?.role?.load ? `${perSec(data.qps)} / ${perSec(data.tps)}` : '—'}</td>}
        {show('conn') && <td className="px-3 py-2 text-right font-mono text-xs tabular-nums">{data?.role?.load ? connText(data.role.load) : '—'}</td>}
        {show('lag') && <td className={`px-3 py-2 text-right font-mono text-xs tabular-nums ${lag >= 1 ? 'font-semibold text-warning' : ''}`}>{lag != null ? `${lag < 10 ? lag.toFixed(1) : Math.round(lag)} s` : '—'}</td>}
        <td className="px-2 py-1">
          <div className="flex items-center justify-end gap-0.5" onClick={(e) => e.stopPropagation()} onDoubleClick={(e) => e.stopPropagation()}>
            <button type="button" aria-label={`Console on ${n.label}`} title="Root console" disabled={dep?.state !== 'running'}
              onClick={() => onConsole(n.id)}
              className="flex h-7 w-7 items-center justify-center rounded-full text-muted hover:bg-surface2 hover:text-fg disabled:opacity-30"><Icon.Terminal size={15} /></button>
            {web.length > 0 && (
              <button type="button" aria-label={`Open ${n.label}'s web UI`} title={web.length > 1 ? `${web[0].label} (more in the menu)` : `Open ${web[0].label || 'web UI'}`}
                onClick={() => onOpenWeb(web[0])}
                className="flex h-7 w-7 items-center justify-center rounded-full text-muted hover:bg-surface2 hover:text-fg"><Icon.External size={15} /></button>
            )}
            <button type="button" aria-label={`Show ${n.label} on the canvas`} title="Show on canvas" onClick={() => onShowOnCanvas(n.id)}
              className="flex h-7 w-7 items-center justify-center rounded-full text-muted hover:bg-surface2 hover:text-fg"><Icon.Frame size={15} /></button>
            <button type="button" aria-label={`More for ${n.label}`} title="Everything the node's menu on the canvas has"
              onClick={(e) => { onSelect(n.id); onMenu(e, n.id) }}
              className="flex h-7 w-7 items-center justify-center rounded-full text-muted hover:bg-surface2 hover:text-fg">⋯</button>
          </div>
        </td>
      </tr>,
      // The selected row spells out everything wrong with it, not just the first thing.
      sel && problems.length > 1 && (
        <tr key={`${n.id}:why`} className="bg-primary/5">
          <td colSpan={span + 1} className="px-3 pb-3 pt-1 text-xs">
            <ul className="ml-8 list-disc space-y-0.5 text-warning">{problems.map((p) => <li key={p}>{p}</li>)}</ul>
          </td>
        </tr>
      ),
    ]
  }

  const th = 'whitespace-nowrap px-3 py-2 text-left text-xs font-medium text-muted'
  return (
    <div className="flex h-full min-h-0 flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <label className="ui-input flex h-8 min-w-[220px] items-center gap-2 rounded-full border bg-surface px-3 text-sm text-muted">
          <Icon.Search size={14} />
          <input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder="Filter by name, type, IP or cluster"
            aria-label="Filter nodes" className="w-full bg-transparent text-fg outline-none" />
        </label>
        {chip('all', 'All', counts.all)}
        {chip('db', 'Databases', counts.db)}
        {chip('attention', 'Needs attention', counts.attention, true)}
        {!liveOn && (
          <button type="button" onClick={onLiveOn} className="text-xs text-primary hover:underline">
            Turn on Live view for roles, addresses and load
          </button>
        )}
        <div className="ml-auto flex items-center gap-2">
          <button type="button" aria-pressed={grouped} onClick={() => setGrouped((g) => { saveJSON(GROUP_KEY, !g); return !g })}
            className={`ui-btn rounded-full border px-3 py-1 text-xs ${grouped ? 'bg-surface2 text-fg' : 'bg-surface text-muted hover:bg-surface2'}`}>
            {grouped ? 'Grouped by cluster' : 'Not grouped'}
          </button>
          <div className="relative">
            <button type="button" aria-haspopup="menu" aria-expanded={colsOpen} onClick={() => setColsOpen((o) => !o)}
              className="ui-btn flex items-center gap-1 rounded-full border bg-surface px-3 py-1 text-xs text-fg hover:bg-surface2">
              Columns <Icon.Chevron size={12} />
            </button>
            {colsOpen && (
              <>
                <div className="fixed inset-0 z-40" onClick={() => setColsOpen(false)} />
                <div role="menu" className="absolute right-0 top-full z-50 mt-1 min-w-[180px] rounded-lg border bg-surface py-1 text-sm shadow-xl">
                  {OPTIONAL.map(([k, label]) => (
                    <label key={k} className="flex cursor-pointer items-center gap-2 px-3 py-1.5 hover:bg-surface2">
                      <input type="checkbox" checked={show(k)} onChange={() => toggleCol(k)} /> {label}
                    </label>
                  ))}
                </div>
              </>
            )}
          </div>
        </div>
      </div>

      <div className="ui-card min-h-0 flex-1 overflow-auto rounded-2xl border bg-surface">
        {nodes.length === 0 ? (
          <div className="py-16 text-center text-sm text-muted">Nothing on this stack yet. Switch to Canvas to add nodes.</div>
        ) : (
          <table className="w-full border-collapse whitespace-nowrap text-sm">
            <thead className="sticky top-0 z-10 bg-surface">
              <tr className="border-b">
                <th className={th}>Node</th>
                <th className={th}>Type</th>
                <th className={th}>Role</th>
                <th className={th}>Status</th>
                {optCols.map(([k, label]) => (
                  <th key={k} className={`${th} ${['load', 'conn', 'lag'].includes(k) ? 'text-right' : ''}`}>{label}</th>
                ))}
                <th className={th}><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {rows.map((g) => {
                const key = g.frame?.id || (g.loose ? '_loose' : '_all')
                const open = !folded.has(key)
                const header = (g.frame || g.loose) && (() => {
                  const live_ = g.frame ? g.members.filter((n) => live?.[n.id]) : []
                  const issues = live_.reduce((s, n) => s + problemCount(live[n.id]), 0)
                  const links = g.frame ? linksOf(g.frame.id) : []
                  return (
                    <tr key={`${key}:h`} className="border-t bg-surface2/60">
                      <td colSpan={span + 1} className="px-3 py-2">
                        <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                          <button type="button" onClick={() => setFolded((s) => { const n = new Set(s); n.has(key) ? n.delete(key) : n.add(key); return n })}
                            aria-expanded={open} className="flex flex-wrap items-center gap-x-3 gap-y-1 text-left">
                            <Icon.Chevron size={12} className={`text-muted transition ${open ? '' : '-rotate-90'}`} />
                            <span className="text-sm font-semibold">{g.frame ? g.frame.label : 'Not in a cluster'}</span>
                            <span className="text-xs text-muted">{g.frame ? frameSub(g.frame, g.members) : `${g.nodes.length} node${g.nodes.length === 1 ? '' : 's'}`}</span>
                          </button>
                          {links.length > 0 && <span className="text-xs text-fg/70">{links.join(' · ')}</span>}
                          {issues > 0 && <span className="rounded-full bg-warning/15 px-2 py-0.5 text-xs font-medium text-warning">{issues} problem{issues === 1 ? '' : 's'}</span>}
                          {g.frame && (
                            <button type="button" onClick={() => onSelect(g.frame.id, 'frame')} className="ml-auto text-xs text-primary hover:underline">
                              Cluster settings
                            </button>
                          )}
                        </div>
                      </td>
                    </tr>
                  )
                })()
                return [header, ...(open ? g.nodes.flatMap((n) => row(n, g.frame || (n.frameId ? frames.find((f) => f.id === n.frameId) : null))) : [])]
              })}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}
