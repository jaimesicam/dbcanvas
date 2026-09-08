// perfschema.go reads Performance Schema statement digests — the
// leaderboard's foundation. events_statements_summary_by_digest already
// tracks everything the leaderboard and the future grading engine (stage
// S5) need per query shape (rows examined/sent, temp tables, sort activity,
// no-index-used, total/avg/max latency) without hand-instrumenting every
// query this app issues — confirmed on live under stage S0's own blocking-
// assumption check that performance_schema=ON and the statements_digest
// consumer are enabled by default on this repo's systemd base images.
package store

import (
	"context"
	"time"
)

// DigestRow is one row of performance_schema.events_statements_summary_by_digest,
// scoped to this app's own schema. Timer fields arrive from MySQL in
// picoseconds; converted to time.Duration here so no caller has to
// remember the /1e6 conversion.
type DigestRow struct {
	Digest    string
	Text      string
	CountStar int64

	SumTimer time.Duration
	AvgTimer time.Duration
	MaxTimer time.Duration

	SumRowsExamined      int64
	SumRowsSent          int64
	SumCreatedTmpTables  int64
	SumCreatedTmpDiskTab int64
	SumSortRows          int64
	SumNoIndexUsed       int64

	FirstSeen time.Time
	LastSeen  time.Time
}

// DigestSnapshot returns every digest performance_schema has recorded
// against this app's own schema — a full snapshot, not a delta; callers
// that want deltas (the leaderboard sampler) diff two consecutive snapshots
// themselves (see internal/sim/leaderboard.go), since a digest's own
// COUNT_STAR/SUM_* columns are lifetime cumulative counters that never reset
// on their own (short of a manual TRUNCATE TABLE on the digest table, which
// this app never does — it isn't this app's table to own).
func (s *Store) DigestSnapshot(ctx context.Context) ([]DigestRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DIGEST, DIGEST_TEXT, COUNT_STAR,
		       SUM_TIMER_WAIT, AVG_TIMER_WAIT, MAX_TIMER_WAIT,
		       SUM_ROWS_EXAMINED, SUM_ROWS_SENT,
		       SUM_CREATED_TMP_TABLES, SUM_CREATED_TMP_DISK_TABLES,
		       SUM_SORT_ROWS, SUM_NO_INDEX_USED,
		       FIRST_SEEN, LAST_SEEN
		FROM performance_schema.events_statements_summary_by_digest
		WHERE SCHEMA_NAME = ? AND DIGEST_TEXT IS NOT NULL`, s.Schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DigestRow
	for rows.Next() {
		var d DigestRow
		var sumPs, avgPs, maxPs uint64
		if err := rows.Scan(&d.Digest, &d.Text, &d.CountStar,
			&sumPs, &avgPs, &maxPs,
			&d.SumRowsExamined, &d.SumRowsSent,
			&d.SumCreatedTmpTables, &d.SumCreatedTmpDiskTab,
			&d.SumSortRows, &d.SumNoIndexUsed,
			&d.FirstSeen, &d.LastSeen); err != nil {
			continue
		}
		d.SumTimer = picoseconds(sumPs)
		d.AvgTimer = picoseconds(avgPs)
		d.MaxTimer = picoseconds(maxPs)
		out = append(out, d)
	}
	return out, rows.Err()
}

func picoseconds(v uint64) time.Duration {
	return time.Duration(v / 1000) // ps -> ns
}
