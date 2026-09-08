package main

// logsummary_mongo_test.go — the Log Summary against real MongoDB replica-set logs.
//
// The `m*` fixtures came off a live three-node Percona Server for MongoDB 8.0.28-12 replica
// set while the scenario in the directory name was performed on it.
//
//	m01-bootstrap        rs.initiate and two members joining
//	m02-stepdown         rs.stepDown() on the primary — a planned handover
//	m03-primary-kill9    SIGKILL on the primary under write load
//	m04-partition        a member cut off port 27017 with tc/netem for 60 s
//	m05-rollback         a PARTITIONED PRIMARY written to with w:1 and then healed —
//	                     a genuine rollback, with 40 acknowledged documents actually lost
//	m06-initial-sync     a wiped data directory resynced from another member
//
// One trim was applied and is worth stating plainly, because everything else in this
// package's corpora is verbatim: MongoDB writes a heartbeat-failure record every two
// seconds for as long as a member is unreachable, which was 1,234 lines in the forty
// seconds of m03 alone. The fixtures keep the components that carry replica-set meaning and
// cap a run of the same message id at six. Nothing was rewritten and nothing was invented —
// the lines are a subset of the real ones.

import (
	"strings"
	"testing"
)

func TestMongoRecognisesAReplicaSet(t *testing.T) {
	b := lsLoadScenario(t, "m01-bootstrap")
	if len(b.Sources) != 3 {
		t.Fatalf("want 3 sources, got %d", len(b.Sources))
	}
	for _, s := range b.Sources {
		if s.Flavour != lsFlavourMongoRS {
			t.Errorf("%s: flavour %q, want %q", s.Name, s.Flavour, lsFlavourMongoRS)
		}
		// The member names its own host in "Found self in config", which is a luxury the
		// Galera and Group Replication catalogues do not have.
		if !strings.HasPrefix(s.Node, "mongo0") {
			t.Errorf("%s: node %q — the member's own name was not found", s.Name, s.Node)
		}
	}
}

// A MongoDB log must not be dragged through the MySQL catalogues, and vice versa.
func TestMongoDoesNotDisturbTheOtherFlavours(t *testing.T) {
	for _, name := range []string{"s06-network-partition", "g05-primary-failover", "r02-dupkey-conflict"} {
		b := lsLoadScenario(t, name)
		for _, s := range b.Sources {
			if s.Flavour == lsFlavourMongoRS {
				t.Errorf("%s/%s: a MySQL-family log classified as a MongoDB replica set", name, s.Name)
			}
		}
		for _, id := range []string{"mongo-rollback", "mongo-no-primary", "mongo-election", "mongo-lag-invisible"} {
			if f := lsHasFinding(b, id); f != nil {
				t.Errorf("%s: MongoDB finding %q fired — %s", name, id, f.Title)
			}
		}
	}
}

// ---------------------------------------------------------------- the state machine

// Id 21358 carries newState AND oldState, which is what makes a log fragment readable: a
// member already PRIMARY when the excerpt begins never logs a transition into it.
func TestMongoStateTransitionsCarryBothSides(t *testing.T) {
	b := lsLoadScenario(t, "m02-stepdown")
	var down, up *lsEvent
	for i := range b.Events {
		e := &b.Events[i]
		if e.Code != "21358" {
			continue
		}
		if e.From == lsStatePrimaryM && e.State == lsStateSecondary {
			down = e
		}
		if e.State == lsStatePrimaryM {
			up = e
		}
	}
	if down == nil {
		t.Fatal("no PRIMARY→SECONDARY transition in the stepdown fixture")
	}
	if down.Label != "Stepped down from PRIMARY" {
		t.Errorf("a primary losing the role should say so, got %q", down.Label)
	}
	if down.Sev != lsSevWarn {
		t.Errorf("a stepdown is a write outage; severity %q", down.Sev)
	}
	if up == nil {
		t.Fatal("nobody became primary in the stepdown fixture")
	}
	if up.Sev != lsSevOK {
		t.Errorf("becoming primary should read as good news, got %q", up.Sev)
	}
}

// A peer's state must not be recorded as this source's, or one member's log makes every
// other member's history look like its own.
func TestMongoPeerReportsAreAboutThePeer(t *testing.T) {
	b := lsLoadScenario(t, "m02-stepdown")
	found := false
	for _, e := range b.Events {
		if e.Code != "21215" {
			continue
		}
		found = true
		if e.Peer == "" {
			t.Errorf("peer report with no peer: %s", e.Message)
		}
		if e.State != "" {
			t.Errorf("a report ABOUT %s set the state of the file it was found in", e.Peer)
		}
	}
	if !found {
		t.Fatal("no peer state reports in the fixture")
	}
}

func TestMongoStatesAreRatedAndExplained(t *testing.T) {
	for state, want := range map[string]string{
		lsStatePrimaryM:  lsSevOK,
		lsStateSecondary: lsSevOK,
		lsStateStartup2:  lsSevWarn,
		lsStateRollback:  lsSevBad,
		lsStateRemoved:   lsSevBad,
	} {
		if got := lsStateSev(state); got != want {
			t.Errorf("state %s: severity %q, want %q", state, got, want)
		}
		if lsStateMeaning[state] == "" {
			t.Errorf("state %s has no entry in lsStateMeaning", state)
		}
	}
	// Both a primary and a secondary serve queries — unlike every other topology in this
	// package, where exactly one state does.
	if !lsStateServes(lsStateSecondary) {
		t.Error("a SECONDARY serves reads and must count as serving")
	}
	// Galera's primary COMPONENT and MongoDB's PRIMARY are different ideas and must not
	// collide, or the swimlane legend explains one word two ways.
	if lsStatePrim == lsStatePrimaryM {
		t.Error("Galera's primary-component state and MongoDB's PRIMARY share a string")
	}
}

// ---------------------------------------------------------------- the incidents

// The one that matters: acknowledged writes discarded, with a receipt.
func TestMongoRollback(t *testing.T) {
	b := lsLoadScenario(t, "m05-rollback")
	if lsHasLabel(b, "Rollback started") == nil {
		t.Fatal("the ROLLBACK transition was not recognised")
	}
	f := lsHasFinding(b, "mongo-rollback")
	if f == nil {
		t.Fatal("no rollback finding")
	}
	if f.Sev != lsSevBad {
		t.Errorf("severity %q, want bad — this is data loss", f.Sev)
	}
	// The count of reverted operations is the size of the loss and has to survive.
	if !strings.Contains(f.Detail, "reverted") {
		t.Errorf("the finding does not say how much was lost: %s", f.Detail)
	}
	// The rollback file is the only copy of the discarded documents. If the finding does
	// not carry the path, the one actionable thing in the incident is missing.
	if !strings.Contains(f.Advice, "rollback") || !strings.Contains(f.Advice, "bsondump") {
		t.Errorf("the advice does not point at the rollback files: %s", f.Advice)
	}
	if !strings.Contains(f.Advice, "w:majority") {
		t.Errorf("the advice does not name the setting that prevents this: %s", f.Advice)
	}
	// And a healthy fixture must not produce one.
	if f := lsHasFinding(lsLoadScenario(t, "m01-bootstrap"), "mongo-rollback"); f != nil {
		t.Errorf("false rollback on a bootstrap: %s", f.Detail)
	}
}

func TestMongoNoPrimaryIsMeasured(t *testing.T) {
	b := lsLoadScenario(t, "m03-primary-kill9")
	f := lsHasFinding(b, "mongo-no-primary")
	if f == nil {
		t.Fatal("killing the primary produced no 'no primary' finding")
	}
	// The measured gap has to be the real one, and the real one is knowable from outside
	// the log: the default electionTimeoutMillis is 10 seconds, so a killed primary costs
	// about that before anybody can stand. This fixture measures 9.6s.
	//
	// It measured 39.6s until a live run exposed why. A member's own transition is keyed by
	// the source name and a peer's by the host out of the record; those two spellings were
	// not being normalised to one, so the election that ended the gap was filed against a
	// different member than the one that had lost the role, and the gap ran to the end of
	// the window. An assertion on the magnitude is what makes that class of bug fail here.
	if !strings.Contains(f.Title, "9.6s") {
		t.Errorf("want a gap of about one election timeout, got %q", f.Title)
	}
	if f.Sev != lsSevWarn {
		t.Errorf("a ten-second failover is a warning, not a crisis; got %q", f.Sev)
	}
	// A longer outage must still escalate.
	rb := lsHasFinding(lsLoadScenario(t, "m05-rollback"), "mongo-no-primary")
	if rb == nil || rb.Sev != lsSevBad {
		t.Errorf("a 30-second write outage should be bad, got %+v", rb)
	}
	// A planned stepdown is quick, and must not be reported as the same kind of event.
	sd := lsLoadScenario(t, "m02-stepdown")
	if f := lsHasFinding(sd, "mongo-no-primary"); f != nil && f.Sev == lsSevBad {
		t.Errorf("a planned stepdown reported as a serious outage: %s", f.Title)
	}
}

func TestMongoElectionTellsPlannedFromUnplanned(t *testing.T) {
	planned := lsHasFinding(lsLoadScenario(t, "m02-stepdown"), "mongo-election")
	if planned == nil {
		t.Fatal("no election finding for the stepdown")
	}
	if !strings.Contains(planned.Detail, "requested step-up") {
		t.Errorf("a deliberate rs.stepDown() should be named as such: %s", planned.Detail)
	}
	unplanned := lsHasFinding(lsLoadScenario(t, "m03-primary-kill9"), "mongo-election")
	if unplanned == nil {
		t.Fatal("no election finding for the kill")
	}
	if strings.Contains(unplanned.Detail, "requested step-up") {
		t.Errorf("a failover after a SIGKILL reported as planned: %s", unplanned.Detail)
	}
}

// The heartbeat failures are the outage, as the survivors experienced it.
func TestMongoMemberDownUsesTheHeartbeatSpan(t *testing.T) {
	b := lsLoadScenario(t, "m03-primary-kill9")
	f := lsHasFinding(b, "mongo-member-down")
	if f == nil {
		t.Fatal("no member-down finding")
	}
	if !strings.Contains(f.Detail, "mongo02") {
		t.Errorf("the unreachable member is not named: %s", f.Detail)
	}
	// The repeats must be collapsed, or one dead member buries the whole file.
	hb := 0
	for _, e := range b.Events {
		if e.Code == "23974" {
			hb++
		}
	}
	if hb > 12 {
		t.Errorf("%d separate heartbeat-failure rows — they are not being collapsed", hb)
	}
}

func TestMongoInitialSync(t *testing.T) {
	b := lsLoadScenario(t, "m06-initial-sync")
	if lsHasLabel(b, "Initial sync started") == nil && lsHasLabel(b, "Initial sync required") == nil {
		t.Fatal("the initial sync was not recognised")
	}
	f := lsHasFinding(b, "mongo-initial-sync")
	if f == nil {
		t.Fatal("no initial-sync finding")
	}
	if !strings.Contains(f.Detail, "mongo01") {
		t.Errorf("the resyncing member is not named: %s", f.Detail)
	}
}

// ---------------------------------------------------------------- honesty

// Lag is not in the log, and the note that says so should point at the artefact that DOES
// have it — which for MongoDB is already on the machine.
func TestMongoLagNotePointsAtFTDC(t *testing.T) {
	f := lsHasFinding(lsLoadScenario(t, "m01-bootstrap"), "mongo-lag-invisible")
	if f == nil {
		t.Fatal("the replication-lag note is missing")
	}
	if f.Sev != lsSevInfo {
		t.Errorf("severity %q — it is a statement about the log, not a fault", f.Sev)
	}
	if !strings.Contains(f.Advice, "diagnostic.data") {
		t.Errorf("the note does not point at diagnostic.data: %s", f.Advice)
	}
}

// Three rules in this catalogue were written from memory rather than from the corpus and
// all three were wrong. This asserts the corrections, so they cannot quietly come back.
func TestMongoCorrectedMappings(t *testing.T) {
	for _, tc := range []struct {
		id                  int
		wantLabel, notLabel string
	}{
		{22322, "Checkpoint thread shutting down", "Fatal assertion"},
		{21444, "Dry-run election succeeded", "Election failed"},
		{20698, "Server restarted", "Shutdown started"},
	} {
		r := lsMongoByID[tc.id]
		if r == nil {
			t.Errorf("id %d is no longer in the catalogue", tc.id)
			continue
		}
		if r.label != tc.wantLabel {
			t.Errorf("id %d: label %q, want %q", tc.id, r.label, tc.wantLabel)
		}
		if r.label == tc.notLabel {
			t.Errorf("id %d has regressed to the wrong label %q", tc.id, tc.notLabel)
		}
	}
}

// ---------------------------------------------------------------- across versions
//
// m07 and m08 are the same incident as m05, driven against PSMDB 6.0.29-23 and 7.0.39-21:
// the primary cut off the network, three hundred writes accepted on the wrong side of the
// partition with w:1, a new primary elected by the other two, the link restored, and a
// member SIGKILLed for good measure. They exist because a catalogue keyed on numeric ids is
// only as good as the claim that those ids are stable, and that claim is worth checking
// rather than believing.

// lsMongoVersions is the same scenario on every supported release.
var lsMongoVersions = []struct{ name, dir string }{
	{"6.0", "m07-rollback-mongo60"},
	{"7.0", "m08-rollback-mongo70"},
	{"8.0", "m05-rollback"},
}

// The whole design rests on MongoDB's numeric ids being stable across releases — the docs
// say the message text may change and the id may not. This is that promise, tested: the
// same physical incident on three releases has to produce the same findings.
func TestMongoSameIncidentOnEveryVersion(t *testing.T) {
	want := []string{"mongo-rollback", "mongo-no-primary", "mongo-member-down", "mongo-election"}
	for _, v := range lsMongoVersions {
		t.Run(v.name, func(t *testing.T) {
			b := lsLoadScenario(t, v.dir)
			if len(b.Sources) != 3 {
				t.Fatalf("want 3 sources, got %d", len(b.Sources))
			}
			for _, s := range b.Sources {
				if s.Flavour != lsFlavourMongoRS {
					t.Errorf("%s: flavour %q on %s", s.Name, s.Flavour, v.name)
				}
				if !strings.HasPrefix(s.Node, "mongo0") {
					t.Errorf("%s: the member did not name itself on %s (got %q)", s.Name, v.name, s.Node)
				}
			}
			for _, id := range want {
				if lsHasFinding(b, id) == nil {
					t.Errorf("finding %q did not fire on %s", id, v.name)
				}
			}
		})
	}
}

// What a rollback threw away has to survive the version change, and this is where it did
// not. 6984700 "Operations reverted by rollback" arrived in 7.0, so a 6.0 rollback read
// through it reports that data was lost without saying how much — the one number the reader
// needs. 21612's rollback summary carries the same counts on every version AND names the
// collections and the directory the discarded documents went to, so it is preferred outright.
func TestMongoRollbackCountSurvivesEveryVersion(t *testing.T) {
	for _, v := range lsMongoVersions {
		t.Run(v.name, func(t *testing.T) {
			b := lsLoadScenario(t, v.dir)
			f := lsHasFinding(b, "mongo-rollback")
			if f == nil {
				t.Fatal("no rollback finding")
			}
			for _, want := range []string{"insert reverted", "discarded documents under"} {
				if !strings.Contains(f.Detail, want) {
					t.Errorf("%s: rollback detail does not say %q:\n  %s", v.name, want, f.Detail)
				}
			}
			// The namespace is the difference between "something was lost" and "this
			// collection lost writes", and it is only in 21612.
			if !strings.Contains(f.Detail, " in ") {
				t.Errorf("%s: rollback detail names no collection:\n  %s", v.name, f.Detail)
			}
		})
	}
}

// 20557 sat in the catalogue as "Unclean shutdown detected" and was a guess. SIGKILLing a
// mongod on 6.0, 7.0 and 8.0 never produced it; 22271, 501401 and 20631 all did, on every
// version. This asserts the replacement, and that the finding it feeds speaks MongoDB
// rather than Galera — the first version of this reported a killed mongod and then advised
// the reader to go and look for a wsrep position recovery.
func TestMongoUncleanShutdownIsRecognised(t *testing.T) {
	for _, v := range []struct{ name, dir string }{
		{"6.0", "m07-rollback-mongo60"}, {"7.0", "m08-rollback-mongo70"},
	} {
		t.Run(v.name, func(t *testing.T) {
			b := lsLoadScenario(t, v.dir)
			f := lsHasFinding(b, "crash")
			if f == nil {
				t.Fatal("a SIGKILLed mongod produced no crash finding")
			}
			if !strings.Contains(f.Detail, "not clean") {
				t.Errorf("crash detail is unexpected: %q", f.Detail)
			}
			if strings.Contains(f.Advice, "wsrep") {
				t.Errorf("a MongoDB-only bundle was given Galera advice: %q", f.Advice)
			}
			if !strings.Contains(f.Advice, "journal") {
				t.Errorf("crash advice does not describe mongod recovery: %q", f.Advice)
			}
		})
	}
}

// The ids that the version sweep proved absent, kept together so that the next person to
// add one has to say which capture matched it. 20557 was removed from the catalogue for
// exactly this reason.
func TestMongoUnverifiedRulesStayFencedOff(t *testing.T) {
	fenced := map[int]bool{4280510: true, 5579600: true, 23015: true}
	for id := range fenced {
		if lsMongoByID[id] == nil {
			t.Errorf("id %d is documented as fenced-off but is not in the catalogue", id)
		}
	}
	// And the ones that ARE verified must be there, because each replaced a guess.
	for _, id := range []int{22271, 501401, 20631, 22302, 21122, 21612} {
		if lsMongoByID[id] == nil {
			t.Errorf("id %d was matched in a real capture on all three versions but is not in the catalogue", id)
		}
	}
	if lsMongoByID[20557] != nil {
		t.Error("20557 is back — it never fired on 6.0, 7.0 or 8.0 and 22271 is the record that does")
	}
}

// ---------------------------------------------------------------- sharded clusters
//
// m09 and m10 are a whole 13-node sharded cluster on 6.0.29-23 and 7.0.39-21 — a mongos, a
// config-server member and a shard member — driven through sharding a collection, a chunk
// migration, an entire shard stopped under live traffic, and the config-server primary
// stopped on top of that.

var lsShardedVersions = []struct{ name, dir string }{
	{"6.0", "m09-sharded-mongo60"},
	{"7.0", "m10-sharded-mongo70"},
	{"8.0", "m11-sharded-mongo80"},
}

// A mongos is not a replica-set member and must not be read as one. It monitors every shard
// and logs a great deal about replica sets without being in one, so without a flavour of its
// own a perfectly healthy router is reported as a member that never became primary.
func TestMongosIsItsOwnFlavour(t *testing.T) {
	for _, v := range lsShardedVersions {
		t.Run(v.name, func(t *testing.T) {
			b := lsLoadScenario(t, v.dir)
			var router *lsSource
			for i := range b.Sources {
				if b.Sources[i].Name == "mongos.log" {
					router = &b.Sources[i]
				}
			}
			if router == nil {
				t.Fatal("no mongos source")
			}
			if router.Flavour != lsFlavourMongos {
				t.Errorf("the router is flavoured %q, want %q", router.Flavour, lsFlavourMongos)
			}
			// Its lane must read as serving. A router has no replica-set state, and leaving
			// the lane empty reports every healthy router as unavailable for the window.
			if !lsStateServes(lsStateRouting) {
				t.Error("a running mongos should count as serving")
			}
			for _, e := range b.Events {
				if e.Src == router.Idx && e.State != "" && e.State != lsStateRouting && e.State != lsStateStarting {
					t.Errorf("router event %d carries replica-set state %q", e.No, e.State)
				}
			}
		})
	}
}

// The sniff rests on mongod and mongos using DIFFERENT IDS for the same message: 471691 on
// a mongod, 471693 on a mongos. Both were confirmed present on 6.0 and 7.0 and neither
// appears in the other's log.
func TestMongosSniffUsesTheRouterOnlyId(t *testing.T) {
	const router = `{"t":{"$date":"2026-08-15T07:39:00.000+00:00"},"s":"I","c":"SHARDING","id":471693,"ctx":"m","msg":"Updating the shard registry with confirmed replica set","attr":{"connectionString":"rs0/s0r1.example.net:27017"}}`
	const member = `{"t":{"$date":"2026-08-15T07:39:00.000+00:00"},"s":"I","c":"SHARDING","id":471691,"ctx":"m","msg":"Updating the shard registry with confirmed replica set","attr":{"connectionString":"rs0/s0r1.example.net:27017"}}`
	if b := lsBuild([]lsInput{{Name: "mongos.log", Data: []byte(router + "\n")}}); b.Sources[0].Flavour != lsFlavourMongos {
		t.Errorf("471693 should identify a router, got flavour %q", b.Sources[0].Flavour)
	}
	// 471691 alone is a mongod, and with no replica-set evidence at all it is not a router
	// either — it must not be promoted to one just because it mentions sharding.
	if b := lsBuild([]lsInput{{Name: "mongod.log", Data: []byte(member + "\n")}}); b.Sources[0].Flavour == lsFlavourMongos {
		t.Error("471691 is a mongod's id and must not identify a router")
	}
}

// A replica set observed without a primary exactly once, while it was still being formed,
// is not an outage. Measuring first-to-last NoPrimary record reported one as "for 0s" and
// inflated a twelve-second config-server outage to 14.4 minutes, because the last such
// record was fourteen minutes after the first with a healthy period in between. The span is
// measured to the record that ENDS it instead.
func TestShardNoPrimaryIsMeasuredToItsRecovery(t *testing.T) {
	for _, v := range lsShardedVersions {
		t.Run(v.name, func(t *testing.T) {
			b := lsLoadScenario(t, v.dir)
			f := lsHasFinding(b, "mongo-shard-no-primary")
			if f == nil {
				t.Fatal("a shard was stopped outright and no finding fired")
			}
			if strings.Contains(f.Detail, "for 0s") {
				t.Errorf("a set observed once while forming is reported as an outage: %s", f.Detail)
			}
			if !strings.Contains(f.Detail, "rs2") {
				t.Errorf("the shard that was actually stopped is not named: %s", f.Detail)
			}
		})
	}
}

// One rule covers the whole sharding vocabulary because the config servers write every
// topology change under one id with the operation in attr.event.what. This asserts the
// enrichment reads it, rather than the rule merely matching.
func TestShardChangelogNamesTheOperations(t *testing.T) {
	for _, v := range lsShardedVersions {
		t.Run(v.name, func(t *testing.T) {
			b := lsLoadScenario(t, v.dir)
			f := lsHasFinding(b, "mongo-shard-changes")
			if f == nil {
				t.Fatal("no changelog finding from a cluster that was built and then sharded")
			}
			if !strings.Contains(f.Detail, "addShard") {
				t.Errorf("the changelog does not name addShard, so attr.event.what is not being read: %s", f.Detail)
			}
			// And the labels have to be readable rather than raw.
			if lsShardEventLabel("moveChunk.commit") != "Chunk moved between shards (completed)" {
				t.Errorf("moveChunk.commit reads as %q", lsShardEventLabel("moveChunk.commit"))
			}
			if lsShardEventLabel("shardCollection.error") != "Collection sharded — FAILED" {
				t.Errorf("a failed phase is not marked: %q", lsShardEventLabel("shardCollection.error"))
			}
		})
	}
}

// The same sharded incident on both releases has to produce the same findings — the point
// of keying on ids rather than on messages, and the thing most likely to break quietly when
// MongoDB changes something. 7.0 replaced 6.0's distributed lock manager with DDL
// coordinators and stopped auto-splitting chunks; neither changed an id.
func TestShardedFindingsMatchOnBothVersions(t *testing.T) {
	want := []string{"mongo-shard-no-primary", "mongo-shard-changes", "mongo-routing-invisible"}
	for _, v := range lsShardedVersions {
		t.Run(v.name, func(t *testing.T) {
			b := lsLoadScenario(t, v.dir)
			for _, id := range want {
				if lsHasFinding(b, id) == nil {
					t.Errorf("finding %q did not fire on %s", id, v.name)
				}
			}
			// And a sharded bundle still gets the replica-set findings for its mongods.
			if lsHasFinding(b, "mongo-no-primary") == nil {
				t.Errorf("the config and shard members' own replica-set findings are missing on %s", v.name)
			}
		})
	}
}

// The changelog rule reads the operation out of attr.event.what rather than matching on it,
// so an operation MongoDB adds later arrives without a code change. 8.0 added automatic
// chunk merging and it appeared in the 8.0 capture as autoMerge.start/end — through a rule
// written and tested entirely against 6.0 and 7.0, which is the property that makes keying
// on 22080 worth doing.
func TestShardChangelogAbsorbsNewOperations(t *testing.T) {
	b := lsLoadScenario(t, "m11-sharded-mongo80")
	f := lsHasFinding(b, "mongo-shard-changes")
	if f == nil {
		t.Fatal("no changelog finding on 8.0")
	}
	if !strings.Contains(f.Detail, "autoMerge") {
		t.Errorf("8.0's autoMerge did not come through the changelog rule: %s", f.Detail)
	}
	// Every operation the fixture contains has to be labelled by something, and an
	// unrecognised one must still read as a sentence rather than as a raw token.
	if got := lsShardEventLabel("somethingNewIn9x.start"); !strings.HasPrefix(got, "Cluster metadata: ") {
		t.Errorf("an unknown operation reads as %q", got)
	}
}
