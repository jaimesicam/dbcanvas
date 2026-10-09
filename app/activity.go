package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// activity.go — what is running inside a database node right now, arranged around the questions
// a DBA asks of it rather than as a processlist: who blocks whom (row locks and metadata locks,
// as a tree), which transactions are open and how much they hold, which DDL is running and what
// queues behind it, the latest deadlock, and which statements cost the most.
//
// One snapshot per request, built from the engine's own catalogues in a single exec. Nothing runs
// unless somebody has the Activity panel open — and the expensive parts (the InnoDB status the
// deadlock comes from, statement digests, per-session resources) only when the panel asks for
// them (?with=). Lock queries read the lock *waits*, never the whole lock table, which can hold
// millions of rows for one big transaction.
//
// Per-session CPU and I/O come from the OS — /proc of the server's thread (MySQL's THREAD_OS_ID,
// MariaDB's TID) or backend process (PostgreSQL) — which costs nothing inside the server. The
// instrumentation that does cost (deep mode, activitydeep.go) is opt-in and on a timer.

type actSession struct {
	ID        string   `json:"id"`
	User      string   `json:"user,omitempty"`
	Host      string   `json:"host,omitempty"`
	DB        string   `json:"db,omitempty"`
	Command   string   `json:"command,omitempty"` // Query / Sleep / … ; PostgreSQL's state; MongoDB's op
	State     string   `json:"state,omitempty"`   // thread state, wait event, MongoDB msg
	TimeSec   float64  `json:"timeSec"`
	Statement string   `json:"statement,omitempty"`
	App       string   `json:"app,omitempty"`
	TrxAge    *float64 `json:"trxAgeSec,omitempty"`
	TrxLocked *int64   `json:"trxRowsLocked,omitempty"`
	TrxMod    *int64   `json:"trxRowsModified,omitempty"`
	TrxState  string   `json:"trxState,omitempty"`
	Waiting   bool     `json:"waiting,omitempty"`
	BlockedBy []string `json:"blockedBy,omitempty"`
	History   []string `json:"history,omitempty"` // what an idle transaction ran before it went idle
	OSID      int64    `json:"osId,omitempty"`    // OS thread (MySQL) or process (PostgreSQL)
	CPUTicks  *int64   `json:"cpuTicks,omitempty"`
	ReadB     *int64   `json:"readBytes,omitempty"`
	WriteB    *int64   `json:"writeBytes,omitempty"`
	MemBytes  *int64   `json:"memBytes,omitempty"`
	NetSent   *int64   `json:"netSent,omitempty"`
	NetRecv   *int64   `json:"netRecv,omitempty"`
	Killable  bool     `json:"killable"`
	// Backend is the database connection a proxy session is on (ProxySQL), resolved to its node.
	Backend *actBackend `json:"backend,omitempty"`
}

type actWait struct {
	Waiter      string   `json:"waiter"`
	Blocker     string   `json:"blocker,omitempty"`
	Kind        string   `json:"kind"` // row | table | metadata | lock
	Mode        string   `json:"mode,omitempty"`
	BlockerMode string   `json:"blockerMode,omitempty"`
	Object      string   `json:"object,omitempty"`
	Index       string   `json:"index,omitempty"`
	Data        string   `json:"data,omitempty"`
	WaitSec     *float64 `json:"waitSec,omitempty"`
}

type actDDL struct {
	Session   string   `json:"session"`
	Statement string   `json:"statement"`
	Phase     string   `json:"phase,omitempty"`
	Done      *float64 `json:"done,omitempty"`
	Total     *float64 `json:"total,omitempty"`
	TimeSec   float64  `json:"timeSec"`
	Queue     []string `json:"queue,omitempty"` // sessions waiting on it
	Tool      string   `json:"tool,omitempty"`  // pt-osc / gh-ost when one drives it
}

type actDigest struct {
	ID      string  `json:"id"`
	Schema  string  `json:"schema,omitempty"`
	Text    string  `json:"text"`
	Calls   float64 `json:"calls"`
	TimeSec float64 `json:"timeSec"`
	RowsExa float64 `json:"rowsExamined"`
	RowsOut float64 `json:"rowsSent"`
	NoIndex float64 `json:"noIndex"`
}

type actSnapshot struct {
	Engine     string        `json:"engine"`
	At         int64         `json:"atMs"`
	Sessions   []*actSession `json:"sessions"`
	Waits      []actWait     `json:"waits"`
	DDL        []actDDL      `json:"ddl"`
	Digests    []actDigest   `json:"digests,omitempty"`
	Deadlock   *deadlockInfo `json:"deadlock,omitempty"`
	Deadlocks  *int64        `json:"deadlockCount,omitempty"` // PostgreSQL's counter
	ClockTicks int           `json:"clockTicks,omitempty"`
	Deep       *deepState    `json:"deep,omitempty"`
	Proxy      *actProxy     `json:"proxy,omitempty"`
	Notes      []string      `json:"notes,omitempty"`
	CostMs     int64         `json:"costMs"`
}

// ------------------------------------------------------------------------------------ MySQL

// activityMySQLSQL is the MySQL-family batch: every statement answers one row
// {"k": <section>, "v": <json>}. --force skips what a flavour does not have (performance_schema
// on MariaDB with it off, data_lock_waits on 5.7, INNODB_LOCK_WAITS on 8.0); each section has a
// fallback that the parser uses only when the preferred one is missing.
const activityMySQLSQL = `SELECT JSON_OBJECT('k','sessions','v', JSON_ARRAYAGG(JSON_OBJECT(
  'id', t.PROCESSLIST_ID, 'user', t.PROCESSLIST_USER, 'host', IFNULL(pl.HOST, t.PROCESSLIST_HOST), 'db', t.PROCESSLIST_DB,
  'cmd', t.PROCESSLIST_COMMAND, 'state', t.PROCESSLIST_STATE, 'time', t.PROCESSLIST_TIME, 'info', LEFT(t.PROCESSLIST_INFO, 4000),
  'os', t.THREAD_OS_ID, 'trxAge', TIMESTAMPDIFF(SECOND, x.trx_started, NOW()), 'locked', x.trx_rows_locked,
  'mod', x.trx_rows_modified, 'trxState', x.trx_state)))
  FROM performance_schema.threads t LEFT JOIN information_schema.INNODB_TRX x ON x.trx_mysql_thread_id = t.PROCESSLIST_ID
  LEFT JOIN performance_schema.processlist pl ON pl.ID = t.PROCESSLIST_ID
  WHERE t.TYPE = 'FOREGROUND' AND t.PROCESSLIST_ID IS NOT NULL AND t.PROCESSLIST_ID <> CONNECTION_ID()
  AND t.PROCESSLIST_COMMAND NOT IN ('Daemon', 'Binlog Dump', 'Binlog Dump GTID');
SELECT JSON_OBJECT('k','sessions_is','v', JSON_ARRAYAGG(JSON_OBJECT(
  'id', p.ID, 'user', p.USER, 'host', p.HOST, 'db', p.DB, 'cmd', p.COMMAND, 'state', p.STATE, 'time', p.TIME,
  'info', LEFT(p.INFO, 4000), 'os', p.TID, 'trxAge', TIMESTAMPDIFF(SECOND, x.trx_started, NOW()),
  'locked', x.trx_rows_locked, 'mod', x.trx_rows_modified, 'trxState', x.trx_state)))
  FROM information_schema.PROCESSLIST p LEFT JOIN information_schema.INNODB_TRX x ON x.trx_mysql_thread_id = p.ID
  WHERE p.ID <> CONNECTION_ID() AND p.COMMAND NOT IN ('Daemon', 'Binlog Dump', 'Binlog Dump GTID', 'Slave_IO', 'Slave_SQL', 'Slave_worker');
SELECT JSON_OBJECT('k','waits','v', JSON_ARRAYAGG(JSON_OBJECT(
  'waiter', rt.PROCESSLIST_ID, 'blocker', bt.PROCESSLIST_ID, 'kind', LOWER(rl.LOCK_TYPE), 'mode', rl.LOCK_MODE,
  'bmode', bl.LOCK_MODE, 'object', CONCAT_WS('.', rl.OBJECT_SCHEMA, rl.OBJECT_NAME), 'index', rl.INDEX_NAME,
  'data', LEFT(rl.LOCK_DATA, 200), 'wait', TIMESTAMPDIFF(SECOND, rx.trx_wait_started, NOW()))))
  FROM performance_schema.data_lock_waits w
  JOIN performance_schema.data_locks rl ON rl.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID AND rl.ENGINE = w.ENGINE
  JOIN performance_schema.data_locks bl ON bl.ENGINE_LOCK_ID = w.BLOCKING_ENGINE_LOCK_ID AND bl.ENGINE = w.ENGINE
  JOIN performance_schema.threads rt ON rt.THREAD_ID = w.REQUESTING_THREAD_ID
  JOIN performance_schema.threads bt ON bt.THREAD_ID = w.BLOCKING_THREAD_ID
  LEFT JOIN information_schema.INNODB_TRX rx ON rx.trx_mysql_thread_id = rt.PROCESSLIST_ID;
SELECT JSON_OBJECT('k','waits_is','v', JSON_ARRAYAGG(JSON_OBJECT(
  'waiter', r.trx_mysql_thread_id, 'blocker', b.trx_mysql_thread_id, 'kind', LOWER(l.lock_type), 'mode', l.lock_mode,
  'object', l.lock_table, 'index', l.lock_index, 'data', LEFT(l.lock_data, 200),
  'wait', TIMESTAMPDIFF(SECOND, r.trx_wait_started, NOW()))))
  FROM information_schema.INNODB_LOCK_WAITS w
  JOIN information_schema.INNODB_TRX r ON r.trx_id = w.requesting_trx_id
  JOIN information_schema.INNODB_TRX b ON b.trx_id = w.blocking_trx_id
  LEFT JOIN information_schema.INNODB_LOCKS l ON l.lock_id = w.requested_lock_id;
SELECT JSON_OBJECT('k','mdl','v', JSON_ARRAYAGG(JSON_OBJECT(
  't', t.PROCESSLIST_ID, 'obj', CONCAT_WS('.', m.OBJECT_SCHEMA, m.OBJECT_NAME), 'type', m.LOCK_TYPE, 'status', m.LOCK_STATUS)))
  FROM performance_schema.metadata_locks m JOIN performance_schema.threads t ON t.THREAD_ID = m.OWNER_THREAD_ID
  WHERE m.OBJECT_TYPE = 'TABLE' AND t.PROCESSLIST_ID IS NOT NULL AND (m.OBJECT_SCHEMA, m.OBJECT_NAME) IN
  (SELECT OBJECT_SCHEMA, OBJECT_NAME FROM performance_schema.metadata_locks WHERE LOCK_STATUS = 'PENDING' AND OBJECT_TYPE = 'TABLE');
SELECT JSON_OBJECT('k','history','v', JSON_ARRAYAGG(JSON_OBJECT('t', t.PROCESSLIST_ID, 'sql', LEFT(h.SQL_TEXT, 600), 'seq', h.EVENT_ID)))
  FROM performance_schema.events_statements_history h JOIN performance_schema.threads t ON t.THREAD_ID = h.THREAD_ID
  JOIN information_schema.INNODB_TRX x ON x.trx_mysql_thread_id = t.PROCESSLIST_ID
  WHERE t.PROCESSLIST_COMMAND = 'Sleep' AND h.SQL_TEXT IS NOT NULL;
SELECT JSON_OBJECT('k','stages','v', JSON_ARRAYAGG(JSON_OBJECT('t', t.PROCESSLIST_ID, 'stage', s.EVENT_NAME,
  'done', s.WORK_COMPLETED, 'total', s.WORK_ESTIMATED)))
  FROM performance_schema.events_stages_current s JOIN performance_schema.threads t ON t.THREAD_ID = s.THREAD_ID;
`

// activityMySQLExtra are the sections a panel asks for only when it shows them.
const activityMySQLDigests = `SELECT JSON_OBJECT('k','digests','v', JSON_ARRAYAGG(JSON_OBJECT('id', d.DIGEST, 'schema', d.SCHEMA_NAME,
  'text', LEFT(d.DIGEST_TEXT, 1500), 'calls', d.COUNT_STAR, 'time', d.SUM_TIMER_WAIT / 1e12, 'exa', d.SUM_ROWS_EXAMINED,
  'sent', d.SUM_ROWS_SENT, 'noidx', d.SUM_NO_INDEX_USED + d.SUM_NO_GOOD_INDEX_USED)))
  FROM (SELECT * FROM performance_schema.events_statements_summary_by_digest WHERE DIGEST IS NOT NULL
        ORDER BY SUM_TIMER_WAIT DESC LIMIT 40) d;
`

const activityMySQLResources = `SELECT JSON_OBJECT('k','mem','v', JSON_ARRAYAGG(JSON_OBJECT('t', x.id, 'b', x.b)))
  FROM (SELECT t.PROCESSLIST_ID id, SUM(m.CURRENT_NUMBER_OF_BYTES_USED) b
        FROM performance_schema.memory_summary_by_thread_by_event_name m JOIN performance_schema.threads t ON t.THREAD_ID = m.THREAD_ID
        WHERE t.TYPE = 'FOREGROUND' AND t.PROCESSLIST_ID IS NOT NULL GROUP BY t.PROCESSLIST_ID) x;
SELECT JSON_OBJECT('k','net','v', JSON_ARRAYAGG(JSON_OBJECT('t', x.id, 's', x.s, 'r', x.r)))
  FROM (SELECT t.PROCESSLIST_ID id, SUM(IF(s.VARIABLE_NAME = 'Bytes_sent', s.VARIABLE_VALUE, 0)) s,
               SUM(IF(s.VARIABLE_NAME = 'Bytes_received', s.VARIABLE_VALUE, 0)) r
        FROM performance_schema.status_by_thread s JOIN performance_schema.threads t ON t.THREAD_ID = s.THREAD_ID
        WHERE s.VARIABLE_NAME IN ('Bytes_sent', 'Bytes_received') AND t.PROCESSLIST_ID IS NOT NULL GROUP BY t.PROCESSLIST_ID) x;
`

type jnum = json.Number

func numP(n jnum) *float64 {
	if n == "" {
		return nil
	}
	f, err := n.Float64()
	if err != nil {
		return nil
	}
	return &f
}

func intP(n jnum) *int64 {
	if n == "" {
		return nil
	}
	if i, err := n.Int64(); err == nil {
		return &i
	}
	if f, err := n.Float64(); err == nil {
		i := int64(f)
		return &i
	}
	return nil
}

// sectionsOf reads the {"k","v"} rows of a batch.
func sectionsOf(out string) map[string]json.RawMessage {
	secs := map[string]json.RawMessage{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var row struct {
			K string          `json:"k"`
			V json.RawMessage `json:"v"`
		}
		if json.Unmarshal([]byte(line), &row) == nil && row.K != "" && len(row.V) > 0 && string(row.V) != "null" {
			if _, seen := secs[row.K]; !seen {
				secs[row.K] = row.V
			}
		}
	}
	return secs
}

func decodeNum(raw json.RawMessage, out any) {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	d.Decode(out)
}

// parseMySQLActivity builds the snapshot from the batch's sections.
func parseMySQLActivity(out string) *actSnapshot {
	secs := sectionsOf(out)
	s := &actSnapshot{Engine: "mysql", Sessions: []*actSession{}, Waits: []actWait{}, DDL: []actDDL{}}
	var rows []struct {
		ID, Time, OS, TrxAge, Locked, Mod jnum
		User, Host, DB, Cmd, State, Info  string
		TrxState                          string `json:"trxState"`
	}
	raw, ok := secs["sessions"]
	if !ok {
		raw = secs["sessions_is"]
		s.Notes = append(s.Notes, "performance_schema is off: sessions from the processlist, without metadata locks or statement history")
	}
	decodeNum(raw, &rows)
	byID := map[string]*actSession{}
	for _, r := range rows {
		x := &actSession{ID: r.ID.String(), User: r.User, Host: r.Host, DB: r.DB, Command: r.Cmd, State: r.State,
			Statement: r.Info, TrxState: r.TrxState, Killable: true}
		if f := numP(r.Time); f != nil {
			x.TimeSec = *f
		}
		if i := intP(r.OS); i != nil {
			x.OSID = *i
		}
		x.TrxAge, x.TrxLocked, x.TrxMod = numP(r.TrxAge), intP(r.Locked), intP(r.Mod)
		s.Sessions = append(s.Sessions, x)
		byID[x.ID] = x
	}

	var waits []struct {
		Waiter, Blocker, Wait            jnum
		Kind, Mode, Bmode, Object, Index string
		Data                             string
	}
	if raw, ok := secs["waits"]; ok {
		decodeNum(raw, &waits)
	} else if raw, ok := secs["waits_is"]; ok {
		decodeNum(raw, &waits)
	}
	for _, w := range waits {
		kind := w.Kind
		if kind == "record" {
			kind = "row"
		}
		s.Waits = append(s.Waits, actWait{Waiter: w.Waiter.String(), Blocker: w.Blocker.String(), Kind: kind, Mode: w.Mode,
			BlockerMode: w.Bmode, Object: strings.ReplaceAll(w.Object, "`", ""), Index: w.Index, Data: w.Data, WaitSec: numP(w.Wait)})
	}

	// Metadata locks: who holds what each pending request needs.
	var mdl []struct {
		T                 jnum
		Obj, Type, Status string
	}
	if raw, ok := secs["mdl"]; ok {
		decodeNum(raw, &mdl)
	}
	s.Waits = append(s.Waits, mdlWaits(mdl, byID)...)

	// What an idle open transaction ran before it went idle — usually the answer to "why is it
	// holding that lock".
	var hist []struct {
		T, Seq jnum
		SQL    string
	}
	if raw, ok := secs["history"]; ok {
		decodeNum(raw, &hist)
	}
	sort.Slice(hist, func(i, j int) bool {
		a, _ := hist[i].Seq.Int64()
		b, _ := hist[j].Seq.Int64()
		return a < b
	})
	for _, h := range hist {
		if x := byID[h.T.String()]; x != nil && strings.TrimSpace(h.SQL) != "" {
			x.History = append(x.History, h.SQL)
			if len(x.History) > 8 {
				x.History = x.History[1:]
			}
		}
	}

	markWaiting(s)

	// DDL: from the statements running, with progress where stage events are collected.
	var stages []struct {
		T, Done, Total jnum
		Stage          string
	}
	if raw, ok := secs["stages"]; ok {
		decodeNum(raw, &stages)
	}
	stageOf := map[string]int{}
	for i, st := range stages {
		stageOf[st.T.String()] = i
	}
	for _, x := range s.Sessions {
		if !isDDL(x.Statement) {
			continue
		}
		d := actDDL{Session: x.ID, Statement: x.Statement, TimeSec: x.TimeSec, Phase: x.State, Tool: ddlTool(x.Statement)}
		if i, ok := stageOf[x.ID]; ok {
			st := stages[i]
			d.Phase = strings.TrimPrefix(st.Stage, "stage/")
			d.Done, d.Total = numP(st.Done), numP(st.Total)
		}
		for _, w := range s.Waits {
			if w.Blocker == x.ID {
				d.Queue = append(d.Queue, w.Waiter)
			}
		}
		s.DDL = append(s.DDL, d)
	}

	var digests []struct {
		ID, Schema, Text              string
		Calls, Time, Exa, Sent, Noidx jnum
	}
	if raw, ok := secs["digests"]; ok {
		decodeNum(raw, &digests)
		for _, d := range digests {
			f := func(n jnum) float64 { v, _ := n.Float64(); return v }
			s.Digests = append(s.Digests, actDigest{ID: d.ID, Schema: d.Schema, Text: d.Text, Calls: f(d.Calls), TimeSec: f(d.Time),
				RowsExa: f(d.Exa), RowsOut: f(d.Sent), NoIndex: f(d.Noidx)})
		}
	}
	var mem []struct{ T, B jnum }
	if raw, ok := secs["mem"]; ok {
		decodeNum(raw, &mem)
		for _, m := range mem {
			if x := byID[m.T.String()]; x != nil {
				x.MemBytes = intP(m.B)
			}
		}
	}
	var net []struct{ T, S, R jnum }
	if raw, ok := secs["net"]; ok {
		decodeNum(raw, &net)
		for _, n := range net {
			if x := byID[n.T.String()]; x != nil {
				x.NetSent, x.NetRecv = intP(n.S), intP(n.R)
			}
		}
	}
	return s
}

// strongMDL are metadata lock types that conflict with the shared ones ordinary statements take.
var strongMDL = map[string]bool{"EXCLUSIVE": true, "SHARED_NO_READ_WRITE": true, "SHARED_NO_WRITE": true, "SHARED_UPGRADABLE": false}

// mdlWaits turns the metadata locks of contended tables into waits. A pending strong request
// (an ALTER) waits for every granted holder; a pending shared request (a SELECT) waits for a
// granted strong holder — or, when there is none, for the strong request queued ahead of it,
// which is the "Waiting for table metadata lock" pile-up behind an ALTER.
func mdlWaits(rows []struct {
	T                 jnum
	Obj, Type, Status string
}, byID map[string]*actSession) []actWait {
	type lk struct{ t, typ string }
	granted, pending := map[string][]lk{}, map[string][]lk{}
	for _, r := range rows {
		l := lk{r.T.String(), r.Type}
		if r.Status == "GRANTED" {
			granted[r.Obj] = append(granted[r.Obj], l)
		} else if r.Status == "PENDING" {
			pending[r.Obj] = append(pending[r.Obj], l)
		}
	}
	var out []actWait
	seen := map[string]bool{}
	add := func(w actWait) {
		k := w.Waiter + ">" + w.Blocker + w.Object
		if w.Waiter == w.Blocker || seen[k] {
			return
		}
		seen[k] = true
		if x := byID[w.Waiter]; x != nil {
			t := x.TimeSec
			w.WaitSec = &t
		}
		out = append(out, w)
	}
	for obj, ps := range pending {
		for _, p := range ps {
			found := false
			for _, g := range granted[obj] {
				if g.t == p.t {
					continue
				}
				if strongMDL[p.typ] || strongMDL[g.typ] {
					add(actWait{Waiter: p.t, Blocker: g.t, Kind: "metadata", Mode: p.typ, BlockerMode: g.typ, Object: obj})
					found = true
				}
			}
			if found || strongMDL[p.typ] {
				continue
			}
			for _, q := range ps {
				if q.t != p.t && strongMDL[q.typ] {
					add(actWait{Waiter: p.t, Blocker: q.t, Kind: "metadata", Mode: p.typ, BlockerMode: q.typ + " (queued)", Object: obj})
				}
			}
		}
	}
	return out
}

// markWaiting records on each session whom it waits for.
func markWaiting(s *actSnapshot) {
	by := map[string]*actSession{}
	for _, x := range s.Sessions {
		by[x.ID] = x
	}
	for _, w := range s.Waits {
		if x := by[w.Waiter]; x != nil {
			x.Waiting = true
			if w.Blocker != "" && !inList(x.BlockedBy, w.Blocker) {
				x.BlockedBy = append(x.BlockedBy, w.Blocker)
			}
		}
	}
}

func inList(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

var ddlRe = regexp.MustCompile(`(?is)^\s*(/\*.*?\*/\s*)*(ALTER\s+TABLE|CREATE\s+(UNIQUE\s+|FULLTEXT\s+|SPATIAL\s+)?INDEX|DROP\s+(TABLE|INDEX)|TRUNCATE|OPTIMIZE\s+TABLE|RENAME\s+TABLE|REINDEX|VACUUM|CLUSTER|CREATE\s+TABLE\s+\S+\s+AS)`)

func isDDL(sql string) bool { return ddlRe.MatchString(sql) }

func ddlTool(sql string) string {
	l := strings.ToLower(sql)
	switch {
	case strings.Contains(l, "_gho`") || strings.Contains(l, "_gho "):
		return "gh-ost"
	case strings.Contains(l, "pt_osc") || strings.Contains(l, "_new`"):
		return "pt-online-schema-change"
	}
	return ""
}

// ---------------------------------------------------------------------------- deadlocks

type deadlockTx struct {
	Number    int    `json:"n"`
	Thread    string `json:"thread,omitempty"`
	User      string `json:"user,omitempty"`
	Statement string `json:"statement,omitempty"`
	Holds     string `json:"holds,omitempty"`
	WaitsFor  string `json:"waitsFor,omitempty"`
	Victim    bool   `json:"victim"`
}

type deadlockInfo struct {
	Engine string       `json:"engine,omitempty"` // "postgres" names processes; InnoDB, transactions and threads
	At     string       `json:"at"`
	Txs    []deadlockTx `json:"txs"`
	Raw    string       `json:"raw"`
}

var (
	dlHeadRe   = regexp.MustCompile(`(?m)^(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`)
	dlTxRe     = regexp.MustCompile(`(?m)^\*\*\* \((\d+)\) TRANSACTION:`)
	dlThreadRe = regexp.MustCompile(`MySQL thread id (\d+), OS thread handle \S+, query id \d+ ?([^\n]*)`)
	dlIPRe     = regexp.MustCompile(`^[0-9a-fA-F.:]+$`)
	dlVictimRe = regexp.MustCompile(`\*\*\* WE ROLL BACK TRANSACTION \((\d+)\)`)
	dlLockRe   = regexp.MustCompile(`(RECORD|TABLE) LOCKS? .*?(index \S+ of )?table ` + "`" + `?([^` + "`" + `\s]+)` + "`" + `?\.?` + "`" + `?([^` + "`" + `\s]*)` + "`" + `?.*?(lock_mode \S+( locks rec but not gap| locks gap before rec| insert intention)?|lock mode \S+)`)
)

// deadlockUser reads "host [ip] user state…" from the thread line: the host, then the
// address when the server knows both, then the account, then the thread's state.
func deadlockUser(f []string) string {
	switch {
	case len(f) == 0:
		return ""
	case len(f) > 2 && dlIPRe.MatchString(f[1]):
		return f[2] + "@" + f[0]
	case len(f) > 1:
		return f[1] + "@" + f[0]
	}
	return f[0]
}

// parseDeadlock reads the LATEST DETECTED DEADLOCK section of SHOW ENGINE INNODB STATUS.
func parseDeadlock(status string) *deadlockInfo {
	i := strings.Index(status, "LATEST DETECTED DEADLOCK")
	if i < 0 {
		return nil
	}
	sec := status[i:]
	if j := strings.Index(sec, "\n------------\nTRANSACTIONS"); j > 0 {
		sec = sec[:j]
	} else if j := strings.Index(sec, "TRANSACTIONS\n------------"); j > 0 {
		sec = sec[:j]
	}
	d := &deadlockInfo{Raw: strings.TrimSpace(clipRaw(sec, 12000))}
	if m := dlHeadRe.FindStringSubmatch(sec); m != nil {
		d.At = m[1]
	}
	victim := 0
	if m := dlVictimRe.FindStringSubmatch(sec); m != nil {
		victim, _ = strconv.Atoi(m[1])
	}
	idx := dlTxRe.FindAllStringSubmatchIndex(sec, -1)
	for k, loc := range idx {
		end := len(sec)
		if k+1 < len(idx) {
			end = idx[k+1][0]
		}
		if v := strings.Index(sec[loc[0]:end], "*** WE ROLL BACK"); v > 0 {
			end = loc[0] + v
		}
		body := sec[loc[0]:end]
		n, _ := strconv.Atoi(sec[loc[2]:loc[3]])
		tx := deadlockTx{Number: n, Victim: n == victim}
		if m := dlThreadRe.FindStringSubmatch(body); m != nil {
			tx.Thread = m[1]
			tx.User = deadlockUser(strings.Fields(m[2]))
		}
		// The statement is the text between the "MySQL thread id" line and the first "***".
		lines := strings.Split(body, "\n")
		var stmt []string
		in := false
		for _, l := range lines {
			if strings.HasPrefix(l, "MySQL thread id") {
				in = true
				continue
			}
			if in {
				if strings.HasPrefix(l, "***") {
					break
				}
				stmt = append(stmt, l)
			}
		}
		tx.Statement = strings.TrimSpace(strings.Join(stmt, "\n"))
		parts := strings.Split(body, "***")
		for _, p := range parts {
			switch {
			case strings.Contains(p, "HOLDS THE LOCK"):
				tx.Holds = lockSummary(p)
			case strings.Contains(p, "WAITING FOR THIS LOCK"):
				tx.WaitsFor = lockSummary(p)
			}
		}
		d.Txs = append(d.Txs, tx)
	}
	return d
}

func lockSummary(p string) string {
	if m := dlLockRe.FindStringSubmatch(p); m != nil {
		obj := m[3]
		if m[4] != "" {
			obj += "." + m[4]
		}
		idx := strings.TrimSuffix(strings.TrimPrefix(m[2], "index "), " of ")
		s := strings.ToLower(m[1]) + " lock on " + obj
		if idx != "" {
			s += " (" + idx + ")"
		}
		s += ", " + m[5]
		if k := lockedKey(p); k != "" {
			s += ", " + k
		}
		return s
	}
	return ""
}

var dlHeapRe = regexp.MustCompile(`Record lock, heap no (\d+)`)

var dlFieldRe = regexp.MustCompile(`(?m)^\s*0: len (\d+); hex ([0-9a-f]+); asc ([^;]*);`)

// lockedKey names the row a record lock is on, from the first field of the record InnoDB dumps:
// the key. Integer keys are stored big-endian with the sign bit flipped; short text keys are
// shown as text.
func lockedKey(p string) string {
	// Heap number 1 is the supremum, the pseudo-record after the last row.
	if h := dlHeapRe.FindStringSubmatch(p); h != nil && h[1] == "1" {
		return "the end of the index (a gap lock)"
	}
	m := dlFieldRe.FindStringSubmatch(p)
	if m == nil {
		return ""
	}
	n, _ := strconv.Atoi(m[1])
	if (n == 1 || n == 2 || n == 4 || n == 8) && len(m[2]) == 2*n {
		if v, err := strconv.ParseUint(m[2], 16, 64); err == nil {
			bits := uint(8 * n)
			v ^= 1 << (bits - 1)
			if v&(1<<(bits-1)) != 0 { // negative: sign-extend
				return fmt.Sprintf("key %d", int64(v)-int64(1)<<bits)
			}
			return fmt.Sprintf("key %d", v)
		}
	}
	if t := strings.TrimSpace(m[3]); n <= 40 && t != "" && !strings.ContainsAny(t, "\x00") {
		return "key '" + t + "'"
	}
	return ""
}

func clipRaw(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…"
}

// --------------------------------------------------------------------------- PostgreSQL

// A row wait in PostgreSQL is a wait on the writer's transaction id, so the lock's object
// names the table the waiter is writing to. (No SQL comments: the text is sent as one line.)
const activityPGSQL = `SELECT json_build_object(
 'sessions', (SELECT coalesce(json_agg(json_build_object(
    'id', a.pid, 'user', a.usename, 'host', coalesce(a.client_addr::text, a.client_hostname, 'local'), 'db', a.datname,
    'cmd', a.state, 'wtype', a.wait_event_type, 'wait', a.wait_event,
    'time', extract(epoch from now() - coalesce(a.state_change, a.backend_start)),
    'qtime', extract(epoch from now() - a.query_start),
    'trxAge', extract(epoch from now() - a.xact_start), 'info', left(a.query, 4000), 'app', a.application_name,
    'blockedBy', pg_blocking_pids(a.pid))), '[]')
   FROM pg_stat_activity a WHERE a.pid <> pg_backend_pid() AND a.backend_type = 'client backend'),
 'locks', (SELECT coalesce(json_agg(json_build_object('pid', l.pid, 'type', l.locktype, 'mode', l.mode,
    'object', coalesce(c.relname, l.relation::text,
      (SELECT 'a row in ' || string_agg(DISTINCT c2.relname, ', ') || ' (transaction ' || l.transactionid || ')'
         FROM pg_locks l2 JOIN pg_class c2 ON c2.oid = l2.relation
        WHERE l2.pid = l.pid AND l2.granted AND l2.locktype = 'relation' AND c2.relkind IN ('r', 'p')
          AND l2.mode IN ('RowExclusiveLock', 'RowShareLock')),
      'transaction ' || l.transactionid, l.virtualxid))), '[]')
   FROM pg_locks l LEFT JOIN pg_class c ON c.oid = l.relation WHERE NOT l.granted),
 'idx', (SELECT coalesce(json_agg(json_build_object('pid', pid, 'phase', phase, 'done', blocks_done, 'total', blocks_total,
    'tdone', tuples_done, 'ttotal', tuples_total)), '[]') FROM pg_stat_progress_create_index),
 'vac', (SELECT coalesce(json_agg(json_build_object('pid', pid, 'phase', phase, 'done', heap_blks_scanned, 'total', heap_blks_total)), '[]')
   FROM pg_stat_progress_vacuum),
 'clu', (SELECT coalesce(json_agg(json_build_object('pid', pid, 'phase', phase, 'done', heap_blks_scanned, 'total', heap_blks_total)), '[]')
   FROM pg_stat_progress_cluster),
 'deadlocks', (SELECT sum(deadlocks) FROM pg_stat_database))`

const activityPGDigests = `SELECT coalesce(json_agg(json_build_object('id', queryid::text, 'text', left(query, 1500), 'calls', calls,
  'time', total_exec_time / 1000, 'exa', rows, 'sent', rows, 'noidx', 0)), '[]')
  FROM (SELECT * FROM pg_stat_statements ORDER BY total_exec_time DESC LIMIT 40) s`

func parsePGActivity(raw []byte) *actSnapshot {
	s := &actSnapshot{Engine: "postgres", Sessions: []*actSession{}, Waits: []actWait{}, DDL: []actDDL{}}
	var v struct {
		Sessions []struct {
			ID, Time, Qtime, TrxAge                     jnum
			User, Host, DB, Cmd, Wtype, Wait, Info, App string
			BlockedBy                                   []jnum
		}
		Locks []struct {
			Pid                jnum
			Type, Mode, Object string
		}
		Idx, Vac, Clu []struct {
			Pid, Done, Total, Tdone, Ttotal jnum
			Phase                           string
		}
		Deadlocks jnum
	}
	decodeNum(raw, &v)
	lockOf := map[string]actWait{}
	for _, l := range v.Locks {
		lockOf[l.Pid.String()] = actWait{Kind: l.Type, Mode: l.Mode, Object: l.Object}
	}
	byID := map[string]*actSession{}
	for _, r := range v.Sessions {
		x := &actSession{ID: r.ID.String(), User: r.User, Host: r.Host, DB: r.DB, Command: r.Cmd, App: r.App, Statement: r.Info, Killable: true}
		if r.Wtype != "" {
			x.State = r.Wtype + ": " + r.Wait
		}
		t := numP(r.Time)
		if r.Cmd == "active" {
			t = numP(r.Qtime)
		}
		if t != nil {
			x.TimeSec = *t
		}
		x.TrxAge = numP(r.TrxAge)
		if pid, err := r.ID.Int64(); err == nil {
			x.OSID = pid
		}
		for _, b := range r.BlockedBy {
			w := lockOf[x.ID]
			w.Waiter, w.Blocker = x.ID, b.String()
			if w.Kind == "" {
				w.Kind = "lock"
			}
			ws := x.TimeSec
			w.WaitSec = &ws
			s.Waits = append(s.Waits, w)
		}
		s.Sessions = append(s.Sessions, x)
		byID[x.ID] = x
	}
	markWaiting(s)
	addProgress := func(rows []struct {
		Pid, Done, Total, Tdone, Ttotal jnum
		Phase                           string
	}, what string) {
		for _, p := range rows {
			x := byID[p.Pid.String()]
			d := actDDL{Session: p.Pid.String(), Phase: what + ": " + p.Phase, Done: numP(p.Done), Total: numP(p.Total)}
			if (d.Total == nil || *d.Total == 0) && numP(p.Ttotal) != nil {
				d.Done, d.Total = numP(p.Tdone), numP(p.Ttotal)
			}
			if x != nil {
				d.Statement, d.TimeSec = x.Statement, x.TimeSec
			}
			for _, w := range s.Waits {
				if w.Blocker == d.Session {
					d.Queue = append(d.Queue, w.Waiter)
				}
			}
			s.DDL = append(s.DDL, d)
		}
	}
	addProgress(v.Idx, "index build")
	addProgress(v.Vac, "vacuum")
	addProgress(v.Clu, "cluster")
	// A DDL with no progress view (ALTER TABLE) is still DDL in flight.
	for _, x := range s.Sessions {
		if x.Command == "active" && isDDL(x.Statement) {
			dup := false
			for _, d := range s.DDL {
				dup = dup || d.Session == x.ID
			}
			if !dup {
				d := actDDL{Session: x.ID, Statement: x.Statement, TimeSec: x.TimeSec, Phase: x.State}
				for _, w := range s.Waits {
					if w.Blocker == x.ID {
						d.Queue = append(d.Queue, w.Waiter)
					}
				}
				s.DDL = append(s.DDL, d)
			}
		}
	}
	s.Deadlocks = intP(v.Deadlocks)
	return s
}

// ------------------------------------------------------------------------------ handlers

// handleActivity is GET /api/stacks/{id}/nodes/{nid}/activity?with=digests,resources,deadlock
func (a *App) handleActivity(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	with := map[string]bool{}
	for _, k := range strings.Split(r.URL.Query().Get("with"), ",") {
		with[strings.TrimSpace(k)] = true
	}
	ctx, cancel := context.WithTimeout(withEngine(r.Context(), a.depEngine(st, nid)), 15*time.Second)
	defer cancel()
	start := time.Now()
	snap, err := a.activitySnapshot(ctx, st, nid, with)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	snap.At = time.Now().UnixMilli()
	snap.CostMs = time.Since(start).Milliseconds()
	snap.Deep = a.deepStateOf(st.ID, nid)
	writeJSON(w, http.StatusOK, snap)
}

func (a *App) activitySnapshot(ctx context.Context, st Stack, nid string, with map[string]bool) (*actSnapshot, error) {
	typ := nodeTypeIn(st, nid)
	switch typ {
	case "proxysql":
		return a.proxysqlActivity(ctx, st, nid, with)
	case "pgbouncer":
		return a.pgbouncerActivity(ctx, st, nid)
	}
	c, ok := a.dbConnFor(st, nid)
	if !ok {
		return nil, errors.New("node is not running")
	}
	switch c.Engine {
	case "mysql":
		sql := activityMySQLSQL
		if with["digests"] {
			sql += activityMySQLDigests
		}
		if with["resources"] {
			sql += activityMySQLResources
		}
		res, err := c.engine().ExecInput(ctx, c.ContainerID, "", append(c.client("mysql"), "-u", c.Super, "-N", "--raw", "-B", "--force"),
			append([]string{"MYSQL_PWD=" + c.Password}, c.Env...), []byte(sql))
		if err != nil {
			return nil, err
		}
		if !strings.Contains(res.Stdout, "{") {
			return nil, fmt.Errorf("%s", lastLines(strings.TrimSpace(res.Stderr), 300))
		}
		snap := parseMySQLActivity(res.Stdout)
		if with["deadlock"] {
			if out, err := c.engine().ExecInput(ctx, c.ContainerID, "", append(c.client("mysql"), "-u", c.Super, "-N", "--raw", "-B"),
				append([]string{"MYSQL_PWD=" + c.Password}, c.Env...), []byte("SHOW ENGINE INNODB STATUS;\n")); err == nil {
				snap.Deadlock = parseDeadlock(out.Stdout)
			}
		}
		if with["resources"] {
			a.osResources(ctx, c.ContainerID, snap, `pidof mysqld mariadbd | awk '{print $1}'`, true)
		}
		return snap, nil
	case "postgres":
		var raw json.RawMessage
		if err := a.queryJSON(ctx, c, "postgres", strings.ReplaceAll(activityPGSQL, "\n", " "), &raw); err != nil {
			return nil, err
		}
		snap := parsePGActivity(raw)
		if with["digests"] {
			var d json.RawMessage
			if err := a.queryJSON(ctx, c, "postgres", strings.ReplaceAll(activityPGDigests, "\n", " "), &d); err == nil {
				var digests []struct {
					ID, Text                      string
					Calls, Time, Exa, Sent, Noidx jnum
				}
				decodeNum(d, &digests)
				for _, x := range digests {
					f := func(n jnum) float64 { v, _ := n.Float64(); return v }
					snap.Digests = append(snap.Digests, actDigest{ID: x.ID, Text: x.Text, Calls: f(x.Calls), TimeSec: f(x.Time), RowsExa: f(x.Exa), RowsOut: f(x.Sent)})
				}
			} else {
				snap.Notes = append(snap.Notes, "pg_stat_statements is not enabled on this server, so there are no statement digests")
			}
		}
		if with["resources"] {
			a.osResources(ctx, c.ContainerID, snap, "", false)
		}
		return snap, nil
	case "mongodb":
		return a.mongoActivity(ctx, c)
	}
	return nil, errors.New("no activity view for this engine")
}

// osResourcesScript prints "id utime+stime read_bytes write_bytes rss" for each OS thread (under
// one server process) or process — the server's own accounting, from /proc, at no cost to it.
const osResourcesScript = `T=$(getconf CLK_TCK)
echo "ticks $T"
P=""; [ -n "$PIDCMD" ] && P=$(sh -c "$PIDCMD")
for id in $IDS; do
  if [ -n "$P" ]; then d=/proc/$P/task/$id; else d=/proc/$id; fi
  [ -r "$d/stat" ] || continue
  c=$(awk '{print $14+$15}' "$d/stat" 2>/dev/null)
  rb=$(awk '/^read_bytes/{print $2}' "$d/io" 2>/dev/null); wb=$(awk '/^write_bytes/{print $2}' "$d/io" 2>/dev/null)
  m=""; [ -z "$P" ] && m=$(awk '/^Pss_Anon|^Private_Dirty/{s+=$2} END{print s*1024}' /proc/$id/smaps_rollup 2>/dev/null)
  echo "$id ${c:-0} ${rb:-0} ${wb:-0} ${m:-}"
done`

func (a *App) osResources(ctx context.Context, cid string, snap *actSnapshot, pidCmd string, threads bool) {
	var ids []string
	byOS := map[int64]*actSession{}
	for _, x := range snap.Sessions {
		if x.OSID > 0 {
			ids = append(ids, strconv.FormatInt(x.OSID, 10))
			byOS[x.OSID] = x
		}
	}
	if len(ids) == 0 {
		return
	}
	res, err := a.engCtx(ctx).Exec(ctx, cid, []string{"sh", "-c", osResourcesScript}, []string{"IDS=" + strings.Join(ids, " "), "PIDCMD=" + pidCmd})
	if err != nil {
		return
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "ticks" {
			snap.ClockTicks, _ = strconv.Atoi(f[1])
			continue
		}
		if len(f) < 4 {
			continue
		}
		id, _ := strconv.ParseInt(f[0], 10, 64)
		x := byOS[id]
		if x == nil {
			continue
		}
		num := func(s string) *int64 {
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil
			}
			return &v
		}
		x.CPUTicks, x.ReadB, x.WriteB = num(f[1]), num(f[2]), num(f[3])
		if len(f) >= 5 && !threads && x.MemBytes == nil {
			x.MemBytes = num(f[4])
		}
	}
}

// handleActivityKill is POST …/activity/kill {"session": "…", "mode": "query"|"connection"}.
func (a *App) handleActivityKill(w http.ResponseWriter, r *http.Request) {
	st, u, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	var in struct{ Session, Mode string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&in); err != nil || !regexp.MustCompile(`^[0-9]+$`).MatchString(in.Session) {
		writeErr(w, http.StatusBadRequest, "session is a numeric id")
		return
	}
	ctx, cancel := context.WithTimeout(withEngine(r.Context(), a.depEngine(st, nid)), 15*time.Second)
	defer cancel()
	if err := a.activityKill(ctx, st, nid, in.Session, in.Mode == "connection"); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	what := map[bool]string{true: "connection", false: "query"}[in.Mode == "connection"]
	a.recordStackEvent(st.ID, nid, "action", "warning", fmt.Sprintf("Killed %s %s on %s", what, in.Session, nodeLabel(buildDoc(st), nid)), "", u.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) activityKill(ctx context.Context, st Stack, nid, session string, conn bool) error {
	if nodeTypeIn(st, nid) == "proxysql" {
		return a.proxysqlKill(ctx, st, nid, session, conn)
	}
	c, ok := a.dbConnFor(st, nid)
	if !ok {
		return errors.New("node is not running")
	}
	switch c.Engine {
	case "mysql":
		stmt := "KILL QUERY " + session
		if conn {
			stmt = "KILL " + session
		}
		return a.execSQL(ctx, c, "mysql", stmt+";")
	case "postgres":
		fn := "pg_cancel_backend"
		if conn {
			fn = "pg_terminate_backend"
		}
		var ok bool
		if err := a.queryJSON(ctx, c, "postgres", "SELECT to_json("+fn+"("+session+"))", &ok); err != nil {
			return err
		}
		if !ok {
			return errors.New("the backend was not signalled (it may have ended)")
		}
		return nil
	case "mongodb":
		return a.mongoKillOp(ctx, c, session)
	}
	return errors.New("not supported for this engine")
}

// handleActivityExplain is POST …/activity/explain {"session": "…"}: the plan of what a session
// runs now — MySQL's EXPLAIN FOR CONNECTION, MariaDB's SHOW EXPLAIN, or for PostgreSQL an
// EXPLAIN (without ANALYZE — nothing is executed) of its statement text.
// explainRefusal turns the server's refusal into what it means for this session.
func explainRefusal(msg, stmt string) string {
	planned := regexp.MustCompile(`(?i)^\s*(\(\s*)*(select|with|update|insert|delete|replace|table)\b`).MatchString(stmt)
	switch {
	case strings.Contains(msg, "1094") || strings.Contains(msg, "Unknown thread"):
		return "That session has ended."
	case strings.Contains(msg, "3012") && planned:
		return "No plan yet: the server plans a statement once it has its table locks, and this one is still waiting for them."
	case strings.Contains(msg, "3012") || (!planned && stmt != ""):
		return "EXPLAIN covers SELECT, INSERT, UPDATE, DELETE and REPLACE; this statement has no plan to show."
	case msg == "":
		return "The server returned no plan for this session."
	}
	return msg
}

func (a *App) handleActivityExplain(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	var in struct{ Session, Statement, DB string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil || !regexp.MustCompile(`^[0-9]+$`).MatchString(in.Session) {
		writeErr(w, http.StatusBadRequest, "session is a numeric id")
		return
	}
	c, ok := a.dbConnFor(st, nid)
	if !ok {
		writeErr(w, http.StatusConflict, "node is not running")
		return
	}
	ctx, cancel := context.WithTimeout(withEngine(r.Context(), a.depEngine(st, nid)), 15*time.Second)
	defer cancel()
	var out string
	switch c.Engine {
	case "mysql":
		// TREE first; a single-table UPDATE or DELETE has no tree plan ("not executable by
		// iterator executor"), so the tabular form follows, spelled out because 9.x
		// defaults to TREE. Plain EXPLAIN is 5.7; SHOW EXPLAIN is MariaDB's spelling.
		var fail string
		for _, q := range []string{"EXPLAIN FORMAT=TREE FOR CONNECTION " + in.Session, "EXPLAIN FORMAT=TRADITIONAL FOR CONNECTION " + in.Session,
			"EXPLAIN FOR CONNECTION " + in.Session, "SHOW EXPLAIN FOR " + in.Session} {
			res, err := c.engine().ExecInput(ctx, c.ContainerID, "", append(c.client("mysql"), "-u", c.Super, "--table"),
				append([]string{"MYSQL_PWD=" + c.Password}, c.Env...), []byte(q+";\n"))
			if err != nil {
				fail = err.Error()
				continue
			}
			if res.Code == 0 && strings.TrimSpace(res.Stdout) != "" && !strings.Contains(res.Stdout, "not executable by iterator executor") {
				out = res.Stdout
				break
			}
			if e := strings.TrimSpace(res.Stderr); e != "" && (fail == "" || !strings.Contains(e, "1064")) {
				fail = e // keep the first real refusal over MariaDB's syntax error on MySQL
			}
		}
		if out == "" {
			out = explainRefusal(fail, in.Statement)
		}
	case "postgres":
		stmt := strings.TrimSpace(in.Statement)
		if stmt == "" || strings.Contains(stmt, "$1") || strings.Count(strings.TrimRight(stmt, "; \n"), ";") > 0 {
			writeErr(w, http.StatusConflict, "this statement cannot be explained from its text (it has bind parameters, or several statements)")
			return
		}
		db := firstNonEmpty(in.DB, "postgres")
		if !dbNameRe.MatchString(db) {
			db = "postgres"
		}
		// EXPLAIN takes the same relation locks the statement needs, so a statement stuck
		// behind a lock would leave this one queued behind it too; give up quickly instead.
		res, err := c.engine().ExecAs(ctx, c.ContainerID, c.pgExecUser(), append(c.client("psql"), "-U", c.Super, "-d", db, "-X", "-q",
			"-c", "SET lock_timeout = '2s'", "-c", "EXPLAIN "+strings.TrimRight(stmt, "; \n")), c.Env)
		if err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		out = firstNonEmpty(res.Stdout, res.Stderr)
		if strings.Contains(out, "lock timeout") {
			out = "No plan: explaining this statement needs the same table locks it is waiting for. Try again once the blocker is gone."
		}
	default:
		writeErr(w, http.StatusConflict, "EXPLAIN is not available for this engine here")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": strings.TrimRight(out, "\n")})
}

var (
	pgDLWaitRe = regexp.MustCompile(`Process (\d+) waits for (.+?); blocked by process (\d+)\.`)
	pgDLStmtRe = regexp.MustCompile(`^Process (\d+): (.*)`)
	pgDLRelRe  = regexp.MustCompile(`relation "([^"]+)"`)
	pgDLSkipRe = regexp.MustCompile(`^\s*(\S+\s+)*?(DETAIL|HINT|CONTEXT|STATEMENT|LOG|ERROR|WARNING|FATAL):\s+`)
)

// pgDeadlockHolds says what the blocker holds, given what its victim waits for: a ShareLock
// on a transaction is a wait for that transaction's row changes; anything else is a lock that
// conflicts with the mode asked for.
func pgDeadlockHolds(waitsFor string) string {
	_, obj, ok := strings.Cut(waitsFor, " on ")
	if !ok {
		return waitsFor
	}
	if id, ok := strings.CutPrefix(obj, "transaction "); ok {
		return "the rows its transaction " + id + " changed"
	}
	return "a conflicting lock on " + obj
}

// parsePGDeadlocks reads the "deadlock detected" reports in a PostgreSQL log, oldest first.
// The process that reports the error is the one the server rolled back; its DETAIL lists
// each process in the cycle, what it waits for and whom it waits on, then their statements.
func parsePGDeadlocks(log string) []*deadlockInfo {
	lines := strings.Split(log, "\n")
	out := []*deadlockInfo{}
	for i, l := range lines {
		if !strings.Contains(l, "ERROR:  deadlock detected") {
			continue
		}
		d := &deadlockInfo{Engine: "postgres"}
		if m := dlHeadRe.FindStringSubmatch(l); m != nil {
			d.At = m[1]
		}
		raw := []string{l}
		var detail []string
		rel := ""
		for j := i + 1; j < len(lines) && j < i+40; j++ {
			x := lines[j]
			cont := strings.HasPrefix(x, "\t")
			if !cont && !pgDLSkipRe.MatchString(x) {
				break
			}
			if !cont && (strings.Contains(x, "ERROR:  ") || strings.Contains(x, "LOG:  ")) {
				break
			}
			raw = append(raw, x)
			switch {
			case strings.Contains(x, "DETAIL:  "):
				detail = append(detail, x[strings.Index(x, "DETAIL:  ")+9:])
			case cont && len(detail) > 0:
				detail = append(detail, strings.TrimPrefix(x, "\t"))
			case strings.Contains(x, "CONTEXT:  "):
				if m := pgDLRelRe.FindStringSubmatch(x); m != nil {
					rel = m[1]
				}
			}
		}
		d.Raw = strings.Join(raw, "\n")
		byPid := map[string]*deadlockTx{}
		var order []string
		tx := func(pid string) *deadlockTx {
			if t, ok := byPid[pid]; ok {
				return t
			}
			t := &deadlockTx{Number: len(order) + 1, Thread: pid}
			byPid[pid] = t
			order = append(order, pid)
			return t
		}
		var last *deadlockTx
		for _, x := range detail {
			if m := pgDLWaitRe.FindStringSubmatch(x); m != nil {
				t := tx(m[1])
				t.WaitsFor = m[2] + " held by process " + m[3]
				tx(m[3]).Holds = pgDeadlockHolds(m[2])
				last = nil
				continue
			}
			if m := pgDLStmtRe.FindStringSubmatch(x); m != nil {
				last = tx(m[1])
				last.Statement = m[2]
				continue
			}
			if last != nil {
				last.Statement += "\n" + x
			}
		}
		if len(order) == 0 {
			continue
		}
		v := byPid[order[0]]
		v.Victim = true
		if rel != "" {
			v.WaitsFor += " (a row in " + rel + ")"
		}
		for _, pid := range order {
			d.Txs = append(d.Txs, *byPid[pid])
		}
		out = append(out, d)
	}
	return out
}

// The tail of the server's current log, where a deadlock's detail is the only place it is kept.
// NULL (so nothing) when the server logs to stderr rather than through its collector.
const pgLogTailSQL = `SELECT to_json(pg_read_file(f, greatest(s.size - 262144, 0), 262144)) FROM pg_current_logfile() f, pg_stat_file(f) s`
