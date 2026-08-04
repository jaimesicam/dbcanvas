package main

// pktvalkeylog.go — Valkey's server log.
//
// Two things make this the odd one of the four. First the format, which is Valkey's own
// and predates anyone caring about machine parsing:
//
//	253:M 04 Aug 2026 12:16:19.361 * Cluster state changed: ok
//	253:S 04 Aug 2026 12:20:01.004 # MASTER aborted replication with an error: …
//	  ^  ^ ^                       ^ ^
//	  |  | |                       | the message
//	  |  | |                       level: . debug  - verbose  * notice  # warning
//	  |  | the timestamp, with the day first and the month as a name
//	  |  the role: M primary, S replica, C the RDB/AOF child, X sentinel
//	  the pid
//
// The **role letter** is worth having: it says what this process thought it was when it
// wrote the line, so a log that changes from M to S mid-file is a demotion, and that is
// often the whole story of an incident.
//
// Second, where it lives. dbcanvas configures no `logfile`, so Valkey writes to stdout and
// systemd captures it — the log is in the **journal**, not in a file. That is a first
// among these engines, and it means the tail command is `journalctl` with its own prefix
// to strip. A node that does set `logfile` is read from the file instead.

import (
	"regexp"
	"strings"
	"time"
)

// pktValkeyLogLine matches Valkey's format, with or without journald's prefix in front of
// it (`Aug 04 12:16:19 valkey01 valkey-server[253]: `), because the journal is where these
// lines are on a dbcanvas node.
var pktValkeyLogLine = regexp.MustCompile(
	`^(?:\w{3} +\d+ [\d:]+ \S+ \S+?(?:\[\d+\])?: )?` + // optional journald prefix
		`(\d+):([MSCX]) ` + // 1 pid, 2 role
		`(\d{1,2} \w{3} \d{4} \d{2}:\d{2}:\d{2}\.\d{3}) ` + // 3 timestamp
		`([.\-*#]) ` + // 4 level
		`(.*)$`) // 5 message

// valkeyLogRole spells out the role letter.
func valkeyLogRole(r string) string {
	switch r {
	case "M":
		return "primary"
	case "S":
		return "replica"
	case "C":
		return "RDB/AOF child"
	case "X":
		return "sentinel"
	}
	return r
}

// valkeyLogLevel spells out the level character.
func valkeyLogLevel(l string) string {
	switch l {
	case ".":
		return "DEBUG"
	case "-":
		return "VERBOSE"
	case "*":
		return "NOTICE"
	case "#":
		return "WARNING"
	}
	return l
}

// pktValkeyLogFamilies classifies Valkey's records into the shared classes, so the class
// filter, the window view and the packet correlation are the same code as the other three.
var pktValkeyLogFamilies = []pktLogFamily{
	// ---- replication: the family a capture is most often read against.
	{substr: []string{"Full resync from master", "Full resync from primary"}, class: pktLogRepl,
		label: "Full resync — the whole dataset is being transferred"},
	{substr: []string{"Starting BGSAVE for SYNC"}, class: pktLogRepl,
		label: "Fork for a full resync — the primary is serialising its dataset for a replica"},
	{substr: []string{"Partial resynchronization accepted"}, class: pktLogRepl,
		label: "Partial resync accepted — only the missing stream is sent (the cheap path)"},
	{substr: []string{"Unable to partial resync", "Partial resynchronization not accepted"},
		class: pktLogRepl, label: "Partial resync refused — a full transfer follows, usually because repl-backlog-size was too small"},
	{substr: []string{"MASTER <-> REPLICA sync: Finished with success", "Successful partial resynchronization"},
		class: pktLogRepl, label: "Replica finished syncing"},
	{substr: []string{"MASTER <-> REPLICA sync started", "MASTER <-> REPLICA sync"},
		class: pktLogRepl, label: "Replica sync in progress"},
	{substr: []string{"MASTER aborted replication", "Connection with master lost",
		"Connection with primary lost", "Connection with replica"}, class: pktLogRepl,
		label: "Replication link broken"},
	{substr: []string{"Connecting to MASTER", "Connecting to PRIMARY", "REPLICAOF", "SLAVEOF"},
		class: pktLogRepl, label: "Replication target changed"},
	{substr: []string{"Setting secondary replication ID"}, class: pktLogRepl,
		label: "Replication ID changed — this node was promoted or reparented"},
	{substr: []string{"backlog"}, class: pktLogRepl, label: "Replication backlog"},

	// ---- cluster.
	{substr: []string{"Cluster state changed: fail"}, class: pktLogCluster,
		label: "Cluster state FAIL — slots are uncovered and clients are being refused"},
	{substr: []string{"Cluster state changed: ok"}, class: pktLogCluster, label: "Cluster state OK"},
	{substr: []string{"FAIL message received"}, class: pktLogCluster,
		label: "A node was declared failed by the cluster"},
	{substr: []string{"Marking node", "as failing"}, class: pktLogCluster,
		label: "Marking a node as failing"},
	{substr: []string{"Failover auth granted", "Failover auth denied", "Failover auth request"},
		class: pktLogCluster, label: "Cluster election vote"},
	{substr: []string{"Manual failover", "manual failover"}, class: pktLogCluster, label: "Manual failover"},
	{substr: []string{"Start of election", "Currently unable to failover"}, class: pktLogCluster,
		label: "Cluster election"},
	{substr: []string{"configEpoch set", "config epoch"}, class: pktLogCluster,
		label: "Cluster configuration epoch changed"},
	{substr: []string{"Mismatch in topology"}, class: pktLogCluster,
		label: "Topology mismatch between nodes — normal while a cluster is forming, a problem afterwards"},
	{substr: []string{"Slot ", "migrat"}, class: pktLogCluster, label: "Slot migration"},

	// ---- persistence, and the two failures that stop writes.
	{substr: []string{"Can't save in background", "Background saving error",
		"Background AOF rewrite terminated with error", "Write error saving DB on disk"},
		class: pktLogOther,
		label: "Background save FAILED — writes will be refused with MISCONF until it succeeds"},
	{substr: []string{"Background saving started", "Background saving terminated with success",
		"DB saved on disk", "Background AOF rewrite finished successfully"},
		class: pktLogOther, label: "Snapshot / AOF rewrite"},
	{substr: []string{"MISCONF"}, class: pktLogOther,
		label: "MISCONF — writes are being refused because persistence is failing"},

	// ---- memory.
	{substr: []string{"Out of memory", "OOM", "used_memory"}, class: pktLogOther,
		label: "Memory pressure"},
	{substr: []string{"evicted", "eviction"}, class: pktLogOther, label: "Eviction"},

	// ---- connections.
	{substr: []string{"max number of clients reached"}, class: pktLogListen,
		label: "maxclients reached — new connections are being refused"},
	{substr: []string{"Protocol error"}, class: pktLogAbort,
		label: "Protocol error — the client sent something that is not RESP and was disconnected"},
	{substr: []string{"Client closed connection", "Closing client"}, class: pktLogAbort,
		label: "Client connection closed"},
	{substr: []string{"Accepted"}, class: pktLogOther, label: "Connection accepted"},
	{substr: []string{"Error accepting", "accept: "}, class: pktLogListen, label: "Could not accept a connection"},

	// ---- auth and TLS.
	{substr: []string{"Wrong password", "unauthenticated", "AUTH", "ACL"}, class: pktLogAuth,
		label: "Authentication / ACL"},
	{substr: []string{"TLS", "SSL", "certificate"}, class: pktLogTLS, label: "TLS / certificate"},

	// ---- lifecycle.
	{substr: []string{"Ready to accept connections"}, class: pktLogLifecycle,
		label: "Ready to accept connections"},
	{substr: []string{"oO0OoO0OoO0Oo", "Valkey version", "Redis version", "starting", "Configuration loaded"},
		class: pktLogLifecycle, label: "Server startup"},
	{substr: []string{"User requested shutdown", "Removing the pid file", "ready to exit",
		"Received SIGTERM"}, class: pktLogLifecycle, label: "Server shutdown"},
	{substr: []string{"DB loaded from", "Loading RDB", "Loading AOF", "Done loading"},
		class: pktLogLifecycle, label: "Dataset loading — the node refuses commands with LOADING until this finishes"},

	// ---- the warnings Valkey emits about its host, which are the ones people ignore
	// until they matter.
	{substr: []string{"overcommit_memory", "Background save may fail"}, class: pktLogOther,
		label: "Host setting warning: overcommit_memory — a fork for BGSAVE may fail under memory pressure"},
	{substr: []string{"TCP backlog"}, class: pktLogListen,
		label: "Host setting warning: somaxconn lower than Valkey's backlog"},
	{substr: []string{"Transparent Huge Pages", "THP"}, class: pktLogOther,
		label: "Host setting warning: transparent huge pages cause latency spikes"},
}

// pktClassifyValkeyLogLine turns one line into an entry.
func pktClassifyValkeyLogLine(line string) (pktLogEntry, bool) {
	m := pktValkeyLogLine.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
	if m == nil {
		return pktLogEntry{}, false
	}
	e := pktLogEntry{
		Time:    m[3],
		Level:   valkeyLogLevel(m[4]),
		Code:    m[1], // the pid is the only stable identifier Valkey's log carries
		Subsys:  valkeyLogRole(m[2]),
		Message: pktPrintable(m[5]),
		Class:   pktLogOther,
	}
	// "04 Aug 2026 12:16:19.361" — day first, month by name, no zone at all. Valkey logs
	// in the server's local time and says nothing about which that is, so it is read as
	// UTC and the log's own text is kept for display. On a dbcanvas node the container
	// runs UTC, which makes it correct there and honest everywhere else.
	if t, err := time.Parse("2 Jan 2006 15:04:05.000", m[3]); err == nil {
		e.TS = float64(t.UnixNano()) / 1e9
	}
	pgApplyFamilies(&e, pktValkeyLogFamilies)
	if e.Label == "" {
		e.Label = valkeyLogLevel(m[4])
	}
	// A warning that matched nothing is still a warning worth surfacing.
	if m[4] == "#" && e.Class == pktLogOther && e.Label == "WARNING" {
		e.Label = "Warning: " + pktEllipsis(e.Message, 60)
	}
	return e, true
}

// pktValkeyLogTailScript reads Valkey's log wherever it is: a file when `logfile` is set,
// and otherwise the journal, which is where dbcanvas's nodes put it. This is the only one
// of the four engines whose log is not a file by default.
const pktValkeyLogTailScript = `set -e
found=""
for pat in $PATHS; do
  f=$(ls -1t $pat 2>/dev/null | head -n 1 || true)
  if [ -n "$f" ] && [ -r "$f" ] && [ -s "$f" ]; then found="$f"; break; fi
done
if [ -n "$found" ]; then
  echo "PATH=$found"
  tail -n "$LINES" "$found"
  exit 0
fi
if command -v journalctl >/dev/null 2>&1; then
  for unit in $UNITS; do
    if journalctl -u "$unit" -n 1 --no-pager >/dev/null 2>&1; then
      echo "PATH=journal:$unit"
      journalctl -u "$unit" -n "$LINES" --no-pager -o short 2>/dev/null
      exit 0
    fi
  done
fi
echo "no readable Valkey log found (tried: $PATHS, then the journal for: $UNITS)" >&2
exit 1`

// pktValkeyLogPaths is where a Valkey log lives when `logfile` is set.
var pktValkeyLogPaths = []string{
	"/var/log/valkey/*.log",
	"/var/log/valkey/valkey.log",
	"/var/log/redis/*.log",
	"/var/lib/valkey/*/valkey.log",
}

// pktValkeyLogUnits is what to ask the journal for, in order.
//
// The templated patterns come first, because that is what a real deployment uses: dbcanvas
// runs valkey@dbcanvas.service (one unit per named instance, so a host can serve several),
// and `journalctl -u valkey` matches none of it. journalctl takes a glob, so the pattern
// is passed through rather than expanded by the shell.
var pktValkeyLogUnits = []string{
	"valkey@*", "valkey", "valkey-server@*", "valkey-server",
	"redis@*", "redis", "redis-server@*", "redis-server",
}
