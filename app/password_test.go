package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// password_test.go — the checks that make a password change safe rather than merely
// present.
//
// The two that matter most are TestChangePasswordNeedsTheCurrentOne (a stolen session
// must not be able to lock the owner out) and TestChangePasswordRefusesAToken (a
// leaked token must not escalate into an account takeover). The rest is the
// validation floor and the session/token cleanup.

// sessionSeq keeps each signedIn call's token distinct — sessions.token is unique,
// and a test that calls this in a loop would otherwise collide.
var sessionSeq int

// signedIn returns a request carrying a real session cookie for u, so the handler's
// cookie lookup — which is how it decides which session to keep — actually works.
func signedIn(t *testing.T, app *App, u User, body string) (*http.Request, string) {
	t.Helper()
	sessionSeq++
	token := fmt.Sprintf("sess-%s-%d", u.Username, sessionSeq)
	if err := app.store.CreateSession(token, u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	r := httptest.NewRequest("POST", "/api/me/password", strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	return r, token
}

// withPassword creates an account with a known password.
func withPassword(t *testing.T, app *App, name, password string) User {
	t.Helper()
	hash, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := app.store.CreateUser(name, hash, RoleUser, StatusApproved)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func TestChangePasswordHappyPath(t *testing.T) {
	app := newTestApp(t)
	u := withPassword(t, app, "jaime", "oldpassword")

	r, _ := signedIn(t, app, u, `{"currentPassword":"oldpassword","newPassword":"a-new-password"}`)
	w := httptest.NewRecorder()
	app.handleChangePassword(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("change returned %d: %s", w.Code, w.Body.String())
	}

	_, hash, err := app.store.CredByUsername("jaime")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !checkPassword(hash, "a-new-password") {
		t.Error("the new password does not work")
	}
	if checkPassword(hash, "oldpassword") {
		t.Error("the old password still works")
	}
	// The owner is told, because if the change was not theirs this is the only
	// warning they get.
	notes, _ := app.store.ListNotifications(u.ID, false, 10)
	found := false
	for _, n := range notes {
		if n.Type == "password.changed" {
			found = true
		}
	}
	if !found {
		t.Error("no notification was raised for a password change")
	}
}

// TestChangePasswordNeedsTheCurrentOne is the point of the endpoint: being signed in
// is not authorisation to change the password.
func TestChangePasswordNeedsTheCurrentOne(t *testing.T) {
	app := newTestApp(t)
	u := withPassword(t, app, "jaime", "oldpassword")

	r, _ := signedIn(t, app, u, `{"currentPassword":"guess","newPassword":"a-new-password"}`)
	w := httptest.NewRecorder()
	app.handleChangePassword(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a wrong current password returned %d, want 403: %s", w.Code, w.Body.String())
	}
	_, hash, _ := app.store.CredByUsername("jaime")
	if !checkPassword(hash, "oldpassword") {
		t.Error("the password changed despite the wrong current password")
	}
}

// TestChangePasswordRefusesAToken pins the second asymmetry: a token can read and
// write your stacks, and cannot take over your account.
func TestChangePasswordRefusesAToken(t *testing.T) {
	app := newTestApp(t)
	u := withPassword(t, app, "jaime", "oldpassword")
	secret, _ := tokenFor(t, app, u.ID, ScopeAdmin, 30)

	var route apiRoute
	for _, rt := range apiRoutes() {
		if rt.Pattern() == "POST /api/me/password" {
			route = rt
		}
	}
	if !route.NoToken {
		t.Fatal("POST /api/me/password must be marked NoToken")
	}
	reached := false
	w := httptest.NewRecorder()
	app.requireScope(route, func(http.ResponseWriter, *http.Request) { reached = true })(
		w, bearerReq("POST", "/api/me/password", secret))
	if w.Code != http.StatusForbidden || reached {
		t.Errorf("an admin-scope token reached the password endpoint: %d (reached=%v)", w.Code, reached)
	}
}

func TestChangePasswordValidation(t *testing.T) {
	app := newTestApp(t)
	u := withPassword(t, app, "jaime", "oldpassword")

	post := func(body string) *httptest.ResponseRecorder {
		r, _ := signedIn(t, app, u, body)
		w := httptest.NewRecorder()
		app.handleChangePassword(w, r)
		return w
	}

	for _, c := range []struct {
		name, body, wants string
		code              int
	}{
		{"too short", `{"currentPassword":"oldpassword","newPassword":"short"}`, "at least 8", 400},
		{"same as current", `{"currentPassword":"oldpassword","newPassword":"oldpassword"}`, "already your password", 400},
		{"trailing space", `{"currentPassword":"oldpassword","newPassword":"trailing space "}`, "space", 400},
		{"leading space", `{"currentPassword":"oldpassword","newPassword":" leading space"}`, "space", 400},
		{"empty new", `{"currentPassword":"oldpassword","newPassword":""}`, "at least 8", 400},
		{"garbage body", `{`, "invalid request body", 400},
	} {
		w := post(c.body)
		if w.Code != c.code {
			t.Errorf("%s: returned %d, want %d (%s)", c.name, w.Code, c.code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), c.wants) {
			t.Errorf("%s: message %q should mention %q", c.name, w.Body.String(), c.wants)
		}
	}
	// None of that changed anything.
	_, hash, _ := app.store.CredByUsername("jaime")
	if !checkPassword(hash, "oldpassword") {
		t.Error("a refused change still altered the password")
	}
}

// TestChangePasswordSignsOutOtherSessions: the usual reason to change a password is
// that somebody else might have it, so their browser has to stop working — and the
// tab doing the changing has to keep working.
func TestChangePasswordSignsOutOtherSessions(t *testing.T) {
	app := newTestApp(t)
	u := withPassword(t, app, "jaime", "oldpassword")

	// A session somewhere else, plus the one making the request.
	if err := app.store.CreateSession("elsewhere", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	r, mine := signedIn(t, app, u, `{"currentPassword":"oldpassword","newPassword":"a-new-password"}`)
	w := httptest.NewRecorder()
	app.handleChangePassword(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("change returned %d: %s", w.Code, w.Body.String())
	}

	if _, err := app.store.SessionUser("elsewhere"); err == nil {
		t.Error("the other session still works")
	}
	if _, err := app.store.SessionUser(mine); err != nil {
		t.Error("the session that made the change was signed out too")
	}
}

// TestChangePasswordAndTokens covers both sides of the opt-in: rotating a password
// must not silently break a CI job, and a password changed because it leaked must be
// able to take the tokens with it.
func TestChangePasswordAndTokens(t *testing.T) {
	t.Run("tokens survive by default", func(t *testing.T) {
		app := newTestApp(t)
		u := withPassword(t, app, "jaime", "oldpassword")
		secret, _ := tokenFor(t, app, u.ID, ScopeWrite, 30)

		r, _ := signedIn(t, app, u, `{"currentPassword":"oldpassword","newPassword":"a-new-password"}`)
		w := httptest.NewRecorder()
		app.handleChangePassword(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("change returned %d: %s", w.Code, w.Body.String())
		}
		if _, _, err := app.tokenAuth(bearerReq("GET", "/api/me", secret)); err != nil {
			t.Errorf("a routine password rotation revoked an API token: %v", err)
		}
	})

	t.Run("revoked on request", func(t *testing.T) {
		app := newTestApp(t)
		u := withPassword(t, app, "jaime", "oldpassword")
		secret, _ := tokenFor(t, app, u.ID, ScopeWrite, 30)

		r, _ := signedIn(t, app, u,
			`{"currentPassword":"oldpassword","newPassword":"a-new-password","revokeTokens":true}`)
		w := httptest.NewRecorder()
		app.handleChangePassword(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("change returned %d: %s", w.Code, w.Body.String())
		}
		if _, _, err := app.tokenAuth(bearerReq("GET", "/api/me", secret)); err == nil {
			t.Error("the token still works after an explicit revoke-on-change")
		}
		var res struct {
			TokensRevoked int `json:"tokensRevoked"`
		}
		json.Unmarshal(w.Body.Bytes(), &res)
		if res.TokensRevoked != 1 {
			t.Errorf("reported %d tokens revoked, want 1", res.TokensRevoked)
		}
	})
}

func TestChangePasswordNeedsAuth(t *testing.T) {
	app := newTestApp(t)
	w := httptest.NewRecorder()
	app.handleChangePassword(w, httptest.NewRequest("POST", "/api/me/password",
		strings.NewReader(`{"currentPassword":"x","newPassword":"yyyyyyyy"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated change returned %d, want 401", w.Code)
	}
}

// TestPasswordFloorIsShared: the sign-up rule and the change rule are the same rule.
func TestPasswordFloorIsShared(t *testing.T) {
	if minPasswordLen != 8 {
		t.Fatalf("minPasswordLen is %d; the messages say 8", minPasswordLen)
	}
	short := strings.Repeat("a", minPasswordLen-1)
	if err := (credentials{Username: "somebody", Password: short}).validate(); err == nil {
		t.Error("sign-up accepted a password below the floor")
	}
	if err := (credentials{Username: "somebody", Password: short + "a"}).validate(); err != nil {
		t.Errorf("sign-up rejected a password at the floor: %v", err)
	}
}

// TestSetUserPasswordOnAMissingAccount: the store reports it rather than silently
// succeeding, which is what lets the handler answer 404 instead of "saved".
func TestSetUserPasswordOnAMissingAccount(t *testing.T) {
	app := newTestApp(t)
	if err := app.store.SetUserPassword(4242, "irrelevant"); err == nil {
		t.Error("setting a password on a non-existent account succeeded")
	}
}

func TestDeleteUserSessionsExcept(t *testing.T) {
	app := newTestApp(t)
	u := withPassword(t, app, "jaime", "oldpassword")
	for _, tok := range []string{"a", "b", "c"} {
		if err := app.store.CreateSession(tok, u.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("create session %s: %v", tok, err)
		}
	}
	if err := app.store.DeleteUserSessionsExcept(u.ID, "b"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := app.store.SessionUser("b"); err != nil {
		t.Error("the kept session was deleted")
	}
	for _, gone := range []string{"a", "c"} {
		if _, err := app.store.SessionUser(gone); err == nil {
			t.Errorf("session %s survived", gone)
		}
	}
	// An empty keep means all of them, matching DeleteUserSessions.
	if err := app.store.DeleteUserSessionsExcept(u.ID, ""); err != nil {
		t.Fatalf("delete all: %v", err)
	}
	if _, err := app.store.SessionUser("b"); err == nil {
		t.Error("an empty keep left a session behind")
	}
}
