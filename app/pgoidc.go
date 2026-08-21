package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// pgoidc.go — Keycloak OIDC login for the standalone PostgreSQL node using PostgreSQL 18's
// native OAuth (`pg_hba` `oauth` method) validated by the `pg_oidc_validator` extension.
// Mirrors applyDirectoryAuth (dbauth.go): auto-configured at deploy, gated by EnableOIDC.
//
// Recipe (validated live, IMPLEMENTATION §104): install percona-pg_oidc_validator18 +
// percona-postgresql18-libs-oauth (the latter via rpm --nodeps — Percona's package has an
// epoch/arch-qualifier dependency bug); set oauth_validator_libraries=pg_oidc_validator and
// pg_oidc_validator.authn_field=preferred_username; add a pg_hba `oauth` line (issuer =
// the Keycloak realm) before the scram catch-all; trust the Intranet CA so the validator can
// fetch the issuer's JWKS over HTTPS. psql logs in with the OAuth 2.0 device flow.

// oidcInfo is persisted into a node's Deployment.Config (key "oidc") so the manager's
// Keycloak-SSO tab can render exact instructions.
type oidcInfo struct {
	Enabled  bool   `json:"enabled"`
	Realm    string `json:"realm"`
	Issuer   string `json:"issuer"`
	ClientID string `json:"clientId"`
	NodeFQDN string `json:"nodeFqdn,omitempty"` // pg, ps
	LoginURL string `json:"loginUrl,omitempty"` // pmm
	// Where the Keycloak console itself lives, so the panel can point at it rather than
	// leaving the reader to work it out from the issuer.
	ConsoleURL string `json:"consoleUrl,omitempty"`
	// Percona Server only: what applyMySQLOIDC actually created on the node, so the
	// login guide can name real accounts instead of a placeholder.
	Users    []string `json:"users,omitempty"`    // MySQL accounts bound to a Keycloak user
	Group    string   `json:"group,omitempty"`    // Keycloak group those users belong to
	Role     string   `json:"role,omitempty"`     // MySQL role that group grants
	Database string   `json:"database,omitempty"` // schema the role can read
}

// oidcIssues validates a pmm/pg/ps node's Keycloak-SSO selection: a linked SSL-enabled
// Keycloak (HTTPS issuer), plus the per-engine version each validator needs — PostgreSQL 18
// for pg (pg_oidc_validator) and Percona Server 8.4.11-11 for ps (auth_openid_connect). For
// pg there is one more rule: no LDAP alongside OIDC, because both claim the same
// `host all all` pg_hba catch-all and the first matching line wins, so the two can never both
// be live. (Kerberos is fine: it matches on a separate `hostgssenc` line. And MySQL has no
// such conflict at all — its auth plugin is chosen per account.)
func oidcIssues(n designNode, keycloakIDs, keycloakSSL map[string]bool) []issue {
	if !n.EnableOIDC {
		return nil
	}
	label := nodeKindLabel(n.Type) + " node " + n.Label
	if !keycloakIDs[n.KeycloakNodeID] {
		return []issue{{"error", label + " has Keycloak SSO enabled but is not linked to a Keycloak node — add a Keycloak node and select it"}}
	}
	if !keycloakSSL[n.KeycloakNodeID] {
		return []issue{{"error", label + " uses Keycloak OIDC, which requires an HTTPS issuer — enable \"Use Intranet CA SSL\" on the Keycloak node"}}
	}
	switch n.Type {
	case "pg":
		if ppgMajorOf(n.PGMajor) != "18" {
			return []issue{{"error", label + " uses Keycloak OIDC (pg_oidc_validator), which requires PostgreSQL 18 — set the version to 18"}}
		}
		if n.LdapAuth {
			return []issue{{"error", label + " has both LDAP and Keycloak OIDC enabled — PostgreSQL cannot use both (they compete for the same pg_hba line); turn one off"}}
		}
	case "ps":
		if psMajorOf(n.PSMajor) != "8.4" {
			return []issue{{"error", label + " uses Keycloak OIDC (auth_openid_connect), which Percona ships in the 8.4 LTS series only — 9.7 does not carry the plugin yet; set the Percona Server major to 8.4"}}
		}
		if !psVersionAtLeast(n.PSVersion, mysqlOIDCMinVersion) {
			return []issue{{"error", label + " is pinned to Percona Server " + n.PSVersion + ", which predates auth_openid_connect — choose " + mysqlOIDCMinVersion + " or newer, or leave the minor version at \"latest\""}}
		}
	}
	return nil
}

func oidcRealmOr(n designNode) string {
	if r := strings.TrimSpace(n.OIDCRealm); r != "" {
		return r
	}
	return "dbcanvas"
}

// persistConfigKey merges key→val into a node's Deployment.Config without disturbing the
// engine's own config keys.
func (a *App) persistConfigKey(st Stack, nodeID, key string, val any) {
	dep, err := a.store.GetDeployment(st.ID, nodeID)
	if err != nil {
		return
	}
	m := map[string]any{}
	if len(dep.Config) > 0 {
		json.Unmarshal(dep.Config, &m)
	}
	m[key] = val
	if b, e := json.Marshal(m); e == nil {
		dep.Config = b
		a.store.UpsertDeployment(dep)
	}
}

// persistSecretKey is persistConfigKey for a node's Deployment.Secrets — it merges key→val
// into the stored JSON without going through the engine's own secrets struct. Used where a
// secret belongs to one node type but the struct is shared: pxcSecrets covers the whole MySQL
// family (PXC, replication, MariaDB, MySQL CE), and none of them but the standalone Percona
// Server node has anything to do with Keycloak.
func (a *App) persistSecretKey(st Stack, nodeID, key string, val any) {
	dep, err := a.store.GetDeployment(st.ID, nodeID)
	if err != nil {
		return
	}
	m := map[string]any{}
	if len(dep.Secrets) > 0 {
		json.Unmarshal(dep.Secrets, &m)
	}
	m[key] = val
	if b, e := json.Marshal(m); e == nil {
		dep.Secrets = b
		a.store.UpsertDeployment(dep)
	}
}

// stageIntranetCA copies the Intranet CA cert into a container at /tmp/dbca-ca.crt so a script
// can add it to the trust store. No-op (nil) if there's no Intranet.
func (a *App) stageIntranetCA(ctx context.Context, st Stack, containerID string) error {
	intranetID := a.intranetContainerID(ctx, st)
	if intranetID == "" {
		return fmt.Errorf("no Intranet node")
	}
	if err := a.waitIntranetCAReady(ctx, intranetID, 120e9); err != nil {
		return err
	}
	ca, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil {
		return err
	}
	return a.engCtx(ctx).PutArchive(ctx, containerID, "/tmp", tarFiles(map[string]fileEntry{"dbca-ca.crt": {0o644, 0, ca}}))
}

// applyPGOIDC configures PostgreSQL 18 on containerID to accept Keycloak OAuth logins.
func (a *App) applyPGOIDC(ctx context.Context, st Stack, n designNode, doc designDoc, containerID, service string, pr *pxcProg) error {
	pr.phase("Configuring Keycloak OIDC (pg_oidc_validator)", 96)
	host, ssl, kcID, adminPW, ok := a.waitKeycloak(ctx, st.ID, n.KeycloakNodeID, deployTimeout())
	if !ok {
		return fmt.Errorf("Keycloak node did not become ready")
	}
	if !ssl {
		return fmt.Errorf("Keycloak must have SSL enabled for an HTTPS issuer")
	}
	domain := envOr("DOMAIN", "example.net")
	realm := oidcRealmOr(n)
	issuer := keycloakIssuer(host, ssl) + "/realms/" + realm
	clientID := "postgres"
	samplePW := keycloakUserPassword()
	if _, err := a.ensureKeycloakClient(ctx, kcID, adminPW, kcClientSpec{
		Realm: realm, ClientID: clientID, Public: true, DeviceFlow: true, Domain: domain, SamplePW: samplePW,
		Users: []kcSampleUser{{"jane", "Jane", "Doe", ""}, {"john", "John", "Doe", ""}},
	}); err != nil {
		return fmt.Errorf("keycloak client: %w", err)
	}
	if err := a.stageIntranetCA(ctx, st, containerID); err != nil {
		return fmt.Errorf("stage CA: %w", err)
	}
	env := []string{"ISSUER=" + issuer, "SERVICE=" + service}
	if err := a.runStep(ctx, containerID, pgOIDCScript, env, pr.logln); err != nil {
		return err
	}
	nodeFQDN := fqdnOf(stackHostnames(doc)[n.ID], domain)
	a.persistConfigKey(st, n.ID, "oidc", oidcInfo{Enabled: true, Realm: realm, Issuer: issuer, ClientID: clientID, NodeFQDN: nodeFQDN})
	pr.logln("Keycloak OAuth login configured (issuer " + issuer + ")")
	return nil
}

// pgOIDCScript installs the validator + client OAuth libs, trusts the Intranet CA, and wires
// oauth into postgresql.conf + pg_hba. Env: ISSUER, SERVICE.
const pgOIDCScript = `set -e
percona-release setup -y ppg-18 >/dev/null 2>&1 || true
dnf -y -q install percona-pg_oidc_validator18 >/dev/null 2>&1
# client OAuth flow module: Percona's package has a broken (epoch/arch-qualified) dep, so
# download + rpm --nodeps.
if ! rpm -q percona-postgresql18-libs-oauth >/dev/null 2>&1; then
  ( cd /tmp && dnf -y -q download percona-postgresql18-libs-oauth >/dev/null 2>&1 && rpm -Uvh --nodeps /tmp/percona-postgresql18-libs-oauth*.rpm >/dev/null 2>&1 ) || true
fi
if [ -f /tmp/dbca-ca.crt ]; then cp -f /tmp/dbca-ca.crt /etc/pki/ca-trust/source/anchors/dbcanvas-ca.crt; update-ca-trust >/dev/null 2>&1 || true; fi
DATA=$(dirname "$(find /var/lib/pgsql /var/lib/postgresql -name pg_hba.conf 2>/dev/null | head -1)")
[ -n "$DATA" ] || { echo "pg_hba.conf not found"; exit 1; }
HBA="$DATA/pg_hba.conf"; CONF="$DATA/postgresql.conf"
sed -i "/# dbcanvas-oidc/d" "$HBA" "$CONF"
cat >> "$CONF" <<EOF
oauth_validator_libraries = 'pg_oidc_validator' # dbcanvas-oidc
pg_oidc_validator.authn_field = 'preferred_username' # dbcanvas-oidc
EOF
SU="host all postgres 0.0.0.0/0 scram-sha-256 # dbcanvas-oidc"
OAUTH="host all all 0.0.0.0/0 oauth scope=\"openid\",issuer=$ISSUER # dbcanvas-oidc"
SU="$SU" OAUTH="$OAUTH" python3 - "$HBA" <<'PY'
import os,re,sys
hba=sys.argv[1]; add=[os.environ["SU"],os.environ["OAUTH"]]
lines=open(hba).read().splitlines(); out=[]; done=False
for ln in lines:
    if not done and re.match(r"host\s+all\s+all\s+0\.0\.0\.0/0\s+scram-sha-256\s*$", ln):
        out+=add; done=True
    out.append(ln)
if not done: out=add+out
open(hba,"w").write("\n".join(out)+"\n")
PY
# oauth_validator_libraries loads at startup → restart (not just reload).
systemctl restart "$SERVICE" >/dev/null 2>&1 || su - postgres -c "pg_ctl -D $DATA restart" >/dev/null 2>&1 || true
for i in $(seq 1 20); do su - postgres -c "psql -tAc 'select 1'" >/dev/null 2>&1 && break; sleep 1; done
echo "pg_oidc_validator configured (issuer $ISSUER)"`
