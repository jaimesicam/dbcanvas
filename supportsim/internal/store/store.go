// Package store is the support desk's MongoDB: the connection, the two collections
// it works on, and the search indexes that make them searchable.
//
// Everything lives in one database (MONGO_DB, default "supportsim"), the same rule
// every DBCanvas simulator follows: a learner exploring the cluster in mongosh, or a
// lab, works on its own databases, and nothing here may touch theirs.
//
// Two kinds of index are created, and the difference between them is half of what
// the app teaches:
//
//   - ordinary B-tree indexes (createIndexes), which mongod builds and maintains
//     itself, inside the write;
//   - search indexes (createSearchIndexes), which mongod only *records*. mongot
//     builds them, from the change stream, a moment after the write — so a search
//     index has a status (PENDING, BUILDING, READY), a document becomes findable
//     slightly after it is inserted, and a cluster without mongot cannot create one
//     at all. The app detects which of those worlds it is in rather than assuming.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Collection names.
const (
	KB       = "kb_articles"
	Tickets  = "tickets"
	Counters = "counters"
)

// Search index names. The UI shows them, the pipelines name them.
const (
	KBVector      = "kb_vector"
	KBText        = "kb_text"
	TicketsVector = "tickets_vector"
	TicketsText   = "tickets_text"
)

// Dims is the embedding width every vector index is declared with.
const Dims = 384

// Store holds the connection and the app's database.
type Store struct {
	Client *mongo.Client
	DB     *mongo.Database
}

// Connect opens a client; callers follow with Ping.
func Connect(ctx context.Context, uri, db string) (*Store, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := mongo.Connect(cctx, options.Client().ApplyURI(uri).SetAppName("supportsim"))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Store{Client: c, DB: c.Database(db)}, nil
}

// Ping checks the server answers.
func (s *Store) Ping(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.Client.Ping(cctx, nil)
}

// C returns a collection.
func (s *Store) C(name string) *mongo.Collection { return s.DB.Collection(name) }

// ServerInfo is what the header shows about the cluster.
type ServerInfo struct {
	Version  string `json:"version"`
	Topology string `json:"topology"` // standalone | replicaset | sharded
	SetName  string `json:"setName,omitempty"`
	Shards   int    `json:"shards,omitempty"` // shards holding tickets (sharded only)
}

// Info reads the server version and the deployment shape.
func (s *Store) Info(ctx context.Context) ServerInfo {
	var out ServerInfo
	var bi bson.M
	if s.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&bi) == nil {
		out.Version, _ = bi["version"].(string)
	}
	var hello bson.M
	if s.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello) == nil {
		switch {
		case hello["msg"] == "isdbgrid":
			out.Topology = "sharded"
		case hello["setName"] != nil:
			out.Topology = "replicaset"
			out.SetName, _ = hello["setName"].(string)
		default:
			out.Topology = "standalone"
		}
	}
	if out.Topology == "sharded" {
		cur, err := s.Client.Database("config").Collection("chunks").Distinct(ctx, "shard", bson.M{})
		if err == nil {
			out.Shards = len(cur)
		}
		var cs bson.M
		if s.DB.RunCommand(ctx, bson.D{{Key: "collStats", Value: Tickets}}).Decode(&cs) == nil {
			if sh, ok := cs["shards"].(bson.M); ok {
				out.Shards = len(sh)
			}
		}
	}
	return out
}

// EnsureCollections creates the collections and their ordinary indexes. Search
// indexes need the collection to exist first, which is the other reason this runs
// before anything else.
func (s *Store) EnsureCollections(ctx context.Context) error {
	existing, err := s.DB.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, n := range existing {
		have[n] = true
	}
	for _, n := range []string{KB, Tickets, Counters} {
		if !have[n] {
			if err := s.DB.CreateCollection(ctx, n); err != nil && !isNamespaceExists(err) {
				return fmt.Errorf("create %s: %w", n, err)
			}
		}
	}
	// On a sharded cluster the tickets are spread over every shard (hashed _id), so a
	// $vectorSearch through mongos really does fan out to each shard's own mongot and
	// merge the answers — the thing worth seeing on a sharded deployment. Best-effort:
	// an unsharded collection still works, it just lives on one shard.
	if s.Info(ctx).Topology == "sharded" {
		admin := s.Client.Database("admin")
		admin.RunCommand(ctx, bson.D{{Key: "enableSharding", Value: s.DB.Name()}})
		admin.RunCommand(ctx, bson.D{{Key: "shardCollection", Value: s.DB.Name() + "." + Tickets}, {Key: "key", Value: bson.D{{Key: "_id", Value: "hashed"}}}})
	}
	_, err = s.C(Tickets).Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "createdAt", Value: -1}}},
		{Keys: bson.D{{Key: "createdAt", Value: -1}}},
		// The classic text index is the keyword engine of last resort: what the
		// Showdown falls back to when there is no mongot to run $search.
		{Keys: bson.D{{Key: "subject", Value: "text"}, {Key: "body", Value: "text"}}, Options: options.Index().SetName("tickets_classic_text")},
	})
	if err != nil {
		return fmt.Errorf("ticket indexes: %w", err)
	}
	_, err = s.C(KB).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "title", Value: "text"}, {Key: "summary", Value: "text"}, {Key: "steps", Value: "text"}},
		Options: options.Index().SetName("kb_classic_text"),
	})
	return err
}

func isNamespaceExists(err error) bool {
	var ce mongo.CommandError
	return errors.As(err, &ce) && ce.Code == 48
}

// NextTicketNumber hands out ticket numbers.
func (s *Store) NextTicketNumber(ctx context.Context) (int64, error) {
	var doc struct {
		Seq int64 `bson:"seq"`
	}
	err := s.C(Counters).FindOneAndUpdate(ctx, bson.M{"_id": "ticket"}, bson.M{"$inc": bson.M{"seq": 1}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&doc)
	return 1000 + doc.Seq, err
}

// ---------------------------------------------------------------- search indexes

// IndexDef is one search index this app declares.
type IndexDef struct {
	Collection string `json:"collection"`
	Name       string `json:"name"`
	Type       string `json:"type"` // vectorSearch | search
	Definition bson.D `json:"-"`
	// Shell is the definition as you would type it in mongosh, for the UI.
	Shell string `json:"shell"`
}

// Indexes are the four search indexes: a vector index and a full-text index on
// each collection. The vector indexes declare their filter fields — a $vectorSearch
// can only pre-filter on fields its index lists as type "filter", which is the
// mistake almost everyone makes once.
var Indexes = []IndexDef{
	{Collection: KB, Name: KBVector, Type: "vectorSearch",
		Definition: bson.D{{Key: "fields", Value: bson.A{
			bson.D{{Key: "type", Value: "vector"}, {Key: "path", Value: "embedding"}, {Key: "numDimensions", Value: Dims}, {Key: "similarity", Value: "cosine"}},
			bson.D{{Key: "type", Value: "filter"}, {Key: "path", Value: "team"}},
		}}},
		Shell: `db.kb_articles.createSearchIndex("kb_vector", "vectorSearch", {
  fields: [
    { type: "vector", path: "embedding", numDimensions: 384, similarity: "cosine" },
    { type: "filter", path: "team" }
  ]
})`},
	{Collection: KB, Name: KBText, Type: "search",
		Definition: bson.D{{Key: "mappings", Value: bson.D{{Key: "dynamic", Value: false}, {Key: "fields", Value: bson.D{
			{Key: "title", Value: bson.D{{Key: "type", Value: "string"}}},
			{Key: "summary", Value: bson.D{{Key: "type", Value: "string"}}},
			{Key: "steps", Value: bson.D{{Key: "type", Value: "string"}}},
		}}}}},
		Shell: `db.kb_articles.createSearchIndex("kb_text", {
  mappings: { dynamic: false, fields: {
    title: { type: "string" }, summary: { type: "string" }, steps: { type: "string" }
  } }
})`},
	{Collection: Tickets, Name: TicketsVector, Type: "vectorSearch",
		Definition: bson.D{{Key: "fields", Value: bson.A{
			bson.D{{Key: "type", Value: "vector"}, {Key: "path", Value: "embedding"}, {Key: "numDimensions", Value: Dims}, {Key: "similarity", Value: "cosine"}},
			bson.D{{Key: "type", Value: "filter"}, {Key: "path", Value: "status"}},
			bson.D{{Key: "type", Value: "filter"}, {Key: "path", Value: "team"}},
			bson.D{{Key: "type", Value: "filter"}, {Key: "path", Value: "plan"}},
			bson.D{{Key: "type", Value: "filter"}, {Key: "path", Value: "createdAt"}},
			bson.D{{Key: "type", Value: "filter"}, {Key: "path", Value: "customer"}},
		}}},
		Shell: `db.tickets.createSearchIndex("tickets_vector", "vectorSearch", {
  fields: [
    { type: "vector", path: "embedding", numDimensions: 384, similarity: "cosine" },
    { type: "filter", path: "status" },
    { type: "filter", path: "team" },
    { type: "filter", path: "plan" },
    { type: "filter", path: "createdAt" },
    { type: "filter", path: "customer" }
  ]
})`},
	{Collection: Tickets, Name: TicketsText, Type: "search",
		Definition: bson.D{{Key: "mappings", Value: bson.D{{Key: "dynamic", Value: false}, {Key: "fields", Value: bson.D{
			{Key: "subject", Value: bson.D{{Key: "type", Value: "string"}}},
			{Key: "body", Value: bson.D{{Key: "type", Value: "string"}}},
			{Key: "status", Value: bson.D{{Key: "type", Value: "token"}}},
		}}}}},
		Shell: `db.tickets.createSearchIndex("tickets_text", {
  mappings: { dynamic: false, fields: {
    subject: { type: "string" }, body: { type: "string" }, status: { type: "token" }
  } }
})`},
}

// IndexStatus is one search index as mongot reports it.
type IndexStatus struct {
	Collection string `json:"collection"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Status     string `json:"status"` // PENDING | BUILDING | READY | FAILED | DOES_NOT_EXIST | ...
	Queryable  bool   `json:"queryable"`
	Shell      string `json:"shell"`
	Error      string `json:"error,omitempty"`
}

// ErrNoSearch means the cluster cannot run search indexes at all — no mongot is
// configured. Everything else still works; search falls back to the app.
var ErrNoSearch = errors.New("search indexes are not available on this cluster (no mongot)")

// EnsureSearchIndexes creates any of the four that are missing. It returns
// ErrNoSearch when the server refuses search-index commands outright.
func (s *Store) EnsureSearchIndexes(ctx context.Context) error {
	st, err := s.SearchIndexStatus(ctx)
	if err != nil {
		return err
	}
	for i, def := range Indexes {
		if st[i].Status != "DOES_NOT_EXIST" {
			continue
		}
		model := mongo.SearchIndexModel{Definition: def.Definition, Options: options.SearchIndexes().SetName(def.Name).SetType(def.Type)}
		if _, err := s.C(def.Collection).SearchIndexes().CreateOne(ctx, model); err != nil {
			if noSearch(err) {
				return ErrNoSearch
			}
			return fmt.Errorf("create search index %s: %w", def.Name, err)
		}
	}
	return nil
}

// SearchIndexStatus lists the four indexes in Indexes order. A missing one reports
// DOES_NOT_EXIST; a cluster without mongot returns ErrNoSearch.
func (s *Store) SearchIndexStatus(ctx context.Context) ([]IndexStatus, error) {
	out := make([]IndexStatus, len(Indexes))
	for i, def := range Indexes {
		out[i] = IndexStatus{Collection: def.Collection, Name: def.Name, Type: def.Type, Status: "DOES_NOT_EXIST", Shell: def.Shell}
		cur, err := s.C(def.Collection).SearchIndexes().List(ctx, options.SearchIndexes().SetName(def.Name))
		if err != nil {
			if noSearch(err) {
				return nil, ErrNoSearch
			}
			out[i].Error = err.Error()
			continue
		}
		var docs []bson.M
		if err := cur.All(ctx, &docs); err != nil {
			if noSearch(err) {
				return nil, ErrNoSearch
			}
			out[i].Error = err.Error()
			continue
		}
		if len(docs) > 0 {
			d := docs[0]
			out[i].Status, _ = d["status"].(string)
			out[i].Queryable, _ = d["queryable"].(bool)
		}
	}
	return out, nil
}

// noSearch recognises the ways a server says "I have no search" — the wording
// differs between versions and between "not configured" and "not supported".
func noSearch(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	for _, s := range []string{"searchindexmanagementhostandport", "mongothost", "search index commands", "requires additional configuration",
		"not supported", "unrecognized pipeline stage", "atlas", "search is not enabled", "command not found", "no such command"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	var ce mongo.CommandError
	if errors.As(err, &ce) && (ce.Code == 31082 || ce.Code == 59 || ce.Code == 40324 || ce.Code == 115) {
		return true
	}
	return false
}

// IsNoSearch exposes the classification for the search layer.
func IsNoSearch(err error) bool { return errors.Is(err, ErrNoSearch) || noSearch(err) }

// Aggregate runs a pipeline and times it.
func (s *Store) Aggregate(ctx context.Context, coll string, pipeline bson.A) ([]bson.M, time.Duration, error) {
	t0 := time.Now()
	cur, err := s.C(coll).Aggregate(ctx, pipeline)
	if err != nil {
		return nil, time.Since(t0), err
	}
	var out []bson.M
	err = cur.All(ctx, &out)
	return out, time.Since(t0), err
}

// ---------------------------------------------------------------- workshop helpers

// Variants is the collection the Index Workshop builds its comparison indexes on — a
// copy of the resolved tickets, so experiments never touch the desk's own indexes.
const Variants = "vector_variants"

// SearchIndexes lists a collection's search indexes as mongot reports them, with the
// fields the workshop shows: id (what mongot's metrics are labelled with), status,
// queryable, latestDefinition, statusDetail.
func (s *Store) SearchIndexes(ctx context.Context, coll string) ([]bson.M, error) {
	cur, err := s.C(coll).Aggregate(ctx, bson.A{bson.D{{Key: "$listSearchIndexes", Value: bson.D{}}}})
	if err != nil {
		return nil, err
	}
	var out []bson.M
	err = cur.All(ctx, &out)
	return out, err
}

// CreateSearchIndex creates one search index; typ is "vectorSearch" or "search".
func (s *Store) CreateSearchIndex(ctx context.Context, coll, name, typ string, def bson.D) error {
	_, err := s.C(coll).SearchIndexes().CreateOne(ctx, mongo.SearchIndexModel{Definition: def, Options: options.SearchIndexes().SetName(name).SetType(typ)})
	return err
}

// UpdateSearchIndex replaces an index's definition. mongot builds the new definition
// alongside the old one and keeps answering from the old until the new is ready.
func (s *Store) UpdateSearchIndex(ctx context.Context, coll, name string, def bson.D) error {
	return s.C(coll).SearchIndexes().UpdateOne(ctx, name, def)
}

// DropSearchIndex drops one.
func (s *Store) DropSearchIndex(ctx context.Context, coll, name string) error {
	return s.C(coll).SearchIndexes().DropOne(ctx, name)
}

// Explain runs an aggregation under explain("executionStats"). For $vectorSearch the
// interesting part is mongot's own: per-segment HNSW statistics, visited documents,
// timings.
func (s *Store) Explain(ctx context.Context, coll string, pipeline bson.A) (bson.M, error) {
	var out bson.M
	err := s.DB.RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{{Key: "aggregate", Value: coll}, {Key: "pipeline", Value: pipeline}, {Key: "cursor", Value: bson.D{}}}},
		{Key: "verbosity", Value: "executionStats"},
	}).Decode(&out)
	return out, err
}

// MongotHost asks the server which mongot it forwards searches to (its mongotHost
// parameter, host:27028). Through mongos this is the one mongos itself uses.
func (s *Store) MongotHost(ctx context.Context) string {
	var out bson.M
	if s.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "getParameter", Value: 1}, {Key: "mongotHost", Value: 1}}).Decode(&out) != nil {
		return ""
	}
	h, _ := out["mongotHost"].(string)
	return h
}
