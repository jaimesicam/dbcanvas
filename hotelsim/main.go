// Command hotelsim runs the MongoDB Hotel Reservation Lab: a small set of background
// agents that simulate a 100-hotel reservation chain and continuously read/write
// MongoDB, plus a web server exposing a live view of the result. See README.md.
package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hotelsim/internal/api"
	"hotelsim/internal/sim"
	"hotelsim/internal/store"
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

	uri := envOr("MONGO_URI", "mongodb://127.0.0.1:27017/?directConnection=true")
	dbName := envOr("MONGO_DB", "hotelsim")
	targetLabel := envOr("MONGO_TARGET_LABEL", "")
	port := envOr("PORT", "8089")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Connect(ctx, uri, dbName)
	if err != nil {
		log.Fatalf("hotelsim: connect: %v", err)
	}
	waitForMongo(ctx, st)

	topo, err := detectTopologyWithRetry(ctx, st)
	if err != nil {
		log.Fatalf("hotelsim: %v", err)
	}
	if err := store.EnsureSchema(ctx, st, topo); err != nil {
		log.Fatalf("hotelsim: schema setup: %v", err)
	}

	webFS, err := fs.Sub(webDist, "web/static")
	if err != nil {
		log.Fatalf("hotelsim: embedded web assets: %v", err)
	}

	engine := sim.NewEngine(st, topo, targetLabel)
	engine.Start(ctx)

	h := api.New(engine, st, webFS)
	srv := &http.Server{Addr: ":" + port, Handler: h.Routes()}

	go func() {
		<-ctx.Done()
		log.Printf("hotelsim: shutting down")
		engine.Stop()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	log.Printf("hotelsim: listening on :%s (mongo %s, db %s)", port, redactURI(uri), dbName)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("hotelsim: %v", err)
	}
}

// detectTopologyWithRetry retries topology detection a few times — right after
// waitForMongo confirms Ping works, a replica set that's still electing a
// primary can transiently fail `hello` (or connecting to it as a fresh client
// doesn't yet reflect its own answer). Fails hard after that: this app treats
// connecting to a config server or a shard member directly as an operator
// configuration error, not something to silently work around.
func detectTopologyWithRetry(ctx context.Context, st *store.Store) (store.Topology, error) {
	var lastErr error
	for i := 0; i < 5; i++ {
		topo, err := st.DetectTopology(ctx)
		if err == nil {
			return topo, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return store.TopologyUnknown, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return store.TopologyUnknown, fmt.Errorf("detect topology: %w", lastErr)
}

func waitForMongo(ctx context.Context, st *store.Store) {
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
	log.Printf("hotelsim: mongo still unreachable after 60s, starting anyway")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// redactURI strips credentials before logging a connection string.
func redactURI(uri string) string {
	at := -1
	for i := 0; i < len(uri); i++ {
		if uri[i] == '@' {
			at = i
		}
		if uri[i] == '/' && i+1 < len(uri) && uri[i+1] == '/' {
			// stop scanning at the first path/query segment after scheme
		}
	}
	if at == -1 {
		return uri
	}
	schemeEnd := 0
	for i := 0; i+2 < len(uri); i++ {
		if uri[i] == ':' && uri[i+1] == '/' && uri[i+2] == '/' {
			schemeEnd = i + 3
			break
		}
	}
	return uri[:schemeEnd] + "***:***@" + uri[at+1:]
}

// healthcheck exits 0 if the already-running server instance's /healthz answers 200,
// non-zero otherwise. See the -healthcheck branch in main above.
func healthcheck() {
	port := envOr("PORT", "8089")
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
