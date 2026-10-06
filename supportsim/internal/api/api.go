// Package api is the dashboard's HTTP surface: a JSON API, a Server-Sent Events
// stream of the desk's activity, and the embedded static frontend.
package api

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"supportsim/internal/corpus"
	"supportsim/internal/search"
	"supportsim/internal/sim"
)

// API serves the dashboard.
type API struct {
	E   *sim.Engine
	Web fs.FS
}

// Routes returns the handler.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("GET /api/events", a.events)
	mux.HandleFunc("POST /api/controls", a.controls)
	mux.HandleFunc("POST /api/outage", a.outage)
	mux.HandleFunc("POST /api/showdown", a.showdown)
	mux.HandleFunc("POST /api/embed", a.embed)
	mux.HandleFunc("POST /api/similarity", a.similarity)
	mux.HandleFunc("GET /api/map", a.mapAll)
	mux.HandleFunc("POST /api/map/locate", a.locate)
	mux.HandleFunc("POST /api/builder", a.builder)
	mux.HandleFunc("POST /api/freshness", a.freshness)
	mux.HandleFunc("GET /api/ticket", a.ticket)
	mux.HandleFunc("GET /api/article", a.article)
	mux.HandleFunc("POST /api/hybrid/tune", a.hybridTune)
	mux.HandleFunc("POST /api/hybrid/apply", a.hybridApply)
	mux.HandleFunc("GET /api/workshop", a.workshop)
	mux.HandleFunc("POST /api/workshop/build", a.workshopBuild)
	mux.HandleFunc("POST /api/workshop/bench", a.workshopBench)
	mux.HandleFunc("POST /api/workshop/explain", a.workshopExplain)
	mux.HandleFunc("GET /api/workshop/metrics", a.workshopMetrics)
	mux.HandleFunc("GET /api/workshop/playground", a.playground)
	mux.HandleFunc("POST /api/workshop/playground", a.playgroundDo)
	mux.Handle("GET /", http.FileServerFS(a.Web))
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) bool {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(v) == nil
}

func (a *API) state(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"status":    a.E.Status(),
		"metrics":   a.E.Stats.Snapshot(),
		"recent":    a.E.Recent(40),
		"incidents": a.E.Incidents(),
		"catalog":   sim.Catalog(),
	})
}

// events streams the desk as Server-Sent Events. A comment line every 15 s keeps
// proxies (and DBCanvas's own browse-through) from timing the stream out.
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, stop := a.E.Bus.Subscribe()
	defer stop()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	w.Write([]byte(": hello\n\n"))
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			w.Write([]byte(": ping\n\n"))
			fl.Flush()
		case msg := <-ch:
			w.Write([]byte("data: "))
			w.Write(msg)
			w.Write([]byte("\n\n"))
			fl.Flush()
		}
	}
}

func (a *API) controls(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paused    *bool    `json:"paused"`
		PerMinute *float64 `json:"perMinute"`
	}
	if !readJSON(r, &req) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	a.E.SetControls(req.Paused, req.PerMinute)
	writeJSON(w, a.E.Status())
}

func (a *API) outage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	readJSON(r, &req)
	writeJSON(w, map[string]string{"outage": a.E.TriggerOutage(req.ID)})
}

func (a *API) showdown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query      string             `json:"query"`
		Collection string             `json:"collection"`
		K          int                `json:"k"`
		Filter     search.Filter      `json:"filter"`
		Hybrid     *search.HybridOpts `json:"hybrid"`
	}
	if !readJSON(r, &req) || strings.TrimSpace(req.Query) == "" {
		http.Error(w, "query required", http.StatusBadRequest)
		return
	}
	writeJSON(w, a.E.RunShowdown(r.Context(), strings.TrimSpace(req.Query), req.Collection, req.K, req.Filter, req.Hybrid))
}

func (a *API) embed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !readJSON(r, &req) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	writeJSON(w, a.E.Embed(req.Text))
}

func (a *API) similarity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		A string `json:"a"`
		B string `json:"b"`
	}
	if !readJSON(r, &req) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	writeJSON(w, a.E.Similarity(req.A, req.B))
}

func (a *API) mapAll(w http.ResponseWriter, _ *http.Request) { writeJSON(w, a.E.Map()) }

func (a *API) locate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !readJSON(r, &req) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	writeJSON(w, a.E.Locate(req.Text))
}

func (a *API) builder(w http.ResponseWriter, r *http.Request) {
	var req sim.BuilderRequest
	if !readJSON(r, &req) || strings.TrimSpace(req.Query) == "" {
		http.Error(w, "query required", http.StatusBadRequest)
		return
	}
	writeJSON(w, a.E.Build(r.Context(), req))
}

func (a *API) freshness(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.E.Freshness(r.Context()))
}

func (a *API) ticket(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	d := a.E.Decision(id)
	if d == nil {
		http.Error(w, "not in the recent window", http.StatusNotFound)
		return
	}
	writeJSON(w, d)
}

func (a *API) article(w http.ResponseWriter, r *http.Request) {
	art, ok := corpus.ArticleBySlug(r.URL.Query().Get("slug"))
	if !ok {
		http.Error(w, "no such article", http.StatusNotFound)
		return
	}
	writeJSON(w, art)
}

func (a *API) hybridTune(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Collection    string `json:"collection"`
		Method        string `json:"method"`
		Normalization string `json:"normalization"`
		Queries       int    `json:"queries"`
	}
	readJSON(r, &req)
	writeJSON(w, a.E.Tune(r.Context(), req.Collection, req.Method, req.Normalization, req.Queries))
}

func (a *API) hybridApply(w http.ResponseWriter, r *http.Request) {
	var o search.HybridOpts
	if !readJSON(r, &o) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	a.E.SetHybrid(o)
	writeJSON(w, a.E.Status())
}

func (a *API) workshop(w http.ResponseWriter, _ *http.Request) {
	type v struct {
		sim.Variant
		Shell string `json:"shell"`
	}
	var vs []v
	for _, x := range a.E.B.Workshop().Variants() {
		vs = append(vs, v{x, x.Shell()})
	}
	writeJSON(w, map[string]any{"state": a.E.WorkshopStatus(), "variants": vs})
}

func (a *API) workshopBuild(w http.ResponseWriter, _ *http.Request) {
	if err := a.E.BuildVariants(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, a.E.WorkshopStatus())
}

func (a *API) workshopBench(w http.ResponseWriter, r *http.Request) {
	var p sim.BenchParams
	readJSON(r, &p)
	res, err := a.E.Bench(r.Context(), p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, res)
}

func (a *API) workshopExplain(w http.ResponseWriter, r *http.Request) {
	var req sim.ExplainRequest
	if !readJSON(r, &req) || strings.TrimSpace(req.Query) == "" {
		http.Error(w, "query required", http.StatusBadRequest)
		return
	}
	writeJSON(w, a.E.Explain(r.Context(), req))
}

func (a *API) workshopMetrics(w http.ResponseWriter, r *http.Request) {
	v := a.E.Metrics(r.Context())
	if r.URL.Query().Get("raw") == "" {
		for i := range v.Mongots {
			v.Mongots[i].Raw = nil
		}
	}
	writeJSON(w, v)
}

func (a *API) playground(w http.ResponseWriter, _ *http.Request) { writeJSON(w, a.E.Playground()) }

func (a *API) playgroundDo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	readJSON(r, &req)
	if err := a.E.PlaygroundDo(r.Context(), req.Action); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, a.E.Playground())
}
