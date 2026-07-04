import { useEffect, useRef, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Card, Button, Badge, Field, inputCls } from '../components/ui.jsx'
import { benchmarkApi, benchTargetKey } from '../lib/benchmarkApi.js'

// Benchmark — load a purpose-built star schema into a chosen database and drive it with
// one of four workload profiles (OLTP / OLAP / read-write / read-only), reporting
// throughput + latency. See docs/BENCHMARK_PLAN.md and docs/BENCHMARK.md.

const WORKLOADS = [
  { v: 'oltp', label: 'OLTP', hint: 'Mixed short transactions (~70/30 read/write)' },
  { v: 'olap', label: 'OLAP', hint: 'Analytical aggregation queries' },
  { v: 'rw', label: 'Read-Write', hint: 'Write-heavy transactions' },
  { v: 'ro', label: 'Read-Only', hint: 'Point + range reads (replica-safe)' },
]

const defaults = {
  target: '', database: 'dbcanvas_bench', createDb: true, workload: 'oltp',
  scale: 1, threads: 8, durationS: 30, warmupS: 5, keepData: false, seed: 0,
}

export default function Benchmark() {
  const [targets, setTargets] = useState(null)
  const [cfg, setCfg] = useState(defaults)
  const [run, setRun] = useState(null)
  const [runId, setRunId] = useState(null)
  const [history, setHistory] = useState([])
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    benchmarkApi.targets().then((t) => setTargets(Array.isArray(t) ? t : [])).catch((e) => {
      setErr(`Could not load targets: ${e.message}. If you just updated, rebuild & restart the backend.`)
      setTargets([])
    })
    refreshHistory()
  }, [])

  const refreshHistory = () => benchmarkApi.history().then((h) => setHistory(Array.isArray(h) ? h : [])).catch(() => {})

  const active = run && !['done', 'error', 'stopped'].includes(run.status)
  const pollRef = useRef(null)
  useEffect(() => {
    if (!runId || !active) return
    pollRef.current = setInterval(async () => {
      try { setRun(await benchmarkApi.status(runId)) } catch { /* keep last */ }
    }, 800)
    return () => clearInterval(pollRef.current)
  }, [runId, active])
  useEffect(() => { if (run && !active) refreshHistory() }, [run?.status])

  const set = (patch) => setCfg((c) => ({ ...c, ...patch }))

  async function doRun() {
    setErr(''); setBusy(true)
    try {
      const [stackId, nodeId] = cfg.target.split(':')
      if (!stackId || !nodeId) throw new Error('pick a target server')
      const { runId } = await benchmarkApi.start({
        stackId: Number(stackId), nodeId, database: cfg.database, createDb: cfg.createDb,
        workload: cfg.workload, scale: Number(cfg.scale), threads: Number(cfg.threads),
        durationS: Number(cfg.durationS), warmupS: Number(cfg.warmupS),
        keepData: cfg.keepData, seed: Number(cfg.seed) || 0,
      })
      setRunId(runId)
      setRun({ id: runId, status: 'preparing', stmts: [] })
    } catch (e) { setErr(e.message) }
    finally { setBusy(false) }
  }

  async function doStop() {
    if (!runId) return
    try { setRun(await benchmarkApi.stop(runId)) } catch (e) { setErr(e.message) }
  }

  return (
    <div className="space-y-4">
      {err && (
        <div className="flex items-center gap-2 rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          <Icon.Bell size={16} /> {err}
        </div>
      )}

      <Card title="New benchmark" subtitle="Loads a bench_* star schema into the chosen database, then drives it with the selected workload.">
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <Field label="Server">
            <select value={cfg.target} onChange={(e) => set({ target: e.target.value })} className={inputCls}>
              <option value="">Select a provisioned server…</option>
              {(targets || []).map((t) => (
                <option key={benchTargetKey(t)} value={benchTargetKey(t)}>{t.label} · {t.stackName} ({t.engine})</option>
              ))}
            </select>
          </Field>
          <Field label="Database" hint="bench_* tables live here">
            <div className="flex items-center gap-2">
              <input value={cfg.database} onChange={(e) => set({ database: e.target.value })} className={inputCls} />
              <label className="flex shrink-0 items-center gap-1 text-xs text-muted">
                <input type="checkbox" checked={cfg.createDb} onChange={(e) => set({ createDb: e.target.checked })} /> create
              </label>
            </div>
          </Field>
        </div>

        <div className="mt-3">
          <div className="mb-1 text-xs font-medium text-muted">Workload</div>
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
            {WORKLOADS.map((wl) => (
              <button key={wl.v} onClick={() => set({ workload: wl.v })} title={wl.hint}
                className={`rounded-lg border px-3 py-2 text-left text-sm ${cfg.workload === wl.v ? 'border-primary bg-primary/10 text-primary' : 'hover:bg-surface2'}`}>
                <div className="font-medium">{wl.label}</div>
                <div className="text-[11px] text-muted">{wl.hint}</div>
              </button>
            ))}
          </div>
        </div>

        <div className="mt-3 grid grid-cols-2 gap-3 sm:grid-cols-4">
          <Field label="Scale" hint="×½M rows"><input type="number" min="1" max="50" value={cfg.scale} onChange={(e) => set({ scale: e.target.value })} className={inputCls} /></Field>
          <Field label="Threads"><input type="number" min="1" max="128" value={cfg.threads} onChange={(e) => set({ threads: e.target.value })} className={inputCls} /></Field>
          <Field label="Duration (s)"><input type="number" min="1" max="3600" value={cfg.durationS} onChange={(e) => set({ durationS: e.target.value })} className={inputCls} /></Field>
          <Field label="Warmup (s)"><input type="number" min="0" max="600" value={cfg.warmupS} onChange={(e) => set({ warmupS: e.target.value })} className={inputCls} /></Field>
        </div>

        <div className="mt-3 flex flex-wrap items-center gap-4">
          <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={cfg.keepData} onChange={(e) => set({ keepData: e.target.checked })} /> Keep data after run</label>
          <Field label="Seed (0 = random)"><input type="number" value={cfg.seed} onChange={(e) => set({ seed: e.target.value })} className={`${inputCls} w-40`} /></Field>
        </div>

        <div className="mt-4 flex items-center gap-2">
          {active ? (
            <Button variant="danger" onClick={doStop}>Stop</Button>
          ) : (
            <Button variant="primary" onClick={doRun} disabled={busy || !targets?.length}>Run benchmark</Button>
          )}
          {!targets?.length && targets !== null && <span className="text-xs text-muted">No running MySQL/PXC or PostgreSQL nodes — deploy one first.</span>}
        </div>
      </Card>

      {run && <Results run={run} />}

      <Card title="History" subtitle="Recent runs (this session)">
        {history.length === 0 ? (
          <div className="py-6 text-center text-sm text-muted">No runs yet.</div>
        ) : (
          <div className="space-y-2">{history.map((h) => <HistoryRow key={h.id} run={h} />)}</div>
        )}
      </Card>
    </div>
  )
}

function StatusBadge({ status }) {
  const tone = { running: 'primary', done: 'success', error: 'danger', stopped: 'warning', preparing: 'muted', loading: 'muted', warmup: 'muted' }[status] || 'muted'
  return <Badge tone={tone}>{status}</Badge>
}

function primaryMetric(run) {
  return run.workload === 'ro' || run.workload === 'olap'
    ? { label: 'QPS', value: run.qps }
    : { label: 'TPS', value: run.tps }
}

function Results({ run }) {
  const pm = primaryMetric(run)
  const loading = run.status === 'loading' || run.status === 'preparing'
  const pct = run.rowsTarget ? Math.min(100, Math.round((run.rowsLoaded / run.rowsTarget) * 100)) : 0
  return (
    <Card title="Result" action={<StatusBadge status={run.status} />}>
      {run.message && <div className="mb-3 text-sm text-muted">{run.message}</div>}
      {loading && (
        <div className="mb-3">
          <div className="mb-1 flex justify-between text-xs text-muted"><span>Loading data</span><span>{run.rowsLoaded?.toLocaleString?.() || run.rowsLoaded} rows</span></div>
          <div className="h-2 overflow-hidden rounded bg-surface2"><div className="h-full bg-primary transition-all" style={{ width: `${pct}%` }} /></div>
        </div>
      )}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Metric label={pm.label} value={pm.value} big />
        <Metric label="Txns" value={run.txns} />
        <Metric label="Queries" value={run.queries} />
        <Metric label="Elapsed (s)" value={run.elapsedS} />
        <Metric label="Txn p50 (ms)" value={run.txnP50Ms} />
        <Metric label="Txn p95 (ms)" value={run.txnP95Ms} />
        <Metric label="Txn p99 (ms)" value={run.txnP99Ms} />
        <Metric label="Other QPS/TPS" value={pm.label === 'TPS' ? `${run.qps} qps` : `${run.tps} tps`} />
      </div>

      {run.stmts?.length > 0 && (
        <div className="mt-4 overflow-x-auto">
          <table className="w-full text-left text-sm">
            <thead className="text-xs text-muted">
              <tr><th className="py-1 pr-4">Statement</th><th className="pr-4">Count</th><th className="pr-4">Errors</th><th className="pr-4">Avg ms</th><th className="pr-4">p95 ms</th><th className="pr-4">p99 ms</th></tr>
            </thead>
            <tbody>
              {run.stmts.map((s) => (
                <tr key={s.type} className="border-t">
                  <td className="py-1 pr-4 font-mono text-xs">{s.type}</td>
                  <td className="pr-4">{s.count}</td>
                  <td className={`pr-4 ${s.errors ? 'text-danger' : ''}`}>{s.errors}</td>
                  <td className="pr-4">{s.avgMs}</td>
                  <td className="pr-4">{s.p95Ms}</td>
                  <td className="pr-4">{s.p99Ms}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  )
}

function Metric({ label, value, big }) {
  return (
    <div className="rounded-lg border bg-bg px-3 py-2">
      <div className="text-[11px] text-muted">{label}</div>
      <div className={big ? 'text-2xl font-semibold text-primary' : 'text-lg font-semibold'}>{value ?? 0}</div>
    </div>
  )
}

function HistoryRow({ run }) {
  const pm = primaryMetric(run)
  return (
    <div className="flex items-center justify-between rounded-lg border bg-bg px-3 py-2 text-sm">
      <div className="flex items-center gap-2">
        <StatusBadge status={run.status} />
        <span className="font-medium">{run.workload.toUpperCase()}</span>
        <span className="text-muted">{run.label} · {run.engine} · scale {run.scale} · {run.threads}t</span>
      </div>
      <div className="flex items-center gap-4 text-xs text-muted">
        <span><span className="font-semibold text-fg">{pm.value ?? 0}</span> {pm.label.toLowerCase()}</span>
        <span>p95 {run.txnP95Ms} ms</span>
        <span>{new Date(run.start).toLocaleTimeString()}</span>
      </div>
    </div>
  )
}
