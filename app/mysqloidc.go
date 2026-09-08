package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// mysqloidc.go — Keycloak OpenID Connect login for the standalone Percona Server node
// (Type=="ps"), via the auth_openid_connect plugin Percona shipped in Percona Server for
// MySQL 8.4.11-11. Mirrors applyPGOIDC (pgoidc.go) and applyDirectoryAuth (dbauth.go):
// auto-configured at deploy, gated by EnableOIDC, and skipped (not fatal) if it fails.
//
// The node ends up with the whole demo already wired: a Keycloak realm + client, sample
// users in a group, MySQL accounts bound to those users' `sub` claims, a schema only the
// group's role can read, and an `oidc-login` helper on the node that does the token dance.
//
// Recipe, validated live against percona-server-server-8.4.11-11.1.el9 + Keycloak 26.5.5:
//
//   - Both libraries — auth_openid_connect.so (server) and
//     authentication_openid_connect_client.so (client) — are inside percona-server-server.
//     There is no separate package, and no client-side install on the node.
//   - The plugin is loaded from /etc/my.cnf with plugin-load-add, NOT with INSTALL PLUGIN:
//     auth_openid_connect_configuration is a plugin variable, so an option file can only
//     carry it if the plugin is already loaded when options are parsed. That also makes the
//     configuration survive a restart without going through mysqld-auto.cnf.
//   - The identity-provider trust document lives in its own file, referenced as
//     FILE:///etc/mysql-oidc-idp.json — inlining it with the JSON:// prefix would mean
//     quoting a JSON document inside a my.cnf value.
//   - The plugin fetches the JWKS over HTTPS using the system trust store, which
//     mysqlPrepareNode has already loaded with the Intranet CA — so an Intranet-CA Keycloak
//     validates with no plugin-specific TLS setting. This is why validation insists on an
//     SSL Keycloak: the issuer has to be HTTPS.
//   - group-role maps a Keycloak group to a MySQL role. ensureKeycloakClient creates the
//     group-membership mapper with full.path=false, so the claim value is "accounting" and
//     the group-role key must be "accounting" — not "/accounting" as in Percona's own
//     quickstart, which assumes a full-path mapper.
//   - A group-mapped role is granted for the life of the connection but NOT activated:
//     SHOW GRANTS lists it while CURRENT_ROLE() stays NONE until SET ROLE. The login guide
//     says so rather than flipping activate_all_roles_on_login, which would change role
//     behaviour for every other account on the node.
//   - Secure transport is mandatory. Over the Unix socket anything works; over TCP,
//     --ssl-mode=DISABLED is refused with a bare "Access denied", so the guide always
//     passes --ssl-mode=REQUIRED.

const (
	// mysqlOIDCMinVersion is the first Percona Server release carrying auth_openid_connect
	// (8.4.11-11, 2026-08-20). The 9.7 series does not have it yet: 9.7.1-1 predates it.
	mysqlOIDCMinVersion = "8.4.11-11"
	mysqlOIDCIDPName    = "keycloak"   // key in auth_openid_connect_configuration
	mysqlOIDCClientID   = "mysql"      // Keycloak client id == the token audience
	mysqlOIDCGroup      = "accounting" // Keycloak group mapped to the MySQL role
	mysqlOIDCRole       = "accounting" // MySQL role that group grants
	mysqlOIDCDatabase   = "oidc_demo"  // schema the role can read
	mysqlOIDCConfigFile = "/etc/mysql-oidc-idp.json"
)

// mysqlOIDCSampleUsers are the Keycloak users created for the demo, all in mysqlOIDCGroup.
// Each gets a matching MySQL account bound to its Keycloak user id (the token's `sub`).
var mysqlOIDCSampleUsers = []kcSampleUser{
	{"jane", "Jane", "Doe", mysqlOIDCGroup},
	{"john", "John", "Doe", mysqlOIDCGroup},
}

// psVersionAtLeast reports whether a catalog version ("8.4.11-11.1") is at least min
// ("8.4.11-11"), comparing numeric components left to right across both . and - separators.
// An empty version means "latest", which is always new enough.
func psVersionAtLeast(v, min string) bool {
	if strings.TrimSpace(v) == "" {
		return true
	}
	got, want := versionFields(v), versionFields(min)
	for i := range want {
		if i >= len(got) {
			return false // fewer components than required, e.g. "8.4" vs "8.4.11-11"
		}
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}

// versionFields splits a Percona version string into its numeric components. Anything
// non-numeric ends the parse, so a version this doesn't understand compares as short.
func versionFields(s string) []int {
	var out []int
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '-' }) {
		n, err := strconv.Atoi(f)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

// mysqlOIDCIDPJSON renders the auth_openid_connect trust document: one identity provider,
// keyed by name, with the issuer to match, where to fetch its signing keys, the audience to
// require, and the group claim → MySQL role mapping.
func mysqlOIDCIDPJSON(issuer, clientID string) string {
	doc := map[string]any{
		mysqlOIDCIDPName: map[string]any{
			"issuer-name": issuer,
			"jwks-url":    issuer + "/protocol/openid-connect/certs",
			"audiences":   []string{clientID},
			"group-claim": "groups",
			"group-role":  []map[string]string{{mysqlOIDCGroup: mysqlOIDCRole}},
		},
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return string(b)
}

// applyMySQLOIDC configures Percona Server on containerID to accept Keycloak ID tokens.
func (a *App) applyMySQLOIDC(ctx context.Context, st Stack, n designNode, doc designDoc, containerID string, sec pxcSecrets, pr *pxcProg) error {
	pr.phase("Configuring Keycloak OIDC (auth_openid_connect)", 96)
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

	// A public client with the resource-owner password grant: the node's oidc-login helper
	// trades a Keycloak username + password for an ID token, which is what Percona's own
	// quickstart does. No browser round-trip, so no redirect URI and no client secret.
	_, userIDs, err := a.ensureKeycloakClientUsers(ctx, kcID, adminPW, kcClientSpec{
		Realm: realm, ClientID: mysqlOIDCClientID, Public: true, DirectGrant: true,
		GroupsClaim: true, Groups: []string{mysqlOIDCGroup},
		Users: mysqlOIDCSampleUsers, Domain: domain, SamplePW: keycloakUserPassword(),
	})
	if err != nil {
		return fmt.Errorf("keycloak client: %w", err)
	}
	// The account is bound to the Keycloak user id, which is the token's `sub` claim —
	// so the MySQL user has to be created after Keycloak has minted the user.
	var pairs, names []string
	for _, u := range mysqlOIDCSampleUsers {
		if id := userIDs[u.Username]; id != "" {
			pairs = append(pairs, u.Username+":"+id)
			names = append(names, u.Username)
		}
	}
	if len(pairs) == 0 {
		return fmt.Errorf("Keycloak returned no user ids — nothing to bind a MySQL account to")
	}

	env := []string{
		"IDP=" + mysqlOIDCIDPName,
		"IDPJSON=" + mysqlOIDCIDPJSON(issuer, mysqlOIDCClientID),
		"CONFFILE=" + mysqlOIDCConfigFile,
		"UNIT=" + mysqlUnit(n.OS),
		"ROOTPW=" + sec.RootPassword,
		"USERS=" + strings.Join(pairs, " "),
		"ROLE=" + mysqlOIDCRole,
		"DB=" + mysqlOIDCDatabase,
		"CLIENT_ID=" + mysqlOIDCClientID,
		"TOKEN_URL=" + issuer + "/protocol/openid-connect/token",
		"MINVER=" + mysqlOIDCMinVersion,
	}
	if err := a.runStep(ctx, containerID, mysqlOIDCScript, env, pr.logln); err != nil {
		return err
	}
	a.persistConfigKey(st, n.ID, "oidc", oidcInfo{
		Enabled: true, Realm: realm, Issuer: issuer, ClientID: mysqlOIDCClientID,
		ConsoleURL: keycloakIssuer(host, ssl),
		NodeFQDN:   fqdnOf(stackHostnames(doc)[n.ID], domain),
		Users:      names, Group: mysqlOIDCGroup, Role: mysqlOIDCRole, Database: mysqlOIDCDatabase,
	})
	// The sample users are useless without their password, so the panel shows it (like the
	// PSMDB node's tab does). It goes in the node's own secrets, not in pxcSecrets — that
	// struct is shared with PXC, which has no OIDC plugin to speak of.
	a.persistSecretKey(st, n.ID, "oidcSamplePassword", keycloakUserPassword())
	pr.logln("auth_openid_connect configured (issuer " + issuer + ", accounts " + strings.Join(names, ", ") + ")")
	return nil
}

// mysqlOIDCScript loads auth_openid_connect from /etc/my.cnf against $CONFFILE, restarts
// mysqld, then creates the demo schema, the group-mapped role and one MySQL account per
// Keycloak user, plus the /usr/local/bin/oidc-login helper.
// Env: IDP, IDPJSON, CONFFILE, UNIT, ROOTPW, USERS ("name:sub …"), ROLE, DB, CLIENT_ID,
// TOKEN_URL, MINVER.
const mysqlOIDCScript = `set -e
PLUG=$(find /usr/lib64/mysql/plugin /usr/lib/mysql/plugin /usr/lib/*/mysql/plugin -name auth_openid_connect.so 2>/dev/null | head -1)
[ -n "$PLUG" ] || { echo "auth_openid_connect.so is not in this build — Percona Server $MINVER or newer is required"; exit 1; }
# curl + jq are only for the oidc-login helper; the server plugin needs neither.
if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl jq >/dev/null 2>&1 || true
  else
    (dnf install -y -q curl jq >/dev/null 2>&1 || microdnf install -y curl jq >/dev/null 2>&1) || true
  fi
fi
printf '%s\n' "$IDPJSON" > "$CONFFILE"
chmod 640 "$CONFFILE"; chown root:mysql "$CONFFILE" 2>/dev/null || true
CNF=/etc/my.cnf
cp -f "$CNF" /tmp/my.cnf.pre-oidc
sed -i "/# dbcanvas-oidc-begin/,/# dbcanvas-oidc-end/d" "$CNF"
cat >> "$CNF" <<EOF

# dbcanvas-oidc-begin
[mysqld]
plugin-load-add=auth_openid_connect.so
auth_openid_connect_configuration=FILE://$CONFFILE
# dbcanvas-oidc-end
EOF
# A bad option file keeps mysqld down, so put the old one back rather than leave a dead node.
if ! systemctl restart "$UNIT"; then
  cp -f /tmp/my.cnf.pre-oidc "$CNF"; systemctl restart "$UNIT" || true
  echo "mysqld refused to start with the OpenID Connect options — reverted /etc/my.cnf"; exit 1
fi
for i in $(seq 1 40); do mysql -uroot -p"$ROOTPW" -e "SELECT 1" >/dev/null 2>&1 && break; sleep 2; done
STATUS=$(mysql -uroot -p"$ROOTPW" -N -B -e "SELECT PLUGIN_STATUS FROM INFORMATION_SCHEMA.PLUGINS WHERE PLUGIN_NAME='auth_openid_connect'" 2>/dev/null)
if [ "$STATUS" != "ACTIVE" ]; then
  cp -f /tmp/my.cnf.pre-oidc "$CNF"; systemctl restart "$UNIT" || true
  echo "auth_openid_connect did not load — reverted /etc/my.cnf"; exit 1
fi
rm -f /tmp/my.cnf.pre-oidc
{
  echo "CREATE DATABASE IF NOT EXISTS $DB;"
  echo "CREATE TABLE IF NOT EXISTS $DB.invoices (id INT PRIMARY KEY, amount DECIMAL(10,2));"
  echo "INSERT IGNORE INTO $DB.invoices VALUES (1,99.95),(2,145.00),(3,12.50);"
  echo "CREATE ROLE IF NOT EXISTS '$ROLE';"
  echo "GRANT SELECT ON $DB.* TO '$ROLE';"
  for spec in $USERS; do
    U=${spec%%:*}; SUB=${spec#*:}
    echo "DROP USER IF EXISTS '$U'@'%';"
    echo "CREATE USER '$U'@'%' IDENTIFIED WITH 'auth_openid_connect' AS '{\"identity_provider\": \"$IDP\", \"user\": \"$SUB\"}';"
  done
} > /tmp/dbcanvas-oidc.sql
mysql -uroot -p"$ROOTPW" < /tmp/dbcanvas-oidc.sql
rm -f /tmp/dbcanvas-oidc.sql
cat > /usr/local/bin/oidc-login <<EOS
#!/bin/sh
# Written by DBCanvas. Trades a Keycloak password for an ID token, then logs into MySQL with it.
TOKEN_URL="$TOKEN_URL"
CLIENT_ID="$CLIENT_ID"
EOS
cat >> /usr/local/bin/oidc-login <<'EOS'
set -e
[ $# -ge 1 ] || { echo "usage: oidc-login <keycloak-user> [mysql args...]" >&2; exit 2; }
U=$1; shift
PW=$OIDC_PASSWORD
if [ -z "$PW" ]; then
  printf 'Keycloak password for %s: ' "$U" >&2
  stty -echo 2>/dev/null || true; read -r PW; stty echo 2>/dev/null || true; echo >&2
fi
TOK=$(mktemp /tmp/id_token.XXXXXX); chmod 600 "$TOK"
trap 'rm -f "$TOK"' EXIT INT TERM
curl -sS -X POST "$TOKEN_URL" -d grant_type=password -d client_id="$CLIENT_ID" -d scope=openid \
  -d username="$U" --data-urlencode "password=$PW" | jq -r '.id_token // empty' > "$TOK"
[ -s "$TOK" ] || { echo "Keycloak issued no ID token for $U (wrong password, or the user is disabled)" >&2; exit 1; }
mysql --user="$U" --authentication-openid-connect-client-id-token-file="$TOK" "$@"
EOS
chmod 755 /usr/local/bin/oidc-login
echo "auth_openid_connect ACTIVE; accounts created; oidc-login installed"`
