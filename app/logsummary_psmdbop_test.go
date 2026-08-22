package main

// logsummary_psmdbop_test.go — the Log Summary against a live Percona Operator for MongoDB
// cluster.
//
// Every fixture under testdata/logsummary/km*/ came off one cluster — PSMDB operator
// 1.23.0 running percona-server-mongodb 8.0.26-11 with PBM 2.15.0 on k3s v1.36.3, a
// three-member replica set backing up to a SeaweedFS S3 endpoint — driven under continuous
// write load. Each directory holds the operator Deployment's log, every member's mongod
// log (tailed, because a mongod under load writes tens of megabytes) and every member's
// pbm-agent sidecar log.
//
//	km01-bootstrap       the replica set created from cr.yaml and reaching ready
//	km04-primary-kill    the primary force-deleted under load: election and rejoin
//	km05-partition       a secondary cut off with netem for 3m 46s
//	km06-unschedulable   a spec edit that dropped anti-affinity, leaving one member
//	                     Pending and the rolling restart blocked indefinitely
//	km09-pitr-broken     after a restore: PITR enabled, reporting ON, and refusing to run
//	km11-final           the whole run, including a point-in-time restore

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------- format

// TestPSMDBOperatorIsNotThePXCOperator. Both are the same controller-runtime process
// writing identically shaped lines, and nothing about the SHAPE tells them apart — only
// the controller group in the field object does. Getting this wrong would run the PXC
// catalogue over a MongoDB cluster's log.
func TestPSMDBOperatorIsNotThePXCOperator(t *testing.T) {
	b := lsLoadScenario(t, "km11-final")
	var op, pbm, members int
	for _, s := range b.Sources {
		switch s.Flavour {
		case lsFlavourPSMDBOperator:
			op++
		case lsFlavourPXCOperator:
			t.Errorf("%s was filed as a PXC operator log", s.Name)
		case lsFlavourPBMAgent:
			pbm++
		case lsFlavourMongoRS, pktEngineMongoDB:
			members++
		}
	}
	if op != 1 {
		t.Errorf("got %d PSMDB operator sources, want 1", op)
	}
	if pbm != 3 {
		t.Errorf("got %d pbm-agent sources, want 3 — one per member", pbm)
	}
	if members != 3 {
		t.Errorf("got %d mongod sources, want 3", members)
	}
}

// TestPBMAgentLogIsTwoFormats. pbm-agent writes its own RFC3339 lines AND Go-stdlib lines,
// and the second kind is the interesting one: PBM keeps its log inside MongoDB, so a
// stderr line is by construction written while the database was unreachable. The
// container's entrypoint uses the same shape for the only record that an agent crashed.
func TestPBMAgentLogIsTwoFormats(t *testing.T) {
	recs := lsFoldPBM(lsReadFixture(t, "km05-partition", "my-cluster-name-rs0-2.pbm.log"))
	var own, entry, stderr int
	for _, r := range recs {
		switch r.Subsys {
		case lsSubsysPBM:
			if strings.HasPrefix(r.Time, "20") && strings.Contains(r.Time, "/") {
				stderr++ // a Go-stdlib stamp marked as PBM's own: the [ERROR] fallback
			} else {
				own++
			}
		case lsSubsysPBMEntry:
			entry++
		}
	}
	if own == 0 {
		t.Error("no pbm-agent records in its own format")
	}
	if stderr == 0 {
		t.Error("no `writing log: db:` stderr records — the half written while the cluster was unreachable")
	}
	if entry == 0 {
		t.Error("no entrypoint records — the only place an agent restart is recorded")
	}
	// And the timestamps have to parse: PBM writes +0000, which time.RFC3339 rejects.
	for _, r := range recs {
		if r.TS == 0 && !r.Approx {
			t.Fatalf("line %d has no timestamp: %q", r.Line, r.Time)
		}
	}
}

// TestPBMBannerFoldsIntoTheStartRecord. Every agent start prints a 26-line ASCII-art
// Percona Squad banner and a version block, none of it timestamped. Folded, it is the
// detail of the start record; unfolded it would be 26 events per start, 26 starts.
func TestPBMBannerFoldsIntoTheStartRecord(t *testing.T) {
	recs := lsFoldPBM(lsReadFixture(t, "km01-bootstrap", "my-cluster-name-rs0-0.pbm.log"))
	for _, r := range recs {
		if strings.Contains(r.Text, "Join Percona Squad") || strings.HasPrefix(r.Text, "Version:") {
			t.Fatalf("line %d became a record of its own: %q", r.Line, r.Text)
		}
	}
	found := false
	for _, r := range recs {
		if strings.Contains(strings.Join(r.Body, "\n"), "Join Percona Squad") {
			found = true
		}
	}
	if !found {
		t.Error("the version banner is in no record's body — it is being dropped rather than folded")
	}
}

// ---------------------------------------------------------------- catalogue

func TestPSMDBOperatorVocabulary(t *testing.T) {
	b := lsLoadScenario(t, "km11-final")
	for _, want := range []string{
		"Replica set initiated",
		"Restore started",
		"Restore is switching point-in-time recovery off",
		"Rolling restart started (smart update)",
		"Backup started",
	} {
		if lsHasLabel(b, want) == nil {
			t.Errorf("no event labelled %q", want)
		}
	}
	for _, prefix := range []string{"Cluster state: ", "PITR can reach ", "Primary is ", "Cluster is running MongoDB "} {
		found := false
		for _, e := range b.Events {
			if strings.HasPrefix(e.Label, prefix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no event whose label starts with %q — the field was not read", prefix)
		}
	}
}

// ---------------------------------------------------------------- verdicts

// TestPITRStalledAfterRestore is the finding this catalogue exists for.
//
// After a restore PBM refuses to slice the oplog until a new full backup exists, and says
// so only in the agent's log — while spec.backup.pitr.enabled is still true and pbm status
// still prints Status [ON].
func TestPITRStalledAfterRestore(t *testing.T) {
	b := lsLoadScenario(t, "km09-pitr-broken")
	f := lsHasFinding(b, "psmdb-pitr-stalled")
	if f == nil {
		t.Fatal("PITR refusing to resume after a restore was not reported")
	}
	if f.Sev != lsSevBad {
		t.Errorf("severity = %q, want bad — nothing is being written to object storage", f.Sev)
	}
	if !strings.Contains(f.Advice, "full backup") {
		t.Errorf("advice does not say what fixes it: %s", f.Advice)
	}
	// The evidence has to really be in the fixture.
	var raw string
	for _, n := range []string{"my-cluster-name-rs0-0.pbm.log", "my-cluster-name-rs0-1.pbm.log", "my-cluster-name-rs0-2.pbm.log"} {
		raw += lsReadFixture(t, "km09-pitr-broken", n)
	}
	if !strings.Contains(raw, "a new backup is required to resume PITR") {
		t.Fatal("fixture no longer contains the refusal the finding is built on")
	}
}

// TestOnlyOneMemberSlices. PBM nominates one agent per replica set; the other two write
// `skip after nomination` and go quiet, which is indistinguishable from PITR being off.
// Reading one agent's log is the mistake this page exists to prevent.
func TestOnlyOneMemberSlices(t *testing.T) {
	b := lsLoadScenario(t, "km11-final")
	f := lsHasFinding(b, "psmdb-pitr-who")
	if f == nil {
		if lsHasFinding(b, "psmdb-pitr-nobody") != nil {
			return // also a valid reading of this window
		}
		t.Fatal("no finding about which member is slicing the oplog")
	}
	if !strings.Contains(f.Detail, "one agent per replica set") {
		t.Errorf("the finding does not explain the other agents' silence: %s", f.Detail)
	}
}

// TestRestoreRunsWithTheClusterWritable is the data-integrity difference between the two
// operators, and it was found by measuring rather than by reading documentation: a
// point-in-time restore that was EXACT to its target still ended with 32,000 documents
// that had never been in the backup, written after PBM re-created the collection.
func TestRestoreRunsWithTheClusterWritable(t *testing.T) {
	b := lsLoadScenario(t, "km11-final")
	f := lsHasFinding(b, "psmdb-restore-writable")
	if f == nil {
		t.Fatal("no finding that a logical restore runs with the cluster accepting writes")
	}
	if f.Sev != lsSevBad {
		t.Errorf("severity = %q, want bad", f.Sev)
	}
	for _, want := range []string{"IN PLACE", "scale the workload to zero"} {
		if !strings.Contains(f.Detail+f.Advice, want) {
			t.Errorf("the finding does not carry %q", want)
		}
	}
}

// TestBlockedRolloutIsMeasuredNotCounted. The operator re-logs `can't start/continue
// 'SmartUpdate'` on every reconcile for as long as the condition holds — 381 records in
// this corpus from ONE unschedulable pod. The span is the outage; the count is the
// reconcile interval.
func TestBlockedRolloutIsMeasuredNotCounted(t *testing.T) {
	b := lsLoadScenario(t, "km06-unschedulable")
	f := lsHasFinding(b, "psmdb-rollout-stuck")
	if f == nil {
		t.Fatal("a blocked rolling restart was not reported")
	}
	if f.Until <= f.At {
		t.Error("the block was not measured")
	}
	if !strings.Contains(f.Title, "blocked for") {
		t.Errorf("the title counts rather than measures: %q", f.Title)
	}
	if !strings.Contains(f.Advice, "merge patch") {
		t.Errorf("advice does not name the cause the corpus found: %s", f.Advice)
	}
}

// TestPartitionedSecondaryIsNotRestarted is the contrast with PXC, and it is asserted
// against the raw fixture because it is a claim about what is NOT there.
//
// A PXC member that leaves the primary component is killed by its liveness probe within 25
// seconds. A mongod's probe asks whether the process answers, so a partitioned secondary is
// left alone — measured, 3m 46s of 100% packet loss with zero restarts.
func TestPartitionedSecondaryIsNotRestarted(t *testing.T) {
	raw := lsReadFixture(t, "km05-partition", "my-cluster-name-rs0-2.log")
	if strings.Contains(raw, "Received SHUTDOWN") || strings.Contains(raw, "got signal") {
		t.Fatal("the partitioned member's log now contains a shutdown — the premise of this contrast has changed")
	}
	// Its agent, on the other hand, has plenty to say — which is the point: the evidence
	// for the partition is in the sidecar, not the database.
	pbm := lsReadFixture(t, "km05-partition", "my-cluster-name-rs0-2.pbm.log")
	if !strings.Contains(pbm, "ReplicaSetNoPrimary") {
		t.Error("the agent's log no longer records losing sight of the primary")
	}
}

// TestPSMDBTuningAdvice. oplogSpanMin is the operator's default RPO and it appears in no
// MongoDB log at all — only in the operator's own `Setting pitr.oplogSpanMin` record.
func TestPSMDBTuningAdvice(t *testing.T) {
	b := lsLoadScenario(t, "km11-final")
	f := lsHasFinding(b, "psmdb-settings")
	if f == nil {
		t.Fatal("no configuration finding")
	}
	for _, want := range []string{"oplogSpanMin", "restores are not fenced", "operations a second"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("the configuration report does not mention %q", want)
		}
	}
	if !strings.Contains(f.Advice, "merge patch replaces a list") {
		t.Errorf("advice does not carry the lesson that cost this corpus a cluster: %s", f.Advice)
	}
}

// TestPSMDBCacheAdviceNeedsAStartup. The engine's real cache size is printed once, when
// the engine is opened, so it is only advisable about when the log reaches back to a
// start. A busy member's tail does not — measured on this corpus, a 3,000-line tail of a
// member under load covers about four minutes — and the finding simply omits the tip
// rather than guessing at it.
func TestPSMDBCacheAdviceNeedsAStartup(t *testing.T) {
	withStart := lsLoadScenario(t, "km01-bootstrap")
	var cfg *lsMongoConfig
	for _, s := range withStart.Sources {
		if s.MongoCfg != nil && s.MongoCfg.CacheMB > 0 {
			cfg = s.MongoCfg
			break
		}
	}
	if cfg == nil {
		t.Fatal("no member's engine configuration was read from a fixture that spans a start")
	}
	// This fixture is from BEFORE the cluster was tuned, and it is the hazard itself:
	// three members on one 29.4 GiB host, each engine sizing its cache from the host
	// rather than from the pod, each claiming 14.5 GiB — 44 GiB of intent on a machine
	// that has 29. The operator sets no cacheSizeGB of its own, so this is what every
	// cluster deployed from the shipped cr.yaml does.
	if cfg.Pinned {
		t.Errorf("km01-bootstrap is meant to be the untuned cluster; the engine reports %.0f MB pinned", cfg.CacheMB)
	}
	if cfg.CacheMB < 4096 {
		t.Errorf("cache = %.0f MB — the point of this fixture is that it is far larger than the pod's share of the host", cfg.CacheMB)
	}
	tips := lsPSMDBAdvice(withStart, cfg)
	found := false
	for _, tip := range tips {
		if !strings.Contains(tip.Key, "cacheSizeGB") {
			continue
		}
		found = true
		if tip.Sev != lsSevWarn {
			t.Errorf("an unpinned 14.5 GiB cache on a shared host is advised at %q", tip.Sev)
		}
	}
	if !found {
		t.Error("no cache advice from a log that carries the engine's own startup line")
	}
	// And with no startup in the window there is simply no tip, rather than a wrong one.
	for _, tip := range lsPSMDBAdvice(withStart, nil) {
		if strings.Contains(tip.Key, "cacheSizeGB") {
			t.Error("cache advice was offered with no engine configuration to base it on")
		}
	}
}

// TestEachAgentIsNamedAfterItsMember. Found by reading the live sources table: all three
// pbm-agent lanes came back called "pbm", because the collector's source name is
// `<pod>/pbm` and the fallback keeps only the last path segment. Three identical lanes are
// worse than useless in a page whose whole premise is comparing members — and the agent
// does say which member it is, once, in the version block folded into its start record.
func TestEachAgentIsNamedAfterItsMember(t *testing.T) {
	b := lsLoadScenario(t, "km11-final")
	seen := map[string]bool{}
	n := 0
	for _, s := range b.Sources {
		if s.Flavour != lsFlavourPBMAgent {
			continue
		}
		n++
		if s.Node == "" || s.Node == "pbm" {
			t.Errorf("agent source %q is called %q", s.Name, s.Node)
		}
		if seen[s.Node] {
			t.Errorf("two agent sources are both called %q", s.Node)
		}
		seen[s.Node] = true
	}
	if n != 3 {
		t.Fatalf("got %d agent sources, want 3", n)
	}
}

// TestOperatorFindingsDoNotCrossVocabularies. Found by reading the live page: on a MongoDB
// bundle the PXC catalogue's findings fired, because both use the same classes and some of
// the same labels. It reported "Gap in the collected binary logs" about a cluster that has
// no binary logs, and "4 backups started and no success was recorded" about backups that
// had all succeeded under a label the PXC catalogue does not own.
func TestOperatorFindingsDoNotCrossVocabularies(t *testing.T) {
	mongo := lsLoadScenario(t, "km11-final")
	for _, id := range []string{"pxcop-pitr-gap", "pxcop-backup-unfinished", "pxcop-backup-ok",
		"pxcop-restore", "pxcop-rollout", "pxcop-reconcile"} {
		if f := lsHasFinding(mongo, id); f != nil {
			t.Errorf("a MongoDB bundle produced the PXC finding %q: %s", id, f.Title)
		}
	}
	pxc := lsLoadScenario(t, "k14-final")
	for _, id := range []string{"psmdb-pitr-stalled", "psmdb-pitr-who", "psmdb-pitr-nobody",
		"psmdb-restore-writable", "psmdb-rollout-stuck", "psmdb-backup-ok", "psmdb-backup-failed"} {
		if f := lsHasFinding(pxc, id); f != nil {
			t.Errorf("a PXC bundle produced the MongoDB finding %q: %s", id, f.Title)
		}
	}
}
