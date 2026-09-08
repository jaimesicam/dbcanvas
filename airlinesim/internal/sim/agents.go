package sim

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"airlinesim/internal/store"
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

// ------------------------------------------------------------- route search

// runRouteSearchAgent spawns and advances PassengerSessions toward
// Profile.Sessions[level]. A browsing session issues a targeted flight_inventory
// lookup (route_id + class_code + date range — hits idx_fi_route_date, a short
// index range scan) or, at Profile.ScatterRatio, a deliberately non-composite-key
// "browse by region" scan instead (hits idx_fi_region_date, but across every route
// in the region — a much wider scan, on purpose, for the query-education panel).
func (e *Engine) runRouteSearchAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "route-search", "idle", detailStr(events, errs))
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
		e.Store.Heartbeat(ctx, "route-search", "ok", detailStr(events, errs))
	})
}

func (e *Engine) spawnSession(rng *rand.Rand) {
	region := AllRegions[rng.Intn(len(AllRegions))]
	passengerID, passengerName := e.World.PassengerName(rng)
	s := &PassengerSession{
		ID: fmt.Sprintf("S%d", rng.Int63()), PassengerID: passengerID, PassengerName: passengerName,
		Stage: "searching", Region: region,
		ClassCode:  pickClassCode(rng),
		FlightDate: pickFlightDate(e.Clock, rng),
		Seats:      pickSeats(rng),
		RequestID:  fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), rng.Int63()),
		StartedAt:  time.Now(), LastActive: time.Now(),
	}
	e.addSession(s)
}

// pickFlightDate weights near-term dates so the check-in agent has something to
// process within the first minute of a demo rather than after twelve.
func pickFlightDate(clock *SimClock, rng *rand.Rand) time.Time {
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

func pickSeats(rng *rand.Rand) int {
	r := rng.Float64()
	switch {
	case r < 0.55:
		return 1
	case r < 0.85:
		return 2
	case r < 0.95:
		return 3
	default:
		return 4 + rng.Intn(3) // 4..6
	}
}

func pickClassCode(rng *rand.Rand) SeatClassCode {
	codes := []SeatClassCode{ClassEconomy, ClassEconomy, ClassEconomy, ClassEconomy, ClassPremium, ClassBusiness, ClassFirst}
	return codes[rng.Intn(len(codes))]
}

// advanceSearch runs the availability lookup for one session and moves it to
// "selecting" (candidates found), or removes it as abandoned (no availability).
func (e *Engine) advanceSearch(ctx context.Context, rng *rand.Rand, s *PassengerSession) {
	scope := e.World.Scope(e.Profile.RouteScope)
	scatter := rng.Float64() < e.Profile.ScatterRatio

	candidateIDs := regionRouteIDs(scope, s.Region)
	if len(candidateIDs) == 0 {
		e.removeSession(s.ID)
		return
	}

	var query string
	var args []any
	targeted := !scatter
	if scatter {
		query = "SELECT route_id, MIN(available_seats) FROM flight_inventory WHERE region=? AND class_code=? AND flight_date=? AND available_seats>=1 AND closed=0 GROUP BY route_id ORDER BY route_id LIMIT 10"
		args = []any{string(s.Region), string(s.ClassCode), s.FlightDate.Format("2006-01-02")}
	} else {
		placeholders, idArgs := inClause(candidateIDs)
		query = "SELECT route_id, MIN(available_seats) FROM flight_inventory WHERE route_id IN (" + placeholders + ") AND class_code=? AND flight_date=? AND available_seats>=1 AND closed=0 GROUP BY route_id ORDER BY route_id LIMIT 10"
		args = append(idArgs, string(s.ClassCode), s.FlightDate.Format("2006-01-02"))
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
	s.Candidates = candidates
	s.ChosenRoute = candidates[rng.Intn(len(candidates))]
	s.Stage = "selecting"
	s.LastActive = time.Now()
}

func regionRouteIDs(scope []*Route, r Region) []string {
	var out []string
	for _, route := range scope {
		if route.Region == r {
			out = append(out, route.ID)
		}
	}
	return out
}

func inClause(ids []string) (string, []any) {
	placeholders := ""
	args := make([]any, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
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
			route := e.World.ByID[s.ChosenRoute]
			if route == nil {
				e.removeSession(s.ID)
				continue
			}
			if !e.confirmAvailability(ctx, rng, route, s) {
				e.removeSession(s.ID) // sold out on this exact flight since it was picked — don't bother booking
				continue
			}
			reqID := s.RequestID
			if lastRequestID != "" && rng.Float64() < e.Profile.DuplicateRate {
				reqID = lastRequestID // intentional duplicate submission
			}
			if _, _, err := e.booker.Reserve(ctx, route, s.ClassCode, s.FlightDate, s.Seats, s.PassengerID, s.PassengerName, reqID, "booking-agent"); err != nil {
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
// flight_inventory row (a primary-key lookup: route+class+date) a passenger
// is about to book — distinct from route-search's broader browse query
// (which only ever checks "does *some* flight in this class/date have room"
// across candidate routes), this is the "double-check this specific flight
// still has room" read every real booking flow does right before committing.
// Recorded as its own targeted query sample so the query-education panel
// shows both query shapes. sqlBooker.Reserve's own guarded UPDATE is still
// the actual concurrency control — this SELECT can go stale between here and
// the write, same as any real "check then book" UI; that's fine, it's an
// early-exit optimization (skip attempting a booking that's almost certainly
// sold out already), not the correctness mechanism.
func (e *Engine) confirmAvailability(ctx context.Context, rng *rand.Rand, route *Route, s *PassengerSession) bool {
	invID := InventoryID(route.ID, s.ClassCode, s.FlightDate)
	const query = "SELECT available_seats FROM flight_inventory WHERE id=?"
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
	return available >= s.Seats
}
