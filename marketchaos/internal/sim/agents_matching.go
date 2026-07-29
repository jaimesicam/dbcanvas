package sim

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"time"
)

// matchingEngineLoop is a concurrency-sensitive worker-pool agent: each
// worker repeatedly picks a popular security and, if there's a crossable
// buy/sell pair of open orders for it, matches them — inserting a trade,
// filling both orders, updating both accounts' positions, and posting the
// ledger entries, all inside one retried transaction (see txn.go's
// withRetry) so two workers racing the same order never double-fill it (the
// SELECT ... FOR UPDATE below is what actually prevents that; withRetry is
// what makes the resulting deadlocks/certification conflicts transparent
// rather than a failed operation the caller has to handle).
func (e *Engine) matchingEngineLoop(ctx context.Context, workerIndex int) {
	rng := newAgentRand()
	lastHB := time.Now()
	for ctx.Err() == nil {
		if !e.Running() {
			jitterSleep(ctx, rng, 400*time.Millisecond)
			continue
		}
		octx, cancel := opCtx(ctx)
		matched, err := e.matchOneOrder(octx, rng)
		cancel()
		if err != nil {
			e.counters.agentErrors.Add(1)
		} else if matched {
			e.counters.tradesExecuted.Add(1)
		}
		if workerIndex == 0 && time.Since(lastHB) > 2*time.Second {
			e.Store.Heartbeat(ctx, "matching-engine", "ok",
				fmt.Sprintf("trades=%d workers=%d", e.counters.tradesExecuted.Load(), e.pools["matching"].size()))
			lastHB = time.Now()
		}
		if !matched {
			jitterSleep(ctx, rng, 300*time.Millisecond)
		}
	}
}

// matchOneOrder picks one popular security and, if it has both an open buy
// and an open sell order, matches them at the security's current last_price
// for min(their remaining quantities). Not a full price-time-priority book —
// a real matching engine's ORDER BY price/time crossing logic is exactly the
// kind of thing a future challenge could deliberately regress (a missing
// composite index degrading it), so keeping this simple now leaves room for
// that later without this stage needing to anticipate it.
func (e *Engine) matchOneOrder(ctx context.Context, rng *rand.Rand) (bool, error) {
	secIdx := weightedPick(e.popCum, rng)
	secID := secIdx + 1
	matched := false

	// broad-select-for-update's bad state: drop the LIMIT 1, locking every
	// open/partial order for this security+side instead of just the one
	// about to be used.
	limitClause := " LIMIT 1"
	if e.ActiveVariant() == "broad-select-for-update" {
		limitClause = ""
	}
	buyQuery := "SELECT order_id, account_id, remaining_quantity FROM orders WHERE security_id=? AND side='buy' AND status IN ('open','partial') ORDER BY order_id" + limitClause + " FOR UPDATE"
	sellQuery := "SELECT order_id, account_id, remaining_quantity FROM orders WHERE security_id=? AND side='sell' AND status IN ('open','partial') ORDER BY order_id" + limitClause + " FOR UPDATE"

	// inconsistent-lock-ordering's bad state: half the time, lock sell
	// before buy instead of always buy-before-sell — two workers racing the
	// same symbol with opposite lock orders is a textbook deadlock.
	sellFirst := e.ActiveVariant() == "inconsistent-lock-ordering" && rng.Intn(2) == 0

	err := withRetry(ctx, e.Store.DB, &e.counters, func(tx *sql.Tx) error {
		matched = false
		var buyID, sellID int64
		var buyAcct, sellAcct, buyRemain, sellRemain int

		fetchBuy := func() error {
			return tx.QueryRowContext(ctx, buyQuery, secID).Scan(&buyID, &buyAcct, &buyRemain)
		}
		fetchSell := func() error {
			return tx.QueryRowContext(ctx, sellQuery, secID).Scan(&sellID, &sellAcct, &sellRemain)
		}
		var err error
		if sellFirst {
			err = fetchSell()
		} else {
			err = fetchBuy()
		}
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if sellFirst {
			err = fetchBuy()
		} else {
			err = fetchSell()
		}
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if buyAcct == sellAcct {
			return nil // never let an account trade with itself
		}

		qty := buyRemain
		if sellRemain < qty {
			qty = sellRemain
		}
		if qty <= 0 {
			return nil
		}

		var price float64
		if err := tx.QueryRowContext(ctx, "SELECT last_price FROM market_quotes WHERE security_id=?", secID).Scan(&price); err != nil {
			return err
		}
		now := time.Now().UTC()
		tradeValue := round2(price * float64(qty))
		res, err := tx.ExecContext(ctx,
			"INSERT INTO trades (buy_order_id, sell_order_id, security_id, quantity, price, trade_value, executed_at) VALUES (?,?,?,?,?,?,?)",
			buyID, sellID, secID, qty, price, tradeValue, now)
		if err != nil {
			return err
		}
		tradeID, _ := res.LastInsertId()

		if err := fillOrder(ctx, tx, buyID, buyRemain, qty, now); err != nil {
			return err
		}
		if err := fillOrder(ctx, tx, sellID, sellRemain, qty, now); err != nil {
			return err
		}
		if err := applyPosition(ctx, tx, buyAcct, secID, qty, price, now); err != nil {
			return err
		}
		if err := applyPosition(ctx, tx, sellAcct, secID, -qty, price, now); err != nil {
			return err
		}

		// balance_after is left at 0 here — computing the real running cash
		// balance would need reading+locking the accounts row too, on every
		// single trade. Deliberately deferred: nothing reads/validates ledger
		// balances until stage S5's grading engine, which is what actually
		// needs it to be correct.
		legs := []struct {
			acct   int
			amount float64
		}{{buyAcct, -tradeValue}, {sellAcct, tradeValue}}
		for _, leg := range legs {
			entryType := "credit"
			if leg.amount < 0 {
				entryType = "debit"
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO account_ledger (account_id, trade_id, entry_type, amount, balance_after, created_at) VALUES (?,?,?,?,0,?)",
				leg.acct, tradeID, entryType, leg.amount, now); err != nil {
				return err
			}
		}
		matched = true
		return nil
	})
	return matched, err
}

func fillOrder(ctx context.Context, tx *sql.Tx, orderID int64, remainBefore, qty int, now time.Time) error {
	remainAfter := remainBefore - qty
	status := "partial"
	if remainAfter <= 0 {
		status = "filled"
	}
	_, err := tx.ExecContext(ctx,
		"UPDATE orders SET remaining_quantity=?, status=?, updated_at=? WHERE order_id=?",
		remainAfter, status, now, orderID)
	return err
}

func applyPosition(ctx context.Context, tx *sql.Tx, accountID, secID, deltaQty int, price float64, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO positions (account_id, security_id, quantity, average_cost, realized_profit, updated_at)
		 VALUES (?,?,?,?,0,?)
		 ON DUPLICATE KEY UPDATE quantity=quantity+VALUES(quantity), updated_at=VALUES(updated_at)`,
		accountID, secID, deltaQty, price, now)
	return err
}
