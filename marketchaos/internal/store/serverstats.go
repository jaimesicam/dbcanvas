package store

import "context"

// ServerStats is a snapshot of the raw GLOBAL STATUS counters the
// database-performance panel derives QPS/TPS/lock-wait/temp-table/buffer-
// pool numbers from. Raw cumulative counters, not deltas — same pattern as
// DigestSnapshot: callers (the sampler in internal/sim) diff two
// consecutive snapshots themselves.
type ServerStats struct {
	Questions            int64
	ComSelect            int64
	ComInsert            int64
	ComUpdate            int64
	ComDelete            int64
	InnodbRowLockWaits   int64
	InnodbRowLockTimeMs  int64
	InnodbDeadlocks      int64
	CreatedTmpTables     int64
	CreatedTmpDiskTables int64
	SortRows             int64
	BufferPoolReadReqs   int64
	BufferPoolReads      int64 // physical reads (misses) — hit rate = 1 - Reads/ReadReqs
	ThreadsConnected     int64
	ThreadsRunning       int64
	MaxUsedConnections   int64
}

// ServerStats queries every GLOBAL STATUS row unconditionally (a few
// hundred rows, cheap) rather than filtering server-side with a WHERE ...
// IN (...) — SHOW statements are a different grammar from SELECT and this
// sidesteps any doubt about whether the driver's prepared-statement
// placeholder binding applies the same way to them.
func (s *Store) ServerStats(ctx context.Context) (ServerStats, error) {
	rows, err := s.DB.QueryContext(ctx, "SHOW GLOBAL STATUS")
	if err != nil {
		return ServerStats{}, err
	}
	defer rows.Close()

	raw := map[string]int64{}
	for rows.Next() {
		var k string
		var v int64
		if rows.Scan(&k, &v) == nil {
			raw[k] = v
		}
	}
	if err := rows.Err(); err != nil {
		return ServerStats{}, err
	}

	return ServerStats{
		Questions: raw["Questions"], ComSelect: raw["Com_select"], ComInsert: raw["Com_insert"],
		ComUpdate: raw["Com_update"], ComDelete: raw["Com_delete"],
		InnodbRowLockWaits: raw["Innodb_row_lock_waits"], InnodbRowLockTimeMs: raw["Innodb_row_lock_time"],
		InnodbDeadlocks: raw["Innodb_deadlocks"], CreatedTmpTables: raw["Created_tmp_tables"],
		CreatedTmpDiskTables: raw["Created_tmp_disk_tables"], SortRows: raw["Sort_rows"],
		BufferPoolReadReqs: raw["Innodb_buffer_pool_read_requests"], BufferPoolReads: raw["Innodb_buffer_pool_reads"],
		ThreadsConnected: raw["Threads_connected"], ThreadsRunning: raw["Threads_running"],
		MaxUsedConnections: raw["Max_used_connections"],
	}, nil
}
