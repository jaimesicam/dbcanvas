package sim

import (
	"context"

	"marketchaos/internal/store"
)

// Snapshot is the /api/state payload. As of stage S0 it only reports
// connection/control state and agent heartbeats — the market/trading/
// portfolio/database-performance/PXC/leaderboard/challenge panels' own
// snapshot fields are added alongside their stages (S1-S6).
type Snapshot struct {
	ServerVersion string                 `json:"serverVersion"`
	Agents        []store.AgentHeartbeat `json:"agents"`
	Control       ControlInfo            `json:"control"`
	Seed          SeedProgress           `json:"seed"`
	UptimeSec     int64                  `json:"uptimeSeconds"`
	Error         string                 `json:"error,omitempty"`
}

type ControlInfo struct {
	State string `json:"state"` // "running" | "paused"
	Level string `json:"level"` // stop|low|medium|high|extreme|custom
	Mix   string `json:"mix"`   // balanced|read-heavy|write-heavy|analytics-heavy|contention-heavy|pxc-conflict-heavy
	Kind  string `json:"kind"`
}

func runningState(running bool) string {
	if running {
		return "running"
	}
	return "paused"
}

// BuildSnapshot assembles the /api/state payload. If MySQL is unreachable,
// it still returns a Snapshot (Error set, everything else empty) rather than
// an error — the web interface must be able to show "can't reach MySQL"
// rather than going blank, and the same Ping failure is what the container's
// own -healthcheck exec (via /healthz) fails on.
func (e *Engine) BuildSnapshot(ctx context.Context) Snapshot {
	snap := Snapshot{
		Control:   ControlInfo{State: runningState(e.Running()), Level: string(e.Level()), Mix: string(e.Mix()), Kind: string(e.Kind)},
		Seed:      e.SeedProgress(),
		UptimeSec: e.UptimeSeconds(),
	}
	if err := e.Store.Ping(ctx); err != nil {
		snap.Error = "cannot reach mysql: " + err.Error()
		return snap
	}
	snap.ServerVersion = e.Store.ServerVersion(ctx)
	if agents, err := e.Store.AllHeartbeats(ctx); err == nil {
		snap.Agents = agents
	}
	return snap
}
