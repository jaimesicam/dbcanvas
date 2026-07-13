import { useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE } from '../lib/stackApi.js'
import { SecretValue } from '../components/Secret.jsx'

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
      {cfg.serverVersion && <KV k="Version" v={cfg.serverVersion} mono />}
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

function Row({ k, v, link, secret }) {
  if (!v) return null
  if (secret) {
    return (
      <div>
        <div className="text-xs text-muted">{k}</div>
        <SecretValue value={v} />
      </div>
    )
  }
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
        <Row k="AWS_SECRET_ACCESS_KEY" v={sec.secretKey} secret />
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

// Snippet shows a copy-paste config. These embed the S3 secret key, so `secret` (the key) is
// blanked out in what is *displayed* — the snippet still copies with the real key in it, and the
// eye reveals it. Same contract as the Credentials rows: copy without revealing.
function Snippet({ title, note, code, secret }) {
  const [show, setShow] = useState(false)
  const shown = secret && !show ? code.split(secret).join('•'.repeat(Math.min(24, secret.length))) : code
  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between">
        <div className="text-xs font-medium text-fg/80">{title}</div>
        <div className="flex items-center gap-1">
          {secret && (
            <button title={show ? 'Hide the secret key' : 'Reveal the secret key'} onClick={() => setShow((s) => !s)}
              className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
              {show ? <Icon.EyeOff size={14} /> : <Icon.Eye size={14} />}
            </button>
          )}
          <CopyButton text={code} />
        </div>
      </div>
      {note && <div className="text-[11px] text-muted">{note}</div>}
      <pre className="overflow-x-auto whitespace-pre rounded-lg border bg-bg p-2 text-[11px] leading-relaxed text-fg">{shown}</pre>
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

  const pgbackrestConf = `# 1) pgBackRest repository config — /etc/pgbackrest/pgbackrest.conf
#    (the [global] block points the S3 repo at this SeaweedFS node; add one
#     [<stanza>] block per PostgreSQL cluster you back up).
[global]
repo1-type=s3
repo1-s3-endpoint=${endpointHostPort}
repo1-s3-uri-style=path
repo1-s3-bucket=${bucket}
repo1-s3-region=${region}
repo1-s3-key=${ak}
repo1-s3-key-secret=${sk}
repo1-s3-verify-tls=n
repo1-path=/pgbackrest
start-fast=y
log-level-console=info

[<stanza>]
pg1-path=<data-dir>     # e.g. /var/lib/pgsql/16/data (EL) or /var/lib/postgresql/16/main (Debian)
pg1-port=5432`

  const pgbackrestArchive = `# 2) Enable WAL archiving in postgresql.conf, then restart PostgreSQL:
archive_mode = on
archive_command = 'pgbackrest --stanza=<stanza> archive-push %p'
wal_level = replica
max_wal_senders = 3`

  const pgbackrestRun = `# 3) Create the stanza, verify it, and take backups (run as the postgres user):
runuser -u postgres -- pgbackrest --stanza=<stanza> stanza-create
runuser -u postgres -- pgbackrest --stanza=<stanza> check
runuser -u postgres -- pgbackrest --stanza=<stanza> --type=full backup
runuser -u postgres -- pgbackrest --stanza=<stanza> --type=incr backup   # incremental
runuser -u postgres -- pgbackrest --stanza=<stanza> info                 # list backups

# Restore (stop PostgreSQL and empty the data dir first):
runuser -u postgres -- pgbackrest --stanza=<stanza> --delta restore`

  const barmanCreds = `# 1) AWS credentials for barman-cloud (as the postgres user):
#    ~postgres/.aws/credentials
[default]
aws_access_key_id = ${ak}
aws_secret_access_key = ${sk}

#    ~postgres/.aws/config — SeaweedFS needs path-style addressing:
[default]
region = ${region}
s3 =
    addressing_style = path`

  const barmanArchive = `# 2) Install barman-cloud + boto3 (from the PGDG / apt.postgresql.org repos),
#    then enable WAL archiving in postgresql.conf and restart PostgreSQL
#    (<server> = a name for the cluster). NOTE on EL: PGDG builds barman for python3.12,
#    so boto3 must go into THAT interpreter (system python3-boto3 lands in 3.9):
#    EL:     dnf install barman-cli python3.12-pip && python3.12 -m pip install boto3
#    Debian: apt-get install barman-cli-cloud python3-boto3
archive_mode = on
archive_command = 'barman-cloud-wal-archive --cloud-provider aws-s3 --endpoint-url ${endpoint} s3://${bucket}/barman/<server> <server> %p'`

  const barmanRun = `# 3) Take / list / restore base backups (run as the postgres user):
runuser -u postgres -- barman-cloud-backup --cloud-provider aws-s3 --endpoint-url ${endpoint} s3://${bucket}/barman/<server> <server>
runuser -u postgres -- barman-cloud-backup-list --cloud-provider aws-s3 --endpoint-url ${endpoint} s3://${bucket}/barman/<server> <server>

# Restore a base backup into an empty data dir, then fetch WAL:
runuser -u postgres -- barman-cloud-restore --cloud-provider aws-s3 --endpoint-url ${endpoint} s3://${bucket}/barman/<server> <server> <backup-id> <data-dir>
restore_command = 'barman-cloud-wal-restore --cloud-provider aws-s3 --endpoint-url ${endpoint} s3://${bucket}/barman/<server> <server> %f %p'`

  return (
    <div className="space-y-4">
      <div className="text-[11px] text-muted">
        These use the in-stack endpoint <span className="font-mono">{endpoint}</span>, so run them from
        the database nodes. Replace <span className="font-mono">backup-&lt;date&gt;</span> with your backup name.
      </div>
      <Snippet title="xtrabackup → xbcloud put (backup)" code={xbcloudPut} secret={sec.secretKey} />
      <Snippet title="my.cnf [xbcloud] section" note="Lets you drop the repeated --s3-* flags." code={myCnf} secret={sec.secretKey} />
      <Snippet title="xbcloud get (restore)" code={xbcloudGet} secret={sec.secretKey} />
      <Snippet title="Percona Backup for MongoDB (pbm)" code={pbm} secret={sec.secretKey} />

      <div className="space-y-2 rounded-lg border border-border bg-surface2/40 p-2">
        <div className="text-xs font-semibold text-fg">pgBackRest → SeaweedFS S3</div>
        <div className="text-[11px] text-muted">
          Back up a PostgreSQL node to this SeaweedFS bucket in three steps: point the repo at
          the S3 endpoint, turn on WAL archiving, then create the stanza and take a backup.
          Replace <span className="font-mono">&lt;stanza&gt;</span> with a name for the cluster
          (e.g. its hostname) and <span className="font-mono">&lt;data-dir&gt;</span> with the
          PostgreSQL data directory. DBCanvas&apos;s <span className="font-medium text-fg/80">PostgreSQL</span>{' '}
          and <span className="font-medium text-fg/80">Patroni</span> nodes do all of this automatically
          when their <span className="font-mono">Use pgBackRest</span> option points at this node —
          these snippets are for a manual or external client.
        </div>
        <Snippet title="1 · pgbackrest.conf (repository + stanza)" code={pgbackrestConf} secret={sec.secretKey} />
        <Snippet title="2 · postgresql.conf (WAL archiving)" code={pgbackrestArchive} secret={sec.secretKey} />
        <Snippet title="3 · stanza-create + backup + restore" code={pgbackrestRun} secret={sec.secretKey} />
      </div>

      <div className="space-y-2 rounded-lg border border-border bg-surface2/40 p-2">
        <div className="text-xs font-semibold text-fg">Barman (cloud) → SeaweedFS S3</div>
        <div className="text-[11px] text-muted">
          Barman&apos;s cloud utilities (<span className="font-mono">barman-cloud-backup</span> /
          <span className="font-mono"> -wal-archive</span>) push WAL + base backups straight to this SeaweedFS
          bucket — no separate Barman server. They use <span className="font-mono">boto3</span>, which works over
          plain HTTP <em>or</em> HTTPS (TLS not required). Replace <span className="font-mono">&lt;server&gt;</span>
          with a name for the cluster and <span className="font-mono">&lt;data-dir&gt;</span> with the PostgreSQL
          data directory. DBCanvas&apos;s <span className="font-medium text-fg/80">repmgr cluster</span> nodes do all
          of this automatically when their <span className="font-mono">Use Barman</span> option points at this node —
          these snippets are for a manual or external client.
        </div>
        <Snippet title="1 · ~postgres/.aws credentials + config" code={barmanCreds} secret={sec.secretKey} />
        <Snippet title="2 · postgresql.conf (WAL archiving)" code={barmanArchive} secret={sec.secretKey} />
        <Snippet title="3 · backup / list / restore" code={barmanRun} secret={sec.secretKey} />
      </div>
    </div>
  )
}
