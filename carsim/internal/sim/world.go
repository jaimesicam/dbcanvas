package sim

import (
	"fmt"
	"math/rand"
)

// worldSeed fixes every location's name, region, tier, and popularity — the
// world is regenerated identically on every process start and every Reset(),
// never influenced by user input. fleetSeed (fleet.go) is a separate constant so
// the fleet can be regenerated independently of the location topology if the two
// ever need to change on different schedules.
const worldSeed = 0x43414e54 // "CANT" (of "CAR RENTAL")

const locationCount = 180

// citiesByRegion gives each region a small set of deterministic city/airport-style
// codes — enough variety that the location list doesn't look mechanical.
var citiesByRegion = map[Region][]string{
	RegionNorth:   {"PDX", "SEA", "BOI", "GEG", "MSO", "YVR"},
	RegionCentral: {"DFW", "ORD", "DEN", "MSP", "STL", "OKC"},
	RegionSouth:   {"MIA", "ATL", "HOU", "TPA", "MSY", "CHS"},
	RegionIsland:  {"HNL", "OGG", "SJU", "NAS", "BDA", "PPT"},
}

var vehicleClassDefs = []struct {
	code     VehicleClassCode
	name     string
	shareOf  float64 // fraction of a location's fleet count
	rateMult float64
}{
	{ClassEconomy, "Economy", 0.40, 1.0},
	{ClassCompact, "Compact", 0.30, 1.3},
	{ClassSUV, "SUV", 0.20, 1.9},
	{ClassLuxury, "Luxury", 0.10, 3.2},
}

// World is the fixed, deterministic 180-location topology — nothing about it is
// user-configurable.
type World struct {
	Locations []*Location
	ByID      map[string]*Location
	ByRegion  map[Region][]*Location

	// pickTable is a popularity-expanded slice: a location with Popularity 1.0
	// appears ~20x, one with the 0.05 floor appears once. Renter search picks
	// uniformly from this, producing the hotspot write skew that makes hot-row
	// contention actually visible under load on any of the four PostgreSQL
	// topologies this app can be linked to.
	pickTable []*Location

	// scopeOrder is a fixed-seed shuffle of all 180 locations. Profile.LocationScope
	// takes the first N, so even a small standalone-scoped subset still spans
	// every region.
	scopeOrder []*Location

	renterNames []string
}

// NewWorld deterministically generates all 180 locations, their vehicle classes,
// and the fixed renter-name pool (there is no `renters` table — names are
// denormalized onto reservations, same as Airline Sim's passengers).
func NewWorld() *World {
	rng := rand.New(rand.NewSource(worldSeed))
	w := &World{ByID: map[string]*Location{}, ByRegion: map[Region][]*Location{}}

	rankInRegion := map[Region]int{}
	for i := 0; i < locationCount; i++ {
		id := fmt.Sprintf("L%03d", i+1)
		region := AllRegions[i/45] // 45 per region, in fixed id order
		rank := rankInRegion[region]
		rankInRegion[region] = rank + 1

		cities := citiesByRegion[region]
		city := cities[rng.Intn(len(cities))]

		tier := randomSizeTier(rng)
		baseRate := baseRateFor(tier, rng)

		// Deterministic Zipf-by-rank-within-region: top-ranked locations in a
		// region absorb disproportionate demand, floor 0.05 so every location gets
		// some.
		popularity := 1.0 / (1.0 + float64(rank)*0.6)
		if popularity < 0.05 {
			popularity = 0.05
		}

		l := &Location{
			ID: id, Code: fmt.Sprintf("%s%d", city, rank+1), Name: fmt.Sprintf("%s Branch %d", city, rank+1),
			Region: region, SizeTier: tier,
			Popularity: popularity, BaseRate: baseRate,
			OperationalStatus: "open", CurrentUtilization: 0,
		}

		w.Locations = append(w.Locations, l)
		w.ByID[id] = l
		w.ByRegion[region] = append(w.ByRegion[region], l)
	}

	// Popularity-expanded pick table: weight rounded to nearest integer count,
	// minimum 1, so PickHot draws uniformly from it.
	for _, l := range w.Locations {
		n := int(l.Popularity*20 + 0.5)
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			w.pickTable = append(w.pickTable, l)
		}
	}

	w.scopeOrder = append([]*Location{}, w.Locations...)
	rng.Shuffle(len(w.scopeOrder), func(i, j int) { w.scopeOrder[i], w.scopeOrder[j] = w.scopeOrder[j], w.scopeOrder[i] })

	w.renterNames = buildRenterNames(rng, 5000)
	return w
}

// vehicleClassesFor builds a location's four vehicle-class rows from its total
// fleet count (derived from the vehicles pooled to it — see fleet.go's
// typicalFleetSize).
func vehicleClassesFor(locationID string, totalVehicles int) []VehicleClass {
	out := make([]VehicleClass, 0, len(vehicleClassDefs))
	for _, d := range vehicleClassDefs {
		count := int(float64(totalVehicles)*d.shareOf + 0.5)
		if count < 1 {
			count = 1
		}
		out = append(out, VehicleClass{LocationID: locationID, Code: d.code, Name: d.name, FleetCount: count, RateMult: d.rateMult})
	}
	return out
}

// Scope returns the first n locations of the fixed scopeOrder shuffle — the
// subset a topology's Profile.LocationScope restricts simulated demand to.
func (w *World) Scope(n int) []*Location {
	if n <= 0 || n > len(w.scopeOrder) {
		n = len(w.scopeOrder)
	}
	return w.scopeOrder[:n]
}

// PickHot draws one location from scope, weighted by popularity when the
// location is also in the popularity pick table, else uniformly from scope.
func (w *World) PickHot(rng *rand.Rand, scope []*Location) *Location {
	if len(scope) == 0 {
		return nil
	}
	inScope := make(map[string]bool, len(scope))
	for _, l := range scope {
		inScope[l.ID] = true
	}
	for tries := 0; tries < 10; tries++ {
		cand := w.pickTable[rng.Intn(len(w.pickTable))]
		if inScope[cand.ID] {
			return cand
		}
	}
	return scope[rng.Intn(len(scope))]
}

func (w *World) RenterName(rng *rand.Rand) (id, name string) {
	i := rng.Intn(len(w.renterNames))
	return fmt.Sprintf("C%05d", i), w.renterNames[i]
}

func randomSizeTier(rng *rand.Rand) SizeTier {
	switch rng.Intn(3) {
	case 0:
		return TierSmall
	case 1:
		return TierMedium
	default:
		return TierLarge
	}
}

func baseRateFor(tier SizeTier, rng *rand.Rand) float64 {
	base := map[SizeTier]float64{TierSmall: 35, TierMedium: 55, TierLarge: 85}[tier]
	jitter := 0.9 + rng.Float64()*0.2 // +/-10%
	return round2(base * jitter)
}

// renterFirstNames / renterLastNames intentionally reuse the same memorable
// first-name pool as Airline Sim's passengers (Alice/Bob/Jane/John Doe-style) —
// consistent, easily recognized sample data across every dbcanvas demo app,
// never invented names.
var renterFirstNames = []string{"Alice", "Bob", "Jane", "John", "Maria", "James", "Linda", "Robert", "Patricia", "Michael", "Barbara", "William", "Elizabeth", "David", "Jennifer", "Richard", "Susan", "Joseph", "Jessica", "Thomas"}
var renterLastNames = []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis", "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson", "Thomas", "Taylor", "Moore", "Jackson", "Martin"}

func buildRenterNames(rng *rand.Rand, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, renterFirstNames[rng.Intn(len(renterFirstNames))]+" "+renterLastNames[rng.Intn(len(renterLastNames))])
	}
	return out
}
