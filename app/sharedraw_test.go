package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// sharedraw_test.go — everyone draws, the host decides who may, and nobody but the
// host erases what someone else drew.

func drawClients(t *testing.T) (*shareFixture, *shareHub, *hubClient, *hubClient, ShareGuest) {
	t.Helper()
	f := newShareFixture(t, RoleUser)
	_, g := f.join(t, "Jane")
	f.admit(t, g)
	h := f.app.hubFor(f.sess)
	host := &hubClient{guestID: 0, name: "host", out: make(chan []byte, 64), cancel: func() {}}
	jane := &hubClient{guestID: g.ID, name: "Jane", out: make(chan []byte, 64), cancel: func() {}}
	h.mu.Lock()
	h.clients[host], h.clients[jane] = true, true
	h.mu.Unlock()
	return f, h, host, jane, g
}

func pen(id string) json.RawMessage {
	return json.RawMessage(`{"id":"` + id + `","kind":"pen","color":"#ef4444","width":0.004,"points":[[0.1,0.1],[0.2,0.25]]}`)
}

func TestEveryoneDrawsAndSeesIt(t *testing.T) {
	_, h, host, jane, g := drawClients(t)
	h.handle(jane, "draw", "", pen("a"))
	if !sentHas(sent(host), "draw") {
		t.Fatal("the host did not see the guest's line")
	}
	h.mu.Lock()
	m := h.marks["a"]
	h.mu.Unlock()
	if m == nil || m.By != g.ID || m.Name != "Jane" {
		t.Fatalf("the mark is not filed as Jane's: %+v", m)
	}
	// A line sent again as it grows is the same mark, not a second one.
	h.handle(jane, "draw", "", json.RawMessage(`{"id":"a","kind":"pen","color":"#ef4444","width":0.004,"points":[[0.1,0.1],[0.2,0.25],[0.3,0.3]]}`))
	h.mu.Lock()
	n, pts := len(h.markOrder), len(h.marks["a"].Points)
	h.mu.Unlock()
	if n != 1 || pts != 3 {
		t.Errorf("growing a line: %d marks, %d points", n, pts)
	}
	// Somebody else's id is not theirs to redraw.
	h.handle(host, "draw", "", pen("a"))
	h.mu.Lock()
	by := h.marks["a"].By
	h.mu.Unlock()
	if by != g.ID {
		t.Error("the host redrew a guest's mark under its id")
	}
	// Junk is refused: a colour off the palette, a point off the screen, empty text.
	for _, bad := range []string{
		`{"id":"b","kind":"pen","color":"red;background:url(x)","width":0.004,"points":[[0.1,0.1]]}`,
		`{"id":"b","kind":"pen","color":"#ef4444","width":0.004,"points":[[4,0.1]]}`,
		`{"id":"b","kind":"text","color":"#ef4444","size":0.03,"x":0.5,"y":0.5,"text":"  "}`,
		`{"id":"b","kind":"html","color":"#ef4444"}`,
	} {
		h.handle(jane, "draw", "", json.RawMessage(bad))
	}
	h.mu.Lock()
	_, got := h.marks["b"]
	h.mu.Unlock()
	if got {
		t.Error("an invalid mark was kept")
	}
	// A late arrival gets every mark with its hello.
	h.mu.Lock()
	list := h.markList()
	h.mu.Unlock()
	if len(list) != 1 {
		t.Errorf("hello would carry %d marks, want 1", len(list))
	}
}

func TestOnlyTheHostErasesOthers(t *testing.T) {
	_, h, host, jane, _ := drawClients(t)
	h.handle(host, "draw", "", pen("h1"))
	h.handle(jane, "draw", "", pen("j1"))
	h.handle(jane, "draw", "", pen("j2"))

	h.handle(jane, "draw-erase", "", json.RawMessage(`{"ids":["h1","j1"]}`))
	h.mu.Lock()
	_, h1 := h.marks["h1"]
	_, j1 := h.marks["j1"]
	h.mu.Unlock()
	if !h1 || j1 {
		t.Errorf("a guest's eraser: host's mark kept=%v, own mark kept=%v", h1, j1)
	}
	// A guest's "clear" is their own marks; the host's "clear all" is everything.
	h.handle(host, "draw", "", pen("h2"))
	h.handle(jane, "draw-clear", "", json.RawMessage(`{"all":true}`))
	h.mu.Lock()
	n := len(h.marks)
	h.mu.Unlock()
	if n != 2 {
		t.Errorf("a guest's clear left %d marks, want the host's 2", n)
	}
	h.handle(jane, "draw", "", pen("j3"))
	h.handle(host, "draw-erase", "", json.RawMessage(`{"ids":["j3"]}`))
	h.handle(host, "draw-clear", "", json.RawMessage(`{"all":true}`))
	h.mu.Lock()
	n = len(h.marks) + len(h.markOrder)
	h.mu.Unlock()
	if n != 0 {
		t.Error("the host's clear all left marks behind")
	}
}

func TestTheHostSaysWhoDraws(t *testing.T) {
	f, h, host, jane, g := drawClients(t)
	sid := strconv.FormatInt(f.sess.ID, 10)
	guestDraw := func(on bool) {
		w := httptest.NewRecorder()
		r := f.asHost(httptest.NewRequest("POST", "/", strings.NewReader(`{"draw":`+strconv.FormatBool(on)+`}`)))
		r.SetPathValue("sid", sid)
		r.SetPathValue("gid", strconv.FormatInt(g.ID, 10))
		f.app.handleShareGuestAction("draw")(f.app)(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("guest draw: %d %s", w.Code, w.Body)
		}
	}
	allDraw := func(on bool) {
		w := httptest.NewRecorder()
		r := f.asHost(httptest.NewRequest("POST", "/", strings.NewReader(`{"draw":`+strconv.FormatBool(on)+`}`)))
		r.SetPathValue("sid", sid)
		f.app.handleShareDraw(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("session draw: %d %s", w.Code, w.Body)
		}
	}
	has := func(id string) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.marks[id] != nil
	}

	guestDraw(false)
	h.handle(jane, "draw", "", pen("x"))
	if has("x") || !sentHas(sent(jane), "error") {
		t.Error("a guest the host stopped could still draw, or was not told")
	}
	if gg, _ := f.app.store.GetShareGuest(g.ID); !gg.DrawOff {
		t.Error("the guest's switch was not saved")
	}
	guestDraw(true)
	h.handle(jane, "draw", "", pen("x"))
	if !has("x") {
		t.Error("a guest let draw again could not")
	}

	allDraw(false)
	h.handle(jane, "draw", "", pen("y"))
	h.handle(jane, "draw-erase", "", json.RawMessage(`{"ids":["x"]}`))
	if has("y") || !has("x") {
		t.Error("with drawing off for guests a guest still drew or erased")
	}
	h.handle(host, "draw", "", pen("z"))
	if !has("z") {
		t.Error("the host always draws")
	}
	if s, _ := f.app.store.GetShareSession(f.sess.ID); s.GuestsDraw {
		t.Error("the session switch was not saved")
	}
	if p := h.presence(); p["guestsDraw"] != false {
		t.Errorf("presence does not say guests cannot draw: %v", p["guestsDraw"])
	}
	_ = host
}
