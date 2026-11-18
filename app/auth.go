package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

const (
	cookieName = "dbcanvas_session"
	sessionTTL = 7 * 24 * time.Hour
)

// issueSession creates a browser session and sets the session cookie.
func (a *App) issueSession(w http.ResponseWriter, r *http.Request) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := hex.EncodeToString(raw)
	expires := time.Now().Add(sessionTTL)
	if err := a.store.CreateSession(token, expires); err != nil {
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

// requireAdmin wraps a system endpoint: the owner's browser, the control socket, or a
// token with the admin scope (requireScope checks the scope), never a shared-session guest.
func (a *App) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.currentUser(r); !ok {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if p, ok := principalOf(r); ok && p.Guest != nil {
			writeErr(w, http.StatusForbidden, "only the host can change this")
			return
		}
		next(w, r)
	}
}

// --- handlers ---

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"authenticated": false,
		"user":          nil,
		// The build, so the UI can show it and decide whether this account has
		// seen its release notes (see whatsnew.go). This endpoint rather than
		// /api/me because the client already calls it on every load, and because
		// /api/me returns a bare User that nothing should be appended to.
		"version": appVersion,
	}
	if u, ok := a.currentUser(r); ok {
		resp["authenticated"] = true
		resp["user"] = u
		// A shared-session guest is "signed in" as the host they act as; this says who
		// they really are, so the UI can draw a guest's shell (share.go).
		if p, ok := principalOf(r); ok && p.Guest != nil {
			resp["guest"] = map[string]any{"guestId": p.Guest.GuestID, "sessionId": p.Guest.SessionID,
				"name": p.Guest.Name, "email": p.Guest.Email, "driving": p.Guest.Driving,
				"avatar": p.Guest.Avatar}
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
