// Package store wraps the PostgreSQL connection and the namespace-isolation rule.
//
// Every table this app touches lives in exactly one schema, in exactly one
// database — "carsim" for every direct/proxied pg, Patroni, or repmgr target, or
// the pre-existing "spockdemo" database for a Spock target (see EnsureDatabase).
// dbcanvas learners poking at the same deployment with psql operate on their own
// tables/databases; this app's own continuous simulation must never be mistaken
// for, or interfere with, that work. Wipe only ever truncates this app's own
// tables — never pg_catalog, information_schema, or any table it doesn't own.
//
// Unlike Hotel Sim, this app never self-detects which PostgreSQL-family shape
// (standalone, Patroni, repmgr, Spock, or any of the three behind HAProxy) it's
// talking to — dbcanvas's own edge-walk already resolved that authoritatively
// before this container was even created (see app/carsim.go), and passes it in as
// TARGET_KIND. Self-detecting here too would just be a second place for the two
// to disagree.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

const connectTimeout = 15 * time.Second

// Store holds the shared *sql.DB pool, scoped to whichever database this app
// ended up in (see EnsureDatabase).
type Store struct {
	DB       *sql.DB
	baseDSN  string // the DSN this app was given, minus its database path
	Database string
}

// Connect opens a lazy connection pool against dsn as given (whatever database
// path it already carries — main.go always passes one, "postgres" at minimum).
// Does not block on network reachability; callers should follow with Ping (see
// waitForPostgres in main.go).
func Connect(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	db.SetConnMaxLifetime(5 * time.Minute)
	return &Store{DB: db, baseDSN: dsn}, nil
}

func (s *Store) Ping(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.DB.PingContext(cctx)
}

// SetPoolSize resizes the live connection pool — called whenever the simulated
// load level changes (see sim.Profile.PoolSize), so a jump to High isn't fighting
// over a handful of idle connections sized for Stop/Low, and a drop back down
// doesn't leave dozens of connections open against a lab-sized cluster for no
// reason. Safe to call on an already-open *sql.DB; database/sql applies new
// limits to the existing pool without reconnecting.
func (s *Store) SetPoolSize(maxOpen, maxIdle int) {
	s.DB.SetMaxOpenConns(maxOpen)
	s.DB.SetMaxIdleConns(maxIdle)
}

// EnsureDatabase points this Store's pool at dbName, creating it first if it
// doesn't already exist (Postgres has no "CREATE DATABASE IF NOT EXISTS" — the
// existence check has to be a separate catalog query). On a Spock target dbName
// is always "spockdemo", which is guaranteed to already exist (spock.go creates
// it during cluster setup) — create is skipped entirely there, since a fresh
// CREATE DATABASE would not be part of the replication set spock.go already
// configured, and this app must land in the SAME database repset_add_all_tables
// already covers, not a new one of its own. baseDSN's own path component (if
// any) is replaced, not appended, so this is safe to call more than once.
func (s *Store) EnsureDatabase(ctx context.Context, dbName string, mustAlreadyExist bool) error {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if !mustAlreadyExist {
		var exists bool
		if err := s.DB.QueryRowContext(cctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", dbName).Scan(&exists); err != nil {
			return fmt.Errorf("check database %s: %w", dbName, err)
		}
		if !exists {
			if _, err := s.DB.ExecContext(cctx, "CREATE DATABASE "+quoteIdent(dbName)); err != nil {
				return fmt.Errorf("create database %s: %w", dbName, err)
			}
		}
	}
	nextDSN, err := withDatabase(s.baseDSN, dbName)
	if err != nil {
		return fmt.Errorf("rewrite dsn: %w", err)
	}
	newDB, err := sql.Open("pgx", nextDSN)
	if err != nil {
		return fmt.Errorf("reopen with database: %w", err)
	}
	newDB.SetConnMaxLifetime(5 * time.Minute)
	if err := newDB.PingContext(cctx); err != nil {
		newDB.Close()
		return fmt.Errorf("ping database %s: %w", dbName, err)
	}
	s.DB.Close()
	s.DB = newDB
	s.baseDSN = nextDSN
	s.Database = dbName
	return nil
}

// ServerVersion returns the deployment's reported server_version.
func (s *Store) ServerVersion(ctx context.Context) string {
	var v string
	if err := s.DB.QueryRowContext(ctx, "SHOW server_version").Scan(&v); err != nil {
		return ""
	}
	return v
}

// Table name constants — the complete set of tables this app owns.
const (
	TableLocations           = "locations"
	TableVehicleClasses      = "vehicle_classes"
	TableVehicles            = "vehicles"
	TableRentalInventory     = "rental_inventory"
	TableReservations        = "reservations"
	TableReservationEvents   = "reservation_events"
	TableReservationRequests = "reservation_requests"
	TableQuerySamples        = "query_samples"
	TableAgents              = "agents"
	TableMetrics             = "metrics"
	TableSimState            = "sim_state"
)

// AllTables lists every table this app owns, for Wipe's truncate step and the
// topology panel's table-size listing.
var AllTables = []string{
	TableLocations, TableVehicleClasses, TableVehicles, TableRentalInventory, TableReservations,
	TableReservationEvents, TableReservationRequests, TableQuerySamples, TableAgents, TableMetrics, TableSimState,
}

// RegisterSpockReplication (re-)runs spock.repset_add_all_tables for the
// 'default' replication set — required on a Spock target only. Spock's logical
// replication is scoped to whatever tables were in the replication set at the
// time each subscription was created (spock.go runs this once, before this
// app's tables exist); tables created afterward are simply invisible to
// replication until explicitly added. Re-running the same call app-side, after
// EnsureSchema, is the only way this app's own tables ever get replicated —
// there is no per-table "ADD TABLE" DDL trigger that would do it automatically.
func (s *Store) RegisterSpockReplication(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, "SELECT spock.repset_add_all_tables('default', ARRAY['public'])")
	return err
}
