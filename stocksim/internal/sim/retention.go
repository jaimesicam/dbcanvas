package sim

import (
	"context"
	"fmt"
	"time"

	"stocksim/internal/store"
)

// The retention agent exists because of one query.
//
// The analytics agent asks for the order book's state every two seconds, which
// on every SQL engine is `SELECT status, COUNT(*) FROM orders GROUP BY status`
// and on Valkey is a read of every order's status. That is fine against a book
// of a few hundred orders. Nothing was ever deleting them, though: the order
// agent creates twenty-five a second at High and the match agent moves them to
// filled, cancelled or rejected, where they sat forever.
//
// Measured on a deployment that had been running a few hours: 615,612,572 rows
// examined across 2,144 executions of that one statement, 1,937 seconds of CPU —
// 71,783 rows read for every row it returned, and getting linearly worse for as
// long as the deployment lived. It never showed up as disk I/O because the rows
// were all cached, so it hid behind a perfectly healthy buffer pool while being
// the single most expensive thing the database did.
//
// So terminal orders are now swept after a retention window. Open orders are
// never touched — they are the live book, and an old resting limit order is
// exactly the thing a user is looking at. Trades are never touched either: they
// are the permanent audit trail the report is built on, and unlike orders they
// are only ever read on demand.
//
// Deleting rows would make the cumulative counts lie, so it doesn't: every sweep
// folds what it removed into a durable tally, and both the dashboard and the
// report read counts through store.CumulativeOrderCounts.

const (
	// DefaultOrderRetention is how long a settled order is kept. Fifteen minutes
	// holds roughly 22,000 orders at High and 1,800 at Low — small enough that
	// the two-second count is trivial at either, long enough that the orders
	// table still shows a real history rather than only the last few seconds.
	DefaultOrderRetention = 15 * time.Minute
	// retentionSweepEvery is the gap between sweeps. Far longer than the two
	// seconds the count runs at, because the table only has to stay bounded,
	// not be exact.
	retentionSweepEvery = 30 * time.Second
	// retentionBatch caps one delete. The first sweep against a deployment that
	// has been running for hours has a lot to get through, and doing it in one
	// statement would hold locks across the whole of it.
	retentionBatch = 5000
	// retentionMaxBatchesPerSweep bounds the catch-up work per pass, so a large
	// backlog is cleared over a few sweeps instead of monopolising a connection.
	retentionMaxBatchesPerSweep = 8
)

// RetentionStatus is what the dashboard shows about the sweep.
type RetentionStatus struct {
	Enabled     bool   `json:"enabled"`
	WindowSec   int64  `json:"windowSeconds"`
	PrunedTotal int64  `json:"prunedTotal"`
	LastSweep   int64  `json:"lastSweepDeleted"`
	Note        string `json:"note"`
}

func (e *Engine) setRetention(s RetentionStatus) {
	e.mu.Lock()
	e.retention = s
	e.mu.Unlock()
}

// Retention returns the current status for BuildSnapshot.
func (e *Engine) Retention() RetentionStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.retention
}

// runRetentionAgent sweeps settled orders older than the retention window.
func (e *Engine) runRetentionAgent(ctx context.Context) {
	window := e.OrderRetention
	if window <= 0 {
		e.setRetention(RetentionStatus{Note: "retention is off — the orders table will grow without limit"})
		e.heartbeat(ctx, "retention", "idle", "disabled")
		return
	}

	total := int64(0)
	if tally, err := store.PrunedTally(ctx, e.Store); err == nil {
		for _, n := range tally {
			total += n
		}
	}

	tickLoop(ctx, retentionSweepEvery, func() {
		if !e.Running() {
			e.heartbeat(ctx, "retention", "idle", "paused")
			return
		}
		cutoff := time.Now().UTC().Add(-window)
		swept := int64(0)
		for i := 0; i < retentionMaxBatchesPerSweep; i++ {
			deleted, err := e.Store.PruneOrders(ctx, cutoff, retentionBatch)
			if err != nil {
				e.noteErr("retention: prune", err)
				e.heartbeat(ctx, "retention", "error", err.Error())
				return
			}
			var n int64
			for _, c := range deleted {
				n += c
			}
			if n == 0 {
				break
			}
			// Fold into the durable tally before counting it locally, so a
			// crash between the two under-reports rather than inventing rows.
			if err := store.AddPrunedTally(ctx, e.Store, deleted); err != nil {
				e.noteErr("retention: tally", err)
			}
			swept += n
			total += n
			if n < int64(retentionBatch) {
				break // caught up: the last batch was not full
			}
		}
		e.setRetention(RetentionStatus{
			Enabled: true, WindowSec: int64(window.Seconds()),
			PrunedTotal: total, LastSweep: swept,
			Note: fmt.Sprintf("settled orders kept for %s — %s removed so far",
				compactDuration(window), compactCount(total)),
		})
		e.heartbeat(ctx, "retention", "ok",
			fmt.Sprintf("%d removed this sweep, %s total", swept, compactCount(total)))
	})
}
