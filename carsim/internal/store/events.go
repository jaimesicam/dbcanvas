package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Event is one reservation_events row — the durable, replayable activity feed the
// eventfeed poller tails (there's no MySQL-style change-stream equivalent on
// PostgreSQL either — LISTEN/NOTIFY exists, but adds a second connection-lifecycle
// concern for no benefit over the same poll-by-id technique already proven in
// Hotel Sim and Airline Sim; this IS the mechanism, not a fallback) and the
// recent-activity panel reads.
type Event struct {
	ID            int64     `json:"id"`
	Kind          string    `json:"kind"`
	ReservationID string    `json:"reservationId,omitempty"`
	LocationID    string    `json:"locationId"`
	RentalDate    time.Time `json:"rentalDate,omitempty"`
	Agent         string    `json:"agent"`
	Detail        string    `json:"detail,omitempty"`
	CreatedAt     time.Time `json:"at"`
	SimAt         time.Time `json:"simAt"`
}

// AppendEvent inserts one reservation_events row (the durable source of truth a
// reconnecting client replays from) and returns it with its generated id.
// Best-effort from the caller's point of view — losing one costs an
// activity-feed line, never correctness, since every state transition that
// matters is already durable via its own guarded update or transaction before
// this is called.
func (s *Store) AppendEvent(ctx context.Context, ev Event) (Event, error) {
	ev.CreatedAt = time.Now().UTC()
	var rentalDate any
	if !ev.RentalDate.IsZero() {
		rentalDate = ev.RentalDate.Format("2006-01-02")
	}
	err := s.DB.QueryRowContext(ctx,
		"INSERT INTO reservation_events (at, sim_at, reservation_id, action, location_id, rental_date, agent, payload) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id",
		ev.CreatedAt, ev.SimAt, ev.ReservationID, ev.Kind, ev.LocationID, rentalDate, nullIfEmpty(ev.Agent), detailPayload(ev.Detail)).Scan(&ev.ID)
	if err != nil {
		return ev, err
	}
	return ev, nil
}

func detailPayload(detail string) []byte {
	b, _ := json.Marshal(map[string]string{"detail": detail})
	return b
}

const eventColumns = "id, at, sim_at, reservation_id, action, location_id, rental_date, agent, payload"

func scanEvents(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var rentalDate sql.NullTime
		var agent, payload sql.NullString
		if err := rows.Scan(&ev.ID, &ev.CreatedAt, &ev.SimAt, &ev.ReservationID, &ev.Kind, &ev.LocationID, &rentalDate, &agent, &payload); err != nil {
			return nil, err
		}
		if rentalDate.Valid {
			ev.RentalDate = rentalDate.Time
		}
		ev.Agent = agent.String
		if payload.Valid {
			var m map[string]string
			json.Unmarshal([]byte(payload.String), &m)
			ev.Detail = m["detail"]
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// EventsSince returns events with id > afterID, oldest first, up to limit — the
// eventfeed poller's cursor query (idx_events_at exists for the recent-activity
// panel's own "last N minutes" reads; this query walks the primary key instead,
// which is monotonic with insertion order same as the id sequence).
func (s *Store) EventsSince(ctx context.Context, afterID int64, limit int) ([]Event, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT "+eventColumns+" FROM reservation_events WHERE id > $1 ORDER BY id ASC LIMIT $2",
		afterID, limit)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// RecentEvents returns the most recent limit events, newest first — used to backfill
// a freshly-connected dashboard client before it starts following the live feed.
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]Event, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT "+eventColumns+" FROM reservation_events ORDER BY id DESC LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// MaxEventID returns the current highest reservation_events id (0 if the table is
// empty) — the eventfeed poller's starting cursor on a fresh boot, so it doesn't
// replay the entire history as "new" events.
func (s *Store) MaxEventID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, "SELECT MAX(id) FROM reservation_events").Scan(&id); err != nil {
		return 0, err
	}
	return id.Int64, nil
}

// PruneEvents deletes reservation_events rows older than olderThan — the manual
// equivalent of Hotel Sim's 24h TTL index, since PostgreSQL has no per-row expiry.
// Run periodically by the fleet-ops agent. ctid-based subselect mirrors Airline
// Sim's LIMIT-bounded DELETE (Postgres DELETE has no top-level LIMIT clause).
func (s *Store) PruneEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.DB.ExecContext(ctx,
		"DELETE FROM reservation_events WHERE ctid IN (SELECT ctid FROM reservation_events WHERE at < $1 LIMIT 5000)", olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneRequests deletes reservation_requests rows older than olderThan — the manual
// equivalent of Hotel Sim's 1h TTL index on the idempotency dedup table.
func (s *Store) PruneRequests(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.DB.ExecContext(ctx,
		"DELETE FROM reservation_requests WHERE ctid IN (SELECT ctid FROM reservation_requests WHERE created_at < $1 LIMIT 5000)", olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Heartbeat upserts one agents/<name> row.
func (s *Store) Heartbeat(ctx context.Context, name, status, detail string) {
	now := time.Now().UTC()
	s.DB.ExecContext(ctx,
		"INSERT INTO agents (agent_name, status, last_tick, detail, updated_at) VALUES ($1,$2,$3,$4,$5) "+
			"ON CONFLICT (agent_name) DO UPDATE SET status=EXCLUDED.status, last_tick=EXCLUDED.last_tick, detail=EXCLUDED.detail, updated_at=EXCLUDED.updated_at",
		name, status, now, detail, now)
}

// AgentHeartbeat is one agents row, as read back for the Agents panel.
type AgentHeartbeat struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	LastTick  time.Time `json:"lastTick"`
	Detail    string    `json:"detail,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (s *Store) AllHeartbeats(ctx context.Context) ([]AgentHeartbeat, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT agent_name, status, last_tick, detail, updated_at FROM agents ORDER BY agent_name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentHeartbeat
	for rows.Next() {
		var h AgentHeartbeat
		var detail sql.NullString
		if err := rows.Scan(&h.Name, &h.Status, &h.LastTick, &detail, &h.UpdatedAt); err != nil {
			return nil, err
		}
		h.Detail = detail.String
		out = append(out, h)
	}
	return out, rows.Err()
}

// QuerySample is one recorded operation for the query-education panel — stored in
// query_samples, pruned to the most recent N rows by PruneQuerySamples since
// PostgreSQL has no capped-collection equivalent.
type QuerySample struct {
	ID           int64     `json:"id"`
	At           time.Time `json:"at"`
	Kind         string    `json:"kind"` // "targeted" | "scatter"
	SQLText      string    `json:"sqlText"`
	RowsExamined int64     `json:"rowsExamined"`
	RowsReturned int64     `json:"rowsReturned"`
	IndexUsed    string    `json:"indexUsed,omitempty"`
	DurationMs   float64   `json:"durationMs"`
}

func (s *Store) RecordQuerySample(ctx context.Context, qs QuerySample) {
	qs.At = time.Now().UTC()
	s.DB.ExecContext(ctx,
		"INSERT INTO query_samples (at, kind, sql_text, rows_examined, rows_returned, index_used, duration_ms) VALUES ($1,$2,$3,$4,$5,$6,$7)",
		qs.At, qs.Kind, qs.SQLText, qs.RowsExamined, qs.RowsReturned, nullIfEmpty(qs.IndexUsed), qs.DurationMs)
}

func (s *Store) RecentQuerySamples(ctx context.Context, limit int) ([]QuerySample, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT id, at, kind, sql_text, rows_examined, rows_returned, index_used, duration_ms FROM query_samples ORDER BY id DESC LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuerySample
	for rows.Next() {
		var q QuerySample
		var idx sql.NullString
		if err := rows.Scan(&q.ID, &q.At, &q.Kind, &q.SQLText, &q.RowsExamined, &q.RowsReturned, &idx, &q.DurationMs); err != nil {
			return nil, err
		}
		q.IndexUsed = idx.String
		out = append(out, q)
	}
	return out, rows.Err()
}

// PruneQuerySamples keeps only the most recent keep rows — the manual equivalent
// of a capped collection.
func (s *Store) PruneQuerySamples(ctx context.Context, keep int) error {
	_, err := s.DB.ExecContext(ctx,
		"DELETE FROM query_samples WHERE id <= (SELECT id FROM (SELECT id FROM query_samples ORDER BY id DESC LIMIT 1 OFFSET $1) t)",
		keep)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
