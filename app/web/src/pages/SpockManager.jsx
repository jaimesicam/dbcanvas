import { useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE } from '../lib/stackApi.js'
import { PGGatherCard } from '../components/Diagnostics.jsx'
import PGCertTab from '../components/PGCertTab.jsx'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'creds', label: 'Credentials' },
  { id: 'cert', label: 'Certificate' },
  { id: 'replication', label: 'Replication' },
  { id: 'diag', label: 'Diagnostics' },
]

function CopyButton({ text, title = 'Copy', size = 14 }) {
  const [done, setDone] = useState(false)
  return (
    <button title={title}
      onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* ignore */ } setDone(true); setTimeout(() => setDone(false), 1200) }}
      className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
      {done ? <Icon.Check size={size} /> : <Icon.Copy size={size} />}
    </button>
  )
}

function KV({ k, v, mono }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="text-muted">{k}</span>
      <span className={`truncate text-fg ${mono ? 'font-mono text-xs' : ''}`}>{v || '—'}</span>
    </div>
  )
}

function Row({ k, v }) {
  if (!v) return null
  return (
    <div>
      <div className="text-xs text-muted">{k}</div>
      <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
        <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{v}</span>
        <CopyButton text={v} />
      </div>
    </div>
  )
}

function CodeBlock({ label, text }) {
  return (
    <div>
      <div className="mb-1 flex items-center justify-between">
        <span className="text-xs font-medium text-muted">{label}</span>
        <CopyButton text={text} />
      </div>
      <pre className="max-h-56 overflow-auto whitespace-pre rounded-lg border bg-bg p-2 font-mono text-[11px] leading-relaxed text-fg">{text}</pre>
    </div>
  )
}

// SpockManager is the properties-panel console for a deployed Spock cluster member —
// a writable node in a full-mesh, active-active PostgreSQL cluster.
export default function SpockManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const cfg = dep.config || {}
  const sec = dep.secrets || {}

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Spock · {cfg.nodeName || cfg.hostname}</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>

      <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
        {TABS.map((t) => (
          <button key={t.id} onClick={() => setTab(t.id)}
            className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${tab === t.id ? 'bg-surface text-fg shadow' : 'text-muted'}`}>
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'overview' && <Overview cfg={cfg} dep={dep} onDeleteNode={onDeleteNode} />}
      {tab === 'creds' && <Creds cfg={cfg} sec={sec} />}
      {tab === 'cert' && <PGCertTab stackId={stackId} nodeId={nodeId} />}
      {tab === 'replication' && <Replication cfg={cfg} sec={sec} />}
      {tab === 'diag' && <PGGatherCard stackId={stackId} nodeId={nodeId} defaultDb={cfg.database} />}
    </div>
  )
}

function Overview({ cfg, dep, onDeleteNode }) {
  return (
    <div className="space-y-2 text-sm">
      <KV k="Cluster" v={cfg.cluster} mono />
      <KV k="Spock node" v={cfg.nodeName} mono />
      <KV k="Topology" v="Active-active (multi-master)" />
      <KV k="Mesh peers" v={(cfg.members || []).length ? `${(cfg.members || []).length} node(s)` : '—'} />
      <KV k="Database" v={cfg.database} mono />
      <KV k="FQDN" v={cfg.fqdn} mono />
      <KV k="PostgreSQL" v={cfg.pgVersion || cfg.pgMajor} mono />
      <KV k="Spock" v={cfg.spockRef ? `source @ ${cfg.spockRef}` : 'source'} mono />
      <KV k="TLS" v={cfg.generateCert ? 'Intranet-CA cert' : 'off'} />
      <KV k="Host port (5432)" v={cfg.exportPort ? String(cfg.exportPort) : 'not published'} mono />
      <KV k="Monitored by" v={cfg.monitoredBy} mono />
      <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

function Creds({ cfg, sec }) {
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const db = cfg.database || 'spockdemo'
  const conn = cfg.exportPort ? `postgresql://${sec.superUser || 'postgres'}:${sec.superPassword || ''}@${host}:${cfg.exportPort}/${db}` : ''
  const inCluster = `psql "host=${cfg.fqdn} port=5432 dbname=${db} user=${sec.superUser || 'postgres'}"`
  return (
    <div className="space-y-3">
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Superuser</div>
        <Row k="Username" v={sec.superUser || 'postgres'} />
        <Row k="Password" v={sec.superPassword} />
      </div>
      {conn && (
        <div className="space-y-2">
          <div className="text-xs font-medium text-muted">Connection (published host port)</div>
          <Row k="psql URI" v={conn} />
        </div>
      )}
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">In-cluster (from another container)</div>
        <Row k="psql" v={inCluster} />
      </div>
    </div>
  )
}

function Replication({ cfg }) {
  const db = cfg.database || 'spockdemo'
  const addTable =
`-- Replicate a NEW table: create it identically on every node (DDL is not auto-replicated),
-- then add it to the 'default' replication set on every node. It needs a PRIMARY KEY.
CREATE TABLE public.orders (id bigint PRIMARY KEY, item text, qty int);
SELECT spock.repset_add_table('default', 'public.orders');

-- Or, to replicate the DDL itself to all peers, run it via Spock on ONE node:
SELECT spock.replicate_ddl($$ CREATE TABLE public.orders (id bigint PRIMARY KEY, item text, qty int) $$);`
  const status =
`-- Cluster nodes and subscription health (run in ${db}):
SELECT node_name FROM spock.node;
SELECT subscription_name, status, provider_node FROM spock.sub_show_status();`
  const demo =
`-- Multi-master demo: write on ANY node, read it on the others.
-- On this node:
INSERT INTO public.spock_demo (id, note) VALUES (1, 'hello from ${cfg.nodeName || 'this node'}');
-- On any peer, the row appears; a write there replicates back here too.`
  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        Full-mesh <span className="font-medium text-fg/80">active-active</span> replication via pgEdge Spock: every node
        is writable and changes propagate to all peers (conflicts resolved <span className="font-medium text-fg/80">last-update-wins</span>
        via commit timestamps). The <span className="font-mono">{db}</span> database and a <span className="font-mono">spock_demo</span> table
        are pre-configured in the <span className="font-mono">default</span> replication set. DDL is not replicated automatically.
      </div>
      <CodeBlock label="Try it (multi-master)" text={demo} />
      <CodeBlock label="Add a table to replication" text={addTable} />
      <CodeBlock label="Check nodes + subscription status" text={status} />
    </div>
  )
}
