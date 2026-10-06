// Command supportsim runs the MongoDB Vector Search Support Desk: a help desk for
// a fictional cloud-database company whose tickets are embedded, searched with
// $vectorSearch, and acted on — answered, routed, merged, escalated — while a
// keyword engine answers the same questions beside it, and a web dashboard shows
// all of it, the pipelines included. See README.md.
package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"supportsim/internal/api"
	embedmodel "supportsim/internal/embed"
	"supportsim/internal/search"
	"supportsim/internal/sim"
	"supportsim/internal/store"
)

//go:embed web/static
var webDist embed.FS

func main() {
	// The runtime image is distroless — no shell, no curl — so DBCanvas's readiness
	// check execs the binary itself, as it does for every other simulator.
	for _, arg := range os.Args[1:] {
		if arg == "-healthcheck" {
			healthcheck()
			return
		}
	}

	uri := envOr("MONGO_URI", "mongodb://127.0.0.1:27017/?directConnection=true")
	dbName := envOr("MONGO_DB", "supportsim")
	label := envOr("MONGO_TARGET_LABEL", "")
	port := envOr("PORT", "8095")
	modelDir := envOr("MODEL_DIR", embedmodel.DefaultDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	t0 := time.Now()
	model, err := embedmodel.Load(modelDir)
	if err != nil {
		log.Fatalf("supportsim: load model: %v", err)
	}
	log.Printf("supportsim: %s loaded from %s in %s", model.Name(), modelDir, time.Since(t0).Round(time.Millisecond))

	// The engine is MongoDB unless DBCanvas says otherwise: DB_ENGINE=postgres with a
	// POSTGRES_DSN runs the same desk on PostgreSQL with pgvector.
	mirror := search.NewMirror()
	var backend sim.Backend
	switch envOr("DB_ENGINE", "mongodb") {
	case "postgres":
		dsn := os.Getenv("POSTGRES_DSN")
		if dsn == "" {
			log.Fatalf("supportsim: DB_ENGINE=postgres needs POSTGRES_DSN")
		}
		b, err := sim.NewPGBackend(ctx, dsn, mirror)
		if err != nil {
			log.Fatalf("supportsim: %v", err)
		}
		backend = b
		uri = dsn
	default:
		st, err := store.Connect(ctx, uri, dbName)
		if err != nil {
			log.Fatalf("supportsim: %v", err)
		}
		for st.Ping(ctx) != nil && ctx.Err() == nil {
			log.Printf("supportsim: waiting for MongoDB at %s", redact(uri))
			time.Sleep(3 * time.Second)
		}
		backend = sim.NewMongoBackend(st, mirror)
	}

	web, err := fs.Sub(webDist, "web/static")
	if err != nil {
		log.Fatalf("supportsim: web assets: %v", err)
	}
	engine := sim.New(backend, mirror, model, label)
	go engine.Run(ctx)

	srv := &http.Server{Addr: ":" + port, Handler: (&api.API{E: engine, Web: web}).Routes()}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("supportsim: listening on :%s (%s %s)", port, backend.Engine(), redact(uri))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("supportsim: %v", err)
	}
}

func healthcheck() {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + envOr("PORT", "8095") + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var credRE = regexp.MustCompile(`//[^@/]*@`)

func redact(uri string) string { return credRE.ReplaceAllString(uri, "//***@") }
