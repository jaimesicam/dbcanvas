package api

import (
	"encoding/csv"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"time"

	"stocksim/internal/store"
)

// The report endpoints. /api/report.json is the single source the printable
// /report page renders from; /api/report.csv serves the same data as a
// download, one table at a time. Both come from one Store.ReportData call, so
// a printed page and a downloaded CSV taken seconds apart describe the same
// market rather than two different instants.

// reportEnvelope wraps the store's raw report with the derived figures the
// page would otherwise have to recompute in JavaScript — and, more to the
// point, would then have to keep in step with the CSV export.
type reportEnvelope struct {
	store.Report
	SessionDate string           `json:"sessionDate"`
	SimRate     float64          `json:"simRate"`
	Summary     reportSummary    `json:"summary"`
	TopGainers  []store.Security `json:"topGainers"`
	TopLosers   []store.Security `json:"topLosers"`
	MostTraded  []store.Security `json:"mostTraded"`
	Statements  []statement      `json:"statements"`
}

type reportSummary struct {
	Securities  int     `json:"securities"`
	Listed      int     `json:"listed"`
	Advancers   int     `json:"advancers"`
	Decliners   int     `json:"decliners"`
	Unchanged   int     `json:"unchanged"`
	MarketCap   float64 `json:"marketCap"`
	DayVolume   int64   `json:"dayVolume"`
	IndexLevel  float64 `json:"indexLevel"`
	TotalAUM    float64 `json:"totalAum"`
	TotalCash   float64 `json:"totalCash"`
	TotalEquity float64 `json:"totalEquity"`
	FillRate    float64 `json:"fillRate"`
}

// statement is one portfolio's section of the report: its cash, its positions,
// and the totals a reader would otherwise have to add up by hand.
type statement struct {
	Portfolio    store.Portfolio `json:"portfolio"`
	Holdings     []store.Holding `json:"holdings"`
	MarketValue  float64         `json:"marketValue"`
	CostBasis    float64         `json:"costBasis"`
	UnrealisedPL float64         `json:"unrealisedPl"`
	TotalValue   float64         `json:"totalValue"`
}

func (h *Handler) buildReport(r *http.Request) (reportEnvelope, error) {
	base, err := h.Store.ReportData(r.Context(), intParam(r, "trades", 100))
	if err != nil {
		return reportEnvelope{}, err
	}
	base.TargetKind = h.Engine.TargetKind
	base.TargetLabel = h.Engine.TargetLabel
	base.SessionDate = h.Engine.Clock.Today()

	env := reportEnvelope{
		Report:      base,
		SessionDate: h.Engine.Clock.Today().Format("2006-01-02"),
		SimRate:     h.Engine.Clock.Rate(),
	}

	var pctSum float64
	for _, s := range base.Securities {
		env.Summary.Securities++
		if !s.Listed {
			continue
		}
		env.Summary.Listed++
		env.Summary.MarketCap += s.MarketCap()
		env.Summary.DayVolume += s.DayVolume
		pctSum += s.ChangePct()
		switch c := s.Change(); {
		case c > 0:
			env.Summary.Advancers++
		case c < 0:
			env.Summary.Decliners++
		default:
			env.Summary.Unchanged++
		}
	}
	if env.Summary.Listed > 0 {
		env.Summary.IndexLevel = 1000 * (1 + pctSum/float64(env.Summary.Listed)/100)
	}

	// Movers: sort copies so the securities list itself keeps its symbol order
	// for the main market table.
	byChange := append([]store.Security(nil), base.Securities...)
	sort.SliceStable(byChange, func(i, j int) bool {
		return byChange[i].ChangePct() > byChange[j].ChangePct()
	})
	env.TopGainers = headSecurities(byChange, 5)
	env.TopLosers = tailSecuritiesReversed(byChange, 5)

	byVolume := append([]store.Security(nil), base.Securities...)
	sort.SliceStable(byVolume, func(i, j int) bool {
		return byVolume[i].DayVolume > byVolume[j].DayVolume
	})
	env.MostTraded = headSecurities(byVolume, 5)

	// Per-portfolio statements.
	holdingsBy := map[string][]store.Holding{}
	for _, hd := range base.Holdings {
		holdingsBy[hd.PortfolioID] = append(holdingsBy[hd.PortfolioID], hd)
	}
	for _, p := range base.Portfolios {
		st := statement{Portfolio: p, Holdings: holdingsBy[p.ID]}
		if st.Holdings == nil {
			st.Holdings = []store.Holding{}
		}
		for _, hd := range st.Holdings {
			st.MarketValue += hd.MarketValue()
			st.CostBasis += hd.CostBasis()
		}
		st.UnrealisedPL = st.MarketValue - st.CostBasis
		st.TotalValue = p.Cash + st.MarketValue
		env.Statements = append(env.Statements, st)

		env.Summary.TotalCash += p.Cash
		env.Summary.TotalEquity += st.MarketValue
	}
	env.Summary.TotalAUM = env.Summary.TotalCash + env.Summary.TotalEquity

	// Every order that was ever placed counts toward the denominator, rejected
	// ones included — they were real instructions the book could not settle,
	// and hiding them would flatter the fill rate.
	var placed int64
	for _, n := range base.OrderCounts {
		placed += n
	}
	if placed > 0 {
		env.Summary.FillRate = float64(base.OrderCounts[store.OrderFilled]) / float64(placed) * 100
	}
	if env.Statements == nil {
		env.Statements = []statement{}
	}
	return env, nil
}

// handleReportPage serves the printable report's HTML. The page fetches
// /api/report.json itself, so this is a plain static file — no templating, and
// no second rendering path to keep in step with the JSON.
func (h *Handler) handleReportPage(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(h.Web, "report.html")
	if err != nil {
		http.Error(w, "report page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (h *Handler) handleReportJSON(w http.ResponseWriter, r *http.Request) {
	env, err := h.buildReport(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, env)
}

// handleReportCSV serves one table of the report as a download. The table is
// chosen by ?table=, defaulting to holdings — the one a reader most often
// wants in a spreadsheet.
func (h *Handler) handleReportCSV(w http.ResponseWriter, r *http.Request) {
	table := r.URL.Query().Get("table")
	if table == "" {
		table = "holdings"
	}
	env, err := h.buildReport(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	var header []string
	var rows [][]string
	switch table {
	case "securities":
		header = []string{"symbol", "name", "sector", "currency", "open", "last",
			"change", "change_pct", "day_high", "day_low", "day_volume",
			"shares_outstanding", "market_cap", "listed"}
		for _, s := range env.Securities {
			rows = append(rows, []string{
				s.Symbol, s.Name, s.Sector, s.Currency,
				f2(s.OpenPrice), f2(s.LastPrice), f2(s.Change()), f2(s.ChangePct()),
				f2(s.DayHigh), f2(s.DayLow), i64(s.DayVolume),
				i64(s.Shares), f2(s.MarketCap()), strconv.FormatBool(s.Listed),
			})
		}
	case "portfolios":
		header = []string{"name", "owner", "cash", "market_value", "cost_basis",
			"unrealised_pl", "total_value"}
		for _, st := range env.Statements {
			rows = append(rows, []string{
				st.Portfolio.Name, st.Portfolio.Owner, f2(st.Portfolio.Cash),
				f2(st.MarketValue), f2(st.CostBasis), f2(st.UnrealisedPL), f2(st.TotalValue),
			})
		}
	case "holdings":
		header = []string{"owner", "symbol", "quantity", "avg_cost", "last_price",
			"market_value", "cost_basis", "unrealised_pl"}
		for _, hd := range env.Holdings {
			rows = append(rows, []string{
				hd.Owner, hd.Symbol, i64(hd.Quantity), f2(hd.AvgCost), f2(hd.LastPrice),
				f2(hd.MarketValue()), f2(hd.CostBasis()), f2(hd.UnrealisedPL()),
			})
		}
	case "trades":
		header = []string{"timestamp", "symbol", "side", "quantity", "price", "notional"}
		for _, t := range env.RecentTrades {
			rows = append(rows, []string{
				t.TS.UTC().Format(time.RFC3339), t.Symbol, t.Side,
				i64(t.Quantity), f2(t.Price), f2(t.Notional()),
			})
		}
	case "objects":
		header = []string{"name", "kind", "rows", "bytes"}
		for _, o := range env.Objects {
			rows = append(rows, []string{o.Name, o.Kind, i64(o.Rows), i64(o.Bytes)})
		}
	default:
		http.Error(w,
			"unknown table: choose one of securities, portfolios, holdings, trades, objects",
			http.StatusBadRequest)
		return
	}

	filename := fmt.Sprintf("stocksim-%s-%s.csv", table, time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	cw := csv.NewWriter(w)
	cw.Write(header)
	cw.WriteAll(rows)
	cw.Flush()
}

func f2(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }
func i64(n int64) string  { return strconv.FormatInt(n, 10) }

func headSecurities(s []store.Security, n int) []store.Security {
	if len(s) < n {
		n = len(s)
	}
	out := append([]store.Security{}, s[:n]...)
	return out
}

// tailSecuritiesReversed takes the last n of a descending-sorted slice and
// flips them, so the worst performer leads the losers table.
func tailSecuritiesReversed(s []store.Security, n int) []store.Security {
	if len(s) < n {
		n = len(s)
	}
	out := make([]store.Security, 0, n)
	for i := len(s) - 1; i >= len(s)-n; i-- {
		out = append(out, s[i])
	}
	return out
}
