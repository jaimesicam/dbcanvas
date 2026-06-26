import { useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE } from '../lib/stackApi.js'
import { useTerminals } from '../terminal/TerminalProvider.jsx'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'access', label: 'Access' },
  { id: 'creds', label: 'Credentials' },
]

function CopyButton({ text, size = 14 }) {
  const [done, setDone] = useState(false)
  return (
    <button title="Copy"
      onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}
      className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
      {done ? <Icon.Check size={size} /> : <Icon.Copy size={size} />}
    </button>
  )
}

function KV({ k, v, mono }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="text-muted">{k}</span>
      <span className={`truncate text-fg ${mono ? 'font-mono text-xs' : ''}`}>{(v ?? '') === '' ? '—' : String(v)}</span>
    </div>
  )
}

function CopyRow({ label, value }) {
  return (
    <div>
      <div className="text-xs text-muted">{label}</div>
      <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
        <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{value || '—'}</span>
        {value && <CopyButton text={value} />}
      </div>
    </div>
  )
}

// roleText renders a member's place in the sharded topology.
function roleText(cfg) {
  if (cfg.role === 'mongos') return 'mongos router'
  if (cfg.role === 'config') return `config server (${cfg.replSet})`
  return `shard ${cfg.shard} member (${cfg.replSet})`
}

export default function MongoDBManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const { openTerminal } = useTerminals()
  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const isMongos = cfg.role === 'mongos'

  // Connect string apps use through the mongos router. When the mongos port is
  // exported, apps can reach it from the host; otherwise it's in-cluster only.
  const adminUser = sec.adminUser || 'admin'
  const hostConn = isMongos && cfg.mongosPort
    ? `mongosh "mongodb://${adminUser}@${host}:${cfg.mongosPort}/?authSource=admin"`
    : ''
  const inClusterConn = `mongosh "mongodb://${adminUser}@${cfg.fqdn}:27017/?authSource=admin"`

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PS MongoDB · {cfg.hostname}</span>
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

      {tab === 'overview' && (
        <div className="space-y-2 text-sm">
          <KV k="Cluster" v={cfg.cluster} />
          <KV k="Role" v={roleText(cfg)} />
          <KV k="FQDN" v={cfg.fqdn} mono />
          <KV k="PS MongoDB" v={`${cfg.psmdbMajor || ''}${cfg.version ? ` (${cfg.version})` : ''}`} />
          {isMongos && <KV k="configDB" v={cfg.configDB} mono />}
          {isMongos && <KV k="Exported port" v={cfg.mongosPort || 'not published'} />}
          <KV k="TLS" v={cfg.generateCert ? 'Intranet CA' : 'none'} />
          <KV k="Monitored by" v={cfg.monitoredBy} mono />
          <KV k="Image" v={cfg.image} mono />
          <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
          <Button variant="outline" size="sm" className="mt-2 w-full" onClick={() => openTerminal({ stackId, nodeId, title: `${cfg.hostname} · root` })}>
            <Icon.Nodes size={16} /> Open root console
          </Button>
          <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
            <Icon.Trash size={16} /> Delete node
          </Button>
        </div>
      )}

      {tab === 'access' && (
        <div className="space-y-2">
          {isMongos ? (
            <>
              <div className="text-[11px] text-muted">Apps connect to the sharded cluster through this mongos router.</div>
              {cfg.mongosPort ? (
                <CopyRow label={`From the host (mongos ${cfg.mongosPort})`} value={hostConn} />
              ) : (
                <div className="text-xs text-muted">mongos port not published to the host (enable export on this node to expose 27017).</div>
              )}
              <CopyRow label="In-cluster (from another container)" value={inClusterConn} />
            </>
          ) : (
            <div className="text-xs text-muted">
              {cfg.role === 'config' ? 'Config servers' : 'Shard members'} are internal to the cluster. Connect applications through the mongos router instead.
              <div className="mt-2"><CopyRow label="Direct (admin, debugging)" value={inClusterConn} /></div>
            </div>
          )}
        </div>
      )}

      {tab === 'creds' && (
        <div className="space-y-2">
          <div className="text-[11px] text-muted">Cluster admin (root) credentials. The internal-auth keyFile is not surfaced.</div>
          {[
            { k: 'Admin user', v: sec.adminUser || 'admin' },
            { k: 'Admin password', v: sec.adminPassword },
          ].map((r) => (
            <div key={r.k}>
              <div className="text-xs text-muted">{r.k}</div>
              <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
                <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{r.v || '—'}</span>
                {r.v && <CopyButton text={r.v} />}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
