package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// safeStatusName is the guard that keeps an interpolated status-variable name
// safe. The name has to be interpolated rather than bound — a placeholder is a
// syntax error in SHOW … LIKE, which is the bug §239 shipped and then found —
// so every name that reaches the SQL passes through here first. Names come only
// from constants in this package; this exists so that stays true.
func safeStatusName(name string) error {
	if name == "" {
		return fmt.Errorf("refusing to read an unnamed status variable")
	}
	for _, r := range name {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return fmt.Errorf("refusing to read status variable %q", name)
		}
	}
	return nil
}

// MySQL's lab knobs. It can do all three, and is the engine every one of them
// was designed against.

func (s *mysqlStore) Capabilities() LabSupport {
	return LabSupport{
		IdleTransaction: true, ExtraTables: true, TempTables: true,
		LockContention: true, ScanQueries: true, WritePressure: true,
	}
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

// EnsureLabTables seeds the two fixed-size tables the contention, commit and
// redo knobs write to. Idempotent: INSERT IGNORE leaves an existing row alone,
// so a redeploy reuses what is already there.
func (s *mysqlStore) EnsureLabTables(ctx context.Context) error {
	now := time.Now().UTC()
	for i := 0; i < LabHotRows; i++ {
		if _, err := s.db.ExecContext(ctx,
			"INSERT IGNORE INTO lab_hotrows (id, counter, updated_at) VALUES (?, 0, ?)",
			hotRowID(i), now); err != nil {
			return err
		}
	}
	for i := 0; i < labCommitRows; i++ {
		if _, err := s.db.ExecContext(ctx,
			"INSERT IGNORE INTO lab_hotrows (id, counter, updated_at) VALUES (?, 0, ?)",
			commitRowID(i), now); err != nil {
			return err
		}
	}
	for i := 1; i <= LabBulkRows; i++ {
		if _, err := s.db.ExecContext(ctx,
			"INSERT IGNORE INTO lab_bulk (id, payload, updated_at) VALUES (?, ?, ?)",
			i, labPayload(int64(i)), now); err != nil {
			return err
		}
	}
	return nil
}

// RunContendedUpdate is the row-lock-wait generator.
//
// It takes an explicit row lock with SELECT … FOR UPDATE, holds it briefly, then
// writes under it. Several of these running at once over a smaller set of rows
// is all row lock contention is; what varies between the modes is the order the
// rows are taken in, which is what turns waiting into deadlocking. See
// contentionPlan.
//
// A deadlock (1213) and a lock wait timeout (1205) are returned as results, not
// errors. They are the condition this exists to produce — reporting a
// successfully-provoked deadlock as a failure would put it in the error banner
// instead of on the chart where it belongs.
func (s *mysqlStore) RunContendedUpdate(ctx context.Context, mode string, worker int) (ContentionResult, error) {
	if mode == ContentionOff {
		return ContentionResult{}, nil
	}
	ids, hold, err := contentionPlan(mode, worker)
	if err != nil {
		return ContentionResult{}, err
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return ContentionResult{}, err
	}
	defer conn.Close()
	// Shorter than the server default of 50s. A worker parked for most of a
	// minute stops producing the churn the knob exists for, and a timeout is
	// itself a result worth reporting quickly.
	conn.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout = 10")

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return ContentionResult{}, err
	}
	defer tx.Rollback()

	start := time.Now()
	var waited time.Duration
	for _, id := range ids {
		lockStart := time.Now()
		var c int64
		if err := tx.QueryRowContext(ctx,
			"SELECT counter FROM lab_hotrows WHERE id = ? FOR UPDATE", id).Scan(&c); err != nil {
			if res, ok := mysqlContentionOutcome(err, waited+time.Since(lockStart), mode, ids); ok {
				return res, nil
			}
			return ContentionResult{}, err
		}
		// Time spent inside the lock request is time blocked behind somebody
		// else — the wait itself, kept apart from the hold below so the two are
		// never confused for each other.
		waited += time.Since(lockStart)

		// Hold after *each* acquisition, not only after the last. This is what
		// makes Heavy work: with the hold at the end, the gap between taking the
		// first row and asking for the second is one round trip, and two workers
		// approaching the same pair from opposite ends almost never interleave
		// inside it — the first one through takes both locks and the second
		// simply queues. Holding between the two acquisitions widens that gap to
		// the hold, so the cycle forms every time. Live testing found this: the
		// first version produced thousands of lock waits and not one deadlock.
		//
		// For Light, which takes a single row, this is the same hold in the same
		// place it always was.
		select {
		case <-ctx.Done():
			return ContentionResult{}, ctx.Err()
		case <-time.After(hold):
		}
	}

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			"UPDATE lab_hotrows SET counter = counter + 1, updated_at = ? WHERE id = ?",
			time.Now().UTC(), id); err != nil {
			if res, ok := mysqlContentionOutcome(err, waited, mode, ids); ok {
				return res, nil
			}
			return ContentionResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		if res, ok := mysqlContentionOutcome(err, waited, mode, ids); ok {
			return res, nil
		}
		return ContentionResult{}, err
	}
	return ContentionResult{
		Waited: waited, Duration: time.Since(start),
		Description: contentionDescription(mode, ids),
	}, nil
}

// mysqlContentionOutcome recognises the two errors that are actually results.
// Anything else is a genuine failure and is passed back to the caller.
func mysqlContentionOutcome(err error, waited time.Duration, mode string, ids []int) (ContentionResult, bool) {
	var me *mysqldriver.MySQLError
	if !errors.As(err, &me) {
		return ContentionResult{}, false
	}
	switch me.Number {
	case 1213: // ER_LOCK_DEADLOCK
		return ContentionResult{
			Waited: waited, Duration: waited, Deadlock: true,
			Description: contentionDescription(mode, ids) + " — deadlock, this transaction was rolled back",
		}, true
	case 1205: // ER_LOCK_WAIT_TIMEOUT
		return ContentionResult{
			Waited: waited, Duration: waited, Timeout: true,
			Description: contentionDescription(mode, ids) + " — lock wait timed out",
		}, true
	}
	return ContentionResult{}, false
}

// mysqlScanQuery has a predicate on a column with no index on it, so there is no
// access path but reading every row. It deliberately does no grouping and no
// sorting: this knob is about the scan, and a GROUP BY would drag the
// temporary-table pathology in and make the two impossible to tell apart on a
// chart.
const mysqlScanQuery = `
	SELECT COUNT(*), COALESCE(SUM(volume), 0)
	FROM price_ticks
	WHERE volume BETWEEN ? AND ?`

// RunScanQuery runs that, and confirms from Handler_read_rnd_next — the
// server's count of rows read from a full scan, session-scoped — that it really
// did read the table rather than finding an index to use.
func (s *mysqlStore) RunScanQuery(ctx context.Context) (ScanResult, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return ScanResult{}, err
	}
	defer conn.Close()

	before, err := mysqlSessionCounter(ctx, conn, "Handler_read_rnd_next")
	if err != nil {
		return ScanResult{}, err
	}
	lo, hi := scanRange()
	start := time.Now()
	var matched, total int64
	if err := conn.QueryRowContext(ctx, mysqlScanQuery, lo, hi).Scan(&matched, &total); err != nil {
		return ScanResult{}, err
	}
	took := time.Since(start)
	after, err := mysqlSessionCounter(ctx, conn, "Handler_read_rnd_next")
	if err != nil {
		return ScanResult{}, err
	}

	read := after - before
	return ScanResult{
		Rows: int(matched), RowsRead: read, Scanned: read > 0, Duration: took,
		Description: scanDescription(lo, hi),
	}, nil
}

// RunWritePressure does one batch in the shape asked for.
//
// Commits is n separate autocommitted single-row updates — one transaction, one
// log flush each — which is what makes the cost fsyncs rather than throughput.
// Redo is one transaction rewriting n wide rows, which dirties pages and fills
// the log without committing often.
//
// Both are measured from the server's own log counters across the batch. Those
// are global rather than session-scoped, so a busy neighbour can move them; the
// batch sizes are large enough that the signal dominates, and the note on the
// panel says which mode produced it.
func (s *mysqlStore) RunWritePressure(ctx context.Context, mode string, n int, budget time.Duration) (WriteResult, error) {
	if mode == WritePressureOff || n <= 0 {
		return WriteResult{Mode: WritePressureOff}, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return WriteResult{}, err
	}
	defer conn.Close()

	syncsBefore, bytesBefore, known := mysqlLogCounters(ctx, conn)
	start := time.Now()

	res := WriteResult{Mode: mode}
	switch mode {
	case WritePressureCommits:
		now := time.Now().UTC()
		deadline := writeDeadline(start, budget)
		for i := 0; i < n; i++ {
			// Checked before the statement rather than after, and against a
			// deadline rather than a cancelled context: cancelling mid-statement
			// would surface as an error, and this is a clean stop rather than a
			// failure.
			if !deadline.IsZero() && time.Now().After(deadline) {
				res.Capped = true
				break
			}
			// Autocommit: each statement is its own transaction and its own
			// commit. No explicit BEGIN, because wrapping them would be one
			// commit for the batch and would measure the opposite of the point.
			if _, err := conn.ExecContext(ctx,
				"UPDATE lab_hotrows SET counter = counter + 1, updated_at = ? WHERE id = ?",
				now, commitRowID(i)); err != nil {
				return WriteResult{}, err
			}
			res.Commits++
			if ctx.Err() != nil {
				break
			}
		}
		res.Description = writeCommitsDescription(res.Commits, n, res.Capped)
	case WritePressureRedo:
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return WriteResult{}, err
		}
		defer tx.Rollback()
		now := time.Now().UTC()
		for i := 0; i < n; i++ {
			id := 1 + i%LabBulkRows
			if _, err := tx.ExecContext(ctx,
				"UPDATE lab_bulk SET payload = ?, updated_at = ? WHERE id = ?",
				labPayload(now.UnixNano()+int64(i)), now, id); err != nil {
				return WriteResult{}, err
			}
			res.Bytes += LabBulkPayload
		}
		if err := tx.Commit(); err != nil {
			return WriteResult{}, err
		}
		res.Commits = 1
		res.Description = fmt.Sprintf("one transaction rewriting %d rows of %s",
			n, byteWord(int64(LabBulkPayload)))
	default:
		return WriteResult{}, fmt.Errorf("unknown write pressure mode %q", mode)
	}
	res.Duration = time.Since(start)

	if known {
		if syncsAfter, bytesAfter, ok := mysqlLogCounters(ctx, conn); ok {
			res.Syncs = syncsAfter - syncsBefore
			res.Bytes = bytesAfter - bytesBefore
			res.SyncsKnown = true
		}
	}
	return res, nil
}

// mysqlLogCounters reads the redo log's fsync count and bytes written. Both are
// global status variables — InnoDB has no session-scoped equivalent — so the
// third return value says whether they could be read at all rather than letting
// an unreadable counter pass as a zero.
func mysqlLogCounters(ctx context.Context, conn *sql.Conn) (syncs, bytes int64, ok bool) {
	syncs, err := mysqlGlobalCounter(ctx, conn, "Innodb_os_log_fsyncs")
	if err != nil {
		return 0, 0, false
	}
	bytes, err = mysqlGlobalCounter(ctx, conn, "Innodb_os_log_written")
	if err != nil {
		return 0, 0, false
	}
	return syncs, bytes, true
}

// mysqlGlobalCounter is mysqlSessionCounter's server-wide sibling, with the same
// interpolation guard and the same refusal to absorb an error.
func mysqlGlobalCounter(ctx context.Context, conn *sql.Conn, name string) (int64, error) {
	if err := safeStatusName(name); err != nil {
		return 0, err
	}
	var k string
	var v int64
	if err := conn.QueryRowContext(ctx,
		"SHOW GLOBAL STATUS LIKE '"+name+"'").Scan(&k, &v); err != nil {
		return 0, fmt.Errorf("read %s: %w", name, err)
	}
	return v, nil
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
	if err := safeStatusName(name); err != nil {
		return 0, err
	}
	var k string
	var v int64
	if err := conn.QueryRowContext(ctx,
		"SHOW SESSION STATUS LIKE '"+name+"'").Scan(&k, &v); err != nil {
		return 0, fmt.Errorf("read %s: %w", name, err)
	}
	return v, nil
}
