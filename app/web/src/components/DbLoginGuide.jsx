import { useState } from 'react'
import { Icon } from './Icons.jsx'

// DbLoginGuide — shown on a deployed DB node that was provisioned with directory authentication.
// The server is already configured; this tab tells the operator how to LOG IN as a directory
// user (and the one-time step to register that user with the engine). Driven by dep.config.dirAuth
// { dirType, dirFQDN, nodeFQDN, realm, kerberos, userAttr }; `engine` ∈ {pg, ps, psm}.

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

export default function DbLoginGuide({ engine, info }) {
  if (!info || !info.enabled) return null
  const dir = info.dirType === 'sambaad' ? 'Samba AD DC' : 'Intranet OpenLDAP'
  const node = info.nodeFQDN
  const realm = info.realm || 'REALM'
  const u = 'alice' // placeholder directory username

  const blocks = []
  if (engine === 'pg') {
    blocks.push({ label: 'One-time: create a matching role (run as postgres on this node)', text:
`sudo -u postgres psql -c 'CREATE ROLE ${u} LOGIN;'   # role name = directory username` })
    blocks.push({ label: 'Log in with an LDAP password', text:
`psql "host=${node} user=${u} dbname=postgres gssencmode=disable"
# prompts for ${u}'s directory password` })
    if (info.kerberos) {
      blocks.push({ label: 'Log in with Kerberos single sign-on', text:
`kinit ${u}@${realm}                       # get a ticket (once per session)
psql "host=${node} user=${u} dbname=postgres gssencmode=require"` })
    }
  } else if (engine === 'ps') {
    blocks.push({ label: "One-time: register the user (run as MySQL root on this node)", text:
`CREATE USER '${u}'@'%' IDENTIFIED WITH authentication_ldap_simple;
GRANT SELECT ON *.* TO '${u}'@'%';   -- grant what you need` })
    blocks.push({ label: 'Log in with an LDAP password (cleartext plugin required)', text:
`mysql -h ${node} -u ${u} -p --enable-cleartext-plugin
# enter ${u}'s directory password at the prompt` })
  } else if (engine === 'psm') {
    blocks.push({ label: "One-time: create the $external user (run as mongo admin)", text:
`db.getSiblingDB("$external").runCommand({
  createUser: "${u}",
  roles: [ { role: "readWriteAnyDatabase", db: "admin" } ]
})` })
    blocks.push({ label: 'Log in with an LDAP password', text:
`mongosh --host ${node} --authenticationMechanism PLAIN \\
  --authenticationDatabase '$external' -u ${u} -p` })
    if (info.kerberos) {
      blocks.push({ label: "One-time: create the $external user named after the Kerberos principal", text:
`db.getSiblingDB("$external").runCommand({
  createUser: "${u}@${realm}",
  roles: [ { role: "readWriteAnyDatabase", db: "admin" } ]
})` })
      blocks.push({ label: 'Log in with Kerberos single sign-on', text:
`kinit ${u}@${realm}
mongosh --host ${node} --authenticationMechanism GSSAPI \\
  --authenticationDatabase '$external' -u '${u}@${realm}'` })
    }
  }

  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        This node authenticates against <span className="font-medium">{dir}</span>
        (<span className="font-mono">{info.dirFQDN}</span>). Create directory users/groups on that
        node's LDAP tab, then log in here with a directory username
        {info.kerberos ? ' — by password (LDAP) or Kerberos ticket (GSSAPI).' : ' and its directory password.'}
        {' '}Replace <span className="font-mono">{u}</span> with a real directory user.
      </div>
      {blocks.map((b, i) => <Code key={i} label={b.label} text={b.text} />)}
    </div>
  )
}
