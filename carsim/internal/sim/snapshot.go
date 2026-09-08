package sim

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"carsim/internal/store"
)

// Snapshot is the full GET /api/state payload — everything the location grid,
// summary dashboard, fleet panel, and PostgreSQL topology panel need in one
// round trip. BuildSnapshot ALWAYS reads from PostgreSQL, never from Engine's
// in-memory maps (the counters are read back from the metrics row the
// analytics agent itself just wrote them into) — this is what makes a page
// refresh always recover full state, and what makes every connected browser see
// identical data.
type Snapshot struct {
	Locations []LocationTile         `json:"locations"`
	Fleet     FleetSummary           `json:"fleet"`
	Summary   json.RawMessage        `json:"summary,omitempty"`
	Diag      json.RawMessage        `json:"diag,omitempty"`
	Agents    []store.AgentHeartbeat `json:"agents"`
	Control   ControlInfo            `json:"control"`
	UptimeSec int64                  `json:"uptimeSeconds"`
	Error     string                 `json:"error,omitempty"`
}

type LocationTile struct {
	ID                 string  `json:"id"`
	Code               string  `json:"code"`
	Name               string  `json:"name"`
	Region             string  `json:"region"`
	SizeTier           string  `json:"sizeTier"`
	OperationalStatus  string  `json:"operationalStatus"`
	UtilizationPct     float64 `json:"utilizationPct"`
	UtilizationClass   string  `json:"utilizationClass"`
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

// BuildSnapshot assembles the full /api/state payload. If PostgreSQL is
// unreachable, it still returns a Snapshot (Error set, everything else empty)
// rather than an error — the web interface must be able to show "can't reach
// PostgreSQL" rather than going blank.
func (e *Engine) BuildSnapshot(ctx context.Context) Snapshot {
	snap := Snapshot{
		Control:   ControlInfo{State: runningState(e.Running()), Level: string(e.Level()), Kind: string(e.Kind)},
		UptimeSec: e.UptimeSeconds(),
	}
	if err := e.Store.Ping(ctx); err != nil {
		snap.Error = "cannot reach PostgreSQL: " + err.Error()
		return snap
	}

	snap.Locations = e.buildLocationTiles(ctx)
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

func (e *Engine) buildLocationTiles(ctx context.Context) []LocationTile {
	scope := e.World.Scope(e.Profile.LocationScope)
	inScope := make(map[string]bool, len(scope))
	for _, l := range scope {
		inScope[l.ID] = true
	}

	activeByLoc := map[string]int64{}
	if rows, err := e.Store.DB.QueryContext(ctx,
		"SELECT pickup_location_id, COUNT(*) FROM reservations WHERE status IN ($1, $2) GROUP BY pickup_location_id",
		string(StatusConfirmed), string(StatusCheckedOut)); err == nil {
		for rows.Next() {
			var id string
			var n int64
			if rows.Scan(&id, &n) == nil {
				activeByLoc[id] = n
			}
		}
		rows.Close()
	}

	tiles := make([]LocationTile, 0, len(e.World.Locations))
	for _, l := range e.World.Locations {
		u := l.CurrentUtilization
		class, badge := UtilizationClass(u)
		if !inScope[l.ID] {
			class, badge = "outofscope", "—" // em dash
		}
		tiles = append(tiles, LocationTile{
			ID: l.ID, Code: l.Code, Name: l.Name, Region: string(l.Region), SizeTier: string(l.SizeTier),
			OperationalStatus: l.OperationalStatus,
			UtilizationPct:    round2(u * 100), UtilizationClass: class, Badge: badge,
			ActiveReservations: activeByLoc[l.ID], InScope: inScope[l.ID],
		})
	}
	return tiles
}

func (e *Engine) buildFleetSummary(ctx context.Context) FleetSummary {
	fs := FleetSummary{Total: len(e.Fleet), ByStatus: map[string]int{}, ByTier: map[string]int{}}
	locByID := e.World.ByID
	for _, v := range e.Fleet {
		fs.ByStatus[string(e.vehicleStatus(v.VIN))]++
		if loc, ok := locByID[v.HomeLocationID]; ok {
			fs.ByTier[string(loc.SizeTier)]++
		}
	}
	return fs
}

// VehicleRow is one row of the Fleet panel's paginated/searchable table.
type VehicleRow struct {
	VIN               string `json:"vin"`
	MakeModel         string `json:"makeModel"`
	ClassCode         string `json:"classCode"`
	HomeLocationID    string `json:"homeLocationId"`
	CurrentLocationID string `json:"currentLocationId"`
	Status            string `json:"status"`
}

// VehiclePage serves the Fleet panel: a searchable (by VIN or location),
// filterable (by status), paginated view over the 2000-row fleet — the vehicle
// dimension deliberately never gets its own dashboard tile (see world.go/
// fleet.go).
func (e *Engine) VehiclePage(ctx context.Context, search, status string, limit, offset int) ([]VehicleRow, int, error) {
	where := "1=1"
	var args []any
	next := 1
	if search != "" {
		where += " AND (vin LIKE $" + strconv.Itoa(next) + " OR current_location_id LIKE $" + strconv.Itoa(next+1) + ")"
		args = append(args, "%"+search+"%", "%"+search+"%")
		next += 2
	}
	if status != "" {
		where += " AND status = $" + strconv.Itoa(next)
		args = append(args, status)
		next++
	}
	var total int
	if err := e.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM vehicles WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := e.Store.DB.QueryContext(ctx,
		"SELECT vin, make_model, class_code, home_location_id, current_location_id, status FROM vehicles WHERE "+where+" ORDER BY vin LIMIT $"+strconv.Itoa(next)+" OFFSET $"+strconv.Itoa(next+1),
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []VehicleRow
	for rows.Next() {
		var v VehicleRow
		if rows.Scan(&v.VIN, &v.MakeModel, &v.ClassCode, &v.HomeLocationID, &v.CurrentLocationID, &v.Status) == nil {
			out = append(out, v)
		}
	}
	return out, total, rows.Err()
}

// LocationDetail serves GET /api/locations/{id}: the location's static info,
// vehicle classes, a 21-day inventory window, its pooled vehicles, and recent
// reservations.
func (e *Engine) LocationDetail(ctx context.Context, locationID string) (map[string]any, bool) {
	l, ok := e.World.ByID[locationID]
	if !ok {
		return nil, false
	}
	today := e.Clock.Today()
	rows, err := e.Store.DB.QueryContext(ctx,
		"SELECT id, class_code, date, total_vehicles, booked_vehicles, available_vehicles, rate FROM rental_inventory WHERE location_id=$1 AND date>=$2 AND date<$3 ORDER BY date, class_code",
		locationID, today.Format("2006-01-02"), today.AddDate(0, 0, 21).Format("2006-01-02"))
	var inventory []RentalInventory
	if err == nil {
		for rows.Next() {
			var ri RentalInventory
			if rows.Scan(&ri.ID, &ri.ClassCode, &ri.Date, &ri.TotalVehicles, &ri.BookedVehicles, &ri.AvailableVehicles, &ri.Rate) == nil {
				ri.LocationID = locationID
				inventory = append(inventory, ri)
			}
		}
		rows.Close()
	}

	recRows, _ := e.Store.DB.QueryContext(ctx, "SELECT id, renter_name, class_code, pickup_date, return_date, status, rate_total FROM reservations WHERE pickup_location_id=$1 ORDER BY id DESC LIMIT 20", locationID)
	var recent []map[string]any
	if recRows != nil {
		for recRows.Next() {
			var id, name, class, status string
			var pickupDate, returnDate time.Time
			var rate float64
			if recRows.Scan(&id, &name, &class, &pickupDate, &returnDate, &status, &rate) == nil {
				recent = append(recent, map[string]any{"id": id, "renterName": name, "classCode": class, "pickupDate": pickupDate, "returnDate": returnDate, "status": status, "rateTotal": rate})
			}
		}
		recRows.Close()
	}

	return map[string]any{
		"location": l, "vehicleClasses": l.VehicleClasses, "vehiclePool": l.VehiclePool,
		"inventory": inventory, "recentReservations": recent,
		"query": map[string]any{"targeted": true, "reason": "location_id equality — hits idx_ri_location_date directly"},
	}, true
}

// ReservationDetail serves GET /api/reservations/{id}, optionally with
// locationId+pickupDate query params — supplying them makes the lookup hit the
// reservations primary key AND the idx_res_pickup_date index in one go;
// omitting them still works (primary key lookup alone), but the pedagogy is in
// timing the difference.
func (e *Engine) ReservationDetail(ctx context.Context, id, locationID, pickupDate string) (map[string]any, bool) {
	query := "SELECT id, renter_id, renter_name, pickup_location_id, dropoff_location_id, region, class_code, pickup_date, return_date, vehicle_vin, rate_total, currency, status, version, history, created_at, updated_at FROM reservations WHERE id=$1"
	args := []any{id}
	targeted := locationID != "" && pickupDate != ""
	if targeted {
		query += " AND pickup_location_id=$2 AND pickup_date=$3"
		args = append(args, locationID, pickupDate)
	}
	start := time.Now()
	var res Reservation
	var vehicleVIN, histJSON string
	row := e.Store.DB.QueryRowContext(ctx, query, args...)
	if err := row.Scan(&res.ID, &res.RenterID, &res.RenterName, &res.PickupLocationID, &res.DropoffLocationID,
		&res.Region, &res.ClassCode, &res.PickupDate, &res.ReturnDate, &vehicleVIN, &res.RateTotal, &res.Currency,
		&res.Status, &res.Version, &histJSON, &res.CreatedAt, &res.UpdatedAt); err != nil {
		return nil, false
	}
	res.VehicleVIN = vehicleVIN
	json.Unmarshal([]byte(histJSON), &res.History)
	elapsed := msSince(start)

	evRows, _ := e.Store.DB.QueryContext(ctx, "SELECT id, at, sim_at, reservation_id, action, location_id, rental_date FROM reservation_events WHERE reservation_id=$1 ORDER BY id DESC LIMIT 50", id)
	var events []store.Event
	if evRows != nil {
		for evRows.Next() {
			var ev store.Event
			var fd time.Time
			if evRows.Scan(&ev.ID, &ev.CreatedAt, &ev.SimAt, &ev.ReservationID, &ev.Kind, &ev.LocationID, &fd) == nil {
				ev.RentalDate = fd
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
		return "id + pickup_location_id + pickup_date — primary key AND idx_res_pickup_date both satisfied"
	}
	return "id only — primary key lookup, still fast at this scale, but doesn't exercise idx_res_pickup_date"
}
