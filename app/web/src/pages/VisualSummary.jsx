import { useEffect, useRef, useState } from 'react'
import { Card, Button, Badge, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import TimeChart from '../components/TimeChart.jsx'
import { visualApi } from '../lib/visualApi.js'

// Visual Summary — upload (or pull from a node) a pt-stalk archive and render it as
// professional timeline charts. ~90% charts, ~10% text. Every card renders only if its
// series is present in the parsed model (resilient to missing files in the archive).

export default function VisualSummary() {
  const [model, setModel] = useState(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)
  const [nodes, setNodes] = useState([])
  const [sel, setSel] = useState('')
  const [drag, setDrag] = useState(false)
  const fileRef = useRef(null)

  useEffect(() => {
    visualApi.nodes().then((n) => setNodes(n || [])).catch(() => {})
    const raw = sessionStorage.getItem('vs.target')
    if (raw) {
      sessionStorage.removeItem('vs.target')
      try { const t = JSON.parse(raw); loadNode(t.stackId, t.nodeId) } catch { /* ignore */ }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function run(fn) {
    setError(null); setLoading(true); setModel(null)
    try { setModel(await fn()) } catch (e) { setError(e.message || 'Failed to parse archive') } finally { setLoading(false) }
  }
  const loadUpload = (file) => file && run(() => visualApi.upload(file))
  const loadNode = (stackId, nodeId) => run(() => visualApi.fromNode(stackId, nodeId))

  return (
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      <header>
        <h1 className="text-lg font-semibold text-fg">Visual Summary</h1>
        <p className="text-sm text-muted">Turn a pt-stalk archive into timeline charts — CPU, memory, swap, disk, and MySQL/InnoDB internals at a glance.</p>
      </header>

      <Card>
        <div className="grid gap-3 p-4 md:grid-cols-2">
          {/* Upload */}
          <div
            onDragOver={(e) => { e.preventDefault(); setDrag(true) }}
            onDragLeave={() => setDrag(false)}
            onDrop={(e) => { e.preventDefault(); setDrag(false); loadUpload(e.dataTransfer.files?.[0]) }}
            onClick={() => fileRef.current?.click()}
            className={`flex cursor-pointer flex-col items-center justify-center rounded-xl border-2 border-dashed px-4 py-8 text-center transition ${drag ? 'border-primary bg-primary/5' : 'border-border hover:border-primary/60'}`}>
            <Icon.Bucket size={22} />
            <div className="mt-2 text-sm font-medium text-fg">Drop a pt-stalk <span className="font-mono">.tar.gz</span> here</div>
            <div className="text-xs text-muted">or click to choose a file</div>
            <input ref={fileRef} type="file" accept=".gz,.tgz,.tar.gz,application/gzip" className="hidden"
              onChange={(e) => loadUpload(e.target.files?.[0])} />
          </div>
          {/* From node */}
          <div className="flex flex-col justify-center gap-2">
            <div className="text-sm font-medium text-fg">…or use a node's collected capture</div>
            <select className={inputCls} value={sel} onChange={(e) => setSel(e.target.value)}>
              <option value="">Select a MySQL / PXC node…</option>
              {nodes.map((n) => <option key={`${n.stackId}:${n.nodeId}`} value={`${n.stackId}:${n.nodeId}`}>{n.stackName} · {n.label} ({n.type})</option>)}
            </select>
            <Button size="sm" disabled={!sel} onClick={() => { const [s, n] = sel.split(':'); loadNode(Number(s), n) }}>
              <Icon.Arrow size={15} /> Analyze pt-stalk
            </Button>
            <p className="text-[11px] text-muted">Runs on the archive from that node's last pt-stalk capture (Diagnostics tab).</p>
          </div>
        </div>
      </Card>

      {loading && <div className="rounded-xl border bg-surface px-4 py-8 text-center text-sm text-muted">Parsing archive…</div>}
      {error && <div className="rounded-xl border border-danger/30 bg-danger/10 px-4 py-3 text-sm text-danger">{error}</div>}
      {model && <Report model={model} />}
    </div>
  )
}

// ---- report ----

const FINDING_TILES = [
  { key: 'peakCpuBusyPct', label: 'Peak CPU busy', unit: '%', warn: 70, crit: 90 },
  { key: 'peakDiskUtilPct', label: 'Peak disk util', unit: '%', warn: 70, crit: 90 },
  { key: 'peakSwapUsedMB', label: 'Peak swap used', unit: ' MB', warn: 1, crit: 512 },
  { key: 'peakBpMissRatioPct', label: 'BP read-miss', unit: '%', warn: 1, crit: 5 },
  { key: 'maxHistoryListLength', label: 'Max history list', unit: '', warn: 1e6, crit: 1e7 },
  { key: 'maxCheckpointAgeBytes', label: 'Max checkpoint age', unit: ' B', warn: 1e9, crit: 4e9, bytes: true },
  { key: 'maxReplicationLagSec', label: 'Max repl lag', unit: ' s', warn: 1, crit: 30 },
  { key: 'peakHandlerReadRndNextPerSec', label: 'Peak rows/s (no index)', unit: '/s', warn: 1e5, crit: 1e7 },
  { key: 'maxLongQuerySec', label: 'Longest query', unit: ' s', warn: 5, crit: 60 },
]

function Report({ model }) {
  const f = model.summary?.findings || {}
  const facts = model.summary?.facts || {}
  const has = (k) => model.available?.[k]

  return (
    <div className="space-y-4">
      {/* 10% text: source facts + headline findings */}
      <Card title="Summary" subtitle={`${model.source?.host || 'host'} · ${model.source?.engine === 'pxc' ? 'Percona XtraDB Cluster' : 'MySQL / Percona Server'}${model.source?.capturedAt ? ' · captured ' + new Date(model.source.capturedAt).toLocaleString() : ''}`}>
        <div className="space-y-3 p-4">
          <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-muted">
            {facts.processors && <span>CPU: <span className="text-fg">{facts.processors}</span></span>}
            {facts.memory && <span>RAM: <span className="text-fg">{facts.memory}</span></span>}
            {facts.mysqlVersion && <span>Version: <span className="text-fg">{facts.mysqlVersion}</span></span>}
            {facts.uptime && <span>Uptime: <span className="text-fg">{facts.uptime}</span></span>}
            {facts.kernel && <span>Kernel: <span className="text-fg">{facts.kernel}</span></span>}
          </div>
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
            {FINDING_TILES.filter((t) => f[t.key] !== undefined).map((t) => <StatTile key={t.key} tile={t} value={f[t.key]} />)}
            {f.deadlockDetected ? <div className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2"><div className="text-[11px] text-muted">Deadlock</div><div className="text-sm font-semibold text-danger">detected</div></div> : null}
          </div>
        </div>
      </Card>

      {/* 90% charts */}
      <div className="grid gap-4 lg:grid-cols-2">
        <SectionHead>Operating system</SectionHead>
        {model.cpu?.overall && (
          <ChartCard title="CPU busy" subtitle="% by mode (excl. idle) · Overall + per-CPU" span>
            <TabbedChart data={model.cpu} labelFor={(k) => 'CPU ' + k} kind="stacked" unit="%"
              lines={[cl('usr', 'user', 0), cl('sys', 'system', 5), cl('iowait', 'iowait', 2), cl('steal', 'steal', 7)]} />
          </ChartCard>
        )}
        {has('memory') && (
          <ChartCard title="Memory" subtitle="MB">
            <TimeChart points={model.series.memory.points} kind="stacked" unit="MB"
              lines={[cl('used', 'used', 0), cl('cache', 'cache', 1), cl('buff', 'buffers', 2), cl('free', 'free', 3)]} />
          </ChartCard>
        )}
        {has('swap') && (
          <ChartCard title="Swap used" subtitle="MB">
            <TimeChart points={model.series.swap.points} unit="MB" lines={[cl('used', 'swap used', 0)]} />
          </ChartCard>
        )}
        {model.disk?.overall && <SectionHead>Disk</SectionHead>}
        {model.disk?.overall && (
          <ChartCard title="Disk utilization" subtitle="% busy · Overall + per-device" span>
            <TabbedChart data={model.disk} labelFor={(k) => k} unit="%"
              linesOverall={[cl('util', 'avg %util', 0)]} lines={[cl('util', '%util', 0)]} />
          </ChartCard>
        )}
        {model.disk?.overall && (
          <ChartCard title="Disk throughput" subtitle="KB/s · Overall + per-device" span>
            <TabbedChart data={model.disk} labelFor={(k) => k} unit="KB/s"
              lines={[cl('rKBs', 'read', 0), cl('wKBs', 'write', 5)]} />
          </ChartCard>
        )}
        {model.disk?.overall && (
          <ChartCard title="Disk IOPS" subtitle="operations/s · Overall + per-device" span>
            <TabbedChart data={model.disk} labelFor={(k) => k} unit="/s"
              lines={[cl('rs', 'read', 0), cl('ws', 'write', 5), cl('iops', 'total (r+w)', 4)]} />
          </ChartCard>
        )}
        {model.disk?.overall && (
          <ChartCard title="Disk latency (await)" subtitle="ms · Overall + per-device" span>
            <TabbedChart data={model.disk} labelFor={(k) => k} unit="ms"
              lines={[cl('rAwait', 'read await', 0), cl('wAwait', 'write await', 5)]} />
          </ChartCard>
        )}

        {(has('networkThroughput') || has('netStates') || has('sockQueues')) && <SectionHead>Network</SectionHead>}
        {has('networkThroughput') && (
          <ChartCard title="MySQL network throughput" subtitle="bytes in/out per second (human-readable)">
            <TimeChart points={model.series.networkThroughput.points} unit="B/s" lines={[cl('received', 'received', 0), cl('sent', 'sent', 5)]} />
          </ChartCard>
        )}
        {has('netStates') && (
          <ChartCard title="Network connection states" subtitle="TCP connections by state (netstat)" span>
            <StackedStatesChart series={model.series.netStates} />
          </ChartCard>
        )}
        {has('sockQueues') && (
          <ChartCard title="Socket send/receive backlog" subtitle="count of sockets with non-zero Recv-Q / Send-Q">
            <TimeChart points={model.series.sockQueues.points} lines={[cl('recvBacklog', 'recv-Q backlog', 6), cl('sendBacklog', 'send-Q backlog', 7)]} />
          </ChartCard>
        )}

        <SectionHead>MySQL / InnoDB</SectionHead>
        {has('bufferPool') && (
          <ChartCard title="InnoDB buffer pool pages" subtitle="pages">
            <TimeChart points={model.series.bufferPool.points}
              lines={[cl('dataPages', 'data', 0), cl('dirtyPages', 'dirty', 5), cl('freePages', 'free', 3)]} />
          </ChartCard>
        )}
        {has('bufferPool') && (
          <ChartCard title="Buffer pool reads" subtitle="logical read requests vs physical disk reads (/s)">
            <div className="grid grid-cols-2 gap-2">
              <TimeChart points={model.series.bufferPool.points} unit="/s" lines={[cl('readReqPerSec', 'read requests', 0)]} height={148} />
              <TimeChart points={model.series.bufferPool.points} unit="/s" lines={[cl('diskReadPerSec', 'disk reads', 6)]} height={148} />
            </div>
          </ChartCard>
        )}
        {has('handlerReadRndNext') && (
          <ChartCard title="Rows scanned without index" subtitle="Handler_read_rnd_next /s">
            <TimeChart points={model.series.handlerReadRndNext.points} unit="/s" lines={[cl('perSec', 'rows/s', 7)]} />
          </ChartCard>
        )}
        {has('historyList') && (
          <ChartCard title="InnoDB history list length" subtitle="undo records pending purge (sparse)">
            <TimeChart points={model.series.historyList.points} lines={[cl('value', 'history list', 4)]} />
          </ChartCard>
        )}
        {has('checkpointAge') && (
          <ChartCard title="InnoDB checkpoint age" subtitle="redo since last checkpoint (sparse)">
            <TimeChart points={model.series.checkpointAge.points} unit="B" lines={[cl('age', 'checkpoint age', 5)]} />
          </ChartCard>
        )}
        {has('replicationLag') && (
          <ChartCard title="Replication lag" subtitle="seconds behind source">
            <TimeChart points={model.series.replicationLag.points} unit="s" lines={[cl('seconds', 'lag', 6)]} />
          </ChartCard>
        )}
        {has('galera') && (
          <ChartCard title="Galera flow control & recv queue" subtitle="PXC cluster replication health" span>
            <div className="grid grid-cols-2 gap-2">
              <TimeChart points={model.series.galera.points} unit="%" lines={[cl('flowControlPausedPct', 'flow-control paused %', 6)]} height={148} />
              <TimeChart points={model.series.galera.points} lines={[cl('recvQueue', 'recv queue', 0), cl('certDepsDistance', 'cert deps dist', 4)]} height={148} />
            </div>
          </ChartCard>
        )}
        {has('rowLockWaits') && (
          <ChartCard title="InnoDB row-lock waits" subtitle="lock contention (deadlock precursor) /s">
            <TimeChart points={model.series.rowLockWaits.points} unit="/s" lines={[cl('perSec', 'lock waits', 5)]} />
          </ChartCard>
        )}
        {has('threads') && (
          <ChartCard title="Threads" subtitle="running vs connected">
            <TimeChart points={model.series.threads.points} lines={[cl('running', 'running', 0), cl('connected', 'connected', 1)]} />
          </ChartCard>
        )}
        {has('qps') && (
          <ChartCard title="Query throughput" subtitle="questions + statement mix /s">
            <TimeChart points={model.series.qps.points} unit="/s"
              lines={[cl('questions', 'questions', 0), cl('select', 'select', 1), cl('insert', 'insert', 3), cl('update', 'update', 2), cl('delete', 'delete', 6)]} />
          </ChartCard>
        )}
        {has('innodbRowOps') && (
          <ChartCard title="InnoDB row operations" subtitle="/s">
            <TimeChart points={model.series.innodbRowOps.points} unit="/s"
              lines={[cl('read', 'read', 0), cl('inserted', 'inserted', 3), cl('updated', 'updated', 2), cl('deleted', 'deleted', 6)]} />
          </ChartCard>
        )}
        {has('tmpDiskTables') && (
          <ChartCard title="Temp tables on disk" subtitle="Created_tmp_disk_tables /s">
            <TimeChart points={model.series.tmpDiskTables.points} unit="/s" lines={[cl('perSec', 'tmp disk tables', 2)]} />
          </ChartCard>
        )}
        {has('slowQueries') && (
          <ChartCard title="Slow queries" subtitle="/s">
            <TimeChart points={model.series.slowQueries.points} unit="/s" lines={[cl('perSec', 'slow queries', 7)]} />
          </ChartCard>
        )}
        {has('abortedConns') && (
          <ChartCard title="Aborted connections" subtitle="/s">
            <TimeChart points={model.series.abortedConns.points} unit="/s" lines={[cl('clients', 'clients', 6), cl('connects', 'connects', 7)]} />
          </ChartCard>
        )}
        {has('threadStates') && (
          <ChartCard title="Thread states" subtitle="what threads were doing (from processlist)" span>
            <StackedStatesChart series={model.series.threadStates} />
          </ChartCard>
        )}
      </div>

      {has('processlist') && (
        <Card title="MySQL processlist" subtitle="consolidated per thread + query — a query recurring across captures is one row (Time = longest observed, Seen = captures). Click a column to sort.">
          <div className="p-3"><SortableTable columns={PL_COLS} rows={model.processlist} initial={{ key: 'time', dir: 'desc' }} /></div>
        </Card>
      )}
      {has('innodbTrx') && (
        <Card title="InnoDB transactions per session" subtitle="from SHOW ENGINE INNODB STATUS, consolidated per session (Active = longest observed). Click a column to sort.">
          <div className="p-3"><SortableTable columns={TRX_COLS} rows={model.innodbTrx} initial={{ key: 'active', dir: 'desc' }} /></div>
        </Card>
      )}
      {model.netQueues && model.netQueues.length > 0 && (
        <Card title="Sockets with sustained send/receive backlog" subtitle="non-zero Recv-Q / Send-Q across multiple captures (possible network/consumer stalls). Click a column to sort.">
          <div className="p-3"><SortableTable columns={NETQ_COLS} rows={model.netQueues} initial={{ key: 'maxSend', dir: 'desc' }} /></div>
        </Card>
      )}
      {model.deadlock?.detected && (
        <Card title="Latest detected deadlock" subtitle={model.deadlock.when ? new Date(model.deadlock.when).toLocaleString() : ''}>
          <pre className="max-h-72 overflow-auto whitespace-pre-wrap p-3 font-mono text-[11px] text-fg">{model.deadlock.text}</pre>
        </Card>
      )}
    </div>
  )
}

// cl builds a chart line spec: value key, legend label, palette slot.
function cl(key, label, color) { return { key, label, color } }

// SectionHead is a full-width group heading inside the chart grid.
function SectionHead({ children }) {
  return <div className="lg:col-span-2 pt-1"><h2 className="text-sm font-semibold text-fg">{children}</h2></div>
}

// SortableTable renders rows (array of string maps) with click-to-sort columns.
// columns: [{ key, label, numeric?, mono?, muted?, wide? }].
function SortableTable({ columns, rows, initial }) {
  const [sortKey, setSortKey] = useState(initial?.key || columns[0].key)
  const [dir, setDir] = useState(initial?.dir || 'asc')
  const col = columns.find((c) => c.key === sortKey) || columns[0]
  const sorted = [...rows].sort((a, b) => {
    if (col.numeric) return (dir === 'asc' ? 1 : -1) * ((parseFloat(a[sortKey]) || 0) - (parseFloat(b[sortKey]) || 0))
    return (dir === 'asc' ? 1 : -1) * String(a[sortKey] || '').localeCompare(String(b[sortKey] || ''))
  })
  const toggle = (k) => { if (k === sortKey) setDir((d) => (d === 'asc' ? 'desc' : 'asc')); else { setSortKey(k); setDir(k === sortKey ? dir : 'asc') } }
  return (
    <div className="max-h-96 overflow-auto rounded-lg border">
      <table className="w-full text-xs">
        <thead className="sticky top-0 z-10 bg-surface2">
          <tr className="text-left">
            {columns.map((c) => (
              <th key={c.key} onClick={() => toggle(c.key)}
                className="cursor-pointer select-none whitespace-nowrap px-2 py-1.5 font-medium text-muted hover:text-fg">
                {c.label}{sortKey === c.key ? (dir === 'asc' ? ' ▲' : ' ▼') : ''}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {sorted.map((r, i) => (
            <tr key={i} className="border-t align-top">
              {columns.map((c) => (
                <td key={c.key} className={`px-2 py-1 ${c.mono ? 'font-mono' : ''} ${c.wide ? 'min-w-[16rem] break-all text-[11px]' : 'whitespace-nowrap'} ${c.muted ? 'text-muted' : 'text-fg'}`}>
                  {r[c.key] === '' ? '—' : r[c.key]}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

const PL_COLS = [
  { key: 'id', label: 'Id', numeric: true, mono: true },
  { key: 'user', label: 'User', muted: true },
  { key: 'db', label: 'DB', muted: true },
  { key: 'command', label: 'Command', muted: true },
  { key: 'state', label: 'State', muted: true },
  { key: 'time', label: 'Time (s)', numeric: true, mono: true },
  { key: 'seen', label: 'Seen', numeric: true, mono: true },
  { key: 'info', label: 'Query', mono: true, wide: true },
]
const TRX_COLS = [
  { key: 'thread', label: 'Thread', numeric: true, mono: true },
  { key: 'trx', label: 'Trx id', mono: true, muted: true },
  { key: 'status', label: 'Status', muted: true },
  { key: 'active', label: 'Active (s)', numeric: true, mono: true },
  { key: 'rowLocks', label: 'Row locks', numeric: true, mono: true },
  { key: 'lockWait', label: 'Lock wait', muted: true },
  { key: 'seen', label: 'Seen', numeric: true, mono: true },
  { key: 'query', label: 'Query', mono: true, wide: true },
]
const NETQ_COLS = [
  { key: 'local', label: 'Local', mono: true },
  { key: 'foreign', label: 'Foreign', mono: true, muted: true },
  { key: 'state', label: 'State', muted: true },
  { key: 'prog', label: 'Program', muted: true },
  { key: 'maxRecv', label: 'max Recv-Q', numeric: true, mono: true },
  { key: 'maxSend', label: 'max Send-Q', numeric: true, mono: true },
  { key: 'hits', label: 'Seen', numeric: true, mono: true },
]

function humanBytes(v) {
  if (v < 1024) return v.toFixed(0) + ' B'
  const u = ['KB', 'MB', 'GB', 'TB']; let n = v, i = -1
  do { n /= 1024; i++ } while (n >= 1024 && i < u.length - 1)
  return n.toFixed(1) + ' ' + u[i]
}
function StatTile({ tile, value }) {
  const tone = value >= tile.crit ? 'text-danger' : value >= tile.warn ? 'text-warning' : 'text-fg'
  let disp
  if (tile.bytes) disp = humanBytes(value)
  else if (value >= 1000) disp = Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 }).format(value) + tile.unit
  else disp = (Math.round(value * 10) / 10) + tile.unit
  return (
    <div className="rounded-lg border bg-surface2 px-3 py-2">
      <div className="text-[11px] text-muted">{tile.label}</div>
      <div className={`text-sm font-semibold ${tone}`}>{disp}</div>
    </div>
  )
}

function ChartCard({ title, subtitle, span, children }) {
  return (
    <Card title={title} subtitle={subtitle} className={span ? 'lg:col-span-2' : ''}>
      <div className="p-3 pt-2">{children}</div>
    </Card>
  )
}

// TabbedChart drives CPU/disk cards: an "Overall" tab plus one tab per CPU/device.
function TabbedChart({ data, lines, linesOverall, labelFor, unit, kind = 'line' }) {
  const [tab, setTab] = useState('overall')
  const tabs = ['overall', ...(data.order || [])]
  const series = tab === 'overall' ? data.overall : data.tabs?.[tab]
  return (
    <div>
      <div className="mb-2 flex flex-wrap gap-1 overflow-x-auto rounded-lg bg-surface2 p-1">
        {tabs.map((k) => (
          <button key={k} onClick={() => setTab(k)}
            className={`whitespace-nowrap rounded-md px-2 py-0.5 text-[11px] font-medium transition ${tab === k ? 'bg-surface text-fg shadow' : 'text-muted'}`}>
            {k === 'overall' ? 'Overall' : labelFor(k)}
          </button>
        ))}
      </div>
      {series
        ? <TimeChart points={series.points} lines={tab === 'overall' ? (linesOverall || lines) : lines} unit={unit} kind={kind} />
        : <div className="py-6 text-center text-xs text-muted">no data</div>}
    </div>
  )
}

// StackedStatesChart collapses dynamic state keys to the top 7 (+ "other") for a readable
// stacked-area, since categorical hues are never cycled beyond 8. Used for processlist
// thread states and netstat connection states.
function StackedStatesChart({ series }) {
  const totals = {}
  for (const p of series.points) for (const k of series.metrics) totals[k] = (totals[k] || 0) + (p.v[k] || 0)
  const top = Object.keys(totals).sort((a, b) => totals[b] - totals[a])
  const keep = top.slice(0, 7)
  const rest = top.slice(7)
  const points = series.points.map((p) => {
    const v = {}
    for (const k of keep) v[k] = p.v[k] || 0
    if (rest.length) v.other = rest.reduce((s, k) => s + (p.v[k] || 0), 0)
    return { t: p.t, v }
  })
  const lines = keep.map((k, i) => cl(k, k, i))
  if (rest.length) lines.push(cl('other', 'other', 7))
  return <TimeChart points={points} lines={lines} kind="stacked" />
}
