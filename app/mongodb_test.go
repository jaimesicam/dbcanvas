package main

import "testing"

// PSMDB may combine LDAP and Kerberos (they share one dirauth setParameter block), but Keycloak
// OIDC renders a setParameter block of its own and so excludes both.
func TestMongoOIDCIssues(t *testing.T) {
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
		{"no auth at all", designNode{Type: "psm", Label: "m1"}, sslOn, false},
		{"ldap only", designNode{Type: "psm", Label: "m1", LdapAuth: true}, sslOn, false},
		{"kerberos only", designNode{Type: "psm", Label: "m1", KerberosAuth: true}, sslOn, false},
		{"ldap + kerberos coexist", designNode{Type: "psm", Label: "m1", LdapAuth: true, KerberosAuth: true}, sslOn, false},
		{"oidc only", designNode{Type: "psm", Label: "m1", EnableOIDC: true, KeycloakNodeID: kc}, sslOn, false},
		{"oidc + ldap", designNode{Type: "psm", Label: "m1", EnableOIDC: true, KeycloakNodeID: kc, LdapAuth: true}, sslOn, true},
		{"oidc + kerberos", designNode{Type: "psm", Label: "m1", EnableOIDC: true, KeycloakNodeID: kc, KerberosAuth: true}, sslOn, true},
		{"oidc + ldap + kerberos", designNode{Type: "psm", Label: "m1", EnableOIDC: true, KeycloakNodeID: kc, LdapAuth: true, KerberosAuth: true}, sslOn, true},
		{"oidc without a keycloak node", designNode{Type: "psm", Label: "m1", EnableOIDC: true}, sslOn, true},
		{"oidc against a plain-HTTP keycloak", designNode{Type: "psm", Label: "m1", EnableOIDC: true, KeycloakNodeID: kc}, sslOff, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mongoOIDCIssues(tc.node, keycloakIDs, tc.keycloakSSL)
			if tc.wantIssue && len(got) == 0 {
				t.Fatalf("expected a validation error, got none")
			}
			if !tc.wantIssue && len(got) > 0 {
				t.Fatalf("expected no validation error, got %q", got[0].Message)
			}
		})
	}
}
