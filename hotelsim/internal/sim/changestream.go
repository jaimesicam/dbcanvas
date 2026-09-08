package sim

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"hotelsim/internal/store"
)

// runEventFeed is the entry point Start launches: it picks the change-stream
// watcher when the topology supports it, else the universal poller — the same
// pattern that decides Booker at NewEngine, but for the push channel instead of
// the write path.
func (e *Engine) runEventFeed(ctx context.Context) {
	if e.Profile.ChangeStreams {
		e.runChangeStreamWatcher(ctx)
		return
	}
	e.runEventPoller(ctx)
}

// runChangeStreamWatcher tails reservationEvents (replica set + sharded only)
// and republishes every insert on the in-process EventBus. Falls back to the
// poller if it can never open a stream (e.g. change streams turn out to be
// unavailable despite the topology check) rather than leaving the feed dead.
func (e *Engine) runChangeStreamWatcher(ctx context.Context) {
	backoff := time.Second
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		token := e.loadResumeToken(ctx)
		csOpts := options.ChangeStream().SetMaxAwaitTime(2 * time.Second)
		if token != nil {
			csOpts.SetResumeAfter(token)
		}
		pipeline := mongo.Pipeline{{{Key: "$match", Value: bson.D{{Key: "operationType", Value: "insert"}}}}}
		cs, err := e.Store.Coll(store.CollReservationEvents).Watch(ctx, pipeline, csOpts)
		if err != nil {
			consecutiveFailures++
			if consecutiveFailures >= 3 {
				log.Printf("hotelsim: change stream unavailable after %d attempts (%v) — falling back to polling", consecutiveFailures, err)
				e.runEventPoller(ctx)
				return
			}
			e.Store.Heartbeat(ctx, "event-feed", "changestream", "error", 0, int64(consecutiveFailures))
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
			continue
		}
		consecutiveFailures = 0
		backoff = time.Second
		e.watchLoop(ctx, cs)
		cs.Close(ctx)
	}
}

// watchLoop drains one change stream until it errors or ctx is done. The resume
// token is captured after every batch — including empty ones — not just after
// an event, so an idle period (level=stop, or a paused engine) never lets the
// token go stale enough to fall off the oplog.
func (e *Engine) watchLoop(ctx context.Context, cs *mongo.ChangeStream) {
	var events int64
	lastPersist := time.Now()
	for cs.Next(ctx) {
		var raw bson.M
		if err := cs.Decode(&raw); err == nil {
			if full, ok := raw["fullDocument"].(bson.M); ok {
				if payload, err := json.Marshal(full); err == nil {
					e.Bus.Publish(payload)
					events++
				}
			}
		}
		if time.Since(lastPersist) > 5*time.Second {
			e.persistResumeToken(ctx, cs.ResumeToken())
			lastPersist = time.Now()
			e.Store.Heartbeat(ctx, "event-feed", "changestream", "ok", events, 0)
		}
		if err := ctx.Err(); err != nil {
			return
		}
	}
	if err := cs.Err(); err != nil {
		if isResumableGap(err) {
			log.Printf("hotelsim: change stream gap (%v) — discarding resume token, restarting from now", err)
			e.clearResumeToken(ctx)
			e.PublishEvent(ctx, "stream_gap", "", "", "", "event-feed", "live feed resumed — some events may have been missed; the dashboard is still authoritative")
		} else {
			log.Printf("hotelsim: change stream error: %v", err)
		}
	}
	e.persistResumeToken(ctx, cs.ResumeToken())
}

// isResumableGap reports whether err is a change-stream failure that can only
// be recovered from by dropping the resume token and restarting fresh —
// ChangeStreamHistoryLost (286) or a stream invalidate/fatal condition (280) —
// as opposed to a transient network error, which the driver already retries
// internally without any help from this code.
func isResumableGap(err error) bool {
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return ce.Code == 286 || ce.Code == 280
	}
	return false
}

type resumeTokenDoc struct {
	ID    string    `bson:"_id"`
	Token bson.Raw  `bson:"token"`
	At    time.Time `bson:"at"`
}

func (e *Engine) loadResumeToken(ctx context.Context) bson.Raw {
	var doc resumeTokenDoc
	if err := e.Store.Coll(store.CollSimState).FindOne(ctx, bson.M{"_id": "eventToken"}).Decode(&doc); err != nil {
		return nil
	}
	return doc.Token
}

func (e *Engine) persistResumeToken(ctx context.Context, token bson.Raw) {
	if token == nil {
		return
	}
	_, err := e.Store.Coll(store.CollSimState).UpdateOne(ctx,
		bson.M{"_id": "eventToken"},
		bson.M{"$set": resumeTokenDoc{ID: "eventToken", Token: token, At: time.Now().UTC()}},
		options.Update().SetUpsert(true))
	if err != nil {
		log.Printf("hotelsim: persist resume token: %v", err)
	}
}

func (e *Engine) clearResumeToken(ctx context.Context) {
	e.Store.Coll(store.CollSimState).DeleteOne(ctx, bson.M{"_id": "eventToken"})
}

// runEventPoller tails reservationEvents by ObjectId order — works on all three
// topologies, and is the only path on standalone (spec §7.1: "no change streams
// required in standalone mode, may use polling"). Relies on ObjectId
// monotonicity, which holds because this process is the only writer to
// reservationEvents; a second sim instance pointed at the same database would
// break this ordering assumption.
func (e *Engine) runEventPoller(ctx context.Context) {
	var lastID primitive.ObjectID
	var events int64
	tickLoop(ctx, time.Second, func() {
		filter := bson.M{}
		if !lastID.IsZero() {
			filter["_id"] = bson.M{"$gt": lastID}
		}
		cur, err := e.Store.Coll(store.CollReservationEvents).Find(ctx, filter,
			options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(200))
		if err != nil {
			e.Store.Heartbeat(ctx, "event-feed", "poller", "error", events, 1)
			return
		}
		defer cur.Close(ctx)
		var batch []store.Event
		if err := cur.All(ctx, &batch); err != nil || len(batch) == 0 {
			e.Store.Heartbeat(ctx, "event-feed", "poller", "ok", events, 0)
			return
		}
		for _, ev := range batch {
			if payload, err := json.Marshal(ev); err == nil {
				e.Bus.Publish(payload)
				events++
			}
			lastID = ev.ID
		}
		e.Store.Heartbeat(ctx, "event-feed", "poller", "ok", events, 0)
	})
}
