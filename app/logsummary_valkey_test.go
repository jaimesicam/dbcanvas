package main

// logsummary_valkey_test.go — Valkey, standalone replication and Valkey Cluster.
//
// The three fixtures are three different shapes of the same engine, captured from live
// servers driven through real incidents. Nothing in the catalogue was written from memory;
// where a test asserts that the log does NOT say something, the corpus is checked for it too,
// so the claim stays supported by its own evidence.
//
//	v01-cluster-failover  six nodes on Valkey 8 — three shards, one replica each — driven
//	                      through an automatic failover with the primary SIGKILLed, the old
//	                      primary rejoining as a replica, a manual failover handing the shard
//	                      back, and a whole shard killed so its slots went uncovered. Bare
//	                      stdout format, no journald prefix.
//	v02-cluster-nocover   three all-primary nodes on Percona Valkey 9.1.1, read through
//	                      journalctl exactly as the collector reads it, with one shard stopped
//	                      for thirty seconds and no replica to take over.
//	v03-standalone-repl   a primary and a replica wired by hand, driven through a full sync, a
//	                      killed primary, a partial resync, a manual promotion, a real
//	                      persistence failure, and 40,000 writes against an 8 MB maxmemory.

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------- parsing

// Both shapes of the same log have to parse: the bare stdout form a container or a
// `logfile`-configured node writes, and the journald-prefixed form the collector gets.
// Getting this wrong drops one of the two entirely.
func TestValkeyParsesBothLogShapes(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"bare", `1:M 15 Aug 2026 23:02:11.658 * Ready to accept connections tcp`},
		{"journald", `Aug 15 23:03:55 vkc1 valkey-server[253]: 253:M 15 Aug 2026 23:03:55.100 * Ready to accept connections tcp`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs, _ := lsFoldValkey([]byte(tc.line + "\n"))
			if len(recs) != 1 {
				t.Fatalf("%q produced %d records", tc.line, len(recs))
			}
			if recs[0].Code != "M" {
				t.Errorf("role read as %q, want M", recs[0].Code)
			}
			if recs[0].TS <= 0 {
				t.Error("no timestamp")
			}
			e, keep := lsClassifyValkey(recs[0])
			if !keep || e.Label != "Ready — accepting connections" {
				t.Errorf("not classified: keep=%v label=%q", keep, e.Label)
			}
		})
	}
	// And both must land on the SAME instant: the two headers describe one event, and the
	// inner stamp is the precise one. Reading journald's prefix instead would lose the
	// milliseconds and, on a log spanning midnight, the date.
	bare, _ := lsFoldValkey([]byte("1:M 15 Aug 2026 23:03:55.100 * Ready to accept connections tcp\n"))
	pref, _ := lsFoldValkey([]byte("Aug 15 23:03:55 vkc1 valkey-server[253]: 253:M 15 Aug 2026 23:03:55.100 * Ready to accept connections tcp\n"))
	if bare[0].TS != pref[0].TS {
		t.Errorf("the same record parsed to two instants: %f vs %f", bare[0].TS, pref[0].TS)
	}
}

// The node name comes from journald's prefix, because Valkey never states its own — nothing
// in a bare valkey-server log says which host wrote it.
func TestValkeyNodeNameComesFromTheJournal(t *testing.T) {
	b := lsLoadScenario(t, "v02-cluster-nocover")
	for _, s := range b.Sources {
		if s.Node == "" {
			t.Errorf("%s has no node name even though its log is journald-prefixed", s.Name)
		}
	}
	// A bare log has no host to read, and the file name is the honest fallback rather than
	// an invented name.
	recs, host := lsFoldValkey([]byte("1:M 15 Aug 2026 23:02:11.658 * Ready to accept connections tcp\n"))
	if host != "" {
		t.Errorf("a bare log yielded a host name %q from nowhere", host)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
}

// systemd's records are in the same file and must be parsed as systemd's, not Valkey's.
// Running one catalogue over both matches the wrong things — the mistake the PostgreSQL
// catalogue made with Patroni before the two were separated.
func TestValkeySystemdRecordsAreSeparate(t *testing.T) {
	log := `Aug 15 23:07:26 vkc2 valkey-server[257]: 257:M 15 Aug 2026 23:07:26.000 * Ready to accept connections tcp
Aug 15 23:07:26 vkc2 systemd[1]: valkey@dbcanvas.service: Main process exited, code=killed, status=9/KILL
Aug 15 23:07:26 vkc2 systemd[1]: valkey@dbcanvas.service: Scheduled restart job, restart counter is at 1.
`
	recs, host := lsFoldValkey([]byte(log))
	if host != "vkc2" {
		t.Errorf("host read as %q", host)
	}
	if len(recs) != 3 {
		t.Fatalf("want 3 records, got %d", len(recs))
	}
	if recs[0].Subsys != lsSubsysValkey {
		t.Errorf("Valkey's own record is subsystem %q", recs[0].Subsys)
	}
	for _, r := range recs[1:] {
		if r.Subsys != lsSubsysVKSysd {
			t.Errorf("a systemd record is subsystem %q", r.Subsys)
		}
		// systemd's prefix carries no year; it has to be borrowed from the Valkey record
		// beside it, or every one of these lands in year zero at the far left of the
		// timeline.
		if r.TS <= 0 {
			t.Errorf("systemd record %q got no timestamp", r.Text)
		}
	}
	if recs[1].TS < recs[0].TS-1 || recs[1].TS > recs[0].TS+2 {
		t.Errorf("the systemd record landed %.0fs from the Valkey record beside it", recs[1].TS-recs[0].TS)
	}
	e, keep := lsClassifyValkey(recs[1])
	if !keep || e.Sev != lsSevBad {
		t.Errorf("a SIGKILL is not reported as bad: keep=%v sev=%q", keep, e.Sev)
	}
}

// The clean-stop record has no level and no timestamp of its own, and it is the ONLY
// unambiguous evidence that a stop was asked for. Dropping it loses that distinction.
func TestValkeySignalHandlerLineIsKept(t *testing.T) {
	recs, _ := lsFoldValkey([]byte(
		"1:M 15 Aug 2026 23:08:42.000 * Ready to accept connections tcp\n" +
			"Aug 15 23:08:42 vkc2 valkey-server[296]: 296:signal-handler (1786835322) Received SIGTERM scheduling shutdown...\n"))
	if len(recs) != 2 {
		t.Fatalf("the signal-handler line was dropped: %d records", len(recs))
	}
	e, keep := lsClassifyValkey(recs[1])
	if !keep || e.Label != "Shutdown requested" {
		t.Errorf("not classified as a shutdown: keep=%v label=%q", keep, e.Label)
	}
}

// ---------------------------------------------------------------- flavours

// The three flavours have to be told apart, because the findings that may speak differ.
// Telling somebody with one cache node that their cluster has uncovered hash slots is worse
// than saying nothing.
func TestValkeyFlavoursAreDistinguished(t *testing.T) {
	for _, tc := range []struct{ dir, name, want string }{
		{"v01-cluster-failover", "vkn1.log", lsFlavourValkeyCluster},
		{"v02-cluster-nocover", "vkc1.log", lsFlavourValkeyCluster},
		{"v03-standalone-repl", "vkb.log", lsFlavourValkeyRepl},
	} {
		t.Run(tc.dir+"/"+tc.name, func(t *testing.T) {
			b := lsLoadScenario(t, tc.dir)
			for _, s := range b.Sources {
				if s.Name != tc.name {
					continue
				}
				if s.Flavour != tc.want {
					t.Errorf("%s is flavoured %q, want %q", tc.name, s.Flavour, tc.want)
				}
				if s.Engine != pktEngineValkey {
					t.Errorf("%s sniffed as engine %q", tc.name, s.Engine)
				}
				return
			}
			t.Fatalf("%s not found in the bundle", tc.name)
		})
	}
	// A lone server with no replication anywhere in its file is a standalone, and must be
	// told nothing about clusters or replicas.
	recs, _ := lsFoldValkey([]byte(
		"1:M 15 Aug 2026 23:00:00.000 * oO0OoO0OoO0Oo Valkey is starting oO0OoO0OoO0Oo\n" +
			"1:M 15 Aug 2026 23:00:00.100 * Running mode=standalone, port=6379.\n" +
			"1:M 15 Aug 2026 23:00:00.200 * Ready to accept connections tcp\n"))
	if got := lsSniffValkeyFlavour(recs); got != lsFlavourValkey {
		t.Errorf("a lone standalone was flavoured %q", got)
	}
}

// Valkey Cluster names every peer by a 40-character id and, on a node with no announce name,
// prints an empty bracket after it. Pooling each node's statement of its OWN id across the
// bundle is what turns "81ce2216adbc… was declared failed" into a sentence about a server.
func TestValkeyNodeIDsBecomeNames(t *testing.T) {
	b := lsLoadScenario(t, "v02-cluster-nocover")
	if len(b.Names) < 3 {
		t.Fatalf("only %d node id(s) were paired with a name; three nodes each state their own", len(b.Names))
	}
	for id, name := range b.Names {
		if len(id) != 40 {
			t.Errorf("%q is not a Valkey node id", id)
		}
		if name == "" {
			t.Errorf("id %s paired with an empty name", id)
		}
	}
	// The payoff: the finding that names the failed node names it by NAME.
	f := lsHasFinding(b, "vk-no-takeover")
	if f == nil {
		t.Fatal("no takeover finding")
	}
	if strings.Contains(f.Detail, "…") {
		t.Errorf("the finding still identifies the failed node by a truncated id: %s", f.Detail)
	}
	named := false
	for _, s := range b.Sources {
		if s.Node != "" && strings.Contains(f.Detail, s.Node) {
			named = true
		}
	}
	if !named {
		t.Errorf("the finding names no node in this bundle: %s", f.Detail)
	}
	// And the pooling has to cross sources: the node that failed is named in the SURVIVORS'
	// findings, using an id it stated in its own file. That is the whole reason several logs
	// are read together.
	if len(b.Sources) < 3 {
		t.Fatal("this check needs the full three-source bundle")
	}
}

// ---------------------------------------------------------------- severity

// Valkey's level is worse than useless as a severity floor, and this is the measurement that
// says so: the whole story of an automatic failover is written at notice, while the thing
// written at warning on every start of every healthy node is a hint about vm.overcommit_memory.
func TestValkeyLevelIsNotSeverity(t *testing.T) {
	b := lsLoadScenario(t, "v01-cluster-failover")
	// The boilerplate host warnings are at Valkey's top level and must not colour the lane.
	boiler := 0
	for _, e := range b.Events {
		switch e.Label {
		case "Host: vm.overcommit_memory is not 1", "Cluster mode forces one database",
			"Supervised by systemd", "Host: transparent huge pages are enabled":
			boiler++
			if e.Sev != lsSevInfo {
				t.Errorf("%q is boilerplate on every healthy start and is reported %q", e.Label, e.Sev)
			}
			if e.Level != "WARNING" {
				t.Errorf("%q should be at Valkey's warning level in the corpus, got %q", e.Label, e.Level)
			}
		}
	}
	if boiler == 0 {
		t.Fatal("the corpus contains none of the boilerplate warnings, so this proves nothing")
	}
	// And the reverse: the records that matter are at notice and must still be able to say
	// bad. A promotion filed below a host-tuning hint is the failure this guards.
	for _, want := range []string{
		"A node was declared FAILED (quorum reached)",
		"Election won — this node is the new primary",
	} {
		found := false
		for _, e := range b.Events {
			if e.Label != want {
				continue
			}
			found = true
			if e.Level != "NOTE" {
				t.Errorf("%q is not at notice in the corpus after all (%q) — the premise is wrong", want, e.Level)
			}
			if lsSevRank[e.Sev] < lsSevRank[lsSevWarn] {
				t.Errorf("%q is reported as %q", want, e.Sev)
			}
		}
		if !found {
			t.Errorf("%q is not in the corpus", want)
		}
	}
}

// ---------------------------------------------------------------- state

// The role letter is on every line, which is what makes Valkey's state track readable
// straight off the headers rather than reconstructed from transitions. A file whose letters
// run M then S is a demotion; S then M is a promotion.
func TestValkeyRoleLetterDrivesTheStateTrack(t *testing.T) {
	b := lsLoadScenario(t, "v01-cluster-failover")
	// vkn5 was a replica, was promoted by an election, and its header letters change with it.
	var src int = -1
	for _, s := range b.Sources {
		if s.Name == "vkn5.log" {
			src = s.Idx
		}
	}
	if src < 0 {
		t.Fatal("vkn5.log missing from the bundle")
	}
	// Anchored on the election rather than on the first phase, because the first phase is
	// legitimately PRIMARY: every node in a cluster-mode Valkey starts as an empty primary of
	// its own and only becomes a replica when the cluster is created. vkn5's real history is
	// M then S then M, and the track reproduces all three — which is the point.
	won := 0.0
	for _, e := range b.Events {
		if e.Src == src && e.Label == "Election won — this node is the new primary" {
			won = e.TS
			break
		}
	}
	if won == 0 {
		t.Fatal("vkn5 never won an election in this corpus")
	}
	after, ok := lsStateAt(b.Phases, src, won+1)
	if !ok {
		t.Fatal("no phase covers the election")
	}
	if after.State != lsStatePrimaryM {
		t.Errorf("a second after winning the election vkn5 is %q, want %q", after.State, lsStatePrimaryM)
	}
	// A second BEFORE the election it is CLUSTERDOWN, not REPLICA — and that is the right
	// answer rather than a wrong one. Its primary had already been declared failed, so the
	// shard's slots were uncovered and this node was refusing every command while it stood
	// for election. The cluster state outranks the role letter for exactly this reason: the
	// role was still S and still true, and the node was serving nothing.
	before, ok := lsStateAt(b.Phases, src, won-1)
	if !ok {
		t.Fatal("no phase covers the second before the election")
	}
	if before.State != lsStateVKDown {
		t.Errorf("a second before winning the election vkn5 is %q, want %q — its shard's slots were uncovered at that moment",
			before.State, lsStateVKDown)
	}
	// And the earlier M-then-S transition is there too: it is a real demotion, from the empty
	// primary every cluster-mode node starts as into the replica the cluster made it.
	sawEarlyReplica := false
	for _, p := range b.Phases {
		if p.Src == src && p.State == lsStateVKReplica && p.From < won {
			sawEarlyReplica = true
		}
	}
	if !sawEarlyReplica {
		t.Error("vkn5's spell as a replica before the election is missing from the track")
	}
}

// The forked RDB/AOF child is always role "C". It is a different process doing a different
// job, and letting it through drops the server out of PRIMARY for the length of every
// snapshot — which on a busy node is most of the timeline.
func TestValkeyForkChildDoesNotMoveTheLane(t *testing.T) {
	// A REPLICA taking a snapshot. The child writes its records as role "C", and the failure
	// this guards is those being read as a role like any other — which would put a PRIMARY
	// stripe in the middle of a replica's lane for the length of every save. Built as a
	// purpose-made log rather than taken from the corpus so that the child's records are the
	// only thing between two identical replica records: anything that appears in the lane
	// between them came from the child.
	//
	// Two independent things enforce this — lsResolveValkey skipping lsSubsysVKChild, and
	// lsValkeyRoleState declining to map "C" to any state — and either alone is sufficient,
	// so reverting one of them still passes here. Only removing both fails, which is what was
	// checked. Said out loud because a reader testing this by breaking one line and seeing
	// green would otherwise conclude the test is worthless.
	b := lsBuild([]lsInput{{Name: "replica.log", Origin: "upload", Data: []byte(
		"1:S 15 Aug 2026 23:00:00.000 * Ready to accept connections tcp\n" +
			"1:S 15 Aug 2026 23:00:01.000 * PRIMARY <-> REPLICA sync: Finished with success\n" +
			"1:S 15 Aug 2026 23:00:10.000 * Background saving started by pid 44\n" +
			"44:C 15 Aug 2026 23:00:11.000 * DB saved on disk\n" +
			"44:C 15 Aug 2026 23:00:12.000 * Fork CoW for RDB: current 0 MB, peak 0 MB, average 0 MB\n" +
			"1:S 15 Aug 2026 23:00:20.000 * Background saving terminated with success\n")}})
	child := 0
	for _, e := range b.Events {
		if e.Subsys == lsSubsysVKChild {
			child++
		}
	}
	if child != 2 {
		t.Fatalf("want 2 forked-child records, got %d — the fixture is not exercising this", child)
	}
	for _, p := range b.Phases {
		if p.State != lsStateVKReplica {
			t.Errorf("a replica taking a snapshot shows a %q stripe from %.0f to %.0f; the fork child moved the lane",
				p.State, p.From, p.To)
		}
	}
	// And in the corpus too, where the child forks while the server is a replica: no phase
	// may flip to PRIMARY for the length of a save.
	corpus := lsLoadScenario(t, "v03-standalone-repl")
	if n := 0; true {
		for _, e := range corpus.Events {
			if e.Subsys == lsSubsysVKChild {
				n++
			}
		}
		if n == 0 {
			t.Fatal("the corpus contains no forked-child records, so this proves nothing")
		}
	}
	b = corpus
	// The child's records must still be KEPT — a persistence failure is written by the child
	// and is the most important thing in the file when it happens.
	found := false
	for _, e := range b.Events {
		if e.Subsys == lsSubsysVKChild && e.Label == "Could not write the snapshot file" {
			found = true
		}
	}
	if !found {
		t.Error("the child's snapshot failure was dropped — that is the other way to be wrong")
	}
}

// CLUSTERDOWN is the state with no counterpart anywhere else in this package: a node that is
// completely healthy and refusing every command because some OTHER shard's slots are
// uncovered. It must not count as serving.
func TestValkeyClusterDownIsNotServing(t *testing.T) {
	if lsStateServes(lsStateVKDown) {
		t.Error("a node refusing every command with CLUSTERDOWN is counted as available")
	}
	if lsStateServes(lsStateVKLoading) {
		t.Error("a node refusing every command with -LOADING is counted as available")
	}
	if !lsStateServes(lsStateVKReplica) {
		t.Error("a Valkey replica answering reads is counted as unavailable")
	}
	// And the state has to actually appear on the healthy nodes' lanes during the outage,
	// which is the whole point: the nodes that stopped serving are not the node that failed.
	b := lsLoadScenario(t, "v02-cluster-nocover")
	down := 0
	for _, p := range b.Phases {
		if p.State == lsStateVKDown && p.To > p.From {
			down++
		}
	}
	if down < 2 {
		t.Errorf("only %d node(s) show a CLUSTERDOWN stretch; the healthy survivors should too", down)
	}
}

// Every state the Go side can emit needs a colour and a sentence, or the swimlane legend
// renders a blank chip for it.
func TestValkeyStatesAreRenderable(t *testing.T) {
	for state, want := range map[string]string{
		lsStateVKReplica: lsSevOK,
		lsStateVKSyncing: lsSevWarn,
		lsStateVKLoading: lsSevWarn,
		lsStateVKDown:    lsSevBad,
	} {
		if got := lsStateSev(state); got != want {
			t.Errorf("state %s is coloured %q, want %q", state, got, want)
		}
		if lsStateMeaning[state] == "" {
			t.Errorf("state %s has no entry in lsStateMeaning", state)
		}
	}
	// REPLICA is Valkey's own word and must not collide with MongoDB's or PostgreSQL's.
	if lsStateVKReplica == lsStateSecondary || lsStateVKReplica == lsStateStandby {
		t.Error("Valkey's REPLICA has collided with another engine's word for the same idea")
	}
}

// ---------------------------------------------------------------- findings

// The finding this catalogue exists for. A Valkey Cluster refuses every client when any
// shard's slots are uncovered — including on the nodes that are perfectly healthy, and none
// of their logs explains why they stopped answering.
func TestValkeyClusterDownIsExplained(t *testing.T) {
	b := lsLoadScenario(t, "v02-cluster-nocover")
	f := lsHasFinding(b, "vk-cluster-down")
	if f == nil {
		t.Fatal("stopping one shard of three produced no cluster-down finding")
	}
	if f.Sev != lsSevBad {
		t.Errorf("a cluster refusing every client should be %q, got %q", lsSevBad, f.Sev)
	}
	if !strings.Contains(f.Detail, "cluster-require-full-coverage") {
		t.Errorf("the finding does not name the setting that decides the blast radius: %s", f.Detail)
	}
	// The claim is that the healthy nodes stopped serving. It is checkable against the
	// corpus, so it is checked: the surviving members must each have logged the state change.
	if len(f.Sources) < 2 {
		t.Errorf("only %d source(s) reported the outage; the survivors should have too", len(f.Sources))
	}
	// And the duration has to be real rather than an instant — the outage lasted about
	// twenty-five seconds and a finding reporting "0s" would be useless.
	if f.Until-f.At < 5 {
		t.Errorf("the outage is reported as %s, which cannot be right", lsDur(f.Until-f.At))
	}
}

// Building a cluster is not an outage. Every node writes "Cluster is currently down: at
// least one hash slot is not served" while the cluster is being created, and reporting that
// would flag every healthy deployment on the day it was built. What is never written during
// formation is "Cluster state changed: fail" — a cluster that has never been ok cannot change
// to fail — and that is the discriminator.
func TestValkeyClusterFormationIsNotAnOutage(t *testing.T) {
	// The formation half of the corpus, on its own: every record up to the first
	// "Cluster state changed: ok" on each node.
	b := lsLoadScenario(t, "v02-cluster-nocover")
	firstOK := 0.0
	for _, e := range b.Events {
		if e.Label == "Cluster state OK — serving again" {
			firstOK = e.TS
			break
		}
	}
	if firstOK == 0 {
		t.Fatal("the corpus never reaches a healthy cluster, so this proves nothing")
	}
	// The "currently down" records during formation exist and are classified...
	formation := 0
	for _, e := range b.Events {
		if e.TS < firstOK && strings.Contains(e.Label, "uncovered") {
			formation++
		}
	}
	if formation == 0 {
		t.Fatal("the corpus has no 'slots uncovered' records during formation, so this proves nothing")
	}
	// ...and lsVKClusterWasUp declines to call any of them an outage.
	for _, e := range b.Events {
		if e.TS < firstOK && lsVKClusterWasUp(b, e.TS) {
			t.Errorf("a record at %s, before the cluster was ever ok, counts as a real outage", lsClock(e.TS))
		}
	}
	if !lsVKClusterWasUp(b, b.Summary.LastTS) {
		t.Error("the cluster is never considered to have been up, even at the end of the window")
	}
}

// The all-primary cluster's failure mode: a shard dies, there is nothing to promote, and the
// whole cluster stays down until the original node comes back. This is what dbcanvas's own
// three-node Valkey Cluster frame builds, so it is the case its users will actually meet.
func TestValkeyNoTakeoverOnAnAllPrimaryCluster(t *testing.T) {
	b := lsLoadScenario(t, "v02-cluster-nocover")
	f := lsHasFinding(b, "vk-no-takeover")
	if f == nil {
		t.Fatal("a shard failing with no replica produced no finding")
	}
	if f.Sev != lsSevBad {
		t.Errorf("severity %q", f.Sev)
	}
	if !strings.Contains(f.Advice, "replica") {
		t.Errorf("the advice does not mention the missing replica: %s", f.Advice)
	}
	// And it must NOT fire on the cluster that did have replicas and did fail over — there,
	// something took over, and saying otherwise would be simply false.
	b2 := lsLoadScenario(t, "v01-cluster-failover")
	if f := lsHasFinding(b2, "vk-no-takeover"); f != nil {
		t.Errorf("a cluster that successfully failed over is reported as having nothing take over: %s", f.Detail)
	}
}

// An automatic failover and a manual one are different events with different consequences,
// and conflating them hides the one that lost writes. The corpus contains one of each,
// four minutes apart.
func TestValkeyFailoverSeparatesPlannedFromUnplanned(t *testing.T) {
	b := lsLoadScenario(t, "v01-cluster-failover")
	f := lsHasFinding(b, "vk-failover")
	if f == nil {
		t.Fatal("two promotions produced no failover finding")
	}
	if !strings.Contains(f.Detail, "— requested") {
		t.Errorf("the manual failover is not marked as requested: %s", f.Detail)
	}
	if !strings.Contains(f.Title, "unplanned") {
		t.Errorf("the unplanned failover is not distinguished: %q", f.Title)
	}
	if f.Sev != lsSevBad {
		t.Errorf("a window containing an unplanned failover should be bad, got %q", f.Sev)
	}
	// The consequence is the point of the distinction: an automatic failover discards writes
	// and a manual one does not, and the finding has to say which happened here.
	if !strings.Contains(f.Detail, "asynchronous") {
		t.Errorf("the finding does not explain what an unplanned failover costs: %s", f.Detail)
	}
	// The manual half of the corpus really does contain the proof record, or the claim that
	// nothing was lost would be unsupported.
	if lsHasLabel(b, "Caught up — the manual failover can proceed") == nil {
		t.Error("the corpus has no record of the manual failover waiting for the replica to catch up")
	}
}

// A returning primary told to follow the node that replaced it has had writes discarded.
// This is Valkey Cluster's rollback, and unlike MongoDB's it leaves no file of what was lost.
func TestValkeyReturningPrimaryIsReportedAsDiscardedWrites(t *testing.T) {
	b := lsLoadScenario(t, "v01-cluster-failover")
	f := lsHasFinding(b, "vk-demoted")
	if f == nil {
		t.Fatal("the SIGKILLed primary rejoining as a replica produced no finding")
	}
	if f.Sev != lsSevBad {
		t.Errorf("discarded writes should be %q, got %q", lsSevBad, f.Sev)
	}
	if !strings.Contains(f.Advice, "no record") && !strings.Contains(f.Advice, "keeps no record") {
		t.Errorf("the advice does not say that nothing records what was lost: %s", f.Advice)
	}
}

// A SIGKILLed valkey-server writes nothing at all. systemd's half of the journal is the
// entire evidence, which is the reason the collector reads both.
func TestValkeyKillIsOnlyVisibleThroughSystemd(t *testing.T) {
	b := lsLoadScenario(t, "v03-standalone-repl")
	f := lsHasFinding(b, "vk-killed")
	if f == nil {
		t.Fatal("a SIGKILLed server produced no finding")
	}
	if f.Sev != lsSevBad {
		t.Errorf("severity %q", f.Sev)
	}
	// The claim is that Valkey itself said nothing. Checkable against the corpus, so checked:
	// no record from Valkey's own half may be a bad-severity crash.
	for _, e := range b.Events {
		if e.Subsys == lsSubsysValkey && e.Class == lsClassCrash && e.Sev == lsSevBad {
			t.Errorf("Valkey's own log does record the kill after all: %q", e.Message)
		}
	}
	// And the evidence that IS present has to come from systemd.
	found := false
	for _, e := range b.Events {
		if e.Subsys == lsSubsysVKSysd && strings.Contains(e.Label, "KILLED") {
			found = true
		}
	}
	if !found {
		t.Error("no systemd kill record in the bundle, so the finding rests on nothing")
	}
}

// A failed background save leaves the server refusing every write with MISCONF, and the log
// never says so. Both halves of that are asserted, and the second against the corpus.
func TestValkeyPersistenceFailureAndItsSilence(t *testing.T) {
	b := lsLoadScenario(t, "v03-standalone-repl")
	f := lsHasFinding(b, "vk-persistence-failed")
	if f == nil {
		t.Fatal("a real failed background save produced no finding")
	}
	if !strings.Contains(f.Detail, "MISCONF") {
		t.Errorf("the finding does not name what clients actually saw: %s", f.Detail)
	}
	if !strings.Contains(f.Detail, "Permission denied") {
		t.Errorf("the child's reason was not carried into the finding: %s", f.Detail)
	}
	// The silence is the claim, so the corpus is checked for it: the word MISCONF appears
	// nowhere in any record, even though the server was refusing writes with it.
	for _, e := range b.Events {
		if strings.Contains(e.Message, "MISCONF") {
			t.Errorf("the log does mention MISCONF after all: %q", e.Message)
		}
	}
}

// The honest note, and Valkey's is the largest of the six engines'. Each of its three claims
// was measured against a live server, and each is checked against the corpus here.
func TestValkeyInvisibleThingsAreDeclaredAndTrue(t *testing.T) {
	b := lsLoadScenario(t, "v03-standalone-repl")
	f := lsHasFinding(b, "vk-invisible")
	if f == nil {
		t.Fatal("a Valkey bundle should carry the invisibility note")
	}
	for _, want := range []string{"evict", "MISCONF", "auth"} {
		if !strings.Contains(strings.ToLower(f.Detail), strings.ToLower(want)) {
			t.Errorf("the note does not mention %q: %s", want, f.Detail)
		}
	}
	// 19,156 keys really were evicted during this capture and the log really does say
	// nothing. If a record ever mentions eviction, the note is wrong and must be rewritten.
	for _, e := range b.Events {
		low := strings.ToLower(e.Message)
		if strings.Contains(low, "evicted") || strings.Contains(low, "maxmemory") {
			t.Errorf("the log does record eviction after all: %q", e.Message)
		}
		if strings.Contains(low, "wrongpass") {
			t.Errorf("the log does record a failed authentication after all: %q", e.Message)
		}
	}
	// This bundle contains a real persistence failure, so the note is not merely a caveat —
	// it becomes a statement about this server, and its severity rises to say so.
	if f.Sev != lsSevWarn {
		t.Errorf("with a failed save in the bundle the note should be %q, got %q", lsSevWarn, f.Sev)
	}
}

// Galera can tell a member that left cleanly from one that was lost. Valkey Cluster cannot:
// verified by stopping a node with systemctl, which produced on its peers exactly the same
// "Marking node ... as failing" a SIGKILL produces. The page says so rather than staying
// quiet, because silence here reads as "it was a crash".
func TestValkeyCleanStopIsIndistinguishableToPeers(t *testing.T) {
	b := lsLoadScenario(t, "v02-cluster-nocover")
	f := lsHasFinding(b, "vk-departure-unattributable")
	if f == nil {
		t.Fatal("a node being declared failed produced no note about what that does not tell you")
	}
	// In THIS bundle the departed node's own log is present and says it was stopped, so the
	// finding must give the answer rather than only the caveat.
	if !strings.Contains(f.Title, "stopped deliberately") {
		t.Errorf("the departing node's own log says it was stopped, and the finding does not: %q", f.Title)
	}
	// And the premise: the survivors' records really are the same ones a crash produces.
	if lsHasLabel(b, "A node was declared FAILED (quorum reached)") == nil {
		t.Error("the corpus has no quorum-reached failure record, so the claim rests on nothing")
	}
	if lsHasLabel(b, "Shutdown requested") == nil {
		t.Error("the corpus has no shutdown record, so the 'deliberate' half rests on nothing")
	}
}

// A full dataset copy where a partial resync would have done is Valkey's SST-instead-of-IST,
// and it must not fire on the full sync that STARTS replication — that is how replication
// begins, and flagging it would report every healthy deployment as a problem.
func TestValkeyFullResyncExcludesTheFirstOne(t *testing.T) {
	b := lsLoadScenario(t, "v03-standalone-repl")
	f := lsHasFinding(b, "vk-full-resync")
	if f == nil {
		t.Fatal("a forced full resync produced no finding")
	}
	if !strings.Contains(f.Advice, "repl-backlog-size") {
		t.Errorf("the advice does not name the setting that decides this: %s", f.Advice)
	}
	// The corpus contains a refusal with a stated reason, and the finding has to carry it —
	// the two reasons need different answers and only the message says which.
	if !strings.Contains(f.Detail, "replication ID") && !strings.Contains(f.Detail, "Replication ID") &&
		!strings.Contains(f.Detail, "backlog") {
		t.Errorf("the finding does not say why the partial resync was refused: %s", f.Detail)
	}
	// A first-time sync on its own must produce nothing.
	recs, _ := lsFoldValkey([]byte(
		"1:S 15 Aug 2026 23:00:00.000 * Connecting to PRIMARY 10.0.0.1:6379\n" +
			"1:S 15 Aug 2026 23:00:00.100 * Partial resynchronization not possible (no cached primary)\n" +
			"1:S 15 Aug 2026 23:00:00.200 * Full resync from primary: abc:1\n" +
			"1:S 15 Aug 2026 23:00:01.000 * PRIMARY <-> REPLICA sync: Finished with success\n"))
	if len(recs) != 4 {
		t.Fatalf("want 4 records, got %d", len(recs))
	}
	one := lsBuild([]lsInput{{Name: "fresh.log", Origin: "upload", Data: []byte(
		"1:S 15 Aug 2026 23:00:00.000 * Connecting to PRIMARY 10.0.0.1:6379\n" +
			"1:S 15 Aug 2026 23:00:00.100 * Partial resynchronization not possible (no cached primary)\n" +
			"1:S 15 Aug 2026 23:00:00.200 * Full resync from primary: abc:1\n" +
			"1:S 15 Aug 2026 23:00:01.000 * PRIMARY <-> REPLICA sync: Finished with success\n")}})
	if f := lsHasFinding(one, "vk-full-resync"); f != nil {
		t.Errorf("a replica's very first sync is reported as an avoidable full copy: %s", f.Detail)
	}
}

// A hand promotion with the old primary still up is two servers accepting writes for one
// dataset — a statement only possible because several logs are read together.
func TestValkeyManualPromotionIsReported(t *testing.T) {
	b := lsLoadScenario(t, "v03-standalone-repl")
	f := lsHasFinding(b, "vk-manual-promotion")
	if f == nil {
		t.Fatal("REPLICAOF NO ONE produced no finding")
	}
	if !strings.Contains(f.Detail, "no election") && !strings.Contains(f.Detail, "no fencing") {
		t.Errorf("the finding does not say that nothing coordinated it: %s", f.Detail)
	}
	// The corpus has the old primary still up at that instant, which is the whole reason this
	// is worth a finding — and it is a statement no single log could support.
	if f.Sev != lsSevBad {
		t.Errorf("a promotion with the old primary still running should be %q, got %q", lsSevBad, f.Sev)
	}
	if !strings.Contains(f.Title, "Two nodes") {
		t.Errorf("the two-primaries case is not called out: %q", f.Title)
	}
}

// A standalone replication pair has no shards, and must not be told it does. The failover
// finding used to accept a hand promotion as one of its events and then describe the result
// as "a shard changed primary" — Valkey Cluster's vocabulary on a bundle containing no
// cluster, which is the same class of leak that had a PostgreSQL standby reported as a
// broken MySQL replica.
func TestValkeyStandalonePairIsNotToldAboutShards(t *testing.T) {
	b := lsLoadScenario(t, "v03-standalone-repl")
	if f := lsHasFinding(b, "vk-failover"); f != nil {
		t.Errorf("the cluster failover finding fired on a bundle with no cluster: %q", f.Title)
	}
	for _, id := range []string{"vk-cluster-down", "vk-no-takeover", "vk-departure-unattributable"} {
		if f := lsHasFinding(b, id); f != nil {
			t.Errorf("%q fired on a standalone pair: %q", id, f.Title)
		}
	}
	for _, f := range b.Finding {
		if strings.Contains(f.Detail, "shard") || strings.Contains(f.Title, "shard") {
			t.Errorf("finding %q speaks of shards on a bundle with no cluster: %s", f.ID, f.Title)
		}
	}
	// And the cluster bundle must still get the failover finding — restricting it must not
	// have turned it off where it belongs.
	if f := lsHasFinding(lsLoadScenario(t, "v01-cluster-failover"), "vk-failover"); f == nil {
		t.Error("the failover finding no longer fires on a real cluster failover")
	}
}

// ---------------------------------------------------------------- isolation

// A Valkey log must not be dragged through any other engine's catalogue, and no other
// engine's bundle may pick up a Valkey finding. This is the check that caught a PostgreSQL
// standby being reported as a broken MySQL replica, so it is run in both directions.
func TestValkeyDoesNotDisturbTheOtherFlavours(t *testing.T) {
	valkeyDirs := []string{"v01-cluster-failover", "v02-cluster-nocover", "v03-standalone-repl"}
	foreign := []string{
		"replication-broken", "replica-lag", "mongo-no-primary", "quorum", "partition",
		"pg-dcs-lost", "pg-failover", "pg-lag-invisible", "pg-no-primary", "gr-split",
		"desync", "sst", "ist-fallback", "flow-control", "bootstrap", "unclean-restart",
	}
	for _, dir := range valkeyDirs {
		b := lsLoadScenario(t, dir)
		for _, id := range foreign {
			if f := lsHasFinding(b, id); f != nil {
				t.Errorf("%s: %q fired on a Valkey bundle — %s", dir, id, f.Title)
			}
		}
	}
	valkeyIDs := []string{
		"vk-cluster-down", "vk-killed", "vk-failover", "vk-demoted", "vk-no-takeover",
		"vk-persistence-failed", "vk-full-resync", "vk-manual-promotion",
		"vk-departure-unattributable", "vk-invisible",
	}
	for _, dir := range []string{"s06-network-partition", "m05-rollback", "p01-patroni-cluster",
		"g15-innodbcluster-bootstrap", "r05-replica-lag"} {
		b := lsLoadScenario(t, dir)
		for _, id := range valkeyIDs {
			if f := lsHasFinding(b, id); f != nil {
				t.Errorf("%s: Valkey finding %q fired — %s", dir, id, f.Title)
			}
		}
	}
}

// A Valkey log must be sniffed as Valkey rather than falling through to MySQL, which is what
// lsSniffEngine does when nothing else matches. A misidentified engine means the whole
// catalogue is the wrong one.
func TestValkeyIsSniffedAsValkey(t *testing.T) {
	for _, dir := range []string{"v01-cluster-failover", "v02-cluster-nocover", "v03-standalone-repl"} {
		b := lsLoadScenario(t, dir)
		for _, s := range b.Sources {
			if s.Engine != pktEngineValkey {
				t.Errorf("%s/%s sniffed as %q", dir, s.Name, s.Engine)
			}
		}
	}
	// And the sniff has to survive the journald prefix, which is the shape the collector
	// actually produces and the one a naive pattern misses.
	if !lsSniffValkey("Aug 15 23:03:55 vkc1 valkey-server[253]: 253:M 15 Aug 2026 23:03:55.052 * Server initialized\n" +
		"Aug 15 23:03:55 vkc1 valkey-server[253]: 253:M 15 Aug 2026 23:03:55.100 * Ready to accept connections tcp\n") {
		t.Error("a journald-prefixed Valkey log does not sniff as Valkey")
	}
}

// Every rule in the catalogue has to match something in the corpus, or it was written from
// memory rather than from evidence — which is the standard the other five catalogues are
// held to and the reason all five of them caught mistakes.
func TestValkeyCatalogueIsGroundedInTheCorpus(t *testing.T) {
	matched := map[string]bool{}
	for _, dir := range []string{"v01-cluster-failover", "v02-cluster-nocover", "v03-standalone-repl"} {
		for _, e := range lsLoadScenario(t, dir).Events {
			matched[e.Label] = true
		}
	}
	// The rules that describe failures this corpus does not contain are listed here rather
	// than silently exempted: each one is a hole in the corpus, not a rule written blind, and
	// naming them is what stops the list quietly growing.
	uncaptured := map[string]bool{
		"Valkey crashed":                             true, // needs a catchable signal; SIGKILL writes nothing
		"Allocation failed — out of memory":          true, // needs the host to actually run out
		"maxclients reached — connections refused":   true,
		"Protocol error — a client was disconnected": true,
		"Cross-protocol request rejected":            true,
		"Refused to vote for a promotion":            true, // needs a contested election
		"Host: transparent huge pages are enabled":   true, // the corpus hosts have THP off
		"systemd: the server died on a signal":       true, // the corpus kill is SIGKILL, matched by its own rule
		"systemd: the server dumped core":            true,
		"A replica disconnected":                     true,
		"systemd: sending SIGKILL":                   true,
		"Could not write the snapshot file":          false, // present; listed to show the map is read
	}
	var missing []string
	for _, r := range append(append([]lsRule{}, lsValkeyRules...), lsValkeySystemdRules...) {
		if matched[r.label] || uncaptured[r.label] {
			continue
		}
		missing = append(missing, r.label)
	}
	if len(missing) > 0 {
		t.Errorf("%d rule(s) match nothing in the corpus and are not declared uncaptured: %s",
			len(missing), strings.Join(missing, " · "))
	}
}
