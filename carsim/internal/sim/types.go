// Package sim is the simulation domain: a deterministic 180-location world plus
// a 2000-vehicle fleet, the engine that drives ten background agents against a
// PostgreSQL-family database, and the snapshot builder the web API reads from.
// Nothing in this package renders anything — see internal/api for that.
package sim

import (
	"math"
	"time"
)

// Region is one of the four traffic regions locations are grouped into — the
// same shape as Airline Sim's four route regions, used for the deliberate
// "browse by region" scatter query in the query-education panel.
type Region string

const (
	RegionNorth   Region = "north"
	RegionCentral Region = "central"
	RegionSouth   Region = "south"
	RegionIsland  Region = "island"
)

var AllRegions = []Region{RegionNorth, RegionCentral, RegionSouth, RegionIsland}

// SizeTier is a location's typical scale bucket (drives how many vehicles get
// pooled to it).
type SizeTier string

const (
	TierSmall  SizeTier = "small"  // neighborhood branch
	TierMedium SizeTier = "medium" // city branch
	TierLarge  SizeTier = "large"  // airport mega-branch
)

// VehicleClassCode is one of the four recurring rental classes every location offers.
type VehicleClassCode string

const (
	ClassEconomy VehicleClassCode = "ECO"
	ClassCompact VehicleClassCode = "CMP"
	ClassSUV     VehicleClassCode = "SUV"
	ClassLuxury  VehicleClassCode = "LUX"
)

// VehicleClass is one location's offering of one rental class — mirrors Airline
// Sim's SeatClass (a per-route, per-cabin-class row).
type VehicleClass struct {
	LocationID string           `json:"locationId"`
	Code       VehicleClassCode `json:"classCode"`
	Name       string           `json:"name"`
	FleetCount int              `json:"fleetCount"`
	RateMult   float64          `json:"rateMult"`
}

// Location is one of the 180 fixed, deterministically-generated branches — the
// static topology this app never mutates after startup (mutable per-location-
// per-date state lives in rental_inventory/reservations in PostgreSQL). Analog of
// Airline Sim's Route: the dashboard's tile-grid unit.
type Location struct {
	ID                 string         `json:"locationId"`
	Code               string         `json:"code"`
	Name               string         `json:"name"`
	Region             Region         `json:"region"`
	SizeTier           SizeTier       `json:"sizeTier"`
	VehicleClasses     []VehicleClass `json:"vehicleClasses,omitempty"` // seeded to its own table, embedded here only for in-memory convenience
	BaseRate           float64        `json:"-"`
	Popularity         float64        `json:"-"` // deterministic Zipf-by-rank-within-region hotspot weight (0.05..1.0)
	VehiclePool        []string       `json:"-"` // VINs pooled to this location (fleet.go)
	OperationalStatus  string         `json:"operationalStatus"`
	CurrentUtilization float64        `json:"currentUtilization"`
	LastUpdated        time.Time      `json:"lastUpdated"`
}

// VehicleStatus is a fleet vehicle's current operational state.
type VehicleStatus string

const (
	VehicleAvailable   VehicleStatus = "available"
	VehicleRented      VehicleStatus = "rented"
	VehicleCleaning    VehicleStatus = "cleaning"
	VehicleMaintenance VehicleStatus = "maintenance"
)

// Vehicle is one of the fleet's 2000 fixed VINs — the dimension Hotel Sim has no
// equivalent of. Vehicles don't get their own dashboard tile (2000 is too many
// DOM nodes); they're a searchable/paginated table. CurrentLocationID starts
// equal to HomeLocationID and only ever changes on a one-way rental's check-in
// (see booking.go's CheckIn) — this is the field the check-out agent's `FOR
// UPDATE SKIP LOCKED` claim query filters on, not HomeLocationID, since a
// vehicle dropped off somewhere else genuinely lives there now.
type Vehicle struct {
	VIN               string           `json:"vin"`
	MakeModel         string           `json:"makeModel"` // e.g. "Toyota Corolla", "Ford Explorer"
	ClassCode         VehicleClassCode `json:"classCode"`
	HomeLocationID    string           `json:"homeLocationId"`
	CurrentLocationID string           `json:"currentLocationId"`
	Status            VehicleStatus    `json:"status"`
	LastUpdated       time.Time        `json:"lastUpdated"`
}

// RentalInventory is one location+vehicleClass+date's capacity — the contended
// resource every booking/cancellation/modification agent fights over. Never
// cached; every read goes to PostgreSQL. Mirrors Airline Sim's FlightInventory,
// except a single Reserve call spans every date row in a rental's pickup->return
// range, not just one (see booking.go).
type RentalInventory struct {
	ID                string           `json:"id"` // "L137|ECO|2026-07-28"
	LocationID        string           `json:"locationId"`
	Region            Region           `json:"region"` // denormalized so the scatter-query demo has a non-composite-key column to filter on
	ClassCode         VehicleClassCode `json:"classCode"`
	Date              time.Time        `json:"date"`
	TotalVehicles     int              `json:"totalVehicles"`
	BookedVehicles    int              `json:"bookedVehicles"`
	AvailableVehicles int              `json:"availableVehicles"`
	Closed            bool             `json:"closed"`
	Rate              float64          `json:"rate"`
	LastUpdated       time.Time        `json:"lastUpdated"`
}

// ResStatus is a reservation's lifecycle state. Unlike Airline Sim's separate
// checked_in -> completed two-step, a rental's "returned" state IS terminal —
// there's no equivalent of a flight still needing to land after boarding.
type ResStatus string

const (
	StatusConfirmed  ResStatus = "confirmed"
	StatusCheckedOut ResStatus = "checked_out" // vehicle picked up
	StatusCheckedIn  ResStatus = "checked_in"  // vehicle returned (terminal)
	StatusCancelled  ResStatus = "cancelled"
	StatusNoShow     ResStatus = "no_show"
)

// StatusBadge is the accessibility letter shown alongside color — never color alone.
func StatusBadge(s ResStatus) string {
	switch s {
	case StatusConfirmed:
		return "C"
	case StatusCheckedOut:
		return "O"
	case StatusCheckedIn:
		return "I"
	case StatusCancelled:
		return "X"
	case StatusNoShow:
		return "N"
	default:
		return "?"
	}
}

// HistoryEntry records one transition in a reservation's life, JSON-encoded into
// the reservations.history column (PostgreSQL JSONB).
type HistoryEntry struct {
	At     time.Time `json:"at"`
	SimAt  time.Time `json:"simAt"`
	Action string    `json:"action"`
	By     string    `json:"by"`
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
}

// Reservation books a class at booking time, a date RANGE (pickup->return, not a
// single date like Airline Sim's flight_date), and a SPECIFIC vehicle only once
// checked out (VehicleVIN stays empty until then — see booking.go's CheckOut).
// ID is the confirmation number (see NewConfirmationNumber). A one-way rental has
// DropoffLocationID != PickupLocationID.
type Reservation struct {
	ID                string           `json:"reservationId"`
	RequestID         string           `json:"requestId"`
	RenterID          string           `json:"renterId"`
	RenterName        string           `json:"renterName"`
	PickupLocationID  string           `json:"pickupLocationId"`
	DropoffLocationID string           `json:"dropoffLocationId"`
	Region            Region           `json:"region"`
	ClassCode         VehicleClassCode `json:"classCode"`
	PickupDate        time.Time        `json:"pickupDate"`
	ReturnDate        time.Time        `json:"returnDate"`
	VehicleVIN        string           `json:"vehicleVin,omitempty"`
	RateTotal         float64          `json:"rateTotal"`
	Currency          string           `json:"currency"`
	Status            ResStatus        `json:"status"`
	Version           int              `json:"version"`
	ActualCheckOut    *time.Time       `json:"actualCheckOut,omitempty"`
	ActualCheckIn     *time.Time       `json:"actualCheckIn,omitempty"`
	History           []HistoryEntry   `json:"history"`
	CreatedAt         time.Time        `json:"createdAt"`
	UpdatedAt         time.Time        `json:"updatedAt"`
}

// Nights returns the number of date rows a reservation's pickup->return range
// spans in rental_inventory — the atomicity guard Reserve checks RowsAffected
// against (see booking.go).
func (r *Reservation) Nights() int {
	return int(r.ReturnDate.Sub(r.PickupDate).Hours() / 24)
}

// RenterSession is an in-memory-only simulated renter — the PassengerSession
// analog. Never persisted; a session that dies with the process is correct
// behavior.
type RenterSession struct {
	ID         string
	RenterID   string
	RenterName string
	Stage      string // "searching" | "selecting" | "booking" | "booked" | "abandoned"
	Region     Region
	Candidates []string
	ChosenLoc  string
	DropoffLoc string
	ClassCode  VehicleClassCode
	PickupDate time.Time
	ReturnDate time.Time
	RequestID  string
	StartedAt  time.Time
	LastActive time.Time
}

// ResRef is a ring-buffer entry recording enough of a just-created reservation so
// later agents can issue a targeted follow-up query instead of always falling
// back to a broadcast scan.
type ResRef struct {
	ID         string
	LocationID string
	PickupDate time.Time
	ReturnDate time.Time
}

// UtilizationClass classifies a 0..1 utilization rate into the badge the UI shows.
func UtilizationClass(rate float64) (class, badge string) {
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
