package sim

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"hotelsim/internal/store"
)

// pickCandidate returns a reservation ref to act on: 60% of the time from the
// in-memory recent-reservation ring (a targeted, shard-key-complete follow-up —
// the common case, a guest acting on their own just-made booking), 40% of the
// time from a broadcast query (deliberately not shard-key-prefixed — a
// dashboard-style "find something to modify" scan, the less common but still
// realistic case).
func (e *Engine) pickCandidate(ctx context.Context, rng *rand.Rand, filter bson.D) (ResRef, bool) {
	if rng.Float64() < 0.6 {
		if ref, ok := e.pickRecentReservation(rng); ok {
			return ref, true
		}
	}
	opts := options.Find().SetLimit(20).SetSort(bson.D{{Key: "_id", Value: -1}})
	cur, err := e.Store.Coll(store.CollReservations).Find(ctx, filter, opts)
	if err != nil {
		return ResRef{}, false
	}
	defer cur.Close(ctx)
	var candidates []Reservation
	if err := cur.All(ctx, &candidates); err != nil || len(candidates) == 0 {
		return ResRef{}, false
	}
	r := candidates[rng.Intn(len(candidates))]
	return ResRef{ID: r.ID, HotelID: r.HotelID, CheckInDate: r.CheckInDate}, true
}

// ------------------------------------------------------------- modification

// runModificationAgent picks a confirmed future reservation and applies one of
// three flavors of change: a plain-field edit (cheapest — no inventory impact,
// no transaction), a single-night extend (one guarded findOneAndUpdate, still
// no transaction — the technique that makes a full transaction unnecessary for
// most modifications), or a date/room-type change (the one case that genuinely
// needs Booker.Rebook's transaction, since it touches multiple inventory nights
// atomically with the reservation update).
func (e *Engine) runModificationAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, 3*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "modification", "lifecycle", "idle", events, errs)
			return
		}
		if rng.Float64() > e.Profile.ModifyRate*4 { // ModifyRate is "per simulated day"; scale to this tick
			e.Store.Heartbeat(ctx, "modification", "lifecycle", "ok", events, errs)
			return
		}
		today := e.Clock.Today()
		ref, ok := e.pickCandidate(ctx, rng, bson.D{{Key: "status", Value: string(StatusConfirmed)}, {Key: "checkInDate", Value: bson.D{{Key: "$gt", Value: today}}}})
		if !ok {
			e.Store.Heartbeat(ctx, "modification", "lifecycle", "ok", events, errs)
			return
		}
		switch rng.Intn(10) {
		case 0, 1, 2, 3: // 40%: plain-field edit, no transaction, no inventory impact
			e.modifyGuestCount(ctx, ref, rng)
		case 4, 5, 6: // 30%: single-night extend, guarded, no transaction
			e.extendOneNight(ctx, ref)
		default: // 30%: date/room-type change — genuinely needs a transaction
			newCheckIn := ref.CheckInDate.AddDate(0, 0, 1+rng.Intn(3))
			if _, err := e.booker.Rebook(ctx, ref, newCheckIn, 1+rng.Intn(3)); err != nil {
				errs++
			}
		}
		events++
		e.Store.Heartbeat(ctx, "modification", "lifecycle", "ok", events, errs)
	})
}

// modifyGuestCount is the cheap technique: the current status lives in the
// filter, so an illegal concurrent transition (e.g. the guest already checked
// out) simply matches zero documents — no transaction needed to make this safe.
func (e *Engine) modifyGuestCount(ctx context.Context, ref ResRef, rng *rand.Rand) {
	filter := bson.M{"_id": ref.ID, "hotelId": ref.HotelID, "checkInDate": ref.CheckInDate, "status": string(StatusConfirmed)}
	update := bson.M{
		"$set":  bson.M{"adults": 1 + rng.Intn(3), "updatedAt": time.Now().UTC()},
		"$inc":  bson.M{"version": 1},
		"$push": bson.M{"history": HistoryEntry{At: time.Now().UTC(), SimAt: e.Clock.Now(), Action: "guest_count_changed", By: "modification-agent"}},
	}
	if res, err := e.Store.Coll(store.CollReservations).UpdateOne(ctx, filter, update); err == nil && res.MatchedCount > 0 {
		e.counters.modificationsTotal.Add(1)
		e.PublishEvent(ctx, "reservation_modified", ref.ID, ref.HotelID, "", "modification-agent", "guest count changed")
	}
}

// extendOneNight claims one additional night via a single guarded
// findOneAndUpdate (the filter's availableRooms>=1 guard IS the concurrency
// check) — cheaper than a transaction and sufficient because only one document
// besides the reservation itself is touched.
func (e *Engine) extendOneNight(ctx context.Context, ref ResRef) {
	var res Reservation
	if err := e.Store.Coll(store.CollReservations).FindOne(ctx, bson.M{"_id": ref.ID, "hotelId": ref.HotelID, "status": string(StatusConfirmed)}).Decode(&res); err != nil {
		return
	}
	nextNight := res.CheckOutDate
	invID := InventoryID(res.HotelID, res.RoomTypeCode, nextNight)
	var before DailyInventory
	opt := options.FindOneAndUpdate().SetReturnDocument(options.Before)
	err := e.Store.Coll(store.CollDailyInventory).FindOneAndUpdate(ctx,
		bson.M{"_id": invID, "hotelId": res.HotelID, "date": nextNight, "availableRooms": bson.M{"$gte": 1}},
		bson.M{"$inc": bson.M{"bookedRooms": 1, "availableRooms": -1}}, opt).Decode(&before)
	if err == mongo.ErrNoDocuments {
		e.counters.soldOut.Add(1)
		return
	}
	if err != nil {
		return
	}
	newCheckOut := nextNight.AddDate(0, 0, 1)
	newRates := append(append([]NightlyRate{}, res.NightlyRates...), NightlyRate{Date: nextNight, Amount: before.Rate})
	update := bson.M{
		"$set":  bson.M{"checkOutDate": newCheckOut, "nightlyRates": newRates, "totalAmount": round2(res.TotalAmount + before.Rate), "updatedAt": time.Now().UTC()},
		"$inc":  bson.M{"version": 1},
		"$push": bson.M{"history": HistoryEntry{At: time.Now().UTC(), SimAt: e.Clock.Now(), Action: "extended_one_night", By: "modification-agent"}},
	}
	if r, err := e.Store.Coll(store.CollReservations).UpdateOne(ctx, bson.M{"_id": ref.ID, "hotelId": ref.HotelID, "status": string(StatusConfirmed)}, update); err == nil && r.MatchedCount > 0 {
		e.counters.modificationsTotal.Add(1)
		e.PublishEvent(ctx, "reservation_modified", ref.ID, ref.HotelID, res.HotelName, "modification-agent", "extended one night")
	} else {
		// Reservation vanished between the two updates (e.g. cancelled
		// concurrently) — compensate the night we just claimed.
		e.Store.Coll(store.CollDailyInventory).UpdateOne(ctx, bson.M{"_id": invID}, bson.M{"$inc": bson.M{"bookedRooms": -1, "availableRooms": 1}})
		e.counters.compensations.Add(1)
	}
}

// ------------------------------------------------------------- cancellation

func (e *Engine) runCancellationAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, 4*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "cancellation", "lifecycle", "idle", events, errs)
			return
		}
		if rng.Float64() > e.Profile.CancelRate*4 {
			e.Store.Heartbeat(ctx, "cancellation", "lifecycle", "ok", events, errs)
			return
		}
		today := e.Clock.Today()
		ref, ok := e.pickCandidate(ctx, rng, bson.D{{Key: "status", Value: string(StatusConfirmed)}, {Key: "checkInDate", Value: bson.D{{Key: "$gt", Value: today}}}})
		if !ok {
			e.Store.Heartbeat(ctx, "cancellation", "lifecycle", "ok", events, errs)
			return
		}
		if _, err := e.booker.Cancel(ctx, ref); err != nil {
			errs++
		} else {
			events++
		}
		e.Store.Heartbeat(ctx, "cancellation", "lifecycle", "ok", events, errs)
	})
}

// ------------------------------------------------------------------ check-in

// runCheckInAgent processes reservations whose (simulated) arrival date has
// been reached. The current status living in the findOneAndUpdate filter IS the
// state-machine guard and the concurrency check in one — two concurrent
// check-in attempts on the same reservation can't both win, no transaction
// required.
func (e *Engine) runCheckInAgent(ctx context.Context) {
	var events, errs int64
	rng := newAgentRand()
	tickLoop(ctx, 2*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "check-in", "lifecycle", "idle", events, errs)
			return
		}
		today := e.Clock.Today()
		limit := int64(e.opsThisTick(0.05, 2*time.Second))
		if limit < 1 {
			limit = 5
		}
		cur, err := e.Store.Coll(store.CollReservations).Find(ctx,
			bson.M{"status": string(StatusConfirmed), "checkInDate": today},
			options.Find().SetLimit(limit))
		if err == nil {
			var due []Reservation
			cur.All(ctx, &due)
			cur.Close(ctx)
			for _, r := range due {
				filter := bson.M{"_id": r.ID, "hotelId": r.HotelID, "checkInDate": r.CheckInDate, "status": string(StatusConfirmed)}
				update := bson.M{
					"$set":  bson.M{"status": string(StatusCheckedIn), "actualCheckIn": time.Now().UTC(), "roomNumber": randomRoomNumber(rng), "updatedAt": time.Now().UTC()},
					"$inc":  bson.M{"version": 1},
					"$push": bson.M{"history": HistoryEntry{At: time.Now().UTC(), SimAt: e.Clock.Now(), Action: "checked_in", By: "check-in-agent", From: string(StatusConfirmed), To: string(StatusCheckedIn)}},
				}
				if res, uerr := e.Store.Coll(store.CollReservations).UpdateOne(ctx, filter, update); uerr == nil && res.MatchedCount > 0 {
					events++
					e.counters.checkInsTotal.Add(1)
					e.PublishEvent(ctx, "checked_in", r.ID, r.HotelID, r.HotelName, "check-in-agent", "")
				}
			}
		} else {
			errs++
		}

		// No-shows: still confirmed a full simulated day past check-in.
		noShowCutoff := today.AddDate(0, 0, -1)
		nsRes, _ := e.Store.Coll(store.CollReservations).UpdateMany(ctx,
			bson.M{"status": string(StatusConfirmed), "checkInDate": bson.M{"$lte": noShowCutoff}},
			bson.M{"$set": bson.M{"status": string(StatusNoShow), "updatedAt": time.Now().UTC()}, "$push": bson.M{"history": HistoryEntry{At: time.Now().UTC(), SimAt: e.Clock.Now(), Action: "no_show", By: "check-in-agent"}}})
		if nsRes != nil && nsRes.ModifiedCount > 0 {
			e.counters.noShowsTotal.Add(nsRes.ModifiedCount)
		}
		e.Store.Heartbeat(ctx, "check-in", "lifecycle", "ok", events, errs)
	})
}

func randomRoomNumber(rng *rand.Rand) string {
	floor := 1 + rng.Intn(12)
	room := 1 + rng.Intn(30)
	return fmt.Sprintf("%d%02d", floor, room)
}

// ----------------------------------------------------------------- check-out

func (e *Engine) runCheckOutAgent(ctx context.Context) {
	var events, errs int64
	rng := newAgentRand()
	tickLoop(ctx, 2*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "check-out", "lifecycle", "idle", events, errs)
			return
		}
		today := e.Clock.Today()
		limit := int64(e.opsThisTick(0.05, 2*time.Second))
		if limit < 1 {
			limit = 5
		}
		cur, err := e.Store.Coll(store.CollReservations).Find(ctx,
			bson.M{"status": string(StatusCheckedIn), "checkOutDate": bson.M{"$lte": today}},
			options.Find().SetLimit(limit))
		if err != nil {
			errs++
			e.Store.Heartbeat(ctx, "check-out", "lifecycle", "error", events, errs)
			return
		}
		var due []Reservation
		cur.All(ctx, &due)
		cur.Close(ctx)
		for _, r := range due {
			if rng.Float64() < 0.10 {
				continue // 10% late checkout — skip this tick, catch it next time
			}
			filter := bson.M{"_id": r.ID, "hotelId": r.HotelID, "checkInDate": r.CheckInDate, "status": string(StatusCheckedIn)}
			update := bson.M{
				"$set":  bson.M{"status": string(StatusCheckedOut), "actualCheckOut": time.Now().UTC(), "updatedAt": time.Now().UTC()},
				"$inc":  bson.M{"version": 1},
				"$push": bson.M{"history": HistoryEntry{At: time.Now().UTC(), SimAt: e.Clock.Now(), Action: "checked_out", By: "check-out-agent", From: string(StatusCheckedIn), To: string(StatusCheckedOut)}},
			}
			if res, uerr := e.Store.Coll(store.CollReservations).UpdateOne(ctx, filter, update); uerr == nil && res.MatchedCount > 0 {
				events++
				e.counters.checkOutsTotal.Add(1)
				e.PublishEvent(ctx, "checked_out", r.ID, r.HotelID, r.HotelName, "check-out-agent", "")
			}
		}
		e.Store.Heartbeat(ctx, "check-out", "lifecycle", "ok", events, errs)
	})
}
