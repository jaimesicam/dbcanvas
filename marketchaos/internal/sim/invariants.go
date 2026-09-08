package sim

import "context"

// invariantCheck is one always-on correctness check, run for every graded
// challenge regardless of which one is active (see challenge.Challenge's
// own FunctionalCheck for challenge-specific checks on top of these). Any
// failure here zeroes the whole score, per the confirmed hard-gate design
// decision — these are cheap enough (bounded LIMIT, indexed lookups) to run
// inside a measurement window without materially disturbing it.
//
// Deliberately NOT included: "no negative position quantity." This sim's
// matching engine doesn't enforce a holding requirement before allowing a
// sell (see agents_matching.go's applyPosition) — naked shorting is an
// accepted simplification of this fictional exchange, not a bug any
// challenge's fix could address, so treating it as an invariant would fire
// on every single grading run regardless of what the learner did.
type invariantCheck struct {
	Name  string
	Query string // must return exactly one row, one column: a COUNT(*) of violations
}

var invariants = []invariantCheck{
	{"order overfilled (remaining_quantity > quantity)", "SELECT COUNT(*) FROM orders WHERE remaining_quantity > quantity"},
	{"order underfilled below zero (remaining_quantity < 0)", "SELECT COUNT(*) FROM orders WHERE remaining_quantity < 0"},
	{"trade with non-positive quantity or price", "SELECT COUNT(*) FROM trades WHERE quantity <= 0 OR price <= 0"},
	{"trade references a nonexistent buy order", "SELECT COUNT(*) FROM trades t LEFT JOIN orders o ON o.order_id=t.buy_order_id WHERE o.order_id IS NULL"},
	{"trade references a nonexistent sell order", "SELECT COUNT(*) FROM trades t LEFT JOIN orders o ON o.order_id=t.sell_order_id WHERE o.order_id IS NULL"},
	{"ledger entry references a nonexistent trade", "SELECT COUNT(*) FROM account_ledger l LEFT JOIN trades t ON t.trade_id=l.trade_id WHERE l.trade_id IS NOT NULL AND t.trade_id IS NULL"},
}

// CheckInvariants runs every always-on invariant and returns the first
// violation's description, or "" if all pass.
func (e *Engine) CheckInvariants(ctx context.Context) string {
	for _, inv := range invariants {
		var n int64
		if err := e.Store.DB.QueryRowContext(ctx, inv.Query).Scan(&n); err != nil {
			return "invariant check failed to run (" + inv.Name + "): " + err.Error()
		}
		if n > 0 {
			return inv.Name
		}
	}
	return ""
}
