package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- MariaDB

func TestMariaDBMajorOf(t *testing.T) {
	for in, want := range map[string]string{
		"10.6": "10.6", "10.11": "10.11", "11.4": "11.4", "11.8": "11.8",
		"": "11.4", "9.9": "11.4", "5.5": "11.4",
	} {
		if got := mariadbMajorOf(in); got != want {
			t.Errorf("mariadbMajorOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// The GTID domain identifies the write source. Every member of one topology must
// agree on it, and two clusters in one stack must not collide, or a later
// cross-cluster link cannot order their transactions. Domain 0 is the server
// default, so it must never be handed out.
func TestMariaDBGTIDDomainIsStableDistinctAndNonZero(t *testing.T) {
	a, b := mariadbGTIDDomain("repl01"), mariadbGTIDDomain("repl01")
	if a != b {
		t.Errorf("not stable: %d vs %d", a, b)
	}
	if a == mariadbGTIDDomain("repl02") {
		t.Errorf("repl01 and repl02 share domain %d", a)
	}
	for _, l := range []string{"", "a", "repl01", "cluster-with-a-long-label"} {
		if d := mariadbGTIDDomain(l); d <= 0 || d > 65535 {
			t.Errorf("mariadbGTIDDomain(%q) = %d, out of range", l, d)
		}
	}
}

// A '#' in an option-file value starts a comment, so an unquoted password is
// silently truncated and SST fails with a bare "Access denied" far from the cause.
func TestMariaDBOptQuoteProtectsHashInPasswords(t *testing.T) {
	got := mariadbOptQuote("sst:Pa#ss")
	if got != `"sst:Pa#ss"` {
		t.Fatalf("mariadbOptQuote = %s", got)
	}
	if mariadbOptQuote(`a"b`) != `"a\"b"` {
		t.Errorf("quote not escaped: %s", mariadbOptQuote(`a"b`))
	}
	cnf := mariadbGaleraCnf(
		designFrame{OS: "oraclelinux", Label: "gal01"},
		"n1", "example.net", "gcomm://n1",
		pxcSecrets{ClusterUser: "sst", ClusterPassword: "Pa#ss"},
	)
	if !strings.Contains(cnf, `wsrep_sst_auth="sst:Pa#ss"`) {
		t.Errorf("wsrep_sst_auth not quoted:\n%s", cnf)
	}
}

func TestMariaDBGaleraCnfHasRequiredSettings(t *testing.T) {
	cnf := mariadbGaleraCnf(
		designFrame{OS: "oraclelinux", Label: "gal01"},
		"n1", "example.net", "gcomm://n1.example.net,n2.example.net",
		pxcSecrets{ClusterUser: "sst", ClusterPassword: "pw"},
	)
	// Galera cannot certify a transaction that took an auto-increment gap lock and
	// cannot apply anything but full row images.
	for _, want := range []string{
		"wsrep_on=ON",
		"wsrep_provider=/usr/lib64/galera-4/libgalera_smm.so",
		"binlog_format=ROW",
		"innodb_autoinc_lock_mode=2",
		"default_storage_engine=InnoDB",
		"wsrep_sst_method=mariabackup",
		`wsrep_cluster_address="gcomm://n1.example.net,n2.example.net"`,
		`wsrep_node_address="n1.example.net"`,
	} {
		if !strings.Contains(cnf, want) {
			t.Errorf("galera cnf missing %q:\n%s", want, cnf)
		}
	}
	if d := mariadbGaleraCnf(designFrame{OS: "ubuntu", Label: "g"}, "n1", "e.net", "gcomm://", pxcSecrets{}); !strings.Contains(d, "/usr/lib/galera/libgalera_smm.so") {
		t.Errorf("debian galera provider path wrong:\n%s", d)
	}
}

// MariaDB's replication vocabulary differs from MySQL's; using the MySQL spelling
// would fail at CHANGE MASTER time, deep inside a deploy.
func TestMariaDBReplCnfUsesMariaDBGTIDVocabulary(t *testing.T) {
	cnf := mariadbReplCnf("oraclelinux", "n1", 42, 7, true)
	for _, want := range []string{"server-id=42", "gtid_domain_id=7", "gtid_strict_mode=ON", "log_slave_updates=ON", "log_bin=binlog"} {
		if !strings.Contains(cnf, want) {
			t.Errorf("repl cnf missing %q:\n%s", want, cnf)
		}
	}
	// These are MySQL-only and are errors on MariaDB.
	for _, bad := range []string{"gtid_mode", "enforce_gtid_consistency"} {
		if strings.Contains(cnf, bad) {
			t.Errorf("repl cnf contains MySQL-only %q:\n%s", bad, cnf)
		}
	}
	if off := mariadbReplCnf("oraclelinux", "n1", 1, 7, false); strings.Contains(off, "gtid_domain_id") {
		t.Errorf("GTID off should not emit gtid_domain_id:\n%s", off)
	}
}

func TestMariaDBAttachScriptUsesMasterUseGtid(t *testing.T) {
	if !strings.Contains(mariadbAttachScript, "MASTER_USE_GTID = slave_pos") {
		t.Error("attach script does not use MASTER_USE_GTID = slave_pos")
	}
	// MariaDB has neither of these; both would be syntax errors.
	for _, bad := range []string{"SOURCE_AUTO_POSITION", "SET PERSIST", "GET_SOURCE_PUBLIC_KEY"} {
		if strings.Contains(mariadbAttachScript, bad) {
			t.Errorf("attach script contains MySQL-only %q", bad)
		}
	}
}

// SET GLOBAL gtid_slave_pos fails with ERROR 1198 while a slave thread is running,
// which only happens on a redeploy — exactly what a first deploy will not catch.
func TestMariaDBBaselineStopsSlaveBeforeResettingGTID(t *testing.T) {
	stop := strings.Index(mariadbBaselineScript, "STOP SLAVE")
	reset := strings.Index(mariadbBaselineScript, "gtid_slave_pos")
	if stop < 0 || reset < 0 {
		t.Fatalf("baseline missing STOP SLAVE (%d) or gtid_slave_pos (%d)", stop, reset)
	}
	if stop > reset {
		t.Error("baseline resets gtid_slave_pos before STOP SLAVE — ERROR 1198 on redeploy")
	}
}

// An existing-but-empty datadir is not auto-initialized by MariaDB; without this
// the server aborts on missing privilege tables, and under Galera that surfaces as
// a FATAL view-callback error that looks like a clustering fault.
func TestMariaDBDatadirInitGuardsOnPrivilegeStore(t *testing.T) {
	if !strings.Contains(mariadbDatadirInit, "mysql/global_priv.frm") {
		t.Error("datadir init does not test for MariaDB's privilege store")
	}
	if !strings.Contains(mariadbDatadirInit, "mariadb-install-db") {
		t.Error("datadir init never runs mariadb-install-db")
	}
	if !strings.Contains(mariadbGaleraBootstrapScript, "mysql/global_priv.frm") {
		t.Error("galera bootstrap does not initialize its datadir")
	}
	// A joiner is wiped and refilled by SST, so initializing it would be pointless
	// work that mariabackup immediately deletes.
	if strings.Contains(mariadbGaleraJoinScript, "mariadb-install-db") {
		t.Error("galera join should not initialize a datadir that SST overwrites")
	}
}

func TestMariaDBPackagesAndPaths(t *testing.T) {
	el := mariadbServerPackages("oraclelinux", true)
	if !contains(el, "MariaDB-server") || !contains(el, "galera-4") {
		t.Errorf("EL packages wrong: %v", el)
	}
	// The lowercase name on EL is the distro's older AppStream build, not this repo's.
	if contains(el, "mariadb-server") {
		t.Errorf("EL should use the capitalised package: %v", el)
	}
	deb := mariadbServerPackages("ubuntu", false)
	if !contains(deb, "mariadb-server") || contains(deb, "galera-4") {
		t.Errorf("Debian packages wrong: %v", deb)
	}
	// The vendor packages ship a conf.d that /etc/my.cnf includes, so dbcanvas adds a
	// drop-in that must sort last to win.
	dir, base := mariadbCnfDir("oraclelinux")
	if dir != "/etc/my.cnf.d" || !strings.HasPrefix(base, "zz") {
		t.Errorf("EL drop-in %s/%s does not sort last", dir, base)
	}
	if mariadbUnit() != "mariadb" {
		t.Errorf("unit = %q", mariadbUnit())
	}
}

// ---------------------------------------------------------------- MySQL Community

func TestMySQLCEMajorOf(t *testing.T) {
	for in, want := range map[string]string{"8.0": "8.0", "8.4": "8.4", "": "8.4", "5.7": "8.4"} {
		if got := mysqlceMajorOf(in); got != want {
			t.Errorf("mysqlceMajorOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// The shared Percona steps branch on PSMajor to pick per-series SQL; the community
// provisioners must therefore populate it or those steps silently use the default.
func TestMySQLCESyntheticFrameCarriesMajorToSharedSteps(t *testing.T) {
	f := mysqlceSyntheticFrame(designFrame{MySQLCEMajor: "8.0", MySQLCEVersion: "8.0.46-1"})
	if f.PSMajor != "8.0" || f.PSVersion != "8.0.46-1" {
		t.Errorf("synthetic frame = %q/%q, want 8.0/8.0.46-1", f.PSMajor, f.PSVersion)
	}
	if psMajorOf(f.PSMajor) != "8.0" || mysqlResetCmd(f.PSMajor) != "RESET MASTER" {
		t.Error("shared per-series helpers do not agree with the synthetic frame")
	}
	f84 := mysqlceSyntheticFrame(designFrame{MySQLCEMajor: "8.4"})
	if mysqlResetCmd(f84.PSMajor) != "RESET BINARY LOGS AND GTIDS" {
		t.Error("8.4 must use the renamed RESET statement")
	}
}

func TestMySQLCEPackages(t *testing.T) {
	el := mysqlceServerPackages("oraclelinux", true, true)
	for _, want := range []string{"mysql-community-server", "mysql-shell", "mysql-router-community"} {
		if !contains(el, want) {
			t.Errorf("EL packages missing %q: %v", want, el)
		}
	}
	deb := mysqlceServerPackages("ubuntu", false, true)
	if !contains(deb, "mysql-router") || contains(deb, "mysql-router-community") {
		t.Errorf("Debian router package wrong: %v", deb)
	}
	if mysqlceToolsRepo("8.4") != "mysql-tools-8.4-community" || mysqlceToolsRepo("8.0") != "mysql-tools-community" {
		t.Error("tools repo mapping wrong")
	}
}

// The -2023 key file expired 2025-10-22; with it, metadata downloads fine and the
// install then fails the signature check.
func TestMySQLCEUsesUnexpiredSigningKey(t *testing.T) {
	for name, s := range map[string]string{"rhel": mysqlceInstallRHEL, "debian": mysqlceInstallDebian} {
		if !strings.Contains(s, "RPM-GPG-KEY-mysql-2025") {
			t.Errorf("%s install script does not use the 2025 key", name)
		}
		if strings.Contains(s, "RPM-GPG-KEY-mysql-2023") {
			t.Errorf("%s install script still references the expired 2023 key", name)
		}
	}
	// yum path vs apt component are spelled differently for 8.4.
	if !strings.Contains(mysqlceInstallRHEL, "mysql-$MAJOR-community") {
		t.Error("RHEL script does not build the versioned community repo path")
	}
	if !strings.Contains(mysqlceInstallDebian, "mysql-8.4-lts") {
		t.Error("Debian script does not use the -lts component for 8.4")
	}
}

// ---------------------------------------------------------------- shared

// These scripts are assembled in Go and only ever run inside a container, where a
// syntax error surfaces as a deploy failure several steps from its cause. `bash -n`
// parses without executing, so the whole set is cheap to check here.
func TestMariaDBAndMySQLCEScriptsAreValidShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scripts := map[string]string{
		"mariadbInstallRHEL":           mariadbInstallRHEL,
		"mariadbInstallDebian":         mariadbInstallDebian,
		"mariadbBaselineScript":        mariadbBaselineScript,
		"mariadbSemisyncScript":        mariadbSemisyncScript,
		"mariadbAttachScript":          mariadbAttachScript,
		"mariadbGaleraBootstrapScript": mariadbGaleraBootstrapScript,
		"mariadbGaleraJoinScript":      mariadbGaleraJoinScript,
		"mysqlceInstallRHEL":           mysqlceInstallRHEL,
		"mysqlceInstallDebian":         mysqlceInstallDebian,
	}
	dir := t.TempDir()
	for name, body := range scripts {
		p := filepath.Join(dir, name+".sh")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
			t.Errorf("%s is not valid shell: %v\n%s", name, err, out)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- validation

// Availability across the image matrix is uneven in ways the form cannot hint at,
// and an unavailable combination otherwise fails minutes later inside a dnf
// transaction with a mirror error rather than a useful message.
func TestUpstreamVersionIssuesCatchesRealAvailabilityGaps(t *testing.T) {
	if len(loadMariaDBCatalog()) == 0 {
		t.Skip("versions.yaml has no mariadb catalog")
	}
	cases := []struct {
		name                 string
		typ, os, osVer, arch string
		mdMaj, mdVer         string
		ceMaj, ceVer         string
		wantErr              bool
	}{
		// mariadb.org publishes no 10.6 build for EL10 or Ubuntu noble.
		{"mariadb 10.6 on EL10", "mariadb", "oraclelinux", "10", "amd64", "10.6", "", "", "", true},
		{"mariadb 10.6 on noble", "mariadb", "ubuntu", "24.04", "amd64", "10.6", "", "", "", true},
		{"mariadb 10.6 on EL9", "mariadb", "oraclelinux", "9", "amd64", "10.6", "", "", "", false},
		{"mariadb 11.4 on EL10", "mariadbgalera", "oraclelinux", "10", "amd64", "11.4", "", "", "", false},
		// Oracle publishes no MySQL 8.0 repository for EL10.
		{"mysqlce 8.0 on EL10", "mysqlce", "oraclelinux", "10", "amd64", "", "", "8.0", "", true},
		{"mysqlce 8.4 on EL10", "mysqlce", "oraclelinux", "10", "amd64", "", "", "8.4", "", false},
		{"mysqlce 8.0 on EL9", "mysqlcerepl", "oraclelinux", "9", "amd64", "", "", "8.0", "", false},
		// A pinned minor that does not exist must be rejected too.
		{"bogus mariadb minor", "mariadb", "oraclelinux", "9", "amd64", "11.4", "11.4.999-1", "", "", true},
		// An unknown image is the missing-image check's job, not this one.
		{"unknown image", "mariadb", "plan9", "1", "amd64", "11.4", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := upstreamVersionIssues(c.typ, "lbl", c.os, c.osVer, c.arch, c.mdMaj, c.mdVer, c.ceMaj, c.ceVer)
			hasErr := false
			for _, i := range got {
				if i.Level == "error" {
					hasErr = true
				}
			}
			if hasErr != c.wantErr {
				t.Errorf("wantErr=%v got %+v", c.wantErr, got)
			}
		})
	}
}

// An empty minor means "newest available" and must never be reported as missing.
func TestUpstreamVersionIssuesAcceptsBlankMinor(t *testing.T) {
	if len(loadMySQLCECatalog()) == 0 {
		t.Skip("versions.yaml has no mysql_community catalog")
	}
	for _, typ := range []string{"mariadb", "mysqlce", "mariadbrepl", "mysqlceinnodb"} {
		if got := upstreamVersionIssues(typ, "l", "oraclelinux", "9", "amd64", "11.4", "", "8.4", ""); len(got) != 0 {
			t.Errorf("%s: blank minor reported %+v", typ, got)
		}
	}
	// A type outside these families must not be judged by this catalog at all.
	if got := upstreamVersionIssues("ps", "l", "oraclelinux", "9", "amd64", "", "", "", ""); got != nil {
		t.Errorf("non-upstream type returned %+v", got)
	}
}
