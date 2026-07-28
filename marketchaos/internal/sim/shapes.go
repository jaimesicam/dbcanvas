package sim

import "strings"

// shapePattern classifies one performance_schema digest into a named,
// human-labeled query shape by matching stable substrings of its
// (MySQL-normalized) DIGEST_TEXT — table names, column lists, and keywords
// survive normalization; only value literals (including bound parameters,
// AND literal NULLs/numbers written directly in the SQL) get folded to `?`.
// That folding is also why a few of these deliberately collapse two agents'
// otherwise-distinct statements into one shape: e.g. matching-engine's two
// SELECT...FOR UPDATE queries (WHERE side='buy' vs side='sell') normalize to
// the exact same digest text once the string literal becomes `?`, so they're
// one shape here, not two — the leaderboard can't see a distinction that
// doesn't survive normalization, and pretending otherwise would be lying
// about what's actually attributable from Performance Schema alone. Where
// that matters (retail vs institutional order volume), Engine's own
// per-agent counters (see engine.go's counters struct) are the source of
// truth instead — this registry is deliberately not the only place agent
// attribution exists.
type shapePattern struct {
	ID       string
	Label    string
	Agent    string // primary agent this shape belongs to, for the leaderboard's "attributes shapes to agents" requirement
	Contains []string
}

var shapeRegistry = []shapePattern{
	{ID: "quotes.tick_update", Label: "Update quote (price/volume/high/low)", Agent: "market-data",
		Contains: []string{"UPDATE", "market_quotes", "day_high", "day_low"}},
	{ID: "quotes.hot_volume_bump", Label: "Bump quote volume (institutional block order)", Agent: "institutional-trader",
		Contains: []string{"UPDATE", "market_quotes", "volume", "updated_at"}},
	{ID: "quotes.price_lookup", Label: "Look up current price", Agent: "market-data / matching-engine",
		Contains: []string{"SELECT", "last_price", "market_quotes", "security_id"}},
	{ID: "ticks.insert", Label: "Record a price tick", Agent: "market-data",
		Contains: []string{"INSERT", "price_ticks"}},
	{ID: "news.insert", Label: "Publish market news", Agent: "news",
		Contains: []string{"INSERT", "market_news"}},
	{ID: "scanner.top_volume", Label: "Scan: most active by volume", Agent: "scanner",
		Contains: []string{"SELECT", "volume", "market_quotes", "ORDER BY", "volume"}},
	{ID: "scanner.top_movers", Label: "Scan: top movers by day range", Agent: "scanner",
		Contains: []string{"SELECT", "day_high", "day_low", "market_quotes"}},
	{ID: "dashboard.portfolio_summary", Label: "Dashboard portfolio summary", Agent: "dashboard-poll",
		Contains: []string{"SELECT", "positions", "market_quotes", "JOIN"}},
	{ID: "portfolio.revalue", Label: "Portfolio revaluation read", Agent: "portfolio",
		Contains: []string{"SELECT", "average_cost", "positions", "market_quotes"}},
	{ID: "compliance.recent_trades", Label: "Compliance: scan recent trades", Agent: "compliance",
		Contains: []string{"SELECT", "trade_value", "trades", "ORDER BY"}},
	{ID: "compliance.flag_insert", Label: "Compliance: flag large trade", Agent: "compliance",
		Contains: []string{"INSERT", "audit_events"}},
	{ID: "cleanup.expire_orders", Label: "Expire stale open orders", Agent: "cleanup",
		Contains: []string{"UPDATE", "orders", "cancelled_at", "created_at"}},
	{ID: "orders.insert", Label: "Place order", Agent: "retail-trader / institutional-trader",
		Contains: []string{"INSERT", "INTO", "orders", "remaining_quantity"}},
	{ID: "orders.match_candidate", Label: "Find a matchable order (FOR UPDATE)", Agent: "matching-engine",
		Contains: []string{"SELECT", "orders", "remaining_quantity", "FOR UPDATE"}},
	{ID: "orders.fill", Label: "Fill an order after a match", Agent: "matching-engine",
		Contains: []string{"UPDATE", "orders", "remaining_quantity", "status"}},
	{ID: "trades.insert", Label: "Record an executed trade", Agent: "matching-engine",
		Contains: []string{"INSERT", "trades", "trade_value"}},
	{ID: "positions.upsert", Label: "Update a position after a fill", Agent: "matching-engine",
		Contains: []string{"INSERT", "positions", "ON DUPLICATE KEY UPDATE"}},
	{ID: "ledger.insert", Label: "Post a ledger entry", Agent: "matching-engine",
		Contains: []string{"INSERT", "account_ledger"}},
}

// ClassifyDigest matches digestText against the registry in order — order
// matters where patterns could both match (e.g. both "quotes.tick_update"
// and "quotes.hot_volume_bump" mention market_quotes+UPDATE; the more
// specific day_high/day_low pattern is listed first so it wins).
func ClassifyDigest(digestText string) (shapePattern, bool) {
	for _, p := range shapeRegistry {
		if matchesAll(digestText, p.Contains) {
			return p, true
		}
	}
	return shapePattern{}, false
}

func matchesAll(text string, needles []string) bool {
	for _, n := range needles {
		if !strings.Contains(text, n) {
			return false
		}
	}
	return true
}
