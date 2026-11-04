package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGuestLogin(t *testing.T) {
	app := newTestApp(t)
	login := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		app.handleGuestLogin(w, httptest.NewRequest("POST", "/api/auth/guest", nil))
		return w
	}
	setSettings := func(body string) {
		w := httptest.NewRecorder()
		app.handleUpdateSystemSettings(w, httptest.NewRequest("PUT", "/api/system/settings", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("settings %s: %d %s", body, w.Code, w.Body)
		}
	}

	// Before setup it cannot be what creates the first account, whatever the setting.
	app.store.SetAppSetting(settingGuestLogin, "1")
	if code := login().Code; code != http.StatusConflict {
		t.Fatalf("guest sign-in before setup returned %d", code)
	}
	app.store.SetAppSetting(settingGuestLogin, "0")
	admin, _ := app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	// Somebody already registered the name; the guest account takes another.
	app.store.CreateUser("guest", "x", RoleUser, StatusApproved)

	// Off by default.
	if app.systemSettings("").GuestLogin {
		t.Fatal("guest sign-in is on by default")
	}
	if code := login().Code; code != http.StatusForbidden {
		t.Fatalf("guest sign-in while off returned %d", code)
	}
	if _, err := app.store.GuestAccount(); err == nil {
		t.Fatal("a refused guest sign-in created the guest account")
	}

	// On: a regular user, signed in.
	setSettings(`{"guestLogin":true}`)
	w := login()
	if w.Code != http.StatusOK || len(w.Result().Cookies()) == 0 {
		t.Fatalf("guest sign-in: %d, cookies %d", w.Code, len(w.Result().Cookies()))
	}
	g, err := app.store.GuestAccount()
	if err != nil || !g.GuestAccount || g.Role != RoleUser || g.Status != StatusApproved || g.Username != "guest-2" {
		t.Fatalf("guest account: %+v %v", g, err)
	}
	// A second guest gets the same account.
	login()
	if users, _ := app.store.ListUsers(); len(users) != 3 {
		t.Errorf("a second guest sign-in made another account: %d accounts", len(users))
	}

	// Guests have neither API tokens nor much else by default (secgroups.go); this
	// test is about the guest account, so give the group the API.
	app.store.SetAppSetting(settingSecurityGroups, `{"guest":{"api":true}}`)

	reached := false
	call := func(pattern string) int {
		reached = false
		r, _ := signedIn(t, app, g, "")
		w := httptest.NewRecorder()
		app.requireScope(routeFor(t, pattern), func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})(w, r)
		return w.Code
	}
	if code := call("POST /api/stacks"); code != http.StatusOK || !reached {
		t.Errorf("a guest creating a stack returned %d", code)
	}
	if code := call("POST /api/tokens"); code != http.StatusOK || !reached {
		t.Errorf("a guest creating an API token returned %d", code)
	}
	for _, p := range []string{"POST /api/me/password", "PUT /api/me/profile"} {
		if code := call(p); code != http.StatusForbidden || reached {
			t.Errorf("%s as the guest returned %d", p, code)
		}
	}

	// Never an admin, never a password.
	w = httptest.NewRecorder()
	app.handleSetUserRole(w, asAdmin(admin, "POST", g.ID, `{"role":"admin"}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("promoting the guest account returned %d", w.Code)
	}
	w = httptest.NewRecorder()
	app.handleSetUserPassword(w, asAdmin(admin, "POST", g.ID, `{"newPassword":"password123"}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("setting the guest account's password returned %d", w.Code)
	}

	// A guest token, through the real handler: a user's token, capped like one.
	{
		r, _ := signedIn(t, app, g, `{"name":"cli","scope":"admin","days":30}`)
		w := httptest.NewRecorder()
		app.handleCreateToken(w, withPrincipal(r, principal{User: g}))
		if w.Code != http.StatusForbidden {
			t.Errorf("a guest admin-scope token returned %d", w.Code)
		}
	}
	guestSecret, _ := tokenFor(t, app, g.ID, ScopeWrite, 30)
	tokenCall := func() int {
		w := httptest.NewRecorder()
		app.requireScope(routeFor(t, "GET /api/stacks"), func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})(w, bearerReq("GET", "/api/stacks", guestSecret))
		return w.Code
	}
	if code := tokenCall(); code != http.StatusOK {
		t.Errorf("a guest token while on returned %d", code)
	}

	// Off again: the open session stops working at once, and is gone.
	r, _ := signedIn(t, app, g, "")
	if _, ok := app.currentUser(r); !ok {
		t.Fatal("guest session not recognised while on")
	}
	setSettings(`{"maxUploadBytes":67108864}`)
	if !app.guestLoginEnabled() {
		t.Error("a settings update that left guestLogin out switched it off")
	}
	setSettings(`{"guestLogin":false}`)
	if _, ok := app.currentUser(r); ok {
		t.Error("a guest session still works after guest sign-in was turned off")
	}
	if code := tokenCall(); code != http.StatusUnauthorized {
		t.Errorf("a guest token after guest sign-in was turned off returned %d", code)
	}
	if stats, _ := app.store.SessionStats(); stats[g.ID].Count != 0 {
		t.Errorf("turning guest sign-in off left %d guest sessions", stats[g.ID].Count)
	}

	// An administrator disabling the account refuses guest sign-in too.
	setSettings(`{"guestLogin":true}`)
	app.store.SetStatus(g.ID, StatusDisabled)
	if code := login().Code; code != http.StatusForbidden {
		t.Errorf("guest sign-in to a disabled guest account returned %d", code)
	}
}
