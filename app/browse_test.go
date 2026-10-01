package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// browse_test.go — the browser window's proxy, against a stand-in for a node web UI
// that does what real ones do: absolute paths, a redirect to its own root_url, a
// Secure cookie, frame-busting headers, and a websocket.

func fakeNodeUI(t *testing.T) (*httptest.Server, int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(cookieName); err == nil {
			t.Errorf("the node was sent DBCanvas's session cookie %q", c.Value)
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head><title>x</title><link href="/static/app.css" rel="stylesheet"></head>`+
			`<body><img src="/img/logo.png"><a href="//other.example/x">ext</a><form action="/login"></form></body></html>`)
	})
	mux.HandleFunc("/static/app.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		io.WriteString(w, `body{background:url(/img/bg.png)}`)
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "app_session", Value: "s1", Path: "/", Secure: true, SameSite: http.SameSiteNoneMode, Domain: "node.example"})
		http.Redirect(w, r, "https://fqdn.example:"+r.URL.Query().Get("port")+"/graph/", http.StatusFound)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		typ, b, err := c.Read(r.Context())
		if err == nil {
			c.Write(r.Context(), typ, append([]byte("echo:"), b...))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(p)
	return srv, port
}

// browseFixture registers a key pointing straight at the fake node, skipping the
// stack-network join a real node needs.
func browseFixture(t *testing.T) (*App, *browseTarget, User, *http.Cookie, *httptest.Server) {
	app := newTestApp(t)
	owner, _ := app.store.CreateUser("owner", "x", RoleUser, StatusApproved)
	srv, port := fakeNodeUI(t)
	tg := &browseTarget{Key: "k1", OwnerID: owner.ID, StackID: 1, ContainerPort: port, HostPort: 32000, Scheme: "http",
		addr: srv.Listener.Addr().String(), lastUsed: time.Now()}
	tg.proxy = app.newBrowseProxy(tg)
	browseTargets.Lock()
	browseTargets.m[tg.Key] = tg
	browseTargets.Unlock()
	t.Cleanup(func() { browseTargets.Lock(); delete(browseTargets.m, "k1"); browseTargets.Unlock() })

	sess := "sess-owner"
	if err := app.store.CreateSession(sess, owner.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(app.browseFallback(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, browsePrefix) {
			app.handleBrowseProxy(w, r)
			return
		}
		io.WriteString(w, "dbcanvas")
	})))
	t.Cleanup(front.Close)
	return app, tg, owner, &http.Cookie{Name: cookieName, Value: sess}, front
}

func get(t *testing.T, front *httptest.Server, path string, c *http.Cookie, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", front.URL+path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestBrowseRewritesAPageIntoItsPrefix(t *testing.T) {
	_, tg, _, c, front := browseFixture(t)
	resp := get(t, front, "/_p/k1/", c, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{`href="/_p/k1/static/app.css"`, `src="/_p/k1/img/logo.png"`, `action="/_p/k1/login"`, `href="//other.example/x"`, `var P="/_p/k1"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("page is missing %s:\n%s", want, body)
		}
	}
	if resp.Header.Get("X-Frame-Options") != "" || resp.Header.Get("Content-Security-Policy") != "" {
		t.Error("frame-busting headers survived, so the window would stay blank")
	}
	css := get(t, front, "/_p/k1/static/app.css", c, nil)
	cb, _ := io.ReadAll(css.Body)
	if !strings.Contains(string(cb), "url(/_p/k1/img/bg.png)") {
		t.Errorf("css not rewritten: %s", cb)
	}
	_ = tg
}

func TestBrowseMovesRedirectsAndCookiesIntoThePrefix(t *testing.T) {
	_, tg, _, c, front := browseFixture(t)
	resp := get(t, front, "/_p/k1/login?port="+strconv.Itoa(tg.ContainerPort), c, nil)
	if loc := resp.Header.Get("Location"); loc != "/_p/k1/graph/" {
		t.Errorf("a redirect to the node's own root_url became %q", loc)
	}
	sc := resp.Header.Get("Set-Cookie")
	if !strings.Contains(sc, "Path=/_p/k1/") || strings.Contains(sc, "Secure") || strings.Contains(sc, "Domain") || strings.Contains(sc, "SameSite=None") {
		t.Errorf("the node's cookie was not moved into the prefix for a plain-http page: %s", sc)
	}
	// Another site's address is left alone.
	if got := tg.mapURL("https://elsewhere.example:9999/x"); got != "https://elsewhere.example:9999/x" {
		t.Errorf("a link to another site was rewritten: %s", got)
	}
}

func TestBrowseCatchesAbsolutePathsByReferer(t *testing.T) {
	_, _, _, c, front := browseFixture(t)
	// The page asked for /public/app.js — outside the prefix, but its Referer gives it away.
	resp := get(t, front, "/public/app.js?v=2", c, map[string]string{"Referer": front.URL + "/_p/k1/some/page"})
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != "/_p/k1/public/app.js?v=2" {
		t.Errorf("an escaped absolute path was not sent back into the prefix: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// DBCanvas's own requests are never routed away.
	own := get(t, front, "/api/stacks", c, map[string]string{"Referer": front.URL + "/#stack-designer"})
	b, _ := io.ReadAll(own.Body)
	if string(b) != "dbcanvas" {
		t.Errorf("a DBCanvas request was proxied: %s", b)
	}
}

func TestBrowseCarriesWebsockets(t *testing.T) {
	_, _, _, c, front := browseFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	h.Add("Cookie", c.String())
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http")+"/_p/k1/echo", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("websocket through the proxy: %v", err)
	}
	defer conn.CloseNow()
	conn.Write(ctx, websocket.MessageText, []byte("hi"))
	_, b, err := conn.Read(ctx)
	if err != nil || string(b) != "echo:hi" {
		t.Errorf("websocket round trip: %q %v", b, err)
	}
}

func TestBrowseIsTheOwnersAndTheirGuests(t *testing.T) {
	app, tg, owner, c, front := browseFixture(t)
	if resp := get(t, front, "/_p/k1/", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no session: %d, want 401", resp.StatusCode)
	}
	other, _ := app.store.CreateUser("other", "x", RoleUser, StatusApproved)
	os := "sess-other"
	app.store.CreateSession(os, other.ID, time.Now().Add(time.Hour))
	if resp := get(t, front, "/_p/k1/", &http.Cookie{Name: cookieName, Value: os}, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("somebody else's session: %d, want 401", resp.StatusCode)
	}
	if resp := get(t, front, "/_p/nope/", c, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown key: %d, want 404", resp.StatusCode)
	}

	// A guest of the owner's session: watching may look, not act.
	app.store.SetAppSetting(settingAllowGuestSessions, "1")
	st, _ := app.store.CreateStack("lab", owner.ID, ttlInfinity, nil, []byte(defaultDesign))
	sess, _ := app.store.CreateShareSession(owner.ID, st.ID, "h", false, false, time.Now().Add(time.Hour))
	g, _ := app.store.CreateShareGuest(sess.ID, "Jane", "j@example.com", hashTokenSecret("gsecret"), "", "")
	app.store.SetShareGuestState(g.ID, guestAdmitted)
	gc := &http.Cookie{Name: guestCookieName, Value: "gsecret"}
	if resp := get(t, front, "/_p/k1/", gc, nil); resp.StatusCode != 200 {
		t.Errorf("a watching guest could not look: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", front.URL+"/_p/k1/login", nil)
	req.AddCookie(gc)
	resp, _ := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a watching guest posted a form: %d, want 403", resp.StatusCode)
	}
	h := app.hubFor(sess)
	defer h.end("ended")
	h.setController(g.ID)
	req, _ = http.NewRequest("POST", front.URL+"/_p/k1/login?port=1", nil)
	req.AddCookie(gc)
	resp, _ = (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if resp.StatusCode != http.StatusFound {
		t.Errorf("a driving guest could not use the page: %d", resp.StatusCode)
	}
	_ = tg
}

func TestStackOfContainer(t *testing.T) {
	for name, want := range map[string]int64{
		"dbcanvas-91-pmm-mt1kvaak-3": 91, "k3d-k3d-01-s1-serverlb": 1, "k3d-everest-s12-server-0": 12,
	} {
		if id, _, ok := stackOfContainer(name); !ok || id != want {
			t.Errorf("%s: %d %v, want %d", name, id, ok, want)
		}
	}
	for _, name := range []string{"dbcanvas-app-1", "postgres", "k3d-registry"} {
		if _, _, ok := stackOfContainer(name); ok {
			t.Errorf("%s was taken for a stack's container", name)
		}
	}
}
