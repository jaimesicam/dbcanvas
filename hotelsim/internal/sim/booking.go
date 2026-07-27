// booking.go is the file to open to learn what this whole app demonstrates: the
// two ways MongoDB gives you to make a multi-step write safe, and when each one
// actually earns its cost.
package sim

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"hotelsim/internal/store"
)

// errSoldOut / errDuplicate are internal sentinels returned from inside a
// transaction callback to short-circuit it — WithTransaction aborts immediately
// on ANY non-nil, non-transient error (only a genuine write conflict gets
// silently retried), so returning one of these is exactly how "this booking
// can't proceed" gets out of the transaction without it being mistaken for a
// retryable failure.
var (
	errSoldOut   = errors.New("sold_out")
	errDuplicate = errors.New("duplicate")
)

type BookRequest struct {
	RequestID    string
	HotelID      string
	HotelName    string
	Region       Region
	RoomTypeCode RoomTypeCode
	CheckIn      time.Time
	Nights       int
	Adults       int
	Children     int
	GuestID      string
	GuestName    string
}

type BookOutcome struct {
	Result      string // "booked" | "sold_out" | "duplicate" | "conflict" | "error"
	Reservation *Reservation
	Attempts    int    // WithTransaction callback invocations — the write-conflict signal (always 1 without transactions)
	ExistingID  string // set when Result == "duplicate"
	Path        string // "transaction" | "guarded"
	DurationMs  float64
}

// Booker is the topology-dependent reservation write path. Multi-document
// transactions require a replica set (4.0+) or a sharded cluster (4.2+) — a
// standalone mongod cannot start a session transaction at all, so hotelsim runs a
// compensating-rollback path there instead, and says so, loudly, in the UI.
type Booker interface {
	Reserve(ctx context.Context, req BookRequest) (BookOutcome, error)
	Cancel(ctx context.Context, ref ResRef) (BookOutcome, error)
	Rebook(ctx context.Context, ref ResRef, newCheckIn time.Time, newNights int) (BookOutcome, error)
}

// txnOpts builds the transaction options every Booker transaction uses: the
// topology's configured write concern, snapshot read concern, and primary read
// preference (transaction reads must go to the primary — this is a hard driver
// requirement, not a tuning choice).
func (e *Engine) txnOpts() *options.TransactionOptions {
	return options.Transaction().
		SetWriteConcern(e.Profile.WriteConcern).
		SetReadConcern(readconcern.Snapshot()).
		SetReadPreference(readpref.Primary())
}

// nightDates returns the calendar nights [checkIn, checkIn+nights).
func nightDates(checkIn time.Time, nights int) []time.Time {
	out := make([]time.Time, nights)
	for i := range out {
		out[i] = checkIn.AddDate(0, 0, i)
	}
	return out
}

// ---------------------------------------------------------------- txnBooker

// txnBooker is used whenever Profile.Transactions is true (replica set, sharded).
type txnBooker struct{ e *Engine }

func (b *txnBooker) Reserve(ctx context.Context, req BookRequest) (BookOutcome, error) {
	start := time.Now()
	e := b.e
	confNo := NewConfirmationNumber(req.HotelID, req.CheckIn, e.nextConfirmationSeq())
	nights := nightDates(req.CheckIn, req.Nights)

	sess, err := e.Store.Client.StartSession()
	if err != nil {
		return BookOutcome{Result: "error"}, err
	}
	defer sess.EndSession(ctx)

	txnOpts := e.txnOpts()

	var reservation Reservation
	attempts := 0
	_, txnErr := sess.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		attempts++ // counted here, not after — WithTransaction retries write conflicts silently
		var rates []NightlyRate
		var total float64
		for _, d := range nights {
			invID := InventoryID(req.HotelID, req.RoomTypeCode, d)
			filter := bson.M{"_id": invID, "hotelId": req.HotelID, "date": d, "availableRooms": bson.M{"$gte": 1}}
			update := bson.M{"$inc": bson.M{"bookedRooms": 1, "availableRooms": -1}}
			var before DailyInventory
			opt := options.FindOneAndUpdate().SetReturnDocument(options.Before)
			err := e.Store.Coll(store.CollDailyInventory).FindOneAndUpdate(sc, filter, update, opt).Decode(&before)
			if err == mongo.ErrNoDocuments {
				return nil, errSoldOut
			}
			if err != nil {
				return nil, err
			}
			rates = append(rates, NightlyRate{Date: d, Amount: before.Rate})
			total += before.Rate
		}

		if _, err := e.Store.Coll(store.CollReservationRequests).InsertOne(sc, bson.M{
			"_id": req.RequestID, "reservationId": confNo, "createdAt": time.Now().UTC(),
		}); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				return nil, errDuplicate
			}
			return nil, err
		}

		reservation = Reservation{
			ID: confNo, RequestID: req.RequestID, GuestID: req.GuestID, GuestName: req.GuestName,
			HotelID: req.HotelID, HotelName: req.HotelName, Region: req.Region, RoomTypeCode: req.RoomTypeCode,
			CheckInDate: req.CheckIn, CheckOutDate: nights[len(nights)-1].AddDate(0, 0, 1),
			NumberOfRooms: 1, Adults: req.Adults, Children: req.Children,
			NightlyRates: rates, TotalAmount: round2(total), Currency: "USD",
			Status: StatusConfirmed, Version: 1,
			History:   []HistoryEntry{{At: time.Now().UTC(), SimAt: e.Clock.Now(), Action: "created", By: "reservation-agent"}},
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if _, err := e.Store.Coll(store.CollReservations).InsertOne(sc, reservation); err != nil {
			return nil, err
		}
		if _, err := e.Store.Coll(store.CollReservationEvents).InsertOne(sc, store.Event{
			Kind: "reservation_created", ReservationID: confNo, HotelID: req.HotelID, HotelName: req.HotelName,
			Agent: "reservation-agent", CreatedAt: time.Now().UTC(), SimAt: e.Clock.Now(),
		}); err != nil {
			return nil, err
		}
		return nil, nil
	}, txnOpts)

	out := BookOutcome{Attempts: attempts, Path: "transaction", DurationMs: msSince(start)}
	if attempts > 1 {
		e.counters.writeConflicts.Add(1)
	}
	switch {
	case errors.Is(txnErr, errSoldOut):
		e.counters.soldOut.Add(1)
		out.Result = "sold_out"
		return out, nil
	case errors.Is(txnErr, errDuplicate):
		e.counters.duplicatesRejected.Add(1)
		out.Result = "duplicate"
		out.ExistingID = lookupExistingReservation(ctx, e, req.RequestID)
		return out, nil
	case txnErr != nil:
		out.Result = "error"
		return out, txnErr
	}
	out.Result = "booked"
	out.Reservation = &reservation
	e.counters.reservationsTotal.Add(1)
	return out, nil
}

func (b *txnBooker) Cancel(ctx context.Context, ref ResRef) (BookOutcome, error) {
	start := time.Now()
	e := b.e
	sess, err := e.Store.Client.StartSession()
	if err != nil {
		return BookOutcome{Result: "error"}, err
	}
	defer sess.EndSession(ctx)

	attempts := 0
	var cancelled bool
	_, txnErr := sess.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		attempts++
		var res Reservation
		filter := bson.M{"_id": ref.ID, "hotelId": ref.HotelID, "checkInDate": ref.CheckInDate, "status": string(StatusConfirmed)}
		update := bson.M{
			"$set":  bson.M{"status": string(StatusCancelled), "updatedAt": time.Now().UTC()},
			"$push": bson.M{"history": HistoryEntry{At: time.Now().UTC(), SimAt: e.Clock.Now(), Action: "cancelled", By: "cancellation-agent", From: string(StatusConfirmed), To: string(StatusCancelled)}},
		}
		opt := options.FindOneAndUpdate().SetReturnDocument(options.Before)
		if err := e.Store.Coll(store.CollReservations).FindOneAndUpdate(sc, filter, update, opt).Decode(&res); err != nil {
			if err == mongo.ErrNoDocuments {
				return nil, nil // already cancelled/checked-in by someone else — not an error, just a no-op
			}
			return nil, err
		}
		cancelled = true
		for _, d := range nightDates(res.CheckInDate, len(res.NightlyRates)) {
			invID := InventoryID(res.HotelID, res.RoomTypeCode, d)
			e.Store.Coll(store.CollDailyInventory).UpdateOne(sc,
				bson.M{"_id": invID, "hotelId": res.HotelID, "date": d},
				bson.M{"$inc": bson.M{"bookedRooms": -1, "availableRooms": 1}})
		}
		e.Store.Coll(store.CollReservationEvents).InsertOne(sc, store.Event{
			Kind: "reservation_cancelled", ReservationID: res.ID, HotelID: res.HotelID, HotelName: res.HotelName,
			Agent: "cancellation-agent", CreatedAt: time.Now().UTC(), SimAt: e.Clock.Now(),
		})
		return nil, nil
	}, e.txnOpts())
	out := BookOutcome{Attempts: attempts, Path: "transaction", DurationMs: msSince(start)}
	if txnErr != nil {
		out.Result = "error"
		return out, txnErr
	}
	if !cancelled {
		out.Result = "conflict"
		return out, nil
	}
	out.Result = "booked"
	e.counters.cancellationsTotal.Add(1)
	return out, nil
}

func (b *txnBooker) Rebook(ctx context.Context, ref ResRef, newCheckIn time.Time, newNights int) (BookOutcome, error) {
	start := time.Now()
	e := b.e
	sess, err := e.Store.Client.StartSession()
	if err != nil {
		return BookOutcome{Result: "error"}, err
	}
	defer sess.EndSession(ctx)

	attempts := 0
	var updated Reservation
	var ok bool
	_, txnErr := sess.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		attempts++
		var res Reservation
		filter := bson.M{"_id": ref.ID, "hotelId": ref.HotelID, "checkInDate": ref.CheckInDate, "status": string(StatusConfirmed)}
		if err := e.Store.Coll(store.CollReservations).FindOne(sc, filter).Decode(&res); err != nil {
			if err == mongo.ErrNoDocuments {
				return nil, nil
			}
			return nil, err
		}
		oldNights := nightDates(res.CheckInDate, len(res.NightlyRates))
		newNightDates := nightDates(newCheckIn, newNights)

		var rates []NightlyRate
		var total float64
		for _, d := range newNightDates {
			invID := InventoryID(res.HotelID, res.RoomTypeCode, d)
			filter := bson.M{"_id": invID, "hotelId": res.HotelID, "date": d, "availableRooms": bson.M{"$gte": 1}}
			var before DailyInventory
			opt := options.FindOneAndUpdate().SetReturnDocument(options.Before)
			if err := e.Store.Coll(store.CollDailyInventory).FindOneAndUpdate(sc,
				filter, bson.M{"$inc": bson.M{"bookedRooms": 1, "availableRooms": -1}}, opt).Decode(&before); err != nil {
				if err == mongo.ErrNoDocuments {
					return nil, errSoldOut
				}
				return nil, err
			}
			rates = append(rates, NightlyRate{Date: d, Amount: before.Rate})
			total += before.Rate
		}
		for _, d := range oldNights {
			invID := InventoryID(res.HotelID, res.RoomTypeCode, d)
			e.Store.Coll(store.CollDailyInventory).UpdateOne(sc,
				bson.M{"_id": invID, "hotelId": res.HotelID, "date": d},
				bson.M{"$inc": bson.M{"bookedRooms": -1, "availableRooms": 1}})
		}

		update := bson.M{
			"$set": bson.M{
				"checkInDate": newCheckIn, "checkOutDate": newNightDates[len(newNightDates)-1].AddDate(0, 0, 1),
				"nightlyRates": rates, "totalAmount": round2(total), "updatedAt": time.Now().UTC(),
			},
			"$inc":  bson.M{"version": 1},
			"$push": bson.M{"history": HistoryEntry{At: time.Now().UTC(), SimAt: e.Clock.Now(), Action: "rebooked", By: "modification-agent"}},
		}
		res2 := e.Store.Coll(store.CollReservations).FindOneAndUpdate(sc, filter, update, options.FindOneAndUpdate().SetReturnDocument(options.After))
		if err := res2.Decode(&updated); err != nil {
			return nil, err
		}
		ok = true
		e.Store.Coll(store.CollReservationEvents).InsertOne(sc, store.Event{
			Kind: "reservation_modified", ReservationID: res.ID, HotelID: res.HotelID, HotelName: res.HotelName,
			Agent: "modification-agent", CreatedAt: time.Now().UTC(), SimAt: e.Clock.Now(),
		})
		return nil, nil
	}, e.txnOpts())

	out := BookOutcome{Attempts: attempts, Path: "transaction", DurationMs: msSince(start)}
	switch {
	case errors.Is(txnErr, errSoldOut):
		out.Result = "sold_out"
		e.counters.soldOut.Add(1)
		return out, nil
	case txnErr != nil:
		out.Result = "error"
		return out, txnErr
	case !ok:
		out.Result = "conflict"
		return out, nil
	}
	out.Result = "booked"
	out.Reservation = &updated
	e.counters.modificationsTotal.Add(1)
	return out, nil
}

// ------------------------------------------------------------- guardedBooker

// guardedBooker is used when Profile.Transactions is false (standalone — no
// multi-document transaction support at all). Each write commits independently;
// on partial failure it issues explicit compensating reversals for whatever
// already succeeded. Between a partial claim and its compensation there is a
// real (if brief) window where inventory is understated — which is precisely why
// transactions exist elsewhere. That window is surfaced via Path:"guarded" in
// the UI rather than hidden.
type guardedBooker struct{ e *Engine }

func (b *guardedBooker) Reserve(ctx context.Context, req BookRequest) (BookOutcome, error) {
	start := time.Now()
	e := b.e
	confNo := NewConfirmationNumber(req.HotelID, req.CheckIn, e.nextConfirmationSeq())
	nights := nightDates(req.CheckIn, req.Nights)

	// Dedup check runs first, before any inventory mutation, so a duplicate is
	// rejected before any compensating work would ever be needed.
	if _, err := e.Store.Coll(store.CollReservationRequests).InsertOne(ctx, bson.M{
		"_id": req.RequestID, "reservationId": confNo, "createdAt": time.Now().UTC(),
	}); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			e.counters.duplicatesRejected.Add(1)
			return BookOutcome{Result: "duplicate", Path: "guarded", ExistingID: lookupExistingReservation(ctx, e, req.RequestID), DurationMs: msSince(start)}, nil
		}
		return BookOutcome{Result: "error", Path: "guarded"}, err
	}

	var rates []NightlyRate
	var total float64
	claimed := 0
	for _, d := range nights {
		invID := InventoryID(req.HotelID, req.RoomTypeCode, d)
		filter := bson.M{"_id": invID, "hotelId": req.HotelID, "date": d, "availableRooms": bson.M{"$gte": 1}}
		var before DailyInventory
		opt := options.FindOneAndUpdate().SetReturnDocument(options.Before)
		err := e.Store.Coll(store.CollDailyInventory).FindOneAndUpdate(ctx, filter,
			bson.M{"$inc": bson.M{"bookedRooms": 1, "availableRooms": -1}}, opt).Decode(&before)
		if err == mongo.ErrNoDocuments {
			// Compensate every night claimed so far, then drop the dedup
			// marker — this attempt never happened, as far as a future retry
			// (same or different requestId) should be concerned.
			for _, cd := range nights[:claimed] {
				cinvID := InventoryID(req.HotelID, req.RoomTypeCode, cd)
				e.Store.Coll(store.CollDailyInventory).UpdateOne(ctx,
					bson.M{"_id": cinvID}, bson.M{"$inc": bson.M{"bookedRooms": -1, "availableRooms": 1}})
			}
			e.Store.Coll(store.CollReservationRequests).DeleteOne(ctx, bson.M{"_id": req.RequestID})
			e.counters.soldOut.Add(1)
			if claimed > 0 {
				e.counters.compensations.Add(1)
			}
			return BookOutcome{Result: "sold_out", Path: "guarded", DurationMs: msSince(start)}, nil
		}
		if err != nil {
			return BookOutcome{Result: "error", Path: "guarded"}, err
		}
		rates = append(rates, NightlyRate{Date: d, Amount: before.Rate})
		total += before.Rate
		claimed++
	}

	reservation := Reservation{
		ID: confNo, RequestID: req.RequestID, GuestID: req.GuestID, GuestName: req.GuestName,
		HotelID: req.HotelID, HotelName: req.HotelName, Region: req.Region, RoomTypeCode: req.RoomTypeCode,
		CheckInDate: req.CheckIn, CheckOutDate: nights[len(nights)-1].AddDate(0, 0, 1),
		NumberOfRooms: 1, Adults: req.Adults, Children: req.Children,
		NightlyRates: rates, TotalAmount: round2(total), Currency: "USD",
		Status: StatusConfirmed, Version: 1,
		History:   []HistoryEntry{{At: time.Now().UTC(), SimAt: e.Clock.Now(), Action: "created", By: "reservation-agent"}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if _, err := e.Store.Coll(store.CollReservations).InsertOne(ctx, reservation); err != nil {
		return BookOutcome{Result: "error", Path: "guarded"}, err
	}
	e.PublishEvent(ctx, "reservation_created", confNo, req.HotelID, req.HotelName, "reservation-agent", "")
	e.counters.reservationsTotal.Add(1)
	return BookOutcome{Result: "booked", Reservation: &reservation, Path: "guarded", DurationMs: msSince(start)}, nil
}

func (b *guardedBooker) Cancel(ctx context.Context, ref ResRef) (BookOutcome, error) {
	start := time.Now()
	e := b.e
	var res Reservation
	filter := bson.M{"_id": ref.ID, "hotelId": ref.HotelID, "checkInDate": ref.CheckInDate, "status": string(StatusConfirmed)}
	update := bson.M{
		"$set":  bson.M{"status": string(StatusCancelled), "updatedAt": time.Now().UTC()},
		"$push": bson.M{"history": HistoryEntry{At: time.Now().UTC(), SimAt: e.Clock.Now(), Action: "cancelled", By: "cancellation-agent"}},
	}
	opt := options.FindOneAndUpdate().SetReturnDocument(options.Before)
	if err := e.Store.Coll(store.CollReservations).FindOneAndUpdate(ctx, filter, update, opt).Decode(&res); err != nil {
		if err == mongo.ErrNoDocuments {
			return BookOutcome{Result: "conflict", Path: "guarded", DurationMs: msSince(start)}, nil
		}
		return BookOutcome{Result: "error", Path: "guarded"}, err
	}
	for _, d := range nightDates(res.CheckInDate, len(res.NightlyRates)) {
		invID := InventoryID(res.HotelID, res.RoomTypeCode, d)
		e.Store.Coll(store.CollDailyInventory).UpdateOne(ctx,
			bson.M{"_id": invID, "hotelId": res.HotelID, "date": d},
			bson.M{"$inc": bson.M{"bookedRooms": -1, "availableRooms": 1}})
	}
	e.PublishEvent(ctx, "reservation_cancelled", res.ID, res.HotelID, res.HotelName, "cancellation-agent", "")
	e.counters.cancellationsTotal.Add(1)
	return BookOutcome{Result: "booked", Path: "guarded", DurationMs: msSince(start)}, nil
}

func (b *guardedBooker) Rebook(ctx context.Context, ref ResRef, newCheckIn time.Time, newNights int) (BookOutcome, error) {
	start := time.Now()
	e := b.e
	var res Reservation
	filter := bson.M{"_id": ref.ID, "hotelId": ref.HotelID, "checkInDate": ref.CheckInDate, "status": string(StatusConfirmed)}
	if err := e.Store.Coll(store.CollReservations).FindOne(ctx, filter).Decode(&res); err != nil {
		if err == mongo.ErrNoDocuments {
			return BookOutcome{Result: "conflict", Path: "guarded", DurationMs: msSince(start)}, nil
		}
		return BookOutcome{Result: "error", Path: "guarded"}, err
	}
	newNightDates := nightDates(newCheckIn, newNights)
	var rates []NightlyRate
	var total float64
	claimed := 0
	for _, d := range newNightDates {
		invID := InventoryID(res.HotelID, res.RoomTypeCode, d)
		var before DailyInventory
		opt := options.FindOneAndUpdate().SetReturnDocument(options.Before)
		err := e.Store.Coll(store.CollDailyInventory).FindOneAndUpdate(ctx,
			bson.M{"_id": invID, "hotelId": res.HotelID, "date": d, "availableRooms": bson.M{"$gte": 1}},
			bson.M{"$inc": bson.M{"bookedRooms": 1, "availableRooms": -1}}, opt).Decode(&before)
		if err == mongo.ErrNoDocuments {
			for _, cd := range newNightDates[:claimed] {
				cinvID := InventoryID(res.HotelID, res.RoomTypeCode, cd)
				e.Store.Coll(store.CollDailyInventory).UpdateOne(ctx, bson.M{"_id": cinvID}, bson.M{"$inc": bson.M{"bookedRooms": -1, "availableRooms": 1}})
			}
			e.counters.soldOut.Add(1)
			if claimed > 0 {
				e.counters.compensations.Add(1)
			}
			return BookOutcome{Result: "sold_out", Path: "guarded", DurationMs: msSince(start)}, nil
		}
		if err != nil {
			return BookOutcome{Result: "error", Path: "guarded"}, err
		}
		rates = append(rates, NightlyRate{Date: d, Amount: before.Rate})
		total += before.Rate
		claimed++
	}
	for _, d := range nightDates(res.CheckInDate, len(res.NightlyRates)) {
		invID := InventoryID(res.HotelID, res.RoomTypeCode, d)
		e.Store.Coll(store.CollDailyInventory).UpdateOne(ctx, bson.M{"_id": invID, "hotelId": res.HotelID, "date": d}, bson.M{"$inc": bson.M{"bookedRooms": -1, "availableRooms": 1}})
	}
	update := bson.M{
		"$set": bson.M{
			"checkInDate": newCheckIn, "checkOutDate": newNightDates[len(newNightDates)-1].AddDate(0, 0, 1),
			"nightlyRates": rates, "totalAmount": round2(total), "updatedAt": time.Now().UTC(),
		},
		"$inc":  bson.M{"version": 1},
		"$push": bson.M{"history": HistoryEntry{At: time.Now().UTC(), SimAt: e.Clock.Now(), Action: "rebooked", By: "modification-agent"}},
	}
	var updated Reservation
	if err := e.Store.Coll(store.CollReservations).FindOneAndUpdate(ctx, filter, update,
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&updated); err != nil {
		return BookOutcome{Result: "error", Path: "guarded"}, err
	}
	e.PublishEvent(ctx, "reservation_modified", res.ID, res.HotelID, res.HotelName, "modification-agent", "")
	e.counters.modificationsTotal.Add(1)
	return BookOutcome{Result: "booked", Reservation: &updated, Path: "guarded", DurationMs: msSince(start)}, nil
}

// ------------------------------------------------------------------- helpers

func lookupExistingReservation(ctx context.Context, e *Engine, requestID string) string {
	var doc struct {
		ReservationID string `bson:"reservationId"`
	}
	if err := e.Store.Coll(store.CollReservationRequests).FindOne(ctx, bson.M{"_id": requestID}).Decode(&doc); err != nil {
		return ""
	}
	return doc.ReservationID
}

func msSince(start time.Time) float64 { return float64(time.Since(start).Microseconds()) / 1000.0 }
