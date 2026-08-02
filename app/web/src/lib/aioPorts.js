// aioPorts.js — the browser-side mirror of app/aio_ports.go.
//
// The All-in-One form shows every instance's ports *before* a deploy exists, so
// the slot arithmetic has to run here too. This file and aio_ports.go must agree
// exactly; the Go side's TestAIOPortsNeverCollide / TestAIOPortsAvoidProductDefaults
// pin the invariants, and aioPorts.test-shape below documents the contract for
// anyone editing either half.
//
// If you change a family base, a slot offset or a member count here, change
// app/aio_ports.go in the same commit.

export const FAM = {
  MYSQL: 'mysql',
  PG: 'postgres',
  MONGO: 'mongodb',
  VALKEY: 'valkey',
  PROXY: 'proxysql',
  HAPROXY: 'haproxy',
  ORCH: 'orchestrator',
}

// Family port bases — deliberately far from every product default, so an
// instance can never collide with what the product would have chosen itself.
export const FAMILY_BASE = {
  [FAM.MYSQL]: 13000,
  [FAM.PG]: 15000,
  [FAM.PROXY]: 16000,
  [FAM.MONGO]: 17000,
  [FAM.HAPROXY]: 18000,
  [FAM.VALKEY]: 19000,
  [FAM.ORCH]: 20000,
}

export const SLOT_WIDTH = 10
export const SLOTS_PER_FAMILY = 100

// AIO_KINDS mirrors aioKinds in app/aio_ports.go: the catalog of features an
// All-in-One node can instantiate, in "Add feature" menu order.
//
// `supported` marks the families whose provisioner exists today. Unsupported
// kinds are still listed — greyed out with a reason — rather than hidden, so the
// menu shows where the node type is going instead of quietly omitting things.
export const AIO_KINDS = [
  { kind: 'ps', label: 'Percona Server', family: FAM.MYSQL, est: 700, supported: true },
  { kind: 'psrepl', label: 'PS Replication', family: FAM.MYSQL, cluster: true, min: 2, max: 5, def: 3, est: 700, supported: true },
  { kind: 'innodb', label: 'InnoDB Cluster / GR', family: FAM.MYSQL, cluster: true, min: 3, max: 9, def: 3, odd: true, est: 800, supported: true },
  { kind: 'pxc', label: 'PXC Cluster', family: FAM.MYSQL, cluster: true, min: 2, max: 5, def: 3, est: 900, supported: true },
  { kind: 'mysqlce', label: 'MySQL Community', family: FAM.MYSQL, est: 700, supported: true },
  { kind: 'mysqlcerepl', label: 'MySQL Replication', family: FAM.MYSQL, cluster: true, min: 2, max: 5, def: 3, est: 700, supported: true },
  { kind: 'mysqlceinnodb', label: 'MySQL InnoDB / GR', family: FAM.MYSQL, cluster: true, min: 3, max: 9, def: 3, odd: true, est: 800, supported: true },
  { kind: 'mariadb', label: 'MariaDB', family: FAM.MYSQL, est: 600, supported: true },
  { kind: 'mariadbrepl', label: 'MariaDB Replication', family: FAM.MYSQL, cluster: true, min: 2, max: 5, def: 3, est: 600, supported: true },
  { kind: 'mariadbgalera', label: 'MariaDB Galera', family: FAM.MYSQL, cluster: true, min: 3, max: 5, def: 3, odd: true, est: 800, supported: true },
  { kind: 'pg', label: 'PostgreSQL', family: FAM.PG, est: 300, supported: true },
  { kind: 'patroni', label: 'Patroni Cluster', family: FAM.PG, cluster: true, min: 2, max: 5, def: 3, est: 450, supported: true },
  { kind: 'repmgr', label: 'repmgr Cluster', family: FAM.PG, cluster: true, min: 2, max: 5, def: 3, est: 350, supported: true },
  { kind: 'spock', label: 'Spock Cluster', family: FAM.PG, cluster: true, min: 2, max: 5, def: 2, est: 350, supported: true },
  { kind: 'psmdb', label: 'PSMDB', family: FAM.MONGO, est: 500, supported: true },
  { kind: 'psmrs', label: 'PSMDB Replica Set', family: FAM.MONGO, cluster: true, min: 3, max: 7, def: 3, odd: true, est: 500, supported: true },
  { kind: 'psmdbsharded', label: 'PSMDB Sharded', family: FAM.MONGO, cluster: true, min: 5, max: 13, def: 5, est: 400, supported: true },
  { kind: 'valkey', label: 'Valkey', family: FAM.VALKEY, est: 120, supported: true },
  { kind: 'valkeycluster', label: 'Valkey Cluster', family: FAM.VALKEY, cluster: true, min: 3, max: 7, def: 3, est: 120, supported: true },
  { kind: 'proxysql', label: 'ProxySQL', family: FAM.PROXY, cluster: true, min: 1, max: 3, def: 1, est: 200, supported: true },
  { kind: 'haproxy', label: 'HAProxy', family: FAM.HAPROXY, est: 80, supported: true },
  { kind: 'orchestrator', label: 'Orchestrator', family: FAM.ORCH, est: 150, supported: true },
]

export const kindOf = (kind) => AIO_KINDS.find((k) => k.kind === kind)
export const familyOf = (kind) => kindOf(kind)?.family || ''

// Family display names for the grouped "Add feature" menu and version pickers.
export const FAMILY_LABEL = {
  [FAM.MYSQL]: 'MySQL',
  [FAM.PG]: 'PostgreSQL',
  [FAM.MONGO]: 'MongoDB',
  [FAM.VALKEY]: 'Valkey',
  [FAM.PROXY]: 'ProxySQL',
  [FAM.HAPROXY]: 'HAProxy',
  [FAM.ORCH]: 'Orchestrator',
}

// memberCount clamps a declared member count the way the Go planner does, so the
// port preview matches what will actually be provisioned.
export function memberCount(kind, members) {
  const k = kindOf(kind)
  if (!k) return 1
  if (!k.cluster) return 1
  if (!members || members < k.min) return k.def
  if (members > k.max) return k.max
  return members
}

// portsFor resolves one member's ports. Offsets must match aioPortsFor().
export function portsFor(kind, slot, member) {
  const fam = familyOf(kind)
  const base = (FAMILY_BASE[fam] ?? 0) + (slot + member) * SLOT_WIDTH
  const p = { base, client: base }
  switch (fam) {
    case FAM.MYSQL:
      p.admin = base + 1
      p.group = base + 2
      p.ist = base + 3
      p.sst = base + 4
      p.check = base + 5
      break
    case FAM.PG:
      p.rest = base + 1
      p.etcdCli = base + 2
      p.etcdPr = base + 3
      break
    case FAM.VALKEY:
      // Valkey's bus defaults to port+10000, which would leave the family's
      // range — it is pinned inside the slot instead (cluster-port).
      p.group = base + 1
      break
    case FAM.PROXY:
      p.admin = base + 1
      p.group = base + 2
      break
    case FAM.HAPROXY:
      p.check = base + 1
      p.admin = base + 2
      break
    default:
      break
  }
  return p
}

// portList is every port a member binds, sorted — the form's per-instance table.
export const portList = (p) =>
  [p.client, p.admin, p.group, p.ist, p.sst, p.check, p.rest, p.etcdCli, p.etcdPr]
    .filter((v) => v)
    .sort((a, b) => a - b)

// PORT_ROLE labels a port in the instance card's table.
export const PORT_ROLE = {
  client: 'client', admin: 'admin', group: 'cluster', ist: 'IST', sst: 'SST',
  check: 'check', rest: 'REST', etcdCli: 'etcd client', etcdPr: 'etcd peer',
}

// assignSlots walks instances in declaration order, giving each its first slot
// within its family. Positional and deterministic, so appending an instance
// never renumbers the ones before it — a deployed instance keeps its port.
export function assignSlots(instances) {
  const next = {}
  const out = {}
  for (const inst of instances) {
    const fam = familyOf(inst.kind)
    if (!fam) continue
    out[inst.id] = next[fam] || 0
    next[fam] = (next[fam] || 0) + memberCount(inst.kind, inst.members)
  }
  return out
}

// memberInst mirrors aioMemberInst: the per-member instance id.
export function memberInst(name, kind, member, total) {
  const base = sanitizeInst(name)
  const k = kindOf(kind)
  if (total <= 1 && !k?.cluster) return base
  return `${base}-n${member + 1}`
}

// sanitizeInst mirrors hostLabel() in app/dns.go — an instance name becomes a
// hostname, a directory and a systemd unit name, so it must survive all three.
export function sanitizeInst(s) {
  return String(s || '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
}

// planMembers expands the declared instances into the concrete member list the
// form previews and the backend will provision.
export function planMembers(instances) {
  const slots = assignSlots(instances)
  const out = []
  for (const inst of instances) {
    const k = kindOf(inst.kind)
    if (!k) continue
    const total = memberCount(inst.kind, inst.members)
    for (let m = 0; m < total; m++) {
      out.push({
        inst: memberInst(inst.name, inst.kind, m, total),
        kind: inst.kind,
        family: k.family,
        group: k.cluster ? sanitizeInst(inst.name) : '',
        ports: portsFor(inst.kind, slots[inst.id], m),
      })
    }
  }
  return out
}

// --- MySQL flavor ----------------------------------------------------------
// Every one of these server packages declares Provides: mysql-server and conflicts
// with the others, so an All-in-One node has at most one MySQL flavor. It is
// derived from the instances, never chosen. Mirrors app/aio_ports.go.

export const FLAVOR_PS = 'ps'
export const FLAVOR_PXC = 'pxc'
export const FLAVOR_MARIADB = 'mariadb'
export const FLAVOR_MYSQLCE = 'mysqlce'

export const FLAVOR_LABEL = {
  [FLAVOR_PS]: 'Percona Server',
  [FLAVOR_PXC]: 'Percona XtraDB Cluster',
  [FLAVOR_MARIADB]: 'MariaDB',
  [FLAVOR_MYSQLCE]: 'MySQL Community',
}

export const flavorOfKind = (kind) => {
  if (kind === 'ps' || kind === 'psrepl' || kind === 'innodb') return FLAVOR_PS
  if (kind === 'pxc') return FLAVOR_PXC
  if (kind === 'mariadb' || kind === 'mariadbrepl' || kind === 'mariadbgalera') return FLAVOR_MARIADB
  if (kind === 'mysqlce' || kind === 'mysqlcerepl' || kind === 'mysqlceinnodb') return FLAVOR_MYSQLCE
  return ''
}

// A kind's shape is its topology, independent of whose build provides it.
export const SHAPE_SINGLE = 'single'
export const SHAPE_REPL = 'repl'
export const SHAPE_GR = 'gr'
export const SHAPE_GALERA = 'galera'

export const shapeOfKind = (kind) => {
  if (kind === 'ps' || kind === 'mariadb' || kind === 'mysqlce') return SHAPE_SINGLE
  if (kind === 'psrepl' || kind === 'mariadbrepl' || kind === 'mysqlcerepl') return SHAPE_REPL
  if (kind === 'innodb' || kind === 'mysqlceinnodb') return SHAPE_GR
  if (kind === 'pxc' || kind === 'mariadbgalera') return SHAPE_GALERA
  return ''
}

// mysqlFlavor returns { flavor, conflict, byFlavor } where byFlavor maps each
// flavor present to the instance names that pulled it in.
export function mysqlFlavor(instances) {
  const byFlavor = {}
  for (const i of instances) {
    const f = flavorOfKind(i.kind)
    if (!f) continue
    ;(byFlavor[f] ||= []).push(i.name)
  }
  const present = Object.keys(byFlavor)
  if (present.length > 1) return { flavor: '', conflict: true, byFlavor }
  return { flavor: present[0] || '', conflict: false, byFlavor }
}

// addBlockedReason explains why a kind cannot be added to the current node, or
// returns '' when it can. The menu shows the entry disabled with this text
// rather than hiding it, so the constraint is discoverable instead of mysterious.
export function addBlockedReason(kind, instances) {
  const k = kindOf(kind)
  if (!k) return 'Unknown feature'
  if (!k.supported) return `${k.label} instances are not implemented yet — use a dedicated ${k.label} node`
  const want = flavorOfKind(kind)
  if (!want) return ''
  const { byFlavor } = mysqlFlavor(instances)
  const other = Object.keys(byFlavor).filter((f) => f !== want)
  if (!other.length) return ''
  const names = other.map((f) => `${FLAVOR_LABEL[f]} (${byFlavor[f].join(', ')})`).join(' and ')
  return `${FLAVOR_LABEL[want]} can't share a container with ${names} — these server packages all provide mysql-server and conflict`
}

// estMemMB is the rough total footprint, for the form's sizing warning.
export const estMemMB = (instances) =>
  instances.reduce((t, i) => t + (kindOf(i.kind)?.est || 0) * memberCount(i.kind, i.members), 0)

// nextInstanceName picks the lowest unused "<prefix>NN" for a kind.
export function nextInstanceName(instances, kind) {
  const k = kindOf(kind)
  const prefix = k?.cluster ? `${kind}-cluster-` : kind
  const used = new Set(instances.map((i) => sanitizeInst(i.name)))
  for (let i = 1; ; i++) {
    const n = `${prefix}${String(i).padStart(2, '0')}`
    if (!used.has(n)) return n
  }
}
