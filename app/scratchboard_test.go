package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestScratchboardKeepAndForget(t *testing.T) {
	app := newTestApp(t)
	ann, _ := app.store.CreateUser("ann", "x", RoleUser, StatusApproved)
	bob, _ := app.store.CreateUser("bob", "x", RoleUser, StatusApproved)
	do := func(u User, method, bid, body string, h func(*App) http.HandlerFunc) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/", strings.NewReader(body))
		if bid != "" {
			r.SetPathValue("bid", bid)
		}
		h(app)(w, withPrincipal(r, principal{User: u}))
		return w
	}
	list := func(u User) (bool, []ScratchboardDoc) {
		t.Helper()
		var out struct {
			Enabled bool
			Boards  []ScratchboardDoc
		}
		w := do(u, "GET", "", "", m((*App).handleScratchboards))
		json.Unmarshal(w.Body.Bytes(), &out)
		return out.Enabled, out.Boards
	}
	board := `{"name":"certs","cards":[{"type":"note","state":{"text":"secret"}}]}`

	// Off by default: nothing is listed and nothing is accepted.
	if s := app.userSettingsFor(ann.ID); s.Scratchboard != ScratchboardStoreOff {
		t.Fatalf("default = %q, want off", s.Scratchboard)
	}
	if w := do(ann, "PUT", "b1", board, m((*App).handleSaveScratchboard)); w.Code != http.StatusConflict {
		t.Fatalf("save while off: %d", w.Code)
	}

	if w := do(ann, "PUT", "", `{"scratchboard":"on"}`, m((*App).handleUpdateSettings)); w.Code != http.StatusOK {
		t.Fatalf("turn on: %d %s", w.Code, w.Body)
	}
	if w := do(ann, "PUT", "b1", board, m((*App).handleSaveScratchboard)); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if w := do(ann, "PUT", "../x", board, m((*App).handleSaveScratchboard)); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", w.Code)
	}
	if w := do(ann, "PUT", "b2", `[1]`, m((*App).handleSaveScratchboard)); w.Code != http.StatusBadRequest {
		t.Fatalf("not an object: %d", w.Code)
	}
	on, boards := list(ann)
	if !on || len(boards) != 1 || boards[0].ID != "b1" || string(boards[0].Data) != board || boards[0].Rev != 1 {
		t.Fatalf("list = %v %+v", on, boards)
	}
	// Sealed at rest.
	var raw string
	app.store.db.QueryRow(`SELECT data FROM scratchboards WHERE owner_id = ?`, ann.ID).Scan(&raw)
	if strings.Contains(raw, "secret") || !strings.HasPrefix(raw, "v1:") {
		t.Fatalf("stored in plaintext: %q", raw)
	}
	// Another account sees none of it.
	if _, b := list(bob); len(b) != 0 {
		t.Fatalf("bob sees %+v", b)
	}

	// A save from a tab that has not seen the latest rev is refused, with the stored copy.
	do(ann, "PUT", "b1", board, m((*App).handleSaveScratchboard)) // rev 2
	cond := func(rev string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", "/?rev="+rev, strings.NewReader(`{"name":"mine"}`))
		r.SetPathValue("bid", "b1")
		m((*App).handleSaveScratchboard)(app)(w, withPrincipal(r, principal{User: ann}))
		return w
	}
	if w := cond("1"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"rev":2`) || !strings.Contains(w.Body.String(), "certs") {
		t.Fatalf("stale save: %d %s", w.Code, w.Body)
	}
	if w := cond("2"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"rev":3`) {
		t.Fatalf("current save: %d %s", w.Code, w.Body)
	}
	if w := do(ann, "PUT", "new", board, m((*App).handleSaveScratchboard)); w.Code != http.StatusOK {
		t.Fatalf("unconditional: %d", w.Code)
	}
	do(ann, "DELETE", "new", "", m((*App).handleDeleteScratchboard))

	// One board stops being kept.
	do(ann, "PUT", "b2", board, m((*App).handleSaveScratchboard))
	if w := do(ann, "DELETE", "b2", "", m((*App).handleDeleteScratchboard)); w.Code != http.StatusOK {
		t.Fatalf("delete: %d", w.Code)
	}
	if _, b := list(ann); len(b) != 1 {
		t.Fatalf("after delete: %+v", b)
	}

	// Switching keeping off deletes everything that was kept.
	if w := do(ann, "PUT", "", `{"scratchboard":"off"}`, m((*App).handleUpdateSettings)); w.Code != http.StatusOK {
		t.Fatalf("turn off: %d", w.Code)
	}
	var n int
	app.store.db.QueryRow(`SELECT COUNT(*) FROM scratchboards WHERE owner_id = ?`, ann.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d boards left after turning keeping off", n)
	}
}

// A database whose scratchboards table predates the rev column opens with the column added.
func TestScratchboardRevColumnAdded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE scratchboards (
  owner_id   INTEGER NOT NULL,
  board_id   TEXT NOT NULL,
  data       TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (owner_id, board_id)
);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Scratchboards(1); err != nil {
		t.Fatalf("list after upgrade: %v", err)
	}
}
