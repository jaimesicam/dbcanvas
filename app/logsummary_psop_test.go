package main

// logsummary_psop_test.go — the last operator, and the Kubernetes Events source.
//
// Fixtures under testdata/logsummary/ko-*/ came off two clusters deployed side by side:
// the Percona Operator for MySQL (Percona Server) running a three-member Group Replication
// cluster, and CloudNativePG. Both were bootstrapped, had their primary force-deleted, and
// were driven through a point-in-time recovery.

import (
	"strings"
	"testing"
)

// TestPSOperatorMembersNeedNoNewCatalogue is the payoff, and it is the MySQL twin of
// TestPatroniMembersNeedNoNewCatalogue: `kubectl logs <pod> -c mysql` on this operator's
// members returns the mysqld error log itself — not the entrypoint trace the PXC operator's
// pods print — and its records are Group Replication's, which this package already reads.
func TestPSOperatorMembersNeedNoNewCatalogue(t *testing.T) {
	b := lsLoadScenario(t, "ko-ps-primary-kill")
	var op, gr int
	for _, s := range b.Sources {
		switch s.Flavour {
		case lsFlavourPSOperator:
			op++
		case lsFlavourGroupRepl:
			gr++
		}
	}
	if op != 1 {
		t.Errorf("got %d PS operator sources, want 1", op)
	}
	if gr == 0 {
		t.Fatal("no member was recognised as a Group Replication member")
	}
}

// TestPSOperatorNamesTheNewPrimary. Where this operator sits between the other two MySQL
// ones is the whole finding: PXC's says nothing about a failover at all, and this one
// records the single fact that is hard to reconstruct afterwards — which member the writes
// moved to, and when.
func TestPSOperatorNamesTheNewPrimary(t *testing.T) {
	b := lsLoadScenario(t, "ko-ps-primary-kill")
	var moved *lsEvent
	for i := range b.Events {
		if strings.HasPrefix(b.Events[i].Label, "Primary moved to ") {
			moved = &b.Events[i]
			break
		}
	}
	if moved == nil {
		t.Fatal("the operator's `Assigning primary label` record was not read")
	}
	if moved.Peer == "" || !strings.Contains(moved.Peer, "mysql") {
		t.Errorf("the new primary was not extracted: peer=%q", moved.Peer)
	}
}

// ---------------------------------------------------------------- Kubernetes Events

// TestK8sEventsAreASourceOfTheirOwn. Not a log: one JSON List, not a line stream, so
// nothing else in this package would parse it — and the sniff has to claim it before the
// engine sniffers, whose vocabulary appears inside the Events' own `message` fields.
func TestK8sEventsAreASourceOfTheirOwn(t *testing.T) {
	raw := lsReadFixture(t, "ko-ps-primary-kill", "events.json")
	if !lsSniffK8sEvents(raw) {
		t.Fatal("the Events document was not recognised")
	}
	if eng := lsSniffEngine(raw); eng != pktEngineK8sEvents {
		t.Errorf("engine = %q, want %q", eng, pktEngineK8sEvents)
	}
	recs := lsFoldK8sEvents([]byte(raw))
	if len(recs) == 0 {
		t.Fatal("no records")
	}
	for _, r := range recs {
		if r.Code == "" {
			t.Fatalf("record %d has no reason, which is what severity comes from", r.Line)
		}
		if r.TS == 0 {
			t.Fatalf("record %d has no timestamp", r.Line)
		}
	}
}

// TestK8sEventsCarryTheKillReason is the reason this source exists, and it closes a gap
// this package has carried since the PXC operator landed.
//
// A container killed by its liveness probe writes an ordinary shutdown record in its own
// log and nothing in the operator's. Kubernetes files the reason as `Normal`, which is why
// severity here comes from the reason rather than from the type.
func TestK8sEventsCarryTheKillReason(t *testing.T) {
	b := lsLoadScenario(t, "ko-ps-primary-kill")
	var killed *lsEvent
	for i := range b.Events {
		if b.Events[i].Label == "Kubernetes killed a container" {
			killed = &b.Events[i]
			break
		}
	}
	if killed == nil {
		t.Fatal("no `Killing` event was classified")
	}
	if killed.Sev != lsSevBad {
		t.Errorf("severity = %q, want bad — Kubernetes files this as Normal and it is not", killed.Sev)
	}
	if killed.Level != "NORMAL" {
		t.Errorf("level = %q; the point of the rule is that the type is Normal", killed.Level)
	}
	if killed.Peer == "" {
		t.Error("the killed object was not named, so the row cannot be lined up with its own log")
	}
}

// TestK8sEventCountsBecomeARepeat. One Event object carries a count and a first/last
// timestamp — exactly the shape a folded log record has — so forty probe failures are one
// row with a span rather than forty rows or one.
func TestK8sEventCountsBecomeARepeat(t *testing.T) {
	b := lsLoadScenario(t, "ko-ps-primary-kill")
	found := false
	for _, e := range b.Events {
		if !lsSrcIs(b, e.Src, lsFlavourK8sEvents) || e.Repeat <= 1 {
			continue
		}
		found = true
		if e.EndTS > 0 && e.EndTS < e.TS {
			t.Errorf("%q ends before it starts", e.Label)
		}
	}
	if !found {
		t.Skip("no repeated Event in this capture")
	}
}

// TestCNPGPointInTimeRecovery. The `recoveryTarget` half of CloudNativePG's recovery, which
// §304 left undone. It is still a NEW cluster — CNPG never restores in place — so the
// finding must say so rather than reporting an outage that did not happen.
func TestCNPGPointInTimeRecovery(t *testing.T) {
	b := lsLoadScenario(t, "ko-cnpg-pitr")
	if f := lsHasFinding(b, "pgop-restore"); f != nil {
		t.Errorf("a CloudNativePG recovery was reported as an in-place restore: %s", f.Title)
	}
	f := lsHasFinding(b, "pgop-recovery-new")
	if f == nil {
		t.Fatal("no recovery finding on a bundle that recovered to a point in time")
	}
	if !strings.Contains(f.Advice, "still pointed at the ORIGINAL") {
		t.Errorf("advice does not say the application did not move: %s", f.Advice)
	}
}

// TestK8sEventsAreNotASerever guards the bug the rendered page caught: an Events feed was
// being counted in "time spent not answering queries" (`kubernetes: 37.7s not serving`)
// and painted red on the swimlane from the first kill onward.
//
// It is not a server. It has no state, it answers no queries, and a kill it reports is a
// fact about a POD — which has its own lane.
func TestK8sEventsAreNotAServer(t *testing.T) {
	b := lsLoadScenario(t, "ko-ps-primary-kill")
	var idx = -1
	for _, s := range b.Sources {
		if s.Engine == pktEngineK8sEvents {
			idx = s.Idx
		}
	}
	if idx < 0 {
		t.Fatal("no Events source in the bundle")
	}
	// No state track: every phase for it is UNKNOWN.
	for _, p := range b.Phases {
		if p.Src == idx && p.State != "UNKNOWN" {
			t.Errorf("the Events lane claims state %q", p.State)
		}
	}
	// And it is not a party to the unavailability measurement.
	if f := lsHasFinding(b, "unavailability"); f != nil {
		for _, s := range f.Sources {
			if s == idx {
				t.Error("the Events feed is counted in time spent not answering queries")
			}
		}
		if strings.Contains(f.Detail, "kubernetes:") {
			t.Errorf("the unavailability finding quotes the Events feed: %s", f.Detail)
		}
	}
}
