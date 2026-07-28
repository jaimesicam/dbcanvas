package store

import (
	"context"
	"fmt"
)

// createTableStmts is executed in order (locations/vehicles before the tables
// that reference them) every startup — idempotent via IF NOT EXISTS, so
// re-running against an already-provisioned schema/database is a no-op. Indexes
// are declared inline rather than as a separate pass: the composite
// (location_id, date) and (region, date) indexes on rental_inventory are this
// app's one deliberate index-design choice for the query-education panel — a
// lookup that supplies location_id hits idx_ri_location_date directly (a short
// index range scan), while the "browse by region" search only has region+date to
// filter on and falls back to a much wider scan of idx_ri_region_date across
// every location in that region — the same targeted-vs-scatter contrast Airline
// Sim demonstrates, reframed once more around indexing since that's what's
// actually true for a relational engine, direct or behind any of the four
// PostgreSQL topologies this app can be linked to.
var createTableStmts = []string{
	`CREATE TABLE IF NOT EXISTS locations (
		id VARCHAR(16) PRIMARY KEY,
		code VARCHAR(8) NOT NULL,
		name VARCHAR(64) NOT NULL,
		region VARCHAR(16) NOT NULL,
		size_tier VARCHAR(16) NOT NULL,
		base_rate NUMERIC(10,2) NOT NULL,
		popularity DOUBLE PRECISION NOT NULL,
		operational_status VARCHAR(16) NOT NULL DEFAULT 'open',
		current_utilization DOUBLE PRECISION NOT NULL DEFAULT 0,
		last_updated TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_locations_region ON locations (region)`,

	`CREATE TABLE IF NOT EXISTS vehicle_classes (
		location_id VARCHAR(16) NOT NULL,
		class_code VARCHAR(8) NOT NULL,
		name VARCHAR(32) NOT NULL,
		fleet_count INT NOT NULL,
		rate_mult DOUBLE PRECISION NOT NULL,
		PRIMARY KEY (location_id, class_code)
	)`,

	`CREATE TABLE IF NOT EXISTS vehicles (
		vin VARCHAR(17) PRIMARY KEY,
		make_model VARCHAR(32) NOT NULL,
		class_code VARCHAR(8) NOT NULL,
		home_location_id VARCHAR(16) NOT NULL,
		current_location_id VARCHAR(16) NOT NULL,
		status VARCHAR(16) NOT NULL DEFAULT 'available',
		last_updated TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_vehicles_current_location ON vehicles (current_location_id)`,
	`CREATE INDEX IF NOT EXISTS idx_vehicles_status ON vehicles (status)`,
	// Composite index backing the check-out agent's FOR UPDATE SKIP LOCKED claim
	// query, which always filters on exactly these three columns together.
	`CREATE INDEX IF NOT EXISTS idx_vehicles_claim ON vehicles (current_location_id, class_code, status)`,

	`CREATE TABLE IF NOT EXISTS rental_inventory (
		id VARCHAR(48) PRIMARY KEY,
		location_id VARCHAR(16) NOT NULL,
		region VARCHAR(16) NOT NULL,
		class_code VARCHAR(8) NOT NULL,
		date DATE NOT NULL,
		total_vehicles INT NOT NULL,
		booked_vehicles INT NOT NULL DEFAULT 0,
		available_vehicles INT NOT NULL,
		closed BOOLEAN NOT NULL DEFAULT FALSE,
		rate NUMERIC(10,2) NOT NULL,
		last_updated TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_ri_location_date ON rental_inventory (location_id, date)`,
	`CREATE INDEX IF NOT EXISTS idx_ri_region_date ON rental_inventory (region, date)`,

	`CREATE TABLE IF NOT EXISTS reservations (
		id VARCHAR(16) PRIMARY KEY,
		request_id VARCHAR(64) NOT NULL UNIQUE,
		renter_id VARCHAR(16) NOT NULL,
		renter_name VARCHAR(64) NOT NULL,
		pickup_location_id VARCHAR(16) NOT NULL,
		dropoff_location_id VARCHAR(16) NOT NULL,
		region VARCHAR(16) NOT NULL,
		class_code VARCHAR(8) NOT NULL,
		pickup_date DATE NOT NULL,
		return_date DATE NOT NULL,
		vehicle_vin VARCHAR(17),
		rate_total NUMERIC(10,2) NOT NULL,
		currency VARCHAR(8) NOT NULL DEFAULT 'USD',
		status VARCHAR(16) NOT NULL,
		version INT NOT NULL DEFAULT 1,
		actual_check_out TIMESTAMPTZ,
		actual_check_in TIMESTAMPTZ,
		history JSONB NOT NULL,
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_res_pickup_date ON reservations (pickup_location_id, pickup_date)`,
	`CREATE INDEX IF NOT EXISTS idx_res_status ON reservations (status)`,
	`CREATE INDEX IF NOT EXISTS idx_res_status_date ON reservations (status, pickup_date)`,

	`CREATE TABLE IF NOT EXISTS reservation_events (
		id BIGSERIAL PRIMARY KEY,
		at TIMESTAMPTZ NOT NULL,
		sim_at TIMESTAMPTZ NOT NULL,
		reservation_id VARCHAR(16) NOT NULL,
		action VARCHAR(32) NOT NULL,
		location_id VARCHAR(16) NOT NULL,
		rental_date DATE,
		agent VARCHAR(32),
		payload JSONB
	)`,
	`CREATE INDEX IF NOT EXISTS idx_events_at ON reservation_events (at)`,

	`CREATE TABLE IF NOT EXISTS reservation_requests (
		request_id VARCHAR(64) PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_requests_created ON reservation_requests (created_at)`,

	`CREATE TABLE IF NOT EXISTS query_samples (
		id BIGSERIAL PRIMARY KEY,
		at TIMESTAMPTZ NOT NULL,
		kind VARCHAR(16) NOT NULL,
		sql_text TEXT NOT NULL,
		rows_examined BIGINT NOT NULL,
		rows_returned BIGINT NOT NULL,
		index_used VARCHAR(64),
		duration_ms DOUBLE PRECISION NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS agents (
		agent_name VARCHAR(32) PRIMARY KEY,
		status VARCHAR(16) NOT NULL,
		last_tick TIMESTAMPTZ NOT NULL,
		detail VARCHAR(255),
		updated_at TIMESTAMPTZ NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS metrics (
		id VARCHAR(16) PRIMARY KEY,
		payload JSONB NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS sim_state (
		id VARCHAR(16) PRIMARY KEY,
		payload JSONB NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	)`,
}

// EnsureSchema creates every table (and index) this app needs. Called once at
// startup, after EnsureDatabase.
func EnsureSchema(ctx context.Context, s *Store) error {
	for _, stmt := range createTableStmts {
		if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
	}
	return nil
}

// Wipe truncates every table this app owns — never pg_catalog/information_schema
// or a table this app doesn't own. Used by Engine.Reset, which re-seeds
// locations/vehicle_classes/vehicles immediately afterward (see seed.go) — Wipe
// itself doesn't distinguish static topology from mutable workload state, exactly
// like Airline Sim's Wipe truncates routes/aircraft too and relies on the caller
// to re-seed. RESTART IDENTITY resets the BIGSERIAL sequences (reservation_events/
// query_samples ids) so a Reset's event feed cursor starts from a clean 0 again,
// same as Airline Sim relying on AUTO_INCREMENT restarting after TRUNCATE.
func Wipe(ctx context.Context, s *Store) error {
	for _, t := range AllTables {
		if _, err := s.DB.ExecContext(ctx, "TRUNCATE TABLE "+t+" RESTART IDENTITY CASCADE"); err != nil {
			return fmt.Errorf("truncate %s: %w", t, err)
		}
	}
	return nil
}
