package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"

	"stocksim/internal/store"
)

// tickLoop runs fn every interval until ctx is done.
func tickLoop(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// opsThisTick converts the level's ops/sec target into a per-tick batch size,
// so the same tick interval produces Low/Medium/High throughput.
func (e *Engine) opsThisTick(interval time.Duration) int {
	target := ordersPerSecond[e.Level()]
	n := int(target * interval.Seconds())
	if n < 1 && target > 0 {
		n = 1
	}
	return n
}

func newAgentRand() *rand.Rand { return rand.New(rand.NewSource(time.Now().UnixNano())) }

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// heartbeat records an agent's state, swallowing the write error — a
// heartbeat failing is itself a symptom of the store being unreachable, which
// the agent's own noteErr has already captured.
func (e *Engine) heartbeat(ctx context.Context, agent, status, detail string) {
	e.Store.Heartbeat(ctx, agent, status, detail)
}

// emit writes an event durably and then publishes it — durable-first, so a
// browser that was not connected still sees it in the backfill.
func (e *Engine) emit(ctx context.Context, kind, symbol, msg string) {
	ev := store.Event{TS: time.Now().UTC(), Kind: kind, Symbol: symbol, Message: msg}
	if err := e.Store.AppendEvent(ctx, ev); err != nil {
		e.noteErr("append event", err)
		return
	}
}

// ------------------------------------------------------------- price agent

// runPriceAgent moves every listed security's price once a second by a small
// random walk, writes the batch of ticks, and applies the new prices to the
// securities rows. Volatility is per-sector so the ticker grid does not look
// uniformly random: technology names move roughly twice as much as utilities.
func (e *Engine) runPriceAgent(ctx context.Context) {
	rnd := newAgentRand()
	const interval = time.Second

	tickLoop(ctx, interval, func() {
		if !e.Running() {
			e.heartbeat(ctx, "price", "idle", "paused")
			return
		}
		secs, _, err := e.Store.ListSecurities(ctx, store.ListQuery{Limit: 500})
		if err != nil {
			e.noteErr("price: list securities", err)
			e.heartbeat(ctx, "price", "error", err.Error())
			return
		}
		if len(secs) == 0 {
			e.heartbeat(ctx, "price", "idle", "no securities")
			return
		}

		now := time.Now().UTC()
		ticks := make([]store.Tick, 0, len(secs))
		quotes := make([]store.Quote, 0, len(secs))
		updates := make([]QuoteUpdate, 0, len(secs))

		for _, s := range secs {
			if !s.Listed || s.LastPrice <= 0 {
				continue
			}
			vol := sectorVolatility(s.Sector)
			// A small mean-reverting pull toward the session open keeps prices
			// from drifting to absurd values over a long-running deployment,
			// while still allowing a real trend within a session.
			drift := (s.OpenPrice - s.LastPrice) / s.OpenPrice * 0.02
			shock := rnd.NormFloat64() * vol
			next := s.LastPrice * (1 + drift + shock)
			if next < 0.01 {
				next = 0.01
			}
			next = round2(next)
			qty := int64(rnd.Intn(9000) + 100)

			ticks = append(ticks, store.Tick{
				SecurityID: s.ID, Symbol: s.Symbol, TS: now, Price: next, Volume: qty,
			})
			quotes = append(quotes, store.Quote{
				SecurityID: s.ID, Price: next, Volume: qty, TS: now,
			})
			changePct := 0.0
			if s.OpenPrice != 0 {
				changePct = (next - s.OpenPrice) / s.OpenPrice * 100
			}
			updates = append(updates, QuoteUpdate{
				SecurityID: s.ID, Symbol: s.Symbol, Price: next, ChangePct: changePct,
			})
		}

		if err := e.Store.AppendTicks(ctx, ticks); err != nil {
			e.noteErr("price: append ticks", err)
			e.heartbeat(ctx, "price", "error", err.Error())
			return
		}
		if err := e.Store.ApplyQuotes(ctx, quotes); err != nil {
			e.noteErr("price: apply quotes", err)
			e.heartbeat(ctx, "price", "error", err.Error())
			return
		}
		e.counters.ticksWritten.Add(int64(len(ticks)))
		e.Bus.publishJSON(busMessage{Type: "quotes", Quotes: updates})
		e.heartbeat(ctx, "price", "ok", fmt.Sprintf("%d quotes", len(quotes)))
	})
}

// sectorVolatility is the per-tick standard deviation of the random walk,
// expressed as a fraction of price.
func sectorVolatility(sector string) float64 {
	switch sector {
	case "Technology":
		return 0.0035
	case "Healthcare", "Energy":
		return 0.0030
	case "Financials", "Consumer", "Communications":
		return 0.0022
	case "Materials", "Industrials":
		return 0.0018
	case "Utilities":
		return 0.0012
	}
	return 0.0020
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// ------------------------------------------------------------- order agent

// runOrderAgent has the seeded portfolios place orders at the level's target
// rate. Most are market orders; a minority are limit orders placed away from
// the current price, so the open book always has something in it for the match
// agent to work through and for the CRUD table to show.
func (e *Engine) runOrderAgent(ctx context.Context) {
	rnd := newAgentRand()
	const interval = time.Second

	tickLoop(ctx, interval, func() {
		if !e.Running() {
			e.heartbeat(ctx, "orders", "idle", "paused")
			return
		}
		n := e.opsThisTick(interval)
		if n == 0 {
			return
		}
		secs, _, err := e.Store.ListSecurities(ctx, store.ListQuery{Limit: 500})
		if err != nil {
			e.noteErr("orders: list securities", err)
			e.heartbeat(ctx, "orders", "error", err.Error())
			return
		}
		pfs, _, err := e.Store.ListPortfolios(ctx, store.ListQuery{Limit: 500})
		if err != nil {
			e.noteErr("orders: list portfolios", err)
			e.heartbeat(ctx, "orders", "error", err.Error())
			return
		}
		if len(secs) == 0 || len(pfs) == 0 {
			e.heartbeat(ctx, "orders", "idle", "nothing to trade")
			return
		}

		placed := 0
		for i := 0; i < n; i++ {
			s := secs[rnd.Intn(len(secs))]
			if !s.Listed || s.LastPrice <= 0 {
				continue
			}
			p := pfs[rnd.Intn(len(pfs))]

			side := store.SideBuy
			if rnd.Float64() < 0.45 {
				side = store.SideSell
			}
			otype, limit := store.TypeMarket, 0.0
			if rnd.Float64() < 0.35 {
				otype = store.TypeLimit
				// Buy limits sit below the market, sell limits above — so they
				// rest in the book until the price comes to them.
				off := 1 + (rnd.Float64()*0.02 + 0.002)
				if side == store.SideBuy {
					limit = round2(s.LastPrice / off)
				} else {
					limit = round2(s.LastPrice * off)
				}
			}
			qty := int64((rnd.Intn(20) + 1) * 25)

			if _, err := e.Store.CreateOrder(ctx, store.Order{
				PortfolioID: p.ID, SecurityID: s.ID, Symbol: s.Symbol, Owner: p.Owner,
				Side: side, OrderType: otype, Quantity: qty, LimitPrice: limit,
				Status: store.OrderOpen, CreatedAt: time.Now().UTC(),
			}); err != nil {
				e.noteErr("orders: create", err)
				continue
			}
			e.counters.ordersCreated.Add(1)
			placed++
		}
		e.heartbeat(ctx, "orders", "ok", fmt.Sprintf("%d placed", placed))
	})
}

// ------------------------------------------------------------- match agent

// runMatchAgent fills what it can from the open book. Market orders fill at
// the current price; limit orders fill only when the market has crossed them.
// A limit order that has been resting for more than an hour of simulated time
// is expired, so the book does not grow without bound.
func (e *Engine) runMatchAgent(ctx context.Context) {
	const interval = time.Second

	tickLoop(ctx, interval, func() {
		if !e.Running() {
			e.heartbeat(ctx, "matching", "idle", "paused")
			return
		}
		open, err := e.Store.OpenOrders(ctx, 400)
		if err != nil {
			e.noteErr("match: open orders", err)
			e.heartbeat(ctx, "matching", "error", err.Error())
			return
		}
		if len(open) == 0 {
			e.heartbeat(ctx, "matching", "ok", "book empty")
			return
		}
		secs, _, err := e.Store.ListSecurities(ctx, store.ListQuery{Limit: 500})
		if err != nil {
			e.noteErr("match: list securities", err)
			return
		}
		price := map[string]float64{}
		for _, s := range secs {
			price[s.ID] = s.LastPrice
		}

		// Settlement limits are enforced here, at the single point where
		// positions and cash actually change. Without them the agents sell
		// stock they never owned and spend money they never had, which leaves
		// negative holdings and negative cash — and a weighted average cost is
		// undefined once a position crosses zero, so the report ends up
		// showing negative average costs and meaningless P/L. Rejecting the
		// order instead keeps every figure in the report interpretable, and
		// "rejected" is a state the orders table already filters on.
		cash := map[string]float64{}
		if pfs, _, perr := e.Store.ListPortfolios(ctx, store.ListQuery{Limit: 500}); perr == nil {
			for _, p := range pfs {
				cash[p.ID] = p.Cash
			}
		}
		held := map[string]int64{}
		if hs, herr := e.Store.ListHoldings(ctx, ""); herr == nil {
			for _, h := range hs {
				held[h.PortfolioID+"\x00"+h.SecurityID] = h.Quantity
			}
		}

		filled, expired, rejected := 0, 0, 0
		cutoff := time.Now().UTC().Add(-90 * time.Second)
		for _, o := range open {
			last, ok := price[o.SecurityID]
			if !ok || last <= 0 {
				continue // the security was deleted out from under the order
			}
			fillAt, canFill := matchPrice(o, last)
			if !canFill {
				if o.OrderType == store.TypeLimit && o.CreatedAt.Before(cutoff) {
					o.Status = store.OrderCancelled
					if _, err := e.Store.UpdateOrder(ctx, o); err == nil {
						e.counters.ordersExpired.Add(1)
						expired++
					}
				}
				continue
			}

			// The running maps are updated as fills are applied, so several
			// fills in one pass cannot together overdraw an account.
			key := o.PortfolioID + "\x00" + o.SecurityID
			notional := fillAt * float64(o.Quantity)
			if reason := settlementBlock(o, notional, cash[o.PortfolioID], held[key]); reason != "" {
				o.Status = store.OrderRejected
				if _, err := e.Store.UpdateOrder(ctx, o); err == nil {
					e.counters.ordersRejected.Add(1)
					rejected++
				}
				continue
			}

			t := store.Trade{
				OrderID: o.ID, PortfolioID: o.PortfolioID, SecurityID: o.SecurityID,
				Symbol: o.Symbol, Side: o.Side, Quantity: o.Quantity,
				Price: fillAt, TS: time.Now().UTC(),
			}
			if err := e.Store.RecordFill(ctx, o, t); err != nil {
				e.noteErr("match: record fill", err)
				continue
			}
			if o.Side == store.SideBuy {
				cash[o.PortfolioID] -= notional
				held[key] += o.Quantity
			} else {
				cash[o.PortfolioID] += notional
				held[key] -= o.Quantity
			}
			e.counters.ordersFilled.Add(1)
			filled++
			if t.Notional() >= 250_000 {
				e.emit(ctx, "trade", o.Symbol, fmt.Sprintf("%s %s %d @ %.2f (%s)",
					titleCase(o.Side), o.Symbol, o.Quantity, fillAt, o.Owner))
			}
		}
		e.heartbeat(ctx, "matching", "ok",
			fmt.Sprintf("%d filled, %d rejected, %d expired, %d resting",
				filled, rejected, expired, len(open)-filled-expired-rejected))
	})
}

// settlementBlock returns why an order cannot settle, or "" if it can. This is
// what keeps holdings non-negative and cash solvent: no naked shorts, no
// buying on credit. Both are simplifications of a real market, and both are
// what make the printed report's average costs and P/L mean something.
func settlementBlock(o store.Order, notional, cash float64, held int64) string {
	if o.Side == store.SideBuy && notional > cash {
		return "insufficient cash"
	}
	if o.Side == store.SideSell && o.Quantity > held {
		return "insufficient position"
	}
	return ""
}

// matchPrice decides whether an order can execute against the current price,
// and at what price it executes.
func matchPrice(o store.Order, last float64) (float64, bool) {
	if o.OrderType == store.TypeMarket {
		return last, true
	}
	if o.Side == store.SideBuy && last <= o.LimitPrice {
		return o.LimitPrice, true
	}
	if o.Side == store.SideSell && last >= o.LimitPrice {
		return o.LimitPrice, true
	}
	return 0, false
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// --------------------------------------------------------- analytics agent

// Metrics is the KPI blob the analytics agent writes and the dashboard reads
// back verbatim. Kept as one JSON document so adding a number here needs no
// change in any store implementation.
type Metrics struct {
	Securities     int     `json:"securities"`
	Portfolios     int     `json:"portfolios"`
	OpenOrders     int64   `json:"openOrders"`
	FilledOrders   int64   `json:"filledOrders"`
	TotalTrades    int64   `json:"totalTrades"`
	OrdersCreated  int64   `json:"ordersCreated"`
	OrdersFilled   int64   `json:"ordersFilled"`
	OrdersExpired  int64   `json:"ordersExpired"`
	OrdersRejected int64   `json:"ordersRejected"`
	TicksWritten   int64   `json:"ticksWritten"`
	Errors         int64   `json:"errors"`
	MarketCap      float64 `json:"marketCap"`
	DayVolume      int64   `json:"dayVolume"`
	Advancers      int     `json:"advancers"`
	Decliners      int     `json:"decliners"`
	Unchanged      int     `json:"unchanged"`
	IndexLevel     float64 `json:"indexLevel"`
	PortfolioAUM   float64 `json:"portfolioAum"`
	UpdatedAt      string  `json:"updatedAt"`
}

// runAnalyticsAgent rolls up the market into the metrics blob every two
// seconds. The index level is an equal-weighted average of every listed
// security's percentage change from its open, rebased to 1000.
func (e *Engine) runAnalyticsAgent(ctx context.Context) {
	tickLoop(ctx, 2*time.Second, func() {
		secs, _, err := e.Store.ListSecurities(ctx, store.ListQuery{Limit: 500})
		if err != nil {
			e.noteErr("analytics: list securities", err)
			e.heartbeat(ctx, "analytics", "error", err.Error())
			return
		}
		pfs, _, err := e.Store.ListPortfolios(ctx, store.ListQuery{Limit: 500})
		if err != nil {
			e.noteErr("analytics: list portfolios", err)
			return
		}
		// Cumulative, not just what is still in the table: retention deletes
		// settled orders, and a "filled orders" figure that went *down* over
		// time would be a wrong number nothing about the page would flag.
		counts, err := store.CumulativeOrderCounts(ctx, e.Store)
		if err != nil {
			e.noteErr("analytics: order counts", err)
			return
		}
		holdings, err := e.Store.ListHoldings(ctx, "")
		if err != nil {
			e.noteErr("analytics: holdings", err)
			return
		}

		m := Metrics{
			Securities:     len(secs),
			Portfolios:     len(pfs),
			OpenOrders:     counts[store.OrderOpen],
			FilledOrders:   counts[store.OrderFilled],
			OrdersCreated:  e.counters.ordersCreated.Load(),
			OrdersFilled:   e.counters.ordersFilled.Load(),
			OrdersExpired:  e.counters.ordersExpired.Load(),
			OrdersRejected: e.counters.ordersRejected.Load(),
			TicksWritten:   e.counters.ticksWritten.Load(),
			Errors:         e.counters.errors.Load(),
			TotalTrades:    counts[store.OrderFilled],
			UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
		}
		var pctSum float64
		var listed int
		for _, s := range secs {
			if !s.Listed {
				continue
			}
			listed++
			m.MarketCap += s.MarketCap()
			m.DayVolume += s.DayVolume
			switch c := s.Change(); {
			case c > 0:
				m.Advancers++
			case c < 0:
				m.Decliners++
			default:
				m.Unchanged++
			}
			pctSum += s.ChangePct()
		}
		if listed > 0 {
			m.IndexLevel = round2(1000 * (1 + pctSum/float64(listed)/100))
		}
		for _, p := range pfs {
			m.PortfolioAUM += p.Cash
		}
		for _, h := range holdings {
			m.PortfolioAUM += h.MarketValue()
		}

		if err := e.Store.PutMetrics(ctx, "current", m); err != nil {
			e.noteErr("analytics: put metrics", err)
			e.heartbeat(ctx, "analytics", "error", err.Error())
			return
		}
		e.heartbeat(ctx, "analytics", "ok",
			fmt.Sprintf("index %.2f, %d up / %d down", m.IndexLevel, m.Advancers, m.Decliners))
	})
}

// -------------------------------------------------------- monitoring agent

// Diag is the backend-facing blob: what engine this is, what version, and what
// objects the app currently owns. Refreshed every five seconds, which is what
// lets the Schema panel show tables appearing after a seed and vanishing after
// a drop without the browser polling the database itself.
type Diag struct {
	Engine        string             `json:"engine"`
	ServerVersion string             `json:"serverVersion"`
	Database      string             `json:"database"`
	Location      string             `json:"location"`
	TargetKind    string             `json:"targetKind"`
	TargetLabel   string             `json:"targetLabel"`
	Objects       []store.ObjectInfo `json:"objects"`
	TotalRows     int64              `json:"totalRows"`
	TotalBytes    int64              `json:"totalBytes"`
}

func (e *Engine) runMonitoringAgent(ctx context.Context) {
	tickLoop(ctx, 5*time.Second, func() {
		d := Diag{
			Engine:      e.Store.Engine(),
			Database:    e.Store.Database(),
			Location:    e.Store.Location(),
			TargetKind:  e.TargetKind,
			TargetLabel: e.TargetLabel,
		}
		if v, err := e.Store.ServerVersion(ctx); err == nil {
			d.ServerVersion = v
		} else {
			e.noteErr("monitoring: server version", err)
			e.heartbeat(ctx, "monitoring", "error", err.Error())
			return
		}
		objs, err := e.Store.Objects(ctx)
		if err != nil {
			e.noteErr("monitoring: objects", err)
			e.heartbeat(ctx, "monitoring", "error", err.Error())
			return
		}
		d.Objects = objs
		for _, o := range objs {
			d.TotalRows += o.Rows
			d.TotalBytes += o.Bytes
		}
		if err := e.Store.PutMetrics(ctx, "diag", d); err != nil {
			e.noteErr("monitoring: put diag", err)
			return
		}
		e.heartbeat(ctx, "monitoring", "ok", fmt.Sprintf("%d objects", len(objs)))
	})
}

// ------------------------------------------------------------- event feed

// runEventFeed polls for newly written events and pushes them to connected
// browsers. Polling the table rather than publishing at the write site is
// deliberate: it means an event written by any process — including a second
// replica of this app, or a human inserting a row by hand — still reaches
// every dashboard.
func (e *Engine) runEventFeed(ctx context.Context) {
	var cursor string
	// Start from the current tail so a restart does not replay the whole
	// history into every connected browser.
	if evs, err := e.Store.EventsSince(ctx, "", 500); err == nil && len(evs) > 0 {
		cursor = evs[len(evs)-1].ID
	}
	tickLoop(ctx, 500*time.Millisecond, func() {
		evs, err := e.Store.EventsSince(ctx, cursor, 100)
		if err != nil {
			e.noteErr("eventfeed", err)
			return
		}
		for i := range evs {
			ev := evs[i]
			cursor = ev.ID
			e.Bus.publishJSON(busMessage{Type: "event", Event: &ev})
		}
	})
}

// runClockPersister checkpoints simulated time every ten seconds so a restart
// resumes the session instead of jumping back to midnight.
func (e *Engine) runClockPersister(ctx context.Context) {
	tickLoop(ctx, 10*time.Second, func() {
		e.persistClock(ctx)
	})
}
