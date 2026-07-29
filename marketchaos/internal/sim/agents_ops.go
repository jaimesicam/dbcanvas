package sim

import (
	"context"
	"math/rand"
	"time"
)

// ------------------------------------------------------------- compliance

// runComplianceAgent looks at the most recently executed trades and flags
// unusually large ones into audit_events — a lightweight stand-in for a real
// exchange's surveillance system. It also runs 3 smaller compliance-style
// reads each tick (recent-trades view, today's-order-count, orders-by-
// account) that exist mainly to give the deep-offset-pagination,
// unbounded-trade-history, function-wrapped-timestamp, and
// pxc-ddl-during-load challenges real, continuous traffic to grade against.
func (e *Engine) runComplianceAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, 2*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "compliance", "idle", detailStr(events, errs))
			return
		}
		n := opsThisTick(e.agentRate(sharesFor(e.Mix()).Compliance), 2*time.Second)
		for i := 0; i < n; i++ {
			octx, cancel := opCtx(ctx)
			flagged, err := e.complianceCheck(octx)
			cancel()
			if err != nil {
				errs++
				e.counters.agentErrors.Add(1)
				continue
			}
			events += int64(flagged)
		}
		octx, cancel := opCtx(ctx)
		e.recentTradesView(octx)
		e.dailyOrderCount(octx)
		e.ordersByAccount(octx, rng)
		cancel()
		e.Store.Heartbeat(ctx, "compliance", "ok", detailStr(events, errs))
	})
}

// recentTradesView is deep-offset-pagination's and unbounded-trade-history's
// target — a plain "recent trades" list. Reference: LIMIT 20, no offset.
// deep-offset-pagination's bad state adds a large OFFSET (still bounded by
// LIMIT, but forces MySQL to scan and discard everything before it);
// unbounded-trade-history's bad state drops LIMIT/OFFSET entirely. Only one
// of the two challenges is ever active at a time, so both safely share this
// one query and shape.
func (e *Engine) recentTradesView(ctx context.Context) {
	query := "SELECT trade_id, security_id, price, executed_at FROM trades ORDER BY trade_id DESC LIMIT 20"
	switch e.ActiveVariant() {
	case "deep-offset-pagination":
		query = "SELECT trade_id, security_id, price, executed_at FROM trades ORDER BY trade_id DESC LIMIT 20 OFFSET 5000"
	case "unbounded-trade-history":
		query = "SELECT trade_id, security_id, price, executed_at FROM trades ORDER BY trade_id DESC"
	}
	rows, err := e.Store.DB.QueryContext(ctx, query)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, secID int64
		var price float64
		var executedAt time.Time
		rows.Scan(&id, &secID, &price, &executedAt)
	}
}

// dailyOrderCount is function-wrapped-timestamp's target. Reference: a
// sargable range predicate. Bad: DATE(created_at) = ?, which defeats any
// index on created_at outright regardless of what exists.
func (e *Engine) dailyOrderCount(ctx context.Context) {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var n int
	if e.ActiveVariant() == "function-wrapped-timestamp" {
		e.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM orders WHERE DATE(created_at) = ?", today.Format("2006-01-02")).Scan(&n)
		return
	}
	e.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM orders WHERE created_at >= ? AND created_at < ?",
		today, today.AddDate(0, 0, 1)).Scan(&n)
}

// ordersByAccount is pxc-ddl-during-load's target — no bad variant of its
// own (that challenge's bad state is a dropped index, applied via Setup,
// not an app-side query change).
func (e *Engine) ordersByAccount(ctx context.Context, rng *rand.Rand) {
	accountID := e.randAccountID(rng)
	if accountID == 0 {
		return
	}
	rows, err := e.Store.DB.QueryContext(ctx,
		"SELECT order_id, side, quantity, status FROM orders WHERE account_id=? ORDER BY created_at DESC LIMIT 20", accountID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var side, status string
		var qty int
		rows.Scan(&id, &side, &qty, &status)
	}
}

// complianceLargeTradeThreshold is the trade_value above which a trade gets
// flagged — arbitrary but well above a typical retail trade's size (see
// placeOrder's 1-500 share quantities against typical seeded start prices).
const complianceLargeTradeThreshold = 50_000.0

func (e *Engine) complianceCheck(ctx context.Context) (int, error) {
	rows, err := e.Store.DB.QueryContext(ctx, "SELECT trade_id, trade_value FROM trades ORDER BY trade_id DESC LIMIT 50")
	if err != nil {
		return 0, err
	}
	var flagged []int64
	for rows.Next() {
		var id int64
		var val float64
		if rows.Scan(&id, &val) == nil && val > complianceLargeTradeThreshold {
			flagged = append(flagged, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	for _, id := range flagged {
		if _, err := e.Store.DB.ExecContext(ctx,
			"INSERT INTO audit_events (entity_type, entity_id, event_type, event_payload, created_at) VALUES ('trade',?,?,NULL,?)",
			id, "large_trade_flagged", now); err != nil {
			return len(flagged), err
		}
	}
	return len(flagged), nil
}

// ---------------------------------------------------------------- cleanup

// runCleanupAgent expires stale "open" orders — a continuously-running,
// low-rate stand-in for the kind of end-of-day housekeeping job a real
// exchange runs periodically, exercised here as ongoing background load
// instead so a short demo session still sees it happen.
func (e *Engine) runCleanupAgent(ctx context.Context) {
	var events, errs int64
	tickLoop(ctx, 10*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "cleanup", "idle", detailStr(events, errs))
			return
		}
		n := opsThisTick(e.agentRate(sharesFor(e.Mix()).Cleanup), 10*time.Second)
		if n < 1 {
			e.Store.Heartbeat(ctx, "cleanup", "ok", detailStr(events, errs))
			return
		}
		octx, cancel := opCtx(ctx)
		aff, err := e.expireStaleOrders(octx, n*10)
		cancel()
		if err != nil {
			errs++
			e.counters.agentErrors.Add(1)
		} else {
			events += aff
		}
		e.Store.Heartbeat(ctx, "cleanup", "ok", detailStr(events, errs))
	})
}

// staleOrderAge is how long an "open" order sits unmatched before cleanup
// expires it — short enough that a demo session actually sees expirations
// happen rather than needing to run for a simulated day.
const staleOrderAge = 2 * time.Hour

func (e *Engine) expireStaleOrders(ctx context.Context, limit int) (int64, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-staleOrderAge)
	res, err := e.Store.DB.ExecContext(ctx,
		"UPDATE orders SET status='cancelled', cancelled_at=?, updated_at=? WHERE status='open' AND created_at<? LIMIT ?",
		now, now, cutoff, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
