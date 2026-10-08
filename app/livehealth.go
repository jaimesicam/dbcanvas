package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// livehealth.go — what the Live view's popups say is wrong, beyond the role (livestate.go).
//
// Every check asks a node about its peers as the engine itself sees them, never as the design
// draws them. That is the rule which makes "a member was deleted" come out right: a MongoDB member
// removed from the canvas but still in the replica set's config is a member the others cannot
// reach, and is reported; one removed with rs.remove() is not a member any more, and is not. The
// same holds for a Valkey node still in CLUSTER NODES, a Group Replication peer still in the group,
// and a replication slot nobody is reading.
//
// The parsers are plain functions over what the engine printed or returned, so they are tested
// without a database (livehealth_test.go).

// problemMax bounds one problem line: an engine's error text can be a paragraph, and the popup is
// 196 pixels wide. The full text is in the line's tooltip as far as it goes.
const problemMax = 180

func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ------------------------------------------------------------------------------- MySQL

// liveMySQLProbe is one batch for every MySQL flavour — Percona Server, MySQL, MariaDB, PXC,
// Galera, Group Replication. The client runs with --vertical, so every answer comes back as
// uniform "name: value" lines under a row marker, and with --force, so a statement a flavour does
// not have (super_read_only on MariaDB, SHOW SLAVE on 8.4+, performance_schema on old MariaDB) is
// skipped, not fatal. Statements end in ";", not "\G": the 9.x client refuses client commands
// such as \G when it reads from a pipe, which failed the whole batch. Every Galera status is asked
// twice: performance_schema for PXC 8, information_schema for MariaDB.
const liveMySQLProbe = `SELECT @@global.read_only AS ro;
SELECT @@global.super_read_only AS sro;
SELECT VARIABLE_VALUE AS wsrep FROM performance_schema.global_status WHERE VARIABLE_NAME='wsrep_local_state_comment';
SELECT VARIABLE_VALUE AS wsrep FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='WSREP_LOCAL_STATE_COMMENT';
SELECT VARIABLE_VALUE AS wsrep_status FROM performance_schema.global_status WHERE VARIABLE_NAME='wsrep_cluster_status';
SELECT VARIABLE_VALUE AS wsrep_status FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='WSREP_CLUSTER_STATUS';
SELECT VARIABLE_VALUE AS wsrep_size FROM performance_schema.global_status WHERE VARIABLE_NAME='wsrep_cluster_size';
SELECT VARIABLE_VALUE AS wsrep_size FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='WSREP_CLUSTER_SIZE';
SELECT MEMBER_ROLE AS gr_role, MEMBER_STATE AS gr_self FROM performance_schema.replication_group_members WHERE MEMBER_ID=@@server_uuid;
SELECT MEMBER_HOST AS gr_host, MEMBER_PORT AS gr_port, MEMBER_STATE AS gr_state FROM performance_schema.replication_group_members WHERE MEMBER_ID<>@@server_uuid AND MEMBER_ID<>'';
SELECT COUNT(*) AS dumps FROM information_schema.PROCESSLIST WHERE COMMAND LIKE 'Binlog Dump%';
SELECT @@global.max_connections AS max_conn;
SHOW GLOBAL STATUS WHERE Variable_name IN ('Questions','Com_commit','Com_rollback','Threads_connected','Threads_running');
SHOW REPLICA STATUS;
SHOW SLAVE STATUS;
`

// mysqlLoad reads the probe's SHOW GLOBAL STATUS rows (Variable_name / Value) and max_connections.
// Commits are Com_commit + Com_rollback: explicit transactions — an autocommit statement is a
// query, not a commit, here as in PMM's MySQL dashboards. Threads_connected includes the probe's
// own connection and Threads_running the probe's own statement, so one is taken off each.
func mysqlLoad(blocks []map[string]string, f map[string]string) *liveLoad {
	st := map[string]int64{}
	for _, b := range blocks {
		if name := b["Variable_name"]; name != "" {
			if v, err := strconv.ParseInt(b["Value"], 10, 64); err == nil {
				st[name] = v
			}
		}
	}
	if len(st) == 0 {
		return nil
	}
	l := &liveLoad{}
	if q, ok := st["Questions"]; ok {
		l.Queries = &q
	}
	if c, ok := st["Com_commit"]; ok {
		c += st["Com_rollback"]
		l.Commits = &c
	}
	l.Conns = minusOne(st, "Threads_connected")
	l.Active = minusOne(st, "Threads_running")
	if n, err := strconv.Atoi(f["max_conn"]); err == nil {
		l.MaxConns = &n
	}
	return l
}

func minusOne(st map[string]int64, k string) *int {
	v, ok := st[k]
	if !ok {
		return nil
	}
	n := int(v) - 1
	if n < 0 {
		n = 0
	}
	return &n
}

// verticalBlocks splits mysql --vertical (or \G) output into its rows, one map per row.
func verticalBlocks(out string) []map[string]string {
	var blocks []map[string]string
	var cur map[string]string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "*") {
			cur = map[string]string{}
			blocks = append(blocks, cur)
			continue
		}
		k, v, ok := strings.Cut(t, ": ")
		if !ok {
			// "Last_IO_Error: " with nothing after it has no space to cut on.
			if strings.HasSuffix(t, ":") && !strings.Contains(t, " ") {
				k, v, ok = strings.TrimSuffix(t, ":"), "", true
			}
		}
		if !ok || cur == nil {
			continue
		}
		if _, seen := cur[k]; !seen {
			cur[k] = strings.TrimSpace(v)
		}
	}
	return blocks
}

// verticalFields reads every "name: value" line of mysql --vertical output; the first answer for a name
// wins (the statements are ordered so the preferred source of a value comes first).
func verticalFields(out string) map[string]string {
	f := map[string]string{}
	for _, b := range verticalBlocks(out) {
		for k, v := range b {
			if _, seen := f[k]; !seen {
				f[k] = v
			}
		}
	}
	return f
}

// replicaChannels picks the replication channels out of the batch's rows. 8.0 answers both SHOW
// REPLICA STATUS and the deprecated SHOW SLAVE STATUS, so the same channel can come back twice: the
// Source_* spelling wins, and the Master_* rows count only when there are no others (MariaDB, old
// MySQL). MariaDB 10.5+ answers both statements with Master_* fields, so the same channel is also
// kept only once by its source and name (Channel_Name on MySQL, Connection_name on MariaDB).
func replicaChannels(blocks []map[string]string) []map[string]string {
	var modern, legacy []map[string]string
	for _, b := range blocks {
		switch {
		case b["Source_Host"] != "":
			modern = append(modern, b)
		case b["Master_Host"] != "":
			legacy = append(legacy, b)
		}
	}
	rows := legacy
	if len(modern) > 0 {
		rows = modern
	}
	seen := map[string]bool{}
	var out []map[string]string
	for _, b := range rows {
		key := firstNonEmpty(b["Source_Host"], b["Master_Host"]) + "\x00" + b["Channel_Name"] + b["Connection_name"]
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, b)
	}
	return out
}

// parseMySQLLive reads the probe batch. expectMembers is how many members the design gives this
// node's Galera cluster (0 when it is not in one): a cluster seeing fewer has lost one.
func parseMySQLLive(out string, expectMembers int) *liveRole {
	blocks := verticalBlocks(out)
	f := verticalFields(out)
	r := &liveRole{Access: "rw", Load: mysqlLoad(blocks, f)}
	// 1 on MySQL; MariaDB 11.x+ answers OFF, ON, NO_LOCK or NO_LOCK_NO_ADMIN — every value but
	// OFF is read-only.
	if on := func(v string) bool { return v != "" && v != "0" && !strings.EqualFold(v, "OFF") }; on(f["ro"]) || on(f["sro"]) {
		r.Access = "ro"
	}
	channels := replicaChannels(blocks)
	switch {
	case f["wsrep"] != "":
		r.Role, r.State = "member", f["wsrep"]
		if st := f["wsrep_status"]; st != "" && st != "Primary" {
			r.Problems = append(r.Problems, fmt.Sprintf("cluster component is %s: this node cannot take writes", st))
		}
		if f["wsrep"] != "Synced" {
			r.Problems = append(r.Problems, "not synced with the cluster ("+f["wsrep"]+")")
		}
		if n, err := strconv.Atoi(f["wsrep_size"]); err == nil && expectMembers > 0 && n < expectMembers {
			r.Problems = append(r.Problems, fmt.Sprintf("sees %d of %d cluster members", n, expectMembers))
		}
	case f["gr_role"] != "":
		r.Role = map[string]string{"PRIMARY": "primary", "SECONDARY": "secondary"}[f["gr_role"]]
		if r.Role == "" {
			r.Role = "member"
		}
		if s := f["gr_self"]; s != "" && s != "ONLINE" {
			r.State = strings.ToLower(s)
			r.Problems = append(r.Problems, "group member state is "+s)
		}
		for _, b := range blocks {
			if b["gr_host"] == "" || b["gr_state"] == "ONLINE" {
				continue
			}
			r.Problems = append(r.Problems, fmt.Sprintf("group member %s:%s is %s", b["gr_host"], b["gr_port"], b["gr_state"]))
		}
	case len(channels) > 0:
		r.Role = "replica"
	default:
		r.Role = "standalone"
		if n, _ := strconv.Atoi(f["dumps"]); n > 0 {
			r.Role, r.Replicas = "primary", &n
		}
	}

	// Every channel is checked whatever the role: a PXC member can also be the replica end of a
	// cross-cluster link (replication.go), and that channel breaking is the problem worth saying.
	running := 0
	if len(channels) > 0 {
		r.Source = firstNonEmpty(channels[0]["Source_Host"], channels[0]["Master_Host"])
	}
	for _, c := range channels {
		host := firstNonEmpty(c["Source_Host"], c["Master_Host"])
		where := host
		if ch := c["Channel_Name"]; ch != "" {
			where += " (channel " + ch + ")"
		}
		io := firstNonEmpty(c["Replica_IO_Running"], c["Slave_IO_Running"])
		sql := firstNonEmpty(c["Replica_SQL_Running"], c["Slave_SQL_Running"])
		if io == "Yes" && sql == "Yes" {
			running++
		}
		if io != "Yes" {
			p := "replication from " + where + ": receiver "
			if io == "Connecting" {
				p += "cannot connect to the source"
			} else {
				p += "stopped"
			}
			if e := c["Last_IO_Error"]; e != "" {
				p += " — " + e
			}
			r.Problems = append(r.Problems, clipLine(p, problemMax))
		}
		if sql != "Yes" {
			p := "replication from " + where + ": applier stopped"
			if e := c["Last_SQL_Error"]; e != "" {
				p += " — " + e
			}
			r.Problems = append(r.Problems, clipLine(p, problemMax))
		}
		if lag, err := strconv.ParseFloat(firstNonEmpty(c["Seconds_Behind_Source"], c["Seconds_Behind_Master"]), 64); err == nil {
			if r.LagSec == nil || lag > *r.LagSec {
				r.LagSec = &lag
			}
		}
	}
	if r.Role == "replica" {
		switch {
		case running == len(channels):
			r.State = "replicating"
		case running == 0:
			r.State = "broken"
		default:
			r.State = fmt.Sprintf("%d of %d channels running", running, len(channels))
		}
	}
	return r
}

// ---------------------------------------------------------------------------- PostgreSQL

// liveProbePG is the PostgreSQL question. receiver is the WAL receiver's status on a replica (null
// when it has none — not streaming); idle_slots are replication slots no client is reading, which
// on a primary means a replica that has gone away while WAL is still kept for it.
const liveProbePG = `SELECT json_build_object(
  'rec', pg_is_in_recovery(),
  'ro', current_setting('default_transaction_read_only'),
  'senders', (SELECT count(*) FROM pg_stat_replication),
  'lag', CASE WHEN NOT pg_is_in_recovery() THEN NULL
              WHEN pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn() THEN 0
              ELSE EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp()) END,
  'receiver', (SELECT status FROM pg_stat_wal_receiver LIMIT 1),
  'sender', (SELECT sender_host FROM pg_stat_wal_receiver LIMIT 1),
  'idle_slots', (SELECT coalesce(json_agg(slot_name ORDER BY slot_name), '[]'::json) FROM pg_replication_slots WHERE NOT active),
  'xact', (SELECT sum(xact_commit + xact_rollback) FROM pg_stat_database),
  'conns', (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()),
  'active', (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND state = 'active' AND pid <> pg_backend_pid()),
  'max_conn', current_setting('max_connections')::int)`

type pgLive struct {
	Rec       bool     `json:"rec"`
	RO        string   `json:"ro"`
	Senders   int      `json:"senders"`
	Lag       *float64 `json:"lag"`
	Receiver  *string  `json:"receiver"`
	Sender    *string  `json:"sender"`
	IdleSlots []string `json:"idle_slots"`
	// PostgreSQL counts transactions, not statements (that needs pg_stat_statements), so a
	// PostgreSQL node has TPS and no QPS.
	Xact    *int64 `json:"xact"`
	Conns   *int   `json:"conns"`
	Active  *int   `json:"active"`
	MaxConn *int   `json:"max_conn"`
}

func pgLiveRole(p pgLive) *liveRole {
	r := &liveRole{Access: "rw"}
	if p.Xact != nil || p.Conns != nil {
		r.Load = &liveLoad{Commits: p.Xact, Conns: p.Conns, Active: p.Active, MaxConns: p.MaxConn}
	}
	if p.Rec || p.RO == "on" {
		r.Access = "ro"
	}
	switch {
	case p.Rec:
		r.Role, r.LagSec = "replica", p.Lag
		if p.Sender != nil {
			r.Source = *p.Sender
		}
		switch {
		case p.Receiver == nil:
			r.State = "not streaming"
			r.Problems = append(r.Problems, "not streaming from the primary: no WAL receiver is running")
		case *p.Receiver != "streaming":
			r.State = *p.Receiver
			r.Problems = append(r.Problems, "WAL receiver is "+*p.Receiver+", not streaming")
		}
	case p.Senders > 0:
		r.Role, r.Replicas = "primary", &p.Senders
	default:
		r.Role = "standalone"
	}
	// Only a primary's idle slots mean a lost replica; a standby can hold copies of them.
	if !p.Rec {
		for _, s := range p.IdleSlots {
			r.Problems = append(r.Problems, "replication slot "+s+" has no replica connected (WAL is kept for it)")
		}
		if len(p.IdleSlots) > 0 && r.Role == "standalone" {
			r.Role = "primary"
			zero := 0
			r.Replicas = &zero
		}
	}
	return r
}

// ------------------------------------------------------------------------------- MongoDB

type mongoMember struct {
	Name     string    `bson:"name"`
	Health   float64   `bson:"health"`
	StateStr string    `bson:"stateStr"`
	Self     bool      `bson:"self"`
	Msg      string    `bson:"lastHeartbeatMessage"`
	Optime   time.Time `bson:"optimeDate"`
	// SyncSource is the member this one copies from ("host:port"), empty on the primary.
	SyncSource string `bson:"syncSourceHost"`
}

// mongoBadStates are member states that mean the member is not serving its part of the set.
var mongoBadStates = map[string]bool{
	"STARTUP": true, "STARTUP2": true, "RECOVERING": true, "ROLLBACK": true,
	"UNKNOWN": true, "DOWN": true, "REMOVED": true,
}

// mongoReplHealth reads replSetGetStatus's members as this node sees them. A member in the set's
// config that does not answer heartbeats is one this node cannot reach — whether it crashed or its
// node was deleted without rs.remove(). A member removed from the config is simply not listed.
func mongoReplHealth(members []mongoMember) (problems []string, lag *float64) {
	var self, primary *mongoMember
	for i := range members {
		m := &members[i]
		if m.Self {
			self = m
		}
		if m.StateStr == "PRIMARY" {
			primary = m
		}
	}
	for _, m := range members {
		switch {
		case m.Self:
			if mongoBadStates[m.StateStr] {
				problems = append(problems, "this member is "+m.StateStr)
			}
		case m.Health == 0:
			p := "cannot reach " + m.Name
			if m.Msg != "" {
				p += " — " + m.Msg
			}
			problems = append(problems, clipLine(p, problemMax))
		case mongoBadStates[m.StateStr]:
			problems = append(problems, m.Name+" is "+m.StateStr)
		}
	}
	if primary == nil && len(members) > 0 {
		problems = append(problems, "no primary: the replica set cannot take writes")
	}
	if self != nil && primary != nil && self.StateStr == "SECONDARY" && !primary.Optime.IsZero() && !self.Optime.IsZero() {
		d := primary.Optime.Sub(self.Optime).Seconds()
		if d < 0 {
			d = 0
		}
		lag = &d
	}
	return problems, lag
}

// -------------------------------------------------------------------------------- Valkey

// liveValkeyScript asks a Valkey node everything in one exec: its replication, and — for a cluster
// member — the cluster's state and every node as this one sees it. The sections are separated by a
// marker line, since valkey-cli's own output has no structure to split on.
const liveValkeyScript = `valkey-cli --no-auth-warning INFO replication 2>&1
valkey-cli --no-auth-warning INFO clients 2>/dev/null | grep -E '^(connected_clients|blocked_clients):'
valkey-cli --no-auth-warning INFO stats 2>/dev/null | grep '^total_commands_processed:'
echo "maxclients:$(valkey-cli --no-auth-warning CONFIG GET maxclients 2>/dev/null | tail -n 1)"
if [ "$CLUSTER" = 1 ]; then
  echo '#==cluster-info'
  valkey-cli --no-auth-warning CLUSTER INFO 2>&1
  echo '#==cluster-nodes'
  valkey-cli --no-auth-warning CLUSTER NODES 2>&1
fi`

func kvLines(s string) map[string]string {
	f := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(strings.TrimSuffix(line, "\r")), ":"); ok {
			f[k] = v
		}
	}
	return f
}

// parseValkeyLive reads liveValkeyScript's output. A node that does not answer INFO at all is
// down; the error text it printed is the reason.
func parseValkeyLive(out string) *liveRole {
	repl, rest, _ := strings.Cut(out, "#==cluster-info")
	cinfo, cnodes, _ := strings.Cut(rest, "#==cluster-nodes")
	f := kvLines(repl)
	var r *liveRole
	switch f["role"] {
	case "slave", "replica":
		r = &liveRole{Role: "replica", Access: "rw", State: f["master_link_status"], Source: f["master_host"]}
		if f["slave_read_only"] == "1" || f["replica_read_only"] == "1" {
			r.Access = "ro"
		}
		if st := f["master_link_status"]; st != "" && st != "up" {
			p := fmt.Sprintf("link to primary %s:%s is %s", f["master_host"], f["master_port"], st)
			if s := f["master_link_down_since_seconds"]; s != "" && s != "-1" {
				p += " (for " + s + "s)"
			}
			r.Problems = append(r.Problems, p)
		}
	case "master":
		n, _ := strconv.Atoi(f["connected_slaves"])
		r = &liveRole{Role: "standalone", Access: "rw"}
		if n > 0 {
			r.Role, r.Replicas = "primary", &n
		}
	default:
		msg, _, _ := strings.Cut(strings.TrimSpace(repl), "\n")
		return &liveRole{Down: true, Err: clipLine(firstNonEmpty(msg, "valkey-cli did not answer"), problemMax)}
	}
	r.Load = valkeyLoad(f)
	if strings.TrimSpace(cinfo) != "" {
		if st := kvLines(cinfo)["cluster_state"]; st != "" && st != "ok" {
			r.Problems = append(r.Problems, "cluster state is "+st+": not every slot is served")
		}
		r.Problems = append(r.Problems, valkeyNodeProblems(cnodes)...)
	}
	return r
}

// valkeyLoad: Valkey counts commands; it has no transactions to count. The probe's own
// valkey-cli is one of the connected clients.
func valkeyLoad(f map[string]string) *liveLoad {
	l := &liveLoad{}
	if v, err := strconv.ParseInt(strings.TrimSpace(f["total_commands_processed"]), 10, 64); err == nil {
		l.Queries = &v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(f["connected_clients"])); err == nil {
		v--
		if v < 0 {
			v = 0
		}
		l.Conns = &v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(f["maxclients"])); err == nil {
		l.MaxConns = &v
	}
	if l.Queries == nil && l.Conns == nil {
		return nil
	}
	return l
}

// valkeyNodeProblems reads CLUSTER NODES: "<id> <ip:port@cport[,host]> <flags> <master> <ping> <pong>
// <epoch> <link> <slots…>". A node flagged fail is one the cluster agrees it cannot reach; fail?
// is this node's suspicion alone. A node removed with CLUSTER FORGET is not listed.
func valkeyNodeProblems(out string) []string {
	var problems []string
	for _, line := range strings.Split(out, "\n") {
		fs := strings.Fields(strings.TrimSuffix(line, "\r"))
		if len(fs) < 8 {
			continue
		}
		ep, host, _ := strings.Cut(fs[1], ",")
		ep, _, _ = strings.Cut(ep, "@") // drop the bus port
		addr := ep
		if host != "" {
			addr = host + " (" + ep + ")"
		}
		flags := map[string]bool{}
		for _, fl := range strings.Split(fs[2], ",") {
			flags[fl] = true
		}
		switch {
		case flags["myself"]:
			continue
		case flags["fail"]:
			problems = append(problems, "cannot reach "+addr+": the cluster has marked it failed")
		case flags["fail?"]:
			problems = append(problems, "cannot reach "+addr+" (suspected failing)")
		case flags["noaddr"]:
			problems = append(problems, addr+" has no known address")
		}
	}
	sort.Strings(problems)
	return problems
}

// ------------------------------------------------------------------------- down or failing

// mysqlUnreachable is a client error that means the server did not answer at all — as opposed to a
// query it refused, which is the probe's problem, not the database's. 2002/2003: no socket or no
// listener; 2006/2013: the server went away mid-conversation.
func mysqlUnreachable(stderr string) bool {
	for _, code := range []string{"ERROR 2002", "ERROR 2003", "ERROR 2006", "ERROR 2013", "Can't connect", "Lost connection"} {
		if strings.Contains(stderr, code) {
			return true
		}
	}
	return false
}

// pgUnreachable is psql's way of saying the same: no server on the socket or port, or one that is
// starting up or shutting down and refusing connections.
func pgUnreachable(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"could not connect", "connection to server", "no such file or directory",
		"the database system is starting up", "the database system is shutting down", "server closed the connection"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
