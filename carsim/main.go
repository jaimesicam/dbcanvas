// Command carsim runs the PostgreSQL Car Rental Lab: ten background agents
// simulate a 180-location rental workload against a 2000-vehicle fleet, reading
// and writing whichever PostgreSQL-family target (standalone, Patroni, repmgr,
// or Spock, direct or behind HAProxy) it was deployed against, plus a web server
// exposing a live dashboard of the result. See app/carsim.go for how dbcanvas
// resolves and wires the target.
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

	"carsim/internal/api"
	"carsim/internal/sim"
	"carsim/internal/store"
)

//go:embed web/static
var webDist embed.FS

// spockDatabase is the fixed database Spock's cluster setup already created and
// wired into its replication set (see app/spock.go) — this app must land in
// THAT database on a Spock target, never one of its own, since Spock's logical
// replication is scoped per-database and per-replication-set, not automatic
// across every database on the server.
const spockDatabase = "spockdemo"

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

	dsn := envOr("POSTGRES_DSN", "postgres://postgres:postgres_password@127.0.0.1:5432/postgres?sslmode=prefer&connect_timeout=10")
	kind := sim.TargetKind(envOr("TARGET_KIND", string(sim.TargetPG)))
	targetLabel := envOr("TARGET_LABEL", "")
	port := envOr("PORT", "8091")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Connect(dsn)
	if err != nil {
		log.Fatalf("carsim: connect: %v", err)
	}
	waitForPostgres(ctx, st)

	isSpock := kind.Family() == "spock"
	dbName := "carsim"
	if isSpock {
		dbName = spockDatabase
	}
	if err := st.EnsureDatabase(ctx, dbName, isSpock); err != nil {
		log.Fatalf("carsim: ensure database: %v", err)
	}
	if err := store.EnsureSchema(ctx, st); err != nil {
		log.Fatalf("carsim: schema setup: %v", err)
	}
	if isSpock {
		if err := st.RegisterSpockReplication(ctx); err != nil {
			log.Printf("carsim: register spock replication (continuing anyway): %v", err)
		}
	}

	webFS, err := fs.Sub(webDist, "web/static")
	if err != nil {
		log.Fatalf("carsim: embedded web assets: %v", err)
	}

	engine := sim.NewEngine(st, kind, targetLabel)
	engine.Start(ctx)

	h := api.New(engine, st, webFS)
	srv := &http.Server{Addr: ":" + port, Handler: h.Routes()}

	go func() {
		<-ctx.Done()
		log.Printf("carsim: shutting down")
		engine.Stop()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	log.Printf("carsim: listening on :%s (database %s, kind %s)", port, dbName, kind)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("carsim: %v", err)
	}
}

func waitForPostgres(ctx context.Context, st *store.Store) {
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
	log.Printf("carsim: postgresql still unreachable after 60s, starting anyway")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// healthcheck exits 0 if the already-running server instance's /healthz answers
// 200, non-zero otherwise. See the -healthcheck branch in main above.
func healthcheck() {
	port := envOr("PORT", "8091")
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
