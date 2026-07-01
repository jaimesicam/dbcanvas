package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	cookieName = "dbcanvas_session"
	sessionTTL = 7 * 24 * time.Hour
)

// credentials is the shared shape for setup/register/login bodies.
type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// validate enforces username 3–32 chars and password ≥ 8.
func (c credentials) validate() error {
	u := strings.TrimSpace(c.Username)
	if len(u) < 3 || len(u) > 32 {
		return errors.New("username must be 3–32 characters")
	}
	if len(c.Password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	return nil
}

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
	if err := a.store.CreateSession(token, userID, expires); err != nil {
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
func (a *App) currentUser(r *http.Request) (User, bool) {
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
	}
	if u, ok := a.currentUser(r); ok {
		resp["authenticated"] = true
		resp["user"] = u
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	n, err := a.store.CountUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read state")
		return
	}
	if n > 0 {
		writeErr(w, http.StatusConflict, "already initialized")
		return
	}
	var c credentials
	if err := decode(r, &c); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := c.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := hashPassword(c.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	u, err := a.store.CreateUser(strings.TrimSpace(c.Username), hash, RoleAdmin, StatusApproved)
	if err != nil {
		if errors.Is(err, ErrUserExists) {
			writeErr(w, http.StatusConflict, "username already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to create user")
		return
	}
	if err := a.issueSession(w, r, u.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (a *App) handleRegister(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := decode(r, &c); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := c.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := hashPassword(c.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	nu, err := a.store.CreateUser(strings.TrimSpace(c.Username), hash, RoleUser, StatusPending)
	if err != nil {
		if errors.Is(err, ErrUserExists) {
			writeErr(w, http.StatusConflict, "username already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to create user")
		return
	}
	a.notify(Notification{Scope: "admin", Type: "user.pending", Severity: "warning",
		Title: "New account awaiting approval", Body: nu.Username + " registered and needs approval."})
	writeJSON(w, http.StatusCreated, map[string]any{
		"status":  "pending",
		"message": "Your account was created and is awaiting administrator approval.",
	})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := decode(r, &c); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	u, hash, err := a.store.CredByUsername(strings.TrimSpace(c.Username))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusUnauthorized, "invalid username or password")
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to read user")
		return
	}
	if !checkPassword(hash, c.Password) {
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	switch u.Status {
	case StatusPending:
		writeErr(w, http.StatusForbidden, "your account is awaiting approval")
		return
	case StatusRejected:
		writeErr(w, http.StatusForbidden, "your account request was rejected")
		return
	case StatusDisabled:
		writeErr(w, http.StatusForbidden, "your account has been disabled")
		return
	}
	if err := a.issueSession(w, r, u.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	writeJSON(w, http.StatusOK, u)
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
	writeJSON(w, http.StatusOK, u)
}
