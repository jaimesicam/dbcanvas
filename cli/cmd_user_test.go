package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// adminProfile stores a signed-in administrator, the state `user create` runs from.
func adminProfile(t *testing.T, url string) {
	t.Helper()
	cfg, _ := loadConfig()
	cfg.Profiles["default"] = Profile{URL: url, Token: "dbc_admintoken", User: "admin", Scope: "admin"}
	cfg.Current = "default"
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// userServer answers the password handshake: login as the given role, then whatever
// extra routes the test cares about.
func userServer(t *testing.T, login map[string]any, extra func(s *testServer, w http.ResponseWriter, r *http.Request) bool) *testServer {
	return newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "dbcanvas_session", Value: "sess", Path: "/"})
			json.NewEncoder(w).Encode(login)
		case "POST /api/auth/logout":
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		default:
			if extra == nil || !extra(s, w, r) {
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}
	})
}

// POST /api/users refuses bearer tokens (NoToken, app/useradmin.go), so the account has
// to be created on a password session — and that session must be signed out after.
func TestUserCreateUsesAnAdminPasswordSession(t *testing.T) {
	isolateConfig(t)
	ts := userServer(t, map[string]any{"username": "admin", "role": "admin"},
		func(s *testServer, w http.ResponseWriter, r *http.Request) bool {
			if r.Method+" "+r.URL.Path != "POST /api/users" {
				return false
			}
			if r.Header.Get("Authorization") != "" {
				t.Errorf("account creation sent an Authorization header: %q", r.Header.Get("Authorization"))
			}
			if _, err := r.Cookie("dbcanvas_session"); err != nil {
				t.Error("account creation did not carry the session cookie")
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"user": map[string]any{"id": 2, "username": "bob", "role": "user", "mustChangePassword": true}})
			return true
		})
	adminProfile(t, ts.URL)

	withStdin(t, "adminpw\nTempPass99\nTempPass99\n")
	if err := cmdUser([]string{"create", "bob", "--first", "Bob", "--last", "Lee", "--email", "bob@example.net"}); err != nil {
		t.Fatalf("user create: %v", err)
	}
	want := []string{"POST /api/auth/login", "POST /api/users", "POST /api/auth/logout"}
	if strings.Join(ts.requests, ",") != strings.Join(want, ",") {
		t.Fatalf("requests were %v, want %v", ts.requests, want)
	}
	var login, body map[string]any
	json.Unmarshal([]byte(ts.bodies[0]), &login)
	json.Unmarshal([]byte(ts.bodies[1]), &body)
	if login["username"] != "admin" || login["password"] != "adminpw" {
		t.Errorf("signed in as %v — want the profile's administrator with the password typed", login)
	}
	for k, v := range map[string]string{"username": "bob", "role": "user", "password": "TempPass99",
		"firstName": "Bob", "lastName": "Lee", "email": "bob@example.net"} {
		if body[k] != v {
			t.Errorf("%s = %v, want %q", k, body[k], v)
		}
	}
}

// --invite sends no password, which is what makes the server return a link instead.
func TestUserCreateInviteSendsNoPassword(t *testing.T) {
	isolateConfig(t)
	ts := userServer(t, map[string]any{"username": "admin", "role": "admin"},
		func(s *testServer, w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/api/users" {
				return false
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"user": map[string]any{"id": 3, "username": "carol", "role": "user"},
				"url":  "http://x/reset-password/abc", "expiresAt": "2026-10-18T00:00:00Z"})
			return true
		})
	adminProfile(t, ts.URL)
	withStdin(t, "adminpw\n") // only the administrator's password is asked for
	if err := cmdUser([]string{"create", "carol", "--first", "C", "--last", "D", "--email", "c@example.net", "--invite"}); err != nil {
		t.Fatalf("user create --invite: %v", err)
	}
	var body map[string]any
	json.Unmarshal([]byte(ts.bodies[1]), &body)
	if body["password"] != "" {
		t.Errorf("--invite sent a password: %v", body["password"])
	}
}

// Every account needs a name and an email; that is refused before any password is
// asked for or any request is made.
func TestUserCreateRefusalsAreLocal(t *testing.T) {
	isolateConfig(t)
	ts := userServer(t, nil, nil)
	adminProfile(t, ts.URL)
	cases := [][]string{
		{"create", "bob", "--first", "Bob"},
		{"create", "--first", "Bob", "--last", "Lee", "--email", "b@example.net"},
		{"create", "bob", "--first", "Bob", "--last", "Lee", "--email", "b@example.net", "--role", "root"},
	}
	for _, args := range cases {
		withStdin(t, "")
		if err := cmdUser(args); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	withStdin(t, "adminpw\nshort\nshort\n")
	if err := cmdUser([]string{"create", "bob", "--first", "B", "--last", "L", "--email", "b@example.net"}); err == nil ||
		!strings.Contains(err.Error(), "8 characters") {
		t.Errorf("a short temporary password: %v", err)
	}
	if len(ts.requests) != 0 {
		t.Errorf("refusals reached the server: %v", ts.requests)
	}
}

// Signed in as somebody who is not an administrator, it says so — and still signs out.
func TestUserCreateRefusesANonAdmin(t *testing.T) {
	isolateConfig(t)
	ts := userServer(t, map[string]any{"username": "bob", "role": "user"}, nil)
	adminProfile(t, ts.URL)
	withStdin(t, "bobpw\nTempPass99\nTempPass99\n")
	err := cmdUser([]string{"create", "eve", "--first", "E", "--last", "V", "--email", "e@example.net", "--admin", "bob"})
	if err == nil || !strings.Contains(err.Error(), "not an administrator") {
		t.Fatalf("got %v, want a not-an-administrator refusal", err)
	}
	if got := ts.requests[len(ts.requests)-1]; got != "POST /api/auth/logout" {
		t.Errorf("the session was not signed out (last request %q)", got)
	}
}

// An account an administrator gave a temporary password can do nothing until its owner
// picks their own — minting a token included — so login does it on the same session.
func TestLoginChoosesAPasswordWhenTheAdminSetOne(t *testing.T) {
	isolateConfig(t)
	ts := userServer(t, map[string]any{"id": 2, "username": "bob", "role": "user", "mustChangePassword": true},
		func(s *testServer, w http.ResponseWriter, r *http.Request) bool {
			switch r.Method + " " + r.URL.Path {
			case "POST /api/me/password":
				json.NewEncoder(w).Encode(map[string]any{"tokensRevoked": 0})
			case "POST /api/tokens":
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]any{
					"token":  map[string]any{"name": "cli", "prefix": "dbc_x", "scope": "write"},
					"secret": "dbc_xsecret"})
			default:
				return false
			}
			return true
		})
	withStdin(t, "TempPass99\nBobsOwn123\nBobsOwn123\n")
	if err := cmdLogin([]string{"--url", ts.URL, "--user", "bob"}); err != nil {
		t.Fatalf("login: %v", err)
	}
	want := []string{"POST /api/auth/login", "POST /api/me/password", "POST /api/tokens", "POST /api/auth/logout"}
	if strings.Join(ts.requests, ",") != strings.Join(want, ",") {
		t.Fatalf("requests were %v, want %v", ts.requests, want)
	}
	var change map[string]any
	json.Unmarshal([]byte(ts.bodies[1]), &change)
	if change["currentPassword"] != "TempPass99" || change["newPassword"] != "BobsOwn123" {
		t.Errorf("password change sent %v", change)
	}
}

func TestLoginRefusesKeepingTheTemporaryPassword(t *testing.T) {
	isolateConfig(t)
	ts := userServer(t, map[string]any{"username": "bob", "mustChangePassword": true}, nil)
	withStdin(t, "TempPass99\nTempPass99\nTempPass99\n")
	if err := cmdLogin([]string{"--url", ts.URL, "--user", "bob"}); err == nil {
		t.Fatal("keeping the temporary password was accepted")
	}
	if got := ts.requests[len(ts.requests)-1]; got != "POST /api/auth/logout" {
		t.Errorf("the session was not signed out (last request %q)", got)
	}
}
