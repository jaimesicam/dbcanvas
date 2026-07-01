import { useEffect, useMemo, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Card, Button, Badge, Field, inputCls } from '../components/ui.jsx'
import { datagenApi, genLabel } from '../lib/datagenApi.js'

// Data Generator — pick a running PostgreSQL connection provisioned by Database Stacks,
// browse to a table, configure a generator per column (smart-inferred), then preview and
// generate FK-aware test data with a live progress readout.

export default function DataGenerator() {
  const [conns, setConns] = useState(null)
  const [conn, setConn] = useState(null)
  const [dbs, setDbs] = useState([])
  const [db, setDb] = useState('')
  const [tables, setTables] = useState([])
  const [tableFilter, setTableFilter] = useState('')
  const [sel, setSel] = useState(null) // {schema, table}
  const [meta, setMeta] = useState(null)
  const [cfg, setCfg] = useState({}) // column name -> {generator, skip, options}
  const [opts, setOpts] = useState({ rows: 1000, batch: 1000, threads: 4, seed: 0, stopOnError: true, fkSampleSize: 500 })
  const [preview, setPreview] = useState(null)
  const [job, setJob] = useState(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  useEffect(() => {
    datagenApi
      .connections()
      .then((d) => setConns(Array.isArray(d) ? d : []))
      .catch((e) => {
        setErr(`Could not load connections: ${e.message}. If you just updated, rebuild & restart the backend.`)
        setConns([])
      })
  }, [])

  async function pickConn(c) {
    setConn(c); setDb(''); setTables([]); setSel(null); setMeta(null); setPreview(null); setJob(null); setErr('')
    try {
      const list = await datagenApi.databases(c.stackId, c.nodeId)
      setDbs(list || [])
    } catch (e) { setErr(e.message) }
  }

  async function pickDb(name) {
    setDb(name); setSel(null); setMeta(null); setPreview(null); setErr('')
    try { setTables((await datagenApi.tables(conn.stackId, conn.nodeId, name)) || []) }
    catch (e) { setErr(e.message) }
  }

  async function pickTable(t) {
    setSel(t); setMeta(null); setPreview(null); setJob(null); setErr('')
    try {
      const m = await datagenApi.columns(conn.stackId, conn.nodeId, db, t.schema, t.table)
      setMeta(m)
      const c0 = {}
      for (const col of m.columns) c0[col.name] = { generator: col.generator, skip: false, options: {} }
      setCfg(c0)
    } catch (e) { setErr(e.message) }
  }

  const buildCfg = () => ({
    database: db, schema: sel.schema, table: sel.table,
    rows: Number(opts.rows), batch: Number(opts.batch), threads: Number(opts.threads),
    seed: Number(opts.seed), stopOnError: opts.stopOnError, fkSampleSize: Number(opts.fkSampleSize),
    columns: meta.columns.map((c) => ({
      name: c.name, generator: cfg[c.name]?.generator || c.generator,
      skip: !!cfg[c.name]?.skip, options: cfg[c.name]?.options || {},
    })),
  })

  async function doPreview() {
    setBusy(true); setErr(''); setPreview(null)
    try { setPreview(await datagenApi.preview(conn.stackId, conn.nodeId, buildCfg())) }
    catch (e) { setErr(e.message) }
    finally { setBusy(false) }
  }

  async function doGenerate() {
    setBusy(true); setErr('')
    try {
      const { jobId } = await datagenApi.generate(conn.stackId, conn.nodeId, buildCfg())
      setJob({ id: jobId, status: 'running', inserted: 0, total: Number(opts.rows) })
    } catch (e) { setErr(e.message) }
    finally { setBusy(false) }
  }

  // Poll job progress while running.
  const jobId = job?.id
  const running = job?.status === 'running'
  useEffect(() => {
    if (!jobId || !running) return
    const t = setInterval(async () => {
      try { setJob(await datagenApi.job(jobId)) } catch { /* keep last */ }
    }, 800)
    return () => clearInterval(t)
  }, [jobId, running])

  const setGen = (name, generator) => setCfg((p) => ({ ...p, [name]: { ...p[name], generator } }))
  const setSkip = (name, skip) => setCfg((p) => ({ ...p, [name]: { ...p[name], skip } }))
  const setOpt = (name, k, v) => setCfg((p) => ({ ...p, [name]: { ...p[name], options: { ...(p[name]?.options || {}), [k]: v } } }))

  const filteredTables = useMemo(
    () => tables.filter((t) => `${t.schema}.${t.table}`.toLowerCase().includes(tableFilter.toLowerCase())),
    [tables, tableFilter],
  )
  const insertCols = meta?.columns.filter((c) => {
    const g = cfg[c.name]?.generator || c.generator
    return !cfg[c.name]?.skip && g !== 'skip' && g !== 'default' && !c.isGenerated && !c.isIdentity
  }) || []
  const fkCols = meta?.columns.filter((c) => c.fk) || []

  return (
    <div className="space-y-4">
      {err && (
        <div className="flex items-center gap-2 rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          <Icon.Bell size={16} /> {err}
        </div>
      )}

      {/* Step 1 — connection */}
      <Card title="1 · Connection" subtitle="Running PostgreSQL nodes from your Database Stacks">
        {conns === null ? (
          <p className="text-sm text-muted">Loading connections…</p>
        ) : conns.length === 0 ? (
          <p className="text-sm text-muted">No running PostgreSQL nodes. Deploy a stack with a Postgres / Patroni / repmgr node first.</p>
        ) : (
          <div className="flex flex-wrap gap-2">
            {conns.map((c) => {
              const on = conn && conn.stackId === c.stackId && conn.nodeId === c.nodeId
              return (
                <button key={`${c.stackId}:${c.nodeId}`} onClick={() => pickConn(c)}
                  className={`rounded-lg border px-3 py-2 text-left text-sm transition ${on ? 'border-primary bg-primary/10' : 'hover:bg-surface2'}`}>
                  <div className="font-medium">{c.label || c.nodeId}</div>
                  <div className="text-xs text-muted">{c.stackName} · {c.type}</div>
                </button>
              )
            })}
          </div>
        )}
      </Card>

      {/* Step 2 — database + table */}
      {conn && (
        <Card title="2 · Database & table" subtitle="Browse databases, then pick a target table">
          <div className="mb-3 flex flex-wrap gap-2">
            {dbs.map((d) => (
              <button key={d} onClick={() => pickDb(d)}
                className={`rounded-md border px-2.5 py-1 text-sm ${db === d ? 'border-primary bg-primary/10' : 'hover:bg-surface2'}`}>
                {d}
              </button>
            ))}
          </div>
          {db && (
            <>
              <input value={tableFilter} onChange={(e) => setTableFilter(e.target.value)} placeholder="Filter tables…" className={`${inputCls} mb-2`} />
              <div className="max-h-56 overflow-auto rounded-lg border">
                {filteredTables.length === 0 && <div className="px-3 py-4 text-center text-sm text-muted">No tables</div>}
                {filteredTables.map((t) => {
                  const on = sel && sel.schema === t.schema && sel.table === t.table
                  return (
                    <button key={`${t.schema}.${t.table}`} onClick={() => pickTable(t)}
                      className={`flex w-full items-center justify-between px-3 py-1.5 text-sm ${on ? 'bg-primary/10 text-primary' : 'hover:bg-surface2'}`}>
                      <span><span className="text-muted">{t.schema}.</span>{t.table}</span>
                      <span className="text-xs text-muted">~{t.estRows.toLocaleString()} rows</span>
                    </button>
                  )
                })}
              </div>
            </>
          )}
        </Card>
      )}

      {/* Step 3 — column template */}
      {meta && (
        <Card
          title="3 · Column template"
          subtitle={`${meta.schema}.${meta.table}`}
          action={meta.isHypertable ? <Badge tone="primary">TimescaleDB hypertable · {meta.timeColumn}</Badge> : null}
        >
          <div className="overflow-auto rounded-lg border">
            <table className="w-full text-sm">
              <thead className="bg-surface2 text-xs text-muted">
                <tr>
                  <th className="px-3 py-2 text-left">Column</th>
                  <th className="px-3 py-2 text-left">Type</th>
                  <th className="px-3 py-2 text-left">Flags</th>
                  <th className="px-3 py-2 text-left">Generator</th>
                  <th className="px-3 py-2 text-left">Options</th>
                </tr>
              </thead>
              <tbody>
                {meta.columns.map((c) => {
                  const g = cfg[c.name]?.generator || c.generator
                  const skip = !!cfg[c.name]?.skip
                  return (
                    <tr key={c.name} className={`border-t ${skip ? 'opacity-40' : ''}`}>
                      <td className="px-3 py-1.5 font-medium">{c.name}</td>
                      <td className="px-3 py-1.5 text-muted">{c.dataType}{c.vectorDim > 0 ? ` (${c.vectorDim})` : ''}</td>
                      <td className="px-3 py-1.5">
                        <div className="flex flex-wrap gap-1">
                          {c.isPrimaryKey && <Badge tone="primary">PK</Badge>}
                          {c.fk && <Badge tone="warning">FK→{c.fk.table}</Badge>}
                          {c.isIdentity && <Badge tone="muted">identity</Badge>}
                          {c.isGenerated && <Badge tone="muted">generated</Badge>}
                          {!c.nullable && <Badge tone="muted">NOT NULL</Badge>}
                        </div>
                      </td>
                      <td className="px-3 py-1.5">
                        <select value={g} onChange={(e) => setGen(c.name, e.target.value)} className={`${inputCls} py-1`}>
                          {c.generators.map((o) => <option key={o} value={o}>{genLabel(o)}</option>)}
                        </select>
                      </td>
                      <td className="px-3 py-1.5">
                        <div className="flex items-center gap-2">
                          <ColumnOptions col={c} gen={g} cfg={cfg[c.name]} setOpt={(k, v) => setOpt(c.name, k, v)} />
                          <label className="flex items-center gap-1 text-xs text-muted">
                            <input type="checkbox" checked={skip} onChange={(e) => setSkip(c.name, e.target.checked)} /> skip
                          </label>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        </Card>
      )}

      {/* Step 4 — options + run */}
      {meta && (
        <Card title="4 · Generate" subtitle="Rows, workers, and batching">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
            <Field label="Total rows"><input type="number" min="1" value={opts.rows} onChange={(e) => setOpts({ ...opts, rows: e.target.value })} className={inputCls} /></Field>
            <Field label="Batch size"><input type="number" min="1" value={opts.batch} onChange={(e) => setOpts({ ...opts, batch: e.target.value })} className={inputCls} /></Field>
            <Field label="Workers"><input type="number" min="1" max="16" value={opts.threads} onChange={(e) => setOpts({ ...opts, threads: e.target.value })} className={inputCls} /></Field>
            <Field label="FK sample"><input type="number" min="1" value={opts.fkSampleSize} onChange={(e) => setOpts({ ...opts, fkSampleSize: e.target.value })} className={inputCls} /></Field>
            <Field label="Seed (0=random)"><input type="number" value={opts.seed} onChange={(e) => setOpts({ ...opts, seed: e.target.value })} className={inputCls} /></Field>
            <Field label="On error">
              <label className="flex h-9 items-center gap-2 text-sm"><input type="checkbox" checked={opts.stopOnError} onChange={(e) => setOpts({ ...opts, stopOnError: e.target.checked })} /> stop</label>
            </Field>
          </div>

          <div className="mt-3 flex flex-wrap items-center gap-2 rounded-lg border bg-surface2/50 px-3 py-2 text-xs text-muted">
            <span>Inserting into <b className="text-fg">{meta.schema}.{meta.table}</b></span>
            <span>· {insertCols.length} column(s)</span>
            <span>· {meta.columns.length - insertCols.length} skipped/DB-managed</span>
            {fkCols.length > 0 && <span>· <Icon.Bell size={12} className="inline" /> {fkCols.length} FK column(s) sampled</span>}
          </div>

          <div className="mt-3 flex gap-2">
            <Button variant="subtle" onClick={doPreview} disabled={busy}><Icon.Monitor size={16} /> Preview</Button>
            <Button onClick={doGenerate} disabled={busy || running}><Icon.Arrow size={16} /> Generate</Button>
          </div>

          {preview && (
            <div className="mt-3 overflow-auto rounded-lg border">
              <table className="w-full text-xs">
                <thead className="bg-surface2 text-muted">
                  <tr>{preview.columns.map((c) => <th key={c} className="px-2 py-1 text-left">{c}</th>)}</tr>
                </thead>
                <tbody>
                  {preview.rows.map((row, i) => (
                    <tr key={i} className="border-t">
                      {preview.columns.map((c) => <td key={c} className="px-2 py-1 font-mono">{String(row[c])}</td>)}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {job && <JobProgress job={job} onCancel={() => datagenApi.cancel(job.id).catch(() => {})} />}
        </Card>
      )}
    </div>
  )
}

function ColumnOptions({ col, gen, cfg, setOpt }) {
  const o = cfg?.options || {}
  const num = (k, ph) => (
    <input type="number" placeholder={ph} value={o[k] ?? ''} onChange={(e) => setOpt(k, e.target.value === '' ? undefined : Number(e.target.value))}
      className={`${inputCls} w-20 py-1`} />
  )
  const fields = []
  if (col.nullable && gen !== 'default' && gen !== 'skip') fields.push(<span key="np" className="flex items-center gap-1 text-xs">null%{num('nullPct', '0')}</span>)
  if (gen === 'randint' || gen === 'decimal' || gen === 'ts_metric') fields.push(<span key="mm" className="flex items-center gap-1 text-xs">{num('min', 'min')}{num('max', 'max')}</span>)
  if (gen === 'pgvector') fields.push(<span key="dim" className="flex items-center gap-1 text-xs">dim{num('dim', String(col.vectorDim || 3))}</span>)
  if (gen === 'constant') fields.push(<input key="cv" placeholder="value" value={o.value ?? ''} onChange={(e) => setOpt('value', e.target.value)} className={`${inputCls} w-28 py-1`} />)
  if (gen === 'seqint') fields.push(<span key="st" className="flex items-center gap-1 text-xs">start{num('start', '1')}</span>)
  if (gen === 'ts_device') fields.push(<span key="dv" className="flex items-center gap-1 text-xs">devices{num('devices', '100')}</span>)
  return <div className="flex flex-wrap items-center gap-1">{fields}</div>
}

function JobProgress({ job, onCancel }) {
  const pct = job.total > 0 ? Math.min(100, Math.round((job.inserted / job.total) * 100)) : 0
  const tone = job.status === 'error' ? 'danger' : job.status === 'done' ? 'primary' : job.status === 'canceled' ? 'warning' : 'muted'
  return (
    <div className="mt-4 rounded-lg border p-3">
      <div className="mb-2 flex items-center gap-2">
        <Badge tone={tone}>{job.status}</Badge>
        <span className="text-sm font-medium">{(job.inserted || 0).toLocaleString()} / {(job.total || 0).toLocaleString()} rows</span>
        {job.status === 'running' && <Button variant="subtle" size="sm" className="ml-auto" onClick={onCancel}>Cancel</Button>}
      </div>
      <div className="h-2 overflow-hidden rounded-full bg-surface2">
        <div className={`h-full transition-all ${job.status === 'error' ? 'bg-danger' : 'bg-primary'}`} style={{ width: `${pct}%` }} />
      </div>
      <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
        <span>{Math.round(job.rowsPerSec || 0).toLocaleString()} rows/s</span>
        <span>elapsed {Math.round(job.elapsedSec || 0)}s</span>
        {job.status === 'running' && <span>ETA {Math.round(job.etaSec || 0)}s</span>}
        {job.errors > 0 && <span className="text-danger">{job.errors.toLocaleString()} errored</span>}
      </div>
      {job.message && <div className="mt-2 rounded bg-surface2 px-2 py-1 font-mono text-xs text-muted">{job.message}</div>}
    </div>
  )
}
