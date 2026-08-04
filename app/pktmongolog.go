package main

// pktmongolog.go — MongoDB's server log, which is the most useful of the three and the
// easiest to read: since 4.4 it is one JSON object per line.
//
//	{"t":{"$date":"2026-08-04T07:38:57.037+00:00"},"s":"I","c":"COMMAND","id":51803,
//	 "ctx":"conn65","msg":"Slow query","attr":{…}}
//
// The `id` is a stable numeric identity for the message — MongoDB's equivalent of
// MySQL's MY-010914 — so a record can be recognised without matching English text, and
// `s`/`c` give severity and component for free.
//
// Why this pairs so well with a capture: the wire shows how long a command took and what
// was asked, and the log shows **why** it took that long. `planSummary: COLLSCAN`,
// `docsExamined: 5600`, `keysExamined: 0`, `numYields`, the write-concern wait — none of
// that is on the wire in any form, and all of it is in the log record for the same
// operation, at the same instant. The Packet Inspector already lines the two up by time;
// for MongoDB that correlation is the whole diagnosis rather than a convenience.
//
// The families below are the shared ones (pktserverlog.go's classes), so the class
// filter, the window view and the packet correlation are the same code for all three
// engines.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// mongoLogLine is the shape of one JSON log record. attr is left as raw JSON and read
// selectively: it can be several kilobytes for a slow query, and almost all of it is
// lock-acquisition counts.
type mongoLogLine struct {
	T struct {
		Date string `json:"$date"`
	} `json:"t"`
	S   string          `json:"s"`
	C   string          `json:"c"`
	ID  int             `json:"id"`
	Ctx string          `json:"ctx"`
	Msg string          `json:"msg"`
	Att json.RawMessage `json:"attr"`
}

// mongoLogIDs maps the log ids worth recognising by number. Text can be reworded between
// releases; an id cannot, and these are the records a capture is most often read against.
var mongoLogIDs = map[int]struct {
	class pktLogClass
	label string
}{
	22943:   {pktLogOther, "Connection accepted"},
	22944:   {pktLogAbort, "Connection ended"},
	51800:   {pktLogOther, "Client metadata"},
	51803:   {pktLogOther, "Slow query"},
	20698:   {pktLogCluster, "Election succeeded — this member is now primary"},
	21358:   {pktLogRepl, "Replica set state transition"},
	21106:   {pktLogRepl, "Resetting sync source"},
	21799:   {pktLogCluster, "Election: starting a dry run"},
	4615611: {pktLogLifecycle, "MongoDB starting"},
	23016:   {pktLogLifecycle, "Waiting for connections"},
	20520:   {pktLogLifecycle, "Shutting down"},
	4784900: {pktLogLifecycle, "Shutdown: stopping the replication coordinator"},
	20883:   {pktLogAuth, "Authentication failed"},
	5286300: {pktLogAuth, "Supported SASL mechanisms requested"},
	20250:   {pktLogAuth, "Authentication succeeded"},
	22986:   {pktLogRepl, "Oplog: slow batch application"},
	21095:   {pktLogRepl, "Replication rollback started"},
	5123005: {pktLogCluster, "Sharding: routing table refreshed"},
	22062:   {pktLogCluster, "Chunk migration"},
	4939300: {pktLogRepl, "Failed to refresh a member's view of the set"},
	23015:   {pktLogListen, "Listening on a socket"},
	22945:   {pktLogListen, "Connection refused: too many open connections"},
}

// pktMongoLogFamilies classifies by message text, for everything the id table does not
// cover. Any-of semantics, first match wins (pgApplyFamilies).
var pktMongoLogFamilies = []pktLogFamily{
	// ---- connections that ended, or never started.
	{substr: []string{"Connection ended"}, class: pktLogAbort, label: "Connection ended"},
	{substr: []string{"Interrupted operation as its client disconnected"}, class: pktLogAbort,
		label: "Client disconnected mid-operation"},
	{substr: []string{"Error sending response to client", "Failed to send response"},
		class: pktLogAbort, label: "Could not write to the client"},
	{substr: []string{"connection accepted"}, class: pktLogOther, label: "Connection accepted"},
	{substr: []string{"can't accept new connections", "too many open connections"},
		class: pktLogListen, label: "Connection limit reached — new clients are refused"},
	{substr: []string{"Socket exception", "SocketException"}, class: pktLogAbort, label: "Socket exception"},

	// ---- authentication.
	{substr: []string{"Failed to authenticate", "Authentication failed"}, class: pktLogAuth,
		label: "Authentication failed"},
	{substr: []string{"Unauthorized"}, class: pktLogAuth, label: "Unauthorized command"},
	{substr: []string{"Successfully authenticated"}, class: pktLogAuth, label: "Authenticated"},
	{substr: []string{"key file", "keyFile"}, class: pktLogAuth,
		label: "Internal authentication key problem — the members cannot trust each other"},

	// ---- replication and elections. The heart of a replica-set capture.
	{substr: []string{"Starting an election"}, class: pktLogCluster, label: "Election starting"},
	{substr: []string{"Election succeeded"}, class: pktLogCluster,
		label: "Election succeeded — the primary changed"},
	{substr: []string{"Not starting an election"}, class: pktLogCluster, label: "Election declined"},
	{substr: []string{"Scheduling priority takeover", "Scheduling catchup takeover"},
		class: pktLogCluster, label: "Takeover scheduled — a higher-priority member is about to take over"},
	{substr: []string{"Stepping down", "Stepping up", "transition to primary", "Transition to primary"},
		class: pktLogCluster, label: "Primary role change"},
	{substr: []string{"Member is now in state"}, class: pktLogRepl, label: "A member changed state"},
	{substr: []string{"Rollback"}, class: pktLogRepl,
		label: "Rollback — this member had writes the new primary does not, and is discarding them"},
	{substr: []string{"Starting rollback", "rollback finished"}, class: pktLogRepl, label: "Rollback"},
	{substr: []string{"could not find member to sync from", "sync source candidate"},
		class: pktLogRepl, label: "Sync source selection"},
	{substr: []string{"Stopping replication producer", "Restarting oplog query"},
		class: pktLogRepl, label: "Oplog fetching interrupted"},
	{substr: []string{"too stale to catch up", "RS102", "we are too stale"}, class: pktLogRepl,
		label: "Too stale to catch up — this member needs a full resync"},
	{substr: []string{"Heartbeat failed", "heartbeat request failed", "Error in heartbeat"},
		class: pktLogRepl, label: "Heartbeat failed — this is what precedes an election"},
	{substr: []string{"oplog"}, class: pktLogRepl, label: "Oplog activity"},

	// ---- sharding.
	{substr: []string{"Migration", "migrate", "moveChunk", "chunk"}, class: pktLogCluster,
		label: "Chunk migration / balancer activity"},
	{substr: []string{"routing info", "routing table", "StaleConfig", "shard version"},
		class: pktLogCluster, label: "Routing table refresh (StaleConfig)"},
	{substr: []string{"Balancer"}, class: pktLogCluster, label: "Balancer"},

	// ---- lifecycle.
	{substr: []string{"MongoDB starting", "Waiting for connections", "build environment"},
		class: pktLogLifecycle, label: "Server startup"},
	{substr: []string{"Shutting down", "shutdown", "Terminating via SIGTERM", "got signal"},
		class: pktLogLifecycle, label: "Server shutdown"},
	{substr: []string{"Detected unclean shutdown", "Recovering data"}, class: pktLogLifecycle,
		label: "Crash recovery"},

	// ---- the storage engine and resources.
	{substr: []string{"WiredTiger message", "Checkpoint"}, class: pktLogOther, label: "Storage engine"},
	{substr: []string{"Out of disk space", "No space left"}, class: pktLogOther, label: "Out of disk"},
	{substr: []string{"Slow query"}, class: pktLogOther, label: "Slow query"},
	{substr: []string{"Refusing to write", "cannot write"}, class: pktLogOther, label: "Write refused"},

	// ---- TLS.
	{substr: []string{"SSL", "TLS", "certificate"}, class: pktLogTLS, label: "TLS / certificate"},

	// ---- DNS.
	{substr: []string{"DNS resolution", "Failed to resolve", "getaddrinfo"}, class: pktLogDNS,
		label: "Name resolution failed"},
}

// pktClassifyMongoLogLine turns one JSON log line into an entry. ok is false for a line
// that is not JSON at all — the first lines of a mongod.log can be plain text, and a
// truncated last line is normal in a tail.
func pktClassifyMongoLogLine(line string) (pktLogEntry, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return pktLogEntry{}, false
	}
	var m mongoLogLine
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return pktLogEntry{}, false
	}
	if m.Msg == "" && m.ID == 0 {
		return pktLogEntry{}, false
	}
	e := pktLogEntry{
		Time:    m.T.Date,
		Level:   mongoLogSeverity(m.S),
		Code:    fmt.Sprintf("%d", m.ID),
		Subsys:  m.C,
		Message: pktPrintable(m.Msg),
		Class:   pktLogOther,
	}
	if t, err := time.Parse(time.RFC3339Nano, m.T.Date); err == nil {
		e.TS = float64(t.UnixNano()) / 1e9
	}
	// The id first: it is stable across releases in a way the text is not.
	if known, ok := mongoLogIDs[m.ID]; ok {
		e.Class, e.Label = known.class, known.label
	} else {
		pgApplyFamilies(&e, pktMongoLogFamilies)
	}
	if e.Label == "" {
		e.Label = m.C
		if e.Label == "" {
			e.Label = e.Level
		}
	}
	// The attributes worth carrying: the ones a capture cannot show. A slow query's plan
	// summary and examined counts are the reason it was slow, and the wire has no idea.
	if len(m.Att) > 0 {
		if extra := mongoLogAttrs(m.Att); extra != "" {
			e.Message += " | " + extra
			e.Reason = extra
		}
	}
	// Severity escalates the class: an error in any component is worth seeing.
	if (m.S == "E" || m.S == "F") && e.Class == pktLogOther {
		e.Class = pktLogAbort
	}
	return e, true
}

// mongoLogSeverity spells out MongoDB's one-letter severities.
func mongoLogSeverity(s string) string {
	switch s {
	case "F":
		return "FATAL"
	case "E":
		return "ERROR"
	case "W":
		return "WARNING"
	case "I":
		return "INFO"
	}
	if strings.HasPrefix(s, "D") {
		return "DEBUG" + strings.TrimPrefix(s, "D")
	}
	return s
}

// mongoLogAttrs pulls the handful of attributes worth one line out of a record's attr
// object. It reads into a map rather than a struct because attr's shape depends entirely
// on the record, and it takes only scalars — a slow query's `command` and `locks` are
// nested objects several kilobytes deep and are not what a correlated view needs.
func mongoLogAttrs(raw json.RawMessage) string {
	var att map[string]any
	if err := json.Unmarshal(raw, &att); err != nil {
		return ""
	}
	var parts []string
	add := func(label, key string) {
		v, ok := att[key]
		if !ok {
			return
		}
		switch t := v.(type) {
		case string:
			if t != "" {
				parts = append(parts, label+"="+pktEllipsis(pktPrintable(t), 70))
			}
		case float64:
			parts = append(parts, fmt.Sprintf("%s=%g", label, t))
		case bool:
			parts = append(parts, fmt.Sprintf("%s=%t", label, t))
		}
	}
	// A slow query, in the order somebody reading it wants: what and where, how long,
	// and then the plan that explains it.
	add("ns", "ns")
	add("type", "type")
	add("durationMillis", "durationMillis")
	add("planSummary", "planSummary")
	add("keysExamined", "keysExamined")
	add("docsExamined", "docsExamined")
	add("nreturned", "nreturned")
	add("nMatched", "nMatched")
	add("nModified", "nModified")
	add("numYields", "numYields")
	add("waitForWriteConcernDurationMillis", "writeConcernWaitMillis")
	// Connections, elections, replication.
	add("remote", "remote")
	add("client", "client")
	add("connectionId", "connectionId")
	add("newState", "newState")
	add("oldState", "oldState")
	add("term", "term")
	add("reason", "reason")
	add("error", "error")
	add("errmsg", "errmsg")
	if len(parts) > 8 {
		parts = parts[:8]
	}
	return strings.Join(parts, " ")
}

// pktMongoLogTailScript finds the newest mongod log and tails it. mongod does not rotate
// by default (logRotate is a command, not a schedule), so a single path is usually right
// — but a rotated file is named mongod.log.2026-08-04T07-38-57, which sorts after the
// live one, so the glob is ordered by mtime like PostgreSQL's.
const pktMongoLogTailScript = `set -e
found=""
for pat in $PATHS; do
  f=$(ls -1t $pat 2>/dev/null | head -n 1 || true)
  if [ -n "$f" ] && [ -r "$f" ]; then found="$f"; break; fi
done
if [ -z "$found" ]; then
  echo "no readable MongoDB log found (tried: $PATHS)" >&2
  exit 1
fi
echo "PATH=$found"
tail -n "$LINES" "$found"
exit 0`

// pktMongoLogPaths is where a MongoDB log lives: what dbcanvas provisions first, then the
// distribution and container defaults.
var pktMongoLogPaths = []string{
	"/var/log/mongo/mongod.log",    // Percona Server for MongoDB on RHEL, what dbcanvas configures
	"/var/log/mongodb/mongod.log",  // the upstream/Debian default
	"/var/log/mongodb/mongodb.log", //
	"/data/db/mongod.log",          // a common container layout
	"/var/log/mongo/mongos.log",    // a router
	"/var/log/mongodb/mongos.log",  //
}
