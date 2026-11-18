package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	f := newShareFixture(t)
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
	f := newShareFixture(t)
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
	f := newShareFixture(t)
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
