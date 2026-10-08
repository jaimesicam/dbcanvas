import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { stackApi, patroniApi, repmgrApi, mongoApi } from '../lib/stackApi.js'
import { usePolling } from '../lib/usePolling.jsx'
import { Button, Toggle } from './ui.jsx'
import TimeChart from './TimeChart.jsx'
import { Icon } from './Icons.jsx'

// StackHistory.jsx — what the server's watcher (app/livewatch.go) recorded about a deployed stack:
// the alerts open now, the timeline of what happened, a day of trend lines per node, and the
// stack's alert rules. Opened from the canvas toolbar's Health button, whose badge is the count of
// open alerts.

const SEV_STYLE = {
  error: { background: 'color-mix(in srgb, var(--danger) 16%, transparent)', color: 'var(--danger)' },
  warning: { background: 'color-mix(in srgb, var(--warning) 18%, transparent)', color: 'var(--warning)' },
  success: { background: 'color-mix(in srgb, var(--success) 16%, transparent)', color: 'var(--success)' },
  info: { background: 'var(--surface2)', color: 'var(--muted)' },
}

const KIND_LABEL = {
  role: 'role', failover: 'failover', switchover: 'switchover', alert: 'alert', resolved: 'resolved', action: 'action',
}

export const ago = (sec) => {
  const d = Math.max(0, Math.round(Date.now() / 1000 - sec))
  if (d < 60) return `${d}s ago`
  if (d < 3600) return `${Math.round(d / 60)}m ago`
  if (d < 86400) return `${Math.floor(d / 3600)}h ${Math.round((d % 3600) / 60)}m ago`
  return `${Math.floor(d / 86400)}d ago`
}
const clock = (sec) => new Date(sec * 1000).toLocaleString([], { month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false })

// useStackAlerts polls the open alerts of a deployed stack — cheap, a database read — so the
// toolbar's Health badge stays current whether or not Live is on.
export function useStackAlerts(stackId, enabled) {
  const [data, setData] = useState({ alerts: [], rules: null, intervalSec: 0 })
  const poll = useCallback(async () => {
    try { setData(await stackApi.alerts(stackId)) } catch { /* the badge just keeps its last answer */ }
  }, [stackId])
  useEffect(() => { if (!enabled) setData({ alerts: [], rules: null, intervalSec: 0 }) }, [enabled])
  usePolling(poll, enabled ? 20000 : 0, { enabled })
  return [data, poll]
}

// HealthButton is the toolbar's summary: a check when nothing is wrong, else the count of open
// alerts in the colour of the worst.
export function HealthButton({ alerts, watching, onClick }) {
  const errors = alerts.filter((a) => a.severity === 'error').length
  const n = alerts.length
  const tone = errors ? 'danger' : n ? 'warning' : 'success'
  return (
    <Button size="sm" variant="ghost" onClick={onClick}
      title={!watching ? 'The history sampler is switched off (Settings → Stack history and alerts)' : n ? `${n} open alert${n === 1 ? '' : 's'}` : 'No open alerts'}>
      <Icon.Monitor size={15} /> <span className="@max-3xl:hidden">Health</span>
      {watching && (
        <span className="rounded px-1 text-[10px] font-bold" style={{ background: `color-mix(in srgb, var(--${tone}) 18%, transparent)`, color: `var(--${tone})` }}>
          {n ? n : '✓'}
        </span>
      )}
    </Button>
  )
}

// fixAction is the button an alert offers: what usually mends that kind of problem.
function fixAction(alert, actions) {
  switch (alert.fix) {
    case 'restart': return actions.restart && { label: 'Restart node', fn: () => actions.restart(alert.nodeId) }
    case 'rebuild': return actions.rebuild && { label: 'Rebuild replica…', fn: () => actions.rebuild(alert.nodeId) }
    default: return actions.inspect && { label: 'Diagnostics', fn: () => actions.inspect(alert.nodeId) }
  }
}

const RANGES = [{ m: 60, label: '1h' }, { m: 360, label: '6h' }, { m: 1440, label: '24h' }]

// METRICS are the trend charts: one line per node.
const METRICS = [
  { key: 'cpu', label: 'CPU', unit: '%' },
  { key: 'mem', label: 'Memory', unit: '%' },
  { key: 'lag', label: 'Replication lag', unit: 's' },
  { key: 'qps', label: 'Queries / s', unit: '' },
  { key: 'tps', label: 'Transactions / s', unit: '' },
  { key: 'conns', label: 'Connections', unit: '' },
  { key: 'readIops', label: 'Read IOPS', unit: '' },
  { key: 'writeIops', label: 'Write IOPS', unit: '' },
  { key: 'fsPct', label: 'Filesystem used', unit: '%' },
]

export function StackHistoryPanel({ stackId, nodes, alerts, rules, intervalSec, tab, setTab, actions, onSelectNode, onRulesSaved, onClose }) {
  const [range, setRange] = useState(60)
  const [hist, setHist] = useState(null)
  const [err, setErr] = useState('')
  const [nodeFilter, setNodeFilter] = useState('')
  const label = useCallback((id) => nodes.find((n) => n.id === id)?.label || id, [nodes])

  const load = useCallback(async () => {
    try {
      setHist(await stackApi.history(stackId, range))
      setErr('')
    } catch (e) { setErr(e.message) }
  }, [stackId, range])
  useEffect(() => { load() }, [load])
  usePolling(load, Math.max(15, intervalSec || 30) * 1000, { enabled: tab !== 'rules' })

  return createPortal(
    <div className="fixed inset-y-0 right-0 z-50 flex w-[min(640px,100vw)] flex-col border-l bg-surface shadow-2xl">
      <div className="flex items-center gap-2 border-b px-4 py-3">
        <Icon.Monitor size={16} />
        <h3 className="text-sm font-semibold">Health &amp; history</h3>
        <span className="text-xs text-muted">{intervalSec ? `sampled every ${intervalSec}s` : 'sampler off'}</span>
        <span className="flex-1" />
        <button type="button" onClick={onClose} className="rounded p-1 text-muted hover:bg-surface2" aria-label="Close"><Icon.Close size={16} /></button>
      </div>
      <div className="flex gap-1 border-b px-3 pt-2 text-xs" role="tablist">
        {[['alerts', `Alerts${alerts.length ? ` (${alerts.length})` : ''}`], ['timeline', 'Timeline'], ['trends', 'Trends'], ['rules', 'Rules']].map(([id, text]) => (
          <button key={id} type="button" role="tab" aria-selected={tab === id} onClick={() => setTab(id)}
            className={`rounded-t-md px-3 py-1.5 ${tab === id ? 'border border-b-0 bg-bg font-semibold text-fg' : 'text-muted hover:text-fg'}`}>{text}</button>
        ))}
        {(tab === 'timeline' || tab === 'trends') && (
          <div className="ml-auto flex items-center gap-1 pb-1">
            {RANGES.map((r) => (
              <button key={r.m} type="button" onClick={() => setRange(r.m)}
                className={`rounded px-2 py-0.5 ${range === r.m ? 'bg-primary text-primary-fg' : 'text-muted hover:bg-surface2'}`}>{r.label}</button>
            ))}
          </div>
        )}
      </div>
      <div className="min-h-0 flex-1 overflow-auto bg-bg p-4">
        {err && <div className="mb-3 rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
        {!intervalSec && (
          <div className="mb-3 rounded-lg border bg-surface px-3 py-2 text-xs text-muted">
            The history sampler is off for this installation, so nothing new is recorded. An administrator switches it on in Settings → Stack history and alerts.
          </div>
        )}
        {tab === 'alerts' && <AlertList alerts={alerts} label={label} actions={actions} onSelectNode={onSelectNode} />}
        {tab === 'timeline' && <Timeline events={hist?.events || []} label={label} onSelectNode={onSelectNode} />}
        {tab === 'trends' && (
          <Trends samples={hist?.samples || {}} events={hist?.events || []} label={label}
            nodeFilter={nodeFilter} setNodeFilter={setNodeFilter} nodes={nodes} />
        )}
        {tab === 'rules' && rules && <Rules stackId={stackId} rules={rules} onSaved={onRulesSaved} />}
      </div>
    </div>,
    document.body,
  )
}

function AlertList({ alerts, label, actions, onSelectNode }) {
  if (!alerts.length) {
    return (
      <div className="rounded-xl border bg-surface p-6 text-center text-sm text-muted">
        <div className="mb-1 text-2xl" style={{ color: 'var(--success)' }}>✓</div>
        Nothing is wrong right now.
      </div>
    )
  }
  return (
    <ul className="space-y-2">
      {alerts.map((a) => {
        const fix = fixAction(a, actions)
        return (
          <li key={a.nodeId + a.kind} className="rounded-xl border bg-surface p-3">
            <div className="flex items-start gap-2">
              <span className="mt-0.5 shrink-0 rounded px-1.5 py-px text-[10px] font-bold uppercase" style={SEV_STYLE[a.severity]}>{a.kind}</span>
              <div className="min-w-0 flex-1">
                <div className="text-sm font-medium">{a.title}</div>
                {a.detail && <div className="mt-0.5 whitespace-pre-wrap break-words text-xs text-muted">{a.detail}</div>}
                <div className="mt-1 text-[11px] text-muted">open since {clock(a.openedAt)} · {ago(a.openedAt)}</div>
              </div>
            </div>
            <div className="mt-2 flex flex-wrap gap-2">
              <Button size="sm" variant="outline" onClick={() => onSelectNode(a.nodeId)}>Show {label(a.nodeId)}</Button>
              {fix && <Button size="sm" variant="outline" onClick={fix.fn}>{fix.label}</Button>}
            </div>
          </li>
        )
      })}
    </ul>
  )
}

function Timeline({ events, label, onSelectNode }) {
  const [kind, setKind] = useState('')
  const kinds = useMemo(() => [...new Set(events.map((e) => e.kind))], [events])
  const shown = kind ? events.filter((e) => e.kind === kind) : events
  return (
    <div>
      <div className="mb-3 flex flex-wrap gap-1 text-xs">
        <button type="button" onClick={() => setKind('')} className={`rounded px-2 py-0.5 ${!kind ? 'bg-primary text-primary-fg' : 'bg-surface hover:bg-surface2'}`}>All</button>
        {kinds.map((k) => (
          <button key={k} type="button" onClick={() => setKind(k)} className={`rounded px-2 py-0.5 ${kind === k ? 'bg-primary text-primary-fg' : 'bg-surface hover:bg-surface2'}`}>{KIND_LABEL[k] || k}</button>
        ))}
      </div>
      {!shown.length ? (
        <div className="rounded-xl border bg-surface p-6 text-center text-sm text-muted">Nothing happened in this window.</div>
      ) : (
        <ol className="relative space-y-2 border-l pl-4">
          {shown.map((e) => (
            <li key={e.id} className="relative">
              <span className="absolute -left-[21px] top-1.5 h-2.5 w-2.5 rounded-full border-2 border-bg" style={{ background: SEV_STYLE[e.severity]?.color }} />
              <div className="flex flex-wrap items-baseline gap-x-2 text-[11px] text-muted">
                <span className="font-mono">{clock(e.at)}</span>
                <span className="rounded px-1 py-px text-[10px] font-semibold" style={SEV_STYLE[e.severity]}>{KIND_LABEL[e.kind] || e.kind}</span>
                {e.nodeId && <button type="button" className="underline-offset-2 hover:underline" onClick={() => onSelectNode(e.nodeId)}>{label(e.nodeId)}</button>}
                {e.actor && <span title="Who asked for it">by {e.actor}</span>}
              </div>
              <div className="text-sm">{e.title}</div>
              {e.detail && <div className="whitespace-pre-wrap break-words text-xs text-muted">{e.detail}</div>}
            </li>
          ))}
        </ol>
      )}
    </div>
  )
}

// Trends draws one chart per metric with a line per node (the first eight — the palette's size),
// skipping a metric no node has a value for. Failovers and alerts in the window are listed under
// the charts' common time axis.
function Trends({ samples, events, label, nodeFilter, setNodeFilter, nodes }) {
  const ids = Object.keys(samples).filter((id) => !nodeFilter || id === nodeFilter)
    .sort((a, b) => label(a).localeCompare(label(b))).slice(0, 8)
  const charts = METRICS.map((m) => {
    const byT = new Map()
    let any = false
    for (const id of ids) {
      for (const s of samples[id]) {
        if (s[m.key] == null) continue
        any = true
        const t = s.at
        if (!byT.has(t)) byT.set(t, { t, v: {} })
        byT.get(t).v[id] = s[m.key]
      }
    }
    if (!any) return null
    const points = [...byT.values()].sort((a, b) => a.t - b.t)
    return { m, points, lines: ids.filter((id) => samples[id].some((s) => s[m.key] != null)).map((id) => ({ key: id, label: label(id), color: ids.indexOf(id) })) }
  }).filter(Boolean)
  const marks = events.filter((e) => e.kind === 'failover' || e.kind === 'switchover' || e.kind === 'alert' || e.kind === 'role')
  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2 text-xs">
        <span className="text-muted">Node</span>
        <select value={nodeFilter} onChange={(e) => setNodeFilter(e.target.value)} className="rounded-md border bg-surface px-1.5 py-1 text-xs">
          <option value="">All (first 8)</option>
          {Object.keys(samples).map((id) => <option key={id} value={id}>{label(id)}</option>)}
        </select>
        {nodes.length > 8 && !nodeFilter && <span className="text-muted">— pick one to see the rest</span>}
      </div>
      {!charts.length && <div className="rounded-xl border bg-surface p-6 text-center text-sm text-muted">No samples yet. The first ones arrive within a sampling interval of the stack being deployed.</div>}
      {charts.map(({ m, points, lines }) => (
        <div key={m.key} className="rounded-xl border bg-surface p-3">
          <div className="mb-1 text-xs font-semibold">{m.label}</div>
          <TimeChart points={points} lines={lines} unit={m.unit} height={140} />
        </div>
      ))}
      {marks.length > 0 && (
        <div className="rounded-xl border bg-surface p-3 text-xs">
          <div className="mb-1 font-semibold">In this window</div>
          <ul className="space-y-0.5">
            {marks.slice(0, 30).map((e) => (
              <li key={e.id}><span className="font-mono text-muted">{clock(e.at)}</span> · {e.title}</li>
            ))}
          </ul>
        </div>
      )}
    </div>
  )
}

function Rules({ stackId, rules, onSaved }) {
  const [r, setR] = useState(rules)
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState('')
  useEffect(() => { setR(rules) }, [rules])
  const set = (k, v) => setR((p) => ({ ...p, [k]: v }))
  const num = (k, label, unit, hint) => (
    <label className="flex flex-wrap items-center gap-2 text-sm">
      <span className="w-48">{label}</span>
      <input type="number" min="0" value={r[k]} onChange={(e) => set(k, Number(e.target.value))}
        className="w-20 rounded-lg border bg-bg px-2 py-1 text-sm" />
      <span className="text-xs text-muted">{unit} · {hint}</span>
    </label>
  )
  const save = async () => {
    setBusy(true); setMsg('')
    try { const out = await stackApi.setAlertRules(stackId, r); setR(out); onSaved?.(); setMsg('Saved.') } catch (e) { setMsg(e.message) } finally { setBusy(false) }
  }
  return (
    <div className="space-y-4 rounded-xl border bg-surface p-4">
      <p className="text-xs text-muted">
        What raises an alert on this stack. An alert opens after the condition holds for two samples in a row (a database down: one), and closes after two clear ones,
        so a single bad sample is not a notification. A node you stopped yourself raises nothing.
      </p>
      <div><Toggle checked={r.down} onChange={(v) => set('down', v)} label="A database or its container stops answering" /></div>
      <div><Toggle checked={r.replication} onChange={(v) => set('replication', v)} label="The engine reports a replication problem" /></div>
      <div><Toggle checked={r.failover} onChange={(v) => set('failover', v)} label="A primary moves without DBCanvas moving it (unplanned failover)" /></div>
      {num('lagSec', 'Replica behind by more than', 'seconds', '0 switches it off')}
      {num('diskPct', 'Filesystem fuller than', '%', '0 switches it off')}
      {num('connPct', 'Connections in use at least', '% of max', '0 switches it off')}
      <div><Toggle checked={r.notify} onChange={(v) => set('notify', v)} label="Send alerts to the notification bell (the timeline records them either way)" /></div>
      <div className="flex items-center gap-3">
        <Button size="sm" onClick={save} disabled={busy}>{busy ? 'Saving…' : 'Save rules'}</Button>
        {msg && <span className="text-xs text-muted">{msg}</span>}
      </div>
    </div>
  )
}

// ErrorLogTail is the last lines of a database node's own error log (app/errorlog.go), fetched
// when asked for — it is the first thing to read when a member is down or its replication stopped.
export function ErrorLogTail({ stackId, nodeId }) {
  const [log, setLog] = useState(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [lines, setLines] = useState(100)
  const load = async (n = lines) => {
    setBusy(true); setErr('')
    try { setLog(await stackApi.errorLog(stackId, nodeId, n)) } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }
  useEffect(() => { setLog(null); setErr('') }, [nodeId])
  return (
    <div className="space-y-1 border-t pt-2">
      <div className="flex items-center gap-2">
        <span className="text-[10px] font-semibold tracking-wide text-muted">ERROR LOG</span>
        <span className="flex-1" />
        {log && (
          <select value={lines} onChange={(e) => { const n = Number(e.target.value); setLines(n); load(n) }} className="rounded border bg-surface px-1 py-0.5 text-[10px]" aria-label="Lines">
            {[50, 100, 200, 500, 1000].map((n) => <option key={n} value={n}>{n} lines</option>)}
          </select>
        )}
        <Button size="sm" variant="outline" onClick={() => load()} disabled={busy}>{busy ? 'Reading…' : log ? 'Refresh' : 'Show last lines'}</Button>
      </div>
      {err && <div className="text-[11px] text-danger">{err}</div>}
      {log && (
        <>
          <div className="truncate font-mono text-[10px] text-muted" title={log.source}>{log.source}</div>
          <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-all rounded-lg border bg-bg p-2 font-mono text-[10px] leading-snug">
            {log.lines.map((l, i) => (
              <div key={i} style={/\b(ERROR|FATAL|PANIC|\[ERROR\]|E\s+[A-Z]+\s)/.test(l) ? { color: 'var(--danger)' } : /\b(WARN|WARNING|\[Warning\])/.test(l) ? { color: 'var(--warning)' } : undefined}>{l}</div>
            ))}
          </pre>
        </>
      )}
    </div>
  )
}

// backupEnabled says whether a cluster was deployed with backups, and how they are taken.
export function backupEnabled(f) {
  if (f.type === 'patroni' && f.usePgBackRest) return 'patroni'
  if (f.type === 'repmgr' && (f.usePgBackRest || f.useBarman)) return 'repmgr'
  if ((f.type === 'psmrs' || f.type === 'psmdb') && f.enablePBM) return 'pbm'
  return ''
}

const fmtSize = (n) => (!n ? '' : n >= 1 << 30 ? `${(n / (1 << 30)).toFixed(1)} GB` : n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : `${Math.max(1, Math.round(n / 1024))} KB`)

// BackupChip is a cluster header's backup age (app/backupstatus.go): red when there is none,
// amber past a day, and a popover with the newest backup and Back up now.
export function BackupChip({ stackId, frame }) {
  const how = backupEnabled(frame)
  const [b, setB] = useState(null)
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState('')
  const ref = useRef(null)
  useEffect(() => {
    if (!open) return undefined
    const away = (e) => { if (ref.current && !ref.current.contains(e.target)) setOpen(false) }
    addEventListener('pointerdown', away)
    return () => removeEventListener('pointerdown', away)
  }, [open])
  const load = useCallback(async () => {
    try { setB(await stackApi.backupStatus(stackId, frame.id)) } catch (e) { setB({ error: e.message }) }
  }, [stackId, frame.id])
  useEffect(() => { if (how) load() }, [how, load])
  usePolling(load, how ? (b?.running ? 10000 : 120000) : 0, { enabled: !!how })
  if (!how || !b) return null
  const age = b.lastAt ? Date.now() / 1000 - b.lastAt : null
  const tone = b.error && !b.count ? 'muted' : !b.count ? 'danger' : age > 86400 ? 'warning' : 'success'
  const short = b.running ? 'running' : !b.count ? (b.error ? '?' : 'none') : ago(b.lastAt).replace(' ago', '')
  const now = async () => {
    setBusy(true); setMsg('')
    try {
      if (how === 'patroni') await patroniApi(stackId, frame.id).backup()
      else if (how === 'repmgr') await repmgrApi(stackId, frame.id).backup()
      else await mongoApi(stackId, frame.id).pbmBackup()
      setMsg('Backup taken.')
    } catch (e) { setMsg(e.message) } finally { setBusy(false); load() }
  }
  return (
    <span ref={ref} className="relative" onPointerDown={(e) => e.stopPropagation()}>
      <button type="button" onClick={() => setOpen((o) => !o)}
        title={b.count ? `Last backup ${ago(b.lastAt)} — ${b.count} in the repository` : b.error || 'No backup has been taken'}
        className="shrink-0 rounded px-1 py-px text-[9px] font-bold"
        style={{ background: `color-mix(in srgb, var(--${tone === 'muted' ? 'muted' : tone}) 16%, transparent)`, color: `var(--${tone === 'muted' ? 'muted' : tone})` }}>
        ⛁ {short}
      </button>
      {open && (
        <div className="absolute left-0 top-full z-30 mt-1 w-64 space-y-2 rounded-lg border bg-surface p-3 text-xs shadow-xl">
          <div className="font-semibold">Backups · {b.engine === 'pbm' ? 'PBM' : b.engine === 'barman' ? 'Barman cloud' : 'pgBackRest'}</div>
          {b.count > 0 ? (
            <div className="space-y-0.5 text-muted">
              <div>Newest: <span className="text-fg">{ago(b.lastAt)}</span>{b.type ? ` · ${b.type}` : ''}{b.size ? ` · ${fmtSize(b.size)}` : ''}</div>
              <div className="truncate font-mono text-[10px]" title={b.last}>{b.last}</div>
              <div>{b.count} in the repository</div>
            </div>
          ) : (
            <div style={{ color: b.error ? 'var(--muted)' : 'var(--danger)' }}>{b.error || 'No backup has been taken of this cluster yet.'}</div>
          )}
          {b.running && <div style={{ color: 'var(--primary)' }}>A backup is running.</div>}
          <div className="flex items-center gap-2">
            <Button size="sm" disabled={busy || b.running} onClick={now}>{busy ? 'Backing up…' : 'Back up now'}</Button>
            <Button size="sm" variant="ghost" onClick={() => setOpen(false)}>Close</Button>
          </div>
          {msg && <div className="text-muted">{msg}</div>}
        </div>
      )}
    </span>
  )
}
