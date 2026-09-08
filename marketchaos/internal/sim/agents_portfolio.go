package sim

import (
	"context"
	"fmt"
	"math/rand"
	"time"
)

// portfolioLoop is a concurrency-sensitive worker-pool agent standing in for
// a portfolio-valuation service: each worker repeatedly re-reads one random
// account's full position list joined against current quotes — read-mostly,
// deliberately the "quiet" one of the four pool agents so a read-heavy mix
// has something to lean on beyond the scanner.
func (e *Engine) portfolioLoop(ctx context.Context, workerIndex int) {
	rng := newAgentRand()
	lastHB := time.Now()
	for ctx.Err() == nil {
		if !e.Running() {
			jitterSleep(ctx, rng, 500*time.Millisecond)
			continue
		}
		octx, cancel := opCtx(ctx)
		ok := e.revaluePortfolio(octx, rng)
		cancel()
		if ok {
			e.counters.portfolioReads.Add(1)
		} else {
			e.counters.agentErrors.Add(1)
		}
		if workerIndex == 0 && time.Since(lastHB) > 2*time.Second {
			e.Store.Heartbeat(ctx, "portfolio", "ok",
				fmt.Sprintf("reads=%d workers=%d", e.counters.portfolioReads.Load(), e.pools["portfolio"].size()))
			lastHB = time.Now()
		}
		jitterSleep(ctx, rng, 600*time.Millisecond)
	}
}

func (e *Engine) revaluePortfolio(ctx context.Context, rng *rand.Rand) bool {
	accountID := e.randAccountID(rng)
	if accountID == 0 {
		return false
	}
	// Full table names, not aliases — see agents_analytics.go's
	// dashboardSummary for why (a reproducible Percona Server 8.0.46 digest
	// quirk with short JOIN aliases, found live).
	rows, err := e.Store.DB.QueryContext(ctx,
		`SELECT positions.security_id, positions.quantity, positions.average_cost, market_quotes.last_price
		 FROM positions JOIN market_quotes ON market_quotes.security_id=positions.security_id
		 WHERE positions.account_id=?`, accountID)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var secID, qty int
		var avgCost, last float64
		rows.Scan(&secID, &qty, &avgCost, &last)
	}
	return rows.Err() == nil
}
