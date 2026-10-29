package valkey

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"perfsim/internal/core"
)

// The stressors beyond replication lag: connections and a busy script. They
// act on the primary the sim writes to (on a cluster, one shard's primary),
// and each one stops what it started when it is turned off.

const (
	groupConns = "Connections"
	groupIO    = "I/O and CPU"
)

// busyScript spins for ARGV[1] milliseconds. Valkey runs a script to the end
// on its one thread, so nothing else on that server is answered meanwhile.
// TIME is frozen while a script runs, so the clock is os.clock, the server's
// CPU time; a server without it gets a counted loop of about the same length.
const busyScript = `local stop = tonumber(ARGV[1]) / 1000
if os and os.clock then
  local t0 = os.clock()
  repeat until os.clock() - t0 >= stop
else
  for i = 1, tonumber(ARGV[1]) * 50000 do end
end
return 1`

func (e *Engine) loadDials() []core.Dial {
	return []core.Dial{
		{ID: "storm", Group: groupConns, Name: "Connection storm", Kind: "toggle", Available: true,
			Desc:  "Opens connections to the primary and holds them idle, up to the number chosen. Each costs memory for its buffers; past maxclients the server refuses new ones, the application's included.",
			Watch: []string{"Connections", "Errors"},
			Params: []core.Param{{Key: "conns", Label: "Connections", Type: "steps", Opts: []any{100, 500, 1000, 2000, 5000}, Def: 500},
				{Key: "hold", Label: "Maximum hold, then release", Type: "pills", Opts: []any{30, 60, 120, 300}, Def: 120, Unit: "s"}},
			SQL: "-- {conns} connections, each idle after PING"},
		{ID: "cpu", Group: groupIO, Name: "Busy Lua script", Kind: "toggle", Available: true,
			Desc:  "Runs a script that spins for a set time, back to back. Valkey runs scripts on its single thread, so every other command on that primary waits for each one to finish.",
			Watch: []string{"p99 latency", "p50 latency"},
			Params: []core.Param{
				{Key: "ms", Label: "Each script runs", Type: "pills", Opts: []any{10, 50, 200, 1000}, Def: 50, Unit: "ms"},
				{Key: "gap", Label: "Pause between scripts", Type: "pills", Opts: []any{0, 50, 200, 1000}, Def: 200, Unit: "ms"},
			},
			Note: "On a cluster it runs on one shard's primary; commands for keys on other shards are not delayed.",
			SQL:  "EVAL \"local t0 = os.clock() repeat until os.clock() - t0 >= {ms} / 1000\" 0 {ms}\n-- then {gap} ms, and again"},
	}
}

func (e *Engine) startLoad(ctx context.Context, id string, p core.Params) (core.Running, bool) {
	switch id {
	case "storm":
		return core.NewJob(ctx, p, e.runStorm), true
	case "cpu":
		return core.NewJob(ctx, p, e.runBusyScript), true
	}
	return nil, false
}

// writerMember is the primary the sim writes to, as a member.
func (e *Engine) writerMember() (MemberConfig, bool) {
	e.mu.Lock()
	wid := e.writerID
	e.mu.Unlock()
	m := e.memberCfg(wid)
	return m, m.ID != ""
}

// runStorm opens connections one by one and holds them. Closing them is the
// whole release.
func (e *Engine) runStorm(ctx context.Context, j *core.Job) {
	want := j.Param().Int("conns", 500)
	m, ok := e.writerMember()
	if !ok {
		j.Finish(fmt.Errorf("no primary to connect to"))
		return
	}
	c := redis.NewClient(&redis.Options{Addr: fmt.Sprintf("%s:%d", m.Host, m.Port), Password: e.cfg.Password, PoolSize: want,
		DialTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second, ConnMaxIdleTime: -1})
	defer c.Close()
	var conns []*redis.Conn
	defer func() {
		for _, cn := range conns {
			cn.Close()
		}
	}()
	refused := 0
	var lastErr string
	for len(conns) < want && ctx.Err() == nil {
		cn := c.Conn()
		if err := cn.Ping(ctx).Err(); err != nil {
			cn.Close()
			refused++
			lastErr = err.Error()
			j.Set(fmt.Sprintf("%d of %d connections open", len(conns), want), float64(len(conns))/float64(want), "refused: "+short(lastErr, 80))
			sleep(ctx, 500*time.Millisecond)
			continue
		}
		conns = append(conns, cn)
		if len(conns)%50 == 0 {
			j.Set(fmt.Sprintf("Opening connections to %s", m.ID), float64(len(conns))/float64(want), fmt.Sprintf("%d of %d", len(conns), want))
		}
	}
	note := ""
	if refused > 0 {
		note = fmt.Sprintf("%d refused, last: %s", refused, short(lastErr, 80))
	}
	j.Set(fmt.Sprintf("Holding %d connections", len(conns)), -1, note)
	<-ctx.Done()
}

// runBusyScript runs the script on the primary's own connection, with no
// keys, so on a cluster it stays on that one node.
func (e *Engine) runBusyScript(ctx context.Context, j *core.Job) {
	m, ok := e.writerMember()
	if !ok {
		j.Finish(fmt.Errorf("no primary to run it on"))
		return
	}
	c := e.mc[m.ID]
	var n atomic.Int64
	go j.RateNote(ctx, "Spinning on "+m.ID, &n, "scripts/s")
	for ctx.Err() == nil {
		p := j.Param()
		ms, gap := p.Int("ms", 50), p.Int("gap", 200)
		// The script ends on its own within a second, so it is never killed:
		// the context only stops the next one.
		if err := c.Eval(context.Background(), busyScript, nil, ms).Err(); err != nil {
			j.Note("last error: " + short(err.Error(), 80))
			sleep(ctx, time.Second)
			continue
		}
		n.Add(1)
		sleep(ctx, time.Duration(gap)*time.Millisecond)
	}
}
