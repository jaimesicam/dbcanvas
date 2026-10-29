package mongo

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"perfsim/internal/core"
)

const groupLag = "Replication lag"

var holdParam = core.Param{Key: "hold", Label: "Maximum hold, then release", Type: "pills", Opts: []any{30, 60, 120, 300, 600}, Def: 120, Unit: "s"}

func (e *Engine) Dials() []core.Dial {
	e.mu.Lock()
	topo := e.topology
	e.mu.Unlock()
	avail := func(d core.Dial) core.Dial {
		d.Group, d.Available = groupLag, topo == "replset" || topo == "sharded"
		if !d.Available {
			d.Why = "No secondaries to fall behind: link a replica set or a sharded cluster to use this."
		}
		return d
	}
	return append([]core.Dial{
		avail(core.Dial{ID: "bulk", Name: "Bulk writes", Kind: "toggle", NeedsTable: "pk",
			Desc:  "One large updateMany on a big collection at a fixed interval. Every changed document is its own oplog entry, and the secondaries apply them in batches behind everything else.",
			Watch: []string{"Replication lag", "p99 latency"},
			Params: []core.Param{
				{Key: "docs", Label: "Documents per batch", Type: "steps", Opts: []any{10000, 50000, 200000, 1000000}, Def: 200000, Unit: "rows"},
				{Key: "every", Label: "One every", Type: "pills", Opts: []any{30, 60, 120}, Def: 60, Unit: "s"},
			},
			SQL: "db.{table}.updateMany(\n  { _id: { $lte: {docs} } },\n  { $inc: { note: 1 } }\n)  // every {every} s"}),
		avail(core.Dial{ID: "index", Name: "Index build on a large collection", Kind: "run", NeedsTable: "any", Params: []core.Param{},
			Desc:  "Builds a new index on a big collection. It builds on every member at once and the primary waits for them to finish, so the secondaries' build competes with applying the oplog.",
			Watch: []string{"Replication lag", "p99 latency"},
			SQL:   "db.{table}.dropIndex(\"perfsim_build\")  // if left from before\ndb.{table}.createIndex({ customer: 1, note: 1 }, { name: \"perfsim_build\" })"}),
		avail(core.Dial{ID: "fsynclock", Name: "fsyncLock on secondaries", Kind: "toggle", Members: "replicas",
			Desc:   "Locks the chosen secondaries against writes. They stop applying the oplog and fall behind second for second. If that leaves fewer than a majority applying, writes with the default w: \"majority\" wait too.",
			Watch:  []string{"Replication lag", "p99 latency"},
			Params: []core.Param{holdParam},
			SQL:    "db.fsyncLock()\n// up to {hold} s later:\ndb.fsyncUnlock()"}),
		avail(core.Dial{ID: "delay", Name: "Delayed secondary", Kind: "toggle", Members: "replicas",
			Desc:   "Makes the chosen secondaries delayed members: priority 0, hidden, and secondaryDelaySecs. Lag settles at the delay. A delayed member still votes, so if a majority is delayed, w: \"majority\" writes wait for the delay.",
			Watch:  []string{"Replication lag", "p99 latency"},
			Params: []core.Param{{Key: "delay", Label: "Delay", Type: "pills", Opts: []any{30, 60, 300}, Def: 60, Unit: "s"}, holdParam},
			Note:   "One replica set reconfiguration for all the chosen members; their previous priority, hidden and delay are restored on release.",
			SQL:    "cfg = rs.conf()\n// for each chosen member:\ncfg.members[i].priority = 0\ncfg.members[i].hidden = true\ncfg.members[i].secondaryDelaySecs = {delay}\nrs.reconfig(cfg)"}),
	}, append(e.loadDials(), e.labDials()...)...)
}

var collName = regexp.MustCompile(`^big_[0-9a-z]+$`)

func (e *Engine) Start(ctx context.Context, id string, p core.Params, targets []string, emit func(level, text string)) (core.Running, error) {
	table := p.Str("table", "")
	for _, d := range e.Dials() {
		if d.ID == id && d.NeedsTable != "" && !collName.MatchString(table) {
			return nil, fmt.Errorf("choose a collection; prepare one under Datasets first")
		}
	}
	for _, t := range targets {
		if e.member(t) == nil {
			return nil, fmt.Errorf("%s is not a member of %s", t, e.cfg.Name)
		}
	}
	switch id {
	case "bulk":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runBulk(ctx, j, table, emit) }), nil
	case "index":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runIndex(ctx, j, table, emit) }), nil
	case "fsynclock":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runFsyncLock(ctx, j, targets) }), nil
	case "delay":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runDelay(ctx, j, targets, p.Int("delay", 60)) }), nil
	}
	if run, ok := e.startLoad(ctx, id, p, table); ok {
		return run, nil
	}
	if run, ok := e.startLab(ctx, id, p, table); ok {
		return run, nil
	}
	return nil, fmt.Errorf("no such stressor %q", id)
}

func (e *Engine) runBulk(ctx context.Context, j *core.Job, coll string, emit func(string, string)) {
	next := time.Now().Add(2 * time.Second)
	for ctx.Err() == nil {
		p := j.Param()
		docs, every := p.Int("docs", 200000), p.Int("every", 60)
		if wait := time.Until(next); wait > 0 {
			j.Set(fmt.Sprintf("Next in %d s", int(wait.Seconds()+0.5)), -1, "")
			sleep(ctx, min(wait, time.Second))
			continue
		}
		next = time.Now().Add(time.Duration(every) * time.Second)
		j.Set(fmt.Sprintf("Updating %s documents", fmtN(docs)), -1, "")
		start := time.Now()
		// MaxTime bounds it on the server, so a stopped dial does not leave it running.
		opCtx, cancel := context.WithCancel(ctx)
		res, err := e.db().Collection(coll).UpdateMany(opCtx, bson.D{{Key: "_id", Value: bson.D{{Key: "$lte", Value: docs}}}},
			bson.D{{Key: "$inc", Value: bson.D{{Key: "note", Value: 1}}}})
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			j.Fail(err)
			emit("warn", "Bulk write failed: "+err.Error())
		default:
			emit("neutral", fmt.Sprintf("Bulk write: %s documents updated in %.1f s", fmtN(int(res.ModifiedCount)), time.Since(start).Seconds()))
		}
	}
}

func (e *Engine) runIndex(ctx context.Context, j *core.Job, coll string, emit func(string, string)) {
	c := e.db().Collection(coll)
	c.Indexes().DropOne(ctx, "perfsim_build")
	start := time.Now()
	j.Set("Building on every member", -1, "")
	done := make(chan error, 1)
	go func() {
		_, err := c.Indexes().CreateOne(context.Background(), mongo.IndexModel{
			Keys: bson.D{{Key: "customer", Value: 1}, {Key: "note", Value: 1}}, Options: options.Index().SetName("perfsim_build")})
		done <- err
	}()
	for {
		select {
		case err := <-done:
			if err != nil {
				j.Finish(err)
				return
			}
			emit("neutral", fmt.Sprintf("Index build on %s committed in %.0f s", coll, time.Since(start).Seconds()))
			e.waitSettled(ctx, j, "Secondaries catching up")
			return
		case <-ctx.Done():
			// Dropping the index aborts a build in progress on every member.
			kctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			c.Indexes().DropOne(kctx, "perfsim_build")
			cancel()
			<-done
			return
		case <-time.After(time.Second):
			j.Set("Building on every member", -1, fmtDur(time.Since(start))+" elapsed")
		}
	}
}

// waitSettled ends a run once every secondary has caught up again.
func (e *Engine) waitSettled(ctx context.Context, j *core.Job, phase string) {
	start, settled := time.Now(), 0
	for ctx.Err() == nil {
		worst := 0.0
		for _, r := range e.replicaIDs() {
			if l := e.lagOf(r); l > worst {
				worst = l
			}
		}
		j.Set(phase, -1, fmt.Sprintf("worst lag %.0f s", worst))
		if time.Since(start) > 5*time.Second && worst < 2 {
			settled++
		} else {
			settled = 0
		}
		if settled >= 3 {
			j.Finish(nil)
			return
		}
		sleep(ctx, time.Second)
	}
}

// runFsyncLock locks each target. The lock belongs to the server, not to the
// connection that took it, so it is recorded first and undone on release, or
// on the next start if the sim was killed.
func (e *Engine) runFsyncLock(ctx context.Context, j *core.Job, targets []string) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok []string
	for _, m := range targets {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			key := "undo:fsynclock:" + m
			u := undo{Kind: "fsyncunlock", Member: m}
			e.saveUndo(ctx, key, u)
			j.OnRelease(func(ctx context.Context) error {
				err := e.restore(ctx, u)
				if err == nil {
					e.Delete(ctx, key)
				}
				return wrap(m, err)
			})
			actx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			if err := e.member(m).Database("admin").RunCommand(actx, bson.D{{Key: "fsync", Value: 1}, {Key: "lock", Value: true}}).Err(); err != nil {
				j.Fail(wrap(m, err))
				return
			}
			mu.Lock()
			ok = append(ok, m)
			mu.Unlock()
		}(m)
	}
	wg.Wait()
	if len(ok) == 0 {
		j.Finish(fmt.Errorf("could not lock any member: %s", j.Status().Err))
		return
	}
	sort.Strings(ok)
	j.Set("fsync-locked: "+strings.Join(ok, ", "), -1, "")
	<-ctx.Done()
}

// runDelay makes the targets delayed members in one reconfiguration: MongoDB
// lets a configuration change only once the previous one is committed, so one
// change per member at once would refuse all but the first.
func (e *Engine) runDelay(ctx context.Context, j *core.Job, targets []string, delay int) {
	// One reconfiguration per replica set the targets are in: one, or on a
	// sharded cluster one per shard.
	for set, ids := range e.setsOf(targets) {
		old := map[string]memberSettings{}
		key := "undo:delay:" + set
		err := e.reconfig(ctx, set, func(m bson.M, id string) {
			if !contains(ids, id) {
				return
			}
			old[id] = memberSettings{Priority: num(m["priority"], 1), Hidden: m["hidden"] == true, Delay: int64(num(m["secondaryDelaySecs"], 0))}
			m["priority"], m["hidden"], m["secondaryDelaySecs"] = 0, true, delay
		}, func() {
			// Recorded before the change is sent.
			b, _ := json.Marshal(old)
			u := undo{Kind: "delay", Member: set, Value: string(b)}
			e.saveUndo(ctx, key, u)
			j.OnRelease(func(ctx context.Context) error {
				err := e.restore(ctx, u)
				if err == nil {
					e.Delete(ctx, key)
				}
				return err
			})
		})
		if err != nil {
			j.Finish(err)
			return
		}
	}
	j.Set(fmt.Sprintf("Delayed by %d s: %s", delay, strings.Join(targets, ", ")), -1, "")
	<-ctx.Done()
}

type memberSettings struct {
	Priority float64 `json:"priority"`
	Hidden   bool    `json:"hidden"`
	Delay    int64   `json:"delay"`
}

// reconfig reads one replica set's configuration from its primary, lets change
// edit each member, runs before() once the edits are known, and applies the
// result. On a sharded cluster each shard is its own replica set.
func (e *Engine) reconfig(ctx context.Context, set string, change func(m bson.M, id string), before func()) error {
	c, _ := e.setPrimaryClient(set)
	if c == nil {
		return fmt.Errorf("replica set %s has no primary to reconfigure", nz(set, "?"))
	}
	admin := c.Database("admin")
	var res struct {
		Config bson.M `bson:"config"`
	}
	if err := admin.RunCommand(ctx, bson.D{{Key: "replSetGetConfig", Value: 1}}).Decode(&res); err != nil {
		return err
	}
	cfg := res.Config
	members, _ := cfg["members"].(bson.A)
	for _, x := range members {
		if m, ok := x.(bson.M); ok {
			change(m, e.memberByAddr(fmt.Sprint(m["host"])))
		}
	}
	cfg["version"] = int64(num(cfg["version"], 1)) + 1
	delete(cfg, "term") // the server sets it; a reconfig must not carry it, as mongosh's rs.reconfig does not
	if before != nil {
		before()
	}
	return admin.RunCommand(ctx, bson.D{{Key: "replSetReconfig", Value: cfg}}).Err()
}

// undo is a change made to the replica set, recorded before it is made.
type undo struct {
	Kind   string `json:"kind"`             // fsyncunlock | delay
	Member string `json:"member,omitempty"` // the member, or for delay its replica set
	Value  string `json:"value,omitempty"`
}

func (e *Engine) saveUndo(ctx context.Context, key string, u undo) {
	b, _ := json.Marshal(u)
	if err := e.Save(ctx, key, b); err != nil {
		log.Printf("perfsim: recording how to undo %s: %v", key, err)
	}
}

func (e *Engine) restore(ctx context.Context, u undo) error {
	switch u.Kind {
	case "fsyncunlock":
		c := e.member(u.Member)
		if c == nil {
			return fmt.Errorf("%s is no longer a member", u.Member)
		}
		// fsyncLock nests; unlock until the count is zero.
		for i := 0; i < 20; i++ {
			var r struct {
				LockCount int `bson:"lockCount"`
			}
			err := c.Database("admin").RunCommand(ctx, bson.D{{Key: "fsyncUnlock", Value: 1}}).Decode(&r)
			if err != nil {
				if strings.Contains(err.Error(), "not locked") {
					return nil
				}
				return err
			}
			if r.LockCount == 0 {
				return nil
			}
		}
		return fmt.Errorf("still locked after 20 unlocks")
	case "delay":
		old := map[string]memberSettings{}
		if err := json.Unmarshal([]byte(u.Value), &old); err != nil {
			return err
		}
		// Member is the replica set; an undo from before sets were tracked has
		// none, and names the set of the members it lists.
		set := u.Member
		if set == "" {
			for id := range old {
				for s := range e.setsOf([]string{id}) {
					set = s
				}
			}
		}
		return e.reconfig(ctx, set, func(m bson.M, id string) {
			if s, ok := old[id]; ok {
				m["priority"], m["hidden"], m["secondaryDelaySecs"] = s.Priority, s.Hidden, s.Delay
			}
		}, nil)
	}
	return fmt.Errorf("unknown undo %q", u.Kind)
}

func (e *Engine) replayUndo(ctx context.Context) {
	all, err := e.Load(ctx, "undo:")
	if err != nil {
		return
	}
	for key, b := range all {
		var u undo
		if json.Unmarshal(b, &u) != nil {
			e.Delete(ctx, key)
			continue
		}
		if err := e.restore(ctx, u); err != nil {
			log.Printf("perfsim: could not undo %s left by an earlier run: %v", key, err)
			continue
		}
		log.Printf("perfsim: undid %s left by an earlier run", key)
		e.Delete(ctx, key)
	}
}

func wrap(m string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", m, err)
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func num(v any, def float64) float64 {
	switch n := v.(type) {
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	case int:
		return float64(n)
	}
	return def
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func fmtN(n int) string {
	switch {
	case n >= 1000000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', -1, 64) + "M"
	case n >= 1000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', -1, 64) + "k"
	}
	return strconv.Itoa(n)
}

func fmtDur(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%d s", s)
	}
	return fmt.Sprintf("%dm %02ds", s/60, s%60)
}
