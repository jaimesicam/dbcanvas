package sim

import (
	"testing"
	"time"
)

// The stall clock is the signal that catches a write endpoint which has quietly become a
// read-only standby — the case where reads keep working and nothing else on the page changes.
// It must move on the first write, not on the first rate sample, or the threshold would be
// quantised to the sample window.
func TestHealthSamplerStallClock(t *testing.T) {
	var h healthSampler

	// First call primes the sampler and cannot report a rate — there is no previous sample to
	// take a delta against.
	wps, rps, _, progress := h.sample(0, 0, 0, true)
	if wps != 0 || rps != 0 {
		t.Errorf("first sample must report no rate, got %v/%v", wps, rps)
	}
	if progress.IsZero() {
		t.Error("the stall clock must start when the sampler does")
	}

	// A write moves the clock even though no sample window has elapsed.
	h.lastProgressAt = time.Now().Add(-time.Minute)
	_, _, _, progress = h.sample(1, 0, 0, true)
	if time.Since(progress) > time.Second {
		t.Errorf("a completed write must reset the stall clock, got %s ago", time.Since(progress))
	}

	// No movement leaves it where it was, which is what lets it age past the threshold.
	old := progress
	_, _, _, progress = h.sample(1, 5, 0, true)
	if !progress.Equal(old) {
		t.Error("reads alone must not count as write progress")
	}

	// A paused engine is not a stalled one: it was asked to stop.
	h.lastProgressAt = time.Now().Add(-time.Hour)
	_, _, _, progress = h.sample(1, 5, 0, false)
	if time.Since(progress) > time.Second {
		t.Error("a paused engine must not accumulate stall time")
	}
}

// Rates are deltas between samples, not averages since start-up: a rate that has dropped to zero
// has to read as zero rather than decaying towards it, or a stall looks like a slowdown for
// minutes.
func TestHealthSamplerRatesAreDeltas(t *testing.T) {
	var h healthSampler
	h.sample(0, 0, 0, true)
	// Backdate the previous sample so the window has elapsed.
	h.lastAt = time.Now().Add(-2 * healthSampleEvery)
	wps, rps, _, _ := h.sample(int64(20*healthSampleEvery.Seconds()*2), int64(10*healthSampleEvery.Seconds()*2), 0, true)
	if wps < 18 || wps > 22 {
		t.Errorf("writes/s = %v, want ~20", wps)
	}
	if rps < 8 || rps > 12 {
		t.Errorf("reads/s = %v, want ~10", rps)
	}

	// Counters stop moving → the next window reports zero, not the earlier rate.
	h.lastAt = time.Now().Add(-2 * healthSampleEvery)
	wps, rps, _, _ = h.sample(int64(20*healthSampleEvery.Seconds()*2), int64(10*healthSampleEvery.Seconds()*2), 0, true)
	if wps != 0 || rps != 0 {
		t.Errorf("a stopped counter must report 0/s, got %v/%v", wps, rps)
	}
}

// An error carries when it last happened and how often, so a blip at start-up can be told from
// a fault happening right now — and clearing it is what takes the banner off the page.
func TestEngineErrorRecordAndClear(t *testing.T) {
	e := &Engine{}
	if e.lastError() != nil {
		t.Fatal("a fresh engine has no error")
	}

	e.noteErr("backfill: measure", errString("connection refused"))
	first := e.lastError()
	if first == nil || first.Count != 1 || first.Where != "backfill: measure" {
		t.Fatalf("first error = %+v", first)
	}
	if first.At.IsZero() || !first.At.Equal(first.FirstAt) {
		t.Error("a first occurrence has one timestamp for both ends of the episode")
	}

	// The same failure repeating is one episode, counted — not a new entry each time, which
	// would lose when it started.
	e.noteErr("backfill: measure", errString("connection refused"))
	again := e.lastError()
	if again.Count != 2 {
		t.Errorf("repeat count = %d, want 2", again.Count)
	}
	if !again.FirstAt.Equal(first.FirstAt) {
		t.Error("a repeat must keep the episode's start time")
	}

	// A different failure is a new episode: the newest problem is the one worth reading.
	e.noteErr("price: apply quotes", errString("read-only transaction"))
	newer := e.lastError()
	if newer.Count != 1 || newer.Where != "price: apply quotes" {
		t.Errorf("a different error must start a new episode, got %+v", newer)
	}

	// lastError hands back a copy — a caller must not be able to edit engine state.
	newer.Message = "mutated"
	if e.lastError().Message == "mutated" {
		t.Error("lastError must return a copy")
	}

	e.ClearErrors()
	if e.lastError() != nil {
		t.Error("ClearErrors must dismiss the recorded failure")
	}
	if got := e.counters.errors.Load(); got != 0 {
		t.Errorf("ClearErrors must zero the counter, got %d", got)
	}
	// And a fault that is still happening comes straight back, with a current timestamp — which
	// is how a cleared-but-live problem is told from a fixed one.
	e.noteErr("price: apply quotes", errString("read-only transaction"))
	if back := e.lastError(); back == nil || back.Count != 1 {
		t.Errorf("a still-failing error must reappear, got %+v", back)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// Backfill must not count as simulation progress. It is a bulk loader running orders of
// magnitude faster than the simulation — on a real deployment 5,848,000 of 5,856,539 writes were
// backfill's — so folding it in made writes/s a reading of the loader, hid a stalled simulation
// completely, and turned backfill finishing normally into a four-order-of-magnitude cliff that
// looked like a failure.
func TestHealthBackfillDoesNotMaskAStall(t *testing.T) {
	var h healthSampler
	h.sample(0, 0, 0, true)

	// The simulation stops writing while backfill keeps pouring rows in.
	h.lastProgressAt = time.Now().Add(-30 * time.Second)
	h.lastAt = time.Now().Add(-2 * healthSampleEvery)
	wps, _, bps, progress := h.sample(0, 0, 1_000_000, true)

	if wps != 0 {
		t.Errorf("simulation writes/s must be 0 while only backfill runs, got %v", wps)
	}
	if bps <= 0 {
		t.Errorf("backfill rows/s must be reported separately, got %v", bps)
	}
	// The stall clock must keep running: this is the case the panel exists to catch.
	if since := time.Since(progress); since < 25*time.Second {
		t.Errorf("backfill must not reset the stall clock — %s since progress, want ~30s", since)
	}
}
