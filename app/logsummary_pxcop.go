package main

// logsummary_pxcop.go — the two logs a Percona Operator for MySQL (PXC) cluster writes
// that are not a database's.
//
// A PXC cluster deployed by DBCanvas onto a K3D frame writes THREE kinds of log, and only
// one of them is the one the rest of this package already reads:
//
//	the operator          a controller-runtime process, one tab-separated line per event,
//	                      with the facts in a trailing JSON object
//	the PITR collector    a sidecar Deployment that copies binary logs to S3, written with
//	                      Go's standard `log` package: a date, a time, and a sentence
//	each member's mysqld  an ordinary Galera error log, which logsummary_galera.go reads
//
// The member logs are where an outage is described. The operator log is where the
// *decisions* are — a rolling restart, a backup, a restore, which pod it considers the
// primary — and the collector log is the only place the state of point-in-time recovery
// is written down at all. None of the three contains another's story, which is the same
// argument for reading them together that PostgreSQL/Patroni made (see lsPGTailScript).
//
// Every rule here was written against a live cluster: PXC operator 1.20.0 running PXC
// 8.4.8-8.1 on k3s v1.36.3, driven through a bootstrap, sustained write load, two full
// backups, a full restore, a point-in-time restore, PITR enabled and disabled, a member
// force-deleted under load, a member cut off with netem, a cr.yaml change that triggered a
// smart update, and two different ways of making a backup fail. The corpus is in
// app/testdata/logsummary/k*/ and the tests read it.
//
// Four things that capture taught, each of which changed the code:
//
//  1. The operator logs its errors at INFO. `reconcile replication error` — a failure to
//     reach the cluster at all — is an INFO record with the failure inside an `err` field.
//     Meanwhile `ERROR Reconciler error` is emitted once per retry with an exponential
//     backoff, so one broken thing produces dozens of them. Neither the level nor the
//     count is the severity, and for the same reason as everywhere else in this package:
//     severity comes from what the record means.
//
//  2. An ERROR record is not one line. controller-runtime appends the Go stack trace of
//     the failure underneath it, unindented, so a line-at-a-time reader turns one failed
//     reconcile into eight events named after functions in `sigs.k8s.io`. Folding is what
//     makes the operator log readable, exactly as it is for a Galera view block.
//
//  3. The operator log says nothing at all about a member dying. Force-deleting
//     cluster1-pxc-1 under write load produced a complete eviction-and-rejoin story in the
//     other two members' logs and, in the operator's, two minutes of `Updated PITR
//     timelines`. See lsPXCOpFindingSilentOnMembers.
//
//  4. The JSON object has DUPLICATE KEYS, and both occurrences matter. Every reconcile
//     record carries `"namespace"` and `"name"` for the object being reconciled and then
//     again for whatever the message is about:
//
//	{… "namespace": "pxc", "name": "cluster1", … "name": "178f8-daily-backup", "schedule": "0 0 * * *"}
//
//     Decoding that into a map keeps the last and throws the cluster's name away. So the
//     fields are read in order and kept in order (lsOpFields), and a rule asks for the
//     first or the last by name.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The engine and its two flavours.
//
// "operator" is not a database engine and is deliberately not spelled like one: the source
// is a Kubernetes controller, it has no port, no query language and no data, and the page
// says so rather than filing it under MySQL because it happens to manage MySQL.
const pktEngineOperator = "operator"

const (
	lsFlavourPXCOperator = "pxcoperator" // the percona-xtradb-cluster-operator controller log
	lsFlavourPXCPITR     = "pxcpitr"     // the binlog collector Deployment's log
)

// Classes for the things only a controller does. The rest of the operator's records reuse
// the shared classes — a TLS certificate is lsClassSecurity wherever it is written.
const (
	lsClassReconcile = "reconcile" // the controller loop itself: leases, retries, watches
	lsClassBackup    = "backup"
	lsClassRestore   = "restore"
	lsClassRollout   = "rollout" // smart update: the operator restarting members in order
	lsClassPITR      = "pitr"
)

// The state track for these two sources.
//
// Neither is a database, so neither has a database's states. What each one has is a single
// question worth a coloured lane: was the controller the one actually reconciling, and was
// the collector actually collecting.
const (
	lsStateOpLeader   = "LEADER"     // holds the leader lease and is reconciling
	lsStateOpFollower = "NOT-LEADER" // running, waiting for the lease — it changes nothing
	lsStatePITRUp     = "COLLECTING" // uploading binary logs on schedule
	lsStatePITRGap    = "PITR-GAP"   // running, but the binlogs it needs are already gone
	lsStatePITRPaused = "PITR-OFF"   // the collector stopped: nothing is being uploaded
)

var lsOpStateMeaning = map[string]string{
	lsStateOpLeader:   "the operator holds the leader lease: this is the process actually reconciling the cluster",
	lsStateOpFollower: "the operator is running but does not hold the lease — it is watching, and changing nothing",
	lsStatePITRUp:     "the binlog collector is uploading binary logs, so point-in-time recovery can reach the present",
	lsStatePITRGap:    "the collector is running and cannot continue the sequence: a binary log it needed is already gone, so recovery cannot cross this point",
	lsStatePITRPaused: "no binlog collector is running — every transaction from here on can only be recovered by restoring a full backup and losing the rest",
}

// ---------------------------------------------------------------- folding

// lsOpHeader matches a controller-runtime (zap) console line.
//
// The shape is tab-separated and the number of fields is 4 OR 5, because the logger name
// is optional:
//
//	2026-08-22T05:20:18.559Z  INFO  setup  Manager starting up  {"gitCommit": …}
//	2026-08-22T05:20:19.871Z  INFO  Attempting to acquire leader lease...  {"lock": …}
//	2026-08-22T05:20:18.566Z  INFO  setup  Registering Components.
//
// The trailing JSON object is optional too. Rather than guess from the field count, the
// last field is taken as the fields object when it looks like one, and what is left is
// "logger, message" or just "message".
var lsOpHeader = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T[\d:.]+(?:Z|[+-]\d{2}:\d{2}))\t([A-Z]+)\t(.*)$`)

// lsPITRHeader matches the binlog collector's lines. Go's standard logger, so: a date with
// slashes, a time, and the message. No level, no zone, no subsecond.
//
// The absence of a zone is worth stating rather than working around. The collector runs in
// a container whose TZ is UTC and beside an operator that writes an explicit Z, so UTC is
// not a guess here — but a bundle that mixed this log with one from a machine on local
// time would need the source's manual offset, which is exactly what it is for.
var lsPITRHeader = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) (.*)$`)

// lsOpField is one key/value from the trailing JSON object, in the order it was written.
type lsOpField struct {
	Key string
	Val string // scalars as written; objects and arrays re-encoded compactly
}

// lsOpFields is a record's field object, in order and with duplicates intact.
type lsOpFields []lsOpField

// first returns the first value written under a key. For "name" and "namespace" that is
// the object being reconciled, which is the one a reader means.
func (f lsOpFields) first(key string) string {
	for _, kv := range f {
		if kv.Key == key {
			return kv.Val
		}
	}
	return ""
}

// last returns the final value written under a key — the message's own, when a reconcile
// record has restated one of the context keys for its own purposes.
func (f lsOpFields) last(key string) string {
	out := ""
	for _, kv := range f {
		if kv.Key == key {
			out = kv.Val
		}
	}
	return out
}

func (f lsOpFields) has(key string) bool {
	for _, kv := range f {
		if kv.Key == key {
			return true
		}
	}
	return false
}

// lsOpParseFields reads the trailing object without letting a later key overwrite an
// earlier one. encoding/json's map decoding does exactly that, and the operator writes
// "name" twice in most of its records — see the note at the top of this file.
func lsOpParseFields(s string) (lsOpFields, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var out lsOpFields
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return out, len(out) > 0
		}
		key, _ := kt.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return out, len(out) > 0
		}
		out = append(out, lsOpField{Key: key, Val: lsOpScalar(raw)})
	}
	return out, true
}

// lsOpScalar renders one field value for display: strings unquoted (so an `err` reads as
// a sentence), everything else as written.
func lsOpScalar(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// lsFoldOperator splits an operator log into records.
//
// Folding matters here for one reason and it is the same one as everywhere else in this
// package: an ERROR record is followed by the Go stack trace of the failure, eight to
// twelve lines of `sigs.k8s.io/controller-runtime/...` and tab-indented file positions.
// None of them is a header, so all of them belong to the record above — and a
// line-at-a-time reader would report one failed reconcile as nine events named after
// controller-runtime internals.
func lsFoldOperator(data string) []lsRecord {
	var out []lsRecord
	var cur *lsRecord
	lastTS := 0.0
	for i, raw := range strings.Split(data, "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			if cur != nil {
				cur.Body = append(cur.Body, "")
			}
			continue
		}
		m := lsOpHeader.FindStringSubmatch(line)
		if m == nil {
			if cur == nil {
				continue // leading junk: a log opened mid-stack-trace
			}
			cur.Body = append(cur.Body, line)
			continue
		}
		if cur != nil {
			out = append(out, *cur)
		}
		rec := lsRecord{Line: i + 1, Time: m[1], Level: m[2], Subsys: lsSubsysOperator}
		rest := strings.Split(m[3], "\t")
		// The last field is the JSON object when it parses as one; what remains is either
		// "logger, message" or a bare message.
		if len(rest) > 1 {
			if fields, ok := lsOpParseFields(rest[len(rest)-1]); ok {
				rec.Body = append(rec.Body, lsOpFieldLines(fields)...)
				rest = rest[:len(rest)-1]
			}
		}
		switch len(rest) {
		case 0:
		case 1:
			rec.Text = strings.TrimSpace(rest[0])
		default:
			// A logger name ("setup", "controller-runtime.webhook") is a subsystem in
			// everything but name, and the page already has a column for one.
			rec.Subsys = strings.TrimSpace(rest[0])
			rec.Text = strings.TrimSpace(strings.Join(rest[1:], " "))
		}
		if ts, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
			rec.TS = float64(ts.UnixNano()) / 1e9
			lastTS = rec.TS
		} else {
			rec.TS, rec.Approx = lastTS, lastTS > 0
		}
		cur = &rec
	}
	if cur != nil {
		out = append(out, *cur)
	}
	for i := range out {
		for len(out[i].Body) > 0 && out[i].Body[len(out[i].Body)-1] == "" {
			out[i].Body = out[i].Body[:len(out[i].Body)-1]
		}
	}
	return out
}

// lsSubsysOperator is the subsystem an operator record gets when it named no logger. Every
// record needs one so the UI's subsystem filter can separate the three logs of a cluster.
const lsSubsysOperator = "operator"

// lsSubsysPITR marks the binlog collector's records.
const lsSubsysPITR = "pitr"

// lsOpFieldLines renders the field object as the record's detail, one `key: value` per
// line and in the order written — including the duplicates, because a reader checking the
// classifier against the file has to see what the file says.
func lsOpFieldLines(f lsOpFields) []string {
	out := make([]string, 0, len(f))
	for _, kv := range f {
		out = append(out, kv.Key+": "+kv.Val)
	}
	return out
}

// lsFoldPITR splits the binlog collector's log into records.
//
// Its multi-line records are not stack traces but data the message refers to:
//
//	2026/08/22 06:00:33 Peer list updated
//	was []
//	now [cluster1-pxc-0.… cluster1-pxc-1.… cluster1-pxc-2.…]
//
// which is the collector telling you which members it can see — worthless as three
// separate events and the whole point as one.
func lsFoldPITR(data string) []lsRecord {
	var out []lsRecord
	var cur *lsRecord
	lastTS := 0.0
	for i, raw := range strings.Split(data, "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			if cur != nil {
				cur.Body = append(cur.Body, "")
			}
			continue
		}
		m := lsPITRHeader.FindStringSubmatch(line)
		if m == nil {
			if cur == nil {
				continue
			}
			cur.Body = append(cur.Body, line)
			continue
		}
		if cur != nil {
			out = append(out, *cur)
		}
		rec := lsRecord{Line: i + 1, Time: m[1], Level: "INFO", Subsys: lsSubsysPITR,
			Text: strings.TrimSpace(m[2])}
		// The collector re-emits the peer finder's own lines with their timestamp intact,
		// so `06:00:34 PXC Node: 2026/08/22 06:00:33 Peer finder enter` carries two stamps.
		// The outer one is when the collector wrote it, which is the one to place it by.
		if ts, err := time.ParseInLocation("2006/01/02 15:04:05", m[1], time.UTC); err == nil {
			rec.TS = float64(ts.UnixNano()) / 1e9
			lastTS = rec.TS
		} else {
			rec.TS, rec.Approx = lastTS, lastTS > 0
		}
		// The collector spells its own warnings; there is no level column to read.
		if strings.HasPrefix(rec.Text, "WARNING:") {
			rec.Level = "WARNING"
		}
		if strings.HasPrefix(rec.Text, "ERROR:") {
			rec.Level = "ERROR"
		}
		cur = &rec
	}
	if cur != nil {
		out = append(out, *cur)
	}
	for i := range out {
		for len(out[i].Body) > 0 && out[i].Body[len(out[i].Body)-1] == "" {
			out[i].Body = out[i].Body[:len(out[i].Body)-1]
		}
	}
	return out
}

// ---------------------------------------------------------------- sniffing

// lsSniffOperator reports whether this is a controller log this file can read, and which.
//
// The operator's own giveaway is `controllerKind` / `controllerGroup: pxc.percona.com`,
// which every reconcile record carries and nothing else writes. Falling back to the header
// shape alone would claim every controller-runtime log in Kubernetes, and this catalogue
// knows nothing about any of them.
func lsSniffOperator(data string) string {
	head := data
	if len(head) > 256<<10 {
		head = head[:256<<10]
	}
	// The MongoDB operator and its agents first: they are read by
	// logsummary_psmdbop.go, and the operator's lines are shaped exactly like this one's.
	if f := lsSniffPSMDB(head); f != "" {
		return f
	}
	// The three PostgreSQL operators, for the same reason — and one of them writes the
	// identical zap format this file's fold reads.
	if f := lsSniffPGOperator(head); f != "" {
		return f
	}
	if strings.Contains(head, "pxc.percona.com") ||
		strings.Contains(head, "PerconaXtraDBCluster") ||
		strings.Contains(head, "percona-xtradb-cluster-operator") {
		return lsFlavourPXCOperator
	}
	if lsPITRSniff(head) {
		return lsFlavourPXCPITR
	}
	return ""
}

// lsPITRSniff recognises the binlog collector. Its lines have no level and no zone, so the
// header shape alone is `log.Print` and could be anything; the discriminator is the
// vocabulary, which is narrow and unmistakable.
func lsPITRSniff(head string) bool {
	if !lsPITRHeader.MatchString(head) && !strings.Contains(head, "\n20") {
		// Cheap reject for data that has no such line anywhere near the top.
		if !lsPITRHeaderAnywhere(head) {
			return false
		}
	}
	for _, s := range []string{
		"running binlog collector", "initializing collector", "reading binlogs from pxc",
		"last uploaded binlog", "successfully wrote binlog file", "binlog collector",
	} {
		if strings.Contains(head, s) {
			return true
		}
	}
	return false
}

func lsPITRHeaderAnywhere(head string) bool {
	for _, line := range strings.Split(head, "\n") {
		if lsPITRHeader.MatchString(line) {
			return true
		}
	}
	return false
}

// lsOpNodeName is what to call this source when the log does not come from a named node.
//
// The operator log names the cluster it reconciles rather than itself, which is the more
// useful of the two: a bundle holding two clusters' operators wants them told apart by
// what they manage. The cluster's name is the FIRST `name` in a reconcile record's
// fields — see the duplicate-key note at the top of this file.
func lsOpNodeName(recs []lsRecord) string {
	for _, r := range recs {
		for _, ln := range r.Body {
			if v, ok := strings.CutPrefix(ln, "PerconaXtraDBCluster: "); ok {
				var obj struct{ Name string }
				if json.Unmarshal([]byte(v), &obj) == nil && obj.Name != "" {
					return "operator/" + obj.Name
				}
			}
		}
	}
	return ""
}

// lsPITRNodeName names the collector after the cluster it reads, which it prints on every
// start: `reading binlogs from pxc with hostname= cluster1-pxc-0.cluster1-pxc.pxc.svc…`.
func lsPITRNodeName(recs []lsRecord) string {
	for _, r := range recs {
		if _, host, ok := strings.Cut(r.Text, "with hostname= "); ok {
			if name, _, ok := strings.Cut(strings.TrimSpace(host), "."); ok && name != "" {
				// cluster1-pxc-0 → cluster1
				if base, _, ok := strings.Cut(name, "-pxc-"); ok && base != "" {
					return "pitr/" + base
				}
				return "pitr/" + name
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------- extractors

var (
	lsOpBackupDest    = regexp.MustCompile(`^(s3|azure|gs)://\S+`)
	lsPITRBinlogWrote = regexp.MustCompile(`^successfully wrote binlog file (\S+) to storage with name (\S+)`)
	lsPITRBinlogRead  = regexp.MustCompile(`^binlog\.(\d+) \((\d+) bytes\)`)
)

// lsOpFieldsOf re-reads a record's detail back into ordered fields. The fold already
// flattened them to `key: value` lines so that "show this in the file" and the detail pane
// agree with each other; rules want them structured again.
func lsOpFieldsOf(r lsRecord) lsOpFields {
	out := make(lsOpFields, 0, len(r.Body))
	for _, ln := range r.Body {
		k, v, ok := strings.Cut(ln, ": ")
		if !ok {
			continue
		}
		out = append(out, lsOpField{Key: k, Val: v})
	}
	return out
}

// lsOpErrText is the failure a record carries, whichever of the three spellings it used.
// `error` is controller-runtime's, `err` is the operator's own, and `errorVerbose` is the
// same failure with the call stack appended — the short one is the sentence.
func lsOpErrText(f lsOpFields) string {
	for _, k := range []string{"error", "err"} {
		if v := f.last(k); v != "" {
			return v
		}
	}
	return ""
}

// lsOpTrunc keeps a label to one line. An operator error can be a paragraph.
func lsOpTrunc(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// lsOpDur formats a span the way the findings read it.
func lsOpDur(sec float64) string {
	switch {
	case sec <= 0:
		return "0s"
	case sec < 60:
		return fmt.Sprintf("%.1fs", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm %ds", int(sec)/60, int(sec)%60)
	default:
		return fmt.Sprintf("%dh %dm", int(sec)/3600, (int(sec)%3600)/60)
	}
}
