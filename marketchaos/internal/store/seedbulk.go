package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// seedBatchSize is how many rows go into one multi-row INSERT statement.
// Each statement is its own implicit transaction (autocommit) — deliberately
// NOT batched into larger multi-statement transactions: on a PXC target,
// every statement here becomes one Galera writeset, and keeping each
// writeset to a single ~1,000-row INSERT is what keeps certification cheap
// regardless of how large the *total* seed is. A LOAD DATA-based loader
// would be faster but would need to flip the server's global local_infile
// setting (off by default in MySQL 8) as a side effect on the learner's own
// database, and would produce one enormous writeset per file on PXC — both
// deliberately avoided; see IMPLEMENTATION.md for the seeding strategy notes.
const seedBatchSize = 1000

// BulkInsert writes rowCount rows into table via batched multi-row INSERT
// statements, using workers goroutines each claiming a contiguous shard of
// the row-index range. rowFn(i) returns the column values for logical row i
// (0-indexed) in the same order as cols; it's called concurrently across
// workers and must be safe for that (pass values already computed
// per-index, not shared mutable state). progress, if non-nil, is called
// after each completed batch with the cumulative rows-written count so far
// across all workers.
func (s *Store) BulkInsert(ctx context.Context, table string, cols []string, rowCount, workers int, rowFn func(i int) []any, progress func(done int)) error {
	if rowCount <= 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	if workers > rowCount {
		workers = rowCount
	}

	placeholders := "(" + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + ")"
	insertPrefix := fmt.Sprintf("INSERT INTO `%s` (`%s`) VALUES ", table, strings.Join(cols, "`,`"))

	var done atomic.Int64
	var firstErr error
	var errMu sync.Mutex
	setErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}

	shard := (rowCount + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * shard
		end := start + shard
		if end > rowCount {
			end = rowCount
		}
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			for batchStart := start; batchStart < end; batchStart += seedBatchSize {
				if ctx.Err() != nil {
					setErr(ctx.Err())
					return
				}
				batchEnd := batchStart + seedBatchSize
				if batchEnd > end {
					batchEnd = end
				}
				n := batchEnd - batchStart
				args := make([]any, 0, n*len(cols))
				var sb strings.Builder
				sb.WriteString(insertPrefix)
				for i := batchStart; i < batchEnd; i++ {
					if i > batchStart {
						sb.WriteByte(',')
					}
					sb.WriteString(placeholders)
					args = append(args, rowFn(i)...)
				}
				if _, err := s.DB.ExecContext(ctx, sb.String(), args...); err != nil {
					setErr(fmt.Errorf("bulk insert %s [%d,%d): %w", table, batchStart, batchEnd, err))
					return
				}
				total := done.Add(int64(n))
				if progress != nil {
					progress(int(total))
				}
			}
		}(start, end)
	}
	wg.Wait()
	return firstErr
}
