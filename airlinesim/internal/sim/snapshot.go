package sim

import (
	"context"
	"encoding/json"
	"time"

	"airlinesim/internal/store"
)

// Snapshot is the full GET /api/state payload — everything the route grid, summary
// dashboard, fleet panel, and MySQL topology panel need in one round trip.
// BuildSnapshot ALWAYS reads from MySQL, never from Engine's in-memory maps (the
// counters are read back from the metrics row the analytics agent itself just wrote
// them into) — this is what makes a page refresh always recover full state, and
// what makes every connected browser see identical data.
type Snapshot struct {
	Routes    []RouteTile            `json:"routes"`
	Fleet     FleetSummary           `json:"fleet"`
	Summary   json.RawMessage        `json:"summary,omitempty"`
	Diag      json.RawMessage        `json:"diag,omitempty"`
	Agents    []store.AgentHeartbeat `json:"agents"`
	Control   ControlInfo            `json:"control"`
	UptimeSec int64                  `json:"uptimeSeconds"`
	Error     string                 `json:"error,omitempty"`
}

type RouteTile struct {
	ID                 string  `json:"id"`
	FlightNumber       string  `json:"flightNumber"`
	Origin             string  `json:"origin"`
	Destination        string  `json:"destination"`
	Region             string  `json:"region"`
	SizeTier           string  `json:"sizeTier"`
	OperationalStatus  string  `json:"operationalStatus"`
	LoadFactorPct      float64 `json:"loadFactorPct"`
	LoadFactorClass    string  `json:"loadFactorClass"`
	Badge              string  `json:"badge"`
	ActiveReservations int64   `json:"activeReservations"`
	InScope            bool    `json:"inScope"`
}

type FleetSummary struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"byStatus"`
	ByTier   map[string]int `json:"byTier"`
}

type ControlInfo struct {
	State string `json:"state"` // "running" | "paused"
	Level string `json:"level"` // stop|low|medium|high
	Kind  string `json:"kind"`
}

func runningState(running bool) string {
	if running {
		return "running"
	}
	return "paused"
}

// BuildSnapshot assembles the full /api/state payload. If MySQL is unreachable, it
// still returns a Snapshot (Error set, everything else empty) rather than an error —
// the web interface must be able to show "can't reach MySQL" rather than going blank.
func (e *Engine) BuildSnapshot(ctx context.Context) Snapshot {
	snap := Snapshot{
		Control:   ControlInfo{State: runningState(e.Running()), Level: string(e.Level()), Kind: string(e.Kind)},
		UptimeSec: e.UptimeSeconds(),
	}
	if err := e.Store.Ping(ctx); err != nil {
		snap.Error = "cannot reach MySQL: " + err.Error()
		return snap
	}

	snap.Routes = e.buildRouteTiles(ctx)
	snap.Fleet = e.buildFleetSummary(ctx)

	var summary, diag string
	e.Store.DB.QueryRowContext(ctx, "SELECT payload FROM metrics WHERE id='current'").Scan(&summary)
	e.Store.DB.QueryRowContext(ctx, "SELECT payload FROM metrics WHERE id='diag'").Scan(&diag)
	if summary != "" {
		snap.Summary = json.RawMessage(summary)
	}
	if diag != "" {
		snap.Diag = json.RawMessage(diag)
	}

	agents, _ := e.Store.AllHeartbeats(ctx)
	snap.Agents = agents
	return snap
}

func (e *Engine) buildRouteTiles(ctx context.Context) []RouteTile {
	scope := e.World.Scope(e.Profile.RouteScope)
	inScope := make(map[string]bool, len(scope))
	for _, r := range scope {
		inScope[r.ID] = true
	}

	activeByRoute := map[string]int64{}
	if rows, err := e.Store.DB.QueryContext(ctx,
		"SELECT route_id, COUNT(*) FROM reservations WHERE status IN (?, ?) GROUP BY route_id",
		string(StatusConfirmed), string(StatusCheckedIn)); err == nil {
		for rows.Next() {
			var id string
			var n int64
			if rows.Scan(&id, &n) == nil {
				activeByRoute[id] = n
			}
		}
		rows.Close()
	}

	tiles := make([]RouteTile, 0, len(e.World.Routes))
	for _, r := range e.World.Routes {
		lf := r.CurrentLoadFactor
		class, badge := LoadFactorClass(lf)
		if !inScope[r.ID] {
			class, badge = "outofscope", "—" // em dash
		}
		tiles = append(tiles, RouteTile{
			ID: r.ID, FlightNumber: r.FlightNumber, Origin: r.Origin, Destination: r.Destination,
			Region: string(r.Region), SizeTier: string(r.SizeTier), OperationalStatus: r.OperationalStatus,
			LoadFactorPct: round2(lf * 100), LoadFactorClass: class, Badge: badge,
			ActiveReservations: activeByRoute[r.ID], InScope: inScope[r.ID],
		})
	}
	return tiles
}

func (e *Engine) buildFleetSummary(ctx context.Context) FleetSummary {
	fs := FleetSummary{Total: len(e.Fleet), ByStatus: map[string]int{}, ByTier: map[string]int{}}
	for _, ac := range e.Fleet {
		fs.ByStatus[string(e.aircraftStatus(ac.TailNumber))]++
		fs.ByTier[string(ac.SizeTier)]++
	}
	return fs
}

// AircraftRow is one row of the Fleet panel's paginated/searchable table.
type AircraftRow struct {
	TailNumber string `json:"tailNumber"`
	Type       string `json:"type"`
	SizeTier   string `json:"sizeTier"`
	HomeBase   string `json:"homeBase"`
	RouteID    string `json:"routeId"`
	Status     string `json:"status"`
}

// AircraftPage serves the Fleet panel: a searchable (by tail number or route),
// filterable (by status), paginated view over the 2000-row fleet — the aircraft
// dimension deliberately never gets its own dashboard tile (see world.go/fleet.go).
func (e *Engine) AircraftPage(ctx context.Context, search, status string, limit, offset int) ([]AircraftRow, int, error) {
	where := "1=1"
	var args []any
	if search != "" {
		where += " AND (tail_number LIKE ? OR route_id LIKE ?)"
		args = append(args, "%"+search+"%", "%"+search+"%")
	}
	if status != "" {
		where += " AND status = ?"
		args = append(args, status)
	}
	var total int
	if err := e.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM aircraft WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := e.Store.DB.QueryContext(ctx,
		"SELECT tail_number, type, size_tier, home_base, route_id, status FROM aircraft WHERE "+where+" ORDER BY tail_number LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []AircraftRow
	for rows.Next() {
		var a AircraftRow
		if rows.Scan(&a.TailNumber, &a.Type, &a.SizeTier, &a.HomeBase, &a.RouteID, &a.Status) == nil {
			out = append(out, a)
		}
	}
	return out, total, rows.Err()
}

// RouteDetail serves GET /api/routes/{id}: the route's static info, seat classes,
// a 21-day inventory window, its pooled aircraft, and recent reservations.
func (e *Engine) RouteDetail(ctx context.Context, routeID string) (map[string]any, bool) {
	r, ok := e.World.ByID[routeID]
	if !ok {
		return nil, false
	}
	today := e.Clock.Today()
	rows, err := e.Store.DB.QueryContext(ctx,
		"SELECT id, class_code, flight_date, tail_number, total_seats, booked_seats, available_seats, fare FROM flight_inventory WHERE route_id=? AND flight_date>=? AND flight_date<? ORDER BY flight_date, class_code",
		routeID, today.Format("2006-01-02"), today.AddDate(0, 0, 21).Format("2006-01-02"))
	var inventory []FlightInventory
	if err == nil {
		for rows.Next() {
			var fi FlightInventory
			if rows.Scan(&fi.ID, &fi.ClassCode, &fi.FlightDate, &fi.TailNumber, &fi.TotalSeats, &fi.BookedSeats, &fi.AvailableSeats, &fi.Fare) == nil {
				fi.RouteID = routeID
				inventory = append(inventory, fi)
			}
		}
		rows.Close()
	}

	recRows, _ := e.Store.DB.QueryContext(ctx, "SELECT id, passenger_name, class_code, flight_date, seats, status, fare_total FROM reservations WHERE route_id=? ORDER BY id DESC LIMIT 20", routeID)
	var recent []map[string]any
	if recRows != nil {
		for recRows.Next() {
			var id, name, class, status string
			var flightDate time.Time
			var seats int
			var fare float64
			if recRows.Scan(&id, &name, &class, &flightDate, &seats, &status, &fare) == nil {
				recent = append(recent, map[string]any{"id": id, "passengerName": name, "classCode": class, "flightDate": flightDate, "seats": seats, "status": status, "fareTotal": fare})
			}
		}
		recRows.Close()
	}

	return map[string]any{
		"route": r, "seatClasses": r.SeatClasses, "aircraftPool": r.AircraftPool,
		"inventory": inventory, "recentReservations": recent,
		"query": map[string]any{"targeted": true, "reason": "route_id equality — hits idx_fi_route_date directly"},
	}, true
}

// ReservationDetail serves GET /api/reservations/{id}, optionally with routeId+
// flightDate query params — supplying them makes the lookup hit the reservations
// primary key AND the idx_res_route_date index in one go; omitting them still works
// (primary key lookup alone), but the pedagogy is in timing the difference.
func (e *Engine) ReservationDetail(ctx context.Context, id, routeID, flightDate string) (map[string]any, bool) {
	query := "SELECT id, passenger_id, passenger_name, route_id, flight_number, origin, destination, region, class_code, flight_date, seats, fare_total, currency, status, version, seat_assignment, history, created_at, updated_at FROM reservations WHERE id=?"
	args := []any{id}
	targeted := routeID != "" && flightDate != ""
	if targeted {
		query += " AND route_id=? AND flight_date=?"
		args = append(args, routeID, flightDate)
	}
	start := time.Now()
	var res Reservation
	var seatAssignment, histJSON string
	row := e.Store.DB.QueryRowContext(ctx, query, args...)
	if err := row.Scan(&res.ID, &res.PassengerID, &res.PassengerName, &res.RouteID, &res.FlightNumber, &res.Origin, &res.Destination,
		&res.Region, &res.ClassCode, &res.FlightDate, &res.Seats, &res.FareTotal, &res.Currency, &res.Status, &res.Version,
		&seatAssignment, &histJSON, &res.CreatedAt, &res.UpdatedAt); err != nil {
		return nil, false
	}
	res.SeatAssignment = seatAssignment
	json.Unmarshal([]byte(histJSON), &res.History)
	elapsed := msSince(start)

	evRows, _ := e.Store.DB.QueryContext(ctx, "SELECT id, at, sim_at, reservation_id, action, route_id, flight_date FROM reservation_events WHERE reservation_id=? ORDER BY id DESC LIMIT 50", id)
	var events []store.Event
	if evRows != nil {
		for evRows.Next() {
			var ev store.Event
			var fd time.Time
			if evRows.Scan(&ev.ID, &ev.CreatedAt, &ev.SimAt, &ev.ReservationID, &ev.Kind, &ev.RouteID, &fd) == nil {
				ev.FlightDate = fd
				events = append(events, ev)
			}
		}
		evRows.Close()
	}

	return map[string]any{
		"reservation": res, "history": res.History, "events": events,
		"query": map[string]any{"targeted": targeted, "durationMs": elapsed, "reason": queryReason(targeted)},
	}, true
}

func queryReason(targeted bool) string {
	if targeted {
		return "id + route_id + flight_date — primary key AND idx_res_route_date both satisfied"
	}
	return "id only — primary key lookup, still fast at this scale, but doesn't exercise idx_res_route_date"
}
