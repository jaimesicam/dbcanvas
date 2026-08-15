package main

// logsummary_postgres_test.go — PostgreSQL, streaming replication and Patroni.
//
// The three fixtures are three different shapes of the same engine, and the difference
// between them is the point:
//
//	p01-patroni-cluster  a live three-node Patroni cluster on PostgreSQL 16.14, driven
//	                     through a planned switchover, an unplanned failover with the
//	                     leader SIGKILLed, and a whole-DCS outage. Each member's file is
//	                     its PostgreSQL log with its Patroni journal appended, which is
//	                     exactly what the collector produces.
//	p02-streaming        a three-node streaming setup with NO cluster manager running:
//	                     the primary stopped, nothing promoted anything, and a standby was
//	                     promoted by hand afterwards.
//	p03-standalone       one PostgreSQL server, on its own.
//
// Every rule in the catalogue matched a record in one of these. Nothing was written from
// memory — the corpus decides, which is the same standard the Galera, Group Replication and
// MongoDB catalogues are held to and the reason all three of them caught mistakes.

import (
	"strings"
	"testing"
)

// The three flavours have to be told apart, because the findings that may speak differ.
// Telling somebody with a single server that their cluster has no leader is worse than
// saying nothing at all.
func TestPGFlavoursAreDistinguished(t *testing.T) {
	for _, tc := range []struct{ dir, name, want string }{
		{"p01-patroni-cluster", "pat1.log", lsFlavourPatroni},
		{"p02-streaming", "rep2.log", lsFlavourPGStream},
		{"p03-standalone", "postgres.log", lsFlavourPostgres},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			b := lsLoadScenario(t, tc.dir)
			for _, s := range b.Sources {
				if s.Name != tc.name {
					continue
				}
				if s.Flavour != tc.want {
					t.Errorf("%s is flavoured %q, want %q", tc.name, s.Flavour, tc.want)
				}
				if s.Engine != pktEnginePostgres {
					t.Errorf("%s sniffed as engine %q", tc.name, s.Engine)
				}
				return
			}
			t.Fatalf("%s not found in the bundle", tc.name)
		})
	}
}

// A standalone server must not be told about a cluster it is not in.
func TestPGStandaloneGetsNoClusterFindings(t *testing.T) {
	b := lsLoadScenario(t, "p03-standalone")
	for _, id := range []string{"pg-dcs-lost", "pg-no-primary", "pg-diverged", "pg-failover", "pg-lag-invisible"} {
		if f := lsHasFinding(b, id); f != nil {
			t.Errorf("finding %q fired on a standalone server: %s", id, f.Title)
		}
	}
}

// The finding this catalogue exists for. A Patroni leader that cannot reach etcd stands
// down, and PostgreSQL's own log for that window shows a clean shutdown with no reason
// attached — so the database log alone says a primary stopped for no reason at all.
func TestPGDCSLossIsExplained(t *testing.T) {
	b := lsLoadScenario(t, "p01-patroni-cluster")
	f := lsHasFinding(b, "pg-dcs-lost")
	if f == nil {
		t.Fatal("stopping etcd on every member produced no finding")
	}
	if f.Sev != lsSevBad {
		t.Errorf("a leader standing down should be %q, got %q", lsSevBad, f.Sev)
	}
	if !strings.Contains(f.Detail, "nothing wrong with PostgreSQL") {
		t.Errorf("the finding does not say the database was healthy: %s", f.Detail)
	}
	// And the same window in PostgreSQL's own log has to be as silent as the finding
	// claims — the claim is checkable against the corpus, so it is checked.
	for _, e := range b.Events {
		if e.Subsys != lsSubsysPostgres {
			continue
		}
		if strings.Contains(strings.ToLower(e.Message), "etcd") || strings.Contains(e.Message, "DCS") {
			t.Errorf("PostgreSQL's log does mention the DCS after all: %q", e.Message)
		}
	}
}

// PostgreSQL's FATAL means a session ended, not that the server failed. Reading the level as
// a floor — which is right for MySQL and MongoDB — reported twenty-seven "bad" events for
// clients that arrived a second early during an ordinary restart.
func TestPGFatalIsNotAServerFailure(t *testing.T) {
	b := lsLoadScenario(t, "p01-patroni-cluster")
	for _, e := range b.Events {
		switch e.Label {
		case "Connections refused — still starting", "Connection terminated by the server",
			"Stream ended abruptly":
			if e.Sev == lsSevBad {
				t.Errorf("%q is a FATAL that ends one session and is reported as bad", e.Label)
			}
		}
	}
	// The catalogue must still be able to say bad when it means it.
	if f := lsHasFinding(b, "pg-dcs-lost"); f == nil || f.Sev != lsSevBad {
		t.Error("nothing in this capture is bad, which cannot be right")
	}
}

// A promotion is classified per promotion, not per window. A cluster switched over once and
// failed over later is not "on request", and calling it that hides the one worth looking at.
func TestPGFailoverSeparatesPlannedFromUnplanned(t *testing.T) {
	b := lsLoadScenario(t, "p01-patroni-cluster")
	f := lsHasFinding(b, "pg-failover")
	if f == nil {
		t.Fatal("three promotions produced no failover finding")
	}
	if !strings.Contains(f.Detail, "— requested") {
		t.Errorf("the switchover is not marked as requested: %s", f.Detail)
	}
	if !strings.Contains(f.Title, "unplanned") {
		t.Errorf("an unplanned failover is not distinguished: %q", f.Title)
	}
	if f.Sev != lsSevBad {
		t.Errorf("a window containing an unplanned failover should be bad, got %q", f.Sev)
	}
}

// Building a cluster is not an incident. Patroni creates every replica by copying the
// leader, so a rebuild during the first minute of a cluster's life is how it is supposed to
// work — and reporting it as discarded writes would flag every healthy deployment on the day
// it was built.
func TestPGClusterCreationIsNotADivergence(t *testing.T) {
	b := lsLoadScenario(t, "p01-patroni-cluster")
	if f := lsHasFinding(b, "pg-diverged"); f != nil {
		t.Errorf("the initial bootstrap of two replicas was reported as a divergence: %s", f.Detail)
	}
	// The records themselves are still classified and still in the timeline — only the
	// verdict declines to call them an incident.
	found := false
	for _, e := range b.Events {
		if e.Label == "Patroni: rebuilding this member from the leader" {
			found = true
		}
	}
	if !found {
		t.Error("the bootstrap records were dropped entirely, which is the other way to be wrong")
	}
}

// Two logs in one file, in two formats, and the state has to be carried in TIME order. The
// collector appends the Patroni journal after the PostgreSQL file, so walking the source as
// it arrives puts the end of the first log's state onto the beginning of the second's.
func TestPGStateFollowsTimeNotFileOrder(t *testing.T) {
	b := lsLoadScenario(t, "p01-patroni-cluster")
	// The earliest event on each source cannot already be in a state that member only
	// reaches later. STANDBY on the first record of a member that has not started
	// PostgreSQL yet is the bug this guards.
	first := map[int]lsEvent{}
	for _, e := range b.Events {
		if _, seen := first[e.Src]; !seen {
			first[e.Src] = e
		}
	}
	for src, e := range first {
		if e.State == lsStateStandby || e.State == lsStatePrimaryM {
			t.Errorf("%s starts in state %q, which it cannot have reached before its first record",
				lsNode(b, src), e.State)
		}
	}
}

// The honest note, and PostgreSQL's is worse than the others: a standby writes "waiting for
// WAL to become available" whether it is idle and up to date or receiving nothing at all.
// The corpus contains both cases and they are indistinguishable, which is exactly what the
// finding says.
func TestPGLagIsInvisibleAndSaysSo(t *testing.T) {
	b := lsLoadScenario(t, "p02-streaming")
	f := lsHasFinding(b, "pg-lag-invisible")
	if f == nil {
		t.Fatal("a streaming bundle should carry the lag note")
	}
	if !strings.Contains(f.Detail, "waiting for WAL") {
		t.Errorf("the note does not mention the message that cannot tell the two apart: %s", f.Detail)
	}
	// And the corpus has to actually contain that message during a window when the primary
	// was stopped, or the claim is not supported by the evidence.
	waiting, downed := false, false
	for _, e := range b.Events {
		if e.Label == "Waiting for WAL" {
			waiting = true
		}
		if e.Label == "Cannot reach the primary" {
			downed = true
		}
	}
	if !waiting || !downed {
		t.Errorf("the fixture does not contain both halves of the claim (waiting=%v, primary down=%v)", waiting, downed)
	}
}

// SQLSTATEs are carried alongside the English because they are the only stable, untranslated
// key PostgreSQL has — and they reach the log only when the operator sets %e, which is not
// the default. The parser has to find one in both of the placements people use.
func TestPGParsesSQLSTATEWhenPresent(t *testing.T) {
	for _, line := range []string{
		`2026-08-15 11:00:00.000 UTC [123] 53300 FATAL:  sorry, too many clients already`,
		`2026-08-15 11:00:00.000 UTC [123] app@db 53300 FATAL:  sorry, too many clients already`,
	} {
		recs := lsFoldPostgres([]byte(line + "\n"))
		if len(recs) != 1 {
			t.Fatalf("%q did not parse", line)
		}
		if recs[0].Code != "53300" {
			t.Errorf("SQLSTATE read as %q from %q", recs[0].Code, line)
		}
		e, keep := lsClassifyPG(recs[0])
		if !keep || e.Label != "Connection limit reached" {
			t.Errorf("not classified: keep=%v label=%q", keep, e.Label)
		}
	}
	// And a log without %e — the default, and what the whole corpus is — still classifies
	// on the English.
	recs := lsFoldPostgres([]byte("2026-08-15 11:00:00.000 UTC [123] FATAL:  sorry, too many clients already\n"))
	if e, keep := lsClassifyPG(recs[0]); !keep || e.Label != "Connection limit reached" {
		t.Errorf("without %%e: keep=%v label=%q", keep, e.Label)
	}
}

// An ERROR's DETAIL, HINT and STATEMENT belong to it. On their own they are unexplained
// fragments, and the STATEMENT is the most useful thing in the group.
func TestPGFoldsContinuationLines(t *testing.T) {
	log := `2026-08-15 11:00:00.000 UTC [123] ERROR:  duplicate key value violates unique constraint "dl_pkey"
2026-08-15 11:00:00.000 UTC [123] DETAIL:  Key (id)=(1) already exists.
2026-08-15 11:00:00.000 UTC [123] STATEMENT:  INSERT INTO dl VALUES (1,1);
`
	recs := lsFoldPostgres([]byte(log))
	if len(recs) != 1 {
		t.Fatalf("want 1 folded record, got %d", len(recs))
	}
	if len(recs[0].Body) != 2 {
		t.Fatalf("want 2 continuation lines, got %d", len(recs[0].Body))
	}
	if !strings.Contains(recs[0].Body[1], "INSERT INTO dl") {
		t.Errorf("the failing statement was lost: %q", recs[0].Body[1])
	}
}

// A PostgreSQL log must not be dragged through the MySQL or MongoDB catalogues, and the
// MySQL replication findings in particular must stay quiet — a standby with a missing
// replication slot was reported as an asynchronous MySQL replica whose channel had stopped.
func TestPGDoesNotDisturbTheOtherFlavours(t *testing.T) {
	for _, dir := range []string{"p01-patroni-cluster", "p02-streaming", "p03-standalone"} {
		b := lsLoadScenario(t, dir)
		for _, id := range []string{"replication-broken", "replica-lag", "mongo-no-primary", "quorum", "partition"} {
			if f := lsHasFinding(b, id); f != nil {
				t.Errorf("%s: %q fired on a PostgreSQL bundle — %s", dir, id, f.Title)
			}
		}
	}
	// And the reverse: a MySQL or MongoDB bundle must get none of the PostgreSQL findings.
	for _, dir := range []string{"s06-network-partition", "m05-rollback"} {
		b := lsLoadScenario(t, dir)
		for _, id := range []string{"pg-dcs-lost", "pg-failover", "pg-lag-invisible", "pg-no-primary"} {
			if f := lsHasFinding(b, id); f != nil {
				t.Errorf("%s: PostgreSQL finding %q fired — %s", dir, id, f.Title)
			}
		}
	}
}
