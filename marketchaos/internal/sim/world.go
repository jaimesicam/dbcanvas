package sim

import (
	"fmt"
	"math/rand"
	"sort"
)

// worldSeed makes every deployment's 200-security universe identical and
// reproducible — the same symbols, sectors, and starting prices every time,
// exactly like every sibling sim's own fixed, seeded world.
const worldSeed = 20260101

// Sector is one of the fixed 10 sectors every deployment seeds.
type Sector struct {
	ID   int
	Name string
}

// Sectors is the fixed 10-sector universe.
var Sectors = []Sector{
	{1, "Technology"}, {2, "Energy"}, {3, "Financials"}, {4, "Healthcare"},
	{5, "Industrials"}, {6, "Consumer Discretionary"}, {7, "Consumer Staples"},
	{8, "Materials"}, {9, "Utilities"}, {10, "Real Estate"},
}

// Security is one seeded, fictional listed company.
type Security struct {
	Symbol      string
	CompanyName string
	SectorID    int
	SharesOut   int64
	StartPrice  float64
	Popularity  float64 // Zipf-weighted trading popularity, 0..1
}

// wordsBySector supplies the two halves of a generated company name, and a
// symbol-friendly abbreviation hint, per sector — enough variety that 200
// generated names don't feel obviously templated within one sector.
var wordsBySector = map[int][2][]string{
	1:  {{"Nebula", "Quantum", "Vertex", "Cipher", "Byteworks", "Silicon", "Nova"}, {"Logistics", "Systems", "Networks", "Computing", "Robotics", "Software", "Labs"}},
	2:  {{"GreenWave", "SolarPeak", "Ironclad", "Bluewater", "Fireline", "Hydro", "Meridian"}, {"Energy", "Power", "Fuels", "Grid", "Petroleum", "Reactors", "Turbines"}},
	3:  {{"Sterling", "Ledger", "Capital", "Anchor", "Trustbridge", "Highmark", "Union"}, {"Financial", "Holdings", "Trust", "Bank", "Partners", "Capital", "Group"}},
	4:  {{"Vitalis", "Genome", "Cascade", "Wellpoint", "Aurora", "Pulse", "Bright"}, {"Health", "Biosciences", "Pharma", "Therapeutics", "Medical", "Diagnostics", "Care"}},
	5:  {{"Ironforge", "Titan", "Continental", "Summit", "Atlas", "Foundry", "Granite"}, {"Industrial", "Manufacturing", "Machinery", "Systems", "Works", "Fabrication", "Engineering"}},
	6:  {{"Urban", "Craft", "Wanderlux", "Trailhead", "Skyline", "Bright", "Modern"}, {"Retail", "Apparel", "Leisure", "Motors", "Outfitters", "Brands", "Hospitality"}},
	7:  {{"Harvest", "Freshfield", "Golden", "Cornerstone", "Pure", "Homestead", "Meadow"}, {"Foods", "Beverages", "Provisions", "Goods", "Staples", "Grocers", "Farms"}},
	8:  {{"Bedrock", "Alloy", "Quarrystone", "Ferrous", "Copperline", "Granite", "Basalt"}, {"Materials", "Mining", "Metals", "Chemicals", "Resources", "Minerals", "Aggregates"}},
	9:  {{"Riverlight", "Coastal", "Meridian", "Steadfast", "Northgrid", "Cascade", "Summit"}, {"Utilities", "Water", "Power", "Electric", "Gas", "Grid", "Municipal"}},
	10: {{"Cornerstone", "Skyline", "Harborview", "Metropolitan", "Pinewood", "Landmark", "Crestline"}, {"Properties", "Realty", "Estates", "REIT", "Developments", "Holdings", "Ventures"}},
}

// seedSecurities are the product spec's own named examples, placed first so
// they always appear regardless of profile — everything after them is
// procedurally generated to fill out the fixed 200-security universe.
var seedSecurities = []struct {
	Symbol, Name string
	Sector       int
}{
	{"NBLX", "Nebula Logistics", 1},
	{"QNTM", "Quantum Manufacturing", 5},
	{"AERO", "Aero Systems", 5},
	{"GRNW", "GreenWave Energy", 2},
	{"DBX", "DataBox Technologies", 1},
	{"MARS", "Mars Agricultural Holdings", 7},
}

// GenerateSecurities builds the fixed, deterministic 200-security universe.
// Popularity is Zipf-weighted (rank-based, not random) so a handful of
// symbols dominate trading volume — the product spec's "a few symbols
// receive 80% of all trading activity" data-skew scenario needs this to be
// true of the seeded world itself, not just of live agent behavior.
func GenerateSecurities() []Security {
	rng := rand.New(rand.NewSource(worldSeed))
	out := make([]Security, 0, SecurityCount)
	used := map[string]bool{}

	add := func(symbol, name string, sector int) {
		out = append(out, Security{
			Symbol: symbol, CompanyName: name, SectorID: sector,
			SharesOut:  int64(5_000_000 + rng.Intn(495_000_000)),
			StartPrice: 5 + rng.Float64()*495,
		})
		used[symbol] = true
	}
	for _, s := range seedSecurities {
		add(s.Symbol, s.Name, s.Sector)
	}

	sectorIdx := make(map[int]int)
	for len(out) < SecurityCount {
		sectorID := (len(out) % SectorCount) + 1
		words := wordsBySector[sectorID]
		i := sectorIdx[sectorID]
		sectorIdx[sectorID] = i + 1
		prefix := words[0][i%len(words[0])]
		suffix := words[1][(i/len(words[0]))%len(words[1])]
		name := prefix + " " + suffix
		symbol := symbolFor(prefix, suffix, i, used)
		add(symbol, name, sectorID)
	}

	// Zipf-weighted popularity by rank: a shuffled rank order (so it's not
	// simply "the first securities generated are the popular ones") maps to
	// a 1/rank weight, normalized to 0..1.
	ranks := rng.Perm(len(out))
	for idx := range out {
		rank := ranks[idx] + 1
		out[idx].Popularity = 1.0 / float64(rank)
	}
	return out
}

// LoadWorld returns the fixed 200-security universe sorted by symbol (the
// stable order both the seeder and every live agent index into) plus its
// cumulative Zipf popularity weights for weightedPick — the one place this
// sort+cumulative-weights pairing happens, so the seeder (seed.go) and the
// live engine (engine.go) can never independently drift out of sync with
// each other's idea of "security index N".
func LoadWorld() ([]Security, []float64) {
	securities := GenerateSecurities()
	sort.Slice(securities, func(i, j int) bool { return securities[i].Symbol < securities[j].Symbol })
	return securities, cumulativeWeights(securities)
}

// symbolFor derives a short, unique ticker-style symbol from a generated
// company name.
func symbolFor(prefix, suffix string, salt int, used map[string]bool) string {
	base := initials(prefix, suffix)
	sym := base
	for used[sym] {
		salt++
		sym = fmt.Sprintf("%s%d", base, salt)
		if len(sym) > 8 {
			sym = sym[:8]
		}
	}
	return sym
}

func initials(words ...string) string {
	out := ""
	for _, w := range words {
		n := 2
		if len(w) < n {
			n = len(w)
		}
		out += upper(w[:n])
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}
