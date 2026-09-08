package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// cli_test.go — the tests that matter for a tool holding a credential.
//
// The one that would be a real bug if it regressed is TestLoginStoresTokenNotPassword:
// it drives the whole handshake against a fake server, asserts the request order, and
// then reads the config file back to prove the password is nowhere in it. Everything
// else here is the argument parsing and error mapping that decides whether a CI job
// gets a useful failure or a mystery.

// testServer stands in for DBCanvas, recording every request it receives.
type testServer struct {
	*httptest.Server
	requests []string // "METHOD /path", in order
	auth     []string // each request's Authorization header
	bodies   []string // each request's body
}

func (s *testServer) record(r *http.Request) {
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		s.bodies = append(s.bodies, string(raw))
	} else {
		s.bodies = append(s.bodies, "")
	}
}

func newTestServer(t *testing.T, h func(*testServer, http.ResponseWriter, *http.Request)) *testServer {
	t.Helper()
	ts := &testServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.record(r)
		h(ts, w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// isolateConfig points the config at a temp file and resets the global flags, so
// tests neither read nor write the developer's real credential.
func isolateConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("DBCANVAS_CONFIG", path)
	t.Setenv("DBCANVAS_URL", "")
	t.Setenv("DBCANVAS_TOKEN", "")
	g = globals{}
	t.Cleanup(func() { g = globals{} })
	return path
}

// ------------------------------------------------------------- config

func TestConfigRoundTrip(t *testing.T) {
	path := isolateConfig(t)

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("a missing config must not be an error: %v", err)
	}
	if len(c.Profiles) != 0 {
		t.Errorf("a missing config produced %d profiles", len(c.Profiles))
	}

	c.Profiles["lab"] = Profile{URL: "https://lab.example.net", Token: "dbc_secret", User: "jaime", Scope: "write"}
	c.Current = "lab"
	if err := saveConfig(c); err != nil {
		t.Fatalf("save: %v", err)
	}
	back, err := loadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if back.Current != "lab" || back.Profiles["lab"].Token != "dbc_secret" {
		t.Errorf("round trip lost data: %+v", back)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the config was not written to DBCANVAS_CONFIG: %v", err)
	}
}

// TestConfigIsPrivate is the one file-permission test worth having: this file holds
// a bearer token, and a world-readable one is a credential leak.
func TestConfigIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes do not apply on Windows")
	}
	path := isolateConfig(t)
	c, _ := loadConfig()
	c.Profiles["x"] = Profile{URL: "http://x", Token: "dbc_x"}
	if err := saveConfig(c); err != nil {
		t.Fatalf("save: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("config mode is %o, want 600", mode)
	}

	// And a pre-existing loose file is tightened rather than left as it was — a
	// restored backup or a copied dotfiles repo is exactly how this happens.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(c); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	st, _ = os.Stat(path)
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("a loose config stayed at %o instead of being tightened to 600", mode)
	}
}

func TestConfigCorruptFileSaysSo(t *testing.T) {
	path := isolateConfig(t)
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := loadConfig(); err == nil {
		t.Error("a corrupt config was accepted silently")
	} else if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("the error should name the problem: %v", err)
	}
}

// TestResolvePrecedence pins the order, and the environment case is the one that
// matters: CI passes a token that way, and a config file that happens to exist on
// the runner must not win over it.
func TestResolvePrecedence(t *testing.T) {
	isolateConfig(t)
	cfg := Config{
		Current: "a",
		Profiles: map[string]Profile{
			"a": {URL: "http://config-a", Token: "tok-a", User: "jaime", Scope: "write"},
			"b": {URL: "http://config-b", Token: "tok-b"},
		},
	}

	p, name, err := cfg.resolve("", "", "")
	if err != nil || p.URL != "http://config-a" || name != "a" {
		t.Errorf("the current profile should win by default: %+v %q %v", p, name, err)
	}

	if p, _, _ := cfg.resolve("b", "", ""); p.Token != "tok-b" {
		t.Errorf("--profile did not select b: %+v", p)
	}

	t.Setenv("DBCANVAS_URL", "http://from-env")
	t.Setenv("DBCANVAS_TOKEN", "tok-env")
	p, _, _ = cfg.resolve("", "", "")
	if p.URL != "http://from-env" || p.Token != "tok-env" {
		t.Errorf("the environment must beat the config file: %+v", p)
	}
	// A token from the environment is not ours to describe, so the profile's stale
	// scope and user must not be reported alongside it.
	if p.User != "" || p.Scope != "" {
		t.Errorf("an env token carried the config profile's metadata: %+v", p)
	}

	p, _, _ = cfg.resolve("", "http://from-flag", "tok-flag")
	if p.URL != "http://from-flag" || p.Token != "tok-flag" {
		t.Errorf("flags must beat the environment: %+v", p)
	}

	// A trailing slash on the URL would double every path.
	p, _, _ = cfg.resolve("", "http://x/", "t")
	if p.URL != "http://x" {
		t.Errorf("the trailing slash was not trimmed: %q", p.URL)
	}

	// An empty config with nothing in the environment either. Set explicitly,
	// because the DBCANVAS_URL above is still in force for the rest of this test.
	t.Setenv("DBCANVAS_URL", "")
	t.Setenv("DBCANVAS_TOKEN", "")
	if _, _, err := (Config{Profiles: map[string]Profile{}}).resolve("", "", ""); err == nil {
		t.Error("an empty config resolved to something")
	}
}

// ------------------------------------------------------------- login

// TestLoginStoresTokenNotPassword is the important one. It asserts the three-request
// handshake happens in order, and that what lands on disk is the token and not the
// password.
func TestLoginStoresTokenNotPassword(t *testing.T) {
	path := isolateConfig(t)
	const password = "correct-horse-battery"
	const secret = "dbc_theactualsecretvalue"

	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "dbcanvas_session", Value: "sess", Path: "/"})
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "username": "jaime", "role": "user"})
		case "POST /api/tokens":
			// The credential-creating request must be cookie-authenticated, never
			// bearer — that asymmetry is the point of the whole flow.
			if r.Header.Get("Authorization") != "" {
				t.Errorf("token creation sent an Authorization header: %q", r.Header.Get("Authorization"))
			}
			if _, err := r.Cookie("dbcanvas_session"); err != nil {
				t.Error("token creation did not carry the session cookie")
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"token": map[string]any{
					"id": 7, "name": "dbcanvas-cli on host", "prefix": "dbc_theactu",
					"scope": "write", "expiresAt": "2026-12-01T00:00:00Z",
				},
				"secret": secret,
			})
		case "POST /api/auth/logout":
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// Non-terminal stdin makes promptSecret read a line, which is how the password
	// gets in without a TTY.
	withStdin(t, password+"\n")
	if err := cmdLogin([]string{"--url", ts.URL, "--user", "jaime", "--expires", "90"}); err != nil {
		t.Fatalf("login: %v", err)
	}

	want := []string{"POST /api/auth/login", "POST /api/tokens", "POST /api/auth/logout"}
	if len(ts.requests) != len(want) {
		t.Fatalf("requests were %v, want %v", ts.requests, want)
	}
	for i := range want {
		if ts.requests[i] != want[i] {
			t.Errorf("request %d was %q, want %q", i, ts.requests[i], want[i])
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the config: %v", err)
	}
	if strings.Contains(string(raw), password) {
		t.Fatal("the password was written to the config file")
	}
	if !strings.Contains(string(raw), secret) {
		t.Fatal("the token was not written to the config file")
	}
	cfg, _ := loadConfig()
	p := cfg.Profiles[cfg.Current]
	if p.Token != secret || p.URL != ts.URL || p.User != "jaime" || p.Scope != "write" {
		t.Errorf("stored profile is %+v", p)
	}
}

// TestLoginSignsOutEvenWhenTokenCreationFails: the session must not be left live
// just because the second request failed.
func TestLoginSignsOutEvenWhenTokenCreationFails(t *testing.T) {
	isolateConfig(t)
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "dbcanvas_session", Value: "sess", Path: "/"})
			json.NewEncoder(w).Encode(map[string]any{"username": "jaime"})
		case "/api/tokens":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "only an administrator can create a token that never expires"})
		case "/api/auth/logout":
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		}
	})
	withStdin(t, "pw\n")
	err := cmdLogin([]string{"--url", ts.URL, "--user", "jaime", "--expires", "0"})
	if err == nil {
		t.Fatal("a refused token creation should be an error")
	}
	if !strings.Contains(err.Error(), "administrator") {
		t.Errorf("the server's message should survive: %v", err)
	}
	if got := ts.requests[len(ts.requests)-1]; got != "POST /api/auth/logout" {
		t.Errorf("the session was not signed out on the error path (last request %q)", got)
	}
}

func TestLoginNeedsAURL(t *testing.T) {
	isolateConfig(t)
	withStdin(t, "pw\n")
	if err := cmdLogin([]string{"--user", "jaime"}); err == nil {
		t.Error("login with no URL anywhere should say so")
	}
}

// TestLogoutRevokesServerSide: deleting the local copy of a live credential is not
// logging out.
func TestLogoutRevokesServerSide(t *testing.T) {
	isolateConfig(t)
	revoked := false
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tokens" && r.Method == "GET":
			json.NewEncoder(w).Encode(map[string]any{"tokens": []map[string]any{
				{"id": 7, "prefix": "dbc_abcd1234", "state": "active"},
				{"id": 8, "prefix": "dbc_zzzz9999", "state": "active"}, // somebody else's
			}})
		case r.Method == "DELETE" && r.URL.Path == "/api/tokens/7":
			revoked = true
			json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
		case r.Method == "DELETE":
			t.Errorf("revoked the wrong token: %s", r.URL.Path)
		}
	})
	cfg, _ := loadConfig()
	cfg.Current = "default"
	cfg.Profiles["default"] = Profile{URL: ts.URL, Token: "dbc_abcd1234rest"}
	saveConfig(cfg)

	if err := cmdLogout(nil); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if !revoked {
		t.Error("logout did not revoke the token on the server")
	}
	back, _ := loadConfig()
	if _, still := back.Profiles["default"]; still {
		t.Error("logout left the profile in the config")
	}
}

// ------------------------------------------------------------- requests

func TestEveryRequestCarriesTheBearerToken(t *testing.T) {
	isolateConfig(t)
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{})
	})
	c := newClient(ts.URL, "dbc_token")
	if err := c.get("/api/stacks", nil); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := ts.auth[0]; got != "Bearer dbc_token" {
		t.Errorf("Authorization was %q", got)
	}
}

// TestErrorMapping is what decides whether a failing CI job is diagnosable.
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status   int
		body     string
		wantCode int
		wantText string
	}{
		{401, `{"error":"invalid, expired or revoked API token"}`, 3, "dbcanvas login"},
		{403, `{"error":"this endpoint needs a token with write scope; this token has read"}`, 1, "write scope"},
		{404, `{"error":"stack not found"}`, 1, "stack not found"},
		{409, `{"error":"node is not running"}`, 1, "node is not running"},
		{500, ``, 1, "request failed (500)"},
	}
	for _, c := range cases {
		ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			io.WriteString(w, c.body)
		})
		err := newClient(ts.URL, "t").get("/api/stacks", nil)
		if err == nil {
			t.Fatalf("%d did not produce an error", c.status)
		}
		if got := exitCodeFor(err); got != c.wantCode {
			t.Errorf("%d mapped to exit %d, want %d", c.status, got, c.wantCode)
		}
		if !strings.Contains(err.Error(), c.wantText) {
			t.Errorf("%d said %q, want it to mention %q", c.status, err.Error(), c.wantText)
		}
		ts.Close()
	}

	// The non-HTTP failures map too.
	if got := exitCodeFor(errNotConfigured); got != 3 {
		t.Errorf("not-configured mapped to %d, want 3", got)
	}
	if got := exitCodeFor(fmt.Errorf("wrapped: %w", errWaitFailed)); got != 4 {
		t.Errorf("a wait failure mapped to %d, want 4", got)
	}
	if got := exitCodeFor(errUsage); got != 2 {
		t.Errorf("a usage error mapped to %d, want 2", got)
	}
}

func TestUnreachableServerNamesTheURL(t *testing.T) {
	// Port 1 on loopback refuses connections promptly on every platform we build for.
	err := newClient("http://127.0.0.1:1", "t").get("/api/stacks", nil)
	if err == nil {
		t.Fatal("connecting to a closed port succeeded")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("the error should name the URL it could not reach: %v", err)
	}
}

// ------------------------------------------------------------- stacks

func TestResolveStackByNameAndID(t *testing.T) {
	stacks := []map[string]any{
		{"id": 1, "name": "pxc-lab", "status": "deployed"},
		{"id": 2, "name": "PXC-Lab", "status": "draft"},
		{"id": 3, "name": "mongo", "status": "draft"},
		{"id": 42, "name": "7", "status": "draft"}, // a name that looks like an id
	}
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(stacks)
	})
	c := newClient(ts.URL, "t")

	if st, err := resolveStack(c, "3"); err != nil || st.Name != "mongo" {
		t.Errorf("by id: %+v %v", st, err)
	}
	if st, err := resolveStack(c, "mongo"); err != nil || st.ID != 3 {
		t.Errorf("by name: %+v %v", st, err)
	}
	// An exact name match wins over a case-insensitive one, so "pxc-lab" is not
	// ambiguous even though "PXC-Lab" also exists.
	if st, err := resolveStack(c, "pxc-lab"); err != nil || st.ID != 1 {
		t.Errorf("exact match should win: %+v %v", st, err)
	}
	if st, err := resolveStack(c, "PXC-LAB"); err == nil {
		t.Errorf("an ambiguous case-insensitive match should refuse, got %+v", st)
	} else if !strings.Contains(err.Error(), "use an id") {
		t.Errorf("the refusal should say what to do: %v", err)
	}
	// A name that looks like an id: the id lookup misses, the name lookup finds it.
	if st, err := resolveStack(c, "7"); err != nil || st.ID != 42 {
		t.Errorf("numeric name: %+v %v", st, err)
	}
	if _, err := resolveStack(c, "nope"); err == nil {
		t.Error("an unknown stack resolved")
	} else if !strings.Contains(err.Error(), "stack list") {
		t.Errorf("the error should suggest how to find one: %v", err)
	}
}

// TestWaitForNodesOutcomes covers the three ways --wait can end, because its exit
// code is what a pipeline branches on.
func TestWaitForNodesOutcomes(t *testing.T) {
	t.Run("all running", func(t *testing.T) {
		ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "name": "s", "deployments": []map[string]any{
					{"nodeId": "a", "state": "running"}, {"nodeId": "b", "state": "running"}}})
		})
		if err := waitForNodes(newClient(ts.URL, "t"), Stack{ID: 1, Name: "s"}, 5*time.Second); err != nil {
			t.Errorf("a fully-running stack reported %v", err)
		}
	})

	t.Run("one failed", func(t *testing.T) {
		ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "name": "s", "deployments": []map[string]any{
					{"nodeId": "a", "state": "running"}, {"nodeId": "b", "state": "error"}}})
		})
		err := waitForNodes(newClient(ts.URL, "t"), Stack{ID: 1, Name: "s"}, 5*time.Second)
		if err == nil {
			t.Fatal("a failed node was reported as success")
		}
		if exitCodeFor(err) != 4 {
			t.Errorf("a failed deploy mapped to exit %d, want 4", exitCodeFor(err))
		}
	})

	t.Run("timeout", func(t *testing.T) {
		ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "name": "s", "deployments": []map[string]any{
					{"nodeId": "a", "state": "provisioning"}}})
		})
		err := waitForNodes(newClient(ts.URL, "t"), Stack{ID: 1, Name: "s"}, 1*time.Millisecond)
		if err == nil || exitCodeFor(err) != 4 {
			t.Errorf("a timeout gave %v (exit %d), want a wait failure", err, exitCodeFor(err))
		}
	})
}

// ------------------------------------------------------------- api command

func TestAPICommandArgumentForms(t *testing.T) {
	isolateConfig(t)
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	t.Setenv("DBCANVAS_URL", ts.URL)
	t.Setenv("DBCANVAS_TOKEN", "dbc_t")

	// `api GET /path`, and the bare `api /path` shorthand.
	if err := cmdAPI([]string{"GET", "/api/stacks"}); err != nil {
		t.Fatalf("explicit method: %v", err)
	}
	if err := cmdAPI([]string{"/api/stacks"}); err != nil {
		t.Fatalf("implied GET: %v", err)
	}
	if got := ts.requests[1]; got != "GET /api/stacks" {
		t.Errorf("the bare path form sent %q", got)
	}
	// A body with no method means POST, which is what somebody means by it.
	if err := cmdAPI([]string{"/api/stacks", "--data", `{"name":"x"}`}); err != nil {
		t.Fatalf("implied POST: %v", err)
	}
	if got := ts.requests[2]; got != "POST /api/stacks" {
		t.Errorf("a body did not imply POST: %q", got)
	}
	// A path without a leading slash still works.
	if err := cmdAPI([]string{"GET", "api/stacks"}); err != nil {
		t.Fatalf("missing leading slash: %v", err)
	}

	// Refusals, each with a message that says what was meant.
	if err := cmdAPI([]string{"GET", "/healthz"}); err == nil {
		t.Error("a non-/api/ path was accepted")
	}
	if err := cmdAPI([]string{"FETCH", "/api/stacks"}); err == nil {
		t.Error("a bogus HTTP method was accepted")
	}
	if err := cmdAPI(nil); err != errUsage {
		t.Errorf("no arguments gave %v, want a usage error", err)
	}
	if err := cmdAPI([]string{"POST", "/api/stacks", "--data", "{oops"}); err == nil {
		t.Error("invalid JSON was sent to the server instead of being caught here")
	}
}

// TestFlagsAfterPositionalsAreParsed pins the fix for a real bug: Go's flag package
// stops at the first non-flag argument, so `api POST /path --data '{}'` silently sent
// no body at all — and that is the form the documentation uses.
func TestFlagsAfterPositionalsAreParsed(t *testing.T) {
	fs := flagsFor("test")
	data := fs.String("data", "", "")
	wait := fs.Bool("wait", false, "")
	if err := parse(fs, []string{"POST", "/api/stacks", "--data", `{"a":1}`, "--wait"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *data != `{"a":1}` {
		t.Errorf("--data after positionals was not parsed: %q", *data)
	}
	if !*wait {
		t.Error("--wait after positionals was not parsed")
	}
	if nargs() != 2 || arg(0) != "POST" || arg(1) != "/api/stacks" {
		t.Errorf("positionals are %v", positional)
	}

	// And `--` ends flag parsing: node exec passes a remote command through with
	// its own flags intact.
	fs2 := flagsFor("test2")
	json := fs2.Bool("json", false, "")
	if err := parse(fs2, []string{"lab", "pxc-01", "--", "mysql", "-e", "SHOW STATUS"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *json {
		t.Error("a flag after -- was consumed as our own")
	}
	want := []string{"lab", "pxc-01", "mysql", "-e", "SHOW STATUS"}
	if len(positional) != len(want) {
		t.Fatalf("positionals after -- are %v, want %v", positional, want)
	}
	for i := range want {
		if positional[i] != want[i] {
			t.Errorf("positional %d is %q, want %q", i, positional[i], want[i])
		}
	}
}

func TestAPIBodyFromFileAndStdin(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "body.json")
	os.WriteFile(file, []byte(`{"from":"file"}`), 0o600)

	if raw, err := bodyFrom("@" + file); err != nil || !strings.Contains(string(raw), "file") {
		t.Errorf("@file: %s %v", raw, err)
	}
	withStdin(t, `{"from":"stdin"}`)
	if raw, err := bodyFrom("@-"); err != nil || !strings.Contains(string(raw), "stdin") {
		t.Errorf("@-: %s %v", raw, err)
	}
	if raw, err := bodyFrom(`{"inline":1}`); err != nil || !strings.Contains(string(raw), "inline") {
		t.Errorf("inline: %s %v", raw, err)
	}
	if _, err := bodyFrom("@" + filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing body file was accepted")
	}
}

// ------------------------------------------------------------- node refs

func TestParseNodeRef(t *testing.T) {
	for _, c := range []struct {
		in     string
		remote bool
		ref    nodeRef
	}{
		{"pxc-lab:pxc-01:/etc/my.cnf", true, nodeRef{"pxc-lab", "pxc-01", "/etc/my.cnf"}},
		{"1:node-1:/tmp/", true, nodeRef{"1", "node-1", "/tmp/"}},
		{"./local/file", false, nodeRef{}},
		{"/absolute/path", false, nodeRef{}},
		{"stack:node", false, nodeRef{}}, // two parts is not a node reference
		// A Windows drive letter must not be mistaken for a stack.
		{`C:\Users\jaime\my.cnf`, false, nodeRef{}},
	} {
		remote, ref := parseNodeRef(c.in)
		if remote != c.remote || ref != c.ref {
			t.Errorf("parseNodeRef(%q) = %v %+v, want %v %+v", c.in, remote, ref, c.remote, c.ref)
		}
	}
}

// ------------------------------------------------------------- endpoints

func TestEndpointFiltering(t *testing.T) {
	ep := endpointDoc{Method: "POST", Path: "/api/stacks/{id}/deploy",
		Group: "Stacks", Summary: "Provision every node in a stack's design.", Scope: "write"}
	for _, c := range []struct {
		q    string
		want bool
	}{
		{"", true},
		{"deploy", true},
		{"DEPLOY", true},
		{"provision", true},
		{"stacks deploy", true},
		{"POST", true},
		{"benchmark", false},
		{"deploy benchmark", false},
	} {
		if got := endpointMatches(ep, c.q); got != c.want {
			t.Errorf("endpointMatches(%q) = %v, want %v", c.q, got, c.want)
		}
	}
}

// ------------------------------------------------------------- formatting

func TestUntilText(t *testing.T) {
	at := func(d time.Duration) string { return time.Now().Add(d).Format(time.RFC3339) }
	for _, c := range []struct {
		in   string
		want string
	}{
		{"", "never"},
		{at(73 * time.Hour), "3d"},
		{at(5*time.Hour + 30*time.Minute), "5h"},
		{at(90 * time.Second), "1m"},
		{at(-time.Hour), "expired"},
	} {
		if got := untilText(c.in); got != c.want {
			t.Errorf("untilText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// An unparseable value is passed through rather than turned into a confident lie.
	if got := untilText("tomorrow"); got != "tomorrow" {
		t.Errorf("untilText on nonsense = %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate kept-length = %q", got)
	}
	if got := truncate("a-very-long-stack-name", 8); got != "a-very-…" {
		t.Errorf("truncate = %q", got)
	}
}

// ------------------------------------------------------------- helpers

// withStdin replaces os.Stdin with a pipe carrying the given text, so the password
// prompt's non-TTY path can be driven.
func withStdin(t *testing.T, text string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		io.WriteString(w, text)
		w.Close()
	}()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
}

// ------------------------------------------------------------- password

// TestPasswordChangeUsesAPasswordSession pins the same asymmetry `login` relies on:
// the endpoint is cookie-only, so the CLI signs in with the current password rather
// than sending its stored token — and the stored token must never be sent to it.
func TestPasswordChangeUsesAPasswordSession(t *testing.T) {
	isolateConfig(t)
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "dbcanvas_session", Value: "sess", Path: "/"})
			json.NewEncoder(w).Encode(map[string]any{"username": "jaime"})
		case "POST /api/me/password":
			if r.Header.Get("Authorization") != "" {
				t.Errorf("the password change sent a bearer token: %q", r.Header.Get("Authorization"))
			}
			if _, err := r.Cookie("dbcanvas_session"); err != nil {
				t.Error("the password change did not carry the session cookie")
			}
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "tokensRevoked": 0})
		case "POST /api/auth/logout":
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	cfg, _ := loadConfig()
	cfg.Current = "default"
	cfg.Profiles["default"] = Profile{URL: ts.URL, Token: "dbc_stored", User: "jaime"}
	saveConfig(cfg)

	withStdin(t, "oldpassword\na-new-password\na-new-password\n")
	if err := cmdPassword(nil); err != nil {
		t.Fatalf("password: %v", err)
	}
	want := []string{"POST /api/auth/login", "POST /api/me/password", "POST /api/auth/logout"}
	if len(ts.requests) != len(want) {
		t.Fatalf("requests were %v, want %v", ts.requests, want)
	}
	for i := range want {
		if ts.requests[i] != want[i] {
			t.Errorf("request %d was %q, want %q", i, ts.requests[i], want[i])
		}
	}
	// The change body must carry both passwords and the opt-out.
	body := ts.bodies[1]
	for _, frag := range []string{`"currentPassword":"oldpassword"`, `"newPassword":"a-new-password"`, `"revokeTokens":false`} {
		if !strings.Contains(body, frag) {
			t.Errorf("the change body %q is missing %s", body, frag)
		}
	}
	// The stored token is untouched by a change that did not revoke it.
	back, _ := loadConfig()
	if back.Profiles["default"].Token != "dbc_stored" {
		t.Error("a non-revoking password change discarded the stored token")
	}
}

// TestPasswordChangeRefusalsAreLocal: a mismatch or a too-short password is caught
// before the round trip, so the server never sees something it would only refuse.
func TestPasswordChangeRefusalsAreLocal(t *testing.T) {
	isolateConfig(t)
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/me/password" {
			t.Error("a locally-invalid change was sent to the server")
		}
		json.NewEncoder(w).Encode(map[string]any{"username": "jaime"})
	})
	cfg, _ := loadConfig()
	cfg.Current = "default"
	cfg.Profiles["default"] = Profile{URL: ts.URL, Token: "dbc_stored", User: "jaime"}
	saveConfig(cfg)

	for _, c := range []struct{ name, stdin, wants string }{
		{"mismatch", "old\nnewpassword1\nnewpassword2\n", "do not match"},
		{"too short", "old\nshort\nshort\n", "at least 8"},
		{"trailing space", "old\npassword123 \npassword123 \n", "space"},
	} {
		withStdin(t, c.stdin)
		err := cmdPassword(nil)
		if err == nil {
			t.Errorf("%s was accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wants) {
			t.Errorf("%s: error %q should mention %q", c.name, err.Error(), c.wants)
		}
	}
}

// TestPasswordChangeWithRevokeDropsTheStoredToken: after --revoke-tokens the config's
// token is dead, and leaving it there would only produce a puzzling 401 later.
func TestPasswordChangeWithRevokeDropsTheStoredToken(t *testing.T) {
	isolateConfig(t)
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "dbcanvas_session", Value: "sess", Path: "/"})
			json.NewEncoder(w).Encode(map[string]any{"username": "jaime"})
		case "/api/me/password":
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "tokensRevoked": 2})
		default:
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		}
	})
	cfg, _ := loadConfig()
	cfg.Current = "default"
	cfg.Profiles["default"] = Profile{URL: ts.URL, Token: "dbc_stored", User: "jaime"}
	saveConfig(cfg)

	withStdin(t, "oldpassword\na-new-password\na-new-password\n")
	if err := cmdPassword([]string{"--revoke-tokens"}); err != nil {
		t.Fatalf("password --revoke-tokens: %v", err)
	}
	back, _ := loadConfig()
	if _, still := back.Profiles["default"]; still {
		t.Error("the dead token was left in the config")
	}
}

// TestPromptsShareOneReader pins the fix for a real bug: a fresh bufio.Reader per
// prompt reads ahead and swallows the next prompt's line, so piping answers into a
// multi-prompt command failed on the second one with EOF.
func TestPromptsShareOneReader(t *testing.T) {
	withStdin(t, "jaime\nfirst-secret\nsecond-secret\n")
	if got, err := prompt("user: "); err != nil || got != "jaime" {
		t.Fatalf("first prompt: %q %v", got, err)
	}
	if got, err := promptSecret("pw: "); err != nil || got != "first-secret" {
		t.Fatalf("second prompt: %q %v", got, err)
	}
	if got, err := promptSecret("pw again: "); err != nil || got != "second-secret" {
		t.Fatalf("third prompt: %q %v", got, err)
	}
}

// TestPromptAcceptsAnUnterminatedLastLine: `printf 'pw'` with no newline is still a
// password, and refusing it would be a surprising way to fail in a script.
func TestPromptAcceptsAnUnterminatedLastLine(t *testing.T) {
	withStdin(t, "no-trailing-newline")
	if got, err := promptSecret("pw: "); err != nil || got != "no-trailing-newline" {
		t.Errorf("unterminated password: %q %v", got, err)
	}
}

// ------------------------------------------------------------- compose

// TestParseNodeSpec covers the --node mini-syntax. It is the only place in the CLI
// that invents a grammar, so it is the only place a typo can mean something other
// than what was typed — hence the refusals matter as much as the successes.
func TestParseNodeSpec(t *testing.T) {
	for _, c := range []struct {
		in   string
		want map[string]any
	}{
		{"ps", map[string]any{"kind": "ps"}},
		{"pxc:3", map[string]any{"kind": "pxc", "count": 3}},
		{"ps,version=8.0.45", map[string]any{"kind": "ps", "version": "8.0.45"}},
		{"ps,os=el9,monitor", map[string]any{"kind": "ps", "os": "el9", "monitor": true}},
		// The user's own line.
		{"pxc:3,version=8.4.5,os=el8,monitor", map[string]any{
			"kind": "pxc", "count": 3, "version": "8.4.5", "os": "el8", "monitor": true}},
		{"ps,ldap,monitor,cert,export", map[string]any{
			"kind": "ps", "ldap": true, "monitor": true, "cert": true, "export": true}},
		// A flag can be given a value, so a spec can turn a default off.
		{"ps,gtid=false", map[string]any{"kind": "ps", "gtid": false}},
		// Keys are case-insensitive and map to the API's camelCase.
		{"ps,MemoryGB=8,monitorWith=pmm-a", map[string]any{
			"kind": "ps", "memoryGb": 8, "monitorWith": "pmm-a"}},
		{"ps , os = el9 , monitor ", map[string]any{"kind": "ps", "os": "el9", "monitor": true}},
	} {
		got, err := parseNodeSpec(c.in)
		if err != nil {
			t.Errorf("parseNodeSpec(%q): %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseNodeSpec(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for k, v := range c.want {
			if fmt.Sprint(got[k]) != fmt.Sprint(v) {
				t.Errorf("parseNodeSpec(%q)[%s] = %v, want %v", c.in, k, got[k], v)
			}
		}
	}
}

func TestParseNodeSpecRefusals(t *testing.T) {
	for _, c := range []struct{ in, wants string }{
		{"", "empty"},
		{"ps,verison=8.0.45", "unknown option"}, // the typo people will make
		{"ps,version", "needs a value"},         // a value option given as a flag
		{"pxc:many", "positive number"},
		{"pxc:0", "positive number"},
		{"ps,cpus=lots", "want a number"},
		{"ps,monitor=perhaps", "true or false"},
	} {
		if _, err := parseNodeSpec(c.in); err == nil {
			t.Errorf("parseNodeSpec(%q) was accepted", c.in)
		} else if !strings.Contains(err.Error(), c.wants) {
			t.Errorf("parseNodeSpec(%q): %q should mention %q", c.in, err.Error(), c.wants)
		}
	}
}

// TestComposeSendsTheSpecItWasGiven: the CLI is a front end, so what matters is that
// the flags become exactly the JSON the API documents.
func TestComposeSendsTheSpecItWasGiven(t *testing.T) {
	isolateConfig(t)
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "resolved": []any{}, "design": map[string]any{},
		})
	})
	t.Setenv("DBCANVAS_URL", ts.URL)
	t.Setenv("DBCANVAS_TOKEN", "dbc_t")

	err := stackCompose([]string{"repro-1234", "--ttl", "8h", "--dry-run",
		"--node", "pxc:3,version=8.4.5,os=el8,monitor",
		"--node", "ps,version=8.0.45,os=el9,ldap,monitor",
		"--node", "pmm,version=3"})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if len(ts.requests) != 1 || ts.requests[0] != "POST /api/stacks/compose" {
		t.Fatalf("requests were %v", ts.requests)
	}

	var sent struct {
		Name   string `json:"name"`
		TTL    string `json:"ttl"`
		DryRun bool   `json:"dryRun"`
		Nodes  []struct {
			Kind    string `json:"kind"`
			Count   int    `json:"count"`
			Version string `json:"version"`
			OS      string `json:"os"`
			Monitor bool   `json:"monitor"`
			LDAP    bool   `json:"ldap"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(ts.bodies[0]), &sent); err != nil {
		t.Fatalf("the body is not the documented shape: %v", err)
	}
	if sent.Name != "repro-1234" || sent.TTL != "8h" || !sent.DryRun {
		t.Errorf("sent %+v", sent)
	}
	if len(sent.Nodes) != 3 {
		t.Fatalf("sent %d nodes, want 3", len(sent.Nodes))
	}
	if n := sent.Nodes[0]; n.Kind != "pxc" || n.Count != 3 || n.Version != "8.4.5" ||
		n.OS != "el8" || !n.Monitor {
		t.Errorf("the PXC entry became %+v", n)
	}
	if n := sent.Nodes[1]; n.Kind != "ps" || !n.LDAP || !n.Monitor || n.Version != "8.0.45" {
		t.Errorf("the PS entry became %+v", n)
	}
}

// TestComposeNeedsNodes: an empty invocation prints the example rather than posting
// a spec the server would only refuse.
func TestComposeNeedsNodes(t *testing.T) {
	isolateConfig(t)
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		t.Error("compose posted a spec with no nodes")
	})
	t.Setenv("DBCANVAS_URL", ts.URL)
	t.Setenv("DBCANVAS_TOKEN", "dbc_t")
	if err := stackCompose([]string{"x"}); err != errUsage {
		t.Errorf("compose with no --node returned %v, want a usage error", err)
	}
}

// TestValidateReadsIssuesNotProblems pins a fix: this decoded the wrong field name,
// so every design "validated cleanly" however broken it was.
func TestValidateReadsIssuesNotProblems(t *testing.T) {
	isolateConfig(t)
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/stacks" && r.Method == "GET":
			json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "name": "lab"}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "issues": []map[string]string{
				{"level": "error", "message": "an Intranet node is required"},
			}})
		}
	})
	t.Setenv("DBCANVAS_URL", ts.URL)
	t.Setenv("DBCANVAS_TOKEN", "dbc_t")

	err := stackValidate([]string{"lab"})
	if err == nil {
		t.Fatal("a design with an error validated cleanly")
	}
	if !strings.Contains(err.Error(), "1 error") {
		t.Errorf("error was %q", err)
	}

	// A warning alone is worth printing and not worth failing over.
	ts2 := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/stacks" && r.Method == "GET" {
			json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "name": "lab"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "issues": []map[string]string{
			{"level": "warning", "message": "DOMAIN is not set"},
		}})
	})
	t.Setenv("DBCANVAS_URL", ts2.URL)
	if err := stackValidate([]string{"lab"}); err != nil {
		t.Errorf("a warning-only design failed: %v", err)
	}
}

// TestResolveNodeByLabelAndID is the regression for a bug that made every per-node
// command unusable without a lookup the CLI did not offer.
//
// The API keys on the design node's id, and the designer generates those from a
// timestamp — "ps-mt1kvaak-3". The label is the node's hostname, "ps-01", and it is
// what the canvas, every panel and the compose plan show. So the one string a person
// has was the one string that did not work, and `node list` printed only the id.
func TestResolveNodeByLabelAndID(t *testing.T) {
	design := map[string]any{"nodes": []map[string]any{
		{"id": "ps-mt1kvaak-3", "label": "ps-01", "type": "ps"},
		{"id": "n-pmm-01", "label": "PS-01", "type": "pmm"}, // differs only in case
		{"id": "n-valkey-01", "label": "valkey-01", "type": "valkey"},
		{"id": "odd-1", "label": "n-valkey-01", "type": "psm"}, // a label that is another's id
	}}
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "name": "lab", "design": design,
			"deployments": []map[string]any{
				{"nodeId": "ps-mt1kvaak-3", "state": "running", "containerId": "abc123def4567"},
			},
		})
	})
	c := newClient(ts.URL, "t")

	for _, tc := range []struct{ ref, want string }{
		{"ps-01", "ps-mt1kvaak-3"},         // by label — the case that was broken
		{"ps-mt1kvaak-3", "ps-mt1kvaak-3"}, // by id — still works
		{"valkey-01", "n-valkey-01"},       // exact label beats the case-fold pass
		{"n-valkey-01", "n-valkey-01"},     // an id wins over a label that looks like it
		{"VALKEY-01", "n-valkey-01"},       // case-insensitive when nothing exact matches
	} {
		got, err := resolveNode(c, 1, tc.ref)
		if err != nil || got != tc.want {
			t.Errorf("%q resolved to %q (%v), want %q", tc.ref, got, err, tc.want)
		}
	}

	// An unknown node names what is there, because that is what somebody needs after
	// a failed guess.
	_, err := resolveNode(c, 1, "nope")
	if err == nil {
		t.Fatal("an unknown node was accepted")
	}
	for _, want := range []string{"nope", "ps-01", "valkey-01"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q should mention %q", err, want)
		}
	}
}

// TestNodeCommandsSendTheResolvedID: resolving is worth nothing if the command then
// sends the raw string anyway, which is exactly the bug.
func TestNodeCommandsSendTheResolvedID(t *testing.T) {
	isolateConfig(t)
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/stacks":
			json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "name": "lab"}})
		case r.URL.Path == "/api/stacks/1":
			json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "name": "lab",
				"design": map[string]any{"nodes": []map[string]any{
					{"id": "ps-mt1kvaak-3", "label": "ps-01", "type": "ps"},
				}},
			})
		default:
			w.Write([]byte(`{}`))
		}
	})
	t.Setenv("DBCANVAS_URL", ts.URL)
	t.Setenv("DBCANVAS_TOKEN", "dbc_x")
	g = globals{}

	if err := nodeAction("restart")([]string{"lab", "ps-01"}); err != nil {
		t.Fatal(err)
	}
	want := "POST /api/stacks/1/nodes/ps-mt1kvaak-3/restart"
	var found bool
	for _, r := range ts.requests {
		if r == want {
			found = true
		}
	}
	if !found {
		t.Errorf("sent %v, want one to be %q", ts.requests, want)
	}
}

// TestShellJoinRoundTripsThroughARealShell is the regression for a bug that made
// `node exec` run a different command than the one it was given.
//
// exec has no endpoint: the command is typed into a login shell over the console, so
// the line has to survive that shell's own splitting. It used to be
// strings.Join(args, " "), which threw every boundary away — an argument with a space
// became two, a backslash was eaten, and an argument containing `;` ran as a SECOND
// command on the node.
//
// The assertion is the property rather than a particular escaping style: hand the
// line to /bin/sh and require the argv it produces to be the argv we started with.
func TestShellJoinRoundTripsThroughARealShell(t *testing.T) {
	for _, argv := range [][]string{
		{"mysql", "-e", `SHOW STATUS LIKE "wsrep%"`},   // the documented example
		{"mysql", "-e", `SHOW ENGINE INNODB STATUS\G`}, // the skill's example
		{"printf", `[%s]\n`, "one two", "three"},
		{"echo", "harmless; id -un"}, // must NOT chain
		{"echo", "a && b", "c | d", "e > f"},
		{"echo", "$HOME", "`id`", "$(id)"}, // must stay literal
		{"echo", "it's", "quoted'twice'"},
		{"echo", "*", "?", "[a-z]"}, // globs must not expand
		{"echo", "", "after-empty"},
		{"echo", "héllo wörld", "日本語"},
		{"echo", "trailing\\"},
	} {
		line := shellJoin(argv)
		// A prologue that prints each argument the shell produced, unambiguously.
		script := `printf '<%s>\n' ` + line
		out, err := exec.Command("/bin/sh", "-c", script).Output()
		if err != nil {
			t.Errorf("%q -> %q: shell refused it: %v", argv, line, err)
			continue
		}
		var got []string
		for _, l := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
			got = append(got, strings.TrimSuffix(strings.TrimPrefix(l, "<"), ">"))
		}
		// argv[0] is the command; the prologue prints every element including it.
		if len(got) != len(argv) {
			t.Errorf("%q -> %q: shell produced %d args %q, want %d",
				argv, line, len(got), got, len(argv))
			continue
		}
		for i := range argv {
			if got[i] != argv[i] {
				t.Errorf("%q -> %q: arg %d became %q, want %q", argv, line, i, got[i], argv[i])
			}
		}
	}
}

// TestShellJoinLeavesPlainArgumentsAlone: the console echoes the line back, so a
// command that needed no protection should still be readable.
func TestShellJoinLeavesPlainArgumentsAlone(t *testing.T) {
	got := shellJoin([]string{"mysql", "-uroot", "--defaults-file=/etc/my.cnf", "-e"})
	want := "mysql -uroot --defaults-file=/etc/my.cnf -e"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if q := shellQuote(""); q != "''" {
		t.Errorf("an empty argument became %q, want '' — it must not vanish", q)
	}
}

// TestShellJoinContainsInjection is the specific case that made this a correctness
// problem and not a formatting one: the metacharacter has to end up inside quotes.
func TestShellJoinContainsInjection(t *testing.T) {
	line := shellJoin([]string{"echo", "a; id -un"})
	if strings.HasSuffix(line, "; id -un") {
		t.Fatalf("the semicolon escaped its argument: %q", line)
	}
	out, err := exec.Command("/bin/sh", "-c", line).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "a; id -un" {
		t.Errorf("the shell ran %q, want the literal text back", got)
	}
}
