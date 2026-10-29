package core

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"time"
)

// State is what the browser polls once a second. Points after since are sent,
// so the first poll (since=0) fills the timeline and later ones add one second.
type State struct {
	Ready     bool                `json:"ready"`
	InitErr   string              `json:"initErr,omitempty"`
	Now       int64               `json:"now"`
	Info      TargetInfo          `json:"info"`
	Threads   int                 `json:"threads"`
	Members   []Member            `json:"members"`
	Points    []Point             `json:"points"`
	Bands     []Band              `json:"bands"`
	Events    []Event             `json:"events"`
	Active    []ActiveState       `json:"active"`
	Baseline  *Baseline           `json:"baseline"`
	Datasets  []Dataset           `json:"datasets"`
	Prep      *prepState          `json:"prep"`
	Recording *int64              `json:"recording"`
	Runs      []RunSummary        `json:"runs"`
	Dials     []Dial              `json:"dials"`
	Params    map[string]Params   `json:"params"`
	Targets   map[string][]string `json:"targets"`
}

type ActiveState struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Targets []string  `json:"targets"`
	Start   int64     `json:"start"`
	Hold    int       `json:"hold"`
	Params  Params    `json:"params"`
	Status  RunStatus `json:"status"`
	Color   int       `json:"color"`
	Seq     int       `json:"seq"`
}

func (h *Hub) State(since int64) State {
	h.mu.Lock()
	defer h.mu.Unlock()
	info, dials := h.info, h.dials
	if h.ready {
		// Both are cheap and change with the topology (a failover moves the writer).
		info, dials = h.eng.Info(), h.allDials()
	}
	s := State{Ready: h.ready, InitErr: h.initErr, Now: time.Now().Unix(), Info: info,
		Threads: h.settings.Threads, Members: h.members, Baseline: h.baseline, Datasets: h.datasets,
		Prep: h.prep, Runs: h.runs, Dials: dials, Params: h.settings.Params, Targets: h.settings.Targets,
		Bands: append([]Band(nil), h.bands...)}
	for _, p := range h.points {
		if p.T > since {
			s.Points = append(s.Points, p)
		}
	}
	ev := h.events
	if len(ev) > 80 {
		ev = ev[len(ev)-80:]
	}
	s.Events = append([]Event(nil), ev...)
	for _, a := range h.active {
		s.Active = append(s.Active, ActiveState{ID: a.dial.ID, Label: a.label, Targets: a.targets, Start: a.start.Unix(),
			Hold: a.hold, Params: a.params, Status: a.run.Status(), Color: a.band.Color, Seq: a.band.Seq})
	}
	if h.recording != nil {
		t := h.recording.start.Unix()
		s.Recording = &t
	}
	return s
}

// Routes is the sim's HTTP API and its embedded dashboard.
func (h *Hub) Routes(static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		writeJSON(w, h.State(since))
	})
	mux.HandleFunc("POST /api/dials/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Params  Params   `json:"params"`
			Targets []string `json:"targets"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			httpErr(w, 400, "invalid request body")
			return
		}
		if err := h.Start(r.PathValue("id"), in.Params, in.Targets); err != nil {
			httpErr(w, 409, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/dials/{id}/params", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Params Params `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			httpErr(w, 400, "invalid request body")
			return
		}
		if err := h.Update(r.PathValue("id"), in.Params); err != nil {
			httpErr(w, 409, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/dials/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Stop(r.PathValue("id"), "user"); err != nil {
			httpErr(w, 409, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/stop-all", func(w http.ResponseWriter, r *http.Request) {
		n := h.StopAll("all")
		if n > 0 {
			h.event("warn", "Stop all: every stressor off, every lock and pause released")
		}
		writeJSON(w, map[string]int{"stopped": n})
	})
	mux.HandleFunc("POST /api/threads", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Threads int `json:"threads"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			httpErr(w, 400, "invalid request body")
			return
		}
		h.SetThreads(in.Threads)
		writeJSON(w, map[string]int{"threads": h.work.Threads()})
	})
	mux.HandleFunc("POST /api/baseline", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, h.SetBaseline()) })
	mux.HandleFunc("POST /api/datasets/prepare", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			GB float64 `json:"gb"`
			PK bool    `json:"pk"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			httpErr(w, 400, "invalid request body")
			return
		}
		if err := h.Prepare(in.GB, in.PK); err != nil {
			httpErr(w, 409, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/datasets/cancel", func(w http.ResponseWriter, r *http.Request) {
		h.CancelPrepare()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/datasets/{name}/drop", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Drop(r.PathValue("name")); err != nil {
			httpErr(w, 409, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		ss, err := h.Sessions(ctx)
		if err != nil {
			httpErr(w, 502, err.Error())
			return
		}
		if ss == nil {
			ss = []Session{}
		}
		writeJSON(w, ss)
	})
	mux.HandleFunc("POST /api/record/start", func(w http.ResponseWriter, r *http.Request) {
		if err := h.RecordStart(); err != nil {
			httpErr(w, 409, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/record/stop", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		s, err := h.RecordStop(in.Name)
		if err != nil {
			httpErr(w, 409, err.Error())
			return
		}
		writeJSON(w, s)
	})
	mux.HandleFunc("GET /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		b, err := h.RunJSON(r.Context(), r.PathValue("id"))
		if err != nil {
			httpErr(w, 404, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("download") != "" {
			w.Header().Set("Content-Disposition", `attachment; filename="perfsim-run-`+r.PathValue("id")+`.json"`)
		}
		w.Write(b)
	})
	mux.HandleFunc("DELETE /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := h.DeleteRun(r.Context(), r.PathValue("id")); err != nil {
			httpErr(w, 409, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/conditions/sync", h.cond.syncHandler)
	mux.Handle("GET /", http.FileServerFS(static))
	return noCache(mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// noCache keeps a browser from showing a dashboard from before a redeploy.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
