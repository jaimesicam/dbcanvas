package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL's lab knobs. The same three pathologies, reached by different
// mechanisms — which is half of what makes them worth having on both engines.

func (s *pgStore) Capabilities() LabSupport {
	return LabSupport{
		IdleTransaction: true, ExtraTables: true, TempTables: true,
		LockContention: true, ScanQueries: true, WritePressure: true,
	}
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

// EnsureLabTables seeds the hot rows, the commit rows and the wide rows.
func (s *pgStore) EnsureLabTables(ctx context.Context) error {
	now := time.Now().UTC()
	seedRow := func(id int) error {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO lab_hotrows (id, counter, updated_at) VALUES ($1, 0, $2)
			 ON CONFLICT (id) DO NOTHING`, id, now)
		return err
	}
	for i := 0; i < LabHotRows; i++ {
		if err := seedRow(hotRowID(i)); err != nil {
			return err
		}
	}
	for i := 0; i < labCommitRows; i++ {
		if err := seedRow(commitRowID(i)); err != nil {
			return err
		}
	}
	for i := 1; i <= LabBulkRows; i++ {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO lab_bulk (id, payload, updated_at) VALUES ($1, $2, $3)
			 ON CONFLICT (id) DO NOTHING`, i, labPayload(int64(i)), now); err != nil {
			return err
		}
	}
	return nil
}

// RunContendedUpdate is PostgreSQL's row contention, and it differs from
// MySQL's in one way worth knowing: PostgreSQL has no lock wait timeout by
// default at all — a blocked writer waits forever — so lock_timeout is set here
// rather than relying on a server setting the way innodb_lock_wait_timeout can
// be. Its deadlock detector, by contrast, is on by default and fires after
// deadlock_timeout, so Heavy needs no help to produce one.
func (s *pgStore) RunContendedUpdate(ctx context.Context, mode string, worker int) (ContentionResult, error) {
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
	conn.ExecContext(ctx, "SET lock_timeout = '10s'")

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
			`SELECT counter FROM lab_hotrows WHERE id = $1 FOR UPDATE`, id).Scan(&c); err != nil {
			if res, ok := pgContentionOutcome(err, waited+time.Since(lockStart), mode, ids); ok {
				return res, nil
			}
			return ContentionResult{}, err
		}
		waited += time.Since(lockStart)
		// Held after each acquisition, for the reason spelled out on the MySQL
		// version: the hold has to sit between the two locks or the cycle Heavy
		// depends on never gets a chance to form.
		select {
		case <-ctx.Done():
			return ContentionResult{}, ctx.Err()
		case <-time.After(hold):
		}
	}

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE lab_hotrows SET counter = counter + 1, updated_at = $1 WHERE id = $2`,
			time.Now().UTC(), id); err != nil {
			if res, ok := pgContentionOutcome(err, waited, mode, ids); ok {
				return res, nil
			}
			return ContentionResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		if res, ok := pgContentionOutcome(err, waited, mode, ids); ok {
			return res, nil
		}
		return ContentionResult{}, err
	}
	return ContentionResult{
		Waited: waited, Duration: time.Since(start),
		Description: contentionDescription(mode, ids),
	}, nil
}

// pgContentionOutcome recognises the SQLSTATEs that are results rather than
// failures: a detected deadlock, and a lock request that gave up.
func pgContentionOutcome(err error, waited time.Duration, mode string, ids []int) (ContentionResult, bool) {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return ContentionResult{}, false
	}
	switch pe.Code {
	case "40P01": // deadlock_detected
		return ContentionResult{
			Waited: waited, Duration: waited, Deadlock: true,
			Description: contentionDescription(mode, ids) + " — deadlock, this transaction was rolled back",
		}, true
	case "55P03", "57014": // lock_not_available (lock_timeout), query_canceled
		return ContentionResult{
			Waited: waited, Duration: waited, Timeout: true,
			Description: contentionDescription(mode, ids) + " — lock wait timed out",
		}, true
	}
	return ContentionResult{}, false
}

// pgScanQuery is the same index-less read MySQL runs.
const pgScanQuery = `
	SELECT COUNT(*), COALESCE(SUM(volume), 0)
	FROM price_ticks
	WHERE volume BETWEEN $1 AND $2`

// RunScanQuery runs that under EXPLAIN (ANALYZE), which executes the query for
// real and reports what the executor actually did.
//
// The obvious alternative — differencing pg_stat_user_tables.seq_tup_read — was
// tried first and does not work here. PostgreSQL throttles a backend's
// statistics reports to at most one per second, so at any scan rate near or
// above 60 a minute most runs flush no stats at all and the delta reads as
// zero. Live testing showed 22 scans accounted for as 960 rows against the
// server's own 5,060: not merely imprecise but wrong in the specific direction
// that matters, because a scan whose stats had not flushed would have been
// reported as "no scan recorded". The plan's own actual-row counts have no such
// lag, are scoped to this query alone rather than to the table, and cannot be
// moved by a concurrent reader.
func (s *pgStore) RunScanQuery(ctx context.Context) (ScanResult, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return ScanResult{}, err
	}
	defer conn.Close()

	lo, hi := scanRange()
	start := time.Now()
	var raw []byte
	if err := conn.QueryRowContext(ctx,
		`EXPLAIN (ANALYZE, FORMAT JSON) `+pgScanQuery, lo, hi).Scan(&raw); err != nil {
		return ScanResult{}, err
	}
	took := time.Since(start)

	matched, read, seqScan, err := pgScanPlanStats(raw)
	if err != nil {
		return ScanResult{}, err
	}
	return ScanResult{
		Rows: int(matched), RowsRead: read, Scanned: seqScan && read > 0, Duration: took,
		Description: scanDescription(lo, hi),
	}, nil
}

// pgPlanNode is the part of an EXPLAIN (FORMAT JSON) plan this needs. Rows read
// by a sequential scan is the rows it emitted plus the rows its filter threw
// away — the second number is the whole point, since it is the work done to
// produce nothing.
type pgPlanNode struct {
	NodeType     string       `json:"Node Type"`
	RelationName string       `json:"Relation Name"`
	ActualRows   float64      `json:"Actual Rows"`
	RemovedRows  float64      `json:"Rows Removed by Filter"`
	Plans        []pgPlanNode `json:"Plans"`
}

// pgScanPlanStats walks the plan for a sequential scan of the tick table and
// reports what it read. Returned as an error rather than a zero when the plan
// cannot be parsed at all — see §239.
func pgScanPlanStats(raw []byte) (matched, read int64, seqScan bool, err error) {
	var plans []struct {
		Plan pgPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		return 0, 0, false, fmt.Errorf("parse scan plan: %w", err)
	}
	if len(plans) == 0 {
		return 0, 0, false, fmt.Errorf("scan plan was empty")
	}
	var walk func(pgPlanNode)
	walk = func(p pgPlanNode) {
		if p.NodeType == "Seq Scan" && p.RelationName == "price_ticks" {
			seqScan = true
			matched += int64(p.ActualRows)
			read += int64(p.ActualRows) + int64(p.RemovedRows)
		}
		for _, c := range p.Plans {
			walk(c)
		}
	}
	walk(plans[0].Plan)
	return matched, read, seqScan, nil
}

// RunWritePressure is the same two shapes as MySQL's, over PostgreSQL's WAL.
//
// The byte count here is exact rather than approximate: pg_current_wal_lsn is
// the write position in the log, so the difference across a batch is precisely
// how much WAL that batch produced. MySQL has no equivalent and has to be asked
// for a counter instead.
func (s *pgStore) RunWritePressure(ctx context.Context, mode string, n int, budget time.Duration) (WriteResult, error) {
	if mode == WritePressureOff || n <= 0 {
		return WriteResult{Mode: WritePressureOff}, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return WriteResult{}, err
	}
	defer conn.Close()

	syncsBefore, syncsOK := pgWalSyncs(ctx, conn)
	lsnBefore, lsnOK := pgWalPosition(ctx, conn)
	start := time.Now()

	res := WriteResult{Mode: mode}
	switch mode {
	case WritePressureCommits:
		// Fanned out across connections — see the MySQL version and WriteFanout.
		done, capped, err := runCommitFanout(ctx, start, budget, n,
			func(ctx context.Context, i int) error {
				c, err := s.db.Conn(ctx)
				if err != nil {
					return err
				}
				defer c.Close()
				_, err = c.ExecContext(ctx,
					`UPDATE lab_hotrows SET counter = counter + 1, updated_at = $1 WHERE id = $2`,
					time.Now().UTC(), commitRowID(i))
				return err
			})
		if err != nil {
			return WriteResult{}, err
		}
		res.Commits, res.Capped = done, capped
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
				`UPDATE lab_bulk SET payload = $1, updated_at = $2 WHERE id = $3`,
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

	if syncsOK {
		if after, ok := pgWalSyncs(ctx, conn); ok {
			res.Syncs = after - syncsBefore
			res.SyncsKnown = true
		}
	}
	if lsnOK {
		if after, ok := pgWalPosition(ctx, conn); ok {
			res.Bytes = after - lsnBefore
		}
	}
	return res, nil
}

// pgWalSyncs reads how many times the WAL has been flushed to disk. pg_stat_wal
// is PostgreSQL 14 and later; on anything older this reports not-known rather
// than zero, and the panel says so.
func pgWalSyncs(ctx context.Context, conn *sql.Conn) (int64, bool) {
	var n int64
	if err := conn.QueryRowContext(ctx, `SELECT wal_sync FROM pg_stat_wal`).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// pgWalPosition is the current write position in the WAL, in bytes from the
// start of the log. Differencing it across a batch gives the exact WAL that
// batch generated.
func pgWalPosition(ctx context.Context, conn *sql.Conn) (int64, bool) {
	var n int64
	if err := conn.QueryRowContext(ctx,
		`SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')::bigint`).Scan(&n); err != nil {
		// A standby has no pg_current_wal_lsn — the function errors there — and
		// this app can legitimately be pointed at one.
		return 0, false
	}
	return n, true
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
