// Package api is the web server: REST endpoints for every UI panel plus the
// WebSocket live-event bridge. Nothing here touches MySQL directly except through
// Engine/Store — the API layer is presentation only.
package api

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"

	"airlinesim/internal/sim"
	"airlinesim/internal/store"
)

type Handler struct {
	Engine *sim.Engine
	Store  *store.Store
	Web    fs.FS
}

func New(e *sim.Engine, st *store.Store, webFS fs.FS) *Handler {
	return &Handler{Engine: e, Store: st, Web: webFS}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /api/state", h.handleState)
	mux.HandleFunc("GET /api/routes/{id}", h.handleRouteDetail)
	mux.HandleFunc("GET /api/aircraft", h.handleAircraft)
	mux.HandleFunc("GET /api/reservations/{id}", h.handleReservationDetail)
	mux.HandleFunc("GET /api/events", h.handleEvents)
	mux.HandleFunc("GET /api/queries", h.handleQueries)
	mux.HandleFunc("GET /ws", h.handleWS)
	mux.HandleFunc("POST /api/control/level", h.handleLevel)
	mux.HandleFunc("POST /api/control/pause", h.controlAction(func() { h.Engine.Pause() }))
	mux.HandleFunc("POST /api/control/resume", h.controlAction(func() { h.Engine.Resume() }))
	mux.HandleFunc("POST /api/control/reset", h.controlAction(func() { h.Engine.Reset() }))
	mux.Handle("GET /", http.FileServerFS(h.Web))
	return mux
}

// handleHealthz is the target of the distroless -healthcheck exec (dbcanvas has no
// shell to curl with inside the container). Reports unhealthy if MySQL itself is
// unreachable — this is also how the web interface's own "connection problem"
// banner ultimately gets its signal, via GET /api/state's error field using the
// same Ping.
func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.Ping(r.Context()); err != nil {
		http.Error(w, "mysql unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleState(w http.ResponseWriter, r *http.Request) {
	snap := h.Engine.BuildSnapshot(r.Context())
	writeJSON(w, snap)
}

func (h *Handler) handleRouteDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, ok := h.Engine.RouteDetail(r.Context(), id)
	if !ok {
		http.Error(w, "route not found", http.StatusNotFound)
		return
	}
	writeJSON(w, detail)
}

func (h *Handler) handleAircraft(w http.ResponseWriter, r *http.Request) {
	limit, offset := 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	rows, total, err := h.Engine.AircraftPage(r.Context(), r.URL.Query().Get("search"), r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"aircraft": rows, "total": total, "limit": limit, "offset": offset})
}

func (h *Handler) handleReservationDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	routeID := r.URL.Query().Get("routeId")
	flightDate := r.URL.Query().Get("flightDate")
	detail, ok := h.Engine.ReservationDetail(r.Context(), id, routeID, flightDate)
	if !ok {
		http.Error(w, "reservation not found", http.StatusNotFound)
		return
	}
	writeJSON(w, detail)
}

func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	events, _ := h.Store.RecentEvents(r.Context(), limit)
	writeJSON(w, map[string]any{"events": events})
}

func (h *Handler) handleQueries(w http.ResponseWriter, r *http.Request) {
	limit := 25
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	samples, _ := h.Store.RecentQuerySamples(r.Context(), limit)
	writeJSON(w, map[string]any{"samples": samples})
}

func (h *Handler) controlAction(fn func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fn()
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
	switch sim.LoadLevel(body.Level) {
	case sim.LevelStop, sim.LevelLow, sim.LevelMedium, sim.LevelHigh:
		h.Engine.SetLevel(sim.LoadLevel(body.Level))
		writeJSON(w, map[string]string{"status": "ok", "level": body.Level})
	default:
		http.Error(w, "level must be one of stop|low|medium|high", http.StatusBadRequest)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
