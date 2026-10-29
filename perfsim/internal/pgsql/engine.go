// Package pgsql is Performance Sim's PostgreSQL engine: a standalone server or
// a streaming-replication cluster (Patroni, repmgr), reached directly or
// through HAProxy or PgBouncer.
package pgsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"perfsim/internal/core"
)

// Config is what DBCanvas resolved for the linked target; the same shape as the
// MySQL engine's, so one PERFSIM_TARGET format serves both.
type Config struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Manager string `json:"manager,omitempty"` // patroni | repmgr | spock | operator | "": see patroni.go, spock.go
	// Database, when set, is where the sim works, in a perfsim schema: the one a
	// Spock cluster replicates, or an operator's application database when the
	// account cannot create databases. Otherwise the sim has its own perfsim
	// database.
	Database string `json:"database,omitempty"`
	// TLS is a manual connection's mode (prefer, require, disable); "" is prefer.
	TLS      string   `json:"tls,omitempty"`
	User     string   `json:"user"`
	Password string   `json:"password"`
	Writer   Endpoint `json:"writer"`
	// Reader, when set, is where the workload's reads go (an HAProxy read port).
	Reader  *Endpoint      `json:"reader,omitempty"`
	Members []MemberConfig `json:"members"`
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

// database is the sim's own database. Physical replication carries every
// database, so it exists on each standby as soon as the primary creates it.
const database = "perfsim"

const ordersRows = 100000

type Engine struct {
	cfg    Config
	writer atomic.Pointer[sql.DB]
	hb     atomic.Pointer[sql.DB]
	reader *sql.DB // the workload's reads, when the target splits them; nil otherwise
	mdb    map[string]*sql.DB

	mu         sync.Mutex
	writerHost string
	flavor     string
	version    string
	topology   string // standalone | streaming
	writerID   string
	lag        map[string]float64
	recovering map[string]bool
	// readsPrev is each member's blks_read and the time at the last sample.
	readsPrev map[string][2]float64
}

func New(cfg Config) *Engine {
	e := &Engine{cfg: cfg, mdb: map[string]*sql.DB{}, lag: map[string]float64{}, recovering: map[string]bool{}, readsPrev: map[string][2]float64{}}
	e.writer.Store(e.open(cfg.Writer.Host, cfg.Writer.Port, 80))
	e.hb.Store(e.open(cfg.Writer.Host, cfg.Writer.Port, 2))
	if cfg.Reader != nil {
		e.reader = e.open(cfg.Reader.Host, cfg.Reader.Port, 80)
	}
	e.writerHost = cfg.Writer.Host
	for _, m := range cfg.Members {
		e.mdb[m.ID] = e.open(m.Host, m.Port, 12)
	}
	return e
}

// open uses the simple query protocol: through PgBouncer in transaction mode a
// prepared statement may not exist on the server connection the next statement
// gets, and the sim's statements are short enough that it costs nothing.
// open is a pool on the sim's database: perfsim, or on Spock the replicated
// database with the perfsim schema first on the search path.
func (e *Engine) open(host string, port, pool int) *sql.DB {
	if e.cfg.Database != "" {
		return open(e.cfg.User, e.cfg.Password, host, port, e.cfg.Database, pool, schema+",public", e.cfg.TLS)
	}
	return open(e.cfg.User, e.cfg.Password, host, port, database, pool, "", e.cfg.TLS)
}

func open(user, pass, host string, port int, db string, pool int, searchPath, tls string) *sql.DB {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, pass), Host: fmt.Sprintf("%s:%d", host, port), Path: "/" + db}
	q := url.Values{}
	sslmode := "prefer"
	if tls == "require" || tls == "disable" {
		sslmode = tls
	}
	q.Set("sslmode", sslmode)
	q.Set("connect_timeout", "3")
	q.Set("default_query_exec_mode", "simple_protocol")
	q.Set("application_name", "perfsim")
	if searchPath != "" {
		// pgx sends a parameter it does not know as a run-time setting.
		q.Set("search_path", searchPath)
	}
	u.RawQuery = q.Encode()
	d, _ := sql.Open("pgx", u.String())
	d.SetMaxOpenConns(pool)
	d.SetMaxIdleConns(pool)
	d.SetConnMaxLifetime(10 * time.Minute)
	return d
}

func (e *Engine) wdb() *sql.DB  { return e.writer.Load() }
func (e *Engine) hbdb() *sql.DB { return e.hb.Load() }

// rdb is where the workload's reads go: the read endpoint when the target has
// one, the write endpoint otherwise.
func (e *Engine) rdb() *sql.DB {
	if e.reader != nil {
		return e.reader
	}
	return e.wdb()
}

func (e *Engine) Close() {
	e.wdb().Close()
	e.hbdb().Close()
	if e.reader != nil {
		e.reader.Close()
	}
	for _, d := range e.mdb {
		d.Close()
	}
}

// Init creates the sim's database on the primary, then its tables through the
// application's endpoint.
func (e *Engine) Init(ctx context.Context) error {
	primary := ""
	var firstErr error
	for _, m := range e.cfg.Members {
		d := open(e.cfg.User, e.cfg.Password, m.Host, m.Port, "postgres", 1, "", e.cfg.TLS)
		var rec bool
		var version string
		err := d.QueryRowContext(ctx, "SELECT pg_is_in_recovery(), version()").Scan(&rec, &version)
		if err == nil && !rec {
			primary = m.ID
			flavor := "postgresql"
			if strings.Contains(strings.ToLower(version), "percona") {
				flavor = "percona"
			}
			var short string
			d.QueryRowContext(ctx, "SHOW server_version").Scan(&short)
			e.mu.Lock()
			e.flavor, e.version = flavor, strings.Fields(short + " ")[0]
			e.mu.Unlock()
			// With a named database the target already has it; otherwise the
			// sim's own is created here once.
			exists := e.cfg.Database != ""
			if !exists {
				d.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", database).Scan(&exists)
			}
			if !exists {
				if _, err := d.ExecContext(ctx, "CREATE DATABASE "+database); err != nil && !strings.Contains(err.Error(), "already exists") {
					d.Close()
					return fmt.Errorf("creating the %s database on %s: %w", database, m.ID, err)
				}
			}
		} else if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", m.ID, err)
		}
		d.Close()
		if primary != "" {
			break
		}
	}
	if primary == "" {
		if firstErr != nil {
			return firstErr
		}
		return fmt.Errorf("no member is a primary yet")
	}
	topo := "standalone"
	if len(e.cfg.Members) > 1 {
		topo = "streaming"
	}
	if e.spock() {
		topo = "spock"
	}
	e.mu.Lock()
	e.topology, e.writerID = topo, primary
	e.mu.Unlock()
	if e.spock() {
		if err := e.spockInit(ctx, primary); err != nil {
			return err
		}
	} else if e.cfg.Database != "" {
		// Physical replication: the schema reaches the standbys from the primary.
		if _, err := e.wdb().ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
			return fmt.Errorf("creating the %s schema: %w", schema, err)
		}
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS heartbeat (id int PRIMARY KEY, ts timestamptz NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS orders (id bigint PRIMARY KEY, customer int NOT NULL, amount numeric(10,2) NOT NULL, status text NOT NULL, note int NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS orders_customer ON orders (customer)`,
		`CREATE TABLE IF NOT EXISTS state (k text PRIMARY KEY, v text NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS conflict (id int PRIMARY KEY)`,
		`INSERT INTO conflict VALUES (1) ON CONFLICT DO NOTHING`,
	}
	for _, s := range stmts {
		if _, err := e.wdb().ExecContext(ctx, s); err != nil {
			return fmt.Errorf("creating the sim's tables: %w", err)
		}
	}
	// Spock replicates no DDL here: the tables exist on every member, and the
	// ones whose rows should reach the others join the replication set.
	if e.spock() {
		for id, d := range e.mdb {
			for _, s := range stmts {
				if _, err := d.ExecContext(ctx, s); err != nil {
					return fmt.Errorf("creating the sim's tables on %s: %w", id, err)
				}
			}
		}
		for _, t := range []string{"heartbeat", "orders"} {
			if err := e.spockReplicate(ctx, t); err != nil {
				return err
			}
		}
	}
	var n int
	e.wdb().QueryRowContext(ctx, "SELECT count(*) FROM orders").Scan(&n)
	for base := n; base < ordersRows; base += 20000 {
		if _, err := e.wdb().ExecContext(ctx, `INSERT INTO orders (id, customer, amount, status)
			SELECT g, (random()*10000)::int, round((random()*500)::numeric, 2), 'new' FROM generate_series($1::bigint + 1, $1::bigint + 20000) g
			ON CONFLICT DO NOTHING`, base); err != nil {
			return fmt.Errorf("seeding orders: %w", err)
		}
	}
	if err := e.loadInit(ctx); err != nil {
		return err
	}
	e.Sample(ctx)
	e.replayUndo(ctx)
	return e.Heartbeat(ctx)
}

func (e *Engine) Info() core.TargetInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	info := core.TargetInfo{Name: e.cfg.Name, Engine: "postgres", Flavor: e.flavor, Version: e.version, Topology: e.topology, Writer: e.writerID,
		LagTitle: "Replay lag", LagSub: "how far each standby's copy of the heartbeat row is behind the primary", Status: "Replication status",
		BacklogLabel: "oldest snapshot age (transactions)", DiskReadsLabel: "blocks read outside shared_buffers/s"}
	if e.spock() {
		info.LagTitle, info.LagSub, info.Status = "Replication lag", "how far each member's copy of the heartbeat row is behind the writer", "Spock status"
	}
	return info
}

func (e *Engine) Heartbeat(ctx context.Context) error {
	_, err := e.hbdb().ExecContext(ctx, "INSERT INTO heartbeat (id, ts) VALUES (1, clock_timestamp()) ON CONFLICT (id) DO UPDATE SET ts = EXCLUDED.ts")
	return err
}

func (e *Engine) Op(ctx context.Context, write bool, n int) error {
	id := n%ordersRows + 1
	if write {
		if n%2 == 0 {
			_, err := e.wdb().ExecContext(ctx, "UPDATE orders SET amount = amount + 1, note = note + 1 WHERE id = $1", id)
			return err
		}
		_, err := e.wdb().ExecContext(ctx, `INSERT INTO orders (id, customer, amount, status) VALUES ($1, $2, $3, 'new')
			ON CONFLICT (id) DO UPDATE SET customer = EXCLUDED.customer, amount = EXCLUDED.amount, status = 'new'`, id, n%10000, float64(n%50000)/100)
		return err
	}
	var rows *sql.Rows
	var err error
	if n%10 == 0 {
		rows, err = e.rdb().QueryContext(ctx, "SELECT id, amount, status FROM orders WHERE customer = $1 ORDER BY id LIMIT 20", n%10000)
	} else {
		rows, err = e.rdb().QueryContext(ctx, "SELECT id, amount, status FROM orders WHERE id = $1", id)
	}
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	rows.Close()
	return rows.Err()
}

// followPrimary moves the workload and heartbeat to the new primary after a
// failover, when the sim writes to members directly. Through HAProxy or
// PgBouncer the endpoint follows the primary itself and nothing changes.
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
	oldW := e.writer.Swap(e.open(host, e.cfg.Writer.Port, 80))
	oldH := e.hb.Swap(e.open(host, e.cfg.Writer.Port, 2))
	log.Printf("perfsim: writes now go to %s, the primary", primary)
	time.AfterFunc(2*time.Minute, func() { oldW.Close(); oldH.Close() })
}

// --- saved state ---

func (e *Engine) Save(ctx context.Context, key string, v []byte) error {
	_, err := e.wdb().ExecContext(ctx, "INSERT INTO state (k, v) VALUES ($1, $2) ON CONFLICT (k) DO UPDATE SET v = EXCLUDED.v", key, string(v))
	return err
}

func (e *Engine) Load(ctx context.Context, prefix string) (map[string][]byte, error) {
	rows, err := e.wdb().QueryContext(ctx, "SELECT k, v FROM state WHERE k LIKE $1 || '%'", prefix)
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
	_, err := e.wdb().ExecContext(ctx, "DELETE FROM state WHERE k = $1", key)
	return err
}

func (e *Engine) member(id string) *sql.DB { return e.mdb[id] }

func (e *Engine) writerDB() (*sql.DB, string) {
	e.mu.Lock()
	id := e.writerID
	e.mu.Unlock()
	if d := e.mdb[id]; d != nil {
		return d, id
	}
	return e.wdb(), e.cfg.Writer.Host
}

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

// connPID opens a dedicated connection and returns it with its backend pid.
func connPID(ctx context.Context, d *sql.DB) (*sql.Conn, int64, error) {
	c, err := d.Conn(ctx)
	if err != nil {
		return nil, 0, err
	}
	var pid int64
	if err := c.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		c.Close()
		return nil, 0, err
	}
	return c, pid, nil
}

// execCancelable runs a statement on a dedicated connection and, if ctx ends
// first, cancels it on the server with pg_cancel_backend.
func execCancelable(ctx context.Context, d *sql.DB, c *sql.Conn, pid int64, query string, args ...any) error {
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
		d.ExecContext(kctx, "SELECT pg_cancel_backend($1)", pid)
		cancel()
		<-done
		return ctx.Err()
	}
}

// Probe connects to the writer and describes the server and whether the sim
// can make its own database there, for a connection test.
func (e *Engine) Probe(ctx context.Context) (string, error) {
	db := e.cfg.Database
	if db == "" {
		db = "postgres"
	}
	d := open(e.cfg.User, e.cfg.Password, e.cfg.Writer.Host, e.cfg.Writer.Port, db, 1, "", e.cfg.TLS)
	defer d.Close()
	var version string
	var create bool
	if err := d.QueryRowContext(ctx, "SELECT current_setting('server_version'), (SELECT rolsuper OR rolcreatedb FROM pg_roles WHERE rolname = current_user)").Scan(&version, &create); err != nil {
		// pgx reports every address and TLS attempt it made; the server's own
		// refusal is the part that says what to fix.
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return "", fmt.Errorf("%s (SQLSTATE %s)", pe.Message, pe.Code)
		}
		return "", err
	}
	msg := "PostgreSQL " + version
	switch {
	case e.cfg.Database != "":
		msg += ", working in database " + e.cfg.Database
	case create:
		msg += ", and this account can create the sim's perfsim database"
	default:
		msg += "; this account cannot create a database, so name one to work in"
	}
	return msg, nil
}
