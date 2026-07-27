package sim

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"hotelsim/internal/store"
)

// tickLoop runs fn every interval until ctx is done — identical shape to Traffic
// Sim's tickLoop.
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

// opsThisTick converts the profile's ops/sec target into a per-tick batch size,
// so the SAME tick interval produces Low/Medium/High throughput — the direct
// analog of Traffic Sim's TrafficLevel.targetVehicles().
func (e *Engine) opsThisTick(share float64, interval time.Duration) int {
	target := e.Profile.OpsPerSecond[e.Level()] * share
	n := int(target * interval.Seconds())
	if n < 1 && target > 0 {
		n = 1
	}
	return n
}

func newAgentRand() *rand.Rand { return rand.New(rand.NewSource(time.Now().UnixNano())) }

// ------------------------------------------------------------- guest search

// runGuestSearchAgent spawns and advances GuestSessions toward
// Profile.Sessions[level]. A browsing session issues a plain hotels.Find (an
// unsharded reference-collection read — a primary-shard-only read on a sharded
// cluster, worth contrasting against the availability aggregation below) and
// then an availability aggregation against dailyInventory: normally
// shard-key-prefixed (hotelId $in — targeted), but at Profile.ScatterRatio a
// region-only filter instead (deliberately not shard-key-prefixed — a
// broadcast, on purpose, for the query-education panel).
func (e *Engine) runGuestSearchAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "guest-search", "search", "idle", events, errs)
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
		e.Store.Heartbeat(ctx, "guest-search", "search", "ok", events, errs)
	})
}

func (e *Engine) spawnSession(rng *rand.Rand) {
	region := AllRegions[rng.Intn(len(AllRegions))]
	guestID, guestName := e.World.GuestName(rng)
	checkIn := e.pickCheckInDate(rng)
	s := &GuestSession{
		ID: fmt.Sprintf("S%d", rng.Int63()), GuestID: guestID, GuestName: guestName,
		Stage: "searching", Region: region,
		RoomTypeCode: pickRoomType(rng),
		CheckIn:      checkIn, Nights: pickNights(rng),
		Adults: 1 + rng.Intn(2), Children: rng.Intn(2),
		RequestID: fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), rng.Int63()),
		StartedAt: time.Now(), LastActive: time.Now(),
	}
	e.addSession(s)
}

// pickCheckInDate weights near-term dates so the check-in agent has something to
// process within the first minute of a demo rather than after twelve.
func pickCheckInDate(clock *SimClock, rng *rand.Rand) time.Time {
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

func (e *Engine) pickCheckInDate(rng *rand.Rand) time.Time { return pickCheckInDate(e.Clock, rng) }

func pickNights(rng *rand.Rand) int {
	r := rng.Float64()
	switch {
	case r < 0.55:
		return 1
	case r < 0.85:
		return 2
	case r < 0.95:
		return 3
	default:
		return 4 + rng.Intn(4) // 4..7
	}
}

func pickRoomType(rng *rand.Rand) RoomTypeCode {
	codes := []RoomTypeCode{RoomStandard, RoomStandard, RoomStandard, RoomDeluxe, RoomDeluxe, RoomSuite, RoomAccessible}
	return codes[rng.Intn(len(codes))]
}

// advanceSearch runs the hotels browse + availability aggregation for one
// session and moves it to "selecting" (candidates found), keeps it "searching"
// (retry later — e.g. transient error), or "abandoned" (no availability after
// a few tries) — removed either way once resolved. §13's "both successful and
// unsuccessful searches" requirement.
func (e *Engine) advanceSearch(ctx context.Context, rng *rand.Rand, s *GuestSession) {
	scope := e.World.Scope(e.Profile.HotelScope)
	scatter := rng.Float64() < e.Profile.ScatterRatio

	// The hotels browse — always unsharded, always "targeted" in the sense that
	// there's only one shard's worth of this collection to ask.
	_, _ = e.Store.Coll(store.CollHotels).Find(ctx, bson.M{"region": string(s.Region)})

	candidateIDs := regionHotelIDs(scope, s.Region)
	if len(candidateIDs) == 0 {
		e.removeSession(s.ID)
		return
	}

	checkOut := s.CheckIn.AddDate(0, 0, s.Nights)
	var filter bson.D
	targeted := !scatter
	if scatter {
		filter = bson.D{
			{Key: "region", Value: string(s.Region)},
			{Key: "roomTypeId", Value: string(s.RoomTypeCode)},
			{Key: "date", Value: bson.D{{Key: "$gte", Value: s.CheckIn}, {Key: "$lt", Value: checkOut}}},
			{Key: "availableRooms", Value: bson.D{{Key: "$gte", Value: 1}}},
			{Key: "closed", Value: false},
		}
	} else {
		filter = bson.D{
			{Key: "hotelId", Value: bson.D{{Key: "$in", Value: candidateIDs}}},
			{Key: "roomTypeId", Value: string(s.RoomTypeCode)},
			{Key: "date", Value: bson.D{{Key: "$gte", Value: s.CheckIn}, {Key: "$lt", Value: checkOut}}},
			{Key: "availableRooms", Value: bson.D{{Key: "$gte", Value: 1}}},
			{Key: "closed", Value: false},
		}
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: filter}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$hotelId"},
			{Key: "nights", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "minAvail", Value: bson.D{{Key: "$min", Value: "$availableRooms"}}},
			{Key: "avgRate", Value: bson.D{{Key: "$avg", Value: "$rate"}}},
		}}},
		{{Key: "$match", Value: bson.D{{Key: "nights", Value: s.Nights}}}},
		{{Key: "$sort", Value: bson.D{{Key: "avgRate", Value: 1}}}},
		{{Key: "$limit", Value: 10}},
	}
	start := time.Now()
	cur, err := e.Store.CollWithReadPref(store.CollDailyInventory, e.Profile.AnalyticsRead).Aggregate(ctx, pipeline)
	if err != nil {
		e.removeSession(s.ID)
		return
	}
	var results []bson.M
	_ = cur.All(ctx, &results)
	cur.Close(ctx)

	e.recordSearchSample(ctx, rng, s, targeted, msSince(start), pipeline)

	if len(results) == 0 {
		s.Stage = "abandoned"
		e.removeSession(s.ID)
		return
	}
	var candidates []string
	for _, r := range results {
		if id, ok := r["_id"].(string); ok {
			candidates = append(candidates, id)
		}
	}
	s.Candidates = candidates
	s.ChosenHotel = candidates[rng.Intn(len(candidates))]
	s.Stage = "selecting"
	s.LastActive = time.Now()
}

func regionHotelIDs(scope []*Hotel, r Region) []string {
	var out []string
	for _, h := range scope {
		if h.Region == r {
			out = append(out, h.ID)
		}
	}
	return out
}

func (e *Engine) recordSearchSample(ctx context.Context, rng *rand.Rand, s *GuestSession, targeted bool, ms float64, pipeline mongo.Pipeline) {
	filterSummary := fmt.Sprintf("region=%s roomType=%s", s.Region, s.RoomTypeCode)
	if targeted {
		filterSummary = fmt.Sprintf("hotelId IN region-scoped candidates, roomType=%s", s.RoomTypeCode)
	}
	sample := store.QuerySample{
		Agent: "guest-search", Collection: store.CollDailyInventory, Op: "aggregate",
		FilterSummary: filterSummary,
		Targeted:      targeted, DurationMs: ms, Reason: classifyFilter(targeted),
	}
	if rng.Float64() < e.Profile.ExplainRate {
		if shards, examined, returned, ok := e.verifyAggregateSample(ctx, store.CollDailyInventory, pipeline); ok {
			sample.Verified = true
			sample.ShardsTouched = shards
			sample.DocsExamined = examined
			sample.NReturned = returned
		}
	}
	e.Store.RecordQuerySample(ctx, sample)
}

// ------------------------------------------------------------------ booking

// runReservationAgent advances "selecting" sessions through the booking write
// path (Booker.Reserve). At Profile.DuplicateRate it deliberately resubmits a
// just-used requestId instead of minting a new one, to exercise the idempotency
// mechanism end to end (spec §22).
func (e *Engine) runReservationAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	var lastRequestID string
	tickLoop(ctx, 500*time.Millisecond, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "reservation", "booking", "idle", events, errs)
			return
		}
		batch := e.opsThisTick(0.15, 500*time.Millisecond)
		sessions := e.sessionsByStage("selecting")
		if len(sessions) > batch {
			sessions = sessions[:batch]
		}
		for _, s := range sessions {
			hotel := e.World.ByID[s.ChosenHotel]
			if hotel == nil {
				e.removeSession(s.ID)
				continue
			}
			reqID := s.RequestID
			if lastRequestID != "" && rng.Float64() < e.Profile.DuplicateRate {
				reqID = lastRequestID // intentional duplicate submission
			}
			out, err := e.booker.Reserve(ctx, BookRequest{
				RequestID: reqID, HotelID: hotel.ID, HotelName: hotel.Name, Region: hotel.Region,
				RoomTypeCode: s.RoomTypeCode, CheckIn: s.CheckIn, Nights: s.Nights,
				Adults: s.Adults, Children: s.Children, GuestID: s.GuestID, GuestName: s.GuestName,
			})
			if err != nil {
				errs++
				e.removeSession(s.ID)
				continue
			}
			events++
			lastRequestID = s.RequestID
			if out.Result == "booked" && out.Reservation != nil {
				e.rememberReservation(ResRef{ID: out.Reservation.ID, HotelID: out.Reservation.HotelID, CheckInDate: out.Reservation.CheckInDate})
			}
			e.removeSession(s.ID)
		}
		e.Store.Heartbeat(ctx, "reservation", "booking", "ok", events, errs)
	})
}
