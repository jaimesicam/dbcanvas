import { useEffect, useMemo, useRef, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Card, Button, Badge, Field, inputCls } from '../components/ui.jsx'
import { queryrunApi, targetKey } from '../lib/queryrunApi.js'

// Query Runner — define one or more queries, each pointed at a canvas-provisioned DB
// node (picked from a dropdown), with per-query load parameters (count / threads /
// time limit) and an optional processlist "run condition" gate. All queries start
// together and run in parallel. See docs/QUERY_RUNNER.md.

const CONDITIONS = [
  { v: 'no_match', label: 'No match running' },
  { v: 'match', label: 'A match IS running' },
]
const CHECKS = [
  { v: 'every', label: 'Every iteration' },
  { v: 'once', label: 'Once (gate start)' },
]

const newQuery = () => ({
  target: '',
  database: '',
  sql: 'SELECT 1',
  count: 1,
  threads: 1,
  timeLimitS: 60,
  gateOn: false,
  pattern: '',
  condition: 'no_match',
  check: 'every',
  pollMs: 1000,
})

export default function QueryRunner() {
  const [targets, setTargets] = useState(null)
  const [queries, setQueries] = useState([newQuery()])
  const [run, setRun] = useState(null) // live run snapshot
  const [runId, setRunId] = useState(null)
  const [history, setHistory] = useState([])
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    queryrunApi.targets().then((t) => setTargets(Array.isArray(t) ? t : [])).catch((e) => {
      setErr(`Could not load targets: ${e.message}. If you just updated, rebuild & restart the backend.`)
      setTargets([])
    })
    refreshHistory()
  }, [])

  const refreshHistory = () => queryrunApi.history().then((h) => setHistory(Array.isArray(h) ? h : [])).catch(() => {})

  const running = run?.status === 'running'
  const pollRef = useRef(null)
  useEffect(() => {
    if (!runId || !running) return
    pollRef.current = setInterval(async () => {
      try { setRun(await queryrunApi.status(runId)) } catch { /* keep last */ }
    }, 800)
    return () => clearInterval(pollRef.current)
  }, [runId, running])

  // When a run finishes, refresh History.
  useEffect(() => { if (run && run.status !== 'running') refreshHistory() }, [run?.status])

  const update = (i, patch) => setQueries((qs) => qs.map((q, j) => (j === i ? { ...q, ...patch } : q)))
  const addQuery = () => setQueries((qs) => [...qs, newQuery()])
  const removeQuery = (i) => setQueries((qs) => (qs.length === 1 ? qs : qs.filter((_, j) => j !== i)))

  async function doRun() {
    setErr(''); setBusy(true)
    try {
      const payload = queries.map((q) => {
        const [stackId, nodeId] = q.target.split(':')
        return {
          stackId: Number(stackId), nodeId, database: q.database, sql: q.sql,
          count: Number(q.count), threads: Number(q.threads), timeLimitS: Number(q.timeLimitS),
          gate: q.gateOn
            ? { enabled: true, pattern: q.pattern, condition: q.condition, check: q.check, pollMs: Number(q.pollMs) }
            : { enabled: false },
        }
      })
      if (payload.some((p) => !p.stackId || !p.nodeId)) throw new Error('every query needs a target server')
      const { runId } = await queryrunApi.start(payload)
      setRunId(runId)
      setRun({ id: runId, status: 'running', queries: [] })
    } catch (e) { setErr(e.message) }
    finally { setBusy(false) }
  }

  async function doStop() {
    if (!runId) return
    try { setRun(await queryrunApi.stop(runId)) } catch (e) { setErr(e.message) }
  }

  const liveFor = (i) => run?.queries?.find((q) => q.index === i)
  const nQueries = queries.length

  return (
    <div className="space-y-4">
      {err && (
        <div className="flex items-center gap-2 rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          <Icon.Bell size={16} /> {err}
        </div>
      )}

      <Card
        title="New query run"
        subtitle="Each query starts together and runs in parallel — point each at a different provisioned server. Each query's gate watches only its own target's processlist."
        action={<Badge tone="muted">{nQueries} {nQueries === 1 ? 'query' : 'queries'}</Badge>}
      >
        <div className="space-y-4">
          {queries.map((q, i) => (
            <QueryCard
              key={i} idx={i} q={q} targets={targets || []} live={liveFor(i)} canRemove={nQueries > 1}
              onChange={(patch) => update(i, patch)} onRemove={() => removeQuery(i)}
            />
          ))}
        </div>

        <div className="mt-4 flex items-center gap-2">
          {running ? (
            <Button variant="danger" onClick={doStop}>Stop</Button>
          ) : (
            <Button variant="primary" onClick={doRun} disabled={busy || !targets?.length}>
              Run {nQueries} {nQueries === 1 ? 'query' : 'queries'}
            </Button>
          )}
          <Button variant="subtle" onClick={addQuery} disabled={running}>
            <Icon.Plus size={16} /> Add another query
          </Button>
          {!targets?.length && targets !== null && (
            <span className="text-xs text-muted">No running MySQL/PXC or PostgreSQL nodes — deploy one first.</span>
          )}
        </div>
      </Card>

      <Card title="History" subtitle="Recent runs (this session)">
        {history.length === 0 ? (
          <div className="py-6 text-center text-sm text-muted">No runs yet.</div>
        ) : (
          <div className="space-y-2">
            {history.map((h) => <HistoryRow key={h.id} run={h} />)}
          </div>
        )}
      </Card>
    </div>
  )
}

function QueryCard({ idx, q, targets, live, canRemove, onChange, onRemove }) {
  return (
    <div className="rounded-lg border bg-bg p-4">
      <div className="mb-3 flex items-center justify-between">
        <div className="flex items-center gap-2">
          <span className="text-sm font-semibold">Query {idx + 1}</span>
          {live && <StatusBadge status={live.status} />}
        </div>
        {canRemove && (
          <button onClick={onRemove} className="text-xs text-danger hover:underline">Remove</button>
        )}
      </div>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <Field label="Server">
          <select value={q.target} onChange={(e) => onChange({ target: e.target.value })} className={inputCls}>
            <option value="">Select a provisioned server…</option>
            {targets.map((t) => (
              <option key={targetKey(t)} value={targetKey(t)}>
                {t.label} · {t.stackName} ({t.engine})
              </option>
            ))}
          </select>
        </Field>
        <Field label="Database" hint="Optional — default schema/db">
          <input value={q.database} onChange={(e) => onChange({ database: e.target.value })} className={inputCls} placeholder="(default)" />
        </Field>
      </div>

      <div className="mt-3">
        <Field label="Query">
          <textarea value={q.sql} onChange={(e) => onChange({ sql: e.target.value })} rows={4}
            className={`${inputCls} font-mono`} spellCheck={false} />
        </Field>
      </div>

      <div className="mt-3 grid grid-cols-3 gap-3">
        <Field label="Count (0=∞)"><input type="number" min="0" value={q.count} onChange={(e) => onChange({ count: e.target.value })} className={inputCls} /></Field>
        <Field label="Threads"><input type="number" min="1" max="64" value={q.threads} onChange={(e) => onChange({ threads: e.target.value })} className={inputCls} /></Field>
        <Field label="Time limit (s)"><input type="number" min="1" max="3600" value={q.timeLimitS} onChange={(e) => onChange({ timeLimitS: e.target.value })} className={inputCls} /></Field>
      </div>

      <label className="mt-4 flex items-center gap-2 text-sm">
        <input type="checkbox" checked={q.gateOn} onChange={(e) => onChange({ gateOn: e.target.checked })} />
        <span className="font-medium">Run condition</span>
        <span className="text-xs text-muted">(watch this target's processlist before firing)</span>
      </label>

      {q.gateOn && (
        <div className="mt-3 space-y-3">
          <Field label="Pattern (regex)" hint="Go RE2 · matched against active statements">
            <input value={q.pattern} onChange={(e) => onChange({ pattern: e.target.value })} className={`${inputCls} font-mono`} placeholder="e.g. ALTER TABLE\s+orders" />
          </Field>
          <div className="grid grid-cols-3 gap-3">
            <Field label="Condition">
              <select value={q.condition} onChange={(e) => onChange({ condition: e.target.value })} className={inputCls}>
                {CONDITIONS.map((c) => <option key={c.v} value={c.v}>{c.label}</option>)}
              </select>
            </Field>
            <Field label="Check">
              <select value={q.check} onChange={(e) => onChange({ check: e.target.value })} className={inputCls}>
                {CHECKS.map((c) => <option key={c.v} value={c.v}>{c.label}</option>)}
              </select>
            </Field>
            <Field label="Poll (ms)"><input type="number" min="100" value={q.pollMs} onChange={(e) => onChange({ pollMs: e.target.value })} className={inputCls} /></Field>
          </div>
        </div>
      )}

      {live && <LiveStats live={live} />}
    </div>
  )
}

function LiveStats({ live }) {
  return (
    <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 border-t pt-3 text-xs text-muted">
      <span><span className="font-semibold text-fg">{live.executed}</span> executed</span>
      <span className={live.errors ? 'text-danger' : ''}><span className="font-semibold">{live.errors}</span> errors</span>
      <span>avg <span className="font-semibold text-fg">{live.latAvgMs}</span> ms</span>
      <span>p95 <span className="font-semibold text-fg">{live.latP95Ms}</span> ms</span>
      <span>max {live.latMaxMs} ms</span>
      {live.gated && (
        <Badge tone={live.gateOpen ? 'primary' : 'warning'}>{live.gateOpen ? 'gate open' : 'gated'} · {live.gateWaits} waits</Badge>
      )}
      {live.lastError && <span className="text-danger" title={live.lastError}>⚠ {live.lastError.slice(0, 80)}</span>}
    </div>
  )
}

function StatusBadge({ status }) {
  const tone = { running: 'primary', done: 'muted', error: 'danger', stopped: 'warning', pending: 'muted' }[status] || 'muted'
  return <Badge tone={tone}>{status}</Badge>
}

function HistoryRow({ run }) {
  const totals = (run.queries || []).reduce((a, q) => ({ executed: a.executed + q.executed, errors: a.errors + q.errors }), { executed: 0, errors: 0 })
  return (
    <div className="flex items-center justify-between rounded-lg border bg-bg px-3 py-2 text-sm">
      <div className="flex items-center gap-2">
        <StatusBadge status={run.status} />
        <span className="text-muted">{(run.queries || []).length} queries</span>
      </div>
      <div className="flex items-center gap-4 text-xs text-muted">
        <span><span className="font-semibold text-fg">{totals.executed}</span> executed</span>
        <span className={totals.errors ? 'text-danger' : ''}>{totals.errors} errors</span>
        <span>{new Date(run.start).toLocaleTimeString()}</span>
      </div>
    </div>
  )
}
