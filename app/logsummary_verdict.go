package main

// logsummary_verdict.go — what the logs add up to.
//
// The event list is the evidence; this is the reading. Everything here is a statement
// that could not be made from one node's log alone, or that requires holding two distant
// records side by side:
//
//	"pxc02 was partitioned away at 01:42:31 and did not serve a query for 42 seconds,
//	 while pxc01 and pxc03 kept quorum throughout"
//
// is four records in three files, and it is the sentence somebody opened the logs to find.
//
// Two rules govern what goes in here. A finding must be derivable from the records rather
// than assumed from their absence — and where the absence itself is misleading, the
// finding says so out loud instead. That is why lsFindingFlowControl exists: flow control
// leaves almost no trace in a PXC error log, and a page that stayed quiet about it would
// be read as "no flow control happened".

import (
	"fmt"
	"sort"
	"strings"
)

// lsFinding is one conclusion.
type lsFinding struct {
	ID      string  `json:"id"`
	Sev     string  `json:"sev"`
	Title   string  `json:"title"`
	Detail  string  `json:"detail"`
	Advice  string  `json:"advice,omitempty"`
	At      float64 `json:"at,omitempty"`    // when it began, for "take me there"
	Until   float64 `json:"until,omitempty"` // when it ended, when that is known
	Sources []int   `json:"sources,omitempty"`
	Events  []int   `json:"events,omitempty"` // event numbers that support it
}

// lsFindings runs every check over a built bundle, worst first.
func lsFindings(b *lsBundle) []lsFinding {
	out := []lsFinding{}
	for _, check := range append([]func(*lsBundle) []lsFinding{
		lsFindingCrash,
		lsFindingUncleanRestart,
		lsFindingPartition,
		lsFindingQuorum,
		lsFindingDisagreement,
		lsFindingUnavailability,
		lsFindingTransfer,
		lsFindingDesync,
		lsFindingReplicationBroken,
		lsFindingSilentReconnect,
		lsFindingReplicaLag,
		lsFindingBootstrap,
		lsFindingCleanStop,
		lsFindingFlowControl,
		lsFindingCoverage,
		lsFindingHealthy,
	}, append(lsGRFindings, lsMongoFindings...)...) {
		out = append(out, check(b)...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return lsSevRank[out[i].Sev] > lsSevRank[out[j].Sev]
	})
	return out
}

// lsNode names a source the way a reader would: its own name from the log, falling back
// to the file it came from.
func lsNode(b *lsBundle, src int) string {
	for _, s := range b.Sources {
		if s.Idx == src {
			if s.Node != "" {
				return s.Node
			}
			return s.Name
		}
	}
	return fmt.Sprintf("source %d", src)
}

// lsName translates a member UUID to its node name when the bundle knows one.
func lsName(b *lsBundle, uuid string) string {
	if n, ok := b.Names[uuid]; ok && n != "" {
		return n
	}
	return uuid
}

// lsNames translates a list of member UUIDs, sorted for a stable sentence.
func lsNames(b *lsBundle, uuids []string) []string {
	out := make([]string, 0, len(uuids))
	for _, u := range uuids {
		out = append(out, lsName(b, u))
	}
	sort.Strings(out)
	return out
}

// lsPick collects the events matching a predicate.
// lsSrcIs reports whether a source has this flavour. Findings written for one clustering
// technology need it: Galera and Group Replication both produce membership events, both
// set lsEvent.Lost, and both can lose quorum — so a finding that picks on the shape of an
// event alone will happily explain a Group Replication outage in Galera's vocabulary,
// telling the reader about the primary component and error 1047 on a server that has
// neither. Live verification against a running group is what surfaced that.
func lsSrcIs(b *lsBundle, src int, flavour string) bool {
	for _, s := range b.Sources {
		if s.Idx == src {
			return s.Flavour == flavour
		}
	}
	return false
}

// lsPickIn is lsPick restricted to sources of one flavour.
func lsPickIn(b *lsBundle, flavour string, pred func(lsEvent) bool) []lsEvent {
	return lsPick(b, func(e lsEvent) bool { return lsSrcIs(b, e.Src, flavour) && pred(e) })
}

func lsPick(b *lsBundle, pred func(lsEvent) bool) []lsEvent {
	var out []lsEvent
	for _, e := range b.Events {
		if pred(e) {
			out = append(out, e)
		}
	}
	return out
}

func lsEventNos(events []lsEvent, max int) []int {
	out := []int{}
	for _, e := range events {
		if len(out) >= max {
			break
		}
		out = append(out, e.No)
	}
	return out
}

func lsSrcSet(events []lsEvent) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, e := range events {
		if !seen[e.Src] {
			seen[e.Src] = true
			out = append(out, e.Src)
		}
	}
	sort.Ints(out)
	return out
}

// lsDur renders a span the way an operator reads one.
func lsDur(sec float64) string {
	switch {
	case sec <= 0:
		return "0s"
	case sec < 1:
		return fmt.Sprintf("%.0f ms", sec*1000)
	case sec < 90:
		return fmt.Sprintf("%.1fs", sec)
	case sec < 5400:
		return fmt.Sprintf("%.1f min", sec/60)
	case sec < 172800:
		return fmt.Sprintf("%.1f h", sec/3600)
	}
	// Days, because MySQL's default replica retry policy is 86400 attempts a minute
	// apart and "1440.0 h" is not a number anybody reads as two months.
	return fmt.Sprintf("%.0f days", sec/86400)
}

// ---------------------------------------------------------------- checks

// lsFindingUncleanRestart — a node whose previous stop was not clean.
//
// Separate from lsFindingCrash because it is evidence of a PAST death rather than one in
// this window, and because the record it rests on (--wsrep-recover finding a real
// position) is written during a perfectly normal start-up.
func lsFindingUncleanRestart(b *lsBundle) []lsFinding {
	ev := lsPick(b, func(e lsEvent) bool { return e.Label == "Restarted after an unclean stop" })
	if len(ev) == 0 {
		return nil
	}
	who := []string{}
	for _, src := range lsSrcSet(ev) {
		who = append(who, lsNode(b, src))
	}
	return []lsFinding{{
		ID: "unclean-restart", Sev: lsSevWarn,
		Title:  "A node restarted after an unclean stop",
		Detail: fmt.Sprintf("%s ran --wsrep-recover and found a real position to recover, which only happens when the previous mysqld did not shut down cleanly.", strings.Join(who, ", ")),
		Advice: "Whatever killed it happened before this log begins, so look at the previous run: the end of the rotated log, the systemd journal for the unit, and the OOM killer. The recovered sequence number tells you how far it got.",
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 4),
	}}
}

// lsFindingCrash — a server process that ended without being asked to.
func lsFindingCrash(b *lsBundle) []lsFinding {
	// Severity, not just class: the crash handler's report is re-emitted line by line on
	// some builds and those lines are class crash too, at info. Listing them would report
	// one crash as "Server crashed — signal 11; Crash report".
	ev := lsPick(b, func(e lsEvent) bool { return e.Class == lsClassCrash && e.Sev == lsSevBad })
	if len(ev) == 0 {
		return nil
	}
	byNode := map[int][]lsEvent{}
	for _, e := range ev {
		byNode[e.Src] = append(byNode[e.Src], e)
	}
	var lines []string
	for _, src := range lsSrcSet(ev) {
		labels := []string{}
		seen := map[string]bool{}
		for _, e := range byNode[src] {
			if !seen[e.Label] {
				seen[e.Label] = true
				labels = append(labels, e.Label)
			}
		}
		lines = append(lines, lsNode(b, src)+": "+strings.Join(labels, "; "))
	}
	return []lsFinding{{
		ID: "crash", Sev: lsSevBad, Title: "A server stopped abnormally",
		Detail: strings.Join(lines, " · "),
		Advice: "Read the records just before each of these: an abort names its cause on the line above, and a wsrep position recovery means the previous stop was unclean. A node that ends in an abort does not come back by itself.",
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 8),
	}}
}

// lsSuspectLead is how far back a view change is allowed to look for evidence that the
// departure was a failure rather than a shutdown.
//
// evs.suspect_timeout defaults to 5 s and evs.inactive_timeout to 15 s, so a node that
// died is declared gone up to fifteen seconds after it stopped answering. Twenty gives
// that room without reaching back into an unrelated earlier incident.
const lsSuspectLead = 20.0

// lsDepartureWasUnclean decides whether the members that left a view were lost or were
// shut down, from what the reporting node logged just before.
//
// This is the discriminator the corpus produced, replacing the one it disproved. A clean
// `systemctl restart mysql` and a SIGKILL both take the departing member out under
// `partitioned`, but they look nothing alike in the seconds leading up to it:
//
//	SIGKILL   ~5 s of "reconnecting to … attempt 0", "Connection refused", then
//	          "declaring node with index 1 suspected, timeout PT5S (evs.suspect_timeout)"
//	          and "suspected node without join message, declaring inactive"
//	restart    nothing at all — the view changes about a millisecond after the socket
//	          closes, because the leaving node announced itself
func lsDepartureWasUnclean(b *lsBundle, view lsEvent) bool {
	for i := len(b.Events) - 1; i >= 0; i-- {
		e := b.Events[i]
		if e.Src != view.Src || e.TS > view.TS {
			continue
		}
		if view.TS-e.TS > lsSuspectLead {
			break
		}
		switch e.Label {
		case "Peer suspected", "Peer declared inactive", "Peer went quiet",
			"Peer refused the connection", "Reconnecting to a peer",
			"Could not connect to a peer", "Relaying messages for unreachable peers":
			return true
		}
	}
	return false
}

// lsFindingPartition — members that were lost rather than stopped.
func lsFindingPartition(b *lsBundle) []lsFinding {
	ev := lsPickIn(b, lsFlavourGalera, func(e lsEvent) bool { return len(e.Lost) > 0 && lsDepartureWasUnclean(b, e) })
	if len(ev) == 0 {
		return nil
	}
	lost := map[string]bool{}
	for _, e := range ev {
		for _, m := range e.Lost {
			lost[m] = true
		}
	}
	uuids := make([]string, 0, len(lost))
	for m := range lost {
		uuids = append(uuids, m)
	}
	names := lsNames(b, uuids)
	// How long the group ran short-handed: to the next view that reported more members
	// and nobody missing.
	until := 0.0
	for _, e := range b.Events {
		if e.TS > ev[0].TS && e.Class == lsClassMember && len(e.Lost) == 0 && e.Members > ev[0].Members {
			until = e.TS
			break
		}
	}
	detail := fmt.Sprintf("%d member(s) (%s) stopped answering and were declared gone, reported by %d of %d node(s). The survivors sat through a suspect timeout first, which is what a failure looks like and what a shutdown does not.",
		len(names), strings.Join(names, ", "), len(lsSrcSet(ev)), len(b.Sources))
	if until > 0 {
		detail += fmt.Sprintf(" The group was back to full membership %s later.", lsDur(until-ev[0].TS))
	}
	return []lsFinding{{
		ID: "partition", Sev: lsSevBad, Title: "A member was lost, not shut down",
		Detail: detail,
		Advice: "The process died or the network to it broke. Check the missing node's own log at this instant: a node that logged a crash tells you which; a node that logged nothing at all, and then came back reporting that IT could not see anyone either, means the network.",
		At:     ev[0].TS, Until: until, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 8),
	}}
}

// lsFindingQuorum — nodes that lost the primary component, and whether the others kept it.
// lsIsShuttingDown reports whether a source had already been asked to stop by time t.
//
// A node on its way out goes non-primary as a matter of course — it closes gcomm, sees a
// group it is no longer part of, and says so. Reading that as "the cluster split" turns
// every planned restart into an incident, which the graceful-restart fixture demonstrated
// before this check existed.
func lsIsShuttingDown(b *lsBundle, src int, t float64) bool {
	shutting := false
	for _, e := range b.Events {
		if e.Src != src || e.TS > t {
			continue
		}
		switch {
		case e.Label == "Shutdown requested" || e.Label == "Left the cluster cleanly":
			shutting = true
		case e.Class == lsClassStartup && strings.HasPrefix(e.Label, "Server starting"):
			shutting = false // a new process; the old one's shutdown is over
		}
	}
	return shutting
}

func lsFindingQuorum(b *lsBundle) []lsFinding {
	// Galera only: Group Replication loses its majority in its own vocabulary and has its
	// own finding for it (lsFindingGRSplit), which says "blocked all updates" rather than
	// "refused every query with 1047" — the second of which is simply not true of a GR
	// member, since it goes on answering reads.
	ev := lsPickIn(b, lsFlavourGalera, func(e lsEvent) bool {
		return e.Class == lsClassQuorum && e.Sev == lsSevBad && !lsIsShuttingDown(b, e.Src, e.TS)
	})
	if len(ev) == 0 {
		return nil
	}
	losers := lsSrcSet(ev)
	// Did anybody keep quorum while these lost it? That is the difference between a
	// partition (some writes still succeeded somewhere) and a total outage.
	keptQuorum := []int{}
	for _, s := range b.Sources {
		isLoser := false
		for _, l := range losers {
			if l == s.Idx {
				isLoser = true
			}
		}
		if isLoser {
			continue
		}
		if p, ok := lsStateAt(b.Phases, s.Idx, ev[0].TS); ok && p.State == lsStateSynced {
			keptQuorum = append(keptQuorum, s.Idx)
		}
	}
	names := []string{}
	for _, l := range losers {
		names = append(names, lsNode(b, l))
	}
	f := lsFinding{
		ID: "quorum", Sev: lsSevBad,
		Title: "A node lost the primary component",
		At:    ev[0].TS,
		Detail: fmt.Sprintf("%s could not see a majority of the cluster and refused every query with 1047 while that lasted.",
			strings.Join(names, ", ")),
		Sources: losers, Events: lsEventNos(ev, 8),
	}
	if len(keptQuorum) > 0 {
		kept := []string{}
		for _, k := range keptQuorum {
			kept = append(kept, lsNode(b, k))
		}
		f.Title = "The cluster split — one side kept quorum, the other did not"
		f.Detail += fmt.Sprintf(" %s stayed in the primary component and carried on serving, so this was a partition rather than a cluster-wide outage.",
			strings.Join(kept, " and "))
		f.Advice = "Writes that reached the majority side committed; anything sent to the minority side was refused, not lost. Check what your load balancer was doing — a proxy that health-checks only TCP will happily keep sending traffic to a node answering 1047."
	} else {
		f.Detail += " No node in this bundle held the primary component at that moment, so nothing anywhere was accepting writes."
		f.Advice = "If the whole cluster is non-primary and the members can see each other again, one of them has to be told to re-form the component (pc.bootstrap=1 on the most advanced node). Check every node's last committed seqno before choosing."
	}
	return []lsFinding{f}
}

// lsFindingDisagreement — two nodes describing different clusters at the same instant.
//
// This is the check that only exists because several logs are read together. Each node
// reports the membership it can see; when two of them disagree at the same moment, the
// disagreement itself is the diagnosis.
func lsFindingDisagreement(b *lsBundle) []lsFinding {
	if len(b.Sources) < 2 {
		return nil
	}
	// The widest disagreement in the window, and when it was.
	var worstAt float64
	var worstLo, worstHi int
	// Sample at every membership or quorum event: those are the only instants at which a
	// node's view can have changed, so nothing is missed by not sampling in between.
	for _, e := range b.Events {
		if e.Class != lsClassMember && e.Class != lsClassQuorum {
			continue
		}
		// A moment later, so every node has processed the change this event announced.
		t := e.TS + 0.5
		lo, hi, seen := 0, 0, 0
		for _, s := range b.Sources {
			p, ok := lsStateAt(b.Phases, s.Idx, t)
			if !ok || p.Members == 0 || p.State == lsStateDown {
				continue
			}
			seen++
			if lo == 0 || p.Members < lo {
				lo = p.Members
			}
			if p.Members > hi {
				hi = p.Members
			}
		}
		if seen >= 2 && hi-lo > worstHi-worstLo {
			worstAt, worstLo, worstHi = t, lo, hi
		}
	}
	if worstHi-worstLo < 1 {
		return nil
	}
	var parts []string
	split := false
	joining := false
	for _, s := range b.Sources {
		p, ok := lsStateAt(b.Phases, s.Idx, worstAt)
		// A node that was not running, or whose log has not said anything yet, is not a
		// party to the disagreement — listing it as "saw 0 members" reads like a third
		// opinion when it is an absence of one.
		if !ok || p.State == lsStateDown || p.State == "UNKNOWN" {
			continue
		}
		prim := ""
		if p.Primary == "no" {
			prim = ", non-primary"
			split = true
		} else if p.Primary == "yes" {
			prim = ", primary"
		}
		if p.State == lsStateJoiner || p.State == lsStateJoined || p.State == lsStatePrim {
			joining = true
		}
		parts = append(parts, fmt.Sprintf("%s saw %d member(s)%s and was %s",
			lsNode(b, s.Idx), p.Members, prim, p.State))
	}
	f := lsFinding{
		ID: "disagreement", Sev: lsSevBad,
		Title:  "The nodes did not agree on who was in the cluster",
		Detail: strings.Join(parts, "; ") + ".",
		Advice: "Each node reports only what it can see, so a disagreement of this shape is a partition seen from both sides at once. The node reporting the smaller membership is the one that was cut off.",
		At:     worstAt, Sources: lsSrcSet(b.Events),
	}
	// Nobody outside the primary component, and somebody mid-join: this is a membership
	// change in progress, not a split. Members legitimately learn about each other a few
	// hundred milliseconds apart, and reporting that as a fault would fire on every
	// healthy rejoin — including every one in the bootstrap fixture.
	if !split && joining {
		f.Sev = lsSevWarn
		f.Title = "The nodes' membership counts differed while one was joining"
		f.Advice = "Every node here was still in the primary component, so this is the ordinary lag between a member being admitted and the others recording it — not a split. Worth knowing only because a load balancer sampling at this instant would have got inconsistent answers."
	}
	return []lsFinding{f}
}

// lsFindingUnavailability — how long each node spent not answering queries.
//
// The single number people want from a cluster's logs, and one nobody can get by reading
// them: it is the sum of every stretch outside SYNCED, which means pairing transitions
// that may be hundreds of lines apart.
func lsFindingUnavailability(b *lsBundle) []lsFinding {
	window := b.Summary.LastTS - b.Summary.FirstTS
	if window <= 0 {
		return nil
	}
	type row struct {
		src  int
		down float64
		why  map[string]float64
	}
	var rows []row
	for _, s := range b.Sources {
		r := row{src: s.Idx, why: map[string]float64{}}
		for _, p := range b.Phases {
			if p.Src != s.Idx || lsStateServes(p.State) {
				continue
			}
			d := p.To - p.From
			// UNKNOWN is "the log had not said yet", not "the node was down". Counting
			// it would report a node that was fine before the first transition as
			// unavailable for the whole lead-in.
			if p.State == "UNKNOWN" {
				continue
			}
			r.down += d
			r.why[p.State] += d
		}
		if r.down > 0.5 {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].down > rows[j].down })
	var parts []string
	stranded := 0.0
	for _, r := range rows {
		states := make([]string, 0, len(r.why))
		for st, d := range r.why {
			states = append(states, fmt.Sprintf("%s %s", lsDur(d), st))
		}
		sort.Strings(states)
		parts = append(parts, fmt.Sprintf("%s: %s not serving (%s)",
			lsNode(b, r.src), lsDur(r.down), strings.Join(states, ", ")))
		if r.why[lsStateOpen] > stranded {
			stranded = r.why[lsStateOpen]
		}
	}
	// Not all downtime is a fault. A node that is JOINER, JOINED or DONOR is doing planned
	// work — receiving a transfer, applying a backlog, feeding a joiner — and one that is
	// DOWN or CLOSED during a restart was asked to be. What is never planned is OPEN: a
	// running server that can see no primary component, refusing every query it is sent.
	// So that is what decides the severity, and a bootstrap no longer reads as an outage.
	sev := lsSevWarn
	if stranded > 1 {
		sev = lsSevBad
	}
	srcs := []int{}
	for _, r := range rows {
		srcs = append(srcs, r.src)
	}
	return []lsFinding{{
		ID: "unavailable", Sev: sev,
		Title: "Time spent not answering queries",
		Detail: strings.Join(parts, " · ") +
			fmt.Sprintf(" — over a %s window.", lsDur(window)),
		Advice:  "Only SYNCED serves. A node in JOINED has the data but is still applying its backlog and is holding flow control on everyone else while it does; a node in DONOR is busy feeding a joiner. Neither belongs in a load balancer's pool, and a proxy that only checks the port will send traffic to both.",
		Sources: srcs,
	}}
}

// lsFindingTransfer — state transfers, and the expensive kind in particular.
func lsFindingTransfer(b *lsBundle) []lsFinding {
	var out []lsFinding
	if fb := lsPick(b, func(e lsEvent) bool {
		return strings.Contains(e.Label, "falling back to SST")
	}); len(fb) > 0 {
		out = append(out, lsFinding{
			ID: "ist-fallback", Sev: lsSevBad,
			Title: "A rejoin needed a full SST because the gcache was too small",
			// The record is written by the DONOR — the node that was asked to help and
			// could not — so it names the node whose gcache is the problem, which is the
			// node whose gcache.size needs raising.
			Detail: fmt.Sprintf("%s was asked for an incremental transfer and its gcache no longer held the writesets the joiner needed, so the whole dataset had to be copied instead.", lsNode(b, fb[0].Src)),
			Advice: "gcache.size decides how long a node can be away and still rejoin by IST. Size it against the longest outage you expect to survive, not against the dataset — a few gigabytes of gcache routinely saves an hour of SST.",
			At:     fb[0].TS, Sources: lsSrcSet(fb), Events: lsEventNos(fb, 4),
		})
	}
	if failed := lsPick(b, func(e lsEvent) bool { return e.Label == "State transfer FAILED" }); len(failed) > 0 {
		out = append(out, lsFinding{
			ID: "sst-failed", Sev: lsSevBad,
			Title: "A state transfer failed",
			// The event's own meaning already names both ends, read out of the record
			// rather than guessed from whose file it was found in.
			Detail: failed[0].Meaning,
			Advice: "Look at what happened to the donor and the network in the seconds before this. A transfer that dies with its donor is the network; one that dies on its own is usually the SST script — check the joiner's innobackup/xtrabackup log in the datadir.",
			At:     failed[0].TS, Sources: lsSrcSet(failed), Events: lsEventNos(failed, 4),
		})
	}
	sst := lsPick(b, func(e lsEvent) bool { return e.Label == "SST in progress" })
	if len(sst) > 0 {
		done := 0.0
		for _, e := range b.Events {
			if e.TS > sst[0].TS && e.Label == "State transfer complete" {
				done = e.TS
				break
			}
		}
		d := ""
		if done > 0 {
			d = fmt.Sprintf(" It took %s.", lsDur(done-sst[0].TS))
		}
		out = append(out, lsFinding{
			ID: "sst", Sev: lsSevWarn,
			Title:  "A full state snapshot transfer ran",
			Detail: fmt.Sprintf("%s received a complete physical copy of the dataset.%s The donor was out of the read pool for the duration.", lsNode(b, sst[0].Src), d),
			Advice: "An SST scales with the dataset and an IST does not. If rejoins are routinely taking an SST, the gcache is the setting to look at before anything else.",
			At:     sst[0].TS, Until: done, Sources: lsSrcSet(sst), Events: lsEventNos(sst, 4),
		})
	}
	return out
}

// lsFindingDesync — a node that took itself out of the group, which on a live cluster is
// almost always a backup and is almost always a surprise to whoever is reading the logs.
func lsFindingDesync(b *lsBundle) []lsFinding {
	// lsDesyncFloor keeps the finding for desyncs long enough to matter. A donor desyncs
	// for milliseconds as part of every state transfer, and PXC's own start-up sequence
	// desyncs and resyncs within 3 ms; reporting those as "a backup took a node out of the
	// pool" is noise, and the state-transfer findings already cover the donor case.
	const lsDesyncFloor = 2.0

	var ev []lsEvent
	until := 0.0
	for _, e := range lsPick(b, func(e lsEvent) bool { return e.Label == "Member desynced itself" }) {
		end := 0.0
		for _, r := range b.Events {
			if r.TS > e.TS && r.Src == e.Src && r.Label == "Member resynced" {
				end = r.TS
				break
			}
		}
		// An unpaired desync is one that had not ended when the log did, which is the
		// worst version of this and must not be dropped for want of a duration.
		if end == 0 || end-e.TS >= lsDesyncFloor {
			ev = append(ev, e)
			if until == 0 {
				until = end
			}
		}
	}
	if len(ev) == 0 {
		return nil
	}
	d := ""
	if until > 0 {
		d = fmt.Sprintf(" for %s", lsDur(until-ev[0].TS))
	} else {
		d = " and had not rejoined by the end of the log"
	}
	who := ev[0].Peer
	if who == "" {
		who = lsNode(b, ev[0].Src)
	}
	return []lsFinding{{
		ID: "desync", Sev: lsSevWarn,
		Title:  "A member desynced itself from the group",
		Detail: fmt.Sprintf("%s left flow control%s — normally FLUSH TABLES WITH READ LOCK, a backup taking a consistent snapshot, or wsrep_desync=ON. It stayed up and reachable the whole time, and it was falling behind the whole time.", who, d),
		Advice: "A desynced node still accepts connections and still passes a port-level health check, so a load balancer will keep sending it reads it can only answer with stale data. Check that whatever takes your backups also takes the node out of rotation.",
		At:     ev[0].TS, Until: until, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 4),
	}}
}

// lsFindingBootstrap — somebody started a cluster instead of joining one.
func lsFindingBootstrap(b *lsBundle) []lsFinding {
	ev := lsPick(b, func(e lsEvent) bool { return e.Label == "Bootstrapped a new cluster" })
	if len(ev) == 0 {
		return nil
	}
	sev, advice := lsSevWarn, "Bootstrapping is correct exactly once per cluster lifetime — at creation, or when bringing the whole cluster back from cold. Confirm this was one of those."
	// More than one node bootstrapping in the same window is the failure mode worth
	// shouting about: it creates two clusters with the same name that will never merge.
	if len(lsSrcSet(ev)) > 1 {
		sev = lsSevBad
		advice = "Two different nodes bootstrapped. That does not produce one cluster with two members — it produces two clusters, each convinced it is the whole thing, with divergent data and no way to merge. Stop all but one and rejoin the rest."
	}
	names := []string{}
	for _, s := range lsSrcSet(ev) {
		names = append(names, lsNode(b, s))
	}
	return []lsFinding{{
		ID: "bootstrap", Sev: sev,
		Title:  "A new cluster was bootstrapped",
		Detail: fmt.Sprintf("%s started a cluster rather than joining one.", strings.Join(names, " and ")),
		Advice: advice,
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 4),
	}}
}

// lsCrashNear reports whether any source recorded a crash close to t, on either side —
// a node's own abort record can be written a moment after the survivors have already
// dropped it from the view.
func lsCrashNear(b *lsBundle, t float64) bool {
	for _, e := range b.Events {
		if e.Class != lsClassCrash {
			continue
		}
		if d := e.TS - t; d > -lsSuspectLead && d < lsSuspectLead {
			return true
		}
	}
	return false
}

// lsFindingCleanStop — the reassuring counterpart to lsFindingPartition, and the reason
// that check has to be evidence-based rather than keyword-based: without it, every planned
// restart in the fleet reads as an incident.
func lsFindingCleanStop(b *lsBundle) []lsFinding {
	// Direct evidence, from the departing node's own log if it is in the bundle.
	direct := lsPickIn(b, lsFlavourGalera, func(e lsEvent) bool {
		return e.Label == "Shutdown requested" || e.Label == "Left the cluster cleanly"
	})
	// Indirect evidence, from the survivors: a member left the view with no suspect
	// timeout in front of it, which happens when it announced its departure.
	//
	// "Happens when", not "only happens when" — and the difference matters. A process that
	// ABORTS also closes its sockets on the way out, so from the survivors' side an
	// orderly shutdown and a crash-on-exit look identical. The network-partition fixture
	// contains exactly that: pxc03 aborted, its sockets closed, and the survivors dropped
	// it from the view instantly. So this evidence is only trusted when the bundle holds
	// no crash for anybody at around the same time.
	quiet := lsPickIn(b, lsFlavourGalera, func(e lsEvent) bool {
		return len(e.Lost) > 0 && !lsDepartureWasUnclean(b, e) && !lsCrashNear(b, e.TS)
	})
	if len(direct) == 0 && len(quiet) == 0 {
		return nil
	}
	ev := direct
	if len(ev) == 0 {
		ev = quiet
	}
	var parts []string
	if len(direct) > 0 {
		who := []string{}
		for _, s := range lsSrcSet(direct) {
			who = append(who, lsNode(b, s))
		}
		parts = append(parts, strings.Join(who, ", ")+" logged a shutdown request and announced its departure to the group")
	}
	if len(quiet) > 0 {
		names := map[string]bool{}
		for _, e := range quiet {
			for _, m := range e.Lost {
				names[m] = true
			}
		}
		uuids := make([]string, 0, len(names))
		for n := range names {
			uuids = append(uuids, n)
		}
		list := lsNames(b, uuids)
		parts = append(parts, fmt.Sprintf("the survivors dropped %s from the view immediately, with no suspect timeout in front of it", strings.Join(list, ", ")))
	}
	return []lsFinding{{
		ID: "clean-stop", Sev: lsSevWarn,
		Title:  "A member left the cluster cleanly",
		Detail: strings.Join(parts, "; ") + " — a planned stop, not a failure.",
		Advice: "Quorum was recalculated at once instead of after an evs.suspect_timeout, so there was no window in which writes stalled waiting for a node that was never coming back. Note that the remaining members were still one short until it returned.",
		At:     ev[0].TS, Sources: lsSrcSet(ev), Events: lsEventNos(ev, 4),
	}}
}

// lsFindingFlowControl — the honest note about what the log cannot tell you.
//
// Verified rather than assumed: driving a three-node cluster hard enough to register ten
// received flow-control messages and 91 ms of measured pause (wsrep_flow_control_recv and
// wsrep_flow_control_paused_ns) produced exactly one line in the error log, and that line
// was the interval, not a pause. A page that said nothing here would be read as "no flow
// control happened", which is a much stronger claim than the file supports.
func lsFindingFlowControl(b *lsBundle) []lsFinding {
	galera := false
	for _, s := range b.Sources {
		if s.Flavour == lsFlavourGalera {
			galera = true
		}
	}
	if !galera {
		return nil
	}
	low := lsPick(b, func(e lsEvent) bool {
		return e.Class == lsClassFlowCtl && e.Label == "Flow-control interval" && e.Sev == lsSevWarn
	})
	f := lsFinding{
		ID: "flow-control", Sev: lsSevInfo,
		Title:  "Flow-control pauses are not recorded in this log",
		Detail: "Galera writes the flow-control interval when membership changes, and nothing at all when it actually pauses the cluster — a run that paused for 91 ms and exchanged ten flow-control messages wrote one line, and that line was the threshold. Absence of flow-control records here is not evidence that none happened.",
		Advice: "The measurements live in the server: wsrep_flow_control_paused (fraction of time paused since the counter was reset), wsrep_flow_control_sent / _recv, and wsrep_local_recv_queue_avg. Watch those, or the same series in PMM. Setting gcs.fc_debug does add `FC: queue size` records to the log, at the cost of a lot of volume.",
	}
	if len(low) > 0 {
		f.Sev = lsSevWarn
		f.Title = "The flow-control threshold is far below the default"
		f.Detail = fmt.Sprintf("%s reported %s. The default is gcs.fc_limit (100) scaled by member count — 141 for two members, 173 for three — so a threshold this low means the cluster pauses every writer as soon as this node is a couple of writesets behind. %s",
			lsNode(b, low[0].Src), low[0].Message, f.Detail)
		f.At = low[0].TS
		f.Sources = lsSrcSet(low)
		f.Events = lsEventNos(low, 4)
	}
	return []lsFinding{f}
}

// lsFindingCoverage — whether the bundle can support a comparison at all.
func lsFindingCoverage(b *lsBundle) []lsFinding {
	var out []lsFinding
	if b.Summary.Disjoint {
		out = append(out, lsFinding{
			ID: "disjoint", Sev: lsSevBad,
			Title:  "These logs do not cover a common period",
			Detail: "No instant is present in every source, so nothing here can be compared across nodes — the timeline will still draw, and any side-by-side reading of it will be wrong.",
			Advice: "Usually a log from a different day, a node whose log was rotated before the incident, or a host clock that is badly wrong. Check each source's own range in the Sources panel; if it is clock skew, set that source's offset.",
		})
	}
	if len(b.Sources) == 1 {
		s := b.Sources[0]
		if s.Flavour == lsFlavourGalera {
			out = append(out, lsFinding{
				ID: "single-source", Sev: lsSevWarn,
				Title:  "Only one member's log is here",
				Detail: "A Galera node's log records what that node could see. When it says a peer went inactive, that is a statement about this node's network as much as about the peer.",
				Advice: "Add the other members' logs. A peer that reports itself perfectly healthy at the moment this one declared it dead points at the link between them, not at either node.",
			})
		}
	}
	return out
}

// lsFindingHealthy — the "good" verdict, which needs stating explicitly.
//
// A healthy PXC cluster under load writes NOTHING to its error log. That was measured:
// thirty seconds of continuous inserts across a three-node cluster produced zero new
// records on all three. An empty event list therefore means either "nothing went wrong" or
// "you gave me the wrong file", and only the source metadata can tell them apart.
func lsFindingHealthy(b *lsBundle) []lsFinding {
	if b.Summary.Counts[lsSevBad] > 0 || b.Summary.Counts[lsSevWarn] > 0 {
		return nil
	}
	if len(b.Sources) == 0 {
		return nil
	}
	total := 0
	for _, s := range b.Sources {
		total += s.Lines
	}
	if b.Summary.Events == 0 {
		return []lsFinding{{
			ID: "quiet", Sev: lsSevOK,
			Title:  "Nothing was written in this window",
			Detail: fmt.Sprintf("%d line(s) across %d source(s) and not one record worth reporting. A healthy cluster under load writes nothing to its error log, so this is the expected shape of a good period — but it is also the shape of the wrong file.", total, len(b.Sources)),
			Advice: "Check each source's covered range in the Sources panel against the period you meant to look at.",
		}}
	}
	return []lsFinding{{
		ID: "healthy", Sev: lsSevOK,
		Title:  "No problems found in this window",
		Detail: fmt.Sprintf("%d record(s) across %d source(s), all of them routine or good news: no crash, no partition, no lost quorum, no state transfer.", b.Summary.Events, len(b.Sources)),
	}}
}
