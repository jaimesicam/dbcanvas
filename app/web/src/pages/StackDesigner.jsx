import { useCallback, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Icon } from '../components/Icons.jsx'
import { Card, Button, Badge, Field, ConfirmButton, inputCls } from '../components/ui.jsx'
import { stackApi, frameApi, TTL_OPTIONS, DEPLOY_TONE } from '../lib/stackApi.js'
import IntranetManager from './IntranetManager.jsx'
import PMMManager from './PMMManager.jsx'
import PXCManager from './PXCManager.jsx'
import ProxySQLManager from './ProxySQLManager.jsx'
import MySQLManager from './MySQLManager.jsx'
import InnoDBManager from './InnoDBManager.jsx'
import MongoDBManager from './MongoDBManager.jsx'
import SeaweedFSManager from './SeaweedFSManager.jsx'
import PatroniManager from './PatroniManager.jsx'
import HAProxyManager from './HAProxyManager.jsx'
import PGManager from './PGManager.jsx'
import RepmgrManager from './RepmgrManager.jsx'
import SpockManager from './SpockManager.jsx'
import { useTerminals } from '../terminal/TerminalProvider.jsx'
import {
  PORTS, dist, portPoint, edgePath, screenToWorld, zoomAt,
} from '../lib/canvas.js'

const NODE_W = 212
const NODE_H = 104
const SNAP = 26

// Architecture options (must match images built by `make images`).
const ARCH_OPTIONS = [
  { id: 'amd64', label: 'amd64 (x86-64)' },
  { id: 'arm64', label: 'arm64 (aarch64)' },
]

// Node-type catalog.
const NODE_TYPES = {
  intranet: {
    label: 'Intranet',
    sub: 'Squid Proxy · DNS · Mail · OpenLDAP · CA',
    color: '#6366f1',
    icon: 'Server',
    singleton: true,
    ports: false, // self-contained; no connection endpoints
    osOptions: [{ id: 'oel9', label: 'Oracle Linux 9' }],
  },
  pmm: {
    label: 'PMM3',
    slug: 'pmm',
    sub: 'Percona Monitoring & Management',
    color: '#0ea5e9',
    icon: 'Monitor',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'pmm', label: 'percona/pmm-server' }],
    defaults: { version: '', adminPassword: '', generateCert: false, watchtowerNodeId: '' },
  },
  // PXC nodes live inside a PXC cluster frame (not added from the toolbar
  // directly); this entry only supplies the color/icon used to render them.
  pxc: {
    label: 'PXC Node',
    slug: 'pxc',
    sub: 'Percona XtraDB Cluster',
    color: '#a855f7',
    icon: 'Database',
  },
  // Percona Server replication members live inside a Percona Server Replication frame.
  mysql: {
    label: 'Percona Server',
    slug: 'mysql',
    sub: 'Percona Server replication member',
    color: '#2563eb',
    icon: 'Database',
  },
  // InnoDB Cluster / GR members live inside an InnoDB Cluster/GR frame.
  innodb: {
    label: 'InnoDB Cluster / GR',
    slug: 'innodb',
    sub: 'Group Replication member',
    color: '#0891b2',
    icon: 'Database',
  },
  // PS MongoDB members (mongod shard/config + mongos router) live inside a fixed
  // PSMDB Sharded Cluster frame.
  psmdb: {
    label: 'PS MongoDB',
    slug: 'psmdb',
    sub: 'PS MongoDB member',
    color: '#10b981',
    icon: 'Database',
  },
  // PS MongoDB replica-set members live inside a PSMDB RS frame.
  psmrs: {
    label: 'PSMDB RS',
    slug: 'psmrs',
    sub: 'PS MongoDB replica-set member',
    color: '#059669',
    icon: 'Database',
  },
  // Patroni members (PostgreSQL + Patroni + etcd) live inside a Patroni cluster frame.
  patroni: {
    label: 'Patroni',
    slug: 'patroni',
    sub: 'PostgreSQL + Patroni + etcd',
    color: '#336791',
    icon: 'Database',
  },
  // repmgr members (PostgreSQL + repmgr streaming replication) live inside a repmgr
  // cluster frame.
  repmgr: {
    label: 'repmgr',
    slug: 'repmgr',
    sub: 'PostgreSQL + repmgr',
    color: '#0e7490',
    icon: 'Database',
  },
  spock: {
    label: 'Spock',
    slug: 'spock',
    sub: 'PostgreSQL + Spock (multi-master)',
    color: '#dc2626',
    icon: 'Database',
  },
  // Standalone single Percona Server for MongoDB instance (no replication).
  psm: {
    label: 'PSMDB',
    slug: 'psm',
    sub: 'PS MongoDB (standalone)',
    color: '#059669',
    icon: 'Database',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', psmdbMajor: '8.0', psmdbVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
      exportEnabled: false, exportHostPort: 0,
      enableOIDC: false, keycloakNodeId: '', oidcRealm: 'mongodb',
      oidcClientId: 'mongodb-client', oidcAuthClaim: 'MyClaim', oidcUseAuthClaim: true,
    },
  },
  // Standalone single Percona Server instance (no replication).
  ps: {
    label: 'Percona Server',
    slug: 'ps',
    sub: 'Percona Server (standalone)',
    color: '#2563eb',
    icon: 'Database',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', psMajor: '8.0', psVersion: '',
      rootPassword: '', gtid: true, pmmNodeId: '', useProxy: false,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
      exportEnabled: false, exportHostPort: 0,
    },
  },
  // Standalone single PostgreSQL instance (no Patroni/etcd/replication).
  pg: {
    label: 'PostgreSQL',
    slug: 'pg',
    sub: 'PostgreSQL (standalone)',
    color: '#336791',
    icon: 'Database',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', pgMajor: '16', pgVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false,
      usePgBackRest: false, seaweedfsNodeId: '',
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
      exportEnabled: false, exportHostPort: 0,
    },
  },
  proxysql: {
    label: 'ProxySQL',
    slug: 'proxysql',
    sub: 'ProxySQL — MySQL proxy',
    color: '#f59e0b',
    icon: 'ProxySQL',
    singleton: false,
    ports: true, // links to a PXC cluster frame (data flows PXC → ProxySQL)
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', arch: 'amd64',
      proxysqlMajor: '2', proxysqlVersion: '', mode: 'singlewrite',
      exportEnabled: false, exportHostPort: 0, pmmNodeId: '', useProxy: false,
    },
  },
  // HAProxy — a TCP load balancer fronting ONE database cluster: a Patroni PostgreSQL
  // cluster OR a Percona XtraDB Cluster (mutually exclusive). Links to the cluster frame
  // (data flows cluster → HAProxy) and routes writes/reads via the backend's health
  // checks (Patroni REST for Patroni; clustercheck :9200 for PXC).
  haproxy: {
    label: 'HAProxy',
    slug: 'haproxy',
    sub: 'HAProxy — PostgreSQL / PXC load balancer',
    color: '#22c55e',
    icon: 'ProxySQL',
    singleton: false,
    ports: true,
    osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }],
    defaults: {
      os: 'oraclelinux', osVersion: '9', arch: 'amd64',
      exportEnabled: false, exportHostPort: 0, pmmNodeId: '', useProxy: false,
    },
  },
  // SeaweedFS — an S3-compatible object store (backup target). Like PMM it runs a
  // ready-made image (pulled at deploy), not a systemd OS image.
  seaweedfs: {
    label: 'SeaweedFS',
    slug: 'seaweedfs',
    sub: 'S3-compatible object storage (backups)',
    color: '#14b8a6',
    icon: 'Bucket',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'seaweedfs', label: 'chrislusf/seaweedfs' }],
    defaults: { accessKey: 'seaweedfs', secretKey: '', bucket: '' },
  },
  // Watchtower — a per-stack singleton running percona/watchtower with the docker
  // socket mounted and its HTTP API enabled. A PMM node associated with it can
  // trigger in-app server upgrades. Runs a ready-made image (pulled at deploy).
  watchtower: {
    label: 'Watchtower',
    slug: 'watchtower',
    sub: 'Container auto-upgrades (PMM)',
    color: '#475569',
    icon: 'Server',
    singleton: true,
    ports: false,
    osOptions: [{ id: 'watchtower', label: 'percona/watchtower' }],
    defaults: {},
  },
  // Keycloak — a per-stack singleton OpenID Connect identity provider. A standalone
  // PSMDB node can enable MONGODB-OIDC authentication against it. Runs the upstream
  // keycloak image in dev mode (pulled at deploy); console published to the host.
  keycloak: {
    label: 'Keycloak',
    slug: 'keycloak',
    sub: 'OIDC identity provider',
    color: '#4f46e5',
    icon: 'Users',
    singleton: true,
    ports: false,
    osOptions: [{ id: 'keycloak', label: 'quay.io/keycloak/keycloak' }],
    defaults: { generateCert: true, certTtlValue: 365, certTtlUnit: 'days' },
  },
  // Ubuntu VNC — a desktop "jump box" (XFCE over web VNC) with the Percona client
  // tools preinstalled, for ad-hoc troubleshooting. Runs ubuntu:24.04 (pulled at deploy).
  vnc: {
    label: 'Ubuntu VNC',
    slug: 'vnc',
    sub: 'Desktop + web VNC (DB clients)',
    color: '#dd4814',
    icon: 'Monitor',
    singleton: true,
    ports: false,
    osOptions: [{ id: 'ubuntu', label: 'Ubuntu' }],
    defaults: {
      os: 'ubuntu', osVersion: '24.04', arch: 'amd64',
      vncUser: 'dbadmin', vncPassword: '', useProxy: false,
    },
  },
  // Standalone Valkey (valkey/valkey-bundle image, pulled at deploy). Analogue of the
  // standalone Percona Server node: a password (requirepass) + optional LDAP auth.
  valkey: {
    label: 'Valkey',
    slug: 'valkey',
    sub: 'Valkey (standalone)',
    color: '#7c3aed',
    icon: 'Database',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'valkey', label: 'valkey/valkey-bundle' }],
    defaults: {
      rootPassword: '', useLdap: false, pmmNodeId: '',
      exportEnabled: false, exportHostPort: 0,
    },
  },
}

// ---------------------------------------------------------- PXC cluster frames
const PXC_NODE_W = 116
const PXC_NODE_H = 78
const FRAME_TITLE = 32
const FRAME_PAD = 14
const FRAME_GAP = 12

// layoutFrame derives a frame's size and lays its member nodes out in a row.
function layoutFrame(frame, frameNodes) {
  const n = Math.max(1, frameNodes.length)
  const w = FRAME_PAD * 2 + n * PXC_NODE_W + (n - 1) * FRAME_GAP
  const h = FRAME_TITLE + FRAME_PAD * 2 + PXC_NODE_H
  const positioned = frameNodes.map((nd, i) => ({
    ...nd,
    x: frame.x + FRAME_PAD + i * (PXC_NODE_W + FRAME_GAP),
    y: frame.y + FRAME_TITLE + FRAME_PAD,
  }))
  return { frame: { ...frame, w, h }, nodes: positioned }
}

// layoutPSMDBFrame lays out a sharded cluster as a grouped grid: a top row with
// the mongos router + the config-server RS members, then one column per shard
// (its replica-set members stacked below). Sizes adapt to the member count so it
// fits both the standard (13-node) and minimum (5-node) setups; the single-row
// layoutFrame is unusable here.
function layoutPSMDBFrame(frame, frameNodes) {
  // Stable ordering independent of array order: derive columns/rows from role.
  const mongos = frameNodes.filter((n) => n.role === 'mongos')
  const config = frameNodes.filter((n) => n.role === 'config')
  const shardIdx = [...new Set(frameNodes.filter((n) => n.role === 'shard').map((n) => n.shard))].sort((a, b) => a - b)
  const shards = shardIdx.map((s) => frameNodes.filter((n) => n.role === 'shard' && n.shard === s))
  const colW = PXC_NODE_W + FRAME_GAP
  const rowH = PXC_NODE_H + FRAME_GAP
  // columns: max(top row = 1 mongos + config members, shard columns).
  const ncols = Math.max(1 + config.length, shards.length, 3)
  const w = FRAME_PAD * 2 + ncols * PXC_NODE_W + (ncols - 1) * FRAME_GAP
  // rows: 1 top row + the tallest shard replica set.
  const maxShardRows = shards.reduce((m, s) => Math.max(m, s.length), 0)
  const nrows = 1 + maxShardRows
  const h = FRAME_TITLE + FRAME_PAD * 2 + nrows * PXC_NODE_H + (nrows - 1) * FRAME_GAP
  const ox = frame.x + FRAME_PAD
  const oy = frame.y + FRAME_TITLE + FRAME_PAD
  const positioned = []
  // Top row: mongos at col 0, config RS at cols 1..n.
  mongos.forEach((nd) => positioned.push({ ...nd, x: ox, y: oy }))
  config.forEach((nd, i) => positioned.push({ ...nd, x: ox + (i + 1) * colW, y: oy }))
  // Shard columns: each shard a column, members stacked in the rows below.
  shards.forEach((members, s) => {
    members.forEach((nd, r) => positioned.push({ ...nd, x: ox + s * colW, y: oy + (r + 1) * rowH }))
  })
  // Preserve original order for any node not matched (defensive).
  const placedIds = new Set(positioned.map((n) => n.id))
  frameNodes.forEach((nd) => { if (!placedIds.has(nd.id)) positioned.push({ ...nd, x: ox, y: oy }) })
  return { frame: { ...frame, w, h }, nodes: positioned }
}

// relayoutFrame picks the right layout for a frame type.
function relayoutFrame(frame, frameNodes) {
  return frame.type === 'psmdb' ? layoutPSMDBFrame(frame, frameNodes) : layoutFrame(frame, frameNodes)
}

// nextClusterName → pxc-cluster-NN, unique across all PXC frames (from 00).
function nextClusterName(frames) {
  let max = -1
  for (const f of frames) {
    const m = (f.label || '').match(/^pxc-cluster-(\d+)$/)
    if (m) max = Math.max(max, parseInt(m[1], 10))
  }
  return `pxc-cluster-${String(max + 1).padStart(2, '0')}`
}

// nextPXCName → lowest pxcNN not already used by any PXC node in the stack.
function nextPXCName(usedSet) {
  for (let i = 1; ; i++) {
    const name = `pxc${String(i).padStart(2, '0')}`
    if (!usedSet.has(name)) return name
  }
}

// nextNamedCluster → "<prefix>-NN" unique across the frames (from 00).
function nextNamedCluster(frames, prefix) {
  let max = -1
  const re = new RegExp(`^${prefix}-(\\d+)$`)
  for (const f of frames) {
    const m = (f.label || '').match(re)
    if (m) max = Math.max(max, parseInt(m[1], 10))
  }
  return `${prefix}-${String(max + 1).padStart(2, '0')}`
}

// nextMemberName → lowest "<prefix>NN" not already used by any node in the stack.
function nextMemberName(usedSet, prefix) {
  for (let i = 1; ; i++) {
    const name = `${prefix}${String(i).padStart(2, '0')}`
    if (!usedSet.has(name)) return name
  }
}

// Per-frame-type presentation: accent color and the description line.
const FRAME_COLORS = { pxc: '#a855f7', proxysql: '#f59e0b', mysql: '#2563eb', innodb: '#0891b2', psmdb: '#10b981', psmrs: '#059669', patroni: '#336791', repmgr: '#0e7490', spock: '#dc2626', valkeycluster: '#7c3aed' }

// typeColor maps a node/frame type to its canvas color so a toolbar "add" button can
// be tinted to match the node/frame it creates. addBtnStyle turns that into inline
// styles (disabled buttons keep the tint but the shared disabled:opacity-50 fades it).
const typeColor = (t) => FRAME_COLORS[t] || NODE_TYPES[t]?.color || null
const addBtnStyle = (t) => {
  const c = typeColor(t)
  return c ? { backgroundColor: c, borderColor: c, color: '#fff' } : undefined
}
const frameColor = (f) => FRAME_COLORS[f?.type] || '#a855f7'

const osLabel = (type, os) => (NODE_TYPES[type]?.osOptions.find((o) => o.id === os)?.label) || os

// pxcOSLabel formats a PXC frame's OS family + version (e.g. "Oracle Linux 9").
const PXC_OS_NAMES = { oraclelinux: 'Oracle Linux', ubuntu: 'Ubuntu', debian: 'Debian' }
const pxcOSLabel = (f) => [PXC_OS_NAMES[f?.os] || f?.os, f?.osVersion].filter(Boolean).join(' ')

// pxcVersionLabel formats a PXC frame's version for display (minor if pinned,
// else the major series, e.g. "Percona XtraDB Cluster 8.0").
const pxcVersionLabel = (f) => `Percona XtraDB Cluster ${f?.pxcVersion || f?.pxcMajor || ''}`.trim()

// frameVersionLabel: the description line for a cluster-frame type.
const frameVersionLabel = (f) => {
  if (f?.type === 'proxysql') return `ProxySQL ${f?.proxysqlVersion || f?.proxysqlMajor || ''}`.trim()
  if (f?.type === 'mysql') return `Percona Server ${f?.psVersion || f?.psMajor || ''} replication`.trim()
  if (f?.type === 'innodb') return `${f?.replMode === 'groupreplication' ? 'Group Replication' : 'InnoDB Cluster'}${f?.pdpsRepo ? ` · ${f.pdpsRepo}` : ''}`
  if (f?.type === 'psmdb') return `PS MongoDB ${f?.psmdbVersion || f?.psmdbMajor || ''} sharded · ${f?.psmdbSetup === 'minimum' ? 'minimum' : 'standard'}`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'psmrs') return `PS MongoDB ${f?.psmdbVersion || f?.psmdbMajor || ''} replica set`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'patroni') return `Percona PostgreSQL ${f?.pgVersion || f?.pgMajor || ''} · Patroni`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'repmgr') return `PostgreSQL ${f?.pgVersion || f?.pgMajor || ''} · repmgr (PGDG)`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'spock') return `PostgreSQL ${f?.pgVersion || f?.pgMajor || ''} · Spock multi-master`.replace(/\s+/g, ' ').trim()
  if (f?.type === 'valkeycluster') return 'Valkey Cluster · valkey/valkey-bundle'
  return pxcVersionLabel(f)
}

// ProxySQL implementation-mode options depend on the linked backend type: PXC
// (proxysql-admin singlewrite/loadbal) vs MySQL replication (primary/rwsplit). The
// "wrong" set is never shown — they switch with the associated cluster.
const PROXY_MODE_OPTS = {
  pxc: [{ id: 'singlewrite', label: 'single writer (default)' }, { id: 'loadbal', label: 'load balancer' }],
  mysql: [{ id: 'rwsplit', label: 'read/write split' }, { id: 'primary', label: 'primary only (all to primary)' }],
}
const proxyModeOpts = (backendType) => PROXY_MODE_OPTS[backendType === 'mysql' ? 'mysql' : 'pxc']

// nodeOSLabel renders a free node's OS line; ProxySQL carries its own os/version
// (like a PXC frame), other nodes map via their osOptions.
const nodeOSLabel = (n) => (n.type === 'proxysql' || n.type === 'ps' || n.type === 'pg' || n.type === 'psm' || n.type === 'haproxy' || n.type === 'vnc' ? pxcOSLabel(n) : osLabel(n.type, n.os))

// Auto-numbered per-type labels: a non-singleton node is named "<slug>-NN" with
// NN zero-padded from 01 and increasing per node type (pmm-01, pmm-02, …, and in
// future psmysql-01, psmysql-02, …). These labels become the node hostnames in
// the Intranet DNS / FQDNs. The Intranet singleton keeps its plain label.
function nextLabel(type, nodes) {
  const def = NODE_TYPES[type]
  if (def.singleton) return def.label
  const base = def.slug || type
  const re = new RegExp(`^${base}-(\\d+)$`)
  let max = 0
  for (const n of nodes) {
    if (n.type !== type) continue
    const m = (n.label || '').match(re)
    if (m) max = Math.max(max, parseInt(m[1], 10))
  }
  return `${base}-${String(max + 1).padStart(2, '0')}`
}

// Small SVG progress ring (upper-right of a provisioning node).
function ProgressRing({ percent = 0, size = 24 }) {
  const r = (size - 5) / 2
  const c = 2 * Math.PI * r
  const off = c * (1 - Math.max(0, Math.min(100, percent)) / 100)
  const k = size / 2
  return (
    <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} title={`${percent}%`}>
      <circle cx={k} cy={k} r={r} fill="var(--surface)" stroke="var(--surface2)" strokeWidth="2.5" />
      <circle cx={k} cy={k} r={r} fill="none" stroke="var(--warning)" strokeWidth="2.5" strokeLinecap="round"
        strokeDasharray={c} strokeDashoffset={off} transform={`rotate(-90 ${k} ${k})`} />
    </svg>
  )
}

const STATUS_TONE = { draft: 'muted', deployed: 'success', expired: 'danger' }

export default function StackDesigner() {
  const [stacks, setStacks] = useState([])
  const [openId, setOpenId] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setError('')
    try {
      setStacks(await stackApi.list())
    } catch (err) {
      setError(err.message)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  if (openId != null) {
    return <StackEditor stackId={openId} onBack={() => { setOpenId(null); load() }} />
  }

  return (
    <StackList
      stacks={stacks}
      loading={loading}
      error={error}
      onOpen={setOpenId}
      onCreated={(s) => setOpenId(s.id)}
      onChanged={load}
    />
  )
}

// ---------------------------------------------------------------- list view

function ttlLabel(id) {
  return TTL_OPTIONS.find((t) => t.id === id)?.label ?? id
}

function expiresIn(iso) {
  if (!iso) return 'never expires'
  const ms = new Date(iso) - new Date()
  if (ms <= 0) return 'expired'
  const h = Math.floor(ms / 3.6e6)
  if (h >= 24) return `expires in ${Math.floor(h / 24)}d`
  if (h >= 1) return `expires in ${h}h`
  return `expires in ${Math.max(1, Math.floor(ms / 6e4))}m`
}

function StackList({ stacks, loading, error, onOpen, onCreated, onChanged }) {
  const [showNew, setShowNew] = useState(false)

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">Database Stacks</h2>
          <p className="text-sm text-muted">Design, deploy, and manage container stacks.</p>
        </div>
        <Button onClick={() => setShowNew(true)}>
          <Icon.Plus size={16} /> New stack
        </Button>
      </div>

      {error && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{error}</div>}

      {loading ? (
        <div className="py-10 text-center text-muted">Loading…</div>
      ) : stacks.length === 0 ? (
        <Card>
          <div className="py-10 text-center text-muted">
            No stacks yet. Create one to start designing.
          </div>
        </Card>
      ) : (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
          {stacks.map((s) => (
            <Card key={s.id} className="transition hover:border-primary">
              <div className="flex items-start justify-between gap-2">
                <button onClick={() => onOpen(s.id)} className="min-w-0 text-left">
                  <div className="truncate text-sm font-semibold text-fg">{s.name}</div>
                  <div className="mt-0.5 text-xs text-muted">{expiresIn(s.expiresAt)}</div>
                </button>
                <Badge tone={STATUS_TONE[s.status] || 'muted'}>{s.status}</Badge>
              </div>
              <div className="mt-3 flex items-center justify-between">
                <Badge tone="primary">{ttlLabel(s.ttl)}</Badge>
                <div className="flex gap-1">
                  <Button size="sm" variant="outline" onClick={() => onOpen(s.id)}>Open</Button>
                  <ConfirmButton
                    size="sm"
                    variant="ghost"
                    title="Delete stack (tears down containers)"
                    confirmLabel="Delete?"
                    onConfirm={async () => { await stackApi.remove(s.id); onChanged() }}
                  >
                    <Icon.Trash size={16} />
                  </ConfirmButton>
                </div>
              </div>
            </Card>
          ))}
        </div>
      )}

      {showNew && (
        <NewStackModal
          onClose={() => setShowNew(false)}
          onCreated={(s) => { setShowNew(false); onCreated(s) }}
        />
      )}
    </div>
  )
}

function NewStackModal({ onClose, onCreated }) {
  const [name, setName] = useState('')
  const [ttl, setTtl] = useState('24h')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function submit(e) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const s = await stackApi.create(name.trim() || 'Untitled stack', ttl)
      onCreated(s)
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-4 text-sm font-semibold">New stack</h3>
        {error && <div className="mb-3 rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{error}</div>}
        <form onSubmit={submit} className="space-y-3">
          <Field label="Name">
            <input className={inputCls} value={name} onChange={(e) => setName(e.target.value)} placeholder="My database stack" autoFocus />
          </Field>
          <Field label="Lifetime" hint="The stack and its containers are torn down when this elapses.">
            <select className={inputCls} value={ttl} onChange={(e) => setTtl(e.target.value)}>
              {TTL_OPTIONS.map((t) => (
                <option key={t.id} value={t.id}>{t.label}</option>
              ))}
            </select>
          </Field>
          <div className="flex justify-end gap-2 pt-1">
            <Button type="button" variant="ghost" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={busy}>{busy ? 'Creating…' : 'Create'}</Button>
          </div>
        </form>
      </div>
    </div>,
    document.body,
  )
}

// -------------------------------------------------------------- editor view

function StackEditor({ stackId, onBack }) {
  const [stack, setStack] = useState(null)
  const [error, setError] = useState('')
  const [nodes, setNodes] = useState([])
  const [edges, setEdges] = useState([])
  const [frames, setFrames] = useState([])
  const [view, setView] = useState({ x: 40, y: 20, z: 1 })
  // Node palette: docked to the left by default; can be undocked into a floating,
  // resizable panel (drag by its header, resize via the corner handle) and re-docked.
  const [paletteDocked, setPaletteDocked] = useState(true)
  const [palettePos, setPalettePos] = useState({ x: 24, y: 24 })
  const [selected, setSelected] = useState(null)
  const [menu, setMenu] = useState(null)
  const [connect, setConnect] = useState(null)
  const [linkPrompt, setLinkPrompt] = useState(null) // ProxySQL↔ProxySQL: choose flow direction
  const [replPrompt, setReplPrompt] = useState(null) // member↔member: choose replication direction/type
  const [confirmDel, setConfirmDel] = useState(null) // confirm deleting a deployed node/cluster
  const [saveState, setSaveState] = useState('saved') // saved | saving
  const [deployments, setDeployments] = useState([])
  const [issues, setIssues] = useState(null) // validate results panel
  const [busy, setBusy] = useState('') // 'validate' | 'deploy' | ''
  const [configNode, setConfigNode] = useState(null) // node whose profile is shown
  const [deployPanel, setDeployPanel] = useState('hidden') // 'open' | 'min' | 'hidden'
  const { openTerminal } = useTerminals()

  const wrapRef = useRef(null)
  const dragRef = useRef(null)
  const counter = useRef(0)
  const uid = (p) => `${p}-${Date.now().toString(36)}-${++counter.current}`

  const refs = useRef({})
  refs.current = { nodes, edges, frames, view }
  const stackRef = useRef(null)
  stackRef.current = stack
  const lastSaved = useRef('')

  // load
  useEffect(() => {
    let alive = true
    stackApi.get(stackId).then((s) => {
      if (!alive) return
      setStack(s)
      setDeployments(s.deployments || [])
      const d = s.design || {}
      const nz = d.nodes || []
      const ez = d.edges || []
      const fz = d.frames || []
      const vw = d.view || { x: 40, y: 20, z: 1 }
      setNodes(nz)
      setEdges(ez)
      setFrames(fz)
      setView(vw)
      lastSaved.current = JSON.stringify({ nodes: nz, edges: ez, frames: fz, view: vw })
    }).catch((err) => setError(err.message))
    return () => { alive = false }
  }, [stackId])

  // poll deployment state (does NOT touch the local design while editing)
  useEffect(() => {
    const t = setInterval(async () => {
      try {
        const s = await stackApi.get(stackId)
        setDeployments(s.deployments || [])
        setStack((prev) => (prev ? { ...prev, status: s.status } : prev))
      } catch {
        // ignore transient errors
      }
    }, 3000)
    return () => clearInterval(t)
  }, [stackId])

  const depByNode = {}
  for (const d of deployments) depByNode[d.nodeId] = d

  // While a deployment is in progress the node set is frozen: no adding or removing
  // nodes (the server rejects it too). Option/position edits stay live. Cleared once
  // every node finishes provisioning.
  const deploying = busy === 'deploy' || deployments.some((d) => d.state === 'pending' || d.state === 'provisioning')

  // auto-open the deployment console while anything is provisioning, but never
  // override the user's minimized choice.
  useEffect(() => {
    if (deployments.some((d) => d.state === 'pending' || d.state === 'provisioning')) {
      setDeployPanel((p) => (p === 'hidden' ? 'open' : p))
    }
  }, [deployments])

  // debounced autosave — only when the design actually differs from the last
  // saved snapshot (so the 3s status poll never triggers a save).
  useEffect(() => {
    if (!stackRef.current) return
    const cur = JSON.stringify({ nodes, edges, frames, view })
    if (cur === lastSaved.current) return
    setSaveState('saving')
    const t = setTimeout(async () => {
      try {
        await stackApi.update(stackRef.current.id, stackRef.current.name, { nodes, edges, frames, view })
        lastSaved.current = cur
      } catch { /* keep dirty; will retry on next change */ }
      setSaveState('saved')
    }, 600)
    return () => clearTimeout(t)
  }, [nodes, edges, frames, view])

  const getWorld = useCallback((cx, cy) => {
    const rect = wrapRef.current.getBoundingClientRect()
    return screenToWorld(rect, refs.current.view, cx, cy)
  }, [])

  // rectOf resolves a connection endpoint id to its rectangle — a free node uses
  // the fixed node size, a PXC cluster frame its own geometry.
  const rectOf = useCallback((id) => {
    const n = refs.current.nodes.find((x) => x.id === id)
    // A cluster member (inside a frame) uses the small member-card geometry; a free
    // node uses the full node size.
    if (n) return n.frameId ? { x: n.x, y: n.y, w: PXC_NODE_W, h: PXC_NODE_H } : { x: n.x, y: n.y, w: NODE_W, h: NODE_H }
    const f = refs.current.frames.find((x) => x.id === id)
    if (f) return { x: f.x, y: f.y, w: f.w, h: f.h }
    return null
  }, [])

  // Endpoints that expose connection ports: free nodes whose type opts in
  // (def.ports), and PXC cluster frames. PXC member nodes connect via their frame.
  function hitPort(world, excludeId) {
    let best = null
    let bestD = SNAP
    const consider = (id, r) => {
      if (id === excludeId) return
      for (const port of PORTS) {
        const d = dist(world, portPoint(r, port))
        if (d < bestD) { bestD = d; best = { id, port } }
      }
    }
    for (const n of refs.current.nodes) {
      if (n.frameId) {
        // PXC and Percona Server replication members expose ports for cross-cluster
        // replication links; other members (ProxySQL, InnoDB) do not.
        if (n.type === 'pxc' || n.type === 'mysql') consider(n.id, { x: n.x, y: n.y, w: PXC_NODE_W, h: PXC_NODE_H })
        continue
      }
      if (!NODE_TYPES[n.type]?.ports) continue
      consider(n.id, { x: n.x, y: n.y, w: NODE_W, h: NODE_H })
    }
    for (const f of refs.current.frames) {
      if (f.type === 'pxc' || f.type === 'proxysql' || f.type === 'mysql' || f.type === 'patroni' || f.type === 'repmgr' || f.type === 'spock') consider(f.id, { x: f.x, y: f.y, w: f.w, h: f.h })
    }
    return best
  }

  // global pointer move/up
  useEffect(() => {
    function onMove(e) {
      const d = dragRef.current
      if (!d) return
      if (d.kind === 'pan') {
        setView((v) => ({ ...v, x: d.ox + (e.clientX - d.sx), y: d.oy + (e.clientY - d.sy) }))
        return
      }
      if (d.kind === 'palette') {
        setPalettePos({ x: Math.max(0, d.ox + (e.clientX - d.sx)), y: Math.max(0, d.oy + (e.clientY - d.sy)) })
        return
      }
      const w = getWorld(e.clientX, e.clientY)
      if (d.kind === 'node') {
        setNodes((ns) => ns.map((n) => (n.id === d.id ? { ...n, x: w.x + d.offx, y: w.y + d.offy } : n)))
      } else if (d.kind === 'frame') {
        const nx = w.x + d.offx, ny = w.y + d.offy
        const frame = refs.current.frames.find((f) => f.id === d.id)
        setFrames((fs) => fs.map((f) => (f.id === d.id ? { ...f, x: nx, y: ny } : f)))
        if (frame) {
          const mine = refs.current.nodes.filter((n) => n.frameId === d.id)
          const laid = new Map(relayoutFrame({ ...frame, x: nx, y: ny }, mine).nodes.map((n) => [n.id, n]))
          setNodes((ns) => ns.map((n) => laid.get(n.id) || n))
        }
      } else if (d.kind === 'connect') {
        const tgt = hitPort(w, d.fromId)
        const src = portPoint(rectOf(d.fromId), d.fromPort)
        const to = tgt ? portPoint(rectOf(tgt.id), tgt.port) : w
        d.lastTarget = tgt
        setConnect({ from: src, to, targetId: tgt?.id ?? null, targetPort: tgt?.port ?? null })
      }
    }
    function onUp() {
      const d = dragRef.current
      if (d?.kind === 'connect') {
        const t = d.lastTarget
        if (t && t.id !== d.fromId) {
          tryConnect({ node: d.fromId, port: d.fromPort }, { node: t.id, port: t.port })
        }
      }
      dragRef.current = null
      setConnect(null)
    }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => {
      removeEventListener('pointermove', onMove)
      removeEventListener('pointerup', onUp)
    }
  }, [getWorld, rectOf])

  // wheel zoom
  useEffect(() => {
    const el = wrapRef.current
    if (!el) return
    function onWheel(e) {
      e.preventDefault()
      const rect = el.getBoundingClientRect()
      setView((v) => zoomAt(v, e.clientX - rect.left, e.clientY - rect.top, e.deltaY))
    }
    el.addEventListener('wheel', onWheel, { passive: false })
    return () => el.removeEventListener('wheel', onWheel)
    // Re-run once the canvas actually mounts: StackEditor renders a "Loading…"
    // placeholder while stack is null, so wrapRef.current is null on first mount and
    // the listener would otherwise never attach (breaking wheel zoom).
  }, [stack])

  // delete key
  useEffect(() => {
    function onKey(e) {
      if (e.key === 'Escape') setMenu(null)
      if (e.key !== 'Delete') return
      const t = e.target
      if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return
      if (selected) { e.preventDefault(); deleteSelected() }
    }
    addEventListener('keydown', onKey)
    return () => removeEventListener('keydown', onKey)
  })

  // interactions
  function startPan(e) {
    if (e.button !== 0) return
    setSelected(null)
    setMenu(null)
    dragRef.current = { kind: 'pan', sx: e.clientX, sy: e.clientY, ox: view.x, oy: view.y }
  }
  function startNode(e, id) {
    if (e.button !== 0) return
    e.stopPropagation()
    setSelected({ kind: 'node', id })
    setMenu(null)
    const w = getWorld(e.clientX, e.clientY)
    const n = nodes.find((x) => x.id === id)
    dragRef.current = { kind: 'node', id, offx: n.x - w.x, offy: n.y - w.y }
  }
  function startFrame(e, id) {
    if (e.button !== 0) return
    e.stopPropagation()
    setSelected({ kind: 'frame', id })
    setMenu(null)
    const w = getWorld(e.clientX, e.clientY)
    const f = frames.find((x) => x.id === id)
    dragRef.current = { kind: 'frame', id, offx: f.x - w.x, offy: f.y - w.y }
  }
  function selectFrameNode(e, id) {
    if (e.button !== 0) return
    e.stopPropagation()
    setSelected({ kind: 'node', id })
    setMenu(null)
  }
  function startConnect(e, ownerId, port) {
    if (e.button !== 0) return
    e.stopPropagation()
    setMenu(null)
    const src = portPoint(rectOf(ownerId), port)
    dragRef.current = { kind: 'connect', fromId: ownerId, fromPort: port, lastTarget: null }
    setConnect({ from: src, to: src, targetId: null, targetPort: null })
  }
  function openMenu(e, id) {
    e.preventDefault()
    e.stopPropagation()
    setSelected({ kind: 'node', id })
    setMenu({ x: e.clientX, y: e.clientY, id })
  }

  // --- association links (read refs.current so they're correct when called from
  // the captured pointer-up handler) ---
  // endpointKind classifies a connectable endpoint:
  //   'pxc'           — PXC cluster frame (source only)
  //   'proxysql'      — standalone ProxySQL node (1 incoming, many outgoing)
  //   'proxysql-frame'— ProxySQL cluster frame (1 incoming from PXC, no outgoing)
  // Member nodes inside a frame are not linkable (no ports).
  // 'backend' = a PXC or MySQL cluster frame (source only); 'proxysql' = standalone
  // ProxySQL node; 'proxysql-frame' = ProxySQL cluster frame.
  // 'replmember' = a PXC or Percona Server replication member node (a source/replica
  // for a cross-cluster replication link).
  function endpointKind(id) {
    const n = refs.current.nodes.find((x) => x.id === id)
    if (n) {
      if (n.type === 'proxysql' && !n.frameId) return 'proxysql'
      if (n.type === 'haproxy' && !n.frameId) return 'haproxy'
      if ((n.type === 'pxc' || n.type === 'mysql') && n.frameId) return 'replmember'
      return null
    }
    const f = refs.current.frames.find((x) => x.id === id)
    if (f) {
      if (f.type === 'pxc' || f.type === 'mysql') return 'backend'
      if (f.type === 'proxysql') return 'proxysql-frame'
      if (f.type === 'patroni') return 'patroni'
      return null
    }
    return null
  }
  // createFlow adds a directed edge from→to (arrow at the destination). The
  // destination may have at most ONE incoming flow; a PXC frame source may have at
  // most ONE outgoing flow (opts.singleOutgoing). Rejected (no arrow) otherwise.
  function createFlow(fromEnd, toEnd, opts = {}) {
    const E = refs.current.edges
    if (E.some((ed) => ed.to.node === toEnd.node)) return // destination already receives
    if (opts.singleOutgoing && E.some((ed) => ed.from.node === fromEnd.node)) return // source already sends
    if (E.some((ed) => (ed.from.node === fromEnd.node && ed.to.node === toEnd.node) || (ed.from.node === toEnd.node && ed.to.node === fromEnd.node))) return
    const id = uid('e')
    setEdges((es) => [...es, { id, from: fromEnd, to: toEnd, type: 'directional' }])
    setSelected({ kind: 'edge', id })
  }
  // tryConnect applies the association rules to a dropped connection.
  function tryConnect(e1, e2) {
    const k1 = endpointKind(e1.node)
    const k2 = endpointKind(e2.node)
    if (!k1 || !k2) return
    // No second link between the same pair.
    if (refs.current.edges.some((ed) => (ed.from.node === e1.node && ed.to.node === e2.node) || (ed.from.node === e2.node && ed.to.node === e1.node))) return
    const isProxyDest = (k) => k === 'proxysql' || k === 'proxysql-frame'
    // PXC/MySQL backend frame → ProxySQL node/cluster frame (frame is always the
    // source, max 1 outgoing).
    if (k1 === 'backend' && isProxyDest(k2)) return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'backend' && isProxyDest(k1)) return createFlow(e2, e1, { singleOutgoing: true })
    // Patroni cluster frame → HAProxy node (frame is the source, max 1 outgoing;
    // HAProxy takes a single incoming via the createFlow dest guard).
    if (k1 === 'patroni' && k2 === 'haproxy') return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'patroni' && k1 === 'haproxy') return createFlow(e2, e1, { singleOutgoing: true })
    // PXC cluster frame → HAProxy node. HAProxy fronts exactly one cluster (Patroni OR
    // PXC) — its single incoming (createFlow dest guard) enforces the mutual exclusivity.
    // Only PXC frames qualify here ('backend' also covers MySQL-replication frames).
    const isPXCFrame = (id) => refs.current.frames.find((x) => x.id === id)?.type === 'pxc'
    if (k1 === 'backend' && k2 === 'haproxy' && isPXCFrame(e1.node)) return createFlow(e1, e2, { singleOutgoing: true })
    if (k2 === 'backend' && k1 === 'haproxy' && isPXCFrame(e2.node)) return createFlow(e2, e1, { singleOutgoing: true })
    // ProxySQL node ↔ ProxySQL node: ask which way the data flows.
    if (k1 === 'proxysql' && k2 === 'proxysql') { setLinkPrompt({ e1, e2 }); return }
    // Cluster member ↔ cluster member (PXC/Percona Server, different frames): a
    // cross-cluster replication link. Ask for async direction or bidirectional.
    if (k1 === 'replmember' && k2 === 'replmember') {
      const n1 = refs.current.nodes.find((x) => x.id === e1.node)
      const n2 = refs.current.nodes.find((x) => x.id === e2.node)
      if (!n1 || !n2 || n1.frameId === n2.frameId) return // same cluster — already replicating
      setReplPrompt({ e1, e2 })
      return
    }
    // Everything else (frame↔frame, ProxySQL cluster frame as source, node↔cluster
    // frame, self) is not allowed.
  }
  // createReplEdge adds a cross-cluster replication link. mode "async" → From is the
  // source, To the replica (arrow at the replica). mode "bidir" → both replicate
  // from each other (double-headed). One link per node pair (tryConnect rejects a
  // second); change direction/type later from the link's Properties panel.
  function createReplEdge(fromEnd, toEnd, mode) {
    const id = uid('e')
    setEdges((es) => [...es, { id, from: fromEnd, to: toEnd, type: mode }])
    setSelected({ kind: 'edge', id })
  }

  // mutations
  const patchNode = (id, patch) => setNodes((ns) => ns.map((n) => (n.id === id ? { ...n, ...patch } : n)))
  const patchFrame = (id, patch) => setFrames((fs) => fs.map((f) => (f.id === id ? { ...f, ...patch } : f)))
  const patchEdge = (id, patch) => setEdges((es) => es.map((e) => (e.id === id ? { ...e, ...patch } : e)))
  // askDelete opens the confirmation modal (used before destroying a *deployed*
  // node/cluster, whose containers + volumes get torn down in real time).
  function askDelete(kind, label, onConfirm, count) {
    setConfirmDel({ kind, label, count, onConfirm })
  }
  function deleteNode(id) {
    if (deploying) return
    const node = nodes.find((n) => n.id === id)
    // PS MongoDB sharded-cluster topology is fixed: members can't be removed
    // individually (delete the whole frame to remove the cluster).
    if (node?.type === 'psmdb') return
    // A deployed node has live containers/volumes — confirm before deleting.
    if (depByNode[id]) { askDelete('node', node?.label || 'node', () => doDeleteNode(id)); return }
    doDeleteNode(id)
  }
  function doDeleteNode(id) {
    // A PXC member belongs to a frame: re-lay the frame after removing it (and
    // drop the frame entirely if it was the last node), so the menu/manager
    // delete behaves like the frame's own remove control.
    const node = nodes.find((n) => n.id === id)
    if (node?.frameId) {
      const siblings = nodes.filter((n) => n.frameId === node.frameId)
      if (siblings.length <= 1) { doDeleteFrame(node.frameId); return }
      const r = relayout(node.frameId, frames, nodes.filter((n) => n.id !== id))
      setFrames(r.frames)
      setNodes(r.nodes)
      // Drop any replication links attached to the removed member.
      setEdges((es) => es.filter((e) => e.from.node !== id && e.to.node !== id))
      setSelected((s) => (s?.kind === 'node' && s.id === id ? { kind: 'frame', id: node.frameId } : s))
      return
    }
    setNodes((ns) => ns.filter((n) => n.id !== id))
    setEdges((es) => es.filter((e) => e.from.node !== id && e.to.node !== id))
    setSelected((s) => (s?.kind === 'node' && s.id === id ? null : s))
  }
  function deleteEdge(id) {
    setEdges((es) => es.filter((e) => e.id !== id))
    setSelected((s) => (s?.kind === 'edge' && s.id === id ? null : s))
  }
  function deleteFrame(id) {
    if (deploying) return
    // Confirm when the cluster has deployed members (their containers + volumes go).
    const deployedMembers = nodes.filter((n) => n.frameId === id && depByNode[n.id]).length
    if (deployedMembers > 0) {
      const label = frames.find((f) => f.id === id)?.label || 'cluster'
      askDelete('frame', label, () => doDeleteFrame(id), deployedMembers)
      return
    }
    doDeleteFrame(id)
  }
  function doDeleteFrame(id) {
    const memberIds = new Set(nodes.filter((n) => n.frameId === id).map((n) => n.id))
    setNodes((ns) => ns.filter((n) => n.frameId !== id))
    setFrames((fs) => fs.filter((f) => f.id !== id))
    // Drop any association lines attached to the frame (or its member nodes).
    setEdges((es) => es.filter((e) => e.from.node !== id && e.to.node !== id && !memberIds.has(e.from.node) && !memberIds.has(e.to.node)))
    setSelected((s) => (s && (s.id === id) ? null : s))
  }
  function deleteSelected() {
    if (selected?.kind === 'node') deleteNode(selected.id)
    else if (selected?.kind === 'edge') deleteEdge(selected.id)
    else if (selected?.kind === 'frame') deleteFrame(selected.id)
  }

  // --- PXC cluster frame operations ---
  // Re-lay a frame's member nodes (positions derive from the frame geometry).
  function relayout(frameId, framesArr, nodesArr) {
    const frame = framesArr.find((f) => f.id === frameId)
    if (!frame) return { frames: framesArr, nodes: nodesArr }
    const mine = nodesArr.filter((n) => n.frameId === frameId)
    const others = nodesArr.filter((n) => n.frameId !== frameId)
    const r = relayoutFrame(frame, mine)
    return {
      frames: framesArr.map((f) => (f.id === frameId ? r.frame : f)),
      nodes: [...others, ...r.nodes],
    }
  }
  function addPXCCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'pxc', label: nextClusterName(frames), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', pxcMajor: '8.0', pxcVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false, gtid: true,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'pxc').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextPXCName(used)
      used.add(name)
      newNodes.push({ id: uid('pxc'), type: 'pxc', label: name, frameId: fid, role: 'regular', exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function addPXCNode(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'pxc').map((n) => n.label))
    const name = nextPXCName(used)
    const newNode = { id: uid('pxc'), type: 'pxc', label: name, frameId, role: 'regular', exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
    const r = relayout(frameId, frames, [...nodes, newNode])
    setFrames(r.frames)
    setNodes(r.nodes)
  }
  function newProxySQLMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'proxysql').map((n) => n.label))
    return { id: uid('proxysql'), type: 'proxysql', label: nextMemberName(used, 'proxysql'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  function addProxySQLCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'proxysql', label: nextNamedCluster(frames, 'proxysql-cluster'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', proxysqlMajor: '2', proxysqlVersion: '',
      mode: 'singlewrite', pmmNodeId: '', useProxy: false,
    }
    const used = new Set(nodes.filter((n) => n.type === 'proxysql').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'proxysql')
      used.add(name)
      newNodes.push({ id: uid('proxysql'), type: 'proxysql', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function newMySQLMember(frameId, role) {
    const used = new Set(nodes.filter((n) => n.type === 'mysql').map((n) => n.label))
    return { id: uid('mysql'), type: 'mysql', label: nextMemberName(used, 'mysql'), frameId, role: role || 'secondary', exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  function addMySQLCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'mysql', label: nextNamedCluster(frames, 'psrepl'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', psMajor: '8.0', psVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false, gtid: true, replMode: 'async',
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'mysql').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'mysql')
      used.add(name)
      newNodes.push({ id: uid('mysql'), type: 'mysql', label: name, frameId: fid, role: i === 0 ? 'primary' : 'secondary', exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function newInnoDBMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'innodb').map((n) => n.label))
    return { id: uid('innodb'), type: 'innodb', label: nextMemberName(used, 'innodb'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  function addInnoDBCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'innodb', label: nextNamedCluster(frames, 'innodb'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', pdpsRepo: '', replMode: 'innodbcluster',
      rootPassword: '', pmmNodeId: '', useProxy: false, mysqlRouter: true,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'innodb').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'innodb')
      used.add(name)
      newNodes.push({ id: uid('innodb'), type: 'innodb', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  // psmdbMembers builds the member nodes for a PS MongoDB sharded cluster of the
  // given setup, always 1 mongos + 3 shards + a config-server RS:
  //   standard → 3-node config RS + 3 shards × 3-node RS (13 nodes)
  //   minimum  → 1 config server + 3 single-node shard RS    (5 nodes)
  // Member labels: mongos (role mongos), cfgNN (role config), sNrM (role shard).
  function psmdbMembers(fid, setup) {
    const rs = setup === 'minimum' ? 1 : 3
    const cfgN = setup === 'minimum' ? 1 : 3
    const mk = (label, role, shard, slot) => {
      const nd = { id: uid('psmdb'), type: 'psmdb', label, frameId: fid, role, _slot: slot, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
      if (shard !== undefined) nd.shard = shard
      return nd
    }
    const out = []
    out.push(mk('mongos', 'mongos', undefined, 0)) // the "mongosh" node
    for (let i = 0; i < cfgN; i++) out.push(mk(`cfg${i + 1}`, 'config', undefined, i)) // config RS
    for (let s = 0; s < 3; s++) {
      for (let r = 0; r < rs; r++) out.push(mk(`s${s}r${r + 1}`, 'shard', s, r)) // shard RS
    }
    return out
  }
  // addMongoDBCluster builds a PS MongoDB sharded cluster frame. Topology is fixed
  // per setup (no add/remove); the setup can be switched in the frame form before
  // deploy.
  function addMongoDBCluster(setup = 'standard') {
    if (!nodes.some((n) => n.type === 'intranet')) return
    setup = setup === 'minimum' ? 'minimum' : 'standard'
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'psmdb', label: nextNamedCluster(frames, 'psmdb'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', psmdbMajor: '8.0', psmdbVersion: '',
      psmdbSetup: setup, rootPassword: '', pmmNodeId: '', useProxy: false,
      enablePBM: false, seaweedfsNodeId: '',
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...psmdbMembers(fid, setup)])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  // rebuildMongoCluster swaps a PS MongoDB frame's members for a different setup
  // (standard ↔ minimum). Only allowed before deploy; replication links never
  // attach to psmdb members, so none need pruning.
  function rebuildMongoCluster(frameId, setup) {
    const frame = frames.find((f) => f.id === frameId)
    if (!frame || frame.type !== 'psmdb') return
    const others = nodes.filter((n) => n.frameId !== frameId)
    const r = relayout(frameId, frames.map((f) => (f.id === frameId ? { ...f, psmdbSetup: setup } : f)), [...others, ...psmdbMembers(frameId, setup)])
    setFrames(r.frames)
    setNodes(r.nodes)
  }
  function newPSMRSMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'psmrs').map((n) => n.label))
    return { id: uid('psmrs'), type: 'psmrs', label: nextMemberName(used, 'psmrs'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  // addMongoRSCluster builds a PS MongoDB replica-set frame with 3 members
  // (resizable 1–9). Members all run mongod in one replica set; an admin user is
  // created on the elected primary.
  function addMongoRSCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'psmrs', label: nextNamedCluster(frames, 'psmrs'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', psmdbMajor: '8.0', psmdbVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false,
      enablePBM: false, seaweedfsNodeId: '',
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'psmrs').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'psmrs')
      used.add(name)
      newNodes.push({ id: uid('psmrs'), type: 'psmrs', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function newPatroniMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'patroni').map((n) => n.label))
    return { id: uid('patroni'), type: 'patroni', label: nextMemberName(used, 'patroni'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  // addPatroniCluster builds a Patroni PostgreSQL cluster frame with 3 members
  // (resizable 3–7). Each member co-locates PostgreSQL + Patroni + an etcd member;
  // one node is elected leader and the rest stream as replicas.
  function addPatroniCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'patroni', label: nextNamedCluster(frames, 'patroni-cluster'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', pgMajor: '16', pgVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false,
      usePgBackRest: false, seaweedfsNodeId: '',
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'patroni').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'patroni')
      used.add(name)
      newNodes.push({ id: uid('patroni'), type: 'patroni', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function newRepmgrMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'repmgr').map((n) => n.label))
    return { id: uid('repmgr'), type: 'repmgr', label: nextMemberName(used, 'repmgr'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  // addRepmgrCluster builds a repmgr PostgreSQL cluster frame with 3 members
  // (resizable 3–7). Streaming replication managed by repmgr; repmgrd does failover.
  function addRepmgrCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'repmgr', label: nextNamedCluster(frames, 'repmgr-cluster'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', pgMajor: '16', pgVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false,
      useBarman: false, seaweedfsNodeId: '',
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'repmgr').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'repmgr')
      used.add(name)
      newNodes.push({ id: uid('repmgr'), type: 'repmgr', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function newSpockMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'spock').map((n) => n.label))
    return { id: uid('spock'), type: 'spock', label: nextMemberName(used, 'spock'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  // addSpockCluster builds a Spock PostgreSQL cluster frame with 3 members (resizable
  // 2–7). Every member is writable — full-mesh active-active via pgEdge Spock.
  function addSpockCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'spock', label: nextNamedCluster(frames, 'spock-cluster'), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', pgMajor: '16', pgVersion: '',
      pmmNodeId: '', useProxy: false,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'spock').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'spock')
      used.add(name)
      newNodes.push({ id: uid('spock'), type: 'spock', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function newValkeyMember(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'valkeycluster').map((n) => n.label))
    return { id: uid('valkey'), type: 'valkeycluster', label: nextMemberName(used, 'valkey'), frameId, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
  }
  // addValkeyCluster builds a Valkey Cluster frame with 3 members (resizable 3–7,
  // all-master shards via valkey-cli --cluster create).
  function addValkeyCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'valkeycluster', label: nextNamedCluster(frames, 'valkey-cluster'), x: fx, y: fy, w: 0, h: 0,
      rootPassword: '', pmmNodeId: '', useLdap: false,
    }
    const used = new Set(nodes.filter((n) => n.type === 'valkeycluster').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextMemberName(used, 'valkey')
      used.add(name)
      newNodes.push({ id: uid('valkey'), type: 'valkeycluster', label: name, frameId: fid, exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  // Frame +/- buttons dispatch by frame type.
  function addFrameMember(frame) {
    if (deploying) return
    if (frame.type === 'proxysql') {
      const r = relayout(frame.id, frames, [...nodes, newProxySQLMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'mysql') {
      // Added members are secondaries (the single primary is kept).
      const r = relayout(frame.id, frames, [...nodes, newMySQLMember(frame.id, 'secondary')])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'innodb') {
      const r = relayout(frame.id, frames, [...nodes, newInnoDBMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'psmrs') {
      if (nodes.filter((n) => n.frameId === frame.id).length >= 9) return // max 9 members
      const r = relayout(frame.id, frames, [...nodes, newPSMRSMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'patroni') {
      if (nodes.filter((n) => n.frameId === frame.id).length >= 7) return // max 7 (etcd quorum)
      const r = relayout(frame.id, frames, [...nodes, newPatroniMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'repmgr') {
      if (nodes.filter((n) => n.frameId === frame.id).length >= 7) return // max 7
      const r = relayout(frame.id, frames, [...nodes, newRepmgrMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'spock') {
      if (nodes.filter((n) => n.frameId === frame.id).length >= 7) return // max 7
      const r = relayout(frame.id, frames, [...nodes, newSpockMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else if (frame.type === 'valkeycluster') {
      if (nodes.filter((n) => n.frameId === frame.id).length >= 7) return // max 7
      const r = relayout(frame.id, frames, [...nodes, newValkeyMember(frame.id)])
      setFrames(r.frames)
      setNodes(r.nodes)
    } else {
      addPXCNode(frame.id)
    }
  }
  function removePXCNode(frameId) {
    if (deploying) return
    const mine = nodes.filter((n) => n.frameId === frameId)
    if (mine.length <= 1) return // keep at least one node
    // Patroni/repmgr need ≥3 members: never drop below 3.
    const frame = frames.find((f) => f.id === frameId)
    if ((frame?.type === 'patroni' || frame?.type === 'repmgr' || frame?.type === 'valkeycluster') && mine.length <= 3) return
    if (frame?.type === 'spock' && mine.length <= 2) return // Spock keeps ≥2 members
    const target = mine[mine.length - 1]
    // Confirm when the member being dropped is deployed (its container + volume go).
    if (depByNode[target.id]) { askDelete('node', target.label || 'node', () => removePXCNodeById(frameId, target.id)); return }
    removePXCNodeById(frameId, target.id)
  }
  function removePXCNodeById(frameId, id) {
    if (deploying) return
    const mine = nodes.filter((n) => n.frameId === frameId)
    if (mine.length <= 1) return // keep at least one node
    const r = relayout(frameId, frames, nodes.filter((n) => n.id !== id))
    setFrames(r.frames)
    setNodes(r.nodes)
    setEdges((es) => es.filter((e) => e.from.node !== id && e.to.node !== id))
    setSelected((s) => (s?.kind === 'node' && s.id === id ? { kind: 'frame', id: frameId } : s))
  }
  function addNode(type) {
    const def = NODE_TYPES[type]
    if (def.singleton && nodes.some((n) => n.type === type)) return
    // The Intranet is required first — it provides DNS/mail/LDAP/CA for the stack.
    if (type !== 'intranet' && !nodes.some((n) => n.type === 'intranet')) return
    const id = uid(type)
    const x = (-view.x + 220) / view.z
    const y = (-view.y + 160) / view.z
    setNodes((ns) => [...ns, { id, type, x, y, label: nextLabel(type, ns), os: def.osOptions[0].id, arch: 'amd64', ...(def.defaults || {}) }])
    setSelected({ kind: 'node', id })
  }

  const upsertDep = (ds, d) => {
    const next = ds.filter((x) => x.nodeId !== d.nodeId)
    next.push(d)
    return next
  }

  // Flush the debounced design save so validate/deploy act on exactly what's on
  // the canvas (otherwise a just-toggled option — e.g. the cert checkbox — may not
  // have hit the server yet and a stale design gets deployed).
  async function saveNow() {
    if (!stackRef.current) return
    const cur = JSON.stringify({ nodes, edges, frames, view })
    if (cur === lastSaved.current) return
    await stackApi.update(stackRef.current.id, stackRef.current.name, { nodes, edges, frames, view })
    lastSaved.current = cur
    setSaveState('saved')
  }

  async function runValidate() {
    setBusy('validate')
    try {
      await saveNow()
      const r = await stackApi.validate(stack.id)
      setIssues(r.issues || [])
    } catch (err) {
      setIssues([{ level: 'error', message: err.message }])
    } finally {
      setBusy('')
    }
  }

  async function runDeploy() {
    setBusy('deploy')
    setIssues(null)
    try {
      await saveNow()
      const v = await stackApi.validate(stack.id)
      if (!v.ok) {
        setIssues(v.issues)
        return
      }
      const r = await stackApi.deploy(stack.id)
      setDeployments(r.deployments || [])
      setStack((p) => ({ ...p, status: 'deployed' }))
      setDeployPanel('open')
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy('')
    }
  }

  async function runDestroy() {
    setBusy('destroy')
    setIssues(null)
    try {
      await stackApi.destroy(stack.id)
      setDeployments([])
      setStack((p) => ({ ...p, status: 'draft' }))
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy('')
    }
  }

  async function nodeAction(nid, action) {
    try {
      const d = await stackApi.nodeAction(stack.id, nid, action)
      setDeployments((ds) => upsertDep(ds, d))
    } catch (err) {
      setError(err.message)
    }
  }

  async function showConfig(nid) {
    try {
      setConfigNode(await stackApi.getNode(stack.id, nid))
    } catch (err) {
      setError(err.message)
    }
  }

  function nodeMenuActions(id) {
    const dep = depByNode[id]
    const actions = []
    if (dep) {
      actions.push({ label: 'View config / profile', fn: () => showConfig(id) })
      if (dep.state === 'running') {
        const node = nodes.find((n) => n.id === id)
        if (node?.type === 'pmm') {
          // The PMM image runs as the unprivileged pmm user, so a plain exec is the pmm
          // console; root needs -u 0.
          actions.push({ label: 'Enter root console', fn: () => openTerminal({ stackId: stack.id, nodeId: id, title: `${node.label} · root`, user: '0' }) })
          actions.push({ label: 'Enter PMM console', fn: () => openTerminal({ stackId: stack.id, nodeId: id, title: `${node.label} · pmm` }) })
        } else {
          actions.push({ label: 'Enter root console', fn: () => openTerminal({ stackId: stack.id, nodeId: id, title: `${node?.label || 'node'} · root` }) })
        }
        actions.push({ label: 'Stop', fn: () => nodeAction(id, 'stop') })
        actions.push({ label: 'Restart', fn: () => nodeAction(id, 'restart') })
      } else if (dep.state === 'stopped' || dep.state === 'error') {
        actions.push({ label: 'Start', fn: () => nodeAction(id, 'start') })
      }
      actions.push({ sep: true })
    }
    // PS MongoDB members are part of a fixed topology — no individual delete.
    if (nodes.find((n) => n.id === id)?.type !== 'psmdb') {
      actions.push({ label: 'Delete node', danger: true, fn: () => deleteNode(id) })
    }
    return actions
  }

  if (error) {
    return (
      <div className="space-y-3">
        <Button variant="ghost" onClick={onBack}><Icon.ArrowLeft size={16} /> Back</Button>
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{error}</div>
      </div>
    )
  }
  if (!stack) return <div className="py-10 text-center text-muted">Loading…</div>

  const hasIntranet = nodes.some((n) => n.type === 'intranet')

  // Node palette: categorized add-buttons (vertical). Used both docked-left and floating.
  const has = (t) => nodes.some((n) => n.type === t)
  const paletteGroups = [
    { title: 'Core', items: [
      { label: 'Intranet', type: 'intranet', onClick: () => addNode('intranet'), off: hasIntranet },
      { label: 'PMM3', type: 'pmm', onClick: () => addNode('pmm') },
      { label: 'Watchtower', type: 'watchtower', onClick: () => addNode('watchtower'), off: has('watchtower') },
      { label: 'Keycloak', type: 'keycloak', onClick: () => addNode('keycloak'), off: has('keycloak') },
    ] },
    { title: 'MySQL', items: [
      { label: 'PXC Cluster', type: 'pxc', onClick: addPXCCluster },
      { label: 'Percona Server', type: 'ps', onClick: () => addNode('ps') },
      { label: 'PS Replication', type: 'mysql', onClick: addMySQLCluster },
      { label: 'InnoDB / GR', type: 'innodb', onClick: addInnoDBCluster },
    ] },
    { title: 'Load Balancer', items: [
      { label: 'ProxySQL', type: 'proxysql', onClick: () => addNode('proxysql') },
      { label: 'ProxySQL Cluster', type: 'proxysql', onClick: addProxySQLCluster },
      { label: 'HAProxy', type: 'haproxy', onClick: () => addNode('haproxy') },
    ] },
    { title: 'MongoDB', items: [
      { label: 'PSMDB Sharded', type: 'psmdb', onClick: () => addMongoDBCluster() },
      { label: 'PSMDB Replica Set', type: 'psmrs', onClick: addMongoRSCluster },
      { label: 'PSMDB Standalone', type: 'psm', onClick: () => addNode('psm') },
    ] },
    { title: 'PostgreSQL', items: [
      { label: 'PostgreSQL', type: 'pg', onClick: () => addNode('pg') },
      { label: 'Patroni Cluster', type: 'patroni', onClick: addPatroniCluster },
      { label: 'repmgr Cluster', type: 'repmgr', onClick: addRepmgrCluster },
      { label: 'Spock Cluster', type: 'spock', onClick: addSpockCluster },
    ] },
    { title: 'Valkey', items: [
      { label: 'Valkey Cluster', type: 'valkeycluster', onClick: addValkeyCluster },
      { label: 'Valkey', type: 'valkey', onClick: () => addNode('valkey') },
    ] },
    { title: 'Storage & Tools', items: [
      { label: 'SeaweedFS', type: 'seaweedfs', onClick: () => addNode('seaweedfs') },
      { label: 'Ubuntu VNC', type: 'vnc', onClick: () => addNode('vnc'), off: has('vnc') },
    ] },
  ]
  const paletteBody = (
    <div className="min-h-0 flex-1 space-y-3 overflow-y-auto px-2 py-2">
      {deploying && (
        <div className="rounded-md border border-warning/30 bg-warning/10 px-2 py-1.5 text-[11px] leading-snug text-warning">
          Deployment in progress — adding and removing nodes is locked until it finishes.
        </div>
      )}
      {paletteGroups.map((g) => (
        <div key={g.title}>
          <div className="px-1 pb-1 text-[10px] font-semibold uppercase tracking-wide text-muted">{g.title}</div>
          <div className="space-y-1">
            {g.items.map((it) => {
              const disabled = it.off || (it.type !== 'intranet' && !hasIntranet) || deploying
              return (
                <button key={it.label} disabled={disabled} onClick={it.onClick}
                  style={addBtnStyle(it.type)}
                  title={deploying ? 'Locked while deploying' : (!hasIntranet && it.type !== 'intranet' ? 'Add an Intranet node first' : '')}
                  className="flex w-full items-center gap-1.5 rounded-md px-2 py-1 text-xs font-medium shadow-sm disabled:opacity-40">
                  <Icon.Plus size={13} /> <span className="truncate">{it.label}</span>
                </button>
              )
            })}
          </div>
        </div>
      ))}
    </div>
  )
  const paletteHeader = (onToggle, dockLabel, dockIcon, onDrag) => (
    <div className={`flex shrink-0 items-center justify-between border-b px-2 py-1.5 ${onDrag ? 'cursor-move' : ''}`} onPointerDown={onDrag}>
      <span className="text-xs font-semibold">Infrastructure Library</span>
      <button title={dockLabel} onClick={onToggle} className="text-muted hover:text-fg">{dockIcon}</button>
    </div>
  )

  return (
    <div className="flex h-[78vh] gap-4">
      <div className="flex min-w-0 flex-1 flex-col gap-3">
        {/* toolbar */}
        <div className="flex flex-wrap items-center gap-2 rounded-xl border bg-surface px-3 py-2">
          <Button size="sm" variant="ghost" onClick={onBack}><Icon.ArrowLeft size={16} /> Stacks</Button>
          <div className="mx-1 h-5 w-px bg-border" />
          <span className="text-sm font-semibold">{stack.name}</span>
          <Badge tone="primary">{ttlLabel(stack.ttl)}</Badge>
          <Badge tone={STATUS_TONE[stack.status] || 'muted'}>{stack.status}</Badge>
          <div className="mx-1 h-5 w-px bg-border" />
          {paletteDocked && <span className="text-xs text-muted">Add nodes from the Infrastructure Library →</span>}
          {!paletteDocked && (
            <Button size="sm" variant="outline" onClick={() => setPaletteDocked(true)}><Icon.Plus size={15} /> Palette</Button>
          )}
          <div className="mx-1 h-5 w-px bg-border" />
          <Button size="sm" variant="outline" disabled={!!busy} onClick={runValidate}>
            <Icon.Check size={15} /> {busy === 'validate' ? 'Validating…' : 'Validate'}
          </Button>
          <Button size="sm" disabled={!!busy || nodes.length === 0} onClick={runDeploy}>
            <Icon.Arrow size={15} /> {busy === 'deploy' ? 'Deploying…' : 'Deploy'}
          </Button>
          {(deployments.length > 0 || stack.status === 'deployed') && (
            <ConfirmButton size="sm" variant="outline" disabled={!!busy} confirmLabel="Destroy — sure?" onConfirm={runDestroy}>
              <Icon.Trash size={15} /> {busy === 'destroy' ? 'Destroying…' : 'Destroy'}
            </ConfirmButton>
          )}
          <div className="ml-auto flex items-center gap-3">
            <span className="text-xs text-muted">{saveState === 'saving' ? 'Saving…' : 'Saved'}</span>
            <span className="text-xs text-muted">{nodes.length} nodes · {edges.length} links</span>
            <Button size="sm" variant="ghost" onClick={() => setView({ x: 40, y: 20, z: 1 })}>
              <Icon.Move size={15} /> Reset view
            </Button>
          </div>
        </div>

        {issues && (
          <div className="rounded-xl border bg-surface p-3">
            <div className="mb-2 flex items-center justify-between">
              <span className="text-xs font-semibold text-muted">Validation</span>
              <button onClick={() => setIssues(null)} className="text-xs text-muted hover:text-fg">dismiss</button>
            </div>
            <ul className="space-y-1">
              {issues.map((it, i) => (
                <li key={i} className="flex items-center gap-2 text-sm">
                  <Badge tone={it.level === 'error' ? 'danger' : it.level === 'warning' ? 'warning' : 'success'}>{it.level}</Badge>
                  <span className="text-fg">{it.message}</span>
                </li>
              ))}
            </ul>
          </div>
        )}

        {/* canvas + node palette (docked left, or floating) */}
        <div className="flex min-h-0 flex-1 gap-3">
        {paletteDocked && (
          <div className="flex w-[200px] shrink-0 flex-col overflow-hidden rounded-xl border bg-surface">
            {paletteHeader(() => setPaletteDocked(false), 'Undock (float)', <Icon.External size={14} />, null)}
            {paletteBody}
          </div>
        )}
        <div
          ref={wrapRef}
          onPointerDown={startPan}
          onContextMenu={(e) => { e.preventDefault(); setMenu(null) }}
          className="relative flex-1 overflow-hidden rounded-xl border bg-bg"
          style={{ touchAction: 'none' }}
        >
          {!paletteDocked && (
            <div className="absolute z-20 flex flex-col rounded-xl border bg-surface shadow-lg"
              onPointerDown={(e) => e.stopPropagation()}
              style={{ left: palettePos.x, top: palettePos.y, width: 210, height: 380, minWidth: 170, minHeight: 220, resize: 'both', overflow: 'hidden' }}>
              {paletteHeader(() => setPaletteDocked(true), 'Dock left', <Icon.ArrowLeft size={14} />, (e) => { e.stopPropagation(); dragRef.current = { kind: 'palette', sx: e.clientX, sy: e.clientY, ox: palettePos.x, oy: palettePos.y } })}
              {paletteBody}
            </div>
          )}
          <div
            className="pointer-events-none absolute inset-0"
            style={{
              backgroundImage: 'radial-gradient(var(--grid) 1.4px, transparent 1.4px)',
              backgroundSize: `${24 * view.z}px ${24 * view.z}px`,
              backgroundPosition: `${view.x}px ${view.y}px`,
            }}
          />
          <div className="absolute left-0 top-0 origin-top-left" style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.z})` }}>
            <svg className="pointer-events-none absolute left-0 top-0 overflow-visible" width="1" height="1">
              <defs>
                <marker id="stk-arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                  <path d="M0,0 L10,5 L0,10 z" fill="context-stroke" />
                </marker>
              </defs>
              {edges.map((ed) => {
                const r0 = rectOf(ed.from.node)
                const r1 = rectOf(ed.to.node)
                if (!r0 || !r1) return null
                const p0 = portPoint(r0, ed.from.port)
                const p1 = portPoint(r1, ed.to.port)
                const d = edgePath(p0, ed.from.port, p1, ed.to.port)
                const on = selected?.kind === 'edge' && selected.id === ed.id
                const repl = ed.type === 'async' || ed.type === 'bidir'
                // Caption: a cross-cluster replication link, or an association line
                // (any link involving a ProxySQL or HAProxy node, or a ProxySQL cluster frame).
                const proxyNodeEnd = nodes.some((n) => (n.id === ed.from.node || n.id === ed.to.node) && (n.type === 'proxysql' || n.type === 'haproxy'))
                const proxyFrameEnd = frames.some((fr) => (fr.id === ed.from.node || fr.id === ed.to.node) && fr.type === 'proxysql')
                const caption = repl
                  ? (ed.type === 'bidir' ? 'bidirectional replication' : 'async replication')
                  : (proxyNodeEnd || proxyFrameEnd ? 'forwards SQL traffic to' : null)
                return (
                  <g key={ed.id}>
                    <path d={d} fill="none" stroke="transparent" strokeWidth="16" className="pointer-events-auto cursor-pointer"
                      onPointerDown={(e) => { e.stopPropagation(); setSelected({ kind: 'edge', id: ed.id }) }} />
                    <path d={d} fill="none" stroke={on ? 'var(--primary)' : repl ? 'var(--success)' : 'var(--muted)'} strokeWidth={on ? 3 : 2}
                      strokeDasharray={repl ? '7 4' : undefined}
                      markerEnd="url(#stk-arrow)" markerStart={ed.type === 'bidir' ? 'url(#stk-arrow)' : undefined} />
                    {caption && (
                      <text x={(p0.x + p1.x) / 2} y={(p0.y + p1.y) / 2 - 5} textAnchor="middle"
                        style={{ fill: 'var(--muted)', fontSize: '9px', paintOrder: 'stroke', stroke: 'var(--bg)', strokeWidth: 3.5, strokeLinejoin: 'round' }}>
                        {caption}
                      </text>
                    )}
                  </g>
                )
              })}
              {connect && (
                <path d={edgePath(connect.from, 'right', connect.to, 'left')} fill="none" stroke="var(--primary)" strokeWidth="2" strokeDasharray="6 5" />
              )}
            </svg>

            {/* Cluster frames (PXC / ProxySQL), rendered behind nodes with their members */}
            {frames.map((f) => {
              const fdef = NODE_TYPES[f.type] || {}
              const on = selected?.kind === 'frame' && selected.id === f.id
              const kids = nodes.filter((n) => n.frameId === f.id)
              const col = frameColor(f)
              return (
                <div key={f.id} className="group absolute" style={{ left: f.x, top: f.y, width: f.w, height: f.h }}>
                  <div className="absolute inset-0 rounded-xl border-2 border-dashed"
                    style={{ borderColor: on ? 'var(--primary)' : col, background: `color-mix(in srgb, ${col} 7%, transparent)` }} />
                  <div
                    onPointerDown={(e) => startFrame(e, f.id)}
                    onContextMenu={(e) => { e.preventDefault(); e.stopPropagation(); setSelected({ kind: 'frame', id: f.id }) }}
                    className="absolute inset-x-0 top-0 flex cursor-grab items-center gap-2 rounded-t-xl px-2 active:cursor-grabbing"
                    style={{ height: FRAME_TITLE, background: `color-mix(in srgb, ${col} 18%, transparent)` }}
                  >
                    <span style={{ color: col }}>{(Icon[fdef.icon] || Icon.Database)({ size: 15 })}</span>
                    <div className="min-w-0 flex-1 leading-tight">
                      <div className="truncate text-xs font-semibold text-fg">{f.label}</div>
                      <div className="truncate text-[10px] text-muted">{frameVersionLabel(f)} · {kids.length} node{kids.length === 1 ? '' : 's'}</div>
                    </div>
                    {/* PS MongoDB has a fixed topology — no add/remove controls. */}
                    {f.type !== 'psmdb' && (
                      <div className="ml-auto flex items-center gap-0.5">
                        <button title={deploying ? 'Locked while deploying' : 'Add node'} disabled={deploying} onPointerDown={(e) => e.stopPropagation()} onClick={() => addFrameMember(f)}
                          className="rounded px-1.5 text-sm leading-none text-muted hover:bg-surface hover:text-fg disabled:opacity-30 disabled:hover:bg-transparent">+</button>
                        <button title={deploying ? 'Locked while deploying' : 'Remove a node'} disabled={deploying} onPointerDown={(e) => e.stopPropagation()} onClick={() => removePXCNode(f.id)}
                          className="rounded px-1.5 text-sm leading-none text-muted hover:bg-surface hover:text-fg disabled:opacity-30 disabled:hover:bg-transparent">−</button>
                      </div>
                    )}
                  </div>
                  {kids.map((n) => {
                    const non = selected?.kind === 'node' && selected.id === n.id
                    const dep = depByNode[n.id]
                    const arb = n.role === 'arbitrator'
                    const isPrimary = n.role === 'primary'
                    let sub = 'Galera data node'
                    if (f.type === 'proxysql') sub = 'ProxySQL'
                    else if (f.type === 'mysql') sub = isPrimary ? 'Primary' : 'Secondary · read-only'
                    else if (f.type === 'innodb') sub = f.replMode === 'groupreplication' ? 'GR member' : 'Cluster member'
                    else if (f.type === 'psmdb') sub = n.role === 'mongos' ? 'mongos router' : n.role === 'config' ? 'config server' : `shard ${n.shard} member`
                    else if (f.type === 'psmrs') sub = 'replica-set member'
    else if (f.type === 'patroni') sub = 'Patroni node'
                    else if (f.type === 'repmgr') sub = 'PostgreSQL + repmgr'
                    else if (f.type === 'spock') sub = 'PostgreSQL + Spock'
                    else if (f.type === 'valkeycluster') sub = 'Valkey shard'
                    else if (arb) sub = 'Arbitrator · garbd'
                    const barCol = (f.type === 'pxc' && arb) || (f.type === 'mysql' && !isPrimary) ? '#64748b' : col
                    // PXC and Percona Server replication members expose ports for
                    // cross-cluster replication links (the wrapper, not the clipped
                    // card, carries them so they sit outside the rounded border).
                    const canRepl = f.type === 'pxc' || f.type === 'mysql'
                    return (
                      <div key={n.id} className="group absolute"
                        style={{ left: n.x - f.x, top: n.y - f.y, width: PXC_NODE_W, height: PXC_NODE_H }}>
                        <div
                          onPointerDown={(e) => selectFrameNode(e, n.id)}
                          onContextMenu={(e) => openMenu(e, n.id)}
                          className={`absolute inset-0 flex cursor-pointer flex-col overflow-hidden rounded-lg border bg-surface shadow-sm ${non ? 'ring-2 ring-primary' : ''}`}
                        >
                          <div className="h-1 w-full shrink-0" style={{ background: barCol }} />
                          <div className="flex flex-1 flex-col justify-center px-2 py-1">
                            <div className="flex items-center gap-1">
                              <span className="min-w-0 flex-1 truncate text-xs font-semibold text-fg">{n.label}</span>
                              {dep?.state === 'provisioning' ? (
                                <ProgressRing percent={dep.progress?.percent || 0} size={15} />
                              ) : dep ? (
                                <span className="h-2 w-2 shrink-0 rounded-full" title={dep.state}
                                  style={{ background: `var(--${DEPLOY_TONE[dep.state] === 'success' ? 'success' : dep.state === 'error' ? 'danger' : 'warning'})` }} />
                              ) : null}
                            </div>
                            <div className="mt-0.5 truncate text-[10px] text-muted">{sub}</div>
                            <div className="truncate text-[9px] font-medium text-fg/80">{f.type === 'valkeycluster' ? 'valkey/valkey-bundle' : `${pxcOSLabel(f)} · ${f.arch || 'amd64'}`}</div>
                            {n.exportEnabled && <div className="text-[9px] font-medium text-primary">⇅ export</div>}
                          </div>
                        </div>
                        {canRepl && (
                          <PortHandles ownerId={n.id} connecting={!!connect} snapPort={connect?.targetId === n.id ? connect.targetPort : null} onStart={startConnect} />
                        )}
                      </div>
                    )
                  })}
                  {/* Association endpoints — InnoDB/GR, repmgr + Valkey cluster have none. */}
                  {f.type !== 'innodb' && f.type !== 'repmgr' && f.type !== 'valkeycluster' && (
                    <PortHandles ownerId={f.id} connecting={!!connect} snapPort={connect?.targetId === f.id ? connect.targetPort : null} onStart={startConnect} />
                  )}
                </div>
              )
            })}

            {nodes.filter((n) => !n.frameId).map((n) => {
              const def = NODE_TYPES[n.type] || NODE_TYPES.intranet
              const on = selected?.kind === 'node' && selected.id === n.id
              return (
                <div
                  key={n.id}
                  onPointerDown={(e) => startNode(e, n.id)}
                  onContextMenu={(e) => openMenu(e, n.id)}
                  className={`group absolute flex cursor-grab flex-col overflow-hidden rounded-xl border bg-surface shadow-sm active:cursor-grabbing ${on ? 'ring-2 ring-primary' : ''}`}
                  style={{ left: n.x, top: n.y, width: NODE_W, height: NODE_H }}
                >
                  <div className="h-1.5 w-full shrink-0" style={{ background: def.color }} />
                  <div className="flex flex-1 flex-col justify-center px-3 py-2">
                    <div className="flex items-start gap-2.5">
                      <span className="mt-0.5 shrink-0" style={{ color: def.color }}>
                        {(Icon[def.icon] || Icon.Server)({ size: 22 })}
                      </span>
                      <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-2">
                          <div className="min-w-0 flex-1 truncate text-sm font-semibold text-fg">{n.label}</div>
                          <span className="shrink-0">
                            {depByNode[n.id]?.state === 'provisioning' ? (
                              <ProgressRing percent={depByNode[n.id].progress?.percent || 0} size={20} />
                            ) : depByNode[n.id] ? (
                              <Badge tone={DEPLOY_TONE[depByNode[n.id].state] || 'muted'}>{depByNode[n.id].state}</Badge>
                            ) : null}
                          </span>
                        </div>
                        <div className="mt-0.5 text-[11px] leading-snug text-muted">{def.sub}</div>
                        <div className="mt-1 text-[11px] font-medium text-fg/80">{nodeOSLabel(n)} · {n.arch || 'amd64'}</div>
                      </div>
                    </div>
                  </div>
                  {def.ports && (
                    <PortHandles ownerId={n.id} connecting={!!connect} snapPort={connect?.targetId === n.id ? connect.targetPort : null} onStart={startConnect} />
                  )}
                </div>
              )
            })}
          </div>

          <div className="pointer-events-none absolute bottom-3 left-3 rounded-lg border bg-surface/80 px-3 py-2 text-xs text-muted backdrop-blur">
            Drag canvas to pan · scroll to zoom · drag a port to connect · right-click for actions
          </div>

          <Minimap nodes={nodes} view={view} setView={setView} wrapRef={wrapRef} selectedId={selected?.kind === 'node' ? selected.id : null} />
        </div>
        </div>
      </div>

      <StackProperties
        selected={selected}
        stackId={stack.id}
        nodes={nodes}
        edges={edges}
        frames={frames}
        depByNode={depByNode}
        patchNode={patchNode}
        patchFrame={patchFrame}
        patchEdge={patchEdge}
        deleteNode={deleteNode}
        deleteEdge={deleteEdge}
        deleteFrame={deleteFrame}
        rebuildMongoCluster={rebuildMongoCluster}
        deployOpen={deployPanel === 'open'}
        deployments={deployments}
        onDeployMinimize={() => setDeployPanel('min')}
      />

      {menu && (
        <ContextMenu menu={menu} onClose={() => setMenu(null)} actions={nodeMenuActions(menu.id)} />
      )}

      {configNode && <ConfigModal dep={configNode} onClose={() => setConfigNode(null)} />}

      {linkPrompt && (
        <LinkDirectionModal
          prompt={linkPrompt} nodes={nodes} edges={edges}
          onClose={() => setLinkPrompt(null)}
          onChoose={(fromEnd, toEnd) => { createFlow(fromEnd, toEnd); setLinkPrompt(null) }}
        />
      )}

      {replPrompt && (
        <ReplicationLinkModal
          prompt={replPrompt} nodes={nodes} frames={frames}
          onClose={() => setReplPrompt(null)}
          onChoose={(fromEnd, toEnd, mode) => { createReplEdge(fromEnd, toEnd, mode); setReplPrompt(null) }}
        />
      )}

      {confirmDel && (
        <DeleteConfirmModal
          info={confirmDel}
          onCancel={() => setConfirmDel(null)}
          onConfirm={() => { const fn = confirmDel.onConfirm; setConfirmDel(null); if (fn) fn() }}
        />
      )}

      {deployPanel === 'min' && createPortal(
        <button
          onClick={() => setDeployPanel('open')}
          className="fixed bottom-3 left-3 z-40 flex items-center gap-2 rounded-lg border bg-surface px-3 py-2 text-sm shadow-lg hover:bg-surface2"
        >
          <Icon.Arrow size={16} /> Deployment
          {deployments.some((d) => d.state === 'pending' || d.state === 'provisioning') && (
            <span className="h-2 w-2 animate-pulse rounded-full bg-warning" />
          )}
        </button>,
        document.body,
      )}
    </div>
  )
}

const DEPLOY_KEY = 'dbcanvas-deploy-layout'
function loadDeployLayout() {
  try { return { docked: true, height: 280, float: { x: 120, y: 120, w: 640, h: 360 }, ...JSON.parse(localStorage.getItem(DEPLOY_KEY) || '{}') } }
  catch { return { docked: true, height: 280, float: { x: 120, y: 120, w: 640, h: 360 } } }
}

// When docked (the default) the console lives at the bottom of the rightmost
// Properties column: `inline` renders it as an in-flow flex child of that column;
// if Properties is detached it falls back to a fixed panel pinned to the right
// edge bottom (`columnWidth` wide). Detached, it floats freely via a portal.
function DeploymentConsole({ deployments, nodes, onMinimize, inline = false, columnWidth = 320 }) {
  const [layout, setLayout] = useState(loadDeployLayout)
  const drag = useRef(null)
  useEffect(() => { try { localStorage.setItem(DEPLOY_KEY, JSON.stringify(layout)) } catch { /* */ } }, [layout])

  useEffect(() => {
    const onMove = (e) => {
      const d = drag.current
      if (!d) return
      if (d.kind === 'height') setLayout((l) => ({ ...l, height: Math.min(Math.max(160, d.h0 + (d.y0 - e.clientY)), window.innerHeight - 80) }))
      else if (d.kind === 'move') setLayout((l) => ({ ...l, float: { ...l.float, x: d.fx + (e.clientX - d.x0), y: d.fy + (e.clientY - d.y0) } }))
      else if (d.kind === 'wh') setLayout((l) => ({ ...l, float: { ...l.float, w: Math.max(360, d.w0 + (e.clientX - d.x0)), h: Math.max(200, d.h0 + (e.clientY - d.y0)) } }))
    }
    const onUp = () => { drag.current = null }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => { removeEventListener('pointermove', onMove); removeEventListener('pointerup', onUp) }
  }, [])

  const provisioning = deployments.some((d) => d.state === 'pending' || d.state === 'provisioning')
  const failed = deployments.filter((d) => d.state === 'error')
  const done = !provisioning && deployments.length > 0
  const label = (nid) => nodes.find((n) => n.id === nid)?.label || nid

  const detached = !layout.docked
  let style, cls = 'z-40 flex flex-col border bg-surface shadow-2xl'
  if (detached) {
    style = { position: 'fixed', left: layout.float.x, top: layout.float.y, width: layout.float.w, height: layout.float.h }
  } else if (inline) {
    // in-flow child at the bottom of the Properties column
    style = { height: layout.height }
    cls += ' shrink-0 overflow-hidden rounded-xl'
  } else {
    // docked but Properties is detached: pin to the right-column bottom
    style = { position: 'fixed', right: 0, bottom: 0, width: columnWidth, height: layout.height }
    cls += ' overflow-hidden rounded-xl'
  }

  const node = (
    <div className={cls} style={style}>
      {!detached && (
        <div onPointerDown={(e) => { drag.current = { kind: 'height', y0: e.clientY, h0: layout.height } }}
          className="h-1.5 w-full cursor-ns-resize bg-border/60 hover:bg-primary" />
      )}
      <div
        className="flex items-center gap-2 border-b bg-surface2 px-3 py-1.5"
        onPointerDown={detached ? (e) => { if (e.target.closest('button')) return; drag.current = { kind: 'move', x0: e.clientX, y0: e.clientY, fx: layout.float.x, fy: layout.float.y } } : undefined}
        style={detached ? { cursor: 'move' } : undefined}
      >
        <span className="text-sm font-semibold">Deployment</span>
        {provisioning ? (
          <Badge tone="warning">provisioning…</Badge>
        ) : done ? (
          failed.length
            ? <Badge tone="danger">completed with errors — {failed.length} of {deployments.length} failed</Badge>
            : <Badge tone="success">deployment complete</Badge>
        ) : null}
        <div className="ml-auto flex items-center gap-1">
          <button title={detached ? 'Dock' : 'Detach'} onClick={() => setLayout((l) => ({ ...l, docked: !l.docked }))}
            className="rounded p-1 text-muted hover:bg-surface hover:text-fg"><Icon.Frame size={14} /></button>
          <button title="Minimize" onClick={onMinimize} className="rounded px-1.5 text-muted hover:bg-surface hover:text-fg">—</button>
        </div>
      </div>
      <div className="flex-1 space-y-3 overflow-auto p-3">
        {deployments.length === 0 && <div className="text-sm text-muted">No nodes deployed.</div>}
        {deployments.map((d) => {
          const p = d.progress || {}
          return (
            <div key={d.nodeId} className="rounded-lg border bg-bg p-2">
              <div className="mb-1 flex items-center gap-2 text-sm">
                <span className="font-medium">{label(d.nodeId)}</span>
                <Badge tone={DEPLOY_TONE[d.state] || 'muted'}>{d.state}</Badge>
                <span className="ml-auto text-xs text-muted">{p.phase || ''}</span>
              </div>
              <div className="h-1.5 w-full overflow-hidden rounded-full bg-surface2">
                <div className={`h-full transition-all ${d.state === 'error' ? 'bg-danger' : d.state === 'running' ? 'bg-success' : 'bg-warning'}`} style={{ width: `${p.percent || 0}%` }} />
              </div>
              {p.message && <div className={`mt-1 text-xs ${d.state === 'error' ? 'text-danger' : 'text-muted'}`}>{p.message}</div>}
              {Array.isArray(p.log) && p.log.length > 0 && (
                <pre className="mt-1.5 max-h-32 overflow-auto whitespace-pre-wrap break-all rounded bg-surface2 p-1.5 text-[11px] leading-tight text-muted">{p.log.slice(-12).join('\n')}</pre>
              )}
            </div>
          )
        })}
      </div>
      {detached && (
        <div onPointerDown={(e) => { drag.current = { kind: 'wh', x0: e.clientX, y0: e.clientY, w0: layout.float.w, h0: layout.float.h } }}
          className="absolute bottom-0 right-0 h-4 w-4 cursor-nwse-resize text-muted">
          <svg viewBox="0 0 10 10" className="h-full w-full"><path d="M9 1 L1 9 M9 5 L5 9" stroke="currentColor" fill="none" /></svg>
        </div>
      )}
    </div>
  )

  // Inline docked → render in flow (the column positions it); otherwise (detached
  // float, or docked-while-Properties-detached) it's fixed, so portal to <body>.
  return inline && !detached ? node : createPortal(node, document.body)
}

// DeleteConfirmModal guards deletion of a *deployed* node or cluster, whose containers
// and volumes are torn down in real time (and can't be undone).
function DeleteConfirmModal({ info, onCancel, onConfirm }) {
  const isFrame = info.kind === 'frame'
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onCancel}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-1 text-sm font-semibold">Delete {isFrame ? 'cluster' : 'node'} “{info.label}”?</h3>
        <p className="mb-4 text-xs text-muted">
          {isFrame
            ? <>This cluster has {info.count} deployed node{info.count === 1 ? '' : 's'}. Deleting it will <span className="font-semibold text-danger">permanently remove</span> their containers and volumes.</>
            : <>This node is deployed. Deleting it will <span className="font-semibold text-danger">permanently remove</span> its container and volumes.</>}
          {' '}This can’t be undone.
        </p>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={onCancel}>Cancel</Button>
          <Button variant="danger" size="sm" onClick={onConfirm}>Delete</Button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

// LinkDirectionModal asks which way data flows when two ProxySQL nodes are linked.
// The option whose destination already receives a flow is disabled (a ProxySQL
// can only have one incoming flow).
function LinkDirectionModal({ prompt, nodes, edges, onClose, onChoose }) {
  const { e1, e2 } = prompt
  const labelOf = (id) => nodes.find((n) => n.id === id)?.label || 'node'
  const hasIncoming = (id) => edges.some((ed) => ed.to.node === id)
  const l1 = labelOf(e1.node)
  const l2 = labelOf(e2.node)
  const opts = [
    { from: e1, to: e2, label: `${l1} → ${l2}`, disabled: hasIncoming(e2.node), dest: l2 },
    { from: e2, to: e1, label: `${l2} → ${l1}`, disabled: hasIncoming(e1.node), dest: l1 },
  ]
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-1 text-sm font-semibold">Which way does SQL traffic flow?</h3>
        <p className="mb-3 text-xs text-muted">Pick the direction of the association line between these two ProxySQL nodes.</p>
        <div className="space-y-2">
          {opts.map((o, i) => (
            <button key={i} disabled={o.disabled} onClick={() => onChoose(o.from, o.to)}
              className={`flex w-full items-center justify-between rounded-lg border px-3 py-2 text-sm ${o.disabled ? 'cursor-not-allowed opacity-50' : 'hover:border-primary hover:bg-primary/10'}`}>
              <span className="font-mono">{o.label}</span>
              {o.disabled && <span className="text-[11px] text-muted">{o.dest} already receives a flow</span>}
            </button>
          ))}
        </div>
        {opts.every((o) => o.disabled) && (
          <p className="mt-3 rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
            Both nodes already receive a flow — no direction is available.
          </p>
        )}
        <div className="mt-4 flex justify-end">
          <Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

function ConfigModal({ dep, onClose }) {
  let cfg = {}
  try { cfg = typeof dep.config === 'string' ? JSON.parse(dep.config) : dep.config || {} } catch { cfg = {} }
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-md rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <div className="mb-3 flex items-center justify-between">
          <h3 className="text-sm font-semibold">Node profile</h3>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
        <dl className="space-y-1.5 text-sm">
          <Row k="FQDN" v={cfg.fqdn} />
          <Row k="Domain" v={cfg.domain} />
          <Row k="Base DN" v={cfg.baseDN} />
          <Row k="LDAP admin" v={cfg.ldapAdminDN} />
          <Row k="OS / arch" v={cfg.os ? `${cfg.os} · ${cfg.arch || ''}` : ''} />
          <Row k="Network alias" v={cfg.alias} />
          <Row k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} />
        </dl>
        {Array.isArray(cfg.services) && (
          <div className="mt-3">
            <div className="mb-1 text-xs font-medium text-muted">Services</div>
            <div className="flex flex-wrap gap-1">
              {cfg.services.map((s) => <Badge key={s} tone="primary">{s}</Badge>)}
            </div>
          </div>
        )}
      </div>
    </div>,
    document.body,
  )
}

function Row({ k, v }) {
  return (
    <div className="flex justify-between gap-3">
      <dt className="text-muted">{k}</dt>
      <dd className="truncate font-mono text-xs text-fg">{v || '—'}</dd>
    </div>
  )
}

// PMMOptions renders the PMM-only node settings: minor-version picker (from the
// catalog produced by `make versions`), admin password (auto-generated when
// empty), and the Intranet-CA certificate toggle.
function PMMOptions({ n, nodes = [], patchNode, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.pmmCatalog().then((c) => { if (alive) setCat(c) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const versions = cat?.versions || []
  const defaultTag = cat?.defaultTag || '3'
  const watchtowers = nodes.filter((x) => x.type === 'watchtower')
  return (
    <>
      <Field
        label="PMM version"
        hint={deployed ? 'Locked — the node is deployed.' : `Default is the rolling latest (percona/pmm-server:${defaultTag}). Pick a minor version to pin it.`}
      >
        <select
          className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
          value={n.version || ''}
          disabled={deployed}
          onChange={(e) => patchNode(n.id, { version: e.target.value })}
        >
          <option value="">latest ({defaultTag})</option>
          {versions.map((v) => (
            <option key={v} value={v}>{v}</option>
          ))}
        </select>
      </Field>
      <Field
        label="Admin password"
        hint={deployed ? 'Set at deploy time.' : 'Leave empty to auto-generate a strong password.'}
      >
        <input
          className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
          value={n.adminPassword || ''}
          disabled={deployed}
          placeholder="(auto-generate if empty)"
          onChange={(e) => patchNode(n.id, { adminPassword: e.target.value })}
        />
      </Field>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input
          type="checkbox"
          checked={!!n.generateCert}
          disabled={deployed}
          onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })}
        />
        <span>Generate nginx certificate from Intranet CA</span>
      </label>
      {n.generateCert && !deployed && (
        <p className="text-xs text-muted">
          Requires an Intranet node in the stack. New certs are written to <span className="font-mono">/srv/nginx</span> at deploy.
        </p>
      )}
      <Field
        label="Watchtower"
        hint={deployed ? 'Set at deploy time.' : watchtowers.length ? 'Associate a Watchtower so PMM can perform in-app server upgrades.' : 'Add a Watchtower node to enable in-app upgrades.'}
      >
        <select
          className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
          value={n.watchtowerNodeId || ''}
          disabled={deployed || watchtowers.length === 0}
          onChange={(e) => patchNode(n.id, { watchtowerNodeId: e.target.value })}
        >
          <option value="">none</option>
          {watchtowers.map((w) => (
            <option key={w.id} value={w.id}>{w.label}</option>
          ))}
        </select>
      </Field>
    </>
  )
}

// ------------------------------------------------------------- PXC cluster forms

// PXCFrameForm edits a PXC cluster frame: version/OS/platform, credentials,
// monitoring/proxy/GTID/TLS options, and shows quorum guidance.
function PXCFrameForm({ frame: f, stackId, nodes, frameNodes, patchFrame, deleteFrame, deployed, running }) {
  const [cat, setCat] = useState(null)
  const [monBusy, setMonBusy] = useState(false)
  const [monMsg, setMonMsg] = useState('')
  const [monErr, setMonErr] = useState('')
  useEffect(() => {
    let alive = true
    stackApi.pxcCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === f.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion && i.arch === f.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.pxcMajor]) || []

  // Cascade-normalize the selection: when OS (or a higher-level field) changes, the
  // dependent fields may become invalid for the new OS (e.g. osVersion stays "9"
  // under ubuntu), leaving major/minor empty. Snap each invalid field to the first
  // valid option for the current catalog, in one pass, until everything is valid.
  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(f.osVersion) ? f.osVersion : (osVersions[0] ?? f.osVersion)
    if (osVer !== f.osVersion) patch.osVersion = osVer
    const archList = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === osVer).map((i) => i.arch))]
    const arch = archList.includes(f.arch) ? f.arch : (archList[0] ?? f.arch)
    if (arch !== f.arch) patch.arch = arch
    const e2 = imgs.find((i) => i.os === f.os && i.osVersion === osVer && i.arch === arch)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(f.pxcMajor) ? f.pxcMajor : (majorList[0] ?? f.pxcMajor)
    if (major !== f.pxcMajor) patch.pxcMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (f.pxcVersion && !minorList.includes(f.pxcVersion)) patch.pxcVersion = ''
    if (Object.keys(patch).length) patchFrame(f.id, patch)
  }, [imgs, f.id, f.os, f.osVersion, f.arch, f.pxcMajor, f.pxcVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const regulars = frameNodes.filter((n) => n.role !== 'arbitrator').length
  const total = frameNodes.length

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PXC Cluster</span>
        <Badge tone="primary">{total} node{total === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={f.arch} disabled={deployed} onChange={(e) => patchFrame(f.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PXC major">
          <select className={`${inputCls} ${lock}`} value={f.pxcMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { pxcMajor: e.target.value, pxcVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="PXC minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.pxcVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { pxcVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint={running ? 'Pick a PMM node (or none), then apply to the running cluster.' : 'Optional — registers the cluster with a PMM node.'}>
        <select className={inputCls} value={f.pmmNodeId || ''} onChange={(e) => { patchFrame(f.id, { pmmNodeId: e.target.value }); setMonMsg(''); setMonErr('') }}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>
      {running && (
        <div className="space-y-1.5 rounded-lg border border-dashed p-2">
          <div className="text-xs text-muted">Applies PMM monitoring to the running data nodes now (installs pmm-client and registers, or deregisters when set to none).</div>
          {monErr && <div className="rounded border border-danger/30 bg-danger/15 px-2 py-1 text-xs text-danger">{monErr}</div>}
          {monMsg && <div className="rounded border border-success/30 bg-success/15 px-2 py-1 text-xs text-success">{monMsg}</div>}
          <Button size="sm" className="w-full" disabled={monBusy}
            onClick={async () => {
              setMonBusy(true); setMonErr(''); setMonMsg('')
              try {
                const r = await frameApi(stackId, f.id).setMonitoring(f.pmmNodeId || '')
                setMonMsg(f.pmmNodeId ? `Monitoring enabled (${r.updated} node${r.updated === 1 ? '' : 's'}).` : `Monitoring disabled (${r.updated} node${r.updated === 1 ? '' : 's'}).`)
              } catch (e) { setMonErr(e.message) } finally { setMonBusy(false) }
            }}>
            {monBusy ? 'Applying…' : (f.pmmNodeId ? 'Apply PMM monitoring' : 'Disable PMM monitoring')}
          </Button>
        </div>
      )}

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for egress</span>
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={f.gtid !== false} onChange={(e) => patchFrame(f.id, { gtid: e.target.checked })} />
        <span>Enable GTID</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(regulars < 3 || total % 2 === 0) && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          {regulars < 3 && <div>For HA, use at least 3 regular nodes ({regulars} now).</div>}
          {total % 2 === 0 && <div>An odd number of nodes keeps quorum on a split network ({total} now).</div>}
        </div>
      )}
      {regulars === 0 && (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">At least one regular (data) node is required.</div>
      )}

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// PXCNodeForm edits a single PXC cluster member: role and host port export.
function PXCNodeForm({ node: n, frame, nodes, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack.">
        <input className={`${inputCls} opacity-70`} value={n.label} readOnly />
      </Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <Field label="Role" hint={deployed ? 'Locked — the node is deployed.' : 'Arbitrator (garbd) votes for quorum but stores no data.'}>
        <select className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.role || 'regular'} disabled={deployed} onChange={(e) => patchNode(n.id, { role: e.target.value })}>
          <option value="regular">regular (data node)</option>
          <option value="arbitrator">arbitrator (garbd)</option>
        </select>
      </Field>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.exportEnabled} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export DB port to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
          <input type="number" min="0" max="65535" className={inputCls} value={n.exportHostPort || 0}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// MySQLFrameForm edits a MySQL replication frame: catalog-driven OS/version +
// Percona Server major/minor, replication mode, root password, PMM/proxy/GTID/cert.
function MySQLFrameForm({ frame: f, nodes, frames, edges, patchFrame, deleteFrame, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.psCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === f.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion && i.arch === f.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.psMajor]) || []

  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(f.osVersion) ? f.osVersion : (osVersions[0] ?? f.osVersion)
    if (osVer !== f.osVersion) patch.osVersion = osVer
    const archList = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === osVer).map((i) => i.arch))]
    const arch = archList.includes(f.arch) ? f.arch : (archList[0] ?? f.arch)
    if (arch !== f.arch) patch.arch = arch
    const e2 = imgs.find((i) => i.os === f.os && i.osVersion === osVer && i.arch === arch)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(f.psMajor) ? f.psMajor : (majorList[0] ?? f.psMajor)
    if (major !== f.psMajor) patch.psMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (f.psVersion && !minorList.includes(f.psVersion)) patch.psVersion = ''
    if (Object.keys(patch).length) patchFrame(f.id, patch)
  }, [imgs, f.id, f.os, f.osVersion, f.arch, f.psMajor, f.psVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  const members = nodes.filter((x) => x.frameId === f.id)
  const primaries = members.filter((x) => x.role === 'primary').length
  const secondaries = members.length - primaries

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Percona Server Replication</span>
        <Badge tone="primary">{members.length} node{members.length === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={f.arch} disabled={deployed} onChange={(e) => patchFrame(f.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Percona Server major">
          <select className={`${inputCls} ${lock}`} value={f.psMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { psMajor: e.target.value, psVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="Percona Server minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.psVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { psVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Replication mode" hint={deployed ? 'Locked.' : 'Semi-sync waits for a replica ack on commit.'}>
        <select className={`${inputCls} ${lock}`} value={f.replMode || 'async'} disabled={deployed} onChange={(e) => patchFrame(f.id, { replMode: e.target.value })}>
          <option value="async">normal (asynchronous)</option>
          <option value="semisync">semi-synchronous</option>
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers each node with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={f.gtid !== false} disabled={deployed} onChange={(e) => patchFrame(f.id, { gtid: e.target.checked })} />
        <span>Enable GTID (required for auto-positioning)</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(primaries !== 1 || secondaries === 0) && (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          {primaries !== 1 && <div>Exactly one node must be the primary ({primaries} now).</div>}
          {secondaries === 0 && <div>At least one secondary is required.</div>}
        </div>
      )}

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// MySQLMemberForm edits a MySQL replication member: its role (choosing primary
// auto-demotes the current primary) and host-port export.
function MySQLMemberForm({ node: n, frame, nodes, patchNode, dep, deployed }) {
  const setRole = (role) => {
    if (role === 'primary') {
      // Exactly one primary: demote any other primary in this frame.
      for (const m of nodes) {
        if (m.frameId === n.frameId && m.id !== n.id && m.role === 'primary') patchNode(m.id, { role: 'secondary' })
      }
    }
    patchNode(n.id, { role })
  }
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <Field label="Role" hint={deployed ? 'Locked — the node is deployed.' : 'There is always exactly one primary; the rest are read-only secondaries.'}>
        <select className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.role || 'secondary'} disabled={deployed} onChange={(e) => setRole(e.target.value)}>
          <option value="primary">primary (read/write)</option>
          <option value="secondary">secondary (read-only)</option>
        </select>
      </Field>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export DB port (3306) to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// PerconaServerForm edits a standalone Percona Server node: catalog-driven OS/version
// + Percona Server major/minor, root password, PMM/proxy/GTID/cert and host export.
// (Same options as the replication frame, minus the replication mode and role.)
function PerconaServerForm({ node: n, nodes, patchNode, deleteNode, dep, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.psCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === n.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === n.os && i.osVersion === n.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === n.os && i.osVersion === n.osVersion && i.arch === n.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[n.psMajor]) || []

  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(n.osVersion) ? n.osVersion : (osVersions[0] ?? n.osVersion)
    if (osVer !== n.osVersion) patch.osVersion = osVer
    const archList = [...new Set(imgs.filter((i) => i.os === n.os && i.osVersion === osVer).map((i) => i.arch))]
    const arch = archList.includes(n.arch) ? n.arch : (archList[0] ?? n.arch)
    if (arch !== n.arch) patch.arch = arch
    const e2 = imgs.find((i) => i.os === n.os && i.osVersion === osVer && i.arch === arch)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(n.psMajor) ? n.psMajor : (majorList[0] ?? n.psMajor)
    if (major !== n.psMajor) patch.psMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (n.psVersion && !minorList.includes(n.psVersion)) patch.psVersion = ''
    if (Object.keys(patch).length) patchNode(n.id, patch)
  }, [imgs, n.id, n.os, n.osVersion, n.arch, n.psMajor, n.psVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((x) => x.type === 'pmm')

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Percona Server</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>

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
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={n.arch} disabled={deployed} onChange={(e) => patchNode(n.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Percona Server major">
          <select className={`${inputCls} ${lock}`} value={n.psMajor} disabled={deployed} onChange={(e) => patchNode(n.id, { psMajor: e.target.value, psVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="Percona Server minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={n.psVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { psVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers this server with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={n.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={n.gtid !== false} disabled={deployed} onChange={(e) => patchNode(n.id, { gtid: e.target.checked })} />
        <span>Enable GTID</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.generateCert} disabled={deployed} onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })} />
        <span>Generate certificate from Intranet CA</span>
      </label>
      {n.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={n.certTtlValue || 365} onChange={(e) => patchNode(n.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={n.certTtlUnit || 'days'} onChange={(e) => patchNode(n.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export DB port (3306) to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${lock}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}

      {!deployed && <p className="text-xs text-muted">Access links and credentials appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// PostgreSQLForm edits a standalone PostgreSQL node: catalog-driven OS/version +
// PostgreSQL major/minor, superuser password, an optional pgBackRest → SeaweedFS S3
// backup (like the Patroni frame), PMM/proxy/cert and host export. A single
// read/write instance — no Patroni/etcd/replication.
function PostgreSQLForm({ node: n, nodes, patchNode, deleteNode, dep, deployed }) {
  const imgs = usePPGCatalog(n, deployed, patchNode)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  const seaweedNodes = nodes.filter((x) => x.type === 'seaweedfs')

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === n.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === n.os && i.osVersion === n.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === n.os && i.osVersion === n.osVersion && i.arch === n.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[n.pgMajor]) || []

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PostgreSQL</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>

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
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={n.arch} disabled={deployed} onChange={(e) => patchNode(n.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PostgreSQL major">
          <select className={`${inputCls} ${lock}`} value={n.pgMajor} disabled={deployed} onChange={(e) => patchNode(n.id, { pgMajor: e.target.value, pgVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="PostgreSQL minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={n.pgVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { pgVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.usePgBackRest} disabled={deployed} onChange={(e) => patchNode(n.id, { usePgBackRest: e.target.checked })} />
        <span>Use pgBackRest (SeaweedFS S3) for backup</span>
      </label>
      {n.usePgBackRest && (
        <Field label="SeaweedFS node (S3 repository)" hint={seaweedNodes.length ? 'WAL archive + an initial full backup land here. The node must have S3 TLS enabled (pgBackRest needs HTTPS).' : 'Add a SeaweedFS node (with S3 TLS enabled) to the stack first.'}>
          <select className={`${inputCls} ${lock}`} value={n.seaweedfsNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { seaweedfsNodeId: e.target.value })}>
            <option value="">select a SeaweedFS node…</option>
            {seaweedNodes.map((s) => <option key={s.id} value={s.id}>{s.label}{s.tls ? '' : ' — needs S3 TLS'}</option>)}
          </select>
        </Field>
      )}

      <Field label="Monitored by (PMM)" hint="Optional — registers this server with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={n.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.generateCert} disabled={deployed} onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })} />
        <span>Generate certificate from Intranet CA (PostgreSQL TLS)</span>
      </label>
      {n.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={n.certTtlValue || 365} onChange={(e) => patchNode(n.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={n.certTtlUnit || 'days'} onChange={(e) => patchNode(n.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export DB port (5432) to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${lock}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}

      {!deployed && <p className="text-xs text-muted">A single read/write PostgreSQL instance (no replication). Access links and credentials appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// SeaweedFSForm edits a (not-yet-running) SeaweedFS node: the S3 access key
// (AWS_ACCESS_KEY_ID, defaults to "seaweedfs"), the secret key (left empty to
// auto-generate), and the bucket to create. The region is fixed at us-east-1.
function SeaweedFSForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  const lock = deployed ? 'opacity-70' : ''
  const bucketOk = /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test((n.bucket || '').trim()) &&
    !/(\.\.|\.-|-\.)/.test((n.bucket || '').trim())
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">SeaweedFS</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        S3-compatible object storage (<span className="font-mono">chrislusf/seaweedfs</span>),
        used as a backup target for xtrabackup/xbcloud, Percona Backup for MongoDB and pgBackRest.
      </p>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <Field label="AWS_ACCESS_KEY_ID" hint={deployed ? 'Set at deploy.' : 'Defaults to "seaweedfs".'}>
        <input className={`${inputCls} ${lock}`} value={n.accessKey ?? 'seaweedfs'} disabled={deployed} placeholder="seaweedfs"
          onChange={(e) => patchNode(n.id, { accessKey: e.target.value })} />
      </Field>

      <Field label="AWS_SECRET_ACCESS_KEY" hint={deployed ? 'Generated at deploy — see Access tab.' : 'Leave empty to auto-generate.'}>
        <input className={`${inputCls} ${lock}`} value={n.secretKey || ''} disabled={deployed} placeholder="(auto-generate if empty)"
          onChange={(e) => patchNode(n.id, { secretKey: e.target.value })} />
      </Field>

      <Field label="Bucket name" hint="Required. 3–63 chars: lowercase letters, digits, dots and hyphens.">
        <input className={`${inputCls} ${lock}`} value={n.bucket || ''} disabled={deployed} placeholder="db-backups"
          onChange={(e) => patchNode(n.id, { bucket: e.target.value })} />
      </Field>
      {!deployed && (n.bucket || '').trim() && !bucketOk && (
        <p className="text-xs text-danger">Invalid bucket name — must be 3–63 chars and start/end with a letter or digit.</p>
      )}

      <div className="rounded-lg bg-surface2 px-3 py-2 text-xs text-muted">
        <span className="font-medium text-fg/80">AWS_DEFAULT_REGION</span> is <span className="font-mono">us-east-1</span>.
        The S3 endpoint stays on <span className="font-mono">:8333</span> (used in-network by the database
        nodes); the <span className="font-mono">:8080</span> web interface is published to the host.
      </div>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.tls} disabled={deployed} onChange={(e) => patchNode(n.id, { tls: e.target.checked })} />
        <span>Serve the S3 endpoint over TLS (HTTPS on :8333)</span>
      </label>
      {n.tls && (
        <>
          <label className={`flex items-center gap-2 pl-5 text-sm ${deployed ? 'opacity-70' : ''}`}>
            <input type="checkbox" checked={!!n.generateCert} disabled={deployed} onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })} />
            <span>Sign the certificate with the Intranet CA</span>
          </label>
          {n.generateCert ? (
            <div className="flex items-center gap-2 pl-5">
              <span className="text-xs text-muted">Cert TTL</span>
              <input type="number" min="1" className={`${inputCls} w-20 ${lock}`} value={n.certTtlValue || 365} disabled={deployed} onChange={(e) => patchNode(n.id, { certTtlValue: Number(e.target.value) })} />
              <select className={`${inputCls} ${lock}`} value={n.certTtlUnit || 'days'} disabled={deployed} onChange={(e) => patchNode(n.id, { certTtlUnit: e.target.value })}>
                <option value="minutes">minutes</option>
                <option value="hours">hours</option>
                <option value="days">days</option>
              </select>
            </div>
          ) : (
            <p className="pl-5 text-xs text-muted">Self-signed — clients must skip TLS verification (the snippets set this).</p>
          )}
        </>
      )}

      {!deployed && <p className="text-xs text-muted">The endpoint URL and copy-paste backup snippets appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// WatchtowerForm edits a (not-yet-running) Watchtower node. It is a per-stack
// singleton with no tunables — it runs percona/watchtower with the docker socket
// mounted and its HTTP API enabled so an associated PMM node can drive upgrades.
function WatchtowerForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Watchtower</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        Runs <span className="font-mono">percona/watchtower</span> with the Docker socket mounted and its
        HTTP API enabled. Associate it from a PMM node (its options) so PMM can trigger in-app server
        upgrades. One Watchtower per stack.
      </p>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <div className="rounded-lg bg-surface2 px-3 py-2 text-xs text-muted">
        Reachable in-network at <span className="font-mono">http://watchtower:8080</span>. A unique HTTP API
        token is generated at deploy and shown here; nothing is published to the host.
      </div>

      {!deployed && <p className="text-xs text-muted">The API token appears here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// WatchtowerManager shows a deployed Watchtower's profile (image, alias, API token).
function WatchtowerManager({ stackId, nodeId, dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const sec = dep?.secrets || {}
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Watchtower</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <p className="text-xs text-muted">
        Container auto-upgrades for PMM. Associate it from a PMM node to enable in-app upgrades.
      </p>
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Image</span><span className="font-mono text-xs">{cfg.image || 'percona/watchtower:latest'}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Host</span><span className="font-mono text-xs">{cfg.fqdn || cfg.hostname}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">API</span><span className="font-mono text-xs">http://{cfg.alias || 'watchtower'}:{cfg.apiPort || 8080}</span></div>
        {sec.apiToken && (
          <div className="flex justify-between gap-3"><span className="text-muted">API token</span><span className="break-all font-mono text-xs">{sec.apiToken}</span></div>
        )}
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// KeycloakForm edits a (not-yet-running) Keycloak node. Per-stack singleton, no
// tunables — it runs the keycloak image in dev mode; a PSMDB node references it to
// enable MONGODB-OIDC. The realm/client/users are set up in the console after deploy.
function KeycloakForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Keycloak</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        OpenID Connect identity provider (<span className="font-mono">quay.io/keycloak/keycloak</span>, dev
        mode). Enable Keycloak OIDC on a PSMDB node to authenticate with it. One Keycloak per stack.
      </p>

      <Field label="Label" hint="Becomes the node hostname (also the OIDC issuer host); must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={n.generateCert !== false} disabled={deployed} onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })} />
        <span>Use Intranet CA SSL (HTTPS issuer)</span>
      </label>
      <p className="text-xs text-muted">
        {n.generateCert !== false
          ? 'Serves HTTPS on 8443 with an Intranet-CA cert; the OIDC issuer is https://<host>:8443. Required for MongoDB OIDC.'
          : 'HTTP only — MongoDB OIDC will not work (it requires an HTTPS issuer). Enable SSL to use it with PSMDB.'}
      </p>
      {n.generateCert !== false && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={n.certTtlValue || 365} disabled={deployed} onChange={(e) => patchNode(n.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={n.certTtlUnit || 'days'} disabled={deployed} onChange={(e) => patchNode(n.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      <div className="rounded-lg bg-surface2 px-3 py-2 text-xs text-muted">
        Admin console is published to the host on auto-assigned ports (8080 http / 8443 https). The bootstrap
        admin user + password appear here after deploy. When a PSMDB node enables OIDC, its realm, client,
        groups and sample users are created automatically.
      </div>

      {!deployed && <p className="text-xs text-muted">Console URL + admin credentials appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// KeycloakManager shows a deployed Keycloak's console URL + bootstrap admin creds.
function KeycloakManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const sec = dep?.secrets || {}
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const consoleURL = cfg.httpPort ? `http://${host}:${cfg.httpPort}` : null
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Keycloak</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <p className="text-xs text-muted">OIDC identity provider. Set up the realm/client/groups/users in the console.</p>
      {consoleURL && (
        <a href={consoleURL} target="_blank" rel="noreferrer"
          className="flex items-center justify-center gap-2 rounded-lg border border-primary/40 bg-primary/10 px-3 py-2 text-sm font-medium text-primary hover:bg-primary/15">
          <Icon.External size={15} /> Open admin console
        </a>
      )}
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Image</span><span className="font-mono text-xs">{cfg.image || 'quay.io/keycloak/keycloak'}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Issuer base</span><span className="font-mono text-xs">{cfg.ssl ? `https://${cfg.fqdn || cfg.hostname}:8443` : `http://${cfg.hostname || 'keycloak'}:8080`}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">TLS</span><span className="font-mono text-xs">{cfg.ssl ? 'Intranet CA' : 'none (HTTP)'}</span></div>
        {cfg.httpPort ? <div className="flex justify-between gap-3"><span className="text-muted">Console (http)</span><span className="font-mono text-xs">{host}:{cfg.httpPort}</span></div> : null}
        {cfg.httpsPort ? <div className="flex justify-between gap-3"><span className="text-muted">Console (https)</span><span className="font-mono text-xs">{host}:{cfg.httpsPort}</span></div> : null}
        <div className="flex justify-between gap-3"><span className="text-muted">Admin user</span><span className="font-mono text-xs">{cfg.adminUser || 'admin'}</span></div>
        {sec.adminPassword && (
          <div className="flex justify-between gap-3"><span className="text-muted">Admin password</span><span className="break-all font-mono text-xs">{sec.adminPassword}</span></div>
        )}
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// VNCForm edits a (not-yet-running) Ubuntu VNC node: the desktop login user + VNC
// password and whether to route apt through the Intranet proxy.
function VNCForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  const lock = deployed ? 'opacity-70' : ''
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Ubuntu VNC</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        XFCE desktop over a browser-based VNC client (on the systemd Ubuntu image), with Firefox, the OpenSSH
        client, the Percona clients (MySQL/PSMDB/Valkey/PostgreSQL), percona-toolkit + ldap-utils preinstalled.
        The login user has sudo for installing more tools.
      </p>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="Ubuntu version" hint="Systemd image (make images).">
          <select className={`${inputCls} ${lock}`} value={n.osVersion || '24.04'} disabled={deployed} onChange={(e) => patchNode(n.id, { osVersion: e.target.value })}>
            <option value="24.04">24.04</option>
            <option value="22.04">22.04</option>
          </select>
        </Field>
        <Field label="Arch" hint="Image architecture.">
          <select className={`${inputCls} ${lock}`} value={n.arch || 'amd64'} disabled={deployed} onChange={(e) => patchNode(n.id, { arch: e.target.value })}>
            <option value="amd64">amd64</option>
            <option value="arm64">arm64</option>
          </select>
        </Field>
      </div>

      <Field label="Desktop user" hint="Linux login user (has passwordless sudo).">
        <input className={`${inputCls} ${lock}`} value={n.vncUser ?? 'dbadmin'} disabled={deployed} onChange={(e) => patchNode(n.id, { vncUser: e.target.value })} />
      </Field>

      <Field label="Password" hint={deployed ? 'Set at deploy.' : 'Desktop + VNC password. Empty = auto-generate. VNC uses the first 8 characters.'}>
        <input className={`${inputCls} ${lock}`} value={n.vncPassword || ''} disabled={deployed} placeholder="(auto-generate if empty)" onChange={(e) => patchNode(n.id, { vncPassword: e.target.value })} />
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>

      {!deployed && <p className="text-xs text-muted">The web desktop URL + credentials appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// VNCManager shows a deployed VNC node's web desktop URL + credentials.
function VNCManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const sec = dep?.secrets || {}
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const url = cfg.webPort ? `http://${host}:${cfg.webPort}/vnc.html` : null
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Ubuntu VNC</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <p className="text-xs text-muted">XFCE desktop with Percona clients. Open the web desktop and enter the VNC password.</p>
      {url && (
        <a href={url} target="_blank" rel="noreferrer"
          className="flex items-center justify-center gap-2 rounded-lg border border-primary/40 bg-primary/10 px-3 py-2 text-sm font-medium text-primary hover:bg-primary/15">
          <Icon.External size={15} /> Open web desktop
        </a>
      )}
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Image</span><span className="font-mono text-xs">{cfg.image || 'ubuntu:24.04'}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Host</span><span className="font-mono text-xs">{cfg.fqdn || cfg.hostname}</span></div>
        {cfg.webPort ? <div className="flex justify-between gap-3"><span className="text-muted">Web desktop</span><span className="font-mono text-xs">{host}:{cfg.webPort}/vnc.html</span></div> : null}
        <div className="flex justify-between gap-3"><span className="text-muted">Desktop user</span><span className="font-mono text-xs">{cfg.vncUser || 'dbadmin'} (sudo)</span></div>
        {sec.vncPassword && (
          <div className="flex justify-between gap-3"><span className="text-muted">VNC password</span><span className="break-all font-mono text-xs">{sec.vncPassword}</span></div>
        )}
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// ValkeyForm edits a (not-yet-running) standalone Valkey node: password (requirepass),
// optional LDAP auth against the Intranet OpenLDAP, PMM monitoring and host-port export.
function ValkeyForm({ node: n, nodes, patchNode, deleteNode, dep, deployed }) {
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Valkey (standalone)</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        Runs <span className="font-mono">valkey/valkey-bundle</span> (pulled at deploy). pmm-client is installed
        via percona-release.
      </p>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.useLdap} disabled={deployed} onChange={(e) => patchNode(n.id, { useLdap: e.target.checked })} />
        <span>Enable LDAP auth (Intranet OpenLDAP)</span>
      </label>
      {n.useLdap && <p className="text-xs text-muted">Wires the valkey-ldap module to <span className="font-mono">ldap://intranet:389</span> (users under <span className="font-mono">ou=People</span>).</p>}

      <Field label="Monitored by (PMM)" hint="Optional — installs/registers pmm-client.">
        <select className={`${inputCls} ${lock}`} value={n.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export Valkey port (6379) to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${lock}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}

      {!deployed && <p className="text-xs text-muted">Connection info + password appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// ValkeyManager shows a deployed standalone Valkey's connection info + credentials.
function ValkeyManager({ dep, onDeleteNode }) {
  const cfg = dep?.config || {}
  const sec = dep?.secrets || {}
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const isCluster = cfg.role === 'cluster'
  const clusterFlag = isCluster ? '-c ' : ''
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">{isCluster ? 'Valkey (cluster member)' : 'Valkey (standalone)'}</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <div className="space-y-2 rounded-lg bg-surface2 px-3 py-2 text-sm">
        <div className="flex justify-between gap-3"><span className="text-muted">Image</span><span className="font-mono text-xs">{cfg.image || 'valkey/valkey-bundle'}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">Host</span><span className="font-mono text-xs">{cfg.fqdn || cfg.hostname}</span></div>
        <div className="flex justify-between gap-3"><span className="text-muted">LDAP</span><span className="font-mono text-xs">{cfg.useLdap ? (cfg.ldapServers || 'enabled') : 'disabled'}</span></div>
        {cfg.exportPort ? <div className="flex justify-between gap-3"><span className="text-muted">Exported port</span><span className="font-mono text-xs">{host}:{cfg.exportPort}</span></div> : null}
        <div className="flex justify-between gap-3"><span className="text-muted">Monitored by</span><span className="font-mono text-xs">{cfg.monitoredBy || '—'}</span></div>
        {sec.password && <div className="flex justify-between gap-3"><span className="text-muted">Default password</span><span className="break-all font-mono text-xs">{sec.password}</span></div>}
      </div>
      <div className="rounded-lg bg-surface2 px-3 py-2 text-xs space-y-1">
        <div className="text-muted">Connect as the default user ({clusterFlag ? 'cluster mode' : 'direct'}):</div>
        {cfg.exportPort ? <div className="break-all font-mono">valkey-cli {clusterFlag}-h {host} -p {cfg.exportPort} -a '{sec.password || ''}'</div> : null}
        <div className="break-all font-mono">valkey-cli {clusterFlag}-h {cfg.fqdn} -p 6379 -a '{sec.password || ''}'  <span className="text-muted">(in-cluster)</span></div>
      </div>
      {cfg.useLdap && (
        <div className="rounded-lg border border-primary/30 bg-primary/5 px-3 py-2 text-xs space-y-1">
          <div className="font-semibold text-primary">LDAP login (Intranet OpenLDAP)</div>
          <div className="text-muted">The LDAP user must first exist as a Valkey ACL user (passwordless — the password is verified against LDAP). As the default user, create it:</div>
          <div className="break-all font-mono">valkey-cli {clusterFlag}-h {cfg.fqdn} -p 6379 -a '{sec.password || ''}' ACL SETUSER alice on '~*' +@all</div>
          <div className="text-muted">Then connect as the LDAP user (uid=alice,ou=People; password from LDAP):</div>
          <div className="break-all font-mono">valkey-cli {clusterFlag}-h {cfg.fqdn} -p 6379 --user alice -a '&lt;ldap-password&gt;'</div>
          <div className="text-muted">From the host use <span className="font-mono">-h {host} -p {cfg.exportPort || '&lt;export-port&gt;'}</span> (enable export to reach it from outside the stack).</div>
        </div>
      )}
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// ValkeyClusterFrameForm edits a Valkey Cluster frame: shared default-user password,
// optional LDAP, PMM monitor. 3–7 all-master shards (resize with the frame +/-).
function ValkeyClusterFrameForm({ frame: f, nodes, frameNodes, patchFrame, deleteFrame, deployed }) {
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  const count = frameNodes.length
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Valkey Cluster</span>
        <Badge tone="muted">{count} node{count === 1 ? '' : 's'}</Badge>
      </div>
      <p className="text-xs text-muted">
        {count} all-master shard{count === 1 ? '' : 's'} of <span className="font-mono">valkey/valkey-bundle</span>,
        formed with <span className="font-mono">valkey-cli --cluster create</span>. Use the frame +/- to resize (3–7).
      </p>

      <Field label="Cluster name" hint="Frame label; must be unique.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.useLdap} disabled={deployed} onChange={(e) => patchFrame(f.id, { useLdap: e.target.checked })} />
        <span>Enable LDAP auth (Intranet OpenLDAP)</span>
      </label>

      <Field label="Monitored by (PMM)" hint="Optional — installs/registers pmm-client on each member.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      {count < 3 && <p className="text-xs text-amber-500">A Valkey cluster needs at least 3 nodes.</p>}
      {count > 7 && <p className="text-xs text-amber-500">A Valkey cluster allows at most 7 nodes.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// ValkeyClusterMemberForm edits one Valkey cluster member (label + host-port export).
function ValkeyClusterMemberForm({ node: n, frame: f, patchNode, dep, deployed }) {
  const lock = deployed ? 'opacity-70' : ''
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Valkey member</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">Member of <span className="font-mono">{f?.label || 'valkey cluster'}</span>. Auth/LDAP/PMM are set on the cluster frame.</p>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export Valkey port (6379) to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${lock}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
      {!deployed && <p className="text-xs text-muted">Use the frame +/- to add or remove members (3–7).</p>}
    </div>
  )
}

// ProxySQLForm edits a (not-yet-running) ProxySQL node: catalog-driven OS/version
// + ProxySQL major/minor, implementation mode, host-port export and PMM monitor.
// It must be linked to a PXC cluster frame by an association line on the canvas.
function ProxySQLForm({ node: n, nodes, frames, edges, patchNode, deleteNode, dep, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.proxysqlCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === n.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === n.os && i.osVersion === n.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === n.os && i.osVersion === n.osVersion && i.arch === n.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[n.proxysqlMajor]) || []

  // Same cascade-normalization as the PXC frame: snap invalid dependent selects.
  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(n.osVersion) ? n.osVersion : (osVersions[0] ?? n.osVersion)
    if (osVer !== n.osVersion) patch.osVersion = osVer
    const archList = [...new Set(imgs.filter((i) => i.os === n.os && i.osVersion === osVer).map((i) => i.arch))]
    const arch = archList.includes(n.arch) ? n.arch : (archList[0] ?? n.arch)
    if (arch !== n.arch) patch.arch = arch
    const e2 = imgs.find((i) => i.os === n.os && i.osVersion === osVer && i.arch === arch)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(n.proxysqlMajor) ? n.proxysqlMajor : (majorList[0] ?? n.proxysqlMajor)
    if (major !== n.proxysqlMajor) patch.proxysqlMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (n.proxysqlVersion && !minorList.includes(n.proxysqlVersion)) patch.proxysqlVersion = ''
    if (Object.keys(patch).length) patchNode(n.id, patch)
  }, [imgs, n.id, n.os, n.osVersion, n.arch, n.proxysqlMajor, n.proxysqlVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  // Walk the association graph (a ProxySQL may reach a cluster through another ProxySQL).
  const linkedFrame = (() => {
    const adj = {}
    for (const e of edges) {
      ;(adj[e.from.node] ||= []).push(e.to.node)
      ;(adj[e.to.node] ||= []).push(e.from.node)
    }
    const seen = new Set([n.id])
    const queue = [n.id]
    while (queue.length) {
      const cur = queue.shift()
      for (const nb of adj[cur] || []) {
        const f = frames.find((fr) => fr.id === nb && (fr.type === 'pxc' || fr.type === 'mysql'))
        if (f) return f
        if (!seen.has(nb)) { seen.add(nb); queue.push(nb) }
      }
    }
    return null
  })()
  const modeOpts = proxyModeOpts(linkedFrame?.type)
  // Normalize the mode when the linked backend changes (PXC vs MySQL modes differ).
  useEffect(() => {
    if (deployed || !linkedFrame) return
    if (!modeOpts.some((m) => m.id === n.mode)) patchNode(n.id, { mode: modeOpts[0].id })
  }, [linkedFrame?.type, n.mode, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">ProxySQL</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      {linkedFrame ? (
        <div className="rounded-lg border border-success/30 bg-success/10 px-2.5 py-1.5 text-xs text-success">
          Linked to {linkedFrame.type === 'mysql' ? 'MySQL' : 'PXC'} cluster <span className="font-semibold">{linkedFrame.label}</span> (data flows cluster → ProxySQL).
        </div>
      ) : (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          Not linked — drag an association line from a PXC cluster frame to this node.
        </div>
      )}

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
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={n.arch} disabled={deployed} onChange={(e) => patchNode(n.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="ProxySQL major">
          <select className={`${inputCls} ${lock}`} value={n.proxysqlMajor} disabled={deployed} onChange={(e) => patchNode(n.id, { proxysqlMajor: e.target.value, proxysqlVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>proxysql{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="ProxySQL minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={n.proxysqlVersion} disabled={deployed} onChange={(e) => patchNode(n.id, { proxysqlVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Implementation mode" hint={deployed ? 'Locked.' : (modeOpts === PROXY_MODE_OPTS.mysql ? 'How ProxySQL routes traffic to the MySQL primary/replicas.' : 'MODE for proxysql-admin.')}>
        <select className={`${inputCls} ${lock}`} value={modeOpts.some((m) => m.id === n.mode) ? n.mode : modeOpts[0].id} disabled={deployed} onChange={(e) => patchNode(n.id, { mode: e.target.value })}>
          {modeOpts.map((m) => <option key={m.id} value={m.id}>{m.label}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers ProxySQL with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={n.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for egress</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Expose ProxySQL ports to the host (6033 MySQL, 6032 admin)</span>
      </label>
      {n.exportEnabled && (
        <Field label="MySQL host port (6033)" hint="0 / empty = random unused port; the admin port (6032) is auto-assigned.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${lock}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}

      {!deployed && <p className="text-xs text-muted">Access links and credentials appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// frameLinkedCluster walks the association graph from a frame/node id to the PXC
// cluster frame it (transitively) reaches, if any.
function frameLinkedCluster(startId, edges, frames) {
  const adj = {}
  for (const e of edges) {
    ;(adj[e.from.node] ||= []).push(e.to.node)
    ;(adj[e.to.node] ||= []).push(e.from.node)
  }
  const seen = new Set([startId])
  const queue = [startId]
  while (queue.length) {
    const cur = queue.shift()
    for (const nb of adj[cur] || []) {
      const f = frames.find((fr) => fr.id === nb && (fr.type === 'pxc' || fr.type === 'mysql'))
      if (f) return f
      if (!seen.has(nb)) { seen.add(nb); queue.push(nb) }
    }
  }
  return null
}

// ProxySQLFrameForm edits a ProxySQL cluster frame: catalog-driven OS/version +
// ProxySQL major/minor, implementation mode, PMM monitor and Intranet-proxy
// options. Per-member host-port export lives on each member node.
function ProxySQLFrameForm({ frame: f, nodes, frames, edges, patchFrame, deleteFrame, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.proxysqlCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === f.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion && i.arch === f.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.proxysqlMajor]) || []

  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(f.osVersion) ? f.osVersion : (osVersions[0] ?? f.osVersion)
    if (osVer !== f.osVersion) patch.osVersion = osVer
    const archList = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === osVer).map((i) => i.arch))]
    const arch = archList.includes(f.arch) ? f.arch : (archList[0] ?? f.arch)
    if (arch !== f.arch) patch.arch = arch
    const e2 = imgs.find((i) => i.os === f.os && i.osVersion === osVer && i.arch === arch)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(f.proxysqlMajor) ? f.proxysqlMajor : (majorList[0] ?? f.proxysqlMajor)
    if (major !== f.proxysqlMajor) patch.proxysqlMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (f.proxysqlVersion && !minorList.includes(f.proxysqlVersion)) patch.proxysqlVersion = ''
    if (Object.keys(patch).length) patchFrame(f.id, patch)
  }, [imgs, f.id, f.os, f.osVersion, f.arch, f.proxysqlMajor, f.proxysqlVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const memberCount = nodes.filter((n) => n.frameId === f.id).length
  const linkedFrame = frameLinkedCluster(f.id, edges, frames)
  const modeOpts = proxyModeOpts(linkedFrame?.type)
  useEffect(() => {
    if (deployed || !linkedFrame) return
    if (!modeOpts.some((m) => m.id === f.mode)) patchFrame(f.id, { mode: modeOpts[0].id })
  }, [linkedFrame?.type, f.mode, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">ProxySQL Cluster</span>
        <Badge tone="primary">{memberCount} node{memberCount === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      {linkedFrame ? (
        <div className="rounded-lg border border-success/30 bg-success/10 px-2.5 py-1.5 text-xs text-success">
          Linked to {linkedFrame.type === 'mysql' ? 'MySQL' : 'PXC'} cluster <span className="font-semibold">{linkedFrame.label}</span> (data flows cluster → ProxySQL).
        </div>
      ) : (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          Not linked — drag an association line from a PXC cluster frame to this cluster.
        </div>
      )}

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={f.arch} disabled={deployed} onChange={(e) => patchFrame(f.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="ProxySQL major">
          <select className={`${inputCls} ${lock}`} value={f.proxysqlMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { proxysqlMajor: e.target.value, proxysqlVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>proxysql{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="ProxySQL minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.proxysqlVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { proxysqlVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Implementation mode" hint={deployed ? 'Locked.' : (modeOpts === PROXY_MODE_OPTS.mysql ? 'How ProxySQL routes traffic to the MySQL primary/replicas.' : 'MODE for proxysql-admin.')}>
        <select className={`${inputCls} ${lock}`} value={modeOpts.some((m) => m.id === f.mode) ? f.mode : modeOpts[0].id} disabled={deployed} onChange={(e) => patchFrame(f.id, { mode: e.target.value })}>
          {modeOpts.map((m) => <option key={m.id} value={m.id}>{m.label}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers each member with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>

      <p className="text-xs text-muted">Add/remove ProxySQL nodes with the +/- on the frame. Per-node host-port export is set on each node.</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// ProxySQLFrameMemberForm edits a ProxySQL cluster member: only host-port export
// (OS/version/mode come from the frame).
function ProxySQLFrameMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack.">
        <input className={`${inputCls} opacity-70`} value={n.label} readOnly />
      </Field>
      <Field label="ProxySQL cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Expose ProxySQL ports to the host (6033 MySQL, 6032 admin)</span>
      </label>
      {n.exportEnabled && (
        <Field label="MySQL host port (6033)" hint="0 / empty = random unused port; the admin port (6032) is auto-assigned.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
      {!deployed && <p className="text-xs text-muted">Cluster settings (OS, version, mode, monitoring) are on the frame.</p>}
    </div>
  )
}

// InnoDBFrameForm edits an InnoDB Cluster / GR frame: image OS/version/arch,
// the PDPS repository (which sets the Percona Server version), the replication mode
// (InnoDB Cluster vs raw Group Replication), root password, PMM/proxy/cert, and the
// MySQL Router toggle. It has no association endpoints (Router is built in).
function InnoDBFrameForm({ frame: f, nodes, patchFrame, deleteFrame, deployed }) {
  const [imgs, setImgs] = useState([])
  const [repos, setRepos] = useState([])
  useEffect(() => {
    let alive = true
    stackApi.psCatalog().then((c) => { if (alive) setImgs(c.images || []) }).catch(() => { /* */ })
    stackApi.pdpsCatalog().then((c) => { if (alive) setRepos(c.repos || []) }).catch(() => { /* */ })
    return () => { alive = false }
  }, [])
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === f.osVersion).map((i) => i.arch))]

  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(f.osVersion) ? f.osVersion : (osVersions[0] ?? f.osVersion)
    if (osVer !== f.osVersion) patch.osVersion = osVer
    const archList = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === osVer).map((i) => i.arch))]
    const arch = archList.includes(f.arch) ? f.arch : (archList[0] ?? f.arch)
    if (arch !== f.arch) patch.arch = arch
    if (Object.keys(patch).length) patchFrame(f.id, patch)
  }, [imgs, f.id, f.os, f.osVersion, f.arch, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (deployed || !repos.length) return
    if (!repos.includes(f.pdpsRepo)) patchFrame(f.id, { pdpsRepo: repos[0] })
  }, [repos, f.pdpsRepo, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  const members = nodes.filter((x) => x.frameId === f.id).length

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">InnoDB Cluster / GR</span>
        <Badge tone="primary">{members} node{members === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <Field label="Replication mode" hint={deployed ? 'Locked.' : 'InnoDB Cluster adds MySQL Shell management + Router metadata.'}>
        <select className={`${inputCls} ${lock}`} value={f.replMode || 'innodbcluster'} disabled={deployed} onChange={(e) => patchFrame(f.id, { replMode: e.target.value })}>
          <option value="innodbcluster">InnoDB Cluster</option>
          <option value="groupreplication">Group Replication</option>
        </select>
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={f.arch} disabled={deployed} onChange={(e) => patchFrame(f.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PDPS repository" hint={deployed ? 'Locked.' : 'Sets the Percona Server version.'}>
          <select className={`${inputCls} ${lock}`} value={f.pdpsRepo} disabled={deployed} onChange={(e) => patchFrame(f.id, { pdpsRepo: e.target.value })}>
            {repos.length === 0 && <option value="">(run make versions)</option>}
            {repos.map((r) => <option key={r} value={r}>{r}</option>)}
          </select>
        </Field>
      </div>

      <Field label="Monitored by (PMM)" hint="Optional — registers each node with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={f.mysqlRouter !== false} disabled={deployed} onChange={(e) => patchFrame(f.id, { mysqlRouter: e.target.checked })} />
        <span>Install MySQL Router on each node (6446 RW / 6447 RO)</span>
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {members < 3 && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          At least 3 nodes are recommended for Group Replication quorum ({members} now).
        </div>
      )}
      <p className="text-xs text-muted">No association line — MySQL Router is built in. Per-node host-port export is set on each node.</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// InnoDBMemberForm edits an InnoDB/GR member: only host-port export of the router
// ports (OS/version/mode come from the frame; GR auto-elects the primary).
function InnoDBMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <p className="text-xs text-muted">Group Replication auto-elects the primary; secondaries are read-only.</p>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export MySQL Router ports to the host (6446 RW / 6447 RO)</span>
      </label>
      {n.exportEnabled && (
        <Field label="RW host port (6446)" hint="0 / empty = random unused port; the RO port (6447) is auto-assigned.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// PBMOptions renders the "Enable Percona Backup for MongoDB" checkbox + the
// SeaweedFS-node selector, shared by the PSMDB sharded-cluster and replica-set
// frame forms. percona-backup-mongodb is installed on every member regardless;
// enabling this configures pbm-agent + the S3 store on the selected SeaweedFS node.
function PBMOptions({ f, nodes, patchFrame, deployed }) {
  const lock = deployed ? 'opacity-70' : ''
  const seaweedNodes = nodes.filter((n) => n.type === 'seaweedfs')
  return (
    <>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.enablePBM} disabled={deployed} onChange={(e) => patchFrame(f.id, { enablePBM: e.target.checked })} />
        <span>Enable backups with Percona Backup for MongoDB (PBM)</span>
      </label>
      {f.enablePBM && (
        <Field label="SeaweedFS node (S3 backup storage)" hint={seaweedNodes.length ? 'pbm-agent runs on every member; backups land in this node\'s S3 bucket.' : 'Add a SeaweedFS node to the stack first.'}>
          <select className={`${inputCls} ${lock}`} value={f.seaweedfsNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { seaweedfsNodeId: e.target.value })}>
            <option value="">select a SeaweedFS node…</option>
            {seaweedNodes.map((s) => <option key={s.id} value={s.id}>{s.label}</option>)}
          </select>
        </Field>
      )}
    </>
  )
}

// MongoDBFrameForm edits a PSMDB Sharded Cluster frame: catalog-driven
// OS/version/arch + PS MongoDB major/minor, admin (root) password, PMM/proxy/cert.
// The 13-node sharded topology is fixed — there are no replication options.
function MongoDBFrameForm({ frame: f, nodes, patchFrame, deleteFrame, rebuildCluster, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.psmdbCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === f.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion && i.arch === f.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.psmdbMajor]) || []

  // Cascade-normalize the selection when a higher-level field changes (same logic
  // as PXCFrameForm), so major/minor never go stale for the chosen image.
  useEffect(() => {
    if (deployed || !imgs.length) return
    const patch = {}
    const osVer = osVersions.includes(f.osVersion) ? f.osVersion : (osVersions[0] ?? f.osVersion)
    if (osVer !== f.osVersion) patch.osVersion = osVer
    const archList = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === osVer).map((i) => i.arch))]
    const arch = archList.includes(f.arch) ? f.arch : (archList[0] ?? f.arch)
    if (arch !== f.arch) patch.arch = arch
    const e2 = imgs.find((i) => i.os === f.os && i.osVersion === osVer && i.arch === arch)
    const majorList = e2 ? Object.keys(e2.versions || {}).filter((m) => (e2.versions[m] || []).length) : []
    const major = majorList.includes(f.psmdbMajor) ? f.psmdbMajor : (majorList[0] ?? f.psmdbMajor)
    if (major !== f.psmdbMajor) patch.psmdbMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (f.psmdbVersion && !minorList.includes(f.psmdbVersion)) patch.psmdbVersion = ''
    if (Object.keys(patch).length) patchFrame(f.id, patch)
  }, [imgs, f.id, f.os, f.osVersion, f.arch, f.psmdbMajor, f.psmdbVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps

  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const total = nodes.filter((n) => n.frameId === f.id).length

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PSMDB Sharded Cluster</span>
        <Badge tone="primary">{total} node{total === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <Field label="Setup" hint={deployed ? 'Locked.' : 'Standard is HA; minimum is the smallest working sharded cluster.'}>
        <select className={`${inputCls} ${lock}`} value={f.psmdbSetup || 'standard'} disabled={deployed}
          onChange={(e) => rebuildCluster?.(f.id, e.target.value)}>
          <option value="standard">standard — 3 shards × 3-node RS + 3-node config RS (13 nodes)</option>
          <option value="minimum">minimum — 3 single-node shards + 1 config server (5 nodes)</option>
        </select>
      </Field>

      <div className="rounded-lg border border-dashed px-2.5 py-1.5 text-xs text-muted">
        {(f.psmdbSetup || 'standard') === 'minimum'
          ? '3 single-node shards + 1 config server + 1 mongos router. Nodes can\'t be added or removed.'
          : '3 shards × 3-node replica set (9 mongod) + 3-node config-server replica set + 1 mongos router. Nodes can\'t be added or removed.'}
      </div>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={f.arch} disabled={deployed} onChange={(e) => patchFrame(f.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PS MongoDB major">
          <select className={`${inputCls} ${lock}`} value={f.psmdbMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { psmdbMajor: e.target.value, psmdbVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="PS MongoDB minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.psmdbVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { psmdbVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers each node with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <PBMOptions f={f} nodes={nodes} patchFrame={patchFrame} deployed={deployed} />

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      <p className="text-xs text-muted">Apps connect through the mongos router; enable host-port export on the mongos node to reach it from the host.</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// MongoDBMemberForm edits a PS MongoDB member: read-only role/shard (the topology
// is fixed); only the mongos router can export its 27017 port to the host.
function MongoDBMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  const roleText = n.role === 'mongos' ? 'mongos router' : n.role === 'config' ? 'config server' : `shard ${n.shard} member`
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <Field label="Role"><input className={`${inputCls} opacity-70`} value={roleText} readOnly /></Field>
      {n.role === 'mongos' ? (
        <>
          <p className="text-xs text-muted">The mongos router is the cluster entry point; export 27017 so apps connect from the host.</p>
          <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
            <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
            <span>Export mongos port to the host (27017)</span>
          </label>
          {n.exportEnabled && (
            <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
              <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
                onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
            </Field>
          )}
        </>
      ) : (
        <p className="text-xs text-muted">Shard and config-server members are internal to the cluster — connect through the mongos router.</p>
      )}
    </div>
  )
}

// MongoCatalogFields renders the shared catalog-driven OS/version/arch + PS MongoDB
// major/minor selects used by both the PSMDB RS frame form (patch=patchFrame, obj=frame)
// and the standalone PSMDB node form (patch=patchNode, obj=node). `patch(id, {...})`
// applies the change. Cascade-normalizes invalid dependent selects like PXCFrameForm.
function useMongoCatalog(obj, deployed, patch) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.psmdbCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const osVersions = [...new Set(imgs.filter((i) => i.os === obj.os).map((i) => i.osVersion))]
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
    const major = majorList.includes(obj.psmdbMajor) ? obj.psmdbMajor : (majorList[0] ?? obj.psmdbMajor)
    if (major !== obj.psmdbMajor) p.psmdbMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (obj.psmdbVersion && !minorList.includes(obj.psmdbVersion)) p.psmdbVersion = ''
    if (Object.keys(p).length) patch(obj.id, p)
  }, [imgs, obj.id, obj.os, obj.osVersion, obj.arch, obj.psmdbMajor, obj.psmdbVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps
  return imgs
}

function MongoCatalogFields({ obj, imgs, deployed, patch }) {
  const lock = deployed ? 'opacity-70' : ''
  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === obj.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === obj.os && i.osVersion === obj.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === obj.os && i.osVersion === obj.osVersion && i.arch === obj.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[obj.psmdbMajor]) || []
  return (
    <>
      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={obj.os} disabled={deployed} onChange={(e) => patch(obj.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={obj.osVersion} disabled={deployed} onChange={(e) => patch(obj.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={obj.arch} disabled={deployed} onChange={(e) => patch(obj.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PS MongoDB major">
          <select className={`${inputCls} ${lock}`} value={obj.psmdbMajor} disabled={deployed} onChange={(e) => patch(obj.id, { psmdbMajor: e.target.value, psmdbVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>
      <Field label="PS MongoDB minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={obj.psmdbVersion} disabled={deployed} onChange={(e) => patch(obj.id, { psmdbVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>
    </>
  )
}

// PSMRSFrameForm edits a PS MongoDB replica-set frame: catalog OS/version/arch + PS
// MongoDB major/minor, admin password, PMM/proxy/cert. Members are resizable 1–9.
function PSMRSFrameForm({ frame: f, nodes, patchFrame, deleteFrame, deployed }) {
  const imgs = useMongoCatalog(f, deployed, patchFrame)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const members = nodes.filter((n) => n.frameId === f.id).length
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PSMDB RS</span>
        <Badge tone="primary">{members} node{members === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Replica-set name" hint="Becomes the replica-set name; must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <MongoCatalogFields obj={f} imgs={imgs} deployed={deployed} patch={patchFrame} />

      <Field label="Monitored by (PMM)" hint="Optional — registers each member with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <PBMOptions f={f} nodes={nodes} patchFrame={patchFrame} deployed={deployed} />

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(members < 3 || members % 2 === 0) && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          {members < 3 && <div>At least 3 members are recommended for election quorum ({members} now).</div>}
          {members % 2 === 0 && <div>An odd number of members keeps quorum on a split network ({members} now).</div>}
        </div>
      )}
      <p className="text-xs text-muted">Use the +/− buttons on the frame to resize the replica set (1–9 members).</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete replica set
      </Button>
    </div>
  )
}

// PSMRSMemberForm edits a PS MongoDB replica-set member: only host-port export
// (OS/version come from the frame; the replica set auto-elects the primary).
function PSMRSMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Replica set"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <p className="text-xs text-muted">The replica set auto-elects the primary; secondaries serve reads.</p>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export mongod port to the host (27017)</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// usePPGCatalog loads the Percona PostgreSQL catalog and cascade-normalizes a
// frame's OS/version/arch + PG major/minor selects (same shape as useMongoCatalog).
function usePPGCatalog(obj, deployed, patch) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.ppgCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const osVersions = [...new Set(imgs.filter((i) => i.os === obj.os).map((i) => i.osVersion))]
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
    const major = majorList.includes(obj.pgMajor) ? obj.pgMajor : (majorList[0] ?? obj.pgMajor)
    if (major !== obj.pgMajor) p.pgMajor = major
    const minorList = (e2?.versions?.[major]) || []
    if (obj.pgVersion && !minorList.includes(obj.pgVersion)) p.pgVersion = ''
    if (Object.keys(p).length) patch(obj.id, p)
  }, [imgs, obj.id, obj.os, obj.osVersion, obj.arch, obj.pgMajor, obj.pgVersion, deployed]) // eslint-disable-line react-hooks/exhaustive-deps
  return imgs
}

// PatroniFrameForm edits a Patroni PostgreSQL cluster frame: catalog OS/version/arch
// + PG major/minor, superuser password, optional pgBackRest → SeaweedFS S3 backup,
// PMM/proxy/cert. Members are resizable 3–7 (etcd quorum; odd recommended).
function PatroniFrameForm({ frame: f, nodes, frameNodes, patchFrame, deleteFrame, deployed }) {
  const imgs = usePPGCatalog(f, deployed, patchFrame)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const seaweedNodes = nodes.filter((n) => n.type === 'seaweedfs')
  const members = frameNodes.length

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === f.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion && i.arch === f.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.pgMajor]) || []

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Patroni Cluster</span>
        <Badge tone="primary">{members} node{members === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Becomes the Patroni scope + pgBackRest stanza; must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={f.arch} disabled={deployed} onChange={(e) => patchFrame(f.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PostgreSQL major">
          <select className={`${inputCls} ${lock}`} value={f.pgMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { pgMajor: e.target.value, pgVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>
      <Field label="PostgreSQL minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.pgVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { pgVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.usePgBackRest} disabled={deployed} onChange={(e) => patchFrame(f.id, { usePgBackRest: e.target.checked })} />
        <span>Use pgBackRest (SeaweedFS S3) for cloning + backup</span>
      </label>
      {f.usePgBackRest && (
        <Field label="SeaweedFS node (S3 repository)" hint={seaweedNodes.length ? 'WAL archive + initial full backup land here; replicas clone via pgBackRest. The node must have S3 TLS enabled (pgBackRest needs HTTPS).' : 'Add a SeaweedFS node (with S3 TLS enabled) to the stack first.'}>
          <select className={`${inputCls} ${lock}`} value={f.seaweedfsNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { seaweedfsNodeId: e.target.value })}>
            <option value="">select a SeaweedFS node…</option>
            {seaweedNodes.map((s) => <option key={s.id} value={s.id}>{s.label}{s.tls ? '' : ' — needs S3 TLS'}</option>)}
          </select>
        </Field>
      )}

      <Field label="Monitored by (PMM)" hint="Optional — registers each member's PostgreSQL with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA (PostgreSQL TLS)</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(members < 3 || members > 7 || members % 2 === 0) && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          {members < 3 && <div>At least 3 members are required for etcd quorum ({members} now).</div>}
          {members > 7 && <div>At most 7 members are allowed ({members} now).</div>}
          {members % 2 === 0 && members >= 3 && members <= 7 && <div>An odd number of members keeps etcd quorum on a split network ({members} now).</div>}
        </div>
      )}
      <p className="text-xs text-muted">Each member runs PostgreSQL + Patroni + an etcd member. Use the +/− buttons on the frame to resize (3–7 members). Link an HAProxy node to route writes → leader and reads → replicas.</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// PatroniMemberForm edits a Patroni cluster member: only host-port export of 5432
// (OS/version come from the frame; Patroni auto-elects the leader).
function PatroniMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <p className="text-xs text-muted">Runs PostgreSQL + Patroni + an etcd member. Patroni auto-elects the leader; replicas stream from it.</p>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export PostgreSQL port to the host (5432)</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// RepmgrFrameForm edits a repmgr PostgreSQL cluster frame: catalog OS/version/arch +
// PG major/minor, superuser password, optional Barman cloud → SeaweedFS S3 backup,
// PMM/proxy/cert. Members are resizable 3–7.
function RepmgrFrameForm({ frame: f, nodes, frameNodes, patchFrame, deleteFrame, deployed }) {
  const imgs = usePPGCatalog(f, deployed, patchFrame)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const seaweedNodes = nodes.filter((n) => n.type === 'seaweedfs')
  const members = frameNodes.length

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === f.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion && i.arch === f.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.pgMajor]) || []

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">repmgr Cluster</span>
        <Badge tone="primary">{members} node{members === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Becomes the Barman server name; must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={f.arch} disabled={deployed} onChange={(e) => patchFrame(f.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PostgreSQL major">
          <select className={`${inputCls} ${lock}`} value={f.pgMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { pgMajor: e.target.value, pgVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>
      <Field label="PostgreSQL minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.pgVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { pgVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.useBarman} disabled={deployed} onChange={(e) => patchFrame(f.id, { useBarman: e.target.checked })} />
        <span>Use Barman (SeaweedFS S3) for backups</span>
      </label>
      {f.useBarman && (
        <Field label="SeaweedFS node (S3 backup storage)" hint={seaweedNodes.length ? 'WAL archive + base backups land here via barman-cloud (works over HTTP or HTTPS).' : 'Add a SeaweedFS node to the stack first.'}>
          <select className={`${inputCls} ${lock}`} value={f.seaweedfsNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { seaweedfsNodeId: e.target.value })}>
            <option value="">select a SeaweedFS node…</option>
            {seaweedNodes.map((s) => <option key={s.id} value={s.id}>{s.label}</option>)}
          </select>
        </Field>
      )}

      <Field label="Monitored by (PMM)" hint="Optional — registers each member's PostgreSQL with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA (PostgreSQL TLS)</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(members < 3 || members > 7 || members % 2 === 0) && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          {members < 3 && <div>At least 3 members are required ({members} now).</div>}
          {members > 7 && <div>At most 7 members are allowed ({members} now).</div>}
          {members % 2 === 0 && members >= 3 && members <= 7 && <div>An odd number of members keeps a clear quorum on a split network ({members} now).</div>}
        </div>
      )}
      <p className="text-xs text-muted">Each member runs PostgreSQL + repmgr; member 1 starts as the primary and the rest stream as standbys. repmgrd handles automatic failover. Use the +/− buttons on the frame to resize (3–7 members).</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// RepmgrMemberForm edits a repmgr cluster member: only host-port export of 5432
// (OS/version come from the frame; repmgr manages roles + failover).
function RepmgrMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <p className="text-xs text-muted">Runs PostgreSQL + repmgr. The cluster's first node bootstraps as primary; this node streams from it (repmgr can fail over to it).</p>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export PostgreSQL port to the host (5432)</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// SpockFrameForm edits a Spock PostgreSQL cluster frame: catalog OS/version/arch + PG
// major/minor, PMM/proxy/cert. Every member is writable (full-mesh active-active). 2–7
// members; no odd-count requirement (no quorum/failover). Spock is compiled from source.
function SpockFrameForm({ frame: f, nodes, frameNodes, patchFrame, deleteFrame, deployed }) {
  const imgs = usePPGCatalog(f, deployed, patchFrame)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const members = frameNodes.length

  // Spock compiles PostgreSQL from source with its patches — Oracle Linux only for now.
  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))].filter((o) => o === 'oraclelinux')
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === f.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion && i.arch === f.arch)
  // Spock 5.x supports PG 15/16/17 — restrict the major picker accordingly.
  const majors = (entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []).filter((m) => Number(m) >= 15)
  const minors = (entry?.versions?.[f.pgMajor]) || []

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Spock Cluster</span>
        <Badge tone="primary">{members} node{members === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={f.arch} disabled={deployed} onChange={(e) => patchFrame(f.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PostgreSQL major" hint="Spock supports 15–17.">
          <select className={`${inputCls} ${lock}`} value={f.pgMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { pgMajor: e.target.value, pgVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>
      <Field label="PostgreSQL minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.pgVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { pgVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers each member's PostgreSQL with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={f.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} disabled={deployed} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA (PostgreSQL TLS)</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(members < 2 || members > 7) && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          {members < 2 && <div>At least 2 members are required ({members} now).</div>}
          {members > 7 && <div>At most 7 members are allowed ({members} now).</div>}
        </div>
      )}
      <p className="text-xs text-muted">Every member compiles a <span className="text-fg/80">patched PostgreSQL from source</span> (Spock's patches) plus the pgEdge Spock extension — a full-mesh active-active cluster where any node is writable and changes replicate to all others (last-update-wins conflicts). A demo database <span className="font-mono">spockdemo</span> is set up for replication. Oracle Linux only; the source build adds several minutes per node. Use the +/− buttons to resize (2–7 members).</p>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// SpockMemberForm edits a Spock cluster member: only host-port export of 5432 (OS/version
// come from the frame; every member is an equal writable node in the mesh).
function SpockMemberForm({ node: n, frame, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack."><input className={`${inputCls} opacity-70`} value={n.label} readOnly /></Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <p className="text-xs text-muted">PostgreSQL + Spock — a writable member of the active-active mesh. Writes here replicate to every peer, and it receives their writes too.</p>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export PostgreSQL port to the host (5432)</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// HAProxyForm edits a (not-yet-running) HAProxy node: it must be linked to exactly one
// Patroni or PXC cluster frame by an association line (mutually exclusive). Image
// OS/version/arch come from the generic images catalog; host-port export publishes the
// write/read/stats ports.
function HAProxyForm({ node: n, nodes, frames, edges, patchNode, deleteNode, dep, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.imagesCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === n.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === n.os && i.osVersion === n.osVersion).map((i) => i.arch))]

  // Snap invalid dependent selects once the catalog loads.
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

  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  // Directly-linked backend cluster frame(s). HAProxy fronts exactly one — a Patroni
  // PostgreSQL cluster OR a PXC cluster (mutually exclusive).
  const linkedFrames = (() => {
    const out = []
    const seen = new Set()
    for (const e of edges) {
      const other = e.from.node === n.id ? e.to.node : (e.to.node === n.id ? e.from.node : null)
      if (!other) continue
      const f = frames.find((fr) => fr.id === other && (fr.type === 'patroni' || fr.type === 'pxc'))
      if (f && !seen.has(f.id)) { seen.add(f.id); out.push(f) }
    }
    return out
  })()
  const linkedFrame = linkedFrames.length === 1 ? linkedFrames[0] : null
  const isPXC = linkedFrame?.type === 'pxc'

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">HAProxy</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>

      {linkedFrames.length > 1 ? (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          Linked to multiple clusters — HAProxy fronts exactly one (Patroni and PXC are mutually exclusive). Remove the extra association line.
        </div>
      ) : linkedFrame ? (
        <div className="rounded-lg border border-primary/30 bg-primary/10 px-2.5 py-1.5 text-xs text-primary">
          Routes to {isPXC ? 'PXC' : 'Patroni'} cluster <span className="font-mono font-medium">{linkedFrame.label}</span> — {isPXC ? 'writes → single writer (:5000), reads → round-robin (:5001)' : 'writes → leader (:5000), reads → replicas (:5001)'}.
        </div>
      ) : (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">
          Not linked. Draw an association line from a Patroni or PXC cluster frame to this HAProxy node.
        </div>
      )}

      <Field label="Label"><input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} /></Field>

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

      <Field label="Monitored by (PMM)" hint="Optional — registers the HAProxy service with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={n.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export ports to the host (write 5000 / read 5001 / stats 7000)</span>
      </label>
      {n.exportEnabled && (
        <Field label="Write (leader) host port" hint="0 / empty = random unused port. The read + stats ports get random host ports.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// PSMStandaloneForm edits a standalone PS MongoDB node: catalog OS/version/arch + PS
// MongoDB major/minor, admin password, PMM/proxy/cert and host export. (Same options
// as the replica-set frame, minus replication.)
function PSMStandaloneForm({ node: n, nodes, patchNode, deleteNode, dep, deployed }) {
  const imgs = useMongoCatalog(n, deployed, patchNode)
  const lock = deployed ? 'opacity-70' : ''
  const pmmNodes = nodes.filter((x) => x.type === 'pmm')
  const keycloakNodes = nodes.filter((x) => x.type === 'keycloak')
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PSMDB (standalone)</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>

      <Field label="Label" hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <MongoCatalogFields obj={n} imgs={imgs} deployed={deployed} patch={patchNode} />

      <Field label="Monitored by (PMM)" hint="Optional — registers this server with a PMM node.">
        <select className={`${inputCls} ${lock}`} value={n.pmmNodeId || ''} disabled={deployed} onChange={(e) => patchNode(n.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.generateCert} disabled={deployed} onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })} />
        <span>Generate certificate from Intranet CA</span>
      </label>
      {n.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={n.certTtlValue || 365} onChange={(e) => patchNode(n.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={n.certTtlUnit || 'days'} onChange={(e) => patchNode(n.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!n.exportEnabled} disabled={deployed} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export mongod port (27017) to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port.">
          <input type="number" min="0" max="65535" className={`${inputCls} ${lock}`} value={n.exportHostPort || 0} disabled={deployed}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}

      <div className="rounded-md border border-border/60 p-2 space-y-2">
        <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
          <input type="checkbox" checked={!!n.enableOIDC} disabled={deployed} onChange={(e) => patchNode(n.id, { enableOIDC: e.target.checked })} />
          <span>Keycloak OIDC authentication (MONGODB-OIDC)</span>
        </label>
        {n.enableOIDC && (
          <div className="space-y-2 pl-1">
            <Field label="Keycloak node" hint={keycloakNodes.length ? 'OIDC identity provider for this MongoDB.' : 'Add a Keycloak node first.'}>
              <select className={`${inputCls} ${lock}`} value={n.keycloakNodeId || ''} disabled={deployed || keycloakNodes.length === 0} onChange={(e) => patchNode(n.id, { keycloakNodeId: e.target.value })}>
                <option value="">none</option>
                {keycloakNodes.map((k) => <option key={k.id} value={k.id}>{k.label}</option>)}
              </select>
            </Field>
            <Field label="Realm" hint="Keycloak realm holding the OIDC client.">
              <input className={`${inputCls} ${lock}`} value={n.oidcRealm ?? 'mongodb'} disabled={deployed} onChange={(e) => patchNode(n.id, { oidcRealm: e.target.value })} />
            </Field>
            <Field label="Client ID" hint="OIDC client id; also used as the token audience.">
              <input className={`${inputCls} ${lock}`} value={n.oidcClientId ?? 'mongodb-client'} disabled={deployed} onChange={(e) => patchNode(n.id, { oidcClientId: e.target.value })} />
            </Field>
            <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
              <input type="checkbox" checked={n.oidcUseAuthClaim !== false} disabled={deployed} onChange={(e) => patchNode(n.id, { oidcUseAuthClaim: e.target.checked })} />
              <span>Authorize by group claim</span>
            </label>
            {n.oidcUseAuthClaim !== false ? (
              <Field label="Authorization claim" hint="Token claim with the user's groups. Creates keycloak/developers + keycloak/dbadmins roles.">
                <input className={`${inputCls} ${lock}`} value={n.oidcAuthClaim ?? 'MyClaim'} disabled={deployed} onChange={(e) => patchNode(n.id, { oidcAuthClaim: e.target.value })} />
              </Field>
            ) : (
              <p className="text-xs text-muted">Users are authorized by username — create them in the <span className="font-mono">$external</span> database after deploy.</p>
            )}
          </div>
        )}
      </div>

      {!deployed && <p className="text-xs text-muted">Access links and credentials appear here after deploy.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// Minimap: a scaled overview of the canvas in the bottom-right corner showing
// every node (colored by type) and the current viewport. Click or drag inside it
// to recenter the main view on that point.
const MINI_W = 184
const MINI_H = 124
const MINI_PAD = 8

function Minimap({ nodes, view, setView, wrapRef, selectedId }) {
  const drag = useRef(false)
  const rect = wrapRef.current?.getBoundingClientRect()
  const vw = rect?.width || 800
  const vh = rect?.height || 600

  // Current viewport expressed in world coordinates.
  const viewWorld = { x: -view.x / view.z, y: -view.y / view.z, w: vw / view.z, h: vh / view.z }

  // Bounds over all nodes plus the viewport, so both are always visible.
  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity
  for (const n of nodes) {
    minX = Math.min(minX, n.x); minY = Math.min(minY, n.y)
    maxX = Math.max(maxX, n.x + NODE_W); maxY = Math.max(maxY, n.y + NODE_H)
  }
  minX = Math.min(minX, viewWorld.x); minY = Math.min(minY, viewWorld.y)
  maxX = Math.max(maxX, viewWorld.x + viewWorld.w); maxY = Math.max(maxY, viewWorld.y + viewWorld.h)
  if (!isFinite(minX)) { minX = 0; minY = 0; maxX = 1; maxY = 1 }

  const bw = (maxX - minX) || 1
  const bh = (maxY - minY) || 1
  const scale = Math.min((MINI_W - 2 * MINI_PAD) / bw, (MINI_H - 2 * MINI_PAD) / bh)
  const ox = MINI_PAD + ((MINI_W - 2 * MINI_PAD) - bw * scale) / 2 - minX * scale
  const oy = MINI_PAD + ((MINI_H - 2 * MINI_PAD) - bh * scale) / 2 - minY * scale
  const tx = (x) => ox + x * scale
  const ty = (y) => oy + y * scale

  function recenter(e) {
    const r = e.currentTarget.getBoundingClientRect()
    const wx = (e.clientX - r.left - ox) / scale
    const wy = (e.clientY - r.top - oy) / scale
    setView((v) => ({ ...v, x: vw / 2 - wx * v.z, y: vh / 2 - wy * v.z }))
  }

  return (
    <div
      className="absolute bottom-3 right-3 overflow-hidden rounded-lg border bg-surface/90 shadow backdrop-blur"
      style={{ width: MINI_W, height: MINI_H }}
      title="Minimap — click or drag to navigate"
    >
      <svg
        width={MINI_W}
        height={MINI_H}
        className="cursor-pointer"
        style={{ touchAction: 'none' }}
        onPointerDown={(e) => { e.stopPropagation(); drag.current = true; recenter(e) }}
        onPointerMove={(e) => { if (drag.current) recenter(e) }}
        onPointerUp={() => { drag.current = false }}
        onPointerLeave={() => { drag.current = false }}
      >
        <rect
          x={tx(viewWorld.x)} y={ty(viewWorld.y)}
          width={viewWorld.w * scale} height={viewWorld.h * scale}
          fill="var(--primary)" fillOpacity="0.12" stroke="var(--primary)" strokeWidth="1"
        />
        {nodes.map((n) => {
          const def = NODE_TYPES[n.type] || {}
          const on = selectedId === n.id
          return (
            <rect
              key={n.id}
              x={tx(n.x)} y={ty(n.y)}
              width={Math.max(2, NODE_W * scale)} height={Math.max(2, NODE_H * scale)}
              rx="1"
              fill={def.color || 'var(--muted)'} fillOpacity={on ? 1 : 0.8}
              stroke={on ? 'var(--fg)' : 'none'} strokeWidth="1"
            />
          )
        })}
      </svg>
    </div>
  )
}

function PortHandles({ ownerId, connecting, snapPort, onStart }) {
  const pos = {
    top: '-top-2 left-1/2 -translate-x-1/2',
    right: '-right-2 top-1/2 -translate-y-1/2',
    bottom: '-bottom-2 left-1/2 -translate-x-1/2',
    left: '-left-2 top-1/2 -translate-y-1/2',
  }
  return (
    <>
      {PORTS.map((port) => {
        const snap = snapPort === port
        return (
          <button
            key={port}
            onPointerDown={(e) => onStart(e, ownerId, port)}
            className={`absolute h-3 w-3 rounded-full border-2 border-primary bg-surface transition ${pos[port]} ${connecting ? 'opacity-100' : 'opacity-0 group-hover:opacity-100'} ${snap ? 'pulse-ring scale-150 bg-primary' : ''}`}
          />
        )
      })}
    </>
  )
}

function ContextMenu({ menu, onClose, actions }) {
  const x = Math.min(menu.x, window.innerWidth - 200)
  const y = Math.min(menu.y, window.innerHeight - 160)
  return createPortal(
    <div className="fixed inset-0 z-50" onClick={onClose} onContextMenu={(e) => { e.preventDefault(); onClose() }}>
      <div className="absolute w-52 rounded-lg border bg-surface p-1 shadow-xl" style={{ left: x, top: y }} onClick={(e) => e.stopPropagation()}>
        {actions.map((a, i) =>
          a.sep ? (
            <div key={i} className="my-1 h-px bg-border" />
          ) : (
            <button
              key={i}
              onClick={() => { a.fn(); onClose() }}
              className={`block w-full rounded-md px-2.5 py-1.5 text-left text-sm hover:bg-surface2 ${a.danger ? 'text-danger' : 'text-fg'}`}
            >
              {a.label}
            </button>
          ),
        )}
      </div>
    </div>,
    document.body,
  )
}

const PROPS_KEY = 'dbcanvas-props-layout'
function loadProps() {
  try { return JSON.parse(localStorage.getItem(PROPS_KEY) || '{}') } catch { return {} }
}

function StackProperties({ selected, stackId, nodes, edges, frames, depByNode, patchNode, patchFrame, patchEdge, deleteNode, deleteEdge, deleteFrame, rebuildMongoCluster, deployOpen, deployments, onDeployMinimize }) {
  const selNode = selected?.kind === 'node' ? nodes.find((n) => n.id === selected.id) : null
  const selDep = selNode ? depByNode[selNode.id] : null
  const wide = (selDep && selDep.state === 'running' && (selNode.type === 'intranet' || selNode.type === 'pmm' || selNode.type === 'pxc' || selNode.type === 'proxysql' || selNode.type === 'mysql' || selNode.type === 'ps' || selNode.type === 'innodb' || selNode.type === 'psmdb' || selNode.type === 'psmrs' || selNode.type === 'psm' || selNode.type === 'seaweedfs' || selNode.type === 'patroni' || selNode.type === 'haproxy' || selNode.type === 'pg' || selNode.type === 'repmgr' || selNode.type === 'spock')) || selected?.kind === 'frame'

  const saved = useRef(loadProps()).current
  const [docked, setDocked] = useState(saved.docked !== false)
  const [width, setWidth] = useState(saved.width || 288)
  const [flt, setFlt] = useState(saved.float || { x: Math.max(20, (typeof window !== 'undefined' ? window.innerWidth : 1200) - 500), y: 96, w: 460, h: 540 })
  const drag = useRef(null)

  // give management tabs room when a running Intranet is selected (docked)
  useEffect(() => { if (wide && docked && width < 440) setWidth(440) }, [wide, docked, width])
  useEffect(() => { try { localStorage.setItem(PROPS_KEY, JSON.stringify({ docked, width, float: flt })) } catch { /* */ } }, [docked, width, flt])

  useEffect(() => {
    const onMove = (e) => {
      const d = drag.current
      if (!d) return
      if (d.kind === 'w') setWidth(Math.min(680, Math.max(260, d.w0 + (d.x0 - e.clientX))))
      else if (d.kind === 'move') setFlt((f) => ({ ...f, x: d.fx + (e.clientX - d.x0), y: d.fy + (e.clientY - d.y0) }))
      else if (d.kind === 'wh') setFlt((f) => ({ ...f, w: Math.max(280, d.w0 + (e.clientX - d.x0)), h: Math.max(220, d.h0 + (e.clientY - d.y0)) }))
    }
    const onUp = () => { drag.current = null }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => { removeEventListener('pointermove', onMove); removeEventListener('pointerup', onUp) }
  }, [])

  const Header = ({ move }) => (
    <div
      className="mb-3 flex items-center justify-between"
      onPointerDown={move ? (e) => { if (e.target.closest('button')) return; drag.current = { kind: 'move', x0: e.clientX, y0: e.clientY, fx: flt.x, fy: flt.y } } : undefined}
      style={move ? { cursor: 'move' } : undefined}
    >
      <h3 className="text-sm font-semibold">Properties</h3>
      <button onClick={() => setDocked((d) => !d)} title={docked ? 'Detach' : 'Dock'} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
        <Icon.Frame size={14} />
      </button>
    </div>
  )
  const body = <Body selected={selected} stackId={stackId} nodes={nodes} edges={edges} frames={frames} depByNode={depByNode} patchNode={patchNode} patchFrame={patchFrame} patchEdge={patchEdge} deleteNode={deleteNode} deleteEdge={deleteEdge} deleteFrame={deleteFrame} rebuildMongoCluster={rebuildMongoCluster} />

  // Docked deployment console lives at the bottom of this column (under Properties).
  const deployConsole = deployOpen && (
    <DeploymentConsole deployments={deployments} nodes={nodes} onMinimize={onDeployMinimize} inline columnWidth={width} />
  )

  if (docked) {
    return (
      <div className="relative flex shrink-0 flex-col gap-4" style={{ width }}>
        <div
          onPointerDown={(e) => { drag.current = { kind: 'w', x0: e.clientX, w0: width } }}
          className="absolute left-0 top-0 z-10 h-full w-1.5 -translate-x-1 cursor-ew-resize hover:bg-primary"
          title="Drag to resize"
        />
        <div className="min-h-0 flex-1 overflow-auto rounded-xl border bg-surface p-4">
          <Header move={false} />
          {body}
        </div>
        {deployConsole}
      </div>
    )
  }
  return (
    <>
      {createPortal(
        <div className="fixed z-40 flex flex-col overflow-hidden rounded-xl border bg-surface shadow-2xl"
          style={{ left: flt.x, top: flt.y, width: flt.w, height: flt.h }}>
          <div className="flex-1 overflow-auto p-4">
            <Header move />
            {body}
          </div>
          <div
            onPointerDown={(e) => { drag.current = { kind: 'wh', x0: e.clientX, y0: e.clientY, w0: flt.w, h0: flt.h } }}
            className="absolute bottom-0 right-0 h-4 w-4 cursor-nwse-resize text-muted"
          >
            <svg viewBox="0 0 10 10" className="h-full w-full"><path d="M9 1 L1 9 M9 5 L5 9" stroke="currentColor" fill="none" /></svg>
          </div>
        </div>,
        document.body,
      )}
      {/* Properties is detached, so the docked console can't sit under it — pin it
          to the right-column bottom instead (handled by DeploymentConsole). */}
      {deployOpen && (
        <DeploymentConsole deployments={deployments} nodes={nodes} onMinimize={onDeployMinimize} columnWidth={width} />
      )}
    </>
  )
}

function Body({ selected, stackId, nodes, edges, frames, depByNode, patchNode, patchFrame, patchEdge, deleteNode, deleteEdge, deleteFrame, rebuildMongoCluster }) {
  if (!selected) return <p className="text-sm text-muted">Select a node, link or PXC cluster to edit it. Add an Intranet node from the toolbar to begin.</p>

  if (selected.kind === 'frame') {
    const f = frames.find((x) => x.id === selected.id)
    if (!f) return null
    const frameNodes = nodes.filter((n) => n.frameId === f.id)
    const deployed = frameNodes.some((n) => depByNode[n.id])
    const running = frameNodes.some((n) => depByNode[n.id]?.state === 'running')
    if (f.type === 'proxysql') {
      return <ProxySQLFrameForm frame={f} nodes={nodes} frames={frames} edges={edges} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'mysql') {
      return <MySQLFrameForm frame={f} nodes={nodes} frames={frames} edges={edges} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'innodb') {
      return <InnoDBFrameForm frame={f} nodes={nodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'psmdb') {
      return <MongoDBFrameForm frame={f} nodes={nodes} patchFrame={patchFrame} deleteFrame={deleteFrame} rebuildCluster={rebuildMongoCluster} deployed={deployed} />
    }
    if (f.type === 'psmrs') {
      return <PSMRSFrameForm frame={f} nodes={nodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'patroni') {
      return <PatroniFrameForm frame={f} nodes={nodes} frameNodes={frameNodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'repmgr') {
      return <RepmgrFrameForm frame={f} nodes={nodes} frameNodes={frameNodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'spock') {
      return <SpockFrameForm frame={f} nodes={nodes} frameNodes={frameNodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    if (f.type === 'valkeycluster') {
      return <ValkeyClusterFrameForm frame={f} nodes={nodes} frameNodes={frameNodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
    }
    return <PXCFrameForm frame={f} stackId={stackId} nodes={nodes} frameNodes={frameNodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} running={running} />
  }

  if (selected.kind === 'node') {
    const n = nodes.find((x) => x.id === selected.id)
    if (!n) return null
    const dep = depByNode[n.id]
    const deployed = !!dep

    // PXC cluster member node.
    if (n.type === 'pxc') {
      if (dep && dep.state === 'running') {
        return <PXCManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PXCNodeForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} nodes={nodes} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // MySQL replication member node.
    if (n.type === 'mysql') {
      if (dep && dep.state === 'running') {
        return <MySQLManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <MySQLMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} nodes={nodes} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // InnoDB Cluster / GR member node.
    if (n.type === 'innodb') {
      if (dep && dep.state === 'running') {
        return <InnoDBManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <InnoDBMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // PS MongoDB sharded-cluster member node.
    if (n.type === 'psmdb') {
      if (dep && dep.state === 'running') {
        return <MongoDBManager stackId={stackId} nodeId={n.id} frameId={n.frameId} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <MongoDBMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // PS MongoDB replica-set member node.
    if (n.type === 'psmrs') {
      if (dep && dep.state === 'running') {
        return <MongoDBManager stackId={stackId} nodeId={n.id} frameId={n.frameId} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PSMRSMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // Standalone PS MongoDB node.
    if (n.type === 'psm') {
      if (dep && dep.state === 'running') {
        return <MongoDBManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PSMStandaloneForm node={n} nodes={nodes} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }

    // Patroni PostgreSQL cluster member node.
    if (n.type === 'patroni') {
      if (dep && dep.state === 'running') {
        return <PatroniManager stackId={stackId} nodeId={n.id} frame={frames.find((fr) => fr.id === n.frameId)} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PatroniMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // repmgr PostgreSQL cluster member node.
    if (n.type === 'repmgr') {
      if (dep && dep.state === 'running') {
        return <RepmgrManager stackId={stackId} nodeId={n.id} frame={frames.find((fr) => fr.id === n.frameId)} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <RepmgrMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }
    // Spock cluster member node.
    if (n.type === 'spock') {
      if (dep && dep.state === 'running') {
        return <SpockManager stackId={stackId} nodeId={n.id} frame={frames.find((fr) => fr.id === n.frameId)} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <SpockMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }
    // Valkey cluster member node.
    if (n.type === 'valkeycluster') {
      if (dep && dep.state === 'running') {
        return <ValkeyManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <ValkeyClusterMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    // HAProxy node (load balancer for a Patroni cluster).
    if (n.type === 'haproxy') {
      if (dep && dep.state === 'running') {
        return <HAProxyManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <HAProxyForm node={n} nodes={nodes} frames={frames} edges={edges} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }

    const def = NODE_TYPES[n.type] || NODE_TYPES.intranet

    // Deployed + running Intranet → full management console.
    if (dep && dep.state === 'running' && n.type === 'intranet') {
      return <IntranetManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
    }
    // Deployed + running PMM → PMM management console.
    if (dep && dep.state === 'running' && n.type === 'pmm') {
      return <PMMManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
    }
    // Deployed + running ProxySQL → ProxySQL management console.
    if (dep && dep.state === 'running' && n.type === 'proxysql') {
      return <ProxySQLManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
    }
    // ProxySQL (not yet running): a cluster member shows the per-member form; a
    // standalone node shows the full options form.
    if (n.type === 'proxysql') {
      if (n.frameId) {
        return <ProxySQLFrameMemberForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} patchNode={patchNode} dep={dep} deployed={deployed} />
      }
      return <ProxySQLForm node={n} nodes={nodes} frames={frames} edges={edges} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Standalone Percona Server node.
    if (n.type === 'ps') {
      if (dep && dep.state === 'running') {
        return <MySQLManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PerconaServerForm node={n} nodes={nodes} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Standalone PostgreSQL node.
    if (n.type === 'pg') {
      if (dep && dep.state === 'running') {
        return <PGManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PostgreSQLForm node={n} nodes={nodes} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // SeaweedFS object-storage node.
    if (n.type === 'seaweedfs') {
      if (dep && dep.state === 'running') {
        return <SeaweedFSManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <SeaweedFSForm node={n} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Watchtower singleton node (container auto-upgrades for PMM).
    if (n.type === 'watchtower') {
      if (dep && dep.state === 'running') {
        return <WatchtowerManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <WatchtowerForm node={n} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Keycloak singleton node (OIDC identity provider).
    if (n.type === 'keycloak') {
      if (dep && dep.state === 'running') {
        return <KeycloakManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <KeycloakForm node={n} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Ubuntu VNC desktop node.
    if (n.type === 'vnc') {
      if (dep && dep.state === 'running') {
        return <VNCManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <VNCForm node={n} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    // Standalone Valkey node.
    if (n.type === 'valkey') {
      if (dep && dep.state === 'running') {
        return <ValkeyManager dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <ValkeyForm node={n} nodes={nodes} patchNode={patchNode} deleteNode={deleteNode} dep={dep} deployed={deployed} />
    }
    return (
      <div className="space-y-3">
        {dep && (
          <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
            <span className="text-muted">Deployment</span>
            <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
          </div>
        )}
        <Field label="Label">
          <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
        </Field>
        <Field label="Type">
          <input className={`${inputCls} opacity-70`} value={def.label} readOnly />
        </Field>
        <Field label="Operating system" hint={deployed ? 'Locked — the node is deployed.' : 'Locked once the stack is deployed.'}>
          <select
            className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
            value={n.os}
            disabled={deployed}
            onChange={(e) => patchNode(n.id, { os: e.target.value })}
          >
            {def.osOptions.map((o) => (
              <option key={o.id} value={o.id}>{o.label}</option>
            ))}
          </select>
        </Field>
        <Field label="Architecture" hint={deployed ? 'Locked — the node is deployed.' : n.type === 'pmm' ? 'PMM currently ships amd64 only.' : 'Must have a matching image built (make images).'}>
          <select
            className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
            value={n.type === 'pmm' ? 'amd64' : (n.arch || 'amd64')}
            disabled={deployed}
            onChange={(e) => patchNode(n.id, { arch: e.target.value })}
          >
            {/* PMM (percona/pmm-server) has no arm64 image yet — offer amd64 only. */}
            {(n.type === 'pmm' ? ARCH_OPTIONS.filter((o) => o.id !== 'arm64') : ARCH_OPTIONS).map((o) => (
              <option key={o.id} value={o.id}>{o.label}</option>
            ))}
          </select>
        </Field>
        {n.type === 'pmm' && <PMMOptions n={n} nodes={nodes} patchNode={patchNode} deployed={deployed} />}
        <div className="grid grid-cols-2 gap-2">
          <Field label="X">
            <input type="number" className={inputCls} value={Math.round(n.x)} onChange={(e) => patchNode(n.id, { x: +e.target.value })} />
          </Field>
          <Field label="Y">
            <input type="number" className={inputCls} value={Math.round(n.y)} onChange={(e) => patchNode(n.id, { y: +e.target.value })} />
          </Field>
        </div>
        {!deployed && <p className="text-xs text-muted">Management tabs (LDAP, email, certificate, credentials, terminal) appear here after deploy.</p>}
        <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
          <Icon.Trash size={16} /> Delete node
        </Button>
      </div>
    )
  }

  const ed = edges.find((x) => x.id === selected.id)
  if (!ed) return null
  if (ed.type === 'async' || ed.type === 'bidir') {
    return <ReplicationLinkForm ed={ed} nodes={nodes} patchEdge={patchEdge} deleteEdge={deleteEdge} />
  }
  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-sm">
        <span className="font-mono text-xs">{ed.from.node}.{ed.from.port}</span>
        <span className="mx-1 text-muted">→</span>
        <span className="font-mono text-xs">{ed.to.node}.{ed.to.port}</span>
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteEdge(ed.id)}>
        <Icon.Trash size={16} /> Delete link
      </Button>
    </div>
  )
}

// ReplicationLinkForm edits a cross-cluster replication link: switch between async
// (either direction) and bidirectional, or delete it. Changes take effect on the
// next Deploy (replication is reconciled at deploy time). Options are anchored to a
// stable node pair (sorted ids) so the active choice doesn't jump when reversed.
function ReplicationLinkForm({ ed, nodes, patchEdge, deleteEdge }) {
  const ends = { [ed.from.node]: ed.from, [ed.to.node]: ed.to }
  const [idA, idB] = [ed.from.node, ed.to.node].sort()
  const endA = ends[idA]
  const endB = ends[idB]
  const labelOf = (id) => nodes.find((n) => n.id === id)?.label || id
  const lA = labelOf(idA)
  const lB = labelOf(idB)
  const current = ed.type === 'bidir' ? 'bidir' : (ed.from.node === idA ? 'ab' : 'ba')
  const opts = [
    { key: 'ab', label: `${lA} → ${lB}`, hint: 'async', apply: () => patchEdge(ed.id, { type: 'async', from: endA, to: endB }) },
    { key: 'ba', label: `${lB} → ${lA}`, hint: 'async', apply: () => patchEdge(ed.id, { type: 'async', from: endB, to: endA }) },
    { key: 'bidir', label: `${lA} ↔ ${lB}`, hint: 'bidirectional', apply: () => patchEdge(ed.id, { type: 'bidir' }) },
  ]
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Replication link</span>
        <Badge tone="success">{ed.type === 'bidir' ? 'bidirectional' : 'async'}</Badge>
      </div>
      <p className="text-xs text-muted">
        Cross-cluster replication between two cluster members. The arrow points from source to replica;
        bidirectional makes each a replica of the other. Applied (and reconciled) on the next Deploy.
      </p>
      <div className="space-y-2">
        {opts.map((o) => (
          <button key={o.key} onClick={o.apply}
            className={`flex w-full items-center justify-between rounded-lg border px-3 py-2 text-sm ${current === o.key ? 'border-primary bg-primary/10' : 'hover:border-primary hover:bg-primary/5'}`}>
            <span className="font-mono">{o.label}</span>
            <span className="text-[11px] text-muted">{o.hint}</span>
          </button>
        ))}
      </div>
      {ed.type === 'bidir' && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          Bidirectional replication is multi-writer — avoid writing the same rows on both sides.
        </div>
      )}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteEdge(ed.id)}>
        <Icon.Trash size={16} /> Delete replication link
      </Button>
    </div>
  )
}

// ReplicationLinkModal asks for the direction/type when a replication link is drawn
// between two cluster members (PXC or Percona Server, in different frames).
function ReplicationLinkModal({ prompt, nodes, frames, onClose, onChoose }) {
  const { e1, e2 } = prompt
  const node = (id) => nodes.find((n) => n.id === id)
  const n1 = node(e1.node)
  const n2 = node(e2.node)
  const frameLabel = (n) => frames.find((f) => f.id === n?.frameId)?.label || ''
  const l1 = n1?.label || 'node'
  const l2 = n2?.label || 'node'
  const opts = [
    { from: e1, to: e2, mode: 'async', label: `${l1} → ${l2}`, hint: 'async — replica reads from source' },
    { from: e2, to: e1, mode: 'async', label: `${l2} → ${l1}`, hint: 'async — replica reads from source' },
    { from: e1, to: e2, mode: 'bidir', label: `${l1} ↔ ${l2}`, hint: 'bidirectional — each replicates from the other' },
  ]
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-1 text-sm font-semibold">Set up replication</h3>
        <p className="mb-3 text-xs text-muted">
          Between <span className="font-semibold">{l1}</span> ({frameLabel(n1)}) and <span className="font-semibold">{l2}</span> ({frameLabel(n2)}).
          Configured at deploy time (GTID auto-position when both clusters use GTID, else binlog file/position).
        </p>
        <div className="space-y-2">
          {opts.map((o, i) => (
            <button key={i} onClick={() => onChoose(o.from, o.to, o.mode)}
              className="flex w-full items-center justify-between rounded-lg border px-3 py-2 text-left text-sm hover:border-primary hover:bg-primary/10">
              <span className="font-mono">{o.label}</span>
              <span className="ml-2 text-[11px] text-muted">{o.hint}</span>
            </button>
          ))}
        </div>
        <div className="mt-4 flex justify-end">
          <Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button>
        </div>
      </div>
    </div>,
    document.body,
  )
}
