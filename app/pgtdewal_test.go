package main

import (
	"strings"
	"testing"
)

// The wrapped commands are Percona's, with one level of %% escaping for postgresql.conf (and
// Patroni, which writes it): PostgreSQL expands the outer %f %p, the wrapper the inner ones.
func TestPGTDEWALCommands(t *testing.T) {
	if got := pgTDEArchiveCommand("/usr/pgsql-17/bin", "pg-01"); got != `/usr/pgsql-17/bin/pg_tde_archive_decrypt %f %p "pgbackrest --stanza=pg-01 archive-push %%p"` {
		t.Errorf("archive_command = %s", got)
	}
	if got := pgTDERestoreCommand("/usr/pgsql-17/bin", "pg-01"); got != `/usr/pgsql-17/bin/pg_tde_restore_encrypt %f %p "pgbackrest --stanza=pg-01 archive-get %%f %%p"` {
		t.Errorf("restore_command = %s", got)
	}
	// pgbackrest restore writes its own restore_command unless it is handed this one.
	if a := pgRestoreTDEArgs("/usr/pgsql-17/bin", "pg-01"); len(a) != 1 || !strings.HasPrefix(a[0], "--recovery-option=restore_command=/usr/pgsql-17/bin/pg_tde_restore_encrypt ") {
		t.Errorf("restore args = %v", a)
	}
}

// pgBackRest can archive through the wrapper only with its header and page checks off.
func TestPGTDEWALBackRestConf(t *testing.T) {
	with := patroniPgBackRestConf("pg-01", "oraclelinux", "17", seaweedConfig{Bucket: "b"}, seaweedSecrets{}, true)
	without := patroniPgBackRestConf("pg-01", "oraclelinux", "17", seaweedConfig{Bucket: "b"}, seaweedSecrets{}, false)
	for _, o := range pgTDEBackRestOptions {
		if !strings.Contains(with, o) {
			t.Errorf("WAL-encrypting stanza lacks %s", o)
		}
		if strings.Contains(without, o) {
			t.Errorf("a plain stanza must not carry %s", o)
		}
	}
}

// Patroni archives through the wrapper from bootstrap, and gives every member the wrapped
// restore_command for the WAL a replica replays.
func TestPatroniYAMLWALEncryption(t *testing.T) {
	f := designFrame{Label: "c1", Type: "patroni", OS: "oraclelinux", PGMajor: "17", EnableVault: true, PGTDEWal: true, UsePgBackRest: true}
	y := patroniYAML(f, "patroni-1", "patroni-1.example.net", []string{"patroni-1.example.net:2379"}, pgSecrets{})
	for _, want := range []string{
		"archive_command: '" + pgTDEArchiveCommand("/usr/pgsql-17/bin", "c1") + "'",
		"restore_command: '" + pgTDERestoreCommand("/usr/pgsql-17/bin", "c1") + "'",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("patroni.yml lacks %s", want)
		}
	}
	if issues := pgTDEWALIssues("c", true, false); len(issues) != 1 {
		t.Error("WAL encryption without table encryption must be refused")
	}
}

// Patroni on Ubuntu could not form a cluster at all: etcd's YAML went to /etc/default/etcd, which
// Percona's etcd-server unit no longer reads, so each member ran a one-node etcd on 127.0.0.1 —
// and the quorum check, which only asked the local endpoint for health, passed anyway. Then the
// package's own default cluster sat in Patroni's data_dir on every member.
func TestPatroniOnUbuntu(t *testing.T) {
	if etcdConfPath("ubuntu") != "/etc/etcd/etcd.conf.yaml" || etcdConfPath("oraclelinux") != "/etc/etcd/etcd.conf.yaml" {
		t.Error("etcd reads /etc/etcd/etcd.conf.yaml on every OS")
	}
	if !strings.Contains(patroniEtcdHealthScript, "member list") || !strings.Contains(patroniEtcdHealthScript, "$MEMBERS") {
		t.Error("the etcd check must count started members, not only ask the local endpoint for health")
	}
	if !strings.Contains(patroniPrepDirsScript, `[ -d "/etc/postgresql/$MAJOR/main" ]`) || !strings.Contains(patroniPrepDirsScript, "pg_dropcluster --stop") {
		t.Error("the package's default Debian cluster must be dropped — and only when /etc/postgresql says it is the package's")
	}
}

// repmgr on Ubuntu: the standbys need the primary's configuration applied (Debian keeps it
// outside the data directory a clone replaces), and a failed repmgr step must fail the deploy.
func TestRepmgrOnUbuntu(t *testing.T) {
	env := strings.Join(repmgrConfEnv(designFrame{Type: "repmgr", Label: "r1"}, "/etc/postgresql/17/main", "/var/lib/postgresql/17/main", pgSecrets{ReplUser: "repmgr"}, seaweedConfig{}), "\n")
	for _, want := range []string{"CONFDIR=/etc/postgresql/17/main", "REPLUSER=repmgr", "PRELOAD=repmgr"} {
		if !strings.Contains(env, want) {
			t.Errorf("repmgrConfEnv lacks %s", want)
		}
	}
	for name, s := range map[string]string{"clone": repmgrStandbyCloneScript, "standby register": repmgrStandbyRegisterScript, "primary register": repmgrPrimaryRegisterScript} {
		if strings.Contains(s, "| tail") && !strings.Contains(s, "pipefail") {
			t.Errorf("%s pipes into tail without pipefail — a repmgr failure would read as success", name)
		}
	}
}
