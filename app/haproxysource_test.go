package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// haproxyCatalogFixture is the compose fixture with HAProxy builds recorded on Oracle Linux 9.
func haproxyCatalogFixture(t *testing.T) {
	t.Helper()
	ol9 := "  - os: oraclelinux\n    version: \"9\"\n    platform: linux/amd64\n    arch: amd64\n"
	if !strings.Contains(composeVersionsYAML, ol9) {
		t.Fatal("the compose fixture has no Oracle Linux 9 entry")
	}
	yaml := strings.Replace(composeVersionsYAML, ol9, ol9+`    percona_haproxy:
      "2.8":
        - 2.8.27
        - 2.8.16
      "2.5":
        - 2.5.11
    pgdg_haproxy:
      "3.4":
        - 3.4.7
        - 3.4.6
      "3.2":
        - 3.2.8
        - 3.2.0
      "2.7":
        - 2.7.3
`, 1)
	path := filepath.Join(t.TempDir(), "versions.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERSIONS_FILE", path)
	t.Setenv("IMAGES_FILE", path)
	t.Setenv("DOCKER_PLATFORM", "linux/amd64")
}

func TestHAProxyCatalogVersions(t *testing.T) {
	haproxyCatalogFixture(t)
	vs, ok := haproxyCatalogVersions("percona", "oraclelinux", "9", "amd64")
	if !ok || strings.Join(vs, " ") != "2.8.27 2.8.16 2.5.11" {
		t.Fatalf("percona on OL9 = %v (%v)", vs, ok)
	}
	vs, _ = haproxyCatalogVersions("pgdg", "oraclelinux", "9", "amd64")
	if strings.Join(vs, " ") != "3.4.7 3.4.6 3.2.8 3.2.0 2.7.3" {
		t.Fatalf("pgdg on OL9 = %v", vs)
	}
	if vs, ok := haproxyCatalogVersions("", "oraclelinux", "9", "amd64"); ok || vs != nil {
		t.Fatalf("the OS package has a catalogue: %v", vs)
	}
}

func TestHAProxySourceIssues(t *testing.T) {
	haproxyCatalogFixture(t)
	node := func(src, ver, os, osVer string) designNode {
		return designNode{Label: "haproxy-01", Type: "haproxy", OS: os, OSVersion: osVer, Arch: "amd64",
			HAProxySource: src, HAProxyVersion: ver}
	}
	for _, tc := range []struct {
		name string
		n    designNode
		want string // "" → no issue
	}{
		{"the OS package", node("", "", "oraclelinux", "9"), ""},
		{"Percona, newest", node("percona", "", "ubuntu", "24.04"), ""},
		{"Percona, an old build", node("percona", "2.5.11", "oraclelinux", "9"), ""},
		{"PGDG, pinned", node("pgdg", "3.2.0", "oraclelinux", "9"), ""},
		{"an unknown source", node("haproxy.org", "", "oraclelinux", "9"), "unknown package source"},
		{"PGDG on Ubuntu", node("pgdg", "", "ubuntu", "24.04"), "EL only"},
		{"pinning the OS package", node("", "2.8.14", "oraclelinux", "9"), "cannot be pinned"},
		{"a build the source does not have", node("pgdg", "2.8.16", "oraclelinux", "9"), "has no HAProxy 2.8.16 on oraclelinux 9 — it has 2.7.3 to 3.4.7"},
		// Nothing recorded for the image: the install is the check.
		{"no catalogue for the image", node("percona", "2.8.16", "oraclelinux", "8"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := haproxySourceIssues(tc.n)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("issues: %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Level != "error" || !strings.Contains(got[0].Message, tc.want) {
				t.Fatalf("want one error containing %q, got %+v", tc.want, got)
			}
		})
	}
}

func TestHAProxyPPGOrder(t *testing.T) {
	if got := strings.Join(haproxyPPGOrder("16"), " "); got != "16 18 17 15 14 13" {
		t.Fatalf("PG 16 backend: %s", got)
	}
	if got := strings.Join(haproxyPPGOrder("16.4"), " "); got != "16 18 17 15 14 13" {
		t.Fatalf("a minor is its series: %s", got)
	}
	if got := strings.Join(haproxyPPGOrder(""), " "); got != "18 17 16 15 14 13" {
		t.Fatalf("a MySQL backend: %s", got)
	}
}

func TestHAProxyInstallStep(t *testing.T) {
	envOf := func(env []string) string { return strings.Join(env, ";") }
	n := designNode{Type: "haproxy", OS: "oraclelinux", OSVersion: "9"}
	if s, env := haproxyInstallStep(n, "17"); s != haproxyInstallRHEL || env != nil {
		t.Fatalf("OS source on EL: %q %v", s, env)
	}
	n.OS = "ubuntu"
	if s, _ := haproxyInstallStep(n, ""); s != haproxyInstallDebian {
		t.Fatal("OS source on Ubuntu")
	}
	n.HAProxySource, n.HAProxyVersion = "percona", "2.5.11"
	if s, env := haproxyInstallStep(n, "17"); s != haproxyInstallPerconaDebian || envOf(env) != "VER=2.5.11;SERIES=17 18 16 15 14 13" {
		t.Fatalf("Percona on Ubuntu: %v", env)
	}
	n.OS = "oraclelinux"
	if s, _ := haproxyInstallStep(n, ""); s != haproxyInstallPerconaRHEL {
		t.Fatal("Percona on EL")
	}
	n.HAProxySource, n.HAProxyVersion = "pgdg", ""
	if s, env := haproxyInstallStep(n, "17"); s != haproxyInstallPGDGRHEL || envOf(env) != "VER=" {
		t.Fatalf("PGDG on EL: %v", env)
	}
	if haproxyBackendPGMajor("patroni", designFrame{PGMajor: "17"}) != "17" || haproxyBackendPGMajor("pxc", designFrame{PGMajor: "17"}) != "" {
		t.Fatal("haproxyBackendPGMajor")
	}
}

func TestResolveHAProxyVersion(t *testing.T) {
	haproxyCatalogFixture(t)
	for _, tc := range []struct {
		src, os, osVer, want, major, minor, err string
	}{
		{"percona", "oraclelinux", "9", "", "", "", ""},
		{"os", "oraclelinux", "9", "", "", "", ""},
		{"percona", "oraclelinux", "9", "2.5.11", "2.5", "2.5.11", ""},
		{"pgdg", "oraclelinux", "9", "3.2", "3.2", "3.2.8", ""},
		{"pgdg", "oraclelinux", "9", "3.4.6", "3.4", "3.4.6", ""},
		{"", "oraclelinux", "9", "2.8", "", "", "needs source=percona or source=pgdg"},
		{"pgdg", "ubuntu", "24.04", "", "", "", "Oracle Linux only"},
		{"upstream", "oraclelinux", "9", "", "", "", "want os, percona or pgdg"},
		{"pgdg", "oraclelinux", "9", "9.9", "", "", "9.9"},
	} {
		major, minor, err := resolveHAProxyVersion(tc.src, tc.os, tc.osVer, "amd64", tc.want)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s %s %q: want error %q, got %v", tc.src, tc.os, tc.want, tc.err, err)
			}
			continue
		}
		if err != nil || major != tc.major || minor != tc.minor {
			t.Errorf("%s %s %q = %q %q %v; want %q %q", tc.src, tc.os, tc.want, major, minor, err, tc.major, tc.minor)
		}
	}
}

func TestComposeHAProxySource(t *testing.T) {
	haproxyCatalogFixture(t)
	doc, _, res, _ := composeBuild(t, composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "patroni", OS: "el9"},
		{Kind: "haproxy", OS: "el9", Source: "pgdg", Version: "3.2"},
	}})
	var hp designNode
	for _, n := range doc.Nodes {
		if n.Type == "haproxy" {
			hp = n
		}
	}
	if hp.HAProxySource != "pgdg" || hp.HAProxyVersion != "3.2.8" {
		t.Fatalf("haproxy node: source %q version %q", hp.HAProxySource, hp.HAProxyVersion)
	}
	for _, r := range res {
		if r.Kind == "haproxy" && r.Version != "3.2.8" {
			t.Fatalf("the plan says %q", r.Version)
		}
	}
	// source= on a kind that has none is refused, as every scalar is.
	if _, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "ps", Source: "percona"}}}); err == nil ||
		!strings.Contains(err.Error(), `does not support "source"`) {
		t.Fatalf("source on ps: %v", err)
	}
	// os is the OS repository, stored as "".
	doc, _, _, _ = composeBuild(t, composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "patroni", OS: "el9"}, {Kind: "haproxy", OS: "el9", Source: "os"},
	}})
	for _, n := range doc.Nodes {
		if n.Type == "haproxy" && (n.HAProxySource != "" || n.HAProxyVersion != "") {
			t.Fatalf("source=os stored as %q %q", n.HAProxySource, n.HAProxyVersion)
		}
	}
}
