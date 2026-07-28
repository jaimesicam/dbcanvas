package sim

import (
	"context"
	"math/rand"
	"time"
)

// ------------------------------------------------------------- market data

// runMarketDataAgent is the market's own price feed: every tick it moves a
// batch of securities' prices by a small random walk (weighted toward
// popular symbols, same as every other agent's security choice) and records
// both the new quote snapshot and a price_ticks row for it. Rate-driven —
// see workers.go's tickLoop.
func (e *Engine) runMarketDataAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "market-data", "idle", detailStr(events, errs))
			return
		}
		n := opsThisTick(e.agentRate(sharesFor(e.Mix()).MarketData), time.Second)
		for i := 0; i < n; i++ {
			octx, cancel := opCtx(ctx)
			ok := e.tickPrice(octx, rng)
			cancel()
			if ok {
				events++
			} else {
				errs++
				e.counters.agentErrors.Add(1)
			}
		}
		e.Store.Heartbeat(ctx, "market-data", "ok", detailStr(events, errs))
	})
}

func (e *Engine) tickPrice(ctx context.Context, rng *rand.Rand) bool {
	secIdx := weightedPick(e.popCum, rng)
	sec := e.Securities[secIdx]
	secID := secIdx + 1

	var last float64
	if err := e.Store.DB.QueryRowContext(ctx, "SELECT last_price FROM market_quotes WHERE security_id=?", secID).Scan(&last); err != nil {
		return false
	}
	move := 1 + (rng.Float64()-0.5)*0.01 // +-0.5% per tick
	next := round2(last * move)
	if lo := sec.StartPrice * 0.5; next < lo {
		next = round2(lo)
	}
	if hi := sec.StartPrice * 2; next > hi {
		next = round2(hi)
	}
	spread := next * 0.001
	vol := 1 + rng.Intn(2000)
	now := time.Now().UTC()

	_, err := e.Store.DB.ExecContext(ctx,
		`UPDATE market_quotes SET last_price=?, bid_price=?, ask_price=?,
		 day_high=GREATEST(day_high,?), day_low=LEAST(day_low,?), volume=volume+?, updated_at=?, version=version+1
		 WHERE security_id=?`,
		next, round2(next-spread), round2(next+spread), next, next, vol, now, secID)
	if err != nil {
		return false
	}
	_, err = e.Store.DB.ExecContext(ctx,
		"INSERT INTO price_ticks (security_id, price, bid_price, ask_price, volume, exchange_code, recorded_at) VALUES (?,?,?,?,?,?,?)",
		secID, next, round2(next-spread), round2(next+spread), vol, "XNAS", now)
	return err == nil
}

// -------------------------------------------------------------------- news

var newsSentiments = []string{"positive", "neutral", "neutral", "negative"}
var newsHeadlines = []string{
	"reports quarterly results", "announces new product line", "expands into new markets",
	"names new chief executive", "faces regulatory scrutiny", "beats analyst expectations",
	"issues guidance update", "completes strategic acquisition", "unveils cost-cutting plan",
	"raises full-year outlook",
}

// runNewsAgent publishes market_news rows at a much lower rate than price
// ticks — a live counterpart to the same headline pool seed.go used to
// backfill history with, so the market_news table keeps growing the same
// way after startup as it did before it.
func (e *Engine) runNewsAgent(ctx context.Context) {
	rng := newAgentRand()
	var events, errs int64
	tickLoop(ctx, 5*time.Second, func() {
		if !e.Running() {
			e.Store.Heartbeat(ctx, "news", "idle", detailStr(events, errs))
			return
		}
		n := opsThisTick(e.agentRate(sharesFor(e.Mix()).News), 5*time.Second)
		for i := 0; i < n; i++ {
			octx, cancel := opCtx(ctx)
			ok := e.publishNews(octx, rng)
			cancel()
			if ok {
				events++
			} else {
				errs++
				e.counters.agentErrors.Add(1)
			}
		}
		e.Store.Heartbeat(ctx, "news", "ok", detailStr(events, errs))
	})
}

func (e *Engine) publishNews(ctx context.Context, rng *rand.Rand) bool {
	headline := newsHeadlines[rng.Intn(len(newsHeadlines))]
	var secID any
	if rng.Intn(10) < 9 {
		secIdx := weightedPick(e.popCum, rng)
		sec := e.Securities[secIdx]
		secID = secIdx + 1
		headline = sec.CompanyName + " " + headline
	} else {
		headline = "Market update: " + headline
	}
	body := headline + ". Full coverage of this fictional development for MarketChaos's simulated exchange."
	_, err := e.Store.DB.ExecContext(ctx,
		"INSERT INTO market_news (security_id, headline, body, sentiment, published_at) VALUES (?,?,?,?,?)",
		secID, headline, body, newsSentiments[rng.Intn(len(newsSentiments))], time.Now().UTC())
	return err == nil
}
