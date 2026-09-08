package main

// logsummary_grouprepl_test.go — the Log Summary against real Group Replication logs.
//
// Same rule as the PXC and asynchronous suites next door: every fixture came off a live
// cluster while the scenario in its directory name was performed on it. The `g*`
// directories are a three-node Percona Server 8.0.46-37 single-primary group, deployed by
// DBCanvas in both of its modes.
//
//	g01-bootstrap                  a group created from nothing, then joined by two members
//	g02-graceful-stop              systemctl stop mysqld on a secondary — the clean departure
//	g03-rejoin-recovery            the member started again and caught up incrementally
//	g04-crash-kill9                SIGKILL on a secondary under load: unreachable, expelled,
//	                               restarted by systemd — and never rejoined, because
//	                               group_replication_start_on_boot is OFF in raw GR mode
//	g05-primary-failover           SIGKILL on the PRIMARY under load, and the election
//	g06-partition-nomajority       one of two members cut off port 33061 — BOTH sides lost
//	                               their majority and blocked every write
//	g07-forced-recovery            an operator bootstrapping the group out of that block
//	g08-divergent-rejoin           a member cycling donors, failing 1062 on each, forever
//	g09-clone-recovery             recovery by clone: data erased, then mysqld restarted
//	g10-errant-gtid                a member refused at the door for holding transactions the
//	                               group never saw — and left WRITABLE
//	g11-applier-conflict           a planted row breaking the applier: the member leaves
//	                               and IS set read-only, the opposite of g10
//	g12-crash-signal11             SIGSEGV under load — a real crash block
//	g13-join-timeout               a member that reached the group and never got a view
//	g14                            deliberately absent. It was the flow-control flood, and
//	                               it produced no log records at all on any member — see
//	                               lsFindingGRFlowControl. There is nothing to commit, and
//	                               the gap in the numbering is the point.
//	g15-innodbcluster-bootstrap    the same cluster built by MySQL Shell instead: healthy,
//	                               and carrying 26 [ERROR] records because of it
//	g16-innodbcluster-failover     SIGKILL on the Shell-managed primary — which rejoined by
//	                               itself, because Shell sets start_on_boot=ON

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------- recognition

func TestGRRecognisesGroupReplication(t *testing.T) {
	b := lsLoadScenario(t, "g01-bootstrap")
	if len(b.Sources) != 3 {
		t.Fatalf("want 3 sources, got %d", len(b.Sources))
	}
	for _, s := range b.Sources {
		if s.Flavour != lsFlavourGroupRepl {
			t.Errorf("%s: flavour %q, want %q", s.Name, s.Flavour, lsFlavourGroupRepl)
		}
		if s.Node == "" || !strings.HasPrefix(s.Node, "gr0") {
			t.Errorf("%s: node name %q, want the member's own host", s.Name, s.Node)
		}
	}
}

// A Group Replication member must NOT be mistaken for a Galera one, or for a standalone.
// The standalone path would seed it into RUNNING and paint a member that is not in its
// group as healthy — see lsResolveGroupRepl.
func TestGRIsNotGaleraOrStandalone(t *testing.T) {
	for _, name := range []string{"g01-bootstrap", "g05-primary-failover", "g15-innodbcluster-bootstrap"} {
		b := lsLoadScenario(t, name)
		for _, s := range b.Sources {
			if s.Flavour == lsFlavourGalera {
				t.Errorf("%s/%s: classified as Galera", name, s.Name)
			}
			if s.Flavour == lsFlavourMySQL {
				t.Errorf("%s/%s: classified as a plain MySQL server", name, s.Name)
			}
		}
	}
}

// A genuine asynchronous replica must keep its own classification now that the GR rules
// run first — the guard is that they all require a group_replication_ channel.
func TestGRDoesNotStealAsyncReplication(t *testing.T) {
	for _, name := range []string{"r02-dupkey-conflict", "r04-binlog-purged", "r09-source-unreachable"} {
		b := lsLoadScenario(t, name)
		for _, s := range b.Sources {
			if s.Flavour == lsFlavourGroupRepl {
				t.Errorf("%s/%s: an asynchronous replica classified as Group Replication", name, s.Name)
			}
		}
		for _, e := range b.Events {
			if strings.HasPrefix(e.Label, "Group Replication applier error") || e.Label == "Recovering from a donor" {
				t.Errorf("%s: async record %q claimed by a GR rule", name, e.Message)
			}
		}
	}
}

// And a PXC bundle must be untouched by any of this.
func TestGRLeavesGaleraAlone(t *testing.T) {
	b := lsLoadScenario(t, "s06-network-partition")
	for _, s := range b.Sources {
		if s.Flavour != lsFlavourGalera {
			t.Errorf("%s: flavour %q, want galera", s.Name, s.Flavour)
		}
	}
	for _, id := range []string{"gr-no-majority", "gr-flow-control", "gr-election"} {
		if f := lsHasFinding(b, id); f != nil {
			t.Errorf("GR finding %q fired on a PXC bundle: %s", id, f.Title)
		}
	}
}

// ---------------------------------------------------------------- membership

func TestGRBootstrapAndJoin(t *testing.T) {
	b := lsLoadScenario(t, "g01-bootstrap")
	if e := lsHasLabel(b, "Online in the group"); e == nil {
		t.Fatal("no member declared itself online")
	}
	// GR's view record is single-line and carries the whole membership, so the member
	// count comes straight out of it — no continuation-line folding needed.
	sizes := map[int]bool{}
	for _, e := range b.Events {
		if e.Code == "MY-011503" && e.Members > 0 {
			sizes[e.Members] = true
		}
	}
	for _, want := range []int{1, 2, 3} {
		if !sizes[want] {
			t.Errorf("no view with %d members; saw %v", want, sizes)
		}
	}
	if f := lsHasFinding(b, "gr-election"); f == nil || f.Sev != lsSevOK {
		t.Errorf("a bootstrap's first election should read as OK, got %+v", f)
	}
}

// The discriminator the corpus was built to settle: in Group Replication a clean stop and
// a death ARE distinguishable from the survivors' side, which is exactly what Galera
// cannot do. A clean stop has no 'unreachable' record before the removal; a SIGKILL does.
func TestGRCleanStopVersusKill(t *testing.T) {
	clean := lsLoadScenario(t, "g02-graceful-stop")
	if lsHasLabel(clean, "Peer unreachable") != nil {
		t.Error("a graceful stop produced an unreachable record — the whole discriminator is wrong")
	}
	if lsHasLabel(clean, "Members removed from the group") == nil {
		t.Error("a graceful stop should still remove the member from the group")
	}

	killed := lsLoadScenario(t, "g04-crash-kill9")
	un := lsHasLabel(killed, "Peer unreachable")
	if un == nil {
		t.Fatal("a SIGKILL produced no unreachable record")
	}
	gone := lsHasLabel(killed, "Members removed from the group")
	if gone == nil {
		t.Fatal("a SIGKILL produced no removal")
	}
	if gone.TS <= un.TS {
		t.Errorf("removal at %v should follow the unreachable record at %v", gone.TS, un.TS)
	}
	// The gap is the expel timeout, and it read 16.0s in both captures that produced one.
	if gap := gone.TS - un.TS; gap < 5 || gap > 60 {
		t.Errorf("expel gap %.1fs is outside anything the captures produced", gap)
	}
}

func TestGRPrimaryFailover(t *testing.T) {
	b := lsLoadScenario(t, "g05-primary-failover")
	if lsHasLabel(b, "Primary left — electing a new one") == nil {
		t.Fatal("no 'primary left' record")
	}
	f := lsHasFinding(b, "gr-election")
	if f == nil {
		t.Fatal("no election finding")
	}
	if f.Sev != lsSevWarn {
		t.Errorf("an election caused by a dead primary should be a warning, got %q", f.Sev)
	}
	// Three members all log the same election; it is one event, not three.
	if !strings.Contains(f.Title, "once") {
		t.Errorf("one election reported as %q — the per-member duplicates are not being folded", f.Title)
	}
	if !strings.Contains(f.Detail, "after it stopped answering") {
		t.Errorf("the write outage was not measured: %s", f.Detail)
	}
	// The new primary must be named, and it must not be the one that died.
	if !strings.Contains(f.Detail, "gr02") {
		t.Errorf("the elected primary is not named: %s", f.Detail)
	}
}

// ---------------------------------------------------------------- the quiet failures

// Both sides of a 1v1 split block every write, and neither ever says it recovered. The
// finding has to report that as still in force rather than as a past event.
func TestGRLostMajorityBlocksAndStays(t *testing.T) {
	b := lsLoadScenario(t, "g06-partition-nomajority")
	blocked := lsCountLabel(b, "Lost majority — updates blocked")
	if blocked != 2 {
		t.Fatalf("want both members blocked, got %d", blocked)
	}
	f := lsHasFinding(b, "gr-no-majority")
	if f == nil {
		t.Fatal("no lost-majority finding")
	}
	if f.Sev != lsSevBad {
		t.Errorf("severity %q, want bad", f.Sev)
	}
	if !strings.Contains(f.Title, "Every member") {
		t.Errorf("a whole-cluster block should say so, got %q", f.Title)
	}
	if !strings.Contains(f.Detail, "still refusing writes") {
		t.Errorf("an unrecovered block must be reported as still in force: %s", f.Detail)
	}
	// A blocked member is not serving, whatever it does for reads.
	if lsStateServes(lsStateBlocked) {
		t.Error("BLOCKED counts as serving — a cluster that takes no writes would read as available")
	}
}

// The counterpart: an operator got the group back, and MY-011498 is the only thing that
// ends a block.
func TestGRMajorityRegained(t *testing.T) {
	b := lsLoadScenario(t, "g07-forced-recovery")
	if lsHasLabel(b, "Majority regained — updates flowing again") == nil {
		t.Fatal("no 'resumed contact' record in the recovery fixture")
	}
}

// A server that came back up and never rejoined. Nothing in the log says so — the finding
// is built on the absence of a plugin start after the last start-up.
func TestGRNeverRejoined(t *testing.T) {
	b := lsLoadScenario(t, "g04-crash-kill9")
	f := lsHasFinding(b, "gr-never-rejoined")
	if f == nil {
		t.Fatal("no 'never rejoined' finding for the restarted member")
	}
	if f.Sev != lsSevBad {
		t.Errorf("severity %q, want bad", f.Sev)
	}
	if !strings.Contains(f.Detail, "gr03") {
		t.Errorf("the stranded member is not named: %s", f.Detail)
	}
	// And the state must not read as healthy for the rest of the window.
	if lsStateServes(lsStateOffline) {
		t.Error("OFFLINE counts as serving — a server outside its cluster would paint green")
	}
}

// The healthy group must NOT produce it, or the check is worthless.
func TestGRNeverRejoinedIsQuietWhenFine(t *testing.T) {
	for _, name := range []string{"g01-bootstrap", "g03-rejoin-recovery", "g16-innodbcluster-failover"} {
		b := lsLoadScenario(t, name)
		if f := lsHasFinding(b, "gr-never-rejoined"); f != nil {
			t.Errorf("%s: false positive — %s", name, f.Detail)
		}
	}
}

// ---------------------------------------------------------------- divergence

// The two exits from the group, and the fact that they end in opposite places. This is the
// pair the whole catalogue was worth writing for.
func TestGRTwoExitsHaveOppositeReadOnly(t *testing.T) {
	// Refused at join: left writable.
	refused := lsLoadScenario(t, "g10-errant-gtid")
	if lsHasLabel(refused, "Diverged: has transactions the group does not") == nil {
		t.Fatal("the errant-GTID rejection was not recognised")
	}
	if lsHasLabel(refused, "super_read_only set OFF") == nil {
		t.Fatal("the capture shows super_read_only going OFF after the refusal; it was not classified")
	}
	sb := lsHasFinding(refused, "gr-writable-outside-group")
	if sb == nil {
		t.Fatal("no split-brain finding for a member left writable outside the group")
	}
	if sb.Sev != lsSevBad {
		t.Errorf("severity %q, want bad", sb.Sev)
	}

	// Applier failure: set read-only on the way out.
	applier := lsLoadScenario(t, "g11-applier-conflict")
	if lsHasLabel(applier, "Applier failed — member leaving the group") == nil {
		t.Fatal("the applier failure was not recognised")
	}
	if lsHasLabel(applier, "Set read-only after an error") == nil {
		t.Fatal("MY-011712 was not recognised")
	}
	if f := lsHasFinding(applier, "gr-writable-outside-group"); f != nil {
		t.Errorf("a member set read-only on its way out must not be reported as writable: %s", f.Detail)
	}
}

func TestGRRefusedForDivergence(t *testing.T) {
	b := lsLoadScenario(t, "g10-errant-gtid")
	f := lsHasFinding(b, "gr-diverged")
	if f == nil {
		t.Fatal("no divergence finding")
	}
	// The GTID sets are the evidence and must survive into the finding.
	if !strings.Contains(f.Detail, "extra transactions on this member") {
		t.Errorf("the local GTID set was not carried into the finding: %s", f.Detail)
	}
	if !strings.Contains(f.Advice, "clone threshold does not help") && !strings.Contains(f.Advice, "GTID comparison happens first") {
		t.Errorf("the advice should record that raising the clone threshold cannot fix this: %s", f.Advice)
	}
}

// A member cycling donors forever, never coming online, never reporting a final failure.
func TestGRStuckRecovery(t *testing.T) {
	b := lsLoadScenario(t, "g08-divergent-rejoin")
	if lsCountLabel(b, "Recovery attempt failed to apply") < 2 {
		t.Fatal("the recovery retry loop was not recognised")
	}
	f := lsHasFinding(b, "gr-stuck-recovery")
	if f == nil {
		t.Fatal("no stuck-recovery finding")
	}
	if f.Sev != lsSevBad {
		t.Errorf("severity %q, want bad", f.Sev)
	}
	if !strings.Contains(f.Detail, "donor") {
		t.Errorf("the donor cycling is the symptom and should be named: %s", f.Detail)
	}
	// A recovery failure is not the same as an applier failure that ejects the member:
	// this one keeps the member in the group, uselessly.
	if lsHasLabel(b, "Applier failed — member leaving the group") != nil {
		t.Error("a recovery-channel failure was reported as the member leaving the group")
	}
}

func TestGRJoinTimeout(t *testing.T) {
	b := lsLoadScenario(t, "g13-join-timeout")
	if lsHasLabel(b, "Timed out waiting for a view after joining") == nil {
		t.Fatal("MY-011640 was not recognised")
	}
	if f := lsHasFinding(b, "gr-join-timeout"); f == nil {
		t.Fatal("no join-timeout finding")
	}
}

// ---------------------------------------------------------------- recovery by clone

func TestGRCloneRecovery(t *testing.T) {
	b := lsLoadScenario(t, "g09-clone-recovery")
	if lsHasLabel(b, "Recovery will clone the whole dataset") == nil {
		t.Fatal("the clone recovery method was not recognised")
	}
	if lsHasLabel(b, "Clone is erasing this member's data") == nil {
		t.Fatal("MY-013460 — the data erasure — was not recognised")
	}
	f := lsHasFinding(b, "gr-clone")
	if f == nil {
		t.Fatal("no clone finding")
	}
	// The restart is the part that misleads, so the finding has to name it.
	if !strings.Contains(f.Detail, "restart") {
		t.Errorf("the clone's own restart of mysqld is not explained: %s", f.Detail)
	}
	// And the incremental path must not be reported as a clone.
	inc := lsLoadScenario(t, "g03-rejoin-recovery")
	if f := lsHasFinding(inc, "gr-clone"); f != nil {
		t.Errorf("an incremental recovery reported as a clone: %s", f.Detail)
	}
	if lsHasLabel(inc, "Recovery method chosen") == nil {
		t.Fatal("the incremental recovery method record was not recognised")
	}
}

func TestGRRecoveryNamesItsDonor(t *testing.T) {
	b := lsLoadScenario(t, "g03-rejoin-recovery")
	e := lsHasLabel(b, "Recovering from a donor")
	if e == nil {
		t.Fatal("the recovery donor record was not recognised")
	}
	if e.Peer == "" || !strings.HasPrefix(e.Peer, "gr0") {
		t.Errorf("donor not extracted, got %q", e.Peer)
	}
}

// ---------------------------------------------------------------- InnoDB Cluster

// A healthy, freshly built InnoDB Cluster carries 26 [ERROR] records, every one of them
// MySQL Shell testing the instance. If those are trusted, every new cluster reports broken.
func TestInnoDBClusterShellProbeIsNotAFailure(t *testing.T) {
	b := lsLoadScenario(t, "g15-innodbcluster-bootstrap")

	// The raw material: the fixture really does contain that many error-level records.
	rawErrors := 0
	for _, s := range b.Sources {
		data := s // silence the loop-variable lint by using it
		_ = data
	}
	for _, e := range b.Events {
		if strings.EqualFold(e.Level, "ERROR") {
			rawErrors++
		}
	}
	if rawErrors < 20 {
		t.Fatalf("fixture no longer carries the Shell probe errors (%d error-level records)", rawErrors)
	}

	// And the classifier must not call them failures.
	for _, e := range b.Events {
		if strings.Contains(e.Message, "mysqlsh.test") && e.Sev == lsSevBad {
			t.Errorf("Shell probe reported as a failure: %s", e.Message)
		}
	}
	f := lsHasFinding(b, "gr-shell-probe")
	if f == nil {
		t.Fatal("no finding explaining the Shell probe records")
	}
	if f.Sev != lsSevInfo {
		t.Errorf("severity %q, want info", f.Sev)
	}
	if !strings.Contains(f.Detail, "supposed to fail") {
		t.Errorf("the finding should say the probes are meant to fail: %s", f.Detail)
	}
}

// The Shell-managed cluster sets group_replication_start_on_boot=ON, so the same SIGKILL
// that strands a raw member leaves this one to rejoin by itself. That contrast is the
// single most useful thing to know about the two modes.
func TestInnoDBClusterRejoinsItself(t *testing.T) {
	b := lsLoadScenario(t, "g16-innodbcluster-failover")
	if lsHasLabel(b, "New primary elected") == nil {
		t.Fatal("no election in the InnoDB Cluster failover fixture")
	}
	if lsHasLabel(b, "Online in the group") == nil {
		t.Fatal("the killed member never came back online")
	}
	if f := lsHasFinding(b, "gr-never-rejoined"); f != nil {
		t.Errorf("a member that DID rejoin was reported as stranded: %s", f.Detail)
	}
}

// ---------------------------------------------------------------- honesty

// Flow control leaves nothing in the log, so the finding must be present unconditionally
// and must not pretend the silence means anything.
func TestGRFlowControlIsAlwaysStated(t *testing.T) {
	b := lsLoadScenario(t, "g01-bootstrap")
	f := lsHasFinding(b, "gr-flow-control")
	if f == nil {
		t.Fatal("the flow-control note is missing from a Group Replication bundle")
	}
	if !strings.Contains(f.Advice, "replication_group_member_stats") {
		t.Errorf("the finding must name where the numbers actually live: %s", f.Advice)
	}
	if f.Sev != lsSevInfo {
		t.Errorf("severity %q, want info — it is a statement about the log, not a fault", f.Sev)
	}
}

// A crash block is a crash block whatever the cluster technology.
func TestGRCrashBlock(t *testing.T) {
	b := lsLoadScenario(t, "g12-crash-signal11")
	// lsEnrichCrash appends the signal to the label, so match the prefix.
	found := false
	for _, e := range b.Events {
		if strings.HasPrefix(e.Label, "Server crashed") {
			found = true
			if !strings.Contains(e.Label, "signal 11") {
				t.Errorf("the signal is not carried into the label: %q", e.Label)
			}
		}
	}
	if !found {
		t.Fatal("the SIGSEGV crash block was not recognised")
	}
	if f := lsHasFinding(b, "crash"); f == nil {
		t.Fatal("no crash finding")
	}
}

// ---------------------------------------------------------------- states

func TestGRStatesAreDistinctFromGalera(t *testing.T) {
	// The severities must line up with what each state means for a reader.
	for state, want := range map[string]string{
		lsStateOnline:     lsSevOK,
		lsStateRecovering: lsSevWarn,
		lsStateBlocked:    lsSevBad,
		lsStateGRError:    lsSevBad,
		lsStateOffline:    lsSevBad,
	} {
		if got := lsStateSev(state); got != want {
			t.Errorf("state %s: severity %q, want %q", state, got, want)
		}
		if lsStateMeaning[state] == "" {
			t.Errorf("state %s has no explanation for the legend", state)
		}
	}
	if lsStateOnline == lsStateSynced {
		t.Error("GR's ONLINE and Galera's SYNCED must stay separate words")
	}

	// Every state the Go side can produce needs an explanation, whichever machine it
	// belongs to: the swimlane legend renders lsStateMeaning, and a state without one
	// shows a reader a coloured stripe and no way to find out what it means.
	for _, state := range []string{
		lsStateSynced, lsStateJoined, lsStateJoiner, lsStateDonor, lsStatePrim,
		lsStateOpen, lsStateClosed, lsStateDown, lsStateUp, lsStateStarting,
		lsStateOnline, lsStateRecovering, lsStateGRError, lsStateOffline, lsStateBlocked,
	} {
		if lsStateMeaning[state] == "" {
			t.Errorf("state %s has no entry in lsStateMeaning", state)
		}
	}
}

// The phase track has to move a member through its states, not leave it at whatever it was
// when the excerpt started.
func TestGRPhasesFollowTheMember(t *testing.T) {
	b := lsLoadScenario(t, "g01-bootstrap")
	seen := map[string]bool{}
	for _, p := range b.Phases {
		seen[p.State] = true
	}
	if !seen[lsStateOnline] {
		t.Errorf("no member ever reached ONLINE; states seen: %v", seen)
	}
}
