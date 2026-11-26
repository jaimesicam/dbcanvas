package main

import "testing"

// TestPMMPolicyPassword: the PMM admin password meets PMM 3's policy (12+ characters, upper,
// lower, digit, symbol) without losing what it was: a generated one gets a symbol only.
func TestPMMPolicyPassword(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Ab3defGhijkLmnoPqrsT", "Ab3defGhijkLmnoPqrsT_"},
		{"admin_password", "admin_passwordA1"},
		{"Already_Ok123", "Already_Ok123"},
		{"aB1", "aB1_00000000"},
	} {
		if got := pmmPolicyPassword(c.in); got != c.want {
			t.Errorf("pmmPolicyPassword(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
