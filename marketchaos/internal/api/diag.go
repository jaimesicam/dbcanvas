package api

import (
	"encoding/json"
	"net/http"
)

func (h *Handler) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.Engine.Leaderboard())
}

func (h *Handler) handleServerStats(w http.ResponseWriter, r *http.Request) {
	view, err := h.Engine.ServerStatsView(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}

// handleWsrep always returns 200 with a zero-valued body on a non-PXC
// target (see store.WsrepStatus's doc comment) — the dashboard's PXC panel
// is what decides whether to render it, based on the target kind already
// carried in GET /api/state, not on this endpoint erroring.
func (h *Handler) handleWsrep(w http.ResponseWriter, r *http.Request) {
	st, err := h.Engine.Wsrep(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, st)
}

func (h *Handler) handleHAProxy(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Engine.HAProxyStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rows)
}

func (h *Handler) handleProcesslist(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Engine.Processlist(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rows)
}

func (h *Handler) handleLockWaits(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Engine.LockWaits(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rows)
}

func (h *Handler) handleTableSizes(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Engine.TableSizes(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rows)
}

// handleExplain is the one endpoint that runs a learner-supplied SQL
// string — restricted to EXPLAIN's own plan output (see store.Explain's doc
// comment on why that's safe regardless of what's typed). Runs against the
// request's own context, which already has an implicit deadline from the
// HTTP server/client — no separate bounded context needed the way agents
// need opCtx.
func (h *Handler) handleExplain(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SQL == "" {
		http.Error(w, "invalid body: expected {\"sql\": \"SELECT ...\"}", http.StatusBadRequest)
		return
	}
	rows, err := h.Engine.Explain(r.Context(), body.SQL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, rows)
}
