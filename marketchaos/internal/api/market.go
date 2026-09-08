package api

import (
	"net/http"
	"strconv"
)

func (h *Handler) handleMarketOverview(w http.ResponseWriter, r *http.Request) {
	ov, err := h.Engine.MarketOverview(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, ov)
}

func (h *Handler) handleRecentTrades(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	trades, err := h.Engine.RecentTrades(r.Context(), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, trades)
}

func (h *Handler) handleOrderBook(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		http.Error(w, "symbol query param required", http.StatusBadRequest)
		return
	}
	depth, err := h.Engine.OrderBookDepth(r.Context(), symbol)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, depth)
}

// handlePortfolio defaults to account 1 — this app has no learner login
// concept (see the product spec: it's a single shared simulated market),
// so the portfolio panel just shows one representative sample account,
// selectable via ?account=N for anyone curious about a different one.
func (h *Handler) handlePortfolio(w http.ResponseWriter, r *http.Request) {
	accountID, err := strconv.Atoi(r.URL.Query().Get("account"))
	if err != nil || accountID <= 0 {
		accountID = 1
	}
	view, err := h.Engine.Portfolio(r.Context(), accountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}
