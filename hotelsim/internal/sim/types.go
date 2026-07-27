// Package sim is the simulation domain: a deterministic 100-hotel world, the
// engine that drives ten background agents against it, and the snapshot builder
// the web API reads from. Nothing in this package renders anything — see
// internal/api for that.
package sim

import (
	"math"
	"time"
)

// Region is one of the chain's four geographic regions (§3 of the spec).
type Region string

const (
	RegionNorth   Region = "north"
	RegionCentral Region = "central"
	RegionSouth   Region = "south"
	RegionIsland  Region = "island"
)

var AllRegions = []Region{RegionNorth, RegionCentral, RegionSouth, RegionIsland}

// SizeTier is the hotel's room-count bucket (spec §3's recommended distribution).
type SizeTier string

const (
	TierSmall  SizeTier = "small"  // 50-100 rooms
	TierMedium SizeTier = "medium" // 101-250 rooms
	TierLarge  SizeTier = "large"  // 251-600 rooms
)

// Category is a flavor label (amenities/rate personality) independent of size —
// the spec's example hotel document shows `category: "business"`.
type Category string

const (
	CategoryBusiness     Category = "business"
	CategoryResort       Category = "resort"
	CategoryBoutique     Category = "boutique"
	CategoryExtendedStay Category = "extended-stay"
)

// RoomTypeCode is one of the recurring room types every hotel offers (spec §9).
type RoomTypeCode string

const (
	RoomStandard   RoomTypeCode = "STD"
	RoomDeluxe     RoomTypeCode = "DLX"
	RoomSuite      RoomTypeCode = "STE"
	RoomAccessible RoomTypeCode = "ACC"
)

// RoomType is one hotel's offering of one room code — mirrors the spec §12
// `roomTypes` example document.
type RoomType struct {
	HotelID   string       `bson:"hotelId" json:"hotelId"`
	Code      RoomTypeCode `bson:"roomTypeId" json:"roomTypeId"`
	Name      string       `bson:"name" json:"name"`
	MaxGuests int          `bson:"maximumGuests" json:"maximumGuests"`
	RoomCount int          `bson:"roomCount" json:"roomCount"`
	BaseRate  float64      `bson:"baseRate" json:"baseRate"`
	Amenities []string     `bson:"amenities" json:"amenities"`
}

// Hotel is one of the 100 fixed, deterministically-generated hotels — the static
// topology this app never mutates after startup (mutable per-hotel state, like
// current occupancy, lives in dailyInventory/reservations in MongoDB).
type Hotel struct {
	ID         string     `bson:"hotelId" json:"hotelId"`
	Name       string     `bson:"name" json:"name"`
	Region     Region     `bson:"region" json:"region"`
	City       string     `bson:"city" json:"city"`
	Category   Category   `bson:"category" json:"category"`
	SizeTier   SizeTier   `bson:"sizeTier" json:"sizeTier"`
	TotalRooms int        `bson:"totalRooms" json:"totalRooms"`
	Location   GeoPoint   `bson:"location" json:"location"`
	Amenities  []string   `bson:"amenities" json:"amenities"`
	RoomTypes  []RoomType `bson:"-" json:"roomTypes,omitempty"` // seeded to its own collection, embedded here only for in-memory convenience

	// Popularity is the deterministic Zipf-by-rank-within-region hotspot weight
	// (0.05..1.0) — the single knob that makes shard-key/hotspot behavior real.
	Popularity float64 `bson:"-" json:"-"`
	// BaseRate is the hotel-level nightly rate baseline room types multiply from.
	BaseRate float64 `bson:"-" json:"-"`

	OperationalStatus    string    `bson:"operationalStatus" json:"operationalStatus"` // "open" | "limited" | "closed"
	CurrentOccupancyRate float64   `bson:"currentOccupancyRate" json:"currentOccupancyRate"`
	LastUpdated          time.Time `bson:"lastUpdated" json:"lastUpdated"`
}

type GeoPoint struct {
	Type        string    `bson:"type" json:"type"`
	Coordinates []float64 `bson:"coordinates" json:"coordinates"` // [lon, lat]
}

// DailyInventory is one hotel+roomType+date's availability — the contended
// resource every booking/cancellation/modification agent fights over. Never
// cached; every read goes to MongoDB (see the engine/snapshot design notes).
type DailyInventory struct {
	ID      string `bson:"_id" json:"id"` // "H001|STD|2026-07-28"
	HotelID string `bson:"hotelId" json:"hotelId"`
	// Region is denormalized from the owning hotel specifically so a
	// deliberate scatter-gather query ({region:...}, no hotelId) is possible at
	// all — region is not part of the {hotelId, date} shard key, so filtering
	// on it alone broadcasts to every shard on purpose (spec §13's "broad
	// chain-wide searches", and the query-education panel's scatter example).
	Region           Region       `bson:"region" json:"region"`
	RoomTypeCode     RoomTypeCode `bson:"roomTypeId" json:"roomTypeId"`
	Date             time.Time    `bson:"date" json:"date"`
	TotalRooms       int          `bson:"totalRooms" json:"totalRooms"`
	BookedRooms      int          `bson:"bookedRooms" json:"bookedRooms"`
	HeldRooms        int          `bson:"heldRooms" json:"heldRooms"`
	UnavailableRooms int          `bson:"unavailableRooms" json:"unavailableRooms"`
	AvailableRooms   int          `bson:"availableRooms" json:"availableRooms"`
	Closed           bool         `bson:"closed" json:"closed"`
	Rate             float64      `bson:"rate" json:"rate"`
	Promotion        string       `bson:"promotion,omitempty" json:"promotion,omitempty"`
	LastUpdated      time.Time    `bson:"lastUpdated" json:"lastUpdated"`
}

// ResStatus is a reservation's lifecycle state (spec §10). "modified" is
// deliberately not a status here — it's an event kind plus a version bump and a
// history entry, because a status a reservation leaves immediately isn't a state,
// and modeling it as one would break the "current status lives in the filter"
// guard pattern every transition depends on.
type ResStatus string

const (
	StatusConfirmed  ResStatus = "confirmed"
	StatusCheckedIn  ResStatus = "checked_in"
	StatusCheckedOut ResStatus = "checked_out"
	StatusCancelled  ResStatus = "cancelled"
	StatusNoShow     ResStatus = "no_show"
)

// StatusBadge is the accessibility letter shown alongside color — never color
// alone (spec §27).
func StatusBadge(s ResStatus) string {
	switch s {
	case StatusConfirmed:
		return "C"
	case StatusCheckedIn:
		return "I"
	case StatusCheckedOut:
		return "O"
	case StatusCancelled:
		return "X"
	case StatusNoShow:
		return "N"
	default:
		return "?"
	}
}

type NightlyRate struct {
	Date   time.Time `bson:"date" json:"date"`
	Amount float64   `bson:"amount" json:"amount"`
}

type HistoryEntry struct {
	At     time.Time `bson:"at" json:"at"`
	SimAt  time.Time `bson:"simAt" json:"simAt"`
	Action string    `bson:"action" json:"action"`
	By     string    `bson:"by" json:"by"`
	From   string    `bson:"from,omitempty" json:"from,omitempty"`
	To     string    `bson:"to,omitempty" json:"to,omitempty"`
}

// Reservation mirrors the spec §12 `reservations` example document. _id is the
// confirmation number (see NewConfirmationNumber) — deliberately not a separate
// unique-indexed field, since a unique index on a sharded collection must be
// prefixed by the shard key {hotelId, checkInDate}.
type Reservation struct {
	ID             string         `bson:"_id" json:"reservationId"` // confirmation number
	RequestID      string         `bson:"requestId" json:"requestId"`
	GuestID        string         `bson:"guestId" json:"guestId"`
	GuestName      string         `bson:"guestName" json:"guestName"`
	HotelID        string         `bson:"hotelId" json:"hotelId"`
	HotelName      string         `bson:"hotelName" json:"hotelName"`
	Region         Region         `bson:"region" json:"region"`
	RoomTypeCode   RoomTypeCode   `bson:"roomTypeId" json:"roomTypeId"`
	CheckInDate    time.Time      `bson:"checkInDate" json:"checkInDate"`
	CheckOutDate   time.Time      `bson:"checkOutDate" json:"checkOutDate"`
	NumberOfRooms  int            `bson:"numberOfRooms" json:"numberOfRooms"`
	Adults         int            `bson:"adults" json:"adults"`
	Children       int            `bson:"children" json:"children"`
	NightlyRates   []NightlyRate  `bson:"nightlyRates" json:"nightlyRates"`
	TotalAmount    float64        `bson:"totalAmount" json:"totalAmount"`
	Currency       string         `bson:"currency" json:"currency"`
	Status         ResStatus      `bson:"status" json:"status"`
	Version        int            `bson:"version" json:"version"`
	RoomNumber     string         `bson:"roomNumber,omitempty" json:"roomNumber,omitempty"`
	ActualCheckIn  *time.Time     `bson:"actualCheckIn,omitempty" json:"actualCheckIn,omitempty"`
	ActualCheckOut *time.Time     `bson:"actualCheckOut,omitempty" json:"actualCheckOut,omitempty"`
	History        []HistoryEntry `bson:"history" json:"history"`
	CreatedAt      time.Time      `bson:"createdAt" json:"createdAt"`
	UpdatedAt      time.Time      `bson:"updatedAt" json:"updatedAt"`
}

// GuestSession is an in-memory-only simulated guest — the Vehicle analog. Never
// persisted; a session that dies with the process is correct behavior.
type GuestSession struct {
	ID           string
	GuestID      string
	GuestName    string
	Stage        string // "searching" | "selecting" | "booking" | "booked" | "abandoned"
	Region       Region
	Candidates   []string
	ChosenHotel  string
	RoomTypeCode RoomTypeCode
	CheckIn      time.Time
	Nights       int
	Adults       int
	Children     int
	RequestID    string
	StartedAt    time.Time
	LastActive   time.Time
}

// ResRef is a ring-buffer entry recording enough of a just-created reservation
// (its full shard key) that later agents can issue a targeted follow-up query
// instead of always falling back to a broadcast scan.
type ResRef struct {
	ID          string
	HotelID     string
	CheckInDate time.Time
}

// OccupancyClass classifies a 0..1 occupancy rate into the badge the UI shows.
// Thresholds are this app's own invention (the spec says "high occupancy"
// without a number) — isolated here so they're one place to retune.
func OccupancyClass(rate float64) (class, badge string) {
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
