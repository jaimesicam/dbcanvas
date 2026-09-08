package main

// logsummary_postgres_verdict.go — what a PostgreSQL cluster's logs add up to.
//
// Two of these say something no single file can, and both were verified by driving the
// incident rather than by reasoning about it.
//
// lsFindingPGDCSLost is the one worth the page. A Patroni leader that cannot reach etcd
// demotes itself, and PostgreSQL's own log for that window shows a clean shutdown with no
// reason attached — "received fast shutdown request", and nothing else. An operator reading
// only the database log sees a primary that stopped for no reason at all. The cause is in a
// different file, written by a different process, and it is a network problem rather than a
// database one.
//
// lsFindingPGLagInvisible is the honest note, and PostgreSQL's version is worse than
// MySQL's or MongoDB's. Not only is lag absent from the log — the standby writes a steady
// stream of "waiting for WAL to become available", which is what a HEALTHY idle standby
// writes, and what a standby receiving nothing at all writes too. The file looks the same
// either way.

import (
	"fmt"
	"sort"
	"strings"
)

// lsPGFindings are the PostgreSQL checks, appended to lsFindings' list.
var lsPGFindings = []func(*lsBundle) []lsFinding{
	lsFindingPGDCSLost,
	lsFindingPGNoPrimary,
	lsFindingPGDiverged,
	lsFindingPGReplBroken,
	lsFindingPGFailover,
	lsFindingPGRecoveryConflict,
	lsFindingPGConnLimit,
	lsFindingPGCheckpointPressure,
	lsFindingPGLagInvisible,
}

// lsEventEnd is when an event finished. Collapsed events carry the last occurrence in
// EndTS, and for a record repeated every few seconds for the length of an outage — which is
// what "cannot reach the DCS" and "could not connect to the primary" both are — that is the
// difference between reporting an instant and reporting the outage.
func lsEventEnd(e lsEvent) float64 {
	if e.EndTS > e.TS {
		return e.EndTS
	}
	return e.TS
}

// lsIsPG reports whether a source speaks any of the three PostgreSQL vocabularies.
func lsIsPG(flavour string) bool {
	return flavour == lsFlavourPostgres || flavour == lsFlavourPGStream || flavour == lsFlavourPatroni
}

// lsHasPG reports whether the bundle contains a PostgreSQL server at all.
func lsHasPG(b *lsBundle) bool {
	for _, s := range b.Sources {
		if lsIsPG(s.Flavour) {
			return true
		}
	}
	return false
}

// lsHasPatroni reports whether any source is a Patroni member — the gate on every finding
// that talks about leader locks and the DCS, neither of which a plain streaming pair has.
func lsHasPatroni(b *lsBundle) bool {
	for _, s := range b.Sources {
		if s.Flavour == lsFlavourPatroni {
			return true
		}
	}
	return false
}

// lsHasPGStreaming reports whether replication is in evidence anywhere.
func lsHasPGStreaming(b *lsBundle) bool {
	for _, s := range b.Sources {
		if s.Flavour == lsFlavourPGStream || s.Flavour == lsFlavourPatroni {
			return true
		}
	}
	return false
}

// lsFindingPGDCSLost — the leader stood down because it could not reach etcd.
//
// The failure people are most often surprised by, and the one the database log cannot
// explain. Verified by stopping etcd on all three members of a healthy cluster: Patroni
// logged "demoting self because DCS is not accessible and I was a leader", PostgreSQL logged
// a fast shutdown, and nothing in PostgreSQL's file connects the two.
func lsFindingPGDCSLost(b *lsBundle) []lsFinding {
	if !lsHasPatroni(b) {
		return nil
	}
	unreachable := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Patroni: cannot reach the DCS" || e.Label == "Patroni: FAILED to renew the leader lock"
	})
	if len(unreachable) == 0 {
		return nil
	}
	stood := lsPick(b, func(e lsEvent) bool {
		return strings.HasPrefix(e.Label, "Patroni: stood down") || e.Label == "Patroni: demoting while offline"
	})
	// How long any member could not see the DCS, per member, from first complaint to last.
	spans := map[int][2]float64{}
	for _, e := range unreachable {
		s, seen := spans[e.Src]
		if !seen {
			spans[e.Src] = [2]float64{e.TS, lsEventEnd(e)}
			continue
		}
		s[1] = lsEventEnd(e)
		spans[e.Src] = s
	}
	var who []string
	worst := 0.0
	for _, src := range lsSrcSet(unreachable) {
		s := spans[src]
		if d := s[1] - s[0]; d > worst {
			worst = d
		}
		who = append(who, fmt.Sprintf("%s from %s for %s", lsNode(b, src), lsClock(s[0]), lsDur(s[1]-s[0])))
	}
	f := lsFinding{
		ID: "pg-dcs-lost", Sev: lsSevWarn,
		Title:  "Patroni could not reach the DCS",
		Detail: strings.Join(who, "; ") + ". Patroni keeps a lease in etcd and a leader that cannot renew it will not stay leader, because it cannot tell an unreachable DCS from one that has already given the lock to somebody else. Nothing about PostgreSQL is wrong while this is happening.",
		Advice: "This is a network or an etcd problem, not a database one — and the PostgreSQL log will not say so. Check etcd's own health and the path between it and the members. If the members are healthy but the DCS is flaky, the cluster will keep losing its primary for reasons no database metric explains.",
		At:     unreachable[0].TS, Sources: lsSrcSet(unreachable), Events: lsEventNos(unreachable, 6),
	}
	if len(stood) > 0 {
		f.Sev = lsSevBad
		f.Title = "The leader stood down because it could not reach the DCS"
		// One line per member. Patroni writes the decision, the demotion and the completion
		// as three records seconds apart, and listing all three reads as three incidents.
		var lines []string
		seen := map[int]bool{}
		for _, e := range stood {
			if seen[e.Src] {
				continue
			}
			seen[e.Src] = true
			lines = append(lines, fmt.Sprintf("%s at %s", lsNode(b, e.Src), lsClock(e.TS)))
		}
		f.Detail = strings.Join(lines, "; ") + " demoted itself with nothing wrong with PostgreSQL at all. " + f.Detail
		f.Events = append(lsEventNos(stood, 3), f.Events...)
	}
	return []lsFinding{f}
}

// lsFindingPGNoPrimary — a stretch with nobody taking writes.
//
// Read from Patroni's own view of the lock rather than inferred from the members' states:
// "Lock owner: None" is Patroni saying outright that the cluster has no leader, and it is
// written every loop for as long as that lasts.
func lsFindingPGNoPrimary(b *lsBundle) []lsFinding {
	if !lsHasPatroni(b) {
		return nil
	}
	none := lsPick(b, func(e lsEvent) bool {
		return e.Code == "" && strings.Contains(e.Message, "Lock owner: None")
	})
	if len(none) == 0 {
		return nil
	}
	// The gap closes at the next record naming an owner, on any member.
	owned := lsPick(b, func(e lsEvent) bool {
		return strings.Contains(e.Message, "Lock owner: ") && !strings.Contains(e.Message, "Lock owner: None")
	})
	total, worst, at := 0.0, 0.0, 0.0
	for _, e := range none {
		end := b.Summary.LastTS
		for _, o := range owned {
			if o.TS > lsEventEnd(e) {
				end = o.TS
				break
			}
		}
		d := end - e.TS
		if d <= 0 {
			continue
		}
		total += d
		if d > worst {
			worst, at = d, e.TS
		}
	}
	if worst < 1 {
		return nil
	}
	return []lsFinding{{
		ID: "pg-no-primary", Sev: lsSevBad,
		Title:  fmt.Sprintf("The cluster had no primary for %s", lsDur(worst)),
		Detail: fmt.Sprintf("Patroni reported the leader lock as unheld from %s. A PostgreSQL cluster with no primary takes no writes at all — every INSERT, UPDATE and DELETE fails — while the standbys carry on answering reads perfectly, which is why an application can look half-alive through one of these.", lsClock(at)),
		Advice: "The interesting question is always why the previous leader stopped, and that is above this in the same window: a demotion, a stood-down leader, or a member that was not healthy enough to take over.",
		At:     at, Sources: lsSrcSet(none), Events: lsEventNos(none, 6),
	}}
}

// lsFindingPGDiverged — writes that were accepted and then thrown away.
//
// PostgreSQL's equivalent of a MongoDB rollback, and rarer only because it takes a real
// split to produce. A member that was primary on the wrong side of a failure has WAL the
// rest of the cluster never agreed to; pg_rewind discards it, and a rebuild from the leader
// discards the whole data directory along with it.
func lsFindingPGDiverged(b *lsBundle) []lsFinding {
	rewind := lsPick(b, func(e lsEvent) bool { return e.Label == "Patroni: rewinding a diverged member" })
	// A rebuild is only an incident if the member had already been serving. Every replica
	// in a new cluster is created by copying the leader, and reporting that as writes
	// discarded would flag every healthy deployment on the day it was built. The test is
	// whether this source had reached a serving state BEFORE the rebuild — a member being
	// created for the first time never has.
	rebuild := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Patroni: rebuilding this member from the leader" && lsPGServedBefore(b, e)
	})
	if len(rewind) == 0 && len(rebuild) == 0 {
		return nil
	}
	ev := append(append([]lsEvent{}, rewind...), rebuild...)
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].TS < ev[j].TS })
	var lines []string
	for _, src := range lsSrcSet(ev) {
		what := "was rebuilt from the leader"
		for _, e := range rewind {
			if e.Src == src {
				what = "was rewound"
			}
		}
		lines = append(lines, lsNode(b, src)+" "+what)
	}
	sev, title := lsSevWarn, "A member was rebuilt from the leader"
	advice := "A rebuild copies the entire database and the leader does real work for the whole of it. On a large cluster this is worth scheduling rather than discovering."
	if len(rewind) > 0 {
		sev, title = lsSevBad, "A diverged member had its writes discarded"
		advice = "pg_rewind is not lossless: it exists precisely to throw away WAL the rest of the cluster never accepted. Any client that received a commit for one of those transactions was told something that stopped being true. Whether that matters depends on whether anything was written to the old primary after it lost contact — which the old primary's own log, just before it stopped, is the place to check."
	}
	return []lsFinding{{
		ID: "pg-diverged", Sev: sev, Title: title,
		Detail: strings.Join(lines, "; ") + ". This happens when a member had accepted writes that the rest of the cluster never agreed to — it was the primary on the wrong side of a failure — and cannot simply resume following the new leader.",
		Advice: advice,
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 6),
	}}
}

// lsFindingPGReplBroken — replication that cannot resume on its own.
//
// Two failures with the same shape and the same fix: the standby needs a fresh base backup.
// Both are ordinary-looking single lines in a log full of ordinary-looking single lines.
func lsFindingPGReplBroken(b *lsBundle) []lsFinding {
	ev := lsPick(b, func(e lsEvent) bool {
		return e.Label == "The WAL the standby needs is gone" ||
			e.Label == "Requested WAL segment already removed" ||
			e.Label == "The replication slot is missing"
	})
	if len(ev) == 0 {
		return nil
	}
	slotOnly := true
	for _, e := range ev {
		if e.Label != "The replication slot is missing" {
			slotOnly = false
		}
	}
	var who []string
	for _, src := range lsSrcSet(ev) {
		who = append(who, lsNode(b, src))
	}
	// Did it come back? A slot that is missing while a cluster is still being built is
	// created moments later, and reporting that as "retrying will not fix it" is wrong —
	// retrying is exactly what fixed it. The claim only holds where nothing resumed.
	last := lsEventEnd(ev[len(ev)-1])
	resumed := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Streaming from the primary" && e.TS > last
	})
	f := lsFinding{
		ID: "pg-repl-broken", Sev: lsSevBad,
		Title:   "Replication cannot resume by itself",
		Detail:  strings.Join(who, ", ") + " could not start or continue streaming, and retrying will not fix it.",
		Advice:  "A standby in this state needs a fresh base backup — pg_basebackup, or whatever the cluster manager calls its rebuild. It will keep retrying and keep failing until somebody does that.",
		At:      ev[0].TS,
		Until:   last,
		Sources: lsSrcSet(ev), Events: lsEventNos(ev, 6),
	}
	if len(resumed) > 0 {
		f.Sev = lsSevWarn
		f.Title = "Replication broke and came back"
		f.Detail = strings.Join(who, ", ") + " could not stream for a while, and did resume afterwards — so this was survivable rather than terminal."
		f.Advice = "Worth knowing anyway: both of these failures are permanent when they happen to a standby that has been away for real, and the same records appear either way. What made this one survivable is that it happened while the cluster was still being built."
		f.Until = resumed[0].TS
	}
	if slotOnly {
		f.Detail += " The replication slot it is configured to use does not exist on the primary."
		f.Advice = "Create the slot on the primary, or point the standby at one that exists. While the slot is missing nothing is holding WAL for this standby either, so the longer it stays missing the more likely the WAL it needs is also gone by the time it is fixed."
	} else {
		f.Detail += " The WAL it asked for had already been recycled by the primary, so there is nothing left to replay from."
		f.Advice += " The reason it happened is that nothing was holding the WAL: no replication slot, or wal_keep_size too small for how long the standby was away."
	}
	return []lsFinding{f}
}

// lsFindingPGFailover — every change of primary, and how long writes stopped for.
func lsFindingPGFailover(b *lsBundle) []lsFinding {
	promoted := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Promoted — new timeline" || e.Label == "Patroni: promoted itself to leader"
	})
	if len(promoted) == 0 {
		return nil
	}
	// Only an explicit request counts as evidence of a planned handover. "Demoting for a
	// switchover" was in this list and should not have been: Patroni logs a graceful
	// demotion during an UNPLANNED failover too, so including it labelled every promotion
	// in the corpus "requested", including one caused by SIGKILL.
	requested := lsPick(b, func(e lsEvent) bool { return e.Label == "Patroni: handover requested" })
	// One line per promotion, deduplicated and classified in the same pass: the PostgreSQL
	// record and Patroni's record of one promotion arrive a second apart and are one event.
	//
	// Deliberately one loop. The first version deduplicated into `lines` and then classified
	// by walking `promoted` and indexing `lines` by position — which are different lengths
	// the moment anything is deduplicated, so the labels landed on the wrong promotions and
	// an unplanned failover four minutes later was marked as requested.
	var lines []string
	planned, unplanned := 0, 0
	last := -100.0
	for _, e := range promoted {
		if e.TS-last < 10 {
			continue
		}
		last = e.TS
		line := fmt.Sprintf("%s at %s", lsNode(b, e.Src), lsClock(e.TS))
		if tl := lsPGTimelineOf(e.Message); tl > 0 {
			line += fmt.Sprintf(" (timeline %d)", tl)
		}
		if lsPGAskedFor(requested, e.TS) {
			planned++
			line += " — requested"
		} else {
			unplanned++
		}
		lines = append(lines, line)
	}
	sev := lsSevBad
	kind := "The cluster changed primary"
	detail := "At least one of these was unplanned: something took the primary away and a standby was promoted in its place."
	switch {
	case unplanned == 0:
		sev = lsSevWarn
		kind = "The cluster changed primary, on request"
		detail = "Every one of these was asked for. Writes still stop for the length of a switchover: it is a short outage taken deliberately, not an avoided one."
	case planned > 0:
		kind = fmt.Sprintf("The cluster changed primary %d times, %d of them unplanned", planned+unplanned, unplanned)
	}
	return []lsFinding{{
		ID: "pg-failover", Sev: sev, Title: kind,
		Detail: strings.Join(lines, "; ") + ". " + detail + " Each promotion forks the cluster's history onto a new timeline, and every other standby has to be told to follow it — one that had already replayed past the fork has diverged and cannot come back without being rewound or rebuilt.",
		Advice: "The write outage is not the promotion itself: it runs from the moment the old primary stopped accepting writes to the 'ready to accept connections' on the new one. Both ends are on the timeline above.",
		At:     promoted[0].TS, Sources: lsSrcSet(promoted), Events: lsEventNos(promoted, 8),
	}}
}

// lsFindingPGRecoveryConflict — reads killed on a standby by the primary's vacuum.
//
// Worth a finding of its own because the cause is on a different server from the symptom.
// The application sees queries failing on the standby; nothing is wrong with the standby.
func lsFindingPGRecoveryConflict(b *lsBundle) []lsFinding {
	ev := lsPick(b, func(e lsEvent) bool {
		return e.Class == lsClassConflict && strings.Contains(e.Label, "recovery conflict")
	})
	if len(ev) == 0 {
		return nil
	}
	n := 0
	for _, e := range ev {
		n += 1 + e.Repeat
	}
	return []lsFinding{{
		ID: "pg-recovery-conflict", Sev: lsSevWarn,
		Title:  fmt.Sprintf("%d quer%s cancelled on a standby by recovery", n, map[bool]string{true: "y was", false: "ies were"}[n == 1]),
		Detail: "Reads on the standby were killed so that WAL replay could continue. The application sees queries failing for no reason it did anything wrong, and the standby is not at fault: the cause is on the PRIMARY, which vacuumed away row versions the standby's queries were still reading.",
		Advice: "Two settings decide this and they trade against each other. max_standby_streaming_delay is how long replay will wait before killing the query — raising it protects the reads and lets the standby fall behind. hot_standby_feedback tells the primary not to vacuum rows the standby still needs — it protects the reads without lag, at the cost of bloat on the primary.",
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 6),
	}}
}

// lsFindingPGConnLimit — the outage that is not an outage.
func lsFindingPGConnLimit(b *lsBundle) []lsFinding {
	ev := lsPick(b, func(e lsEvent) bool { return e.Label == "Connection limit reached" })
	if len(ev) == 0 {
		return nil
	}
	n := 0
	for _, e := range ev {
		n += 1 + e.Repeat
	}
	return []lsFinding{{
		ID: "pg-conn-limit", Sev: lsSevBad,
		Title:  fmt.Sprintf("The connection limit was reached %d time(s)", n),
		Detail: "max_connections was full and PostgreSQL refused new clients while serving its existing ones perfectly well. From outside it looks like the database is down; from inside it looks healthy, which is why this is so often diagnosed as something else.",
		Advice: "A connection pooler is the fix. Raising max_connections mostly converts the problem into memory pressure, because every backend is a process with its own work_mem.",
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 6),
	}}
}

// lsFindingPGCheckpointPressure — PostgreSQL asking, in words, for a configuration change.
func lsFindingPGCheckpointPressure(b *lsBundle) []lsFinding {
	ev := lsPick(b, func(e lsEvent) bool { return e.Label == "Checkpoints too frequent" })
	if len(ev) == 0 {
		return nil
	}
	return []lsFinding{{
		ID: "pg-checkpoint-pressure", Sev: lsSevWarn,
		Title:  "Checkpoints are being forced by WAL volume",
		Detail: "PostgreSQL is checkpointing because it ran out of WAL room rather than because the interval elapsed. Each forced checkpoint is a burst of writes, and the server gets slower in a way no individual query accounts for.",
		Advice: "The message says what to raise max_wal_size to, and it is one of the few times PostgreSQL asks for a specific change in words. It costs disk and nothing else.",
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 4),
	}}
}

// lsFindingPGLagInvisible — the honest note, and the worst of the three.
//
// MySQL writes nothing about replica lag and MongoDB writes nothing about member lag. A
// PostgreSQL standby is worse than silent: it writes "waiting for WAL to become available"
// steadily, which is exactly what a healthy idle standby writes AND exactly what a standby
// receiving nothing at all writes. The reader is not merely uninformed but actively
// reassured.
func lsFindingPGLagInvisible(b *lsBundle) []lsFinding {
	if !lsHasPGStreaming(b) {
		return nil
	}
	waiting := 0
	for _, e := range lsPick(b, func(e lsEvent) bool { return e.Label == "Waiting for WAL" }) {
		waiting += 1 + e.Repeat
	}
	f := lsFinding{
		ID: "pg-lag-invisible", Sev: lsSevInfo,
		Title:  "Replication lag is not in this log",
		Detail: "A PostgreSQL standby writes nothing when it falls behind — no warning, no periodic note. A member can be hours behind and leave no trace in a window like this one.",
		Advice: "Lag lives in the server: pg_stat_replication on the primary gives write, flush and replay LSNs per standby, and pg_last_wal_replay_lsn() with pg_last_xact_replay_timestamp() on the standby gives the other side. Patroni's own `patronictl list` prints it per member, which is the quickest of the three.",
	}
	if waiting > 0 {
		f.Detail += fmt.Sprintf(" Worse, there are %d 'waiting for WAL to become available' records here, and that message means the same thing whether the standby is idle and up to date or receiving nothing at all — it is written in both cases and cannot tell them apart.", waiting)
	}
	return []lsFinding{f}
}

// lsPGServedBefore reports whether a rebuild happened to a member that was already part of a
// working cluster, rather than one being created.
//
// The obvious test — had this member ever been serving? — does not work, and the corpus is
// why. Patroni initdbs a throwaway local instance on a brand-new member before it copies the
// leader, so every replica IS briefly a primary of its own empty cluster, minutes before it
// joins anything. Asking whether the CLUSTER already existed separates the two cleanly:
// "initialized a new cluster" is written once, by the first member, and a rebuild in the
// minute after it is the cluster being built rather than repaired.
func lsPGServedBefore(b *lsBundle, e lsEvent) bool {
	for _, x := range b.Events {
		if x.Label != "Patroni: created the cluster" {
			continue
		}
		if e.TS-x.TS < 120 && e.TS >= x.TS-60 {
			return false
		}
	}
	return true
}

// lsPGAskedFor reports whether a handover was requested close enough before a promotion to
// be the reason for it. Patroni writes the request, then demotes, then the new leader
// promotes — seconds apart, but not instantly, and on different members.
func lsPGAskedFor(requested []lsEvent, ts float64) bool {
	for _, r := range requested {
		if d := ts - r.TS; d >= -5 && d <= 90 {
			return true
		}
	}
	return false
}
