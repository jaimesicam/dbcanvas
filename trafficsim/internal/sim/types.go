package sim

import "time"

// RoadState is a road segment's visible classification — labels/icons carry the
// meaning, never color alone (spec §19 accessibility).
type RoadState string

const (
	StateFreeFlow RoadState = "free"
	StateModerate RoadState = "moderate"
	StateHeavy    RoadState = "heavy"
	StateSevere   RoadState = "severe"
	StateBlocked  RoadState = "blocked"
	StateNoData   RoadState = "nodata"
)

// TrafficLevel scales how much traffic the vehicle-mover and incident-generator
// agents produce — the Off/Low/Medium/High control the web UI exposes.
type TrafficLevel string

const (
	LevelOff    TrafficLevel = "off"
	LevelLow    TrafficLevel = "low"
	LevelMedium TrafficLevel = "medium"
	LevelHigh   TrafficLevel = "high"
)

// targetVehicles is the steady-state vehicle count the mover agent spawns toward.
func (l TrafficLevel) targetVehicles() int {
	switch l {
	case LevelLow:
		return 10
	case LevelMedium:
		return 30
	case LevelHigh:
		return 70
	default: // off
		return 0
	}
}

// incidentChancePerTick is the per-tick probability (out of 1.0) the incident
// generator rolls to create a new incident.
func (l TrafficLevel) incidentChancePerTick() float64 {
	switch l {
	case LevelLow:
		return 0.01
	case LevelMedium:
		return 0.03
	case LevelHigh:
		return 0.07
	default: // off
		return 0
	}
}

// Intersection is a static map node — a grid corner with a traffic signal.
type Intersection struct {
	ID   string
	Row  int
	Col  int
	Lon  float64
	Lat  float64
	Name string
}

// Road is a static, directed segment between two intersections. Dynamic fields
// (state, congestion, avg speed, vehicle count) live in Valkey, recomputed every
// tick by the state-calculator agent — this struct only carries the fixed topology.
type Road struct {
	ID         string
	Name       string
	From       *Intersection
	To         *Intersection
	SpeedLimit float64 // km/h
	LengthM    float64
	Lanes      int
	SignalID   string // the signal a vehicle approaching To must respect
}

// CityMap is the whole fixed, deterministic topology generated once at startup.
type CityMap struct {
	Intersections []*Intersection
	Roads         []*Road
	RoadByID      map[string]*Road
	// AdjOut[intersectionID] lists roads leaving that intersection — how a vehicle
	// picks its next road when it reaches one.
	AdjOut map[string][]*Road
}

// Vehicle is a live, in-memory simulated vehicle. Only the mover agent mutates it;
// every tick its current snapshot is written to Valkey (HSET + GEOADD), never read
// back from Valkey — Valkey is the durable/shareable view, not the agent's own state.
type Vehicle struct {
	ID         string
	Type       string // "car" | "truck" | "bus" | "emergency"
	RoadID     string
	Position   float64 // 0..1 along the road
	Speed      float64 // km/h, current
	Status     string  // "moving" | "queued" | "arrived"
	SpawnedAt  time.Time
	LastUpdate time.Time
}

// Signal is a live, in-memory traffic signal. Cycled by the signal agent; vehicles
// approaching read SignalStates (via Engine) to decide whether to queue.
type Signal struct {
	ID             string
	IntersectionID string
	State          string // "green" | "yellow" | "red"
	Since          time.Time
}

// Incident is a live, in-memory temporary event affecting a road.
type Incident struct {
	ID        string
	Type      string // "accident" | "stall" | "construction" | "closure"
	RoadID    string
	Severity  string // "minor" | "moderate" | "major"
	StartedAt time.Time
	EndsAt    time.Time
	Status    string // "active" | "cleared"
}
