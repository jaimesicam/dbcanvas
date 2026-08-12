package sim

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"stocksim/internal/store"
)

// Snapshot is the full GET /api/state payload — everything the ticker grid,
// KPI row, schema panel and agent list need in one round trip.
//
// BuildSnapshot ALWAYS reads from the store, never from Engine's in-memory
// fields (the counters come back out of the metrics row the analytics agent
// wrote them into). That is what makes a page refresh recover full state, and
// what makes every connected browser see identical data.
type Snapshot struct {
	Ticker   []TickerRow            `json:"ticker"`
	Summary  json.RawMessage        `json:"summary,omitempty"`
	Diag     json.RawMessage        `json:"diag,omitempty"`
	Agents   []store.AgentHeartbeat `json:"agents"`
	Control  ControlInfo            `json:"control"`
	Seed     SeedProgress           `json:"seed"`
	Backfill BackfillStatus         `json:"backfill"`
	// WorkingSet is the read side of Backfill: how much of the data that was
	// grown is actually being touched. The two belong together on the page —
	// a size target without a working set explains why a small buffer pool
	// changed nothing.
	WorkingSet WorkingSetStatus `json:"workingSet"`
	// Retention is what keeps the order book bounded, and is reported so a user
	// can see why the orders table is not growing.
	Retention RetentionStatus `json:"retention"`
	// Lab is the state of the three deliberately-pathological knobs.
	Lab       LabStatus `json:"lab"`
	UptimeSec int64     `json:"uptimeSeconds"`
	Error     string    `json:"error,omitempty"`
	Warning   string    `json:"warning,omitempty"`
}

// TickerRow is one security as the dashboard grid shows it — the derived
// change fields computed once here rather than in three places in JavaScript.
type TickerRow struct {
	ID        string  `json:"id"`
	Symbol    string  `json:"symbol"`
	Name      string  `json:"name"`
	Sector    string  `json:"sector"`
	Open      float64 `json:"open"`
	Last      float64 `json:"last"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Volume    int64   `json:"volume"`
	Change    float64 `json:"change"`
	ChangePct float64 `json:"changePct"`
	MarketCap float64 `json:"marketCap"`
	Listed    bool    `json:"listed"`
}

// ControlInfo is the knob state every sibling sim reports the same way.
type ControlInfo struct {
	State       string  `json:"state"` // "running" | "paused"
	Level       string  `json:"level"`
	Engine      string  `json:"engine"`
	Database    string  `json:"database"`
	Location    string  `json:"location"`
	TargetKind  string  `json:"targetKind"`
	TargetLabel string  `json:"targetLabel"`
	SimNow      string  `json:"simNow"`
	SimRate     float64 `json:"simRate"`
}

// BuildSnapshot assembles the dashboard payload. If the database is
// unreachable it still returns a Snapshot — Error set, everything else empty —
// rather than an error, so the interface can show "can't reach MySQL" instead
// of going blank.
func (e *Engine) BuildSnapshot(ctx context.Context) Snapshot {
	snap := Snapshot{
		Control: ControlInfo{
			State:       stateWord(e.Running()),
			Level:       e.Level(),
			Engine:      e.Store.Engine(),
			Database:    e.Store.Database(),
			Location:    e.Store.Location(),
			TargetKind:  e.TargetKind,
			TargetLabel: e.TargetLabel,
			SimNow:      e.Clock.Now().UTC().Format(time.RFC3339),
			SimRate:     e.Clock.Rate(),
		},
		Backfill:   e.Backfill(),
		WorkingSet: e.WorkingSetStatus(),
		Retention:  e.Retention(),
		Lab:        e.Lab(),
		Seed:       e.Seed(),
		UptimeSec:  e.UptimeSeconds(),
		Agents:     []store.AgentHeartbeat{},
		Ticker:     []TickerRow{},
	}

	if err := e.Store.Ping(ctx); err != nil {
		snap.Error = "cannot reach " + engineDisplayName(e.Store.Engine()) + ": " + err.Error()
		return snap
	}

	secs, _, err := e.Store.ListSecurities(ctx, store.ListQuery{Limit: 500})
	if err != nil {
		snap.Error = "cannot read securities: " + err.Error()
		return snap
	}
	for _, s := range secs {
		snap.Ticker = append(snap.Ticker, TickerRow{
			ID: s.ID, Symbol: s.Symbol, Name: s.Name, Sector: s.Sector,
			Open: s.OpenPrice, Last: s.LastPrice, High: s.DayHigh, Low: s.DayLow,
			Volume: s.DayVolume, Change: round2(s.Change()), ChangePct: round2(s.ChangePct()),
			MarketCap: s.MarketCap(), Listed: s.Listed,
		})
	}
	// Biggest movers first — the grid's most useful default ordering, and it
	// means the top of the page changes as the market does.
	sort.SliceStable(snap.Ticker, func(i, j int) bool {
		return abs(snap.Ticker[i].ChangePct) > abs(snap.Ticker[j].ChangePct)
	})

	if raw, err := e.Store.GetMetrics(ctx, "current"); err == nil && len(raw) > 0 {
		snap.Summary = raw
	}
	if raw, err := e.Store.GetMetrics(ctx, "diag"); err == nil && len(raw) > 0 {
		snap.Diag = raw
	}
	if hb, err := e.Store.AllHeartbeats(ctx); err == nil {
		snap.Agents = hb
	}
	// A background failure that has since recovered is a warning, not an
	// error: the page stays live and simply says something went wrong.
	snap.Warning = e.lastError()
	return snap
}

func stateWord(running bool) string {
	if running {
		return "running"
	}
	return "paused"
}

func engineDisplayName(e string) string {
	switch e {
	case store.EngineMySQL:
		return "MySQL"
	case store.EnginePostgres:
		return "PostgreSQL"
	case store.EngineMongoDB:
		return "MongoDB"
	case store.EngineValkey:
		return "Valkey"
	}
	return e
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
