package sim

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"trafficsim/internal/store"
)

// Engine owns all live simulation state in memory and drives the background agent
// goroutines. Valkey is the durable, shareable view of this state — the web API and
// every browser client only ever read from Valkey, never from Engine directly, so
// Engine is free to be a plain mutex-protected struct.
type Engine struct {
	Map   *CityMap
	Store *store.Store

	mu        sync.RWMutex
	vehicles  map[string]*Vehicle
	signals   map[string]*Signal
	incidents map[string]*Incident
	nextVeh   int
	nextInc   int

	level     atomic.Value // TrafficLevel
	running   atomic.Bool
	startedAt time.Time

	baseCtx context.Context // the process's long-lived context — Start/Reset always derive from this, never from a caller's request-scoped context
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func NewEngine(st *store.Store) *Engine {
	e := &Engine{
		Map:       NewCityMap(),
		Store:     st,
		vehicles:  map[string]*Vehicle{},
		signals:   map[string]*Signal{},
		incidents: map[string]*Incident{},
	}
	for _, s := range e.Map.Signals() {
		e.signals[s.ID] = s
	}
	e.level.Store(LevelLow)
	e.running.Store(true)
	e.startedAt = time.Now()
	return e
}

func (e *Engine) Level() TrafficLevel { return e.level.Load().(TrafficLevel) }
func (e *Engine) SetLevel(l TrafficLevel) {
	e.level.Store(l)
	e.Store.Client.Set(e.ctx, store.ControlLevel, string(l), 0)
}
func (e *Engine) Running() bool { return e.running.Load() }

// Start launches every background agent goroutine and seeds static topology
// (roads/intersections/sensors) into Valkey once. Idempotent to call again after Stop.
//
// ctx is remembered as the engine's own long-lived base context (baseCtx) the first
// time Start is called, and every subsequent Start/Reset derives from that — never
// from whatever context the caller happens to pass (e.g. Reset is triggered from an
// HTTP handler, whose r.Context() is canceled the instant that response is written;
// deriving from it would silently kill every freshly-restarted agent goroutine).
func (e *Engine) Start(ctx context.Context) {
	if e.baseCtx == nil {
		e.baseCtx = ctx
	}
	e.ctx, e.cancel = context.WithCancel(e.baseCtx)
	e.running.Store(true)
	e.Store.Client.Set(e.ctx, store.ControlState, "running", 0)
	e.Store.Client.Set(e.ctx, store.ControlLevel, string(e.Level()), 0)
	e.seedStaticTopology()

	agents := []func(context.Context){
		e.runVehicleMover,
		e.runSensorSampler,
		e.runSignalCycler,
		e.runIncidentGenerator,
		e.runStateCalculator,
	}
	for _, fn := range agents {
		e.wg.Add(1)
		go func(f func(context.Context)) {
			defer e.wg.Done()
			f(e.ctx)
		}(fn)
	}
	log.Printf("trafficsim: engine started (%d roads, %d intersections)", len(e.Map.Roads), len(e.Map.Intersections))
}

// Stop cancels every agent goroutine and waits for them to exit — used by Reset
// (stop, wipe, start fresh) and graceful shutdown.
func (e *Engine) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
	e.wg.Wait()
}

func (e *Engine) Pause() {
	e.running.Store(false)
	e.Store.Client.Set(e.ctx, store.ControlState, "paused", 0)
}
func (e *Engine) Resume() {
	e.running.Store(true)
	e.Store.Client.Set(e.ctx, store.ControlState, "running", 0)
}

// seedStaticTopology writes each road/intersection/sensor's fixed fields once. The
// state-calculator agent HSETs the dynamic fields (state, congestion, speed, ...) on
// top of this same hash every tick — one Hash per entity holds both, matching the
// spec's "Current State" data category directly.
func (e *Engine) seedStaticTopology() {
	ctx := e.ctx
	pipe := e.Store.Client.Pipeline()
	for _, r := range e.Map.Roads {
		pipe.HSet(ctx, store.RoadKey(r.ID), map[string]any{
			"id": r.ID, "name": r.Name,
			"fromId": r.From.ID, "toId": r.To.ID,
			"fromLon": r.From.Lon, "fromLat": r.From.Lat,
			"toLon": r.To.Lon, "toLat": r.To.Lat,
			"speedLimit": r.SpeedLimit, "lengthM": r.LengthM, "lanes": r.Lanes,
			"state": string(StateNoData), "congestionScore": 0,
			"avgSpeed": r.SpeedLimit, "vehicleCount": 0, "occupancy": 0,
			"lastUpdate": time.Now().UTC().Format(time.RFC3339),
		})
	}
	for id, sig := range e.signals {
		pipe.HSet(ctx, store.SignalKey(id), map[string]any{
			"id": id, "intersectionId": sig.IntersectionID, "state": sig.State,
		})
	}
	for _, r := range e.Map.Roads {
		sid := "sen-" + r.ID
		pipe.HSet(ctx, store.SensorKey(sid), map[string]any{
			"id": sid, "roadId": r.ID, "condition": "ok",
			"vehicleCount": 0, "avgSpeed": r.SpeedLimit, "occupancy": 0,
			"lastObservation": time.Now().UTC().Format(time.RFC3339),
		})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("trafficsim: seed topology: %v", err)
	}
}

// Reset stops every agent, wipes every ts:* key, clears in-memory state, and starts
// fresh — restoring the exact known starting condition the spec's "Repeatability"
// non-functional requirement calls for. Takes no context: it always restarts from
// the engine's own baseCtx (see Start), not a caller-supplied one.
func (e *Engine) Reset() {
	e.Stop()
	e.mu.Lock()
	e.vehicles = map[string]*Vehicle{}
	e.incidents = map[string]*Incident{}
	e.nextVeh, e.nextInc = 0, 0
	for _, s := range e.Map.Signals() {
		e.signals[s.ID] = s
	}
	e.mu.Unlock()
	e.wipeValkey(e.baseCtx)
	e.Start(e.baseCtx)
}

func (e *Engine) wipeValkey(ctx context.Context) {
	var cursor uint64
	for {
		keys, next, err := e.Store.Client.Scan(ctx, cursor, "ts:*", 500).Result()
		if err != nil {
			log.Printf("trafficsim: reset scan: %v", err)
			return
		}
		if len(keys) > 0 {
			e.Store.Client.Unlink(ctx, keys...)
		}
		cursor = next
		if cursor == 0 {
			return
		}
	}
}
