package main

import (
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed all:web/dist
var embeddedFS embed.FS

// App holds shared dependencies for the HTTP handlers.
type App struct {
	store  *Store
	docker *Docker
	// vagrant provisions nodes as VirtualBox VMs (the "vagrant" backend). nil
	// when the host has no vagrant/VirtualBox; App.eng then falls back to docker.
	vagrant *Vagrant
	// barriers holds the per-stack replication barrier for an in-flight deploy
	// (stackID -> *deployBarrier). See replication.go.
	barriers sync.Map
	// captures holds the state of in-flight/completed on-node diagnostic captures
	// (pg_gather / pt-stalk), keyed by stack/node/kind. See diag.go.
	captures sync.Map
	// versionProbes holds the in-flight "what version actually got deployed" probes
	// (nodeID -> true), so a polling UI cannot pile them up. See nodeversion.go.
	versionProbes sync.Map
	// deploys holds the in-flight provisioning run per stack
	// (stackID -> *deployRun), so destroy can cancel it and wait for the
	// provisioners to exit before removing containers. See deployrun.go.
	deploys sync.Map
	// debugSessions holds the live Delve session of each K3D frame whose operator runs under a
	// debugger (stackID/frameID -> *k3dDebugSession). Sessions outlive the browsers watching
	// them: the breakpoint set is worth keeping across a page reload. See k3ddebugsess.go.
	debugSessions sync.Map
	// gdbSessions holds the live gdb of each Linux Client analysing a core dump
	// (stackID/nodeID -> *gdbSession). They outlive the browsers watching them for a different
	// reason than the debugger's do: loading an 800 MB core takes tens of seconds, and paying
	// that again because somebody reloaded the page is a poor trade. See gdbsess.go.
	gdbSessions sync.Map
}

func main() {
	// Health-check mode for the container HEALTHCHECK (distroless has no shell).
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}

	dbPath := envOr("DB_PATH", "dbcanvas.db")
	useDataTempDir(dbPath)
	store, err := OpenStore(dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	app := &App{store: store, docker: NewDocker(envOr("DOCKER_SOCK", "/var/run/docker.sock"))}
	// The vagrant backend is optional: only usable when the host has vagrant +
	// VirtualBox and DBCanvas runs on that host (not in the distroless container).
	if v := NewVagrant(); v != nil {
		app.vagrant = v
	}

	mux := http.NewServeMux()
	// Every endpoint is declared in api_routes.go, which is also what the API
	// page and the OpenAPI document are generated from.
	app.registerRoutes(mux)

	app.startReaper()
	// Flushes buffered token last-used stamps and reaps long-dead tokens.
	app.startTokenMaintenance()

	mux.Handle("/", spaHandler())

	host := envOr("APP_HOST", "127.0.0.1")
	port := envOr("APP_PORT", "8080")
	addr := net.JoinHostPort(host, port)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("DBCanvas listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// spaHandler serves embedded static files, falling back to index.html for
// client-side routes.
func spaHandler() http.Handler {
	dist, err := fs.Sub(embeddedFS, "web/dist")
	if err != nil {
		log.Fatalf("failed to locate embedded SPA: %v", err)
	}
	fileServer := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if f, err := dist.Open(p); err == nil {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		// Unknown path → serve the SPA entrypoint for client routing.
		index, err := dist.Open("index.html")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer index.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.Copy(w, index)
	})
}

// healthcheck performs GET /api/setup/status against localhost and returns an
// exit code.
func healthcheck() int {
	port := envOr("APP_PORT", "8080")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/api/setup/status")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// deployTimeout is how long a provisioner waits for a dependency (a cluster,
// node, or shared service) to become ready before giving up. Configurable via
// DEPLOYMENT_TIMEOUT (in minutes); defaults to 60. Large stacks with many
// containers routinely need well over the old fixed 15m before everything is up.
func deployTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("DEPLOYMENT_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return 60 * time.Minute
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	defer io.Copy(io.Discard, r.Body)
	return json.NewDecoder(r.Body).Decode(v)
}
