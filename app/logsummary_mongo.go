package main

// logsummary_mongo.go — what a MongoDB replica-set member's log actually says.
//
// The fourth cluster vocabulary in this package, and structurally the easiest to read:
// since 4.4 mongod writes one JSON object per line, and every message carries a stable
// numeric `id` that does not change between releases even when the English does.
//
//	{"t":{"$date":"2026-08-14T17:51:00.630+00:00"},"s":"I","c":"REPL","id":21358,
//	 "ctx":"conn301","msg":"Replica set state transition",
//	 "attr":{"newState":"SECONDARY","oldState":"PRIMARY"}}
//
// That single record is the whole state machine: `newState` and `oldState` for the member
// whose file this is. Its companion, 21215, reports the same thing about a PEER
// (`{"hostAndPort":…,"newState":…}`), which is what lets one member's log describe the
// whole set — the same shape as Galera's per-member reports, but structured.
//
// The Packet Inspector already has a MongoDB log classifier (pktmongolog.go) and the Log
// Summary has been reusing it. That was right while MongoDB was one of several engines
// getting a shared treatment; it is not enough for a replica set, because the useful facts
// live in `attr` and pktLogEntry does not carry it. So replica-set members are parsed here
// instead — a second read of the same bytes, for the fields the first one drops.
//
// Every rule was written against logs from a live three-node Percona Server for MongoDB
// 8.0.28-12 replica set, captured while doing the thing that produces them: rs.stepDown(),
// SIGKILL on the primary under write load, a member cut off port 27017 with tc/netem, a
// PARTITIONED PRIMARY written to and then healed (a genuine rollback, with data actually
// lost), and a wiped data directory resynced from scratch. The fixtures are the `m*`
// directories under app/testdata/logsummary/.
//
// Four things the capture taught:
//
//  1. MongoDB repeats itself, endlessly, in a way the other three do not. A dead peer
//     produces `Heartbeat failed after max retries` every two seconds for as long as it is
//     dead — 1,234 log lines in the forty seconds after one SIGKILL. MySQL and Galera
//     write a failure once and go quiet. Record collapsing is not a nicety here; without
//     it a one-member outage buries every other record in the file.
//
//  2. A rollback is silent data loss with a receipt, and the receipt is in the log. When a
//     partitioned primary rejoins, `Operations reverted by rollback` states exactly how
//     many writes vanished, and `Preparing to write deleted documents to a rollback file`
//     gives the path they were written to. In the capture, 40 documents acknowledged with
//     w:1 were reverted and 43 operations rolled back. Nothing else in this package
//     reports an incident where committed, acknowledged data is deliberately discarded.
//
//  3. A cut-off SECONDARY does nothing and says almost nothing. Galera's minority node
//     refuses queries with 1047 and Group Replication's blocks every write; a MongoDB
//     secondary that cannot see the primary just keeps serving reads of whatever it has,
//     falling further behind, logging only heartbeat failures. There is no state change to
//     find — which is why lsFindingMongoStale is built on the heartbeat failures and the
//     absence of anything else.
//
//  4. The level is nearly useless, in the direction Galera's is. A rollback — the one
//     event here that loses data — is logged entirely at severity "I". So is an election,
//     an initial sync and a member going DOWN. Severity comes from the message id.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MongoDB replica-set member states, spelled as `rs.status()` spells them.
const (
	lsStatePrimaryM  = "PRIMARY"
	lsStateSecondary = "SECONDARY"
	lsStateStartup2  = "STARTUP2" // initial sync: it has no usable data yet
	lsStateRollback  = "ROLLBACK" // discarding writes the set never accepted
	lsStateArbiter   = "ARBITER"
	lsStateRemoved   = "REMOVED" // not in the config any more
	// RECOVERING is shared with Group Replication (see logsummary_grouprepl.go). The word
	// means the same thing in both — in the set, holding data, not serving — so it is one
	// state rather than two that would need two entries in the legend.
)

// lsFlavourMongoRS is the flavour for a member of a replica set. A standalone mongod keeps
// the generic `mongodb` engine flavour and the shared classifier: it has no member states,
// no elections and no rollbacks, and giving it a swimlane of replica-set states would be
// inventing a topology it does not have.
const lsFlavourMongoRS = "mongors"

// lsMongoRecord is one JSON log line, with `attr` kept.
type lsMongoRecord struct {
	Line  int
	TS    float64
	Time  string
	Level string // I | W | E | F | D
	Comp  string // REPL | ELECTION | ROLLBACK | REPL_HB | NETWORK | …
	ID    int
	Ctx   string
	Msg   string
	Attr  map[string]json.RawMessage
}

// str reads a string attribute, "" when absent or not a string.
func (r lsMongoRecord) str(key string) string {
	raw, ok := r.Attr[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

// num reads a numeric attribute.
func (r lsMongoRecord) num(key string) (int64, bool) {
	raw, ok := r.Attr[key]
	if !ok {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	return int64(f), true
}

// strs reads an array-of-strings attribute — affectedNamespaces on a rollback summary is
// the one that matters, because it says which collections lost writes.
func (r lsMongoRecord) strs(key string) []string {
	raw, ok := r.Attr[key]
	if !ok {
		return nil
	}
	var out []string
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// host reads a `hostAndPort`-shaped attribute and returns the short name, which is what
// every other part of the UI calls a node.
func (r lsMongoRecord) host(key string) string {
	return lsMongoShortHost(r.str(key))
}

// lsMongoShortHost turns "mongo02.example.net:27017" into "mongo02".
func lsMongoShortHost(s string) string {
	if i := strings.LastIndex(s, ":"); i > 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "."); i > 0 {
		s = s[:i]
	}
	return s
}

// lsFoldMongo parses a mongod log into records. There is nothing to fold — a JSON log has
// no continuation lines, which is one of the two reasons it is the pleasantest of the four
// formats — so this is a parse, kept under the same name as its siblings.
func lsFoldMongo(data []byte) []lsMongoRecord {
	var out []lsMongoRecord
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 2 || line[0] != '{' {
			continue
		}
		var raw struct {
			T struct {
				Date string `json:"$date"`
			} `json:"t"`
			S    string                     `json:"s"`
			C    string                     `json:"c"`
			ID   int                        `json:"id"`
			Ctx  string                     `json:"ctx"`
			Msg  string                     `json:"msg"`
			Attr map[string]json.RawMessage `json:"attr"`
		}
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		r := lsMongoRecord{
			Line: i + 1, Time: raw.T.Date, Level: raw.S, Comp: raw.C,
			ID: raw.ID, Ctx: raw.Ctx, Msg: raw.Msg, Attr: raw.Attr,
		}
		// MongoDB stamps with a numeric offset ("+00:00"), which RFC3339 handles.
		if t, err := time.Parse(time.RFC3339Nano, raw.T.Date); err == nil {
			r.TS = float64(t.UnixNano()) / 1e9
		}
		out = append(out, r)
	}
	return out
}

// lsMongoRule recognises one family of records, by message id.
//
// By id and nothing else. MongoDB guarantees the id is stable and lets the English drift,
// which is the opposite of MySQL's guarantee and much the more useful one: a rule keyed on
// 21358 keeps working across an upgrade that rewords the message.
type lsMongoRule struct {
	ids    []int
	class  string
	sev    string
	label  string
	means  string
	enrich func(r lsMongoRecord, e *lsEvent)
}

// lsMongoRules is the replica-set catalogue, in no particular order — ids are unique, so
// unlike the text-matched catalogues next door there is no first-match-wins subtlety here.
var lsMongoRules = []lsMongoRule{
	// ---- this member's own state -------------------------------------------------
	// The whole state machine in one record. `oldState` is what makes a log fragment
	// readable: a member that was already PRIMARY when the excerpt begins never logs a
	// transition into it, and this is the only record that says where it came from.
	{ids: []int{21358}, class: lsClassState, sev: lsSevInfo, label: "Replica set state transition",
		means: "This member changed state. Only PRIMARY and SECONDARY serve queries; everything else is either catching up or giving up.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			e.State, e.From = r.str("newState"), r.str("oldState")
			e.Message = e.From + " → " + e.State
			switch e.State {
			case lsStatePrimaryM:
				e.Sev, e.Label = lsSevOK, "Became PRIMARY"
				e.Meaning = "This member is now the only one in the set that accepts writes."
			case lsStateSecondary:
				e.Label = "Became SECONDARY"
				if e.From == lsStatePrimaryM {
					e.Sev = lsSevWarn
					e.Label = "Stepped down from PRIMARY"
					e.Meaning = "This member stopped accepting writes. Every write in flight against it failed, and the application saw errors until it found the new primary."
				} else {
					e.Sev = lsSevOK
					e.Meaning = "This member is serving reads and applying the primary's oplog."
				}
			case lsStateRollback:
				e.Sev, e.Label = lsSevBad, "Entered ROLLBACK"
				e.Meaning = "This member is about to discard writes it had already accepted, because the rest of the set never agreed to them. It is not serving anything while this happens."
			case lsStateRecovering:
				e.Sev, e.Label = lsSevWarn, "Entered RECOVERING"
				e.Meaning = "The member is in the set but not serving reads — it is applying a backlog, or it has fallen off the end of the primary's oplog and cannot catch up at all."
			case lsStateStartup2:
				e.Sev, e.Label = lsSevWarn, "Started initial sync (STARTUP2)"
				e.Meaning = "The member has no usable data and is copying the whole dataset from another member."
			case lsStateRemoved:
				e.Sev, e.Label = lsSevBad, "Removed from the replica set"
				e.Meaning = "This member is no longer in the replica-set configuration. It is running and it is not part of anything."
			}
		}},
	{ids: []int{21393}, class: lsClassStartup, sev: lsSevInfo, label: "Found itself in the config",
		means: "The member matched its own address against the replica-set configuration. This is how it knows which member it is, and it is how this page knows too."},

	// ---- what it can see of the others -------------------------------------------
	{ids: []int{21215}, class: lsClassMember, sev: lsSevInfo, label: "A peer changed state",
		means: "Another member's state, as this one heard it. Every member reports every other, so the same transition appears in all of their logs — which is what makes one file enough to reconstruct the set.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			e.Peer = r.host("hostAndPort")
			st := r.str("newState")
			e.Message = e.Peer + " → " + st
			e.Label = "Peer " + e.Peer + " is now " + st
			if st == lsStatePrimaryM {
				e.Sev = lsSevOK
			}
		}},
	{ids: []int{21216}, class: lsClassMember, sev: lsSevBad, label: "A peer went DOWN",
		means: "This member stopped being able to reach a peer and has marked it down. It is a statement about this member's view — a peer that reports itself perfectly healthy at the same instant means the link between them, not the peer.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			e.Peer = r.host("hostAndPort")
			e.Label = "Peer " + e.Peer + " went DOWN"
			if m := r.str("heartbeatMessage"); m != "" {
				e.Message = m
			}
		}},
	{ids: []int{23974}, class: lsClassNetwork, sev: lsSevWarn, label: "Heartbeat failed after max retries",
		means: "This member could not reach a peer within the heartbeat timeout. MongoDB retries every two seconds for as long as the peer is unreachable, so a single outage produces one of these every two seconds until it ends — the count and span on this row are the length of the outage.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if p := lsMongoShortHost(r.str("target")); p != "" {
				e.Peer = p
				e.Label = "Cannot reach " + p
			}
		}},

	// ---- elections ----------------------------------------------------------------
	{ids: []int{4615661}, class: lsClassMember, sev: lsSevWarn, label: "Election called by a step-up request",
		means: "Somebody ran rs.stepDown() on the primary, or a member was asked to take over. This is a planned handover rather than a failure — but writes still fail for the length of it."},
	{ids: []int{21438, 21437}, class: lsClassMember, sev: lsSevWarn, label: "Election started",
		means: "This member is standing for primary. In a healthy set this happens because the primary went away; the interesting question is always what happened just before it."},
	{ids: []int{21450}, class: lsClassMember, sev: lsSevOK, label: "Election succeeded — assuming primary",
		means: "This member won and is taking over. Writes do not resume on this line: the new primary first catches up on anything the old one had already replicated."},
	// 21444 is "Dry election run succeeded, running for election" — a step TOWARDS winning.
	// It was mapped to "Election failed" on a guess and the corpus disagreed.
	{ids: []int{21444}, class: lsClassMember, sev: lsSevInfo, label: "Dry-run election succeeded",
		means: "The member checked whether it COULD win before disturbing anything, and the answer was yes, so it is now standing for real. The dry run is why a brief network blip does not cost an election."},
	{ids: []int{21359}, class: lsClassMember, sev: lsSevInfo, label: "Entering primary catch-up",
		means: "The new primary is applying everything the old one had replicated before it starts accepting writes. This is the tail of the write outage, after the election is already decided."},
	{ids: []int{21363, 21364}, class: lsClassMember, sev: lsSevOK, label: "Primary caught up — writes resume",
		means: "The new primary has finished catching up. This is where the application's write outage actually ends."},
	{ids: []int{21106}, class: lsClassReplica, sev: lsSevInfo, label: "Sync source reset",
		means:  "This member stopped replicating from whichever member it had been following, usually because that member is no longer the primary.",
		enrich: func(r lsMongoRecord, e *lsEvent) { e.Peer = r.host("previousSyncSource") }},
	{ids: []int{21799, 3873117}, class: lsClassReplica, sev: lsSevInfo, label: "Chose a sync source",
		means: "The member picked which other member to replicate from. A secondary syncing from another secondary rather than the primary is normal and adds a little lag.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if p := lsMongoShortHost(r.str("syncSource")); p != "" {
				e.Peer = p
			}
		}},

	// ---- rollback: the one that loses data ----------------------------------------
	{ids: []int{21593}, class: lsClassConflict, sev: lsSevBad, label: "Rollback started",
		means:  "This member had accepted writes that the rest of the set never agreed to, and is now discarding them. That happens when it was the primary on the wrong side of a partition: it acknowledged writes to clients, and those writes are about to be undone.",
		enrich: func(_ lsMongoRecord, e *lsEvent) { e.State = lsStateRollback }},
	{ids: []int{6984700}, class: lsClassConflict, sev: lsSevBad, label: "Operations reverted by rollback",
		means: "The count of writes this member is throwing away. Every one of them may have been acknowledged to a client that believed it had been stored.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			var parts []string
			total := int64(0)
			for _, k := range []string{"insert", "update", "delete"} {
				if n, ok := r.num(k); ok && n > 0 {
					parts = append(parts, fmt.Sprintf("%d %s", n, k))
					total += n
				}
			}
			if len(parts) > 0 {
				e.Message = strings.Join(parts, ", ") + " reverted"
				e.Label = fmt.Sprintf("Rollback reverted %d operation(s)", total)
			}
		}},
	{ids: []int{21609}, class: lsClassConflict, sev: lsSevBad, label: "Rolled-back documents written to a file",
		means: "The discarded documents were saved rather than deleted outright. This path is the only copy of data that was acknowledged and then taken away — it is the first thing to go and fetch, and it is deleted by nothing but you.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			ns, file := r.str("namespace"), r.str("file")
			if ns != "" && file != "" {
				e.Message = ns + " → " + file
				e.Label = "Rollback file written for " + ns
			}
		}},
	{ids: []int{21607}, class: lsClassConflict, sev: lsSevWarn, label: "Rollback common point found",
		means: "The last point at which this member and the set still agreed. Everything this member did after it is being undone."},
	{ids: []int{21592}, class: lsClassConflict, sev: lsSevWarn, label: "Rollback complete",
		means: "The member has finished discarding and will now catch up from the current primary normally."},
	// The rollback summary, and the only record that carries the count on EVERY version.
	// 6984700 says the same thing more directly but does not exist before 7.0, so a 6.0
	// rollback read through 6984700 alone reports that data was lost without saying how
	// much — which is the one number the person reading it needs. Verified identical on
	// 6.0.29-23, 7.0.39-21 and 8.0.28-12.
	{ids: []int{21612}, class: lsClassConflict, sev: lsSevBad, label: "Rollback summary",
		means: "What the rollback actually removed, and where the discarded documents were put. This is the record to read first: it is the only one that survives on every server version and it carries both the count and the path.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			var parts []string
			if raw, ok := r.Attr["rollbackCommandCounts"]; ok {
				var counts map[string]int64
				if json.Unmarshal(raw, &counts) == nil {
					var kinds []string
					for k := range counts {
						kinds = append(kinds, k)
					}
					sort.Strings(kinds)
					for _, k := range kinds {
						if counts[k] > 0 {
							parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
						}
					}
				}
			}
			total, hasTotal := r.num("totalEntriesRolledBackIncludingNoops")
			switch {
			case len(parts) > 0:
				e.Message = strings.Join(parts, ", ") + " reverted"
				e.Label = "Rollback reverted " + strings.Join(parts, ", ")
			case hasTotal:
				e.Message = fmt.Sprintf("%d oplog entries reverted", total)
			}
			if ns := r.strs("affectedNamespaces"); len(ns) > 0 {
				e.Message += " in " + strings.Join(ns, ", ")
			}
			if dir := r.str("rollbackDataFileDirectory"); dir != "" {
				e.Message += " — discarded documents under " + dir
			}
		}},
	{ids: []int{21611}, class: lsClassState, sev: lsSevOK, label: "Back to SECONDARY after rollback",
		means:  "The member is serving again, without the writes it lost.",
		enrich: func(_ lsMongoRecord, e *lsEvent) { e.State = lsStateSecondary }},

	// ---- initial sync ---------------------------------------------------------------
	{ids: []int{4280513, 21164}, class: lsClassTransfer, sev: lsSevWarn, label: "Initial sync required",
		means:  "The member has no usable data — a new member, a wiped data directory, or one that fell so far behind that the primary's oplog no longer covers the gap. It will copy the entire dataset from another member.",
		enrich: func(_ lsMongoRecord, e *lsEvent) { e.State = lsStateStartup2 }},
	{ids: []int{4280514}, class: lsClassTransfer, sev: lsSevWarn, label: "Initial sync started",
		means:  "The whole-dataset copy is running. The member serves nothing until it finishes, and the member it is copying from does real work for the duration.",
		enrich: func(_ lsMongoRecord, e *lsEvent) { e.State = lsStateStartup2 }},
	{ids: []int{21191}, class: lsClassTransfer, sev: lsSevOK, label: "Initial sync finished",
		means: "The copy completed and the member can join the set properly."},
	{ids: []int{21181}, class: lsClassTransfer, sev: lsSevInfo, label: "Finished fetching the oplog for initial sync",
		means: "The catch-up phase of the initial sync — the member is applying everything that changed while the copy was running."},

	// ---- oplog: the thing that decides whether recovery is possible at all -----------
	{ids: []int{21107, 21094}, class: lsClassReplica, sev: lsSevInfo, label: "Replication producer stopped",
		means: "The member stopped fetching the oplog, normally because it is becoming primary or changing sync source."},

	// ---- lifecycle ------------------------------------------------------------------
	{ids: []int{4615611}, class: lsClassStartup, sev: lsSevInfo, label: "MongoDB starting",
		means:  "A new mongod process. Everything above this line belongs to a different run of the server.",
		enrich: func(_ lsMongoRecord, e *lsEvent) { e.State = lsStateStarting }},
	// 20698 is "***** SERVER RESTARTED *****", a start-up marker — not a shutdown.
	{ids: []int{20698}, class: lsClassStartup, sev: lsSevWarn, label: "Server restarted",
		means: "mongod's own restart marker. Everything above this line belongs to a previous run of the process."},
	{ids: []int{4784900}, class: lsClassShutdown, sev: lsSevWarn, label: "Stepping down for shutdown",
		means: "The server was asked to stop and is giving up the primary role first. A clean shutdown of a primary always costs an election, which is why rolling restarts are done secondaries-first."},
	{ids: []int{20565, 23138}, class: lsClassShutdown, sev: lsSevInfo, label: "Shutdown complete",
		means:  "The process ended on request.",
		enrich: func(_ lsMongoRecord, e *lsEvent) { e.State = lsStateDown }},
	{ids: []int{22322}, class: lsClassShutdown, sev: lsSevInfo, label: "Checkpoint thread shutting down",
		means: "WiredTiger's checkpointer stopping, which happens on the way out of every shutdown — clean or not."},

	// ---- NOT SEEN IN A CAPTURE ------------------------------------------------------
	//
	// Every rule above matched a record in app/testdata/logsummary/m*. These four did not.
	// The scenarios that produce them — a member left down past the oplog window, a failed
	// initial sync, an unclean shutdown recovered by WiredTiger — are not ones the capture
	// session produced, and inventing them was not worth the risk of inventing the records
	// too. They are kept because the ids are documented and the events are worth
	// recognising; they are fenced off here because an unexercised rule is a guess until a
	// real log matches it. Three earlier rules in this file WERE guesses of exactly this
	// kind and all three were wrong — 22322 is "Shutting down checkpoint thread", not a
	// fatal assertion; 21444 is a dry-run election SUCCEEDING, not failing; 20698 is a
	// restart marker, not a shutdown. The corpus caught all three. Nothing below it has
	// been caught by anything.
	//
	// The 6.0/7.0 sweep shrank this block from four rules to two and confirmed why it is
	// fenced. 20557 was moved out of it — not because it was verified, but because SIGKILL
	// on all three versions never produced it, and 22271/501401/20631 did. The two left are
	// still guesses, and a deliberate attempt was made on both: rolling a 6.0 member off the
	// end of a 990 MiB oplog — first by stopping it, then by SIGSTOPping it so its optime
	// froze while the process stayed alive — produced an ordinary catch-up and an ordinary
	// initial sync, never 5579600. So even the scenario that is supposed to produce it does
	// not, reliably, and the rule stays here until a real log says otherwise.
	{ids: []int{4280510}, class: lsClassTransfer, sev: lsSevBad, label: "Initial sync failed",
		means: "The whole-dataset copy did not complete. MongoDB retries, and a member that keeps failing here never becomes usable — check whether the donor is healthy and whether the member has room for the data."},
	{ids: []int{5579600}, class: lsClassReplica, sev: lsSevBad, label: "Too stale to catch up",
		means:  "This member is further behind than the primary's oplog goes back, so there is nothing left to replay. It cannot catch up incrementally and needs a full initial sync. The fix is a bigger oplog, sized against the longest outage the set has to survive.",
		enrich: func(_ lsMongoRecord, e *lsEvent) { e.State = lsStateRecovering }},
	{ids: []int{23015}, class: lsClassStartup, sev: lsSevOK, label: "Ready for connections",
		means: "mongod is accepting connections. For a replica-set member this is not the same as being usable: it says nothing about whether the member has joined the set or has current data."},

	// ---- an unclean stop, and what it costs ------------------------------------------
	//
	// 20557 used to sit in the fenced-off block below as "Unclean shutdown detected". It was
	// a guess, and SIGKILLing a mongod on 6.0, 7.0 and 8.0 never produced it once. These
	// three did, on all three versions — mongod says it three times, from three different
	// subsystems, which is why they share a rule and collapse into one row.
	{ids: []int{22271, 501401, 20631}, class: lsClassCrash, sev: lsSevBad, label: "Previous shutdown was not clean",
		means:  "The last mongod on this data directory did not stop on request — it was killed, OOM-killed, or the host went away. Nothing is lost: WiredTiger replays its journal on the way up. What it costs is time, and the fact that it happened at all is not otherwise recorded anywhere the application can see.",
		enrich: func(_ lsMongoRecord, e *lsEvent) { e.State = lsStateStarting }},
	{ids: []int{22302}, class: lsClassCrash, sev: lsSevInfo, label: "Recovering from the last checkpoint",
		means: "The recovery half of an unclean start: WiredTiger is replaying from its most recent checkpoint. On a large busy dataset this is where the start-up time goes, and until it finishes the member is not in the set at all."},

	// ---- replication breaking rather than lagging ------------------------------------
	{ids: []int{21122}, class: lsClassReplica, sev: lsSevWarn, label: "Oplog fetcher stopped with an error",
		means: "The member's oplog fetcher gave up on its sync source. It retries, so one of these is unremarkable; a run of them is a member that is not receiving the oplog at all, which looks like lag from the outside and is not lag.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if m := r.str("error"); m != "" {
				e.Message = m
			}
			if p := lsMongoShortHost(r.str("lastOpTimeFetched")); p == "" {
				if p := lsMongoShortHost(r.str("syncSource")); p != "" {
					e.Peer = p
				}
			}
		}},

	// ---- configuration ---------------------------------------------------------------
	{ids: []int{21392}, class: lsClassConfig, sev: lsSevInfo, label: "New replica set config in use",
		means: "The set's membership or settings changed. The config version and term increment on every change, so a gap in them means a reconfiguration this log did not witness.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if raw, ok := r.Attr["config"]; ok {
				var cfg struct {
					Version int `json:"version"`
					Term    int `json:"term"`
					Members []struct {
						Host string `json:"host"`
					} `json:"members"`
				}
				if json.Unmarshal(raw, &cfg) == nil {
					e.Members = len(cfg.Members)
					var names []string
					for _, m := range cfg.Members {
						names = append(names, lsMongoShortHost(m.Host))
					}
					e.Message = fmt.Sprintf("v%d term %d: %s", cfg.Version, cfg.Term, strings.Join(names, ", "))
				}
			}
		}},

	// ---- index builds ------------------------------------------------------------
	// A build on a large collection is minutes of work with a beginning, an end and a
	// commit that every member has to agree to, and until now every line of it was
	// dropped as noise. It matters for two reasons: a foreground build blocks the
	// collection it is on, and a build in progress is aborted when the member steps down
	// (7738702), so a failover during one silently throws away however long it had run.
	{ids: []int{20438, 20346, 20384}, class: lsClassStorage, sev: lsSevInfo, label: "Index build started",
		means: "A new index is being built. On a large collection this is minutes to hours of scanning, sorting and spilling to disk, and it competes with everything else for the same cache and the same device.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if ns := r.str("namespace"); ns != "" {
				e.Message = ns
				if props := r.str("properties"); props != "" {
					e.Message += " " + props
				}
			}
		}},
	{ids: []int{20440}, class: lsClassStorage, sev: lsSevInfo, label: "Waiting for an index build",
		means: "The command that asked for the index is waiting for the build to finish. On a foreground build this is the application's connection, held open for the whole build."},
	{ids: []int{20689}, class: lsClassStorage, sev: lsSevInfo, label: "Index build: side writes drained",
		means: "Writes that arrived during the build have been applied to the new index. This is the last phase before it can be committed, and it is where a build on a busy collection spends longer than the scan did."},
	{ids: []int{7568000, 3856201}, class: lsClassStorage, sev: lsSevInfo, label: "Index build: commit quorum",
		means: "The members are voting on committing the index. A build cannot finish until enough of them are ready, so an unreachable member holds every build in the set open."},
	{ids: []int{20345, 20447, 20663}, class: lsClassStorage, sev: lsSevOK, label: "Index build finished",
		means: "The index is built and usable. Pair this with the start record for how long it took — that duration is how long the collection was competing with a scan of itself.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if ns := r.str("namespace"); ns != "" {
				e.Message = ns
			}
			if ms, ok := r.num("durationMillis"); ok {
				e.Message = strings.TrimSpace(e.Message + fmt.Sprintf(" in %s", lsMongoDur(float64(ms)/1000)))
			}
		}},
	{ids: []int{7738702}, class: lsClassStorage, sev: lsSevWarn, label: "Aborting all index builds",
		means: "Every index build on this member is being abandoned, which is what a stepdown does to them. Whatever they had scanned is discarded and the build has to start again on the new primary."},
	{ids: []int{20657}, class: lsClassStorage, sev: lsSevInfo, label: "Index builds resumed on step-up",
		means: "This member became primary and took over the index builds the old primary was running."},

	// ---- who this member replicates from -----------------------------------------
	// Replication is a tree and each secondary picks its own parent. The choice is
	// invisible everywhere else, and a member that cannot find one — or that keeps
	// changing its mind — lags for a reason none of its own metrics explain.
	{ids: []int{21088, 21080, 4744901, 21834, 21150}, class: lsClassReplica, sev: lsSevInfo, label: "Changed sync source",
		means: "This member started replicating from a different member. One change is routine; a stream of them means it cannot find a stable parent, and everything downstream of it inherits the interruption.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			from, to := r.host("oldSyncSource"), r.host("newSyncSource")
			if to == "" {
				to = r.host("syncSource")
			}
			switch {
			case from != "" && to != "":
				e.Peer, e.Message = to, from+" → "+to
			case to != "":
				e.Peer, e.Message = to, "now syncing from "+to
			}
		}},
	{ids: []int{3873113, 3873106, 3873107, 21090, 8423402}, class: lsClassReplica, sev: lsSevWarn, label: "No usable sync source",
		means: "This member could not find anyone to replicate from — every candidate was behind it, unreachable or not readable. While that lasts it is not applying anything, and its lag grows with no error of its own to explain it.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if h := r.host("syncSourceCandidate"); h != "" {
				e.Peer = h
			}
		}},

	// ---- the connection pool this member keeps to the others ----------------------
	// The cluster of records a peer failure produces on the OTHER members, minutes before
	// anything says the peer is down. Individually they are chatter; together they are
	// the earliest evidence in the file that something on the far end has stopped.
	{ids: []int{22566, 22561, 22572}, class: lsClassNetwork, sev: lsSevWarn, label: "Dropped pooled connections",
		means: "This member threw away its pooled connections to another host, which it does when they stop working. It is the first symptom of a peer that has died or been partitioned away — earlier than the heartbeat failures, and much earlier than any state change.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if h := r.host("hostAndPort"); h != "" {
				e.Peer = h
			}
		}},
	{ids: []int{6496500}, class: lsClassNetwork, sev: lsSevWarn, label: "Timed out waiting for a connection",
		means: "An operation gave up waiting for a connection from the pool. Either the pool is at its limit, or the far end is not answering fast enough to hand one back."},
	{ids: []int{6496400}, class: lsClassNetwork, sev: lsSevInfo, label: "Slow connection establishment",
		means: "Opening a connection to another host took long enough to be worth logging — DNS, TLS handshake or the network itself."},
	{ids: []int{4712102}, class: lsClassMember, sev: lsSevWarn, label: "Host failed in the replica set",
		means: "This member's own view of the set marked a host as failed. It is the client-side twin of a heartbeat failure and it fires on routers too, which have no heartbeats of their own.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if h := r.host("host"); h != "" {
				e.Peer = h
			}
		}},

	// ---- errors the server returned to somebody ------------------------------------
	{ids: []int{23074, 23077, 21962}, class: lsClassClient, sev: lsSevWarn, label: "Assertion returned to a client",
		means: "The server refused an operation and told the client why. These are the errors an application sees, counted in serverStatus.asserts — a burst of them is worth pairing with what the application was doing.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if err := r.str("error"); err != "" {
				e.Message = lsTruncateLabel(err)
			}
		}},
	{ids: []int{23799, 20478}, class: lsClassClient, sev: lsSevWarn, label: "Query executor error",
		means: "An aggregation or a getMore failed part-way through. The client saw an error on a cursor it had already started reading, which is the shape of failure applications handle worst."},
	{ids: []int{20482, 20884, 558700}, class: lsClassClient, sev: lsSevWarn, label: "Operation killed",
		means: "Somebody ran killOp, or the server killed the operation itself. A killed operation has already done its work and thrown it away."},

	// ---- storage housekeeping -------------------------------------------------------
	{ids: []int{22214, 20318, 22260, 22237, 6776600}, class: lsClassStorage, sev: lsSevInfo, label: "Collection dropped",
		means: "A collection or index was dropped and its storage is being released. MongoDB defers the actual file removal until no reader needs the old snapshot, which is why these arrive in bursts long after the drop."},
	{ids: []int{6936300}, class: lsClassStorage, sev: lsSevWarn, label: "Drop is blocked by a reader",
		means: "The storage for a dropped collection cannot be released because something is still using it. Long-running readers and long snapshots hold disk that the drop was supposed to give back."},
	{ids: []int{5479200}, class: lsClassStorage, sev: lsSevInfo, label: "TTL deleted expired documents",
		means: "The TTL monitor removed expired documents. It is a background writer nobody remembers is there: its deletes replicate, they hit the oplog, and they compete with the application for the same tickets.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if n, ok := r.num("numDeleted"); ok {
				e.Message = fmt.Sprintf("%d document(s) from %s", n, r.str("namespace"))
			}
		}},

	// ---- the server telling you about your own configuration ------------------------
	{ids: []int{8386700, 23803, 636300, 4648601, 20712}, class: lsClassConfig, sev: lsSevWarn, label: "Configuration warning",
		means: "The server flagged something about how it was started or how the host is set up. These are written once at startup and never again, which is why nobody sees them.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			for _, k := range []string{"note", "message", "deprecatedParameter", "parameter"} {
				if v := r.str(k); v != "" {
					e.Message = lsTruncateLabel(v)
					return
				}
			}
		}},
	{ids: []int{7024600}, class: lsClassConfig, sev: lsSevInfo, label: "Deprecated command used",
		means: "Something still calls a command this server version has deprecated. It works today; it is a note for whoever owns the client."},

	// ---- WiredTiger's own complaint about a long snapshot ---------------------------
	// Only present at verbosity 1 or above, and worth keeping when it is: a snapshot held
	// open for tens of seconds is what pins the history store and grows the cache pressure
	// the storage charts show.
	{ids: []int{22411}, class: lsClassStorage, sev: lsSevInfo, label: "Long-lived storage snapshot",
		means: "A WiredTiger transaction stayed open long enough for the engine to complain. Every update to a document it read has to be kept somewhere until it closes, which is what fills the history store and squeezes the cache.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if ms, ok := r.num("transactionTime"); ok {
				e.Message = fmt.Sprintf("snapshot open for %s", lsMongoDur(float64(ms)/1000))
				if ms >= 30000 {
					e.Sev = lsSevWarn
				}
			}
		}},
}

// lsMongoByID indexes the catalogue. Built once: a busy member's log is hundreds of
// thousands of lines and a linear scan of the rules per line is the difference between a
// page that loads and one that does not.
var lsMongoByID = func() map[int]*lsMongoRule {
	m := map[int]*lsMongoRule{}
	// The sharded catalogue is merged rather than kept separate: a shard member's log is
	// both a replica-set log and a sharding one, and which rules apply is decided by which
	// records are in the file rather than by anything declared up front.
	for i := range lsShardRules {
		r := &lsShardRules[i]
		for _, id := range r.ids {
			m[id] = r
		}
	}
	for i := range lsMongoRules {
		for _, id := range lsMongoRules[i].ids {
			m[id] = &lsMongoRules[i]
		}
	}
	return m
}()

// lsClassifyMongo turns one record into an event, or reports that it is noise.
func lsClassifyMongo(r lsMongoRecord) (lsEvent, bool) {
	e := lsEvent{
		TS: r.TS, Line: r.Line, Time: r.Time, Level: lsMongoLevel(r.Level),
		Code: strconv.Itoa(r.ID), Subsys: r.Comp,
		Class: lsClassOther, Sev: lsSevInfo, Label: r.Msg, Message: r.Msg,
	}
	if rule := lsMongoByID[r.ID]; rule != nil {
		e.Class, e.Sev, e.Label, e.Meaning = rule.class, rule.sev, rule.label, rule.means
		if rule.enrich != nil {
			rule.enrich(r, &e)
		}
		// The level is a floor here as elsewhere, but it lifts almost nothing: MongoDB
		// logs a rollback at "I". The catalogue is doing the work.
		e.Sev = lsWorse(e.Sev, lsLevelFloor(lsMongoLevel(r.Level)))
		return e, true
	}
	// Unrecognised. Keep what the server itself called a problem; drop the rest, which on
	// a MongoDB member is the overwhelming majority — connection accounting, index build
	// progress, per-command chatter.
	if floor := lsLevelFloor(lsMongoLevel(r.Level)); lsSevRank[floor] >= lsSevRank[lsSevWarn] {
		e.Sev = floor
		e.Label = lsTruncateLabel(r.Msg)
		return e, true
	}
	return lsEvent{}, false
}

// lsMongoLevel maps MongoDB's one-letter severity onto the words the rest of the package
// uses. D (debug) is below anything the shared floor recognises and maps to nothing.
func lsMongoLevel(s string) string {
	switch strings.TrimSpace(s) {
	case "F":
		return "ERROR" // fatal is worse than error, and the floor has no rung above bad
	case "E":
		return "ERROR"
	case "W":
		return "WARNING"
	}
	return "NOTE"
}

// lsSniffMongoRS answers "is this a replica-set member's log?" A standalone mongod has no
// REPL or ELECTION component and never logs a state transition; a member logs all three
// within the first second of start-up.
func lsSniffMongoRS(recs []lsMongoRecord) bool {
	for _, r := range recs {
		switch r.Comp {
		case "REPL", "ELECTION", "ROLLBACK", "REPL_HB", "INITSYNC":
			return true
		}
		// The components above are the obvious evidence and they are not always there. A
		// twenty-thousand-line tail from a real member turned out to be entirely NETWORK
		// and CONNPOOL — the replica-set monitor complaining about an unreachable peer,
		// several thousand times, and not one REPL record among them. That member was
		// filed as a standalone mongod, which then dragged it through the MySQL findings.
		//
		// `attr.replicaSet` is the durable signal: every record the replica-set monitor
		// writes carries the set's name, on every version, and a standalone mongod has
		// nothing to put in it.
		if _, ok := r.Attr["replicaSet"]; ok {
			return true
		}
	}
	return false
}

// lsMongoNodeName pulls the member's own name out of its log.
//
// Two records state it outright, which is a luxury the Galera and Group Replication
// catalogues do not have: 21393 ("Found self in config") carries the member's own
// hostAndPort, and 4615611 ("MongoDB starting") carries the host name. Both are written
// during start-up, before anything can have gone wrong.
func lsMongoNodeName(recs []lsMongoRecord) string {
	for _, r := range recs {
		if r.ID == 21393 {
			if h := r.host("hostAndPort"); h != "" {
				return h
			}
		}
	}
	for _, r := range recs {
		if r.ID == 4615611 {
			if h := lsMongoShortHost(r.str("host")); h != "" {
				return h
			}
		}
	}
	return ""
}

// lsMongoStateMeaning explains the replica-set states for the legend.
var lsMongoStateMeaning = map[string]string{
	lsStatePrimaryM:  "the one member accepting writes",
	lsStateSecondary: "applying the primary's oplog and serving reads",
	lsStateStartup2:  "copying the whole dataset from another member — it serves nothing until that finishes",
	lsStateRollback:  "discarding writes it had already accepted and acknowledged, because the rest of the set never agreed to them",
	lsStateArbiter:   "votes in elections and holds no data",
	lsStateRemoved:   "not in the replica-set configuration — running, and part of nothing",
}

// lsResolveMongo fills in the states that the record itself does not state, and turns the
// peer reports into evidence about the peer rather than about the file they were found in.
//
// The peer half matters more here than anywhere else in this package. Every member reports
// every other member's state (id 21215), so mongo01's log contains the full history of
// mongo02 and mongo03 — but an event carrying `State` would otherwise be read as a
// statement about mongo01, which is where the file came from. So a peer record's state is
// deliberately NOT written to e.State; the finding layer reads e.Peer instead.
func lsResolveMongo(node string, events []lsEvent) {
	for i := range events {
		e := &events[i]
		// A peer report is about somebody else. Its state must not become this source's.
		if e.Peer != "" && e.Peer != node && (e.Code == "21215" || e.Code == "21216") {
			e.State = ""
		}
	}
}

// lsMongoDur renders a duration the way an operator says one, so a build that took eight
// and a half minutes does not read as "507000 ms".
func lsMongoDur(sec float64) string {
	switch {
	case sec >= 3600:
		return fmt.Sprintf("%.1f h", sec/3600)
	case sec >= 90:
		return fmt.Sprintf("%.0f min %02.0f s", sec/60, sec-60*float64(int(sec/60)))
	case sec >= 1:
		return fmt.Sprintf("%.1f s", sec)
	default:
		return fmt.Sprintf("%.0f ms", sec*1000)
	}
}
