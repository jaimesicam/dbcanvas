import { useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE } from '../lib/stackApi.js'
import { useTerminals } from '../terminal/TerminalProvider.jsx'

const TABS = [
  { id: 'overview', label: 'Overview' },
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

export default function MySQLManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const { openTerminal } = useTerminals()
  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const isPrimary = cfg.role === 'primary'

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Percona Server · {cfg.hostname}</span>
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
          {cfg.cluster && <KV k="Cluster" v={cfg.cluster} />}
          <KV k="Role" v={cfg.role === 'standalone' ? 'standalone (read/write)' : isPrimary ? 'primary (read/write)' : 'secondary (read-only)'} />
          {cfg.replMode && <KV k="Replication" v={cfg.replMode === 'semisync' ? 'semi-synchronous' : 'asynchronous'} />}
          {cfg.role === 'secondary' && <KV k="Source (primary)" v={cfg.sourceHost} mono />}
          <KV k="FQDN" v={cfg.fqdn} mono />
          <KV k="server-id" v={cfg.serverId} />
          <KV k="GTID" v={cfg.gtid ? 'on' : 'off'} />
          <KV k="read_only" v={cfg.readOnly ? 'ON' : 'OFF'} />
          <KV k="TLS" v={cfg.generateCert ? 'Intranet CA' : 'none'} />
          <KV k="Monitored by" v={cfg.monitoredBy} mono />
          <KV k="Image" v={cfg.image} mono />
          {cfg.exportPort ? (
            <div>
              <div className="text-xs text-muted">Host access (3306)</div>
              <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
                <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{host}:{cfg.exportPort}</span>
                <CopyButton text={`${host}:${cfg.exportPort}`} />
              </div>
            </div>
          ) : null}
          <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
          <Button variant="outline" size="sm" className="mt-2 w-full" onClick={() => openTerminal({ stackId, nodeId, title: `${cfg.hostname} · root` })}>
            <Icon.Nodes size={16} /> Open root console
          </Button>
          <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
            <Icon.Trash size={16} /> Delete node
          </Button>
        </div>
      )}

      {tab === 'creds' && (
        <div className="space-y-2">
          <div className="text-[11px] text-muted">App/repl/monitor/cluster passwords come from the host .env.</div>
          {[
            { k: 'Root user', v: sec.rootUser || 'root' },
            { k: 'Root password', v: sec.rootPassword },
            { k: 'App user', v: sec.appUser || 'app' },
            { k: 'App password', v: sec.appPassword },
            { k: 'Repl user', v: sec.replUser || 'repl' },
            { k: 'Repl password', v: sec.replPassword },
            { k: 'Monitor user', v: sec.monitorUser || 'monitor' },
            { k: 'Monitor password', v: sec.monitorPassword },
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
