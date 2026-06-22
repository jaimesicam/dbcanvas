import { useEffect, useRef, useState } from 'react'
import { Card, Badge } from '../components/ui.jsx'

const STATS = [
  { label: 'Active sessions', value: '2,481', delta: '+12.4%', tone: 'success' },
  { label: 'Requests / min', value: '38.2k', delta: '+3.1%', tone: 'success' },
  { label: 'Error rate', value: '0.42%', delta: '-0.08%', tone: 'success' },
  { label: 'Avg latency', value: '128 ms', delta: '+6 ms', tone: 'warning' },
]

const SERVICES = [
  { name: 'API gateway', pct: 99, tone: 'success' },
  { name: 'Auth service', pct: 97, tone: 'success' },
  { name: 'Database', pct: 88, tone: 'warning' },
  { name: 'Worker queue', pct: 72, tone: 'warning' },
  { name: 'Edge cache', pct: 95, tone: 'success' },
]

const FEED = [
  { who: 'AM', text: 'Ada deployed v2.4.1 to production', t: '2m ago' },
  { who: 'JS', text: 'Jordan approved 3 pending users', t: '14m ago' },
  { who: 'RL', text: 'Rin closed incident #1042', t: '1h ago' },
  { who: 'KP', text: 'Kai updated the billing schema', t: '3h ago' },
  { who: 'TQ', text: 'Tess merged feature/node-editor', t: '5h ago' },
]

const WEEKDAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun']

export default function Dashboard() {
  return (
    <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
      <div className="grid grid-cols-2 gap-4 lg:col-span-3 lg:grid-cols-4">
        {STATS.map((s) => (
          <Card key={s.label}>
            <StatInner {...s} />
          </Card>
        ))}
      </div>

      <Card title="Requests / sec" subtitle="Live stream" className="lg:col-span-2" action={<LiveBadge />}>
        <LiveLineChart />
      </Card>

      <Card title="Completion" subtitle="Sprint goal">
        <CompletionRing pct={72} />
      </Card>

      <Card title="Traffic by weekday" className="lg:col-span-2">
        <BarChart />
      </Card>

      <Card title="Service health">
        <div className="space-y-3">
          {SERVICES.map((s) => (
            <div key={s.name}>
              <div className="mb-1 flex justify-between text-xs">
                <span className="text-fg">{s.name}</span>
                <span className="text-muted">{s.pct}%</span>
              </div>
              <div className="h-2 overflow-hidden rounded-full bg-surface2">
                <div
                  className={`h-full rounded-full ${s.tone === 'success' ? 'bg-success' : 'bg-warning'}`}
                  style={{ width: `${s.pct}%` }}
                />
              </div>
            </div>
          ))}
        </div>
      </Card>

      <Card title="Activity" subtitle="Recent events" className="lg:col-span-3">
        <ul className="space-y-3">
          {FEED.map((f, i) => (
            <li key={i} className="flex items-center gap-3">
              <span className="flex h-8 w-8 items-center justify-center rounded-full bg-accent/15 text-xs font-semibold text-accent">
                {f.who}
              </span>
              <span className="flex-1 text-sm text-fg">{f.text}</span>
              <span className="text-xs text-muted">{f.t}</span>
            </li>
          ))}
        </ul>
      </Card>
    </div>
  )
}

function StatInner({ label, value, delta, tone }) {
  return (
    <div>
      <div className="text-xs text-muted">{label}</div>
      <div className="mt-1 flex items-end justify-between">
        <span className="text-2xl font-semibold text-fg">{value}</span>
        <Badge tone={tone}>{delta}</Badge>
      </div>
    </div>
  )
}

function LiveBadge() {
  return (
    <span className="inline-flex items-center gap-1.5 text-xs text-success">
      <span className="h-2 w-2 animate-pulse rounded-full bg-success" />
      streaming
    </span>
  )
}

function LiveLineChart() {
  const W = 560
  const H = 180
  const N = 40
  const [data, setData] = useState(() => Array.from({ length: N }, () => 40 + Math.random() * 40))

  useEffect(() => {
    const id = setInterval(() => {
      setData((prev) => {
        const next = prev.slice(1)
        const last = prev[prev.length - 1]
        let v = last + (Math.random() - 0.5) * 22
        v = Math.max(8, Math.min(96, v))
        next.push(v)
        return next
      })
    }, 1100)
    return () => clearInterval(id)
  }, [])

  const max = 100
  const pts = data.map((v, i) => [(i / (N - 1)) * W, H - (v / max) * H])
  const line = pts.map((p, i) => `${i === 0 ? 'M' : 'L'}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(' ')
  const area = `${line} L${W},${H} L0,${H} Z`

  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="w-full" preserveAspectRatio="none" style={{ height: 180 }}>
      <defs>
        <linearGradient id="liveFill" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="var(--primary)" stopOpacity="0.35" />
          <stop offset="100%" stopColor="var(--primary)" stopOpacity="0" />
        </linearGradient>
      </defs>
      <path d={area} fill="url(#liveFill)" />
      <path d={line} fill="none" stroke="var(--primary)" strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  )
}

function BarChart() {
  const [data] = useState(() => WEEKDAYS.map(() => 30 + Math.random() * 70))
  const [hover, setHover] = useState(-1)
  const max = Math.max(...data)
  return (
    <div className="flex h-44 items-end gap-2">
      {data.map((v, i) => (
        <div key={i} className="flex flex-1 flex-col items-center gap-1">
          <div className="flex w-full flex-1 items-end">
            <div
              onMouseEnter={() => setHover(i)}
              onMouseLeave={() => setHover(-1)}
              className={`w-full rounded-t-md transition-colors ${hover === i ? 'bg-accent' : 'bg-primary'}`}
              style={{ height: `${(v / max) * 100}%` }}
            />
          </div>
          <span className="text-xs text-muted">{WEEKDAYS[i]}</span>
        </div>
      ))}
    </div>
  )
}

function CompletionRing({ pct }) {
  const r = 56
  const c = 2 * Math.PI * r
  const off = c * (1 - pct / 100)
  return (
    <div className="flex items-center gap-4">
      <svg width="140" height="140" viewBox="0 0 140 140">
        <circle cx="70" cy="70" r={r} fill="none" stroke="var(--surface2)" strokeWidth="14" />
        <circle
          cx="70"
          cy="70"
          r={r}
          fill="none"
          stroke="var(--primary)"
          strokeWidth="14"
          strokeLinecap="round"
          strokeDasharray={c}
          strokeDashoffset={off}
          transform="rotate(-90 70 70)"
        />
        <text x="70" y="76" textAnchor="middle" className="fill-fg" style={{ fontSize: 22, fontWeight: 700 }}>
          {pct}%
        </text>
      </svg>
      <ul className="space-y-2 text-sm">
        <li className="flex items-center gap-2">
          <span className="h-3 w-3 rounded-sm bg-primary" /> Completed
        </li>
        <li className="flex items-center gap-2">
          <span className="h-3 w-3 rounded-sm bg-surface2" /> Remaining
        </li>
      </ul>
    </div>
  )
}
