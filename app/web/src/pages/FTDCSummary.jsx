import { useEffect, useState } from 'react'
import { Card, Button, Badge, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import TimeChart from '../components/TimeChart.jsx'
import { FilePick } from './PacketInspector.jsx'
import {
  ftdcApi, chartPoints, chartLines, fmtSpan, fmtClock, fmtNum,
  ADVICE_TEXT, ADVICE_FILL, ADVICE_TONE, COMPARE_CHARTS, compareSeries,
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

// LAST_WINDOWS are the zooms people ask for by name rather than by dragging. The end of the
// capture is "now" for a file read off a running node, which is the common case.
const LAST_WINDOWS = [
  { label: 'last 15 min', secs: 15 * 60 },
  { label: 'last hour', secs: 3600 },
]

export default function FTDCSummary() {
  const [model, setModel] = useState(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)
  const [nodes, setNodes] = useState([])
  const [picked, setPicked] = useState('')
  const [files, setFiles] = useState([])
  const [dropped, setDropped] = useState(0)
  // What produced the model on screen, so a zoom can re-read the same thing narrowed:
  // { kind: 'node', stackId, nodeId, label } or { kind: 'upload', files }.
  const [source, setSource] = useState(null)
  const [zoom, setZoom] = useState(null)   // { from, to } epoch seconds, or null for all
  const [zoomStack, setZoomStack] = useState([])
  // Comparison mode: several members' captures, read at once and overlaid.
  const [compare, setCompare] = useState([])       // picked "stackId:nodeId" keys
  const [members, setMembers] = useState(null)     // what came back

  // takeFiles keeps what can be FTDC and counts the rest.
  //
  // A folder pick is the reason this exists: a reader who aims one directory too high sends
  // the whole dbPath, and a 40 GB collection file is not something to put through an upload
  // to find out. One deliberately chosen file is never filtered — the name is the reader's
  // business, and an archive somebody renamed is still recognised from its bytes server-side.
  //
  // The archive test is deliberately not anchored to the end of the name: an archive off a
  // ticket is as often "diagnostic.data.tar.gz.20260814" or "ftdc.zip.bak" as it is a clean
  // .tar.gz, and greying those out is the same mistake an `accept` list would make.
  function takeFiles(list) {
    const all = [...(list || [])]
    if (all.length <= 1) { setFiles(all); setDropped(0); return }
    const keep = all.filter((f) => /^metrics\./.test(f.name) || /\.(gz|tgz|tar|zip)\b/i.test(f.name))
    setFiles(keep)
    setDropped(all.length - keep.length)
  }

  useEffect(() => {
    ftdcApi.nodes().then((n) => setNodes(n || [])).catch(() => {})
  }, [])


  async function run(fn) {
    setError(null); setLoading(true); setModel(null)
    try { setModel(await fn()) }
    catch (e) { setError(e.message || 'Failed to read diagnostic.data') }
    finally { setLoading(false) }
  }

  // A zoom re-reads the SOURCE for the chosen window rather than magnifying the drawn
  // line: the page is sent 1,200 points however long the capture is, so a sixty-second
  // event in an eight-hour file is two points until the window is the event.
  function reread(range) {
    if (!source) return
    setZoom(range)
    if (source.kind === 'node') run(() => ftdcApi.fromNode(source.stackId, source.nodeId, range))
    else run(() => ftdcApi.upload(source.files, range))
  }
  function zoomTo(from, to) {
    setZoomStack((st) => [...st, zoom])
    reread({ from, to })
  }
  function zoomBack() {
    const prev = zoomStack[zoomStack.length - 1] ?? null
    setZoomStack((st) => st.slice(0, -1))
    reread(prev)
  }
  function zoomAll() {
    setZoomStack([])
    reread(null)
  }

  // A comparison is its own read: several members, the same window, one request. It
  // replaces the single-member model on screen rather than sitting beside it, because two
  // sets of charts answering the same question differently is how a page gets misread.
  async function loadCompare() {
    const targets = compare.map((k) => {
      const [stackId, nodeId] = k.split(':')
      return { stackId: Number(stackId), nodeId }
    })
    if (targets.length < 2) return
    setError(null); setLoading(true); setModel(null); setMembers(null)
    try {
      const r = await ftdcApi.compare(targets, zoom)
      setMembers(r.members || [])
    } catch (e) { setError(e.message || 'Failed to read the captures') }
    finally { setLoading(false) }
  }

  const loadNode = () => {
    const n = nodes.find((x) => `${x.stackId}:${x.nodeId}` === picked)
    if (!n) return
    setSource({ kind: 'node', stackId: n.stackId, nodeId: n.nodeId, label: n.label })
    setZoom(null); setZoomStack([]); setMembers(null)
    run(() => ftdcApi.fromNode(n.stackId, n.nodeId))
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

          {/* Comparison. Lag, elections, sync source and quorum are questions about a set,
              and one member's file can only ever say what that member believed. */}
          {nodes.length > 1 && (
            <div className="border-t pt-3">
              <div className="mb-1 text-xs font-medium text-muted">…or compare several members over the same window</div>
              <div className="flex flex-wrap items-center gap-2">
                {nodes.map((n) => {
                  const key = `${n.stackId}:${n.nodeId}`
                  const on = compare.includes(key)
                  return (
                    <button key={key} type="button"
                      onClick={() => setCompare((c) => (on ? c.filter((x) => x !== key) : [...c, key]))}
                      className={`rounded-md border px-2 py-1 text-xs transition ${on ? 'border-primary bg-primary/10 text-primary' : 'bg-bg text-muted hover:text-fg'}`}>
                      {n.label}
                    </button>
                  )
                })}
                <Button size="sm" variant="outline" disabled={compare.length < 2 || loading} onClick={loadCompare}>
                  <Icon.Monitor size={14} /> Compare {compare.length || ''} member{compare.length === 1 ? '' : 's'}
                </Button>
              </div>
            </div>
          )}

          <div className="border-t pt-3">
            <label className="mb-1 block text-xs font-medium text-muted" htmlFor="ftdc-file">
              …or upload the directory's <code>metrics.*</code> files, a <code>.tar.gz</code> or <code>.zip</code> of
              it, or the whole <code> diagnostic.data</code> folder
            </label>
            <div className="grid gap-2 sm:grid-cols-2">
              {/* No `accept`. A metrics file is named metrics.2026-08-16T09-31-52Z-00000,
                  so the browser reads its extension as ".2026-08-16T09-31-52Z-00000" and
                  any extension list at all greys out exactly the files this page exists to
                  read. The archive case is sniffed from the file's magic bytes server-side
                  (gzip or zip), so the filter was never load-bearing — which is just as well,
                  because an archive off a ticket is routinely renamed with a timestamp. */}
              {/* Neither picker shows the selection itself: both feed the same list, and a
                  count printed inside whichever box was clicked last reads as if only that
                  box's pick counted. It is stated once, below, for both. */}
              <FilePick
                id="ftdc-file" multiple file={null}
                onPick={() => {}} onPickMany={takeFiles}
                placeholder="Choose metrics.* files, or a .tar.gz / .zip"
              />
              <FilePick
                id="ftdc-dir" directory file={null}
                onPick={() => {}} onPickMany={takeFiles}
                placeholder="…or choose a diagnostic.data folder"
              />
            </div>
            {(files.length > 0 || dropped > 0) && (
              <p className="mt-1 text-[11px] text-muted">
                {files.length} file(s) selected
                {dropped > 0 && <>, {dropped} ignored — not named <code>metrics.*</code> and not an archive</>}
              </p>
            )}
            {files.length > 0 && (
              <Button className="mt-2" disabled={loading}
                onClick={() => { setSource({ kind: 'upload', files }); setZoom(null); setZoomStack([]); run(() => ftdcApi.upload(files)) }}>
                <Icon.Monitor size={15} /> Chart {files.length} file(s)
              </Button>
            )}
          </div>
        </div>
      </Card>

      {loading && <Card><div className="p-4 text-sm text-muted">Decoding…</div></Card>}
      {error && (
        <Card><div className="p-4 text-sm text-status-crit">{error}</div></Card>
      )}

      {/* The window control. Present whenever there is something to narrow, not only once
          the reader has already found the drag — an 8-hour capture drawn as 1,200 points
          hides a 60-second event, and the way to see it was undiscoverable when the only
          affordance was a cursor change over a chart. */}
      {model && source && (
        <Card>
          <div className="flex flex-wrap items-center gap-2 p-3 text-sm">
            {zoom ? (
              <>
                <span className="text-muted">Zoomed to</span>
                <span className="font-medium text-fg">{fmtClock(zoom.from)} → {fmtClock(zoom.to)}</span>
                <span className="text-[11px] text-muted">
                  ({fmtSpan(zoom.from, zoom.to)} · re-read from the source at full resolution)
                </span>
              </>
            ) : (
              <>
                <span className="text-muted">Window</span>
                <span className="font-medium text-fg">whole capture</span>
                <span className="text-[11px] text-muted">
                  ({fmtSpan(model.from, model.to)} · <b>drag across any chart</b> to zoom into a window,
                  which is re-read from the source at full resolution)
                </span>
              </>
            )}
            <div className="ml-auto flex flex-wrap gap-2">
              {LAST_WINDOWS.map((wdw) => (
                <Button key={wdw.label} size="sm" variant="subtle" disabled={loading || model.to - model.from <= wdw.secs}
                  onClick={() => zoomTo(Math.max(model.from, model.to - wdw.secs), model.to)}>
                  {wdw.label}
                </Button>
              ))}
              {zoom && <Button size="sm" variant="subtle" onClick={zoomBack} disabled={loading}>Back</Button>}
              {zoom && <Button size="sm" variant="subtle" onClick={zoomAll} disabled={loading}>Whole capture</Button>}
            </div>
          </div>
        </Card>
      )}
      {members && <CompareView members={members} />}
      {model && <Summary model={model} />}
      {model && <Findings model={model} />}
      {model && <ConfigAdvice model={model} />}
      {model && <Charts model={model} onZoom={source ? zoomTo : null} />}
    </div>
  )
}

// ConfigAdvice — what to CHANGE, as opposed to what happened.
//
// Every other block on this page reads one chart. This one reads the capture as a whole,
// because the useful recommendations are cross-cutting: the cache is oversized because the
// host is swapping, and the tickets are exhausted because the disk is saturated, and
// neither sentence can be written from a single chart. It sits directly under the findings
// for the same reason the findings sit above the charts — it is the part somebody arriving
// with a capture and a slow server is actually looking for.
//
// "Keep this as it is" is a first-class answer here, deliberately. Half of tuning is being
// told, with evidence, that the knob you were about to turn is not the problem.
export function ConfigAdvice({ model }) {
  const items = model.config || []
  if (items.length === 0) return null
  const changes = items.filter((c) => c.level !== 'ok').length
  return (
    <Card>
      <div className="border-b px-3 py-2">
        <h2 className="text-sm font-medium text-fg">Configuration</h2>
        <p className="mt-0.5 text-xs text-muted">
          {changes === 0
            ? 'Nothing here needs changing — each line below says why, from this capture\u2019s own numbers.'
            : `${changes} setting${changes === 1 ? '' : 's'} worth changing, and the rest confirmed as they are. Every line is argued from this capture, not from a general rule.`}
        </p>
      </div>
      <ul className="divide-y">
        {items.map((c, i) => (
          <li key={i} className="px-3 py-3">
            <div className="flex flex-wrap items-center gap-2">
              <span className={`inline-block h-2 w-2 shrink-0 rounded-full ${ADVICE_FILL[c.level] || ADVICE_FILL.info}`} />
              <span className={`text-[11px] font-medium uppercase tracking-wide ${ADVICE_TONE[c.level] || ADVICE_TONE.info}`}>
                {c.level === 'ok' ? 'keep' : ADVICE_TEXT[c.level] || ADVICE_TEXT.info}
              </span>
              <code className="text-sm font-medium text-fg">{c.setting}</code>
            </div>
            <dl className="mt-1.5 grid gap-x-6 gap-y-1 text-xs sm:grid-cols-[auto_1fr]">
              {c.current && (<><dt className="text-muted">Now</dt><dd className="text-fg">{c.current}</dd></>)}
              {c.suggest && (<><dt className="text-muted">Change to</dt><dd className="text-fg">{c.suggest}</dd></>)}
            </dl>
            <p className="mt-1.5 text-xs leading-relaxed text-muted">{c.why}</p>
            {/* What the change was worth when it was measured, on the hardware it was
                measured on. Kept visually distinct from the evidence above it: one is
                about this capture, the other is about somebody else's run. */}
            {c.effect && <p className="mt-1 border-l-2 pl-2 text-xs leading-relaxed text-muted">{c.effect}</p>}
          </li>
        ))}
      </ul>
    </Card>
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
export function Charts({ model, onZoom = null }) {
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
            <ChartCard chart={c} ts={model.ts} onZoom={onZoom} />
          </div>
        )
      })}
    </div>
  )
}

// CompareView — several members' captures, one chart per question.
//
// The single-member charts answer "what was this server doing". These answer "which member",
// which is a different question and the one an incident on a replica set actually raises.
// Only the series that carry a comparison are overlaid (see COMPARE_CHARTS); the rest stay
// on the per-member view, because three members' cache charts drawn together is nine lines
// and no answer.
export function CompareView({ members }) {
  const ok = (members || []).filter((m) => m.model)
  const failed = (members || []).filter((m) => !m.model)
  if (!ok.length) {
    return (
      <Card>
        <div className="p-4 text-sm text-status-crit">
          None of the selected members could be read.
          {failed.map((m) => <div key={m.nodeId} className="mt-1 text-xs">{m.label}: {m.error}</div>)}
        </div>
      </Card>
    )
  }
  const charts = COMPARE_CHARTS.map((spec) => ({ spec, data: compareSeries(ok, spec) })).filter((c) => c.data)
  return (
    <div className="space-y-3">
      <Card>
        <div className="flex flex-wrap items-center gap-x-6 gap-y-2 p-3 text-sm">
          <Field k="Comparing" v={ok.map((m) => m.label).join(' · ')} />
          <Field k="Window" v={`${fmtClock(Math.min(...ok.map((m) => m.model.from)))} → ${fmtClock(Math.max(...ok.map((m) => m.model.to)))}`} />
          <Field k="Charts" v={String(charts.length)} />
        </div>
        {failed.length > 0 && (
          <div className="border-t px-3 py-2 text-xs text-status-warn">
            {failed.map((m) => <div key={m.nodeId}>{m.label}: {m.error}</div>)}
          </div>
        )}
        <div className="border-t px-3 py-2 text-xs text-muted">
          Each member's capture is decoded on its own and the lines are merged on a shared clock —
          a member with no sample for an instant (it was down, or its capture starts later) draws
          nothing there rather than a zero.
        </div>
      </Card>
      {charts.map(({ spec, data }) => (
        <Card key={spec.id}>
          <div className="flex flex-wrap items-baseline justify-between gap-2 border-b px-3 py-2">
            <h3 className="text-sm font-semibold text-fg">{spec.title}</h3>
            <span className="font-mono text-[10px] text-muted">{data.unit}</span>
          </div>
          <div className="px-2 pt-2">
            <TimeChart points={data.points} lines={data.lines} unit={data.unit} />
          </div>
          <p className="px-3 pb-3 pt-1 text-xs text-muted">{spec.why}</p>
        </Card>
      ))}
    </div>
  )
}

// Summary — what the file is, before anything is read from it. Deliberately first: half of
// "this chart looks wrong" is a file covering a different window than the reader assumed.
export function Summary({ model }) {
  const [open, setOpen] = useState(false)
  if (!model) return null
  const facts = model.server || []
  return (
    <Card>
      <div className="flex flex-wrap items-center gap-x-6 gap-y-2 p-3 text-sm">
        <Field k="Host" v={model.host || '—'} />
        <Field k="Version" v={model.version || '—'} />
        {/* What KIND of process wrote this. A mongos has no storage engine, no oplog and no
            replica-set status, so half the page is legitimately absent from its capture —
            saying so is the difference between "this file is thin" and "this is a router". */}
        {model.role && <Field k="Role" v={model.role} />}
        <Field k="Replica set" v={model.replSet || 'none'} />
        <Field k="Window" v={`${fmtClock(model.from)} → ${fmtClock(model.to)}`} />
        <Field k="Span" v={fmtSpan(model.from, model.to)} />
        <Field k="Samples" v={fmtNum(model.samples)} />
        <Field k="Metrics" v={fmtNum(model.metrics)} />
      </div>
      {/* The type-0 document, which is the only place a capture says what the server WAS.
          Collapsed by default because it is reference rather than reading — but the notes
          inside it (huge pages left on, a cache sized from memory the container does not
          have, a file-descriptor ceiling below 64000) explain more charts than any single
          metric does. */}
      {facts.length > 0 && (
        <div className="border-t">
          <button type="button" onClick={() => setOpen(!open)}
            className="flex w-full items-center justify-between px-3 py-2 text-left text-xs font-medium text-muted hover:text-fg">
            <span>Capture header — what this server was ({facts.length} facts)</span>
            <span aria-hidden="true">{open ? '−' : '+'}</span>
          </button>
          {open && (
            <dl className="grid gap-x-6 gap-y-2 border-t px-3 py-3 text-sm sm:grid-cols-2">
              {facts.map((f) => (
                <div key={f.label} className="min-w-0">
                  <dt className="text-[11px] uppercase tracking-wide text-muted">{f.label}</dt>
                  <dd className="break-words font-medium text-fg">{f.value}</dd>
                  {f.note && <dd className="mt-0.5 text-[11px] leading-snug text-status-warn">{f.note}</dd>}
                </div>
              ))}
            </dl>
          )}
        </div>
      )}
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
export function ChartCard({ chart, ts, onZoom = null }) {
  if (!chart) return null
  const points = chartPoints(ts, chart.series)
  const lines = chartLines(chart.series)
  return (
    <Card>
      <div className="group flex flex-wrap items-baseline justify-between gap-2 border-b px-3 py-2">
        <h3 className="text-sm font-semibold text-fg">{chart.title}</h3>
        <span className="flex items-baseline gap-3">
          {/* The affordance next to the hand. The window bar above says it once; this says
              it where the reader's cursor already is, and only while it is there. */}
          {onZoom && (
            <span className="text-[10px] text-muted opacity-0 transition-opacity group-hover:opacity-100">
              drag across to zoom
            </span>
          )}
          {chart.unit && <span className="text-[11px] text-muted">{chart.unit}</span>}
        </span>
      </div>
      <div className="px-3 pt-3">
        <TimeChart points={points} lines={lines} unit={chart.unit} kind={chart.stack ? 'stacked' : 'line'} onZoom={onZoom} />
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
