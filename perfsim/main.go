// Command perfsim is Performance Sim: a dashboard of stressors that make a
// linked database cluster misbehave on purpose (replication lag first), with a
// timeline that shows what each one does to the cluster and to an application
// running against it.
//
// DBCanvas resolves the linked cluster at deploy and passes every member in
// PERFSIM_TARGET; this binary only reads its environment.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"perfsim/internal/core"
	"perfsim/internal/mongo"
	"perfsim/internal/mysqlx"
	"perfsim/internal/pgsql"
	"perfsim/internal/valkey"
)

//go:embed web/static
var webDist embed.FS

func main() {
	// The runtime image is distroless, so DBCanvas checks readiness by
	// executing the binary itself with this flag.
	testconn := false
	for _, arg := range os.Args[1:] {
		if arg == "-healthcheck" || arg == "--healthcheck" {
			healthcheck()
			return
		}
		// DBCanvas tests a hand-entered connection by running this in a
		// container with the target the sim would get.
		testconn = testconn || arg == "-testconn" || arg == "--testconn"
	}
	raw := []byte(os.Getenv("PERFSIM_TARGET"))
	var target struct {
		Name    string            `json:"name"`
		Engine  string            `json:"engine"`
		Members []json.RawMessage `json:"members"`
	}
	if err := json.Unmarshal(raw, &target); err != nil || len(target.Members) == 0 {
		log.Fatalf("perfsim: PERFSIM_TARGET must describe the linked target and its members: %v", err)
	}
	var eng core.Engine
	switch target.Engine {
	case "", "mysql":
		var cfg mysqlx.Config
		json.Unmarshal(raw, &cfg)
		eng = mysqlx.New(cfg)
	case "postgres":
		var cfg pgsql.Config
		json.Unmarshal(raw, &cfg)
		eng = pgsql.New(cfg)
	case "mongodb":
		var cfg mongo.Config
		json.Unmarshal(raw, &cfg)
		eng = mongo.New(cfg)
	case "valkey":
		var cfg valkey.Config
		json.Unmarshal(raw, &cfg)
		eng = valkey.New(cfg)
	default:
		log.Fatalf("perfsim: engine %q is not supported", target.Engine)
	}
	if testconn {
		probe(eng)
		return
	}
	port := envOr("PORT", "8096")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := core.NewHub(ctx, eng)
	go hub.Run(ctx)

	static, err := fs.Sub(webDist, "web/static")
	if err != nil {
		log.Fatalf("perfsim: embedded web assets: %v", err)
	}
	srv := &http.Server{Addr: ":" + port, Handler: hub.Routes(static), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		// Release every lock, pause and setting before the container goes away.
		hub.Close()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Printf("perfsim: %s (%s, %d members), dashboard on :%s", target.Name, target.Engine, len(target.Members), port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("perfsim: %v", err)
	}
	// Wait for hub.Close to finish releasing.
	time.Sleep(200 * time.Millisecond)
}

func healthcheck() {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + envOr("PORT", "8096") + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// probe connects as the engine does, prints what it found, and exits non-zero
// when it could not connect.
func probe(eng core.Engine) {
	p, ok := eng.(interface {
		Probe(context.Context) (string, error)
	})
	if !ok {
		fmt.Println("error: this engine has no connection test")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	msg, err := p.Probe(ctx)
	if err != nil {
		fmt.Println("error: " + err.Error())
		os.Exit(1)
	}
	fmt.Println("connected: " + msg)
}
