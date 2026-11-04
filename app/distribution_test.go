package main

import (
	"strings"
	"testing"
)

func envHas(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestMDBMajorAndKey(t *testing.T) {
	for in, want := range map[string][2]string{
		"":    {"8.0", "8.0"},
		"6.0": {"6.0", "6.0"},
		"7":   {"7.0", "7.0"},
		"8.0": {"8.0", "8.0"},
		"8.2": {"8.2", "8.0"}, // no server-8.2.asc: 8.2 is signed with the 8.0 key
		"8.3": {"8.0", "8.0"}, // a PSMDB-only series falls back to the default
	} {
		if got := mdbMajorOf(in); got != want[0] {
			t.Errorf("mdbMajorOf(%q) = %q, want %q", in, got, want[0])
		}
		if got := mdbKeyMajor(in); got != want[1] {
			t.Errorf("mdbKeyMajor(%q) = %q, want %q", in, got, want[1])
		}
	}
}

func TestMongoInstallStep(t *testing.T) {
	script, env, what := mongoInstallStep(distMongoDB, "oraclelinux", "8.2", "8.2.12", "")
	if !strings.Contains(script, "repo.mongodb.org/yum/redhat") || strings.Contains(script, "percona-release") {
		t.Errorf("community EL install does not use repo.mongodb.org:\n%s", script)
	}
	for _, kv := range []string{"MAJOR=8.2", "KEY=8.0", "PKGS=mongodb-org-server", "VER=8.2.12"} {
		if !envHas(env, kv) {
			t.Errorf("env %v lacks %s", env, kv)
		}
	}
	if !strings.Contains(what, "Community") {
		t.Errorf("what = %q", what)
	}
	// The community package runs mongod as mongodb on Debian; everything downstream chowns to mongod.
	if !strings.Contains(script, "User=mongod") {
		t.Error("community install does not run the service as mongod")
	}
	script, env, _ = mongoInstallStep(distMongoDB, "ubuntu", "8.0", "", "mongos")
	if !strings.Contains(script, "repo.mongodb.org/apt") || !envHas(env, "PKGS=mongodb-org-mongos") {
		t.Errorf("community Debian mongos install: env %v", env)
	}
	// Percona's path is unchanged.
	script, env, _ = mongoInstallStep("", "oraclelinux", "8.0", "", "")
	if script != mongoInstallRHEL || !envHas(env, "PSMDB_REPO=psmdb-80") {
		t.Errorf("Percona install changed: %v", env)
	}
}

func TestPGInstallStep(t *testing.T) {
	pkgs := pgServerPackagesFor(distPGDG, "oraclelinux", "17")
	if strings.Join(pkgs, " ") != "postgresql17-server postgresql17-contrib" {
		t.Errorf("PGDG EL packages = %v", pkgs)
	}
	if got := pgServerPackagesFor(distPGDG, "ubuntu", "17"); strings.Join(got, " ") != "postgresql-17" {
		t.Errorf("PGDG Debian packages = %v", got)
	}
	script, env := pgInstallStep(distPGDG, "oraclelinux", "9", "17", "17.11", pkgs, true)
	if !strings.Contains(script, "pgdg-redhat-repo-latest") || !strings.Contains(script, "-extras") {
		t.Errorf("PGDG EL script lacks the repository or the extras switch")
	}
	// The extras switch must sit before the install, or etcd is not found.
	if strings.Index(script, "-extras") > strings.LastIndex(script, "pin_install $PKGS") {
		t.Error("extras are enabled after the install")
	}
	if !envHas(env, "EXTRAS=1") || strings.Contains(strings.Join(env, " "), "PRODUCT=") {
		t.Errorf("env = %v", env)
	}
	_, env = pgInstallStep(distPGDG, "oraclelinux", "9", "17", "", pkgs, false)
	if envHas(env, "EXTRAS=1") {
		t.Error("extras enabled for a standalone server")
	}
	script, _ = pgInstallStep(distPGDG, "debian", "12", "17", "", pkgs, false)
	if script != repmgrInstallDebian {
		t.Error("PGDG Debian install is not apt.postgresql.org's")
	}
	script, env = pgInstallStep("", "oraclelinux", "9", "17", "", pgServerPackages("oraclelinux", "17"), false)
	if script != patroniInstallRHEL || !envHas(env, "PRODUCT=ppg-17") {
		t.Errorf("Percona install changed: %v", env)
	}
	// Debian's etcd has no gRPC gateway, which Patroni's etcd3 client needs: the upstream
	// release is installed instead, so no distribution etcd package may come in.
	if got := strings.Join(pgdgPatroniPackages("ubuntu"), " "); strings.Contains(got, "etcd") {
		t.Errorf("Debian Patroni packages %q pull the distribution's etcd", got)
	}
	if !strings.Contains(pgdgEtcdUpstreamScript, "github.com/etcd-io/etcd/releases") || !strings.Contains(pgdgEtcdUpstreamScript, "--config-file /etc/etcd/etcd.conf.yaml") {
		t.Error("the upstream etcd install does not fetch the release or start from the YAML config")
	}
	if pgBackRestPackage(distPGDG) != "pgbackrest" || pgBackRestPackage("") != "percona-pgbackrest" {
		t.Error("pgBackRest package names")
	}
}

func TestDistributionIssues(t *testing.T) {
	has := func(out []issue, sub string) bool {
		for _, i := range out {
			if strings.Contains(i.Message, sub) {
				return true
			}
		}
		return false
	}
	if out := distributionIssues("n", "psm", "", distOptions{OIDC: true, Vault: true}); len(out) != 0 {
		t.Errorf("Percona node refused: %v", out)
	}
	out := distributionIssues("n", "psm", distMongoDB, distOptions{OIDC: true, LDAP: true, Kerberos: true, Vault: true})
	for _, want := range []string{"OIDC", "LDAP", "Kerberos", "encryption at rest"} {
		if !has(out, want) {
			t.Errorf("community MongoDB with %s not refused: %v", want, out)
		}
	}
	if out := distributionIssues("n", "psmrs", distMongoDB, distOptions{}); len(out) != 0 {
		t.Errorf("plain MDB replica set refused: %v", out)
	}
	out = distributionIssues("n", "patroni", distPGDG, distOptions{PGTDE: true, PGStatMonitor: true, PGAudit: true, PGSetUser: true, OIDC: true})
	for _, want := range []string{"pg_tde", "pg_stat_monitor", "pgAudit", "set_user", "OIDC"} {
		if !has(out, want) {
			t.Errorf("PGDG with %s not refused: %v", want, out)
		}
	}
	if !has(distributionIssues("n", "pg", distMongoDB, distOptions{}), "MongoDB nodes only") {
		t.Error("community on a PostgreSQL node accepted")
	}
	if !has(distributionIssues("n", "repmgr", distPGDG, distOptions{}), "applies to") {
		t.Error("pgdg on repmgr accepted (it is PGDG already, without the field)")
	}
	if !has(distributionIssues("n", "pg", "mariadb", distOptions{}), "unknown distribution") {
		t.Error("unknown distribution accepted")
	}
}

func TestDistributionVersionGate(t *testing.T) {
	s, ok := nodeDBVersionSpec(designNode{Type: "pg", Distribution: distPGDG, PGMajor: "17"})
	if !ok || s.Section != catPGDG || s.Major != "17" {
		t.Errorf("PGDG node spec = %+v", s)
	}
	s, _ = nodeDBVersionSpec(designNode{Type: "psm", Distribution: distMongoDB, PSMDBMajor: "8.2"})
	if s.Section != catMongoCommunity || s.Major != "8.2" {
		t.Errorf("MDB node spec = %+v", s)
	}
	s, _ = frameDBVersionSpec(designFrame{Type: "psmrs"})
	if s.Section != "percona_server_mongodb" || s.Major != "8.0" {
		t.Errorf("PSMDB frame spec changed: %+v", s)
	}
	s, _ = frameDBVersionSpec(designFrame{Type: "pgstream", Distribution: distPGDG, PGMajor: "18"})
	if s.Section != catPGDG {
		t.Errorf("PGDG frame spec = %+v", s)
	}
}

func TestAIODistributions(t *testing.T) {
	n := designNode{AIOInstances: []aioInstance{
		{Kind: "psmrs", Name: "rs1", Distribution: distMongoDB},
		{Kind: "psmdb", Name: "m1", Distribution: distMongoDB},
	}, AIOMDBMajor: "8.2", AIOMDBVersion: "8.2.12", AIOPSMDBMajor: "7.0"}
	if aioMongoDist(n) != distMongoDB || aioMongoDistConflict(n) != nil {
		t.Errorf("all-MDB node: dist %q conflict %v", aioMongoDist(n), aioMongoDistConflict(n))
	}
	if m, v := aioMongoMajorVersion(n); m != "8.2" || v != "8.2.12" {
		t.Errorf("MDB node installs %s %s, want its MDB pair", m, v)
	}
	n.AIOInstances = append(n.AIOInstances, aioInstance{Kind: "psmdb", Name: "p1"})
	if by := aioMongoDistConflict(n); by == nil || len(by[distPercona]) != 1 || len(by[distMongoDB]) != 2 {
		t.Errorf("mixed node conflict = %v", by)
	}

	pgdg := aioInstance{Kind: "pg", Name: "a", PGMajor: "17", Distribution: distPGDG}
	perc := aioInstance{Kind: "pg", Name: "b", PGMajor: "17"}
	rep := aioInstance{Kind: "repmgr", Name: "c", PGMajor: "17"}
	if aioPGFlavorOf(pgdg) != pgFlavorPGDG || aioPGFlavorOf(perc) != pgFlavorPercona {
		t.Error("pg flavor by distribution")
	}
	// PGDG PostgreSQL and repmgr share a major happily; Percona's does not.
	if c := aioPGFlavorConflicts([]aioInstance{pgdg, rep}); len(c) != 0 {
		t.Errorf("PGDG pg + repmgr on one major conflicted: %v", c)
	}
	if c := aioPGFlavorConflicts([]aioInstance{pgdg, perc}); len(c["17"]) != 2 {
		t.Errorf("PGDG + Percona on one major did not conflict: %v", c)
	}
}
