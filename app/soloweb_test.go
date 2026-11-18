package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSoloGuard(t *testing.T) {
	t.Setenv("PUBLIC_URL", "https://lab.example.com")
	app := newTestApp(t)
	owner, _ := app.ensureOwner()
	app.store.CreateSession("sess-owner", time.Now().Add(time.Hour))
	secret, _ := tokenFor(t, app, owner.ID, ScopeRead, 1)
	session := &http.Cookie{Name: cookieName, Value: "sess-owner"}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := app.soloGuard("7480", ok)
	// Any file of the built web app: its names change with every build.
	asset := "missing"
	if ents, err := fs.ReadDir(embeddedFS, "web/dist/assets"); err == nil && len(ents) > 0 {
		asset = ents[0].Name()
	}
	cases := []struct {
		method, host, path, origin string
		cookie                     *http.Cookie
		bearer                     string
		upgrade                    bool
		want                       int
	}{
		// Addressed to the port by its own name, or the shared session's public one.
		{"GET", "127.0.0.1:7480", "/api/stacks", "", session, "", false, http.StatusTeapot},
		{"GET", "localhost:7480", "/api/stacks", "", session, "", false, http.StatusTeapot},
		{"GET", "lab.example.com", "/api/stacks", "", session, "", false, http.StatusTeapot},
		{"GET", "rebound.example.org:7480", "/api/stacks", "", session, "", false, http.StatusMisdirectedRequest}, // DNS rebinding
		{"GET", "127.0.0.1:7481", "/api/stacks", "", session, "", false, http.StatusMisdirectedRequest},
		// Cross-site.
		{"POST", "127.0.0.1:7480", "/api/stacks", "http://127.0.0.1:7480", session, "", false, http.StatusTeapot},
		{"POST", "127.0.0.1:7480", "/api/stacks", "https://evil.example.org", session, "", false, http.StatusForbidden},
		{"GET", "127.0.0.1:7480", "/api/stacks", "https://evil.example.org", session, "", true, http.StatusForbidden},
		// Credentials.
		{"GET", "127.0.0.1:7480", "/api/stacks", "", nil, "", false, http.StatusUnauthorized},
		{"GET", "127.0.0.1:7480", "/", "", nil, "", false, http.StatusUnauthorized},
		{"GET", "127.0.0.1:7480", "/", "", session, "", false, http.StatusTeapot},
		{"GET", "127.0.0.1:7480", "/api/stacks", "", &http.Cookie{Name: cookieName, Value: "made-up"}, "", false, http.StatusUnauthorized},
		{"GET", "127.0.0.1:7480", "/api/stacks", "", nil, secret, false, http.StatusTeapot},
		{"GET", "127.0.0.1:7480", "/api/stacks", "", nil, "dbc_not-a-real-token", false, http.StatusUnauthorized},
		{"GET", "127.0.0.1:7480", "/api/stacks", "", &http.Cookie{Name: guestCookieName, Value: "made-up"}, "", false, http.StatusUnauthorized},
		{"GET", "127.0.0.1:7480", "/_p/k1/", "", nil, "", false, http.StatusUnauthorized},
		// What answers without one: the start link and the app's static files.
		{"GET", "127.0.0.1:7480", "/start/anything", "", nil, "", false, http.StatusTeapot},
		{"GET", "127.0.0.1:7480", "/assets/" + asset, "", nil, "", false, http.StatusTeapot},
		{"GET", "127.0.0.1:7480", "/index.html", "", nil, "", false, http.StatusUnauthorized},
		{"GET", "127.0.0.1:7480", "/join/not-a-live-link", "", nil, "", false, http.StatusUnauthorized},
		{"POST", "127.0.0.1:7480", "/api/join/not-a-live-link", "", nil, "", false, http.StatusUnauthorized},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.upgrade {
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Upgrade", "websocket")
		}
		if c.cookie != nil {
			r.AddCookie(c.cookie)
		}
		if c.bearer != "" {
			r.Header.Set("Authorization", "Bearer "+c.bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Errorf("%s %s host %s origin %q cookie %v bearer %v: %d, want %d", c.method, c.path, c.host, c.origin, c.cookie != nil, c.bearer != "", rec.Code, c.want)
		}
	}
}

// A start link signs in once; a second use, an unknown token and an expired one do not.
func TestStartLinkWorksOnce(t *testing.T) {
	app := newTestApp(t)
	app.store.CreateUser("owner")
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	mux.HandleFunc("GET /start/{token}", app.handleStartLink)
	path := newStartLink()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("first use: %d %s", rec.Code, rec.Body)
	}
	cookie := rec.Result().Cookies()
	if len(cookie) == 0 || cookie[0].Name != cookieName {
		t.Fatalf("no session cookie: %v", cookie)
	}
	r := httptest.NewRequest("GET", "/api/setup/status", nil)
	r.AddCookie(cookie[0])
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if !strings.Contains(rec.Body.String(), `"authenticated":true`) {
		t.Fatalf("status after sign-in: %s", rec.Body)
	}

	for _, p := range []string{path, "/start/unknown"} {
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", p, rec.Code)
		}
	}
	expired := newStartLink()
	startLinks.Lock()
	startLinks.m[strings.TrimPrefix(expired, "/start/")] = time.Now().Add(-time.Second)
	startLinks.Unlock()
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", expired, nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("expired link: %d", rec.Code)
	}
}

// A guest gets in by a live join link, and then by their cookie; an ended link does not.
func TestSoloGuardLetsGuestsIn(t *testing.T) {
	f := newShareFixture(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := f.app.soloGuard("7480", ok)
	get := func(method, path string, c *http.Cookie) int {
		r := httptest.NewRequest(method, path, nil)
		r.Host = "127.0.0.1:7480"
		if c != nil {
			r.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := get("GET", "/join/"+f.token, nil); code != http.StatusTeapot {
		t.Errorf("the join page with a live link: %d", code)
	}
	if code := get("POST", "/api/join/"+f.token, nil); code != http.StatusTeapot {
		t.Errorf("joining with a live link: %d", code)
	}
	c, g := f.join(t, "Jane")
	if code := get("GET", "/api/stacks", c); code != http.StatusTeapot {
		t.Errorf("a joined guest's cookie: %d", code)
	}
	// A guest who has been removed, or whose session ended, is a request without a cookie.
	f.app.store.SetShareGuestState(g.ID, guestRemoved)
	if code := get("GET", "/api/stacks", c); code != http.StatusUnauthorized {
		t.Errorf("a removed guest's cookie: %d", code)
	}
	c2, _ := f.join(t, "John")
	f.app.store.EndShareSession(f.sess.ID, "test")
	if code := get("GET", "/api/stacks", c2); code != http.StatusUnauthorized {
		t.Errorf("a guest's cookie after the session ended: %d", code)
	}
	f.app.store.SetAppSetting(settingAllowGuestSessions, "0")
	if code := get("GET", "/join/"+f.token, nil); code != http.StatusUnauthorized {
		t.Errorf("a join link with shared sessions switched off: %d", code)
	}
}

// Where no browser can open — an SSH session, or Linux without a display — dbcanvas says so and
// prints the tunnel to make and the link to open at its other end.
func TestNoBrowserOnAServer(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("SSH_CONNECTION", "203.0.113.5 52000 198.51.100.7 22")
	if noBrowserHere() == "" {
		t.Error("an SSH session was taken for a desktop")
	}
	t.Setenv("SSH_CONNECTION", "")
	if got := noBrowserHere(); (got == "") != (runtime.GOOS != "linux") {
		t.Errorf("no display on %s: %q", runtime.GOOS, got)
	}
	t.Setenv("DISPLAY", ":0")
	if got := noBrowserHere(); got != "" {
		t.Errorf("a desktop with a display: %q", got)
	}

	msg := remoteSignIn(startLink{URL: "http://127.0.0.1:8123/start/abc"}, "this is an SSH session")
	for _, want := range []string{"ssh -N -L 8123:127.0.0.1:8123 ", "http://127.0.0.1:8123/start/abc", "dbcanvas open --print"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "publicUrl") {
		t.Error("a publicUrl line without a publicUrl")
	}
	msg = remoteSignIn(startLink{URL: "http://127.0.0.1:8123/start/abc", PublicURL: "https://dbc.example.com/start/abc"}, "x")
	if !strings.Contains(msg, "https://dbc.example.com/start/abc") {
		t.Errorf("the publicUrl link is missing:\n%s", msg)
	}
}

// With publicUrl set, the start link is offered on it too, carrying the same one-time token.
func TestStartLinkOnPublicURL(t *testing.T) {
	app := newTestApp(t)
	app.soloURL = "http://127.0.0.1:8123"
	call := func() map[string]string {
		w := httptest.NewRecorder()
		app.handleNewStartLink(w, withPrincipal(httptest.NewRequest("POST", "/api/solo/start-link", nil), principal{Socket: true}))
		var out map[string]string
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	t.Setenv("PUBLIC_URL", "")
	if out := call(); out["publicUrl"] != "" || !strings.HasPrefix(out["url"], "http://127.0.0.1:8123/start/") {
		t.Fatalf("without publicUrl: %v", out)
	}
	t.Setenv("PUBLIC_URL", "https://dbc.example.com/")
	out := call()
	tok := strings.TrimPrefix(out["url"], "http://127.0.0.1:8123")
	if out["publicUrl"] != "https://dbc.example.com"+tok {
		t.Fatalf("with publicUrl: %v", out)
	}
}
