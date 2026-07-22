package main

// Benchmark — MongoDB backend. The SQL engines drive a normalized star schema over database/sql;
// MongoDB is a different shape and this path reflects that rather than imitating joins:
//
//   - Transport is the Go driver over the stack network (mongoClientFor, shared with the data
//     generator), not database/sql — there is no database/sql MongoDB driver, and the driver gives
//     us native documents and aggregation pipelines.
//   - The dataset is an *embedded document* model: an order carries its line items inside it, and
//     the few fields an analytic query needs from other collections (a product's category, a
//     customer's country/segment) are denormalized onto the order. That keeps every aggregation a
//     single-collection $unwind/$group — which is how you actually model this in MongoDB — instead
//     of a $lookup that would benchmark the wrong thing.
//   - Ops are single-document (no multi-document transactions): a Mongo OLTP unit is a burst of
//     find-by-_id / range-find / insert / update / delete, not a BEGIN…COMMIT.
//
// It reuses the SQL harness wholesale: the same benchRun lifecycle, latAcc buckets (recStmt),
// thread fan-out, warmup/measure windows, snapshot/DTO and stop/status. Only the target of the
// work changes.

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	benchMCust = "bench_customers"
	benchMProd = "bench_products"
	benchMOrd  = "bench_orders"
	benchMMeta = "bench_meta"
)

// mongoCtx carries the collection handles (and the CRUD plan) through the drive loop, the way the
// SQL path threads a *sql.DB.
type mongoCtx struct {
	cust, prod, ord *mongo.Collection
	crud            *mongoCrudPlan
}

// ------------------------------------------------------------------- lifecycle

func (run *benchRun) executeMongo(ctx context.Context) {
	conn := dbConn{ContainerID: run.nodeContainerID, Engine: "mongodb",
		Super: run.dbUser, Password: run.dbPass, StackID: run.cfg.StackID,
		eng: run.app.dialEngine(run.cfg.StackID, run.nodeContainerID)}
	client, closer, err := run.app.mongoClientFor(ctx, conn)
	if err != nil {
		run.fail(err.Error())
		return
	}
	defer closer()
	db := client.Database(run.cfg.Database)
	mc := &mongoCtx{cust: db.Collection(benchMCust), prod: db.Collection(benchMProd), ord: db.Collection(benchMOrd)}

	if run.cfg.Workload == "crud" {
		run.setPhase("preparing", "introspecting collection")
		plan, perr := run.buildMongoCRUDPlan(ctx, db)
		if perr != nil {
			run.fail(perr.Error())
			return
		}
		mc.crud = plan
		run.mongoRefreshSample(ctx, plan) // initial filter pool
		go run.mongoSampler(ctx, plan)
	} else if err := run.prepareMongo(ctx, mc); err != nil {
		run.fail(err.Error())
		return
	}
	if ctx.Err() != nil {
		return
	}

	if run.cfg.WarmupS > 0 {
		run.setPhase("warmup", "")
		run.driveMongo(ctx, mc, time.Duration(run.cfg.WarmupS)*time.Second, false)
		if ctx.Err() != nil {
			return
		}
	}

	run.mu.Lock()
	run.phase = "running"
	run.message = ""
	run.measureAt = time.Now()
	run.mu.Unlock()
	run.driveMongo(ctx, mc, time.Duration(run.cfg.DurationS)*time.Second, true)
	run.mu.Lock()
	run.measureEnd = time.Now()
	run.mu.Unlock()

	if !run.cfg.KeepData && run.cfg.Workload != "crud" {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		mc.ord.Drop(cctx)
		mc.cust.Drop(cctx)
		mc.prod.Drop(cctx)
		db.Collection(benchMMeta).Drop(cctx)
		ccancel()
	}
}

func (run *benchRun) driveMongo(ctx context.Context, mc *mongoCtx, dur time.Duration, record bool) {
	dctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	var wg sync.WaitGroup
	for t := 0; t < run.cfg.Threads; t++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for dctx.Err() == nil {
				run.unitMongo(dctx, mc, rng, record)
			}
		}(run.cfg.Seed + int64(t) + 1)
	}
	wg.Wait()
}

func (run *benchRun) unitMongo(ctx context.Context, mc *mongoCtx, rng *rand.Rand, record bool) {
	t0 := time.Now()
	var err error
	switch run.cfg.Workload {
	case "olap":
		err = run.mongoOLAP(ctx, mc, rng, record)
	case "ro":
		err = run.mongoRO(ctx, mc, rng, record)
	case "rw":
		err = run.mongoRW(ctx, mc, rng, record)
	case "crud":
		err = run.mongoCRUD(ctx, mc, rng, record)
	default:
		err = run.mongoOLTP(ctx, mc, rng, record)
	}
	if record && ctx.Err() == nil {
		atomic.AddInt64(&run.txns, 1)
		if err == nil {
			run.txnLat.rec(time.Since(t0))
		}
	}
}

// mop times one MongoDB operation and records it against a latency bucket, mirroring tq/te on the
// SQL side. A "not found" is a normal benchmark outcome, not an error.
func (run *benchRun) mop(ctx context.Context, typ string, record bool, fn func() error) error {
	t0 := time.Now()
	err := fn()
	if err == mongo.ErrNoDocuments {
		err = nil
	}
	run.recStmt(ctx, typ, record, t0, err)
	return err
}

func drainCursor(ctx context.Context, cur *mongo.Cursor, err error) error {
	if err != nil {
		return err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
	}
	return cur.Err()
}

// ------------------------------------------------------------------- shared ops

func (run *benchRun) mFindID(ctx context.Context, coll *mongo.Collection, id int64, typ string, record bool) error {
	return run.mop(ctx, typ, record, func() error {
		return coll.FindOne(ctx, bson.M{"_id": id}).Err()
	})
}

func (run *benchRun) mRangeOrdersByCust(ctx context.Context, mc *mongoCtx, cust int64, record bool) error {
	return run.mop(ctx, "range_find", record, func() error {
		cur, err := mc.ord.Find(ctx, bson.M{"customer_id": cust},
			options.Find().SetSort(bson.D{{Key: "order_ts", Value: -1}}).SetLimit(20))
		return drainCursor(ctx, cur, err)
	})
}

func (run *benchRun) mInsertOrder(ctx context.Context, mc *mongoCtx, rng *rand.Rand, record bool) error {
	oid := atomic.AddInt64(&run.nextOrderID, 1)
	doc := run.buildOrderDoc(rng, oid)
	return run.mop(ctx, "insert", record, func() error {
		_, err := mc.ord.InsertOne(ctx, doc)
		return err
	})
}

func (run *benchRun) mUpdateOrderStatus(ctx context.Context, mc *mongoCtx, ord int64, rng *rand.Rand, record bool) error {
	return run.mop(ctx, "update", record, func() error {
		_, err := mc.ord.UpdateOne(ctx, bson.M{"_id": ord},
			bson.M{"$set": bson.M{"status": benchPickStatus(rng)}})
		return err
	})
}

func (run *benchRun) mDeleteOrder(ctx context.Context, mc *mongoCtx, ord int64, record bool) error {
	return run.mop(ctx, "delete", record, func() error {
		_, err := mc.ord.DeleteOne(ctx, bson.M{"_id": ord})
		return err
	})
}

// ------------------------------------------------------------------- workloads

func (run *benchRun) mongoOLTP(ctx context.Context, mc *mongoCtx, rng *rand.Rand, record bool) error {
	ord := rng.Int63n(run.nOrd) + 1
	cust := rng.Int63n(run.nCust) + 1
	prod := rng.Int63n(run.nProd) + 1
	run.mFindID(ctx, mc.ord, ord, "point_find", record)
	run.mFindID(ctx, mc.cust, cust, "point_find", record)
	run.mFindID(ctx, mc.prod, prod, "point_find", record)
	run.mRangeOrdersByCust(ctx, mc, cust, record)
	run.mUpdateOrderStatus(ctx, mc, ord, rng, record)
	run.mInsertOrder(ctx, mc, rng, record)
	run.mDeleteOrder(ctx, mc, rng.Int63n(run.nOrd)+1, record)
	return nil
}

func (run *benchRun) mongoRW(ctx context.Context, mc *mongoCtx, rng *rand.Rand, record bool) error {
	run.mInsertOrder(ctx, mc, rng, record)
	run.mUpdateOrderStatus(ctx, mc, rng.Int63n(run.nOrd)+1, rng, record)
	run.mop(ctx, "update", record, func() error {
		_, err := mc.prod.UpdateOne(ctx, bson.M{"_id": rng.Int63n(run.nProd) + 1},
			bson.M{"$mul": bson.M{"price": 1.0}})
		return err
	})
	run.mDeleteOrder(ctx, mc, rng.Int63n(run.nOrd)+1, record)
	run.mFindID(ctx, mc.ord, rng.Int63n(run.nOrd)+1, "point_find", record)
	run.mFindID(ctx, mc.cust, rng.Int63n(run.nCust)+1, "point_find", record)
	return nil
}

func (run *benchRun) mongoRO(ctx context.Context, mc *mongoCtx, rng *rand.Rand, record bool) error {
	for i := 0; i < 10; i++ {
		switch i % 3 {
		case 0:
			run.mFindID(ctx, mc.ord, rng.Int63n(run.nOrd)+1, "point_find", record)
		case 1:
			run.mFindID(ctx, mc.cust, rng.Int63n(run.nCust)+1, "point_find", record)
		default:
			run.mFindID(ctx, mc.prod, rng.Int63n(run.nProd)+1, "point_find", record)
		}
	}
	return run.mRangeOrdersByCust(ctx, mc, rng.Int63n(run.nCust)+1, record)
}

func (run *benchRun) mongoOLAP(ctx context.Context, mc *mongoCtx, rng *rand.Rand, record bool) error {
	aggOpts := options.Aggregate().SetAllowDiskUse(true)
	agg := func(typ string, pipeline mongo.Pipeline) error {
		return run.mop(ctx, typ, record, func() error {
			cur, err := mc.ord.Aggregate(ctx, pipeline, aggOpts)
			return drainCursor(ctx, cur, err)
		})
	}
	switch rng.Intn(5) {
	case 0: // revenue by product category
		return agg("agg_q1", mongo.Pipeline{
			{{Key: "$unwind", Value: "$items"}},
			{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$items.category"},
				{Key: "rev", Value: bson.D{{Key: "$sum", Value: "$items.line_amount"}}},
				{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
			{{Key: "$sort", Value: bson.D{{Key: "rev", Value: -1}}}},
		})
	case 1: // monthly revenue
		return agg("agg_q2", mongo.Pipeline{
			{{Key: "$group", Value: bson.D{
				{Key: "_id", Value: bson.D{{Key: "$dateToString", Value: bson.D{{Key: "format", Value: "%Y-%m"}, {Key: "date", Value: "$order_ts"}}}}},
				{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
				{Key: "rev", Value: bson.D{{Key: "$sum", Value: "$total_amount"}}}}}},
			{{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}},
		})
	case 2: // top customers by spend
		return agg("agg_q3", mongo.Pipeline{
			{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$customer_id"},
				{Key: "spend", Value: bson.D{{Key: "$sum", Value: "$total_amount"}}}}}},
			{{Key: "$sort", Value: bson.D{{Key: "spend", Value: -1}}}},
			{{Key: "$limit", Value: 50}},
		})
	case 3: // average order value by country + segment
		return agg("agg_q4", mongo.Pipeline{
			{{Key: "$group", Value: bson.D{
				{Key: "_id", Value: bson.D{{Key: "country", Value: "$cust_country"}, {Key: "segment", Value: "$cust_segment"}}},
				{Key: "aov", Value: bson.D{{Key: "$avg", Value: "$total_amount"}}},
				{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
			{{Key: "$sort", Value: bson.D{{Key: "aov", Value: -1}}}},
		})
	default: // top products in a recent window
		since := time.Now().Add(-time.Duration(rng.Intn(300)+30) * 24 * time.Hour)
		return agg("agg_q5", mongo.Pipeline{
			{{Key: "$match", Value: bson.D{{Key: "order_ts", Value: bson.D{{Key: "$gte", Value: since}}}}}},
			{{Key: "$unwind", Value: "$items"}},
			{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$items.product_id"},
				{Key: "units", Value: bson.D{{Key: "$sum", Value: "$items.quantity"}}},
				{Key: "rev", Value: bson.D{{Key: "$sum", Value: "$items.line_amount"}}}}}},
			{{Key: "$sort", Value: bson.D{{Key: "rev", Value: -1}}}},
			{{Key: "$limit", Value: 100}},
		})
	}
}

// ------------------------------------------------------------------- dataset

func (run *benchRun) prepareMongo(ctx context.Context, mc *mongoCtx) error {
	db := mc.ord.Database()
	if run.cfg.KeepData && run.mongoDatasetMatches(ctx, db) {
		run.setPhase("preparing", "reusing existing dataset")
		return run.initMongoCounters(ctx, mc)
	}
	run.setPhase("preparing", "creating collections")
	mc.ord.Drop(ctx)
	mc.cust.Drop(ctx)
	mc.prod.Drop(ctx)
	run.setPhase("loading", "loading data")
	if err := run.loadMongo(ctx, mc); err != nil {
		return err
	}
	if err := run.ensureMongoIndexes(ctx, mc); err != nil {
		return err
	}
	run.writeMongoMeta(ctx, db)
	return run.initMongoCounters(ctx, mc)
}

// buildOrderDoc renders one order document with embedded items. category (from the product) and
// the customer's country/segment are denormalized onto the order so the OLAP aggregations stay
// single-collection.
func (run *benchRun) buildOrderDoc(rng *rand.Rand, oid int64) bson.M {
	cust := rng.Int63n(run.nCust) + 1
	nItems := rng.Intn(benchMaxItemsOrd) + 1
	total := 0.0
	items := make(bson.A, 0, nItems)
	for k := 0; k < nItems; k++ {
		qty := rng.Intn(5) + 1
		price := float64(rng.Intn(9900)+100) / 100.0
		line := price * float64(qty)
		total += line
		items = append(items, bson.M{
			"product_id":  rng.Int63n(run.nProd) + 1,
			"category":    benchCategories[rng.Intn(len(benchCategories))],
			"quantity":    qty,
			"unit_price":  price,
			"line_amount": line,
		})
	}
	return bson.M{
		"_id":          oid,
		"customer_id":  cust,
		"order_ts":     time.Now().Add(-time.Duration(rng.Intn(365*24)) * time.Hour),
		"status":       benchPickStatus(rng),
		"total_amount": total,
		"item_count":   nItems,
		"cust_country": benchCountries[rng.Intn(len(benchCountries))],
		"cust_segment": benchSegments[rng.Intn(len(benchSegments))],
		"items":        items,
	}
}

func (run *benchRun) loadMongo(ctx context.Context, mc *mongoCtx) error {
	rng := rand.New(rand.NewSource(run.cfg.Seed))
	now := time.Now()

	flush := func(coll *mongo.Collection, docs []any) error {
		if len(docs) == 0 {
			return nil
		}
		_, err := coll.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false))
		atomic.AddInt64(&run.rowsLoaded, int64(len(docs)))
		return err
	}

	// Customers.
	buf := make([]any, 0, benchLoadBatch)
	for id := int64(1); id <= run.nCust; id++ {
		buf = append(buf, bson.M{
			"_id": id, "name": "Customer", "email": "user@example.com",
			"city": benchCities[rng.Intn(len(benchCities))], "country": benchCountries[rng.Intn(len(benchCountries))],
			"segment":    benchSegments[rng.Intn(len(benchSegments))],
			"created_at": now.Add(-time.Duration(rng.Intn(3*365*24)) * time.Hour),
		})
		if len(buf) >= benchLoadBatch {
			if err := flush(mc.cust, buf); err != nil {
				return err
			}
			buf = buf[:0]
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	if err := flush(mc.cust, buf); err != nil {
		return err
	}

	// Products.
	buf = buf[:0]
	for id := int64(1); id <= run.nProd; id++ {
		cat := benchCategories[rng.Intn(len(benchCategories))]
		price := float64(rng.Intn(99000)+100) / 100.0
		buf = append(buf, bson.M{
			"_id": id, "name": "Product", "category": cat, "price": price, "cost": price * 0.6, "active": 1,
		})
		if len(buf) >= benchLoadBatch {
			if err := flush(mc.prod, buf); err != nil {
				return err
			}
			buf = buf[:0]
		}
	}
	if err := flush(mc.prod, buf); err != nil {
		return err
	}

	// Orders (embedded items).
	buf = buf[:0]
	for oid := int64(1); oid <= run.nOrd; oid++ {
		buf = append(buf, run.buildOrderDoc(rng, oid))
		if len(buf) >= benchLoadBatch {
			if err := flush(mc.ord, buf); err != nil {
				return err
			}
			buf = buf[:0]
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return flush(mc.ord, buf)
}

func (run *benchRun) ensureMongoIndexes(ctx context.Context, mc *mongoCtx) error {
	idx := func(coll *mongo.Collection, keys bson.D) error {
		_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: keys})
		return err
	}
	if err := idx(mc.prod, bson.D{{Key: "category", Value: 1}}); err != nil {
		return err
	}
	if err := idx(mc.ord, bson.D{{Key: "customer_id", Value: 1}}); err != nil {
		return err
	}
	if err := idx(mc.ord, bson.D{{Key: "order_ts", Value: 1}}); err != nil {
		return err
	}
	return idx(mc.ord, bson.D{{Key: "status", Value: 1}})
}

func (run *benchRun) writeMongoMeta(ctx context.Context, db *mongo.Database) {
	meta := db.Collection(benchMMeta)
	meta.Drop(ctx)
	meta.InsertOne(ctx, bson.M{"_id": "params", "scale": run.cfg.Scale, "seed": run.cfg.Seed})
}

func (run *benchRun) mongoDatasetMatches(ctx context.Context, db *mongo.Database) bool {
	var m struct {
		Scale int   `bson:"scale"`
		Seed  int64 `bson:"seed"`
	}
	if err := db.Collection(benchMMeta).FindOne(ctx, bson.M{"_id": "params"}).Decode(&m); err != nil {
		return false
	}
	return m.Scale == run.cfg.Scale && m.Seed == run.cfg.Seed
}

// initMongoCounters points the order-id allocator past the highest loaded/inserted _id so new
// orders never collide with an existing one.
func (run *benchRun) initMongoCounters(ctx context.Context, mc *mongoCtx) error {
	var top struct {
		ID int64 `bson:"_id"`
	}
	mc.ord.FindOne(ctx, bson.M{}, options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}}).
		SetProjection(bson.M{"_id": 1})).Decode(&top)
	start := top.ID
	if run.nOrd > start {
		start = run.nOrd
	}
	atomic.StoreInt64(&run.nextOrderID, start)
	return nil
}

// ------------------------------------------------------------------- CRUD (existing collection)

// mongoCrudPlan drives weighted find/insert/update/delete against a user's existing collection.
// Inserts synthesize documents from the introspected field schema (reusing the data generator's
// mongoValue); find/update/delete filter on sampled real key values so they hit live documents.
type mongoCrudPlan struct {
	coll      *mongo.Collection
	gens      []colGen // insertable fields (everything except _id, which the server assigns)
	updatable []colGen // gens whose field is not a filter key ($set targets)
	filter    []string // filter field names (the user's choice, else _id)
	nonce     string
	rowSeq    int64 // atomic

	mu   sync.RWMutex
	rows []bson.M // sampled filter-key tuples
}

func (p *mongoCrudPlan) setRows(r []bson.M) { p.mu.Lock(); p.rows = r; p.mu.Unlock() }

func (p *mongoCrudPlan) pick(rng *rand.Rand) bson.M {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.rows) == 0 {
		return nil
	}
	return p.rows[rng.Intn(len(p.rows))]
}

// filterOf builds a query from a random non-empty subset of the filter fields.
func (p *mongoCrudPlan) filterOf(rng *rand.Rand, tuple bson.M) bson.M {
	idx := rng.Perm(len(p.filter))[:1+rng.Intn(len(p.filter))]
	f := bson.M{}
	for _, j := range idx {
		name := p.filter[j]
		f[name] = tuple[name]
	}
	return f
}

func (run *benchRun) buildMongoCRUDPlan(ctx context.Context, db *mongo.Database) (*mongoCrudPlan, error) {
	st, err := run.app.store.GetStack(run.cfg.StackID)
	if err != nil {
		return nil, errMongo("stack not found")
	}
	c, ok := run.app.dbConnFor(st, run.cfg.NodeID)
	if !ok {
		return nil, errMongo("node is not running")
	}
	meta, err := run.app.mongoCollectionMeta(ctx, c, run.cfg.Database, run.cfg.Table)
	if err != nil {
		return nil, err
	}
	byName := map[string]bool{}
	for _, col := range meta.Columns {
		byName[col.Name] = true
	}

	filter := run.cfg.FilterColumns
	if len(filter) == 0 {
		filter = []string{"_id"} // every document has one
	}
	for _, f := range filter {
		if !byName[f] && f != "_id" {
			return nil, errMongo("filter field " + f + " not found in collection")
		}
	}
	filterSet := map[string]bool{}
	for _, f := range filter {
		filterSet[f] = true
	}

	var gens, updatable []colGen
	for _, col := range meta.Columns {
		if col.Name == "_id" {
			continue // let the server assign _id so inserts never collide
		}
		g := colGen{col: col, gen: col.Generator, opts: map[string]any{}}
		gens = append(gens, g)
		if !filterSet[col.Name] {
			updatable = append(updatable, g)
		}
	}

	return &mongoCrudPlan{
		coll: db.Collection(run.cfg.Table), gens: gens, updatable: updatable,
		filter: filter, nonce: qrNewID()[:8],
	}, nil
}

func (run *benchRun) mongoRefreshSample(ctx context.Context, p *mongoCrudPlan) {
	proj := bson.M{}
	for _, f := range p.filter {
		proj[f] = 1
	}
	cur, err := p.coll.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$sample", Value: bson.D{{Key: "size", Value: crudSampleSize}}}},
		{{Key: "$project", Value: proj}},
	})
	if err != nil {
		return
	}
	defer cur.Close(ctx)
	var rows []bson.M
	for cur.Next(ctx) {
		var m bson.M
		if cur.Decode(&m) == nil {
			rows = append(rows, m)
		}
	}
	if cur.Err() == nil {
		p.setRows(rows)
	}
}

func (run *benchRun) mongoSampler(ctx context.Context, p *mongoCrudPlan) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run.mongoRefreshSample(ctx, p)
		}
	}
}

func (run *benchRun) mongoCRUD(ctx context.Context, mc *mongoCtx, rng *rand.Rand, record bool) error {
	p := mc.crud
	switch run.crudPickOp(rng) { // reuse the SQL weight picker (insert/update/delete/select)
	case "insert":
		gc := &genCtx{rng: rng, engine: "mongodb", uniq: p.nonce, row: atomic.AddInt64(&p.rowSeq, 1)}
		doc := mongoDoc(p.gens, gc)
		return run.mop(ctx, "insert", record, func() error {
			_, err := p.coll.InsertOne(ctx, doc)
			return err
		})
	case "update":
		if len(p.updatable) == 0 {
			return run.mongoCRUDFind(ctx, p, rng, record) // nothing to $set → read instead
		}
		tuple := p.pick(rng)
		if tuple == nil {
			return nil
		}
		g := p.updatable[rng.Intn(len(p.updatable))]
		gc := &genCtx{rng: rng, engine: "mongodb", uniq: p.nonce, row: atomic.AddInt64(&p.rowSeq, 1)}
		val, _ := mongoValue(g.col, g.gen, g.opts, gc)
		return run.mop(ctx, "update", record, func() error {
			_, err := p.coll.UpdateOne(ctx, p.filterOf(rng, tuple), bson.M{"$set": bson.M{g.col.Name: val}})
			return err
		})
	case "delete":
		tuple := p.pick(rng)
		if tuple == nil {
			return nil
		}
		return run.mop(ctx, "delete", record, func() error {
			_, err := p.coll.DeleteOne(ctx, p.filterOf(rng, tuple))
			return err
		})
	default:
		return run.mongoCRUDFind(ctx, p, rng, record)
	}
}

func (run *benchRun) mongoCRUDFind(ctx context.Context, p *mongoCrudPlan, rng *rand.Rand, record bool) error {
	tuple := p.pick(rng)
	if tuple == nil {
		return nil
	}
	return run.mop(ctx, "point_find", record, func() error {
		return p.coll.FindOne(ctx, p.filterOf(rng, tuple)).Err()
	})
}

func errMongo(msg string) error { return fmt.Errorf("%s", msg) }
