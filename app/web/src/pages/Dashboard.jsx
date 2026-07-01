import { useCallback, useEffect, useRef, useState } from 'react'
import { Card, Badge } from '../components/ui.jsx'
import { dashApi, fmtBytes } from '../lib/dashApi.js'
import { relTime } from '../lib/notifApi.js'

// Dashboard — store-backed summary counters plus focus-gated live OS stats. The live sample
// polls only while this page is mounted AND the tab is visible/focused, so there is no
// CPU/disk cost when nobody is looking. Network/disk are shown as per-node rates (bytes/s)
// derived by diffing consecutive samples.

// useFocusGatedInterval runs cb every ms, but only while the tab is visible and focused.
// It stops on blur/hide and on unmount (leaving the dashboard).
function useFocusGatedInterval(cb, ms, onActive) {
  const ref = useRef(cb)
  ref.current = cb
  useEffect(() => {
    let timer = null
    const active = () => document.visibilityState === 'visible' && document.hasFocus()
    const tick = () => {
      if (active()) ref.current()
    }
    const start = () => {
      if (!timer) {
        tick()
        timer = setInterval(tick, ms)
      }
    }
    const stop = () => {
      if (timer) {
        clearInterval(timer)
        timer = null
      }
    }
    const sync = () => {
      const on = active()
      if (onActive) onActive(on)
      on ? start() : stop()
    }
    sync()
    document.addEventListener('visibilitychange', sync)
    window.addEventListener('focus', sync)
    window.addEventListener('blur', sync)
    return () => {
      stop()
      if (onActive) onActive(false)
      document.removeEventListener('visibilitychange', sync)
      window.removeEventListener('focus', sync)
      window.removeEventListener('blur', sync)
    }
  }, [ms])
}

export default function Dashboard() {
  const [sum, setSum] = useState(null)
  const [stats, setStats] = useState(null)
  const [rates, setRates] = useState([])
  const [live, setLive] = useState(false)
  const prev = useRef(null) // { at, byName } for rate deltas

  const loadSummary = useCallback(() => { dashApi.summary().then(setSum).catch(() => {}) }, [])
  const loadStats = useCallback(() => {
    dashApi.stats().then((d) => {
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
  useFocusGatedInterval(loadSummary, 15000)
  useFocusGatedInterval(loadStats, 4000, setLive)

  const admin = sum?.scope === 'admin'
  const cpuTop = (stats?.nodes || []).slice().sort((a, b) => b.cpuPercent - a.cpuPercent).slice(0, 8)

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2">
        <Badge tone={admin ? 'primary' : 'muted'}>{admin ? 'System-wide' : 'Your account'}</Badge>
        <span className="flex items-center gap-1.5 text-xs text-muted">
          <span className={`h-2 w-2 rounded-full ${live ? 'animate-pulse bg-primary' : 'bg-muted'}`} />
          {live ? 'Live · monitoring active' : 'Paused (focus the tab to resume)'}
        </span>
      </div>

      {/* Counters */}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
        <Stat label="Stacks" value={sum?.stacks.total ?? '—'} sub={`${sum?.stacks.deployed ?? 0} deployed`} />
        <Stat label="Nodes running" value={sum?.nodes.running ?? '—'}
          sub={sum?.nodes.error ? `${sum.nodes.error} error` : 'all healthy'} tone={sum?.nodes.error ? 'danger' : 'primary'} />
        <Stat label="Containers" value={stats?.containers ?? '—'} sub="live" />
        <Stat label="CPU" value={stats ? `${stats.cpuPercent.toFixed(0)}%` : '—'} sub="aggregate" />
        <Stat label="Memory" value={stats ? fmtBytes(stats.memUsed) : '—'} sub={stats ? `of ${fmtBytes(stats.memLimit)}` : ''} />
        {admin
          ? <Stat label="Users" value={sum?.users?.total ?? '—'} sub={sum?.users?.pending ? `${sum.users.pending} pending` : 'none pending'} tone={sum?.users?.pending ? 'warning' : 'muted'} />
          : <Stat label="Data-gen jobs" value={sum ? sum.dataGen.active + sum.dataGen.done + sum.dataGen.error : '—'} sub={`${sum?.dataGen.active ?? 0} active`} />}
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Card title="Top containers" subtitle="By CPU" className="lg:col-span-2">
          <NodeTable rows={cpuTop} render={(c) => `${(c.cpuPercent || 0).toFixed(1)}%`} valueHead="CPU" />
        </Card>
        <Card title="By engine" subtitle="Running DB nodes">
          <Breakdown data={sum?.byEngine} labels={{ postgres: 'PostgreSQL', mysql: 'MySQL/PXC' }} />
        </Card>
      </div>

      {/* Per-node network / disk rates */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Card title="Top network in" subtitle="Per node (live)"><RateTable rows={rates} metric="netIn" /></Card>
        <Card title="Top network out" subtitle="Per node (live)"><RateTable rows={rates} metric="netOut" /></Card>
        <Card title="Top disk in" subtitle="Per node (live)"><RateTable rows={rates} metric="diskIn" /></Card>
        <Card title="Top disk out" subtitle="Per node (live)"><RateTable rows={rates} metric="diskOut" /></Card>
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
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
      <div className={`mt-1 text-2xl font-semibold ${color}`}>{value}</div>
      {sub && <div className="mt-0.5 text-xs text-muted">{sub}</div>}
    </Card>
  )
}

const shortName = (n) => n.replace(/^dbcanvas-/, '')

// NodeTable renders a name + one metric column (used for the CPU table).
function NodeTable({ rows, render, valueHead }) {
  if (!rows.length) return <div className="py-6 text-center text-sm text-muted">No running containers</div>
  return (
    <table className="w-full text-sm">
      <thead className="text-xs text-muted">
        <tr><th className="px-2 py-1 text-left">Node</th><th className="px-2 py-1 text-right">{valueHead}</th></tr>
      </thead>
      <tbody>
        {rows.map((c) => (
          <tr key={c.name} className="border-t">
            <td className="px-2 py-1 font-mono text-xs">{shortName(c.name)}</td>
            <td className="px-2 py-1 text-right tabular-nums">{render(c)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// RateTable ranks nodes by a per-second byte rate (top 8).
function RateTable({ rows, metric }) {
  const sorted = (rows || []).slice().sort((a, b) => b[metric] - a[metric]).slice(0, 8)
  if (!sorted.length) return <div className="py-6 text-center text-sm text-muted">Waiting for samples…</div>
  return (
    <table className="w-full text-sm">
      <thead className="text-xs text-muted">
        <tr><th className="px-2 py-1 text-left">Node</th><th className="px-2 py-1 text-right">Rate</th></tr>
      </thead>
      <tbody>
        {sorted.map((c) => (
          <tr key={c.name} className="border-t">
            <td className="px-2 py-1 font-mono text-xs">{shortName(c.name)}</td>
            <td className="px-2 py-1 text-right tabular-nums text-muted">{fmtBytes(c[metric])}/s</td>
          </tr>
        ))}
      </tbody>
    </table>
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

function Activity({ items }) {
  if (!items.length) return <div className="py-6 text-center text-sm text-muted">No recent activity</div>
  const dot = { info: 'bg-muted', success: 'bg-primary', warning: 'bg-warning', error: 'bg-danger' }
  return (
    <div className="space-y-2">
      {items.map((n) => (
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
