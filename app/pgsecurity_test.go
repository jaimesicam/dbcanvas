package main

import (
	"strings"
	"testing"
)

// One shared_preload_libraries value carries everything a node preloads: a second line would
// silently replace the first, and the server would start without pgAudit and say nothing.
func TestPGSecurityPreloadJoinsQueryAnalytics(t *testing.T) {
	exts := pgSecurityExtensions(true, "", true)
	if got := pgPreloadValue(pgExtPreload(exts), pgQSMonitor); got != "pgaudit,set_user,pg_stat_statements,pg_stat_monitor" {
		t.Errorf("preload = %q", got)
	}
	// pg_tde's step rewrites the line; it must keep the extensions.
	if got := pgPreloadValue(append([]string{"pg_tde"}, pgExtPreload(exts)...), pgQSNone); got != "pg_tde,pgaudit,set_user" {
		t.Errorf("preload with pg_tde = %q", got)
	}
	if got := pgPreloadValue(pgExtPreload(nil), pgQSNone); got != "" {
		t.Errorf("no extensions, no analytics → no preload line, got %q", got)
	}
}

func TestPGAuditLogDefaultAndConf(t *testing.T) {
	lines := strings.Join(pgExtConfLines(pgSecurityExtensions(true, "", false)), "\n")
	if !strings.Contains(lines, "pgaudit.log = 'ddl,role'") {
		t.Errorf("default log classes missing:\n%s", lines)
	}
	lines = strings.Join(pgExtConfLines(pgSecurityExtensions(true, " write,ddl ", false)), "\n")
	if !strings.Contains(lines, "pgaudit.log = 'write,ddl'") {
		t.Errorf("chosen log classes missing:\n%s", lines)
	}
}

// A class pgAudit does not know stops the server at preload, so it is a validation error.
func TestPGAuditLogIssue(t *testing.T) {
	for _, ok := range []string{"", "ddl,role", "all,-misc", "READ, write", "misc_set"} {
		if why := pgAuditLogIssue(ok); why != "" {
			t.Errorf("%q rejected: %s", ok, why)
		}
	}
	for _, bad := range []string{"ddl;drop", "everything", "ddl,,role"} {
		if pgAuditLogIssue(bad) == "" {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The packages exist for every PPG major and both OS families; what validation must still
// catch is a design that asks for one where the table says no.
func TestPGExtensionIssuesUsesComponentTable(t *testing.T) {
	if is := pgExtensionIssues("PostgreSQL node pg", "ubuntu", "13", "", true, "", true); len(is) != 0 {
		t.Errorf("pgaudit + set_user on 13/ubuntu: %v", is)
	}
	if is := pgExtensionIssues("PostgreSQL node pg", "oraclelinux", "12", "", true, "", false); len(is) != 1 {
		t.Errorf("pgaudit on 12 should be refused, got %v", is)
	}
}

func TestPGSecurityPackages(t *testing.T) {
	for _, tc := range []struct{ comp, os, major, want string }{
		{"pgaudit", "oraclelinux", "17", "percona-pgaudit17"},
		{"pgaudit", "ubuntu", "17", "percona-postgresql-17-pgaudit"},
		{"set_user", "oraclelinux", "18", "percona-pgaudit18_set_user"},
		{"set_user", "ubuntu", "18", "percona-pgaudit18-set-user"},
	} {
		c, _ := ppgComponentByID(tc.comp)
		if got := c.Package(tc.os, tc.major); got != tc.want {
			t.Errorf("%s on %s %s = %q, want %q", tc.comp, tc.os, tc.major, got, tc.want)
		}
	}
}

func TestPGBenchIssues(t *testing.T) {
	if is := pgBenchIssues("n", 0); len(is) != 0 {
		t.Errorf("0 = none, got %v", is)
	}
	if is := pgBenchIssues("n", 50); len(is) != 0 {
		t.Errorf("50: %v", is)
	}
	if is := pgBenchIssues("n", 500); len(is) != 1 || is[0].Level != "warning" {
		t.Errorf("500 should warn, got %v", is)
	}
	for _, bad := range []int{-1, 1001} {
		if is := pgBenchIssues("n", bad); len(is) != 1 || is[0].Level != "error" {
			t.Errorf("%d should be an error, got %v", bad, is)
		}
	}
}
