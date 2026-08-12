package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// PostgreSQL's lab knobs. The same three pathologies, reached by different
// mechanisms — which is half of what makes them worth having on both engines.

func (s *pgStore) Capabilities() LabSupport {
	return LabSupport{IdleTransaction: true, ExtraTables: true, TempTables: true}
}

// HoldIdleTransaction is PostgreSQL's version of the same incident, and it is
// worse here than on MySQL.
//
// An open REPEATABLE READ snapshot holds back the xmin horizon, so autovacuum
// cannot remove any dead tuple newer than it. The simulation keeps updating
// securities and orders throughout, so those dead tuples accumulate as visible
// table bloat that does not go away when the transaction finally ends — the
// space stays allocated. "Idle in transaction" is the single most reported
// cause of unexplained PostgreSQL bloat, and this reproduces it exactly.
func (s *pgStore) HoldIdleTransaction(ctx context.Context, d time.Duration) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Establish the snapshot. REPEATABLE READ takes it at the first statement,
	// not at BEGIN, so this read is what actually pins the horizon.
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM securities").Scan(&n); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE lab_parking SET touched_at = $1, holds = holds + 1 WHERE id = 1",
		time.Now().UTC()); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (s *pgStore) EnsureExtraTables(ctx context.Context, n int) (int, error) {
	n = ClampExtraTables(n)
	have, err := s.extraTableSet(ctx)
	if err != nil {
		return 0, err
	}
	want := map[string]bool{}
	for _, name := range ExtraTableNames(n) {
		want[name] = true
	}
	for name := range have {
		if !want[name] {
			if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS "`+name+`"`); err != nil {
				return len(have), err
			}
			delete(have, name)
		}
	}
	for _, name := range ExtraTableNames(n) {
		if have[name] {
			continue
		}
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(pgExtraTableDDL, name)); err != nil {
			return len(have), err
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO "`+name+`" (symbol, close_price, volume) VALUES `+
				`('ACME',100.00,1000),('BRVO',200.00,2000),('CDLA',50.00,3000)`); err != nil {
			return len(have), err
		}
		have[name] = true
	}
	return len(have), nil
}

const pgExtraTableDDL = `CREATE TABLE IF NOT EXISTS "%s" (
	id SERIAL PRIMARY KEY,
	symbol VARCHAR(16) NOT NULL,
	close_price NUMERIC(18,4) NOT NULL DEFAULT 0,
	volume BIGINT NOT NULL DEFAULT 0)`

func (s *pgStore) extraTableSet(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname = $1 AND tablename LIKE 'eod\_summary\_%'`,
		s.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// TouchExtraTables is the relation-cache equivalent. PostgreSQL has no
// table_open_cache, but every relation touched by a backend is cached per
// backend (relcache) and costs a file descriptor; thousands of them is felt as
// memory per connection and pressure on max_files_per_process.
func (s *pgStore) TouchExtraTables(ctx context.Context, names []string) error {
	for _, name := range names {
		if !strings.HasPrefix(name, "eod_summary_") {
			continue
		}
		var symbol string
		err := s.db.QueryRowContext(ctx,
			`SELECT symbol FROM "`+name+`" ORDER BY id LIMIT 1`).Scan(&symbol)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
	}
	return nil
}

// pgTempQuery is the same intraday rollup MySQL runs, in PostgreSQL's dialect.
const pgTempQuery = `
	SELECT date_trunc('minute', ts) AS minute, symbol,
	       COUNT(*) AS ticks, AVG(price) AS avg_price,
	       MAX(price) AS high, MIN(price) AS low, SUM(volume) AS volume
	FROM price_ticks
	WHERE ts >= $1
	GROUP BY minute, symbol
	ORDER BY ticks DESC, volume DESC
	LIMIT 5000`

// RunTempTableQuery forces the sort and grouping to spill, or not, through
// work_mem — the knob that decides whether PostgreSQL keeps an intermediate
// result in memory or writes it to a temporary file.
//
// Spilling is confirmed from pg_stat_database.temp_files, which counts temporary
// files written for this database. Read before and after, on the same
// connection, so it is measurement rather than inference.
func (s *pgStore) RunTempTableQuery(ctx context.Context, mode string) (TempQueryResult, error) {
	if mode == TempOff {
		return TempQueryResult{}, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return TempQueryResult{}, err
	}
	defer conn.Close()

	switch mode {
	case TempDisk:
		conn.ExecContext(ctx, "SET work_mem = '64kB'")
	case TempMemory:
		conn.ExecContext(ctx, "SET work_mem = '256MB'")
	default:
		return TempQueryResult{}, fmt.Errorf("unknown temp mode %q", mode)
	}

	before, err := pgTempFiles(ctx, conn)
	if err != nil {
		return TempQueryResult{}, err
	}
	since := time.Now().UTC().Add(-6 * time.Hour)
	start := time.Now()
	rows, err := conn.QueryContext(ctx, pgTempQuery, since)
	if err != nil {
		return TempQueryResult{}, err
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return TempQueryResult{}, err
	}
	took := time.Since(start)
	after, err := pgTempFiles(ctx, conn)
	if err != nil {
		return TempQueryResult{}, err
	}

	return TempQueryResult{
		Rows: n, Spilled: after > before, Duration: took,
		Description: "intraday rollup: one row per minute per symbol, ordered by count",
	}, nil
}

// pgTempFiles reads how many temporary files this database has written. Unlike
// MySQL's session counter this is database-wide, so a busy neighbour could in
// principle move it — which is why the query above is sized to spill decisively
// rather than marginally when asked to.
//
// The error is returned rather than absorbed, for the reason given on
// mysqlSessionCounter: a measurement that could not be taken must not read as a
// measurement of nothing happening.
func pgTempFiles(ctx context.Context, conn *sql.Conn) (int64, error) {
	var n int64
	if err := conn.QueryRowContext(ctx,
		"SELECT temp_files FROM pg_stat_database WHERE datname = current_database()").Scan(&n); err != nil {
		return 0, fmt.Errorf("read temp_files: %w", err)
	}
	return n, nil
}
