package mongo

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"perfsim/internal/core"
)

type rsMember struct {
	Name          string    `bson:"name"`
	StateStr      string    `bson:"stateStr"`
	Health        float64   `bson:"health"`
	OptimeDate    time.Time `bson:"optimeDate"`
	SyncSource    string    `bson:"syncSourceHost"`
	LastHeartbeat string    `bson:"lastHeartbeatMessage"`
}

type rsConfigMember struct {
	Host        string  `bson:"host"`
	Priority    float64 `bson:"priority"`
	Hidden      bool    `bson:"hidden"`
	SecondDelay int64   `bson:"secondaryDelaySecs"`
	Votes       int     `bson:"votes"`
}

func (e *Engine) Sample(ctx context.Context) ([]core.Member, core.ServerSample) {
	e.mu.Lock()
	topo := e.topology
	e.mu.Unlock()

	out := make([]core.Member, len(e.cfg.Members))
	info := make([]memberInfo, len(e.cfg.Members))
	var wg sync.WaitGroup
	for i, mc := range e.cfg.Members {
		wg.Add(1)
		go func(i int, mc MemberConfig) {
			defer wg.Done()
			out[i], info[i] = e.sampleMember(ctx, mc)
		}(i, mc)
	}
	wg.Wait()

	// Each replica set's own view, from one of its members that answers: one set
	// for a replica set, one per shard for a sharded cluster.
	status := map[string]rsMember{}
	conf := map[string]rsConfigMember{}
	setOf := map[string]string{}
	asked := map[string]bool{}
	for i, in := range info {
		if in.set == "" {
			continue
		}
		setOf[out[i].ID] = in.set
		if asked[in.set] {
			continue
		}
		asked[in.set] = true
		var st struct {
			Members []rsMember `bson:"members"`
		}
		var cf struct {
			Config struct {
				Members []rsConfigMember `bson:"members"`
			} `bson:"config"`
		}
		admin := e.mc[out[i].ID].Database("admin")
		if admin.RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&st) == nil {
			for _, m := range st.Members {
				status[e.memberByAddr(m.Name)] = m
			}
		}
		if admin.RunCommand(ctx, bson.D{{Key: "replSetGetConfig", Value: 1}}).Decode(&cf) == nil {
			for _, m := range cf.Config.Members {
				conf[e.memberByAddr(m.Host)] = m
			}
		}
	}

	primary := ""
	setPrimary := map[string]string{}
	for i := range out {
		m := &out[i]
		s, ok := status[m.ID]
		if topo == "standalone" {
			primary = m.ID
			m.Role, m.Chip, m.ChipText, m.Lag = "standalone", "ok", "standalone", 0
			continue
		}
		if !ok {
			continue
		}
		c := conf[m.ID]
		m.Role = map[string]string{"PRIMARY": "primary", "SECONDARY": "secondary", "ARBITER": "arbiter"}[s.StateStr]
		if m.Role == "" {
			m.Role = s.StateStr
		}
		if s.StateStr == "PRIMARY" {
			setPrimary[setOf[m.ID]] = m.ID
			// The writer the sim reports is the first shard's primary on a
			// sharded cluster; the workload goes through mongos either way.
			if primary == "" {
				primary = m.ID
			}
		}
		fields := [][2]string{
			{"stateStr", s.StateStr},
			{"syncSourceHost", nz(s.SyncSource, "—")},
			{"optimeDate", s.OptimeDate.UTC().Format("15:04:05")},
			{"secondaryDelaySecs", strconv.FormatInt(c.SecondDelay, 10)},
			{"priority / hidden", fmt.Sprintf("%g / %v", c.Priority, c.Hidden)},
			{"fsync locked", strconv.FormatBool(info[i].locked)},
		}
		if topo == "sharded" {
			fields = append([][2]string{{"shard (replica set)", setOf[m.ID]}}, fields...)
		}
		m.Fields = append(fields, m.Fields...)
		if s.LastHeartbeat != "" {
			m.Fields = append(m.Fields, [2]string{"lastHeartbeatMessage", short(s.LastHeartbeat, 100)})
		}
		switch {
		case !m.Up:
		case s.StateStr == "PRIMARY":
			m.Chip, m.ChipText, m.Lag = "ok", "primary", 0
		case info[i].locked:
			m.Chip, m.ChipText = "bad", "fsync-locked"
		case s.StateStr != "SECONDARY":
			m.Chip, m.ChipText = "warn", s.StateStr
		case m.Lag < 0:
			// On a sharded cluster, until the balancer has moved this shard's
			// heartbeat document to it.
			m.Chip, m.ChipText = "neutral", "no heartbeat yet"
		case m.Lag > 30:
			m.Chip, m.ChipText = "bad", "lagging"
		case m.Lag > 5:
			m.Chip, m.ChipText = "warn", "behind"
		default:
			m.Chip, m.ChipText = "ok", "in sync"
		}
	}
	for i := range out {
		if p := setPrimary[setOf[out[i].ID]]; p != "" && p != out[i].ID {
			out[i].Source = p
		}
	}

	e.mu.Lock()
	e.memberSet, e.setPrimary = setOf, setPrimary
	e.mu.Unlock()
	if topo == "replset" {
		e.followPrimary(primary)
	}

	e.mu.Lock()
	if primary != "" {
		e.writerID = primary
	}
	for _, m := range out {
		if m.Lag >= 0 {
			e.lag[m.ID] = m.Lag
		} else {
			delete(e.lag, m.ID)
		}
	}
	e.mu.Unlock()

	// The writer's figures: on a sharded cluster, every shard primary's summed.
	var srv core.ServerSample
	for _, p := range setPrimary {
		srv.Add(e.serverSample(ctx, p))
	}
	if len(setPrimary) == 0 && primary != "" {
		srv = e.serverSample(ctx, primary)
	}
	return out, srv
}

// memberInfo is what a member says about itself.
type memberInfo struct {
	set    string // its replica set
	locked bool   // fsync-locked
}

// sampleMember reads the member's copy of its heartbeat, its replica set and
// whether it is fsync-locked.
func (e *Engine) sampleMember(ctx context.Context, mc MemberConfig) (core.Member, memberInfo) {
	m := core.Member{ID: mc.ID, Role: mc.Role, Lag: -1}
	c := e.mc[mc.ID]
	var in memberInfo
	var hello struct {
		SetName string `bson:"setName"`
	}
	if err := c.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		m.Chip, m.ChipText = "bad", "unreachable"
		m.Fields = [][2]string{{"error", short(err.Error(), 120)}}
		return m, in
	}
	in.set = hello.SetName
	var hb struct {
		TS time.Time `bson:"ts"`
	}
	err := c.Database(database).Collection("heartbeat").FindOne(ctx, bson.D{{Key: "_id", Value: e.hbID(in.set)}}).Decode(&hb)
	if err != nil && err.Error() != "mongo: no documents in result" {
		m.Chip, m.ChipText = "bad", "unreachable"
		m.Fields = [][2]string{{"error", short(err.Error(), 120)}}
		return m, in
	}
	m.Up = true
	if !hb.TS.IsZero() {
		m.Lag = time.Since(hb.TS).Seconds()
		if m.Lag < 0 {
			m.Lag = 0
		}
	}
	var op struct {
		FsyncLock bool `bson:"fsyncLock"`
	}
	c.Database("admin").RunCommand(ctx, bson.D{{Key: "currentOp", Value: 1}, {Key: "$all", Value: false}, {Key: "active", Value: false}, {Key: "op", Value: "none"}}).Decode(&op)
	in.locked = op.FsyncLock
	return m, in
}

// serverSample is one primary's connections, queue, active clients and pages
// read into the cache.
func (e *Engine) serverSample(ctx context.Context, id string) core.ServerSample {
	var ss struct {
		Connections struct {
			Current float64 `bson:"current"`
		} `bson:"connections"`
		GlobalLock struct {
			CurrentQueue struct {
				Total float64 `bson:"total"`
			} `bson:"currentQueue"`
			ActiveClients struct {
				Total float64 `bson:"total"`
			} `bson:"activeClients"`
		} `bson:"globalLock"`
	}
	var raw bson.Raw
	c := e.mc[id]
	if c == nil || c.Database("admin").RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&raw) != nil || bson.Unmarshal(raw, &ss) != nil {
		return core.ServerSample{}
	}
	srv := core.ServerSample{Conns: ss.Connections.Current, LockWaits: ss.GlobalLock.CurrentQueue.Total, Threads: ss.GlobalLock.ActiveClients.Total}
	var all bson.M
	if bson.Unmarshal(raw, &all) == nil {
		srv.DiskReads = e.readRate(id, cacheStat(all, "pages read into cache"))
	}
	return srv
}

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// readRate turns a member's pages-read counter into a rate since the last sample.
func (e *Engine) readRate(id string, reads float64) float64 {
	now := float64(time.Now().UnixNano()) / 1e9
	e.mu.Lock()
	defer e.mu.Unlock()
	prev, ok := e.readsPrev[id]
	e.readsPrev[id] = [2]float64{reads, now}
	if ok && now > prev[1] && reads >= prev[0] {
		return (reads - prev[0]) / (now - prev[1])
	}
	return 0
}
