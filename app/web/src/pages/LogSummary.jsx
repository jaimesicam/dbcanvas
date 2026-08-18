import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Card, Button, Badge, Field, inputCls } from '../components/ui.jsx'
import { FilePick } from './PacketInspector.jsx'
import {
  logApi, SEVS, SEV_LABEL, SEV_TEXT, SEV_ROW, SEV_CARD, SEV_MARK, SEV_FILL,
  CLASS_LABEL, STATE_TEXT, STATE_SEV, ENGINE_LABEL, FLAVOUR_LABEL,
  nodeFill, nodeTint, nodeEdge, nodeEdgeSoft, NODE_SLOTS,
  logTimeOfDay, logDateTime, logISO, logDur, logBytes,
} from '../lib/logApi.js'

// Log Summary — read several database servers' logs as one classified timeline.
//
// The Packet Inspector answers "what crossed the wire". This answers the other half:
// what the servers said about themselves, which for a cluster is where the entire story
// of an outage lives. A Galera member's error log records a crash, an eviction, a state
// transfer and a rejoin without once using the ERROR level, so severity here comes from
// what a record MEANS — the good, the warning and the bad — not from its level field.
//
// The layout is built around the question three logs are actually opened to answer:
//
//	Verdict        what the whole bundle adds up to, worst first
//	Cluster timeline  one swimlane per node, coloured by the state it was in, with event
//	               ticks over it. Click an instant and every node's state at that instant
//	               is printed side by side; drag to narrow everything below.
//	Events         the evidence, filtered and paged by the server
//
// PXC is where the depth is (see logsummary_galera.go and the fixtures its rules were
// written against); PostgreSQL, MongoDB and Valkey come through the Packet Inspector's
// existing classifiers with the same timeline, severity split and comparison on top.

const PAGE = 200
const emptyRange = () => ({ fromTs: '', toTs: '', src: -1, class: '', q: '', sev: [] })

// NodeChip names a node, in its own colour, everywhere a node is named.
//
// The dot carries the colour and the text stays in ink — a hue validated to 3:1 against the
// surface is a legal MARK, not legal type, and colouring the label would quietly ask it to
// be both. The name is always present, which is what lets the colour be an accelerant
// rather than the signal: a reader who cannot separate two of the hues still reads "pxc03".
export function NodeChip({ src, name, size = 'sm' }) {
  const big = size === 'lg'
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 ${nodeTint(src)} ${nodeEdgeSoft(src)} ` +
      (big ? 'text-sm font-semibold' : 'text-[11px] font-medium')}>
      <span className={`inline-block shrink-0 rounded-full ${nodeFill(src)} ${big ? 'h-2.5 w-2.5' : 'h-2 w-2'}`} />
      <span className="text-fg">{name}</span>
    </span>
  )
}

export default function LogSummary() {
  const [targets, setTargets] = useState(null)
  const [picked, setPicked] = useState([])
  const [lines, setLines] = useState(5000)
  const [bundles, setBundles] = useState([])
  const [id, setId] = useState('')
  const [bundle, setBundle] = useState(null)
  const [findings, setFindings] = useState([])
  const [range, setRange] = useState(emptyRange())
  const [buckets, setBuckets] = useState(180)
  const [timeline, setTimeline] = useState(null)
  const [page, setPage] = useState({ events: [], matched: 0, offset: 0 })
  const [selected, setSelected] = useState(null)
  const [snapshot, setSnapshot] = useState(null) // the "at this instant" readout
  const [uploadOpen, setUploadOpen] = useState(false)
  const [files, setFiles] = useState([])
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    logApi.targets()
      .then((t) => setTargets(Array.isArray(t) ? t : []))
      .catch((e) => { setErr(`Could not load nodes: ${e.message}`); setTargets([]) })
    refreshBundles()
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  const refreshBundles = useCallback(
    () => logApi.list().then((l) => setBundles(Array.isArray(l) ? l : [])).catch(() => {}), [])

  const reload = useCallback(async (offset) => {
    if (!id) return
    const params = { ...range, buckets }
    try {
      const [tl, pg] = await Promise.all([
        logApi.timeline(id, params),
        logApi.events(id, { ...params, limit: PAGE, offset }),
      ])
      setTimeline(tl)
      setPage({ ...pg, offset })
    } catch (e) { setErr(e.message) }
  }, [id, range, buckets])

  useEffect(() => { if (id) reload(0) }, [id, JSON.stringify(range), buckets]) // eslint-disable-line react-hooks/exhaustive-deps

  async function open(bid) {
    setErr(''); setSelected(null); setSnapshot(null); setTimeline(null); setRange(emptyRange())
    try {
      const d = await logApi.get(bid)
      setBundle(d.bundle)
      setFindings(d.findings || [])
      setId(bid)
    } catch (e) { setErr(e.message) }
  }

  async function collect() {
    if (!picked.length) { setErr('choose at least one node'); return }
    setErr(''); setBusy(true)
    try {
      const stackId = Number(picked[0].split(':')[0])
      const nodeIds = picked.filter((p) => Number(p.split(':')[0]) === stackId).map((p) => p.split(':').slice(1).join(':'))
      const rec = await logApi.collect({ stackId, nodeIds, lines: Number(lines) })
      await refreshBundles()
      await open(rec.id)
    } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  async function upload() {
    if (!files.length) { setErr('choose at least one log file'); return }
    setErr(''); setBusy(true)
    try {
      const rec = await logApi.upload(files)
      await refreshBundles()
      await open(rec.id)
      setUploadOpen(false); setFiles([])
    } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  async function dropBundle(bid) {
    try {
      await logApi.remove(bid)
      if (bid === id) { setId(''); setBundle(null); setTimeline(null); setFindings([]) }
      refreshBundles()
    } catch (e) { setErr(e.message) }
  }

  // Clicking the timeline asks the server what every node was doing at that instant —
  // the question three logs are opened to answer, and the one that is genuinely tedious
  // to answer by hand.
  const inspectAt = useCallback(async (ts) => {
    if (!id) return
    try { setSnapshot(await logApi.at(id, ts.toFixed(6))) } catch (e) { setErr(e.message) }
  }, [id])

  // A finding's "take me there": narrow the window around it and land on its first event.
  const goToFinding = useCallback(async (f) => {
    if (!f.at || !bundle) return
    const pad = Math.max(5, ((f.until || f.at) - f.at) * 0.6)
    setRange((r) => ({ ...r, fromTs: (f.at - pad).toFixed(6), toTs: ((f.until || f.at) + pad).toFixed(6) }))
    inspectAt(f.at)
  }, [bundle, inspectAt])

  const s = bundle?.summary
  const span = s && s.lastTs > s.firstTs ? s.lastTs - s.firstTs : 0

  // Sources for the current stack selection, grouped so a multi-node pick is one click
  // per node rather than a hunt through every stack's nodes.
  const byStack = useMemo(() => {
    const m = new Map()
    for (const t of targets || []) {
      if (!m.has(t.stackId)) m.set(t.stackId, { name: t.stackName, id: t.stackId, nodes: [] })
      m.get(t.stackId).nodes.push(t)
    }
    return [...m.values()]
  }, [targets])

  return (
    <div className="space-y-4">
      {err && (
        <div className="flex items-start gap-2 rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          <Icon.Bell size={16} className="mt-0.5 shrink-0" />
          <span className="min-w-0 break-words">{err}</span>
          <button className="ml-auto shrink-0 text-xs underline" onClick={() => setErr('')}>dismiss</button>
        </div>
      )}

      <Card
        title="Logs"
        subtitle="Read several nodes' logs together. One node's log says what that node could see; three say what the cluster was doing."
        action={
          <Button variant="subtle" size="sm" onClick={() => setUploadOpen((v) => !v)} disabled={busy}>
            <Icon.External size={14} /> Upload logs
          </Button>
        }
      >
        {uploadOpen && (
          <UploadPanel
            files={files} setFiles={setFiles} busy={busy}
            onUpload={upload} onCancel={() => setUploadOpen(false)}
          />
        )}

        {byStack.length === 0 && targets !== null && (
          <p className="text-sm text-muted">
            No running MySQL, PostgreSQL, MongoDB or Valkey nodes — deploy a stack first, or upload logs above.
          </p>
        )}

        {byStack.map((st) => (
          <div key={st.id} className="mb-3">
            <div className="mb-1.5 flex items-center gap-2">
              <span className="text-xs font-semibold text-fg">{st.name}</span>
              <button
                className="text-[11px] text-muted underline hover:text-fg"
                onClick={() => {
                  const keys = st.nodes.map((n) => `${n.stackId}:${n.nodeId}`)
                  const all = keys.every((k) => picked.includes(k))
                  setPicked(all ? [] : keys)
                }}
              >
                {st.nodes.every((n) => picked.includes(`${n.stackId}:${n.nodeId}`)) ? 'none' : 'all'}
              </button>
            </div>
            <div className="flex flex-wrap gap-1.5">
              {st.nodes.map((n) => {
                const key = `${n.stackId}:${n.nodeId}`
                const on = picked.includes(key)
                return (
                  <button
                    key={key}
                    onClick={() => setPicked((p) => (on ? p.filter((x) => x !== key) : [...p, key]))}
                    className={`rounded-md border px-2 py-1 text-[11px] ${on ? 'border-primary bg-primary/10' : 'bg-bg hover:bg-surface2'}`}
                  >
                    {n.label} <span className="text-muted">· {ENGINE_LABEL[n.engine] || n.engine}</span>
                  </button>
                )
              })}
            </div>
          </div>
        ))}

        <div className="mt-3 flex flex-wrap items-end gap-3">
          <Field label="Lines per node" hint="tail -n · up to 200,000">
            <input type="number" min="100" max="200000" className={`${inputCls} w-32`} value={lines}
              onChange={(e) => setLines(e.target.value)} />
          </Field>
          <Button variant="primary" onClick={collect} disabled={busy || !picked.length}>
            <Icon.Search size={16} /> Read {picked.length || ''} log{picked.length === 1 ? '' : 's'}
          </Button>
          {picked.length > 1 && new Set(picked.map((p) => p.split(':')[0])).size > 1 && (
            <span className="text-[11px] text-warning">
              Nodes from more than one stack are selected; only the first stack&apos;s will be read.
            </span>
          )}
        </div>

        {bundles.length > 0 && (
          <div className="mt-3 flex flex-wrap gap-1.5">
            {bundles.map((b) => (
              <span key={b.id}
                className={`flex items-center gap-1.5 rounded-md border px-2 py-1 text-[11px] ${b.id === id ? 'border-primary bg-primary/10' : 'bg-bg'}`}>
                <button className="hover:underline" onClick={() => open(b.id)}>
                  {b.label} · {b.summary?.events || 0} events
                  {b.origin === 'upload' && ' · uploaded'}
                </button>
                <button className="text-muted hover:text-danger" title="Forget this bundle"
                  onClick={() => dropBundle(b.id)}>×</button>
              </span>
            ))}
          </div>
        )}
      </Card>

      {bundle && s && (
        <>
          <SourcesCard bundle={bundle} id={id} />
          <Verdict findings={findings} onGo={goToFinding} />

          <Card
            title="Cluster timeline"
            subtitle="One lane per node, filled with the state it was in. Click for a readout of every node at that instant; drag to narrow everything below."
            action={<Legend />}
          >
            <Swimlane
              timeline={timeline} sources={bundle.sources} first={s.firstTs}
              onPick={inspectAt}
              onSelect={(fromTs, toTs) => setRange((r) => ({ ...r, fromTs, toTs }))}
            />
            <RangeControls
              range={range} setRange={setRange} buckets={buckets} setBuckets={setBuckets}
              summary={s} span={span}
            />
            {snapshot && <Snapshot snap={snapshot} sources={bundle.sources} onClose={() => setSnapshot(null)} />}
          </Card>

          <div className="grid grid-cols-1 gap-4 xl:grid-cols-[minmax(0,1.6fr)_minmax(0,1fr)]">
            <Card
              title="Events"
              subtitle={`${page.matched.toLocaleString()} of ${s.events.toLocaleString()} match the current filters`}
            >
              <Filters range={range} setRange={setRange} summary={s} sources={bundle.sources} />
              <EventList
                events={page.events} sources={bundle.sources} first={s.firstTs}
                selectedNo={selected?.no} onSelect={setSelected}
              />
              <Pager page={page} onPage={(o) => reload(o)} />
            </Card>

            <div className="min-w-0 space-y-4 xl:sticky xl:top-4 xl:max-h-[calc(100vh-2rem)] xl:overflow-y-auto">
              <Card title="Record" subtitle={selected ? `Event #${selected.no}` : 'Select an event'}>
                {selected
                  ? <EventDetail e={selected} bundle={bundle} id={id} first={s.firstTs} />
                  : (
                    <div className="flex flex-col items-center justify-center gap-3 py-16 text-muted">
                      <Icon.Monitor size={40} className="opacity-40" />
                      <p className="text-sm">Select an event to read the record and what it means.</p>
                    </div>
                  )}
              </Card>
              <TopStrip summary={s} range={range} setRange={setRange} />
            </div>
          </div>
        </>
      )}
    </div>
  )
}

// ---------------------------------------------------------------- upload

export function UploadPanel({ files, setFiles, busy, onUpload, onCancel }) {
  return (
    <div className="mb-4 rounded-lg border bg-bg p-3">
      <Field label="Log files" hint="one per node · MySQL/PXC, PostgreSQL, MongoDB or Valkey — the format is read from the bytes, whatever the file is called">
        {/* No `accept`. The engine is detected from the bytes, never from the name, and the
            names logs actually arrive under defeat any extension list: a rotated log is
            mysqld.log.1 or error.log-20260814, and a file off a ticket comes back as
            "mysqld_node2_2026-08-14" with no extension at all. Every one of those was
            greyed out in the picker while the list was here. */}
        <FilePick
          id="log-upload" multiple
          file={files.length === 1 ? files[0] : null}
          onPick={(f) => setFiles(f ? [f] : [])}
          onPickMany={setFiles}
          placeholder={files.length > 1 ? `${files.length} files chosen` : 'Choose logs, or drop them here'}
        />
      </Field>
      {files.length > 1 && (
        <div className="mt-2">
          <ul className="space-y-0.5 text-[11px] text-muted">
            {files.map((f) => <li key={f.name}>{f.name} · {logBytes(f.size)}</li>)}
          </ul>
          <button type="button" onClick={() => setFiles([])}
            className="mt-1 text-[10px] text-muted underline hover:text-danger">
            clear
          </button>
        </div>
      )}
      <p className="mt-2 text-[11px] text-muted">
        Upload every member&apos;s log from the same period, not just the one that looks broken.
        A node reporting that a peer went inactive is telling you about its own network as much as
        about the peer, and the only way to tell those apart is the peer&apos;s own log for the
        same seconds.
      </p>
      <div className="mt-3 flex gap-2">
        <Button variant="primary" size="sm" onClick={onUpload} disabled={busy || !files.length}>
          Read {files.length || ''} log{files.length === 1 ? '' : 's'}
        </Button>
        <Button variant="subtle" size="sm" onClick={onCancel} disabled={busy}>Cancel</Button>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------- sources

export function SourcesCard({ bundle, id }) {
  const s = bundle.summary
  return (
    <Card
      title="Sources"
      subtitle={`${bundle.label} · ${logDateTime(s.firstTs, 0)} → ${logTimeOfDay(s.lastTs, 0)} · ${logDur(s.lastTs - s.firstTs)}`}
    >
      {bundle.note && (
        <div className="mb-2 rounded-lg border border-warning/40 bg-warning/10 px-3 py-2 text-xs text-warning">
          {bundle.note}
        </div>
      )}
      {s.disjoint && (
        <div className="mb-2 rounded-lg border border-status-crit/45 bg-status-crit/10 px-3 py-2 text-xs text-status-crit">
          <span className="font-semibold">No common period.</span> These logs do not overlap, so nothing
          in the timeline below can be compared across nodes — check each source&apos;s range in the table.
        </div>
      )}
      <div className="overflow-x-auto">
        <table className="w-full min-w-[640px] text-left text-xs">
          <thead className="text-[10px] uppercase tracking-wide text-muted">
            <tr className="border-b">
              <th className="py-1.5 pr-2">Node</th>
              <th className="py-1.5 pr-2">Source</th>
              <th className="py-1.5 pr-2">Covers</th>
              <th className="py-1.5 pr-2">Lines</th>
              <th className="py-1.5 pr-2">Events</th>
              <th className="py-1.5">Severity</th>
            </tr>
          </thead>
          <tbody>
            {bundle.sources.map((src) => (
              <tr key={src.idx} className="border-b last:border-0">
                <td className="py-1.5 pr-2">
                  <NodeChip src={src.idx} name={src.node} />
                  {FLAVOUR_LABEL[src.flavour] && (
                    <span className="ml-1.5 text-[10px] text-muted">{FLAVOUR_LABEL[src.flavour]}</span>
                  )}
                </td>
                <td className="py-1.5 pr-2 text-muted">
                  <span title={src.path || src.name}>{src.name}</span>
                  <span className="ml-1 text-[10px] opacity-70">{ENGINE_LABEL[src.engine] || src.engine}</span>
                </td>
                <td className="whitespace-nowrap py-1.5 pr-2 font-mono text-[10px] text-muted">
                  {logTimeOfDay(src.firstTs, 0)} → {logTimeOfDay(src.lastTs, 0)}
                </td>
                <td className="py-1.5 pr-2 text-muted">{src.lines.toLocaleString()}</td>
                <td className="py-1.5 pr-2">{src.events.toLocaleString()}</td>
                <td className="py-1.5">
                  <span className="flex flex-wrap gap-1.5">
                    {SEVS.filter((v) => src.counts?.[v]).map((v) => (
                      <span key={v} className={`text-[11px] ${SEV_TEXT[v]}`}>
                        {SEV_MARK[v]} {src.counts[v]}
                      </span>
                    ))}
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {bundle.sources.length > NODE_SLOTS && (
        <p className="mt-2 text-[11px] text-muted">
          Node colours run out after {NODE_SLOTS}: a sixth hue could not be separated from the
          first five by a colour-blind reader, so the extra sources share a neutral chip and are
          told apart by name.
        </p>
      )}
      <div className="mt-2 flex flex-wrap gap-2 text-[11px] text-muted">
        {bundle.sources.map((src) => (
          <a key={src.idx} className="underline hover:text-fg" href={logApi.rawURL(id, src.idx)}>
            download {src.name}
          </a>
        ))}
      </div>
    </Card>
  )
}

// ---------------------------------------------------------------- verdict

export function Verdict({ findings, onGo }) {
  if (!findings?.length) return null
  return (
    <Card title="Verdict" subtitle="What the logs add up to — worst first. Every line here rests on records you can open.">
      <div className="space-y-2">
        {findings.map((f) => (
          <div key={f.id} className={`rounded-lg border px-3 py-2 ${SEV_CARD[f.sev] || SEV_CARD.info}`}>
            <div className="flex flex-wrap items-baseline gap-2">
              <span className={`text-xs font-bold ${SEV_TEXT[f.sev]}`}>
                {SEV_MARK[f.sev]} {SEV_LABEL[f.sev]}
              </span>
              <span className="text-sm font-semibold text-fg">{f.title}</span>
              {f.at > 0 && (
                <button className="ml-auto text-[11px] text-muted underline hover:text-fg"
                  onClick={() => onGo(f)}>
                  {logTimeOfDay(f.at)} — show me
                </button>
              )}
            </div>
            <p className="mt-1 text-xs text-fg/90">{f.detail}</p>
            {f.advice && <p className="mt-1 text-xs text-muted">{f.advice}</p>}
          </div>
        ))}
      </div>
    </Card>
  )
}

// ---------------------------------------------------------------- timeline

export function Legend() {
  return (
    <div className="flex flex-wrap items-center gap-2 text-[10px] text-muted">
      {[['ok', 'serving'], ['warn', 'up, not serving'], ['bad', 'out of the cluster'], ['info', 'not stated']]
        .map(([sev, text]) => (
          <span key={sev} className="flex items-center gap-1">
            <span className={`inline-block h-2.5 w-2.5 rounded-sm ${SEV_FILL[sev]}`} />
            {text}
          </span>
        ))}
    </div>
  )
}

// Swimlane draws one lane per source: the state track as a filled bar, the event counts as
// ticks above it.
//
// The state track is the point. A row of event dots tells you something happened; a filled
// lane tells you what the node WAS during and between them, which is the only way to see at
// a glance that two members stayed green while a third went red for fifty seconds.
export function Swimlane({ timeline, sources, first, onSelect, onPick }) {
  const ref = useRef(null)
  // The drag lives in a ref as well as in state, and the ref is what mouseup reads.
  // State alone is a race: a fast click delivers mousedown and mouseup before React has
  // re-rendered, so the mouseup handler still closes over `drag === null` and the click
  // does nothing. It works when a human clicks slowly and fails under a synthetic click —
  // which is exactly the kind of bug that reaches users.
  const dragRef = useRef(null)
  const [drag, setDrag] = useState(null)

  const from = timeline?.fromTs ?? 0
  const to = timeline?.toTs ?? 0
  const width = to > from ? to - from : 1
  const pctOf = (e) => {
    const r = ref.current.getBoundingClientRect()
    return Math.min(100, Math.max(0, ((e.clientX - r.left) / r.width) * 100))
  }
  const tsAt = (pct) => from + (width * pct) / 100

  const onDown = (e) => {
    const p = pctOf(e)
    dragRef.current = { x0: p, x1: p }
    setDrag({ x0: p, x1: p })
  }
  const onMove = (e) => {
    if (!dragRef.current) return
    const p = pctOf(e)
    dragRef.current = { ...dragRef.current, x1: p }
    setDrag((d) => (d ? { ...d, x1: p } : d))
  }
  const cancel = () => { dragRef.current = null; setDrag(null) }
  const onUp = (e) => {
    const d = dragRef.current
    dragRef.current = null
    setDrag(null)
    if (!d || !(to > from)) return
    const lo = Math.min(d.x0, d.x1), hi = Math.max(d.x0, d.x1)
    // A click is an instant to inspect; a drag is a window to narrow to. Both are wanted,
    // and the threshold is what separates a wobbly click from a deliberate drag.
    if (hi - lo < 0.6) { onPick?.(tsAt(pctOf(e))); return }
    onSelect(tsAt(lo).toFixed(6), tsAt(hi).toFixed(6))
  }

  const maxCount = useMemo(() => Math.max(1, ...(timeline?.buckets || []).map((b) => b.count)), [timeline])

  if (!timeline) return <div className="h-40 animate-pulse rounded-lg bg-surface2" />

  const sel = drag ? { left: Math.min(drag.x0, drag.x1), width: Math.abs(drag.x1 - drag.x0) } : null
  const nBuckets = Math.max(1, (timeline.buckets || []).filter((b) => b.src === sources[0]?.idx).length)

  return (
    <div>
      <div
        ref={ref}
        onMouseDown={onDown} onMouseMove={onMove} onMouseUp={onUp} onMouseLeave={cancel}
        className="relative select-none space-y-1.5 overflow-hidden rounded-lg border bg-bg p-2"
        style={{ cursor: 'crosshair' }}
      >
        {sources.map((src) => {
          const phases = (timeline.phases || []).filter((p) => p.src === src.idx)
          const bks = (timeline.buckets || []).filter((b) => b.src === src.idx)
          return (
            <div key={src.idx} className={`border-l-2 pl-2 ${nodeEdge(src.idx)}`}>
              <div className="mb-0.5 flex items-center gap-2">
                <NodeChip src={src.idx} name={src.node} />
                {/* Only when it adds something: a log read from a node is named after the
                    node, and "pxc01 pxc01" is noise. An uploaded file often is not. */}
                {src.name !== src.node && <span className="text-[10px] text-muted">{src.name}</span>}
              </div>
              {/* the event ticks */}
              <div className="relative h-8">
                {bks.map((b) => {
                  if (!b.count) return null
                  const sev = b.bad ? 'bad' : b.warn ? 'warn' : b.ok ? 'ok' : 'info'
                  return (
                    <div key={b.i}
                      title={`${logTimeOfDay(b.ts)} · ${b.count} event(s): ${b.bad} bad, ${b.warn} warning, ${b.ok} good, ${b.info} background`}
                      className={`absolute bottom-0 ${SEV_FILL[sev]}`}
                      style={{
                        left: `${(b.i / nBuckets) * 100}%`,
                        width: `${100 / nBuckets}%`,
                        height: `${Math.max(12, (b.count / maxCount) * 100)}%`,
                      }}
                    />
                  )
                })}
              </div>
              {/* the state track */}
              <div className="relative h-4 overflow-hidden rounded-sm bg-surface2">
                {phases.map((p, i) => (
                  <div key={i}
                    title={`${p.state}${p.inferred ? ' (deduced — the log does not state it)' : ''} · ${logTimeOfDay(p.from, 0)} → ${logTimeOfDay(p.to, 0)}` +
                      `${p.members ? ` · ${p.members} member(s)` : ''}${p.primary ? `, primary = ${p.primary}` : ''}\n${STATE_TEXT[p.state] || ''}`}
                    // The server already decided this stripe's severity (lsStateSev) and
                    // sends it; STATE_SEV is the fallback for a phase that predates the
                    // field. Preferring p.sev keeps one source of truth — a state added to
                    // the Go catalogue and not to the JS table used to paint as 'info',
                    // which reads as a state nobody has an opinion about rather than a bad
                    // one.
                    className={`absolute inset-y-0 ${SEV_FILL[p.sev || STATE_SEV[p.state] || 'info']} ${p.inferred ? 'opacity-45' : ''}`}
                    style={{
                      left: `${((p.from - from) / width) * 100}%`,
                      width: `${Math.max(0.15, ((p.to - p.from) / width) * 100)}%`,
                    }}
                  />
                ))}
              </div>
            </div>
          )
        })}
        {sel && sel.width > 0.3 && (
          <div className="pointer-events-none absolute inset-y-0 border-x border-primary bg-primary/20"
            style={{ left: `${sel.left}%`, width: `${sel.width}%` }} />
        )}
      </div>
      <div className="mt-1 flex justify-between font-mono text-[10px] text-muted">
        <span title={logISO(from)}>{logTimeOfDay(from)} · +{(from - first).toFixed(3)}s</span>
        <span>{(timeline.matched || 0).toLocaleString()} events in window · click for a readout, drag to narrow</span>
        <span title={logISO(to)}>+{(to - first).toFixed(3)}s · {logTimeOfDay(to)}</span>
      </div>
    </div>
  )
}

// Snapshot is the answer to "what was the cluster doing at this exact moment", printed for
// every node side by side. Reconstructing this by eye from three interleaved logs is the
// tedious part of reading them, so it is one click.
export function Snapshot({ snap, sources, onClose }) {
  const name = (src) => sources.find((s) => s.idx === src)?.node || `source ${src}`
  return (
    <div className="mt-3 rounded-lg border bg-bg p-3">
      <div className="mb-2 flex flex-wrap items-baseline gap-2">
        <span className="text-xs font-semibold text-fg">At {logDateTime(snap.at)}</span>
        <span className="font-mono text-[10px] text-muted">{logISO(snap.at)}</span>
        {!snap.agree && (
          <span className="text-[11px] font-semibold text-status-crit">
            {SEV_MARK.bad} the nodes disagreed about the membership at this instant
          </span>
        )}
        <button className="ml-auto text-[11px] text-muted underline hover:text-fg" onClick={onClose}>close</button>
      </div>
      <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
        {snap.nodes.map((n) => (
          <div key={n.src} className={`rounded-lg border px-2.5 py-2 ${SEV_CARD[n.sev] || SEV_CARD.info}`}>
            <div className="flex flex-wrap items-center gap-2">
              <NodeChip src={n.src} name={name(n.src)} size="lg" />
              <span className={`text-xs font-bold ${SEV_TEXT[n.sev]}`}>{n.state}</span>
            </div>
            <div className="mt-0.5 text-[11px] text-muted">
              {n.members ? `${n.members} member(s)` : 'membership not stated'}
              {n.primary === 'yes' && ' · primary component'}
              {n.primary === 'no' && ' · NOT the primary component'}
            </div>
            <p className="mt-1 text-[11px] text-fg/85">{n.meaning || STATE_TEXT[n.state]}</p>
            {!n.covered && (
              <p className="mt-1 text-[11px] text-status-warn">
                This node&apos;s log does not cover this instant — the state shown is carried from the
                nearest record it does cover.
              </p>
            )}
          </div>
        ))}
      </div>
      {(snap.before || snap.after) && (
        <div className="mt-2 space-y-0.5 text-[11px] text-muted">
          {snap.before && (
            <div>
              <span className="text-fg/70">before:</span> {logTimeOfDay(snap.before.ts)}{' '}
              <span className={SEV_TEXT[snap.before.sev]}>{snap.before.label}</span> — {name(snap.before.src)}
            </div>
          )}
          {snap.after && (
            <div>
              <span className="text-fg/70">after:</span> {logTimeOfDay(snap.after.ts)}{' '}
              <span className={SEV_TEXT[snap.after.sev]}>{snap.after.label}</span> — {name(snap.after.src)}
            </div>
          )}
        </div>
      )}
    </div>
  )
}

export function RangeControls({ range, setRange, buckets, setBuckets, summary, span }) {
  const first = summary.firstTs
  const set = (patch) => setRange((r) => ({ ...r, ...patch }))
  const winFrom = range.fromTs ? Number(range.fromTs) - first : 0
  const winTo = range.toTs ? Number(range.toTs) - first : span

  const applyOffsets = (a, b) => {
    const lo = Math.max(0, Math.min(a, b))
    const hi = Math.min(span, Math.max(a, b))
    if (hi - lo <= 0) return
    set({ fromTs: (first + lo).toFixed(6), toTs: (first + hi).toFixed(6) })
  }
  const zoom = (factor) => {
    const mid = (winFrom + winTo) / 2
    const half = ((winTo - winFrom) * factor) / 2
    applyOffsets(mid - half, mid + half)
  }
  const pan = (dir) => {
    const w = winTo - winFrom
    applyOffsets(winFrom + dir * w * 0.5, winTo + dir * w * 0.5)
  }

  return (
    <div className="mt-3 flex flex-wrap items-center gap-1.5">
      <Button size="sm" variant="subtle" onClick={() => zoom(0.5)}>Zoom in</Button>
      <Button size="sm" variant="subtle" onClick={() => zoom(2)}>Zoom out</Button>
      <Button size="sm" variant="subtle" onClick={() => pan(-1)}>◀ Pan</Button>
      <Button size="sm" variant="subtle" onClick={() => pan(1)}>Pan ▶</Button>
      <Button size="sm" variant="subtle" onClick={() => set({ fromTs: '', toTs: '' })}>Whole window</Button>
      <label className="ml-2 flex items-center gap-1.5 text-xs text-muted">
        Resolution
        <select className={`${inputCls} w-20 py-1`} value={buckets} onChange={(e) => setBuckets(Number(e.target.value))}>
          {[60, 120, 180, 360, 720].map((n) => <option key={n} value={n}>{n}</option>)}
        </select>
      </label>
      <span className="ml-auto font-mono text-[10px] text-muted">window {logDur(winTo - winFrom)}</span>
    </div>
  )
}

// ---------------------------------------------------------------- filters

export function Filters({ range, setRange, summary, sources }) {
  const set = (patch) => setRange((r) => ({ ...r, ...patch }))
  const toggleSev = (v) =>
    set({ sev: range.sev.includes(v) ? range.sev.filter((x) => x !== v) : [...range.sev, v] })
  return (
    <div className="mb-3 space-y-2">
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="text-xs text-muted">Show:</span>
        {SEVS.map((v) => {
          const on = range.sev.length === 0 || range.sev.includes(v)
          return (
            <button key={v} onClick={() => toggleSev(v)}
              className={`rounded-md border px-2 py-0.5 text-[11px] ${on ? 'border-primary bg-primary/10' : 'bg-bg opacity-60 hover:opacity-100'}`}>
              <span className={SEV_TEXT[v]}>{SEV_MARK[v]}</span> {SEV_LABEL[v]}
              <span className="ml-1 text-muted">{(summary.counts?.[v] || 0).toLocaleString()}</span>
            </button>
          )
        })}
        {range.sev.length > 0 && (
          <button className="text-[11px] text-muted underline hover:text-fg" onClick={() => set({ sev: [] })}>
            all
          </button>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="text-xs text-muted">Node:</span>
        <button onClick={() => set({ src: -1 })}
          className={`rounded-md border px-2 py-0.5 text-[11px] ${range.src < 0 ? 'border-primary bg-primary/10' : 'bg-bg hover:bg-surface2'}`}>
          Every node
        </button>
        {sources.map((s) => (
          <button key={s.idx} onClick={() => set({ src: range.src === s.idx ? -1 : s.idx })}
            className={`rounded-md border px-1 py-0.5 ${range.src === s.idx ? 'border-primary bg-primary/10' : 'border-transparent hover:bg-surface2'}`}>
            <NodeChip src={s.idx} name={s.node} />
          </button>
        ))}
      </div>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <Field label="Kind">
          <select className={inputCls} value={range.class} onChange={(e) => set({ class: e.target.value })}>
            <option value="">Everything</option>
            {Object.entries(summary.classes || {}).sort((a, b) => b[1] - a[1]).map(([c, n]) => (
              <option key={c} value={c}>{CLASS_LABEL[c] || c} ({n})</option>
            ))}
          </select>
        </Field>
        <Field label="Search" hint="message, code, peer, or what it means">
          <input className={inputCls} placeholder="e.g. donor, SST, 4567" value={range.q}
            onChange={(e) => set({ q: e.target.value })} />
        </Field>
      </div>
    </div>
  )
}

// TopStrip is "what dominated this bundle", as clickable filters.
export function TopStrip({ summary, range, setRange }) {
  if (!summary.top?.length) return null
  return (
    <Card title="What happened most" subtitle="Worst first, then most frequent. Click to filter.">
      <div className="flex flex-wrap gap-1.5">
        {summary.top.map((t) => (
          <button key={`${t.label}-${t.class}`}
            onClick={() => setRange((r) => ({ ...r, q: r.q === t.label ? '' : t.label }))}
            className={`rounded-md border px-2 py-0.5 text-left text-[11px] ${range.q === t.label ? 'border-primary bg-primary/10' : 'bg-bg hover:bg-surface2'}`}>
            <span className={SEV_TEXT[t.sev]}>{SEV_MARK[t.sev]}</span> {t.label}
            <span className="ml-1 text-muted">×{t.count}</span>
          </button>
        ))}
      </div>
    </Card>
  )
}

// ---------------------------------------------------------------- events

export function EventList({ events, sources, first, selectedNo, onSelect }) {
  const name = (src) => sources.find((s) => s.idx === src)?.node || `#${src}`
  if (!events.length) {
    return <div className="py-10 text-center text-sm text-muted">No events match the current filters.</div>
  }
  return (
    <div className="space-y-1">
      {events.map((e) => (
        <button key={e.no} onClick={() => onSelect(e)}
          className={`block w-full rounded-md px-2 py-1.5 text-left transition hover:bg-surface2 ${SEV_ROW[e.sev] || SEV_ROW.info} ${e.no === selectedNo ? 'ring-1 ring-primary' : ''}`}>
          <div className="flex flex-wrap items-baseline gap-2 text-[11px]">
            <span className={`w-3 shrink-0 font-bold ${SEV_TEXT[e.sev]}`}>{SEV_MARK[e.sev]}</span>
            <span className="font-mono text-muted" title={logISO(e.ts)}>{logTimeOfDay(e.ts)}</span>
            <span className="font-mono text-[10px] text-muted opacity-60">+{(e.ts - first).toFixed(3)}s</span>
            <NodeChip src={e.src} name={name(e.src)} />
            <span className="font-medium text-fg">{e.label}</span>
            {e.repeat > 1 && (
              <span className="rounded bg-surface2 px-1 text-[10px] text-muted"
                title={`repeated until ${logTimeOfDay(e.endTs)}`}>
                ×{e.repeat} over {logDur(e.endTs - e.ts)}
              </span>
            )}
            {e.approx && <span className="text-[10px] text-muted" title="this record carried no timestamp of its own">≈</span>}
            {e.code && <span className="font-mono text-[10px] text-muted opacity-70">{e.code}</span>}
          </div>
          {e.message && e.message !== e.label && (
            <div className="mt-0.5 break-words pl-5 text-[11px] text-muted">{e.message}</div>
          )}
        </button>
      ))}
    </div>
  )
}

export function Pager({ page, onPage }) {
  const { matched, offset, limit = PAGE } = page
  if (matched <= limit) return null
  const from = offset + 1
  const to = Math.min(offset + limit, matched)
  return (
    <div className="mt-2 flex items-center justify-between text-xs text-muted">
      <span>{from.toLocaleString()}–{to.toLocaleString()} of {matched.toLocaleString()}</span>
      <div className="flex gap-1.5">
        <Button size="sm" variant="subtle" disabled={offset === 0} onClick={() => onPage(Math.max(0, offset - limit))}>Previous</Button>
        <Button size="sm" variant="subtle" disabled={to >= matched} onClick={() => onPage(offset + limit)}>Next</Button>
      </div>
    </div>
  )
}

// EventDetail shows the record, what it means, and the lines around it in the original
// file — because a classifier is a reading of a log and the reader has to be able to check
// it against the log itself.
export function EventDetail({ e, bundle, id, first }) {
  const [ctx, setCtx] = useState(null)
  const [loading, setLoading] = useState(false)
  const src = bundle.sources.find((s) => s.idx === e.src)

  useEffect(() => { setCtx(null) }, [e.no])

  async function showInFile() {
    setLoading(true)
    try { setCtx(await logApi.context(id, e.src, e.line, 12)) }
    catch { setCtx({ text: 'could not read the source file' }) }
    finally { setLoading(false) }
  }

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2 text-xs">
        <span className={`font-bold ${SEV_TEXT[e.sev]}`}>{SEV_MARK[e.sev]} {SEV_LABEL[e.sev]}</span>
        <NodeChip src={e.src} name={src?.node || `#${e.src}`} />
        <Badge tone="accent">{CLASS_LABEL[e.class] || e.class}</Badge>
        {e.subsystem && <span className="font-mono text-[10px] text-muted">[{e.subsystem}]</span>}
        {e.level && <span className="font-mono text-[10px] text-muted">[{e.level}]</span>}
      </div>

      <div className="text-sm font-semibold text-fg">{e.label}</div>
      {e.meaning && <p className="text-xs text-muted">{e.meaning}</p>}

      <div className="grid grid-cols-1 gap-1 rounded-lg border bg-bg px-3 py-2 font-mono text-[11px]">
        <Row k="at" v={logDateTime(e.ts)} />
        <Row k="utc" v={logISO(e.ts)} />
        <Row k="offset" v={`+${(e.ts - first).toFixed(3)} s into the window`} />
        {e.repeat > 1 ? <Row k="repeated" v={`${e.repeat}× until ${logTimeOfDay(e.endTs)}`} /> : null}
        {e.approx ? <Row k="note" v="this record carried no timestamp; it was placed from the record beside it" /> : null}
        <Row k="line" v={`${src?.name || 'source'}:${e.line}`} />
        {e.peer ? <Row k="peer" v={e.peer} /> : null}
        {e.state ? <Row k="state after" v={e.state} /> : null}
        {e.members ? <Row k="members" v={e.total ? `${e.members} of ${e.total}` : String(e.members)} /> : null}
        {e.primary ? <Row k="primary" v={e.primary} /> : null}
        {e.seqno ? <Row k="seqno" v={String(e.seqno)} /> : null}
      </div>

      <div>
        <h4 className="mb-1 text-xs font-semibold text-muted">Record</h4>
        <pre className="max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-md bg-surface2 p-2 font-mono text-[11px]">
          {e.message || e.label}
        </pre>
      </div>

      {e.detail && (
        <details>
          <summary className="cursor-pointer text-xs font-semibold text-muted">
            Continuation lines ({e.detail.split('\n').length})
          </summary>
          <pre className="mt-1 max-h-60 overflow-auto whitespace-pre rounded-md bg-surface2 p-2 font-mono text-[10px] leading-snug">
            {e.detail}
          </pre>
        </details>
      )}

      <div>
        {!ctx ? (
          <Button size="sm" variant="subtle" onClick={showInFile} disabled={loading}>
            {loading ? 'Reading…' : 'Show this in the file'}
          </Button>
        ) : (
          <div>
            <div className="mb-1 flex items-center justify-between text-[10px] text-muted">
              <span>{src?.name} lines {ctx.from}–{ctx.to}</span>
              <button className="underline hover:text-fg" onClick={() => setCtx(null)}>hide</button>
            </div>
            <pre className="max-h-72 overflow-auto whitespace-pre rounded-md bg-surface2 p-2 font-mono text-[10px] leading-snug">
              {ctx.text}
            </pre>
          </div>
        )}
      </div>
    </div>
  )
}

function Row({ k, v }) {
  return (
    <div className="flex gap-2">
      <span className="w-24 shrink-0 text-muted">{k}</span>
      <span className="min-w-0 break-words">{String(v)}</span>
    </div>
  )
}
