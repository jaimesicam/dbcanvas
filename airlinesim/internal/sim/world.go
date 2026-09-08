package sim

import (
	"fmt"
	"math/rand"
)

// worldSeed fixes every route's name, region, tier, and popularity — the world is
// regenerated identically on every process start and every Reset(), never influenced
// by user input. fleetSeed (fleet.go) is a separate constant so the fleet can be
// regenerated independently of the route topology if the two ever need to change on
// different schedules.
const worldSeed = 0x41494e45 // "AINE" (of "AIRLINE")

const routeCount = 200

// airportsByRegion gives each region a small set of deterministic three-letter
// airport codes — enough variety that the route list doesn't look mechanical.
var airportsByRegion = map[Region][]string{
	RegionNorth:   {"PDX", "SEA", "BOI", "GEG", "MSO", "YVR"},
	RegionCentral: {"DFW", "ORD", "DEN", "MSP", "STL", "OKC"},
	RegionSouth:   {"MIA", "ATL", "HOU", "TPA", "MSY", "CHS"},
	RegionIsland:  {"HNL", "OGG", "SJU", "NAS", "BDA", "PPT"},
}

var seatClassDefs = []struct {
	code     SeatClassCode
	name     string
	shareOf  float64 // fraction of TotalSeats
	fareMult float64
}{
	{ClassEconomy, "Economy", 0.72, 1.0},
	{ClassPremium, "Premium Economy", 0.14, 1.6},
	{ClassBusiness, "Business", 0.11, 2.8},
	{ClassFirst, "First", 0.03, 4.5},
}

// World is the fixed, deterministic 200-route topology — the CityMap/hotel-chain
// analog. Nothing about it is user-configurable.
type World struct {
	Routes   []*Route
	ByID     map[string]*Route
	ByRegion map[Region][]*Route

	// pickTable is a popularity-expanded slice: a route with Popularity 1.0 appears
	// ~20x, one with the 0.05 floor appears once. Passenger search picks uniformly
	// from this, producing the hotspot write skew that makes hot-row Galera
	// certification conflicts (on `pxc` targets) actually visible under load.
	pickTable []*Route

	// scopeOrder is a fixed-seed shuffle of all 200 routes. Profile.RouteScope takes
	// the first N, so even a small standalone-scoped subset still spans every region.
	scopeOrder []*Route

	passengerNames []string
}

// NewWorld deterministically generates all 200 routes, their seat classes, and the
// fixed passenger-name pool (there is no `passengers` table — names are denormalized
// onto reservations, same as Hotel Sim's guests).
func NewWorld() *World {
	rng := rand.New(rand.NewSource(worldSeed))
	w := &World{ByID: map[string]*Route{}, ByRegion: map[Region][]*Route{}}

	rankInRegion := map[Region]int{}
	flightNo := 100
	for i := 0; i < routeCount; i++ {
		id := fmt.Sprintf("R%03d", i+1)
		region := AllRegions[i/50] // 50 per region, in fixed id order
		rank := rankInRegion[region]
		rankInRegion[region] = rank + 1

		airports := airportsByRegion[region]
		origin := airports[rng.Intn(len(airports))]
		dest := airports[rng.Intn(len(airports))]
		for dest == origin {
			dest = airports[rng.Intn(len(airports))]
		}
		flightNo += 1 + rng.Intn(3)

		tier := randomSizeTier(rng)
		baseFare := baseFareFor(tier, rng)

		// Deterministic Zipf-by-rank-within-region: top-ranked routes in a region
		// absorb disproportionate demand, floor 0.05 so every route gets some.
		popularity := 1.0 / (1.0 + float64(rank)*0.6)
		if popularity < 0.05 {
			popularity = 0.05
		}

		r := &Route{
			ID: id, FlightNumber: fmt.Sprintf("DB%d", flightNo),
			Origin: origin, Destination: dest, Region: region, SizeTier: tier,
			Popularity: popularity, BaseFare: baseFare,
			OperationalStatus: "open", CurrentLoadFactor: 0,
		}

		w.Routes = append(w.Routes, r)
		w.ByID[id] = r
		w.ByRegion[region] = append(w.ByRegion[region], r)
	}

	// Popularity-expanded pick table: weight rounded to nearest integer count,
	// minimum 1, so PickHot draws uniformly from it.
	for _, r := range w.Routes {
		n := int(r.Popularity*20 + 0.5)
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			w.pickTable = append(w.pickTable, r)
		}
	}

	w.scopeOrder = append([]*Route{}, w.Routes...)
	rng.Shuffle(len(w.scopeOrder), func(i, j int) { w.scopeOrder[i], w.scopeOrder[j] = w.scopeOrder[j], w.scopeOrder[i] })

	w.passengerNames = buildPassengerNames(rng, 5000)
	return w
}

// seatClassesFor builds a route's four seat-class rows from its total seat count
// (derived from the aircraft pooled to it — see fleet.go's typicalSeats).
func seatClassesFor(routeID string, totalSeats int) []SeatClass {
	out := make([]SeatClass, 0, len(seatClassDefs))
	for _, d := range seatClassDefs {
		count := int(float64(totalSeats)*d.shareOf + 0.5)
		if count < 1 {
			count = 1
		}
		out = append(out, SeatClass{RouteID: routeID, Code: d.code, Name: d.name, SeatCount: count, FareMult: d.fareMult})
	}
	return out
}

// Scope returns the first n routes of the fixed scopeOrder shuffle — the subset a
// topology's Profile.RouteScope restricts simulated demand to.
func (w *World) Scope(n int) []*Route {
	if n <= 0 || n > len(w.scopeOrder) {
		n = len(w.scopeOrder)
	}
	return w.scopeOrder[:n]
}

// PickHot draws one route from scope, weighted by popularity when the route is also
// in the popularity pick table, else uniformly from scope.
func (w *World) PickHot(rng *rand.Rand, scope []*Route) *Route {
	if len(scope) == 0 {
		return nil
	}
	inScope := make(map[string]bool, len(scope))
	for _, r := range scope {
		inScope[r.ID] = true
	}
	for tries := 0; tries < 10; tries++ {
		cand := w.pickTable[rng.Intn(len(w.pickTable))]
		if inScope[cand.ID] {
			return cand
		}
	}
	return scope[rng.Intn(len(scope))]
}

func (w *World) PassengerName(rng *rand.Rand) (id, name string) {
	i := rng.Intn(len(w.passengerNames))
	return fmt.Sprintf("P%05d", i), w.passengerNames[i]
}

func randomSizeTier(rng *rand.Rand) SizeTier {
	switch rng.Intn(3) {
	case 0:
		return TierRegional
	case 1:
		return TierNarrow
	default:
		return TierWide
	}
}

func baseFareFor(tier SizeTier, rng *rand.Rand) float64 {
	base := map[SizeTier]float64{TierRegional: 120, TierNarrow: 210, TierWide: 480}[tier]
	jitter := 0.9 + rng.Float64()*0.2 // +/-10%
	return round2(base * jitter)
}

var passengerFirstNames = []string{"Alice", "Bob", "Jane", "John", "Maria", "James", "Linda", "Robert", "Patricia", "Michael", "Barbara", "William", "Elizabeth", "David", "Jennifer", "Richard", "Susan", "Joseph", "Jessica", "Thomas"}
var passengerLastNames = []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis", "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson", "Thomas", "Taylor", "Moore", "Jackson", "Martin"}

func buildPassengerNames(rng *rand.Rand, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, passengerFirstNames[rng.Intn(len(passengerFirstNames))]+" "+passengerLastNames[rng.Intn(len(passengerLastNames))])
	}
	return out
}
