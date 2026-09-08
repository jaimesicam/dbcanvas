package sim

import (
	"context"
	"time"

	"trafficsim/internal/store"
)

// Snapshot is the full authoritative state a client fetches on load or reconnect
// (spec §15) — everything a fresh browser tab needs to render immediately, before
// any WebSocket message has arrived. It always reads from Valkey, never from
// Engine's in-memory state, so it reflects exactly what every other client sees too.
type Snapshot struct {
	Roads       []map[string]string `json:"roads"`
	Signals     []map[string]string `json:"signals"`
	Incidents   []map[string]string `json:"incidents"`
	Agents      []map[string]string `json:"agents"`
	Rankings    Rankings            `json:"rankings"`
	Stats       Stats               `json:"stats"`
	Control     Control             `json:"control"`
	LastEventID string              `json:"lastEventId"`
	UptimeSec   int64               `json:"uptimeSeconds"`
}

type RankEntry struct {
	RoadID string  `json:"roadId"`
	Name   string  `json:"name"`
	Score  float64 `json:"score"`
}
type Rankings struct {
	Congestion []RankEntry `json:"congestion"`
	Slowest    []RankEntry `json:"slowest"`
}
type Stats struct {
	EventsTotal     int64 `json:"eventsTotal"`
	VehiclesTotal   int64 `json:"vehiclesTotal"`
	VehiclesActive  int   `json:"vehiclesActive"`
	IncidentsActive int   `json:"incidentsActive"`
}
type Control struct {
	State string `json:"state"`
	Level string `json:"level"`
}

func (e *Engine) BuildSnapshot(ctx context.Context) Snapshot {
	c := e.Store.Client
	var snap Snapshot

	for _, r := range e.Map.Roads {
		if m, err := c.HGetAll(ctx, store.RoadKey(r.ID)).Result(); err == nil && len(m) > 0 {
			snap.Roads = append(snap.Roads, m)
		}
	}
	e.mu.RLock()
	for id := range e.signals {
		if m, err := c.HGetAll(ctx, store.SignalKey(id)).Result(); err == nil && len(m) > 0 {
			snap.Signals = append(snap.Signals, m)
		}
	}
	incidentIDs := make([]string, 0, len(e.incidents))
	for id := range e.incidents {
		incidentIDs = append(incidentIDs, id)
	}
	vehicleCount := len(e.vehicles)
	e.mu.RUnlock()

	for _, id := range incidentIDs {
		if m, err := c.HGetAll(ctx, store.IncidentKey(id)).Result(); err == nil && len(m) > 0 {
			snap.Incidents = append(snap.Incidents, m)
		}
	}

	for _, agentID := range []string{"vehicle-mover", "sensor-sampler", "signal-cycler", "incident-generator", "state-calculator"} {
		if m, err := c.HGetAll(ctx, store.AgentKey(agentID)).Result(); err == nil && len(m) > 0 {
			snap.Agents = append(snap.Agents, m)
		}
	}

	snap.Rankings.Congestion = e.topRoads(ctx, store.RankCongestion, true, 10)
	snap.Rankings.Slowest = e.topRoads(ctx, store.RankSlowest, false, 10)

	snap.Stats.EventsTotal, _ = c.Get(ctx, store.StatEventsTotal).Int64()
	snap.Stats.VehiclesTotal, _ = c.Get(ctx, store.StatVehiclesSeen).Int64()
	snap.Stats.VehiclesActive = vehicleCount
	snap.Stats.IncidentsActive = len(incidentIDs)

	snap.Control.State, _ = c.Get(ctx, store.ControlState).Result()
	snap.Control.Level, _ = c.Get(ctx, store.ControlLevel).Result()

	if entries, err := c.XRevRangeN(ctx, store.StreamKey, "+", "-", 1).Result(); err == nil && len(entries) > 0 {
		snap.LastEventID = entries[0].ID
	}
	snap.UptimeSec = int64(time.Since(e.startedAt).Seconds())
	return snap
}

func (e *Engine) topRoads(ctx context.Context, key string, highToLow bool, n int64) []RankEntry {
	var zs []struct {
		Member string
		Score  float64
	}
	if highToLow {
		res, err := e.Store.Client.ZRevRangeWithScores(ctx, key, 0, n-1).Result()
		if err != nil {
			return nil
		}
		for _, z := range res {
			zs = append(zs, struct {
				Member string
				Score  float64
			}{z.Member.(string), z.Score})
		}
	} else {
		res, err := e.Store.Client.ZRangeWithScores(ctx, key, 0, n-1).Result()
		if err != nil {
			return nil
		}
		for _, z := range res {
			zs = append(zs, struct {
				Member string
				Score  float64
			}{z.Member.(string), z.Score})
		}
	}
	out := make([]RankEntry, 0, len(zs))
	for _, z := range zs {
		name := z.Member
		if road := e.Map.RoadByID[z.Member]; road != nil {
			name = road.Name
		}
		out = append(out, RankEntry{RoadID: z.Member, Name: name, Score: z.Score})
	}
	return out
}
