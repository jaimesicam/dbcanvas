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
// the product spec's traffic-control table. Stage S2's workload engine
// splits this across agent pools by WorkloadMix; stage S0 only needs the
// constants to exist for the dashboard's traffic-level selector.
var WorkerCounts = map[LoadLevel]int{
	LevelStop:   0,
	LevelLow:    8,
	LevelMedium: 32,
	LevelHigh:   96,
	LevelExtrm:  192,
}
