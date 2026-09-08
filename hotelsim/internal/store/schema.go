package store

import (
	"context"
	"errors"
	"fmt"
	"log"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ShardKeyDailyInventory / ShardKeyReservations / ShardKeyReservationEvents are the
// one shard-key strategy this app actually implements (of the four the spec
// discusses): a compound, ranged {hotelId, <time field>} key. hotelId alone would
// let this app's own popularity hotspot create one chunk that can never split;
// region alone gives only 4 possible values (guaranteed imbalance across more than
// 4 shards' worth of chunks); a hashed key would be evenly distributed but
// destroys the date-range locality a hotel's own availability queries rely on and
// blocks a unique index on the key. The other three strategies are explained,
// but not built, in the query-education panel.
var (
	ShardKeyDailyInventory    = bson.D{{Key: "hotelId", Value: 1}, {Key: "date", Value: 1}}
	ShardKeyReservations      = bson.D{{Key: "hotelId", Value: 1}, {Key: "checkInDate", Value: 1}}
	ShardKeyReservationEvents = bson.D{{Key: "hotelId", Value: 1}, {Key: "createdAt", Value: 1}}
)

// EnsureSchema creates every index this app needs (idempotent — CreateMany is a
// no-op for an index that already exists with the same spec) and, on a sharded
// deployment, enables sharding on the database and shards the three collections
// that need it. Called once at startup, not on every Reset (see Wipe) — a schema
// or shard-key change requires a container restart, not just a reset.
func EnsureSchema(ctx context.Context, s *Store, topo Topology) error {
	idx := map[string][]mongo.IndexModel{
		CollHotels: {
			{Keys: bson.D{{Key: "region", Value: 1}, {Key: "tier", Value: 1}}},
		},
		CollRoomTypes: {
			{Keys: bson.D{{Key: "hotelId", Value: 1}}},
		},
		CollDailyInventory: {
			{Keys: bson.D{{Key: "date", Value: 1}, {Key: "availableRooms", Value: 1}}},
		},
		CollReservations: {
			{Keys: bson.D{{Key: "status", Value: 1}, {Key: "checkInDate", Value: 1}}},
			{Keys: bson.D{{Key: "status", Value: 1}, {Key: "checkOutDate", Value: 1}}},
			{Keys: bson.D{{Key: "guestId", Value: 1}}},
			{Keys: bson.D{{Key: "requestId", Value: 1}}},
			{Keys: bson.D{{Key: "region", Value: 1}, {Key: "checkInDate", Value: 1}}},
		},
		CollReservationEvents: {
			{Keys: bson.D{{Key: "createdAt", Value: -1}}},
			// TTL: 24h — durable enough to reconstruct recent history, bounded
			// enough to never grow unbounded.
			{Keys: bson.D{{Key: "createdAt", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(86400)},
		},
		CollReservationRequests: {
			// TTL: 1h — the idempotency dedup window; the collection self-prunes.
			{Keys: bson.D{{Key: "createdAt", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(3600)},
		},
	}
	for coll, models := range idx {
		if _, err := s.Coll(coll).Indexes().CreateMany(ctx, models); err != nil {
			return fmt.Errorf("create indexes on %s: %w", coll, err)
		}
	}

	// querySamples: capped, not TTL'd (capped collections cannot carry a TTL
	// index) — created once; "already exists" (code 48, NamespaceExists) is fine.
	capOpts := options.CreateCollection().SetCapped(true).SetSizeInBytes(4 << 20).SetMaxDocuments(20000)
	if err := s.DB.CreateCollection(ctx, CollQuerySamples, capOpts); err != nil && !hasCode(err, 48) {
		return fmt.Errorf("create capped querySamples: %w", err)
	}

	if topo != TopologySharded {
		return nil
	}
	return ensureSharding(ctx, s)
}

// ensureSharding enables sharding on this app's database and shards the three
// collections that carry real per-hotel volume. Best-effort pre-splitting is
// attempted afterward so the shard-distribution panel has something meaningful to
// show quickly, but a failure there never blocks basic sharding setup — the
// balancer redistributes on its own over the following minutes regardless.
func ensureSharding(ctx context.Context, s *Store) error {
	admin := s.Client.Database("admin")
	dbName := s.DB.Name()

	if err := admin.RunCommand(ctx, bson.D{{Key: "enableSharding", Value: dbName}}).Err(); err != nil {
		// Modern MongoDB treats this as idempotent; older versions may complain
		// the database is already sharding-enabled. Either way, proceed — the
		// shardCollection calls below are the real signal if sharding truly
		// isn't available on this deployment.
		log.Printf("hotelsim: enableSharding %s: %v (continuing)", dbName, err)
	}

	toShard := []struct {
		coll string
		key  bson.D
	}{
		{CollDailyInventory, ShardKeyDailyInventory},
		{CollReservations, ShardKeyReservations},
		{CollReservationEvents, ShardKeyReservationEvents},
	}
	for _, t := range toShard {
		ns := dbName + "." + t.coll
		cmd := bson.D{{Key: "shardCollection", Value: ns}, {Key: "key", Value: t.key}}
		if err := admin.RunCommand(ctx, cmd).Err(); err != nil && !hasCode(err, 20) { // 20 = AlreadyInitialized
			return fmt.Errorf("shardCollection %s: %w", ns, err)
		}
	}

	preSplitBestEffort(ctx, admin, dbName)
	return nil
}

// preSplitBestEffort splits reservations/dailyInventory/reservationEvents into 3
// ranges at roughly H034/H067 and spreads them across the discovered shards, so
// hotels are distributed across shards from the first snapshot rather than only
// after the balancer's own migrations catch up. Every step is logged and
// tolerated on failure — this is a presentation nicety, not correctness.
func preSplitBestEffort(ctx context.Context, admin *mongo.Database, dbName string) {
	// The registry of shards lives in config.shards, not admin.shards — admin only
	// hosts the sharding *commands* (enableSharding/shardCollection/split/moveChunk).
	cur, err := admin.Client().Database("config").Collection("shards").Find(ctx, bson.D{})
	if err != nil {
		log.Printf("hotelsim: pre-split: listing shards: %v (skipping pre-split, balancer will distribute over time)", err)
		return
	}
	var shards []struct {
		ID string `bson:"_id"`
	}
	if err := cur.All(ctx, &shards); err != nil || len(shards) < 2 {
		log.Printf("hotelsim: pre-split: %d shard(s) found, skipping (need >=2)", len(shards))
		return
	}

	splitPoints := []string{"H034", "H067"}
	targets := []struct {
		coll string
		key  string
	}{
		{CollDailyInventory, "date"},
		{CollReservations, "checkInDate"},
		{CollReservationEvents, "createdAt"},
	}
	for _, t := range targets {
		ns := dbName + "." + t.coll
		for _, hotelID := range splitPoints {
			middle := bson.D{{Key: "hotelId", Value: hotelID}, {Key: t.key, Value: primitive.MinKey{}}}
			if err := admin.RunCommand(ctx, bson.D{{Key: "split", Value: ns}, {Key: "middle", Value: middle}}).Err(); err != nil {
				log.Printf("hotelsim: pre-split %s at %s: %v (non-fatal)", ns, hotelID, err)
			}
		}
		// Spread the resulting chunks round-robin across shards. moveChunk is
		// best-effort per chunk — a chunk already on its target shard, or a
		// concurrent balancer round, both surface as tolerable errors here.
		for i, hotelID := range splitPoints {
			find := bson.D{{Key: "hotelId", Value: hotelID}, {Key: t.key, Value: primitive.MinKey{}}}
			to := shards[(i+1)%len(shards)].ID
			cmd := bson.D{{Key: "moveChunk", Value: ns}, {Key: "find", Value: find}, {Key: "to", Value: to}}
			if err := admin.RunCommand(ctx, cmd).Err(); err != nil {
				log.Printf("hotelsim: pre-split moveChunk %s -> %s: %v (non-fatal)", ns, to, err)
			}
		}
	}
}

// Wipe deletes every document from this app's own collections — never admin,
// local, config, or a database this app doesn't own — leaving indexes, shard
// keys, and any pre-split chunk layout in place. Used by Engine.Reset.
func Wipe(ctx context.Context, s *Store) error {
	for _, coll := range AllCollections {
		if _, err := s.Coll(coll).DeleteMany(ctx, bson.D{}); err != nil {
			return fmt.Errorf("wipe %s: %w", coll, err)
		}
	}
	return nil
}

// hasCode reports whether err is a MongoDB command/write error carrying the given
// server error code.
func hasCode(err error, code int) bool {
	var ce mongo.CommandError
	if ok := asCommandError(err, &ce); ok {
		return int(ce.Code) == code
	}
	var we mongo.WriteException
	if errors.As(err, &we) {
		for _, e := range we.WriteErrors {
			if e.Code == code {
				return true
			}
		}
	}
	return false
}

func asCommandError(err error, target *mongo.CommandError) bool {
	return errors.As(err, target)
}
