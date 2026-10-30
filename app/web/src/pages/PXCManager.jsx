import { useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE } from '../lib/stackApi.js'
import { PTStalkCard } from '../components/Diagnostics.jsx'
import { useTerminals } from '../terminal/TerminalProvider.jsx'
import { SecretValue } from '../components/Secret.jsx'
import { Help } from '../components/Tooltip.jsx'
import { DEP_HELP } from '../lib/help.js'
import { useFollowedState } from '../session/SessionProvider.jsx'
import XtraBackupPanel from '../components/XtraBackupPanel.jsx'
import DbLoginGuide from '../components/DbLoginGuide.jsx'
import OidcLoginGuide from '../components/OidcLoginGuide.jsx'
import MySQLCertTab from '../components/MySQLCertTab.jsx'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'creds', label: 'Credentials' },
  { id: 'cert', label: 'Certificate' },
  { id: 'dirlogin', label: 'Directory Login' },
  { id: 'sso', label: 'Keycloak SSO' },
  { id: 'backup', label: 'Backup' },
  { id: 'diag', label: 'Diagnostics' },
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

function Err({ children }) {
  if (!children) return null
  return <div className="mb-2 rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">{children}</div>
}

function KV({ k, v, mono, help }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="flex shrink-0 items-center gap-1 text-muted">{k}<Help text={help} /></span>
      <span className={`truncate text-fg ${mono ? 'font-mono text-xs' : ''}`}>{(v ?? '') === '' ? '—' : String(v)}</span>
    </div>
  )
}

// xb, when set, is the cluster frame whose XtraBackup backups the Backup tab shows.
export default function PXCManager({ stackId, nodeId, dep, onDeleteNode, xb = null }) {
  const [tab, setTab] = useFollowedState(`tab:${nodeId}`, 'overview')
  const { openTerminal } = useTerminals()
  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const arbiter = cfg.role === 'arbitrator'

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PXC · {cfg.hostname}</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>

      <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
        {TABS.filter((t) => !(arbiter && t.id !== 'overview') && (t.id !== 'backup' || xb) && (t.id !== 'dirlogin' || cfg.dirAuth?.enabled) && (t.id !== 'sso' || cfg.oidc?.enabled)).map((t) => (
          <button key={t.id} onClick={() => setTab(t.id)}
            className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${tab === t.id ? 'bg-surface text-fg shadow' : 'text-muted'}`}>
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'overview' && (
        <Overview cfg={cfg} dep={dep} arbiter={arbiter}
          onDeleteNode={onDeleteNode}
          onOpenTerminal={() => openTerminal({ stackId, nodeId, title: `${cfg.hostname} · root` })} />
      )}
      {tab === 'creds' && !arbiter && <CredsTab cfg={cfg} sec={sec} />}
      {tab === 'cert' && !arbiter && <MySQLCertTab stackId={stackId} nodeId={nodeId} ttlValue={cfg.certTtlValue} ttlUnit={cfg.certTtlUnit} />}
      {tab === 'dirlogin' && !arbiter && <DbLoginGuide engine="ps" info={cfg.dirAuth} />}
      {tab === 'sso' && !arbiter && <OidcLoginGuide engine="ps" info={cfg.oidc} secrets={sec} />}
      {tab === 'backup' && !arbiter && xb && <XtraBackupPanel stackId={stackId} scope={xb.scope} id={xb.id} kind="pxc" />}
      {tab === 'diag' && !arbiter && <PTStalkCard stackId={stackId} nodeId={nodeId} />}
    </div>
  )
}

function Overview({ cfg, dep, arbiter, onDeleteNode, onOpenTerminal }) {
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const exportUrl = cfg.exportPort ? `${host}:${cfg.exportPort}` : null
  return (
    <div className="space-y-2 text-sm">
      <KV k="Cluster" help={DEP_HELP.Cluster} v={cfg.cluster} />
      <KV k="Role" help={DEP_HELP.Role} v={arbiter ? 'arbitrator (garbd)' : 'regular (data)'} />
      <KV k="FQDN" help={DEP_HELP.FQDN} v={cfg.fqdn} mono />
      <KV k="server-id" help={DEP_HELP['server-id']} v={cfg.serverId} />
      {cfg.serverVersion && <KV k="Version" help={DEP_HELP.Version} v={cfg.serverVersion} mono />}
      <KV k="Image" help={DEP_HELP.Image} v={cfg.image} mono />
      <KV k="GTID" help={DEP_HELP.GTID} v={cfg.gtid ? 'on' : 'off'} />
      <KV k="TLS" help={DEP_HELP.TLS} v={cfg.generateCert ? 'Intranet CA' : 'none'} />
      <KV k="Monitored by" help={DEP_HELP['Monitored by']} v={cfg.monitoredBy} mono />
      <KV k="Ports" help={DEP_HELP.Ports} v={(cfg.ports || []).join(', ')} mono />
      {!arbiter && exportUrl && (
        <div>
          <div className="text-xs text-muted">Host access (3306)</div>
          <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
            <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{exportUrl}</span>
            <CopyButton text={exportUrl} />
          </div>
        </div>
      )}
      <KV k="Container" help={DEP_HELP.Container} v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
      <Button variant="outline" size="sm" className="mt-2 w-full" onClick={onOpenTerminal}>
        <Icon.Nodes size={16} /> Open root console
      </Button>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

function CredsTab({ cfg, sec }) {
  const rows = [
    { k: 'Root user', v: sec.rootUser || 'root' },
    { k: 'Root password', v: sec.rootPassword, secret: true },
    { k: 'App user', v: sec.appUser || 'app' },
    { k: 'App password', v: sec.appPassword, secret: true },
    { k: 'Repl user', v: sec.replUser || 'repl' },
    { k: 'Repl password', v: sec.replPassword, secret: true },
    { k: 'Monitor user', v: sec.monitorUser || 'monitor' },
    { k: 'Monitor password', v: sec.monitorPassword, secret: true },
  ]
  return (
    <div className="space-y-2">
      <div className="text-[11px] text-muted">App/repl/monitor passwords come from the host .env (APP_PASSWORD / REPL_PASSWORD / MONITOR_PASSWORD).</div>
      {rows.map((r) => (
        <div key={r.k}>
          <div className="text-xs text-muted">{r.k}</div>
          {r.secret
            ? <SecretValue value={r.v} />
            : (
              <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
                <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{r.v || '—'}</span>
                {r.v && <CopyButton text={r.v} />}
              </div>
            )}
        </div>
      ))}
    </div>
  )
}

