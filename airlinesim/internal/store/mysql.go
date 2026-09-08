// Package store wraps the MySQL-family connection, table naming, and the
// namespace-isolation rule.
//
// Every table this app touches lives in exactly one schema (MYSQL_DB, default
// "airlinesim") — this is a hard rule, not a convention: dbcanvas labs and any
// learner poking at the same deployment with the mysql client operate on their own
// schemas/tables, and this app's own continuous simulation must never be mistaken
// for, or interfere with, that work. Reset() only ever truncates this app's own
// tables inside this one schema — it never touches mysql/information_schema/
// performance_schema or any other schema.
//
// Unlike Hotel Sim, this app never self-detects which MySQL-family shape (standalone
// Percona Server, MySQL replication, PXC, or either behind a proxy) it's talking to —
// dbcanvas's own edge-walk already resolved that authoritatively before this
// container was even created (see app/airlinesim.go), and passes it in as
// TARGET_KIND. Self-detecting here too would just be a second place for the two to
// disagree.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

const connectTimeout = 15 * time.Second

// Store holds the shared *sql.DB pool, scoped to this app's own schema. cfg is kept
// around (rather than reopened by re-parsing something recovered from *sql.DB, which
// exposes no such thing) so EnsureDatabase can reopen the pool with DBName set once
// the schema exists.
type Store struct {
	DB     *sql.DB
	cfg    *mysqldriver.Config
	Schema string
}

// Connect opens a lazy connection pool against dsn (no DBName set — the schema may
// not exist yet). Does not block on network reachability; callers should follow with
// Ping (see waitForMySQL in main.go) and then EnsureDatabase.
func Connect(dsn string) (*Store, error) {
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.DBName = ""
	// ParseTime is required — without it the driver hands back DATETIME columns as
	// []byte instead of time.Time, and every Scan(&someTime) in this app fails
	// silently wherever the caller discards the error (e.g. AllHeartbeats).
	cfg.ParseTime = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	db.SetConnMaxLifetime(5 * time.Minute)
	return &Store{DB: db, cfg: cfg, Schema: ""}, nil
}

func (s *Store) Ping(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.DB.PingContext(cctx)
}

// SetPoolSize resizes the live connection pool — called whenever the
// simulated load level changes (see sim.Profile.PoolSize), so a jump to High
// isn't fighting over a handful of idle connections sized for Stop/Low, and a
// drop back down doesn't leave dozens of connections open against a
// lab-sized cluster for no reason. Safe to call on an already-open *sql.DB;
// database/sql applies new limits to the existing pool without reconnecting.
func (s *Store) SetPoolSize(maxOpen, maxIdle int) {
	s.DB.SetMaxOpenConns(maxOpen)
	s.DB.SetMaxIdleConns(maxIdle)
}

// EnsureDatabase creates schemaName if it doesn't exist, then reopens the pool
// pointed at it — go-sql-driver's DSN carries the default schema per-connection, so
// switching schemas means reopening rather than issuing USE (which wouldn't survive
// the pool handing out a different underlying connection next call).
func (s *Store) EnsureDatabase(ctx context.Context, schemaName string) error {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if _, err := s.DB.ExecContext(cctx, "CREATE DATABASE IF NOT EXISTS `"+schemaName+"` CHARACTER SET utf8mb4"); err != nil {
		return fmt.Errorf("create database %s: %w", schemaName, err)
	}
	next := *s.cfg
	next.DBName = schemaName
	newDB, err := sql.Open("mysql", next.FormatDSN())
	if err != nil {
		return fmt.Errorf("reopen with schema: %w", err)
	}
	newDB.SetConnMaxLifetime(5 * time.Minute)
	if err := newDB.PingContext(cctx); err != nil {
		newDB.Close()
		return fmt.Errorf("ping schema %s: %w", schemaName, err)
	}
	s.DB.Close()
	s.DB = newDB
	s.cfg = &next
	s.Schema = schemaName
	return nil
}

// ServerVersion returns the deployment's reported @@version.
func (s *Store) ServerVersion(ctx context.Context) string {
	var v string
	if err := s.DB.QueryRowContext(ctx, "SELECT @@version").Scan(&v); err != nil {
		return ""
	}
	return v
}

// Table name constants — the complete set of tables this app owns.
const (
	TableRoutes             = "routes"
	TableSeatClasses        = "seat_classes"
	TableAircraft           = "aircraft"
	TableFlightInventory    = "flight_inventory"
	TableReservations       = "reservations"
	TableReservationEvents  = "reservation_events"
	TableReservationRequest = "reservation_requests"
	TableAgents             = "agents" // per-agent heartbeat rows
	TableMetrics            = "metrics"
	TableSimState           = "sim_state"
	TableQuerySamples       = "query_samples"
)

// AllTables lists every table this app owns, for Reset's truncate step and the
// topology panel's table-size listing.
var AllTables = []string{
	TableRoutes, TableSeatClasses, TableAircraft, TableFlightInventory, TableReservations,
	TableReservationEvents, TableReservationRequest, TableAgents, TableMetrics,
	TableSimState, TableQuerySamples,
}
