package main

// logsummary_galera.go — what a Percona XtraDB Cluster node's log actually says.
//
// Every rule in this file was written against real logs from a real three-node PXC 8.0.46
// cluster, captured while doing the things that produce them: bootstrapping from scratch,
// killing a node with SIGKILL, restarting one cleanly, holding FLUSH TABLES WITH READ LOCK
// to force a desync, and cutting a member off the group-communication port with tc/netem
// for fifty seconds. The corpus is in app/testdata/logsummary/ and the tests read it.
//
// Three things that capture taught, which shape everything here:
//
//  1. The log LEVEL is not the severity. Across a complete crash, an eviction, a state
//     transfer and a rejoin there were 314 [Note] records, 5 [Warning], 8 [System] and no
//     [ERROR] at all. "declaring node with index 1 suspected", "suspected node without
//     join message, declaring inactive" and "Shifting SYNCED -> OPEN" are the whole story
//     of an outage and every one of them is a [Note].
//
//  2. A view record's `left {}` section does NOT mean "shut down cleanly". That was the
//     first guess and the corpus disproved it: across every fixture, including a plain
//     `systemctl restart mysql`, `left` is empty and the departing member appears under
//     `partitioned`, because the EVS layer sees a closed socket either way. What actually
//     separates maintenance from an incident is what the survivors logged in the seconds
//     BEFORE the view changed — a clean stop produces a view change about a millisecond
//     after the socket closes, while a death produces five seconds of reconnect attempts
//     and an evs.suspect_timeout first. lsFindingPartition decides on that evidence.
//
//  3. Flow control is very nearly invisible. With the default gcs.fc_debug the log records
//     no pause at all: the run that produced 91 ms of measured pause and ten received FC
//     messages wrote exactly one line, `Flow-control interval: [2, 2]`. What the log CAN
//     tell you is the interval itself — and an unusually low one means the cluster will
//     pause readily. lsFindingFlowControl says so rather than letting silence read as
//     health.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// lsRule recognises one family of records.
//
// Matching is: code first (exact, and stable across versions), then any `substr` in the
// header, then `re`. `bodySubstr` additionally requires a fragment in the folded
// continuation lines, which is how the two kinds of view change are told apart — their
// headers are identical.
type lsRule struct {
	codes      []string
	substr     []string
	bodySubstr []string
	re         *regexp.Regexp
	notSubstr  []string // a match here disqualifies the rule
	// needSubstr is a precondition on the header, the way bodySubstr is on the body: the
	// rule cannot match unless one of these is present, whatever else does match.
	//
	// It exists because Group Replication reuses the ordinary replication codes for its
	// own channels. `codes` and `substr` are alternatives — either can carry a match on
	// its own — so a rule saying "MY-010584, and only on a group_replication_ channel"
	// cannot be written with them: the code alone would fire it, and every asynchronous
	// replica's applier error would be reported as a Group Replication one.
	needSubstr []string

	class string
	sev   string
	label string
	means string

	// overLevel lets a rule keep its own severity when the record's level is HIGHER.
	//
	// Normally the level is a floor and nothing may sink below it: an unrecognised [ERROR]
	// must never be filed as background. That rule is right in every case the corpus
	// contains but one — MySQL Shell's instance checks, which are logged as [ERROR]
	// because a deliberate probe genuinely failed. Twenty-six of them appear in the log of
	// a healthy, brand-new InnoDB Cluster. Without this, every such cluster reports as
	// broken on the day it is built.
	//
	// Set it only where the catalogue has better evidence than the level does, and say why
	// at the rule.
	overLevel bool

	// enrich pulls structured facts out of a matched record.
	enrich func(r lsRecord, e *lsEvent)
}

// wsrep states, in the order a healthy join walks them. Only SYNCED serves queries;
// everything else either refuses them (1047, "WSREP has not yet prepared node for
// application use") or is deliberately out of the read pool.
const (
	lsStateSynced = "SYNCED"
	lsStateJoined = "JOINED"
	lsStateJoiner = "JOINER"
	lsStateDonor  = "DONOR"
	// Spelled PRIMARY-COMP, not PRIMARY. Galera's is the primary COMPONENT — the side of
	// a split that kept quorum — while MongoDB's PRIMARY is the one member accepting
	// writes. They are different ideas and a bundle can hold both, so the swimlane legend
	// cannot explain one word two ways.
	lsStatePrim   = "PRIMARY-COMP"
	lsStateOpen   = "OPEN"
	lsStateClosed = "CLOSED"
	lsStateDown   = "DOWN"
	// The two states a server that is NOT a cluster member has. A standalone MySQL or an
	// asynchronous replica has no wsrep state machine at all: "is it serving" is simply
	// whether mysqld is up and past `ready for connections`. Reusing the Galera vocabulary
	// for those nodes reported a perfectly healthy replication topology as three servers
	// that had been CLOSED — not serving — for thirteen minutes.
	lsStateUp       = "RUNNING"
	lsStateStarting = "STARTING"
)

// lsStateSev is how a state colours the timeline. This is the mapping that makes a
// three-node swimlane readable at a glance: green means "this node answers queries",
// amber means "it is up but not serving", red means "it is not part of a working cluster".
func lsStateSev(state string) string {
	switch state {
	case lsStateSynced, lsStateUp, lsStateOnline, lsStatePrimaryM, lsStateSecondary, lsStateRouting,
		lsStateStandby, lsStateVKReplica, lsStateOpLeader, lsStatePITRUp,
		lsStatePSMDBReady, lsStatePBMSlicing:
		return lsSevOK
	case lsStateJoined, lsStateJoiner, lsStateDonor, lsStatePrim, lsStateStarting, lsStateRecovering,
		lsStateStartup2, lsStateArbiter, lsStatePromoting, lsStateVKSyncing, lsStateVKLoading,
		lsStateOpFollower, lsStatePSMDBInit, lsStatePBMIdle:
		return lsSevWarn
	case lsStateOpen, lsStateClosed, lsStateDown, lsStateBlocked, lsStateGRError, lsStateOffline,
		lsStateRollback, lsStateRemoved, lsStateVKDown, lsStatePITRGap, lsStatePITRPaused,
		lsStatePSMDBErr, lsStatePBMLost:
		return lsSevBad
	}
	return lsSevInfo
}

// lsStateServes is the one question the unavailability finding asks of a state: was the
// server answering queries in it?
//
// Three states qualify and they belong to different worlds — SYNCED for a Galera member,
// ONLINE for a Group Replication one, RUNNING for a server that is not in a cluster at
// all. Treating "not SYNCED" as "not serving" is right for Galera and completely wrong for
// a standalone or an asynchronous replica, which never reaches SYNCED because it has no
// wsrep state machine at all.
//
// BLOCKED is deliberately not here even though a blocked member answers SELECTs. A member
// that has lost its majority serves reads of whatever it last applied and refuses every
// write; counting that as availability would report a cluster that cannot accept a single
// transaction as fully available, which is the opposite of what the page is for.
func lsStateServes(state string) bool {
	// ROUTING is a mongos: it has no data of its own, so "serving" means it was up and
	// willing to route. Leaving it out would report every router in a healthy cluster as
	// unavailable for the whole window, purely because it has no replica-set state.
	// STANDBY is a PostgreSQL standby: up, answering reads, refusing every write. It counts
	// as serving for the same reason SECONDARY does on a replica set — an application
	// reading from it is being served — and the write side is what the no-primary findings
	// are for.
	// REPLICA is a Valkey replica: up, following a primary, answering reads. It counts as
	// serving for the same reason SECONDARY and STANDBY do. CLUSTERDOWN deliberately does
	// not, and it is the interesting one — a Valkey Cluster node in that state is completely
	// healthy and refusing every command, including reads, because some other shard's slots
	// are uncovered. Counting a node that answers nothing as available would defeat the
	// purpose of the measurement.
	return state == lsStateSynced || state == lsStateUp || state == lsStateOnline ||
		state == lsStatePrimaryM || state == lsStateSecondary || state == lsStateRouting ||
		state == lsStateStandby || state == lsStateVKReplica
}

// lsStateMeaning explains a state once, for the timeline legend and the tooltip.
var lsStateMeaning = map[string]string{
	lsStateSynced:    "up to date with the group and serving queries",
	lsStateJoined:    "has the data but is still applying the backlog — flow control is holding the cluster back for it, and it is not in the read pool",
	lsStateJoiner:    "receiving a state transfer; it cannot answer queries at all",
	lsStateDonor:     "serving a state transfer to another member, and desynced from the group while it does",
	lsStatePrim:      "part of the primary component but not yet joined — the state a Galera node passes through on its way in",
	lsStateOpen:      "connected to no primary component: it will refuse queries with 1047 until it rejoins",
	lsStateClosed:    "the provider is shut down — the node is not in the cluster",
	lsStateDown:      "the server is not running",
	lsStateUp:        "up and accepting connections",
	lsStateStarting:  "starting up — not accepting connections yet",
	lsStateStandby:   "a PostgreSQL standby: replaying the primary's WAL, answering reads, and refusing every write",
	lsStatePromoting: "a promote was requested and has not finished — writes resume when it does",
	lsStateRouting:   "a mongos, up and routing — it holds no data of its own, so its lane says only that the router was running; the shards' health is in their own lanes",

	// Group Replication's states, spelled as replication_group_members spells them. Kept
	// in one map because the legend renders whatever states the bundle actually contains,
	// and a bundle can hold both kinds at once — a PXC cluster and a GR cluster in the
	// same incident is an ordinary thing to compare.
	lsStateOnline:     lsGRStateMeaning[lsStateOnline],
	lsStateRecovering: lsGRStateMeaning[lsStateRecovering],
	lsStateGRError:    lsGRStateMeaning[lsStateGRError],
	lsStateOffline:    lsGRStateMeaning[lsStateOffline],
	lsStateBlocked:    lsGRStateMeaning[lsStateBlocked],

	// Valkey's states. REPLICA is its own word for what MongoDB calls SECONDARY and
	// PostgreSQL calls STANDBY, kept separate because a reader looking at a Valkey node wants
	// the word `INFO replication` prints. CLUSTERDOWN has no counterpart anywhere else here.
	lsStateVKReplica: "a Valkey replica: following a primary and answering reads, with no way to know from the log how far behind it is",
	lsStateVKSyncing: "receiving a full copy of the primary's dataset — what it can answer while that runs is stale by an unbounded amount",
	lsStateVKLoading: "up and listening, and refusing every command with -LOADING while it reads its dataset off disk. A health check that only opens a socket sees a healthy node",
	lsStateVKDown:    "up, healthy, and refusing every command with CLUSTERDOWN because some other shard's hash slots are uncovered. Nothing is wrong with this node",

	// The two Kubernetes sources. Neither is a database and neither has a database's
	// states; what each has is the one question worth a coloured lane — was this process
	// the one actually in charge, and was point-in-time recovery actually being collected.
	lsStateOpLeader:   lsOpStateMeaning[lsStateOpLeader],
	lsStateOpFollower: lsOpStateMeaning[lsStateOpFollower],
	lsStatePITRUp:     lsOpStateMeaning[lsStatePITRUp],
	lsStatePITRGap:    lsOpStateMeaning[lsStatePITRGap],
	lsStatePITRPaused: lsOpStateMeaning[lsStatePITRPaused],
	lsStatePSMDBReady: lsPSMDBStateMeaning[lsStatePSMDBReady],
	lsStatePSMDBInit:  lsPSMDBStateMeaning[lsStatePSMDBInit],
	lsStatePSMDBErr:   lsPSMDBStateMeaning[lsStatePSMDBErr],
	lsStatePBMSlicing: lsPSMDBStateMeaning[lsStatePBMSlicing],
	lsStatePBMIdle:    lsPSMDBStateMeaning[lsStatePBMIdle],
	lsStatePBMLost:    lsPSMDBStateMeaning[lsStatePBMLost],

	// MongoDB's replica-set states, spelled as rs.status() spells them.
	lsStatePrimaryM:  lsMongoStateMeaning[lsStatePrimaryM],
	lsStateSecondary: lsMongoStateMeaning[lsStateSecondary],
	lsStateStartup2:  lsMongoStateMeaning[lsStateStartup2],
	lsStateRollback:  lsMongoStateMeaning[lsStateRollback],
	lsStateArbiter:   lsMongoStateMeaning[lsStateArbiter],
	lsStateRemoved:   lsMongoStateMeaning[lsStateRemoved],
}

// lsNullUUID is what Galera writes when there is no state at all — a first start on an
// empty datadir. Distinguishing it from a real UUID is what keeps a fresh bootstrap from
// being reported as three crashes.
const lsNullUUID = "00000000-0000-0000-0000-000000000000"

// ---------------------------------------------------------------- extractors

var (
	lsShifting   = regexp.MustCompile(`^(?:Shifting|Restored state) ([A-Z/]+) -> ([A-Z/]+)(?: \(TO: (-?\d+)\))?`)
	lsSrvStatus  = regexp.MustCompile(`^Server status change (\S+) -> (\S+)`)
	lsComponent  = regexp.MustCompile(`^New COMPONENT: primary = (yes|no), bootstrap = (yes|no), my_idx = (\d+), memb_num = (\d+)`)
	lsQuorumMemb = regexp.MustCompile(`members\s*=\s*(\d+)/(\d+)`)
	lsQuorumComp = regexp.MustCompile(`component\s*=\s*(\w+)`)
	lsMemberName = regexp.MustCompile(`\((\w[\w.-]*)\)`)
	lsFCInterval = regexp.MustCompile(`^Flow-control interval: \[(\d+), (\d+)\]`)
	lsFCQueue    = regexp.MustCompile(`^FC: queue size: (\d+)b \(\s*([\d.]+)% of soft limit\)`)
	lsSeqnoTail  = regexp.MustCompile(`:(\d+)\s*$`)
	lsUUIDShort  = regexp.MustCompile(`^\s*([0-9a-f]{8}-[0-9a-f]{4}),`)
	lsSuspectIdx = regexp.MustCompile(`declaring node with index (\d+) (suspected|inactive)`)
	lsPeerAddr   = regexp.MustCompile(`tcp://([\d.]+|[^:/]+):\d+`)
	lsSTDonor    = regexp.MustCompile(`Member \d+\.\d+ \(([^)]+)\) requested state transfer from '[^']*'\.\s*Selected \d+\.\d+ \(([^)]+)\)`)
	lsSTComplete = regexp.MustCompile(`\d+\.\d+ \(([^)]+)\): State transfer (to|from) \d+\.\d+ \(([^)]+)\) (complete|failed)`)
)

// lsViewKind reads PRIM or NON_PRIM out of the `view (view_id(KIND,…)` line.
//
// It decides whether the record is a statement about other members or about this one. A
// PRIM view is the surviving group saying who is no longer in it. A NON_PRIM view is a
// node saying it can no longer see the group — and its `partitioned` list then names
// everybody ELSE, which read naively turns one node leaving into "the whole cluster
// vanished". The departing node in the graceful-restart fixture writes exactly that.
var lsViewKind = regexp.MustCompile(`view_id\((PRIM|NON_PRIM),`)

func lsViewIsPrimary(body []string) (primary bool, known bool) {
	for _, line := range body {
		if m := lsViewKind.FindStringSubmatch(line); m != nil {
			return m[1] == "PRIM", true
		}
	}
	return false, false
}

// lsViewMembers reads a `view (...)` block's memb / joined / left / partitioned sections.
//
// This is the parser that earns the whole record-folding design: the header line says only
// "Current view of cluster as seen by this node", and every fact is below it.
//
// A note on `left`, because the obvious reading of it is wrong. Galera's view record has
// both a `left` and a `partitioned` section, and it is tempting to read them as "shut down
// cleanly" versus "crashed". On PXC 8.0.46 that is not what happens: across every fixture
// in testdata/logsummary — including a `systemctl restart mysql` — `left` is empty and the
// departing member is listed under `partitioned`, because the EVS layer sees the socket
// close either way. What actually separates the two is what came BEFORE the view change,
// and lsFindingPartition is where that is decided.
func lsViewMembers(body []string) (memb, joined, left, partitioned []string) {
	section := ""
	for _, line := range body {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "memb {"):
			section = "memb"
			continue
		case strings.HasPrefix(t, "joined {"):
			section = "joined"
			continue
		case strings.HasPrefix(t, "left {"):
			section = "left"
			continue
		case strings.HasPrefix(t, "partitioned {"):
			section = "partitioned"
			continue
		case t == "}" || t == ")":
			section = ""
			continue
		}
		m := lsUUIDShort.FindStringSubmatch(line)
		if m == nil || section == "" {
			continue
		}
		switch section {
		case "memb":
			memb = append(memb, m[1])
		case "joined":
			joined = append(joined, m[1])
		case "left":
			left = append(left, m[1])
		case "partitioned":
			partitioned = append(partitioned, m[1])
		}
	}
	return memb, joined, left, partitioned
}

// lsViewNames reads the named member list out of a `View:` block, which unlike the raw
// `view (...)` block carries node names rather than UUID prefixes:
//
//	members(3):
//		0: 0bc20092-…, pxc01
func lsViewNames(body []string) []string {
	var out []string
	in := false
	for _, line := range body {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "members(") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if strings.HasPrefix(t, "=====") || t == "" {
			break
		}
		if _, name, ok := strings.Cut(t, ", "); ok {
			out = append(out, name)
		}
	}
	return out
}

// lsShortUUID renders a member UUID the way the `view (...)` block does: the first group,
// a dash, and the FOURTH group. Galera abbreviates that way and only that way, so it is
// the join key between the two records that together name a member —
//
//	view (…)     memb { 119e686d-8943,0 }                       ← the short form
//	View:        1: 119e686d-9780-11f1-8943-029518c5b122, pxc02  ← the name
//
// Without the join a partition finding can only report "119e686d-8943 stopped answering",
// which is true and useless. With it, it says pxc02.
func lsShortUUID(full string) string {
	parts := strings.Split(full, "-")
	if len(parts) < 4 {
		return full
	}
	return parts[0] + "-" + parts[3]
}

// lsUUIDNames maps every member UUID a log names to its node name, from the `View:` blocks.
func lsUUIDNames(recs []lsRecord) map[string]string {
	out := map[string]string{}
	for _, r := range recs {
		for _, line := range r.Body {
			t := strings.TrimSpace(line)
			// "0: 0bc20092-9780-11f1-ac42-cbde0865291e, pxc01"
			_, rest, ok := strings.Cut(t, ": ")
			if !ok {
				continue
			}
			uuid, name, ok := strings.Cut(rest, ", ")
			if !ok || strings.Count(uuid, "-") != 4 || name == "" {
				continue
			}
			out[lsShortUUID(uuid)] = name
			out[uuid] = name
		}
	}
	return out
}

// ---------------------------------------------------------------- the catalogue
//
// Order matters: the first rule that matches wins, so the specific ones come first. The
// clearest case is `Shifting` — "SYNCED -> DONOR/DESYNCED" is a planned, expected, amber
// event and "SYNCED -> OPEN" is an outage, and a single rule on the word "Shifting" would
// have to call them the same thing.

var lsGaleraRules = []lsRule{
	// ---- the node is gone, or going ---------------------------------------------
	// The state matters as much as the record: without it the phase track keeps whatever
	// the node was doing when it died — a node that aborted mid-join read as JOINER for the
	// eleven minutes it was actually switched off.
	{substr: []string{"mysqld: Terminated."},
		class: lsClassCrash, sev: lsSevBad, label: "mysqld terminated",
		means:  "The server process ended here. Anything after this line is from a different run of mysqld.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State = lsStateDown }},
	{substr: []string{"Will never receive state. Need to abort."},
		class: lsClassCrash, sev: lsSevBad, label: "Aborting: will never receive state",
		means: "The node asked for a state transfer and the donor went away before it arrived, so it gave up and aborted. The service is down and will not come back on its own — it needs to be started again."},
	// The crash handler's own block. lsCrashHeader is what makes this line a record at
	// all — see the note there — and everything the handler printed after it lands in the
	// body, which is exactly the material a bug report needs.
	{substr: []string{"Assertion failure", "signal 11", "signal 6", "Segmentation fault", "got signal"},
		class: lsClassCrash, sev: lsSevBad, label: "Server crashed",
		means:  "mysqld died on a fatal signal and wrote its own crash block: build id, server version, the thread it died on, and a backtrace. Everything after this line until the restart belongs to the crash.",
		enrich: lsEnrichCrash},
	{substr: []string{"Inconsistency detected", "Inconsistent by consensus", "Node consistency compromised"},
		class: lsClassCrash, sev: lsSevBad, label: "Data inconsistency detected",
		means: "This node's copy of the data no longer agrees with the group's. Galera removes such a node rather than let it spread the divergence; it will need a full SST to come back."},
	// --wsrep-recover runs as a start-up step on EVERY start, including the first one on
	// an empty datadir, so its mere presence proves nothing. What distinguishes an unclean
	// stop is the position it recovered: a real UUID and a real sequence number means there
	// was uncommitted progress to find, while the null UUID with seqno -1 is what a brand
	// new node reports. The bootstrap fixture contains three of the harmless kind, and an
	// earlier version of this rule reported all three as crashes.
	{substr: []string{"Log of wsrep recovery"},
		class: lsClassStartup, sev: lsSevInfo, label: "wsrep position recovery ran",
		means: "mysqld ran --wsrep-recover before starting. This happens on every start; what matters is the position it found.",
		enrich: func(r lsRecord, e *lsEvent) {
			for _, b := range r.Body {
				i := strings.Index(b, "Recovered position")
				if i < 0 {
					continue
				}
				e.Message = strings.TrimSpace(b[i:])
				if m := lsSeqnoTail.FindStringSubmatch(e.Message); m != nil {
					e.Seqno, _ = strconv.ParseInt(m[1], 10, 64)
				}
				if strings.Contains(e.Message, lsNullUUID) || e.Seqno <= 0 {
					e.Message += " — nothing to recover; this was a first start or a clean one"
					return
				}
				e.Sev = lsSevWarn
				e.Label = "Restarted after an unclean stop"
				e.Meaning = "--wsrep-recover found a real position to recover, which means the previous mysqld did not shut down cleanly. The sequence number is how far it had got before it died."
			}
		}},

	// ---- clean lifecycle ---------------------------------------------------------
	{codes: []string{"MY-013172"}, substr: []string{"Received SHUTDOWN from user", "Received shutdown signal"},
		class: lsClassShutdown, sev: lsSevWarn, label: "Shutdown requested",
		means: "Somebody or something asked this server to stop. A member that leaves this way leaves cleanly, and the survivors will show it under `left` rather than `partitioned`."},
	{substr: []string{"Shutdown replication", "New SELF-LEAVE", "Received SELF-LEAVE"},
		class: lsClassShutdown, sev: lsSevWarn, label: "Left the cluster cleanly",
		means:  "The node announced its departure to the group before closing. No suspect timeout, no eviction — this is what a planned stop looks like from the inside.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State = lsStateClosed }},
	{substr: []string{"Shutdown complete"},
		class: lsClassShutdown, sev: lsSevInfo, label: "Shutdown complete",
		means:  "mysqld stopped cleanly.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State = lsStateDown }},
	{codes: []string{"MY-010931"}, substr: []string{"ready for connections. Version:"},
		class: lsClassStartup, sev: lsSevOK, label: "Server ready for connections",
		means: "mysqld is accepting client connections. On a cluster member this does NOT mean it will answer queries — that needs SYNCED as well."},
	{codes: []string{"MY-010116"}, substr: []string{"starting as process"},
		class: lsClassStartup, sev: lsSevInfo, label: "Server starting",
		means: "A new mysqld process. Every record above this belongs to a previous run."},
	{substr: []string{"Synchronized with group, ready for connections"},
		class: lsClassState, sev: lsSevOK, label: "Synced and serving",
		means:  "The node is caught up with the group and will now answer queries. This is the line to look for when asking 'was it actually available?'",
		enrich: func(_ lsRecord, e *lsEvent) { e.State = lsStateSynced }},
	{substr: []string{"PXC upgrade completed successfully"},
		class: lsClassStartup, sev: lsSevInfo, label: "PXC upgrade check completed"},

	// ---- quorum ------------------------------------------------------------------
	{substr: []string{"Received NON-PRIMARY", "Non-primary view"},
		class: lsClassQuorum, sev: lsSevBad, label: "Lost the primary component",
		means:  "This node can no longer see a majority of the cluster. It refuses every query with 1047 (\"WSREP has not yet prepared node for application use\") until it rejoins, whether or not it is otherwise healthy.",
		enrich: func(_ lsRecord, e *lsEvent) { e.Primary = "no" }},
	{substr: []string{"Quorum results:"},
		class: lsClassQuorum, sev: lsSevInfo, label: "Quorum result",
		means: "The group agreed on its membership. `members = a/b` is how many of the b known members are in the primary component.",
		enrich: func(r lsRecord, e *lsEvent) {
			body := r.bodyText()
			if m := lsQuorumMemb.FindStringSubmatch(body); m != nil {
				e.Members, _ = strconv.Atoi(m[1])
				e.Total, _ = strconv.Atoi(m[2])
			}
			comp := ""
			if m := lsQuorumComp.FindStringSubmatch(body); m != nil {
				comp = m[1]
			}
			if comp == "PRIMARY" {
				e.Primary, e.Sev = "yes", lsSevInfo
			} else if comp != "" {
				e.Primary, e.Sev = "no", lsSevBad
				e.Label = "Quorum result: " + comp
				e.Meaning = "The group did not form a primary component. Nothing in this partition will accept a write."
			}
			if e.Total > 0 {
				e.Message = fmt.Sprintf("%s — %d of %d members in the primary component", comp, e.Members, e.Total)
				if e.Members*2 <= e.Total && comp == "PRIMARY" {
					e.Sev = lsSevWarn
					e.Meaning = "The primary component holds no more than half the known members. One more loss and this side of the cluster stops accepting writes."
				}
			}
		}},
	{re: regexp.MustCompile(`^New COMPONENT: primary = no`),
		class: lsClassQuorum, sev: lsSevBad, label: "New component, not primary",
		means:  "A new group membership formed and it is not the primary one — this side of the split will not serve.",
		enrich: lsEnrichComponent},
	{substr: []string{"New COMPONENT: primary = yes"},
		class: lsClassQuorum, sev: lsSevInfo, label: "New component",
		means:  "A new group membership formed and it is the primary one.",
		enrich: lsEnrichComponent},
	{substr: []string{"WSREP has not yet prepared node for application use"},
		class: lsClassQuorum, sev: lsSevBad, label: "Query refused — node not ready",
		means: "A client's query was rejected with error 1047 because this node is not SYNCED. Any load balancer still sending it traffic is sending it into a wall."},
	{substr: []string{"Action message in non-primary configuration"},
		class: lsClassQuorum, sev: lsSevWarn, label: "Write attempted while non-primary",
		means: "Something tried to replicate while this node was outside the primary component. The write did not happen."},

	// ---- membership --------------------------------------------------------------
	// One rule for every view, because every view has the same header and the whole
	// distinction is in the body — which is why records are folded before classification.
	// lsEnrichView decides what this particular view is saying.
	{substr: []string{"Current view of cluster as seen by this node"},
		class: lsClassMember, sev: lsSevInfo, label: "Cluster view",
		means:  "The membership as this node sees it right now.",
		enrich: lsEnrichView},
	{re: regexp.MustCompile(`^={10,}$`), bodySubstr: []string{"members("},
		class: lsClassMember, sev: lsSevInfo, label: "View, by name",
		means: "The same membership again, with node names rather than UUID prefixes.",
		enrich: func(r lsRecord, e *lsEvent) {
			names := lsViewNames(r.Body)
			e.Members = len(names)
			status := ""
			for _, b := range r.Body {
				if s, ok := strings.CutPrefix(strings.TrimSpace(b), "status: "); ok {
					status = s
				}
			}
			if status == "non-primary" {
				e.Sev, e.Primary = lsSevBad, "no"
				e.Label = "View: non-primary"
			} else if status != "" {
				e.Primary = "yes"
			}
			if len(names) > 0 {
				e.Message = fmt.Sprintf("%d member(s): %s", len(names), strings.Join(names, ", "))
			}
		}},
	{substr: []string{"suspected node without join message, declaring inactive"},
		class: lsClassMember, sev: lsSevBad, label: "Peer declared inactive",
		means:  "A peer stopped answering and never sent a join message, so this node has written it off. A view change removing it follows within a second or two.",
		enrich: lsEnrichPeer},
	{substr: []string{"declaring node with index"},
		class: lsClassMember, sev: lsSevBad, label: "Peer suspected / inactive",
		means: "evs.suspect_timeout (5 s by default) or evs.inactive_timeout (15 s) expired without hearing from a peer. On a healthy network this never happens.",
		enrich: func(r lsRecord, e *lsEvent) {
			if m := lsSuspectIdx.FindStringSubmatch(r.Text); m != nil {
				e.Peer = "index " + m[1]
				if m[2] == "inactive" {
					e.Label = "Peer declared inactive"
				} else {
					e.Label = "Peer suspected"
					e.Sev = lsSevWarn
				}
			}
		}},
	{substr: []string{"evicting", "has been evicted", "Evicting node", "member has been evicted"},
		class: lsClassMember, sev: lsSevBad, label: "Member evicted",
		means:  "The group threw a member out. An evicted node will not rejoin until it is restarted, and on auto-eviction it was thrown out for being persistently slow rather than for being unreachable.",
		enrich: lsEnrichPeer},
	{substr: []string{"forgetting "},
		class: lsClassMember, sev: lsSevWarn, label: "Peer forgotten",
		means:  "The node dropped its record of a departed member.",
		enrich: lsEnrichPeer},
	{re: regexp.MustCompile(`^Server (\S+) synced with group$`),
		class: lsClassState, sev: lsSevOK, label: "Member synced with group",
		means: "This node reached SYNCED. Galera writes this line in the log of the node it is about, so here it is about the node that wrote it.",
		enrich: func(r lsRecord, e *lsEvent) {
			if m := regexp.MustCompile(`^Server (\S+) synced`).FindStringSubmatch(r.Text); m != nil {
				e.Peer = m[1]
			}
		}},
	{re: regexp.MustCompile(`^Member \d+\.\d+ \([^)]+\) synced with group`),
		class: lsClassMember, sev: lsSevOK, label: "Member synced with group",
		means:  "A member finished catching up and is now serving.",
		enrich: lsEnrichMemberName},
	{re: regexp.MustCompile(`^Member \d+\.\d+ \([^)]+\) desyncs itself`),
		class: lsClassState, sev: lsSevWarn, label: "Member desynced itself",
		means:  "A member took itself out of the group's flow control — normally FLUSH TABLES WITH READ LOCK, a backup, or wsrep_desync=ON. It is up, but it is not a valid read target and it is falling behind while this lasts.",
		enrich: lsEnrichMemberName},
	{re: regexp.MustCompile(`^Member \d+\.\d+ \([^)]+\) resyncs itself`),
		class: lsClassState, sev: lsSevOK, label: "Member resynced",
		means:  "A desynced member rejoined flow control and began catching up.",
		enrich: lsEnrichMemberName},
	{substr: []string{"gcomm: bootstrapping new group", "Starting new group from scratch",
		"Bootstrapping a new cluster"},
		class: lsClassMember, sev: lsSevWarn, label: "Bootstrapped a new cluster",
		means: "This node started a cluster rather than joining one — wsrep-new-cluster or safe_to_bootstrap. Correct exactly once, when the cluster is first created or when the whole cluster is being brought back up; done by accident, it creates a second cluster that will never merge with the first."},
	{substr: []string{"safe_to_bootstrap: 0"},
		class: lsClassMember, sev: lsSevInfo, label: "Not safe to bootstrap",
		means: "grastate.dat says this node was not the last one standing, so it must join an existing cluster rather than start one."},

	// ---- state transitions -------------------------------------------------------
	{re: lsShifting,
		class: lsClassState, sev: lsSevInfo, label: "State change",
		enrich: lsEnrichShifting},
	{re: lsSrvStatus,
		class: lsClassState, sev: lsSevInfo, label: "Server status change",
		enrich: func(r lsRecord, e *lsEvent) {
			m := lsSrvStatus.FindStringSubmatch(r.Text)
			e.Label = "Server status: " + m[1] + " → " + m[2]
			// The server-status track is a coarser view of the same machine, so where a
			// value maps onto a wsrep state it is recorded as one. That is what tells the
			// timeline a node was SYNCED before an excerpt began.
			e.From, e.State = lsServerStatusState(m[1]), lsServerStatusState(m[2])
			switch m[2] {
			case "synced":
				e.Sev, e.Meaning = lsSevOK, "The server is caught up and serving."
			case "donor":
				e.Sev, e.Meaning = lsSevWarn, "The server is streaming a state transfer to another member and is desynced while it does."
			case "joiner":
				e.Sev, e.Meaning = lsSevWarn, "The server is receiving a state transfer and cannot answer queries."
			case "disconnecting", "disconnected":
				e.Sev, e.Meaning = lsSevWarn, "The server is leaving the cluster."
			case "connected":
				if m[1] == "synced" {
					e.Sev, e.Meaning = lsSevBad, "The server dropped out of SYNCED back to merely connected — it has lost the primary component or is about to ask for a state transfer."
				}
			}
		}},

	// ---- state transfer ----------------------------------------------------------
	{substr: []string{"requested state transfer from"},
		class: lsClassTransfer, sev: lsSevWarn, label: "State transfer requested",
		means: "A member needs data from a donor before it can join. Which donor was picked matters: the donor is taken out of the read pool for the duration.",
		enrich: func(r lsRecord, e *lsEvent) {
			if m := lsSTDonor.FindStringSubmatch(r.Text); m != nil {
				e.Peer = m[1]
				e.Message = m[1] + " is joining; " + m[2] + " was chosen as donor"
			}
		}},
	{substr: []string{"Initiating SST/IST transfer on JOINER side"},
		class: lsClassTransfer, sev: lsSevWarn, label: "Joining: state transfer starting",
		means: "This node is about to receive a state transfer. It answers no queries until it finishes."},
	{substr: []string{"Initiating SST/IST transfer on DONOR side"},
		class: lsClassTransfer, sev: lsSevWarn, label: "Donating a state transfer",
		means: "This node is streaming its dataset (or a slice of it) to a joiner. It is desynced for the duration and should not be receiving client traffic."},
	// Written by the DONOR, about the joiner — so the node whose log this is read from is
	// the one that could not help, not the one that needed helping.
	{substr: []string{"IST first seqno", "falling back to SST"},
		class: lsClassTransfer, sev: lsSevBad, label: "IST impossible — falling back to SST",
		means: "This node was asked to serve an incremental transfer and its gcache no longer holds the writesets the joiner needs, so the cheap catch-up is off the table and a full physical copy of the dataset has to be streamed instead. On a large dataset that is the difference between seconds and hours. A bigger gcache.size is the fix."},
	{substr: []string{"Proceeding with SST", "Streaming the backup to joiner", "Waiting for SST streaming to complete"},
		class: lsClassTransfer, sev: lsSevWarn, label: "SST in progress",
		means: "A full physical copy of the dataset is crossing the network. Both ends are busy and the donor is out of the read pool."},
	{substr: []string{"async IST sender starting to serve", "IST sender", "Receiving IST", "IST request", "Prepared IST receiver"},
		class: lsClassTransfer, sev: lsSevInfo, label: "IST in progress",
		means:  "Incremental state transfer: the joiner is replaying the writesets it missed from the donor's gcache. This is the cheap path and the one you want.",
		enrich: lsEnrichSeqno},
	{substr: []string{"State transfer to", "State transfer from"},
		class: lsClassTransfer, sev: lsSevOK, label: "State transfer complete",
		means: "The joiner has the data. It still has to apply whatever arrived while the transfer ran before it reaches SYNCED.",
		enrich: func(r lsRecord, e *lsEvent) {
			m := lsSTComplete.FindStringSubmatch(r.Text)
			if m == nil {
				return
			}
			// Name both ends. Galera writes this record in every member's log, so the
			// source it was read from is NOT the node it is about — reporting the reader
			// as the participant is how a transfer between two other nodes gets attributed
			// to the one that merely watched.
			donor, joiner := m[1], m[3]
			if m[2] == "from" {
				donor, joiner = m[3], m[1]
			}
			e.Peer = joiner
			e.Message = donor + " → " + joiner
			if m[4] == "failed" {
				e.Sev, e.Label = lsSevBad, "State transfer FAILED"
				e.Meaning = "The transfer from " + donor + " to " + joiner + " did not complete. The joiner has no usable dataset and will abort or retry from the beginning; the donor spent the time for nothing."
			}
		}},
	{substr: []string{"SST received:", "IST received:", "SST completed", "Installed new state from SST"},
		class: lsClassTransfer, sev: lsSevOK, label: "State received",
		means:  "The node now holds the group's state as of this sequence number.",
		enrich: lsEnrichSeqno},
	{substr: []string{"Processing event queue:"},
		class: lsClassTransfer, sev: lsSevWarn, label: "Applying backlog",
		means: "The node has the data and is working through everything that happened while it was away. Until this reaches 100% it is JOINED, not SYNCED — it holds flow control on the whole cluster and serves nobody.",
		enrich: func(r lsRecord, e *lsEvent) {
			if strings.Contains(r.Text, "100.0%") {
				e.Sev, e.Label = lsSevOK, "Backlog applied"
			}
		}},
	{substr: []string{"State transfer required"},
		class: lsClassTransfer, sev: lsSevWarn, label: "State transfer required",
		means: "The node's position is behind or unrelated to the group's, so it cannot simply resume — it needs data from a donor first."},
	{substr: []string{"cannot be performed on a running server"},
		class: lsClassConfig, sev: lsSevWarn, label: "SST method needs a stopped server",
		means: "The configured wsrep_sst_method cannot run against a live server, so the provider has no fallback if the usual transfer fails. If that happens the node must be restarted by hand."},

	// ---- flow control ------------------------------------------------------------
	{re: lsFCQueue,
		class: lsClassFlowCtl, sev: lsSevInfo, label: "Flow control: receive queue",
		means: "The size of this node's unapplied writeset queue. It only appears at all when gcs.fc_debug is switched on.",
		enrich: func(r lsRecord, e *lsEvent) {
			m := lsFCQueue.FindStringSubmatch(r.Text)
			pct, _ := strconv.ParseFloat(m[2], 64)
			if pct >= 50 {
				e.Sev = lsSevWarn
				e.Meaning = "The receive queue is over half its soft limit — this node is close to pausing the whole cluster."
			}
		}},
	{re: lsFCInterval,
		class: lsClassFlowCtl, sev: lsSevInfo, label: "Flow-control interval",
		means: "The queue depth at which this node will pause the cluster. It is recalculated on every membership change and scales with the member count.",
		enrich: func(r lsRecord, e *lsEvent) {
			m := lsFCInterval.FindStringSubmatch(r.Text)
			hi, _ := strconv.Atoi(m[2])
			e.Message = fmt.Sprintf("pauses the cluster above %s queued writesets", m[2])
			// The default is gcs.fc_limit (100) scaled by member count — 141 for two
			// members, 173 for three. Anything far below that was configured that way,
			// and a cluster that pauses at 2 queued writesets will pause constantly.
			if hi > 0 && hi < 32 {
				e.Sev = lsSevWarn
				e.Meaning = "This threshold is far below the default. The cluster will apply flow control — pause every writer, everywhere — as soon as this node is a couple of writesets behind. Check gcs.fc_limit."
			}
		}},
	{substr: []string{"SST leaving flow control"},
		class: lsClassFlowCtl, sev: lsSevOK, label: "Flow control released after transfer",
		means: "The joiner stopped holding the cluster back."},
	{substr: []string{"Provider paused at", "Server paused at", "Desyncing and pausing the provider"},
		class: lsClassFlowCtl, sev: lsSevWarn, label: "Provider paused",
		means:  "Replication was paused on this node — usually FLUSH TABLES WITH READ LOCK or a backup taking a consistent snapshot. Writes elsewhere continue; this node stops applying them.",
		enrich: lsEnrichSeqno},
	{substr: []string{"Resuming and resyncing the provider", "Provider resumed"},
		class: lsClassFlowCtl, sev: lsSevOK, label: "Provider resumed",
		means: "The node started applying again and is catching up."},

	// ---- network -----------------------------------------------------------------
	{substr: []string{"(gmcast.peer_timeout)"},
		class: lsClassNetwork, sev: lsSevWarn, label: "Peer went quiet",
		means:  "No group-communication message from a peer for gmcast.peer_timeout (3 s). One of these is a hiccup; a run of them is a network problem or a peer that is gone.",
		enrich: lsEnrichPeer},
	{substr: []string{"turning message relay requesting on"},
		class: lsClassNetwork, sev: lsSevWarn, label: "Relaying messages for unreachable peers",
		means:  "This node can no longer talk to some peer directly and is asking the others to relay for it. The cluster still works; the network under it does not.",
		enrich: lsEnrichPeer},
	{substr: []string{"turning message relay requesting off"},
		class: lsClassNetwork, sev: lsSevOK, label: "Direct peer connectivity restored",
		means: "Relaying is no longer needed — every peer is directly reachable again."},
	{substr: []string{"Failed to establish connection: Connection refused"},
		class: lsClassNetwork, sev: lsSevWarn, label: "Peer refused the connection",
		means:  "Something answered on the peer's address and refused: the host is up but mysqld is not listening on 4567. Usually the peer is stopped or still starting.",
		enrich: lsEnrichPeer},
	{substr: []string{"Failed to establish connection"},
		class: lsClassNetwork, sev: lsSevWarn, label: "Could not connect to a peer",
		means:  "A group-communication connection attempt failed.",
		enrich: lsEnrichPeer},
	{substr: []string{"reconnecting to "},
		class: lsClassNetwork, sev: lsSevWarn, label: "Reconnecting to a peer",
		enrich: lsEnrichPeer},
	{substr: []string{"connection established to"},
		class: lsClassNetwork, sev: lsSevOK, label: "Peer connected",
		means:  "A group-communication link came up.",
		enrich: lsEnrichPeer},
	{substr: []string{"declaring ", " stable"},
		class: lsClassNetwork, sev: lsSevOK, label: "Peer declared stable",
		means:  "A peer has been answering consistently and is now trusted as part of the view.",
		enrich: lsEnrichPeer},
	{substr: []string{"pc.wait_restored_prim_timeout is set to 0"},
		class: lsClassNetwork, sev: lsSevWarn, label: "Will wait forever for a primary component",
		means: "If this node cannot find a primary component it will wait indefinitely rather than give up — which looks exactly like a hung start-up. Set pc.wait_restored_prim_timeout if that is not what you want."},

	// ---- the MySQL server underneath ---------------------------------------------
	{codes: []string{"MY-014084"}, substr: []string{"unable to reserve space in redo log"},
		class: lsClassStorage, sev: lsSevWarn, label: "Redo log too small for the write rate",
		means: "Writers are stalling because the redo log filled faster than the checkpointer could free it. On a cluster member this shows up as apply lag and then as flow control. Raise innodb_redo_log_capacity."},
	{codes: []string{"MY-010914", "MY-013104", "MY-013130"}, substr: []string{"Aborted connection"},
		class: lsClassClient, sev: lsSevWarn, label: "Aborted connection",
		means: "A client connection ended without a clean QUIT. The reason in brackets is the server's own diagnosis.",
		enrich: func(r lsRecord, e *lsEvent) {
			if m := pktLogReason.FindStringSubmatch(r.Text); m != nil {
				e.Message = strings.TrimSpace(m[1])
			}
		}},
	{codes: []string{"MY-010055", "MY-010056", "MY-010057", "MY-010058"},
		class: lsClassClient, sev: lsSevWarn, label: "Client address could not be resolved",
		means: "Reverse DNS for a connecting client failed. Every connection pays the timeout; skip_name_resolve avoids it entirely."},
	{substr: []string{"Access denied for user", "is blocked because of many connection errors"},
		class: lsClassSecurity, sev: lsSevWarn, label: "Authentication refused"},
	{substr: []string{"Too many connections"},
		class: lsClassClient, sev: lsSevBad, label: "Too many connections",
		means: "max_connections was reached and clients were turned away."},
	{substr: []string{"Deadlock found", "Lock wait timeout exceeded"},
		class: lsClassConflict, sev: lsSevWarn, label: "Lock conflict"},
	{substr: []string{"WSREP: BF Abort", "cluster conflict", "Certification failed"},
		class: lsClassConflict, sev: lsSevWarn, label: "Certification conflict",
		means: "Two members changed the same rows at once and one transaction had to be rolled back. Expected under multi-master writes; a lot of them means the write pattern should be pinned to one node."},
	{codes: []string{"MY-013602", "MY-010068"}, substr: []string{"self signed", "configured to support TLS"},
		class: lsClassSecurity, sev: lsSevInfo, label: "TLS configuration"},
	{codes: []string{"MY-011070"}, substr: []string{"is deprecated and will be removed"},
		class: lsClassConfig, sev: lsSevInfo, label: "Deprecated setting"},
	{codes: []string{"MY-010097"}, substr: []string{"Insecure configuration"},
		class: lsClassConfig, sev: lsSevWarn, label: "Insecure configuration"},
	{codes: []string{"MY-013712"}, substr: []string{"No suitable", "service implementation found"},
		class: lsClassConfig, sev: lsSevInfo, label: "Optional component not present"},
	{codes: []string{"MY-010453"}, substr: []string{"created with an empty password"},
		class: lsClassSecurity, sev: lsSevWarn, label: "root created with an empty password",
		means: "The server was initialised with --initialize-insecure, so root@localhost has no password at all until somebody sets one. Anyone who can reach the socket is root."},
	{codes: []string{"MY-013169"}, substr: []string{"initializing of server in progress"},
		class: lsClassStartup, sev: lsSevInfo, label: "Data directory being initialised",
		means: "A first start on an empty datadir. Every record before the next `starting as process` belongs to initialisation, not to a running server."},
	{codes: []string{"MY-010454"}, substr: []string{"A temporary password is generated"},
		class: lsClassSecurity, sev: lsSevWarn, label: "Temporary root password in the log",
		means: "The initial root password was written to the error log in clear text. Anyone who can read this file can read that password."},
	{substr: []string{"Passing config to GCS:"},
		class: lsClassConfig, sev: lsSevInfo, label: "Galera provider configuration",
		means:  "The full wsrep_provider_options this node started with — the reference for every timeout and limit the rest of the log mentions.",
		enrich: func(r lsRecord, e *lsEvent) { e.Message = "wsrep_provider_options (full text in the detail)" }},
}

// ---------------------------------------------------------------- enrichers

func lsEnrichComponent(r lsRecord, e *lsEvent) {
	m := lsComponent.FindStringSubmatch(r.Text)
	if m == nil {
		return
	}
	e.Primary = m[1]
	e.Members, _ = strconv.Atoi(m[4])
	e.Message = fmt.Sprintf("%d member(s), primary = %s", e.Members, m[1])
	if m[2] == "yes" {
		e.Message += ", bootstrapped"
	}
}

// lsEnrichView reads a view record and decides what it is a statement ABOUT.
//
// Three different records share this one header, and telling them apart is most of what
// makes a three-node bundle legible:
//
//	PRIM view, members lost      the surviving group reporting who is no longer in it
//	NON_PRIM view, alone         this node reporting that it can no longer see the group
//	PRIM view, nobody lost       routine — a join, or the membership restated
//
// The second is the one that must not be read as the first. When a node leaves or is cut
// off it writes a NON_PRIM view whose `partitioned` list names every OTHER member, and
// taking that at face value turns one node's departure into "the entire cluster vanished".
// The graceful-restart fixture contains exactly that record.
func lsEnrichView(r lsRecord, e *lsEvent) {
	memb, joined, left, part := lsViewMembers(r.Body)
	primary, known := lsViewIsPrimary(r.Body)
	e.Members = len(memb)
	if known {
		if primary {
			e.Primary = "yes"
		} else {
			e.Primary = "no"
		}
	}
	var bits []string
	bits = append(bits, fmt.Sprintf("%d member(s)", len(memb)))
	if len(joined) > 0 {
		bits = append(bits, "joined: "+strings.Join(joined, ", "))
	}
	if len(left) > 0 {
		bits = append(bits, "announced departure: "+strings.Join(left, ", "))
	}

	if known && !primary {
		// This node's own loss of the group. Its `partitioned` list is everyone else, so
		// it says nothing about whether THEY are healthy — only that this node cannot
		// reach them.
		e.Sev, e.Label = lsSevBad, "Alone in the cluster"
		e.Meaning = "This node can no longer see a majority. It is non-primary and will refuse every query with 1047 until it rejoins. The members it lists as unreachable may be perfectly healthy and talking to each other."
		if len(memb) > 1 {
			e.Label = "Cut off with a minority"
			e.Meaning = "This node is in a group too small to be the primary component. Nothing on this side will accept a write."
		}
		if len(part) > 0 {
			bits = append(bits, "cannot reach: "+strings.Join(part, ", "))
		}
		e.Message = strings.Join(bits, " · ")
		return
	}

	// A primary view. Now `partitioned` IS a statement about other members: the group
	// carried on and these are the ones no longer in it.
	e.Lost, e.Left = part, left
	if len(part) > 0 {
		bits = append(bits, "no longer in the group: "+strings.Join(part, ", "))
		e.Sev, e.Label = lsSevWarn, "Member(s) left the group"
		e.Meaning = "The surviving members re-formed without these. Whether that was a shutdown or a failure depends on what this node logged in the seconds before — a suspect timeout means it was a failure."
	}
	e.Message = strings.Join(bits, " · ")
}

// lsJoinOrder ranks the states along the path a node walks from "not in the cluster" to
// "serving". It exists so a transition can be read as progress or as regression, which is
// what decides whether it is news.
//
// Without it every start-up looks like an incident: CLOSED → OPEN lands in a state that is
// genuinely bad to be IN — a node in OPEN answers nothing — but arriving there from CLOSED
// is a node starting normally, and colouring it red buries the one transition that is a
// real fault, SYNCED → OPEN.
var lsJoinOrder = map[string]int{
	lsStateDown: 0, lsStateClosed: 1, lsStateOpen: 2, lsStatePrim: 3,
	lsStateJoiner: 4, lsStateJoined: 5, lsStateDonor: 5, lsStateSynced: 6,
}

func lsEnrichShifting(r lsRecord, e *lsEvent) {
	m := lsShifting.FindStringSubmatch(r.Text)
	if m == nil {
		return
	}
	from, to := m[1], m[2]
	e.From, e.State = lsNormaliseState(from), lsNormaliseState(to)
	e.Label = "State: " + from + " → " + to
	if m[3] != "" {
		e.Seqno, _ = strconv.ParseInt(m[3], 10, 64)
	}
	if s, ok := lsStateMeaning[e.State]; ok {
		e.Meaning = "The node is now " + s + "."
	}
	fromRank, fromKnown := lsJoinOrder[e.From]
	toRank := lsJoinOrder[e.State]
	switch {
	case e.State == lsStateSynced:
		e.Sev = lsSevOK
	case fromKnown && toRank > fromRank:
		// Moving up the join path: this is a node getting closer to serving.
		e.Sev = lsSevInfo
	default:
		e.Sev = lsStateSev(e.State)
	}
	// Falling out of SYNCED is the transition worth flagging hardest: whatever the
	// destination, the node has stopped serving.
	if e.From == lsStateSynced && e.State != lsStateDonor && lsStateSev(e.State) == lsSevBad {
		e.Label = "Dropped out of SYNCED"
		e.Sev = lsSevBad
	}
}

// lsServerStatusState maps the `Server status change` vocabulary onto the wsrep states
// the timeline draws. Only the values with an unambiguous equivalent are mapped; the
// transitional ones (initializing, initialized) are left blank rather than guessed at.
func lsServerStatusState(s string) string {
	switch s {
	case "synced":
		return lsStateSynced
	case "joined":
		return lsStateJoined
	case "joiner":
		return lsStateJoiner
	case "donor":
		return lsStateDonor
	case "disconnected":
		return lsStateClosed
	}
	return ""
}

// lsNormaliseState maps Galera's spellings onto the small set the timeline draws.
func lsNormaliseState(s string) string {
	switch s {
	case "DONOR/DESYNCED", "DONOR", "DESYNCED":
		return lsStateDonor
	case "SYNCED", "JOINED", "JOINER", "PRIMARY", "PRIMARY-COMP", "OPEN", "CLOSED":
		return s
	}
	return s
}

// lsCrashSignal / lsCrashQuery / lsCrashVersion read the facts out of a crash block.
//
// The signal number is the first question ("11 — a segfault; 6 — an assertion the server
// raised on itself"), and the query is the second: when mysqld dies inside a statement the
// handler prints it, and that statement is usually the whole bug report.
var (
	lsCrashSignal  = regexp.MustCompile(`got signal (\d+)`)
	lsCrashQuery   = regexp.MustCompile(`(?m)^Query \([^)]*\):\s*(.+)$`)
	lsCrashVersion = regexp.MustCompile(`(?m)^Server Version:\s*(.+)$`)
	lsCrashThread  = regexp.MustCompile(`(?m)^Connection ID \(thread ID\):\s*(\d+)`)
)

// lsSignalMeaning names the signals a database actually dies on.
var lsSignalMeaning = map[string]string{
	"11": "SIGSEGV — the server dereferenced memory it does not own. Almost always a bug; occasionally failing RAM.",
	"6":  "SIGABRT — the server aborted itself, normally because an assertion or a consistency check failed.",
	"7":  "SIGBUS — a bad memory access, which on a database usually means the storage under a mapped file went away.",
	"8":  "SIGFPE — an arithmetic fault.",
	"4":  "SIGILL — an illegal instruction, which on a server that was running fine usually means a binary built for a different CPU.",
	"9":  "SIGKILL — the process was killed outright, most often by the OOM killer. Nothing in the server chose this.",
	"15": "SIGTERM — an orderly stop was requested. If the server crashed instead of stopping, look at what it was doing.",
}

func lsEnrichCrash(r lsRecord, e *lsEvent) {
	body := r.bodyText()
	if m := lsCrashSignal.FindStringSubmatch(r.Text); m != nil {
		e.Label = "Server crashed — signal " + m[1]
		if s, ok := lsSignalMeaning[m[1]]; ok {
			e.Meaning = s + " " + e.Meaning
		}
	}
	var bits []string
	if m := lsCrashVersion.FindStringSubmatch(body); m != nil {
		bits = append(bits, strings.TrimSpace(m[1]))
	}
	if m := lsCrashThread.FindStringSubmatch(body); m != nil {
		bits = append(bits, "connection "+m[1])
	}
	if m := lsCrashQuery.FindStringSubmatch(body); m != nil {
		bits = append(bits, "running: "+truncate(strings.TrimSpace(m[1]), 160))
	} else if strings.Contains(body, "Thread pointer: 0x0") {
		// No thread and no query: the server died in a background thread, so there is no
		// statement to blame and the backtrace is all there is.
		bits = append(bits, "died in a background thread — no statement was running")
	}
	// A backtrace with every frame <unknown> is the common case on a stripped package
	// build, and saying so saves the reader wondering whether the tool lost it.
	if n := strings.Count(body, "<unknown>"); n > 0 && n >= strings.Count(body, "\n#") {
		bits = append(bits, "backtrace unresolved (no debug symbols installed)")
	}
	if len(bits) > 0 {
		e.Message = strings.Join(bits, " · ")
	}
	// The process is gone; the phase track has to know, or the node keeps whatever state
	// it was in until the next start-up record.
	e.State = lsStateDown
}

func lsEnrichSeqno(r lsRecord, e *lsEvent) {
	if m := lsSeqnoTail.FindStringSubmatch(strings.TrimSpace(r.Text)); m != nil {
		e.Seqno, _ = strconv.ParseInt(m[1], 10, 64)
	}
}

func lsEnrichPeer(r lsRecord, e *lsEvent) {
	if m := lsPeerAddr.FindStringSubmatch(r.Text); m != nil {
		e.Peer = m[1]
	}
}

func lsEnrichMemberName(r lsRecord, e *lsEvent) {
	if m := lsMemberName.FindStringSubmatch(r.Text); m != nil {
		e.Peer = m[1]
	}
}

// ---------------------------------------------------------------- classification

// lsNoise are records that exist in enormous numbers and say nothing an operator can act
// on: internal bookkeeping, thread lifecycle, monitor drains. They are dropped rather than
// classified as "other", because a three-node bundle is ~3000 lines and two thirds of them
// are these.
//
// Dropping is safe here in a way it would not be in a log viewer, because the Log Summary
// always offers the raw file alongside: nothing is hidden, it is only not promoted.
var lsNoise = []string{
	"wsrep_notify_cmd is not defined",
	"Service thread queue flushed",
	"STATE EXCHANGE:", "STATE_EXCHANGE:",
	"#######",
	"Maybe drain monitors", "Drain monitors", "Draining apply monitors",
	"REPL Protocols:", "Skipping cert index reset", "Cert index reset to",
	"Recording CC from", "Lowest cert index boundary", "Min available from gcache",
	"Starting applier thread", "Starting rollbacker thread", "Applier thread exiting",
	"rollbacker thread exiting", "Slave thread exit", "recv_thread() joined",
	"Waiting for active wsrep applier", "Waiting for rollback thread",
	"All applier thread terminated", "Rollback thread terminated",
	"GCache::RingBuffer", "GCache DEBUG", "Created page /var/lib/mysql/gcache.page",
	"Resolved symbol", "Symbol '", "wsrep_load()", "CRC-32C:", "protonet asio version",
	"Using CRC-32C", "backend: asio", "gcomm thread scheduling priority",
	"GMCast version", "EVS version", "PC protocol", "Changing maximum packet size",
	"discarding pending addr", "deleting entry", "Node ", "Save the discovered primary-component",
	"Opened channel", "Loading provider", "Setting GCS initial position",
	"Detected STR version", "Seqno ", "Cert index preload", "Cert. index preload",
	"str_proto_ver_", "mon: entered", "MemPool(", "avg deps dist", "avg cert interval",
	"cert index usage", "cert trx map usage", "deps set usage", "cert index size",
	"wsdb trx map usage", "dtor state:", "Flushing memory map",
	"Closing send", "Closing receive", "Closed send", "RECV thread exiting",
	"wsrep_init_schema_and_SR", "Initialized wsrep sidno", "Server initialized",
	"Cluster table is empty", "clear restored view", "start_prim is enabled",
	"Recovering GCache ring buffer", "Skipped GCache ring buffer recovery",
	"Generated new GCache ID", "GCache history reset", "Resetting GCache seqno map",
	"Process first view", "Recovered cluster id", "Recovered position from storage",
	"Recovered view from SST", "First IST (CC) event", "Found matching local endpoint",
	"Connecting with bootstrap option", "Could not open state file",
	"No persistent state found", "Fail to access the file", "Restoring primary-component",
	"listening at tcp://", "multicast: ", "gcomm: connect", "gcomm: term", "gcomm: join",
	"gcomm: clos", "Flushing tables", "DONOR thread signaled",
	"X Plugin ready for connections", "InnoDB initialization has",
	"Running post-processing", "post-processing done", "Skipping mysql_upgrade",
	"Decompressing the backup", "Preparing the backup", "Moving the backup",
	"Waiting for server instance to start", "Galera co-ords from recovery",
	"Processing SST received", "SST request is null", "Prepared SST request",
	"Check if state gap can be serviced", "IST receiver addr using",
	"Node is running in bootstrap/initialize mode", "Forcing close of thread",
	"xtrabackup_ist received from donor", "Bypassing SST",
}

func lsIsNoise(text string) bool {
	for _, n := range lsNoise {
		if strings.Contains(text, n) {
			return true
		}
	}
	return false
}

// lsMySQLRules is the whole MySQL-family catalogue: Group Replication's rules first, then
// the asynchronous-replication ones, then Galera's.
//
// One ordered table rather than a per-flavour dispatch, because the vocabularies genuinely
// coexist. A PXC member can also be an asynchronous replica of another cluster — that is
// what a cross-cluster channel is — so its log carries [Galera] and [Repl] records
// together, and a Group Replication member can be an asynchronous replica too.
//
// The order is not cosmetic. Group Replication reuses the ordinary replication codes for
// its own channels — MY-010584 for an applier failure, MY-014002 for a receiver connecting
// — distinguished only by the channel name. Put the GR rules second and an applier
// conflict inside a group would be reported as a broken asynchronous replica, naming a
// channel nobody configured. The GR rules that claim a shared code all require
// "group_replication_" in the record, so they never steal a genuine asynchronous one.
var lsMySQLRules = append(append(append([]lsRule{}, lsGroupRules...), lsReplRules...), lsGaleraRules...)

// lsClassifyMySQL turns one folded record into an event, or reports that it is noise.
func lsClassifyMySQL(r lsRecord) (lsEvent, bool) {
	e := lsEvent{
		TS: r.TS, Approx: r.Approx, Line: r.Line, Time: r.Time, Level: r.Level,
		Code: r.Code, Subsys: r.Subsys, Message: r.Text,
		Class: lsClassOther, Sev: lsSevInfo, Label: r.Text,
	}
	if len(r.Body) > 0 {
		e.Detail = strings.Join(r.Body, "\n")
	}
	for _, rule := range lsMySQLRules {
		if !lsRuleMatches(rule, r) {
			continue
		}
		e.Class, e.Sev, e.Label = rule.class, rule.sev, rule.label
		e.Meaning = rule.means
		if rule.enrich != nil {
			rule.enrich(r, &e)
		}
		// A record the catalogue calls background but the server itself called an error
		// is an error: an unrecognised [ERROR] must never be filed as info. The exception
		// is a rule that knows better than the level — see lsRule.overLevel.
		if !rule.overLevel {
			e.Sev = lsWorse(e.Sev, lsLevelFloor(r.Level))
		}
		return e, true
	}
	// Unmatched. An ERROR or a Warning is still worth showing; the rest is noise.
	if floor := lsLevelFloor(r.Level); lsSevRank[floor] >= lsSevRank[lsSevWarn] {
		e.Sev = floor
		e.Label = lsTruncateLabel(r.Text)
		e.Class = lsClassOther
		return e, true
	}
	if lsIsNoise(r.Text) {
		return lsEvent{}, false
	}
	e.Label = lsTruncateLabel(r.Text)
	return e, true
}

// lsLevelFloor is the minimum severity a record's own level justifies. [Note] contributes
// nothing — on a Galera node it is the level of almost everything, including the outage.
func lsLevelFloor(level string) string {
	switch strings.ToUpper(level) {
	case "ERROR":
		return lsSevBad
	case "WARNING":
		return lsSevWarn
	}
	return lsSevInfo
}

// lsTruncateLabel keeps an unrecognised message usable as a row title. The full text is
// always in the row's message, so nothing is lost.
func lsTruncateLabel(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 90 {
		return s
	}
	return s[:87] + "…"
}

func lsRuleMatches(rule lsRule, r lsRecord) bool {
	for _, n := range rule.notSubstr {
		if strings.Contains(r.Text, n) {
			return false
		}
	}
	if len(rule.needSubstr) > 0 {
		found := false
		for _, s := range rule.needSubstr {
			if strings.Contains(r.Text, s) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	// bodySubstr is a precondition, not an alternative: a view record only counts as a
	// partition when `partitioned {` is actually in the body.
	if len(rule.bodySubstr) > 0 {
		body := r.bodyText()
		found := false
		for _, b := range rule.bodySubstr {
			if strings.Contains(body, b) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, c := range rule.codes {
		if r.Code != "" && r.Code == c {
			return true
		}
	}
	for _, s := range rule.substr {
		if strings.Contains(r.Text, s) {
			return true
		}
	}
	if rule.re != nil && rule.re.MatchString(r.Text) {
		return true
	}
	// A rule with only a bodySubstr (and no header matcher) matches on the body alone.
	return len(rule.codes) == 0 && len(rule.substr) == 0 && rule.re == nil && len(rule.bodySubstr) > 0
}
