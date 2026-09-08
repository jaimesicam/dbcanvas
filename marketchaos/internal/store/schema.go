package store

import (
	"context"
	"fmt"
)

// createTableStmts is executed in order every startup — idempotent via
// IF NOT EXISTS, so re-running against an already-provisioned schema is a
// no-op.
//
// Indexes here are the BASELINE, competently-designed shape — challenges
// (stage S4+) deliberately drop, add, or misalign specific indexes via their
// own Setup/Teardown SQL against this starting point; this file is never
// itself the "unoptimized" part of the app. price_ticks is the one table
// worth reading closely: idx_ticks_security_time is exactly the index the
// product spec's "missing price-history index" challenge (B1) drops at
// challenge-setup time, and price_ticks is otherwise the largest table by a
// wide margin (up to 25M rows on the Large profile) and the primary target
// for the indexing/range-query/pagination challenge category.
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

	// --- reference data ---

	`CREATE TABLE IF NOT EXISTS sectors (
		sector_id TINYINT UNSIGNED PRIMARY KEY,
		name VARCHAR(32) NOT NULL
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS securities (
		security_id INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		symbol VARCHAR(8) NOT NULL,
		company_name VARCHAR(128) NOT NULL,
		sector_id TINYINT UNSIGNED NOT NULL,
		status VARCHAR(16) NOT NULL DEFAULT 'active',
		ipo_date DATE NOT NULL,
		shares_outstanding BIGINT UNSIGNED NOT NULL,
		created_at DATETIME(3) NOT NULL,
		UNIQUE KEY uq_securities_symbol (symbol),
		INDEX idx_securities_sector (sector_id)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS market_quotes (
		security_id INT UNSIGNED PRIMARY KEY,
		last_price DECIMAL(12,4) NOT NULL,
		bid_price DECIMAL(12,4) NOT NULL,
		ask_price DECIMAL(12,4) NOT NULL,
		day_open DECIMAL(12,4) NOT NULL,
		day_high DECIMAL(12,4) NOT NULL,
		day_low DECIMAL(12,4) NOT NULL,
		previous_close DECIMAL(12,4) NOT NULL,
		volume BIGINT UNSIGNED NOT NULL DEFAULT 0,
		updated_at DATETIME(3) NOT NULL,
		version INT UNSIGNED NOT NULL DEFAULT 1
	) ENGINE=InnoDB`,

	// price_ticks is append-only and by far the largest table. idx_ticks_security_time
	// is the baseline index the "missing price-history index" challenge (B1) removes.
	`CREATE TABLE IF NOT EXISTS price_ticks (
		tick_id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		security_id INT UNSIGNED NOT NULL,
		price DECIMAL(12,4) NOT NULL,
		bid_price DECIMAL(12,4) NOT NULL,
		ask_price DECIMAL(12,4) NOT NULL,
		volume INT UNSIGNED NOT NULL,
		exchange_code VARCHAR(8) NOT NULL DEFAULT 'XNAS',
		recorded_at DATETIME(3) NOT NULL,
		INDEX idx_ticks_security_time (security_id, recorded_at)
	) ENGINE=InnoDB`,

	// --- traders & accounts ---

	`CREATE TABLE IF NOT EXISTS traders (
		trader_id INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		username VARCHAR(32) NOT NULL,
		email VARCHAR(128) NOT NULL,
		account_status VARCHAR(16) NOT NULL DEFAULT 'active',
		risk_level VARCHAR(16) NOT NULL DEFAULT 'moderate',
		country_code VARCHAR(4) NOT NULL,
		created_at DATETIME(3) NOT NULL,
		last_login_at DATETIME(3) NULL,
		UNIQUE KEY uq_traders_username (username),
		UNIQUE KEY uq_traders_email (email)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS accounts (
		account_id INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		trader_id INT UNSIGNED NOT NULL,
		cash_balance DECIMAL(16,2) NOT NULL,
		reserved_cash DECIMAL(16,2) NOT NULL DEFAULT 0,
		margin_limit DECIMAL(16,2) NOT NULL DEFAULT 0,
		updated_at DATETIME(3) NOT NULL,
		INDEX idx_accounts_trader (trader_id)
	) ENGINE=InnoDB`,

	// --- orders, trades, positions, ledger ---

	`CREATE TABLE IF NOT EXISTS orders (
		order_id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		account_id INT UNSIGNED NOT NULL,
		security_id INT UNSIGNED NOT NULL,
		side VARCHAR(4) NOT NULL,
		order_type VARCHAR(8) NOT NULL,
		quantity INT UNSIGNED NOT NULL,
		remaining_quantity INT UNSIGNED NOT NULL,
		limit_price DECIMAL(12,4) NULL,
		status VARCHAR(16) NOT NULL,
		priority INT UNSIGNED NOT NULL DEFAULT 0,
		created_at DATETIME(3) NOT NULL,
		updated_at DATETIME(3) NOT NULL,
		cancelled_at DATETIME(3) NULL,
		INDEX idx_orders_account (account_id),
		INDEX idx_orders_security_status (security_id, status),
		INDEX idx_orders_status_created (status, created_at)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS trades (
		trade_id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		buy_order_id BIGINT UNSIGNED NOT NULL,
		sell_order_id BIGINT UNSIGNED NOT NULL,
		security_id INT UNSIGNED NOT NULL,
		quantity INT UNSIGNED NOT NULL,
		price DECIMAL(12,4) NOT NULL,
		trade_value DECIMAL(16,2) NOT NULL,
		executed_at DATETIME(3) NOT NULL,
		INDEX idx_trades_security_time (security_id, executed_at),
		INDEX idx_trades_buy_order (buy_order_id),
		INDEX idx_trades_sell_order (sell_order_id)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS positions (
		account_id INT UNSIGNED NOT NULL,
		security_id INT UNSIGNED NOT NULL,
		quantity INT NOT NULL DEFAULT 0,
		average_cost DECIMAL(12,4) NOT NULL DEFAULT 0,
		realized_profit DECIMAL(16,2) NOT NULL DEFAULT 0,
		updated_at DATETIME(3) NOT NULL,
		PRIMARY KEY (account_id, security_id),
		INDEX idx_positions_security (security_id)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS account_ledger (
		entry_id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		account_id INT UNSIGNED NOT NULL,
		trade_id BIGINT UNSIGNED NULL,
		entry_type VARCHAR(16) NOT NULL,
		amount DECIMAL(16,2) NOT NULL,
		balance_after DECIMAL(16,2) NOT NULL,
		created_at DATETIME(3) NOT NULL,
		INDEX idx_ledger_account_time (account_id, created_at)
	) ENGINE=InnoDB`,

	// --- watchlists, news, audit ---

	`CREATE TABLE IF NOT EXISTS watchlists (
		watchlist_id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		trader_id INT UNSIGNED NOT NULL,
		security_id INT UNSIGNED NOT NULL,
		added_at DATETIME(3) NOT NULL,
		display_order INT UNSIGNED NOT NULL DEFAULT 0,
		UNIQUE KEY uq_watchlists_trader_security (trader_id, security_id),
		INDEX idx_watchlists_trader (trader_id)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS market_news (
		news_id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		security_id INT UNSIGNED NULL,
		headline VARCHAR(255) NOT NULL,
		body TEXT NOT NULL,
		sentiment VARCHAR(16) NOT NULL DEFAULT 'neutral',
		published_at DATETIME(3) NOT NULL,
		INDEX idx_news_security_time (security_id, published_at)
	) ENGINE=InnoDB`,

	`CREATE TABLE IF NOT EXISTS audit_events (
		event_id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		entity_type VARCHAR(32) NOT NULL,
		entity_id BIGINT UNSIGNED NOT NULL,
		event_type VARCHAR(32) NOT NULL,
		event_payload JSON NULL,
		created_at DATETIME(3) NOT NULL,
		INDEX idx_audit_entity (entity_type, entity_id),
		INDEX idx_audit_created (created_at)
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
// own. Used by Engine.Reset, which re-seeds immediately afterward.
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
