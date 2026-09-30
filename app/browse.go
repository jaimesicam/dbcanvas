package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// browse.go — a node's web UI through DBCanvas's own port.
//
// Every web UI a node offers — PMM, the noVNC desktop, OpenEverest, the simulators'
// dashboards, HAProxy stats, SeaweedFS, webmail — is published on a host port of its
// own, and the UI links to it as http://<this host>:<port>/. That works on the machine
// DBCanvas runs on and nowhere else without a port forward per UI, which is exactly
// what a shared-session guest, or anyone reaching DBCanvas through one SSH tunnel,
// does not have.
//
// So the browser window proxies it. POST /api/browse turns such a link into
// /_p/<key>/<path>: the key names one node port, found from the host port in the link
// (Docker knows which container publishes it), and everything under the prefix is
// forwarded to that container over the stack network — the path the app already
// uses to reach a node for a benchmark.
//
// A web UI at a path prefix it was not written for is the hard part, because they are
// full of absolute paths ("/graph/", "/websockify"). Four things together cover it:
//
//   - redirects and cookies are rewritten into the prefix;
//   - HTML and CSS have their absolute src/href/url(...) rewritten;
//   - a small script injected into each page keeps fetch, XHR, WebSocket, EventSource
//     and history calls inside the prefix — the URLs a single-page app builds at run
//     time, which no rewrite of the markup can see;
//   - anything that still escapes to an absolute path arrives with the proxied page as
//     its Referer, and is sent back under the prefix (browseFallback).
//
// A proxied page is same-origin with DBCanvas. These are the user's own lab services,
// deployed from known images, and that is what lets them keep their own logins in an
// iframe; the proxy never forwards DBCanvas's own cookies to them.

const browsePrefix = "/_p/"

// browseIdle is how long an unused key lives; a window reopened after that asks again.
const browseIdle = 12 * time.Hour

type browseTarget struct {
	Key           string
	OwnerID       int64
	StackID       int64
	ContainerID   string
	ContainerPort int
	HostPort      int
	Scheme        string
	Title         string
	// Kind is the node's type when it matters to the proxy: "vnc" lets a watching
	// guest see the desktop through a view-only filter (browsevnc.go).
	Kind string

	mu       sync.Mutex
	addr     string
	lastUsed time.Time
	proxy    *httputil.ReverseProxy
}

func (t *browseTarget) prefix() string { return browsePrefix + t.Key }

var browseTargets = struct {
	sync.Mutex
	m map[string]*browseTarget
}{m: map[string]*browseTarget{}}

func browseLookup(key string) *browseTarget {
	browseTargets.Lock()
	defer browseTargets.Unlock()
	now := time.Now()
	for k, t := range browseTargets.m {
		t.mu.Lock()
		stale := now.Sub(t.lastUsed) > browseIdle
		t.mu.Unlock()
		if stale {
			delete(browseTargets.m, k)
		}
	}
	t := browseTargets.m[key]
	if t != nil {
		t.mu.Lock()
		t.lastUsed = now
		t.mu.Unlock()
	}
	return t
}

var (
	browseNodeRe = regexp.MustCompile(`^dbcanvas-(\d+)-(.+)$`)
	browseK3dRe  = regexp.MustCompile(`^k3d-(.+)-s(\d+)-(serverlb|server-\d+|agent-\d+)$`)
)

// stackOfContainer reads which stack a container belongs to from its name: a node's
// (containerName) or a K3D frame's load balancer or members (k3dClusterName).
func stackOfContainer(name string) (stackID int64, label string, ok bool) {
	if m := browseNodeRe.FindStringSubmatch(name); m != nil {
		id, err := strconv.ParseInt(m[1], 10, 64)
		return id, m[2], err == nil
	}
	if m := browseK3dRe.FindStringSubmatch(name); m != nil {
		id, err := strconv.ParseInt(m[2], 10, 64)
		return id, m[1], err == nil
	}
	return 0, "", false
}

func newBrowseKey() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// handleBrowse resolves a node link to a proxied address the browser window opens.
func (a *App) handleBrowse(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var in struct {
		URL string `json:"url"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	link, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (link.Scheme != "http" && link.Scheme != "https") || link.Host == "" {
		writeErr(w, http.StatusBadRequest, "a node link is an http:// or https:// address")
		return
	}
	port := link.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[link.Scheme]
	}
	hostPort, _ := strconv.Atoi(port)
	if a.docker == nil {
		writeErr(w, http.StatusNotImplemented, "the browser window needs the Docker engine")
		return
	}
	pp, found, err := a.docker.ContainerByHostPort(r.Context(), hostPort)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not ask Docker which node publishes port "+port)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "no running node publishes port "+port+" on this installation")
		return
	}
	stackID, label, ok := stackOfContainer(pp.Name)
	if !ok {
		writeErr(w, http.StatusForbidden, "port "+port+" does not belong to a DBCanvas node")
		return
	}
	st, err := a.store.GetStack(stackID)
	if err != nil || (st.OwnerID != u.ID && u.Role != RoleAdmin) {
		writeErr(w, http.StatusForbidden, "port "+port+" belongs to a stack that is not yours")
		return
	}

	// Which design node the container is — for its label, and whether it is a VNC
	// desktop (whose password the window fills in for whoever may read it).
	kind, vncPassword := "", ""
	for _, n := range buildDoc(st).Nodes {
		if containerName(stackID, n.ID) == pp.Name {
			label = n.Label
			if n.Type == "vnc" {
				kind = "vnc"
				if dep, err := a.store.GetDeployment(stackID, n.ID); err == nil {
					var sec struct {
						VNCPassword string `json:"vncPassword"`
					}
					json.Unmarshal(dep.Secrets, &sec)
					vncPassword = sec.VNCPassword
				}
			}
		}
	}
	if p, ok := principalOf(r); ok && p.Guest != nil && p.Guest.HideSecrets {
		vncPassword = "" // the host chose to keep passwords off guests' screens
	}

	// One key per owner and node port, so reopening a link reuses its cookies.
	browseTargets.Lock()
	var t *browseTarget
	for _, x := range browseTargets.m {
		if x.OwnerID == st.OwnerID && x.ContainerID == pp.ContainerID && x.ContainerPort == pp.ContainerPort && x.Scheme == link.Scheme {
			t = x
		}
	}
	if t == nil {
		t = &browseTarget{Key: newBrowseKey(), OwnerID: st.OwnerID, StackID: stackID, ContainerID: pp.ContainerID,
			ContainerPort: pp.ContainerPort, HostPort: hostPort, Scheme: link.Scheme, Title: label, Kind: kind, lastUsed: time.Now()}
		t.proxy = a.newBrowseProxy(t)
		browseTargets.m[t.Key] = t
	}
	browseTargets.Unlock()

	target := t.prefix() + link.EscapedPath()
	if link.Path == "" {
		target = t.prefix() + "/"
	}
	if link.RawQuery != "" {
		target += "?" + link.RawQuery
	}
	if link.Fragment != "" {
		target += "#" + link.EscapedFragment()
	}
	resp := map[string]any{"url": target, "title": t.Title, "stackId": stackID,
		"stackName": st.Name, "port": pp.ContainerPort, "kind": t.Kind}
	if vncPassword != "" {
		resp["vncPassword"] = vncPassword
	}
	writeJSON(w, http.StatusOK, resp)
}

// browseAuth decides who may use a key: its owner, on their own session, or an
// admitted guest of a live session the owner hosts. A guest who is only watching may
// look but not act — no form posts, no websockets — the same line the API draws.
func (a *App) browseAuth(r *http.Request, t *browseTarget) (allowed, readOnly bool) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		if u, err := a.store.SessionUser(c.Value); err == nil && (u.ID == t.OwnerID || u.Role == RoleAdmin) {
			return true, false
		}
	}
	if g, sess, host, err := a.guestAuth(r); err == nil && host.ID == t.OwnerID {
		return true, shareHubs.controller(sess.ID) != g.ID
	}
	return false, false
}

func browsePage(w http.ResponseWriter, code int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title>
<body style="font:14px system-ui,sans-serif;margin:0;display:flex;align-items:center;justify-content:center;height:100vh;background:#0e1117;color:#e6eaf2">
<div style="max-width:420px;text-align:center"><h2 style="font-weight:600">%s</h2><p style="color:#9aa4b2">%s</p></div>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(body))
}

func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// handleBrowseProxy serves everything under /_p/<key>/.
func (a *App) handleBrowseProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, browsePrefix)
	key, sub, _ := strings.Cut(rest, "/")
	t := browseLookup(key)
	if t == nil {
		browsePage(w, http.StatusNotFound, "This window has expired", "Open the link again from DBCanvas.")
		return
	}
	a.serveBrowse(t, "/"+sub, w, r)
}

func (a *App) serveBrowse(t *browseTarget, sub string, w http.ResponseWriter, r *http.Request) {
	allowed, readOnly := a.browseAuth(r, t)
	if !allowed {
		browsePage(w, http.StatusUnauthorized, "Not signed in", "Sign in to DBCanvas, or join the shared session, and open the link again.")
		return
	}
	// A watcher sees a VNC desktop live, through a filter that drops their keyboard,
	// pointer and clipboard (browsevnc.go); every other socket or form is the driver's.
	if readOnly && isUpgrade(r) && t.Kind == "vnc" {
		a.serveVNCViewOnly(t, sub, w, r)
		return
	}
	if readOnly && (isUpgrade(r) || (r.Method != http.MethodGet && r.Method != http.MethodHead)) {
		browsePage(w, http.StatusForbidden, "You are watching", "Ask the host for control to use this page.")
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = sub
	r2.URL.RawPath = ""
	t.proxy.ServeHTTP(w, r2)
}

// addr resolves the node's address on the stack network, joining it first.
func (a *App) browseAddr(ctx context.Context, t *browseTarget) (string, error) {
	t.mu.Lock()
	addr := t.addr
	t.mu.Unlock()
	if addr != "" {
		return addr, nil
	}
	netName := networkName(t.StackID)
	eng := a.dialEngine(t.StackID, t.ContainerID)
	if err := a.joinStackForDial(ctx, eng, netName); err != nil {
		return "", fmt.Errorf("join the stack network: %w", err)
	}
	ip, err := eng.ContainerIP(ctx, t.ContainerID, netName)
	if err != nil || ip == "" {
		return "", fmt.Errorf("the node has no address on the stack network")
	}
	addr = net.JoinHostPort(ip, strconv.Itoa(t.ContainerPort))
	t.mu.Lock()
	t.addr = addr
	t.mu.Unlock()
	return addr, nil
}

func (a *App) newBrowseProxy(t *browseTarget) *httputil.ReverseProxy {
	transport := &http.Transport{
		// The node's address is resolved per connection, so a container that moved to
		// a new IP (a restart) is found again after one failed dial.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			addr, err := a.browseAddr(ctx, t)
			if err != nil {
				return nil, err
			}
			c, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
			if err != nil {
				t.mu.Lock()
				t.addr = ""
				t.mu.Unlock()
			}
			return c, err
		},
		// Lab services present self-signed or Intranet-CA certificates for names this
		// process cannot verify; the connection is inside the stack's own network.
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	upstream := fmt.Sprintf("%s://node:%d", t.Scheme, t.ContainerPort)
	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = t.Scheme
			pr.Out.URL.Host = fmt.Sprintf("node:%d", t.ContainerPort)
			pr.Out.Host = pr.Out.URL.Host
			// Identity, so HTML and CSS can be rewritten on the way back.
			pr.Out.Header.Del("Accept-Encoding")
			pr.Out.Header.Del(guestHeader)
			// DBCanvas's own credentials are never a node's business.
			var keep []string
			for _, c := range pr.In.Cookies() {
				if c.Name != cookieName && c.Name != guestCookieName {
					keep = append(keep, c.Name+"="+c.Value)
				}
			}
			pr.Out.Header.Del("Cookie")
			if len(keep) > 0 {
				pr.Out.Header.Set("Cookie", strings.Join(keep, "; "))
			}
			// Apps that check Origin against Host (Grafana's CSRF check) see their own.
			if pr.In.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", upstream)
			}
			if ref := pr.In.Header.Get("Referer"); ref != "" {
				if ru, err := url.Parse(ref); err == nil && strings.HasPrefix(ru.Path, t.prefix()) {
					pr.Out.Header.Set("Referer", upstream+strings.TrimPrefix(ru.Path, t.prefix()))
				} else {
					pr.Out.Header.Del("Referer")
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error { return t.rewriteResponse(resp) },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			browsePage(w, http.StatusBadGateway, "The node did not answer",
				"DBCanvas could not reach this page on the node. It may still be starting, or it may be stopped.")
		},
	}
}

var (
	browseAttrRe = regexp.MustCompile(`(?i)(\s(?:src|href|action|poster|data)\s*=\s*["'])/([^/])`)
	browseCSSRe  = regexp.MustCompile(`(?i)(url\(\s*["']?)/([^/])`)
	browseHeadRe = regexp.MustCompile(`(?i)<head[^>]*>`)
)

// mapURL moves a URL the node wrote for itself — a redirect, a root_url — into the
// prefix. Absolute URLs are moved when they point at the node's own port; anything
// else (a link to another site) is left alone.
func (t *browseTarget) mapURL(loc string) string {
	if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
		if strings.HasPrefix(loc, t.prefix()+"/") {
			return loc
		}
		return t.prefix() + loc
	}
	u, err := url.Parse(loc)
	if err != nil || !u.IsAbs() {
		return loc
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	if port == strconv.Itoa(t.ContainerPort) || port == strconv.Itoa(t.HostPort) {
		out := t.prefix() + u.EscapedPath()
		if u.RawQuery != "" {
			out += "?" + u.RawQuery
		}
		if u.Fragment != "" {
			out += "#" + u.EscapedFragment()
		}
		return out
	}
	return loc
}

func (t *browseTarget) rewriteResponse(resp *http.Response) error {
	h := resp.Header
	// Framing and transport policies written for the node's own origin would stop the
	// page from loading in the window, or pin this origin to HTTPS.
	for _, k := range []string{"X-Frame-Options", "Content-Security-Policy", "Content-Security-Policy-Report-Only", "Strict-Transport-Security"} {
		h.Del(k)
	}
	if loc := h.Get("Location"); loc != "" {
		h.Set("Location", t.mapURL(loc))
	}
	if cookies := resp.Cookies(); len(cookies) > 0 {
		h.Del("Set-Cookie")
		tlsIn := resp.Request != nil && resp.Request.TLS != nil
		for _, c := range cookies {
			p := c.Path
			if p == "" || !strings.HasPrefix(p, "/") {
				p = "/"
			}
			c.Path = t.prefix() + p
			c.Domain = ""
			// A node on https sets Secure cookies that a plain-http DBCanvas would drop,
			// which is a login that never sticks.
			if !tlsIn {
				c.Secure = false
				if c.SameSite == http.SameSiteNoneMode {
					c.SameSite = http.SameSiteLaxMode
				}
			}
			h.Add("Set-Cookie", c.String())
		}
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return nil
	}
	ct := strings.ToLower(h.Get("Content-Type"))
	isHTML := strings.Contains(ct, "text/html")
	isCSS := strings.Contains(ct, "text/css")
	if !isHTML && !isCSS {
		return nil
	}
	if enc := h.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return nil // the node compressed anyway; pass it through untouched
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	pfx := []byte("${1}" + t.prefix() + "/${2}")
	if isHTML {
		body = browseAttrRe.ReplaceAll(body, pfx)
		body = browseCSSRe.ReplaceAll(body, pfx)
		shim := []byte(browseShim(t.prefix()))
		if loc := browseHeadRe.FindIndex(body); loc != nil {
			body = append(body[:loc[1]:loc[1]], append(shim, body[loc[1]:]...)...)
		} else {
			body = append(shim, body...)
		}
	} else {
		body = browseCSSRe.ReplaceAll(body, pfx)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	h.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

// browseShim keeps a page's run-time URLs inside the prefix: fetch, XHR, WebSocket,
// EventSource, history, window.open. It runs first in <head>, before the page's own
// scripts capture the originals.
func browseShim(prefix string) string {
	return `<script>(function(){var P=` + strconvQuote(prefix) + `;
function fix(u){if(u==null)return u;u=String(u);if(u.indexOf('//')===0)return u;
if(u.charAt(0)==='/'){return (u===P||u.indexOf(P+'/')===0)?u:P+u}
try{var x=new URL(u,location.href);var same=x.host===location.host&&(x.protocol===location.protocol||x.protocol==='ws:'||x.protocol==='wss:');
if(same&&x.pathname.indexOf(P+'/')!==0&&x.pathname!==P){x.pathname=P+x.pathname;return x.href}}catch(e){}return u}
var f=window.fetch;if(f)window.fetch=function(i,o){if(typeof i==='string'||i instanceof URL)i=fix(i);else if(i&&i.url){var n=fix(i.url);if(n!==i.url)i=new Request(n,i)}return f.call(this,i,o)};
var xo=XMLHttpRequest.prototype.open;XMLHttpRequest.prototype.open=function(m,u){arguments[1]=fix(u);return xo.apply(this,arguments)};
var W=window.WebSocket;if(W){var NW=function(u,p){return p===undefined?new W(fix(u)):new W(fix(u),p)};NW.prototype=W.prototype;['CONNECTING','OPEN','CLOSING','CLOSED'].forEach(function(k){NW[k]=W[k]});window.WebSocket=NW}
var E=window.EventSource;if(E){var NE=function(u,o){return new E(fix(u),o)};NE.prototype=E.prototype;window.EventSource=NE}
['pushState','replaceState'].forEach(function(k){var o=history[k];history[k]=function(s,t,u){return o.call(this,s,t,u==null?u:fix(u))}});
var wo=window.open;window.open=function(u){arguments[0]=fix(u);return wo.apply(this,arguments)};
})();</script>`
}

func strconvQuote(s string) string { return strconv.Quote(s) }

// browseFallback catches what the rewrites missed: a proxied page asking for an
// absolute path ("/public/app.js", "/api/login") reaches DBCanvas's root with that
// page as its Referer, and is sent to the same path under the page's prefix. A
// request from DBCanvas's own pages never carries such a Referer, so nothing of
// DBCanvas's is ever routed away.
func (a *App) browseFallback(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, browsePrefix) {
			if ref, err := url.Parse(r.Header.Get("Referer")); err == nil && ref.Host == r.Host && strings.HasPrefix(ref.Path, browsePrefix) {
				key, _, _ := strings.Cut(strings.TrimPrefix(ref.Path, browsePrefix), "/")
				if t := browseLookup(key); t != nil {
					if isUpgrade(r) {
						a.serveBrowse(t, r.URL.Path, w, r)
						return
					}
					http.Redirect(w, r, t.prefix()+r.URL.RequestURI(), http.StatusTemporaryRedirect)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
