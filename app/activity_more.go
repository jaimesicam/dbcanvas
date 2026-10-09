package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
)

// activity_more.go — the Activity view (activity.go) for MongoDB and the two proxies.

// ------------------------------------------------------------------------------- MongoDB

// mongoActivity is currentOp for the operations that are doing something — active, or holding a
// transaction open — never every idle connection.
func (a *App) mongoActivity(ctx context.Context, c dbConn) (*actSnapshot, error) {
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return nil, err
	}
	defer closer()
	cmd := bson.D{{Key: "currentOp", Value: 1}, {Key: "$all", Value: true},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "active", Value: true}, {Key: "op", Value: bson.D{{Key: "$ne", Value: "none"}}}},
			bson.D{{Key: "transaction", Value: bson.D{{Key: "$exists", Value: true}}}},
		}}}
	var res struct {
		Inprog []bson.M `bson:"inprog"`
	}
	if err := client.Database("admin").RunCommand(ctx, cmd).Decode(&res); err != nil {
		return nil, err
	}
	return parseMongoActivity(res.Inprog), nil
}

func parseMongoActivity(ops []bson.M) *actSnapshot {
	s := &actSnapshot{Engine: "mongodb", Sessions: []*actSession{}, Waits: []actWait{}, DDL: []actDDL{}}
	str := func(v any) string {
		switch t := v.(type) {
		case string:
			return t
		case nil:
			return ""
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	num := func(v any) float64 {
		switch t := v.(type) {
		case int32:
			return float64(t)
		case int64:
			return float64(t)
		case float64:
			return t
		}
		return 0
	}
	for _, op := range ops {
		desc := str(op["desc"])
		// The server's own background work (replication, TTL monitor, …) is not a client.
		if strings.HasPrefix(desc, "conn") == false && op["client"] == nil && op["transaction"] == nil {
			continue
		}
		x := &actSession{ID: strconv.FormatInt(int64(num(op["opid"])), 10), Command: str(op["op"]), DB: str(op["ns"]),
			Host: str(op["client"]), App: str(op["appName"]), State: str(op["msg"]), Killable: op["opid"] != nil}
		if us, ok := op["effectiveUsers"].(bson.A); ok && len(us) > 0 {
			if u, ok := us[0].(bson.M); ok {
				x.User = str(u["user"])
			}
		}
		if x.User == "__system" {
			continue // a member's replication or heartbeat traffic
		}
		x.TimeSec = num(op["microsecs_running"]) / 1e6
		if cmd, ok := op["command"].(bson.M); ok {
			if cmd["currentOp"] != nil || cmd["hello"] != nil || cmd["isMaster"] != nil || cmd["ismaster"] != nil {
				continue // this snapshot itself, and drivers' long-polled topology checks
			}
			x.Statement = clipLine(mongoStatement(cmd), 4000)
		} else if cmd, ok := op["command"]; ok {
			x.Statement = clipLine(str(cmd), 4000)
		}
		if t, ok := op["transaction"].(bson.M); ok {
			age := num(t["timeOpenMicros"]) / 1e6
			x.TrxAge = &age
			if op["active"] != true {
				x.Command = "idle in transaction"
			}
		}
		if op["waitingForLock"] == true {
			x.Waiting = true
			w := actWait{Waiter: x.ID, Kind: "lock", Object: x.DB}
			t := x.TimeSec
			w.WaitSec = &t
			s.Waits = append(s.Waits, w)
		}
		s.Sessions = append(s.Sessions, x)
		// Index builds report their progress.
		if p, ok := op["progress"].(bson.M); ok {
			done, total := num(p["done"]), num(p["total"])
			s.DDL = append(s.DDL, actDDL{Session: x.ID, Statement: x.Statement, Phase: x.State, Done: &done, Total: &total, TimeSec: x.TimeSec})
		} else if strings.Contains(x.Statement, `"createIndexes"`) || strings.Contains(x.Statement, `"dropIndexes"`) || strings.Contains(x.Statement, `"collMod"`) {
			s.DDL = append(s.DDL, actDDL{Session: x.ID, Statement: x.Statement, Phase: x.State, TimeSec: x.TimeSec})
		}
	}
	return s
}

// mongoCommands are the command names an op's document leads with; currentOp returns the
// document as a map, so the name is found rather than taken from the first key.
var mongoCommands = []string{"find", "aggregate", "update", "delete", "insert", "findAndModify", "count", "distinct", "getMore",
	"createIndexes", "dropIndexes", "collMod", "create", "drop", "renameCollection", "mapReduce", "explain", "compact",
	"validate", "commitTransaction", "abortTransaction", "bulkWrite"}

// mongoNoise is what a driver adds to every command; none of it says what the op does.
var mongoNoise = map[string]bool{"$db": true, "$clusterTime": true, "lsid": true, "$readPreference": true, "signature": true,
	"txnNumber": true, "autocommit": true, "startTransaction": true, "apiVersion": true, "apiStrict": true, "$audit": true,
	"$client": true, "$configTime": true, "$topologyTime": true, "maxTimeMS": true, "readConcern": true, "writeConcern": true}

// mongoStatement renders an op's command as `find orders {"filter":…}`.
func mongoStatement(cmd bson.M) string {
	name, target := "", ""
	for _, c := range mongoCommands {
		if v, ok := cmd[c]; ok {
			name = c
			if t, ok := v.(string); ok {
				target = t
			}
			break
		}
	}
	rest := bson.M{}
	for k, v := range cmd {
		if k != name && !mongoNoise[k] {
			rest[k] = v
		}
	}
	b, _ := json.Marshal(rest)
	return strings.TrimSpace(strings.Join([]string{name, target, string(b)}, " "))
}

func (a *App) mongoKillOp(ctx context.Context, c dbConn, op string) error {
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return err
	}
	defer closer()
	id, err := strconv.ParseInt(op, 10, 64)
	if err != nil {
		return err
	}
	return client.Database("admin").RunCommand(ctx, bson.D{{Key: "killOp", Value: 1}, {Key: "op", Value: id}}).Err()
}

// ------------------------------------------------------------------------------ ProxySQL

const proxysqlActivitySQL = `SELECT SessionID, user, db, cli_host, cli_port, hostgroup, l_srv_host, l_srv_port, srv_host, srv_port, command, time_ms, IFNULL(info,'') FROM stats_mysql_processlist;
SELECT '##POOL##';
SELECT hostgroup, srv_host, srv_port, status, ConnUsed, ConnFree, ConnOK, ConnERR, Queries, Latency_us FROM stats_mysql_connection_pool;
`

const proxysqlDigestSQL = `SELECT '##DIGEST##';
SELECT digest, schemaname, count_star, sum_time, sum_rows_sent, sum_rows_affected, REPLACE(REPLACE(SUBSTR(digest_text,1,1500), CHAR(10), ' '), CHAR(9), ' ') FROM stats_mysql_query_digest ORDER BY sum_time DESC LIMIT 40;
`

// actPool is a connection pool row: ProxySQL's per backend, PgBouncer's per database/user.
type actPool struct {
	Name    string  `json:"name"`
	Backend string  `json:"backend,omitempty"`
	Status  string  `json:"status,omitempty"`
	Used    float64 `json:"used"`
	Free    float64 `json:"free"`
	Waiting float64 `json:"waiting"`
	Errors  float64 `json:"errors,omitempty"`
	Queries float64 `json:"queries,omitempty"`
	Latency float64 `json:"latencyMs,omitempty"`
	MaxWait float64 `json:"maxWaitSec,omitempty"`
}

func (a *App) proxysqlActivity(ctx context.Context, st Stack, nid string, with map[string]bool) (*actSnapshot, error) {
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
		return nil, errors.New("node is not running")
	}
	var sec proxysqlSecrets
	json.Unmarshal(dep.Secrets, &sec)
	user, pw := firstNonEmpty(sec.AdminUser, "admin"), firstNonEmpty(sec.AdminPassword, proxysqlAdminPassword())
	sql := proxysqlActivitySQL
	if with["digests"] {
		sql += proxysqlDigestSQL
	}
	res, err := a.engCtx(ctx).ExecInput(ctx, dep.ContainerID, "", []string{"mysql", "--no-defaults", "-u" + user, "-h127.0.0.1", "-P6032", "--protocol=tcp", "-N", "-B", "--force"},
		[]string{"MYSQL_PWD=" + pw}, []byte(sql))
	if err != nil {
		return nil, err
	}
	snap := parseProxySQLActivity(res.Stdout)
	a.linkBackends(st, snap)
	return snap, nil
}

// Proxy carries what only a proxy has: its pools, and the backend each session is on.
type actProxy struct {
	Pools []actPool `json:"pools"`
}

func parseProxySQLActivity(out string) *actSnapshot {
	s := &actSnapshot{Engine: "proxysql", Sessions: []*actSession{}, Waits: []actWait{}, DDL: []actDDL{}}
	part := "sessions"
	f := func(v string) float64 { x, _ := strconv.ParseFloat(v, 64); return x }
	for _, line := range strings.Split(out, "\n") {
		switch strings.TrimSpace(line) {
		case "##POOL##":
			part = "pool"
			continue
		case "##DIGEST##":
			part = "digest"
			continue
		case "":
			continue
		}
		c := strings.Split(line, "\t")
		switch part {
		case "sessions":
			if len(c) < 13 {
				continue
			}
			x := &actSession{ID: c[0], User: c[1], DB: c[2], Host: c[3] + ":" + c[4], Command: c[10], TimeSec: f(c[11]) / 1000,
				Statement: c[12], Killable: true}
			if c[8] != "" && c[8] != "NULL" {
				x.State = fmt.Sprintf("hostgroup %s → %s:%s", c[5], c[8], c[9])
				x.Backend = &actBackend{Host: c[8], Port: c[9], LocalPort: c[7]}
			} else {
				x.State = "hostgroup " + c[5] + " (no backend connection)"
			}
			s.Sessions = append(s.Sessions, x)
		case "pool":
			if len(c) < 10 {
				continue
			}
			if s.Proxy == nil {
				s.Proxy = &actProxy{}
			}
			s.Proxy.Pools = append(s.Proxy.Pools, actPool{Name: "hostgroup " + c[0], Backend: c[1] + ":" + c[2], Status: c[3],
				Used: f(c[4]), Free: f(c[5]), Errors: f(c[7]), Queries: f(c[8]), Latency: f(c[9]) / 1000})
		case "digest":
			if len(c) < 7 {
				continue
			}
			s.Digests = append(s.Digests, actDigest{ID: c[0], Schema: c[1], Calls: f(c[2]), TimeSec: f(c[3]) / 1e6, RowsOut: f(c[4]), Text: c[6]})
		}
	}
	return s
}

// linkBackends resolves each proxy session's backend host to the node it is, so the panel can
// open that node's Activity on the session serving it (the backend sees the proxy's connection
// as host:port, and l_srv_port is the proxy's side of it).
func (a *App) linkBackends(st Stack, s *actSnapshot) {
	doc := buildDoc(st)
	hosts := stackHostnames(doc)
	byHost := map[string]string{}
	for id, h := range hosts {
		byHost[strings.ToLower(h)] = id
	}
	for _, x := range s.Sessions {
		if x.Backend == nil {
			continue
		}
		h := strings.ToLower(x.Backend.Host)
		short, _, _ := strings.Cut(h, ".")
		x.Backend.Node = firstNonEmpty(byHost[h], byHost[short])
		if x.Backend.Node != "" {
			x.Backend.Label = nodeLabel(doc, x.Backend.Node)
		}
	}
}

type actBackend struct {
	Host      string `json:"host"`
	Port      string `json:"port"`
	LocalPort string `json:"localPort,omitempty"`
	Node      string `json:"node,omitempty"`
	Label     string `json:"label,omitempty"`
}

func (a *App) proxysqlKill(ctx context.Context, st Stack, nid, session string, conn bool) error {
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil || dep.ContainerID == "" {
		return errors.New("node is not running")
	}
	var sec proxysqlSecrets
	json.Unmarshal(dep.Secrets, &sec)
	user, pw := firstNonEmpty(sec.AdminUser, "admin"), firstNonEmpty(sec.AdminPassword, proxysqlAdminPassword())
	// ProxySQL kills the client session; there is no kill-query of its own.
	res, err := a.engCtx(ctx).ExecInput(ctx, dep.ContainerID, "", []string{"mysql", "--no-defaults", "-u" + user, "-h127.0.0.1", "-P6032", "--protocol=tcp"},
		[]string{"MYSQL_PWD=" + pw}, []byte("KILL CONNECTION "+session+";\n"))
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("%s", lastLines(strings.TrimSpace(res.Stderr), 200))
	}
	return nil
}

// ----------------------------------------------------------------------------- PgBouncer

// The pool's own deployment keeps no password; the watcher's env file has the account
// PgBouncer authenticates as, which is also its admin_users entry.
const pgbouncerActivityScript = `if [ -z "$PW" ] && [ -r /etc/pgbouncer/backend.env ]; then . /etc/pgbouncer/backend.env; U="$PGUSER"; PW="$PGPASSWORD"; fi
export PGPASSWORD="$PW"
q() { psql -h 127.0.0.1 -p 6432 -U "$U" -X -A -F '	' -P footer=off pgbouncer -c "$1" 2>/dev/null || psql -h /var/run/pgbouncer -p 6432 -U "$U" -X -A -F '	' -P footer=off pgbouncer -c "$1"; }
echo '##POOLS##'; q 'SHOW POOLS'
echo '##CLIENTS##'; q 'SHOW CLIENTS'`

func (a *App) pgbouncerActivity(ctx context.Context, st Stack, nid string) (*actSnapshot, error) {
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
		return nil, errors.New("node is not running")
	}
	var sec pgSecrets
	json.Unmarshal(dep.Secrets, &sec)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"sh", "-c", pgbouncerActivityScript},
		[]string{"U=" + firstNonEmpty(sec.SuperUser, "postgres"), "PW=" + sec.SuperPassword})
	if err != nil {
		return nil, err
	}
	return parsePgBouncerActivity(res.Stdout), nil
}

// parsePgBouncerActivity reads SHOW POOLS and SHOW CLIENTS, each led by its header line: the
// columns differ between PgBouncer releases, so they are read by name. Clients waiting for a
// server connection are the pool's own blocking: the pool is exhausted.
func parsePgBouncerActivity(out string) *actSnapshot {
	s := &actSnapshot{Engine: "pgbouncer", Sessions: []*actSession{}, Waits: []actWait{}, DDL: []actDDL{}, Proxy: &actProxy{}}
	part := ""
	var head map[string]int
	for _, line := range strings.Split(out, "\n") {
		switch strings.TrimSpace(line) {
		case "##POOLS##":
			part, head = "pools", nil
			continue
		case "##CLIENTS##":
			part, head = "clients", nil
			continue
		case "":
			continue
		}
		c := strings.Split(line, "\t")
		if head == nil {
			head = map[string]int{}
			for i, h := range c {
				head[strings.TrimSpace(h)] = i
			}
			continue
		}
		col := func(name string) string {
			if i, ok := head[name]; ok && i < len(c) {
				return c[i]
			}
			return ""
		}
		f := func(name string) float64 { x, _ := strconv.ParseFloat(col(name), 64); return x }
		switch part {
		case "pools":
			if col("database") == "pgbouncer" {
				continue
			}
			p := actPool{Name: col("database") + " / " + col("user"), Waiting: f("cl_waiting"), Used: f("sv_active"),
				Free: f("sv_idle") + f("sv_used"), MaxWait: f("maxwait") + f("maxwait_us")/1e6, Status: col("pool_mode")}
			s.Proxy.Pools = append(s.Proxy.Pools, p)
		case "clients":
			if col("database") == "pgbouncer" {
				continue
			}
			x := &actSession{ID: firstNonEmpty(col("id"), col("ptr")), User: col("user"), DB: col("database"), Command: col("state"),
				Host: col("addr") + ":" + col("port"), App: col("application_name"), TimeSec: f("wait") + f("wait_us")/1e6}
			if x.Command == "waiting" {
				x.Waiting = true
				t := x.TimeSec
				s.Waits = append(s.Waits, actWait{Waiter: x.ID, Kind: "pool", Object: x.DB, Mode: "waiting for a server connection", WaitSec: &t})
			}
			s.Sessions = append(s.Sessions, x)
		}
	}
	return s
}

// ------------------------------------------------------------------------- deep mode

// deepState is a node's deep instrumentation, while it is on.
type deepState struct {
	Until   int64    `json:"untilMs"`
	Since   int64    `json:"sinceMs"`
	Enabled []string `json:"enabled"` // what it switched on, which is what it switches back off
	QPSBase *float64 `json:"qpsBefore,omitempty"`
}

// deepConsumers and deepInstruments are what deep mode switches on in performance_schema: stage
// events (DDL progress), current waits (what a session waits on, beyond locks) and socket I/O.
// Only those that were off are switched on, and only those are switched back.
var deepConsumers = []string{"events_stages_current", "events_waits_current", "events_transactions_current"}

// Not wait/synch/%: mutex and rwlock instrumentation is the kind that does slow a busy server.
var deepInstruments = []string{"stage/%", "wait/io/socket/%", "transaction"}
