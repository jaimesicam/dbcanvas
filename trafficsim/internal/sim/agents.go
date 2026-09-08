package sim

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"trafficsim/internal/store"
)

const stateCalcConsumer = "state-calc-1"

func atoiOr(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}
func atofOr(s string, def float64) float64 {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v
	}
	return def
}

func tickLoop(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// ---------------------------------------------------------------- vehicle mover

func (e *Engine) runVehicleMover(ctx context.Context) {
	events := 0
	tickLoop(ctx, 500*time.Millisecond, func() {
		if e.Running() {
			events += e.moveTick()
		}
		e.Store.Heartbeat(ctx, "vehicle-mover", "mover", "ok", events, 0)
	})
}

func (e *Engine) moveTick() int {
	ctx := e.ctx
	dtHours := 0.5 / 3600.0
	e.mu.Lock()

	// Spawn toward the current traffic-level target.
	target := e.Level().targetVehicles()
	for len(e.vehicles) < target {
		e.nextVeh++
		road := e.Map.Roads[rand.Intn(len(e.Map.Roads))]
		v := &Vehicle{
			ID: fmt.Sprintf("v%d", e.nextVeh), Type: randVehicleType(),
			RoadID: road.ID, Position: 0, Speed: road.SpeedLimit,
			Status: "moving", SpawnedAt: time.Now(), LastUpdate: time.Now(),
		}
		e.vehicles[v.ID] = v
	}

	// Group by road for signal/following behavior.
	byRoad := map[string][]*Vehicle{}
	for _, v := range e.vehicles {
		byRoad[v.RoadID] = append(byRoad[v.RoadID], v)
	}
	for _, list := range byRoad {
		sort.Slice(list, func(i, j int) bool { return list[i].Position < list[j].Position })
	}

	var arrived, transitioned []*Vehicle
	pipe := e.Store.Client.Pipeline()

	for roadID, list := range byRoad {
		road := e.Map.RoadByID[roadID]
		if road == nil {
			continue
		}
		sig := e.signals[road.SignalID]
		incident := e.activeIncidentOnLocked(roadID) // e.mu already held for this whole tick
		lengthKm := road.LengthM / 1000

		for i, v := range list {
			target := road.SpeedLimit
			if incident != nil {
				switch incident.Severity {
				case "major":
					target = 0
				case "moderate":
					target *= 0.35
				default:
					target *= 0.65
				}
			}
			// Signal: slow to a stop approaching a red/yellow light.
			if sig != nil && sig.State != "green" && v.Position > 0.75 {
				target = 0
				v.Status = "queued"
			} else {
				v.Status = "moving"
			}
			// Following distance: don't outrun the vehicle ahead on the same road.
			if i > 0 {
				ahead := list[i-1]
				if ahead.Position-v.Position < 0.04 {
					if ahead.Speed < target {
						target = ahead.Speed
					}
				}
			}
			v.Speed = target
			if target > 0 && lengthKm > 0 {
				v.Position += (target * dtHours) / lengthKm
			}
			v.LastUpdate = time.Now()

			if v.Position >= 1 {
				next := e.pickNextRoad(road)
				if next == nil || rand.Float64() < 0.08 { // occasionally a trip ends
					arrived = append(arrived, v)
					continue
				}
				v.RoadID = next.ID
				v.Position = 0
				transitioned = append(transitioned, v)
			}

			lon, lat := road.LonLat(v.Position)
			pipe.HSet(ctx, store.VehicleKey(v.ID), map[string]any{
				"id": v.ID, "type": v.Type, "roadId": v.RoadID,
				"position": v.Position, "speed": v.Speed, "status": v.Status,
				"lastUpdate": v.LastUpdate.UTC().Format(time.RFC3339),
			})
			pipe.Expire(ctx, store.VehicleKey(v.ID), store.VehicleTTL)
			pipe.GeoAdd(ctx, store.GeoVehicles, &redis.GeoLocation{Name: v.ID, Longitude: lon, Latitude: lat})
		}
	}
	for _, v := range arrived {
		delete(e.vehicles, v.ID)
		pipe.Del(ctx, store.VehicleKey(v.ID))
		pipe.ZRem(ctx, store.GeoVehicles, v.ID)
	}
	pipe.Set(ctx, store.StatVehiclesSeen, e.nextVeh, 0)
	vehicleCount := len(e.vehicles)
	e.mu.Unlock()

	pipe.Exec(ctx)
	_ = vehicleCount
	for _, v := range transitioned {
		e.Store.PublishEvent(ctx, "vehicle_entered_road", v.ID, v.RoadID, "vehicle-mover", v.ID+" entered "+v.RoadID)
	}
	return len(transitioned)
}

func (e *Engine) pickNextRoad(from *Road) *Road {
	opts := e.Map.AdjOut[from.To.ID]
	if len(opts) == 0 {
		return nil
	}
	return opts[rand.Intn(len(opts))]
}

func randVehicleType() string {
	types := []string{"car", "car", "car", "truck", "bus"}
	return types[rand.Intn(len(types))]
}

// ---------------------------------------------------------------- sensor sampler

func (e *Engine) runSensorSampler(ctx context.Context) {
	events := 0
	lastCount := map[string]int{}
	tickLoop(ctx, 2*time.Second, func() {
		if e.Running() {
			events += e.sensorTick(lastCount)
		}
		e.Store.Heartbeat(ctx, "sensor-sampler", "sensor", "ok", events, 0)
	})
}

func (e *Engine) sensorTick(lastCount map[string]int) int {
	ctx := e.ctx
	e.mu.RLock()
	byRoad := map[string][]*Vehicle{}
	for _, v := range e.vehicles {
		byRoad[v.RoadID] = append(byRoad[v.RoadID], v)
	}
	e.mu.RUnlock()

	pipe := e.Store.Client.Pipeline()
	emitted := 0
	for _, road := range e.Map.Roads {
		list := byRoad[road.ID]
		count := len(list)
		var speedSum float64
		for _, v := range list {
			speedSum += v.Speed
		}
		avgSpeed := road.SpeedLimit
		if count > 0 {
			avgSpeed = speedSum / float64(count)
		}
		occupancy := 0.0
		if road.LengthM > 0 {
			occupancy = float64(count) * 6.0 / road.LengthM
			if occupancy > 1 {
				occupancy = 1
			}
		}
		sid := "sen-" + road.ID
		pipe.HSet(ctx, store.SensorKey(sid), map[string]any{
			"id": sid, "roadId": road.ID, "condition": "ok",
			"vehicleCount": count, "avgSpeed": avgSpeed, "occupancy": occupancy,
			"lastObservation": time.Now().UTC().Format(time.RFC3339),
		})
		if count != lastCount[road.ID] {
			lastCount[road.ID] = count
			e.Store.PublishEvent(ctx, "sensor_reading", sid, road.ID, "sensor-sampler",
				fmt.Sprintf("%d vehicles on %s", count, road.Name))
			emitted++
		}
	}
	pipe.Exec(ctx)
	return emitted
}

// ---------------------------------------------------------------- signal cycler

const (
	signalGreen  = 8 * time.Second
	signalYellow = 2 * time.Second
	signalRed    = 8 * time.Second
	signalCycle  = signalGreen + signalYellow + signalRed
)

func (e *Engine) runSignalCycler(ctx context.Context) {
	events := 0
	start := time.Now()
	tickLoop(ctx, 1*time.Second, func() {
		if e.Running() {
			events += e.signalTick(start)
		}
		e.Store.Heartbeat(ctx, "signal-cycler", "signal", "ok", events, 0)
	})
}

func staggerOffset(id string) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(id))
	return time.Duration(h.Sum32()%uint32(signalCycle.Seconds())) * time.Second
}

func (e *Engine) signalTick(start time.Time) int {
	ctx := e.ctx
	changed := 0
	e.mu.Lock()
	pipe := e.Store.Client.Pipeline()
	for id, sig := range e.signals {
		elapsed := (time.Since(start) + staggerOffset(id)) % signalCycle
		state := "red"
		switch {
		case elapsed < signalGreen:
			state = "green"
		case elapsed < signalGreen+signalYellow:
			state = "yellow"
		}
		if state != sig.State {
			sig.State = state
			sig.Since = time.Now()
			pipe.HSet(ctx, store.SignalKey(id), map[string]any{"id": id, "intersectionId": sig.IntersectionID, "state": state})
			changed++
		}
	}
	e.mu.Unlock()
	pipe.Exec(ctx)
	if changed > 0 {
		e.Store.PublishEvent(ctx, "signal_changed", "", "", "signal-cycler", fmt.Sprintf("%d signal(s) changed", changed))
	}
	return changed
}

// ---------------------------------------------------------------- incident generator

func (e *Engine) runIncidentGenerator(ctx context.Context) {
	events := 0
	tickLoop(ctx, 5*time.Second, func() {
		if e.Running() {
			events += e.incidentTick()
		}
		e.Store.Heartbeat(ctx, "incident-generator", "incident", "ok", events, 0)
	})
}

func (e *Engine) incidentTick() int {
	n := 0
	// Expire due incidents.
	e.mu.Lock()
	var due []*Incident
	for _, inc := range e.incidents {
		if inc.Status == "active" && time.Now().After(inc.EndsAt) {
			due = append(due, inc)
		}
	}
	e.mu.Unlock()
	for _, inc := range due {
		e.ClearIncident(inc.ID)
		n++
	}

	// Maybe create a new one.
	if rand.Float64() < e.Level().incidentChancePerTick() {
		road := e.Map.Roads[rand.Intn(len(e.Map.Roads))]
		if e.activeIncidentOn(road.ID) == nil {
			e.CreateIncident(randIncidentType(), road.ID, "")
			n++
		}
	}
	return n
}

func randIncidentType() string {
	types := []string{"accident", "stall", "construction", "closure"}
	weights := []int{3, 3, 2, 1} // closures are rarer
	total := 0
	for _, w := range weights {
		total += w
	}
	r := rand.Intn(total)
	for i, w := range weights {
		if r < w {
			return types[i]
		}
		r -= w
	}
	return "stall"
}

// CreateIncident starts a new incident on roadID (severity "" picks one at random).
// Shared by the incident-generator agent and the web control API's manual trigger.
func (e *Engine) CreateIncident(kind, roadID, severity string) *Incident {
	road := e.Map.RoadByID[roadID]
	if road == nil {
		return nil
	}
	if severity == "" {
		sevs := []string{"minor", "moderate", "major"}
		severity = sevs[rand.Intn(len(sevs))]
	}
	if kind == "closure" {
		severity = "major"
	}
	e.mu.Lock()
	e.nextInc++
	inc := &Incident{
		ID: fmt.Sprintf("inc%d", e.nextInc), Type: kind, RoadID: roadID, Severity: severity,
		StartedAt: time.Now(), EndsAt: time.Now().Add(time.Duration(20+rand.Intn(40)) * time.Second),
		Status: "active",
	}
	e.incidents[inc.ID] = inc
	e.mu.Unlock()

	ctx := e.ctx
	lon, lat := road.LonLat(0.5)
	e.Store.Client.HSet(ctx, store.IncidentKey(inc.ID), map[string]any{
		"id": inc.ID, "type": inc.Type, "roadId": inc.RoadID, "severity": inc.Severity,
		"startTime": inc.StartedAt.UTC().Format(time.RFC3339), "status": "active",
	})
	e.Store.Client.GeoAdd(ctx, store.GeoIncidents, &redis.GeoLocation{Name: inc.ID, Longitude: lon, Latitude: lat})
	e.Store.Client.ZIncrBy(ctx, store.RankIncidents, 1, roadID)
	e.Store.PublishEvent(ctx, "incident_created", inc.ID, roadID, "incident-generator",
		inc.Type+" ("+inc.Severity+") on "+road.Name)
	return inc
}

// ClearIncident ends an active incident — called by expiry or the manual control API.
func (e *Engine) ClearIncident(id string) bool {
	e.mu.Lock()
	inc, ok := e.incidents[id]
	if !ok {
		e.mu.Unlock()
		return false
	}
	inc.Status = "cleared"
	delete(e.incidents, id)
	e.mu.Unlock()

	ctx := e.ctx
	e.Store.Client.Del(ctx, store.IncidentKey(id))
	e.Store.Client.ZRem(ctx, store.GeoIncidents, id)
	e.Store.PublishEvent(ctx, "incident_cleared", id, inc.RoadID, "incident-generator", id+" cleared")
	return true
}

// activeIncidentOn acquires e.mu itself — callers that already hold it (moveTick,
// which locks for its whole tick) must use activeIncidentOnLocked instead, or they
// self-deadlock: sync.RWMutex is not reentrant.
func (e *Engine) activeIncidentOn(roadID string) *Incident {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.activeIncidentOnLocked(roadID)
}

func (e *Engine) activeIncidentOnLocked(roadID string) *Incident {
	for _, inc := range e.incidents {
		if inc.RoadID == roadID && inc.Status == "active" {
			return inc
		}
	}
	return nil
}

// ---------------------------------------------------------------- state calculator

// runStateCalculator is the processing agent: it both consumes the shared event
// stream via a durable consumer group (demonstrating XREADGROUP/XACK resilience —
// restart this process and it resumes from the group's own cursor, and XPENDING/
// XCLAIM at startup reclaims anything left unacked by a prior crash) and, on its own
// tick, recomputes every road's congestion classification from current sensor +
// signal + incident state.
func (e *Engine) runStateCalculator(ctx context.Context) {
	e.ensureConsumerGroup(ctx)
	e.reclaimPending(ctx)
	events := 0
	go e.consumeEvents(ctx, &events)

	lastState := map[string]RoadState{}
	tickLoop(ctx, 2*time.Second, func() {
		if e.Running() {
			e.recomputeCongestion(lastState)
		}
		e.Store.Heartbeat(ctx, "state-calculator", "processor", "ok", events, 0)
	})
}

func (e *Engine) ensureConsumerGroup(ctx context.Context) {
	e.Store.Client.XGroupCreateMkStream(ctx, store.StreamKey, store.ConsumerGrp, "0")
}

// reclaimPending re-delivers any message a prior instance of this consumer read but
// never XACKed (e.g. it crashed mid-processing) — spec §16's "recovery of events that
// have not completed processing."
func (e *Engine) reclaimPending(ctx context.Context) {
	res, err := e.Store.Client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: store.StreamKey, Group: store.ConsumerGrp, Start: "-", End: "+", Count: 100,
	}).Result()
	if err != nil || len(res) == 0 {
		return
	}
	ids := make([]string, 0, len(res))
	for _, p := range res {
		ids = append(ids, p.ID)
	}
	e.Store.Client.XClaim(ctx, &redis.XClaimArgs{
		Stream: store.StreamKey, Group: store.ConsumerGrp, Consumer: stateCalcConsumer,
		MinIdle: 0, Messages: ids,
	})
	e.Store.Client.XAck(ctx, store.StreamKey, store.ConsumerGrp, ids...)
}

func (e *Engine) consumeEvents(ctx context.Context, counter *int) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		streams, err := e.Store.Client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: store.ConsumerGrp, Consumer: stateCalcConsumer,
			Streams: []string{store.StreamKey, ">"}, Count: 100, Block: 2 * time.Second,
		}).Result()
		if err != nil {
			if err != redis.Nil {
				time.Sleep(time.Second)
			}
			continue
		}
		var ids []string
		for _, s := range streams {
			for _, msg := range s.Messages {
				ids = append(ids, msg.ID)
				*counter++
			}
		}
		if len(ids) > 0 {
			e.Store.Client.XAck(ctx, store.StreamKey, store.ConsumerGrp, ids...)
		}
	}
}

// congestionScore is deliberately simple, understandable and repeatable (spec §10):
// 40% weight on lane occupancy, 40% on how far avg speed has dropped below the speed
// limit, plus a flat penalty for any active incident, clamped to 0-100.
func congestionScore(occupancy, avgSpeed, speedLimit float64, incident *Incident) float64 {
	score := occupancy * 40
	if speedLimit > 0 {
		ratio := avgSpeed / speedLimit
		if ratio > 1 {
			ratio = 1
		}
		score += (1 - ratio) * 40
	}
	if incident != nil {
		switch incident.Severity {
		case "major":
			score += 40
		case "moderate":
			score += 25
		default:
			score += 10
		}
	}
	if score > 100 {
		score = 100
	}
	return score
}

func classify(score float64, incident *Incident) RoadState {
	if incident != nil && incident.Type == "closure" {
		return StateBlocked
	}
	switch {
	case score >= 80:
		return StateSevere
	case score >= 55:
		return StateHeavy
	case score >= 25:
		return StateModerate
	default:
		return StateFreeFlow
	}
}

func (e *Engine) recomputeCongestion(lastState map[string]RoadState) {
	ctx := e.ctx
	pipe := e.Store.Client.Pipeline()
	var changedEvents []string
	for _, road := range e.Map.Roads {
		sensor, err := e.Store.Client.HGetAll(ctx, store.SensorKey("sen-"+road.ID)).Result()
		if err != nil || len(sensor) == 0 {
			continue
		}
		count := atoiOr(sensor["vehicleCount"], 0)
		avgSpeed := atofOr(sensor["avgSpeed"], road.SpeedLimit)
		occupancy := atofOr(sensor["occupancy"], 0)
		incident := e.activeIncidentOn(road.ID)

		score := congestionScore(occupancy, avgSpeed, road.SpeedLimit, incident)
		state := classify(score, incident)

		pipe.HSet(ctx, store.RoadKey(road.ID), map[string]any{
			"state": string(state), "congestionScore": score,
			"avgSpeed": avgSpeed, "vehicleCount": count, "occupancy": occupancy,
			"lastUpdate": time.Now().UTC().Format(time.RFC3339),
		})
		pipe.ZAdd(ctx, store.RankCongestion, redis.Z{Score: score, Member: road.ID})
		pipe.ZAdd(ctx, store.RankSlowest, redis.Z{Score: avgSpeed, Member: road.ID})

		if lastState[road.ID] != state {
			lastState[road.ID] = state
			changedEvents = append(changedEvents, road.ID+" -> "+string(state))
		}
	}
	pipe.Exec(ctx)
	for _, msg := range changedEvents {
		e.Store.PublishEvent(ctx, "congestion_changed", "", "", "state-calculator", msg)
	}
}
