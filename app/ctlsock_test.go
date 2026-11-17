package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
