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

	// The replica set's own view, from whichever member answers.
	status := map[string]rsMember{}
	conf := map[string]rsConfigMember{}
	if topo == "replset" {
		var st struct {
			Members []rsMember `bson:"members"`
		}
		var cf struct {
			Config struct {
				Members []rsConfigMember `bson:"members"`
			} `bson:"config"`
		}
		admin := e.writer.Database("admin")
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

	out := make([]core.Member, len(e.cfg.Members))
	locked := make([]bool, len(e.cfg.Members))
	var wg sync.WaitGroup
	for i, mc := range e.cfg.Members {
		wg.Add(1)
		go func(i int, mc MemberConfig) {
			defer wg.Done()
			out[i], locked[i] = e.sampleMember(ctx, mc)
		}(i, mc)
	}
	wg.Wait()

	primary := ""
	for i := range out {
		m := &out[i]
		s, ok := status[m.ID]
		if topo != "replset" {
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
			primary = m.ID
		}
		m.Fields = append([][2]string{
			{"stateStr", s.StateStr},
			{"syncSourceHost", nz(s.SyncSource, "—")},
			{"optimeDate", s.OptimeDate.UTC().Format("15:04:05")},
			{"secondaryDelaySecs", strconv.FormatInt(c.SecondDelay, 10)},
			{"priority / hidden", fmt.Sprintf("%g / %v", c.Priority, c.Hidden)},
			{"fsync locked", strconv.FormatBool(locked[i])},
		}, m.Fields...)
		if s.LastHeartbeat != "" {
			m.Fields = append(m.Fields, [2]string{"lastHeartbeatMessage", short(s.LastHeartbeat, 100)})
		}
		switch {
		case !m.Up:
		case s.StateStr == "PRIMARY":
			m.Chip, m.ChipText, m.Lag = "ok", "primary", 0
		case locked[i]:
			m.Chip, m.ChipText = "bad", "fsync-locked"
		case s.StateStr != "SECONDARY":
			m.Chip, m.ChipText = "warn", s.StateStr
		case m.Lag > 30:
			m.Chip, m.ChipText = "bad", "lagging"
		case m.Lag > 5:
			m.Chip, m.ChipText = "warn", "behind"
		default:
			m.Chip, m.ChipText = "ok", "in sync"
		}
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

	var srv core.ServerSample
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
	if c := e.mc[primary]; c != nil && c.Database("admin").RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&raw) == nil && bson.Unmarshal(raw, &ss) == nil {
		srv = core.ServerSample{Conns: ss.Connections.Current, LockWaits: ss.GlobalLock.CurrentQueue.Total, Threads: ss.GlobalLock.ActiveClients.Total}
		var all bson.M
		if bson.Unmarshal(raw, &all) == nil {
			srv.DiskReads = e.readRate(primary, cacheStat(all, "pages read into cache"))
		}
	}
	return out, srv
}

// sampleMember reads the member's copy of the heartbeat and whether it is
// fsync-locked.
func (e *Engine) sampleMember(ctx context.Context, mc MemberConfig) (core.Member, bool) {
	m := core.Member{ID: mc.ID, Role: mc.Role, Lag: -1}
	c := e.mc[mc.ID]
	var hb struct {
		TS time.Time `bson:"ts"`
	}
	err := c.Database(database).Collection("heartbeat").FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&hb)
	if err != nil && err.Error() != "mongo: no documents in result" {
		m.Chip, m.ChipText = "bad", "unreachable"
		m.Fields = [][2]string{{"error", short(err.Error(), 120)}}
		return m, false
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
	return m, op.FsyncLock
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
