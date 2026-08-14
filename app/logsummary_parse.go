package main

// logsummary_parse.go — turning a database server's log file into records.
//
// The Packet Inspector already reads server logs (pktserverlog.go and friends), but it
// reads them the way a *correlation* pane needs to: one line, one entry, anything that
// does not match the header pattern discarded. That is right for "show me the aborted
// connections near this packet" and wrong for reading a log on its own, because the
// most informative things a Galera node writes are not single lines:
//
//	2026-08-14T01:42:31.045860Z 0 [Note] [MY-000000] [Galera] Current view of cluster …
//	view (view_id(PRIM,0bc20092-ac42,4)
//	memb {
//		0bc20092-ac42,0
//		23503af2-adb5,0
//		}
//	…
//	partitioned {
//		119e686d-8942,0
//		}
//	)
//
// The header line says nothing at all. Everything that matters — who is still here, who
// left cleanly, who was *partitioned* away — is in the lines below it, and a line-at-a-
// time reader throws all of it away. Same for `Quorum results:` (members = 2/3) and for
// the `View:` block (the member list by name).
//
// So this file folds a log into RECORDS: a header line plus every line under it that is
// not itself a header. The classifier then sees the whole record, header and body, which
// is what makes "two of three members are present and the third was partitioned, not
// shut down" a thing the tool can say.
//
// The second job here is repetition. A partitioned node writes the same peer-timeout
// line every three seconds for as long as the partition lasts — 24 of them in a 50-second
// outage in the capture this was built against. Listing all of them buries the four lines
// that explain the outage, so identical consecutive records collapse into one carrying a
// count and a span.

import (
	"regexp"
	"strings"
	"time"
)

// lsSeverity is the three-way highlight plus a neutral bulk.
//
// It is deliberately NOT the log's own level field. On a Percona XtraDB Cluster node the
// level is nearly useless for this: the capture behind this feature contains a complete
// node crash, an eviction, a state transfer and a rejoin, and across all of it there are
// 314 [Note] lines, 8 [System], 5 [Warning] and zero [ERROR]. "Suspecting node", "declaring
// inactive" and "Shifting SYNCED -> OPEN" — the entire story of an outage — are all
// [Note]. Severity here therefore comes from what a record MEANS, and the level is kept
// only as one more input to it.
const (
	lsSevOK   = "ok"   // the good: the cluster reaching a healthy state
	lsSevWarn = "warn" // the warning: degraded, transitional, or expensive but working
	lsSevBad  = "bad"  // the bad: something is broken, gone, or refusing to serve
	lsSevInfo = "info" // background: real records, but not news
)

// lsSevRank orders severities for "worst wins" folding.
var lsSevRank = map[string]int{lsSevInfo: 0, lsSevOK: 1, lsSevWarn: 2, lsSevBad: 3}

func lsWorse(a, b string) string {
	if lsSevRank[b] > lsSevRank[a] {
		return b
	}
	return a
}

// Event classes. A class groups records by the question they answer, which is what the
// UI filters on — "show me only the membership changes" is the first thing anybody wants
// from three nodes' logs side by side.
const (
	lsClassStartup  = "startup"
	lsClassShutdown = "shutdown"
	lsClassCrash    = "crash"
	lsClassMember   = "membership"
	lsClassState    = "state"
	lsClassTransfer = "transfer"
	lsClassNetwork  = "network"
	lsClassQuorum   = "quorum"
	lsClassFlowCtl  = "flowcontrol"
	lsClassReplica  = "replication"
	lsClassConflict = "conflict"
	lsClassClient   = "client"
	lsClassStorage  = "storage"
	lsClassConfig   = "config"
	lsClassSecurity = "security"
	lsClassOther    = "other"
)

// lsRecord is one folded log record before classification: the header line as written,
// plus the continuation lines beneath it.
type lsRecord struct {
	Line   int      // 1-based line number of the header, for "show me this in the file"
	TS     float64  // epoch seconds; 0 when the header carried no timestamp
	Approx bool     // TS was inherited from the record above (an untimestamped header)
	Time   string   // the stamp as written
	Level  string   // Note | Warning | System | ERROR
	Code   string   // MY-010931 …
	Subsys string   // Server | InnoDB | Galera | WSREP | WSREP-SST
	Thread string   // the connection id column
	Text   string   // the header's message
	Body   []string // continuation lines, verbatim
}

// isCrash reports whether this record is a crash report — either the raw block the signal
// handler wrote, or one of the MY-013951 lines it is re-emitted as.
func (r lsRecord) isCrash() bool {
	return r.Code == lsCodeCrashRelog || lsCrashHeader.MatchString(r.Time+" - "+r.Text) ||
		strings.Contains(r.Text, "mysqld got signal")
}

// bodyText joins the continuation lines for substring matching. Kept separate from Text
// so a rule can require a fragment to be in the header rather than anywhere in a 40-line
// GCS configuration dump.
func (r lsRecord) bodyText() string { return strings.Join(r.Body, "\n") }

// lsMySQLHeader matches MySQL 8 / PXC error-log header lines. It is deliberately its own
// pattern rather than pktLogLine's: this one also captures the thread column and tolerates
// a missing subsystem, and it must NOT match the indented continuation lines that follow
// a view or quorum block.
//
// Both timezone forms are required for the same reason pktserverlog.go needs them:
// log_timestamps defaults to UTC but SYSTEM is common, and a SYSTEM-stamped log writes an
// offset instead of a Z. Parsing with the offset intact is also what lets three nodes in
// three timezones line up on one timeline — the instant is unambiguous even when the
// written stamps are not.
var lsMySQLHeader = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2}T[\d:.]+(?:Z|[+-]\d{2}:\d{2}))\s+(\d+)\s+\[(\w+)\]\s*(?:\[([\w-]+)\])?\s*(?:\[([\w-]+)\])?\s*(.*)$`)

// lsCrashHeader matches the one record MySQL writes in a format of its own.
//
// When mysqld dies on a fatal signal the block that follows is written by the SIGNAL
// HANDLER, which cannot call into the normal logging path — no thread column, no [Level],
// no [MY-nnnnnn], no subsystem:
//
//	2026-08-14T07:51:05Z UTC - mysqld got signal 11 ;
//	Most likely, you have hit a bug, but this error can also be caused by …
//	Server Version: 8.0.46-38.1 Percona XtraDB Cluster (GPL) …
//	Thread pointer: 0x0
//	stack_bottom = 0 thread_stack 0x100000
//	 #0 0x7c27e246dc2f <unknown>
//	 …
//
// Without this pattern none of it is a header, so the whole block folds into the body of
// whatever record came before — and on a live cluster that record was "Member synced with
// group", severity *good*. A crash was being reported as good news. It survived the first
// round of testing because the corpus's crash fixture was made with `kill -9`: SIGKILL
// cannot be caught, so no handler runs and no such block is ever written. A process
// vanishing and a process crashing are different events and only one of them writes this.
//
// Both timestamp forms are matched: MySQL 8 writes RFC3339 with a Z, older builds write
// `YYMMDD HH:MM:SS`, and the " UTC " is absent in some.
var lsCrashHeader = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2}T[\d:.]+Z|\d{6}\s+\d{1,2}:\d{2}:\d{2})\s+(?:UTC\s+)?-+\s+(mysqld got .*)$`)

// lsOrphanHeaders are untimestamped lines that nevertheless start a record of their own.
//
// There is exactly one family of these in a PXC log and it matters: when mysqld is
// restarted after an unclean stop, systemd runs `mysqld --wsrep-recover` first and splices
// its output into the error log with no timestamps at all —
//
//	Log of wsrep recovery (--wsrep-recover):
//	 INFO: WSREP: Recovered position 0bc30087-…:4520
//
// Folded into whatever came before, that block would be invisible; and its presence is the
// single clearest evidence in the file that the node did not shut down cleanly.
var lsOrphanHeaders = []string{
	"Log of wsrep recovery",
	"WSREP_SST:",
}

// lsFoldMySQL splits a MySQL-family log into records.
func lsFoldMySQL(data string) []lsRecord {
	var out []lsRecord
	var cur *lsRecord
	lastTS := 0.0
	for i, raw := range strings.Split(data, "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			// A blank line inside a View: block is part of it; a blank line with no open
			// record is nothing.
			if cur != nil {
				cur.Body = append(cur.Body, "")
			}
			continue
		}
		if m := lsMySQLHeader.FindStringSubmatch(line); m != nil {
			// Percona Server emits the crash handler's block TWICE: once raw, and then
			// again line by line through the normal error log as MY-013951. Every one of
			// those lines is a valid header, so left alone a single crash becomes
			// twenty-odd separate "Server crashed" rows — the backtrace, the "Most likely
			// you have hit a bug" boilerplate and the bug-report URL each promoted to an
			// event of its own. They are one report, so they fold into one record: into
			// the raw block when there is one, and into the first of themselves when the
			// build only writes the re-emitted form.
			if m[4] == lsCodeCrashRelog && cur != nil && cur.isCrash() {
				cur.Body = append(cur.Body, strings.TrimSpace(m[6]))
				continue
			}
			if cur != nil {
				out = append(out, *cur)
			}
			rec := lsRecord{
				Line: i + 1, Time: m[1], Thread: m[2], Level: m[3], Code: m[4], Subsys: m[5],
				Text: strings.TrimSpace(m[6]),
			}
			if ts, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
				rec.TS = float64(ts.UnixNano()) / 1e9
				lastTS = rec.TS
			}
			cur = &rec
			continue
		}
		if m := lsCrashHeader.FindStringSubmatch(line); m != nil {
			if cur != nil {
				out = append(out, *cur)
			}
			rec := lsRecord{Line: i + 1, Time: m[1], Level: "ERROR", Subsys: "Server",
				Text: strings.TrimSpace(m[2])}
			if ts, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
				rec.TS = float64(ts.UnixNano()) / 1e9
				lastTS = rec.TS
			} else {
				// The legacy stamp has no zone and no year separator worth guessing at.
				// Inheriting from the record above is exact enough — the crash happened
				// between it and the restart that follows — and is marked approximate.
				rec.TS, rec.Approx = lastTS, lastTS > 0
			}
			cur = &rec
			continue
		}
		if lsIsOrphanHeader(line) {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &lsRecord{Line: i + 1, TS: lastTS, Approx: lastTS > 0,
				Level: "Note", Subsys: "WSREP", Text: strings.TrimSpace(line)}
			continue
		}
		if cur == nil {
			// Leading junk before the first header — a rotated log opened mid-record.
			continue
		}
		cur.Body = append(cur.Body, line)
	}
	if cur != nil {
		out = append(out, *cur)
	}
	// Trailing blanks in a body are noise from the blank-line rule above.
	for i := range out {
		for len(out[i].Body) > 0 && out[i].Body[len(out[i].Body)-1] == "" {
			out[i].Body = out[i].Body[:len(out[i].Body)-1]
		}
	}
	// An untimestamped record at the very top of a file has nothing above it to inherit
	// from, so it borrows from below instead. This is not a corner case: a log that begins
	// with the wsrep recovery block is what every unclean restart produces, and a record
	// left at time zero would sit at the far left of every timeline it appeared on.
	for i := range out {
		if out[i].TS > 0 {
			continue
		}
		for j := i + 1; j < len(out); j++ {
			if out[j].TS > 0 {
				out[i].TS, out[i].Approx = out[j].TS, true
				break
			}
		}
	}
	return out
}

func lsIsOrphanHeader(line string) bool {
	for _, p := range lsOrphanHeaders {
		if strings.HasPrefix(strings.TrimSpace(line), p) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- events

// lsEvent is one classified record: what happened, how bad it is, and — for the records
// that carry them — the structured facts pulled out of the text.
type lsEvent struct {
	No     int     `json:"no"`  // 1-based across the whole bundle, in time order
	Src    int     `json:"src"` // index into the bundle's sources
	TS     float64 `json:"ts"`
	EndTS  float64 `json:"endTs,omitempty"` // last occurrence when Repeat > 1
	Approx bool    `json:"approx,omitempty"`
	Line   int     `json:"line"`
	Time   string  `json:"time"`
	Level  string  `json:"level"`
	Code   string  `json:"code,omitempty"`
	Subsys string  `json:"subsystem,omitempty"`

	Class   string `json:"class"`
	Sev     string `json:"sev"`
	Label   string `json:"label"`
	Meaning string `json:"meaning,omitempty"` // what it means, in words, for an operator

	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"` // the folded continuation lines
	Repeat  int    `json:"repeat,omitempty"` // >1 when identical records were collapsed

	// Structured facts, set only by the rules that can extract them.
	Peer  string `json:"peer,omitempty"`  // the other node this record names
	State string `json:"state,omitempty"` // wsrep state this record moves the node to
	// From is the state the node was in BEFORE this record. It matters because a log is
	// usually a fragment: a node that was already SYNCED when the excerpt begins never
	// logs a transition INTO SYNCED, so the only evidence of where it started is the
	// left-hand side of its first transition out.
	From    string   `json:"fromState,omitempty"`
	Members int      `json:"members,omitempty"` // cluster size after this record
	Total   int      `json:"total,omitempty"`   // total members seen in a quorum result
	Primary string   `json:"primary,omitempty"` // "yes" | "no" — primary component or not
	Seqno   int64    `json:"seqno,omitempty"`
	Lost    []string `json:"lost,omitempty"` // members partitioned away in a view record
	Left    []string `json:"left,omitempty"` // members that left cleanly
}

// lsSignature is what makes two records "the same thing again". It deliberately excludes
// the numbers that change every time (socket statistics, sequence numbers, timestamps):
// 24 peer-timeout records differing only in their RTT counters are one event repeated,
// not 24 events.
func lsSignature(e lsEvent) string {
	return e.Class + "|" + e.Label + "|" + e.Peer + "|" + e.State
}

// lsCollapseWindow is how far apart two identical records can be and still fold together.
// A partitioned node retries every 3 s and Galera's own keepalive periods are ~1 s, so a
// minute is wide enough to fold a whole outage's worth of retries into one row and narrow
// enough that the same message an hour later is reported as a new occurrence.
const lsCollapseWindow = 60.0

// lsCollapsible is the set of classes whose repeats carry no information.
//
// The whitelist is the important half of this. State transitions, membership changes and
// state transfers must NEVER fold, however identical they look: a node that goes
// JOINED → SYNCED three times in a window did three different things at three different
// times, and folding them destroys the state track the timeline is built from. That is not
// hypothetical — an earlier version folded them, and a node that spent five seconds
// catching up was reported as having spent a minute and a half.
var lsCollapsible = map[string]bool{
	lsClassNetwork:  true,
	lsClassFlowCtl:  true,
	lsClassClient:   true,
	lsClassConfig:   true,
	lsClassSecurity: true,
	lsClassStorage:  true,
	lsClassConflict: true,
	lsClassOther:    true,
}

// lsCollapse folds repeats of the same event into one row carrying a count and a span.
//
// Per SOURCE, never across sources: on three nodes the retries interleave, and folding
// across them would attribute one node's repeats to another.
//
// Not merely adjacent, either. A partitioned node retries each of its peers in turn, so
// its timeout records alternate — .3, .5, .3, .5 — and an adjacent-only fold leaves the
// list exactly as long as it was. Folding into the most recent matching event within the
// window instead turns fifty rows into two, one per peer, which is what the outage
// actually was.
func lsCollapse(events []lsEvent) []lsEvent {
	out := make([]lsEvent, 0, len(events))
	// recent[src][signature] is the index in out of the last event of that kind.
	recent := map[int]map[string]int{}
	for _, e := range events {
		if !lsCollapsible[e.Class] {
			out = append(out, e)
			continue
		}
		sig := lsSignature(e)
		bySig := recent[e.Src]
		if bySig == nil {
			bySig = map[string]int{}
			recent[e.Src] = bySig
		}
		if i, ok := bySig[sig]; ok {
			p := &out[i]
			if e.TS-lsEndOf(*p) <= lsCollapseWindow {
				if p.Repeat == 0 {
					p.Repeat = 1
				}
				p.Repeat++
				p.EndTS = e.TS
				continue
			}
		}
		bySig[sig] = len(out)
		out = append(out, e)
	}
	return out
}

func lsEndOf(e lsEvent) float64 {
	if e.EndTS > e.TS {
		return e.EndTS
	}
	return e.TS
}

// ---------------------------------------------------------------- engine sniffing

// lsSniffEngine decides which product wrote a log. MySQL and PXC share a format, so the
// answer here is the family; lsSniffFlavour then separates a Galera member from a plain
// MySQL server, which is a different question and has a much better signal.
func lsSniffEngine(data string) string {
	my, pg, mg, vk := 0, 0, 0, 0
	for i, line := range strings.Split(data, "\n") {
		if i > 800 || my >= 8 || pg >= 8 || mg >= 8 || vk >= 8 {
			break
		}
		if lsMySQLHeader.MatchString(line) {
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
	best, engine := 0, ""
	for _, c := range []struct {
		n int
		e string
	}{{mg, pktEngineMongoDB}, {pg, pktEnginePostgres}, {vk, pktEngineValkey}, {my, pktEngineMySQL}} {
		if c.n > best {
			best, engine = c.n, c.e
		}
	}
	if engine == "" {
		return pktEngineMySQL
	}
	return engine
}

// Flavours within the MySQL family. A Galera member's log is a different document from a
// standalone server's — most of it is written by a subsystem a standalone server does not
// have — and the catalogue that reads it is correspondingly different.
const (
	lsFlavourGalera = "galera"
	lsFlavourMySQL  = "mysql"
)

// lsSniffFlavour answers "is this a cluster member's log, and whose kind of cluster?" The
// [Galera] and [WSREP] subsystem tags are conclusive and appear within the first few lines
// of any wsrep-enabled server's startup, so that half needs no heuristics worth the name.
//
// Group Replication has no subsystem tag of its own — its records are tagged [Repl], the
// same as an asynchronous replica's — so it is sniffed from the plugin's own prefix and
// its private code ranges instead. See lsGRSniff. Galera is checked first because the two
// are mutually exclusive in practice and the tag is the stronger evidence.
func lsSniffFlavour(recs []lsRecord) string {
	for _, r := range recs {
		switch r.Subsys {
		case "Galera", "WSREP", "WSREP-SST":
			return lsFlavourGalera
		}
	}
	if lsGRSniff(recs) {
		return lsFlavourGroupRepl
	}
	return lsFlavourMySQL
}

// lsNodeName pulls the node's own name out of its log.
//
// Galera writes it in several places; `base_host` in the GCS configuration dump is the
// most reliable because it is present in every start-up, before anything can have gone
// wrong. "Server <name> synced with group" is the fallback, and the file name is the last
// resort — an uploaded pxc02.err is usually named after the node for exactly this reason.
var (
	lsBaseHost  = regexp.MustCompile(`base_host = ([^;\s]+)`)
	lsServerSyn = regexp.MustCompile(`^Server (\S+) (?:synced with group|connected to cluster)`)
)

func lsNodeName(recs []lsRecord) string {
	for _, r := range recs {
		if m := lsServerSyn.FindStringSubmatch(r.Text); m != nil {
			return m[1]
		}
		if m := lsBaseHost.FindStringSubmatch(r.Text); m != nil {
			// base_host is an FQDN; the short name is what every other line uses.
			return strings.SplitN(m[1], ".", 2)[0]
		}
	}
	return ""
}
