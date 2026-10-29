package mongo

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"

	"perfsim/internal/core"
)

// The stressors beyond replication lag: connections, write conflicts, commit
// rate and memory. They act on the primary, through the sim's own
// collections, and each one stops what it started when it is turned off.

const (
	groupConns = "Connections"
	groupLocks = "Locks"
	groupIO    = "I/O and CPU"
	groupMem   = "Memory"
)

const hotDocs = 16

func (e *Engine) loadDials() []core.Dial {
	e.mu.Lock()
	replset := e.topology == "replset" || e.topology == "sharded"
	e.mu.Unlock()
	hot := core.Dial{ID: "hotrow", Group: groupLocks, Name: "Hot-document write conflicts", Kind: "toggle", Available: replset,
		Desc:  "Workers update the same few documents in short multi-document transactions. MongoDB does not queue a second writer: it aborts its transaction with a WriteConflict, and the worker retries, so the work done climbs while the work committed does not.",
		Watch: []string{"p99 latency", "Write QPS"},
		Params: []core.Param{
			{Key: "workers", Label: "Workers", Type: "steps", Opts: []any{4, 16, 32, 64}, Def: 16},
			{Key: "rows", Label: "Documents they share", Type: "pills", Opts: []any{1, 4, 16}, Def: 4},
		},
		SQL: "session.startTransaction()\ndb.hot.updateOne({ _id: a }, { $inc: { n: 1 } })  // then 20 ms\ndb.hot.updateOne({ _id: b }, { $inc: { n: 1 } })\nsession.commitTransaction()  // retried on WriteConflict"}
	if !replset {
		hot.Why = "Multi-document transactions need a replica set or a sharded cluster: link one to use this."
	}
	return []core.Dial{
		{ID: "storm", Group: groupConns, Name: "Connection storm", Kind: "toggle", Available: true,
			Desc:  "Opens connections to the primary and holds them idle, up to the number chosen. Each one is a thread and memory on the server; past net.maxIncomingConnections, or the process's open-file limit, it refuses new ones.",
			Watch: []string{"Connections", "Errors"},
			Params: []core.Param{{Key: "conns", Label: "Connections", Type: "steps", Opts: []any{100, 250, 500, 1000, 2000}, Def: 250},
				{Key: "hold", Label: "Maximum hold, then release", Type: "pills", Opts: []any{30, 60, 120, 300}, Def: 120, Unit: "s"}},
			SQL: "// {conns} at once, then idle in the pool:\ndb.storm.find().tailable({ awaitData: true }).maxAwaitTimeMS(3000)  // a capped collection"},
		hot,
		{ID: "commits", Group: groupIO, Name: "Commit storm", Kind: "toggle", Available: true,
			Desc:   "Many single-document writes from many workers, each acknowledged only once it is journaled on a majority. Every write waits for a journal flush, and on a replica set for the secondaries too.",
			Watch:  []string{"Write QPS", "p99 latency"},
			Params: []core.Param{{Key: "workers", Label: "Workers", Type: "steps", Opts: []any{8, 32, 64, 128}, Def: 32}},
			SQL:    "db.commits.updateOne({ _id: n }, { $inc: { n: 1 } }, { upsert: true,\n  writeConcern: { w: \"majority\", j: true } })  // from every worker"},
		{ID: "scan", Group: groupMem, Name: "Working set larger than memory", Kind: "toggle", NeedsTable: "pk", Available: true,
			Desc:  "Random point reads across a whole prepared collection. When it is larger than the WiredTiger cache, most of them miss it and read the page from disk, and pages the workload needs are evicted.",
			Watch: []string{"Pages read into cache", "p99 latency"},
			Params: []core.Param{{Key: "workers", Label: "Readers", Type: "steps", Opts: []any{4, 16, 32}, Def: 16},
				{Key: "share", Label: "Working set", Type: "pills", Opts: []any{10, 25, 50, 100}, Def: 100, Unit: "%"}},
			Note: "Prepare a collection bigger than the WiredTiger cache for the effect; the dial says how they compare when it starts. A page read from disk may still come from the operating system's page cache.",
			SQL:  "db.{table}.findOne({ _id: n })  // a random _id each time"},
	}
}

func (e *Engine) startLoad(ctx context.Context, id string, p core.Params, table string) (core.Running, bool) {
	var run func(context.Context, *core.Job)
	switch id {
	case "storm":
		run = e.runStorm
	case "hotrow":
		run = e.runHotDocs
	case "commits":
		run = e.runCommits
	case "scan":
		run = func(ctx context.Context, j *core.Job) { e.runScan(ctx, j, table) }
	default:
		return nil, false
	}
	return core.NewJob(ctx, p, run), true
}

// runStorm keeps a client to the primary (mongos on a sharded cluster) with a
// pool as large as the number chosen, and fills it by checking that many
// connections out at once: each waits in a tailable cursor's getMore, which the
// server holds for a few seconds. Afterwards they stay open, idle in the pool.
// Disconnecting is the whole release.
func (e *Engine) runStorm(ctx context.Context, j *core.Job) {
	want := j.Param().Int("conns", 250)
	e.mu.Lock()
	wid, sharded := e.writerID, e.topology == "sharded"
	e.mu.Unlock()
	var host string
	var port int
	for _, m := range e.cfg.Members {
		if m.ID == wid {
			host, port = m.Host, m.Port
		}
	}
	u := fmt.Sprintf("mongodb://%s:%s@%s:%d/?authSource=admin&directConnection=true",
		url.QueryEscape(e.cfg.User), url.QueryEscape(e.cfg.Password), host, port)
	if sharded {
		// The application's connections go to mongos.
		u, wid = e.cfg.URI, "mongos"
	} else if host == "" {
		j.Finish(fmt.Errorf("no primary to connect to"))
		return
	}
	c, err := mongo.Connect(ctx, options.Client().ApplyURI(u).SetAppName("perfsim-storm").
		SetMaxPoolSize(uint64(want)).SetMaxConnecting(64).SetMaxConnIdleTime(0))
	if err != nil {
		j.Finish(err)
		return
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c.Disconnect(dctx)
		cancel()
	}()
	admin := c.Database("admin")
	var lastErr atomic.Value
	// A tailable cursor on a capped collection: its getMore waits for new
	// documents up to maxAwaitTime, and holds its connection meanwhile, on a
	// mongod and through mongos alike (mongos answers an awaitable hello at
	// once, so that cannot hold one).
	db := c.Database(database)
	db.CreateCollection(ctx, "storm", options.CreateCollection().SetCapped(true).SetSizeInBytes(4096))
	db.Collection("storm").InsertOne(ctx, bson.D{{Key: "n", Value: 0}})
	fill := func() {
		var wg sync.WaitGroup
		for i := 0; i < want; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cur, err := db.Collection("storm").Find(ctx, bson.D{},
					options.Find().SetCursorType(options.TailableAwait).SetMaxAwaitTime(3*time.Second))
				if err != nil {
					if ctx.Err() == nil {
						lastErr.Store(err.Error())
					}
					return
				}
				defer cur.Close(context.Background())
				cur.TryNext(ctx) // the document already there
				cur.TryNext(ctx) // one getMore, which waits up to maxAwaitTime
			}()
		}
		wg.Wait()
	}
	for n := 0; ctx.Err() == nil; n++ {
		// Again every half minute, in case the server or a network blip closed some.
		if n%15 == 0 {
			j.Set(fmt.Sprintf("Opening %d connections to %s", want, wid), -1, "")
			fill()
		}
		var ss struct {
			Connections struct {
				Current float64 `bson:"current"`
			} `bson:"connections"`
		}
		admin.RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&ss)
		note := fmt.Sprintf("the primary has %.0f connections in all", ss.Connections.Current)
		if sharded {
			// One address may front several mongos (a Kubernetes Service does),
			// which share the storm; the one that answered reports its own.
			note = fmt.Sprintf("the mongos that answered has %.0f connections in all; with several mongos behind one address, the storm is spread across them", ss.Connections.Current)
		}
		if v, _ := lastErr.Load().(string); v != "" {
			note += " · last error: " + short(v, 80)
		}
		j.Set(fmt.Sprintf("Holding a pool of %d connections to %s", want, wid), -1, note)
		sleep(ctx, 2*time.Second)
	}
}

func (e *Engine) runHotDocs(ctx context.Context, j *core.Job) {
	hot := e.db().Collection("hot")
	for i := 1; i <= hotDocs; i++ {
		hot.UpdateOne(ctx, bson.D{{Key: "_id", Value: i}}, bson.D{{Key: "$setOnInsert", Value: bson.D{{Key: "n", Value: 0}}}}, options.Update().SetUpsert(true))
	}
	var commits, conflicts atomic.Int64
	var wg sync.WaitGroup
	workers := j.Param().Int("workers", 16)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for ctx.Err() == nil {
				rows := j.Param().Int("rows", 4)
				a := rand.IntN(rows) + 1
				ids := []int{a}
				if rows > 1 {
					b := a%rows + 1
					if w%2 == 1 {
						a, b = b, a
					}
					ids = []int{a, b}
				}
				err := e.hotTxn(ctx, hot, ids)
				var se mongo.ServerError
				switch {
				case err == nil:
					commits.Add(1)
				case errors.As(err, &se) && (se.HasErrorLabel("TransientTransactionError") || se.HasErrorCode(112)):
					conflicts.Add(1)
				default:
					sleep(ctx, 200*time.Millisecond)
				}
			}
		}(w)
	}
	go func() {
		for ctx.Err() == nil {
			j.Set(fmt.Sprintf("%d workers on %d documents", workers, j.Param().Int("rows", 4)), -1,
				fmt.Sprintf("%d commits · %d write conflicts", commits.Load(), conflicts.Load()))
			sleep(ctx, time.Second)
		}
	}()
	wg.Wait()
}

func (e *Engine) hotTxn(ctx context.Context, hot *mongo.Collection, ids []int) error {
	s, err := e.wr().StartSession()
	if err != nil {
		return err
	}
	defer s.EndSession(context.Background())
	if err := s.StartTransaction(); err != nil {
		return err
	}
	err = mongo.WithSession(ctx, s, func(sc mongo.SessionContext) error {
		for _, id := range ids {
			if _, err := hot.UpdateOne(sc, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "n", Value: 1}}}}); err != nil {
				return err
			}
			sleep(ctx, 20*time.Millisecond)
		}
		return s.CommitTransaction(sc)
	})
	if err != nil {
		actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.AbortTransaction(actx)
		cancel()
	}
	return err
}

func (e *Engine) runCommits(ctx context.Context, j *core.Job) {
	journal := true
	commits := e.db().Collection("commits", options.Collection().SetWriteConcern(&writeconcern.WriteConcern{W: "majority", Journal: &journal}))
	var n atomic.Int64
	var wg sync.WaitGroup
	workers := j.Param().Int("workers", 32)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_, err := commits.UpdateOne(ctx, bson.D{{Key: "_id", Value: rand.IntN(10000)}},
					bson.D{{Key: "$inc", Value: bson.D{{Key: "n", Value: 1}}}}, options.Update().SetUpsert(true))
				if err == nil {
					n.Add(1)
				} else {
					sleep(ctx, 200*time.Millisecond)
				}
			}
		}()
	}
	go j.RateNote(ctx, fmt.Sprintf("%d workers, w: majority, j: true", workers), &n, "writes/s")
	wg.Wait()
}

func (e *Engine) runScan(ctx context.Context, j *core.Job, table string) {
	c := e.db().Collection(table)
	var last struct {
		ID int64 `bson:"_id"`
	}
	if err := c.FindOne(ctx, bson.D{}, options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}})).Decode(&last); err != nil || last.ID < 1 {
		j.Finish(fmt.Errorf("%s is empty or unreadable", table))
		return
	}
	var size int64
	if ds, err := e.Datasets(ctx); err == nil {
		for _, d := range ds {
			if d.Name == table {
				size = d.Bytes
			}
		}
	}
	var ss bson.M
	// The cache is a mongod's: on a sharded cluster, mongos has none, so a
	// shard primary's (the collection lives on the database's primary shard).
	status := e.wr()
	e.mu.Lock()
	if c := e.mc[e.writerID]; c != nil && e.topology == "sharded" {
		status = c
	}
	e.mu.Unlock()
	status.Database("admin").RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&ss)
	cache := cacheStat(ss, "maximum bytes configured")
	compare := fmt.Sprintf("collection %s, WiredTiger cache %s", fmtBytes(float64(size)), fmtBytes(cache))
	if size > 0 && float64(size) < cache {
		compare += ": the collection fits, so reads will soon all hit the cache"
	}
	var n atomic.Int64
	var wg sync.WaitGroup
	workers := j.Param().Int("workers", 16)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				var doc bson.Raw
				err := c.FindOne(ctx, bson.D{{Key: "_id", Value: 1 + rand.Int64N(max(1, last.ID*int64(j.Param().Int("share", 100))/100))}}).Decode(&doc)
				if err == nil || errors.Is(err, mongo.ErrNoDocuments) {
					n.Add(1)
				} else {
					sleep(ctx, 200*time.Millisecond)
				}
			}
		}()
	}
	go j.RateNote(ctx, fmt.Sprintf("%d readers across %s", workers, table), &n, "reads/s · "+compare)
	wg.Wait()
}

// cacheStat is one of serverStatus's wiredTiger.cache counters.
func cacheStat(ss bson.M, name string) float64 {
	wt, _ := ss["wiredTiger"].(bson.M)
	cache, _ := wt["cache"].(bson.M)
	return num(cache[name], 0)
}

func fmtBytes(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", b/(1<<20))
	}
	return fmt.Sprintf("%.0f kB", b/(1<<10))
}
