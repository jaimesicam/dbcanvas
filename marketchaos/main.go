// Command marketchaos runs the "Unoptimized MySQL Challenge" — a fictional
// stock-exchange simulator (product name MarketChaos) deployed with
// deliberately bad indexes, queries, and transaction patterns so a learner
// can diagnose and fix them. It reads and writes whichever MySQL-family
// target (standalone Percona Server, a direct PXC member, a PXC cluster, a
// MySQL replication frame, or either behind HAProxy) it was deployed
// against, plus a web server exposing a live operations-style dashboard.
// See app/marketchaos.go for how dbcanvas resolves and wires the target.
//
// This is stage S0 of the implementation plan: module skeleton, schema
// bootstrap, and an empty dashboard proving the node deploys and reports
// healthy against every connection-target shape. The market/challenge
// engine (stages S1+) is not implemented yet.
package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"marketchaos/internal/api"
	"marketchaos/internal/sim"
	"marketchaos/internal/store"
)

//go:embed web/static
var webDist embed.FS

func main() {
	// The runtime image is distroless (no shell, no curl) — dbcanvas's own
	// provisioning waits for readiness the same way it does for every other
	// container node type, `Exec`ing a check *inside* the container. With no
	// shell available, that check is this flag: exec the binary itself.
	for _, arg := range os.Args[1:] {
		if arg == "-healthcheck" {
			healthcheck()
			return
		}
	}

	dsn := envOr("MYSQL_DSN", "app:app_password@tcp(127.0.0.1:3306)/")
	dbName := envOr("MYSQL_DB", "marketchaos")
	kind := sim.TargetKind(envOr("TARGET_KIND", string(sim.TargetPS)))
	targetLabel := envOr("TARGET_LABEL", "")
	port := envOr("PORT", "8092")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Connect(dsn)
	if err != nil {
		log.Fatalf("marketchaos: connect: %v", err)
	}
	waitForMySQL(ctx, st)

	if err := st.EnsureDatabase(ctx, dbName); err != nil {
		log.Fatalf("marketchaos: ensure database: %v", err)
	}
	if err := store.EnsureSchema(ctx, st); err != nil {
		log.Fatalf("marketchaos: schema setup: %v", err)
	}

	webFS, err := fs.Sub(webDist, "web/static")
	if err != nil {
		log.Fatalf("marketchaos: embedded web assets: %v", err)
	}

	engine := sim.NewEngine(st, kind, targetLabel)
	engine.Start(ctx)

	h := api.New(engine, st, webFS)
	srv := &http.Server{Addr: ":" + port, Handler: h.Routes()}

	go func() {
		<-ctx.Done()
		log.Printf("marketchaos: shutting down")
		engine.Stop()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	log.Printf("marketchaos: listening on :%s (mysql schema %s, kind %s)", port, dbName, kind)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("marketchaos: %v", err)
	}
}

func waitForMySQL(ctx context.Context, st *store.Store) {
	for i := 0; i < 60; i++ {
		if err := st.Ping(ctx); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	log.Printf("marketchaos: mysql still unreachable after 60s, starting anyway")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// healthcheck exits 0 if the already-running server instance's /healthz
// answers 200, non-zero otherwise. See the -healthcheck branch in main above.
func healthcheck() {
	port := envOr("PORT", "8092")
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}
