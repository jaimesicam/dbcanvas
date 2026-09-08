package sim

import "math/rand"

// fleetSeed is separate from worldSeed so the fleet can be regenerated on its
// own deterministic schedule without disturbing location topology (they happen
// to be generated together at startup today, but the seeds are intentionally
// independent).
const fleetSeed = 0x464c4545 // "FLEE" (of "FLEET")

const fleetSize = 2000

var makeModelsByTier = map[SizeTier][]string{
	TierSmall:  {"Toyota Corolla", "Honda Civic", "Nissan Sentra"},
	TierMedium: {"Toyota Camry", "Honda Accord", "Ford Explorer", "Jeep Grand Cherokee"},
	TierLarge:  {"Chevrolet Tahoe", "BMW 5 Series", "Mercedes E-Class"},
}

// vehiclesPerLocation is fleetSize/locationCount — 2000 vehicles pooled roughly
// evenly across 180 locations, ~11 each. A location's date-seeded inventory
// (seed.go/fleet-ops agent) draws from its own pool; a maintenance/cleaning
// vehicle temporarily narrows that pool's available count.
const vehiclesPerLocation = fleetSize / locationCount

// GenerateFleet deterministically builds the 2000-vehicle fleet, pools
// vehiclesPerLocation VINs onto each of w.Locations (populating
// Location.VehiclePool and, from the pool's tier-typical size,
// Location.VehicleClasses), and returns the flat vehicle list for the store to
// seed into the `vehicles` table. Must be called once, after NewWorld, before
// the world is considered ready. Any remainder from fleetSize/locationCount not
// dividing evenly is distributed one extra each to the first N locations, so
// every one of the 2000 VINs generated is actually pooled somewhere.
func GenerateFleet(w *World) []*Vehicle {
	rng := rand.New(rand.NewSource(fleetSeed))
	var fleet []*Vehicle
	remainder := fleetSize - vehiclesPerLocation*len(w.Locations)
	n := 1
	for i, l := range w.Locations {
		poolSize := vehiclesPerLocation
		if i < remainder {
			poolSize++
		}
		models := makeModelsByTier[l.SizeTier]
		pool := make([]string, 0, poolSize)
		for j := 0; j < poolSize; j++ {
			vin := vinNumber(n)
			n++
			status := VehicleAvailable
			// ~3% of the fleet starts in maintenance so the fleet-ops agent and the
			// Fleet panel have something to show from the very first snapshot.
			if rng.Float64() < 0.03 {
				status = VehicleMaintenance
			}
			fleet = append(fleet, &Vehicle{
				VIN: vin, MakeModel: models[rng.Intn(len(models))], ClassCode: pickClassForTier(rng),
				HomeLocationID: l.ID, CurrentLocationID: l.ID, Status: status,
			})
			pool = append(pool, vin)
		}
		l.VehiclePool = pool
		l.VehicleClasses = vehicleClassesFor(l.ID, poolSize)
	}
	return fleet
}

func pickClassForTier(rng *rand.Rand) VehicleClassCode {
	codes := []VehicleClassCode{ClassEconomy, ClassEconomy, ClassCompact, ClassCompact, ClassSUV, ClassLuxury}
	return codes[rng.Intn(len(codes))]
}

// vinNumber renders a deterministic dbcanvas-flavored VIN, e.g. "DBCARS0001".
func vinNumber(n int) string {
	const digits = "0123456789"
	s := make([]byte, 4)
	for i := 3; i >= 0; i-- {
		s[i] = digits[n%10]
		n /= 10
	}
	return "DBCARS" + string(s)
}

// PickAvailableVehicle returns the first available (non-rented, non-maintenance,
// non-cleaning) VIN in a location's home pool, deterministically round-robined
// by date so the same date always picks the same VIN (repeatability) but
// different dates spread across the pool. Falls back to the pool's first VIN
// (even if unavailable) rather than failing outright — a temporarily-short-
// handed location still needs a vehicle assigned so its inventory row has a
// capacity number; the actual specific-vehicle claim at check-out (see
// booking.go's CheckOut) is a separate, concurrency-safe step that doesn't rely
// on this pick being accurate.
func PickAvailableVehicle(pool []string, statusOf map[string]VehicleStatus, dayOrdinal int) string {
	if len(pool) == 0 {
		return ""
	}
	start := dayOrdinal % len(pool)
	for i := 0; i < len(pool); i++ {
		vin := pool[(start+i)%len(pool)]
		if statusOf[vin] == VehicleAvailable {
			return vin
		}
	}
	return pool[start]
}
