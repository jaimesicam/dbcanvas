package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Percona Server's auth_openid_connect exists in exactly one series and only from one minor
// (8.4.11-11), so the design has to be rejected before a deploy discovers the missing .so.
// Unlike PostgreSQL, MySQL chooses its auth plugin per account, so LDAP is not a conflict.
func TestOIDCIssuesPerconaServer(t *testing.T) {
	const kc = "kc1"
	keycloakIDs := map[string]bool{kc: true}
	sslOn := map[string]bool{kc: true}
	sslOff := map[string]bool{kc: false}

	tests := []struct {
		name        string
		node        designNode
		keycloakSSL map[string]bool
		wantIssue   bool
	}{
		{"oidc off is never an issue", designNode{Type: "ps", Label: "ps1", PSMajor: "8.0"}, sslOn, false},
		{"8.4 latest minor", designNode{Type: "ps", Label: "ps1", PSMajor: "8.4", EnableOIDC: true, KeycloakNodeID: kc}, sslOn, false},
		{"8.4 pinned to the first release with the plugin", designNode{Type: "ps", Label: "ps1", PSMajor: "8.4", PSVersion: "8.4.11-11.1", EnableOIDC: true, KeycloakNodeID: kc}, sslOn, false},
		{"alongside LDAP, which MySQL allows", designNode{Type: "ps", Label: "ps1", PSMajor: "8.4", EnableOIDC: true, KeycloakNodeID: kc, LdapAuth: true}, sslOn, false},
		{"8.0 has no plugin", designNode{Type: "ps", Label: "ps1", PSMajor: "8.0", EnableOIDC: true, KeycloakNodeID: kc}, sslOn, true},
		{"9.7 does not carry it yet", designNode{Type: "ps", Label: "ps1", PSMajor: "9.7", EnableOIDC: true, KeycloakNodeID: kc}, sslOn, true},
		{"8.4 pinned to an older minor", designNode{Type: "ps", Label: "ps1", PSMajor: "8.4", PSVersion: "8.4.10-10.1", EnableOIDC: true, KeycloakNodeID: kc}, sslOn, true},
		{"no keycloak node selected", designNode{Type: "ps", Label: "ps1", PSMajor: "8.4", EnableOIDC: true}, sslOn, true},
		{"keycloak without SSL cannot issue an HTTPS issuer", designNode{Type: "ps", Label: "ps1", PSMajor: "8.4", EnableOIDC: true, KeycloakNodeID: kc}, sslOff, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := oidcIssues(tc.node, keycloakIDs, tc.keycloakSSL)
			if tc.wantIssue && len(got) == 0 {
				t.Fatalf("expected a validation error, got none")
			}
			if !tc.wantIssue && len(got) > 0 {
				t.Fatalf("expected no validation error, got %q", got[0].Message)
			}
			if tc.wantIssue && !strings.Contains(got[0].Message, "ps1") {
				t.Fatalf("the error does not name the node: %q", got[0].Message)
			}
		})
	}
}

func TestPSVersionAtLeast(t *testing.T) {
	tests := []struct {
		v, min string
		want   bool
	}{
		{"", mysqlOIDCMinVersion, true},     // "latest" is always new enough
		{"   ", mysqlOIDCMinVersion, true},  // and so is a blank one
		{"8.4.11-11.1", "8.4.11-11", true},  // the catalog carries a trailing build number
		{"8.4.11-11", "8.4.11-11", true},    // exactly the minimum
		{"8.4.12-12.1", "8.4.11-11", true},  // a later patch
		{"8.4.10-10.1", "8.4.11-11", false}, // 10 < 11, and not lexicographically
		{"8.4.9-9.1", "8.4.11-11", false},   // 9 < 11, where a string compare would say otherwise
		{"8.4.11-10.1", "8.4.11-11", false}, // same upstream, older Percona build
		{"9.7.1-1.1", "8.4.11-11", true},    // newer series — the major gate handles 9.7, not this
		{"8.4", "8.4.11-11", false},         // too coarse to prove anything
		{"nonsense", "8.4.11-11", false},    // unparseable compares as short
	}
	for _, tc := range tests {
		if got := psVersionAtLeast(tc.v, tc.min); got != tc.want {
			t.Errorf("psVersionAtLeast(%q, %q) = %v, want %v", tc.v, tc.min, got, tc.want)
		}
	}
}

// The trust document is what the plugin parses out of $CONFFILE, and every key in it is
// spelled the way Percona's reference expects — a typo there fails at login, not at deploy.
func TestMySQLOIDCIDPJSON(t *testing.T) {
	const issuer = "https://keycloak.example.net:8443/realms/dbcanvas"
	var doc map[string]map[string]any
	if err := json.Unmarshal([]byte(mysqlOIDCIDPJSON(issuer, mysqlOIDCClientID)), &doc); err != nil {
		t.Fatalf("the rendered configuration is not JSON: %v", err)
	}
	idp, ok := doc[mysqlOIDCIDPName]
	if !ok {
		t.Fatalf("no %q identity provider in %v", mysqlOIDCIDPName, doc)
	}
	if idp["issuer-name"] != issuer {
		t.Errorf("issuer-name = %v, want %s", idp["issuer-name"], issuer)
	}
	// The JWKS endpoint has to be the issuer's own, over HTTPS: the plugin verifies the
	// signature against those keys using the system trust store.
	if want := issuer + "/protocol/openid-connect/certs"; idp["jwks-url"] != want {
		t.Errorf("jwks-url = %v, want %s", idp["jwks-url"], want)
	}
	auds, _ := idp["audiences"].([]any)
	if len(auds) != 1 || auds[0] != mysqlOIDCClientID {
		t.Errorf("audiences = %v, want [%s]", idp["audiences"], mysqlOIDCClientID)
	}
	if idp["group-claim"] != "groups" {
		t.Errorf("group-claim = %v, want groups", idp["group-claim"])
	}
	// Keycloak's group mapper is created with full.path=false, so the key must be the bare
	// group name — "/accounting", as in Percona's quickstart, would never match the claim.
	roles, _ := idp["group-role"].([]any)
	if len(roles) != 1 {
		t.Fatalf("group-role = %v, want one mapping", idp["group-role"])
	}
	m, _ := roles[0].(map[string]any)
	if m[mysqlOIDCGroup] != mysqlOIDCRole {
		t.Errorf("group-role = %v, want %s → %s", roles[0], mysqlOIDCGroup, mysqlOIDCRole)
	}
	if strings.HasPrefix(mysqlOIDCGroup, "/") {
		t.Errorf("group name %q is a full path — the mapper emits bare names", mysqlOIDCGroup)
	}
}
