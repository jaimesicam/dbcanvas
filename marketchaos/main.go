// Command marketchaos runs the "Unoptimized MySQL Challenge" — a fictional
// stock-exchange simulator (product name MarketChaos) deployed with
// deliberately bad indexes, queries, and transaction patterns so a learner
// can diagnose and fix them. It reads and writes whichever MySQL-family
// target (standalone Percona Server, a direct PXC member, a PXC cluster, a
// MySQL replication frame, or either behind HAProxy) it was deployed
// against, plus a web server exposing a live operations-style dashboard.
// See app/marketchaos.go for how dbcanvas resolves and wires the target.
//
// Stage S2: the 10 workload agents (hybrid ticker/worker-pool concurrency)
// on top of stage S1's schema/profiles/seeder. The challenge/grading engine
// and full dashboard panels (stage S3+) are not implemented yet.
package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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
	dataset := datasetFromEnv()

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

	members := openMembers(envOr("MYSQL_DSN_MEMBERS", ""))
	haproxyStatsURL := envOr("HAPROXY_STATS_URL", "")

	engine := sim.NewEngine(st, kind, targetLabel, dataset, members, haproxyStatsURL)
	engine.Start(ctx)

	h := api.New(engine, st, webFS)
	srv := &http.Server{Addr: ":" + port, Handler: h.Routes()}

	go func() {
		<-ctx.Done()
		log.Printf("marketchaos: shutting down")
		engine.Stop()
		members.Close()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	// Seeding runs in the background, AFTER the HTTP server starts listening
	// (below) — a Large profile's ~28M rows can take many minutes, and
	// dbcanvas's own readiness check only waits up to 60s for /healthz to
	// answer. /healthz only checks MySQL reachability, never seed
	// completion, so the node is correctly marked "running" as soon as it
	// can talk to its target — the dashboard's own seeding panel (driven by
	// GET /api/state's "seed" field) is what shows real progress from there.
	// Workload agents only start once seeding is confirmed done — pointing
	// 10 agent types at a half-seeded market would be meaningless at best.
	go func() {
		if err := engine.SeedIfNeeded(ctx); err != nil {
			log.Printf("marketchaos: seed: %v", err)
			return
		}
		engine.StartAgents(ctx)
	}()

	log.Printf("marketchaos: listening on :%s (mysql schema %s, kind %s, dataset %d traders/%d orders/%d trades/%d ticks)",
		port, dbName, kind, dataset.Traders, dataset.Orders, dataset.Trades, dataset.Ticks)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("marketchaos: %v", err)
	}
}

// datasetFromEnv resolves the dataset-size profile dbcanvas passed in.
// DATASET_PROFILE selects a preset (small/medium/large); DATASET_TRADERS/
// ORDERS/TRADES/TICKS override individual counts — set by dbcanvas for a
// "custom" profile, but also honored for a named preset in case a future
// dbcanvas version wants to fine-tune one count without a 5th preset name.
func datasetFromEnv() sim.DatasetCounts {
	profile := sim.DatasetProfile(envOr("DATASET_PROFILE", string(sim.ProfileMedium)))
	d := sim.Preset(profile)
	if v := envInt("DATASET_TRADERS", 0); v > 0 {
		d.Traders = v
	}
	if v := envInt("DATASET_ORDERS", 0); v > 0 {
		d.Orders = v
	}
	if v := envInt("DATASET_TRADES", 0); v > 0 {
		d.Trades = v
	}
	if v := envInt("DATASET_TICKS", 0); v > 0 {
		d.Ticks = v
	}
	return d
}

// openMembers parses MYSQL_DSN_MEMBERS (a JSON array of DSNs, set only for a
// direct PXC cluster-frame link — see app/marketchaos.go's
// waitPXCAllMembersRunning) and opens an independent connection to each.
// Returns nil (a valid, empty *sim.MemberPool per pxc.go's nil-receiver
// methods) for every other target shape, or if any member DSN fails to
// parse — a malformed member list shouldn't take down the whole app when
// the primary MYSQL_DSN connection above already works fine on its own.
func openMembers(raw string) *sim.MemberPool {
	if raw == "" {
		return nil
	}
	var dsns []string
	if err := json.Unmarshal([]byte(raw), &dsns); err != nil {
		log.Printf("marketchaos: MYSQL_DSN_MEMBERS: invalid JSON, ignoring: %v", err)
		return nil
	}
	dbs := make([]*sql.DB, 0, len(dsns))
	for _, dsn := range dsns {
		db, err := store.OpenMember(dsn, sim.MemberConnCap)
		if err != nil {
			log.Printf("marketchaos: MYSQL_DSN_MEMBERS: %v", err)
			continue
		}
		dbs = append(dbs, db)
	}
	if len(dbs) == 0 {
		return nil
	}
	return sim.NewMemberPool(dbs)
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
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
