import { useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE, mongoApi } from '../lib/stackApi.js'
import { useTerminals } from '../terminal/TerminalProvider.jsx'
import DbLoginGuide from '../components/DbLoginGuide.jsx'
import VaultGuide from '../components/VaultGuide.jsx'
import MongoCertReissue from '../components/MongoCertReissue.jsx'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'access', label: 'Access' },
  { id: 'tls', label: 'TLS' },
  { id: 'creds', label: 'Credentials' },
  { id: 'dirlogin', label: 'Directory Login' },
  { id: 'sso', label: 'Keycloak SSO' },
  { id: 'encryption', label: 'Encryption' },
  { id: 'backup', label: 'Backup' },
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

// CodeBlock is a labelled, copyable multi-line snippet (config / command).
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

// roleText renders a member's place in its topology.
function roleText(cfg) {
  if (cfg.role === 'mongos') return 'mongos router'
  if (cfg.role === 'config') return `config server (${cfg.replSet})`
  if (cfg.role === 'member') return `replica-set member (${cfg.replSet})`
  if (cfg.role === 'standalone') return 'standalone server'
  return `shard ${cfg.shard} member (${cfg.replSet})`
}

export default function MongoDBManager({ stackId, nodeId, frameId, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const { openTerminal } = useTerminals()
  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const hasBackup = !!cfg.enablePBM
  const isMongos = cfg.role === 'mongos'
  // Sharded shard/config members are meant to be reached via the router; mongos,
  // replica-set members and standalone nodes are reachable directly.
  const isInternal = cfg.role === 'config' || cfg.role === 'shard'
  const exportPort = isMongos ? (cfg.mongosPort || cfg.exportPort) : cfg.exportPort

  const adminUser = sec.adminUser || 'admin'
  const hostConn = exportPort
    ? `mongosh "mongodb://${adminUser}@${host}:${exportPort}/?authSource=admin"`
    : ''
  const inClusterConn = `mongosh "mongodb://${adminUser}@${cfg.fqdn}:27017/?authSource=admin"`

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PS MongoDB · {cfg.hostname}</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>

      <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
        {TABS.filter((t) => (t.id !== 'backup' || hasBackup) && (t.id !== 'tls' || cfg.generateCert) && (t.id !== 'dirlogin' || cfg.dirAuth?.enabled) && (t.id !== 'sso' || cfg.oidcEnabled) && (t.id !== 'encryption' || cfg.vault?.enabled)).map((t) => (
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
          {!isInternal && <KV k="Exported port" v={exportPort || 'not published'} />}
          <KV k="TLS" v={cfg.generateCert ? 'cert issued (see TLS tab)' : 'none'} />
          <KV k="Backups (PBM)" v={cfg.enablePBM ? (cfg.backupRepo || 'enabled') : 'disabled'} />
          {cfg.oidcEnabled && <KV k="Keycloak SSO" v="enabled (see Keycloak SSO tab)" />}
          {cfg.vault?.enabled && <KV k="Encryption at rest" v={`OpenBao · ${cfg.vault.mount}`} />}
          <KV k="Monitored by" v={cfg.monitoredBy} mono />
          {cfg.serverVersion && <KV k="Version" v={cfg.serverVersion} mono />}
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
          {isInternal ? (
            <div className="text-xs text-muted">
              {cfg.role === 'config' ? 'Config servers' : 'Shard members'} are internal to the cluster. Connect applications through the mongos router instead.
              <div className="mt-2"><CopyRow label="Direct (admin, debugging)" value={inClusterConn} /></div>
            </div>
          ) : (
            <>
              <div className="text-[11px] text-muted">
                {isMongos ? 'Apps connect to the sharded cluster through this mongos router.'
                  : cfg.role === 'standalone' ? 'Connect applications directly to this standalone server.'
                    : 'Connect applications to the replica set (this member auto-elects with its peers).'}
              </div>
              {exportPort ? (
                <CopyRow label={`From the host (${exportPort})`} value={hostConn} />
              ) : (
                <div className="text-xs text-muted">Port not published to the host (enable export on this node to expose 27017).</div>
              )}
              <CopyRow label="In-cluster (from another container)" value={inClusterConn} />
              {cfg.oidcEnabled && <div className="pt-1 text-[11px] text-muted">Signing in as a Keycloak user? See the <span className="font-medium">Keycloak SSO</span> tab.</div>}
            </>
          )}
        </div>
      )}

      {tab === 'tls' && cfg.generateCert && (
        (() => {
          const dir = '/etc/mongo/certs'
          const svcFile = isMongos ? '/etc/mongos.conf' : '/etc/mongod.conf'
          const svc = isMongos ? 'mongos' : 'mongod'
          const isCluster = cfg.role !== 'standalone'
          const serverCfg =
`# ${svcFile} — merge into the existing net: block, then restart the service.
net:
  tls:
    mode: requireTLS
    certificateKeyFile: ${dir}/server.pem            # this node's cert + key
    CAFile: ${dir}/ca.crt                             # Intranet CA (verifies peers/clients)
    allowConnectionsWithoutCertificates: true         # allow password auth over TLS (no client cert)

# systemctl restart ${svc}
# Drop allowConnectionsWithoutCertificates to REQUIRE X.509 client certs (see below).`
          const clientInCluster =
`mongosh --tls --tlsCAFile ${dir}/ca.crt \\
  --host ${cfg.fqdn} --port 27017 \\
  -u ${adminUser} -p --authenticationDatabase admin`
          const clientHost = exportPort
? `# Copy the CA locally first (from a root console: cat ${dir}/ca.crt), then:
mongosh --tls --tlsCAFile ./ca.crt \\
  --host ${host} --port ${exportPort} \\
  -u ${adminUser} -p --authenticationDatabase admin`
            : ''
          const x509 =
`# Optional — X.509 client-certificate auth (the issued cert also has clientAuth).
# On the server, create an $external user named after a client cert's subject:
db.getSiblingDB("$external").runCommand({ createUser: "CN=<client>,O=DBCanvas",
  roles: [ { role: "readWrite", db: "admin" } ] })
# Connect with a client cert + key PEM:
mongosh --tls --tlsCAFile ${dir}/ca.crt --tlsCertificateKeyFile client.pem \\
  --host ${cfg.fqdn} --authenticationDatabase '$external' \\
  --authenticationMechanism MONGODB-X509`
          return (
            <div className="space-y-3">
              <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
                A per-node certificate signed by the Intranet CA has been issued and stored on this
                node. TLS is <span className="font-semibold">not auto-enabled</span> — apply the server
                config below to turn it on{isCluster ? ', then repeat on every member (and the mongos) and restart each' : ' and restart mongod'}.
                {isCluster && <> For a rolling enable without downtime, use <span className="font-mono">mode: preferTLS</span> first, then switch to <span className="font-mono">requireTLS</span>.</>}
              </div>
              <MongoCertReissue stackId={stackId} nodeId={nodeId} />
              <CopyRow label="Certificate + key (PEM)" value={`${dir}/server.pem`} />
              <CopyRow label="CA certificate" value={`${dir}/ca.crt`} />
              <CodeBlock label={`Server configuration — ${svcFile}`} text={serverCfg} />
              <CodeBlock label="Client — in-cluster (mongosh over TLS)" text={clientInCluster} />
              {clientHost && <CodeBlock label={`Client — from the host (port ${exportPort})`} text={clientHost} />}
              <CodeBlock label="Client — X.509 certificate auth (optional)" text={x509} />
            </div>
          )
        })()
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

      {tab === 'dirlogin' && <DbLoginGuide engine="psm" info={cfg.dirAuth} />}
      {tab === 'sso' && cfg.oidcEnabled && <KeycloakSSOTab cfg={cfg} sec={sec} />}
      {tab === 'encryption' && <VaultGuide engine="psm" info={cfg.vault} />}
      {tab === 'backup' && hasBackup && <BackupTab stackId={stackId} frameId={frameId} cfg={cfg} sec={sec} />}
    </div>
  )
}

// KeycloakSSOTab — everything about MONGODB-OIDC logins on this node: the identity provider it
// trusts, how users are authorized, the sample Keycloak accounts, and the mongosh invocations.
// Kept apart from "Directory Login" (LDAP/Kerberos), which is a different mechanism and cannot
// be enabled at the same time.
function KeycloakSSOTab({ cfg, sec }) {
  const mongosh = `mongosh --host ${cfg.fqdn} --authenticationMechanism MONGODB-OIDC`
  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        This node accepts Keycloak logins over <span className="font-mono">MONGODB-OIDC</span>. Every flow
        opens a browser to Keycloak, so run mongosh where a browser is reachable — the
        <span className="font-medium"> Ubuntu VNC</span> desktop node is the usual place. Manage users and
        groups on the Keycloak node.
      </div>

      <div className="space-y-2 text-sm">
        <KV k="Issuer" v={cfg.oidcIssuer} mono />
        <KV k="Client ID" v={cfg.oidcClientId} mono />
        <KV k="Authorization" v={cfg.oidcUseAuthClaim ? `by group claim (${cfg.oidcAuthClaim})` : 'by username ($external users)'} />
        {cfg.oidcSampleUsers && <KV k="Sample users" v={cfg.oidcSampleUsers} />}
      </div>

      {sec.oidcSamplePassword && (
        <div>
          <div className="text-xs text-muted">Sample users password</div>
          <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
            <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{sec.oidcSamplePassword}</span>
            <CopyButton text={sec.oidcSamplePassword} />
          </div>
        </div>
      )}

      <div className="space-y-1">
        <div className="text-[11px] text-muted">
          Log in as a Keycloak user (e.g. <span className="font-mono">alice</span>). From any host other than
          the server itself, mongosh needs <span className="font-mono">--oidcTrustedEndpoint</span> — it
          otherwise only permits OIDC to localhost.
        </div>
        <CopyRow label="From the VNC desktop (auth-code, opens a browser)" value={`${mongosh} --oidcFlows auth-code --oidcTrustedEndpoint`} />
        <CopyRow label="Headless (device-auth, enter a code in a browser)" value={`${mongosh} --oidcFlows device-auth --oidcTrustedEndpoint`} />
        <CopyRow label="On the server itself (localhost, no flag needed)" value="mongosh --authenticationMechanism MONGODB-OIDC --oidcFlows device-auth" />
      </div>

      {!cfg.oidcUseAuthClaim && (
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          Authorization is by username: create each Keycloak user in the
          <span className="font-mono"> $external</span> database before they can log in.
        </div>
      )}
    </div>
  )
}

// BackupTab runs an on-demand Percona Backup for MongoDB (PBM) backup for the whole
// cluster (coordinated from a config server / RS primary by the backend).
function BackupTab({ stackId, frameId, cfg, sec }) {
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState(null)
  async function runBackup() {
    setBusy(true)
    setMsg(null)
    try {
      await mongoApi(stackId, frameId).pbmBackup()
      setMsg({ tone: 'success', text: 'PBM backup started.' })
    } catch (e) {
      setMsg({ tone: 'danger', text: e.message || 'Backup failed.' })
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="space-y-3 text-sm">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] text-muted">
        Percona Backup for MongoDB (<span className="font-mono">pbm-agent</span> on every member) backs the
        cluster up to the SeaweedFS S3 store (<span className="font-mono">{cfg.backupRepo || 'SeaweedFS'}</span>).
        Backups are cluster-wide; this runs <span className="font-mono">pbm backup</span> from a coordinating
        member. List/restore with <span className="font-mono">pbm list</span> / <span className="font-mono">pbm restore</span> from a root console.
      </div>
      <Button size="sm" className="w-full" disabled={busy} onClick={runBackup}>
        <Icon.Arrow size={15} /> {busy ? 'Starting backup…' : 'Backup now'}
      </Button>
      {sec.pbmUser && <CopyRow label="PBM user" value={sec.pbmUser} />}
      {sec.pbmPassword && <CopyRow label="PBM password" value={sec.pbmPassword} />}
      {msg && (
        <div className={`rounded-lg border px-2.5 py-1.5 text-xs ${msg.tone === 'danger' ? 'border-danger/30 bg-danger/15 text-danger' : 'border-success/30 bg-success/15 text-success'}`}>
          {msg.text}
        </div>
      )}
    </div>
  )
}
