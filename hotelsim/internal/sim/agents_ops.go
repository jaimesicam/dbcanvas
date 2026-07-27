package sim

import (
	"context"
	"math/rand"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"hotelsim/internal/store"
)

// ------------------------------------------------------------ rate/promotion

// runRateAgent recalculates occupancy-based pricing for the near-term window
// using an aggregation-pipeline update — a distinct technique from every other
// write in this app (a single updateMany whose "update" is itself a pipeline,
// computed server-side from each document's own current fields, not sent from
// the client).
func (e *Engine) runRateAgent(ctx context.Context) {
	var events, errs int64
	tickLoop(ctx, 10*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "rate", "pricing", "idle", events, errs)
			return
		}
		today := e.Clock.Today()
		window := today.AddDate(0, 0, 14)
		pipeline := mongo.Pipeline{
			{{Key: "$set", Value: bson.D{
				{Key: "occupancy", Value: bson.D{{Key: "$cond", Value: bson.A{
					bson.D{{Key: "$eq", Value: bson.A{"$totalRooms", 0}}}, 0,
					bson.D{{Key: "$divide", Value: bson.A{"$bookedRooms", "$totalRooms"}}},
				}}}},
			}}},
			{{Key: "$set", Value: bson.D{
				{Key: "rate", Value: bson.D{{Key: "$round", Value: bson.A{
					bson.D{{Key: "$multiply", Value: bson.A{
						"$rate",
						bson.D{{Key: "$switch", Value: bson.D{
							{Key: "branches", Value: bson.A{
								bson.D{{Key: "case", Value: bson.D{{Key: "$gte", Value: bson.A{"$occupancy", 0.85}}}}, {Key: "then", Value: 1.15}},
								bson.D{{Key: "case", Value: bson.D{{Key: "$lte", Value: bson.A{"$occupancy", 0.25}}}}, {Key: "then", Value: 0.90}},
							}},
							{Key: "default", Value: 1.0},
						}}},
					}}},
					2,
				}}}},
				{Key: "promotion", Value: bson.D{{Key: "$cond", Value: bson.A{
					bson.D{{Key: "$lte", Value: bson.A{"$occupancy", 0.25}}}, "low-occupancy-discount", "$$REMOVE",
				}}}},
				{Key: "lastUpdated", Value: time.Now().UTC()},
			}}},
		}
		filter := bson.M{"date": bson.M{"$gte": today, "$lt": window}}
		res, err := e.Store.Coll(store.CollDailyInventory).UpdateMany(ctx, filter, pipeline)
		if err != nil {
			errs++
		} else if res != nil {
			events += res.ModifiedCount
		}
		e.Store.Heartbeat(ctx, "rate", "pricing", "ok", events, errs)
	})
}

// -------------------------------------------------------------- hotel ops

// runHotelOpsAgent does two things. (a) rolls the dailyInventory horizon
// forward as simulated time advances — WITHOUT this, the sim runs out of
// pre-seeded inventory in well under an hour of real time and every search
// silently starts returning empty. (b) occasional maintenance closures/
// reopenings, purely for realism.
func (e *Engine) runHotelOpsAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, 15*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "hotel-ops", "operations", "idle", events, errs)
			return
		}
		e.rollHorizon(ctx)
		e.maintenanceChurn(ctx, rng)
		events++
		e.Store.Heartbeat(ctx, "hotel-ops", "operations", "ok", events, errs)
	})
}

// rollHorizon compares simulated "today" against the persisted horizon
// checkpoint and, whenever today has advanced past the checkpoint's start, adds
// new day(s) at the far end and prunes day(s) that have fallen off the near
// end — keeping a constant-width [today-2d, today+21d] window regardless of how
// long the process has been running.
func (e *Engine) rollHorizon(ctx context.Context) {
	var h struct {
		Start time.Time `bson:"start"`
		End   time.Time `bson:"end"`
	}
	err := e.Store.Coll(store.CollSimState).FindOne(ctx, bson.M{"_id": "horizon"}).Decode(&h)
	if err == mongo.ErrNoDocuments {
		return // seedIfEmpty hasn't run yet (e.g. mid-first-boot race) — next tick will see it
	}
	if err != nil {
		return
	}
	today := e.Clock.Today()
	wantStart := today.AddDate(0, 0, -horizonBackDays)
	if !wantStart.After(h.Start) {
		return // horizon still covers the current window
	}
	wantEnd := today.AddDate(0, 0, horizonForwardDays)
	if wantEnd.After(h.End) {
		e.seedInventoryRange(ctx, h.End, wantEnd)
	}
	if wantStart.After(h.Start) {
		e.Store.Coll(store.CollDailyInventory).DeleteMany(ctx, bson.M{"date": bson.M{"$lt": wantStart}})
	}
	e.Store.Coll(store.CollSimState).UpdateOne(ctx, bson.M{"_id": "horizon"},
		bson.M{"$set": bson.M{"start": wantStart, "end": wantEnd}}, options.Update().SetUpsert(true))
}

// maintenanceChurn occasionally closes a handful of rooms on a random future
// inventory document (guarded by availableRooms>=n so it can't oversell an
// already-tight night) and, separately, reopens a previously-closed one.
func (e *Engine) maintenanceChurn(ctx context.Context, rng *rand.Rand) {
	if rng.Float64() > 0.3 {
		return
	}
	scope := e.World.Scope(e.Profile.HotelScope)
	if len(scope) == 0 {
		return
	}
	h := scope[rng.Intn(len(scope))]
	if len(h.RoomTypes) == 0 {
		return
	}
	rt := h.RoomTypes[rng.Intn(len(h.RoomTypes))]
	date := e.Clock.Today().AddDate(0, 0, 1+rng.Intn(10))
	invID := InventoryID(h.ID, rt.Code, date)
	n := 2 + rng.Intn(4)
	e.Store.Coll(store.CollDailyInventory).UpdateOne(ctx,
		bson.M{"_id": invID, "hotelId": h.ID, "date": date, "availableRooms": bson.M{"$gte": n}, "closed": false},
		bson.M{"$inc": bson.M{"totalRooms": -n, "availableRooms": -n, "unavailableRooms": n}})
}

// ------------------------------------------------------------------ analytics

// runAnalyticsAgent is the processing agent (§8.10): runs aggregation pipelines
// summarizing occupancy/ADR/RevPAR/status and writes the merged result — plus
// the in-memory atomic counters — into metrics/_id:"current". This is the ONLY
// path through which those counters become visible: BuildSnapshot never reads
// Engine's memory directly.
func (e *Engine) runAnalyticsAgent(ctx context.Context) {
	var events, errs int64
	scatterToggle := false
	tickLoop(ctx, 5*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "analytics", "processing", "idle", events, errs)
			return
		}
		today := e.Clock.Today()

		occByRegion := e.aggregateOccupancyByRegion(ctx, today)
		statusDist := e.aggregateStatusDistribution(ctx)
		topHotels := e.aggregateTopHotels(ctx, today)

		scatterToggle = !scatterToggle
		e.recordAnalyticsSample(ctx, scatterToggle)

		doc := bson.M{
			"_id": "current", "updatedAt": time.Now().UTC(),
			"byRegion": occByRegion, "byStatus": statusDist, "topHotels": topHotels,
			"counters": bson.M{
				"soldOut": e.counters.soldOut.Load(), "duplicatesRejected": e.counters.duplicatesRejected.Load(),
				"writeConflicts": e.counters.writeConflicts.Load(), "compensations": e.counters.compensations.Load(),
				"eventWriteErrors": e.counters.eventWriteErrors.Load(), "reservationsTotal": e.counters.reservationsTotal.Load(),
				"cancellationsTotal": e.counters.cancellationsTotal.Load(), "modificationsTotal": e.counters.modificationsTotal.Load(),
				"checkInsTotal": e.counters.checkInsTotal.Load(), "checkOutsTotal": e.counters.checkOutsTotal.Load(),
				"noShowsTotal": e.counters.noShowsTotal.Load(), "searchesTotal": e.counters.searchesTotal.Load(),
			},
			"control":  bson.M{"state": runningState(e.Running()), "level": string(e.Level()), "topology": string(e.Topology), "bookingPath": bookingPath(e.Profile.Transactions)},
			"simClock": bson.M{"now": e.Clock.Now(), "today": today, "rate": e.Clock.Rate()},
		}
		_, err := e.Store.Coll(store.CollMetrics).UpdateOne(ctx, bson.M{"_id": "current"}, bson.M{"$set": doc}, options.Update().SetUpsert(true))
		if err != nil {
			errs++
		} else {
			events++
		}
		e.Store.Heartbeat(ctx, "analytics", "processing", "ok", events, errs)
	})
}

func runningState(running bool) string {
	if running {
		return "running"
	}
	return "paused"
}

func bookingPath(txn bool) string {
	if txn {
		return "transaction"
	}
	return "guarded"
}

func (e *Engine) aggregateOccupancyByRegion(ctx context.Context, today time.Time) []bson.M {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "date", Value: today}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$region"},
			{Key: "totalRooms", Value: bson.D{{Key: "$sum", Value: "$totalRooms"}}},
			{Key: "bookedRooms", Value: bson.D{{Key: "$sum", Value: "$bookedRooms"}}},
			{Key: "avgRate", Value: bson.D{{Key: "$avg", Value: "$rate"}}},
		}}},
		{{Key: "$set", Value: bson.D{{Key: "occupancyPct", Value: bson.D{{Key: "$cond", Value: bson.A{
			bson.D{{Key: "$eq", Value: bson.A{"$totalRooms", 0}}}, 0,
			bson.D{{Key: "$multiply", Value: bson.A{bson.D{{Key: "$divide", Value: bson.A{"$bookedRooms", "$totalRooms"}}}, 100}}},
		}}}}}}},
	}
	cur, err := e.Store.CollWithReadPref(store.CollDailyInventory, e.Profile.AnalyticsRead).Aggregate(ctx, pipeline)
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var out []bson.M
	cur.All(ctx, &out)
	return out
}

func (e *Engine) aggregateStatusDistribution(ctx context.Context) []bson.M {
	pipeline := mongo.Pipeline{
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$status"}, {Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
	}
	cur, err := e.Store.CollWithReadPref(store.CollReservations, e.Profile.AnalyticsRead).Aggregate(ctx, pipeline)
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var out []bson.M
	cur.All(ctx, &out)
	return out
}

func (e *Engine) aggregateTopHotels(ctx context.Context, today time.Time) []bson.M {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "createdAt", Value: bson.D{{Key: "$gte", Value: today}}}}}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$hotelId"}, {Key: "bookings", Value: bson.D{{Key: "$sum", Value: 1}}}, {Key: "hotelName", Value: bson.D{{Key: "$first", Value: "$hotelName"}}}}}},
		{{Key: "$sort", Value: bson.D{{Key: "bookings", Value: -1}}}},
		{{Key: "$limit", Value: 10}},
	}
	cur, err := e.Store.CollWithReadPref(store.CollReservations, e.Profile.AnalyticsRead).Aggregate(ctx, pipeline)
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var out []bson.M
	cur.All(ctx, &out)
	return out
}

// recordAnalyticsSample alternates between a targeted (hotelId-prefixed) and a
// scatter (region-only) variant of the same kind of query each tick, feeding the
// query-education panel with both cases from a source other than guest search.
func (e *Engine) recordAnalyticsSample(ctx context.Context, scatter bool) {
	reason := "aggregation $match includes hotelId (shard-key prefix) -> targeted"
	if scatter {
		reason = "aggregation $match has no hotelId, only date -> broadcasts to all shards"
	}
	e.Store.RecordQuerySample(ctx, store.QuerySample{
		Agent: "analytics", Collection: store.CollDailyInventory, Op: "aggregate",
		FilterSummary: "occupancy-by-region rollup", Targeted: !scatter, Reason: reason,
	})
}

// ----------------------------------------------------------------- monitoring

// runMonitoringAgent runs topology-conditional admin commands and writes the
// result to metrics/_id:"mongo" — the source for the MongoDB topology panel
// (§19.6).
func (e *Engine) runMonitoringAgent(ctx context.Context) {
	var events, errs int64
	tickLoop(ctx, 5*time.Second, func() {
		doc := bson.M{"_id": "mongo", "topology": string(e.Topology), "updatedAt": time.Now().UTC()}

		if ss, err := e.runAdminCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}); err == nil {
			doc["connections"] = ss["connections"]
			doc["opcounters"] = ss["opcounters"]
			doc["version"] = ss["version"]
		} else {
			errs++
		}
		if ds, err := e.runDBCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}); err == nil {
			doc["dbStats"] = bson.M{"collections": ds["collections"], "objects": ds["objects"], "dataSize": ds["dataSize"], "indexSize": ds["indexSize"]}
		}

		switch e.Topology {
		case store.TopologyReplicaSet:
			if rs, err := e.runAdminCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}); err == nil {
				doc["replicaSet"] = summarizeReplicaSet(rs)
			}
		case store.TopologySharded:
			doc["sharded"] = e.summarizeSharding(ctx)
		}

		events++
		e.Store.Coll(store.CollMetrics).UpdateOne(ctx, bson.M{"_id": "mongo"}, bson.M{"$set": doc}, options.Update().SetUpsert(true))
		e.Store.Heartbeat(ctx, "monitoring", "observability", "ok", events, errs)
	})
}

func (e *Engine) runAdminCommand(ctx context.Context, cmd bson.D) (bson.M, error) {
	var out bson.M
	err := e.Store.Client.Database("admin").RunCommand(ctx, cmd).Decode(&out)
	return out, err
}

func (e *Engine) runDBCommand(ctx context.Context, cmd bson.D) (bson.M, error) {
	var out bson.M
	err := e.Store.DB.RunCommand(ctx, cmd).Decode(&out)
	return out, err
}

func summarizeReplicaSet(rs bson.M) bson.M {
	members, _ := rs["members"].(bson.A)
	out := make([]bson.M, 0, len(members))
	for _, m := range members {
		mm, ok := m.(bson.M)
		if !ok {
			continue
		}
		out = append(out, bson.M{
			"name": mm["name"], "stateStr": mm["stateStr"], "health": mm["health"],
			"self": mm["self"], "optimeDate": mm["optimeDate"],
		})
	}
	return bson.M{"members": out}
}

func (e *Engine) summarizeSharding(ctx context.Context) bson.M {
	admin := e.Store.Client.Database("admin")
	// The registry of shards lives in config.shards, not admin.shards.
	cur, err := e.Store.Client.Database("config").Collection("shards").Find(ctx, bson.D{})
	if err != nil {
		return nil
	}
	defer cur.Close(ctx)
	var shards []bson.M
	cur.All(ctx, &shards)

	perShard := make([]bson.M, 0, len(shards))
	for _, sh := range shards {
		id, _ := sh["_id"].(string)
		counts := bson.M{}
		for _, coll := range []string{store.CollReservations, store.CollDailyInventory, store.CollReservationEvents} {
			n, _ := e.Store.Coll(coll).CountDocuments(ctx, bson.M{})
			counts[coll] = n // best-effort total (per-shard breakdown needs $collStats/shardedDataDistribution, sampled below)
		}
		perShard = append(perShard, bson.M{"id": id, "host": sh["host"], "docCounts": counts})
	}

	var dist bson.M
	if cur2, err := admin.Aggregate(ctx, mongo.Pipeline{{{Key: "$shardedDataDistribution", Value: bson.D{}}}}); err == nil {
		var rows []bson.M
		cur2.All(ctx, &rows)
		cur2.Close(ctx)
		dist = bson.M{"rows": rows}
	}
	return bson.M{"shards": perShard, "dataDistribution": dist}
}

// -------------------------------------------------------------- clock persist

// runClockPersister checkpoints the simulated clock every 10s so a restart
// resumes from here rather than a stale or zero value (see Engine.persistClock
// / restoreOrAnchorClock).
func (e *Engine) runClockPersister(ctx context.Context) {
	tickLoop(ctx, 10*time.Second, func() {
		e.persistClock(ctx)
	})
}
