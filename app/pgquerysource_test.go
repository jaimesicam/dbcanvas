package main

import (
	"strings"
	"testing"
)

// The standalone node and Patroni treat the extension as a PMM option; repmgr and
// Spock offer pg_stat_statements on its own and never pg_stat_monitor.
func TestPGQuerySourceFor(t *testing.T) {
	cases := []struct {
		typ, spec, pmm, want string
	}{
		{"pg", "pgstatements", "pmm1", pgQSStatements},
		{"pg", "pgstatmonitor", "pmm1", pgQSMonitor},
		{"pg", "pgstatmonitor", "", pgQSNone}, // PMM unselected → the option is moot
		{"patroni", "pg_stat_monitor", "pmm1", pgQSMonitor},
		{"patroni", "", "pmm1", pgQSNone},
		{"repmgr", "pgstatements", "", pgQSStatements},
		{"repmgr", "pgstatmonitor", "pmm1", pgQSNone},
		{"spock", "pgstatements", "pmm1", pgQSStatements},
		{"spock", "", "pmm1", pgQSNone},
	}
	for _, c := range cases {
		if got := pgQuerySourceFor(c.typ, c.spec, c.pmm); got != c.want {
			t.Errorf("pgQuerySourceFor(%q, %q, %q) = %q, want %q", c.typ, c.spec, c.pmm, got, c.want)
		}
	}
}

// One shared_preload_libraries value carrying everything: a second line would win.
func TestPGPreloadValue(t *testing.T) {
	if got := pgPreloadValue([]string{"repmgr"}, pgQSStatements); got != "repmgr,pg_stat_statements" {
		t.Errorf("repmgr + pg_stat_statements = %q", got)
	}
	if got := pgPreloadValue([]string{"pg_tde"}, pgQSMonitor); got != "pg_tde,pg_stat_statements,pg_stat_monitor" {
		t.Errorf("pg_tde + pg_stat_monitor = %q", got)
	}
	if got := pgPreloadValue([]string{"spock"}, pgQSNone); got != "spock" {
		t.Errorf("spock alone = %q", got)
	}
}

func TestPGQueryPMMFlags(t *testing.T) {
	if pgQueryPMMFlags(pgQSNone) != "" {
		t.Error("no extension must leave pmm-admin's command line as it was")
	}
	if pgQueryPMMFlags(pgQSMonitor) != "--query-source=pgstatmonitor" {
		t.Error("pg_stat_monitor must be named to pmm-admin")
	}
}

func TestPGQuerySourceIssues(t *testing.T) {
	if len(pgQuerySourceIssues("x", "patroni", "pgstatmonitor")) != 0 {
		t.Error("pg_stat_monitor is valid on Patroni")
	}
	if len(pgQuerySourceIssues("x", "spock", "pgstatmonitor")) == 0 {
		t.Error("pg_stat_monitor on Spock must be an error")
	}
	if len(pgQuerySourceIssues("x", "pg", "bogus")) == 0 {
		t.Error("an unknown value must be named")
	}
}

func TestPatroniYAMLCarriesQuerySource(t *testing.T) {
	f := designFrame{Type: "patroni", Label: "c1", PGMajor: "17", PMMNodeID: "pmm1", PGQuerySource: "pgstatmonitor"}
	y := patroniYAML(f, "h", "h.example.net", []string{"h:2379"}, pgSecrets{SuperUser: "postgres", ReplUser: "replicator"})
	for _, want := range []string{
		`shared_preload_libraries: "pg_stat_statements,pg_stat_monitor"`,
		`pg_stat_monitor.pgsm_enable_query_plan: "off"`,
	} {
		if !strings.Contains(y, want) {
			t.Errorf("patroni.yml is missing %q", want)
		}
	}
	f.PMMNodeID = ""
	if strings.Contains(patroniYAML(f, "h", "h.example.net", nil, pgSecrets{}), "shared_preload_libraries") {
		t.Error("without PMM the Patroni frame must not preload anything")
	}
}
