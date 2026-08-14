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
	"strings"
)

// lsMongoFindings are the replica-set checks, appended to lsFindings' list.
var lsMongoFindings = []func(*lsBundle) []lsFinding{
	lsFindingMongoRollback,
	lsFindingMongoNoPrimary,
	lsFindingMongoElection,
	lsFindingMongoMemberDown,
	lsFindingMongoInitialSync,
	lsFindingMongoTooStale,
	lsFindingMongoLagInvisible,
}

// lsHasMongoRS reports whether any source is a replica-set member.
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
	reverted := lsPick(b, func(e lsEvent) bool { return e.Code == "6984700" })
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
