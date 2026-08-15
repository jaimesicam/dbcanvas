package main

// logsummary_valkey_verdict.go — what a Valkey cluster's logs add up to.
//
// Three of these say something no single file can, and all three were verified by driving
// the incident rather than by reasoning about it.
//
// lsFindingVKClusterDown is the one worth the page. A Valkey Cluster refuses every client
// when any shard's slots are uncovered, and the nodes that are refusing are not the node
// that failed — they are perfectly healthy, they say so in their own logs, and none of them
// explains why it stopped answering. Measured: stopping one shard of three for thirty
// seconds left the other two logging "Cluster state changed: fail" and nothing else, while
// every client got CLUSTERDOWN.
//
// lsFindingVKKilled is the one that only exists because the collector reads two logs. A
// SIGKILLed valkey-server writes nothing at all — no crash report, no last line — so the
// Valkey half of the journal shows a log that simply stops and then starts again. systemd's
// half of the same journal says "Main process exited, code=killed, status=9/KILL", and that
// is the entire evidence.
//
// lsFindingVKInvisible is the honest note, and Valkey's is the largest of the six. Three
// separate things this server does are wholly absent from its log, and each was measured:
// 19,156 evictions produced zero records, a MISCONF write refusal produced zero records
// naming it, and three failed authentications produced zero records.

import (
	"fmt"
	"sort"
	"strings"
)

// lsValkeyFindings are the Valkey checks, appended to lsFindings' list.
var lsValkeyFindings = []func(*lsBundle) []lsFinding{
	lsFindingVKClusterDown,
	lsFindingVKKilled,
	lsFindingVKFailover,
	lsFindingVKDemoted,
	lsFindingVKNoTakeover,
	lsFindingVKPersistence,
	lsFindingVKFullResync,
	lsFindingVKSplitPromotion,
	lsFindingVKCleanStopIsInvisible,
	lsFindingVKInvisible,
}

// lsHasValkey reports whether the bundle contains a Valkey server at all.
func lsHasValkey(b *lsBundle) bool {
	for _, s := range b.Sources {
		if lsIsValkey(s.Flavour) {
			return true
		}
	}
	return false
}

// lsHasValkeyCluster reports whether any source is a Valkey Cluster member — the gate on
// every finding that talks about hash slots and elections, neither of which a standalone
// pair has.
func lsHasValkeyCluster(b *lsBundle) bool {
	for _, s := range b.Sources {
		if s.Flavour == lsFlavourValkeyCluster {
			return true
		}
	}
	return false
}

// lsVKPeerName turns a Valkey node id or address into something a reader recognises.
//
// Valkey Cluster identifies nodes by a 40-character hex id and, on a node with no announce
// name, prints an EMPTY bracket after it — so the id is all there is. When another source in
// the bundle turns out to be that node, its name is used instead: the same pooling Galera's
// UUID names get, and the same payoff, because "vkn5 was promoted" is a sentence and
// "e5a31ee3f6c5115dd337ce5e375717f8772762fb was promoted" is not.
func lsVKPeerName(b *lsBundle, peer string) string {
	if peer == "" {
		return ""
	}
	if n, ok := b.Names[peer]; ok && n != "" {
		return n
	}
	if len(peer) == 40 {
		return peer[:12] + "…"
	}
	return peer
}

// ---------------------------------------------------------------- the cluster outage

// lsFindingVKClusterDown — the whole cluster stopped serving.
//
// The finding this catalogue exists for, and the one with the widest gap between what the
// logs say and what the application saw. Verified: one shard of three stopped for thirty
// seconds, and every client of every node — including the two nodes that were entirely
// healthy — got CLUSTERDOWN for the duration.
func lsFindingVKClusterDown(b *lsBundle) []lsFinding {
	if !lsHasValkeyCluster(b) {
		return nil
	}
	failed := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Cluster state FAIL — the cluster stopped serving"
	})
	if len(failed) == 0 {
		return nil
	}
	ok := lsPick(b, func(e lsEvent) bool { return e.Label == "Cluster state OK — serving again" })

	// One outage per window, not one per node: every member logs the transition, so three
	// members reporting the same thirty seconds is one incident described three times.
	type outage struct {
		from, to float64
		srcs     []int
		open     bool
	}
	var outages []outage
	for _, e := range failed {
		merged := false
		for i := range outages {
			if e.TS-outages[i].from < 5 {
				outages[i].srcs = append(outages[i].srcs, e.Src)
				merged = true
				break
			}
		}
		if !merged {
			outages = append(outages, outage{from: e.TS, srcs: []int{e.Src}})
		}
	}
	for i := range outages {
		outages[i].open = true
		for _, o := range ok {
			if o.TS > outages[i].from {
				outages[i].to, outages[i].open = o.TS, false
				break
			}
		}
		if outages[i].open {
			outages[i].to = b.Summary.LastTS
		}
	}
	total, worst := 0.0, 0.0
	var lines []string
	stillDown := false
	for _, o := range outages {
		d := o.to - o.from
		total += d
		if d > worst {
			worst = d
		}
		if o.open {
			stillDown = true
		}
		seen := map[int]bool{}
		var who []string
		for _, s := range o.srcs {
			if !seen[s] {
				seen[s] = true
				who = append(who, lsNode(b, s))
			}
		}
		sort.Strings(who)
		line := fmt.Sprintf("from %s for %s, reported by %s", lsClock(o.from), lsDur(d), strings.Join(who, ", "))
		if o.open {
			line += " and still down when the log ended"
		}
		lines = append(lines, line)
	}
	// Why the slots were uncovered, if the bundle says. Two shapes and they need different
	// answers: a shard whose primary died with no replica cannot recover by itself, while one
	// whose replica was mid-election recovers in seconds.
	why := ""
	if len(lsPick(b, func(e lsEvent) bool { return e.Label == "Slots uncovered — the cluster is refusing clients" })) > 0 {
		why = " The reason given is that at least one hash slot had no reachable node to serve it."
	}
	f := lsFinding{
		ID: "vk-cluster-down", Sev: lsSevBad,
		Title: fmt.Sprintf("The cluster refused every client for %s", lsDur(worst)),
		Detail: strings.Join(lines, "; ") + "." + why +
			" With cluster-require-full-coverage at its default of yes, one shard's slots being uncovered makes the WHOLE cluster refuse every command — including on the nodes that are completely healthy. Those nodes log the state change and nothing else, so no single node's log explains why it stopped answering.",
		Advice: "Two questions, in this order. Which shard lost its slots, and did it have a replica to promote — a shard with no replica cannot recover on its own and the cluster stays down until somebody brings that node back. And second, whether cluster-require-full-coverage should be yes here at all: setting it to no confines the outage to the affected slots instead of the whole keyspace, which is right for a cache and wrong for anything that cannot serve a partial dataset.",
		At:     outages[0].from, Sources: lsSrcSet(failed), Events: lsEventNos(failed, 8),
	}
	if !stillDown {
		f.Until = outages[len(outages)-1].to
	}
	if len(outages) > 1 {
		f.Title = fmt.Sprintf("The cluster refused every client %d times, the longest for %s", len(outages), lsDur(worst))
	}
	return []lsFinding{f}
}

// ---------------------------------------------------------------- the kill

// lsFindingVKKilled — the process was killed, and only systemd says so.
//
// Verified: SIGKILLing valkey-server produced not one line from Valkey itself. The evidence
// is entirely in systemd's half of the same journal, which is why the collector appends it.
func lsFindingVKKilled(b *lsBundle) []lsFinding {
	ev := lsPick(b, func(e lsEvent) bool {
		return e.Subsys == lsSubsysVKSysd && e.Class == lsClassCrash && e.Sev == lsSevBad
	})
	if len(ev) == 0 {
		return nil
	}
	var lines []string
	seen := map[int]bool{}
	for _, e := range ev {
		if seen[e.Src] {
			continue
		}
		seen[e.Src] = true
		lines = append(lines, fmt.Sprintf("%s at %s", lsNode(b, e.Src), lsClock(e.TS)))
	}
	// A restart loop is a different problem from one kill, and the counter says which.
	restarts := lsPick(b, func(e lsEvent) bool { return e.Label == "systemd: restarting the server" })
	f := lsFinding{
		ID: "vk-killed", Sev: lsSevBad,
		Title:  "A server was killed, not stopped",
		Detail: strings.Join(lines, "; ") + " — systemd recorded the process being terminated by a signal. Valkey itself wrote nothing about it: a SIGKILLed valkey-server has no chance to, so the Valkey half of this log is simply a log that stops and then starts again.",
		Advice: "What killed it is not in either half of this file. The OOM killer leaves its record in dmesg, an orchestrator in its own events, and a person in their shell history. Check the memory the node was using against its limit first — a Valkey that forks for a snapshot needs headroom well beyond its dataset, and that fork is the most common moment for the kernel to choose it.",
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 6),
	}
	if len(restarts) > 1 {
		f.Title = "A server was killed repeatedly — systemd is restarting it in a loop"
		f.Detail += fmt.Sprintf(" systemd restarted it %d times in this window, so what looks like a series of ordinary starts in the Valkey log is one process dying over and over.", len(restarts))
		f.Events = append(f.Events, lsEventNos(restarts, 4)...)
	}
	return []lsFinding{f}
}

// ---------------------------------------------------------------- failover

// lsFindingVKFailover — a shard changed primary, and whether anybody asked for it.
//
// The distinction is not cosmetic here, it is the difference between losing writes and not.
// A manual failover pauses the primary, waits for the replica to process the entire stream,
// and only then swaps — Valkey logs "All primary replication stream processed" as the proof.
// An automatic failover does none of that: replication is asynchronous and the election does
// not wait, so everything the old primary had accepted and not yet sent is gone.
func lsFindingVKFailover(b *lsBundle) []lsFinding {
	// Election wins only. A hand promotion — REPLICAOF NO ONE on a standalone pair — used to
	// be in this list and should not have been: it made this finding fire on a bundle with no
	// cluster in it and describe the result as "a shard changed primary", which is Valkey
	// Cluster's word for something a standalone pair does not have. lsFindingVKSplitPromotion
	// owns that case and says something true about it instead. A MANUAL cluster failover is
	// still covered, because CLUSTER FAILOVER also ends in an election win — verified in the
	// corpus, where the requested handover logs "Failover election won" ten milliseconds
	// after "Manual failover user request accepted".
	won := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Election won — this node is the new primary"
	})
	if len(won) == 0 {
		return nil
	}
	manual := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Manual failover requested" ||
			e.Label == "Caught up — the manual failover can proceed"
	})
	var lines []string
	planned, unplanned := 0, 0
	last := -1e9
	for _, e := range won {
		// One promotion, not one record per member that noticed it.
		if e.TS-last < 5 {
			continue
		}
		last = e.TS
		line := fmt.Sprintf("%s at %s", lsNode(b, e.Src), lsClock(e.TS))
		if e.Seqno > 0 {
			line += fmt.Sprintf(" (epoch %d)", e.Seqno)
		}
		if lsVKAskedFor(manual, e.TS) {
			planned++
			line += " — requested"
		} else {
			unplanned++
		}
		lines = append(lines, line)
	}
	sev, title := lsSevBad, "A shard changed primary"
	detail := "At least one of these was not asked for: a primary went away and a replica was voted into its place."
	loss := " An automatic failover discards whatever the old primary had accepted and not yet replicated — Valkey replication is asynchronous and the election does not wait for it, so those writes were acknowledged to clients and no longer exist."
	switch {
	case unplanned == 0:
		sev, title = lsSevWarn, "A shard changed primary, on request"
		detail = "Every one of these was asked for."
		loss = " A manual failover pauses the primary until the replica has processed the entire stream before swapping, so nothing is lost — but writes do stop for the length of it."
	case planned > 0:
		title = fmt.Sprintf("The cluster changed primary %d times, %d of them unplanned", planned+unplanned, unplanned)
	}
	return []lsFinding{{
		ID: "vk-failover", Sev: sev, Title: title,
		Detail: strings.Join(lines, "; ") + ". " + detail + loss,
		Advice: "The write outage for that shard runs from the moment the old primary stopped answering to the election being won — which on this timeline is the gap between the first 'possibly failing' and the promotion, and is bounded below by cluster-node-timeout. Lowering that shortens the outage and makes spurious failovers more likely; there is no setting that avoids the trade.",
		At:     won[0].TS, Sources: lsSrcSet(won), Events: lsEventNos(won, 8),
	}}
}

// lsVKAskedFor reports whether a manual failover was requested close enough before a
// promotion to be the reason for it. The request, the catch-up and the promotion are three
// records seconds apart on two different nodes.
func lsVKAskedFor(manual []lsEvent, ts float64) bool {
	for _, m := range manual {
		if d := ts - m.TS; d >= -5 && d <= 60 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- the discarded writes

// lsFindingVKDemoted — a primary came back and was told to follow the node that replaced it.
//
// Valkey Cluster's rollback, and it is worse than MongoDB's in one specific way: MongoDB
// writes the discarded documents to a rollback file, so what was lost can at least be
// looked at. Valkey writes nothing. The node simply reloads from its new primary and the
// writes are gone with no record of what they were.
func lsFindingVKDemoted(b *lsBundle) []lsFinding {
	ev := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Demoted — reconfigured as a replica of the node that replaced it"
	})
	if len(ev) == 0 {
		return nil
	}
	var lines []string
	seen := map[int]bool{}
	for _, e := range ev {
		if seen[e.Src] {
			continue
		}
		seen[e.Src] = true
		who := lsNode(b, e.Src)
		if p := lsVKPeerName(b, e.Peer); p != "" {
			who += " now follows " + p
		}
		lines = append(lines, fmt.Sprintf("%s (at %s)", who, lsClock(e.TS)))
	}
	return []lsFinding{{
		ID: "vk-demoted", Sev: lsSevBad,
		Title:  "A returning primary had its writes discarded",
		Detail: strings.Join(lines, "; ") + ". This node was the primary of its shard, the cluster promoted a replica while it was unreachable, and on coming back it was told to follow that replica. Anything it accepted between losing contact and stopping is discarded when it resynchronises — those writes were acknowledged to clients and no longer exist anywhere.",
		Advice: "Valkey keeps no record of what was thrown away — unlike MongoDB, which writes a rollback file you can at least read. The only way to know whether it mattered is from the application side: what was written to that shard between the moment it lost contact with the cluster and the moment it stopped. If that window must never lose writes, the answer is not a Valkey setting; it is an application that does not treat an acknowledged write here as durable.",
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 6),
	}}
}

// ---------------------------------------------------------------- no takeover

// lsFindingVKNoTakeover — a shard died and nothing could replace it.
//
// The all-primary cluster's failure mode, and the reason dbcanvas's own three-node Valkey
// Cluster frame is worth reading these logs for: with --cluster-replicas 0 there is nothing
// to promote, so a single node stopping takes the entire cluster out until it comes back.
// Verified: exactly that, for thirty seconds, on a healthy three-node cluster.
func lsFindingVKNoTakeover(b *lsBundle) []lsFinding {
	if !lsHasValkeyCluster(b) {
		return nil
	}
	uncovered := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Slots uncovered — the cluster is refusing clients" && lsVKClusterWasUp(b, e.TS)
	})
	if len(uncovered) == 0 {
		return nil
	}
	// Did anybody stand for election in that window? If a replica tried, this is an ordinary
	// failover that took a moment, and lsFindingVKFailover already covers it properly.
	elected := lsPick(b, func(e lsEvent) bool {
		return e.Class == lsClassMember && (strings.Contains(e.Label, "election") ||
			e.Label == "Took over the shard")
	})
	for _, e := range elected {
		for _, u := range uncovered {
			if d := e.TS - u.TS; d > -30 && d < 120 {
				return nil
			}
		}
	}
	failedNode := ""
	for _, e := range lsPick(b, func(e lsEvent) bool { return e.Label == "A node was declared FAILED (quorum reached)" }) {
		if d := e.TS - uncovered[0].TS; d > -60 && d < 60 {
			failedNode = lsVKPeerName(b, e.Peer)
			break
		}
	}
	detail := "A shard's slots had no reachable node to serve them and no replica stood for election, so nothing took over."
	if failedNode != "" {
		detail = fmt.Sprintf("%s was declared failed and no replica stood for election in its place, so its slots had nobody to serve them.", failedNode)
	}
	return []lsFinding{{
		ID: "vk-no-takeover", Sev: lsSevBad,
		Title:  "A shard failed and nothing took over",
		Detail: detail + " A Valkey Cluster promotes only a replica of the failed primary; with no replica in that shard there is nothing to promote and the slots stay uncovered until the original node comes back. Every other node in the cluster is healthy and refusing clients for the whole of it.",
		Advice: "An all-primary cluster has no redundancy of any kind — it shards, it does not replicate, and one node stopping is a cluster-wide outage for as long as it takes to restart. If this cluster is expected to survive a node failing, every shard needs at least one replica. If it is not, the thing to fix is the expectation: a three-node all-primary cluster is a third of the availability of a single server, not three times it, because any one of the three failing takes the whole thing down.",
		At:     uncovered[0].TS, Sources: lsSrcSet(uncovered), Events: lsEventNos(uncovered, 6),
	}}
}

// lsVKClusterWasUp reports whether the cluster had ever been healthy before this instant.
//
// The discriminator the corpus produced, and it is exact. "Cluster is currently down: at
// least one hash slot is not served" is written during a cluster's first seconds too, before
// anybody has met anybody — every node writes it while the cluster is being created, and
// treating that as an outage would report every healthy deployment as broken on the day it
// was built. What is NEVER written during formation is "Cluster state changed: fail": a
// cluster that has never been ok cannot change to fail. So the presence of an earlier
// "changed: ok" is what separates a real outage from a cluster still coming up.
func lsVKClusterWasUp(b *lsBundle, t float64) bool {
	for _, e := range b.Events {
		if e.Label == "Cluster state OK — serving again" && e.TS < t {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- persistence

// lsFindingVKPersistence — the failure that stops writes and does not say so.
func lsFindingVKPersistence(b *lsBundle) []lsFinding {
	ev := lsPick(b, func(e lsEvent) bool {
		return e.Class == lsClassStorage && e.Sev == lsSevBad
	})
	if len(ev) == 0 {
		return nil
	}
	why := ""
	for _, e := range ev {
		if e.Label == "Could not write the snapshot file" {
			why = " The child process reported: " + e.Message
			break
		}
	}
	// Did it recover? A save that succeeded afterwards is what lifts the MISCONF, and saying
	// so matters — the finding otherwise reads as an outage that is still running.
	last := lsEventEnd(ev[len(ev)-1])
	recovered := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Snapshot complete" && e.TS > last
	})
	f := lsFinding{
		ID: "vk-persistence-failed", Sev: lsSevBad,
		Title:  "A background save failed — writes were being refused",
		Detail: lsNode(b, ev[0].Src) + " could not write its snapshot." + why + " With stop-writes-on-bgsave-error at its default of yes, the server refuses every write with MISCONF from that moment until a save succeeds — and it does not log that it is doing so. Nothing further appears in this file; the refusal is visible only to clients.",
		Advice: "Look at the path in the child's message: a full filesystem and a permission problem look identical from the application's side, and both present as a server that answers reads perfectly and refuses every write. If the cause was memory rather than disk, the vm.overcommit_memory warning at the top of this log is the explanation — a fork for a snapshot needs the kernel to promise memory it will probably not use.",
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 6),
	}
	if len(recovered) > 0 {
		f.Sev = lsSevWarn
		f.Title = "A background save failed and later succeeded"
		f.Until = recovered[0].TS
		f.Detail += fmt.Sprintf(" A later save succeeded at %s, which is what lifted the refusal — so writes were refused for about %s.",
			lsClock(recovered[0].TS), lsDur(recovered[0].TS-ev[0].TS))
	}
	return []lsFinding{f}
}

// ---------------------------------------------------------------- full resync

// lsFindingVKFullResync — a whole dataset copied where a partial resync would have done.
//
// Valkey's equivalent of Galera's SST-instead-of-IST, and it has the same cause and the same
// fix: a buffer that was too small for how long the replica was away.
func lsFindingVKFullResync(b *lsBundle) []lsFinding {
	refused := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Partial resync REFUSED — a full copy follows"
	})
	full := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Full resync — the whole dataset is being copied"
	})
	if len(full) == 0 {
		return nil
	}
	// A full sync of a replica that has never followed anybody is how replication STARTS.
	// Reporting that as a problem would flag every cluster on the day it was built — the same
	// mistake the PostgreSQL catalogue made with Patroni's bootstrap, caught the same way.
	firstTime := lsPick(b, func(e lsEvent) bool {
		return e.Label == "No cached primary — a full sync is the only option"
	})
	avoidable := 0
	for _, e := range full {
		bootstrap := false
		for _, ft := range firstTime {
			if d := e.TS - ft.TS; d >= -5 && d <= 30 {
				bootstrap = true
			}
		}
		if !bootstrap {
			avoidable++
		}
	}
	if avoidable == 0 {
		return nil
	}
	reason := ""
	for _, e := range refused {
		if strings.Contains(e.Message, "Replication ID mismatch") {
			reason = " The primary refused the partial resync because the replication IDs did not match — this replica had been following a different primary, or the primary it asked had been restarted and reset its own history. No backlog size would have helped."
			break
		}
		reason = " The primary refused the partial resync, so the replica's offset was outside what the backlog still held."
	}
	backlogFreed := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Replication backlog freed — no replicas left"
	})
	if len(backlogFreed) > 0 {
		reason += " The primary had also freed its backlog entirely after repl-backlog-ttl elapsed with no replicas connected, which guarantees a full resync for anything that comes back afterwards."
	}
	return []lsFinding{{
		ID: "vk-full-resync", Sev: lsSevWarn,
		Title:  fmt.Sprintf("A full dataset copy ran %d time(s) where a partial resync would have done", avoidable),
		Detail: "A replica reconnected and had to be sent the entire dataset instead of just the stretch it missed." + reason + " The primary forks and serialises everything it holds for each of these, and the replica throws away what it had and reloads from scratch.",
		Advice: "repl-backlog-size decides how long a replica can be away and still resume cheaply — it is a ring buffer of the write stream, so size it against your write rate multiplied by the longest blip you expect, not against the dataset. repl-backlog-ttl decides how long the primary keeps it with no replicas attached, and its default of an hour is what turns a long outage into a guaranteed full copy.",
		At:     full[0].TS, Sources: lsSrcSet(full), Events: lsEventNos(full, 6),
	}}
}

// ---------------------------------------------------------------- two primaries

// lsFindingVKSplitPromotion — somebody promoted a node by hand and nothing coordinated it.
//
// Standalone Valkey replication has no election, no arbitration and no fencing: REPLICAOF NO
// ONE takes effect immediately and unconditionally. If the old primary is still running and
// reachable, there are now two servers accepting writes for the same dataset and neither of
// them will ever notice.
func lsFindingVKSplitPromotion(b *lsBundle) []lsFinding {
	ev := lsPick(b, func(e lsEvent) bool { return e.Label == "Promoted to primary by hand" })
	if len(ev) == 0 {
		return nil
	}
	// Was the old primary still up at that instant? Only answerable because several logs are
	// read together, which is the whole reason this check can exist at all.
	var alsoUp []string
	for _, e := range ev {
		for _, s := range b.Sources {
			if s.Idx == e.Src || !lsIsValkey(s.Flavour) {
				continue
			}
			p, ok := lsStateAt(b.Phases, s.Idx, e.TS)
			if ok && (p.State == lsStatePrimaryM || p.State == lsStateUp) {
				alsoUp = append(alsoUp, lsNode(b, s.Idx))
			}
		}
	}
	f := lsFinding{
		ID: "vk-manual-promotion", Sev: lsSevWarn,
		Title:  "A node was promoted to primary by hand",
		Detail: lsNode(b, ev[0].Src) + " was told REPLICAOF NO ONE at " + lsClock(ev[0].TS) + ". Standalone Valkey replication has no election and no fencing: the promotion takes effect immediately whatever the rest of the topology is doing, and no other node is told about it.",
		Advice: "Whatever was still pointed at the old primary is still pointed at it. Valkey will not redirect and will not complain — the application has to be moved, or a Sentinel deployment has to be doing this instead of a person.",
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 4),
	}
	if len(alsoUp) > 0 {
		sort.Strings(alsoUp)
		f.Sev = lsSevBad
		f.Title = "Two nodes were accepting writes for the same dataset"
		f.Detail += fmt.Sprintf(" And %s was up and accepting writes at that same instant, so for a period there were two primaries for one dataset with no arbitration between them.",
			strings.Join(uniqueStrings(alsoUp), " and "))
		f.Advice = "Writes that went to each primary exist only on that one. There is no merge: whichever node ends up as the replica will discard everything it accepted when it resynchronises. Establish which one the application was actually talking to before pointing anything at the other."
	}
	return []lsFinding{f}
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------- the honest notes

// lsFindingVKCleanStopIsInvisible — the thing the peers' logs cannot tell you.
//
// Galera distinguishes a member that left cleanly from one that was lost, and the Log Summary
// makes a finding of each (lsFindingCleanStop, lsFindingPartition). Valkey Cluster cannot:
// verified by stopping a node with systemctl, which produced on its peers exactly the same
// "Marking node ... as failing (quorum reached)" that a SIGKILL produces, seven seconds
// later, with nothing to distinguish them.
// lsVKDepartLead is how far back a declared failure may look for the departing node's own
// account of why it went.
//
// cluster-node-timeout is what stands between a node stopping and the cluster agreeing it is
// gone, and it defaults to 15 s. The corpus, at 5 s, took 6.3 s from the SIGTERM to the
// quorum-reached record. Sixty seconds covers a default-configured cluster comfortably
// without reaching back into an unrelated earlier restart.
const lsVKDepartLead = 60.0

func lsFindingVKCleanStopIsInvisible(b *lsBundle) []lsFinding {
	if !lsHasValkeyCluster(b) {
		return nil
	}
	failing := lsPick(b, func(e lsEvent) bool {
		return e.Label == "A node was declared FAILED (quorum reached)"
	})
	if len(failing) == 0 {
		return nil
	}
	// Is the departing node's own log in the bundle? If it is, the question is answerable
	// after all, and the finding says which answer rather than that there is none.
	//
	// Near the departure, not anywhere in the window. A node can be killed early in a bundle,
	// restarted within the second so that nobody notices, and then stopped cleanly much later
	// — which is exactly what the v02 corpus contains. Searching the whole window found the
	// earlier kill and attributed it to a departure eighty seconds afterwards that was in
	// fact a deliberate stop.
	depart := failing[0].TS
	near := func(e lsEvent) bool { return e.TS >= depart-lsVKDepartLead && e.TS <= depart+5 }
	stopped := lsPick(b, func(e lsEvent) bool { return e.Label == "Shutdown requested" && near(e) })
	killed := lsPick(b, func(e lsEvent) bool {
		return e.Subsys == lsSubsysVKSysd && e.Class == lsClassCrash && near(e)
	})
	f := lsFinding{
		ID: "vk-departure-unattributable", Sev: lsSevInfo,
		Title:  "A clean stop and a crash look identical from the other nodes",
		Detail: "The surviving nodes recorded a peer being declared failed after cluster-node-timeout. That record is the same whether the node was shut down deliberately or killed outright: Valkey Cluster has no goodbye message, so a member that stops on request simply stops answering, exactly like one that died.",
		Advice: "The answer is only ever in the departed node's OWN log, and only if it is in this bundle: 'Received SIGTERM scheduling shutdown' means it was asked to stop, and systemd's 'code=killed, status=9/KILL' means it was not. Collect every member's log, not just the survivors' — the survivors cannot tell you.",
		At:     failing[0].TS, Sources: lsSrcSet(failing), Events: lsEventNos(failing, 4),
	}
	switch {
	case len(killed) > 0:
		f.Title = "The departed node was killed — its own log is what says so"
		f.Detail += " In this bundle the departing node's own log IS present, and it says it was killed rather than stopped."
	case len(stopped) > 0:
		f.Title = "The departed node was stopped deliberately — its own log is what says so"
		f.Detail += " In this bundle the departing node's own log IS present, and it recorded a shutdown request — so this departure was planned, which no survivor's log could have told you."
	}
	return []lsFinding{f}
}

// lsFindingVKInvisible — the honest note, and the largest of the six engines'.
//
// Each of the three claims below was measured against a live server rather than assumed:
//
//	evictions   40,000 writes against an 8 MB maxmemory evicted 19,156 keys and produced
//	            ZERO log records of any kind.
//	MISCONF     a real failed background save left the server refusing every write, and the
//	            string "MISCONF" appears nowhere in its log — only the client sees it.
//	auth        three failed authentications produced zero records.
func lsFindingVKInvisible(b *lsBundle) []lsFinding {
	if !lsHasValkey(b) {
		return nil
	}
	detail := "Three things a Valkey server does are entirely absent from its log, and each was measured rather than assumed. Evicting keys under maxmemory: 40,000 writes against an 8 MB limit evicted 19,156 keys and wrote nothing at all. Refusing writes after a failed snapshot: the server answers every write with MISCONF and the word appears nowhere in the log — only the client is told. Rejecting an authentication: a wrong password produces no record whatsoever."
	advice := "All three live in the server rather than the log. INFO stats has evicted_keys and expired_keys; INFO persistence has rdb_last_bgsave_status and aof_last_write_status, either of which being 'err' is the MISCONF state; INFO clients has the connection counts. For replication lag there is no log line either — master_repl_offset on the primary against slave_repl_offset on each replica is the only measure, and the difference between them is the data that would be lost if the primary went now."

	f := lsFinding{
		ID: "vk-invisible", Sev: lsSevInfo,
		Title: "Evictions, MISCONF refusals and failed logins are not in this log",
	}
	// If the bundle contains a persistence failure, the MISCONF half stops being a general
	// caveat and becomes a specific warning about this server, right now.
	if len(lsPick(b, func(e lsEvent) bool { return e.Class == lsClassStorage && e.Sev == lsSevBad })) > 0 {
		f.Sev = lsSevWarn
		f.Title = "This server was refusing writes and its log does not say so"
		detail = "There is a failed background save in this bundle, and that is the whole of what the log will tell you: with stop-writes-on-bgsave-error at its default the server was refusing every write with MISCONF, and no record of that refusal is ever written. " + detail
	}
	f.Detail, f.Advice = detail, advice
	return []lsFinding{f}
}
