package mysqlx

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"perfsim/internal/core"
)

// fcLimit is Galera's default gcs.fc_limit: a receive queue this long sends
// flow control.
const fcLimit = 100

type memberSample struct {
	m       core.Member
	srv     core.ServerSample
	writer  bool
	fcPause float64
}

// Sample reads every member in parallel. See core.Engine.
func (e *Engine) Sample(ctx context.Context) ([]core.Member, core.ServerSample) {
	e.mu.Lock()
	topo := e.topology
	e.mu.Unlock()

	out := make([]memberSample, len(e.cfg.Members))
	var wg sync.WaitGroup
	for i, mc := range e.cfg.Members {
		wg.Add(1)
		go func(i int, mc MemberConfig) {
			defer wg.Done()
			out[i] = e.sampleMember(ctx, mc, topo)
		}(i, mc)
	}
	wg.Wait()

	var members []core.Member
	var srv core.ServerSample
	writerID := ""
	for _, s := range out {
		if s.writer && writerID == "" {
			writerID = s.m.ID
		}
	}
	if writerID == "" && len(out) > 0 {
		writerID = out[0].m.ID
	}
	if topo == "gr" && writerID != "" {
		for _, x := range out {
			if x.m.ID == writerID && x.writer {
				e.followPrimary(writerID)
			}
		}
	}
	e.mu.Lock()
	e.writerID = writerID
	for _, s := range out {
		if s.m.Lag >= 0 {
			e.lag[s.m.ID] = s.m.Lag
		} else {
			delete(e.lag, s.m.ID)
		}
	}
	e.mu.Unlock()
	for _, s := range out {
		if s.m.ID == writerID && s.m.Role != "writer" && topo == "galera" {
			s.m.Role = "writer"
		}
		if s.m.ID == writerID {
			srv.LockWaits, srv.Threads, srv.Conns, srv.SendQ = s.srv.LockWaits, s.srv.Threads, s.srv.Conns, s.m.SendQ
		}
		if s.fcPause > srv.FC {
			srv.FC = s.fcPause
		}
		members = append(members, s.m)
	}
	return members, srv
}

func (e *Engine) sampleMember(ctx context.Context, mc MemberConfig, topo string) memberSample {
	db := e.mdb[mc.ID]
	s := memberSample{m: core.Member{ID: mc.ID, Role: mc.Role, Lag: -1}}

	var lagUS sql.NullInt64
	err := db.QueryRowContext(ctx, "SELECT TIMESTAMPDIFF(MICROSECOND, ts, NOW(6)) FROM perfsim.heartbeat WHERE id = 1").Scan(&lagUS)
	if err != nil && !isNoRows(err) && !strings.Contains(err.Error(), "doesn't exist") {
		s.m.Chip, s.m.ChipText = "bad", "unreachable"
		s.m.Fields = [][2]string{{"error", short(err.Error(), 120)}}
		return s
	}
	s.m.Up = true
	if lagUS.Valid {
		s.m.Lag = float64(lagUS.Int64) / 1e6
		if s.m.Lag < 0 {
			s.m.Lag = 0
		}
	}
	st := globalStatus(ctx, db, "Threads_running", "Threads_connected", "Innodb_row_lock_current_waits")
	s.srv = core.ServerSample{Threads: num(st["Threads_running"]), Conns: num(st["Threads_connected"]), LockWaits: num(st["Innodb_row_lock_current_waits"])}

	switch topo {
	case "galera":
		e.sampleGalera(ctx, db, &s)
	case "gr":
		e.sampleGR(ctx, db, &s)
	default:
		e.sampleAsync(ctx, db, &s)
	}
	return s
}

func (e *Engine) sampleAsync(ctx context.Context, db *sql.DB, s *memberSample) {
	row, ok := queryRow(ctx, db, e.w("status"))
	if !ok {
		s.writer, s.m.Role = true, "source"
		s.m.Chip, s.m.ChipText = "ok", "source"
		s.m.Lag = 0
		ro := globalVar(ctx, db, "read_only")
		s.m.Fields = [][2]string{{"read_only", onOff(ro)}, {"gtid_mode", globalVar(ctx, db, "gtid_mode")}}
		return
	}
	s.m.Role = "replica"
	pick := func(names ...string) string {
		for _, n := range names {
			if v, ok := row[n]; ok {
				return v
			}
		}
		return ""
	}
	io := pick("Replica_IO_Running", "Slave_IO_Running")
	sqlRun := pick("Replica_SQL_Running", "Slave_SQL_Running")
	sbs := pick("Seconds_Behind_Source", "Seconds_Behind_Master")
	state := pick("Replica_SQL_Running_State", "Slave_SQL_Running_State")
	lastErr := pick("Last_SQL_Error")
	workers := globalVar(ctx, db, e.w("workers"))
	if sbs == "" {
		sbs = "NULL"
	}
	s.m.Fields = [][2]string{
		{pickName(row, "Replica_IO_Running", "Slave_IO_Running"), io},
		{pickName(row, "Replica_SQL_Running", "Slave_SQL_Running"), sqlRun},
		{pickName(row, "Seconds_Behind_Source", "Seconds_Behind_Master"), sbs},
		{pickName(row, "Replica_SQL_Running_State", "Slave_SQL_Running_State"), state},
		{"Retrieved_Gtid_Set", short(pick("Retrieved_Gtid_Set", "Gtid_IO_Pos"), 64)},
		{"Executed_Gtid_Set", short(pick("Executed_Gtid_Set", "Gtid_Slave_Pos"), 64)},
		{"SQL_Delay", pick("SQL_Delay")},
		{e.w("workers"), workers},
	}
	if lastErr != "" {
		s.m.Fields = append(s.m.Fields, [2]string{"Last_SQL_Error", short(lastErr, 120)})
	}
	ls := strings.ToLower(state)
	switch {
	case !strings.EqualFold(io, "Yes") && !strings.EqualFold(io, "Connecting"):
		s.m.Chip, s.m.ChipText = "bad", "receiver stopped"
	case strings.EqualFold(io, "Connecting"):
		s.m.Chip, s.m.ChipText = "bad", "disconnected"
	case !strings.EqualFold(sqlRun, "Yes"):
		s.m.Chip, s.m.ChipText = "bad", "applier stopped"
	case strings.Contains(ls, "global read lock"):
		s.m.Chip, s.m.ChipText = "bad", "waiting for global read lock"
	case strings.Contains(ls, "metadata lock"):
		s.m.Chip, s.m.ChipText = "bad", "waiting on metadata lock"
	default:
		lagChip(s)
	}
}

func (e *Engine) sampleGR(ctx context.Context, db *sql.DB, s *memberSample) {
	var role, state sql.NullString
	db.QueryRowContext(ctx, `SELECT MEMBER_ROLE, MEMBER_STATE FROM performance_schema.replication_group_members WHERE MEMBER_ID = @@server_uuid`).Scan(&role, &state)
	var applierQ, certQ sql.NullInt64
	db.QueryRowContext(ctx, `SELECT COUNT_TRANSACTIONS_REMOTE_IN_APPLIER_QUEUE, COUNT_TRANSACTIONS_IN_QUEUE
		FROM performance_schema.replication_group_member_stats WHERE MEMBER_ID = @@server_uuid`).Scan(&applierQ, &certQ)
	s.m.Role = strings.ToLower(role.String)
	if s.m.Role == "" {
		s.m.Role = "member"
	}
	s.writer = strings.EqualFold(role.String, "PRIMARY")
	s.m.RecvQ = float64(applierQ.Int64)
	s.m.Fields = [][2]string{
		{"MEMBER_STATE", state.String}, {"MEMBER_ROLE", role.String},
		{"applier queue", strconv.FormatInt(applierQ.Int64, 10)}, {"certification queue", strconv.FormatInt(certQ.Int64, 10)},
		{"flow_control_mode", globalVar(ctx, db, "group_replication_flow_control_mode")},
	}
	switch {
	case !strings.EqualFold(state.String, "ONLINE"):
		s.m.Chip, s.m.ChipText = "bad", strings.ToLower(nz(state.String, "not in the group"))
	case s.writer:
		s.m.Chip, s.m.ChipText, s.m.Lag = "ok", "primary", 0
	default:
		lagChip(s)
	}
}

func (e *Engine) sampleGalera(ctx context.Context, db *sql.DB, s *memberSample) {
	st := globalStatus(ctx, db, "wsrep_local_recv_queue", "wsrep_local_send_queue", "wsrep_flow_control_paused_ns",
		"wsrep_flow_control_sent", "wsrep_flow_control_recv", "wsrep_local_state_comment", "wsrep_cluster_size", "wsrep_cluster_status", "wsrep_ready")
	s.m.RecvQ, s.m.SendQ = num(st["wsrep_local_recv_queue"]), num(st["wsrep_local_send_queue"])
	s.m.Role = "node"
	s.writer = s.m.ID == e.firstMember()

	// The share of the last interval flow control held the cluster paused.
	paused, now := num(st["wsrep_flow_control_paused_ns"]), float64(time.Now().UnixNano())
	e.mu.Lock()
	prev, had := e.fcPrev[s.m.ID]
	e.fcPrev[s.m.ID] = [2]float64{paused, now}
	e.mu.Unlock()
	if had && now > prev[1] && paused >= prev[0] {
		s.fcPause = (paused - prev[0]) / (now - prev[1])
		if s.fcPause > 1 {
			s.fcPause = 1
		}
	}
	s.m.Fields = [][2]string{
		{"wsrep_local_state_comment", st["wsrep_local_state_comment"]},
		{"wsrep_cluster_status", st["wsrep_cluster_status"]},
		{"wsrep_cluster_size", st["wsrep_cluster_size"]},
		{"wsrep_local_recv_queue", st["wsrep_local_recv_queue"]},
		{"wsrep_local_send_queue", st["wsrep_local_send_queue"]},
		{"flow control paused", fmt.Sprintf("%.0f%%", s.fcPause*100)},
		{"wsrep_flow_control_sent", st["wsrep_flow_control_sent"]},
		{e.w("applier"), globalVar(ctx, db, e.w("applier"))},
	}
	switch {
	case st["wsrep_cluster_status"] != "" && st["wsrep_cluster_status"] != "Primary":
		s.m.Chip, s.m.ChipText, s.m.Evicted = "bad", "non-Primary", true
	case s.m.RecvQ >= fcLimit:
		s.m.Chip, s.m.ChipText = "bad", "sending flow control"
	case s.m.RecvQ > 20:
		s.m.Chip, s.m.ChipText = "warn", "queue building"
	case st["wsrep_local_state_comment"] != "" && st["wsrep_local_state_comment"] != "Synced":
		s.m.Chip, s.m.ChipText = "warn", strings.ToLower(st["wsrep_local_state_comment"])
	default:
		s.m.Chip, s.m.ChipText = "ok", "Synced"
	}
}

func (e *Engine) firstMember() string {
	// The application's endpoint is a member for Galera (DBCanvas links the
	// sim to the first node); fall back to the first member.
	for _, m := range e.cfg.Members {
		if m.Host == e.cfg.Writer.Host {
			return m.ID
		}
	}
	if len(e.cfg.Members) > 0 {
		return e.cfg.Members[0].ID
	}
	return ""
}

func lagChip(s *memberSample) {
	switch {
	case s.m.Lag > 30:
		s.m.Chip, s.m.ChipText = "bad", "lagging"
	case s.m.Lag > 5:
		s.m.Chip, s.m.ChipText = "warn", "behind"
	default:
		s.m.Chip, s.m.ChipText = "ok", "in sync"
	}
}

// --- small query helpers ---

func globalStatus(ctx context.Context, db *sql.DB, names ...string) map[string]string {
	out := map[string]string{}
	q := "SHOW GLOBAL STATUS WHERE Variable_name IN ('" + strings.Join(names, "','") + "')"
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

func globalVar(ctx context.Context, db *sql.DB, name string) string {
	var v sql.NullString
	if db.QueryRowContext(ctx, "SELECT @@GLOBAL."+name).Scan(&v) != nil {
		return ""
	}
	return v.String
}

// queryRow runs a SHOW statement and returns its first row by column name.
func queryRow(ctx context.Context, db *sql.DB, q string) (map[string]string, bool) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	if !rows.Next() {
		return nil, false
	}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if rows.Scan(ptrs...) != nil {
		return nil, false
	}
	out := map[string]string{}
	for i, c := range cols {
		if vals[i] == nil {
			out[c] = "NULL"
		} else {
			out[c] = string(vals[i])
		}
	}
	return out, true
}

func pickName(row map[string]string, names ...string) string {
	for _, n := range names {
		if _, ok := row[n]; ok {
			return n
		}
	}
	return names[0]
}

func num(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func onOff(s string) string {
	switch s {
	case "1":
		return "ON"
	case "0":
		return "OFF"
	}
	return s
}

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
