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

	// Off by default.
	if app.systemSettings("").GuestLogin {
		t.Fatal("guest sign-in is on by default")
	}
	if code := login().Code; code != http.StatusForbidden {
		t.Fatalf("guest sign-in while off returned %d", code)
	}
	if gs, _ := app.store.ListGuestAccounts(); len(gs) != 0 {
		t.Fatal("a refused guest sign-in created a guest account")
	}

	// On: a regular user, signed in.
	setSettings(`{"guestLogin":true}`)
	w := login()
	if w.Code != http.StatusOK || len(w.Result().Cookies()) == 0 {
		t.Fatalf("guest sign-in: %d, cookies %d", w.Code, len(w.Result().Cookies()))
	}
	gs, _ := app.store.ListGuestAccounts()
	if len(gs) != 1 {
		t.Fatalf("guest accounts: %d", len(gs))
	}
	g := gs[0]
	if !g.GuestAccount || g.Role != RoleUser || g.Status != StatusApproved || !strings.HasPrefix(g.Username, "guest-") {
		t.Fatalf("guest account: %+v", g)
	}
	// A second guest gets an account of its own…
	login()
	if gs, _ := app.store.ListGuestAccounts(); len(gs) != 2 || gs[1].ID == g.ID {
		t.Errorf("a second guest sign-in: %d guest accounts", len(gs))
	}
	// …while a browser that already has a guest session keeps its account.
	{
		r, _ := signedIn(t, app, g, "")
		w := httptest.NewRecorder()
		app.handleGuestLogin(w, r)
		if gs, _ := app.store.ListGuestAccounts(); w.Code != http.StatusOK || len(gs) != 2 {
			t.Errorf("signing in again from a guest session: %d, %d guest accounts", w.Code, len(gs))
		}
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
	if code := call("POST /api/me/password"); code != http.StatusForbidden || reached {
		t.Errorf("setting a password as a guest returned %d", code)
	}
	// The profile is the guest's own now, so the guest rule lets it change it (whether the
	// Guests group has the page that does is the group's setting, secgroups.go).
	if !app.guestGate(httptest.NewRecorder(), g, routeFor(t, "PUT /api/me/profile")) {
		t.Error("the guest rule refused a guest's own profile")
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

	// Keeping existing guests: turning sign-in off then refuses only new ones.
	setSettings(`{"guestLogin":true}`)
	guestSecret, _ = tokenFor(t, app, g.ID, ScopeWrite, 30)
	setSettings(`{"guestKeepAccess":true}`)
	setSettings(`{"guestLogin":false}`)
	if code := tokenCall(); code != http.StatusOK {
		t.Errorf("a kept guest's token after sign-in was turned off returned %d", code)
	}
	if code := login().Code; code != http.StatusForbidden {
		t.Errorf("a new guest while sign-in is off returned %d", code)
	}
}

func TestReapIdleGuests(t *testing.T) {
	app := newTestApp(t)
	fresh, _ := app.store.CreateGuestAccount()
	idle, _ := app.store.CreateGuestAccount()
	regular, _ := app.store.CreateUser("ann", "x", RoleUser, StatusApproved)
	old := "2020-01-01T00:00:00Z"
	for _, id := range []int64{idle.ID, regular.ID} {
		app.store.db.Exec("UPDATE users SET created_at = ?, last_login_at = ? WHERE id = ?", old, old, id)
	}
	app.store.SetAppSetting(settingGuestIdleDays, "7")
	app.reapIdleGuests()
	if _, err := app.store.GetUser(idle.ID); err == nil {
		t.Error("an idle guest account was kept")
	}
	if _, err := app.store.GetUser(fresh.ID); err != nil {
		t.Error("a fresh guest account was removed")
	}
	if _, err := app.store.GetUser(regular.ID); err != nil {
		t.Error("an idle regular account was removed")
	}
	// 0 keeps them.
	app.store.db.Exec("UPDATE users SET created_at = ?, last_login_at = ? WHERE id = ?", old, old, fresh.ID)
	app.store.SetAppSetting(settingGuestIdleDays, "0")
	app.reapIdleGuests()
	if _, err := app.store.GetUser(fresh.ID); err != nil {
		t.Error("guestIdleDays=0 removed a guest account")
	}
}
