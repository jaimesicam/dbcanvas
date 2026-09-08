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

// institutionalSymbolPick is the normal weighted-random pick, except for the
// 2 PXC challenges that deliberately concentrate writes beyond it: hot-
// parent-row always targets the single most popular symbol, and
// hot-symbol-conflict leans toward it most (not all) of the time — a
// graduated distinction between "worse than baseline" and "worst case."
func (e *Engine) institutionalSymbolPick(rng *rand.Rand) int {
	switch e.ActiveVariant() {
	case "pxc-hot-parent-row":
		return e.topSymbolIdx
	case "pxc-hot-symbol-conflict":
		if rng.Intn(10) < 7 {
			return e.topSymbolIdx
		}
		return weightedPick(e.popCum, rng)
	default:
		return weightedPick(e.popCum, rng)
	}
}

// placeInstitutionalOrder covers 6 of the PXC-specific challenge pack's
// bad states, each an app-only behavior no learner SQL could reach (see
// internal/challenge's package doc comment) — every branch here reverts to
// the plain baseline behavior the instant ActiveVariant() stops returning
// that challenge's ID (a challenge is reset, or its fix is applied).
func (e *Engine) placeInstitutionalOrder(ctx context.Context, rng *rand.Rand, db *sql.DB) bool {
	accountID := e.randAccountID(rng)
	if accountID == 0 {
		return false
	}
	variant := e.ActiveVariant()
	now := time.Now().UTC()

	maxAttempts := maxTxnRetries
	if variant == "pxc-no-retry-classification" {
		maxAttempts = 1
	}
	batch := 1
	if variant == "pxc-flow-control-pressure" {
		batch = 50 // one oversized writeset instead of 50 small ones
	}

	err := withRetryN(ctx, db, &e.counters, maxAttempts, func(tx *sql.Tx) error {
		if variant == "pxc-oversized-transaction" {
			// touches every security's quote row, not just the one this
			// order is actually about — a much larger writeset than the job
			// needs, certified as a single unit.
			if _, err := tx.ExecContext(ctx, "UPDATE market_quotes SET updated_at=?", now); err != nil {
				return err
			}
		}
		for i := 0; i < batch; i++ {
			secID := e.institutionalSymbolPick(rng) + 1
			side := "buy"
			if rng.Intn(2) == 0 {
				side = "sell"
			}
			qty := 500 + rng.Intn(5000)
			if _, err := tx.ExecContext(ctx,
				"UPDATE market_quotes SET volume=volume+?, updated_at=? WHERE security_id=?", qty, now, secID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO orders (account_id, security_id, side, order_type, quantity, remaining_quantity, limit_price, status, priority, created_at, updated_at, cancelled_at)
				 VALUES (?,?,?,?,?,?,NULL,?,?,?,?,NULL)`,
				accountID, secID, side, "market", qty, qty, "open", 1, now, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false
	}

	// pxc-read-after-write's bad state: read back from a different,
	// round-robined member instead of the one that just wrote — the read's
	// result is discarded (this is a demonstration of possible staleness,
	// not something anything downstream depends on).
	if variant == "pxc-read-after-write" && e.Members.Len() > 1 {
		octx, cancel := opCtx(ctx)
		var status sql.NullString
		e.Members.Next().QueryRowContext(octx, "SELECT status FROM orders WHERE account_id=? ORDER BY order_id DESC LIMIT 1", accountID).Scan(&status)
		cancel()
	}
	return true
}
