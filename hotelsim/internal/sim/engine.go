package sim

import (
	"context"
	"encoding/json"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"hotelsim/internal/store"
)

// counters holds every atomic metric the analytics agent flushes into the
// metrics/_id:"current" document — the only path through which anything counted
// here becomes visible to BuildSnapshot (which never reads Engine's memory
// directly; see snapshot.go).
type counters struct {
	soldOut            atomic.Int64
	duplicatesRejected atomic.Int64
	writeConflicts     atomic.Int64
	compensations      atomic.Int64
	eventWriteErrors   atomic.Int64
	reservationsTotal  atomic.Int64
	cancellationsTotal atomic.Int64
	modificationsTotal atomic.Int64
	checkInsTotal      atomic.Int64
	checkOutsTotal     atomic.Int64
	noShowsTotal       atomic.Int64
	searchesTotal      atomic.Int64
}

// Engine owns all live simulation state in memory and drives the ten background
// agent goroutines. MongoDB is the durable, shareable view of this state — the
// web API and every browser client only ever read from MongoDB (via
// BuildSnapshot), never from Engine directly.
type Engine struct {
	Store       *store.Store
	World       *World
	Topology    store.Topology
	Profile     Profile
	Clock       *SimClock
	Bus         *EventBus
	TargetLabel string

	booker Booker

	mu         sync.RWMutex
	sessions   map[string]*GuestSession
	recentRes  []ResRef // bounded ring buffer
	nextResSeq int64

	counters counters

	level     atomic.Value // LoadLevel
	running   atomic.Bool
	startedAt time.Time

	// baseCtx is the process's long-lived context — Start/Reset always derive
	// from this, never from a caller's request-scoped context. Start latches it
	// on the first call only; every subsequent Start/Reset re-derives e.ctx via
	// context.WithCancel(e.baseCtx). Reset is triggered from an HTTP handler,
	// whose r.Context() is canceled the instant that response is written —
	// deriving from it would silently kill every freshly-restarted agent
	// goroutine a moment after Reset returned "ok".
	baseCtx context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

const recentResCap = 500

// NewEngine constructs the engine for an already-detected topology. Schema/index/
// shard-key setup (store.EnsureSchema) must already have run — that's a one-time
// startup step, not something Start/Reset repeats.
func NewEngine(st *store.Store, topo store.Topology, targetLabel string) *Engine {
	e := &Engine{
		Store: st, World: NewWorld(), Topology: topo, Profile: NewProfile(topo),
		Bus: NewEventBus(), TargetLabel: targetLabel,
		sessions: map[string]*GuestSession{},
	}
	e.Clock = NewSimClock(time.Now().UTC())
	if e.Profile.Transactions {
		e.booker = &txnBooker{e: e}
	} else {
		e.booker = &guardedBooker{e: e}
	}
	e.level.Store(LevelLow)
	e.running.Store(true)
	e.startedAt = time.Now()
	return e
}

func (e *Engine) Level() LoadLevel     { return e.level.Load().(LoadLevel) }
func (e *Engine) Running() bool        { return e.running.Load() }
func (e *Engine) UptimeSeconds() int64 { return int64(time.Since(e.startedAt).Seconds()) }

func (e *Engine) SetLevel(l LoadLevel) {
	e.level.Store(l)
}

// Start launches every background agent goroutine, restores the simulated clock
// from its last checkpoint (or anchors a fresh one), and seeds the static hotel
// topology into MongoDB if this is a brand-new database. Idempotent to call again
// after Stop.
//
// ctx is remembered as baseCtx the first time Start is called; see the Engine
// struct comment.
func (e *Engine) Start(ctx context.Context) {
	if e.baseCtx == nil {
		e.baseCtx = ctx
	}
	e.ctx, e.cancel = context.WithCancel(e.baseCtx)
	e.running.Store(true)

	e.restoreOrAnchorClock(e.ctx)
	e.seedIfEmpty(e.ctx)

	agents := []func(context.Context){
		e.runGuestSearchAgent,
		e.runReservationAgent,
		e.runModificationAgent,
		e.runCancellationAgent,
		e.runCheckInAgent,
		e.runCheckOutAgent,
		e.runRateAgent,
		e.runHotelOpsAgent,
		e.runAnalyticsAgent,
		e.runMonitoringAgent,
		e.runEventFeed, // change-stream watcher or poller, chosen by Profile.ChangeStreams
		e.runClockPersister,
	}
	for _, fn := range agents {
		e.wg.Add(1)
		go func(f func(context.Context)) {
			defer e.wg.Done()
			f(e.ctx)
		}(fn)
	}
	log.Printf("hotelsim: engine started (topology=%s, hotels=%d, scope=%d)", e.Topology, len(e.World.Hotels), e.Profile.HotelScope)
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

// Reset stops every agent, wipes every collection this app owns, clears
// in-memory state, re-anchors the simulated clock to "now", and starts fresh —
// restoring the exact known starting condition spec §29's Repeatability
// requirement calls for. Takes no context: always restarts from baseCtx (see
// Start), never a caller-supplied one.
func (e *Engine) Reset() {
	e.Stop()

	e.mu.Lock()
	e.sessions = map[string]*GuestSession{}
	e.recentRes = nil
	e.nextResSeq = 0
	e.mu.Unlock()
	e.counters = counters{}

	if err := store.Wipe(e.baseCtx, e.Store); err != nil {
		log.Printf("hotelsim: reset wipe: %v", err)
	}
	e.Clock.ResetToday()
	e.persistClock(e.baseCtx)

	e.Start(e.baseCtx)
}

// --- small in-memory bookkeeping helpers shared by agents ---

func (e *Engine) addSession(s *GuestSession) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sessions[s.ID] = s
}

func (e *Engine) removeSession(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.sessions, id)
}

func (e *Engine) sessionsByStage(stage string) []*GuestSession {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []*GuestSession
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

// nextConfirmationSeq atomically increments the in-memory reservation sequence
// counter used by NewConfirmationNumber. Seeded from a persisted high-water mark
// at startup (see seedIfEmpty / restoreOrAnchorClock) so restarts never reuse a
// confirmation number.
func (e *Engine) nextConfirmationSeq() int64 {
	return atomic.AddInt64(&e.nextResSeq, 1)
}

// PublishEvent inserts a durable reservationEvents document and best-effort fans
// the same payload out over the in-process EventBus for anyone currently
// connected — the live push is a convenience, never the only path a client can
// rely on (see bus.go).
func (e *Engine) PublishEvent(ctx context.Context, kind, reservationID, hotelID, hotelName, agent, detail string) {
	ev, err := e.Store.AppendEvent(ctx, store.Event{
		Kind: kind, ReservationID: reservationID, HotelID: hotelID, HotelName: hotelName,
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

// simStateClockDoc / simStateResSeqDoc are the simstate/_id documents the clock
// and confirmation-number sequence checkpoint themselves into.
type simStateClockDoc struct {
	ID     string    `bson:"_id"`
	SimNow time.Time `bson:"simNow"`
	Rate   float64   `bson:"rate"`
}

// restoreOrAnchorClock re-anchors the simulated clock from its last persisted
// checkpoint, and seeds the confirmation-number sequence from the reservations
// collection's current size. Critically, on restart this does NOT advance
// simulated time by however long the process was down — it resumes at exactly
// the simulated instant it left off, anchored to the current wall clock. Wall-
// time-based catch-up (jumping simulated time forward by real downtime) would,
// after any nontrivial outage, walk simulated "today" past the entire inventory
// horizon and instantly no-show every open reservation.
func (e *Engine) restoreOrAnchorClock(ctx context.Context) {
	var doc simStateClockDoc
	err := e.Store.Coll(store.CollSimState).FindOne(ctx, bson.M{"_id": "clock"}).Decode(&doc)
	switch {
	case err == nil:
		e.Clock.Anchor(doc.SimNow)
	case err == mongo.ErrNoDocuments:
		e.Clock.ResetToday()
	default:
		log.Printf("hotelsim: restore clock: %v (anchoring fresh)", err)
		e.Clock.ResetToday()
	}

	count, err := e.Store.Coll(store.CollReservations).EstimatedDocumentCount(ctx)
	if err != nil {
		count = 0
	}
	atomic.StoreInt64(&e.nextResSeq, count+100000)
}

// persistClock checkpoints the simulated clock to simstate so a restart resumes
// from here rather than a stale or zero value. Called once at Start and
// periodically by runClockPersister.
func (e *Engine) persistClock(ctx context.Context) {
	_, err := e.Store.Coll(store.CollSimState).UpdateOne(ctx,
		bson.M{"_id": "clock"},
		bson.M{"$set": simStateClockDoc{ID: "clock", SimNow: e.Clock.Now(), Rate: e.Clock.Rate()}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		log.Printf("hotelsim: persist clock: %v", err)
	}
}
