package sim

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"time"
)

// ---------------------------------------------------------------- fare pricing

// runFarePricingAgent recalculates fares from current load factor with a single
// multi-table UPDATE...JOIN (the SQL-native analog of Hotel Sim's server-side
// aggregation-pipeline pricing update) — fares rise up to 50% as a flight's seats
// fill up. Also refreshes each route's current_load_factor from today's inventory,
// which is what the route-grid tiles color by.
func (e *Engine) runFarePricingAgent(ctx context.Context) {
	var events, errs int64
	tickLoop(ctx, 10*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "fare-pricing", "idle", detailStr(events, errs))
			return
		}
		today := e.Clock.Today().Format("2006-01-02")
		_, err := e.Store.DB.ExecContext(ctx,
			`UPDATE flight_inventory fi
			 JOIN routes r ON r.id = fi.route_id
			 JOIN seat_classes sc ON sc.route_id = fi.route_id AND sc.class_code = fi.class_code
			 SET fi.fare = ROUND(r.base_fare * sc.fare_mult * (1 + LEAST(fi.booked_seats / GREATEST(fi.total_seats,1), 1) * 0.5), 2),
			     fi.last_updated = ?
			 WHERE fi.flight_date >= ? AND fi.closed = 0`,
			time.Now().UTC(), today)
		if err != nil {
			errs++
		} else {
			events++
		}
		if _, err := e.Store.DB.ExecContext(ctx,
			`UPDATE routes r
			 JOIN (SELECT route_id, AVG(booked_seats / GREATEST(total_seats,1)) AS lf FROM flight_inventory WHERE flight_date = ? GROUP BY route_id) t
			   ON t.route_id = r.id
			 SET r.current_load_factor = t.lf, r.last_updated = ?`,
			today, time.Now().UTC()); err != nil {
			errs++
		}
		e.Store.Heartbeat(ctx, "fare-pricing", "ok", detailStr(events, errs))
	})
}

// -------------------------------------------------------------------- fleet ops

// runFleetOpsAgent rolls the flight_inventory date horizon forward as simulated
// days pass, randomly grounds/returns aircraft to service (which narrows/restores
// the pool pickTailForDate draws from for newly-seeded dates), and prunes the
// append-only tables MySQL has no TTL index to do this for automatically.
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
		if err := e.Store.DB.QueryRowContext(ctx, "SELECT MAX(flight_date) FROM flight_inventory").Scan(&maxDate); err == nil && maxDate.Valid && maxDate.Time.Before(wantThrough) {
			e.seedInventoryRange(ctx, maxDate.Time.AddDate(0, 0, 1), wantThrough.AddDate(0, 0, 1))
			events++
		}

		// Random maintenance/return-to-service — small probability per tick per
		// aircraft, kept as bulk in-memory + one bulk-ish DB write per tick rather
		// than 2000 individual round trips.
		var toMaintenance, toActive []string
		for _, ac := range e.Fleet {
			switch e.aircraftStatus(ac.TailNumber) {
			case AircraftActive:
				if rng.Float64() < 0.0015 {
					toMaintenance = append(toMaintenance, ac.TailNumber)
				}
			case AircraftMaintenance:
				if rng.Float64() < 0.05 {
					toActive = append(toActive, ac.TailNumber)
				}
			}
		}
		if len(toMaintenance) > 0 {
			e.updateAircraftStatus(ctx, toMaintenance, AircraftMaintenance)
			events++
		}
		if len(toActive) > 0 {
			e.updateAircraftStatus(ctx, toActive, AircraftActive)
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

func (e *Engine) updateAircraftStatus(ctx context.Context, tails []string, status AircraftStatus) {
	placeholders, args := inClause(tails)
	args = append([]any{string(status), time.Now().UTC()}, args...)
	if _, err := e.Store.DB.ExecContext(ctx, "UPDATE aircraft SET status=?, last_updated=? WHERE tail_number IN ("+placeholders+")", args...); err != nil {
		log.Printf("airlinesim: update aircraft status: %v", err)
		return
	}
	for _, t := range tails {
		e.setAircraftStatus(t, status)
	}
}

// ---------------------------------------------------------------------- analytics

// MetricsPayload is the metrics/id:"current" row's JSON payload — the only path
// through which anything the atomic counters track becomes visible to
// BuildSnapshot.
type MetricsPayload struct {
	SoldOut            int64              `json:"soldOut"`
	DuplicatesRejected int64              `json:"duplicatesRejected"`
	TxnRetries         int64              `json:"txnRetries"`
	EventWriteErrors   int64              `json:"eventWriteErrors"`
	ReservationsTotal  int64              `json:"reservationsTotal"`
	CancellationsTotal int64              `json:"cancellationsTotal"`
	ModificationsTotal int64              `json:"modificationsTotal"`
	CheckInsTotal      int64              `json:"checkInsTotal"`
	CompletionsTotal   int64              `json:"completionsTotal"`
	NoShowsTotal       int64              `json:"noShowsTotal"`
	SearchesTotal      int64              `json:"searchesTotal"`
	StatusCounts       map[string]int64   `json:"statusCounts"`
	RegionLoadFactor   map[string]float64 `json:"regionLoadFactor"`
	TopRoutes          []TopRoute         `json:"topRoutes"`
	ActivePassengers   int                `json:"activePassengers"`
	UpdatedAt          time.Time          `json:"updatedAt"`
}

type TopRoute struct {
	RouteID      string `json:"routeId"`
	FlightNumber string `json:"flightNumber"`
	Bookings     int64  `json:"bookings"`
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
			TxnRetries: e.counters.txnRetries.Load(), EventWriteErrors: e.counters.eventWriteErrors.Load(),
			ReservationsTotal: e.counters.reservationsTotal.Load(), CancellationsTotal: e.counters.cancellationsTotal.Load(),
			ModificationsTotal: e.counters.modificationsTotal.Load(), CheckInsTotal: e.counters.checkInsTotal.Load(),
			CompletionsTotal: e.counters.completionsTotal.Load(), NoShowsTotal: e.counters.noShowsTotal.Load(),
			SearchesTotal: e.counters.searchesTotal.Load(), ActivePassengers: e.sessionCount(),
			StatusCounts: map[string]int64{}, RegionLoadFactor: map[string]float64{}, UpdatedAt: time.Now().UTC(),
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
		if rows, err := e.Store.DB.QueryContext(ctx, "SELECT region, AVG(current_load_factor) FROM routes GROUP BY region"); err == nil {
			for rows.Next() {
				var region string
				var lf float64
				if rows.Scan(&region, &lf) == nil {
					payload.RegionLoadFactor[region] = lf
				}
			}
			rows.Close()
		} else {
			errs++
		}
		if rows, err := e.Store.DB.QueryContext(ctx,
			`SELECT route_id, flight_number, COUNT(*) c FROM reservations WHERE status != ? GROUP BY route_id, flight_number ORDER BY c DESC LIMIT 10`,
			string(StatusCancelled)); err == nil {
			for rows.Next() {
				var t TopRoute
				if rows.Scan(&t.RouteID, &t.FlightNumber, &t.Bookings) == nil {
					payload.TopRoutes = append(payload.TopRoutes, t)
				}
			}
			rows.Close()
		} else {
			errs++
		}

		b, _ := json.Marshal(payload)
		if _, err := e.Store.DB.ExecContext(ctx,
			"INSERT INTO metrics (id, payload, updated_at) VALUES ('current', ?, ?) ON DUPLICATE KEY UPDATE payload=VALUES(payload), updated_at=VALUES(updated_at)",
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
// MySQL-family diagnostics for the Topology panel.
type DiagPayload struct {
	Kind             TargetKind        `json:"kind"`
	ServerVersion    string            `json:"serverVersion"`
	ThreadsConnected string            `json:"threadsConnected,omitempty"`
	Uptime           string            `json:"uptime,omitempty"`
	WsrepStatus      map[string]string `json:"wsrepStatus,omitempty"`
	ReplicaStatus    map[string]string `json:"replicaStatus,omitempty"`
	UpdatedAt        time.Time         `json:"updatedAt"`
}

func (e *Engine) runMonitoringAgent(ctx context.Context) {
	var events, errs int64
	tickLoop(ctx, 5*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "monitoring", "idle", detailStr(events, errs))
			return
		}
		payload := DiagPayload{Kind: e.Kind, ServerVersion: e.Store.ServerVersion(ctx), UpdatedAt: time.Now().UTC()}
		payload.ThreadsConnected = e.showStatusLike(ctx, "Threads_connected")
		payload.Uptime = e.showStatusLike(ctx, "Uptime")
		switch e.Profile.Family {
		case "pxc":
			payload.WsrepStatus = map[string]string{
				"wsrep_cluster_size":        e.showStatusLike(ctx, "wsrep_cluster_size"),
				"wsrep_cluster_status":      e.showStatusLike(ctx, "wsrep_cluster_status"),
				"wsrep_local_state_comment": e.showStatusLike(ctx, "wsrep_local_state_comment"),
				"wsrep_flow_control_paused": e.showStatusLike(ctx, "wsrep_flow_control_paused"),
			}
		case "mysql":
			// Only a secondary reports replica status; a primary connection (the
			// common case when talking through HAProxy's write port or directly to
			// a primary) legitimately returns nothing here — that's not an error.
			payload.ReplicaStatus = e.showReplicaStatus(ctx)
		}
		b, _ := json.Marshal(payload)
		if _, err := e.Store.DB.ExecContext(ctx,
			"INSERT INTO metrics (id, payload, updated_at) VALUES ('diag', ?, ?) ON DUPLICATE KEY UPDATE payload=VALUES(payload), updated_at=VALUES(updated_at)",
			string(b), payload.UpdatedAt); err != nil {
			errs++
		} else {
			events++
		}
		e.Store.Heartbeat(ctx, "monitoring", "ok", detailStr(events, errs))
	})
}

func (e *Engine) showStatusLike(ctx context.Context, name string) string {
	var varName, value string
	if err := e.Store.DB.QueryRowContext(ctx, "SHOW STATUS LIKE ?", name).Scan(&varName, &value); err != nil {
		return ""
	}
	return value
}

// showReplicaStatus tries the modern (8.0.22+) SHOW REPLICA STATUS first, falling
// back to the legacy SHOW SLAVE STATUS on older servers — best-effort, returns nil
// on a primary (no rows) rather than treating that as an error.
func (e *Engine) showReplicaStatus(ctx context.Context) map[string]string {
	rows, err := e.Store.DB.QueryContext(ctx, "SHOW REPLICA STATUS")
	if err != nil {
		rows, err = e.Store.DB.QueryContext(ctx, "SHOW SLAVE STATUS")
		if err != nil {
			return nil
		}
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil || !rows.Next() {
		return nil
	}
	vals := make([]sql.NullString, len(cols))
	scanArgs := make([]any, len(cols))
	for i := range vals {
		scanArgs[i] = &vals[i]
	}
	if rows.Scan(scanArgs...) != nil {
		return nil
	}
	want := map[string]bool{"Seconds_Behind_Master": true, "Seconds_Behind_Source": true, "Slave_IO_Running": true, "Replica_IO_Running": true, "Slave_SQL_Running": true, "Replica_SQL_Running": true}
	out := map[string]string{}
	for i, c := range cols {
		if want[c] && vals[i].Valid {
			out[c] = vals[i].String
		}
	}
	return out
}
