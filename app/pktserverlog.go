package main

// pktserverlog.go — the half of the story a capture cannot tell.
//
// MySQL's communication failures split three ways, and only the first is on the wire:
//
//	ERR packets (1040, 1045, 1153, 1156, 1158, 1835 …)
//	    the server told the client, so a capture sees it — pktErrCatalog names these.
//	client codes (2003, 2006, 2013, 2026, 2027 …)
//	    the client library's own diagnosis. These NEVER cross the wire; what a
//	    capture sees is the evidence behind them (an unanswered SYN, a reset with a
//	    query in flight, a TLS alert), which pktdecode.go flags.
//	error-log records (MY-010055, MY-013104, MY-010262, MY-015010 …)
//	    written by the server to its own log and never sent to anybody. A capture is
//	    blind to these by construction: the events that matter most here are the ones
//	    where the server refused, aborted or never accepted the connection, so there
//	    is no client to tell.
//
// That last class is what this file reads. The node's error log is tailed, each line
// classified against the families below, and the result narrowed to the capture's own
// time window — so "the aborted-connection entries that belong to the packets you are
// looking at" is a question with an answer.
//
// It is a *reader*, not a parser of everything: the goal is to recognise the network
// and connection families reliably and pass everything else through with its MY- code
// intact, rather than to model MySQL's whole error-log vocabulary.

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// pktLogClass groups an error-log record the way pktErrClass groups an ERR packet.
type pktLogClass string

const (
	pktLogAbort  pktLogClass = "aborted"     // a connection died mid-flight
	pktLogAuth   pktLogClass = "auth"        // refused at connect
	pktLogDNS    pktLogClass = "dns"         // name/address resolution
	pktLogListen pktLogClass = "listener"    // bind/listen/socket problems at startup
	pktLogTLS    pktLogClass = "tls"         // certificates and TLS setup
	pktLogRepl   pktLogClass = "replication" // replica/source transport
	// Lifecycle and cluster state: not "errors", but the two things that explain a
	// capture full of resets and refusals wholesale. A real PXC capture paired with its
	// log had 483 records in the window and the first one was "Received SHUTDOWN" —
	// which is the entire reason the packets showed 1053s, wsrep-not-ready errors and
	// 247 refused connections.
	pktLogLifecycle pktLogClass = "lifecycle"
	pktLogCluster   pktLogClass = "cluster"
	pktLogOther     pktLogClass = "other"
)

// pktLogEntry is one classified error-log line.
type pktLogEntry struct {
	TS      float64     `json:"ts"`   // epoch seconds, 0 when the line carries no timestamp
	Time    string      `json:"time"` // as written in the log
	Level   string      `json:"level"`
	Code    string      `json:"code"`      // MY-010055 …
	Subsys  string      `json:"subsystem"` // Server, InnoDB, Repl …
	Class   pktLogClass `json:"class"`
	Label   string      `json:"label"`  // what it means, in words
	Reason  string      `json:"reason"` // the parenthesised reason, for aborted connections
	Message string      `json:"message"`
	InWin   bool        `json:"inWindow"` // falls inside the capture's time range
}

// pktLogFamily recognises one family of records. Code is matched first when present,
// because it is stable across versions; the text is the fallback for the many messages
// that carry no MY- number.
type pktLogFamily struct {
	codes  []string
	substr []string // any one of these is a match
	// allOf is for the families that can only be told apart by two fragments at once:
	// "role … does not exist" and "database … does not exist" differ by one word, and
	// PostgreSQL puts the name between them. Empty for every other family.
	allOf []string
	class pktLogClass
	label string
}

// pktLogFamilies covers the network-facing families, including the ones the user of a
// packet capture will be looking for: aborted connections and their reasons.
var pktLogFamilies = []pktLogFamily{
	{codes: []string{"MY-010914", "MY-013104", "MY-013130"}, substr: []string{"Aborted connection"},
		class: pktLogAbort, label: "Aborted connection"},
	{substr: []string{"This connection closed normally without authentication"},
		class: pktLogAbort, label: "Closed before authenticating"},
	{substr: []string{"disconnected by the server because of inactivity"},
		class: pktLogAbort, label: "Disconnected for inactivity (wait_timeout)"},
	{substr: []string{"Got an error reading communication packets"},
		class: pktLogAbort, label: "Aborted: error reading communication packets"},
	{substr: []string{"Got an error writing communication packets"},
		class: pktLogAbort, label: "Aborted: error writing communication packets"},
	{substr: []string{"Got timeout reading communication packets"},
		class: pktLogAbort, label: "Aborted: timeout reading communication packets"},
	{substr: []string{"Got timeout writing communication packets"},
		class: pktLogAbort, label: "Aborted: timeout writing communication packets"},
	{substr: []string{"Got packets out of order"},
		class: pktLogAbort, label: "Aborted: packets out of order"},

	{codes: []string{"MY-010926"}, substr: []string{"Access denied for user", "is blocked because of many connection errors"},
		class: pktLogAuth, label: "Connection refused at authentication"},
	{substr: []string{"Too many connections"}, class: pktLogAuth, label: "Too many connections"},

	{codes: []string{"MY-010055"}, substr: []string{"could not be resolved"},
		class: pktLogDNS, label: "Client IP could not be resolved"},
	{codes: []string{"MY-010056"}, class: pktLogDNS, label: "Host name could not be resolved"},
	{codes: []string{"MY-010057"}, class: pktLogDNS, label: "Reverse DNS produced an IP-like hostname"},
	{codes: []string{"MY-010058"}, class: pktLogDNS, label: "Hostname does not resolve back to its IP"},

	{codes: []string{"MY-010249", "MY-010250", "MY-010255", "MY-010256", "MY-010257",
		"MY-010260", "MY-010261", "MY-010262", "MY-010265", "MY-010266"},
		class: pktLogListen, label: "TCP listener problem"},
	{codes: []string{"MY-010258", "MY-010259", "MY-010267", "MY-010268", "MY-010269",
		"MY-010270", "MY-010271"},
		class: pktLogListen, label: "Unix socket problem"},

	{codes: []string{"MY-013595", "MY-015005", "MY-015006", "MY-015007", "MY-015008",
		"MY-015009", "MY-015010", "MY-015011"},
		class: pktLogTLS, label: "TLS / certificate problem"},
	{codes: []string{"MY-010068"}, substr: []string{"self signed"},
		class: pktLogTLS, label: "Self-signed certificate in use"},

	{codes: []string{"MY-013172"}, substr: []string{"Received SHUTDOWN", "Received shutdown signal",
		"Shutdown replication", "Shutdown complete", "Giving", "mysqld: Shutdown"},
		class: pktLogLifecycle, label: "Server shutdown"},
	{codes: []string{"MY-010931", "MY-011323"}, substr: []string{"ready for connections", "Starting MySQL", "starting as process"},
		class: pktLogLifecycle, label: "Server startup"},

	// wsrep state, which is what makes a PXC capture legible: a node that is not SYNCED
	// refuses queries with 1047, and a cluster view change is why connections dropped.
	{substr: []string{"Server status change", "Shifting ", "New cluster view", "Quorum results",
		"evicting", "Member ", "SST request", "IST request", "gcomm: terminating",
		"turning message relay", "declaring node"},
		class: pktLogCluster, label: "Cluster state change"},

	{substr: []string{"error connecting to source", "error reconnecting to source",
		"Error reading packet from server", "Net error reading from source",
		"Net error writing to source", "COM_REGISTER_REPLICA failed",
		"Error requesting binary log dump", "replica I/O thread"},
		class: pktLogRepl, label: "Replication transport problem"},
}

// pktLogLine matches MySQL 8's error-log format:
//
//	2026-08-03T16:27:07.737302Z      0 [Warning] [MY-010068] [Server] CA certificate …
//	2026-08-03T16:27:07.737302+08:00 0 [Warning] [MY-010068] [Server] CA certificate …
//
// Both zone forms are required, not optional: `log_timestamps` is UTC by default but
// SYSTEM is common, and a SYSTEM-stamped log writes an offset instead of a Z. The first
// version of this pattern demanded the Z and therefore rejected every line of such a
// log — which does not matter for a node this app provisioned, and matters completely
// for an uploaded log from somebody else's server.
var pktLogLine = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2}T[\d:.]+(?:Z|[+-]\d{2}:\d{2}))\s+(\d+)\s+\[(\w+)\]\s*(?:\[(MY-\d+)\])?\s*(?:\[(\w+)\])?\s*(.*)$`)

// pktLogReason pulls the parenthesised reason out of an aborted-connection line.
var pktLogReason = regexp.MustCompile(`\(([^()]*)\)[.\s]*$`)

// pktClassifyLogLine turns one raw line into an entry. ok is false for a line that is
// not a recognisable error-log record (a stack trace, a continuation).
func pktClassifyLogLine(line string) (pktLogEntry, bool) {
	m := pktLogLine.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
	if m == nil {
		return pktLogEntry{}, false
	}
	e := pktLogEntry{
		Time: m[1], Level: m[3], Code: m[4], Subsys: m[5],
		Message: strings.TrimSpace(m[6]), Class: pktLogOther,
	}
	// RFC3339 covers both zone forms; the offset one carries its own zone, so an
	// uploaded log from a server on another continent still lands at the right instant.
	if ts, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
		e.TS = float64(ts.UnixNano()) / 1e9
	}
	// Codes first, across every family: a MY- number identifies a record exactly,
	// while the text patterns overlap (several families say "could not be resolved").
	matched := false
	for _, f := range pktLogFamilies {
		for _, c := range f.codes {
			if e.Code != "" && e.Code == c {
				e.Class, e.Label, matched = f.class, f.label, true
				break
			}
		}
		if matched {
			break
		}
	}
	if !matched {
		for _, f := range pktLogFamilies {
			for _, sub := range f.substr {
				if strings.Contains(e.Message, sub) {
					e.Class, e.Label, matched = f.class, f.label, true
					break
				}
			}
			if matched {
				break
			}
		}
	}
	if e.Class == pktLogAbort {
		if r := pktLogReason.FindStringSubmatch(e.Message); r != nil {
			e.Reason = strings.TrimSpace(r[1])
		}
	}
	if e.Label == "" {
		e.Label = e.Level
	}
	return e, true
}

// pktLogTail reads the end of the node's MySQL error log. The path follows the
// provisioner's own choice (pxcLogError), and a couple of common alternatives are
// tried so an unusual node still works.
const pktLogTailScript = `set -e
for f in $PATHS; do
  if [ -r "$f" ]; then echo "PATH=$f"; tail -n "$LINES" "$f"; exit 0; fi
done
echo "PATH=" >&2
echo "no readable MySQL error log found (tried: $PATHS)" >&2
exit 1`

// pktServerLogPaths is where a dbcanvas MySQL node keeps its error log, plus the
// distribution defaults for a node provisioned elsewhere.
var pktServerLogPaths = []string{
	"/var/log/mysqld.log",      // Percona/RHEL, what pxcLogError chooses
	"/var/log/mysql/error.log", // Debian/Ubuntu
	"/var/log/mysql/mysqld.log",
	"/var/lib/mysql/error.log",
}

// pktReadServerLog tails and classifies the node's server log. Which log, and which
// format it is in, follows the engine: MySQL's single error log at a fixed path, or
// PostgreSQL's day-of-week-rotated file plus Patroni's own log when the node has one.
func (a *App) pktReadServerLog(ctx context.Context, containerID, engine string, lines int) ([]pktLogEntry, string, error) {
	if lines <= 0 || lines > 20000 {
		lines = 2000
	}
	script, env := pktLogTailScript, []string{
		"PATHS=" + strings.Join(pktServerLogPaths, " "), "LINES=" + strconv.Itoa(lines)}
	switch engine {
	case pktEnginePostgres:
		script, env = pktPGLogTailScript, []string{
			"PATHS=" + strings.Join(pktPGLogPaths, " "),
			"PATRONI_PATHS=" + strings.Join(pktPatroniLogPaths, " "),
			"LINES=" + strconv.Itoa(lines)}
	case pktEngineMongoDB:
		script, env = pktMongoLogTailScript, []string{
			"PATHS=" + strings.Join(pktMongoLogPaths, " "), "LINES=" + strconv.Itoa(lines)}
	case pktEngineValkey:
		// The only engine here whose log is not a file by default: dbcanvas sets no
		// `logfile`, so Valkey writes to stdout and systemd captures it.
		script, env = pktValkeyLogTailScript, []string{
			"PATHS=" + strings.Join(pktValkeyLogPaths, " "),
			"UNITS=" + strings.Join(pktValkeyLogUnits, " "),
			"LINES=" + strconv.Itoa(lines)}
	}
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", script}, env)
	if err != nil {
		return nil, "", err
	}
	if res.Code != 0 {
		return nil, "", fmt.Errorf("%s", strings.TrimSpace(res.Stderr+res.Stdout))
	}
	path := ""
	var out []pktLogEntry
	for _, line := range strings.Split(res.Stdout, "\n") {
		if p, ok := strings.CutPrefix(line, "PATH="); ok {
			path = strings.TrimSpace(p)
			continue
		}
		// The PostgreSQL script may append Patroni's log after the PostgreSQL one;
		// both are shown, and the path says so.
		if p, ok := strings.CutPrefix(line, "ALSO="); ok {
			path += " + " + strings.TrimSpace(p)
			continue
		}
		out = pktAppendLogLine(out, line, engine)
	}
	return out, path, nil
}

// pktAppendLogLine classifies one line for the right engine and appends it, folding a
// PostgreSQL continuation record (DETAIL, HINT, STATEMENT …) into the record above it.
// Those lines are not events of their own: STATEMENT carries the SQL that produced the
// ERROR on the line before, and shown separately it is an orphan fragment.
func pktAppendLogLine(out []pktLogEntry, line, engine string) []pktLogEntry {
	if engine == pktEngineValkey {
		if e, ok := pktClassifyValkeyLogLine(line); ok {
			return append(out, e)
		}
		return out
	}
	if engine == pktEngineMongoDB {
		// One JSON object per line, so there are no continuations to fold and nothing to
		// resynchronise: a line either parses or it is not a record.
		if e, ok := pktClassifyMongoLogLine(line); ok {
			return append(out, e)
		}
		return out
	}
	if engine == pktEnginePostgres {
		e, continuation, ok := pktClassifyPGLogLine(line)
		if !ok {
			return out
		}
		if continuation {
			if n := len(out); n > 0 {
				out[n-1].Message += " | " + e.Level + ": " + e.Message
				if e.Level == "DETAIL" && out[n-1].Reason == "" {
					out[n-1].Reason = e.Message
				}
			}
			return out
		}
		return append(out, e)
	}
	if e, ok := pktClassifyLogLine(line); ok {
		return append(out, e)
	}
	return out
}

// pktAbortStatsScript reads the counters and settings that decide whether aborted
// connections are even loggable on this server. Run over the local socket, so it
// works when the network path is exactly what is being investigated.
const pktAbortStatsScript = `set -e
mysql --protocol=socket -u"$DB_USER" -p"$DB_PW" -N -B -e "
  SELECT 'verbosity', @@log_error_verbosity
  UNION ALL SELECT 'suppression', IFNULL(@@log_error_suppression_list,'')
  UNION ALL SELECT VARIABLE_NAME, VARIABLE_VALUE FROM performance_schema.global_status
    WHERE VARIABLE_NAME IN ('Aborted_clients','Aborted_connects','Connection_errors_max_connections',
      'Connection_errors_internal','Connection_errors_peer_address','Connection_errors_select',
      'Connection_errors_tcpwrap','Connection_errors_accept','Ssl_accept_renegotiates')" 2>/dev/null || true
exit 0`

// pktAbortStats is what the server itself counts, whether or not it logs anything.
//
// This matters because a vanished client increments Aborted_clients but does NOT
// necessarily produce a log note: the note needs the server to hit a real read/write
// error, and even then only at log_error_verbosity 3. A tool that only read the log
// would report "no aborted connections" on a server with thousands of them.
type pktAbortStats struct {
	Verbosity   int               `json:"verbosity"`
	Suppression string            `json:"suppressionList"`
	Counters    map[string]string `json:"counters"`
	Hint        string            `json:"hint,omitempty"`
}

// pktAbortStatsFor asks the server for its own counters, when the server is one that
// has them. PostgreSQL does not: it logs every aborted connection regardless of
// verbosity, so the question MySQL's counters answer ("is the log even telling me?")
// does not arise, and there is nothing to read.
func (a *App) pktAbortStatsFor(ctx context.Context, containerID, engine, user, pass string) pktAbortStats {
	switch engine {
	case pktEnginePostgres:
		return pktAbortStats{Counters: map[string]string{},
			Hint: "PostgreSQL logs a dropped or refused connection unconditionally — there is no verbosity setting to check and no Aborted_clients equivalent to read."}
	case pktEngineMongoDB:
		return pktAbortStats{Counters: map[string]string{},
			Hint: "MongoDB logs every connection accepted and ended (ids 22943 and 22944) at its default verbosity, so there is nothing to switch on and no counter to read."}
	case pktEngineValkey:
		return pktAbortStats{Counters: map[string]string{},
			Hint: "Valkey has no aborted-connection counters: INFO's stats section counts rejected_connections and total_connections_received, and a client that simply vanished leaves nothing behind but a log line."}
	}
	return a.pktAbortStats(ctx, containerID, user, pass)
}

func (a *App) pktAbortStats(ctx context.Context, containerID, user, pass string) pktAbortStats {
	out := pktAbortStats{Counters: map[string]string{}}
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", pktAbortStatsScript},
		[]string{"DB_USER=" + user, "DB_PW=" + pass})
	if err != nil {
		return out
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		switch k {
		case "verbosity":
			out.Verbosity, _ = strconv.Atoi(v)
		case "suppression":
			out.Suppression = v
		default:
			out.Counters[k] = v
		}
	}
	if out.Verbosity > 0 && out.Verbosity < 3 {
		out.Hint = "log_error_verbosity is " + strconv.Itoa(out.Verbosity) +
			": aborted-connection notes are not written at all below 3. The Aborted_clients counter still moves."
	} else if n := out.Counters["Aborted_clients"]; n != "" && n != "0" {
		out.Hint = "Aborted_clients is " + n +
			" — the server has counted that many clients disappearing without a clean QUIT, which it only writes a log note for when the disconnect produced a read/write error."
	}
	return out
}

// ---------------------------------------------------------------- windowing

// pktLogStat is one classified family and how often it appeared.
type pktLogStat struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// pktLogView is what the endpoint returns: the records that belong to a capture's
// window, what families they fall into, and the log's own extent.
type pktLogView struct {
	Entries  []pktLogEntry `json:"entries"`
	Top      []pktLogStat  `json:"top"`
	Scanned  int           `json:"scanned"`
	InWindow int           `json:"inWindow"`
	LogFrom  float64       `json:"logFrom"`
	LogTo    float64       `json:"logTo"`
	// Mismatch is set when the log holds timestamped records but NONE of them fall in
	// the capture's window. For an uploaded pair that is the likely mistake — a log
	// from a different day, a different server, or a rotation that already dropped the
	// relevant lines — and it needs saying, because "no events" and "wrong file" look
	// identical otherwise.
	Mismatch bool `json:"mismatch"`
}

// pktLogWindowView filters classified records to a capture's window and counts families.
// `all` keeps records outside the window (marked as such) instead of dropping them.
func pktLogWindowView(entries []pktLogEntry, from, to float64, class string, all bool) pktLogView {
	v := pktLogView{Entries: []pktLogEntry{}, Top: []pktLogStat{}, Scanned: len(entries)}
	counts := map[string]int{}
	timed := 0
	for _, e := range entries {
		if e.TS > 0 {
			timed++
			if v.LogFrom == 0 || e.TS < v.LogFrom {
				v.LogFrom = e.TS
			}
			if e.TS > v.LogTo {
				v.LogTo = e.TS
			}
		}
		// A record with no readable timestamp cannot be excluded on time.
		e.InWin = e.TS == 0 || (e.TS >= from && e.TS <= to)
		if e.InWin {
			v.InWindow++
		}
		if class != "" && string(e.Class) != class {
			continue
		}
		if !all && !e.InWin {
			continue
		}
		if e.Class != pktLogOther {
			counts[e.Label]++
		}
		v.Entries = append(v.Entries, e)
	}
	for l, n := range counts {
		v.Top = append(v.Top, pktLogStat{l, n})
	}
	sort.Slice(v.Top, func(i, j int) bool {
		if v.Top[i].Count != v.Top[j].Count {
			return v.Top[i].Count > v.Top[j].Count
		}
		return v.Top[i].Label < v.Top[j].Label
	})
	v.Mismatch = timed > 0 && v.InWindow == 0
	return v
}

// ---------------------------------------------------------------- HTTP

// handlePktServerLog returns the error-log records around a capture's window.
//
// Where the records come from depends on the capture. A capture taken on a node tails
// that node's own log. An **uploaded** capture uses the log uploaded with it — a pcap from
// somebody else's server has no node to ask, and the log is exactly the half of the story
// the packets cannot tell, so it can be carried along with them.
//
// Query parameters: `lines` (how much of a node's log to tail), `class` (a pktLogClass),
// and `all=1` to include records outside the capture window rather than only overlapping
// ones.
func (a *App) handlePktServerLog(w http.ResponseWriter, r *http.Request) {
	c, ok := a.pktOwned(w, r)
	if !ok {
		return
	}
	class := strings.TrimSpace(r.URL.Query().Get("class"))
	all := r.URL.Query().Get("all") == "1"

	// The capture's window, widened a little: an aborted-connection record is written
	// when the server gives up, which can be seconds after the packets that explain it.
	const margin = 30.0
	from, to := c.Summary.FirstTS-margin, c.Summary.LastTS+margin

	if c.Source != "node" || c.NodeID == "" {
		c.mu.Lock()
		entries, name := c.logEntries, c.LogFile
		c.mu.Unlock()
		if len(entries) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{
				"path": "", "source": "upload", "entries": []pktLogEntry{}, "top": []pktLogStat{},
				"scanned": 0, "windowFrom": from, "windowTo": to,
				"note": "no server log was uploaded with this capture — upload one alongside the pcap to correlate",
			})
			return
		}
		v := pktLogWindowView(entries, from, to, class, all)
		writeJSON(w, http.StatusOK, map[string]any{
			"path": name, "source": "upload", "entries": v.Entries, "top": v.Top,
			"scanned": v.Scanned, "inWindow": v.InWindow, "logFrom": v.LogFrom, "logTo": v.LogTo,
			"mismatch": v.Mismatch, "windowFrom": from, "windowTo": to,
		})
		return
	}

	u, _ := a.currentUser(r)
	_, containerID, _, dbUser, dbPass, _, err := a.pktResolveTarget(u, c.StackID, c.NodeID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	entries, path, err := a.pktReadServerLog(r.Context(), containerID, c.Engine,
		atoiDef(r.URL.Query().Get("lines"), 2000))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	v := pktLogWindowView(entries, from, to, class, all)
	writeJSON(w, http.StatusOK, map[string]any{
		"path": path, "source": "node", "entries": v.Entries, "top": v.Top,
		"scanned": v.Scanned, "inWindow": v.InWindow, "logFrom": v.LogFrom, "logTo": v.LogTo,
		"mismatch": v.Mismatch, "windowFrom": from, "windowTo": to,
		// Aborted_clients and log_error_verbosity are MySQL's; PostgreSQL logs an
		// aborted connection unconditionally, so there is no equivalent question to
		// ask and no equivalent counter to read.
		"stats": a.pktAbortStatsFor(r.Context(), containerID, c.Engine, dbUser, dbPass),
	})
}

// pktMaxLogBytes bounds an uploaded error log. A rotated MySQL log is usually a few MB;
// this leaves room for a busy one without letting an upload eat the app's heap.
const pktMaxLogBytes = 64 << 20

// pktMaxLogEntries bounds what is kept from one. Past this the panel is not the right
// tool anyway.
const pktMaxLogEntries = 200000

// pktParseServerLog classifies a whole uploaded log file for the given engine
// ("mysql" or "postgres"; empty means try both and keep whichever recognised more).
//
// Sniffing rather than asking matters for the upload path: somebody with a capture and
// a log has no reason to also tell the tool which product wrote the log, and the two
// formats are unmistakable — MySQL's stamp is RFC3339 with a T in it, PostgreSQL's has
// a space and a zone name.
func pktParseServerLog(b []byte, engine string) []pktLogEntry {
	if engine == "" {
		engine = pktSniffLogEngine(b)
	}
	var out []pktLogEntry
	for _, line := range strings.Split(string(b), "\n") {
		if len(out) >= pktMaxLogEntries {
			break
		}
		out = pktAppendLogLine(out, line, engine)
	}
	return out
}

// pktSniffLogEngine decides which product wrote a log, by trying both classifiers over
// the first lines that parse at all.
func pktSniffLogEngine(b []byte) string {
	my, pg, mg, vk := 0, 0, 0, 0
	for i, line := range strings.Split(string(b), "\n") {
		if i > 500 || my >= 5 || pg >= 5 || mg >= 5 || vk >= 5 {
			break
		}
		if _, ok := pktClassifyLogLine(line); ok {
			my++
		}
		if _, _, ok := pktClassifyPGLogLine(line); ok {
			pg++
		}
		if _, ok := pktClassifyMongoLogLine(line); ok {
			mg++
		}
		if _, ok := pktClassifyValkeyLogLine(line); ok {
			vk++
		}
	}
	best, engine := 0, pktEngineMySQL
	for _, cand := range []struct {
		n      int
		engine string
	}{{mg, pktEngineMongoDB}, {pg, pktEnginePostgres}, {vk, pktEngineValkey}, {my, pktEngineMySQL}} {
		if cand.n > best {
			best, engine = cand.n, cand.engine
		}
	}
	return engine
}
