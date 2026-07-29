package sim

import (
	"context"
	"math/rand"
	"time"
)

// ----------------------------------------------------------------- scanner

// runScannerAgent is a market-scanner tool: rotates through "most active"
// (top volume), "top movers" (widest day range), and a price-history range
// read against the small, fixed-size market_quotes table (cheap by
// construction) plus a volume-aggregation read against the much larger
// price_ticks table (the live-full-history-aggregation challenge's target —
// bounded to a recent window by default, unbounded when that challenge is
// active and not yet fixed).
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

// scannerHistoryWindow bounds how far back the price-history scan and the
// (fixed) volume-aggregation scan look — a real "today plus a bit" window,
// not the whole seeded history.
const scannerHistoryWindow = 24 * time.Hour

func (e *Engine) runScan(ctx context.Context, rng *rand.Rand) bool {
	switch rng.Intn(4) {
	case 0:
		return e.scanQuery(ctx, "SELECT security_id, volume FROM market_quotes ORDER BY volume DESC LIMIT 20")
	case 1:
		return e.scanQuery(ctx, "SELECT security_id, (day_high-day_low) AS spread FROM market_quotes ORDER BY spread DESC LIMIT 20")
	case 2:
		return e.priceHistoryScan(ctx, rng)
	default:
		return e.fullHistoryAggScan(ctx)
	}
}

func (e *Engine) scanQuery(ctx context.Context, query string) bool {
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

// priceHistoryScan is idx-price-history's target — a plain equality+range
// lookup on price_ticks that wants (security_id, recorded_at) as a
// composite index.
func (e *Engine) priceHistoryScan(ctx context.Context, rng *rand.Rand) bool {
	secIdx := weightedPick(e.popCum, rng)
	rows, err := e.Store.DB.QueryContext(ctx,
		"SELECT price, recorded_at FROM price_ticks WHERE security_id=? AND recorded_at > ? ORDER BY recorded_at DESC LIMIT 200",
		secIdx+1, time.Now().Add(-scannerHistoryWindow))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var price float64
		var recordedAt time.Time
		rows.Scan(&price, &recordedAt)
	}
	return rows.Err() == nil
}

// fullHistoryAggScan is live-full-history-aggregation's target — bounded to
// scannerHistoryWindow by default; the challenge's bad state drops the
// WHERE clause entirely, aggregating the whole (ever-growing) price_ticks
// table on every single call.
func (e *Engine) fullHistoryAggScan(ctx context.Context) bool {
	query := "SELECT security_id, SUM(volume) AS vol FROM price_ticks WHERE recorded_at > ? GROUP BY security_id ORDER BY vol DESC LIMIT 20"
	args := []any{time.Now().Add(-scannerHistoryWindow)}
	if e.ActiveVariant() == "live-full-history-aggregation" {
		query = "SELECT security_id, SUM(volume) AS vol FROM price_ticks GROUP BY security_id ORDER BY vol DESC LIMIT 20"
		args = nil
	}
	rows, err := e.Store.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var secID int
		var vol int64
		rows.Scan(&secID, &vol)
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

// dashboardSummary is portfolio-n-plus-1's target. Reference: one JOIN,
// counted as 1 query regardless of how many positions the account holds.
// Bad (N+1, active only while that challenge is armed and unfixed): one
// query to list the account's positions, then one MORE query per position
// row to look up its price — portfolioSummaryQueries is what grading
// compares (queries issued per poll), since the N+1 variant's constituent
// queries classify to entirely different leaderboard shapes than the JOIN
// does (there's no shared shape ID to diff a before/after delta against).
func (e *Engine) dashboardSummary(ctx context.Context, rng *rand.Rand) bool {
	accountID := e.randAccountID(rng)
	if accountID == 0 {
		return false
	}
	if e.ActiveVariant() == "portfolio-n-plus-1" {
		return e.dashboardSummaryNPlusOne(ctx, accountID)
	}
	// Full table names, not single-letter aliases (p/q) — found live (stage
	// S4 verification): Percona Server 8.0.46's digest computation never
	// accumulates count_star past 1 for a JOIN using short table aliases,
	// a reproducible quirk confirmed by running the identical query by hand
	// via the mysql client both ways. Spelling out the table names instead
	// is functionally identical SQL and sidesteps it entirely.
	rows, err := e.Store.DB.QueryContext(ctx,
		`SELECT positions.security_id, positions.quantity, market_quotes.last_price FROM positions
		 JOIN market_quotes ON market_quotes.security_id=positions.security_id
		 WHERE positions.account_id=?`, accountID)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var secID, qty int
		var last float64
		rows.Scan(&secID, &qty, &last)
	}
	e.counters.portfolioSummaryQueries.Add(1)
	return rows.Err() == nil
}

func (e *Engine) dashboardSummaryNPlusOne(ctx context.Context, accountID int) bool {
	rows, err := e.Store.DB.QueryContext(ctx,
		"SELECT security_id, quantity FROM positions WHERE account_id=?", accountID)
	if err != nil {
		return false
	}
	var secIDs []int
	for rows.Next() {
		var secID, qty int
		if rows.Scan(&secID, &qty) == nil {
			secIDs = append(secIDs, secID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false
	}
	queries := int64(1)
	for _, secID := range secIDs {
		var last float64
		e.Store.DB.QueryRowContext(ctx, "SELECT last_price FROM market_quotes WHERE security_id=?", secID).Scan(&last)
		queries++
	}
	e.counters.portfolioSummaryQueries.Add(queries)
	return true
}
