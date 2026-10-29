package mongo

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"perfsim/internal/core"
)

const batchDocs = 20000

func (e *Engine) Datasets(ctx context.Context) ([]core.Dataset, error) {
	names, err := e.db().ListCollectionNames(ctx, bson.D{{Key: "name", Value: bson.D{{Key: "$regex", Value: "^big_"}}}})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var out []core.Dataset
	for _, n := range names {
		var st struct {
			Count          int64 `bson:"count"`
			Size           int64 `bson:"size"`
			StorageSize    int64 `bson:"storageSize"`
			TotalIndexSize int64 `bson:"totalIndexSize"`
		}
		e.db().RunCommand(ctx, bson.D{{Key: "collStats", Value: n}}).Decode(&st)
		bytes := st.StorageSize + st.TotalIndexSize
		if bytes == 0 {
			bytes = st.Size
		}
		out = append(out, core.Dataset{Name: n, Bytes: bytes, Rows: st.Count, PK: true})
	}
	return out, nil
}

// Prepare builds a collection of about the given size in 20,000-document inserts.
func (e *Engine) Prepare(ctx context.Context, name string, bytes int64, pk bool, progress func(float64)) error {
	if !pk {
		return fmt.Errorf("every MongoDB document has an _id index; there is no collection without one")
	}
	name = strings.ReplaceAll(name, ".", "_")
	if !collName.MatchString(name) {
		return fmt.Errorf("invalid collection name %q", name)
	}
	c := e.db().Collection(name)
	var have int64
	var last struct {
		ID int64 `bson:"_id"`
	}
	if c.FindOne(ctx, bson.D{}, options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}})).Decode(&last) == nil {
		have = last.ID
	}
	// MongoDB compresses what it stores, so the padding is random (it would
	// otherwise shrink to nothing) and the size that counts is the one on disk,
	// checked every ten batches.
	stored := func() int64 {
		var st struct {
			StorageSize    int64 `bson:"storageSize"`
			TotalIndexSize int64 `bson:"totalIndexSize"`
		}
		e.db().RunCommand(ctx, bson.D{{Key: "collStats", Value: name}}).Decode(&st)
		return st.StorageSize + st.TotalIndexSize
	}
	buf := make([]byte, 150)
	for n := 0; ; n++ {
		if n%10 == 0 {
			size := stored()
			if size >= bytes {
				break
			}
			progress(float64(size) / float64(bytes))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		docs := make([]any, 0, batchDocs)
		for i := have + 1; i <= have+batchDocs; i++ {
			for k := range buf {
				buf[k] = byte(rand.IntN(256))
			}
			docs = append(docs, bson.D{{Key: "_id", Value: i}, {Key: "customer", Value: i % 100000}, {Key: "note", Value: 0}, {Key: "pad", Value: base64.StdEncoding.EncodeToString(buf)}})
		}
		if _, err := c.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false)); err != nil {
			return err
		}
		have += batchDocs
	}
	progress(1)
	return nil
}

func (e *Engine) Drop(ctx context.Context, name string) error {
	if !collName.MatchString(name) {
		return fmt.Errorf("only prepared collections can be dropped")
	}
	return e.db().Collection(name).Drop(ctx)
}

// Sessions lists active operations on every member, and an fsync lock as the
// blocker it is.
func (e *Engine) Sessions(ctx context.Context, held map[string][]string) ([]core.Session, error) {
	var mu sync.Mutex
	var out []core.Session
	var wg sync.WaitGroup
	for _, mc := range e.cfg.Members {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			ss := e.memberSessions(ctx, m)
			mu.Lock()
			out = append(out, ss...)
			mu.Unlock()
		}(mc.ID)
	}
	wg.Wait()
	rank := map[string]int{"blocker": 0, "blocked": 1, "": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Kind] != rank[out[j].Kind] {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		return out[i].Time > out[j].Time
	})
	if len(out) > 60 {
		out = out[:60]
	}
	return out, nil
}

func (e *Engine) memberSessions(ctx context.Context, m string) []core.Session {
	var res struct {
		Inprog    []bson.M `bson:"inprog"`
		FsyncLock bool     `bson:"fsyncLock"`
	}
	if e.member(m).Database("admin").RunCommand(ctx, bson.D{{Key: "currentOp", Value: 1}, {Key: "active", Value: true}}).Decode(&res) != nil {
		return nil
	}
	var out []core.Session
	if res.FsyncLock {
		out = append(out, core.Session{ID: "fsyncLock", Member: m, User: "perfsim", State: "fsync lock held", Query: "db.fsyncLock()", Kind: "blocker"})
	}
	for _, op := range res.Inprog {
		desc := fmt.Sprint(op["desc"])
		cmd := fmt.Sprint(op["command"])
		if strings.Contains(cmd, "currentOp") || strings.Contains(cmd, "heartbeat") || strings.Contains(cmd, "hello") || strings.Contains(cmd, "isMaster") {
			continue
		}
		s := core.Session{ID: fmt.Sprint(op["opid"]), Member: m, User: desc, State: fmt.Sprint(op["op"]),
			Time: int64(num(op["secs_running"], 0)), Query: short(fmt.Sprint(op["ns"])+" "+cmd, 300)}
		if wl, _ := op["waitingForLock"].(bool); wl {
			s.Kind, s.State = "blocked", s.State+" · waiting for lock"
			if res.FsyncLock {
				s.Waits = "fsyncLock"
			}
		}
		if strings.HasPrefix(desc, "conn") && s.State == "none" {
			continue
		}
		out = append(out, s)
	}
	return out
}
