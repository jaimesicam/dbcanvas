// Command pagerpanic is Pager Panic: an on-call game played against a cluster on a
// DBCanvas canvas. This server keeps the game and serves its page. It never touches the
// cluster: DBCanvas polls /api/dbcanvas/sync, runs what the game asks for on the
// cluster's nodes, and reports back.
package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"pagerpanic/internal/game"
)

//go:embed web/static
var webDist embed.FS

func main() {
	for _, arg := range os.Args[1:] {
		// The image is distroless, so DBCanvas checks readiness by running the binary.
		if arg == "-healthcheck" || arg == "--healthcheck" {
			healthcheck()
			return
		}
	}
	port := envOr("PORT", "8097")
	// Only DBCanvas knows the token, so a browser on the game's page cannot post results.
	token := os.Getenv("PAGERPANIC_TOKEN")
	g := game.New(nil)
	go func() {
		for range time.Tick(250 * time.Millisecond) {
			g.Tick()
		}
	}()

	mux := http.NewServeMux()
	static, _ := fs.Sub(webDist, "web/static")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("POST /api/dbcanvas/sync", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Pager-Panic-Token")), []byte(token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var req game.SyncRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, g.Sync(req))
	})

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.Atoi(r.URL.Query().Get("since"))
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, g.View(since))
	})

	mux.HandleFunc("POST /api/play", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Do     string `json:"do"`
			Mode   string `json:"mode"`
			Task   string `json:"task"`
			Ticket string `json:"ticket"`
			Name   string `json:"name"`
			Seed   int64  `json:"seed"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		var err error
		out := map[string]any{"ok": true}
		switch body.Do {
		case "check-system":
			err = g.CheckSystem()
		case "begin":
			err = g.Begin(body.Mode, body.Task, body.Seed)
		case "pause":
			err = g.Pause()
		case "resume":
			err = g.Resume()
		case "hint":
			var text string
			text, err = g.Hint(body.Ticket)
			out["hint"] = text
		case "give-up":
			err = g.GiveUp(body.Ticket)
		case "check-now":
			err = g.CheckNow(body.Ticket)
		case "quit":
			err = g.Quit()
		case "title":
			err = g.Title()
		case "initials":
			err = g.Initials(body.Name)
		default:
			err = fmt.Errorf("unknown move %q", body.Do)
		}
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, out)
	})

	log.Printf("pagerpanic: listening on :%s", port)
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func healthcheck() {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + envOr("PORT", "8097") + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		os.Exit(1)
	}
	os.Exit(0)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
