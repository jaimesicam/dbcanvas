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

	// TARGET_BYTES is how big the dataset should get at the High load level.
	// Unset means the default; an explicit "0" or "off" means never grow it.
	// A malformed value is a warning rather than a fatal, because refusing to
	// start a whole simulation over a mistyped size would be a poor trade.
	targetBytes := sim.DefaultTargetBytes
	if v := strings.TrimSpace(os.Getenv("TARGET_BYTES")); v != "" {
		if strings.EqualFold(v, "off") || strings.EqualFold(v, "none") {
			targetBytes = 0
		} else if n, err := sim.ParseSize(v); err != nil {
			log.Printf("stocksim: ignoring TARGET_BYTES: %v", err)
		} else {
			targetBytes = n
		}
	}

	// WORKING_SET is how much of that dataset is kept under continuous random
	// read — "50%", "0.5", "2G", or "off". Same malformed-value policy as above.
	workingSet := sim.DefaultWorkingSet()
	if ws, err := sim.ParseWorkingSet(os.Getenv("WORKING_SET")); err != nil {
		log.Printf("stocksim: ignoring WORKING_SET: %v", err)
	} else {
		workingSet = ws
	}

	// ORDER_RETENTION is how long a settled order is kept before the sweep
	// removes it — "15m", "2h", or "off" to keep everything. Left alone it is
	// DefaultOrderRetention, which is what stops the two-second order count
	// from getting linearly more expensive for the life of the deployment.
	retention := sim.DefaultOrderRetention
	if v := strings.TrimSpace(os.Getenv("ORDER_RETENTION")); v != "" {
		if strings.EqualFold(v, "off") || strings.EqualFold(v, "none") {
			retention = 0
		} else if d, err := time.ParseDuration(v); err != nil {
			log.Printf("stocksim: ignoring ORDER_RETENTION: %v", err)
		} else {
			retention = d
		}
	}

	// The lab knobs, all off unless asked for. Each exists to make the target
	// exhibit one measurable pathology; see internal/sim/lab.go and
	// internal/sim/labstress.go.
	//
	// IDLE_TXN holds a transaction open with a read snapshot ("30m", "2h", max
	// 24h) so purge cannot advance and the history list grows. EXTRA_TABLES
	// creates that many synthetic tables and reads them in rotation, for table
	// cache pressure. TEMP_TABLES is off | memory | disk, for a query shaped to
	// build a large intermediate result. LOCK_CONTENTION is off | light | heavy,
	// for writers competing over a few rows. SCAN_QUERIES is how many index-less
	// reads to run per minute. WRITE_PRESSURE is off | commits | redo, for the
	// two distinct shapes of write cost.
	idleTxn := time.Duration(0)
	if v := strings.TrimSpace(os.Getenv("IDLE_TXN")); v != "" && !strings.EqualFold(v, "off") {
		if d, err := time.ParseDuration(v); err != nil {
			log.Printf("stocksim: ignoring IDLE_TXN: %v", err)
		} else {
			idleTxn = store.ClampIdleTransaction(d)
		}
	}
	extraTables := store.ClampExtraTables(envInt("EXTRA_TABLES", 0))
	tempTables := strings.ToLower(strings.TrimSpace(envOr("TEMP_TABLES", store.TempOff)))
	if !store.ValidTempMode(tempTables) {
		log.Printf("stocksim: ignoring TEMP_TABLES=%q (want off, memory or disk)", tempTables)
		tempTables = store.TempOff
	}
	lockContention := strings.ToLower(strings.TrimSpace(envOr("LOCK_CONTENTION", store.ContentionOff)))
	if !store.ValidContentionMode(lockContention) {
		log.Printf("stocksim: ignoring LOCK_CONTENTION=%q (want off, light or heavy)", lockContention)
		lockContention = store.ContentionOff
	}
	scanRate := store.ClampScanRate(envInt("SCAN_QUERIES", 0))
	writePressure := strings.ToLower(strings.TrimSpace(envOr("WRITE_PRESSURE", store.WritePressureOff)))
	if !store.ValidWritePressureMode(writePressure) {
		log.Printf("stocksim: ignoring WRITE_PRESSURE=%q (want off, commits or redo)", writePressure)
		writePressure = store.WritePressureOff
	}

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
	engine.TargetBytes = targetBytes
	engine.WorkingSet = workingSet
	engine.Threads = cfg.Threads
	engine.OrderRetention = retention
	engine.IdleTxn = idleTxn
	engine.ExtraTables = extraTables
	engine.TempTables = tempTables
	engine.LockContention = lockContention
	engine.ScanRate = scanRate
	engine.WritePressure = writePressure
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

	log.Printf("stocksim: listening on :%s (engine=%s, objects in %s, target=%s, threads=%d, "+
		"idle txn=%s, extra tables=%d, temp tables=%s, lock contention=%s, scans/min=%d, "+
		"write pressure=%s)",
		port, cfg.Engine, st.Location(), describeTarget(cfg, targetLabel), cfg.Threads,
		durationWord(idleTxn), extraTables, tempTables, lockContention, scanRate, writePressure)
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
		// DB_THREADS is the concurrency knob: how many workers each heavy agent
		// runs, and — through Config.PoolSize — how many connections the pool
		// may open. Read here rather than in the engine because the pool is
		// built at Open, before the engine exists.
		Threads: store.ClampThreads(envInt("DB_THREADS", store.DefaultThreads)),
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

// durationWord renders a lab duration for the startup line.
func durationWord(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	return d.String()
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
		fmt.Printf("ok: connected to %s %s — %s not readable yet (%v)\n",
			cfg.Engine, version, st.Location(), err)
		return
	}
	var rows int64
	for _, o := range objects {
		rows += o.Rows
	}
	switch len(objects) {
	case 0:
		fmt.Printf("ok: connected to %s %s — %s is empty, ready to be created\n",
			cfg.Engine, version, st.Location())
	default:
		fmt.Printf("ok: connected to %s %s — %s already has %d of this app's objects (~%d rows)\n",
			cfg.Engine, version, st.Location(), len(objects), rows)
	}
}
