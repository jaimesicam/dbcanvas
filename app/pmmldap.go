package main

import (
	"context"
	"fmt"
)

// pmmldap.go — LDAP authentication for PMM. PMM is Grafana-based, so this points Grafana's
// [auth.ldap] at the stack directory the user chose (Intranet OpenLDAP or Samba AD DC),
// writing /etc/grafana/ldap.toml. Auto-configured at deploy, gated by LdapAuth. Reuses the
// DB-node directory resolver (resolveDirectory) so Intranet vs Samba params (host, bind DN,
// base DN, user attribute) are derived in one place.

type pmmLDAPInfo struct {
	Enabled  bool   `json:"enabled"`
	DirType  string `json:"dirType"` // intranet | sambaad
	DirFQDN  string `json:"dirFQDN"`
	UserAttr string `json:"userAttr"` // uid | sAMAccountName
	LoginURL string `json:"loginUrl"`
}

// pmmConfigureLDAP writes ldap.toml + enables [auth.ldap] in grafana.ini and restarts Grafana.
func (a *App) pmmConfigureLDAP(ctx context.Context, st Stack, n designNode, doc designDoc, containerID string, setPhase func(string, int), logln func(string)) error {
	setPhase("Configuring LDAP authentication (Grafana)", 90)
	dir, err := a.resolveDirectory(ctx, st, doc, n.LdapDirNodeID)
	if err != nil {
		return fmt.Errorf("directory: %w", err)
	}
	domain := envOr("DOMAIN", "example.net")
	pmmFQDN := fqdnOf(stackHostnames(doc)[n.ID], domain)

	// Grafana LDAP config. Plain ldap:// (389) — Intranet OpenLDAP and Samba (strong-auth off)
	// both allow it. Grafana binds as the service account, searches for the user, then binds as
	// the user. Everyone authenticated gets the Editor org role (group_dn "*"); the built-in
	// admin account still manages the server. Refine group_mappings in ldap.toml if needed.
	toml := fmt.Sprintf(`[[servers]]
host = %q
port = 389
use_ssl = false
start_tls = false
ssl_skip_verify = true
bind_dn = %q
bind_password = %q
search_filter = "(%s=%%s)"
search_base_dns = [%q]

[servers.attributes]
name = "givenName"
surname = "sn"
username = %q
member_of = "memberOf"
email = "mail"

[[servers.group_mappings]]
group_dn = "*"
org_role = "Editor"
`, dir.FQDN, dir.BindDN, dir.BindPW, dir.UserAttr, dir.BaseDN, dir.UserAttr)

	if err := a.docker.CopyFile(ctx, containerID, "/etc/grafana", "ldap.toml", 0o644, []byte(toml)); err != nil {
		return fmt.Errorf("write ldap.toml: %w", err)
	}
	if err := a.runStep(ctx, containerID, pmmLDAPScript, nil, logln); err != nil {
		return err
	}
	a.persistConfigKey(st, n.ID, "ldap", pmmLDAPInfo{
		Enabled: true, DirType: dir.Type, DirFQDN: dir.FQDN, UserAttr: dir.UserAttr,
		LoginURL: fmt.Sprintf("https://%s:8443/graph/login", pmmFQDN),
	})
	logln("Grafana LDAP configured against " + dir.FQDN + " (sign in with a directory username)")
	return nil
}

// pmmLDAPScript enables [auth.ldap] in grafana.ini (pointing at the ldap.toml written above)
// and restarts Grafana.
const pmmLDAPScript = `set -e
INI=/etc/grafana/grafana.ini
[ -f "$INI" ] || { echo "grafana.ini not found"; exit 1; }
awk '
  /^[[:space:]]*\[auth\.ldap\]/ { skip=1; next }
  /^[[:space:]]*\[/             { skip=0 }
  { if (!skip) print }
' "$INI" > "$INI.tmp"
cat >> "$INI.tmp" <<EOF

[auth.ldap]
enabled = true
config_file = /etc/grafana/ldap.toml
allow_sign_up = true
EOF
mv "$INI.tmp" "$INI"
supervisorctl restart grafana >/dev/null 2>&1 || true
echo "grafana LDAP enabled"`
