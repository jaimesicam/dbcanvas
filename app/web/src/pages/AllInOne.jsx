import { useCallback, useEffect, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Button, Badge, Field, inputCls } from '../components/ui.jsx'
import { stackApi, aioApi, DEPLOY_TONE } from '../lib/stackApi.js'
import {
  AIO_KINDS, FAMILY_LABEL, kindOf, familyOf, memberCount, planMembers, portList,
  PORT_ROLE, estMemMB, mysqlFlavor, addBlockedReason, nextInstanceName, sanitizeInst,
} from '../lib/aioPorts.js'

// AllInOne.jsx — the All-in-One node's designer form and its deployed manager.
//
// An All-in-One node is one container running many database instances. That
// makes its form different in kind from every other node's: instead of one
// product's options, it edits a *list* of feature instances, each with its own
// options and its own slice of the node's port space.
//
// Two things the form must make impossible to get wrong, because both fail
// confusingly at deploy time otherwise:
//
//   - the MySQL flavor conflict (PXC cannot share a container with Percona
//     Server), which appears as a disabled, annotated entry in the Add menu, and
//   - which ports an instance will actually use, which is shown per instance
//     rather than left to be discovered after the fact.
//
// Instances live on the node as `aioInstances` (see aioInstance in app/aio.go).

const uid = () => Math.random().toString(36).slice(2, 10)

// Family accent colours, matched to the equivalent standalone node types on the
// canvas so an instance card reads as "the same thing, inside this node".
const FAMILY_COLOR = {
  mysql: '#2563eb', postgres: '#336791', mongodb: '#10b981',
  valkey: '#7c3aed', proxysql: '#f59e0b', haproxy: '#22c55e', orchestrator: '#f97316',
}

// ---------------------------------------------------------------- the form

export function AllInOneForm({ node: n, nodes, patchNode, deleteNode, dep, deployed }) {
  const instances = n.aioInstances || []
  const [cat, setCat] = useState(null)
  const [openId, setOpenId] = useState(null)
  const [adding, setAdding] = useState(false)

  useEffect(() => {
    let alive = true
    stackApi.imagesCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => {})
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const setInstances = useCallback((next) => patchNode(n.id, { aioInstances: next }), [n.id, patchNode])
  const patchInstance = useCallback((id, patch) => {
    setInstances(instances.map((i) => (i.id === id ? { ...i, ...patch } : i)))
  }, [instances, setInstances])
  const removeInstance = (id) => setInstances(instances.filter((i) => i.id !== id))

  const addInstance = (kind) => {
    const k = kindOf(kind)
    const inst = {
      id: uid(),
      kind,
      name: nextInstanceName(instances, kind),
      members: k.cluster ? k.def : 1,
      gtid: true,
      replMode: kind === 'innodb' ? 'groupreplication' : 'async',
      certTtlValue: 365,
      certTtlUnit: 'days',
    }
    setInstances([...instances, inst])
    setOpenId(inst.id)
    setAdding(false)
  }

  const flavor = mysqlFlavor(instances)
  const members = planMembers(instances)
  const est = estMemMB(instances)

  const osFamilies = [...new Set(imgs.map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === n.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === n.os && i.osVersion === n.osVersion).map((i) => i.arch))]

  // Snap dependent selects once the catalog loads (same pattern as LinuxClientForm).
  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(n.osVersion) ? n.osVersion : (osVersions[0] ?? n.osVersion)
    if (osVer !== n.osVersion) patch.osVersion = osVer
    const archList = [...new Set(imgs.filter((i) => i.os === n.os && i.osVersion === osVer).map((i) => i.arch))]
    const arch = archList.includes(n.arch) ? n.arch : (archList[0] ?? n.arch)
    if (arch !== n.arch) patch.arch = arch
    if (Object.keys(patch).length) patchNode(n.id, patch)
  }, [imgs, n.id, n.os, n.osVersion, n.arch, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const familiesUsed = [...new Set(instances.map((i) => familyOf(i.kind)))].filter(Boolean)

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">All in One</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        One container running many database instances side by side. Nothing uses its product's
        default port — each instance gets its own port slot, its own datadir and its own systemd
        unit, controlled with <code className="font-mono">aioctl</code> in the node's terminal.
      </p>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={n.os} disabled={deployed} onChange={(e) => patchNode(n.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={n.osVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>
      <Field label="Platform / arch">
        <select className={`${inputCls} ${lock}`} value={n.arch} disabled={deployed} onChange={(e) => patchNode(n.id, { arch: e.target.value })}>
          {archs.map((o) => <option key={o} value={o}>{o}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>

      {/* Per-family versions. One package install serves every instance of a
          family, so these are node-level — the hint says so where it matters. */}
      {familiesUsed.length > 0 && (
        <div className="space-y-2 rounded-lg border border-border bg-surface2 px-3 py-2">
          <div className="text-xs font-semibold">Versions</div>
          {familiesUsed.includes('mysql') && flavor.flavor === 'ps' && (
            <Field label="Percona Server" hint={`Applies to all ${instances.filter((i) => familyOf(i.kind) === 'mysql').length} MySQL instance(s) — one install per container.`}>
              <select className={`${inputCls} ${lock}`} value={n.aioPsMajor || '8.0'} disabled={deployed}
                onChange={(e) => patchNode(n.id, { aioPsMajor: e.target.value })}>
                <option value="8.0">8.0</option>
                <option value="8.4">8.4</option>
              </select>
            </Field>
          )}
          {familiesUsed.includes('mysql') && flavor.flavor === 'pxc' && (
            <Field label="Percona XtraDB Cluster" hint="Applies to every PXC cluster in this node — one install per container.">
              <select className={`${inputCls} ${lock}`} value={n.aioPxcMajor || '8.0'} disabled={deployed}
                onChange={(e) => patchNode(n.id, { aioPxcMajor: e.target.value })}>
                <option value="8.0">8.0</option>
                <option value="8.4">8.4</option>
              </select>
            </Field>
          )}
        </div>
      )}

      {/* The flavor conflict, if a saved design somehow contains one. */}
      {flavor.conflict && (
        <div className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-xs leading-snug text-danger">
          <strong>PXC and Percona Server can't share a container.</strong> This node declares
          PXC ({flavor.pxc.join(', ')}) and Percona Server ({flavor.ps.join(', ')}).
          <code className="font-mono"> percona-xtradb-cluster-server</code> conflicts with
          <code className="font-mono"> percona-server-server</code> at the package level, so the
          deploy is blocked until one set is removed.
        </div>
      )}

      {/* Instance list */}
      <div className="flex items-center justify-between pt-1">
        <span className="text-xs font-semibold">
          Features {instances.length > 0 && <span className="font-normal text-muted">· {members.length} daemon(s)</span>}
        </span>
        {!deployed && (
          <Button size="sm" variant="secondary" onClick={() => setAdding((v) => !v)}>
            <Icon.Plus size={14} /> Add feature
          </Button>
        )}
      </div>

      {adding && <AddFeatureMenu instances={instances} onPick={addInstance} onClose={() => setAdding(false)} />}

      {instances.length === 0 && !adding && (
        <div className="rounded-lg border border-dashed border-border px-3 py-4 text-center text-xs text-muted">
          No features yet — add one to give this container something to run.
        </div>
      )}

      <div className="space-y-2">
        {instances.map((inst) => (
          <InstanceCard
            key={inst.id} inst={inst} node={n} nodes={nodes} instances={instances}
            open={openId === inst.id} onToggle={() => setOpenId(openId === inst.id ? null : inst.id)}
            patch={(p) => patchInstance(inst.id, p)} onRemove={() => removeInstance(inst.id)}
            deployed={deployed}
          />
        ))}
      </div>

      {est > 0 && (
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          Rough footprint: <strong>{(est / 1024).toFixed(1)} GiB</strong> across {members.length} daemon(s).
          {n.memoryGb > 0 && est > n.memoryGb * 1024 && (
            <span className="text-warning"> Exceeds this node's {n.memoryGb} GiB limit.</span>
          )}
        </div>
      )}

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// AddFeatureMenu lists every kind grouped by family. Entries that cannot be
// added are shown DISABLED with the reason inline rather than hidden — a missing
// menu item reads as a bug, an explained one teaches the constraint.
function AddFeatureMenu({ instances, onPick, onClose }) {
  const families = [...new Set(AIO_KINDS.map((k) => k.family))]
  return (
    <div className="space-y-2 rounded-lg border border-border bg-surface2 p-2">
      <div className="flex items-center justify-between px-1">
        <span className="text-[11px] font-semibold uppercase tracking-wide text-muted">Add a feature</span>
        <button onClick={onClose} className="px-1 text-xs text-muted hover:text-fg">✕</button>
      </div>
      {families.map((fam) => (
        <div key={fam}>
          <div className="px-1 pb-1 text-[10px] font-semibold uppercase tracking-wide text-muted">{FAMILY_LABEL[fam]}</div>
          <div className="space-y-1">
            {AIO_KINDS.filter((k) => k.family === fam).map((k) => {
              const reason = addBlockedReason(k.kind, instances)
              return (
                <button
                  key={k.kind} disabled={!!reason} title={reason || `Add a ${k.label} instance`}
                  onClick={() => onPick(k.kind)}
                  className="flex w-full items-start gap-2 rounded-md px-2 py-1 text-left text-xs hover:bg-surface disabled:cursor-not-allowed disabled:opacity-50">
                  <span className="mt-0.5 h-2 w-2 shrink-0 rounded-full" style={{ backgroundColor: FAMILY_COLOR[fam] }} />
                  <span className="min-w-0">
                    <span className="font-medium">{k.label}</span>
                    {reason && <span className="block text-[10px] leading-snug text-muted">{reason}</span>}
                  </span>
                </button>
              )
            })}
          </div>
        </div>
      ))}
    </div>
  )
}

// InstanceCard edits one feature instance: its name, member count, kind-specific
// options, the drop-downs that replace association lines, and a read-only view
// of the ports it will occupy.
function InstanceCard({ inst, node, nodes, instances, open, onToggle, patch, onRemove, deployed }) {
  const k = kindOf(inst.kind)
  if (!k) return null
  const fam = k.family
  const color = FAMILY_COLOR[fam]
  const lock = deployed ? 'opacity-70' : ''
  const total = memberCount(inst.kind, inst.members)

  // This instance's own members, recomputed from the whole list so the preview
  // matches exactly what the backend planner will produce.
  const mine = planMembers(instances).filter((m) =>
    m.group === sanitizeInst(inst.name) || m.inst === sanitizeInst(inst.name))

  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  const baoNodes = nodes.filter((x) => x.type === 'openbao')
  const dirNodes = nodes.filter((x) => x.type === 'intranet' || x.type === 'sambaad')
  const seaweedNodes = nodes.filter((x) => x.type === 'seaweedfs')
  const orchNodes = nodes.filter((x) => x.type === 'orchestrator')
  const orchInstances = instances.filter((i) => i.kind === 'orchestrator')
  const backendCandidates = instances.filter((i) =>
    i.id !== inst.id && (inst.kind === 'proxysql' ? familyOf(i.kind) === 'mysql'
      : ['mysql', 'postgres'].includes(familyOf(i.kind))))

  const isMySQL = fam === 'mysql'
  const isProxy = fam === 'proxysql' || fam === 'haproxy'

  return (
    <div className="overflow-hidden rounded-lg border border-border">
      <button onClick={onToggle}
        className="flex w-full items-center gap-2 bg-surface2 px-2 py-1.5 text-left text-xs hover:bg-surface">
        <span className="h-2 w-2 shrink-0 rounded-full" style={{ backgroundColor: color }} />
        <span className="min-w-0 flex-1 truncate">
          <span className="font-medium">{inst.name}</span>
          <span className="text-muted"> · {k.label}{k.cluster ? ` · ${total} members` : ''}</span>
        </span>
        <span className="shrink-0 font-mono text-[10px] text-muted">
          {mine.length ? `${mine[0].ports.client}${mine.length > 1 ? `–${mine[mine.length - 1].ports.client}` : ''}` : ''}
        </span>
        <Icon.Chevron size={13} className={open ? 'transition' : '-rotate-90 transition'} />
      </button>

      {open && (
        <div className="space-y-2 px-2 py-2">
          <Field label="Name" hint="Becomes the instance's hostname, directory and systemd unit.">
            <input className={`${inputCls} ${lock}`} value={inst.name} disabled={deployed}
              onChange={(e) => patch({ name: e.target.value })} />
          </Field>

          {k.cluster && (
            <Field label="Members" hint={`${k.min}–${k.max}${k.odd ? ', odd only (quorum)' : ''}`}>
              <input type="number" min={k.min} max={k.max} className={`${inputCls} ${lock}`}
                value={inst.members ?? k.def} disabled={deployed}
                onChange={(e) => patch({ members: parseInt(e.target.value, 10) || k.def })} />
            </Field>
          )}

          {isMySQL && inst.kind === 'psrepl' && (
            <Field label="Replication mode">
              <select className={`${inputCls} ${lock}`} value={inst.replMode || 'async'} disabled={deployed}
                onChange={(e) => patch({ replMode: e.target.value })}>
                <option value="async">Asynchronous</option>
                <option value="semisync">Semi-synchronous</option>
              </select>
            </Field>
          )}
          {inst.kind === 'innodb' && (
            <Field label="Topology"
              hint="InnoDB Cluster proper needs MySQL Shell, which All-in-One does not install yet.">
              <select className={`${inputCls} ${lock}`} value={inst.replMode || 'groupreplication'} disabled={deployed}
                onChange={(e) => patch({ replMode: e.target.value })}>
                <option value="groupreplication">Group Replication</option>
                <option value="innodbcluster" disabled>InnoDB Cluster (needs MySQL Shell)</option>
              </select>
            </Field>
          )}
          {isMySQL && inst.kind === 'ps' && (
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" checked={!!inst.gtid} disabled={deployed}
                onChange={(e) => patch({ gtid: e.target.checked })} />
              <span>Enable GTID</span>
            </label>
          )}

          {isProxy && (
            <Field label="Fronts" hint="Which instance in this node this proxy load-balances.">
              <select className={`${inputCls} ${lock}`} value={inst.backendInstanceId || ''} disabled={deployed}
                onChange={(e) => patch({ backendInstanceId: e.target.value })}>
                <option value="">— select a backend —</option>
                {backendCandidates.map((b) => <option key={b.id} value={b.id}>{b.name} ({kindOf(b.kind)?.label})</option>)}
              </select>
            </Field>
          )}

          {/* Drop-down wiring — an All-in-One node draws no association lines. */}
          <Field label="Monitored by (PMM)">
            <select className={`${inputCls} ${lock}`} value={inst.pmmNodeId || ''} disabled={deployed}
              onChange={(e) => patch({ pmmNodeId: e.target.value })}>
              <option value="">— none —</option>
              {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
            </select>
          </Field>

          {isMySQL && (
            <Field label="Monitored by (Orchestrator)">
              <select className={`${inputCls} ${lock}`} value={inst.orchestratorRef || ''} disabled={deployed}
                onChange={(e) => patch({ orchestratorRef: e.target.value })}>
                <option value="">— none —</option>
                {orchInstances.map((o) => <option key={o.id} value={`inst:${o.id}`}>{o.name} (this node)</option>)}
                {orchNodes.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
              </select>
            </Field>
          )}

          {/* Directory auth, OpenBao encryption, SeaweedFS backups and per-instance
              TLS are deliberately NOT offered yet: no provisioner reads them, and a
              control that silently does nothing is worse than an absent one.
              validateStack rejects them too, so a design saved elsewhere cannot
              sneak them past. They return here with their wiring. */}
          {/* TLS is wired for the three database engines. Directory auth, OpenBao
              encryption and SeaweedFS backups are deliberately NOT offered: no
              provisioner reads them, and a control that silently does nothing is
              worse than an absent one. validateStack rejects them too. */}
          {['mysql', 'postgres', 'mongodb'].includes(fam) && (
            <>
              <label className="flex items-center gap-2 text-sm">
                <input type="checkbox" checked={!!inst.generateCert} disabled={deployed}
                  onChange={(e) => patch({ generateCert: e.target.checked })} />
                <span>Issue a TLS certificate from the Intranet CA</span>
              </label>
              {inst.generateCert && (
                <div className="grid grid-cols-2 gap-2">
                  <Field label="Certificate lifetime">
                    <input type="number" min={1} className={`${inputCls} ${lock}`} disabled={deployed}
                      value={inst.certTtlValue || 365}
                      onChange={(e) => patch({ certTtlValue: parseInt(e.target.value, 10) || 365 })} />
                  </Field>
                  <Field label="Unit">
                    <select className={`${inputCls} ${lock}`} value={inst.certTtlUnit || 'days'} disabled={deployed}
                      onChange={(e) => patch({ certTtlUnit: e.target.value })}>
                      <option value="minutes">minutes</option>
                      <option value="hours">hours</option>
                      <option value="days">days</option>
                    </select>
                  </Field>
                </div>
              )}
            </>
          )}

          <div className="rounded-md bg-surface2 px-2 py-1.5 text-[10px] leading-snug text-muted">
            Directory auth, OpenBao encryption and SeaweedFS backups aren&apos;t available on
            All-in-One instances yet — use a dedicated node for those.
          </div>

          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={!!inst.exportEnabled} disabled={deployed}
              onChange={(e) => patch({ exportEnabled: e.target.checked })} />
            <span>Publish client port to the host</span>
          </label>
          {inst.exportEnabled && (
            <Field label="Host port" hint="0 = pick a free one automatically.">
              <input type="number" className={`${inputCls} ${lock}`} value={inst.exportHostPort || 0} disabled={deployed}
                onChange={(e) => patch({ exportHostPort: parseInt(e.target.value, 10) || 0 })} />
            </Field>
          )}

          {/* Ports are computed, never entered — the point of the node type is
              that none of them is a product default. */}
          <div className="rounded-md bg-surface2 px-2 py-1.5">
            <div className="pb-1 text-[10px] font-semibold uppercase tracking-wide text-muted">Ports (assigned)</div>
            <div className="space-y-0.5">
              {mine.map((m) => (
                <div key={m.inst} className="flex justify-between gap-2 font-mono text-[10px]">
                  <span className="truncate text-muted">{m.inst}</span>
                  <span>{portList(m.ports).join(', ')}</span>
                </div>
              ))}
            </div>
          </div>

          {!deployed && (
            <Button variant="danger" size="sm" className="w-full" onClick={onRemove}>
              <Icon.Trash size={14} /> Remove {inst.name}
            </Button>
          )}
        </div>
      )}
    </div>
  )
}

// ---------------------------------------------------------------- the manager

// AllInOneManager is the deployed node's console: the `aioctl list` table with
// per-row and per-group lifecycle controls. Every button calls the API, which
// execs aioctl — so the UI and the terminal do the same thing, including the
// cluster start ordering.
export function AllInOneManager({ stackId, nodeId, dep, onDeleteNode }) {
  const api = aioApi(stackId, nodeId)
  const [data, setData] = useState(null)
  const [busy, setBusy] = useState('')
  const [err, setErr] = useState('')
  const [logs, setLogs] = useState(null)

  const load = useCallback(() => {
    api.instances().then(setData).catch((e) => setErr(e.message))
  }, [stackId, nodeId]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    load()
    const t = setInterval(load, 5000)
    return () => clearInterval(t)
  }, [load])

  const act = async (action, sel) => {
    setBusy(`${action}:${sel}`); setErr('')
    try { await api[action](sel); await load() } catch (e) { setErr(e.message) } finally { setBusy('') }
  }
  const showLogs = async (inst) => {
    setBusy(`logs:${inst}`); setErr('')
    try { const r = await api.logs(inst); setLogs({ inst, text: r.logs }) } catch (e) { setErr(e.message) } finally { setBusy('') }
  }

  const rows = data?.instances || []
  const groups = [...new Set(rows.map((r) => r.group).filter(Boolean))]
  const tone = (s) => (s === 'active' ? 'success' : s === 'failed' ? 'danger' : 'muted')

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">All in One</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <p className="text-xs text-muted">
        {rows.length} instance(s) in one container. The same operations are available as
        <code className="font-mono"> aioctl</code> in this node's terminal.
      </p>

      {err && <div className="rounded-md border border-danger/40 bg-danger/10 px-2 py-1.5 text-xs text-danger">{err}</div>}

      <div className="flex gap-1">
        <Button size="sm" variant="secondary" disabled={!!busy} onClick={() => act('start', 'all')}>Start all</Button>
        <Button size="sm" variant="secondary" disabled={!!busy} onClick={() => act('stop', 'all')}>Stop all</Button>
        <Button size="sm" variant="secondary" disabled={!!busy} onClick={load}>Refresh</Button>
      </div>

      {groups.map((g) => (
        <div key={g} className="flex items-center gap-2 rounded-md bg-surface2 px-2 py-1 text-xs">
          <span className="flex-1 truncate font-medium">{g}</span>
          <button className="text-[11px] text-primary hover:underline" disabled={!!busy} onClick={() => act('start', g)}>start</button>
          <button className="text-[11px] text-primary hover:underline" disabled={!!busy} onClick={() => act('stop', g)}>stop</button>
          <button className="text-[11px] text-primary hover:underline" disabled={!!busy} onClick={() => act('restart', g)}>restart</button>
        </div>
      ))}

      <div className="space-y-1">
        {rows.map((r) => (
          <div key={r.inst} className="rounded-md border border-border px-2 py-1.5">
            <div className="flex items-center gap-2">
              <span className="h-2 w-2 shrink-0 rounded-full" style={{ backgroundColor: FAMILY_COLOR[r.family] }} />
              <span className="min-w-0 flex-1 truncate text-xs font-medium">{r.inst}</span>
              <Badge tone={tone(r.state)}>{r.state}</Badge>
            </div>
            <div className="flex items-center gap-2 pt-1 text-[10px] text-muted">
              <span className="font-mono">:{r.ports?.client}</span>
              <span>{kindOf(r.kind)?.label || r.kind}</span>
              {r.role && r.role !== 'standalone' && <span>· {r.role}</span>}
              {r.export > 0 && <span>· host :{r.export}</span>}
              <span className="flex-1" />
              <button className="text-primary hover:underline" disabled={!!busy} onClick={() => act('start', r.inst)}>start</button>
              <button className="text-primary hover:underline" disabled={!!busy} onClick={() => act('stop', r.inst)}>stop</button>
              <button className="text-primary hover:underline" disabled={!!busy} onClick={() => act('restart', r.inst)}>restart</button>
              <button className="text-primary hover:underline" disabled={!!busy} onClick={() => showLogs(r.inst)}>logs</button>
            </div>
          </div>
        ))}
      </div>

      {logs && (
        <div className="space-y-1">
          <div className="flex items-center justify-between">
            <span className="text-xs font-semibold">{logs.inst} — last 200 lines</span>
            <button onClick={() => setLogs(null)} className="px-1 text-xs text-muted hover:text-fg">✕</button>
          </div>
          <pre className="max-h-64 overflow-auto rounded-md bg-surface2 p-2 font-mono text-[10px] leading-snug">{logs.text || '(empty)'}</pre>
        </div>
      )}

      {/* Ports matter more here than on any other node type: nothing is on a
          default, so this table is how anyone connects. */}
      <div className="rounded-lg bg-surface2 px-2 py-1.5">
        <div className="pb-1 text-[10px] font-semibold uppercase tracking-wide text-muted">Port map</div>
        {rows.map((r) => (
          <div key={r.inst} className="flex justify-between gap-2 font-mono text-[10px]">
            <span className="truncate text-muted">{r.inst}</span>
            <span>{Object.entries(r.ports || {}).filter(([kk, v]) => v && kk !== 'base')
              .map(([kk, v]) => `${PORT_ROLE[kk] || kk}:${v}`).join('  ')}</span>
          </div>
        ))}
      </div>

      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}
