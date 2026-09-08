package sim

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"hotelsim/internal/store"
)

// findOptsLimitSortDesc limits to n (0 = driver default, effectively unbounded)
// and sorts by _id descending — a reasonable "most recent first" proxy for
// every collection this app queries this way (ObjectId and the confirmation-
// number sequence both trend upward over time).
func findOptsLimitSortDesc(n int64) *options.FindOptions {
	opts := options.Find().SetSort(bson.D{{Key: "_id", Value: -1}})
	if n > 0 {
		opts.SetLimit(n)
	}
	return opts
}

// Snapshot is the full GET /api/state payload — everything the overview grid,
// summary dashboard, and MongoDB topology panel need in one round trip.
// BuildSnapshot ALWAYS reads from MongoDB, never from Engine's in-memory maps
// (the counters are read back from the metrics document the analytics agent
// itself just wrote them into) — this is what makes a page refresh always
// recover full state, and what makes every connected browser see identical data.
type Snapshot struct {
	Hotels      []HotelTile   `json:"hotels"`
	Summary     bson.M        `json:"summary"`
	Mongo       bson.M        `json:"mongo"`
	Agents      []AgentStatus `json:"agents"`
	Control     ControlInfo   `json:"control"`
	LastEventID string        `json:"lastEventId"`
	UptimeSec   int64         `json:"uptimeSeconds"`
	MongoError  string        `json:"mongoError,omitempty"`
}

type HotelTile struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	Region             string  `json:"region"`
	Tier               string  `json:"tier"`
	Category           string  `json:"category"`
	TotalRooms         int     `json:"totalRooms"`
	OccupancyPct       float64 `json:"occupancyPct"`
	OccupancyClass     string  `json:"occupancyClass"`
	Badge              string  `json:"badge"`
	ActiveReservations int64   `json:"activeReservations"`
	ArrivalsToday      int64   `json:"arrivalsToday"`
	DeparturesToday    int64   `json:"departuresToday"`
	InScope            bool    `json:"inScope"`
	Shard              string  `json:"shard,omitempty"`
}

type AgentStatus struct {
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	Status       string    `json:"status"`
	LastActivity time.Time `json:"lastActivity"`
	Events       int64     `json:"events"`
	Errors       int64     `json:"errors"`
	Stale        bool      `json:"stale"`
}

type ControlInfo struct {
	State    string `json:"state"` // "running" | "paused"
	Level    string `json:"level"` // off|low|medium|high
	Topology string `json:"topology"`
}

// staleAfter is how old an agent's last heartbeat can be before the UI marks it
// "stale" — generously above every agent's own tick interval (the slowest is
// 15s) so a momentarily slow tick doesn't flicker the badge.
const staleAfter = 30 * time.Second

// BuildSnapshot assembles the full /api/state payload. If MongoDB is
// unreachable, it still returns a Snapshot (MongoError set, everything else
// empty) rather than an error — the web interface must be able to show "can't
// reach MongoDB" rather than going blank.
func (e *Engine) BuildSnapshot(ctx context.Context) Snapshot {
	snap := Snapshot{
		Control:   ControlInfo{State: runningState(e.Running()), Level: string(e.Level()), Topology: string(e.Topology)},
		UptimeSec: e.UptimeSeconds(),
	}
	if err := e.Store.Ping(ctx); err != nil {
		snap.MongoError = "cannot reach MongoDB: " + err.Error()
		return snap
	}

	snap.Hotels = e.buildHotelTiles(ctx)
	snap.Agents = e.buildAgentStatuses(ctx)

	var metricsCurrent bson.M
	e.Store.Coll(store.CollMetrics).FindOne(ctx, bson.M{"_id": "current"}).Decode(&metricsCurrent)
	if metricsCurrent == nil {
		metricsCurrent = bson.M{}
	}
	snap.Summary = metricsCurrent

	var metricsMongo bson.M
	e.Store.Coll(store.CollMetrics).FindOne(ctx, bson.M{"_id": "mongo"}).Decode(&metricsMongo)
	if metricsMongo == nil {
		metricsMongo = bson.M{"topology": string(e.Topology)}
	}
	snap.Mongo = metricsMongo

	return snap
}

func (e *Engine) buildHotelTiles(ctx context.Context) []HotelTile {
	scope := e.World.Scope(e.Profile.HotelScope)
	inScope := make(map[string]bool, len(scope))
	for _, h := range scope {
		inScope[h.ID] = true
	}

	today := e.Clock.Today()
	occByHotel := map[string]struct{ total, booked int }{}
	cur, err := e.Store.Coll(store.CollDailyInventory).Aggregate(ctx, []bson.M{
		{"$match": bson.M{"date": today}},
		{"$group": bson.M{"_id": "$hotelId", "totalRooms": bson.M{"$sum": "$totalRooms"}, "bookedRooms": bson.M{"$sum": "$bookedRooms"}}},
	})
	if err == nil {
		var rows []bson.M
		cur.All(ctx, &rows)
		cur.Close(ctx)
		for _, r := range rows {
			id, _ := r["_id"].(string)
			total, _ := toInt(r["totalRooms"])
			booked, _ := toInt(r["bookedRooms"])
			occByHotel[id] = struct{ total, booked int }{total, booked}
		}
	}

	activeByHotel := countByHotel(ctx, e.Store, store.CollReservations, bson.M{"status": bson.M{"$in": []string{string(StatusConfirmed), string(StatusCheckedIn)}}})
	arrivalsByHotel := countByHotel(ctx, e.Store, store.CollReservations, bson.M{"checkInDate": today})
	departuresByHotel := countByHotel(ctx, e.Store, store.CollReservations, bson.M{"checkOutDate": today, "status": string(StatusCheckedIn)})

	tiles := make([]HotelTile, 0, len(e.World.Hotels))
	for _, h := range e.World.Hotels {
		occ := occByHotel[h.ID]
		pct := 0.0
		if occ.total > 0 {
			pct = float64(occ.booked) / float64(occ.total)
		}
		class, badge := OccupancyClass(pct)
		if !inScope[h.ID] {
			class, badge = "outofscope", "—" // em dash
		}
		tiles = append(tiles, HotelTile{
			ID: h.ID, Name: h.Name, Region: string(h.Region), Tier: string(h.SizeTier), Category: string(h.Category),
			TotalRooms: h.TotalRooms, OccupancyPct: round2(pct * 100), OccupancyClass: class, Badge: badge,
			ActiveReservations: activeByHotel[h.ID], ArrivalsToday: arrivalsByHotel[h.ID], DeparturesToday: departuresByHotel[h.ID],
			InScope: inScope[h.ID],
		})
	}
	return tiles
}

func countByHotel(ctx context.Context, st *store.Store, coll string, match bson.M) map[string]int64 {
	out := map[string]int64{}
	cur, err := st.Coll(coll).Aggregate(ctx, []bson.M{
		{"$match": match},
		{"$group": bson.M{"_id": "$hotelId", "n": bson.M{"$sum": 1}}},
	})
	if err != nil {
		return out
	}
	defer cur.Close(ctx)
	var rows []bson.M
	cur.All(ctx, &rows)
	for _, r := range rows {
		id, _ := r["_id"].(string)
		n, _ := toInt(r["n"])
		out[id] = int64(n)
	}
	return out
}

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

// HotelDetail serves GET /api/hotels/{id} (§19.3): the hotel's static info, its
// room types, a 21-day inventory window, recent reservations, and — since the
// hotelId equality filter used to fetch all of this is itself shard-key-
// prefixed — an embedded query-routing fact, the same shape §19.7 uses, so
// every detail fetch teaches on its way past.
func (e *Engine) HotelDetail(ctx context.Context, hotelID string) (bson.M, bool) {
	h, ok := e.World.ByID[hotelID]
	if !ok {
		return nil, false
	}
	today := e.Clock.Today()
	cur, err := e.Store.Coll(store.CollDailyInventory).Find(ctx,
		bson.M{"hotelId": hotelID, "date": bson.M{"$gte": today, "$lt": today.AddDate(0, 0, 21)}})
	var inventory []DailyInventory
	if err == nil {
		cur.All(ctx, &inventory)
		cur.Close(ctx)
	}
	recentCur, err := e.Store.Coll(store.CollReservations).Find(ctx, bson.M{"hotelId": hotelID},
		findOptsLimitSortDesc(20))
	var recent []Reservation
	if err == nil {
		recentCur.All(ctx, &recent)
		recentCur.Close(ctx)
	}
	occ := 0.0
	inHouse, arrivals, departures := int64(0), int64(0), int64(0)
	for _, inv := range inventory {
		if inv.Date.Equal(today) && inv.TotalRooms > 0 {
			occ = float64(inv.BookedRooms) / float64(inv.TotalRooms)
		}
	}
	inHouse, _ = e.Store.Coll(store.CollReservations).CountDocuments(ctx, bson.M{"hotelId": hotelID, "status": string(StatusCheckedIn)})
	arrivals, _ = e.Store.Coll(store.CollReservations).CountDocuments(ctx, bson.M{"hotelId": hotelID, "checkInDate": today})
	departures, _ = e.Store.Coll(store.CollReservations).CountDocuments(ctx, bson.M{"hotelId": hotelID, "checkOutDate": today, "status": string(StatusCheckedIn)})

	class, badge := OccupancyClass(occ)
	return bson.M{
		"hotel": h, "roomTypes": h.RoomTypes, "inventory": inventory, "recentReservations": recent,
		"stats": bson.M{"occupancyPct": round2(occ * 100), "occupancyClass": class, "badge": badge, "inHouse": inHouse, "arrivalsToday": arrivals, "departuresToday": departures},
		"query": bson.M{"targeted": true, "shardsTouched": 1, "reason": classifyFilter(true)},
	}, true
}

// ReservationDetail serves GET /api/reservations/{id}, optionally with hotelId+
// checkInDate query params — the pedagogy is in whether those are supplied:
// supplied means a targeted single-shard lookup (the shard key is in the
// filter); omitted means a broadcast to every shard, timed so the difference is
// visible, not just asserted (§19.4).
func (e *Engine) ReservationDetail(ctx context.Context, id, hotelID, checkInDate string) (bson.M, bool) {
	filter := bson.M{"_id": id}
	targeted := hotelID != "" && checkInDate != ""
	if targeted {
		if t, err := time.Parse("2006-01-02", checkInDate); err == nil {
			filter["hotelId"] = hotelID
			filter["checkInDate"] = t
		} else {
			targeted = false
		}
	}
	start := time.Now()
	var res Reservation
	if err := e.Store.Coll(store.CollReservations).FindOne(ctx, filter).Decode(&res); err != nil {
		return nil, false
	}
	elapsed := msSince(start)

	evCur, _ := e.Store.Coll(store.CollReservationEvents).Find(ctx, bson.M{"reservationId": id}, findOptsLimitSortDesc(50))
	var events []store.Event
	if evCur != nil {
		evCur.All(ctx, &events)
		evCur.Close(ctx)
	}

	hotelName := res.HotelName
	return bson.M{
		"reservation": res, "history": res.History, "events": events,
		"hotel": bson.M{"id": res.HotelID, "name": hotelName},
		"query": bson.M{"targeted": targeted, "shardsTouched": shardsTouchedFor(targeted), "durationMs": elapsed, "reason": classifyFilter(targeted)},
	}, true
}

func shardsTouchedFor(targeted bool) int {
	if targeted {
		return 1
	}
	return 3
}

// RecentEvents serves GET /api/events — the authoritative backfill for the
// activity feed on load/reconnect, complementing the WebSocket's lossy live push.
func (e *Engine) RecentEvents(ctx context.Context, limit int64, hotelID string) []store.Event {
	filter := bson.M{}
	if hotelID != "" {
		filter["hotelId"] = hotelID
	}
	cur, err := e.Store.Coll(store.CollReservationEvents).Find(ctx, filter, findOptsLimitSortDesc(limit))
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var out []store.Event
	cur.All(ctx, &out)
	return out
}

// RecentQuerySamples serves GET /api/queries (§19.7).
func (e *Engine) RecentQuerySamples(ctx context.Context, limit int64) []store.QuerySample {
	cur, err := e.Store.Coll(store.CollQuerySamples).Find(ctx, bson.M{}, findOptsLimitSortDesc(limit))
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var out []store.QuerySample
	cur.All(ctx, &out)
	return out
}

func (e *Engine) buildAgentStatuses(ctx context.Context) []AgentStatus {
	cur, err := e.Store.Coll(store.CollAgents).Find(ctx, bson.M{})
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var rows []struct {
		ID           string    `bson:"_id"`
		Type         string    `bson:"type"`
		Status       string    `bson:"status"`
		LastActivity time.Time `bson:"lastActivity"`
		Events       int64     `bson:"events"`
		Errors       int64     `bson:"errors"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil
	}
	now := time.Now()
	out := make([]AgentStatus, 0, len(rows))
	for _, r := range rows {
		out = append(out, AgentStatus{
			Name: r.ID, Type: r.Type, Status: r.Status, LastActivity: r.LastActivity,
			Events: r.Events, Errors: r.Errors, Stale: now.Sub(r.LastActivity) > staleAfter,
		})
	}
	return out
}
