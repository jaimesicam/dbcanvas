package challenge

import (
	"context"
	"database/sql"
)

// indexExists is the shared FunctionalCheck helper every DB-fixable
// indexing challenge uses: did an index matching this name get (re)created
// on this table? Doesn't care whether it's exactly the original index
// definition — a learner who recreates it with different column order or
// an extra covering column still legitimately fixed the underlying
// problem, and re-checking the exact original DDL would be checking their
// answer instead of the outcome (the whole point of this app, per the
// product spec).
func indexExists(table, indexName string) func(context.Context, *sql.DB) string {
	return func(ctx context.Context, db *sql.DB) string {
		var n int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND INDEX_NAME=?",
			table, indexName).Scan(&n)
		if err != nil || n == 0 {
			return "no index named " + indexName + " on " + table + " (recreate it, or any index that lets the same lookup avoid a full scan)"
		}
		return ""
	}
}

// anyUsableIndex is more lenient than indexExists — used where the exact
// original index name doesn't matter, only that SOME index now covers the
// leading column(s) a query needs (a learner might reasonably choose a
// different name or column order that still fixes the query).
func anyIndexOnColumn(table, column string) func(context.Context, *sql.DB) string {
	return func(ctx context.Context, db *sql.DB) string {
		var n int
		err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.STATISTICS
			WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND SEQ_IN_INDEX=1 AND COLUMN_NAME=?`,
			table, column).Scan(&n)
		if err != nil || n == 0 {
			return "no index has " + column + " as its leading column on " + table + " yet"
		}
		return ""
	}
}

// Catalog is every challenge MarketChaos ships — the spec's 10 generic
// challenges (5 beginner + 5 intermediate) plus the full PXC-specific pack
// (8), gated to appear only when the linked target is PXC-family. See the
// package doc comment for the DB-vs-app-variant split and IMPLEMENTATION.md
// session 180 for the live-verified symptom each one actually reproduces.
var Catalog = []Challenge{
	// ---------------------------------------------------------- beginner
	{
		ID: "idx-price-history", Title: "Missing price-history index",
		Category: CategoryIndexing, Difficulty: Beginner, Mechanism: MechanismDB,
		Scenario: "A junior DBA dropped an index during a \"cleanup\" pass last week, certain it was unused.",
		Symptom:  "Price-history lookups (the scanner's history reads) get slower and their leaderboard row shows rising rowsExamined with every tick added to price_ticks.",
		Setup:    []string{"DROP INDEX idx_ticks_security_time ON price_ticks"},
		Teardown: []string{"CREATE INDEX idx_ticks_security_time ON price_ticks (security_id, recorded_at)"},
		ShapeIDs: []string{"history.price_range"},
		Hints: []Hint{
			{1, "Which table is price-history read from, and what does its WHERE clause filter on?"},
			{2, "Check information_schema.STATISTICS for price_ticks — what indexes exist right now?"},
			{3, "CREATE INDEX ... ON price_ticks (security_id, recorded_at) — column order matters: equality column first, range column second."},
		},
		FunctionalCheck: indexExists("price_ticks", "idx_ticks_security_time"),
	},
	{
		ID: "function-wrapped-timestamp", Title: "Function-wrapped timestamp column",
		Category: CategoryQueryRewrite, Difficulty: Beginner, Mechanism: MechanismApp,
		Scenario: "The compliance team's \"today's orders\" report was rewritten to use DATE(created_at) = ? for readability.",
		Symptom:  "Wrapping created_at in DATE(...) defeats any index on that column outright — MySQL can't range-scan a computed expression, so this becomes a full scan of orders no matter what indexes exist.",
		ShapeIDs: []string{"orders.daily_lookup"},
		Hints: []Hint{
			{1, "EXPLAIN the daily-lookup query the compliance agent issues — what's \"type\" and \"key\"?"},
			{2, "A function wrapped around an indexed column (DATE(created_at)) can't use a range scan on that column, even if the index exists."},
			{3, "The equivalent range form is created_at >= CURDATE() AND created_at < CURDATE() + INTERVAL 1 DAY — same result, sargable."},
		},
	},
	{
		ID: "deep-offset-pagination", Title: "Deep OFFSET pagination",
		Category: CategoryQueryRewrite, Difficulty: Beginner, Mechanism: MechanismApp,
		Scenario: "The trade-history viewer added \"jump to page 250\" — implemented as a bigger OFFSET.",
		Symptom:  "MySQL still has to scan and discard every one of the first 5,000 rows before returning page 251 — rowsExamined on this shape is far larger than rowsSent, and it gets worse the deeper a learner \"pages.\"",
		ShapeIDs: []string{"trades.recent"},
		Hints: []Hint{
			{1, "Compare rowsExamined to rowsSent for this shape on the leaderboard — what does the gap tell you?"},
			{2, "OFFSET doesn't skip work — MySQL still reads and discards every skipped row."},
			{3, "Keyset pagination (WHERE trade_id < :last_seen_id ORDER BY trade_id DESC LIMIT n) reads exactly n rows regardless of how deep you are."},
		},
	},
	{
		ID: "portfolio-n-plus-1", Title: "Portfolio N+1",
		Category: CategoryNPlusOne, Difficulty: Beginner, Mechanism: MechanismApp,
		Scenario: "A recent \"simplify the portfolio widget\" refactor split one query into a per-position loop.",
		Symptom:  "One dashboard load for an account with 4 positions now issues 5 queries instead of 1 — call counts on the dashboard-summary shape jump far faster than the number of learners actually looking at their portfolio.",
		ShapeIDs: []string{"dashboard.portfolio_summary"},
		Hints: []Hint{
			{1, "How many queries does one dashboard summary load actually issue? Compare calls on this shape to how often the agent ticks."},
			{2, "A query issued once per row in a result set, inside a loop over another query's results, is the N+1 pattern."},
			{3, "A single JOIN across positions and market_quotes returns exactly the same data in one round trip."},
		},
	},
	{
		ID: "unbounded-trade-history", Title: "Unbounded trade-history query",
		Category: CategoryQueryRewrite, Difficulty: Beginner, Mechanism: MechanismApp,
		Scenario: "The \"show recent trades\" query lost its LIMIT clause in a merge.",
		Symptom:  "Every call now returns (and the server has to materialize) the ENTIRE trades table — rowsSent balloons, and it gets strictly worse as more trades accumulate.",
		ShapeIDs: []string{"trades.recent"},
		Hints: []Hint{
			{1, "How many rows does this shape actually send per call, compared to what a \"recent trades\" list needs to show?"},
			{2, "Missing LIMIT clauses don't error — they just quietly return everything."},
			{3, "Add ORDER BY trade_id DESC LIMIT n back — bound the result set to what's actually displayed."},
		},
	},

	// ------------------------------------------------------- intermediate
	{
		ID: "idx-order-book-composite", Title: "Wrong order-book composite index",
		Category: CategoryIndexing, Difficulty: Intermediate, Mechanism: MechanismDB,
		Scenario: "An index migration replaced the order-book lookup index with a single-column version, assuming it was equivalent.",
		Symptom:  "The matching engine's order lookup (security_id + status) now only benefits from a single-column index on security_id, examining every order for a symbol regardless of status instead of jumping straight to open ones.",
		Setup: []string{
			"DROP INDEX idx_orders_security_status ON orders",
			"CREATE INDEX idx_orders_security_only ON orders (security_id)",
		},
		Teardown: []string{
			"DROP INDEX idx_orders_security_only ON orders",
			"CREATE INDEX idx_orders_security_status ON orders (security_id, status)",
		},
		ShapeIDs: []string{"orders.match_candidate"},
		Hints: []Hint{
			{1, "EXPLAIN the matching engine's order-lookup query — which index does it choose, and how many rows does it estimate?"},
			{2, "A composite index on (security_id, status) lets MySQL seek directly to open orders for a symbol; an index on security_id alone still has to filter every status value for that symbol by hand."},
			{3, "CREATE INDEX ... ON orders (security_id, status) — column order matches the WHERE clause's equality predicates."},
		},
		FunctionalCheck: indexExists("orders", "idx_orders_security_status"),
	},
	{
		ID: "live-full-history-aggregation", Title: "Live full-history dashboard aggregation",
		Category: CategorySorting, Difficulty: Intermediate, Mechanism: MechanismApp,
		Scenario: "The scanner's \"most active today\" panel was implemented by aggregating the entire price_ticks table on every call, \"to be safe.\"",
		Symptom:  "Every scan now does SUM(volume) GROUP BY security_id over the FULL price-tick history instead of a bounded recent window — rowsExamined on this shape grows without bound as the market runs.",
		ShapeIDs: []string{"scanner.full_history_agg"},
		Hints: []Hint{
			{1, "Does \"most active today\" need every tick ever recorded, or just a recent window?"},
			{2, "An unbounded GROUP BY over a growing table gets slower every single day it runs, even if nothing else changes."},
			{3, "Bound the aggregation with a WHERE recorded_at > (a fixed recent window) before grouping."},
		},
	},
	{
		ID: "broad-select-for-update", Title: "Broad SELECT ... FOR UPDATE",
		Category: CategoryLocking, Difficulty: Intermediate, Mechanism: MechanismApp,
		Scenario: "The matching engine's order-locking query lost its LIMIT during a refactor meant to \"batch-match faster.\"",
		Symptom:  "Every match attempt now locks every open order for a symbol instead of just the one it's about to act on — lock waits and deadlock/certification retries climb sharply under concurrent matching.",
		ShapeIDs: []string{"orders.match_candidate"},
		Hints: []Hint{
			{1, "How many rows does the matching engine's FOR UPDATE query actually need to lock to match one order?"},
			{2, "A FOR UPDATE without a LIMIT locks every row it examines, not just the one that ends up used — that's real lock contention held for the whole transaction."},
			{3, "LIMIT 1 on the lock-acquiring SELECT bounds the lock footprint to exactly the row being matched."},
		},
	},
	{
		ID: "inconsistent-lock-ordering", Title: "Inconsistent lock ordering",
		Category: CategoryLocking, Difficulty: Intermediate, Mechanism: MechanismApp,
		Scenario: "A \"fairness\" tweak made the matching engine sometimes lock the sell side before the buy side, to avoid always favoring buyers.",
		Symptom:  "Two matching-engine workers racing the same symbol now sometimes lock in opposite orders — real InnoDB deadlocks (or Galera certification conflicts on a PXC target) climb, distinct from the ordinary contention of matching itself.",
		ShapeIDs: []string{"orders.match_candidate"},
		Hints: []Hint{
			{1, "Two transactions that lock the same two rows in opposite orders can deadlock even when neither is individually doing anything wrong."},
			{2, "Check txnRetries — is it climbing faster than the traffic level alone would explain?"},
			{3, "Always lock in the same fixed order (e.g. always buy row before sell row) across every worker, every time."},
		},
	},
	{
		ID: "redundant-indexes-tick-inserts", Title: "Redundant indexes degrading tick inserts",
		Category: CategorySchema, Difficulty: Intermediate, Mechanism: MechanismDB,
		Scenario: "Three single-column indexes were added to price_ticks over time, each meant to speed up a different one-off report, and never cleaned up.",
		Symptom:  "Every price tick now has to update 4 indexes instead of 1 on insert — the market-data agent's tick-insert shape shows rising avg latency even though nothing else about the workload changed.",
		Setup: []string{
			"CREATE INDEX idx_ticks_sec_only ON price_ticks (security_id)",
			"CREATE INDEX idx_ticks_time_only ON price_ticks (recorded_at)",
			"CREATE INDEX idx_ticks_price_only ON price_ticks (price)",
		},
		Teardown: []string{
			"DROP INDEX idx_ticks_sec_only ON price_ticks",
			"DROP INDEX idx_ticks_time_only ON price_ticks",
			"DROP INDEX idx_ticks_price_only ON price_ticks",
		},
		ShapeIDs: []string{"ticks.insert"},
		Hints: []Hint{
			{1, "How many indexes does price_ticks have right now, and does anything actually query by price alone or recorded_at alone?"},
			{2, "Every index an INSERT touches adds real write cost — indexes aren't free, and unused ones are pure overhead."},
			{3, "DROP the indexes that don't serve idx_ticks_security_time's job and aren't used by any query shape on the leaderboard."},
		},
		MaxNewIndexes: 0,
	},

	// -------------------------------------------------------------- PXC
	{
		ID: "pxc-hot-symbol-conflict", Title: "Multi-writer hot-symbol conflict",
		Category: CategoryPXC, Difficulty: Intermediate, Mechanism: MechanismApp, RequiresFamily: "pxc",
		Scenario: "Institutional trading intentionally spreads writes across every PXC member rather than routing through one node.",
		Symptom:  "Two institutional-trader workers pinned to different members hammering the same popular symbol's market_quotes row produce real cross-node Galera certification conflicts — wsrep_local_cert_failures and wsrep_local_bf_aborts climb on every member.",
		ShapeIDs: []string{"quotes.hot_volume_bump"},
		Hints: []Hint{
			{1, "Check the PXC panel — is wsrep_local_cert_failures climbing on more than one node at once?"},
			{2, "Two nodes both trying to certify a write to the SAME row at the same time is exactly what Galera certification conflicts are — the busier a single popular symbol gets, the worse it is."},
			{3, "Spreading institutional writes across the full 200-security universe (round-robin) instead of concentrating on the handful of popular symbols removes the same-row race."},
		},
	},
	{
		ID: "pxc-no-retry-classification", Title: "No certification-conflict retry",
		Category: CategoryPXC, Difficulty: Intermediate, Mechanism: MechanismApp, RequiresFamily: "pxc",
		Scenario: "A recent change to the retry loop capped it at 1 attempt, assuming certification conflicts were rare enough not to matter.",
		Symptom:  "Every certification conflict that would normally retry transparently now surfaces as a hard error instead — agentErrors climbs in lockstep with wsrep_local_cert_failures instead of txnRetries absorbing it.",
		ShapeIDs: []string{"quotes.hot_volume_bump"},
		Hints: []Hint{
			{1, "Compare txnRetries to agentErrors while cert failures are climbing — which one is actually absorbing the conflicts?"},
			{2, "A MySQL 1213 (certification conflict) is specifically the error class that's supposed to be retried, not surfaced — Galera doesn't retry a client's transaction on its own."},
			{3, "Restore the retry loop's normal attempt budget instead of capping it at 1."},
		},
	},
	{
		ID: "pxc-oversized-transaction", Title: "Oversized transaction",
		Category: CategoryPXC, Difficulty: Intermediate, Mechanism: MechanismApp, RequiresFamily: "pxc",
		Scenario: "An institutional order's transaction was widened to also \"refresh\" every security's quote timestamp, to make downstream reads look fresher.",
		Symptom:  "Every institutional order now writesets a bulk touch of all 200 market_quotes rows alongside the actual order — a much bigger Galera writeset, replicated and certified as one unit, holding certification-relevant locks far longer than the order itself needs.",
		ShapeIDs: []string{"quotes.hot_volume_bump"},
		Hints: []Hint{
			{1, "How many rows does one institutional order transaction actually need to touch to do its job?"},
			{2, "A larger writeset means more certification surface — every row it touches is a row that can conflict with a concurrent writer on another node."},
			{3, "Scope the transaction back down to exactly the one order and the one hot quote row it's placing."},
		},
	},
	{
		ID: "pxc-hot-parent-row", Title: "Hot parent-row contention",
		Category: CategoryPXC, Difficulty: Intermediate, Mechanism: MechanismApp, RequiresFamily: "pxc",
		Scenario: "A load-test script pinned every institutional order to the exchange's single most-traded symbol, to \"stress the hot path.\"",
		Symptom:  "Every institutional worker, on every member, now updates the exact same market_quotes row — the worst-case single-row multi-writer conflict scenario, far beyond what normal Zipf-weighted popularity alone produces.",
		ShapeIDs: []string{"quotes.hot_volume_bump"},
		Hints: []Hint{
			{1, "Is institutional trading actually spread across multiple popular symbols, or all landing on one?"},
			{2, "Concentrating every writer on one single row is the worst case for row-level (and Galera certification) contention — worse than even a naturally popular symbol."},
			{3, "Restore weighted-random symbol selection instead of a single fixed security."},
		},
	},
	{
		ID: "pxc-flow-control-pressure", Title: "Flow-control pressure",
		Category: CategoryPXC, Difficulty: Intermediate, Mechanism: MechanismApp, RequiresFamily: "pxc",
		Scenario: "Institutional orders were batched, 50 at a time, into one transaction \"to reduce round trips.\"",
		Symptom:  "Each institutional commit is now a much larger Galera writeset that slower members take longer to apply — wsrep_flow_control_paused climbs as faster nodes have to pause and wait for slower ones to catch up.",
		ShapeIDs: []string{"quotes.hot_volume_bump"},
		Hints: []Hint{
			{1, "Check the PXC panel's flow-control-paused fraction — is it meaningfully above 0?"},
			{2, "A bigger writeset takes every member longer to apply — Galera pauses faster nodes (flow control) rather than let them race ahead of a slower one."},
			{3, "Commit each institutional order as its own transaction instead of batching many into one."},
		},
	},
	{
		ID: "pxc-read-after-write", Title: "Read-after-write consistency",
		Category: CategoryPXC, Difficulty: Intermediate, Mechanism: MechanismApp, RequiresFamily: "pxc",
		Scenario: "A \"load balance everything\" pass made institutional trading read back its own just-placed order from a round-robin member instead of the one it just wrote to.",
		Symptom:  "A read immediately following a write can land on a member that hasn't applied that writeset yet — the read-back can appear stale (missing the order that was just placed), even though the write itself succeeded.",
		ShapeIDs: []string{"quotes.hot_volume_bump"},
		Hints: []Hint{
			{1, "Does institutional trading read from the same member it just wrote to, or a different one?"},
			{2, "Every PXC member applies a writeset asynchronously (relative to the writer's own commit) unless the session explicitly waits for it — a different node can be momentarily behind."},
			{3, "Read back from the same connection/member that performed the write, not a round-robined one."},
		},
	},
	{
		ID: "pxc-ddl-during-load", Title: "DDL during active load",
		Category: CategoryPXC, Difficulty: Intermediate, Mechanism: MechanismDB, RequiresFamily: "pxc",
		Scenario: "An index was dropped live, mid-trading-day, on the assumption that a quick DDL wouldn't be noticed under normal traffic.",
		Symptom:  "Order lookups by account (the compliance-style account audit query) degrade to scanning every order for that account instead of seeking directly — exactly the kind of thing that's easy to miss until it's already live on a cluster.",
		Setup:    []string{"DROP INDEX idx_orders_account ON orders"},
		Teardown: []string{"CREATE INDEX idx_orders_account ON orders (account_id)"},
		ShapeIDs: []string{"orders.by_account"},
		Hints: []Hint{
			{1, "EXPLAIN the account-scoped order lookup — what does it use as its key right now?"},
			{2, "A DDL applied without checking what still depends on an index can regress a query the moment it lands, cluster-wide, immediately."},
			{3, "CREATE INDEX ... ON orders (account_id) restores the seek path."},
		},
		FunctionalCheck: anyIndexOnColumn("orders", "account_id"),
	},
	{
		ID: "pxc-table-without-pk", Title: "Table without a primary key",
		Category: CategoryPXC, Difficulty: Intermediate, Mechanism: MechanismDB, RequiresFamily: "pxc",
		Scenario: "A scratch table for a one-off audit export was created without a primary key, then left behind.",
		Symptom:  "Galera replicates row-based changes by primary key when one exists; without one, replication falls back to matching on the full row image — this scratch table demonstrates the difference safely, without touching any table this app's own workload actually depends on.",
		Setup: []string{
			"CREATE TABLE IF NOT EXISTS challenge_scratch_no_pk (label VARCHAR(64), val INT) ENGINE=InnoDB",
			"INSERT INTO challenge_scratch_no_pk (label, val) VALUES ('seed-a',1),('seed-b',2),('seed-c',3)",
		},
		Teardown: []string{"DROP TABLE IF EXISTS challenge_scratch_no_pk"},
		ShapeIDs: []string{},
		Hints: []Hint{
			{1, "Does challenge_scratch_no_pk have a primary key? Check information_schema or DESCRIBE it."},
			{2, "Galera's row-based replication uses the primary key to identify which row an UPDATE/DELETE targets on other nodes — without one, it has to match on every column instead."},
			{3, "ALTER TABLE challenge_scratch_no_pk ADD COLUMN id INT AUTO_INCREMENT PRIMARY KEY FIRST — or any equivalent primary key."},
		},
		FunctionalCheck: func(ctx context.Context, db *sql.DB) string {
			var n int
			err := db.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS
				WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='challenge_scratch_no_pk' AND CONSTRAINT_TYPE='PRIMARY KEY'`).Scan(&n)
			if err != nil || n == 0 {
				return "challenge_scratch_no_pk still has no primary key"
			}
			return ""
		},
	},
}

// ByID returns the challenge for id, or ok=false.
func ByID(id string) (Challenge, bool) {
	for _, c := range Catalog {
		if c.ID == id {
			return c, true
		}
	}
	return Challenge{}, false
}
