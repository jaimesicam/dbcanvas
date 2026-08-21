package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// keycloakclient.go — a reusable helper to (idempotently) ensure a realm + OIDC client on a
// running Keycloak node via kcadm, generalising the PSMDB-specific keycloakSetupScript
// (mongodb.go). Used by the PMM (confidential + redirect + group→role) and PostgreSQL
// (public + device-authorization) Keycloak integrations.

type kcSampleUser struct {
	Username, First, Last, Group string
}

// keycloakUserPassword is the password set on every sample Keycloak user created by the
// SSO integrations (PostgreSQL, MongoDB standalone, PMM). Read from .env on each deploy,
// like the other node credentials — it used to be a random per-deploy secret that was
// never surfaced, so nobody could actually sign in as those users.
func keycloakUserPassword() string {
	return envOr("KEYCLOAK_USER_PASSWORD", "keycloak_user_password")
}

type kcClientSpec struct {
	Realm       string
	ClientID    string
	Public      bool           // public client (no secret) vs confidential
	StdFlow     bool           // authorization-code flow (PMM); false for device-only
	DeviceFlow  bool           // OAuth 2.0 device authorization grant (psql/libpq)
	DirectGrant bool           // resource-owner password grant (Percona Server's ID-token fetch)
	Redirect    []string       // redirect URIs (confidential/standard flow)
	GroupsClaim bool           // add a "groups" group-membership mapper
	Groups      []string       // groups to ensure
	Users       []kcSampleUser // sample users to create (+ optional group membership)
	Domain      string         // email domain for sample users
	SamplePW    string         // sample-user password
}

// ensureKeycloakClient runs kcadm inside the Keycloak container and returns the client secret
// (empty for a public client).
func (a *App) ensureKeycloakClient(ctx context.Context, kcContainerID, adminPW string, spec kcClientSpec) (string, error) {
	secret, _, err := a.ensureKeycloakClientUsers(ctx, kcContainerID, adminPW, spec)
	return secret, err
}

// ensureKeycloakClientUsers is ensureKeycloakClient plus the Keycloak user id of every sample
// user, keyed by username. That id is the `sub` claim of the tokens Keycloak issues for the
// user, which is what Percona Server's auth_openid_connect matches an account against — so a
// caller that has to create accounts up front needs the ids, not just the usernames.
func (a *App) ensureKeycloakClientUsers(ctx context.Context, kcContainerID, adminPW string, spec kcClientSpec) (string, map[string]string, error) {
	clientJSON, _ := json.Marshal(map[string]any{
		"clientId":                  spec.ClientID,
		"protocol":                  "openid-connect",
		"enabled":                   true,
		"publicClient":              spec.Public,
		"standardFlowEnabled":       spec.StdFlow,
		"directAccessGrantsEnabled": spec.DirectGrant,
		"redirectUris":              spec.Redirect,
		"attributes":                map[string]any{"oauth2.device.authorization.grant.enabled": fmt.Sprintf("%v", spec.DeviceFlow)},
	})
	var users []string
	for _, u := range spec.Users {
		users = append(users, strings.Join([]string{u.Username, u.First, u.Last, u.Group}, ":"))
	}
	env := []string{
		"KC_ADMIN_PW=" + adminPW,
		"REALM=" + spec.Realm,
		"CLIENT_ID=" + spec.ClientID,
		"CLIENT_JSON=" + string(clientJSON),
		"GROUPS_MAPPER=" + boolEnv(spec.GroupsClaim),
		"GROUPS=" + strings.Join(spec.Groups, " "),
		"USERS=" + strings.Join(users, " "),
		"DOMAIN=" + spec.Domain,
		"SAMPLE_PW=" + spec.SamplePW,
	}
	out, err := a.execScript(ctx, kcContainerID, keycloakClientScript, env)
	if err != nil {
		return "", nil, err
	}
	secret, ids := "", map[string]string{}
	var noPassword []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "SECRET="); ok {
			secret = v
		}
		if v, ok := strings.CutPrefix(line, "USERID="); ok {
			if name, id, found := strings.Cut(v, ":"); found && id != "" {
				ids[name] = id
			}
		}
		if v, ok := strings.CutPrefix(line, "PWFAIL="); ok {
			noPassword = append(noPassword, v)
		}
	}
	// A sample user nobody can sign in as is a broken demo, not a partial success — and it
	// used to pass silently, which is exactly how it went unnoticed.
	if len(noPassword) > 0 {
		return "", nil, fmt.Errorf("Keycloak would not set a password for %s", strings.Join(noPassword, ", "))
	}
	return secret, ids, nil
}

// keycloakClientScript ensures realm + client (from $CLIENT_JSON) + optional groups mapper,
// groups and sample users. It prints USERID=<username>:<id> per sample user and finally
// SECRET=<client secret> (empty for public clients).
const keycloakClientScript = `set -e
KC=/opt/keycloak/bin/kcadm.sh
$KC config credentials --server http://localhost:8080 --realm master --user admin --password "$KC_ADMIN_PW" >/dev/null
$KC get "realms/$REALM" >/dev/null 2>&1 || $KC create realms -s realm="$REALM" -s enabled=true -s sslRequired=external >/dev/null
CID=$($KC get clients -r "$REALM" -q clientId="$CLIENT_ID" --fields id --format csv --noquotes 2>/dev/null | tail -n1)
if [ -z "$CID" ]; then
  printf '%s' "$CLIENT_JSON" > /tmp/kc-client.json
  $KC create clients -r "$REALM" -f /tmp/kc-client.json >/dev/null
  CID=$($KC get clients -r "$REALM" -q clientId="$CLIENT_ID" --fields id --format csv --noquotes | tail -n1)
fi
# Audience mapper so the access token's aud carries the client id.
$KC get "clients/$CID/protocol-mappers/models" -r "$REALM" --fields name --format csv --noquotes 2>/dev/null | grep -q 'aud-mapper' || \
  $KC create "clients/$CID/protocol-mappers/models" -r "$REALM" -s name=aud-mapper -s protocol=openid-connect -s protocolMapper=oidc-audience-mapper -s 'config."included.client.audience"='"$CLIENT_ID" -s 'config."access.token.claim"=true' >/dev/null
if [ "$GROUPS_MAPPER" = "1" ]; then
  $KC get "clients/$CID/protocol-mappers/models" -r "$REALM" --fields name --format csv --noquotes 2>/dev/null | grep -q 'groups-mapper' || \
    $KC create "clients/$CID/protocol-mappers/models" -r "$REALM" -s name=groups-mapper -s protocol=openid-connect -s protocolMapper=oidc-group-membership-mapper -s 'config."claim.name"=groups' -s 'config."full.path"=false' -s 'config."access.token.claim"=true' -s 'config."id.token.claim"=true' -s 'config."userinfo.token.claim"=true' >/dev/null
fi
for g in $GROUPS; do
  $KC get groups -r "$REALM" --fields name --format csv --noquotes 2>/dev/null | grep -q "\"\?$g\"\?" || $KC create groups -r "$REALM" -s name="$g" >/dev/null
done
for spec in $USERS; do
  U=$(echo "$spec" | cut -d: -f1); FN=$(echo "$spec" | cut -d: -f2); LN=$(echo "$spec" | cut -d: -f3); GRP=$(echo "$spec" | cut -d: -f4)
  $KC create users -r "$REALM" -s username="$U" -s enabled=true -s email="$U@$DOMAIN" -s emailVerified=true -s firstName="$FN" -s lastName="$LN" >/dev/null 2>&1 || true
  UID1=$($KC get users -r "$REALM" -q username="$U" --fields id --format csv --noquotes | tail -n1)
  [ -n "$UID1" ] || continue
  echo "USERID=$U:$UID1"
  # set-password used to be fire-and-forget, and the FIRST user of a freshly created realm
  # would silently end up with no credential at all — Keycloak is still initialising the realm
  # when the credential write lands. (A later user in the same loop always succeeded.) The
  # re-login also covers the other way this fails quietly: the master realm's admin access
  # token lives 60s, which a long user list can outrun. Retry, then say so.
  n=0
  until $KC set-password -r "$REALM" --userid "$UID1" --new-password "$SAMPLE_PW" --temporary=false >/dev/null 2>&1; do
    n=$((n + 1))
    if [ "$n" -ge 5 ]; then echo "PWFAIL=$U"; break; fi
    sleep 2
    $KC config credentials --server http://localhost:8080 --realm master --user admin --password "$KC_ADMIN_PW" >/dev/null 2>&1 || true
  done
  if [ -n "$GRP" ]; then
    GID=$($KC get groups -r "$REALM" --fields id,name --format csv --noquotes | grep ",\?$GRP\"\?$" | head -n1 | cut -d, -f1 | tr -d '"')
    [ -n "$GID" ] && $KC update "users/$UID1/groups/$GID" -r "$REALM" -n >/dev/null 2>&1 || true
  fi
done
SECRET=$($KC get "clients/$CID/client-secret" -r "$REALM" --fields value --format csv --noquotes 2>/dev/null | tail -n1)
echo "SECRET=$SECRET"`
