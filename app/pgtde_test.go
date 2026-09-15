package main

import (
	"strings"
	"testing"
)

// Percona builds pg_tde for two majors, and the designer, the validator and the deploy all have
// to agree on which. ppgMajorOf's "" → 16 default means an unset major is NOT supported, which is
// the safe direction: the checkbox stays off until a major is chosen deliberately.
func TestPGTDEMajorOK(t *testing.T) {
	for _, tc := range []struct {
		major string
		want  bool
	}{
		{"17", true},
		{"18", true},
		{"16", false},
		{"13", false},
		{"19", false}, // not built yet — do not guess forward
		{"", false},   // unset defaults to 16
	} {
		if got := pgTDEMajorOK(tc.major); got != tc.want {
			t.Errorf("pgTDEMajorOK(%q) = %v, want %v", tc.major, got, tc.want)
		}
	}
}

// The package name differs by distribution family — underscore on RHEL, hyphens on Debian —
// and getting it wrong is an install that fails on a name that looks right.
func TestPGTDEPackage(t *testing.T) {
	if got := pgTDEPackage("oraclelinux", "18"); got != "percona-pg_tde18" {
		t.Errorf("RHEL package = %q", got)
	}
	if got := pgTDEPackage("oraclelinux", "17"); got != "percona-pg_tde17" {
		t.Errorf("RHEL package = %q", got)
	}
	if got := pgTDEPackage("ubuntu", "18"); got != "percona-pg-tde-18" {
		t.Errorf("Debian package = %q", got)
	}
}

// vaultIssues is what stops a design reaching the deploy with encryption asked for on a major
// that has no pg_tde. Both halves are errors and both must fire independently.
func TestVaultIssuesGatesPostgreSQLMajor(t *testing.T) {
	bao := map[string]bool{"bao1": true}
	ok := designNode{Type: "pg", Label: "pg-01", EnableVault: true, OpenBaoNodeID: "bao1", PGMajor: "18"}
	if got := vaultIssues(ok, bao); len(got) != 0 {
		t.Fatalf("PostgreSQL 18 with an OpenBao node is valid, got %q", got[0].Message)
	}
	if got := vaultIssues(designNode{Type: "pg", Label: "pg-01", PGMajor: "16"}, bao); len(got) != 0 {
		t.Errorf("encryption off must raise nothing whatever the major, got %q", got[0].Message)
	}

	old := ok
	old.PGMajor = "16"
	got := vaultIssues(old, bao)
	if len(got) != 1 || got[0].Level != "error" {
		t.Fatalf("PostgreSQL 16 with encryption on must be one error, got %v", got)
	}
	if !strings.Contains(got[0].Message, "pg_tde") || !strings.Contains(got[0].Message, "16") {
		t.Errorf("the error must name pg_tde and the major it found: %q", got[0].Message)
	}

	// Both problems at once: no OpenBao node AND an unsupported major.
	both := designNode{Type: "pg", Label: "pg-01", EnableVault: true, PGMajor: "16"}
	if got := vaultIssues(both, bao); len(got) != 2 {
		t.Errorf("an unlinked node on an unsupported major must raise both errors, got %v", got)
	}
}

// Each node gets its own KV v2 mount — pg_tde speaks no other engine version, and two servers
// writing principal keys into one mount is how you lose both.
func TestVaultMountForPostgreSQL(t *testing.T) {
	mount, kv, version := vaultMountFor(designNode{Type: "pg", PGMajor: "18"}, "pg01")
	if mount != "postgresql-pg01" || kv != "kv-v2" || version != "2" {
		t.Errorf("got %q/%q/%q, want postgresql-pg01/kv-v2/2", mount, kv, version)
	}
}

// The policy pg_tde gets is MySQL's full KV v2 access PLUS read on sys/mounts/<mount>, which is
// where its keyring checks the engine really is KV v2 when a provider is registered.
func TestOpenBaoPolicyForPostgreSQL(t *testing.T) {
	p := openbaoPolicy("postgresql-pg01", "kv-v2", "Percona Distribution for PostgreSQL")
	for _, want := range []string{
		`path "postgresql-pg01/data/*"`,
		`path "postgresql-pg01/metadata/*"`,
		`path "sys/mounts/postgresql-pg01"`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in:\n%s", want, p)
		}
	}
	// The data and metadata trees are both writable: pg_tde creates and rotates its key.
	if strings.Contains(p, `path "postgresql-pg01/metadata/*" {`+"\n"+`  capabilities = ["read"]`) {
		t.Error("PostgreSQL needs a writable metadata tree, not MongoDB's read-only one")
	}
	// sys/mounts is PostgreSQL's alone — nothing else probes it.
	for _, engine := range []string{"Percona Server for MySQL", "Percona Server for MongoDB"} {
		if strings.Contains(openbaoPolicy("m", "kv-v2", engine), "sys/mounts") {
			t.Errorf("%s must not be granted sys/mounts", engine)
		}
	}
}

// The OpenBao node ships one mount per engine + KV version, and PostgreSQL is KV v2 only.
func TestOpenBaoMountsCoverPostgreSQL(t *testing.T) {
	var found bool
	for _, m := range openbaoMounts {
		if m.Path == "postgresql-v2" {
			found = true
			if m.KV != "kv-v2" || m.Version != "2" {
				t.Errorf("the PostgreSQL mount must be KV v2, got %q/%q", m.KV, m.Version)
			}
			if !strings.Contains(m.Engine, "PostgreSQL") {
				t.Errorf("the PostgreSQL mount's engine label is %q", m.Engine)
			}
		}
		if m.Path == "postgresql-v1" {
			t.Error("pg_tde speaks KV v2 only — a v1 PostgreSQL mount is a trap, not an option")
		}
	}
	if !found {
		t.Error("no postgresql-v2 mount in openbaoMounts")
	}
}

// The configure script is where the order-of-operations knowledge lives, and each of these is a
// step whose absence would produce a node that looks encrypted and is not.
func TestPGTDEConfigureScriptDoesTheWholeSequence(t *testing.T) {
	for _, want := range []string{
		"shared_preload_libraries = 'pg_tde'",         // pg_tde needs shared memory
		"CREATE EXTENSION IF NOT EXISTS pg_tde;",      // per database
		"template1",                                   // …so new databases inherit it
		"pg_tde_add_global_key_provider_vault_v2",     // register OpenBao
		"pg_tde_change_global_key_provider_vault_v2",  // …or update it on a redeploy
		"pg_tde_create_key_using_global_key_provider", // the principal key
		"pg_tde_set_default_key_using_global_key_provider",
		"USING tde_heap", // prove it before claiming it
		"pg_tde_is_encrypted",
		"default_table_access_method = 'tde_heap'",
	} {
		if !strings.Contains(pgTDEConfigureScript, want) {
			t.Errorf("the pg_tde configure script never does %q", want)
		}
	}
	// The token is handed over as a PATH; the token itself must never reach a SQL string.
	if !strings.Contains(pgTDEConfigureScript, "chmod 0600 \"$TOKENFILE\"") {
		t.Error("the token file must be 0600 — postgres reads it, nothing else should")
	}
	// ca_path is optional in pg_tde, and "" is not the same as absent.
	if !strings.Contains(pgTDEConfigureScript, `NULLIF(:'ca','')`) {
		t.Error("a plaintext OpenBao node must pass a real NULL ca_path, not an empty string")
	}
	// default_table_access_method has to come after template1 has the extension.
	if strings.Index(pgTDEConfigureScript, "default_table_access_method") <
		strings.Index(pgTDEConfigureScript, "template1") {
		t.Error("default_table_access_method must be set only after template1 carries pg_tde")
	}
}

// A missing pg_tde package is normal below PPG 17.7 (the extension is inside the server package),
// so the install step must test for the extension files rather than for the package.
func TestPGTDEInstallToleratesABundledExtension(t *testing.T) {
	for _, s := range []string{pgTDEInstallRHEL, pgTDEInstallDebian} {
		if !strings.Contains(s, "pg_tde.control") {
			t.Error("the install step must verify pg_tde.control, not just the package")
		}
		if !strings.Contains(s, "ships inside the server package") {
			t.Error("a missing package must be explained in the deploy log, not treated as failure")
		}
	}
}
