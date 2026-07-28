package sim

// TargetKind is the MySQL-family shape Airline Sim is connected to, resolved
// authoritatively by dbcanvas at container-creation time (see app/airlinesim.go) and
// passed in via the TARGET_KIND env var. Unlike Hotel Sim, this app never
// self-detects topology from inside — dbcanvas's own edge-walk already knows exactly
// which of the 7 link shapes it resolved, and duplicating that detection here would
// just be a second place for the two to disagree.
type TargetKind string

const (
	TargetPS            TargetKind = "ps"
	TargetMySQL         TargetKind = "mysql"
	TargetPXC           TargetKind = "pxc"
	TargetHAProxyPXC    TargetKind = "haproxy-pxc"
	TargetHAProxyMySQL  TargetKind = "haproxy-mysql"
	TargetProxySQLPXC   TargetKind = "proxysql-pxc"
	TargetProxySQLMySQL TargetKind = "proxysql-mysql"
)

// Family folds the 7 resolvable kinds down to the 3 that actually change agent
// behavior. A proxy in front of PXC carries the same hot-row Galera
// certification-conflict risk as talking to PXC directly, and a proxy in front of a
// MySQL replication frame behaves like that frame directly — the proxy is
// transparent once connected.
func (k TargetKind) Family() string {
	switch k {
	case TargetPS:
		return "ps"
	case TargetPXC, TargetHAProxyPXC, TargetProxySQLPXC:
		return "pxc"
	default:
		return "mysql"
	}
}

// LoadLevel is the Stop/Low/Medium/High control the web UI exposes — the only thing
// a user can change.
type LoadLevel string

const (
	LevelStop   LoadLevel = "stop"
	LevelLow    LoadLevel = "low"
	LevelMedium LoadLevel = "medium"
	LevelHigh   LoadLevel = "high"
)

// Profile is the ONLY thing that differs between target families. Agent code is
// identical across all three — it reads these fields. One booker implementation
// (booking.go) serves every family, since SQL gives real transactions everywhere;
// what actually differs is how much of the 200-route world is in play and how often
// the query-education panel deliberately issues a broadcast-style scan.
type Profile struct {
	Kind   TargetKind
	Family string

	// RouteScope: how much of the 200-route world the workload touches. A
	// standalone `ps` target concentrates load onto a small subset so a single
	// mysqld shows real hot-row contention; `pxc` (direct or proxied) spans every
	// route so cluster-wide steady-state throughput is what's on display instead.
	RouteScope int

	// ScatterRatio: fraction of searches/analytics reads deliberately filtered by
	// region+date only (no route_id) — a genuine full/wide index scan for the
	// query-education panel, on purpose. Zero on a lone standalone node, where the
	// working set is small enough that the distinction barely shows.
	ScatterRatio float64
	// ExplainRate: sampling rate for verified EXPLAIN classification — kept low so
	// explaining queries doesn't itself distort the throughput being measured.
	ExplainRate float64

	Sessions     map[LoadLevel]int
	OpsPerSecond map[LoadLevel]float64

	DuplicateRate float64 // fraction of booking attempts that intentionally resubmit a prior requestId
	CancelRate    float64 // fraction of confirmed reservations cancelled per simulated day
	ModifyRate    float64
	NoShowRate    float64
}

// PoolSize returns the *sql.DB connection pool sizing (maxOpen, maxIdle) for a
// given level, scaled off this profile's own OpsPerSecond target for that
// level — traffic at High on a `pxc` target (full cluster, up to 1000 ops/sec)
// genuinely needs a bigger pool than the same level on a lone `ps` standalone
// (topping out at 70 ops/sec), so this is keyed off the actual target family +
// level combination, not level alone. Called from Engine.SetLevel whenever the
// level changes, so a jump to High isn't starved by a pool sized for Stop/Low.
func (p Profile) PoolSize(level LoadLevel) (maxOpen, maxIdle int) {
	ops := p.OpsPerSecond[level]
	maxOpen = int(ops/4) + 8
	if maxOpen > 100 {
		maxOpen = 100
	}
	maxIdle = maxOpen / 2
	if maxIdle < 4 {
		maxIdle = 4
	}
	return maxOpen, maxIdle
}

// NewProfile builds the Profile for a resolved target kind.
func NewProfile(kind TargetKind) Profile {
	family := kind.Family()
	switch family {
	case "pxc":
		return Profile{
			Kind: kind, Family: family, RouteScope: routeCount,
			ScatterRatio: 0.25, ExplainRate: 0.10,
			Sessions:      map[LoadLevel]int{LevelStop: 0, LevelLow: 300, LevelMedium: 1250, LevelHigh: 2000},
			OpsPerSecond:  map[LoadLevel]float64{LevelStop: 0, LevelLow: 150, LevelMedium: 625, LevelHigh: 1000},
			DuplicateRate: 0.05, CancelRate: 0.10, ModifyRate: 0.15, NoShowRate: 0.05,
		}
	case "mysql":
		return Profile{
			Kind: kind, Family: family, RouteScope: 75,
			ScatterRatio: 0.10, ExplainRate: 0.05,
			Sessions:      map[LoadLevel]int{LevelStop: 0, LevelLow: 60, LevelMedium: 250, LevelHigh: 700},
			OpsPerSecond:  map[LoadLevel]float64{LevelStop: 0, LevelLow: 30, LevelMedium: 125, LevelHigh: 350},
			DuplicateRate: 0.05, CancelRate: 0.10, ModifyRate: 0.15, NoShowRate: 0.05,
		}
	default: // "ps" — standalone
		return Profile{
			Kind: kind, Family: family, RouteScope: 15,
			ScatterRatio: 0, ExplainRate: 0.05,
			Sessions:      map[LoadLevel]int{LevelStop: 0, LevelLow: 12, LevelMedium: 45, LevelHigh: 130},
			OpsPerSecond:  map[LoadLevel]float64{LevelStop: 0, LevelLow: 6, LevelMedium: 25, LevelHigh: 70},
			DuplicateRate: 0.05, CancelRate: 0.10, ModifyRate: 0.15, NoShowRate: 0.05,
		}
	}
}
