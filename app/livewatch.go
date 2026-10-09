package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// livewatch.go — what happened to a stack while nobody was looking at it.
//
// The Live view (livestate.go) only asks while a canvas has it switched on. The watcher asks on
// its own, every liveWatchSeconds, of every deployed stack, through the same liveSnapshot, and
// keeps three things from each pass:
//
//   - a sample per node (CPU, memory, disk, lag, QPS/TPS, connections, IOPS, its role) for the
//     trend lines of the last day;
//   - events: a node changing role, a primary that moved without anybody asking DBCanvas to move
//     it (an unplanned failover), and every alert opening and closing;
//   - alerts: conditions that hold now — a database down, replication broken, a replica behind,
//     a filesystem filling, connections running out — each opened and closed with some hysteresis
//     so one bad sample is not a notification, and pushed to the owner's notification bell.
//
// Rules are per stack (stack_alert_rules), with defaults that suit a lab. A stack the owner
// stopped on purpose raises nothing: a stopped node is a decision, not a failure.

const (
	settingWatchSeconds = "liveWatchSeconds"
	defaultWatchSeconds = 30
	minWatchSeconds     = 15
	maxWatchSeconds     = 600
	// watchSampleKeep is how far back the trend lines go; watchEventKeep the timeline.
	watchSampleKeep = 24 * time.Hour
	watchEventKeep  = 14 * 24 * time.Hour
	// plannedWindow is how long after DBCanvas switched a primary a role change counts as that
	// switch, not a failover.
	plannedWindow = 5 * time.Minute
	// watchStacksAtOnce bounds the probing: each stack's pass is one exec per node.
	watchStacksAtOnce = 3
)

const watchSchema = `
CREATE TABLE IF NOT EXISTS live_samples (
  stack_id   INTEGER NOT NULL,
  node_id    TEXT NOT NULL,
  at         INTEGER NOT NULL,           -- unix seconds
  state      TEXT NOT NULL DEFAULT '',
  role       TEXT NOT NULL DEFAULT '',
  access     TEXT NOT NULL DEFAULT '',
  cpu        REAL, mem REAL, fs_pct REAL, lag REAL,
  qps        REAL, tps REAL, conns INTEGER, max_conns INTEGER,
  read_iops  REAL, write_iops REAL, net_in REAL, net_out REAL,
  problems   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_live_samples ON live_samples(stack_id, at);
CREATE TABLE IF NOT EXISTS live_events (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  stack_id   INTEGER NOT NULL,
  node_id    TEXT NOT NULL DEFAULT '',
  at         INTEGER NOT NULL,
  kind       TEXT NOT NULL,              -- role|failover|switchover|alert|resolved|action
  severity   TEXT NOT NULL,              -- info|success|warning|error
  title      TEXT NOT NULL,
  detail     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_live_events ON live_events(stack_id, id DESC);
CREATE TABLE IF NOT EXISTS live_alerts (
  stack_id   INTEGER NOT NULL,
  node_id    TEXT NOT NULL,
  kind       TEXT NOT NULL,              -- down|replication|lag|disk|conns
  opened_at  INTEGER NOT NULL,
  severity   TEXT NOT NULL,
  title      TEXT NOT NULL,
  detail     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (stack_id, node_id, kind)
);
CREATE TABLE IF NOT EXISTS stack_alert_rules (
  stack_id   INTEGER PRIMARY KEY,
  rules_json TEXT NOT NULL
);`

// alertRules are a stack's thresholds. A zero threshold switches that alert off.
type alertRules struct {
	Down        bool    `json:"down"`        // the database or its container stops answering
	Replication bool    `json:"replication"` // the engine reports a replication problem
	Failover    bool    `json:"failover"`    // a primary moved without DBCanvas moving it
	LagSec      float64 `json:"lagSec"`      // a replica this far behind
	DiskPct     float64 `json:"diskPct"`     // the data directory's filesystem this full
	ConnPct     float64 `json:"connPct"`     // this share of max connections in use
	TrxSec      float64 `json:"trxSec"`      // a transaction open this long
	LockWaitSec float64 `json:"lockWaitSec"` // a session waiting this long for a lock
	Deadlock    bool    `json:"deadlock"`    // a deadlock (a timeline event, and the bell)
	Notify      bool    `json:"notify"`      // to the owner's notification bell, not just the timeline
}

func defaultAlertRules() alertRules {
	return alertRules{Down: true, Replication: true, Failover: true, LagSec: 30, DiskPct: 85, ConnPct: 90, TrxSec: 600, LockWaitSec: 30, Deadlock: true, Notify: true}
}

func (r alertRules) normalize() alertRules {
	clamp := func(v, lo, hi float64) float64 {
		if v <= 0 || math.IsNaN(v) {
			return 0
		}
		return math.Max(lo, math.Min(hi, v))
	}
	r.LagSec = clamp(r.LagSec, 1, 86400)
	r.DiskPct = clamp(r.DiskPct, 10, 99)
	r.ConnPct = clamp(r.ConnPct, 10, 100)
	r.TrxSec = clamp(r.TrxSec, 10, 86400)
	r.LockWaitSec = clamp(r.LockWaitSec, 1, 86400)
	return r
}

// ---------------------------------------------------------------------------- store

func (s *Store) AlertRules(stackID int64) alertRules {
	var raw string
	if err := s.db.QueryRow("SELECT rules_json FROM stack_alert_rules WHERE stack_id=?", stackID).Scan(&raw); err != nil {
		return defaultAlertRules()
	}
	r := defaultAlertRules()
	if json.Unmarshal([]byte(raw), &r) != nil {
		return defaultAlertRules()
	}
	return r.normalize()
}

func (s *Store) SetAlertRules(stackID int64, r alertRules) error {
	b, _ := json.Marshal(r)
	_, err := s.db.Exec(`INSERT INTO stack_alert_rules(stack_id, rules_json) VALUES(?,?)
	  ON CONFLICT(stack_id) DO UPDATE SET rules_json=excluded.rules_json`, stackID, string(b))
	return err
}

type liveSampleRow struct {
	At        int64    `json:"at"`
	State     string   `json:"state,omitempty"`
	Role      string   `json:"role,omitempty"`
	Access    string   `json:"access,omitempty"`
	CPU       *float64 `json:"cpu,omitempty"`
	Mem       *float64 `json:"mem,omitempty"`
	FSPct     *float64 `json:"fsPct,omitempty"`
	Lag       *float64 `json:"lag,omitempty"`
	QPS       *float64 `json:"qps,omitempty"`
	TPS       *float64 `json:"tps,omitempty"`
	Conns     *int     `json:"conns,omitempty"`
	MaxConns  *int     `json:"maxConns,omitempty"`
	ReadIOPS  *float64 `json:"readIops,omitempty"`
	WriteIOPS *float64 `json:"writeIops,omitempty"`
	NetIn     *float64 `json:"netIn,omitempty"`
	NetOut    *float64 `json:"netOut,omitempty"`
	Problems  int      `json:"problems,omitempty"`
}

func (s *Store) AddLiveSamples(stackID int64, rows map[string]liveSampleRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO live_samples(stack_id,node_id,at,state,role,access,cpu,mem,fs_pct,lag,qps,tps,conns,max_conns,read_iops,write_iops,net_in,net_out,problems)
	  VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for nid, r := range rows {
		if _, err := stmt.Exec(stackID, nid, r.At, r.State, r.Role, r.Access, r.CPU, r.Mem, r.FSPct, r.Lag, r.QPS, r.TPS,
			r.Conns, r.MaxConns, r.ReadIOPS, r.WriteIOPS, r.NetIn, r.NetOut, r.Problems); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// LatestSamples is each node's newest sample since a time.
func (s *Store) LatestSamples(stackID, since int64) map[string]liveSampleRow {
	out := map[string]liveSampleRow{}
	rows, err := s.db.Query(`SELECT node_id, role, MAX(at) FROM live_samples WHERE stack_id=? AND at>=? GROUP BY node_id`, stackID, since)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var nid string
		var r liveSampleRow
		if rows.Scan(&nid, &r.Role, &r.At) == nil {
			out[nid] = r
		}
	}
	return out
}

func (s *Store) LiveSamples(stackID int64, since int64) (map[string][]liveSampleRow, error) {
	rows, err := s.db.Query(`SELECT node_id,at,state,role,access,cpu,mem,fs_pct,lag,qps,tps,conns,max_conns,read_iops,write_iops,net_in,net_out,problems
	  FROM live_samples WHERE stack_id=? AND at>=? ORDER BY at`, stackID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]liveSampleRow{}
	for rows.Next() {
		var nid string
		var r liveSampleRow
		var cpu, mem, fs, lag, qps, tps, ri, wi, ni, no sql.NullFloat64
		var conns, maxc sql.NullInt64
		if err := rows.Scan(&nid, &r.At, &r.State, &r.Role, &r.Access, &cpu, &mem, &fs, &lag, &qps, &tps, &conns, &maxc, &ri, &wi, &ni, &no, &r.Problems); err != nil {
			return nil, err
		}
		f := func(v sql.NullFloat64) *float64 {
			if !v.Valid {
				return nil
			}
			x := math.Round(v.Float64*100) / 100
			return &x
		}
		i := func(v sql.NullInt64) *int {
			if !v.Valid {
				return nil
			}
			x := int(v.Int64)
			return &x
		}
		r.CPU, r.Mem, r.FSPct, r.Lag, r.QPS, r.TPS = f(cpu), f(mem), f(fs), f(lag), f(qps), f(tps)
		r.ReadIOPS, r.WriteIOPS, r.NetIn, r.NetOut = f(ri), f(wi), f(ni), f(no)
		r.Conns, r.MaxConns = i(conns), i(maxc)
		out[nid] = append(out[nid], r)
	}
	return out, rows.Err()
}

type liveEvent struct {
	ID       int64  `json:"id"`
	NodeID   string `json:"nodeId,omitempty"`
	At       int64  `json:"at"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	// Actor is the account that did it, for what DBCanvas was asked to do; empty for what the
	// watcher saw happen.
	Actor string `json:"actor,omitempty"`
}

func (s *Store) AddLiveEvent(stackID int64, e liveEvent) error {
	if e.At == 0 {
		e.At = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO live_events(stack_id,node_id,at,kind,severity,title,detail,actor) VALUES(?,?,?,?,?,?,?,?)`,
		stackID, e.NodeID, e.At, e.Kind, e.Severity, e.Title, e.Detail, e.Actor)
	return err
}

func (s *Store) LiveEvents(stackID int64, since int64, limit int) ([]liveEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id,node_id,at,kind,severity,title,detail,actor FROM live_events
	  WHERE stack_id=? AND at>=? ORDER BY id DESC LIMIT ?`, stackID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []liveEvent{}
	for rows.Next() {
		var e liveEvent
		if err := rows.Scan(&e.ID, &e.NodeID, &e.At, &e.Kind, &e.Severity, &e.Title, &e.Detail, &e.Actor); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// liveAlert is a condition that holds now. Fix names what usually mends it — the UI turns it into
// the action (watchFix).
type liveAlert struct {
	NodeID   string `json:"nodeId"`
	Kind     string `json:"kind"`
	OpenedAt int64  `json:"openedAt"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	Fix      string `json:"fix,omitempty"`
}

func (s *Store) LiveAlerts(stackID int64) ([]liveAlert, error) {
	rows, err := s.db.Query(`SELECT node_id,kind,opened_at,severity,title,detail FROM live_alerts WHERE stack_id=? ORDER BY opened_at`, stackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []liveAlert{}
	for rows.Next() {
		var a liveAlert
		if err := rows.Scan(&a.NodeID, &a.Kind, &a.OpenedAt, &a.Severity, &a.Title, &a.Detail); err != nil {
			return nil, err
		}
		a.Fix = watchFix[a.Kind]
		out = append(out, a)
	}
	return out, rows.Err()
}

// AlertCounts is every stack's open alerts by severity, for the stacks list and a canvas header.
func (s *Store) AlertCounts() (map[int64]map[string]int, error) {
	rows, err := s.db.Query(`SELECT stack_id, severity, COUNT(*) FROM live_alerts GROUP BY stack_id, severity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]int{}
	for rows.Next() {
		var id int64
		var sev string
		var n int
		if err := rows.Scan(&id, &sev, &n); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]int{}
		}
		out[id][sev] = n
	}
	return out, rows.Err()
}

func (s *Store) openAlert(stackID int64, a liveAlert) error {
	_, err := s.db.Exec(`INSERT INTO live_alerts(stack_id,node_id,kind,opened_at,severity,title,detail) VALUES(?,?,?,?,?,?,?)
	  ON CONFLICT(stack_id,node_id,kind) DO UPDATE SET severity=excluded.severity, title=excluded.title, detail=excluded.detail`,
		stackID, a.NodeID, a.Kind, a.OpenedAt, a.Severity, a.Title, a.Detail)
	return err
}

func (s *Store) closeAlert(stackID int64, nodeID, kind string) error {
	_, err := s.db.Exec(`DELETE FROM live_alerts WHERE stack_id=? AND node_id=? AND kind=?`, stackID, nodeID, kind)
	return err
}

// pruneLiveHistory drops what is past keeping, and everything of a stack that is gone or is no
// longer deployed (its alerts: nothing is running to be wrong).
func (s *Store) pruneLiveHistory() {
	now := time.Now()
	s.db.Exec(`DELETE FROM live_samples WHERE at < ?`, now.Add(-watchSampleKeep).Unix())
	s.db.Exec(`DELETE FROM live_events WHERE at < ?`, now.Add(-watchEventKeep).Unix())
	s.db.Exec(`DELETE FROM live_alerts WHERE stack_id NOT IN (SELECT id FROM stacks WHERE status=?)`, StackDeployed)
	for _, t := range []string{"live_samples", "live_events", "stack_alert_rules"} {
		s.db.Exec(`DELETE FROM ` + t + ` WHERE stack_id NOT IN (SELECT id FROM stacks)`)
	}
}

// ---------------------------------------------------------------------------- the watcher

// watchFix is the remedy an alert's kind usually has; the UI offers it beside the alert.
var watchFix = map[string]string{
	"down":        "restart", // restart the node, then read its logs if it will not stay up
	"replication": "rebuild", // re-clone the replica from the primary
	"lag":         "inspect", // the replica's applier: what it is doing, and what it is waiting on
	"disk":        "inspect", // what is filling it
	"conns":       "inspect", // who holds the connections
}

// watchNode is what the watcher remembers of a node between passes.
type watchNode struct {
	at                time.Time
	role, access      string
	readOps, writeOps int64
	netRx, netTx      int64
	queries, commits  *int64
	loadAt            int64
	streak            map[string]int // kind -> consecutive passes the condition held (+) or did not (-)
}

var watchState = struct {
	mu      sync.Mutex
	stacks  map[int64]map[string]*watchNode
	primary map[int64]map[string]string // stack -> frame -> node id of its primary
	planned map[int64]time.Time         // stack -> when DBCanvas last switched one of its primaries
	action  map[string]time.Time        // "stack:node" -> when DBCanvas last started, stopped or restarted it
}{stacks: map[int64]map[string]*watchNode{}, primary: map[int64]map[string]string{}, planned: map[int64]time.Time{}, action: map[string]time.Time{}}

// nodeActionWindow is how long after DBCanvas started, stopped or restarted a node its being down
// is that action, not an alert.
const nodeActionWindow = 2 * time.Minute

// markNodeAction is called by a node's start, stop and restart.
func markNodeAction(stackID int64, nid string) {
	watchState.mu.Lock()
	watchState.action[fmt.Sprintf("%d:%s", stackID, nid)] = time.Now()
	for k, at := range watchState.action {
		if time.Since(at) > nodeActionWindow {
			delete(watchState.action, k)
		}
	}
	watchState.mu.Unlock()
}

// markPlannedSwitch is called by the switchover before it moves a primary, so the role changes
// that follow are recorded as that switch rather than taken for a failover.
func markPlannedSwitch(stackID int64) {
	watchState.mu.Lock()
	watchState.planned[stackID] = time.Now()
	watchState.mu.Unlock()
}

func (a *App) watchSeconds() int {
	v, err := a.store.AppSetting(settingWatchSeconds)
	if err != nil || strings.TrimSpace(v) == "" {
		return defaultWatchSeconds
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return defaultWatchSeconds
	}
	return clampWatchSeconds(n)
}

// clampWatchSeconds: 0 is off; anything else lands between the bounds.
func clampWatchSeconds(n int) int {
	switch {
	case n <= 0:
		return 0
	case n < minWatchSeconds:
		return minWatchSeconds
	case n > maxWatchSeconds:
		return maxWatchSeconds
	}
	return n
}

func (a *App) startLiveWatch() {
	go func() {
		lastPrune := time.Time{}
		for {
			sec := a.watchSeconds()
			if sec == 0 {
				time.Sleep(time.Minute)
				continue
			}
			start := time.Now()
			a.watchPass()
			if time.Since(lastPrune) > 10*time.Minute {
				a.store.pruneLiveHistory()
				lastPrune = time.Now()
			}
			if d := time.Duration(sec)*time.Second - time.Since(start); d > 0 {
				time.Sleep(d)
			}
		}
	}()
}

// watchPass samples every deployed stack once.
func (a *App) watchPass() {
	stacks, err := a.store.ListStacks(0, true)
	if err != nil {
		return
	}
	sem := make(chan struct{}, watchStacksAtOnce)
	var wg sync.WaitGroup
	live := map[int64]bool{}
	for _, s := range stacks {
		if s.Status != StackDeployed {
			continue
		}
		live[s.ID] = true
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			st, err := a.store.GetStack(id)
			if err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			snap, err := a.liveSnapshot(ctx, st)
			if err != nil || len(snap) == 0 {
				return
			}
			a.watchStack(st, snap, time.Now())
			a.watchDeadlocks(ctx, st, snap)
		}(s.ID)
	}
	wg.Wait()
	// Forget the stacks that went away, so a redeploy starts from a clean slate.
	watchState.mu.Lock()
	for id := range watchState.stacks {
		if !live[id] {
			delete(watchState.stacks, id)
			delete(watchState.primary, id)
		}
	}
	watchState.mu.Unlock()
}

// watchCond is one alert condition as evaluated on one pass.
type watchCond struct {
	kind, severity, title, detail string
	holds                         bool
	openAfter, closeAfter         int // passes in a row
}

// watchStack records one pass of a stack: its samples, its role changes, and its alerts.
func (a *App) watchStack(st Stack, snap map[string]*liveNode, now time.Time) {
	doc := buildDoc(st)
	label := map[string]string{}
	frameOf := map[string]string{}
	frameLabel := map[string]string{}
	for _, n := range doc.Nodes {
		label[n.ID] = firstNonEmpty(n.Label, n.ID)
		frameOf[n.ID] = n.FrameID
	}
	for _, f := range doc.Frames {
		frameLabel[f.ID] = firstNonEmpty(f.Label, f.ID)
	}
	rules := a.store.AlertRules(st.ID)
	open, _ := a.store.LiveAlerts(st.ID)
	isOpen := map[string]liveAlert{}
	for _, al := range open {
		isOpen[al.NodeID+"\x00"+al.Kind] = al
	}
	deps, _ := a.store.ListDeployments(st.ID)
	stoppedByUser := map[string]bool{}
	for _, d := range deps {
		if d.State == DeployStopped {
			stoppedByUser[d.NodeID] = true
		}
	}

	watchState.mu.Lock()
	nodes := watchState.stacks[st.ID]
	first := nodes == nil
	if first {
		nodes = map[string]*watchNode{}
		watchState.stacks[st.ID] = nodes
	}
	watchState.mu.Unlock()
	// After a restart the watcher remembers nothing: what the last samples said (if recent) is
	// where it carries on from, so a failover across a restart is still one.
	var seeded map[string]liveSampleRow
	if first {
		seeded = a.store.LatestSamples(st.ID, now.Add(-10*time.Minute).Unix())
	}
	watchState.mu.Lock()
	primaries := watchState.primary[st.ID]
	if primaries == nil {
		primaries = map[string]string{}
		watchState.primary[st.ID] = primaries
		for nid, r := range seeded {
			if r.Role == "primary" && frameOf[nid] != "" {
				primaries[frameOf[nid]] = nid
			}
		}
	}
	planned := now.Sub(watchState.planned[st.ID]) < plannedWindow
	watchState.mu.Unlock()

	samples := map[string]liveSampleRow{}
	var events []liveEvent
	type change struct {
		open bool
		al   liveAlert
	}
	var changes []change
	newPrimary := map[string]string{} // frame -> node now primary

	ids := make([]string, 0, len(snap))
	for id := range snap {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, nid := range ids {
		n := snap[nid]
		name := label[nid]
		row := liveSampleRow{At: now.Unix(), State: n.State}
		watchState.mu.Lock()
		busy := now.Sub(watchState.action[fmt.Sprintf("%d:%s", st.ID, nid)]) < nodeActionWindow
		prev := nodes[nid]
		if prev == nil {
			prev = &watchNode{streak: map[string]int{}, role: seeded[nid].Role}
			nodes[nid] = prev
		}
		watchState.mu.Unlock()
		dt := now.Sub(prev.at).Seconds()
		ok := !prev.at.IsZero() && dt > 0
		rate := func(cur, old int64) *float64 {
			if !ok || cur < old {
				return nil
			}
			v := float64(cur-old) / dt
			return &v
		}
		if n.State == "running" {
			cpu, mem := n.CPUPercent, n.MemPercent
			row.CPU, row.Mem = &cpu, &mem
			row.NetIn, row.NetOut = rate(n.NetRx, prev.netRx), rate(n.NetTx, prev.netTx)
			if n.liveIO != nil {
				row.ReadIOPS, row.WriteIOPS = rate(n.ReadOps, prev.readOps), rate(n.WriteOps, prev.writeOps)
				prev.readOps, prev.writeOps = n.ReadOps, n.WriteOps
			}
			prev.netRx, prev.netTx = n.NetRx, n.NetTx
			if n.Disk != nil && n.Disk.FSTotal > 0 {
				p := float64(n.Disk.FSUsed) / float64(n.Disk.FSTotal) * 100
				row.FSPct = &p
			}
		}
		r := n.Role
		if r != nil {
			row.Role, row.Access, row.Lag, row.Problems = r.Role, r.Access, r.LagSec, len(r.Problems)
			if l := r.Load; l != nil {
				row.Conns, row.MaxConns = l.Conns, l.MaxConns
				if l.AtMs != prev.loadAt && prev.loadAt != 0 {
					ldt := float64(l.AtMs-prev.loadAt) / 1000
					lr := func(cur, old *int64) *float64 {
						if cur == nil || old == nil || *cur < *old || ldt <= 0 {
							return nil
						}
						v := float64(*cur-*old) / ldt
						return &v
					}
					row.QPS, row.TPS = lr(l.Queries, prev.queries), lr(l.Commits, prev.commits)
				}
				prev.queries, prev.commits, prev.loadAt = l.Queries, l.Commits, l.AtMs
			}
			// A role change, only between two roles the engine actually gave.
			if r.Err == "" && !r.Down && r.Role != "" {
				if prev.role != "" && prev.role != r.Role {
					events = append(events, liveEvent{NodeID: nid, At: now.Unix(), Kind: "role", Severity: "info",
						Title: fmt.Sprintf("%s is now %s (was %s)", name, r.Role, prev.role)})
				}
				prev.role, prev.access = r.Role, r.Access
				if r.Role == "primary" && frameOf[nid] != "" {
					newPrimary[frameOf[nid]] = nid
				}
			}
		}
		prev.at = now
		samples[nid] = row

		// The conditions.
		var conds []watchCond
		down := n.State == "error" || n.State == "unreachable" || (r != nil && r.Down)
		if rules.Down && !stoppedByUser[nid] {
			what := "the database is not answering"
			switch {
			case n.State == "error":
				what = "the node is in error"
			case n.State == "unreachable":
				what = "the container is not answering"
			}
			detail := ""
			if r != nil && r.Down {
				detail = r.Err
			}
			conds = append(conds, watchCond{kind: "down", severity: "error", title: name + ": " + what, detail: detail,
				holds: down && !busy, openAfter: 1, closeAfter: 1})
		}
		if rules.Replication {
			c := watchCond{kind: "replication", severity: "warning", openAfter: 2, closeAfter: 2}
			if r != nil && !down && !busy && len(r.Problems) > 0 {
				c.holds = true
				c.title = fmt.Sprintf("%s: %s", name, clipLine(r.Problems[0], 120))
				c.detail = strings.Join(r.Problems, "\n")
			}
			conds = append(conds, c)
		}
		if rules.LagSec > 0 {
			c := watchCond{kind: "lag", severity: "warning", openAfter: 2, closeAfter: 2}
			if r != nil && r.LagSec != nil && *r.LagSec > rules.LagSec {
				c.holds = true
				c.title = fmt.Sprintf("%s is %.0fs behind its source", name, *r.LagSec)
				c.detail = fmt.Sprintf("threshold %.0fs", rules.LagSec)
			}
			conds = append(conds, c)
		}
		if rules.DiskPct > 0 {
			c := watchCond{kind: "disk", severity: "warning", openAfter: 1, closeAfter: 2}
			if row.FSPct != nil && *row.FSPct > rules.DiskPct {
				c.holds = true
				c.title = fmt.Sprintf("%s: filesystem %.0f%% full", name, *row.FSPct)
				c.detail = fmt.Sprintf("%s · threshold %.0f%%", n.Disk.Path, rules.DiskPct)
				if *row.FSPct > 95 {
					c.severity = "error"
				}
			}
			conds = append(conds, c)
		}
		if rules.ConnPct > 0 {
			c := watchCond{kind: "conns", severity: "warning", openAfter: 2, closeAfter: 2}
			if row.Conns != nil && row.MaxConns != nil && *row.MaxConns > 0 {
				if p := float64(*row.Conns) / float64(*row.MaxConns) * 100; p >= rules.ConnPct {
					c.holds = true
					c.title = fmt.Sprintf("%s: %d of %d connections in use", name, *row.Conns, *row.MaxConns)
					c.detail = fmt.Sprintf("threshold %.0f%%", rules.ConnPct)
				}
			}
			conds = append(conds, c)
		}
		if rules.TrxSec > 0 {
			c := watchCond{kind: "trx", severity: "warning", openAfter: 2, closeAfter: 2}
			if r != nil && r.Txn != nil && r.Txn.OldestSec > rules.TrxSec {
				c.holds = true
				c.title = fmt.Sprintf("%s: a transaction has been open %s", name, (time.Duration(r.Txn.OldestSec) * time.Second).String())
				c.detail = fmt.Sprintf("threshold %s — see Activity → Transactions", (time.Duration(rules.TrxSec) * time.Second).String())
			}
			conds = append(conds, c)
		}
		if rules.LockWaitSec > 0 {
			c := watchCond{kind: "lockwait", severity: "warning", openAfter: 1, closeAfter: 2}
			if r != nil && r.Txn != nil && r.Txn.LockWaiters > 0 && r.Txn.LockWaitMaxSec > rules.LockWaitSec {
				c.holds = true
				c.title = fmt.Sprintf("%s: %d session(s) waiting for a lock, the longest %.0fs", name, r.Txn.LockWaiters, r.Txn.LockWaitMaxSec)
				c.detail = fmt.Sprintf("threshold %.0fs — see Activity → Blocking", rules.LockWaitSec)
			}
			conds = append(conds, c)
		}
		watchState.mu.Lock()
		for _, c := range conds {
			s := prev.streak[c.kind]
			if c.holds {
				if s < 0 {
					s = 0
				}
				s++
			} else {
				if s > 0 {
					s = 0
				}
				s--
			}
			prev.streak[c.kind] = s
			key := nid + "\x00" + c.kind
			cur, wasOpen := isOpen[key]
			switch {
			case c.holds && !wasOpen && s >= c.openAfter:
				changes = append(changes, change{true, liveAlert{NodeID: nid, Kind: c.kind, OpenedAt: now.Unix(), Severity: c.severity, Title: c.title, Detail: c.detail}})
			case c.holds && wasOpen && (cur.Title != c.title || cur.Detail != c.detail || cur.Severity != c.severity):
				a.store.openAlert(st.ID, liveAlert{NodeID: nid, Kind: c.kind, OpenedAt: cur.OpenedAt, Severity: c.severity, Title: c.title, Detail: c.detail})
			case !c.holds && wasOpen && -s >= c.closeAfter:
				changes = append(changes, change{false, cur})
			}
			delete(isOpen, key)
		}
		watchState.mu.Unlock()
	}
	// An alert on a node that is gone from the snapshot, or whose rule was switched off, closes.
	for _, al := range isOpen {
		if _, there := snap[al.NodeID]; !there || !ruleOn(rules, al.Kind) {
			changes = append(changes, change{false, al})
		}
	}

	// A primary that moved. The first pass only learns who is primary.
	for fid, nid := range newPrimary {
		was := primaries[fid]
		primaries[fid] = nid
		if was == "" || was == nid {
			continue
		}
		fl := frameLabel[fid]
		if planned {
			events = append(events, liveEvent{NodeID: nid, At: now.Unix(), Kind: "switchover", Severity: "info",
				Title: fmt.Sprintf("%s: %s took over as primary from %s (switchover)", fl, label[nid], label[was])})
			continue
		}
		e := liveEvent{NodeID: nid, At: now.Unix(), Kind: "failover", Severity: "warning",
			Title:  fmt.Sprintf("Unplanned failover in %s: %s is now primary (was %s)", fl, label[nid], label[was]),
			Detail: "DBCanvas did not switch this primary — the cluster's own failover (or somebody by hand) did. The design still marks the old one; Replication role on the new primary records it."}
		events = append(events, e)
		if rules.Failover && rules.Notify {
			a.notifyStack(st.ID, "live.failover", "warning", "Unplanned failover", st.Name+": "+e.Title, nid)
		}
	}

	if err := a.store.AddLiveSamples(st.ID, samples); err != nil {
		log.Printf("livewatch: samples for stack %d: %v", st.ID, err)
	}
	for _, c := range changes {
		al := c.al
		if c.open {
			a.store.openAlert(st.ID, al)
			events = append(events, liveEvent{NodeID: al.NodeID, At: now.Unix(), Kind: "alert", Severity: al.Severity, Title: al.Title, Detail: al.Detail})
			if rules.Notify {
				a.notifyStack(st.ID, "live.alert", al.Severity, alertHeading(al.Kind), st.Name+": "+al.Title, al.NodeID)
			}
			continue
		}
		a.store.closeAlert(st.ID, al.NodeID, al.Kind)
		lasted := time.Duration(now.Unix()-al.OpenedAt) * time.Second
		title := "Resolved: " + al.Title
		events = append(events, liveEvent{NodeID: al.NodeID, At: now.Unix(), Kind: "resolved", Severity: "success", Title: title,
			Detail: "lasted " + lasted.Round(time.Second).String()})
		if rules.Notify {
			a.notifyStack(st.ID, "live.resolved", "success", alertHeading(al.Kind)+" resolved", st.Name+": "+al.Title+" — resolved after "+lasted.Round(time.Second).String(), al.NodeID)
		}
	}
	for _, e := range events {
		a.store.AddLiveEvent(st.ID, e)
	}
}

func ruleOn(r alertRules, kind string) bool {
	switch kind {
	case "down":
		return r.Down
	case "replication":
		return r.Replication
	case "lag":
		return r.LagSec > 0
	case "disk":
		return r.DiskPct > 0
	case "conns":
		return r.ConnPct > 0
	case "trx":
		return r.TrxSec > 0
	case "lockwait":
		return r.LockWaitSec > 0
	}
	return false
}

func alertHeading(kind string) string {
	return map[string]string{"down": "Database down", "replication": "Replication problem", "lag": "Replica behind",
		"disk": "Filesystem filling", "conns": "Connections running out", "trx": "Long transaction", "lockwait": "Lock wait"}[kind]
}

// recordStackEvent puts something DBCanvas did on the stack's timeline (a switchover, a rebuild),
// with the account that asked for it — the stack's audit of operational actions.
func (a *App) recordStackEvent(stackID int64, nodeID, kind, severity, title, detail, actor string) {
	a.store.AddLiveEvent(stackID, liveEvent{NodeID: nodeID, Kind: kind, Severity: severity, Title: title, Detail: detail, Actor: actor})
}

// ---------------------------------------------------------------------------- HTTP

// handleStackHistory is the stack's last minutes of samples, its timeline and its open alerts.
func (a *App) handleStackHistory(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	minutes, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
	if minutes <= 0 {
		minutes = 60
	}
	if minutes > int(watchSampleKeep/time.Minute) {
		minutes = int(watchSampleKeep / time.Minute)
	}
	since := time.Now().Add(-time.Duration(minutes) * time.Minute).Unix()
	samples, err := a.store.LiveSamples(st.ID, since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read history")
		return
	}
	evSince := int64(0)
	if r.URL.Query().Get("events") != "all" {
		evSince = since
	}
	events, _ := a.store.LiveEvents(st.ID, evSince, 300)
	alerts, _ := a.store.LiveAlerts(st.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"intervalSec": a.watchSeconds(), "minutes": minutes,
		"samples": samples, "events": events, "alerts": alerts,
	})
}

// handleStackAlerts is just the open alerts and the rules — the canvas header's health summary.
func (a *App) handleStackAlerts(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	alerts, err := a.store.LiveAlerts(st.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read alerts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts, "rules": a.store.AlertRules(st.ID), "intervalSec": a.watchSeconds()})
}

func (a *App) handlePutAlertRules(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var in alertRules
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	in = in.normalize()
	if err := a.store.SetAlertRules(st.ID, in); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save rules")
		return
	}
	writeJSON(w, http.StatusOK, in)
}

// handleAlertSummary is every visible stack's open alerts by severity, for the stacks list.
func (a *App) handleAlertSummary(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	counts, err := a.store.AlertCounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read alerts")
		return
	}
	stacks, _ := a.store.ListStacks(u.ID, u.Role == RoleAdmin)
	out := map[string]map[string]int{}
	for _, s := range stacks {
		if c := counts[s.ID]; c != nil {
			out[strconv.FormatInt(s.ID, 10)] = c
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stacks": out})
}

// ------------------------------------------------------------------------------ deadlocks

// watchDeadlocks records each new deadlock on the stack's timeline. InnoDB keeps only its latest
// deadlock, so each pass reads it and compares it with the last one recorded for the node:
// a deadlock that comes and goes between two passes is still caught unless another replaces it
// within the same 30 seconds. PostgreSQL keeps a count; its increase is the event.
var pgDeadlockCounts = struct {
	sync.Mutex
	m map[string]int64
}{m: map[string]int64{}}

func (a *App) watchDeadlocks(ctx context.Context, st Stack, snap map[string]*liveNode) {
	rules := a.store.AlertRules(st.ID)
	if !rules.Deadlock {
		return
	}
	doc := buildDoc(st)
	for nid, ln := range snap {
		if ln == nil || ln.State != "running" || ln.Role == nil || ln.Role.Down {
			continue
		}
		c, ok := a.dbConnFor(st, nid)
		if !ok {
			continue
		}
		cctx, cancel := context.WithTimeout(withEngine(ctx, a.depEngine(st, nid)), 10*time.Second)
		switch c.Engine {
		case "mysql":
			res, err := c.engine().ExecInput(cctx, c.ContainerID, "", append(c.client("mysql"), "-u", c.Super, "-N", "--raw", "-B"),
				append([]string{"MYSQL_PWD=" + c.Password}, c.Env...), []byte("SHOW ENGINE INNODB STATUS;\n"))
			if err == nil {
				if d := parseDeadlock(res.Stdout); d != nil && d.At != "" {
					a.recordDeadlock(st, doc, nid, d, rules)
				}
			}
		case "postgres":
			var n int64
			if a.queryJSON(cctx, c, "postgres", "SELECT coalesce(sum(deadlocks), 0) FROM pg_stat_database", &n) == nil {
				key := fmt.Sprintf("%d:%s", st.ID, nid)
				pgDeadlockCounts.Lock()
				prev, seen := pgDeadlockCounts.m[key]
				pgDeadlockCounts.m[key] = n
				pgDeadlockCounts.Unlock()
				if seen && n > prev {
					a.recordPGDeadlocks(cctx, st, doc, nid, c, int(n-prev), rules)
				}
			}
		}
		cancel()
	}
}

// recordDeadlock adds the deadlock to the timeline unless it is the one recorded last for this
// node (InnoDB keeps showing its latest until another happens).
func (a *App) recordDeadlock(st Stack, doc designDoc, nid string, d *deadlockInfo, rules alertRules) {
	evs, _ := a.store.LiveEventsOf(st.ID, nid, "deadlock", 1)
	if len(evs) > 0 {
		var last deadlockInfo
		if json.Unmarshal([]byte(evs[0].Detail), &last) == nil && last.At == d.At {
			return
		}
	}
	name := nodeLabel(doc, nid)
	victim := ""
	for _, t := range d.Txs {
		if t.Victim {
			victim = fmt.Sprintf(" — transaction %d (thread %s) rolled back", t.Number, t.Thread)
			if d.Engine == "postgres" {
				victim = fmt.Sprintf(" — process %s's transaction rolled back", t.Thread)
			}
		}
	}
	b, _ := json.Marshal(d)
	title := fmt.Sprintf("Deadlock on %s at %s%s", name, d.At, victim)
	a.store.AddLiveEvent(st.ID, liveEvent{NodeID: nid, Kind: "deadlock", Severity: "warning", Title: title, Detail: string(b)})
	if rules.Notify {
		a.notifyStack(st.ID, "live.deadlock", "warning", "Deadlock", st.Name+": "+title, nid)
	}
}

// recordPGDeadlocks records the deadlocks the counter says happened, with their detail from
// the server log when it can be read; otherwise a count, pointing at the log.
func (a *App) recordPGDeadlocks(ctx context.Context, st Stack, doc designDoc, nid string, c dbConn, n int, rules alertRules) {
	var log string
	var found []*deadlockInfo
	if a.queryJSON(ctx, c, "postgres", pgLogTailSQL, &log) == nil {
		found = parsePGDeadlocks(log)
	}
	if len(found) > n {
		found = found[len(found)-n:]
	}
	recent, _ := a.store.LiveEventsOf(st.ID, nid, "deadlock", 50)
	seen := map[string]bool{}
	for _, e := range recent {
		var d deadlockInfo
		if json.Unmarshal([]byte(e.Detail), &d) == nil && d.At != "" && len(d.Txs) > 0 {
			seen[d.At+"/"+d.Txs[0].Thread] = true
		}
	}
	recorded := 0
	for _, d := range found {
		if d.At == "" || seen[d.At+"/"+d.Txs[0].Thread] {
			continue
		}
		a.recordDeadlock(st, doc, nid, d, rules)
		recorded++
	}
	if recorded > 0 || len(found) > 0 {
		return
	}
	title := fmt.Sprintf("%d deadlocks on %s", n, nodeLabel(doc, nid))
	if n == 1 {
		title = "A deadlock on " + nodeLabel(doc, nid)
	}
	a.store.AddLiveEvent(st.ID, liveEvent{NodeID: nid, Kind: "deadlock", Severity: "warning", Title: title,
		Detail: "PostgreSQL counted it, but its detail is only in the server log, which could not be read here — see the node's error log (Live details)."})
	if rules.Notify {
		a.notifyStack(st.ID, "live.deadlock", "warning", "Deadlock", st.Name+": "+title, nid)
	}
}

// LiveEventsOf is a node's latest events of one kind.
func (s *Store) LiveEventsOf(stackID int64, nid, kind string, limit int) ([]liveEvent, error) {
	rows, err := s.db.Query(`SELECT id,node_id,at,kind,severity,title,detail,actor FROM live_events
	  WHERE stack_id=? AND node_id=? AND kind=? ORDER BY id DESC LIMIT ?`, stackID, nid, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []liveEvent{}
	for rows.Next() {
		var e liveEvent
		if err := rows.Scan(&e.ID, &e.NodeID, &e.At, &e.Kind, &e.Severity, &e.Title, &e.Detail, &e.Actor); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// handleNodeDeadlocks is the node's deadlock history, newest first, each parsed.
func (a *App) handleNodeDeadlocks(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	evs, err := a.store.LiveEventsOf(st.ID, r.PathValue("nid"), "deadlock", 50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read deadlocks")
		return
	}
	type item struct {
		At       int64         `json:"at"`
		Title    string        `json:"title"`
		Deadlock *deadlockInfo `json:"deadlock,omitempty"`
		Note     string        `json:"note,omitempty"`
	}
	out := []item{}
	for _, e := range evs {
		it := item{At: e.At, Title: e.Title}
		var d deadlockInfo
		if json.Unmarshal([]byte(e.Detail), &d) == nil && d.At != "" {
			it.Deadlock = &d
		} else {
			it.Note = e.Detail
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deadlocks": out})
}
