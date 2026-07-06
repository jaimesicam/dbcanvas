import { useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE } from '../lib/stackApi.js'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'access', label: 'Access' },
]

function CopyButton({ text, title = 'Copy', size = 14 }) {
  const [done, setDone] = useState(false)
  return (
    <button
      title={title}
      onClick={async () => {
        try { await navigator.clipboard.writeText(text) } catch { /* ignore */ }
        setDone(true)
        setTimeout(() => setDone(false), 1200)
      }}
      className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg"
    >
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

function Row({ k, v, link }) {
  if (!v) return null
  return (
    <div>
      <div className="text-xs text-muted">{k}</div>
      <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
        {link
          ? <a href={v} target="_blank" rel="noreferrer" className="min-w-0 flex-1 truncate font-mono text-xs text-primary hover:underline">{v}</a>
          : <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{v}</span>}
        <CopyButton text={v} />
      </div>
    </div>
  )
}

export default function HAProxyManager({ dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const cfg = dep.config || {}

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">HAProxy</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>

      <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
        {TABS.map((t) => (
          <button
            key={t.id}
            onClick={() => setTab(t.id)}
            className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${tab === t.id ? 'bg-surface text-fg shadow' : 'text-muted'}`}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'overview' && <Overview cfg={cfg} dep={dep} onDeleteNode={onDeleteNode} />}
      {tab === 'access' && <AccessTab cfg={cfg} />}
    </div>
  )
}

function Overview({ cfg, dep, onDeleteNode }) {
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const stats = cfg.statsPort ? `http://${host}:${cfg.statsPort}/` : ''
  const isPXC = cfg.backend === 'pxc'
  const writeLabel = isPXC ? 'Write port (→ writer · 5000)' : 'Write port (→ leader · 5000)'
  const readLabel = isPXC ? 'Read port (→ round-robin · 5001)' : 'Read port (→ replicas · 5001)'
  return (
    <div className="space-y-2 text-sm">
      <KV k="FQDN" v={cfg.fqdn} mono />
      <KV k="Image" v={cfg.image} mono />
      <KV k="Backend" v={isPXC ? 'Percona XtraDB Cluster' : 'Patroni PostgreSQL'} />
      <KV k="Routes to cluster" v={cfg.cluster} mono />
      <KV k="Backend members" v={(cfg.members || []).length ? `${(cfg.members || []).length} member(s)` : '—'} />
      <KV k={writeLabel} v={cfg.writePort ? String(cfg.writePort) : 'not published'} mono />
      <KV k={readLabel} v={cfg.readPort ? String(cfg.readPort) : 'not published'} mono />
      <KV k="Stats port (7000)" v={cfg.statsPort ? String(cfg.statsPort) : 'not published'} mono />
      <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
      {stats && (
        <a href={stats} target="_blank" rel="noreferrer"
          className="mt-2 flex items-center justify-center gap-2 rounded-lg border border-primary/40 bg-primary/10 px-3 py-2 text-sm font-medium text-primary hover:bg-primary/15">
          <Icon.External size={15} /> Open stats page (7000)
        </a>
      )}
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

function AccessTab({ cfg }) {
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const stats = cfg.statsPort ? `http://${host}:${cfg.statsPort}/` : ''
  if (cfg.backend === 'pxc') return <PXCAccess cfg={cfg} host={host} stats={stats} />
  const writeURI = cfg.writePort ? `postgresql://<user>:<pw>@${host}:${cfg.writePort}/postgres` : ''
  const readURI = cfg.readPort ? `postgresql://<user>:<pw>@${host}:${cfg.readPort}/postgres` : ''
  const writePsql = cfg.writePort ? `psql "host=${host} port=${cfg.writePort} dbname=postgres user=postgres"` : ''
  const readPsql = cfg.readPort ? `psql "host=${host} port=${cfg.readPort} dbname=postgres user=postgres"` : ''
  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] text-muted">
        HAProxy routes by Patroni REST health checks: the <span className="font-medium text-fg/80">write</span> port
        always lands on the current leader (writable); the <span className="font-medium text-fg/80">read</span> port
        round-robins the streaming replicas (read-only). On failover the write port follows the new leader automatically.
      </div>
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Write (leader · :5000)</div>
        <Row k="psql URI" v={writeURI} />
        <Row k="psql command" v={writePsql} />
      </div>
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Read (replicas · :5001)</div>
        <Row k="psql URI" v={readURI} />
        <Row k="psql command" v={readPsql} />
      </div>
      {stats && (
        <div className="space-y-2">
          <div className="text-xs font-medium text-muted">Stats</div>
          <Row k="HAProxy stats page" v={stats} link />
        </div>
      )}
      {!cfg.writePort && !cfg.readPort && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          No host ports published — enable export before deploying to reach HAProxy from your machine. In-stack
          clients can still reach it at <span className="font-mono">{cfg.fqdn}</span>:5000/5001.
        </div>
      )}
    </div>
  )
}

// PXCAccess documents connecting to a Percona XtraDB Cluster behind HAProxy.
function PXCAccess({ cfg, host, stats }) {
  const writeCmd = cfg.writePort ? `mysql -h ${host} -P ${cfg.writePort} -u<user> -p` : ''
  const readCmd = cfg.readPort ? `mysql -h ${host} -P ${cfg.readPort} -u<user> -p` : ''
  const writeIn = `mysql -h ${cfg.fqdn} -P 5000 -u<user> -p`
  const readIn = `mysql -h ${cfg.fqdn} -P 5001 -u<user> -p`
  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        HAProxy checks each PXC node's <span className="font-mono">clustercheck</span> endpoint (:9200) and routes
        only to wsrep-<span className="font-medium text-fg/80">Synced</span> nodes. The <span className="font-medium text-fg/80">write</span> port
        (:5000) sends all traffic to a single active node — the rest are hot backups, promoted on failure — to avoid
        multi-master write conflicts. The <span className="font-medium text-fg/80">read</span> port (:5001) round-robins
        every Synced node. Use any MySQL user (e.g. the app user from <span className="font-mono">.env</span>).
      </div>
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Write (single writer · :5000)</div>
        {writeCmd ? <Row k="From the host" v={writeCmd} /> : null}
        <Row k="In-stack (from another container)" v={writeIn} />
      </div>
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Read (round-robin · :5001)</div>
        {readCmd ? <Row k="From the host" v={readCmd} /> : null}
        <Row k="In-stack (from another container)" v={readIn} />
      </div>
      {stats && (
        <div className="space-y-2">
          <div className="text-xs font-medium text-muted">Stats</div>
          <Row k="HAProxy stats page" v={stats} link />
        </div>
      )}
      {!cfg.writePort && !cfg.readPort && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          No host ports published — enable export before deploying to reach HAProxy from your machine. In-stack
          clients can still reach it at <span className="font-mono">{cfg.fqdn}</span>:5000/5001.
        </div>
      )}
    </div>
  )
}
