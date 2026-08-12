package sim

import (
	"context"
	"errors"
	"fmt"
	"time"

	"stocksim/internal/store"
)

// The agents whose only job is to make the database exhibit a condition worth
// measuring. Each pairs with something a monitoring tool already reports and
// that is otherwise hard to produce on demand:
//
//	idle transaction -> InnoDB history list length / PostgreSQL xmin horizon
//	extra tables     -> table_open_cache misses, Opened_tables
//	temp tables      -> Created_tmp_disk_tables, PostgreSQL temp_files
//	lock contention  -> Innodb_row_lock_waits, deadlocks, pg_locks
//	scan queries     -> Handler_read_rnd_next, seq_tup_read, slow queries
//	write pressure   -> Innodb_os_log_fsyncs, checkpoint age, WAL bytes
//
// The first three are here; the last three are in labstress.go. All are off
// unless configured, and none of them touches the trading data: the idle
// transaction parks on its own row, the synthetic tables are separate objects,
// the contended and committed writes go to lab_hotrows, the bulk rewrites go to
// lab_bulk, and both query knobs only read.

const (
	// labIdleGap is the pause between one idle transaction ending and the next
	// beginning, so a short hold produces a repeating sawtooth in the history
	// list rather than one spike and silence.
	labIdleGap = 15 * time.Second
	// labTableTouchEvery is how often the table-cache agent sweeps a batch.
	labTableTouchEvery = 2 * time.Second
	// labTableTouchBatch is how many tables one sweep opens. Larger than any
	// sane table_open_cache is not the goal — churning through more distinct
	// tables than the cache holds, repeatedly, is.
	labTableTouchBatch = 200
)

// tempQueryEvery is how often the temporary-table query runs at each level.
// Frequent enough to show on a chart, rare enough that it does not become the
// entire workload.
var tempQueryEvery = map[string]time.Duration{
	LevelLow:    30 * time.Second,
	LevelMedium: 10 * time.Second,
	LevelHigh:   3 * time.Second,
}

// LabStatus is what the dashboard shows about all three.
type LabStatus struct {
	// IdleTxn
	IdleTxnEnabled bool   `json:"idleTxnEnabled"`
	IdleTxnSeconds int64  `json:"idleTxnSeconds"`
	IdleTxnHolding bool   `json:"idleTxnHolding"`
	IdleTxnHeldFor int64  `json:"idleTxnHeldForSeconds"`
	IdleTxnHolds   int64  `json:"idleTxnHolds"`
	IdleTxnNote    string `json:"idleTxnNote"`
	// Extra tables
	TablesWanted int    `json:"tablesWanted"`
	TablesHave   int    `json:"tablesHave"`
	TablesTouchd int64  `json:"tablesTouched"`
	TablesNote   string `json:"tablesNote"`
	// Temp tables
	TempMode    string `json:"tempMode"`
	TempRuns    int64  `json:"tempRuns"`
	TempSpills  int64  `json:"tempSpills"`
	TempLastMs  int64  `json:"tempLastMs"`
	TempLastRow int    `json:"tempLastRows"`
	TempNote    string `json:"tempNote"`
	// Lock contention
	ContentionMode    string `json:"contentionMode"`
	ContentionWorkers int    `json:"contentionWorkers"`
	ContentionRuns    int64  `json:"contentionRuns"`
	ContentionWaits   int64  `json:"contentionWaits"`
	ContentionDeadlks int64  `json:"contentionDeadlocks"`
	ContentionTimeout int64  `json:"contentionTimeouts"`
	ContentionMaxMs   int64  `json:"contentionMaxWaitMs"`
	ContentionAvgMs   int64  `json:"contentionAvgWaitMs"`
	ContentionNote    string `json:"contentionNote"`
	// Scan queries
	ScanRate     int    `json:"scanRate"`
	ScanRuns     int64  `json:"scanRuns"`
	ScanRowsRead int64  `json:"scanRowsRead"`
	ScanRowsRet  int64  `json:"scanRowsReturned"`
	ScanLastMs   int64  `json:"scanLastMs"`
	ScanNote     string `json:"scanNote"`
	// Write pressure
	WriteMode    string `json:"writeMode"`
	WriteBatches int64  `json:"writeBatches"`
	WriteCommits int64  `json:"writeCommits"`
	WriteSyncs   int64  `json:"writeSyncs"`
	WriteBytes   int64  `json:"writeBytes"`
	WriteMeasurd bool   `json:"writeMeasured"`
	WriteNote    string `json:"writeNote"`
}

// contentionWaitTotal accumulates wait time so the average on the panel is a
// real mean rather than a decaying guess. Kept out of LabStatus because it is
// bookkeeping, not something the dashboard should show.
type labTotals struct {
	contentionWaitMs int64
}

func (e *Engine) setLab(mutate func(*LabStatus)) {
	e.mu.Lock()
	mutate(&e.lab)
	e.mu.Unlock()
}

// Lab returns the current status for BuildSnapshot.
func (e *Engine) Lab() LabStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lab
}

// labStore narrows the store to the optional half, reporting whether this
// engine implements it at all.
func (e *Engine) labStore() (store.LabStore, store.LabSupport, bool) {
	ls, ok := e.Store.(store.LabStore)
	if !ok {
		return nil, store.LabSupport{}, false
	}
	return ls, ls.Capabilities(), true
}

// ------------------------------------------------------- idle transaction

// runIdleTxnAgent holds a transaction open for the configured time, over and
// over, so the history list has something to grow against.
//
// The hold blocks a goroutine for its whole duration — up to a day — which is
// why it gets a connection of its own inside the store rather than one from the
// shared pool. See HoldIdleTransaction.
func (e *Engine) runIdleTxnAgent(ctx context.Context) {
	hold := store.ClampIdleTransaction(e.IdleTxn)
	ls, caps, ok := e.labStore()

	switch {
	case hold <= 0:
		e.setLab(func(s *LabStatus) { s.IdleTxnNote = "no idle transaction configured" })
		e.heartbeat(ctx, "idletxn", "idle", "not configured")
		return
	case !ok || !caps.IdleTransaction:
		note := "not available for " + engineDisplayName(e.Store.Engine()) +
			" — it has no transaction that holds a read snapshot open"
		e.setLab(func(s *LabStatus) { s.IdleTxnNote = note })
		e.heartbeat(ctx, "idletxn", "idle", "not available for this engine")
		return
	}

	e.setLab(func(s *LabStatus) {
		s.IdleTxnEnabled = true
		s.IdleTxnSeconds = int64(hold.Seconds())
	})

	for {
		if ctx.Err() != nil {
			return
		}
		if !e.Running() {
			e.setLab(func(s *LabStatus) { s.IdleTxnHolding = false; s.IdleTxnNote = "paused" })
			e.heartbeat(ctx, "idletxn", "idle", "paused")
			if !sleepCtx(ctx, labIdleGap) {
				return
			}
			continue
		}

		started := time.Now()
		e.setLab(func(s *LabStatus) {
			s.IdleTxnHolding = true
			s.IdleTxnNote = fmt.Sprintf("holding a transaction open for %s — purge cannot advance past it",
				compactDuration(hold))
		})
		// Report progress while it sits there, so the panel is not frozen for
		// what may be hours.
		done := make(chan struct{})
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					held := int64(time.Since(started).Seconds())
					e.setLab(func(s *LabStatus) { s.IdleTxnHeldFor = held })
					e.heartbeat(ctx, "idletxn", "ok",
						fmt.Sprintf("held for %s of %s", compactDuration(time.Since(started)), compactDuration(hold)))
				}
			}
		}()

		err := ls.HoldIdleTransaction(ctx, hold)
		close(done)
		if err != nil && !errors.Is(err, context.Canceled) {
			e.noteErr("idle transaction", err)
			e.heartbeat(ctx, "idletxn", "error", err.Error())
		}
		e.setLab(func(s *LabStatus) {
			s.IdleTxnHolding = false
			s.IdleTxnHeldFor = 0
			if err == nil {
				s.IdleTxnHolds++
			}
			s.IdleTxnNote = fmt.Sprintf("released after %s; next hold in %s",
				compactDuration(time.Since(started)), compactDuration(labIdleGap))
		})
		if !sleepCtx(ctx, labIdleGap) {
			return
		}
	}
}

// ------------------------------------------------------------ extra tables

// runTableCacheAgent creates the requested number of synthetic tables and then
// keeps reading from them in a rotating batch, which is what forces the server
// to keep opening table handles it has just evicted.
func (e *Engine) runTableCacheAgent(ctx context.Context) {
	want := store.ClampExtraTables(e.ExtraTables)
	ls, caps, ok := e.labStore()

	switch {
	case want <= 0:
		e.setLab(func(s *LabStatus) { s.TablesNote = "no extra tables configured" })
		e.heartbeat(ctx, "tablecache", "idle", "not configured")
		return
	case !ok || !caps.ExtraTables:
		note := "not available for " + engineDisplayName(e.Store.Engine()) +
			" — it has no per-table handle to cache"
		e.setLab(func(s *LabStatus) { s.TablesNote = note })
		e.heartbeat(ctx, "tablecache", "idle", "not available for this engine")
		return
	}

	e.setLab(func(s *LabStatus) {
		s.TablesWanted = want
		s.TablesNote = fmt.Sprintf("creating %d tables…", want)
	})
	e.heartbeat(ctx, "tablecache", "ok", fmt.Sprintf("creating %d tables", want))

	have, err := ls.EnsureExtraTables(ctx, want)
	if err != nil {
		e.noteErr("table cache: create", err)
		e.setLab(func(s *LabStatus) { s.TablesNote = "could not create: " + err.Error() })
		e.heartbeat(ctx, "tablecache", "error", err.Error())
		return
	}
	names := store.ExtraTableNames(have)
	e.setLab(func(s *LabStatus) {
		s.TablesHave = have
		s.TablesNote = fmt.Sprintf("%d tables, reading %d of them every %s",
			have, labTableTouchBatch, compactDuration(labTableTouchEvery))
	})
	e.emit(ctx, "system", "", fmt.Sprintf(
		"Table cache load: %d extra tables created and being read in rotation", have))

	// Walk the whole set in batches rather than picking at random, so every
	// table is opened once per cycle — random picking would keep re-opening a
	// hot subset the cache would simply hold.
	pos := 0
	tickLoop(ctx, labTableTouchEvery, func() {
		if !e.Running() || len(names) == 0 {
			return
		}
		end := pos + labTableTouchBatch
		if end > len(names) {
			end = len(names)
		}
		batch := names[pos:end]
		pos = end
		if pos >= len(names) {
			pos = 0
		}
		if err := ls.TouchExtraTables(ctx, batch); err != nil {
			e.noteErr("table cache: touch", err)
			e.heartbeat(ctx, "tablecache", "error", err.Error())
			return
		}
		e.setLab(func(s *LabStatus) { s.TablesTouchd += int64(len(batch)) })
		e.heartbeat(ctx, "tablecache", "ok",
			fmt.Sprintf("%d of %d tables open per sweep", len(batch), len(names)))
	})
}

// ------------------------------------------------------------ temp tables

// runTempTableAgent runs an aggregation shaped to build a large intermediate
// result, at the level's cadence, in whichever mode was asked for.
func (e *Engine) runTempTableAgent(ctx context.Context) {
	mode := e.TempTables
	ls, caps, ok := e.labStore()

	switch {
	case mode == "" || mode == store.TempOff:
		e.setLab(func(s *LabStatus) { s.TempMode = store.TempOff; s.TempNote = "no temporary-table query configured" })
		e.heartbeat(ctx, "temptables", "idle", "not configured")
		return
	case !ok || !caps.TempTables:
		note := "not available for " + engineDisplayName(e.Store.Engine()) +
			" — it has no query planner that materialises intermediate results"
		e.setLab(func(s *LabStatus) { s.TempMode = mode; s.TempNote = note })
		e.heartbeat(ctx, "temptables", "idle", "not available for this engine")
		return
	}

	e.setLab(func(s *LabStatus) { s.TempMode = mode })
	rnd := newAgentRand()
	for {
		if ctx.Err() != nil {
			return
		}
		every, okLevel := tempQueryEvery[e.Level()]
		if !okLevel {
			every = 10 * time.Second
		}
		if !e.Running() {
			e.heartbeat(ctx, "temptables", "idle", "paused")
			if !sleepCtx(ctx, every) {
				return
			}
			continue
		}

		res, err := ls.RunTempTableQuery(ctx, mode)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			e.noteErr("temp table query", err)
			e.heartbeat(ctx, "temptables", "error", err.Error())
		} else {
			e.setLab(func(s *LabStatus) {
				s.TempRuns++
				if res.Spilled {
					s.TempSpills++
				}
				s.TempLastMs = res.Duration.Milliseconds()
				s.TempLastRow = res.Rows
				where := "in memory"
				if res.Spilled {
					where = "spilled to disk"
				}
				s.TempNote = fmt.Sprintf("%s — %d rows %s in %s",
					res.Description, res.Rows, where, res.Duration.Round(time.Millisecond))
			})
			e.heartbeat(ctx, "temptables", "ok",
				fmt.Sprintf("%d rows, %s, %dms", res.Rows,
					map[bool]string{true: "on disk", false: "in memory"}[res.Spilled],
					res.Duration.Milliseconds()))
		}
		// A little jitter so this never lines up with the analytics agent's own
		// two-second beat and reads as one combined spike.
		if !sleepCtx(ctx, every+time.Duration(rnd.Intn(500))*time.Millisecond) {
			return
		}
	}
}
