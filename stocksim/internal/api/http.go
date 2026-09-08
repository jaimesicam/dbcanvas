// Package api serves the dashboard, the CRUD endpoints, and the report.
//
// Two things here have no precedent in the sibling sims. The first is real
// CRUD: every other sim's browser is a read-only observer plus a knob panel,
// while this one has POST/PUT/DELETE on three resources. The second is CORS —
// see corsMiddleware for exactly why it exists and what it is scoped to.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"stocksim/internal/sim"
	"stocksim/internal/store"
)

type Handler struct {
	Engine *sim.Engine
	Store  store.Store
	Web    fs.FS
}

func New(e *sim.Engine, st store.Store, webFS fs.FS) *Handler {
	return &Handler{Engine: e, Store: st, Web: webFS}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /api/state", h.handleState)
	mux.HandleFunc("GET /api/schema", h.handleSchema)
	mux.HandleFunc("GET /api/events", h.handleEvents)
	mux.HandleFunc("GET /ws", h.handleWS)

	// Securities.
	mux.HandleFunc("GET /api/securities", h.listSecurities)
	mux.HandleFunc("POST /api/securities", h.createSecurity)
	mux.HandleFunc("GET /api/securities/{id}", h.getSecurity)
	mux.HandleFunc("PUT /api/securities/{id}", h.updateSecurity)
	mux.HandleFunc("DELETE /api/securities/{id}", h.deleteSecurity)
	mux.HandleFunc("GET /api/securities/{id}/ticks", h.securityTicks)

	// Portfolios.
	mux.HandleFunc("GET /api/portfolios", h.listPortfolios)
	mux.HandleFunc("POST /api/portfolios", h.createPortfolio)
	mux.HandleFunc("GET /api/portfolios/{id}", h.getPortfolio)
	mux.HandleFunc("PUT /api/portfolios/{id}", h.updatePortfolio)
	mux.HandleFunc("DELETE /api/portfolios/{id}", h.deletePortfolio)
	mux.HandleFunc("GET /api/portfolios/{id}/holdings", h.portfolioHoldings)

	// Orders.
	mux.HandleFunc("GET /api/orders", h.listOrders)
	mux.HandleFunc("POST /api/orders", h.createOrder)
	mux.HandleFunc("GET /api/orders/{id}", h.getOrder)
	mux.HandleFunc("PUT /api/orders/{id}", h.updateOrder)
	mux.HandleFunc("DELETE /api/orders/{id}", h.deleteOrder)

	// Simulation controls.
	mux.HandleFunc("POST /api/control/pause", h.controlAction(func() { h.Engine.Pause() }))
	mux.HandleFunc("POST /api/control/resume", h.controlAction(func() { h.Engine.Resume() }))
	mux.HandleFunc("POST /api/control/level", h.handleLevel)
	mux.HandleFunc("POST /api/control/reset", h.controlActionCtx(h.Engine.Reset))
	mux.HandleFunc("POST /api/control/seed", h.controlActionCtx(h.Engine.Reseed))
	mux.HandleFunc("POST /api/control/wipe", h.handleWipe)
	mux.HandleFunc("POST /api/control/drop", h.handleDrop)

	// Report. /report is a clean URL for report.html, which the file server
	// below would otherwise only expose under its full filename.
	mux.HandleFunc("GET /report", h.handleReportPage)
	mux.HandleFunc("GET /api/report.json", h.handleReportJSON)
	mux.HandleFunc("GET /api/report.csv", h.handleReportCSV)

	mux.Handle("GET /", http.FileServerFS(h.Web))
	return corsMiddleware(mux)
}

// corsMiddleware allows cross-origin calls from any origin.
//
// This exists for exactly one caller: the dbcanvas control plane's own node
// properties panel, which is served from a different port and needs to call
// POST /api/control/drop when a user chooses to delete a Stock Market Sim node
// along with its data (see StockSimManager in app/web/src/pages/StackDesigner.jsx).
// Without it the browser's preflight fails and the checkbox silently does
// nothing.
//
// The permissiveness is bounded by where this app can run: a per-stack Docker
// bridge network, with its dashboard published to the host for a single
// operator. It has no authentication of its own to bypass and holds no
// credentials — it is a lab tool, and treating it as one is deliberate rather
// than an oversight.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleHealthz is the target of the distroless -healthcheck exec (there is no
// shell inside the container to curl with). Reports unhealthy only if the
// database itself is unreachable — never on seed progress, so dbcanvas marks
// the node running as soon as it can talk to its target.
func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.Ping(r.Context()); err != nil {
		http.Error(w, h.Store.Engine()+" unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.Engine.BuildSnapshot(r.Context()))
}

func (h *Handler) handleSchema(w http.ResponseWriter, r *http.Request) {
	objs, err := h.Store.Objects(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if objs == nil {
		objs = []store.ObjectInfo{}
	}
	writeJSON(w, map[string]any{
		"engine":   h.Store.Engine(),
		"database": h.Store.Database(),
		"objects":  objs,
	})
}

func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request) {
	evs, err := h.Store.EventsSince(r.Context(),
		r.URL.Query().Get("after"), intParam(r, "limit", 50))
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if evs == nil {
		evs = []store.Event{}
	}
	writeJSON(w, map[string]any{"events": evs})
}

// ------------------------------------------------------------- controls

func (h *Handler) controlAction(fn func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fn()
		writeJSON(w, map[string]string{"status": "ok"})
	}
}

// controlActionCtx is the fallible variant: the action can fail, and the
// failure is reported rather than swallowed. Note it hands the action
// r.Context() only for the duration of the call — the engine deliberately
// restarts its agents from its own long-lived context, never this one.
func (h *Handler) controlActionCtx(fn func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	}
}

func (h *Handler) handleLevel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if !sim.ValidLevel(body.Level) {
		http.Error(w, "level must be one of low, medium, high", http.StatusBadRequest)
		return
	}
	h.Engine.SetLevel(body.Level)
	writeJSON(w, map[string]string{"status": "ok", "level": body.Level})
}

func (h *Handler) handleWipe(w http.ResponseWriter, r *http.Request) {
	if err := h.Engine.Wipe(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleDrop removes every object this app created. It requires the caller to
// name the database in the request body — a deliberate speed bump, because a
// Stock Market Sim node can be pointed at a database outside the stack that
// dbcanvas did not create and may not exclusively own.
func (h *Handler) handleDrop(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm string `json:"confirm"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if strings.TrimSpace(body.Confirm) != h.Store.Database() {
		http.Error(w,
			"confirmation required: send {\"confirm\":\""+h.Store.Database()+"\"} to drop this application's objects",
			http.StatusBadRequest)
		return
	}
	if err := h.Engine.Drop(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "dropped": h.Store.Database()})
}

// ------------------------------------------------------------- utilities

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// writeStoreErr maps the store's two sentinel errors onto status codes so
// every handler treats a missing row and a duplicate key the same way.
func writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func intParam(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func listQuery(r *http.Request) store.ListQuery {
	return store.ListQuery{
		Search: r.URL.Query().Get("search"),
		Filter: r.URL.Query().Get("filter"),
		Limit:  intParam(r, "limit", 50),
		Offset: intParam(r, "offset", 0),
	}
}
