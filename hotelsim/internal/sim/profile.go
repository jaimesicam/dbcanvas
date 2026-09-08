package sim

import (
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"

	"hotelsim/internal/store"
)

// LoadLevel is the Stop/Low/Medium/High control the web UI exposes (spec §5) —
// the only thing a user can change.
type LoadLevel string

const (
	LevelStop   LoadLevel = "stop"
	LevelLow    LoadLevel = "low"
	LevelMedium LoadLevel = "medium"
	LevelHigh   LoadLevel = "high"
)

// Profile is the ONLY thing that differs between MongoDB topologies. Agent code
// is identical across all three — it reads these fields. Mirrors the way Traffic
// Sim's TrafficLevel maps to targetVehicles()/incidentChancePerTick(): one set of
// agent goroutines, three sets of numbers.
type Profile struct {
	Topology store.Topology

	// HotelScope: how much of the 100-hotel world the workload touches.
	// Standalone concentrates load onto a small subset so a single mongod shows
	// real inventory contention; sharded spans everything so chunk distribution
	// and cross-shard routing are observable.
	HotelScope int

	// Capability gates, derived from Topology at NewProfile — never independently
	// configurable.
	Transactions  bool // false on standalone: no multi-document transaction support
	ChangeStreams bool // false on standalone: change streams need an oplog

	WriteConcern  *writeconcern.WriteConcern
	ReadConcern   *readconcern.ReadConcern
	AnalyticsRead *readpref.ReadPref

	// ScatterRatio: fraction of searches/analytics reads deliberately NOT
	// shard-key-prefixed (a broadcast, on purpose, for the query-education
	// panel). Zero on unsharded topologies.
	ScatterRatio float64
	// ExplainRate: sampling rate for verified explain() classification — kept low
	// so explaining queries doesn't itself distort the throughput being measured.
	ExplainRate float64

	// Per-level throughput targets — from the spec's own §7.1/7.2/7.3 tables
	// (values below are the midpoint of each range; High on sharded is capped
	// well below the spec's 10,000-session ceiling to stay within what a demo
	// container can usefully show, per the spec's own "actual maximum shall
	// depend on the available test environment").
	Sessions     map[LoadLevel]int
	OpsPerSecond map[LoadLevel]float64

	DuplicateRate float64 // §22: fraction of reservation attempts that intentionally resubmit a prior requestId
	CancelRate    float64 // fraction of confirmed reservations cancelled per simulated day
	ModifyRate    float64
	NoShowRate    float64
}

// NewProfile builds the Profile for a detected topology.
func NewProfile(topo store.Topology) Profile {
	switch topo {
	case store.TopologyReplicaSet:
		return Profile{
			Topology: topo, HotelScope: 75, Transactions: true, ChangeStreams: true,
			WriteConcern: writeconcern.Majority(), ReadConcern: readconcern.Majority(),
			AnalyticsRead: readpref.SecondaryPreferred(),
			ScatterRatio:  0, ExplainRate: 0.05,
			Sessions:      map[LoadLevel]int{LevelStop: 0, LevelLow: 60, LevelMedium: 250, LevelHigh: 700},
			OpsPerSecond:  map[LoadLevel]float64{LevelStop: 0, LevelLow: 30, LevelMedium: 125, LevelHigh: 350},
			DuplicateRate: 0.05, CancelRate: 0.10, ModifyRate: 0.15, NoShowRate: 0.05,
		}
	case store.TopologySharded:
		return Profile{
			Topology: topo, HotelScope: hotelCount, Transactions: true, ChangeStreams: true,
			WriteConcern: writeconcern.Majority(), ReadConcern: readconcern.Majority(),
			AnalyticsRead: readpref.SecondaryPreferred(),
			ScatterRatio:  0.25, ExplainRate: 0.10,
			Sessions:      map[LoadLevel]int{LevelStop: 0, LevelLow: 300, LevelMedium: 1250, LevelHigh: 2000},
			OpsPerSecond:  map[LoadLevel]float64{LevelStop: 0, LevelLow: 150, LevelMedium: 625, LevelHigh: 1000},
			DuplicateRate: 0.05, CancelRate: 0.10, ModifyRate: 0.15, NoShowRate: 0.05,
		}
	default: // standalone
		return Profile{
			Topology: store.TopologyStandalone, HotelScope: 15, Transactions: false, ChangeStreams: false,
			WriteConcern: writeconcern.W1(), ReadConcern: readconcern.Local(),
			AnalyticsRead: readpref.Primary(),
			ScatterRatio:  0, ExplainRate: 0.05,
			Sessions:      map[LoadLevel]int{LevelStop: 0, LevelLow: 12, LevelMedium: 45, LevelHigh: 130},
			OpsPerSecond:  map[LoadLevel]float64{LevelStop: 0, LevelLow: 6, LevelMedium: 25, LevelHigh: 70},
			DuplicateRate: 0.05, CancelRate: 0.10, ModifyRate: 0.15, NoShowRate: 0.05,
		}
	}
}
