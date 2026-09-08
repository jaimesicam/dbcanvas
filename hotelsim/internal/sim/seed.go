package sim

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"hotelsim/internal/store"
)

// horizonBackDays / horizonForwardDays bound the pre-seeded (and continuously
// rolled — see runHotelOpsAgent) dailyInventory window relative to simulated
// "today". 21 days forward is enough runway for the longest booking lead time
// (14 days out) plus the longest stay (7 nights) to always land inside seeded
// inventory.
const (
	horizonBackDays    = 2
	horizonForwardDays = 21
)

// InventoryID builds the deterministic composite _id dailyInventory uses instead
// of a separate unique index — a unique index on a sharded collection must be
// prefixed by its shard key ({hotelId, date}), and {hotelId, roomTypeId, date}
// is not, so the "obvious" unique index would be illegal. The composite _id
// sidesteps that entirely and makes every inventory upsert idempotent.
func InventoryID(hotelID string, code RoomTypeCode, date time.Time) string {
	return fmt.Sprintf("%s|%s|%s", hotelID, code, date.Format("2006-01-02"))
}

// NewConfirmationNumber builds the reservation _id — also deliberately not paired
// with a separate unique "confirmationNumber" field with its own index, for the
// same sharded-unique-index reason as InventoryID.
func NewConfirmationNumber(hotelID string, checkIn time.Time, seq int64) string {
	return fmt.Sprintf("%s-%s-%04d", hotelID, checkIn.Format("060102"), seq%10000)
}

// seedIfEmpty inserts the static hotel/room-type topology and the initial
// dailyInventory horizon exactly once (checked via the hotels collection being
// empty) — never re-run on every Start, only when the database is genuinely
// fresh (a first boot, or right after Reset's wipe).
func (e *Engine) seedIfEmpty(ctx context.Context) {
	n, err := e.Store.Coll(store.CollHotels).EstimatedDocumentCount(ctx)
	if err != nil {
		log.Printf("hotelsim: seed check: %v", err)
		return
	}
	if n > 0 {
		return
	}

	log.Printf("hotelsim: seeding %d hotels + inventory horizon", len(e.World.Hotels))

	hotelDocs := make([]interface{}, 0, len(e.World.Hotels))
	roomTypeDocs := make([]interface{}, 0, len(e.World.Hotels)*4)
	for _, h := range e.World.Hotels {
		h.LastUpdated = time.Now().UTC()
		hotelDocs = append(hotelDocs, *h)
		for _, rt := range h.RoomTypes {
			roomTypeDocs = append(roomTypeDocs, rt)
		}
	}
	if _, err := e.Store.Coll(store.CollHotels).InsertMany(ctx, hotelDocs); err != nil {
		log.Printf("hotelsim: seed hotels: %v", err)
	}
	if _, err := e.Store.Coll(store.CollRoomTypes).InsertMany(ctx, roomTypeDocs); err != nil {
		log.Printf("hotelsim: seed room types: %v", err)
	}

	today := e.Clock.Today()
	start := today.AddDate(0, 0, -horizonBackDays)
	end := today.AddDate(0, 0, horizonForwardDays)
	e.seedInventoryRange(ctx, start, end)

	_, err = e.Store.Coll(store.CollSimState).UpdateOne(ctx,
		bson.M{"_id": "horizon"},
		bson.M{"$set": bson.M{"_id": "horizon", "start": start, "end": end}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		log.Printf("hotelsim: seed horizon checkpoint: %v", err)
	}
}

// seedInventoryRange inserts one dailyInventory document per hotel × room type ×
// day in [start, end), batched to stay well under MongoDB's 16MB/1000-op limits.
// Unordered so one duplicate _id (a day already seeded by a prior partial run or
// a concurrent horizon-roller tick) doesn't abort the rest of the batch.
func (e *Engine) seedInventoryRange(ctx context.Context, start, end time.Time) {
	const batchSize = 1000
	opts := options.InsertMany().SetOrdered(false)
	var batch []interface{}
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if _, err := e.Store.Coll(store.CollDailyInventory).InsertMany(ctx, batch, opts); err != nil && !isAllDuplicateKeyErrors(err) {
			log.Printf("hotelsim: seed inventory batch: %v", err)
		}
		batch = batch[:0]
	}
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		for _, h := range e.World.Hotels {
			for _, rt := range h.RoomTypes {
				batch = append(batch, DailyInventory{
					ID: InventoryID(h.ID, rt.Code, d), HotelID: h.ID, Region: h.Region, RoomTypeCode: rt.Code, Date: d,
					TotalRooms: rt.RoomCount, AvailableRooms: rt.RoomCount, Rate: rt.BaseRate,
					LastUpdated: time.Now().UTC(),
				})
				if len(batch) >= batchSize {
					flush()
				}
			}
		}
	}
	flush()
}

// isAllDuplicateKeyErrors reports whether every write error in a bulk-write
// exception is a duplicate-key error (code 11000) — the expected, harmless
// outcome when re-seeding a day that already exists.
func isAllDuplicateKeyErrors(err error) bool {
	var bwe mongo.BulkWriteException
	if !errors.As(err, &bwe) || len(bwe.WriteErrors) == 0 {
		return false
	}
	for _, e := range bwe.WriteErrors {
		if e.Code != 11000 {
			return false
		}
	}
	return true
}
