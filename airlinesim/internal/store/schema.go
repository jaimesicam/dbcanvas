package store

import (
	"context"
	"fmt"
)

// createTableStmts is executed in order (routes/aircraft before the tables that
// foreign-key or logically reference them) every startup — idempotent via
// IF NOT EXISTS, so re-running against an already-provisioned schema is a no-op.
// Indexes are declared inline rather than as a separate ALTER pass: the composite
// (route_id, flight_date) and (region, flight_date) indexes on flight_inventory are
// this app's one deliberate index-design choice for the query-education panel — a
// lookup that supplies route_id hits idx_fi_route_date directly (a short index
// range scan), while the "browse by region" search only has region+date to filter
// on and falls back to a much wider range scan of idx_fi_region_date across every
// route in that region, exactly analogous to Hotel Sim's shard-targeted vs
// scatter-gather query-education demo, reframed around indexing since that's what's
// actually true for a relational engine.
var createTableStmts = []string{
	`CREATE TABLE IF NOT EXISTS routes (
		id VARCHAR(16) PRIMARY KEY,
		flight_number VARCHAR(16) NOT NULL,
		origin VARCHAR(8) NOT NULL,
		destination VARCHAR(8) NOT NULL,
		region VARCHAR(16) NOT NULL,
		size_tier VARCHAR(16) NOT NULL,
		base_fare DECIMAL(10,2) NOT NULL,
		popularity DOUBLE NOT NULL,
		operational_status VARCHAR(16) NOT NULL DEFAULT 'open',
		current_load_factor DOUBLE NOT NULL DEFAULT 0,
		last_updated DATETIME(3) NOT NULL,
		INDEX idx_routes_region (region)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS seat_classes (
		route_id VARCHAR(16) NOT NULL,
		class_code VARCHAR(8) NOT NULL,
		name VARCHAR(32) NOT NULL,
		seat_count INT NOT NULL,
		fare_mult DOUBLE NOT NULL,
		PRIMARY KEY (route_id, class_code)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS aircraft (
		tail_number VARCHAR(16) PRIMARY KEY,
		type VARCHAR(32) NOT NULL,
		size_tier VARCHAR(16) NOT NULL,
		home_base VARCHAR(8) NOT NULL,
		route_id VARCHAR(16) NOT NULL,
		status VARCHAR(16) NOT NULL DEFAULT 'active',
		last_updated DATETIME(3) NOT NULL,
		INDEX idx_aircraft_route (route_id),
		INDEX idx_aircraft_status (status)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS flight_inventory (
		id VARCHAR(48) PRIMARY KEY,
		route_id VARCHAR(16) NOT NULL,
		region VARCHAR(16) NOT NULL,
		class_code VARCHAR(8) NOT NULL,
		flight_date DATE NOT NULL,
		tail_number VARCHAR(16) NOT NULL,
		total_seats INT NOT NULL,
		booked_seats INT NOT NULL DEFAULT 0,
		held_seats INT NOT NULL DEFAULT 0,
		unavailable_seats INT NOT NULL DEFAULT 0,
		available_seats INT NOT NULL,
		closed TINYINT(1) NOT NULL DEFAULT 0,
		fare DECIMAL(10,2) NOT NULL,
		promotion VARCHAR(32) NULL,
		last_updated DATETIME(3) NOT NULL,
		INDEX idx_fi_route_date (route_id, flight_date),
		INDEX idx_fi_region_date (region, flight_date)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS reservations (
		id VARCHAR(16) PRIMARY KEY,
		request_id VARCHAR(64) NOT NULL,
		passenger_id VARCHAR(16) NOT NULL,
		passenger_name VARCHAR(64) NOT NULL,
		route_id VARCHAR(16) NOT NULL,
		flight_number VARCHAR(16) NOT NULL,
		origin VARCHAR(8) NOT NULL,
		destination VARCHAR(8) NOT NULL,
		region VARCHAR(16) NOT NULL,
		class_code VARCHAR(8) NOT NULL,
		flight_date DATE NOT NULL,
		seats INT NOT NULL,
		fare_total DECIMAL(10,2) NOT NULL,
		currency VARCHAR(8) NOT NULL DEFAULT 'USD',
		status VARCHAR(16) NOT NULL,
		version INT NOT NULL DEFAULT 1,
		seat_assignment VARCHAR(8) NULL,
		actual_check_in DATETIME(3) NULL,
		actual_boarding DATETIME(3) NULL,
		history JSON NOT NULL,
		created_at DATETIME(3) NOT NULL,
		updated_at DATETIME(3) NOT NULL,
		UNIQUE KEY uq_reservations_request (request_id),
		INDEX idx_res_route_date (route_id, flight_date),
		INDEX idx_res_status (status),
		INDEX idx_res_status_date (status, flight_date)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS reservation_events (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		at DATETIME(3) NOT NULL,
		sim_at DATETIME(3) NOT NULL,
		reservation_id VARCHAR(16) NOT NULL,
		action VARCHAR(32) NOT NULL,
		route_id VARCHAR(16) NOT NULL,
		flight_date DATE NULL,
		agent VARCHAR(32) NULL,
		payload JSON NULL,
		INDEX idx_events_at (at)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS reservation_requests (
		request_id VARCHAR(64) PRIMARY KEY,
		created_at DATETIME(3) NOT NULL,
		INDEX idx_requests_created (created_at)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS query_samples (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		at DATETIME(3) NOT NULL,
		kind VARCHAR(16) NOT NULL,
		sql_text TEXT NOT NULL,
		rows_examined BIGINT NOT NULL,
		rows_returned BIGINT NOT NULL,
		index_used VARCHAR(64) NULL,
		duration_ms DOUBLE NOT NULL
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS agents (
		agent_name VARCHAR(32) PRIMARY KEY,
		status VARCHAR(16) NOT NULL,
		last_tick DATETIME(3) NOT NULL,
		detail VARCHAR(255) NULL,
		updated_at DATETIME(3) NOT NULL
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS metrics (
		id VARCHAR(16) PRIMARY KEY,
		payload JSON NOT NULL,
		updated_at DATETIME(3) NOT NULL
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS sim_state (
		id VARCHAR(16) PRIMARY KEY,
		payload JSON NOT NULL,
		updated_at DATETIME(3) NOT NULL
	) ENGINE=InnoDB`,
}

// EnsureSchema creates every table this app needs. Called once at startup.
func EnsureSchema(ctx context.Context, s *Store) error {
	for _, stmt := range createTableStmts {
		if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
	}
	return nil
}

// Wipe truncates every table this app owns — never mysql/information_schema/
// performance_schema or a schema this app doesn't own. Used by Engine.Reset, which
// re-seeds routes/seat_classes/aircraft immediately afterward (see seed.go) — Wipe
// itself doesn't distinguish static topology from mutable workload state, exactly
// like Hotel Sim's Wipe deletes hotels/roomTypes too and relies on the caller to
// re-seed.
func Wipe(ctx context.Context, s *Store) error {
	if _, err := s.DB.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0"); err != nil {
		return fmt.Errorf("disable fk checks: %w", err)
	}
	defer s.DB.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=1")
	for _, t := range AllTables {
		if _, err := s.DB.ExecContext(ctx, "TRUNCATE TABLE `"+t+"`"); err != nil {
			return fmt.Errorf("truncate %s: %w", t, err)
		}
	}
	return nil
}
