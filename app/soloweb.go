package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// soloweb.go — the browser side of the dbcanvas command: how a browser signs in, and
// which requests the browser port accepts.
//
// There are no passwords. The command opens a start link, /start/<token>, which works
// once and for 3 minutes and signs that browser in as the owner with an ordinary
// session. A running dbcanvas hands a fresh link to whoever asks on the control socket,
// which only the owner can reach; so a second `dbcanvas` opens a signed-in browser, and
// a link found in a browser's history signs no one in.
//
// The browser port listens on 127.0.0.1 only, but a web page anywhere can still make a
// browser send it requests: through a DNS name that resolves to 127.0.0.1 (rebinding) or
// as a cross-site form or fetch. So the port answers only requests addressed to it by a
// loopback name, or to the public URL a shared session is reached through, and refuses
// a request whose Origin is anywhere else.

// startLinkTTL is how long a start link works.
const startLinkTTL = 3 * time.Minute

var startLinks = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

// newStartLink returns the path of a fresh one-time start link.
func newStartLink() string {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	startLinks.Lock()
	defer startLinks.Unlock()
	now := time.Now()
	for k, exp := range startLinks.m {
		if now.After(exp) {
			delete(startLinks.m, k)
		}
	}
	startLinks.m[tok] = now.Add(startLinkTTL)
	return "/start/" + tok
}

// useStartLink consumes a start link's token; false when it is unknown, used or expired.
func useStartLink(tok string) bool {
	startLinks.Lock()
	defer startLinks.Unlock()
	exp, ok := startLinks.m[tok]
	delete(startLinks.m, tok)
	return ok && time.Now().Before(exp)
}

// handleStartLink signs the browser in as the owner and opens the app.
func (a *App) handleStartLink(w http.ResponseWriter, r *http.Request) {
	if !useStartLink(r.PathValue("token")) {
		browsePage(w, http.StatusForbidden, "This link has expired",
			"A start link works once, for 3 minutes. Run dbcanvas in a terminal on this computer to open DBCanvas again.")
		return
	}
	if err := a.issueSession(w, r); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not sign in: "+err.Error())
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleNewStartLink answers the control socket with a start link to open.
func (a *App) handleNewStartLink(w http.ResponseWriter, r *http.Request) {
	if p, ok := principalOf(r); !ok || !p.Socket {
		writeErr(w, http.StatusForbidden, "a start link is given on the control socket only")
		return
	}
	if a.soloURL == "" {
		writeErr(w, http.StatusConflict, "this dbcanvas is not serving a browser")
		return
	}
	// One token, two addresses: the port on 127.0.0.1, and publicUrl when the owner reaches
	// DBCanvas through a tunnel or reverse proxy of their own. Whichever is used first spends it.
	path := newStartLink()
	out := map[string]string{"url": a.soloURL + path}
	if pub := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_URL")), "/"); pub != "" {
		out["publicUrl"] = pub + path
	}
	writeJSON(w, http.StatusOK, out)
}

// soloGuard is the browser port's door. It refuses a request addressed to a name other
// than its own, and a cross-site write or websocket (see the file comment); and it answers
// nothing without a credential. 127.0.0.1 is reachable by every user and every process on
// this computer, so a request must carry one of:
//
//   - the owner's browser session, from a start link;
//   - an API token (dbcanvas token create), as a bearer token;
//   - a shared session guest's cookie, or a live join link in the path (/join/<token>,
//     /api/join/<token>/…), which is how a guest arrives before having the cookie.
//
// Without one, only the start link itself and the web app's static files answer: the
// files are the same public code that is in the binary, and they are what the join page
// needs to draw. Everything else is a 401 that says how to get in and nothing about what
// is running.
func (a *App) soloGuard(port string, next http.Handler) http.Handler {
	hosts := map[string]bool{}
	for _, h := range []string{"127.0.0.1", "localhost", "[::1]"} {
		hosts[h+":"+port] = true
	}
	// A shared session's guests come through the owner's own tunnel, under its name.
	if u, err := url.Parse(strings.TrimSpace(os.Getenv("PUBLIC_URL"))); err == nil && u.Host != "" {
		hosts[strings.ToLower(u.Host)] = true
		if u.Port() == "" {
			hosts[strings.ToLower(u.Hostname())] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[strings.ToLower(r.Host)] {
			http.Error(w, "DBCanvas answers only on 127.0.0.1:"+port, http.StatusMisdirectedRequest)
			return
		}
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
		if o := r.Header.Get("Origin"); o != "" && (!safe || isUpgrade(r)) {
			ou, err := url.Parse(o)
			if err != nil || !hosts[strings.ToLower(ou.Host)] {
				writeErr(w, http.StatusForbidden, "cross-site request refused")
				return
			}
		}
		if !a.soloCredential(r) && !soloPublic(r) && !a.liveJoinPath(r) {
			soloUnauthorized(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// soloCredential reports whether the request carries a credential that is still good.
func (a *App) soloCredential(r *http.Request) bool {
	if bearerToken(r) != "" {
		_, _, err := a.tokenAuth(r)
		return err == nil
	}
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		if _, err := a.store.SessionUser(c.Value); err == nil {
			return true
		}
	}
	if c, err := r.Cookie(guestCookieName); err == nil && c.Value != "" {
		return a.liveGuest(hashTokenSecret(c.Value))
	}
	return false
}

// liveGuest reports whether a guest cookie belongs to a guest still in the lobby or
// admitted, of a shared session that is still live: once the session ends or the guest
// leaves, is denied or removed, the cookie gets no further than a request without one.
func (a *App) liveGuest(cookieHash string) bool {
	g, err := a.store.ShareGuestByCookie(cookieHash)
	if err != nil || (g.State != guestWaiting && g.State != guestAdmitted) {
		return false
	}
	sess, err := a.store.GetShareSession(g.SessionID)
	return err == nil && sess.live(time.Now())
}

// soloPublic is what answers without a credential: the start link and the web app's
// static files (not its page, which is served only to a credential or a join link).
func soloPublic(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/start/") || strings.HasPrefix(p, "/licenses/") {
		return true
	}
	if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, browsePrefix) || p == "/" || p == "/index.html" {
		return false
	}
	return spaStaticFile(strings.TrimPrefix(p, "/"))
}

// liveJoinPath reports whether the path carries a live shared session's join link.
func (a *App) liveJoinPath(r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/join/")
	if !ok {
		rest, ok = strings.CutPrefix(r.URL.Path, "/api/join/")
	}
	if !ok {
		return false
	}
	tok, _, _ := strings.Cut(rest, "/")
	if tok == "" || len(tok) > 100 {
		return false
	}
	if allowed, _ := a.guestSessionSettings(); !allowed {
		return false
	}
	sess, err := a.store.ShareSessionByToken(hashTokenSecret(tok))
	return err == nil && sess.live(time.Now())
}

// soloUnauthorized answers a request without a credential: how to get in, nothing more.
func soloUnauthorized(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || isUpgrade(r) {
		writeErr(w, http.StatusUnauthorized, "sign in: run dbcanvas on this computer, or send an API token (dbcanvas token create)")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>DBCanvas</title>
<body style="font:15px system-ui,sans-serif;margin:0;display:flex;align-items:center;justify-content:center;height:100vh">
<div style="max-width:460px;padding:24px"><h2 style="font-weight:600">Open DBCanvas from the terminal</h2>
<p>This browser is not signed in. Run <code>dbcanvas</code> in a terminal on this computer: it opens DBCanvas here, signed in.</p></div>`)
}

// loopbackURL is the browser's address for a listener on addr.
func loopbackURL(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return "http://127.0.0.1:" + port
}
