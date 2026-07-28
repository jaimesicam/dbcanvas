package sim

// TargetKind is the MySQL-family shape MarketChaos is connected to, resolved
// authoritatively by dbcanvas at container-creation time (see
// app/marketchaos.go) and passed in via the TARGET_KIND env var. This app
// never self-detects topology from inside — dbcanvas's own edge-walk already
// knows exactly which link shape it resolved, and duplicating that detection
// here would just be a second place for the two to disagree.
//
// Unlike every prior sim, MarketChaos distinguishes a direct PXC member node
// ("pxcnode" — a single, un-load-balanced connection, useful for challenges
// about what happens when an app doesn't spread writes at all) from the PXC
// cluster frame ("pxc" — dbcanvas resolves the frame to one running member
// for the app's primary connection, but the PXC challenge pack additionally
// needs the full member list; see waitMarketChaosTarget in app/marketchaos.go).
type TargetKind string

const (
	TargetPS           TargetKind = "ps"
	TargetPXCNode      TargetKind = "pxcnode"
	TargetPXC          TargetKind = "pxc"
	TargetMySQL        TargetKind = "mysql"
	TargetHAProxyPXC   TargetKind = "haproxy-pxc"
	TargetHAProxyMySQL TargetKind = "haproxy-mysql"
)

// Family folds the resolvable kinds down to the 3 that actually change agent
// behavior and gate the PXC-specific challenge pack.
func (k TargetKind) Family() string {
	switch k {
	case TargetPS:
		return "ps"
	case TargetPXCNode, TargetPXC, TargetHAProxyPXC:
		return "pxc"
	default:
		return "mysql"
	}
}

// LoadLevel is the traffic control the web UI exposes.
type LoadLevel string

const (
	LevelStop   LoadLevel = "stop"
	LevelLow    LoadLevel = "low"
	LevelMedium LoadLevel = "medium"
	LevelHigh   LoadLevel = "high"
	LevelExtrm  LoadLevel = "extreme"
	LevelCustom LoadLevel = "custom"
)

// WorkerCounts is the approximate total worker count per traffic level, per
// the product spec's traffic-control table — deliberately family-independent
// (unlike rateOpsPerSecond below): app-side worker count not scaling with
// what the target can actually take is itself one of the teachable states
// (the "workers != MySQL connections" panel), not an oversight.
var WorkerCounts = map[LoadLevel]int{
	LevelStop:   0,
	LevelLow:    8,
	LevelMedium: 32,
	LevelHigh:   96,
	LevelExtrm:  192,
}

// WorkloadMix selects each of the 10 agent types' relative share of the
// current traffic level's budget. Unlike LoadLevel (how much traffic), this
// is what kind of traffic — read-heavy vs write-heavy vs deliberately
// contention-prone, etc.
type WorkloadMix string

const (
	MixBalanced         WorkloadMix = "balanced"
	MixReadHeavy        WorkloadMix = "read-heavy"
	MixWriteHeavy       WorkloadMix = "write-heavy"
	MixAnalyticsHeavy   WorkloadMix = "analytics-heavy"
	MixContentionHeavy  WorkloadMix = "contention-heavy"
	MixPXCConflictHeavy WorkloadMix = "pxc-conflict-heavy"
)

// AgentShares gives each of the 10 workload agent types a relative weight
// within its own group — RateShare fields (MarketData..DashboardPoll) split
// a shared ops/sec budget among the ticker-driven agents, PoolShare fields
// (RetailTrader..Portfolio) split the shared WorkerCounts[level] total among
// the goroutine-pool agents (see engine.go's hybrid concurrency model).
// Weights are relative, not required to sum to 1 — normalized at the call
// site (agentOpsPerSecond/agentWorkerCount below).
type AgentShares struct {
	MarketData, News, Scanner, Compliance, Cleanup, DashboardPoll float64
	RetailTrader, InstitutionalTrader, MatchingEngine, Portfolio  float64
}

func (s AgentShares) rateTotal() float64 {
	return s.MarketData + s.News + s.Scanner + s.Compliance + s.Cleanup + s.DashboardPoll
}

func (s AgentShares) poolTotal() float64 {
	return s.RetailTrader + s.InstitutionalTrader + s.MatchingEngine + s.Portfolio
}

var mixShares = map[WorkloadMix]AgentShares{
	MixBalanced: {
		MarketData: 0.30, News: 0.05, Scanner: 0.20, Compliance: 0.10, Cleanup: 0.05, DashboardPoll: 0.30,
		RetailTrader: 0.35, InstitutionalTrader: 0.15, MatchingEngine: 0.30, Portfolio: 0.20,
	},
	MixReadHeavy: {
		MarketData: 0.25, News: 0.05, Scanner: 0.35, Compliance: 0.05, Cleanup: 0.05, DashboardPoll: 0.25,
		RetailTrader: 0.30, InstitutionalTrader: 0.10, MatchingEngine: 0.25, Portfolio: 0.35,
	},
	MixWriteHeavy: {
		MarketData: 0.35, News: 0.05, Scanner: 0.10, Compliance: 0.15, Cleanup: 0.10, DashboardPoll: 0.25,
		RetailTrader: 0.45, InstitutionalTrader: 0.20, MatchingEngine: 0.25, Portfolio: 0.10,
	},
	MixAnalyticsHeavy: {
		MarketData: 0.20, News: 0.10, Scanner: 0.40, Compliance: 0.10, Cleanup: 0.05, DashboardPoll: 0.15,
		RetailTrader: 0.25, InstitutionalTrader: 0.10, MatchingEngine: 0.25, Portfolio: 0.40,
	},
	MixContentionHeavy: {
		MarketData: 0.30, News: 0.02, Scanner: 0.10, Compliance: 0.08, Cleanup: 0.05, DashboardPoll: 0.45,
		RetailTrader: 0.50, InstitutionalTrader: 0.20, MatchingEngine: 0.20, Portfolio: 0.10,
	},
	MixPXCConflictHeavy: {
		MarketData: 0.25, News: 0.02, Scanner: 0.08, Compliance: 0.05, Cleanup: 0.05, DashboardPoll: 0.55,
		RetailTrader: 0.25, InstitutionalTrader: 0.45, MatchingEngine: 0.20, Portfolio: 0.10,
	},
}

func sharesFor(mix WorkloadMix) AgentShares {
	if s, ok := mixShares[mix]; ok {
		return s
	}
	return mixShares[MixBalanced]
}

// rateOpsPerSecond is the baseline total ops/sec across every rate-driven
// agent combined, before WorkloadMix splits it up — scaled by target family
// the same way every sibling sim scales load headroom to what the backing
// target can actually take (a PXC cluster tolerates materially more than a
// lone standalone node).
var rateOpsPerSecond = map[string]map[LoadLevel]float64{
	"ps":    {LevelStop: 0, LevelLow: 8, LevelMedium: 30, LevelHigh: 80, LevelExtrm: 150},
	"mysql": {LevelStop: 0, LevelLow: 15, LevelMedium: 60, LevelHigh: 160, LevelExtrm: 300},
	"pxc":   {LevelStop: 0, LevelLow: 20, LevelMedium: 90, LevelHigh: 240, LevelExtrm: 450},
}

// PoolSize returns the *sql.DB connection pool sizing (maxOpen, maxIdle) for
// a given level — deliberately NOT set equal to WorkerCounts[level]: an
// undersized pool causing app-side queueing under load (sql.DBStats.WaitCount
// climbing while worker goroutines sit blocked) is itself one of the
// teachable states the "workers != MySQL connections" panel exists to show,
// not something to hide by always sizing the pool to match demand exactly.
// maxPoolOpen caps the primary pool well under MySQL's stock
// max_connections=151 default — found live (stage S2 verification): the
// original 150 cap left no headroom at all once a direct PXC cluster-frame
// link's per-member pools (see pxc.go/MemberConnCap below) added their own
// connections on top, and every one of pxc-1/2/3 enforces its OWN 151-
// connection limit independently, not a cluster-wide budget. Even after
// that first fix (a 90 cap), a live 3-node-cluster run under Extreme +
// pxc-conflict-heavy still transiently exceeded 151 real connections on the
// node carrying both the primary pool AND one member pool — real Galera
// certification conflicts hold COMMITs open for several seconds under that
// mix (see agentOpTimeout's comment), so 90+MemberConnCap concurrent slow
// commits is genuinely more simultaneous connection pressure than it looks
// like on paper. 70 leaves enough headroom for that plus normal cluster
// overhead (replication/IST threads, PMM, an interactive mysql client).
const maxPoolOpen = 70

func PoolSize(family string, level LoadLevel) (maxOpen, maxIdle int) {
	total := WorkerCounts[level]
	ops := rateOpsPerSecond[family][level]
	maxOpen = total/2 + int(ops/5) + 8
	if maxOpen > maxPoolOpen {
		maxOpen = maxPoolOpen
	}
	maxIdle = maxOpen / 2
	if maxIdle < 4 {
		maxIdle = 4
	}
	return maxOpen, maxIdle
}

// MemberConnCap bounds each PXC member's own connection pool (see
// pxc.go/MemberPool and openMembers in main.go) — kept small and fixed
// rather than derived from worker counts: each institutional-trader worker
// pinned to a member only ever holds one connection at a time (see
// agents_trading.go), so even the full Extreme worker budget spread across
// 3 members needs far less than this per member; the cap exists purely as a
// safety ceiling against MemberConnCap*3 + PoolSize's own maxOpen exceeding
// a single node's max_connections if a future mix ever pins most workers to
// one node's share.
const MemberConnCap = 15
