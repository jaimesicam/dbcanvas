// Command stocksim is the Stock Market Sim demo app: a small stock-exchange
// application with full browser CRUD, a live dashboard, and a printable
// report, running against whichever database it was pointed at.
//
// Unlike its five sibling sims it is not tied to one engine. dbcanvas either
// resolves a linked database node on the canvas, or passes through connection
// options a user typed by hand — including for a database outside the stack
// entirely. Either way this binary just reads its environment; see
// internal/store for what it does with it.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"stocksim/internal/api"
	"stocksim/internal/sim"
	"stocksim/internal/store"
)

//go:embed web/static
var webDist embed.FS

func main() {
	// The runtime image is distroless (no shell, no curl) — dbcanvas's own
	// provisioning waits for readiness the same way it does for every other
	// container node type, `Exec`ing a check *inside* the container. With no
	// shell available, that check is this flag: exec the binary itself.
	for _, arg := range os.Args[1:] {
		switch arg {
		case "-healthcheck", "--healthcheck":
			healthcheck()
			return
		case "-testconn", "--testconn":
			// Same idea, but for a connection that has not been committed to
			// yet: dbcanvas starts a throwaway container with candidate
			// settings and execs this, so a user can check an external
			// database before deploying rather than after a failed deploy.
			testconn()
			return
		}
	}

	cfg := configFromEnv()
	port := envOr("PORT", "8093")
	targetKind := envOr("TARGET_KIND", "")
	targetLabel := envOr("TARGET_LABEL", "")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg)
	if err != nil {
		// A misconfiguration (unsupported engine, reserved database name) is
		// fatal and worth saying loudly — it will never fix itself by
		// retrying, unlike an unreachable host.
		log.Fatalf("stocksim: %v", err)
	}
	defer st.Close()

	waitForDatabase(ctx, st)

	webFS, err := fs.Sub(webDist, "web/static")
	if err != nil {
		log.Fatalf("stocksim: embedded web assets: %v", err)
	}

	engine := sim.NewEngine(st, targetKind, targetLabel)
	h := api.New(engine, st, webFS)
	srv := &http.Server{Addr: ":" + port, Handler: h.Routes()}

	go func() {
		<-ctx.Done()
		log.Printf("stocksim: shutting down")
		engine.Stop()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	// Seeding runs in the background, AFTER the HTTP server starts listening
	// (below). dbcanvas's own readiness check only waits up to 60s for
	// /healthz to answer, and /healthz deliberately reports only database
	// reachability, never seed completion — so the node is marked running as
	// soon as it can talk to its target, and the dashboard's own seeding panel
	// (driven by GET /api/state's "seed" field) shows real progress from
	// there. Against a distant external database that first schema creation
	// can take a while.
	go func() {
		if err := engine.SeedIfNeeded(ctx); err != nil {
			log.Printf("stocksim: seed: %v", err)
			return
		}
		engine.StartAgents(ctx)
	}()

	log.Printf("stocksim: listening on :%s (engine=%s, database=%s, target=%s)",
		port, cfg.Engine, cfg.Database, describeTarget(cfg, targetLabel))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("stocksim: %v", err)
	}
}

// configFromEnv assembles the connection description. Each engine reads the
// same variable names its sibling sim uses, so the shape is familiar; DB_DSN
// is the escape hatch that beats all of them.
func configFromEnv() store.Config {
	c := store.Config{
		Engine:   strings.ToLower(envOr("DB_ENGINE", store.EngineMySQL)),
		DSN:      os.Getenv("DB_DSN"),
		Host:     os.Getenv("DB_HOST"),
		Port:     envInt("DB_PORT", 0),
		User:     os.Getenv("DB_USER"),
		Password: os.Getenv("DB_PASSWORD"),
		Database: envOr("DB_NAME", store.DefaultDatabase),
		TLS:      envOr("DB_TLS", "prefer"),
		Params:   os.Getenv("DB_PARAMS"),
	}
	// Per-engine aliases, so a linked node's environment reads naturally and
	// matches what the sibling sims already set.
	switch c.Engine {
	case store.EngineMySQL:
		c.DSN = firstNonEmpty(c.DSN, os.Getenv("MYSQL_DSN"))
		c.Database = firstNonEmpty(os.Getenv("MYSQL_DB"), c.Database)
	case store.EnginePostgres:
		c.DSN = firstNonEmpty(c.DSN, os.Getenv("POSTGRES_DSN"))
		c.Database = firstNonEmpty(os.Getenv("POSTGRES_DB"), c.Database)
	case store.EngineMongoDB:
		c.DSN = firstNonEmpty(c.DSN, os.Getenv("MONGO_URI"))
		c.Database = firstNonEmpty(os.Getenv("MONGO_DB"), c.Database)
	case store.EngineValkey:
		c.Host = firstNonEmpty(c.Host, os.Getenv("VALKEY_ADDRS"))
		c.Password = firstNonEmpty(c.Password, os.Getenv("VALKEY_PASSWORD"))
		c.Database = firstNonEmpty(os.Getenv("VALKEY_PREFIX"), c.Database)
	}
	return c
}

// waitForDatabase gives the target a minute to come up before starting anyway.
// Starting anyway is the right call: the dashboard can report "cannot reach
// MySQL" far more usefully than a container that exits and gets restarted.
func waitForDatabase(ctx context.Context, st store.Store) {
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
	log.Printf("stocksim: database still unreachable after 60s, starting anyway")
}

func describeTarget(c store.Config, label string) string {
	if label != "" {
		return label
	}
	if c.Host != "" {
		return fmt.Sprintf("%s:%d", c.Host, c.Port)
	}
	return "(dsn)"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// healthcheck exits 0 if the already-running server instance's /healthz
// answers 200, non-zero otherwise. See the -healthcheck branch in main above.
func healthcheck() {
	port := envOr("PORT", "8093")
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

// testconn opens the configured database, reports what it found, and exits.
// Unlike healthcheck it talks to the database directly rather than to a
// running server, because the whole point is to check a configuration before
// anything has been deployed with it. Output is a single line on stdout, which
// is what dbcanvas shows the user verbatim.
func testconn() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cfg := configFromEnv()
	st, err := store.Open(ctx, cfg)
	if err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	if err := st.Ping(ctx); err != nil {
		fmt.Printf("error: cannot reach %s at %s: %v\n",
			cfg.Engine, describeTarget(cfg, ""), err)
		os.Exit(1)
	}
	version, err := st.ServerVersion(ctx)
	if err != nil {
		fmt.Printf("error: connected, but could not read the server version: %v\n", err)
		os.Exit(1)
	}
	// Report what is already there, so a user pointing at a database that
	// another deployment is using finds out now rather than after seeding.
	objects, err := st.Objects(ctx)
	if err != nil {
		fmt.Printf("ok: connected to %s %s — database %q not readable yet (%v)\n",
			cfg.Engine, version, cfg.Database, err)
		return
	}
	var rows int64
	for _, o := range objects {
		rows += o.Rows
	}
	switch len(objects) {
	case 0:
		fmt.Printf("ok: connected to %s %s — %q is empty, ready to be created\n",
			cfg.Engine, version, cfg.Database)
	default:
		fmt.Printf("ok: connected to %s %s — %q already has %d of this app's objects (~%d rows)\n",
			cfg.Engine, version, cfg.Database, len(objects), rows)
	}
}
