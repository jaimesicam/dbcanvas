package main

import (
	"strings"
	"testing"
)

// Each shape installs the package from the repository its PostgreSQL came from, named the way
// that repository names it.
func TestPGVectorPackage(t *testing.T) {
	for _, c := range []struct{ os, major, flavor, want string }{
		{"oraclelinux", "17", "ppg", "percona-pgvector_17"},
		{"rocky", "13", "ppg", "percona-pgvector_13"},
		{"ubuntu", "18", "ppg", "percona-postgresql-18-pgvector"},
		{"debian", "16", "ppg", "percona-postgresql-16-pgvector"},
		{"oraclelinux", "17", "pgdg", "pgvector_17"},
		{"ubuntu", "15", "pgdg", "postgresql-15-pgvector"},
		{"oraclelinux", "", "ppg", "percona-pgvector_16"}, // the major's own default
	} {
		if got := pgVectorPackage(c.os, c.major, c.flavor); got != c.want {
			t.Errorf("pgVectorPackage(%q, %q, %q) = %q, want %q", c.os, c.major, c.flavor, got, c.want)
		}
	}
}

// The config records where pgvector came from, per shape.
func TestPGVectorSource(t *testing.T) {
	if got := pgVectorSource("patroni", "oraclelinux", "17"); got != "percona-pgvector_17 (Percona)" {
		t.Errorf("patroni: %q", got)
	}
	if got := pgVectorSource("repmgr", "ubuntu", "17"); got != "postgresql-17-pgvector (PGDG)" {
		t.Errorf("repmgr: %q", got)
	}
	if got := pgVectorSource("spock", "oraclelinux", "17"); !strings.Contains(got, pgVectorRef) {
		t.Errorf("spock must name the tag it compiled: %q", got)
	}
}

// The enable script creates the extension in postgres AND template1 (so later databases have
// it), skips a standby, and never touches shared_preload_libraries — pgvector is not a preload.
func TestPGVectorEnableScript(t *testing.T) {
	s := pgVectorEnableScript
	for _, want := range []string{"for DB in postgres template1", "CREATE EXTENSION IF NOT EXISTS vector;", "pg_is_in_recovery()", `-d "$DB"`} {
		if !strings.Contains(s, want) {
			t.Errorf("enable script is missing %q", want)
		}
	}
	for _, script := range []string{s, pgVectorInstallRHEL, pgVectorInstallDebian, pgVectorBuildScript} {
		if strings.Contains(script, "shared_preload_libraries") {
			t.Error("pgvector is not a preload library; no script should touch shared_preload_libraries")
		}
	}
	for _, script := range []string{pgVectorInstallRHEL, pgVectorInstallDebian, pgVectorBuildScript} {
		if !strings.Contains(script, "extension/vector.control") {
			t.Error("every install path must prove vector.control landed in the server's sharedir")
		}
	}
	if !strings.Contains(pgVectorVersionScript, "extname='vector'") {
		t.Error("the version query must read pg_extension for vector")
	}
}

// The Spock build compiles against the frame's own pg_config, at the pinned tag.
func TestPGVectorBuildScript(t *testing.T) {
	for _, want := range []string{`--branch "$PGVECTOR_REF"`, "https://github.com/pgvector/pgvector", `PG_CONFIG="$PREFIX/bin/pg_config"`} {
		if !strings.Contains(pgVectorBuildScript, want) {
			t.Errorf("build script is missing %q", want)
		}
	}
}

// The operator gate: 2.6.0 and newer; an unknown version is not "newest".
func TestPGHasPGVector(t *testing.T) {
	for v, want := range map[string]bool{"2.6.0": true, "2.9.0": true, "3.1.0": true, "2.5.1": false, "2.3.0": false, "": false} {
		if got := pgHasPGVector(v); got != want {
			t.Errorf("pgHasPGVector(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestK3DPgVectorIssues(t *testing.T) {
	cat := OperatorCatalog{"pg": {Latest: "3.1.0", Versions: []string{"3.1.0", "2.6.0", "2.5.1"}}}

	// Off → silent, whatever else.
	off := designFrame{Type: "k3d", Label: "k3d-00", K3DOperator: "pg", K3DOperatorVer: "2.5.1"}
	if iss := k3dPgVectorIssues(off, cat, false); len(iss) != 0 {
		t.Fatalf("pgvector off must be silent, got %v", iss)
	}

	// Below 2.6.0 → error naming both versions.
	old := off
	old.K3DPgVector = true
	iss := k3dPgVectorIssues(old, cat, false)
	if len(iss) != 1 || iss[0].Level != "error" {
		t.Fatalf("pgvector on 2.5.1 must be an error, got %v", iss)
	}
	if !strings.Contains(iss[0].Message, "2.5.1") || !strings.Contains(iss[0].Message, pgVectorK3DMinVer) {
		t.Errorf("the error must name both versions: %q", iss[0].Message)
	}
	// …a warning once the frame is already running (nothing re-applies its cr.yaml).
	if iss := k3dPgVectorIssues(old, cat, true); len(iss) != 1 || iss[0].Level != "warning" {
		t.Errorf("a running frame must only be warned, got %v", iss)
	}

	// 2.6.0, and a blank version (the catalog's latest, 3.1.0), are fine.
	for _, v := range []string{"2.6.0", ""} {
		ok := old
		ok.K3DOperatorVer = v
		if iss := k3dPgVectorIssues(ok, cat, false); len(iss) != 0 {
			t.Errorf("operator %q must accept pgvector, got %v", v, iss)
		}
	}

	// A version the catalog cannot resolve → error.
	unknown := old
	unknown.K3DOperatorVer = "9.9.9"
	if iss := k3dPgVectorIssues(unknown, cat, false); len(iss) != 1 || iss[0].Level != "error" {
		t.Errorf("an unknown operator version must be an error, got %v", iss)
	}

	// CloudNativePG and Crunchy PGO images carry pgvector: accepted, whatever the version.
	for _, op := range []string{"cnpg", "pgo"} {
		o := old
		o.K3DOperator = op
		if iss := k3dPgVectorIssues(o, cat, false); len(iss) != 0 {
			t.Errorf("pgvector on %s must be accepted, got %v", op, iss)
		}
	}

	// A non-PostgreSQL operator → a warning that it is ignored.
	ps := old
	ps.K3DOperator = "psmdb"
	if iss := k3dPgVectorIssues(ps, cat, false); len(iss) != 1 || iss[0].Level != "warning" || !strings.Contains(iss[0].Message, "ignored") {
		t.Errorf("pgvector on a non-PostgreSQL operator must warn it is ignored, got %v", iss)
	}
}
