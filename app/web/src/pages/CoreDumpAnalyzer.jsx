import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Button, Badge, Card, inputCls } from '../components/ui.jsx'
import { Panel, PanelMaximize, EventLog } from '../components/DebugPanel.jsx'
import {
  gdbApi, gdbNodeApi, openGDBSession, GDB_STATUS_TONE, GDB_STATUS_TEXT,
  gdbTargetKey, shortFunc, libraryOf, sourceOf, isSystemFrame, formatBytes,
} from '../lib/gdbApi.js'

// Core Dump Analyzer — read a mysqld core dump from another server, here.
//
// The workflow this replaces is one command and a wall of text:
//
//   gdb -ex "set solib-search-path <libs>" -ex "set sysroot <libs>" \
//       -ex "thread apply all bt" /usr/sbin/mysqld <core>
//
// Everything that command does, DBCanvas has already arranged: the core and the crashed host's
// libraries are mounted on a Linux Client node, the matching debug symbols are installed, and the
// node is one exec away. What it cannot do is the reading — sixty threads and four hundred frames
// scroll past in one go, with no way to ask a frame what it was holding.
//
// So the layout is the one a stack actually wants: which core (and *is it the right one*) and
// which thread on the left, that thread's frames in the middle, the selected frame's arguments and
// locals on the right. Three things here do not exist in the command line at all:
//
//   - **The verdict.** gdb prints a backtrace whether or not the libraries match the process and
//     whether or not the binary is the build that crashed. Both produce fiction that looks exactly
//     like fact. The core list carries the build-id comparison and the list of mapped objects that
//     could not be found, before any stack is on screen to be believed.
//   - **The summary.** The top frame is almost never the bug — a stack overflow surfaces inside
//     _int_malloc, an assertion inside abort. The banner names the first frame below the C library
//     and any recursion it found, which for the crash class people bring cores for *is* the answer.
//   - **Collapsed recursion.** Four hundred identical frames render as one row with a count. The
//     top and the bottom of a recursion are the interesting parts and both are otherwise offscreen.

// FRAME_WINDOW mirrors gdbFrameWindow in gdbsess.go — how many frames one request brings back.
const FRAME_WINDOW = 200

export default function CoreDumpAnalyzer() {
  const [targets, setTargets] = useState(null)
  const [key, setKey] = useState('')
  const [err, setErr] = useState('')

  const [state, setState] = useState(null)
  const [log, setLog] = useState([])
  const [cores, setCores] = useState([])
  const [frames, setFrames] = useState([])
  const [more, setMore] = useState(false)
  const [frameIdx, setFrameIdx] = useState(0)
  const [vars, setVars] = useState([])
  const [source, setSource] = useState(null)   // { lines, from, line, file } | { error }
  const [watch, setWatch] = useState('')
  const [watches, setWatches] = useState([])
  const [cmd, setCmd] = useState('')
  const [output, setOutput] = useState('')
  const [busy, setBusy] = useState('')
  const [maxPanel, setMaxPanel] = useState(null)

  const sessionRef = useRef(null)
  const target = useMemo(
    () => (targets || []).find((t) => gdbTargetKey(t) === key) || null, [targets, key])
  const api = useMemo(
    () => (target ? gdbNodeApi(target.stackId, target.nodeId) : null), [target])

  // ---- targets ---------------------------------------------------------------

  useEffect(() => {
    gdbApi.targets()
      .then((list) => {
        setTargets(list)
        // The node panel's "Open analyzer" button leaves the node it wants here, because a hash
        // route carries no parameters.
        let want = ''
        try { want = sessionStorage.getItem('dbcanvas.gdbTarget') || '' } catch { /* private mode */ }
        try { sessionStorage.removeItem('dbcanvas.gdbTarget') } catch { /* ignore */ }
        const found = list.find((t) => gdbTargetKey(t) === want)
        setKey(gdbTargetKey(found || list[0] || {}))
      })
      .catch((e) => { setTargets([]); setErr(e.message) })
  }, [])

  // ---- the session -----------------------------------------------------------

  useEffect(() => {
    let live = true
    let timer = null

    function connect() {
      if (!live || !target) return
      const s = openGDBSession(target.stackId, target.nodeId, {
        state: (st) => { if (live) setState(st) },
        log: (line) => { if (live) setLog((prev) => [...prev.slice(-199), line]) },
        close: () => {
          if (!live) return
          setState((prev) => (prev ? { ...prev, status: 'idle' } : prev))
          timer = setTimeout(connect, 1500)
        },
      })
      sessionRef.current = s
    }

    sessionRef.current?.close()
    sessionRef.current = null
    setState(null); setLog([]); setFrames([]); setVars([]); setWatches([]); setOutput(''); setSource(null)
    if (!target) return undefined
    connect()

    return () => {
      live = false
      if (timer) clearTimeout(timer)
      sessionRef.current?.close()
      sessionRef.current = null
    }
  }, [target?.stackId, target?.nodeId]) // eslint-disable-line react-hooks/exhaustive-deps

  // ---- the core listing ------------------------------------------------------

  useEffect(() => {
    if (!api) return
    setCores([])
    api.cores()
      .then((r) => setCores(r.cores || []))
      .catch((e) => setErr(`Could not read ${target?.coreDir || 'the core directory'}: ${e.message}`))
  }, [api]) // eslint-disable-line react-hooks/exhaustive-deps

  const call = useCallback(async (c, label) => {
    const s = sessionRef.current
    if (!s) return null
    setBusy(label || c.cmd)
    setErr('')
    try {
      return await s.call(c)
    } catch (e) {
      setErr(e.message)
      return null
    } finally {
      setBusy('')
    }
  }, [])

  const status = state?.status || 'idle'
  const thread = state?.thread || ''

  // When a core opens, or the thread changes, fetch that thread's stack.
  const loadFrames = useCallback(async (tid, offset = 0) => {
    const r = await call({ cmd: 'backtrace', thread: tid, offset }, 'backtrace')
    if (!r) return
    const next = r.frames || []
    setFrames((prev) => (offset === 0 ? next : [...prev, ...next]))
    setMore(!!r.more)
    if (offset === 0) setFrameIdx(0)
    return next
  }, [call])

  // Open on the frame that crashed, not on frame 0.
  //
  // On a MySQL core frame 0 is always the crash handler — the server catches its own SIGSEGV — so
  // landing there shows a libc frame with no source and no arguments, which is the least useful
  // thing on the page. The analysis already knows which frame the program was actually in; this
  // just follows it, the same way a debugger follows a stop.
  const followed = useRef('')
  useEffect(() => {
    const culprit = state?.verdict?.culprit
    if (!culprit || frames.length === 0) return
    const key = `${state.core}/${state.thread}/${culprit.level}`
    if (followed.current === key) return
    const i = frames.findIndex((f) => f.level === culprit.level)
    if (i >= 0) { followed.current = key; setFrameIdx(i) }
  }, [state?.verdict?.culprit?.level, state?.core, state?.thread, frames.length]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (status !== 'ready' || !thread) { setFrames([]); return }
    loadFrames(thread, 0)
  }, [status, thread]) // eslint-disable-line react-hooks/exhaustive-deps

  // The selected frame's variables, and the watches, re-read whenever either moves.
  const selected = frames[frameIdx] || null
  useEffect(() => {
    if (status !== 'ready' || !thread || !selected) { setVars([]); return }
    let live = true
    sessionRef.current?.call({ cmd: 'variables', thread, frame: selected.level })
      .then((r) => { if (live) setVars(r.variables || []) })
      .catch(() => { if (live) setVars([]) })
    return () => { live = false }
  }, [status, thread, selected?.level]) // eslint-disable-line react-hooks/exhaustive-deps

  // The source of the selected frame. This is the pane that turns "fts0que.cc:2815" from a
  // coordinate into an explanation, and it only works because the debugsource package installs to
  // exactly the path the DWARF records.
  useEffect(() => {
    if (status !== 'ready' || !selected?.file || !selected?.line) { setSource(null); return }
    let live = true
    sessionRef.current?.call({ cmd: 'source', file: selected.file, line: selected.line, span: 14 })
      .then((r) => { if (live) setSource(r) })
      .catch((e) => { if (live) setSource({ error: e.message }) })
    return () => { live = false }
  }, [status, selected?.file, selected?.line]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    const s = sessionRef.current
    if (!s || status !== 'ready' || !selected || watches.length === 0) return
    let live = true
    Promise.all(watches.map((wv) => s.call({ cmd: 'evaluate', thread, frame: selected.level, expr: wv.expr })
      .then((r) => ({ expr: wv.expr, value: r.value }))
      .catch((e) => ({ expr: wv.expr, error: e.message }))))
      .then((next) => { if (live) setWatches(next) })
    return () => { live = false }
  }, [status, thread, selected?.level]) // eslint-disable-line react-hooks/exhaustive-deps

  const addWatch = () => {
    const expr = watch.trim()
    const s = sessionRef.current
    if (!expr || !s || !selected) return
    setWatch('')
    setWatches((prev) => [...prev.filter((wv) => wv.expr !== expr), { expr }])
    const settle = (v) => setWatches((prev) => prev.map((wv) => (wv.expr === expr ? v : wv)))
    s.call({ cmd: 'evaluate', thread, frame: selected.level, expr })
      .then((r) => settle({ expr, value: r?.value }))
      .catch((e) => settle({ expr, error: e.message }))
  }

  const runConsole = async () => {
    const text = cmd.trim()
    if (!text) return
    const r = await call({ cmd: 'console', text }, 'console')
    if (r) { setOutput(r.output || '(no output)'); setCmd('') }
  }

  // ---- render ----------------------------------------------------------------

  if (targets === null) return <div className="p-6 text-sm text-muted">Loading…</div>

  return (
    <div className="flex flex-col gap-3">
      <Header
        targets={targets} value={key} onChange={setKey}
        state={state} busy={busy}
        onClose={() => call({ cmd: 'close' }, 'close')}
        onRefresh={() => api?.cores().then((r) => setCores(r.cores || [])).catch((e) => setErr(e.message))}
      />

      {err && (
        <div className="flex items-start justify-between gap-3 rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">
          <span>{err}</span>
          <button onClick={() => setErr('')} className="shrink-0 opacity-70 hover:opacity-100"><Icon.Close size={14} /></button>
        </div>
      )}

      {targets.length === 0 ? <NoTargets /> : (
        <>
          <CrashSummary state={state} frames={frames} core={cores.find((c) => c.name === state?.core)} />
          <PanelMaximize value={maxPanel} onChange={setMaxPanel}>
            <div className="relative grid h-[calc(100vh-17rem)] min-h-[560px] grid-cols-12 gap-3 overflow-hidden">
              <div className="col-span-3 flex min-h-0 flex-col gap-3 overflow-y-auto">
                <CoreList cores={cores} open={state?.core} busy={busy} target={target}
                  onOpen={(name) => call({ cmd: 'open', core: name }, 'open')} />
                <ThreadList threads={state?.threads || []} selected={thread} signal={state?.signal}
                  onSelect={(id) => call({ cmd: 'backtrace', thread: id, offset: 0 }, 'backtrace')
                    .then((r) => { if (r) { setFrames(r.frames || []); setMore(!!r.more); setFrameIdx(0) } })} />
              </div>

              <div className="col-span-6 flex min-h-0 flex-col gap-3">
                <Backtrace
                  frames={frames} selected={frameIdx} onSelect={setFrameIdx}
                  more={more} busy={busy} state={state}
                  onMore={() => loadFrames(thread, frames.length)} />
                <SourceView source={source} frame={selected} />
              </div>

              <div className="col-span-3 flex min-h-0 flex-col gap-3 overflow-y-auto">
                <FrameVars vars={vars} frame={selected} />
                <EvaluateBox
                  value={watch} onChange={setWatch} onAdd={addWatch}
                  watches={watches} onRemove={(expr) => setWatches((p) => p.filter((wv) => wv.expr !== expr))} />
                <ConsoleBox
                  value={cmd} onChange={setCmd} onRun={runConsole} output={output} busy={busy}
                  allowShell={!!state?.allowShell}
                  onAllowShell={(on) => sessionRef.current?.send({ cmd: 'allowShell', on })} />
                <EventLog lines={log} />
              </div>
            </div>
          </PanelMaximize>
        </>
      )}
    </div>
  )
}

// ---------------------------------------------------------------- header

export function Header({ targets, value, onChange, state, busy, onClose, onRefresh }) {
  const status = state?.status || 'idle'
  return (
    <div className="flex flex-wrap items-center gap-3">
      <div>
        <h1 className="text-lg font-semibold text-fg">Core Dump Analyzer</h1>
        <p className="text-xs text-muted">
          Read a crashed server's core dump — threads, stack, arguments — without touching the server.
        </p>
      </div>
      <div className="ml-auto flex flex-wrap items-center gap-2">
        <select className={`${inputCls} w-72`} value={value} onChange={(e) => onChange(e.target.value)}>
          {(targets || []).map((t) => (
            <option key={`${t.stackId}/${t.nodeId}`} value={`${t.stackId}/${t.nodeId}`}>
              {t.stackName} · {t.label} · {(t.product || 'ps').toUpperCase()} {t.version || t.major}
            </option>
          ))}
        </select>
        <Badge tone={GDB_STATUS_TONE[status] || 'muted'}>{GDB_STATUS_TEXT[status] || status}</Badge>
        <Button variant="outline" size="sm" onClick={onRefresh} disabled={!!busy}>
          <Icon.Both size={14} /> Rescan
        </Button>
        <Button variant="outline" size="sm" disabled={status !== 'ready' || !!busy} onClick={onClose}>
          <Icon.Close size={14} /> Close core
        </Button>
      </div>
    </div>
  )
}

export function NoTargets() {
  return (
    <Card title="Nothing to analyse yet">
      <div className="space-y-2 text-sm text-muted">
        <p>
          A <span className="font-medium text-fg">Linux Client</span> node has to be deployed for
          core-dump analysis. In the Database Stacks designer, add one, tick{' '}
          <span className="font-medium text-fg">Use this client for core-dump analysis</span>, give it
          the host directory holding the core file and the host directory holding the crashed
          server's libraries, and pick the product and version that crashed.
        </p>
        <p>
          Both directories are mounted read-only, so an 800 MB core file is read where it lies rather
          than copied. Set the node's OS to the one the crashed server ran: the debug symbols are
          per-build, and an el8 build and an el9 build of one version do not share them.
        </p>
      </div>
    </Card>
  )
}

// CrashSummary is the answer to "what killed it", above everything else on the page.
//
// It exists because the top frame is the wrong place to look and everybody looks there anyway. A
// stack overflow ends inside the allocator, an assertion inside abort, a double free inside libc —
// the frame that names the bug is the first one that belongs to the program. The verdict line next
// to it is the other half: a backtrace assembled from the wrong libraries is not a worse answer, it
// is a different program's answer.
export function CrashSummary({ state, frames, core }) {
  if (!state || state.status === 'idle') return null
  if (state.status === 'loading') {
    return (
      <div className="rounded-lg border border-accent/30 bg-accent/10 px-3 py-2 text-[11px] leading-snug text-muted">
        <span className="font-medium text-fg">Reading the core file.</span> {state.detail || ''} A core
        is the size of the server's memory, so this takes a few seconds — gdb is mapping it, not copying it.
      </div>
    )
  }
  if (state.status === 'error') {
    return (
      <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-[11px] leading-snug text-danger">
        <span className="font-medium">gdb could not read that core.</span> {state.detail}
      </div>
    )
  }

  const v = state.verdict
  const missing = core?.missing?.length || 0
  const untrustworthy = (core && !core.buildIdMatch && core.buildId) || missing > 0 || !!state.symbols

  return (
    <div className={`rounded-lg border px-3 py-2 text-[11px] leading-snug ${
      untrustworthy ? 'border-warning/40 bg-warning/10' : 'border-accent/30 bg-accent/10'}`}>
      {v ? (
        <>
          <p className="text-sm font-medium text-fg">{v.headline}</p>
          {v.why && <p className="mt-1 text-muted">{v.why}</p>}

          {v.query?.text && (
            <div className="mt-2">
              <p className="text-[10px] uppercase tracking-wide text-muted">
                The thread was running
                {v.query.length > v.query.text.length && <> — {v.query.length} bytes, truncated below</>}
              </p>
              <pre className="mt-1 max-h-24 overflow-auto whitespace-pre-wrap break-all rounded bg-surface2 p-2 font-mono text-[10px] text-fg">
                {v.query.text}
              </pre>
            </div>
          )}

          {v.trigger && (
            <div className="mt-2">
              <p className="text-[10px] uppercase tracking-wide text-muted">
                What set it off — <span className="font-mono normal-case">{v.trigger.name}</span> in{' '}
                <span className="font-mono normal-case">{shortFunc(v.trigger.func)}</span>, frame #{v.trigger.frame}
                {v.trigger.where && <> at <span className="font-mono normal-case">{v.trigger.where}</span></>}
                {v.trigger.extra && <> · <span className="font-mono normal-case">{v.trigger.extra}</span></>}
              </p>
              <pre className="mt-1 max-h-24 overflow-auto whitespace-pre-wrap break-all rounded bg-surface2 p-2 font-mono text-[10px] text-fg">
                {v.trigger.value}
              </pre>
            </div>
          )}

          {v.evidence?.length > 0 && (
            <ul className="mt-2 space-y-0.5 text-muted">
              {v.evidence.map((e, i) => (
                <li key={i} className="flex gap-1.5">
                  <span className="shrink-0 opacity-60">·</span>
                  <span>{e}</span>
                </li>
              ))}
            </ul>
          )}
        </>
      ) : (
        // No verdict yet, or the analysis could not run. Say what is known rather than nothing.
        <p className="text-sm font-medium text-fg">
          {state.signal || 'The process'}{state.signalText ? ` — ${state.signalText}` : ''} on thread{' '}
          <span className="font-mono">{state.thread}</span> of {state.totalThreads}.
        </p>
      )}

      <p className="mt-2 border-t pt-1.5 text-muted">
        {state.symbols
          ? <span className="text-warning">{state.symbols}</span>
          : <span>Symbols resolved for {state.target?.binary || 'the executable'}
            {state.target?.binaryFrom === 'mounted' && ' — the copy taken off the crashed host'}.</span>}
        {state.target?.status && state.target.status !== 'ready' && (
          <span className="text-warning"> {state.target.status}.</span>
        )}
        {core && !core.buildIdMatch && core.buildId && (
          <span className="text-danger"> The installed binary is a different build from the one that
            crashed — this backtrace is not trustworthy.</span>
        )}
        {missing > 0 && (
          <span className="text-warning"> {missing} mapped object{missing === 1 ? '' : 's'} could not be
            found in the mounted library directory.</span>
        )}
      </p>
    </div>
  )
}

// ---------------------------------------------------------------- left column

// CoreList is the pre-flight. Each row says what the file is and whether it can be believed, and
// those are different questions — a core opens fine against the wrong symbols.
export function CoreList({ cores, open, busy, target, onOpen }) {
  return (
    <Panel id="cores" title="Core files" className="shrink-0"
      action={<span className="font-mono text-[10px] text-muted">{target?.coreDir || ''}</span>}
      bodyClass="max-h-64 overflow-auto p-2">
      {(!cores || cores.length === 0) ? (
        <p className="px-1 py-2 text-[11px] text-muted">
          Nothing in the mounted directory. Copy the core file into it on the host — the mount is live.
        </p>
      ) : (
        <div className="space-y-1">
          {cores.map((c) => {
            const bad = c.buildId && !c.buildIdMatch
            const miss = c.missing?.length || 0
            return (
              <button key={c.name} onClick={() => onOpen(c.name)} disabled={!!busy}
                className={`block w-full rounded px-2 py-1.5 text-left transition disabled:opacity-50 ${
                  c.name === open ? 'bg-primary/15' : 'hover:bg-surface2'}`}>
                <span className={`block truncate font-mono text-[11px] ${c.name === open ? 'text-primary' : 'text-fg'}`}
                  title={c.name}>{c.name}</span>
                <span className="block text-[10px] text-muted">
                  {formatBytes(c.size)}
                  {c.executable && <> · {c.executable.slice(c.executable.lastIndexOf('/') + 1)}</>}
                  {c.signal && <> · {c.signal}</>}
                </span>
                {c.note && <span className="block text-[10px] text-muted">{c.note}</span>}
                {(bad || miss > 0) && (
                  <span className="mt-0.5 flex flex-wrap gap-1">
                    {bad && <Badge tone="danger">different build</Badge>}
                    {miss > 0 && <Badge tone="warning">{miss} object{miss === 1 ? '' : 's'} missing</Badge>}
                  </span>
                )}
              </button>
            )
          })}
        </div>
      )}
    </Panel>
  )
}

// ThreadList puts the signalled thread first and labels the rest by their top frame. Sixty threads
// named only "Thread 0x7f… (LWP 9756)" is a list nobody can read; what each one was *doing* is.
export function ThreadList({ threads, selected, signal, onSelect }) {
  const ordered = useMemo(() => {
    const list = [...(threads || [])]
    list.sort((a, b) => (a.id === selected ? -1 : b.id === selected ? 1 : Number(a.id) - Number(b.id)))
    return list
  }, [threads, selected])
  return (
    <Panel id="threads" title={`Threads${threads?.length ? ` (${threads.length})` : ''}`}
      className="flex-1" bodyClass="min-h-0 flex-1 overflow-auto p-2">
      {(!threads || threads.length === 0) ? (
        <p className="px-1 py-2 text-[11px] text-muted">Open a core file to see its threads.</p>
      ) : (
        <div className="space-y-0.5">
          {ordered.map((t) => (
            <button key={t.id} onClick={() => onSelect(t.id)}
              className={`block w-full rounded px-2 py-1 text-left ${
                t.id === selected ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2 hover:text-fg'}`}>
              <span className="flex items-baseline gap-1.5">
                <span className="shrink-0 font-mono text-[11px]">#{t.id}</span>
                {t.id === selected && signal && <Badge tone="danger">{signal}</Badge>}
                {t.name && <span className="truncate text-[10px]">{t.name}</span>}
              </span>
              <span className="block truncate font-mono text-[10px] opacity-80"
                title={t.frame?.func || t.target}>
                {t.frame ? shortFunc(t.frame.func) : t.target}
              </span>
            </button>
          ))}
        </div>
      )}
    </Panel>
  )
}

// ---------------------------------------------------------------- the stack

// Backtrace is the middle column: one row per frame, with a repeating cycle folded into a single
// row carrying its count. Frames in the C runtime are dimmed, because in a crash they are the
// scaffolding rather than the fault.
export function Backtrace({ frames, selected, onSelect, more, busy, state, onMore }) {
  const empty = !frames || frames.length === 0
  return (
    <Panel id="stack" title={state?.core ? `Stack — thread ${state.thread}` : 'Stack'}
      className="min-h-0 flex-1"
      action={<span className="text-[10px] text-muted">{empty ? ''
        : state?.verdict?.depth ? `${frames.length} of ${state.verdict.depth} frames`
        : `${frames.length} frame${frames.length === 1 ? '' : 's'}`}</span>}
      bodyClass="min-h-0 flex-1 overflow-auto p-0">
      {empty ? (
        <p className="p-3 text-[11px] text-muted">
          Pick a core file on the left. Its threads and their stacks appear once gdb has read it.
        </p>
      ) : (
        <div className="font-mono text-[11px]">
          {frames.map((f, i) => {
            const sys = isSystemFrame(f)
            return (
              <button key={`${f.level}-${i}`} onClick={() => onSelect(i)}
                className={`flex w-full items-baseline gap-2 border-b px-3 py-1 text-left ${
                  i === selected ? 'bg-primary/10' : 'hover:bg-surface2/60'}`}>
                <span className="w-10 shrink-0 tabular-nums text-muted">#{f.level}</span>
                <span className={`min-w-0 flex-1 break-all ${sys ? 'text-muted' : 'text-fg'}`} title={f.func}>
                  {f.func || '??'}
                  {f.repeat > 1 && (
                    // The count shown is the *stack's*, not the window's, whenever the analysis
                    // has one — a pane holding 200 of 1,085 frames sees a cycle repeat 39 times
                    // and the whole stack sees it repeat 212, and two numbers on one screen that
                    // disagree is worse than either alone.
                    <span className="ml-2 rounded bg-warning/20 px-1 text-[10px] text-warning"
                      title={cycleTotal(state, f) !== f.repeat
                        ? `${f.repeat} times in the frames loaded here; ${cycleTotal(state, f)} in the whole stack`
                        : `${f.repeat} consecutive copies of this cycle`}>
                      ×{cycleTotal(state, f)}
                    </span>
                  )}
                </span>
                <span className="shrink-0 text-[10px] text-muted">{sourceOf(f)}</span>
              </button>
            )
          })}
          {more && (
            <div className="p-2">
              <Button variant="outline" size="sm" disabled={!!busy} onClick={onMore}>
                Load {FRAME_WINDOW} more frames
              </Button>
            </div>
          )}
        </div>
      )}
    </Panel>
  )
}

// cycleTotal is how many times this collapsed row's cycle runs in the whole stack, falling back to
// the count within the frames actually loaded when there is no analysis to ask.
function cycleTotal(state, frame) {
  const v = state?.verdict
  if (v?.repeats && v.cycle?.some((fn) => frame.func?.startsWith(fn))) return v.repeats
  return frame.repeat
}

// SourceView shows the code the selected frame is standing on.
//
// This is the pane that answers "why", and it exists because a file and a line number are not an
// explanation on their own — `temptable/table.h:190` tells you nothing until you can see that the
// line is `return m_rows.size();` and the frame's `this` is 0x0. It needs the *-debugsource
// package, which installs the code to exactly the path the debug symbols record, so no path
// mapping is involved and a missing pane means a missing package.
export function SourceView({ source, frame }) {
  const hasLoc = !!frame?.file && !!frame?.line
  // Reveal the line the frame is on. The window is centred on it, but the pane is shorter than the
  // window, so without this the crashing line sits below the fold and the pane shows the lines
  // before it — which is the one thing it must not do.
  const marker = useRef(null)
  useEffect(() => {
    if (marker.current) marker.current.scrollIntoView({ block: 'center' })
  }, [source?.file, source?.line, source?.from])
  return (
    <Panel id="source" title={hasLoc ? sourceOf(frame) : 'Source'} className="h-64 shrink-0"
      action={frame?.file && <span className="truncate font-mono text-[10px] text-muted" title={frame.file}>
        {frame.file.replace(/^.*\/percona-server-[^/]*\//, '')}</span>}
      bodyClass="min-h-0 flex-1 overflow-auto p-0">
      {!hasLoc ? (
        <p className="p-3 text-[11px] text-muted">
          This frame has no source location — it is in a library, or in code compiled without debug
          information. Pick a frame that shows a <span className="font-mono">file:line</span>.
        </p>
      ) : source?.error ? (
        <p className="p-3 text-[11px] text-muted">{source.error}</p>
      ) : !source?.lines ? (
        <p className="p-3 text-[11px] text-muted">Reading {sourceOf(frame)}…</p>
      ) : (
        <div className="font-mono text-[11px] leading-[1.45]">
          {source.lines.map((ln, i) => {
            const n = source.from + i
            const here = n === source.line
            return (
              <div key={n} ref={here ? marker : null}
                className={`flex w-max min-w-full ${here ? 'bg-warning/20' : ''}`}>
                <span className={`sticky left-0 z-10 w-12 shrink-0 select-none border-r px-2 text-right ${
                  here ? 'bg-warning/20 font-semibold text-warning' : 'bg-surface text-muted'}`}>{n}</span>
                <pre className="whitespace-pre px-2 text-fg">{ln || ' '}</pre>
              </div>
            )
          })}
        </div>
      )}
    </Panel>
  )
}

// ---------------------------------------------------------------- right column

export function FrameVars({ vars, frame }) {
  const args = (vars || []).filter((v) => v.arg)
  const locals = (vars || []).filter((v) => !v.arg)
  return (
    <Panel id="vars" title="Frame" className="shrink-0" bodyClass="max-h-72 overflow-auto p-3">
      {!frame ? (
        <p className="text-[11px] text-muted">No frame selected.</p>
      ) : (
        <div className="space-y-2 text-[11px]">
          <p className="break-all font-mono text-fg">{frame.func || '??'}</p>
          <p className="font-mono text-[10px] text-muted">
            {frame.addr}{sourceOf(frame) && <> · {sourceOf(frame)}</>}
            {frame.from && <> · {libraryOf(frame.from)}</>}
          </p>
          {(vars || []).length === 0 && (
            <p className="text-muted">
              No arguments or locals here. That is normal for a frame in a library, and for every
              frame when the executable has no separate debug symbols installed.
            </p>
          )}
          <VarGroup label="Arguments" vars={args} />
          <VarGroup label="Locals" vars={locals} />
        </div>
      )}
    </Panel>
  )
}

export function VarGroup({ label, vars }) {
  if (!vars || vars.length === 0) return null
  return (
    <div>
      <p className="mb-1 text-[10px] uppercase tracking-wide text-muted">{label}</p>
      <ul className="space-y-1">
        {vars.map((v) => (
          <li key={v.name} className="flex items-start gap-2">
            <span className="shrink-0 font-mono text-fg">{v.name}</span>
            <span className="min-w-0 flex-1 whitespace-pre-wrap break-all font-mono text-muted">{v.value}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}

export function EvaluateBox({ value, onChange, onAdd, watches, onRemove }) {
  return (
    <Panel id="evaluate" title="Evaluate" className="shrink-0">
      <div className="flex gap-1">
        <input className={inputCls} placeholder="node->type"
          value={value} onChange={(e) => onChange(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') onAdd() }} />
        <Button size="sm" variant="outline" onClick={onAdd}><Icon.Plus size={14} /></Button>
      </div>
      <div className="mt-2 space-y-1">
        {(watches || []).map((wv) => (
          <div key={wv.expr} className="group flex items-start gap-2 text-[11px]">
            <span className="shrink-0 font-mono text-fg">{wv.expr}</span>
            <span className={`min-w-0 flex-1 whitespace-pre-wrap break-all font-mono ${wv.error ? 'text-danger' : 'text-muted'}`}>
              {wv.error || wv.value}</span>
            <button onClick={() => onRemove(wv.expr)} className="shrink-0 text-muted opacity-0 group-hover:opacity-100 hover:text-danger">
              <Icon.Close size={12} />
            </button>
          </div>
        ))}
      </div>
      <p className="mt-2 text-[10px] text-muted">
        Expressions are read in the selected frame and re-read when you change frames. A core file is
        a dead process, so nothing here can run the program's code.
      </p>
    </Panel>
  )
}

// ConsoleBox is the escape hatch: anything the panels do not cover, said to gdb directly.
//
// The tick is not ceremony. gdb is a programmable debugger — `shell` is a root shell on the node,
// `python` is an interpreter, `source` loads a script — and none of those is part of reading a
// stack. They are refused until somebody says otherwise, and the session log records it when they
// are used.
export function ConsoleBox({ value, onChange, onRun, output, busy, allowShell, onAllowShell }) {
  return (
    <Panel id="console" title="gdb console" className="shrink-0">
      <div className="flex gap-1">
        <input className={`${inputCls} font-mono`} placeholder="info sharedlibrary"
          value={value} onChange={(e) => onChange(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') onRun() }} />
        <Button size="sm" variant="outline" disabled={!!busy} onClick={onRun}>
          <Icon.Play size={14} />
        </Button>
      </div>
      {output && (
        <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap break-all rounded bg-surface2 p-2 font-mono text-[10px] text-muted">
          {output}
        </pre>
      )}
      <label className="mt-2 flex items-start gap-2 text-[11px] text-muted">
        <input type="checkbox" className="mt-0.5" checked={!!allowShell}
          onChange={(e) => onAllowShell(e.target.checked)} />
        <span>
          Allow shell commands — <span className="font-mono">shell</span>,{' '}
          <span className="font-mono">python</span> and <span className="font-mono">source</span> then
          <span className="font-medium text-fg"> run as root</span> on this node.
        </span>
      </label>
    </Panel>
  )
}
