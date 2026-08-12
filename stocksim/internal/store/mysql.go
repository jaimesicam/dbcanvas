package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

const connectTimeout = 15 * time.Second

// mysqlStore implements Store against MySQL, Percona Server, MariaDB, or
// anything else that speaks the protocol — including a managed instance
// outside the stack, which is why nothing here assumes it may create users,
// change global settings, or see other schemas.
type mysqlStore struct {
	db     *sql.DB
	cfg    *mysqldriver.Config
	schema string
	pool   int // connection ceiling, kept so EnsureSchema's reopen matches
}

// openMySQL builds the DSN (or uses the supplied one verbatim), opens a lazy
// pool with no default schema — the schema may not exist yet — and pings it.
func openMySQL(ctx context.Context, c Config) (Store, error) {
	dsn := c.DSN
	if dsn == "" {
		dsn = mysqlDSN(c)
	}
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse mysql dsn: %w", err)
	}
	cfg.DBName = ""
	// ParseTime is required — without it the driver hands back DATETIME columns
	// as []byte instead of time.Time and every Scan(&someTime) fails. The
	// sibling sims all carry this line for the same reason.
	cfg.ParseTime = true
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	pool := c.PoolSize()
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetMaxOpenConns(pool)
	// Every connection is idle between one agent tick and the next, so an idle
	// ceiling below the open ceiling would have the pool closing and reopening
	// connections continuously under exactly the load it was raised for.
	db.SetMaxIdleConns(pool)
	return &mysqlStore{db: db, cfg: cfg, schema: c.Database, pool: pool}, nil
}

// mysqlDSN assembles a go-sql-driver DSN from discrete fields. TLS maps onto
// the driver's own tls parameter: "require" demands a TLS handshake but does
// not verify the certificate chain, which is the only thing that can work
// against a server using a self-signed cert — the common case for both a
// dbcanvas node and a self-hosted external server.
func mysqlDSN(c Config) string {
	port := c.Port
	if port == 0 {
		port = DefaultPort(EngineMySQL)
	}
	params := url.Values{}
	switch c.TLS {
	case "require":
		params.Set("tls", "skip-verify")
	case "prefer":
		params.Set("tls", "preferred")
	default:
		params.Set("tls", "false")
	}
	q := params.Encode()
	if extra := strings.TrimSpace(c.Params); extra != "" {
		q += "&" + strings.TrimPrefix(extra, "&")
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/?%s", c.User, c.Password, c.Host, port, q)
}

func (s *mysqlStore) Engine() string   { return EngineMySQL }
func (s *mysqlStore) Database() string { return s.schema }
func (s *mysqlStore) Close() error     { return s.db.Close() }

// Location: in MySQL a schema and a database are the same object, so there is
// only ever one answer.
func (s *mysqlStore) Location() string { return fmt.Sprintf("database %q", s.schema) }

func (s *mysqlStore) Ping(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.db.PingContext(cctx)
}

func (s *mysqlStore) ServerVersion(ctx context.Context) (string, error) {
	var v string
	if err := s.db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}

// EnsureSchema creates the app's own schema if absent, reopens the pool
// pointed at it, then creates every table. Reopening rather than issuing USE
// is required: go-sql-driver carries the default schema per-connection, so a
// USE would not survive the pool handing out a different connection next call.
func (s *mysqlStore) EnsureSchema(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(cctx,
		"CREATE DATABASE IF NOT EXISTS `"+s.schema+"` CHARACTER SET utf8mb4"); err != nil {
		// A user without CREATE DATABASE is normal against a managed or
		// shared external server, where an administrator has already made the
		// database and scoped our grant to it. That is only a failure if the
		// database genuinely is not there — so ask, and carry on if it is.
		var exists int
		if qerr := s.db.QueryRowContext(cctx,
			"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?",
			s.schema).Scan(&exists); qerr != nil || exists == 0 {
			return fmt.Errorf("database %q does not exist and could not be created "+
				"(%w) — create it first, or grant this user CREATE privileges", s.schema, err)
		}
	}
	// Reopen against the schema whenever the current pool is not already
	// usably pointed at it. Checking the connection rather than just the
	// recorded DBName matters after a DropSchema: the name still matches while
	// the pool's connections may no longer be able to see the tables.
	if s.cfg.DBName != s.schema || s.db.PingContext(cctx) != nil {
		next := *s.cfg
		next.DBName = s.schema
		newDB, err := sql.Open("mysql", next.FormatDSN())
		if err != nil {
			return fmt.Errorf("reopen with schema: %w", err)
		}
		newDB.SetConnMaxLifetime(5 * time.Minute)
		newDB.SetMaxOpenConns(s.pool)
		newDB.SetMaxIdleConns(s.pool)
		if err := newDB.PingContext(cctx); err != nil {
			newDB.Close()
			return fmt.Errorf("ping schema %s: %w", s.schema, err)
		}
		s.db.Close()
		s.db, s.cfg = newDB, &next
	}
	for _, stmt := range mysqlCreateStmts {
		if _, err := s.db.ExecContext(cctx, stmt); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
	}
	return nil
}

// Wipe empties every table this app owns, leaving the schema and its tables in
// place. Scoped to mysqlTables — never a wildcard over the schema, so an
// object someone else created in the same schema survives.
func (s *mysqlStore) Wipe(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0"); err != nil {
		return err
	}
	defer s.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=1")
	for _, t := range mysqlTables {
		if _, err := s.db.ExecContext(ctx, "TRUNCATE TABLE `"+t+"`"); err != nil {
			return fmt.Errorf("truncate %s: %w", t, err)
		}
	}
	return nil
}

// DropSchema removes every table this app created and stops there — it
// deliberately does NOT drop the database itself, even when that leaves it
// empty.
//
// Two reasons, both found by running this against a scoped user rather than by
// reasoning about it. First, on an external server the database is frequently
// provisioned by an administrator who then grants this user rights *on* it; a
// user with ALL PRIVILEGES ON db.* can drop that database but cannot create it
// again, so dropping it destroys our own access and a later re-seed fails with
// "No database selected". Second, "remove what this application created" is
// the promise the UI makes, and the database is not necessarily ours to count
// as one of our objects. An empty schema left behind is harmless; a deleted
// one may not be recoverable.
func (s *mysqlStore) DropSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0"); err != nil {
		return err
	}
	defer s.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=1")
	for _, t := range mysqlTables {
		if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+t+"`"); err != nil {
			return fmt.Errorf("drop %s: %w", t, err)
		}
	}
	return nil
}

// Objects reports the tables this app owns that currently exist, with
// information_schema's row and size estimates. TABLE_ROWS is an InnoDB
// estimate, not a count — accurate enough for a "what did this app create"
// panel and far cheaper than COUNT(*) over every table every few seconds.
// mysqlObjectsWithFileSize sizes each table from its tablespace file rather
// than from InnoDB's row statistics.
//
// Getting this right took two goes. DATA_LENGTH+INDEX_LENGTH comes from
// InnoDB's persistent statistics, and those are recalculated lazily — the
// figure for a table being written to hard lags the truth by a factor of two,
// long enough for the backfill agent to blow well past its size target before
// the number it is watching catches up. (The session variable below is the
// first half of the fix: MySQL 8 additionally *caches* what information_schema
// reports for information_schema_stats_expiry seconds, a whole day by default.
// It does not exist on MySQL 5.7 or MariaDB, so its failure is ignored.)
//
// FILE_SIZE is the .ibd file's own size. It moves the moment the file extends,
// it needs no statistics to be up to date, and for a feature whose entire
// purpose is to put bytes on a disk it is the more honest measure anyway.
//
// It also needs the PROCESS privilege, which a stack's application user has no
// reason to hold, and reading INNODB_TABLESPACES without it is an error rather
// than an empty result — hence the caller's fallback.
const mysqlObjectsWithFileSize = `
	SELECT t.TABLE_NAME, IFNULL(t.TABLE_ROWS,0),
	       IFNULL(ts.FILE_SIZE, IFNULL(t.DATA_LENGTH,0)+IFNULL(t.INDEX_LENGTH,0))
	FROM information_schema.TABLES t
	LEFT JOIN information_schema.INNODB_TABLESPACES ts
	       ON ts.NAME = CONCAT(t.TABLE_SCHEMA, '/', t.TABLE_NAME)
	WHERE t.TABLE_SCHEMA = ? ORDER BY t.TABLE_NAME`

const mysqlObjectsFromStats = `
	SELECT TABLE_NAME, IFNULL(TABLE_ROWS,0), IFNULL(DATA_LENGTH,0)+IFNULL(INDEX_LENGTH,0)
	FROM information_schema.TABLES
	WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME`

// Objects reports this app's tables with their row counts and sizes. See
// mysqlObjectsWithFileSize for where the sizes come from and why there are two
// queries rather than one.
func (s *mysqlStore) Objects(ctx context.Context) ([]ObjectInfo, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.ExecContext(ctx, "SET SESSION information_schema_stats_expiry = 0")

	// The privilege error for INNODB_TABLESPACES arrives while the result set
	// is being streamed, not when the query is issued, so the whole read has to
	// be attempted before the fallback can be ruled in or out.
	out, err := mysqlScanObjects(ctx, conn, mysqlObjectsWithFileSize, s.schema)
	if err != nil {
		// On the fallback the sizes come from InnoDB's persistent statistics,
		// which are recalculated so lazily that a table being written to hard
		// reads back five times smaller than it is. ANALYZE recomputes them
		// from the actual index page count, which is cheap — it samples twenty
		// pages — and needs only the SELECT and INSERT this app already holds
		// on its own tables. NO_WRITE_TO_BINLOG keeps it off the replication
		// stream, where it would be pure noise.
		conn.ExecContext(ctx, "ANALYZE NO_WRITE_TO_BINLOG TABLE "+mysqlQualifiedTables(s.schema))
		return mysqlScanObjects(ctx, conn, mysqlObjectsFromStats, s.schema)
	}
	return out, nil
}

// mysqlQualifiedTables lists this app's tables, schema-qualified and quoted,
// for a statement that takes a table list.
func mysqlQualifiedTables(schema string) string {
	names := make([]string, 0, len(mysqlTables))
	for _, t := range mysqlTables {
		names = append(names, "`"+schema+"`.`"+t+"`")
	}
	return strings.Join(names, ", ")
}

// mysqlScanObjects runs one of the two object queries to completion, keeping
// only the tables this app owns.
func mysqlScanObjects(ctx context.Context, conn *sql.Conn, query, schema string) ([]ObjectInfo, error) {
	rows, err := conn.QueryContext(ctx, query, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	owned := map[string]bool{}
	for _, t := range mysqlTables {
		owned[t] = true
	}
	var out []ObjectInfo
	for rows.Next() {
		var o ObjectInfo
		if err := rows.Scan(&o.Name, &o.Rows, &o.Bytes); err != nil {
			return nil, err
		}
		if !owned[o.Name] {
			continue
		}
		o.Kind = "table"
		out = append(out, o)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- helpers

// newID returns a sortable, opaque 26-character id. Time-prefixed so ids sort
// by creation, with random low bits — the same shape works on all four
// engines, which an AUTO_INCREMENT would not.
func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return fmt.Sprintf("%013x%s", time.Now().UTC().UnixMilli(), hex.EncodeToString(b[:])[:13])
}

// likeEscape makes a user-supplied search term safe to drop into a LIKE
// pattern, so a literal % or _ is matched as itself rather than as a wildcard.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// clampLimit keeps a caller-supplied page size inside sane bounds. Handlers
// clamp too; doing it here as well means a store is never asked to materialise
// an unbounded result set no matter who calls it.
func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// ------------------------------------------------- metrics / state / agents

func (s *mysqlStore) putBlob(ctx context.Context, table, id string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO `"+table+"` (id, payload, updated_at) VALUES (?,?,?) "+
			"ON DUPLICATE KEY UPDATE payload=VALUES(payload), updated_at=VALUES(updated_at)",
		id, string(b), time.Now().UTC())
	return err
}

func (s *mysqlStore) getBlob(ctx context.Context, table, id string) (json.RawMessage, error) {
	var payload string
	err := s.db.QueryRowContext(ctx,
		"SELECT payload FROM `"+table+"` WHERE id = ?", id).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(payload), nil
}

func (s *mysqlStore) PutMetrics(ctx context.Context, id string, payload any) error {
	return s.putBlob(ctx, "metrics", id, payload)
}

func (s *mysqlStore) GetMetrics(ctx context.Context, id string) (json.RawMessage, error) {
	return s.getBlob(ctx, "metrics", id)
}

func (s *mysqlStore) PutState(ctx context.Context, id string, payload any) error {
	return s.putBlob(ctx, "sim_state", id, payload)
}

func (s *mysqlStore) GetState(ctx context.Context, id string) (json.RawMessage, error) {
	return s.getBlob(ctx, "sim_state", id)
}

func (s *mysqlStore) Heartbeat(ctx context.Context, agent, status, detail string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO agents (agent_name, status, last_tick, detail, updated_at) VALUES (?,?,?,?,?) "+
			"ON DUPLICATE KEY UPDATE status=VALUES(status), last_tick=VALUES(last_tick), "+
			"detail=VALUES(detail), updated_at=VALUES(updated_at)",
		agent, status, now, truncate(detail, 255), now)
	return err
}

func (s *mysqlStore) AllHeartbeats(ctx context.Context) ([]AgentHeartbeat, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT agent_name, status, last_tick, detail, updated_at FROM agents ORDER BY agent_name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentHeartbeat
	for rows.Next() {
		var h AgentHeartbeat
		if err := rows.Scan(&h.Agent, &h.Status, &h.LastTick, &h.Detail, &h.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *mysqlStore) AppendEvent(ctx context.Context, e Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO events (ts, kind, symbol, message) VALUES (?,?,?,?)",
		e.TS, e.Kind, e.Symbol, truncate(e.Message, 512))
	return err
}

func (s *mysqlStore) EventsSince(ctx context.Context, afterID string, limit int) ([]Event, error) {
	after, _ := strconv.ParseInt(afterID, 10, 64)
	limit = clampLimit(limit, 50, 500)
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, ts, kind, symbol, message FROM events WHERE id > ? ORDER BY id ASC LIMIT ?",
		after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var id int64
		if err := rows.Scan(&id, &e.TS, &e.Kind, &e.Symbol, &e.Message); err != nil {
			return nil, err
		}
		e.ID = strconv.FormatInt(id, 10)
		out = append(out, e)
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
