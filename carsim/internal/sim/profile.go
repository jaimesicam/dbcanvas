package sim

// TargetKind is the PostgreSQL-family shape Car Rental Sim is connected to,
// resolved authoritatively by dbcanvas at container-creation time (see
// app/carsim.go) and passed in via the TARGET_KIND env var. Unlike Hotel Sim,
// this app never self-detects topology from inside — dbcanvas's own edge-walk
// already knows exactly which of the 7 link shapes it resolved, and duplicating
// that detection here would just be a second place for the two to disagree.
type TargetKind string

const (
	TargetPG             TargetKind = "pg"
	TargetPatroni        TargetKind = "patroni"
	TargetRepmgr         TargetKind = "repmgr"
	TargetSpock          TargetKind = "spock"
	TargetHAProxyPatroni TargetKind = "haproxy-patroni"
	TargetHAProxyRepmgr  TargetKind = "haproxy-repmgr"
	TargetHAProxySpock   TargetKind = "haproxy-spock"
)

// Family folds the 7 resolvable kinds down to the 4 that actually change agent
// behavior. A proxy in front of a cluster carries the same characteristics as
// talking to that cluster directly — the proxy is transparent once connected.
func (k TargetKind) Family() string {
	switch k {
	case TargetPG:
		return "pg"
	case TargetRepmgr, TargetHAProxyRepmgr:
		return "repmgr"
	case TargetSpock, TargetHAProxySpock:
		return "spock"
	default: // TargetPatroni, TargetHAProxyPatroni
		return "patroni"
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
// identical across all four — it reads these fields. One booker implementation
// (booking.go) serves every family (real ACID transactions everywhere,
// including a lone standalone node, and no client-visible synchronous conflict
// on any PostgreSQL topology this app can be linked to — not even Spock, whose
// multi-master conflict resolution is asynchronous and invisible to the client);
// what actually differs is how much of the 180-location world is in play.
type Profile struct {
	Kind   TargetKind
	Family string

	// LocationScope: how much of the 180-location world the workload touches. A
	// standalone `pg` target concentrates load onto a small subset so a single
	// postgres shows real hot-row contention; a clustered target (direct or
	// proxied) spans every location so cluster-wide steady-state throughput is
	// what's on display instead.
	LocationScope int

	// ScatterRatio: fraction of searches deliberately filtered by region+date only
	// (no location_id) — a genuine full/wide index scan for the query-education
	// panel, on purpose. Zero on a lone standalone node, where the working set is
	// small enough that the distinction barely shows.
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
// given level, scaled off this profile's own OpsPerSecond target for that level.
// Called from Engine.SetLevel whenever the level changes, so a jump to High
// isn't starved by a pool sized for Stop/Low.
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
	case "spock", "repmgr", "patroni":
		return Profile{
			Kind: kind, Family: family, LocationScope: locationCount,
			ScatterRatio: 0.25, ExplainRate: 0.10,
			Sessions:      map[LoadLevel]int{LevelStop: 0, LevelLow: 300, LevelMedium: 1250, LevelHigh: 2000},
			OpsPerSecond:  map[LoadLevel]float64{LevelStop: 0, LevelLow: 150, LevelMedium: 625, LevelHigh: 1000},
			DuplicateRate: 0.05, CancelRate: 0.10, ModifyRate: 0.15, NoShowRate: 0.05,
		}
	default: // "pg" — standalone
		return Profile{
			Kind: kind, Family: family, LocationScope: 20,
			ScatterRatio: 0, ExplainRate: 0.05,
			Sessions:      map[LoadLevel]int{LevelStop: 0, LevelLow: 12, LevelMedium: 45, LevelHigh: 130},
			OpsPerSecond:  map[LoadLevel]float64{LevelStop: 0, LevelLow: 6, LevelMedium: 25, LevelHigh: 70},
			DuplicateRate: 0.05, CancelRate: 0.10, ModifyRate: 0.15, NoShowRate: 0.05,
		}
	}
}
