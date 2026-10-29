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
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"perfsim/internal/core"
	"perfsim/internal/mysqlx"
)

//go:embed web/static
var webDist embed.FS

func main() {
	// The runtime image is distroless, so DBCanvas checks readiness by
	// executing the binary itself with this flag.
	for _, arg := range os.Args[1:] {
		if arg == "-healthcheck" || arg == "--healthcheck" {
			healthcheck()
			return
		}
	}
	var cfg mysqlx.Config
	if err := json.Unmarshal([]byte(os.Getenv("PERFSIM_TARGET")), &cfg); err != nil || len(cfg.Members) == 0 {
		log.Fatalf("perfsim: PERFSIM_TARGET must describe the linked target and its members: %v", err)
	}
	port := envOr("PORT", "8096")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := core.NewHub(ctx, mysqlx.New(cfg))
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
	log.Printf("perfsim: %s (%d members), dashboard on :%s", cfg.Name, len(cfg.Members), port)
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
