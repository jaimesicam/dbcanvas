package main

// logsummary_valkey.go — Valkey, standalone replication and Valkey Cluster.
//
// The sixth cluster vocabulary, and the one whose log is least like the other five.
//
// Every other engine here writes a structured header: a level, usually a code, and a
// subsystem. Valkey writes a pid, a ROLE LETTER, a date with the day first and the month as
// a name, and one of four punctuation marks for the level:
//
//	253:M 15 Aug 2026 23:03:55.100 * Ready to accept connections tcp
//	  ^  ^ ^                       ^ ^
//	  |  | |                       | the message
//	  |  | |                       level: . debug  - verbose  * notice  # warning
//	  |  | the timestamp — no zone, no year on the journald prefix, no ISO anything
//	  |  the role this process thought it had when it wrote the line
//	  the pid
//
// The role letter is the reason this file exists in the shape it does. On every other
// engine, working out what state a node was in means pairing transitions that may be
// hundreds of lines apart, and a log fragment that contains no transition leaves the lane
// blank (see lsSeedState, and the trouble it goes to). Valkey stamps the role on EVERY LINE.
// A file whose letters run M M M S S S is a demotion with a timestamp on it, and the state
// track can be read straight off the headers rather than reconstructed. Nothing else here
// offers that.
//
// The level, by contrast, is worth almost nothing. Across the whole corpus the entire story
// of an automatic failover — the failure detection, the election, the vote, the promotion —
// is written at `*`, notice. What is written at `#` is a boilerplate warning about
// vm.overcommit_memory that appears on every single start of every healthy node. Taking the
// level as a floor would file a promotion below a host-tuning hint, so severity here comes
// from what a record MEANS, exactly as it does for Galera.
//
// Two logs in one file, again. dbcanvas sets no `logfile`, so Valkey writes to stdout and
// systemd keeps it — which means the collector reads the JOURNAL, and the journal contains
// systemd's own records as well as Valkey's. That is not a nuisance to be filtered out: it
// is the only place a kill is recorded at all. A SIGKILLed valkey-server writes nothing
// whatsoever, and `Main process exited, code=killed, status=9/KILL` in systemd's half of the
// same file is the entire evidence that the process was killed rather than stopped. So both
// halves are parsed, marked with different subsystems, and read together — the same shape as
// the PostgreSQL/Patroni pair.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The three flavours of a Valkey source.
//
// They are separated for the same reason PostgreSQL's are: the findings that may speak
// differ, and telling somebody with a single cache node that their cluster has uncovered
// hash slots is worse than saying nothing.
const (
	lsFlavourValkey        = "valkey"        // one server, no replication in evidence
	lsFlavourValkeyRepl    = "valkeyrepl"    // primary/replica replication, no cluster bus
	lsFlavourValkeyCluster = "valkeycluster" // a Valkey Cluster member: hash slots and a gossip bus
)

// lsSubsys marks which half of the journal a record came from, so a rule can require one.
const (
	lsSubsysValkey  = "valkey"
	lsSubsysVKSysd  = "systemd"
	lsSubsysVKChild = "child" // the forked RDB/AOF process, which is not the server
)

// lsValkeyHeader matches Valkey's own header, with or without journald's prefix in front.
//
// The prefix is optional because both shapes are real and the corpus contains both: a node
// that sets `logfile` (or a container reading stdout) writes the bare form, and a dbcanvas
// node read through journalctl writes the prefixed one.
var lsValkeyHeader = regexp.MustCompile(
	`^(?:([A-Z][a-z]{2} +\d{1,2} \d{2}:\d{2}:\d{2}) (\S+) \S+?(?:\[\d+\])?: )?` + // 1 journal stamp, 2 host
		`(\d+):([MSCX]) ` + // 3 pid, 4 role
		`(\d{1,2} \w{3} \d{4} \d{2}:\d{2}:\d{2}\.\d{3}) ` + // 5 valkey's own stamp
		`([.\-*#]) ` + // 6 level
		`(.*)$`) // 7 message

// lsValkeySignal matches the one line Valkey writes without a level or a timestamp:
//
//	296:signal-handler (1786835322) Received SIGTERM scheduling shutdown...
//
// It is the first record of every clean stop and would otherwise be dropped, taking with it
// the only unambiguous evidence that the stop was ASKED FOR rather than imposed.
var lsValkeySignal = regexp.MustCompile(
	`^(?:([A-Z][a-z]{2} +\d{1,2} \d{2}:\d{2}:\d{2}) (\S+) \S+?(?:\[\d+\])?: )?` +
		`(\d+):signal-handler \((\d+)\) (.*)$`)

// lsValkeySystemd matches systemd's own journal records for the unit.
//
// Only the unit's records, not the whole journal: `journalctl -u valkey@*` already restricts
// it, and the pattern requires the unit name so a bundle assembled by hand from a full
// journal does not pick up every other service on the host.
var lsValkeySystemd = regexp.MustCompile(
	`^([A-Z][a-z]{2} +\d{1,2} \d{2}:\d{2}:\d{2}) (\S+) systemd\[\d+\]: (.*)$`)

// lsValkeyLevel maps the punctuation to the words the rest of the package uses. Valkey's
// warning mark is `#` and there is nothing above it — no error level, no fatal level — which
// is another reason the level cannot carry the severity here.
func lsValkeyLevel(c string) string {
	switch c {
	case "#":
		return "WARNING"
	case "*":
		return "NOTE"
	case "-":
		return "VERBOSE"
	case ".":
		return "DEBUG"
	}
	return "NOTE"
}

// lsValkeyTime parses Valkey's own stamp: "15 Aug 2026 23:03:55.052".
//
// Day first, month by name, and no zone at all. Valkey logs in the server's local time and
// says nothing about which that is, so it is read as UTC — correct on a dbcanvas node, whose
// container runs UTC, and honest everywhere else. The timeline is built from differences
// within a bundle, so a uniform offset moves the whole picture and changes none of the
// durations; a source that really is in another zone gets the manual offset control, which
// is what it exists for.
func lsValkeyTime(stamp string) float64 {
	if t, err := time.Parse("2 Jan 2006 15:04:05.000", stamp); err == nil {
		return float64(t.UnixNano()) / 1e9
	}
	return 0
}

// lsValkeyJournalTime parses journald's prefix stamp, which carries no year.
//
// The year comes from the Valkey records around it — every one of those has a full date, and
// the two halves of the file are the same journal. A systemd record that appears before any
// Valkey record has nothing to borrow from and inherits the previous record's timestamp
// instead, marked approximate, exactly as an untimestamped MySQL block does.
func lsValkeyJournalTime(stamp string, year int) float64 {
	if year == 0 {
		return 0
	}
	t, err := time.Parse("2006 Jan 2 15:04:05", strconv.Itoa(year)+" "+strings.Join(strings.Fields(stamp), " "))
	if err != nil {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// lsFoldValkey parses a Valkey log — bare or journald-prefixed, Valkey's records and
// systemd's — into records, and reports the host name if the journal gave one.
//
// "Fold" is nearly a misnomer here: a Valkey record is a line, unlike PostgreSQL's ERROR
// with its DETAIL or Galera's view block. The one exception is the crash report, which is
// many lines of stack trace under a banner and belongs to the banner.
func lsFoldValkey(data []byte) ([]lsRecord, string) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	out := make([]lsRecord, 0, len(lines))
	host, year := "", 0
	lastTS := 0.0
	inCrash := false
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := lsValkeyHeader.FindStringSubmatch(line); m != nil {
			inCrash = false
			if host == "" {
				host = m[2]
			}
			ts := lsValkeyTime(m[5])
			if ts > 0 {
				lastTS = ts
				if year == 0 {
					if t, err := time.Parse("2 Jan 2006 15:04:05.000", m[5]); err == nil {
						year = t.Year()
					}
				}
			}
			rec := lsRecord{
				Line: i + 1, TS: ts, Time: m[5], Level: lsValkeyLevel(m[6]),
				Thread: m[3], Subsys: lsSubsysValkey, Text: strings.TrimSpace(m[7]),
				// The role letter is carried in Code. It is not a code in Valkey's sense —
				// Valkey has none — but it is the one stable, machine-readable token in the
				// header, and putting it where every other engine's code goes is what lets
				// lsRule's `codes` match on it without a field of its own.
				Code: m[4],
			}
			if m[4] == "C" {
				// The forked child is a different process doing a different job. Its records
				// are real and worth keeping — "Failed opening the temp RDB file" is written
				// by the child and is the whole of a persistence failure — but it must never
				// move the server's state lane, so it is marked as what it is.
				rec.Subsys = lsSubsysVKChild
			}
			// The crash report is a banner plus everything under it.
			if strings.Contains(rec.Text, "Valkey ") && strings.Contains(rec.Text, "crashed by signal") ||
				strings.Contains(rec.Text, "BUG REPORT START") || strings.Contains(rec.Text, "=== ASSERTION FAILED ===") {
				inCrash = true
			}
			out = append(out, rec)
			continue
		}
		if m := lsValkeySignal.FindStringSubmatch(line); m != nil {
			inCrash = false
			if host == "" {
				host = m[2]
			}
			ts := lsValkeyJournalTime(m[1], year)
			approx := false
			if ts == 0 {
				ts, approx = lastTS, lastTS > 0
			}
			out = append(out, lsRecord{
				Line: i + 1, TS: ts, Approx: approx, Time: m[1], Level: "NOTE",
				Thread: m[3], Subsys: lsSubsysValkey, Text: "signal-handler: " + strings.TrimSpace(m[5]),
			})
			continue
		}
		if m := lsValkeySystemd.FindStringSubmatch(line); m != nil {
			inCrash = false
			if host == "" {
				host = m[2]
			}
			ts := lsValkeyJournalTime(m[1], year)
			approx := false
			if ts == 0 {
				ts, approx = lastTS, lastTS > 0
			}
			if ts > 0 {
				lastTS = ts
			}
			out = append(out, lsRecord{
				Line: i + 1, TS: ts, Approx: approx, Time: m[1], Level: "NOTE",
				Subsys: lsSubsysVKSysd, Text: strings.TrimSpace(m[3]),
			})
			continue
		}
		// Neither shape. Under a crash banner it is the stack trace and belongs to it;
		// anywhere else it is a line from a log this parser does not own.
		if inCrash && len(out) > 0 {
			out[len(out)-1].Body = append(out[len(out)-1].Body, line)
		}
	}
	// Systemd records that came before the first Valkey record could not be dated from a
	// year that had not been seen yet. Now that one has, date them.
	if year != 0 {
		for i := range out {
			if out[i].TS == 0 || out[i].Approx {
				if ts := lsValkeyJournalTime(out[i].Time, year); ts > 0 {
					out[i].TS, out[i].Approx = ts, false
				}
			}
		}
	}
	return out, host
}

// lsValkeySelfID is the 40-character cluster node id this log's own server has.
//
// It is what makes a Valkey Cluster bundle readable. Every record about a peer names it by
// id and, on a node with no announce name, prints an EMPTY bracket after it — so a finding
// built from those records says "81ce2216adbc… was declared failed", which is not a sentence
// anybody can act on. Each node states its own id exactly once, on the way up, and pooling
// those across the sources turns every id in the bundle into a name.
//
// This is Galera's lsUUIDNames trick applied to a different engine, and it pays off the same
// way: one log names only itself, and the name for the id in front of you is almost always
// in a DIFFERENT file.
var lsValkeySelfRe = regexp.MustCompile(`(?:No cluster configuration found|Node configuration loaded), I'm ([0-9a-f]{40})`)

func lsValkeySelfID(recs []lsRecord) string {
	for _, r := range recs {
		if m := lsValkeySelfRe.FindStringSubmatch(r.Text); m != nil {
			return m[1]
		}
	}
	return ""
}

// lsValkeyNodeName is the host the journal recorded, shortened.
//
// Valkey itself never states its own name — nothing in a bare valkey-server log says which
// host wrote it, the same gap PostgreSQL has. journald's prefix does, so a node read through
// the collector names itself and a raw stdout log does not, and the caller falls back to the
// file name. Inventing one would be worse than a blank.
func lsValkeyNodeName(host string) string {
	if host == "" {
		return ""
	}
	return lsMongoShortHost(host)
}

// ---------------------------------------------------------------- the states

// Valkey's states.
//
// PRIMARY is shared with MongoDB and PostgreSQL deliberately — it means the same thing in
// all three, the one member accepting writes — and REPLICA is Valkey's own word for what
// MongoDB calls SECONDARY and PostgreSQL calls STANDBY. They are kept separate rather than
// unified because a reader looking at a Valkey node wants the word `INFO replication` prints,
// not another engine's.
const (
	lsStateVKReplica = "REPLICA" // following a primary, serving reads
	// SYNCING is a replica receiving a full copy of the dataset. Whether it answers anything
	// while it does depends on replica-serve-stale-data, which defaults to yes — so it may be
	// serving, and what it serves is stale by an unbounded amount. Either way it is not a
	// state to route fresh reads at.
	lsStateVKSyncing = "SYNCING"
	// LOADING is a server that is up, listening, and refusing every command with -LOADING
	// while it reads its RDB or AOF off disk. On a large dataset this is minutes, and it is
	// invisible to a health check that only opens a socket.
	lsStateVKLoading = "LOADING"
	// CLUSTERDOWN is the state that matters most and the one with no counterpart anywhere
	// else in this package: a Valkey Cluster node that is perfectly healthy, fully up, and
	// refusing every command with CLUSTERDOWN because some OTHER shard's slots are uncovered.
	// The node is fine. The cluster is not, and the node will not serve while that is true.
	lsStateVKDown = "CLUSTERDOWN"
)

// ---------------------------------------------------------------- the catalogue
//
// Every rule below matched a record in a capture from live servers, and the corpus is
// app/testdata/logsummary/v*. Nothing here was written from memory.
//
//	v01-cluster-failover  a six-node Valkey Cluster (three shards, one replica each) on
//	                      Valkey 8, driven through an automatic failover with the primary
//	                      SIGKILLed, the old primary rejoining as a replica, a manual
//	                      failover handing the shard back, and a whole shard — primary and
//	                      replica together — being killed so its slots went uncovered.
//	v02-cluster-nocover   a three-node all-primary Valkey Cluster on Percona Valkey 9.1.1,
//	                      read through journalctl exactly as the collector reads it, with one
//	                      shard stopped for thirty seconds and no replica to take over.
//	v03-standalone-repl   a primary and a replica wired by hand, driven through a full sync,
//	                      a killed primary, a partial resync, a manual promotion, a real
//	                      persistence failure, and forty thousand writes against an 8 MB
//	                      maxmemory.
//
// A note on `codes`: for Valkey the code IS the role letter, so a rule carrying
// codes: []string{"S"} fires on any record a replica wrote. That is used sparingly — the
// role is better read as state than as a matcher — but it is what tells the two sides of a
// replication record apart when the text alone cannot.
var lsValkeyRules = []lsRule{
	// ---- lifecycle -------------------------------------------------------------------
	{substr: []string{"Ready to accept connections"},
		class: lsClassState, sev: lsSevOK, label: "Ready — accepting connections",
		means: "The server is up and serving. On a cluster member this says nothing about whether the CLUSTER will let it answer: a node in a cluster whose slots are not fully covered logs this and then refuses every command with CLUSTERDOWN."},
	{substr: []string{"oO0OoO0OoO0Oo", "Valkey is starting", "Redis is starting"},
		class: lsClassStartup, sev: lsSevInfo, label: "Valkey starting",
		means: "A new server process. Everything above this line belongs to a different run."},
	{substr: []string{"Valkey version=", "Redis version="},
		class: lsClassStartup, sev: lsSevInfo, label: "Version and pid",
		means: "The build that wrote everything below. Worth reading when a cluster is behaving inconsistently — a mixed-version cluster is a real and common cause."},
	{substr: []string{"Running mode=cluster"},
		class: lsClassStartup, sev: lsSevInfo, label: "Running in cluster mode",
		means: "This node is a Valkey Cluster member: the keyspace is split into 16384 hash slots and this node owns some of them. A key that hashes elsewhere is not here and never was."},
	{substr: []string{"Running mode=standalone"},
		class: lsClassStartup, sev: lsSevInfo, label: "Running standalone",
		means: "No cluster bus. This node holds the whole keyspace it is given, and any replication is whatever REPLICAOF was pointed at."},
	{substr: []string{"Configuration loaded"},
		class: lsClassConfig, sev: lsSevInfo, label: "Configuration loaded"},
	{substr: []string{"Server initialized"},
		class: lsClassStartup, sev: lsSevInfo, label: "Server initialized"},
	{substr: []string{"signal-handler: Received SIGTERM", "User requested shutdown"},
		class: lsClassShutdown, sev: lsSevInfo, label: "Shutdown requested",
		means: "Somebody asked this server to stop — systemctl stop, a SHUTDOWN command, or an orchestrator. This is the record that distinguishes a planned stop from a kill, and it is the ONLY one: from every other node in the cluster the two are indistinguishable."},
	{substr: []string{"Valkey is now ready to exit", "Redis is now ready to exit"},
		class: lsClassShutdown, sev: lsSevInfo, label: "Stopped cleanly",
		means: "The server finished its shutdown: the final snapshot was written and the process exited on its own terms. A start after this one loads a complete dataset."},
	{substr: []string{"Saving the final RDB snapshot before exiting"},
		class: lsClassShutdown, sev: lsSevInfo, label: "Final snapshot before exit",
		means: "The clean stop is persisting the dataset. Its absence in a shutdown is what makes the difference between losing nothing and losing everything since the last save."},
	{substr: []string{"Saving the cluster configuration file before exiting"},
		class: lsClassShutdown, sev: lsSevInfo, label: "Cluster configuration saved before exit",
		means: "The node's view of the cluster — who owns which slots — was written to nodes.conf, so it comes back knowing where it stood rather than as a stranger."},

	// ---- loading, which is an outage nothing else measures ------------------------------
	{substr: []string{"Loading RDB produced by", "Reading RDB base file on AOF loading",
		"Loading the RDB and finalizing"},
		class: lsClassStartup, sev: lsSevInfo, label: "Loading the dataset",
		means: "The server is reading its dataset off disk and refuses every command with -LOADING until it finishes. The socket is open the whole time, so a health check that only connects sees a healthy node."},
	{substr: []string{"DB loaded from", "Done loading RDB"},
		class: lsClassStartup, sev: lsSevOK, label: "Dataset loaded",
		means: "The seconds on this line are how long the node was up, listening, and answering nothing. On a large dataset it is the largest single component of a restart and it appears nowhere but here."},

	// ---- replication, from the replica's side --------------------------------------------
	{substr: []string{"MASTER <-> REPLICA sync: Finished with success",
		"PRIMARY <-> REPLICA sync: Finished with success", "Successful partial resynchronization"},
		class: lsClassReplica, sev: lsSevOK, label: "Replica finished syncing",
		means: "The replica has the primary's dataset and is following the live stream. From here it is a serving replica."},
	{substr: []string{"Full resync from master:", "Full resync from primary:"},
		class: lsClassTransfer, sev: lsSevWarn, label: "Full resync — the whole dataset is being copied",
		means: "The expensive path. The primary forks, serialises everything it holds and ships it, and the replica throws away what it had and reloads from scratch. It costs the primary a fork and a burst of I/O, and it happens because a partial resync was not possible — the reason is on the primary's side of this exchange."},
	{substr: []string{"Trying a partial resynchronization"},
		class: lsClassReplica, sev: lsSevInfo, label: "Asking for a partial resync",
		means: "The replica is offering its replication id and offset in the hope that the primary still has that stretch in its backlog. Whether it does is the next record on the primary."},
	{substr: []string{"Partial resynchronization not accepted", "Unable to partial resync"},
		class: lsClassReplica, sev: lsSevWarn, label: "Partial resync REFUSED — a full copy follows",
		means: "The primary could not resume the stream where the replica left off, so the whole dataset has to be copied instead. Two reasons and the message says which: a replication ID mismatch means the replica was following a different primary (or the primary was restarted and its history reset), and an offset outside the backlog means repl-backlog-size was too small for how long the replica was away."},
	{substr: []string{"Partial resynchronization request from", "Primary accepted a Partial Resynchronization",
		"Master accepted a Partial Resynchronization"},
		class: lsClassReplica, sev: lsSevOK, label: "Partial resync accepted — only the gap is sent",
		means: "The cheap path: the primary still had the missing stretch in its backlog and sent only that. No fork, no full copy, and the replica is caught up in milliseconds."},
	{substr: []string{"Partial resynchronization not possible (no cached primary)",
		"Partial resynchronization not possible (no cached master)"},
		class: lsClassReplica, sev: lsSevInfo, label: "No cached primary — a full sync is the only option",
		means: "This replica has just been told who to follow and has never followed anybody, so there is no stream to resume. A full sync here is ordinary rather than a symptom."},
	{substr: []string{"Connection with master lost", "Connection with primary lost"},
		class: lsClassReplica, sev: lsSevWarn, label: "The link to the primary broke",
		means: "The replica lost its primary. It keeps serving whatever it had — stale from this instant, and with nothing in the log to say how stale — and retries until it reconnects or somebody promotes it."},
	{substr: []string{"Connecting to MASTER", "Connecting to PRIMARY", "Reconnecting to PRIMARY",
		"Reconnecting to MASTER"},
		class: lsClassReplica, sev: lsSevInfo, label: "Connecting to the primary",
		means: "The replica is opening a replication connection. Repeated every second for as long as the primary is unreachable, so the count and span on this row are the length of the outage."},
	{substr: []string{"MASTER aborted replication", "Error condition on socket for SYNC"},
		class: lsClassReplica, sev: lsSevWarn, label: "The sync attempt failed",
		means: "The replica reached the primary and the exchange did not complete. Connection refused here means the primary's process is gone rather than merely busy."},
	{substr: []string{"Caching the disconnected primary state", "Caching the disconnected master state"},
		class: lsClassReplica, sev: lsSevInfo, label: "Caching the primary's state for a partial resync",
		means: "The replica is keeping what it needs to resume the stream rather than recopy the dataset, in case the primary comes back."},
	{substr: []string{"Discarding previously cached primary state", "Discarding previously cached master state"},
		class: lsClassReplica, sev: lsSevInfo, label: "Discarded the cached primary state",
		means: "The chance of a partial resync is gone: whatever happens next, this node will not be resuming that stream."},

	// ---- replication, from the primary's side ----------------------------------------------
	{substr: []string{"Replica ", "asks for synchronization"}, needSubstr: []string{"asks for synchronization"},
		class: lsClassReplica, sev: lsSevInfo, label: "A replica asked to sync",
		means: "Written by the PRIMARY. The address on this line is the replica; whether it got a partial or a full transfer is the next record."},
	{substr: []string{"Starting BGSAVE for SYNC"},
		class: lsClassTransfer, sev: lsSevWarn, label: "Forking to serialise the dataset for a replica",
		means: "The primary is forking to produce a copy of everything it holds. The fork itself is where a Valkey primary is most likely to fall over: it needs enough memory for the pages the parent then modifies, which is why the overcommit warning at every start is not the noise it looks like."},
	{substr: []string{"Synchronization with replica", "Streamed RDB transfer with replica",
		"Background RDB transfer terminated with success"},
		class: lsClassTransfer, sev: lsSevOK, label: "A replica was synced",
		means: "The transfer finished. From here that replica is following the live stream."},
	{substr: []string{"Connection with replica", "Connection with slave"},
		needSubstr: []string{"lost"},
		class:      lsClassReplica, sev: lsSevWarn, label: "A replica disconnected",
		means: "Written by the PRIMARY when a replica's connection ended. Which replica is on the line; why is in that replica's own log, and usually nowhere else."},
	{substr: []string{"Replication backlog created"},
		class: lsClassReplica, sev: lsSevInfo, label: "Replication backlog created",
		means: "The buffer that makes a partial resync possible. Its size is repl-backlog-size, and it is the single setting that decides whether a replica coming back after a blip costs a full dataset copy or nothing at all."},
	{substr: []string{"Replication backlog freed"},
		class: lsClassReplica, sev: lsSevWarn, label: "Replication backlog freed — no replicas left",
		means: "Every replica has been gone for longer than repl-backlog-ttl, so the primary threw the buffer away. Any replica that returns now is guaranteed a FULL resync, however briefly it was away."},

	// ---- promotion and role changes --------------------------------------------------------
	{substr: []string{"MASTER MODE enabled", "PRIMARY MODE enabled"},
		class: lsClassMember, sev: lsSevWarn, label: "Promoted to primary by hand",
		means: "Somebody ran REPLICAOF NO ONE against this node. Nothing coordinated it: in standalone replication there is no election and no arbitration, so if the old primary is still up and reachable there are now two servers accepting writes for the same dataset, and Valkey will not notice."},
	{substr: []string{"REPLICAOF ", "SLAVEOF "},
		needSubstr: []string{"enabled"},
		class:      lsClassMember, sev: lsSevWarn, label: "Told to follow a new primary",
		means: "This node was pointed at a primary by hand or by a tool. Everything it held that the new primary does not have is about to be discarded."},
	{substr: []string{"Setting secondary replication ID"},
		class: lsClassMember, sev: lsSevInfo, label: "Replication ID changed — this node was promoted or reparented",
		means: "Valkey keeps a second replication id so replicas of the OLD primary can partially resync with this one instead of recopying everything. The offset on this line is where the two histories part company."},
	{substr: []string{"Before turning into a replica, using my own primary parameters to synthesize a cached primary",
		"Before turning into a replica, using my own master parameters"},
		class: lsClassMember, sev: lsSevInfo, label: "Demoting — trying to keep a partial resync possible",
		means: "A primary being turned into a replica is manufacturing the state it would have had as a replica, so it can resume from an offset rather than recopy the dataset. It works only if its history and the new primary's actually share a prefix."},

	// ---- Valkey Cluster: failure detection ---------------------------------------------------
	{substr: []string{"possibly failing"},
		class: lsClassNetwork, sev: lsSevWarn, label: "A peer stopped answering",
		means: "This node has not heard from a peer within cluster-node-timeout and has flagged it PFAIL — a private suspicion, not yet a cluster decision. It becomes one only when a majority of primaries agree."},
	{substr: []string{"reported node", "as not reachable"}, needSubstr: []string{"as not reachable"},
		class: lsClassNetwork, sev: lsSevWarn, label: "Another node reports a peer unreachable",
		means: "Gossip: some other primary is also failing to reach that node. These accumulating is what turns one node's suspicion into the cluster's verdict."},
	{substr: []string{"Marking node", "as failing"}, needSubstr: []string{"as failing"},
		class: lsClassMember, sev: lsSevBad, label: "A node was declared FAILED (quorum reached)",
		means: "A majority of primaries agreed that node is gone, so the cluster now treats it as dead: its replica may be promoted, and if it has none its slots are uncovered. Note what this record does NOT tell you — a node that was cleanly shut down produces exactly this same line. Valkey Cluster has no goodbye."},
	{substr: []string{"FAIL message received"},
		class: lsClassMember, sev: lsSevWarn, label: "Told that a node has failed",
		means: "Another node reached the verdict first and broadcast it. This node is accepting somebody else's conclusion rather than forming its own."},
	{substr: []string{"Clear FAIL state for node"},
		class: lsClassMember, sev: lsSevOK, label: "A failed node is back",
		means: "The node is reachable again and the cluster has stopped treating it as dead. If its slots were uncovered, they are covered again from here."},
	{substr: []string{"Sending MEET packet to node"},
		class: lsClassNetwork, sev: lsSevInfo, label: "Re-establishing a link to a peer",
		means: "The gossip link to that node was one-way, so this node is rebuilding it. Ordinary after any restart; continuous, it is a network that drops connections."},

	// ---- Valkey Cluster: the outage --------------------------------------------------------
	{substr: []string{"Cluster state changed: fail"},
		class: lsClassQuorum, sev: lsSevBad, label: "Cluster state FAIL — the cluster stopped serving",
		means: "The cluster had been healthy and is not any more. Every client of every node now gets CLUSTERDOWN for keys in the missing slots — and with cluster-require-full-coverage at its default of yes, for EVERY key. The individual nodes are fine; nothing is being served."},
	{substr: []string{"Cluster state changed: ok"},
		class: lsClassQuorum, sev: lsSevOK, label: "Cluster state OK — serving again",
		means: "Every hash slot is covered by a reachable node and the cluster answers normally."},
	{substr: []string{"At least one hash slot is not served"},
		class: lsClassQuorum, sev: lsSevWarn, label: "Slots uncovered — the cluster is refusing clients",
		means: "A shard has no reachable node to serve its slots, either because its primary died with no replica or because the replica could not be promoted. cluster-require-full-coverage decides how bad this is: at its default, yes, the whole cluster refuses every command rather than the affected slots only."},
	{substr: []string{"I am part of a minority partition"},
		class: lsClassQuorum, sev: lsSevWarn, label: "In the minority side of a partition",
		means: "This node cannot see a majority of the cluster's primaries, so it stops serving on purpose. Written on every node during a cluster's first seconds too, before anybody has met anybody — there it means nothing, and only its position in the timeline tells the two apart."},

	// ---- Valkey Cluster: elections and failover ------------------------------------------------
	{substr: []string{"Start of election delayed"},
		class: lsClassMember, sev: lsSevWarn, label: "A replica is preparing to stand for election",
		means: "The replica has noticed its primary is failed and is waiting out a deliberate delay before standing, so that the best-caught-up replica goes first. The rank in this line is its place in that queue and the offset is how much of the primary's stream it has."},
	{substr: []string{"Starting a failover election for epoch"},
		class: lsClassMember, sev: lsSevWarn, label: "Failover election started",
		means: "A replica is asking the cluster's primaries to vote it into its dead primary's place. Everything from here to the promotion is the window in which that shard takes no writes."},
	{substr: []string{"Failover election won"},
		class: lsClassMember, sev: lsSevWarn, label: "Election won — this node is the new primary",
		means: "A majority of primaries voted for this replica and it now owns its old primary's slots. Anything the old primary accepted but had not yet sent is gone: Valkey replication is asynchronous, and the election does not wait for it."},
	{substr: []string{"Failover auth granted"},
		class: lsClassMember, sev: lsSevInfo, label: "Voted for a replica's promotion",
		means: "Written by a voting PRIMARY, not by the candidate. Each primary votes once per epoch, which is what stops two replicas of the same shard both winning."},
	{substr: []string{"Failover auth denied"},
		class: lsClassMember, sev: lsSevWarn, label: "Refused to vote for a promotion",
		means: "A primary declined the request — usually because it already voted this epoch, or because the candidate's data is too far behind. Enough of these and the failover does not happen at all."},
	{substr: []string{"Currently unable to failover"},
		class: lsClassMember, sev: lsSevWarn, label: "Cannot fail over yet",
		means: "The replica wants to take over and cannot. The reason is on the line, and 'waiting for votes' that never resolves is the shape of a shard that stays down: no majority of primaries is reachable to authorise anybody."},
	{substr: []string{"Needed quorum:"},
		class: lsClassMember, sev: lsSevInfo, label: "Votes needed and received",
		means: "The arithmetic of the election. Votes come from PRIMARIES only — replicas do not vote — so a cluster with three shards needs two surviving primaries to promote anything at all."},
	{substr: []string{"Manual failover user request accepted", "Manual failover requested by replica"},
		class: lsClassMember, sev: lsSevWarn, label: "Manual failover requested",
		means: "Somebody ran CLUSTER FAILOVER. Unlike an automatic one this is coordinated: the primary pauses writes, the replica catches up completely, and only then do they swap — so no writes are lost, and writes do stop for the length of it."},
	{substr: []string{"All primary replication stream processed, manual failover can start",
		"All master replication stream processed"},
		class: lsClassMember, sev: lsSevInfo, label: "Caught up — the manual failover can proceed",
		means: "The candidate has every byte the primary had. This record is the difference between a manual failover and an automatic one: it is the proof that nothing is being discarded."},
	{substr: []string{"Setting myself to primary in shard", "Setting myself to master in shard"},
		class: lsClassMember, sev: lsSevWarn, label: "Took over the shard",
		means: "This node now owns the shard's slots. The old primary is named on the line, and if it ever comes back it will be told to follow this node — discarding anything it accepted after the split."},
	{substr: []string{"Configuration change detected. Reconfiguring myself as a replica"},
		class: lsClassMember, sev: lsSevBad, label: "Demoted — reconfigured as a replica of the node that replaced it",
		means: "This node was a primary, the cluster promoted somebody else while it was away, and on returning it has been told to follow them. Everything it accepted after it lost contact and before it stopped is discarded: those writes were acknowledged to clients and no longer exist. This is Valkey Cluster's rollback, and unlike MongoDB's it writes no file of what was lost."},
	{substr: []string{"is now a replica of node"},
		class: lsClassMember, sev: lsSevInfo, label: "A node became a replica of another",
		means: "The gossip view changing: some node in the cluster now follows another. Written by every node about every other, so one reparenting appears once per member."},
	{substr: []string{"is no longer primary of shard", "is no longer master of shard"},
		class: lsClassMember, sev: lsSevInfo, label: "A node gave up its slots",
		means: "That node stopped owning the slots named here — because it was demoted, or because the slots moved."},
	{substr: []string{"configEpoch set to"},
		class: lsClassMember, sev: lsSevInfo, label: "Configuration epoch changed",
		means: "The epoch is how Valkey Cluster settles disagreements about who owns a slot: the higher epoch wins. It goes up at every promotion, which is why it is the cleanest way to count how many times a cluster has failed over."},
	{substr: []string{"Mismatch in topology information"},
		class: lsClassMember, sev: lsSevInfo, label: "Topology mismatch with a peer",
		means: "Two nodes disagree about the shape of a shard. Ordinary while a cluster is forming or immediately after a failover; persistent, it is a cluster that has not converged."},
	{substr: []string{"Cluster meet"},
		class: lsClassMember, sev: lsSevInfo, label: "A node was introduced to the cluster",
		means: "CLUSTER MEET — somebody added this node's address to the cluster by hand or through valkey-cli --cluster."},
	{substr: []string{"IP address for this node updated"},
		class: lsClassNetwork, sev: lsSevInfo, label: "This node's address changed",
		means: "The address the cluster knows this node by has changed. In a container this happens on every restart, and it is why cluster-announce-ip exists — without it, a rescheduled node can become unreachable to peers that still hold its old address."},

	// ---- persistence, and the failure that silently stops writes ----------------------------
	{substr: []string{"Background saving error", "Can't save in background",
		"Write error saving DB on disk", "Background AOF rewrite terminated with error"},
		class: lsClassStorage, sev: lsSevBad, label: "Background save FAILED",
		means: "The snapshot could not be written. With stop-writes-on-bgsave-error at its default of yes, this server is now refusing every write with MISCONF — and the log does NOT say so. The refusal is visible only to clients; nothing further is written here until a save succeeds."},
	{substr: []string{"Failed opening the temp RDB file", "Write error while writing the AOF"},
		class: lsClassStorage, sev: lsSevBad, label: "Could not write the snapshot file",
		means: "The reason the save failed, written by the forked child: a full disk, a permission problem, or a directory that is not there. The path on the line is exactly where it tried."},
	{substr: []string{"Background saving started"},
		class: lsClassStorage, sev: lsSevInfo, label: "Snapshot started",
		means: "A fork to write the dataset to disk. The fork is where memory use spikes — the child shares the parent's pages until the parent writes to them."},
	{substr: []string{"Background saving terminated with success", "DB saved on disk"},
		class: lsClassStorage, sev: lsSevInfo, label: "Snapshot complete"},
	{substr: []string{"Background AOF rewrite finished successfully",
		"Background append only file rewriting started"},
		class: lsClassStorage, sev: lsSevInfo, label: "AOF rewrite"},
	{substr: []string{"Fork CoW for"},
		class: lsClassStorage, sev: lsSevInfo, label: "Copy-on-write cost of the fork",
		means: "How much memory the fork actually cost. A peak approaching the dataset size means the server was being written to hard throughout the save, and that a host sized for the dataset is not sized for saving it."},
	{substr: []string{"changes in ", "seconds. Saving..."}, needSubstr: []string{"seconds. Saving..."},
		class: lsClassStorage, sev: lsSevInfo, label: "Scheduled snapshot triggered",
		means: "A `save` rule fired: this many changes accumulated within this many seconds."},

	// ---- the host warnings, which are ignored until the day they are the cause ---------------
	{substr: []string{"Memory overcommit must be enabled", "overcommit_memory"},
		overLevel: true, class: lsClassConfig, sev: lsSevInfo, label: "Host: vm.overcommit_memory is not 1",
		means: "Written at `#`, warning level, on every start of every healthy Valkey — which is why it is filed as background here rather than allowed to colour the lane. It matters exactly once: when a fork for a snapshot or a replica sync fails for want of memory the kernel would have let it have. If this server also has a failed background save in its log, this line is the explanation."},
	{substr: []string{"Transparent Huge Pages", "THP"},
		overLevel: true, class: lsClassConfig, sev: lsSevInfo, label: "Host: transparent huge pages are enabled",
		means: "THP makes the copy-on-write cost of every snapshot fork much worse and adds latency spikes that no Valkey metric explains. Also boilerplate at every start, so also background here."},
	{substr: []string{"WARNING: Changing databases number"},
		overLevel: true, class: lsClassConfig, sev: lsSevInfo, label: "Cluster mode forces one database",
		means: "Valkey Cluster supports database 0 only. Written at warning level on every cluster node's every start, and it means nothing has gone wrong."},
	{substr: []string{"Supervised by systemd"},
		overLevel: true, class: lsClassConfig, sev: lsSevInfo, label: "Supervised by systemd"},
	{substr: []string{"Increased maximum number of open files", "TCP backlog"},
		overLevel: true, class: lsClassConfig, sev: lsSevInfo, label: "Host limit adjusted at start",
		means: "Valkey raised or complained about a host limit on the way up. The ones it could not fix are the ones worth acting on."},

	// ---- clients ---------------------------------------------------------------------------
	{substr: []string{"max number of clients reached"},
		class: lsClassClient, sev: lsSevBad, label: "maxclients reached — connections refused",
		means: "The server is refusing new connections while serving its existing ones perfectly. From outside it looks like the node is down; from inside it looks healthy. Almost always an application that opens connections without a pool."},
	{substr: []string{"Protocol error"},
		class: lsClassClient, sev: lsSevWarn, label: "Protocol error — a client was disconnected",
		means: "Something spoke to the port that was not RESP, or a client sent a malformed command and was dropped. A steady trickle is usually a health checker or a port scanner."},
	{substr: []string{"Possible SECURITY ATTACK detected", "Cross Protocol Scripting"},
		class: lsClassSecurity, sev: lsSevWarn, label: "Cross-protocol request rejected",
		means: "Something sent an HTTP request to the Valkey port. Usually a misconfigured probe pointed at the wrong port; occasionally what it says it is."},

	// ---- the crash, on the rare occasions Valkey gets to write one ----------------------------
	{substr: []string{"crashed by signal", "Guru Meditation", "BUG REPORT START", "ASSERTION FAILED"},
		class: lsClassCrash, sev: lsSevBad, label: "Valkey crashed",
		means: "The server died on a signal it could catch and managed to write a report on the way out. The stack trace under it is the evidence; note that the far more common ways a Valkey dies — SIGKILL and the OOM killer — catch no signal and write nothing at all here."},
	{substr: []string{"Out of memory allocating"},
		class: lsClassStorage, sev: lsSevBad, label: "Allocation failed — out of memory",
		means: "malloc returned nothing and Valkey cannot continue. Distinct from hitting maxmemory, which is a policy the server applies deliberately and does not log at all: this is the host running out."},
}

// ---------------------------------------------------------------- systemd's half
//
// A separate catalogue for the same reason Patroni's is separate from PostgreSQL's: these
// are systemd's sentences, not Valkey's, and running one list over both files matches the
// wrong things. It is a short list because only a few of systemd's records say anything the
// database log does not — but one of them says the most important thing in the file.
var lsValkeySystemdRules = []lsRule{
	{substr: []string{"Main process exited, code=killed, status=9/KILL"},
		class: lsClassCrash, sev: lsSevBad, label: "systemd: the server was KILLED (SIGKILL)",
		means: "The one record that says so. A SIGKILLed valkey-server writes nothing whatsoever — no crash report, no last words — so without systemd's half of this journal the log of a killed node is simply a log that stops. Everything the server held that was not persisted is gone, and whatever killed it (the OOM killer, an orchestrator, a person) is recorded somewhere else entirely."},
	{substr: []string{"Main process exited, code=killed"},
		notSubstr: []string{"status=9/KILL"},
		class:     lsClassCrash, sev: lsSevBad, label: "systemd: the server died on a signal",
		means: "The process was terminated by a signal rather than exiting. The signal number is on the line and it is the best clue available to what did it."},
	{substr: []string{"Main process exited, code=dumped"},
		class: lsClassCrash, sev: lsSevBad, label: "systemd: the server dumped core",
		means: "A hard crash with a core file, if the host is configured to keep one."},
	{substr: []string{"Failed with result"},
		class: lsClassCrash, sev: lsSevWarn, label: "systemd: the unit failed",
		means: "systemd's verdict on the run that just ended. 'signal' means it was killed; 'exit-code' means valkey-server chose to exit, which almost always means it could not start."},
	{substr: []string{"Scheduled restart job"},
		class: lsClassStartup, sev: lsSevWarn, label: "systemd: restarting the server",
		means: "systemd is bringing it back. This is why a killed node can be missing for under a second and leave no trace anywhere else — the restart counter on this line is how many times it has happened, and a counter climbing through a window is a crash loop that the Valkey log alone shows only as repeated clean starts."},
	{substr: []string{"Sending signal SIGKILL", "Sent signal SIGKILL", "Killing process"},
		class: lsClassShutdown, sev: lsSevWarn, label: "systemd: sending SIGKILL",
		means: "systemd escalated to SIGKILL — either because somebody asked it to, or because the unit did not stop within TimeoutStopSec. The second case loses whatever had not been persisted."},
	{substr: []string{"Stopping "},
		class: lsClassShutdown, sev: lsSevInfo, label: "systemd: stopping the server",
		means: "A deliberate stop. Everything below this until the next start belongs to a server that was asked to go away."},
	{substr: []string{"Stopped "},
		class: lsClassShutdown, sev: lsSevInfo, label: "systemd: stopped",
		means: "The unit is down and stays down until something starts it."},
	{substr: []string{"Starting "},
		class: lsClassStartup, sev: lsSevInfo, label: "systemd: starting the server"},
	{substr: []string{"Started "},
		class: lsClassStartup, sev: lsSevOK, label: "systemd: started",
		means: "systemd considers the unit up. On a Type=notify unit this means valkey-server called sd_notify, which it only does with `supervised systemd` in the config — without that directive the unit sits in 'activating' forever while the server serves perfectly."},
	{substr: []string{"Deactivated successfully"},
		class: lsClassShutdown, sev: lsSevInfo, label: "systemd: deactivated cleanly"},
}

// ---------------------------------------------------------------- classify

// lsClassifyValkey turns one folded record into an event, or drops it.
func lsClassifyValkey(r lsRecord) (lsEvent, bool) {
	rules := lsValkeyRules
	if r.Subsys == lsSubsysVKSysd {
		rules = lsValkeySystemdRules
	}
	e := lsEvent{
		Line: r.Line, TS: r.TS, Approx: r.Approx, Time: r.Time, Level: r.Level,
		Subsys: r.Subsys, Code: r.Code,
		Class: lsClassOther, Sev: lsSevInfo, Label: lsTruncateLabel(r.Text), Message: r.Text,
		Detail: strings.Join(r.Body, "\n"),
	}
	for _, rule := range rules {
		if !lsRuleMatches(rule, r) {
			continue
		}
		e.Class, e.Sev, e.Label, e.Meaning = rule.class, rule.sev, rule.label, rule.means
		// The level is a floor everywhere else in this package. For Valkey it is worse than
		// useless: `#` is the top of its scale and the thing most often written at it is the
		// vm.overcommit_memory hint, on every start of every healthy server. Applying the
		// floor here painted 17 of the corpus's healthy starts amber. So the floor applies
		// only to records the catalogue did NOT recognise, where the server's own opinion is
		// the only one available.
		lsEnrichValkey(r, &e)
		return e, true
	}
	// Unrecognised. Keep what Valkey itself marked as a warning and drop the rest — on a busy
	// server the remainder is overwhelmingly per-connection and per-snapshot chatter.
	if lsSevRank[lsLevelFloor(r.Level)] >= lsSevRank[lsSevWarn] {
		e.Sev = lsLevelFloor(r.Level)
		return e, true
	}
	return lsEvent{}, false
}

// lsValkeyPeer finds the node or address a record is about.
//
// Valkey Cluster names peers by their 40-character node id, sometimes followed by the
// announced address in brackets — and on a node with no announce name those brackets are
// empty, which is why the id itself has to be the identifier of record.
var (
	lsValkeyNodeID = regexp.MustCompile(`\b([0-9a-f]{40})\b`)
	lsValkeyAddr   = regexp.MustCompile(`\b(\d{1,3}(?:\.\d{1,3}){3}):(\d+)\b`)
	lsValkeyEpoch  = regexp.MustCompile(`epoch (\d+)`)
)

// lsEnrichValkey pulls the structured facts out of a matched record.
func lsEnrichValkey(r lsRecord, e *lsEvent) {
	switch e.Class {
	case lsClassMember, lsClassNetwork, lsClassReplica, lsClassTransfer:
		if m := lsValkeyNodeID.FindStringSubmatch(r.Text); m != nil {
			e.Peer = m[1]
		} else if m := lsValkeyAddr.FindStringSubmatch(r.Text); m != nil {
			e.Peer = m[1] + ":" + m[2]
		}
	}
	// The epoch is Valkey Cluster's count of how many times ownership has been settled by
	// force. It goes up at every promotion and nowhere else, which makes it the cleanest
	// available answer to "how many times has this cluster failed over".
	if m := lsValkeyEpoch.FindStringSubmatch(r.Text); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		e.Seqno = n
	}
}

// ---------------------------------------------------------------- resolve

// lsResolveValkey fills in the state track.
//
// This is the shortest resolver in the package and the most reliable, and both facts have
// the same cause: Valkey stamps the role on every line. Where the other engines have to pair
// a transition with a much later one and guess at what came before the fragment began, here
// the answer is in the header of whatever record is nearest.
//
// Two things override the role, and both are cases where the role is true and irrelevant:
//
//   - LOADING. A server reading its dataset off disk reports M, is listening, and refuses
//     every command with -LOADING. The role is right; the node is not serving.
//   - CLUSTERDOWN. A cluster member whose cluster has uncovered slots reports M or S,
//     is completely healthy, and refuses every command. This is the state that makes Valkey
//     Cluster worth a vocabulary of its own — a lane of perfectly good nodes, none of which
//     is answering, and no single node's log saying why.
func lsResolveValkey(flavour string, events []lsEvent) {
	order := make([]int, len(events))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return events[order[a]].TS < events[order[b]].TS })

	clusterDown, loading, running := false, false, false
	state := ""
	for _, i := range order {
		e := &events[i]
		// The forked child is not the server. Its records are kept and shown — a persistence
		// failure is written by the child and is the most important thing in the file when it
		// happens — but it must not touch the lane: the child is always "C", and letting it
		// through would drop the server out of PRIMARY for the length of every snapshot.
		if e.Subsys == lsSubsysVKChild {
			e.State = state
			continue
		}
		switch {
		case e.Class == lsClassCrash && e.Sev == lsSevBad:
			state, running, clusterDown, loading = lsStateDown, false, false, false
		case e.Label == "systemd: stopped" || e.Label == "Stopped cleanly" ||
			e.Label == "systemd: deactivated cleanly":
			state, running, clusterDown, loading = lsStateDown, false, false, false
		case e.Class == lsClassStartup && strings.HasPrefix(e.Label, "Valkey starting"):
			state, running, clusterDown, loading = lsStateStarting, false, false, false
		case e.Label == "Loading the dataset":
			loading = true
		case e.Label == "Dataset loaded":
			loading = false
		case e.Label == "Ready — accepting connections":
			running, loading = true, false
		case e.Label == "Cluster state FAIL — the cluster stopped serving":
			clusterDown = true
		case e.Label == "Cluster state OK — serving again":
			clusterDown = false
		case e.Label == "Full resync — the whole dataset is being copied":
			loading = true // a replica reloading from scratch is not answering for fresh data
		case e.Label == "Replica finished syncing":
			loading = false
		}

		switch {
		case state == lsStateDown || state == lsStateStarting:
			// A server that has not reached "Ready" yet keeps the lifecycle state: the role
			// letter is on its startup records too, and believing it would report a node as
			// PRIMARY while it was still reading its config.
			if running {
				state = lsValkeyRoleState(flavour, e.Code)
			}
		case loading:
			state = lsStateVKLoading
		case clusterDown:
			state = lsStateVKDown
		default:
			if s := lsValkeyRoleState(flavour, e.Code); s != "" {
				state = s
			}
		}
		e.State = state
	}
}

// lsValkeyRoleState turns the header's role letter into a state.
//
// On a standalone with no replication anywhere in the file, "primary" is a distinction
// without a difference — there is nothing to be primary OF — so such a node is simply
// RUNNING. Calling it PRIMARY would put a word on the lane that implies a topology the
// server is not in.
func lsValkeyRoleState(flavour, role string) string {
	switch role {
	case "M":
		if flavour == lsFlavourValkey {
			return lsStateUp
		}
		return lsStatePrimaryM
	case "S":
		return lsStateVKReplica
	}
	return ""
}

// ---------------------------------------------------------------- sniffing

// lsSniffValkey reports whether this looks like a Valkey log at all.
func lsSniffValkey(data string) bool {
	n := 0
	for _, line := range strings.SplitN(data, "\n", 400) {
		if lsValkeyHeader.MatchString(line) {
			n++
			if n >= 2 {
				return true
			}
		}
	}
	return false
}

// lsSniffValkeyFlavour decides which of the three vocabularies a source speaks.
//
// Cluster first, because a cluster member is also a replication participant and would
// otherwise be filed as one — and the findings differ. The evidence for a cluster is the
// cluster bus doing something only a cluster does; the evidence for replication is a role
// letter of S, or either side of a sync exchange. A file with neither is one server on its
// own, and it is told nothing about clusters it is not in.
func lsSniffValkeyFlavour(recs []lsRecord) string {
	repl := false
	for _, r := range recs {
		if r.Subsys == lsSubsysVKSysd {
			continue
		}
		switch {
		case strings.Contains(r.Text, "Running mode=cluster"),
			strings.Contains(r.Text, "Cluster state changed"),
			strings.Contains(r.Text, "configEpoch set to"),
			strings.Contains(r.Text, "No cluster configuration found"),
			strings.Contains(r.Text, "Node configuration loaded"),
			strings.Contains(r.Text, "in shard "):
			return lsFlavourValkeyCluster
		case r.Code == "S",
			strings.Contains(r.Text, "asks for synchronization"),
			strings.Contains(r.Text, "REPLICA sync"),
			strings.Contains(r.Text, "Replication backlog created"),
			strings.Contains(r.Text, "MODE enabled"),
			strings.Contains(r.Text, "Full resync"):
			repl = true
		}
	}
	if repl {
		return lsFlavourValkeyRepl
	}
	return lsFlavourValkey
}

// lsIsValkey reports whether a flavour is one of Valkey's three.
func lsIsValkey(flavour string) bool {
	return flavour == lsFlavourValkey || flavour == lsFlavourValkeyRepl ||
		flavour == lsFlavourValkeyCluster
}
