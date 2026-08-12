package sim

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"stocksim/internal/store"
)

// The second set of lab knobs: contention, scans, and write pressure. See the
// header of lab.go for what all six are for and what each one lights up.
//
// These three differ from the first three in one structural way. An idle
// transaction is one goroutine sitting still, and the table-cache sweep is one
// goroutine walking a list — but contention does not exist with a single
// writer, so that agent is the only one here that fans out. The other two stay
// single-threaded on purpose: a scan and a commit storm are both about rate,
// and a rate is easier to hold to a number from one place.

const (
	// contentionSettle is the pause after a deadlock before that worker tries
	// again. Without it, a rolled-back worker re-enters the same cycle
	// immediately and the deadlock rate becomes a function of round-trip time
	// rather than of the workload.
	contentionSettle = 250 * time.Millisecond
	// contentionGap is the pause between one worker's successful writes. Small,
	// because the hold inside the transaction is what creates the wait.
	contentionGap = 10 * time.Millisecond
	// writePressureEvery is how often a write-pressure batch is issued.
	writePressureEvery = time.Second
)

// commitBatch is how many tiny transactions one batch of "commits" mode runs at
// each level, and redoBatch how many wide rows one "redo" transaction rewrites.
// Both are per second — see writePressureEvery.
var (
	commitBatch = map[string]int{
		LevelLow:    100,
		LevelMedium: 400,
		LevelHigh:   1200,
	}
	redoBatch = map[string]int{
		LevelLow:    64,
		LevelMedium: 256,
		LevelHigh:   768,
	}
)

// ------------------------------------------------------- lock contention

// runContentionAgent starts several writers that fight over a small set of
// rows.
//
// One writer cannot contend with anything, so this is the one lab agent that
// fans out: store.ContentionWorkers decides how many, and every one of them is
// given a distinct worker number because that number is what decides which rows
// it takes and in which order. In Heavy mode the odd-numbered workers take their
// two rows in the opposite order to the even-numbered ones, which is the cycle
// that produces a real deadlock rather than a long wait.
func (e *Engine) runContentionAgent(ctx context.Context) {
	mode := e.LockContention
	ls, caps, ok := e.labStore()

	switch {
	case mode == "" || mode == store.ContentionOff:
		e.setLab(func(s *LabStatus) {
			s.ContentionMode = store.ContentionOff
			s.ContentionNote = "no lock contention configured"
		})
		e.heartbeat(ctx, "contention", "idle", "not configured")
		return
	case !ok || !caps.LockContention:
		note := "not available for " + engineDisplayName(e.Store.Engine()) +
			" — it has no row lock a writer can take and hold"
		e.setLab(func(s *LabStatus) { s.ContentionMode = mode; s.ContentionNote = note })
		e.heartbeat(ctx, "contention", "idle", "not available for this engine")
		return
	}

	if err := ls.EnsureLabTables(ctx); err != nil {
		e.noteErr("lock contention: prepare", err)
		e.setLab(func(s *LabStatus) {
			s.ContentionMode = mode
			s.ContentionNote = "could not prepare the hot rows: " + err.Error()
		})
		e.heartbeat(ctx, "contention", "error", err.Error())
		return
	}

	workers, rows := store.ContentionWorkers(mode), store.ContentionRows(mode)
	e.setLab(func(s *LabStatus) {
		s.ContentionMode = mode
		s.ContentionWorkers = workers
		s.ContentionNote = fmt.Sprintf("%d writers competing for %d rows", workers, rows)
	})
	e.emit(ctx, "system", "", fmt.Sprintf(
		"Lock contention (%s): %d writers competing for %d rows", mode, workers, rows))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			e.contentionWorker(ctx, ls, mode, worker)
		}(i)
	}
	wg.Wait()
}

// contentionWorker is one writer's loop.
func (e *Engine) contentionWorker(ctx context.Context, ls store.LabStore, mode string, worker int) {
	for {
		if ctx.Err() != nil {
			return
		}
		if !e.Running() {
			if !sleepCtx(ctx, contentionGap*10) {
				return
			}
			continue
		}

		res, err := ls.RunContendedUpdate(ctx, mode, worker)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			e.noteErr("lock contention", err)
			e.heartbeat(ctx, "contention", "error", err.Error())
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}

		waitMs := res.Waited.Milliseconds()
		e.mu.Lock()
		e.lab.ContentionRuns++
		// Only waits of a millisecond or more are counted, and the note says so.
		// Below that this cannot separate a lock wait from the round trip that
		// carried it, so counting them would inflate the number with latency.
		//
		// This is deliberately *lower* than the server's own
		// Innodb_row_lock_waits, which counts every wait however brief — a live
		// run showed 4,657 there against 422 here. The two are both right and
		// answer different questions; the label is what keeps them from looking
		// like a contradiction.
		if waitMs > 0 {
			e.lab.ContentionWaits++
			e.totals.contentionWaitMs += waitMs
			if waitMs > e.lab.ContentionMaxMs {
				e.lab.ContentionMaxMs = waitMs
			}
		}
		if res.Deadlock {
			e.lab.ContentionDeadlks++
		}
		if res.Timeout {
			e.lab.ContentionTimeout++
		}
		if e.lab.ContentionWaits > 0 {
			e.lab.ContentionAvgMs = e.totals.contentionWaitMs / e.lab.ContentionWaits
		}
		e.lab.ContentionNote = contentionNote(e.lab)
		deadlocks, timeouts := e.lab.ContentionDeadlks, e.lab.ContentionTimeout
		e.mu.Unlock()

		e.heartbeat(ctx, "contention", "ok", fmt.Sprintf(
			"waited %dms, %d deadlocks, %d timeouts", waitMs, deadlocks, timeouts))

		// Back off after losing a deadlock, so the retry does not simply
		// re-form the cycle it was just broken out of.
		gap := contentionGap
		if res.Deadlock {
			gap = contentionSettle
		}
		if !sleepCtx(ctx, gap) {
			return
		}
	}
}

func contentionNote(s LabStatus) string {
	note := fmt.Sprintf("%d writes, %d waited ≥1ms (avg %dms, worst %dms)",
		s.ContentionRuns, s.ContentionWaits, s.ContentionAvgMs, s.ContentionMaxMs)
	if s.ContentionDeadlks > 0 {
		note += fmt.Sprintf(", %d deadlocks", s.ContentionDeadlks)
	}
	if s.ContentionTimeout > 0 {
		note += fmt.Sprintf(", %d lock timeouts", s.ContentionTimeout)
	}
	return note
}

// ------------------------------------------------------------ scan queries

// runScanAgent issues an index-less read at the configured rate.
//
// The rate is per minute rather than per second because this is the most
// expensive single thing the application can ask a server to do — one scan of a
// multi-gigabyte tick history — and a per-second knob would invite a number
// that makes everything else on the chart unreadable.
func (e *Engine) runScanAgent(ctx context.Context) {
	rate := store.ClampScanRate(e.ScanRate)
	ls, caps, ok := e.labStore()

	switch {
	case rate <= 0:
		e.setLab(func(s *LabStatus) { s.ScanNote = "no scan queries configured" })
		e.heartbeat(ctx, "scans", "idle", "not configured")
		return
	case !ok || !caps.ScanQueries:
		note := "not available for " + engineDisplayName(e.Store.Engine()) +
			" — it has no planner that can be made to read every row"
		e.setLab(func(s *LabStatus) { s.ScanRate = rate; s.ScanNote = note })
		e.heartbeat(ctx, "scans", "idle", "not available for this engine")
		return
	}

	every := time.Minute / time.Duration(rate)
	e.setLab(func(s *LabStatus) {
		s.ScanRate = rate
		// Said as a rate rather than an interval: at 60 a minute the interval is
		// exactly one second, and the shared duration formatter renders that as
		// "1 seconds".
		s.ScanNote = fmt.Sprintf("%d full scans a minute, starting", rate)
	})
	e.emit(ctx, "system", "", fmt.Sprintf(
		"Scan queries: %d per minute against the tick history, with no index to serve them", rate))

	tickLoop(ctx, every, func() {
		if !e.Running() {
			e.heartbeat(ctx, "scans", "idle", "paused")
			return
		}
		res, err := ls.RunScanQuery(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			e.noteErr("scan query", err)
			e.heartbeat(ctx, "scans", "error", err.Error())
			return
		}
		e.setLab(func(s *LabStatus) {
			s.ScanRuns++
			s.ScanRowsRead += res.RowsRead
			s.ScanRowsRet += int64(res.Rows)
			s.ScanLastMs = res.Duration.Milliseconds()
			s.ScanNote = scanNote(res)
		})
		e.heartbeat(ctx, "scans", "ok", fmt.Sprintf(
			"%d rows read to return %d, %dms", res.RowsRead, res.Rows, res.Duration.Milliseconds()))
	})
}

// scanNote states the ratio the knob exists to demonstrate: how many rows the
// server read to produce how few.
func scanNote(res store.ScanResult) string {
	if !res.Scanned {
		// Worth saying plainly rather than hiding: if an index turned up, or the
		// counter could not be read, the run did not demonstrate what it claims.
		return fmt.Sprintf("%s — returned %d rows, but no scan was recorded",
			res.Description, res.Rows)
	}
	note := fmt.Sprintf("%s — read %s rows to return %d",
		res.Description, formatCount(res.RowsRead), res.Rows)
	if res.Rows > 0 && res.RowsRead > int64(res.Rows) {
		note += fmt.Sprintf(" (%.0f read per row returned)",
			float64(res.RowsRead)/float64(res.Rows))
	}
	return note
}

// ---------------------------------------------------------- write pressure

// runWritePressureAgent issues one batch of writes a second, in whichever of the
// two shapes was asked for.
//
// The two shapes cost different things, which is the reason they are one knob
// with two modes rather than two knobs:
//
//	commits  many tiny transactions, one log flush each -> fsyncs/sec
//	redo     one transaction rewriting many wide rows   -> checkpoint age
//
// Neither grows anything. The commit mode updates a counter on rows it owns and
// the redo mode overwrites a fixed set of wide rows in place, so a deployment
// left running for a week generates unbounded write volume against a table that
// is exactly as big as it was on the first day.
func (e *Engine) runWritePressureAgent(ctx context.Context) {
	mode := e.WritePressure
	ls, caps, ok := e.labStore()

	switch {
	case mode == "" || mode == store.WritePressureOff:
		e.setLab(func(s *LabStatus) {
			s.WriteMode = store.WritePressureOff
			s.WriteNote = "no write pressure configured"
		})
		e.heartbeat(ctx, "writepressure", "idle", "not configured")
		return
	case !ok || !caps.WritePressure:
		note := "not available for " + engineDisplayName(e.Store.Engine()) +
			" — it has no per-commit durable flush for this to cost anything"
		e.setLab(func(s *LabStatus) { s.WriteMode = mode; s.WriteNote = note })
		e.heartbeat(ctx, "writepressure", "idle", "not available for this engine")
		return
	}

	if err := ls.EnsureLabTables(ctx); err != nil {
		e.noteErr("write pressure: prepare", err)
		e.setLab(func(s *LabStatus) {
			s.WriteMode = mode
			s.WriteNote = "could not prepare the target rows: " + err.Error()
		})
		e.heartbeat(ctx, "writepressure", "error", err.Error())
		return
	}

	e.setLab(func(s *LabStatus) { s.WriteMode = mode })
	e.emit(ctx, "system", "", "Write pressure: "+writePressureIntro(mode))

	tickLoop(ctx, writePressureEvery, func() {
		if !e.Running() {
			e.heartbeat(ctx, "writepressure", "idle", "paused")
			return
		}
		n := writeBatchSize(mode, e.Level())
		// Nine tenths of the interval, so a batch that cannot reach its
		// requested rate still finishes before the next tick is due instead of
		// queueing behind itself. See RunWritePressure on why this exists.
		res, err := ls.RunWritePressure(ctx, mode, n, writePressureEvery*9/10)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			e.noteErr("write pressure", err)
			e.heartbeat(ctx, "writepressure", "error", err.Error())
			return
		}
		e.setLab(func(s *LabStatus) {
			s.WriteBatches++
			s.WriteCommits += int64(res.Commits)
			s.WriteMeasurd = res.SyncsKnown
			if res.SyncsKnown {
				s.WriteSyncs += res.Syncs
			}
			s.WriteBytes += res.Bytes
			s.WriteNote = writeNote(res)
		})
		e.heartbeat(ctx, "writepressure", "ok", fmt.Sprintf(
			"%d commits, %d syncs, %dms", res.Commits, res.Syncs, res.Duration.Milliseconds()))
	})
}

func writeBatchSize(mode, level string) int {
	table := commitBatch
	if mode == store.WritePressureRedo {
		table = redoBatch
	}
	if n, ok := table[level]; ok {
		return n
	}
	return table[LevelMedium]
}

func writePressureIntro(mode string) string {
	if mode == store.WritePressureRedo {
		return "bulk rewrites of wide rows, to fill the write-ahead log and shorten checkpoint headroom"
	}
	return "many tiny transactions, to make every commit pay for its own log flush"
}

// writeNote says what the batch cost. When the log counter could not be read it
// says so rather than showing a zero — a measurement that could not be taken
// must not read as a measurement of nothing happening (see §239).
func writeNote(res store.WriteResult) string {
	head := res.Description
	if !res.SyncsKnown {
		return head + " — this server does not expose a log-sync counter, so the fsync cost is not measured"
	}
	return fmt.Sprintf("%s — %d log syncs, %s written in %s",
		head, res.Syncs, FormatBytes(res.Bytes), res.Duration.Round(time.Millisecond))
}

// formatCount abbreviates the large row counts a scan produces, so a note reads
// "41.2M rows" rather than a wall of digits.
func formatCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}
