import { useCallback, useEffect, useRef, useState } from 'react'
import { Card, Badge } from '../components/ui.jsx'
import { dashApi, fmtBytes } from '../lib/dashApi.js'
import { relTime } from '../lib/notifApi.js'

// Dashboard — store-backed summary counters plus focus-gated live OS stats. The live sample
// polls only while this page is mounted AND the tab is visible/focused, so there is no
// CPU/disk cost when nobody is looking.

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
  const [cpuHist, setCpuHist] = useState([])
  const [live, setLive] = useState(false)

  const loadSummary = useCallback(() => { dashApi.summary().then(setSum).catch(() => {}) }, [])
  const loadStats = useCallback(() => {
    dashApi.stats().then((d) => {
      setStats(d)
      setCpuHist((prev) => [...prev, d.cpuPercent || 0].slice(-48))
    }).catch(() => {})
  }, [])

  useEffect(() => { loadSummary() }, [loadSummary])
  useFocusGatedInterval(loadSummary, 15000)
  useFocusGatedInterval(loadStats, 4000, setLive)

  const admin = sum?.scope === 'admin'

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

      {/* CPU sparkline */}
      <Card title="Live CPU" subtitle="Aggregate across containers (focus-gated)">
        <Sparkline data={cpuHist} />
      </Card>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Card title="Top containers" subtitle="By CPU" className="lg:col-span-2">
          <TopContainers rows={stats?.top || []} />
        </Card>
        <Card title="By engine" subtitle="Running DB nodes">
          <Breakdown data={sum?.byEngine} labels={{ postgres: 'PostgreSQL', mysql: 'MySQL/PXC' }} />
        </Card>
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

function Sparkline({ data }) {
  if (!data || data.length < 2) return <div className="flex h-28 items-center justify-center text-sm text-muted">Waiting for samples…</div>
  const w = 600, h = 110, max = Math.max(10, ...data)
  const pts = data.map((v, i) => `${(i / (data.length - 1)) * w},${h - (v / max) * (h - 8) - 4}`).join(' ')
  return (
    <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className="h-28 w-full">
      <polyline points={pts} fill="none" stroke="var(--color-primary)" strokeWidth="2" vectorEffect="non-scaling-stroke" />
      <text x="2" y="12" className="fill-muted text-[10px]">{max.toFixed(0)}%</text>
    </svg>
  )
}

function TopContainers({ rows }) {
  if (!rows.length) return <div className="py-6 text-center text-sm text-muted">No running containers</div>
  return (
    <table className="w-full text-sm">
      <thead className="text-xs text-muted">
        <tr><th className="px-2 py-1 text-left">Container</th><th className="px-2 py-1 text-right">CPU</th><th className="px-2 py-1 text-right">Memory</th></tr>
      </thead>
      <tbody>
        {rows.map((c) => (
          <tr key={c.name} className="border-t">
            <td className="px-2 py-1 font-mono text-xs">{c.name.replace(/^dbcanvas-/, '')}</td>
            <td className="px-2 py-1 text-right">{(c.cpuPercent || 0).toFixed(1)}%</td>
            <td className="px-2 py-1 text-right text-muted">{fmtBytes(c.memUsed)}</td>
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
