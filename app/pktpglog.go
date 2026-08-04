package main

// pktpglog.go — PostgreSQL's server log, read the same way pktserverlog.go reads
// MySQL's and for the same reason: the events that matter most are the ones with
// nobody to tell.
//
// PostgreSQL is better than MySQL at telling the client what went wrong — an
// ErrorResponse carries a SQLSTATE, a message, a detail and a hint, and pktpg.go
// decodes all of it. What still never reaches the wire:
//
//	"incomplete startup packet"         a TCP health check, or a port scanner. The
//	                                    connection is gone before it says anything,
//	                                    so there is no client to answer.
//	"could not receive data from client: Connection reset by peer"
//	                                    the client vanished mid-statement.
//	"terminating connection because of crash of another server process"
//	                                    one backend died, so every other connection
//	                                    is dropped. The capture shows the drops; only
//	                                    the log says why.
//	"requested WAL segment … has already been removed"
//	                                    a standby asked for WAL the primary no longer
//	                                    has, which ends replication permanently and
//	                                    needs a rebuild.
//	checkpoint and autovacuum records   the I/O behind latency the capture measures
//	                                    but cannot explain.
//
// A second log is worth reading on a Patroni node, and it is not PostgreSQL's:
// Patroni's own. A failover is a Patroni decision, taken because a lease expired or a
// member could not reach etcd, and the PostgreSQL log only shows its consequences
// ("received promote request", "database system is ready"). Patroni's lines have a
// different shape — a comma before the milliseconds, a level, no pid — so they are
// recognised separately and folded into the same classified stream.
//
// Continuation records (DETAIL, HINT, STATEMENT, CONTEXT, QUERY) are attached to the
// record above them rather than listed separately. STATEMENT in particular carries the
// SQL that failed, which is exactly what somebody reading an ERROR wants next, and as
// its own row it would be an unexplained fragment.

import (
	"regexp"
	"strings"
	"time"
)

// pktPGLogLine matches PostgreSQL's stderr format, which is log_line_prefix followed
// by a level and the message. dbcanvas provisions '%m [%p] ', and the two additions
// seen most often elsewhere are the session's user@database (%u@%d) and a log line
// number (%l, written as "[4-1]"), so both are tolerated between the pid and the
// level rather than demanded:
//
//	2026-08-04 06:15:44.142 UTC [2948] ERROR:  duplicate key value violates …
//	2026-08-04 06:15:44.142 +08 [2948] carsim@rental LOG:  statement: BEGIN
//	2026-08-04 06:15:44.142 UTC [2948] [7-1] postgres@postgres FATAL:  …
//
// The timestamp's zone is required to be present but not required to be UTC: a server
// with log_timezone set to a real zone writes an abbreviation or an offset, and the
// first version of this pattern accepting only "UTC" would have rejected every line of
// such a log — the same mistake the MySQL pattern made with Z versus +08:00.
var pktPGLogLine = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:[.,]\d+)?) ([A-Z]{2,5}|[+-]\d{2}(?::?\d{2})?)\s+` + // 1 time, 2 zone
		`\[(\d+)\]\s*` + // 3 pid
		`(?:\[[\d-]+\]\s*)?` + // optional %l line number
		`(?:([^\s:\[\]]+)\s+)?` + // 4 optional user@database or application
		`(DEBUG[1-5]?|INFO|NOTICE|WARNING|ERROR|LOG|FATAL|PANIC|DETAIL|HINT|STATEMENT|CONTEXT|QUERY):\s+(.*)$`) // 5 level, 6 message

// pktPatroniLogLine matches Patroni's own log, which is Python logging rather than
// PostgreSQL:
//
//	2026-08-04 06:03:00,510 INFO: no action. I am (patroni01), the leader with the lock
//	2026-08-04 06:03:00,510 WARNING: Loop time exceeded, rescheduling immediately.
var pktPatroniLogLine = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}[.,]\d+)\s+(DEBUG|INFO|WARNING|ERROR|CRITICAL|EXCEPTION):\s+(.*)$`)

// pgLogContinuation is the set of levels that belong to the record above them.
var pgLogContinuation = map[string]bool{
	"DETAIL": true, "HINT": true, "STATEMENT": true, "CONTEXT": true, "QUERY": true,
}

// pktPGLogFamilies classifies PostgreSQL log records. Ordered: the first match wins,
// so the specific patterns come before the general ones.
//
// The classes are pktserverlog.go's, deliberately — an aborted connection is an
// aborted connection whichever engine wrote the line, and the UI's class filter, the
// window view and the packet correlation are then shared rather than duplicated.
var pktPGLogFamilies = []pktLogFamily{
	// ---- connections that ended without a conversation.
	{substr: []string{"incomplete startup packet"}, class: pktLogAbort,
		label: "Incomplete startup packet (health check or port probe)"},
	{substr: []string{"could not receive data from client: Connection reset by peer"},
		class: pktLogAbort, label: "Client connection reset mid-statement"},
	{substr: []string{"could not receive data from client"}, class: pktLogAbort,
		label: "Could not read from client"},
	{substr: []string{"could not send data to client"}, class: pktLogAbort,
		label: "Could not write to client"},
	{substr: []string{"unexpected EOF on client connection"}, class: pktLogAbort,
		label: "Client vanished (unexpected EOF)"},
	{substr: []string{"connection to client lost"}, class: pktLogAbort, label: "Connection to client lost"},
	{substr: []string{"terminating connection because of crash of another server process"},
		class: pktLogAbort, label: "Dropped because another backend crashed"},
	{substr: []string{"terminating connection due to administrator command"},
		class: pktLogAbort, label: "Terminated by administrator command"},
	{substr: []string{"terminating connection due to idle-in-transaction timeout"},
		class: pktLogAbort, label: "Terminated: idle in transaction too long"},
	{substr: []string{"terminating connection due to idle-session timeout"},
		class: pktLogAbort, label: "Terminated: idle session timeout"},
	{substr: []string{"terminating connection due to conflict with recovery"},
		class: pktLogAbort, label: "Terminated: conflict with recovery (standby)"},

	// ---- refused at the door.
	{substr: []string{"password authentication failed"}, class: pktLogAuth,
		label: "Password authentication failed"},
	{substr: []string{"no pg_hba.conf entry"}, class: pktLogAuth,
		label: "No pg_hba.conf entry for this host/user/database"},
	{substr: []string{"pg_hba.conf rejects connection"}, class: pktLogAuth,
		label: "pg_hba.conf rejects this connection"},
	{allOf: []string{"role ", " does not exist"}, class: pktLogAuth, label: "Role does not exist"},
	{allOf: []string{"database ", " does not exist"}, class: pktLogAuth, label: "Database does not exist"},
	{substr: []string{"sorry, too many clients already"}, class: pktLogAuth,
		label: "Too many clients (max_connections reached)"},
	{substr: []string{"remaining connection slots are reserved"}, class: pktLogAuth,
		label: "Only superuser slots left"},
	{substr: []string{"is not permitted to log in"}, class: pktLogAuth, label: "Role may not log in (NOLOGIN)"},
	{substr: []string{"the database system is starting up"}, class: pktLogAuth,
		label: "Refused: still starting up"},
	{substr: []string{"the database system is in recovery mode"}, class: pktLogAuth,
		label: "Refused: in recovery"},
	{substr: []string{"the database system is shutting down"}, class: pktLogAuth,
		label: "Refused: shutting down"},

	// ---- names and addresses.
	{substr: []string{"could not translate host name"}, class: pktLogDNS,
		label: "Host name could not be resolved"},
	{substr: []string{"could not resolve"}, class: pktLogDNS, label: "Name could not be resolved"},
	{substr: []string{"reverse lookup"}, class: pktLogDNS, label: "Reverse lookup problem"},

	// ---- the listener itself.
	{substr: []string{"could not bind"}, class: pktLogListen, label: "Could not bind the listening address"},
	{substr: []string{"could not create any TCP/IP sockets"}, class: pktLogListen,
		label: "No TCP/IP sockets could be created"},
	{substr: []string{"listening on"}, class: pktLogListen, label: "Listening"},
	{substr: []string{"could not accept new connection"}, class: pktLogListen,
		label: "Could not accept a new connection"},

	// ---- TLS.
	{substr: []string{"could not accept SSL connection"}, class: pktLogTLS,
		label: "SSL connection could not be established"},
	{substr: []string{"SSL error"}, class: pktLogTLS, label: "SSL error"},
	{substr: []string{"SSL off"}, class: pktLogTLS, label: "Connection required SSL, which is off"},
	{substr: []string{"certificate"}, class: pktLogTLS, label: "Certificate problem"},

	// ---- replication and recovery. The heart of a PostgreSQL cluster capture.
	{substr: []string{"has already been removed"}, class: pktLogRepl,
		label: "Requested WAL segment already removed — the standby cannot catch up"},
	{substr: []string{"terminating walsender process due to replication timeout"},
		class: pktLogRepl, label: "Walsender timed out"},
	{substr: []string{"replication terminated by primary server"}, class: pktLogRepl,
		label: "Replication terminated by the primary"},
	{substr: []string{"could not receive data from WAL stream"}, class: pktLogRepl,
		label: "WAL stream broke"},
	{substr: []string{"started streaming WAL"}, class: pktLogRepl, label: "Started streaming WAL"},
	{substr: []string{"streaming replication successfully connected"}, class: pktLogRepl,
		label: "Replication connected"},
	{substr: []string{"walreceiver"}, class: pktLogRepl, label: "WAL receiver"},
	{substr: []string{"invalid record length", "invalid magic number", "incorrect resource manager"},
		class: pktLogRepl, label: "WAL record could not be read (end of stream or corruption)"},
	{substr: []string{"canceling statement due to conflict with recovery"}, class: pktLogRepl,
		label: "Query cancelled by recovery conflict (standby)"},
	{substr: []string{"consistent recovery state reached"}, class: pktLogRepl,
		label: "Standby reached a consistent state"},
	{substr: []string{"redo starts at", "redo done at", "restartpoint"}, class: pktLogRepl,
		label: "Recovery/restartpoint activity"},
	{substr: []string{"selected new timeline", "new target timeline"}, class: pktLogRepl,
		label: "Timeline changed (a promotion happened)"},

	// ---- lifecycle.
	{substr: []string{"database system is ready to accept connections"}, class: pktLogLifecycle,
		label: "Ready to accept connections"},
	{substr: []string{"database system is ready to accept read-only connections"},
		class: pktLogLifecycle, label: "Ready, read-only (this is a standby)"},
	{substr: []string{"received fast shutdown request", "received smart shutdown request",
		"received immediate shutdown request"}, class: pktLogLifecycle, label: "Shutdown requested"},
	{substr: []string{"shutting down", "database system is shut down"}, class: pktLogLifecycle,
		label: "Server shutdown"},
	{substr: []string{"starting PostgreSQL"}, class: pktLogLifecycle, label: "Server startup"},
	{substr: []string{"database system was interrupted", "automatic recovery in progress"},
		class: pktLogLifecycle, label: "Crash recovery"},
	{substr: []string{"received promote request", "promoting"}, class: pktLogLifecycle,
		label: "Promotion — this node is becoming the primary"},
	{substr: []string{"entering standby mode"}, class: pktLogLifecycle, label: "Entering standby mode"},

	// ---- resources and locking, which is where latency in the capture comes from.
	{substr: []string{"checkpoints are occurring too frequently"}, class: pktLogOther,
		label: "Checkpoints too frequent — max_wal_size is too small for this write rate"},
	{substr: []string{"deadlock detected"}, class: pktLogOther, label: "Deadlock detected"},
	{substr: []string{"still waiting for"}, class: pktLogOther, label: "Waiting on a lock"},
	{substr: []string{"canceling statement due to statement timeout"}, class: pktLogOther,
		label: "Statement timeout"},
	{substr: []string{"canceling statement due to lock timeout"}, class: pktLogOther, label: "Lock timeout"},
	{substr: []string{"canceling statement due to user request"}, class: pktLogOther,
		label: "Statement cancelled by the client"},
	{substr: []string{"out of memory", "could not fork", "too many open files", "No space left"},
		class: pktLogOther, label: "Resource exhaustion"},
	{substr: []string{"automatic vacuum", "automatic analyze"}, class: pktLogOther, label: "Autovacuum"},
	{substr: []string{"checkpoint starting", "checkpoint complete"}, class: pktLogOther, label: "Checkpoint"},
	{substr: []string{"duplicate key value", "violates foreign key", "violates not-null",
		"violates check constraint"}, class: pktLogOther, label: "Constraint violation"},
}

// pktPatroniLogFamilies classifies Patroni's own records. These are the cluster class
// throughout: every one of them is Patroni deciding something about who leads.
var pktPatroniLogFamilies = []pktLogFamily{
	{substr: []string{"the leader with the lock"}, class: pktLogCluster, label: "Patroni: leader, holding the lock"},
	{substr: []string{"promoted self to leader"}, class: pktLogCluster, label: "Patroni: promoted itself to leader"},
	{substr: []string{"promote"}, class: pktLogCluster, label: "Patroni: promotion"},
	{substr: []string{"demoting self", "demoted self"}, class: pktLogCluster, label: "Patroni: demoted itself"},
	{substr: []string{"following a different leader", "following new leader", "Local timeline"},
		class: pktLogCluster, label: "Patroni: following a new leader"},
	{substr: []string{"does not have lock", "not healthy enough for leader race"},
		class: pktLogCluster, label: "Patroni: not the leader"},
	{substr: []string{"failed to update leader lock", "Failed to update leader lock"},
		class: pktLogCluster, label: "Patroni: FAILED to renew the leader lock — a failover follows"},
	{substr: []string{"Error communicating with DCS", "DCS is not accessible",
		"etcd", "Etcd", "Connection refused"}, class: pktLogCluster,
		label: "Patroni: cannot reach the DCS (etcd)"},
	{substr: []string{"switchover", "Switchover"}, class: pktLogCluster, label: "Patroni: switchover"},
	{substr: []string{"failover", "Failover"}, class: pktLogCluster, label: "Patroni: failover"},
	{substr: []string{"pg_rewind", "rewind"}, class: pktLogCluster, label: "Patroni: pg_rewind"},
	{substr: []string{"bootstrap", "Bootstrap", "initialize"}, class: pktLogCluster, label: "Patroni: bootstrap"},
	{substr: []string{"Loop time exceeded"}, class: pktLogCluster,
		label: "Patroni: loop time exceeded — the member is too slow to renew its lease reliably"},
	{substr: []string{"no action"}, class: pktLogCluster, label: "Patroni: no action needed"},
}

// pktClassifyPGLogLine turns one PostgreSQL or Patroni log line into an entry.
// continuation is true for a DETAIL/HINT/STATEMENT/CONTEXT/QUERY record, which the
// caller folds into the record above it.
func pktClassifyPGLogLine(line string) (e pktLogEntry, continuation, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if m := pktPGLogLine.FindStringSubmatch(line); m != nil {
		e = pktLogEntry{
			Time: m[1] + " " + m[2], Level: m[5], Subsys: m[4],
			Message: strings.TrimSpace(m[6]), Class: pktLogOther,
		}
		e.TS = pgLogTime(m[1], m[2])
		if pgLogContinuation[m[5]] {
			return e, true, true
		}
		pgApplyFamilies(&e, pktPGLogFamilies)
		// A FATAL that matched nothing is still a refused or killed connection, and
		// saying so beats leaving it as "other".
		if e.Label == "" {
			switch e.Level {
			case "FATAL", "PANIC":
				e.Class, e.Label = pktLogAbort, "Connection ended by the server ("+e.Level+")"
			case "ERROR":
				e.Label = "Statement error"
			default:
				e.Label = e.Level
			}
		}
		return e, false, true
	}
	if m := pktPatroniLogLine.FindStringSubmatch(line); m != nil {
		e = pktLogEntry{
			Time: m[1], Level: m[2], Subsys: "Patroni",
			Message: strings.TrimSpace(m[3]), Class: pktLogCluster,
		}
		e.TS = pgLogTime(m[1], "")
		pgApplyFamilies(&e, pktPatroniLogFamilies)
		if e.Label == "" {
			e.Label = "Patroni: " + strings.ToLower(m[2])
		}
		return e, false, true
	}
	return pktLogEntry{}, false, false
}

// pgApplyFamilies applies the first matching family: any one of substr matches, and
// every one of allOf must. The two-list split is what lets a family list alternatives
// ("received fast/smart/immediate shutdown request") while another still requires two
// fragments together ("role" AND "does not exist").
func pgApplyFamilies(e *pktLogEntry, families []pktLogFamily) {
	for _, f := range families {
		if len(f.allOf) > 0 {
			all := true
			for _, sub := range f.allOf {
				if !strings.Contains(e.Message, sub) {
					all = false
					break
				}
			}
			if !all {
				continue
			}
			e.Class, e.Label = f.class, f.label
			return
		}
		for _, sub := range f.substr {
			if strings.Contains(e.Message, sub) {
				e.Class, e.Label = f.class, f.label
				return
			}
		}
	}
}

// pgLogTime parses PostgreSQL's log timestamp. The zone is an abbreviation (UTC, CET)
// or an offset (+08, +0800), and only the offset can be resolved without a zone
// database lookup — an abbreviation is ambiguous worldwide (CST is three different
// zones), so anything that is not UTC or an offset is read as UTC and the log's own
// text is kept for display. Getting this wrong shifts every record relative to the
// capture, which is worse than showing the raw string, so the parsed value is only
// used where it can be trusted.
func pgLogTime(stamp, zone string) float64 {
	stamp = strings.Replace(stamp, ",", ".", 1)
	layout := "2006-01-02 15:04:05.999999"
	switch {
	case zone == "" || zone == "UTC" || zone == "GMT" || zone == "Z":
		if t, err := time.Parse(layout, stamp); err == nil {
			return float64(t.UnixNano()) / 1e9
		}
	case strings.HasPrefix(zone, "+"), strings.HasPrefix(zone, "-"):
		// Normalise +08 and +0800 to +08:00, which time.Parse understands.
		off := zone
		switch len(off) {
		case 3:
			off += ":00"
		case 5:
			off = off[:3] + ":" + off[3:]
		}
		if t, err := time.Parse(layout+"-07:00", stamp+off); err == nil {
			return float64(t.UnixNano()) / 1e9
		}
	default:
		// A named abbreviation: read the wall clock as UTC rather than guessing.
		if t, err := time.Parse(layout, stamp); err == nil {
			return float64(t.UnixNano()) / 1e9
		}
	}
	return 0
}

// pktPGLogTailScript finds the newest PostgreSQL log file and tails it, together with
// Patroni's log when there is one. PostgreSQL rotates by day of week
// (postgresql-Tue.log), so the path is a glob and the newest match is the live one —
// a fixed list of paths, which is enough for MySQL, would pick last Tuesday's file.
const pktPGLogTailScript = `set -e
found=""
for pat in $PATHS; do
  f=$(ls -1t $pat 2>/dev/null | head -n 1 || true)
  if [ -n "$f" ] && [ -r "$f" ]; then found="$f"; break; fi
done
if [ -z "$found" ]; then
  echo "no readable PostgreSQL log found (tried: $PATHS)" >&2
  exit 1
fi
echo "PATH=$found"
tail -n "$LINES" "$found"
# Patroni's own log, when this node has one: a failover is its decision, and
# PostgreSQL's log only records the consequences.
for pl in $PATRONI_PATHS; do
  p=$(ls -1t $pl 2>/dev/null | head -n 1 || true)
  if [ -n "$p" ] && [ -r "$p" ]; then echo "ALSO=$p"; tail -n "$LINES" "$p"; break; fi
done
exit 0`

// pktPGLogPaths is where a PostgreSQL log lives: what dbcanvas provisions first, then
// the distribution defaults for a node provisioned elsewhere.
var pktPGLogPaths = []string{
	"/var/lib/pgsql/*/data/log/postgresql-*.log", // Percona/RHEL, what pg.go and patroni.go configure
	"/var/lib/pgsql/*/data/log/*.log",
	"/var/log/postgresql/postgresql-*.log", // Debian/Ubuntu
	"/var/lib/postgresql/*/main/log/*.log",
	"/var/log/postgresql/*.log",
	"/pgdata/log/*.log", // container images that keep the cluster under /pgdata
}

// pktPatroniLogPaths is where Patroni writes, when it writes to a file at all. A
// Patroni that logs to the journal is not read here: the journal is not a file, and
// reading it needs a different command on every init system.
var pktPatroniLogPaths = []string{
	"/var/log/patroni/*.log",
	"/var/log/patroni.log",
	"/var/lib/pgsql/patroni.log",
}
