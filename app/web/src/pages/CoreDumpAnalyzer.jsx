import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Button, Badge, Card, inputCls } from '../components/ui.jsx'
import { Panel, PanelMaximize, EventLog } from '../components/DebugPanel.jsx'
import {
  gdbApi, gdbNodeApi, openGDBSession, GDB_STATUS_TONE, GDB_STATUS_TEXT,
  gdbTargetKey, shortFunc, libraryOf, sourceOf, isSystemFrame, formatBytes,
  canExpand, valueSummary, groupThreadStacks, threadStacksAsText,
} from '../lib/gdbApi.js'
import { useHandoff } from '../lib/handoff.js'

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

// FULL_WINDOW mirrors gdbFullVarsMax in gdbsess.go — how many frames `bt full` reads locals for.
// Locals are a round trip per frame and gdb is not fast at them, so the rest of the stack says it
// was not read rather than waiting for something that is not coming.
const FULL_WINDOW = 40

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
  const [source, setSource] = useState(null)   // { lines, from, line, file, path, truncated } | { error }
  const [stackMode, setStackMode] = useState('thread') // 'thread' | 'all'
  const [stacks, setStacks] = useState(null)   // every thread's stack — `thread apply all bt`
  const [locals, setLocals] = useState(false)  // the `full` in `bt full`
  const [frameVars, setFrameVars] = useState({}) // frame level -> that frame's arguments and locals
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
      .then(setTargets)
      .catch((e) => { setTargets([]); setErr(e.message) })
  }, [])

  // The node panel's "Open analyzer" button leaves the node it wants here, because
  // a hash route carries no parameters. Held as state rather than consumed inside
  // the fetch above: arriving at a tab that is already open re-runs neither the
  // fetch nor a mount effect.
  const [want, setWant] = useState('')
  useHandoff('dbcanvas.gdbTarget', setWant)
  useEffect(() => {
    // targets is null until the fetch above lands, so this must not assume an array.
    // An effect that throws is caught by nothing: React unmounts the root and the
    // page goes blank, with the console the only clue. That is exactly what shipped
    // here and in the Operator Debugger when the selection moved out of the
    // fetch's .then() and into an effect of its own — see smoke/browser.jsx, which
    // mounts these pages in a real browser to catch it.
    if (!targets?.length) return
    const found = targets.find((t) => gdbTargetKey(t) === want)
    setKey(gdbTargetKey(found || targets[0] || {}))
  }, [targets, want])

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
    setStacks(null); setFrameVars({})
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
    setFrameVars({})
    loadFrames(thread, 0)
  }, [status, thread]) // eslint-disable-line react-hooks/exhaustive-deps

  // `thread apply all bt`, which is the command everybody runs first and the one this page had
  // no answer to: a stack for every thread, in one reply. It is a round trip per thread on the
  // server, so it is fetched when the view is opened rather than kept up to date — a core file
  // does not change, so once is enough.
  useEffect(() => { setStacks(null) }, [state?.core])
  useEffect(() => {
    if (stackMode !== 'all' || status !== 'ready' || stacks) return undefined
    let live = true
    setBusy('threads')
    sessionRef.current?.call({ cmd: 'threads' })
      .then((r) => { if (live) setStacks(r.stacks || []) })
      .catch((e) => { if (live) { setErr(e.message); setStacks([]) } })
      .finally(() => { if (live) setBusy('') })
    return () => { live = false }
  }, [stackMode, status, stacks, state?.core]) // eslint-disable-line react-hooks/exhaustive-deps

  // The `full` in `bt full`: every loaded frame's arguments and locals at once, so a stack can be
  // read without clicking thirty times to find which frame was holding the bad value.
  useEffect(() => {
    if (!locals || status !== 'ready' || !thread || frames.length === 0) return undefined
    let live = true
    const levels = frames.slice(0, FULL_WINDOW).map((f) => f.level)
    // In chunks, top down, rather than one request for forty frames. gdb reads locals one frame
    // at a time and takes a second or two over each, so a single request is three quarters of a
    // minute during which every row says "reading…" — while the frames anybody is looking at,
    // the ones at the top, were ready in the first two seconds.
    const CHUNK = 8
    ;(async () => {
      for (let i = 0; i < levels.length && live; i += CHUNK) {
        try {
          const r = await sessionRef.current?.call({ cmd: 'framevars', thread, levels: levels.slice(i, i + CHUNK) })
          if (!live) return
          setFrameVars((prev) => ({ ...prev, ...(r?.frames || {}) }))
        } catch {
          return // the session went away; the rows keep saying they are waiting, which they are
        }
      }
    })()
    return () => { live = false }
  }, [locals, status, thread, frames.length]) // eslint-disable-line react-hooks/exhaustive-deps

  // The selected frame's variables, and the watches, re-read whenever either moves.
  const selected = frames[frameIdx] || null
  useEffect(() => {
    if (status !== 'ready' || !thread || !selected) { setVars([]); return }
    let live = true
    // null while it is in flight, so the panel can tell "not read yet" from "this frame has
    // none" — two sentences that look identical as an empty list, and gdb takes seconds over a
    // frame the first time it is asked.
    setVars(null)
    sessionRef.current?.call({ cmd: 'variables', thread, frame: selected.level })
      .then((r) => { if (live) setVars(r.variables || []) })
      .catch(() => { if (live) setVars([]) })
    return () => { live = false }
  }, [status, thread, selected?.level]) // eslint-disable-line react-hooks/exhaustive-deps

  // The source of the selected frame — the whole file, not a window around the line. This is the
  // pane that turns "fts0que.cc:2815" from a coordinate into an explanation, and it only works
  // because the debugsource package installs to exactly the path the DWARF records.
  useEffect(() => {
    if (status !== 'ready' || !selected?.file || !selected?.line) { setSource(null); return }
    let live = true
    sessionRef.current?.call({ cmd: 'source', file: selected.file, line: selected.line })
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

  // Watching an expression is also what a click on a variable does, so it is a callback rather
  // than an event handler on the one input that used to be the only way in.
  const addWatchExpr = useCallback((raw) => {
    const expr = (raw || '').trim()
    const s = sessionRef.current
    if (!expr || !s || !selected) return
    setWatches((prev) => [...prev.filter((wv) => wv.expr !== expr), { expr }])
    const settle = (v) => setWatches((prev) => prev.map((wv) => (wv.expr === expr ? v : wv)))
    s.call({ cmd: 'evaluate', thread, frame: selected.level, expr })
      .then((r) => settle({ expr, value: r?.value }))
      .catch((e) => settle({ expr, error: e.message }))
  }, [thread, selected?.level]) // eslint-disable-line react-hooks/exhaustive-deps

  const addWatch = () => {
    const expr = watch.trim()
    if (!expr) return
    setWatch('')
    addWatchExpr(expr)
  }

  // Opening a value: what is inside this struct, or what does this pointer point at. The reply is
  // the members and — crucially — the expression that reads each of them, so a member can itself
  // be opened, watched or evaluated without the page having to keep gdb handles alive.
  const expand = useCallback((expr, level) => {
    const s = sessionRef.current
    if (!s) return Promise.reject(new Error('the gdb session is not open'))
    const frame = level === undefined ? selected?.level : level
    if (frame === undefined || frame === null) return Promise.reject(new Error('no frame is selected'))
    return s.call({ cmd: 'children', thread, frame, expr }).then((r) => r.children || [])
  }, [thread, selected?.level]) // eslint-disable-line react-hooks/exhaustive-deps

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
          <CrashSummary state={state} frames={frames} core={cores.find((c) => c.name === state?.core)}
            onExpand={expand} onWatch={addWatchExpr} />
          <GdbRecipe recipe={state?.recipe} />
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
                  onMore={() => loadFrames(thread, frames.length)}
                  mode={stackMode} onMode={setStackMode}
                  stacks={stacks} locals={locals} onLocals={setLocals} frameVars={frameVars}
                  onThread={(id) => {
                    setStackMode('thread')
                    call({ cmd: 'backtrace', thread: id, offset: 0 }, 'backtrace')
                      .then((r) => { if (r) { setFrames(r.frames || []); setMore(!!r.more); setFrameIdx(0) } })
                  }} />
                <SourceView source={source} frame={selected} />
              </div>

              <div className="col-span-3 flex min-h-0 flex-col gap-3 overflow-y-auto">
                <FrameVars vars={vars} frame={selected} onExpand={expand} onWatch={addWatchExpr} />
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
export function CrashSummary({ state, frames, core, onExpand, onWatch }) {
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

          <CrashState state={v.state} fault={v.fault} onExpand={onExpand} onWatch={onWatch} />

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

// CrashState is the answer to "what was it evaluating, and what was in it".
//
// Everything above this in the banner reasons about frames: which one is the bug, what class of
// crash it is. This is the evidence underneath that reasoning, and it is the part a backtrace
// cannot carry at all — the line of code the program was on, and the values that line was reading,
// side by side.
//
// The fault line above it is the kernel's own record, out of the core's siginfo note: the address
// the program actually touched. A crash stack is full of plausible pointers and that address says
// which of them it was, which is the difference between "a null pointer somewhere in here" and
// "it dereferenced `thd`, which was 0x0".
//
// Every value is clickable for the same reason it is in the Frame panel: `this = 0x0` ends the
// question, but `entry = {…}` starts one.
export function CrashState({ state, fault, onExpand, onWatch }) {
  if (!state && !fault) return null
  const before = state?.before || []
  const firstLine = state ? state.line - before.length : 0
  return (
    <div className="mt-2 space-y-2">
      {fault && (
        <p className="text-muted">
          <span className="text-[10px] uppercase tracking-wide">The address it touched</span>{' '}
          <span className="font-mono text-fg">{fault.addr}</span>
          {fault.codeText && <> — {fault.codeText}</>}
          {fault.note && <>. {fault.note}</>}
          {fault.through && (
            <>. That is <span className="font-mono text-fg">{fault.through.name}</span>
              {' '}= <span className="font-mono text-fg">{fault.through.value}</span>
              {fault.through.offset > 0 && <> plus {fault.through.offset} bytes</>}
              , {fault.through.arg ? 'an argument of' : 'a local in'}{' '}
              <span className="font-mono">{shortFunc(fault.through.func)}</span> (frame #{fault.through.frame})
            </>
          )}
        </p>
      )}

      {state && (
        <div>
          <p className="text-[10px] uppercase tracking-wide text-muted">
            What it was evaluating — <span className="font-mono normal-case">{shortFunc(state.func)}</span>,
            frame #{state.frame}
            {state.file && <> at <span className="font-mono normal-case">{state.file.slice(state.file.lastIndexOf('/') + 1)}:{state.line}</span></>}
          </p>
          {state.text ? (
            <pre className="mt-1 overflow-x-auto rounded bg-surface2 p-2 font-mono text-[10px] leading-[1.5] text-muted">
              {before.map((ln, i) => (
                <div key={i}><span className="mr-2 select-none opacity-50">{firstLine + i}</span>{ln}</div>
              ))}
              <div className="text-fg"><span className="mr-2 select-none text-warning">{state.line}</span>{state.text}</div>
            </pre>
          ) : state.missing ? (
            <p className="mt-1 text-muted">{state.missing}</p>
          ) : null}
          {state.vars?.length > 0 && (
            <ul className="mt-1 space-y-0.5">
              {state.vars.map((v) => (
                <VarNode key={v.name} node={{ ...v, expr: v.expr || v.name }}
                  onExpand={onExpand} onWatch={onWatch} />
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  )
}

// GdbRecipe hands back the command line — the same session, in the operator's own terminal.
//
// This page is a better way to read a core than gdb's prompt right up to the moment it is not: a
// command it does not offer, a colleague who wants the session on their own screen, a ticket that
// has to record what was actually run. The recipe in every wiki is
//
//   gdb -ex "set solib-search-path <libs>" -ex "set sysroot <libs>" mysqld <core>
//
// and the gap between that and a session that resolves anything is exactly what DBCanvas worked
// out when it opened this core: which node, which binary, which core file, and the search path it
// found by walking the mounted library directory. So the line is built on the server
// (app/gdbcore.go) from those values rather than reconstructed here from a template — a recipe you
// retype from memory is a recipe that quietly reads a different set of libraries.
//
// Collapsed, because most of the time you are reading the page instead. Copy works either way:
// that is the button people actually came for.
export function GdbRecipe({ recipe }) {
  const [open, setOpen] = useState(false)
  const [copied, setCopied] = useState(false)
  if (!recipe) return null
  const copy = async () => {
    try { await navigator.clipboard.writeText(recipe) } catch { /* clipboard refused; the text is on screen */ }
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }
  return (
    <div className="rounded-lg border bg-surface px-3 py-2 text-[11px]">
      <div className="flex items-center gap-2">
        <button onClick={() => setOpen((v) => !v)} className="flex min-w-0 items-center gap-1.5 text-muted hover:text-fg">
          <Icon.Chevron size={13} className={`shrink-0 transition-transform ${open ? '' : '-rotate-90'}`} />
          <span className="font-medium text-fg">Run this in your own terminal</span>
          <span className="hidden truncate sm:inline">— the same gdb session, with this node's paths</span>
        </button>
        <button onClick={copy} title="Copy the command"
          className="ml-auto flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 text-muted transition hover:bg-surface2 hover:text-fg">
          {copied ? <Icon.Check size={13} /> : <Icon.Copy size={13} />}
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      {open && (
        <pre className="mt-2 overflow-x-auto whitespace-pre rounded bg-surface2 p-2 font-mono text-[10px] leading-[1.5] text-fg">{recipe}</pre>
      )}
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
export function Backtrace({
  frames, selected, onSelect, more, busy, state, onMore,
  mode = 'thread', onMode, stacks, locals, onLocals, frameVars, onThread,
}) {
  const empty = !frames || frames.length === 0
  const all = mode === 'all'
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try { await navigator.clipboard.writeText(threadStacksAsText(stacks || [])) } catch { /* on screen anyway */ }
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }
  return (
    <Panel id="stack"
      title={all ? `Every thread${stacks ? ` (${stacks.length})` : ''}` : state?.core ? `Stack — thread ${state.thread}` : 'Stack'}
      className="min-h-0 flex-1"
      action={(
        <div className="flex items-center gap-2">
          {onMode && (
            <div className="flex overflow-hidden rounded border text-[10px]">
              {[['thread', 'This thread'], ['all', 'All threads']].map(([m, label]) => (
                <button key={m} onClick={() => onMode(m)}
                  className={`px-1.5 py-0.5 transition ${mode === m
                    ? 'bg-primary/15 font-medium text-primary' : 'text-muted hover:bg-surface2 hover:text-fg'}`}>
                  {label}
                </button>
              ))}
            </div>
          )}
          {all ? (
            <button onClick={copy} title="Copy every stack as text, the way `thread apply all bt` prints it"
              className="flex items-center gap-1 rounded px-1 py-0.5 text-[10px] text-muted hover:bg-surface2 hover:text-fg">
              {copied ? <Icon.Check size={12} /> : <Icon.Copy size={12} />}{copied ? 'Copied' : 'Copy'}
            </button>
          ) : (
            <>
              {onLocals && (
                <button onClick={() => onLocals(!locals)}
                  title="Show every frame's arguments and locals, which is what `bt full` adds to `bt`"
                  className={`rounded border px-1.5 py-0.5 text-[10px] transition ${locals
                    ? 'border-primary/40 bg-primary/15 text-primary' : 'text-muted hover:bg-surface2 hover:text-fg'}`}>
                  full
                </button>
              )}
              <span className="text-[10px] text-muted">{empty ? ''
                : state?.verdict?.depth ? `${frames.length} of ${state.verdict.depth} frames`
                : `${frames.length} frame${frames.length === 1 ? '' : 's'}`}</span>
            </>
          )}
        </div>
      )}
      bodyClass="min-h-0 flex-1 overflow-auto p-0">
      {all ? (
        <AllThreads stacks={stacks} selected={state?.thread} onThread={onThread} />
      ) : empty ? (
        <p className="p-3 text-[11px] text-muted">
          Pick a core file on the left. Its threads and their stacks appear once gdb has read it.
        </p>
      ) : (
        <div className="font-mono text-[11px]">
          {frames.map((f, i) => {
            const sys = isSystemFrame(f)
            return (
              <div key={`${f.level}-${i}`}>
                <button onClick={() => onSelect(i)}
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
                {locals && (i < FULL_WINDOW
                  ? <FrameLocals vars={frameVars?.[String(f.level)]} />
                  : i === FULL_WINDOW
                    ? <p className="border-b px-3 py-1 pl-14 text-[10px] text-muted">
                        locals stop here — gdb reads them one frame at a time, so `full` covers the
                        first {FULL_WINDOW} frames. Select a frame to see all of its state.
                      </p>
                    : null)}
              </div>
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

// FrameLocals is the `full` of `bt full`, under the frame it belongs to.
//
// Values are clipped here rather than wrapped: this is the view you scroll to find which frame was
// holding the bad value, and one std::string printed in full is forty lines of allocator
// boilerplate between you and the next frame. The full text is in the row's title, and the panel
// on the right opens it properly.
export function FrameLocals({ vars }) {
  if (!vars) {
    return <p className="border-b px-3 py-1 pl-14 text-[10px] text-muted">reading this frame&apos;s locals…</p>
  }
  if (vars.length === 0) {
    return <p className="border-b px-3 py-1 pl-14 text-[10px] text-muted">no arguments or locals here</p>
  }
  return (
    <div className="space-y-0.5 border-b bg-surface2/40 px-3 py-1 pl-14 text-[10px]">
      {vars.map((v) => (
        <div key={v.name} className="flex gap-2">
          <span className="w-6 shrink-0 text-muted opacity-70">{v.arg ? 'arg' : 'var'}</span>
          <span className="shrink-0 text-fg">{v.name}</span>
          <span className="min-w-0 flex-1 truncate text-muted" title={v.value}>
            {valueSummary(v.value) ? `"${valueSummary(v.value)}"` : v.value}
          </span>
        </div>
      ))}
    </div>
  )
}

// AllThreads is `thread apply all bt`, folded.
//
// The command prints one stack per thread, which on a real core is sixty stacks and most of them
// are the same stack: every idle worker in a pool is parked in the same wait, printed over and
// over. So identical stacks are grouped into one entry with its threads listed next to it, the
// thread that took the signal comes first, and the biggest groups come next — because a group of
// forty threads all in the same place is itself a finding, and it is invisible in the wall of
// text the command produces.
export function AllThreads({ stacks, selected, onThread }) {
  const groups = useMemo(() => groupThreadStacks(stacks || []), [stacks])
  if (!stacks) {
    return (
      <p className="p-3 text-[11px] text-muted">
        Reading every thread&apos;s stack — one request per thread, so this takes a moment on a
        core with sixty of them.
      </p>
    )
  }
  if (stacks.length === 0) {
    return <p className="p-3 text-[11px] text-muted">Open a core file to see its threads.</p>
  }
  return (
    <div className="divide-y font-mono text-[11px]">
      {groups.map((g) => <ThreadGroup key={g.sig} group={g} selected={selected} onThread={onThread} />)}
    </div>
  )
}

export function ThreadGroup({ group, selected, onThread }) {
  // The signalled thread's stack is the one anybody opened this view to read, so it is the one
  // that starts open. The rest are a line each until asked for.
  const [open, setOpen] = useState(!!group.signal)
  const st = group.stack
  const frames = st.frames || []
  const top = frames.find((f) => !isSystemFrame(f)) || frames[0]
  return (
    <div className={group.signal ? 'bg-danger/5' : ''}>
      <button onClick={() => setOpen((v) => !v)}
        className="flex w-full items-baseline gap-2 px-3 py-1.5 text-left hover:bg-surface2/60">
        <Icon.Chevron size={12} className={`shrink-0 text-muted transition-transform ${open ? '' : '-rotate-90'}`} />
        <span className="shrink-0 tabular-nums text-muted">
          {group.threads.length === 1 ? `#${st.thread}` : `${group.threads.length}×`}
        </span>
        {group.signal && <Badge tone="danger">took the signal</Badge>}
        <span className="min-w-0 flex-1 truncate text-fg" title={top?.func}>{shortFunc(top?.func)}</span>
        <span className="shrink-0 text-[10px] text-muted">{st.depth || frames.length} frames</span>
      </button>
      {open && (
        <div className="bg-surface2/30 pb-1">
          <div className="flex flex-wrap gap-1 px-3 pb-1 pl-7">
            {group.threads.map((t) => (
              <button key={t.thread} onClick={() => onThread?.(t.thread)}
                title={`${t.target || ''}${t.name ? ` — ${t.name}` : ''} · read this thread's whole stack`}
                className={`rounded px-1.5 py-0.5 text-[10px] transition ${t.thread === selected
                  ? 'bg-primary/20 text-primary' : 'bg-surface text-muted hover:text-fg'}`}>
                #{t.thread}{t.name ? ` ${t.name}` : ''}
              </button>
            ))}
          </div>
          {frames.map((f, i) => (
            <div key={`${f.level}-${i}`} className="flex items-baseline gap-2 px-3 py-0.5 pl-7">
              <span className="w-8 shrink-0 tabular-nums text-muted">#{f.level}</span>
              <span className={`min-w-0 flex-1 truncate ${isSystemFrame(f) ? 'text-muted' : 'text-fg'}`}
                title={f.func}>
                {shortFunc(f.func)}
                {f.repeat > 1 && <span className="ml-2 rounded bg-warning/20 px-1 text-[10px] text-warning">×{f.repeat}</span>}
              </span>
              <span className="shrink-0 text-[10px] text-muted">{sourceOf(f)}</span>
            </div>
          ))}
          {st.more && (
            <p className="px-3 py-1 pl-7 text-[10px] text-muted">
              deeper frames not loaded — open this thread to read the rest of it
            </p>
          )}
          {st.error && <p className="px-3 py-1 pl-7 text-[10px] text-danger">{st.error}</p>}
        </div>
      )}
    </div>
  )
}

// cycleTotal is how many times this collapsed row's cycle runs in the whole stack, falling back to
// the count within the frames actually loaded when there is no analysis to ask.
function cycleTotal(state, frame) {
  const v = state?.verdict
  if (v?.repeats && v.cycle?.some((fn) => frame.func?.startsWith(fn))) return v.repeats
  return frame.repeat
}

// SourceView shows the code the selected frame is standing on — the WHOLE file, scrolled to the
// frame's line.
//
// This is the pane that answers "why", and it exists because a file and a line number are not an
// explanation on their own — `temptable/table.h:190` tells you nothing until you can see that the
// line is `return m_rows.size();` and the frame's `this` is 0x0. It needs the *-debugsource
// package, which installs the code to exactly the path the debug symbols record, so no path
// mapping is involved and a missing pane means a missing package.
//
// It used to show a window of twenty-nine lines around the frame, and scrolling stopped at the
// edges of it. That is the wrong shape for the question being asked: you scroll up to see what the
// function was handed, down to see what it does with it, and past both to the function above — and
// a pane that stops mid-file reads as broken rather than as bounded. The server sends the file
// (app/gdbcore.go), capped only by a size no source file reaches.
//
// Maximize it (the button in the header) to read it as a file rather than through a 16-line slot.
export function SourceView({ source, frame }) {
  const hasLoc = !!frame?.file && !!frame?.line
  // Reveal the line the frame is on. It is somewhere in the middle of a whole file now, so this
  // is the difference between a source pane and a source file: without it you get line 1 of
  // fts0que.cc and a scrollbar.
  const marker = useRef(null)
  useEffect(() => {
    const el = marker.current
    if (!el) return
    // scrollIntoView would be the obvious call and it is the wrong one: it scrolls *every*
    // scrollable ancestor, the window included, so clicking a frame quietly scrolled the page and
    // took the verdict — which is the thing worth reading — off the top of the screen. Only this
    // pane should move, so the scroll box is found and moved by hand.
    let box = el.parentElement
    while (box && !/auto|scroll/.test(getComputedStyle(box).overflowY)) box = box.parentElement
    if (!box) return
    box.scrollTop += el.getBoundingClientRect().top - box.getBoundingClientRect().top - box.clientHeight / 2
  }, [source?.file, source?.line, source?.from])
  const count = source?.lines?.length || 0
  return (
    <Panel id="source" title={hasLoc ? sourceOf(frame) : 'Source'} className="h-64 shrink-0"
      action={frame?.file && (
        // The path shown is where the file actually *is* on the node once it was found, because
        // that is the one somebody reading along in their own terminal can open. What the debug
        // information recorded — which is frequently not openable at all — stays in the tooltip.
        <span className="truncate font-mono text-[10px] text-muted" title={source?.path || frame.file}>
          {count > 0 && <span className="tabular-nums">{`${count.toLocaleString()} lines · `}</span>}
          {(source?.path || frame.file).replace(/^.*\/percona-server-[^/]*\//, '')}</span>
      )}
      bodyClass="min-h-0 flex-1 overflow-auto p-0">
      {!hasLoc ? (
        <p className="p-3 text-[11px] text-muted">
          This frame has no source location{frame?.from ? <> — it is in <span className="font-mono">{libraryOf(frame.from)}</span>,
            which has no debug information on this node</> : ' — it was compiled without debug information'}.
          Pick a frame that shows a <span className="font-mono">file:line</span>.
        </p>
      ) : source?.error ? (
        // The path in the debug information is the *compiler's*, and it is usually not a path on
        // this node — see gdbSourceCandidates in app/gdbcore.go. When the search comes back empty
        // the reason is the useful part, so it is shown as prose rather than as a failure.
        <div className="space-y-1 p-3 text-[11px] text-muted">
          <p>{source.error}</p>
          <p className="break-all font-mono text-[10px] opacity-70">{frame.file}</p>
        </div>
      ) : !source?.lines ? (
        <p className="p-3 text-[11px] text-muted">Reading {sourceOf(frame)}…</p>
      ) : (
        <div className="font-mono text-[11px] leading-[1.45]">
          {source.lines.map((ln, i) => {
            const n = source.from + i
            const here = n === source.line
            return (
              // content-visibility lets the browser skip layout and paint for the lines that are
              // off screen, which is what makes a whole file cheap: the rows are one line each and
              // exactly 16px tall, so the intrinsic size is right and the scrollbar and
              // scrollIntoView both land where they would without it. Measured on a 40,000-line
              // file: 414ms to lay out without it, 132ms with.
              <div key={n} ref={here ? marker : null}
                className={`flex w-max min-w-full [content-visibility:auto] [contain-intrinsic-size:auto_16px] ${here ? 'bg-warning/20' : ''}`}>
                <span className={`sticky left-0 z-10 w-12 shrink-0 select-none border-r px-2 text-right ${
                  here ? 'bg-warning/20 font-semibold text-warning' : 'bg-surface text-muted'}`}>{n}</span>
                <pre className="whitespace-pre px-2 text-fg">{ln || ' '}</pre>
              </div>
            )
          })}
          {source.truncated && (
            <p className="border-t px-3 py-2 text-[11px] text-muted">
              This file is larger than the pane will read and is cut off here. Everything above is
              the file as it is on the node.
            </p>
          )}
        </div>
      )}
    </Panel>
  )
}

// ---------------------------------------------------------------- right column

// FrameVars is the selected frame's state, and every value in it can be opened.
//
// A printed value is where the panel used to stop, and it is where the question usually starts.
// `thd = 0x7f1c000a2e00` is not an answer — the answer is what is *in* that THD, and gdb has
// always been able to say: a variable object walks a value one level at a time, which is how a
// pointer becomes the object it points at and a struct becomes its fields. Each row here carries
// the expression that reads it, so opening a member, watching it, or pasting it into the console
// are the same click.
//
// Values are printed raw because gdb's Python pretty-printers are off (auto-load is how a mounted
// directory becomes code execution), so a std::string arrives as its allocator internals. The
// readable part — the quoted text inside — is pulled out and shown first; the rest stays, because
// on a crash stack the internals are sometimes exactly what is wrong.
export function FrameVars({ vars, frame, onExpand, onWatch }) {
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
          {vars === null ? (
            <p className="text-muted">Reading this frame&apos;s arguments and locals…</p>
          ) : vars.length === 0 ? (
            <p className="text-muted">
              No arguments or locals here. That is normal for a frame in a library, and for every
              frame when the executable has no separate debug symbols installed.
            </p>
          ) : null}
          <VarGroup label="Arguments" vars={args} onExpand={onExpand} onWatch={onWatch} />
          <VarGroup label="Locals" vars={locals} onExpand={onExpand} onWatch={onWatch} />
        </div>
      )}
    </Panel>
  )
}

export function VarGroup({ label, vars, onExpand, onWatch }) {
  if (!vars || vars.length === 0) return null
  return (
    <div>
      <p className="mb-1 text-[10px] uppercase tracking-wide text-muted">{label}</p>
      <ul className="space-y-0.5">
        {vars.map((v) => (
          <VarNode key={v.name} node={{ ...v, expr: v.expr || v.name }}
            onExpand={onExpand} onWatch={onWatch} />
        ))}
      </ul>
    </div>
  )
}

// VarNode is one value, and — when there is something inside it — the way in.
//
// Children are fetched on the first open and kept, because a core file is a dead process: what is
// in that struct cannot change while you are reading it, so re-reading on every toggle would be
// round trips for nothing.
export function VarNode({ node, depth = 0, onExpand, onWatch }) {
  const [open, setOpen] = useState(false)
  const [kids, setKids] = useState(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const openable = !!node.expr && !!onExpand && (node.children > 0 || canExpand(node))

  const toggle = () => {
    if (!openable) return
    if (open) { setOpen(false); return }
    setOpen(true)
    if (kids || loading) return
    setLoading(true)
    setError('')
    Promise.resolve(onExpand(node.expr))
      .then((list) => setKids(list || []))
      // gdb's own message is the useful one here: "Cannot access memory at address 0x0" on a
      // crash stack is not a failed request, it is the diagnosis.
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false))
  }

  const text = valueSummary(node.value)
  return (
    <li>
      <div className="group flex items-start gap-1" style={{ paddingLeft: depth * 12 }}>
        <button onClick={toggle} disabled={!openable} aria-label={openable ? 'Open this value' : undefined}
          className={`mt-[3px] shrink-0 ${openable ? 'text-muted hover:text-fg' : 'cursor-default opacity-0'}`}>
          <Icon.Chevron size={11} className={`transition-transform ${open ? '' : '-rotate-90'}`} />
        </button>
        <button onClick={toggle} title={node.type || node.expr}
          className={`shrink-0 font-mono ${openable ? 'text-fg hover:text-primary' : 'cursor-default text-fg'}`}>
          {node.name}
        </button>
        <span className="min-w-0 flex-1 break-all font-mono text-muted" title={node.value}>
          {text && <span className="text-fg">&quot;{text}&quot;</span>}
          {text ? <span className="opacity-60"> {clipValue(node.value)}</span> : clipValue(node.value)}
        </span>
        {onWatch && node.expr && (
          <button onClick={() => onWatch(node.expr)} title="Watch this expression"
            className="shrink-0 text-muted opacity-0 transition group-hover:opacity-100 hover:text-primary">
            <Icon.Plus size={11} />
          </button>
        )}
      </div>
      {open && (
        <ul className="space-y-0.5">
          {loading && (
            // Not a spinner's worth of waiting: opening a value makes gdb read the whole type out
            // of a gigabyte of debug information, and the first one in a session is routinely
            // three quarters of a minute. Saying so is the difference between waiting and
            // clicking again.
            <li className="py-0.5 text-[10px] text-muted" style={{ paddingLeft: (depth + 1) * 12 }}>
              reading… gdb expands the whole type the first time it is asked, which is slow on a
              server binary
            </li>
          )}
          {error && <li className="py-0.5 text-[10px] text-danger" style={{ paddingLeft: (depth + 1) * 12 }}>{error}</li>}
          {kids && kids.length === 0 && !error && (
            <li className="py-0.5 text-[10px] text-muted" style={{ paddingLeft: (depth + 1) * 12 }}>
              nothing inside this one
            </li>
          )}
          {(kids || []).map((k, i) => (
            <VarNode key={`${k.name}-${i}`} node={k} depth={depth + 1} onExpand={onExpand} onWatch={onWatch} />
          ))}
        </ul>
      )}
    </li>
  )
}

// clipValue keeps one row one row. The whole value is in the title and one click away in the
// children; a std::string's 300 characters of allocator internals pushing the next variable off
// the panel is not a reason to scroll.
const VALUE_CLIP = 140
export function clipValue(v) {
  const value = v || ''
  return value.length > VALUE_CLIP ? `${value.slice(0, VALUE_CLIP)}…` : value
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
