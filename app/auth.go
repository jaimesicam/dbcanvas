package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	cookieName = "dbcanvas_session"
	sessionTTL = 7 * 24 * time.Hour
)

// hashPassword hashes a plaintext password with bcrypt.
func hashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// checkPassword compares a bcrypt hash against a plaintext password.
func checkPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// issueSession creates a session for the user and sets the session cookie.
func (a *App) issueSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := hex.EncodeToString(raw)
	expires := time.Now().Add(sessionTTL)
	if err := a.store.CreateSessionFrom(token, userID, expires, remoteHost(r), r.UserAgent()); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		Expires:  expires,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return nil
}

// clearSessionCookie expires the session cookie on the client.
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   -1,
	})
}

// currentUser returns the authenticated user for the request, if any.
//
// Two credentials resolve to the same thing. A bearer API token has already been
// verified and scope-checked by requireScope (see apitokens.go), which leaves the
// account on the request context — so the token path here is a context read, not a
// second database lookup. Everything after this point, in all 200-odd handlers, is
// identical either way: the API is the app, not a parallel implementation of it.
func (a *App) currentUser(r *http.Request) (User, bool) {
	if p, ok := principalOf(r); ok {
		return p.User, true
	}
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return User{}, false
	}
	u, err := a.store.SessionUser(c.Value)
	if err != nil {
		return User{}, false
	}
	return u, true
}

// requireAdmin wraps a handler, enforcing an authenticated admin.
func (a *App) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := a.currentUser(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if u.Role != RoleAdmin {
			writeErr(w, http.StatusForbidden, "admin access required")
			return
		}
		next(w, r)
	}
}

// --- handlers ---

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := a.store.CountUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read state")
		return
	}
	resp := map[string]any{
		"initialized":   n > 0,
		"authenticated": false,
		"user":          nil,
		// The build, so the UI can show it and decide whether this account has
		// seen its release notes (see whatsnew.go). This endpoint rather than
		// /api/me because the client already calls it on every load, and because
		// /api/me returns a bare User that nothing should be appended to.
		"version": appVersion,
		// The dbcanvas command: no passwords; a browser signs in by start link (soloweb.go).
		"solo": a.solo,
	}
	if u, ok := a.currentUser(r); ok {
		resp["authenticated"] = true
		resp["user"] = u
		// A shared-session guest is "signed in" as the host they act as; this says who
		// they really are, so the UI can draw a guest's shell (share.go).
		if p, ok := principalOf(r); ok && p.Guest != nil {
			resp["guest"] = map[string]any{"guestId": p.Guest.GuestID, "sessionId": p.Guest.SessionID,
				"name": p.Guest.Name, "email": p.Guest.Email, "driving": p.Guest.Driving,
				"avatar": p.Guest.Avatar, "account": p.Guest.Account}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		a.store.DeleteSession(c.Value)
	}
	clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// A request made with an API token also gets that token's name, scope and expiry, so a
	// client can say what it is signed in with (`dbcanvas-cli whoami`) without keeping its own
	// copy of those, which goes stale or was never written.
	var tok *meToken
	if p, ok := principalOf(r); ok && p.Token != nil {
		tok = &meToken{ID: p.Token.ID, Name: p.Token.Name, Prefix: p.Token.Prefix, Scope: p.Token.Scope,
			CreatedAt: p.Token.CreatedAt, ExpiresAt: p.Token.ExpiresAt}
	}
	writeJSON(w, http.StatusOK, struct {
		User
		Token *meToken `json:"token,omitempty"`
	}{u, tok})
}

// meToken is the API token a /api/me request was made with. ExpiresAt is absent for a token
// that never expires.
type meToken struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	Prefix    string  `json:"prefix"`
	Scope     string  `json:"scope"`
	CreatedAt string  `json:"createdAt"`
	ExpiresAt *string `json:"expiresAt,omitempty"`
}
