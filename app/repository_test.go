package main

import (
	"compress/gzip"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// repository_test.go — the Repository node's decisions: which builds a spec selects from real
// upstream metadata (testdata/repository holds ps-84-lts's el9 primary.xml and noble Packages as
// repo.percona.com published them), version order and matching, image paths, the files a
// consuming node and k3s read, and design validation. What was proven against a running stack is
// in IMPLEMENTATION.md §418.

func loadFixture(t *testing.T, name string) *gzip.Reader {
	t.Helper()
	f, err := os.Open("testdata/repository/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

func rpmFixture(t *testing.T) []repoPkg {
	pkgs, err := parsePrimaryXML(loadFixture(t, "ps-84-lts-el9-x86_64-primary.xml.gz"))
	if err != nil {
		t.Fatal(err)
	}
	return pkgs
}

func debFixture(t *testing.T) []repoPkg {
	pkgs, err := parseDebPackages(loadFixture(t, "ps-84-lts-noble-amd64-Packages.gz"))
	if err != nil {
		t.Fatal(err)
	}
	return pkgs
}

func repoNames(pkgs []repoPkg) map[string][]string {
	out := map[string][]string{}
	for _, p := range pkgs {
		out[p.Name] = append(out[p.Name], p.Version)
	}
	return out
}

func TestRepoRepomdPrimaryHref(t *testing.T) {
	b, err := os.ReadFile("testdata/repository/ps-84-lts-el9-x86_64-repomd.xml")
	if err != nil {
		t.Fatal(err)
	}
	href, err := repomdPrimaryHref(b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(href, "repodata/") || !strings.HasSuffix(href, "-primary.xml.gz") {
		t.Fatalf("primary href = %q", href)
	}
	if _, err := repomdPrimaryHref([]byte(`<repomd><data type="other"/></repomd>`)); err == nil {
		t.Fatal("a repomd without primary must be refused")
	}
}

func TestRepoParsePrimary(t *testing.T) {
	pkgs := rpmFixture(t)
	if len(pkgs) != 195 {
		t.Fatalf("parsed %d packages, the fixture has 195", len(pkgs))
	}
	var server *repoPkg
	for i, p := range pkgs {
		if p.Name == "percona-server-server" && p.Version == "8.4.5-5.1.el9" {
			server = &pkgs[i]
		}
	}
	if server == nil {
		t.Fatal("percona-server-server 8.4.5-5.1.el9 not parsed")
	}
	if server.Location != "percona-server-server-8.4.5-5.1.el9.x86_64.rpm" || len(server.SHA256) != 64 || server.Size <= 0 {
		t.Fatalf("server fields: %+v", *server)
	}
	if len(server.Requires) == 0 || len(server.Provides) == 0 {
		t.Fatal("requires/provides not parsed")
	}
}

func TestRepoSelectVersionRPM(t *testing.T) {
	got := repoNames(selectRepoPackages(rpmFixture(t), repoPkgSpec{Repo: "ps-84-lts", Versions: []string{"8.4.5-5.1"}}, false, false))
	for n, vs := range got {
		if repoIsDebugName(n) {
			t.Errorf("%s is debug symbols, not asked for", n)
		}
		if len(vs) != 1 {
			t.Errorf("%s: %v — one build per name expected", n, vs)
		}
	}
	for _, n := range []string{"percona-server-server", "percona-server-client", "percona-server-shared", "percona-icu-data-files"} {
		if len(got[n]) != 1 || got[n][0] != "8.4.5-5.1.el9" {
			t.Errorf("%s = %v, want 8.4.5-5.1.el9", n, got[n])
		}
	}
	// mysql-shell is on its own series (8.4.5-1): no 8.4.5-5.1 build, so the newest one — which is
	// what pin_install would install beside a pinned server.
	newest := ""
	for _, p := range rpmFixture(t) {
		if p.Name == "percona-mysql-shell" && (newest == "" || rpmVerCmp(p.Version, newest) > 0) {
			newest = p.Version
		}
	}
	if v := got["percona-mysql-shell"]; len(v) != 1 || v[0] != newest {
		t.Errorf("percona-mysql-shell = %v, want the newest build %s", v, newest)
	}
}

func TestRepoSelectShortVersionMatchesBothSeries(t *testing.T) {
	got := repoNames(selectRepoPackages(rpmFixture(t), repoPkgSpec{Versions: []string{"8.4.5"}}, false, false))
	if v := got["percona-server-server"]; len(v) != 1 || v[0] != "8.4.5-5.1.el9" {
		t.Errorf("server = %v", v)
	}
	if v := got["percona-mysql-shell"]; len(v) != 1 || v[0] != "8.4.5-1.el9" {
		t.Errorf("mysql-shell = %v, want its own 8.4.5 build", v)
	}
}

func TestRepoSelectLatestAndAll(t *testing.T) {
	pkgs := rpmFixture(t)
	latest := repoNames(selectRepoPackages(pkgs, repoPkgSpec{}, false, false))
	if v := latest["percona-server-server"]; len(v) != 1 || v[0] != "8.4.11-11.1.el9" {
		t.Errorf("latest server = %v", v)
	}
	all := selectRepoPackages(pkgs, repoPkgSpec{Versions: []string{"*"}}, false, true)
	if len(all) != len(pkgs) {
		t.Errorf("* with debug = %d builds, want all %d", len(all), len(pkgs))
	}
	noDebug := selectRepoPackages(pkgs, repoPkgSpec{Versions: []string{"*"}}, false, false)
	if len(noDebug) >= len(all) {
		t.Error("debug packages were not left out")
	}
}

func TestRepoSelectPackagesFollowsDependencies(t *testing.T) {
	got := repoNames(selectRepoPackages(rpmFixture(t), repoPkgSpec{Packages: []string{"percona-server-server"}, Versions: []string{"8.4.7-7.1"}}, false, false))
	for _, n := range []string{"percona-server-server", "percona-server-client", "percona-server-shared"} {
		if len(got[n]) != 1 || got[n][0] != "8.4.7-7.1.el9" {
			t.Errorf("%s = %v, want 8.4.7-7.1.el9 (pulled in by the server)", n, got[n])
		}
	}
	for _, n := range []string{"percona-server-test", "percona-server-devel", "percona-mysql-router"} {
		if len(got[n]) > 0 {
			t.Errorf("%s was not asked for and the server does not need it", n)
		}
	}
	glob := repoNames(selectRepoPackages(rpmFixture(t), repoPkgSpec{Packages: []string{"percona-mysql-*"}}, false, false))
	if len(glob["percona-mysql-router"]) != 1 || len(glob["percona-mysql-shell"]) != 1 || len(glob["percona-server-server"]) != 0 {
		t.Errorf("glob selection = %v", glob)
	}
}

func TestRepoSelectDebianWithRPMSpelling(t *testing.T) {
	pkgs := debFixture(t)
	if len(pkgs) == 0 {
		t.Fatal("no packages parsed")
	}
	// The design writes the version once, in the catalog's RPM spelling; Ubuntu's build is
	// 8.4.7-7-1.noble.
	chosen := selectRepoPackages(pkgs, repoPkgSpec{Versions: []string{"8.4.7-7.1"}}, true, false)
	got := repoNames(chosen)
	if v := got["percona-server-server"]; len(v) != 1 || v[0] != "8.4.7-7-1.noble" {
		t.Fatalf("server = %v", v)
	}
	// The index is the upstream paragraphs verbatim, and parses back to the same set.
	back, err := parseDebPackages(strings.NewReader(debPackagesIndex(chosen)))
	if err != nil || len(back) != len(chosen) {
		t.Fatalf("re-parse: %d vs %d (%v)", len(back), len(chosen), err)
	}
	for i := range back {
		if back[i].SHA256 != chosen[i].SHA256 || back[i].Location != chosen[i].Location {
			t.Fatalf("paragraph %d changed on the way through", i)
		}
	}
	latest := repoNames(selectRepoPackages(pkgs, repoPkgSpec{}, true, false))
	if v := latest["percona-server-server"]; len(v) != 1 || v[0] != "8.4.11-11-1.noble" {
		t.Errorf("latest server = %v", v)
	}
}

func TestRepoVersionMatch(t *testing.T) {
	for _, tc := range []struct {
		version, want string
		ok            bool
	}{
		{"8.4.5-5.1.el9", "8.4.5-5.1", true},
		{"8.4.5-5.1.el9", "8.4.5", true},
		{"8.4.5-5-1.noble", "8.4.5-5.1", true},
		{"8.4.10-10.1.el9", "8.4.1", false},
		{"8.4.1-1.el9", "8.4.1", true},
		{"1:16.10-1.el9", "16.10", true},
		{"16.10-1.noble", "2:16.10", true},
		{"16.1-1.el9", "16.10", false},
		{"8.0.42-33.1.el8", "8.0.42-33.1", true},
		{"8.0.42-33.1.el8", "", false},
	} {
		if got := repoVersionMatch(tc.version, tc.want); got != tc.ok {
			t.Errorf("repoVersionMatch(%q, %q) = %v, want %v", tc.version, tc.want, got, tc.ok)
		}
	}
}

func TestRepoVersionOrder(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"8.4.10-10.1.el9", "8.4.9-9.1.el9", 1},
		{"8.4.5-5.1.el9", "8.4.5-5.1.el9", 0},
		{"1.0~rc1", "1.0", -1},
		{"1.0a", "1.0", 1},
		{"2.0", "2.0.1", -1},
		{"1.01", "1.1", 0},
		{"1.0^git1", "1.0", 1},
	} {
		if got := rpmVerCmp(tc.a, tc.b); got != tc.want {
			t.Errorf("rpmVerCmp(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"8.4.10-10-1.noble", "8.4.9-9-1.noble", 1},
		{"1.0~rc1-1", "1.0-1", -1},
		{"1.0-1", "1.0-1", 0},
		{"1.0+b1", "1.0", 1},
		{"1.0a", "1.0+", -1},
	} {
		if got := debVerCmp(tc.a, tc.b); (got > 0) != (tc.want > 0) || (got < 0) != (tc.want < 0) {
			t.Errorf("debVerCmp(%q, %q) = %d, want sign of %d", tc.a, tc.b, got, tc.want)
		}
	}
	if repoPkgCmp(false, repoPkg{Epoch: "1", Version: "1.0"}, repoPkg{Version: "9.0"}) <= 0 {
		t.Error("an epoch outranks the version")
	}
}

func TestRepoDebRelationNames(t *testing.T) {
	got := debRelationNames("a (>= 1), b | c:any, d [amd64] ,")
	want := []string{"a", "b", "c", "d"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v", got)
	}
}

func TestRepoImagePaths(t *testing.T) {
	for _, tc := range []struct{ ref, reg, local, src string }{
		{"percona/percona-xtradb-cluster:8.0.42", "docker.io", "percona/percona-xtradb-cluster:8.0.42", "docker.io/percona/percona-xtradb-cluster:8.0.42"},
		{"haproxy:2.8", "docker.io", "library/haproxy:2.8", "docker.io/library/haproxy:2.8"},
		{"docker.io/percona/pmm-client:3", "docker.io", "percona/pmm-client:3", "docker.io/percona/pmm-client:3"},
		{"busybox", "docker.io", "library/busybox:latest", "docker.io/library/busybox"},
		{"quay.io/prometheus/node-exporter:v1.8", "quay.io", "quay.io/prometheus/node-exporter:v1.8", "quay.io/prometheus/node-exporter:v1.8"},
		{"registry.local:5000/a/b", "registry.local:5000", "registry.local:5000/a/b:latest", "registry.local:5000/a/b"},
		{"percona/x@sha256:abc", "docker.io", "percona/x:sha256-abc", "docker.io/percona/x@sha256:abc"},
	} {
		reg, local := repoImageSplit(tc.ref)
		if reg != tc.reg || local != tc.local || repoImageSource(tc.ref) != tc.src {
			t.Errorf("%s → (%s, %s, %s), want (%s, %s, %s)", tc.ref, reg, local, repoImageSource(tc.ref), tc.reg, tc.local, tc.src)
		}
	}
}

func TestRepoManifestImages(t *testing.T) {
	cr, err := os.ReadFile("testdata/cr.yaml")
	if err != nil {
		t.Fatal(err)
	}
	imgs := repoManifestImages(string(cr))
	if len(imgs) == 0 {
		t.Fatal("no images found in cr.yaml")
	}
	for _, i := range imgs {
		if strings.ContainsAny(i, " #\"'") {
			t.Errorf("image %q carries quoting or a comment", i)
		}
	}
	local := repoRewriteImages(string(cr), "repo.example.net:5000")
	for _, i := range repoManifestImages(local) {
		if !strings.HasPrefix(i, "repo.example.net:5000/") {
			t.Errorf("image %q not pointed at the Repository", i)
		}
	}
	// Commented images are the CR's options, not what it starts.
	m := "spec:\n  image: percona/a:1\n  # image: percona/b:2\n  - image: \"percona/c:3\" # note\n"
	if got := strings.Join(repoManifestImages(m), " "); got != "percona/a:1 percona/c:3" {
		t.Errorf("got %q", got)
	}
}

func TestRepoRegistriesYAML(t *testing.T) {
	y := repoRegistriesYAML("repo.example.net", []repoImage{{Source: "percona/a:1"}, {Source: "quay.io/x/y:2"}})
	var doc struct {
		Mirrors map[string]struct {
			Endpoint []string          `json:"endpoint"`
			Rewrite  map[string]string `json:"rewrite"`
		} `json:"mirrors"`
	}
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("registries.yaml does not parse: %v\n%s", err, y)
	}
	if doc.Mirrors["docker.io"].Endpoint[0] != "http://repo.example.net:5000" || len(doc.Mirrors["docker.io"].Rewrite) != 0 {
		t.Errorf("docker.io mirror: %+v", doc.Mirrors["docker.io"])
	}
	if doc.Mirrors["quay.io"].Rewrite["^(.*)$"] != "quay.io/$1" {
		t.Errorf("quay.io must be rewritten under its host: %+v", doc.Mirrors["quay.io"])
	}
}

func TestRepoTargetsAndCarriedKeys(t *testing.T) {
	// The key is computed on the consuming node from /etc/os-release and uname/dpkg — these are
	// the spellings those produce.
	for _, tc := range []struct{ os, ver, arch, key string }{
		{"oraclelinux", "9", "amd64", "yum-9-x86_64"},
		{"oraclelinux", "8.10", "arm64", "yum-8-aarch64"},
		{"ubuntu", "24.04", "amd64", "apt-noble-amd64"},
		{"debian", "12", "arm64", "apt-bookworm-arm64"},
	} {
		tg, ok := repoTargetForNode(tc.os, tc.ver)
		if !ok {
			t.Fatalf("%s %s has no target", tc.os, tc.ver)
		}
		if k := repoCarriedKey(tg, tc.arch); k != tc.key {
			t.Errorf("%s %s %s → %s, want %s", tc.os, tc.ver, tc.arch, k, tc.key)
		}
	}
	if _, ok := repoTargetForNode("centos", "7"); ok {
		t.Error("CentOS 7 is not a target")
	}
	if !strings.Contains(repoRewriteScript, `KEY="yum-${VERSION_ID%%.*}-$(uname -m)"`) ||
		!strings.Contains(repoRewriteScript, `KEY="apt-${VERSION_CODENAME}-$(dpkg --print-architecture)"`) {
		t.Error("the rewrite script no longer computes the key repoCarriedKey writes")
	}
}

func TestRepoPickChart(t *testing.T) {
	entries := []map[string]any{
		{"version": "1.19.0", "appVersion": "1.19.0"},
		{"version": "1.20.0", "appVersion": "1.20.0"},
		{"version": "1.20.1", "appVersion": "1.20.0"},
	}
	if e := repoPickChart(entries, "1.20.0"); e == nil || e["version"] != "1.20.1" {
		t.Errorf("picked %v, want the newest chart for 1.20.0", e)
	}
	if repoPickChart(entries, "9.9.9") != nil {
		t.Error("no chart for an unknown operator version")
	}
}

func TestRepositoryIssues(t *testing.T) {
	ok := designNode{ID: "r", Type: "repository", Label: "repo-01", RepoPackages: []repoPkgSpec{{Repo: "ps-84-lts"}}}
	if errs := errorsIn(repositoryIssues(ok)); len(errs) != 0 {
		t.Fatalf("a valid design: %v", errs)
	}
	for _, tc := range []struct {
		n    designNode
		want string
	}{
		{designNode{Label: "r", RepoTargets: []string{"centos-7"}, RepoImages: []string{"a"}}, "not an OS release"},
		{designNode{Label: "r", RepoArches: []string{"s390x"}, RepoImages: []string{"a"}}, "neither amd64 nor arm64"},
		{designNode{Label: "r", RepoPackages: []repoPkgSpec{{Repo: "PS 84"}}}, "not a repository name"},
		{designNode{Label: "r", RepoPackages: []repoPkgSpec{{Repo: "ps-80", Packages: []string{"[x"}}}}, "malformed"},
		{designNode{Label: "r", RepoImages: []string{"not an image"}}, "not an image reference"},
		{designNode{Label: "r", RepoOperators: []repoOperatorSpec{{Kind: "cnpg"}}}, "pxc, ps, psmdb or pg"},
	} {
		errs := errorsIn(repositoryIssues(tc.n))
		if len(errs) == 0 || !strings.Contains(errs[0].Message, tc.want) {
			t.Errorf("%+v: %v, want %q", tc.n, errs, tc.want)
		}
	}
	empty := repositoryIssues(designNode{Label: "r"})
	if len(errorsIn(empty)) != 0 || len(empty) != 1 || empty[0].Level != "warning" {
		t.Errorf("an empty Repository warns, it does not fail: %v", empty)
	}
}

func TestRepositoryConsumerIssues(t *testing.T) {
	doc := designDoc{
		Nodes: []designNode{
			{ID: "r", Type: "repository", Label: "repo-01", RepoTargets: []string{"oraclelinux-9"}, RepoArches: []string{"amd64"}},
			{ID: "a", Type: "ps", Label: "ps-01", OS: "oraclelinux", OSVersion: "9", Arch: "amd64", RepositoryNodeID: "r"},
			{ID: "b", Type: "ps", Label: "ps-02", OS: "ubuntu", OSVersion: "24.04", Arch: "amd64", RepositoryNodeID: "r"},
			{ID: "c", Type: "pg", Label: "pg-01", OS: "oraclelinux", OSVersion: "9", RepositoryNodeID: "gone"},
			{ID: "d", Type: "pg", Label: "pg-02", OS: "oraclelinux", OSVersion: "9", Arch: "arm64", RepositoryNodeID: "r"},
		},
		Frames: []designFrame{
			{ID: "k", Type: "k3d", Label: "k3d-01", RepositoryNodeID: "r"},
			{ID: "p", Type: "pxc", Label: "pxc-01", OS: "oraclelinux", OSVersion: "9", Arch: "amd64", RepositoryNodeID: "r"},
		},
	}
	t.Setenv("K3D_PLATFORM", "linux/amd64")
	all := repositoryConsumerIssues(doc)
	text := ""
	for _, i := range all {
		text += i.Level + ": " + i.Message + "\n"
	}
	if strings.Contains(text, "ps-01") || strings.Contains(text, "k3d-01") || strings.Contains(text, "pxc-01") {
		t.Errorf("carried consumers were flagged:\n%s", text)
	}
	if !strings.Contains(text, "warning: Node ps-02 runs Ubuntu 24.04 (noble), which Repository repo-01 does not carry") {
		t.Errorf("an uncarried OS must warn:\n%s", text)
	}
	if !strings.Contains(text, "error: Node pg-01 uses a Repository that is not on the canvas") {
		t.Errorf("a dangling association is an error:\n%s", text)
	}
	if !strings.Contains(text, "warning: Node pg-02 is arm64") {
		t.Errorf("an uncarried architecture must warn:\n%s", text)
	}
}

func TestRepoUseScriptEmbedsRewrite(t *testing.T) {
	// The heredoc delimiter must not appear inside the script it wraps, or the install would cut
	// it short and leave a truncated rewrite script on the node.
	if strings.Contains(repoRewriteScript, "DBCANVAS_REWRITE") {
		t.Fatal("the rewrite script contains its own heredoc delimiter")
	}
	for _, want := range []string{"dnf yum apt-get apt", "/usr/local/sbin/dbcanvas-repo-rewrite", "exec %s"} {
		if !strings.Contains(repoUseScript, want) {
			t.Errorf("repoUseScript lost %q", want)
		}
	}
}

func errorsIn(is []issue) []issue {
	var out []issue
	for _, i := range is {
		if i.Level == "error" {
			out = append(out, i)
		}
	}
	return out
}

func TestRepositoryK3DArchitecture(t *testing.T) {
	doc := designDoc{
		Nodes:  []designNode{{ID: "r", Type: "repository", Label: "repo-01", RepoArches: []string{"amd64"}}},
		Frames: []designFrame{{ID: "k", Type: "k3d", Label: "k3d-01", RepositoryNodeID: "r"}},
	}
	t.Setenv("K3D_PLATFORM", "linux/arm64")
	is := repositoryConsumerIssues(doc)
	if len(is) != 1 || !strings.Contains(is[0].Message, "runs arm64 k3s nodes") {
		t.Fatalf("an arm64 cluster on an amd64-only Repository must warn: %v", is)
	}
	doc.Nodes[0].RepoArches = []string{"amd64", "arm64"}
	if is := repositoryConsumerIssues(doc); len(is) != 0 {
		t.Fatalf("carried: %v", is)
	}
}

func TestRepositoryBlankRowIsIgnored(t *testing.T) {
	// "Add a repository… → Other" leaves a row with no name until one is typed; a deploy with it
	// still blank must not be refused.
	n := designNode{Label: "repo-cool", RepoPackages: []repoPkgSpec{{Repo: "ps-84-lts"}, {Repo: ""}, {Repo: "  "}}}
	is := repositoryIssues(n)
	if errs := errorsIn(is); len(errs) != 0 {
		t.Fatalf("blank rows refused: %v", errs)
	}
	if len(is) != 1 || !strings.Contains(is[0].Message, "2 package row(s) with no repository name") {
		t.Fatalf("want one warning naming the blank rows: %v", is)
	}
	c := repositoryConfig{Packages: n.RepoPackages, Added: repoAdditions{Packages: []repoPkgSpec{{Repo: ""}}}}
	if got := c.allPackages(); len(got) != 1 || got[0].Repo != "ps-84-lts" {
		t.Fatalf("a sync must skip blank rows: %v", got)
	}
	only := repositoryIssues(designNode{Label: "r", RepoPackages: []repoPkgSpec{{Repo: ""}}})
	if len(errorsIn(only)) != 0 || len(only) != 2 {
		t.Fatalf("only blank rows = the blank warning + carries nothing: %v", only)
	}
}
