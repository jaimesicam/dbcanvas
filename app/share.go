package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// share.go — shared sessions.
//
// A host shares a live DBCanvas session through a link that lasts at most two hours.
// Whoever opens it gives a name and an email and waits in a lobby; the host admits
// them; from then on they follow everything the host does, across every tab, and
// chat. A guest may ask for control, and while they hold it they can do anything the
// host can in the workspace, with everyone else watching.
//
// The design rests on one decision: a guest is not an account. A guest request is
// the HOST's request, made with a narrower scope — read while watching, write while
// driving — exactly as an API token of that scope would be. So the two hundred
// handlers, and every ownership check in them, serve guests without knowing they
// exist; what is new is only who may send the request (guestAuth), what a guest may
// never reach whatever the scope (guestForbidden), and a record of what they did.
//
// Guest mode is explicit per request. The guest's browser marks every API call with
// the X-DBCanvas-Guest header (sockets: ?guest=1), because a cookie alone would turn
// a colleague who also has an account here into a guest in every tab they own.

const (
	guestCookieName = "dbcanvas_guest"
	guestHeader     = "X-DBCanvas-Guest"

	shareMinMinutes     = 5
	shareMaxMinutes     = 120 // the hard ceiling; an admin may set a lower one
	shareDefaultMinutes = 60

	shareChatMax  = 2000
	shareNameMax  = 80
	shareEmailMax = 254

	settingAllowGuestSessions = "allowGuestSessions"
	settingMaxGuestMinutes    = "maxGuestMinutes"
	settingShareRetentionDays = "sessionRetentionDays"

	// defaultShareRetentionDays is how long an ended session's records are kept: the
	// same ninety days as an API token's ceiling — long enough to go back to a support
	// case, short enough that guests' names and addresses do not pile up.
	defaultShareRetentionDays = 90
	maxShareRetentionDays     = 3650
)

// ------------------------------------------------------------- settings

// guestSessionSettings is what the instance allows. Off unless an admin turned it on:
// a link that admits people without an account is a decision for the installation.
func (a *App) guestSessionSettings() (allowed bool, maxMinutes int) {
	if v, err := a.store.AppSetting(settingAllowGuestSessions); err == nil {
		allowed = v == "1"
	}
	maxMinutes = shareMaxMinutes
	if v, err := a.store.AppSetting(settingMaxGuestMinutes); err == nil && v != "" {
		maxMinutes = clampGuestMinutes(v)
	}
	return allowed, maxMinutes
}

func clampGuestMinutes(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return shareMaxMinutes
	}
	if n < shareMinMinutes {
		return shareMinMinutes
	}
	if n > shareMaxMinutes {
		return shareMaxMinutes
	}
	return n
}

func clampRetentionDays(n int) int {
	if n < 0 {
		return defaultShareRetentionDays
	}
	if n > maxShareRetentionDays {
		return maxShareRetentionDays
	}
	return n
}

// shareRetentionDays is the configured retention; 0 keeps records forever.
func (a *App) shareRetentionDays() int {
	v, err := a.store.AppSetting(settingShareRetentionDays)
	if err != nil || strings.TrimSpace(v) == "" {
		return defaultShareRetentionDays
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return defaultShareRetentionDays
	}
	return clampRetentionDays(n)
}

// purgeShareSessions deletes the sessions that ended longer ago than the retention,
// and with them their guests, transcript and actions (ON DELETE CASCADE). A live
// session is never touched, however old it is.
func (a *App) purgeShareSessions() int {
	days := a.shareRetentionDays()
	if days == 0 {
		return 0
	}
	n, err := a.store.PurgeShareSessions(time.Now().Add(-time.Duration(days) * 24 * time.Hour))
	if err != nil {
		log.Printf("shared sessions: purge failed: %v", err)
	}
	return n
}

// publicURL is the base a share link is built on. PUBLIC_URL when the operator set
// one — the address colleagues use, a LAN IP or a forwarded port — otherwise the
// address the host's own browser used, which is right unless that was localhost.
func publicURL(r *http.Request) string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_URL")), "/"); v != "" {
		return v
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ------------------------------------------------------------- guest identity

// guestPrincipal rides on a request's principal when a guest sent it. The principal's
// User is the host (demoted to a plain user; see serveGuest); this says who really
// clicked, for the audit and for the handlers that need to know — the hub, the
// shared terminals.
type guestPrincipal struct {
	GuestID     int64
	SessionID   int64
	Name        string
	Email       string
	HideSecrets bool
	Driving     bool
}

// isGuestRequest reports whether the browser asked to be treated as a guest.
func isGuestRequest(r *http.Request) bool {
	return r.Header.Get(guestHeader) != "" || r.URL.Query().Get("guest") == "1"
}

var errNotAdmitted = errors.New("not admitted to a live shared session")

// guestAuth resolves the guest cookie to an admitted guest of a live session, and the
// host they act as. Every refusal is the same error: the caller answers 401, and a
// guest learns nothing from which check failed.
func (a *App) guestAuth(r *http.Request) (ShareGuest, ShareSession, User, error) {
	c, err := r.Cookie(guestCookieName)
	if err != nil || c.Value == "" {
		return ShareGuest{}, ShareSession{}, User{}, errNotAdmitted
	}
	g, err := a.store.ShareGuestByCookie(hashTokenSecret(c.Value))
	if err != nil || g.State != guestAdmitted {
		return ShareGuest{}, ShareSession{}, User{}, errNotAdmitted
	}
	sess, err := a.store.GetShareSession(g.SessionID)
	if err != nil || !sess.live(time.Now()) {
		return ShareGuest{}, ShareSession{}, User{}, errNotAdmitted
	}
	if allowed, _ := a.guestSessionSettings(); !allowed {
		return ShareGuest{}, ShareSession{}, User{}, errNotAdmitted
	}
	host, err := a.store.GetUser(sess.HostID)
	if err != nil || host.Status != StatusApproved {
		return ShareGuest{}, ShareSession{}, User{}, errNotAdmitted
	}
	return g, sess, host, nil
}

// guestForbidden is what no guest may reach, driving or not, and why. The account and
// everything that manages the session itself belong to the host; administration
// belongs to nobody who came in through a link.
func guestForbidden(rt apiRoute) string {
	switch {
	case rt.Auth == authAdmin:
		return "administration is not available in a shared session"
	case rt.NoToken:
		return "this belongs to the host's account and is not available in a shared session"
	case rt.Group == gUsers || rt.Group == gTokens:
		return "accounts and API tokens are not available in a shared session"
	case rt.Group == gNotif:
		return "notifications belong to the host"
	case rt.Group == gAuth && rt.Method != http.MethodGet:
		return "signing in and out is not available in a shared session"
	case rt.Group == gPrefs && rt.Mutates():
		return "settings belong to the host"
	case rt.Group == gShare && !rt.GuestOK:
		return "only the host manages a shared session"
	}
	return ""
}

// guestNeed is the scope a guest needs for a route. As for a token — GET reads, the
// rest writes — with one addition: an interactive socket (a node's terminal, the
// debugger, gdb) is a GET that hands over a shell, so it takes control.
func guestNeed(rt apiRoute) string {
	if rt.Media == mediaWebSocket && rt.Group != gShare {
		return ScopeWrite
	}
	return routeScope(rt)
}

// serveGuest is requireScope's guest path.
func (a *App) serveGuest(rt apiRoute, next http.HandlerFunc, w http.ResponseWriter, r *http.Request) {
	g, sess, host, err := a.guestAuth(r)
	if err != nil {
		if rt.Auth == authPublic {
			// Public routes stay public: the join page itself sends the header
			// before its guest is admitted.
			next(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, "this shared session has ended, or you have not been admitted to it")
		return
	}
	if why := guestForbidden(rt); why != "" {
		writeErr(w, http.StatusForbidden, why)
		return
	}
	driving := shareHubs.controller(sess.ID) == g.ID
	have := ScopeRead
	if driving {
		have = ScopeWrite
	}
	need := guestNeed(rt)
	if !scopeAllows(have, need) {
		writeErr(w, http.StatusForbidden, "you are watching — ask the host for control to change things")
		return
	}
	// A guest never inherits admin: an admin host shares their own workspace, not
	// every stack an admin can see.
	if host.Role == RoleAdmin {
		host.Role = RoleUser
	}
	gp := &guestPrincipal{GuestID: g.ID, SessionID: sess.ID, Name: g.Name, Email: g.Email,
		HideSecrets: sess.HideSecrets, Driving: driving}
	r = withPrincipal(r, principal{User: host, Guest: gp})

	if sess.HideSecrets && rt.Method == http.MethodPut && rt.Path == "/api/stacks/{id}" {
		if err := a.unmaskDesignBody(r); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	audit := need == ScopeWrite && rt.Group != gShare
	var actionID int64
	if audit {
		actionID = a.recordGuestAction(sess, g, rt, r)
	}
	switch {
	case rt.Media == mediaWebSocket:
		next(w, r)
	case sess.HideSecrets && rt.Method == http.MethodGet && rt.Media == mediaJSON:
		sw := &scrubWriter{ResponseWriter: w, status: http.StatusOK}
		next(sw, r)
		sw.finish()
		if audit {
			a.store.FinishShareAction(actionID, sw.status)
		}
	default:
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)
		if audit {
			a.store.FinishShareAction(actionID, rec.status)
		}
	}
}

// recordGuestAction writes the audit row and the matching line in chat before a
// guest's write runs, so everyone sees it as it happens and a request that never
// returns is still on the record.
func (a *App) recordGuestAction(sess ShareSession, g ShareGuest, rt apiRoute, r *http.Request) int64 {
	summary := strings.TrimSuffix(rt.Summary, ".")
	body := fmt.Sprintf("%s — %s %s", summary, r.Method, r.URL.Path)
	var msgID int64
	if h := shareHubs.get(sess.ID); h != nil {
		msgID = h.event("action", g.ID, g.Name, body)
	} else if m, err := a.store.AddShareMessage(ShareMessage{SessionID: sess.ID, AuthorKind: "guest",
		GuestID: g.ID, Author: g.Name, Kind: "action", Body: body}); err == nil {
		msgID = m.ID
	}
	id, _ := a.store.StartShareAction(ShareAction{SessionID: sess.ID, GuestID: g.ID, Method: r.Method,
		Path: r.URL.Path, Summary: summary}, msgID)
	return id
}

// statusRecorder notes the status a handler wrote, and passes everything else
// through — including Flush and Hijack, which streaming and upgraded routes need.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// ------------------------------------------------------------- hide secrets

// Hide secrets is the host's per-session choice to keep passwords off guests'
// screens. It is a courtesy, not a wall: a guest who drives can open a shell and read
// a config file. What it does guarantee is that the values never reach a guest's
// browser through the API, and that a guest's save cannot overwrite a real password
// with the mask it was shown.

const secretMask = "••••••••"

// secretKey reports whether a JSON key names a credential.
func secretKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range []string{"password", "passwd", "secret", "privatekey", "private_key", "accesskey", "access_key", "apikey", "api_key", "token"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// scrubJSON masks every non-empty string under a credential key. Under a key that is
// itself a credential name ("secrets"), every string below it is masked.
func scrubJSON(v any, under bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = scrubJSON(x, under || secretKey(k))
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = scrubJSON(x, under)
		}
		return t
	case string:
		if under && t != "" {
			return secretMask
		}
		return t
	}
	return v
}

// unmaskJSON puts the stored values back wherever an incoming document still holds
// the mask. Arrays of objects are matched by their "id", since a design's nodes and
// frames can be reordered between the read and the save; anything else by index.
func unmaskJSON(in, old any) any {
	switch t := in.(type) {
	case map[string]any:
		o, _ := old.(map[string]any)
		for k, x := range t {
			t[k] = unmaskJSON(x, o[k])
		}
		return t
	case []any:
		o, _ := old.([]any)
		byID := map[string]any{}
		for _, x := range o {
			if m, ok := x.(map[string]any); ok {
				if id, ok := m["id"].(string); ok {
					byID[id] = m
				}
			}
		}
		for i, x := range t {
			var prev any
			if m, ok := x.(map[string]any); ok {
				if id, ok := m["id"].(string); ok {
					prev = byID[id]
				}
			}
			if prev == nil && i < len(o) {
				prev = o[i]
			}
			t[i] = unmaskJSON(x, prev)
		}
		return t
	case string:
		if t == secretMask {
			if s, ok := old.(string); ok {
				return s
			}
			return ""
		}
		return t
	}
	return in
}

// unmaskDesignBody rewrites a guest's stack save so masked values keep what is stored.
func (a *App) unmaskDesignBody(r *http.Request) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	r.Body.Close()
	if err != nil {
		return err
	}
	restore := func(b []byte) { r.Body = io.NopCloser(bytes.NewReader(b)); r.ContentLength = int64(len(b)) }
	id, err := pathID(r)
	if err != nil {
		restore(raw)
		return nil
	}
	st, err := a.store.GetStack(id)
	if err != nil {
		restore(raw)
		return nil
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	var stored any
	json.Unmarshal(st.Design, &stored)
	if d, ok := body["design"]; ok {
		body["design"] = unmaskJSON(d, stored)
	}
	out, _ := json.Marshal(body)
	restore(out)
	return nil
}

// scrubWriter buffers a JSON response so it can be masked before it leaves.
type scrubWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (s *scrubWriter) WriteHeader(code int)        { s.status = code }
func (s *scrubWriter) Write(b []byte) (int, error) { return s.buf.Write(b) }
func (s *scrubWriter) finish() {
	body := s.buf.Bytes()
	if strings.Contains(s.Header().Get("Content-Type"), "json") {
		var v any
		if json.Unmarshal(body, &v) == nil {
			if out, err := json.Marshal(scrubJSON(v, false)); err == nil {
				body = append(out, '\n')
			}
		}
	}
	s.Header().Del("Content-Length")
	s.ResponseWriter.WriteHeader(s.status)
	s.ResponseWriter.Write(body)
}

// ------------------------------------------------------------- host handlers

// loadHostedSession resolves {sid} to a session the signed-in user hosts. Guests
// never get here (guestForbidden), and an admin cannot manage somebody else's session:
// a session is a conversation its host started.
func (a *App) loadHostedSession(w http.ResponseWriter, r *http.Request) (ShareSession, User, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return ShareSession{}, User{}, false
	}
	sid, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return ShareSession{}, User{}, false
	}
	sess, err := a.store.GetShareSession(sid)
	if err != nil || sess.HostID != u.ID {
		writeErr(w, http.StatusNotFound, "shared session not found")
		return ShareSession{}, User{}, false
	}
	return sess, u, true
}

func newShareToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// handleCreateShare starts a session on a stack and returns its link, once.
func (a *App) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	st, u, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	if st.OwnerID != u.ID {
		writeErr(w, http.StatusForbidden, "only a stack's owner can share it")
		return
	}
	allowed, maxMinutes := a.guestSessionSettings()
	if !allowed {
		writeErr(w, http.StatusForbidden, "guest sessions are turned off on this installation — an administrator can turn them on in Settings")
		return
	}
	var in struct {
		Minutes     int  `json:"minutes"`
		HideSecrets bool `json:"hideSecrets"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if in.Minutes == 0 {
		in.Minutes = shareDefaultMinutes
		if maxMinutes < in.Minutes {
			in.Minutes = maxMinutes
		}
	}
	if in.Minutes < shareMinMinutes || in.Minutes > maxMinutes {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("a session lasts from %d to %d minutes", shareMinMinutes, maxMinutes))
		return
	}
	token, err := newShareToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create the link")
		return
	}
	sess, err := a.store.CreateShareSession(u.ID, st.ID, hashTokenSecret(token), in.HideSecrets,
		time.Now().Add(time.Duration(in.Minutes)*time.Minute))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create the session")
		return
	}
	a.hubFor(sess).event("start", 0, u.Username, fmt.Sprintf("%s started a shared session on %s for %d minutes", u.Username, st.Name, in.Minutes))
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "url": publicURL(r) + "/join/" + token})
}

func (a *App) handleListShares(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	list, err := a.store.ListShareSessions(u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list sessions")
		return
	}
	if r.URL.Query().Get("live") == "1" {
		now := time.Now()
		live := []ShareSession{}
		for _, s := range list {
			if s.live(now) {
				live = append(live, s)
			}
		}
		list = live
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *App) handleGetShare(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := a.loadHostedSession(w, r)
	if !ok {
		return
	}
	guests, _ := a.store.ListShareGuests(sess.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"session": sess, "guests": guests, "controller": shareHubs.controller(sess.ID), "live": sess.live(time.Now()),
	})
}

// handleShareGuestAction admits, denies or removes a guest.
func (a *App) handleShareGuestAction(action string) func(*App) http.HandlerFunc {
	return func(a *App) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			sess, _, ok := a.loadHostedSession(w, r)
			if !ok {
				return
			}
			if !sess.live(time.Now()) {
				writeErr(w, http.StatusConflict, "this session has ended")
				return
			}
			gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid guest id")
				return
			}
			g, err := a.store.GetShareGuest(gid)
			if err != nil || g.SessionID != sess.ID {
				writeErr(w, http.StatusNotFound, "guest not found")
				return
			}
			h := a.hubFor(sess)
			switch action {
			case "admit":
				if g.State != guestWaiting {
					writeErr(w, http.StatusConflict, "this guest is not waiting in the lobby")
					return
				}
				a.store.SetShareGuestState(g.ID, guestAdmitted)
				h.event("join", g.ID, g.Name, g.Name+" joined")
			case "deny":
				if g.State != guestWaiting {
					writeErr(w, http.StatusConflict, "this guest is not waiting in the lobby")
					return
				}
				a.store.SetShareGuestState(g.ID, guestDenied)
				h.event("lobby", g.ID, g.Name, g.Name+" was not admitted")
			case "remove":
				if g.State != guestAdmitted {
					writeErr(w, http.StatusConflict, "this guest is not in the session")
					return
				}
				a.store.SetShareGuestState(g.ID, guestRemoved)
				h.dropGuest(g.ID)
				h.event("leave", g.ID, g.Name, g.Name+" was removed by the host")
			case "mute":
				var in struct {
					Muted bool `json:"muted"`
				}
				if err := decode(r, &in); err != nil {
					writeErr(w, http.StatusBadRequest, "invalid request body")
					return
				}
				a.store.SetShareGuestMuted(g.ID, in.Muted)
				word := "unmuted"
				if in.Muted {
					word = "muted"
				}
				h.event("control", g.ID, g.Name, g.Name+" was "+word+" by the host")
			}
			h.broadcastPresence()
			g, _ = a.store.GetShareGuest(g.ID)
			writeJSON(w, http.StatusOK, g)
		}
	}
}

// handleShareControl gives control to a guest, or takes it back for the host.
func (a *App) handleShareControl(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := a.loadHostedSession(w, r)
	if !ok {
		return
	}
	if !sess.live(time.Now()) {
		writeErr(w, http.StatusConflict, "this session has ended")
		return
	}
	var in struct {
		To json.RawMessage `json:"to"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var to int64
	if s := strings.Trim(string(in.To), `"`); s != "host" && s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, `"to" is a guest id or "host"`)
			return
		}
		g, err := a.store.GetShareGuest(n)
		if err != nil || g.SessionID != sess.ID || g.State != guestAdmitted {
			writeErr(w, http.StatusNotFound, "that guest is not in the session")
			return
		}
		to = n
	}
	a.hubFor(sess).setController(to)
	writeJSON(w, http.StatusOK, map[string]any{"controller": to})
}

func (a *App) handleEndShare(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := a.loadHostedSession(w, r)
	if !ok {
		return
	}
	a.endShare(sess, "ended")
	writeJSON(w, http.StatusOK, map[string]any{"status": "ended"})
}

// endShare ends a session wherever it is: its hub if one is running, the row always.
func (a *App) endShare(sess ShareSession, reason string) {
	if h := shareHubs.get(sess.ID); h != nil {
		h.end(reason)
		return
	}
	a.store.EndShareSession(sess.ID, reason)
}

func (a *App) handleStackTranscripts(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	list, err := a.store.ListStackShareSessions(st.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list transcripts")
		return
	}
	type row struct {
		ShareSession
		Messages int  `json:"messages"`
		Live     bool `json:"live"`
	}
	now := time.Now()
	out := []row{}
	for _, s := range list {
		out = append(out, row{s, a.store.CountShareMessages(s.ID), s.live(now)})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleShareTranscript downloads a session's transcript: chat, events and the guest
// actions, as text (the default) or JSON.
func (a *App) handleShareTranscript(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := a.loadHostedSession(w, r)
	if !ok {
		return
	}
	msgs, _ := a.store.ListShareMessages(sess.ID, 0)
	guests, _ := a.store.ListShareGuests(sess.ID)
	actions, _ := a.store.ListShareActions(sess.ID)
	name := fmt.Sprintf("dbcanvas-session-%d", sess.ID)
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.json"`)
		writeJSON(w, http.StatusOK, map[string]any{"session": sess, "guests": guests, "messages": msgs, "actions": actions})
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "DBCanvas shared session %d — stack %q, host %s\n", sess.ID, sess.StackName, sess.HostName)
	fmt.Fprintf(&b, "Started %s, expires %s", sess.CreatedAt, sess.ExpiresAt)
	if sess.EndedAt != nil {
		fmt.Fprintf(&b, ", ended %s (%s)", *sess.EndedAt, sess.EndedReason)
	}
	b.WriteString("\n\nGuests:\n")
	for _, g := range guests {
		fmt.Fprintf(&b, "  %s <%s> — %s, from %s\n", g.Name, g.Email, g.State, g.RemoteAddr)
	}
	b.WriteString("\nTranscript:\n")
	for _, m := range msgs {
		who := m.Author
		if m.Kind != "chat" {
			who = "*"
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", m.CreatedAt, who, m.Body)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.txt"`)
	w.Write([]byte(b.String()))
}

// ------------------------------------------------------------- guest handlers (public)

// joinLimiter slows down anyone hammering a link: ten joins a minute per address.
var joinLimiter = struct {
	sync.Mutex
	seen map[string][]time.Time
}{seen: map[string][]time.Time{}}

func joinAllowed(addr string) bool {
	joinLimiter.Lock()
	defer joinLimiter.Unlock()
	now := time.Now()
	keep := joinLimiter.seen[addr][:0]
	for _, t := range joinLimiter.seen[addr] {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= 10 {
		joinLimiter.seen[addr] = keep
		return false
	}
	joinLimiter.seen[addr] = append(keep, now)
	return true
}

func remoteHost(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// shareByToken resolves a link. Unknown, ended and expired all read the same.
func (a *App) shareByToken(r *http.Request) (ShareSession, bool) {
	tok := r.PathValue("token")
	if tok == "" || len(tok) > 100 {
		return ShareSession{}, false
	}
	if allowed, _ := a.guestSessionSettings(); !allowed {
		return ShareSession{}, false
	}
	sess, err := a.store.ShareSessionByToken(hashTokenSecret(tok))
	if err != nil || !sess.live(time.Now()) {
		return ShareSession{}, false
	}
	return sess, true
}

// currentGuestOf returns the browser's guest record for this session, if it has one.
func (a *App) currentGuestOf(r *http.Request, sess ShareSession) (ShareGuest, bool) {
	c, err := r.Cookie(guestCookieName)
	if err != nil || c.Value == "" {
		return ShareGuest{}, false
	}
	g, err := a.store.ShareGuestByCookie(hashTokenSecret(c.Value))
	if err != nil || g.SessionID != sess.ID {
		return ShareGuest{}, false
	}
	return g, true
}

// handleJoinInfo is what the join page shows before anyone types anything.
func (a *App) handleJoinInfo(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.shareByToken(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "this link has expired or was never valid")
		return
	}
	resp := map[string]any{"hostName": sess.HostName, "stackName": sess.StackName, "expiresAt": sess.ExpiresAt}
	if g, ok := a.currentGuestOf(r, sess); ok {
		resp["guest"] = g
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleJoin puts a guest in the lobby and gives their browser its cookie.
func (a *App) handleJoin(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.shareByToken(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "this link has expired or was never valid")
		return
	}
	if !joinAllowed(remoteHost(r)) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts — wait a minute and try again")
		return
	}
	if g, ok := a.currentGuestOf(r, sess); ok && (g.State == guestWaiting || g.State == guestAdmitted) {
		writeJSON(w, http.StatusOK, g) // already in: a double click, or a reload
		return
	}
	var in struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(in.Name)
	email := strings.TrimSpace(in.Email)
	switch {
	case name == "" || len([]rune(name)) > shareNameMax:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("a name of 1 to %d characters is required", shareNameMax))
		return
	case len(email) > shareEmailMax:
		writeErr(w, http.StatusBadRequest, "that email address is too long")
		return
	}
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		writeErr(w, http.StatusBadRequest, "a valid email address is required")
		return
	}
	secret, err := newShareToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to join")
		return
	}
	g, err := a.store.CreateShareGuest(sess.ID, name, email, hashTokenSecret(secret), remoteHost(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to join")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: guestCookieName, Value: secret, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
		Expires: sess.expires(), MaxAge: int(time.Until(sess.expires()).Seconds()),
	})
	h := a.hubFor(sess)
	h.event("lobby", g.ID, g.Name, fmt.Sprintf("%s (%s, from %s) is waiting in the lobby", g.Name, g.Email, g.RemoteAddr))
	h.broadcastPresence()
	a.notify(Notification{UserID: sess.HostID, Scope: "user", Type: "share.lobby", Severity: "info",
		Title:   g.Name + " wants to join your shared session",
		Body:    fmt.Sprintf("%s (%s) is waiting in the lobby of the session on %s.", g.Name, g.Email, sess.StackName),
		StackID: sess.StackID})
	writeJSON(w, http.StatusOK, g)
}

// handleJoinStatus is what the lobby polls.
func (a *App) handleJoinStatus(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	sess, err := a.store.ShareSessionByToken(hashTokenSecret(tok))
	if err != nil {
		writeErr(w, http.StatusNotFound, "this link has expired or was never valid")
		return
	}
	g, ok := a.currentGuestOf(r, sess)
	if !ok {
		writeErr(w, http.StatusNotFound, "you have not joined this session")
		return
	}
	state := g.State
	if !sess.live(time.Now()) {
		state = "ended"
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": state, "sessionId": sess.ID, "guest": g,
		"hostName": sess.HostName, "stackName": sess.StackName, "expiresAt": sess.ExpiresAt})
}

func (a *App) handleJoinLeave(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	sess, err := a.store.ShareSessionByToken(hashTokenSecret(tok))
	if err != nil {
		writeErr(w, http.StatusNotFound, "this link has expired or was never valid")
		return
	}
	if g, ok := a.currentGuestOf(r, sess); ok && (g.State == guestWaiting || g.State == guestAdmitted) {
		a.store.SetShareGuestState(g.ID, guestLeft)
		if h := shareHubs.get(sess.ID); h != nil {
			h.dropGuest(g.ID)
			h.event("leave", g.ID, g.Name, g.Name+" left")
			h.broadcastPresence()
		}
	}
	http.SetCookie(w, &http.Cookie{Name: guestCookieName, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"status": "left"})
}

// ------------------------------------------------------------- expiry

// endAllShares ends every session still running — the admin switch turned off.
func (a *App) endAllShares(reason string) {
	shareHubs.mu.Lock()
	hubs := make([]*shareHub, 0, len(shareHubs.hubs))
	for _, h := range shareHubs.hubs {
		hubs = append(hubs, h)
	}
	shareHubs.mu.Unlock()
	for _, h := range hubs {
		h.end(reason)
	}
	a.store.db.Exec(`UPDATE share_sessions SET ended_at = ?, ended_reason = ? WHERE ended_at IS NULL`, nowRFC3339(), reason)
}

var shareReaperOnce sync.Once

// startShareReaper ends sessions whose time ran out while no hub was watching them —
// across a restart, say. A live hub ends its own session on a timer (sharehub.go);
// this is the backstop, once a minute. It also deletes the records of sessions that
// ended longer ago than the retention allows, once an hour.
func (a *App) startShareReaper() {
	shareReaperOnce.Do(func() {
		go func() {
			var purged time.Time
			for {
				if time.Since(purged) >= time.Hour {
					if n := a.purgeShareSessions(); n > 0 {
						log.Printf("shared sessions: deleted %d past the %d-day retention", n, a.shareRetentionDays())
					}
					purged = time.Now()
				}
				ids, _ := a.store.ExpiredShareSessions(time.Now())
				for _, id := range ids {
					if sess, err := a.store.GetShareSession(id); err == nil {
						a.endShare(sess, "expired")
					}
				}
				time.Sleep(time.Minute)
			}
		}()
	})
}
