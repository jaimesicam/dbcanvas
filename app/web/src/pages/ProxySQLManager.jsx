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
    <button
      title="Copy"
      onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}
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

export default function ProxySQLManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const { openTerminal } = useTerminals()
  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">ProxySQL · {cfg.hostname}</span>
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
          {cfg.proxysqlCluster && <KV k="ProxySQL cluster" v={cfg.proxysqlCluster} />}
          <KV k="PXC cluster" v={cfg.cluster} />
          <KV k="Backend (CLUSTER_HOSTNAME)" v={cfg.clusterHost} mono />
          <KV k="Mode" v={cfg.backendKind === 'mysql'
            ? (cfg.mode === 'primary' ? 'primary only' : 'read/write split')
            : (cfg.mode === 'loadbal' ? 'load balancer' : 'single writer')} />
          <KV k="FQDN" v={cfg.fqdn} mono />
          {cfg.serverVersion && <KV k="Version" v={cfg.serverVersion} mono />}
          <KV k="Image" v={cfg.image} mono />
          <KV k="ProxySQL" v={`proxysql${cfg.major}${cfg.proxysqlVersion ? ` · ${cfg.proxysqlVersion}` : ''}`} />
          <KV k="Monitored by" v={cfg.monitoredBy} mono />
          <KV k="Ports" v={(cfg.ports || []).join(', ')} mono />
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
          <div className="text-[11px] text-muted">Applications connect to the MySQL traffic port; the admin interface manages ProxySQL.</div>
          {cfg.mysqlPort ? (
            <CopyRow label={`MySQL traffic (6033) — connect as ${sec.appUser || 'app'}`} value={`mysql -h ${host} -P ${cfg.mysqlPort} -u ${sec.appUser || 'app'} -p`} />
          ) : (
            <div className="text-xs text-muted">MySQL port not published to the host (enable export to expose 6033).</div>
          )}
          {cfg.adminPort ? (
            <CopyRow label={`Admin interface (6032) — ${sec.adminUser || 'admin'}`} value={`mysql -h ${host} -P ${cfg.adminPort} -u ${sec.adminUser || 'admin'} -p`} />
          ) : null}
        </div>
      )}

      {tab === 'creds' && (
        <div className="space-y-2">
          <div className="text-[11px] text-muted">App/monitor/cluster credentials come from the linked PXC cluster (passwords from .env).</div>
          <CopyRow label="Admin user (6032)" value={sec.adminUser || 'admin'} />
          <CopyRow label="Admin password" value={sec.adminPassword} />
          <CopyRow label="App user (6033)" value={sec.appUser || 'app'} />
          <CopyRow label="App password" value={sec.appPassword} />
          <CopyRow label="Monitor user" value={sec.monitorUser || 'monitor'} />
          <CopyRow label="Monitor password" value={sec.monitorPassword} />
          <CopyRow label="Cluster user (CLUSTER_USERNAME)" value={sec.clusterUser || 'cluster'} />
          <CopyRow label="Cluster password" value={sec.clusterPassword} />
        </div>
      )}
    </div>
  )
}
