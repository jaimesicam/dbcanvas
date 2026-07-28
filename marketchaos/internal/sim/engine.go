package sim

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"marketchaos/internal/store"
)

// Engine owns the connection to the target database and (from stage S2
// onward) every background workload agent. As of stage S1 it can seed the
// domain schema and report progress while doing so; the actual workload
// agents are still stage S2.
type Engine struct {
	Store       *store.Store
	Kind        TargetKind
	TargetLabel string
	Dataset     DatasetCounts
	Bus         *EventBus

	level   atomic.Value // LoadLevel
	running atomic.Bool
	started time.Time

	seedMu       sync.RWMutex
	seedProgress SeedProgress
	lastPersist  time.Time
}

func NewEngine(st *store.Store, kind TargetKind, targetLabel string, dataset DatasetCounts) *Engine {
	e := &Engine{Store: st, Kind: kind, TargetLabel: targetLabel, Dataset: dataset, Bus: NewEventBus()}
	e.level.Store(LevelStop)
	return e
}

// Start marks the engine running. Stage S2 adds the actual agent goroutines
// here; for now this only flips state so the dashboard's status bar and
// control buttons have something real to reflect.
func (e *Engine) Start(ctx context.Context) {
	e.running.Store(true)
	e.started = time.Now()
	e.Store.Heartbeat(ctx, "system", "ok", "marketchaos started")
}

func (e *Engine) Stop() {
	e.running.Store(false)
}

func (e *Engine) Pause() {
	e.running.Store(false)
}

func (e *Engine) Resume() {
	e.running.Store(true)
}

// Reset truncates every owned table, then re-seeds with the same dataset
// counts the node was deployed with — the same "wipe, then rebuild the
// static/starting world" shape every sibling sim's Reset follows.
func (e *Engine) Reset(ctx context.Context) error {
	if err := store.Wipe(ctx, e.Store); err != nil {
		return err
	}
	return e.RunSeed(ctx)
}

// SeedIfNeeded seeds the schema only if it looks empty — idempotent across
// container restarts, so a crash-and-restart mid-seed (or a restart of an
// already-fully-seeded node) doesn't wipe and redo work, and a genuinely
// empty schema always gets seeded exactly once.
func (e *Engine) SeedIfNeeded(ctx context.Context) error {
	n, err := e.Store.CountRows(ctx, store.TableSecurities)
	if err != nil {
		return err
	}
	if n > 0 {
		e.seedMu.Lock()
		e.seedProgress = SeedProgress{Done: true, RowsTotal: 1, RowsDone: 1}
		e.seedMu.Unlock()
		return nil
	}
	return e.RunSeed(ctx)
}

// RunSeed runs the seeder now (used directly by SeedIfNeeded on first boot,
// and by Reset after a Wipe). Progress is kept in memory for every
// GET /api/state poll to read cheaply, published on the EventBus for
// WS-connected dashboards, and persisted to the sim_state table at most a
// few times a second — not on every one of the possibly tens of thousands
// of batches a Large-profile seed produces, which would otherwise turn the
// seeder's own progress reporting into a meaningful fraction of its load.
func (e *Engine) RunSeed(ctx context.Context) error {
	err := Seed(ctx, e.Store, e.Dataset, e.Kind.Family(), func(p SeedProgress) {
		e.seedMu.Lock()
		e.seedProgress = p
		persist := p.Done || time.Since(e.lastPersist) > 500*time.Millisecond
		if persist {
			e.lastPersist = time.Now()
		}
		e.seedMu.Unlock()

		if b, jerr := json.Marshal(busMessage{Type: "seed", Seed: &p}); jerr == nil {
			e.Bus.Publish(b)
		}
		if persist {
			if perr := e.Store.PutMetrics(ctx, "seed", p); perr != nil {
				log.Printf("marketchaos: persist seed progress: %v", perr)
			}
		}
	})
	if err != nil {
		e.seedMu.Lock()
		e.seedProgress.Error = err.Error()
		e.seedMu.Unlock()
		e.Store.PutMetrics(ctx, "seed", e.seedProgress)
		return err
	}
	return nil
}

// SeedProgress returns the current in-memory seeding state.
func (e *Engine) SeedProgress() SeedProgress {
	e.seedMu.RLock()
	defer e.seedMu.RUnlock()
	return e.seedProgress
}

func (e *Engine) SetLevel(level LoadLevel) {
	e.level.Store(level)
}

func (e *Engine) Level() LoadLevel {
	if v, ok := e.level.Load().(LoadLevel); ok {
		return v
	}
	return LevelStop
}

func (e *Engine) Running() bool {
	return e.running.Load()
}

func (e *Engine) UptimeSeconds() int64 {
	if e.started.IsZero() {
		return 0
	}
	return int64(time.Since(e.started).Seconds())
}
