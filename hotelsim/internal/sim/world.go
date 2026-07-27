package sim

import (
	"fmt"
	"math/rand"
)

// worldSeed fixes every hotel's name, region, tier, rooms, rate, and popularity —
// the world is regenerated identically on every process start and every Reset()
// (spec §29 Repeatability), never influenced by user input.
const worldSeed = 0x484f54454c // "HOTEL"

const hotelCount = 100

// regionCities gives each region a small set of deterministic city names —
// enough variety that the overview grid doesn't look mechanical.
var regionCities = map[Region][]string{
	RegionNorth:   {"Northgate", "Rivermont", "Crestwood", "Fairhaven", "Millbrook"},
	RegionCentral: {"Harbor View Central", "Midtown", "Union Square", "Capitol Heights", "Parkside"},
	RegionSouth:   {"Sunset Bay", "Palmetto", "Coral Ridge", "Gulf Point", "Magnolia"},
	RegionIsland:  {"Bluewater Cay", "Coconut Grove", "Windward Isle", "Pearl Harbor", "Tradewinds"},
}

var hotelSuffixes = []string{"Grand", "Plaza", "Suites", "Inn", "Resort & Spa", "Tower", "Lodge", "Gardens", "Palace", "Bay Club"}

var amenityPool = []string{
	"pool", "restaurant", "fitness-center", "conference-rooms", "spa", "bar",
	"business-center", "free-parking", "airport-shuttle", "pet-friendly",
	"rooftop-lounge", "kids-club", "beach-access", "golf-course",
}

var roomTypeDefs = []struct {
	code      RoomTypeCode
	name      string
	shareOf   float64 // fraction of TotalRooms
	rateMult  float64
	maxGuests int
}{
	{RoomStandard, "Standard", 0.60, 1.0, 2},
	{RoomDeluxe, "Deluxe King", 0.25, 1.45, 2},
	{RoomSuite, "Suite", 0.10, 2.30, 4},
	{RoomAccessible, "Accessible", 0.05, 1.0, 2},
}

// World is the fixed, deterministic 100-hotel topology — the CityMap analog.
// Nothing about it is user-configurable.
type World struct {
	Hotels   []*Hotel
	ByID     map[string]*Hotel
	ByRegion map[Region][]*Hotel

	// pickTable is a popularity-expanded slice: a hotel with Popularity 1.0
	// appears ~20x, one with the 0.05 floor appears once. Guest search picks
	// uniformly from this, producing the hotspot write skew that makes shard-key
	// choice matter.
	pickTable []*Hotel

	// scopeOrder is a fixed-seed shuffle of all 100 hotels. Profile.HotelScope
	// takes the first N, so even a 15-hotel standalone scope still spans every
	// region and tier rather than an arbitrary contiguous block.
	scopeOrder []*Hotel

	guestNames []string
}

// NewWorld deterministically generates all 100 hotels, their room types, and the
// fixed guest-name pool (spec §8's simulated guests need names; there is no
// `guests` collection per §12, so names are denormalized onto reservations).
func NewWorld() *World {
	rng := rand.New(rand.NewSource(worldSeed))
	w := &World{ByID: map[string]*Hotel{}, ByRegion: map[Region][]*Hotel{}}

	rankInRegion := map[Region]int{}
	for i := 0; i < hotelCount; i++ {
		id := fmt.Sprintf("H%03d", i+1)
		region := AllRegions[i/25] // 25 per region, in fixed id order
		rank := rankInRegion[region]
		rankInRegion[region] = rank + 1

		cities := regionCities[region]
		city := cities[rng.Intn(len(cities))]
		name := fmt.Sprintf("%s %s", city, hotelSuffixes[rng.Intn(len(hotelSuffixes))])

		tier, rooms := randomSizeTier(rng)
		category := []Category{CategoryBusiness, CategoryResort, CategoryBoutique, CategoryExtendedStay}[rng.Intn(4)]
		baseRate := baseRateFor(tier, category, rng)

		// Deterministic Zipf-by-rank-within-region: top-ranked hotels in a region
		// absorb disproportionate demand, floor 0.05 so every hotel gets some.
		popularity := 1.0 / (1.0 + float64(rank)*0.6)
		if popularity < 0.05 {
			popularity = 0.05
		}

		h := &Hotel{
			ID: id, Name: name, Region: region, City: city,
			Category: category, SizeTier: tier, TotalRooms: rooms,
			Location:             randomGeo(region, rng),
			Amenities:            pickAmenities(rng),
			Popularity:           popularity,
			BaseRate:             baseRate,
			OperationalStatus:    "open",
			CurrentOccupancyRate: 0,
		}
		h.RoomTypes = buildRoomTypes(h, rng)

		w.Hotels = append(w.Hotels, h)
		w.ByID[id] = h
		w.ByRegion[region] = append(w.ByRegion[region], h)
	}

	// Popularity-expanded pick table: weight rounded to nearest integer count,
	// minimum 1, so PickHot draws uniformly from it.
	for _, h := range w.Hotels {
		n := int(h.Popularity*20 + 0.5)
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			w.pickTable = append(w.pickTable, h)
		}
	}

	w.scopeOrder = append([]*Hotel{}, w.Hotels...)
	rng.Shuffle(len(w.scopeOrder), func(i, j int) { w.scopeOrder[i], w.scopeOrder[j] = w.scopeOrder[j], w.scopeOrder[i] })

	w.guestNames = buildGuestNames(rng, 5000)
	return w
}

// Scope returns the first n hotels of the fixed scopeOrder shuffle — the subset a
// topology's Profile.HotelScope restricts simulated demand to.
func (w *World) Scope(n int) []*Hotel {
	if n <= 0 || n > len(w.scopeOrder) {
		n = len(w.scopeOrder)
	}
	return w.scopeOrder[:n]
}

// PickHot draws one hotel from scope, weighted by popularity when the hotel is
// also in the popularity pick table, else uniformly from scope. This keeps the
// hotspot behavior visible even when HotelScope has narrowed the candidate set.
func (w *World) PickHot(rng *rand.Rand, scope []*Hotel) *Hotel {
	if len(scope) == 0 {
		return nil
	}
	inScope := make(map[string]bool, len(scope))
	for _, h := range scope {
		inScope[h.ID] = true
	}
	for tries := 0; tries < 10; tries++ {
		cand := w.pickTable[rng.Intn(len(w.pickTable))]
		if inScope[cand.ID] {
			return cand
		}
	}
	return scope[rng.Intn(len(scope))]
}

func (w *World) GuestName(rng *rand.Rand) (id, name string) {
	i := rng.Intn(len(w.guestNames))
	return fmt.Sprintf("G%05d", i), w.guestNames[i]
}

func randomSizeTier(rng *rand.Rand) (SizeTier, int) {
	switch rng.Intn(3) {
	case 0:
		return TierSmall, 50 + rng.Intn(51) // 50..100
	case 1:
		return TierMedium, 101 + rng.Intn(150) // 101..250
	default:
		return TierLarge, 251 + rng.Intn(350) // 251..600
	}
}

func baseRateFor(tier SizeTier, cat Category, rng *rand.Rand) float64 {
	base := map[SizeTier]float64{TierSmall: 95, TierMedium: 150, TierLarge: 230}[tier]
	if cat == CategoryResort {
		base *= 1.35
	} else if cat == CategoryBoutique {
		base *= 1.15
	}
	jitter := 0.9 + rng.Float64()*0.2 // +/-10%
	return round2(base * jitter)
}

func randomGeo(r Region, rng *rand.Rand) GeoPoint {
	// Rough real-world-shaped anchor per region; jittered so hotels spread out
	// but stay clustered by region on the map.
	anchors := map[Region][2]float64{
		RegionNorth:   {-122.42, 45.52},
		RegionCentral: {-97.74, 30.27},
		RegionSouth:   {-80.19, 25.76},
		RegionIsland:  {-64.79, 32.31},
	}
	a := anchors[r]
	lon := a[0] + (rng.Float64()-0.5)*2.0
	lat := a[1] + (rng.Float64()-0.5)*1.5
	return GeoPoint{Type: "Point", Coordinates: []float64{round2(lon), round2(lat)}}
}

func pickAmenities(rng *rand.Rand) []string {
	n := 3 + rng.Intn(4)
	perm := rng.Perm(len(amenityPool))
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, amenityPool[perm[i]])
	}
	return out
}

func buildRoomTypes(h *Hotel, rng *rand.Rand) []RoomType {
	out := make([]RoomType, 0, len(roomTypeDefs))
	for _, d := range roomTypeDefs {
		count := int(float64(h.TotalRooms)*d.shareOf + 0.5)
		if count < 1 {
			count = 1
		}
		out = append(out, RoomType{
			HotelID: h.ID, Code: d.code, Name: d.name,
			MaxGuests: d.maxGuests, RoomCount: count,
			BaseRate:  round2(h.BaseRate * d.rateMult),
			Amenities: pickAmenities(rng),
		})
	}
	return out
}

var guestFirstNames = []string{"Alice", "Bob", "Jane", "John", "Maria", "James", "Linda", "Robert", "Patricia", "Michael", "Barbara", "William", "Elizabeth", "David", "Jennifer", "Richard", "Susan", "Joseph", "Jessica", "Thomas"}
var guestLastNames = []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis", "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson", "Thomas", "Taylor", "Moore", "Jackson", "Martin"}

func buildGuestNames(rng *rand.Rand, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, guestFirstNames[rng.Intn(len(guestFirstNames))]+" "+guestLastNames[rng.Intn(len(guestLastNames))])
	}
	return out
}
