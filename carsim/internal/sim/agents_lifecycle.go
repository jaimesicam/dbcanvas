package sim

import (
	"context"
	"math/rand"
	"time"
)

// pickCandidate returns a reservation ref to act on: 60% of the time from the
// in-memory recent-reservation ring (a targeted follow-up — the common case, a
// renter acting on their own just-made booking), 40% of the time from a plain
// SQL query (a "find something to act on" scan, the less common but still
// realistic case — e.g. a modification/cancellation agent operating on someone
// else's booking).
func (e *Engine) pickCandidate(ctx context.Context, rng *rand.Rand, whereSQL string, args ...any) (ResRef, bool) {
	if rng.Float64() < 0.6 {
		if ref, ok := e.pickRecentReservation(rng); ok {
			return ref, true
		}
	}
	rows, err := e.Store.DB.QueryContext(ctx, "SELECT id, pickup_location_id, pickup_date, return_date FROM reservations WHERE "+whereSQL+" ORDER BY id DESC LIMIT 20", args...)
	if err != nil {
		return ResRef{}, false
	}
	defer rows.Close()
	var candidates []ResRef
	for rows.Next() {
		var ref ResRef
		if rows.Scan(&ref.ID, &ref.LocationID, &ref.PickupDate, &ref.ReturnDate) == nil {
			candidates = append(candidates, ref)
		}
	}
	if len(candidates) == 0 {
		return ResRef{}, false
	}
	return candidates[rng.Intn(len(candidates))], true
}

// ------------------------------------------------------------- modification

// runModificationAgent picks a confirmed future reservation and applies one of
// three flavors of change: extend/shorten the return date (guarded
// availability update, same mechanism as booking), change the drop-off
// location (no inventory impact), or change vehicle class entirely (cancel +
// rebook as a new reservation, since a class swap changes which
// rental_inventory rows are even relevant).
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
		ref, ok := e.pickCandidate(ctx, rng, "status=$1 AND pickup_date>$2", string(StatusConfirmed), today.Format("2006-01-02"))
		if !ok {
			e.Store.Heartbeat(ctx, "modification", "ok", detailStr(events, errs))
			return
		}
		switch rng.Intn(3) {
		case 0:
			delta := 1
			if rng.Intn(2) == 0 {
				delta = -1
			}
			if _, err := e.booker.ModifyDates(ctx, ref.ID, delta, "modification-agent"); err != nil {
				errs++
			} else {
				events++
			}
		case 1:
			scope := e.World.Scope(e.Profile.LocationScope)
			alt := e.World.PickHot(rng, scope)
			if alt == nil {
				break
			}
			if applied, err := e.booker.ModifyDropoff(ctx, ref.ID, alt.ID, "modification-agent"); err != nil {
				errs++
			} else if applied {
				events++
			}
		default:
			// Class swap: release the old booking and rebook as a new reservation —
			// simpler and just as correct as a cross-class transaction, since the old
			// and new bookings touch entirely different rental_inventory rows anyway.
			loc := e.World.ByID[ref.LocationID]
			if loc == nil {
				break
			}
			if err := e.booker.Cancel(ctx, ref.ID, "modification-agent"); err != nil {
				errs++
				break
			}
			renterID, renterName := e.World.RenterName(rng)
			reqID := "modify-" + ref.ID
			if _, _, err := e.booker.Reserve(ctx, loc, pickClassCode(rng), ref.PickupDate, ref.ReturnDate, loc.ID, renterID, renterName, reqID, "modification-agent"); err != nil {
				errs++
			} else {
				events++
			}
		}
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
		ref, ok := e.pickCandidate(ctx, rng, "status=$1 AND pickup_date>$2", string(StatusConfirmed), today.Format("2006-01-02"))
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

// ------------------------------------------------------------------ check-out

// runCheckOutAgent processes confirmed reservations whose (simulated) pickup
// date has arrived — claiming a specific vehicle via sqlBooker.CheckOut's `FOR
// UPDATE SKIP LOCKED`.
func (e *Engine) runCheckOutAgent(ctx context.Context) {
	var events, errs int64
	tickLoop(ctx, 2*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "check-out", "idle", detailStr(events, errs))
			return
		}
		today := e.Clock.Today()
		limit := e.opsThisTick(0.05, 2*time.Second)
		if limit < 1 {
			limit = 5
		}
		rows, err := e.Store.DB.QueryContext(ctx, "SELECT id FROM reservations WHERE status=$1 AND pickup_date=$2 LIMIT $3",
			string(StatusConfirmed), today.Format("2006-01-02"), limit)
		if err != nil {
			errs++
			e.Store.Heartbeat(ctx, "check-out", "error", detailStr(events, errs))
			return
		}
		var due []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				due = append(due, id)
			}
		}
		rows.Close()
		for _, id := range due {
			ok, err := e.booker.CheckOut(ctx, id, "check-out-agent")
			if err != nil {
				errs++
			} else if ok {
				events++
			}
		}
		e.Store.Heartbeat(ctx, "check-out", "ok", detailStr(events, errs))
	})
}

// ------------------------------------------------------------------- check-in

// runCheckInAgent returns checked-out reservations once their (simulated)
// return date has arrived — 10% skip per tick (simulating a renter still on the
// road / running late), same as Airline Sim's boarding-still-in-progress skip.
// Also flips still-confirmed reservations a full simulated day past pickup date
// to no_show — the renter never showed up to claim a vehicle at all.
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
		rows, err := e.Store.DB.QueryContext(ctx, "SELECT id FROM reservations WHERE status=$1 AND return_date<=$2 LIMIT $3",
			string(StatusCheckedOut), today.Format("2006-01-02"), limit)
		if err != nil {
			errs++
			e.Store.Heartbeat(ctx, "check-in", "error", detailStr(events, errs))
			return
		}
		var due []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				due = append(due, id)
			}
		}
		rows.Close()
		for _, id := range due {
			if rng.Float64() < 0.10 {
				continue // 10% still on the road — skip this tick, catch it next time
			}
			ok, err := e.booker.CheckIn(ctx, id, "check-in-agent")
			if err != nil {
				errs++
			} else if ok {
				events++
			}
		}

		// No-shows: still confirmed a full simulated day past pickup date. Unlike
		// Airline Sim (a no-show's seat genuinely can't be resold — the flight
		// already departed), a no-show car sat unused at its location the whole
		// time and IS still available for someone else — so each no-show must
		// also release its held date range back to rental_inventory, the same
		// releaseRange step Cancel uses. That per-row release is why this is a
		// bounded loop rather than Airline's single bulk UPDATE.
		e.sweepNoShows(ctx, today.AddDate(0, 0, -1), limit)
		e.Store.Heartbeat(ctx, "check-in", "ok", detailStr(events, errs))
	})
}
