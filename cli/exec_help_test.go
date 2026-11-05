package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// exec_help_test.go — node exec through POST …/exec, the -d alias, and help.

// nodeExecServer is a DBCanvas with one stack "lab" and one node "ps-01", whose exec
// endpoint answers with reply (or, when reply is nil, with the web app's page, as a server
// older than the endpoint does).
func nodeExecServer(t *testing.T, reply map[string]any) *testServer {
	t.Helper()
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/stacks":
			json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "name": "lab"}})
		case r.URL.Path == "/api/stacks/1":
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "name": "lab",
				"design": map[string]any{"nodes": []map[string]any{{"id": "n-ps", "label": "ps-01", "type": "ps"}}}})
		case strings.HasSuffix(r.URL.Path, "/exec"):
			if reply == nil {
				w.Header().Set("Content-Type", "text/html")
				w.Write([]byte("<!doctype html><html></html>"))
				return
			}
			json.NewEncoder(w).Encode(reply)
		default:
			http.NotFound(w, r)
		}
	})
	t.Setenv("DBCANVAS_URL", ts.URL)
	t.Setenv("DBCANVAS_TOKEN", "dbc_x")
	g = globals{}
	return ts
}

// captureStd runs fn with stdout and stderr redirected, and returns what each received.
func captureStd(t *testing.T, fn func()) (string, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	os.Stdout, os.Stderr = wo, we
	outc, errc := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(ro); outc <- string(b) }()
	go func() { b, _ := io.ReadAll(re); errc <- string(b) }()
	fn()
	wo.Close()
	we.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	return <-outc, <-errc
}

func TestNodeExecUsesTheExecEndpoint(t *testing.T) {
	isolateConfig(t)
	ts := nodeExecServer(t, map[string]any{"exitCode": 3, "stdout": "out\n", "stderr": "err\n"})
	var err error
	stdout, stderr := captureStd(t, func() {
		err = nodeExec([]string{"lab", "ps-01", "--timeout", "90s", "--", "mysql", "-e", "SELECT 'a b'; -- x"})
	})
	// The command's own status, its own output, and nothing of the CLI's.
	var rx *remoteExit
	if !errors.As(err, &rx) || rx.code != 3 || exitCodeFor(err) != 3 {
		t.Fatalf("err = %v, want the remote status 3", err)
	}
	if stdout != "out\n" || stderr != "err\n" {
		t.Errorf("stdout %q stderr %q", stdout, stderr)
	}
	// argv arrives as given — no shell joining or quoting on the way.
	last := ts.bodies[len(ts.bodies)-1]
	var body struct {
		Argv    []string `json:"argv"`
		Timeout int      `json:"timeout"`
		Stdin   *string  `json:"stdin"`
	}
	if err := json.Unmarshal([]byte(last), &body); err != nil {
		t.Fatal(err)
	}
	if strings.Join(body.Argv, "|") != "mysql|-e|SELECT 'a b'; -- x" || body.Timeout != 90 || body.Stdin != nil {
		t.Errorf("sent %s", last)
	}
	if got := ts.requests[len(ts.requests)-1]; got != "POST /api/stacks/1/nodes/n-ps/exec" {
		t.Errorf("last request %q", got)
	}
}

func TestNodeExecSuccessAndBinaryOutput(t *testing.T) {
	isolateConfig(t)
	bin := []byte{0x1f, 0x8b, 0x00, 0xff}
	nodeExecServer(t, map[string]any{"exitCode": 0, "stdoutBase64": base64.StdEncoding.EncodeToString(bin)})
	var err error
	stdout, _ := captureStd(t, func() { err = nodeExec([]string{"lab", "ps-01", "--", "cat", "/x.gz"}) })
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(bin) {
		t.Errorf("stdout %q, want the bytes back", stdout)
	}
}

func TestNodeExecStdinIsSentOnlyWhenAsked(t *testing.T) {
	isolateConfig(t)
	ts := nodeExecServer(t, map[string]any{"exitCode": 0})
	withStdin(t, "SELECT 1;\n")
	captureStd(t, func() { nodeExec([]string{"lab", "ps-01", "--stdin", "--", "mysql"}) })
	if !strings.Contains(ts.bodies[len(ts.bodies)-1], `"stdin":"SELECT 1;\n"`) {
		t.Errorf("sent %s", ts.bodies[len(ts.bodies)-1])
	}
}

// A server older than the endpoint answers it with the web app's page; exec then goes
// through the console as before (here the console fails, which is all this checks: that it
// was tried, and that --stdin is refused rather than silently dropped).
func TestNodeExecFallsBackOnAnOlderServer(t *testing.T) {
	isolateConfig(t)
	ts := nodeExecServer(t, nil)
	var err error
	_, stderr := captureStd(t, func() { err = nodeExec([]string{"lab", "ps-01", "--", "true"}) })
	if !strings.Contains(stderr, "no exec endpoint") {
		t.Errorf("no fallback note: %q", stderr)
	}
	tried := false
	for _, r := range ts.requests {
		tried = tried || strings.HasSuffix(r, "/term")
	}
	if !tried {
		t.Errorf("the console was not tried: %v (err %v)", ts.requests, err)
	}
	withStdin(t, "x")
	if err := nodeExec([]string{"lab", "ps-01", "--stdin", "--", "cat"}); err == nil || !strings.Contains(err.Error(), "older") {
		t.Errorf("--stdin on an older server: %v", err)
	}
}

func TestAPIAcceptsDashD(t *testing.T) {
	isolateConfig(t)
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	t.Setenv("DBCANVAS_URL", ts.URL)
	t.Setenv("DBCANVAS_TOKEN", "dbc_x")
	g = globals{}
	captureStd(t, func() {
		if err := cmdAPI([]string{"POST", "/api/stacks/1/design/nodes", "-d", `{"type":"pmm"}`}); err != nil {
			t.Error(err)
		}
	})
	if ts.bodies[0] != `{"type":"pmm"}` || ts.requests[0] != "POST /api/stacks/1/design/nodes" {
		t.Errorf("sent %v %v", ts.requests, ts.bodies)
	}
}

// -h on every command and every subcommand prints its usage, succeeds, and sends nothing:
// every command parses its flags before it does anything. `help <command> <sub>` is the same.
func TestHelpEverywhereAndNothingRuns(t *testing.T) {
	isolateConfig(t)
	ts := newTestServer(t, func(s *testServer, w http.ResponseWriter, r *http.Request) {
		t.Errorf("-h made a request: %s %s", r.Method, r.URL.Path)
	})
	t.Setenv("DBCANVAS_URL", ts.URL)
	t.Setenv("DBCANVAS_TOKEN", "dbc_x")
	g = globals{}
	for _, c := range commands {
		if c.name == "version" || c.name == "help" {
			continue
		}
		var err error
		_, stderr := captureStd(t, func() { err = c.run([]string{"-h"}) })
		if !errors.Is(err, errHelp) || exitCodeFor(err) != 0 {
			t.Errorf("%s -h: %v", c.name, err)
		}
		// A group lists its subcommands; each of those gets -h too, directly and through help.
		_, subs, isGroup := strings.Cut(stderr, "Subcommands:\n")
		if !isGroup {
			if !strings.Contains(stderr, "Usage: dbcanvas-cli "+c.name) {
				t.Errorf("%s -h printed %q", c.name, stderr)
			}
			continue
		}
		for _, line := range strings.Split(subs, "\n") {
			f := strings.Fields(line)
			if len(f) == 0 || !strings.HasPrefix(line, "  ") {
				break
			}
			name := c.name + " " + f[0]
			if helpText[name].summary == "" {
				t.Errorf("%s has no help text", name)
			}
			for _, args := range [][]string{{f[0], "-h"}, {f[0], "--help"}} {
				var err error
				_, out := captureStd(t, func() { err = c.run(args) })
				if !errors.Is(err, errHelp) || !strings.Contains(out, "Usage: dbcanvas-cli "+name) {
					t.Errorf("%s %v: %v, printed %q", c.name, args, err, out)
				}
			}
			var err error
			_, out := captureStd(t, func() { err = cmdHelpCmd([]string{c.name, f[0]}) })
			if !errors.Is(err, errHelp) || !strings.Contains(out, "Usage: dbcanvas-cli "+name) {
				t.Errorf("help %s: %v, printed %q", name, err, out)
			}
		}
	}
	// node exec's usage shows its arguments and its flags.
	_, out := captureStd(t, func() { cmdHelpCmd([]string{"node", "exec"}) })
	for _, want := range []string{"<stack> <node> -- <command>", "-stdin", "-timeout", "exits with the command's own status"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("node exec help lacks %q:\n%s", want, out)
		}
	}
}
