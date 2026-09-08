package sim

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"stocksim/internal/store"
)

// The backfill agent exists for one reason the other six do not share: disk.
//
// Left to itself this simulation is tiny. The price agent writes one tick per
// listed security per second — twenty rows a second against the seeded
// universe, a few hundred megabytes a day — which is the right size for
// watching CRUD and a report work, and useless for finding out how a volume,
// a storage class or a backup behaves under load. Reaching a few gigabytes
// organically would take weeks.
//
// So at the High load level, and only there, this agent writes *historical*
// ticks in bulk until the app's own footprint reaches TargetBytes, then stops
// and lets the simulation carry on at its normal rate. Historical, walking
// backwards from the moment the backfill started, because the dashboard's
// sparklines and the report read the newest rows: filling in history makes the
// database big without making the application lie about what just happened.
//
// It is deliberately not rate-limited. The point of the exercise is to find
// out what the storage underneath does when a database is written to as fast
// as it will accept writes, and a throttle would be measuring the throttle.
// Backpressure comes from the store: every writer below blocks on its INSERT.

const (
	// backfillBatchRows is rows per statement. PostgreSQL's protocol caps a
	// statement at 65535 bind parameters and a tick binds six, so the ceiling
	// is 10922; a thousand is comfortably under it and large enough that
	// per-statement overhead stops mattering.
	backfillBatchRows = 1000
	// backfillMeasureEvery is how often the footprint is re-measured. Sizes
	// come from the same catalogue query the Schema panel uses, so this is a
	// real measurement rather than a running estimate, and the target is met
	// on the server's own accounting rather than on ours.
	backfillMeasureEvery = 5 * time.Second
	// backfillIdlePoll is the pause when there is nothing to do — wrong load
	// level, paused, or already at target.
	backfillIdlePoll = 2 * time.Second
)

// DefaultTargetBytes is the size a High-level deployment grows to when no
// explicit target is configured. Five gibibytes: past the point where a
// database is being served entirely out of the page cache on a normal lab
// machine, which is where disk behaviour starts to be the thing under test.
const DefaultTargetBytes int64 = 5 << 30

// BackfillStatus is what the dashboard shows about all this. Reported even
// when the agent is off, because "off, and here is why" is the answer to the
// question a user asks when the database is not growing.
type BackfillStatus struct {
	Supported   bool   `json:"supported"`
	TargetBytes int64  `json:"targetBytes"`
	Bytes       int64  `json:"bytes"`
	RowsWritten int64  `json:"rowsWritten"`
	Active      bool   `json:"active"`
	Reached     bool   `json:"reached"`
	Note        string `json:"note"`
}

func (e *Engine) setBackfill(s BackfillStatus) {
	e.mu.Lock()
	e.backfill = s
	e.mu.Unlock()
}

// Backfill returns the current status for BuildSnapshot.
func (e *Engine) Backfill() BackfillStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.backfill
}

// runBackfillAgent is the loop described at the top of this file.
func (e *Engine) runBackfillAgent(ctx context.Context) {
	supported := store.CanGrowToSize(e.Store.Engine())
	target := e.TargetBytes

	switch {
	case !supported:
		e.setBackfill(BackfillStatus{Note: "not available for " +
			engineDisplayName(e.Store.Engine()) + " — its tick history is a capped stream"})
		e.heartbeat(ctx, "backfill", "idle", "not available for this engine")
		return
	case target <= 0:
		e.setBackfill(BackfillStatus{Supported: true, Note: "no size target set"})
		e.heartbeat(ctx, "backfill", "idle", "no size target set")
		return
	}

	rnd := newAgentRand()
	// cursor is the timestamp of the history being written, walking backwards
	// from where the simulation started so it never collides with live ticks.
	cursor := time.Now().UTC().Add(-time.Hour)

	var (
		bytes      int64
		measuredAt time.Time
		secs       []store.Security
		secsAt     time.Time
		announced  bool
	)

	for {
		if ctx.Err() != nil {
			return
		}

		if !e.Running() || e.Level() != LevelHigh {
			why := "load level is " + e.Level() + ", not high"
			if !e.Running() {
				why = "paused"
			}
			e.setBackfill(BackfillStatus{
				Supported: true, TargetBytes: target, Bytes: bytes,
				RowsWritten: e.counters.ticksBackfilled.Load(), Note: why,
			})
			e.heartbeat(ctx, "backfill", "idle", why)
			if !sleepCtx(ctx, backfillIdlePoll) {
				return
			}
			continue
		}

		// Four writers get through something like 200 MiB between measurements
		// — proportionally more with more threads — so measuring on a fixed
		// interval all the way to the target overshoots it by that much. Close
		// to the line, measure after every round instead:
		// the query is cheap next to the writes, and it bounds the overshoot to
		// one round rather than one interval.
		measureEvery := backfillMeasureEvery
		if target > 0 && bytes*4 >= target*3 {
			measureEvery = 0
		}
		if time.Since(measuredAt) >= measureEvery {
			n, err := e.measureFootprint(ctx)
			if err != nil {
				e.noteErr("backfill: measure", err)
				e.heartbeat(ctx, "backfill", "error", err.Error())
				if !sleepCtx(ctx, backfillIdlePoll) {
					return
				}
				continue
			}
			bytes, measuredAt = n, time.Now()
		}

		rows := e.counters.ticksBackfilled.Load()
		if bytes >= target {
			e.setBackfill(BackfillStatus{
				Supported: true, TargetBytes: target, Bytes: bytes,
				RowsWritten: rows, Reached: true,
				Note: fmt.Sprintf("target reached — %s of %s", FormatBytes(bytes), FormatBytes(target)),
			})
			e.heartbeat(ctx, "backfill", "ok",
				fmt.Sprintf("at target, %s", FormatBytes(bytes)))
			if !announced {
				announced = true
				e.emit(ctx, "system", "", fmt.Sprintf(
					"Backfill complete: %s of history written, target was %s",
					FormatBytes(bytes), FormatBytes(target)))
			}
			// Nothing to do until the target moves or the data shrinks, but a
			// wipe does shrink it, so keep looking.
			if !sleepCtx(ctx, backfillMeasureEvery) {
				return
			}
			measuredAt = time.Time{}
			continue
		}
		announced = false

		if time.Since(secsAt) >= 30*time.Second || len(secs) == 0 {
			list, _, err := e.Store.ListSecurities(ctx, store.ListQuery{Limit: 500})
			if err != nil {
				e.noteErr("backfill: list securities", err)
				if !sleepCtx(ctx, backfillIdlePoll) {
					return
				}
				continue
			}
			secs, secsAt = list, time.Now()
		}
		if len(secs) == 0 {
			e.heartbeat(ctx, "backfill", "idle", "no securities to write history for")
			if !sleepCtx(ctx, backfillIdlePoll) {
				return
			}
			continue
		}

		written := e.backfillRound(ctx, secs, &cursor, rnd)
		e.counters.ticksBackfilled.Add(int64(written))
		rows += int64(written)

		pct := float64(bytes) / float64(target) * 100
		e.setBackfill(BackfillStatus{
			Supported: true, TargetBytes: target, Bytes: bytes,
			RowsWritten: rows, Active: true,
			Note: fmt.Sprintf("%s of %s (%.1f%%)", FormatBytes(bytes), FormatBytes(target), pct),
		})
		e.heartbeat(ctx, "backfill", "ok",
			fmt.Sprintf("%s of %s, %s rows", FormatBytes(bytes), FormatBytes(target), compactCount(rows)))

		if written == 0 {
			// Every writer failed — do not spin on a database that is refusing
			// writes; the error is already on the dashboard.
			if !sleepCtx(ctx, backfillIdlePoll) {
				return
			}
		}
	}
}

// backfillRound writes one batch per configured thread concurrently and returns
// how many rows actually landed. cursor is advanced backwards by the whole round
// before any writer starts, so the writers cannot hand out the same timestamps.
//
// The writer count is Engine.Threads: enough concurrency to keep a disk busy,
// and raising it is how a user asks a target that can take more to be given
// more. The store's connection pool is sized from the same number, so the
// writers never starve the seven other agents or the operator's own CRUD.
func (e *Engine) backfillRound(ctx context.Context, secs []store.Security, cursor *time.Time, rnd *rand.Rand) int {
	type slice struct {
		start time.Time
		seed  int64
	}
	// One tick per security per second of history, which is the cadence the
	// live price agent writes at — so a batch of backfillBatchRows rows covers
	// this many seconds, and that is how far back the next slice must start.
	span := time.Duration(backfillBatchRows/len(secs)+1) * time.Second
	slices := make([]slice, store.ClampThreads(e.Threads))
	for i := range slices {
		slices[i] = slice{start: *cursor, seed: rnd.Int63()}
		*cursor = cursor.Add(-span)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		written int
	)
	for _, sl := range slices {
		wg.Add(1)
		go func(sl slice) {
			defer wg.Done()
			ticks := buildHistory(secs, sl.start, backfillBatchRows, rand.New(rand.NewSource(sl.seed)))
			if err := e.Store.AppendTicks(ctx, ticks); err != nil {
				e.noteErr("backfill: append ticks", err)
				return
			}
			mu.Lock()
			written += len(ticks)
			mu.Unlock()
		}(sl)
	}
	wg.Wait()
	return written
}

// buildHistory generates n plausible past ticks ending at end and walking
// backwards one second at a time. Prices random-walk away from each security's
// current price using the same per-sector volatility the live price agent
// uses, so a backfilled chart and a live one are drawn from the same process.
func buildHistory(secs []store.Security, end time.Time, n int, rnd *rand.Rand) []store.Tick {
	if len(secs) == 0 {
		return nil
	}
	last := make([]float64, len(secs))
	for i, s := range secs {
		last[i] = s.LastPrice
		if last[i] <= 0 {
			last[i] = 1
		}
	}
	out := make([]store.Tick, 0, n)
	ts := end
	for i := 0; i < n; i++ {
		j := i % len(secs)
		s := secs[j]
		p := last[j] * (1 + rnd.NormFloat64()*sectorVolatility(s.Sector))
		if p < 0.01 {
			p = 0.01
		}
		last[j] = p
		out = append(out, store.Tick{
			SecurityID: s.ID, Symbol: s.Symbol, TS: ts,
			Price: round2(p), Volume: int64(rnd.Intn(9000) + 100),
		})
		if j == len(secs)-1 {
			ts = ts.Add(-time.Second)
		}
	}
	return out
}

// measureFootprint totals what this app currently occupies on the server,
// from the same Objects() call the Schema panel and the monitoring agent use.
func (e *Engine) measureFootprint(ctx context.Context) (int64, error) {
	objs, err := e.Store.Objects(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, o := range objs {
		total += o.Bytes
	}
	return total, nil
}

// sleepCtx waits for d, reporting false if the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ParseSize reads a byte size written the way a person writes one: "5G",
// "512Mi", "2 TB", or a plain byte count. Both the decimal and binary suffixes
// are accepted and both mean the binary quantity — a lab tool arguing about
// whether the user's "5G" was 5×10^9 would be picking a fight nobody wants,
// and rounding up is the safe direction for a "grow to at least" target.
// An empty string means "no target" and is not an error.
//
// app/stocksim.go carries a mirror of this so a bad value is rejected on the
// canvas rather than inside a container that has already been deployed.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	up := strings.ToUpper(s)
	up = strings.TrimSuffix(up, "B") // "GB", "GiB" and "G" all mean the same here
	unit := int64(1)
	// Two-letter suffixes first, so "Ki" is never read as "K" with a stray "i".
	for _, u := range []struct {
		suffix string
		mult   int64
	}{
		{"KI", 1 << 10}, {"MI", 1 << 20}, {"GI", 1 << 30}, {"TI", 1 << 40},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
	} {
		if rest, ok := strings.CutSuffix(up, u.suffix); ok {
			unit, up = u.mult, rest
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(up), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size — write it like 5G, 512Mi or a plain byte count", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return int64(n * float64(unit)), nil
}

// FormatBytes renders a size the way the dashboard shows it.
func FormatBytes(n int64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.2f TiB", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func compactCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}
