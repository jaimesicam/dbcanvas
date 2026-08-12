package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"stocksim/internal/store"
)

// The CRUD handlers. Each follows the same shape: decode into a request struct
// whose fields are all optional pointers on update, validate against the typed
// constants in the store package, then hand a domain struct to the store and
// let writeStoreErr map its sentinel errors onto status codes.

// ------------------------------------------------------------- securities

type securityReq struct {
	Symbol    string  `json:"symbol"`
	Name      string  `json:"name"`
	Sector    string  `json:"sector"`
	Currency  string  `json:"currency"`
	Shares    int64   `json:"sharesOutstanding"`
	OpenPrice float64 `json:"openPrice"`
	LastPrice float64 `json:"lastPrice"`
	Listed    *bool   `json:"listed"`
}

// normalise applies the defaults and formatting rules that make a
// hand-created security behave like a seeded one.
func (q *securityReq) normalise() {
	q.Symbol = strings.ToUpper(strings.TrimSpace(q.Symbol))
	q.Name = strings.TrimSpace(q.Name)
	q.Sector = strings.TrimSpace(q.Sector)
	q.Currency = strings.ToUpper(strings.TrimSpace(q.Currency))
	if q.Currency == "" {
		q.Currency = "USD"
	}
	// A security with an opening price but no last price has simply not traded
	// yet — start it at its open so the ticker shows a flat line, not a crash
	// to zero.
	if q.LastPrice == 0 {
		q.LastPrice = q.OpenPrice
	}
	if q.OpenPrice == 0 {
		q.OpenPrice = q.LastPrice
	}
}

func (q securityReq) validate() string {
	switch {
	case q.Symbol == "":
		return "symbol is required"
	case len(q.Symbol) > 16:
		return "symbol must be 16 characters or fewer"
	case q.Name == "":
		return "name is required"
	case q.OpenPrice < 0 || q.LastPrice < 0:
		return "prices cannot be negative"
	case q.Shares < 0:
		return "shares outstanding cannot be negative"
	}
	return ""
}

func (h *Handler) listSecurities(w http.ResponseWriter, r *http.Request) {
	items, total, err := h.Store.ListSecurities(r.Context(), listQuery(r))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"securities": items, "total": total,
		"limit": intParam(r, "limit", 50), "offset": intParam(r, "offset", 0),
	})
}

func (h *Handler) getSecurity(w http.ResponseWriter, r *http.Request) {
	s, err := h.Store.GetSecurity(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, s)
}

func (h *Handler) createSecurity(w http.ResponseWriter, r *http.Request) {
	var q securityReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	q.normalise()
	if msg := q.validate(); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	listed := true
	if q.Listed != nil {
		listed = *q.Listed
	}
	created, err := h.Store.CreateSecurity(r.Context(), store.Security{
		Symbol: q.Symbol, Name: q.Name, Sector: q.Sector, Currency: q.Currency,
		Shares: q.Shares, OpenPrice: q.OpenPrice, LastPrice: q.LastPrice, Listed: listed,
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	h.Store.AppendEvent(r.Context(), store.Event{
		Kind: "crud", Symbol: created.Symbol,
		Message: "Security " + created.Symbol + " created",
	})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, created)
}

func (h *Handler) updateSecurity(w http.ResponseWriter, r *http.Request) {
	existing, err := h.Store.GetSecurity(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	// Seed the request from the stored row so a partial body only changes what
	// it actually mentions.
	q := securityReq{
		Symbol: existing.Symbol, Name: existing.Name, Sector: existing.Sector,
		Currency: existing.Currency, Shares: existing.Shares,
		OpenPrice: existing.OpenPrice, LastPrice: existing.LastPrice,
	}
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	q.normalise()
	if msg := q.validate(); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	existing.Symbol, existing.Name, existing.Sector = q.Symbol, q.Name, q.Sector
	existing.Currency, existing.Shares, existing.OpenPrice = q.Currency, q.Shares, q.OpenPrice
	if q.Listed != nil {
		existing.Listed = *q.Listed
	}
	updated, err := h.Store.UpdateSecurity(r.Context(), existing)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	h.Store.AppendEvent(r.Context(), store.Event{
		Kind: "crud", Symbol: updated.Symbol,
		Message: "Security " + updated.Symbol + " updated",
	})
	writeJSON(w, updated)
}

func (h *Handler) deleteSecurity(w http.ResponseWriter, r *http.Request) {
	existing, err := h.Store.GetSecurity(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if err := h.Store.DeleteSecurity(r.Context(), existing.ID); err != nil {
		writeStoreErr(w, err)
		return
	}
	h.Store.AppendEvent(r.Context(), store.Event{
		Kind: "crud", Symbol: existing.Symbol,
		Message: "Security " + existing.Symbol + " delisted and removed",
	})
	writeJSON(w, map[string]string{"status": "ok", "deleted": existing.ID})
}

func (h *Handler) securityTicks(w http.ResponseWriter, r *http.Request) {
	// A zero time asks for the newest ticks there are, which is what a sparkline
	// wants; the working-set agent is the only caller that passes a real one.
	ticks, err := h.Store.TicksBefore(r.Context(), r.PathValue("id"),
		time.Time{}, intParam(r, "limit", 60))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if ticks == nil {
		ticks = []store.Tick{}
	}
	writeJSON(w, map[string]any{"ticks": ticks})
}

// ------------------------------------------------------------- portfolios

type portfolioReq struct {
	Name  string   `json:"name"`
	Owner string   `json:"owner"`
	Cash  *float64 `json:"cash"`
}

func (h *Handler) listPortfolios(w http.ResponseWriter, r *http.Request) {
	items, total, err := h.Store.ListPortfolios(r.Context(), listQuery(r))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"portfolios": items, "total": total,
		"limit": intParam(r, "limit", 50), "offset": intParam(r, "offset", 0),
	})
}

func (h *Handler) getPortfolio(w http.ResponseWriter, r *http.Request) {
	p, err := h.Store.GetPortfolio(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, p)
}

func (h *Handler) createPortfolio(w http.ResponseWriter, r *http.Request) {
	var q portfolioReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	q.Name, q.Owner = strings.TrimSpace(q.Name), strings.TrimSpace(q.Owner)
	if q.Name == "" || q.Owner == "" {
		http.Error(w, "name and owner are required", http.StatusBadRequest)
		return
	}
	cash := 1_000_000.0
	if q.Cash != nil {
		cash = *q.Cash
	}
	created, err := h.Store.CreatePortfolio(r.Context(), store.Portfolio{
		Name: q.Name, Owner: q.Owner, Cash: cash,
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	h.Store.AppendEvent(r.Context(), store.Event{
		Kind: "crud", Message: "Portfolio " + created.Name + " opened for " + created.Owner,
	})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, created)
}

func (h *Handler) updatePortfolio(w http.ResponseWriter, r *http.Request) {
	existing, err := h.Store.GetPortfolio(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	q := portfolioReq{Name: existing.Name, Owner: existing.Owner}
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	q.Name, q.Owner = strings.TrimSpace(q.Name), strings.TrimSpace(q.Owner)
	if q.Name == "" || q.Owner == "" {
		http.Error(w, "name and owner are required", http.StatusBadRequest)
		return
	}
	existing.Name, existing.Owner = q.Name, q.Owner
	if q.Cash != nil {
		existing.Cash = *q.Cash
	}
	updated, err := h.Store.UpdatePortfolio(r.Context(), existing)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, updated)
}

func (h *Handler) deletePortfolio(w http.ResponseWriter, r *http.Request) {
	existing, err := h.Store.GetPortfolio(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if err := h.Store.DeletePortfolio(r.Context(), existing.ID); err != nil {
		writeStoreErr(w, err)
		return
	}
	h.Store.AppendEvent(r.Context(), store.Event{
		Kind: "crud", Message: "Portfolio " + existing.Name + " closed",
	})
	writeJSON(w, map[string]string{"status": "ok", "deleted": existing.ID})
}

func (h *Handler) portfolioHoldings(w http.ResponseWriter, r *http.Request) {
	hs, err := h.Store.ListHoldings(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if hs == nil {
		hs = []store.Holding{}
	}
	writeJSON(w, map[string]any{"holdings": hs})
}

// ----------------------------------------------------------------- orders

type orderReq struct {
	PortfolioID string  `json:"portfolioId"`
	SecurityID  string  `json:"securityId"`
	Side        string  `json:"side"`
	OrderType   string  `json:"orderType"`
	Quantity    int64   `json:"quantity"`
	LimitPrice  float64 `json:"limitPrice"`
	Status      string  `json:"status"`
}

func (h *Handler) listOrders(w http.ResponseWriter, r *http.Request) {
	items, total, err := h.Store.ListOrders(r.Context(), listQuery(r))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"orders": items, "total": total,
		"limit": intParam(r, "limit", 50), "offset": intParam(r, "offset", 0),
	})
}

func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request) {
	o, err := h.Store.GetOrder(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, o)
}

// createOrder resolves the portfolio and security by id so the denormalised
// symbol and owner columns stay correct — they exist so the orders table can
// be listed and searched without a join on every engine, including the two
// that have no joins at all.
func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	var q orderReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if q.OrderType == "" {
		q.OrderType = store.TypeMarket
	}
	switch {
	case !store.ValidSide(q.Side):
		http.Error(w, "side must be buy or sell", http.StatusBadRequest)
		return
	case !store.ValidType(q.OrderType):
		http.Error(w, "orderType must be market or limit", http.StatusBadRequest)
		return
	case q.Quantity <= 0:
		http.Error(w, "quantity must be greater than zero", http.StatusBadRequest)
		return
	case q.OrderType == store.TypeLimit && q.LimitPrice <= 0:
		http.Error(w, "a limit order needs a limit price", http.StatusBadRequest)
		return
	}

	pf, err := h.Store.GetPortfolio(r.Context(), q.PortfolioID)
	if err != nil {
		http.Error(w, "unknown portfolio", http.StatusBadRequest)
		return
	}
	sec, err := h.Store.GetSecurity(r.Context(), q.SecurityID)
	if err != nil {
		http.Error(w, "unknown security", http.StatusBadRequest)
		return
	}

	created, err := h.Store.CreateOrder(r.Context(), store.Order{
		PortfolioID: pf.ID, SecurityID: sec.ID, Symbol: sec.Symbol, Owner: pf.Owner,
		Side: q.Side, OrderType: q.OrderType, Quantity: q.Quantity,
		LimitPrice: q.LimitPrice, Status: store.OrderOpen,
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	h.Store.AppendEvent(r.Context(), store.Event{
		Kind: "crud", Symbol: sec.Symbol,
		Message: titleWord(q.Side) + " order for " + sec.Symbol + " placed by " + pf.Owner,
	})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, created)
}

func (h *Handler) updateOrder(w http.ResponseWriter, r *http.Request) {
	existing, err := h.Store.GetOrder(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	q := orderReq{
		Side: existing.Side, OrderType: existing.OrderType,
		Quantity: existing.Quantity, LimitPrice: existing.LimitPrice,
		Status: existing.Status,
	}
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	switch {
	case !store.ValidSide(q.Side):
		http.Error(w, "side must be buy or sell", http.StatusBadRequest)
		return
	case !store.ValidType(q.OrderType):
		http.Error(w, "orderType must be market or limit", http.StatusBadRequest)
		return
	case !store.ValidStatus(q.Status):
		http.Error(w, "status must be one of open, filled, cancelled, rejected", http.StatusBadRequest)
		return
	case q.Quantity <= 0:
		http.Error(w, "quantity must be greater than zero", http.StatusBadRequest)
		return
	}
	existing.Side, existing.OrderType = q.Side, q.OrderType
	existing.Quantity, existing.LimitPrice, existing.Status = q.Quantity, q.LimitPrice, q.Status
	updated, err := h.Store.UpdateOrder(r.Context(), existing)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, updated)
}

func (h *Handler) deleteOrder(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.DeleteOrder(r.Context(), r.PathValue("id")); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func titleWord(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
