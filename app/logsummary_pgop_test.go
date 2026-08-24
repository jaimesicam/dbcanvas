package main

// logsummary_pgop_test.go — the Log Summary against the three PostgreSQL operators.
//
// Every fixture under testdata/logsummary/kg-*/ came off three live clusters deployed side
// by side on one host and driven through the same three things at the same time: a
// bootstrap, a force-deleted leader, and a backup.
//
//	kg-pg-*     Percona Operator for PostgreSQL 3.0.0 — zap, PostgreSQL 18, Patroni
//	kg-pgo-*    Crunchy PGO — logfmt, PostgreSQL 18, Patroni
//	kg-cnpg-*   CloudNativePG — JSON, no Patroni, instance manager
//
// Deploying them together is what makes the comparison worth anything: the same incident,
// the same minute, three operators, and only one of them says it happened.

import (
	"strings"
	"testing"
)

// TestThreePGOperatorsAreToldApart. Percona's operator is a FORK of Crunchy's and drives
// Crunchy's own custom resource, so its log is full of `postgres-operator.crunchydata.com`
// — only the Percona group tells them apart, and it has to be checked first. CloudNativePG
// is JSON and shares nothing with either.
func TestThreePGOperatorsAreToldApart(t *testing.T) {
	for _, tc := range []struct{ dir, file, want string }{
		{"kg-pg-failover", "operator.log", lsFlavourPerconaPG},
		{"kg-pgo-failover", "operator.log", lsFlavourCrunchyPGO},
		{"kg-cnpg-failover", "operator.log", lsFlavourCNPG},
	} {
		got := lsSniffPGOperator(lsReadFixture(t, tc.dir, tc.file))
		if got != tc.want {
			t.Errorf("%s/%s sniffed as %q, want %q", tc.dir, tc.file, got, tc.want)
		}
		if eng := lsSniffEngine(lsReadFixture(t, tc.dir, tc.file)); eng != pktEngineOperator {
			t.Errorf("%s/%s engine = %q, want %q", tc.dir, tc.file, eng, pktEngineOperator)
		}
	}
}

// TestCrunchyLogfmtParses. logrus's logfmt is a different library from the zap the other
// Percona operators use, even though this operator is the one Percona's was forked from.
func TestCrunchyLogfmtParses(t *testing.T) {
	recs := lsFoldCrunchy(lsReadFixture(t, "kg-pgo-failover", "operator.log"))
	if len(recs) == 0 {
		t.Fatal("no records")
	}
	msgs, timed := 0, 0
	for _, r := range recs {
		if r.Text != "" {
			msgs++
		}
		if r.TS > 0 {
			timed++
		}
	}
	if msgs < len(recs)/2 {
		t.Errorf("only %d of %d records carry a message — msg= is not being read", msgs, len(recs))
	}
	if timed != len(recs) {
		t.Errorf("%d of %d records have no timestamp", len(recs)-timed, len(recs))
	}
	// Quoted values with spaces must survive whole.
	f := lsParseLogfmt(`msg="reconciled instance set" PostgresCluster=pgo/crunchy instance-set=instance1`)
	if f.last("msg") != "reconciled instance set" {
		t.Errorf("quoted value = %q", f.last("msg"))
	}
	if f.last("PostgresCluster") != "pgo/crunchy" {
		t.Errorf("bare value = %q", f.last("PostgresCluster"))
	}
}

// TestCNPGMemberLogIsSplitInTwo is the shape no other source in this package has: one JSON
// stream carrying the instance manager's records AND PostgreSQL's, the latter wrapped as
// `{"logger":"postgres","msg":"record","record":{…the CSV fields…}}`. Left alone, a CNPG
// member's log is a wall of `msg: record` saying nothing.
func TestCNPGMemberLogIsSplitInTwo(t *testing.T) {
	recs := lsFoldCNPG(lsReadFixture(t, "kg-cnpg-failover", "cnpgc-2.log"))
	var pg, mgr int
	for _, r := range recs {
		switch r.Subsys {
		case lsSubsysPostgres:
			pg++
			if r.Text == "record" || r.Text == "" {
				t.Fatalf("line %d was not unwrapped: %q", r.Line, r.Text)
			}
			if r.TS == 0 {
				t.Errorf("line %d has no timestamp — the CSV stamp did not parse", r.Line)
			}
		default:
			mgr++
		}
	}
	if pg == 0 {
		t.Fatal("no PostgreSQL records were unwrapped out of the member's stream")
	}
	if mgr == 0 {
		t.Fatal("no instance-manager records survived the split")
	}
}

// TestPatroniMembersNeedNoNewCatalogue is the payoff of reading the `database` container:
// a Percona or Crunchy member IS a Patroni node, and the rules written against a
// hand-built Patroni cluster read an operator-managed one unchanged.
func TestPatroniMembersNeedNoNewCatalogue(t *testing.T) {
	for _, dir := range []string{"kg-pg-failover", "kg-pgo-failover"} {
		b := lsLoadScenario(t, dir)
		patroni := 0
		for _, s := range b.Sources {
			if s.Flavour == lsFlavourPatroni {
				patroni++
			}
		}
		if patroni == 0 {
			t.Errorf("%s: no member was recognised as a Patroni node", dir)
		}
	}
}

// TestOperatorsAreSilentAboutTheFailover is the finding, and it is asserted against the
// raw text as well as the parse because it is a claim about an absence.
func TestOperatorsAreSilentAboutTheFailover(t *testing.T) {
	for _, dir := range []string{"kg-pg-failover", "kg-pgo-failover"} {
		raw := lsReadFixture(t, dir, "operator.log")
		// Words about the DATABASE's failover. "election" is deliberately not among them:
		// both operators run a controller-runtime leader election of their own
		// (`cpk-leader-election-lease`), which is about the operator process and not about
		// PostgreSQL at all — the first version of this test failed on exactly that.
		for _, mustNot := range []string{"failover", "promoted", "new primary", "switchover"} {
			if strings.Contains(strings.ToLower(raw), mustNot) {
				t.Errorf("%s: the operator log now mentions %q — the premise has changed", dir, mustNot)
			}
		}
		b := lsLoadScenario(t, dir)
		members := 0
		for _, s := range b.Sources {
			if s.Flavour == lsFlavourPatroni {
				members++
			}
		}
		if members == 0 {
			t.Fatalf("%s: no Patroni member in the bundle", dir)
		}
		if lsHasFinding(b, "pgop-silent-failover") == nil {
			// Only a failure when the members actually recorded something worth reporting;
			// a bundle whose window holds no incident has nothing to be silent about.
			if lsPick(b, func(e lsEvent) bool {
				return lsSrcIs(b, e.Src, lsFlavourPatroni) && e.Sev == lsSevBad
			}) != nil {
				t.Errorf("%s: the operator's silence about the members' incidents was not reported", dir)
			}
		}
	}
	// CloudNativePG is the exception and must NOT be accused of it: it runs the failover
	// and says so.
	raw := lsReadFixture(t, "kg-cnpg-failover", "operator.log")
	if !strings.Contains(raw, "switchover or a failover in progress") {
		t.Error("the CNPG operator no longer narrates its own failover")
	}
}

// TestCNPGWALArchiveFailureIsReported. Measured on the corpus: every archive attempt
// failed, the cluster went on serving and reporting healthy, and the first thing that
// failed visibly was a backup.
func TestCNPGWALArchiveFailureIsReported(t *testing.T) {
	b := lsLoadScenario(t, "kg-cnpg-backup")
	f := lsHasFinding(b, "cnpg-wal-archive")
	if f == nil {
		t.Fatal("WAL archiving failing was not reported")
	}
	if f.Sev != lsSevBad {
		t.Errorf("severity = %q, want bad — nothing is reaching object storage", f.Sev)
	}
	for _, want := range []string{"pg_stat_archiver", "pg_wal"} {
		if !strings.Contains(f.Advice, want) {
			t.Errorf("advice does not point at %q: %s", want, f.Advice)
		}
	}
}

// TestPGOperatorFindingsDoNotCrossVocabularies — the third time this hazard has bitten in
// this package, so it is now checked for every operator family added.
func TestPGOperatorFindingsDoNotCrossVocabularies(t *testing.T) {
	pg := lsLoadScenario(t, "kg-pg-failover")
	for _, id := range []string{"pxcop-pitr-gap", "pxcop-restore", "pxcop-rollout",
		"psmdb-pitr-stalled", "psmdb-restore-writable", "psmdb-rollout-stuck"} {
		if f := lsHasFinding(pg, id); f != nil {
			t.Errorf("a PostgreSQL bundle produced %q: %s", id, f.Title)
		}
	}
	pxc := lsLoadScenario(t, "k14-final")
	for _, id := range []string{"pgop-silent-failover", "cnpg-wal-archive", "cnpg-switchover", "pgop-backup-ok"} {
		if f := lsHasFinding(pxc, id); f != nil {
			t.Errorf("a PXC bundle produced %q: %s", id, f.Title)
		}
	}
}

// TestPGPerformanceAdvisorReadsTheSymptoms. PostgreSQL prints no configuration, so the
// advisor is a reading of what the server complained about. kg-pg-stress is a cluster
// driven with pgbench on a deliberately starved configuration — 64MB shared_buffers, 64MB
// max_wal_size, 64kB work_mem — which is what produces the evidence.
func TestPGPerformanceAdvisorReadsTheSymptoms(t *testing.T) {
	b := lsLoadScenario(t, "kg-pg-stress")
	var p *lsPGPerf
	for _, s := range b.Sources {
		if s.PGPerf != nil && s.PGPerf.CheckpointsTooFrequent > 0 {
			p = s.PGPerf
			break
		}
	}
	if p == nil {
		t.Fatal("no member reported checkpoints occurring too frequently")
	}
	if p.CheckpointGapSecs <= 0 || p.CheckpointGapSecs > 60 {
		t.Errorf("closest checkpoint complaint was %.0fs apart", p.CheckpointGapSecs)
	}
	if p.TempFiles == 0 {
		t.Error("no sorts spilled to disk, on a cluster running work_mem=64kB")
	}
	if p.SlowQueries == 0 {
		t.Error("no slow statements, with log_min_duration_statement at 100ms")
	}
	tips := lsPGPerfAdvice(p, 600)
	var keys []string
	for _, tip := range tips {
		keys = append(keys, tip.Key)
	}
	for _, want := range []string{"max_wal_size", "work_mem", "slow statements"} {
		found := false
		for _, k := range keys {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no advice about %q; got %v", want, keys)
		}
	}
	// The advisor must never tell anybody to raise a setting merely because it is small:
	// measured on these three clusters, doing exactly that moved throughput by between
	// -8% and +1.6%.
	for _, tip := range tips {
		if tip.Key == "shared_buffers" {
			t.Error("the advisor is recommending shared_buffers off the back of no evidence")
		}
	}
}

// TestPGAdvisorSaysWhenTheLogWasNotAllowedToRecord. The gate that matters more than any of
// the numbers: all three operators ship with slow-query, temp-file and lock-wait logging
// off, so an empty result is not evidence of health.
func TestPGAdvisorSaysWhenTheLogWasNotAllowedToRecord(t *testing.T) {
	quiet := &lsPGPerf{Checkpoints: 3, SawCheckpointLogging: true}
	tips := lsPGPerfAdvice(quiet, 600)
	found := false
	for _, tip := range tips {
		if strings.Contains(tip.Key, "allowed to record") {
			found = true
			for _, want := range []string{"log_min_duration_statement", "log_temp_files"} {
				if !strings.Contains(tip.Is+tip.Want, want) {
					t.Errorf("the gate does not name %q", want)
				}
			}
		}
	}
	if !found {
		t.Fatal("a log with no slow-query or temp-file records said nothing about why")
	}
	// And with everything on, the gate stays quiet rather than nagging.
	loud := &lsPGPerf{Checkpoints: 3, SawCheckpointLogging: true, SawSlowLogging: true, SawTempLogging: true}
	for _, tip := range lsPGPerfAdvice(loud, 600) {
		if strings.Contains(tip.Key, "allowed to record") {
			t.Error("the gate fires on a log that does record all three")
		}
	}
}

// TestThreeRestoreModels. The three operators restore in three different ways, and the
// difference is the one that matters to whoever is watching: Percona takes the cluster
// down, CloudNativePG builds a second one beside it, and Crunchy says nothing at all.
func TestThreeRestoreModels(t *testing.T) {
	// Percona: in place, and measured.
	pg := lsLoadScenario(t, "kg-pg-restore")
	f := lsHasFinding(pg, "pgop-restore")
	if f == nil {
		t.Fatal("no restore finding on a bundle whose operator ran one")
	}
	if f.Until <= f.At {
		t.Error("the outage was not measured")
	}
	if !strings.Contains(f.Detail, "IN PLACE") {
		t.Errorf("the finding does not say which model this is: %s", f.Detail)
	}
	if !strings.Contains(f.Advice, "new timeline") {
		t.Errorf("advice does not warn that the old backups are no longer a base: %s", f.Advice)
	}

	// CloudNativePG: a second cluster, and the application is still pointed at the first.
	cn := lsLoadScenario(t, "kg-cnpg-restore")
	g := lsHasFinding(cn, "pgop-recovery-new")
	if g == nil {
		t.Fatal("no recovery finding on a CloudNativePG bundle that recovered")
	}
	if !strings.Contains(g.Advice, "still pointed at the ORIGINAL") {
		t.Errorf("advice does not say the application did not move: %s", g.Advice)
	}
	if lsHasFinding(cn, "pgop-restore") != nil {
		t.Error("a CloudNativePG recovery was reported as an in-place restore")
	}

	// Crunchy: its operator narrated nothing, so there is nothing to report and the page
	// must not invent it.
	raw := lsReadFixture(t, "kg-pgo-restore", "operator.log")
	for _, mustNot := range []string{"restore succeeded", "restore in progress"} {
		if strings.Contains(strings.ToLower(raw), mustNot) {
			t.Errorf("the Crunchy operator now narrates its restore — the premise has changed")
		}
	}
}
