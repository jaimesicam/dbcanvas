package sim

import "math/rand"

// fleetSeed is separate from worldSeed so the fleet can be regenerated on its own
// deterministic schedule without disturbing route topology (they happen to be
// generated together at startup today, but the seeds are intentionally independent).
const fleetSeed = 0x464c4545 // "FLEE" (of "FLEET")

const fleetSize = 2000

var aircraftTypesByTier = map[SizeTier][]string{
	TierRegional: {"E175", "CRJ900", "ERJ145"},
	TierNarrow:   {"A320", "A321", "B737-800", "B737 MAX 8"},
	TierWide:     {"B787-9", "A330-300", "B777-300ER"},
}

// typicalSeats is the seat capacity assumed for every aircraft of a size tier — a
// deliberate simplification (real fleets vary seat count by sub-type) that keeps
// FlightInventory capacity a pure function of which route (and thus which tier's
// aircraft pool) a date's flight draws from.
var typicalSeats = map[SizeTier]int{
	TierRegional: 80,
	TierNarrow:   180,
	TierWide:     320,
}

// aircraftPerRoute is fleetSize/routeCount — 2000 aircraft pooled evenly across 200
// routes, 10 tail numbers each. A route's date-seeded flight (seed.go/fleet-ops
// agent) round-robins its own pool, so which of its ~10 tails actually flies a given
// date varies, and a maintenance/grounded tail temporarily narrows that pool.
const aircraftPerRoute = fleetSize / routeCount

// GenerateFleet deterministically builds the 2000-aircraft fleet, pools
// aircraftPerRoute tail numbers onto each of w.Routes (populating Route.AircraftPool
// and, from the pool's tier-typical seat count, Route.SeatClasses), and returns the
// flat aircraft list for the store to seed into the `aircraft` table. Must be called
// once, after NewWorld, before the world is considered ready.
func GenerateFleet(w *World) []*Aircraft {
	rng := rand.New(rand.NewSource(fleetSeed))
	var fleet []*Aircraft
	n := 1
	for _, r := range w.Routes {
		types := aircraftTypesByTier[r.SizeTier]
		pool := make([]string, 0, aircraftPerRoute)
		for i := 0; i < aircraftPerRoute; i++ {
			tail := tailNumber(n)
			n++
			status := AircraftActive
			// ~3% of the fleet starts in maintenance so the fleet-ops agent and the
			// Fleet panel have something to show from the very first snapshot.
			if rng.Float64() < 0.03 {
				status = AircraftMaintenance
			}
			base := r.Origin
			if i%2 == 1 {
				base = r.Destination
			}
			fleet = append(fleet, &Aircraft{
				TailNumber: tail, Type: types[rng.Intn(len(types))], SizeTier: r.SizeTier,
				HomeBase: base, RouteID: r.ID, Status: status,
			})
			pool = append(pool, tail)
		}
		r.AircraftPool = pool
		r.SeatClasses = seatClassesFor(r.ID, typicalSeats[r.SizeTier])
	}
	return fleet
}

// tailNumber renders a deterministic dbcanvas-flavored tail number, e.g. "N0001DB".
func tailNumber(n int) string {
	const digits = "0123456789"
	s := make([]byte, 4)
	for i := 3; i >= 0; i-- {
		s[i] = digits[n%10]
		n /= 10
	}
	return "N" + string(s) + "DB"
}

// PickAvailableTail returns the first active (non-maintenance, non-grounded) tail
// number in a route's pool, deterministically round-robined by flight date so the
// same date always picks the same tail (repeatability) but different dates spread
// across the pool. Falls back to the pool's first tail (even if grounded) rather than
// failing outright — a temporarily-short-handed route still needs an aircraft
// assigned so its inventory row has a capacity number.
func PickAvailableTail(pool []string, statusOf map[string]AircraftStatus, dayOrdinal int) string {
	if len(pool) == 0 {
		return ""
	}
	start := dayOrdinal % len(pool)
	for i := 0; i < len(pool); i++ {
		tail := pool[(start+i)%len(pool)]
		if statusOf[tail] == AircraftActive {
			return tail
		}
	}
	return pool[start]
}
