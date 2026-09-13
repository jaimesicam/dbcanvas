import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Button, Badge, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { Help } from '../components/Tooltip.jsx'
import { HELP } from '../lib/help.js'
import { usePolling } from '../lib/usePolling.jsx'
import { useHandoff } from '../lib/handoff.js'
import {
  k3dApi, k3dStateTargets, k8sStateDumps, k8sStateFromDump, k8sStateUpload, k8sArchiveApi,
  k8sArchiveFiles,
} from '../lib/stackApi.js'
import { zoomAt } from '../lib/canvas.js'
import {
  mergeStates, emptyModel, dismissGone, dismissAllGone, filterStates, groupByKind,
  countTones, isHighlighted, changeNote, isFresh, summariseWarnings,
  targetKey, parseTargetKey, POLL_CHOICES, TONE_LABEL, HIGHLIGHT_MS,
  paneId, samePane, defaultContainer, defaultLogTarget, sortContainers, movePaneView,
  sourceId, parseSourceId, SOURCE_LIVE, hiddenByDefault, countKinds,
} from '../lib/k8sStates.js'

// Kubernetes States — a canvas of what a Kubernetes cluster is doing, right now.
//
// Every other Kubernetes view in DBCanvas answers a question you knew to ask. This one is
// for the minutes when you do not: a failover is running, an operator is reconciling
// something, a backup is halfway through, and the question is simply *what is changing*.
//
// So it is a board, not a table. One column per kind, one card per object, each card
// carrying the two or three numbers that say what the object thinks of itself — and the
// canvas does three things a table cannot:
//
//   IT COLOURS WHAT IS WRONG. A card is red because the object says it is broken, and the
//   property row that says so is red inside it. The rule comes from the server
//   (app/k3dstates.go, rule 1) and is deliberately narrow: a cluster full of red is a
//   cluster nobody looks at.
//
//   IT LIGHTS WHAT MOVED. A property whose value changed since the last sample is lit for
//   a few seconds and says what it changed from — "2/3  was 3/3" is the failover, visible
//   without reading anything.
//
//   IT KEEPS WHAT DIED. An object that disappears is not removed. It stays where it was,
//   greyed and dashed, until dismissed — because the pod that vanished while you were
//   looking at another card is the one you needed to see.
//
// THREE SOURCES, ONE BOARD. A live cluster sampled on a timer; a pt-k8s-debug-collector
// capture kept by Diagnostics; an archive uploaded from a host — a customer's cluster this
// installation has never seen. The server hands back the same shape for all three
// (app/k3dstatesdump.go), so everything below draws a capture exactly as it draws a cluster.
// What differs is what a capture cannot be: it is one instant, so there is nothing to poll
// and nothing is "changing" — unless you load a second capture with Compare on, which turns
// the highlight-and-tombstone machinery into a diff between two moments.
//
// Beside the board is a rail of panes. A pane is one object seen one of three ways — its
// State, a container's Log, or its YAML — and ANY of them can be pinned, which is what makes
// this a workbench rather than a viewer: pin the custom resource's state, pin the crashing
// container's log, and keep both while you click through everything else. A pod is several
// logs and not one, so a log pane picks its container (opening on whichever is unhealthy)
// and can read `--previous`, which is the only log a CrashLoopBackOff has anything in.

const CARD_W = 248
const RAIL_MIN = 300
const RAIL_MAX = 1100

// Toolbar controls are auto-width: inputCls is w-full, which is right for a form and wrong
// for a toolbar — full-width selects stacked the header five rows deep and left the board a
// letterbox, which is how this page first went out.
const selectCls = `${inputCls.replace('w-full ', '')} w-auto py-1 text-xs`
const boxCls = `${inputCls.replace('w-full ', '')} py-1 text-xs`

// TONE_CARD / TONE_ROW paint the four tones the server reports plus the browser's own
// 'gone'. Backgrounds are tinted rather than solid so that six themes (see index.css) all
// stay readable, and so a red card is still a card rather than a block of colour.
export const TONE_CARD = {
  bad: 'border-danger/60 bg-danger/10',
  warn: 'border-warning/60 bg-warning/10',
  ok: 'border-border bg-surface',
  done: 'border-border bg-surface2/70',
  gone: 'border-dashed border-muted/60 bg-muted/10 opacity-80',
}

export const TONE_ROW = {
  bad: 'bg-danger/20 text-danger',
  warn: 'bg-warning/20 text-warning',
  ok: '',
  done: 'text-muted',
  gone: 'text-muted line-through',
  '': '',
}

export const TONE_BADGE = { bad: 'danger', warn: 'warning', ok: 'success', done: 'muted', gone: 'muted' }

// toneOf is the card's colour: a tombstone is always 'gone', whatever it last reported.
export const toneOf = (o) => (o.gone ? 'gone' : o.tone)

// shortName trims the generated suffix Kubernetes hangs off pod names, which is what makes
// a column of them unreadable at a glance. The full name is always in the pane.
export function shortName(name, cap = 34) {
  if (!name || name.length <= cap) return name
  return `${name.slice(0, cap - 1)}…`
}

// visibleProps picks what fits on a card: red rows first, then amber, then the rest, capped.
// The order is the answer to "why is this card red" — the row that caused the colour has to
// be one of the ones that fit. The pane shows everything, in the server's order.
export function visibleProps(obj, cap = 4) {
  const props = obj.props || []
  const of = (t) => props.filter((p) => p.tone === t)
  const plain = props.filter((p) => p.tone !== 'bad' && p.tone !== 'warn')
  return [...of('bad'), ...of('warn'), ...plain].slice(0, cap)
}

// A pod has logs; everything has a state and a manifest. The tabs follow.
export function viewsFor(obj) {
  const views = [{ id: 'state', label: 'State' }]
  if (obj?.kind === 'Pod') views.push({ id: 'logs', label: 'Logs' })
  views.push({ id: 'yaml', label: 'YAML' })
  return views
}

export const TAIL_CHOICES = [100, 200, 1000, 5000]

// PropRow is one property, on a card or in a pane. The highlight is the point: a value that
// changed within HIGHLIGHT_MS gets a ring and says where it came from.
export function PropRow({ obj, prop, now, wide = false }) {
  const lit = isHighlighted(obj, prop.key, now)
  const note = changeNote(obj, prop.key, now)
  return (
    <div className={`flex items-baseline justify-between gap-2 rounded px-1 py-0.5 ${TONE_ROW[prop.tone] || ''} ${lit ? 'ring-1 ring-primary' : ''}`}>
      <span className={`shrink-0 ${wide ? 'text-xs' : 'text-[10px]'} text-muted`}>{prop.key}</span>
      <span className={`truncate text-right font-mono ${wide ? 'text-xs' : 'text-[10px]'}`}>
        {prop.value}
        {note && <span className="ml-1 text-primary">{`(${note})`}</span>}
      </span>
    </div>
  )
}

// StateCard is one object on the board.
export function StateCard({ obj, now, selected, pinned, onSelect, onPin, onDismiss }) {
  const tone = toneOf(obj)
  const fresh = isFresh(obj, now)
  const props = visibleProps(obj)
  const hidden = (obj.props?.length ?? 0) - props.length
  return (
    <div
      role="button"
      tabIndex={0}
      onClick={() => onSelect(obj.key)}
      onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onSelect(obj.key) } }}
      style={{ width: CARD_W }}
      className={`cursor-pointer rounded-lg border p-2 text-left transition-shadow ${TONE_CARD[tone] || TONE_CARD.ok}
        ${selected ? 'ring-2 ring-primary' : fresh ? 'ring-2 ring-accent' : ''}`}
    >
      <div className="flex items-start justify-between gap-1">
        <div className="min-w-0">
          <div className={`truncate font-mono text-xs font-semibold ${obj.gone ? 'text-muted line-through' : 'text-fg'}`}>
            {shortName(obj.name)}
          </div>
          <div className="truncate text-[10px] text-muted">{obj.namespace || 'cluster'}</div>
        </div>
        <div className="flex shrink-0 items-center gap-0.5">
          {obj.events?.length > 0 && (
            <span title={`${obj.events.length} warning event(s)`} className="rounded bg-warning/20 px-1 text-[10px] font-medium text-warning">
              {`!${obj.events.length}`}
            </span>
          )}
          <button
            title={pinned ? 'Unpin this object' : 'Pin this object'}
            onClick={(e) => { e.stopPropagation(); onPin(obj.key) }}
            className={`rounded p-0.5 hover:bg-surface2 ${pinned ? 'text-primary' : 'text-muted'}`}
          >
            <Icon.Pin size={12} />
          </button>
          {obj.gone && (
            <button
              title="Dismiss this deleted object"
              onClick={(e) => { e.stopPropagation(); onDismiss(obj.key) }}
              className="rounded p-0.5 text-muted hover:bg-surface2 hover:text-fg"
            >
              <Icon.Close size={12} />
            </button>
          )}
        </div>
      </div>
      <div className="mt-1 truncate text-[11px] font-medium">{obj.gone ? 'deleted' : obj.summary}</div>
      <div className="mt-1 space-y-0.5">
        {props.map((p) => <PropRow key={p.key} obj={obj} prop={p} now={now} />)}
        {hidden > 0 && <div className="px-1 text-[10px] text-muted">{`+${hidden} more`}</div>}
      </div>
    </div>
  )
}

// ---------------------------------------------------------------- pane bodies

function CopyButton({ text, title = 'Copy' }) {
  const [done, setDone] = useState(false)
  return (
    <button title={title} disabled={!text}
      onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}
      className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg disabled:opacity-40">
      {done ? <Icon.Check size={13} /> : <Icon.Copy size={13} />}
    </button>
  )
}

function Spinner() {
  return <span className="h-2.5 w-2.5 animate-spin rounded-full border-2 border-surface2 border-t-primary" />
}

// StateBody is what the card says, in full.
export function StateBody({ obj, now }) {
  return (
    <div className="min-h-0 flex-1 overflow-auto">
      <div className="space-y-0.5 px-2 py-2">
        {(obj.props || []).map((p) => <PropRow key={p.key} obj={obj} prop={p} now={now} wide />)}
        {(obj.props || []).length === 0 && <div className="px-1 text-xs text-muted">No properties reported.</div>}
      </div>
      {obj.events?.length > 0 && (
        <div className="border-t px-3 py-2">
          <div className="mb-1 text-[11px] font-medium text-muted">Recent warnings</div>
          <ul className="space-y-1">
            {obj.events.map((e, i) => (
              <li key={`${e.reason}-${i}`} className="text-[11px] leading-snug">
                <span className="font-medium text-warning">{e.reason}</span>
                {e.count > 1 && <span className="text-muted">{` ×${e.count}`}</span>}
                <div className="break-words text-muted">{e.message}</div>
              </li>
            ))}
          </ul>
        </div>
      )}
      {obj.gone && (
        <div className="border-t px-3 py-2 text-[11px] text-muted">
          Gone from the cluster. Kept here, in its last known state, until you dismiss it.
        </div>
      )}
    </div>
  )
}

// LogBody tails one container.
//
// The container picker is the whole reason this is not one text box: a Percona database pod
// runs six containers, and the one worth reading is whichever is unhealthy — so the list
// leads with that one and the pane opens on it. `previous` reads the run that died, which is
// the only log a CrashLoopBackOff has anything in, and it is offered once something has
// actually restarted.
export function LogBody({ obj, api, tick }) {
  const [data, setData] = useState(null)
  // A capture keeps one log per pod plus whatever it pulled off disk, so its picker offers
  // FILES; a live pod offers containers. Which of the two is decided by the SOURCE, not by
  // the reply — that is the whole fix for a pane that used to open an archive by asking for
  // the pod's worst container, be told there is no such file, and never find out that
  // logs.txt, summary.txt and var/lib/mysql/mysqld-error.log were sitting right there.
  const archive = !!api?.archive
  const archiveFiles = archive ? data?.files || null : null
  const containers = useMemo(
    () => (archive ? (archiveFiles || []).map((f) => ({ name: f, tone: '' })) : sortContainers(obj.containers)),
    [archive, archiveFiles, obj.containers],
  )
  const [container, setContainer] = useState(() => defaultLogTarget(obj, archive))
  const [tail, setTail] = useState(200)
  const [previous, setPrevious] = useState(false)
  const [follow, setFollow] = useState(true)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const boxRef = useRef(null)
  const atBottom = useRef(true)

  // A pinned pane outlives its pod: an operator deletes and recreates one, and the
  // replacement may not have the container this pane was reading.
  useEffect(() => {
    if (archive) return // the file list arrives with the reply; the server picked one
    if (!container || containers.some((c) => c.name === container)) return
    setContainer(defaultContainer(obj.containers))
  }, [archive, containers, container, obj.containers])

  // And it outlives its SOURCE: the board switches between a live cluster and a capture with
  // the pane still open. A container name means nothing to an archive and a file name means
  // nothing to kubectl, so the pane starts again from whatever the new source names.
  const wasArchive = useRef(archive)
  useEffect(() => {
    if (wasArchive.current === archive) return
    wasArchive.current = archive
    setContainer(defaultLogTarget(obj, archive))
    setData(null)
    setErr('')
  }, [archive, obj])

  const load = useCallback(async () => {
    if (!api || obj.gone) return
    setBusy(true)
    try {
      const r = await api.stateLogs({ namespace: obj.namespace, name: obj.name, container, tail, previous })
      setData(r)
      setErr(r.error || '')
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy(false)
    }
  }, [api, obj.namespace, obj.name, obj.gone, container, tail, previous])

  // On open, and whenever an option changes.
  useEffect(() => { load() }, [load])

  // Following means "with the board's own sampling", so a log and the card it belongs to are
  // never different ages. A pane that is not following stays exactly where it was put, which
  // is what you want the moment you are actually reading it.
  const loadRef = useRef(load)
  loadRef.current = load
  useEffect(() => {
    if (!follow || !tick) return
    loadRef.current()
    // load is held in a ref on purpose: it changes identity on every option change, and
    // depending on it here would re-read on each of those twice.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tick, follow])

  // Stay pinned to the newest line as the tail grows — unless the reader scrolled up, in
  // which case they are reading something and the pane must not yank it away.
  useEffect(() => {
    const el = boxRef.current
    if (el && atBottom.current) el.scrollTop = el.scrollHeight
  }, [data])

  const text = data?.text || ''
  const restarted = containers.some((c) => c.restarts > 0)
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex flex-wrap items-center gap-1 border-b px-2 py-1">
        <select className={selectCls} value={archive ? (data?.file || '') : container}
          onChange={(e) => setContainer(e.target.value)}
          title={archive ? 'Which file the capture kept for this pod' : "Which container's log — a pod has one per container"}>
          {containers.length === 0 && (
            <option value="">
              {!archive ? '(only container)' : data ? '(the capture kept none)' : '(reading the capture…)'}
            </option>
          )}
          {containers.map((c) => (
            <option key={c.name} value={c.name}>
              {`${c.init ? 'init · ' : ''}${c.name}${c.tone === 'bad' ? ' !' : ''}`}
            </option>
          ))}
        </select>
        {!archive && (
          <select className={selectCls} value={tail} onChange={(e) => setTail(Number(e.target.value))} title="How many lines">
            {TAIL_CHOICES.map((n) => <option key={n} value={n}>{`${n} lines`}</option>)}
          </select>
        )}
        {!archive && (
          <label className="flex items-center gap-1 text-[11px] text-muted" title="Re-read it with every sample of the board">
            <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> follow
          </label>
        )}
        {!archive && restarted && (
          <label className="flex items-center gap-1 text-[11px] text-muted"
            title="Read the log of the run that died — the only log a crash-looping container has anything in">
            <input type="checkbox" checked={previous} onChange={(e) => setPrevious(e.target.checked)} /> previous
          </label>
        )}
        <div className="ml-auto flex items-center gap-1">
          {busy && <Spinner />}
          <button title="Read it again now" onClick={load} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
            <Icon.Play size={13} />
          </button>
          <CopyButton text={text} title="Copy the log" />
        </div>
      </div>
      <div ref={boxRef} onScroll={(e) => {
        const el = e.currentTarget
        atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
      }}
        className="min-h-0 flex-1 overflow-auto bg-bg px-2 py-1 font-mono text-[11px] leading-relaxed text-fg">
        {err && <div className="whitespace-pre-wrap text-warning">{err}</div>}
        {/* kubectl's own note about a successful read: which container it defaulted to, or
            why the previous run's log could not be retrieved. Not an error — but without it
            an empty pane says "No log lines", which is not what happened. */}
        {data?.note && <div className="whitespace-pre-wrap pb-1 text-muted">{data.note}</div>}
        {!err && text === '' && <div className="text-muted">{busy ? 'Reading…' : 'No log lines.'}</div>}
        {text !== '' && <pre className="whitespace-pre-wrap break-all">{text}</pre>}
      </div>
      <div className="flex shrink-0 items-center justify-between border-t px-2 py-1 text-[10px] text-muted">
        <span>{data?.truncated ? 'oldest lines dropped — this is a tail' : ''}</span>
        <span>{data?.readAt ? data.readAt.replace('T', ' ').replace('Z', ' UTC') : ''}</span>
      </div>
    </div>
  )
}

// YamlBody is the object itself. Read-only, deliberately: this page is a monitor, and the
// editors that write (cr.yaml, Secrets and ConfigMaps) dry-run against the API server first.
export function YamlBody({ obj, api, tick }) {
  const [data, setData] = useState(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [follow, setFollow] = useState(false)

  const load = useCallback(async () => {
    if (!api || obj.gone) return
    setBusy(true)
    try {
      setData(await api.stateManifest({ kind: obj.kind, namespace: obj.namespace, name: obj.name }))
      setErr('')
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy(false)
    }
  }, [api, obj.kind, obj.namespace, obj.name, obj.gone])

  useEffect(() => { load() }, [load])

  // Off by default, unlike a log: a manifest that reloads under you takes your scroll
  // position with it, and a YAML changes far less often than a tail grows.
  const loadRef = useRef(load)
  loadRef.current = load
  useEffect(() => {
    if (!follow || !tick) return
    loadRef.current()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tick, follow])

  const yaml = data?.yaml || ''
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex items-center gap-2 border-b px-2 py-1">
        <label className="flex items-center gap-1 text-[11px] text-muted" title="Re-read it with every sample of the board">
          <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> follow
        </label>
        <span className="text-[10px] text-muted">read-only</span>
        <div className="ml-auto flex items-center gap-1">
          {busy && <Spinner />}
          <button title="Read it again now" onClick={load} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
            <Icon.Play size={13} />
          </button>
          <CopyButton text={yaml} title="Copy the YAML" />
        </div>
      </div>
      <div className="min-h-0 flex-1 overflow-auto bg-bg px-2 py-1 font-mono text-[11px] leading-relaxed text-fg">
        {err && <div className="whitespace-pre-wrap text-warning">{err}</div>}
        {!err && yaml === '' && <div className="text-muted">{busy ? 'Reading…' : 'Nothing to show.'}</div>}
        {yaml !== '' && <pre className="whitespace-pre">{yaml}</pre>}
      </div>
      <div className="flex shrink-0 items-center justify-end border-t px-2 py-1 text-[10px] text-muted">
        {data?.readAt ? data.readAt.replace('T', ' ').replace('Z', ' UTC') : ''}
      </div>
    </div>
  )
}

// StatePane is one pane in the rail: an object, seen one of three ways, pinned or not.
export function StatePane({ obj, view, now, api, tick, pinned, maxed, onView, onPin, onMax, onClose, onDismiss }) {
  if (!obj) return null
  const views = viewsFor(obj)
  const shown = views.some((v) => v.id === view) ? view : 'state'
  const pane = { key: obj.key, view: shown }
  const tone = toneOf(obj)
  return (
    <div className={`flex min-h-0 flex-col rounded-lg border ${TONE_CARD[tone] || TONE_CARD.ok} ${maxed ? 'h-full' : shown === 'state' ? 'max-h-[28rem]' : 'h-96'}`}>
      <div className="flex shrink-0 items-start justify-between gap-2 border-b px-3 py-2">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <Badge tone={TONE_BADGE[tone]}>{obj.gone ? 'deleted' : obj.summary}</Badge>
            <span className="text-xs text-muted">{obj.kind}</span>
          </div>
          <div className="mt-1 break-all font-mono text-xs text-fg">{obj.name}</div>
          <div className="text-[11px] text-muted">{obj.namespace || 'cluster-scoped'}{obj.owner ? ` · ${obj.owner}` : ''}</div>
        </div>
        <div className="flex shrink-0 items-center gap-1">
          <button title={pinned ? 'Unpin this pane' : 'Pin this pane — it stays while you look at other cards'}
            onClick={() => onPin(pane)}
            className={`rounded p-1 hover:bg-surface2 ${pinned ? 'text-primary' : 'text-muted'}`}>
            <Icon.Pin size={14} />
          </button>
          {onMax && (
            <button title={maxed ? 'Back to the rail (Esc)' : 'Fill the page with this pane'} onClick={() => onMax(maxed ? null : pane)}
              className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
              {maxed ? <Icon.Minimize size={14} /> : <Icon.Maximize size={14} />}
            </button>
          )}
          {obj.gone && (
            <button title="Dismiss" onClick={() => onDismiss(obj.key)} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
              <Icon.Trash size={14} />
            </button>
          )}
          {onClose && (
            <button title="Close" onClick={onClose} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
              <Icon.Close size={14} />
            </button>
          )}
        </div>
      </div>
      <div className="flex shrink-0 items-center gap-1 border-b px-2 py-1">
        {views.map((v) => (
          <button key={v.id} onClick={() => onView(pane, { key: obj.key, view: v.id })}
            className={`rounded px-2 py-0.5 text-[11px] ${shown === v.id ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2'}`}>
            {v.label}
          </button>
        ))}
        {obj.gone && shown !== 'state' && (
          <span className="ml-auto text-[10px] text-muted">the object is gone — there is nothing left to read</span>
        )}
      </div>
      {shown === 'state' && <StateBody obj={obj} now={now} />}
      {shown === 'logs' && <LogBody obj={obj} api={api} tick={tick} />}
      {shown === 'yaml' && <YamlBody obj={obj} api={api} tick={tick} />}
    </div>
  )
}

// CaptureFiles is the archive itself, browsable: every file pt-k8s-debug-collector wrote,
// what it is, and what it belongs to. It exists so the honest answer to "is there anything in
// this capture the board is not showing me" is no — the board draws the objects, and this is
// everything else the collector kept: the per-container logs, pt-mysql-summary's output, the
// innobackup and pgBackRest backup logs, the decoded TLS certificates, and errors.txt.
export function CaptureFiles({ source, onClose }) {
  const [tree, setTree] = useState(null)
  const [err, setErr] = useState('')
  const [open, setOpen] = useState(null)   // { path, text, truncated }
  const [filter, setFilter] = useState('')
  const api = useMemo(
    () => (source?.kind === 'dump' ? k8sArchiveFiles({ dump: source.id })
      : source?.kind === 'upload' ? k8sArchiveFiles({ upload: source.id }) : null),
    [source],
  )

  useEffect(() => {
    if (!api) return
    let alive = true
    api.list().then((r) => alive && setTree(r)).catch((e) => alive && setErr(e.message))
    return () => { alive = false }
  }, [api])

  const files = useMemo(() => {
    const q = filter.trim().toLowerCase()
    const all = tree?.files || []
    return q ? all.filter((f) => f.path.toLowerCase().includes(q)) : all
  }, [tree, filter])

  return (
    <div className="flex min-h-0 flex-col rounded-lg border bg-surface" style={{ maxHeight: '32rem' }}>
      <div className="flex shrink-0 items-center justify-between gap-2 border-b px-3 py-2">
        <div className="min-w-0">
          <div className="text-xs font-semibold text-fg">Capture files</div>
          <div className="truncate text-[11px] text-muted">
            {tree ? `${tree.files.length} files${tree.collectorErrors ? ` · ${tree.collectorErrors} collection error(s)` : ''}` : 'reading…'}
          </div>
        </div>
        <button title="Close" onClick={onClose} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
          <Icon.Close size={14} />
        </button>
      </div>
      <div className="shrink-0 border-b px-2 py-1">
        <input className={`${boxCls} w-full`} placeholder="Filter files" value={filter}
          onChange={(e) => setFilter(e.target.value)} />
      </div>
      {err && <div className="px-3 py-2 text-xs text-danger">{err}</div>}
      {!open && (
        <div className="min-h-0 flex-1 overflow-auto">
          {files.map((f) => (
            <button key={f.path} onClick={() => api.read(f.path).then(setOpen).catch((e) => setErr(e.message))}
              className="flex w-full items-baseline justify-between gap-2 px-3 py-1 text-left hover:bg-surface2">
              <span className="truncate font-mono text-[11px] text-fg">{f.path}</span>
              <span className="shrink-0 text-[10px] text-muted">{`${f.kind} · ${sizeLabel(f.size)}`}</span>
            </button>
          ))}
          {tree && files.length === 0 && <div className="px-3 py-2 text-xs text-muted">Nothing matches.</div>}
        </div>
      )}
      {open && (
        <div className="flex min-h-0 flex-1 flex-col">
          <div className="flex shrink-0 items-center gap-2 border-b px-2 py-1">
            <button onClick={() => setOpen(null)} className="rounded px-2 py-0.5 text-[11px] text-muted hover:bg-surface2">← files</button>
            <span className="truncate font-mono text-[11px] text-fg">{open.path}</span>
            <div className="ml-auto"><CopyButton text={open.text} title="Copy the file" /></div>
          </div>
          <div className="min-h-0 flex-1 overflow-auto bg-bg px-2 py-1 font-mono text-[11px] leading-relaxed text-fg">
            {open.truncated && <div className="pb-1 text-muted">oldest lines dropped — this is a tail</div>}
            <pre className="whitespace-pre-wrap break-all">{open.text || '(empty)'}</pre>
          </div>
        </div>
      )}
    </div>
  )
}

// sizeLabel keeps a file list readable: bytes for the small ones, KiB/MiB above that.
export function sizeLabel(n) {
  if (!(n > 0)) return '0 B'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} KiB`
  return `${(n / 1024 / 1024).toFixed(1)} MiB`
}

// Legend doubles as the tone filter's explanation. Four tones and a tombstone is the whole
// vocabulary of this page, so it is worth one line on screen rather than a tooltip.
export function ToneLegend({ counts }) {
  return (
    <div className="flex flex-wrap items-center gap-2 text-[11px]">
      {['bad', 'warn', 'ok', 'done', 'gone'].map((t) => (
        <span key={t} className="flex items-center gap-1 text-muted">
          <span className={`h-2.5 w-2.5 rounded-sm border ${TONE_CARD[t]}`} />
          {`${TONE_LABEL[t]} ${counts?.[t] ?? 0}`}
        </span>
      ))}
    </div>
  )
}

export default function K8sStates() {
  const [targets, setTargets] = useState(null)
  const [dumps, setDumps] = useState([])       // pt-k8s-debug-collector captures kept here
  const [uploads, setUploads] = useState([])   // archives dropped on the page this session
  const [key, setKey] = useState('')           // the selected source: live:… | dump:… | upload:…
  const [busy, setBusy] = useState('')
  const [model, setModel] = useState(emptyModel)
  const [meta, setMeta] = useState(null)     // namespaces, kinds, operator, warnings
  const [err, setErr] = useState('')
  const [pollMs, setPollMs] = useState(5000)
  const [namespace, setNamespace] = useState('')
  // Kind visibility is an EXCLUDE set, not an include set: everything the source has is on
  // the board, and the noisy kinds (a capture's 468 ClusterRoles) start folded away. `seen`
  // is what stops a reload from re-hiding a kind somebody turned on.
  const [hidden, setHidden] = useState(() => new Set())
  const [seenKinds, setSeenKinds] = useState(() => new Set())
  const [filesOpen, setFilesOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [problems, setProblems] = useState(false)
  const [sel, setSel] = useState(null)        // { key, view } — the one pane that is not pinned
  const [pins, setPins] = useState(() => [])   // [{ key, view }] — any object, any view
  const [maxed, setMaxed] = useState(null)    // one pane, filling the page
  const [full, setFull] = useState(false)     // the page itself, filling the window
  const [railW, setRailW] = useState(400)
  const [view, setView] = useState({ x: 24, y: 16, z: 1 })
  const [now, setNow] = useState(() => Date.now())

  const wrapRef = useRef(null)
  const dragRef = useRef(null)

  // The source, and the API the panes read their logs and YAML through. Both switch
  // together, and everything downstream is written against the pair rather than against a
  // cluster: a pane does not know whether it is reading a cluster or a capture of one.
  const source = useMemo(() => parseSourceId(key), [key])
  const live = source?.kind === SOURCE_LIVE
  const api = useMemo(() => {
    if (!source) return null
    if (source.kind === SOURCE_LIVE) return k3dApi(source.stackId, source.frameId)
    if (source.kind === 'dump') return k8sArchiveApi({ dump: source.id })
    return k8sArchiveApi({ upload: source.id })
  }, [source])

  // Opened from a cluster's own panel ("Watch Kubernetes states"), which hands over the target.
  useHandoff('dbcanvas.statesTarget', (raw) => { if (parseTargetKey(raw)) setKey(raw) })

  useEffect(() => {
    let alive = true
    k3dStateTargets()
      .then((list) => {
        if (!alive) return
        setTargets(list)
        // Open on a live cluster when there is one; a capture is the fallback, and the page
        // is still useful with no cluster at all, which is the point of reading archives.
        setKey((k) => (k ? k : (list[0] ? sourceId(SOURCE_LIVE, targetKey(list[0])) : '')))
      })
      .catch((e) => alive && setErr(e.message))
    k8sStateDumps().then((list) => alive && setDumps(list)).catch(() => {})
    return () => { alive = false }
  }, [])

  // Switching clusters starts a new board: the old model's tombstones, pins and highlights
  // are about a different cluster, and carrying them over would be a lie about this one.
  useEffect(() => {
    if (!compareRef.current) {
      setModel(emptyModel())
      setNamespace('')
    }
    setFilesOpen(false)
    setMeta(null)
    setSel(null)
    setPins([])
    setMaxed(null)
  }, [key])

  // compare keeps the board when a new source is loaded, so a second capture arrives as a
  // DIFF of the first: what changed is lit, what is gone is a tombstone. It is off by
  // default because the usual case is "show me this capture", and a board silently holding
  // another cluster's objects would be a lie.
  const [compare, setCompare] = useState(false)
  const compareRef = useRef(compare)
  compareRef.current = compare

  const sample = useCallback(async () => {
    if (!source) return
    try {
      let data
      if (source.kind === SOURCE_LIVE) data = await k3dApi(source.stackId, source.frameId).states()
      else if (source.kind === 'dump') data = await k8sStateFromDump(source.id)
      else data = uploads.find((u) => u.token === source.id)?.sample
      if (!data) throw new Error('that uploaded archive is no longer held — upload it again')
      const t = Date.now()
      setModel((m) => mergeStates(m, data, t))
      setMeta(data)
      setNow(t)
      setErr('')
    } catch (e) {
      setErr(e.message)
    }
  }, [source, uploads])

  // Only a live cluster is polled: a capture is one instant and asking again reads the same
  // file for the same answer.
  usePolling(sample, live ? pollMs : 0)

  // An archive is loaded once, when it becomes the source.
  useEffect(() => {
    if (!source || live) return
    sample()
    // sample changes identity with `uploads`; re-running on that would reload the archive
    // every time another one is dropped.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key])

  const onUpload = useCallback(async (file) => {
    if (!file) return
    setBusy('Reading the archive…')
    try {
      const data = await k8sStateUpload(file)
      const entry = { token: data.upload, name: data.label || file.name, sample: data }
      setUploads((u) => [entry, ...u.filter((x) => x.token !== entry.token)].slice(0, 3))
      setKey(sourceId('upload', entry.token))
      setErr('')
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy('')
    }
  }, [])

  // A second clock, only while something is still lit: highlights fade on their own even
  // when the poll is slow or paused, and without this a 1-minute sample rate would leave a
  // change ringed for a minute.
  const lit = useMemo(
    () => model.objects.some((o) => Object.values(o.changed || {}).some((c) => now - c.at < HIGHLIGHT_MS) || isFresh(o, now)),
    [model, now],
  )
  useEffect(() => {
    if (!lit) return undefined
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [lit])

  // Esc leaves whatever is covering the page, innermost first.
  useEffect(() => {
    if (!full && !maxed) return undefined
    const onKey = (e) => {
      if (e.key !== 'Escape') return
      if (maxed) setMaxed(null)
      else setFull(false)
    }
    addEventListener('keydown', onKey)
    return () => removeEventListener('keydown', onKey)
  }, [full, maxed])

  // Panning the board and dragging the rail's divider are one pointer session.
  useEffect(() => {
    function onMove(e) {
      const d = dragRef.current
      if (!d) return
      if (d.kind === 'rail') {
        setRailW(Math.min(RAIL_MAX, Math.max(RAIL_MIN, d.ow - (e.clientX - d.sx))))
        return
      }
      setView((v) => ({ ...v, x: d.ox + (e.clientX - d.sx), y: d.oy + (e.clientY - d.sy) }))
    }
    function onUp() { dragRef.current = null }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => { removeEventListener('pointermove', onMove); removeEventListener('pointerup', onUp) }
  }, [])

  useEffect(() => {
    const el = wrapRef.current
    if (!el) return undefined
    function onWheel(e) {
      e.preventDefault()
      const rect = el.getBoundingClientRect()
      setView((v) => zoomAt(v, e.clientX - rect.left, e.clientY - rect.top, e.deltaY))
    }
    el.addEventListener('wheel', onWheel, { passive: false })
    return () => el.removeEventListener('wheel', onWheel)
    // The board is unmounted while a pane is maximized, so the listener has to be attached
    // again when it comes back.
  }, [targets, full, maxed])

  // Whatever kinds this source turned out to have, hidden-by-default applied to the ones
  // the board has not met before.
  useEffect(() => {
    const kinds = meta?.kinds || []
    if (!kinds.length) return
    setSeenKinds((prevSeen) => {
      setHidden((prevHidden) => hiddenByDefault(kinds, prevSeen, prevHidden).hidden)
      return hiddenByDefault(kinds, prevSeen).seen
    })
  }, [meta])

  const shownObjects = useMemo(
    () => filterStates(model.objects, { namespace, hidden, query, problems }),
    [model, namespace, hidden, query, problems],
  )
  const kindCounts = useMemo(() => countKinds(model.objects), [model])
  const columns = useMemo(() => groupByKind(shownObjects), [shownObjects])
  const counts = useMemo(() => countTones(shownObjects), [shownObjects])
  const warnings = useMemo(() => summariseWarnings(shownObjects), [shownObjects])
  const goneCount = counts.gone

  const togglePin = useCallback((pane) => {
    setPins((p) => (p.some((x) => samePane(x, pane)) ? p.filter((x) => !samePane(x, pane)) : [...p, pane]))
  }, [])
  // The pin on a card is the state pane, because the state is what a card shows.
  const pinCard = useCallback((k) => togglePin({ key: k, view: 'state' }), [togglePin])

  const dismiss = useCallback((k) => {
    setModel((m) => dismissGone(m, k))
    setPins((p) => p.filter((x) => x.key !== k))
    setSel((s) => (s?.key === k ? null : s))
    setMaxed((m) => (m?.key === k ? null : m))
  }, [])

  // Switching a pane's view moves that pane, wherever it lives: a pinned log that you flip
  // to YAML stays pinned, as YAML, and does not silently unpin or clone itself.
  const changeView = useCallback((from, to) => {
    setPins((p) => movePaneView(p, from, to))
    setSel((s) => (s && samePane(s, from) ? to : s))
    setMaxed((m) => (m && samePane(m, from) ? to : m))
  }, [])

  const toggleKind = useCallback((kind) => {
    setHidden((s) => {
      const next = new Set(s)
      next.has(kind) ? next.delete(kind) : next.add(kind)
      return next
    })
  }, [])

  const objOf = useCallback((pane) => (pane ? model.byKey?.get(pane.key) || null : null), [model])
  const panes = useMemo(() => {
    const list = pins.map((p) => ({ ...p, pinned: true }))
    if (sel && !pins.some((p) => samePane(p, sel))) list.push({ ...sel, pinned: false })
    return list.filter((p) => model.byKey?.has(p.key))
  }, [pins, sel, model])
  const maxedObj = objOf(maxed)

  return (
    <div className={full
      ? 'fixed inset-0 z-40 flex flex-col gap-2 bg-bg p-3'
      : 'flex h-full min-h-[40rem] flex-col gap-2'}>
      <div className="flex flex-wrap items-center gap-2">
        <h1 className="flex items-center gap-2 text-base font-semibold text-fg">
          <Icon.Kanban size={18} /> Kubernetes States
          <Help text={HELP.k8sStates} />
        </h1>
        <select className={selectCls} value={key} onChange={(e) => setKey(e.target.value)}
          title="A live cluster, a kept capture, or an archive you uploaded">
          {targets === null && <option value="">Loading…</option>}
          {targets !== null && !targets.length && !dumps.length && !uploads.length && (
            <option value="">Nothing to watch — deploy a cluster or upload a cluster-dump</option>
          )}
          {(targets || []).length > 0 && (
            <optgroup label="Live clusters">
              {targets.map((t) => (
                <option key={targetKey(t)} value={sourceId(SOURCE_LIVE, targetKey(t))}>
                  {`${t.stackName} · ${t.label}${t.operator ? ` (${t.operator})` : ''}`}
                </option>
              ))}
            </optgroup>
          )}
          {dumps.length > 0 && (
            <optgroup label="Captures (pt-k8s-debug-collector)">
              {dumps.map((d) => (
                <option key={d.id} value={sourceId('dump', d.id)}>
                  {`${d.cluster} · ${(d.capturedAt || '').replace('T', ' ').replace('Z', '')}`}
                </option>
              ))}
            </optgroup>
          )}
          {uploads.length > 0 && (
            <optgroup label="Uploaded">
              {uploads.map((u) => <option key={u.token} value={sourceId('upload', u.token)}>{u.name}</option>)}
            </optgroup>
          )}
        </select>
        {live && (
          <select className={selectCls} value={pollMs} onChange={(e) => setPollMs(Number(e.target.value))}
            title="How often the cluster is sampled">
            {POLL_CHOICES.map((c) => <option key={c.value} value={c.value}>{c.label}</option>)}
          </select>
        )}
        <Button size="sm" variant="ghost" onClick={sample} disabled={!source}>{live ? 'Sample now' : 'Reload'}</Button>
        <label className={`cursor-pointer rounded-lg border px-2 py-1 text-xs ${busy ? 'opacity-50' : 'hover:bg-surface2'}`}
          title="Read a pt-k8s-debug-collector cluster-dump from this machine — a cluster this installation has never seen reads exactly the same">
          {busy || 'Upload a cluster-dump'}
          <input type="file" accept=".gz,.tgz,.tar.gz,application/gzip" className="hidden"
            onChange={(e) => { onUpload(e.target.files?.[0]); e.target.value = '' }} />
        </label>
        <label className="flex items-center gap-1 text-xs text-muted"
          title="Keep what is on the board when you load another source, so a second capture arrives as a diff of the first">
          <input type="checkbox" checked={compare} onChange={(e) => setCompare(e.target.checked)} /> compare
        </label>
        <input className={`${boxCls} w-52`} placeholder="Filter by name, kind or value"
          value={query} onChange={(e) => setQuery(e.target.value)} />
        <select className={selectCls} value={namespace} onChange={(e) => setNamespace(e.target.value)}>
          <option value="">All namespaces</option>
          {(meta?.namespaces || []).map((ns) => <option key={ns} value={ns}>{ns}</option>)}
        </select>
        <label className="flex items-center gap-1 text-xs text-muted">
          <input type="checkbox" checked={problems} onChange={(e) => setProblems(e.target.checked)} />
          Only what is wrong or changing
        </label>
        <div className="ml-auto flex items-center gap-2">
          <ToneLegend counts={counts} />
          {goneCount > 0 && (
            <Button size="sm" variant="ghost" onClick={() => { setModel(dismissAllGone(model)); setPins((p) => p.filter((x) => model.byKey.get(x.key) && !model.byKey.get(x.key).gone)) }}>
              {`Dismiss ${goneCount} deleted`}
            </Button>
          )}
          <Button size="sm" variant="ghost" onClick={() => setView({ x: 24, y: 16, z: 1 })}>Reset view</Button>
          <button title={full ? 'Leave full screen (Esc)' : 'Fill the window'} onClick={() => setFull((f) => !f)}
            className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
            {full ? <Icon.Minimize size={15} /> : <Icon.Maximize size={15} />}
          </button>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-1">
        {(meta?.kinds || []).map((k) => (
          <button key={k} onClick={() => toggleKind(k)}
            title={hidden.has(k) ? `Show the ${kindCounts.get(k) ?? 0} ${k} objects` : `Hide ${k}`}
            className={`rounded-full border px-2 py-0.5 text-[11px] ${hidden.has(k)
              ? 'border-dashed border-border text-muted hover:bg-surface2'
              : 'border-primary bg-primary/15 text-primary'}`}>
            {`${k} ${kindCounts.get(k) ?? 0}`}
          </button>
        ))}
        {hidden.size > 0 && (
          <button onClick={() => setHidden(new Set())} className="rounded-full border border-dashed px-2 py-0.5 text-[11px] text-muted hover:bg-surface2">
            {`show all (${hidden.size} folded)`}
          </button>
        )}
        {meta?.source === 'archive' && (
          <button onClick={() => setFilesOpen((v) => !v)}
            title="Every file in this capture — the logs, the summaries, the certificates, the collector's own errors"
            className={`rounded-full border px-2 py-0.5 text-[11px] ${filesOpen ? 'border-primary bg-primary/15 text-primary' : 'border-border text-muted hover:bg-surface2'}`}>
            Capture files
          </button>
        )}
        {warnings > 0 && <span className="text-[11px] text-warning">{`${warnings} warning event(s) on screen`}</span>}
        <span className="ml-auto text-[11px] text-muted">
          {meta?.source === 'archive'
            ? `capture${meta.label ? ` · ${meta.label}` : ''}${meta.capturedAt ? ` · ${meta.capturedAt.replace('T', ' ').replace('Z', ' UTC')}` : ''}`
            : meta?.capturedAt ? `sampled ${meta.capturedAt.replace('T', ' ').replace('Z', ' UTC')}` : ''}
        </span>
      </div>

      {err && <div className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{err}</div>}
      {meta?.warnings?.length > 0 && (
        <div className="rounded-lg border border-warning/40 bg-warning/10 px-3 py-2 text-xs text-warning">
          {meta.warnings.join(' · ')}
        </div>
      )}

      {maxedObj ? (
        <div className="min-h-0 flex-1">
          <StatePane obj={maxedObj} view={maxed.view} now={now} api={api} tick={model.samples} maxed
            pinned={pins.some((p) => samePane(p, maxed))} onView={changeView} onPin={togglePin}
            onMax={setMaxed} onDismiss={dismiss} onClose={() => setMaxed(null)} />
        </div>
      ) : (
        <div className="flex min-h-0 flex-1">
          <div
            ref={wrapRef}
            onPointerDown={(e) => {
              if (e.target.closest('[role="button"]')) return
              dragRef.current = { kind: 'pan', sx: e.clientX, sy: e.clientY, ox: view.x, oy: view.y }
            }}
            className="relative min-w-0 flex-1 overflow-hidden rounded-lg border bg-bg"
          >
            <div className="absolute left-0 top-0 origin-top-left" style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.z})` }}>
              <div className="flex items-start gap-4 p-2">
                {columns.map((col) => (
                  <div key={col.kind} className="flex flex-col gap-2" style={{ width: CARD_W }}>
                    <div className="flex items-baseline justify-between">
                      <span className="text-xs font-semibold text-fg">{col.kind}</span>
                      <span className="text-[10px] text-muted">{col.objects.length}</span>
                    </div>
                    {col.objects.map((o) => (
                      <StateCard key={o.key} obj={o} now={now} selected={sel?.key === o.key}
                        pinned={pins.some((p) => p.key === o.key)}
                        onSelect={(k) => setSel((s) => (s?.key === k ? s : { key: k, view: 'state' }))}
                        onPin={pinCard} onDismiss={dismiss} />
                    ))}
                  </div>
                ))}
              </div>
            </div>
            {columns.length === 0 && (
              <div className="absolute inset-0 flex items-center justify-center px-6 text-center text-sm text-muted">
                {!source ? 'Deploy a Kubernetes frame, or upload a pt-k8s-debug-collector cluster-dump.'
                  : model.samples === 0 ? (live ? 'Sampling the cluster…' : 'Reading the capture…')
                    : 'Nothing matches the filters.'}
              </div>
            )}
          </div>

          {/* The rail's width, dragged: a log wants far more room than a property list. */}
          <div
            onPointerDown={(e) => { dragRef.current = { kind: 'rail', sx: e.clientX, ow: railW } }}
            title="Drag to resize"
            className="mx-1 w-1.5 shrink-0 cursor-col-resize rounded hover:bg-primary/30"
          />

          <div className="flex shrink-0 flex-col gap-2 overflow-y-auto" style={{ width: railW }}>
            {filesOpen && meta?.source === 'archive' && (
              <CaptureFiles source={source} onClose={() => setFilesOpen(false)} />
            )}
            {panes.map((p) => (
              <StatePane key={paneId(p)} obj={objOf(p)} view={p.view} now={now} api={api} tick={model.samples}
                pinned={p.pinned} onView={changeView} onPin={togglePin} onMax={setMaxed} onDismiss={dismiss}
                onClose={p.pinned ? null : () => setSel(null)} />
            ))}
            {panes.length === 0 && !filesOpen && (
              <div className="rounded-lg border border-dashed p-3 text-xs text-muted">
                Click a card for everything it reports — its state, a container&apos;s log, or its YAML.
                Pin any of those and it stays here while you look at the rest.
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  )
}
