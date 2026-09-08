// Package api exposes the trafficsim HTTP surface: the embedded static frontend, a
// full-state snapshot for load/reconnect recovery, a live-push WebSocket, and the
// simulation controls (start/pause/reset/incident/traffic-level).
package api

import (
	"encoding/json"
	"io/fs"
	"math/rand"
	"net/http"

	"trafficsim/internal/sim"
	"trafficsim/internal/store"
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
	mux.HandleFunc("GET /ws", h.handleWS)
	mux.HandleFunc("POST /api/control/start", h.controlAction(func() { h.Engine.Resume() }))
	mux.HandleFunc("POST /api/control/pause", h.controlAction(func() { h.Engine.Pause() }))
	mux.HandleFunc("POST /api/control/resume", h.controlAction(func() { h.Engine.Resume() }))
	mux.HandleFunc("POST /api/control/reset", h.handleReset)
	mux.HandleFunc("POST /api/control/traffic-level", h.handleTrafficLevel)
	mux.HandleFunc("POST /api/control/incident", h.handleCreateIncident)
	mux.HandleFunc("POST /api/control/incident/clear", h.handleClearIncident)
	mux.Handle("GET /", http.FileServerFS(h.Web))
	return mux
}

func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.Ping(r.Context()); err != nil {
		http.Error(w, "valkey unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleState(w http.ResponseWriter, r *http.Request) {
	snap := h.Engine.BuildSnapshot(r.Context())
	writeJSON(w, snap)
}

func (h *Handler) controlAction(fn func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fn()
		writeJSON(w, map[string]string{"status": "ok"})
	}
}

func (h *Handler) handleReset(w http.ResponseWriter, r *http.Request) {
	h.Engine.Reset()
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handler) handleTrafficLevel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	switch sim.TrafficLevel(body.Level) {
	case sim.LevelOff, sim.LevelLow, sim.LevelMedium, sim.LevelHigh:
		h.Engine.SetLevel(sim.TrafficLevel(body.Level))
		writeJSON(w, map[string]string{"status": "ok", "level": body.Level})
	default:
		http.Error(w, "level must be one of off|low|medium|high", http.StatusBadRequest)
	}
}

func (h *Handler) handleCreateIncident(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type     string `json:"type"`
		RoadID   string `json:"roadId"`
		Severity string `json:"severity"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if body.RoadID == "" {
		roads := h.Engine.Map.Roads
		body.RoadID = roads[rand.Intn(len(roads))].ID
	}
	if body.Type == "" {
		body.Type = "accident"
	}
	inc := h.Engine.CreateIncident(body.Type, body.RoadID, body.Severity)
	if inc == nil {
		http.Error(w, "unknown roadId", http.StatusBadRequest)
		return
	}
	writeJSON(w, inc)
}

func (h *Handler) handleClearIncident(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if !h.Engine.ClearIncident(body.ID) {
		http.Error(w, "unknown incident id", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
