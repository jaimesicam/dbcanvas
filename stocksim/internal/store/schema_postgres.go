package store

// The PostgreSQL schema. Same tables as MySQL, same ids, same absence of
// foreign keys — see schema_mysql.go for why. The differences are only the
// dialect's: NUMERIC instead of DECIMAL, TIMESTAMPTZ instead of DATETIME(3),
// JSONB instead of JSON, BIGSERIAL for the one table that needs a monotonic
// cursor, and indexes as separate CREATE INDEX statements.
//
// Everything lives in one schema, created if absent. Which schema, and in
// which database, is decided at open time — see the pgStore doc comment for
// the two possible placements and why there are two.

var pgTables = []string{
	"securities", "price_ticks", "portfolios", "orders", "trades", "holdings",
	"metrics", "sim_state", "agents", "events",
	"lab_parking", "lab_hotrows", "lab_bulk",
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
	// See the MySQL schema: one row for an idle transaction to hold, on a table
	// nothing else writes.
	`CREATE TABLE IF NOT EXISTS lab_parking (
		id INT PRIMARY KEY,
		holds BIGINT NOT NULL DEFAULT 0,
		touched_at TIMESTAMPTZ NOT NULL
	)`,
	`INSERT INTO lab_parking (id, holds, touched_at) VALUES (1, 0, NOW())
	 ON CONFLICT (id) DO NOTHING`,

	// See the MySQL schema for both of these. Rows are seeded by EnsureLabTables
	// so their counts follow the LabHotRows/LabBulkRows constants.
	`CREATE TABLE IF NOT EXISTS lab_hotrows (
		id INT PRIMARY KEY,
		counter BIGINT NOT NULL DEFAULT 0,
		updated_at TIMESTAMPTZ NOT NULL
	)`,
	// The redo knob's target needs autovacuum tuned for it, or the promise that
	// it does not grow is false on PostgreSQL.
	//
	// Every rewrite of a row is a new version under MVCC, and a 4 KiB payload is
	// past the TOAST threshold so the bulk of each version lands in this table's
	// TOAST relation rather than its heap. With stock settings autovacuum did not
	// touch either one during a live run: 256 rows, unchanged, occupying 77 MB
	// after 56 batches and still climbing. The dead space is reusable only once
	// something reclaims it.
	//
	// A scale factor of zero with a flat threshold makes autovacuum fire on a
	// fixed number of dead tuples rather than on a fraction of a table that never
	// gets any bigger — which is the standard treatment for a small, extremely
	// high-churn table, and has to be set on the TOAST relation separately.
	`CREATE TABLE IF NOT EXISTS lab_bulk (
		id INT PRIMARY KEY,
		payload BYTEA NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	) WITH (
		autovacuum_vacuum_scale_factor = 0,
		autovacuum_vacuum_threshold = 500,
		toast.autovacuum_vacuum_scale_factor = 0,
		toast.autovacuum_vacuum_threshold = 500
	)`,
	// Applied separately as well, so a table left over from a deployment made
	// before these settings existed picks them up on the next start rather than
	// keeping the stock behaviour forever.
	`ALTER TABLE lab_bulk SET (
		autovacuum_vacuum_scale_factor = 0,
		autovacuum_vacuum_threshold = 500,
		toast.autovacuum_vacuum_scale_factor = 0,
		toast.autovacuum_vacuum_threshold = 500
	)`,

	`CREATE TABLE IF NOT EXISTS events (
		id BIGSERIAL PRIMARY KEY,
		ts TIMESTAMPTZ NOT NULL,
		kind VARCHAR(32) NOT NULL,
		symbol VARCHAR(16) NOT NULL DEFAULT '',
		message VARCHAR(512) NOT NULL
	)`,
}
