package sim

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"carsim/internal/store"
)

// tickLoop runs fn every interval until ctx is done.
func tickLoop(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// opsThisTick converts the profile's ops/sec target into a per-tick batch size, so
// the same tick interval produces Low/Medium/High throughput.
func (e *Engine) opsThisTick(share float64, interval time.Duration) int {
	target := e.Profile.OpsPerSecond[e.Level()] * share
	n := int(target * interval.Seconds())
	if n < 1 && target > 0 {
		n = 1
	}
	return n
}

func newAgentRand() *rand.Rand { return rand.New(rand.NewSource(time.Now().UnixNano())) }

// ------------------------------------------------------------- location search

// runLocationSearchAgent spawns and advances RenterSessions toward
// Profile.Sessions[level]. A browsing session issues a targeted rental_inventory
// lookup (location_id + class_code + date range — hits idx_ri_location_date, a
// short index range scan) or, at Profile.ScatterRatio, a deliberately
// non-composite-key "browse by region" scan instead (hits idx_ri_region_date,
// but across every location in the region — a much wider scan, on purpose, for
// the query-education panel).
func (e *Engine) runLocationSearchAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "location-search", "idle", detailStr(events, errs))
			return
		}
		target := e.Profile.Sessions[e.Level()]
		current := e.sessionCount()
		spawn := target - current
		if spawn > 20 {
			spawn = 20 // cap per-tick spawn burst so a level jump ramps rather than slamming
		}
		for i := 0; i < spawn; i++ {
			e.spawnSession(rng)
		}
		for _, s := range e.sessionsByStage("searching") {
			e.advanceSearch(ctx, rng, s)
			events++
			e.counters.searchesTotal.Add(1)
		}
		e.Store.Heartbeat(ctx, "location-search", "ok", detailStr(events, errs))
	})
}

func (e *Engine) spawnSession(rng *rand.Rand) *RenterSession {
	region := AllRegions[rng.Intn(len(AllRegions))]
	renterID, renterName := e.World.RenterName(rng)
	pickupDate := pickPickupDate(e.Clock, rng)
	nights := 1 + rng.Intn(7)
	s := &RenterSession{
		ID: fmt.Sprintf("S%d", rng.Int63()), RenterID: renterID, RenterName: renterName,
		Stage: "searching", Region: region,
		ClassCode:  pickClassCode(rng),
		PickupDate: pickupDate, ReturnDate: pickupDate.AddDate(0, 0, nights),
		RequestID: fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), rng.Int63()),
		StartedAt: time.Now(), LastActive: time.Now(),
	}
	e.addSession(s)
	return s
}

// pickPickupDate weights near-term dates so the check-out agent has something to
// process within the first minute of a demo rather than after twelve.
func pickPickupDate(clock *SimClock, rng *rand.Rand) time.Time {
	today := clock.Today()
	r := rng.Float64()
	var daysOut int
	switch {
	case r < 0.30:
		daysOut = rng.Intn(2) // today/tomorrow
	case r < 0.70:
		daysOut = 2 + rng.Intn(4) // 2..5
	default:
		daysOut = 6 + rng.Intn(9) // 6..14
	}
	return today.AddDate(0, 0, daysOut)
}

func pickClassCode(rng *rand.Rand) VehicleClassCode {
	codes := []VehicleClassCode{ClassEconomy, ClassEconomy, ClassEconomy, ClassCompact, ClassCompact, ClassSUV, ClassLuxury}
	return codes[rng.Intn(len(codes))]
}

// advanceSearch runs the availability lookup for one session and moves it to
// "selecting" (candidates found), or removes it as abandoned (no availability).
func (e *Engine) advanceSearch(ctx context.Context, rng *rand.Rand, s *RenterSession) {
	scope := e.World.Scope(e.Profile.LocationScope)
	scatter := rng.Float64() < e.Profile.ScatterRatio

	candidateIDs := regionLocationIDs(scope, s.Region)
	if len(candidateIDs) == 0 {
		e.removeSession(s.ID)
		return
	}

	var query string
	var args []any
	targeted := !scatter
	pickup, ret := s.PickupDate.Format("2006-01-02"), s.ReturnDate.Format("2006-01-02")
	if scatter {
		query = "SELECT location_id, MIN(available_vehicles) FROM rental_inventory WHERE region=$1 AND class_code=$2 AND date>=$3 AND date<$4 AND available_vehicles>=1 AND closed=false GROUP BY location_id ORDER BY location_id LIMIT 10"
		args = []any{string(s.Region), string(s.ClassCode), pickup, ret}
	} else {
		placeholders, idArgs := inClause(candidateIDs, 3)
		query = "SELECT location_id, MIN(available_vehicles) FROM rental_inventory WHERE location_id IN (" + placeholders + ") AND class_code=$1 AND date>=$2 AND date<" + fmt.Sprintf("$%d", len(idArgs)+3) + " AND available_vehicles>=1 AND closed=false GROUP BY location_id ORDER BY location_id LIMIT 10"
		args = append([]any{string(s.ClassCode), pickup}, append(idArgs, ret)...)
	}

	start := time.Now()
	rows, err := e.Store.DB.QueryContext(ctx, query, args...)
	if err != nil {
		e.removeSession(s.ID)
		return
	}
	var candidates []string
	for rows.Next() {
		var id string
		var minAvail int
		if rows.Scan(&id, &minAvail) == nil {
			candidates = append(candidates, id)
		}
	}
	rows.Close()

	e.recordSearchSample(ctx, rng, targeted, msSince(start), query, args)

	if len(candidates) == 0 {
		s.Stage = "abandoned"
		e.removeSession(s.ID)
		return
	}
	s.ChosenLoc = candidates[rng.Intn(len(candidates))]
	s.DropoffLoc = s.ChosenLoc
	if rng.Float64() < 0.12 { // ~12% one-way rentals
		if alt := e.World.PickHot(rng, scope); alt != nil {
			s.DropoffLoc = alt.ID
		}
	}
	s.Stage = "selecting"
	s.LastActive = time.Now()
}

func regionLocationIDs(scope []*Location, r Region) []string {
	var out []string
	for _, l := range scope {
		if l.Region == r {
			out = append(out, l.ID)
		}
	}
	return out
}

// inClause builds a Postgres "$N,$N+1,..." placeholder list starting at
// startAt, for a dynamic-length IN(...) clause — the $N-numbered analog of
// MySQL's positional "?,?,..." version.
func inClause(ids []string, startAt int) (string, []any) {
	placeholders := ""
	args := make([]any, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders += ","
		}
		placeholders += fmt.Sprintf("$%d", startAt+i)
		args[i] = id
	}
	return placeholders, args
}

func msSince(start time.Time) float64 { return float64(time.Since(start).Microseconds()) / 1000.0 }

func detailStr(events, errs int64) string { return fmt.Sprintf("events=%d errs=%d", events, errs) }

func (e *Engine) recordSearchSample(ctx context.Context, rng *rand.Rand, targeted bool, ms float64, query string, args []any) {
	kind := "scatter"
	if targeted {
		kind = "targeted"
	}
	if rng.Float64() < e.Profile.ExplainRate {
		e.verifyExplain(ctx, query, args, kind, ms)
		return
	}
	e.Store.RecordQuerySample(ctx, store.QuerySample{Kind: kind, SQLText: query, DurationMs: ms})
}

// ------------------------------------------------------------------ booking

// runBookingAgent advances "selecting" sessions through the booking write path
// (sqlBooker.Reserve). At Profile.DuplicateRate it deliberately resubmits a
// just-used requestId instead of minting a new one, to exercise the idempotency
// mechanism end to end.
func (e *Engine) runBookingAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	var lastRequestID string
	tickLoop(ctx, 500*time.Millisecond, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "booking", "idle", detailStr(events, errs))
			return
		}
		batch := e.opsThisTick(0.15, 500*time.Millisecond)
		sessions := e.sessionsByStage("selecting")
		if len(sessions) > batch {
			sessions = sessions[:batch]
		}
		for _, s := range sessions {
			loc := e.World.ByID[s.ChosenLoc]
			if loc == nil {
				e.removeSession(s.ID)
				continue
			}
			if !e.confirmAvailability(ctx, rng, loc, s) {
				e.removeSession(s.ID) // sold out on this exact location/date range since it was picked — don't bother booking
				continue
			}
			reqID := s.RequestID
			if lastRequestID != "" && rng.Float64() < e.Profile.DuplicateRate {
				reqID = lastRequestID // intentional duplicate submission
			}
			if _, _, err := e.booker.Reserve(ctx, loc, s.ClassCode, s.PickupDate, s.ReturnDate, s.DropoffLoc, s.RenterID, s.RenterName, reqID, "booking-agent"); err != nil {
				errs++
				e.removeSession(s.ID)
				continue
			}
			events++
			lastRequestID = s.RequestID
			e.removeSession(s.ID)
		}
		e.Store.Heartbeat(ctx, "booking", "ok", detailStr(events, errs))
	})
}

// confirmAvailability runs a plain, targeted SELECT against the exact
// rental_inventory row for the renter's first night (a primary-key-shaped
// lookup: location+class+date) a renter is about to book — distinct from
// location-search's broader browse query (which only ever checks "does *some*
// day in this class/range have room" across candidate locations), this is the
// "double-check this specific location still has room" read every real booking
// flow does right before committing. Recorded as its own targeted query sample
// so the query-education panel shows both query shapes. sqlBooker.Reserve's own
// guarded multi-row UPDATE is still the actual concurrency control — this
// SELECT can go stale between here and the write, same as any real
// "check then book" UI; that's fine, it's an early-exit optimization (skip
// attempting a booking that's almost certainly sold out already), not the
// correctness mechanism.
func (e *Engine) confirmAvailability(ctx context.Context, rng *rand.Rand, loc *Location, s *RenterSession) bool {
	invID := InventoryID(loc.ID, s.ClassCode, s.PickupDate)
	const query = "SELECT available_vehicles FROM rental_inventory WHERE id=$1"
	start := time.Now()
	var available int
	err := e.Store.DB.QueryRowContext(ctx, query, invID).Scan(&available)
	ms := msSince(start)
	if rng.Float64() < e.Profile.ExplainRate {
		e.verifyExplain(ctx, query, []any{invID}, "targeted", ms)
	} else {
		e.Store.RecordQuerySample(ctx, store.QuerySample{Kind: "targeted", SQLText: query, DurationMs: ms})
	}
	if err != nil {
		return false
	}
	return available >= 1
}
