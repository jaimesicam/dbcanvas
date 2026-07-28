package sim

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"airlinesim/internal/store"
)

// counters holds every atomic metric the analytics agent flushes into the
// metrics/id:"current" row — the only path through which anything counted here
// becomes visible to BuildSnapshot (which never reads Engine's memory directly; see
// snapshot.go).
type counters struct {
	soldOut            atomic.Int64
	duplicatesRejected atomic.Int64
	txnRetries         atomic.Int64 // deadlock/Galera-certification retries (error 1213)
	eventWriteErrors   atomic.Int64
	reservationsTotal  atomic.Int64
	cancellationsTotal atomic.Int64
	modificationsTotal atomic.Int64
	checkInsTotal      atomic.Int64
	completionsTotal   atomic.Int64
	noShowsTotal       atomic.Int64
	searchesTotal      atomic.Int64
}

// Engine owns all live simulation state in memory and drives the ten background
// agent goroutines. MySQL is the durable, shareable view of this state — the web API
// and every browser client only ever read from it (via BuildSnapshot), never from
// Engine directly.
type Engine struct {
	Store       *store.Store
	World       *World
	Fleet       []*Aircraft
	Kind        TargetKind
	Profile     Profile
	Clock       *SimClock
	Bus         *EventBus
	TargetLabel string

	booker *sqlBooker

	mu         sync.RWMutex
	sessions   map[string]*PassengerSession
	recentRes  []ResRef // bounded ring buffer
	nextResSeq int64
	statusOf   map[string]AircraftStatus // tail number -> current status

	counters counters

	level     atomic.Value // LoadLevel
	running   atomic.Bool
	startedAt time.Time

	// baseCtx is the process's long-lived context — Start/Reset always derive from
	// this, never from a caller's request-scoped context. See Hotel Sim's identical
	// comment on this exact field: a Reset triggered from an HTTP handler whose
	// r.Context() is canceled the instant that response is written would silently
	// kill every freshly-restarted agent goroutine a moment after Reset returned "ok".
	baseCtx context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

const recentResCap = 500

// NewEngine constructs the engine for an already-resolved target kind. Schema setup
// (store.EnsureSchema) must already have run — that's a one-time startup step, not
// something Start/Reset repeats.
func NewEngine(st *store.Store, kind TargetKind, targetLabel string) *Engine {
	world := NewWorld()
	fleet := GenerateFleet(world)
	statusOf := make(map[string]AircraftStatus, len(fleet))
	for _, ac := range fleet {
		statusOf[ac.TailNumber] = ac.Status
	}
	e := &Engine{
		Store: st, World: world, Fleet: fleet, Kind: kind, Profile: NewProfile(kind),
		Bus: NewEventBus(), TargetLabel: targetLabel,
		sessions: map[string]*PassengerSession{}, statusOf: statusOf,
	}
	e.Clock = NewSimClock(time.Now().UTC())
	e.booker = &sqlBooker{e: e}
	e.running.Store(true)
	e.startedAt = time.Now()
	e.SetLevel(LevelLow)
	return e
}

func (e *Engine) Level() LoadLevel     { return e.level.Load().(LoadLevel) }
func (e *Engine) Running() bool        { return e.running.Load() }
func (e *Engine) UptimeSeconds() int64 { return int64(time.Since(e.startedAt).Seconds()) }

// SetLevel changes the simulated load level and resizes the MySQL connection
// pool to match (see Profile.PoolSize) — a jump to High needs materially more
// concurrent connections than Stop/Low, and the reverse shouldn't leave a
// bloated pool open against a lab-sized cluster indefinitely.
func (e *Engine) SetLevel(l LoadLevel) {
	e.level.Store(l)
	maxOpen, maxIdle := e.Profile.PoolSize(l)
	e.Store.SetPoolSize(maxOpen, maxIdle)
}

// Start launches every background agent goroutine, restores the simulated clock from
// its last checkpoint (or anchors a fresh one), and seeds the static route/fleet
// topology into MySQL if this is a brand-new schema. Idempotent to call again after
// Stop.
//
// ctx is remembered as baseCtx the first time Start is called; see the Engine struct
// comment.
func (e *Engine) Start(ctx context.Context) {
	if e.baseCtx == nil {
		e.baseCtx = ctx
	}
	e.ctx, e.cancel = context.WithCancel(e.baseCtx)
	e.running.Store(true)

	e.restoreOrAnchorClock(e.ctx)
	e.seedIfEmpty(e.ctx)

	agents := []func(context.Context){
		e.runRouteSearchAgent,
		e.runBookingAgent,
		e.runModificationAgent,
		e.runCancellationAgent,
		e.runCheckInAgent,
		e.runFlightCompletionAgent,
		e.runFarePricingAgent,
		e.runFleetOpsAgent,
		e.runAnalyticsAgent,
		e.runMonitoringAgent,
		e.runEventFeed,
		e.runClockPersister,
	}
	for _, fn := range agents {
		e.wg.Add(1)
		go func(f func(context.Context)) {
			defer e.wg.Done()
			f(e.ctx)
		}(fn)
	}
	log.Printf("airlinesim: engine started (kind=%s, routes=%d, aircraft=%d, scope=%d)", e.Kind, len(e.World.Routes), len(e.Fleet), e.Profile.RouteScope)
}

// Stop cancels every agent goroutine and waits for them to exit.
func (e *Engine) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
	e.wg.Wait()
}

func (e *Engine) Pause()  { e.running.Store(false) }
func (e *Engine) Resume() { e.running.Store(true) }

// Reset stops every agent, wipes every table this app owns, clears in-memory state,
// re-anchors the simulated clock to "now", and starts fresh. Takes no context:
// always restarts from baseCtx (see Start), never a caller-supplied one.
func (e *Engine) Reset() {
	e.Stop()

	e.mu.Lock()
	e.sessions = map[string]*PassengerSession{}
	e.recentRes = nil
	e.nextResSeq = 0
	for _, ac := range e.Fleet {
		e.statusOf[ac.TailNumber] = ac.Status
	}
	e.mu.Unlock()
	e.counters = counters{}

	if err := store.Wipe(e.baseCtx, e.Store); err != nil {
		log.Printf("airlinesim: reset wipe: %v", err)
	}
	e.Clock.ResetToday()
	e.persistClock(e.baseCtx)

	e.Start(e.baseCtx)
}

// --- small in-memory bookkeeping helpers shared by agents ---

func (e *Engine) addSession(s *PassengerSession) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sessions[s.ID] = s
}

func (e *Engine) removeSession(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.sessions, id)
}

func (e *Engine) sessionsByStage(stage string) []*PassengerSession {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []*PassengerSession
	for _, s := range e.sessions {
		if s.Stage == stage {
			out = append(out, s)
		}
	}
	return out
}

func (e *Engine) sessionCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.sessions)
}

func (e *Engine) rememberReservation(ref ResRef) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recentRes = append(e.recentRes, ref)
	if len(e.recentRes) > recentResCap {
		e.recentRes = e.recentRes[len(e.recentRes)-recentResCap:]
	}
}

// pickRecentReservation returns a random recent reservation ref for a targeted
// follow-up (modify/cancel/check-in candidate), or ok=false if none exist yet.
func (e *Engine) pickRecentReservation(rng *rand.Rand) (ResRef, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(e.recentRes) == 0 {
		return ResRef{}, false
	}
	return e.recentRes[rng.Intn(len(e.recentRes))], true
}

// aircraftStatus returns a tail number's current in-memory status.
func (e *Engine) aircraftStatus(tail string) AircraftStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.statusOf[tail]
}

// setAircraftStatus updates a tail number's in-memory status (fleet-ops agent).
func (e *Engine) setAircraftStatus(tail string, status AircraftStatus) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.statusOf[tail] = status
}

// statusSnapshot returns a copy of the full tail->status map, for the fleet panel.
func (e *Engine) statusSnapshot() map[string]AircraftStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]AircraftStatus, len(e.statusOf))
	for k, v := range e.statusOf {
		out[k] = v
	}
	return out
}

// pickTailForDate resolves which tail number flies a route on a given date from the
// route's pool, honoring current in-memory aircraft status.
func (e *Engine) pickTailForDate(route *Route, date time.Time) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	dayOrdinal := int(date.Unix() / 86400)
	return PickAvailableTail(route.AircraftPool, e.statusOf, dayOrdinal)
}

// nextConfirmationSeq atomically increments the in-memory reservation sequence
// counter used by NewConfirmationNumber. Seeded from a persisted high-water mark at
// startup (see restoreOrAnchorClock) so restarts never reuse a confirmation number.
func (e *Engine) nextConfirmationSeq() int64 {
	return atomic.AddInt64(&e.nextResSeq, 1)
}

// PublishEvent inserts a durable reservation_events row and best-effort fans the
// same payload out over the in-process EventBus for anyone currently connected — the
// live push is a convenience, never the only path a client can rely on (see bus.go).
func (e *Engine) PublishEvent(ctx context.Context, kind, reservationID, routeID string, flightDate time.Time, agent, detail string) {
	ev, err := e.Store.AppendEvent(ctx, store.Event{
		Kind: kind, ReservationID: reservationID, RouteID: routeID, FlightDate: flightDate,
		Agent: agent, Detail: detail, SimAt: e.Clock.Now(),
	})
	if err != nil {
		e.counters.eventWriteErrors.Add(1)
		return
	}
	if payload, mErr := json.Marshal(ev); mErr == nil {
		e.Bus.Publish(payload)
	}
}

// simClockState is the sim_state/id:"clock" row's JSON payload.
type simClockState struct {
	SimNow time.Time `json:"simNow"`
	Rate   float64   `json:"rate"`
}

// restoreOrAnchorClock re-anchors the simulated clock from its last persisted
// checkpoint, and seeds the confirmation-number sequence from the reservations
// table's current size. Critically, on restart this does NOT advance simulated time
// by however long the process was down — it resumes at exactly the simulated instant
// it left off, anchored to the current wall clock.
func (e *Engine) restoreOrAnchorClock(ctx context.Context) {
	var payload string
	err := e.Store.DB.QueryRowContext(ctx, "SELECT payload FROM sim_state WHERE id='clock'").Scan(&payload)
	switch {
	case err == nil:
		var st simClockState
		if jerr := json.Unmarshal([]byte(payload), &st); jerr == nil {
			e.Clock.Anchor(st.SimNow)
		} else {
			e.Clock.ResetToday()
		}
	case err == sql.ErrNoRows:
		e.Clock.ResetToday()
	default:
		log.Printf("airlinesim: restore clock: %v (anchoring fresh)", err)
		e.Clock.ResetToday()
	}

	var count int64
	if qerr := e.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM reservations").Scan(&count); qerr != nil {
		count = 0
	}
	atomic.StoreInt64(&e.nextResSeq, count+100000)
}

// persistClock checkpoints the simulated clock to sim_state so a restart resumes
// from here rather than a stale or zero value. Called once at Start and periodically
// by runClockPersister.
func (e *Engine) persistClock(ctx context.Context) {
	b, _ := json.Marshal(simClockState{SimNow: e.Clock.Now(), Rate: e.Clock.Rate()})
	_, err := e.Store.DB.ExecContext(ctx,
		"INSERT INTO sim_state (id, payload, updated_at) VALUES ('clock', ?, ?) ON DUPLICATE KEY UPDATE payload=VALUES(payload), updated_at=VALUES(updated_at)",
		string(b), time.Now().UTC())
	if err != nil {
		log.Printf("airlinesim: persist clock: %v", err)
	}
}
