package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Heartbeat upserts one agents/<name> row. No agent goroutines exist yet as
// of stage S0 (that's stage S2's workload engine) — this exists now so the
// snapshot/dashboard plumbing has something real to read while it's built.
func (s *Store) Heartbeat(ctx context.Context, name, status, detail string) {
	now := time.Now().UTC()
	s.DB.ExecContext(ctx,
		"INSERT INTO agents (agent_name, status, last_tick, detail, updated_at) VALUES (?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE status=VALUES(status), last_tick=VALUES(last_tick), detail=VALUES(detail), updated_at=VALUES(updated_at)",
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

// PutMetrics/GetMetrics implement the shared "one JSON blob row, upserted,
// read back verbatim" idiom every sibling sim uses for its metrics/sim_state
// tables — BuildSnapshot drops GetMetrics's result straight into the
// response as json.RawMessage, no re-marshaling needed.
func (s *Store) PutMetrics(ctx context.Context, id string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		"INSERT INTO metrics (id, payload, updated_at) VALUES (?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE payload=VALUES(payload), updated_at=VALUES(updated_at)",
		id, b, time.Now().UTC())
	return err
}

func (s *Store) GetMetrics(ctx context.Context, id string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := s.DB.QueryRowContext(ctx, "SELECT payload FROM metrics WHERE id = ?", id).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return raw, err
}
