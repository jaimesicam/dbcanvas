package store

import (
	"context"
	"time"
)

// reportFrom assembles a Report using nothing but the Store interface, so all
// four engines share one implementation and cannot drift into disagreeing
// about what a report contains. Each store's ReportData is a one-line call to
// this.
//
// The order matters only in that everything is read in one pass, close
// together in time: a report describes an instant, and the further apart these
// reads drift the less internally consistent the printed page becomes.
func reportFrom(ctx context.Context, s Store, limitTrades int) (Report, error) {
	r := Report{
		GeneratedAt: time.Now().UTC(),
		Engine:      s.Engine(),
		Database:    s.Database(),
	}
	var err error
	if r.ServerVersion, err = s.ServerVersion(ctx); err != nil {
		return r, err
	}
	if r.Securities, _, err = s.ListSecurities(ctx, ListQuery{Limit: 500}); err != nil {
		return r, err
	}
	if r.Portfolios, _, err = s.ListPortfolios(ctx, ListQuery{Limit: 500}); err != nil {
		return r, err
	}
	if r.Holdings, err = s.ListHoldings(ctx, ""); err != nil {
		return r, err
	}
	if r.RecentTrades, err = s.RecentTrades(ctx, clampLimit(limitTrades, 50, 1000)); err != nil {
		return r, err
	}
	if r.OrderCounts, err = s.CountOrdersByStatus(ctx); err != nil {
		return r, err
	}
	if r.Objects, err = s.Objects(ctx); err != nil {
		return r, err
	}
	if r.TotalTrades, r.TotalVolume, err = s.TradeTotals(ctx); err != nil {
		return r, err
	}
	return r, nil
}
