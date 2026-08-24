import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Button, Badge, Card, inputCls } from '../components/ui.jsx'
import {
  debugApi, debugFrameApi, openDebugSession,
  STATUS_TONE, STATUS_TEXT, goHighlight, TOKEN_CLS, shortFrameName,
} from '../lib/debugApi.js'

// Operator Debugger — step through a Kubernetes operator from inside DBCanvas.
//
// A K3D frame can be deployed with its operator running under Delve (see k3ddebug.go).
// Until now that gave you a port to point an IDE at, which meant a clone of the
// operator at the right tag, a Go toolchain, gopls pointed at GOOS=linux, and a
// launch.json with the substitutePath that makes source line up. This page is the
// other option: DBCanvas already knows the tag, has the source on the k3s node, and
// can reach the debugger over the stack network, so it can be the debugger client
// itself. Nothing to install, and one button — Force a reconcile — that no IDE has.
//
// The layout is the one every debugger converges on, because the questions are
// ordered: where can I stop (left), what is the code doing (centre), what is it
// holding (right). The event log lives under the right column because in this
// particular debugger the interesting events are not all yours — the operator is
// managing a real cluster while you read.
//
// The banner is not decoration. Sitting at a breakpoint stops the operator, and a
// stopped operator reconciles nothing at all — no probe fails, nothing is logged, the
// cluster just quietly stops being managed. The session resumes it for you after a
// few idle minutes; the banner is what makes that predictable rather than surprising.

const IDLE_CHOICES = [
  { value: 120, label: '2 minutes' },
  { value: 300, label: '5 minutes' },
  { value: 900, label: '15 minutes' },
  { value: 0, label: 'never (careful)' },
]

// targetKey identifies a frame across stacks — the same shape the picker's value uses.
const targetKey = (t) => (t ? `${t.stackId}/${t.frameId}` : '')

// Every panel can be maximized and docked back again.
//
// Three columns is the right default and the wrong one the moment you actually need something:
// a 60-frame call stack, a struct four levels deep, a file whose lines run past the middle
// column. Maximizing covers the workspace with the one panel you are reading; docking back puts
// it where it was.
//
// It is done by *covering* rather than by re-laying-out: the maximized panel becomes
// `absolute inset-0` over the grid, and its siblings stay mounted underneath. That is not a
// shortcut — a panel that unmounts loses what you expanded in it, and coming back from full
// screen to a collapsed variables tree would defeat the point.
//
// PanelMaximize is a context so the panels (which are also rendered on their own by the smoke
// suite) do not each need the state threaded through them. Without a provider there is simply
// no maximize button.
const MaximizeContext = createContext(null)

export function PanelMaximize({ value, onChange, children }) {
  const ctx = useMemo(() => ({
    maxId: value,
    toggle: (id) => onChange(value === id ? null : id),
  }), [value, onChange])
  return <MaximizeContext.Provider value={ctx}>{children}</MaximizeContext.Provider>
}

function useMaximize(id) {
  const ctx = useContext(MaximizeContext)
  return {
    maxed: !!id && ctx?.maxId === id,
    toggle: ctx && id ? () => ctx.toggle(id) : null,
  }
}

// The class that lifts a panel over the workspace. Its sizing classes are dropped with it —
// a maximized panel that kept `max-h-52` would be a tall panel in an empty page.
const MAXED_CLS = 'absolute inset-0 z-30 shadow-2xl'

function MaxButton({ maxed, onClick }) {
  const Ico = maxed ? Icon.Minimize : Icon.Maximize
  return (
    <button onClick={onClick} title={maxed ? 'Dock back (Esc)' : 'Maximize'}
      aria-label={maxed ? 'Dock back' : 'Maximize'}
      className="shrink-0 rounded p-1 text-muted transition hover:bg-surface2 hover:text-fg">
      <Ico size={14} />
    </button>
  )
}

// Panel is Card's look with a body that can be told to scroll. Card's body is a plain
// padded div, so a list inside it grows the page instead of scrolling in place — which in
// a three-column debugger means the toolbar you need scrolls off the top the moment the
// operator stops in a long file.
function Panel({ id, title, action, className = '', bodyClass = 'p-3', children }) {
  const { maxed, toggle } = useMaximize(id)
  return (
    <div className={`flex min-h-0 flex-col rounded-xl border bg-surface ${maxed ? MAXED_CLS : className}`}>
      {(title || action || toggle) && (
        <div className="flex shrink-0 items-center justify-between gap-2 border-b px-3 py-2">
          <h3 className="min-w-0 truncate text-sm font-semibold text-fg">{title}</h3>
          <div className="flex shrink-0 items-center gap-2">
            {action}
            {toggle && <MaxButton maxed={maxed} onClick={toggle} />}
          </div>
        </div>
      )}
      <div className={`min-h-0 flex-1 ${bodyClass}`}>{children}</div>
    </div>
  )
}

export default function OperatorDebugger() {
  const [targets, setTargets] = useState(null)
  const [key, setKey] = useState('')
  const [err, setErr] = useState('')

  const [state, setState] = useState(null)
  const [log, setLog] = useState([])
  const [files, setFiles] = useState([])
  const [file, setFile] = useState('')        // the file open in the centre pane
  const [source, setSource] = useState(null)  // { path, content }
  const [frameID, setFrameID] = useState(0)   // the selected stack frame
  const [scopes, setScopes] = useState([])
  const [filter, setFilter] = useState('')
  const [watch, setWatch] = useState('')
  const [watches, setWatches] = useState([])  // [{expr, value, type, error}]
  const [busy, setBusy] = useState('')
  const [maxPanel, setMaxPanel] = useState(null) // the panel covering the workspace, if any

  const sessionRef = useRef(null)
  const target = useMemo(
    () => (targets || []).find((t) => targetKey(t) === key) || null, [targets, key])
  const api = useMemo(
    () => (target ? debugFrameApi(target.stackId, target.frameId) : null), [target])

  // ---- targets ---------------------------------------------------------------

  useEffect(() => {
    debugApi.targets()
      .then((list) => {
        setTargets(list)
        // A node panel's "Open debugger" button leaves the frame it wants here,
        // because a hash route carries no parameters.
        let want = ''
        try { want = sessionStorage.getItem('dbcanvas.debugTarget') || '' } catch { /* private mode */ }
        try { sessionStorage.removeItem('dbcanvas.debugTarget') } catch { /* ignore */ }
        const found = list.find((t) => targetKey(t) === want)
        setKey(targetKey(found || list[0]))
      })
      .catch((e) => { setErr(e.message); setTargets([]) })
  }, [])

  // ---- the session -----------------------------------------------------------

  // The session socket, with a reconnect.
  //
  // Reconnecting is not defensive programming for its own sake: the app joins the stack network
  // to reach the debugger, and connecting a running container to a Docker network reprograms its
  // NAT rules, which resets connections already open through the published port — this one
  // included. A frame deployed before that join moved into the deploy (k3ddebug.go) drops its
  // first socket for exactly that reason. The session itself is server-side and survives, so a
  // reconnect picks up the same breakpoints and the same stop.
  useEffect(() => {
    let live = true
    let timer = null

    const connect = () => {
      if (!live) return
      const s = openDebugSession(target.stackId, target.frameId, {
        state: (st) => setState(st),
        log: (line) => setLog((prev) => [...prev.slice(-400), line]),
        close: () => {
          if (!live) return
          setState((prev) => (prev ? { ...prev, status: 'detached' } : prev))
          timer = setTimeout(connect, 1500)
        },
      })
      sessionRef.current = s
    }

    sessionRef.current?.close()
    sessionRef.current = null
    setState(null); setLog([]); setScopes([]); setFrameID(0); setWatches([])
    if (!target) return undefined
    connect()

    return () => {
      live = false
      if (timer) clearTimeout(timer)
      sessionRef.current?.close()
      sessionRef.current = null
    }
  }, [target?.stackId, target?.frameId]) // eslint-disable-line react-hooks/exhaustive-deps

  // The file list and the first file to show.
  useEffect(() => {
    if (!api || !target) return
    setFiles([]); setSource(null); setFile('')
    api.sources()
      .then((r) => {
        setFiles(r.files || [])
        const start = target.startFile || (r.files || [])[0] || ''
        if (start) setFile(start)
      })
      .catch((e) => setErr(`Could not list the operator's source: ${e.message}`))
  }, [api, target?.startFile]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (!api || !file) return
    let live = true
    api.source(file)
      .then((r) => { if (live) setSource(r) })
      .catch((e) => { if (live) setErr(`Could not read ${file}: ${e.message}`) })
    return () => { live = false }
  }, [api, file])

  // ---- following the operator ------------------------------------------------

  const frames = state?.frames || []
  const stopped = state?.status === 'stopped'

  // When it stops, follow it: select the top frame that has source, open that file.
  useEffect(() => {
    if (!stopped || frames.length === 0) return
    const top = frames.find((f) => f.hasSource) || frames[0]
    setFrameID(top.id)
    if (top.hasSource && top.file) setFile(top.file)
  }, [stopped, frames.length, frames[0]?.id]) // eslint-disable-line react-hooks/exhaustive-deps

  // Whatever frame is selected, show its variables.
  useEffect(() => {
    const s = sessionRef.current
    if (!s || !stopped || !frameID) { setScopes([]); return }
    let live = true
    s.call({ cmd: 'scopes', frameId: frameID })
      .then((r) => { if (live) setScopes(r.scopes || []) })
      .catch(() => { if (live) setScopes([]) })
    return () => { live = false }
  }, [frameID, stopped])

  // Watches are re-evaluated on every stop, in the frame you are looking at — a value
  // from the previous stop is worse than no value.
  useEffect(() => {
    const s = sessionRef.current
    if (!s || !stopped || !frameID || watches.length === 0) return
    let live = true
    Promise.all(watches.map((wv) => s.call({ cmd: 'evaluate', expr: wv.expr, frameId: frameID })
      .then((r) => ({ expr: wv.expr, ...r.result }))
      .catch((e) => ({ expr: wv.expr, error: e.message }))))
      .then((next) => { if (live) setWatches(next) })
    return () => { live = false }
  }, [frameID, stopped, state?.reason]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (!maxPanel) return undefined
    const onKey = (e) => { if (e.key === 'Escape') setMaxPanel(null) }
    addEventListener('keydown', onKey)
    return () => removeEventListener('keydown', onKey)
  }, [maxPanel])

  // ---- commands --------------------------------------------------------------

  const send = useCallback((cmd) => sessionRef.current?.send(cmd), [])
  const call = useCallback(async (cmd, label) => {
    const s = sessionRef.current
    if (!s) return
    setBusy(label || cmd.cmd); setErr('')
    try { return await s.call(cmd) } catch (e) { setErr(e.message) } finally { setBusy('') }
  }, [])

  const toggleBreakpoint = useCallback((line) => {
    if (!file) return
    const on = !(state?.breakpoints || []).some((b) => b.file === file && b.line === line)
    send({ cmd: 'breakpoint', file, line, on })
  }, [file, state?.breakpoints, send])

  const addWatch = () => {
    const expr = watch.trim()
    const s = sessionRef.current
    if (!expr || !s) return
    setWatch('')
    setWatches((prev) => [...prev.filter((wv) => wv.expr !== expr), { expr }])
    const settle = (v) => setWatches((prev) => prev.map((wv) => (wv.expr === expr ? v : wv)))
    s.call({ cmd: 'evaluate', expr, frameId: frameID })
      .then((r) => settle({ expr, ...(r?.result || {}) }))
      .catch((e) => settle({ expr, error: e.message }))
  }

  // ---- render ----------------------------------------------------------------

  if (targets === null) return <div className="p-6 text-sm text-muted">Loading…</div>

  return (
    <div className="flex flex-col gap-3">
      <Header
        targets={targets} value={key} onChange={setKey}
        state={state} busy={busy}
        onAttach={() => call({ cmd: 'attach' })}
        onDetach={() => call({ cmd: 'detach' })}
        onReconcile={() => call({ cmd: 'reconcile' })}
      />

      {err && (
        <div className="flex items-start justify-between gap-3 rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">
          <span>{err}</span>
          <button onClick={() => setErr('')} className="shrink-0 opacity-70 hover:opacity-100"><Icon.Close size={14} /></button>
        </div>
      )}

      {targets.length === 0 ? <NoTargets /> : (
        <>
          <SessionBanner state={state} target={target} />
          <PanelMaximize value={maxPanel} onChange={setMaxPanel}>
          <div className="relative grid h-[calc(100vh-15rem)] min-h-[560px] grid-cols-12 gap-3 overflow-hidden">
            <div className="col-span-3 flex min-h-0 flex-col gap-3 overflow-y-auto">
              <QuickBreakpoints target={target} state={state} onToggle={(name, on) =>
                send({ cmd: 'fnbreakpoint', name, on })} />
              <BreakpointList state={state} onOpen={setFile}
                onRemove={(b) => send({ cmd: 'breakpoint', file: b.file, line: b.line, on: false })}
                onRemoveFn={(f) => send({ cmd: 'fnbreakpoint', name: f.name, on: false })}
                onClear={() => send({ cmd: 'clearBreakpoints' })} />
              <FileTree files={files} value={file} filter={filter} onFilter={setFilter} onOpen={setFile} />
            </div>

            <div className="col-span-6 flex min-h-0 flex-col">
              <SourceView
                path={file} source={source} state={state}
                stoppedLine={stopped ? (frames.find((f) => f.id === frameID)?.line || 0) : 0}
                stoppedFile={stopped ? (frames.find((f) => f.id === frameID)?.file || '') : ''}
                onToggle={toggleBreakpoint}
                onStep={(cmd) => call({ cmd })}
                busy={busy}
              />
            </div>

            <div className="col-span-3 flex min-h-0 flex-col gap-3 overflow-y-auto">
              <CallStack frames={frames} selected={frameID} onSelect={(f) => {
                setFrameID(f.id)
                if (f.hasSource && f.file) setFile(f.file)
              }} />
              <Variables
                scopes={scopes} stopped={stopped}
                onExpand={(ref) => sessionRef.current?.call({ cmd: 'variables', ref })
                  .then((r) => r.variables || [])}
                onSet={(ref, name, value) => sessionRef.current
                  .call({ cmd: 'setVariable', ref, name, value })
                  .then((r) => r.result)}
                onFull={(expr) => sessionRef.current
                  .call({ cmd: 'evaluate', expr, frameId: frameID })
                  .then((r) => r.result?.value ?? '')}
              />
              <WatchBox
                value={watch} onChange={setWatch} onAdd={addWatch}
                watches={watches} onRemove={(expr) => setWatches((p) => p.filter((wv) => wv.expr !== expr))}
                allowCalls={!!state?.allowCalls}
                onAllowCalls={(on) => send({ cmd: 'allowCalls', on })}
                idle={state?.idleSeconds ?? 300}
                onIdle={(seconds) => send({ cmd: 'idle', seconds })}
              />
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

export function Header({ targets, value, onChange, state, busy, onAttach, onDetach, onReconcile }) {
  const status = state?.status || 'detached'
  const attached = status !== 'detached'
  return (
    <div className="flex flex-wrap items-center gap-3">
      <div>
        <h1 className="text-lg font-semibold text-fg">Operator Debugger</h1>
        <p className="text-xs text-muted">
          Step through the Kubernetes operator itself — no IDE, no clone, no Go toolchain.
        </p>
      </div>
      <div className="ml-auto flex flex-wrap items-center gap-2">
        <select className={`${inputCls} w-72`} value={value} onChange={(e) => onChange(e.target.value)}>
          {(targets || []).map((t) => (
            <option key={`${t.stackId}/${t.frameId}`} value={`${t.stackId}/${t.frameId}`}>
              {t.stackName} · {t.label} · {t.operator?.toUpperCase()} {t.operatorVer}
            </option>
          ))}
        </select>
        <Badge tone={STATUS_TONE[status] || 'muted'}>{STATUS_TEXT[status] || status}</Badge>
        {attached ? (
          <Button variant="outline" size="sm" disabled={busy === 'detach'} onClick={onDetach}>
            <Icon.Close size={14} /> Detach
          </Button>
        ) : (
          <Button size="sm" disabled={busy === 'attach'} onClick={onAttach}>
            <Icon.Link size={14} /> Attach
          </Button>
        )}
        <Button variant="outline" size="sm" disabled={!attached || busy === 'reconcile'} onClick={onReconcile}>
          <Icon.Both size={14} /> Force a reconcile
        </Button>
      </div>
    </div>
  )
}

export function NoTargets() {
  return (
    <Card title="Nothing to debug yet">
      <div className="space-y-2 text-sm text-muted">
        <p>
          A Kubernetes frame has to be deployed with its operator under a debugger. In the
          Database Stacks designer, select a <span className="font-medium text-fg">K3D cluster</span>{' '}
          frame running the <span className="font-medium text-fg">Percona Operator for MySQL (PXC)</span>,
          tick <span className="font-medium text-fg">Run the operator under Delve</span>, and deploy.
        </p>
        <p>
          It is a deploy-time decision twice over: the operator is rebuilt from that release's own
          source with the optimiser off, and the debugger's port can only be published while the
          cluster is being created. The cluster still comes up normally either way — Delve starts
          with <span className="font-mono">--continue</span>, so nothing waits for you to attach.
        </p>
      </div>
    </Card>
  )
}

// SessionBanner says what is true right now, and the one consequence that is easy to forget.
export function SessionBanner({ state, target }) {
  const status = state?.status || 'detached'
  if (status === 'stopped') {
    const idle = state?.idleSeconds
    return (
      <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-snug text-muted">
        <span className="font-medium text-fg">The operator is stopped.</span> While it sits here it
        reconciles nothing — no probe fails and nothing is logged, so the cluster is simply not being
        managed until you continue.{' '}
        {idle ? `If nobody touches this for ${Math.round(idle / 60)} minutes it will be resumed for you.`
          : 'The idle guard is off, so it will stay stopped until you resume it.'}
      </div>
    )
  }
  if (status === 'detached') {
    return (
      <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-snug text-muted">
        Not attached. {state?.detail ? <span className="text-danger">{state.detail}</span> : 'Press Attach to connect to the operator’s debugger.'}
      </div>
    )
  }
  return (
    <div className="rounded-lg border border-accent/30 bg-accent/10 px-3 py-2 text-[11px] leading-snug text-muted">
      <span className="font-medium text-fg">Attached — the operator is running.</span> Set a
      breakpoint (click a line number, or use a quick breakpoint), then press{' '}
      <span className="font-medium text-fg">Force a reconcile</span>: that annotates{' '}
      <span className="font-mono">{target?.cr || 'the cluster'}</span>, and the operator comes
      straight through <span className="font-mono">Reconcile</span>.
    </div>
  )
}

// ---------------------------------------------------------------- left column

export function QuickBreakpoints({ target, state, onToggle }) {
  const presets = target?.presets || []
  const set = new Set((state?.functions || []).map((f) => f.name))
  const byName = Object.fromEntries((state?.functions || []).map((f) => [f.name, f]))
  if (presets.length === 0) return null
  return (
    <Panel id="quick" title="Quick breakpoints" className="shrink-0">
      <div className="space-y-1">
        {presets.map((p) => {
          const on = set.has(p.func)
          const fn = byName[p.func]
          return (
            <button key={p.func} onClick={() => onToggle(p.func, !on)}
              className={`flex w-full items-start gap-2 rounded-lg px-2 py-1.5 text-left text-xs transition ${
                on ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2 hover:text-fg'}`}>
              <span className={`mt-1 h-2 w-2 shrink-0 rounded-full ${
                on ? (fn && fn.verified === false ? 'bg-danger' : 'bg-primary') : 'bg-surface2 ring-1 ring-muted/40'}`} />
              <span className="min-w-0">
                <span className="block font-medium">{p.label}</span>
                <span className="block truncate">{on && fn?.message ? fn.message : p.hint}</span>
              </span>
            </button>
          )
        })}
      </div>
    </Panel>
  )
}

export function BreakpointList({ state, onOpen, onRemove, onRemoveFn, onClear }) {
  const bps = state?.breakpoints || []
  // A quick breakpoint is a breakpoint. Counting only the source ones makes the panel say
  // "Breakpoints (0)" while the operator is sitting at one, which reads as a broken debugger.
  const fns = state?.functions || []
  const total = bps.length + fns.length
  return (
    <Panel id="breakpoints" title={`Breakpoints (${total})`} className="max-h-56 shrink-0" bodyClass="overflow-auto p-3"
      action={total > 0 && (
        <button onClick={onClear} className="text-xs text-muted hover:text-fg">Clear all</button>
      )}>
      {total === 0 ? (
        <p className="text-xs text-muted">Click a line number in the source to set one.</p>
      ) : (
        <div className="space-y-1">
          {fns.map((f) => (
            <div key={f.name} className="flex items-center gap-2 text-xs">
              <span className={`h-2 w-2 shrink-0 rounded-full ${f.verified ? 'bg-danger' : 'bg-muted'}`}
                title={f.verified ? 'armed' : (f.message || 'not resolved')} />
              <button className="min-w-0 flex-1 truncate text-left font-mono text-muted hover:text-fg"
                onClick={() => f.file && onOpen(f.file)}
                title={`${f.name}${f.file ? ` — ${f.file}:${f.line}` : ''}`}>
                {shortFrameName(f.name)}{f.line ? `:${f.line}` : ''}
              </button>
              <button onClick={() => onRemoveFn(f)} className="shrink-0 text-muted hover:text-danger">
                <Icon.Close size={12} />
              </button>
            </div>
          ))}
          {bps.map((b) => (
            <div key={`${b.file}:${b.line}`} className="flex items-center gap-2 text-xs">
              <span className={`h-2 w-2 shrink-0 rounded-full ${b.verified ? 'bg-danger' : 'bg-muted'}`}
                title={b.verified ? 'armed' : (b.message || 'not resolved')} />
              <button className="min-w-0 flex-1 truncate text-left font-mono text-muted hover:text-fg"
                onClick={() => onOpen(b.file)} title={`${b.file}:${b.line}${b.message ? ` — ${b.message}` : ''}`}>
                {b.file.split('/').pop()}:{b.line}
              </button>
              <button onClick={() => onRemove(b)} className="shrink-0 text-muted hover:text-danger">
                <Icon.Close size={12} />
              </button>
            </div>
          ))}
        </div>
      )}
    </Panel>
  )
}

export function FileTree({ files, value, filter, onFilter, onOpen }) {
  const shown = useMemo(() => {
    const q = (filter || '').toLowerCase().trim()
    const list = q ? files.filter((f) => f.toLowerCase().includes(q)) : files
    return list.slice(0, 400)
  }, [files, filter])
  return (
    <Panel id="files" title="Operator source" className="flex-1" bodyClass="flex min-h-0 flex-1 flex-col p-3">
      <input className={`${inputCls} mb-2 shrink-0`} placeholder="Filter files…"
        value={filter} onChange={(e) => onFilter(e.target.value)} />
      <div className="min-h-0 flex-1 space-y-0.5 overflow-auto">
        {shown.map((f) => (
          <button key={f} onClick={() => onOpen(f)}
            title={f}
            className={`block w-full truncate rounded px-2 py-1 text-left font-mono text-[11px] ${
              f === value ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2 hover:text-fg'}`}>
            {f}
          </button>
        ))}
        {files.length > shown.length && (
          <p className="px-2 py-1 text-[11px] text-muted">…{files.length - shown.length} more — narrow the filter.</p>
        )}
      </div>
    </Panel>
  )
}

// ---------------------------------------------------------------- source

export function SourceView({ path, source, state, stoppedLine, stoppedFile, onToggle, onStep, busy }) {
  const lines = useMemo(() => goHighlight(source?.content || ''), [source?.content])
  const bpLines = useMemo(() => {
    const s = new Set()
    for (const b of state?.breakpoints || []) if (b.file === path) s.add(b.line)
    return s
  }, [state?.breakpoints, path])
  const here = stoppedFile === path ? stoppedLine : 0
  const scroller = useRef(null)
  const marker = useRef(null)

  // Scroll to the stopped line — including when the file itself has only just arrived, which
  // is the common case: the stop names a file, the panel fetches it, and the line to reveal
  // does not exist in the DOM until that lands.
  useEffect(() => {
    if (here && marker.current) marker.current.scrollIntoView({ block: 'center', behavior: 'smooth' })
  }, [here, path, source?.content])

  const stopped = state?.status === 'stopped'
  const attached = state && state.status !== 'detached'
  const { maxed, toggle } = useMaximize('source')

  return (
    <div className={`flex min-h-0 flex-1 flex-col rounded-xl border bg-surface ${maxed ? MAXED_CLS : ''}`}>
      <div className="flex flex-wrap items-center gap-2 border-b px-3 py-2">
        <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg" title={source?.buildPath || path}>
          {path || 'no file open'}
        </span>
        <div className="flex items-center gap-1">
          <StepButton label="Continue" icon="Play" disabled={!stopped || !!busy} onClick={() => onStep('continue')} />
          <StepButton label="Pause" icon="Pause" disabled={!attached || stopped || !!busy} onClick={() => onStep('pause')} />
          <StepButton label="Step over" icon="StepOver" disabled={!stopped || !!busy} onClick={() => onStep('next')} />
          <StepButton label="Step into" icon="StepInto" disabled={!stopped || !!busy} onClick={() => onStep('stepIn')} />
          <StepButton label="Step out" icon="StepOut" disabled={!stopped || !!busy} onClick={() => onStep('stepOut')} />
          {toggle && <MaxButton maxed={maxed} onClick={toggle} />}
        </div>
      </div>
      <div ref={scroller} className="min-h-0 flex-1 overflow-auto font-mono text-[11px] leading-[1.45]">
        {!source && <p className="p-3 text-muted">Pick a file, or set a quick breakpoint and force a reconcile.</p>}
        {source && lines.map((tokens, i) => {
          const n = i + 1
          const isHere = n === here
          const hasBp = bpLines.has(n)
          return (
            <div key={n} ref={isHere ? marker : null}
              className={`flex w-max min-w-full ${isHere ? 'bg-warning/20' : 'hover:bg-surface2/60'}`}>
              <button onClick={() => onToggle(n)}
                title={hasBp ? 'Remove this breakpoint' : 'Break here'}
                className={`sticky left-0 z-10 w-14 shrink-0 select-none border-r px-2 text-right ${
                  hasBp ? 'bg-danger/15 text-danger' : 'bg-surface text-muted hover:text-fg'}`}>
                {hasBp ? '● ' : ''}{n}
              </button>
              <pre className="whitespace-pre px-2 text-fg">
                {tokens.map(([cls, text], j) => (
                  <span key={j} className={TOKEN_CLS[cls] || ''}>{text}</span>
                ))}
              </pre>
            </div>
          )
        })}
      </div>
    </div>
  )
}

function StepButton({ label, icon, disabled, onClick }) {
  const Ico = Icon[icon] || Icon.Play
  return (
    <button title={label} disabled={disabled} onClick={onClick}
      className="rounded-lg px-2 py-1 text-xs text-muted transition hover:bg-surface2 hover:text-fg disabled:opacity-40 disabled:hover:bg-transparent">
      <span className="flex items-center gap-1"><Ico size={14} />{label}</span>
    </button>
  )
}

// ---------------------------------------------------------------- right column

export function CallStack({ frames, selected, onSelect }) {
  return (
    <Panel id="stack" title="Call stack" className="max-h-52 shrink-0" bodyClass="overflow-auto p-3">
      {frames.length === 0 ? (
        <p className="text-xs text-muted">The operator is running. It appears here when it stops.</p>
      ) : (
        <div className="space-y-0.5">
          {frames.map((f) => (
            <button key={f.id} onClick={() => onSelect(f)}
              className={`block w-full rounded px-2 py-1 text-left text-[11px] ${
                f.id === selected ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2 hover:text-fg'}`}>
              <span className="block truncate font-medium">{shortFrameName(f.name)}</span>
              <span className={`block truncate font-mono ${f.hasSource ? '' : 'opacity-60'}`}>
                {f.hasSource ? `${f.file}:${f.line}` : `${shortFrameName(f.file)}:${f.line} · no source`}
              </span>
            </button>
          ))}
        </div>
      )}
    </Panel>
  )
}

// Variables renders the selected frame's scopes, and lets you change what is in them.
//
// Three things it has to get right, all of them learned from the real thing:
//
//  1. **Nothing is cut off.** Delve summarises a value in the `variables` response and in an
//     ordinary `evaluate` — the receiver of `Reconcile` is 259 characters that way and 4,209 in
//     full — and a debugger that shows you the first line of a struct and an ellipsis has told
//     you nothing. Values wrap rather than truncate, and any value Delve summarised can be
//     loaded whole (`Show all`, which re-reads it with the full evaluation context).
//  2. **Children are fetched when expanded**, which is the protocol's own model and the only
//     sane one: an operator's cluster object is a graph, and serialising all of it to look at
//     one field would cost seconds and megabytes.
//  3. **Values can be edited** — a variable is set through its *container* (`setVariable`), and
//     Delve can only do that for a variable it can name. Compiler-generated entries (`~r0` and
//     friends) have no evaluate name and cannot be set, so those rows offer no pencil rather
//     than offering one that always fails.
export function Variables({ scopes, stopped, onExpand, onSet, onFull }) {
  return (
    <Panel id="variables" title="Variables" className="min-h-[180px] flex-1" bodyClass="overflow-auto p-3">
      {!stopped ? (
        <p className="text-xs text-muted">Nothing to show while the operator is running.</p>
      ) : scopes.length === 0 ? (
        <p className="text-xs text-muted">No scopes for this frame.</p>
      ) : (
        <div className="space-y-2">
          {scopes.map((sc) => (
            <Scope key={sc.variablesReference} scope={sc} onExpand={onExpand} onSet={onSet} onFull={onFull} />
          ))}
        </div>
      )}
    </Panel>
  )
}

function Scope({ scope, onExpand, onSet, onFull }) {
  const [open, setOpen] = useState(!scope.expensive)
  const [vars, setVars] = useState(null)
  const [nonce, setNonce] = useState(0)
  useEffect(() => {
    if (!open) return undefined
    let live = true
    onExpand(scope.variablesReference).then((v) => { if (live) setVars(v) }).catch(() => { if (live) setVars([]) })
    return () => { live = false }
  }, [open, nonce]) // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <div>
      <button onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center gap-1 text-xs font-medium text-fg">
        <span className={`transition-transform ${open ? 'rotate-90' : ''}`}><Icon.Chevron size={12} /></span>
        {scope.name}
      </button>
      {open && (
        <div className="mt-1 space-y-1 pl-3">
          {vars === null && <p className="text-[11px] text-muted">Reading…</p>}
          {vars?.length === 0 && <p className="text-[11px] text-muted">empty</p>}
          {(vars || []).map((v, i) => (
            <VarNode key={`${v.name}-${i}`} v={v} depth={0} containerRef={scope.variablesReference}
              onExpand={onExpand} onSet={onSet} onFull={onFull} onChanged={() => setNonce((n) => n + 1)} />
          ))}
        </div>
      )}
    </div>
  )
}

// varIsSummarised reports whether Delve shortened this value. Its marker is an ellipsis, and it
// can land anywhere: at the end of a string, before a closing brace on a struct, inside the
// brackets of a slice (`[1,2,...]`). So the test is simply whether the value contains one.
//
// A Go string whose *contents* end in "..." would be a false positive. That costs a "show all"
// that returns the same text — which is the harmless way round, next to hiding the fact that
// four fifths of a value is missing.
export function varIsSummarised(value) {
  return String(value || '').includes('...')
}

export function VarNode({ v, depth, containerRef, onExpand, onSet, onFull, onChanged }) {
  const [open, setOpen] = useState(false)
  const [kids, setKids] = useState(null)
  const [nonce, setNonce] = useState(0)
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [full, setFull] = useState(null)
  const expandable = v.variablesReference > 0
  // Delve refuses to set anything it cannot name; the rows without one are the compiler's.
  const settable = !!v.evaluateName && !!onSet && containerRef > 0

  useEffect(() => {
    if (!open) return undefined
    let live = true
    onExpand(v.variablesReference).then((k) => { if (live) setKids(k) }).catch(() => { if (live) setKids([]) })
    return () => { live = false }
  }, [open, nonce]) // eslint-disable-line react-hooks/exhaustive-deps

  const shown = full ?? v.value
  const commit = async () => {
    if (busy) return
    setBusy(true); setErr('')
    try {
      await onSet(containerRef, v.name, draft)
      setEditing(false)
      setFull(null)
      onChanged?.()          // the container's other fields may have moved with it
      setNonce((n) => n + 1) // ...and this row's children are now stale
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div style={{ paddingLeft: depth * 8 }} className="group/var">
      <div className="flex items-start gap-1 text-[11px]">
        <button disabled={!expandable} onClick={() => setOpen((o) => !o)}
          title={expandable ? 'Expand' : undefined}
          className={`mt-0.5 shrink-0 transition-transform disabled:cursor-default ${expandable ? '' : 'opacity-0'} ${open ? 'rotate-90' : ''}`}>
          <Icon.Chevron size={10} />
        </button>
        <span className="shrink-0 font-mono text-fg">{v.name}</span>

        {editing ? (
          <span className="flex min-w-0 flex-1 flex-col gap-1">
            <span className="flex items-center gap-1">
              <input autoFocus className={`${inputCls} py-1 font-mono text-[11px]`} value={draft} disabled={busy}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') commit()
                  if (e.key === 'Escape') { setEditing(false); setErr('') }
                }} />
              <button onClick={commit} disabled={busy} title="Set (Enter)"
                className="shrink-0 rounded p-1 text-muted hover:bg-surface2 hover:text-fg"><Icon.Check size={12} /></button>
              <button onClick={() => { setEditing(false); setErr('') }} title="Cancel (Esc)"
                className="shrink-0 rounded p-1 text-muted hover:bg-surface2 hover:text-fg"><Icon.Close size={12} /></button>
            </span>
            {err && <span className="text-danger">{err}</span>}
          </span>
        ) : (
          <span className="min-w-0 flex-1">
            <span className="whitespace-pre-wrap break-all font-mono text-muted">{shown}</span>
            {varIsSummarised(shown) && v.evaluateName && onFull && (
              <button className="ml-1 whitespace-nowrap text-primary hover:underline"
                onClick={async () => {
                  try { setFull(await onFull(v.evaluateName)) } catch (e) { setErr(e.message) }
                }}>show all</button>
            )}
            {settable && (
              <button title={`Set ${v.evaluateName}`}
                onClick={() => { setDraft(v.value); setEditing(true); setErr('') }}
                className="ml-1 align-middle text-muted opacity-0 transition group-hover/var:opacity-100 hover:text-fg">
                <Icon.Pencil size={11} />
              </button>
            )}
            {err && !editing && <span className="ml-1 text-danger">{err}</span>}
          </span>
        )}
      </div>
      {open && (
        <div className="space-y-1">
          {kids === null && <p className="pl-3 text-[11px] text-muted">Reading…</p>}
          {(kids || []).map((k, i) => (
            <VarNode key={`${k.name}-${i}`} v={k} depth={depth + 1} containerRef={v.variablesReference}
              onExpand={onExpand} onSet={onSet} onFull={onFull} onChanged={() => setNonce((n) => n + 1)} />
          ))}
        </div>
      )}
    </div>
  )
}

export function WatchBox({ value, onChange, onAdd, watches, onRemove, allowCalls, onAllowCalls, idle, onIdle }) {
  return (
    <Panel id="evaluate" title="Evaluate" className="shrink-0">
      <div className="flex gap-1">
        <input className={inputCls} placeholder="request.NamespacedName"
          value={value} onChange={(e) => onChange(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') onAdd() }} />
        <Button size="sm" variant="outline" onClick={onAdd}><Icon.Plus size={14} /></Button>
      </div>
      <div className="mt-2 space-y-1">
        {watches.map((wv) => (
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
      <label className="mt-2 flex items-start gap-2 text-[11px] text-muted">
        <input type="checkbox" className="mt-0.5" checked={allowCalls} onChange={(e) => onAllowCalls(e.target.checked)} />
        <span>
          Allow function calls — an expression like <span className="font-mono">cr.Status.State()</span> then
          <span className="font-medium text-fg"> runs</span> inside the operator, against the real cluster.
        </span>
      </label>
      <label className="mt-2 flex items-center gap-2 text-[11px] text-muted">
        <span className="shrink-0">Resume if idle for</span>
        <select className={`${inputCls} w-36`} value={idle} onChange={(e) => onIdle(Number(e.target.value))}>
          {IDLE_CHOICES.map((c) => <option key={c.value} value={c.value}>{c.label}</option>)}
        </select>
      </label>
    </Panel>
  )
}

const LOG_CLS = {
  error: 'text-danger',
  warn: 'text-warning',
  output: 'text-muted',
  info: 'text-muted',
}

export function EventLog({ lines }) {
  const box = useRef(null)
  useEffect(() => { if (box.current) box.current.scrollTop = box.current.scrollHeight }, [lines.length])
  return (
    <Panel id="log" title="Session log" className="h-36 shrink-0" bodyClass="overflow-auto p-3">
      <div ref={box} className="space-y-0.5 font-mono text-[10px] leading-snug">
        {lines.length === 0 && <p className="text-muted">Nothing yet.</p>}
        {lines.map((ln, i) => (
          <div key={i} className={LOG_CLS[ln.kind] || 'text-muted'}>
            <span className="opacity-60">{new Date(ln.at).toLocaleTimeString()} </span>{ln.text}
          </div>
        ))}
      </div>
    </Panel>
  )
}
