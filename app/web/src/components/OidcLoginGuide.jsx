import { useState } from 'react'
import { Icon } from './Icons.jsx'

// OidcLoginGuide — shown on a deployed PMM or PostgreSQL node configured for Keycloak SSO.
// The server is already configured; this explains how to sign in. Driven by dep.config.oidc
// { issuer, clientId, realm, nodeFqdn, loginUrl }; `engine` ∈ {pmm, pg}.

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

export default function OidcLoginGuide({ engine, info }) {
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
