package main

import (
	"strings"
	"testing"
)

// On Debian/Ubuntu the MySQL-family packages split their config across
// `!includedir` drop-ins, and percona-xtradb-cluster-server ships a *populated*
// one: /etc/mysql/mysql.conf.d/mysqld.cnf carries server-id=1,
// wsrep_cluster_name=pxc-cluster, wsrep_node_name=pxc-cluster-node-1 and an empty
// wsrep_cluster_address=gcomm://. DBCanvas wins that at runtime by appending a
// trailing `!include /etc/mysql/dbcanvas.cnf`, and then comments the vendor's copy
// of every option it sets so the two cannot disagree on disk
// (pxcDebianDisableVendorCnf, driven by mysqlCnfOptionKeys).
//
// Verified on the live ubuntu-22.04 PXC stack: with both steps applied,
// `my_print_defaults mysqld client` resolves to a byte-identical effective config
// (only the shadowed duplicates disappear), and the node restarts and rejoins
// Synced with wsrep_node_name=pxc03.

// The keys handed to the disable script must be exactly the ones the config sets —
// disabling anything else would leave a vendor default unset with nothing replacing it.
func TestMySQLCnfOptionKeys(t *testing.T) {
	cnf := "[client]\nsocket=/var/lib/mysql/mysql.sock\n\n" +
		"[mysqld]\n# a comment\nserver-id=50082766\nsocket=/var/lib/mysql/mysql.sock\n" +
		"log-error=/var/log/mysql/error.log\nwsrep_node_name=pxc03\n" +
		"wsrep_provider_options=\"gmcast.listen_addr=tcp://127.0.0.1:14567\"\n" +
		"!include /etc/mysql/other.cnf\n"
	got := mysqlCnfOptionKeys(cnf)
	want := []string{"socket", "server-id", "log-error", "wsrep_node_name", "wsrep_provider_options"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("mysqlCnfOptionKeys = %v, want %v", got, want)
	}
}

// A value may itself contain '=' (wsrep_provider_options) and an option may appear
// in two sections (socket, in [client] and [mysqld]) — neither may produce a bogus
// or duplicated key, since each one is interpolated into a sed expression.
func TestMySQLCnfOptionKeysRejectsNonOptionLines(t *testing.T) {
	for _, line := range []string{"[mysqld]", "# server-id=1", "; server-id=1", "wsrep_log_conflicts", "=orphan", "  "} {
		if got := mysqlCnfOptionKeys(line + "\n"); len(got) != 0 {
			t.Errorf("mysqlCnfOptionKeys(%q) = %v, want none", line, got)
		}
	}
	// Shell/sed metacharacters never reach the script.
	if got := mysqlCnfOptionKeys("foo$(id)=1\nbar;rm=2\ngood_key=3\n"); strings.Join(got, " ") != "good_key" {
		t.Errorf("mysqlCnfOptionKeys kept an unsafe key: %v", got)
	}
}

// The real configs must yield keys covering the vendor settings that actually
// conflict — the PXC identity block above all.
func TestPXCCnfDisablesVendorIdentity(t *testing.T) {
	frame := designFrame{OS: "ubuntu", Label: "pxc-cluster-00", PXCMajor: "8.0", PXCVersion: "8.0.45-36.1", GTID: true}
	keys := mysqlCnfOptionKeys(pxcMyCnf(frame, designNode{}, "pxc03", "example.net", "pxc01.example.net"))
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	// Exactly the options percona-xtradb-cluster-server's mysqld.cnf hardcodes.
	for _, k := range []string{"server-id", "wsrep_node_name", "wsrep_cluster_name", "wsrep_cluster_address",
		"wsrep_provider", "wsrep_sst_method", "pxc_strict_mode", "binlog_format", "innodb_autoinc_lock_mode",
		"datadir", "socket", "log-error", "pid-file"} {
		if !set[k] {
			t.Errorf("pxcMyCnf does not set %q, so the vendor drop-in's copy stays live", k)
		}
	}
}

// Percona Server / MySQL Community ship only pid-file, socket, datadir and
// log-error in their drop-in; the replication config must own all four.
func TestMySQLReplCnfDisablesVendorPaths(t *testing.T) {
	frame := designFrame{OS: "debian", PSMajor: "8.0", PSVersion: "8.0.46-37", GTID: true}
	set := map[string]bool{}
	for _, k := range mysqlCnfOptionKeys(mysqlMyCnf(frame, "ps01")) {
		set[k] = true
	}
	for _, k := range []string{"pid-file", "socket", "datadir", "log-error", "server-id"} {
		if !set[k] {
			t.Errorf("mysqlMyCnf does not set %q", k)
		}
	}
}

// The disable pass only ever runs where the drop-ins exist. On RHEL DBCanvas owns
// /etc/my.cnf outright and there is no includedir to tidy.
func TestDebianCnfLayout(t *testing.T) {
	for _, os := range []string{"ubuntu", "debian"} {
		if pxcCnfPath(os) != "/etc/mysql/dbcanvas.cnf" {
			t.Errorf("pxcCnfPath(%s) = %q", os, pxcCnfPath(os))
		}
	}
	if pxcCnfPath("oraclelinux") != "/etc/my.cnf" {
		t.Errorf("pxcCnfPath(oraclelinux) = %q", pxcCnfPath("oraclelinux"))
	}
	// The include must land in every update-alternatives candidate: my.cnf is a
	// symlink into /etc/alternatives (my.cnf.fallback at 100, the server package's
	// mysql.cnf at 300), so appending only through the link loses the config if a
	// package change switches the alternative.
	if !strings.Contains(pxcDebianIncludeCnf, "update-alternatives --list my.cnf") {
		t.Error("pxcDebianIncludeCnf only edits the selected alternative")
	}
}
