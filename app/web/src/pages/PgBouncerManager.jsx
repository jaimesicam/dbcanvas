import { useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE } from '../lib/stackApi.js'
import { Help } from '../components/Tooltip.jsx'
import { DEP_HELP } from '../lib/help.js'

// PgBouncerManager is the panel for a running PgBouncer node. Three tabs, because a
// pooler raises three different questions and they have different answers:
//
//   Overview — what is this pool, and what is behind it
//   Access   — the connection strings. Every pool is the same host and port; what
//              selects one is the database name, which is the thing people get wrong.
//   Pooling  — what the settings actually mean for this deployment, and the admin
//              console commands that show the pool doing its job.
//
// Everything here is read off the deployment config the provisioner wrote, so the
// panel cannot describe a pool that was not deployed.

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'access', label: 'Access' },
  { id: 'pooling', label: 'Pooling' },
]

const BACKEND_LABEL = {
  pg: 'PostgreSQL (standalone)',
  patroni: 'Patroni cluster',
  repmgr: 'repmgr cluster',
  spock: 'Spock cluster (multi-master)',
}

const POOL_MODE_NOTE = {
  transaction: 'A server connection is held only for the length of a transaction. Session state does not survive between transactions — no session-level SET, no LISTEN/NOTIFY, no plain server-side prepared statements.',
  session: 'A server connection is held until the client disconnects, so session state is safe and the pool only saves the cost of connecting.',
  statement: 'A server connection is held for one statement. Multi-statement transactions are refused outright.',
}

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

function KV({ k, v, mono, help }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="flex shrink-0 items-center gap-1 text-muted">{k}<Help text={help} /></span>
      <span className={`truncate text-fg ${mono ? 'font-mono text-xs' : ''}`}>{v || '—'}</span>
    </div>
  )
}

function Row({ k, v, note }) {
  if (!v) return null
  return (
    <div>
      <div className="text-xs text-muted">{k}</div>
      <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
        <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{v}</span>
        <CopyButton text={v} />
      </div>
      {note && <div className="mt-0.5 text-[11px] leading-snug text-muted">{note}</div>}
    </div>
  )
}

export default function PgBouncerManager({ dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const cfg = dep.config || {}

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PgBouncer</span>
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
      {tab === 'pooling' && <PoolingTab cfg={cfg} />}
    </div>
  )
}

function Overview({ cfg, dep, onDeleteNode }) {
  const members = cfg.members || []
  return (
    <div className="space-y-2 text-sm">
      <KV k="FQDN" help={DEP_HELP.FQDN} v={cfg.fqdn} mono />
      {cfg.serverVersion && <KV k="Version" help={DEP_HELP.Version} v={cfg.serverVersion} mono />}
      <KV k="Image" help={DEP_HELP.Image} v={cfg.image} mono />
      <KV k="Repository" v={cfg.pgMajor ? `ppg-${cfg.pgMajor}` : '—'} mono />
      <KV k="Backend" help={DEP_HELP.Backend} v={BACKEND_LABEL[cfg.backend] || cfg.backend} />
      <KV k="Pools for" help={DEP_HELP['Routes to cluster']} v={cfg.cluster} mono />
      <KV k="Backend members" help={DEP_HELP['Backend members']} v={members.length ? `${members.length} member(s)` : '—'} />
      <KV k="Pool port (6432)" help={DEP_HELP['Exported port']} v={cfg.exportPort ? String(cfg.exportPort) : 'not published'} mono />
      <KV k="Pool mode" v={cfg.poolMode} mono />
      <KV k="Follows the writable member" v={cfg.followRole ? `yes — every ${cfg.watchSeconds}s` : (cfg.backend === 'pg' ? 'n/a — one server' : 'no')} />
      <KV k="Container" help={DEP_HELP.Container} v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
      {members.length > 0 && (
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          <div className="mb-1 font-medium text-fg/80">Members behind the pool</div>
          {members.map((m) => <div key={m} className="font-mono">{m}</div>)}
        </div>
      )}
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// AccessTab is the tab that has to correct the one wrong assumption people bring to a
// pooler: that the read/write split is a second port, the way it is on HAProxy. It is
// not — every pool is :6432 and the *database name* selects one.
function AccessTab({ cfg }) {
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const port = cfg.exportPort
  const inHost = cfg.fqdn
  const tls = cfg.generateCert
  const cert = cfg.authType === 'cert'
  const dir = cfg.clientCertDir || '/etc/pgbouncer/client'
  const role = cfg.clientCertUser || 'postgres'
  // Under cert auth there is no password anywhere in the connection string — the
  // certificate is the credential, and its CN is the user name.
  const certOpts = ` sslmode=verify-full sslrootcert=${dir}/ca.crt sslcert=${dir}/${role}.crt sslkey=${dir}/${role}.key`
  const sslmode = tls ? '?sslmode=verify-full' : ''
  const uri = (db) => (port ? `postgresql://postgres:<pw>@${host}:${port}/${db}${sslmode}` : '')
  const psqlIn = (db) => (cert
    ? `psql "host=${inHost} port=6432 dbname=${db} user=${role}${certOpts}"`
    : `psql "host=${inHost} port=6432 dbname=${db} user=postgres"`)

  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        Every pool on this node is the same host and the same port. What picks one is the
        <span className="font-medium text-fg/80"> database name</span> in the connection —
        PgBouncer matches it against its <span className="font-mono">[databases]</span> section, and the
        wildcard pool takes anything it does not recognise.
        {cfg.followRole && ' The wildcard pool follows the member that can currently take writes, so it survives a failover without a reconnect string change.'}
      </div>

      {cert && (
        <div className="rounded-lg border border-primary/30 bg-primary/10 px-2.5 py-2 text-[11px] leading-snug text-primary">
          This pool authenticates clients by <span className="font-medium">certificate</span>, so there is no password
          in any of the commands below. The user name comes from the certificate’s
          <span className="font-mono"> CN</span> — the one minted at deploy is for
          <span className="font-mono"> {role}</span>, in <span className="font-mono">{dir}</span> on this node.
          Copy it out with <span className="font-mono">dbcanvas node cp</span> to connect from anywhere else.
        </div>
      )}

      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Wildcard pool — any database, the writable member</div>
        {port && !cert ? <Row k="From the host" v={uri('postgres')} /> : null}
        <Row k="In-stack (from another container)" v={psqlIn('postgres')} />
        {port && cert && (
          <div className="text-[11px] leading-snug text-muted">
            Reachable from your machine on port <span className="font-mono">{port}</span> too, once you have copied
            the certificate, its key and <span className="font-mono">ca.crt</span> out of{' '}
            <span className="font-mono">{dir}</span>.
          </div>
        )}
      </div>

      {cfg.readAlias && (
        <div className="space-y-2">
          <div className="text-xs font-medium text-muted">Read-only pool — a standby</div>
          {port && !cert ? <Row k="From the host" v={uri(cfg.readAlias)} /> : null}
          <Row k="In-stack (from another container)" v={psqlIn(cfg.readAlias)}
            note="Writes here fail because PostgreSQL is in recovery, not because PgBouncer refuses them." />
        </div>
      )}

      {(cfg.memberAliases || []).length > 0 && (
        <div className="space-y-2">
          <div className="text-xs font-medium text-muted">Per-member pools — every Spock node is a writer</div>
          {(cfg.memberAliases || []).map((a) => <Row key={a} k={a} v={psqlIn(a)} />)}
        </div>
      )}

      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Admin console</div>
        {cert ? (
          <Row k="On the node (over TLS)" v={`psql "host=${inHost} port=6432 dbname=pgbouncer user=${role}${certOpts}"`}
            note="The unix socket does not work under certificate authentication — there is no TLS on it, so no certificate to present. SHOW POOLS; SHOW SERVERS; SHOW CLIENTS; SHOW STATS;" />
        ) : (
          <Row k="On the node" v="psql -p 6432 -U postgres pgbouncer"
            note="SHOW POOLS; SHOW SERVERS; SHOW CLIENTS; SHOW STATS; — the console is a virtual database on the same port." />
        )}
      </div>

      {!port && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          No host port published — enable export before deploying to reach the pool from your machine. In-stack
          clients can still reach it at <span className="font-mono">{inHost}</span>:6432.
        </div>
      )}
    </div>
  )
}

// PoolingTab turns the numbers in the config into the sentence they imply, and gives
// the three console queries that show whether the pool is doing anything.
function PoolingTab({ cfg }) {
  const clients = cfg.maxClientConn || 0
  const size = cfg.defaultPoolSize || 0
  const ratio = clients && size ? Math.max(1, Math.round(clients / size)) : 0
  return (
    <div className="space-y-3 text-sm">
      {ratio > 0 && (
        <div className="rounded-lg border border-primary/30 bg-primary/10 px-2.5 py-2 text-[11px] leading-snug text-primary">
          Up to <span className="font-mono font-medium">{clients}</span> client connections share
          <span className="font-mono font-medium"> {size}</span> server connections per user and database — a
          <span className="font-medium">{` ${ratio}:1`}</span> reduction in PostgreSQL backend processes.
        </div>
      )}
      <div className="space-y-2">
        <KV k="pool_mode" v={cfg.poolMode} mono />
        <KV k="max_client_conn" v={String(cfg.maxClientConn ?? '—')} mono />
        <KV k="default_pool_size" v={String(cfg.defaultPoolSize ?? '—')} mono />
        <KV k="min_pool_size" v={String(cfg.minPoolSize ?? 0)} mono />
        <KV k="reserve_pool_size" v={String(cfg.reservePoolSize ?? 0)} mono />
        <KV k="max_db_connections" v={cfg.maxDbConnections ? String(cfg.maxDbConnections) : 'unlimited'} mono />
        <KV k="auth_type" v={cfg.authType} mono />
        <KV k="auth_query" v={cfg.authType === 'cert'
          ? 'n/a under certificate authentication — userlist.txt only'
          : (cfg.authQuery ? 'on — roles looked up in pg_shadow' : 'off — userlist.txt only')} />
        <KV k="ignore_startup_parameters" v={cfg.ignoreStartupParameters} mono />
        <KV k="server_tls_sslmode" v={cfg.serverTlsMode} mono />
        <KV k="client_tls_sslmode" v={cfg.authType === 'cert'
          ? 'verify-full — required by auth_type=cert'
          : (cfg.generateCert ? 'require (Intranet CA certificate)' : 'disable')} />
        <KV k="Routing" v={cfg.routing} mono />
        <KV k="Pooled database" v={cfg.database} mono />
      </div>
      {cfg.poolMode && (
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          {POOL_MODE_NOTE[cfg.poolMode]}
        </div>
      )}
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Watch it work</div>
        <Row k="Pools, one row per user × database" v="psql -p 6432 -U postgres pgbouncer -c 'SHOW POOLS'"
          note="cl_active vs sv_active is the whole story: many clients, few servers." />
        <Row k="Server connections the pool holds" v="psql -p 6432 -U postgres pgbouncer -c 'SHOW SERVERS'" />
        <Row k="Where the pool currently points" v="cat /etc/pgbouncer/databases.ini" />
        {cfg.followRole && (
          <Row k="Re-probe the backend now" v="/usr/local/bin/dbcanvas-pgbouncer-watch"
            note={`Normally runs on a timer every ${cfg.watchSeconds}s; run it by hand after forcing a failover to see the pool move.`} />
        )}
      </div>
    </div>
  )
}
