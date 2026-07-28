package sim

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// horizonBackDays / horizonForwardDays bound the pre-seeded (and continuously rolled
// forward — see runFleetOpsAgent) flight_inventory window relative to simulated
// "today". Same shape as Hotel Sim's inventory horizon.
const (
	horizonBackDays    = 2
	horizonForwardDays = 21
)

// InventoryID builds the deterministic composite id flight_inventory uses instead of
// relying on a separate auto-increment key plus a unique index — this makes every
// inventory seed/upsert idempotent by construction (INSERT IGNORE on a duplicate id
// is simply a no-op).
func InventoryID(routeID string, code SeatClassCode, date time.Time) string {
	return fmt.Sprintf("%s|%s|%s", routeID, code, date.Format("2006-01-02"))
}

// NewConfirmationNumber builds the reservation id: route + flight date + a
// zero-padded sequence, e.g. "R137-260728-0001".
func NewConfirmationNumber(routeID string, flightDate time.Time, seq int64) string {
	return fmt.Sprintf("%s-%s-%04d", routeID, flightDate.Format("060102"), seq%10000)
}

// seedIfEmpty inserts the static route/seat-class/aircraft topology and the initial
// flight_inventory horizon exactly once (checked via the routes table being empty) —
// never re-run on every Start, only when the schema is genuinely fresh (a first
// boot, or right after Reset's wipe).
func (e *Engine) seedIfEmpty(ctx context.Context) {
	var n int64
	if err := e.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM routes").Scan(&n); err != nil {
		log.Printf("airlinesim: seed check: %v", err)
		return
	}
	if n > 0 {
		return
	}

	log.Printf("airlinesim: seeding %d routes + %d aircraft + inventory horizon", len(e.World.Routes), len(e.Fleet))

	now := time.Now().UTC()
	if err := e.seedRoutes(ctx, now); err != nil {
		log.Printf("airlinesim: seed routes: %v", err)
	}
	if err := e.seedAircraft(ctx, now); err != nil {
		log.Printf("airlinesim: seed aircraft: %v", err)
	}

	today := e.Clock.Today()
	start := today.AddDate(0, 0, -horizonBackDays)
	end := today.AddDate(0, 0, horizonForwardDays)
	e.seedInventoryRange(ctx, start, end)
}

func (e *Engine) seedRoutes(ctx context.Context, now time.Time) error {
	var routeVals []string
	var routeArgs []any
	var classVals []string
	var classArgs []any
	for _, r := range e.World.Routes {
		routeVals = append(routeVals, "(?,?,?,?,?,?,?,?,?,?,?)")
		routeArgs = append(routeArgs, r.ID, r.FlightNumber, r.Origin, r.Destination, string(r.Region), string(r.SizeTier),
			r.BaseFare, r.Popularity, "open", 0.0, now)
		for _, sc := range r.SeatClasses {
			classVals = append(classVals, "(?,?,?,?,?)")
			classArgs = append(classArgs, r.ID, string(sc.Code), sc.Name, sc.SeatCount, sc.FareMult)
		}
	}
	if _, err := e.Store.DB.ExecContext(ctx,
		"INSERT INTO routes (id, flight_number, origin, destination, region, size_tier, base_fare, popularity, operational_status, current_load_factor, last_updated) VALUES "+strings.Join(routeVals, ","),
		routeArgs...); err != nil {
		return fmt.Errorf("insert routes: %w", err)
	}
	if _, err := e.Store.DB.ExecContext(ctx,
		"INSERT INTO seat_classes (route_id, class_code, name, seat_count, fare_mult) VALUES "+strings.Join(classVals, ","),
		classArgs...); err != nil {
		return fmt.Errorf("insert seat_classes: %w", err)
	}
	return nil
}

// seedAircraft batches the 2000-row fleet insert to stay well under MySQL's
// max_allowed_packet / placeholder-count limits.
func (e *Engine) seedAircraft(ctx context.Context, now time.Time) error {
	const batchSize = 250
	for i := 0; i < len(e.Fleet); i += batchSize {
		end := i + batchSize
		if end > len(e.Fleet) {
			end = len(e.Fleet)
		}
		var vals []string
		var args []any
		for _, ac := range e.Fleet[i:end] {
			vals = append(vals, "(?,?,?,?,?,?,?)")
			args = append(args, ac.TailNumber, ac.Type, string(ac.SizeTier), ac.HomeBase, ac.RouteID, string(ac.Status), now)
		}
		if _, err := e.Store.DB.ExecContext(ctx,
			"INSERT INTO aircraft (tail_number, type, size_tier, home_base, route_id, status, last_updated) VALUES "+strings.Join(vals, ","),
			args...); err != nil {
			return fmt.Errorf("insert aircraft batch %d: %w", i, err)
		}
	}
	return nil
}

// seedInventoryRange inserts one flight_inventory row per route x seat class x day
// in [start, end), batched to stay under placeholder limits. INSERT IGNORE so a day
// already seeded by a prior partial run or a concurrent horizon-roller tick doesn't
// abort the rest of the batch.
func (e *Engine) seedInventoryRange(ctx context.Context, start, end time.Time) {
	const batchSize = 500
	var vals []string
	var args []any
	now := time.Now().UTC()
	flush := func() {
		if len(vals) == 0 {
			return
		}
		_, err := e.Store.DB.ExecContext(ctx,
			"INSERT IGNORE INTO flight_inventory (id, route_id, region, class_code, flight_date, tail_number, total_seats, booked_seats, held_seats, unavailable_seats, available_seats, closed, fare, last_updated) VALUES "+strings.Join(vals, ","),
			args...)
		if err != nil {
			log.Printf("airlinesim: seed inventory batch: %v", err)
		}
		vals, args = nil, nil
	}
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		for _, r := range e.World.Routes {
			tail := e.pickTailForDate(r, d)
			for _, sc := range r.SeatClasses {
				vals = append(vals, "(?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
				args = append(args, InventoryID(r.ID, sc.Code, d), r.ID, string(r.Region), string(sc.Code), d.Format("2006-01-02"),
					tail, sc.SeatCount, 0, 0, 0, sc.SeatCount, false, round2(r.BaseFare*sc.FareMult), now)
				if len(vals) >= batchSize {
					flush()
				}
			}
		}
	}
	flush()
}
