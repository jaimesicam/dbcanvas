package sim

import (
	"context"
	"sync/atomic"
	"time"

	"marketchaos/internal/store"
)

// Engine owns the connection to the target database and (from stage S2
// onward) every background workload agent. As of stage S0 it has no agents
// at all — Start/Stop/Pause/Resume/Reset exist as the shape future stages
// fill in, and BuildSnapshot reports only connection/control state, enough
// for the dashboard to prove the node is alive and talking to its target.
type Engine struct {
	Store       *store.Store
	Kind        TargetKind
	TargetLabel string
	Bus         *EventBus

	level   atomic.Value // LoadLevel
	running atomic.Bool
	started time.Time
}

func NewEngine(st *store.Store, kind TargetKind, targetLabel string) *Engine {
	e := &Engine{Store: st, Kind: kind, TargetLabel: targetLabel, Bus: NewEventBus()}
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

// Reset truncates every owned table. Stage S1 adds re-seeding immediately
// afterward, the way every sibling sim's Reset re-seeds its static topology
// once Wipe finishes.
func (e *Engine) Reset(ctx context.Context) error {
	return store.Wipe(ctx, e.Store)
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
