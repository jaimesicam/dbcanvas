package sim

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"time"
)

// ------------------------------------------------------------------ pricing

// runPricingAgent recalculates rates from current utilization with a single
// multi-table UPDATE...JOIN (the SQL-native analog of Hotel Sim's server-side
// aggregation-pipeline pricing update) — rates rise up to 50% as a day's
// vehicles book up. Also refreshes each location's current_utilization from
// today's inventory, which is what the location-grid tiles color by.
func (e *Engine) runPricingAgent(ctx context.Context) {
	var events, errs int64
	tickLoop(ctx, 10*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "pricing", "idle", detailStr(events, errs))
			return
		}
		today := e.Clock.Today().Format("2006-01-02")
		_, err := e.Store.DB.ExecContext(ctx,
			`UPDATE rental_inventory ri
			 SET rate = ROUND((l.base_rate * vc.rate_mult * (1 + LEAST(ri.booked_vehicles::float8 / GREATEST(ri.total_vehicles,1), 1) * 0.5))::numeric, 2),
			     last_updated = $1
			 FROM locations l, vehicle_classes vc
			 WHERE l.id = ri.location_id AND vc.location_id = ri.location_id AND vc.class_code = ri.class_code
			   AND ri.date >= $2 AND ri.closed = false`,
			time.Now().UTC(), today)
		if err != nil {
			errs++
		} else {
			events++
		}
		if _, err := e.Store.DB.ExecContext(ctx,
			`UPDATE locations l
			 SET current_utilization = t.u, last_updated = $1
			 FROM (SELECT location_id, AVG(booked_vehicles::float8 / GREATEST(total_vehicles,1)) AS u FROM rental_inventory WHERE date = $2 GROUP BY location_id) t
			 WHERE t.location_id = l.id`,
			time.Now().UTC(), today); err != nil {
			errs++
		}
		e.Store.Heartbeat(ctx, "pricing", "ok", detailStr(events, errs))
	})
}

// -------------------------------------------------------------------- fleet ops

// runFleetOpsAgent rolls the rental_inventory date horizon forward as simulated
// days pass, randomly grounds/returns vehicles to service (which narrows/
// restores the pool pickVehicleForDate draws from for newly-seeded dates),
// cycles `cleaning` vehicles back to `available` once their turnaround is done,
// and prunes the append-only tables PostgreSQL has no TTL index to do this for
// automatically.
func (e *Engine) runFleetOpsAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, 15*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "fleet-ops", "idle", detailStr(events, errs))
			return
		}
		today := e.Clock.Today()
		wantThrough := today.AddDate(0, 0, horizonForwardDays)
		var maxDate sql.NullTime
		if err := e.Store.DB.QueryRowContext(ctx, "SELECT MAX(date) FROM rental_inventory").Scan(&maxDate); err == nil && maxDate.Valid && maxDate.Time.Before(wantThrough) {
			e.seedInventoryRange(ctx, maxDate.Time.AddDate(0, 0, 1), wantThrough.AddDate(0, 0, 1))
			events++
		}

		// Random maintenance/return-to-service and cleaning-turnaround — small
		// probability per tick per vehicle, kept as bulk in-memory + one bulk-ish
		// DB write per tick rather than 2000 individual round trips.
		var toMaintenance, toActive, toAvailable []string
		for _, v := range e.Fleet {
			switch e.vehicleStatus(v.VIN) {
			case VehicleAvailable:
				if rng.Float64() < 0.0015 {
					toMaintenance = append(toMaintenance, v.VIN)
				}
			case VehicleMaintenance:
				if rng.Float64() < 0.05 {
					toActive = append(toActive, v.VIN)
				}
			case VehicleCleaning:
				if rng.Float64() < 0.3 { // cleaning turnaround is quick — most tick out within a minute of real time
					toAvailable = append(toAvailable, v.VIN)
				}
			}
		}
		if len(toMaintenance) > 0 {
			e.updateVehicleStatus(ctx, toMaintenance, VehicleMaintenance)
			events++
		}
		if len(toActive) > 0 {
			e.updateVehicleStatus(ctx, toActive, VehicleAvailable)
			events++
		}
		if len(toAvailable) > 0 {
			e.updateVehicleStatus(ctx, toAvailable, VehicleAvailable)
			events++
		}

		if _, err := e.Store.PruneEvents(ctx, time.Now().Add(-24*time.Hour)); err != nil {
			errs++
		}
		if _, err := e.Store.PruneRequests(ctx, time.Now().Add(-1*time.Hour)); err != nil {
			errs++
		}
		if err := e.Store.PruneQuerySamples(ctx, 20000); err != nil {
			errs++
		}
		e.Store.Heartbeat(ctx, "fleet-ops", "ok", detailStr(events, errs))
	})
}

func (e *Engine) updateVehicleStatus(ctx context.Context, vins []string, status VehicleStatus) {
	placeholders, args := inClause(vins, 3)
	args = append([]any{string(status), time.Now().UTC()}, args...)
	if _, err := e.Store.DB.ExecContext(ctx, "UPDATE vehicles SET status=$1, last_updated=$2 WHERE vin IN ("+placeholders+")", args...); err != nil {
		log.Printf("carsim: update vehicle status: %v", err)
		return
	}
	for _, v := range vins {
		e.setVehicleStatus(v, status)
	}
}

// ---------------------------------------------------------------------- analytics

// MetricsPayload is the metrics/id:"current" row's JSON payload — the only path
// through which anything the atomic counters track becomes visible to
// BuildSnapshot.
type MetricsPayload struct {
	SoldOut            int64              `json:"soldOut"`
	DuplicatesRejected int64              `json:"duplicatesRejected"`
	EventWriteErrors   int64              `json:"eventWriteErrors"`
	ReservationsTotal  int64              `json:"reservationsTotal"`
	CancellationsTotal int64              `json:"cancellationsTotal"`
	ModificationsTotal int64              `json:"modificationsTotal"`
	CheckOutsTotal     int64              `json:"checkOutsTotal"`
	CheckInsTotal      int64              `json:"checkInsTotal"`
	NoShowsTotal       int64              `json:"noShowsTotal"`
	SearchesTotal      int64              `json:"searchesTotal"`
	StatusCounts       map[string]int64   `json:"statusCounts"`
	RegionUtilization  map[string]float64 `json:"regionUtilization"`
	TopLocations       []TopLocation      `json:"topLocations"`
	ActiveRenters      int                `json:"activeRenters"`
	UpdatedAt          time.Time          `json:"updatedAt"`
}

type TopLocation struct {
	LocationID string `json:"locationId"`
	Name       string `json:"name"`
	Bookings   int64  `json:"bookings"`
}

func (e *Engine) runAnalyticsAgent(ctx context.Context) {
	var events, errs int64
	tickLoop(ctx, 5*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "analytics", "idle", detailStr(events, errs))
			return
		}
		payload := MetricsPayload{
			SoldOut: e.counters.soldOut.Load(), DuplicatesRejected: e.counters.duplicatesRejected.Load(),
			EventWriteErrors:  e.counters.eventWriteErrors.Load(),
			ReservationsTotal: e.counters.reservationsTotal.Load(), CancellationsTotal: e.counters.cancellationsTotal.Load(),
			ModificationsTotal: e.counters.modificationsTotal.Load(), CheckOutsTotal: e.counters.checkOutsTotal.Load(),
			CheckInsTotal: e.counters.checkInsTotal.Load(), NoShowsTotal: e.counters.noShowsTotal.Load(),
			SearchesTotal: e.counters.searchesTotal.Load(), ActiveRenters: e.sessionCount(),
			StatusCounts: map[string]int64{}, RegionUtilization: map[string]float64{}, UpdatedAt: time.Now().UTC(),
		}
		if rows, err := e.Store.DB.QueryContext(ctx, "SELECT status, COUNT(*) FROM reservations GROUP BY status"); err == nil {
			for rows.Next() {
				var status string
				var n int64
				if rows.Scan(&status, &n) == nil {
					payload.StatusCounts[status] = n
				}
			}
			rows.Close()
		} else {
			errs++
		}
		if rows, err := e.Store.DB.QueryContext(ctx, "SELECT region, AVG(current_utilization) FROM locations GROUP BY region"); err == nil {
			for rows.Next() {
				var region string
				var u float64
				if rows.Scan(&region, &u) == nil {
					payload.RegionUtilization[region] = u
				}
			}
			rows.Close()
		} else {
			errs++
		}
		if rows, err := e.Store.DB.QueryContext(ctx,
			`SELECT r.pickup_location_id, l.name, COUNT(*) c FROM reservations r JOIN locations l ON l.id = r.pickup_location_id
			 WHERE r.status != $1 GROUP BY r.pickup_location_id, l.name ORDER BY c DESC LIMIT 10`,
			string(StatusCancelled)); err == nil {
			for rows.Next() {
				var t TopLocation
				if rows.Scan(&t.LocationID, &t.Name, &t.Bookings) == nil {
					payload.TopLocations = append(payload.TopLocations, t)
				}
			}
			rows.Close()
		} else {
			errs++
		}

		b, _ := json.Marshal(payload)
		if _, err := e.Store.DB.ExecContext(ctx,
			"INSERT INTO metrics (id, payload, updated_at) VALUES ('current', $1, $2) ON CONFLICT (id) DO UPDATE SET payload=EXCLUDED.payload, updated_at=EXCLUDED.updated_at",
			string(b), payload.UpdatedAt); err != nil {
			errs++
		} else {
			events++
		}
		e.Store.Heartbeat(ctx, "analytics", "ok", detailStr(events, errs))
	})
}

// ---------------------------------------------------------------------- monitoring

// DiagPayload is the metrics/id:"diag" row's JSON payload — topology-conditional
// PostgreSQL-family diagnostics for the Topology panel.
type DiagPayload struct {
	Kind          TargetKind        `json:"kind"`
	ServerVersion string            `json:"serverVersion"`
	ActiveConns   string            `json:"activeConns,omitempty"`
	Uptime        string            `json:"uptime,omitempty"`
	PatroniStatus map[string]string `json:"patroniStatus,omitempty"`
	RepmgrStatus  map[string]string `json:"repmgrStatus,omitempty"`
	SpockStatus   map[string]string `json:"spockStatus,omitempty"`
	UpdatedAt     time.Time         `json:"updatedAt"`
}

func (e *Engine) runMonitoringAgent(ctx context.Context) {
	var events, errs int64
	tickLoop(ctx, 5*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "monitoring", "idle", detailStr(events, errs))
			return
		}
		payload := DiagPayload{Kind: e.Kind, ServerVersion: e.Store.ServerVersion(ctx), UpdatedAt: time.Now().UTC()}
		payload.ActiveConns = e.scalarString(ctx, "SELECT count(*)::text FROM pg_stat_activity")
		payload.Uptime = e.scalarString(ctx, "SELECT (now() - pg_postmaster_start_time())::text")
		switch e.Profile.Family {
		case "patroni":
			payload.PatroniStatus = map[string]string{
				"pg_is_in_recovery":     e.scalarString(ctx, "SELECT pg_is_in_recovery()::text"),
				"replication_lag_bytes": e.scalarString(ctx, "SELECT COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)::text, '') FROM pg_stat_replication LIMIT 1"),
			}
		case "repmgr":
			payload.RepmgrStatus = e.showRepmgrStatus(ctx)
		case "spock":
			payload.SpockStatus = e.showSpockStatus(ctx)
		}
		b, _ := json.Marshal(payload)
		if _, err := e.Store.DB.ExecContext(ctx,
			"INSERT INTO metrics (id, payload, updated_at) VALUES ('diag', $1, $2) ON CONFLICT (id) DO UPDATE SET payload=EXCLUDED.payload, updated_at=EXCLUDED.updated_at",
			string(b), payload.UpdatedAt); err != nil {
			errs++
		} else {
			events++
		}
		e.Store.Heartbeat(ctx, "monitoring", "ok", detailStr(events, errs))
	})
}

func (e *Engine) scalarString(ctx context.Context, query string) string {
	var v string
	if err := e.Store.DB.QueryRowContext(ctx, query).Scan(&v); err != nil {
		return ""
	}
	return v
}

// showRepmgrStatus reads repmgr's own bookkeeping schema, when reachable — best
// effort, returns nil rather than treating "repmgr schema not visible from this
// connection" as an error (e.g. a connection through HAProxy's read port that
// landed on a standby with a different search_path).
func (e *Engine) showRepmgrStatus(ctx context.Context) map[string]string {
	rows, err := e.Store.DB.QueryContext(ctx, "SELECT node_id::text, type, active::text FROM repmgr.nodes ORDER BY node_id")
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, typ, active string
		if rows.Scan(&id, &typ, &active) == nil {
			out["node_"+id] = typ + "/active=" + active
		}
	}
	return out
}

// showSpockStatus reads pg_stat_subscription — visible on every Spock member
// regardless of which one this connection landed on, unlike repmgr's
// primary-only bookkeeping view.
func (e *Engine) showSpockStatus(ctx context.Context) map[string]string {
	rows, err := e.Store.DB.QueryContext(ctx, "SELECT subname, received_lsn::text FROM pg_stat_subscription")
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, lsn string
		if rows.Scan(&name, &lsn) == nil {
			out[name] = lsn
		}
	}
	return out
}
