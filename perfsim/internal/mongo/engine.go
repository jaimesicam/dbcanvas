// Package mongo is Performance Sim's MongoDB engine: a standalone server or a
// replica set (Percona Server for MongoDB or MongoDB Community).
package mongo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"

	"perfsim/internal/core"
)

// Config is what DBCanvas resolved: the application's connection string (the
// whole replica set, or the one server), and every member for the stressors.
type Config struct {
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	User     string         `json:"user"`
	Password string         `json:"password"`
	URI      string         `json:"uri"`
	Members  []MemberConfig `json:"members"`
}

type MemberConfig struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Port int    `json:"port"`
	Role string `json:"role"`
}

const database = "perfsim"
const ordersDocs = 100000

type Engine struct {
	cfg    Config
	writer atomic.Pointer[mongo.Client] // the application's: default write concern (majority on a replica set)
	hb     atomic.Pointer[mongo.Client] // heartbeat: w:1, so a stalled majority does not stop the clock
	// direct is the member the clients point at when the connection string
	// names one member directly (a Kubernetes replica set reached through one
	// pod's address); "" otherwise. See followPrimary.
	direct string
	mc     map[string]*mongo.Client

	mu       sync.Mutex
	flavor   string
	version  string
	topology string // standalone | replset | sharded
	writerID string
	lag      map[string]float64
	setName  string
	// readsPrev is each member's pages read into the cache and the time at the
	// last sample.
	readsPrev map[string][2]float64
	// memberSet is each member's replica set name and setPrimary each set's
	// primary, as last sampled; on a sharded cluster there is one set per shard.
	memberSet  map[string]string
	setPrimary map[string]string
	// hbIdx is the heartbeat document each shard owns (see shard.go).
	hbIdx map[string]int
}

func New(cfg Config) *Engine {
	e := &Engine{cfg: cfg, mc: map[string]*mongo.Client{}, lag: map[string]float64{}, readsPrev: map[string][2]float64{},
		memberSet: map[string]string{}, setPrimary: map[string]string{}, hbIdx: map[string]int{}}
	w, hb := connectWriter(cfg.URI)
	e.writer.Store(w)
	e.hb.Store(hb)
	e.direct = directMember(cfg)
	for _, m := range cfg.Members {
		u := fmt.Sprintf("mongodb://%s:%s@%s:%d/?authSource=admin&directConnection=true",
			url.QueryEscape(cfg.User), url.QueryEscape(cfg.Password), m.Host, m.Port)
		c, _ := mongo.Connect(context.Background(), options.Client().ApplyURI(u).
			SetMaxPoolSize(8).SetServerSelectionTimeout(3*time.Second).SetConnectTimeout(3*time.Second).SetAppName("perfsim"))
		e.mc[m.ID] = c
	}
	return e
}

func (e *Engine) wr() *mongo.Client  { return e.writer.Load() }
func (e *Engine) hbc() *mongo.Client { return e.hb.Load() }

// connectWriter opens the workload's client and the heartbeat's on a
// connection string.
func connectWriter(uri string) (w, hb *mongo.Client) {
	w, _ = mongo.Connect(context.Background(), options.Client().ApplyURI(uri).
		SetMaxPoolSize(80).SetServerSelectionTimeout(5*time.Second).SetAppName("perfsim"))
	hb, _ = mongo.Connect(context.Background(), options.Client().ApplyURI(uri).
		SetMaxPoolSize(2).SetServerSelectionTimeout(5*time.Second).SetAppName("perfsim").SetWriteConcern(writeconcern.W1()))
	return w, hb
}

// directMember is the member a directConnection=true connection string names,
// or "" for any other.
func directMember(cfg Config) string {
	u, err := url.Parse(cfg.URI)
	if err != nil || u.Query().Get("directConnection") != "true" {
		return ""
	}
	for _, m := range cfg.Members {
		if u.Host == fmt.Sprintf("%s:%d", m.Host, m.Port) {
			return m.ID
		}
	}
	return ""
}

// followPrimary points the workload and the heartbeat at a replica set's new
// primary when they reach it through one member directly, as on Kubernetes,
// where the members' own names do not resolve outside the cluster and the
// driver cannot follow a failover by itself. Through a replica set connection
// string or mongos the driver follows it, and nothing changes here.
func (e *Engine) followPrimary(primary string) {
	e.mu.Lock()
	cur := e.direct
	e.mu.Unlock()
	if cur == "" || primary == "" || primary == cur {
		return
	}
	var to MemberConfig
	for _, m := range e.cfg.Members {
		if m.ID == primary {
			to = m
		}
	}
	u, err := url.Parse(e.cfg.URI)
	if err != nil || to.ID == "" {
		return
	}
	u.Host = fmt.Sprintf("%s:%d", to.Host, to.Port)
	w, hb := connectWriter(u.String())
	oldW, oldH := e.writer.Swap(w), e.hb.Swap(hb)
	e.mu.Lock()
	e.direct = primary
	e.mu.Unlock()
	log.Printf("perfsim: %s is the primary now; the workload follows it", primary)
	// Statements in flight finish on the old clients first.
	go func() {
		time.Sleep(30 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		oldW.Disconnect(ctx)
		oldH.Disconnect(ctx)
	}()
}

func (e *Engine) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.wr().Disconnect(ctx)
	e.hbc().Disconnect(ctx)
	for _, c := range e.mc {
		c.Disconnect(ctx)
	}
}

func (e *Engine) db() *mongo.Database { return e.wr().Database(database) }

func (e *Engine) Init(ctx context.Context) error {
	var build bson.M
	if err := e.wr().Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&build); err != nil {
		return err
	}
	version, _ := build["version"].(string)
	flavor := "mongodb"
	if _, ok := build["psmdbVersion"]; ok || strings.Contains(fmt.Sprint(build["gitVersion"])+fmt.Sprint(build["modules"]), "percona") {
		flavor = "percona"
	}
	var hello bson.M
	e.wr().Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
	topo := "standalone"
	if hello["msg"] == "isdbgrid" {
		// mongos: a sharded cluster.
		topo = "sharded"
	} else if name, ok := hello["setName"].(string); ok && name != "" {
		topo = "replset"
		e.mu.Lock()
		e.setName = name
		e.mu.Unlock()
	}
	e.mu.Lock()
	e.flavor, e.version, e.topology = flavor, version, topo
	e.mu.Unlock()
	if topo == "sharded" {
		idx, err := e.setupShardedHeartbeat(ctx)
		if err != nil {
			return err
		}
		e.mu.Lock()
		e.hbIdx = idx
		e.mu.Unlock()
	}

	orders := e.db().Collection("orders")
	orders.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "customer", Value: 1}}})
	n, err := orders.EstimatedDocumentCount(ctx)
	if err != nil {
		return err
	}
	for base := int(n); base < ordersDocs; base += 10000 {
		docs := make([]any, 0, 10000)
		for i := base + 1; i <= base+10000 && i <= ordersDocs; i++ {
			docs = append(docs, bson.D{{Key: "_id", Value: i}, {Key: "customer", Value: i % 10000}, {Key: "amount", Value: float64(i%50000) / 100}, {Key: "status", Value: "new"}, {Key: "note", Value: 0}})
		}
		if _, err := orders.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false)); err != nil && !mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("seeding orders: %w", err)
		}
	}
	e.Sample(ctx)
	e.replayUndo(ctx)
	return e.Heartbeat(ctx)
}

func (e *Engine) Info() core.TargetInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return core.TargetInfo{Name: e.cfg.Name, Engine: "mongodb", Flavor: e.flavor, Version: e.version, Topology: e.topology, Writer: e.writerID,
		LagTitle: "Replication lag", LagSub: "how far each member's copy of the heartbeat document is behind the primary", Status: "Replica set status",
		DiskReadsLabel: "pages read into the WiredTiger cache/s"}
}

func (e *Engine) Heartbeat(ctx context.Context) error {
	ids := []int{1}
	e.mu.Lock()
	if e.topology == "sharded" {
		ids = ids[:0]
		for _, i := range e.hbIdx {
			ids = append(ids, i)
		}
	}
	e.mu.Unlock()
	now := time.Now()
	var err error
	for _, id := range ids {
		_, werr := e.hbc().Database(database).Collection("heartbeat").UpdateOne(ctx, bson.D{{Key: "_id", Value: id}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "ts", Value: now}}}}, options.Update().SetUpsert(true))
		if werr != nil && err == nil {
			err = werr
		}
	}
	return err
}

func (e *Engine) Op(ctx context.Context, write bool, n int) error {
	orders := e.db().Collection("orders")
	id := n%ordersDocs + 1
	if write {
		if n%2 == 0 {
			_, err := orders.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "amount", Value: 1}, {Key: "note", Value: 1}}}})
			return err
		}
		_, err := orders.ReplaceOne(ctx, bson.D{{Key: "_id", Value: id}},
			bson.D{{Key: "_id", Value: id}, {Key: "customer", Value: n % 10000}, {Key: "amount", Value: float64(n%50000) / 100}, {Key: "status", Value: "new"}, {Key: "note", Value: 0}},
			options.Replace().SetUpsert(true))
		return err
	}
	if n%10 == 0 {
		cur, err := orders.Find(ctx, bson.D{{Key: "customer", Value: n % 10000}}, options.Find().SetLimit(20))
		if err != nil {
			return err
		}
		defer cur.Close(ctx)
		for cur.Next(ctx) {
		}
		return cur.Err()
	}
	err := orders.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	return err
}

// --- saved state ---

func (e *Engine) Save(ctx context.Context, key string, v []byte) error {
	_, err := e.db().Collection("state").UpdateOne(ctx, bson.D{{Key: "_id", Value: key}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: string(v)}}}}, options.Update().SetUpsert(true))
	return err
}

func (e *Engine) Load(ctx context.Context, prefix string) (map[string][]byte, error) {
	cur, err := e.db().Collection("state").Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$regex", Value: "^" + regexQuote(prefix)}}}})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := map[string][]byte{}
	for cur.Next(ctx) {
		var d struct {
			ID string `bson:"_id"`
			V  string `bson:"v"`
		}
		if cur.Decode(&d) == nil {
			out[d.ID] = []byte(d.V)
		}
	}
	return out, cur.Err()
}

func (e *Engine) Delete(ctx context.Context, key string) error {
	_, err := e.db().Collection("state").DeleteOne(ctx, bson.D{{Key: "_id", Value: key}})
	return err
}

func regexQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (e *Engine) member(id string) *mongo.Client { return e.mc[id] }

func (e *Engine) lagOf(id string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v, ok := e.lag[id]; ok {
		return v
	}
	return -1
}

// replicaIDs are the members that are not their replica set's primary.
func (e *Engine) replicaIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	primary := map[string]bool{e.writerID: true}
	for _, p := range e.setPrimary {
		primary[p] = true
	}
	var out []string
	for _, m := range e.cfg.Members {
		if !primary[m.ID] {
			out = append(out, m.ID)
		}
	}
	return out
}

// memberByAddr maps a replica set member's "host:port" to the sim's id for it.
func (e *Engine) memberByAddr(addr string) string {
	host := strings.Split(addr, ":")[0]
	for _, m := range e.cfg.Members {
		// On Kubernetes the member's id is its pod name, the first label of the
		// in-cluster name the replica set reports, and its host an address.
		short := strings.Split(host, ".")[0]
		if m.Host == host || strings.Split(m.Host, ".")[0] == short || m.ID == short {
			return m.ID
		}
	}
	return ""
}

// Probe connects as the workload does and describes the server, for a
// connection test.
func (e *Engine) Probe(ctx context.Context) (string, error) {
	var build, hello bson.M
	if err := e.wr().Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&build); err != nil {
		return "", err
	}
	e.wr().Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
	what := "a standalone server"
	switch {
	case hello["msg"] == "isdbgrid":
		what = "mongos"
	case hello["setName"] != nil:
		what = fmt.Sprintf("replica set %v", hello["setName"])
	}
	return fmt.Sprintf("MongoDB %v, %s", build["version"], what), nil
}
