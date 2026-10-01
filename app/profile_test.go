package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The browser draws the avatars and the server accepts their ids: the two lists are
// one list, written twice.
func TestAvatarListsAgree(t *testing.T) {
	src, err := os.ReadFile("web/src/components/Avatar.jsx")
	if err != nil {
		t.Fatal(err)
	}
	var js []string
	for _, m := range regexp.MustCompile(`\{ id: '([a-z]+)'`).FindAllStringSubmatch(string(src), -1) {
		js = append(js, m[1])
	}
	if strings.Join(js, ",") != strings.Join(avatarIDs, ",") {
		t.Errorf("Avatar.jsx has %v\nprofile.go has %v", js, avatarIDs)
	}
	if len(avatarIDs) < 20 {
		t.Errorf("only %d avatars to choose from", len(avatarIDs))
	}
}

func TestNamesAreSealedAtRest(t *testing.T) {
	app := newTestApp(t)
	u, err := app.store.CreateUser("ada", "x", RoleUser, StatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.SetUserProfile(u.ID, Profile{FirstName: "Ada", LastName: "Lovelace", Avatar: "owl"}); err != nil {
		t.Fatal(err)
	}
	var first, last string
	app.store.db.QueryRow(`SELECT first_name, last_name FROM users WHERE id = ?`, u.ID).Scan(&first, &last)
	if !strings.HasPrefix(first, "v1:") || !strings.HasPrefix(last, "v1:") || strings.Contains(first+last, "Ada") {
		t.Errorf("names stored in the clear: %q %q", first, last)
	}
	got, err := app.store.GetUser(u.ID)
	if err != nil || got.FirstName != "Ada" || got.LastName != "Lovelace" || got.Avatar != "owl" || got.displayName() != "Ada Lovelace" {
		t.Errorf("read back %+v, %v", got, err)
	}
	if cu, _, err := app.store.CredByUsername("ada"); err != nil || cu.FirstName != "Ada" {
		t.Errorf("sign-in lookup lost the profile: %+v %v", cu, err)
	}
}

func TestProfileValidation(t *testing.T) {
	for _, c := range []struct {
		p    Profile
		req  bool
		okay bool
	}{
		{Profile{FirstName: "Ada", LastName: "Lovelace", Avatar: "fox"}, true, true},
		{Profile{FirstName: " Ada ", LastName: "Lovelace"}, true, true}, // no avatar: initials
		{Profile{FirstName: "Ada"}, true, false},                         // a new account needs both names
		{Profile{}, false, true},
		{Profile{FirstName: "Ada", LastName: "L", Avatar: "../etc"}, true, false},
		{Profile{FirstName: strings.Repeat("a", 61), LastName: "L"}, true, false},
		{Profile{FirstName: "Ada\x00", LastName: "L"}, true, false},
	} {
		p := c.p
		if err := p.clean(c.req); (err == nil) != c.okay {
			t.Errorf("%+v required=%v: %v", c.p, c.req, err)
		}
	}
}

func TestRegisterTakesAProfile(t *testing.T) {
	app := newTestApp(t)
	app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	post := func(body string) int {
		w := httptest.NewRecorder()
		app.handleRegister(w, httptest.NewRequest("POST", "/api/auth/register", strings.NewReader(body)))
		return w.Code
	}
	if code := post(`{"username":"grace","password":"password123"}`); code != http.StatusBadRequest {
		t.Errorf("an account without a name was created: %d", code)
	}
	if code := post(`{"username":"grace","password":"password123","firstName":"Grace","lastName":"Hopper","avatar":"rocket"}`); code != http.StatusCreated {
		t.Fatalf("register: %d", code)
	}
	u, _, _ := app.store.CredByUsername("grace")
	if u.displayName() != "Grace Hopper" || u.Avatar != "rocket" {
		t.Errorf("profile not saved: %+v", u)
	}
}

func TestUpdateProfile(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("ada", "x", RoleUser, StatusApproved)
	w := httptest.NewRecorder()
	r := withPrincipal(httptest.NewRequest("PUT", "/api/me/profile", strings.NewReader(`{"firstName":"Ada","lastName":"King","avatar":"cat"}`)), principal{User: u})
	app.handleUpdateProfile(w, r)
	var got User
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.LastName != "King" || got.Avatar != "cat" {
		t.Errorf("update: %d %s", w.Code, w.Body)
	}
}

// joinAccount posts to the account route as a browser that may carry a login cookie.
func (f *shareFixture) joinAccount(t *testing.T, body string, login *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/join/"+f.token+"/account", strings.NewReader(body))
	r.SetPathValue("token", f.token)
	r.RemoteAddr = "10.0.1.7:5555"
	if login != nil {
		r.AddCookie(login)
	}
	f.app.handleJoinAccount(w, r)
	return w
}

func TestJoinWithAnAccount(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	hash, _ := hashPassword("password123")
	grace, err := f.app.store.CreateUser("grace", hash, RoleUser, StatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	f.app.store.SetUserProfile(grace.ID, Profile{FirstName: "Grace", LastName: "Hopper", Avatar: "rocket"})

	if w := f.joinAccount(t, `{"username":"grace","password":"wrong-password"}`, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a wrong password joined: %d", w.Code)
	}
	w := f.joinAccount(t, `{"username":"grace","password":"password123"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	var g ShareGuest
	json.Unmarshal(w.Body.Bytes(), &g)
	if g.Name != "Grace Hopper" || g.UserID != grace.ID || g.Account != "grace" || g.Avatar != "rocket" || g.State != guestWaiting {
		t.Errorf("guest: %+v", g)
	}
	// It is a lobby place, nothing more: no login session is handed out.
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			t.Error("joining signed the browser in")
		}
	}
	if !strings.Contains(w.Header().Get("Set-Cookie"), guestCookieName) {
		t.Error("no guest cookie")
	}
}

func TestJoinAsTheSignedInAccount(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	grace, _ := f.app.store.CreateUser("grace", "x", RoleUser, StatusApproved)
	f.app.store.SetUserProfile(grace.ID, Profile{FirstName: "Grace", LastName: "Hopper", Avatar: "owl"})
	f.app.store.CreateSession("grace-token", grace.ID, time.Now().Add(time.Hour))
	login := &http.Cookie{Name: cookieName, Value: "grace-token"}

	if w := f.joinAccount(t, `{"useSession":true}`, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("useSession without a login joined: %d", w.Code)
	}
	w := f.joinAccount(t, `{"useSession":true}`, login)
	var g ShareGuest
	json.Unmarshal(w.Body.Bytes(), &g)
	if w.Code != http.StatusOK || g.UserID != grace.ID || g.Avatar != "owl" {
		t.Errorf("join: %d %s", w.Code, w.Body)
	}

	// The join page offers it, and says who.
	iw := httptest.NewRecorder()
	ir := httptest.NewRequest("GET", "/api/join/"+f.token, nil)
	ir.SetPathValue("token", f.token)
	ir.AddCookie(login)
	f.app.handleJoinInfo(iw, ir)
	if !strings.Contains(iw.Body.String(), `"name":"Grace Hopper"`) {
		t.Errorf("join info does not name the account: %s", iw.Body)
	}
}

func TestTheHostCannotJoinTheirOwnSession(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	f.app.store.CreateSession("host-token", f.host.ID, time.Now().Add(time.Hour))
	w := f.joinAccount(t, `{"useSession":true}`, &http.Cookie{Name: cookieName, Value: "host-token"})
	if w.Code != http.StatusConflict {
		t.Errorf("the host joined their own session: %d %s", w.Code, w.Body)
	}
}

func TestAGuestPicksAnAvatar(t *testing.T) {
	f := newShareFixture(t, RoleUser)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/join/"+f.token, strings.NewReader(`{"name":"Jane","email":"jane@example.com","avatar":"fox"}`))
	r.SetPathValue("token", f.token)
	r.RemoteAddr = "10.0.2." + strconv.Itoa(9) + ":5555"
	f.app.handleJoin(w, r)
	var g ShareGuest
	json.Unmarshal(w.Body.Bytes(), &g)
	if w.Code != http.StatusOK || g.Avatar != "fox" || g.UserID != 0 {
		t.Errorf("join: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/join/"+f.token, strings.NewReader(`{"name":"Joe","email":"joe@example.com","avatar":"<script>"}`))
	r.SetPathValue("token", f.token)
	r.RemoteAddr = "10.0.2.10:5555"
	f.app.handleJoin(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("an unknown avatar was accepted: %d", w.Code)
	}
}
