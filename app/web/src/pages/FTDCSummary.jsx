import { useEffect, useRef, useState } from 'react'
import { Card, Button, Badge, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import TimeChart from '../components/TimeChart.jsx'
import { FilePick } from './PacketInspector.jsx'
import {
  ftdcApi, chartPoints, chartLines, fmtSpan, fmtClock, fmtNum,
  ADVICE_TEXT, ADVICE_FILL, ADVICE_TONE,
} from '../lib/ftdcApi.js'

// FTDC Summary — MongoDB's diagnostic.data, charted.
//
// Every mongod writes this directory once a second, with no configuration and almost no
// cost: serverStatus, replSetGetStatus, the WiredTiger statistics and the host's counters.
// It is the black box, and it is the only artefact that can answer "what was the server
// doing at 04:12 last Tuesday" when nobody thought to be watching at the time.
//
// The page is deliberately a short list of charts rather than a metric browser. A decoded
// file holds about four thousand metrics; a browser of all four thousand helps somebody who
// already knows what they are looking for, and this page is for the other case.

export default function FTDCSummary() {
  const [model, setModel] = useState(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)
  const [nodes, setNodes] = useState([])
  const [picked, setPicked] = useState('')
  const [files, setFiles] = useState([])
  const fileRef = useRef(null)

  useEffect(() => {
    ftdcApi.nodes().then((n) => setNodes(n || [])).catch(() => {})
  }, [])

  async function run(fn) {
    setError(null); setLoading(true); setModel(null)
    try { setModel(await fn()) }
    catch (e) { setError(e.message || 'Failed to read diagnostic.data') }
    finally { setLoading(false) }
  }

  const loadNode = () => {
    const n = nodes.find((x) => `${x.stackId}:${x.nodeId}` === picked)
    if (n) run(() => ftdcApi.fromNode(n.stackId, n.nodeId))
  }

  return (
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      <header>
        <h1 className="text-lg font-semibold text-fg">FTDC Summary</h1>
        <p className="text-sm text-muted">
          MongoDB's <code className="rounded bg-surface2 px-1">diagnostic.data</code> — the per-second black box every
          mongod writes without being asked — decoded and charted. Replication lag, the oplog window, tickets, queues
          and the cache, none of which the server log records.
        </p>
      </header>

      <Card>
        <div className="space-y-3 p-3">
          <div className="flex flex-wrap items-end gap-2">
            <div className="min-w-56 flex-1">
              <label className="mb-1 block text-xs font-medium text-muted" htmlFor="ftdc-node">Read from a running node</label>
              <select id="ftdc-node" className={inputCls} value={picked} onChange={(e) => setPicked(e.target.value)}>
                <option value="">Pick a MongoDB node…</option>
                {nodes.map((n) => (
                  <option key={`${n.stackId}:${n.nodeId}`} value={`${n.stackId}:${n.nodeId}`}>
                    {n.label} — {n.stackName}
                  </option>
                ))}
              </select>
            </div>
            <Button onClick={loadNode} disabled={!picked || loading}>
              <Icon.Server size={15} /> Read diagnostic.data
            </Button>
          </div>

          <div className="border-t pt-3">
            <label className="mb-1 block text-xs font-medium text-muted" htmlFor="ftdc-file">
              …or upload the directory's <code>metrics.*</code> files, or a <code>.tar.gz</code> of it
            </label>
            <FilePick
              id="ftdc-file" multiple accept=".gz,.tgz,.tar,metrics.*"
              file={files.length ? { name: `${files.length} file(s) selected` } : null}
              onPick={() => {}} onPickMany={setFiles}
              placeholder="Choose metrics.* files or a diagnostic.data archive"
            />
            {files.length > 0 && (
              <Button className="mt-2" onClick={() => run(() => ftdcApi.upload(files))} disabled={loading}>
                <Icon.Monitor size={15} /> Chart {files.length} file(s)
              </Button>
            )}
            <input ref={fileRef} type="file" className="hidden" />
          </div>
        </div>
      </Card>

      {loading && <Card><div className="p-4 text-sm text-muted">Decoding…</div></Card>}
      {error && (
        <Card><div className="p-4 text-sm text-status-crit">{error}</div></Card>
      )}

      {model && <Summary model={model} />}
      {model && <Findings model={model} />}
      {model && <Charts model={model} />}
    </div>
  )
}

// Findings — the shortlist, and the reason the rest of the page is allowed to be long.
//
// Thirty-odd charts is more than anybody reads in order. Every chart already carries an
// advisor saying what its own numbers mean, so the ones that came back warn or crit are
// exactly the shortlist, and they can be gathered here as jump links. Nothing is hidden —
// the full page is still below, because "nothing flagged" is not the same as "nothing
// happened" and the reader is often looking for something no threshold knows about.
export function Findings({ model }) {
  const hits = (model?.charts || []).filter((c) => c.advice?.level === 'crit' || c.advice?.level === 'warn')
  const jump = (id) => document.getElementById(`ftdc-${id}`)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  return (
    <Card>
      <div className="border-b px-3 py-2 text-sm font-semibold text-fg">
        What stands out {hits.length > 0 && <span className="font-normal text-muted">({hits.length} of {model.charts.length} charts)</span>}
      </div>
      {hits.length === 0 ? (
        <p className="px-3 py-2 text-sm text-muted">
          Nothing on this page crossed a threshold. That is not the same as nothing having happened — read the
          charts for the window you care about, because no advisor knows what you are looking for.
        </p>
      ) : (
        <ul className="divide-y">
          {hits.map((c) => (
            <li key={c.id}>
              <button
                type="button" onClick={() => jump(c.id)}
                className="flex w-full items-baseline gap-2 px-3 py-2 text-left hover:bg-surface2"
              >
                <span className={`mt-1.5 inline-block h-2 w-2 shrink-0 rounded-full ${ADVICE_FILL[c.advice.level]}`} />
                <span className={`shrink-0 text-[11px] font-medium uppercase tracking-wide ${ADVICE_TONE[c.advice.level]}`}>
                  {ADVICE_TEXT[c.advice.level]}
                </span>
                <span className="shrink-0 text-sm font-medium text-fg">{c.title}</span>
                <span className="text-sm text-muted">— {c.advice.headline}</span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </Card>
  )
}

// Charts — the chart list, broken by group heading.
//
// Thirty-odd charts in one flat column is a column nobody reads to the bottom of. The
// backend already decides the order and the grouping; this only inserts a heading each time
// the group changes, so adding a chart on the server needs no change here.
export function Charts({ model }) {
  if (!model?.charts?.length) return null
  let last = null
  return (
    <div className="space-y-3">
      {model.charts.map((c) => {
        const head = c.group && c.group !== last ? c.group : null
        last = c.group || last
        return (
          // scroll-mt keeps a jumped-to chart clear of the top of the viewport rather than
          // flush against it, which reads as the chart above being the one you landed on.
          <div key={c.id} id={`ftdc-${c.id}`} className="scroll-mt-4 space-y-3">
            {head && (
              <h2 className="pt-2 text-xs font-semibold uppercase tracking-wide text-muted">{head}</h2>
            )}
            <ChartCard chart={c} ts={model.ts} />
          </div>
        )
      })}
    </div>
  )
}

// Summary — what the file is, before anything is read from it. Deliberately first: half of
// "this chart looks wrong" is a file covering a different window than the reader assumed.
export function Summary({ model }) {
  if (!model) return null
  return (
    <Card>
      <div className="flex flex-wrap items-center gap-x-6 gap-y-2 p-3 text-sm">
        <Field k="Host" v={model.host || '—'} />
        <Field k="Version" v={model.version || '—'} />
        <Field k="Replica set" v={model.replSet || 'none'} />
        <Field k="Window" v={`${fmtClock(model.from)} → ${fmtClock(model.to)}`} />
        <Field k="Span" v={fmtSpan(model.from, model.to)} />
        <Field k="Samples" v={fmtNum(model.samples)} />
        <Field k="Metrics" v={fmtNum(model.metrics)} />
      </div>
      {model.notes?.length > 0 && (
        <div className="border-t px-3 py-2 text-xs text-muted">
          {model.notes.map((n, i) => <p key={i}>{n}</p>)}
        </div>
      )}
    </Card>
  )
}

function Field({ k, v }) {
  return (
    <div>
      <div className="text-[11px] uppercase tracking-wide text-muted">{k}</div>
      <div className="font-medium text-fg">{v}</div>
    </div>
  )
}

// ChartCard — one chart, what it is for, and what this capture's numbers say about it.
//
// The "why" line is not decoration. Somebody who knows what a WiredTiger ticket is does not
// need it, and somebody who does not is exactly the person holding a diagnostic.data
// directory they were sent and no idea what to do with it.
export function ChartCard({ chart, ts }) {
  if (!chart) return null
  const points = chartPoints(ts, chart.series)
  const lines = chartLines(chart.series)
  return (
    <Card>
      <div className="flex flex-wrap items-baseline justify-between gap-2 border-b px-3 py-2">
        <h3 className="text-sm font-semibold text-fg">{chart.title}</h3>
        {chart.unit && <span className="text-[11px] text-muted">{chart.unit}</span>}
      </div>
      <div className="px-3 pt-3">
        <TimeChart points={points} lines={lines} unit={chart.unit} kind={chart.stack ? 'stacked' : 'line'} />
      </div>
      <p className="px-3 pb-2 pt-1 text-xs text-muted">{chart.why}</p>
      {chart.advice && <Advice a={chart.advice} />}
    </Card>
  )
}

// Advice — the advisor line. Colour is never the only signal: the level is spelled out in
// words beside the dot, so it survives greyscale and a colour-blind reader.
export function Advice({ a }) {
  if (!a) return null
  return (
    <div className="border-t px-3 py-2">
      <div className="flex items-center gap-2">
        <span className={`inline-block h-2 w-2 shrink-0 rounded-full ${ADVICE_FILL[a.level] || ADVICE_FILL.info}`} />
        <span className={`text-[11px] font-medium uppercase tracking-wide ${ADVICE_TONE[a.level] || ADVICE_TONE.info}`}>
          {ADVICE_TEXT[a.level] || ADVICE_TEXT.info}
        </span>
        <span className="text-sm font-medium text-fg">{a.headline}</span>
      </div>
      {a.detail && <p className="mt-1 text-xs text-muted">{a.detail}</p>}
      {a.action && (
        <p className="mt-1 text-xs text-fg/85">
          <Badge>What to do</Badge> <span className="ml-1">{a.action}</span>
        </p>
      )}
    </div>
  )
}
