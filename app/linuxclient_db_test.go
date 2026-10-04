package main

import (
	"slices"
	"strings"
	"testing"
)

// What each release can install was probed on that release (see linuxclient_db.go). The two that
// differ are the ones a design is most likely to get wrong: Debian 13 has no 8.0 client, and
// CentOS 7 stops at 8.0, psql 13, and no ClusterSync.
func TestLCDBClientSeries(t *testing.T) {
	if got := lcMySQLSeries("debian", "13"); slices.Contains(got, "8.0") {
		t.Errorf("Debian 13 has no 8.0 percona-server-client: %v", got)
	}
	if got := lcMySQLSeries("debian", "12"); !slices.Equal(got, []string{"8.0", "8.4", "9.7"}) {
		t.Errorf("Debian 12 series = %v", got)
	}
	if got := lcMySQLSeries("centos", "7"); !slices.Equal(got, []string{"8.0"}) {
		t.Errorf("CentOS 7 series = %v, want only 8.0", got)
	}
	if got := lcPsqlSeries("centos"); !slices.Equal(got, []string{"13"}) {
		t.Errorf("CentOS 7 psql = %v, want only 13", got)
	}
	if lcClusterSyncOn("centos") || !lcClusterSyncOn("oraclelinux") {
		t.Error("ClusterSync is published for everything but CentOS 7")
	}
	// A node that picked nothing gets a default its release carries.
	for _, n := range []designNode{
		{OS: "oraclelinux", OSVersion: "9"}, {OS: "debian", OSVersion: "13"}, {OS: "centos", OSVersion: "7"},
	} {
		if m := lcMySQLMajor(n); !slices.Contains(lcMySQLSeries(n.OS, n.OSVersion), m) {
			t.Errorf("%s %s: default MySQL series %s is not offered", n.OS, n.OSVersion, m)
		}
		if m := lcPsqlMajor(n); !slices.Contains(lcPsqlSeries(n.OS), m) {
			t.Errorf("%s %s: default psql %s is not offered", n.OS, n.OSVersion, m)
		}
	}
}

func TestLCDBClientIssues(t *testing.T) {
	ok := designNode{Label: "lc", OS: "ubuntu", OSVersion: "24.04", LCMySQLClient: true, LCMySQLShell: true,
		LCMySQLMajor: "8.0", LCMongosh: true, LCPsql: true, LCPsqlMajor: "18", LCClusterSync: true}
	if is := lcDBClientIssues(ok); len(is) != 0 {
		t.Errorf("a valid design was refused: %v", is)
	}
	bad := designNode{Label: "lc", OS: "centos", OSVersion: "7", LCMySQLShell: true, LCMySQLMajor: "8.4",
		LCPsql: true, LCPsqlMajor: "17", LCClusterSync: true}
	is := lcDBClientIssues(bad)
	if len(is) != 3 {
		t.Fatalf("want three issues for CentOS 7, got %v", is)
	}
	for _, i := range is {
		if i.Level != "error" || !strings.Contains(i.Message, "CentOS 7") {
			t.Errorf("issue %+v must be an error naming the release", i)
		}
	}
	// A series that is not picked is not checked.
	if is := lcDBClientIssues(designNode{OS: "debian", OSVersion: "13", LCMySQLMajor: "8.0"}); len(is) != 0 {
		t.Errorf("no MySQL tool chosen, nothing to refuse: %v", is)
	}
}

func TestLCDBSteps(t *testing.T) {
	steps := lcDBSteps(designNode{OS: "oraclelinux", OSVersion: "9", LCMySQLClient: true, LCMySQLShell: true,
		LCMySQLMajor: "8.4", LCMongosh: true, LCPsql: true, LCPsqlMajor: "16", LCClusterSync: true})
	if len(steps) != 4 {
		t.Fatalf("want one step per repository family, got %d", len(steps))
	}
	if !slices.Equal(steps[0].Packages, []string{"percona-server-client", "percona-mysql-shell"}) || steps[0].Repos[0] != "ps-84-lts" {
		t.Errorf("MySQL step = %+v", steps[0])
	}
	if steps[2].Packages[0] != "percona-postgresql16" || steps[2].Repos[0] != "ppg-16" {
		t.Errorf("EL psql step = %+v", steps[2])
	}
	if steps[3].Packages[0] != "percona-clustersync-mongodb" || steps[3].Repos[0] != "pcsm" {
		t.Errorf("ClusterSync step = %+v", steps[3])
	}
	// Debian names the psql client package differently.
	deb := lcDBSteps(designNode{OS: "debian", OSVersion: "12", LCPsql: true})
	if deb[0].Packages[0] != "percona-postgresql-client-17" {
		t.Errorf("Debian psql package = %v", deb[0].Packages)
	}
	// 9.7 is the series percona-release cannot enable; it goes through the hand-written repository.
	s97 := lcDBSteps(designNode{OS: "ubuntu", OSVersion: "24.04", LCMySQLClient: true, LCMySQLMajor: "9.7"})
	if s97[0].HandRepo != "ps-97-lts" || len(s97[0].Repos) != 0 {
		t.Errorf("9.7 step = %+v", s97[0])
	}
	// CentOS 7's mongosh comes from psmdb-70.
	if r := lcDBSteps(designNode{OS: "centos", OSVersion: "7", LCMongosh: true})[0].Repos[0]; r != "psmdb-70" {
		t.Errorf("CentOS 7 mongosh repo = %s", r)
	}
	if len(lcDBSteps(designNode{OS: "oraclelinux"})) != 0 {
		t.Error("nothing chosen, nothing to install")
	}
}

// The repositories are enabled, never set up: setup disables every other Percona repository, so a
// second step would undo the first.
func TestLCDBScripts(t *testing.T) {
	for name, s := range map[string]string{"RHEL": lcDBInstallRHEL, "EL7": lcDBInstallEL7, "Debian": lcDBInstallDebian} {
		if !strings.Contains(s, `percona-release enable "$r"`) {
			t.Errorf("%s does not enable repositories additively", name)
		}
		if !strings.Contains(s, `$PKGS`) || !strings.Contains(s, `if [ -n "$PROXY" ]`) {
			t.Errorf("%s does not install $PKGS through the proxy", name)
		}
	}
	if !strings.Contains(lcDBInstallRHEL, "module disable mysql postgresql") {
		t.Error("EL8's AppStream modules hide the Percona packages")
	}
	for _, tool := range []lcDBTool{lcToolMySQL, lcToolMySQLShell, lcToolMongosh, lcToolPsql, lcToolClusterSync} {
		if lcDBVersionCmd[tool.ID] == "" {
			t.Errorf("no way to read back %s", tool.ID)
		}
	}
}
