import { useEffect, useState } from 'react'
import { stackApi, frameApi } from '../lib/stackApi'
import { Badge, Button, ConfirmButton, Field, Toggle, inputCls } from '../components/ui'

// Designer forms for the non-Percona upstreams: MariaDB (standalone, replication,
// Galera) and MySQL Community (standalone, replication, InnoDB Cluster / GR).
//
// They live outside StackDesigner because all six share one non-trivial concern —
// picking an OS, a major series and a minor from a catalog whose coverage is uneven
// — and inlining that six more times is how the version pickers silently lost their
// minor lists once already. useUpstreamCatalog is the single implementation.

// useUpstreamCatalog drives the OS / OS-version / arch / major / minor pickers from
// a catalog endpoint, and keeps the object's current selection legal.
//
// The normalization effect is the point. Coverage genuinely differs across the
// image matrix (mariadb.org publishes no 10.6 for EL10 or Ubuntu noble; Oracle
// publishes no MySQL 8.0 for EL10), so switching OS can strand a selection on a
// series that has no packages. Rather than let that reach validation, each change
// re-derives the legal lists and snaps anything now-invalid to the first available
// value — except the minor, which falls back to '' meaning "newest available".
//
// `obj` is a node or a frame; majorKey/versionKey name its two version fields, so
// the same hook serves both shapes.
export function useUpstreamCatalog({ fetchCatalog, obj, majorKey, versionKey, patch, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    fetchCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  const imgs = cat || []
  const hasAny = (i) => Object.values(i.versions || {}).some((a) => a.length)
  const osFamilies = [...new Set(imgs.filter(hasAny).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === obj.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === obj.os && i.osVersion === obj.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === obj.os && i.osVersion === obj.osVersion && i.arch === obj.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[obj[majorKey]]) || []

  useEffect(() => {
    if (deployed || !imgs.length) return
    const p = {}
    const osVer = osVersions.includes(obj.osVersion) ? obj.osVersion : (osVersions[0] ?? obj.osVersion)
    if (osVer !== obj.osVersion) p.osVersion = osVer
    const archList = [...new Set(imgs.filter((i) => i.os === obj.os && i.osVersion === osVer).map((i) => i.arch))]
    const arch = archList.includes(obj.arch) ? obj.arch : (archList[0] ?? obj.arch)
    if (arch !== obj.arch) p.arch = arch
    const e2 = imgs.find((i) => i.os === obj.os && i.osVersion === osVer && i.arch === arch)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(obj[majorKey]) ? obj[majorKey] : (majorList[0] ?? obj[majorKey])
    if (major !== obj[majorKey]) p[majorKey] = major
    const minorList = (e2?.versions?.[major]) || []
    if (obj[versionKey] && !minorList.includes(obj[versionKey])) p[versionKey] = ''
    if (Object.keys(p).length) patch(p)
  }, [imgs, obj.id, obj.os, obj.osVersion, obj.arch, obj[majorKey], obj[versionKey], deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  return { imgs, osFamilies, osVersions, archs, majors, minors, loading: cat === null }
}

// VersionPickers renders the five linked selects every one of these forms needs.
function VersionPickers({ obj, patch, deployed, majorKey, versionKey, majorLabel, cat }) {
  const lock = deployed ? 'opacity-70' : ''
  const { osFamilies, osVersions, archs, majors, minors, loading } = cat
  return (
    <>
      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={obj.os} disabled={deployed} onChange={(e) => patch({ os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={obj.osVersion} disabled={deployed} onChange={(e) => patch({ osVersion: e.target.value })}>
            {osVersions.map((v) => <option key={v} value={v}>{v}</option>)}
          </select>
        </Field>
      </div>
      <div className="grid grid-cols-3 gap-2">
        <Field label="Arch">
          <select className={`${inputCls} ${lock}`} value={obj.arch} disabled={deployed} onChange={(e) => patch({ arch: e.target.value })}>
            {archs.map((a) => <option key={a} value={a}>{a}</option>)}
          </select>
        </Field>
        <Field label={majorLabel}>
          <select className={`${inputCls} ${lock}`} value={obj[majorKey]} disabled={deployed} onChange={(e) => patch({ [majorKey]: e.target.value, [versionKey]: '' })}>
            {majors.map((m) => <option key={m} value={m}>{m}</option>)}
          </select>
        </Field>
        <Field label="Version" hint={loading ? 'Loading…' : 'Blank = newest'}>
          <select className={`${inputCls} ${lock}`} value={obj[versionKey]} disabled={deployed} onChange={(e) => patch({ [versionKey]: e.target.value })}>
            <option value="">latest</option>
            {minors.map((v) => <option key={v} value={v}>{v}</option>)}
          </select>
        </Field>
      </div>
      {!loading && !majors.length && (
        <p className="text-xs text-amber-600 dark:text-amber-400">
          No packages for this OS/arch. Pick another combination, or run <code>make versions</code>.
        </p>
      )}
    </>
  )
}

// Shared tail: PMM, package proxy, TLS. Kept together because every form offers the
// same set and they behave identically across all six types.
function CommonOptions({ obj, patch, nodes, deployed, showGtid, showRepl }) {
  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  return (
    <>
      {showRepl && (
        <Field label="Replication mode" hint="Semi-sync makes the primary wait for one replica to acknowledge.">
          <select className={inputCls} value={obj.replMode || 'async'} onChange={(e) => patch({ replMode: e.target.value })}>
            <option value="async">async</option>
            <option value="semisync">semisync</option>
          </select>
        </Field>
      )}
      <Field label="Monitored by">
        <select className={inputCls} value={obj.pmmNodeId || ''} onChange={(e) => patch({ pmmNodeId: e.target.value })}>
          <option value="">not monitored</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>
      <div className="space-y-1.5">
        {showGtid && (
          <Toggle checked={obj.gtid !== false} onChange={(v) => patch({ gtid: v })} label="Enable GTID" />
        )}
        <Toggle checked={!!obj.useProxy} onChange={(v) => patch({ useProxy: v })} label="Install packages via the Intranet proxy" />
        <Toggle checked={!!obj.generateCert} onChange={(v) => patch({ generateCert: v })} label="Issue a TLS certificate from the Intranet CA" />
      </div>
      {obj.generateCert && (
        <div className="grid grid-cols-2 gap-2">
          <Field label="Certificate TTL">
            <input type="number" min="1" className={inputCls} value={obj.certTtlValue ?? 365} onChange={(e) => patch({ certTtlValue: Number(e.target.value) })} />
          </Field>
          <Field label="Unit">
            <select className={inputCls} value={obj.certTtlUnit || 'days'} onChange={(e) => patch({ certTtlUnit: e.target.value })}>
              <option value="days">days</option>
              <option value="months">months</option>
              <option value="years">years</option>
            </select>
          </Field>
        </div>
      )}
    </>
  )
}

// OrchestratorPicker links a replication cluster to an Orchestrator node, and once
// the cluster is running lets discovery be re-seeded without a redeploy.
//
// Offered on the two replication kinds only. Orchestrator manages classic
// source/replica topologies — it has nothing to fail over in a Galera or Group
// Replication cluster, which elect their own primary.
//
// It works for MariaDB unchanged: Orchestrator detects the flavour from the version
// banner and reads MariaDB's own GTID state, and the topology account the baseline
// creates is the same one the Percona clusters use.
function OrchestratorPicker({ frame: f, stackId, nodes, patch, running }) {
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState('')
  const [err, setErr] = useState('')
  const orchestratorNodes = nodes.filter((x) => x.type === 'orchestrator')
  return (
    <>
      <Field
        label="Monitored by (Orchestrator)"
        hint={running ? 'Pick an Orchestrator node (or none), then apply to the running cluster.' : 'Optional — seeds topology discovery on an Orchestrator node.'}
      >
        <select className={inputCls} value={f.orchestratorNodeId || ''} onChange={(e) => { patch({ orchestratorNodeId: e.target.value }); setMsg(''); setErr('') }}>
          <option value="">none</option>
          {orchestratorNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>
      {running && stackId && (
        <div className="space-y-1.5 rounded-lg border border-dashed p-2">
          <div className="text-xs text-muted">
            Seeds/refreshes topology discovery on the Orchestrator node now (clearing it just stops
            re-seeding — Orchestrator itself isn&apos;t asked to forget the cluster).
          </div>
          {err && <div className="rounded border border-danger/30 bg-danger/15 px-2 py-1 text-xs text-danger">{err}</div>}
          {msg && <div className="rounded border border-success/30 bg-success/15 px-2 py-1 text-xs text-success">{msg}</div>}
          <Button size="sm" className="w-full" disabled={busy}
            onClick={async () => {
              setBusy(true); setErr(''); setMsg('')
              try {
                const r = await frameApi(stackId, f.id).setOrchestrator(f.orchestratorNodeId || '')
                setMsg(f.orchestratorNodeId ? `Discovery seeded (${r.updated} node${r.updated === 1 ? '' : 's'}).` : `Link cleared (${r.updated} node${r.updated === 1 ? '' : 's'}).`)
              } catch (e) { setErr(e.message) } finally { setBusy(false) }
            }}>
            {busy ? 'Applying…' : (f.orchestratorNodeId ? 'Apply Orchestrator discovery' : 'Clear Orchestrator link')}
          </Button>
        </div>
      )}
    </>
  )
}

function ExportRow({ node: n, patchNode, deployed }) {
  return (
    <>
      <Toggle checked={!!n.exportEnabled} onChange={(v) => patchNode(n.id, { exportEnabled: v })} label="Publish port 3306 to the host" />
      {n.exportEnabled && (
        <Field label="Host port" hint={deployed ? 'Changing this needs a destroy + redeploy — published ports are fixed at container creation.' : '0 = pick a free port'}>
          <input type="number" min="0" className={inputCls} value={n.exportHostPort || 0} onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </>
  )
}

// ---------------------------------------------------------------- MariaDB

export function MariaDBNodeForm({ node: n, nodes, patchNode, deleteNode, deployed }) {
  const patch = (p) => patchNode(n.id, p)
  const cat = useUpstreamCatalog({
    fetchCatalog: stackApi.mariadbCatalog, obj: n,
    majorKey: 'mariadbMajor', versionKey: 'mariadbVersion', patch, deployed,
  })
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">MariaDB</span>
        <Badge tone="primary">standalone</Badge>
      </div>
      <Field label="Name"><input className={inputCls} value={n.label} onChange={(e) => patch({ label: e.target.value })} /></Field>
      <VersionPickers obj={n} patch={patch} deployed={deployed} majorKey="mariadbMajor" versionKey="mariadbVersion" majorLabel="MariaDB" cat={cat} />
      <CommonOptions obj={n} patch={patch} nodes={nodes} deployed={deployed} showGtid />
      <ExportRow node={n} patchNode={patchNode} deployed={deployed} />
      <ConfirmButton onConfirm={() => deleteNode(n.id)}>Delete node</ConfirmButton>
    </div>
  )
}

export function MariaDBFrameForm({ frame: f, stackId, nodes, patchFrame, deleteFrame, deployed, running }) {
  const patch = (p) => patchFrame(f.id, p)
  const cat = useUpstreamCatalog({
    fetchCatalog: stackApi.mariadbCatalog, obj: f,
    majorKey: 'mariadbMajor', versionKey: 'mariadbVersion', patch, deployed,
  })
  const members = nodes.filter((x) => x.frameId === f.id)
  const primaries = members.filter((x) => x.role === 'primary').length
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">MariaDB Replication</span>
        <Badge tone="primary">{members.length} node{members.length === 1 ? '' : 's'}</Badge>
      </div>
      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patch({ label: e.target.value })} />
      </Field>
      <VersionPickers obj={f} patch={patch} deployed={deployed} majorKey="mariadbMajor" versionKey="mariadbVersion" majorLabel="MariaDB" cat={cat} />
      <CommonOptions obj={f} patch={patch} nodes={nodes} deployed={deployed} showGtid showRepl />
      <OrchestratorPicker frame={f} stackId={stackId} nodes={nodes} patch={patch} running={running} />
      {/* MariaDB GTIDs are domain-server-seq; the domain is derived from the cluster
          name so every member shares it and two clusters never collide. */}
      {f.gtid !== false && (
        <p className="text-xs text-slate-500 dark:text-slate-400">
          GTID positions are <code>domain-server-seq</code>; the domain is derived from the cluster name.
        </p>
      )}
      {primaries !== 1 && (
        <p className="text-xs text-red-600 dark:text-red-400">Needs exactly one primary (has {primaries}).</p>
      )}
      <ConfirmButton onConfirm={() => deleteFrame(f.id)}>Delete cluster</ConfirmButton>
    </div>
  )
}

export function MariaDBGaleraFrameForm({ frame: f, nodes, patchFrame, deleteFrame, deployed }) {
  const patch = (p) => patchFrame(f.id, p)
  const cat = useUpstreamCatalog({
    fetchCatalog: stackApi.mariadbCatalog, obj: f,
    majorKey: 'mariadbMajor', versionKey: 'mariadbVersion', patch, deployed,
  })
  const members = nodes.filter((x) => x.frameId === f.id)
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">MariaDB Galera</span>
        <Badge tone="primary">{members.length} node{members.length === 1 ? '' : 's'}</Badge>
      </div>
      <Field label="Cluster name" hint="Also the wsrep_cluster_name.">
        <input className={inputCls} value={f.label} onChange={(e) => patch({ label: e.target.value })} />
      </Field>
      <VersionPickers obj={f} patch={patch} deployed={deployed} majorKey="mariadbMajor" versionKey="mariadbVersion" majorLabel="MariaDB" cat={cat} />
      <CommonOptions obj={f} patch={patch} nodes={nodes} deployed={deployed} />
      <p className="text-xs text-slate-500 dark:text-slate-400">
        State transfer uses <code>mariabackup</code>. The seed is bootstrapped first, then each
        joiner starts in turn — Galera services one state transfer per donor at a time.
      </p>
      {members.length < 3 && (
        <p className="text-xs text-red-600 dark:text-red-400">Needs at least 3 members (has {members.length}).</p>
      )}
      {members.length >= 3 && members.length % 2 === 0 && (
        <p className="text-xs text-amber-600 dark:text-amber-400">
          An even member count cannot break a tie — use an odd number.
        </p>
      )}
      <ConfirmButton onConfirm={() => deleteFrame(f.id)}>Delete cluster</ConfirmButton>
    </div>
  )
}

// ---------------------------------------------------------------- MySQL Community

export function MySQLCENodeForm({ node: n, nodes, patchNode, deleteNode, deployed }) {
  const patch = (p) => patchNode(n.id, p)
  const cat = useUpstreamCatalog({
    fetchCatalog: stackApi.mysqlceCatalog, obj: n,
    majorKey: 'mysqlceMajor', versionKey: 'mysqlceVersion', patch, deployed,
  })
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">MySQL Community</span>
        <Badge tone="primary">standalone</Badge>
      </div>
      <Field label="Name"><input className={inputCls} value={n.label} onChange={(e) => patch({ label: e.target.value })} /></Field>
      <VersionPickers obj={n} patch={patch} deployed={deployed} majorKey="mysqlceMajor" versionKey="mysqlceVersion" majorLabel="MySQL" cat={cat} />
      <CommonOptions obj={n} patch={patch} nodes={nodes} deployed={deployed} showGtid />
      <ExportRow node={n} patchNode={patchNode} deployed={deployed} />
      <ConfirmButton onConfirm={() => deleteNode(n.id)}>Delete node</ConfirmButton>
    </div>
  )
}

export function MySQLCEFrameForm({ frame: f, stackId, nodes, patchFrame, deleteFrame, deployed, running }) {
  const patch = (p) => patchFrame(f.id, p)
  const cat = useUpstreamCatalog({
    fetchCatalog: stackApi.mysqlceCatalog, obj: f,
    majorKey: 'mysqlceMajor', versionKey: 'mysqlceVersion', patch, deployed,
  })
  const members = nodes.filter((x) => x.frameId === f.id)
  const primaries = members.filter((x) => x.role === 'primary').length
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">MySQL Replication</span>
        <Badge tone="primary">{members.length} node{members.length === 1 ? '' : 's'}</Badge>
      </div>
      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patch({ label: e.target.value })} />
      </Field>
      <VersionPickers obj={f} patch={patch} deployed={deployed} majorKey="mysqlceMajor" versionKey="mysqlceVersion" majorLabel="MySQL" cat={cat} />
      <CommonOptions obj={f} patch={patch} nodes={nodes} deployed={deployed} showGtid showRepl />
      <OrchestratorPicker frame={f} stackId={stackId} nodes={nodes} patch={patch} running={running} />
      {primaries !== 1 && (
        <p className="text-xs text-red-600 dark:text-red-400">Needs exactly one primary (has {primaries}).</p>
      )}
      <ConfirmButton onConfirm={() => deleteFrame(f.id)}>Delete cluster</ConfirmButton>
    </div>
  )
}

export function MySQLCEInnoDBFrameForm({ frame: f, nodes, patchFrame, deleteFrame, deployed }) {
  const patch = (p) => patchFrame(f.id, p)
  const cat = useUpstreamCatalog({
    fetchCatalog: stackApi.mysqlceCatalog, obj: f,
    majorKey: 'mysqlceMajor', versionKey: 'mysqlceVersion', patch, deployed,
  })
  const members = nodes.filter((x) => x.frameId === f.id)
  const mode = f.replMode || 'innodbcluster'
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">MySQL InnoDB Cluster / GR</span>
        <Badge tone="primary">{members.length} node{members.length === 1 ? '' : 's'}</Badge>
      </div>
      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patch({ label: e.target.value })} />
      </Field>
      <VersionPickers obj={f} patch={patch} deployed={deployed} majorKey="mysqlceMajor" versionKey="mysqlceVersion" majorLabel="MySQL" cat={cat} />
      <Field label="Mode" hint="InnoDB Cluster adds MySQL Shell orchestration and cluster metadata on top of Group Replication.">
        <select className={inputCls} value={mode} disabled={deployed} onChange={(e) => patch({ replMode: e.target.value })}>
          <option value="innodbcluster">InnoDB Cluster (MySQL Shell)</option>
          <option value="groupreplication">Group Replication (raw)</option>
        </select>
      </Field>
      <Toggle checked={!!f.mysqlRouter} onChange={(v) => patch({ mysqlRouter: v })} label="Install MySQL Router on each member" />
      <CommonOptions obj={f} patch={patch} nodes={nodes} deployed={deployed} />
      {members.length < 3 && (
        <p className="text-xs text-red-600 dark:text-red-400">Needs at least 3 members (has {members.length}).</p>
      )}
      {members.length >= 3 && members.length % 2 === 0 && (
        <p className="text-xs text-amber-600 dark:text-amber-400">
          Group Replication needs an odd member count to reach quorum.
        </p>
      )}
      <ConfirmButton onConfirm={() => deleteFrame(f.id)}>Delete cluster</ConfirmButton>
    </div>
  )
}

// ---------------------------------------------------------------- members

// UpstreamMemberForm edits one member of any of the four cluster types. Members
// carry only their own identity and export setting; everything else is the frame's.
export function UpstreamMemberForm({ node: n, frame, patchNode, deleteNode, deployed, roles }) {
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">{frame?.label || 'Cluster'} member</span>
        {n.role ? <Badge tone={n.role === 'primary' ? 'primary' : 'muted'}>{n.role}</Badge> : null}
      </div>
      <Field label="Name"><input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} /></Field>
      {roles && (
        <Field label="Role" hint="Exactly one primary per cluster.">
          <select className={inputCls} value={n.role || 'secondary'} disabled={deployed} onChange={(e) => patchNode(n.id, { role: e.target.value })}>
            <option value="primary">primary</option>
            <option value="secondary">secondary</option>
          </select>
        </Field>
      )}
      <ExportRow node={n} patchNode={patchNode} deployed={deployed} />
      <ConfirmButton onConfirm={() => deleteNode(n.id)}>Delete node</ConfirmButton>
    </div>
  )
}
