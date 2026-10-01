package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// share_test.go — the guarantees shared sessions make: nobody gets in without the
// host's admit, a watcher changes nothing, a driver changes anything but the
// account, the host's admin rights never leak, and the clock is the server's.

// shareFixture is an app with sessions on, a host, a stack, and a session.
type shareFixture struct {
	app   *App
	host  User
	stack Stack
	sess  ShareSession
	token string
}

func newShareFixture(t *testing.T, role string) *shareFixture {
	t.Helper()
	app := newTestApp(t)
	app.store.SetAppSetting(settingAllowGuestSessions, "1")
	// The join limiter is process-wide; each test starts from a clean slate so the
	// suite's many joins do not trip it.
	joinLimiter.Lock()
	joinLimiter.seen = map[string][]time.Time{}
	joinLimiter.Unlock()
	host, err := app.store.CreateUser("host", "x", role, StatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	st, err := app.store.CreateStack("lab", host.ID, ttlInfinity, nil, []byte(defaultDesign))
	if err != nil {
		t.Fatal(err)
	}
	f := &shareFixture{app: app, host: host, stack: st}
	w := httptest.NewRecorder()
	r := f.asHost(httptest.NewRequest("POST", "/api/stacks/"+strconv.FormatInt(st.ID, 10)+"/share", strings.NewReader(`{"minutes":30}`)))
	r.SetPathValue("id", strconv.FormatInt(st.ID, 10))
	app.handleCreateShare(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("create share: %d %s", w.Code, w.Body)
	}
	var out struct {
		Session ShareSession `json:"session"`
		URL     string       `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	f.sess = out.Session
	f.token = out.URL[strings.LastIndex(out.URL, "/")+1:]
	t.Cleanup(func() {
		if h := shareHubs.get(f.sess.ID); h != nil {
			h.end("ended")
		}
	})
	return f
}

func (f *shareFixture) asHost(r *http.Request) *http.Request {
	return withPrincipal(r, principal{User: f.host})
}

// join puts a guest in the lobby and returns their cookie.
func (f *shareFixture) join(t *testing.T, name string) (*http.Cookie, ShareGuest) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/join/"+f.token, strings.NewReader(`{"name":"`+name+`","email":"`+strings.ToLower(name)+`@example.com"}`))
	r.SetPathValue("token", f.token)
	r.RemoteAddr = "10.0.0." + strconv.Itoa(len(name)) + ":5555"
	f.app.handleJoin(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	var g ShareGuest
	json.Unmarshal(w.Body.Bytes(), &g)
	for _, c := range w.Result().Cookies() {
		if c.Name == guestCookieName {
			return c, g
		}
	}
	t.Fatal("join set no guest cookie")
	return nil, g
}

func (f *shareFixture) admit(t *testing.T, g ShareGuest) {
	t.Helper()
	w := httptest.NewRecorder()
	r := f.asHost(httptest.NewRequest("POST", "/", nil))
	r.SetPathValue("sid", strconv.FormatInt(f.sess.ID, 10))
	r.SetPathValue("gid", strconv.FormatInt(g.ID, 10))
	f.app.handleShareGuestAction("admit")(f.app)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("admit: %d %s", w.Code, w.Body)
	}
}

// call runs a route of the real table, as a guest, through requireScope, with a
// handler that just says it was reached.
func (f *shareFixture) call(t *testing.T, pattern string, c *http.Cookie) (int, bool) {
	t.Helper()
	var rt apiRoute
	for _, x := range apiRoutes() {
		if x.Pattern() == pattern {
			rt = x
		}
	}
	if rt.Path == "" {
		t.Fatalf("no route %s", pattern)
	}
	reached := false
	path := strings.ReplaceAll(rt.Path, "{id}", strconv.FormatInt(f.stack.ID, 10))
	r := httptest.NewRequest(rt.Method, path, nil)
	r.Header.Set(guestHeader, "1")
	if c != nil {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	f.app.requireScope(rt, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if p, ok := principalOf(r); !ok || p.Guest == nil || p.User.ID != f.host.ID || p.User.Role == RoleAdmin {
			t.Errorf("%s: a guest request must carry the host, never as admin: %+v", pattern, p)
		}
		w.WriteHeader(http.StatusOK)
	})(w, r)
	return w.Code, reached
}

func TestGuestNeedsTheHostsAdmit(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	c, g := f.join(t, "Jane")
	if code, reached := f.call(t, "GET /api/stacks", c); code != http.StatusUnauthorized || reached {
		t.Errorf("a guest in the lobby reached the API: %d", code)
	}
	if code, _ := f.call(t, "GET /api/stacks", nil); code != http.StatusUnauthorized {
		t.Errorf("no cookie at all: %d, want 401", code)
	}
	f.admit(t, g)
	if code, reached := f.call(t, "GET /api/stacks", c); code != http.StatusOK || !reached {
		t.Errorf("an admitted guest could not read: %d", code)
	}
}

func TestWatcherReadsButNeverWrites(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	c, g := f.join(t, "Jane")
	f.admit(t, g)
	for _, p := range []string{"POST /api/stacks/{id}/deploy", "DELETE /api/stacks/{id}", "PUT /api/stacks/{id}",
		"GET /api/stacks/{id}/nodes/{nid}/term"} {
		if code, reached := f.call(t, p, c); code != http.StatusForbidden || reached {
			t.Errorf("a watcher reached %s: %d", p, code)
		}
	}
	// A read-only POST is a read.
	if code, _ := f.call(t, "POST /api/stacks/{id}/validate", c); code != http.StatusOK {
		t.Errorf("a watcher could not validate: %d", code)
	}
}

// A watcher may look at the shared desktop, not put pages on it; the driver may.
func TestOnlyTheDriverOpensPagesOnTheDesktop(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	c, g := f.join(t, "Jane")
	f.admit(t, g)
	if code, reached := f.call(t, "POST /api/browse/desktop", c); code != http.StatusForbidden || reached {
		t.Errorf("a watcher opened a page on the desktop: %d", code)
	}
	if code, _ := f.call(t, "POST /api/browse", c); code != http.StatusOK {
		t.Errorf("a watcher could not open a browser window: %d", code)
	}
	f.app.hubFor(f.sess).setController(g.ID)
	if code, reached := f.call(t, "POST /api/browse/desktop", c); code != http.StatusOK || !reached {
		t.Errorf("the driver could not open a page on the desktop: %d", code)
	}
}

func TestDriverWritesAndIsAudited(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	c, g := f.join(t, "Jane")
	f.admit(t, g)
	f.app.hubFor(f.sess).setController(g.ID)
	if code, reached := f.call(t, "POST /api/stacks/{id}/deploy", c); code != http.StatusOK || !reached {
		t.Fatalf("the driver could not deploy: %d", code)
	}
	if code, _ := f.call(t, "GET /api/stacks/{id}/nodes/{nid}/term", c); code != http.StatusOK {
		t.Errorf("the driver could not open a terminal: %d", code)
	}
	acts, _ := f.app.store.ListShareActions(f.sess.ID)
	if len(acts) != 2 || acts[0].GuestID != g.ID || acts[0].Method != "POST" || acts[0].Status != http.StatusOK {
		t.Errorf("guest writes were not audited: %+v", acts)
	}
	msgs, _ := f.app.store.ListShareMessages(f.sess.ID, 0)
	found := false
	for _, m := range msgs {
		if m.Kind == "action" && m.Author == "Jane" {
			found = true
		}
	}
	if !found {
		t.Error("a guest's write did not appear in the transcript")
	}
	// Taking control back ends it.
	f.app.hubFor(f.sess).setController(0)
	if code, _ := f.call(t, "POST /api/stacks/{id}/deploy", c); code != http.StatusForbidden {
		t.Errorf("control taken back, still writing: %d", code)
	}
}

func TestSomeThingsNoGuestReaches(t *testing.T) {
	f := newShareFixture(t, RoleAdmin) // an admin host: nothing of admin may leak
	c, g := f.join(t, "Jane")
	f.admit(t, g)
	f.app.hubFor(f.sess).setController(g.ID) // even while driving
	for _, p := range []string{
		"GET /api/tokens", "POST /api/tokens", "DELETE /api/tokens/{id}",
		"GET /api/users", "GET /api/admin/tokens", "PUT /api/system/settings",
		"PUT /api/me/settings", "POST /api/me/password", "POST /api/auth/logout", "POST /api/auth/login",
		"GET /api/notifications", "GET /api/notifications/stream",
		"POST /api/stacks/{id}/share", "POST /api/share/sessions/{sid}/end",
		"POST /api/share/sessions/{sid}/control", "POST /api/share/sessions/{sid}/guests/{gid}/admit",
		"GET /api/share/sessions",
	} {
		if code, reached := f.call(t, p, c); reached || (code != http.StatusForbidden && code != http.StatusOK) {
			t.Errorf("%s: reached=%v code=%d, want refused", p, reached, code)
		} else if reached {
			t.Errorf("%s was reached by a guest", p)
		}
	}
	// What a guest does need still works.
	for _, p := range []string{"GET /api/share/sessions/{sid}/ws", "GET /api/setup/status", "GET /api/me", "GET /api/system/settings"} {
		if code, reached := f.call(t, p, c); !reached {
			t.Errorf("%s: a guest needs this and got %d", p, code)
		}
	}
}

func TestSessionLengthIsCapped(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	try := func(minutes int) int {
		w := httptest.NewRecorder()
		r := f.asHost(httptest.NewRequest("POST", "/", strings.NewReader(`{"minutes":`+strconv.Itoa(minutes)+`}`)))
		r.SetPathValue("id", strconv.FormatInt(f.stack.ID, 10))
		f.app.handleCreateShare(w, r)
		return w.Code
	}
	if code := try(121); code != http.StatusBadRequest {
		t.Errorf("121 minutes: %d, want 400", code)
	}
	if code := try(120); code != http.StatusOK {
		t.Errorf("120 minutes: %d, want 200", code)
	}
	f.app.store.SetAppSetting(settingMaxGuestMinutes, "30")
	if code := try(31); code != http.StatusBadRequest {
		t.Errorf("31 minutes under a 30-minute cap: %d, want 400", code)
	}
	f.app.store.SetAppSetting(settingAllowGuestSessions, "0")
	if code := try(10); code != http.StatusForbidden {
		t.Errorf("sessions switched off: %d, want 403", code)
	}
}

func TestExpiryIsTheServersClock(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	c, g := f.join(t, "Jane")
	f.admit(t, g)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	f.app.store.db.Exec(`UPDATE share_sessions SET expires_at = ? WHERE id = ?`, past, f.sess.ID)
	if code, reached := f.call(t, "GET /api/stacks", c); code != http.StatusUnauthorized || reached {
		t.Errorf("an expired session still answered: %d", code)
	}
	// And the link no longer opens.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("token", f.token)
	f.app.handleJoinInfo(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("an expired link: %d, want 404", w.Code)
	}
}

func TestSwitchingSessionsOffShutsGuestsOut(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	c, g := f.join(t, "Jane")
	f.admit(t, g)
	f.app.store.SetAppSetting(settingAllowGuestSessions, "0")
	if code, _ := f.call(t, "GET /api/stacks", c); code != http.StatusUnauthorized {
		t.Errorf("sessions off, guest still in: %d", code)
	}
}

func TestJoinValidatesAndRateLimits(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	post := func(body, addr string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.SetPathValue("token", f.token)
		r.RemoteAddr = addr
		f.app.handleJoin(w, r)
		return w.Code
	}
	if code := post(`{"name":"","email":"a@b.c"}`, "10.9.9.1:1"); code != http.StatusBadRequest {
		t.Errorf("no name: %d", code)
	}
	if code := post(`{"name":"A","email":"not an email"}`, "10.9.9.2:1"); code != http.StatusBadRequest {
		t.Errorf("bad email: %d", code)
	}
	codes := 0
	for i := 0; i < 12; i++ {
		if post(`{"name":"A","email":"a@b.c"}`, "10.9.9.3:1") == http.StatusTooManyRequests {
			codes++
		}
	}
	if codes == 0 {
		t.Error("twelve joins a minute from one address were all accepted")
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"A","email":"a@b.c"}`))
	r.SetPathValue("token", "not-a-real-token")
	f.app.handleJoin(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("an unknown link: %d, want 404", w.Code)
	}
}

func TestChatLimits(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	_, g := f.join(t, "Jane")
	f.admit(t, g)
	h := f.app.hubFor(f.sess)
	c := &hubClient{guestID: g.ID, name: g.Name, out: make(chan []byte, 16), cancel: func() {}}
	h.handle(c, "chat", strings.Repeat("x", shareChatMax+1), nil)
	h.handle(c, "chat", "<script>alert(1)</script>", nil)
	h.handle(c, "chat", "too soon", nil) // inside the one-second window
	msgs, _ := f.app.store.ListShareMessages(f.sess.ID, 0)
	var chats []string
	for _, m := range msgs {
		if m.Kind == "chat" {
			chats = append(chats, m.Body)
		}
	}
	if len(chats) != 1 || chats[0] != "<script>alert(1)</script>" {
		t.Errorf("chat kept %q; want only the second message, stored verbatim (the UI renders text, never HTML)", chats)
	}
	f.app.store.SetShareGuestMuted(g.ID, true)
	time.Sleep(shareChatInterval)
	h.handle(c, "chat", "muted now", nil)
	if n := len(chats); n != 1 {
		t.Fatal("unexpected")
	}
	msgs, _ = f.app.store.ListShareMessages(f.sess.ID, 0)
	for _, m := range msgs {
		if m.Body == "muted now" {
			t.Error("a muted guest's message was kept")
		}
	}
}

func TestOnlyTheDriverSteers(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	_, g := f.join(t, "Jane")
	f.admit(t, g)
	h := f.app.hubFor(f.sess)
	jane := &hubClient{guestID: g.ID, name: "Jane", out: make(chan []byte, 16), cancel: func() {}}
	h.handle(jane, "follow", "", json.RawMessage(`{"page":"benchmark"}`))
	if h.lastFollow != nil {
		t.Error("a watching guest moved everyone")
	}
	host := &hubClient{guestID: 0, name: "host", out: make(chan []byte, 16), cancel: func() {}}
	h.handle(host, "follow", "", json.RawMessage(`{"page":"stack-designer"}`))
	if string(h.lastFollow) != `{"page":"stack-designer"}` {
		t.Errorf("the driving host did not steer: %s", h.lastFollow)
	}
	// A guest cannot hand themselves control.
	h.handle(jane, "control-take", "", nil)
	h.handle(jane, "control-release", "", nil)
	if shareHubs.controller(f.sess.ID) != 0 {
		t.Error("a guest took control without the host")
	}
}

func TestHiddenSecretsNeverReachAGuest(t *testing.T) {
	in := map[string]any{"deployments": []any{map[string]any{"nodeId": "ps-01",
		"secrets": map[string]any{"superUser": "postgres", "superPassword": "hunter2"}}},
		"design": map[string]any{"nodes": []any{map[string]any{"id": "n1", "rootPassword": "pw", "label": "ps-01"}}}}
	out, _ := json.Marshal(scrubJSON(in, false))
	for _, s := range []string{"hunter2", "\"pw\"", "postgres"} {
		if bytes.Contains(out, []byte(s)) {
			t.Errorf("%s survived the scrub: %s", s, out)
		}
	}
	if !bytes.Contains(out, []byte("ps-01")) {
		t.Errorf("the scrub took more than credentials: %s", out)
	}

	// A guest's save of a masked design keeps the real value, even with nodes reordered.
	stored := map[string]any{"nodes": []any{
		map[string]any{"id": "a", "rootPassword": "pa"}, map[string]any{"id": "b", "rootPassword": "pb"}}}
	incoming := map[string]any{"nodes": []any{
		map[string]any{"id": "b", "rootPassword": secretMask}, map[string]any{"id": "a", "rootPassword": "changed"}}}
	got, _ := json.Marshal(unmaskJSON(incoming, stored))
	if string(got) != `{"nodes":[{"id":"b","rootPassword":"pb"},{"id":"a","rootPassword":"changed"}]}` {
		t.Errorf("unmask: %s", got)
	}
}

func TestLeavingDropsControl(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	c, g := f.join(t, "Jane")
	f.admit(t, g)
	f.app.hubFor(f.sess).setController(g.ID)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("token", f.token)
	r.AddCookie(c)
	f.app.handleJoinLeave(w, r)
	if shareHubs.controller(f.sess.ID) != 0 {
		t.Error("a guest who left still has control")
	}
	if code, _ := f.call(t, "GET /api/stacks", c); code != http.StatusUnauthorized {
		t.Errorf("a guest who left still reads: %d", code)
	}
}

// joinStatus is what the guest's page asks on /join/<token>: its state, or the HTTP
// code when there is none.
func (f *shareFixture) joinStatus(t *testing.T, token string, c *http.Cookie) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/join/"+token+"/status", nil)
	r.SetPathValue("token", token)
	if c != nil {
		r.AddCookie(c)
	}
	f.app.handleJoinStatus(w, r)
	var out struct {
		State string `json:"state"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.State
}

// The guest's browser marks every API call as a guest's, Leave included. A watcher
// may not write, but leaving is not a write to the host's workspace: it must work.
func TestAWatcherCanLeave(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	c, g := f.join(t, "Jane")
	f.admit(t, g)
	var rt apiRoute
	for _, x := range apiRoutes() {
		if x.Pattern() == "POST /api/join/{token}/leave" {
			rt = x
		}
	}
	r := httptest.NewRequest("POST", "/api/join/"+f.token+"/leave", nil)
	r.SetPathValue("token", f.token)
	r.Header.Set(guestHeader, "1")
	r.AddCookie(c)
	w := httptest.NewRecorder()
	f.app.requireScope(rt, f.app.handleJoinLeave)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("a watcher could not leave: %d %s", w.Code, w.Body)
	}
	if got, _ := f.app.store.GetShareGuest(g.ID); got.State != guestLeft {
		t.Errorf("after Leave the guest is %q, want left", got.State)
	}
}

func TestLeavingNeedsANewInvitation(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	c, g := f.join(t, "Jane")
	f.admit(t, g)
	other, og := f.join(t, "Lee")
	f.admit(t, og)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("token", f.token)
	r.AddCookie(c)
	f.app.handleJoinLeave(w, r)
	for _, k := range w.Result().Cookies() {
		if k.Name == guestCookieName && k.MaxAge < 0 {
			t.Fatal("leaving forgot the cookie, so the same link would open the join form again")
		}
	}
	if code, state := f.joinStatus(t, f.token, c); code != http.StatusOK || state != guestLeft {
		t.Fatalf("after leaving, the old link shows %d %q, want the left screen", code, state)
	}

	// Asking to join again on the same link is refused.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/join/"+f.token, strings.NewReader(`{"name":"Jane","email":"jane@example.com"}`))
	r.SetPathValue("token", f.token)
	r.RemoteAddr = "10.0.0.4:5555"
	r.AddCookie(c)
	f.app.handleJoin(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a guest who left rejoined on the same link: %d %s", w.Code, w.Body)
	}

	// The host issues a new link: the old one is dead for newcomers...
	w = httptest.NewRecorder()
	r = f.asHost(httptest.NewRequest("POST", "/", nil))
	r.SetPathValue("sid", strconv.FormatInt(f.sess.ID, 10))
	f.app.handleShareNewLink(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("new link: %d %s", w.Code, w.Body)
	}
	var out struct {
		URL string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	fresh := out.URL[strings.LastIndex(out.URL, "/")+1:]
	if fresh == f.token {
		t.Fatal("the new link is the old one")
	}
	if code, _ := f.joinStatus(t, f.token, nil); code != http.StatusNotFound {
		t.Errorf("the replaced link still answers a newcomer: %d", code)
	}
	// ...but a guest already through it keeps their place on reload.
	if code, state := f.joinStatus(t, f.token, other); code != http.StatusOK || state != guestAdmitted {
		t.Errorf("an admitted guest lost their place when the link was replaced: %d %q", code, state)
	}
	if code, _ := f.call(t, "GET /api/stacks", other); code != http.StatusOK {
		t.Errorf("an admitted guest stopped reading when the link was replaced: %d", code)
	}

	// The guest who left, given the new link, is invited again.
	if code, _ := f.joinStatus(t, fresh, c); code != http.StatusNotFound {
		t.Errorf("the new link should show a guest who left the join form, got %d", code)
	}
	f.token = fresh
	c2, g2 := f.join(t, "Jane")
	if g2.ID == g.ID || g2.State != guestWaiting {
		t.Errorf("rejoining on a new link should be a new lobby entry: %+v", g2)
	}
	if code, _ := f.call(t, "GET /api/stacks", c2); code != http.StatusUnauthorized {
		t.Errorf("a rejoining guest got in without the host's admit: %d", code)
	}
}

func TestADriverWhoDropsLosesControlAfterTheGrace(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	_, g := f.join(t, "Jane")
	f.admit(t, g)
	old := shareDriverGrace
	shareDriverGrace = 50 * time.Millisecond
	t.Cleanup(func() { shareDriverGrace = old })
	h := f.app.hubFor(f.sess)
	h.setController(g.ID)
	c := &hubClient{guestID: g.ID, name: "Jane", out: make(chan []byte, 64), cancel: func() {}}
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
	h.clientGone(c)
	if shareHubs.controller(f.sess.ID) != g.ID {
		t.Fatal("control moved the moment the driver dropped; a reload must not cost it")
	}
	time.Sleep(200 * time.Millisecond)
	if shareHubs.controller(f.sess.ID) != 0 {
		t.Error("a driver gone past the grace still has control")
	}
}

// Retention: an ended session older than the limit goes, with its guests, transcript
// and actions; a recent one stays, a live one is never touched, and 0 keeps all.
func TestEndedSessionsArePurgedAfterTheRetention(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	_, g := f.join(t, "Jane")
	f.app.store.AddShareMessage(ShareMessage{SessionID: f.sess.ID, AuthorKind: "guest", GuestID: g.ID, Author: "Jane", Kind: "chat", Body: "hi"})
	f.app.store.StartShareAction(ShareAction{SessionID: f.sess.ID, GuestID: g.ID, Method: "POST", Path: "/x"}, 0)

	old, _ := f.app.store.CreateShareSession(f.host.ID, f.stack.ID, "old", false, time.Now().Add(time.Hour))
	recent, _ := f.app.store.CreateShareSession(f.host.ID, f.stack.ID, "recent", false, time.Now().Add(time.Hour))
	past := time.Now().Add(-100 * 24 * time.Hour).UTC().Format(time.RFC3339)
	f.app.store.db.Exec(`UPDATE share_sessions SET ended_at = ?, ended_reason = 'ended' WHERE id IN (?, ?)`, past, old.ID, f.sess.ID)
	f.app.store.EndShareSession(recent.ID, "ended")
	live, _ := f.app.store.CreateShareSession(f.host.ID, f.stack.ID, "live", false, time.Now().Add(time.Hour))
	f.app.store.db.Exec(`UPDATE share_sessions SET created_at = ? WHERE id = ?`, past, live.ID)

	f.app.store.SetAppSetting(settingShareRetentionDays, "0")
	if n := f.app.purgeShareSessions(); n != 0 {
		t.Errorf("retention 0 (keep forever) deleted %d", n)
	}
	f.app.store.SetAppSetting(settingShareRetentionDays, "90")
	if n := f.app.purgeShareSessions(); n != 2 {
		t.Errorf("purged %d, want the two that ended 100 days ago", n)
	}
	for _, id := range []int64{recent.ID, live.ID} {
		if _, err := f.app.store.GetShareSession(id); err != nil {
			t.Errorf("session %d should have been kept: %v", id, err)
		}
	}
	var guests, msgs, acts int
	f.app.store.db.QueryRow(`SELECT COUNT(*) FROM share_guests WHERE session_id = ?`, f.sess.ID).Scan(&guests)
	f.app.store.db.QueryRow(`SELECT COUNT(*) FROM share_messages WHERE session_id = ?`, f.sess.ID).Scan(&msgs)
	f.app.store.db.QueryRow(`SELECT COUNT(*) FROM share_actions WHERE session_id = ?`, f.sess.ID).Scan(&acts)
	if guests+msgs+acts != 0 {
		t.Errorf("a purged session left guests=%d messages=%d actions=%d behind", guests, msgs, acts)
	}
}

// 0 means "keep forever", so a settings save that leaves the field out must not
// read as 0 — and an out-of-range value is clamped.
func TestRetentionSettingSurvivesAPartialSave(t *testing.T) {
	app := newTestApp(t)
	if got := app.shareRetentionDays(); got != defaultShareRetentionDays {
		t.Errorf("unset retention = %d, want the default %d", got, defaultShareRetentionDays)
	}
	put := func(body string) SystemSettings {
		w := httptest.NewRecorder()
		app.handleUpdateSystemSettings(w, httptest.NewRequest("PUT", "/api/system/settings", strings.NewReader(body)))
		var s SystemSettings
		json.Unmarshal(w.Body.Bytes(), &s)
		return s
	}
	if s := put(`{"maxTokenDays":30,"sessionRetentionDays":14}`); s.SessionRetentionDays != 14 {
		t.Errorf("saved 14, read back %d", s.SessionRetentionDays)
	}
	if s := put(`{"maxTokenDays":30}`); s.SessionRetentionDays != 14 {
		t.Errorf("a save without the field changed retention to %d", s.SessionRetentionDays)
	}
	if s := put(`{"sessionRetentionDays":0}`); s.SessionRetentionDays != 0 {
		t.Errorf("keep forever (0) read back as %d", s.SessionRetentionDays)
	}
	if s := put(`{"sessionRetentionDays":999999}`); s.SessionRetentionDays != maxShareRetentionDays {
		t.Errorf("an absurd retention was not clamped: %d", s.SessionRetentionDays)
	}
}

// A viewer cannot take a shared window off everyone's screen: closing is the driver's.
func TestOnlyTheDriverClosesSharedWindows(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	_, g := f.join(t, "Jane")
	f.admit(t, g)
	h := f.app.hubFor(f.sess)
	host := &hubClient{guestID: 0, name: "host", out: make(chan []byte, 64), cancel: func() {}}
	jane := &hubClient{guestID: g.ID, name: "Jane", out: make(chan []byte, 64), cancel: func() {}}
	open := json.RawMessage(`{"id":"w1","link":"http://localhost:1/","title":"VNC"}`)
	h.handle(host, "browser-open", "", open)
	h.setController(g.ID) // Jane drives; the host is now a viewer
	h.handle(host, "browser-close", "", json.RawMessage(`{"id":"w1"}`))
	if h.browsers["w1"] == nil {
		t.Error("the host closed a shared window while not in control")
	}
	h.setController(0) // the host drives; Jane is a viewer
	h.handle(jane, "browser-close", "", json.RawMessage(`{"id":"w1"}`))
	if h.browsers["w1"] == nil {
		t.Error("a watching guest closed a shared window")
	}
	h.handle(host, "browser-close", "", json.RawMessage(`{"id":"w1"}`))
	if h.browsers["w1"] != nil {
		t.Error("the driver could not close the window")
	}
}
