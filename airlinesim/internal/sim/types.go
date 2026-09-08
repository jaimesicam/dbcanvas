// Package sim is the simulation domain: a deterministic 200-route world plus a
// 2000-aircraft fleet, the engine that drives ten background agents against a
// MySQL-family database, and the snapshot builder the web API reads from. Nothing in
// this package renders anything — see internal/api for that.
package sim

import (
	"math"
	"time"
)

// Region is one of the four traffic regions routes are grouped into (by departure
// hub) — the same shape as Hotel Sim's four hotel regions, used for the deliberate
// "browse by region" scatter query in the query-education panel.
type Region string

const (
	RegionNorth   Region = "north"
	RegionCentral Region = "central"
	RegionSouth   Region = "south"
	RegionIsland  Region = "island"
)

var AllRegions = []Region{RegionNorth, RegionCentral, RegionSouth, RegionIsland}

// SizeTier is a route's typical equipment size bucket (drives which aircraft types
// get pooled to it).
type SizeTier string

const (
	TierRegional SizeTier = "regional" // regional jet, ~50-90 seats
	TierNarrow   SizeTier = "narrow"   // narrow-body, ~140-230 seats
	TierWide     SizeTier = "wide"     // wide-body, ~250-400 seats
)

// SeatClassCode is one of the four recurring cabin classes every route offers.
type SeatClassCode string

const (
	ClassEconomy  SeatClassCode = "ECO"
	ClassPremium  SeatClassCode = "PEY"
	ClassBusiness SeatClassCode = "BUS"
	ClassFirst    SeatClassCode = "FST"
)

// SeatClass is one route's offering of one cabin class — mirrors Hotel Sim's
// RoomType (a per-hotel, per-room-type row).
type SeatClass struct {
	RouteID   string        `json:"routeId"`
	Code      SeatClassCode `json:"classCode"`
	Name      string        `json:"name"`
	SeatCount int           `json:"seatCount"`
	FareMult  float64       `json:"fareMult"`
}

// Route is one of the 200 fixed, deterministically-generated routes — the static
// topology this app never mutates after startup (mutable per-route-per-date state
// lives in flight_inventory/reservations in MySQL). Aircraft-analog of Hotel Sim's
// Hotel: the dashboard's tile-grid unit.
type Route struct {
	ID                string      `json:"routeId"`
	FlightNumber      string      `json:"flightNumber"`
	Origin            string      `json:"origin"`
	Destination       string      `json:"destination"`
	Region            Region      `json:"region"`
	SizeTier          SizeTier    `json:"sizeTier"`
	SeatClasses       []SeatClass `json:"seatClasses,omitempty"` // seeded to its own table, embedded here only for in-memory convenience
	BaseFare          float64     `json:"-"`
	Popularity        float64     `json:"-"`                 // deterministic Zipf-by-rank-within-region hotspot weight (0.05..1.0)
	AircraftPool      []string    `json:"-"`                 // tail numbers pooled to this route (fleet.go)
	OperationalStatus string      `json:"operationalStatus"` // "open" | "limited" | "closed"
	CurrentLoadFactor float64     `json:"currentLoadFactor"`
	LastUpdated       time.Time   `json:"lastUpdated"`
}

// AircraftStatus is a fleet aircraft's current operational state.
type AircraftStatus string

const (
	AircraftActive      AircraftStatus = "active"
	AircraftMaintenance AircraftStatus = "maintenance"
	AircraftGrounded    AircraftStatus = "grounded"
)

// Aircraft is one of the fleet's 2000 fixed tail numbers — the dimension Hotel Sim
// has no equivalent of. Aircraft don't get their own dashboard tile (2000 is too many
// DOM nodes); they're a searchable/paginated table, and their Status/RouteID feed the
// capacity a route+date's flight_inventory row gets seeded with.
type Aircraft struct {
	TailNumber  string         `json:"tailNumber"`
	Type        string         `json:"type"` // e.g. "E175", "A320", "B787-9"
	SizeTier    SizeTier       `json:"sizeTier"`
	HomeBase    string         `json:"homeBase"`
	RouteID     string         `json:"routeId"` // route this tail is pooled to
	Status      AircraftStatus `json:"status"`
	LastUpdated time.Time      `json:"lastUpdated"`
}

// FlightInventory is one route+seatClass+date's seat availability — the contended
// resource every booking/cancellation/modification agent fights over. Never cached;
// every read goes to MySQL. Mirrors Hotel Sim's DailyInventory.
type FlightInventory struct {
	ID               string        `json:"id"` // "R137|ECO|2026-07-28"
	RouteID          string        `json:"routeId"`
	Region           Region        `json:"region"` // denormalized so the scatter-query demo has a non-composite-key column to filter on
	ClassCode        SeatClassCode `json:"classCode"`
	FlightDate       time.Time     `json:"flightDate"`
	TailNumber       string        `json:"tailNumber"` // the aircraft assigned to this date's flight
	TotalSeats       int           `json:"totalSeats"`
	BookedSeats      int           `json:"bookedSeats"`
	HeldSeats        int           `json:"heldSeats"`
	UnavailableSeats int           `json:"unavailableSeats"`
	AvailableSeats   int           `json:"availableSeats"`
	Closed           bool          `json:"closed"`
	Fare             float64       `json:"fare"`
	Promotion        string        `json:"promotion,omitempty"`
	LastUpdated      time.Time     `json:"lastUpdated"`
}

// ResStatus is a reservation's lifecycle state — kept 1:1 with Hotel Sim's five
// states, just airline-flavored (checked_out -> completed).
type ResStatus string

const (
	StatusConfirmed ResStatus = "confirmed"
	StatusCheckedIn ResStatus = "checked_in"
	StatusCompleted ResStatus = "completed"
	StatusCancelled ResStatus = "cancelled"
	StatusNoShow    ResStatus = "no_show"
)

// StatusBadge is the accessibility letter shown alongside color — never color alone.
func StatusBadge(s ResStatus) string {
	switch s {
	case StatusConfirmed:
		return "C"
	case StatusCheckedIn:
		return "I"
	case StatusCompleted:
		return "O"
	case StatusCancelled:
		return "X"
	case StatusNoShow:
		return "N"
	default:
		return "?"
	}
}

// HistoryEntry records one transition in a reservation's life, JSON-encoded into the
// reservations.history column (MySQL JSON type) — the relational analog of Hotel
// Sim's embedded history array.
type HistoryEntry struct {
	At     time.Time `json:"at"`
	SimAt  time.Time `json:"simAt"`
	Action string    `json:"action"`
	By     string    `json:"by"`
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
}

// Reservation mirrors Hotel Sim's Reservation, minus the multi-night date range — an
// airline booking is a single flight_date. ID is the confirmation number (see
// NewConfirmationNumber).
type Reservation struct {
	ID             string         `json:"reservationId"`
	RequestID      string         `json:"requestId"`
	PassengerID    string         `json:"passengerId"`
	PassengerName  string         `json:"passengerName"`
	RouteID        string         `json:"routeId"`
	FlightNumber   string         `json:"flightNumber"`
	Origin         string         `json:"origin"`
	Destination    string         `json:"destination"`
	Region         Region         `json:"region"`
	ClassCode      SeatClassCode  `json:"classCode"`
	FlightDate     time.Time      `json:"flightDate"`
	Seats          int            `json:"seats"` // number of passengers on this booking
	FareTotal      float64        `json:"fareTotal"`
	Currency       string         `json:"currency"`
	Status         ResStatus      `json:"status"`
	Version        int            `json:"version"`
	SeatAssignment string         `json:"seatAssignment,omitempty"`
	ActualCheckIn  *time.Time     `json:"actualCheckIn,omitempty"`
	ActualBoarding *time.Time     `json:"actualBoarding,omitempty"`
	History        []HistoryEntry `json:"history"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
}

// PassengerSession is an in-memory-only simulated passenger — the GuestSession
// analog. Never persisted; a session that dies with the process is correct behavior.
type PassengerSession struct {
	ID            string
	PassengerID   string
	PassengerName string
	Stage         string // "searching" | "selecting" | "booking" | "booked" | "abandoned"
	Region        Region
	Candidates    []string
	ChosenRoute   string
	ClassCode     SeatClassCode
	FlightDate    time.Time
	Seats         int
	RequestID     string
	StartedAt     time.Time
	LastActive    time.Time
}

// ResRef is a ring-buffer entry recording enough of a just-created reservation that
// later agents can issue a targeted follow-up query instead of always falling back to
// a broadcast scan.
type ResRef struct {
	ID         string
	RouteID    string
	FlightDate time.Time
}

// LoadFactorClass classifies a 0..1 load factor into the badge the UI shows.
func LoadFactorClass(rate float64) (class, badge string) {
	switch {
	case rate >= 1.0:
		return "soldout", "X"
	case rate >= 0.85:
		return "high", "H"
	case rate >= 0.60:
		return "busy", "B"
	case rate >= 0.30:
		return "moderate", "M"
	default:
		return "quiet", "Q"
	}
}

// round2 rounds a currency amount to cents.
func round2(v float64) float64 { return math.Round(v*100) / 100 }
