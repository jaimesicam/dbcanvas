package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// A request on the control socket acts as the owner, created once on first use.
func TestCtlSocketActsAsTheOwner(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	h := app.ctlSocketHandler(mux)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stacks", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/stacks over the socket: %d %s", rec.Code, rec.Body)
		}
	}
	users, _ := app.store.ListUsers()
	if len(users) != 1 || users[0].Role != RoleAdmin {
		t.Fatalf("want one owner admin, got %+v", users)
	}
	// The same request without the socket is not signed in.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stacks", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without the socket: %d", rec.Code)
	}
}

func TestEngineLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	l, err := lockEngine(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockEngine(dir); !errors.Is(err, errEngineRunning) {
		t.Fatalf("second lock: %v", err)
	}
	l.release()
	l2, err := lockEngine(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	l2.release()
}

// Unset fields are left out of a compose design; a pointer option set to false stays.
func TestLeanJSONMap(t *testing.T) {
	off := false
	v := struct {
		ID    string  `json:"id"`
		Label string  `json:"label"`
		Port  int     `json:"port"`
		OS    string  `json:"os"`
		GTID  *bool   `json:"gtid"`
		Extra *string `json:"extra"`
	}{ID: "n1", OS: "ubuntu", GTID: &off}
	m, err := leanJSONMap(v)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(m)
	if string(b) != `{"gtid":false,"id":"n1","label":"","os":"ubuntu"}` {
		t.Fatalf("got %s", b)
	}
}

func TestSoloInstanceIsValid(t *testing.T) {
	if why := validInstance(soloInstance()); why != "" {
		t.Fatalf("%q: %s", soloInstance(), why)
	}
}

// A lock is no longer this process's once its data folder is deleted, or deleted and made again.
func TestEngineLockNoticesTheFolderReplaced(t *testing.T) {
	dir := t.TempDir() + "/data"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := lockEngine(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.release()
	if !l.stillOurs() {
		t.Fatal("a fresh lock is not ours")
	}
	os.RemoveAll(dir)
	if l.stillOurs() {
		t.Error("the folder is gone, and the lock still counts as ours")
	}
	os.MkdirAll(dir, 0o700)
	l2, err := lockEngine(dir)
	if err != nil {
		t.Fatalf("a new service on the new folder: %v", err)
	}
	defer l2.release()
	if l.stillOurs() {
		t.Error("the folder was replaced, and the old lock still counts as ours")
	}
}

// The service notices its binary replaced, and not merely removed.
func TestBinaryUpdated(t *testing.T) {
	f := t.TempDir() + "/dbcanvas"
	os.WriteFile(f, []byte("v1"), 0o755)
	fi, _ := os.Stat(f)
	old := soloBinary
	defer func() { soloBinary = old }()
	soloBinary.path, soloBinary.size, soloBinary.modTime = f, fi.Size(), fi.ModTime()
	if binaryUpdated() {
		t.Fatal("unchanged, but updated")
	}
	os.WriteFile(f, []byte("version 2"), 0o755)
	if !binaryUpdated() {
		t.Error("replaced, but not noticed")
	}
	os.Remove(f)
	if binaryUpdated() {
		t.Error("removed counts as an update")
	}
}
