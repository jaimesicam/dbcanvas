import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Badge, Button, ConfirmButton, Field, inputCls } from './ui.jsx'
import { Icon } from './Icons.jsx'
import { Help } from './Tooltip.jsx'
import { stackApi } from '../lib/stackApi.js'

// ClusterSyncPanel — Percona ClusterSync for MongoDB (PCSM), run from a Linux Client
// (app/lcclustersync.go). Two faces: a setup card that picks a source and a target among the
// stack's MongoDB clusters (pairs PCSM does not support are shown, greyed, with the reason), and
// once a sync exists, ClusterSyncProgress — the live view also used by a K3D frame's operator-run
// sync. Polls every 2s while a sync is on the node and the tab is visible.

const fmtBytes = (n) => {
  if (n == null || Number.isNaN(n)) return '—'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = Number(n)
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++ }
  return `${v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${u[i]}`
}
const fmtNum = (n) => (n == null ? '—' : Number(n).toLocaleString())
const fmtDur = (s) => {
  if (s == null || !Number.isFinite(s)) return '—'
  s = Math.max(0, Math.round(s))
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}
const ago = (iso) => {
  if (!iso) return '—'
  const s = (Date.now() - new Date(iso).getTime()) / 1000
  return Number.isFinite(s) ? `${fmtDur(s)} ago` : '—'
}

// The phases a sync goes through, in order, and what each means — the stepper's labels.
const STEPS = [
  { id: 'clone', label: 'Clone', hint: 'Copying every collection from the source as it stands.' },
  { id: 'catchup', label: 'Catch up', hint: 'Applying the changes the source took while the clone ran.' },
  { id: 'replicate', label: 'Replicate', hint: 'Following the source live through its change stream. Finalize when the lag is low.' },
  { id: 'finalize', label: 'Finalize', hint: 'Building the remaining indexes on the target and stopping. One-way: a new sync starts over.' },
]

// phaseOf turns PCSM's status into one of: starting, cloning, catchup, replicating, paused,
// finalizing, finalized, failed, stopped.
export function phaseOf(status, service) {
  const s = status || {}
  const st = String(s.state || '').toLowerCase()
  if (st === 'failed') return 'failed'
  if (st === 'finalized' || s.finalization?.completed) return 'finalized'
  if (st === 'finalizing') return 'finalizing'
  if (st === 'paused') return 'paused'
  if (service && !['active', 'activating', 'reloading'].includes(service) && st !== 'running') return 'stopped'
  if (st === 'running') {
    const is = s.initialSync || {}
    if (!is.cloneCompleted) return 'cloning'
    if (!is.completed) return 'catchup'
    return 'replicating'
  }
  return 'starting'
}

const PHASE = {
  starting: { label: 'Starting', tone: 'primary', step: 0 },
  cloning: { label: 'Cloning', tone: 'primary', step: 0 },
  catchup: { label: 'Catching up', tone: 'primary', step: 1 },
  replicating: { label: 'Replicating', tone: 'success', step: 2 },
  paused: { label: 'Paused', tone: 'warning', step: -1 },
  finalizing: { label: 'Finalizing', tone: 'primary', step: 3 },
  finalized: { label: 'Finalized', tone: 'success', step: 4 },
  failed: { label: 'Failed', tone: 'danger', step: -1 },
  stopped: { label: 'Stopped', tone: 'muted', step: -1 },
}

function lagTone(lag) {
  if (lag == null) return 'text-muted'
  if (lag <= 5) return 'text-success'
  if (lag <= 60) return 'text-warning'
  return 'text-danger'
}

// Sparkline is a small SVG line of the last samples, with the newest value's dot.
function Sparkline({ points, height = 36 }) {
  if (points.length < 2) {
    return <div className="flex h-9 items-center text-[11px] text-muted">collecting samples…</div>
  }
  const w = 240
  const max = Math.max(1, ...points.map((p) => p.v))
  const step = w / (points.length - 1)
  const xy = points.map((p, i) => [i * step, height - 3 - (p.v / max) * (height - 6)])
  const line = xy.map(([x, y], i) => `${i ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)}`).join(' ')
  const area = `${line} L${w},${height} L0,${height} Z`
  const [lx, ly] = xy[xy.length - 1]
  return (
    <svg viewBox={`0 0 ${w} ${height}`} preserveAspectRatio="none" className="h-9 w-full overflow-visible" role="img" aria-label="lag over time">
      <path d={area} fill="var(--primary)" opacity="0.12" />
      <path d={line} fill="none" stroke="var(--primary)" strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
      <circle cx={lx} cy={ly} r="2.5" fill="var(--primary)" />
    </svg>
  )
}

function Stat({ label, value, sub, tone = '', help }) {
  return (
    <div className="rounded-lg bg-surface2 px-2.5 py-2">
      <div className="flex items-center gap-1 text-[10px] font-medium uppercase tracking-wide text-muted">{label}<Help text={help} size={11} /></div>
      <div className={`mt-0.5 font-mono text-sm font-semibold tabular-nums ${tone}`}>{value}</div>
      {sub && <div className="text-[10px] text-muted tabular-nums">{sub}</div>}
    </div>
  )
}

// Endpoint is one end of the source → target strip: a row, so a cluster name keeps the full
// width of the narrow Properties column rather than half of it.
function Endpoint({ side, label, version, topology, done }) {
  return (
    <div className="flex items-center gap-2.5">
      <span className={`h-2.5 w-2.5 shrink-0 rounded-full ${done ? 'bg-success' : 'bg-primary'}`} />
      <div className="min-w-0 flex-1">
        <div className="flex items-baseline justify-between gap-2">
          <span className="truncate text-sm font-semibold" title={label}>{shortLabel({ label })}</span>
          <span className="shrink-0 text-[10px] font-medium uppercase tracking-wide text-muted">{side}</span>
        </div>
        <div className="truncate text-[11px] text-muted">
          {[version && `MongoDB ${version}`, topology === 'sharded' ? 'sharded' : topology === 'replset' ? 'replica set' : topology].filter(Boolean).join(' · ')}
        </div>
      </div>
    </div>
  )
}

// Stepper shows where in clone → catch up → replicate → finalize the sync is.
function Stepper({ phase }) {
  const at = PHASE[phase]?.step ?? -1
  const halted = phase === 'paused' || phase === 'failed' || phase === 'stopped'
  // A halted sync keeps showing the step it halted on, from what it had finished.
  return (
    <ol className="grid grid-cols-4 gap-1">
      {STEPS.map((s, i) => {
        const done = at > i || phase === 'finalized'
        const active = at === i && !halted
        return (
          <li key={s.id} title={s.hint} className="flex flex-col items-center gap-1 text-center">
            <div className="flex w-full items-center">
              <span className={`h-0.5 flex-1 rounded ${i === 0 ? 'opacity-0' : done || active ? 'bg-primary' : 'bg-surface2'}`} />
              <span className={`flex h-6 w-6 shrink-0 items-center justify-center rounded-full text-[11px] font-semibold transition ${
                done ? 'bg-primary text-primary-fg' : active ? 'pulse-ring bg-primary/15 text-primary ring-2 ring-primary' : 'bg-surface2 text-muted'}`}>
                {done ? <Icon.Check size={13} /> : i + 1}
              </span>
              <span className={`h-0.5 flex-1 rounded ${i === STEPS.length - 1 ? 'opacity-0' : done ? 'bg-primary' : 'bg-surface2'}`} />
            </div>
            <span className={`text-[10px] font-medium ${active ? 'text-primary' : done ? 'text-fg' : 'text-muted'}`}>{s.label}</span>
          </li>
        )
      })}
    </ol>
  )
}

// useRates turns successive status samples into per-second rates (smoothed) and a lag history.
function useRates(status) {
  const prev = useRef(null)
  const [rates, setRates] = useState({ clone: null, applied: null, read: null })
  const [lagHist, setLagHist] = useState([])
  useEffect(() => {
    if (!status) return
    const now = Date.now()
    const cur = {
      t: now,
      cloned: status.initialSync?.clonedSizeBytes ?? null,
      applied: status.eventsApplied ?? null,
      read: status.eventsRead ?? null,
    }
    const p = prev.current
    if (p && now - p.t > 500) {
      const dt = (now - p.t) / 1000
      const rate = (a, b) => (a == null || b == null ? null : Math.max(0, (a - b) / dt))
      const ema = (old, v) => (v == null ? old : old == null ? v : old * 0.6 + v * 0.4)
      setRates((r) => ({
        clone: ema(r.clone, rate(cur.cloned, p.cloned)),
        applied: ema(r.applied, rate(cur.applied, p.applied)),
        read: ema(r.read, rate(cur.read, p.read)),
      }))
    }
    prev.current = cur
    if (status.lagTimeSeconds != null && status.initialSync?.completed) {
      setLagHist((h) => [...h.slice(-89), { t: now, v: Number(status.lagTimeSeconds) || 0 }])
    }
  }, [status])
  return { rates, lagHist }
}

// ClusterSyncProgress is the live view of one sync. `source`/`target` are { label, version,
// topology }; `actions` is { pause, resume, resumeFailed, finalize, stop, compare } — any may be
// missing, which hides its button. `extra` renders under the stats (the K3D CR's own status).
export function ClusterSyncProgress({ status, service, source, target, startedAt, log, actions = {}, busy, extra }) {
  const phase = phaseOf(status, service)
  const meta = PHASE[phase] || PHASE.starting
  const s = status || {}
  const is = s.initialSync || {}
  const { rates, lagHist } = useRates(status)
  const est = is.estimatedCloneSizeBytes
  const cloned = is.clonedSizeBytes
  const pct = est > 0 ? Math.min(100, (cloned / est) * 100) : is.cloneCompleted ? 100 : 0
  const eta = !is.cloneCompleted && rates.clone > 0 && est > cloned ? (est - cloned) / rates.clone : null
  const moving = ['cloning', 'catchup', 'replicating', 'starting', 'finalizing'].includes(phase)
  const canFinalize = phase === 'replicating' || (phase === 'paused' && is.completed)
  const unsuccessful = s.finalization?.unsuccessfulIndexes || []

  return (
    <div className="space-y-3">
      {/* source → target */}
      <div className="rounded-xl border bg-surface px-3 py-2.5">
        <Endpoint side="Source" {...source} />
        <div className="flex items-center gap-2.5 py-1">
          {/* centred under the 10px dots above and below */}
          <span className={`ml-1 h-7 w-0.5 shrink-0 rounded-full ${moving ? 'pcsm-flow-v' : phase === 'finalized' ? 'bg-success' : 'bg-surface2'}`} />
          <span className="shrink-0 whitespace-nowrap"><Badge tone={meta.tone}>{meta.label}</Badge></span>
          {s.info && <span className="truncate text-[11px] text-muted" title={s.info}>{s.info}</span>}
        </div>
        <Endpoint side="Target" {...target} done={phase === 'finalized'} />
      </div>

      <Stepper phase={phase} />

      {/* clone progress */}
      <div className="space-y-1">
        <div className="flex items-baseline justify-between text-xs">
          <span className="font-medium">Initial clone</span>
          <span className="tabular-nums text-muted">
            {is.cloneCompleted ? <span className="text-success">complete · {fmtBytes(cloned ?? est)}</span>
              : est ? `${fmtBytes(cloned)} of ${fmtBytes(est)} · ${pct.toFixed(1)}%` : 'estimating size…'}
          </span>
        </div>
        <div className="h-2.5 overflow-hidden rounded-full bg-surface2">
          <div className={`h-full rounded-full transition-all duration-700 ${is.cloneCompleted ? 'bg-success' : 'bg-primary'} ${phase === 'cloning' && !est ? 'w-1/3 animate-pulse' : ''}`}
            style={est || is.cloneCompleted ? { width: `${pct}%` } : undefined} />
        </div>
        {!is.cloneCompleted && (rates.clone > 0 || eta) && (
          <div className="flex justify-between text-[10px] text-muted tabular-nums">
            <span>{fmtBytes(rates.clone)}/s</span>
            {eta && <span>about {fmtDur(eta)} left</span>}
          </div>
        )}
      </div>

      {/* live numbers */}
      <div className="grid grid-cols-2 gap-1.5">
        <Stat label="Lag" value={!is.completed || s.lagTimeSeconds == null ? '—' : fmtDur(s.lagTimeSeconds)}
          tone={is.completed ? lagTone(s.lagTimeSeconds) : 'text-muted'}
          sub={is.completed ? 'behind the source' : 'measured once the initial sync is done'}
          help="How far the target trails the source, in the source's own clock. Finalize when this is near zero and the application is stopped." />
        <Stat label="Last applied" value={ago(s.lastReplicatedOpTime?.isoDate)}
          sub={s.lastReplicatedOpTime?.isoDate ? new Date(s.lastReplicatedOpTime.isoDate).toLocaleTimeString() : ''}
          help="The source operation time of the newest change applied on the target." />
        <Stat label="Read" value={fmtNum(s.eventsRead)} sub={`events${rates.read != null ? ` · ${fmtNum(Math.round(rates.read))}/s` : ''}`}
          help="Change-stream events read from the source since the sync started." />
        <Stat label="Applied" value={fmtNum(s.eventsApplied)} sub={`events${rates.applied != null ? ` · ${fmtNum(Math.round(rates.applied))}/s` : ''}`}
          help="Change-stream events written to the target. Read minus applied is the backlog in PCSM's queues." />
      </div>

      <div className="rounded-lg border px-2.5 py-2">
        <div className="mb-1 flex items-center justify-between text-[11px] text-muted">
          <span>{lagHist.length > 1 ? `Lag, last ${fmtDur((lagHist.at(-1).t - lagHist[0].t) / 1000)}` : 'Lag'}</span>
          {startedAt ? <span>started {fmtDur(Date.now() / 1000 - startedAt)} ago</span> : null}
        </div>
        {is.completed ? <Sparkline points={lagHist} />
          : <div className="flex h-9 items-center text-[11px] text-muted">The lag chart starts when the initial sync completes.</div>}
      </div>

      {extra}

      {(s.error || phase === 'failed') && (
        <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">
          <div className="font-semibold">PCSM reports a failure</div>
          <div className="mt-0.5 break-words font-mono text-[11px]">{s.error || s.info || 'see the log below'}</div>
        </div>
      )}
      {phase === 'stopped' && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-xs text-muted">
          The pcsm service is not running ({service}). Its log below says why; Start again to resume from the target's recovery state, or Stop to forget this sync.
        </div>
      )}
      {phase === 'finalized' && (
        <div className="rounded-lg border border-success/30 bg-success/10 px-3 py-2 text-xs text-muted">
          <span className="font-semibold text-success">Done.</span> The target holds the source's data and indexes and is no longer being written to by PCSM — point the application at it.
          {unsuccessful.length > 0 && <div className="mt-1 text-warning">{unsuccessful.length} index{unsuccessful.length === 1 ? '' : 'es'} could not be built: {unsuccessful.map((u) => `${u.namespace}.${u.indexName}`).join(', ')}</div>}
        </div>
      )}

      {/* controls */}
      <div className="flex flex-wrap gap-1.5">
        {actions.pause && (phase === 'cloning' || phase === 'catchup' || phase === 'replicating') && (
          <Button size="sm" variant="outline" disabled={!!busy} onClick={actions.pause}><Icon.Pause size={14} /> Pause</Button>
        )}
        {actions.resume && phase === 'paused' && (
          <Button size="sm" disabled={!!busy} onClick={actions.resume}><Icon.Play size={14} /> Resume</Button>
        )}
        {actions.resumeFailed && phase === 'failed' && (
          <Button size="sm" disabled={!!busy} onClick={actions.resumeFailed}><Icon.Refresh size={14} /> Resume from failure</Button>
        )}
        {actions.finalize && phase !== 'finalized' && phase !== 'finalizing' && (
          <ConfirmButton size="sm" disabled={!!busy || !canFinalize} onConfirm={actions.finalize} confirmLabel="Finalize — no way back"
            title={canFinalize ? 'Build the indexes and stop. A new sync starts from scratch.' : 'Available once the initial sync has completed'}>
            <Icon.Check size={14} /> Finalize
          </ConfirmButton>
        )}
        {actions.compare && (
          <Button size="sm" variant="outline" disabled={!!busy} onClick={actions.compare}><Icon.Table size={14} /> Verify counts</Button>
        )}
        {actions.stop && (
          <ConfirmButton size="sm" variant="ghost" disabled={!!busy} onConfirm={actions.stop} confirmLabel="Stop and forget?">
            <Icon.Close size={14} /> Stop
          </ConfirmButton>
        )}
        {busy && <span className="self-center text-[11px] text-muted">{busy}…</span>}
      </div>

      {log?.length > 0 && (
        <details className="group rounded-lg border">
          <summary className="flex cursor-pointer select-none items-center justify-between px-2.5 py-1.5 text-xs font-medium">
            <span className="flex items-center gap-1.5"><Icon.Logs size={13} /> pcsm log</span>
            <span className="text-[10px] text-muted">last {log.length} lines</span>
          </summary>
          <pre className="max-h-56 overflow-auto whitespace-pre-wrap break-all border-t bg-bg px-2.5 py-2 font-mono text-[10px] leading-relaxed text-muted">
            {log.join('\n')}
          </pre>
        </details>
      )}
    </div>
  )
}

// CompareTable shows document counts per collection on both sides of a sync.
export function CompareTable({ rows, at, onClose }) {
  const diff = rows.filter((r) => r.source !== r.target).length
  return (
    <div className="space-y-1.5 rounded-lg border p-2.5 animate-fade-in">
      <div className="flex items-center justify-between text-xs">
        <span className="font-medium">
          {rows.length === 0 ? 'No user collections on either side'
            : diff === 0 ? <span className="text-success">All {rows.length} collections match</span>
              : <span className="text-warning">{diff} of {rows.length} collections differ</span>}
        </span>
        <span className="flex items-center gap-2 text-[10px] text-muted">
          {at ? new Date(at * 1000).toLocaleTimeString() : ''}
          {onClose && <button className="rounded p-0.5 hover:bg-surface2" onClick={onClose} title="Close"><Icon.Close size={12} /></button>}
        </span>
      </div>
      {rows.length > 0 && (
        <div className="max-h-60 overflow-auto">
          <table className="w-full text-[11px] tabular-nums">
            <thead className="sticky top-0 bg-surface text-left text-muted">
              <tr><th className="py-1 font-medium">Collection</th><th className="py-1 text-right font-medium">Source</th><th className="py-1 text-right font-medium">Target</th><th className="w-5" /></tr>
            </thead>
            <tbody>
              {rows.map((r) => {
                const ok = r.source === r.target
                return (
                  <tr key={r.namespace} className="border-t">
                    <td className="max-w-0 truncate py-1 pr-2 font-mono" title={r.namespace}>{r.namespace}</td>
                    <td className="py-1 text-right">{r.source == null ? '—' : fmtNum(r.source)}</td>
                    <td className={`py-1 text-right ${ok ? '' : 'text-warning'}`}>{r.target == null ? '—' : fmtNum(r.target)}</td>
                    <td className="py-1 pl-1 text-right">{ok ? <Icon.Check size={12} className="inline text-success" /> : <span className="text-warning">Δ</span>}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

// CodeBlock is one copyable command block of the by-hand instructions.
function CodeBlock({ label, text, note }) {
  const [done, setDone] = useState(false)
  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between gap-2">
        <span className="text-[11px] font-medium">{label}</span>
        <button title="Copy" className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg"
          onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}>
          {done ? <Icon.Check size={13} /> : <Icon.Copy size={13} />}
        </button>
      </div>
      <pre className="max-h-64 overflow-auto whitespace-pre rounded-lg border bg-bg p-2 font-mono text-[10px] leading-relaxed text-fg">{text}</pre>
      {note && <div className="text-[10px] leading-snug text-muted">{note}</div>}
    </div>
  )
}

// ManualGuide is the "Do it by hand" section: the same sync as plain commands, filled in with this
// stack's hosts and names, for learning what the panel does or running it somewhere else.
export function ManualGuide({ intro, steps }) {
  if (!steps?.length) return null
  return (
    <details className="group rounded-lg border">
      <summary className="flex cursor-pointer select-none items-center justify-between px-2.5 py-1.5 text-xs font-medium">
        <span className="flex items-center gap-1.5"><Icon.Terminal size={13} /> Do it by hand</span>
        <Icon.Chevron size={12} className="-rotate-90 transition group-open:rotate-0" />
      </summary>
      <div className="space-y-3 border-t px-2.5 py-2.5">
        {intro && <div className="text-[11px] leading-snug text-muted">{intro}</div>}
        {steps.map((st, i) => <CodeBlock key={i} label={`${i + 1}. ${st.label}`} text={st.text} note={st.note} />)}
      </div>
    </details>
  )
}

// uriOf is a connection string for the instructions, with the password left to fill in.
const uriOf = (user, hosts, rs) => `mongodb://${user}:<password>@${hosts}/?authSource=admin${rs ? `&replicaSet=${rs}` : ''}`
const adminURIOf = (hosts, rs) => `mongodb://admin@${hosts}/?authSource=admin${rs ? `&replicaSet=${rs}` : ''}`
const rolesJS = (roles) => `[${(roles || []).map((r) => `"${r}"`).join(', ')}]`
export const createUserCmd = (adminURI, user, roles) =>
  `mongosh "${adminURI}" --eval '
db.getSiblingDB("admin").createUser({
  user: "${user}",
  pwd: passwordPrompt(),
  roles: ${rolesJS(roles)}
})'`

// lcManualSteps is the Linux Client's sync by hand: the two accounts, the package's own
// environment file, and pcsm's commands.
function lcManualSteps(state, src, dst) {
  if (!src || !dst) return []
  const deb = ['ubuntu', 'debian'].includes(state.os)
  const envFile = deb ? '/etc/default/pcsm' : '/etc/sysconfig/pcsm'
  const su = state.sourceUser || 'pcsm-source'
  const tu = state.targetUser || 'pcsm-target'
  const srcURI = uriOf(su, src.hosts, src.replicaSet)
  const dstURI = uriOf(tu, dst.hosts, dst.replicaSet)
  return [
    { label: `Create the source account on ${shortLabel(src)}`, text: createUserCmd(adminURIOf(src.hosts, src.replicaSet), su, state.sourceRoles),
      note: "mongosh asks for the cluster's admin password (the cluster node's Credentials tab), then for the new account's." },
    { label: `Create the target account on ${shortLabel(dst)}`, text: createUserCmd(adminURIOf(dst.hosts, dst.replicaSet), tu, state.targetRoles) },
    { label: 'Point the pcsm service at both — on this node (its terminal is root)', text:
`tee ${envFile} >/dev/null <<'EOF'
PCSM_SOURCE_URI="${srcURI}"
PCSM_TARGET_URI="${dstURI}"
EOF
systemctl restart pcsm
journalctl -u pcsm -f        # Ctrl-C once it says "Starting HTTP server"`,
      note: <>The package reads <span className="font-mono">{envFile}</span>. The panel writes its own <span className="font-mono">/etc/dbcanvas/pcsm.env</span> through
        a drop-in, which wins — remove <span className="font-mono">/etc/systemd/system/pcsm.service.d/dbcanvas.conf</span> (and <span className="font-mono">systemctl daemon-reload</span>) to take over by hand.</> },
    { label: 'Start, watch, finish', text:
`pcsm start                         # or: --include-namespaces=shop.* --exclude-namespaces=shop.audit
pcsm status                        # the JSON this panel reads
curl -s localhost:2242/status; echo  # the HTTP API behind it (localhost only)
pcsm pause
pcsm resume                        # a failed sync: pcsm resume --from-failure
pcsm finalize                      # once lag is near 0 and the application has stopped writing` },
    { label: 'Start over against the same target', text:
`systemctl stop pcsm
pcsm reset --target "${dstURI}"
systemctl start pcsm && pcsm start`,
      note: "PCSM keeps its recovery state in the target's percona_clustersync_mongodb database; reset clears it so the next start is a fresh clone." },
    { label: 'Or run it in the foreground, without the service', text:
`systemctl stop pcsm               # both listen on localhost:2242
pcsm --source "${srcURI}" \\
     --target "${dstURI}"
# then, from another shell: pcsm start` },
  ]
}

// shortLabel drops the "(replica set)" / "(sharded, via …)" suffix the target list carries — the
// picker states the topology itself, and the column is narrow.
const shortLabel = (e) => String(e?.label || '').replace(/\s*\((replica set|sharded[^)]*)\)$/, '')

// SetupCard picks the pair and starts the sync.
function SetupCard({ state, onStart, busy, onPair }) {
  const eps = state.endpoints || []
  const pairs = state.pairs || []
  const [src, setSrc] = useState('')
  const [dst, setDst] = useState('')
  const [adv, setAdv] = useState(false)
  const [include, setInclude] = useState('')
  const [exclude, setExclude] = useState('')
  const [fresh, setFresh] = useState(true)
  const issueOf = useCallback((s, t) => pairs.find((p) => p.source === s && p.target === t)?.issue || '', [pairs])

  // Preselect a workable pair: the first source with any supported target, and that target.
  useEffect(() => {
    if (src || eps.length < 2) return
    for (const s of eps) {
      const t = eps.find((x) => x.id !== s.id && !issueOf(s.id, x.id))
      if (t) { setSrc(s.id); setDst(t.id); return }
    }
    setSrc(eps[0].id)
  }, [eps, src, issueOf])

  useEffect(() => { onPair?.(src, dst) }, [src, dst]) // eslint-disable-line react-hooks/exhaustive-deps

  if (eps.length < 2) {
    return (
      <div className="rounded-lg border border-dashed px-3 py-3 text-xs text-muted">
        A sync needs two running MongoDB clusters in this stack — a <span className="font-medium text-fg">PS MongoDB replica set</span> or a
        {' '}<span className="font-medium text-fg">sharded cluster</span> for each side ({eps.length} found). Standalone servers have no change stream for PCSM to follow.
      </div>
    )
  }
  const s = eps.find((e) => e.id === src)
  const t = eps.find((e) => e.id === dst)
  const issue = src && dst ? issueOf(src, dst) : ''
  const path = s && t && !issue ? (s.major === t.major ? `same version (${s.major})` : `upgrade path ${s.major} → ${t.major}`) : ''

  const side = (label, value, setValue, other, isTarget) => (
    <Field label={label}>
      <select className={inputCls} value={value} disabled={!!busy} onChange={(e) => setValue(e.target.value)}>
        <option value="">— select —</option>
        {eps.map((e) => {
          const why = isTarget && other ? issueOf(other, e.id) : ''
          return (
            <option key={e.id} value={e.id} disabled={e.id === other || (isTarget && !!why)}>
              {shortLabel(e)} · {e.major} {e.topology === 'sharded' ? 'sharded' : 'RS'}{why && e.id !== other ? ` — ${why}` : ''}
            </option>
          )
        })}
      </select>
    </Field>
  )

  return (
    <div className="space-y-2.5 rounded-xl border p-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">New sync</span>
        <Help text="PCSM clones every collection from the source, then follows its change stream so the target keeps up, until you finalize. The target must be Percona Server for MongoDB; the source may be any MongoDB 6.0, 7.0 or 8.0. DBCanvas creates a pcsm-source and a pcsm-target account with the roles PCSM documents." />
      </div>
      {side('Source', src, (v) => { setSrc(v); if (v === dst) setDst('') }, '', false)}
      <div className="flex justify-center text-muted"><Icon.Arrow size={16} className="rotate-90" /></div>
      {side('Target', dst, setDst, src, true)}
      {s && t && (
        <div className={`rounded-lg px-2.5 py-1.5 text-[11px] ${issue ? 'bg-danger/10 text-danger' : 'bg-success/10 text-success'}`}>
          {issue || <>Supported: {s.topology === t.topology ? (s.topology === 'sharded' ? 'sharded → sharded' : 'replica set → replica set') : 'sharded → replica set'}, {path}.</>}
        </div>
      )}
      {t && !issue && (
        <div className="rounded-lg bg-warning/10 px-2.5 py-1.5 text-[11px] text-muted">
          Collections on <span className="font-medium text-fg">{shortLabel(t)}</span> that also exist on the source will be overwritten.
        </div>
      )}
      <button type="button" className="flex items-center gap-1 text-[11px] font-medium text-muted hover:text-fg" onClick={() => setAdv(!adv)}>
        <Icon.Chevron size={12} className={`transition ${adv ? '' : '-rotate-90'}`} /> Options
      </button>
      {adv && (
        <div className="space-y-2 animate-fade-in">
          <Field label="Only these namespaces" hint="db.* or db.collection, comma-separated. Empty = everything.">
            <input className={inputCls} value={include} placeholder="shop.*, crm.customers" onChange={(e) => setInclude(e.target.value)} />
          </Field>
          <Field label="Skip these namespaces" hint="Applied after the list above.">
            <input className={inputCls} value={exclude} placeholder="shop.audit_log" onChange={(e) => setExclude(e.target.value)} />
          </Field>
          <label className="flex items-start gap-2 text-xs">
            <input type="checkbox" className="mt-0.5" checked={fresh} onChange={(e) => setFresh(e.target.checked)} />
            <span>Start fresh — clear PCSM's recovery state on the target first (<span className="font-mono">pcsm reset</span>). Turn off to resume a sync this target was already receiving.</span>
          </label>
        </div>
      )}
      <Button className="w-full" disabled={!!busy || !s || !t || !!issue}
        onClick={() => onStart({ source: src, target: dst, include: include ? [include] : [], exclude: exclude ? [exclude] : [], fresh })}>
        <Icon.Play size={15} /> {busy ? `${busy}…` : 'Start sync'}
      </Button>
    </div>
  )
}

export default function ClusterSyncPanel({ stackId, nodeId }) {
  const [state, setState] = useState(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState('')
  const [cmp, setCmp] = useState(null)
  const [pair, setPair] = useState(['', ''])
  const alive = useRef(true)

  const load = useCallback(async () => {
    try {
      const s = await stackApi.pcsm(stackId, nodeId)
      if (alive.current) { setState(s); setErr('') }
    } catch (e) { if (alive.current) setErr(e.message) }
  }, [stackId, nodeId])

  // Poll: fast while a sync is on the node, slow otherwise; never while the tab is hidden.
  useEffect(() => {
    alive.current = true
    let t
    const tick = async () => {
      if (!document.hidden) await load()
      if (!alive.current) return
      t = setTimeout(tick, state?.sync ? 2000 : 10000)
    }
    tick()
    return () => { alive.current = false; clearTimeout(t) }
  }, [load, !!state?.sync]) // eslint-disable-line react-hooks/exhaustive-deps

  const run = async (label, fn) => {
    setBusy(label); setErr('')
    try { await fn(); await load() } catch (e) { setErr(e.message) } finally { setBusy('') }
  }

  const sync = state?.sync
  const sideOf = useMemo(() => (side) => side && {
    label: side.label, version: side.version, topology: side.topology,
  }, [])

  if (!state && !err) {
    return <div className="rounded-lg border px-3 py-3 text-xs text-muted">Loading ClusterSync…</div>
  }
  if (state && !state.installed) return null

  return (
    <div className="space-y-2.5 rounded-xl border p-3">
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <div className="flex items-center gap-1.5 text-sm font-semibold"><Icon.Share size={15} /> ClusterSync</div>
          <div className="text-[10px] text-muted">Percona ClusterSync for MongoDB{state?.version ? ` · pcsm ${state.version}` : ''}</div>
        </div>
        {/* The unit's state only means something once a sync is set up; before that it is
            simply stopped, which reads as a fault. */}
        {sync && state?.service && <span className="shrink-0 whitespace-nowrap"><Badge tone={state.service === 'active' ? 'success' : 'warning'}>{state.service === 'active' ? 'running' : state.service}</Badge></span>}
      </div>

      {state && !sync && <SetupCard state={state} busy={busy} onPair={(s, d) => setPair([s, d])}
        onStart={(body) => run('Creating accounts and starting', () => stackApi.pcsmStart(stackId, nodeId, body))} />}

      {state && sync && (
        <ClusterSyncProgress
          status={state.status} service={state.service} log={state.log}
          source={sideOf(sync.source)} target={sideOf(sync.target)} startedAt={sync.startedAt} busy={busy}
          actions={{
            pause: () => run('Pausing', () => stackApi.pcsmAction(stackId, nodeId, 'pause')),
            resume: () => run('Resuming', () => stackApi.pcsmAction(stackId, nodeId, 'resume')),
            resumeFailed: () => run('Resuming', () => stackApi.pcsmAction(stackId, nodeId, 'resume-failed')),
            finalize: () => run('Finalizing', () => stackApi.pcsmAction(stackId, nodeId, 'finalize')),
            compare: () => run('Counting documents', async () => setCmp(await stackApi.pcsmCompare(stackId, nodeId))),
            stop: () => run('Stopping', async () => { await stackApi.pcsmStop(stackId, nodeId); setCmp(null) }),
          }}
          extra={cmp && <CompareTable rows={cmp.rows} at={cmp.at} onClose={() => setCmp(null)} />}
        />
      )}
      {err && <div className="rounded-lg bg-danger/10 px-2.5 py-1.5 text-[11px] text-danger">{err}</div>}
      {state && (() => {
        const [s, d] = sync ? [sync.source.id, sync.target.id] : pair
        const ep = (id) => (state.endpoints || []).find((e) => e.id === id)
        return <ManualGuide steps={lcManualSteps(state, ep(s), ep(d))}
          intro={<>The same sync with <span className="font-mono">mongosh</span> and <span className="font-mono">pcsm</span> on this node's terminal — what the panel does for you, step by step. Replace each <span className="font-mono">&lt;password&gt;</span>.</>} />
      })()}
    </div>
  )
}
