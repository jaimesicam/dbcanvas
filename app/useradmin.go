package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dbcanvas/internal/seal"
)

// useradmin.go — what an administrator can do to accounts beyond approving them:
// create one, edit its profile and username, change its role, see and end its
// sessions, clear its sign-in history, and get its owner back in when they have lost
// the password.
//
// Decisions, each the answer to a way this goes wrong:
//
//   - **Nobody changes their own role.** Demoting yourself is the one way an instance
//     can end up with no administrator at all, and promoting yourself is not a thing
//     an admin needs. Every other admin's role is fair game; you are always left as one.
//   - **A role change takes effect on the next request.** Sessions and tokens resolve
//     the account fresh each time (currentUser, requireAdmin), so a demoted admin's
//     open browser stops being an admin's without being signed out.
//   - **Setting a password signs the account out everywhere.** Whoever was using it
//     — the owner, or whoever made the reset necessary — is out. Tokens are only
//     revoked when asked, as with a change of your own (password.go).
//   - **A password an admin typed is temporary by default.** must_change_password
//     holds the account to one thing — choosing its own — until it has (the gate in
//     requireScope, passwordGateAllows). Otherwise the admin knows it for good.
//   - **A link is one-time and stored hashed.** It is a password in transit: it works
//     once, a newer link for the same account replaces it, and the admin never learns
//     the password the owner picks. A reset link lasts 24 hours; the link a new
//     account is created with lasts 7 days, since nobody is waiting on it.
//   - **Sign-in history is the admin's to clear,** for one account or all of them:
//     the last sign-in, and each session's address, browser and times. Clearing it
//     signs nobody out — that is a separate button, per account or for everyone but
//     the admin pressing it — and it is not history that
//     decides whether a link is an invite: invite_pending is.
//   - **Nothing that hands over an account is reachable with an API token** (NoToken):
//     creating one, setting a password, issuing a link. A leaked admin token must not
//     be able to do that.
//
// The admin's own password goes through Settings (password.go), which asks for the
// current one; the password endpoints refuse to act on the caller's own account so
// there is no way around that check.

const (
	resetLinkTTL  = 24 * time.Hour
	inviteLinkTTL = 7 * 24 * time.Hour

	linkReset  = "reset"
	linkInvite = "invite"
)

const passwordResetSchema = `
-- One row per password link an admin issued (useradmin.go): a reset for an existing
-- account, or the invite a new one is created with (purpose). The secret is in the
-- link only; token_hash is its SHA-256. used_at is set when the link is spent;
-- issuing a new link for the same account deletes the unused ones.
CREATE TABLE IF NOT EXISTS password_resets (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_by  INTEGER NOT NULL,
  token_hash  TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  expires_at  TEXT NOT NULL,
  used_at     TEXT,
  purpose     TEXT NOT NULL DEFAULT 'reset'
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_password_resets_token ON password_resets(token_hash);
CREATE INDEX IF NOT EXISTS idx_password_resets_user ON password_resets(user_id);`

// --- the must-change gate ---

const errPasswordChangeRequired = "an administrator set your password — choose your own before doing anything else"

// passwordGateRoutes is everything an account that must change its password can
// still reach: who it is, the change itself, and signing out. The app draws only the
// change screen meanwhile, so nothing else is asked for.
var passwordGateRoutes = map[string]bool{
	"GET /api/setup/status": true,
	"GET /api/me":           true,
	"POST /api/me/password": true,
	"POST /api/auth/logout": true,
	"GET /api/me/settings":  true,
}

func passwordGateAllows(u User, rt apiRoute) bool {
	return !u.MustChangePassword || passwordGateRoutes[rt.Pattern()]
}

// --- store ---

// SetRole changes an account's role.
func (s *Store) SetRole(id int64, role string) (User, error) {
	res, err := s.db.Exec("UPDATE users SET role = ? WHERE id = ?", role, id)
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, sql.ErrNoRows
	}
	return s.GetUser(id)
}

// SetUsername renames an account. Sessions and everything it owns hang off the id,
// so nothing else moves.
func (s *Store) SetUsername(id int64, username string) error {
	_, err := s.db.Exec("UPDATE users SET username = ? WHERE id = ?", username, id)
	if isUniqueViolation(err) {
		return ErrUserExists
	}
	return err
}

// SetMustChangePassword marks a password as temporary (or not).
func (s *Store) SetMustChangePassword(id int64, must bool) error {
	_, err := s.db.Exec("UPDATE users SET must_change_password = ? WHERE id = ?", must, id)
	return err
}

// ClearSignInHistory forgets when and where an account signed in — or every
// account's, for userID 0. Sessions stay signed in; only what was recorded about
// them goes. Last-seen fills in again with the next request, which is new activity.
func (s *Store) ClearSignInHistory(userID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	where, args := "", []any{}
	if userID != 0 {
		where, args = " WHERE id = ?", []any{userID}
	}
	if _, err := tx.Exec("UPDATE users SET last_login_at = NULL"+where, args...); err != nil {
		return err
	}
	if userID != 0 {
		where = " WHERE user_id = ?"
	}
	if _, err := tx.Exec("UPDATE sessions SET created_at = '', last_seen_at = '', ip = '', user_agent = ''"+where, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// SetInvitePending marks an account as created with a link not used yet. Setting a
// password, by any route, settles it (SetUserPassword).
func (s *Store) SetInvitePending(id int64) error {
	_, err := s.db.Exec("UPDATE users SET invite_pending = 1 WHERE id = ?", id)
	return err
}

// TouchLastLogin records a password sign-in.
func (s *Store) TouchLastLogin(id int64) error {
	_, err := s.db.Exec("UPDATE users SET last_login_at = ? WHERE id = ?", nowRFC3339(), id)
	return err
}

// UserSession is one signed-in browser, as an admin sees it. ID is the row's
// rowid: the token itself is never shown, not even hashed.
type UserSession struct {
	ID         int64  `json:"id"`
	CreatedAt  string `json:"createdAt,omitempty"`
	LastSeenAt string `json:"lastSeenAt,omitempty"`
	ExpiresAt  string `json:"expiresAt"`
	IP         string `json:"ip,omitempty"`
	UserAgent  string `json:"userAgent,omitempty"`
	// Current marks the session the request came from, which is never ended here.
	Current bool `json:"current,omitempty"`
}

// ListUserSessions returns an account's unexpired sessions, most recently used first.
func (s *Store) ListUserSessions(userID int64) ([]UserSession, error) {
	rows, err := s.db.Query(`SELECT rowid, created_at, last_seen_at, expires_at, ip, user_agent FROM sessions
		WHERE user_id = ? AND expires_at > ? ORDER BY last_seen_at DESC, rowid DESC`, userID, nowRFC3339())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserSession{}
	for rows.Next() {
		var x UserSession
		if err := rows.Scan(&x.ID, &x.CreatedAt, &x.LastSeenAt, &x.ExpiresAt, &x.IP, &x.UserAgent); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SessionRowID is the id ListUserSessions gives the session behind a token, or 0.
func (s *Store) SessionRowID(token string) int64 {
	var id int64
	s.db.QueryRow("SELECT rowid FROM sessions WHERE token = ?", seal.HashSecret(token)).Scan(&id)
	return id
}

// DeleteAllSessionsExcept signs every account out, apart from one session — the
// caller's own, so the admin who pressed the button is not signed out by it.
func (s *Store) DeleteAllSessionsExcept(keep string) (int64, error) {
	res, err := s.db.Exec("DELETE FROM sessions WHERE token != ?", seal.HashSecret(keep))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteUserSession ends one session, by the id ListUserSessions gave it.
func (s *Store) DeleteUserSession(userID, sessionID int64) (bool, error) {
	res, err := s.db.Exec("DELETE FROM sessions WHERE user_id = ? AND rowid = ?", userID, sessionID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// sessionStats is, per account, how many live sessions it has and when one was last used.
type sessionStats struct {
	Count    int
	LastSeen string
}

func (s *Store) SessionStats() (map[int64]sessionStats, error) {
	rows, err := s.db.Query(`SELECT user_id, COUNT(*), MAX(last_seen_at) FROM sessions
		WHERE expires_at > ? GROUP BY user_id`, nowRFC3339())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]sessionStats{}
	for rows.Next() {
		var id int64
		var st sessionStats
		if err := rows.Scan(&id, &st.Count, &st.LastSeen); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// CreatePasswordReset issues a password link for an account, replacing any unused
// one it already had, and returns the secret and its expiry.
func (s *Store) CreatePasswordReset(userID, createdBy int64, purpose string, ttl time.Duration) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := hex.EncodeToString(raw)
	expires := time.Now().Add(ttl).UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM password_resets WHERE user_id = ? AND used_at IS NULL", userID); err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.Exec(
		"INSERT INTO password_resets (user_id, created_by, token_hash, created_at, expires_at, purpose) VALUES (?,?,?,?,?,?)",
		userID, createdBy, seal.HashSecret(token), nowRFC3339(), expires.Format(time.RFC3339), purpose,
	); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, tx.Commit()
}

// passwordLink is a live password link, resolved.
type passwordLink struct {
	ID      int64
	User    User
	Expires time.Time
	Purpose string
}

// PasswordReset resolves an unused, unexpired link secret.
func (s *Store) PasswordReset(token string) (passwordLink, error) {
	var l passwordLink
	var userID int64
	var expiresStr string
	var used sql.NullString
	err := s.db.QueryRow(
		"SELECT id, user_id, expires_at, used_at, purpose FROM password_resets WHERE token_hash = ?", seal.HashSecret(token),
	).Scan(&l.ID, &userID, &expiresStr, &used, &l.Purpose)
	if err != nil {
		return passwordLink{}, err
	}
	l.Expires, err = time.Parse(time.RFC3339, expiresStr)
	if err != nil || used.Valid || time.Now().After(l.Expires) {
		return passwordLink{}, sql.ErrNoRows
	}
	if l.User, err = s.GetUser(userID); err != nil {
		return passwordLink{}, err
	}
	return l, nil
}

// SpendPasswordReset marks a link used. It reports false when another request spent
// it first, so a link cannot set two passwords.
func (s *Store) SpendPasswordReset(id int64) (bool, error) {
	res, err := s.db.Exec("UPDATE password_resets SET used_at = ? WHERE id = ? AND used_at IS NULL", nowRFC3339(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// --- shared checks ---

// newPasswordProblem is the rule every password set on someone's behalf follows: the
// same floor as sign-up, and no leading or trailing space (see handleChangePassword).
func newPasswordProblem(pw string) string {
	if len(pw) < minPasswordLen {
		return "the password must be at least 8 characters"
	}
	if strings.TrimSpace(pw) != pw {
		return "the password starts or ends with a space — remove it, or that space becomes part of the password"
	}
	return ""
}

// validUsername is the sign-up rule (credentials.validate), on its own.
func validUsername(name string) (string, error) {
	name = strings.TrimSpace(name)
	if len(name) < 3 || len(name) > 32 {
		return "", errors.New("username must be 3–32 characters")
	}
	return name, nil
}

// targetUser resolves the {id} an admin action is about. With own non-empty, the
// caller's own account is refused with that message. It writes the error response
// itself and reports whether to continue.
func (a *App) targetUser(w http.ResponseWriter, r *http.Request, own string) (User, User, bool) {
	me, _ := a.currentUser(r)
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid user id")
		return User{}, User{}, false
	}
	if own != "" && id == me.ID {
		writeErr(w, http.StatusBadRequest, own)
		return User{}, User{}, false
	}
	u, err := a.store.GetUser(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "user not found")
			return User{}, User{}, false
		}
		writeErr(w, http.StatusInternalServerError, "failed to read user")
		return User{}, User{}, false
	}
	return me, u, true
}

// linkURL is the address a password link is opened at (web/src/auth/AuthScreens.jsx).
func linkURL(r *http.Request, token string) string {
	return publicURL(r) + "/reset-password/" + token
}

// --- handlers ---

// handleCreateUser is an admin creating an account outright: approved, with the role
// they chose, and either a link for the owner to set a password or a temporary one
// the admin typed.
func (a *App) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		// Password, when set, is a temporary password the owner must change at first
		// sign-in. When empty, the response carries a link instead.
		Password string `json:"password"`
		Profile
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	username, err := validUsername(in.Username)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Role == "" {
		in.Role = RoleUser
	}
	if in.Role != RoleAdmin && in.Role != RoleUser {
		writeErr(w, http.StatusBadRequest, `role must be "admin" or "user"`)
		return
	}
	if in.Password != "" {
		if p := newPasswordProblem(in.Password); p != "" {
			writeErr(w, http.StatusBadRequest, p)
			return
		}
	}
	if err := in.Profile.clean(true); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if taken, err := a.store.EmailInUse(in.Profile.Email, 0); err != nil || taken {
		writeErr(w, http.StatusConflict, ErrEmailTaken.Error())
		return
	}
	// With no password given, the account gets one nobody knows: it cannot be signed
	// in to until the link is used.
	pw := in.Password
	if pw == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to create the account")
			return
		}
		pw = hex.EncodeToString(raw)
	}
	hash, err := hashPassword(pw)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to hash the password")
		return
	}
	u, err := a.store.CreateUser(username, hash, in.Role, StatusApproved)
	if err != nil {
		if errors.Is(err, ErrUserExists) {
			writeErr(w, http.StatusConflict, "username already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to create user")
		return
	}
	if err := a.store.SetUserProfile(u.ID, in.Profile); err != nil {
		a.store.DeleteUser(u.ID)
		writeErr(w, http.StatusInternalServerError, "failed to save the profile")
		return
	}
	a.stampWhatsNewSeen(u.ID)
	resp := map[string]any{}
	if in.Password != "" {
		a.store.SetMustChangePassword(u.ID, true)
	} else {
		a.store.SetInvitePending(u.ID)
		me, _ := a.currentUser(r)
		token, expires, err := a.store.CreatePasswordReset(u.ID, me.ID, linkInvite, inviteLinkTTL)
		if err != nil {
			a.store.DeleteUser(u.ID)
			writeErr(w, http.StatusInternalServerError, "failed to create the link")
			return
		}
		resp["url"] = linkURL(r, token)
		resp["expiresAt"] = expires.Format(time.RFC3339)
	}
	if u, err = a.store.GetUser(u.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read the account back")
		return
	}
	resp["user"] = u
	writeJSON(w, http.StatusCreated, resp)
}

// handleUpdateUser is an admin correcting an account's username and profile. Their
// own included: nothing here is a privilege.
func (a *App) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Profile
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	_, target, ok := a.targetUser(w, r, "")
	if !ok {
		return
	}
	username, err := validUsername(in.Username)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Not required: an account from before profiles may have none, and an admin
	// fixing its username should not have to invent one.
	if err := in.Profile.clean(false); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if taken, err := a.store.EmailInUse(in.Profile.Email, target.ID); err != nil || taken {
		writeErr(w, http.StatusConflict, ErrEmailTaken.Error())
		return
	}
	if username != target.Username {
		if err := a.store.SetUsername(target.ID, username); err != nil {
			if errors.Is(err, ErrUserExists) {
				writeErr(w, http.StatusConflict, "username already exists")
				return
			}
			writeErr(w, http.StatusInternalServerError, "failed to rename the account")
			return
		}
	}
	if err := a.store.SetUserProfile(target.ID, in.Profile); err != nil {
		if errors.Is(err, ErrEmailTaken) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to save the profile")
		return
	}
	if username != target.Username {
		a.notify(Notification{UserID: target.ID, Scope: "user", Type: "user.renamed", Severity: "warning",
			Title: "Your username changed",
			Body:  "An administrator renamed your account from " + target.Username + " to " + username + ". Sign in with the new name from now on."})
	}
	u, err := a.store.GetUser(target.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read the account back")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (a *App) handleSetUserRole(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Role string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if in.Role != RoleAdmin && in.Role != RoleUser {
		writeErr(w, http.StatusBadRequest, `role must be "admin" or "user"`)
		return
	}
	_, target, ok := a.targetUser(w, r, "you cannot change your own role")
	if !ok {
		return
	}
	if target.Role == in.Role {
		writeJSON(w, http.StatusOK, target)
		return
	}
	u, err := a.store.SetRole(target.ID, in.Role)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to update user")
		return
	}
	title, body := "You are now an administrator", "An administrator gave your account administrator access."
	if in.Role == RoleUser {
		title, body = "Administrator access removed", "An administrator changed your account to a regular user."
	}
	a.notify(Notification{UserID: u.ID, Scope: "user", Type: "user.role", Severity: "warning", Title: title, Body: body})
	writeJSON(w, http.StatusOK, u)
}

func (a *App) handleSetUserPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		NewPassword  string `json:"newPassword"`
		RevokeTokens bool   `json:"revokeTokens"`
		// RequireChange makes the password temporary: the owner must choose their own
		// at next sign-in. The UI ticks it by default.
		RequireChange bool `json:"requireChange"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	_, target, ok := a.targetUser(w, r, "change your own password from Settings")
	if !ok {
		return
	}
	if p := newPasswordProblem(in.NewPassword); p != "" {
		writeErr(w, http.StatusBadRequest, p)
		return
	}
	hash, err := hashPassword(in.NewPassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to hash the new password")
		return
	}
	if err := a.store.SetUserPassword(target.ID, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the new password")
		return
	}
	if in.RequireChange {
		a.store.SetMustChangePassword(target.ID, true)
	}
	a.store.DeleteUserSessions(target.ID)
	if in.RevokeTokens {
		a.store.RevokeUserAPITokens(target.ID)
	}
	a.notify(Notification{UserID: target.ID, Scope: "user", Type: "password.changed", Severity: "warning",
		Title: "Password reset by an administrator",
		Body:  "An administrator set a new password for your account and signed out all of its sessions."})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *App) handleCreateResetLink(w http.ResponseWriter, r *http.Request) {
	me, target, ok := a.targetUser(w, r, "change your own password from Settings")
	if !ok {
		return
	}
	// An account whose invite was never used is still waiting on it: a new link for
	// it is another invite, and lasts as long as one.
	purpose, ttl := linkReset, resetLinkTTL
	if target.InvitePending {
		purpose, ttl = linkInvite, inviteLinkTTL
	}
	token, expires, err := a.store.CreatePasswordReset(target.ID, me.ID, purpose, ttl)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create the reset link")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"url":       linkURL(r, token),
		"expiresAt": expires.Format(time.RFC3339),
		"purpose":   purpose,
	})
}

// currentSession is the cookie session a request came from: its token ("" for a
// bearer token) and its id (0 when there is none).
func (a *App) currentSession(r *http.Request) (string, int64) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return "", 0
	}
	return c.Value, a.store.SessionRowID(c.Value)
}

func (a *App) handleListUserSessions(w http.ResponseWriter, r *http.Request) {
	_, target, ok := a.targetUser(w, r, "")
	if !ok {
		return
	}
	list, err := a.store.ListUserSessions(target.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list sessions")
		return
	}
	if _, cur := a.currentSession(r); cur != 0 {
		for i := range list {
			list[i].Current = list[i].ID == cur
		}
	}
	writeJSON(w, http.StatusOK, list)
}

// handleEndUserSessions signs an account out everywhere — without touching its
// password, for "I left myself signed in on the lab PC". On the caller's own account
// it keeps the session asking, so an admin can clear out their old ones.
func (a *App) handleEndUserSessions(w http.ResponseWriter, r *http.Request) {
	_, target, ok := a.targetUser(w, r, "")
	if !ok {
		return
	}
	keep, _ := a.currentSession(r)
	n := 0
	if list, err := a.store.ListUserSessions(target.ID); err == nil {
		n = len(list)
	}
	if err := a.store.DeleteUserSessionsExcept(target.ID, keep); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to sign out the sessions")
		return
	}
	if keep != "" && a.store.SessionRowID(keep) != 0 {
		n-- // the caller's own, which was kept
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "signedOut": n})
}

// handleEndAllSessions signs everybody out — for after a suspected leak, or before
// maintenance — except the session that asked. Passwords and API tokens are untouched.
func (a *App) handleEndAllSessions(w http.ResponseWriter, r *http.Request) {
	keep := ""
	if c, err := r.Cookie(cookieName); err == nil {
		keep = c.Value
	}
	n, err := a.store.DeleteAllSessionsExcept(keep)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to sign out the sessions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "signedOut": n})
}

func (a *App) handleEndUserSession(w http.ResponseWriter, r *http.Request) {
	_, target, ok := a.targetUser(w, r, "")
	if !ok {
		return
	}
	sid, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return
	}
	if _, cur := a.currentSession(r); cur != 0 && sid == cur {
		writeErr(w, http.StatusBadRequest, "that is this session — sign out from the account menu")
		return
	}
	found, err := a.store.DeleteUserSession(target.ID, sid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to sign out the session")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleClearSignInHistory clears one account's sign-in history; the caller's own
// included, since it is nobody's but the admins' to keep.
func (a *App) handleClearSignInHistory(w http.ResponseWriter, r *http.Request) {
	_, target, ok := a.targetUser(w, r, "")
	if !ok {
		return
	}
	if err := a.store.ClearSignInHistory(target.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to clear the sign-in history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *App) handleClearAllSignInHistory(w http.ResponseWriter, r *http.Request) {
	if err := a.store.ClearSignInHistory(0); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to clear the sign-in history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

const deadLink = "this link is invalid, expired or already used — ask an administrator for a new one"

// handleResetLinkInfo says whose password a link sets, and whether it is an invite,
// so the page can name the account — and say that the link is dead — before anything
// is typed.
func (a *App) handleResetLinkInfo(w http.ResponseWriter, r *http.Request) {
	l, err := a.store.PasswordReset(r.PathValue("token"))
	if err != nil {
		writeErr(w, http.StatusNotFound, deadLink)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username": l.User.Username, "firstName": l.User.FirstName,
		"expiresAt": l.Expires.Format(time.RFC3339), "purpose": l.Purpose,
	})
}

func (a *App) handleUseResetLink(w http.ResponseWriter, r *http.Request) {
	var in struct {
		NewPassword string `json:"newPassword"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	l, err := a.store.PasswordReset(r.PathValue("token"))
	if err != nil {
		writeErr(w, http.StatusNotFound, deadLink)
		return
	}
	// Checked before the link is spent, so a too-short password does not burn it.
	if p := newPasswordProblem(in.NewPassword); p != "" {
		writeErr(w, http.StatusBadRequest, p)
		return
	}
	hash, err := hashPassword(in.NewPassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to hash the new password")
		return
	}
	if spent, err := a.store.SpendPasswordReset(l.ID); err != nil || !spent {
		writeErr(w, http.StatusNotFound, deadLink)
		return
	}
	if err := a.store.SetUserPassword(l.User.ID, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the new password")
		return
	}
	a.store.DeleteUserSessions(l.User.ID)
	if l.Purpose == linkReset {
		a.notify(Notification{UserID: l.User.ID, Scope: "user", Type: "password.changed", Severity: "warning",
			Title: "Password changed", Body: "Your password was changed with a reset link, and every session was signed out."})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "username": l.User.Username})
}
