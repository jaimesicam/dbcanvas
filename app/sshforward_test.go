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
		{in: "127.0.0.1", ok: true, user: "", host: "127.0.0.1", port: 22},
		{in: "127.0.0.1:22", ok: true, user: "", host: "127.0.0.1", port: 22},
		{in: "10.0.0.7:2222", ok: true, user: "", host: "10.0.0.7", port: 2222},
		{in: " dbcanvas.example.net ", ok: true, user: "", host: "dbcanvas.example.net", port: 22},
		// A login may be pinned for everybody, for an installation whose operators
		// all ssh in under the same account. Without one it is left empty here and
		// resolved per request — see TestSSHForwardWithLogin.
		{in: "jaime@10.0.0.7", ok: true, user: "jaime", host: "10.0.0.7", port: 22},
		{in: "jaime@10.0.0.7:2222", ok: true, user: "jaime", host: "10.0.0.7", port: 2222},
		{in: "@10.0.0.7", ok: true, user: "", host: "10.0.0.7", port: 22},
		// IPv6, bracketed as anywhere else a host:port is written — and bare,
		// which SplitHostPort rejects and we then take whole.
		{in: "[::1]:2222", ok: true, user: "", host: "::1", port: 2222},
		{in: "::1", ok: true, user: "", host: "::1", port: 22},
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
	tgt = tgt.withLogin("user") // the placeholder, so the expected line is unchanged
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
	if s := sshForwardingSetting(""); s.Enabled {
		t.Errorf("unset SSH_FORWARDING_HOST should leave the feature off, got %+v", s)
	}
	t.Setenv(sshForwardingHostEnv, "jaime@10.0.0.7:2222")
	s := sshForwardingSetting("")
	if !s.Enabled || s.User != "jaime" || s.Host != "10.0.0.7" || s.Port != 2222 {
		t.Errorf("sshForwardingSetting() = %+v, want the parsed target enabled", s)
	}
	// With no "user@" the signed-in account is what the payload advertises — this is
	// the whole point of the login being optional.
	t.Setenv(sshForwardingHostEnv, "10.0.0.7")
	if s := sshForwardingSetting("jaime"); !s.Enabled || s.User != "jaime" {
		t.Errorf(`sshForwardingSetting("jaime") = %+v, want the signed-in account as the login`, s)
	}
	// Nobody signed in (or an unusable name) falls back to the placeholder rather
	// than shipping an empty login.
	if s := sshForwardingSetting(""); !s.Enabled || s.User != sshForwardPlaceholderUser {
		t.Errorf("sshForwardingSetting(\"\") = %+v, want the %q placeholder", s, sshForwardPlaceholderUser)
	}

	// A value the parser refuses turns the feature off rather than half-on — the host
	// and a configured login are held to the same standard.
	for _, bad := range []string{"10.0.0.7; id", "bad user@10.0.0.7", "o'brien@10.0.0.7"} {
		t.Setenv(sshForwardingHostEnv, bad)
		if s := sshForwardingSetting("jaime"); s.Enabled {
			t.Errorf("SSH_FORWARDING_HOST=%q should leave the feature off, got %+v", bad, s)
		}
	}
}

func TestSSHForwardWithLogin(t *testing.T) {
	// SSH_FORWARDING_HOST named no login, so the signed-in DBCanvas account is used —
	// on a server install the person at the browser usually has the ssh account too.
	bare, ok := parseSSHForwardingHost("10.0.0.7")
	if !ok || bare.User != "" {
		t.Fatalf("expected no login from a bare host, got %+v", bare)
	}
	if got := bare.withLogin("jaime").User; got != "jaime" {
		t.Errorf("withLogin(%q) = %q, want the signed-in account", "jaime", got)
	}

	// A configured "user@" wins: an administrator who wrote one meant it for everybody.
	pinned, ok := parseSSHForwardingHost("deploy@10.0.0.7")
	if !ok {
		t.Fatal("parseSSHForwardingHost rejected a pinned login")
	}
	if got := pinned.withLogin("jaime").User; got != "deploy" {
		t.Errorf("withLogin should not override a configured login, got %q", got)
	}

	// A username that cannot go into a shell command unquoted degrades to the
	// placeholder. DBCanvas validates usernames for length only (auth.go), so these
	// are all registerable.
	for _, bad := range []string{"", "   ", "a b", "bob;id", "$(whoami)", "o'brien", "x`id`", "a|b", "a\\b"} {
		if got := bare.withLogin(bad).User; got != sshForwardPlaceholderUser {
			t.Errorf("withLogin(%q) = %q, want the placeholder %q", bad, got, sshForwardPlaceholderUser)
		}
	}

	// And the resolved login really does reach the rendered command.
	cmd := sshForwardCommand(bare.withLogin("jaime"), "127.0.0.1", []sshForwardPort{{Container: 3306, Host: 13306}})
	if want := "ssh -L 13306:127.0.0.1:13306 jaime@10.0.0.7 -p 22"; cmd != want {
		t.Errorf("sshForwardCommand() = %q, want %q", cmd, want)
	}
}
