package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// upstreamProxySQLFixture is the compose fixture with repo.proxysql.com builds on Oracle Linux 9.
func upstreamProxySQLFixture(t *testing.T) {
	t.Helper()
	ol9 := "  - os: oraclelinux\n    version: \"9\"\n    platform: linux/amd64\n    arch: amd64\n"
	if !strings.Contains(composeVersionsYAML, ol9) {
		t.Fatal("the compose fixture has no Oracle Linux 9 entry")
	}
	yaml := strings.Replace(composeVersionsYAML, ol9, ol9+`    upstream_proxysql:
      "2":
        - 2.7.4
        - 2.6.6
        - 2.5.5
      "3":
        - 3.0.12
        - 3.0.11
        - 3.0.1
`, 1)
	path := filepath.Join(t.TempDir(), "versions.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERSIONS_FILE", path)
	t.Setenv("IMAGES_FILE", path)
	t.Setenv("DOCKER_PLATFORM", "linux/amd64")
}

func TestProxySQLUpstreamSeries(t *testing.T) {
	upstreamProxySQLFixture(t)
	for _, c := range []struct{ os, osVer, major, version, want string }{
		{"oraclelinux", "9", "3", "", "3.0.x"},       // newest 3 in the catalogue
		{"oraclelinux", "9", "2", "", "2.7.x"},       // newest 2
		{"oraclelinux", "9", "2", "2.5.5", "2.5.x"},  // pinned
		{"oraclelinux", "9", "3", "3.0.11", "3.0.x"}, // pinned
		{"ubuntu", "24.04", "2", "", "2.7.x"},        // nothing recorded: the newest series
	} {
		if got := proxysqlUpstreamSeries(c.os, c.osVer, "amd64", c.major, c.version); got != c.want {
			t.Errorf("%s %s %s %q: %s, want %s", c.os, c.osVer, c.major, c.version, got, c.want)
		}
	}
}

func TestProxySQLInstallStep(t *testing.T) {
	upstreamProxySQLFixture(t)
	env := func(e []string) string { return strings.Join(e, ";") }
	s, e, what := proxysqlInstallStep("", "oraclelinux", "9", "amd64", "3", "3.0.9-1.1", false, "example.net")
	if s != proxysqlInstallRHEL || env(e) != "PKG=proxysql3;VER=3.0.9-1.1" || what != "proxysql3" {
		t.Fatalf("Percona on EL: %q %q", env(e), what)
	}
	if s, _, _ := proxysqlInstallStep("", "ubuntu", "24.04", "amd64", "2", "", false, "example.net"); s != proxysqlInstallDebian {
		t.Fatal("Percona on Ubuntu")
	}
	s, e, what = proxysqlInstallStep("upstream-native", "oraclelinux", "9", "amd64", "3", "3.0.11", false, "example.net")
	if s != proxysqlInstallUpstreamRHEL || env(e) != "SERIES=3.0.x;VER=3.0.11" || strings.Contains(what, "proxysql-admin") {
		t.Fatalf("upstream-native on EL: %q %q", env(e), what)
	}
	s, e, what = proxysqlInstallStep("upstream-admin", "ubuntu", "24.04", "amd64", "2", "2.7.4", true, "example.net")
	if !strings.HasPrefix(s, proxysqlInstallUpstreamDebian) || !strings.HasSuffix(s, proxysqlAdminToolScript) {
		t.Fatal("upstream-admin on Ubuntu: the admin tool is installed after the package")
	}
	if env(e) != "SERIES=2.7.x;VER=2.7.4;HTTPS_PROXY=http://intranet.example.net:3128;TAG=v2.7.3" || !strings.Contains(what, "proxysql-admin v2.7.3") {
		t.Fatalf("upstream-admin on Ubuntu: %q %q", env(e), what)
	}
	if got := proxysqlRepoName("upstream-admin", "oraclelinux", "9", "amd64", "3", ""); got != "repo.proxysql.com proxysql-3.0.x" {
		t.Fatalf("repo name %q", got)
	}
	if !proxysqlPXCNative("upstream-native") || proxysqlPXCNative("upstream-admin") || proxysqlPXCNative("") {
		t.Fatal("proxysqlPXCNative")
	}
}

func TestProxySQLSourceIssues(t *testing.T) {
	upstreamProxySQLFixture(t)
	for _, c := range []struct {
		name, src, os, osVer, major, version, want string
	}{
		{"Percona, anything", "", "oraclelinux", "9", "2", "2.7.3-1.1", ""},
		{"upstream, newest", "upstream-native", "oraclelinux", "9", "3", "", ""},
		{"upstream, pinned", "upstream-admin", "oraclelinux", "9", "2", "2.6.6", ""},
		{"an unknown source", "proxysql.com", "oraclelinux", "9", "2", "", "unknown ProxySQL source"},
		{"a version from the other major", "upstream-native", "oraclelinux", "9", "2", "3.0.12", "not in the 2 series"},
		{"a build upstream does not have here", "upstream-native", "oraclelinux", "9", "3", "3.0.5", "no ProxySQL 3.0.5 for oraclelinux 9 — the 3 series there runs 3.0.1 to 3.0.12"},
		{"nothing recorded for the image", "upstream-native", "ubuntu", "24.04", "3", "3.0.5", ""},
	} {
		got := proxysqlSourceIssues("ProxySQL node p", c.src, c.os, c.osVer, "amd64", c.major, c.version)
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("%s: %+v", c.name, got)
			}
			continue
		}
		if len(got) != 1 || !strings.Contains(got[0].Message, c.want) {
			t.Errorf("%s: want %q, got %+v", c.name, c.want, got)
		}
	}
}

// The client-version limit follows where proxysql-admin comes from.
func TestProxySQLBackendIssuesBySource(t *testing.T) {
	design := func(series string) designDoc {
		return designDoc{
			Frames: []designFrame{{ID: "b", Type: "pxc", Label: "backend", PXCMajor: series}},
			Nodes:  []designNode{{ID: "px", Type: "proxysql", Label: "proxysql-01"}},
			Edges:  []designEdge{{From: edgeEnd{Node: "b"}, To: edgeEnd{Node: "px"}}},
		}
	}
	for _, c := range []struct {
		series, major, version, src string
		refused                     bool
	}{
		// Percona: the bundled tool's limit (8.4 needs 2.7.3).
		{"8.4", "2", "2.7.1-1.1", "", true},
		// upstream-admin: the release tag reads 8.4 whatever the ProxySQL build…
		{"8.4", "2", "2.7.1", "upstream-admin", false},
		// …but no release reads 9.x.
		{"9.7", "3", "3.0.12", "upstream-admin", true},
		// native: no proxysql-admin, no client limit.
		{"9.7", "3", "3.0.12", "upstream-native", false},
		// Authentication applies to every source.
		{"8.4", "2", "2.5.5", "upstream-native", true},
	} {
		got := proxysqlBackendIssues(design(c.series), "ProxySQL node proxysql-01", "px", c.major, c.version, c.src)
		if (len(got) > 0) != c.refused {
			t.Errorf("PXC %s behind %s %q: %v, want refused=%v", c.series, c.src, c.version, got, c.refused)
		}
	}
}

func TestProxySQLGaleraEnv(t *testing.T) {
	got := strings.Join(proxysqlGaleraEnv(proxysqlSecrets{AdminUser: "admin", AdminPassword: "a", AppUser: "app", AppPassword: "b", MonitorPassword: "c"},
		[]string{"pxc-1.example.net", "pxc-2.example.net"}, ""), ";")
	want := "ADMIN_USER=admin;ADMIN_PW=a;APP_USER=app;APP_PW=b;MON_USER=monitor;MON_PW=c;SERVERS=pxc-1.example.net,pxc-2.example.net;MODE=singlewrite"
	if got != want {
		t.Fatalf("env\n got %s\nwant %s", got, want)
	}
	doc := designDoc{
		Frames: []designFrame{{ID: "c", Type: "pxc", Label: "pxc-cluster-01"}},
		Nodes: []designNode{
			{ID: "a", FrameID: "c", Type: "pxc", Label: "pxc-1"},
			{ID: "b", FrameID: "c", Type: "pxc", Label: "pxc-2"},
			{ID: "g", FrameID: "c", Type: "pxc", Label: "garbd-1", Role: "arbitrator"},
		},
	}
	if got := strings.Join(pxcRegularFQDNs(doc, doc.Frames[0], "example.net"), ","); got != "pxc-1.example.net,pxc-2.example.net" {
		t.Fatalf("members %s", got)
	}
}

func TestComposeProxySQLSource(t *testing.T) {
	upstreamProxySQLFixture(t)
	doc, _, res, _ := composeBuild(t, composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "pxc", OS: "el9"},
		{Kind: "proxysql", OS: "el9", Source: "upstream-native", Version: "3.0.11"},
	}})
	for _, n := range doc.Nodes {
		if n.Type == "proxysql" && (n.ProxySQLSource != "upstream-native" || n.ProxySQLMajor != "3" || n.ProxySQLVersion != "3.0.11") {
			t.Fatalf("proxysql node: %q %q %q", n.ProxySQLSource, n.ProxySQLMajor, n.ProxySQLVersion)
		}
	}
	for _, r := range res {
		if r.Kind == "proxysql" && r.Version != "3.0.11" {
			t.Fatalf("plan %q", r.Version)
		}
	}
	// percona is the default, stored as "".
	if v, err := composeProxySQLSource("Percona"); err != nil || v != "" {
		t.Fatalf("source=Percona: %q %v", v, err)
	}
	// A Percona build string is not an upstream one.
	if _, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "pxc", OS: "el9"}, {Kind: "proxysql", OS: "el9", Source: "upstream-admin", Version: "2.7.3-1.1"}}}); err == nil {
		t.Fatal("an upstream entry accepted a Percona build string")
	}
	if _, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "pxc", OS: "el9"}, {Kind: "proxysql", OS: "el9", Source: "github"}}}); err == nil || !strings.Contains(err.Error(), "want percona, upstream-admin or upstream-native") {
		t.Fatalf("unknown source: %v", err)
	}
}

func TestProxySQLPGBackendAndIssues(t *testing.T) {
	doc := designDoc{
		Frames: []designFrame{{ID: "p", Type: "patroni", Label: "patroni-cluster-01", PGMajor: "17"}},
		Nodes:  []designNode{{ID: "px", Type: "proxysql", Label: "proxysql-01"}},
		Edges:  []designEdge{{From: edgeEnd{Node: "p"}, To: edgeEnd{Node: "px"}}},
	}
	kind, frame, node, ok := proxysqlPGBackend(doc, "px")
	if !ok || kind != "patroni" || frame.ID != "p" {
		t.Fatalf("backend %q %v %v", kind, frame.ID, ok)
	}
	if proxysqlPGMajor(kind, frame, node) != "17" {
		t.Fatal("major from the frame")
	}
	if _, _, ok := backendFrameForProxySQL(doc, "px"); ok {
		t.Fatal("a Patroni frame is not a MySQL-family backend")
	}
	n := doc.Nodes[0]
	if got := proxysqlPGIssues("ProxySQL node proxysql-01", n); len(got) != 2 {
		t.Fatalf("Percona 2: want two issues, got %v", got)
	}
	n.ProxySQLSource, n.ProxySQLMajor = "upstream-native", "3"
	if got := proxysqlPGIssues("ProxySQL node proxysql-01", n); len(got) != 0 {
		t.Fatalf("upstream 3: %v", got)
	}
	n.ProxySQLSource = "upstream-admin"
	if got := proxysqlPGIssues("ProxySQL node proxysql-01", n); len(got) != 0 {
		t.Fatalf("upstream-admin 3: %v", got)
	}
}

func TestProxySQLPGEnvAndClient(t *testing.T) {
	got := strings.Join(proxysqlPGEnv(pgSecrets{SuperUser: "postgres", SuperPassword: "pw"}, []string{"a.example.net", "b.example.net"}, ""), ";")
	for _, want := range []string{"APP_USER=postgres", "APP_PW=pw", "MON_USER=postgres", "SERVERS=a.example.net,b.example.net", "MODE=rwsplit"} {
		if !strings.Contains(got, want) {
			t.Errorf("env lacks %s: %s", want, got)
		}
	}
	s, env := proxysqlPGClientStep("oraclelinux", "17")
	if s != pgBouncerInstallRHEL || strings.Join(env, ";") != "PRODUCT=ppg-17;PKGS=percona-postgresql17" {
		t.Fatalf("EL client: %v", env)
	}
	s, env = proxysqlPGClientStep("ubuntu", "16")
	if s != pgBouncerInstallDebian || strings.Join(env, ";") != "PRODUCT=ppg-16;PKGS=percona-postgresql-client-16" {
		t.Fatalf("Ubuntu client: %v", env)
	}
}
