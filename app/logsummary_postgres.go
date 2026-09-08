package main

// logsummary_postgres.go — PostgreSQL, streaming replication and Patroni.
//
// The fifth cluster vocabulary, and the one with the weakest guarantee of the lot.
//
// MongoDB gives every message a numeric id and promises it is stable. MySQL gives most of
// them an MY-nnnnnn code. PostgreSQL gives neither: a server log line is a timestamp, a pid,
// a level and a sentence, and the sentence is the only thing to match on. Worse, it is
// TRANSLATED — a server running with lc_messages set to anything but English writes a log
// this catalogue cannot read at all, which is a limitation worth stating rather than
// discovering.
//
// There is one escape and the catalogue uses it wherever it can. PostgreSQL has SQLSTATE,
// a five-character code that is defined by the SQL standard and by the manual, does not
// change between releases, and is not translated. It reaches the log only if the operator
// puts %e in log_line_prefix, which is not the default and which dbcanvas does not set
// either — so every rule that has a SQLSTATE carries BOTH, and a server configured with %e
// gets the robust match while a server without it falls back to the English. lsRule already
// had exactly this shape for MySQL's codes-or-text, so nothing new was needed to express it.
//
// Three flavours share the file because they share a log. A Patroni member writes an
// ordinary PostgreSQL log plus a second one of its own, and the PostgreSQL half of it is
// indistinguishable from a streaming standby's — which it is. What separates them is what
// else is present: a member with no replication records at all is a standalone, one with
// them is streaming, and one whose Patroni log is in the bundle is a Patroni member. The
// distinction matters because it decides which findings may speak: telling somebody with a
// single server that their cluster has no leader is worse than saying nothing.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The three flavours of a PostgreSQL source.
const (
	lsFlavourPostgres = "postgres" // a server with no replication in evidence
	lsFlavourPGStream = "pgstream" // streaming replication, by itself or under repmgr
	lsFlavourPatroni  = "patroni"  // a Patroni member: PostgreSQL plus Patroni's own log
)

// lsPGHeader matches a PostgreSQL server-log header.
//
// Deliberately a superset of the Packet Inspector's pattern rather than a copy of it,
// because this one also has to find the SQLSTATE. %e renders as five alphanumerics and sits
// wherever the operator put it; the common placements are right before the level and right
// after the pid, so an optional five-character token is tolerated in both positions rather
// than demanded in either.
//
// The zone is required to be present but not to be UTC — a server with log_timezone set to
// a real zone writes an abbreviation or a numeric offset, and a pattern accepting only "UTC"
// would reject every line such a server writes.
var lsPGHeader = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:[.,]\d+)?) ([A-Z]{2,5}|[+-]\d{2}(?::?\d{2})?)\s+` + // 1 time, 2 zone
		`\[(\d+)\]\s*` + // 3 pid
		`(?:\[[\d-]+\]\s*)?` + // an optional %l line number
		`(?:([0-9A-Z]{5})\s+)?` + // 4 an optional SQLSTATE in the %e-after-pid placement
		`(?:([^\s:\[\]]+)\s+)?` + // 5 an optional user@database
		`(?:([0-9A-Z]{5})\s+)?` + // 6 an optional SQLSTATE in the %e-before-level placement
		`(DEBUG[1-5]?|INFO|NOTICE|WARNING|ERROR|LOG|FATAL|PANIC|DETAIL|HINT|STATEMENT|CONTEXT|QUERY):\s+(.*)$`) // 7 level, 8 message

// lsPatroniHeader matches Patroni's own log, which is Python logging and not PostgreSQL:
//
//	2026-08-15 06:03:00,510 INFO: no action. I am (pat1), the leader with the lock
var lsPatroniHeader = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}[.,]\d+)\s+(DEBUG|INFO|WARNING|ERROR|CRITICAL|EXCEPTION):\s+(.*)$`)

// lsPGContinuation are the levels that belong to the record above them rather than to
// themselves. STATEMENT is the important one: it carries the SQL that failed, which is
// exactly what somebody reading an ERROR wants next and which as its own row would be an
// unexplained fragment.
var lsPGContinuation = map[string]bool{
	"DETAIL": true, "HINT": true, "STATEMENT": true, "CONTEXT": true, "QUERY": true,
}

// lsPGSubsys marks which of the two logs a record came from, so a rule can require one.
const (
	lsSubsysPostgres = "postgres"
	lsSubsysPatroni  = "patroni"
)

// lsFoldPostgres parses a PostgreSQL or Patroni log into records.
//
// "Fold" because a PostgreSQL record is not a line. An ERROR is followed by its DETAIL, its
// HINT and the STATEMENT that caused it, each on its own line at its own level, and a
// statement itself can run to many lines. All of it belongs to the record above, and a
// summary that listed the fragments separately would be unreadable — the STATEMENT is the
// most useful thing in the group and on its own it is a stray piece of SQL.
func lsFoldPostgres(data []byte) []lsRecord {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var out []lsRecord
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := lsPGHeader.FindStringSubmatch(line); m != nil {
			level, msg := m[7], strings.TrimSpace(m[8])
			if lsPGContinuation[level] && len(out) > 0 {
				// Keep the level on the continuation, because "DETAIL: …" and
				// "STATEMENT: …" read very differently and the body is shown verbatim.
				out[len(out)-1].Body = append(out[len(out)-1].Body, level+": "+msg)
				continue
			}
			code := m[4]
			if code == "" {
				code = m[6]
			}
			out = append(out, lsRecord{
				Line: i + 1, TS: lsPGTime(m[1], m[2]), Time: m[1],
				Level: lsPGLevel(level), Code: code, Subsys: lsSubsysPostgres,
				Thread: m[3], Text: msg,
			})
			continue
		}
		if m := lsPatroniHeader.FindStringSubmatch(line); m != nil {
			out = append(out, lsRecord{
				Line: i + 1, TS: lsPGTime(m[1], ""), Time: m[1],
				Level: lsPGLevel(m[2]), Subsys: lsSubsysPatroni, Text: strings.TrimSpace(m[3]),
			})
			continue
		}
		// Neither shape: a continuation of the statement above, or a line PostgreSQL wrote
		// without a prefix at all (the startup banner, a crash report). Attach it rather
		// than drop it — a stack trace under a PANIC is the whole of the evidence.
		if len(out) > 0 {
			out[len(out)-1].Body = append(out[len(out)-1].Body, line)
		}
	}
	return out
}

// lsPGTime parses a header stamp. PostgreSQL writes the zone as an abbreviation or an
// offset; Patroni writes none at all and its log is in the server's local time, the same
// zone the PostgreSQL log beside it uses.
func lsPGTime(stamp, zone string) float64 {
	stamp = strings.Replace(stamp, ",", ".", 1)
	layouts := []string{"2006-01-02 15:04:05.000000", "2006-01-02 15:04:05.000", "2006-01-02 15:04:05"}
	// A numeric offset can be honoured exactly. An abbreviation cannot — "CEST" does not
	// identify a zone unambiguously and Go will not guess — so those are read as UTC, which
	// keeps every record in one file consistent with every other. The timeline is built
	// from differences within a bundle, so a uniform offset moves the whole picture and
	// changes none of the durations.
	if zone != "" && (zone[0] == '+' || zone[0] == '-') {
		for _, l := range layouts {
			if t, err := time.Parse(l+" -0700", stamp+" "+strings.ReplaceAll(zone, ":", "")); err == nil {
				return float64(t.UnixNano()) / 1e9
			}
		}
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, stamp); err == nil {
			return float64(t.UnixNano()) / 1e9
		}
	}
	return 0
}

// lsPGLevel maps PostgreSQL's and Patroni's levels onto the words the rest of the package
// uses. PostgreSQL has more of them than anything else here, and the distinction that
// matters is that LOG is not a problem — it is where PostgreSQL puts its most important
// operational records, including every one about replication and recovery.
func lsPGLevel(level string) string {
	switch level {
	case "PANIC", "FATAL", "CRITICAL", "EXCEPTION":
		return "ERROR"
	case "ERROR":
		return "ERROR"
	case "WARNING":
		return "WARNING"
	case "LOG", "INFO", "NOTICE", "DEBUG", "DEBUG1", "DEBUG2", "DEBUG3", "DEBUG4", "DEBUG5":
		return "NOTE"
	}
	return "NOTE"
}

// lsPGIsFatal reports whether a record's raw level was FATAL or PANIC, which the folded
// level cannot say — both map to ERROR, and the difference between "this session died" and
// "the server died" is worth keeping.
func lsPGIsFatal(text string) bool {
	return strings.HasPrefix(text, "PANIC") || strings.HasPrefix(text, "FATAL")
}

// lsSniffPostgres reports whether this looks like a PostgreSQL or Patroni log at all.
func lsSniffPostgres(data string) bool {
	n := 0
	for _, line := range strings.SplitN(data, "\n", 400) {
		if lsPGHeader.MatchString(line) || lsPatroniHeader.MatchString(line) {
			n++
			if n >= 2 {
				return true
			}
		}
	}
	return false
}

// lsPGNodeName pulls the server's own name out of its log.
//
// PostgreSQL never states it: there is nothing in a server log that says which host wrote
// it. Patroni does, in almost every line it writes — "I am (pat1)" — so a Patroni member
// names itself and a plain PostgreSQL server does not, and the caller falls back to the
// file name. Inventing one would be worse: two files both called "the server" is a summary
// nobody can read, but a wrong name is a summary that misleads.
var lsPatroniSelf = regexp.MustCompile(`I am \(?([A-Za-z0-9_.-]+)\)?`)

func lsPGNodeName(recs []lsRecord) string {
	for _, r := range recs {
		if r.Subsys != lsSubsysPatroni {
			continue
		}
		if m := lsPatroniSelf.FindStringSubmatch(r.Text); m != nil {
			return lsMongoShortHost(m[1])
		}
	}
	return ""
}

// lsPGTimeline extracts a timeline id from the messages that carry one. The timeline is
// PostgreSQL's record of how many times the cluster has been promoted, and a standby on the
// wrong one cannot follow the primary at all.
var lsPGTimelineRe = regexp.MustCompile(`timeline (\d+)`)

func lsPGTimelineOf(text string) int {
	if m := lsPGTimelineRe.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// ---------------------------------------------------------------- the states

// PostgreSQL's states, as its own log states them.
//
// PRIMARY is shared with MongoDB deliberately: it means the same thing in both — the one
// member accepting writes — and a reader moving between a replica set and a Patroni cluster
// should not have to learn two words for it. STANDBY is PostgreSQL's own word and collides
// with nothing.
const (
	lsStateStandby   = "STANDBY"    // streaming and serving reads
	lsStatePGRecover = "RECOVERING" // replaying WAL, not yet consistent — shares Galera's spelling
	lsStatePromoting = "PROMOTING"  // a promote was requested; writes resume when it finishes
)

// ---------------------------------------------------------------- the PostgreSQL catalogue
//
// Every rule below matched a record in a capture from a live three-node Patroni cluster on
// PostgreSQL 16.14, driven through a planned switchover, an unplanned failover with the
// leader SIGKILLed, a whole-DCS outage, a crash, a deadlock, a statement timeout and a
// constraint violation. Nothing here was written from memory: the corpus is
// app/testdata/logsummary/p*.
//
// The `codes` are SQLSTATEs and they only ever match when the operator has put %e in
// log_line_prefix, which is not the default. They are worth carrying anyway — a SQLSTATE is
// defined by the standard, does not change between releases and is not translated, whereas
// every `substr` here is English text that a server with lc_messages set to anything else
// will not produce.
// lsPGFatalIsASession is the note behind every `overLevel` in the catalogue below.
//
// In MySQL and MongoDB the log level is a reliable floor: an [ERROR] is a problem and
// nothing may be filed below it. PostgreSQL does not work that way, and reading it as though
// it did produces a page that is wrong in both directions.
//
//   - FATAL in PostgreSQL means THIS BACKEND is ending, not that the server is failing. A
//     client that connects while the server is still starting gets a FATAL. So does every
//     connection Patroni terminates during a routine switchover, and so does a standby
//     noticing that the primary it was streaming from has gone. On the corpus, taking the
//     level as a floor reported twenty-seven "bad" events for clients arriving a second too
//     early during an ordinary restart.
//   - LOG, meanwhile, is where PostgreSQL puts its most important operational records —
//     every promotion, every timeline switch, every recovery. A page that trusted the level
//     would rank a failover below a mistyped password.
//
// So the rules that know better say so, and the ones that do not still take the floor.
const lsPGFatalIsASession = "PostgreSQL's FATAL ends a session, not the server"

var lsPGRules = []lsRule{
	// ---- lifecycle: what state the server is in ---------------------------------------
	{substr: []string{"database system is ready to accept connections"},
		class: lsClassState, sev: lsSevOK, label: "Ready — accepting writes",
		means: "The server is up as a primary. This is the line that ends a restart or a promotion, and the point at which an application can write again."},
	{substr: []string{"database system is ready to accept read-only connections"},
		class: lsClassState, sev: lsSevOK, label: "Ready — read-only (a standby)",
		means: "The server is up as a standby. It serves reads and refuses every write; an application pointed here that expects to write fails on the first one."},
	{substr: []string{"entering standby mode"},
		class: lsClassState, sev: lsSevInfo, label: "Entering standby mode",
		means: "The server is starting as a standby: it will replay WAL from the primary rather than accept writes of its own."},
	{substr: []string{"starting PostgreSQL"},
		class: lsClassStartup, sev: lsSevInfo, label: "PostgreSQL starting",
		means: "A new postmaster. Everything above this line belongs to a different run of the server."},
	{substr: []string{"received fast shutdown request", "received smart shutdown request"},
		class: lsClassShutdown, sev: lsSevInfo, label: "Shutdown requested",
		means: "Somebody asked the server to stop. On a Patroni member this is usually Patroni itself, as one half of a switchover."},
	{substr: []string{"received immediate shutdown request"},
		class: lsClassShutdown, sev: lsSevWarn, label: "Immediate shutdown — no clean checkpoint",
		means: "The harshest stop PostgreSQL offers: backends are killed and no shutdown checkpoint is written, so the next start has to recover. Nothing is lost, but the restart is slower than a clean one."},
	{substr: []string{"database system is shut down"},
		class: lsClassShutdown, sev: lsSevInfo, label: "Shut down cleanly",
		means: "The server stopped on request and wrote a shutdown checkpoint. A start after this one needs no recovery."},
	{substr: []string{"aborting any active transactions"},
		class: lsClassShutdown, sev: lsSevInfo, label: "Aborting active transactions for shutdown",
		means: "The stop is not waiting for open transactions any more. Whatever was in flight was rolled back."},

	// ---- recovery: was the last stop clean? -------------------------------------------
	{substr: []string{"database system was interrupted; last known up at",
		"database system was interrupted while in recovery"},
		class: lsClassCrash, sev: lsSevBad, label: "Previous shutdown was not clean",
		means: "The last postmaster on this data directory did not stop on request — it was killed, OOM-killed, or the host went away. PostgreSQL replays its WAL from the last checkpoint on the way up, so nothing committed is lost; what it costs is the time to replay, and on a busy server that can be minutes."},
	{substr: []string{"database system was shut down at"},
		class: lsClassStartup, sev: lsSevOK, label: "Previous shutdown was clean",
		means: "The data directory was left in a consistent state, so this start needs no recovery."},
	{substr: []string{"automatic recovery in progress"},
		class: lsClassCrash, sev: lsSevWarn, label: "Crash recovery running",
		means: "WAL is being replayed to bring the data files back to the last committed state. The server accepts no connections until it finishes."},
	{substr: []string{"redo starts at"},
		class: lsClassStartup, sev: lsSevInfo, label: "WAL replay started",
		means: "Recovery is replaying from this point. The distance from here to the end of the WAL is how much work the restart has to do."},
	{substr: []string{"redo done at", "redo is not required"},
		class: lsClassStartup, sev: lsSevInfo, label: "WAL replay finished",
		means: "Recovery has caught up. The elapsed time on this line is what the unclean stop actually cost."},
	{substr: []string{"consistent recovery state reached"},
		class: lsClassState, sev: lsSevOK, label: "Consistent state reached",
		means: "The standby has replayed enough to be internally consistent, and can start answering read-only queries from this point."},
	{substr: []string{"archive recovery complete"},
		class: lsClassState, sev: lsSevInfo, label: "Archive recovery complete",
		means: "Recovery from the archive finished; the server is about to come up in its new role."},

	// ---- promotion and timelines: the part that decides who takes writes ---------------
	//
	// The timeline is PostgreSQL's count of how many times this cluster has been promoted.
	// It is the single most useful number in a failover, because a standby on the wrong one
	// cannot follow the primary at all — and it is in these records and nowhere else.
	{substr: []string{"received promote request"},
		class: lsClassMember, sev: lsSevWarn, label: "Promotion requested",
		means: "Something asked this standby to become the primary — Patroni, repmgr, or a person running pg_ctl promote. Writes do not resume on this line: the server first finishes replaying what it already has."},
	{substr: []string{"selected new timeline ID"},
		class: lsClassMember, sev: lsSevWarn, label: "Promoted — new timeline",
		means: "The promotion completed and the cluster's history forked here. Every other standby must be told to follow the new timeline or it stops replicating; one that had already replayed past this point has diverged and needs pg_rewind or a rebuild."},
	{substr: []string{"new target timeline is"},
		class: lsClassReplica, sev: lsSevInfo, label: "Following a new timeline",
		means: "This standby noticed the promotion and switched to the new history. This is the record that says it survived a failover rather than being orphaned by it."},
	{substr: []string{"fetching timeline history file"},
		class: lsClassReplica, sev: lsSevInfo, label: "Fetching the timeline history",
		means: "The standby is reading how the cluster's history forked, which is how it works out where to start following the new primary."},
	{substr: []string{"last completed transaction was at log time"},
		class: lsClassMember, sev: lsSevInfo, label: "Last transaction before the promotion",
		means: "The wall-clock time of the last transaction this server had replayed when it was promoted. Anything the old primary accepted after this was not received here, and unless it comes back and rewinds, it is gone."},

	// ---- streaming replication ----------------------------------------------------------
	{substr: []string{"started streaming WAL from primary"},
		class: lsClassReplica, sev: lsSevOK, label: "Streaming from the primary",
		means: "Replication is running. The LSN and timeline on this line are where it resumed from."},
	{substr: []string{"restarted WAL streaming at"},
		class: lsClassReplica, sev: lsSevInfo, label: "Streaming restarted",
		means: "The stream was interrupted and picked up again. One of these is unremarkable; a run of them is a link that will not hold."},
	{substr: []string{"replication terminated by primary server"},
		class: lsClassReplica, sev: lsSevWarn, label: "The primary ended the stream",
		means: "The primary closed the replication connection. Normal at a promotion or a clean shutdown of the primary; otherwise it is the primary going away."},
	{substr: []string{"could not receive data from WAL stream"},
		class: lsClassReplica, sev: lsSevWarn, label: "The WAL stream broke",
		means: "The standby lost its connection mid-stream. It retries, so the question is whether a 'started streaming' record follows — and how long the gap was."},
	{substr: []string{"terminating walsender process due to replication timeout"},
		class: lsClassReplica, sev: lsSevWarn, label: "Walsender timed out",
		means: "The primary gave up on a standby that stopped acknowledging. From the primary's side this is the standby disappearing; from the standby's side there is usually nothing at all."},
	{substr: []string{"could not connect to the primary server"},
		overLevel: true, class: lsClassNetwork, sev: lsSevWarn, label: "Cannot reach the primary",
		means: "The standby could not open a replication connection. Repeated every few seconds for as long as it lasts, so the count and span on this row are the length of the outage."},
	{substr: []string{"could not send end-of-streaming message to primary"},
		overLevel: true, class: lsClassReplica, sev: lsSevInfo, label: "Stream ended abruptly",
		means: "The standby was closing the stream and the primary had already gone. Ordinary during a failover — it is the standby noticing the old primary died."},
	{substr: []string{"waiting for WAL to become available"},
		class: lsClassReplica, sev: lsSevInfo, label: "Waiting for WAL",
		means: "The standby has replayed everything it has and is waiting for more. On an idle cluster this is the normal resting state; during an incident, a run of them with no streaming record between is a standby receiving nothing."},
	{substr: []string{"invalid record length", "invalid magic number", "incorrect resource manager"},
		class: lsClassReplica, sev: lsSevInfo, label: "End of readable WAL",
		means: "The standby read to the end of what the primary had written. This looks alarming and is routine — it is how PostgreSQL discovers where the WAL stops, and it appears at every timeline switch."},
	{substr: []string{"unexpected EOF on standby connection"},
		class: lsClassReplica, sev: lsSevInfo, label: "A standby disconnected",
		means: "Written by the PRIMARY when a standby's connection ended without a goodbye. Which standby it was is not in the line; the standby's own log has the other half."},

	// ---- the failure that ends replication permanently -----------------------------------
	{substr: []string{"has already been removed"},
		class: lsClassReplica, sev: lsSevBad, label: "The WAL the standby needs is gone",
		means: "The standby asked for a WAL segment the primary has already recycled. Replication cannot resume from here by any means — the data simply is not there any more. The standby needs a fresh base backup, and the reason it happened is that nothing was holding the WAL: no replication slot, and wal_keep_size too small for how long the standby was away."},
	{substr: []string{"requested WAL segment", "removed"},
		notSubstr: []string{"recycled"}, needSubstr: []string{"has already been removed"},
		class: lsClassReplica, sev: lsSevBad, label: "Requested WAL segment already removed",
		means: "The same failure stated from the primary's side."},
	{substr: []string{"replication slot", "does not exist"},
		needSubstr: []string{"does not exist"},
		class:      lsClassReplica, sev: lsSevBad, label: "The replication slot is missing",
		means: "The standby is configured to use a slot the primary does not have. Until the slot exists, replication cannot start at all — and while it is missing nothing is holding WAL for this standby either."},

	// ---- standby-side conflicts, which look like application bugs -------------------------
	{codes: []string{"40001"}, substr: []string{"canceling statement due to conflict with recovery"},
		overLevel: true, class: lsClassConflict, sev: lsSevWarn, label: "Query cancelled by recovery conflict",
		means: "A read on the standby was killed so that WAL replay could continue. The application sees a query that failed for no reason it did anything wrong — the cause is on the PRIMARY, which vacuumed away rows the standby's query was still reading. max_standby_streaming_delay decides how long replay waits before doing this; hot_standby_feedback stops it happening at the cost of bloat on the primary."},
	{substr: []string{"terminating connection due to conflict with recovery"},
		overLevel: true, class: lsClassConflict, sev: lsSevWarn, label: "Connection killed by recovery conflict",
		means: "Worse than a cancelled query: the whole session was terminated to let replay proceed."},

	// ---- the ordinary refusals, which are what an application actually meets ---------------
	{codes: []string{"57P03"}, substr: []string{"the database system is starting up",
		"the database system is not yet accepting connections", "the database system is in recovery mode"},
		overLevel: true, class: lsClassClient, sev: lsSevInfo, label: "Connections refused — still starting",
		means: "Clients arrived before the server was ready. Harmless in itself, and a useful measure of how long a restart cost: the first and last of these bracket the window in which the server was down as far as any application could tell."},
	{codes: []string{"53300"}, substr: []string{"sorry, too many clients already"},
		class: lsClassClient, sev: lsSevBad, label: "Connection limit reached",
		means: "max_connections is full and the server is refusing new clients while serving the ones it has perfectly well. It looks like an outage from outside and like a healthy server from inside — which is why it is so often misdiagnosed. A pooler is the fix; raising max_connections mostly moves the problem into memory."},
	{codes: []string{"28P01"}, substr: []string{"password authentication failed"},
		overLevel: true, class: lsClassSecurity, sev: lsSevWarn, label: "Password authentication failed",
		means: "A client offered the wrong password. A steady trickle is usually one stale credential in one service; a burst is worth looking at properly."},
	{codes: []string{"28000"}, substr: []string{"no pg_hba.conf entry", "pg_hba.conf rejects connection"},
		overLevel: true, class: lsClassSecurity, sev: lsSevWarn, label: "Refused by pg_hba.conf",
		means: "The client reached the server and no rule allowed it. The address, user and database in the line are exactly what a pg_hba.conf entry would need to match."},
	{substr: []string{"terminating connection due to administrator command"},
		overLevel: true, class: lsClassClient, sev: lsSevInfo, label: "Connection terminated by the server",
		means: "Somebody or something called pg_terminate_backend. During a switchover this is Patroni clearing the way for the demotion, and every one of these was a client whose work was cut off."},

	// ---- locking and statements ------------------------------------------------------------
	{codes: []string{"40P01"}, substr: []string{"deadlock detected"},
		overLevel: true, class: lsClassConflict, sev: lsSevWarn, label: "Deadlock detected",
		means: "Two transactions each held what the other wanted, and PostgreSQL broke the tie by killing one. The victim's application sees a failed transaction it must retry; the DETAIL on this record names both processes and the statements involved."},
	{codes: []string{"57014"}, substr: []string{"canceling statement due to statement timeout"},
		overLevel: true, class: lsClassClient, sev: lsSevWarn, label: "Statement timeout",
		means: "A query ran past statement_timeout and was killed. The timeout is doing its job; whether the query should have been that slow is the actual question."},
	{codes: []string{"55P03"}, substr: []string{"canceling statement due to lock timeout"},
		overLevel: true, class: lsClassConflict, sev: lsSevWarn, label: "Lock timeout",
		means: "A statement gave up waiting for a lock. Usually a schema change queued behind an ordinary transaction, with everything else queued behind IT."},
	{substr: []string{"still waiting for"},
		class: lsClassConflict, sev: lsSevWarn, label: "Waiting on a lock",
		means: "PostgreSQL logs this after deadlock_timeout of waiting, so every one of these is a statement that has already waited a second or more. The DETAIL names what is holding the lock."},

	// ---- the I/O behind latency nothing else explains -----------------------------------------
	{substr: []string{"checkpoints are occurring too frequently"},
		class: lsClassStorage, sev: lsSevWarn, label: "Checkpoints too frequent",
		means: "max_wal_size is too small for this write rate, so PostgreSQL is checkpointing because it ran out of WAL room rather than because the interval elapsed. Every one of those is a burst of writes, and the server gets slower in a way no individual query explains. The message says what to raise it to."},
	{substr: []string{"checkpoint complete"},
		class: lsClassStorage, sev: lsSevInfo, label: "Checkpoint complete",
		means: "The numbers on this line — buffers written, WAL files added and recycled, and the sync time — are the clearest picture of write pressure PostgreSQL offers. A sync time of seconds is a disk that cannot keep up."},
	{substr: []string{"checkpoint starting: shutdown", "checkpoint starting: immediate"},
		class: lsClassStorage, sev: lsSevInfo, label: "Forced checkpoint",
		means: "A checkpoint that was demanded rather than scheduled — a shutdown, a promotion, or a backup."},
	{substr: []string{"restartpoint starting", "restartpoint complete"},
		class: lsClassStorage, sev: lsSevInfo, label: "Restartpoint",
		means: "A standby's version of a checkpoint. It can only happen when the primary has checkpointed, so a standby under replay pressure spends longer between them."},
	{substr: []string{"automatic vacuum of table", "automatic analyze of table"},
		class: lsClassStorage, sev: lsSevInfo, label: "Autovacuum",
		means: "Autovacuum only logs when it runs longer than log_autovacuum_min_duration, so anything here already took a while. The elapsed time and the buffer counts are the I/O behind latency the query log cannot account for."},
	{substr: []string{"to avoid wraparound"},
		class: lsClassStorage, sev: lsSevBad, label: "Anti-wraparound vacuum",
		means: "This vacuum is not optional and will not yield: PostgreSQL runs it to stop transaction ids wrapping around, and if it cannot finish the server eventually refuses to accept writes at all. A table that needs one repeatedly is not being vacuumed enough the rest of the time."},
	{codes: []string{"53100"}, substr: []string{"No space left on device", "could not extend file"},
		class: lsClassStorage, sev: lsSevBad, label: "Out of disk space",
		means: "The filesystem holding the data directory is full. PostgreSQL survives it better than most, but writes fail while it lasts and a full WAL directory can stop the server outright."},
	{codes: []string{"53200"}, substr: []string{"out of memory"},
		class: lsClassStorage, sev: lsSevBad, label: "Out of memory",
		means: "An allocation failed. The statement that caused it is in the record below; work_mem multiplied by the number of concurrent sorts is the usual explanation."},
	{substr: []string{"terminating connection because of crash of another server process"},
		class: lsClassCrash, sev: lsSevBad, label: "A backend crashed — everything was disconnected",
		means: "One backend died in a way PostgreSQL could not contain, so it dropped every other connection and restarted the whole cluster to be certain shared memory is sane. Every application connected at that moment saw its connection vanish."},
	{substr: []string{"background worker", "exited with exit code"},
		needSubstr: []string{"exited with exit code"},
		class:      lsClassOther, sev: lsSevInfo, label: "Background worker exited",
		means: "A worker process ended. Routine at shutdown; in the middle of a run it is worth reading what came just before."},
}

// ---------------------------------------------------------------- the Patroni catalogue
//
// Patroni's log answers a question PostgreSQL's cannot: WHY the cluster changed shape. The
// PostgreSQL log records a promotion; only Patroni records that the promotion happened
// because a lease expired, or because somebody asked for a switchover, or because the leader
// could not reach etcd and stood down on its own.
//
// That last one is the reason this catalogue exists. A Patroni leader that loses the DCS
// demotes itself even though PostgreSQL is perfectly healthy — the cluster loses its primary
// because of a network problem between the leader and etcd, and PostgreSQL's own log shows
// only a clean shutdown with nothing to explain it. Verified: stopping etcd on all three
// members produced exactly that, and the PostgreSQL log for the same window says nothing
// beyond "received fast shutdown request".
var lsPatroniRules = []lsRule{
	// ---- the steady state, which is most of the file -----------------------------------
	{substr: []string{"the leader with the lock"},
		class: lsClassState, sev: lsSevOK, label: "Patroni: leader, holding the lock",
		means: "Patroni is renewing the leader lock in the DCS every loop. This is the healthy resting state of a primary, and it repeats every few seconds — the gap between two of them is a gap in which the lock was not renewed."},
	{substr: []string{"a secondary, and following a leader"},
		class: lsClassState, sev: lsSevOK, label: "Patroni: replica, following the leader",
		means: "The healthy resting state of a replica. The leader it names is the leader as this member understands it, which is what makes two members disagreeing visible."},
	{substr: []string{"Lock owner:"},
		class: lsClassMember, sev: lsSevInfo, label: "Patroni: who holds the leader lock",
		means: "Patroni's view of who leads, written every loop. 'Lock owner: None' is the cluster with no leader at all — no member is accepting writes, and nothing else in either log says so as plainly."},

	// ---- promotion --------------------------------------------------------------------
	{substr: []string{"promoted self to leader"},
		class: lsClassMember, sev: lsSevWarn, label: "Patroni: promoted itself to leader",
		means: "This member took the leader lock and promoted its PostgreSQL. Everything between the previous leader stopping and this line is a window in which the cluster took no writes."},
	{substr: []string{"updated leader lock during promote"},
		class: lsClassMember, sev: lsSevInfo, label: "Patroni: leader lock taken during promotion",
		means: "The promotion is committed in the DCS. From here the rest of the cluster will be told to follow this member."},
	{substr: []string{"Cleaning up failover key"},
		class: lsClassMember, sev: lsSevInfo, label: "Patroni: failover key cleaned up",
		means: "The requested handover finished and Patroni removed the marker that asked for it."},

	// ---- demotion, and the difference between the two kinds -----------------------------
	{substr: []string{"demoting self because DCS is not accessible", "demoted self because DCS is not accessible"},
		class: lsClassQuorum, sev: lsSevBad, label: "Patroni: stood down because it could not reach the DCS",
		means: "The leader demoted itself with nothing wrong with PostgreSQL at all. Patroni will not stay primary while it cannot renew its lock, because it cannot tell 'etcd is unreachable' from 'somebody else has already been promoted' — so it steps down to guarantee there is never more than one primary. The cluster loses writes because of a network problem between this member and etcd, and PostgreSQL's own log shows only a shutdown with no reason attached."},
	{substr: []string{"switchover: demoting myself", "switchover: demote in progress", "Demoting self (graceful)"},
		class: lsClassMember, sev: lsSevWarn, label: "Patroni: demoting for a switchover",
		means: "A planned handover. Writes still stop for the length of it — a switchover is a short outage taken deliberately rather than an avoided one."},
	{substr: []string{"Demoting self (offline)"},
		class: lsClassQuorum, sev: lsSevBad, label: "Patroni: demoting while offline",
		means: "Patroni is standing the leader down without being able to coordinate it, which is what it does when the DCS is gone."},
	{substr: []string{"Leader key released"},
		class: lsClassMember, sev: lsSevWarn, label: "Patroni: released the leader key",
		means: "The lock is free. From this instant until another member takes it, the cluster has no primary."},

	// ---- the races that decide a failover ------------------------------------------------
	{substr: []string{"not healthy enough for leader race"},
		class: lsClassMember, sev: lsSevWarn, label: "Patroni: not eligible to lead",
		means: "This member stood out of the election because it is too far behind. If every member says this, the cluster stays without a primary until one of them catches up — which is a failover that never completes."},
	{substr: []string{"following a different leader because i am not the healthiest node",
		"following new leader after trying and failing to obtain lock"},
		class: lsClassMember, sev: lsSevInfo, label: "Patroni: deferring to another member",
		means: "This member tried for the lock, or considered it, and followed somebody else instead."},
	{substr: []string{"Could not take out TTL lock"},
		class: lsClassQuorum, sev: lsSevWarn, label: "Patroni: could not take the lock",
		means: "The attempt to become leader failed. Usually somebody else got there first; if nobody did, the DCS is the problem."},
	{substr: []string{"received switchover request", "received failover request"},
		class: lsClassMember, sev: lsSevWarn, label: "Patroni: handover requested",
		means: "Somebody asked for this, with patronictl or the REST API. It names the leader it expected and the candidate it wants — a failover that was requested rather than provoked."},

	// ---- the DCS itself --------------------------------------------------------------------
	{substr: []string{"Error communicating with DCS", "DCS is not accessible",
		"EtcdConnectionFailed", "Failed to get list of machines", "Exceeded retry deadline"},
		class: lsClassNetwork, sev: lsSevBad, label: "Patroni: cannot reach the DCS",
		means: "Patroni could not talk to etcd. Nothing about PostgreSQL is wrong when this appears — but the leader cannot renew its lock, so if it lasts longer than the TTL the leader stands down and the cluster stops taking writes. This is the failure mode people are most often surprised by."},
	{substr: []string{"Failed to update leader lock"},
		class: lsClassQuorum, sev: lsSevBad, label: "Patroni: FAILED to renew the leader lock",
		means: "The renewal did not go through. A demotion follows unless it recovers within the TTL, and the cluster is already at risk of having no primary."},
	{substr: []string{"Loop time exceeded"},
		class: lsClassOther, sev: lsSevWarn, label: "Patroni: loop time exceeded",
		means: "Patroni's control loop took longer than its own interval. A member that cannot finish a loop reliably cannot renew its lease reliably either, so a run of these is a failover waiting to happen — usually caused by the host being starved of CPU or the DCS being slow to answer."},
	{substr: []string{"Reconnection allowed, looking for another server", "Selected new etcd server",
		"Retrying on"},
		class: lsClassNetwork, sev: lsSevInfo, label: "Patroni: trying another DCS node",
		means: "Patroni moved to a different etcd member. Routine on its own; continuous, it means no etcd node is answering reliably."},

	// ---- building a member ------------------------------------------------------------------
	{substr: []string{"trying to bootstrap a new cluster", "initialized a new cluster"},
		class: lsClassStartup, sev: lsSevInfo, label: "Patroni: created the cluster",
		means: "The first member initialised an empty cluster. Anything above this line belongs to a different cluster entirely."},
	{substr: []string{"trying to bootstrap from leader", "bootstrap from leader", "bootstrapped from leader",
		"replica has been created using"},
		class: lsClassTransfer, sev: lsSevWarn, label: "Patroni: rebuilding this member from the leader",
		means: "Patroni is taking a fresh copy of the whole database from the leader, because this member had nothing usable or had diverged too far to rewind. The member serves nothing until it finishes and the leader does real work for the duration."},
	{substr: []string{"running pg_rewind", "pg_rewind"},
		class: lsClassConflict, sev: lsSevBad, label: "Patroni: rewinding a diverged member",
		means: "This member had accepted writes the rest of the cluster never agreed to — it was the primary on the wrong side of a failure — and pg_rewind is discarding them so it can follow the new leader. Those writes are gone, and any client that received a commit for one was told something that is no longer true."},
	{substr: []string{"waiting for leader to bootstrap"},
		class: lsClassStartup, sev: lsSevInfo, label: "Patroni: waiting for a leader to exist",
		means: "This member cannot start until somebody creates the cluster. On a first deployment it is ordinary; later it means the leader's data is gone."},
	{substr: []string{"starting as a secondary"},
		class: lsClassState, sev: lsSevInfo, label: "Patroni: starting as a replica",
		means: "Patroni is starting PostgreSQL in standby mode."},
	{substr: []string{"Postgresql is not running"},
		class: lsClassState, sev: lsSevWarn, label: "Patroni: PostgreSQL is not running",
		means: "Patroni looked for its PostgreSQL and did not find it. If Patroni did not stop it deliberately, the postmaster died — and Patroni will restart it, which is why the PostgreSQL log may show a start with no matching stop."},
	{substr: []string{"Local timeline=", "primary_timeline="},
		class: lsClassReplica, sev: lsSevInfo, label: "Patroni: timeline check",
		means: "Patroni comparing this member's timeline against the leader's. A member on an older timeline is one that missed a promotion and has to rewind or rebuild to come back."},
}

// ---------------------------------------------------------------- classify and resolve

// lsClassifyPG turns one folded record into an event, or drops it.
//
// The two catalogues are kept apart rather than merged: Patroni's messages are ordinary
// English sentences and several of them contain words the PostgreSQL rules match on. Running
// one list over both logs filed "Postgresql is not running" as a PostgreSQL lifecycle record
// of a server that was running perfectly.
func lsClassifyPG(r lsRecord) (lsEvent, bool) {
	rules := lsPGRules
	if r.Subsys == lsSubsysPatroni {
		rules = lsPatroniRules
	}
	e := lsEvent{
		Line: r.Line, TS: r.TS, Approx: r.Approx, Time: r.Time, Level: r.Level, Code: r.Code,
		Subsys: r.Subsys,
		Class:  lsClassOther, Sev: lsSevInfo, Label: lsTruncateLabel(r.Text), Message: r.Text,
		Detail: strings.Join(r.Body, "\n"),
	}
	for _, rule := range rules {
		if !lsRuleMatches(rule, r) {
			continue
		}
		e.Class, e.Sev, e.Label, e.Meaning = rule.class, rule.sev, rule.label, rule.means
		// The level is a floor everywhere else in this package. PostgreSQL is the one
		// engine where that is wrong often enough to need an exception on nearly a dozen
		// rules — see lsPGFatalIsASession.
		if !rule.overLevel {
			e.Sev = lsWorse(e.Sev, lsLevelFloor(r.Level))
		}
		lsEnrichPG(r, &e)
		return e, true
	}
	// Unrecognised. Keep what the server itself called a problem and drop the rest, which on
	// a busy PostgreSQL server is overwhelmingly connection accounting and per-statement
	// chatter. PostgreSQL puts its most important operational records at LOG, not at
	// WARNING, so the level floor alone would keep almost nothing — which is exactly why the
	// catalogue above has to do the work.
	if floor := lsLevelFloor(r.Level); lsSevRank[floor] >= lsSevRank[lsSevWarn] {
		e.Sev = floor
		return e, true
	}
	return lsEvent{}, false
}

// lsEnrichPG pulls the numbers out of a matched record. The timeline is the one that
// matters most: it is how a reader tells a standby that survived a failover from one that
// was orphaned by it.
func lsEnrichPG(r lsRecord, e *lsEvent) {
	if tl := lsPGTimelineOf(r.Text); tl > 0 {
		e.Message = r.Text
	}
	// Patroni names the other member in most of its interesting records, and a name is what
	// turns "somebody was promoted" into a sentence about a cluster.
	if r.Subsys == lsSubsysPatroni {
		if m := lsPatroniLeaderRe.FindStringSubmatch(r.Text); m != nil {
			e.Peer = lsMongoShortHost(m[1])
		}
	}
}

// lsPatroniLeaderRe finds the member Patroni is talking about: the leader it is following,
// the owner of the lock, or the candidate of a switchover.
var lsPatroniLeaderRe = regexp.MustCompile(
	`following a leader \(?([A-Za-z0-9_.-]+?)\)?$|Lock owner: ([A-Za-z0-9_.-]+);|leader=([A-Za-z0-9_.-]+)|from leader '([A-Za-z0-9_.-]+)'`)

// lsResolvePG carries the server's state forward between the records that change it.
//
// PostgreSQL states its state only when it changes, and the words it uses are not states —
// "ready to accept connections" is an event. The lane has to be filled in between them, and
// the distinction that matters throughout is read-write against read-only: a standby is up,
// answers queries, and fails every write, which is not the same kind of available.
func lsResolvePG(events []lsEvent) {
	// In TIME order, not file order. A Patroni member's source is two logs concatenated —
	// PostgreSQL's file and the journal appended after it — so walking the slice as it
	// arrives carries the state from the END of the first log onto the START of the second.
	// The corpus showed it immediately: Patroni records from the first seconds of a cluster
	// were labelled STANDBY, a state that member did not reach for another minute.
	order := make([]int, len(events))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return events[order[a]].TS < events[order[b]].TS })
	state := ""
	for _, i := range order {
		// Only the PostgreSQL half moves the lane. Patroni describes the state it INTENDS —
		// "starting as a secondary" is written before PostgreSQL has done anything — and
		// letting it drive the lane put members in states their server had not reached.
		if events[i].Subsys == lsSubsysPatroni {
			if events[i].State == "" {
				events[i].State = state
			}
			continue
		}
		msg := events[i].Message
		switch {
		case strings.Contains(msg, "database system is ready to accept connections"):
			state = lsStatePrimaryM
		case strings.Contains(msg, "ready to accept read-only connections"):
			state = lsStateStandby
		case strings.Contains(msg, "entering standby mode"),
			strings.Contains(msg, "consistent recovery state reached"):
			if state != lsStateStandby {
				state = lsStatePGRecover
			}
		case strings.Contains(msg, "received promote request"):
			state = lsStatePromoting
		case strings.Contains(msg, "starting PostgreSQL"),
			strings.Contains(msg, "automatic recovery in progress"),
			strings.Contains(msg, "database system was interrupted"):
			state = lsStateStarting
		case strings.Contains(msg, "database system is shut down"):
			state = lsStateDown
		}
		if events[i].State == "" {
			events[i].State = state
		}
	}
}

// lsSniffPGFlavour decides which of the three vocabularies a source speaks.
//
// Patroni first, because a Patroni member is also a streaming standby and would otherwise be
// filed as one — and the findings that may speak differ. A server with no replication
// records at all is a standalone, and telling somebody with a standalone that their cluster
// has no leader is worse than saying nothing.
func lsSniffPGFlavour(recs []lsRecord) string {
	stream := false
	for _, r := range recs {
		if r.Subsys == lsSubsysPatroni {
			return lsFlavourPatroni
		}
		switch {
		case strings.Contains(r.Text, "entering standby mode"),
			strings.Contains(r.Text, "started streaming WAL"),
			strings.Contains(r.Text, "ready to accept read-only connections"),
			strings.Contains(r.Text, "walsender"),
			strings.Contains(r.Text, "replication terminated by primary"),
			strings.Contains(r.Text, "standby connection"),
			strings.Contains(r.Text, "primary_conninfo"):
			stream = true
		}
	}
	if stream {
		return lsFlavourPGStream
	}
	return lsFlavourPostgres
}
