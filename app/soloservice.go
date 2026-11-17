package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// soloservice.go — dbcanvas runs as a background service that starts and stops itself.
//
// There is no command to start it. A command that needs it (opening the browser, `up`,
// `ls`, …) starts it when it is not running, detached from the terminal, and returns the
// prompt as soon as it answers. It stops by itself once nothing needs it: no stack has a
// container, no deploy is running, nothing is connected (a browser, a shared session's
// guests, a command waiting on a deploy), and that has lasted soloIdleGrace. So it lives
// as long as the longest-lived stack. After a reboot it is not running until the next
// command; it then catches up, removing what expired while it was down.
//
// `dbcanvas status` says whether it runs and what keeps it up; `dbcanvas stop` stops it.
// Neither starts it, and neither do `version`, `help` and `versions`.

// soloIdleGraceDefault is how long the service waits, with nothing to keep it up, before
// it stops; DBCANVAS_IDLE_GRACE (a Go duration, e.g. 30m) changes it.
const soloIdleGraceDefault = 10 * time.Minute

// soloIdleGrace is the grace in effect.
func soloIdleGrace() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv("DBCANVAS_IDLE_GRACE"))); err == nil && d > 0 {
		return d
	}
	return soloIdleGraceDefault
}

// serviceArg is the hidden command the service runs as.
const serviceArg = "_service"

// soloLogName is the service's log in the data folder; soloLogMax rotates it.
const (
	soloLogName = "dbcanvas.log"
	soloLogMax  = 10 << 20
)

// soloActivity counts what the browser port and the socket are serving, for the idle check.
type soloActivity struct {
	active atomic.Int64 // requests in flight, long-lived ones (event streams, websockets) included
	last   atomic.Int64 // unix nanoseconds of the last request to finish
}

func (s *soloActivity) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Asking whether it runs is not a reason to keep it running.
		if r.URL.Path == "/api/solo/status" {
			next.ServeHTTP(w, r)
			return
		}
		s.active.Add(1)
		defer func() {
			s.active.Add(-1)
			s.last.Store(time.Now().UnixNano())
		}()
		next.ServeHTTP(w, r)
	})
}

// soloKeepAlive is what keeps the service up, for the idle check and `dbcanvas status`.
type soloKeepAlive struct {
	Containers  int    `json:"containers"`  // deployed nodes with a container, or still being built
	Deploying   int    `json:"deploying"`   // deploys in progress
	Connections int64  `json:"connections"` // requests in flight: browsers, guests, waiting commands
	IdleSince   string `json:"idleSince,omitempty"`
}

func (k soloKeepAlive) busy() bool { return k.Containers > 0 || k.Deploying > 0 || k.Connections > 0 }

func (a *App) soloKeepAlive() soloKeepAlive {
	var k soloKeepAlive
	a.store.db.QueryRow(`SELECT COUNT(*) FROM deployments
		WHERE COALESCE(container_id, '') <> '' OR state IN ('pending', 'provisioning')`).Scan(&k.Containers)
	a.deploys.Range(func(_, _ any) bool { k.Deploying++; return true })
	if a.activity != nil {
		k.Connections = a.activity.active.Load()
		if last := a.activity.last.Load(); last != 0 {
			k.IdleSince = time.Unix(0, last).UTC().Format(time.RFC3339)
		}
	}
	return k
}

// watchIdle stops the service once nothing has kept it up for soloIdleGrace.
func (a *App) watchIdle(ctx context.Context, stop func()) {
	grace := soloIdleGrace()
	every := 30 * time.Second
	if grace/2 < every {
		every = grace / 2
	}
	if every < time.Second {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if a.soloKeepAlive().busy() {
			continue
		}
		last := time.Unix(0, a.activity.last.Load())
		if time.Since(last) < grace {
			continue
		}
		log.Printf("nothing to keep dbcanvas up for %s: stopping", grace)
		stop()
		return
	}
}

// soloStartedAt is when this service started.
var soloStartedAt = time.Now()

// handleSoloStatus answers `dbcanvas status` on the control socket.
func (a *App) handleSoloStatus(w http.ResponseWriter, r *http.Request) {
	if p, ok := principalOf(r); !ok || !p.Socket {
		writeErr(w, http.StatusForbidden, "status is given on the control socket only")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pid": os.Getpid(), "url": a.soloURL, "version": appVersion, "instance": instanceName,
		"startedAt": soloStartedAt.UTC().Format(time.RFC3339), "keepAlive": a.soloKeepAlive(),
		"idleGraceSeconds": int(soloIdleGrace().Seconds()),
		"hostArch":         a.hostArch, "defaultArch": platformArch(), "kubernetesArch": k8sArch(),
		"binaryUpdated": binaryUpdated(),
	})
}

// handleSoloStop answers `dbcanvas stop` on the control socket, then stops the service.
func (a *App) handleSoloStop(w http.ResponseWriter, r *http.Request) {
	if p, ok := principalOf(r); !ok || !p.Socket {
		writeErr(w, http.StatusForbidden, "stop is given on the control socket only")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopping": true, "keepAlive": a.soloKeepAlive()})
	if a.soloStop != nil {
		go func() {
			time.Sleep(100 * time.Millisecond) // let the answer go out first
			a.soloStop()
		}()
	}
}

// runService is the service process: it takes the engine lock and serves until it is
// stopped, idle, or signalled. A second one finding the lock taken exits quietly: the
// command that started it talks to the first.
func runService() int {
	dir, err := ensureDataDir()
	if err != nil {
		log.Print(err)
		return 1
	}
	lock, err := lockEngine(dir)
	if errors.Is(err, errEngineRunning) {
		return 0
	}
	if err != nil {
		log.Print(err)
		return 1
	}
	defer lock.release()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go watchDataDir(ctx, lock, dir, stop)
	log.Printf("dbcanvas %s service starting (pid %d)", appVersion, os.Getpid())
	recordBinary()
	if err := runServer(serveOpts{ctx: ctx, stop: stop, tcp: true, dataDir: dir, service: true}); err != nil {
		log.Print(err)
		return 1
	}
	if restartRequested.Load() {
		// The binary was replaced (make dbcanvas, a new release): carry on as the new one. The
		// lock goes first, so the new process can take it; the log stays open across the exec.
		lock.release()
		log.Printf("restarting as the updated %s", soloBinary.path)
		if err := syscall.Exec(soloBinary.path, []string{soloBinary.path, serviceArg}, os.Environ()); err != nil {
			log.Printf("could not restart (%v); the next dbcanvas command starts it", err)
		}
		return 0
	}
	log.Print("dbcanvas service stopped")
	return 0
}

// soloBinary is the file the service runs from, as it was when the service started.
var soloBinary struct {
	path    string
	size    int64
	modTime time.Time
}

// restartRequested is set when the service stops to restart as an updated binary.
var restartRequested atomic.Bool

func recordBinary() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if fi, err := os.Stat(exe); err == nil {
		soloBinary.path, soloBinary.size, soloBinary.modTime = exe, fi.Size(), fi.ModTime()
	}
}

// binaryUpdated reports whether the file the service started from has been replaced since.
// A file that is gone is not an update: there is nothing to restart as.
func binaryUpdated() bool {
	if soloBinary.path == "" {
		return false
	}
	fi, err := os.Stat(soloBinary.path)
	return err == nil && (fi.Size() != soloBinary.size || !fi.ModTime().Equal(soloBinary.modTime))
}

// watchBinary restarts the service as its updated binary once nothing would notice: no deploy
// running and nothing connected (an open browser tab keeps it as it is until closed).
func (a *App) watchBinary(ctx context.Context, stop func()) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !binaryUpdated() {
			continue
		}
		if k := a.soloKeepAlive(); k.Deploying > 0 || k.Connections > 0 {
			continue
		}
		log.Printf("%s was updated: restarting", soloBinary.path)
		restartRequested.Store(true)
		stop()
		return
	}
}

// ensureService makes sure the service answers on the control socket, starting it when it
// does not, and returns once it does.
func ensureService(dir string) error {
	if ctlReachable(dir) {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(dir, soloLogName)
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > soloLogMax {
		os.Rename(logPath, logPath+".1")
	}
	logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	before, _ := logf.Seek(0, 2)
	cmd := exec.Command(exe, serviceArg)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logf, logf
	// Its own session: no terminal to lose, and Ctrl-C in this one does not reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the dbcanvas service: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ctlReachable(dir) {
			return nil
		}
		select {
		case err := <-exited:
			// It exits at once, cleanly, when another one holds the lock: that one is starting.
			if err == nil {
				for time.Now().Before(deadline) && !ctlReachable(dir) {
					time.Sleep(200 * time.Millisecond)
				}
				if ctlReachable(dir) {
					return nil
				}
			}
			return fmt.Errorf("the dbcanvas service did not start: %s", logTail(logPath, before))
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("the dbcanvas service did not answer within a minute; see %s", logPath)
}

// logTail is what the log gained after offset, last lines first, for an error message.
func logTail(path string, offset int64) string {
	b, err := os.ReadFile(path)
	if err != nil || int64(len(b)) <= offset {
		return "see " + path
	}
	lines := strings.Split(strings.TrimSpace(string(b[offset:])), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return strings.Join(lines, " | ")
}

// soloPlatforms says which platforms the configuration or the environment set.
type soloPlatforms struct{ docker, k8s bool }

// platformsSet is what the command found set, before any default was filled in.
var platformsSet soloPlatforms

// hostArchName turns the Docker daemon's architecture (x86_64, aarch64, arm64) into amd64 or
// arm64; "" when it is neither or unknown.
func hostArchName(a string) string {
	switch {
	case strings.Contains(a, "arm") || strings.Contains(a, "aarch64"):
		return "arm64"
	case strings.Contains(a, "x86_64") || strings.Contains(a, "amd64"):
		return "amd64"
	}
	return ""
}

// detectPlatforms makes the Docker host's own architecture the default for what the
// configuration does not set: the platform of nodes (DOCKER_PLATFORM) and of Kubernetes nodes
// (K8S_PLATFORM), which must be native. A node can still ask for the other architecture.
func (a *App) detectPlatforms(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	arch := hostArchName(a.docker.HostArch(ctx))
	a.hostArch = arch
	if arch == "" {
		log.Printf("could not ask Docker for its architecture; nodes default to %s", pullPlatform())
		return
	}
	if !platformsSet.docker {
		os.Setenv("DOCKER_PLATFORM", "linux/"+arch)
	}
	if !platformsSet.k8s {
		os.Setenv("K8S_PLATFORM", "linux/"+arch)
	}
	log.Printf("Docker runs on %s: nodes default to %s, Kubernetes nodes run on %s", arch, pullPlatform(), k8sPlatform())
}

// watchDataDir stops the service when its data folder is deleted or replaced under it (an
// `rm -rf ~/.dbcanvas` while it runs). Its database and lock are then gone for everyone else,
// and the next command starts a service on the new folder, so this one would only linger,
// holding the browser's port.
func watchDataDir(ctx context.Context, lock *engineLock, dir string, stop func()) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !lock.stillOurs() {
			log.Printf("the data folder %s was removed or replaced while dbcanvas ran: stopping", dir)
			stop()
			return
		}
	}
}
