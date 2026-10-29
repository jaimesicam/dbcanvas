package mongo

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"perfsim/internal/core"
)

// Stressors that make the server show a condition its own statistics report:
// many collections (WiredTiger data handles and files) and full collection
// scans (COLLSCAN, keys and documents examined).

// extraCollPrefix names the many-collections stressor's collections; nothing
// else in the sim uses it.
const extraCollPrefix = "many_"

func (e *Engine) labDials() []core.Dial {
	return []core.Dial{
		{ID: "tables", Group: groupMem, Name: "Many collections", Kind: "toggle", Available: true,
			Desc:  "Creates small collections up to the number chosen, then keeps reading one document from each in turn. Every collection and index is a WiredTiger file with a data handle; past what the server keeps open, handles are closed and reopened, and checkpoints have more files to visit.",
			Watch: []string{"p99 latency"},
			Params: []core.Param{
				{Key: "count", Label: "Collections", Type: "steps", Opts: []any{500, 1000, 2000, 5000}, Def: 1000},
				{Key: "after", Label: "When it stops", Type: "pills", Opts: []any{"keep the collections", "drop them"}, Def: "keep the collections"},
			},
			Note: "Creating thousands of collections takes a while the first time; kept, they make the next start quick.",
			SQL:  "db.many_00001.insertMany([...])  // up to {count}\ndb.many_NNNNN.findOne()  // each in turn, about 100 a second"},
		{ID: "fullscan", Group: groupIO, Name: "Full collection scans", Kind: "toggle", NeedsTable: "pk", Available: true,
			Desc:   "Queries on a field that has no index, so the only plan is a COLLSCAN of a prepared collection. They raise documents examined, fill the slow query log, and push the workload's pages out of the WiredTiger cache.",
			Watch:  []string{"Pages read into cache", "p99 latency"},
			Params: []core.Param{{Key: "workers", Label: "Concurrent scans", Type: "steps", Opts: []any{1, 2, 4, 8}, Def: 2}},
			SQL:    "db.{table}.countDocuments({ note: { $gte: n, $lte: n + 10 } })  // note has no index"},
	}
}

func (e *Engine) startLab(ctx context.Context, id string, p core.Params, table string) (core.Running, bool) {
	var run func(context.Context, *core.Job)
	switch id {
	case "tables":
		run = e.runManyCollections
	case "fullscan":
		run = func(ctx context.Context, j *core.Job) { e.runFullScans(ctx, j, table) }
	default:
		return nil, false
	}
	return core.NewJob(ctx, p, run), true
}

func extraCollName(i int) string { return fmt.Sprintf("%s%05d", extraCollPrefix, i) }

func (e *Engine) runManyCollections(ctx context.Context, j *core.Job) {
	want := j.Param().Int("count", 1000)
	db := e.db()
	have := map[string]bool{}
	if names, err := db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: bson.D{{Key: "$regex", Value: "^" + extraCollPrefix}}}}); err == nil {
		for _, n := range names {
			have[n] = true
		}
	}
	for i := 1; i <= want && ctx.Err() == nil; i++ {
		name := extraCollName(i)
		if have[name] {
			continue
		}
		if _, err := db.Collection(name).InsertMany(ctx, []any{bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "_id", Value: 2}}},
			options.InsertMany().SetOrdered(false)); err != nil && !strings.Contains(err.Error(), "duplicate key") {
			j.Fail(err)
			return
		}
		if i%50 == 0 {
			j.Set("Creating collections", float64(i)/float64(want), fmt.Sprintf("%d of %d", i, want))
		}
	}
	j.OnRelease(func(context.Context) error {
		if j.Param().Str("after", "keep the collections") != "drop them" {
			return nil
		}
		// Dropping thousands of collections outlasts a release; it carries on alone.
		go func() {
			dctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			for i := 1; i <= 5000; i++ {
				db.Collection(extraCollName(i)).Drop(dctx)
			}
		}()
		return nil
	})
	var touched atomic.Int64
	go j.RateNote(ctx, fmt.Sprintf("Reading %d collections in turn", want), &touched, "collection reads/s")
	// Evenly, about 100 a second, so the rate reads steadily.
	for next := 0; ctx.Err() == nil; {
		var doc bson.Raw
		db.Collection(extraCollName(next%want+1)).FindOne(ctx, bson.D{}).Decode(&doc)
		touched.Add(1)
		next++
		sleep(ctx, 10*time.Millisecond)
	}
}

func (e *Engine) runFullScans(ctx context.Context, j *core.Job, table string) {
	c := e.db().Collection(table)
	var n atomic.Int64
	var wg sync.WaitGroup
	workers := j.Param().Int("workers", 2)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				lo := rand.IntN(1000)
				// maxTime bounds a scan the server would otherwise finish after
				// the dial is turned off.
				_, err := c.CountDocuments(ctx, bson.D{{Key: "note", Value: bson.D{{Key: "$gte", Value: lo}, {Key: "$lte", Value: lo + 10}}}},
					options.Count().SetMaxTime(5*time.Minute))
				if err == nil {
					n.Add(1)
				} else {
					sleep(ctx, time.Second)
				}
			}
		}()
	}
	go j.RateNote(ctx, fmt.Sprintf("%d scans at a time over %s", workers, table), &n, "scans/s")
	wg.Wait()
}
