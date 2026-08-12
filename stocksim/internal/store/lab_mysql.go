package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MySQL's lab knobs. It can do all three, and is the engine every one of them
// was designed against.

func (s *mysqlStore) Capabilities() LabSupport {
	return LabSupport{IdleTransaction: true, ExtraTables: true, TempTables: true}
}

// HoldIdleTransaction is the history-list-length generator.
//
// REPEATABLE READ plus a first read establishes a snapshot, and InnoDB cannot
// purge any row version newer than the oldest open snapshot. The simulation
// keeps writing ticks and orders throughout, so every one of those versions
// accumulates in the undo log for as long as this sits here — which is exactly
// the shape of the production incident where one forgotten session in a
// transaction slowly strangles a server.
//
// It takes a connection of its own rather than a pooled one. A transaction held
// for hours on a pooled connection would take that connection out of circulation
// without the pool knowing why, and database/sql would happily hand the same
// connection to somebody else's query mid-transaction.
func (s *mysqlStore) HoldIdleTransaction(ctx context.Context, d time.Duration) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	// Always rolled back: this transaction exists to be open, never to land.
	defer tx.Rollback()

	// The read that pins the snapshot. Cheap on purpose — the point is the read
	// view, not the work.
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM securities").Scan(&n); err != nil {
		return err
	}
	// One uncommitted change, so this shows up as a writing transaction in
	// SHOW ENGINE INNODB STATUS and holds a row lock — on its own dedicated row,
	// which nothing else in the application ever touches, so it cannot block the
	// simulation it is meant to be observed alongside.
	if _, err := tx.ExecContext(ctx,
		"UPDATE lab_parking SET touched_at = ?, holds = holds + 1 WHERE id = 1",
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

// EnsureExtraTables creates or drops synthetic tables until there are n.
//
// Each is tiny and gets a couple of rows, because what is being measured is the
// cost of *having* the table — a handle in table_open_cache, a file descriptor,
// an entry in the data dictionary — not the cost of its contents.
func (s *mysqlStore) EnsureExtraTables(ctx context.Context, n int) (int, error) {
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
			if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+name+"`"); err != nil {
				return len(have), err
			}
			delete(have, name)
		}
	}
	for _, name := range ExtraTableNames(n) {
		if have[name] {
			continue
		}
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(mysqlExtraTableDDL, name)); err != nil {
			return len(have), err
		}
		// A couple of rows so a read of it is a real read rather than an
		// immediately-empty scan.
		if _, err := s.db.ExecContext(ctx,
			"INSERT INTO `"+name+"` (symbol, close_price, volume) VALUES "+
				"('ACME',100.00,1000),('BRVO',200.00,2000),('CDLA',50.00,3000)"); err != nil {
			return len(have), err
		}
		have[name] = true
	}
	return len(have), nil
}

const mysqlExtraTableDDL = "CREATE TABLE IF NOT EXISTS `%s` (" +
	"id INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
	"symbol VARCHAR(16) NOT NULL," +
	"close_price DECIMAL(18,4) NOT NULL DEFAULT 0," +
	"volume BIGINT NOT NULL DEFAULT 0," +
	"KEY ix_symbol (symbol)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"

// extraTableSet lists the synthetic tables that currently exist.
func (s *mysqlStore) extraTableSet(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT TABLE_NAME FROM information_schema.TABLES "+
			"WHERE TABLE_SCHEMA = ? AND TABLE_NAME LIKE 'eod\\_summary\\_%'", s.schema)
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

// TouchExtraTables reads one row from each named table. Every table touched
// needs a handle in table_open_cache; touching more of them than the cache holds
// is what makes Opened_tables climb and Table_open_cache_misses appear.
func (s *mysqlStore) TouchExtraTables(ctx context.Context, names []string) error {
	for _, name := range names {
		if !strings.HasPrefix(name, "eod_summary_") {
			continue // never interpolate a name this package did not generate
		}
		var symbol string
		err := s.db.QueryRowContext(ctx,
			"SELECT symbol FROM `"+name+"` ORDER BY id LIMIT 1").Scan(&symbol)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
	}
	return nil
}

// mysqlTempQuery is an intraday rollup: one row per minute per symbol, ordered
// by something that is not the grouping. It cannot be answered from an index, so
// the server has to materialise every group before it can sort them — which is
// precisely a temporary table, and a large one.
const mysqlTempQuery = `
	SELECT DATE_FORMAT(ts, '%Y-%m-%d %H:%i') AS minute, symbol,
	       COUNT(*) AS ticks, AVG(price) AS avg_price,
	       MAX(price) AS high, MIN(price) AS low, SUM(volume) AS volume
	FROM price_ticks
	WHERE ts >= ?
	GROUP BY minute, symbol
	ORDER BY ticks DESC, volume DESC
	LIMIT 5000`

// RunTempTableQuery runs that rollup with the session sized to force the
// outcome asked for.
//
// tmp_table_size and max_heap_table_size are session variables and need no
// privilege, so "disk" is made deterministic by shrinking them for this
// connection rather than by hoping the query is big enough. Both are restored
// with the connection, which is closed at the end.
//
// Whether it actually spilled is read from Created_tmp_disk_tables on the same
// session, before and after — the server's own accounting rather than an
// inference from the plan.
func (s *mysqlStore) RunTempTableQuery(ctx context.Context, mode string) (TempQueryResult, error) {
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
		// Small enough that any real grouping overflows it immediately.
		conn.ExecContext(ctx, "SET SESSION tmp_table_size = 16384, max_heap_table_size = 16384")
	case TempMemory:
		// Large enough to hold the whole rollup in memory.
		conn.ExecContext(ctx, "SET SESSION tmp_table_size = 268435456, max_heap_table_size = 268435456")
	default:
		return TempQueryResult{}, fmt.Errorf("unknown temp mode %q", mode)
	}

	before, err := mysqlSessionCounter(ctx, conn, "Created_tmp_disk_tables")
	if err != nil {
		return TempQueryResult{}, err
	}
	// A window wide enough to make many groups: one row per minute per symbol.
	since := time.Now().UTC().Add(-6 * time.Hour)
	start := time.Now()
	rows, err := conn.QueryContext(ctx, mysqlTempQuery, since)
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
	after, err := mysqlSessionCounter(ctx, conn, "Created_tmp_disk_tables")
	if err != nil {
		return TempQueryResult{}, err
	}

	return TempQueryResult{
		Rows: n, Spilled: after > before, Duration: took,
		Description: "intraday rollup: one row per minute per symbol, ordered by count",
	}, nil
}

// mysqlSessionCounter reads one SHOW SESSION STATUS counter, session-scoped so
// concurrent work on other connections cannot be mistaken for this query's.
//
// The name is interpolated rather than bound, because a placeholder is a syntax
// error in SHOW … LIKE — found by watching this report "did not spill" against a
// server whose Created_tmp_disk_tables was climbing the whole time, since the
// first version bound it and swallowed the resulting error as a zero. Names come
// only from constants in this file, and the guard below keeps it that way.
//
// The error is returned rather than absorbed: a measurement that cannot be taken
// must not read as a measurement of nothing happening.
func mysqlSessionCounter(ctx context.Context, conn *sql.Conn, name string) (int64, error) {
	for _, r := range name {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return 0, fmt.Errorf("refusing to read status variable %q", name)
		}
	}
	var k string
	var v int64
	if err := conn.QueryRowContext(ctx,
		"SHOW SESSION STATUS LIKE '"+name+"'").Scan(&k, &v); err != nil {
		return 0, fmt.Errorf("read %s: %w", name, err)
	}
	return v, nil
}
