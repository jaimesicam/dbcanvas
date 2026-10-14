import { useState } from 'react'
import { Icon } from './Icons.jsx'
import { ConfirmButton } from './ui.jsx'

// VaultGuide — the "Encryption" tab of a deployed Percona Server / PSMDB / PostgreSQL node that
// was wired to an OpenBao node at deploy. The keyring is already configured; this says what was
// configured (so it can be audited) and how to actually use it. Driven by dep.config.vault
// (vaultInfo in app/dbvault.go); `engine` ∈ {ps, psm, pg, pxc, mysql}.
//
// pxc and mysql are cluster MEMBERS and share the ps guide, plus one paragraph they need and a
// standalone server does not: this member's mount holds this member's master key and nobody
// else's. It is the question everybody asks of a cluster with a vault in front of it, and the
// answer is counter-intuitive enough to be worth a sentence — rows replicate, keys do not.

function CopyButton({ text }) {
  const [done, setDone] = useState(false)
  return (
    <button title="Copy" onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}
      className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
      {done ? <Icon.Check size={14} /> : <Icon.Copy size={14} />}
    </button>
  )
}

function Code({ label, text }) {
  return (
    <div>
      <div className="mb-1 flex items-center justify-between">
        <span className="text-xs font-medium text-muted">{label}</span>
        <CopyButton text={text} />
      </div>
      <pre className="max-h-60 overflow-auto whitespace-pre rounded-lg border bg-bg p-2 font-mono text-[11px] leading-relaxed text-fg">{text}</pre>
    </div>
  )
}

function KV({ k, v }) {
  return (
    <div className="flex justify-between gap-3 text-sm">
      <span className="text-muted">{k}</span>
      <span className="truncate font-mono text-xs text-fg">{v || '—'}</span>
    </div>
  )
}

// `onRotate` (PostgreSQL only) is the pg_tde principal-key rotation for this node or cluster —
// pgApi/patroniApi .tdeRotate; it resolves to the new vault info.
export default function VaultGuide({ engine, info, onRotate }) {
  if (!info || !info.enabled) return null
  const plugin = info.method === 'keyring_vault plugin'

  const rows = (
    <div className="space-y-1.5 rounded-lg border p-2">
      <KV k="Method" v={info.method} />
      {info.build && <KV k="pg_tde" v={info.build} />}
      <KV k="OpenBao" v={info.addr} />
      <KV k="KV mount" v={`${info.mount} (v${info.kvVersion})`} />
      {info.secretPath && <KV k="Secret" v={info.secretPath} />}
      {info.confFile && <KV k="Config" v={info.confFile} />}
      {info.tokenFile && <KV k="Token file" v={info.tokenFile} />}
      <KV k="CA" v={info.caCert || 'none (plain HTTP OpenBao)'} />
    </div>
  )

  if (engine === 'psm') {
    return (
      <div className="space-y-3">
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          This node encrypts its data at rest with a master key kept in OpenBao
          (<span className="font-mono">security.vault</span>). The key was written at first start, and mongod
          reads it back on every boot — the node cannot start if OpenBao is sealed or unreachable.
        </div>
        {rows}
        <Code label="Confirm encryption is on" text={`mongosh --quiet --eval 'db.serverStatus().encryptionAtRest'`} />
        <Code label="Read the master key straight from OpenBao (on the OpenBao node)" text={`bao kv get ${info.mount}/${(info.secretPath || '').split('/data/')[1] || 'master-key'}`} />
        <Code label="Rotate the master key (one restart, then remove the flag)" text={`# add to the vault: block in /etc/mongod.conf
#     rotateMasterKey: true
systemctl restart mongod     # mongod rotates, then exits by design
# remove rotateMasterKey, then start it again:
systemctl start mongod`} />
      </div>
    )
  }

  if (engine === 'pg') {
    const key = (info.secretPath || '').split('/data/')[1] || 'principal'
    return (
      <div className="space-y-3">
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          This node encrypts at rest with <span className="font-mono">pg_tde</span>, whose principal key lives in
          OpenBao. Unlike the other engines the keyring is not a config file: OpenBao is registered as a{' '}
          <span className="font-mono">global key provider</span> from SQL, and the token is read from a file on
          disk. PostgreSQL cannot read an encrypted table while OpenBao is sealed or unreachable.
        </div>
        {rows}
        {info.build && (
          <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
            Each PostgreSQL server build loads only some <span className="font-mono">pg_tde</span> builds, and the
            packages do not say which — the rest install and then stop the server. The deploy tried them in a
            scratch cluster, newest first, and kept <span className="font-mono">{info.build}</span>.
          </div>
        )}
        {info.shared && (
          <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
            The whole Patroni cluster shares this KV mount and principal key — every member reads it with the same
            token file. Replicas are physical copies of the leader&apos;s encrypted files, so a member with a key of
            its own could not read a page it was given. <span className="font-mono">tde_heap</span> is the default
            through Patroni&apos;s configuration (<span className="font-mono">patronictl show-config</span>), so a
            member promoted later has it too, and Patroni rewinds and rebuilds members with{' '}
            <span className="font-mono">pg_tde_rewind</span> and <span className="font-mono">pg_tde_basebackup</span> —
            plain <span className="font-mono">pg_rewind</span> leaves a rewound member with unreadable encrypted pages.
          </div>
        )}
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          <span className="font-mono">default_table_access_method</span> is{' '}
          <span className="font-mono">tde_heap</span> on this node, and the extension is in{' '}
          <span className="font-mono">template1</span> — so tables created here, in any database made from the
          default template, are encrypted without asking. A database created from{' '}
          <span className="font-mono">template0</span> needs its own{' '}
          <span className="font-mono">CREATE EXTENSION pg_tde</span> before it can create a table at all.
        </div>
        <Code label="Confirm the provider and the default key" text={`psql -U postgres -xc "SELECT * FROM pg_tde_list_all_global_key_providers();"
psql -U postgres -xc "SELECT * FROM pg_tde_default_key_info();"`} />
        <Code label="Create a table and check it is encrypted" text={`psql -U postgres -c "CREATE TABLE enc_demo (id int) USING tde_heap;"
psql -U postgres -c "SELECT pg_tde_is_encrypted('enc_demo');"   # -> t`} />
        <Code label="Encrypt an existing table (rewrites it)" text={`psql -U postgres -c "ALTER TABLE my_table SET ACCESS METHOD tde_heap;"
# SET ACCESS METHOD drops hint bits — reset them so reads do not pay for it:
psql -U postgres -c "SELECT count(*) FROM my_table;"`} />
        <Code label="Read the principal key straight from OpenBao (on the OpenBao node)" text={`bao kv get ${info.mount}/${key}`} />
        {onRotate && <RotateKey info={info} onRotate={onRotate} />}
        {info.wal ? (
          <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
            WAL is encrypted too (<span className="font-mono">pg_tde.wal_encrypt = on</span>, with the server key).
            pgBackRest archives through <span className="font-mono">pg_tde_archive_decrypt</span> and restores through{' '}
            <span className="font-mono">pg_tde_restore_encrypt</span>, so the archive holds WAL it can verify and
            replay — protect the repository itself. Its <span className="font-mono">archive-header-check</span> and{' '}
            <span className="font-mono">checksum-page</span> are off, as Percona requires.
          </div>
        ) : (
          <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-snug text-muted">
            WAL encryption is <span className="font-medium">not</span> on, so a row written to an encrypted table is
            readable in the WAL that carries it. It is a deploy-time choice here (&quot;Encrypt WAL too&quot; on the
            form), because Percona supports it only with <span className="font-mono">pg_tde_archive_decrypt</span> /{' '}
            <span className="font-mono">pg_tde_restore_encrypt</span> around archiving and restore, which DBCanvas then
            sets up. Table data is encrypted either way.
          </div>
        )}
      </div>
    )
  }

  // ps — Percona Server for MySQL, standalone or as a cluster member (pxc / mysql)
  const member = engine === 'pxc' || engine === 'mysql'
  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        This node's keyring is OpenBao: {plugin
          ? <>the <span className="font-mono">keyring_vault</span> plugin (loaded with <span className="font-mono">early-plugin-load</span>, since the keyring component only exists from Percona Server 8.4)</>
          : <>the <span className="font-mono">component_keyring_vault</span> component, declared by the global manifest next to <span className="font-mono">mysqld</span></>}.
        Master keys live in OpenBao — MySQL will not open an encrypted tablespace while it is sealed or unreachable.
      </div>
      {rows}
      {member && (
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          Every member of this cluster is encrypted, and each has a KV mount of its own —{' '}
          <span className="font-mono">{info.mount}</span> is this one's. That is Percona's rule (a{' '}
          <span className="font-mono">secret_mount_point</span> serves exactly one server) and it works because
          keys are never what travels between members: {engine === 'pxc'
            ? <>write-sets carry rows, and a joiner's SST re-encrypts the donor's tablespace keys under a master key it generates for itself.</>
            : <>a replica applies the source's changes and encrypts what it writes with its own master key.</>}
          {' '}So rotating here rotates this member only, and a member whose OpenBao mount is unreachable is the
          only one that stops.
        </div>
      )}
      <Code label="Confirm the keyring is loaded" text={plugin
        ? `mysql -e "SELECT PLUGIN_NAME, PLUGIN_STATUS FROM information_schema.plugins WHERE PLUGIN_NAME='keyring_vault'"`
        : `mysql -e "SELECT * FROM performance_schema.keyring_component_status"`} />
      <Code label="Encrypt a table (and check)" text={`mysql -e "CREATE DATABASE IF NOT EXISTS enc; CREATE TABLE enc.t (id INT PRIMARY KEY) ENCRYPTION='Y';"
mysql -e "SELECT NAME, ENCRYPTION FROM information_schema.innodb_tablespaces WHERE NAME LIKE 'enc/%'"`} />
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        <span className="font-mono">default_table_encryption=ON</span> is already set in this server's config, so a
        table created here is encrypted without the <span className="font-mono">ENCRYPTION=&apos;Y&apos;</span> clause
        above. An explicit clause still wins, in either direction.
      </div>
      <Code label="Rotate the master key" text={`mysql -e "ALTER INSTANCE ROTATE INNODB MASTER KEY;"`} />
    </div>
  )
}

// RotateKey is pg_tde's principal-key rotation: a new key in OpenBao, made the default (tables)
// and the server key (WAL); pg_tde re-wraps its internal keys, the pages are not rewritten. The
// old key stays in OpenBao, so a backup from before the rotation can still be restored.
function RotateKey({ info, onRotate }) {
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState(null)
  async function rotate() {
    setBusy(true)
    setMsg(null)
    try {
      const r = await onRotate()
      setMsg({ ok: true, text: `Rotated to ${r.key}.` })
    } catch (e) {
      setMsg({ ok: false, text: e.message || 'Rotation failed.' })
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="space-y-1.5 rounded-lg border p-2">
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs font-medium text-muted">Principal key</span>
        <ConfirmButton size="sm" variant="outline" disabled={busy} confirmLabel="Rotate now?" onConfirm={rotate}>
          {busy ? 'Rotating…' : 'Rotate key'}
        </ConfirmButton>
      </div>
      <p className="text-[11px] leading-snug text-muted">
        Creates a new key in <span className="font-mono">{info.mount}</span> and makes it the default and server key;
        existing data is re-wrapped, not rewritten, and the old key stays in OpenBao for older backups.
        {info.rotatedAt && <> Last rotated {new Date(info.rotatedAt).toLocaleString()}.</>}
      </p>
      {msg && <p className={`text-[11px] ${msg.ok ? 'text-success' : 'text-danger'}`}>{msg.text}</p>}
    </div>
  )
}
