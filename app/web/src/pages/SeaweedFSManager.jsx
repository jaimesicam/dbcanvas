import { useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE } from '../lib/stackApi.js'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'access', label: 'Access' },
  { id: 'backups', label: 'Backups' },
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

export default function SeaweedFSManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const cfg = dep.config || {}
  const sec = dep.secrets || {}

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">SeaweedFS</span>
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
      {tab === 'access' && <AccessTab cfg={cfg} sec={sec} />}
      {tab === 'backups' && <BackupsTab cfg={cfg} sec={sec} />}
    </div>
  )
}

// hostEndpoint is the S3 URL reachable from your machine (published host port);
// internalEndpoint (cfg.internalEndpoint) is what the in-stack DB nodes use.
// webEndpoint is the SeaweedFS web interface (volume-server status UI, served at
// /ui/index.html on container 8080) reachable from your machine via the published
// host port.
function webEndpoint(cfg) {
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  return cfg.webPort ? `http://${host}:${cfg.webPort}/ui/index.html` : ''
}

function Overview({ cfg, dep, onDeleteNode }) {
  const web = webEndpoint(cfg)
  return (
    <div className="space-y-2 text-sm">
      <KV k="FQDN" v={cfg.fqdn} mono />
      <KV k="Image" v={cfg.image} mono />
      <KV k="Network alias" v={cfg.alias} mono />
      <KV k="Bucket" v={cfg.bucket} mono />
      <KV k="Region" v={cfg.region || 'us-east-1'} mono />
      <KV k="S3 TLS" v={cfg.tls ? (cfg.generateCert ? 'HTTPS · Intranet-CA cert' : 'HTTPS · self-signed') : 'disabled (HTTP)'} />
      <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
      {web && (
        <a href={web} target="_blank" rel="noreferrer"
          className="mt-2 flex items-center justify-center gap-2 rounded-lg border border-primary/40 bg-primary/10 px-3 py-2 text-sm font-medium text-primary hover:bg-primary/15">
          <Icon.External size={15} /> Open web interface (8080)
        </a>
      )}
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// ----------------------------------------------------------------- access tab

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

function AccessTab({ cfg, sec }) {
  const web = webEndpoint(cfg)
  return (
    <div className="space-y-3">
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Endpoints</div>
        <Row k="S3 endpoint (use from the database nodes · :8333)" v={cfg.internalEndpoint} />
        <Row k="Web interface (from your machine · :8080)" v={web} link />
      </div>
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">S3 credentials</div>
        <Row k="AWS_ACCESS_KEY_ID" v={cfg.accessKey || sec.accessKey} />
        <Row k="AWS_SECRET_ACCESS_KEY" v={sec.secretKey} />
        <Row k="AWS_DEFAULT_REGION" v={cfg.region || 'us-east-1'} />
        <Row k="Bucket" v={cfg.bucket} />
      </div>
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] text-muted">
        The S3 API stays on <span className="font-mono">:8333</span> (reached in-network by the database
        nodes); the <span className="font-mono">:8080</span> web interface is what's published to your host.
        SeaweedFS requires <span className="font-mono">path-style</span> addressing
        {cfg.tls ? <> over <span className="font-mono">HTTPS</span>{cfg.generateCert ? ' (Intranet-CA cert)' : ' (self-signed — TLS verification is skipped)'}</> : <> over plain <span className="font-mono">HTTP</span></>} — the
        snippets in the <span className="font-medium text-fg/80">Backups</span> tab already set these.
      </div>
    </div>
  )
}

// ---------------------------------------------------------------- backups tab

function Snippet({ title, note, code }) {
  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between">
        <div className="text-xs font-medium text-fg/80">{title}</div>
        <CopyButton text={code} />
      </div>
      {note && <div className="text-[11px] text-muted">{note}</div>}
      <pre className="overflow-x-auto whitespace-pre rounded-lg border bg-bg p-2 text-[11px] leading-relaxed text-fg">{code}</pre>
    </div>
  )
}

function BackupsTab({ cfg, sec }) {
  // The DB nodes run inside the stack, so the snippets use the in-network S3 endpoint.
  const endpoint = cfg.internalEndpoint || `http://${cfg.fqdn || cfg.alias}:8333`
  const endpointHostPort = endpoint.replace(/^https?:\/\//, '')
  const ak = cfg.accessKey || sec.accessKey || 'seaweedfs'
  const sk = sec.secretKey || '<secret-key>'
  const bucket = cfg.bucket || '<bucket>'
  const region = cfg.region || 'us-east-1'

  const xbcloudPut = `# Stream a backup straight to SeaweedFS S3:
xtrabackup --backup --stream=xbstream --target-dir=/tmp/backup \\
  | xbcloud put \\
      --storage=s3 \\
      --s3-endpoint='${endpoint}' \\
      --s3-bucket-lookup=path \\
      --s3-api-version=4 \\
      --s3-access-key='${ak}' \\
      --s3-secret-key='${sk}' \\
      --s3-region='${region}' \\
      --s3-bucket='${bucket}' \\
      --parallel=10 \\
      backup-$(date +%F)`

  const myCnf = `# /etc/my.cnf (or ~/.my.cnf) — shared xbcloud settings so the
# xtrabackup/xbcloud commands can be run without repeating the flags.
[xbcloud]
storage=s3
s3-endpoint=${endpoint}
s3-bucket-lookup=path
s3-api-version=4
s3-access-key=${ak}
s3-secret-key=${sk}
s3-region=${region}
s3-bucket=${bucket}`

  const xbcloudGet = `# Restore: pull a backup back from SeaweedFS and unpack it.
xbcloud get \\
    --storage=s3 \\
    --s3-endpoint='${endpoint}' \\
    --s3-bucket-lookup=path \\
    --s3-api-version=4 \\
    --s3-access-key='${ak}' \\
    --s3-secret-key='${sk}' \\
    --s3-region='${region}' \\
    --s3-bucket='${bucket}' \\
    --parallel=10 \\
    backup-$(date +%F) \\
  | xbstream -x -C /var/lib/mysql_restore`

  const pbm = `# Percona Backup for MongoDB — save as pbm-s3.yaml, then:
#   pbm config --file pbm-s3.yaml
storage:
  type: s3
  s3:
    region: ${region}
    endpointUrl: ${endpoint}
    forcePathStyle: true
    bucket: ${bucket}
    prefix: pbm
    credentials:
      access-key-id: ${ak}
      secret-access-key: ${sk}`

  const pgbackrest = `# pgBackRest — /etc/pgbackrest/pgbackrest.conf
[global]
repo1-type=s3
repo1-s3-endpoint=${endpointHostPort}
repo1-s3-uri-style=path
repo1-s3-bucket=${bucket}
repo1-s3-region=${region}
repo1-s3-key=${ak}
repo1-s3-key-secret=${sk}
repo1-s3-verify-tls=n
repo1-path=/pgbackrest`

  return (
    <div className="space-y-4">
      <div className="text-[11px] text-muted">
        These use the in-stack endpoint <span className="font-mono">{endpoint}</span>, so run them from
        the database nodes. Replace <span className="font-mono">backup-&lt;date&gt;</span> with your backup name.
      </div>
      <Snippet title="xtrabackup → xbcloud put (backup)" code={xbcloudPut} />
      <Snippet title="my.cnf [xbcloud] section" note="Lets you drop the repeated --s3-* flags." code={myCnf} />
      <Snippet title="xbcloud get (restore)" code={xbcloudGet} />
      <Snippet title="Percona Backup for MongoDB (pbm)" code={pbm} />
      <Snippet title="pgBackRest config" code={pgbackrest} />
    </div>
  )
}
