package sim

import (
	"sync"
	"time"
)

// health.go — is this app actually working?
//
// The dashboard could already say a great deal about what the simulation is *doing* — rows
// written, working-set reads per second, how big the dataset has grown — and almost nothing
// about whether any of it is still happening. That gap is not academic. When the write endpoint
// became a read-only standby after a repmgr switchover, every write failed, every read kept
// working, the page kept drawing a live-looking market, and the only sign was a counter nobody
// surfaced and a banner with no timestamp on it.
//
// So this is the small set of numbers that answer "is it having issues", in the order you would
// ask them:
//
//   - **writes/s and reads/s** — the two rates. Writes are the ones that stop first, because a
//     standby serves reads perfectly well. Writes here means the SIMULATION's writes; backfill
//     is reported beside them, never folded in — see WritesPerSec for what that cost.
//   - **stalled** — running, but nothing written for a while. This is the signal the switchover
//     case needed: it is true within seconds, whatever the cause, and it does not depend on an
//     error having been recorded at all.
//   - **errors** — how many since they were last cleared, and how long ago the last one was.
//
// The rates are sampled the way workingset.go already samples its own: deltas between polls
// rather than an average since start-up, so a rate that has dropped to zero reads as zero
// instead of decaying slowly towards it and looking like a slowdown.

// healthSampleEvery is how often the rates are recomputed. Matched to the dashboard's own poll,
// so a number that appears on the page is never much older than the page.
const healthSampleEvery = 2 * time.Second

// healthStallAfter is how long the engine can run without completing a write before the page
// says it is stalled.
//
// Generous on purpose. At the lowest load level the order agent writes a few times a second, but
// backfill and retention work in bursts with quiet gaps between them, and a threshold tight
// enough to catch a stall instantly would spend its life crying wolf on an idle-but-healthy
// simulation. Ten seconds is far longer than any legitimate gap and far shorter than the five
// minutes it used to take anyone to notice.
const healthStallAfter = 10 * time.Second

// HealthStatus is the app's own health, as opposed to the database's.
type HealthStatus struct {
	// The two rates, over the last sample window.
	//
	// WritesPerSec is the SIMULATION's writes — ticks from the price agent, orders created and
	// filled. Backfill is deliberately not in it: it is a bulk loader that manufactures history
	// at six figures a second and then stops, so counting it here did two harmful things at
	// once. It buried the simulation (of 5.86M writes on a real deployment, 5.85M were
	// backfill's), and it made the number crash by four orders of magnitude when backfill
	// finished normally — which looks exactly like a failure and is not one.
	WritesPerSec float64 `json:"writesPerSec"`
	ReadsPerSec  float64 `json:"readsPerSec"`
	// BackfillRowsPerSec is that bulk stream, reported separately so the big number is still
	// visible without being mistaken for the health of the simulation.
	BackfillRowsPerSec float64 `json:"backfillRowsPerSec"`
	// Totals since the last reset, so a rate of zero can be told apart from having never done
	// anything at all.
	Writes int64 `json:"writes"`
	Reads  int64 `json:"reads"`
	// Stalled is the headline: the engine is running and has not completed a write in
	// healthStallAfter. StalledForSec says how long, so the page can distinguish "just now"
	// from "since you went to lunch".
	Stalled       bool  `json:"stalled"`
	StalledForSec int64 `json:"stalledForSeconds"`
	// LastWriteAgoSec is how long since the last successful write, reported whether or not it
	// has crossed the stall threshold — it is the number that moves first.
	LastWriteAgoSec int64 `json:"lastWriteAgoSeconds"`
	// Errors since they were last cleared, and the most recent one. LastError is nil once
	// cleared, which is what takes the banner off the page.
	Errors    int64      `json:"errors"`
	LastError *ErrorInfo `json:"lastError,omitempty"`
}

// healthSampler turns the engine's monotonic counters into rates. One instance per engine, read
// under its own lock so a dashboard poll never blocks an agent.
type healthSampler struct {
	mu sync.Mutex
	// lastAt is when the previous sample was taken, and last{Writes,Reads} the counter values
	// then — the deltas between two samples are the rates.
	lastAt              time.Time
	lastWrites          int64
	lastReads           int64
	lastBackfill        int64
	writesPerSec        float64
	readsPerSec         float64
	backfillPerSec      float64
	lastProgressAt      time.Time // when the simulation's write counter last moved
	writesAtLastSampleN int64
}

// sample recomputes the rates from the current counter totals. Called on every snapshot, and
// cheap enough to be: two subtractions and a division, guarded so two polls arriving together
// cannot divide by a zero interval.
func (h *healthSampler) sample(writes, reads, backfill int64, running bool) (wps, rps, bps float64, lastProgress time.Time) {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.lastAt.IsZero() {
		h.lastAt, h.lastWrites, h.lastReads, h.lastBackfill = now, writes, reads, backfill
		h.lastProgressAt = now
		h.writesAtLastSampleN = writes
		return 0, 0, 0, h.lastProgressAt
	}
	// Any movement in the write counter is progress, recorded whether or not the sample window
	// has elapsed — the stall clock must not be quantised to the rate window.
	if writes > h.writesAtLastSampleN {
		h.lastProgressAt = now
	}
	h.writesAtLastSampleN = writes
	// A paused engine is not stalled: it was asked to stop. Keep the clock moving with it so
	// resuming does not immediately report a stall that was really a pause.
	if !running {
		h.lastProgressAt = now
	}

	if elapsed := now.Sub(h.lastAt).Seconds(); elapsed >= healthSampleEvery.Seconds() {
		h.writesPerSec = float64(writes-h.lastWrites) / elapsed
		h.readsPerSec = float64(reads-h.lastReads) / elapsed
		h.backfillPerSec = float64(backfill-h.lastBackfill) / elapsed
		h.lastAt, h.lastWrites, h.lastReads, h.lastBackfill = now, writes, reads, backfill
	}
	return h.writesPerSec, h.readsPerSec, h.backfillPerSec, h.lastProgressAt
}

// Health is the engine's own health, for the dashboard.
//
// Writes are what the SIMULATION completed: ticks from the price agent, and orders created and
// filled. Each of those counters only advances after the store call returned without error, which
// is what makes the rate falling to zero mean something rather than merely being quiet.
//
// Backfill is counted apart, for the reason on BackfillRowsPerSec: it is bulk history, it runs
// orders of magnitude faster than the simulation, and folding it in hid both the simulation's own
// health and the stall this panel exists to catch.
func (e *Engine) Health() HealthStatus {
	c := &e.counters
	writes := c.ticksWritten.Load() + c.ordersCreated.Load() + c.ordersFilled.Load()
	backfill := c.ticksBackfilled.Load()
	reads := c.wsReads.Load()
	running := e.Running()

	wps, rps, bps, lastProgress := e.health.sample(writes, reads, backfill, running)

	hs := HealthStatus{
		WritesPerSec: round2(wps), ReadsPerSec: round2(rps), BackfillRowsPerSec: round2(bps),
		Writes: writes, Reads: reads,
		Errors: c.errors.Load(), LastError: e.lastError(),
	}
	if !lastProgress.IsZero() {
		since := time.Since(lastProgress)
		hs.LastWriteAgoSec = int64(since.Seconds())
		// Only while running: a paused simulation has not written for as long as it has been
		// paused, and saying so would be true and useless.
		if running && since >= healthStallAfter {
			hs.Stalled = true
			hs.StalledForSec = int64(since.Seconds())
		}
	}
	return hs
}
