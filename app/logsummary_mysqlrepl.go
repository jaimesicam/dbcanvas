package main

// logsummary_mysqlrepl.go — what a MySQL asynchronous-replication log actually says.
//
// The Galera catalogue next door reads a *synchronous* cluster, where the interesting
// records are about membership and quorum. Asynchronous replication has none of that. Its
// vocabulary is a different one entirely — channels, the I/O and SQL threads, GTID sets,
// binary log coordinates — and its failure modes are different in kind: a Galera member
// that falls behind is held back by flow control, while a replica that falls behind is
// simply allowed to, silently, for as long as you let it.
//
// Every rule here was written against logs from a live three-node Percona Server 8.0.46-37
// GTID topology (one source, two replicas), captured while doing the thing that produces
// them. The fixtures are the `r0*` directories under app/testdata/logsummary/.
//
// Four things that capture taught:
//
//  1. The level field IS usable here, unlike Galera. Replication failures come through as
//     real [ERROR] records with MY- codes, and the good news comes through as [System].
//     What the level cannot tell you is which of them stopped replication and which is a
//     transient the server will retry out of — so severity still comes from meaning.
//
//  2. A replica can stop and keep looking healthy. `Replica_IO_Running: Connecting` is not
//     a state anything logs repeatedly: the server writes one error and then retries every
//     60 seconds for 86400 attempts — sixty days — saying nothing more.
//
//  3. Lag is invisible. A replica held 61 seconds behind its source wrote nothing about it
//     at all. See lsFindingReplicaLag.
//
//  4. A network-caused outage can leave NO trace but the recovery. Cutting a replica off
//     its source with 100% packet loss produced no "lost connection" and no "error
//     reconnecting" — the I/O thread simply blocked until slave_net_timeout, then
//     reconnected, and the only record of an 80-second outage was the line saying it had
//     connected. lsFindingSilentReconnect is built on exactly that absence.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Percona/MySQL replication error codes worth knowing by number rather than by text: the
// text changes between releases and the number does not.
const (
	lsCodeReplIOError    = "MY-010584" // Replica I/O or SQL thread reported a failure
	lsCodeReplPacket     = "MY-010557" // Error reading packet from server
	lsCodeReplFatal      = "MY-013114" // Got fatal error N from source
	lsCodeReplConnected  = "MY-014002" // receiver thread connected, replication starting
	lsCodeReplResumed    = "MY-010592" // connected and resumed at a coordinate
	lsCodeReplMetaInsec  = "MY-010897" // credentials stored in the metadata repository
	lsCodeReplRelayName  = "MY-010604" // neither --relay-log nor --relay-log-index
	lsCodeCrashRelog     = "MY-013951" // the crash block, re-emitted through the error log
	lsCodeXARecoveryOpen = "MY-010229" // Starting XA crash recovery
	lsCodeXARecoveryDone = "MY-010232" // XA crash recovery finished
)

var (
	// The channel name, which is "" for the default channel and a real name for a
	// multi-source or cross-cluster setup. Worth surfacing: on a server with several
	// channels, "replication is broken" is only ever true of one of them.
	lsReplChannel = regexp.MustCompile(`for channel '([^']*)'`)
	lsReplErrno   = regexp.MustCompile(`(?:Error_code: MY-0*|server_errno=)(\d+)`)
	lsReplAttempt = regexp.MustCompile(`attempt (\d+)/(\d+), with a delay of (\d+) seconds`)
	lsReplGTIDGap = regexp.MustCompile(`missing transactions are '([^']*)'`)
	lsReplTable   = regexp.MustCompile(`event on table (\S+?);`)
	lsReplDupKey  = regexp.MustCompile(`Duplicate entry '([^']*)' for key '([^']*)'`)
	lsReplTrx     = regexp.MustCompile(`executing transaction '([^']*)'`)
	lsReplSource  = regexp.MustCompile(`to source '([^']*)'`)
	lsReplCoord   = regexp.MustCompile(`in log '([^']*)' at position (\d+)`)
)

// lsReplRules are matched BEFORE the Galera rules, because they are the specific ones: a
// record carrying MY-010584 is a replication failure whatever else its text resembles.
var lsReplRules = []lsRule{
	// ---- replication stopped -----------------------------------------------------
	{codes: []string{lsCodeReplFatal}, substr: []string{"Got fatal error"},
		class: lsClassReplica, sev: lsSevBad, label: "Replication stopped by a fatal error",
		means:  "The I/O thread hit an error it will not retry past. Replication from this source is stopped until somebody intervenes.",
		enrich: lsEnrichRepl},
	{substr: []string{"purged required binary logs"},
		class: lsClassReplica, sev: lsSevBad, label: "The source purged binary logs this replica still needed",
		means:  "The transactions this replica had not read yet no longer exist on the source. Replication cannot resume by itself: the replica has to be rebuilt from a backup, or the missing transactions replayed from somewhere that still has them. binlog_expire_logs_seconds on the source is the setting that decides how long a replica may be away.",
		enrich: lsEnrichRepl},
	{substr: []string{"Could not execute", "handler error"},
		class: lsClassConflict, sev: lsSevBad, label: "Replication stopped applying an event",
		means:  "The SQL thread could not apply a change from the source, so it stopped. This is data drift: the row the source is describing is not what this replica holds. Skipping the event hides the difference rather than fixing it.",
		enrich: lsEnrichRepl},
	{substr: []string{"The replica coordinator and worker threads are stopped"},
		class: lsClassReplica, sev: lsSevBad, label: "Replica applier threads stopped",
		means:  "The parallel applier stopped mid-transaction. A restart normally restores consistency by itself; non-transactional tables and DDL are the cases where it does not.",
		enrich: lsEnrichRepl},
	{substr: []string{"Error connecting to source"},
		class: lsClassReplica, sev: lsSevBad, label: "Replica cannot connect to its source",
		means:  "The I/O thread could not establish a connection at all. Note the retry policy in the message — the server will keep trying quietly, so this one record may be the only warning you get.",
		enrich: lsEnrichRepl},
	{substr: []string{"Error reconnecting to source"},
		class: lsClassReplica, sev: lsSevBad, label: "Replica lost its source",
		means:  "The connection to the source dropped and could not be re-established. Between this and the record that says it reconnected, this replica received nothing.",
		enrich: lsEnrichRepl},
	{codes: []string{lsCodeReplPacket}, substr: []string{"Error reading packet from server"},
		class: lsClassReplica, sev: lsSevBad, label: "Replication stream broke",
		means:  "The replica was reading the binary log stream and it ended unexpectedly — the source went away, was restarted, or the network dropped the connection.",
		enrich: lsEnrichRepl},
	{codes: []string{lsCodeReplIOError},
		class: lsClassReplica, sev: lsSevBad, label: "Replication error",
		means:  "The replica's I/O or SQL thread reported a failure.",
		enrich: lsEnrichRepl},

	// ---- the source's side of it --------------------------------------------------
	{substr: []string{"Cannot replicate to server with server_uuid"},
		class: lsClassReplica, sev: lsSevBad, label: "This server purged logs a replica needed",
		means:  "Read on the SOURCE: a replica asked for transactions that have already been purged here, and was refused. It names the replica that can no longer catch up.",
		enrich: lsEnrichRepl},

	// ---- replication healthy ------------------------------------------------------
	{codes: []string{lsCodeReplConnected}, substr: []string{"Starting GTID-based replication"},
		class: lsClassReplica, sev: lsSevOK, label: "Replica connected to its source",
		means:  "The receiver thread is connected and replication is running. On a log with no matching disconnect above it, this line is the only evidence that there WAS an outage — see the verdict.",
		enrich: lsEnrichRepl},
	{codes: []string{lsCodeReplResumed}, substr: []string{"replication resumed in log"},
		class: lsClassReplica, sev: lsSevOK, label: "Replication resumed",
		means:  "The replica reconnected and picked up from the coordinate given.",
		enrich: lsEnrichRepl},

	// ---- replication configuration ------------------------------------------------
	// Somebody repointed replication. Worth a warning of its own: it is the one record
	// that explains a replica suddenly following a different server, and it carries both
	// the previous and the new coordinates.
	{codes: []string{"MY-010597"}, substr: []string{"CHANGE REPLICATION SOURCE TO"},
		class: lsClassReplica, sev: lsSevWarn, label: "Replication was repointed",
		means: "CHANGE REPLICATION SOURCE TO ran on this server. The record carries the previous source and coordinates as well as the new ones — which is the only place that history exists.",
		enrich: func(r lsRecord, e *lsEvent) {
			if m := regexp.MustCompile(`New state source_host='([^']*)'`).FindStringSubmatch(r.Text); m != nil {
				e.Message = "now following " + m[1]
				e.Peer = m[1]
			}
		}},
	{codes: []string{lsCodeReplRelayName},
		class: lsClassConfig, sev: lsSevWarn, label: "Relay logs are named after the hostname",
		means: "Neither --relay-log nor --relay-log-index is set, so the relay logs carry this machine's hostname. Rename the host and replication breaks on the next restart."},
	{codes: []string{lsCodeReplMetaInsec},
		class: lsClassSecurity, sev: lsSevInfo, label: "Replication credentials stored on disk",
		means: "The replication user's password is kept in the connection metadata repository. Expected on a server set up with CHANGE REPLICATION SOURCE; noted because it is written every time replication starts."},

	// ---- crash recovery -----------------------------------------------------------
	{codes: []string{lsCodeXARecoveryOpen}, substr: []string{"Starting XA crash recovery"},
		class: lsClassStartup, sev: lsSevWarn, label: "Crash recovery running",
		means: "The server is recovering prepared transactions on start-up, which only happens after an unclean stop."},
	{codes: []string{lsCodeXARecoveryDone}, substr: []string{"XA crash recovery finished"},
		class: lsClassStartup, sev: lsSevOK, label: "Crash recovery finished",
		means: "Recovery completed and the server is carrying on."},

	// ---- the crash block, re-emitted ----------------------------------------------
	//
	// Percona Server writes the crash handler's block TWICE: once raw (which
	// lsCrashHeader catches) and once again line by line through the normal error log as
	// MY-013951. Only the line that names the signal is promoted; the rest is the same
	// backtrace a second time and is folded into it by lsFoldMySQL.
	// Matched on the TEXT, not the code: a rule that also listed MY-013951 would match
	// every line of the re-emitted block, because a code match wins on its own — and the
	// bug-report URL would be promoted to a crash of its own.
	{substr: []string{"got signal"},
		class: lsClassCrash, sev: lsSevBad, label: "Server crashed",
		means:  "The crash handler's report, re-emitted through the error log.",
		enrich: lsEnrichCrash},
	{codes: []string{lsCodeCrashRelog},
		class: lsClassCrash, sev: lsSevInfo, label: "Crash report",
		means: "A line of the crash handler's report, re-emitted through the error log. The crash itself is the record that names the signal."},
}

// lsEnrichRepl pulls the facts out of a replication record: which channel, which source,
// the error number, the retry policy, and — for the failures that name them — the
// transaction, the table and the GTIDs that are missing.
func lsEnrichRepl(r lsRecord, e *lsEvent) {
	text := r.Text
	var bits []string

	if m := lsReplChannel.FindStringSubmatch(text); m != nil {
		if m[1] == "" {
			e.Peer = "default channel"
		} else {
			e.Peer = "channel " + m[1]
			bits = append(bits, "channel "+m[1])
		}
	}
	if m := lsReplSource.FindStringSubmatch(text); m != nil {
		bits = append(bits, "source "+m[1])
	}
	if m := lsReplErrno.FindStringSubmatch(text); m != nil {
		bits = append(bits, "error "+m[1])
	}
	if m := lsReplDupKey.FindStringSubmatch(text); m != nil {
		bits = append(bits, fmt.Sprintf("duplicate %s on %s", m[1], m[2]))
	}
	if m := lsReplTable.FindStringSubmatch(text); m != nil {
		bits = append(bits, "table "+m[1])
	}
	if m := lsReplTrx.FindStringSubmatch(text); m != nil {
		bits = append(bits, "transaction "+m[1])
	}
	if m := lsReplCoord.FindStringSubmatch(text); m != nil {
		bits = append(bits, "at "+m[1]+":"+m[2])
	}
	// The retry policy is the difference between "it will fix itself" and "it will sit
	// there silently for two months". 86400 attempts a minute apart is the default.
	if m := lsReplAttempt.FindStringSubmatch(text); m != nil {
		total, _ := strconv.Atoi(m[2])
		delay, _ := strconv.Atoi(m[3])
		if total > 0 && delay > 0 {
			bits = append(bits, fmt.Sprintf("retrying every %ds for up to %s",
				delay, lsDur(float64(total*delay))))
			e.Meaning += fmt.Sprintf(" It will retry %s times at %d-second intervals — %s — and write nothing further while it does.",
				m[2], delay, lsDur(float64(total*delay)))
		}
	}
	if m := lsReplGTIDGap.FindStringSubmatch(text); m != nil && m[1] != "" {
		bits = append(bits, "missing "+truncate(m[1], 80))
	}
	if len(bits) > 0 {
		e.Message = strings.Join(bits, " · ")
	}
}

// ---------------------------------------------------------------- findings

// lsFindingReplicationBroken — replication that stopped and did not come back.
//
// "Did not come back" is the part that needs the whole timeline: a replica that lost its
// source and reconnected ten seconds later is a blip, and the same records with no
// reconnect after them are an outage still running when the log ends.
func lsFindingReplicationBroken(b *lsBundle) []lsFinding {
	broken := lsPick(b, func(e lsEvent) bool {
		return (e.Class == lsClassReplica || e.Class == lsClassConflict) && e.Sev == lsSevBad
	})
	if len(broken) == 0 {
		return nil
	}
	// Per source, the last failure and whether anything said it recovered afterwards.
	type row struct {
		src         int
		first, last lsEvent
		recovered   float64
	}
	var rows []row
	for _, src := range lsSrcSet(broken) {
		// first names the cause; last is where recovery is measured from, because a
		// failure is usually followed by a more generic record about the same thing.
		var first, last lsEvent
		for _, e := range broken {
			if e.Src != src {
				continue
			}
			if first.No == 0 {
				first = e
			}
			last = e
		}
		rec := 0.0
		for _, e := range b.Events {
			if e.Src == src && e.TS > last.TS && e.Sev == lsSevOK && e.Class == lsClassReplica {
				rec = e.TS
				break
			}
		}
		rows = append(rows, row{src, first, last, rec})
	}
	var still, healed []string
	for _, r := range rows {
		if r.recovered > 0 {
			healed = append(healed, fmt.Sprintf("%s (%s, back after %s)",
				lsNode(b, r.src), r.first.Label, lsDur(r.recovered-r.last.TS)))
			continue
		}
		still = append(still, fmt.Sprintf("%s (%s)", lsNode(b, r.src), r.first.Label))
	}
	f := lsFinding{
		ID: "replication-broken", Sev: lsSevWarn,
		Title: "Replication stopped", At: broken[0].TS,
		Sources: lsSrcSet(broken), Events: lsEventNos(broken, 8),
	}
	var parts []string
	if len(still) > 0 {
		f.Sev = lsSevBad
		f.Title = "Replication is still broken at the end of the log"
		parts = append(parts, "not recovered: "+strings.Join(still, ", "))
		f.Advice = "A stopped replica keeps answering reads with data that gets staler by the second, and nothing in the log will mention it again. Whatever health check fronts these replicas has to read Replica_IO_Running and Replica_SQL_Running, not the port."
	}
	if len(healed) > 0 {
		parts = append(parts, "recovered: "+strings.Join(healed, ", "))
		if f.Advice == "" {
			f.Advice = "Replication came back on its own. The window between the failure and the recovery is time this replica was serving stale data."
		}
	}
	f.Detail = strings.Join(parts, " · ") + "."
	return []lsFinding{f}
}

// lsFindingSilentReconnect — an outage whose only trace is the recovery.
//
// Measured, not guessed. Cutting a replica off its source with 100% packet loss on 3306
// produced NO "lost connection" and NO "error reconnecting": the I/O thread blocked until
// slave_net_timeout, then reconnected, and the sole record of an eighty-second outage was
// the line saying it had connected. So a "connected to source" with no failure in front of
// it is not noise — it is the shape a network-caused replication outage leaves behind.
func lsFindingSilentReconnect(b *lsBundle) []lsFinding {
	var silent []lsEvent
	for _, e := range b.Events {
		if e.Label != "Replica connected to its source" {
			continue
		}
		// Was there a failure on this source in the minutes before? slave_net_timeout is
		// 60 s by default, so the reconnect can trail the cut by more than a minute.
		explained := false
		for _, p := range b.Events {
			if p.Src != e.Src || p.TS > e.TS || e.TS-p.TS > 300 {
				continue
			}
			if p.Sev == lsSevBad && (p.Class == lsClassReplica || p.Class == lsClassCrash) {
				explained = true
			}
			// A start-up explains it too: replication always connects after a restart.
			if p.Class == lsClassStartup {
				explained = true
			}
		}
		if !explained {
			silent = append(silent, e)
		}
	}
	if len(silent) == 0 {
		return nil
	}
	who := []string{}
	for _, s := range lsSrcSet(silent) {
		who = append(who, lsNode(b, s))
	}
	return []lsFinding{{
		ID: "silent-reconnect", Sev: lsSevWarn,
		Title:  "A replica reconnected without anything saying it had disconnected",
		Detail: fmt.Sprintf("%s logged that it connected to its source, with no failure and no restart before it to explain why it had to.", strings.Join(who, ", ")),
		Advice: "Two things look like this and the log cannot tell them apart. A network-caused outage: the I/O thread blocks until slave_net_timeout (60 s by default) rather than erroring, so the disconnect is never written and only the recovery is — a measured 80-second outage left exactly this and nothing else. Or somebody ran START REPLICA, which is also logged only on the way up. Check Replica_IO_Running's history, or shorten slave_net_timeout if you want the disconnects reported.",
		At:     silent[0].TS, Sources: lsSrcSet(silent), Events: lsEventNos(silent, 4),
	}}
}

// lsFindingReplicaLag — the honest note, the async twin of lsFindingFlowControl.
//
// Verified: a replica held 61 seconds behind its source by a table lock, while the source
// took 400-row inserts continuously, wrote NOTHING about the lag. Not a warning, not a
// note. A page that stayed quiet here would be read as "no lag", which the file does not
// support.
func lsFindingReplicaLag(b *lsBundle) []lsFinding {
	repl := false
	for _, e := range b.Events {
		if e.Class == lsClassReplica {
			repl = true
		}
	}
	if !repl {
		return nil
	}
	return []lsFinding{{
		ID: "replica-lag", Sev: lsSevInfo,
		Title:  "Replication lag is not recorded in this log",
		Detail: "MySQL writes nothing at all when a replica falls behind. A replica held 61 seconds behind its source, while the source wrote continuously, produced not one record about it. Absence of lag records here is not evidence that there was no lag.",
		Advice: "Lag lives in the server: Seconds_Behind_Source from SHOW REPLICA STATUS, and more honestly performance_schema.replication_applier_status_by_worker together with replication_connection_status, which separate 'not received yet' from 'received but not applied'. Or the same series in PMM.",
	}}
}
