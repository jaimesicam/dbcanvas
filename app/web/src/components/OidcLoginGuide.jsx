import { useState } from 'react'
import { Icon } from './Icons.jsx'
import { SecretValue } from './Secret.jsx'

// OidcLoginGuide — shown on a deployed PMM, PostgreSQL or Percona Server node configured for
// Keycloak SSO. The server is already configured; this explains how to sign in. Driven by
// dep.config.oidc { issuer, consoleUrl, clientId, realm, nodeFqdn, loginUrl, users, group,
// role, database }; `engine` ∈ {pmm, pg, ps}. `secrets` is the deployment's secrets, read by
// the ps branch for the sample users' password.
//
// The ps branch follows the PSMDB tab (MongoDBManager's KeycloakSSOTab): state the facts —
// where Keycloak is, which accounts exist, what their password actually is — before any
// command. A command whose inputs the reader cannot see reads as invented.

function CopyButton({ text }) {
  const [done, setDone] = useState(false)
  return (
    <button title="Copy" onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}
      className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
      {done ? <Icon.Check size={14} /> : <Icon.Copy size={14} />}
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

export default function OidcLoginGuide({ engine, info, secrets }) {
  if (!info || !info.enabled) return null

  // No sign-in link here: the OAuth round-trip only completes in a browser that can resolve
  // both PMM's and Keycloak's stack FQDNs — i.e. one running inside the stack network, not the
  // host browser showing this page. Point the operator at the VNC desktop node instead.
  if (engine === 'pmm') {
    return (
      <div className="space-y-3">
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          PMM authenticates against Keycloak (realm <span className="font-mono">{info.realm}</span>).
          Open PMM at <span className="font-mono">{info.loginUrl}</span> and click
          <span className="font-medium"> “Sign in with Keycloak”</span>. Users in the
          <span className="font-mono"> pmm-admins</span> group get the Grafana <b>Admin</b> role; everyone
          else is <b>Viewer</b>. Manage users/groups on the Keycloak node.
        </div>
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-snug text-muted">
          <span className="font-medium text-fg">Keycloak sign-in needs a browser inside the stack.</span> The
          OAuth redirect goes to Keycloak's stack FQDN, which only the stack's Intranet DNS resolves —
          your host browser cannot complete it. Add an <span className="font-medium">Ubuntu VNC</span> node
          to the stack, open its desktop, and browse to <span className="font-mono">{info.loginUrl}</span> from
          there. (Reaching PMM on its published host port shows the login page, but the Keycloak
          round-trip still fails.)
        </div>
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] text-muted">
          Sample Keycloak users (password: <span className="font-mono">KEYCLOAK_USER_PASSWORD</span> from <span className="font-mono">.env</span>):
          <span className="font-mono"> alice</span> (Admin) · <span className="font-mono">bob</span> (Viewer).
          The built-in <span className="font-mono">admin</span> account still logs in directly.
        </div>
      </div>
    )
  }

  // ps — Percona Server 8.4's auth_openid_connect. Unlike psql's device flow, the MySQL
  // client takes an ID token from a file, so the round-trip is: ask Keycloak for a token,
  // then hand mysql the file. The node's oidc-login helper does both in one command.
  if (engine === 'ps') {
    const users = info.users?.length ? info.users : []
    const who = users[0] || 'jane'
    const tokenURL = `${info.issuer}/protocol/openid-connect/token`
    const manual = `# 1. ask Keycloak for an ID token (not the access token)
curl -s -X POST ${tokenURL} \\
  -d grant_type=password -d client_id=${info.clientId} -d scope=openid \\
  -d username=${who} -d password='<the password above>' \\
  | jq -r .id_token > /root/id_token.jwt && chmod 600 /root/id_token.jwt

# 2. log in with it — no MySQL password is ever sent
mysql --user=${who} --authentication-openid-connect-client-id-token-file=/root/id_token.jwt`
    const roleCmd = `SET ROLE ${info.role};                 -- the group-mapped role is granted, not activated
SELECT CURRENT_USER(), CURRENT_ROLE();
SELECT * FROM ${info.database}.invoices;`
    const addUser = `-- 'sub' is the Keycloak user id, which the console shows on the user's page:
CREATE USER 'carol'@'%' IDENTIFIED WITH 'auth_openid_connect'
  AS '{"identity_provider": "keycloak", "user": "<that user's id>"}';`
    return (
      <div className="space-y-3">
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          This node accepts signed ID tokens from Keycloak through the
          <span className="font-mono"> auth_openid_connect</span> plugin. The accounts below already exist
          in MySQL, each bound to a Keycloak user — no MySQL password is involved. Manage users and
          groups on the Keycloak node.
        </div>

        <div className="space-y-2 text-sm">
          <KV k="Keycloak" v={info.consoleUrl} mono />
          <KV k="Issuer" v={info.issuer} mono />
          <KV k="Client ID" v={info.clientId} mono />
          <KV k="Realm" v={info.realm} />
          <KV k="MySQL accounts" v={users.join(', ')} mono />
          <KV k="Authorization" v={`group ${info.group} → role ${info.role} (SELECT on ${info.database})`} />
        </div>

        {secrets?.oidcSamplePassword && (
          <div>
            <div className="text-xs text-muted">Password for those Keycloak users</div>
            <SecretValue value={secrets.oidcSamplePassword} />
          </div>
        )}

        <div className="space-y-1">
          <div className="text-[11px] text-muted">
            DBCanvas writes a small shell wrapper to <span className="font-mono">/usr/local/bin/oidc-login</span> on
            this node at deploy — it is not an upstream tool, just the two steps below in one command. Run it
            from the node's root console; it prompts for the Keycloak password above.
          </div>
          <CopyRow label="On the node (over the local socket)" value={`oidc-login ${who}`} />
          <CopyRow label="Over the network (TLS is required)" value={`oidc-login ${who} --host=${info.nodeFqdn} --protocol=TCP --ssl-mode=REQUIRED`} />
        </div>

        <Code label="The same thing by hand — what oidc-login runs for you" text={manual} />
        <Code label="Then, in the session" text={roleCmd} />

        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          The connection must be encrypted: a Unix socket is fine, and over TCP you need
          <span className="font-mono"> --ssl-mode=REQUIRED</span> — the plugin refuses a plaintext link with a
          plain “Access denied”. The token file must be the <span className="font-mono">id_token</span>, not the
          access token.
        </div>
        <Code label="Add your own Keycloak user" text={addUser} />
      </div>
    )
  }

  // pg
  const u = 'jane' // sample directory user (password: KEYCLOAK_USER_PASSWORD in .env)
  const roleCmd = `sudo -u postgres psql -c 'CREATE ROLE ${u} LOGIN;'   # role name = Keycloak username`
  const clientPkg = `# one-time on the client running psql (Oracle Linux / RHEL):
sudo percona-release setup ppg-18
sudo dnf install percona-postgresql18   # provides psql
# OAuth device flow needs the libpq-oauth module:
sudo dnf download percona-postgresql18-libs-oauth && sudo rpm -Uvh --nodeps percona-postgresql18-libs-oauth*.rpm`
  const loginCmd = `psql "host=${info.nodeFqdn} dbname=postgres user=${u} \\
  oauth_issuer=${info.issuer} oauth_client_id=${info.clientId}"
# psql prints a URL + code — open it, sign in to Keycloak, and psql connects.`
  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        This PostgreSQL node accepts Keycloak OAuth logins (realm <span className="font-mono">{info.realm}</span>,
        validated by <span className="font-mono">pg_oidc_validator</span>). Log in as a Keycloak user with the
        OAuth 2.0 device flow — no password is sent to PostgreSQL. Replace <span className="font-mono">{u}</span> with a
        real Keycloak username; a matching PG role must exist.
      </div>
      <Code label="One-time: create a matching role (run as postgres on this node)" text={roleCmd} />
      <Code label="Client prerequisites (psql + libpq-oauth)" text={clientPkg} />
      <Code label="Log in with Keycloak (device flow)" text={loginCmd} />
    </div>
  )
}
