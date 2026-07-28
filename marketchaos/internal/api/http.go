// Package api is the web server: REST endpoints for every UI panel plus the
// WebSocket live-event bridge. Nothing here touches MySQL directly except
// through Engine/Store — the API layer is presentation only.
package api

import (
	"encoding/json"
	"io/fs"
	"net/http"

	"marketchaos/internal/sim"
	"marketchaos/internal/store"
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
	mux.HandleFunc("POST /api/control/level", h.handleLevel)
	mux.HandleFunc("POST /api/control/mix", h.handleMix)
	mux.HandleFunc("POST /api/control/pause", h.controlAction(func() { h.Engine.Pause() }))
	mux.HandleFunc("POST /api/control/resume", h.controlAction(func() { h.Engine.Resume() }))
	mux.HandleFunc("POST /api/control/reset", h.controlActionCtx(func(r *http.Request) error { return h.Engine.Reset(r.Context()) }))
	mux.HandleFunc("GET /api/diag/leaderboard", h.handleLeaderboard)
	mux.HandleFunc("GET /api/diag/serverstats", h.handleServerStats)
	mux.HandleFunc("GET /api/diag/wsrep", h.handleWsrep)
	mux.HandleFunc("GET /api/diag/haproxy", h.handleHAProxy)
	mux.HandleFunc("GET /api/diag/processlist", h.handleProcesslist)
	mux.HandleFunc("GET /api/diag/locks", h.handleLockWaits)
	mux.HandleFunc("GET /api/diag/tablesizes", h.handleTableSizes)
	mux.HandleFunc("POST /api/diag/explain", h.handleExplain)
	mux.Handle("GET /", http.FileServerFS(h.Web))
	return mux
}

// handleHealthz is the target of the distroless -healthcheck exec (dbcanvas
// has no shell to curl with inside the container). Reports unhealthy if
// MySQL itself is unreachable — this is also how the web interface's own
// "connection problem" banner ultimately gets its signal, via GET
// /api/state's error field using the same Ping.
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

func (h *Handler) controlAction(fn func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fn()
		writeJSON(w, map[string]string{"status": "ok"})
	}
}

func (h *Handler) controlActionCtx(fn func(*http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(r); err != nil {
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
	switch sim.LoadLevel(body.Level) {
	case sim.LevelStop, sim.LevelLow, sim.LevelMedium, sim.LevelHigh, sim.LevelExtrm, sim.LevelCustom:
		h.Engine.SetLevel(sim.LoadLevel(body.Level))
		writeJSON(w, map[string]string{"status": "ok", "level": body.Level})
	default:
		http.Error(w, "level must be one of stop|low|medium|high|extreme|custom", http.StatusBadRequest)
	}
}

func (h *Handler) handleMix(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mix string `json:"mix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	switch sim.WorkloadMix(body.Mix) {
	case sim.MixBalanced, sim.MixReadHeavy, sim.MixWriteHeavy, sim.MixAnalyticsHeavy, sim.MixContentionHeavy, sim.MixPXCConflictHeavy:
		h.Engine.SetMix(sim.WorkloadMix(body.Mix))
		writeJSON(w, map[string]string{"status": "ok", "mix": body.Mix})
	default:
		http.Error(w, "mix must be one of balanced|read-heavy|write-heavy|analytics-heavy|contention-heavy|pxc-conflict-heavy", http.StatusBadRequest)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
