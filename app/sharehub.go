package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// sharehub.go — the live half of a shared session.
//
// One hub per session, in memory, and one websocket per browser in it. The hub is a
// relay with a memory: it fans out where the driver is (follow), their pointer
// (cursor), chat and events, and it remembers the few things a browser arriving late
// needs — who is here, who drives, the last follow state, the last of the chat, the
// shared terminals. What must outlive the process is written to SQLite as it passes
// (share_store.go); the rest is rebuilt from nothing after a restart, which costs a
// reconnect and nothing else.
//
// Control is the hub's one piece of authority: controller is 0 for the host or the
// id of the guest who drives, and serveGuest reads it on every guest request.

// shareDriverGrace is how long a driver who drops keeps control. A var so a test can
// shorten it.
var shareDriverGrace = 30 * time.Second

const (
	shareHistory       = 200         // chat lines a browser gets on arrival
	shareChatInterval  = time.Second // one chat line a second per guest
	shareCursorMaxRate = 50 * time.Millisecond
)

type shareRegistry struct {
	mu   sync.Mutex
	hubs map[int64]*shareHub
}

var shareHubs = &shareRegistry{hubs: map[int64]*shareHub{}}

func (reg *shareRegistry) get(id int64) *shareHub {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.hubs[id]
}

// controller is who drives a session: 0 is the host, and also the answer for a
// session with no hub, which is a session nobody is in.
func (reg *shareRegistry) controller(id int64) int64 {
	if h := reg.get(id); h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.controller
	}
	return 0
}

func (reg *shareRegistry) drop(id int64) {
	reg.mu.Lock()
	delete(reg.hubs, id)
	reg.mu.Unlock()
}

// hubFor returns the session's hub, starting one if it has none.
func (a *App) hubFor(sess ShareSession) *shareHub {
	a.startShareReaper()
	shareHubs.mu.Lock()
	defer shareHubs.mu.Unlock()
	if h := shareHubs.hubs[sess.ID]; h != nil {
		return h
	}
	h := &shareHub{app: a, sess: sess, clients: map[*hubClient]bool{}, terms: map[string]*sharedTerm{},
		browsers: map[string]*sharedBrowser{}, lastChat: map[int64]time.Time{}}
	shareHubs.hubs[sess.ID] = h
	h.armTimers()
	return h
}

type shareHub struct {
	app  *App
	sess ShareSession

	mu         sync.Mutex
	clients    map[*hubClient]bool
	controller int64
	lastFollow json.RawMessage
	terms      map[string]*sharedTerm
	termSeq    int
	// browsers are the shared browser windows (browse.go): opened by the driver or
	// the host, shown to everyone, and kept at the path the driver last had.
	browsers   map[string]*sharedBrowser
	timers     []*time.Timer
	driverLost *time.Timer
	lastChat   map[int64]time.Time
	ended      bool
}

// sharedBrowser is a browser window everyone in the session sees. Link is the node
// link it was opened from; every browser resolves it for itself (POST /api/browse),
// because what each one may do with it — a watcher's view-only VNC — differs.
type sharedBrowser struct {
	ID     string `json:"id"`
	Link   string `json:"link"`
	Title  string `json:"title"`
	Path   string `json:"path"`
	Opener string `json:"opener"`
}

// hubClient is one browser in the session.
type hubClient struct {
	guestID int64 // 0 = the host
	name    string
	out     chan []byte
	cancel  context.CancelFunc
	lastCur time.Time
}

func (c *hubClient) isHost() bool { return c.guestID == 0 }

// armTimers schedules the two warnings and the end itself.
func (h *shareHub) armTimers() {
	exp := h.sess.expires()
	for _, w := range []struct {
		before time.Duration
		text   string
	}{{10 * time.Minute, "This session ends in 10 minutes"}, {2 * time.Minute, "This session ends in 2 minutes"}} {
		if d := time.Until(exp.Add(-w.before)); d > 0 {
			text := w.text
			h.timers = append(h.timers, time.AfterFunc(d, func() { h.event("expiry-warning", 0, "", text) }))
		}
	}
	left := time.Until(exp)
	if left < 0 {
		left = 0
	}
	h.timers = append(h.timers, time.AfterFunc(left, func() { h.end("expired") }))
}

func (h *shareHub) send(c *hubClient, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	select {
	case c.out <- b:
	default: // a browser that cannot keep up is dropped rather than slowing the room
		c.cancel()
	}
}

// broadcast sends to every browser in the session except `skip`. Callers hold no lock.
func (h *shareHub) broadcast(v any, skip *hubClient) {
	h.mu.Lock()
	cs := make([]*hubClient, 0, len(h.clients))
	for c := range h.clients {
		if c != skip {
			cs = append(cs, c)
		}
	}
	h.mu.Unlock()
	for _, c := range cs {
		h.send(c, v)
	}
}

// event writes a system line to the transcript and shows it to everyone.
func (h *shareHub) event(kind string, guestID int64, author, body string) int64 {
	authorKind := "system"
	if guestID != 0 {
		authorKind = "guest"
	}
	m, err := h.app.store.AddShareMessage(ShareMessage{SessionID: h.sess.ID, AuthorKind: authorKind,
		GuestID: guestID, Author: author, Kind: kind, Body: body})
	if err != nil {
		return 0
	}
	h.broadcast(map[string]any{"t": "message", "message": m}, nil)
	return m.ID
}

// presence is who is here, as every browser draws it.
func (h *shareHub) presence() map[string]any {
	guests, _ := h.app.store.ListShareGuests(h.sess.ID)
	h.mu.Lock()
	online := map[int64]bool{}
	for c := range h.clients {
		online[c.guestID] = true
	}
	controller := h.controller
	h.mu.Unlock()
	type person struct {
		ShareGuest
		Online bool `json:"online"`
	}
	people := []person{}
	for _, g := range guests {
		if g.State == guestWaiting || g.State == guestAdmitted {
			people = append(people, person{g, online[g.ID]})
		}
	}
	return map[string]any{
		"t": "presence", "host": map[string]any{"name": h.sess.HostName, "online": online[0]},
		"guests": people, "controller": controller, "expiresAt": h.sess.ExpiresAt, "hideSecrets": h.sess.HideSecrets,
	}
}

func (h *shareHub) broadcastPresence() { h.broadcast(h.presence(), nil) }

func (h *shareHub) nameOf(guestID int64) string {
	if guestID == 0 {
		return h.sess.HostName
	}
	if g, err := h.app.store.GetShareGuest(guestID); err == nil {
		return g.Name
	}
	return "a guest"
}

// setController hands control over. Anything in flight from the old driver — a
// keystroke into a shared terminal — is refused from this moment.
func (h *shareHub) setController(to int64) {
	h.mu.Lock()
	if h.ended || h.controller == to {
		h.mu.Unlock()
		return
	}
	h.controller = to
	if h.driverLost != nil {
		h.driverLost.Stop()
		h.driverLost = nil
	}
	h.mu.Unlock()
	if to == 0 {
		h.event("control", 0, "", h.sess.HostName+" has control")
	} else {
		h.event("control", to, h.nameOf(to), h.sess.HostName+" gave control to "+h.nameOf(to))
	}
	h.broadcastPresence()
}

// dropGuest closes every browser a guest has in the session, and takes control back
// if they held it. Used when they are removed or leave.
func (h *shareHub) dropGuest(guestID int64) {
	h.mu.Lock()
	for c := range h.clients {
		if c.guestID == guestID {
			c.cancel()
		}
	}
	driving := h.controller == guestID
	h.mu.Unlock()
	if driving {
		h.setController(0)
	}
}

// end closes the session for everyone: the row, the sockets, the shared terminals.
func (h *shareHub) end(reason string) {
	h.mu.Lock()
	if h.ended {
		h.mu.Unlock()
		return
	}
	h.ended = true
	for _, t := range h.timers {
		t.Stop()
	}
	if h.driverLost != nil {
		h.driverLost.Stop()
	}
	terms := make([]*sharedTerm, 0, len(h.terms))
	for _, t := range h.terms {
		terms = append(terms, t)
	}
	h.mu.Unlock()

	h.app.store.EndShareSession(h.sess.ID, reason)
	text := map[string]string{"expired": "The session reached its time limit and ended",
		"ended": "The host ended the session", "revoked": "The host revoked the link"}[reason]
	if text == "" {
		text = "The session ended"
	}
	h.event("end", 0, "", text)
	h.broadcast(map[string]any{"t": "end", "reason": reason}, nil)
	for _, t := range terms {
		t.close()
	}
	// Give the end message a moment to reach the browsers before the sockets go.
	time.AfterFunc(500*time.Millisecond, func() {
		h.mu.Lock()
		for c := range h.clients {
			c.cancel()
		}
		h.mu.Unlock()
	})
	shareHubs.drop(h.sess.ID)
}

// clientGone runs when a browser leaves. A driver whose last browser went keeps
// control for a grace period — a reload, a flaky Wi-Fi — and then loses it.
func (h *shareHub) clientGone(c *hubClient) {
	h.mu.Lock()
	delete(h.clients, c)
	still := false
	for o := range h.clients {
		if o.guestID == c.guestID {
			still = true
		}
	}
	if !still && c.guestID != 0 && h.controller == c.guestID && h.driverLost == nil && !h.ended {
		gid := c.guestID
		h.driverLost = time.AfterFunc(shareDriverGrace, func() {
			h.mu.Lock()
			h.driverLost = nil
			lost := h.controller == gid
			for o := range h.clients {
				if o.guestID == gid {
					lost = false
				}
			}
			h.mu.Unlock()
			if lost {
				h.event("control", gid, h.nameOf(gid), h.nameOf(gid)+" disconnected; control returned to "+h.sess.HostName)
				h.setController(0)
			}
		})
	}
	h.mu.Unlock()
	h.broadcastPresence()
}

// ------------------------------------------------------------- the socket

// shareCaller works out who is opening a session socket or terminal: its host, on
// their own cookie, or an admitted guest of this very session.
func (a *App) shareCaller(w http.ResponseWriter, r *http.Request) (ShareSession, *hubClient, bool) {
	sid, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return ShareSession{}, nil, false
	}
	sess, err := a.store.GetShareSession(sid)
	if err != nil || !sess.live(time.Now()) {
		writeErr(w, http.StatusNotFound, "this session has ended")
		return ShareSession{}, nil, false
	}
	if p, ok := principalOf(r); ok && p.Guest != nil {
		if p.Guest.SessionID != sess.ID {
			writeErr(w, http.StatusForbidden, "you were not admitted to this session")
			return ShareSession{}, nil, false
		}
		return sess, &hubClient{guestID: p.Guest.GuestID, name: p.Guest.Name}, true
	}
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return ShareSession{}, nil, false
	}
	if u.ID != sess.HostID {
		writeErr(w, http.StatusForbidden, "this is not your session")
		return ShareSession{}, nil, false
	}
	return sess, &hubClient{guestID: 0, name: u.Username}, true
}

// handleShareWS is a browser joining the session's live channel.
func (a *App) handleShareWS(w http.ResponseWriter, r *http.Request) {
	sess, c, ok := a.shareCaller(w, r)
	if !ok {
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 10)

	h := a.hubFor(sess)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.out = make(chan []byte, 256)
	c.cancel = cancel

	h.mu.Lock()
	if h.ended {
		h.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "ended")
		return
	}
	h.clients[c] = true
	if h.driverLost != nil && h.controller == c.guestID {
		h.driverLost.Stop() // the driver is back in time
		h.driverLost = nil
	}
	follow := h.lastFollow
	terms := h.termList()
	browsers := make([]*sharedBrowser, 0, len(h.browsers))
	for _, b := range h.browsers {
		browsers = append(browsers, b)
	}
	h.mu.Unlock()
	defer h.clientGone(c)

	history, _ := a.store.ListShareMessages(sess.ID, shareHistory)
	h.send(c, map[string]any{"t": "hello", "you": map[string]any{"guestId": c.guestID, "name": c.name, "host": c.isHost()},
		"session": sess, "history": history, "follow": follow, "terms": terms, "browsers": browsers})
	h.broadcastPresence()

	// writer
	go func() {
		defer cancel()
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-c.out:
				wctx, wc := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(wctx, websocket.MessageText, b)
				wc()
				if err != nil {
					return
				}
			case <-ping.C:
				pctx, pc := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pctx)
				pc()
				if err != nil {
					return
				}
			}
		}
	}()

	// reader
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg struct {
			T    string          `json:"t"`
			Body string          `json:"body"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		h.handle(c, msg.T, msg.Body, msg.Data)
	}
}

// handle is one message from a browser.
func (h *shareHub) handle(c *hubClient, t, body string, data json.RawMessage) {
	h.mu.Lock()
	driving := h.controller == c.guestID
	h.mu.Unlock()
	switch t {
	case "chat":
		body = strings.TrimSpace(body)
		if body == "" {
			return
		}
		if len([]rune(body)) > shareChatMax {
			h.send(c, map[string]any{"t": "error", "error": "a message is at most 2,000 characters"})
			return
		}
		authorKind := "host"
		if !c.isHost() {
			authorKind = "guest"
			g, err := h.app.store.GetShareGuest(c.guestID)
			if err != nil || g.State != guestAdmitted {
				return
			}
			if g.Muted {
				h.send(c, map[string]any{"t": "error", "error": "the host has muted you"})
				return
			}
			h.mu.Lock()
			last := h.lastChat[c.guestID]
			ok := time.Since(last) >= shareChatInterval
			if ok {
				h.lastChat[c.guestID] = time.Now()
			}
			h.mu.Unlock()
			if !ok {
				h.send(c, map[string]any{"t": "error", "error": "slow down — one message a second"})
				return
			}
		}
		m, err := h.app.store.AddShareMessage(ShareMessage{SessionID: h.sess.ID, AuthorKind: authorKind,
			GuestID: c.guestID, Author: c.name, Kind: "chat", Body: body})
		if err == nil {
			h.broadcast(map[string]any{"t": "message", "message": m}, nil)
		}
	case "follow":
		// Only the driver says where everyone is; the host too, while they drive.
		if !driving || len(data) == 0 || len(data) > 16<<10 {
			return
		}
		h.mu.Lock()
		h.lastFollow = data
		h.mu.Unlock()
		h.broadcast(map[string]any{"t": "follow", "data": data, "from": c.guestID}, c)
	case "cursor":
		if !driving || len(data) > 1024 || time.Since(c.lastCur) < shareCursorMaxRate {
			return
		}
		c.lastCur = time.Now()
		h.broadcast(map[string]any{"t": "cursor", "data": data, "from": c.guestID}, c)
	case "invalidate":
		// The driver changed something on the server; everyone else re-reads.
		if !driving || len(data) > 4096 {
			return
		}
		h.broadcast(map[string]any{"t": "invalidate", "data": data}, c)
	case "browser-open", "browser-nav", "browser-close":
		// A window is shared when the driver or the host opens it; after that only the
		// driver moves it or closes it — a viewer, the host included while someone else
		// drives, cannot take a window off everyone's screen.
		if !driving && !(c.isHost() && t == "browser-open") {
			return
		}
		var b sharedBrowser
		if json.Unmarshal(data, &b) != nil || b.ID == "" || len(b.ID) > 64 || len(b.Link) > 2048 || len(b.Path) > 4096 {
			return
		}
		h.mu.Lock()
		switch t {
		case "browser-open":
			if len(h.browsers) >= 16 {
				h.mu.Unlock()
				return
			}
			b.Opener = c.name
			if len(b.Title) > 200 {
				b.Title = b.Title[:200]
			}
			h.browsers[b.ID] = &b
		case "browser-nav":
			if cur := h.browsers[b.ID]; cur != nil {
				cur.Path = b.Path
			} else {
				h.mu.Unlock()
				return
			}
		case "browser-close":
			delete(h.browsers, b.ID)
		}
		h.mu.Unlock()
		if t == "browser-open" {
			h.event("browser", c.guestID, c.name, c.name+" opened "+b.Title+" in a browser window")
		}
		h.broadcast(map[string]any{"t": t, "browser": b}, c)
	case "control-request":
		if c.isHost() || driving {
			return
		}
		h.event("control", c.guestID, c.name, c.name+" asked for control")
		h.broadcast(map[string]any{"t": "control-request", "guestId": c.guestID, "name": c.name}, nil)
	case "control-release":
		if !c.isHost() && driving {
			h.event("control", c.guestID, c.name, c.name+" handed control back")
			h.setController(0)
		}
	case "control-take":
		if c.isHost() {
			h.setController(0)
		}
	}
}
