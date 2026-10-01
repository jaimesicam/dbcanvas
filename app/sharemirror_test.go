package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sent returns the message types a test client has been sent so far.
func sent(c *hubClient) []string {
	var ts []string
	for {
		select {
		case b := <-c.out:
			var m struct {
				T string `json:"t"`
			}
			json.Unmarshal(b, &m)
			ts = append(ts, m.T)
		default:
			return ts
		}
	}
}

func sentHas(ts []string, t string) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

func TestOnlyTheDriversScreenIsMirrored(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	_, g := f.join(t, "Jane")
	f.admit(t, g)
	h := f.app.hubFor(f.sess)
	host := &hubClient{guestID: 0, name: "host", out: make(chan []byte, 64), cancel: func() {}}
	jane := &hubClient{guestID: g.ID, name: "Jane", out: make(chan []byte, 64), cancel: func() {}}
	h.mu.Lock()
	h.clients[host], h.clients[jane] = true, true
	h.mu.Unlock()
	batch := json.RawMessage(`[{"type":2,"data":{}}]`)

	h.handle(host, "mirror", "", batch)
	if sentHas(sent(jane), "mirror") {
		t.Error("the screen was relayed with Mirror everything off")
	}
	h.setMirror(true)
	sent(host)
	sent(jane)
	h.handle(host, "mirror", "", batch)
	if !sentHas(sent(jane), "mirror") {
		t.Error("the driving host's screen did not reach the guest")
	}
	h.handle(jane, "mirror", "", batch)
	if sentHas(sent(host), "mirror") {
		t.Error("a watching guest's screen was relayed")
	}
	// A viewer who lost their place asks; the driver is the one asked.
	h.handle(jane, "mirror-resync", "", nil)
	if !sentHas(sent(host), "mirror-resync") {
		t.Error("the driver was not asked for a fresh snapshot")
	}
	h.handle(jane, "mirror-resync", "", nil)
	if sentHas(sent(host), "mirror-resync") {
		t.Error("resync requests are not throttled")
	}
}

func TestTheHostSwitchesMirroring(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	sid := f.sess.ID
	post := func(body string) int {
		w := httptest.NewRecorder()
		r := f.asHost(httptest.NewRequest("POST", "/", strings.NewReader(body)))
		r.SetPathValue("sid", strconv.FormatInt(sid, 10))
		f.app.handleShareMirror(w, r)
		return w.Code
	}
	if code := post(`{"mirror":true}`); code != http.StatusOK {
		t.Fatalf("mirror on: %d", code)
	}
	if s, _ := f.app.store.GetShareSession(sid); !s.Mirror {
		t.Error("the switch was not saved")
	}
	if h := shareHubs.get(sid); h == nil || !h.mirror {
		t.Error("the live hub did not hear the switch")
	}
}

func TestASessionNeedsNoStack(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	w := httptest.NewRecorder()
	f.app.handleCreateAppShare(w, f.asHost(httptest.NewRequest("POST", "/api/share/sessions",
		strings.NewReader(`{"minutes":30,"mirror":true}`))))
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var out struct {
		Session ShareSession `json:"session"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	t.Cleanup(func() {
		if h := shareHubs.get(out.Session.ID); h != nil {
			h.end("ended")
		}
	})
	if out.Session.StackID != 0 || !out.Session.Mirror {
		t.Errorf("session: %+v", out.Session)
	}
	// A session started from a stack outlives the stack.
	if err := f.app.store.DeleteStack(f.stack.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.store.GetShareSession(f.sess.ID); err != nil {
		t.Errorf("deleting the stack took its session with it: %v", err)
	}
}

// An older database filed every session on a stack, NOT NULL; opening it rebuilds
// the table without losing a session or the guests that point at it.
func TestOldShareSessionsAreMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE stacks (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE share_sessions (id INTEGER PRIMARY KEY AUTOINCREMENT,
		  host_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		  stack_id INTEGER NOT NULL REFERENCES stacks(id) ON DELETE CASCADE,
		  token_hash TEXT NOT NULL, hide_secrets INTEGER NOT NULL DEFAULT 0,
		  created_at TEXT NOT NULL, expires_at TEXT NOT NULL, ended_at TEXT, ended_reason TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE share_guests (id INTEGER PRIMARY KEY AUTOINCREMENT,
		  session_id INTEGER NOT NULL REFERENCES share_sessions(id) ON DELETE CASCADE, name TEXT NOT NULL)`,
		`INSERT INTO users (id) VALUES (1)`,
		`INSERT INTO stacks (id) VALUES (7)`,
		`INSERT INTO share_sessions (host_id, stack_id, token_hash, created_at, expires_at) VALUES (1, 7, 'tok', 'a', 'b')`,
		`INSERT INTO share_guests (session_id, name) VALUES (1, 'Jane')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()

	db, _ = sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := migrateShareSessions(db); err != nil {
		t.Fatal(err)
	}
	if err := migrateShareSessions(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var stack sql.NullInt64
	var tok string
	if err := db.QueryRow(`SELECT stack_id, token_hash FROM share_sessions WHERE id = 1`).Scan(&stack, &tok); err != nil || stack.Int64 != 7 || tok != "tok" {
		t.Fatalf("session lost: %v %v %q", err, stack, tok)
	}
	if _, err := db.Exec(`INSERT INTO share_sessions (host_id, stack_id, token_hash, created_at, expires_at) VALUES (1, NULL, 't2', 'a', 'b')`); err != nil {
		t.Errorf("stack_id is still NOT NULL: %v", err)
	}
	var guests int
	db.QueryRow(`SELECT COUNT(*) FROM share_guests WHERE session_id = 1`).Scan(&guests)
	if guests != 1 {
		t.Error("the rebuild cascaded to the guests")
	}
	var ref string
	db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'share_guests'`).Scan(&ref)
	if !strings.Contains(ref, "REFERENCES share_sessions(id)") {
		t.Errorf("share_guests no longer points at share_sessions: %s", ref)
	}
}
