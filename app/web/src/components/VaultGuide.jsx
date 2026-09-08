import { useState } from 'react'
import { Icon } from './Icons.jsx'

// VaultGuide — the "Encryption" tab of a deployed Percona Server / PSMDB node that was wired to
// an OpenBao node at deploy. The keyring is already configured; this says what was configured
// (so it can be audited) and how to actually use it. Driven by dep.config.vault (vaultInfo in
// app/dbvault.go); `engine` ∈ {ps, psm}.

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
