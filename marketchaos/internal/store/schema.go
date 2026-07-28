package store

import (
	"context"
	"fmt"
)

// createTableStmts is executed in order every startup — idempotent via
// IF NOT EXISTS, so re-running against an already-provisioned schema is a
// no-op. As of stage S0 this is only the app-infra tables needed to prove
// the node deploys, reports healthy, and can show live connection status.
// The 13 domain tables (securities, market_quotes, price_ticks, traders,
// accounts, orders, trades, positions, account_ledger, watchlists,
// market_news, audit_events) plus the challenge-run tables
// (query_shapes/challenge_runs/challenge_run_windows) are added in stage S1
// alongside the data-size profiles and seeder.
var createTableStmts = []string{
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

// Wipe truncates every table this app owns — never
// mysql/information_schema/performance_schema or a schema this app doesn't
// own. Used by Engine.Reset.
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
