// Package store wraps the MongoDB connection, collection naming, and the
// namespace-isolation rule.
//
// Every collection this app touches lives in exactly one database (MONGO_DB,
// default "hotelsim") — this is a hard rule, not a convention: dbcanvas labs and
// any learner poking at the same deployment with mongosh operate on their own
// databases/collections, and this app's own continuous simulation must never be
// mistaken for, or interfere with, that work. Reset() only ever deletes documents
// from this app's own collections inside this one database — it never touches
// admin/local/config or any other database.
package store

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// Topology is the MongoDB deployment shape this app is connected to, detected at
// startup (see DetectTopology) and never user-configurable — the whole point of
// this app is to demonstrate that the SAME simulation behaves differently
// depending on what it actually finds itself talking to.
type Topology string

const (
	TopologyStandalone Topology = "standalone"
	TopologyReplicaSet Topology = "replicaset"
	TopologySharded    Topology = "sharded"
	TopologyUnknown    Topology = "unknown"
)

// connectTimeout bounds the initial handshake — an unreachable deployment should
// fail fast, not stall the process.
const connectTimeout = 15 * time.Second

// Store holds the shared MongoDB connection and the app's own database handle.
type Store struct {
	Client *mongo.Client
	DB     *mongo.Database
}

// Connect opens a *mongo.Client against uri and returns a Store scoped to dbName.
// Does not block on network reachability beyond the driver's own handshake —
// callers should follow with Ping (see waitForMongo in main.go).
func Connect(ctx context.Context, uri, dbName string) (*Store, error) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	client, err := mongo.Connect(cctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Store{Client: client, DB: client.Database(dbName)}, nil
}

func (s *Store) Ping(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.Client.Ping(cctx, nil)
}

// DetectTopology runs the `hello` admin command (the current name for what older
// deployments call isMaster) and classifies the deployment. Only a mongos answers
// with msg:"isdbgrid"; only a replica-set member sets setName. Fails loudly rather
// than guessing if the other end turns out to be a config server or (on a sharded
// cluster) a shard member reached directly — dbcanvas's own tooling always routes
// through the mongos for exactly this reason (see app/datagen.go's psmdb guard),
// and this app enforces the same rule for itself at connect time.
func (s *Store) DetectTopology(ctx context.Context) (Topology, error) {
	var hello bson.M
	if err := s.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		return TopologyUnknown, fmt.Errorf("hello: %w", err)
	}
	if v, _ := hello["configsvr"].(int32); v == 2 {
		return TopologyUnknown, fmt.Errorf("connected to a config server directly — connect through the mongos router instead")
	}
	if msg, _ := hello["msg"].(string); msg == "isdbgrid" {
		return TopologySharded, nil
	}
	if setName, _ := hello["setName"].(string); setName != "" {
		return TopologyReplicaSet, nil
	}
	// A shard member reached directly still looks like an ordinary replica-set
	// member to `hello` (it has a setName) — the sharding-specific give-away is
	// serverStatus().sharding, present only on nodes that know they're a shard.
	var status bson.M
	if err := s.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&status); err == nil {
		if _, ok := status["sharding"]; ok {
			if setName, _ := hello["setName"].(string); setName != "" {
				return TopologyUnknown, fmt.Errorf("connected to a shard member directly — connect through the mongos router instead")
			}
		}
	}
	return TopologyStandalone, nil
}

// ServerVersion returns the deployment's reported MongoDB version (buildInfo.version).
func (s *Store) ServerVersion(ctx context.Context) string {
	var info bson.M
	if err := s.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
		return ""
	}
	v, _ := info["version"].(string)
	return v
}

// Collection name constants — the complete set of collections this app owns.
const (
	CollHotels              = "hotels"
	CollRoomTypes           = "roomTypes"
	CollDailyInventory      = "dailyInventory"
	CollReservations        = "reservations"
	CollReservationEvents   = "reservationEvents"
	CollReservationRequests = "reservationRequests"
	CollAgents              = "agents"
	CollMetrics             = "metrics"
	CollSimState            = "simstate"
	CollQuerySamples        = "querySamples"
)

// AllCollections lists every collection this app owns, for Reset's wipe step and
// for the topology panel's dbStats/collection listing.
var AllCollections = []string{
	CollHotels, CollRoomTypes, CollDailyInventory, CollReservations,
	CollReservationEvents, CollReservationRequests, CollAgents, CollMetrics,
	CollSimState, CollQuerySamples,
}

func (s *Store) Coll(name string) *mongo.Collection { return s.DB.Collection(name) }

// CollWithReadPref returns a handle to the named collection cloned with a
// specific read preference — the v1 driver has no per-call read-preference
// option on Aggregate/Find themselves, so a read that should prefer a
// secondary (the replica-set/sharded analytics profile) goes through a cloned
// collection instead. Falls back to the plain collection if rp is nil or the
// clone fails (Clone only errors on invalid option combinations, never on
// topology state).
func (s *Store) CollWithReadPref(name string, rp *readpref.ReadPref) *mongo.Collection {
	if rp == nil {
		return s.Coll(name)
	}
	c, err := s.Coll(name).Clone(options.Collection().SetReadPreference(rp))
	if err != nil {
		return s.Coll(name)
	}
	return c
}
