import { useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE, pgStreamApi } from '../lib/stackApi.js'
import BackupGuide from '../components/BackupGuide.jsx'
import { PGGatherCard } from '../components/Diagnostics.jsx'
import PGCertTab from '../components/PGCertTab.jsx'
import { SecretValue } from '../components/Secret.jsx'
import { Help } from '../components/Tooltip.jsx'
import { DEP_HELP } from '../lib/help.js'
import { useFollowedState } from '../session/SessionProvider.jsx'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'creds', label: 'Credentials' },
  { id: 'cert', label: 'Certificate' },
  { id: 'replication', label: 'Replication' },
  { id: 'backup', label: 'Backup' },
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

function KV({ k, v, mono, help }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="flex shrink-0 items-center gap-1 text-muted">{k}<Help text={help} /></span>
      <span className={`truncate text-fg ${mono ? 'font-mono text-xs' : ''}`}>{v || '—'}</span>
    </div>
  )
}

function Row({ k, v, secret }) {
  if (!v) return null
  return (
    <div>
      <div className="text-xs text-muted">{k}</div>
      {secret ? <SecretValue value={v} /> : (
        <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
          <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{v}</span>
          <CopyButton text={v} />
        </div>
      )}
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

// syncLabel describes the synchronous replication the cluster was deployed with.
function syncLabel(cfg) {
  if (!cfg.syncStandbys) return 'asynchronous'
  return `ANY ${cfg.syncStandbys} · synchronous_commit = ${cfg.syncCommit || 'on'}`
}

// PGStreamManager is the properties-panel console for a deployed member of a streaming
// replication cluster: PostgreSQL's own physical replication, with no failover manager.
export default function PGStreamManager({ stackId, nodeId, frame, dep, onDeleteNode }) {
  const [tab, setTab] = useFollowedState(`tab:${nodeId}`, 'overview')
  const cfg = dep.config || {}
  const sec = dep.secrets || {}

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Streaming · {cfg.hostname}</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>

      <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
        {TABS.filter((t) => t.id !== 'backup' || cfg.backupEngine).map((t) => (
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
      {tab === 'backup' && cfg.backupEngine && <BackupTab stackId={stackId} frameId={frame?.id} cfg={cfg} />}
      {tab === 'diag' && <PGGatherCard stackId={stackId} nodeId={nodeId} defaultDb="postgres" />}
    </div>
  )
}

function Overview({ cfg, dep, onDeleteNode }) {
  return (
    <div className="space-y-2 text-sm">
      <KV k="Cluster" help={DEP_HELP.Cluster} v={cfg.cluster} mono />
      <KV k="Role (initial)" help={DEP_HELP['Role (initial)']} v={cfg.role === 'primary' ? 'primary' : 'hot standby'} />
      {cfg.role !== 'primary' && <KV k="Streams from" help={DEP_HELP['Streams from']} v={cfg.sourceHost} mono />}
      {cfg.role !== 'primary' && <KV k="Replication slot" help={DEP_HELP['Replication slot']} v={cfg.slotName || 'none (wal_keep_size only)'} mono />}
      <KV k="application_name" help={DEP_HELP.application_name} v={cfg.hostname} mono />
      <KV k="Synchronous commit" help={DEP_HELP['Synchronous commit']} v={syncLabel(cfg)} />
      <KV k="Backups" help={DEP_HELP.Backups} v={cfg.backupEngine ? (cfg.backupRepo || 'pgBackRest') : 'none'} />
      <KV k="FQDN" help={DEP_HELP.FQDN} v={cfg.fqdn} mono />
      <KV k="PostgreSQL" help={DEP_HELP.PostgreSQL} v={`Percona ${cfg.pgVersion || cfg.pgMajor || ''}`.trim()} mono />
      <KV k="Data directory" help={DEP_HELP['Data directory']} v={cfg.dataDir} mono />
      <KV k="Service" help={DEP_HELP.Service} v={cfg.service} mono />
      {cfg.pgVector && <KV k="pgvector" help={DEP_HELP.pgvector} v={cfg.pgVector} mono />}
      <KV k="TLS" help={DEP_HELP.TLS} v={cfg.generateCert ? 'Intranet-CA cert' : 'off'} />
      <KV k="Host port (5432)" help={DEP_HELP['Host port (5432)']} v={cfg.exportPort ? String(cfg.exportPort) : 'not published'} mono />
      <KV k="Monitored by" help={DEP_HELP['Monitored by']} v={cfg.monitoredBy} mono />
      <KV k="Container" help={DEP_HELP.Container} v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
      <p className="text-[11px] leading-snug text-muted">
        The role is the one this member was deployed with. Its live role is on the canvas; a switchover from the
        right-click menu moves the primary and re-points every standby.
      </p>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

function Creds({ cfg, sec }) {
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const su = sec.superUser || 'postgres'
  const conn = cfg.exportPort ? `postgresql://${su}:${sec.superPassword || ''}@${host}:${cfg.exportPort}/postgres` : ''
  const inCluster = `psql "host=${cfg.fqdn} port=5432 dbname=postgres user=${su}"`
  // Every member, primary first: libpq tries them in order and keeps the one that takes writes.
  const all = [cfg.fqdn, ...(cfg.peers || []).map((p) => p.fqdn)].filter(Boolean)
  const primaryFirst = `psql "host=${all.join(',')} port=5432 dbname=postgres user=${su} target_session_attrs=read-write"`
  return (
    <div className="space-y-3">
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Superuser</div>
        <Row k="Username" v={su} />
        <Row k="Password" v={sec.superPassword} secret />
      </div>
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Replication role</div>
        <Row k="Username" v={sec.replUser || 'replicator'} />
        <Row k="Password" v={sec.replPassword} secret />
      </div>
      {conn && (
        <div className="space-y-2">
          <div className="text-xs font-medium text-muted">Connection (published host port)</div>
          <Row k="psql URI" v={conn} secret />
        </div>
      )}
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">In-cluster (from another container)</div>
        <Row k="This member" v={inCluster} />
        {all.length > 1 && <Row k="Whichever member is primary" v={primaryFirst} />}
      </div>
    </div>
  )
}

function Replication({ cfg, sec }) {
  const bin = cfg.binDir || '/usr/pgsql-17/bin'
  const repl = sec.replUser || 'replicator'
  const onPrimary =
`-- On the primary: every standby, its state, and how far behind it is.
SELECT application_name, client_addr, state, sync_state,
       sent_lsn, write_lsn, flush_lsn, replay_lsn,
       write_lag, flush_lag, replay_lag
  FROM pg_stat_replication;

-- The slots, and how much WAL each one is holding back.
SELECT slot_name, active, restart_lsn,
       pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) AS retained
  FROM pg_replication_slots;

SHOW synchronous_standby_names;
SHOW synchronous_commit;`
  const onStandby =
`-- On a standby: the WAL receiver and how far replay has got.
SELECT status, sender_host, slot_name, received_lsn, latest_end_lsn, last_msg_receipt_time
  FROM pg_stat_wal_receiver;
SELECT pg_is_in_recovery(),
       pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn(),
       now() - pg_last_xact_replay_timestamp() AS replay_age;`
  const sync =
`-- Make commits wait for one standby (any of them), applied with a reload:
ALTER SYSTEM SET synchronous_standby_names = 'ANY 1 (${[...(cfg.peers || []).filter((p) => p.role !== 'primary').map((p) => `"${p.hostname}"`), cfg.role !== 'primary' ? `"${cfg.hostname}"` : null].filter(Boolean).join(', ') || '"standby1", "standby2"'})';
ALTER SYSTEM SET synchronous_commit = 'on';   -- or remote_write, remote_apply
SELECT pg_reload_conf();

-- Back to asynchronous:
ALTER SYSTEM RESET synchronous_standby_names;
SELECT pg_reload_conf();`
  const promote =
`-- Promote this standby by hand (there is no failover manager here).
-- Do it only once the old primary is stopped, or there are two primaries.
SELECT pg_promote();

-- An old primary that kept running after a promotion has diverged. To make it a standby
-- of the new primary, stop it and rewind it (wal_log_hints is on):
#   systemctl stop ${cfg.service || 'postgresql-17'}
#   runuser -u postgres -- ${bin}/pg_rewind -D ${cfg.dataDir || '/var/lib/pgsql/17/data'} \\
#       --source-server="host=<new primary> user=postgres dbname=postgres"
#   touch ${cfg.dataDir || '/var/lib/pgsql/17/data'}/standby.signal   # then set primary_conninfo, and start`
  const basebackup =
`# A new standby by hand: an empty data directory, then a copy of the primary.
systemctl stop ${cfg.service || 'postgresql-17'}
rm -rf ${cfg.dataDir || '/var/lib/pgsql/17/data'}/*
runuser -u postgres -- ${bin}/pg_basebackup -h ${cfg.sourceHost || '<primary>'} -U ${repl} \\
    -D ${cfg.dataDir || '/var/lib/pgsql/17/data'} -X stream --checkpoint=fast -R${cfg.slotName ? ` --slot=${cfg.slotName}` : ''}
systemctl start ${cfg.service || 'postgresql-17'}`
  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        Physical <span className="font-medium text-fg/80">streaming replication</span>: the primary ships its WAL to every
        standby as it is written, and each standby replays it and answers read-only queries. Nothing watches the
        primary — if it stops, the standbys keep what they have and wait. Promotion is a switchover from a standby's
        right-click menu, or <span className="font-mono">pg_promote()</span> by hand.
      </div>
      <CodeBlock label="Who is streaming (run on the primary)" text={onPrimary} />
      <CodeBlock label="Receiver and replay (run on a standby)" text={onStandby} />
      <CodeBlock label="Synchronous replication" text={sync} />
      <CodeBlock label="Promote by hand" text={promote} />
      <CodeBlock label="Rebuild a standby by hand" text={basebackup} />
    </div>
  )
}

function BackupTab({ stackId, frameId, cfg }) {
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState(null)
  async function runBackup() {
    setBusy(true)
    setMsg(null)
    try {
      await pgStreamApi(stackId, frameId).backup()
      setMsg({ tone: 'success', text: 'pgBackRest full backup completed.' })
    } catch (e) {
      setMsg({ tone: 'danger', text: e.message || 'Backup failed.' })
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="space-y-3 text-sm">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        pgBackRest (<span className="font-mono">archive-push</span> / <span className="font-mono">backup</span>) ships WAL and
        base backups to <span className="font-mono">{cfg.backupRepo || 'the SeaweedFS S3 bucket'}</span>. Every member carries
        the same <span className="font-mono">archive_command</span>, and only the primary archives, so a switchover moves the
        archiving with it. The first full backup ran at deploy; the button takes another on whichever member is primary now.
      </div>
      <div className="space-y-1 text-xs">
        <KV k="Bucket" v={cfg.backupBucket} />
        <KV k="Stanza" v={cfg.backupStanza} mono />
      </div>
      <Button size="sm" className="w-full" disabled={busy || !frameId} onClick={runBackup}>
        <Icon.Arrow size={15} /> {busy ? 'Backing up…' : 'Backup now'}
      </Button>
      {msg && (
        <div className={`rounded-lg border px-2.5 py-1.5 text-xs ${msg.tone === 'danger' ? 'border-danger/30 bg-danger/15 text-danger' : 'border-success/30 bg-success/15 text-success'}`}>
          {msg.text}
        </div>
      )}
      <BackupGuide engine="pgbackrest" cfg={cfg} nodeLabel={cfg.cluster} />
    </div>
  )
}
