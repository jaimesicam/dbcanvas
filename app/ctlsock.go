package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ctlsock.go — where dbcanvas keeps its data, the lock that lets one engine run at a time,
// and the control socket the command line uses to talk to a running dbcanvas.
//
// The data folder holds the database, its encryption key and the socket. It is the
// owner's alone (0700), and so is the socket in it, which is the whole of its
// authentication: a request that arrives on it acts as the owner, the account every
// stack belongs to. Nothing else listens beyond 127.0.0.1.

// dataDir is dbcanvas's data folder: DBCANVAS_DATA, else ~/.dbcanvas. It holds the
// configuration (config.yaml), the database and its key, the engine lock, the control
// socket, and an updated version catalogue when there is one.
func dataDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("DBCANVAS_DATA")); d != "" {
		return filepath.Abs(d)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home folder to keep data in (set DBCANVAS_DATA): %v", err)
	}
	return filepath.Join(home, ".dbcanvas"), nil
}

// ensureDataDir creates the data folder, owner-only, and refuses one others can enter.
func ensureDataDir() (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", fmt.Errorf("%s is open to other users and could not be made private: %v", dir, err)
		}
	}
	return dir, nil
}

func ctlSocketPath(dir string) string { return filepath.Join(dir, "dbcanvas.sock") }

// engineLock is held by the one process running the engine: `dbcanvas serve`, or a
// command that found none running.
type engineLock struct{ f *os.File }

// errEngineRunning is another process holding the engine lock.
var errEngineRunning = errors.New("another dbcanvas is running")

// lockEngine takes the engine lock without waiting.
func lockEngine(dir string) (*engineLock, error) {
	f, err := os.OpenFile(filepath.Join(dir, "engine.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errEngineRunning
		}
		return nil, err
	}
	f.Truncate(0)
	f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return &engineLock{f: f}, nil
}

// stillOurs reports whether the lock file in the data folder is still the one this process
// locked. It is not when the folder was deleted, or deleted and made again, under a running
// service: that service is then an orphan holding a database nobody can reach.
func (l *engineLock) stillOurs() bool {
	if l == nil || l.f == nil {
		return false
	}
	onDisk, err := os.Stat(l.f.Name())
	if err != nil {
		return false
	}
	held, err := l.f.Stat()
	return err == nil && os.SameFile(onDisk, held)
}

func (l *engineLock) release() {
	if l != nil && l.f != nil {
		syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		l.f.Close()
	}
}

// listenCtlSocket listens on the control socket, owner-only. The caller holds the
// engine lock, so a socket file already there is a previous run's.
func listenCtlSocket(dir string) (net.Listener, error) {
	path := ctlSocketPath(dir)
	// A Unix socket's path is limited to about a hundred bytes.
	if len(path) > 100 {
		return nil, fmt.Errorf("the control socket's path is too long for the OS (%d bytes): set DBCANVAS_DATA to a shorter folder", len(path))
	}
	os.Remove(path)
	old := syscall.Umask(0o077)
	l, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	os.Chmod(path, 0o600)
	return l, nil
}

// ctlConnKey marks a request that arrived on the control socket.
type ctlConnKey struct{}

// ctlSocketHandler serves the control socket: every request acts as the owner.
func (a *App) ctlSocketHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, err := a.ensureOwner()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "no owner account: "+err.Error())
			return
		}
		r = withPrincipal(r, principal{User: owner, Socket: true})
		next.ServeHTTP(w, r)
	})
}

var ownerNameRe = regexp.MustCompile(`[^a-z0-9._-]+`)

// ensureOwner returns the account, created on first use and named after the OS login.
func (a *App) ensureOwner() (User, error) {
	if u, err := a.store.Owner(); err == nil || !errors.Is(err, sql.ErrNoRows) {
		return u, err
	}
	name := "owner"
	if cu, err := user.Current(); err == nil {
		if n := strings.Trim(ownerNameRe.ReplaceAllString(strings.ToLower(cu.Username), "-"), "-."); n != "" {
			name = n
		}
	}
	u, err := a.store.CreateUser(name)
	if err != nil {
		return u, err
	}
	// A new owner has nothing to catch up on: the release notes start from this version.
	a.stampWhatsNewSeen(u.ID)
	return u, nil
}

// ctlClient is an HTTP client that reaches dbcanvas over the control socket.
func ctlClient(dir string) *http.Client {
	path := ctlSocketPath(dir)
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", path)
		},
	}}
}

// ctlReachable reports whether a dbcanvas answers on the control socket.
func ctlReachable(dir string) bool {
	c, err := net.DialTimeout("unix", ctlSocketPath(dir), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
