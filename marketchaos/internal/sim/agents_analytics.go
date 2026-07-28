package sim

import (
	"context"
	"math/rand"
	"time"
)

// ----------------------------------------------------------------- scanner

// runScannerAgent is a market-scanner tool: alternates between "most active"
// (top volume) and "top movers" (widest day range) reads against the small,
// fixed-size market_quotes table (one row per security — 200 rows, never
// grows) — cheap by construction, not a "bad query" example (see the query
// education panel for that, stage S4+).
func (e *Engine) runScannerAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "scanner", "idle", detailStr(events, errs))
			return
		}
		n := opsThisTick(e.agentRate(sharesFor(e.Mix()).Scanner), time.Second)
		for i := 0; i < n; i++ {
			octx, cancel := opCtx(ctx)
			ok := e.runScan(octx, rng)
			cancel()
			if ok {
				events++
			} else {
				errs++
				e.counters.agentErrors.Add(1)
			}
		}
		e.Store.Heartbeat(ctx, "scanner", "ok", detailStr(events, errs))
	})
}

func (e *Engine) runScan(ctx context.Context, rng *rand.Rand) bool {
	query := "SELECT security_id, volume FROM market_quotes ORDER BY volume DESC LIMIT 20"
	if rng.Intn(2) == 1 {
		query = "SELECT security_id, (day_high-day_low) AS spread FROM market_quotes ORDER BY spread DESC LIMIT 20"
	}
	rows, err := e.Store.DB.QueryContext(ctx, query)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var a int
		var b float64
		rows.Scan(&a, &b)
	}
	return rows.Err() == nil
}

// -------------------------------------------------------------- dashboard-poll

// runDashboardPollAgent simulates many simultaneous dashboard viewers each
// polling their own portfolio summary — the agent the future "dashboard
// polling storm" challenge (spec category I2) dials up: at Extreme with a
// dashboard-poll-heavy mix, this alone can generate a meaningful fraction of
// total read load, which is the point.
func (e *Engine) runDashboardPollAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, 500*time.Millisecond, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "dashboard-poll", "idle", detailStr(events, errs))
			return
		}
		n := opsThisTick(e.agentRate(sharesFor(e.Mix()).DashboardPoll), 500*time.Millisecond)
		for i := 0; i < n; i++ {
			octx, cancel := opCtx(ctx)
			ok := e.dashboardSummary(octx, rng)
			cancel()
			if ok {
				events++
			} else {
				errs++
				e.counters.agentErrors.Add(1)
			}
		}
		e.Store.Heartbeat(ctx, "dashboard-poll", "ok", detailStr(events, errs))
	})
}

func (e *Engine) dashboardSummary(ctx context.Context, rng *rand.Rand) bool {
	accountID := e.randAccountID(rng)
	if accountID == 0 {
		return false
	}
	rows, err := e.Store.DB.QueryContext(ctx,
		`SELECT p.security_id, p.quantity, q.last_price FROM positions p
		 JOIN market_quotes q ON q.security_id=p.security_id
		 WHERE p.account_id=?`, accountID)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var secID, qty int
		var last float64
		rows.Scan(&secID, &qty, &last)
	}
	return rows.Err() == nil
}
