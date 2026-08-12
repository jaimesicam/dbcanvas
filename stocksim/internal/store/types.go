package store

import (
	"encoding/json"
	"time"
)

// The domain model, and the only vocabulary internal/sim and internal/api ever
// speak. These are plain structs on purpose: nothing here imports a database
// driver, which is what lets one engine be swapped for another underneath
// without either of those packages noticing.

// Security is a tradable instrument. Symbol is unique within a deployment and
// is what every other table joins on conceptually (the actual foreign key is
// ID, which is engine-assigned and opaque).
type Security struct {
	ID        string    `json:"id"`
	Symbol    string    `json:"symbol"`
	Name      string    `json:"name"`
	Sector    string    `json:"sector"`
	Currency  string    `json:"currency"`
	Shares    int64     `json:"sharesOutstanding"`
	OpenPrice float64   `json:"openPrice"`
	LastPrice float64   `json:"lastPrice"`
	DayHigh   float64   `json:"dayHigh"`
	DayLow    float64   `json:"dayLow"`
	DayVolume int64     `json:"dayVolume"`
	Listed    bool      `json:"listed"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Change is the absolute move from the session open, and ChangePct the same as
// a percentage. Both are derived rather than stored — computed on read so a
// hand-edited OpenPrice takes effect immediately.
func (s Security) Change() float64 { return s.LastPrice - s.OpenPrice }

func (s Security) ChangePct() float64 {
	if s.OpenPrice == 0 {
		return 0
	}
	return (s.LastPrice - s.OpenPrice) / s.OpenPrice * 100
}

// MarketCap is last price times shares outstanding.
func (s Security) MarketCap() float64 { return s.LastPrice * float64(s.Shares) }

// Tick is one observed price for one security. Append-only and high-volume:
// the price agent writes a batch of these every second, and they are the only
// table/collection/stream that is ever capped or trimmed.
type Tick struct {
	ID         string    `json:"id"`
	SecurityID string    `json:"securityId"`
	Symbol     string    `json:"symbol"`
	TS         time.Time `json:"ts"`
	Price      float64   `json:"price"`
	Volume     int64     `json:"volume"`
}

// Portfolio is one account that can hold positions and place orders. Owner is
// a person's name; Cash is settled buying power in the deployment's currency.
type Portfolio struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	Cash      float64   `json:"cash"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Order side and status values. These are the complete sets — every handler
// that accepts one validates against these rather than storing free text.
const (
	SideBuy  = "buy"
	SideSell = "sell"

	OrderOpen      = "open"
	OrderFilled    = "filled"
	OrderCancelled = "cancelled"
	OrderRejected  = "rejected"

	TypeMarket = "market"
	TypeLimit  = "limit"
)

// TerminalOrderStatuses are the states an order never leaves. Only these are
// ever pruned by retention: an open order is the live book and must survive
// however old it is, and an order that has settled is history that the trades
// table already records permanently.
var TerminalOrderStatuses = []string{OrderFilled, OrderCancelled, OrderRejected}

func ValidSide(s string) bool { return s == SideBuy || s == SideSell }
func ValidType(s string) bool { return s == TypeMarket || s == TypeLimit }
func ValidStatus(s string) bool {
	return s == OrderOpen || s == OrderFilled || s == OrderCancelled || s == OrderRejected
}

// Order is an instruction to trade. A market order has LimitPrice 0 and is
// fillable at whatever the security's last price is when the match agent gets
// to it; a limit order only fills when the market crosses it.
type Order struct {
	ID          string     `json:"id"`
	PortfolioID string     `json:"portfolioId"`
	SecurityID  string     `json:"securityId"`
	Symbol      string     `json:"symbol"`
	Owner       string     `json:"owner"`
	Side        string     `json:"side"`
	OrderType   string     `json:"orderType"`
	Quantity    int64      `json:"quantity"`
	LimitPrice  float64    `json:"limitPrice"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	FilledAt    *time.Time `json:"filledAt,omitempty"`
}

// Trade is the record of an order actually executing. One order produces at
// most one trade here — partial fills are deliberately out of scope, since the
// point of this app is legible CRUD, not a faithful matching engine.
type Trade struct {
	ID          string    `json:"id"`
	OrderID     string    `json:"orderId"`
	PortfolioID string    `json:"portfolioId"`
	SecurityID  string    `json:"securityId"`
	Symbol      string    `json:"symbol"`
	Side        string    `json:"side"`
	Quantity    int64     `json:"quantity"`
	Price       float64   `json:"price"`
	TS          time.Time `json:"ts"`
}

// Notional is the cash value of the trade.
func (t Trade) Notional() float64 { return t.Price * float64(t.Quantity) }

// Holding is a portfolio's current position in one security. Quantity can go
// negative (a short) — nothing prevents it, and the report shows it as such.
type Holding struct {
	PortfolioID string    `json:"portfolioId"`
	Owner       string    `json:"owner"`
	SecurityID  string    `json:"securityId"`
	Symbol      string    `json:"symbol"`
	Quantity    int64     `json:"quantity"`
	AvgCost     float64   `json:"avgCost"`
	LastPrice   float64   `json:"lastPrice"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func (h Holding) MarketValue() float64 { return h.LastPrice * float64(h.Quantity) }
func (h Holding) CostBasis() float64   { return h.AvgCost * float64(h.Quantity) }
func (h Holding) UnrealisedPL() float64 {
	return h.MarketValue() - h.CostBasis()
}

// Event is one line of the dashboard's live activity feed. Both the WebSocket
// push and the /api/events backfill carry these; the feed is durable-first,
// meaning the row is written before it is published (see sim.Engine.emit).
type Event struct {
	ID      string    `json:"id"`
	TS      time.Time `json:"ts"`
	Kind    string    `json:"kind"`
	Symbol  string    `json:"symbol,omitempty"`
	Message string    `json:"message"`
}

// AgentHeartbeat is one background agent's last-known state, as shown in the
// dashboard's Agents panel. Every sibling sim carries this same shape.
type AgentHeartbeat struct {
	Agent     string    `json:"agent"`
	Status    string    `json:"status"`
	LastTick  time.Time `json:"lastTick"`
	Detail    string    `json:"detail"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ObjectInfo describes one object this app created in the target database — a
// table, a collection, or a key prefix, depending on the engine. It is what
// makes the dashboard's Schema panel meaningful across all four backends, and
// what lets you watch objects appear on seed and vanish on drop.
type ObjectInfo struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"` // "table" | "collection" | "keyspace"
	Rows  int64  `json:"rows"`
	Bytes int64  `json:"bytes"`
}

// ListQuery is the shared pagination/filter shape for the three CRUD list
// endpoints. Limit is always clamped by the handler before it reaches a store.
type ListQuery struct {
	Search string
	Filter string // sector for securities, status for orders, owner for portfolios
	Limit  int
	Offset int
}

// Report is one round trip's worth of everything /report and the CSV exports
// need. Built by a single Store.ReportData call so the printed page is
// internally consistent rather than stitched from several racing reads.
type Report struct {
	GeneratedAt   time.Time        `json:"generatedAt"`
	SessionDate   time.Time        `json:"sessionDate"`
	Engine        string           `json:"engine"`
	ServerVersion string           `json:"serverVersion"`
	TargetLabel   string           `json:"targetLabel"`
	TargetKind    string           `json:"targetKind"`
	Database      string           `json:"database"`
	Securities    []Security       `json:"securities"`
	Portfolios    []Portfolio      `json:"portfolios"`
	Holdings      []Holding        `json:"holdings"`
	RecentTrades  []Trade          `json:"recentTrades"`
	Objects       []ObjectInfo     `json:"objects"`
	OrderCounts   map[string]int64 `json:"orderCounts"`
	TotalVolume   int64            `json:"totalVolume"`
	TotalTrades   int64            `json:"totalTrades"`
}

// Snapshot-side metrics are stored as an opaque JSON blob and handed back
// verbatim, so the analytics agent can change what it records without every
// store implementation needing to know.
type RawJSON = json.RawMessage
