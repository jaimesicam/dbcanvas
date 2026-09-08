// Command airlinesim runs the MySQL Airline Reservation Lab: ten background agents
// simulate a 200-route reservation workload against a 2000-aircraft fleet, reading
// and writing whichever MySQL-family target (standalone Percona Server, MySQL
// replication, PXC, or either behind HAProxy/ProxySQL) it was deployed against, plus
// a web server exposing a live dashboard of the result. See app/airlinesim.go for
// how dbcanvas resolves and wires the target.
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

	"airlinesim/internal/api"
	"airlinesim/internal/sim"
	"airlinesim/internal/store"
)

//go:embed web/static
var webDist embed.FS

func main() {
	// The runtime image is distroless (no shell, no curl) — dbcanvas's own
	// provisioning waits for readiness the same way it does for every other
	// container node type, `Exec`ing a check *inside* the container. With no shell
	// available, that check is this flag: exec the binary itself.
	for _, arg := range os.Args[1:] {
		if arg == "-healthcheck" {
			healthcheck()
			return
		}
	}

	dsn := envOr("MYSQL_DSN", "app:app_password@tcp(127.0.0.1:3306)/")
	dbName := envOr("MYSQL_DB", "airlinesim")
	kind := sim.TargetKind(envOr("TARGET_KIND", string(sim.TargetPS)))
	targetLabel := envOr("TARGET_LABEL", "")
	port := envOr("PORT", "8090")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Connect(dsn)
	if err != nil {
		log.Fatalf("airlinesim: connect: %v", err)
	}
	waitForMySQL(ctx, st)

	if err := st.EnsureDatabase(ctx, dbName); err != nil {
		log.Fatalf("airlinesim: ensure database: %v", err)
	}
	if err := store.EnsureSchema(ctx, st); err != nil {
		log.Fatalf("airlinesim: schema setup: %v", err)
	}

	webFS, err := fs.Sub(webDist, "web/static")
	if err != nil {
		log.Fatalf("airlinesim: embedded web assets: %v", err)
	}

	engine := sim.NewEngine(st, kind, targetLabel)
	engine.Start(ctx)

	h := api.New(engine, st, webFS)
	srv := &http.Server{Addr: ":" + port, Handler: h.Routes()}

	go func() {
		<-ctx.Done()
		log.Printf("airlinesim: shutting down")
		engine.Stop()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	log.Printf("airlinesim: listening on :%s (mysql schema %s, kind %s)", port, dbName, kind)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("airlinesim: %v", err)
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
	log.Printf("airlinesim: mysql still unreachable after 60s, starting anyway")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// healthcheck exits 0 if the already-running server instance's /healthz answers 200,
// non-zero otherwise. See the -healthcheck branch in main above.
func healthcheck() {
	port := envOr("PORT", "8090")
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
