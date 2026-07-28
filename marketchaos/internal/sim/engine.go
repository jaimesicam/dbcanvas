package sim

import (
	"context"
	"encoding/json"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"marketchaos/internal/challenge"
	"marketchaos/internal/store"
)

// counters holds every atomic metric the workload agents update — the only
// path through which anything counted here becomes visible outside the
// Engine (surfaced via BuildSnapshot/metrics in a later stage; S2 just
// starts counting so that plumbing has real numbers to read).
type counters struct {
	ordersPlaced   atomic.Int64 // retail+institutional combined
	tradesExecuted atomic.Int64
	txnRetries     atomic.Int64 // deadlock/Galera-certification retries (error 1213/1205)
	agentErrors    atomic.Int64

	retailOrders        atomic.Int64
	institutionalOrders atomic.Int64
	portfolioReads      atomic.Int64
	// portfolioSummaryQueries is portfolio-n-plus-1's grading signal — see
	// agents_analytics.go's dashboardSummary — the actual SQL statement
	// count behind however many dashboard-poll events ran, since the N+1
	// bad state issues entirely different leaderboard shapes than the
	// reference JOIN and there's no shared shape ID to diff against.
	portfolioSummaryQueries atomic.Int64
}

// Engine owns the connection(s) to the target database and every background
// workload agent. MySQL is the durable, shareable view of everything it
// does — the web API and every browser client only ever read from it (via
// BuildSnapshot), never from Engine's own memory directly.
type Engine struct {
	Store       *store.Store
	Kind        TargetKind
	TargetLabel string
	Dataset     DatasetCounts
	Bus         *EventBus
	// Members holds independent connections to specific PXC cluster members
	// — nil for every target shape except a direct "pxc" cluster-frame link
	// (see pxc.go). Only the Institutional Trader agent uses it.
	Members *MemberPool
	// HAProxyStatsURL is "" unless linked through HAProxy (TARGET_KIND
	// haproxy-pxc/haproxy-mysql) — see app/marketchaos.go's
	// HAPROXY_STATS_URL env var and diagnostics.go's HAProxyStats.
	HAProxyStatsURL string

	// Securities/popCum are the fixed, deterministic 200-security universe
	// (see world.go's LoadWorld) — loaded once at construction, shared
	// read-only by every agent, indexed identically to how the seeder wrote
	// security_id = index+1.
	Securities []Security
	popCum     []float64
	// topSymbolIdx is the single most-popular security's index — used only
	// by the pxc-hot-parent-row and pxc-hot-symbol-conflict challenges'
	// institutional-trader variants (see agents_trading.go) to concentrate
	// writes beyond what natural Zipf weighting alone produces.
	topSymbolIdx int

	counters counters

	level   atomic.Value // LoadLevel
	mix     atomic.Value // WorkloadMix
	running atomic.Bool
	started time.Time

	seedMu       sync.RWMutex
	seedProgress SeedProgress
	lastPersist  time.Time

	// statsMu guards ServerStatsView's rolling baseline (see diagnostics.go)
	// — the previous point-in-time GLOBAL STATUS read, so a rate can be
	// derived without a dedicated background sampler.
	statsMu           sync.Mutex
	lastServerStats   store.ServerStats
	lastServerStatsAt time.Time

	leaderboard *leaderboard

	// gradingMu guards baseline — the one stored CaptureBaseline result a
	// subsequent ValidateSolution scores against (see grading.go).
	gradingMu sync.Mutex
	baseline  baselineState

	// Challenges is the challenge lifecycle manager — public because the API
	// layer drives it directly (start/reset/hint/diagnosis/apply-variant are
	// all thin passthroughs, see internal/api/challenge.go); grading (stage
	// S5) lives on Engine itself in grading.go, since it needs
	// leaderboard/wsrep/serverstats access Manager deliberately doesn't have.
	Challenges *challenge.Manager

	// baseCtx is the process's long-lived context — StartAgents/Reset always
	// derive from this, never from a caller's request-scoped context (a
	// Reset triggered from an HTTP handler whose r.Context() is canceled the
	// instant that response is written would otherwise kill every freshly-
	// restarted agent goroutine a moment after Reset returned "ok"; same
	// reasoning as every sibling sim's identical baseCtx field).
	baseCtx context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// pools are the 4 concurrency-sensitive (worker-pool) agents — created
	// once here and resized live by resizePools, never recreated, so Stop
	// followed by StartAgents (as Reset does) resumes from a clean 0 rather
	// than accumulating goroutines across restarts.
	pools map[string]*workerPool
}

func NewEngine(st *store.Store, kind TargetKind, targetLabel string, dataset DatasetCounts, members *MemberPool, haproxyStatsURL string) *Engine {
	securities, popCum := LoadWorld()
	topIdx := 0
	for i, s := range securities {
		if s.Popularity > securities[topIdx].Popularity {
			topIdx = i
		}
	}
	e := &Engine{
		Store: st, Kind: kind, TargetLabel: targetLabel, Dataset: dataset, Bus: NewEventBus(),
		Members: members, Securities: securities, popCum: popCum, HAProxyStatsURL: haproxyStatsURL,
		topSymbolIdx: topIdx,
		leaderboard:  newLeaderboard(), Challenges: challenge.NewManager(st.DB),
	}
	e.level.Store(LevelStop)
	e.mix.Store(MixBalanced)
	// Bound the primary pool from construction, not just from the first
	// explicit SetLevel call — Go's database/sql defaults to unlimited open
	// connections, and nothing else touches pool sizing before a user picks
	// a traffic level for the first time.
	maxOpen, maxIdle := PoolSize(kind.Family(), LevelStop)
	st.SetPoolSize(maxOpen, maxIdle)
	e.pools = map[string]*workerPool{
		"retail":        newWorkerPool(e.retailTraderLoop),
		"institutional": newWorkerPool(e.institutionalTraderLoop),
		"matching":      newWorkerPool(e.matchingEngineLoop),
		"portfolio":     newWorkerPool(e.portfolioLoop),
	}
	return e
}

// Start marks the engine up (heartbeat, uptime clock) without starting any
// workload agent — main.go calls this immediately, then StartAgents only
// once seeding has finished (see main.go's comment on why: a Large-profile
// seed can take many minutes, and agents hitting an half-seeded market would
// be at best meaningless, at worst actively confusing).
func (e *Engine) Start(ctx context.Context) {
	if e.baseCtx == nil {
		e.baseCtx = ctx
	}
	e.running.Store(true)
	e.started = time.Now()
	e.Store.Heartbeat(ctx, "system", "ok", "marketchaos started")
}

// StartAgents launches every background workload agent goroutine and sizes
// the worker pools for the current level/mix. Idempotent to call again after
// Stop (used by Reset).
func (e *Engine) StartAgents(ctx context.Context) {
	if e.baseCtx == nil {
		e.baseCtx = ctx
	}
	e.ctx, e.cancel = context.WithCancel(e.baseCtx)
	e.running.Store(true)

	rateAgents := []func(context.Context){
		e.runMarketDataAgent,
		e.runNewsAgent,
		e.runScannerAgent,
		e.runComplianceAgent,
		e.runCleanupAgent,
		e.runDashboardPollAgent,
		e.runLeaderboardSampler,
	}
	for _, fn := range rateAgents {
		e.wg.Add(1)
		go func(f func(context.Context)) {
			defer e.wg.Done()
			f(e.ctx)
		}(fn)
	}
	e.resizePools()
	log.Printf("marketchaos: agents started (kind=%s family=%s mix=%s level=%s members=%d)",
		e.Kind, e.Kind.Family(), e.Mix(), e.Level(), e.Members.Len())
}

// Stop cancels every agent goroutine (rate-driven and worker-pool alike —
// pool workers derive their context from e.ctx, so canceling it cascades to
// them too) and waits for all of them to exit.
func (e *Engine) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
	e.wg.Wait()
	for _, p := range e.pools {
		p.Stop()
	}
}

func (e *Engine) Pause()  { e.running.Store(false) }
func (e *Engine) Resume() { e.running.Store(true) }

// Reset stops every agent, wipes every table this app owns, re-seeds with
// the same dataset counts the node was deployed with, then starts fresh —
// the same "wipe, then rebuild the static/starting world" shape every
// sibling sim's Reset follows. Takes no meaningful use of ctx for the
// restart itself: agents always resume from baseCtx (see StartAgents),
// never a caller-supplied request context.
func (e *Engine) Reset(ctx context.Context) error {
	e.Stop()
	e.counters = counters{}

	if err := store.Wipe(ctx, e.Store); err != nil {
		return err
	}
	if err := e.RunSeed(ctx); err != nil {
		return err
	}
	e.StartAgents(e.baseCtx)
	return nil
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

// SetLevel changes the simulated traffic level, resizes the MySQL connection
// pool to match (see PoolSize — deliberately not equal to the worker count),
// and resizes every worker-pool agent for the new level x mix combination.
func (e *Engine) SetLevel(level LoadLevel) {
	e.level.Store(level)
	maxOpen, maxIdle := PoolSize(e.Kind.Family(), level)
	e.Store.SetPoolSize(maxOpen, maxIdle)
	e.resizePools()
}

func (e *Engine) Level() LoadLevel {
	if v, ok := e.level.Load().(LoadLevel); ok {
		return v
	}
	return LevelStop
}

// SetMix changes which kind of traffic the current level produces and
// re-sizes the worker pools accordingly (rate-driven agents just read the
// new mix on their next tick — no resize needed for those).
func (e *Engine) SetMix(mix WorkloadMix) {
	e.mix.Store(mix)
	e.resizePools()
}

func (e *Engine) Mix() WorkloadMix {
	if v, ok := e.mix.Load().(WorkloadMix); ok {
		return v
	}
	return MixBalanced
}

// resizePools splits WorkerCounts[level] across the 4 concurrency-sensitive
// agents by the current mix's pool shares. A no-op before StartAgents has
// run once (e.ctx is nil — nothing to parent the pool workers' contexts to
// yet); StartAgents calls this itself once it has set e.ctx.
func (e *Engine) resizePools() {
	if e.ctx == nil {
		return
	}
	shares := sharesFor(e.Mix())
	total := WorkerCounts[e.Level()]
	poolTotal := shares.poolTotal()
	sizeFor := func(w float64) int {
		if poolTotal <= 0 {
			return 0
		}
		return int(float64(total) * w / poolTotal)
	}
	e.pools["retail"].Resize(e.ctx, sizeFor(shares.RetailTrader))
	e.pools["institutional"].Resize(e.ctx, sizeFor(shares.InstitutionalTrader))
	e.pools["matching"].Resize(e.ctx, sizeFor(shares.MatchingEngine))
	e.pools["portfolio"].Resize(e.ctx, sizeFor(shares.Portfolio))
}

// agentRate returns a rate-driven agent's own ops/sec budget: the target
// family's total rate-agent budget for the current level, split by the
// current mix's rate shares.
func (e *Engine) agentRate(weight float64) float64 {
	shares := sharesFor(e.Mix())
	rt := shares.rateTotal()
	if rt <= 0 {
		return 0
	}
	total := rateOpsPerSecond[e.Kind.Family()][e.Level()]
	return total * weight / rt
}

// opsThisTick converts a per-second rate into a per-tick batch size, so the
// same tick interval produces proportionally different throughput at every
// traffic level.
func opsThisTick(ratePerSec float64, interval time.Duration) int {
	n := int(ratePerSec * interval.Seconds())
	if n < 1 && ratePerSec > 0 {
		n = 1
	}
	return n
}

// randAccountID picks a random account/trader id in [1, Dataset.Traders] —
// accounts and traders share the same 1:1 auto-increment range (see seed.go:
// "trader_id N maps to trader row N"), so every agent that needs either
// picks from this one range. Returns 0 (never a valid id) if the dataset
// somehow has no traders, so callers can bail out instead of panicking on
// rand.Intn(0).
func (e *Engine) randAccountID(rng *rand.Rand) int {
	if e.Dataset.Traders <= 0 {
		return 0
	}
	return 1 + rng.Intn(e.Dataset.Traders)
}

// ActiveVariant is the short form agent code actually calls — see
// challenge.Manager.ActiveVariantID's doc comment. Returns "" almost always
// (no challenge active, or a DB-mechanism one, or an app-mechanism one
// whose fix has already been applied) — every app-variant branch in the
// agent files is `if e.ActiveVariant() == "some-challenge-id" { bad } else
// { reference }`.
func (e *Engine) ActiveVariant() string {
	return e.Challenges.ActiveVariantID()
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
