package main

// logsummary_pgop.go — the three PostgreSQL operators, which agree on almost nothing.
//
// MySQL and MongoDB each had one operator writing one format. PostgreSQL has three
// operators writing three DIFFERENT formats, and the member logs differ too:
//
//	Percona Operator for PostgreSQL   zap, tab-separated — the same shape the PXC and
//	                                  PSMDB operators write, so lsFoldOperator reads it
//	                                  unchanged. Controller group pgv2.percona.com.
//	Crunchy PGO                       logfmt: time="…" level=debug msg="…" key=value.
//	                                  A different library entirely (logrus), even though
//	                                  Percona's operator is a FORK of this one.
//	CloudNativePG                     JSON, one object per line.
//
// The members split the same way and it is the more important half:
//
//   - Percona and Crunchy both run **Patroni** in the `database` container, and Patroni's
//     stdout is what `kubectl logs` returns. PostgreSQL's own log is a file on the volume
//     (`/pgdata/<version>/log/`). Both are already read by logsummary_postgres.go — the
//     Patroni catalogue this package has had since the Patroni frames were added works on
//     an operator-managed member without a line of new code.
//   - CloudNativePG runs **no Patroni**. Its instance manager is the failover manager, and
//     the member's log is one JSON stream carrying BOTH the instance manager's own records
//     and PostgreSQL's, the latter wrapped as `{"logger":"postgres","msg":"record",
//     "record":{…the CSV log fields…}}`. So a CNPG member log is two documents in one and
//     has to be split before either can be read.
//
// What the corpus settled, and it is the reason to read the member logs rather than the
// operator's: **the two Patroni-based operators say nothing at all about a failover.**
// Killing the leader of each of the three at the same moment produced, in the operators'
// logs over the following minute:
//
//	Percona   7 × "v1 Endpoints is deprecated in v1.33+"   — and nothing else
//	Crunchy   14 × "reconciled instance", 7 × "reconciled cluster"
//	CNPG      13 × "There is a switchover or a failover in progress, waiting…",
//	          "Old primary pod not found in managed instances, skipping label demotion",
//	          "Setting primary label", "Setting replica label"
//
// CNPG narrates it because CNPG *is* the failover manager. The other two delegate to
// Patroni and never mention that anything happened — the whole story is in the member's
// `database` container, in records this package already classifies.

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

const (
	lsFlavourPerconaPG   = "perconapgoperator" // percona-postgresql-operator (zap)
	lsFlavourCrunchyPGO  = "crunchypgo"        // Crunchy postgres-operator (logfmt)
	lsFlavourCNPG        = "cnpgoperator"      // cloudnative-pg manager (JSON)
	lsFlavourCNPGManager = "cnpginstance"      // a CNPG member's instance manager (JSON)
)

// The state track these sources get. The two Patroni-based operators have nothing of their
// own to say about the cluster, so their lanes carry only whether the controller was up;
// CNPG has a real one, because it runs the failover.
const (
	lsStateCNPGSwitch = "SWITCHOVER" // CNPG is moving the primary and says so
	lsStateCNPGHealth = "MANAGING"   // the instance manager is up and reconciling its member
)

var lsPGOpStateMeaning = map[string]string{
	lsStateCNPGSwitch: "CloudNativePG is moving the primary — it runs the failover itself, so this is the one operator whose log dates a switchover",
	lsStateCNPGHealth: "the instance manager is up and looking after its member",
}

// ---------------------------------------------------------------- logfmt (Crunchy)

// lsLogfmtHeader matches a Crunchy PGO line. logrus's logfmt output leads with the
// timestamp and level, and everything after is `key=value` with quoted values where they
// contain spaces:
//
//	time="2026-08-22T11:15:23Z" level=debug msg="reconciled instance" PostgresCluster=pgo/crunchy …
var lsLogfmtHeader = regexp.MustCompile(`^time="([^"]+)"\s+level=(\w+)\s+(.*)$`)

// lsParseLogfmt splits the `key=value` tail, honouring quotes. It is deliberately its own
// small parser rather than a dependency: the format is three rules wide, and this package
// has hand-parsed every other format it reads.
func lsParseLogfmt(s string) lsOpFields {
	var out lsOpFields
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		j := strings.IndexByte(s[i:], '=')
		if j < 0 {
			break
		}
		key := s[i : i+j]
		i += j + 1
		var val string
		if i < len(s) && s[i] == '"' {
			i++
			var b strings.Builder
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			i++ // closing quote
			val = b.String()
		} else {
			k := strings.IndexByte(s[i:], ' ')
			if k < 0 {
				val, i = s[i:], len(s)
			} else {
				val, i = s[i:i+k], i+k
			}
		}
		if key != "" {
			out = append(out, lsOpField{Key: key, Val: val})
		}
	}
	return out
}

// lsFoldCrunchy splits a Crunchy PGO log into records.
func lsFoldCrunchy(data string) []lsRecord {
	var out []lsRecord
	var cur *lsRecord
	last := 0.0
	for i, raw := range strings.Split(data, "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			if cur != nil {
				cur.Body = append(cur.Body, "")
			}
			continue
		}
		m := lsLogfmtHeader.FindStringSubmatch(line)
		if m == nil {
			if cur != nil {
				cur.Body = append(cur.Body, line)
			}
			continue
		}
		if cur != nil {
			out = append(out, *cur)
		}
		f := lsParseLogfmt(m[3])
		rec := lsRecord{Line: i + 1, Time: m[1], Level: strings.ToUpper(m[2]),
			Subsys: lsSubsysPGO, Text: f.last("msg"), Body: lsOpFieldLines(f)}
		if c := f.last("controller"); c != "" {
			rec.Subsys = c
		}
		if ts, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
			rec.TS = float64(ts.UnixNano()) / 1e9
			last = rec.TS
		} else {
			rec.TS, rec.Approx = last, last > 0
		}
		cur = &rec
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return lsTrimBodies(out)
}

const (
	lsSubsysPGO      = "pgo"
	lsSubsysCNPG     = "cnpg"
	lsSubsysInstance = "instance-manager"
)

// ---------------------------------------------------------------- JSON (CloudNativePG)

// lsFoldCNPG splits a CloudNativePG log — the operator's or a member's — into records.
//
// One JSON object per line, so folding is not about continuations here; it is about
// SPLITTING. A member's stream carries the instance manager's own records and PostgreSQL's
// side by side, and the PostgreSQL ones are not text at all but the CSV log's fields as an
// object:
//
//	{"level":"info","logger":"postgres","msg":"record","logging_pod":"cnpgc-1",
//	 "record":{"log_time":"…","error_severity":"LOG","message":"redo starts at 0/…"}}
//
// Those are re-emitted as ordinary PostgreSQL records — timestamp, level, message — so the
// PostgreSQL catalogue reads a CNPG member exactly as it reads any other server. Without
// that a CNPG member's log is a wall of `msg: record` saying nothing.
func lsFoldCNPG(data string) []lsRecord {
	var out []lsRecord
	for i, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] != '{' {
			continue
		}
		var e struct {
			Level  string          `json:"level"`
			TS     string          `json:"ts"`
			Logger string          `json:"logger"`
			Msg    string          `json:"msg"`
			Pod    string          `json:"logging_pod"`
			Record json.RawMessage `json:"record"`
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.Logger == "postgres" && len(e.Record) > 0 {
			if r, ok := lsCNPGPostgresRecord(e.Record, i+1); ok {
				out = append(out, r)
				continue
			}
		}
		rec := lsRecord{
			Line: i + 1, Time: e.TS, Level: strings.ToUpper(e.Level),
			Subsys: e.Logger, Text: e.Msg, Thread: e.Pod,
		}
		if rec.Subsys == "" {
			rec.Subsys = lsSubsysCNPG
		}
		if f, ok := lsOpParseFields(line); ok {
			rec.Body = lsOpFieldLines(lsCNPGDetail(f))
		}
		if ts, err := time.Parse(time.RFC3339Nano, e.TS); err == nil {
			rec.TS = float64(ts.UnixNano()) / 1e9
		}
		out = append(out, rec)
	}
	return out
}

// lsCNPGDetail drops the keys that are on the record already, so the detail pane is the
// facts rather than a restatement of the header.
func lsCNPGDetail(f lsOpFields) lsOpFields {
	out := make(lsOpFields, 0, len(f))
	for _, kv := range f {
		switch kv.Key {
		case "level", "ts", "logger", "msg", "logging_pod":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// lsCNPGPostgresRecord turns CNPG's wrapped PostgreSQL record back into one.
func lsCNPGPostgresRecord(raw json.RawMessage, line int) (lsRecord, bool) {
	var r struct {
		LogTime  string `json:"log_time"`
		Severity string `json:"error_severity"`
		Message  string `json:"message"`
		Detail   string `json:"detail"`
		Hint     string `json:"hint"`
		State    string `json:"state_code"`
		User     string `json:"user_name"`
		DB       string `json:"database_name"`
		PID      string `json:"process_id"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Message == "" {
		return lsRecord{}, false
	}
	rec := lsRecord{
		Line: line, Time: r.LogTime, Level: r.Severity, Code: r.State,
		Subsys: lsSubsysPostgres, Thread: r.PID, Text: r.Message,
	}
	for _, extra := range []struct{ k, v string }{
		{"DETAIL", r.Detail}, {"HINT", r.Hint}, {"user", r.User}, {"database", r.DB},
	} {
		if extra.v != "" {
			rec.Body = append(rec.Body, extra.k+": "+extra.v)
		}
	}
	// PostgreSQL's CSV stamp is `2026-08-22 11:16:54.106 UTC`.
	for _, layout := range []string{"2006-01-02 15:04:05.000 MST", "2006-01-02 15:04:05 MST", "2006-01-02 15:04:05.000-07"} {
		if ts, err := time.Parse(layout, r.LogTime); err == nil {
			rec.TS = float64(ts.UnixNano()) / 1e9
			break
		}
	}
	return rec, true
}

// lsTrimBodies drops trailing blank continuation lines, which every fold produces.
func lsTrimBodies(recs []lsRecord) []lsRecord {
	for i := range recs {
		for len(recs[i].Body) > 0 && recs[i].Body[len(recs[i].Body)-1] == "" {
			recs[i].Body = recs[i].Body[:len(recs[i].Body)-1]
		}
	}
	return recs
}

// ---------------------------------------------------------------- sniffing

// lsSniffPGOperator recognises the three PostgreSQL operators and a CNPG member.
//
// Order matters and the reason is worth stating: Percona's operator is a FORK of Crunchy's
// and drives Crunchy's own `PostgresCluster` custom resource, so a Percona operator's log
// is full of `postgres-operator.crunchydata.com`. Only the Percona-specific group
// (`pgv2.percona.com`) tells them apart, and it has to be checked first.
func lsSniffPGOperator(data string) string {
	head := data
	if len(head) > 256<<10 {
		head = head[:256<<10]
	}
	if strings.Contains(head, "pgv2.percona.com") || strings.Contains(head, "PerconaPGBackup") ||
		strings.Contains(head, "percona-postgresql-operator") {
		return lsFlavourPerconaPG
	}
	if lsLogfmtAnywhere(head) && strings.Contains(head, "postgres-operator.crunchydata.com") {
		return lsFlavourCrunchyPGO
	}
	// CloudNativePG: JSON with its own logger names. `logging_pod` only ever appears in an
	// instance manager's stream, which is what separates a member from the controller.
	if strings.Contains(head, `"logger":"instance-manager"`) || strings.Contains(head, `"logging_pod"`) {
		return lsFlavourCNPGManager
	}
	if strings.Contains(head, `"logger":"cluster-resource"`) || strings.Contains(head, `"logger":"setup"`) &&
		strings.Contains(head, "cnpg") {
		return lsFlavourCNPG
	}
	return ""
}

func lsLogfmtAnywhere(head string) bool {
	for i, line := range strings.Split(head, "\n") {
		if i > 200 {
			break
		}
		if lsLogfmtHeader.MatchString(line) {
			return true
		}
	}
	return false
}

// lsPGOpNodeName names an operator source after the cluster it reconciles.
func lsPGOpNodeName(recs []lsRecord, flavour string) string {
	for _, r := range recs {
		f := lsOpFieldsOf(r)
		switch flavour {
		case lsFlavourCrunchyPGO:
			// `PostgresCluster=pgo/crunchy`
			if v := f.last("PostgresCluster"); v != "" {
				if _, name, ok := strings.Cut(v, "/"); ok {
					return "operator/" + name
				}
				return "operator/" + v
			}
		case lsFlavourPerconaPG:
			for _, ln := range r.Body {
				if v, ok := strings.CutPrefix(ln, "PostgresCluster: "); ok {
					if n := lsOpObjName(v); n != "" {
						return "operator/" + n
					}
				}
				if v, ok := strings.CutPrefix(ln, "PerconaPGCluster: "); ok {
					if n := lsOpObjName(v); n != "" {
						return "operator/" + n
					}
				}
			}
		case lsFlavourCNPGManager:
			// A member's own stream stamps every record with the pod it came from.
			if v := f.last("logging_pod"); v != "" {
				return v
			}
		case lsFlavourCNPG:
			// The controller reconciles many clusters, so it is named after the one it
			// talks about: `cluster_name` where the record carries it, and otherwise the
			// bare `name` that the webhook's defaulting records use.
			for _, k := range []string{"cluster_name", "name"} {
				if v := f.last(k); v != "" {
					return "operator/" + v
				}
			}
		}
	}
	return ""
}
