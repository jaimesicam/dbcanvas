import { useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE, pgApi } from '../lib/stackApi.js'
import { PGGatherCard } from '../components/Diagnostics.jsx'
import DbLoginGuide from '../components/DbLoginGuide.jsx'
import OidcLoginGuide from '../components/OidcLoginGuide.jsx'
import PGCertTab from '../components/PGCertTab.jsx'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'creds', label: 'Credentials' },
  { id: 'dirlogin', label: 'Directory Login' },
  { id: 'sso', label: 'Keycloak SSO' },
  { id: 'cert', label: 'Certificate' },
  { id: 'backup', label: 'Backup' },
  { id: 'diag', label: 'Diagnostics' },
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

// PGManager is the properties-panel console for a deployed standalone PostgreSQL
// node: Overview, Credentials (superuser + psql URI when the port is published),
// and a Backup tab (only when pgBackRest is enabled).
export default function PGManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const hasBackup = !!cfg.usePgBackRest

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PostgreSQL · standalone</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>

      <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
        {TABS.filter((t) => (t.id !== 'backup' || hasBackup) && (t.id !== 'dirlogin' || cfg.dirAuth?.enabled) && (t.id !== 'sso' || cfg.oidc?.enabled)).map((t) => (
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
      {tab === 'creds' && <Creds cfg={cfg} sec={sec} />}
      {tab === 'dirlogin' && <DbLoginGuide engine="pg" info={cfg.dirAuth} />}
      {tab === 'sso' && <OidcLoginGuide engine="pg" info={cfg.oidc} />}
      {tab === 'cert' && <PGCertTab stackId={stackId} nodeId={nodeId} />}
      {tab === 'backup' && hasBackup && <BackupTab stackId={stackId} nodeId={nodeId} cfg={cfg} />}
      {tab === 'diag' && <PGGatherCard stackId={stackId} nodeId={nodeId} defaultDb={cfg.database} />}
    </div>
  )
}

function Overview({ cfg, dep, onDeleteNode }) {
  return (
    <div className="space-y-2 text-sm">
      <KV k="FQDN" v={cfg.fqdn} mono />
      <KV k="PostgreSQL" v={cfg.pgVersion || cfg.pgMajor} mono />
      <KV k="Image" v={cfg.image} mono />
      <KV k="Role" v="standalone (read/write)" />
      <KV k="pgBackRest" v={cfg.usePgBackRest ? (cfg.backupRepo || 'enabled') : 'disabled'} />
      <KV k="TLS" v={cfg.generateCert ? 'Intranet-CA cert' : 'off'} />
      <KV k="Monitored by" v={cfg.monitoredBy || 'none'} mono />
      <KV k="Host port (5432)" v={cfg.exportPort ? String(cfg.exportPort) : 'not published'} mono />
      <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

function Creds({ cfg, sec }) {
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const conn = cfg.exportPort ? `postgresql://${sec.superUser || 'postgres'}:${sec.superPassword || ''}@${host}:${cfg.exportPort}/postgres` : ''
  const incluster = `postgresql://${sec.superUser || 'postgres'}:${sec.superPassword || ''}@${cfg.fqdn || cfg.hostname}:5432/postgres`
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
        <div className="text-xs font-medium text-muted">Connection (in-stack network)</div>
        <Row k="psql URI" v={incluster} />
      </div>
    </div>
  )
}

function BackupTab({ stackId, nodeId, cfg }) {
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState(null)
  async function runBackup() {
    setBusy(true)
    setMsg(null)
    try {
      await pgApi(stackId, nodeId).backup()
      setMsg({ tone: 'success', text: 'pgBackRest full backup completed.' })
    } catch (e) {
      setMsg({ tone: 'danger', text: e.message || 'Backup failed.' })
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="space-y-3 text-sm">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] text-muted">
        pgBackRest archives WAL + full backups to the SeaweedFS S3 bucket
        (<span className="font-mono">{cfg.backupRepo || 'SeaweedFS'}</span>). The initial full backup runs at
        deploy; use the button below to take another on demand.
      </div>
      <Button size="sm" className="w-full" disabled={busy} onClick={runBackup}>
        <Icon.Arrow size={15} /> {busy ? 'Backing up…' : 'Backup now (full)'}
      </Button>
      {msg && (
        <div className={`rounded-lg border px-2.5 py-1.5 text-xs ${msg.tone === 'danger' ? 'border-danger/30 bg-danger/15 text-danger' : 'border-success/30 bg-success/15 text-success'}`}>
          {msg.text}
        </div>
      )}
    </div>
  )
}
