package sim

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"stocksim/internal/store"
)

// Load levels. The same three every sibling sim uses, driving how many orders
// per second the trading agents attempt.
const (
	LevelLow    = "low"
	LevelMedium = "medium"
	LevelHigh   = "high"
)

// ordersPerSecond is the target order-creation rate at each level.
var ordersPerSecond = map[string]float64{
	LevelLow:    2,
	LevelMedium: 8,
	LevelHigh:   25,
}

func ValidLevel(s string) bool {
	_, ok := ordersPerSecond[s]
	return ok
}

// counters are the cheap in-memory tallies the analytics agent flushes into
// the metrics blob every couple of seconds. They are a convenience for the
// dashboard, never the source of truth — every number the UI shows that must
// be correct is read back from the store.
type counters struct {
	ordersCreated  atomic.Int64
	ordersFilled   atomic.Int64
	ordersExpired  atomic.Int64
	ordersRejected atomic.Int64
	ticksWritten   atomic.Int64
	// ticksBackfilled is counted apart from ticksWritten because the two answer
	// different questions: one is what the simulation is doing now, the other
	// is how much history has been manufactured to make the database big.
	ticksBackfilled atomic.Int64
	// wsReads/wsRows are the working-set agent's random history reads and the
	// rows they returned — the read side of the same story ticksBackfilled
	// tells about writes. See workingset.go.
	wsReads atomic.Int64
	wsRows  atomic.Int64
	errors  atomic.Int64
}

// Engine owns the background simulation and is the only thing that writes to
// the store without a user asking. The database is the durable, shareable view
// of all of it: the API and every browser read through BuildSnapshot, never
// from Engine's own fields.
type Engine struct {
	Store       store.Store
	Bus         *EventBus
	Clock       *SimClock
	TargetKind  string
	TargetLabel string
	// TargetBytes is how large the dataset should be grown to at the High load
	// level; 0 disables that entirely. See backfill.go.
	TargetBytes int64
	// WorkingSet is how much of that dataset is kept under continuous random
	// read, so a database can be made to miss its cache rather than merely to
	// own a lot of data it never touches. See workingset.go.
	WorkingSet WorkingSet
	// Threads is how many concurrent database workers each of the two heavy
	// agents runs — backfill writers, and working-set readers. It is the knob
	// that decides how much concurrency the target sees, and the store's
	// connection pool is sized from the same number (store.Config.PoolSize).
	Threads int

	counters counters

	level     atomic.Value // string
	running   atomic.Bool
	startedAt time.Time

	mu         sync.RWMutex
	seed       SeedProgress
	lastErr    string
	agentsUp   bool
	backfill   BackfillStatus
	workingSet WorkingSetStatus

	// baseCtx is the process's long-lived context — Start/Reset always derive
	// from this, never from a caller's request-scoped context. A Reset
	// triggered from an HTTP handler whose r.Context() is canceled the instant
	// that response is written would silently kill every freshly-restarted
	// agent goroutine a moment after Reset returned "ok".
	baseCtx context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func NewEngine(st store.Store, targetKind, targetLabel string) *Engine {
	e := &Engine{
		Store:       st,
		Bus:         NewEventBus(),
		Clock:       NewSimClock(time.Now().UTC()),
		TargetKind:  targetKind,
		TargetLabel: targetLabel,
		TargetBytes: DefaultTargetBytes,
		WorkingSet:  DefaultWorkingSet(),
		Threads:     store.DefaultThreads,
		startedAt:   time.Now(),
	}
	e.level.Store(LevelMedium)
	e.running.Store(true)
	return e
}

func (e *Engine) Level() string {
	if v, ok := e.level.Load().(string); ok {
		return v
	}
	return LevelMedium
}

func (e *Engine) SetLevel(l string) {
	if ValidLevel(l) {
		e.level.Store(l)
	}
}

func sizeTargetWord(n int64) string {
	if n <= 0 {
		return "none"
	}
	return FormatBytes(n)
}

func (e *Engine) Running() bool { return e.running.Load() }
func (e *Engine) Pause()        { e.running.Store(false) }
func (e *Engine) Resume()       { e.running.Store(true) }

func (e *Engine) UptimeSeconds() int64 {
	return int64(time.Since(e.startedAt).Seconds())
}

func (e *Engine) setSeed(p SeedProgress) {
	e.mu.Lock()
	e.seed = p
	e.mu.Unlock()
	e.Bus.publishJSON(busMessage{Type: "seed", Seed: &p})
}

func (e *Engine) Seed() SeedProgress {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.seed
}

// noteErr records the most recent background failure for the dashboard's
// banner. Agents keep running: a database that is briefly unreachable should
// produce a visible warning and then recover, not a dead simulation.
func (e *Engine) noteErr(where string, err error) {
	if err == nil {
		return
	}
	e.counters.errors.Add(1)
	e.mu.Lock()
	e.lastErr = where + ": " + err.Error()
	e.mu.Unlock()
}

func (e *Engine) lastError() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastErr
}

// StartAgents launches the background goroutines. Called after seeding
// completes, so agents never race an empty schema.
func (e *Engine) StartAgents(ctx context.Context) {
	if e.baseCtx == nil {
		e.baseCtx = ctx
	}
	e.mu.Lock()
	if e.agentsUp {
		e.mu.Unlock()
		return
	}
	e.agentsUp = true
	e.mu.Unlock()

	e.ctx, e.cancel = context.WithCancel(e.baseCtx)
	e.restoreOrAnchorClock(e.ctx)

	agents := []func(context.Context){
		e.runPriceAgent,
		e.runOrderAgent,
		e.runMatchAgent,
		e.runAnalyticsAgent,
		e.runMonitoringAgent,
		e.runEventFeed,
		e.runClockPersister,
		e.runBackfillAgent,
		e.runWorkingSetAgent,
	}
	for _, fn := range agents {
		e.wg.Add(1)
		go func(f func(context.Context)) {
			defer e.wg.Done()
			f(e.ctx)
		}(fn)
	}
	log.Printf("stocksim: engine started (engine=%s, kind=%s, target=%s, level=%s, in %s, "+
		"size target=%s, working set=%s, threads=%d)",
		e.Store.Engine(), e.TargetKind, e.TargetLabel, e.Level(),
		e.Store.Location(), sizeTargetWord(e.TargetBytes),
		e.WorkingSet, store.ClampThreads(e.Threads))
}

// Stop cancels every agent and waits for them to finish.
func (e *Engine) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
	e.wg.Wait()
	e.mu.Lock()
	e.agentsUp = false
	e.mu.Unlock()
}

// Reset empties this app's data and starts over from the seed universe. Note
// it restarts from e.baseCtx, never from the caller's context — see the field
// comment on baseCtx for why that distinction is load-bearing.
func (e *Engine) Reset(ctx context.Context) error {
	e.Stop()
	if err := e.Store.Wipe(ctx); err != nil {
		return err
	}
	e.Clock.ResetToday()
	e.counters = counters{}
	e.mu.Lock()
	e.lastErr = ""
	e.mu.Unlock()

	base := e.baseCtx
	if base == nil {
		base = context.Background()
	}
	if err := e.SeedIfNeeded(base); err != nil {
		return err
	}
	e.StartAgents(base)
	return nil
}

// Wipe empties the data but does not re-seed — the dashboard's "wipe" control,
// as distinct from "reset".
func (e *Engine) Wipe(ctx context.Context) error {
	e.Stop()
	if err := e.Store.Wipe(ctx); err != nil {
		return err
	}
	e.counters = counters{}
	base := e.baseCtx
	if base == nil {
		base = context.Background()
	}
	e.StartAgents(base)
	return nil
}

// Drop removes every object this app created and leaves the agents stopped —
// there is nothing left for them to write to. Bringing the app back is a
// deliberate act: POST /api/control/seed.
func (e *Engine) Drop(ctx context.Context) error {
	e.Stop()
	if err := e.Store.DropSchema(ctx); err != nil {
		return err
	}
	e.counters = counters{}
	e.mu.Lock()
	e.seed = SeedProgress{Done: false, Step: "Dropped"}
	e.mu.Unlock()
	return nil
}

// Reseed recreates the schema and seed data after a Drop, then restarts the
// agents.
func (e *Engine) Reseed(ctx context.Context) error {
	base := e.baseCtx
	if base == nil {
		base = context.Background()
	}
	if err := e.SeedIfNeeded(base); err != nil {
		return err
	}
	e.StartAgents(base)
	return nil
}

// clockState is what gets persisted so an accelerated clock survives a restart
// rather than jumping back to midnight.
type clockState struct {
	SimNow time.Time `json:"simNow"`
	Rate   float64   `json:"rate"`
}

func (e *Engine) restoreOrAnchorClock(ctx context.Context) {
	raw, err := e.Store.GetState(ctx, "clock")
	if err == nil && len(raw) > 0 {
		var cs clockState
		if jsonUnmarshal(raw, &cs) == nil && !cs.SimNow.IsZero() {
			e.Clock.Anchor(cs.SimNow)
			return
		}
	}
	e.Clock.ResetToday()
}

func (e *Engine) persistClock(ctx context.Context) {
	e.noteErr("persist clock", e.Store.PutState(ctx, "clock",
		clockState{SimNow: e.Clock.Now().UTC(), Rate: e.Clock.Rate()}))
}
