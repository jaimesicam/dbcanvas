package sim

// SecurityCount and SectorCount are fixed constants of the fictional
// market — "200 publicly traded companies" and "10 sectors" are part of the
// world's identity, not something a dataset-size profile scales. Every
// profile seeds the exact same 200 securities across the same 10 sectors;
// only the trader/order/trade/tick volumes (and everything derived from
// them) change.
const (
	SecurityCount = 200
	SectorCount   = 10
)

// DatasetProfile selects how much data gets seeded. Medium is the default —
// large enough that the indexing/query-rewrite challenges are actually
// educational (a missing index on a 500-row table proves nothing), small
// enough to seed in a few minutes.
type DatasetProfile string

const (
	ProfileSmall  DatasetProfile = "small"
	ProfileMedium DatasetProfile = "medium"
	ProfileLarge  DatasetProfile = "large"
	ProfileCustom DatasetProfile = "custom"
)

// DatasetCounts is the full set of row counts to seed. Traders/Orders/Trades/
// Ticks are the 4 numbers a profile or a Custom selection sets directly;
// every other field is derived from them by Derive() so seed.go never
// hardcodes a ratio itself.
type DatasetCounts struct {
	Traders int
	Orders  int
	Trades  int
	Ticks   int

	// Derived — filled in by Derive(), not set directly.
	Accounts   int
	Watchlists int
	News       int
	AuditLogs  int
}

// datasetPresets are the product spec's own Small/Medium/Large row counts.
var datasetPresets = map[DatasetProfile]DatasetCounts{
	ProfileSmall:  {Traders: 2_000, Orders: 50_000, Trades: 25_000, Ticks: 500_000},
	ProfileMedium: {Traders: 10_000, Orders: 500_000, Trades: 250_000, Ticks: 5_000_000},
	ProfileLarge:  {Traders: 25_000, Orders: 2_000_000, Trades: 1_000_000, Ticks: 25_000_000},
}

// Preset returns the base counts for a named profile. ProfileCustom returns
// the zero value — the caller supplies its own Traders/Orders/Trades/Ticks.
func Preset(p DatasetProfile) DatasetCounts {
	return datasetPresets[p]
}

// Derive fills in every row count that isn't independently chosen: one
// account per trader, roughly 2 watchlist entries per trader (deduplicated
// against SecurityCount so it never asks for more distinct (trader,
// security) pairs than exist), news scaled much more lightly than the core
// tables (a busy exchange doesn't produce as much news copy as it does
// ticks), and one audit event per order (every order is itself an audited
// action).
func (c DatasetCounts) Derive() DatasetCounts {
	c.Accounts = c.Traders
	perTrader := 2
	if perTrader > SecurityCount {
		perTrader = SecurityCount
	}
	c.Watchlists = c.Traders * perTrader
	c.News = clamp(c.Orders/200, 200, 10_000)
	c.AuditLogs = c.Orders
	return c
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// bytesPerRow are rough, measured-order-of-magnitude InnoDB row-size
// constants (data + one secondary index) used only to interpolate a size
// estimate for a Custom profile — see EstimateBytes. Calibrated against the
// Small profile's actual on-disk size at stage S1; not exact for every
// possible Custom combination, and not meant to be.
var bytesPerRow = map[string]int{
	"traders": 220, "accounts": 90, "orders": 140, "trades": 110,
	"ticks": 90, "watchlists": 60, "news": 400, "audit": 200,
}

// EstimateBytes returns a rough total on-disk size estimate for these
// counts, used to show "about how big will this be" before seeding starts.
func (c DatasetCounts) EstimateBytes() int64 {
	d := c.Derive()
	total := int64(d.Traders)*int64(bytesPerRow["traders"]) +
		int64(d.Accounts)*int64(bytesPerRow["accounts"]) +
		int64(d.Orders)*int64(bytesPerRow["orders"]) +
		int64(d.Trades)*int64(bytesPerRow["trades"]) +
		int64(d.Ticks)*int64(bytesPerRow["ticks"]) +
		int64(d.Watchlists)*int64(bytesPerRow["watchlists"]) +
		int64(d.News)*int64(bytesPerRow["news"]) +
		int64(d.AuditLogs)*int64(bytesPerRow["audit"])
	return total
}

// rowsPerSecond is the calibrated batched-parallel-insert throughput this
// app's seeder achieves against a standalone target (see seedbulk.go) —
// used only to estimate seed time before it starts. PXC targets are
// materially slower (Galera certification+apply overhead per writeset), so
// EstimateSeconds applies a fixed multiplier for pxc-family targets rather
// than a second calibrated constant, which stage S1's live verification
// couldn't fully calibrate in the time available — see IMPLEMENTATION.md.
const rowsPerSecond = 45_000

// pxcSeedSlowdown is the multiplier applied to the estimate for pxc-family
// targets.
const pxcSeedSlowdown = 2.5

func (c DatasetCounts) totalRows() int {
	d := c.Derive()
	return d.Traders + d.Accounts + d.Orders + d.Trades + d.Ticks + d.Watchlists + d.News + d.AuditLogs + SecurityCount + SectorCount
}

// EstimateSeconds returns a rough seed-time estimate for these counts
// against the given target family ("ps", "pxc", or "mysql").
func (c DatasetCounts) EstimateSeconds(family string) int {
	secs := c.totalRows() / rowsPerSecond
	if secs < 1 {
		secs = 1
	}
	if family == "pxc" {
		secs = int(float64(secs) * pxcSeedSlowdown)
	}
	return secs
}
