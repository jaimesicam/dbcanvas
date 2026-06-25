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

export default function InnoDBManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const { openTerminal } = useTerminals()
  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">InnoDB/GR · {cfg.hostname}</span>
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
          <KV k="Mode" v={cfg.replMode === 'groupreplication' ? 'Group Replication' : 'InnoDB Cluster'} />
          <KV k="PDPS repo" v={cfg.pdpsRepo} />
          <KV k="FQDN" v={cfg.fqdn} mono />
          <KV k="server-id" v={cfg.serverId} />
          <KV k="Group name" v={cfg.groupName} mono />
          <KV k="Bootstrap node" v={cfg.bootstrap ? 'yes' : 'no'} />
          <KV k="MySQL Router" v={cfg.router ? 'on each node' : 'off'} />
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
          <div className="text-[11px] text-muted">Apps connect through this node's MySQL Router (read/write to the primary, reads to secondaries).</div>
          {cfg.router && cfg.rwPort ? (
            <CopyRow label={`Read/write (router 6446) — as ${sec.appUser || 'app'}`} value={`mysql -h ${host} -P ${cfg.rwPort} -u ${sec.appUser || 'app'} -p`} />
          ) : (
            <div className="text-xs text-muted">Router RW port not published to the host (enable export to expose 6446).</div>
          )}
          {cfg.router && cfg.roPort ? (
            <CopyRow label="Read-only (router 6447)" value={`mysql -h ${host} -P ${cfg.roPort} -u ${sec.appUser || 'app'} -p`} />
          ) : null}
        </div>
      )}

      {tab === 'creds' && (
        <div className="space-y-2">
          <div className="text-[11px] text-muted">App/monitor/cluster passwords come from the host .env.</div>
          {[
            { k: 'Root user', v: sec.rootUser || 'root' },
            { k: 'Root password', v: sec.rootPassword },
            { k: 'App user', v: sec.appUser || 'app' },
            { k: 'App password', v: sec.appPassword },
            { k: 'Monitor user', v: sec.monitorUser || 'monitor' },
            { k: 'Monitor password', v: sec.monitorPassword },
            { k: 'Cluster user', v: sec.clusterUser || 'cluster' },
            { k: 'Cluster password', v: sec.clusterPassword },
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
