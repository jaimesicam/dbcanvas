import { useState } from 'react'
import { Icon } from './Icons.jsx'

// VaultGuide — the "Encryption" tab of a deployed Percona Server / PSMDB / PostgreSQL node that
// was wired to an OpenBao node at deploy. The keyring is already configured; this says what was
// configured (so it can be audited) and how to actually use it. Driven by dep.config.vault
// (vaultInfo in app/dbvault.go); `engine` ∈ {ps, psm, pg}.

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

export default function VaultGuide({ engine, info }) {
  if (!info || !info.enabled) return null
  const plugin = info.method === 'keyring_vault plugin'

  const rows = (
    <div className="space-y-1.5 rounded-lg border p-2">
      <KV k="Method" v={info.method} />
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
        <Code label="Rotate the principal key" text={`psql -U postgres -c "SELECT pg_tde_set_default_key_using_global_key_provider('${key}-2', 'openbao');"`} />
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-snug text-muted">
          WAL encryption is <span className="font-medium">not</span> on. It is a separate switch
          (<span className="font-mono">ALTER SYSTEM SET pg_tde.wal_encrypt = on</span>) and Percona supports it only
          with <span className="font-mono">pg_tde_archive_decrypt</span> in{' '}
          <span className="font-mono">archive_command</span> and{' '}
          <span className="font-mono">pg_tde_restore_encrypt</span> in{' '}
          <span className="font-mono">restore_command</span> — which this node's pgBackRest archiving does not use.
          Table data is encrypted either way.
        </div>
      </div>
    )
  }

  // ps — Percona Server for MySQL
  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        This node's keyring is OpenBao: {plugin
          ? <>the <span className="font-mono">keyring_vault</span> plugin (loaded with <span className="font-mono">early-plugin-load</span>, since the keyring component only exists from Percona Server 8.4)</>
          : <>the <span className="font-mono">component_keyring_vault</span> component, declared by the global manifest next to <span className="font-mono">mysqld</span></>}.
        Master keys live in OpenBao — MySQL will not open an encrypted tablespace while it is sealed or unreachable.
      </div>
      {rows}
      <Code label="Confirm the keyring is loaded" text={plugin
        ? `mysql -e "SELECT PLUGIN_NAME, PLUGIN_STATUS FROM information_schema.plugins WHERE PLUGIN_NAME='keyring_vault'"`
        : `mysql -e "SELECT * FROM performance_schema.keyring_component_status"`} />
      <Code label="Encrypt a table (and check)" text={`mysql -e "CREATE DATABASE IF NOT EXISTS enc; CREATE TABLE enc.t (id INT PRIMARY KEY) ENCRYPTION='Y';"
mysql -e "SELECT NAME, ENCRYPTION FROM information_schema.innodb_tablespaces WHERE NAME LIKE 'enc/%'"`} />
      <Code label="Encrypt everything new by default" text={`mysql -e "SET PERSIST default_table_encryption=ON;"`} />
      <Code label="Rotate the master key" text={`mysql -e "ALTER INSTANCE ROTATE INNODB MASTER KEY;"`} />
    </div>
  )
}
