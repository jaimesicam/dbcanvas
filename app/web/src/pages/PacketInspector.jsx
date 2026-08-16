import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Card, Button, Badge, Field, inputCls } from '../components/ui.jsx'
import {
  pktApi, pktTargetKey, pktBytesFmt, PROTO_TONE, isSevereIssue, issueKind,
  PORT_ROLE_TEXT, ENGINE_LABEL, MONGO_KIND_TEXT, TIME_MODES, pktFormatTime, pktDateTime, pktISO, pktTimeOfDay,
} from '../lib/pktApi.js'

// Packet Inspector — run tcpdump on a provisioned MySQL or PostgreSQL node, then read
// the capture as decoded protocol: queries, responses, latency, and the network
// problems underneath (retransmissions, resets, zero windows, gaps).
//
// Both engines land in the same UI because the questions are the same, and only the
// vocabulary differs: MySQL answers with an error number, PostgreSQL with a SQLSTATE;
// a PXC member's cluster traffic is on Galera's three ports, a Patroni member's is on
// Patroni's REST API and etcd. The protocol column and the ports panel say which is
// which, and every filter, range and correlation works identically for both.
//
// The layout follows the Packet Reviewer mock: a Traffic Timeline over a packet list
// on the left, a deep-inspection panel on the right. The timeline is deliberately
// MORE configurable than the mock's drag-a-window: a range can be set by dragging,
// by typing packet numbers, by typing a time offset, with zoom/pan buttons, and with
// presets — and the server does the filtering and bucketing, so a 400k-packet capture
// stays responsive because the browser only ever holds one page of it.
//
// See docs/PACKET_INSPECTOR.md.

const DEFAULTS = { seconds: 20, packets: 50000, snaplen: 65535, filter: '', allPorts: false }
// Each protocol's own port, for the upload form's placeholder. Blank means the decoder
// sniffs the protocol first and then applies its default.
const DEFAULT_PORTS = { mysql: '3306', postgres: '5432', mongodb: '27017', valkey: '6379' }
const PAGE = 200

// A range is the single source of truth for what the list and the timeline show.
const emptyRange = () => ({
  fromNo: '', toNo: '', fromTs: '', toTs: '',
  stream: -1, proto: '', dir: '', issue: '', q: '',
})

export default function PacketInspector() {
  const [targets, setTargets] = useState(null)
  const [target, setTarget] = useState('')
  const [opts, setOpts] = useState(DEFAULTS)
  const [captures, setCaptures] = useState([])
  const [capId, setCapId] = useState('')
  const [cap, setCap] = useState(null)
  const [range, setRange] = useState(emptyRange())
  const [buckets, setBuckets] = useState(160)
  const [timeline, setTimeline] = useState(null)
  const [page, setPage] = useState({ packets: [], matched: 0, offset: 0, streams: [] })
  const [selected, setSelected] = useState(null) // {packet, stream, hex, bytes}
  const [srvLog, setSrvLog] = useState(null)
  const [timeMode, setTimeMode] = useState('relative')
  const [uploadOpen, setUploadOpen] = useState(false)
  const [uploadPort, setUploadPort] = useState('')
  // '' = work the protocol out from the bytes, which is what an upload usually wants:
  // whoever has a pcap has no reason to also state which product wrote it.
  const [uploadEngine, setUploadEngine] = useState('')
  // The instant a server-log record points at, so the packet list can tint its
  // neighbourhood the same way the log tints the selected packet's.
  const [logMarkTs, setLogMarkTs] = useState(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [capFile, setCapFile] = useState(null)
  const [logFile, setLogFile] = useState(null)

  useEffect(() => {
    pktApi.targets()
      .then((t) => {
        const list = Array.isArray(t) ? t : []
        setTargets(list)
        if (list.length && !target) setTarget(pktTargetKey(list[0]))
      })
      .catch((e) => { setErr(`Could not load targets: ${e.message}`); setTargets([]) })
    refreshCaptures()
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  const refreshCaptures = useCallback(() => pktApi.list()
    .then((l) => setCaptures(Array.isArray(l) ? l : []))
    .catch(() => {}), [])

  // Poll while a capture is running or decoding.
  const live = cap && (cap.state === 'capturing' || cap.state === 'decoding')
  useEffect(() => {
    if (!capId || !live) return
    const t = setInterval(async () => {
      try { setCap(await pktApi.get(capId)) } catch { /* keep last */ }
    }, 900)
    return () => clearInterval(t)
  }, [capId, live])

  // When a capture becomes ready, load its timeline and first page.
  useEffect(() => {
    if (cap?.state === 'ready') { refreshCaptures(); reload(0) }
  }, [cap?.state, cap?.id]) // eslint-disable-line react-hooks/exhaustive-deps

  // Reload whenever the range or bucket count changes.
  useEffect(() => {
    if (cap?.state === 'ready') reload(0)
  }, [JSON.stringify(range), buckets]) // eslint-disable-line react-hooks/exhaustive-deps

  const reload = useCallback(async (offset) => {
    if (!cap?.id) return
    const params = { ...range, stream: range.stream, buckets }
    try {
      const [tl, pg] = await Promise.all([
        pktApi.timeline(cap.id, params),
        pktApi.packets(cap.id, { ...params, limit: PAGE, offset }),
      ])
      setTimeline(tl)
      setPage({ ...pg, offset })
    } catch (e) { setErr(e.message) }
  }, [cap?.id, range, buckets])

  async function startCapture() {
    setErr(''); setBusy(true); setSelected(null); setTimeline(null)
    try {
      const [stackId, nodeId] = target.split(':')
      const c = await pktApi.start({
        stackId: Number(stackId), nodeId,
        seconds: Number(opts.seconds), packets: Number(opts.packets),
        snaplen: Number(opts.snaplen), filter: opts.filter, allPorts: opts.allPorts,
      })
      setRange(emptyRange())
      setCapId(c.id); setCap(c)
      refreshCaptures()
    } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  async function stopCapture() {
    try { setCap(await pktApi.stop(cap.id)) } catch (e) { setErr(e.message) }
  }

  async function openCapture(id) {
    setErr(''); setSelected(null); setTimeline(null); setRange(emptyRange())
    setCapId(id)
    try { setCap(await pktApi.get(id)) } catch (e) { setErr(e.message) }
  }

  async function upload() {
    if (!capFile) { setErr('choose a capture file to upload'); return }
    setErr(''); setBusy(true)
    try {
      const c = await pktApi.upload(capFile, Number(uploadPort) || 0, logFile, uploadEngine)
      await refreshCaptures()
      await openCapture(c.id)
      setUploadOpen(false)
      setCapFile(null)
      setLogFile(null)
    } catch (e2) { setErr(e2.message) } finally { setBusy(false) }
  }

  // The server's own error log, for the events no packet can carry.
  const loadServerLog = useCallback(async (all) => {
    if (!cap?.id) return
    try { setSrvLog(await pktApi.serverLog(cap.id, { all: all ? 1 : '' })) } catch (e) { setErr(e.message) }
  }, [cap?.id])

  useEffect(() => { if (cap?.state === 'ready') loadServerLog(false) },
    [cap?.state, cap?.id]) // eslint-disable-line react-hooks/exhaustive-deps

  // The reverse of "follow selection": a server-log record sends the packet list to the
  // moment it describes. The server finds the page — the range and the filters stay as
  // the user set them, only the paging moves.
  async function jumpToLogRecord(ts) {
    if (!cap?.id || !ts) return
    setErr(''); setLogMarkTs(ts)
    try {
      const pg = await pktApi.packets(cap.id, { ...range, buckets, limit: PAGE, around: ts.toFixed(6) })
      setPage({ ...pg, offset: pg.offset })
      if (pg.nearestNo) {
        await selectPacket(pg.nearestNo)
      } else {
        setErr('no packet near that log record is in the current range — widen the range or clear the filters')
      }
    } catch (e) { setErr(e.message) }
  }

  async function selectPacket(no) {
    try { setSelected(await pktApi.packet(cap.id, no)) } catch (e) { setErr(e.message) }
  }

  const s = cap?.summary
  const span = s && s.lastTs > s.firstTs ? s.lastTs - s.firstTs : 0

  return (
    <div className="space-y-4">
      {err && (
        <div className="flex items-start gap-2 rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          <Icon.Bell size={16} className="mt-0.5 shrink-0" /> <span className="min-w-0 break-words">{err}</span>
          <button className="ml-auto shrink-0 text-xs underline" onClick={() => setErr('')}>dismiss</button>
        </div>
      )}

      <Card
        title="Capture"
        subtitle="tcpdump runs inside the database node, on the interface carrying its stack address — the only place that sees what the server actually received."
        action={
          <Button variant="subtle" size="sm" onClick={() => setUploadOpen((v) => !v)} disabled={busy}>
            <Icon.External size={14} /> Upload a capture
          </Button>
        }
      >
        {uploadOpen && (
          <div className="mb-4 rounded-lg border bg-bg p-3">
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
              <Field label="Capture file" hint=".pcap · .pcapng · .cap">
                <FilePick id="pkt-upload-cap" accept=".pcap,.cap,.pcapng,.dmp"
                  file={capFile} onPick={setCapFile} placeholder="Choose a capture, or drop it here" />
              </Field>
              <Field label="Server log" hint="optional — MySQL or PostgreSQL, correlated with the timeline">
                <FilePick id="pkt-upload-log" accept=".log,.err,.txt,text/plain"
                  file={logFile} onPick={setLogFile} placeholder="Choose a log, or drop it here" />
              </Field>
              <Field label="Protocol" hint="detected from the bytes unless you say">
                <select className={inputCls} value={uploadEngine}
                  onChange={(e) => setUploadEngine(e.target.value)}>
                  <option value="">Detect automatically</option>
                  <option value="mysql">MySQL</option>
                  <option value="postgres">PostgreSQL</option>
                  <option value="mongodb">MongoDB</option>
                  <option value="valkey">Valkey</option>
                </select>
              </Field>
              <Field label="Server port"
                hint={`blank = ${DEFAULT_PORTS[uploadEngine] || 'the protocol default'}`}>
                <input type="number" min="1" max="65535" className={inputCls} value={uploadPort}
                  placeholder={DEFAULT_PORTS[uploadEngine] || 'detected'}
                  onChange={(e) => setUploadPort(e.target.value)} />
              </Field>
            </div>
            <p className="mt-2 text-[11px] text-muted">
              The server log is where aborted connections, DNS, TLS and listener failures live — a
              capture cannot contain them. Upload the server&apos;s log covering the same period and
              its records are shown against this capture&apos;s window. Give the port only if the
              traffic was not on the default one; the protocol is read out of the capture itself.
            </p>
            <div className="mt-3 flex gap-2">
              <Button variant="primary" size="sm" onClick={upload} disabled={busy || !capFile}>
                Upload and decode
              </Button>
              <Button variant="subtle" size="sm" onClick={() => setUploadOpen(false)} disabled={busy}>Cancel</Button>
            </div>
          </div>
        )}

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <Field label="Database node">
            <select className={inputCls} value={target} onChange={(e) => setTarget(e.target.value)}>
              <option value="">Select a provisioned MySQL, PostgreSQL, MongoDB or Valkey node…</option>
              {(targets || []).map((t) => (
                <option key={pktTargetKey(t)} value={pktTargetKey(t)}>
                  {t.label} · {t.stackName} ({ENGINE_LABEL[t.engine] || t.engine}, port {t.port})
                </option>
              ))}
            </select>
          </Field>
          <Field label="Duration (s)" hint="1–3600 (up to an hour)">
            <input type="number" min="1" max="3600" className={inputCls} value={opts.seconds}
              onChange={(e) => setOpts({ ...opts, seconds: e.target.value })} />
          </Field>
          <Field label="Max packets" hint="tcpdump -c · up to 100,000">
            <input type="number" min="100" max="100000" className={inputCls} value={opts.packets}
              onChange={(e) => setOpts({ ...opts, packets: e.target.value })} />
          </Field>
          <Field label="Snaplen" hint="tcpdump -s · 65535 = whole frame">
            <input type="number" min="64" className={inputCls} value={opts.snaplen}
              onChange={(e) => setOpts({ ...opts, snaplen: e.target.value })} />
          </Field>
        </div>

        <p className="mt-2 text-[11px] text-muted">
          A capture ends at whichever comes first: the duration, the packet ceiling, or
          192 MB on disk. On a busy server the packet ceiling is usually what stops it —
          100,000 packets can arrive in seconds, so for a long watch narrow the filter.
        </p>

        <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-[2fr_auto]">
          <Field label="Extra BPF filter" hint="ANDed with the port filter, e.g. host 172.29.0.5">
            <input className={`${inputCls} font-mono`} placeholder="(none)" value={opts.filter}
              onChange={(e) => setOpts({ ...opts, filter: e.target.value })} />
          </Field>
          <label className="flex items-end gap-2 pb-2 text-sm">
            <input type="checkbox" checked={opts.allPorts}
              onChange={(e) => setOpts({ ...opts, allPorts: e.target.checked })} />
            <span>All ports <span className="text-xs text-muted">(drop the port filter)</span></span>
          </label>
        </div>

        <div className="mt-4 flex flex-wrap items-center gap-2">
          {live ? (
            <Button variant="danger" onClick={stopCapture}>
              <Icon.Bell size={16} /> Stop {cap.state === 'capturing' ? 'capture' : 'and decode'}
            </Button>
          ) : (
            <Button variant="primary" onClick={startCapture} disabled={busy || !target}>
              <Icon.Search size={16} /> Start capture
            </Button>
          )}
          {cap && <CaptureState cap={cap} />}
          {cap?.state === 'ready' && (
            <a className="text-xs underline text-muted hover:text-fg" href={pktApi.downloadURL(cap.id)}>
              download .pcap
            </a>
          )}
          {!targets?.length && targets !== null && (
            <span className="text-xs text-muted">No running MySQL, PostgreSQL, MongoDB or Valkey nodes — deploy one first.</span>
          )}
        </div>

        {cap?.command && (
          <pre className="mt-3 overflow-x-auto rounded-md bg-surface2 px-2 py-1.5 font-mono text-[11px] text-muted">{cap.command}</pre>
        )}
        {cap?.engine === 'mongodb' && cap?.summary?.protos && (
          <div className="mt-2 rounded-lg border bg-bg px-3 py-2 text-[11px]">
            <div className="mb-1 text-muted">
              Every MongoDB process listens on {cap.port} — mongod, mongos and the config servers alike —
              so this capture&apos;s connections are told apart by what is in them, not by port:
            </div>
            <div className="space-y-0.5">
              {Object.keys(cap.summary.protos)
                .filter((pr) => pr.startsWith('MongoDB'))
                .sort()
                .map((pr) => {
                  const kind = pr === 'MongoDB' ? 'client' : pr.slice('MongoDB/'.length)
                  return (
                    <div key={pr} className="flex gap-2">
                      <span className="w-36 shrink-0 font-mono text-fg">{pr}</span>
                      <span className="text-muted">{MONGO_KIND_TEXT[kind] || kind}</span>
                    </div>
                  )
                })}
            </div>
          </div>
        )}
        {cap?.ports && Object.keys(cap.ports).length > 1 && (
          <div className="mt-2 rounded-lg border bg-bg px-3 py-2 text-[11px]">
            <div className="mb-1 text-muted">
              {cap.nodeType === 'pxc' || cap.nodeType === 'mariadbgalera'
                ? 'A Galera cluster member speaks four protocols on four ports — all four are captured:'
                : cap.nodeType === 'patroni'
                  ? 'A Patroni member speaks PostgreSQL, its own REST API and etcd — all of them are captured, because a failover is decided in etcd rather than in PostgreSQL:'
                  : cap.nodeType === 'valkeycluster'
                    ? 'A clustered Valkey node speaks RESP on its client port and a binary gossip bus on that port + 10000, where failure detection and failover votes happen:'
                    : 'Ports covered by this capture:'}
            </div>
            <div className="space-y-0.5">
              {Object.entries(cap.ports).sort((a, b) => Number(a[0]) - Number(b[0])).map(([port, role]) => (
                <div key={port} className="flex gap-2">
                  <span className="w-14 shrink-0 font-mono text-fg">{port}</span>
                  <span className="text-muted">{PORT_ROLE_TEXT[role] || role}</span>
                </div>
              ))}
            </div>
          </div>
        )}

        {captures.length > 1 && (
          <div className="mt-3 flex flex-wrap gap-1.5">
            {captures.map((c) => (
              <button key={c.id}
                onClick={() => openCapture(c.id)}
                className={`rounded-md border px-2 py-1 text-[11px] ${c.id === capId ? 'border-primary bg-primary/10' : 'bg-bg hover:bg-surface2'}`}>
                {c.label || c.id} · {c.summary?.packets || 0} pkts
                {c.source === 'upload' && ' · uploaded'}
              </button>
            ))}
          </div>
        )}
      </Card>

      {cap?.state === 'ready' && s && (
        <>
          <SummaryStrip cap={cap} range={range} setRange={setRange} />

          <div className="grid grid-cols-1 gap-4 xl:grid-cols-[minmax(0,1.55fr)_minmax(0,1fr)]">
            <div className="min-w-0 space-y-4">
              <Card
                title="Traffic Timeline"
                subtitle="Drag on the strip to select a window. Every control below narrows what the list shows — the server filters, so this stays fast on a 400k-packet capture."
                action={
                  <span className="text-xs text-muted">
                    showing {page.matched.toLocaleString()} of {s.packets.toLocaleString()} packets
                  </span>
                }
              >
                <Timeline timeline={timeline} first={s.firstTs} onSelect={(fromTs, toTs) =>
                  setRange((r) => ({ ...r, fromTs, toTs, fromNo: '', toNo: '' }))} />
                <RangeControls
                  range={range} setRange={setRange} buckets={buckets} setBuckets={setBuckets}
                  summary={s} timeline={timeline} span={span}
                />
                <Filters range={range} setRange={setRange} summary={s} streams={page.streams || []} />
              </Card>

              <Card
                title="Packets"
                subtitle={`${page.matched.toLocaleString()} match the current range`}
                action={
                  <label className="flex items-center gap-1.5 text-xs text-muted">
                    Time
                    <select className={`${inputCls} w-52 py-1`} value={timeMode}
                      onChange={(e) => setTimeMode(e.target.value)}>
                      {TIME_MODES.map((m) => <option key={m.v} value={m.v}>{m.label}</option>)}
                    </select>
                  </label>
                }
              >
                <PacketList
                  packets={page.packets} first={s.firstTs} timeMode={timeMode}
                  selectedNo={selected?.packet?.no} onSelect={selectPacket} markTs={logMarkTs}
                />
                <Pager page={page} onPage={(o) => reload(o)} />
              </Card>
            </div>

            <div className="min-w-0 space-y-4 xl:sticky xl:top-4 xl:max-h-[calc(100vh-2rem)] xl:overflow-y-auto">
              <Card title="Packet Inspection" subtitle={selected ? `Frame #${selected.packet.no}` : 'Select a packet'}>
                {selected
                  ? <PacketDetails d={selected} first={s.firstTs} />
                  : (
                    <div className="flex flex-col items-center justify-center gap-3 py-16 text-muted">
                      <Icon.Monitor size={40} className="opacity-40" />
                      <p className="text-sm">Select a packet to view deep inspection details.</p>
                    </div>
                  )}
              </Card>

              {srvLog && (
                <ServerLogCard log={srvLog} onReload={loadServerLog}
                  selectedTs={selected?.packet?.ts} selectedNo={selected?.packet?.no}
                  onPick={jumpToLogRecord} />
              )}
            </div>
          </div>
        </>
      )}
    </div>
  )
}

// dropEntries pulls the FileSystemEntry of each dropped item out of a DataTransfer.
//
// Synchronous on purpose, and it must stay that way: the DataTransfer is emptied as soon as
// the drop handler yields, so an `await` before this line loses the drop.
function dropEntries(dt) {
  return [...(dt?.items || [])]
    .map((i) => (i.webkitGetAsEntry ? i.webkitGetAsEntry() : null))
    .filter((e) => e && e.isDirectory)
}

// walkEntries flattens dropped directories into their files.
async function walkEntries(entries) {
  const out = []
  const walk = async (entry) => {
    if (entry.isFile) {
      out.push(await new Promise((res, rej) => entry.file(res, rej)))
      return
    }
    // readEntries hands back at most a hundred at a time and signals the end with an empty
    // batch — a single call would silently truncate a long-running node's directory.
    const reader = entry.createReader()
    for (;;) {
      const batch = await new Promise((res, rej) => reader.readEntries(res, rej))
      if (!batch.length) break
      for (const e of batch) await walk(e)
    }
  }
  for (const e of entries) await walk(e)
  return out
}

// FilePick is a file input that looks like something you can click.
//
// A bare <input type="file"> renders as browser chrome — "Choose File / No file chosen" in
// a system font, with no border and no affordance — which in this UI reads as static text
// rather than a control. The native input is kept (it is the only way to open a file
// dialog, and screen readers and keyboards depend on it) but moved out of sight with
// `sr-only`, while a label styled as a dashed drop target does the talking. The label's
// `htmlFor` is what makes clicking anywhere in the box open the dialog, and the input's
// focus ring is mirrored onto the box with `peer-focus-visible` so tabbing to it is still
// visible.
//
// `multiple` + `onPickMany` are for the Log Summary, which takes one file per node in a
// single pick: choosing three members' logs one dialog at a time would make the comparison
// the user's chore. Single-file callers are untouched — they pass neither, and get the same
// behaviour as before.
//
// `directory` is for the FTDC Summary, where the artefact IS a directory — diagnostic.data,
// whose files are named metrics.<timestamp> and mean nothing one at a time. It puts the
// input in directory mode and makes a dropped folder walk itself rather than arrive as one
// zero-byte entry the reader can do nothing with.
export function FilePick({ id, accept, file, onPick, onPickMany, placeholder, multiple = false, directory = false }) {
  const [over, setOver] = useState(false)
  const take = (list) => {
    const files = [...(list || [])]
    if (multiple && onPickMany) onPickMany(files)
    else onPick(files[0] || null)
  }
  return (
    <div>
      <input
        id={id} type="file" accept={accept} multiple={multiple || directory} className="peer sr-only"
        {...(directory ? { webkitdirectory: '', directory: '' } : {})}
        onChange={(e) => take(e.target.files)}
      />
      <label
        htmlFor={id}
        onDragOver={(e) => { e.preventDefault(); setOver(true) }}
        onDragLeave={() => setOver(false)}
        onDrop={(e) => {
          e.preventDefault()
          setOver(false)
          // A dropped folder is in `items` as an entry to walk; `files` holds only the
          // folder itself, zero bytes and unreadable. Entries have to be taken out of the
          // DataTransfer synchronously, before the first await, or the browser empties it.
          const entries = directory ? dropEntries(e.dataTransfer) : []
          if (entries.length) { walkEntries(entries).then((fs) => { if (fs.length) take(fs) }); return }
          if (e.dataTransfer?.files?.length) take(e.dataTransfer.files)
        }}
        className={'flex cursor-pointer items-center gap-2 rounded-lg border border-dashed px-3 py-2 text-xs transition ' +
          'hover:border-primary hover:bg-primary/5 peer-focus-visible:ring-2 peer-focus-visible:ring-primary ' +
          (over ? 'border-primary bg-primary/10 ' : file ? 'border-primary/50 bg-primary/5 ' : 'bg-bg ')}
      >
        <Icon.External size={14} className="shrink-0 opacity-70" />
        {file ? (
          <>
            <span className="min-w-0 flex-1 truncate font-medium" title={file.name}>{file.name}</span>
            <span className="shrink-0 text-[10px] text-muted">{pktBytesFmt(file.size)}</span>
          </>
        ) : (
          <span className="flex-1 text-muted">{placeholder}</span>
        )}
      </label>
      {file && (
        <button type="button" onClick={() => (multiple && onPickMany ? onPickMany([]) : onPick(null))}
          className="mt-1 text-[10px] text-muted underline hover:text-danger">
          remove
        </button>
      )}
    </div>
  )
}

// ---------------------------------------------------------------- capture state

export function CaptureState({ cap }) {
  const tone = { capturing: 'primary', decoding: 'primary', ready: 'muted', error: 'danger', stopped: 'warning' }[cap.state] || 'muted'
  return (
    <div className="flex flex-wrap items-center gap-2 text-xs text-muted">
      <Badge tone={tone}>{cap.state}</Badge>
      {/* Which protocol the capture was read as. It matters most for an upload, where
          nobody chose it and the decoder worked it out from the bytes. */}
      {cap.engine && <Badge tone="accent">{ENGINE_LABEL[cap.engine] || cap.engine}</Badge>}
      {cap.iface && <span>iface <span className="font-mono text-fg">{cap.iface}</span></span>}
      {cap.state === 'capturing' && <span>{pktBytesFmt(cap.bytes)} captured</span>}
      {cap.sizeCapped && <span className="text-warning">stopped at the 192 MB size limit</span>}
      {cap.nodePackets > 0 && <span>{cap.nodePackets.toLocaleString()} packets on node</span>}
      {cap.kernelDropped > 0 && <span className="text-warning">{cap.kernelDropped} dropped by kernel</span>}
      {cap.error && <span className="text-danger">{cap.error}</span>}
    </div>
  )
}

// SummaryStrip is the headline: counts, and the issue kinds as clickable filters.
export function SummaryStrip({ cap, range, setRange }) {
  const s = cap.summary
  const stat = (label, value, tone) => (
    <div className="rounded-lg border bg-bg px-3 py-2">
      <div className="text-[10px] uppercase tracking-wide text-muted">{label}</div>
      <div className={`text-lg font-semibold ${tone || ''}`}>{value}</div>
    </div>
  )
  return (
    <Card
      title="Summary"
      subtitle={`${cap.label}${cap.stackName ? ` · ${cap.stackName}` : ''} · ${s.format} · link type ${s.linkType}`}
      action={
        s.firstTs ? (
          <span className="text-right text-[11px] leading-tight text-muted">
            <span className="block font-mono">{pktDateTime(s.firstTs, 3)}</span>
            <span className="block font-mono opacity-70">
              → {pktTimeOfDay(s.lastTs, 3)} · {(s.lastTs - s.firstTs).toFixed(3)}s
            </span>
          </span>
        ) : null
      }
    >
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-6">
        {stat('Packets', s.packets.toLocaleString())}
        {stat('Bytes', pktBytesFmt(s.bytes))}
        {stat('Connections', s.streams)}
        {stat('Queries', s.queries.toLocaleString())}
        {stat(`${ENGINE_LABEL[cap.engine] || 'Server'} errors`, s.errors, s.errors ? 'text-danger' : '')}
        {stat('TLS streams', s.tlsStreams, s.tlsStreams ? 'text-warning' : '')}
      </div>
      {/* The protocol mix, as filters. On a cluster member this is the fastest way to
          see how much of a capture is health checks and lock renewals rather than
          database traffic — a 20-second capture of a Patroni leader held 596 etcd raft
          frames and 55 Patroni REST frames, and being able to click either away is
          what makes the remaining 18 000 legible. */}
      {Object.keys(s.protos || {}).length > 1 && (
        <div className="mt-3 flex flex-wrap items-center gap-1.5">
          <span className="text-xs text-muted">Protocols:</span>
          {Object.entries(s.protos).sort((a, b) => b[1] - a[1]).map(([proto, n]) => (
            <button key={proto} onClick={() => setRange((r) => ({ ...r, proto: r.proto === proto ? '' : proto }))}
              className={`rounded-md border px-2 py-0.5 text-[11px] ${range.proto === proto ? 'border-primary bg-primary/10' : 'bg-bg hover:bg-surface2'}`}>
              {proto} · {n.toLocaleString()}
            </button>
          ))}
        </div>
      )}
      {(s.issueTop || []).length > 0 && (
        <div className="mt-3 flex flex-wrap items-center gap-1.5">
          <span className="text-xs text-muted">Issues found:</span>
          {s.issueTop.map((i) => (
            <button key={i.kind} onClick={() => setRange((r) => ({ ...r, issue: r.issue === i.kind ? '' : i.kind }))}
              className={`rounded-md border px-2 py-0.5 text-[11px] ${range.issue === i.kind ? 'border-primary bg-primary/10' : 'bg-bg hover:bg-surface2'} ${isSevereIssue(i.kind) ? 'text-danger' : 'text-warning'}`}>
              {i.kind} · {i.count}
            </button>
          ))}
        </div>
      )}
      {s.tlsStreams > 0 && (
        <div className="mt-3 rounded-lg border border-warning/40 bg-warning/10 px-3 py-2 text-xs">
          <span className="font-medium text-warning">
            {s.tlsStreams} connection(s) are encrypted, so their payload is not readable here.
          </span>
          {cap.engine === 'valkey' ? (
            <span className="mt-1 block text-muted">
              Sizes, timing and every TCP-level problem are still accurate; only the commands are
              not available. Valkey&apos;s TLS is a separate port (<code className="font-mono">tls-port</code>)
              with no in-band upgrade, so a capture of it is opaque from the first byte. For the
              commands: capture the plaintext port for the diagnostic window, or read them from
              the server&apos;s own <code className="font-mono">SLOWLOG</code> or a
              <code className="font-mono"> MONITOR</code> stream — remembering that MONITOR makes the
              server serialise every command a second time.
            </span>
          ) : cap.engine === 'mongodb' ? (
            <span className="mt-1 block text-muted">
              Sizes, timing and every TCP-level problem are still accurate; only the commands are
              not available. MongoDB has no in-band upgrade — TLS either starts the connection or
              never happens — so a capture of a TLS-enabled member is opaque from the first byte.
              For the commands themselves: capture with <code className="font-mono">tls=false</code> on the
              client for the diagnostic window, or read them from the server&apos;s own log
              (<code className="font-mono">Slow query</code> records carry the whole command) or the
              profiler (<code className="font-mono">system.profile</code>).
            </span>
          ) : cap.engine === 'postgres' ? (
            <span className="mt-1 block text-muted">
              Sizes, timing and every TCP-level problem are still accurate; only the statements are
              not available. psql and libpq default to
              <code className="font-mono"> sslmode=prefer</code>, so every connection to a server with
              <code className="font-mono"> ssl=on</code> is encrypted without anyone choosing it. For the
              statements themselves: capture again with
              <code className="font-mono"> PGSSLMODE=disable</code> for the diagnostic window, or read them
              from the server&apos;s own log (<code className="font-mono">log_statement</code>) or
              <code className="font-mono"> pg_stat_statements</code>.
            </span>
          ) : (
            <span className="mt-1 block text-muted">
              Sizes, timing and every TCP-level problem are still accurate; only the statements are
              not available. MySQL 8 clients use TLS by default
              (<code className="font-mono">--ssl-mode=PREFERRED</code>), and
              <code className="font-mono"> caching_sha2_password</code> refuses to send a password in the clear
              at all — <code className="font-mono">--ssl-mode=DISABLED</code> alone fails with ERROR 2061. For the
              statements themselves: capture again with an account that allows a cleartext password
              (<code className="font-mono">mysql_native_password</code>, or
              <code className="font-mono"> --get-server-public-key</code>), or read them from the server&apos;s
              own general log / <code className="font-mono">performance_schema</code>.
            </span>
          )}
        </div>
      )}
      {(s.dropped > 0 || s.truncated > 0 || cap.kernelDropped > 0) && (
        <p className="mt-2 text-[11px] text-warning">
          {cap.kernelDropped > 0 && `${cap.kernelDropped} packets dropped by the kernel (capture buffer too small for this load). `}
          {s.truncated > 0 && `${s.truncated} frames were cut short by the snaplen. `}
          {s.dropped > 0 && `${s.dropped} packets past the decode limit were not decoded.`}
        </p>
      )}
    </Card>
  )
}

// ---------------------------------------------------------------- timeline

// Timeline draws the density strip and lets a drag select a time window. Bars are
// coloured by the worst thing in their bucket, so a burst of retransmissions or
// errors is visible before anything is filtered.
export function Timeline({ timeline, first, onSelect }) {
  const ref = useRef(null)
  const [drag, setDrag] = useState(null) // {x0,x1}

  const buckets = timeline?.buckets || []
  const max = useMemo(() => Math.max(1, ...buckets.map((b) => b.count)), [buckets])

  const pctOf = (e) => {
    const r = ref.current.getBoundingClientRect()
    return Math.min(100, Math.max(0, ((e.clientX - r.left) / r.width) * 100))
  }

  const onDown = (e) => { const p = pctOf(e); setDrag({ x0: p, x1: p }) }
  const onMove = (e) => { if (drag) setDrag((d) => ({ ...d, x1: pctOf(e) })) }
  const onUp = () => {
    if (!drag || !timeline) return setDrag(null)
    const lo = Math.min(drag.x0, drag.x1), hi = Math.max(drag.x0, drag.x1)
    setDrag(null)
    if (hi - lo < 0.6) return // a click, not a drag
    const from = timeline.fromTs, to = timeline.toTs
    if (!(to > from)) return
    onSelect((from + ((to - from) * lo) / 100).toFixed(6), (from + ((to - from) * hi) / 100).toFixed(6))
  }

  if (!timeline) return <div className="h-24 animate-pulse rounded-lg bg-surface2" />

  const sel = drag ? { left: Math.min(drag.x0, drag.x1), width: Math.abs(drag.x1 - drag.x0) } : null
  const t0 = timeline.fromTs - first
  const t1 = timeline.toTs - first

  return (
    <div>
      <div
        ref={ref}
        onMouseDown={onDown} onMouseMove={onMove} onMouseUp={onUp} onMouseLeave={onUp}
        className="relative h-24 cursor-crosshair select-none overflow-hidden rounded-lg border bg-bg"
      >
        {buckets.map((b, i) => {
          const tone = b.errors ? 'bg-danger' : b.warnings ? 'bg-warning' : b.queries ? 'bg-primary' : 'bg-fg/25'
          return (
            <div key={i}
              title={`${pktTimeOfDay(b.ts, 3)} · ${b.count} packets, ${b.queries} queries, ${b.errors} errors · #${b.firstNo}–${b.lastNo}`}
              className={`absolute bottom-0 ${tone}`}
              style={{
                left: `${(i / buckets.length) * 100}%`,
                width: `${100 / buckets.length}%`,
                height: `${Math.max(b.count ? 4 : 0, (b.count / max) * 100)}%`,
                opacity: b.count ? 1 : 0,
              }}
            />
          )
        })}
        {sel && sel.width > 0.3 && (
          <div className="absolute inset-y-0 border-x border-primary bg-primary/20"
            style={{ left: `${sel.left}%`, width: `${sel.width}%` }} />
        )}
        {buckets.length === 0 && (
          <div className="flex h-full items-center justify-center text-xs text-muted">No packets in this range.</div>
        )}
      </div>
      <div className="mt-1 flex justify-between font-mono text-[10px] text-muted">
        <span title={pktISO(timeline.fromTs)}>
          {pktTimeOfDay(timeline.fromTs, 3)} · +{t0.toFixed(3)}s · #{timeline.fromNo}
        </span>
        <span>{timeline.total.toLocaleString()} packets in window</span>
        <span title={pktISO(timeline.toTs)}>
          #{timeline.toNo} · +{t1.toFixed(3)}s · {pktTimeOfDay(timeline.toTs, 3)}
        </span>
      </div>
    </div>
  )
}

// RangeControls is the "more configurable" part: exact packet numbers, exact time
// offsets, zoom, pan, presets and bucket resolution — none of which the drag-only
// mock could express.
export function RangeControls({ range, setRange, buckets, setBuckets, summary, timeline, span }) {
  const first = summary.firstTs
  const set = (patch) => setRange((r) => ({ ...r, ...patch }))

  // Current window in seconds-from-start, defaulting to the whole capture.
  const winFrom = range.fromTs ? Number(range.fromTs) - first : 0
  const winTo = range.toTs ? Number(range.toTs) - first : span

  const applyOffsets = (from, to) => {
    const lo = Math.max(0, Math.min(from, to))
    const hi = Math.min(span, Math.max(from, to))
    if (hi - lo <= 0) return
    set({ fromTs: (first + lo).toFixed(6), toTs: (first + hi).toFixed(6), fromNo: '', toNo: '' })
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
  const lastSeconds = (n) => applyOffsets(Math.max(0, span - n), span)

  return (
    <div className="mt-3 space-y-3 rounded-lg border bg-bg p-3">
      <div className="flex flex-wrap items-end gap-3">
        <Field label="From packet #">
          <input type="number" min="1" className={`${inputCls} w-28`} placeholder="1" value={range.fromNo}
            onChange={(e) => set({ fromNo: e.target.value, fromTs: '', toTs: '' })} />
        </Field>
        <Field label="To packet #">
          <input type="number" min="1" className={`${inputCls} w-28`} placeholder={String(summary.packets)} value={range.toNo}
            onChange={(e) => set({ toNo: e.target.value, fromTs: '', toTs: '' })} />
        </Field>
        <Field label="From +s" hint="seconds into the capture">
          <input type="number" step="0.001" min="0" className={`${inputCls} w-28`} placeholder="0"
            value={range.fromTs ? (Number(range.fromTs) - first).toFixed(3) : ''}
            onChange={(e) => applyOffsets(Number(e.target.value || 0), winTo)} />
        </Field>
        <Field label="To +s">
          <input type="number" step="0.001" min="0" className={`${inputCls} w-28`} placeholder={span.toFixed(3)}
            value={range.toTs ? (Number(range.toTs) - first).toFixed(3) : ''}
            onChange={(e) => applyOffsets(winFrom, Number(e.target.value || span))} />
        </Field>
        <Field label="Resolution" hint="timeline buckets">
          <select className={`${inputCls} w-24`} value={buckets} onChange={(e) => setBuckets(Number(e.target.value))}>
            {[40, 80, 160, 320, 640].map((n) => <option key={n} value={n}>{n}</option>)}
          </select>
        </Field>
      </div>

      <div className="flex flex-wrap items-center gap-1.5">
        <Button size="sm" variant="subtle" onClick={() => zoom(0.5)}>Zoom in</Button>
        <Button size="sm" variant="subtle" onClick={() => zoom(2)}>Zoom out</Button>
        <Button size="sm" variant="subtle" onClick={() => pan(-1)}>◀ Pan</Button>
        <Button size="sm" variant="subtle" onClick={() => pan(1)}>Pan ▶</Button>
        <span className="mx-1 text-xs text-muted">presets:</span>
        {[1, 5, 10].filter((n) => n < span).map((n) => (
          <Button key={n} size="sm" variant="subtle" onClick={() => lastSeconds(n)}>last {n}s</Button>
        ))}
        <Button size="sm" variant="subtle" onClick={() => set({ fromNo: '', toNo: '', fromTs: '', toTs: '' })}>
          Whole capture
        </Button>
        {timeline && (
          <span className="ml-auto font-mono text-[10px] text-muted">
            window {(winTo - winFrom).toFixed(3)}s
          </span>
        )}
      </div>
    </div>
  )
}

// Filters narrows by connection, protocol, direction, issue kind and free text.
export function Filters({ range, setRange, summary, streams }) {
  const set = (patch) => setRange((r) => ({ ...r, ...patch }))
  return (
    <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-5">
      <Field label="Connection">
        <select className={inputCls} value={range.stream} onChange={(e) => set({ stream: Number(e.target.value) })}>
          <option value={-1}>All connections</option>
          {streams.map((st) => (
            <option key={st.index} value={st.index}>
              #{st.index} {st.client} {st.tls ? '· TLS' : ''}{st.user ? ` · ${st.user}` : ''}
            </option>
          ))}
        </select>
      </Field>
      <Field label="Protocol">
        <select className={inputCls} value={range.proto} onChange={(e) => set({ proto: e.target.value })}>
          <option value="">All</option>
          {Object.keys(summary.protos || {}).sort().map((p) => (
            <option key={p} value={p}>{p} ({summary.protos[p]})</option>
          ))}
        </select>
      </Field>
      <Field label="Direction">
        <select className={inputCls} value={range.dir} onChange={(e) => set({ dir: e.target.value })}>
          <option value="">Both</option>
          <option value="c2s">Client → server</option>
          <option value="s2c">Server → client</option>
        </select>
      </Field>
      <Field label="Issues">
        <select className={inputCls} value={range.issue} onChange={(e) => set({ issue: e.target.value })}>
          <option value="">Any packet</option>
          <option value="any">Only packets with issues</option>
          {(summary.issueTop || []).map((i) => <option key={i.kind} value={i.kind}>{i.kind}</option>)}
        </select>
      </Field>
      <Field label="Search" hint="info, SQL, address">
        <input className={inputCls} placeholder="e.g. SELECT, 1054, 172.29" value={range.q}
          onChange={(e) => set({ q: e.target.value })} />
      </Field>
    </div>
  )
}

// ServerLogCard shows the server's error-log records around the capture's window, and
// keeps them lined up with whichever packet is selected.
//
// This is not decoration: MySQL's aborted-connection notes, DNS failures, TLS and
// listener errors are written to the log and sent to nobody, so a capture cannot contain
// them however long it runs. The counters matter for the same reason — a server can abort
// thousands of connections while logging none of them, depending on log_error_verbosity
// and on whether the disconnect produced a read/write error.
//
// Following the selection is what makes the pane a correlation tool rather than a second
// log viewer: pick a reset in the packet list, and the note the server wrote about that
// connection is scrolled to and highlighted. It can be switched off, because reading the
// log on its own is also legitimate and having it jump under you would be maddening.
export function ServerLogCard({ log, onReload, selectedTs, selectedNo, onPick }) {
  const [all, setAll] = useState(false)
  const [follow, setFollow] = useState(true)
  const scrollRef = useRef(null)
  const nearestRef = useRef(null)
  const counters = Object.entries(log.stats?.counters || {}).filter(([, v]) => v && v !== '0')
  const where = log.source === 'upload' ? 'uploaded' : 'read from the node'
  const entries = log.entries || []

  // NEAR_S is what "the vicinity" means. A server writes its note when it gives up, which
  // is after the packets that explain it — seconds later for a timeout, immediately for a
  // refused connection — so the window is generous enough to catch the note and tight
  // enough that it still points at something.
  const NEAR_S = 2
  const { nearestIdx, nearDelta } = useMemo(() => {
    if (!selectedTs) return { nearestIdx: -1, nearDelta: 0 }
    let best = -1
    let bestDist = Infinity
    entries.forEach((e, i) => {
      if (!e.ts) return
      const d = Math.abs(e.ts - selectedTs)
      if (d < bestDist) { bestDist = d; best = i }
    })
    return { nearestIdx: best, nearDelta: best >= 0 ? entries[best].ts - selectedTs : 0 }
  }, [entries, selectedTs])

  // Scroll the log's own container, with block:'nearest' so the surrounding column does
  // not jump as well.
  useEffect(() => {
    if (!follow || nearestIdx < 0 || !nearestRef.current) return
    nearestRef.current.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
  }, [follow, nearestIdx, selectedNo])

  return (
    <Card
      title="Server error log"
      subtitle={`${log.path || 'no log'} · ${where} · what the packets cannot show: aborted connections, DNS, TLS, listener`}
      action={
        <div className="flex items-center gap-3 text-xs text-muted">
          <label className="flex items-center gap-1.5" title="Scroll to the record nearest the selected packet">
            <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />
            follow selection
          </label>
          <label className="flex items-center gap-1.5">
            <input type="checkbox" checked={all} onChange={(e) => { setAll(e.target.checked); onReload(e.target.checked) }} />
            whole log
          </label>
        </div>
      }
    >
      {log.note && (
        <div className="mb-2 rounded-lg border bg-bg px-3 py-2 text-xs text-muted">{log.note}</div>
      )}
      {log.mismatch && (
        <div className="mb-2 rounded-lg border border-warning/40 bg-warning/10 px-3 py-2 text-xs text-warning">
          The log has {log.scanned} record(s) but none of them fall in this capture&apos;s window — it
          covers {pktDateTime(log.logFrom, 0)} → {pktDateTime(log.logTo, 0)}, the capture
          {' '}{pktDateTime(log.windowFrom, 0)} → {pktDateTime(log.windowTo, 0)}. Likely a log from a
          different period or a different server; tick <em>whole log</em> to see it anyway.
        </div>
      )}
      {log.stats?.hint && (
        <div className="mb-2 rounded-lg border border-warning/40 bg-warning/10 px-3 py-2 text-xs text-warning">
          {log.stats.hint}
        </div>
      )}
      {counters.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-1.5">
          {counters.map(([k, v]) => (
            <span key={k} className="rounded-md border bg-bg px-2 py-0.5 font-mono text-[10px]">
              {k} <span className="font-semibold text-fg">{v}</span>
            </span>
          ))}
        </div>
      )}
      {(log.top || []).length > 0 && (
        <div className="mb-2 flex flex-wrap gap-1.5">
          {log.top.map((t) => (
            <span key={t.label} className="rounded-md border px-2 py-0.5 text-[11px] text-muted">
              {t.label} · {t.count}
            </span>
          ))}
        </div>
      )}

      {/* What the nearest record is, relative to the selected packet — the sentence that
          makes the highlight below mean something. */}
      {selectedTs && nearestIdx >= 0 && (
        <div className="mb-2 text-[11px] text-muted">
          Nearest record to frame #{selectedNo}:{' '}
          <span className={Math.abs(nearDelta) <= NEAR_S ? 'font-semibold text-fg' : ''}>
            {nearDelta >= 0 ? '+' : '−'}{Math.abs(nearDelta).toFixed(3)} s
          </span>
          {Math.abs(nearDelta) > NEAR_S && ' — nothing in the log is close to this packet'}
        </div>
      )}

      {entries.length > 0 && onPick && (
        <div className="mb-1 text-[10px] text-muted opacity-70">
          Click a record to send the packet list to that moment.
        </div>
      )}
      {entries.length === 0 ? (
        <div className="py-4 text-center text-xs text-muted">
          No matching records in this window ({log.scanned || 0} lines scanned).
        </div>
      ) : (
        <div ref={scrollRef} className="max-h-72 space-y-1 overflow-y-auto pr-1">
          {entries.slice(0, 400).map((e, i) => {
            const near = selectedTs && e.ts && Math.abs(e.ts - selectedTs) <= NEAR_S
            const isNearest = i === nearestIdx && near
            const tone = e.class === 'aborted' || e.class === 'listener' ? 'text-danger'
              : e.class === 'other' ? 'text-muted' : 'text-warning'
            return (
              <div
                key={i}
                ref={isNearest ? nearestRef : null}
                onClick={() => e.ts && onPick?.(e.ts)}
                title={e.ts ? 'Jump the packet list to this moment' : e.time}
                className={`rounded-md border px-2 py-1 text-[11px] ${e.inWindow ? '' : 'opacity-50'} ` +
                  (e.ts && onPick ? 'cursor-pointer hover:border-primary/60 ' : '') +
                  (isNearest ? 'border-primary bg-primary/10 ring-1 ring-primary'
                    : near ? 'border-primary/40 bg-primary/5' : 'bg-bg')}
              >
                <div className="flex items-center gap-2">
                  {/* From the parsed instant, not the raw string: a log written with
                      log_timestamps=SYSTEM carries a zone offset, and showing it as
                      written puts rows on two different clocks in one list. */}
                  <span className="font-mono text-muted" title={e.time}>
                    {e.ts ? pktTimeOfDay(e.ts, 3) : (e.time || '').slice(11, 23)}
                  </span>
                  {e.code && <span className="font-mono text-[10px] text-muted opacity-70">{e.code}</span>}
                  <span className={`truncate font-medium ${tone}`}>{e.label}</span>
                  {isNearest && <span className="ml-auto shrink-0 text-[10px] text-primary">nearest</span>}
                </div>
                {(e.reason || e.message) && (
                  <div className="mt-0.5 break-words text-muted">{e.reason || e.message}</div>
                )}
              </div>
            )
          })}
        </div>
      )}
    </Card>
  )
}

// ---------------------------------------------------------------- list

export function PacketList({ packets, first, selectedNo, onSelect, timeMode = 'relative', markTs = null }) {
  if (!packets.length) {
    return <div className="py-10 text-center text-sm text-muted">No packets in the selected range.</div>
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[720px] text-left text-xs">
        <thead className="text-[10px] uppercase tracking-wide text-muted">
          <tr className="border-b">
            <th className="py-1.5 pr-2">Time</th>
            <th className="py-1.5 pr-2">Source</th>
            <th className="py-1.5 pr-2">Destination</th>
            <th className="py-1.5 pr-2">Protocol</th>
            <th className="py-1.5 pr-2">Info</th>
            <th className="py-1.5">Issues</th>
          </tr>
        </thead>
        <tbody>
          {packets.map((p, i) => (
            <tr key={p.no}
              onClick={() => onSelect(p.no)}
              className={'cursor-pointer border-b last:border-0 hover:bg-surface2 ' +
                (p.no === selectedNo ? 'bg-primary/10'
                  // Within 2 s of the log record that was clicked — the same "vicinity"
                  // the log pane uses for a selected packet, in the other direction.
                  : markTs && Math.abs(p.ts - markTs) <= 2 ? 'bg-warning/10' : '')}>
              <td className="whitespace-nowrap py-1.5 pr-2 font-mono text-[11px] text-muted"
                title={pktISO(p.ts)}>
                {pktFormatTime(timeMode, p.ts, first, packets[i - 1]?.ts)}
                <span className="ml-1 opacity-60">#{p.no}</span>
              </td>
              <td className="whitespace-nowrap py-1.5 pr-2 font-mono text-[11px] text-muted">{p.src}</td>
              <td className="whitespace-nowrap py-1.5 pr-2 font-mono text-[11px] text-muted">{p.dst}</td>
              <td className="py-1.5 pr-2"><Badge tone={PROTO_TONE[p.proto] || 'muted'}>{p.proto}</Badge></td>
              <td className="max-w-[26rem] truncate py-1.5 pr-2" title={p.info}>{p.info}</td>
              <td className="py-1.5">
                {p.issues?.length ? (
                  <span className={`flex items-center gap-1 ${isSevereIssue(p.issues[0]) ? 'text-danger' : 'text-warning'}`}>
                    <Icon.Bell size={12} className="shrink-0" />
                    <span className="max-w-[14rem] truncate" title={p.issues.join(' · ')}>
                      {p.issues.length > 1 ? `${issueKind(p.issues[0])} +${p.issues.length - 1}` : p.issues[0]}
                    </span>
                  </span>
                ) : <span className="text-[10px] text-muted opacity-60">clean</span>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
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

// ---------------------------------------------------------------- detail

export function PacketDetails({ d, first }) {
  const p = d.packet
  const lagTone = p.lagMs >= 100 ? 'text-danger' : p.lagMs >= 20 ? 'text-warning' : 'text-success'
  const bad = (p.status || '').startsWith('Error') || p.issues?.some(isSevereIssue)

  const metric = (label, value, tone) => (
    <div className="rounded-lg border bg-bg px-3 py-2">
      <div className="text-[10px] uppercase tracking-wide text-muted">{label}</div>
      <div className={`mt-0.5 text-base font-semibold ${tone || ''}`}>{value}</div>
    </div>
  )

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2 text-xs text-muted">
        <Badge tone={PROTO_TONE[p.proto] || 'muted'}>{p.proto}</Badge>
        <span>connection #{p.stream}</span>
        <span>{p.dir === 'c2s' ? 'client → server' : p.dir === 's2c' ? 'server → client' : ''}</span>
      </div>
      {/* When it happened, in all three forms: the local date and time to read, UTC to
          paste next to a server log line, and the offset to locate it in the capture. */}
      <div className="grid grid-cols-1 gap-1 rounded-lg border bg-bg px-3 py-2 font-mono text-[11px]">
        <Row k="captured" v={pktDateTime(p.ts)} />
        <Row k="utc" v={pktISO(p.ts)} />
        <Row k="offset" v={`+${(p.ts - first).toFixed(6)} s into the capture`} />
      </div>

      <div className="grid grid-cols-2 gap-2">
        {metric('Response time', p.lagMs ? `${p.lagMs.toFixed(2)} ms` : '—', p.lagMs ? lagTone : '')}
        {metric('Payload', `${p.payloadLen} B`)}
        {metric('Frame', `${p.frameLen} B`)}
        {metric('Status', p.status || (p.issues?.length ? 'See issues' : '—'),
          bad ? 'text-danger' : p.status === 'Success' ? 'text-success' : '')}
      </div>

      {p.issues?.length > 0 && (
        <div className="rounded-lg border border-warning/40 bg-warning/10 p-2">
          <div className="text-[10px] uppercase tracking-wide text-muted">Issues</div>
          <ul className="mt-1 space-y-0.5 text-xs">
            {p.issues.map((i, n) => (
              <li key={n} className={isSevereIssue(i) ? 'text-danger' : 'text-warning'}>• {i}</li>
            ))}
          </ul>
        </div>
      )}

      <div>
        <div className="mb-1 flex items-center justify-between">
          <h4 className="text-xs font-semibold text-muted">
            {p.command ? `${p.command} — query / command` : 'Payload summary'}
          </h4>
          {p.rows > 0 && <span className="text-[10px] text-muted">{p.rows} row(s)</span>}
        </div>
        <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md bg-surface2 p-2 font-mono text-[11px]">
          {sqlHighlight(p.query || p.info)}
        </pre>
      </div>

      <div className="grid grid-cols-2 gap-x-4 gap-y-1 rounded-lg border bg-bg p-2 font-mono text-[11px]">
        <Row k="src" v={p.src} /><Row k="dst" v={p.dst} />
        <Row k="flags" v={p.flags || '—'} /><Row k="window" v={p.window ?? '—'} />
        <Row k="seq" v={p.seq ?? '—'} /><Row k="ack" v={p.ack ?? '—'} />
        {p.errCode ? <Row k="error" v={p.errCode} /> : null}
        {p.errState ? <Row k="sqlstate" v={p.errState} /> : null}
        {d.stream?.version ? <Row k="server" v={d.stream.version} /> : null}
        {d.stream?.user ? <Row k="user" v={d.stream.user} /> : null}
        {d.stream?.tls ? <Row k="tls" v="encrypted after handshake" /> : null}
      </div>

      <details>
        <summary className="cursor-pointer text-xs font-semibold text-muted">Hex dump ({d.bytes} bytes)</summary>
        <pre className="mt-1 max-h-72 overflow-auto rounded-md bg-surface2 p-2 font-mono text-[10px] leading-snug">{d.hex}</pre>
      </details>
    </div>
  )
}

function Row({ k, v }) {
  return <div className="flex gap-2"><span className="text-muted">{k}</span><span className="min-w-0 truncate">{String(v)}</span></div>
}

// sqlHighlight is a light touch: SQL keywords bolded, nothing more. The mock used
// dangerouslySetInnerHTML for this; a decoded packet is untrusted input from the
// wire, so it is rendered as React children instead — a payload that contains
// markup must never become markup.
const SQL_WORDS = /\b(SELECT|INSERT|UPDATE|DELETE|REPLACE|FROM|WHERE|VALUES|SET|JOIN|LEFT|RIGHT|INNER|ON|AND|OR|NOT|NULL|LIMIT|ORDER|GROUP|BY|HAVING|INTO|CREATE|DROP|ALTER|TABLE|DATABASE|INDEX|BEGIN|START|TRANSACTION|COMMIT|ROLLBACK|SHOW|EXPLAIN|USE|AS|DISTINCT|COUNT|SUM|AVG|MIN|MAX|NOW|LIKE|IN|IS|DUPLICATE|KEY)\b/gi

function sqlHighlight(text) {
  if (!text) return '—'
  const parts = String(text).split(SQL_WORDS)
  return parts.map((part, i) =>
    SQL_WORDS.test(part) && i % 2 === 1
      ? <span key={i} className="font-semibold text-primary">{part}</span>
      : <span key={i}>{part}</span>)
}
