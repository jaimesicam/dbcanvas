package main

import (
	"crypto/rand"
	"encoding/hex"
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
// once and for a minute and signs that browser in as the owner with an ordinary
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
const startLinkTTL = time.Minute

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
			"A start link works once, for a minute. Run dbcanvas in a terminal on this computer to open DBCanvas again.")
		return
	}
	owner, err := a.ensureOwner()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "no owner account: "+err.Error())
		return
	}
	if err := a.issueSession(w, r, owner.ID); err != nil {
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
	writeJSON(w, http.StatusOK, map[string]string{"url": a.soloURL + newStartLink()})
}

// soloGuard refuses requests the browser port should never serve: one addressed to a
// name other than its own, and a cross-site write or websocket.
func soloGuard(port string, next http.Handler) http.Handler {
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
		next.ServeHTTP(w, r)
	})
}

// loopbackURL is the browser's address for a listener on addr.
func loopbackURL(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return "http://127.0.0.1:" + port
}
