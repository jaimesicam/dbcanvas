package sim

import (
	"sync"
	"time"
)

// simRate: simulated time runs this many times faster than wall-clock time.
// 720x = one simulated trading day per two real minutes, so a ten-minute look
// at the dashboard spans several complete sessions and a report printed at the
// end covers a plausible-looking day. Same rate as the sibling sims, for the
// same reason.
const simRate = 720.0

// SimClock is an accelerated clock anchored at a (wallEpoch, simEpoch) pair.
// Reading it is cheap; re-anchoring happens only at startup and Reset.
type SimClock struct {
	mu        sync.RWMutex
	wallEpoch time.Time
	simEpoch  time.Time
}

// NewSimClock anchors the clock at sim, "now" in wall-clock terms.
func NewSimClock(sim time.Time) *SimClock {
	c := &SimClock{}
	c.Anchor(sim)
	return c
}

// Anchor re-anchors the clock: simulated time becomes sim right now, and
// advances at simRate from this instant forward. Used at startup (from a
// persisted checkpoint or a fresh Reset baseline) — never mid-run.
func (c *SimClock) Anchor(sim time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.simEpoch = sim
	c.wallEpoch = time.Now()
}

// Now returns the current simulated instant.
func (c *SimClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	elapsed := time.Since(c.wallEpoch)
	return c.simEpoch.Add(time.Duration(float64(elapsed) * simRate))
}

// Today returns the current simulated date truncated to UTC midnight — the
// report's "session date".
func (c *SimClock) Today() time.Time {
	n := c.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// ResetToday anchors simulated "today" to the real calendar date at midnight
// UTC — used on Reset so dates look plausible and every run has the same
// relative structure.
func (c *SimClock) ResetToday() {
	n := time.Now().UTC()
	c.Anchor(time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC))
}

// Rate exposes the acceleration factor for display.
func (c *SimClock) Rate() float64 { return simRate }
