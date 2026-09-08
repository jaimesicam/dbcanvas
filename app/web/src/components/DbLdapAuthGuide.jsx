import { useState } from 'react'
import { Icon } from './Icons.jsx'

// DbLdapAuthGuide — a copy-paste how-to for pointing MongoDB, Percona Server and PostgreSQL
// at this directory for authentication. Shared by the Intranet (OpenLDAP) and Samba AD DC
// managers. Props: fqdn, baseDN, bindDN; `kind` ∈ {'ldap','ad'} picks the user attribute
// (uid vs sAMAccountName) and whether the Kerberos/GSSAPI section is shown.

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
      <pre className="max-h-72 overflow-auto whitespace-pre rounded-lg border bg-bg p-2 font-mono text-[11px] leading-relaxed text-fg">{text}</pre>
    </div>
  )
}

export default function DbLdapAuthGuide({ fqdn, baseDN, bindDN, kind = 'ldap' }) {
  const ad = kind === 'ad'
  const userAttr = ad ? 'sAMAccountName' : 'uid'
  const dir = ad ? 'Active Directory' : 'OpenLDAP'
  const bpw = '<bind password>' // from the Credentials tab

  const pg = `# PostgreSQL — pg_hba.conf (search+bind), then reload:
host  all  all  0.0.0.0/0  ldap ldapserver=${fqdn} ldapbasedn="${baseDN}" \\
  ldapbinddn="${bindDN}" ldapbindpasswd="${bpw}" ldapsearchattribute=${userAttr}
# then: SELECT pg_reload_conf();  and create matching roles:
CREATE ROLE dbuser1 LOGIN;   -- password authenticated by ${dir}`

  const ps = `-- Percona Server (MySQL) — LDAP simple auth:
INSTALL PLUGIN authentication_ldap_simple SONAME 'authentication_ldap_simple.so';
SET PERSIST authentication_ldap_simple_server_host = '${fqdn}';
SET PERSIST authentication_ldap_simple_bind_base_dn = '${baseDN}';
SET PERSIST authentication_ldap_simple_user_search_attr = '${userAttr}';
SET PERSIST authentication_ldap_simple_bind_root_dn = '${bindDN}';
SET PERSIST authentication_ldap_simple_bind_root_pwd = '${bpw}';
CREATE USER 'dbuser1' IDENTIFIED WITH authentication_ldap_simple;`

  const mongo = `# MongoDB (PSMDB) — mongod.conf, then restart:
security:
  authorization: enabled
  ldap:
    servers: "${fqdn}"
    bind:
      method: simple
      queryUser: "${bindDN}"
      queryPassword: "${bpw}"
    userToDNMapping: '[{ match: "(.+)", ldapQuery: "${baseDN}??sub?(${userAttr}={0})" }]'
    authz:
      queryTemplate: "${baseDN}??sub?(member={USER})"
setParameter:
  authenticationMechanisms: PLAIN
# then create an external user:
#   db.getSiblingDB("$external").createUser({ user: "dbuser1", roles: [ ... ] })`

  const gss = `# Kerberos / GSSAPI (single sign-on) — use a keytab from the Kerberos tab:
#   1) Download krb5.conf → /etc/krb5.conf on the DB node.
#   2) Create the service principal (postgres/<fqdn> or mongodb/<fqdn>) and download its keytab.
# PostgreSQL:  pg_hba.conf →  host all all 0.0.0.0/0 gss include_realm=0
#              postgresql.conf → krb_server_keyfile = '/etc/postgresql/krb5.keytab'
# MongoDB:     mongod --setParameter authenticationMechanisms=GSSAPI
#              export KRB5_KTNAME=/etc/mongodb.keytab   (principal mongodb/<fqdn>)`

  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        Point a database at this {dir} directory for authentication. Directory:
        <span className="font-mono"> {fqdn}</span> · base DN <span className="font-mono">{baseDN}</span> · bind DN
        <span className="font-mono"> {bindDN}</span> (bind password is on the Credentials tab).
        {ad ? ' Plain ldap:// binds are enabled; use LDAPS if you generated a cert.' : ''}
      </div>
      <Code label="PostgreSQL (pg_hba LDAP)" text={pg} />
      <Code label="Percona Server (authentication_ldap_simple)" text={ps} />
      <Code label="MongoDB / PSMDB (LDAP)" text={mongo} />
      {ad && <Code label="Kerberos / GSSAPI (keytab-based SSO)" text={gss} />}
    </div>
  )
}
