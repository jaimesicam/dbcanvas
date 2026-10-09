import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { stackApi } from '../lib/stackApi.js'
import { Button } from './ui.jsx'
import { Icon } from './Icons.jsx'
import { useDialog } from './Dialog.jsx'

// Activity.jsx — what is running inside a node (app/activity.go), arranged around the questions a
// DBA asks: who blocks whom, which transactions are open and what they hold, what DDL is running
// and what queues behind it, when the last deadlocks were and how they went, which statements
// cost the most. Polls only while open; Freeze holds a snapshot still, Record keeps a minute of
// them to scrub through. Deep instrumentation (MySQL) is opt-in, on a timer, and says what it
// costs.

const TABS = [
  ['overview', 'Overview'], ['blocking', 'Blocking'], ['trx', 'Transactions'], ['sessions', 'Sessions'],
  ['ddl', 'DDL'], ['deadlocks', 'Deadlocks'], ['statements', 'Statements'], ['pools', 'Pools'],
]
const RECORD_MAX = 60

const fmtDur = (s) => {
  if (s == null || Number.isNaN(s)) return '—'
  if (s < 1) return `${Math.round(s * 1000)}ms`
  if (s < 60) return `${s < 10 ? s.toFixed(1) : Math.round(s)}s`
  if (s < 3600) return `${Math.floor(s / 60)}m ${Math.round(s % 60)}s`
  return `${Math.floor(s / 3600)}h ${Math.round((s % 3600) / 60)}m`
}
const fmtNum = (n) => (n == null ? '—' : n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${(n / 1e3).toFixed(1)}k` : `${Math.round(n)}`)
const fmtBytes = (n) => (n == null ? '—' : n >= 1 << 30 ? `${(n / (1 << 30)).toFixed(1)} GB` : n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : n >= 1024 ? `${(n / 1024).toFixed(0)} KB` : `${n} B`)
const oneLine = (s) => (s || '').replace(/\s+/g, ' ').trim()

const toneStyle = (tone) => ({ background: `color-mix(in srgb, var(--${tone}) 15%, transparent)`, color: `var(--${tone})` })

// withRates adds what needs two snapshots: CPU %, I/O and network per second, and per statement
// digest calls/s and time/s since the previous one.
function withRates(cur, prev) {
  if (!cur) return cur
  const dt = prev ? (cur.atMs - prev.atMs) / 1000 : 0
  const ticks = cur.clockTicks || 100
  const pmap = new Map((prev?.sessions || []).map((s) => [s.id, s]))
  const sessions = (cur.sessions || []).map((s) => {
    const p = pmap.get(s.id)
    const r = { ...s, isNew: prev && !p }
    if (p && dt > 0) {
      const d = (k) => (s[k] != null && p[k] != null && s[k] >= p[k] ? (s[k] - p[k]) / dt : null)
      const cpu = d('cpuTicks')
      r.cpuPct = cpu == null ? null : (cpu / ticks) * 100
      r.readRate = d('readBytes')
      r.writeRate = d('writeBytes')
      r.netRate = d('netSent') != null || d('netRecv') != null ? (d('netSent') || 0) + (d('netRecv') || 0) : null
    }
    return r
  })
  const dmap = new Map((prev?.digests || []).map((x) => [x.id, x]))
  const digests = (cur.digests || []).map((x) => {
    const p = dmap.get(x.id)
    if (!p || dt <= 0 || x.calls < p.calls) return { ...x }
    const calls = (x.calls - p.calls) / dt
    const time = (x.timeSec - p.timeSec) / dt
    return { ...x, callsPerSec: calls, timePerSec: time, avgMs: calls > 0 ? (time / calls) * 1000 : null,
      exaPerCall: x.calls - p.calls > 0 ? (x.rowsExamined - p.rowsExamined) / (x.calls - p.calls) : null }
  })
  return { ...cur, sessions, digests }
}

export function ActivityPanel({ stackId, node, members, nodeLabel, onClose, onOpenNode, focusSession }) {
  const [nid, setNid] = useState(node.id)
  const [tab, setTab] = useState(focusSession ? 'sessions' : 'overview')
  const [snap, setSnap] = useState(null)
  const [prev, setPrev] = useState(null)
  const [err, setErr] = useState('')
  const [frozen, setFrozen] = useState(false)
  const [interval, setIntervalMs] = useState(2000)
  const [resources, setResources] = useState(true)
  const [rec, setRec] = useState(null) // { frames: [], on: bool }
  const [scrub, setScrub] = useState(null) // index into rec.frames while scrubbing
  const [series, setSeries] = useState([]) // [{t, active, waiting}] — the Overview's sparkline
  const [highlight, setHighlight] = useState(focusSession || '')
  const [dialog, ask] = useDialog()
  const [plan, setPlan] = useState(null)
  const [busy, setBusy] = useState('')
  const inflight = useRef(false)
  const last = useRef(null)
  const label = nodeLabel(nid)

  const want = useMemo(() => {
    const w = []
    if (tab === 'statements') w.push('digests')
    if (tab === 'deadlocks') w.push('deadlock')
    if (resources && (tab === 'sessions' || tab === 'trx' || tab === 'overview')) w.push('resources')
    return w.join(',')
  }, [tab, resources])

  const poll = useCallback(async () => {
    if (inflight.current) return
    inflight.current = true
    try {
      const s = await stackApi.activity(stackId, nid, want)
      setPrev(last.current)
      last.current = s
      setSnap(s)
      setErr('')
      const active = (s.sessions || []).filter((x) => x.command && !/sleep|idle$/i.test(x.command) && x.command !== 'idle').length
      const waiting = (s.sessions || []).filter((x) => x.waiting).length
      setSeries((ser) => [...ser, { t: s.atMs, active, waiting }].slice(-90))
      setRec((r) => (r?.on ? { ...r, frames: [...r.frames, s].slice(-RECORD_MAX), on: r.frames.length + 1 < RECORD_MAX } : r))
    } catch (e) {
      setErr(e.message)
    } finally {
      inflight.current = false
    }
  }, [stackId, nid, want])

  useEffect(() => { last.current = null; setSnap(null); setPrev(null); setSeries([]); setRec(null); setScrub(null) }, [nid])
  useEffect(() => {
    if (frozen || scrub != null) return undefined
    poll()
    const t = setInterval(poll, rec?.on ? 1000 : interval)
    return () => clearInterval(t)
  }, [poll, frozen, interval, scrub, rec?.on])

  const shown = useMemo(() => {
    if (scrub != null && rec?.frames?.length) {
      const i = Math.min(scrub, rec.frames.length - 1)
      return withRates(rec.frames[i], rec.frames[i - 1])
    }
    return withRates(snap, prev)
  }, [snap, prev, scrub, rec])

  const engine = shown?.engine
  const isProxy = engine === 'proxysql' || engine === 'pgbouncer'
  const tabs = TABS.filter(([id]) => {
    if (isProxy) return ['overview', 'blocking', 'sessions', 'statements', 'pools'].includes(id) && !(engine === 'pgbouncer' && id === 'statements')
    if (id === 'pools') return false
    if (engine === 'mongodb' && (id === 'deadlocks' || id === 'statements')) return false
    return true
  })

  const kill = async (s, mode) => {
    const impact = s.trxRowsModified ? ` Its transaction has modified ${fmtNum(s.trxRowsModified)} rows, which roll back first — that can take as long as it took to write them.` : ''
    const waiters = (shown?.waits || []).filter((w) => w.blocker === s.id).length
    const ok = await ask.confirm({
      title: mode === 'connection' ? `Kill connection ${s.id}?` : `Kill the statement of session ${s.id}?`,
      body: `${s.user || ''}${s.host ? '@' + s.host : ''} — ${oneLine(s.statement).slice(0, 160) || s.command}.${impact}${waiters ? ` It releases ${waiters} waiting session(s).` : ''}`,
      confirmLabel: mode === 'connection' ? 'Kill connection' : 'Kill statement', danger: true,
    })
    if (!ok) return
    setBusy(s.id)
    try { await stackApi.activityKill(stackId, nid, s.id, mode); poll() } catch (e) { setErr(e.message) } finally { setBusy('') }
  }
  const explain = async (s) => {
    setPlan({ session: s, text: 'Asking the server…' })
    try { const r = await stackApi.activityExplain(stackId, nid, s.id, s.statement, s.db); setPlan({ session: s, text: r.plan || '(no plan)' }) } catch (e) { setPlan({ session: s, text: e.message }) }
  }
  const actions = { kill, explain, busy, killable: !!shown && engine !== 'pgbouncer', explainable: engine === 'mysql' || engine === 'postgres', proxy: isProxy, onOpenNode, highlight, setHighlight }

  return createPortal(
    <div className="fixed inset-y-0 right-0 z-50 flex w-[min(1180px,100vw)] flex-col border-l bg-surface shadow-2xl">
      {/* header */}
      <div className="flex flex-wrap items-center gap-2 border-b px-4 py-2.5">
        <Icon.Pulse size={16} />
        <h3 className="text-sm font-semibold">Activity</h3>
        {members.length > 1 ? (
          <select value={nid} onChange={(e) => setNid(e.target.value)} className="rounded-md border bg-bg px-1.5 py-1 text-xs" aria-label="Member">
            {members.map((m) => <option key={m.id} value={m.id}>{m.label}</option>)}
          </select>
        ) : <span className="text-sm">{label}</span>}
        {engine && <span className="rounded bg-surface2 px-1.5 py-px text-[10px] font-semibold uppercase text-muted">{engine}</span>}
        <span className="flex-1" />
        {scrub == null && (
          <>
            <select value={interval} onChange={(e) => setIntervalMs(Number(e.target.value))} className="rounded-md border bg-bg px-1.5 py-1 text-xs" aria-label="Refresh">
              {[1000, 2000, 5000, 10000].map((ms) => <option key={ms} value={ms}>every {ms / 1000}s</option>)}
            </select>
            <Button size="sm" variant={frozen ? 'primary' : 'outline'} onClick={() => setFrozen((f) => !f)} title="Hold this snapshot still while you read it">
              {frozen ? 'Frozen — resume' : 'Freeze'}
            </Button>
          </>
        )}
        {!rec?.on && (
          <Button size="sm" variant="outline" onClick={() => { setRec({ frames: [], on: true }); setScrub(null); setFrozen(false) }}
            title="Take a snapshot every second for a minute, then scrub through them">⏺ Record 60s</Button>
        )}
        {rec?.on && <span className="rounded px-2 py-0.5 text-xs font-semibold" style={toneStyle('danger')}>⏺ recording {rec.frames.length}/{RECORD_MAX}</span>}
        {rec?.on && <Button size="sm" variant="ghost" onClick={() => setRec((r) => ({ ...r, on: false }))}>Stop</Button>}
        {engine === 'mysql' && <DeepControl stackId={stackId} nid={nid} deep={shown?.deep} onChange={poll} />}
        <button type="button" onClick={onClose} className="rounded p-1 text-muted hover:bg-surface2" aria-label="Close"><Icon.Close size={16} /></button>
      </div>
      {rec && !rec.on && rec.frames.length > 0 && (
        <div className="flex items-center gap-3 border-b bg-bg px-4 py-2 text-xs">
          <span className="font-semibold">Recording</span>
          <input type="range" min="0" max={rec.frames.length - 1} value={scrub ?? rec.frames.length - 1} className="flex-1"
            onChange={(e) => setScrub(Number(e.target.value))} aria-label="Scrub the recording" />
          <span className="w-40 font-mono text-muted">{new Date(rec.frames[Math.min(scrub ?? rec.frames.length - 1, rec.frames.length - 1)].atMs).toLocaleTimeString()} ({(scrub ?? rec.frames.length - 1) + 1}/{rec.frames.length})</span>
          <Button size="sm" variant="ghost" onClick={() => { setScrub(null); setRec(null) }}>Back to live</Button>
        </div>
      )}
      <div className="flex gap-1 overflow-x-auto border-b px-3 pt-2 text-xs" role="tablist">
        {tabs.map(([id, text]) => {
          const n = badgeFor(id, shown)
          return (
            <button key={id} type="button" role="tab" aria-selected={tab === id} onClick={() => setTab(id)}
              className={`whitespace-nowrap rounded-t-md px-3 py-1.5 ${tab === id ? 'border border-b-0 bg-bg font-semibold text-fg' : 'text-muted hover:text-fg'}`}>
              {text}{n ? <span className="ml-1 rounded px-1 text-[10px] font-bold" style={toneStyle(n.tone)}>{n.n}</span> : null}
            </button>
          )
        })}
      </div>
      <div className="min-h-0 flex-1 overflow-auto bg-bg p-4">
        {err && <div className="mb-3 rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">{err}</div>}
        {!shown && !err && <div className="text-sm text-muted">Asking {label}…</div>}
        {shown && (
          <>
            {(shown.notes || []).map((n) => <div key={n} className="mb-2 text-xs text-muted">ⓘ {n}</div>)}
            {tab === 'overview' && <Overview s={shown} series={series} goto={setTab} a={actions} />}
            {tab === 'blocking' && <Blocking s={shown} a={actions} />}
            {tab === 'trx' && <Transactions s={shown} a={actions} />}
            {tab === 'sessions' && <Sessions s={shown} a={actions} resources={resources} setResources={setResources} />}
            {tab === 'ddl' && <DDL s={shown} prev={prev} />}
            {tab === 'deadlocks' && <Deadlocks stackId={stackId} nid={nid} latest={shown.deadlock} count={shown.deadlockCount} />}
            {tab === 'statements' && <Statements s={shown} />}
            {tab === 'pools' && <Pools s={shown} />}
          </>
        )}
      </div>
      {shown && (
        <div className="flex items-center gap-3 border-t px-4 py-1.5 text-[11px] text-muted">
          <span>Snapshot {shown.costMs} ms{scrub == null && !frozen ? `, every ${(rec?.on ? 1000 : interval) / 1000}s while this is open` : ''}{want ? ` · with ${want.replaceAll(',', ', ')}` : ''}</span>
          <span className="flex-1" />
          <span>{new Date(shown.atMs).toLocaleTimeString()}</span>
        </div>
      )}
      {plan && (
        <div className="fixed inset-0 z-[60] flex items-center justify-center bg-black/40 p-4" onMouseDown={() => setPlan(null)}>
          <div className="flex max-h-[80vh] w-full max-w-3xl flex-col rounded-xl border bg-surface p-4 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
            <div className="mb-2 text-sm font-semibold">Plan of session {plan.session.id}</div>
            <pre className="mb-2 max-h-24 overflow-auto whitespace-pre-wrap rounded border bg-bg p-2 font-mono text-[11px]">{plan.session.statement}</pre>
            <pre className="min-h-0 flex-1 overflow-auto whitespace-pre rounded border bg-bg p-2 font-mono text-[11px]">{plan.text}</pre>
            <div className="mt-2 flex justify-end"><Button size="sm" variant="ghost" onClick={() => setPlan(null)}>Close</Button></div>
          </div>
        </div>
      )}
      {dialog}
    </div>,
    document.body,
  )
}

function badgeFor(id, s) {
  if (!s) return null
  const waiting = (s.waits || []).length
  switch (id) {
    case 'blocking': return waiting ? { n: waiting, tone: 'danger' } : null
    case 'trx': {
      const n = (s.sessions || []).filter((x) => x.trxAgeSec != null).length
      return n ? { n, tone: (s.sessions || []).some((x) => x.trxAgeSec > 60 && isIdle(x)) ? 'warning' : 'muted' } : null
    }
    case 'ddl': return s.ddl?.length ? { n: s.ddl.length, tone: 'primary' } : null
    case 'sessions': return { n: (s.sessions || []).length, tone: 'muted' }
    default: return null
  }
}

const isIdle = (x) => /^(sleep|idle|idle in transaction.*)$/i.test(x.command || '')

// sessionLine is a session in one line, for the trees and lists.
function SessionCard({ s, a, wait, depth = 0, note, children }) {
  const [open, setOpen] = useState(false)
  const hi = a.highlight && (a.highlight === s.id || (s.host || '').endsWith(':' + a.highlight))
  return (
    <div className={`rounded-lg border bg-surface ${hi ? 'ring-2 ring-primary' : ''}`} style={{ marginLeft: depth * 22 }}>
      <div className="flex flex-wrap items-center gap-2 px-3 py-2 text-xs">
        <span className="font-mono font-semibold">#{s.id}</span>
        <span className="text-muted">{s.user}{s.host ? `@${s.host}` : ''}{s.db ? ` · ${s.db}` : ''}</span>
        <span className="rounded bg-surface2 px-1.5 py-px text-[10px]">{s.command}{s.state ? ` · ${s.state}` : ''}</span>
        <span className="font-mono">{fmtDur(s.timeSec)}</span>
        {s.trxAgeSec != null && <span className="rounded px-1.5 py-px text-[10px] font-semibold" style={toneStyle(s.trxAgeSec > 60 ? 'warning' : 'muted')}>trx {fmtDur(s.trxAgeSec)}{s.trxRowsModified ? ` · ${fmtNum(s.trxRowsModified)} modified` : ''}{s.trxRowsLocked ? ` · ${fmtNum(s.trxRowsLocked)} locked` : ''}</span>}
        {wait && (
          <span className="rounded px-1.5 py-px text-[10px] font-semibold" style={toneStyle('danger')}>
            waits {fmtDur(wait.waitSec)} for {wait.kind} lock{wait.mode ? ` ${wait.mode}` : ''}{wait.object ? ` on ${wait.object}` : ''}{wait.index ? ` (${wait.index})` : ''}{wait.blockerMode ? ` — held as ${wait.blockerMode}` : ''}
          </span>
        )}
        {note && <span className="text-[10px] text-muted">{note}</span>}
        <span className="flex-1" />
        {s.history?.length > 0 && <button type="button" className="text-primary hover:underline" onClick={() => setOpen((o) => !o)}>{open ? 'hide' : 'what it ran'}</button>}
        {a.explainable && s.statement && !isIdle(s) && <button type="button" className="text-primary hover:underline" onClick={() => a.explain(s)}>EXPLAIN</button>}
        {s.statement && <button type="button" className="text-muted hover:text-fg" title="Copy the statement" onClick={() => navigator.clipboard?.writeText(s.statement)}>copy</button>}
        {a.killable && s.killable && (
          <>
            {!a.proxy && !isIdle(s) && <button type="button" disabled={a.busy === s.id} className="text-warning hover:underline" onClick={() => a.kill(s, 'query')}>kill statement</button>}
            <button type="button" disabled={a.busy === s.id} className="text-danger hover:underline" onClick={() => a.kill(s, 'connection')}>kill connection</button>
          </>
        )}
      </div>
      {s.statement && <div className="border-t px-3 py-1.5 font-mono text-[11px] text-fg" title={s.statement}>{oneLine(s.statement).slice(0, 300)}</div>}
      {s.backend && (
        <div className="border-t px-3 py-1.5 text-[11px] text-muted">
          on backend {s.backend.host}:{s.backend.port}{s.backend.node && a.onOpenNode && (
            <> — <button type="button" className="text-primary hover:underline" onClick={() => a.onOpenNode(s.backend.node, s.backend.localPort)}>open {s.backend.label}&apos;s session for it →</button></>
          )}
        </div>
      )}
      {open && (
        <ol className="border-t bg-bg px-3 py-1.5 font-mono text-[11px] text-muted">
          {s.history.map((h, i) => <li key={i}>{oneLine(h).slice(0, 300)}</li>)}
        </ol>
      )}
      {children}
    </div>
  )
}

function Overview({ s, series, goto, a }) {
  const sessions = s.sessions || []
  const active = sessions.filter((x) => !isIdle(x)).length
  const waiting = sessions.filter((x) => x.waiting).length
  const idleTrx = sessions.filter((x) => isIdle(x) && x.trxAgeSec != null).length
  const trx = sessions.filter((x) => x.trxAgeSec != null)
  // The worst offender: the session blocking the most others, else the oldest transaction.
  const blocks = {}
  for (const w of s.waits || []) if (w.blocker) blocks[w.blocker] = (blocks[w.blocker] || 0) + 1
  const top = Object.entries(blocks).sort((x, y) => y[1] - x[1])[0]
  const topS = top && sessions.find((x) => x.id === top[0])
  const oldest = [...trx].sort((x, y) => y.trxAgeSec - x.trxAgeSec)[0]
  const card = (n, text, tone, tab) => (
    <button type="button" onClick={() => tab && goto(tab)} className="rounded-xl border bg-surface p-3 text-left hover:bg-surface2">
      <div className="text-2xl font-semibold" style={n && tone ? { color: `var(--${tone})` } : undefined}>{n}</div>
      <div className="text-xs text-muted">{text}</div>
    </button>
  )
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
        {card(active, 'running now', null, 'sessions')}
        {card(waiting, 'waiting for a lock', 'danger', 'blocking')}
        {card(idleTrx, 'idle in a transaction', 'warning', 'trx')}
        {card((s.ddl || []).length, 'DDL running', 'primary', 'ddl')}
        {card(sessions.length, 'sessions', null, 'sessions')}
      </div>
      {(topS || (oldest && oldest.trxAgeSec > 30)) && (
        <div className="rounded-xl border p-3 text-sm" style={{ borderColor: 'color-mix(in srgb, var(--warning) 45%, transparent)' }}>
          <div className="mb-2 text-xs font-semibold" style={{ color: 'var(--warning)' }}>Look here first</div>
          {topS
            ? <p className="mb-2">Session <b>#{topS.id}</b> ({topS.user}@{topS.host}) blocks <b>{top[1]}</b> session{top[1] > 1 ? 's' : ''}{topS.trxAgeSec != null ? <> and has held its transaction open for <b>{fmtDur(topS.trxAgeSec)}</b></> : null}{isIdle(topS) ? ' while idle — it is waiting for its application, not the database' : ''}.</p>
            : <p className="mb-2">The oldest open transaction, session <b>#{oldest.id}</b> ({oldest.user}@{oldest.host}), has been open <b>{fmtDur(oldest.trxAgeSec)}</b>{isIdle(oldest) ? ' and is idle' : ''}.</p>}
          <SessionCard s={topS || oldest} a={a} />
        </div>
      )}
      <div className="rounded-xl border bg-surface p-3">
        <div className="mb-1 flex items-center gap-3 text-xs"><span className="font-semibold">Last {series.length} snapshots</span>
          <span style={{ color: 'var(--primary)' }}>● running</span><span style={{ color: 'var(--danger)' }}>● waiting</span></div>
        <Spark series={series} />
      </div>
    </div>
  )
}

function Spark({ series }) {
  if (series.length < 2) return <div className="py-6 text-center text-xs text-muted">collecting…</div>
  const w = 1000, h = 70
  const max = Math.max(1, ...series.map((p) => Math.max(p.active, p.waiting)))
  const line = (k) => series.map((p, i) => `${(i / (series.length - 1)) * w},${h - (p[k] / max) * (h - 4) - 2}`).join(' ')
  return (
    <svg viewBox={`0 0 ${w} ${h}`} className="h-[70px] w-full" preserveAspectRatio="none" aria-hidden="true">
      <polyline points={line('active')} fill="none" stroke="var(--primary)" strokeWidth="2" vectorEffect="non-scaling-stroke" />
      <polyline points={line('waiting')} fill="none" stroke="var(--danger)" strokeWidth="2" vectorEffect="non-scaling-stroke" />
    </svg>
  )
}

// Blocking draws the lock waits as trees, each waiter once: under its root cause — a blocker that
// waits for nobody — when one of its blockers is one, else under its first blocker. A waiter held
// up by several (an ALTER waits for every session that touched the table) says who else on its
// card, and a second root whose waiters are already shown says whom it also blocks. A cycle (a
// deadlock in the making) has no root and is shown from any of its members.
function Blocking({ s, a }) {
  const sessions = new Map((s.sessions || []).map((x) => [x.id, x]))
  const waits = s.waits || []
  if (!waits.length) return <Empty text="Nobody is waiting for a lock." />
  const blockersOf = {}
  const waitOf = {}
  for (const w of waits) {
    if (!w.blocker) continue
    ;(blockersOf[w.waiter] ||= []).push(w.blocker)
    if (!waitOf[w.waiter]) waitOf[w.waiter] = w
  }
  const waiting = new Set(Object.keys(blockersOf))
  const isRoot = (id) => !waiting.has(id)
  const parentOf = {}
  for (const [w, bs] of Object.entries(blockersOf)) parentOf[w] = bs.find(isRoot) || bs[0]
  const kids = {}
  for (const [w, p] of Object.entries(parentOf)) (kids[p] ||= []).push(w)
  const allBlockers = new Set(Object.values(blockersOf).flat())
  let roots = [...allBlockers].filter(isRoot)
  if (!roots.length) roots = [...allBlockers].slice(0, 1)
  const done = new Set()
  const under = (id) => { let n = 0; for (const k of kids[id] || []) n += 1 + under(k); return n }
  const draw = (id, depth) => {
    if (done.has(id) || depth > 12) return null
    done.add(id)
    const x = sessions.get(id) || { id, command: 'gone', statement: '' }
    const others = (blockersOf[id] || []).filter((b) => b !== parentOf[id])
    const alsoBlocks = (kids[id] || []).length === 0 ? waits.filter((w) => w.blocker === id).map((w) => w.waiter) : []
    const note = [others.length ? `also waits for ${others.map((o) => '#' + o).join(', ')}` : '', alsoBlocks.length ? `blocks ${[...new Set(alsoBlocks)].map((o) => '#' + o).join(', ')} (shown under another blocker)` : ''].filter(Boolean).join(' · ')
    return (
      <div key={id} className="space-y-2">
        <SessionCard s={x} a={a} wait={waitOf[id]} depth={depth} note={note} />
        {(kids[id] || []).map((k) => draw(k, depth + 1))}
      </div>
    )
  }
  const unknown = waits.filter((w) => !w.blocker)
  return (
    <div className="space-y-4">
      {roots.map((r) => (
        <div key={r} className="space-y-2">
          <div className="text-xs font-semibold" style={{ color: 'var(--danger)' }}>
            Blocker #{r} — {under(r) || waits.filter((w) => w.blocker === r).length} waiting behind it
            {isIdle(sessions.get(r) || {}) ? ' · it is idle: its application holds the transaction open' : ''}
          </div>
          {draw(r, 0)}
        </div>
      ))}
      {unknown.length > 0 && (
        <div className="space-y-2">
          <div className="text-xs font-semibold text-muted">Waiting — the engine does not say for whom</div>
          {unknown.map((w) => <SessionCard key={w.waiter} s={sessions.get(w.waiter) || { id: w.waiter }} a={a} wait={w} />)}
        </div>
      )}
    </div>
  )
}

function countUnder(id, kids, seen = new Set()) {
  let n = 0
  for (const w of kids[id] || []) {
    if (seen.has(w.waiter)) continue
    seen.add(w.waiter)
    n += 1 + countUnder(w.waiter, kids, seen)
  }
  return n
}

function Transactions({ s, a }) {
  const trx = (s.sessions || []).filter((x) => x.trxAgeSec != null).sort((x, y) => y.trxAgeSec - x.trxAgeSec)
  if (!trx.length) return <Empty text="No transaction is open." />
  return (
    <div className="space-y-2">
      <p className="text-xs text-muted">Open transactions, oldest first. An idle one is holding its locks — and on PostgreSQL holding back vacuum — while it waits for its application.</p>
      {trx.map((x) => <SessionCard key={x.id} s={x} a={a} wait={(s.waits || []).find((w) => w.waiter === x.id)} />)}
    </div>
  )
}

function Sessions({ s, a, resources, setResources }) {
  const [q, setQ] = useState('')
  const [hideIdle, setHideIdle] = useState(true)
  const [sort, setSort] = useState('time')
  const rows = (s.sessions || [])
    .filter((x) => !hideIdle || !isIdle(x) || x.trxAgeSec != null || a.highlight === x.id)
    .filter((x) => !q || JSON.stringify([x.id, x.user, x.host, x.db, x.statement, x.state]).toLowerCase().includes(q.toLowerCase()))
    .sort((x, y) => (sort === 'cpu' ? (y.cpuPct || 0) - (x.cpuPct || 0) : sort === 'mem' ? (y.memBytes || 0) - (x.memBytes || 0) : y.timeSec - x.timeSec) || x.id.localeCompare(y.id))
  const th = 'px-2 py-1 text-left font-semibold'
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-3 text-xs">
        <input placeholder="Filter: user, host, statement…" value={q} onChange={(e) => setQ(e.target.value)} className="w-64 rounded-md border bg-surface px-2 py-1" />
        <label className="flex items-center gap-1"><input type="checkbox" checked={hideIdle} onChange={(e) => setHideIdle(e.target.checked)} /> hide idle</label>
        <label className="flex items-center gap-1" title="CPU and I/O per session from the OS (/proc), memory and network from the server's own accounting — nothing is switched on to get them">
          <input type="checkbox" checked={resources} onChange={(e) => setResources(e.target.checked)} /> CPU / I/O / memory per session</label>
        <span className="flex-1" />
        <span className="text-muted">sort</span>
        <select value={sort} onChange={(e) => setSort(e.target.value)} className="rounded-md border bg-surface px-1 py-0.5"><option value="time">time</option><option value="cpu">CPU</option><option value="mem">memory</option></select>
      </div>
      <div className="overflow-hidden rounded-lg border bg-surface">
        <table className="w-full border-collapse text-[11px]">
          <thead><tr className="bg-surface2 text-muted">
            <th className={th}>SESSION</th><th className={th}>USER@HOST</th><th className={th}>STATE</th><th className={th}>TIME</th>
            {resources && <><th className={th}>CPU</th><th className={th}>I/O R/W</th><th className={th}>MEM</th><th className={th}>NET</th></>}
            <th className={th}>STATEMENT</th><th />
          </tr></thead>
          <tbody>
            {rows.map((x) => (
              <tr key={x.id} className={`border-t align-top ${x.isNew ? 'bg-primary/5' : ''} ${a.highlight && (a.highlight === x.id || (x.host || '').endsWith(':' + a.highlight)) ? 'bg-primary/15' : ''}`}>
                <td className="px-2 py-1 font-mono">{x.id}{x.waiting && <span className="ml-1" style={{ color: 'var(--danger)' }} title={`waits for ${(x.blockedBy || []).join(', ') || 'a lock'}`}>🔒</span>}</td>
                <td className="max-w-[180px] truncate px-2 py-1" title={`${x.user}@${x.host}`}>{x.user}@{x.host}</td>
                <td className="max-w-[160px] truncate px-2 py-1" title={x.state}>{x.command}{x.state ? ` · ${x.state}` : ''}</td>
                <td className="px-2 py-1 font-mono">{fmtDur(x.timeSec)}</td>
                {resources && (
                  <>
                    <td className="px-2 py-1 font-mono">{x.cpuPct == null ? '—' : `${x.cpuPct.toFixed(0)}%`}</td>
                    <td className="px-2 py-1 font-mono">{x.readRate == null ? '—' : `${fmtBytes(x.readRate)}/${fmtBytes(x.writeRate)}`}</td>
                    <td className="px-2 py-1 font-mono">{fmtBytes(x.memBytes)}</td>
                    <td className="px-2 py-1 font-mono">{x.netRate == null ? '—' : `${fmtBytes(x.netRate)}/s`}</td>
                  </>
                )}
                <td className="max-w-[360px] px-2 py-1 font-mono" title={x.statement}><div className="line-clamp-2">{oneLine(x.statement)}</div></td>
                <td className="whitespace-nowrap px-2 py-1 text-right">
                  {a.explainable && x.statement && !isIdle(x) && <button type="button" className="mr-2 text-primary hover:underline" onClick={() => a.explain(x)}>EXPLAIN</button>}
                  {x.backend?.node && a.onOpenNode && <button type="button" className="mr-2 text-primary hover:underline" onClick={() => a.onOpenNode(x.backend.node, x.backend.localPort)}>→ {x.backend.label}</button>}
                  {a.killable && x.killable && <button type="button" className="text-danger hover:underline" onClick={() => a.kill(x, 'connection')}>kill</button>}
                </td>
              </tr>
            ))}
            {!rows.length && <tr><td colSpan="10" className="px-2 py-4 text-center text-muted">No sessions match.</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  )
}

// DDL shows each running change with its progress and an ETA from the progress rate between two
// snapshots, and the sessions queued behind it.
function DDL({ s, prev }) {
  const ddl = s.ddl || []
  if (!ddl.length) return <Empty text="No DDL is running." />
  const before = new Map((prev?.ddl || []).map((d) => [d.session, d]))
  return (
    <div className="space-y-3">
      {ddl.map((d) => {
        const pct = d.total ? Math.min(100, (d.done / d.total) * 100) : null
        const p = before.get(d.session)
        let eta = null
        if (p && d.total && p.done != null && d.done > p.done && s.atMs && prev?.atMs) {
          const rate = (d.done - p.done) / ((s.atMs - prev.atMs) / 1000)
          eta = (d.total - d.done) / rate
        }
        return (
          <div key={d.session} className="rounded-xl border bg-surface p-3 text-xs">
            <div className="mb-1 flex flex-wrap items-center gap-2">
              <span className="font-mono font-semibold">#{d.session}</span>
              <span className="text-muted">{d.phase}</span>
              {d.tool && <span className="rounded bg-surface2 px-1.5 py-px text-[10px]">{d.tool}</span>}
              <span className="flex-1" />
              <span className="font-mono">{fmtDur(d.timeSec)}{eta != null ? ` · about ${fmtDur(eta)} left` : ''}</span>
            </div>
            <div className="mb-2 font-mono text-[11px]">{oneLine(d.statement).slice(0, 400)}</div>
            {pct != null ? (
              <div className="mb-1 h-2 overflow-hidden rounded-full bg-surface2"><div className="h-full rounded-full bg-primary transition-[width] duration-700" style={{ width: `${pct}%` }} /></div>
            ) : (
              <div className="mb-1 text-[11px] text-muted">No progress figures for this one{s.engine === 'mysql' ? ' — deep instrumentation (top right) adds them for ALTER TABLE' : ''}.</div>
            )}
            {pct != null && <div className="text-[11px] text-muted">{pct.toFixed(1)}% ({fmtNum(d.done)} of {fmtNum(d.total)})</div>}
            {d.queue?.length > 0 && (
              <div className="mt-2 rounded border px-2 py-1 text-[11px]" style={{ borderColor: 'color-mix(in srgb, var(--danger) 40%, transparent)', color: 'var(--danger)' }}>
                {d.queue.length} session(s) waiting behind it: {d.queue.map((q) => `#${q}`).join(', ')}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}

// Deadlocks: the history the watcher recorded, each drawn as the two transactions — what each
// held, what each waited for, and which one was rolled back.
function Deadlocks({ stackId, nid, latest, count }) {
  const [list, setList] = useState(null)
  useEffect(() => { stackApi.deadlocks(stackId, nid).then((r) => setList(r.deadlocks)).catch(() => setList([])) }, [stackId, nid])
  const items = [...(list || [])]
  if (latest && !items.some((i) => i.deadlock?.at === latest.at)) items.unshift({ at: 0, title: `Latest, at ${latest.at} (not yet recorded by the watcher)`, deadlock: latest })
  if (!list) return <div className="text-xs text-muted">Reading…</div>
  if (!items.length) return <Empty text={count != null ? `No deadlock recorded. The server counts ${count} since its statistics were reset.` : 'No deadlock recorded on this node.'} />
  return (
    <div className="space-y-4">
      {items.map((it, i) => <DeadlockCard key={i} it={it} />)}
    </div>
  )
}

function DeadlockCard({ it }) {
  const [raw, setRaw] = useState(false)
  const d = it.deadlock
  const pg = d?.engine === 'postgres'
  return (
    <div className="rounded-xl border bg-surface p-3 text-xs">
      <div className="mb-2 font-semibold">{it.title}</div>
      {d?.txs?.length ? (
        <div className="grid gap-3 md:grid-cols-2">
          {d.txs.map((t) => (
            <div key={t.n} className="rounded-lg border p-2" style={t.victim ? { borderColor: 'color-mix(in srgb, var(--danger) 50%, transparent)' } : undefined}>
              <div className="mb-1 flex items-center gap-2"><span className="font-semibold">Transaction {t.n}</span>
                {t.thread && <span className="text-muted">{pg ? 'process' : 'thread'} {t.thread}{t.user ? ` · ${t.user}` : ''}</span>}
                {t.victim && <span className="rounded px-1.5 py-px text-[10px] font-bold" style={toneStyle('danger')}>rolled back</span>}</div>
              <div className="mb-1 font-mono text-[11px]">{oneLine(t.statement)}</div>
              {t.holds && <div><span className="text-muted">holds </span>{t.holds}</div>}
              {t.waitsFor && <div><span className="text-muted">waits for </span>{t.waitsFor}</div>}
            </div>
          ))}
        </div>
      ) : <div className="text-muted">{it.note}</div>}
      {d?.raw && <button type="button" className="mt-2 text-primary hover:underline" onClick={() => setRaw((r) => !r)}>{raw ? 'hide' : 'show'} {pg ? 'the server log’s report' : 'InnoDB’s own report'}</button>}
      {raw && <pre className="mt-1 max-h-72 overflow-auto whitespace-pre-wrap rounded border bg-bg p-2 font-mono text-[10px]">{d.raw}</pre>}
    </div>
  )
}

// What DBCanvas itself runs against a node: the live view's probes, this panel's
// snapshot and the provisioner's account setup. Hidden by default so the list
// shows the workload, not the observer.
const OWN_PROBE = /JSON_OBJECT|JSON_ARRAYAGG|json_build_object|json_agg|trx_max|wsrep|replication_group_members|replication_connection_configuration|@@GLOBAL ?\. ?`?read_only|@@`?version_comment|^SHOW (GLOBAL STATUS|REPLICA STATUS|SLAVE STATUS|ENGINE `?INNODB`? STATUS|MASTER STATUS|BINARY LOG STATUS)|AS `dumps`|SYSTEM_USER|pg_stat_replication|pg_is_in_recovery|pg_stat_wal_receiver/i

function Statements({ s }) {
  const [own, setOwn] = useState(false)
  const all = [...(s.digests || [])].sort((x, y) => (y.timePerSec ?? -1) - (x.timePerSec ?? -1) || y.timeSec - x.timeSec)
  if (!all.length) return <Empty text="No statement statistics on this node." />
  const rows = own ? all : all.filter((x) => !OWN_PROBE.test(x.text || ''))
  const hidden = all.length - rows.length
  const th = 'px-2 py-1 text-left font-semibold'
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-3 text-xs text-muted">
        <p className="flex-1">Normalised statements by the time they take, per second since the previous snapshot (the totals since the server started in the tooltip).</p>
        <label className="flex shrink-0 items-center gap-1"><input type="checkbox" checked={own} onChange={(e) => setOwn(e.target.checked)} />show DBCanvas's own queries{!own && hidden > 0 ? ` (${hidden} hidden)` : ''}</label>
      </div>
      {!rows.length && <Empty text="Nothing but DBCanvas's own monitoring queries so far." />}
      <div className="overflow-hidden rounded-lg border bg-surface">
        <table className="w-full border-collapse text-[11px]">
          <thead><tr className="bg-surface2 text-muted"><th className={th}>STATEMENT</th><th className={th}>CALLS/S</th><th className={th}>AVG</th><th className={th}>TIME/S</th><th className={th}>ROWS EXAMINED / CALL</th><th className={th}>NO INDEX</th></tr></thead>
          <tbody>
            {rows.map((x) => (
              <tr key={x.id} className="border-t align-top" title={`since the start: ${fmtNum(x.calls)} calls, ${fmtDur(x.timeSec)} in total, ${fmtNum(x.rowsExamined)} rows examined`}>
                <td className="max-w-[520px] px-2 py-1 font-mono"><div className="line-clamp-2">{oneLine(x.text)}</div>{x.schema && <span className="text-muted">{x.schema}</span>}</td>
                <td className="px-2 py-1 font-mono">{x.callsPerSec == null ? '…' : x.callsPerSec.toFixed(1)}</td>
                <td className="px-2 py-1 font-mono">{x.avgMs == null ? '—' : `${x.avgMs.toFixed(1)}ms`}</td>
                <td className="px-2 py-1 font-mono">{x.timePerSec == null ? '…' : `${(x.timePerSec * 1000).toFixed(0)}ms`}</td>
                <td className="px-2 py-1 font-mono">{x.exaPerCall == null ? '—' : fmtNum(x.exaPerCall)}</td>
                <td className="px-2 py-1">{x.noIndex > 0 ? <span style={{ color: 'var(--warning)' }}>{fmtNum(x.noIndex)}×</span> : ''}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

function Pools({ s }) {
  const pools = s.proxy?.pools || []
  if (!pools.length) return <Empty text="No pool figures." />
  const th = 'px-2 py-1 text-left font-semibold'
  return (
    <div className="overflow-hidden rounded-lg border bg-surface">
      <table className="w-full border-collapse text-[11px]">
        <thead><tr className="bg-surface2 text-muted"><th className={th}>POOL</th><th className={th}>BACKEND</th><th className={th}>STATUS</th><th className={th}>IN USE</th><th className={th}>FREE</th><th className={th}>WAITING</th><th className={th}>ERRORS</th><th className={th}>QUERIES</th><th className={th}>LATENCY</th></tr></thead>
        <tbody>
          {pools.map((p, i) => (
            <tr key={i} className={`border-t ${p.waiting > 0 ? 'bg-danger/5' : ''}`}>
              <td className="px-2 py-1">{p.name}</td><td className="px-2 py-1 font-mono">{p.backend || '—'}</td><td className="px-2 py-1">{p.status || '—'}</td>
              <td className="px-2 py-1 font-mono">{fmtNum(p.used)}</td><td className="px-2 py-1 font-mono">{fmtNum(p.free)}</td>
              <td className="px-2 py-1 font-mono" style={p.waiting > 0 ? { color: 'var(--danger)', fontWeight: 600 } : undefined}>{fmtNum(p.waiting)}{p.maxWaitSec ? ` (max ${fmtDur(p.maxWaitSec)})` : ''}</td>
              <td className="px-2 py-1 font-mono">{p.errors ? fmtNum(p.errors) : ''}</td><td className="px-2 py-1 font-mono">{p.queries ? fmtNum(p.queries) : ''}</td>
              <td className="px-2 py-1 font-mono">{p.latencyMs ? `${p.latencyMs.toFixed(1)}ms` : ''}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// DeepControl switches deep instrumentation on for a while, shows the time left and what it costs
// (QPS before against now), and off.
function DeepControl({ stackId, nid, deep, onChange }) {
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [, tick] = useState(0)
  useEffect(() => { const t = setInterval(() => tick((n) => n + 1), 1000); return () => clearInterval(t) }, [])
  const set = async (minutes) => {
    setBusy(true); setErr('')
    try { await stackApi.activityDeep(stackId, nid, minutes); setOpen(false); onChange?.() } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }
  const left = deep ? Math.max(0, (deep.untilMs - Date.now()) / 1000) : 0
  return (
    <span className="relative">
      {deep ? (
        <span className="inline-flex items-center gap-1 rounded px-2 py-0.5 text-xs font-semibold" style={toneStyle('warning')}
          title={`Switched on: ${(deep.enabled || []).join(', ')}${deep.qpsBefore != null ? ` · QPS before: ${deep.qpsBefore.toFixed(1)}` : ''}`}>
          Deep instrumentation · {fmtDur(left)} left
          <button type="button" className="ml-1 underline" onClick={() => setOpen((o) => !o)}>change</button>
        </span>
      ) : (
        <Button size="sm" variant="ghost" onClick={() => setOpen((o) => !o)} title="Switch on the performance_schema events that cost something: DDL progress, what sessions wait on, socket I/O. Off again by itself.">Deep: off</Button>
      )}
      {open && (
        <div className="absolute right-0 top-full z-10 mt-1 w-72 space-y-2 rounded-lg border bg-surface p-3 text-xs shadow-xl">
          <p className="text-muted">Switches on the performance_schema stage, wait and transaction events that are off — DDL progress, what each session waits on, its socket I/O. They cost a busy server some throughput, so they switch back off by themselves.</p>
          <div className="flex flex-wrap gap-1">
            {[5, 15, 30, 60].map((m) => <Button key={m} size="sm" variant="outline" disabled={busy} onClick={() => set(m)}>{deep ? `${m} min from now` : `${m} min`}</Button>)}
          </div>
          {deep && <Button size="sm" variant="danger" disabled={busy} onClick={() => set(0)}>Switch off now</Button>}
          {err && <div className="text-danger">{err}</div>}
        </div>
      )}
    </span>
  )
}

function Empty({ text }) {
  return <div className="rounded-xl border bg-surface p-6 text-center text-sm text-muted">{text}</div>
}
