package store

// The PostgreSQL schema. Same tables as MySQL, same ids, same absence of
// foreign keys — see schema_mysql.go for why. The differences are only the
// dialect's: NUMERIC instead of DECIMAL, TIMESTAMPTZ instead of DATETIME(3),
// JSONB instead of JSON, BIGSERIAL for the one table that needs a monotonic
// cursor, and indexes as separate CREATE INDEX statements.
//
// Everything lives in the schema named by Config.Database, created if absent —
// a *schema*, not a database, so a user granted rights on one database can
// still be given a private namespace inside it.

var pgTables = []string{
	"securities", "price_ticks", "portfolios", "orders", "trades", "holdings",
	"metrics", "sim_state", "agents", "events",
}

var pgCreateStmts = []string{
	`CREATE TABLE IF NOT EXISTS securities (
		id CHAR(26) PRIMARY KEY,
		symbol VARCHAR(16) NOT NULL UNIQUE,
		name VARCHAR(128) NOT NULL,
		sector VARCHAR(64) NOT NULL DEFAULT '',
		currency CHAR(3) NOT NULL DEFAULT 'USD',
		shares_outstanding BIGINT NOT NULL DEFAULT 0,
		open_price NUMERIC(18,4) NOT NULL DEFAULT 0,
		last_price NUMERIC(18,4) NOT NULL DEFAULT 0,
		day_high NUMERIC(18,4) NOT NULL DEFAULT 0,
		day_low NUMERIC(18,4) NOT NULL DEFAULT 0,
		day_volume BIGINT NOT NULL DEFAULT 0,
		listed BOOLEAN NOT NULL DEFAULT TRUE,
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS ix_securities_sector ON securities (sector)`,

	`CREATE TABLE IF NOT EXISTS price_ticks (
		id CHAR(26) PRIMARY KEY,
		security_id CHAR(26) NOT NULL,
		symbol VARCHAR(16) NOT NULL,
		ts TIMESTAMPTZ NOT NULL,
		price NUMERIC(18,4) NOT NULL,
		volume BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS ix_ticks_security_ts ON price_ticks (security_id, ts)`,

	`CREATE TABLE IF NOT EXISTS portfolios (
		id CHAR(26) PRIMARY KEY,
		name VARCHAR(128) NOT NULL,
		owner VARCHAR(128) NOT NULL,
		cash NUMERIC(18,2) NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS ix_portfolios_owner ON portfolios (owner)`,

	`CREATE TABLE IF NOT EXISTS orders (
		id CHAR(26) PRIMARY KEY,
		portfolio_id CHAR(26) NOT NULL,
		security_id CHAR(26) NOT NULL,
		symbol VARCHAR(16) NOT NULL,
		owner VARCHAR(128) NOT NULL DEFAULT '',
		side VARCHAR(8) NOT NULL,
		order_type VARCHAR(8) NOT NULL DEFAULT 'market',
		quantity BIGINT NOT NULL,
		limit_price NUMERIC(18,4) NOT NULL DEFAULT 0,
		status VARCHAR(16) NOT NULL DEFAULT 'open',
		created_at TIMESTAMPTZ NOT NULL,
		filled_at TIMESTAMPTZ NULL
	)`,
	`CREATE INDEX IF NOT EXISTS ix_orders_status_created ON orders (status, created_at)`,
	`CREATE INDEX IF NOT EXISTS ix_orders_portfolio ON orders (portfolio_id)`,
	`CREATE INDEX IF NOT EXISTS ix_orders_security ON orders (security_id)`,

	`CREATE TABLE IF NOT EXISTS trades (
		id CHAR(26) PRIMARY KEY,
		order_id CHAR(26) NOT NULL,
		portfolio_id CHAR(26) NOT NULL,
		security_id CHAR(26) NOT NULL,
		symbol VARCHAR(16) NOT NULL,
		side VARCHAR(8) NOT NULL,
		quantity BIGINT NOT NULL,
		price NUMERIC(18,4) NOT NULL,
		ts TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS ix_trades_ts ON trades (ts)`,
	`CREATE INDEX IF NOT EXISTS ix_trades_security ON trades (security_id)`,
	`CREATE INDEX IF NOT EXISTS ix_trades_portfolio ON trades (portfolio_id)`,

	`CREATE TABLE IF NOT EXISTS holdings (
		portfolio_id CHAR(26) NOT NULL,
		security_id CHAR(26) NOT NULL,
		symbol VARCHAR(16) NOT NULL,
		quantity BIGINT NOT NULL DEFAULT 0,
		avg_cost NUMERIC(18,4) NOT NULL DEFAULT 0,
		updated_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (portfolio_id, security_id)
	)`,
	`CREATE INDEX IF NOT EXISTS ix_holdings_security ON holdings (security_id)`,

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
	`CREATE TABLE IF NOT EXISTS agents (
		agent_name VARCHAR(32) PRIMARY KEY,
		status VARCHAR(16) NOT NULL DEFAULT '',
		last_tick TIMESTAMPTZ NOT NULL,
		detail VARCHAR(255) NOT NULL DEFAULT '',
		updated_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS events (
		id BIGSERIAL PRIMARY KEY,
		ts TIMESTAMPTZ NOT NULL,
		kind VARCHAR(32) NOT NULL,
		symbol VARCHAR(16) NOT NULL DEFAULT '',
		message VARCHAR(512) NOT NULL
	)`,
}
