package main

// logsummary_model.go — several nodes' logs as one timeline.
//
// A single log read on its own answers "what happened here". Three logs read together
// answer the question people actually have, which is "what state was the cluster in at
// 01:49:35, and which node is telling the truth about it".
//
// That question needs three things this file builds:
//
//	events  every classified record from every source, merged and renumbered in time order
//	phases  a continuous state track per source, so any instant can be looked up rather
//	        than reconstructed by eye from the transitions around it
//	buckets per-source severity counts over a time grid, which is what the swimlane draws
//
// Clocks are the quiet hazard. MySQL writes RFC3339 with an explicit zone (Z under the
// default log_timestamps=UTC, an offset under SYSTEM), so records from nodes in different
// timezones land on the correct absolute instant without anyone configuring anything.
// What can still go wrong is host clock skew, which no amount of parsing fixes — so each
// source carries an adjustable offset, and lsOverlap reports when two sources do not
// overlap at all, which is the shape of "you uploaded logs from different days".

import (
	"sort"
	"strings"
)

// lsSource is one log file: one node's view of the cluster.
type lsSource struct {
	Idx     int     `json:"idx"`
	Name    string  `json:"name"`    // file name, or the node label it was read from
	Node    string  `json:"node"`    // the node's own name, discovered in the log
	Engine  string  `json:"engine"`  // mysql | postgres | mongodb | valkey
	Flavour string  `json:"flavour"` // galera | mysql
	Path    string  `json:"path,omitempty"`
	Origin  string  `json:"origin"` // upload | node
	Bytes   int     `json:"bytes"`
	Lines   int     `json:"lines"`
	Records int     `json:"records"` // folded records, before noise is dropped
	Events  int     `json:"events"`  // classified events kept
	FirstTS float64 `json:"firstTs"`
	LastTS  float64 `json:"lastTs"`
	// Offset is added to every timestamp from this source, in seconds. It exists for
	// host clock skew, which is the one misalignment parsing cannot fix.
	Offset float64        `json:"offset"`
	Counts map[string]int `json:"counts"` // severity → number of events
	// Untimed is the number of records whose header carried no timestamp. They inherit
	// the previous record's, which is close enough to place them but not to trust to the
	// millisecond, and saying how many there were is more honest than silently placing them.
	Untimed int `json:"untimed"`
}

// lsPhase is a stretch of time during which a source was in one state. Phases tile the
// whole bundle window without gaps, so "what was this node doing at t" is a lookup.
type lsPhase struct {
	Src     int     `json:"src"`
	From    float64 `json:"from"`
	To      float64 `json:"to"`
	State   string  `json:"state"`
	Sev     string  `json:"sev"`
	Members int     `json:"members,omitempty"`
	Primary string  `json:"primary,omitempty"` // yes | no | ""
	// Inferred marks a state the log did not state outright. See lsSeedState: a member
	// that was already SYNCED when the excerpt begins may log no transition at all, and
	// the UI has to be able to say "deduced" rather than "recorded".
	Inferred bool `json:"inferred,omitempty"`
}

// lsBucket is one cell of the swimlane: how many events of each severity a source
// produced in one slice of time.
type lsBucket struct {
	Src   int     `json:"src"`
	I     int     `json:"i"`
	TS    float64 `json:"ts"`
	OK    int     `json:"ok"`
	Warn  int     `json:"warn"`
	Bad   int     `json:"bad"`
	Info  int     `json:"info"`
	Count int     `json:"count"`
}

// lsStat is one label and how often it appeared, for the "what dominated this window"
// strip above the event list.
type lsStat struct {
	Label string `json:"label"`
	Class string `json:"class"`
	Sev   string `json:"sev"`
	Count int    `json:"count"`
}

// lsSummary is the headline for a whole bundle.
type lsSummary struct {
	Sources  int            `json:"sources"`
	Events   int            `json:"events"`
	FirstTS  float64        `json:"firstTs"`
	LastTS   float64        `json:"lastTs"`
	Counts   map[string]int `json:"counts"`  // severity → count
	Classes  map[string]int `json:"classes"` // class → count
	Top      []lsStat       `json:"top"`
	Overlap  float64        `json:"overlap"` // seconds every source has in common; 0 = none
	Disjoint bool           `json:"disjoint"`
}

// lsBundle is a set of logs read together.
type lsBundle struct {
	Sources []lsSource  `json:"sources"`
	Events  []lsEvent   `json:"events"`
	Phases  []lsPhase   `json:"phases"`
	Summary lsSummary   `json:"summary"`
	Finding []lsFinding `json:"findings"`
	// Names maps member UUIDs — in both the full and the abbreviated form Galera uses —
	// to node names, pooled across every source. One log names the members it can see; put
	// three together and almost every UUID in the bundle has a name attached to it, which
	// is the difference between "119e686d-8943 stopped answering" and "pxc02 stopped
	// answering".
	Names map[string]string `json:"names,omitempty"`
}

// lsInput is one log handed to the builder: its bytes and where they came from.
type lsInput struct {
	Name   string
	Path   string
	Origin string
	Engine string // "" = sniff it
	Data   []byte
	// Offset is added to every timestamp read from this source, in seconds — the manual
	// correction for host clock skew. Parsing gets the timezone right on its own; it can
	// do nothing about a machine whose clock is forty seconds fast.
	Offset float64
}

// lsMaxEvents bounds a bundle. Past this the page is not the right tool: the classifier
// keeps roughly one event per twenty raw lines on a Galera log, so this is a few million
// lines of input.
const lsMaxEvents = 120000

// lsBuild parses, classifies and merges a set of logs into one bundle.
func lsBuild(inputs []lsInput) *lsBundle {
	b := &lsBundle{Sources: []lsSource{}, Events: []lsEvent{}, Phases: []lsPhase{},
		Names: map[string]string{}}
	for i, in := range inputs {
		src, events, names := lsBuildSource(i, in)
		b.Sources = append(b.Sources, src)
		b.Events = append(b.Events, events...)
		for k, v := range names {
			b.Names[k] = v
		}
	}
	// Merge in time order. Stable, and tie-broken by source then line, so two records
	// written in the same microsecond on two nodes keep a deterministic order instead of
	// shuffling between requests.
	sort.SliceStable(b.Events, func(i, j int) bool {
		if b.Events[i].TS != b.Events[j].TS {
			return b.Events[i].TS < b.Events[j].TS
		}
		if b.Events[i].Src != b.Events[j].Src {
			return b.Events[i].Src < b.Events[j].Src
		}
		return b.Events[i].Line < b.Events[j].Line
	})
	lsApplyNames(b)
	lsGRPromoteMembers(b)
	b.Events = lsCollapse(b.Events)
	if len(b.Events) > lsMaxEvents {
		b.Events = b.Events[:lsMaxEvents]
	}
	for i := range b.Events {
		b.Events[i].No = i + 1
	}
	b.Summary = lsSummarise(b)
	b.Phases = lsBuildPhases(b)
	b.Finding = lsFindings(b)
	return b
}

// lsApplyNames rewrites member UUIDs in event text as node names.
//
// It has to run here rather than in the classifier: one node's log usually names only some
// of the members, and the name for the UUID in front of you is very often in a DIFFERENT
// file. Pooling the maps first and rewriting afterwards is what lets "119e686d-8943 is no
// longer in the group" become "pxc02 is no longer in the group" — which is the whole
// reason to read three logs together rather than one at a time.
func lsApplyNames(b *lsBundle) {
	if len(b.Names) == 0 {
		return
	}
	rewrite := func(s string) string {
		for uuid, name := range b.Names {
			if name == "" || !strings.Contains(s, uuid) {
				continue
			}
			// The FIRST occurrence only, and the UUID stays beside the name.
			//
			// Keeping the UUID matters because it is what appears in the raw log, and a
			// reader checking the classifier against the file has to be able to find the
			// line. Rewriting only the first occurrence matters because Galera's internal
			// records repeat a UUID three or four times —
			// "evs::proto(<uuid>, OPERATIONAL, view_id(REG,<uuid>,3))" — and expanding
			// every one turns a terse line into an unreadable one.
			s = strings.Replace(s, uuid, name+" ("+uuid+")", 1)
		}
		return s
	}
	for i := range b.Events {
		e := &b.Events[i]
		e.Message = rewrite(e.Message)
		if e.Peer != "" {
			if n, ok := b.Names[e.Peer]; ok && n != "" {
				e.Peer = n
			}
		}
	}
}

// lsBuildSource reads one log into events.
func lsBuildSource(idx int, in lsInput) (lsSource, []lsEvent, map[string]string) {
	data := string(in.Data)
	src := lsSource{
		Idx: idx, Name: in.Name, Path: in.Path, Origin: in.Origin,
		Bytes: len(in.Data), Lines: strings.Count(data, "\n") + 1,
		Offset: in.Offset, Counts: map[string]int{},
	}
	if src.Origin == "" {
		src.Origin = "upload"
	}
	src.Engine = in.Engine
	if src.Engine == "" {
		src.Engine = lsSniffEngine(data)
	}

	var events []lsEvent
	names := map[string]string{}
	switch src.Engine {
	case pktEngineMySQL:
		recs := lsFoldMySQL(data)
		src.Records = len(recs)
		src.Flavour = lsSniffFlavour(recs)
		src.Node = lsNodeName(recs)
		if src.Node == "" && src.Flavour == lsFlavourGroupRepl {
			src.Node = lsGRNodeName(recs)
		}
		names = lsUUIDNames(recs)
		for _, r := range recs {
			e, keep := lsClassifyMySQL(r)
			if !keep {
				continue
			}
			e.Src = idx
			events = append(events, e)
		}
	case pktEngineMongoDB:
		// A replica-set member is parsed here rather than by the shared classifier. The
		// facts that matter — newState/oldState, hostAndPort, the rollback counts — live
		// in the record's `attr` object, and pktLogEntry does not carry it. A standalone
		// mongod has none of them and keeps the shared path below.
		// Every mongod log is parsed here, replica-set member or not. The sniff decides the
		// FLAVOUR — which findings may speak about this source — and nothing else.
		//
		// It used to decide the parse as well, and a log that failed the sniff fell through
		// to the shared classifier, which has no severity filter for MongoDB: twenty
		// thousand records became twenty thousand events, all of class other, and the
		// verdict layer read them as a broken asynchronous replica. lsClassifyMongo keeps
		// what the catalogue recognises plus anything the server itself called a warning,
		// which is the right filter for a standalone mongod too.
		recs := lsFoldMongo(in.Data)
		src.Records = len(recs)
		src.Node = lsMongoNodeName(recs)
		if lsSniffMongoRS(recs) {
			src.Flavour = lsFlavourMongoRS
		} else {
			src.Flavour = src.Engine
		}
		for _, r := range recs {
			e, keep := lsClassifyMongo(r)
			if !keep {
				continue
			}
			e.Src = idx
			events = append(events, e)
		}
	default:
		// The other engines already have line classifiers, written for the Packet
		// Inspector's correlation pane. They are reused verbatim rather than reimplemented:
		// what the Log Summary adds for them is the shared timeline, the severity split and
		// the multi-source comparison, not a second parse of the same formats.
		entries := pktParseServerLog(in.Data, src.Engine)
		src.Records = len(entries)
		src.Flavour = src.Engine
		for i, en := range entries {
			e := lsFromPktEntry(en, i+1)
			e.Src = idx
			events = append(events, e)
		}
	}

	lsResolveSelf(src.Node, events)
	switch src.Flavour {
	case lsFlavourGalera:
		// Galera's own records carry the state; nothing to resolve.
	case lsFlavourGroupRepl:
		lsResolveGroupRepl(events)
	case lsFlavourMongoRS:
		lsResolveMongo(src.Node, events)
	default:
		lsResolveStandalone(events)
	}
	for i := range events {
		if events[i].TS == 0 {
			continue
		}
		if in.Offset != 0 {
			events[i].TS += in.Offset
			if events[i].EndTS > 0 {
				events[i].EndTS += in.Offset
			}
		}
		if src.FirstTS == 0 || events[i].TS < src.FirstTS {
			src.FirstTS = events[i].TS
		}
		if events[i].TS > src.LastTS {
			src.LastTS = events[i].TS
		}
		if events[i].Approx {
			src.Untimed++
		}
		src.Counts[events[i].Sev]++
	}
	src.Events = len(events)
	if src.Node == "" {
		src.Node = lsNodeFromName(in.Name)
	}
	return src, events, names
}

// lsResolveSelf turns the records in which a node names ITSELF into state evidence.
//
// Galera reports every member's progress to every member, so "Member 1.0 (pxc02) synced
// with group" appears in all three logs — and in pxc02's own it is a statement about
// pxc02. That distinction cannot be made inside the classifier, which sees one record at a
// time and does not know whose file it is in; here the source's own name is known.
//
// It matters because a log fragment can contain no transition at all. In the network-
// partition fixture pxc02 was SYNCED throughout and logged not one `Shifting` line, so
// without this it would sit at UNKNOWN for the whole incident — and the majority that
// stayed up would show as one node rather than two.
func lsResolveSelf(node string, events []lsEvent) {
	if node == "" {
		return
	}
	for i := range events {
		e := &events[i]
		if e.State != "" || e.Peer != node {
			continue
		}
		switch e.Label {
		case "Member synced with group":
			e.State = lsStateSynced
			e.Meaning = "This node reached SYNCED and is serving queries."
		case "Member desynced itself":
			e.State = lsStateDonor
		}
	}
}

// lsResolveStandalone gives a non-cluster server the only two states it has.
//
// A standalone MySQL or an asynchronous replica has no wsrep state machine, so the Galera
// vocabulary means nothing on it — and left to the shared phase builder every such node
// sat in CLOSED from its first start-up record onward, because nothing ever moved it out.
// A live three-node replication topology, entirely healthy, was reported as three servers
// that had not served a query in thirteen minutes.
//
// For these nodes the question is simply whether mysqld is up: `ready for connections` is
// the line that means yes, and a shutdown or a crash is what means no.
func lsResolveStandalone(events []lsEvent) {
	for i := range events {
		e := &events[i]
		switch {
		case e.Class == lsClassCrash && e.Sev == lsSevBad:
			e.State = lsStateDown
		case strings.HasPrefix(e.Label, "Server ready for connections"):
			e.State = lsStateUp
		case strings.HasPrefix(e.Label, "Server starting"):
			e.State = lsStateStarting
		case e.Label == "Shutdown complete":
			e.State = lsStateDown
		case e.Label == "Shutdown requested":
			e.State = lsStateStarting // stopping: up, but on its way out of service
		}
	}
}

// lsResolveGroupRepl is lsResolveStandalone's Group Replication counterpart, and differs
// from it in exactly one place that matters.
//
// A standalone server that reaches `ready for connections` is RUNNING — up and serving,
// which is the whole of what is being asked of it. A Group Replication member that reaches
// the same line is OFFLINE: mysqld is up, but the plugin is not, and nothing in the
// cluster's data will reach it. The corpus contains the case that makes this worth
// separating (g04-crash-kill9): systemd restarted a SIGKILLed member, mysqld came back and
// logged `ready for connections`, group_replication_start_on_boot was OFF so nothing
// rejoined, and the log said nothing further. Measured at that moment the server was
// writable and 666 transactions behind the group. Calling that RUNNING would have painted
// the lane green for the rest of the window.
//
// The plugin's own records move it on from there — MY-013587 to RECOVERING, MY-011490 to
// ONLINE — so a healthy start shows a brief OFFLINE stripe before it joins, which is
// exactly what was true.
func lsResolveGroupRepl(events []lsEvent) {
	for i := range events {
		e := &events[i]
		if e.State != "" {
			continue // a plugin record already said what state this is
		}
		switch {
		case e.Class == lsClassCrash && e.Sev == lsSevBad:
			e.State = lsStateDown
		case strings.HasPrefix(e.Label, "Server ready for connections"):
			e.State = lsStateOffline
		case strings.HasPrefix(e.Label, "Server starting"):
			e.State = lsStateStarting
		case e.Label == "Shutdown complete":
			e.State = lsStateDown
		case e.Label == "Shutdown requested":
			e.State = lsStateStarting
		}
	}
}

// lsNodeFromName falls back to the file name for a node's identity. An uploaded pxc02.err
// is named after the node far more often than not, and a label is better than a blank.
func lsNodeFromName(name string) string {
	base := name
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	for _, ext := range []string{".err", ".log", ".txt", ".gz"} {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// lsFromPktEntry adapts a Packet-Inspector log entry to a Log Summary event.
//
// The severity mapping is the interesting part: those classifiers were written to answer
// "which of these records could explain the packets I am looking at", so everything they
// recognise is a problem of some kind and everything they do not is `other`. That maps
// cleanly onto warn/bad, and leaves `ok` unused for those engines until they get a
// catalogue of their own like Galera's.
func lsFromPktEntry(en pktLogEntry, line int) lsEvent {
	sev := lsSevInfo
	class := lsClassOther
	switch en.Class {
	case pktLogAbort:
		sev, class = lsSevWarn, lsClassClient
	case pktLogAuth:
		sev, class = lsSevWarn, lsClassSecurity
	case pktLogDNS:
		sev, class = lsSevWarn, lsClassNetwork
	case pktLogListen:
		sev, class = lsSevBad, lsClassStartup
	case pktLogTLS:
		sev, class = lsSevWarn, lsClassSecurity
	case pktLogRepl:
		sev, class = lsSevBad, lsClassReplica
	case pktLogLifecycle:
		sev, class = lsSevWarn, lsClassStartup
		if strings.Contains(en.Label, "startup") || strings.Contains(en.Message, "ready for connections") {
			sev = lsSevOK
		}
	case pktLogCluster:
		sev, class = lsSevWarn, lsClassMember
	}
	sev = lsWorse(sev, lsLevelFloor(en.Level))
	return lsEvent{
		TS: en.TS, Line: line, Time: en.Time, Level: en.Level, Code: en.Code,
		Subsys: en.Subsys, Class: class, Sev: sev,
		Label: en.Label, Message: en.Message, Detail: en.Reason,
	}
}

// ---------------------------------------------------------------- summary

func lsSummarise(b *lsBundle) lsSummary {
	s := lsSummary{
		Sources: len(b.Sources), Events: len(b.Events),
		Counts: map[string]int{}, Classes: map[string]int{}, Top: []lsStat{},
	}
	type key struct{ label, class, sev string }
	counts := map[key]int{}
	for _, e := range b.Events {
		n := 1
		if e.Repeat > 1 {
			n = e.Repeat
		}
		s.Counts[e.Sev] += n
		s.Classes[e.Class] += n
		if e.Sev != lsSevInfo {
			counts[key{e.Label, e.Class, e.Sev}] += n
		}
		if e.TS > 0 {
			if s.FirstTS == 0 || e.TS < s.FirstTS {
				s.FirstTS = e.TS
			}
			if end := lsEndOf(e); end > s.LastTS {
				s.LastTS = end
			}
		}
	}
	for k, n := range counts {
		s.Top = append(s.Top, lsStat{Label: k.label, Class: k.class, Sev: k.sev, Count: n})
	}
	sort.Slice(s.Top, func(i, j int) bool {
		// Worst first, then most frequent: "one node crashed" must outrank "412 peer
		// timeouts", because it is the thing that caused them.
		if lsSevRank[s.Top[i].Sev] != lsSevRank[s.Top[j].Sev] {
			return lsSevRank[s.Top[i].Sev] > lsSevRank[s.Top[j].Sev]
		}
		if s.Top[i].Count != s.Top[j].Count {
			return s.Top[i].Count > s.Top[j].Count
		}
		return s.Top[i].Label < s.Top[j].Label
	})
	if len(s.Top) > 24 {
		s.Top = s.Top[:24]
	}
	s.Overlap, s.Disjoint = lsOverlap(b.Sources)
	return s
}

// lsOverlap is how much time every source has in common — the window in which a
// comparison between them means anything.
//
// Disjoint is the case worth naming: logs from different days, or from a node whose log
// was rotated before the incident. Two files that never overlap will still draw a
// perfectly plausible-looking timeline, and it will be a lie.
func lsOverlap(sources []lsSource) (float64, bool) {
	lo, hi := 0.0, 0.0
	n := 0
	for _, s := range sources {
		if s.FirstTS == 0 {
			continue
		}
		n++
		if lo == 0 || s.FirstTS > lo {
			lo = s.FirstTS
		}
		if hi == 0 || s.LastTS < hi {
			hi = s.LastTS
		}
	}
	if n < 2 {
		return 0, false
	}
	if hi <= lo {
		return 0, true
	}
	return hi - lo, false
}

// ---------------------------------------------------------------- phases

// lsBuildPhases turns each source's state-bearing events into a continuous track.
//
// Every phase runs to the start of the next one, and the last runs to the end of the
// bundle window — a log that simply stops is a node that carried on and had nothing to
// say, which on a database server is the definition of a good day. The exception is a
// track that ends in DOWN, which stays down.
func lsBuildPhases(b *lsBundle) []lsPhase {
	out := []lsPhase{}
	if len(b.Sources) == 0 {
		return out
	}
	start, end := b.Summary.FirstTS, b.Summary.LastTS
	if end <= start {
		end = start + 1
	}
	for _, src := range b.Sources {
		seed, inferred := lsSeedState(b, src.Idx)
		cur := lsPhase{Src: src.Idx, From: start, State: seed, Sev: lsStateSev(seed), Inferred: inferred}
		if seed == "UNKNOWN" {
			cur.Sev = lsSevInfo
		}
		var track []lsPhase
		for _, e := range b.Events {
			if e.Src != src.Idx || e.TS <= 0 {
				continue
			}
			next := cur
			next.From = e.TS
			// A server start wipes the slate: whatever state the previous run ended in
			// says nothing about this one, and neither does the membership it last saw.
			if e.Class == lsClassStartup && e.State == "" && strings.HasPrefix(e.Label, "Server starting") {
				next.State, next.Members, next.Primary = lsStateClosed, 0, ""
			} else {
				// Membership and primary-ness are part of the phase, not separate tracks:
				// "SYNCED, 2 members, primary" and "SYNCED, 3 members, primary" are two
				// different answers to "what was this node doing", and a reader asking
				// about an instant wants the one that was true THEN. Folding them into one
				// stripe would date-stamp the phase with whichever value happened last.
				if e.Members > 0 && (e.Class == lsClassQuorum || e.Class == lsClassMember) {
					next.Members = e.Members
				}
				if e.Primary != "" {
					next.Primary = e.Primary
				}
				if e.State != "" {
					next.State, next.Inferred = e.State, false
				}
				if next.State == lsStateDown || next.State == lsStateClosed {
					next.Members, next.Primary = 0, ""
				}
			}
			if next.State == cur.State && next.Members == cur.Members && next.Primary == cur.Primary {
				continue
			}
			next.Sev = lsStateSev(next.State)
			track = lsPushPhase(track, cur, e.TS)
			cur = next
		}
		cur.To = end
		track = append(track, cur)
		out = append(out, track...)
	}
	return out
}

// lsSeedState works out what state a source was in before its log says anything, and
// whether that answer was stated or deduced.
//
// A log is almost always a fragment, and this matters more than it sounds. A node that was
// already SYNCED when the excerpt begins never logs a transition INTO SYNCED — so without
// some answer here, the two members that stayed up through a partition both read as
// "unknown", and the one question the page exists to answer has no answer for exactly the
// nodes that were fine.
//
// Two answers, in order of how much they can be trusted:
//
//  1. Stated. The left-hand side of the first transition OUT of a state is not a guess:
//     `Shifting SYNCED -> DONOR/DESYNCED` says outright what the node was doing a moment
//     earlier.
//
//  2. Deduced, and flagged as such. A member that logs no state transition at ALL did not
//     change state during the window — every transient state (JOINER, JOINED, DONOR,
//     PRIMARY) necessarily ends, and would have logged its end. If such a node also
//     reports itself inside a primary component, SYNCED is the only state left. That is
//     sound, but it is reasoning rather than reading, so it is marked Inferred and the UI
//     says so rather than presenting it as something the file stated.
func lsSeedState(b *lsBundle, src int) (string, bool) {
	sawPrimary, sawAny := false, false
	for _, e := range b.Events {
		if e.Src != src {
			continue
		}
		sawAny = true
		// A start-up in the excerpt means the node was not in the cluster before it.
		if e.Class == lsClassStartup && strings.HasPrefix(e.Label, "Server starting") {
			return lsStateDown, false
		}
		if e.From != "" {
			return e.From, false
		}
		if e.State != "" {
			// A transition with no stated origin (a bare "Synced and serving") says
			// nothing about what came before it.
			return "UNKNOWN", false
		}
		if e.Primary == "yes" {
			sawPrimary = true
		}
	}
	if sawAny && sawPrimary {
		return lsStateSynced, true
	}
	// A server with no cluster records at all that is writing to its log is running: the
	// log is the evidence. Deduced, and flagged as such.
	for _, s := range b.Sources {
		if s.Idx == src && s.Flavour != lsFlavourGalera && s.Events > 0 {
			return lsStateUp, true
		}
	}
	return "UNKNOWN", false
}

// lsPushPhase closes a phase at t and appends it, dropping zero-length ones — a node can
// pass through PRIMARY and JOINER in the same microsecond and neither is worth a stripe.
func lsPushPhase(track []lsPhase, p lsPhase, t float64) []lsPhase {
	p.To = t
	if p.To <= p.From {
		return track
	}
	return append(track, p)
}

// lsSettledMS is the shortest a phase can be and still count as a state the node was IN,
// rather than a transition it was passing THROUGH.
//
// Galera walks several states in the same microsecond. Cutting a member off produced, in
// order and within 370 µs of each other: a view saying "1 member, non-primary" while the
// node was still nominally SYNCED, then NON-PRIMARY, then `Shifting SYNCED -> OPEN`. All
// three are real records and the phases between them are real, but a readout that lands in
// the first of them reports "SYNCED, 1 member, non-primary" — three facts that were
// momentarily all true and together describe nothing. Fifty milliseconds is far longer
// than any of those slivers and far shorter than any state worth reporting.
const lsSettledMS = 0.050

// lsStateAt answers "what was this source doing at t" from the phase track, skipping past
// the transitional slivers. Use this for anything a human reads; use lsPhaseAt when the
// literal phase covering an instant is what is wanted.
func lsStateAt(phases []lsPhase, src int, t float64) (lsPhase, bool) {
	p, ok := lsPhaseAt(phases, src, t)
	if !ok || p.To-p.From >= lsSettledMS {
		return p, ok
	}
	// Walk forward to the first phase that lasted long enough to mean something.
	best, found := p, false
	for _, q := range phases {
		if q.Src != src || q.From < p.From || q.To-q.From < lsSettledMS {
			continue
		}
		if !found || q.From < best.From {
			best, found = q, true
		}
	}
	if found {
		return best, true
	}
	return p, ok
}

// lsPhaseAt returns the phase literally covering t.
func lsPhaseAt(phases []lsPhase, src int, t float64) (lsPhase, bool) {
	for _, p := range phases {
		if p.Src == src && t >= p.From && t < p.To {
			return p, true
		}
	}
	// Past the end of the track, the last phase still applies.
	var last lsPhase
	found := false
	for _, p := range phases {
		if p.Src == src && (!found || p.To > last.To) {
			last, found = p, true
		}
	}
	return last, found
}

// ---------------------------------------------------------------- bucketing

// lsBucketise counts events per source over a time grid. The server does this so the
// browser never holds more than one page of a bundle that may be a hundred thousand
// events, exactly as the Packet Inspector's timeline does.
func lsBucketise(events []lsEvent, sources []lsSource, from, to float64, n int) []lsBucket {
	if n < 2 {
		n = 2
	}
	if to <= from {
		to = from + 1
	}
	width := (to - from) / float64(n)
	out := make([]lsBucket, 0, n*len(sources))
	index := map[[2]int]int{}
	for _, s := range sources {
		for i := 0; i < n; i++ {
			index[[2]int{s.Idx, i}] = len(out)
			out = append(out, lsBucket{Src: s.Idx, I: i, TS: from + float64(i)*width})
		}
	}
	for _, e := range events {
		if e.TS < from || e.TS > to {
			continue
		}
		i := int((e.TS - from) / width)
		if i >= n {
			i = n - 1
		}
		if i < 0 {
			i = 0
		}
		at, ok := index[[2]int{e.Src, i}]
		if !ok {
			continue
		}
		c := 1
		if e.Repeat > 1 {
			c = e.Repeat
		}
		b := &out[at]
		b.Count += c
		switch e.Sev {
		case lsSevOK:
			b.OK += c
		case lsSevWarn:
			b.Warn += c
		case lsSevBad:
			b.Bad += c
		default:
			b.Info += c
		}
	}
	return out
}
