package main

import "testing"

func TestParseSSHForwardingHost(t *testing.T) {
	cases := []struct {
		in         string
		ok         bool
		user, host string
		port       int
	}{
		// The documented forms.
		{in: "", ok: false},
		{in: "   ", ok: false},
		{in: "127.0.0.1", ok: true, user: "user", host: "127.0.0.1", port: 22},
		{in: "127.0.0.1:22", ok: true, user: "user", host: "127.0.0.1", port: 22},
		{in: "10.0.0.7:2222", ok: true, user: "user", host: "10.0.0.7", port: 2222},
		{in: " dbcanvas.example.net ", ok: true, user: "user", host: "dbcanvas.example.net", port: 22},
		// A login may be carried along, so an installation whose operators share
		// an account is not stuck editing the placeholder every time.
		{in: "jaime@10.0.0.7", ok: true, user: "jaime", host: "10.0.0.7", port: 22},
		{in: "jaime@10.0.0.7:2222", ok: true, user: "jaime", host: "10.0.0.7", port: 2222},
		{in: "@10.0.0.7", ok: true, user: "user", host: "10.0.0.7", port: 22},
		// IPv6, bracketed as anywhere else a host:port is written — and bare,
		// which SplitHostPort rejects and we then take whole.
		{in: "[::1]:2222", ok: true, user: "user", host: "::1", port: 2222},
		{in: "::1", ok: true, user: "user", host: "::1", port: 22},
		// Rejected rather than patched up: the value is rendered into a command
		// the operator pastes into a shell.
		{in: "10.0.0.7:0", ok: false},
		{in: "10.0.0.7:99999", ok: false},
		{in: "10.0.0.7:ssh", ok: false},
		{in: "10.0.0.7; rm -rf /", ok: false},
		{in: "$(hostname)", ok: false},
		{in: "10.0.0.7 -o ProxyCommand=x", ok: false},
		{in: "10.0.0.7`id`", ok: false},
		{in: "10.0.0.7:22:22", ok: false},
	}
	for _, c := range cases {
		got, ok := parseSSHForwardingHost(c.in)
		if ok != c.ok {
			t.Errorf("parseSSHForwardingHost(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.User != c.user || got.Host != c.host || got.Port != c.port {
			t.Errorf("parseSSHForwardingHost(%q) = %+v, want user=%q host=%q port=%d",
				c.in, got, c.user, c.host, c.port)
		}
	}
}

func TestSSHForwardBindHost(t *testing.T) {
	cases := map[string]string{
		"":             "127.0.0.1", // unset — the app's own default
		"127.0.0.1":    "127.0.0.1",
		"0.0.0.0":      "127.0.0.1", // a wildcard bind is not an address you dial
		"::":           "127.0.0.1",
		"192.168.1.10": "192.168.1.10",
		"::1":          "[::1]", // ssh -L wants an IPv6 literal bracketed
		"nonsense; id": "127.0.0.1",
	}
	for in, want := range cases {
		t.Setenv("CONTAINER_BIND_IP", in)
		if got := sshForwardBindHost(); got != want {
			t.Errorf("sshForwardBindHost() with CONTAINER_BIND_IP=%q = %q, want %q", in, got, want)
		}
	}
}

func TestSSHForwardCommand(t *testing.T) {
	tgt, ok := parseSSHForwardingHost("127.0.0.1:22")
	if !ok {
		t.Fatal("parseSSHForwardingHost rejected the documented example")
	}
	// The example from the .env comment: a PMM node's two web ports.
	got := sshForwardCommand(tgt, "127.0.0.1", []sshForwardPort{
		{Container: 8443, Host: 8443}, {Container: 8080, Host: 8080},
	})
	want := "ssh -L 8443:127.0.0.1:8443 -L 8080:127.0.0.1:8080 user@127.0.0.1 -p 22"
	if got != want {
		t.Errorf("sshForwardCommand() = %q, want %q", got, want)
	}
	// A node that publishes nothing has no tunnel; the caller says so rather
	// than handing over a bare `ssh` line.
	if got := sshForwardCommand(tgt, "127.0.0.1", nil); got != "" {
		t.Errorf("sshForwardCommand() with no ports = %q, want empty", got)
	}
}

func TestSSHForwardingSetting(t *testing.T) {
	t.Setenv(sshForwardingHostEnv, "")
	if s := sshForwardingSetting(); s.Enabled {
		t.Errorf("unset SSH_FORWARDING_HOST should leave the feature off, got %+v", s)
	}
	t.Setenv(sshForwardingHostEnv, "jaime@10.0.0.7:2222")
	s := sshForwardingSetting()
	if !s.Enabled || s.User != "jaime" || s.Host != "10.0.0.7" || s.Port != 2222 {
		t.Errorf("sshForwardingSetting() = %+v, want the parsed target enabled", s)
	}
	// A value the parser refuses turns the feature off rather than half-on.
	t.Setenv(sshForwardingHostEnv, "10.0.0.7; id")
	if s := sshForwardingSetting(); s.Enabled {
		t.Errorf("an unsafe SSH_FORWARDING_HOST should leave the feature off, got %+v", s)
	}
}
