package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSoloGuard(t *testing.T) {
	t.Setenv("PUBLIC_URL", "https://lab.example.com")
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := soloGuard("7480", ok)
	cases := []struct {
		method, host, origin string
		upgrade              bool
		want                 int
	}{
		{"GET", "127.0.0.1:7480", "", false, http.StatusTeapot},
		{"GET", "localhost:7480", "", false, http.StatusTeapot},
		{"GET", "lab.example.com", "", false, http.StatusTeapot},                      // the shared session's tunnel
		{"GET", "rebound.example.org:7480", "", false, http.StatusMisdirectedRequest}, // DNS rebinding
		{"GET", "127.0.0.1:7481", "", false, http.StatusMisdirectedRequest},
		{"POST", "127.0.0.1:7480", "http://127.0.0.1:7480", false, http.StatusTeapot},
		{"POST", "127.0.0.1:7480", "https://evil.example.org", false, http.StatusForbidden}, // cross-site form
		{"POST", "127.0.0.1:7480", "", false, http.StatusTeapot},                            // no Origin: not a browser
		{"GET", "127.0.0.1:7480", "https://evil.example.org", false, http.StatusTeapot},     // a cross-site read cannot see the answer
		{"GET", "127.0.0.1:7480", "https://evil.example.org", true, http.StatusForbidden},   // a cross-site websocket can
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/api/stacks", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.upgrade {
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Upgrade", "websocket")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Errorf("%s host %s origin %q upgrade %v: %d, want %d", c.method, c.host, c.origin, c.upgrade, rec.Code, c.want)
		}
	}
}

// A start link signs in once; a second use, an unknown token and an expired one do not.
func TestStartLinkWorksOnce(t *testing.T) {
	app := newTestApp(t)
	app.solo = true
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
	if !strings.Contains(rec.Body.String(), `"authenticated":true`) || !strings.Contains(rec.Body.String(), `"solo":true`) {
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

func TestSoloRefusesPasswordRoutes(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := soloRefusePasswords(ok)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/api/setup", 404}, {"POST", "/api/auth/login", 404}, {"POST", "/api/auth/register", 404},
		{"POST", "/api/auth/guest", 404}, {"GET", "/api/auth/reset/abc", 404}, {"POST", "/api/join/abc/account", 404},
		{"GET", "/api/setup/status", 418}, {"POST", "/api/auth/logout", 418}, {"POST", "/api/join/abc", 418},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
}
