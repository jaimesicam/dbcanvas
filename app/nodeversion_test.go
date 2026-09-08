package main

import "testing"

// Banners captured from the real engines (the containers used to verify the OpenBao work).
func TestParseVersionBanner(t *testing.T) {
	tests := []struct {
		name   string
		banner string
		want   string
	}{
		{"Percona Server 8.4 (mysqld)", "/usr/sbin/mysqld  Ver 8.4.10-10 for Linux on x86_64 (Percona Server (GPL), Release 10, Revision d76e81f4)", "8.4.10-10"},
		{"Percona Server 8.0 (mysql client)", "mysql  Ver 8.0.46-37 for Linux on x86_64 (Percona Server (GPL), Release 37, Revision 39e2b60e)", "8.0.46-37"},
		{"PSMDB (mongod)", "db version v8.0.26-11", "8.0.26-11"},
		{"OpenBao (bao)", "OpenBao v2.5.5-1.el9, built 2026-06-16 (cgo)", "2.5.5-1.el9"},
		{"PostgreSQL (psql)", "psql (PostgreSQL) 16.10 - Percona Distribution", "16.10"},
		{"ProxySQL", "ProxySQL version 2.7.3-percona-1.1, codename Truls", "2.7.3-percona-1.1"},
		{"HAProxy", "HAProxy version 2.4.22-f8e3218 2023/02/14 - https://haproxy.org/", "2.4.22-f8e3218"},
		{"Valkey", "Valkey server v=8.1.1 sha=00000000:0 malloc=jemalloc bits=64 build=abc", "8.1.1"},
		{"Samba", "Version 4.19.4", "4.19.4"},
		{"PMM", "Version: 3.3.1", "3.3.1"},
		{"no version in the output", "command not found", ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseVersionBanner(tc.banner); got != tc.want {
				t.Errorf("parseVersionBanner(%q) = %q, want %q", tc.banner, got, tc.want)
			}
		})
	}
}

// Pulled-image nodes with no useful CLI fall back to their image tag — but only when the tag is
// a version. "latest" says nothing, and a registry port must not be mistaken for a tag.
func TestImageTagVersion(t *testing.T) {
	tests := []struct{ image, want string }{
		{"quay.io/keycloak/keycloak:26.5.5", "26.5.5"},
		{"percona/pmm-server:3.3.1", "3.3.1"},
		{"chrislusf/seaweedfs:latest", ""},
		{"valkey/valkey-bundle:latest", ""},
		{"percona/watchtower", ""},
		{"registry:5000/foo/bar", ""}, // a port, not a tag
	}
	for _, tc := range tests {
		if got := imageTagVersion(tc.image); got != tc.want {
			t.Errorf("imageTagVersion(%q) = %q, want %q", tc.image, got, tc.want)
		}
	}
}
