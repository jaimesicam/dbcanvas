package sim

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"time"
)

// ------------------------------------------------------------ retail trader

// retailTraderLoop is a concurrency-sensitive worker-pool agent (see
// engine.go's hybrid model / workers.go's workerPool) — each worker places
// one small order at a time in a tight loop, always against the primary
// connection. Retail flow is deliberately simple (a single INSERT, no
// transaction) — placeInstitutionalOrder below is where the interesting
// contention comes from.
func (e *Engine) retailTraderLoop(ctx context.Context, workerIndex int) {
	rng := newAgentRand()
	lastHB := time.Now()
	for ctx.Err() == nil {
		if !e.Running() {
			jitterSleep(ctx, rng, 300*time.Millisecond)
			continue
		}
		octx, cancel := opCtx(ctx)
		ok := e.placeOrder(octx, rng, e.Store.DB)
		cancel()
		if ok {
			e.counters.retailOrders.Add(1)
			e.counters.ordersPlaced.Add(1)
		} else {
			e.counters.agentErrors.Add(1)
		}
		if workerIndex == 0 && time.Since(lastHB) > 2*time.Second {
			e.Store.Heartbeat(ctx, "retail-trader", "ok",
				fmt.Sprintf("orders=%d workers=%d", e.counters.retailOrders.Load(), e.pools["retail"].size()))
			lastHB = time.Now()
		}
		jitterSleep(ctx, rng, 400*time.Millisecond)
	}
}

// placeOrder inserts one new order for a random account into a
// weighted-random security. A single INSERT is already atomic on its
// own — no transaction needed.
func (e *Engine) placeOrder(ctx context.Context, rng *rand.Rand, db *sql.DB) bool {
	accountID := e.randAccountID(rng)
	if accountID == 0 {
		return false
	}
	secIdx := weightedPick(e.popCum, rng)
	sec := e.Securities[secIdx]
	side := "buy"
	if rng.Intn(2) == 0 {
		side = "sell"
	}
	orderType := "market"
	var limitPrice any
	if rng.Intn(2) == 0 {
		orderType = "limit"
		limitPrice = round2(sec.StartPrice * (0.95 + rng.Float64()*0.1))
	}
	qty := 1 + rng.Intn(500)
	now := time.Now().UTC()
	_, err := db.ExecContext(ctx,
		`INSERT INTO orders (account_id, security_id, side, order_type, quantity, remaining_quantity, limit_price, status, priority, created_at, updated_at, cancelled_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL)`,
		accountID, secIdx+1, side, orderType, qty, qty, limitPrice, "open", 0, now, now)
	return err == nil
}

// ------------------------------------------------------ institutional trader

// institutionalTraderLoop places large block orders and, on a direct PXC
// cluster-frame link, pins each worker to one member connection for its
// whole lifetime (round-robin by worker index, not per-operation). Combined
// with placeInstitutionalOrder's UPDATE against the hot market_quotes row
// for a popular symbol, two workers pinned to two different nodes hammering
// the same popular symbol concurrently is what manufactures real cross-node
// Galera certification conflicts — the mechanism the PXC-specific challenge
// pack (stage S4+) needs (see the written plan's §5.3 design note on why
// this replaces "multi-writer through HAProxy", which this repo's
// single-writer HAProxy+PXC config can't represent). On every other target
// shape (Members is empty) this agent behaves like retail trading, just with
// bigger orders and a real transaction.
func (e *Engine) institutionalTraderLoop(ctx context.Context, workerIndex int) {
	rng := newAgentRand()
	lastHB := time.Now()
	db := e.Store.DB
	if e.Members.Len() > 0 {
		db = e.Members.At(workerIndex)
	}
	for ctx.Err() == nil {
		if !e.Running() {
			jitterSleep(ctx, rng, 500*time.Millisecond)
			continue
		}
		octx, cancel := opCtx(ctx)
		ok := e.placeInstitutionalOrder(octx, rng, db)
		cancel()
		if ok {
			e.counters.institutionalOrders.Add(1)
			e.counters.ordersPlaced.Add(1)
		} else {
			e.counters.agentErrors.Add(1)
		}
		if workerIndex == 0 && time.Since(lastHB) > 2*time.Second {
			e.Store.Heartbeat(ctx, "institutional-trader", "ok",
				fmt.Sprintf("orders=%d workers=%d members=%d", e.counters.institutionalOrders.Load(), e.pools["institutional"].size(), e.Members.Len()))
			lastHB = time.Now()
		}
		jitterSleep(ctx, rng, 700*time.Millisecond)
	}
}

func (e *Engine) placeInstitutionalOrder(ctx context.Context, rng *rand.Rand, db *sql.DB) bool {
	accountID := e.randAccountID(rng)
	if accountID == 0 {
		return false
	}
	secIdx := weightedPick(e.popCum, rng)
	secID := secIdx + 1
	side := "buy"
	if rng.Intn(2) == 0 {
		side = "sell"
	}
	qty := 500 + rng.Intn(5000)
	now := time.Now().UTC()

	err := withRetry(ctx, db, &e.counters, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"UPDATE market_quotes SET volume=volume+?, updated_at=? WHERE security_id=?", qty, now, secID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO orders (account_id, security_id, side, order_type, quantity, remaining_quantity, limit_price, status, priority, created_at, updated_at, cancelled_at)
			 VALUES (?,?,?,?,?,?,NULL,?,?,?,?,NULL)`,
			accountID, secID, side, "market", qty, qty, "open", 1, now, now)
		return err
	})
	return err == nil
}
