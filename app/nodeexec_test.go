package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// runWrapper runs nodeExecWrapper with the local /bin/sh, as the engine runs it in a node.
func runWrapper(t *testing.T, timeout string, stdin string, argv ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"-c", nodeExecWrapper, "sh", timeout}, argv...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// argv reaches the command exactly as given: spaces, quotes, `;` and `$` are data, not shell.
func TestNodeExecWrapperKeepsArgv(t *testing.T) {
	out, code := runWrapper(t, "30", "", "printf", "[%s]", "a b", "it's", "x; id", "$HOME", "")
	if code != 0 || out != "[a b][it's][x; id][$HOME][]" {
		t.Errorf("code %d out %q", code, out)
	}
}

func TestNodeExecWrapperExitStatusAndStdin(t *testing.T) {
	if _, code := runWrapper(t, "30", "", "sh", "-c", "exit 7"); code != 7 {
		t.Errorf("exit status %d, want 7", code)
	}
	if out, _ := runWrapper(t, "30", "line one\nline two\n", "wc", "-l"); strings.TrimSpace(out) != "2" {
		t.Errorf("stdin not passed: %q", out)
	}
}

// Where the image has timeout(1), a command past its time is killed and exits 124.
func TestNodeExecWrapperTimeout(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("no timeout(1) here; every node image has it")
	}
	start := time.Now()
	if _, code := runWrapper(t, "1", "", "sleep", "20"); code != 124 {
		t.Errorf("exit %d, want 124", code)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the command was not stopped")
	}
}

func TestNodeExecField(t *testing.T) {
	if text, b64, cut := nodeExecField("héllo"); text != "héllo" || b64 != "" || cut {
		t.Errorf("text: %q %q %v", text, b64, cut)
	}
	if text, b64, _ := nodeExecField("\x1f\x8b\xff"); text != "" || b64 != "H4v/" {
		t.Errorf("binary: %q %q", text, b64)
	}
	big := strings.Repeat("a", nodeExecOutputLimit+10)
	if text, _, cut := nodeExecField(big); len(text) != nodeExecOutputLimit || !cut || !utf8.ValidString(text) {
		t.Errorf("big: len %d cut %v", len(text), cut)
	}
}

// An /api path no route matched is a JSON 404; anything else still gets the web app.
func TestUnknownAPIPathIsAJSON404(t *testing.T) {
	h := spaHandler()
	for _, p := range []string{"/api/no/such/thing", "/api"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", p, nil))
		var e struct{ Error string }
		if w.Code != http.StatusNotFound || json.Unmarshal(w.Body.Bytes(), &e) != nil || !strings.Contains(e.Error, "POST "+p) {
			t.Errorf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/stacks/12", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("client route: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
}

// /api/me names the token a request was made with; a session has none.
func TestMeReportsItsToken(t *testing.T) {
	app := newTestApp(t)
	u, err := app.store.CreateUser("ada", "x", RoleUser, StatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	exp := "2027-02-03T00:00:00Z"
	tok := &APIToken{ID: 9, Name: "ci", Prefix: "dbc_ab", Scope: ScopeWrite, ExpiresAt: &exp}
	w := httptest.NewRecorder()
	app.handleMe(w, withPrincipal(httptest.NewRequest("GET", "/api/me", nil), principal{User: u, Token: tok}))
	var me struct {
		Username string `json:"username"`
		Token    *struct {
			Name, Scope string
			ExpiresAt   *string
		} `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Username != "ada" || me.Token == nil || me.Token.Name != "ci" || me.Token.Scope != ScopeWrite || me.Token.ExpiresAt == nil || *me.Token.ExpiresAt != exp {
		t.Errorf("token: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "hash") {
		t.Errorf("leaks more than the token's description: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	app.handleMe(w, withPrincipal(httptest.NewRequest("GET", "/api/me", nil), principal{User: u}))
	if strings.Contains(w.Body.String(), `"token"`) {
		t.Errorf("a session reported a token: %s", w.Body.String())
	}
}
