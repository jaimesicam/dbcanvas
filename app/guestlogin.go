package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// guestlogin.go — signing in without an account.
//
// With guest sign-in on, the sign-in screen offers "Continue as guest", which creates a guest
// account for that browser (users.guest_account = 1, named guest-<random>) and signs it in. It is
// an ordinary account with the user role: a guest designs, deploys and tears down stacks exactly
// as a registered user does, and its stacks are its own — another guest neither sees nor changes
// them. The browser keeps its account through its session; signing in as a guest again, from
// another browser or after signing out, is a new account.
//
// Not to be confused with a shared-session guest (share.go), who is admitted to one live
// session through a link and acts as its host. This is a whole account.
//
// Off by default, and an administrator's decision: anyone who can reach the sign-in page can
// use it. What makes it safe to leave around:
//
//   - A guest account has no password anybody knows (a random one, thrown away), so the only
//     way in is the guest button, and it cannot be given a password or made an administrator.
//   - Every guest account is in the Guests security group, whose pages and limits — stack
//     lifetime, stacks and nodes per account, node types (grouppolicy.go) — apply to each.
//   - An idle guest account is removed with its stacks after settingGuestIdleDays (7 by
//     default) without a sign-in, a request or a token use (reapIdleGuests).
//   - Turning the setting off refuses new guests and, unless an administrator chose to keep
//     them (settingGuestKeepAccess), signs out every guest session and revokes every guest
//     token; the check is made on every request as well.
//
// The API and dbcanvas-cli work as a guest too. POST /api/auth/guest gives a session cookie
// like POST /api/auth/login, and that session may create an API token (which is how
// `dbcanvas-cli login --guest` works). A guest token is an ordinary user's token — write scope
// at most, its lifetime capped by maxTokenDays. GET /api/setup/status says whether guest
// sign-in is on (guestLogin), and GET /api/me marks a guest account (guestAccount).
//
// An administrator manages guest accounts under Users like any other.

// settingGuestLogin is the app_settings key for the switch.
const settingGuestLogin = "guestLogin"

// settingGuestKeepAccess keeps existing guest accounts working when guest sign-in is turned
// off: new guests are refused, but nobody already signed in loses a session or a token.
const settingGuestKeepAccess = "guestKeepAccess"

// settingGuestIdleDays is how long a guest account may go unused before it is removed with
// its stacks; "0" keeps them.
const settingGuestIdleDays = "guestIdleDays"

const (
	errGuestLoginOff      = "guest sign-in is turned off"
	errGuestAccountFixed  = "a guest account has no password and cannot be given one: guests sign in with \"Continue as guest\""
	errGuestAccountNoAuth = "a guest account has no password: guests sign in with \"Continue as guest\""
)

// guestBlockedRoutes are what a guest account may not do: set a password, which would turn it
// into an account somebody could sign in to by name.
var guestBlockedRoutes = map[string]bool{
	"POST /api/me/password": true,
}

// guestKeepAccess reports whether guests keep working while guest sign-in is off.
func (a *App) guestKeepAccess() bool {
	v, err := a.store.AppSetting(settingGuestKeepAccess)
	return err == nil && v == "1"
}

// guestIdleDays is how many days unused a guest account is kept; 0 keeps them.
func (a *App) guestIdleDays() int {
	v, err := a.store.AppSetting(settingGuestIdleDays)
	if err != nil || strings.TrimSpace(v) == "" {
		return 7
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 7
	}
	return n
}

// guestLoginEnabled reports whether guest sign-in is on. Anything but an explicit "1"
// is off, so a missing or hand-edited row keeps the door shut. Read on every guest
// request, never cached, so turning it off takes effect at once.
func (a *App) guestLoginEnabled() bool {
	v, err := a.store.AppSetting(settingGuestLogin)
	return err == nil && v == "1"
}

// guestAccountUsable reports whether u may act right now: always for an ordinary
// account, and for the guest account only while guest sign-in is on.
func (a *App) guestAccountUsable(u User) bool {
	return !u.GuestAccount || a.guestLoginEnabled() || a.guestKeepAccess()
}

// guestGate is the per-request check for the guest account (apitokens.go,
// requireScope): refused outright when guest sign-in is off, and refused the routes
// that would change the shared account for everyone.
func (a *App) guestGate(w http.ResponseWriter, u User, rt apiRoute) bool {
	if !u.GuestAccount {
		return true
	}
	if !a.guestAccountUsable(u) {
		writeErr(w, http.StatusUnauthorized, errGuestLoginOff)
		return false
	}
	if guestBlockedRoutes[rt.Pattern()] {
		writeErr(w, http.StatusForbidden, errGuestAccountFixed)
		return false
	}
	return true
}

// endGuestAccess signs out every guest session and revokes every guest token. Called when
// guest sign-in is turned off, unless guests are to keep their access.
func (a *App) endGuestAccess() {
	if a.guestKeepAccess() {
		return
	}
	guests, err := a.store.ListGuestAccounts()
	if err != nil {
		return
	}
	for _, u := range guests {
		a.store.DeleteUserSessions(u.ID)
		a.store.RevokeUserAPITokens(u.ID)
	}
}

// reapIdleGuests removes guest accounts unused for guestIdleDays, tearing their stacks down
// first as deleting an account does (handleDeleteUser). Run by the stack reaper.
func (a *App) reapIdleGuests() {
	days := a.guestIdleDays()
	if days == 0 {
		return
	}
	guests, err := a.store.ListGuestAccounts()
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	for _, u := range guests {
		last := a.store.GuestLastActivity(u)
		if last.After(cutoff) {
			continue
		}
		stacks, _ := a.store.ListStacks(u.ID, false)
		for _, st := range stacks {
			a.teardownStack(st.ID)
			a.deleteStackRecord(st, "guest-idle", "")
		}
		if err := a.store.DeleteUser(u.ID); err == nil {
			log.Printf("guest account %s removed: unused since %s (%d stack(s) torn down)", u.Username, last.Format(time.RFC3339), len(stacks))
		}
	}
}

// handleGuestLogin signs the caller in as the guest account, creating it the first
// time.
func (a *App) handleGuestLogin(w http.ResponseWriter, r *http.Request) {
	// Before setup there is no administrator, and the guest account must never be
	// what makes an installation count as set up.
	if n, err := a.store.CountUsers(); err != nil || n == 0 {
		writeErr(w, http.StatusConflict, "this installation is not set up yet")
		return
	}
	if !a.guestLoginEnabled() {
		writeErr(w, http.StatusForbidden, errGuestLoginOff)
		return
	}
	if why := a.guestExposureBlocks(); why != "" {
		writeErr(w, http.StatusForbidden, why)
		return
	}
	// One account per guest: a browser keeps its own through its session, so a request that
	// already carries a live guest session is signed in to that account again, not a new one.
	if cur, ok := a.currentUser(r); ok && cur.GuestAccount && cur.Status == StatusApproved {
		writeJSON(w, http.StatusOK, cur)
		return
	}
	u, err := a.store.CreateGuestAccount()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to prepare a guest account")
		return
	}
	if err := a.issueSession(w, r, u.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// --- store ---

// ListGuestAccounts is every guest account.
func (s *Store) ListGuestAccounts() ([]User, error) {
	rows, err := s.db.Query("SELECT " + userCols + " FROM users WHERE guest_account = 1 ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := s.scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GuestLastActivity is the latest of an account's creation, its last sign-in, any of its
// sessions being seen and any of its tokens being used.
func (s *Store) GuestLastActivity(u User) time.Time {
	latest := u.CreatedAt
	for _, q := range []string{
		"SELECT COALESCE(last_login_at, '') FROM users WHERE id = ?",
		"SELECT COALESCE(MAX(last_seen_at), '') FROM sessions WHERE user_id = ?",
		"SELECT COALESCE(MAX(last_used_at), '') FROM api_tokens WHERE user_id = ?",
	} {
		var v string
		if s.db.QueryRow(q, u.ID).Scan(&v) == nil && v > latest {
			latest = v
		}
	}
	t, _ := time.Parse(time.RFC3339, latest)
	return t
}

// CreateGuestAccount creates a guest account: approved, the user role, and a random
// password that is hashed and forgotten, named guest-<6 hex digits>.
func (s *Store) CreateGuestAccount() (User, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return User{}, err
	}
	hash, err := hashPassword(hex.EncodeToString(raw))
	if err != nil {
		return User{}, err
	}
	var u User
	for i := 1; ; i++ {
		tag := make([]byte, 3)
		rand.Read(tag)
		name := "guest-" + hex.EncodeToString(tag)
		u, err = s.CreateUser(name, hash, RoleUser, StatusApproved)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrUserExists) || i >= 50 {
			return User{}, err
		}
	}
	if _, err := s.db.Exec("UPDATE users SET guest_account = 1 WHERE id = ?", u.ID); err != nil {
		s.DeleteUser(u.ID)
		return User{}, err
	}
	// A name, so the account reads as what it is wherever people are shown, and so
	// the app does not ask the guest to fill in a profile they may not change.
	s.SetUserProfile(u.ID, Profile{FirstName: "Guest", LastName: "User"})
	return s.GetUser(u.ID)
}
