import { useEffect, useRef, useState } from 'react'
import { Card, Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { opSummaryApi } from '../lib/opSummaryApi.js'
import { useHandoff, sendHandoff } from '../lib/handoff.js'

// Operator Summary — read a pt-k8s-debug-collector cluster-dump and say what is
// wrong with the cluster.
//
// Where Stalk Summary is ~90% charts, this is ~90% text, because the questions are
// different in kind: a pt-stalk capture is a time series and the answer is a shape,
// while a cluster-dump is one instant and the answer is a sentence — this pod is
// crash-looping, that custom resource never went ready, the operator has been
// failing to reconcile for an hour. So the page leads with verdicts, then the
// evidence behind them.
//
// The archive can come from a K3D node's Diagnostics tab, from a capture kept
// here, or from a file dropped on the page — a support engineer's cluster-dump
// from a customer cluster is the case that matters most, and it never touched
// this installation.

const TONE = { bad: 'danger', warn: 'warning', good: 'success' }
const SEV = { critical: 'danger', warning: 'warning', note: 'muted' }

function Empty({ children }) {
  return <div className="rounded-lg border border-dashed px-3 py-6 text-center text-xs text-muted">{children}</div>
}

// Verdicts are the whole point of the page: four questions, always answered, in
// the order a reader would ask them.
export function Verdicts({ verdicts }) {
  if (!verdicts?.length) return null
  return (
    <div className="grid gap-2 sm:grid-cols-2">
      {verdicts.map((v) => (
        <div key={v.key} className={`rounded-xl border p-3 ${
          v.tone === 'bad' ? 'border-danger/30 bg-danger/10'
            : v.tone === 'warn' ? 'border-warning/30 bg-warning/10'
              : 'border-success/30 bg-success/10'}`}>
          <div className="flex items-start gap-2">
            <Badge tone={TONE[v.tone] || 'muted'}>{v.tone === 'good' ? 'ok' : v.tone}</Badge>
            <div className="min-w-0">
              <div className="text-sm font-semibold">{v.title}</div>
              {v.detail && <div className="mt-0.5 text-xs text-muted">{v.detail}</div>}
            </div>
          </div>
        </div>
      ))}
    </div>
  )
}

export function Findings({ findings }) {
  if (!findings?.length) return <Empty>Nothing to report — no unhealthy workload, pod or custom resource in this capture.</Empty>
  return (
    <div className="space-y-1.5">
      {findings.map((f, i) => (
        <div key={i} className="flex items-start gap-2 rounded-lg border px-2.5 py-2">
          <Badge tone={SEV[f.severity] || 'muted'}>{f.severity}</Badge>
          <div className="min-w-0 flex-1">
            <div className="text-sm">{f.title}</div>
            {f.detail && <div className="mt-0.5 whitespace-pre-wrap break-words text-xs text-muted">{f.detail}</div>}
          </div>
          {f.where && <span className="shrink-0 font-mono text-[11px] text-muted">{f.where}</span>}
        </div>
      ))}
    </div>
  )
}

export function Workloads({ workloads }) {
  if (!workloads?.length) return <Empty>No Deployments or StatefulSets in this capture.</Empty>
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-xs">
        <thead className="text-muted">
          <tr><th className="py-1 pr-3">Kind</th><th className="py-1 pr-3">Namespace</th><th className="py-1 pr-3">Name</th><th className="py-1 pr-3">Ready</th></tr>
        </thead>
        <tbody>
          {workloads.map((w, i) => {
            const short = w.ready < w.desired
            return (
              <tr key={i} className="border-t">
                <td className="py-1 pr-3 text-muted">{w.kind}</td>
                <td className="py-1 pr-3 font-mono">{w.namespace}</td>
                <td className="py-1 pr-3 font-mono">{w.name}</td>
                <td className={`py-1 pr-3 font-mono ${short ? 'text-danger' : 'text-success'}`}>{w.ready}/{w.desired}</td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

export function Pods({ pods }) {
  if (!pods?.length) return <Empty>Every pod in this capture is Running and Ready.</Empty>
  return (
    <div className="space-y-2">
      {pods.map((p, i) => (
        <div key={i} className="rounded-lg border px-2.5 py-2">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-mono text-sm">{p.namespace}/{p.name}</span>
            <Badge tone="danger">{p.reason}</Badge>
            {p.restarts > 0 && <span className="text-[11px] text-muted">{p.restarts} restart{p.restarts === 1 ? '' : 's'}</span>}
            {p.age && <span className="text-[11px] text-muted">age {p.age}</span>}
            {p.node && <span className="ml-auto font-mono text-[11px] text-muted">{p.node}</span>}
          </div>
          {p.message && <div className="mt-1 whitespace-pre-wrap break-words text-xs text-muted">{p.message}</div>}
          {p.containers?.length > 0 && (
            <div className="mt-1.5 flex flex-wrap gap-1.5">
              {p.containers.map((c, j) => (
                <span key={j} className={`rounded px-1.5 py-0.5 font-mono text-[11px] ${c.ready ? 'bg-success/15 text-success' : 'bg-danger/15 text-danger'}`}>
                  {c.name} {c.state}{c.reason ? ` · ${c.reason}` : ''}{c.restartCount ? ` ×${c.restartCount}` : ''}
                </span>
              ))}
            </div>
          )}
          {p.logTail?.length > 0 && (
            <pre className="mt-1.5 max-h-40 overflow-auto rounded bg-surface2 p-2 text-[11px] leading-snug">{p.logTail.join('\n')}</pre>
          )}
        </div>
      ))}
    </div>
  )
}

export function CRs({ crs }) {
  if (!crs?.length) {
    return <Empty>No operator custom resources in this capture. Take it with a <span className="font-mono">-resource</span> that matches the cluster.</Empty>
  }
  return (
    <div className="space-y-2">
      {crs.map((cr, i) => {
        const ready = (cr.state || '').toLowerCase() === 'ready'
        return (
          <div key={i} className="rounded-lg border px-2.5 py-2">
            <div className="flex flex-wrap items-center gap-2">
              <span className="font-mono text-sm">{cr.namespace}/{cr.name}</span>
              <span className="text-[11px] text-muted">{cr.kind}</span>
              {cr.state && <Badge tone={ready ? 'success' : 'danger'}>{cr.state}</Badge>}
              {cr.version && <span className="text-[11px] text-muted">crVersion {cr.version}</span>}
              {cr.host && <span className="ml-auto font-mono text-[11px] text-muted">{cr.host}</span>}
            </div>
            {cr.components?.length > 0 && (
              <div className="mt-1.5 flex flex-wrap gap-1.5">
                {cr.components.map((c, j) => (
                  <span key={j} className={`rounded px-1.5 py-0.5 font-mono text-[11px] ${
                    c.size && c.ready < c.size ? 'bg-danger/15 text-danger' : 'bg-success/15 text-success'}`}>
                    {c.name} {c.ready}/{c.size}{c.status ? ` · ${c.status}` : ''}
                  </span>
                ))}
              </div>
            )}
            {cr.conditions?.filter((c) => c.status === 'True' && c.type === 'Error').map((c, j) => (
              <div key={j} className="mt-1 whitespace-pre-wrap break-words rounded bg-danger/10 px-2 py-1 text-xs text-danger">
                {c.reason ? `${c.reason}: ` : ''}{c.message}
              </div>
            ))}
          </div>
        )
      })}
    </div>
  )
}

export function Operators({ operators }) {
  if (!operators?.length) return <Empty>No operator pod log in this capture.</Empty>
  return (
    <div className="space-y-3">
      {operators.map((op, i) => (
        <div key={i} className="rounded-lg border px-2.5 py-2">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-mono text-sm">{op.namespace}/{op.pod}</span>
            <Badge tone={op.errors > 0 ? 'danger' : 'success'}>{op.kind}</Badge>
            <span className="text-[11px] text-muted">{op.lines} lines · {op.errors} error lines</span>
          </div>
          {op.groups?.length > 0 && (
            <div className="mt-1.5 space-y-1">
              {op.groups.map((g, j) => (
                <div key={j} className="flex items-start gap-2 text-xs">
                  <span className={`shrink-0 font-mono ${g.level === 'error' ? 'text-danger' : 'text-warning'}`}>×{g.count}</span>
                  <span className="min-w-0 whitespace-pre-wrap break-words">{g.message}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      ))}
    </div>
  )
}


// Deployment is the "what am I looking at" header: which operator, at what
// version, on what Kubernetes. Every value is read from the archive — the
// operator's version is the tag on its own Deployment image, which is the only
// place it is actually stated.
export function Deployment({ deployment }) {
  const d = deployment
  if (!d) return <Empty>No operator Deployment in this capture.</Empty>
  const kv = [
    ['Operator', d.operatorName ? `${d.operatorName}:${d.version}` : d.operator],
    ['CR version', d.crVersion],
    ['Kubernetes', d.kubernetes],
    ['Namespace', d.namespace],
    ['PMM client', d.pmm],
  ].filter(([, v]) => v)
  return (
    <dl className="grid gap-x-6 gap-y-1.5 text-sm sm:grid-cols-2">
      {kv.map(([k, v]) => (
        <div key={k} className="flex items-baseline justify-between gap-3 border-b py-1">
          <dt className="shrink-0 text-xs text-muted">{k}</dt>
          <dd className="min-w-0 truncate font-mono text-xs">{v}</dd>
        </div>
      ))}
    </dl>
  )
}

// Images is where a half-finished upgrade shows up and nowhere else: one row per
// distinct image, with what runs it.
export function Images({ images }) {
  if (!images?.length) return <Empty>No container images read from this capture.</Empty>
  return (
    <div className="space-y-1.5">
      {images.map((img, i) => (
        <div key={i} className="flex flex-wrap items-baseline gap-2 rounded-lg border px-2.5 py-1.5">
          <span className="font-mono text-xs">{img.repo}</span>
          <Badge tone="primary">{img.tag || 'untagged'}</Badge>
          <span className="text-[11px] text-muted">×{img.count}</span>
          <span className="ml-auto truncate font-mono text-[11px] text-muted">{img.used?.slice(0, 3).join(' · ')}</span>
        </div>
      ))}
    </div>
  )
}

// Secrets is a reference graph, not content — the collector never dumps Secret
// objects, and it should not. What matters is which secrets the deployment
// depends on: one named by a custom resource but mounted by nothing is the
// classic operator failure, and nothing else in the archive would mention it.
export function Secrets({ secrets }) {
  if (!secrets?.length) return <Empty>No secret references in this capture.</Empty>
  return (
    <div className="space-y-1.5">
      <p className="text-[11px] text-muted">
        Names only. pt-k8s-debug-collector deliberately does not collect Secret contents, so nothing here carries a value.
      </p>
      {secrets.map((s, i) => (
        <div key={i} className="flex flex-wrap items-baseline gap-2 rounded-lg border px-2.5 py-1.5">
          <span className="font-mono text-xs">{s.name}</span>
          <Badge tone={s.kind === 'tls' ? 'success' : s.kind === 'referenced' ? 'warning' : 'muted'}>{s.kind}</Badge>
          <span className="ml-auto truncate font-mono text-[11px] text-muted">
            {s.used?.length ? s.used.slice(0, 3).join(' · ') : 'named by a custom resource, mounted by no pod'}
          </span>
        </div>
      ))}
    </div>
  )
}

export function Backups({ backups }) {
  if (!backups?.length) return <Empty>No backup or restore custom resources in this capture.</Empty>
  return (
    <div className="space-y-2">
      {backups.map((b, i) => {
        const failed = /error|fail/i.test(b.state || '')
        const running = /running|starting|requested|waiting/i.test(b.state || '')
        return (
          <div key={i} className="rounded-lg border px-2.5 py-2">
            <div className="flex flex-wrap items-center gap-2">
              <Badge tone={failed ? 'danger' : running ? 'warning' : 'success'}>{b.restore ? 'restore' : 'backup'}</Badge>
              <span className="font-mono text-sm">{b.namespace}/{b.name}</span>
              {b.state && <span className={`text-xs ${failed ? 'text-danger' : running ? 'text-warning' : 'text-success'}`}>{b.state}</span>}
              {b.cluster && <span className="text-[11px] text-muted">cluster {b.cluster}</span>}
              {b.storage && <span className="ml-auto text-[11px] text-muted">{b.storage}{b.storageType ? ` · ${b.storageType}` : ''}</span>}
            </div>
            {b.destination && <div className="mt-1 truncate font-mono text-[11px] text-muted">{b.destination}</div>}
            {b.image && <div className="mt-0.5 truncate font-mono text-[11px] text-muted">{b.image}</div>}
            {b.error && <div className="mt-1 whitespace-pre-wrap rounded bg-danger/10 px-2 py-1 text-xs text-danger">{b.error}</div>}
          </div>
        )
      })}
    </div>
  )
}

// Certificates carry the only hard deadline in the whole archive.
export function Certs({ certs }) {
  if (!certs?.length) return <Empty>No TLS certificate metadata in this capture.</Empty>
  return (
    <div className="space-y-1.5">
      {certs.map((c, i) => {
        const d = c.daysLeft
        const tone = d == null ? 'muted' : d < 0 ? 'danger' : d <= 30 ? 'warning' : 'success'
        return (
          <div key={i} className="flex flex-wrap items-baseline gap-2 rounded-lg border px-2.5 py-1.5">
            <span className="font-mono text-xs">{c.secret}</span>
            <span className="text-[11px] text-muted">{c.entry}</span>
            {c.selfSigned && <Badge tone="muted">self-signed</Badge>}
            <Badge tone={tone}>{d == null ? 'unknown' : d < 0 ? `expired ${-d}d ago` : `${d}d left`}</Badge>
            <span className="ml-auto truncate font-mono text-[11px] text-muted">{c.subject} ← {c.issuer}</span>
          </div>
        )
      })}
    </div>
  )
}

// Storage: a claim that never bound is why a database never started.
export function Storage({ storage }) {
  if (!storage?.length) return <Empty>No persistent volume claims in this capture.</Empty>
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-xs">
        <thead className="text-muted">
          <tr><th className="py-1 pr-3">Claim</th><th className="py-1 pr-3">Status</th><th className="py-1 pr-3">Capacity</th><th className="py-1 pr-3">Class</th></tr>
        </thead>
        <tbody>
          {storage.map((v, i) => (
            <tr key={i} className="border-t">
              <td className="py-1 pr-3 font-mono">{v.namespace}/{v.name}</td>
              <td className={`py-1 pr-3 ${v.status === 'Bound' ? 'text-success' : 'text-danger'}`}>{v.status}</td>
              <td className="py-1 pr-3 font-mono">{v.capacity || v.requested}</td>
              <td className="py-1 pr-3 font-mono text-muted">{v.storageClass}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}


// Logs is Log Summary's verdict on the archive's log-shaped files, not a second
// classifier. The events themselves belong on the Log Summary page, which is
// built to hold a hundred thousand of them; what belongs here is the finding and
// the worst lines behind it.
export function Logs({ logs }) {
  if (!logs) return <Empty>No log-shaped files in this capture.</Empty>
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <p className="text-[11px] text-muted">
          {logs.sources} source{logs.sources === 1 ? '' : 's'} · {logs.events} classified events · read by Log Summary's own classifiers
          (Kubernetes Events, the operator's log, each database's error log).
        </p>
        {logs.bundleId && (
          // The same handoff whether the archive was captured here or uploaded
          // from a cluster this installation has never seen.
          <button
            onClick={() => { sendHandoff('ls.bundle', logs.bundleId); location.hash = 'log-summary' }}
            className="ml-auto shrink-0 rounded-md border px-2 py-1 text-xs font-medium hover:bg-surface2">
            Open the timeline in Log Summary →
          </button>
        )}
      </div>
      {logs.findings?.map((f, i) => (
        <div key={i} className="flex items-start gap-2 rounded-lg border px-2.5 py-2">
          <Badge tone={/crit|bad|error/i.test(f.severity) ? 'danger' : /warn/i.test(f.severity) ? 'warning' : 'muted'}>{f.severity}</Badge>
          <div className="min-w-0">
            <div className="text-sm">{f.title}</div>
            {f.detail && <div className="mt-0.5 whitespace-pre-wrap break-words text-xs text-muted">{f.detail}</div>}
          </div>
        </div>
      ))}
      {logs.worst?.length > 0 && (
        <div className="space-y-1">
          <div className="text-[11px] font-semibold uppercase tracking-wide text-muted">Worst events</div>
          {logs.worst.map((e, i) => (
            <div key={i} className="flex items-start gap-2 text-xs">
              <span className={`shrink-0 font-mono ${/crit|error/i.test(e.severity) ? 'text-danger' : 'text-warning'}`}>{e.severity}</span>
              <span className="shrink-0 font-mono text-muted">{e.node || e.source}</span>
              <span className="min-w-0 whitespace-pre-wrap break-words">{e.message}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

// Galera state decides whether a stopped PXC cluster can come back at all, and
// from which member. Nothing else in the archive answers it.
export function Galera({ galera }) {
  if (!galera?.length) return <Empty>No Galera state files — this is not a PXC cluster.</Empty>
  const anySafe = galera.some((g) => g.safeToBootstrap)
  return (
    <div className="space-y-2">
      {!anySafe && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-2 text-xs text-warning">
          No member is marked <span className="font-mono">safe_to_bootstrap</span>. If this cluster is fully stopped it will not
          start on its own — the member with the highest seqno has to be bootstrapped deliberately.
        </div>
      )}
      <div className="overflow-x-auto">
        <table className="w-full text-left text-xs">
          <thead className="text-muted">
            <tr><th className="py-1 pr-3">Member</th><th className="py-1 pr-3">seqno</th><th className="py-1 pr-3">safe_to_bootstrap</th><th className="py-1 pr-3">holds a view</th></tr>
          </thead>
          <tbody>
            {galera.map((g, i) => (
              <tr key={i} className="border-t">
                <td className="py-1 pr-3 font-mono">{g.pod}</td>
                <td className="py-1 pr-3 font-mono">{g.seqno}</td>
                <td className={`py-1 pr-3 font-mono ${g.safeToBootstrap ? 'text-success' : 'text-muted'}`}>{String(g.safeToBootstrap)}</td>
                <td className="py-1 pr-3 font-mono text-muted">{String(g.hasView)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

// PodSummaries is the database's own view of itself. Everything else in the
// archive is Kubernetes' opinion of the database; this is the database's.
export function PodSummaries({ summaries }) {
  if (!summaries?.length) return <Empty>No per-pod database summary in this capture.</Empty>
  return (
    <div className="space-y-2">
      {summaries.map((s, i) => (
        <div key={i} className="rounded-lg border px-2.5 py-2">
          <div className="font-mono text-sm">{s.namespace}/{s.pod}</div>
          <dl className="mt-1 grid gap-x-6 gap-y-0.5 text-xs sm:grid-cols-2">
            {Object.entries(s.facts || {}).map(([k, v]) => (
              <div key={k} className="flex items-baseline justify-between gap-3">
                <dt className="shrink-0 text-muted">{k}</dt>
                <dd className="min-w-0 truncate font-mono">{v}</dd>
              </div>
            ))}
          </dl>
        </div>
      ))}
    </div>
  )
}

// BackupLogs is where a backup says why it failed — which no custom resource records.
export function BackupLogs({ logs }) {
  if (!logs?.length) return <Empty>No xtrabackup logs in this capture.</Empty>
  return (
    <div className="space-y-2">
      {logs.map((l, i) => (
        <div key={i} className="rounded-lg border px-2.5 py-2">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-mono text-sm">{l.pod}</span>
            <span className="text-[11px] text-muted">{l.file}</span>
            {l.completedOk && <Badge tone="success">completed OK</Badge>}
            {l.errors?.length > 0 && <Badge tone="danger">{l.errors.length} error line{l.errors.length === 1 ? '' : 's'}</Badge>}
          </div>
          {l.errors?.length > 0 && (
            <pre className="mt-1 max-h-40 overflow-auto rounded bg-danger/10 p-2 text-[11px] leading-snug text-danger">{l.errors.join('\n')}</pre>
          )}
          {!l.errors?.length && l.tail?.length > 0 && (
            <pre className="mt-1 max-h-32 overflow-auto rounded bg-surface2 p-2 text-[11px] leading-snug">{l.tail.join('\n')}</pre>
          )}
        </div>
      ))}
    </div>
  )
}

// Everything below is one small table each: schedules, rollouts, budgets, RBAC.
export function Extras({ model }) {
  const rows = [
    ['Scheduled jobs', (model.schedules || []).map((s) => `${s.name} · ${s.schedule}${s.suspended ? ' · SUSPENDED' : ''}`)],
    ['Rollouts', (model.rollouts || []).map((r) => `${r.owner} · ${r.revisions} revision${r.revisions === 1 ? '' : 's'}${r.stuck ? ` · ${r.stuck} stale` : ''}`)],
    ['Disruption budgets', (model.budgets || []).map((b) => `${b.name} · ${b.healthy}/${b.desired} healthy · ${b.disruptions} disruption(s) allowed`)],
    ['Operator RBAC', (model.rbac || []).map((r) => `${r.kind} ${r.name} · ${r.rules} rule${r.rules === 1 ? '' : 's'}${r.wildcard ? ' · wildcard' : ''}`)],
    ['Rendered config', (model.config || []).map((c) => `${c.name} · ${(c.keys || []).join(', ')}`)],
  ].filter(([, v]) => v.length)
  if (!rows.length) return <Empty>Nothing else in this capture.</Empty>
  return (
    <div className="space-y-3">
      {rows.map(([label, items]) => (
        <div key={label}>
          <div className="text-[11px] font-semibold uppercase tracking-wide text-muted">{label}</div>
          <div className="mt-1 space-y-0.5">
            {items.map((t, i) => <div key={i} className="truncate font-mono text-xs">{t}</div>)}
          </div>
        </div>
      ))}
    </div>
  )
}

export default function OperatorSummary() {
  const [model, setModel] = useState(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)
  const [dumps, setDumps] = useState([])
  const [drag, setDrag] = useState(false)
  const fileRef = useRef(null)

  const reloadDumps = () => opSummaryApi.dumps().then((d) => setDumps(d || [])).catch(() => {})

  useEffect(() => {
    reloadDumps()
    // A K3D node's Diagnostics tab hands the page a capture to open, the same way
    // pt-stalk's card hands one to Stalk Summary.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // A K3D node's Diagnostics tab hands a kept capture over here.
  useHandoff('op.dump', (id) => run(() => opSummaryApi.fromDump(id)))

  async function run(fn) {
    setError(null); setLoading(true); setModel(null)
    try { setModel(await fn()) } catch (e) { setError(e.message) } finally { setLoading(false) }
  }

  const onFile = (file) => { if (file) run(() => opSummaryApi.upload(file)) }

  return (
    <div className="space-y-4">
      <Card
        title="Operator Summary"
        subtitle="A pt-k8s-debug-collector cluster-dump, distilled: what is not running, what the operator's custom resources say about themselves, and what the operator has been logging."
      >
        <div className="space-y-3">
          <div
            onDragOver={(e) => { e.preventDefault(); setDrag(true) }}
            onDragLeave={() => setDrag(false)}
            onDrop={(e) => { e.preventDefault(); setDrag(false); onFile(e.dataTransfer.files?.[0]) }}
            className={`rounded-xl border-2 border-dashed px-4 py-6 text-center transition ${drag ? 'border-primary bg-primary/5' : ''}`}
          >
            <p className="text-sm">Drop a <span className="font-mono">cluster-dump.tar.gz</span> here</p>
            <p className="mt-1 text-xs text-muted">
              From this installation, or from any cluster —
              <span className="font-mono"> pt-k8s-debug-collector</span> output is the same shape wherever it was taken.
            </p>
            <input ref={fileRef} type="file" accept=".gz,.tgz,.tar.gz" className="hidden"
              onChange={(e) => onFile(e.target.files?.[0])} />
            <Button size="sm" variant="outline" className="mt-3" onClick={() => fileRef.current?.click()}>
              <Icon.External size={15} /> Choose a file
            </Button>
          </div>

          {dumps.length > 0 && (
            <div className="space-y-1.5">
              <div className="text-[11px] font-semibold uppercase tracking-wide text-muted">Captures kept here</div>
              {dumps.map((d) => (
                <div key={d.id} className="flex flex-wrap items-center gap-2 rounded-lg border px-2.5 py-1.5 text-xs">
                  <span className="font-mono">{d.cluster}</span>
                  {d.operator && <Badge tone="primary">{d.operator}</Badge>}
                  <span className="text-muted">{new Date(d.capturedAt).toLocaleString()}</span>
                  {d.stackName && <span className="text-muted">· {d.stackName}</span>}
                  <div className="ml-auto flex gap-2">
                    <button onClick={() => run(() => opSummaryApi.fromDump(d.id))}
                      className="rounded-md border px-2 py-1 font-medium hover:bg-surface2">Analyse</button>
                    <a href={opSummaryApi.downloadURL(d.id)} download
                      className="rounded-md border px-2 py-1 font-medium hover:bg-surface2">Download</a>
                    <button onClick={() => opSummaryApi.remove(d.id).then(reloadDumps).catch((e) => setError(e.message))}
                      className="rounded-md border px-2 py-1 font-medium text-danger hover:bg-danger/10">Delete</button>
                  </div>
                </div>
              ))}
            </div>
          )}

          {loading && <div className="text-xs text-muted">Reading the archive…</div>}
          {error && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{error}</div>}
        </div>
      </Card>

      {model && (
        <>
          <Card title="Verdict" subtitle={`${model.source}${model.kubernetes ? ` · Kubernetes ${model.kubernetes}` : ''}`}>
            <Verdicts verdicts={model.verdicts} />
          </Card>

          <Card title="Findings" subtitle="Everything worth acting on, most severe first.">
            <Findings findings={model.findings} />
          </Card>

          <Card title="Deployment" subtitle="Which operator is running this, and at what version.">
            <Deployment deployment={model.deployment} />
          </Card>

          <Card title="Custom resources" subtitle="What the operator publishes about the clusters it manages.">
            <CRs crs={model.crs} />
          </Card>

          <Card title="Backups & restores" subtitle="Every backup and restore custom resource, newest first — including one caught mid-run.">
            <Backups backups={model.backups} />
          </Card>

          <Card title="Certificates" subtitle="Soonest to expire first. The only hard deadline in the archive.">
            <Certs certs={model.certs} />
          </Card>

          <Card title="Logs" subtitle="Kubernetes Events, the operator's log and each database's error log — read by Log Summary's classifiers.">
            <Logs logs={model.logs} />
          </Card>

          <Card title="Operator log" subtitle="Error and warning lines, with repeats collapsed.">
            <Operators operators={model.operators} />
          </Card>

          <Card title="Galera state" subtitle="Whether this cluster can bootstrap, and from which member.">
            <Galera galera={model.galera} />
          </Card>

          <Card title="Backup logs" subtitle="What xtrabackup itself wrote on each member.">
            <BackupLogs logs={model.backupLogs} />
          </Card>

          <Card title="Pods needing attention" subtitle="Only the unhealthy ones, with the tail of each pod's own log.">
            <Pods pods={model.pods} />
          </Card>

          <Card title="Workloads" subtitle="Deployments and StatefulSets, short ones first.">
            <Workloads workloads={model.workloads} />
          </Card>

          <Card title="Storage" subtitle="Persistent volume claims — unbound ones first.">
            <Storage storage={model.storage} />
          </Card>

          <Card title="Images" subtitle="Every distinct image this deployment runs. A half-finished upgrade shows up here and nowhere else.">
            <Images images={model.images} />
          </Card>

          <Card title="Secrets" subtitle="What this deployment depends on, by name.">
            <Secrets secrets={model.secrets} />
          </Card>

          <Card title="Database summary" subtitle="The database's own view of itself, from inside each pod.">
            <PodSummaries summaries={model.podSummaries} />
          </Card>

          <Card title="Schedules, rollouts, budgets and RBAC">
            <Extras model={model} />
          </Card>

          {model.collectorErrors?.length > 0 && (
            <Card title="The collector could not read everything" subtitle="These are the collector's own failures, not the cluster's — a summary built on a partial capture should say so.">
              <pre className="max-h-48 overflow-auto rounded bg-surface2 p-2 text-[11px] leading-snug">{model.collectorErrors.join('\n')}</pre>
            </Card>
          )}
        </>
      )}
    </div>
  )
}
