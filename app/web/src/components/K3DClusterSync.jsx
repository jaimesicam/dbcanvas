import { useCallback, useEffect, useRef, useState } from 'react'
import { Badge, Button, ConfirmButton, Field, inputCls } from './ui.jsx'
import { Icon } from './Icons.jsx'
import { Help } from './Tooltip.jsx'
import { k3dApi } from '../lib/stackApi.js'
import { ClusterSyncProgress, ManualGuide, createUserCmd } from './ClusterSyncPanel.jsx'

// K3DClusterSync is a PSMDB-operator K3D cluster's ClusterSync tab (app/k3dclustersync.go). With a
// sync: the same live view a Linux Client's sync gets, fed by PCSM's own status from inside the
// operator's pod, with the custom resource beside it — and Stop and remove, which deletes the CR
// and its Secret and clears PCSM's state on the target so another sync can start clean. Without
// one: a source picker (the operator's rules applied, each refusal with its reason) and Start.
// Either way, "Do it by hand" spells out the kubectl that does the same.

// k3dManualSteps is the operator-run sync by hand, built from what the server says it applies.
function k3dManualSteps(st, source) {
  if (!st) return []
  const ns = st.namespace || 'default'
  const k = `kubectl -n ${ns}`
  const uri = source?.uri || st.sync?.uri || '<source-uri>'
  // The admin connection string for the source, from the operator-style URI (hosts/admin?replicaSet=…).
  const m = /^mongodb:\/\/([^/]+)\/admin(?:\?replicaSet=([^&]+))?/.exec(uri)
  const adminURI = m ? `mongodb://admin@${m[1]}/?authSource=admin${m[2] ? `&replicaSet=${m[2]}` : ''}` : '<source admin URI>'
  const cr = String(st.crTemplate || '').replace('__SOURCE_URI__', uri)
  const [pod, container] = st.targetPod || [`${st.target}-rs0-0`, 'mongod']
  const srcLabel = source?.label || st.sync?.source || 'the source'
  return [
    { label: `Create the source account on ${srcLabel}`, text: createUserCmd(adminURI, st.user || 'pcsm-source', st.roles),
      note: "Run it from the source's own node or any host with mongosh; it asks for the cluster's admin password (its Credentials tab), then the new account's." },
    { label: 'Store its credentials in a Secret, beside the target', text:
`${k} create secret generic ${st.name}-source \\
  --from-literal=username=${st.user || 'pcsm-source'} \\
  --from-literal=password='<the password from step 1>'`,
      note: <>Run kubectl from this node's <span className="font-medium">kubectl</span> tab, or a Linux Client with the stack's kubeconfig.</> },
    { label: 'Create the ClusterSync — exactly what DBCanvas applies', text: `${k} apply -f - <<'EOF'\n${cr}EOF`,
      note: "The probes and env are DBCanvas's additions for a small lab cluster: the operator's default probes give /status four seconds and kill PCSM mid-clone, and PCSM sizes its workers from the host's CPUs. Percona's example omits both." },
    { label: 'Watch it', text:
`${k} get psmdb-clustersync                      # MODE, STATE, LAG(S)
${k} exec deploy/${st.name}-pcsm -- pcsm status   # PCSM's own status, as this tab shows it
${k} logs deploy/${st.name}-pcsm -f` },
    { label: 'Pause, resume, finalize', text:
`${k} patch psmdb-clustersync ${st.name} --type=merge -p '{"spec":{"mode":"paused"}}'
${k} patch psmdb-clustersync ${st.name} --type=merge -p '{"spec":{"mode":"running"}}'
${k} patch psmdb-clustersync ${st.name} --type=merge -p '{"spec":{"mode":"finalized"}}'   # one-way` },
    { label: 'Stop and remove — and clear PCSM\'s state for the next sync', text:
`${k} delete psmdb-clustersync ${st.name}
${k} delete secret ${st.name}-source
PW=$(${k} get secret ${st.target}-secrets -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d)
${k} exec ${pod} -c ${container} -- mongosh "mongodb://databaseAdmin:$PW@localhost:27017/admin" \\
  --eval 'db.getSiblingDB("${st.stateDB || 'percona_clustersync_mongodb'}").dropDatabase()'`,
      note: "Deleting the custom resource leaves PCSM's heartbeat and recovery data on the target; a ClusterSync created later finds it, tries to resume a run that is gone, and crash-loops. The copied data stays." },
  ]
}

function SyncSetup({ st, busy, onStart, source, setSource }) {
  const sources = st.sources || []
  const sel = sources.find((x) => x.id === source)
  useEffect(() => {
    if (!source && sources.length) setSource((sources.find((x) => !x.issue) || sources[0]).id)
  }, [sources, source, setSource])
  if (!st.supported) {
    return <div className="rounded-lg border border-dashed px-3 py-3 text-xs text-muted">Operator {st.operatorVer} has no PerconaServerMongoDBClusterSync — it arrived in 1.23.0.</div>
  }
  return (
    <div className="space-y-2.5 rounded-xl border p-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">New sync into {st.target}</span>
        <Help text="DBCanvas creates a pcsm-source account on the source, a Secret with its credentials, and a PerconaServerMongoDBClusterSync. The operator runs PCSM beside this cluster: it clones the source, then follows its change stream until you finalize. The operator requires the same major on both sides and matching topology." />
      </div>
      {sources.length === 0 ? (
        <div className="text-xs text-muted">Add a PS MongoDB replica set{st.sharding ? ' or sharded cluster' : ''} to the canvas to sync from.</div>
      ) : (
        <Field label="Source">
          <select className={inputCls} value={source} disabled={!!busy} onChange={(e) => setSource(e.target.value)}>
            {sources.map((x) => (
              <option key={x.id} value={x.id} disabled={!!x.issue}>
                {x.label} · {x.major || 'latest'} {x.topology === 'sharded' ? 'sharded' : 'RS'}{x.issue ? ' — unavailable' : ''}
              </option>
            ))}
          </select>
        </Field>
      )}
      {sel && (
        <div className={`rounded-lg px-2.5 py-1.5 text-[11px] ${sel.issue ? 'bg-danger/10 text-danger' : 'bg-success/10 text-success'}`}>
          {sel.issue || <>Supported: {sel.topology === 'sharded' ? 'sharded → sharded' : 'replica set → replica set'}, MongoDB {sel.major}.</>}
        </div>
      )}
      <Button className="w-full" disabled={!!busy || !sel || !!sel.issue} onClick={() => onStart(source)}>
        <Icon.Play size={15} /> {busy ? `${busy}…` : 'Start sync'}
      </Button>
    </div>
  )
}

export default function K3DClusterSync({ stackId, frame, cfg }) {
  const api = k3dApi(stackId, frame.id)
  const [st, setSt] = useState(null)
  const [err, setErr] = useState('')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState('')
  const [source, setSource] = useState('')
  const alive = useRef(true)
  const load = useCallback(async () => {
    try {
      const s = await k3dApi(stackId, frame.id).clusterSync()
      if (alive.current) { setSt(s); setErr('') }
    } catch (e) { if (alive.current) setErr(e.message) }
  }, [stackId, frame.id])
  useEffect(() => {
    alive.current = true
    let t
    const tick = async () => {
      if (!document.hidden) await load()
      if (alive.current) t = setTimeout(tick, st?.sync ? 3000 : 10000)
    }
    tick()
    return () => { alive.current = false; clearTimeout(t) }
  }, [load, !!st?.sync]) // eslint-disable-line react-hooks/exhaustive-deps
  const run = async (label, fn) => {
    setBusy(label); setErr(''); setNote('')
    try { const r = await fn(); if (r?.note) setNote(r.note); await load() } catch (e) { setErr(e.message) } finally { setBusy('') }
  }

  const info = st?.sync
  const crStatus = st?.cr?.status || {}
  const crSpec = st?.cr?.spec || {}
  // The operator's view, for when PCSM's own is not reachable (the pod is still starting).
  const status = st?.status || (crStatus.state ? { state: crStatus.state, lagTimeSeconds: crStatus.lagTimeSeconds, error: crStatus.error } : null)
  // The pod being Running is what "the service is up" means here.
  const service = st?.service === 'running' ? 'active' : st?.service || ''
  const topology = cfg.sharding ? 'sharded' : 'replset'

  return (
    <div className="space-y-2.5">
      {!st && !err && <div className="text-xs text-muted">Loading ClusterSync…</div>}
      {st && !info && (
        <SyncSetup st={st} busy={busy} source={source} setSource={setSource}
          onStart={(id) => run('Creating the account and the ClusterSync', () => api.clusterSyncStart(id))} />
      )}
      {info && (
        <ClusterSyncProgress
          status={status} service={service} log={st?.log}
          source={{ label: info.source, version: info.sourceVersion, topology }}
          target={{ label: st?.target || cfg.crName || 'this cluster', version: '', topology }}
          startedAt={info.startedAt} busy={busy}
          actions={{
            pause: () => run('Pausing', () => api.clusterSyncMode('pause')),
            resume: () => run('Resuming', () => api.clusterSyncMode('resume')),
            finalize: () => run('Finalizing', () => api.clusterSyncMode('finalize')),
          }}
          extra={
            <div className="space-y-1 rounded-lg bg-surface2 px-2.5 py-2 text-[11px]">
              <div className="flex flex-wrap items-center justify-between gap-1">
                <span className="truncate font-medium" title="PerconaServerMongoDBClusterSync">psmdb-clustersync</span>
                <span className="flex shrink-0 gap-1 whitespace-nowrap">
                  {crSpec.mode && <Badge tone="primary">mode {crSpec.mode}</Badge>}
                  {crStatus.state && <Badge tone={crStatus.state === 'failed' ? 'danger' : 'muted'}>{crStatus.state}</Badge>}
                </span>
              </div>
              <div className="truncate font-mono text-muted" title={info.name}>{info.name} · {st?.namespace}</div>
              <div className="truncate font-mono text-muted" title={info.uri}>{info.uri}</div>
              <div className="truncate font-mono text-muted">{info.image}</div>
            </div>
          }
        />
      )}
      {info && (
        <ConfirmButton size="sm" variant="outline" className="w-full" disabled={!!busy} confirmLabel="Delete the ClusterSync and clear its state?"
          onConfirm={() => run('Stopping and removing', () => api.clusterSyncStop())}>
          <Icon.Trash size={14} /> Stop and remove
        </ConfirmButton>
      )}
      {info && <div className="text-[10px] leading-snug text-muted">Deletes the custom resource (the operator removes PCSM with it) and its Secret, and clears PCSM's state on {st?.target} so a later sync starts clean. The data already copied stays.</div>}
      {note && <div className="rounded-lg bg-warning/10 px-2.5 py-1.5 text-[11px] text-muted">{note}</div>}
      {st?.crError && <div className="rounded-lg bg-warning/10 px-2.5 py-1.5 text-[11px] text-muted">{st.crError}</div>}
      {err && <div className="rounded-lg bg-danger/10 px-2.5 py-1.5 text-[11px] text-danger">{err}</div>}
      {st?.supported && (
        <ManualGuide steps={k3dManualSteps(st, info ? null : (st.sources || []).find((x) => x.id === source))}
          intro={<>The same sync with <span className="font-mono">mongosh</span> and <span className="font-mono">kubectl</span>, filled in for {st.target} in namespace <span className="font-mono">{st.namespace}</span>. Replace each <span className="font-mono">&lt;password&gt;</span>.</>} />
      )}
    </div>
  )
}
