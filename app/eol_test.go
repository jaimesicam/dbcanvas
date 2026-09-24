package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// eol_test.go — EOL, and the two things behind it: CentOS 7 as a Linux Client release, and PMM 2.

func TestEOLDefaultsOff(t *testing.T) {
	t.Setenv(eolEnv, "")
	if eolEnabled() {
		t.Error("unset EOL should be off")
	}
	for _, off := range []string{"off", "OFF", "false", "0", "no", " ", "maybe"} {
		t.Setenv(eolEnv, off)
		if eolEnabled() {
			t.Errorf("EOL=%q should be off", off)
		}
	}
	for _, on := range []string{"on", "ON", " On ", "true", "1", "yes", "y", "enabled"} {
		t.Setenv(eolEnv, on)
		if !eolEnabled() {
			t.Errorf("EOL=%q should be on", on)
		}
	}
}

// The designer decides what to offer from this field, so it rides on the settings every client
// fetches, is re-derived on every read, and is independent of EXPERIMENTAL.
func TestSystemSettingsCarryEOL(t *testing.T) {
	app := newTestApp(t)
	t.Setenv(experimentalEnv, "on")
	t.Setenv(eolEnv, "")
	if s := app.systemSettings(""); s.EOL || !s.Experimental {
		t.Errorf("EOL unset, EXPERIMENTAL on: got eol=%v experimental=%v", s.EOL, s.Experimental)
	}
	t.Setenv(experimentalEnv, "")
	t.Setenv(eolEnv, "on")
	if s := app.systemSettings(""); !s.EOL || s.Experimental {
		t.Errorf("EOL on, EXPERIMENTAL unset: got eol=%v experimental=%v", s.EOL, s.Experimental)
	}
}

// CentOS 7 rides beside the images catalogue, never inside it — HAProxy and All in One pick an OS
// from that list and install nothing on CentOS 7 — and only when EOL is on.
func TestImagesCatalogOffersCentOS7OnlyWithEOL(t *testing.T) {
	app := newTestApp(t)
	u, err := app.store.CreateUser("tester", "x", RoleUser, StatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	get := func() (images, eol []PXCImage) {
		r, _ := signedIn(t, app, u, "")
		w := httptest.NewRecorder()
		app.handleImagesCatalog(w, r)
		var out struct {
			Images []PXCImage `json:"images"`
			EOL    []PXCImage `json:"eol"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
		return out.Images, out.EOL
	}
	t.Setenv(eolEnv, "off")
	if _, eol := get(); len(eol) != 0 {
		t.Errorf("EOL off still offers %v", eol)
	}
	t.Setenv(eolEnv, "on")
	images, eol := get()
	if len(eol) != 1 || eol[0].OS != "centos" || eol[0].OSVersion != "7" {
		t.Errorf("EOL on should offer exactly CentOS 7, got %v", eol)
	}
	for _, i := range images {
		if isEL7OS(i.OS) {
			t.Error("CentOS 7 leaked into the main images catalogue")
		}
	}
}

// The repo file is the user-supplied one, and what matters about it is that nothing in it still
// points at the mirrors that went away.
func TestEL7RepoFileUsesTheVault(t *testing.T) {
	for _, section := range []string{"[base]", "[updates]", "[extras]"} {
		if !strings.Contains(el7CentOSBaseRepo, section) {
			t.Errorf("CentOS-Base.repo has no %s", section)
		}
	}
	for _, line := range strings.Split(el7CentOSBaseRepo, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "mirrorlist=") || strings.HasPrefix(line, "baseurl=http://mirror.centos.org") {
			t.Errorf("active line still points at a dead mirror: %s", line)
		}
		if strings.HasPrefix(line, "baseurl=") && !strings.Contains(line, "vault.centos.org") {
			t.Errorf("baseurl not on the vault: %s", line)
		}
	}
	env := strings.Join(el7BootstrapEnv(""), "\n")
	if !strings.Contains(env, "REPO_FILE=# CentOS-Base.repo") {
		t.Error("the bootstrap does not receive the repo file")
	}
	for _, want := range []string{"/etc/yum.repos.d/CentOS-Base.repo", "ip_resolve=4", "/etc/yum.conf",
		"percona-release-latest.noarch.rpm", "vault.centos.org/7.9.2009/sclo"} {
		if !strings.Contains(el7BootstrapScript, want) {
			t.Errorf("el7BootstrapScript does not mention %s", want)
		}
	}
	if strings.Contains(el7BootstrapScript, "dnf") {
		t.Error("el7BootstrapScript uses dnf, which CentOS 7 does not have")
	}
}

// Sample Client Code on CentOS 7: what cannot run is refused with a reason before anything is
// installed, and everything else installs with yum or a pinned upstream archive — never dnf.
func TestSampleCodeOnCentOS7(t *testing.T) {
	el7 := scOS{"centos", "7"}
	refused := map[string]bool{}
	for k := range scUnsupportedOn(el7) {
		refused[k] = true
	}
	for _, c := range scClients {
		key := c.Database + "/" + c.Language + "/" + c.ID
		why := scUnsupported(c, el7)
		wantRefused := c.Runtime == scRuntimeDotnet || (c.Database == scValkey && c.Runtime == scRuntimeShell)
		if wantRefused != (why != "") {
			t.Errorf("%s: refused=%v (%q), want refused=%v", key, why != "", why, wantRefused)
		}
		if refused[key] != wantRefused {
			t.Errorf("%s: scUnsupportedOn disagrees with scUnsupported", key)
		}
		if scUnsupported(c, scOS{"oraclelinux", "9"}) != "" {
			t.Errorf("%s: refused on Oracle Linux 9, which runs everything", key)
		}
		if wantRefused {
			continue
		}
		g := scNewGen(scSampleID(c.Database, c.Language, c.ID, "crud"), c, scScenarios[5], scTestTarget(c.Database), "centos", "")
		for _, s := range scBuildPlan(c, g, el7, false).System {
			if strings.Contains(s.Cmd, "dnf ") {
				t.Errorf("%s: step %q runs dnf on CentOS 7", key, s.Label)
			}
			if !strings.Contains(s.Show, "yum -y install") && !strings.Contains(s.Show, "checksum pinned") {
				t.Errorf("%s: step %q is neither yum nor a pinned archive: %s", key, s.Label, s.Show)
			}
		}
	}

	// The package choices that differ from EL8+, each one found on a live CentOS 7 node.
	pick := func(id string, t0 scTarget) (pkgs []string, repo string, tb *scTarball) {
		p := scSysPackages[id]
		pkgs = p.Packages(el7, t0)
		if p.Repo != nil {
			repo = p.Repo(el7, t0)
		}
		if p.Tarball != nil {
			tb = p.Tarball(el7)
		}
		return
	}
	if pkgs, _, _ := pick("python3", scTarget{}); pkgs[0] != "rh-python38-python" {
		t.Errorf("python on CentOS 7 = %v, want the rh-python38 collection", pkgs)
	}
	if scPythonBin(el7) != scPythonEL7 {
		t.Errorf("the virtualenv is built from %s, not rh-python38", scPythonBin(el7))
	}
	for id, want := range map[string]*scTarball{"nodejs": &scNodeUpstreamEL7, "golang": &scGoUpstream,
		"jdk": &scJDKUpstreamEL7, "maven": &scMavenUpstream} {
		if _, _, tb := pick(id, scTarget{}); tb != want {
			t.Errorf("%s on CentOS 7 does not come from its pinned archive", id)
		}
	}
	pg17 := scTarget{Major: "17"}
	if pkgs, repo, _ := pick("psql-client", pg17); pkgs[0] != "percona-postgresql13" || repo != "ppg-13" {
		t.Errorf("psql for PostgreSQL 17 on CentOS 7 = %v from %s, want percona-postgresql13 from ppg-13 (14+ need libzstd)", pkgs, repo)
	}
	if got := scPgBinDir("centos", pg17); got != "/usr/pgsql-13/bin" {
		t.Errorf("psql bin dir on CentOS 7 = %s", got)
	}
	if _, repo, _ := pick("psql-client", scTarget{Major: "13"}); repo != "ppg-13" {
		t.Errorf("a PostgreSQL 13 target should keep its own client repo, got %s", repo)
	}
	if _, repo, _ := pick("mysql-client", scTarget{Major: "8.4"}); repo != psClientProduct("8.0") {
		t.Errorf("mysql client for 8.4 on CentOS 7 comes from %s, want the 8.0 repo", repo)
	}
	if pkgs, repo, _ := pick("mysql-client", scTarget{Major: "5.7"}); pkgs[0] != "Percona-Server-client-57" || repo != psClientProduct("5.7") {
		t.Errorf("mysql client for 5.7 on CentOS 7 = %v from %s", pkgs, repo)
	}
	if _, repo, _ := pick("mongosh", scTarget{Major: "8.0"}); repo != "psmdb-70" {
		t.Errorf("mongosh on CentOS 7 comes from %s, want psmdb-70", repo)
	}
	// And nothing about CentOS 7 leaks into Oracle Linux 9.
	if _, repo, _ := func() ([]string, string, *scTarball) {
		p := scSysPackages["psql-client"]
		return nil, p.Repo(scOS{"oraclelinux", "9"}, pg17), nil
	}(); repo != "ppg-17" {
		t.Errorf("Oracle Linux 9 psql for 17 comes from %s", repo)
	}
}

// Maven's archive is the same file on every architecture, so its URL has no %s — and Sprintf must
// not be allowed to decorate it.
func TestTarballURLWithoutArchToken(t *testing.T) {
	if got := scMavenUpstream.urlFor("x64"); got != scMavenUpstream.URL {
		t.Errorf("maven url = %s", got)
	}
	if got := scJDKUpstreamEL7.urlFor("x64"); !strings.Contains(got, "jdk-21.0.12.1%2B1/OpenJDK21U-jdk_x64_linux") {
		t.Errorf("temurin url = %s", got)
	}
	for _, e := range scTarballEnv(&scMavenUpstream) {
		if strings.Contains(e, "%!") {
			t.Errorf("maven env carries a Sprintf artefact: %s", e)
		}
	}
}

func TestPMM2Versions(t *testing.T) {
	if pmm2Version("") != pmm2DefaultVersion || pmm2Versions[0] != pmm2DefaultVersion {
		t.Error("an unset version should deploy the last release, which heads the list")
	}
	for _, v := range pmm2Versions {
		if !strings.HasPrefix(v, "2.") || pmm2Version(v) != v {
			t.Errorf("%q is not a PMM 2 release this node resolves", v)
		}
	}
	for _, bad := range []string{"3", "3.3.1", "2", "latest", "2.44"} {
		if pmm2Version(bad) != "" {
			t.Errorf("%q should not resolve to a PMM 2 tag", bad)
		}
	}
	if !strings.Contains(pmm2AdminPasswordScript, "change-admin-password") || !strings.Contains(pmm2AdminPasswordScript, "grafana-cli") {
		t.Error("the password step needs both the helper and the grafana-cli fallback older releases require")
	}
}
