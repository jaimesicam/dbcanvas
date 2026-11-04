package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
)

// guestlogin.go — signing in without an account.
//
// With guest sign-in on, the sign-in screen offers "Continue as guest", which signs
// the browser in to one built-in account, users.guest_account = 1. It is an ordinary
// account with the user role: a guest designs, deploys and tears down stacks exactly
// as a registered user does, and everyone who signs in as a guest shares that one
// account and so the same stacks.
//
// Not to be confused with a shared-session guest (share.go), who is admitted to one
// live session through a link and acts as its host. This is a whole account.
//
// Off by default, and an administrator's decision: anyone who can reach the sign-in
// page can use it. What makes it safe to leave around:
//
//   - The account has no password anybody knows (a random one, thrown away), so the
//     only way in is the guest button, and turning the setting off closes it.
//   - Turning the setting off signs out every guest session and revokes the
//     account's tokens; and the check is made on every request as well, so a guest
//     session that somehow survived stops working the moment the setting changes.
//   - Because the account is shared, guests cannot change what would lock the next
//     guest out: its password or its profile.
//   - It can never be made an administrator, nor given a password.
//
// The API and dbcanvas-cli work as a guest too. POST /api/auth/guest gives a session
// cookie like POST /api/auth/login, and that session may create an API token (which is
// how `dbcanvas login --guest` works). A guest token is an ordinary user's token —
// write scope at most, its lifetime capped by maxTokenDays — and it lives no longer
// than guest sign-in does: turning the setting off revokes it, and guestGate refuses
// it on every request meanwhile. GET /api/setup/status says whether guest sign-in is
// on (guestLogin), and GET /api/me marks the account (guestAccount).
//
// An administrator manages it under Users like any account: disabling it refuses
// guest sign-in, and deleting it (with its stacks) gives the next guest a fresh one.

// settingGuestLogin is the app_settings key for the switch.
const settingGuestLogin = "guestLogin"

const (
	errGuestLoginOff      = "guest sign-in is turned off"
	errGuestAccountFixed  = "the guest account is shared by everyone who signs in as a guest, so this cannot be changed from it"
	errGuestAccountNoAuth = "the guest account has no password: guests sign in with \"Continue as guest\""
)

// guestBlockedRoutes are what the shared guest account may not do: change its
// password or profile, which every other guest would then live with.
var guestBlockedRoutes = map[string]bool{
	"POST /api/me/password": true,
	"PUT /api/me/profile":   true,
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
	return !u.GuestAccount || a.guestLoginEnabled()
}

// guestGate is the per-request check for the guest account (apitokens.go,
// requireScope): refused outright when guest sign-in is off, and refused the routes
// that would change the shared account for everyone.
func (a *App) guestGate(w http.ResponseWriter, u User, rt apiRoute) bool {
	if !u.GuestAccount {
		return true
	}
	if !a.guestLoginEnabled() {
		writeErr(w, http.StatusUnauthorized, errGuestLoginOff)
		return false
	}
	if guestBlockedRoutes[rt.Pattern()] {
		writeErr(w, http.StatusForbidden, errGuestAccountFixed)
		return false
	}
	return true
}

// endGuestAccess signs out every guest session and revokes the guest account's
// tokens. Called when guest sign-in is turned off.
func (a *App) endGuestAccess() {
	u, err := a.store.GuestAccount()
	if err != nil {
		return
	}
	a.store.DeleteUserSessions(u.ID)
	a.store.RevokeUserAPITokens(u.ID)
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
	u, err := a.store.GuestAccount()
	if errors.Is(err, sql.ErrNoRows) {
		u, err = a.store.CreateGuestAccount()
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to prepare the guest account")
		return
	}
	if u.Status != StatusApproved {
		writeErr(w, http.StatusForbidden, "the guest account has been disabled")
		return
	}
	if err := a.issueSession(w, r, u.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// --- store ---

// GuestAccount is the built-in guest account, or sql.ErrNoRows before the first guest
// sign-in (or after an administrator deleted it).
func (s *Store) GuestAccount() (User, error) {
	row := s.db.QueryRow("SELECT " + userCols + " FROM users WHERE guest_account = 1 ORDER BY id LIMIT 1")
	return s.scanUser(row)
}

// CreateGuestAccount creates the guest account: approved, the user role, and a
// random password that is hashed and forgotten. It is named "guest", or "guest-2"
// and so on when somebody already registered that name.
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
		name := "guest"
		if i > 1 {
			name += "-" + strconv.Itoa(i)
		}
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
