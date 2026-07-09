package main

import "testing"

// VncAuth only carries 8 bytes; `vncpasswd -f` truncates silently. We truncate
// up-front so the password we store (and show in the node panel) is the one that
// actually authenticates.
func TestVNCAuthPassword(t *testing.T) {
	cases := []struct{ in, want string }{
		{"vnc_password", "vnc_pass"}, // the VNC_PASSWORD default
		{"vnc_pass", "vnc_pass"},     // exactly 8 — unchanged
		{"short", "short"},           // shorter than 8 — unchanged
		{"", ""},                     // empty — unchanged
		{"012345678", "01234567"},    // 9 → first 8
	}
	for _, c := range cases {
		if got := vncAuthPassword(c.in); got != c.want {
			t.Errorf("vncAuthPassword(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
