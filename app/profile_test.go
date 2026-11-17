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
	if err := app.store.SetUserProfile(u.ID, Profile{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", Avatar: "owl"}); err != nil {
		t.Fatal(err)
	}
	var first, last, email string
	app.store.db.QueryRow(`SELECT first_name, last_name, email FROM users WHERE id = ?`, u.ID).Scan(&first, &last, &email)
	if !strings.HasPrefix(first, "v1:") || !strings.HasPrefix(last, "v1:") || !strings.HasPrefix(email, "v1:") || strings.Contains(first+last+email, "Ada") {
		t.Errorf("stored in the clear: %q %q %q", first, last, email)
	}
	got, err := app.store.GetUser(u.ID)
	if err != nil || got.FirstName != "Ada" || got.LastName != "Lovelace" || got.Email != "ada@example.com" || got.Avatar != "owl" || got.displayName() != "Ada Lovelace" {
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
		{Profile{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", Avatar: "fox"}, true, true},
		{Profile{FirstName: " Ada ", LastName: "Lovelace", Email: " ada@example.com "}, true, true}, // no avatar: initials
		{Profile{FirstName: "Ada", LastName: "Lovelace"}, true, false},                              // a new account needs an email
		{Profile{FirstName: "Ada", LastName: "Lovelace", Email: "not an address"}, true, false},
		{Profile{FirstName: "Ada", LastName: "Lovelace", Email: "Ada <ada@example.com>"}, true, false},
		{Profile{FirstName: "Ada"}, true, false}, // a new account needs both names
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

func TestUpdateProfile(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("ada", "x", RoleUser, StatusApproved)
	w := httptest.NewRecorder()
	r := withPrincipal(httptest.NewRequest("PUT", "/api/me/profile", strings.NewReader(`{"firstName":"Ada","lastName":"King","email":"ada@example.com","avatar":"cat"}`)), principal{User: u})
	app.handleUpdateProfile(w, r)
	var got User
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.LastName != "King" || got.Avatar != "cat" || got.Email != "ada@example.com" {
		t.Errorf("update: %d %s", w.Code, w.Body)
	}
	// Someone else's address is refused.
	bob, _ := app.store.CreateUser("bob", "x", RoleUser, StatusApproved)
	w = httptest.NewRecorder()
	r = withPrincipal(httptest.NewRequest("PUT", "/api/me/profile", strings.NewReader(`{"firstName":"Bob","lastName":"B","email":"Ada@Example.com"}`)), principal{User: bob})
	app.handleUpdateProfile(w, r)
	if w.Code != http.StatusConflict {
		t.Errorf("bob took ada's email: %d %s", w.Code, w.Body)
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
