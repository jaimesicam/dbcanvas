package valkey

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"perfsim/internal/core"
)

const groupLag = "Replication lag"

func (e *Engine) Dials() []core.Dial {
	e.mu.Lock()
	topo := e.topology
	coordinated := coordinatedFailover(e.version)
	debug := false
	for id, ok := range e.debugOK {
		if _, replica := e.primaryOf[id]; replica && ok {
			debug = true
		}
	}
	e.mu.Unlock()
	avail := func(d core.Dial, ok bool, why string) core.Dial {
		d.Group, d.Available = groupLag, topo != "standalone" && ok
		switch {
		case topo == "standalone":
			d.Why = "No replicas to fall behind: link a Valkey Cluster with replicas, or Valkey with Sentinel, to use this."
		case !ok:
			d.Why = why
		}
		return d
	}
	return append([]core.Dial{
		avail(core.Dial{ID: "bigvalues", Name: "Large values", Kind: "toggle",
			Desc:  "SETs multi-megabyte values at a fixed rate. Every byte goes through each replica's replication stream and output buffer.",
			Watch: []string{"Replication lag", "p99 latency"},
			Params: []core.Param{
				{Key: "mb", Label: "Value size", Type: "pills", Opts: []any{1, 8, 32}, Def: 8, Unit: "MB"},
				{Key: "per", Label: "Values per second", Type: "steps", Opts: []any{1, 5, 20}, Def: 5, Unit: ""},
			},
			SQL: "SET perfsim:big:{n} <{mb} MB>  -- {per} a second"}, true, ""),
		avail(core.Dial{ID: "bigdel", Name: "Delete a big key", Kind: "run",
			Desc:  "Builds a set with millions of members, then deletes it with DEL. Freeing it blocks the primary's single thread, and then each replica's while it applies the delete.",
			Watch: []string{"p99 latency", "Replication lag"},
			Params: []core.Param{{Key: "members", Label: "Set members", Type: "steps", Opts: []any{1000000, 5000000, 10000000}, Def: 5000000, Unit: "rows"},
				{Key: "blocking", Label: "Free it", Type: "pills", Opts: []any{"blocking", "as configured"}, Def: "blocking"}},
			Note: "Valkey 8 and later free a deleted key in the background (lazyfree-lazy-user-del yes), which blocks nothing. Blocking turns that off on the key's primary for this one delete and back on afterwards.",
			SQL:  "SADD perfsim:bigset <members>  -- built in batches\nCONFIG SET lazyfree-lazy-user-del no  -- blocking only, restored after\nDEL perfsim:bigset"}, true, ""),
		avail(core.Dial{ID: "debugsleep", Name: "DEBUG SLEEP on replicas", Kind: "toggle", Members: "replicas",
			Desc:   "Blocks the chosen replicas' event loop again and again. While it sleeps, a replica reads nothing from its primary.",
			Watch:  []string{"Replication lag"},
			Params: []core.Param{{Key: "sleep", Label: "Each sleep", Type: "pills", Opts: []any{5, 10, 30}, Def: 10, Unit: "s"}, {Key: "hold", Label: "Maximum hold, then release", Type: "pills", Opts: []any{30, 60, 120, 300}, Def: 120, Unit: "s"}},
			SQL:    "DEBUG SLEEP {sleep}  -- repeated, a second apart"}, debug,
			"DEBUG is disabled on these servers (enable-debug-command is no)."),
		avail(failoverDial(nzs(e.cfg.SentinelMaster, "<name>"), coordinated), topo == "sentinel",
			"Only Valkey with Sentinel promotes a replica on its own; link one to use this."),
	}, e.loadDials()...)
}

func (e *Engine) Start(ctx context.Context, id string, p core.Params, targets []string, emit func(level, text string)) (core.Running, error) {
	for _, t := range targets {
		if e.member(t) == nil {
			return nil, fmt.Errorf("%s is not a member of %s", t, e.cfg.Name)
		}
	}
	switch id {
	case "bigvalues":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runBigValues(ctx, j) }), nil
	case "bigdel":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runBigDel(ctx, j, emit) }), nil
	case "debugsleep":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runDebugSleep(ctx, j, targets) }), nil
	case "failover":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runFailover(ctx, j, emit) }), nil
	}
	if run, ok := e.startLoad(ctx, id, p); ok {
		return run, nil
	}
	return nil, fmt.Errorf("no such stressor %q", id)
}

func (e *Engine) runBigValues(ctx context.Context, j *core.Job) {
	n, sent := 0, int64(0)
	start := time.Now()
	for ctx.Err() == nil {
		p := j.Param()
		mb, per := p.Int("mb", 8), p.Int("per", 5)
		val := strings.Repeat("v", mb<<20)
		tick := time.Now()
		for i := 0; i < per && ctx.Err() == nil; i++ {
			if err := e.writer.Set(ctx, "perfsim:big:"+strconv.Itoa(n%20), val, 10*time.Minute).Err(); err != nil {
				j.Fail(err)
			} else {
				sent += int64(len(val))
			}
			n++
		}
		j.Set(fmt.Sprintf("Writing %d × %d MB a second", per, mb), -1,
			fmt.Sprintf("%.0f MB written · %.1f MB/s", float64(sent)/(1<<20), float64(sent)/(1<<20)/time.Since(start).Seconds()))
		sleep(ctx, time.Second-time.Since(tick))
	}
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 20; i++ {
		e.writer.Unlink(cctx, "perfsim:big:"+strconv.Itoa(i))
	}
}

func (e *Engine) runBigDel(ctx context.Context, j *core.Job, emit func(string, string)) {
	members := j.Param().Int("members", 5000000)
	const key = "perfsim:bigset"
	e.writer.Unlink(ctx, key)
	// The key's own primary is the server whose setting decides how DEL frees it.
	owner := redis.Cmdable(e.writer)
	if cc, ok := e.writer.(*redis.ClusterClient); ok {
		if node, err := cc.MasterForKey(ctx, key); err == nil {
			owner = node
		}
	}
	lazy := false
	if v, err := owner.ConfigGet(ctx, "lazyfree-lazy-user-del").Result(); err == nil && v["lazyfree-lazy-user-del"] == "yes" {
		lazy = true
	}
	const batch = 10000
	for have := 0; have < members && ctx.Err() == nil; have += batch {
		args := make([]any, 0, batch)
		for i := have; i < have+batch && i < members; i++ {
			args = append(args, i)
		}
		if err := e.writer.SAdd(ctx, key, args...).Err(); err != nil {
			j.Finish(err)
			return
		}
		j.Set("Building the set", float64(have+batch)/float64(members), fmt.Sprintf("%s of %s members", fmtN(have+batch), fmtN(members)))
	}
	if ctx.Err() != nil {
		e.writer.Unlink(context.Background(), key)
		return
	}
	// Let the replicas receive the whole set before it is deleted.
	e.waitSettled(ctx, j, "Replicas receiving the set", false)
	if ctx.Err() != nil {
		e.writer.Unlink(context.Background(), key)
		return
	}
	if lazy && j.Param().Str("blocking", "blocking") == "blocking" {
		// Recorded first, like every change that outlives the sim.
		e.Save(ctx, "undo:lazyfree", []byte("yes"))
		if err := owner.ConfigSet(ctx, "lazyfree-lazy-user-del", "no").Err(); err != nil {
			j.Finish(err)
			return
		}
		defer func() {
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if owner.ConfigSet(rctx, "lazyfree-lazy-user-del", "yes").Err() == nil {
				e.Delete(rctx, "undo:lazyfree")
			}
		}()
		lazy = false
	}
	j.Set("Deleting it", -1, "")
	start := time.Now()
	if err := e.writer.Del(context.Background(), key).Err(); err != nil {
		j.Finish(err)
		return
	}
	took := time.Since(start)
	if lazy {
		emit("neutral", fmt.Sprintf("DEL of a %s-member set returned in %.0f ms: lazyfree-lazy-user-del is on, so it was freed in the background", fmtN(members), float64(took.Milliseconds())))
	} else {
		emit("neutral", fmt.Sprintf("DEL of a %s-member set blocked the primary for %.1f s", fmtN(members), took.Seconds()))
	}
	e.waitSettled(ctx, j, "Replicas applying the delete", true)
}

// runFailover asks one Sentinel to fail the primary over, then waits until a
// different member reports itself primary and every replica has caught up.
func (e *Engine) runFailover(ctx context.Context, j *core.Job, emit func(string, string)) {
	e.mu.Lock()
	before := e.writerID
	e.mu.Unlock()
	coordinated := j.Param().Str("mode", "forced") == "coordinated" && coordinatedFailover(e.version)
	args := []any{"SENTINEL", "FAILOVER", e.cfg.SentinelMaster}
	if coordinated {
		args = append(args, "COORDINATED")
	}
	// The probe writes through the application's client, which follows the
	// primary Sentinel announces, exactly as the workload does.
	probe := e.startFailoverProbe(ctx)
	sleep(ctx, time.Second)
	var err error
	for _, addr := range e.cfg.Sentinels {
		// A plain client: SentinelClient has no way to add COORDINATED.
		s := redis.NewClient(&redis.Options{Addr: addr, Password: e.cfg.Password, DialTimeout: 3 * time.Second})
		err = s.Do(ctx, args...).Err()
		s.Close()
		if err == nil {
			break
		}
	}
	if err != nil {
		probe.stop()
		j.Finish(fmt.Errorf("no Sentinel accepted the failover: %w", err))
		return
	}
	start := time.Now()
	mode := "forced"
	if coordinated {
		mode = "coordinated"
	}
	emit("warn", fmt.Sprintf("Sentinel failover (%s) of %s requested; %s is the primary until it completes", mode, e.cfg.SentinelMaster, before))
	for ctx.Err() == nil {
		e.mu.Lock()
		now := e.writerID
		e.mu.Unlock()
		if now != "" && now != before {
			promoted := time.Since(start)
			emit("neutral", fmt.Sprintf("%s is the new primary, %.0f s after the request", now, promoted.Seconds()))
			e.waitDemoted(ctx, j, before, now, start, promoted, emit)
			e.waitSettled(ctx, j, "Replicas following "+now, false)
			acked, kept, err := probe.result(ctx)
			switch {
			case err != nil:
				emit("warn", "Could not read the write probe back: "+err.Error())
			case acked > kept:
				emit("bad", fmt.Sprintf("%d of %d acknowledged probe writes were lost: the old primary took them and then resynced from %s", acked-kept, acked, now))
			default:
				emit("ok", fmt.Sprintf("All %d acknowledged probe writes survived the failover", acked))
			}
			j.Finish(nil)
			return
		}
		if time.Since(start) > 2*time.Minute {
			probe.stop()
			j.Finish(fmt.Errorf("no new primary within 2 minutes"))
			return
		}
		j.Set("Waiting for a replica to be promoted", -1, fmt.Sprintf("%.0f s since the request", time.Since(start).Seconds()))
		sleep(ctx, time.Second)
	}
}

// coordinatedFailover says whether this Valkey's Sentinel takes SENTINEL
// FAILOVER ... COORDINATED, which arrived in Valkey 9.0.
func coordinatedFailover(version string) bool {
	major, _ := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	return major >= 9
}

func failoverDial(name string, coordinated bool) core.Dial {
	d := core.Dial{ID: "failover", Name: "Sentinel failover", Kind: "run",
		Desc: "Asks Sentinel to promote a replica and demote the primary. Forced, it acts as if the primary had failed: the old primary keeps taking writes until Sentinel demotes it, and loses them when it resyncs from the new one. " +
			"Coordinated, the old primary pauses writes until the replica has caught up and been promoted, so nothing acknowledged is lost. The dial counts acknowledged writes that did not survive.",
		Watch: []string{"Errors", "p99 latency", "Replication lag"},
		SQL:   "SENTINEL FAILOVER " + name + " {mode}  -- sent to one Sentinel; COORDINATED, or nothing for forced\nINCR perfsim:failover:probe  -- every 10 ms throughout, counting acknowledgements"}
	if coordinated {
		d.Params = []core.Param{{Key: "mode", Label: "Failover", Type: "pills", Opts: []any{"coordinated", "forced"}, Def: "coordinated"}}
	} else {
		d.Params = []core.Param{}
		d.Note = "Coordinated failover needs Valkey 9.0 or later; this one is forced."
	}
	return d
}

// waitDemoted waits for the old primary to rejoin as a replica. Until then it
// still answers as a primary, and anything still writing to it is writing to a
// server whose data the resync will throw away.
func (e *Engine) waitDemoted(ctx context.Context, j *core.Job, old, now string, start time.Time, promoted time.Duration, emit func(string, string)) {
	for ctx.Err() == nil {
		e.mu.Lock()
		_, replica := e.primaryOf[old]
		e.mu.Unlock()
		if replica {
			if two := time.Since(start) - promoted; two >= 2*time.Second {
				emit("warn", fmt.Sprintf("%s rejoined as a replica of %s; it was still a primary for %.0f s after %s was promoted", old, now, two.Seconds(), now))
			} else {
				emit("neutral", fmt.Sprintf("%s rejoined as a replica of %s", old, now))
			}
			return
		}
		if time.Since(start) > 2*time.Minute {
			emit("warn", fmt.Sprintf("%s has not rejoined as a replica within 2 minutes", old))
			return
		}
		j.Set("Waiting for "+old+" to be demoted", -1, fmt.Sprintf("two primaries for %.0f s", (time.Since(start)-promoted).Seconds()))
		sleep(ctx, time.Second)
	}
}

// waitSettled waits until every replica is caught up; finish ends the run then.
func (e *Engine) waitSettled(ctx context.Context, j *core.Job, phase string, finish bool) {
	start, settled := time.Now(), 0
	for ctx.Err() == nil {
		worst := 0.0
		for _, r := range e.replicaIDs() {
			if l := e.lagOf(r); l > worst {
				worst = l
			}
		}
		j.Set(phase, -1, fmt.Sprintf("worst lag %.1f s", worst))
		if time.Since(start) > 3*time.Second && worst < 2 {
			settled++
		} else {
			settled = 0
		}
		if settled >= 3 {
			if finish {
				j.Finish(nil)
			}
			return
		}
		sleep(ctx, time.Second)
	}
}

// runDebugSleep sleeps each target's event loop, with a second between sleeps.
// Nothing outlives it: a sleep ends on its own.
func (e *Engine) runDebugSleep(ctx context.Context, j *core.Job, targets []string) {
	var wg sync.WaitGroup
	for _, m := range targets {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			opts := *e.member(m).Options()
			opts.ReadTimeout, opts.PoolSize = 120*time.Second, 1
			c := redis.NewClient(&opts)
			defer c.Close()
			for ctx.Err() == nil {
				secs := j.Param().Int("sleep", 10)
				if err := c.Do(context.Background(), "DEBUG", "SLEEP", secs).Err(); err != nil {
					j.Fail(fmt.Errorf("%s: %w", m, err))
					return
				}
				sleep(ctx, time.Second)
			}
		}(m)
	}
	sort.Strings(targets)
	j.Set("Sleeping "+strings.Join(targets, ", "), -1, "")
	wg.Wait()
}

// replayUndo turns lazy freeing back on where a killed sim left it off. The
// record does not say which primary, so every primary gets it back: it is on
// by default, and the sim only ever turns it off.
func (e *Engine) replayUndo(ctx context.Context) {
	all, err := e.Load(ctx, "undo:lazyfree")
	if err != nil || len(all) == 0 {
		return
	}
	for _, c := range e.mc {
		if r, err := c.Do(ctx, "ROLE").Slice(); err == nil && len(r) > 0 && r[0] == "master" {
			c.ConfigSet(ctx, "lazyfree-lazy-user-del", "yes")
		}
	}
	e.Delete(ctx, "undo:lazyfree")
}

// Sessions lists blocked clients and the replication links on every member.
func (e *Engine) Sessions(ctx context.Context, held map[string][]string) ([]core.Session, error) {
	var mu sync.Mutex
	var out []core.Session
	var wg sync.WaitGroup
	for _, mc := range e.cfg.Members {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			list, err := e.mc[m].ClientList(ctx).Result()
			if err != nil {
				return
			}
			for _, line := range strings.Split(strings.TrimSpace(list), "\n") {
				f := map[string]string{}
				for _, kv := range strings.Fields(line) {
					if k, v, ok := strings.Cut(kv, "="); ok {
						f[k] = v
					}
				}
				flags := f["flags"]
				kind, state := "", ""
				switch {
				case strings.Contains(flags, "b"):
					kind, state = "blocked", "blocked on "+f["cmd"]
				case strings.Contains(flags, "S"):
					state = "replica link (this is the primary)"
				case strings.Contains(flags, "M"):
					state = "link to its primary"
				default:
					continue
				}
				age, _ := strconv.ParseInt(f["idle"], 10, 64)
				s := core.Session{ID: f["id"], Member: m, User: nz(f["user"], "default"), State: state, Time: age, Query: f["addr"] + " " + f["cmd"], Kind: kind}
				mu.Lock()
				out = append(out, s)
				mu.Unlock()
			}
		}(mc.ID)
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind > out[j].Kind })
	return out, nil
}

func nzs(s, def string) string {
	if s == "" {
		return def
	}
	return s
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

// failoverProbe increments one key through the application's client every
// 10 ms while a failover runs, counting the increments the server acknowledged.
// Afterwards the key on the new primary says how many of them survived.
type failoverProbe struct {
	e      *Engine
	acked  atomic.Int64
	cancel context.CancelFunc
	done   chan struct{}
}

const probeKey = "perfsim:failover:probe"

func (e *Engine) startFailoverProbe(ctx context.Context) *failoverProbe {
	p := &failoverProbe{e: e, done: make(chan struct{})}
	e.writer.Set(ctx, probeKey, 0, time.Hour)
	pctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	go func() {
		defer close(p.done)
		for pctx.Err() == nil {
			if v, err := e.writer.Incr(pctx, probeKey).Result(); err == nil && v > 0 {
				p.acked.Add(1)
			}
			sleep(pctx, 10*time.Millisecond)
		}
	}()
	return p
}

func (p *failoverProbe) stop() {
	p.cancel()
	<-p.done
}

// result stops the probe and reads the key back from the current primary.
func (p *failoverProbe) result(ctx context.Context) (acked, kept int64, err error) {
	p.stop()
	kept, err = p.e.writer.Get(ctx, probeKey).Int64()
	return p.acked.Load(), kept, err
}
