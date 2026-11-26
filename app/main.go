package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed all:web/dist
var embeddedFS embed.FS

// App holds shared dependencies for the HTTP handlers.
type App struct {
	store  *Store
	docker *Docker
	// lima runs the nodes that run as virtual machines; nil when limactl is missing or too old,
	// and limaWhy says why. Everything else works without it.
	lima    *Lima
	limaWhy string
	// soloURL is the browser's address, once it listens; browsers sign in by start link
	// (soloweb.go).
	soloURL string
	// hostArch is the Docker host's architecture (amd64, arm64), when detectPlatforms asked.
	hostArch string
	// activity and soloStop belong to the background service (soloservice.go): what it is
	// serving, for the idle check, and how it stops itself.
	activity *soloActivity
	soloStop func()
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
	os.Exit(runCLI(os.Args[1:]))
}

// serveOpts is how runServer runs.
type serveOpts struct {
	ctx context.Context // the server stops when it is done
	// stop ends ctx; the background service runs it when idle.
	stop func()
	// dataDir is the data folder: the database lives in it and the control socket
	// listens in it.
	dataDir string
	// ready is closed once the server listens; addr is then the browser's address.
	ready chan<- string
}

// runServer runs the engine and serves the API and the web app until o.ctx ends.
func runServer(o serveOpts) error {
	dbPath := filepath.Join(o.dataDir, "dbcanvas.db")
	// Recordings, pt-stalk archives and diagnostics are kept beside the database, found
	// through DB_PATH; without it they would land in whatever folder started the service.
	os.Setenv("DB_PATH", dbPath)
	// Refuse a malformed instance name rather than deploy under it: every name this installation
	// gives a container is built on it, and so is how it tells its own from another's (instance.go).
	if why := validInstance(instanceName); why != "" {
		return fmt.Errorf("DBCANVAS_INSTANCE=%q: %s", instanceName, why)
	}
	log.Printf("instance %s", instanceName)
	useDataTempDir(dbPath)
	store, err := OpenStore(dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %v", err)
	}
	defer store.Close()

	app := &App{store: store, docker: NewDocker(dockerSocket())}
	if app.lima, app.limaWhy = NewLima(limaHomeFor(o.dataDir)); app.lima == nil {
		log.Printf("nodes run in containers only: %s", app.limaWhy)
	}
	applyBundle(o.dataDir)
	app.detectPlatforms(context.Background())
	// The node accounts' passwords: generated once, kept sealed (nodepasswords.go).
	if err := app.applyNodePasswords(); err != nil {
		return fmt.Errorf("node passwords: %v", err)
	}
	// The account exists before anything listens.
	if _, err := app.ensureOwner(); err != nil {
		return fmt.Errorf("owner account: %v", err)
	}
	mux := http.NewServeMux()
	// Every endpoint is declared in api_routes.go, which is also what the API
	// page and the OpenAPI document are generated from.
	app.registerRoutes(mux)

	// Every published host port is chosen on the host, pinned and recorded
	// (hostports.go); the record is rebuilt from the containers before anything deploys.
	app.docker.portGuard = app.guardPublish
	app.docker.pullMissing = app.pullMissing
	app.reseedHostPorts(context.Background())

	// Nodes a previous process was still provisioning can never finish now (deployrun.go).
	app.failInterruptedDeployments()
	app.startReaper()
	// Samples every deployed stack for its history and alerts (livewatch.go).
	app.startLiveWatch()
	// Network and disk conditions asked for by Performance Sim nodes (perfsimcond.go).
	app.startPerfSimConditions()
	app.startPagerPanicSync()
	// Turns deep performance_schema instrumentation off when its time is up (activitydeep.go).
	app.startDeepReverter()
	// Flushes buffered token last-used stamps and reaps long-dead tokens.
	app.startTokenMaintenance()
	// Ends expired shared sessions and deletes the ones past their retention.
	app.startShareReaper()

	// A node's web UI through this port, for the browser window (browse.go).
	mux.HandleFunc(browsePrefix, app.handleBrowseProxy)
	// Licence texts and the GPL notice, public (licenses.go).
	mux.HandleFunc("GET /licenses/", handleLicenses)
	mux.Handle("/", spaHandler())
	mux.HandleFunc("GET /start/{token}", app.handleStartLink)
	mux.HandleFunc("POST /api/solo/start-link", app.handleNewStartLink)
	mux.HandleFunc("GET /api/solo/status", app.handleSoloStatus)
	mux.HandleFunc("POST /api/solo/stop", app.handleSoloStop)
	mux.HandleFunc("GET /api/solo/passwords", app.handleNodePasswords)
	handler := app.browseFallback(mux)
	app.activity = &soloActivity{}
	app.activity.last.Store(time.Now().UnixNano())
	app.soloStop = o.stop
	handler = app.activity.wrap(handler)
	go app.watchIdle(o.ctx, o.stop)
	go app.watchBinary(o.ctx, o.stop)

	errc := make(chan error, 2)
	var servers []*http.Server
	// 127.0.0.1 only, always: a browser elsewhere comes through the owner's own tunnel.
	host := "127.0.0.1"
	port := os.Getenv("APP_PORT")
	defaultPort := port == ""
	if defaultPort {
		port = soloPort
	}
	l, err := net.Listen("tcp", net.JoinHostPort(host, port))
	// A port that was asked for and is busy may be held by a service on its way out: one
	// whose data folder was replaced stops within seconds (watchDataDir). Wait for it
	// before failing. The default port is often held by something else entirely, so a busy
	// default goes straight to a free port instead.
	for deadline := time.Now().Add(15 * time.Second); err != nil && !defaultPort && errors.Is(err, syscall.EADDRINUSE) && time.Now().Before(deadline); {
		time.Sleep(500 * time.Millisecond)
		l, err = net.Listen("tcp", net.JoinHostPort(host, port))
	}
	if err != nil && defaultPort {
		// The default port is taken: any free one will do for a browser opened by link.
		l, err = net.Listen("tcp", net.JoinHostPort(host, "0"))
	}
	if err != nil {
		return fmt.Errorf("listen: %v", err)
	}
	addr := l.Addr().String()
	_, lport, _ := net.SplitHostPort(addr)
	app.soloURL = loopbackURL(addr)
	srv := &http.Server{Handler: app.soloGuard(lport, handler), ReadHeaderTimeout: 10 * time.Second}
	servers = append(servers, srv)
	log.Printf("DBCanvas listening on %s", addr)
	go func() { errc <- srv.Serve(l) }()

	cl, err := listenCtlSocket(o.dataDir)
	if err != nil {
		return fmt.Errorf("control socket: %v", err)
	}
	// Removed on the way out only if it is still this server's: after the data folder was
	// replaced, the socket at that path is a newer service's.
	ours, _ := os.Stat(ctlSocketPath(o.dataDir))
	defer func() {
		if now, err := os.Stat(ctlSocketPath(o.dataDir)); err == nil && ours != nil && os.SameFile(ours, now) {
			os.Remove(ctlSocketPath(o.dataDir))
		}
	}()
	csrv := &http.Server{Handler: app.ctlSocketHandler(handler), ReadHeaderTimeout: 10 * time.Second}
	servers = append(servers, csrv)
	go func() { errc <- csrv.Serve(cl) }()
	if o.ready != nil {
		o.ready <- addr
		close(o.ready)
	}
	select {
	case err := <-errc:
		return fmt.Errorf("server error: %v", err)
	case <-o.ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(sctx)
	}
	return nil
}

// dockerSocket is the Docker API socket: DOCKER_SOCK, else DOCKER_HOST's unix path,
// else the current Docker context's, else the standard one.
func dockerSocket() string {
	if s := os.Getenv("DOCKER_SOCK"); s != "" {
		return s
	}
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		return strings.TrimPrefix(h, "unix://")
	}
	if s := dockerContextSocket(); s != "" {
		return s
	}
	return "/var/run/docker.sock"
}

// daemonSocket is the Docker socket's path as the Docker daemon sees it, for bind-mounting
// into a container (k3d's tools node, image builds). On Linux the daemon runs on this host,
// so it is dockerSocket(), rootless Docker's /run/user/<uid>/docker.sock included. On macOS
// the daemon runs in a VM (Docker Desktop, Rancher Desktop, OrbStack, Colima), where the
// client's socket path (~/.rd/docker.sock, ~/.docker/run/docker.sock) does not exist and
// the daemon's own socket is /var/run/docker.sock.
func daemonSocket() string {
	if runtime.GOOS == "linux" {
		return dockerSocket()
	}
	return "/var/run/docker.sock"
}

// spaStaticFile reports whether p is one of the web app's files other than its page.
func spaStaticFile(p string) bool {
	if p == "" || p == "index.html" {
		return false
	}
	dist, err := fs.Sub(embeddedFS, "web/dist")
	if err != nil {
		return false
	}
	fi, err := fs.Stat(dist, p)
	return err == nil && !fi.IsDir()
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
		// An API path no route matched — a typo, a route this server does not have yet, or the
		// right path with the wrong method — is a JSON 404, not the web app's page with a 200,
		// which a script reads as success and then fails to parse.
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusNotFound, "no endpoint "+r.Method+" "+r.URL.Path+
				" — GET /api/meta/endpoints lists every one")
			return
		}
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// deployTimeout is how long a provisioner waits for a dependency (a cluster,
// node, or shared service) to become ready before giving up. Configurable via
// DEPLOYMENT_TIMEOUT, in minutes (60) or as a duration (45m, 2h); defaults to 60 minutes.
// Large stacks with many containers routinely need well over 15 minutes.
func deployTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("DEPLOYMENT_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 60 * time.Minute
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v == nil {
		return
	}
	// The status line is already sent, so a marshalling failure cannot be turned into a 500
	// — but it must not be silent either. An unmarshallable value (a func captured into a
	// response map by an unlucky variable name, which is exactly how this line came to be
	// written) otherwise leaves the caller holding "HTTP 200, zero bytes" and the server
	// with nothing to say about it.
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	defer io.Copy(io.Discard, r.Body)
	return json.NewDecoder(r.Body).Decode(v)
}

// pathID parses the {id} path value as an int64.
func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}
