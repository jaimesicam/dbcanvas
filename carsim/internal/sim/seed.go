package sim

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// horizonBackDays / horizonForwardDays bound the pre-seeded (and continuously
// rolled forward — see runFleetOpsAgent) rental_inventory window relative to
// simulated "today". Same shape as Airline Sim's / Hotel Sim's inventory horizon.
const (
	horizonBackDays    = 2
	horizonForwardDays = 21
)

// InventoryID builds the deterministic composite id rental_inventory uses
// instead of relying on a separate sequence plus a unique index — this makes
// every inventory seed/upsert idempotent by construction (ON CONFLICT DO NOTHING
// on a duplicate id is simply a no-op).
func InventoryID(locationID string, code VehicleClassCode, date time.Time) string {
	return fmt.Sprintf("%s|%s|%s", locationID, code, date.Format("2006-01-02"))
}

// NewConfirmationNumber builds the reservation id: location + pickup date + a
// zero-padded sequence, e.g. "L137-260728-0001".
func NewConfirmationNumber(locationID string, pickupDate time.Time, seq int64) string {
	return fmt.Sprintf("%s-%s-%04d", locationID, pickupDate.Format("060102"), seq%10000)
}

// placeholderGroup returns "($n,$n+1,...,$n+cols-1)" and advances *next by cols
// — the $N-numbered analog of MySQL's positional "(?,?,...)" used for batch
// VALUES inserts, where placeholder numbers must stay unique and increasing
// across the entire statement rather than resetting per row.
func placeholderGroup(next *int, cols int) string {
	ph := make([]string, cols)
	for i := 0; i < cols; i++ {
		ph[i] = fmt.Sprintf("$%d", *next)
		*next++
	}
	return "(" + strings.Join(ph, ",") + ")"
}

// seedIfEmpty inserts the static location/vehicle-class/vehicle topology and the
// initial rental_inventory horizon exactly once (checked via the locations table
// being empty) — never re-run on every Start, only when the schema is genuinely
// fresh (a first boot, or right after Reset's wipe).
func (e *Engine) seedIfEmpty(ctx context.Context) {
	var n int64
	if err := e.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM locations").Scan(&n); err != nil {
		log.Printf("carsim: seed check: %v", err)
		return
	}
	if n > 0 {
		return
	}

	log.Printf("carsim: seeding %d locations + %d vehicles + inventory horizon", len(e.World.Locations), len(e.Fleet))

	now := time.Now().UTC()
	if err := e.seedLocations(ctx, now); err != nil {
		log.Printf("carsim: seed locations: %v", err)
	}
	if err := e.seedVehicles(ctx, now); err != nil {
		log.Printf("carsim: seed vehicles: %v", err)
	}

	today := e.Clock.Today()
	start := today.AddDate(0, 0, -horizonBackDays)
	end := today.AddDate(0, 0, horizonForwardDays)
	e.seedInventoryRange(ctx, start, end)
}

func (e *Engine) seedLocations(ctx context.Context, now time.Time) error {
	var locVals []string
	var locArgs []any
	var classVals []string
	var classArgs []any
	locNext, classNext := 1, 1
	for _, l := range e.World.Locations {
		locVals = append(locVals, placeholderGroup(&locNext, 10))
		locArgs = append(locArgs, l.ID, l.Code, l.Name, string(l.Region), string(l.SizeTier),
			l.BaseRate, l.Popularity, "open", 0.0, now)
		for _, vc := range l.VehicleClasses {
			classVals = append(classVals, placeholderGroup(&classNext, 5))
			classArgs = append(classArgs, l.ID, string(vc.Code), vc.Name, vc.FleetCount, vc.RateMult)
		}
	}
	if _, err := e.Store.DB.ExecContext(ctx,
		"INSERT INTO locations (id, code, name, region, size_tier, base_rate, popularity, operational_status, current_utilization, last_updated) VALUES "+strings.Join(locVals, ","),
		locArgs...); err != nil {
		return fmt.Errorf("insert locations: %w", err)
	}
	if _, err := e.Store.DB.ExecContext(ctx,
		"INSERT INTO vehicle_classes (location_id, class_code, name, fleet_count, rate_mult) VALUES "+strings.Join(classVals, ","),
		classArgs...); err != nil {
		return fmt.Errorf("insert vehicle_classes: %w", err)
	}
	return nil
}

// seedVehicles batches the 2000-row fleet insert to stay well under PostgreSQL's
// max placeholder-count-per-statement limit (65535).
func (e *Engine) seedVehicles(ctx context.Context, now time.Time) error {
	const batchSize = 250
	for i := 0; i < len(e.Fleet); i += batchSize {
		end := i + batchSize
		if end > len(e.Fleet) {
			end = len(e.Fleet)
		}
		var vals []string
		var args []any
		next := 1
		for _, v := range e.Fleet[i:end] {
			vals = append(vals, placeholderGroup(&next, 7))
			args = append(args, v.VIN, v.MakeModel, string(v.ClassCode), v.HomeLocationID, v.CurrentLocationID, string(v.Status), now)
		}
		if _, err := e.Store.DB.ExecContext(ctx,
			"INSERT INTO vehicles (vin, make_model, class_code, home_location_id, current_location_id, status, last_updated) VALUES "+strings.Join(vals, ","),
			args...); err != nil {
			return fmt.Errorf("insert vehicles batch %d: %w", i, err)
		}
	}
	return nil
}

// seedInventoryRange inserts one rental_inventory row per location x vehicle
// class x day in [start, end), batched to stay under placeholder limits. ON
// CONFLICT DO NOTHING so a day already seeded by a prior partial run or a
// concurrent horizon-roller tick doesn't abort the rest of the batch.
func (e *Engine) seedInventoryRange(ctx context.Context, start, end time.Time) {
	const batchSize = 400 // 400 rows * 14 cols = 5600 placeholders, comfortably under the 65535 limit
	var vals []string
	var args []any
	next := 1
	now := time.Now().UTC()
	flush := func() {
		if len(vals) == 0 {
			return
		}
		_, err := e.Store.DB.ExecContext(ctx,
			"INSERT INTO rental_inventory (id, location_id, region, class_code, date, total_vehicles, booked_vehicles, available_vehicles, closed, rate, last_updated) VALUES "+strings.Join(vals, ",")+" ON CONFLICT (id) DO NOTHING",
			args...)
		if err != nil {
			log.Printf("carsim: seed inventory batch: %v", err)
		}
		vals, args = nil, nil
		next = 1
	}
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		for _, l := range e.World.Locations {
			for _, vc := range l.VehicleClasses {
				vals = append(vals, placeholderGroup(&next, 11))
				args = append(args, InventoryID(l.ID, vc.Code, d), l.ID, string(l.Region), string(vc.Code), d.Format("2006-01-02"),
					vc.FleetCount, 0, vc.FleetCount, false, round2(l.BaseRate*vc.RateMult), now)
				if len(vals) >= batchSize {
					flush()
				}
			}
		}
	}
	flush()
}
