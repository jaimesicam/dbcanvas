package main

// logsummary_grouprepl.go — what a MySQL Group Replication member's log actually says.
//
// The third cluster vocabulary in this package, and the one that reads most easily. Galera
// next door narrates an outage almost entirely in [Note] records and multi-line view
// blocks; asynchronous replication narrates it in MY- coded errors on a channel. Group
// Replication does something different again: the plugin writes one coded, complete,
// single-line record per event, and it says what it is doing in English.
//
//	Plugin group_replication reported: 'Member with address gr03:3306 has become unreachable.'
//	Plugin group_replication reported: 'Primary server with address gr01:3306 left the group. Electing new Primary.'
//	Plugin group_replication reported: 'This server is not able to reach a majority of members in the group.
//	                                    This server will now block all updates.'
//
// Every rule here was written against logs from a live three-node Percona Server 8.0.46-37
// single-primary group, captured while doing the thing that produces them: bootstrapping,
// stopping a member cleanly, killing one with SIGKILL under write load, killing the
// PRIMARY, cutting a member off port 33061 with tc/netem, rejoining by incremental
// recovery and by clone, planting a conflicting row to break the applier, and offering the
// group a member that had diverged. The fixtures are the `g*` directories under
// app/testdata/logsummary/ and the tests read them.
//
// Six things that capture taught, four of which corrected a first guess:
//
//  1. The level IS usable here, and better than in either neighbour. A lost majority is an
//     [ERROR] (MY-011495), an expelled member is a [Warning] (MY-011493/011499), an
//     election is a [System] (MY-011500/011507). Severity still comes from meaning — a
//     [Warning] that the group has stopped accepting writes is not a warning — but the
//     level is never actively misleading the way Galera's [Note] is.
//
//  2. A clean stop and a death ARE distinguishable, and by one record. Galera cannot tell
//     them apart from the survivors' side (see lsFindingPartition next door); GR can. A
//     `systemctl stop` produces `Members removed from the group` with nothing before it,
//     because the leaving member announces itself. A SIGKILL produces
//     `Member with address … has become unreachable` FIRST, and the removal follows one
//     expel timeout later — 16.0s in the captures, twice, to the tenth of a second.
//
//  3. The two ways of leaving the group have opposite read-only outcomes, and this is the
//     single most dangerous thing in the file. A member whose applier fails is set
//     read-only on the way out (MY-011712) — safe. A member REJECTED at join for having
//     transactions the group does not have (MY-011526/011522) ends with
//     `Setting super_read_only=OFF` — writable, outside the group, and stale. Both read as
//     "the member left the group" if you only match the leave record. lsFindingGRSplitBrain
//     exists because the corpus contains a real instance: the load generator in the test
//     harness reconnected to exactly such a member and wrote 1,263 rows into it.
//
//  4. A split does not heal itself, and the log does not say so. Cutting one of two
//     remaining members off produced MY-011495 on BOTH sides — neither had a majority,
//     both blocked writes. Restoring the link changed nothing: for the two and a half
//     minutes until an operator intervened, neither side logged another word. The plugin
//     does own the vocabulary for recovery (MY-011494 `is reachable again`, MY-011498
//     `has resumed contact with a majority`) — it appeared only once an operator acted.
//     So "blocked" is a state that persists until contradicted, and lsFindingGRSplit reads
//     an un-contradicted MY-011495 as a cluster that is still down.
//
//  5. Flow control is invisible — measured, and more absolutely than in Galera. With
//     group_replication_flow_control_mode=QUOTA, both thresholds set to 1 (the most
//     aggressive setting there is), a member slowed to 120 ms RTT with netem, and 1,364
//     transactions certified through the flood, all three members wrote ZERO lines. Galera
//     at least logs its interval once. See lsFindingGRFlowControl.
//
//  6. A member can be up, serving connections, writable, and not in the group at all —
//     and its log will not mention it. `kill -9` on a member left systemd to restart
//     mysqld, which came back, logged `ready for connections`, and stopped there:
//     group_replication_start_on_boot is OFF, so nothing rejoined. Measured at that
//     moment: super_read_only=0, one row in replication_group_members (itself), and 666
//     transactions behind the group. The evidence for it is an absence, which is why
//     lsFindingGRNotRejoined is built the way it is.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Group Replication member states, spelled as performance_schema.replication_group_members
// spells them, plus one the plugin describes but the table does not name.
//
// These are deliberately NOT folded into Galera's SYNCED/JOINER/DONOR vocabulary. The two
// state machines answer the same question — is this node serving? — but a reader looking
// at a GR member wants the word GR uses, and RECOVERING is not JOINER: a JOINER has no
// data, a RECOVERING member has data and is catching up.
const (
	lsStateOnline     = "ONLINE"     // in the group, up to date, serving
	lsStateRecovering = "RECOVERING" // in the group, applying the backlog, not serving
	lsStateGRError    = "ERROR"      // the plugin stopped on an error; not serving
	lsStateOffline    = "OFFLINE"    // mysqld is up, the plugin is not running
	lsStateBlocked    = "BLOCKED"    // no majority: the member refuses writes until contact returns
)

// lsFlavourGroupRepl is the third flavour. It matters beyond labelling: the phase machine
// seeds a non-cluster MySQL server into RUNNING (see lsResolveStandalone), which is right
// for a standalone and wrong for a GR member — a member sitting at OFFLINE is not "up and
// serving", it is a server that is not in its cluster.
const lsFlavourGroupRepl = "grouprepl"

// The plugin's own prefix. Every record the plugin writes carries it, which makes it both
// the flavour sniff and the guard that stops these rules matching an asynchronous replica.
const lsGRPrefix = "Plugin group_replication reported:"

// GR channel names. The recovery and applier channels reuse the ordinary replication error
// codes — MY-010584 for an applier failure, MY-014002 for a receiver connecting — so the
// channel name is the only thing that makes such a record a Group Replication event rather
// than an asynchronous one. That is why these rules sit ahead of lsReplRules and match on
// the channel.
const (
	lsGRChanApplier  = "group_replication_applier"
	lsGRChanRecovery = "group_replication_recovery"
)

var (
	// `Group membership changed to gr02:3306, gr03:3306, gr01:3306 on view 178672:3.`
	// GR's view record is one line and carries the whole membership, which is the reason
	// this catalogue needs no continuation-line handling where Galera's does.
	lsGRView = regexp.MustCompile(`Group membership changed to (.+) on view ([\d:]+)`)
	// `Members removed from the group: gr03:3306` / `Members joined the group: …`
	lsGRMembersGone   = regexp.MustCompile(`Members removed from the group: (.+)`)
	lsGRMembersJoined = regexp.MustCompile(`Members joined the group: (.+)`)
	// `Member with address gr03:3306 has become unreachable.`
	lsGRUnreachable = regexp.MustCompile(`Member with address (\S+?):\d+ (?:has become unreachable|is reachable again)`)
	// `A new primary with address gr02:3306 was elected.`
	lsGRNewPrimary = regexp.MustCompile(`A new primary with address (\S+?):\d+ was elected`)
	// `Primary server with address gr01:3306 left the group.`
	lsGROldPrimary = regexp.MustCompile(`Primary server with address (\S+?):\d+ left the group`)
	// `This server is working as secondary member with primary member address gr02:3306.`
	lsGRSecondaryOf = regexp.MustCompile(`working as secondary member with primary member address (\S+?):\d+`)
	// `The member with address gr03:3306 was declared online`
	lsGRPeerOnline = regexp.MustCompile(`The member with address (\S+?):\d+ was declared online`)
	// `connected to source 'repl@gr02:3306'` on a GR channel — the donor.
	lsGRDonor = regexp.MustCompile(`connected to source '[^@']*@(\S+?):\d+'`)
	// `Local transactions: <uuid>:1-290, <uuid>:1-906 > Group transactions: <uuid>:1-906`
	lsGRLocalGTIDs = regexp.MustCompile(`Local transactions: (.+?) > Group transactions:`)
	// The channel a replication record belongs to, quoted: `for channel 'x'` / `FOR CHANNEL 'x'`.
	lsGRChannel = regexp.MustCompile(`(?i)(?:for )?channel '([^']+)'`)
)

// lsGRMemberList turns `gr02:3306, gr03:3306, gr01:3306` into the short host names, in the
// order the plugin printed them.
func lsGRMemberList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(part), "."))
		if part == "" {
			continue
		}
		if i := strings.LastIndex(part, ":"); i > 0 {
			part = part[:i]
		}
		out = append(out, part)
	}
	return out
}

// lsGRChannelOf reports which replication channel a record is about, or "" when it names
// none.
func lsGRChannelOf(r lsRecord) string {
	if m := lsGRChannel.FindStringSubmatch(r.Text); m != nil {
		return m[1]
	}
	return ""
}

// lsGRIsGroupChannel is the test that keeps the shared replication codes on the right side
// of the line: MY-010584 on 'group_replication_applier' is a GR applier failure, and the
// same code on ” or on a named channel is an asynchronous replica's.
func lsGRIsGroupChannel(r lsRecord) bool {
	return strings.HasPrefix(lsGRChannelOf(r), "group_replication_")
}

// lsGroupRules is the Group Replication catalogue, ordered most-specific first.
//
// It runs AHEAD of both other MySQL catalogues. The GR-only records could sit anywhere —
// nothing else claims MY-0115xx — but the records GR shares with asynchronous replication
// could not: put these second and an applier conflict inside a group would be reported as
// a broken asynchronous replica, naming a channel the operator never configured.
var lsGroupRules = []lsRule{
	// ---- the group has stopped working -------------------------------------------
	// The single most important record in the file. It is also the one whose meaning
	// outlives it: nothing marks its end except MY-011498, so a bundle that ends here
	// ends with the cluster still refusing writes. See lsFindingGRSplit.
	{codes: []string{"MY-011495"},
		class: lsClassQuorum, sev: lsSevBad, label: "Lost majority — updates blocked",
		means: "This member cannot see more than half the group, so it has stopped accepting writes and will keep refusing them until it can. Reads still work and will return whatever this member last managed to apply, which is why an application can look healthy while the cluster is not.",
		enrich: func(_ lsRecord, e *lsEvent) {
			e.State, e.Primary = lsStateBlocked, "no"
		}},
	{codes: []string{"MY-011498"},
		class: lsClassQuorum, sev: lsSevOK, label: "Majority regained — updates flowing again",
		means: "Contact with more than half the group came back and this member is accepting writes again. This is the record that ends a block; without it, a block earlier in the file is still in force at the end of it.",
		enrich: func(_ lsRecord, e *lsEvent) {
			e.State, e.Primary = lsStateOnline, "yes"
		}},

	// ---- rejected at the door ----------------------------------------------------
	// The dangerous pair. Note the state: this member does NOT end up read-only — the
	// captures show `Setting super_read_only=OFF` three seconds later, every time.
	{codes: []string{"MY-011526"},
		class: lsClassConflict, sev: lsSevBad, label: "Diverged: has transactions the group does not",
		means: "This member executed transactions that never reached the group — typically because it was a primary that died with writes in flight, or because something wrote to it while it was outside the group. Group Replication compares GTID sets before it will let a member in, and this comparison failed.",
		enrich: func(r lsRecord, e *lsEvent) {
			if m := lsGRLocalGTIDs.FindStringSubmatch(r.Text); m != nil {
				e.Message = "extra transactions on this member: " + strings.TrimSpace(m[1])
			}
		}},
	{codes: []string{"MY-011522"},
		class: lsClassConflict, sev: lsSevBad, label: "Refused by the group and leaving",
		means:  "The group would not accept this member because of the divergence above, so it is leaving again. Watch what happens next: a member rejected this way is NOT left read-only — it goes back to accepting writes while holding data the cluster does not have. Anything that reconnects to it writes into a server that is no longer part of the cluster.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State = lsStateOffline }},
	{codes: []string{"MY-011640"},
		class: lsClassMember, sev: lsSevBad, label: "Timed out waiting for a view after joining",
		means:  "The member reached the group but never received the view that would confirm its membership, and gave up. In the capture this happened because the group still listed this member as UNREACHABLE from a previous incarnation and would not admit a second one — the old entry has to go before the new process can join.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State = lsStateOffline }},

	// ---- the applier gave up -----------------------------------------------------
	{codes: []string{"MY-011451", "MY-011452"},
		class: lsClassReplica, sev: lsSevBad, label: "Applier failed — member leaving the group",
		means:  "A transaction the group had already committed could not be applied here, and Group Replication does not allow a member to skip one. The member removes itself rather than diverge. The cause is in the applier error immediately above this line.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State = lsStateGRError }},
	{codes: []string{"MY-011712"},
		class: lsClassReplica, sev: lsSevWarn, label: "Set read-only after an error",
		means:  "The plugin put the server into read-only mode on its way out of the group. This is the safe outcome and worth telling apart from the other way a member leaves: a member rejected for divergence at join time is left WRITABLE instead.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State = lsStateGRError }},
	// The shared applier codes, claimed for GR only when the channel says so.
	{codes: []string{"MY-010584", "MY-010586"}, needSubstr: []string{"group_replication_"},
		class: lsClassReplica, sev: lsSevBad, label: "Group Replication applier error",
		means: "A transaction from the group failed to apply on this member. On the group_replication_applier channel this stops the member; on group_replication_recovery it fails one attempt at catching up, and the member tries another donor.",
		enrich: func(r lsRecord, e *lsEvent) {
			ch := lsGRChannelOf(r)
			if ch == lsGRChanRecovery {
				e.Sev, e.Label = lsSevWarn, "Recovery attempt failed to apply"
				e.Meaning = "This member tried to catch up from a donor and could not apply what it received. It will try the next donor and keep cycling; a member that repeats this indefinitely is stuck in RECOVERING and is not serving. Recovery cannot skip the row it is stuck on — the member's own copy of that row has to go, or it needs a clone instead."
				e.State = lsStateRecovering
			}
			if i := strings.Index(r.Text, "Duplicate entry"); i >= 0 {
				e.Message = strings.TrimSpace(r.Text[i:])
			}
		}},

	// ---- membership --------------------------------------------------------------
	// Unreachable is the record that separates a death from a departure. It never
	// appears before a clean stop — verified across every fixture — and it always
	// appears before an expulsion.
	{codes: []string{"MY-011493"},
		class: lsClassNetwork, sev: lsSevWarn, label: "Peer unreachable",
		means:  "This member stopped hearing from a peer. Nothing has been decided yet: the peer is expelled only if it stays silent past group_replication_member_expel_timeout, and it can come back before then. A clean shutdown never produces this record — its absence before a removal is what says the peer left on purpose.",
		enrich: lsGREnrichPeer},
	{codes: []string{"MY-011494"},
		class: lsClassNetwork, sev: lsSevOK, label: "Peer reachable again",
		means:  "The peer started answering before the expel timeout ran out, so nothing was removed.",
		enrich: lsGREnrichPeer},
	{codes: []string{"MY-011499"},
		class: lsClassMember, sev: lsSevWarn, label: "Members removed from the group",
		means: "These members are no longer in the group. Whether they left or were thrown out is decided by what came before: an unreachable record first means expelled, nothing first means a clean stop.",
		enrich: func(r lsRecord, e *lsEvent) {
			if m := lsGRMembersGone.FindStringSubmatch(r.Text); m != nil {
				e.Lost = lsGRMemberList(m[1])
				if len(e.Lost) > 0 {
					e.Peer = e.Lost[0]
				}
			}
		}},
	{codes: []string{"MY-011497"},
		class: lsClassMember, sev: lsSevOK, label: "Members joined the group",
		means: "New members appeared in the view. They are not serving yet — a member is usable only once it is declared online.",
		enrich: func(r lsRecord, e *lsEvent) {
			if m := lsGRMembersJoined.FindStringSubmatch(r.Text); m != nil {
				e.Left = lsGRMemberList(m[1])
			}
		}},
	{codes: []string{"MY-011503"},
		class: lsClassMember, sev: lsSevInfo, label: "Group membership changed",
		means: "The group agreed on a new membership. The view identifier increments once per change, so a gap in it means a change this log did not see.",
		enrich: func(r lsRecord, e *lsEvent) {
			m := lsGRView.FindStringSubmatch(r.Text)
			if m == nil {
				return
			}
			members := lsGRMemberList(m[1])
			e.Members = len(members)
			e.Message = fmt.Sprintf("view %s: %s", m[2], strings.Join(members, ", "))
		}},
	{codes: []string{"MY-011504"},
		class: lsClassMember, sev: lsSevWarn, label: "This member left the group",
		means:  "The plugin took this server out of the group. On its own this says nothing about why — the reason is the record above it, and the difference between a clean stop and a rejection matters a great deal.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State, e.Members, e.Primary = lsStateOffline, 0, "" }},

	// ---- who is the primary ------------------------------------------------------
	{codes: []string{"MY-011500"},
		class: lsClassMember, sev: lsSevWarn, label: "Primary left — electing a new one",
		means: "The member that was accepting writes is gone and the group is choosing a replacement. Writes fail from here until the election completes and the new primary has drained the backlog.",
		enrich: func(r lsRecord, e *lsEvent) {
			if m := lsGROldPrimary.FindStringSubmatch(r.Text); m != nil {
				e.Peer = m[1]
			}
		}},
	{codes: []string{"MY-011507"},
		class: lsClassMember, sev: lsSevOK, label: "New primary elected",
		means: "The group has a writable member again. It still has to execute everything the group committed before it will accept a write, so the outage ends slightly after this line, not on it.",
		enrich: func(r lsRecord, e *lsEvent) {
			if m := lsGRNewPrimary.FindStringSubmatch(r.Text); m != nil {
				e.Peer, e.Primary = m[1], "yes"
			}
		}},
	{codes: []string{"MY-011510"},
		class: lsClassState, sev: lsSevOK, label: "Now the primary",
		means:  "This member is the one accepting writes.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State, e.Primary = lsStateOnline, "yes" }},
	{codes: []string{"MY-011511"},
		class: lsClassState, sev: lsSevInfo, label: "Now a secondary",
		means: "This member is read-only and applying what the primary commits.",
		enrich: func(r lsRecord, e *lsEvent) {
			e.State = lsStateOnline
			if m := lsGRSecondaryOf.FindStringSubmatch(r.Text); m != nil {
				e.Peer = m[1]
			}
		}},

	// ---- joining and recovery ----------------------------------------------------
	{codes: []string{"MY-011490"},
		class: lsClassState, sev: lsSevOK, label: "Online in the group",
		means:  "Distributed recovery finished: this member has everything the group had when it joined, and is serving.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State = lsStateOnline }},
	{codes: []string{"MY-011492"},
		class: lsClassMember, sev: lsSevOK, label: "Peer online in the group",
		means: "Another member finished recovering and is serving.",
		enrich: func(r lsRecord, e *lsEvent) {
			if m := lsGRPeerOnline.FindStringSubmatch(r.Text); m != nil {
				e.Peer = m[1]
			}
		}},
	{codes: []string{"MY-013471"},
		class: lsClassTransfer, sev: lsSevInfo, label: "Recovery method chosen",
		means: "How this member intends to catch up. Incremental reads the missing transactions from a donor's binary log and is cheap; cloning copies the donor's entire data directory and is not.",
		enrich: func(r lsRecord, e *lsEvent) {
			if strings.Contains(r.Text, "Cloning") {
				e.Sev, e.Label = lsSevWarn, "Recovery will clone the whole dataset"
				e.Meaning = "The gap was too large for the binary log, so this member will copy the donor's entire data directory. Three things follow that a reader needs to expect: everything already on this member is deleted first, the donor does real work for the duration, and mysqld restarts itself at the end."
			}
			e.State = lsStateRecovering
		}},
	{codes: []string{"MY-013469"},
		class: lsClassTransfer, sev: lsSevWarn, label: "Too far behind for incremental recovery",
		means:  "The member is missing more transactions than the threshold allows, or the binary logs holding them are gone, so recovery falls back to a clone.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State = lsStateRecovering }},
	{codes: []string{"MY-013460"},
		class: lsClassTransfer, sev: lsSevWarn, label: "Clone is erasing this member's data",
		means:  "The clone is deleting everything in this data directory before copying the donor's. This is normal for a clone and catastrophic if the clone was not what you intended — there is no undo, and a member picked as the RECIPIENT loses whatever it held.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State = lsStateRecovering }},
	// The receiver connecting on a GR channel names the donor. Same code as an
	// asynchronous replica's — the channel is the difference.
	{codes: []string{"MY-014002"}, needSubstr: []string{"group_replication_"},
		class: lsClassTransfer, sev: lsSevInfo, label: "Recovering from a donor",
		means: "This member is pulling the transactions it missed from another member. If this line repeats with a different donor each time, recovery is failing and cycling.",
		enrich: func(r lsRecord, e *lsEvent) {
			e.State = lsStateRecovering
			if m := lsGRDonor.FindStringSubmatch(r.Text); m != nil {
				e.Peer = m[1]
				e.Message = "donor: " + m[1]
			}
		}},

	// ---- the plugin's own lifecycle ----------------------------------------------
	{codes: []string{"MY-013587"},
		class: lsClassStartup, sev: lsSevInfo, label: "Group Replication starting",
		means:  "START GROUP_REPLICATION was issued, or the server was configured to start it on boot. Everything before this line is a server that was not in the cluster.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State = lsStateRecovering }},
	{codes: []string{"MY-014010"},
		class: lsClassStartup, sev: lsSevInfo, label: "Group Replication started",
		means: "The plugin is running. It is not the same as being online in the group — recovery may still be in progress."},
	{codes: []string{"MY-011650", "MY-011651"},
		class: lsClassShutdown, sev: lsSevWarn, label: "Group Replication stopped",
		means:  "The plugin is no longer running. mysqld may well still be up and answering queries — a server in this state is not part of the cluster and nothing in its later log will say so.",
		enrich: func(_ lsRecord, e *lsEvent) { e.State, e.Members, e.Primary = lsStateOffline, 0, "" }},
	{codes: []string{"MY-011486"},
		class: lsClassMember, sev: lsSevWarn, label: "Message discarded — plugin not ready",
		means: "The group sent this member something while its plugin was starting or stopping, and it was dropped. On its own it is a symptom of the join or leave happening around it, not a fault."},
	// GCS connection noise. It appears on healthy joins as well as real network trouble,
	// so it is information — but an [ERROR]-level one still floats up, via the level floor.
	{codes: []string{"MY-011735"},
		class: lsClassNetwork, sev: lsSevInfo, label: "Group communication connection reset",
		means: "The group-communication layer dropped and re-made a connection to a peer. One of these around a join or a leave is normal; a run of them between two members points at the link between those two."},

	// ---- MySQL Shell's provisioning noise ----------------------------------------
	//
	// The single most misleading thing in an InnoDB Cluster's log, and the reason these
	// rules exist at all: deploying a healthy three-node cluster wrote TWENTY-SIX [ERROR]
	// records across the three members, and every one of them was MySQL Shell checking
	// something rather than anything failing.
	//
	// Shell's configureInstance/createCluster open a throwaway replication channel called
	// `mysqlsh.test` and deliberately try to start it, to find out whether the instance
	// can replicate at all and whether the server ids collide. The attempts fail on
	// purpose. It also asks the server to start Group Replication before it has been
	// configured, so `group_replication_group_name is mandatory` and `plugin is not
	// installed` are answers to questions Shell asked, not faults.
	//
	// Filing these as errors would report every freshly built InnoDB Cluster as broken.
	// They are kept — deleted evidence is not evidence — but as information, with the
	// explanation attached. Note that this is the mirror image of the Galera lesson next
	// door: there the level UNDER-reports the severity, here it wildly over-reports it.
	{substr: []string{"mysqlsh.test"}, overLevel: true,
		class: lsClassStartup, sev: lsSevInfo, label: "MySQL Shell instance check",
		means: "MySQL Shell opened a temporary replication channel called 'mysqlsh.test' to find out whether this instance can replicate, and whether its server id collides with another member's. The attempt is meant to fail — this record is the answer to Shell's question, not a fault. Expect a run of these whenever an instance is added to an InnoDB Cluster."},
	{codes: []string{"MY-011685", "MY-010381"}, overLevel: true,
		class: lsClassConfig, sev: lsSevInfo, label: "Group Replication not configured yet",
		means: "Something asked this server to start Group Replication before it had been configured for it. During an InnoDB Cluster deployment that something is MySQL Shell, probing the instance before it configures it, and the record is expected. On a server that was already a working member it is not: it means the configuration went missing."},
	{codes: []string{"MY-011660"}, overLevel: true,
		class: lsClassStartup, sev: lsSevWarn, label: "Could not start Group Replication on boot",
		means: "The server was configured to join the group at start-up and could not. During an InnoDB Cluster's first deployment this is Shell's probe and harmless; on an established member it means the server came up outside its cluster and stayed there."},

	// ---- read-only bookkeeping ---------------------------------------------------
	// Individually these are noise — a healthy bootstrap writes five of them. They earn
	// their place because super_read_only=OFF is the difference between a safe exit from
	// the group and a writable stale server, and lsFindingGRSplitBrain needs to find them.
	{codes: []string{"MY-011565"},
		class: lsClassConfig, sev: lsSevInfo, label: "super_read_only set ON",
		means: "The plugin made the server read-only. Every member does this on the way in, and secondaries stay this way."},
	{codes: []string{"MY-011566"},
		class: lsClassConfig, sev: lsSevInfo, label: "super_read_only set OFF",
		means: "The server will now accept writes. That is correct for a member that has just been elected primary — and alarming right after a member has left or been refused by the group, because then it is a server outside the cluster accepting writes."},
	{codes: []string{"MY-013731"},
		class: lsClassConfig, sev: lsSevInfo, label: "Post-election member action",
		means: "One of the actions Group Replication runs on the new primary after an election — clearing read-only, starting failover channels."},
	{codes: []string{"MY-014081", "MY-014082"},
		class: lsClassStartup, sev: lsSevInfo, label: "Certifier broadcast thread",
		means: "The plugin's internal certifier thread starting or stopping. It brackets the plugin's lifetime and means nothing on its own."},
}

// lsGREnrichPeer pulls the peer out of the reachability records, which share a shape.
func lsGREnrichPeer(r lsRecord, e *lsEvent) {
	if m := lsGRUnreachable.FindStringSubmatch(r.Text); m != nil {
		e.Peer = m[1]
	}
}

// lsGRStateMeaning documents the GR states for the timeline legend, alongside Galera's.
var lsGRStateMeaning = map[string]string{
	lsStateOnline:     "in the group, caught up, and serving queries",
	lsStateRecovering: "in the group but still applying the backlog — it has data, it is not yet usable, and it is not in the read pool",
	lsStateGRError:    "the plugin stopped on an error and the member removed itself from the group",
	lsStateOffline:    "mysqld is up but Group Replication is not running on it — the server is not in the cluster, and nothing further in its log will say so",
	lsStateBlocked:    "it cannot see a majority, so it refuses every write until contact returns; reads still succeed and return stale data",
}

// lsGRSniff answers "is this a Group Replication member's log?".
//
// The plugin's prefix is conclusive and the codes are a fallback for a fragment that
// starts mid-incident: MY-0114xx/MY-0115xx belong to the plugin and to nothing else.
func lsGRSniff(recs []lsRecord) bool {
	for _, r := range recs {
		if strings.Contains(r.Text, lsGRPrefix) {
			return true
		}
		if strings.HasPrefix(r.Code, "MY-0115") || strings.HasPrefix(r.Code, "MY-0114") {
			return true
		}
		if strings.Contains(r.Text, "group_replication_applier") || strings.Contains(r.Text, "group_replication_recovery") {
			return true
		}
	}
	return false
}

// lsGRNodeName pulls the member's own name out of its log.
//
// A GR member never prints its own name the way a Galera node prints base_host. What it
// does print is every OTHER member's address, plus its own in the view list — so the name
// is the one member that appears in views but never in a "member with address …" record
// about somebody else. That is fragile on its own, so the relay-log hint is tried first:
// `Please use '--relay-log=gr03-relay-bin'` names the host directly, and it is written on
// every start-up before anything can have gone wrong.
var lsGRRelayHint = regexp.MustCompile(`--relay-log=(\S+?)-relay-bin`)

func lsGRNodeName(recs []lsRecord) string {
	for _, r := range recs {
		if m := lsGRRelayHint.FindStringSubmatch(r.Text); m != nil {
			return m[1]
		}
	}
	// Fallback: members named in views, minus every member named as somebody else.
	inView, other := map[string]int{}, map[string]bool{}
	for _, r := range recs {
		if m := lsGRView.FindStringSubmatch(r.Text); m != nil {
			for _, h := range lsGRMemberList(m[1]) {
				inView[h]++
			}
		}
		for _, re := range []*regexp.Regexp{lsGRUnreachable, lsGRNewPrimary, lsGROldPrimary, lsGRSecondaryOf, lsGRPeerOnline, lsGRDonor} {
			if m := re.FindStringSubmatch(r.Text); m != nil {
				other[m[1]] = true
			}
		}
		if m := lsGRMembersGone.FindStringSubmatch(r.Text); m != nil {
			for _, h := range lsGRMemberList(m[1]) {
				other[h] = true
			}
		}
	}
	best, bestN := "", 0
	for h, n := range inView {
		if !other[h] && n > bestN {
			best, bestN = h, n
		}
	}
	return best
}

// lsGRPromoteMembers recognises a group member whose OWN log fragment does not say so.
//
// A member's log only proves it is in a group while the plugin is writing to it. Both
// captures where a member was killed produced a fragment containing nothing but a restart —
// mysqld starting, InnoDB recovering, `ready for connections`, and then silence, because
// group_replication_start_on_boot was OFF and nothing rejoined. Read alone, such a file is
// a plain MySQL server, and lsResolveStandalone would correctly call it RUNNING.
//
// In a bundle it is not alone, and the other members are still talking about it:
//
//	gr02.err: Primary server with address gr01:3306 left the group. Electing new Primary.
//	gr02.err: Members removed from the group: gr03:3306
//
// A source whose own name is named as a member by another source's Group Replication
// records is a member of that group — that is evidence, not inference. Promoting it
// matters for exactly one thing, and it is the thing that misleads: `ready for connections`
// on such a server means OFFLINE, not RUNNING. A server that is up, writable and outside
// its cluster must not paint the lane green for the rest of the window.
//
// The promotion is deliberately narrow. It requires the source to name no cluster of its
// own (it stays as-is if it is a Galera node or already a GR one) and requires another
// source to name it, so a genuine standalone server uploaded alongside a group is left
// alone — nothing in the group's logs mentions it.
func lsGRPromoteMembers(b *lsBundle) {
	named := map[string]bool{}
	anyGR := false
	for _, s := range b.Sources {
		if s.Flavour == lsFlavourGroupRepl {
			anyGR = true
		}
	}
	if !anyGR {
		return
	}
	for _, e := range b.Events {
		if e.Peer != "" {
			named[e.Peer] = true
		}
		for _, h := range e.Lost {
			named[h] = true
		}
		for _, h := range e.Left {
			named[h] = true
		}
		// The view record lists every member by name in its message.
		if e.Code == "MY-011503" {
			for _, h := range lsGRMemberList(strings.TrimPrefix(e.Message, "view ")) {
				if i := strings.Index(h, ": "); i >= 0 {
					h = h[i+2:]
				}
				named[h] = true
			}
		}
	}
	for i := range b.Sources {
		s := &b.Sources[i]
		if s.Flavour != lsFlavourMySQL || s.Node == "" || !named[s.Node] {
			continue
		}
		s.Flavour = lsFlavourGroupRepl
		// Re-resolve the one state whose meaning changes. lsResolveStandalone has already
		// run over this source's events; RUNNING is the only verdict it reached that a
		// group member's log does not support.
		for j := range b.Events {
			if b.Events[j].Src == s.Idx && b.Events[j].State == lsStateUp {
				b.Events[j].State = lsStateOffline
			}
		}
	}
}

// lsGRExpelGap formats the delay between a peer going unreachable and its removal, which
// is group_replication_member_expel_timeout plus detection. It read 16.0s in both captures
// that produced one, against a default expel timeout of 5s.
func lsGRExpelGap(sec float64) string {
	if sec <= 0 {
		return ""
	}
	return strconv.FormatFloat(sec, 'f', 1, 64) + "s"
}
