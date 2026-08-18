package main

// logsummary_mongo_verdict.go — what a MongoDB replica-set bundle adds up to.
//
// One of these matters more than anything else in this package: lsFindingMongoRollback.
// Every other incident the Log Summary reports is an outage — something stopped serving,
// and then it served again. A rollback is different in kind. Writes that were accepted and
// acknowledged to a client are deliberately discarded, the client is never told, and the
// only record that it happened at all is the one this finding reads.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// lsMongoFindings are the replica-set checks, appended to lsFindings' list.
var lsMongoFindings = []func(*lsBundle) []lsFinding{
	lsFindingShardNoPrimary,
	lsFindingShardChanges,
	lsFindingRoutingInvisible,
	lsFindingMongoRollback,
	lsFindingMongoNoPrimary,
	lsFindingMongoElection,
	lsFindingMongoMemberDown,
	lsFindingMongoInitialSync,
	lsFindingMongoTooStale,
	lsFindingMongoLagInvisible,
	// Added with the slow-query pass: what the workload itself cost, read out of the
	// lines the catalogue drops.
	lsFindingMongoCollscan,
	lsFindingMongoDiskReads,
	lsFindingMongoWaiting,
	lsFindingMongoWriteConcern,
	lsFindingMongoIndexBuild,
	lsFindingMongoSyncChurn,
	lsFindingMongoPoolChurn,
	lsFindingMongoLogVolume,
}

// lsHasMongoRS reports whether any source is a replica-set member.
//
// Every replica-set finding is gated on this, which is also what keeps them off a router.
// A mongos monitors every shard and logs a great deal about replica sets without being in
// one; without the gate, a perfectly healthy router is reported as a member that never
// became primary and never had an oplog.
func lsHasMongoRS(b *lsBundle) bool {
	for _, s := range b.Sources {
		if s.Flavour == lsFlavourMongoRS {
			return true
		}
	}
	return false
}

// lsFindingMongoRollback — acknowledged writes that were thrown away.
//
// The capture behind it: a primary was cut off from the other two, forty documents were
// written to it with w:1 and acknowledged, the majority elected a new primary, and the link
// was restored. MongoDB reverted 43 operations on the old primary and wrote the discarded
// documents to files under the data directory. Nothing told the client.
//
// So this finding reports three things in the order an engineer needs them: that it
// happened, how much went, and where the only surviving copy is.
func lsFindingMongoRollback(b *lsBundle) []lsFinding {
	if !lsHasMongoRS(b) {
		return nil
	}
	started := lsPick(b, func(e lsEvent) bool { return e.Code == "21593" })
	if len(started) == 0 {
		return nil
	}
	// Two records say what was thrown away, and the older one is the better one. 6984700
	// arrived in 7.0 and gives the counts alone; 21612's rollback summary exists on 6.0 too
	// AND carries the affected namespaces and the directory the discarded documents went to.
	// Preferring it means a 6.0 capture reports how much was lost rather than only that
	// something was — and that 7.0 and 8.0 stop reporting LESS than 6.0 did, which is how
	// this read before the version sweep.
	reverted := lsPick(b, func(e lsEvent) bool { return e.Code == "21612" && e.Message != "" })
	if len(reverted) == 0 {
		reverted = lsPick(b, func(e lsEvent) bool { return e.Code == "6984700" })
	}
	files := lsPick(b, func(e lsEvent) bool { return e.Code == "21609" })

	var who []string
	seen := map[int]bool{}
	for _, e := range started {
		if seen[e.Src] {
			continue
		}
		seen[e.Src] = true
		who = append(who, fmt.Sprintf("%s at %s", lsNode(b, e.Src), lsClock(e.TS)))
	}
	detail := fmt.Sprintf("%s rolled back. A rollback happens when a member that had been the primary rejoins and finds the rest of the set never accepted its most recent writes — it had been on the wrong side of a partition. Those writes are undone. Any client that wrote with w:1 was told they had succeeded.", strings.Join(who, "; "))
	for _, e := range reverted {
		if e.Message != "" {
			detail += fmt.Sprintf(" On %s: %s.", lsNode(b, e.Src), e.Message)
		}
	}
	f := lsFinding{
		ID: "mongo-rollback", Sev: lsSevBad, At: started[0].TS,
		Title:   "Acknowledged writes were rolled back and lost",
		Detail:  detail,
		Sources: lsSrcSet(started),
		Events:  lsEventNos(append(append([]lsEvent{}, started...), reverted...), 6),
	}
	if len(files) > 0 {
		var paths []string
		for _, e := range files {
			if e.Message != "" {
				paths = append(paths, e.Message)
			}
		}
		sort.Strings(paths)
		if len(paths) > 4 {
			paths = append(paths[:4], fmt.Sprintf("… and %d more", len(paths)-4))
		}
		f.Advice = "The discarded documents were written to rollback files before being removed, and those files are the only copy left: " + strings.Join(paths, "; ") + ". Nothing deletes them for you and nothing replays them for you — read them with bsondump and decide what has to go back in. Then work out why a write was acknowledged before the set had agreed to it: w:1 (or w:majority with journaling off) is what allows this, and w:majority is what prevents it."
	} else {
		f.Advice = "Look under the member's data directory for a rollback/ folder — the discarded documents are written there before they are removed, and that is the only copy. Then move the application to w:majority, which is what stops a write being acknowledged before the set has agreed to it."
	}
	return []lsFinding{f}
}

// lsFindingMongoNoPrimary — stretches when the set had no primary, and so took no writes.
//
// Built from the peer reports rather than from any one member's own state, because that is
// what makes it a statement about the SET: a member knows its own state, and it knows every
// other member's, so one file is enough to say "nobody was primary between these times".
func lsFindingMongoNoPrimary(b *lsBundle) []lsFinding {
	if !lsHasMongoRS(b) {
		return nil
	}
	// Every moment somebody became primary, and every moment somebody stopped being one.
	type mark struct {
		ts   float64
		who  string
		up   bool
		evNo int
	}
	// Names have to be normalised before anything is counted. A member's own transition is
	// keyed by the SOURCE name (which may be a file name like "mongo02.log") and a peer
	// report is keyed by the host out of the record ("mongo02") — and if those two spellings
	// do not collapse to one key, the same member is tracked twice. The half that never
	// sees a "became primary" then stays false for ever, and the set reads as having had no
	// primary for the rest of the window. That is exactly what it did on the first live run:
	// 21.5 minutes of "no primary" against a set that had one throughout.
	name := func(s string) string { return lsMongoShortHost(s) }
	var marks []mark
	for _, e := range b.Events {
		switch e.Code {
		case "21358": // this member's own transition
			if e.State == lsStatePrimaryM {
				marks = append(marks, mark{e.TS, name(lsNode(b, e.Src)), true, e.No})
			} else if e.From == lsStatePrimaryM {
				marks = append(marks, mark{e.TS, name(lsNode(b, e.Src)), false, e.No})
			}
		case "21215": // a peer's transition, as this member heard it
			if e.Peer == "" {
				continue
			}
			up := strings.HasSuffix(e.Message, lsStatePrimaryM)
			marks = append(marks, mark{e.TS, name(e.Peer), up, e.No})
		case "21216": // a peer went down; if it was primary, it is not any more
			if e.Peer != "" {
				marks = append(marks, mark{e.TS, name(e.Peer), false, e.No})
			}
		}
	}
	if len(marks) == 0 {
		return nil
	}
	sort.Slice(marks, func(i, j int) bool { return marks[i].ts < marks[j].ts })

	primaries := map[string]bool{}
	var gaps [][2]float64
	gapFrom := 0.0
	haveSeenOne := false
	for _, m := range marks {
		was := len(primaries) > 0
		primaries[m.who] = m.up
		now := false
		for _, v := range primaries {
			if v {
				now = true
			}
		}
		if was && !now {
			gapFrom = m.ts
		}
		if !was && now && gapFrom > 0 {
			gaps = append(gaps, [2]float64{gapFrom, m.ts})
			gapFrom = 0
		}
		if now {
			haveSeenOne = true
		}
	}
	// A gap still open when the log ends is reported, but only as far as the last record
	// that could have closed it. Running it to the end of the window asserts an outage
	// nothing witnessed — the log simply stopped, and "no member said it was primary
	// after this point" is a much weaker claim than "there was no primary".
	openEnded := false
	if gapFrom > 0 && haveSeenOne {
		last := gapFrom
		for _, m := range marks {
			if m.ts > last {
				last = m.ts
			}
		}
		if last > gapFrom {
			gaps = append(gaps, [2]float64{gapFrom, last})
			openEnded = true
		}
	}
	if len(gaps) == 0 {
		return nil
	}
	worst, total := 0.0, 0.0
	var lines []string
	for _, g := range gaps {
		d := g[1] - g[0]
		if d <= 0.5 {
			continue // a handover measured in milliseconds is not an outage
		}
		total += d
		if d > worst {
			worst = d
		}
		lines = append(lines, fmt.Sprintf("%s for %s", lsClock(g[0]), lsDur(d)))
	}
	if len(lines) == 0 {
		return nil
	}
	sev := lsSevWarn
	if worst > 30 {
		sev = lsSevBad
	}
	detail := fmt.Sprintf("No member was primary during: %s. A replica set with no primary accepts no writes at all — every insert, update and delete fails, while reads against secondaries keep working and returning data. That asymmetry is why an application can look half-alive during one of these.", strings.Join(lines, "; "))
	if openEnded {
		detail += " The last of those stretches is still open where this log ends — nothing here records a primary again, which is not the same as there not having been one."
	}
	return []lsFinding{{
		ID: "mongo-no-primary", Sev: sev, At: gaps[0][0],
		Title:  fmt.Sprintf("The set had no primary for %s", lsDur(total)),
		Detail: detail,
		Advice: "The default electionTimeoutMillis is 10 seconds, and a failover costs roughly that plus the catch-up. A gap much longer than that means the election itself struggled — usually because a majority was not reachable, so no candidate could win. Check which members could see each other at the time, not just which one died.",
	}}
}

// lsFindingMongoElection — who took over from whom, and how long writes were failing.
func lsFindingMongoElection(b *lsBundle) []lsFinding {
	if !lsHasMongoRS(b) {
		return nil
	}
	won := lsPick(b, func(e lsEvent) bool { return e.Code == "21450" })
	if len(won) == 0 {
		return nil
	}
	var lines []string
	seen := map[string]bool{}
	for _, e := range won {
		key := fmt.Sprintf("%s@%.0f", lsNode(b, e.Src), e.TS)
		if seen[key] {
			continue
		}
		seen[key] = true
		line := fmt.Sprintf("%s became primary at %s", lsNode(b, e.Src), lsClock(e.TS))
		// Was this a planned handover or a failure? rs.stepDown() logs 4615661 first.
		for _, s := range b.Events {
			if s.Code == "4615661" && s.TS <= e.TS && e.TS-s.TS < 30 {
				line += " (a requested step-up, not a failure)"
				break
			}
		}
		lines = append(lines, line)
	}
	return []lsFinding{{
		ID: "mongo-election", Sev: lsSevWarn, At: won[0].TS,
		Title:   fmt.Sprintf("The set elected a primary %s", lsTimes(len(lines))),
		Detail:  strings.Join(lines, "; ") + ". A new primary does not accept writes the moment it wins — it first applies everything the old primary had already replicated, which is the catch-up phase, and the application's outage ends at the end of that rather than at the election.",
		Sources: lsSrcSet(won), Events: lsEventNos(won, 4),
	}}
}

// lsFindingMongoMemberDown — a member the others could not reach, and for how long.
//
// The duration comes from the heartbeat failures, which is the one place MongoDB's
// relentless repetition is an asset: it retries every two seconds for as long as the member
// is unreachable, so the span of the collapsed row IS the length of the outage.
func lsFindingMongoMemberDown(b *lsBundle) []lsFinding {
	if !lsHasMongoRS(b) {
		return nil
	}
	down := lsPick(b, func(e lsEvent) bool { return e.Code == "21216" && e.Peer != "" })
	if len(down) == 0 {
		return nil
	}
	// Per unreachable member: when it was first declared down, and when heartbeats to it
	// last failed.
	type row struct{ from, to float64 }
	byPeer := map[string]*row{}
	for _, e := range down {
		r := byPeer[e.Peer]
		if r == nil {
			byPeer[e.Peer] = &row{from: e.TS, to: e.TS}
			continue
		}
		if e.TS < r.from {
			r.from = e.TS
		}
	}
	for _, e := range b.Events {
		if e.Code != "23974" || e.Peer == "" {
			continue
		}
		if r := byPeer[e.Peer]; r != nil {
			end := e.TS
			if e.EndTS > end {
				end = e.EndTS
			}
			if end > r.to {
				r.to = end
			}
		}
	}
	var peers []string
	for p := range byPeer {
		peers = append(peers, p)
	}
	sort.Strings(peers)
	var lines []string
	for _, p := range peers {
		r := byPeer[p]
		if d := r.to - r.from; d > 1 {
			lines = append(lines, fmt.Sprintf("%s from %s for %s", p, lsClock(r.from), lsDur(d)))
		} else {
			lines = append(lines, fmt.Sprintf("%s at %s", p, lsClock(r.from)))
		}
	}
	return []lsFinding{{
		ID: "mongo-member-down", Sev: lsSevBad, At: down[0].TS,
		Title:   "A member could not be reached by the rest of the set",
		Detail:  fmt.Sprintf("%s. The duration is measured from the heartbeat failures, which repeat every two seconds for as long as the member is unreachable — so it is the length of the outage as the SURVIVORS experienced it, which is what matters for whether the set could still elect and still write.", strings.Join(lines, "; ")),
		Advice:  "A member unreachable to some peers and fine to others is a network problem between exactly those pairs. A member unreachable to everybody is the member. Either way, a three-member set survives one loss and stops accepting writes at two.",
		Sources: lsSrcSet(down), Events: lsEventNos(down, 4),
	}}
}

// lsFindingMongoInitialSync — a member that rebuilt itself from scratch.
func lsFindingMongoInitialSync(b *lsBundle) []lsFinding {
	if !lsHasMongoRS(b) {
		return nil
	}
	started := lsPick(b, func(e lsEvent) bool { return e.Code == "4280514" || e.Code == "4280513" })
	if len(started) == 0 {
		return nil
	}
	done := lsPick(b, func(e lsEvent) bool { return e.Code == "4280509" || e.Code == "21191" })
	var lines []string
	seen := map[int]bool{}
	for _, e := range started {
		if seen[e.Src] {
			continue
		}
		seen[e.Src] = true
		line := fmt.Sprintf("%s started an initial sync at %s", lsNode(b, e.Src), lsClock(e.TS))
		for _, d := range done {
			if d.Src == e.Src && d.TS > e.TS {
				line += fmt.Sprintf(", finished %s later", lsDur(d.TS-e.TS))
				break
			}
		}
		lines = append(lines, line)
	}
	return []lsFinding{{
		ID: "mongo-initial-sync", Sev: lsSevWarn, At: started[0].TS,
		Title:   "A member rebuilt itself from another member's data",
		Detail:  fmt.Sprintf("%s. An initial sync copies the entire dataset. The member serves nothing at all until it completes, and the member it copies from does real work for the whole duration — on a busy set that is felt by the application.", strings.Join(lines, "; ")),
		Advice:  "A new member doing this is expected. An existing member doing it is not, and means it lost its data or fell further behind than the primary's oplog reaches. If it is the latter, the oplog is too small for the outages this set actually has — size it against the longest one you need to survive, not against a rule of thumb.",
		Sources: lsSrcSet(started), Events: lsEventNos(started, 4),
	}}
}

// lsFindingMongoTooStale — the member that cannot catch up at all.
func lsFindingMongoTooStale(b *lsBundle) []lsFinding {
	if !lsHasMongoRS(b) {
		return nil
	}
	stale := lsPick(b, func(e lsEvent) bool { return e.Label == "Too stale to catch up" })
	if len(stale) == 0 {
		return nil
	}
	return []lsFinding{{
		ID: "mongo-too-stale", Sev: lsSevBad, At: stale[0].TS,
		Title:   fmt.Sprintf("%s fell off the end of the oplog", lsNode(b, stale[0].Src)),
		Detail:  "This member is further behind than the primary's oplog goes back, so there is nothing left for it to replay. It cannot catch up incrementally, however long it is left alone — it stays in RECOVERING, serving nothing, until somebody resyncs it.",
		Advice:  "Resync it (an initial sync, or restore from a backup taken inside the oplog window), then make the oplog big enough that this cannot happen again. The question to size it against is 'how long can a member be away and still come back cheaply' — replSetResizeOplog changes it without a restart.",
		Sources: lsSrcSet(stale), Events: lsEventNos(stale, 4),
	}}
}

// lsFindingMongoLagInvisible — the honest note, and a pointer at where the answer is.
//
// The same shape as the Galera and Group Replication flow-control notes, and true for the
// same reason: a secondary that falls behind writes nothing about it. What is different
// here is that MongoDB ships the answer with every server — diagnostic.data holds the
// replication lag, per second, whether or not anybody thought to turn anything on. So this
// note can do better than naming a status variable: it can point at a file that already
// exists on the machine, and at the page in this app that reads it.
func lsFindingMongoLagInvisible(b *lsBundle) []lsFinding {
	if !lsHasMongoRS(b) {
		return nil
	}
	return []lsFinding{{
		ID: "mongo-lag-invisible", Sev: lsSevInfo,
		Title:  "Replication lag is not in this log",
		Detail: "A MongoDB secondary that falls behind its primary writes nothing about it — not a warning, not a periodic note, nothing. The log records that replication is running and says no more, so a member can be hours behind through a window like this one and leave no trace in it at all.",
		Advice: "It is recorded somewhere else, and that somewhere is already on the machine: every mongod writes diagnostic.data in its dbPath, once a second, with no configuration — the replication lag, the oplog window, the queues and the cache are all in it. Feed that directory to FTDC Summary in this app, or read rs.printSecondaryReplicationInfo() for the position right now.",
	}}
}

// ---------------------------------------------------------------- sharded clusters

// lsHasMongos reports whether any source is a query router.
func lsHasMongos(b *lsBundle) bool {
	for _, s := range b.Sources {
		if s.Flavour == lsFlavourMongos {
			return true
		}
	}
	return false
}

// lsFindingShardNoPrimary — a shard that could not take writes, read out of the router's log.
//
// The router is the only process that watches every shard, and a shard with no primary is
// recorded there as a topology description inside an INFO record. That is the whole of the
// evidence: nothing is logged as a warning, nothing names the operations that failed.
func lsFindingShardNoPrimary(b *lsBundle) []lsFinding {
	ev := lsPick(b, func(e lsEvent) bool {
		return e.Code == "4333213" && strings.Contains(e.Message, "NoPrimary")
	})
	if len(ev) == 0 {
		return nil
	}
	// Per replica set, because a shard losing its primary and the config servers losing
	// theirs are different incidents with different blast radii.
	//
	// The span is measured to the record that ENDS it — the next topology description for
	// the same set that has a primary again — rather than between the first and last
	// NoPrimary record. Those give the same answer only when the outage is long: a set
	// observed without a primary exactly once reads as "for 0s", which is how the first
	// version of this reported a replica set that was still being formed.
	type window struct{ from, to float64 }
	open := map[string]float64{}
	windows := map[string][]window{}
	order := []string{}
	for _, e := range lsPick(b, func(e lsEvent) bool { return e.Code == "4333213" }) {
		set := e.Peer
		if set == "" {
			set = "a replica set"
		}
		lost := strings.Contains(e.Message, "NoPrimary")
		start, isOpen := open[set]
		switch {
		case lost && !isOpen:
			open[set] = e.TS
			if _, seen := windows[set]; !seen {
				order = append(order, set)
				windows[set] = nil
			}
		case !lost && isOpen:
			windows[set] = append(windows[set], window{start, e.TS})
			delete(open, set)
		}
	}
	// A window still open where the log ends is bounded by the last record in it, and
	// said to be still open rather than silently closed at a time nothing happened.
	stillOpen := map[string]bool{}
	for set, start := range open {
		windows[set] = append(windows[set], window{start, b.Summary.LastTS})
		stillOpen[set] = true
	}
	var lines []string
	config := false
	for _, set := range order {
		total, worst := 0.0, 0.0
		for _, w := range windows[set] {
			total += w.to - w.from
			if d := w.to - w.from; d > worst {
				worst = d
			}
		}
		// Under two seconds is a set forming or an election finishing, not an outage.
		// Reporting those alongside a fourteen-minute one buries it.
		if total < 2 {
			continue
		}
		line := fmt.Sprintf("%s from %s for %s", set, lsClock(windows[set][0].from), lsDur(total))
		if stillOpen[set] {
			line += " (still without one where this log ends)"
		}
		lines = append(lines, line)
		if set == "cfg" || set == "config" || strings.HasPrefix(set, "config") {
			config = true
		}
	}
	if len(lines) == 0 {
		return nil
	}
	f := lsFinding{
		ID: "mongo-shard-no-primary", Sev: lsSevBad,
		Title:  "A shard had no primary to route writes to",
		Detail: strings.Join(lines, "; ") + ". A sharded cluster fails one shard at a time: writes whose shard key lands on the affected shard fail, everything else carries on, and the application sees a fraction of its traffic erroring for no reason it can see from the outside.",
		Advice: "Read this together with that shard's own members' logs, which is where the election is. The router only records that the shape changed.",
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 8),
	}
	if config {
		f.Detail += " One of these is the CONFIG servers, which is the more serious of the two: while they have no primary the cluster's metadata is read-only, no chunk can move, no collection can be sharded or dropped, and any migration already in its critical section holds writes to that range until it ends."
	}
	return []lsFinding{f}
}

// lsFindingShardChanges — what the cluster's shape actually did, from the config servers'
// own changelog.
//
// Not an incident: a statement of what changed, which is the question asked of a sharded
// cluster more often than any other and which no single shard's log can answer.
func lsFindingShardChanges(b *lsBundle) []lsFinding {
	parts := lsShardChangeSummary(b)
	if len(parts) == 0 {
		return nil
	}
	ev := lsPick(b, func(e lsEvent) bool { return e.Code == "22080" })
	sev := lsSevInfo
	for _, e := range ev {
		if e.Sev == lsSevWarn {
			sev = lsSevWarn
		}
	}
	return []lsFinding{{
		ID: "mongo-shard-changes", Sev: sev,
		Title:  "The cluster's shape changed",
		Detail: "From the config servers' changelog: " + strings.Join(parts, ", ") + ". This is the only place any of it is recorded, and only on whichever config server was primary at the time — a bundle that does not include that member contains none of this.",
		Advice: "config.changelog holds the same events with their full details and outlives the log file. sh.status() shows where the chunks ended up.",
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 8),
	}}
}

// lsFindingRoutingInvisible — the honest note, and the sharded twin of lsFindingMongoLagInvisible.
//
// Verified rather than assumed: a whole three-member shard was stopped under live traffic on
// 6.0 and 7.0. The client got FailedToSatisfyReadPreference on a read and a refusal on a
// write. The router's log recorded the shard's topology changing — at INFO — and nothing
// else. Not a warning, not an error, no mention of an operation that failed.
//
// A page that stayed quiet about that would be read as "the router was fine", which is
// exactly the wrong conclusion to draw from a file that cannot say otherwise.
func lsFindingRoutingInvisible(b *lsBundle) []lsFinding {
	if !lsHasMongos(b) {
		return nil
	}
	return []lsFinding{{
		ID: "mongo-routing-invisible", Sev: lsSevInfo,
		Title:  "Failed routing is not in the router's log",
		Detail: "A mongos does not record the operations it could not route. Taking an entire shard down under live traffic produces reads that fail with FailedToSatisfyReadPreference and writes that are refused outright, and the router's log for that window contains neither — only INFO records saying the shard's topology changed. Absence of errors here is not evidence that clients were served.",
		Advice: "The router's side of it is in FTDC rather than the log: connPoolStats names every shard member and its round-trip time, and shardingStatistics counts how many shards each operation had to touch. The application's own error log has the failures themselves.",
	}}
}

// ---------------------------------------------------------------- what the work cost
//
// The findings below read the slow-query summary (logsummary_mongo_slow.go) rather than
// individual events. They are the questions six million dropped lines can answer and no
// single line can: which collection, which plan, how much of the time was the server
// working, and how much came off the disk to answer.

// lsFindingMongoCollscan — operations with no index to use.
//
// The ratio that matters is examined against returned. A query that reads 200,000
// documents to return 100 is doing 2,000 times the work it needs to, and it is doing it
// against the same cache and the same device as everything else on the server.
func lsFindingMongoCollscan(b *lsBundle) []lsFinding {
	var out []lsFinding
	for _, s := range b.Sources {
		st := s.Mongo
		if st == nil || st.Ops == 0 || st.Collscans == 0 {
			continue
		}
		// Scans are counted by what they COST, not by how many there were. A collection
		// scan over a handful of documents is the right plan and the majority of the
		// scans on any real member — reporting those as a finding is how a page teaches
		// people to ignore it.
		if st.CollDocs < 20000 {
			continue
		}
		share := float64(st.Collscans) / float64(st.Ops) * 100
		ratio := 0.0
		if st.CollRet > 0 {
			ratio = float64(st.CollDocs) / float64(st.CollRet)
		}
		waste := ""
		if ratio >= 2 {
			waste = fmt.Sprintf(" — %s× more reading than the answers needed", lsRatio(ratio))
		}
		worst := ""
		if len(st.Namespaces) > 0 {
			worst = fmt.Sprintf(" The busiest collection was %s (%s slow operations, %s documents examined).",
				st.Namespaces[0].Name, lsNum(int64(st.Namespaces[0].Ops)), lsNum(st.Namespaces[0].Docs))
		}
		sev := lsSevWarn
		if st.CollDocs >= 1000000 || ratio >= 100 {
			sev = lsSevBad
		}
		out = append(out, lsFinding{
			ID: "mongo-collscan-" + strconv.Itoa(s.Idx), Sev: sev, At: s.FirstTS, Until: s.LastTS,
			Title: fmt.Sprintf("%s: %s collection scans examined %s documents", lsNode(b, s.Idx), lsNum(int64(st.Collscans)), lsNum(st.CollDocs)),
			Detail: fmt.Sprintf("%.0f%% of the %s slow operations in this log had no index to use. Those scans read %s documents to return %s%s.%s",
				share, lsNum(int64(st.Ops)), lsNum(st.CollDocs), lsNum(st.CollRet), waste, worst),
			Advice:  "The plan summary on each slow-operation row names the shape with no index. Take its filter, and the examined-to-returned ratio is what an index on those fields would remove.",
			Sources: []int{s.Idx},
		})
	}
	return out
}

// lsRatio renders a multiple without pretending to precision it does not have.
func lsRatio(r float64) string {
	if r >= 10 {
		return fmt.Sprintf("%.0f", r)
	}
	return fmt.Sprintf("%.1f", r)
}

// lsFindingMongoDiskReads — how much of the answer came off the disk.
//
// This is the one number in the log that FTDC cannot attribute. The capture's cache
// counters say the server read 3.7 TiB into cache; only the log says which collection
// asked for it.
func lsFindingMongoDiskReads(b *lsBundle) []lsFinding {
	var out []lsFinding
	for _, s := range b.Sources {
		st := s.Mongo
		if st == nil || st.Bytes == 0 {
			continue
		}
		span := s.LastTS - s.FirstTS
		if span <= 0 {
			span = 1
		}
		rate := float64(st.Bytes) / span
		readSec := float64(st.ReadMicros) / 1e6
		// The share is of OPERATION time, not of the window. Thirty-two concurrent workers
		// accumulate thirty-two seconds of reading per second of wall clock, and a
		// percentage of the window computed from that reads as 1770%, which is not a
		// number anybody can act on. Against the time those same operations took, it is.
		opSec := float64(st.Millis) / 1000
		share := 0.0
		if opSec > 0 {
			share = readSec / opSec * 100
		}
		if share < 10 && rate < 1<<20 {
			continue
		}
		ns := ""
		if len(st.Namespaces) > 0 && st.Namespaces[0].Bytes > 0 {
			ns = fmt.Sprintf(" Most of it for %s.", st.Namespaces[0].Name)
		}
		sev := lsSevWarn
		if share >= 40 {
			sev = lsSevBad
		}
		out = append(out, lsFinding{
			ID: "mongo-disk-reads-" + strconv.Itoa(s.Idx), Sev: sev, At: s.FirstTS, Until: s.LastTS,
			Title: fmt.Sprintf("%s: slow operations read %s from disk", lsNode(b, s.Idx), lsBytesShort(st.Bytes)),
			Detail: fmt.Sprintf("%s/s across the window they cover, and %s of the %s those operations took was spent waiting for the device — %.0f%% of their own time. A working set that fitted in cache would read almost nothing.%s",
				lsBytesShort(int64(rate)), lsDur(readSec), lsDur(opSec), share, ns),
			Advice:  "Pair this with the FTDC capture for the same window: the cache chart says whether the cache was full, and the eviction chart says whether application threads were made to do the evicting. If both are healthy and this is still high, the working set is simply larger than the cache.",
			Sources: []int{s.Idx},
		})
	}
	return out
}

// lsFindingMongoWaiting — time the server had the operation and was not working on it.
func lsFindingMongoWaiting(b *lsBundle) []lsFinding {
	var out []lsFinding
	for _, s := range b.Sources {
		st := s.Mongo
		if st == nil || st.Millis == 0 || st.WaitedMs == 0 {
			continue
		}
		share := float64(st.WaitedMs) / float64(st.Millis) * 100
		if share < 15 {
			continue
		}
		sev := lsSevWarn
		if share >= 40 {
			sev = lsSevBad
		}
		out = append(out, lsFinding{
			ID: "mongo-waiting-" + strconv.Itoa(s.Idx), Sev: sev, At: s.FirstTS, Until: s.LastTS,
			Title: fmt.Sprintf("%s: %.0f%% of slow-operation time was spent waiting, not working", lsNode(b, s.Idx), share),
			Detail: fmt.Sprintf("Across %s logged operations, %s of %s was time the server had the operation and was not working on it — queued for admission, or blocked behind something else.",
				lsNum(int64(st.Ops)), lsDur(float64(st.WaitedMs)/1000), lsDur(float64(st.Millis)/1000)),
			Advice:  "Tuning the queries will not move this. The FTDC tickets and admission charts for the same window say what they were queued behind.",
			Sources: []int{s.Idx},
		})
	}
	return out
}

// lsFindingMongoWriteConcern — writes that finished and then waited for other members.
func lsFindingMongoWriteConcern(b *lsBundle) []lsFinding {
	var out []lsFinding
	for _, s := range b.Sources {
		st := s.Mongo
		if st == nil || st.WriteConOps == 0 {
			continue
		}
		avg := float64(st.WriteConMs) / float64(st.WriteConOps)
		if avg < 20 {
			continue
		}
		sev := lsSevWarn
		if avg >= 200 {
			sev = lsSevBad
		}
		out = append(out, lsFinding{
			ID: "mongo-write-concern-" + strconv.Itoa(s.Idx), Sev: sev, At: s.FirstTS, Until: s.LastTS,
			Title: fmt.Sprintf("%s: writes waited an average of %.0f ms for other members", lsNode(b, s.Idx), avg),
			Detail: fmt.Sprintf("%s logged write(s) spent %s in total waiting for write concern after the primary had already done the work. The application pays this on its own clock and nothing on the primary looks slow.",
				lsNum(int64(st.WriteConOps)), lsDur(float64(st.WriteConMs)/1000)),
			Advice:  "It is the secondaries: their apply rate and their disks. A member that is up but slow costs every majority write on the primary.",
			Sources: []int{s.Idx},
		})
	}
	return out
}

// lsFindingMongoIndexBuild — how long a build ran, and whether anything interrupted it.
func lsFindingMongoIndexBuild(b *lsBundle) []lsFinding {
	if !lsHasMongoRS(b) && !lsHasMongo(b) {
		return nil
	}
	starts := lsPick(b, func(e lsEvent) bool { return e.Code == "20438" || e.Code == "20384" })
	if len(starts) == 0 {
		return nil
	}
	dones := lsPick(b, func(e lsEvent) bool { return e.Code == "20345" || e.Code == "20447" || e.Code == "20663" })
	aborts := lsPick(b, func(e lsEvent) bool { return e.Code == "7738702" })
	// One build per source: pair the first start with the first completion after it.
	var lines []string
	longest := 0.0
	var at, until float64
	for _, st := range starts {
		var end float64
		for _, d := range dones {
			if d.Src == st.Src && d.TS >= st.TS {
				end = d.TS
				break
			}
		}
		if end == 0 {
			lines = append(lines, fmt.Sprintf("%s started a build at %s that never finished in this log", lsNode(b, st.Src), lsClock(st.TS)))
			if at == 0 {
				at = st.TS
			}
			continue
		}
		if d := end - st.TS; d > longest {
			longest, at, until = d, st.TS, end
		}
		lines = append(lines, fmt.Sprintf("%s: %s (%s)", lsNode(b, st.Src), lsDur(end-st.TS), lsIndexName(st.Message)))
	}
	if len(lines) == 0 {
		return nil
	}
	sev := lsSevInfo
	detail := strings.Join(lines, "; ") + "."
	if longest >= 60 {
		sev = lsSevWarn
		detail += " While a build runs it competes with the workload for the same cache and device, and the collection it is on is being scanned end to end."
	}
	if len(aborts) > 0 {
		sev = lsSevBad
		detail += fmt.Sprintf(" %s abandoned every running build (a stepdown does this) — whatever they had scanned was discarded.", lsNode(b, aborts[0].Src))
	}
	return []lsFinding{{
		ID: "mongo-index-build", Sev: sev, At: at, Until: until,
		Title:   fmt.Sprintf("Index build%s in this window", lsPluralS(len(lines))),
		Detail:  detail,
		Advice:  "An index build is a scheduled event even when nobody scheduled it. If one overlaps an incident, it is a candidate cause rather than a coincidence — and if a failover aborted one, it will start again from the beginning on the new primary.",
		Sources: lsSrcSet(starts), Events: lsEventNos(starts, 4),
	}}
}

// lsFindingMongoSyncChurn — a member that could not settle on a sync source.
func lsFindingMongoSyncChurn(b *lsBundle) []lsFinding {
	if !lsHasMongoRS(b) {
		return nil
	}
	none := lsPick(b, func(e lsEvent) bool {
		return e.Code == "3873113" || e.Code == "3873106" || e.Code == "3873107" || e.Code == "21090" || e.Code == "8423402"
	})
	changed := lsPick(b, func(e lsEvent) bool {
		return e.Code == "21088" || e.Code == "21080" || e.Code == "4744901" || e.Code == "21834" || e.Code == "21150"
	})
	if len(none) == 0 && len(changed) < 3 {
		return nil
	}
	byNode := map[string]int{}
	for _, e := range append(append([]lsEvent{}, none...), changed...) {
		byNode[lsNode(b, e.Src)]++
	}
	var parts []string
	for n, c := range byNode {
		parts = append(parts, fmt.Sprintf("%s (%d)", n, c))
	}
	sort.Strings(parts)
	sev := lsSevWarn
	if len(none) > 0 {
		sev = lsSevBad
	}
	all := append(append([]lsEvent{}, none...), changed...)
	return []lsFinding{{
		ID: "mongo-sync-churn", Sev: sev, At: all[0].TS,
		Title: "Members could not settle on a sync source",
		Detail: fmt.Sprintf("%d record(s) about choosing where to replicate from: %s. %s",
			len(all), strings.Join(parts, ", "),
			map[bool]string{true: "At least one member found no usable source at all — while that lasts it applies nothing and its lag grows with no error of its own.",
				false: "Repeated changes interrupt the oplog stream each time, and everything downstream of the member inherits the interruption."}[len(none) > 0]),
		Advice:  "Replication is a tree: a secondary may sync from another secondary. Check whether the member they keep rejecting is behind them, unreachable, or simply not readable yet.",
		Sources: lsSrcSet(all), Events: lsEventNos(all, 4),
	}}
}

// lsFindingMongoPoolChurn — connection pools thrown away, which is a peer failing.
func lsFindingMongoPoolChurn(b *lsBundle) []lsFinding {
	drops := lsPick(b, func(e lsEvent) bool { return e.Code == "22572" || e.Code == "22566" || e.Code == "22561" })
	timeouts := lsPick(b, func(e lsEvent) bool { return e.Code == "6496500" })
	if len(drops) < 2 && len(timeouts) == 0 {
		return nil
	}
	all := append(append([]lsEvent{}, drops...), timeouts...)
	peers := map[string]bool{}
	for _, e := range drops {
		if e.Peer != "" {
			peers[e.Peer] = true
		}
	}
	var names []string
	for p := range peers {
		names = append(names, p)
	}
	sort.Strings(names)
	who := "another host"
	if len(names) > 0 {
		who = strings.Join(names, ", ")
	}
	return []lsFinding{{
		ID: "mongo-pool-churn", Sev: lsSevWarn, At: all[0].TS,
		Title: fmt.Sprintf("Connections to %s were dropped and re-made", who),
		Detail: fmt.Sprintf("%d pool drop(s) and %d operation(s) that timed out waiting for a connection. This is the earliest thing in the file that says a peer stopped answering — it precedes the heartbeat failures, which precede any state change.",
			len(drops), len(timeouts)),
		Advice:  "If the FTDC capture for the same window shows TCP retransmits or resets, the network dropped it; if it does not, the far end stopped answering while its network stayed up.",
		Sources: lsSrcSet(all), Events: lsEventNos(all, 4),
	}}
}

// lsFindingMongoLogVolume — what this log cost to write.
//
// Not a database problem, and worth saying anyway: the capture behind this feature produced
// 10.5 GiB per member in 86 minutes at verbosity 1, on the same device the database was
// struggling to read from. Anybody about to recommend raising verbosity on a busy system
// should see the number first.
func lsFindingMongoLogVolume(b *lsBundle) []lsFinding {
	var out []lsFinding
	for _, s := range b.Sources {
		st := s.Mongo
		if st == nil || st.Debug == 0 {
			continue
		}
		span := s.LastTS - s.FirstTS
		if span <= 0 {
			continue
		}
		rate := float64(s.Bytes) / span
		share := float64(st.Debug) / float64(max(s.Records, 1)) * 100
		// Two different statements share this check, and conflating them was wrong: a high
		// share of debug records says the member is running at raised verbosity, while a
		// high byte rate says the log is expensive whatever its level. The tail of a log
		// can easily be one without the other.
		verbose := share >= 10
		if rate < 256*1024 && !verbose {
			continue
		}
		sev := lsSevInfo
		if rate >= 512*1024 {
			sev = lsSevWarn
		}
		why := fmt.Sprintf("At that rate it is %s an hour on the same filesystem the database is using.", lsBytesShort(int64(rate*3600)))
		if verbose {
			why = fmt.Sprintf("%.0f%% of its records are debug-level (%s lines), so this member is running at verbosity 1 or above — %s an hour on the same filesystem the database is using.",
				share, lsNum(int64(st.Debug)), lsBytesShort(int64(rate*3600)))
		}
		out = append(out, lsFinding{
			ID: "mongo-log-volume-" + strconv.Itoa(s.Idx), Sev: sev, At: s.FirstTS, Until: s.LastTS,
			Title:   fmt.Sprintf("%s: %s of log over %s (%s/s)", lsNode(b, s.Idx), lsBytesShort(int64(s.Bytes)), lsDur(span), lsBytesShort(int64(rate))),
			Detail:  why,
			Advice:  "Raise verbosity to answer a specific question, for as short a window as possible. FTDC is already running, costs a few megabytes an hour, and answers most of what a verbosity bump is reached for.",
			Sources: []int{s.Idx},
		})
	}
	return out
}

// lsHasMongo reports whether any source is a mongod at all, replica-set member or not.
func lsHasMongo(b *lsBundle) bool {
	for _, s := range b.Sources {
		if s.Engine == pktEngineMongoDB {
			return true
		}
	}
	return false
}

// lsNum renders a count with thousands separators, because these are large.
func lsNum(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// lsPluralS is "" or "s".
func lsPluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// lsIndexName shortens an index-build message to the collection and the index name. The
// record carries the whole index specification as JSON, which is precise and unreadable in
// a sentence.
func lsIndexName(msg string) string {
	ns, rest, ok := strings.Cut(msg, " ")
	if !ok {
		return msg
	}
	if i := strings.Index(rest, `"name":"`); i >= 0 {
		if name, _, ok := strings.Cut(rest[i+8:], `"`); ok {
			return ns + " · " + name
		}
	}
	return ns
}
