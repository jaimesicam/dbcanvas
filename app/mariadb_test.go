package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// The first deploy gives root a password, which switches root@localhost off
// unix_socket auth — so a redeploy's `mariadb -uroot` is rejected with ERROR 1045
// and the whole step dies before it starts. Found by re-running a baseline against
// an already-provisioned node, which a first deploy cannot exercise.
func TestMariaDBScriptsSurviveARedeploy(t *testing.T) {
	for name, s := range map[string]string{
		"baseline":  mariadbBaselineScript,
		"bootstrap": mariadbGaleraBootstrapScript,
		"join":      mariadbGaleraJoinScript,
	} {
		if !strings.Contains(s, "mdb_root()") {
			t.Errorf("%s does not define the auth-probing client", name)
		}
	}
	// A bare `mariadb -uroot` outside the probe would be the 1045 path again. The
	// helper's own definition is the only legitimate occurrence.
	body := strings.Replace(mariadbBaselineScript, mdbRootClient, "", 1)
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "mariadb -uroot ") && !strings.Contains(l, "-p") {
			t.Errorf("baseline uses unauthenticated root outside mdb_root: %s", l)
		}
	}
}

// MariaDB refuses an option file whose first line is a bare option. Because the
// drop-in directory is read by the client too, a malformed file breaks every later
// mariadb invocation on the node — not just the server setting it was meant to fix.
func TestMariaDBReadOnlyDropInHasSectionHeader(t *testing.T) {
	i := strings.Index(mariadbAttachScript, "zz-dbcanvas-readonly.cnf")
	if i < 0 {
		t.Fatal("attach script no longer writes the read_only drop-in")
	}
	if !strings.Contains(mariadbAttachScript, `printf '[mysqld]\nread_only=ON\n'`) {
		t.Error("read_only drop-in is written without a [mysqld] group header")
	}
}

// MariaDB 10.5 split the old REPLICATION CLIENT privilege in two: BINLOG MONITOR
// (SHOW BINLOG STATUS) and SLAVE MONITOR (SHOW SLAVE STATUS). Granting the MySQL
// spelling maps to BINLOG MONITOR *only*, so Orchestrator's very first probe fails
// with "Access denied; you need (at least one of) the SLAVE MONITOR privilege(s)"
// and the cluster never gets discovered. Found by pointing a real Orchestrator at
// a real MariaDB pair.
func TestMariaDBGrantsIncludeSlaveMonitor(t *testing.T) {
	for _, user := range []string{"$ORCH_USER", "$MON_USER"} {
		i := strings.Index(mariadbRootSQL, "GRANT")
		if i < 0 {
			t.Fatal("no grants in the baseline SQL")
		}
		// Find the GRANT line that names this user on *.*.
		var line string
		for _, l := range strings.Split(mariadbRootSQL, "\n") {
			if strings.HasPrefix(l, "GRANT ") && strings.Contains(l, "ON *.* TO '"+user+"'") {
				line = l
				break
			}
		}
		if line == "" {
			t.Fatalf("no *.* GRANT for %s", user)
		}
		if !strings.Contains(line, "SLAVE MONITOR") {
			t.Errorf("%s cannot run SHOW SLAVE STATUS on MariaDB 10.5+: %s", user, line)
		}
		if !strings.Contains(line, "BINLOG MONITOR") {
			t.Errorf("%s is missing BINLOG MONITOR: %s", user, line)
		}
		// The MySQL spelling silently degrades to BINLOG MONITOR alone, so it must
		// not be relied on here.
		if strings.Contains(line, "REPLICATION CLIENT") {
			t.Errorf("%s uses the MySQL-only REPLICATION CLIENT spelling: %s", user, line)
		}
	}
}

// Orchestrator manages classic source/replica topologies. Galera and Group
// Replication elect their own primary, so there is nothing for it to fail over.
func TestOrchestratableFramesAreReplicationOnly(t *testing.T) {
	for _, want := range []string{"mysql", "mariadbrepl", "mysqlcerepl", "pxc"} {
		if !orchestratableFrame(want) {
			t.Errorf("%s should be Orchestrator-manageable", want)
		}
	}
	for _, no := range []string{"mariadbgalera", "mysqlceinnodb", "innodb", "psmdb", "patroni", ""} {
		if orchestratableFrame(no) {
			t.Errorf("%s should not be offered to Orchestrator", no)
		}
	}
	// Every MySQL-family frame must clear the shared baseline barrier, including
	// the cluster kinds Orchestrator does not manage.
	for _, want := range []string{"pxc", "mysql", "innodb", "mariadbrepl", "mariadbgalera", "mysqlcerepl", "mysqlceinnodb"} {
		if !mysqlFamilyFrame(want) {
			t.Errorf("%s missing from the MySQL-family barrier set", want)
		}
	}
	if mysqlFamilyFrame("patroni") || mysqlFamilyFrame("") {
		t.Error("non-MySQL frame types must not join the barrier")
	}
}

// ---------------------------------------------------------------- All-in-One

// All four MySQL flavors are mutually exclusive inside one container, and every
// kind must map to exactly one flavor and one shape or the provisioner silently
// treats it as "not MySQL family".
func TestAIOFlavorAndShapeCoverEveryMySQLKind(t *testing.T) {
	want := map[string][2]string{
		"ps":            {flavorPS, shapeSingle},
		"psrepl":        {flavorPS, shapeRepl},
		"innodb":        {flavorPS, shapeGR},
		"pxc":           {flavorPXC, shapeGalera},
		"mysqlce":       {flavorMySQLCE, shapeSingle},
		"mysqlcerepl":   {flavorMySQLCE, shapeRepl},
		"mysqlceinnodb": {flavorMySQLCE, shapeGR},
		"mariadb":       {flavorMariaDB, shapeSingle},
		"mariadbrepl":   {flavorMariaDB, shapeRepl},
		"mariadbgalera": {flavorMariaDB, shapeGalera},
	}
	for kind, w := range want {
		if got := aioMySQLFlavorOfKind(kind); got != w[0] {
			t.Errorf("%s flavor = %q, want %q", kind, got, w[0])
		}
		if got := aioMySQLShape(kind); got != w[1] {
			t.Errorf("%s shape = %q, want %q", kind, got, w[1])
		}
	}
	// Every MySQL-family kind in the catalog must be covered above, or a newly
	// added kind would fall through every switch without a compile error.
	for _, k := range aioKinds {
		if k.Family != famMySQL {
			continue
		}
		if _, ok := want[k.Kind]; !ok {
			t.Errorf("MySQL-family kind %q has no flavor/shape mapping", k.Kind)
		}
	}
	// Non-MySQL kinds must map to neither.
	for _, k := range []string{"pg", "psmdb", "valkey", "haproxy", ""} {
		if aioMySQLFlavorOfKind(k) != flavorNone || aioMySQLShape(k) != shapeNone {
			t.Errorf("%q should not be MySQL-family", k)
		}
	}
}

func TestAIOMySQLFlavorConflictIsNWay(t *testing.T) {
	mk := func(kinds ...string) []aioInstance {
		var out []aioInstance
		for i, k := range kinds {
			out = append(out, aioInstance{ID: string(rune('a' + i)), Kind: k, Name: k + "01"})
		}
		return out
	}
	// Any two distinct flavors collide — not just the original PS/PXC pair.
	for _, pair := range [][]string{
		{"ps", "pxc"}, {"ps", "mariadb"}, {"ps", "mysqlce"},
		{"mariadb", "mysqlce"}, {"pxc", "mariadbgalera"}, {"mysqlceinnodb", "innodb"},
	} {
		if _, conflict := aioMySQLFlavor(mk(pair...)); !conflict {
			t.Errorf("%v should conflict", pair)
		}
	}
	// Same flavor, different shapes: fine, one install serves them all.
	for _, set := range [][]string{
		{"ps", "psrepl", "innodb"},
		{"mariadb", "mariadbrepl", "mariadbgalera"},
		{"mysqlce", "mysqlcerepl", "mysqlceinnodb"},
	} {
		f, conflict := aioMySQLFlavor(mk(set...))
		if conflict {
			t.Errorf("%v should not conflict", set)
		}
		if f != aioMySQLFlavorOfKind(set[0]) {
			t.Errorf("%v resolved to %q", set, f)
		}
	}
	if f, c := aioMySQLFlavor(mk("pg", "valkey")); f != flavorNone || c {
		t.Errorf("non-MySQL instances resolved to %q/%v", f, c)
	}
}

// Each flavor keeps its own version fields: a version string carried across a
// flavor switch would silently mean a different product's numbering.
func TestAIOFlavorVersionReadsThePerFlavorFields(t *testing.T) {
	n := designNode{
		AIOPSMajor: "8.0", AIOPSVersion: "8.0.46-37.1",
		AIOPXCMajor: "8.4", AIOPXCVersion: "8.4.5-5.1",
		AIOMariaDBMajor: "11.4", AIOMariaDBVersion: "11.4.11",
		AIOMySQLCEMajor: "8.0", AIOMySQLCEVersion: "8.0.46-1",
	}
	for flavor, want := range map[string][2]string{
		flavorPS:      {"8.0", "8.0.46-37.1"},
		flavorPXC:     {"8.4", "8.4.5-5.1"},
		flavorMariaDB: {"11.4", "11.4.11"},
		flavorMySQLCE: {"8.0", "8.0.46-1"},
	} {
		maj, ver := aioFlavorVersion(n, flavor)
		if maj != want[0] || ver != want[1] {
			t.Errorf("%s → %q/%q, want %q/%q", flavor, maj, ver, want[0], want[1])
		}
	}
}

// The AiO MariaDB config must speak MariaDB, not MySQL: the MySQL-only keys are
// unknown variables there and the server refuses to start.
func TestAIOMariaDBCnfUsesMariaDBDialect(t *testing.T) {
	m := aioInstanceRuntime{Inst: "mariadb01", Kind: "mariadb", Group: "", Ports: aioPortsFor("mariadb", 0, 0)}
	l := aioLayout(m.Inst, m.Kind, m.Ports)
	cnf := aioMySQLCnf(l, m, designNode{AIOInstances: []aioInstance{{Kind: "mariadb", Name: "mariadb01", GTID: true}}}, "11.4", "")
	for _, bad := range []string{"gtid_mode", "enforce_gtid_consistency", "mysqlx_port", "mysqlx_socket"} {
		if strings.Contains(cnf, bad) {
			t.Errorf("MariaDB config contains MySQL-only %q:\n%s", bad, cnf)
		}
	}
	for _, want := range []string{"gtid_domain_id=", "gtid_strict_mode=ON", "log_slave_updates=ON"} {
		if !strings.Contains(cnf, want) {
			t.Errorf("MariaDB config missing %q:\n%s", want, cnf)
		}
	}
	// The Percona path must be unchanged.
	pm := aioInstanceRuntime{Inst: "ps01", Kind: "ps", Ports: aioPortsFor("ps", 0, 0)}
	ps := aioMySQLCnf(aioLayout(pm.Inst, pm.Kind, pm.Ports), pm, designNode{AIOInstances: []aioInstance{{Kind: "ps", Name: "ps01", GTID: true}}}, "8.0", "")
	if !strings.Contains(ps, "gtid_mode=ON") || !strings.Contains(ps, "mysqlx_port=") {
		t.Errorf("Percona Server config changed:\n%s", ps)
	}
}

// A MariaDB Galera member in a shared container must pin every wsrep listener into
// its own port slot, and quote the SST credentials.
func TestAIOMariaDBGaleraSettingsPinPortsAndQuoteAuth(t *testing.T) {
	members := []aioInstanceRuntime{
		{Inst: "gal01-n1", Kind: "mariadbgalera", Ports: aioPortsFor("mariadbgalera", 0, 0)},
		{Inst: "gal01-n2", Kind: "mariadbgalera", Ports: aioPortsFor("mariadbgalera", 0, 1)},
	}
	s := aioMariaDBGaleraSettings(members[0], designNode{OS: "oraclelinux"}, "gal01", members)
	for _, want := range []string{
		"wsrep_on=ON", "wsrep_sst_method=mariabackup",
		"/usr/lib64/galera-4/libgalera_smm.so", "innodb_autoinc_lock_mode=2",
		fmt.Sprintf("gmcast.listen_addr=tcp://127.0.0.1:%d", members[0].Ports.Group),
		fmt.Sprintf("ist.recv_addr=127.0.0.1:%d", members[0].Ports.IST),
	} {
		if !strings.Contains(s, want) {
			t.Errorf("galera settings missing %q:\n%s", want, s)
		}
	}
	// wsrep_sst_auth must be quoted — '#' in a password would otherwise truncate it.
	if !strings.Contains(s, `wsrep_sst_auth="`) {
		t.Errorf("wsrep_sst_auth is not quoted:\n%s", s)
	}
	// Both members must appear in the gcomm list, on their own group ports.
	for _, m := range members {
		if !strings.Contains(s, fmt.Sprintf("127.0.0.1:%d", m.Ports.Group)) {
			t.Errorf("gcomm list omits %s:\n%s", m.Inst, s)
		}
	}
}

// The MariaDB AiO scripts must speak MariaDB and stay re-runnable.
func TestAIOMariaDBScriptsDialect(t *testing.T) {
	if !strings.Contains(aioMariaDBInitScript, "mariadb-install-db") ||
		!strings.Contains(aioMariaDBInitScript, "mysql/global_priv.frm") {
		t.Error("AiO MariaDB init does not use mariadb-install-db guarded on the privilege store")
	}
	if strings.Contains(aioMariaDBBaselineScript, "validate_password") {
		t.Error("MariaDB ships no validate_password component to relax")
	}
	if !strings.Contains(aioMariaDBBaselineScript, "SLAVE MONITOR") {
		t.Error("MariaDB accounts need SLAVE MONITOR for SHOW SLAVE STATUS")
	}
	stop := strings.Index(aioMariaDBBaselineScript, "STOP SLAVE")
	reset := strings.Index(aioMariaDBBaselineScript, "gtid_slave_pos")
	if stop < 0 || reset < 0 || stop > reset {
		t.Error("baseline must STOP SLAVE before clearing gtid_slave_pos (ERROR 1198 on redeploy)")
	}
	if !strings.Contains(aioMariaDBAttachScript, "MASTER_USE_GTID = slave_pos") {
		t.Error("attach does not use MariaDB auto-positioning")
	}
	for _, bad := range []string{"SOURCE_AUTO_POSITION", "SET PERSIST"} {
		if strings.Contains(aioMariaDBAttachScript, bad) {
			t.Errorf("attach contains MySQL-only %q", bad)
		}
	}
	// read_only drop-ins need a group header or MariaDB rejects the whole file.
	for name, s := range map[string]string{"attach": aioMariaDBAttachScript, "semisync": aioMariaDBSemisyncScript} {
		if strings.Contains(s, "printf") && !strings.Contains(s, "[mysqld]") {
			t.Errorf("%s writes a drop-in without a [mysqld] header", name)
		}
	}
}

// The Galera start wrapper is shared by both flavors and must launch the right
// daemon; --wsrep-new-cluster is the same flag for mariadbd and mysqld.
func TestAIOGaleraStartWrapperPicksTheDaemon(t *testing.T) {
	l := aioLayout("gal01-n1", "mariadbgalera", aioPortsFor("mariadbgalera", 0, 0))
	md := aioGaleraStartWrapper(l, true, "/usr/sbin/mariadbd")
	if !strings.Contains(md, "exec /usr/sbin/mariadbd") {
		t.Errorf("MariaDB wrapper does not exec mariadbd:\n%s", md)
	}
	if !strings.Contains(md, "--wsrep-new-cluster") || !strings.Contains(md, "safe_to_bootstrap") {
		t.Error("wrapper lost its bootstrap logic")
	}
	if px := aioGaleraStartWrapper(l, true, "/usr/sbin/mysqld"); !strings.Contains(px, "exec /usr/sbin/mysqld") {
		t.Errorf("PXC wrapper does not exec mysqld:\n%s", px)
	}
}

// mariadb-install-db creates anonymous ”@'localhost' and ”@'<hostname>' accounts.
// They are MORE host-specific than a '%' grant, so a connection made over localhost
// matches them first and fails with a misleading "Access denied for user
// 'repl'@'localhost'" — even though repl@'%' exists with the right password.
//
// Found by running an All-in-One MariaDB replication pair, where both servers live in
// one container and the replica therefore dials 127.0.0.1. The classic node path
// attaches over an FQDN, so it never hit this.
func TestMariaDBBaselinesDropAnonymousUsers(t *testing.T) {
	for name, s := range map[string]string{
		"classic": mariadbRootSQL,
		"aio":     aioMariaDBBaselineScript,
	} {
		if !strings.Contains(s, "DELETE FROM mysql.global_priv WHERE User=''") {
			t.Errorf("%s baseline does not remove the anonymous accounts", name)
		}
		// The removal must precede the accounts it would otherwise shadow.
		del := strings.Index(s, "WHERE User=''")
		repl := strings.Index(s, "'$REPL_USER'@'%'")
		if del < 0 || repl < 0 || del > repl {
			t.Errorf("%s: anonymous cleanup must come before the real grants", name)
		}
	}
	// Neither init should leave the sample `test` database behind either.
	for name, s := range map[string]string{"classic": mariadbDatadirInit, "aio": aioMariaDBInitScript} {
		if !strings.Contains(s, "--skip-test-db") {
			t.Errorf("%s init does not pass --skip-test-db", name)
		}
	}
}

// MySQL Shell 8.0 needs interactive:false on dba.configureInstance() or it prompts
// "[y/n]" and hangs on a TTY-less exec. Shell 8.4 REMOVED the option and rejects it
// with "Invalid options: interactive (ArgumentError)", failing every attempt of the
// retry loop. Found deploying a MySQL Community InnoDB Cluster, whose Shell is 8.4.
func TestInnoDBShellOptionsAreVersionSelected(t *testing.T) {
	s := innodbShellClusterScript
	if !strings.Contains(s, "SHVER=") || !strings.Contains(s, "mysqlsh --version") {
		t.Error("the Shell option set is not derived from the shell's own version")
	}
	if !strings.Contains(s, `CFGOPT="{interactive:false, restart:false}"`) {
		t.Error("no 8.0 branch: Shell 8.0 would hang on the configure wizard")
	}
	if !strings.Contains(s, `CFGOPT="{restart:false}"`) {
		t.Error("no 8.4 branch: Shell 8.4 rejects the interactive option")
	}
	// Nothing may pass `interactive` literally any more — that is the 8.4 failure.
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "configureInstance") && strings.Contains(line, "interactive:false") &&
			!strings.Contains(line, "CFGOPT=") {
			t.Errorf("configureInstance still hardcodes the option: %s", strings.TrimSpace(line))
		}
	}
	// Both call sites must use the selected set.
	if n := strings.Count(s, "configureInstance"); n != strings.Count(s, "$CFGOPT")+1 {
		t.Errorf("configureInstance calls (%d) do not all use $CFGOPT", n)
	}
}

// mysqlceContainer is shared by the replication and InnoDB prepare paths, which
// record different config shapes (mysqlConfig vs innodbConfig). It must patch the
// stored config generically: round-tripping through either struct drops every field
// the other has, which is how the deployed InnoDB members lost bootstrap, router,
// groupName and the Router port pair while still reporting success.
func TestMySQLCEContainerDoesNotRoundTripThroughOneConfigStruct(t *testing.T) {
	src, err := os.ReadFile("mysqlce.go")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(src), "func (a *App) mysqlceContainer(")
	if i < 0 {
		t.Fatal("mysqlceContainer not found")
	}
	body := string(src)[i:]
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}
	for _, bad := range []string{"var cfg mysqlConfig", "var cfg innodbConfig"} {
		if strings.Contains(body, bad) {
			t.Errorf("mysqlceContainer decodes into a concrete config struct (%q) and will drop the other shape's fields", bad)
		}
	}
	if !strings.Contains(body, "cfgMap") {
		t.Error("mysqlceContainer should patch the stored config through a map")
	}
}

// Spock is not a package — spock.go compiles a patched PostgreSQL from source, and
// the patch set exists only for some majors. The All-in-One form was offering the
// Percona PostgreSQL list (13-18) for Spock instances, advertising 13 and 14, which
// cannot be built. The catalog itself was already correct; only the consumer was wrong.
func TestSpockMajorsExcludeUnsupportedSeries(t *testing.T) {
	if len(loadSpockCatalog()) == 0 {
		t.Skip("versions.yaml has no spock catalog")
	}
	majors := aioSpockMajors("oraclelinux", "9", "amd64")
	if len(majors) == 0 {
		t.Fatal("no Spock majors for oraclelinux 9")
	}
	for _, m := range majors {
		if n := pgMajorNum(m); n < 15 {
			t.Errorf("Spock must not offer PostgreSQL %s — supported series start at 15 (got %v)", m, majors)
		}
	}
	// Newest first, so a picker's default lands on the newest supported major.
	for i := 1; i < len(majors); i++ {
		if pgMajorNum(majors[i-1]) < pgMajorNum(majors[i]) {
			t.Errorf("Spock majors not ordered newest-first: %v", majors)
		}
	}
	// The Percona PostgreSQL catalog DOES carry 13/14 — which is exactly why Spock
	// must not be driven from it.
	ppg := map[string]bool{}
	for _, im := range loadPPGCatalog() {
		if im.OS == "oraclelinux" && im.OSVersion == "9" && im.Arch == "amd64" {
			for m, vs := range im.Versions {
				if len(vs) > 0 {
					ppg[m] = true
				}
			}
		}
	}
	if len(ppg) > 0 && !ppg["13"] && !ppg["14"] {
		t.Skip("PPG catalog no longer carries 13/14; the distinction this guards is moot")
	}
	for _, old := range []string{"13", "14"} {
		if slices.Contains(majors, old) {
			t.Errorf("Spock majors leaked PPG-only series %s", old)
		}
	}
}

// A design that names an unsupported major for Spock must be refused before deploy,
// or it fails minutes into a source build with a compiler error.
func TestAIORejectsSpockOnUnsupportedMajor(t *testing.T) {
	if len(loadSpockCatalog()) == 0 {
		t.Skip("versions.yaml has no spock catalog")
	}
	n := designNode{
		Label: "aio1", OS: "oraclelinux", OSVersion: "9", Arch: "amd64",
		AIOInstances: []aioInstance{{ID: "s", Kind: "spock", Name: "spock01", Members: 2, PGMajor: "13"}},
	}
	found := false
	for _, is := range aioIssues(n, designDoc{Nodes: []designNode{n}}, map[int][]string{}, 0) {
		if is.Level == "error" && strings.Contains(is.Message, "Spock does not support") {
			found = true
			if !strings.Contains(is.Message, "spock01") || !strings.Contains(is.Message, "13") {
				t.Errorf("message should name the instance and the bad major: %s", is.Message)
			}
		}
	}
	if !found {
		t.Error("PostgreSQL 13 accepted for a Spock instance")
	}
	// A supported major must pass.
	ok := aioSpockMajors("oraclelinux", "9", "amd64")[0]
	n.AIOInstances[0].PGMajor = ok
	for _, is := range aioIssues(n, designDoc{Nodes: []designNode{n}}, map[int][]string{}, 0) {
		if is.Level == "error" && strings.Contains(is.Message, "Spock does not support") {
			t.Errorf("supported major %s was rejected: %s", ok, is.Message)
		}
	}
}

// ---------------------------------------------------------------- AiO PMM + web

// `pmm-admin config` re-registers the node and DROPS every service already added.
// Running it per instance meant each one silently deleted its predecessors: a node
// with eleven monitored instances ended up with one in PMM while the deploy log
// said "11 instance(s) registered with PMM".
func TestAIOPMMConfiguresTheAgentOncePerNode(t *testing.T) {
	if strings.Contains(aioPMMAddScript, "pmm-admin config") {
		t.Error("the per-instance script still reconfigures the agent — it will wipe earlier services")
	}
	if !strings.Contains(aioPMMSetupScript, "pmm-admin config") {
		t.Error("the node-level setup script never configures the agent")
	}
	// The setup must not reconfigure an already-registered agent either, or an
	// incremental redeploy wipes the instances it is not re-adding.
	if !strings.Contains(aioPMMSetupScript, "pmm-admin status") {
		t.Error("setup does not check for an already-registered agent before reconfiguring")
	}
	// And it must fail loudly rather than leave the adds to fail one by one.
	if !strings.Contains(aioPMMSetupScript, "did not register") {
		t.Error("setup does not verify the agent actually registered")
	}
}

// Only the client port used to be published, so an instance's web UI was
// unreachable from the host even with the export ticked.
func TestAIOPublishesWebPortsAlongsideTheClientPort(t *testing.T) {
	cases := map[string]struct {
		label     string
		wantExtra bool
	}{
		"orchestrator": {"Orchestrator", false}, // its UI *is* the client port
		"haproxy":      {"HAProxy stats", true}, // stats sits on admin (+2)
		"patroni":      {"Patroni REST", true},  // REST sits on +1
	}
	for kind, c := range cases {
		p := aioPortsFor(kind, 0, 0)
		eps := aioWebEndpoints(kind, p)
		if len(eps) == 0 {
			t.Errorf("%s should expose a web endpoint", kind)
			continue
		}
		if eps[0].Label != c.label {
			t.Errorf("%s label = %q, want %q", kind, eps[0].Label, c.label)
		}
		if eps[0].Port == 0 {
			t.Errorf("%s web endpoint has no port", kind)
		}
		pub := aioPublishPorts(kind, p)
		if pub[0] != p.Client {
			t.Errorf("%s: the client port must stay first (it keeps the requested host port)", kind)
		}
		if got := len(pub) > 1; got != c.wantExtra {
			t.Errorf("%s: extra published port = %v, want %v (%v)", kind, got, c.wantExtra, pub)
		}
		for _, cp := range pub {
			if cp < p.Base || cp >= p.Base+aioSlotWidth {
				t.Errorf("%s publishes %d, outside its slot %d..%d", kind, cp, p.Base, p.Base+aioSlotWidth-1)
			}
		}
	}
	// A kind with no HTTP interface publishes exactly its client port.
	for _, kind := range []string{"ps", "mariadb", "psmdb", "valkey"} {
		if eps := aioWebEndpoints(kind, aioPortsFor(kind, 0, 0)); len(eps) != 0 {
			t.Errorf("%s should have no web endpoint, got %v", kind, eps)
		}
		if pub := aioPublishPorts(kind, aioPortsFor(kind, 0, 0)); len(pub) != 1 {
			t.Errorf("%s should publish one port, got %v", kind, pub)
		}
	}
}

// The plan must carry the web endpoints, or the manager has nothing to link to.
func TestAIOPlanRecordsWebEndpoints(t *testing.T) {
	n := designNode{
		Label: "aio1", OS: "oraclelinux", OSVersion: "9", Arch: "amd64",
		AIOInstances: []aioInstance{{ID: "o", Kind: "orchestrator", Name: "orch01", ExportEnabled: true}},
	}
	plan := aioPlan(n, "example.net", "aio1")
	if len(plan) != 1 {
		t.Fatalf("plan = %d members", len(plan))
	}
	m := plan[0]
	if len(m.Web) != 1 || m.Web[0].Port != m.Ports.Client {
		t.Fatalf("orchestrator web endpoint not recorded: %+v", m.Web)
	}
	if !m.ExportOn {
		t.Error("export not carried into the plan")
	}
	// HostPort stays 0 until the container exists — that is what the manager keys on.
	if m.Web[0].HostPort != 0 {
		t.Errorf("HostPort should be unresolved in the plan, got %d", m.Web[0].HostPort)
	}
}

// The form names the web interface on the export toggle, so the Go catalog and the
// JS table cannot drift about which kinds have one.
func TestAIOWebKindsMatchTheJSTable(t *testing.T) {
	js, err := os.ReadFile("web/src/pages/AllInOne.jsx")
	if err != nil {
		t.Skip("AllInOne.jsx not readable")
	}
	block := string(js)
	i := strings.Index(block, "const WEB_KINDS = {")
	if i < 0 {
		t.Fatal("WEB_KINDS table not found in AllInOne.jsx")
	}
	block = block[i:]
	block = block[:strings.Index(block, "}")]
	for _, k := range aioKinds {
		hasGo := len(aioWebEndpoints(k.Kind, aioPortsFor(k.Kind, 0, 0))) > 0
		hasJS := strings.Contains(block, k.Kind+":")
		if hasGo != hasJS {
			t.Errorf("%s: Go web endpoint=%v, JS WEB_KINDS=%v — the two must agree", k.Kind, hasGo, hasJS)
		}
	}
}

// A control that provably does nothing must not be offered. PMM ships an exporter
// for every kind here except Orchestrator, which has no PMM service type at all.
func TestAIOPMMOfferedOnlyWhereItWorks(t *testing.T) {
	for _, kind := range []string{
		"ps", "mysqlce", "mariadb", "psrepl", "innodb", "pxc",
		"pg", "patroni", "repmgr", "spock", "psmdb", "psmrs",
		"valkey", "valkeycluster", "proxysql", "haproxy",
	} {
		if !aioPMMSupported(kind) {
			t.Errorf("%s should support PMM", kind)
		}
	}
	if aioPMMSupported("orchestrator") {
		t.Error("PMM has no Orchestrator service type — the option must not be offered")
	}
	// Each kind maps to the right `pmm-admin add <type>` sub-command.
	for kind, want := range map[string]string{
		"mysqlce": "mysql", "mariadbgalera": "mysql", "patroni": "postgresql",
		"psmrs": "mongodb", "valkeycluster": "valkey", "proxysql": "proxysql",
		"haproxy": "haproxy", "orchestrator": "",
	} {
		if got := aioPMMServiceType(kind); got != want {
			t.Errorf("%s service type = %q, want %q", kind, got, want)
		}
	}
	// Setting it on Orchestrator is reported as a warning: the instance is fine, and
	// the node's OS metrics are collected either way.
	warn := func(in aioInstance) string {
		n := designNode{ID: "n1", Type: "aio", Label: "aio1", OS: "oraclelinux", OSVersion: "9", Arch: "amd64", AIOInstances: []aioInstance{in}}
		doc := designDoc{Nodes: []designNode{n, {ID: "pmm1", Type: "pmm", Label: "pmm"}}}
		for _, is := range aioIssues(n, doc, map[int][]string{}, 0) {
			if strings.Contains(is.Message, "not monitored as a service") {
				if is.Level != "warning" {
					t.Errorf("should be a warning, got %q", is.Level)
				}
				return is.Message
			}
		}
		return ""
	}
	if m := warn(aioInstance{ID: "a", Kind: "orchestrator", Name: "orch01", PMMNodeID: "pmm1"}); m == "" {
		t.Error("PMM on an Orchestrator instance not reported")
	} else if strings.Contains(m, "dedicated") {
		t.Errorf("must not suggest a dedicated node — Orchestrator has no PMM exporter anywhere: %s", m)
	}
	// Everything else is registered, so nothing to warn about.
	for _, k := range []string{"ps", "valkey", "proxysql", "haproxy"} {
		if m := warn(aioInstance{ID: "a", Kind: k, Name: k + "01", PMMNodeID: "pmm1", Members: 1}); m != "" {
			t.Errorf("%s is monitored now and must not be flagged: %s", k, m)
		}
	}
}

// Two exporters do NOT scrape the client port: ProxySQL's is read over its admin
// interface and HAProxy's over its stats listener. Passing the client port would
// register a service that never reports.
func TestAIOPMMTargetUsesTheRightPortPerKind(t *testing.T) {
	dep := Deployment{Secrets: []byte(`{"clusterUser":"cl","clusterPassword":"clpw","adminUser":"admin","adminPassword":"apw"}`)}
	for _, tc := range []struct {
		kind string
		want func(aioPorts) int
	}{
		{"proxysql", func(p aioPorts) int { return p.Admin }},
		{"haproxy", func(p aioPorts) int { return p.Admin }},
		{"valkey", func(p aioPorts) int { return p.Client }},
		{"ps", func(p aioPorts) int { return p.Client }},
	} {
		ports := aioPortsFor(tc.kind, 0, 0)
		m := aioInstanceRuntime{Inst: tc.kind + "01", Kind: tc.kind, Ports: ports}
		got, _, _ := aioPMMTarget(dep, aioInstance{Kind: tc.kind}, m)
		if want := tc.want(ports); got != want {
			t.Errorf("%s: PMM port = %d, want %d (client=%d admin=%d)", tc.kind, got, want, ports.Client, ports.Admin)
		}
	}
	// A clustered Valkey member is tagged so PMM groups its shards.
	if arg := aioValkeyClusterArg(aioInstanceRuntime{Kind: "valkeycluster", Group: "vk01"}); arg != "--cluster=vk01" {
		t.Errorf("cluster arg = %q", arg)
	}
	if arg := aioValkeyClusterArg(aioInstanceRuntime{Kind: "valkey"}); arg != "" {
		t.Errorf("a standalone Valkey must not be tagged as a cluster: %q", arg)
	}
}

// The registration path and the form must agree on which kinds are offered PMM, or
// the picker promises something aioRegisterPMM will skip.
func TestAIOPMMFormGateMatchesTheRegistrationPath(t *testing.T) {
	js, err := os.ReadFile("web/src/pages/AllInOne.jsx")
	if err != nil {
		t.Skip("AllInOne.jsx not readable")
	}
	// The form gates the PMM picker on the kind, not a hardcoded family list, so it
	// cannot drift from aioPMMSupported.
	if !strings.Contains(string(js), "PMM_KINDS") {
		t.Error("the form should gate the PMM picker on a kind table, not an inline family list")
	}
	// Orchestrator is the one kind that must be absent from it.
	i := strings.Index(string(js), "const PMM_KINDS")
	if i < 0 {
		t.Fatal("PMM_KINDS table not found")
	}
	block := string(js)[i:]
	block = block[:strings.Index(block, "]")]
	for _, k := range aioKinds {
		inJS := strings.Contains(block, "'"+k.Kind+"'")
		if inJS != aioPMMSupported(k.Kind) {
			t.Errorf("%s: JS PMM_KINDS=%v, aioPMMSupported=%v", k.Kind, inJS, aioPMMSupported(k.Kind))
		}
	}
}

// Every instance in an All-in-One container shares the container's hostname, so
// without report_host they all announce themselves identically and are told apart
// only by port: SHOW SLAVE HOSTS reports "localhost" for each replica, and
// Orchestrator lists six servers all called aio-01.
func TestAIOMySQLCnfAnnouncesTheInstancesOwnName(t *testing.T) {
	for _, kind := range []string{"mariadbrepl", "mysqlcerepl", "psrepl", "ps"} {
		ports := aioPortsFor(kind, 0, 1)
		m := aioInstanceRuntime{
			Inst: "cluster01-n2", Kind: kind, Group: "cluster01",
			FQDN: "cluster01-n2.example.net", Ports: ports,
		}
		n := designNode{AIOInstances: []aioInstance{{Kind: kind, Name: "cluster01", GTID: true}}}
		cnf := aioMySQLCnf(aioLayout(m.Inst, m.Kind, ports), m, n, "11.4", "")
		if !strings.Contains(cnf, "report_host=cluster01-n2.example.net") {
			t.Errorf("%s: config does not announce the instance's own name:\n%s", kind, cnf)
		}
		if !strings.Contains(cnf, fmt.Sprintf("report_port=%d", ports.Client)) {
			t.Errorf("%s: config does not announce the instance's own port", kind)
		}
	}
}

// Orchestrator names a cluster after its master's host:port unless an alias is
// detected, so two All-in-One clusters render as "aio-01:13000" and "aio-01:13030".
// The alias query recovers the name the user typed from report_host.
func TestAIOOrchClusterAliasQueryRecoversTheClusterName(t *testing.T) {
	q := aioOrchClusterAliasQuery
	if !strings.Contains(q, "@@report_host") {
		t.Error("the alias must be derived from report_host")
	}
	// SUBSTRING_INDEX(x,'-n',1) would truncate a cluster whose own name contains
	// "-n"; the suffix must be stripped with an anchored pattern instead.
	if strings.Contains(q, "'-n', 1") || strings.Contains(q, `"-n", 1`) {
		t.Error("stripping the member suffix with SUBSTRING_INDEX truncates names containing '-n'")
	}
	if !strings.Contains(q, "-n[0-9]+$") {
		t.Error("the member suffix should be removed with an end-anchored pattern")
	}
	// The AiO member naming this relies on must not drift.
	if got := aioMemberInst("mariadbrepl-cluster-01", "mariadbrepl", 1, 3); got != "mariadbrepl-cluster-01-n2" {
		t.Errorf("member naming changed to %q — the alias query's suffix pattern assumes -n<N>", got)
	}
	if got := aioMemberInst("mariadb01", "mariadb", 0, 1); got != "mariadb01" {
		t.Errorf("a standalone must have no member suffix, got %q", got)
	}
}

// Replicas must record the primary's own alias, not 127.0.0.1. Every instance in an
// All-in-One container shares loopback, so anything resolving Master_Host — which
// Orchestrator does, to one host per address string — collapses every cluster in the
// node onto a single master. Seen live: cluster-01's replicas were displayed as
// replicas of cluster-02's primary.
func TestAIOReplicasRecordTheirOwnPrimary(t *testing.T) {
	p := aioInstanceRuntime{Inst: "c01-n1", FQDN: "c01-n1.example.net", Ports: aioPortsFor("mariadbrepl", 0, 0)}
	if got := aioSourceHost(p); got != "c01-n1.example.net" {
		t.Errorf("source host = %q, want the primary's alias", got)
	}
	// Two primaries in one node must not share an address.
	q := aioInstanceRuntime{Inst: "c02-n1", FQDN: "c02-n1.example.net"}
	if aioSourceHost(p) == aioSourceHost(q) {
		t.Error("two clusters' primaries resolved to the same address")
	}
	if got := aioSourceHost(aioInstanceRuntime{Inst: "x"}); got != "127.0.0.1" {
		t.Errorf("fallback = %q, want loopback", got)
	}
	// Neither attach script may hardcode loopback any more.
	for name, s := range map[string]string{"mysql": aioMySQLAttachScript, "mariadb": aioMariaDBAttachScript} {
		if strings.Contains(s, "'127.0.0.1'") {
			t.Errorf("%s attach script still hardcodes 127.0.0.1 as the source", name)
		}
		if !strings.Contains(s, "$SOURCE_HOST") {
			t.Errorf("%s attach script does not use $SOURCE_HOST", name)
		}
	}
}

// MariaDB 10.5 split SUPER into fine-grained privileges. Orchestrator needs three of
// them and each was found the hard way: SLAVE MONITOR for SHOW SLAVE STATUS,
// BINLOG MONITOR for SHOW BINLOG STATUS, and REPLICATION MASTER ADMIN for SHOW SLAVE
// HOSTS — which only surfaced once report_host made that call meaningful.
func TestMariaDBOrchestratorGrantCoversEveryProbe(t *testing.T) {
	want := []string{"SLAVE MONITOR", "BINLOG MONITOR", "REPLICATION MASTER ADMIN"}
	for name, sql := range map[string]string{
		"classic": mariadbRootSQL,
		"aio":     aioMariaDBBaselineScript,
	} {
		var line string
		for _, l := range strings.Split(sql, "\n") {
			if strings.HasPrefix(l, "GRANT ") && strings.Contains(l, "'$ORCH_USER'@'%'") {
				line = l
			}
		}
		if line == "" {
			t.Errorf("%s: no orchestrator GRANT found", name)
			continue
		}
		for _, p := range want {
			if !strings.Contains(line, p) {
				t.Errorf("%s: orchestrator grant lacks %s — %s", name, p, line)
			}
		}
	}
}

// Orchestrator resolves its web templates relative to the cwd, so its unit must set
// one. Without it every /web/ page 500s with "templates/layout is undefined" while
// the API answers normally — the AiO Orchestrator UI never worked.
func TestAIOOrchestratorUnitSetsItsWorkingDirectory(t *testing.T) {
	l := aioLayout("orch01", "orchestrator", aioPortsFor("orchestrator", 0, 0))
	unit := aioUnitFile(l, aioUnitSpec{
		Description: "x", ExecStart: "/usr/local/orchestrator/orchestrator http",
		WorkingDirectory: "/usr/local/orchestrator", Type: "simple", User: "root", Group: "root",
	})
	if !strings.Contains(unit, "WorkingDirectory=/usr/local/orchestrator") {
		t.Errorf("unit does not set WorkingDirectory:\n%s", unit)
	}
	// An instance that does not need one must not get an empty directive.
	plain := aioUnitFile(l, aioUnitSpec{Description: "x", ExecStart: "/bin/true", Type: "simple", User: "root", Group: "root"})
	if strings.Contains(plain, "WorkingDirectory=") {
		t.Errorf("empty WorkingDirectory should be omitted:\n%s", plain)
	}
}
