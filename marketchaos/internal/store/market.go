// market.go backs the market-overview/live-trading/portfolio dashboard
// panels (stage S6) — plain read-only queries for on-demand display, not
// part of the continuous leaderboard-tracked workload, so unlike the agent
// queries in internal/sim these are free to use whatever JOIN/alias shape
// reads best (see IMPLEMENTATION.md session 180 on why that distinction
// matters for the *workload* queries but not for these).
package store

import "context"

type MarketMover struct {
	Symbol      string  `json:"symbol"`
	CompanyName string  `json:"companyName"`
	LastPrice   float64 `json:"lastPrice"`
	PctChange   float64 `json:"pctChange"`
	Volume      int64   `json:"volume"`
}

type SectorVolume struct {
	SectorName string `json:"sectorName"`
	Volume     int64  `json:"volume"`
}

type MarketOverview struct {
	TopGainers []MarketMover  `json:"topGainers"`
	TopLosers  []MarketMover  `json:"topLosers"`
	TotalVol   int64          `json:"totalVolume"`
	Sectors    []SectorVolume `json:"sectors"`
}

func (s *Store) MarketOverview(ctx context.Context) (MarketOverview, error) {
	var ov MarketOverview

	gainers, err := s.queryMovers(ctx, "DESC")
	if err != nil {
		return ov, err
	}
	losers, err := s.queryMovers(ctx, "ASC")
	if err != nil {
		return ov, err
	}
	ov.TopGainers, ov.TopLosers = gainers, losers

	if err := s.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(volume),0) FROM market_quotes").Scan(&ov.TotalVol); err != nil {
		return ov, err
	}

	rows, err := s.DB.QueryContext(ctx, `
		SELECT sectors.name, COALESCE(SUM(market_quotes.volume),0) AS vol
		FROM sectors
		JOIN securities ON securities.sector_id = sectors.sector_id
		JOIN market_quotes ON market_quotes.security_id = securities.security_id
		GROUP BY sectors.sector_id, sectors.name ORDER BY vol DESC`)
	if err != nil {
		return ov, err
	}
	defer rows.Close()
	for rows.Next() {
		var sv SectorVolume
		if rows.Scan(&sv.SectorName, &sv.Volume) == nil {
			ov.Sectors = append(ov.Sectors, sv)
		}
	}
	return ov, rows.Err()
}

func (s *Store) queryMovers(ctx context.Context, direction string) ([]MarketMover, error) {
	order := "DESC"
	if direction == "ASC" {
		order = "ASC"
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT securities.symbol, securities.company_name, market_quotes.last_price,
		       (market_quotes.last_price - market_quotes.previous_close) / market_quotes.previous_close * 100 AS pct_change,
		       market_quotes.volume
		FROM market_quotes JOIN securities ON securities.security_id = market_quotes.security_id
		WHERE market_quotes.previous_close > 0
		ORDER BY pct_change `+order+` LIMIT 5`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MarketMover
	for rows.Next() {
		var m MarketMover
		if rows.Scan(&m.Symbol, &m.CompanyName, &m.LastPrice, &m.PctChange, &m.Volume) == nil {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

type RecentTrade struct {
	TradeID    int64   `json:"tradeId"`
	Symbol     string  `json:"symbol"`
	Price      float64 `json:"price"`
	Quantity   int64   `json:"quantity"`
	ExecutedAt string  `json:"executedAt"`
}

func (s *Store) RecentTrades(ctx context.Context, limit int) ([]RecentTrade, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT trades.trade_id, securities.symbol, trades.price, trades.quantity, trades.executed_at
		FROM trades JOIN securities ON securities.security_id = trades.security_id
		ORDER BY trades.trade_id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentTrade
	for rows.Next() {
		var t RecentTrade
		if rows.Scan(&t.TradeID, &t.Symbol, &t.Price, &t.Quantity, &t.ExecutedAt) == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

type OrderBookLevel struct {
	Side  string `json:"side"`
	Count int    `json:"count"`
	Qty   int64  `json:"qty"`
}

// OrderBookDepth summarizes open buy/sell order counts+quantity for one
// symbol — not a real price-level order book (this app doesn't need one),
// just enough to show "how many open orders are on each side right now."
func (s *Store) OrderBookDepth(ctx context.Context, symbol string) ([]OrderBookLevel, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT orders.side, COUNT(*), COALESCE(SUM(orders.remaining_quantity),0)
		FROM orders JOIN securities ON securities.security_id = orders.security_id
		WHERE securities.symbol = ? AND orders.status IN ('open','partial')
		GROUP BY orders.side`, symbol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrderBookLevel
	for rows.Next() {
		var l OrderBookLevel
		if rows.Scan(&l.Side, &l.Count, &l.Qty) == nil {
			out = append(out, l)
		}
	}
	return out, rows.Err()
}

type PortfolioHolding struct {
	Symbol       string  `json:"symbol"`
	Quantity     int     `json:"quantity"`
	AverageCost  float64 `json:"averageCost"`
	LastPrice    float64 `json:"lastPrice"`
	MarketValue  float64 `json:"marketValue"`
	UnrealizedPL float64 `json:"unrealizedPl"`
}

type PortfolioView struct {
	AccountID   int                `json:"accountId"`
	CashBalance float64            `json:"cashBalance"`
	Holdings    []PortfolioHolding `json:"holdings"`
	HoldingsVal float64            `json:"holdingsValue"`
	TotalValue  float64            `json:"totalValue"`
}

func (s *Store) Portfolio(ctx context.Context, accountID int) (PortfolioView, error) {
	view := PortfolioView{AccountID: accountID}
	if err := s.DB.QueryRowContext(ctx, "SELECT cash_balance FROM accounts WHERE account_id=?", accountID).Scan(&view.CashBalance); err != nil {
		return view, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT securities.symbol, positions.quantity, positions.average_cost, market_quotes.last_price
		FROM positions
		JOIN securities ON securities.security_id = positions.security_id
		JOIN market_quotes ON market_quotes.security_id = positions.security_id
		WHERE positions.account_id = ? AND positions.quantity <> 0
		ORDER BY securities.symbol`, accountID)
	if err != nil {
		return view, err
	}
	defer rows.Close()
	for rows.Next() {
		var h PortfolioHolding
		if rows.Scan(&h.Symbol, &h.Quantity, &h.AverageCost, &h.LastPrice) != nil {
			continue
		}
		h.MarketValue = float64(h.Quantity) * h.LastPrice
		h.UnrealizedPL = float64(h.Quantity) * (h.LastPrice - h.AverageCost)
		view.HoldingsVal += h.MarketValue
		view.Holdings = append(view.Holdings, h)
	}
	view.TotalValue = view.CashBalance + view.HoldingsVal
	return view, rows.Err()
}
