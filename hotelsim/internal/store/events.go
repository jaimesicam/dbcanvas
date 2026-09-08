package store

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Event is one reservationEvents document — the durable, replayable activity feed
// the change-stream watcher / poller tails and the recent-activity panel reads.
type Event struct {
	ID            primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Kind          string             `bson:"kind" json:"kind"`
	ReservationID string             `bson:"reservationId,omitempty" json:"reservationId,omitempty"`
	HotelID       string             `bson:"hotelId" json:"hotelId"`
	HotelName     string             `bson:"hotelName,omitempty" json:"hotelName,omitempty"`
	Agent         string             `bson:"agent" json:"agent"`
	Detail        string             `bson:"detail,omitempty" json:"detail,omitempty"`
	CreatedAt     time.Time          `bson:"createdAt" json:"at"`
	SimAt         time.Time          `bson:"simAt" json:"simAt"`
}

// AppendEvent inserts one reservationEvents document (the durable source of truth
// a reconnecting client replays from) and returns it with its generated _id. This
// insert is deliberately best-effort from the caller's point of view — losing one
// costs an activity-feed line, never correctness, because every state transition
// that matters is already durable via its own guarded update or transaction before
// this is ever called.
func (s *Store) AppendEvent(ctx context.Context, ev Event) (Event, error) {
	ev.CreatedAt = time.Now().UTC()
	res, err := s.Coll(CollReservationEvents).InsertOne(ctx, ev)
	if err != nil {
		return ev, err
	}
	ev.ID = res.InsertedID.(primitive.ObjectID)
	return ev, nil
}

// Heartbeat upserts one agents/<name> document — the same shape as trafficsim's
// ts:agent:<id> hash, minus the TTL (Mongo has no key-level TTL on a whole
// document update cheaply enough for a 1s-tick agent; staleness is instead judged
// by comparing LastActivity against "now" when the snapshot is built).
func (s *Store) Heartbeat(ctx context.Context, name, kind, status string, events, errs int64) {
	_, _ = s.Coll(CollAgents).UpdateOne(ctx,
		bson.M{"_id": name},
		bson.M{"$set": bson.M{
			"type": kind, "status": status,
			"lastActivity": time.Now().UTC(),
			"events":       events, "errors": errs,
		}},
		options.Update().SetUpsert(true),
	)
}

// QuerySample is one recorded operation for the query-education panel (§19.7) —
// stored in the capped querySamples collection, natural (insertion) order.
type QuerySample struct {
	ID            primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	At            time.Time          `bson:"at" json:"at"`
	Agent         string             `bson:"agent" json:"agent"`
	Collection    string             `bson:"collection" json:"collection"`
	Op            string             `bson:"op" json:"op"`
	FilterSummary string             `bson:"filterSummary" json:"filterSummary"`
	Targeted      bool               `bson:"targeted" json:"targeted"`
	ShardsTouched int                `bson:"shardsTouched" json:"shardsTouched"`
	Verified      bool               `bson:"verified" json:"verified"`
	DurationMs    float64            `bson:"durationMs" json:"durationMs"`
	PlanStage     string             `bson:"planStage,omitempty" json:"planStage,omitempty"`
	DocsExamined  int64              `bson:"docsExamined,omitempty" json:"docsExamined,omitempty"`
	NReturned     int64              `bson:"nReturned,omitempty" json:"nReturned,omitempty"`
	Reason        string             `bson:"reason" json:"reason"`
}

func (s *Store) RecordQuerySample(ctx context.Context, qs QuerySample) {
	qs.At = time.Now().UTC()
	_, _ = s.Coll(CollQuerySamples).InsertOne(ctx, qs)
}
