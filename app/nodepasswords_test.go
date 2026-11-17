package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

func TestNewNodePassword(t *testing.T) {
	for i := 0; i < 200; i++ {
		for _, n := range []int{8, 20} {
			pw := newNodePassword(n)
			var lo, up, dig bool
			for _, r := range pw {
				switch {
				case unicode.IsLower(r):
					lo = true
				case unicode.IsUpper(r):
					up = true
				case unicode.IsDigit(r):
					dig = true
				default:
					t.Fatalf("%q has %q, which is not a letter or digit", pw, r)
				}
			}
			if len(pw) != n || !lo || !up || !dig {
				t.Fatalf("%q: length %d, lower %v upper %v digit %v", pw, len(pw), lo, up, dig)
			}
		}
	}
	if nodePasswordLen("VNC_PASSWORD") != 8 || nodePasswordLen("MYSQL_ROOT_PASSWORD") != 20 {
		t.Error("lengths")
	}
}

// Generated once, sealed, the same after a restart; a value set in the environment wins and is
// reported as replacing the generated one.
func TestNodePasswordsAreGeneratedOnce(t *testing.T) {
	for _, name := range nodePasswordNames() {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	passwordsSet = map[string]bool{}
	path := filepath.Join(t.TempDir(), "test.db")
	open := func() *App {
		st, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return &App{store: st}
	}
	app := open()
	if err := app.applyNodePasswords(); err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("MYSQL_ROOT_PASSWORD")
	if len(root) != 20 || root == exampleSecrets["MYSQL_ROOT_PASSWORD"] || len(os.Getenv("VNC_PASSWORD")) != 8 {
		t.Fatalf("generated root %q, vnc %q", root, os.Getenv("VNC_PASSWORD"))
	}
	var raw string
	app.store.db.QueryRow(`SELECT value FROM node_passwords WHERE name = 'MYSQL_ROOT_PASSWORD'`).Scan(&raw)
	if !strings.HasPrefix(raw, "v1:") || strings.Contains(raw, root) {
		t.Errorf("stored unsealed: %q", raw)
	}
	app.store.Close()

	// A restart: the same passwords, and an override from the environment.
	for _, name := range nodePasswordNames() {
		os.Unsetenv(name)
	}
	os.Setenv("POSTGRES_PASSWORD", "chosen-by-me")
	t.Cleanup(func() { os.Unsetenv("POSTGRES_PASSWORD") })
	app = open()
	if err := app.applyNodePasswords(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("MYSQL_ROOT_PASSWORD") != root {
		t.Error("the generated password changed on restart")
	}
	if os.Getenv("POSTGRES_PASSWORD") != "chosen-by-me" {
		t.Error("the environment's value was replaced")
	}
	r := withPrincipal(httptest.NewRequest("GET", "/api/solo/passwords", nil), principal{Socket: true})
	w := httptest.NewRecorder()
	app.handleNodePasswords(w, r)
	var out struct {
		Passwords []nodePasswordInfo `json:"passwords"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	for _, p := range out.Passwords {
		switch p.Name {
		case "POSTGRES_PASSWORD":
			if p.Source != "config" || !p.Replaced || p.Value != "chosen-by-me" {
				t.Errorf("postgres: %+v", p)
			}
		case "MYSQL_ROOT_PASSWORD":
			if p.Source != "generated" || p.Value != root {
				t.Errorf("root: %+v", p)
			}
		}
	}
	// Only on the socket.
	w = httptest.NewRecorder()
	app.handleNodePasswords(w, httptest.NewRequest("GET", "/api/solo/passwords", nil))
	if w.Code != http.StatusForbidden {
		t.Errorf("without the socket: %d", w.Code)
	}
}
