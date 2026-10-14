import { useCallback, useEffect, useRef, useState } from 'react'
import { Card, Badge } from '../components/ui.jsx'
import WhatsNew, { WhatsNewLink, useWhatsNew } from '../components/WhatsNew.jsx'
import { UpdateCheckButton } from '../components/UpdateCheck.jsx'
import { dashApi, fmtBytes } from '../lib/dashApi.js'
import { usePolling } from '../lib/usePolling.jsx'
import { relTime } from '../lib/notifApi.js'
import { useRefresh } from '../lib/useRefresh.jsx'
import { openStackInDesigner } from '../desktop/Stacks.jsx'

// Dashboard — store-backed summary counters plus focus-gated live OS stats. The live sample
// polls only while this page is on screen AND the tab is visible/focused, so there is no
// CPU/disk cost when nobody is looking. The gating hook started here and now lives in
// lib/usePolling.jsx, where every polling page can use it. Network/disk are shown as
// per-node rates (bytes/s) derived by diffing consecutive samples.

export default function Dashboard() {
  // Release notes live here rather than in App: the dashboard is where somebody
  // lands after signing in, which is the moment "what changed?" is worth answering.
  const whatsNew = useWhatsNew()
  const [sum, setSum] = useState(null)
  const [stats, setStats] = useState(null)
  const [rates, setRates] = useState([])
  const [live, setLive] = useState(false)
  const prev = useRef(null) // { at, byName } for rate deltas

  const loadSummary = useCallback(() => dashApi.summary().then(setSum).catch(() => {}), [])
  const loadStats = useCallback(() => {
    return dashApi.stats().then((d) => {
      setStats(d)
      const nodes = d.nodes || []
      const now = d.sampledAtSec || Date.now() / 1000
      const p = prev.current
      if (p && now > p.at) {
        const dt = now - p.at
        const r = (cur, was) => (was != null && cur >= was ? Math.max(0, (cur - was) / dt) : 0)
        setRates(nodes.map((n) => {
          const o = p.byName[n.name]
          return {
            name: n.name,
            netIn: r(n.netRx, o?.netRx),
            netOut: r(n.netTx, o?.netTx),
            diskIn: r(n.blkRead, o?.blkRead),
            diskOut: r(n.blkWrite, o?.blkWrite),
          }
        }))
      }
      prev.current = { at: now, byName: Object.fromEntries(nodes.map((n) => [n.name, n])) }
    }).catch(() => {})
  }, [])

  useEffect(() => { loadSummary() }, [loadSummary])
  usePolling(loadSummary, 15000)
  usePolling(loadStats, 4000, { onActive: setLive })
  useRefresh(() => Promise.all([loadSummary(), loadStats()]))

  const admin = sum?.scope === 'admin'
  // Each bar knows its own scale (frac, 0–1) rather than being drawn against the busiest row:
  // relative bars made a container at 0.8% CPU look pinned. labelOf/stackOf are the canvas's
  // names for the container (app/dashboard.go), so a row can be traced — and clicked — to its stack.
  const labelOf = (name) => (stats?.nodes || []).find((n) => n.name === name)
  const bars = (rows, key, fmt, frac) => {
    const top = (rows || [])
      .map((r) => ({ name: r.name, value: r[key] || 0, display: fmt(r[key] || 0), row: labelOf(r.name) || r }))
      .sort((a, b) => b.value - a.value)
      .slice(0, 5)
    const max = Math.max(...top.map((t) => t.value), 0)
    return top.map((t) => ({ ...t, frac: frac(t, max) }))
  }
  // CPU: Docker's figure is per core, so a full bar is one full core (more is capped, and
  // the number says so). Memory: the container's own limit. Rates have no ceiling, so they
  // are ranked against each other — but against at least 1 MB/s, so a trickle stays a trickle.
  const cpuFrac = (t) => t.value / 100
  const memFrac = (t) => (t.row.memPercent || 0) / 100
  const rateFrac = (t, max) => t.value / Math.max(max, 1 << 20)
  const openStack = (id) => openStackInDesigner((page) => { location.hash = page }, id)
  const pct = (v) => `${v.toFixed(1)}%`
  const rate = (v) => `${fmtBytes(v)}/s`

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <Badge tone={admin ? 'primary' : 'muted'}>{admin ? 'System-wide' : 'Your account'}</Badge>
        <span className="flex items-center gap-1.5 text-xs text-muted">
          <span className={`h-2 w-2 rounded-full ${live ? 'animate-pulse bg-primary' : 'bg-muted'}`} />
          {live ? 'Live · monitoring active' : 'Paused (focus the tab to resume)'}
        </span>
        <span className="ml-auto flex flex-wrap items-center gap-3">
          <UpdateCheckButton />
          <WhatsNewLink data={whatsNew.data} onClick={whatsNew.showAll} />
        </span>
      </div>

      <WhatsNew
        data={whatsNew.data}
        open={whatsNew.open}
        onClose={whatsNew.close}
        acknowledging={whatsNew.acknowledging}
      />

      {/* Counters */}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
        <Stat label="Stacks deployed" value={sum ? `${sum.stacks.deployed}/${sum.stacks.total}` : '—'}
          sub={sum ? [sum.stacks.draft ? `${sum.stacks.draft} draft` : '', sum.stacks.expired ? `${sum.stacks.expired} expired` : ''].filter(Boolean).join(' · ') || 'all deployed' : ''} />
        <Stat label="Nodes running" value={sum ? `${sum.nodes.running}/${sum.nodes.total}` : '—'}
          sub={sum?.nodes.error ? `${sum.nodes.error} error` : (sum?.nodes.other ? `${sum.nodes.other} provisioning` : 'all healthy')}
          tone={sum?.nodes.error ? 'danger' : 'primary'} />
        <Stat label="Containers" value={stats?.containers ?? '—'} sub="live" />
        <Stat label="CPU" value={stats ? `${stats.cpuPercent.toFixed(0)}%` : '—'} sub="aggregate" />
        <Stat label="Memory" value={stats ? fmtBytes(stats.memUsed) : '—'} sub={stats ? `of ${fmtBytes(stats.memLimit)}` : ''} />
        {admin
          ? <Stat label="Users" value={sum?.users?.total ?? '—'} sub={sum?.users?.pending ? `${sum.users.pending} pending` : 'none pending'} tone={sum?.users?.pending ? 'warning' : 'muted'} />
          : <Stat label="Data-gen jobs" value={sum ? sum.dataGen.active + sum.dataGen.done + sum.dataGen.error : '—'} sub={`${sum?.dataGen.active ?? 0} active`} />}
      </div>

      <div className="grid grid-cols-1 gap-3 lg:grid-cols-3">
        <Card title="Top containers" subtitle="By CPU — a full bar is one core">
          <TopBars items={bars(stats?.nodes, 'cpuPercent', pct, cpuFrac)} empty="No running containers" accent="var(--color-primary)" onOpen={openStack} />
        </Card>
        <Card title="Top containers" subtitle="By memory — a full bar is the container's limit">
          <TopBars items={bars(stats?.nodes, 'memUsed', fmtBytes, memFrac)} empty="No running containers" accent="var(--color-accent)" onOpen={openStack} />
        </Card>
        <Card title="By engine" subtitle="Running DB nodes">
          <Breakdown data={sum?.byEngine} labels={{ postgres: 'PostgreSQL', mysql: 'MySQL/PXC', mongodb: 'MongoDB', valkey: 'Valkey' }} />
        </Card>
      </div>

      {/* Per-node network / disk rates */}
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <Card title="Top network in" subtitle="Per node (live)"><TopBars items={bars(rates, 'netIn', rate, rateFrac)} accent="var(--color-success)" onOpen={openStack} /></Card>
        <Card title="Top network out" subtitle="Per node (live)"><TopBars items={bars(rates, 'netOut', rate, rateFrac)} accent="var(--color-accent)" onOpen={openStack} /></Card>
        <Card title="Top disk in" subtitle="Per node (live)"><TopBars items={bars(rates, 'diskIn', rate, rateFrac)} accent="var(--color-warning)" onOpen={openStack} /></Card>
        <Card title="Top disk out" subtitle="Per node (live)"><TopBars items={bars(rates, 'diskOut', rate, rateFrac)} accent="var(--color-primary)" onOpen={openStack} /></Card>
      </div>

      <div className="grid grid-cols-1 gap-3 lg:grid-cols-3">
        <Card title="Node types">
          <Breakdown data={sum?.byType} />
        </Card>
        <Card title="Recent activity" subtitle="Notifications" className="lg:col-span-2">
          <Activity items={sum?.activity || []} />
        </Card>
      </div>
    </div>
  )
}

function Stat({ label, value, sub, tone = 'muted' }) {
  const color = { primary: 'text-primary', danger: 'text-danger', warning: 'text-warning', muted: 'text-fg' }[tone]
  return (
    <Card>
      <div className="text-xs text-muted">{label}</div>
      <div className={`mt-0.5 text-xl font-semibold ${color}`}>{value}</div>
      {sub && <div className="mt-0.5 text-xs text-muted">{sub}</div>}
    </Card>
  )
}

// shortName trims the installation's prefix and the random deploy token for readability
// (dbcanvas-115-pxc-mr263gcu-13 → 115-pxc-13, dbcanvas-jane-115-pg-1 → 115-pg-1): everything up
// to the stack id, which is the first all-digit part (instance names cannot have one).
const shortName = (n) => n.replace(/^(?!k3d-)[a-z][a-z0-9-]*?-(?=\d+-)/, '').replace(/-[a-z0-9]{6,}-(\d+)$/i, '-$1')

// TopBars renders a ranked horizontal bar chart (top-N). HTML/CSS bars keep the app's
// font crisp and animate smoothly — no distorted SVG text. Each item carries its own frac
// (see bars() in Dashboard); a row with a known stack opens it in Database Stacks.
function TopBars({ items, empty = 'Waiting for samples…', accent = 'var(--color-primary)', onOpen }) {
  if (!items || !items.length) return <div className="py-6 text-center text-sm text-muted">{empty}</div>
  return (
    <div className="space-y-2">
      {items.map((it) => {
        const r = it.row || {}
        const name = r.label ? `${r.stack} / ${r.label}` : shortName(it.name)
        const click = onOpen && r.stackId ? () => onOpen(r.stackId) : undefined
        return (
          <div key={it.name} className={click ? 'cursor-pointer rounded hover:bg-surface2/60' : ''} onClick={click}
            title={click ? `${it.name} — open ${r.stack || 'its stack'} in Database Stacks` : it.name}>
            <div className="mb-0.5 flex items-baseline justify-between gap-3">
              <span className="truncate text-xs text-fg/90">{name}</span>
              <span className="shrink-0 text-[11px] tabular-nums text-muted">{it.display}</span>
            </div>
            <div className="h-1.5 overflow-hidden rounded-full bg-surface2">
              <div
                className="h-full rounded-full transition-[width] duration-500 ease-out"
                style={{ width: `${it.value <= 0 ? 0 : Math.min(100, Math.max(2, it.frac * 100))}%`, background: accent }}
              />
            </div>
          </div>
        )
      })}
    </div>
  )
}

function Breakdown({ data, labels = {} }) {
  const entries = Object.entries(data || {}).sort((a, b) => b[1] - a[1])
  if (!entries.length) return <div className="py-6 text-center text-sm text-muted">Nothing running</div>
  const max = Math.max(...entries.map(([, v]) => v))
  return (
    <div className="space-y-2">
      {entries.map(([k, v]) => (
        <div key={k}>
          <div className="mb-0.5 flex justify-between text-xs"><span>{labels[k] || k}</span><span className="text-muted">{v}</span></div>
          <div className="h-2 rounded-full bg-surface2"><div className="h-full rounded-full bg-primary" style={{ width: `${(v / max) * 100}%` }} /></div>
        </div>
      ))}
    </div>
  )
}

// foldActivity folds a burst of the same event on one stack into one row — three members of
// one cluster crossing the CPU threshold together is one thing that happened, not three. Items
// arrive newest first; a burst is the same title and stack within two minutes of the first.
function foldActivity(items) {
  const out = []
  for (const n of items) {
    const g = out[out.length - 1]
    const t = Date.parse(n.createdAt)
    if (g && n.stackId && g.stackId === n.stackId && g.title === n.title && Math.abs(Date.parse(g.createdAt) - t) < 120000) {
      g.count++
      g.bodies.push(n.body)
      continue
    }
    out.push({ ...n, count: 1, bodies: [n.body] })
  }
  return out.map((g) => (g.count > 1 ? { ...g, title: `${g.title} ×${g.count}`, body: g.bodies.map((b) => (b || '').split(':')[0]).join(', ') } : g))
}

function Activity({ items }) {
  if (!items.length) return <div className="py-6 text-center text-sm text-muted">No recent activity</div>
  const dot = { info: 'bg-muted', success: 'bg-primary', warning: 'bg-warning', error: 'bg-danger' }
  return (
    <div className="space-y-2">
      {foldActivity(items).map((n) => (
        <div key={n.id} className="flex items-start gap-2.5">
          <span className={`mt-1.5 h-2 w-2 shrink-0 rounded-full ${dot[n.severity] || 'bg-muted'}`} />
          <div className="min-w-0 flex-1">
            <div className="flex items-center justify-between gap-2">
              <span className="truncate text-sm font-medium">{n.title}</span>
              <span className="shrink-0 text-[11px] text-muted">{relTime(n.createdAt)}</span>
            </div>
            {n.body && <div className="truncate text-xs text-muted">{n.body}</div>}
          </div>
        </div>
      ))}
    </div>
  )
}
