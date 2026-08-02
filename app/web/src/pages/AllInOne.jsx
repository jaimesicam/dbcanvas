import { useCallback, useEffect, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Button, Badge, Field, inputCls } from '../components/ui.jsx'
import { stackApi, aioApi, DEPLOY_TONE } from '../lib/stackApi.js'
import { useTerminals } from '../terminal/TerminalProvider.jsx'
import SecretRow, { CopyButton as CopyBtn } from '../components/Secret.jsx'
import {
  AIO_KINDS, FAMILY_LABEL, kindOf, familyOf, memberCount, planMembers, portList,
  PORT_ROLE, estMemMB, mysqlFlavor, FLAVOR_LABEL, addBlockedReason, nextInstanceName, sanitizeInst,
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
      // The Intranet is mandatory and always provisions an admin@<domain> mailbox,
      // so an Orchestrator has somewhere to send failure alerts out of the box. The
      // bare local part is stored deliberately: alertEmailAddress qualifies it with
      // the stack's own domain, so this keeps working if DOMAIN changes. Clear it to
      // turn alerts off.
      ...(kind === 'orchestrator' ? { alertEmail: ORCH_DEFAULT_ALERT } : {}),
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
          family, so these are node-level — except PostgreSQL, whose packages are
          per-major and co-install, so its major lives on each instance. Minors
          come from the same `make versions` catalog the classic node forms use;
          blank means "the newest the catalog has". */}
      {familiesUsed.length > 0 && (
        <div className="space-y-2 rounded-lg border border-border bg-surface2 px-3 py-2">
          <div className="text-xs font-semibold">Versions</div>
          {familiesUsed.includes('mysql') && flavor.flavor === 'ps' && (
            <VersionPicker
              label="Percona Server" catalog="ps" node={n} patchNode={patchNode} deployed={deployed}
              majorKey="aioPsMajor" minorKey="aioPsVersion"
              hint={`Applies to all ${instances.filter((i) => familyOf(i.kind) === 'mysql').length} MySQL instance(s) — one install per container.`} />
          )}
          {familiesUsed.includes('mysql') && flavor.flavor === 'pxc' && (
            <VersionPicker
              label="Percona XtraDB Cluster" catalog="pxc" node={n} patchNode={patchNode} deployed={deployed}
              majorKey="aioPxcMajor" minorKey="aioPxcVersion"
              hint="Applies to every PXC cluster in this node — one install per container." />
          )}
          {familiesUsed.includes('mysql') && flavor.flavor === 'mariadb' && (
            <VersionPicker
              label="MariaDB" catalog="mariadb" node={n} patchNode={patchNode} deployed={deployed}
              majorKey="aioMariadbMajor" minorKey="aioMariadbVersion"
              hint="Applies to every MariaDB instance in this node — one install per container." />
          )}
          {familiesUsed.includes('mysql') && flavor.flavor === 'mysqlce' && (
            <VersionPicker
              label="MySQL Community" catalog="mysqlce" node={n} patchNode={patchNode} deployed={deployed}
              majorKey="aioMysqlceMajor" minorKey="aioMysqlceVersion"
              hint="Applies to every MySQL Community instance in this node — one install per container." />
          )}
          {familiesUsed.includes('mongodb') && (
            <VersionPicker
              label="PS MongoDB" catalog="psmdb" node={n} patchNode={patchNode} deployed={deployed}
              majorKey="aioPsmdbMajor" minorKey="aioPsmdbVersion"
              hint="One install serves the standalone, replica-set and sharded instances alike." />
          )}
          {familiesUsed.includes('valkey') && (
            <VersionPicker
              label="Valkey" catalog="valkey" node={n} patchNode={patchNode} deployed={deployed}
              majorKey="aioValkeyMajor" minorKey="aioValkeyVersion" />
          )}
          {familiesUsed.includes('proxysql') && (
            <VersionPicker
              label="ProxySQL" catalog="proxysql" node={n} patchNode={patchNode} deployed={deployed}
              majorKey="aioProxysqlMajor" minorKey="aioProxysqlVersion" />
          )}
          {familiesUsed.includes('orchestrator') && (
            <VersionPicker
              label="Orchestrator" catalog="orchestrator" node={n} patchNode={patchNode} deployed={deployed}
              minorKey="aioOrchestratorVersion" />
          )}
          {familiesUsed.includes('postgres') && (
            <div className="text-[10px] leading-snug text-muted">
              PostgreSQL packages are per-major and install side by side, so each PostgreSQL
              instance carries its own major — set it on the instance below.
            </div>
          )}
        </div>
      )}

      {/* The flavor conflict, if a saved design somehow contains one. */}
      {flavor.conflict && (
        <div className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-xs leading-snug text-danger">
          <strong>Only one MySQL flavor can share a container.</strong> This node declares{' '}
          {Object.keys(flavor.byFlavor).map((f) => `${FLAVOR_LABEL[f]} (${flavor.byFlavor[f].join(', ')})`).join(' and ')}.
          Each of these server packages provides <code className="font-mono">mysql-server</code> and
          conflicts with the others at the package level, so the deploy is blocked until all but one
          set is removed.
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

// VersionPicker is a catalog-driven major + minor pair for one family.
//
// The minors come from the same `make versions` catalog every classic node form
// reads, keyed by the node's OS/version/arch — so the list only ever offers what
// is actually installable on this image. Blank minor means "newest available",
// which is what the provisioners pass as an empty $VER.
//
// Omit majorKey for a product with no major series of its own (Orchestrator):
// the minor list is then flattened across whatever the catalog reports.
function VersionPicker({ label, catalog, node: n, patchNode, deployed, majorKey, minorKey, hint }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    const fn = stackApi[`${catalog}Catalog`]
    if (typeof fn !== 'function') return undefined
    fn().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep whatever is set */ })
    return () => { alive = false }
  }, [catalog])

  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''
  const entry = imgs.find((i) => i.os === n.os && i.osVersion === n.osVersion && i.arch === n.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const major = majorKey ? (n[majorKey] || majors[0] || '') : ''
  const minors = majorKey
    ? (entry?.versions?.[major] || [])
    : Object.values(entry?.versions || {}).flat()

  // Snap a stale selection once the catalog loads, the same way the classic
  // forms do — an OS change can invalidate both the major and the minor.
  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    if (majorKey && majors.length && !majors.includes(n[majorKey])) patch[majorKey] = majors[0]
    const list = majorKey
      ? (entry?.versions?.[patch[majorKey] || major] || [])
      : Object.values(entry?.versions || {}).flat()
    if (n[minorKey] && !list.includes(n[minorKey])) patch[minorKey] = ''
    if (Object.keys(patch).length) patchNode(n.id, patch)
  }, [imgs, n.id, n.os, n.osVersion, n.arch, n[majorKey], n[minorKey], deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const unavailable = imgs.length > 0 && majors.length === 0 && minors.length === 0

  return (
    <div className="space-y-1">
      <div className="grid grid-cols-2 gap-2">
        {majorKey && (
          <Field label={label}>
            <select className={`${inputCls} ${lock}`} value={major} disabled={deployed || !majors.length}
              onChange={(e) => patchNode(n.id, { [majorKey]: e.target.value, [minorKey]: '' })}>
              {majors.length === 0 && <option value={major}>{major || '—'}</option>}
              {majors.map((m) => <option key={m} value={m}>{m}</option>)}
            </select>
          </Field>
        )}
        <Field label={majorKey ? 'Minor version' : label}>
          <select className={`${inputCls} ${lock}`} value={n[minorKey] || ''} disabled={deployed}
            onChange={(e) => patchNode(n.id, { [minorKey]: e.target.value })}>
            <option value="">latest</option>
            {minors.map((v) => <option key={v} value={v}>{v}</option>)}
          </select>
        </Field>
      </div>
      {hint && <div className="text-[10px] leading-snug text-muted">{hint}</div>}
      {unavailable && (
        <div className="text-[10px] leading-snug text-warning">
          No {label} versions catalogued for {n.os} {n.osVersion} {n.arch} — run <code className="font-mono">make versions</code>.
        </div>
      )}
    </div>
  )
}

// The Intranet's admin mailbox, as the default Orchestrator alert recipient. Stored
// as the bare local part so it follows the stack's DOMAIN — see alertEmailAddress in
// app/orchestrator.go, and MAIL_ADMIN in app/intranet.go which creates the mailbox.
export const ORCH_DEFAULT_ALERT = 'admin'

// Kinds PMM ships an exporter for — everything except Orchestrator, which has no
// PMM service type at all. Mirrors aioPMMSupported in app/aio_target.go;
// TestAIOPMMFormGateMatchesTheRegistrationPath keeps the two in step.
const PMM_KINDS = [
  'ps', 'psrepl', 'innodb', 'pxc',
  'mysqlce', 'mysqlcerepl', 'mysqlceinnodb',
  'mariadb', 'mariadbrepl', 'mariadbgalera',
  'pg', 'patroni', 'repmgr', 'spock',
  'psmdb', 'psmrs', 'psmdbsharded',
  'valkey', 'valkeycluster', 'proxysql', 'haproxy',
]

// Kinds that serve an HTTP interface, and what to call it. Mirrors
// aioWebEndpoints in app/aio_ports.go — the Go side decides which ports get
// published; this only names them for the form. Keep the two in step.
const WEB_KINDS = {
  orchestrator: 'Orchestrator web UI',
  haproxy: 'HAProxy stats page',
  patroni: 'Patroni REST API',
}

// PGVersionPicker is the per-instance PostgreSQL major + minor. Unlike every
// other family this cannot be node-level: percona-postgresql16 and
// postgresql17 install into different prefixes and coexist, so two instances in
// one container may genuinely run different majors.
function PGVersionPicker({ inst, node: n, patch, deployed }) {
  const [cat, setCat] = useState(null)
  // Spock is not a Percona PostgreSQL package — it is a patched PostgreSQL built
  // from source, and only some majors carry the patch set. It therefore has its own
  // catalog (15–18 today), and offering it the PPG list would advertise 13 and 14,
  // which cannot be built. Same split the classic Spock frame form makes.
  const catalogFn = inst.kind === 'spock' ? stackApi.spockCatalog : stackApi.ppgCatalog
  useEffect(() => {
    let alive = true
    catalogFn().then((c) => { if (alive) setCat(c.images || []) }).catch(() => {})
    return () => { alive = false }
  }, [inst.kind]) // eslint-disable-line react-hooks/exhaustive-deps
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''
  const entry = imgs.find((i) => i.os === n.os && i.osVersion === n.osVersion && i.arch === n.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const major = inst.pgMajor || majors[0] || '16'
  const minors = entry?.versions?.[major] || []

  useEffect(() => {
    if (deployed || !imgs.length) return
    if (majors.length && !majors.includes(inst.pgMajor)) patch({ pgMajor: majors[0], pgVersion: '' })
    else if (inst.pgVersion && !minors.includes(inst.pgVersion)) patch({ pgVersion: '' })
  }, [imgs, inst.pgMajor, inst.pgVersion, n.os, n.osVersion, n.arch, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="grid grid-cols-2 gap-2">
      <Field label="PostgreSQL major" hint={deployed ? '' : 'Per instance — majors co-install.'}>
        <select className={`${inputCls} ${lock}`} value={major} disabled={deployed || !majors.length}
          onChange={(e) => patch({ pgMajor: e.target.value, pgVersion: '' })}>
          {majors.length === 0 && <option value={major}>{major}</option>}
          {majors.map((m) => <option key={m} value={m}>{m}</option>)}
        </select>
      </Field>
      <Field label="Minor version">
        <select className={`${inputCls} ${lock}`} value={inst.pgVersion || ''} disabled={deployed}
          onChange={(e) => patch({ pgVersion: e.target.value })}>
          <option value="">latest</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>
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
          {inst.kind === 'orchestrator' && (
            <Field
              label="Alert email"
              hint="Orchestrator mails this address when it detects a failure. A bare name is delivered inside the stack's domain via the Intranet mail server, which always provisions admin. Clear it to disable alerts."
            >
              <input className={`${inputCls} ${lock}`} type="text" placeholder="dba  or  dba@example.net"
                value={inst.alertEmail || ''} disabled={deployed}
                onChange={(e) => patch({ alertEmail: e.target.value })} />
            </Field>
          )}
          {isMySQL && inst.kind === 'ps' && (
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" checked={!!inst.gtid} disabled={deployed}
                onChange={(e) => patch({ gtid: e.target.checked })} />
              <span>Enable GTID</span>
            </label>
          )}

          {/* PostgreSQL is the one family whose packages are per-major and
              co-install, so its version is per instance rather than node-level. */}
          {fam === 'postgres' && (
            <PGVersionPicker inst={inst} node={node} patch={patch} deployed={deployed} />
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

          {/* Drop-down wiring — an All-in-One node draws no association lines.
              PMM is offered only for the three database engines: Orchestrator has no
              PMM service type, and Valkey and the proxies have one on their dedicated
              nodes but no All-in-One provisioner registers it. Same rule as the TLS
              control below, and validateStack rejects a stale value. */}
          {PMM_KINDS.includes(inst.kind) ? (
            <Field label="Monitored by (PMM)">
              <select className={`${inputCls} ${lock}`} value={inst.pmmNodeId || ''} disabled={deployed}
                onChange={(e) => patch({ pmmNodeId: e.target.value })}>
                <option value="">— none —</option>
                {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
              </select>
            </Field>
          ) : inst.pmmNodeId ? (
            // A design saved while the picker was offered. Say why it is going away
            // rather than dropping the value silently.
            <div className="rounded-lg border border-amber-500/40 bg-amber-500/10 px-2 py-1.5 text-[11px] leading-snug text-amber-700 dark:text-amber-400">
              PMM has no service type for {kindOf(inst.kind)?.label || inst.kind}, so this instance
              cannot be monitored as a service. Its OS metrics are still collected.
              <button type="button" className="ml-1 underline" disabled={deployed}
                onClick={() => patch({ pmmNodeId: '' })}>Clear the setting</button>
            </div>
          ) : null}

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
            <span>{WEB_KINDS[inst.kind] ? `Publish to the host (includes the ${WEB_KINDS[inst.kind]})` : 'Publish client port to the host'}</span>
          </label>
          {WEB_KINDS[inst.kind] && !inst.exportEnabled && (
            <p className="text-[10px] text-muted">
              {WEB_KINDS[inst.kind]} is only reachable from your browser once this is published.
            </p>
          )}
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
// ---------------------------------------------------------------- the manager

// AllInOneManager is the deployed node's console. It follows the tab shape every
// other *Manager.jsx uses, because the questions are the same ones: what is
// running, how do I reach it, what are the credentials, let me in.
//
// The difference is that all four answers are per INSTANCE rather than per node,
// which is precisely what a node holding six databases needs — a panel that only
// listed ports would leave an operator reading the deploy log to find a password.
const MGR_TABS = [
  { id: 'instances', label: 'Instances' },
  { id: 'connect', label: 'Connect' },
  { id: 'creds', label: 'Credentials' },
  { id: 'ports', label: 'Ports' },
]

export function AllInOneManager({ stackId, nodeId, dep, onDeleteNode }) {
  const api = aioApi(stackId, nodeId)
  const { openTerminal } = useTerminals()
  const [tab, setTab] = useState('instances')
  const [data, setData] = useState(null)
  const [busy, setBusy] = useState('')
  const [err, setErr] = useState('')
  const [logs, setLogs] = useState(null)

  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const openConsole = () => openTerminal({ stackId, nodeId, title: `${cfg.hostname || 'aio'} · root` })

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

  // Prefer the live list (it carries systemd state); fall back to the stored plan
  // so the panel is useful even before the first poll returns.
  const rows = data?.instances || cfg.instances || []
  const groups = [...new Set(rows.map((r) => r.group).filter(Boolean))]
  const tone = (s) => (s === 'active' ? 'success' : s === 'failed' ? 'danger' : 'muted')
  const dbRows = rows.filter((r) => ENGINE_OF[r.family])

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">All in One · {cfg.hostname || 'node'}</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>

      <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
        {MGR_TABS.map((t) => (
          <button key={t.id} onClick={() => setTab(t.id)}
            className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${tab === t.id ? 'bg-surface text-fg shadow' : 'text-muted'}`}>
            {t.label}
          </button>
        ))}
      </div>

      {err && <div className="rounded-md border border-danger/40 bg-danger/10 px-2 py-1.5 text-xs text-danger">{err}</div>}

      {tab === 'instances' && (
        <div className="space-y-3">
          <p className="text-xs text-muted">
            {rows.length} instance(s) in one container. The same operations are available as
            <code className="font-mono"> aioctl</code> in this node&apos;s console.
          </p>
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
                  <Badge tone={tone(r.state)}>{r.state || 'unknown'}</Badge>
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
        </div>
      )}

      {tab === 'connect' && <ConnectTab rows={dbRows} sec={sec} onConsole={openConsole} />}

      {tab === 'creds' && <CredentialsTab rows={rows} sec={sec} />}

      {tab === 'ports' && <PortsTab rows={rows} />}

      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// ENGINE_OF maps an instance family to the engine whose client/DSN shape applies.
// Non-database families are absent, which is what excludes them from Connect.
const ENGINE_OF = { mysql: 'mysql', postgres: 'postgres', mongodb: 'mongodb' }

const browserHost = () => (typeof location !== 'undefined' ? location.hostname : 'localhost')

// connectString is how to reach an instance from INSIDE the stack — the node's
// own console, or any other node. Each carries the instance's port explicitly,
// because none of them is where the client would look by default.
export function connectString(r, sec) {
  const p = r.ports?.client
  switch (ENGINE_OF[r.family]) {
    case 'mysql':
      return `mysql -h ${r.fqdn} -P ${p} -u ${sec.adminUser || 'admin'} -p'${sec.adminPassword || ''}'`
    case 'postgres':
      return `psql "postgresql://postgres:${sec.superPassword || ''}@${r.fqdn}:${p}/postgres"`
    case 'mongodb':
      return `mongosh "mongodb://admin:${sec.adminPassword || ''}@${r.fqdn}:${p}/?authSource=admin"`
    default:
      return `${r.fqdn}:${p}`
  }
}

// hostConnectString is the same instance reached from the machine running the
// browser, valid only when the instance published a port.
function hostConnectString(r, sec) {
  const h = browserHost()
  switch (ENGINE_OF[r.family]) {
    case 'mysql':
      return `mysql -h ${h} -P ${r.export} -u ${sec.adminUser || 'admin'} -p'${sec.adminPassword || ''}'`
    case 'postgres':
      return `psql "postgresql://postgres:${sec.superPassword || ''}@${h}:${r.export}/postgres"`
    case 'mongodb':
      return `mongosh "mongodb://admin:${sec.adminPassword || ''}@${h}:${r.export}/?authSource=admin"`
    default:
      return `${h}:${r.export}`
  }
}

// credRows is the credential set actually relevant to the families present, so a
// Valkey-only node is not shown a MySQL root password it does not have.
export function credRows(rows, sec) {
  const fams = new Set(rows.map((r) => r.family))
  const out = []
  if (fams.has('mysql')) {
    out.push({ label: 'MySQL — network superuser', user: sec.adminUser || 'admin', pass: sec.adminPassword || '',
      note: 'admin@\'%\' — use this over the network; root only works over the local socket.' })
    out.push({ label: 'MySQL — root (local socket)', user: sec.rootUser || 'root', pass: sec.rootPassword || '',
      note: 'From the console: aioctl connect <instance>.' })
    out.push({ label: 'MySQL — replication', user: sec.replUser || 'repl', pass: sec.replPassword || '', note: '' })
  }
  if (fams.has('postgres')) {
    out.push({ label: 'PostgreSQL — superuser', user: 'postgres', pass: sec.superPassword || '',
      note: 'Shared by every PostgreSQL instance on this node.' })
  }
  if (fams.has('mongodb')) {
    out.push({ label: 'MongoDB — root', user: 'admin', pass: sec.adminPassword || '',
      note: 'authSource=admin.' })
  }
  if (fams.has('valkey')) {
    out.push({ label: 'Valkey — requirepass', user: 'default', pass: sec.valkeyPassword || '', note: '' })
  }
  return out.filter((c) => c.pass)
}

// CopyLine is a one-line, copyable command. Long strings scroll rather than
// wrapping, so the panel stays readable at the designer's width.
function CopyLine({ text, label }) {
  return (
    <div>
      {label && <div className="pb-0.5 text-[10px] text-muted">{label}</div>}
      <div className="flex items-center gap-1 rounded-lg border border-border bg-bg px-2 py-1.5">
        <span className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap font-mono text-[10px] text-fg">{text}</span>
        <CopyBtn text={text} />
      </div>
    </div>
  )
}

// The three read-only tabs are separate components, not inline JSX, so the render
// smoke test can exercise each one. Only the default tab renders under SSR, so
// anything inline in a non-default tab is invisible to it — which is precisely
// how a bad icon reference reached a user in the first place.

function ConnectTab({ rows, sec, onConsole }) {
  return (
    <div className="space-y-3">
      <p className="text-xs text-muted">
        Every instance is on its own port — none of them the product default — so a
        connection string is the only reliable way in. Ready to paste from this node&apos;s
        console, or from any other node on the stack.
      </p>
      {rows.length === 0 && <div className="text-xs text-muted">No database instances on this node.</div>}
      {rows.map((r) => (
        <div key={r.inst} className="space-y-1">
          <div className="flex items-center gap-2 text-xs font-medium">
            <span className="h-2 w-2 rounded-full" style={{ backgroundColor: FAMILY_COLOR[r.family] }} />
            {r.inst}
            <span className="font-normal text-muted">· {kindOf(r.kind)?.label || r.kind}</span>
          </div>
          <CopyLine text={connectString(r, sec)} />
          {r.export > 0 && <CopyLine text={hostConnectString(r, sec)} label="from the host" />}
        </div>
      ))}
      <Button variant="outline" size="sm" className="w-full" onClick={onConsole}>
        <Icon.Nodes size={16} /> Open root console
      </Button>
      <p className="text-[10px] leading-snug text-muted">
        In the console, <code className="font-mono">aioctl list</code> shows every instance and
        <code className="font-mono"> aioctl connect &lt;instance&gt;</code> opens its own client
        with the credentials already applied.
      </p>
    </div>
  )
}

function CredentialsTab({ rows, sec }) {
  const creds = credRows(rows, sec)
  return (
    <div className="space-y-3">
      <p className="text-xs text-muted">
        Shared across this node&apos;s instances of a family, exactly as a classic multi-node
        cluster shares them — all from the stack&apos;s <code className="font-mono">.env</code>.
      </p>
      {creds.length === 0 && <div className="text-xs text-muted">No credentialed instances on this node.</div>}
      {creds.map((c) => (
        <div key={c.label} className="space-y-1">
          <div className="text-xs font-medium">{c.label}</div>
          {c.note && <div className="text-[10px] text-muted">{c.note}</div>}
          <SecretRow label={c.user} value={c.pass} />
        </div>
      ))}
    </div>
  )
}

function PortsTab({ rows }) {
  const published = rows.filter((r) => r.export > 0)
  // Instances that serve HTTP and had that port published: Orchestrator's UI,
  // HAProxy's stats page, Patroni's REST API. Without this the manager showed a
  // host:port string the user had to turn into a URL by hand.
  const web = rows.flatMap((r) => (r.web || [])
    .filter((w) => w.hostPort > 0)
    .map((w) => ({ ...w, inst: r.inst })))
  return (
    <div className="space-y-2">
      <p className="text-xs text-muted">Nothing here listens on its product&apos;s default port.</p>
      {web.length > 0 && (
        <div className="rounded-lg bg-surface2 px-2 py-1.5">
          <div className="pb-1 text-[10px] font-semibold uppercase tracking-wide text-muted">Web interfaces</div>
          {web.map((w) => {
            const url = `http://${browserHost()}:${w.hostPort}${w.path || '/'}`
            return (
              <div key={`${w.inst}-${w.port}`} className="flex items-center justify-between gap-2 py-0.5 text-[10px]">
                <span className="truncate text-muted">{w.inst} · {w.label}</span>
                <a href={url} target="_blank" rel="noreferrer"
                  className="truncate font-mono text-primary hover:underline">{url}</a>
              </div>
            )
          })}
        </div>
      )}
      <div className="rounded-lg bg-surface2 px-2 py-1.5">
        {rows.map((r) => (
          <div key={r.inst} className="flex justify-between gap-2 py-0.5 font-mono text-[10px]">
            <span className="truncate text-muted">{r.inst}</span>
            <span>{Object.entries(r.ports || {}).filter(([k, v]) => v && k !== 'base')
              .map(([k, v]) => `${PORT_ROLE[k] || k}:${v}`).join('  ')}</span>
          </div>
        ))}
      </div>
      {published.length > 0 && (
        <div className="rounded-lg bg-surface2 px-2 py-1.5">
          <div className="pb-1 text-[10px] font-semibold uppercase tracking-wide text-muted">Published to the host</div>
          {published.map((r) => (
            <div key={r.inst} className="flex justify-between gap-2 font-mono text-[10px]">
              <span className="truncate text-muted">{r.inst}</span>
              <span>{browserHost()}:{r.export}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

export const __tabsForTest = { ConnectTab, CredentialsTab, PortsTab }
