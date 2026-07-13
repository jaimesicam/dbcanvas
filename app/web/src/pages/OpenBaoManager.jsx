import { useCallback, useEffect, useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE, openbaoApi } from '../lib/stackApi.js'
import { useTerminals } from '../terminal/TerminalProvider.jsx'

// OpenBaoManager — properties of a deployed OpenBao node.
//
// "Unseal & Token" carries the only copy of what `bao operator init` printed: OpenBao shows the
// five unseal keys and the root token once and never again, so they are stored with the
// deployment and surfaced here. "Clients" is the reason the node exists: copy-paste setup for
// Percona Server for MySQL (component_keyring_vault), Percona Server for MongoDB
// (security.vault) and the bao CLI itself.

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'keys', label: 'Unseal & Token' },
  { id: 'policies', label: 'Policies' },
  { id: 'clients', label: 'Clients' },
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

// Secret is a value the operator must copy exactly (unseal key / token), masked until revealed.
function Secret({ label, value }) {
  const [show, setShow] = useState(false)
  return (
    <div>
      {label && <div className="text-xs text-muted">{label}</div>}
      <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
        <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">
          {show ? value : '•'.repeat(Math.min(44, (value || '').length))}
        </span>
        <button title={show ? 'Hide' : 'Reveal'} onClick={() => setShow((v) => !v)}
          className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
          <Icon.Search size={14} />
        </button>
        <CopyButton text={value} />
      </div>
    </div>
  )
}

// Code is a labelled, copyable snippet (config file / command block).
function Code({ label, text }) {
  return (
    <div>
      <div className="mb-1 flex items-center justify-between">
        <span className="text-xs font-medium text-muted">{label}</span>
        <CopyButton text={text} />
      </div>
      <pre className="max-h-72 overflow-auto whitespace-pre rounded-lg border bg-bg p-2 font-mono text-[11px] leading-relaxed text-fg">{text}</pre>
    </div>
  )
}

export default function OpenBaoManager({ dep, stackId, nodeId, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const { openTerminal } = useTerminals()
  const api = openbaoApi(stackId, nodeId)
  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const keys = sec.unsealKeys || []
  const mounts = cfg.mounts || []
  const tls = !!cfg.tls

  // Live seal state: OpenBao seals itself whenever the process restarts, so the state stored at
  // deploy goes stale the moment the node is restarted.
  const [seal, setSeal] = useState(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const load = useCallback(async () => {
    try { setSeal(await api.status()) } catch (e) { setErr(e.message) }
  }, [api]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => { load() }, [load])

  const unseal = async () => {
    setBusy(true); setErr('')
    try { await api.unseal(); await load() } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">OpenBao · {cfg.hostname}</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>

      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">{err}</div>}

      {/* Sealed is not an error — it is what a restart leaves behind — but nothing works until it
          is unsealed, so it is surfaced above the tabs with the fix one click away. */}
      {seal?.sealed && (
        <div className="space-y-2 rounded-lg border border-warning/30 bg-warning/10 px-3 py-2">
          <div className="text-[11px] leading-snug text-muted">
            <span className="font-medium text-fg">This node is sealed.</span> OpenBao seals itself every time it
            restarts, and answers nothing until {seal.threshold || sec.threshold || 3} of its unseal keys are
            replayed. DBCanvas holds them.
          </div>
          <Button size="sm" className="w-full" disabled={busy} onClick={unseal}>
            {busy ? 'Unsealing…' : 'Unseal with the stored keys'}
          </Button>
        </div>
      )}

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
          <KV k="FQDN" v={cfg.fqdn} mono />
          <KV k="VAULT_ADDR" v={cfg.addr} mono />
          <KV k="VAULT_CACERT" v={cfg.caCert} mono />
          <KV k="TLS" v={tls ? 'Intranet CA-signed (8200)' : 'disabled — plain HTTP'} />
          <KV k="Config" v={cfg.confFile} mono />
          <KV k="Seal state" v={seal
            ? `${seal.initialized ? 'initialized' : 'not initialized'} · ${seal.sealed ? 'sealed' : 'unsealed'}`
            : (cfg.initted ? 'initialized' : 'not initialized')} />
          {cfg.serverVersion && <KV k="Version" v={cfg.serverVersion} mono />}
          <KV k="Image" v={cfg.image} mono />
          <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
          <p className="pt-1 text-[11px] text-muted">
            The client environment (<span className="font-mono">VAULT_ADDR</span>,
            {tls && <span className="font-mono"> VAULT_CACERT</span>}) is exported from
            <span className="font-mono"> /etc/profile.d/openbao.sh</span>, so <span className="font-mono">bao</span> works
            on this node with no flags.
          </p>
          <Button variant="outline" size="sm" className="mt-2 w-full"
            onClick={() => openTerminal({ stackId, nodeId, title: `${cfg.hostname} · root` })}>
            <Icon.Nodes size={16} /> Open root console
          </Button>
          <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
            <Icon.Trash size={16} /> Delete node
          </Button>
        </div>
      )}

      {tab === 'keys' && (
        <div className="space-y-3">
          <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-snug text-muted">
            <span className="font-medium text-fg">This is the only copy.</span> OpenBao prints these once, at
            <span className="font-mono"> bao operator init</span>. Any
            <span className="font-medium"> {sec.threshold || 3} of the {keys.length || 5}</span> keys unseal the
            server; the root token authenticates every admin command. The node was unsealed for you at deploy,
            and a restart seals it again — the Overview tab replays the stored keys for you, or use the command
            below.
          </div>
          <Secret label="Root token" value={sec.rootToken || ''} />
          <div className="space-y-1.5">
            <div className="text-xs font-medium text-muted">Unseal keys (base64)</div>
            {keys.length === 0 && <div className="text-xs text-muted">No keys stored — the node was not initialized.</div>}
            {keys.map((k, i) => <Secret key={i} label={`Key ${i + 1}`} value={k} />)}
          </div>
          <Code label="Unseal after a restart (any 3 keys)" text={`bao operator unseal   # repeat ${sec.threshold || 3}×, one key each time
bao status`} />
        </div>
      )}

      {tab === 'policies' && (
        <div className="space-y-3">
          <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
            One KV mount + policy per engine and KV version, created at deploy. The policy files live in
            <span className="font-mono"> {cfg.policyDir || '/etc/openbao.d'}</span>; each policy is named after its
            mount. Give a database its own token with
            <span className="font-mono"> bao token create -policy=&lt;name&gt;</span>.
          </div>
          <div className="space-y-2">
            {mounts.map((m) => (
              <div key={m.path} className="rounded-lg border p-2 text-xs">
                <div className="flex items-center justify-between">
                  <span className="font-mono font-medium text-fg">{m.path}</span>
                  <Badge tone="primary">KV v{m.version}</Badge>
                </div>
                <div className="mt-1 text-muted">{m.engine}</div>
                <div className="mt-0.5 font-mono text-[11px] text-muted">{m.policyFile}</div>
              </div>
            ))}
          </div>
          <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-snug text-muted">
            Percona Server for MongoDB supports <span className="font-medium">KV v2 only</span>, so there is no
            v1 MongoDB mount. Percona Server for MySQL works with either version — and each server instance
            needs its <span className="font-medium">own</span> secret path.
          </div>
          <Code label="Mint a token for a database (root console)" text={`# MySQL (KV v2 mount; mysql-v1 works too)
bao token create -policy=mysql-v2 -period=768h -field=token

# MongoDB (KV v2 — the only version PSMDB supports)
bao token create -policy=mongodb-v2 -period=768h -field=token`} />
        </div>
      )}

      {tab === 'clients' && <ClientsTab cfg={cfg} tls={tls} />}
    </div>
  )
}

// ClientsTab — copy-paste setup for the clients: the bao CLI, Percona Server for MySQL (the
// keyring component on 8.4, the keyring plugin on 5.7/8.0 — the component does not exist there)
// and Percona Server for MongoDB (security.vault). Rendered against this node's real addr.
//
// These are the manual path. A ps/psm node can instead tick "Encrypt with OpenBao" in the
// designer and DBCanvas performs exactly these steps at deploy (see app/dbvault.go) — the
// snippets deliberately use the same files it does.
//
// Nothing here copies a certificate: a stack has exactly one CA (the Intranet CA) and every node
// already carries it in its trust store, so vault_ca / serverCAFile just point at that file.
function ClientsTab({ cfg, tls }) {
  const addr = cfg.addr || 'https://openbao:8200'
  const host = cfg.fqdn || 'openbao'
  const CA = '/etc/pki/ca-trust/source/anchors/dbcanvas-ca.crt'

  const cli = `# Already exported on this node (/etc/profile.d/openbao.sh). On any other node in the
# stack, the Intranet CA is in its trust store, so this is all it takes:
export VAULT_ADDR=${addr}${tls ? `\nexport VAULT_CACERT=${CA}` : ''}
export VAULT_TOKEN=<root token, or a policy token>

bao status
bao kv put mysql-v2/test key=value && bao kv get mysql-v2/test`

  const mysql = `# 1) On the OpenBao node — a token limited to one policy. Give each server its OWN
#    mount (Percona: a secret_mount_point must be used by a single server):
bao secrets enable -path=mysql-ps01 kv-v2         # kv (v1) for Percona Server 5.7
bao policy write mysql-ps01 /etc/openbao.d/policy-mysql-v2.hcl   # edit the path inside first
bao token create -policy=mysql-ps01 -field=token

# 2a) Percona Server 8.4 — the keyring COMPONENT. The manifest goes beside the mysqld binary, but
#     the config goes in plugin_dir — that is where the server resolves file://component_keyring_vault.
#     (Put the .cnf beside mysqld and the component loads Disabled; the first encrypted table then
#     kills the server.)
BINDIR=$(dirname "$(readlink -f "$(command -v mysqld)")")     # /usr/sbin
PLUGIN_DIR=$(mysql -N -e "SELECT @@plugin_dir")               # /usr/lib64/mysql/plugin
cat > "$PLUGIN_DIR/component_keyring_vault.cnf" <<CNF
{
  "timeout": 15,
  "vault_url": "${addr}",
  "secret_mount_point": "mysql-ps01",
  "secret_mount_point_version": "AUTO",
  "token": "<token from step 1>"${tls ? `,
  "vault_ca": "${CA}"` : ''}
}
CNF
chown mysql:mysql "$PLUGIN_DIR/component_keyring_vault.cnf"
chmod 0600 "$PLUGIN_DIR/component_keyring_vault.cnf"
printf '{ "components": "file://component_keyring_vault" }\\n' > "$BINDIR/mysqld.my"
systemctl restart mysqld
mysql -e "SELECT * FROM performance_schema.keyring_component_status;"   # Component_status must be Active

# 2b) Percona Server 5.7 / 8.0 — the keyring PLUGIN (no component before 8.4):
install -d -o mysql -g mysql -m 0750 /var/lib/mysql-keyring
cat > /var/lib/mysql-keyring/keyring_vault.conf <<CONF
vault_url = ${addr}
secret_mount_point = mysql-ps01
token = <token from step 1>${tls ? `
vault_ca = ${CA}` : ''}
secret_mount_point_version = 2      # omit on 5.7 (KV v1 only — create the mount as kv)
CONF
chown mysql:mysql /var/lib/mysql-keyring/keyring_vault.conf
chmod 0600 /var/lib/mysql-keyring/keyring_vault.conf
cat > /etc/my.cnf.d/dbcanvas-keyring.cnf <<CNF
[mysqld]
early-plugin-load=keyring_vault.so
keyring_vault_config=/var/lib/mysql-keyring/keyring_vault.conf
CNF
systemctl restart mysqld
mysql -e "SELECT PLUGIN_NAME, PLUGIN_STATUS FROM information_schema.plugins WHERE PLUGIN_NAME='keyring_vault';"

# 3) Encrypt:
mysql -e "CREATE TABLE db.t (id INT PRIMARY KEY) ENCRYPTION='Y';"`

  const mongo = `# 1) On the OpenBao node — a token limited to the MongoDB policy (KV v2 only):
bao token create -policy=mongodb-v2 -field=token

# 2) On the PSMDB node — the token file, readable by mongod alone (it refuses lax permissions):
install -d -o mongod -g mongod -m 0755 /etc/mongo
printf '%s' '<token from step 1>' > /etc/mongo/vault.token
chown mongod:mongod /etc/mongo/vault.token && chmod 0600 /etc/mongo/vault.token

# 3) mongod.conf — the secret MUST be <mount>/data/<name> and unique per server. serverCAFile is
#    the Intranet CA already on this node; no certificate is copied anywhere.
cat >> /etc/mongod.conf <<CONF
security:
  enableEncryption: true
  vault:
    serverName: ${host}
    port: 8200
    secret: mongodb-v2/data/$(hostname -s)
    tokenFile: /etc/mongo/vault.token${tls ? `
    serverCAFile: ${CA}` : `
    disableTLSForTesting: true`}
CONF

# 4) Encryption is established at first start, so this only takes on an EMPTY dbPath:
systemctl stop mongod && rm -rf /var/lib/mongo/* && systemctl start mongod
mongosh --eval 'db.serverStatus().encryptionAtRest'   # -> encryptionEnabled: true`

  return (
    <div className="space-y-3">
      <div className="rounded-lg border border-primary/30 bg-primary/10 px-3 py-2 text-[11px] leading-snug text-muted">
        <span className="font-medium text-fg">You usually do not need any of this.</span> Tick
        <span className="font-medium"> “Encrypt with OpenBao”</span> on a Percona Server or PSMDB node in the
        designer and DBCanvas performs these steps at deploy: its own KV mount, a token scoped to it, and the
        right client wiring for that engine and version. The snippets below are the manual equivalent.
      </div>
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        OpenBao speaks the Vault API, so the Percona engines use their normal Vault settings.
        {tls && <> They verify this listener with the Intranet CA — the one CA in the stack, already in every
        node's trust store, so no certificate is ever copied around.</>}
      </div>
      <Code label="bao CLI (this node, or any node in the stack)" text={cli} />
      <Code label="Percona Server for MySQL — component (8.4) or plugin (5.7 / 8.0)" text={mysql} />
      <Code label="Percona Server for MongoDB — security.vault (KV v2)" text={mongo} />
      <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-snug text-muted">
        MongoDB writes its master key at first start, so encryption can only be turned on with an empty
        <span className="font-mono"> dbPath</span> — enabling it on a server that already holds data means
        re-creating that data. To rotate a key later, restart once with
        <span className="font-mono"> rotateMasterKey: true</span>.
      </div>
    </div>
  )
}
