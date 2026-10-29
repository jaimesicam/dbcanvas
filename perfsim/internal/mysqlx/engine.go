// Package mysqlx is Performance Sim's MySQL-family engine: Percona Server,
// MySQL and MariaDB, standalone or as asynchronous replication, Group
// Replication or Galera (PXC, MariaDB Galera).
package mysqlx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"

	"perfsim/internal/core"
)

// Config is what DBCanvas resolved for the linked target: every member with its
// role at deploy time, the endpoint the application writes to (a member, or a
// router in front of the cluster), and an account allowed to do what the dials
// do (FLUSH TABLES WITH READ LOCK, STOP REPLICA, SET GLOBAL). The workload
// may use a different account (WriterUser): a router such as ProxySQL only
// knows the application's.
type Config struct {
	Name           string         `json:"name"`
	Kind           string         `json:"kind"`
	User           string         `json:"user"`
	Password       string         `json:"password"`
	WriterUser     string         `json:"writerUser,omitempty"`
	WriterPassword string         `json:"writerPassword,omitempty"`
	Writer         Endpoint       `json:"writer"`
	Members        []MemberConfig `json:"members"`
}

type Endpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type MemberConfig struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Port int    `json:"port"`
	Role string `json:"role"`
}

const schema = "perfsim"

// ordersRows is the size of the workload's own table. Writes upsert inside this
// id range, so the table stays the same size however long the sim runs.
const ordersRows = 100000

type Engine struct {
	cfg    Config
	writer atomic.Pointer[sql.DB] // the application's endpoint: workload and dials that act on the writer
	hb     atomic.Pointer[sql.DB] // heartbeat only, so a busy workload pool never delays it
	mdb    map[string]*sql.DB
	// writerHost is where writer and hb point now. It starts as the configured
	// endpoint and moves only on Group Replication reached without a router: see
	// followPrimary.
	writerHost string

	mu       sync.Mutex
	flavor   string // percona | mysql | mariadb
	version  string
	topology string // standalone | async | gr | galera
	writerID string
	lag      map[string]float64
	fcPrev   map[string][2]float64 // flow-control paused ns and wall ns at the last sample
	vars     map[string]string     // flavor-dependent statement words, see words()
	applier  map[string]*applierState
}

func New(cfg Config) *Engine {
	e := &Engine{cfg: cfg, mdb: map[string]*sql.DB{}, lag: map[string]float64{}, fcPrev: map[string][2]float64{}}
	wu, wp := cfg.User, cfg.Password
	if cfg.WriterUser != "" {
		wu, wp = cfg.WriterUser, cfg.WriterPassword
	}
	e.writer.Store(open(wu, wp, cfg.Writer.Host, cfg.Writer.Port, 80))
	e.hb.Store(open(wu, wp, cfg.Writer.Host, cfg.Writer.Port, 2))
	e.writerHost = cfg.Writer.Host
	for _, m := range cfg.Members {
		e.mdb[m.ID] = open(cfg.User, cfg.Password, m.Host, m.Port, 12)
	}
	return e
}

func open(user, pass, host string, port, pool int) *sql.DB {
	c := mysql.NewConfig()
	c.User, c.Passwd, c.Net = user, pass, "tcp"
	c.Addr = fmt.Sprintf("%s:%d", host, port)
	c.Timeout = 3 * time.Second
	c.ParseTime = true
	c.InterpolateParams = true
	c.AllowNativePasswords = true
	db, _ := sql.Open("mysql", c.FormatDSN())
	db.SetMaxOpenConns(pool)
	db.SetMaxIdleConns(pool)
	db.SetConnMaxLifetime(10 * time.Minute)
	return db
}

func (e *Engine) wdb() *sql.DB  { return e.writer.Load() }
func (e *Engine) hbdb() *sql.DB { return e.hb.Load() }

// followPrimary points the workload and the heartbeat at the current primary
// when the sim writes to Group Replication members directly, with no router to
// find the primary for it: after a failover, or when the member it was given
// was not the primary. Through a router or a proxy the endpoint already follows
// the primary, and nothing changes.
func (e *Engine) followPrimary(primary string) {
	var host string
	direct := false
	for _, m := range e.cfg.Members {
		if m.ID == primary {
			host = m.Host
		}
		if m.Host == e.cfg.Writer.Host && m.Port == e.cfg.Writer.Port {
			direct = true
		}
	}
	e.mu.Lock()
	if !direct || host == "" || host == e.writerHost {
		e.mu.Unlock()
		return
	}
	e.writerHost = host
	e.mu.Unlock()
	wu, wp := e.cfg.User, e.cfg.Password
	if e.cfg.WriterUser != "" {
		wu, wp = e.cfg.WriterUser, e.cfg.WriterPassword
	}
	oldW := e.writer.Swap(open(wu, wp, host, e.cfg.Writer.Port, 80))
	oldH := e.hb.Swap(open(wu, wp, host, e.cfg.Writer.Port, 2))
	log.Printf("perfsim: writes now go to %s, the group's primary", primary)
	// Statements already running on the old pools finish there.
	time.AfterFunc(2*time.Minute, func() { oldW.Close(); oldH.Close() })
}

func (e *Engine) Close() {
	e.wdb().Close()
	e.hbdb().Close()
	for _, db := range e.mdb {
		db.Close()
	}
}

// Init learns the server and topology, then creates the sim's schema.
func (e *Engine) Init(ctx context.Context) error {
	var version, comment string
	if err := e.wdb().QueryRowContext(ctx, "SELECT VERSION(), @@version_comment").Scan(&version, &comment); err != nil {
		return fmt.Errorf("%s:%d: %w", e.cfg.Writer.Host, e.cfg.Writer.Port, err)
	}
	flavor := "mysql"
	switch {
	case strings.Contains(strings.ToLower(version), "mariadb"):
		flavor = "mariadb"
	case strings.Contains(strings.ToLower(comment), "percona"):
		flavor = "percona"
	}
	e.mu.Lock()
	e.flavor, e.version = flavor, strings.SplitN(version, "-", 2)[0]
	e.vars = words(flavor, e.version)
	e.mu.Unlock()
	e.detectTopology(ctx)

	stmts := []string{
		"CREATE DATABASE IF NOT EXISTS " + schema,
		`CREATE TABLE IF NOT EXISTS perfsim.heartbeat (id TINYINT NOT NULL PRIMARY KEY, ts DATETIME(6) NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS perfsim.orders (
			id BIGINT NOT NULL PRIMARY KEY,
			customer INT NOT NULL,
			amount DECIMAL(10,2) NOT NULL,
			status VARCHAR(16) NOT NULL,
			note INT NOT NULL DEFAULT 0,
			KEY customer (customer)) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS perfsim.state (k VARCHAR(128) NOT NULL PRIMARY KEY, v LONGTEXT NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS perfsim.seq (n INT NOT NULL PRIMARY KEY) ENGINE=InnoDB`,
	}
	for _, s := range stmts {
		if _, err := e.wdb().ExecContext(ctx, s); err != nil {
			return fmt.Errorf("creating the perfsim schema: %w", err)
		}
	}
	// seq holds 0–19999: a portable row source for bulk inserts (no recursive CTE
	// limits to raise, and the same statement works on MySQL and MariaDB).
	var n int
	e.wdb().QueryRowContext(ctx, "SELECT COUNT(*) FROM perfsim.seq").Scan(&n)
	if n < 20000 {
		d := "(SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9)"
		q := "INSERT IGNORE INTO perfsim.seq SELECT a.n + b.n*10 + c.n*100 + d.n*1000 + e.n*10000 FROM " +
			d + " a, " + d + " b, " + d + " c, " + d + " d, (SELECT 0 n UNION ALL SELECT 1) e"
		if _, err := e.wdb().ExecContext(ctx, q); err != nil {
			return fmt.Errorf("filling perfsim.seq: %w", err)
		}
	}
	e.wdb().QueryRowContext(ctx, "SELECT COUNT(*) FROM perfsim.orders").Scan(&n)
	for base := n; base < ordersRows; base += 20000 {
		_, err := e.wdb().ExecContext(ctx, `INSERT IGNORE INTO perfsim.orders (id, customer, amount, status)
			SELECT n + ? + 1, FLOOR(RAND()*10000), ROUND(RAND()*500, 2), 'new' FROM perfsim.seq`, base)
		if err != nil {
			return fmt.Errorf("seeding perfsim.orders: %w", err)
		}
	}
	// The topology is known now, so the members' applier statements can run.
	e.Sample(ctx)
	e.replayUndo(ctx)
	return e.Heartbeat(ctx)
}

// words are the statements whose spelling depends on the server: MySQL 8.0.22+
// says REPLICA and SOURCE, MariaDB and older MySQL say SLAVE and MASTER.
func words(flavor, version string) map[string]string {
	newSyntax := flavor != "mariadb" && versionAtLeast(version, 8, 0, 23)
	w := map[string]string{
		"stop":    "STOP SLAVE SQL_THREAD",
		"start":   "START SLAVE SQL_THREAD",
		"status":  "SHOW SLAVE STATUS",
		"delay":   "CHANGE MASTER TO MASTER_DELAY = %d",
		"workers": "slave_parallel_workers",
		"single":  "1",
		"applier": "wsrep_slave_threads",
	}
	if newSyntax {
		w["stop"], w["start"], w["status"] = "STOP REPLICA SQL_THREAD", "START REPLICA SQL_THREAD", "SHOW REPLICA STATUS"
		w["delay"] = "CHANGE REPLICATION SOURCE TO SOURCE_DELAY = %d"
		w["workers"] = "replica_parallel_workers"
	}
	if flavor == "mariadb" {
		w["workers"], w["single"] = "slave_parallel_threads", "0"
	} else if versionAtLeast(version, 8, 0, 26) {
		w["applier"] = "wsrep_applier_threads"
	}
	return w
}

func versionAtLeast(v string, maj, min, patch int) bool {
	p := strings.Split(v, ".")
	n := [3]int{}
	for i := 0; i < 3 && i < len(p); i++ {
		n[i], _ = strconv.Atoi(strings.TrimFunc(p[i], func(r rune) bool { return r < '0' || r > '9' }))
	}
	if n[0] != maj {
		return n[0] > maj
	}
	if n[1] != min {
		return n[1] > min
	}
	return n[2] >= patch
}

func (e *Engine) w(k string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.vars[k]
}

// detectTopology asks the writer what it is part of.
func (e *Engine) detectTopology(ctx context.Context) {
	topo := "standalone"
	var wsrep sql.NullString
	if e.wdb().QueryRowContext(ctx, "SELECT @@wsrep_on").Scan(&wsrep) == nil && (wsrep.String == "1" || strings.EqualFold(wsrep.String, "ON")) {
		topo = "galera"
	} else {
		var online int
		if e.wdb().QueryRowContext(ctx, "SELECT COUNT(*) FROM performance_schema.replication_group_members WHERE MEMBER_STATE = 'ONLINE'").Scan(&online) == nil && online > 0 {
			topo = "gr"
		} else if len(e.cfg.Members) > 1 {
			topo = "async"
		}
	}
	e.mu.Lock()
	e.topology = topo
	e.mu.Unlock()
}

func (e *Engine) Info() core.TargetInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	i := core.TargetInfo{Name: e.cfg.Name, Engine: "mysql", Flavor: e.flavor, Version: e.version, Topology: e.topology, Writer: e.writerID,
		LagTitle: "Replication lag", LagSub: "how far each member's copy of the heartbeat row is behind the writer", Status: "Replica status"}
	switch e.topology {
	case "galera":
		i.LagTitle, i.LagSub, i.Status = "Receive queue", "wsrep_local_recv_queue per node; flow control starts at gcs.fc_limit", "Galera status"
	case "gr":
		i.Status = "Group status"
	}
	return i
}

// Heartbeat writes the time on the writer. See core.Engine.
func (e *Engine) Heartbeat(ctx context.Context) error {
	_, err := e.hbdb().ExecContext(ctx, "REPLACE INTO perfsim.heartbeat (id, ts) VALUES (1, NOW(6))")
	return err
}

// Op is one statement of the steady workload. Writes upsert within a fixed id
// range so the table never grows.
func (e *Engine) Op(ctx context.Context, write bool, n int) error {
	id := n%ordersRows + 1
	if write {
		if n%2 == 0 {
			_, err := e.wdb().ExecContext(ctx, "UPDATE perfsim.orders SET amount = amount + 1, note = note + 1 WHERE id = ?", id)
			return err
		}
		_, err := e.wdb().ExecContext(ctx, `INSERT INTO perfsim.orders (id, customer, amount, status) VALUES (?, ?, ?, 'new')
			ON DUPLICATE KEY UPDATE customer = VALUES(customer), amount = VALUES(amount), status = 'new'`, id, n%10000, float64(n%50000)/100)
		return err
	}
	var rows *sql.Rows
	var err error
	if n%10 == 0 {
		rows, err = e.wdb().QueryContext(ctx, "SELECT id, amount, status FROM perfsim.orders WHERE customer = ? ORDER BY id LIMIT 20", n%10000)
	} else {
		rows, err = e.wdb().QueryContext(ctx, "SELECT id, amount, status FROM perfsim.orders WHERE id = ?", id)
	}
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	rows.Close()
	return rows.Err()
}

// --- saved state ---

func (e *Engine) Save(ctx context.Context, key string, v []byte) error {
	_, err := e.wdb().ExecContext(ctx, "REPLACE INTO perfsim.state (k, v) VALUES (?, ?)", key, string(v))
	return err
}

func (e *Engine) Load(ctx context.Context, prefix string) (map[string][]byte, error) {
	rows, err := e.wdb().QueryContext(ctx, "SELECT k, v FROM perfsim.state WHERE k LIKE CONCAT(?, '%')", prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = []byte(v)
		}
	}
	return out, rows.Err()
}

func (e *Engine) Delete(ctx context.Context, key string) error {
	_, err := e.wdb().ExecContext(ctx, "DELETE FROM perfsim.state WHERE k = ?", key)
	return err
}

// --- helpers shared by the dials ---

// member returns a member's pool, or nil.
func (e *Engine) member(id string) *sql.DB { return e.mdb[id] }

// writerDB is the pool of the member that currently takes writes, falling back
// to the application endpoint when the writer is not one of the members.
func (e *Engine) writerDB() (*sql.DB, string) {
	e.mu.Lock()
	id := e.writerID
	e.mu.Unlock()
	if db := e.mdb[id]; db != nil {
		return db, id
	}
	return e.wdb(), e.cfg.Writer.Host
}

// lagOf is the member's last sampled lag in seconds, or -1.
func (e *Engine) lagOf(id string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v, ok := e.lag[id]; ok {
		return v
	}
	return -1
}

func (e *Engine) replicaIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, m := range e.cfg.Members {
		if m.ID != e.writerID {
			out = append(out, m.ID)
		}
	}
	return out
}

// connID opens a dedicated connection and returns it with its server id.
func connID(ctx context.Context, db *sql.DB) (*sql.Conn, int64, error) {
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, 0, err
	}
	var id int64
	if err := c.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
		c.Close()
		return nil, 0, err
	}
	return c, id, nil
}

// execKillable runs a statement on a dedicated connection. If ctx ends first
// it kills the statement on the server: closing the client side alone would
// leave the server running it.
func execKillable(ctx context.Context, db *sql.DB, c *sql.Conn, id int64, query string, args ...any) error {
	done := make(chan error, 1)
	go func() {
		_, err := c.ExecContext(context.Background(), query, args...)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		kctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		db.ExecContext(kctx, fmt.Sprintf("KILL QUERY %d", id))
		cancel()
		<-done
		return ctx.Err()
	}
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

func randBetween(lo, hi int64) int64 {
	if hi <= lo {
		return lo
	}
	return lo + rand.Int64N(hi-lo)
}
