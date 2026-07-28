package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"
)

// pickCandidate returns a reservation ref to act on: 60% of the time from the
// in-memory recent-reservation ring (a targeted follow-up — the common case, a
// passenger acting on their own just-made booking), 40% of the time from a plain SQL
// query (a "find something to act on" scan, the less common but still realistic
// case — e.g. a modification/cancellation agent operating on someone else's
// booking).
func (e *Engine) pickCandidate(ctx context.Context, rng *rand.Rand, whereSQL string, args ...any) (ResRef, bool) {
	if rng.Float64() < 0.6 {
		if ref, ok := e.pickRecentReservation(rng); ok {
			return ref, true
		}
	}
	rows, err := e.Store.DB.QueryContext(ctx, "SELECT id, route_id, flight_date FROM reservations WHERE "+whereSQL+" ORDER BY id DESC LIMIT 20", args...)
	if err != nil {
		return ResRef{}, false
	}
	defer rows.Close()
	var candidates []ResRef
	for rows.Next() {
		var ref ResRef
		if rows.Scan(&ref.ID, &ref.RouteID, &ref.FlightDate) == nil {
			candidates = append(candidates, ref)
		}
	}
	if len(candidates) == 0 {
		return ResRef{}, false
	}
	return candidates[rng.Intn(len(candidates))], true
}

// ------------------------------------------------------------- modification

// runModificationAgent picks a confirmed future reservation and applies one of two
// flavors of change: a passenger-count edit (guarded seat-availability update, same
// mechanism as booking) or a full rebook to a different route/class/date (the case
// that genuinely needs a transaction touching two flight_inventory rows atomically).
// Hotel Sim also has a cheap single-night-extend flavor with no airline analog
// (a booking here is always a single flight_date, not a date range), so this is
// intentionally a two-way split instead of three.
func (e *Engine) runModificationAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, 3*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "modification", "idle", detailStr(events, errs))
			return
		}
		if rng.Float64() > e.Profile.ModifyRate*4 { // ModifyRate is "per simulated day"; scale to this tick
			e.Store.Heartbeat(ctx, "modification", "ok", detailStr(events, errs))
			return
		}
		today := e.Clock.Today()
		ref, ok := e.pickCandidate(ctx, rng, "status=? AND flight_date>?", string(StatusConfirmed), today.Format("2006-01-02"))
		if !ok {
			e.Store.Heartbeat(ctx, "modification", "ok", detailStr(events, errs))
			return
		}
		if rng.Intn(2) == 0 {
			delta := 1
			if rng.Intn(2) == 0 {
				delta = -1
			}
			if _, err := e.booker.ModifySeats(ctx, ref.ID, delta, "modification-agent"); err != nil {
				errs++
			}
		} else {
			scope := e.World.Scope(e.Profile.RouteScope)
			newRoute := e.World.PickHot(rng, scope)
			if newRoute == nil {
				e.Store.Heartbeat(ctx, "modification", "ok", detailStr(events, errs))
				return
			}
			newDate := ref.FlightDate.AddDate(0, 0, 1+rng.Intn(3))
			if _, err := e.booker.Rebook(ctx, ref.ID, newRoute, pickClassCode(rng), newDate, "modification-agent"); err != nil {
				errs++
			}
		}
		events++
		e.Store.Heartbeat(ctx, "modification", "ok", detailStr(events, errs))
	})
}

// ------------------------------------------------------------- cancellation

func (e *Engine) runCancellationAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, 4*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "cancellation", "idle", detailStr(events, errs))
			return
		}
		if rng.Float64() > e.Profile.CancelRate*4 {
			e.Store.Heartbeat(ctx, "cancellation", "ok", detailStr(events, errs))
			return
		}
		today := e.Clock.Today()
		ref, ok := e.pickCandidate(ctx, rng, "status=? AND flight_date>?", string(StatusConfirmed), today.Format("2006-01-02"))
		if !ok {
			e.Store.Heartbeat(ctx, "cancellation", "ok", detailStr(events, errs))
			return
		}
		if err := e.booker.Cancel(ctx, ref.ID, "cancellation-agent"); err != nil {
			errs++
		} else {
			events++
		}
		e.Store.Heartbeat(ctx, "cancellation", "ok", detailStr(events, errs))
	})
}

// ------------------------------------------------------------------ check-in

// runCheckInAgent processes reservations whose (simulated) flight date has arrived.
// The current status living in the guarded UPDATE's WHERE clause IS the state-
// machine guard and the concurrency check in one — two concurrent check-in attempts
// on the same reservation can't both win, no transaction required.
func (e *Engine) runCheckInAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, 2*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "check-in", "idle", detailStr(events, errs))
			return
		}
		today := e.Clock.Today()
		limit := e.opsThisTick(0.05, 2*time.Second)
		if limit < 1 {
			limit = 5
		}
		rows, err := e.Store.DB.QueryContext(ctx, "SELECT id, route_id, flight_date FROM reservations WHERE status=? AND flight_date=? LIMIT ?",
			string(StatusConfirmed), today.Format("2006-01-02"), limit)
		if err != nil {
			errs++
		} else {
			var due []ResRef
			for rows.Next() {
				var ref ResRef
				if rows.Scan(&ref.ID, &ref.RouteID, &ref.FlightDate) == nil {
					due = append(due, ref)
				}
			}
			rows.Close()
			for _, r := range due {
				now := time.Now().UTC()
				hist := []HistoryEntry{{At: now, SimAt: e.Clock.Now(), Action: "checked_in", By: "check-in-agent", From: string(StatusConfirmed), To: string(StatusCheckedIn)}}
				histJSON, _ := json.Marshal(hist)
				res, uerr := e.Store.DB.ExecContext(ctx,
					"UPDATE reservations SET status=?, actual_check_in=?, seat_assignment=?, version=version+1, history=JSON_MERGE_PRESERVE(history, ?), updated_at=? WHERE id=? AND status=?",
					string(StatusCheckedIn), now, randomSeatAssignment(rng), string(histJSON), now, r.ID, string(StatusConfirmed))
				if uerr == nil {
					if n, _ := res.RowsAffected(); n > 0 {
						events++
						e.counters.checkInsTotal.Add(1)
						e.PublishEvent(ctx, "checked_in", r.ID, r.RouteID, r.FlightDate, "check-in-agent", "")
					}
				}
			}
		}

		// No-shows: still confirmed a full simulated day past flight date.
		noShowCutoff := today.AddDate(0, 0, -1)
		now := time.Now().UTC()
		hist := []HistoryEntry{{At: now, SimAt: e.Clock.Now(), Action: "no_show", By: "check-in-agent"}}
		histJSON, _ := json.Marshal(hist)
		nsRes, _ := e.Store.DB.ExecContext(ctx,
			"UPDATE reservations SET status=?, history=JSON_MERGE_PRESERVE(history, ?), updated_at=? WHERE status=? AND flight_date<=?",
			string(StatusNoShow), string(histJSON), now, string(StatusConfirmed), noShowCutoff.Format("2006-01-02"))
		if nsRes != nil {
			if n, _ := nsRes.RowsAffected(); n > 0 {
				e.counters.noShowsTotal.Add(n)
			}
		}
		e.Store.Heartbeat(ctx, "check-in", "ok", detailStr(events, errs))
	})
}

func randomSeatAssignment(rng *rand.Rand) string {
	row := 1 + rng.Intn(38)
	letter := "ABCDEF"[rng.Intn(6)]
	return fmt.Sprintf("%d%c", row, letter)
}

// ------------------------------------------------------------ flight completion

// runFlightCompletionAgent completes checked-in reservations once their flight date
// has passed — the check-out analog. 10% skip per tick (simulating a flight still
// in the air / boarding running long), same as Hotel Sim's late-checkout skip.
func (e *Engine) runFlightCompletionAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, 2*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "flight-completion", "idle", detailStr(events, errs))
			return
		}
		today := e.Clock.Today()
		limit := e.opsThisTick(0.05, 2*time.Second)
		if limit < 1 {
			limit = 5
		}
		rows, err := e.Store.DB.QueryContext(ctx, "SELECT id, route_id, flight_date FROM reservations WHERE status=? AND flight_date<=? LIMIT ?",
			string(StatusCheckedIn), today.Format("2006-01-02"), limit)
		if err != nil {
			errs++
			e.Store.Heartbeat(ctx, "flight-completion", "error", detailStr(events, errs))
			return
		}
		var due []ResRef
		for rows.Next() {
			var ref ResRef
			if rows.Scan(&ref.ID, &ref.RouteID, &ref.FlightDate) == nil {
				due = append(due, ref)
			}
		}
		rows.Close()
		for _, r := range due {
			if rng.Float64() < 0.10 {
				continue // 10% still boarding — skip this tick, catch it next time
			}
			now := time.Now().UTC()
			hist := []HistoryEntry{{At: now, SimAt: e.Clock.Now(), Action: "completed", By: "flight-completion-agent", From: string(StatusCheckedIn), To: string(StatusCompleted)}}
			histJSON, _ := json.Marshal(hist)
			res, uerr := e.Store.DB.ExecContext(ctx,
				"UPDATE reservations SET status=?, actual_boarding=?, version=version+1, history=JSON_MERGE_PRESERVE(history, ?), updated_at=? WHERE id=? AND status=?",
				string(StatusCompleted), now, string(histJSON), now, r.ID, string(StatusCheckedIn))
			if uerr == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					events++
					e.counters.completionsTotal.Add(1)
					e.PublishEvent(ctx, "completed", r.ID, r.RouteID, r.FlightDate, "flight-completion-agent", "")
				}
			}
		}
		e.Store.Heartbeat(ctx, "flight-completion", "ok", detailStr(events, errs))
	})
}
