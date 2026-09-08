import { useEffect, useRef, useState } from 'react'
import { Button, Field, inputCls } from './ui.jsx'
import { Icon } from './Icons.jsx'
import { diagApi, k3dApi } from '../lib/stackApi.js'
import { sendHandoff } from '../lib/handoff.js'
import { datagenApi } from '../lib/datagenApi.js'
import { TOOL_HELP } from '../lib/help.js'

// Shared "Diagnostics" cards for node property panels.
//   PGGatherCard — pg_gather report (PostgreSQL: pg / patroni / repmgr / spock)
//   PTStalkCard  — pt-stalk capture  (MySQL family: pxc / mysql / ps / innodb)
//   K8sCollectorCard — pt-k8s-debug-collector (a K3D Kubernetes cluster)
// Each starts an async capture, polls for completion, then exposes a download.
//
// The K3D one is keyed to the cluster *frame* rather than a node: the capture is
// of the whole cluster, and the k3s node it is taken through is an implementation
// detail. It also runs in a throwaway container beside the cluster rather than on
// it — see app/k8sdiag.go.

const noteCls = 'rounded-lg border px-2.5 py-2 text-xs leading-snug'

function DownloadLink({ href, children }) {
  return (
    <a href={href} download
      className="inline-flex items-center justify-center gap-1.5 rounded-lg bg-primary px-3 py-1.5 text-sm font-medium text-white transition hover:opacity-90">
      <Icon.External size={15} /> {children}
    </a>
  )
}

// usePoll runs fetchStatus now and re-polls every 3s while status.status === 'running'.
function usePoll(fetchStatus, deps) {
  const [status, setStatus] = useState({ status: 'idle' })
  const timer = useRef(null)
  const live = useRef(true)
  async function refresh() {
    try {
      const s = await fetchStatus()
      if (!live.current) return
      setStatus(s || { status: 'idle' })
      clearTimeout(timer.current)
      if (s && s.status === 'running') timer.current = setTimeout(refresh, 3000)
    } catch (e) {
      if (live.current) setStatus({ status: 'error', message: e.message })
    }
  }
  useEffect(() => {
    live.current = true
    refresh()
    return () => { live.current = false; clearTimeout(timer.current) }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)
  return [status, setStatus, refresh]
}

// PGGatherCard captures a pg_gather GatherReport.html from a chosen database.
export function PGGatherCard({ stackId, nodeId, defaultDb }) {
  const api = diagApi(stackId, nodeId)
  const [dbs, setDbs] = useState([])
  const [db, setDb] = useState(defaultDb || '')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState(null)
  const [status, setStatus, refresh] = usePoll(api.pgGatherStatus, [stackId, nodeId])

  useEffect(() => {
    let live = true
    datagenApi.databases(stackId, nodeId)
      .then((list) => { if (!live) return; setDbs(list || []); setDb((d) => d || (list || [])[0] || '') })
      .catch(() => {})
    return () => { live = false }
  }, [stackId, nodeId])

  async function start() {
    setErr(null); setBusy(true)
    try {
      await api.pgGatherStart(db)
      setStatus({ status: 'running', database: db })
      refresh()
    } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  const running = status.status === 'running'
  return (
    <div className="space-y-3">
      <p className="text-xs text-muted">
        <span className="font-medium text-fg/80">pg_gather</span> collects a diagnostic snapshot of a database
        (settings, activity, bloat, wait events…) and builds a single <span className="font-mono">GatherReport.html</span> you can download.
      </p>
      <Field label="Database to gather from" help={TOOL_HELP.pgGatherDatabase} hint="pg_gather runs against this database.">
        <select className={inputCls} value={db} disabled={running} onChange={(e) => setDb(e.target.value)}>
          {dbs.length === 0 && <option value="">loading…</option>}
          {dbs.map((d) => <option key={d} value={d}>{d}</option>)}
        </select>
      </Field>
      <Button size="sm" className="w-full" disabled={busy || running || !db} onClick={start}>
        <Icon.Arrow size={15} /> {running ? 'Collecting…' : 'Generate report'}
      </Button>
      {running && (
        <div className={`${noteCls} border-primary/30 bg-primary/10 text-primary`}>
          Collecting pg_gather data{status.database ? ` from ${status.database}` : ''}… this can take a little while.
        </div>
      )}
      {status.status === 'done' && !running && (
        <div className="space-y-2">
          <div className={`${noteCls} border-success/30 bg-success/15 text-success`}>
            Report ready{status.database ? ` for ${status.database}` : ''}.
          </div>
          <DownloadLink href={api.pgGatherDownloadURL()}>Download GatherReport.html</DownloadLink>
        </div>
      )}
      {(err || status.status === 'error') && (
        <div className={`${noteCls} border-danger/30 bg-danger/15 text-danger whitespace-pre-wrap`}>{err || status.message || 'Capture failed.'}</div>
      )}
    </div>
  )
}

// PTStalkCard captures pt-summary + pt-mysql-summary + pt-stalk into a downloadable tarball.
export function PTStalkCard({ stackId, nodeId }) {
  const api = diagApi(stackId, nodeId)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState(null)
  const [status, setStatus, refresh] = usePoll(api.ptStalkStatus, [stackId, nodeId])

  async function start() {
    setErr(null); setBusy(true)
    try {
      await api.ptStalkStart()
      setStatus({ status: 'running' })
      refresh()
    } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  const running = status.status === 'running'
  return (
    <div className="space-y-3">
      <p className="text-xs text-muted">
        <span className="font-medium text-fg/80">pt-stalk</span> captures <span className="font-mono">pt-summary</span>,{' '}
        <span className="font-mono">pt-mysql-summary</span> and a <span className="font-mono">pt-stalk</span> sample set into one
        downloadable archive. The sampling window runs for about <span className="text-fg/80">90 seconds</span> — the download appears when it finishes.
      </p>
      <Button size="sm" className="w-full" disabled={busy || running} onClick={start}>
        <Icon.Arrow size={15} /> {running ? 'Capturing…' : 'Start capture'}
      </Button>
      {running && (
        <div className={`${noteCls} border-primary/30 bg-primary/10 text-primary`}>
          Running pt-summary + pt-mysql-summary + pt-stalk (~90s of sampling)… leave this open; the download appears when ready.
        </div>
      )}
      {status.status === 'done' && !running && (
        <div className="space-y-2">
          <div className={`${noteCls} border-success/30 bg-success/15 text-success`}>Capture complete.</div>
          <div className="flex flex-wrap gap-2">
            <DownloadLink href={api.ptStalkDownloadURL()}>Download (.tar.gz)</DownloadLink>
            <button
              onClick={() => { sendHandoff('vs.target', JSON.stringify({ stackId, nodeId })); location.hash = 'stalk-summary' }}
              className="inline-flex items-center justify-center gap-1.5 rounded-lg border px-3 py-1.5 text-sm font-medium text-fg transition hover:bg-surface2">
              <Icon.Monitor size={15} /> Stalk Summary
            </button>
          </div>
        </div>
      )}
      {(err || status.status === 'error') && (
        <div className={`${noteCls} border-danger/30 bg-danger/15 text-danger whitespace-pre-wrap`}>{err || status.message || 'Capture failed.'}</div>
      )}
    </div>
  )
}

// K8sCollectorCard captures a pt-k8s-debug-collector cluster-dump of a K3D
// cluster: every namespace's resources, every pod's log, and the operator's own
// custom resources.
export function K8sCollectorCard({ stackId, frameId, operator }) {
  const api = k3dApi(stackId, frameId)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState(null)
  const [dumps, setDumps] = useState([])
  const [status, setStatus, refresh] = usePoll(api.collectStatus, [stackId, frameId])

  const loadDumps = async () => {
    try { setDumps((await api.collectDumps())?.dumps || []) } catch { /* the list is a convenience */ }
  }
  useEffect(() => { loadDumps() /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [stackId, frameId, status.status])

  async function start() {
    setErr(null); setBusy(true)
    try {
      await api.collectStart()
      setStatus({ status: 'running' })
      refresh()
    } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  const running = status.status === 'running'
  return (
    <div className="space-y-3">
      <p className="text-xs text-muted">
        <span className="font-medium text-fg/80">pt-k8s-debug-collector</span> dumps every namespace's resources, every pod's
        log and the operator's custom resources into one archive. It runs in a throwaway container beside the cluster, not on it
        {operator ? <> — with <span className="font-mono">-resource {operator}</span>, so the operator's own CRs come too</> : null}.
        A capture of a busy cluster takes a few minutes.
      </p>
      <Button size="sm" className="w-full" disabled={busy || running} onClick={start}>
        <Icon.Arrow size={15} /> {running ? 'Collecting…' : 'Start capture'}
      </Button>
      {running && (
        <div className={`${noteCls} border-primary/30 bg-primary/10 text-primary`}>
          Collecting cluster data… leave this open; the capture appears below when it finishes.
        </div>
      )}
      {(err || status.status === 'error') && (
        <div className={`${noteCls} border-danger/30 bg-danger/15 text-danger whitespace-pre-wrap`}>
          {err || status.message || 'Capture failed.'}
        </div>
      )}
      {dumps.length > 0 && (
        <div className="space-y-2">
          <div className="text-[11px] font-semibold uppercase tracking-wide text-muted">Kept captures</div>
          {dumps.map((d) => (
            <div key={d.id} className="flex flex-wrap items-center gap-2 rounded-lg border px-2.5 py-2 text-xs">
              <span className="font-mono text-fg/80">{new Date(d.capturedAt).toLocaleString()}</span>
              <span className="text-muted">{Math.round((d.sizeBytes || 0) / 1024)} KiB</span>
              <div className="ml-auto flex gap-2">
                <a href={`/api/k8sdumps/${d.id}/download`} download
                  className="rounded-md border px-2 py-1 font-medium text-fg hover:bg-surface2">Download</a>
                <button
                  onClick={() => { sendHandoff('op.dump', String(d.id)); location.hash = 'operator-summary' }}
                  className="rounded-md border px-2 py-1 font-medium text-fg hover:bg-surface2">Operator Summary</button>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
