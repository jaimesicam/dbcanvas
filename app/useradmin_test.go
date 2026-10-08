package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbcanvas/internal/seal"
)

// useradmin_test.go — an admin changing another account's role or password.
//
// The ones that matter most: an admin cannot act on their own account (no instance
// left without an admin, no way round Settings' current-password check), and a reset
// link works exactly once.

// asAdmin builds a request to an /api/users/{id}/… route as the given admin.
func asAdmin(admin User, method string, target int64, body string) *http.Request {
	r := httptest.NewRequest(method, "/api/users/"+strconv.FormatInt(target, 10)+"/x", strings.NewReader(body))
	r.SetPathValue("id", strconv.FormatInt(target, 10))
	return withPrincipal(r, principal{User: admin})
}

func TestSetUserRole(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	u, _ := app.store.CreateUser("pat", "x", RoleUser, StatusApproved)

	w := httptest.NewRecorder()
	app.handleSetUserRole(w, asAdmin(admin, "POST", u.ID, `{"role":"admin"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("promote returned %d: %s", w.Code, w.Body.String())
	}
	if got, _ := app.store.GetUser(u.ID); got.Role != RoleAdmin {
		t.Fatalf("role is %q after promotion, want admin", got.Role)
	}

	w = httptest.NewRecorder()
	app.handleSetUserRole(w, asAdmin(admin, "POST", u.ID, `{"role":"user"}`))
	if got, _ := app.store.GetUser(u.ID); w.Code != http.StatusOK || got.Role != RoleUser {
		t.Fatalf("demote returned %d, role %q", w.Code, got.Role)
	}

	// An unknown role is refused, not stored.
	w = httptest.NewRecorder()
	app.handleSetUserRole(w, asAdmin(admin, "POST", u.ID, `{"role":"root"}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("an unknown role returned %d, want 400", w.Code)
	}
	// Demoting yourself could leave the instance with no admin.
	w = httptest.NewRecorder()
	app.handleSetUserRole(w, asAdmin(admin, "POST", admin.ID, `{"role":"user"}`))
	if got, _ := app.store.GetUser(admin.ID); w.Code != http.StatusBadRequest || got.Role != RoleAdmin {
		t.Errorf("self-demotion returned %d, role %q — want 400 and still admin", w.Code, got.Role)
	}
	w = httptest.NewRecorder()
	app.handleSetUserRole(w, asAdmin(admin, "POST", 9999, `{"role":"admin"}`))
	if w.Code != http.StatusNotFound {
		t.Errorf("an unknown user returned %d, want 404", w.Code)
	}
}

func TestAdminSetsAnotherPassword(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	u := withPassword(t, app, "pat", "oldpassword")
	signedIn(t, app, u, "")
	secret, _ := tokenFor(t, app, u.ID, ScopeRead, 30)

	w := httptest.NewRecorder()
	app.handleSetUserPassword(w, asAdmin(admin, "POST", u.ID, `{"newPassword":"brand-new-pw","revokeTokens":true}`))
	if w.Code != http.StatusOK {
		t.Fatalf("set password returned %d: %s", w.Code, w.Body.String())
	}
	_, hash, _ := app.store.CredByUsername("pat")
	if !checkPassword(hash, "brand-new-pw") {
		t.Fatal("the new password does not work")
	}
	var n int
	app.store.db.QueryRow("SELECT count(*) FROM sessions WHERE user_id = ?", u.ID).Scan(&n)
	if n != 0 {
		t.Errorf("%d sessions survived an admin password reset", n)
	}
	if tok, _, err := app.store.APITokenByHash(hashTokenSecret(secret)); err != nil || tok.state(time.Now()) != "revoked" {
		t.Errorf("revokeTokens left the account's token %q", tok.state(time.Now()))
	}

	for _, body := range []string{`{"newPassword":"short"}`, `{"newPassword":" spaced-out "}`} {
		w = httptest.NewRecorder()
		app.handleSetUserPassword(w, asAdmin(admin, "POST", u.ID, body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s returned %d, want 400", body, w.Code)
		}
	}
	// Your own password goes through Settings, which asks for the current one.
	w = httptest.NewRecorder()
	app.handleSetUserPassword(w, asAdmin(admin, "POST", admin.ID, `{"newPassword":"brand-new-pw"}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("an admin setting their own password returned %d, want 400", w.Code)
	}
}

func TestResetLink(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	u := withPassword(t, app, "pat", "oldpassword")
	signedIn(t, app, u, "")

	issue := func() string {
		t.Helper()
		w := httptest.NewRecorder()
		app.handleCreateResetLink(w, asAdmin(admin, "POST", u.ID, ""))
		if w.Code != http.StatusCreated {
			t.Fatalf("create link returned %d: %s", w.Code, w.Body.String())
		}
		var got struct {
			URL string `json:"url"`
		}
		json.Unmarshal(w.Body.Bytes(), &got)
		i := strings.Index(got.URL, "/reset-password/")
		if i < 0 {
			t.Fatalf("link %q has no /reset-password/ path", got.URL)
		}
		return got.URL[i+len("/reset-password/"):]
	}
	call := func(h http.HandlerFunc, method, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/auth/reset/"+token, strings.NewReader(body))
		r.SetPathValue("token", token)
		w := httptest.NewRecorder()
		h(w, r)
		return w
	}

	first := issue()
	token := issue()
	// A newer link replaces the older one.
	if w := call(app.handleResetLinkInfo, "GET", first, ""); w.Code != http.StatusNotFound {
		t.Errorf("a replaced link still resolves (%d)", w.Code)
	}
	if w := call(app.handleResetLinkInfo, "GET", token, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"pat"`) {
		t.Fatalf("link info returned %d: %s", w.Code, w.Body.String())
	}
	// A password that fails the rule does not burn the link.
	if w := call(app.handleUseResetLink, "POST", token, `{"newPassword":"short"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a short password returned %d, want 400", w.Code)
	}
	if w := call(app.handleUseResetLink, "POST", token, `{"newPassword":"chosen-by-pat"}`); w.Code != http.StatusOK {
		t.Fatalf("using the link returned %d: %s", w.Code, w.Body.String())
	}
	_, hash, _ := app.store.CredByUsername("pat")
	if !checkPassword(hash, "chosen-by-pat") {
		t.Fatal("the password chosen through the link does not work")
	}
	var n int
	app.store.db.QueryRow("SELECT count(*) FROM sessions WHERE user_id = ?", u.ID).Scan(&n)
	if n != 0 {
		t.Errorf("%d sessions survived a reset through a link", n)
	}
	// Once only.
	if w := call(app.handleUseResetLink, "POST", token, `{"newPassword":"second-attempt"}`); w.Code != http.StatusNotFound {
		t.Errorf("a spent link returned %d, want 404", w.Code)
	}

	// An expired link is dead.
	expired := issue()
	app.store.db.Exec("UPDATE password_resets SET expires_at = ?", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	if w := call(app.handleUseResetLink, "POST", expired, `{"newPassword":"too-late-now"}`); w.Code != http.StatusNotFound {
		t.Errorf("an expired link returned %d, want 404", w.Code)
	}
}

// routeFor finds a route by its pattern, for tests that go through requireScope.
func routeFor(t *testing.T, pattern string) apiRoute {
	t.Helper()
	for _, rt := range apiRoutes() {
		if rt.Pattern() == pattern {
			return rt
		}
	}
	t.Fatalf("no route %s", pattern)
	return apiRoute{}
}

func TestAdminCreatesAccount(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	create := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/users", strings.NewReader(body))
		w := httptest.NewRecorder()
		app.handleCreateUser(w, withPrincipal(r, principal{User: admin}))
		return w
	}

	// Without a password: an approved account and an invite link.
	w := create(`{"username":"newbie","role":"user","firstName":"New","lastName":"Bie","email":"n@x.io","avatar":"fox"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		User User   `json:"user"`
		URL  string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.User.Status != StatusApproved || got.User.Role != RoleUser || got.User.FirstName != "New" {
		t.Errorf("account came back as %+v", got.User)
	}
	token := got.URL[strings.Index(got.URL, "/reset-password/")+len("/reset-password/"):]
	l, err := app.store.PasswordReset(token)
	if err != nil || l.Purpose != linkInvite || time.Until(l.Expires) < 6*24*time.Hour {
		t.Errorf("the invite link is %+v (%v), want a 7-day invite", l, err)
	}

	// With a password: temporary, and no link.
	w = create(`{"username":"temp","role":"admin","firstName":"T","lastName":"P","email":"t@x.io","password":"temporary1"}`)
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusCreated || !got.User.MustChangePassword || got.User.Role != RoleAdmin {
		t.Errorf("temporary-password account: %d %+v", w.Code, got.User)
	}
	if strings.Contains(w.Body.String(), "reset-password") {
		t.Error("an account created with a password should not get a link")
	}

	for _, c := range []struct {
		body string
		code int
	}{
		{`{"username":"newbie","firstName":"A","lastName":"B","email":"other@x.io"}`, http.StatusConflict},
		{`{"username":"other","firstName":"A","lastName":"B","email":"n@x.io"}`, http.StatusConflict},
		{`{"username":"ab","firstName":"A","lastName":"B","email":"ab@x.io"}`, http.StatusBadRequest},
		{`{"username":"nameless","email":"nl@x.io"}`, http.StatusBadRequest},
		{`{"username":"weak","firstName":"A","lastName":"B","email":"w@x.io","password":"short"}`, http.StatusBadRequest},
		{`{"username":"rooty","role":"root","firstName":"A","lastName":"B","email":"r@x.io"}`, http.StatusBadRequest},
	} {
		if w := create(c.body); w.Code != c.code {
			t.Errorf("%s returned %d, want %d", c.body, w.Code, c.code)
		}
	}
}

func TestMustChangePasswordGate(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	u := withPassword(t, app, "pat", "oldpassword")

	w := httptest.NewRecorder()
	app.handleSetUserPassword(w, asAdmin(admin, "POST", u.ID, `{"newPassword":"temporary1","requireChange":true}`))
	if w.Code != http.StatusOK {
		t.Fatalf("set password returned %d", w.Code)
	}
	u, _ = app.store.GetUser(u.ID)
	if !u.MustChangePassword {
		t.Fatal("requireChange did not mark the password temporary")
	}

	reached := false
	call := func(pattern string, body string) int {
		reached = false
		r, _ := signedIn(t, app, u, body)
		w := httptest.NewRecorder()
		app.requireScope(routeFor(t, pattern), func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})(w, r)
		return w.Code
	}
	if code := call("GET /api/stacks", ""); code != http.StatusForbidden || reached {
		t.Errorf("listing stacks with a temporary password returned %d", code)
	}
	if code := call("GET /api/me", ""); code != http.StatusOK || !reached {
		t.Errorf("GET /api/me with a temporary password returned %d", code)
	}
	// A token is held to the same rule.
	secret, _ := tokenFor(t, app, u.ID, ScopeRead, 30)
	w = httptest.NewRecorder()
	app.requireScope(routeFor(t, "GET /api/stacks"), func(w http.ResponseWriter, r *http.Request) {})(w, bearerReq("GET", "/api/stacks", secret))
	if w.Code != http.StatusForbidden {
		t.Errorf("a token on a must-change account returned %d, want 403", w.Code)
	}

	// Changing it lifts the gate.
	r, _ := signedIn(t, app, u, `{"currentPassword":"temporary1","newPassword":"mine-at-last"}`)
	w = httptest.NewRecorder()
	app.handleChangePassword(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("change returned %d: %s", w.Code, w.Body.String())
	}
	if u, _ = app.store.GetUser(u.ID); u.MustChangePassword {
		t.Fatal("changing the password left the flag set")
	}
	if code := call("GET /api/stacks", ""); code != http.StatusOK {
		t.Errorf("after the change, listing stacks returned %d", code)
	}

	// Without requireChange the password is simply set.
	w = httptest.NewRecorder()
	app.handleSetUserPassword(w, asAdmin(admin, "POST", u.ID, `{"newPassword":"permanent1"}`))
	if u, _ = app.store.GetUser(u.ID); u.MustChangePassword {
		t.Error("a password set without requireChange was marked temporary")
	}
}

func TestUpdateUser(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	u, _ := app.store.CreateUser("pat", "x", RoleUser, StatusApproved)
	app.store.CreateUser("taken", "x", RoleUser, StatusApproved)
	app.store.SetUserProfile(admin.ID, Profile{FirstName: "B", LastName: "O", Email: "boss@x.io"})

	put := func(id int64, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		app.handleUpdateUser(w, asAdmin(admin, "PUT", id, body))
		return w
	}
	w := put(u.ID, `{"username":"patricia","firstName":"Pat","lastName":"Smith","email":"pat@x.io","avatar":"owl"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update returned %d: %s", w.Code, w.Body.String())
	}
	got, _ := app.store.GetUser(u.ID)
	if got.Username != "patricia" || got.LastName != "Smith" || got.Email != "pat@x.io" || got.Avatar != "owl" {
		t.Errorf("account is %+v after the update", got)
	}
	if w := put(u.ID, `{"username":"taken"}`); w.Code != http.StatusConflict {
		t.Errorf("renaming onto a taken username returned %d, want 409", w.Code)
	}
	if w := put(u.ID, `{"username":"patricia","email":"boss@x.io"}`); w.Code != http.StatusConflict {
		t.Errorf("taking another account's email returned %d, want 409", w.Code)
	}
	if w := put(u.ID, `{"username":"p"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a too-short username returned %d, want 400", w.Code)
	}
	// An admin may correct their own profile here too.
	if w := put(admin.ID, `{"username":"boss","firstName":"Big","lastName":"O","email":"boss@x.io"}`); w.Code != http.StatusOK {
		t.Errorf("editing yourself returned %d", w.Code)
	}
}

func TestUserSessions(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	u, _ := app.store.CreateUser("pat", "x", RoleUser, StatusApproved)
	app.store.CreateSessionFrom("s1", u.ID, time.Now().Add(time.Hour), "10.0.0.1", "Firefox")
	app.store.CreateSessionFrom("s2", u.ID, time.Now().Add(time.Hour), "10.0.0.2", "Chrome")
	app.store.CreateSessionFrom("old", u.ID, time.Now().Add(-time.Hour), "10.0.0.3", "Expired")

	w := httptest.NewRecorder()
	app.handleListUserSessions(w, asAdmin(admin, "GET", u.ID, ""))
	var list []UserSession
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 2 {
		t.Fatalf("listed %d sessions, want the 2 unexpired: %s", len(list), w.Body.String())
	}
	if strings.Contains(w.Body.String(), seal.HashSecret("s1")) {
		t.Fatal("the session listing leaked a token hash")
	}

	// The user listing carries the count.
	w = httptest.NewRecorder()
	app.handleListUsers(w, withPrincipal(httptest.NewRequest("GET", "/api/users", nil), principal{User: admin}))
	if !strings.Contains(w.Body.String(), `"sessions":2`) {
		t.Errorf("the user listing does not count pat's 2 sessions: %s", w.Body.String())
	}

	// End one.
	r := asAdmin(admin, "DELETE", u.ID, "")
	r.SetPathValue("sid", strconv.FormatInt(list[0].ID, 10))
	w = httptest.NewRecorder()
	app.handleEndUserSession(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("ending one session returned %d", w.Code)
	}
	if _, err := app.store.SessionUser(map[string]string{"10.0.0.1": "s1", "10.0.0.2": "s2"}[list[0].IP]); err == nil {
		t.Error("the ended session still signs in")
	}
	// Another account's session id is not reachable through this one.
	other, _ := app.store.CreateUser("other", "x", RoleUser, StatusApproved)
	r = asAdmin(admin, "DELETE", other.ID, "")
	r.SetPathValue("sid", strconv.FormatInt(list[1].ID, 10))
	w = httptest.NewRecorder()
	app.handleEndUserSession(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("ending pat's session through another account returned %d, want 404", w.Code)
	}

	// End all.
	w = httptest.NewRecorder()
	app.handleEndUserSessions(w, asAdmin(admin, "DELETE", u.ID, ""))
	if left, _ := app.store.ListUserSessions(u.ID); w.Code != http.StatusOK || len(left) != 0 {
		t.Errorf("sign out everywhere returned %d and left %d sessions", w.Code, len(left))
	}
}

// An admin can clear out their own old sessions, but never the one they are using.
func TestEndOwnSessionsKeepsCurrent(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	for i := 0; i < 5; i++ {
		app.store.CreateSession("old-"+strconv.Itoa(i), admin.ID, time.Now().Add(time.Hour))
	}
	_, mine := signedIn(t, app, admin, "")
	// asMe is a request from the admin's current browser about their own account.
	asMe := func(method string) *http.Request {
		r := httptest.NewRequest(method, "/api/users/x/sessions", nil)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: mine})
		r.SetPathValue("id", strconv.FormatInt(admin.ID, 10))
		return withPrincipal(r, principal{User: admin})
	}

	w := httptest.NewRecorder()
	app.handleListUserSessions(w, asMe("GET"))
	var list []UserSession
	json.Unmarshal(w.Body.Bytes(), &list)
	var current []UserSession
	for _, s := range list {
		if s.Current {
			current = append(current, s)
		}
	}
	if len(list) != 6 || len(current) != 1 {
		t.Fatalf("listed %d sessions with %d current, want 6 with 1", len(list), len(current))
	}

	// The current one cannot be ended from here.
	r := asMe("DELETE")
	r.SetPathValue("sid", strconv.FormatInt(current[0].ID, 10))
	w = httptest.NewRecorder()
	app.handleEndUserSession(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("ending your current session returned %d, want 400", w.Code)
	}

	// Signing out the rest keeps it.
	w = httptest.NewRecorder()
	app.handleEndUserSessions(w, asMe("DELETE"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"signedOut":5`) {
		t.Fatalf("signing out your other sessions returned %d: %s", w.Code, w.Body.String())
	}
	if left, _ := app.store.ListUserSessions(admin.ID); len(left) != 1 || left[0].ID != current[0].ID {
		t.Fatalf("left %+v, want only the current session", left)
	}
}

func TestSignInRecordsLastLogin(t *testing.T) {
	app := newTestApp(t)
	withPassword(t, app, "pat", "patpassword")
	w := httptest.NewRecorder()
	app.handleLogin(w, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"pat","password":"patpassword"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("login returned %d", w.Code)
	}
	u, _, _ := app.store.CredByUsername("pat")
	if u.LastLoginAt == nil {
		t.Error("signing in did not record last_login_at")
	}
	list, _ := app.store.ListUserSessions(u.ID)
	if len(list) != 1 || list[0].IP == "" {
		t.Errorf("the session did not record where it came from: %+v", list)
	}
}

func TestClearSignInHistory(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	pat, _ := app.store.CreateUser("pat", "x", RoleUser, StatusApproved)
	kim, _ := app.store.CreateUser("kim", "x", RoleUser, StatusApproved)
	for _, u := range []User{pat, kim} {
		app.store.TouchLastLogin(u.ID)
		app.store.CreateSessionFrom("sess-"+u.Username, u.ID, time.Now().Add(time.Hour), "10.0.0.9", "Firefox")
	}
	recorded := func(u User) bool {
		got, _ := app.store.GetUser(u.ID)
		list, _ := app.store.ListUserSessions(u.ID)
		return got.LastLoginAt != nil || list[0].IP != "" || list[0].UserAgent != "" || list[0].CreatedAt != ""
	}

	w := httptest.NewRecorder()
	app.handleClearSignInHistory(w, asAdmin(admin, "DELETE", pat.ID, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("clear returned %d", w.Code)
	}
	if recorded(pat) {
		t.Error("pat's sign-in history survived clearing it")
	}
	if !recorded(kim) {
		t.Error("clearing pat's history cleared kim's too")
	}
	// Nobody is signed out by it.
	if _, err := app.store.SessionUser("sess-pat"); err != nil {
		t.Errorf("clearing the history signed pat out: %v", err)
	}

	w = httptest.NewRecorder()
	app.handleClearAllSignInHistory(w, withPrincipal(httptest.NewRequest("DELETE", "/api/users/sign-ins", nil), principal{User: admin}))
	if w.Code != http.StatusOK || recorded(kim) {
		t.Errorf("clear all returned %d and left kim's history", w.Code)
	}
}

// A cleared history must not turn a reset into an invite: whether a link is an
// invite is the invite_pending flag, not the absence of a sign-in.
func TestInviteIsNotInferredFromHistory(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	u := withPassword(t, app, "pat", "patpassword")
	app.store.ClearSignInHistory(0)

	w := httptest.NewRecorder()
	app.handleCreateResetLink(w, asAdmin(admin, "POST", u.ID, ""))
	if !strings.Contains(w.Body.String(), `"purpose":"reset"`) {
		t.Errorf("a reset for an account with no recorded sign-in became %s", w.Body.String())
	}

	// An invited account stays invited until its link is used.
	r := httptest.NewRequest("POST", "/api/users", strings.NewReader(`{"username":"newbie","firstName":"N","lastName":"B","email":"n@x.io"}`))
	w = httptest.NewRecorder()
	app.handleCreateUser(w, withPrincipal(r, principal{User: admin}))
	var got struct {
		User User `json:"user"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if !got.User.InvitePending {
		t.Fatal("an account created with a link is not marked invite-pending")
	}
	w = httptest.NewRecorder()
	app.handleCreateResetLink(w, asAdmin(admin, "POST", got.User.ID, ""))
	if !strings.Contains(w.Body.String(), `"purpose":"invite"`) {
		t.Errorf("a new link for an unused invite became %s", w.Body.String())
	}
	var link struct {
		URL string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &link)
	token := link.URL[strings.Index(link.URL, "/reset-password/")+len("/reset-password/"):]
	r = httptest.NewRequest("POST", "/api/auth/reset/"+token, strings.NewReader(`{"newPassword":"chosen-pw-1"}`))
	r.SetPathValue("token", token)
	app.handleUseResetLink(httptest.NewRecorder(), r)
	if after, _ := app.store.GetUser(got.User.ID); after.InvitePending {
		t.Error("using the invite left the account invite-pending")
	}
}

func TestSignEveryoneOut(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	pat, _ := app.store.CreateUser("pat", "x", RoleUser, StatusApproved)
	app.store.CreateSession("pat-1", pat.ID, time.Now().Add(time.Hour))
	app.store.CreateSession("pat-2", pat.ID, time.Now().Add(time.Hour))
	app.store.CreateSession("boss-other", admin.ID, time.Now().Add(time.Hour))
	secret, _ := tokenFor(t, app, pat.ID, ScopeRead, 30)

	r, mine := signedIn(t, app, admin, "")
	w := httptest.NewRecorder()
	app.handleEndAllSessions(w, withPrincipal(r, principal{User: admin}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"signedOut":3`) {
		t.Fatalf("sign everyone out returned %d: %s", w.Code, w.Body.String())
	}
	for _, tok := range []string{"pat-1", "pat-2", "boss-other"} {
		if _, err := app.store.SessionUser(tok); err == nil {
			t.Errorf("session %s survived", tok)
		}
	}
	// The admin who asked stays signed in, and API tokens are not sessions.
	if _, err := app.store.SessionUser(mine); err != nil {
		t.Error("signing everyone out signed out the admin who asked")
	}
	if tok, _, err := app.store.APITokenByHash(hashTokenSecret(secret)); err != nil || tok.state(time.Now()) != "active" {
		t.Error("signing everyone out revoked an API token")
	}
}
