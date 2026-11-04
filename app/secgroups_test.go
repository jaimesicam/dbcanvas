package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestFeaturesMatchNav: the feature ids are the web UI's page ids, so a page added to
// the sidebar without a feature here (or the other way round) fails the build.
func TestFeaturesMatchNav(t *testing.T) {
	src, err := os.ReadFile("web/src/App.jsx")
	if err != nil {
		t.Fatal(err)
	}
	nav := map[string]bool{}
	for _, m := range regexp.MustCompile(`\{ id: '([a-z0-9-]+)', label:`).FindAllStringSubmatch(string(src), -1) {
		nav[m[1]] = true
	}
	for _, f := range features {
		if !nav[f.ID] {
			t.Errorf("feature %s is not a page in App.jsx", f.ID)
		}
		delete(nav, f.ID)
	}
	for id := range nav {
		t.Errorf("page %s in App.jsx has no feature in secgroups.go", id)
	}
}

// TestEveryRouteHasAFeature: a route group that falls through to Database Stacks by
// default should do so on purpose; this lists what does, so a new group is a choice.
func TestRouteFeatures(t *testing.T) {
	want := map[string]string{
		"GET /api/stacks":             featStacks,
		"GET /api/dashboard/summary":  featDashboard,
		"GET /api/users":              featManageUsers,
		"POST /api/tokens":            featAPI,
		"GET /api/me":                 "",
		"GET /api/me/settings":        "",
		"PUT /api/me/profile":         featProfile,
		"GET /api/me/scratchboards":   featScratch,
		"PUT /api/system/settings":    featSettings,
		"GET /api/system/settings":    "",
		"GET /api/k3d/states/targets": featK8sStates,
		"POST /api/auth/login":        "",
		"GET /api/security/groups":    featManageUsers,
	}
	for p, f := range want {
		if got := routeFeature(routeFor(t, p)); got != f {
			t.Errorf("%s belongs to %q, want %q", p, got, f)
		}
	}
	for _, rt := range apiRoutes() {
		if f := routeFeature(rt); f != "" {
			if _, ok := featureByID(f); !ok {
				t.Errorf("%s maps to unknown feature %q", rt.Pattern(), f)
			}
		}
	}
}

func TestSecurityGroups(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	bob, _ := app.store.CreateUser("bob", "x", RoleUser, StatusApproved)
	app.store.SetAppSetting(settingGuestLogin, "1")
	guest, err := app.store.CreateGuestAccount()
	if err != nil {
		t.Fatal(err)
	}

	// Defaults.
	if got := strings.Join(app.allowedFeatures(guest), ","); got != "dashboard,stack-designer" {
		t.Errorf("guest defaults: %s", got)
	}
	if app.featureAllowed(bob, featManageUsers) || !app.featureAllowed(bob, featKanban) || !app.featureAllowed(bob, featAPI) {
		t.Error("regular user defaults wrong")
	}
	if len(app.allowedFeatures(admin)) != len(features) {
		t.Error("an administrator does not have every feature by default")
	}

	call := func(u User, pattern string, token bool) int {
		reached := false
		var r *http.Request
		if token {
			secret, _ := tokenFor(t, app, u.ID, ScopeWrite, 30)
			rt := routeFor(t, pattern)
			r = bearerReq(rt.Method, rt.Path, secret)
		} else {
			r, _ = signedIn(t, app, u, "")
		}
		w := httptest.NewRecorder()
		app.requireScope(routeFor(t, pattern), func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})(w, r)
		if (w.Code == http.StatusOK) != reached {
			t.Fatalf("%s: %d but reached=%v", pattern, w.Code, reached)
		}
		return w.Code
	}
	// A guest, in the browser: stacks yes, Kanban and tokens no; the core always.
	for p, want := range map[string]int{"GET /api/stacks": 200, "GET /api/kanban/boards": 403, "POST /api/tokens": 403, "GET /api/me": 200} {
		if code := call(guest, p, false); code != want {
			t.Errorf("guest %s: %d, want %d", p, code, want)
		}
	}
	// A regular user: everything but the users page.
	if code := call(bob, "GET /api/kanban/boards", false); code != 200 {
		t.Errorf("bob kanban: %d", code)
	}
	if code := call(bob, "GET /api/stacks", true); code != 200 {
		t.Errorf("bob's token on stacks: %d", code)
	}

	put := func(body string) securityGroupsView {
		w := httptest.NewRecorder()
		app.handleSetSecurityGroups(w, withPrincipal(httptest.NewRequest("PUT", "/api/security/groups", strings.NewReader(body)), principal{User: admin}))
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", body, w.Code, w.Body)
		}
		return app.securityGroupsView()
	}
	// Take Kanban from regular users: the browser and the token are both refused.
	put(`{"allowed":{"user":{"kanban":false}}}`)
	if call(bob, "GET /api/kanban/boards", false) != 403 || call(bob, "GET /api/kanban/boards", true) != 403 {
		t.Error("kanban still reachable after it was taken away")
	}
	// Take the API from regular users: every token request is refused, even for a
	// feature the group has.
	put(`{"allowed":{"user":{"api":false}}}`)
	if code := call(bob, "GET /api/stacks", true); code != 403 {
		t.Errorf("a token without the API feature: %d", code)
	}
	if code := call(bob, "GET /api/stacks", false); code != 200 {
		t.Errorf("the browser without the API feature: %d", code)
	}
	// Granting a tool grants Database Stacks; taking Stacks takes the tool.
	v := put(`{"allowed":{"guest":{"stack-designer":false,"log-summary":false}}}`)
	if v.Allowed[GroupGuest][featStacks] {
		t.Error("stacks not taken from guests")
	}
	v = put(`{"allowed":{"guest":{"log-summary":true}}}`)
	if !v.Allowed[GroupGuest][featStacks] || !v.Allowed[GroupGuest][featLogSummary] {
		t.Error("granting log summary did not grant stacks")
	}
	v = put(`{"allowed":{"guest":{"stack-designer":false,"log-summary":true}}}`)
	if v.Allowed[GroupGuest][featStacks] || v.Allowed[GroupGuest][featLogSummary] {
		t.Error("a tool kept without stacks")
	}
	// The locks hold whatever is sent.
	v = put(`{"allowed":{"admin":{"users":false,"settings":false,"kanban":false},"user":{"users":true}}}`)
	if !v.Allowed[GroupAdmin][featManageUsers] || !v.Allowed[GroupAdmin][featSettings] || v.Allowed[GroupUser][featManageUsers] {
		t.Error("a locked cell moved")
	}
	if v.Allowed[GroupAdmin][featKanban] {
		t.Error("an unlocked admin cell did not move")
	}
	// Unknown names are refused.
	w := httptest.NewRecorder()
	app.handleSetSecurityGroups(w, httptest.NewRequest("PUT", "/", strings.NewReader(`{"allowed":{"user":{"nope":true}}}`)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown feature: %d", w.Code)
	}
}
