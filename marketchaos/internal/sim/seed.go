package sim

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"marketchaos/internal/store"
)

// SeedProgress is reported to the caller (main.go / Engine) after every
// completed batch, and mirrored into the sim_state table (id="seed") so the
// dashboard's seeding panel survives a page reload and reflects the same
// state every browser tab polling this container sees.
type SeedProgress struct {
	Table     string    `json:"table"`
	RowsDone  int64     `json:"rowsDone"`
	RowsTotal int64     `json:"rowsTotal"`
	StartedAt time.Time `json:"startedAt"`
	Done      bool      `json:"done"`
	Error     string    `json:"error,omitempty"`
}

// seedWorkers is how many parallel BulkInsert loaders run per table — the
// product spec's own "4-8 parallel loaders partitioned by symbol"
// recommendation. A flat constant rather than scaling with traffic level or
// dataset size: seeding happens once, before any workload agent is running,
// so it isn't competing with anything else for the connection pool.
const seedWorkers = 6

// seedLookbackDays bounds how far back generated historical timestamps
// (orders/trades/ticks/news) spread — recent enough that "today's" market
// overview and the price-history challenges have a believable recent
// window, wide enough that range-scan challenges have real breadth to scan.
const seedLookbackDays = 90

// Seed populates every domain table for the given counts, in dependency
// order, against an already-schema'd, empty (or freshly Wiped) database.
// Determinism: every table's row generator is seeded from worldSeed plus a
// fixed per-table salt, so re-seeding an identically-sized profile always
// produces the same data — useful for challenge validation (see stage S4's
// plan), which needs a stable baseline to compare against.
func Seed(ctx context.Context, st *store.Store, counts DatasetCounts, family string, onProgress func(SeedProgress)) error {
	d := counts.Derive()
	started := time.Now()

	securities, popCum := LoadWorld()

	now := time.Now().UTC()
	lookback := now.Add(-seedLookbackDays * 24 * time.Hour)

	// watchlists and positions are precomputed pair lists (see their BulkInsert
	// call sites below for why) — built up front, before `total` below, since
	// their real row counts (driven by per-trader/per-account random inclusion,
	// not a fixed formula) are exactly what a size estimate needs and Derive()
	// can't predict.
	type wlRow struct{ traderID, secIdx, order int }
	var wlRows []wlRow
	for t := 0; t < d.Traders; t++ {
		r := rand.New(rand.NewSource(worldSeed + 3_000_000 + int64(t)))
		picked := map[int]bool{}
		for len(picked) < 2 && len(picked) < len(securities) {
			picked[r.Intn(len(securities))] = true
		}
		order := 0
		for secIdx := range picked {
			wlRows = append(wlRows, wlRow{traderID: t + 1, secIdx: secIdx, order: order})
			order++
		}
	}
	type posRow struct{ accountID, secIdx int }
	var posRows []posRow
	for a := 0; a < d.Accounts; a++ {
		r := rand.New(rand.NewSource(worldSeed + 6_000_000 + int64(a)))
		if r.Intn(100) >= 60 { // ~60% of accounts hold any positions at all
			continue
		}
		n := 1 + r.Intn(4)
		picked := map[int]bool{}
		for len(picked) < n && len(picked) < len(securities) {
			picked[r.Intn(len(securities))] = true
		}
		for secIdx := range picked {
			posRows = append(posRows, posRow{accountID: a + 1, secIdx: secIdx})
		}
	}
	ledgerRows := d.Trades * 2

	total := int64(SectorCount+SecurityCount+SecurityCount) + // sectors + securities + market_quotes
		int64(d.Traders+d.Accounts+d.Orders+d.Trades+d.Ticks+d.News+d.AuditLogs) +
		int64(len(wlRows)+len(posRows)+ledgerRows)

	// completedBase is the row count of every table fully finished so far;
	// mu guards it and the onProgress publish together. BulkInsert's own
	// `done` callback argument is already a correct atomic running total for
	// the table currently in flight, but it's called concurrently from every
	// worker goroutine — publishing straight through without a lock let two
	// workers race on the previous version of this function's delta-tracking
	// state and corrupt rowsDone (caught by live verification: reported done
	// exceeded the reported total). Locking the whole read-add-publish
	// sequence here removes that race without slowing seeding itself, since
	// progress publishing is already throttled far below batch frequency by
	// the Engine.
	var mu sync.Mutex
	var completedBase int64
	report := func(table string, doneForTable int) {
		mu.Lock()
		rowsDone := completedBase + int64(doneForTable)
		snap := SeedProgress{Table: table, RowsDone: rowsDone, RowsTotal: total, StartedAt: started}
		mu.Unlock()
		if onProgress != nil {
			onProgress(snap)
		}
	}
	progressFn := func(table string) func(done int) {
		return func(done int) { report(table, done) }
	}
	// finishTable is called once a table's BulkInsert has fully returned
	// (workers are joined, so no further progress(...) calls for it can
	// still be in flight) — safe to fold its exact row count into the base
	// for every subsequent table's reports.
	finishTable := func(rows int) {
		mu.Lock()
		completedBase += int64(rows)
		mu.Unlock()
	}

	// --- sectors ---
	if err := st.BulkInsert(ctx, store.TableSectors, []string{"sector_id", "name"}, len(Sectors), 1,
		func(i int) []any { return []any{Sectors[i].ID, Sectors[i].Name} }, progressFn("sectors")); err != nil {
		return fmt.Errorf("seed sectors: %w", err)
	}
	finishTable(len(Sectors))

	// --- securities. security_id is assigned explicitly (i+1): every other
	// table below references a security via secIdx+1 assuming the row at
	// LoadWorld() index i has security_id exactly i+1 — the same
	// auto-increment-gap hazard documented at the orders insert below, except
	// here the blast radius is every table in the schema (all of them
	// reference security_id), so this is the single most important table to
	// pin explicitly. ---
	if err := st.BulkInsert(ctx, store.TableSecurities,
		[]string{"security_id", "symbol", "company_name", "sector_id", "status", "ipo_date", "shares_outstanding", "created_at"},
		len(securities), 1, func(i int) []any {
			s := securities[i]
			ipo := lookback.AddDate(-rand.New(rand.NewSource(worldSeed+int64(i))).Intn(15), 0, 0)
			return []any{i + 1, s.Symbol, s.CompanyName, s.SectorID, "active", ipo, s.SharesOut, now}
		}, progressFn("securities")); err != nil {
		return fmt.Errorf("seed securities: %w", err)
	}
	finishTable(len(securities))

	// --- traders. trader_id assigned explicitly (i+1) — accounts.trader_id
	// and watchlists.trader_id below both assume traders occupy exactly
	// [1, d.Traders]; seedWorkers=6 concurrent loaders makes a retried batch
	// here more likely than most tables, so this is not a theoretical risk.
	// See the orders insert below for the full explanation of this hazard. ---
	if err := st.BulkInsert(ctx, store.TableTraders,
		[]string{"trader_id", "username", "email", "account_status", "risk_level", "country_code", "created_at", "last_login_at"},
		d.Traders, seedWorkers, func(i int) []any {
			r := rand.New(rand.NewSource(worldSeed + 1_000_000 + int64(i)))
			username := fmt.Sprintf("trader%06d", i+1)
			email := username + "@example.net"
			risk := []string{"conservative", "moderate", "aggressive"}[r.Intn(3)]
			country := []string{"US", "GB", "DE", "SG", "AU", "CA", "JP"}[r.Intn(7)]
			created := lookback.Add(-time.Duration(r.Intn(365*3)) * 24 * time.Hour)
			lastLogin := created.Add(time.Duration(r.Intn(int(now.Sub(created).Hours()))) * time.Hour)
			return []any{i + 1, username, email, "active", risk, country, created, lastLogin}
		}, progressFn("traders")); err != nil {
		return fmt.Errorf("seed traders: %w", err)
	}
	finishTable(d.Traders)

	// --- accounts. account_id assigned explicitly (i+1) — orders,
	// posRows/positions, and account_ledger below all pick a random account
	// via 1+r.Intn(d.Accounts), assuming accounts occupy exactly
	// [1, d.Accounts]. trader_id (i+1) relies on the same guarantee just
	// established for traders above. ---
	if err := st.BulkInsert(ctx, store.TableAccounts,
		[]string{"account_id", "trader_id", "cash_balance", "reserved_cash", "margin_limit", "updated_at"},
		d.Accounts, seedWorkers, func(i int) []any {
			r := rand.New(rand.NewSource(worldSeed + 2_000_000 + int64(i)))
			cash := 1_000 + r.Float64()*499_000
			return []any{i + 1, i + 1, round2(cash), 0.0, round2(cash * 0.5), now}
		}, progressFn("accounts")); err != nil {
		return fmt.Errorf("seed accounts: %w", err)
	}
	finishTable(d.Accounts)

	// --- watchlists: 2 distinct securities per trader, precomputed above
	// into wlRows so the UNIQUE(trader_id, security_id) key never collides
	// (BulkInsert needs a fixed row count up front). ---
	if err := st.BulkInsert(ctx, store.TableWatchlists,
		[]string{"trader_id", "security_id", "added_at", "display_order"},
		len(wlRows), seedWorkers, func(i int) []any {
			w := wlRows[i]
			return []any{w.traderID, w.secIdx + 1, now, w.order}
		}, progressFn("watchlists")); err != nil {
		return fmt.Errorf("seed watchlists: %w", err)
	}
	finishTable(len(wlRows))

	// --- orders. order_id is assigned explicitly (i+1) rather than left to
	// AUTO_INCREMENT — found live (stage S7 final verification): a PXC
	// batch that fails its Galera certification and gets retried (see
	// seedbulk.go's execBatchWithRetry, added in this same verification
	// pass) permanently burns the auto-increment values that failed
	// attempt would have used, since InnoDB never rolls back the counter
	// itself. That leaves gaps in the actual order_id range, breaking
	// trades' assumption below that valid order ids span exactly
	// [1, d.Orders] — a real trade ended up seeded with a buy_order_id
	// that had been burned by a retry and never belonged to an actual row,
	// tripping CheckInvariants' "trade references a nonexistent order"
	// check the very first time a PXC seed needed to retry. Explicit ids
	// make a retried batch idempotent (the failed attempt left nothing
	// behind for those specific values, so re-inserting them is safe) and
	// guarantee the [1, d.Orders] range trades relies on is exactly right. ---
	orderStatuses := []string{"filled", "filled", "filled", "cancelled", "open", "partial"}
	if err := st.BulkInsert(ctx, store.TableOrders,
		[]string{"order_id", "account_id", "security_id", "side", "order_type", "quantity", "remaining_quantity", "limit_price", "status", "priority", "created_at", "updated_at", "cancelled_at"},
		d.Orders, seedWorkers, func(i int) []any {
			r := rand.New(rand.NewSource(worldSeed + 4_000_000 + int64(i)))
			secIdx := weightedPick(popCum, r)
			sec := securities[secIdx]
			side := "buy"
			if r.Intn(2) == 0 {
				side = "sell"
			}
			orderType := "market"
			var limitPrice any
			if r.Intn(2) == 0 {
				orderType = "limit"
				limitPrice = round2(sec.StartPrice * (0.95 + r.Float64()*0.1))
			}
			qty := 1 + r.Intn(500)
			status := orderStatuses[r.Intn(len(orderStatuses))]
			remaining := 0
			if status == "open" {
				remaining = qty
			} else if status == "partial" {
				remaining = r.Intn(qty)
			}
			created := lookback.Add(time.Duration(r.Int63n(int64(now.Sub(lookback)))))
			updated := created.Add(time.Duration(r.Intn(3600)) * time.Second)
			var cancelledAt any
			if status == "cancelled" {
				cancelledAt = updated
			}
			return []any{
				i + 1, 1 + r.Intn(d.Accounts), secIdx + 1, side, orderType, qty, remaining, limitPrice,
				status, 0, created, updated, cancelledAt,
			}
		}, progressFn("orders")); err != nil {
		return fmt.Errorf("seed orders: %w", err)
	}
	finishTable(d.Orders)

	// --- trades. Buy/sell order ids reference random valid orders (not
	// necessarily the literal pair a matching engine would have produced —
	// seed data is statistically plausible, not perfectly cross-table
	// reconciled; stage S2's live matching-engine agents are what maintains
	// true consistency for everything generated after the app starts).
	// trade_id assigned explicitly (i+1) — account_ledger below picks
	// tradeID := i/2+1, assuming trades occupy exactly [1, d.Trades]; see
	// the orders insert above for the full explanation of this hazard. ---
	if err := st.BulkInsert(ctx, store.TableTrades,
		[]string{"trade_id", "buy_order_id", "sell_order_id", "security_id", "quantity", "price", "trade_value", "executed_at"},
		d.Trades, seedWorkers, func(i int) []any {
			r := rand.New(rand.NewSource(worldSeed + 5_000_000 + int64(i)))
			secIdx := weightedPick(popCum, r)
			sec := securities[secIdx]
			qty := 1 + r.Intn(300)
			price := round2(sec.StartPrice * (0.9 + r.Float64()*0.2))
			executed := lookback.Add(time.Duration(r.Int63n(int64(now.Sub(lookback)))))
			buyOrder := 1 + r.Intn(d.Orders)
			sellOrder := 1 + r.Intn(d.Orders)
			return []any{i + 1, buyOrder, sellOrder, secIdx + 1, qty, price, round2(price * float64(qty)), executed}
		}, progressFn("trades")); err != nil {
		return fmt.Errorf("seed trades: %w", err)
	}
	finishTable(d.Trades)

	// --- positions: a subset of accounts each hold 1-4 distinct securities,
	// precomputed above into posRows for the same PK-uniqueness reason as
	// watchlists. ---
	if err := st.BulkInsert(ctx, store.TablePositions,
		[]string{"account_id", "security_id", "quantity", "average_cost", "realized_profit", "updated_at"},
		len(posRows), seedWorkers, func(i int) []any {
			p := posRows[i]
			r := rand.New(rand.NewSource(worldSeed + 7_000_000 + int64(i)))
			sec := securities[p.secIdx]
			qty := 1 + r.Intn(1000)
			avgCost := round2(sec.StartPrice * (0.85 + r.Float64()*0.3))
			realized := round2((r.Float64() - 0.5) * 5000)
			return []any{p.accountID, p.secIdx + 1, qty, avgCost, realized, now}
		}, progressFn("positions")); err != nil {
		return fmt.Errorf("seed positions: %w", err)
	}
	finishTable(len(posRows))

	// --- account_ledger: 2 entries per trade (a debit and a credit),
	// referencing random accounts rather than the trade's own (unknown at
	// seed time, since orders/trades aren't cross-referenced — see above). ---
	if err := st.BulkInsert(ctx, store.TableAccountLedger,
		[]string{"account_id", "trade_id", "entry_type", "amount", "balance_after", "created_at"},
		ledgerRows, seedWorkers, func(i int) []any {
			r := rand.New(rand.NewSource(worldSeed + 8_000_000 + int64(i)))
			tradeID := i/2 + 1
			entryType := "credit"
			amount := round2(r.Float64() * 5000)
			if i%2 == 0 {
				entryType = "debit"
				amount = -amount
			}
			created := lookback.Add(time.Duration(r.Int63n(int64(now.Sub(lookback)))))
			return []any{1 + r.Intn(d.Accounts), tradeID, entryType, amount, round2(r.Float64() * 50000), created}
		}, progressFn("account_ledger")); err != nil {
		return fmt.Errorf("seed account_ledger: %w", err)
	}
	finishTable(ledgerRows)

	// --- market_news ---
	sentiments := []string{"positive", "neutral", "neutral", "negative"}
	headlines := []string{
		"reports quarterly results", "announces new product line", "expands into new markets",
		"names new chief executive", "faces regulatory scrutiny", "beats analyst expectations",
		"issues guidance update", "completes strategic acquisition", "unveils cost-cutting plan",
		"raises full-year outlook",
	}
	if err := st.BulkInsert(ctx, store.TableMarketNews,
		[]string{"security_id", "headline", "body", "sentiment", "published_at"},
		d.News, seedWorkers, func(i int) []any {
			r := rand.New(rand.NewSource(worldSeed + 9_000_000 + int64(i)))
			var secID any
			headline := headlines[r.Intn(len(headlines))]
			if r.Intn(10) < 9 {
				secIdx := weightedPick(popCum, r)
				sec := securities[secIdx]
				secID = secIdx + 1
				headline = sec.CompanyName + " " + headline
			} else {
				headline = "Market update: " + headline
			}
			body := headline + ". Full coverage of this fictional development for MarketChaos's simulated exchange."
			published := lookback.Add(time.Duration(r.Int63n(int64(now.Sub(lookback)))))
			return []any{secID, headline, body, sentiments[r.Intn(len(sentiments))], published}
		}, progressFn("market_news")); err != nil {
		return fmt.Errorf("seed market_news: %w", err)
	}
	finishTable(d.News)

	// --- audit_events: one per order, "order_placed" ---
	if err := st.BulkInsert(ctx, store.TableAuditEvents,
		[]string{"entity_type", "entity_id", "event_type", "event_payload", "created_at"},
		d.AuditLogs, seedWorkers, func(i int) []any {
			r := rand.New(rand.NewSource(worldSeed + 10_000_000 + int64(i)))
			created := lookback.Add(time.Duration(r.Int63n(int64(now.Sub(lookback)))))
			return []any{"order", i + 1, "order_placed", nil, created}
		}, progressFn("audit_events")); err != nil {
		return fmt.Errorf("seed audit_events: %w", err)
	}
	finishTable(d.AuditLogs)

	// --- market_quotes: exactly one row per security, current-state snapshot ---
	if err := st.BulkInsert(ctx, store.TableMarketQuotes,
		[]string{"security_id", "last_price", "bid_price", "ask_price", "day_open", "day_high", "day_low", "previous_close", "volume", "updated_at", "version"},
		len(securities), seedWorkers, func(i int) []any {
			r := rand.New(rand.NewSource(worldSeed + 11_000_000 + int64(i)))
			sec := securities[i]
			last := round2(sec.StartPrice * (0.98 + r.Float64()*0.04))
			spread := last * 0.001
			dayHigh := round2(last * (1 + r.Float64()*0.02))
			dayLow := round2(last * (1 - r.Float64()*0.02))
			return []any{
				i + 1, last, round2(last - spread), round2(last + spread), round2(sec.StartPrice), dayHigh, dayLow,
				round2(sec.StartPrice), int64(1000 + r.Intn(500000)), now, 1,
			}
		}, progressFn("market_quotes")); err != nil {
		return fmt.Errorf("seed market_quotes: %w", err)
	}
	finishTable(len(securities))

	// --- price_ticks: largest table, seeded last. ---
	if err := st.BulkInsert(ctx, store.TablePriceTicks,
		[]string{"security_id", "price", "bid_price", "ask_price", "volume", "exchange_code", "recorded_at"},
		d.Ticks, seedWorkers, func(i int) []any {
			r := rand.New(rand.NewSource(worldSeed + 12_000_000 + int64(i)))
			secIdx := weightedPick(popCum, r)
			sec := securities[secIdx]
			price := round2(sec.StartPrice * (0.9 + r.Float64()*0.2))
			spread := price * 0.001
			recorded := lookback.Add(time.Duration(r.Int63n(int64(now.Sub(lookback)))))
			return []any{secIdx + 1, price, round2(price - spread), round2(price + spread), 1 + r.Intn(2000), "XNAS", recorded}
		}, progressFn("price_ticks")); err != nil {
		return fmt.Errorf("seed price_ticks: %w", err)
	}

	finishTable(d.Ticks)
	if onProgress != nil {
		mu.Lock()
		rowsDone := completedBase
		mu.Unlock()
		onProgress(SeedProgress{Table: "", RowsDone: rowsDone, RowsTotal: total, StartedAt: started, Done: true})
	}
	return nil
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// cumulativeWeights builds a cumulative-sum array of each security's
// Popularity for O(log N) weighted random selection (weightedPick) — used
// everywhere a symbol needs to be picked with the product spec's "a few
// symbols receive most of the activity" skew, both at seed time and (stage
// S2 onward) by live agents.
func cumulativeWeights(securities []Security) []float64 {
	cum := make([]float64, len(securities))
	sum := 0.0
	for i, s := range securities {
		sum += s.Popularity
		cum[i] = sum
	}
	return cum
}

func weightedPick(cum []float64, r *rand.Rand) int {
	if len(cum) == 0 {
		return 0
	}
	target := r.Float64() * cum[len(cum)-1]
	i := sort.SearchFloat64s(cum, target)
	if i >= len(cum) {
		i = len(cum) - 1
	}
	return i
}
