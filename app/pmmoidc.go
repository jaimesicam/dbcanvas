package main

import (
	"context"
	"fmt"
)

// pmmoidc.go — Keycloak single sign-on for PMM. PMM is Grafana-based, so this creates a
// confidential Keycloak client and configures Grafana's generic OAuth
// ([auth.generic_oauth] in grafana.ini), mapping a "groups" claim to Grafana roles
// (pmm-admins → Admin, else Viewer). Auto-configured at deploy, gated by EnableOIDC.
//
// Recipe (validated live, IMPLEMENTATION §104): grafana.ini [auth.generic_oauth] +
// [server] root_url = https://<pmm-fqdn>:8443/graph/ (so the OAuth redirect matches the
// registered redirect URI); restart grafana. tls_skip_verify_insecure is used because the
// PMM image's /etc/pki is read-only for the runtime user, so the Intranet CA can't be added.

// pmmConfigureOIDC creates the Keycloak client + groups and wires Grafana generic OAuth.
func (a *App) pmmConfigureOIDC(ctx context.Context, st Stack, n designNode, doc designDoc, containerID string, setPhase func(string, int), logln func(string)) error {
	setPhase("Configuring Keycloak SSO (Grafana OAuth)", 92)
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
	pmmFQDN := fqdnOf(stackHostnames(doc)[n.ID], domain)
	rootURL := fmt.Sprintf("https://%s:8443/graph/", pmmFQDN)
	redirect := rootURL + "login/generic_oauth"
	clientID := "pmm"

	secret, err := a.ensureKeycloakClient(ctx, kcID, adminPW, kcClientSpec{
		Realm: realm, ClientID: clientID, Public: false, StdFlow: true, Redirect: []string{redirect},
		GroupsClaim: true, Groups: []string{"pmm-admins", "pmm-viewers"},
		Users:    []kcSampleUser{{"alice", "Alice", "Admin", "pmm-admins"}, {"bob", "Bob", "Viewer", "pmm-viewers"}},
		Domain:   domain,
		SamplePW: keycloakUserPassword(),
	})
	if err != nil {
		return fmt.Errorf("keycloak client: %w", err)
	}
	env := []string{"ISSUER=" + issuer, "CLIENT_ID=" + clientID, "SECRET=" + secret, "ROOT_URL=" + rootURL}
	if err := a.runStep(ctx, containerID, pmmOIDCScript, env, logln); err != nil {
		return err
	}
	a.persistConfigKey(st, n.ID, "oidc", oidcInfo{Enabled: true, Realm: realm, Issuer: issuer, ClientID: clientID, LoginURL: rootURL + "login"})
	logln("Grafana generic OAuth configured (Sign in with Keycloak at " + rootURL + ")")
	return nil
}

// pmmOIDCScript rewrites grafana.ini's [auth.generic_oauth] section + root_url and restarts
// Grafana. Env: ISSUER, CLIENT_ID, SECRET, ROOT_URL.
const pmmOIDCScript = `set -e
INI=/etc/grafana/grafana.ini
[ -f "$INI" ] || { echo "grafana.ini not found"; exit 1; }
awk '
  /^[[:space:]]*\[auth\.generic_oauth\]/ { skip=1; next }
  /^[[:space:]]*\[/                      { skip=0 }
  { if (!skip) print }
' "$INI" > "$INI.tmp"
sed -i "s#^root_url = .*#root_url = $ROOT_URL#" "$INI.tmp"
cat >> "$INI.tmp" <<EOF

[auth.generic_oauth]
enabled = true
name = Keycloak
client_id = $CLIENT_ID
client_secret = $SECRET
# NB: do NOT request a "groups" scope — Keycloak validates requested scopes against the
# client's assigned client scopes and returns invalid_scope ("Login provider denied login
# request") for it. The groups claim is supplied by a client-level mapper (below), so it's
# present in the token/userinfo without being requested as a scope.
scopes = openid profile email
auth_url = $ISSUER/protocol/openid-connect/auth
token_url = $ISSUER/protocol/openid-connect/token
api_url = $ISSUER/protocol/openid-connect/userinfo
role_attribute_path = contains(groups[*], 'pmm-admins') && 'Admin' || 'Viewer'
allow_assign_grafana_admin = true
allow_sign_up = true
use_pkce = true
tls_skip_verify_insecure = true
EOF
mv "$INI.tmp" "$INI"
supervisorctl restart grafana >/dev/null 2>&1 || true
echo "grafana generic_oauth configured (root_url $ROOT_URL)"`
