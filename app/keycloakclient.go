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

type kcClientSpec struct {
	Realm       string
	ClientID    string
	Public      bool           // public client (no secret) vs confidential
	StdFlow     bool           // authorization-code flow (PMM); false for device-only
	DeviceFlow  bool           // OAuth 2.0 device authorization grant (psql/libpq)
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
	clientJSON, _ := json.Marshal(map[string]any{
		"clientId":                  spec.ClientID,
		"protocol":                  "openid-connect",
		"enabled":                   true,
		"publicClient":              spec.Public,
		"standardFlowEnabled":       spec.StdFlow,
		"directAccessGrantsEnabled": false,
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
		return "", err
	}
	secret := ""
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "SECRET="); ok {
			secret = v
		}
	}
	return secret, nil
}

// keycloakClientScript ensures realm + client (from $CLIENT_JSON) + optional groups mapper,
// groups and sample users, then prints SECRET=<client secret> (empty for public clients).
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
  $KC set-password -r "$REALM" --userid "$UID1" --new-password "$SAMPLE_PW" --temporary=false >/dev/null 2>&1 || true
  if [ -n "$GRP" ]; then
    GID=$($KC get groups -r "$REALM" --fields id,name --format csv --noquotes | grep ",\?$GRP\"\?$" | head -n1 | cut -d, -f1 | tr -d '"')
    [ -n "$GID" ] && $KC update "users/$UID1/groups/$GID" -r "$REALM" -n >/dev/null 2>&1 || true
  fi
done
SECRET=$($KC get "clients/$CID/client-secret" -r "$REALM" --fields value --format csv --noquotes 2>/dev/null | tail -n1)
echo "SECRET=$SECRET"`
