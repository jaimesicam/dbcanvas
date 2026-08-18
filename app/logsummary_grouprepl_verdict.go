package main

// logsummary_grouprepl_verdict.go — what a Group Replication bundle adds up to.
//
// The catalogue next door reads one record at a time. These read the whole bundle, which
// is what the interesting Group Replication conclusions require: every one of them is two
// records held apart, or a record whose meaning is decided by something that is NOT in the
// file afterwards.
//
// Three of them are built on absences, and that is deliberate rather than clever. GR's
// worst states are the quiet ones — a member blocked on a lost majority says so once and
// then goes silent for as long as it stays broken; a member that never rejoined says
// nothing at all. A page that only reported what the log says would show those as calm.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// lsGRFindings are the Group Replication checks, appended to lsFindings' list.
var lsGRFindings = []func(*lsBundle) []lsFinding{
	lsFindingGRSplit,
	lsFindingGRSplitBrain,
	lsFindingGRRefused,
	lsFindingGRNotRejoined,
	lsFindingGRElection,
	lsFindingGRClone,
	lsFindingGRStuckRecovery,
	lsFindingGRShellProbe,
	lsFindingGRFlowControl,
}

// lsHasGroupRepl reports whether any source in the bundle is a Group Replication member.
// Every check here returns nothing for a bundle without one, so a PXC or standalone
// summary is unchanged by this file.
func lsHasGroupRepl(b *lsBundle) bool {
	for _, s := range b.Sources {
		if s.Flavour == lsFlavourGroupRepl {
			return true
		}
	}
	return false
}

// lsFindingGRSplit — members that lost their majority, and whether they ever got it back.
//
// The most consequential reading in this file, and the one that depends on an absence.
// MY-011495 says the member has blocked every write; only MY-011498 says it stopped. The
// capture that produced the rule (g06-partition-nomajority) is a 1v1 split where BOTH
// members logged MY-011495 — neither side had a majority, so the whole cluster refused
// writes while every member was up and answering reads perfectly happily.
//
// Restoring the network did not end it and did not log anything. The block was still in
// force two and a half minutes later when an operator intervened, so a block with no
// matching MY-011498 after it is reported as still in force at the end of the window,
// rather than as something that presumably sorted itself out.
func lsFindingGRSplit(b *lsBundle) []lsFinding {
	if !lsHasGroupRepl(b) {
		return nil
	}
	blocked := lsPick(b, func(e lsEvent) bool { return e.Code == "MY-011495" })
	if len(blocked) == 0 {
		return nil
	}
	resumed := lsPick(b, func(e lsEvent) bool { return e.Code == "MY-011498" })

	// Per source: the first block, and the first resumption after it.
	type span struct {
		src         int
		from, until float64
		ended       bool
	}
	var spans []span
	seen := map[int]bool{}
	for _, e := range blocked {
		if seen[e.Src] {
			continue
		}
		seen[e.Src] = true
		s := span{src: e.Src, from: e.TS}
		for _, r := range resumed {
			if r.Src == e.Src && r.TS > e.TS {
				s.until, s.ended = r.TS, true
				break
			}
		}
		spans = append(spans, s)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].from < spans[j].from })

	var stillBlocked, recovered []string
	for _, s := range spans {
		if s.ended {
			recovered = append(recovered, fmt.Sprintf("%s (blocked %s)", lsNode(b, s.src), lsDur(s.until-s.from)))
		} else {
			stillBlocked = append(stillBlocked, fmt.Sprintf("%s (from %s to the end of the log, %s)",
				lsNode(b, s.src), lsClock(s.from), lsDur(b.Summary.LastTS-s.from)))
		}
	}

	f := lsFinding{
		ID: "gr-no-majority", Sev: lsSevBad, At: spans[0].from,
		Sources: lsSrcSet(blocked), Events: lsEventNos(blocked, 6),
	}
	// Every member blocking at once is a different sentence from one member blocking:
	// the first means the cluster took no writes at all, the second means one member was
	// on the wrong side of a split that the rest survived.
	whole := len(seen) == len(b.Sources) && len(b.Sources) > 1
	switch {
	case len(stillBlocked) > 0 && whole:
		f.Title = "Every member lost its majority — the cluster stopped accepting writes"
		f.Detail = fmt.Sprintf("All %d members reported that they could not reach a majority and blocked updates. No member in this bundle ever reported regaining contact, so as far as these logs go the cluster is still refusing writes: %s. Reads kept working throughout, which is why an application can look alive while nothing it writes is being kept.",
			len(b.Sources), strings.Join(stillBlocked, "; "))
	case len(stillBlocked) > 0:
		f.Title = "A member lost its majority and never regained it in this log"
		f.Detail = fmt.Sprintf("%s blocked every update and this log never records it recovering. A block ends only with 'has resumed contact with a majority' — nothing else, and nothing implicitly. In the capture behind this check, restoring the network did NOT end the block and wrote nothing to either log; it took an operator.",
			strings.Join(stillBlocked, "; "))
		if len(recovered) > 0 {
			f.Detail += " Recovered elsewhere: " + strings.Join(recovered, ", ") + "."
		}
	default:
		f.Sev = lsSevWarn
		f.Title = "A member lost its majority and got it back"
		f.Detail = fmt.Sprintf("Writes were refused while it lasted: %s.", strings.Join(recovered, ", "))
		if len(spans) > 0 && spans[0].ended {
			f.Until = spans[0].until
		}
	}
	f.Advice = "Members that cannot see each other but are all running is a network problem, not a database one — check the group-communication port (33061 by default) between exactly the members that named each other. A group that stays blocked has to be restarted into a majority by hand: group_replication_force_members on the side you choose to keep, or STOP/START GROUP_REPLICATION with one member bootstrapping. Whichever side you drop, its writes are gone."
	return []lsFinding{f}
}

// lsFindingGRSplitBrain — a member that left the group and was left WRITABLE.
//
// The most dangerous thing this package looks for, and it exists because the corpus caught
// it happening. Group Replication has two exits and they end in opposite places:
//
//	applier failure   → MY-011712, "set into read only mode"       — safe
//	refused at join   → MY-011522, then `Setting super_read_only=OFF` — writable
//
// A member out of the second exit is up, accepting writes, and holding data the cluster
// does not have. In the capture the load generator did exactly what a connection pool
// does — reconnected to the first member that answered — and wrote 1,263 rows into a
// server that was no longer part of the cluster.
func lsFindingGRSplitBrain(b *lsBundle) []lsFinding {
	if !lsHasGroupRepl(b) {
		return nil
	}
	left := lsPick(b, func(e lsEvent) bool {
		return e.Code == "MY-011504" || e.Code == "MY-011522" || e.Code == "MY-011651"
	})
	if len(left) == 0 {
		return nil
	}
	writable := lsPick(b, func(e lsEvent) bool { return e.Code == "MY-011566" })

	var hits []lsEvent
	var lines []string
	done := map[int]bool{}
	for _, l := range left {
		for _, w := range writable {
			// The same member, writable within a minute of leaving, and not because it
			// was elected primary — an election writes MY-011507/MY-011510 alongside.
			if w.Src != l.Src || w.TS < l.TS || w.TS-l.TS > 60 {
				continue
			}
			if lsGRElectedNear(b, w.Src, w.TS) {
				continue
			}
			if done[w.Src] {
				break // one sentence per member: a member that leaves repeatedly is one problem
			}
			done[w.Src] = true
			hits = append(hits, w)
			lines = append(lines, fmt.Sprintf("%s at %s, %s after it left", lsNode(b, w.Src), lsClock(w.TS), lsDur(w.TS-l.TS)))
			break
		}
	}
	if len(hits) == 0 {
		return nil
	}
	return []lsFinding{{
		ID: "gr-writable-outside-group", Sev: lsSevBad, At: hits[0].TS,
		Title:   "A member left the group and went back to accepting writes",
		Detail:  fmt.Sprintf("%s. A member outside the group with super_read_only OFF is a second, silent copy of the database: it answers connections, it accepts writes, and nothing it is told will ever reach the cluster. Anything with a connection pool — an application, a proxy, a load generator — will find it and use it, because it looks perfectly healthy from the outside.", strings.Join(lines, "; ")),
		Advice:  "Take it out of service before anything writes to it, then work out what it holds that the group does not (compare gtid_executed against the group's). Group Replication only sets a leaving member read-only when the applier failed; a member refused at join for having extra transactions is left writable, which is the case worth guarding against. group_replication_exit_state_action=OFFLINE_MODE (or ABORT_SERVER) makes the server refuse ordinary connections instead of quietly serving them.",
		Sources: lsSrcSet(hits), Events: lsEventNos(hits, 4),
	}}
}

// lsGRElectedNear reports whether a member was made primary within a few seconds of t,
// which is the legitimate reason for super_read_only to go OFF.
func lsGRElectedNear(b *lsBundle, src int, t float64) bool {
	for _, e := range b.Events {
		if e.Src != src || (e.Code != "MY-011510" && e.Code != "MY-011507") {
			continue
		}
		if e.TS >= t-5 && e.TS <= t+5 {
			return true
		}
	}
	return false
}

// lsFindingGRRefused — a member the group would not take back, and why.
func lsFindingGRRefused(b *lsBundle) []lsFinding {
	if !lsHasGroupRepl(b) {
		return nil
	}
	diverged := lsPick(b, func(e lsEvent) bool { return e.Code == "MY-011526" })
	timedOut := lsPick(b, func(e lsEvent) bool { return e.Code == "MY-011640" })
	if len(diverged) == 0 && len(timedOut) == 0 {
		return nil
	}
	var out []lsFinding
	if len(diverged) > 0 {
		var who []string
		for _, e := range diverged {
			who = append(who, fmt.Sprintf("%s at %s — %s", lsNode(b, e.Src), lsClock(e.TS), e.Message))
		}
		out = append(out, lsFinding{
			ID: "gr-diverged", Sev: lsSevBad, At: diverged[0].TS,
			Title:   "A member has transactions the group does not, and was refused",
			Detail:  fmt.Sprintf("%s. Group Replication compares GTID sets before admitting a member and will not accept one holding transactions the group never saw — accepting it would mean the cluster silently disagreeing with itself. The usual cause is a primary that died with writes committed locally but not yet certified, or something writing to a member while it was outside the group.", strings.Join(who, "; ")),
			Advice:  "Decide what those extra transactions were worth before doing anything else — rejoining the member destroys them. If they matter, extract them from its binary log first. To rejoin: clear the member's GTID state and let distributed recovery refill it, which for a member this far out means a clone (group_replication_clone_threshold), not an incremental. Note that raising the clone threshold does not help by itself: the GTID comparison happens first and rejects the member before a recovery method is ever chosen.",
			Sources: lsSrcSet(diverged), Events: lsEventNos(diverged, 4),
		})
	}
	if len(timedOut) > 0 {
		out = append(out, lsFinding{
			ID: "gr-join-timeout", Sev: lsSevWarn, At: timedOut[0].TS,
			Title:   fmt.Sprintf("%s reached the group but never got a view", lsNode(b, timedOut[0].Src)),
			Detail:  "The member joined the group communication layer and then timed out waiting to be told the membership, so it gave up. In the capture behind this check the group still listed this member as UNREACHABLE from its previous life, and would not admit a second incarnation of it while that entry stood.",
			Advice:  "Check what the OTHER members think the membership is. If they still list this host as UNREACHABLE, that stale entry has to go first — it clears on expulsion, or you clear it by hand — before the new process can join.",
			Sources: lsSrcSet(timedOut), Events: lsEventNos(timedOut, 4),
		})
	}
	return out
}

// lsFindingGRNotRejoined — a server that came back up and never rejoined its group.
//
// Built entirely on an absence, and worth the risk that carries. In g04-crash-kill9 systemd
// restarted a SIGKILLed member; mysqld came up, logged `ready for connections`, and that
// was the last thing it ever said. group_replication_start_on_boot was OFF, so nothing
// rejoined. Measured at that instant the server was writable and 666 transactions behind.
//
// Nothing in the log marks this state — that IS the state — so the check looks for a
// start-up with no plugin start after it, in a source that was a group member earlier.
func lsFindingGRNotRejoined(b *lsBundle) []lsFinding {
	if !lsHasGroupRepl(b) {
		return nil
	}
	var hits []lsEvent
	var lines []string
	for _, s := range b.Sources {
		if s.Flavour != lsFlavourGroupRepl {
			continue
		}
		var lastUp lsEvent
		found := false
		for _, e := range b.Events {
			if e.Src != s.Idx {
				continue
			}
			if strings.HasPrefix(e.Label, "Server ready for connections") {
				lastUp, found = e, true
			}
		}
		if !found {
			continue
		}
		rejoined := false
		for _, e := range b.Events {
			if e.Src == s.Idx && e.TS >= lastUp.TS && (e.Code == "MY-013587" || e.Code == "MY-011490" || e.Code == "MY-014010") {
				rejoined = true
				break
			}
		}
		if rejoined {
			continue
		}
		hits = append(hits, lastUp)
		lines = append(lines, fmt.Sprintf("%s came up at %s and was still not in the group %s later, at the end of its log",
			lsNode(b, s.Idx), lsClock(lastUp.TS), lsDur(b.Summary.LastTS-lastUp.TS)))
	}
	if len(hits) == 0 {
		return nil
	}
	return []lsFinding{{
		ID: "gr-never-rejoined", Sev: lsSevBad, At: hits[0].TS,
		Title:   "A server restarted and never rejoined its group",
		Detail:  fmt.Sprintf("%s. Group Replication does not start itself unless group_replication_start_on_boot is ON, and this server's log carries no 'Plugin group_replication is starting' after it came up. It is not a member: it answers connections, it serves whatever data it had when it died, and it falls further behind every second. Nothing further in its own log will mention any of this — a monitor that checks whether mysqld is up sees a healthy server.", strings.Join(lines, "; ")),
		Advice:  "START GROUP_REPLICATION on it, and expect the join to be refused if it died as the primary with writes in flight. To stop it happening again set group_replication_start_on_boot=ON — that is exactly what MySQL Shell does when it builds an InnoDB Cluster, and it is why the same kill that strands a raw Group Replication member leaves a Shell-managed one to rejoin on its own.",
		Sources: lsSrcSet(hits), Events: lsEventNos(hits, 4),
	}}
}

// lsFindingGRElection — primary elections, and the write outage each one cost.
//
// The measurement is the point. An election is not instantaneous and the log brackets it
// precisely: the primary goes unreachable, and one expel timeout later the group elects a
// replacement. Killing the primary under load measured 16.0s between those two records.
func lsFindingGRElection(b *lsBundle) []lsFinding {
	if !lsHasGroupRepl(b) {
		return nil
	}
	elected := lsPick(b, func(e lsEvent) bool { return e.Code == "MY-011507" })
	if len(elected) == 0 {
		return nil
	}
	// One election is reported once, however many members witnessed it: they all log it,
	// within microseconds, and three copies of the same sentence is not three elections.
	type ev struct {
		at    float64
		who   string
		lost  string
		gap   float64
		event lsEvent
	}
	var evs []ev
	for _, e := range elected {
		dup := false
		for _, x := range evs {
			if x.who == e.Peer && e.TS-x.at < 5 {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		x := ev{at: e.TS, who: e.Peer, event: e}
		// Who left is stated outright by MY-011500, and that is the only trustworthy
		// source for it: matching the nearest 'unreachable' record instead produced
		// "gr02 took over from gr02" against the live cluster, because an unrelated peer
		// had gone quiet closer in time than the primary did.
		for _, o := range b.Events {
			if o.Code == "MY-011500" && o.TS <= e.TS && e.TS-o.TS < 60 && o.Peer != "" {
				x.lost = o.Peer
			}
		}
		// The outage began when the old primary stopped answering, not when the group
		// finished agreeing about it — so the unreachable record has to be about THAT
		// member, and recent enough to belong to the same incident.
		//
		// The EARLIEST such record, not the nearest. Every surviving member logs its own,
		// up to a second apart, and the writers were already failing by the time the first
		// one noticed. Taking the nearest reported the live failover as a 15.0s outage
		// where the first member had seen the primary go quiet 16.0s before the election.
		if x.lost != "" {
			for _, u := range b.Events {
				if u.Code != "MY-011493" || u.Peer != x.lost || u.TS > e.TS || e.TS-u.TS > 120 {
					continue
				}
				if gap := e.TS - u.TS; gap > x.gap {
					x.gap = gap
				}
			}
		}
		evs = append(evs, x)
	}
	if len(evs) == 0 {
		return nil
	}
	var lines []string
	worst := 0.0
	for _, x := range evs {
		s := fmt.Sprintf("%s became primary at %s", x.who, lsClock(x.at))
		if x.lost != "" {
			s = fmt.Sprintf("%s took over from %s at %s", x.who, x.lost, lsClock(x.at))
		}
		if x.gap > 0 {
			s += fmt.Sprintf(", %s after it stopped answering", lsDur(x.gap))
			if x.gap > worst {
				worst = x.gap
			}
		}
		lines = append(lines, s)
	}
	f := lsFinding{
		ID: "gr-election", Sev: lsSevWarn, At: evs[0].at,
		Title:  fmt.Sprintf("The group elected a new primary %s", lsTimes(len(evs))),
		Detail: strings.Join(lines, "; ") + ".",
		Advice: "Writes fail for the whole of that gap, and most of it is not the election — it is group_replication_member_expel_timeout waiting to be sure the old primary is really gone. Lowering it shortens the outage and makes a brief network hiccup more likely to cost you a member. The new primary also has to finish applying everything the group committed before it accepts a write, so the application's outage ends slightly after the election does.",
	}
	if worst > 0 {
		f.Detail += fmt.Sprintf(" The longest gap between the primary going quiet and a replacement being elected was %s.", lsDur(worst))
	}
	// A bootstrap elects a primary too, and that is not an incident.
	if len(evs) == 1 && evs[0].gap == 0 && evs[0].lost == "" {
		f.Sev = lsSevOK
		f.Title = "The group chose its first primary"
		f.Advice = ""
	}
	for _, x := range evs {
		f.Events = append(f.Events, x.event.No)
		f.Sources = append(f.Sources, x.event.Src)
	}
	return []lsFinding{f}
}

// lsFindingGRClone — a clone recovery, and the two things about it that surprise people.
//
// It erases the recipient before it copies anything (MY-013460 says so outright), and it
// restarts mysqld when it finishes. That restart is why this check exists: in the log it
// is a `Shutdown complete` followed by a fresh start-up, which is indistinguishable from an
// unplanned restart unless you connect it to the clone that caused it.
func lsFindingGRClone(b *lsBundle) []lsFinding {
	if !lsHasGroupRepl(b) {
		return nil
	}
	clones := lsPick(b, func(e lsEvent) bool {
		return e.Code == "MY-013460" || (e.Code == "MY-013471" && strings.Contains(e.Message, "Cloning"))
	})
	if len(clones) == 0 {
		return nil
	}
	var who []string
	seen := map[int]bool{}
	for _, e := range clones {
		if seen[e.Src] {
			continue
		}
		seen[e.Src] = true
		who = append(who, fmt.Sprintf("%s at %s", lsNode(b, e.Src), lsClock(e.TS)))
	}
	return []lsFinding{{
		ID: "gr-clone", Sev: lsSevWarn, At: clones[0].TS,
		Title:   "A member rebuilt itself from a clone",
		Detail:  fmt.Sprintf("%s. Cloning is distributed recovery's expensive path: it is chosen when the member is missing more than the binary logs can supply. Two consequences are worth reading for explicitly — everything the recipient held was deleted before the copy began, and mysqld restarted itself when the copy finished. That restart appears in this log as a clean shutdown followed by a start-up, and without the clone records above it, it looks exactly like somebody bouncing the server.", strings.Join(who, "; ")),
		Advice:  "If the clone was expected, the only cost is the donor's time and the network. If it was not, the question is why the member fell so far behind that its donor's binary logs no longer covered the gap — usually binlog_expire_logs_seconds set shorter than a member is allowed to be down.",
		Sources: lsSrcSet(clones), Events: lsEventNos(clones, 4),
	}}
}

// lsFindingGRStuckRecovery — a member cycling donors and never coming online.
//
// Recovery failing is not one record but a loop: a donor is chosen, the applier fails on
// it, another donor is chosen, and it fails identically. In g08-divergent-rejoin the member
// tried both available donors three times each and stayed RECOVERING throughout — never
// serving, never giving up, and never saying anything new.
func lsFindingGRStuckRecovery(b *lsBundle) []lsFinding {
	if !lsHasGroupRepl(b) {
		return nil
	}
	fails := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Recovery attempt failed to apply"
	})
	if len(fails) < 2 {
		return nil
	}
	bySrc := map[int][]lsEvent{}
	for _, e := range fails {
		bySrc[e.Src] = append(bySrc[e.Src], e)
	}
	var lines []string
	var hits []lsEvent
	for src, es := range bySrc {
		if len(es) < 2 {
			continue
		}
		online := false
		for _, e := range b.Events {
			if e.Src == src && e.Code == "MY-011490" && e.TS > es[len(es)-1].TS {
				online = true
			}
		}
		if online {
			continue
		}
		donors := map[string]bool{}
		for _, e := range b.Events {
			if e.Src == src && e.Code == "MY-014002" && e.Peer != "" {
				donors[e.Peer] = true
			}
		}
		lines = append(lines, fmt.Sprintf("%s failed %d recovery attempts across %d donor(s) over %s and never came online",
			lsNode(b, src), len(es), len(donors), lsDur(es[len(es)-1].TS-es[0].TS)))
		hits = append(hits, es...)
	}
	if len(lines) == 0 {
		return nil
	}
	sort.Strings(lines)
	return []lsFinding{{
		ID: "gr-stuck-recovery", Sev: lsSevBad, At: hits[0].TS,
		Title:   "A member is stuck recovering and is not serving",
		Detail:  fmt.Sprintf("%s. Distributed recovery cannot skip a transaction it fails to apply, so a member that hits one cycles through the available donors and fails on each in the same place, indefinitely. It counts as a member of the group the whole time — it is simply never usable, and it never reports a final failure.", strings.Join(lines, "; ")),
		Advice:  "Read the applier error itself: a duplicate-key failure during recovery means the member already holds a row the group is trying to give it, which is divergence rather than a transient. Rejoining it means discarding what it has — clear its GTID state and let it clone. Cycling donors is not progress, and no number of retries will change the outcome.",
		Sources: lsSrcSet(hits), Events: lsEventNos(hits, 6),
	}}
}

// lsFindingGRShellProbe — the explanation for a pile of errors that mean nothing.
//
// Deploying a healthy three-node InnoDB Cluster wrote 26 [ERROR] records across the three
// members. Every one was MySQL Shell checking the instance: it opens a throwaway channel
// called mysqlsh.test and deliberately fails to start it, and asks the server to start
// Group Replication before configuring it. Somebody reading their first InnoDB Cluster log
// will find those errors and conclude the deployment failed.
//
// The rules file the mysqlsh.test records as information. This says why, and reaches the
// records the rules cannot safely reclassify — "Replica I/O thread couldn't register on
// source" carries no channel name, so on its own it is indistinguishable from a genuinely
// broken asynchronous replica.
func lsFindingGRShellProbe(b *lsBundle) []lsFinding {
	probes := lsPick(b, func(e lsEvent) bool { return strings.Contains(e.Message, "mysqlsh.test") })
	if len(probes) == 0 {
		return nil
	}
	first, last := probes[0].TS, probes[len(probes)-1].TS
	nearby := 0
	for _, e := range b.Events {
		if e.Code == "MY-010564" && e.TS >= first-60 && e.TS <= last+60 {
			nearby++
		}
	}
	d := fmt.Sprintf("%d records here come from MySQL Shell's instance checks, which run when a server is configured for or added to an InnoDB Cluster. Shell opens a temporary replication channel called 'mysqlsh.test' and tries to start it on purpose, to learn whether the instance can replicate and whether its server id collides with another member's. The attempts are supposed to fail.", len(probes))
	if nearby > 0 {
		d += fmt.Sprintf(" A further %d 'Replica I/O thread couldn't register on source' errors sit in the same window and belong to the same probe — that message carries no channel name, so nothing in the record itself distinguishes it from a real replication failure.", nearby)
	}
	return []lsFinding{{
		ID: "gr-shell-probe", Sev: lsSevInfo, At: first,
		Title:   "Some of these errors are MySQL Shell testing the instance, not failures",
		Detail:  d,
		Advice:  "Check when they happened. Clustered around a deployment or an addInstance, they are noise and the cluster is fine. Appearing on an established cluster that nobody was reconfiguring, they are not — something ran a Shell operation against it.",
		Sources: lsSrcSet(probes), Events: lsEventNos(probes, 4),
	}}
}

// lsFindingGRFlowControl — the honest note, and a more absolute one than Galera's.
//
// Measured, not assumed, and the measurement was pushed as far as the settings allow:
// group_replication_flow_control_mode=QUOTA with both thresholds at 1 — the most eager
// configuration there is — a member slowed to 120 ms RTT with netem, and 1,364
// transactions certified through the flood. All three members wrote nothing at all. Galera
// at least writes its interval once when membership changes; Group Replication does not
// write even that.
func lsFindingGRFlowControl(b *lsBundle) []lsFinding {
	if !lsHasGroupRepl(b) {
		return nil
	}
	return []lsFinding{{
		ID: "gr-flow-control", Sev: lsSevInfo,
		Title:  "Flow control leaves no trace in this log at all",
		Detail: "Group Replication throttles writers when a member falls behind and records none of it. That was measured rather than assumed: with flow control in QUOTA mode, both thresholds set to 1, a member slowed to 120 ms and 1,364 transactions certified through a flood, all three members wrote zero lines. Nothing here can tell you whether the group was throttled, and silence is not evidence that it was not.",
		Advice: "The numbers are in the server. performance_schema.replication_group_member_stats gives COUNT_TRANSACTIONS_IN_QUEUE and COUNT_TRANSACTIONS_REMOTE_IN_APPLIER_QUEUE per member — a queue that stays above the applier threshold is a member being waited for — and replication_group_members gives MEMBER_STATE. Watch those, or the same series in PMM.",
	}}
}

// lsClock names an instant, in UTC, the way the log itself stamps one.
//
// The other findings in this package locate things by duration and by event reference, and
// that is right when the reader can click through. These findings are frequently about a
// state that persists to the end of the window, where "from 16:33:04 to the end of the log"
// is the only phrasing that says what is true — a duration alone would not.
func lsClock(ts float64) string {
	if ts <= 0 {
		return "an unknown time"
	}
	// The date is part of the answer, not decoration. A bundle is several logs read
	// together and they routinely span days — a 200,000-line tail off a quiet server
	// reaches back a week, and two uploaded files can be from different days entirely.
	// "15:04:05" in a finding about a bundle like that names several different moments
	// and leaves the reader to guess which. The month is spelled rather than numbered
	// because 08-09 is September to half the world and August to the other half.
	return time.Unix(int64(ts), 0).UTC().Format("02 Jan 15:04:05")
}

// lsTimes renders a small count as words, because "elected a new primary 1 times" is the
// kind of sentence that makes a page look automated.
func lsTimes(n int) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	default:
		return fmt.Sprintf("%d times", n)
	}
}
