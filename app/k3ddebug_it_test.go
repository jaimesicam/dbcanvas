package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// k3ddebug_it_test.go — the DAP client against a REAL operator running under Delve.
//
// The unit tests prove the client's half of the protocol against a fake adapter. What they
// cannot prove is what Delve actually does, and two of those answers shaped the session:
// whether attaching to a `--continue`d headless instance leaves the debuggee running, and
// whether the quick-breakpoint specs resolve to real functions in the shipped binary.
//
// Opt-in, because it needs a deployed K3D frame with the debugger on:
//
//	DELVE_IT=127.0.0.1:40000 go test -run TestDelveLive -v .
//
// (that address is the frame's published debugger port; from inside the app it would be the
// k3s node's own address on :30400 instead). The test halts the operator it attaches to and
// always clears up after itself — the same clear-then-resume the session's teardown does.
//
// DELVE_IT_OPERATOR picks which operator is on the other end (pxc, ps, psmdb, pg; pxc by
// default) — the presets, the build directory and the file to break in all come from that
// operator's own entries, so the one thing this test exists to prove, that the quick
// breakpoints resolve against the shipped binary, can be proved for each of them.
func TestDelveLiveSession(t *testing.T) {
	addr := os.Getenv("DELVE_IT")
	if addr == "" {
		t.Skip("integration test; set DELVE_IT=<host:port> to run against a real Delve")
	}
	op := os.Getenv("DELVE_IT_OPERATOR")
	if op == "" {
		op = "pxc"
	}
	if !k3dDebuggableOperator(op) {
		t.Fatalf("DELVE_IT_OPERATOR=%q is not an operator the debugger is wired up for", op)
	}
	buildDir := k3dDebugBuildDir(op)
	controller := k3dDebugStartFile[op]
	t.Logf("operator %s — source under %s, breaking in %s", op, buildDir, controller)

	ctx := context.Background()
	var mu sync.Mutex
	stops := make(chan dapStoppedEvent, 8)
	var events []string
	cli, err := dapDial(ctx, addr, func(name string, body json.RawMessage) {
		mu.Lock()
		events = append(events, name)
		mu.Unlock()
		if name == "stopped" {
			var st dapStoppedEvent
			json.Unmarshal(body, &st)
			select {
			case stops <- st:
			default:
			}
		}
	})
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer cli.Close()

	if err := cli.initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := cli.attachRemote(ctx); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := cli.waitInitialized(ctx); err != nil {
		t.Fatalf("initialized: %v", err)
	}
	t.Log("attached")

	// Whatever the attach did to the debuggee, the session's contract is that the operator is
	// running when the handshake ends. Record what actually happened — this is the behaviour
	// the session's belt-and-braces continue exists for.
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	t.Logf("events during the handshake: %v", events)
	mu.Unlock()

	// The quick breakpoints, as the panel sends them. A preset that does not resolve is a
	// preset that lies, so this is the test that keeps them honest.
	presets := k3dDebugPresets[op]
	fns := make([]dapFunctionBreakpoint, 0, len(presets))
	for _, p := range presets {
		fns = append(fns, dapFunctionBreakpoint{Name: p.Func})
	}
	// The slowest call in the test, and the only one that needs its own deadline: Delve halts a
	// running debuggee and resolves every spec against a ~100 MiB unstripped binary, which on a
	// busy box (an operator mid-cluster-creation, a 2-CPU limit) runs past the client's default
	// 30s. Timing out here reads as "the presets do not resolve", which is the opposite of true.
	bpCtx, cancelBP := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelBP()
	got, err := cli.setFunctionBreakpoints(bpCtx, fns)
	if err != nil {
		t.Fatalf("setFunctionBreakpoints: %v", err)
	}
	// Where Reconcile turned out to be, which is where the source breakpoint below goes: the
	// line is per operator and per release, and Delve has just told us it.
	reconcileLine := 0
	for i, bp := range got {
		name := presets[i].Func
		t.Logf("preset %-70s verified=%v line=%d %s", name, bp.Verified, bp.Line, bp.Message)
		if !bp.Verified {
			t.Errorf("preset %q did not resolve: %s", name, bp.Message)
			continue
		}
		if presets[i].Label == "Reconcile" {
			reconcileLine = bp.Line
		}
	}
	if reconcileLine == 0 {
		t.Fatalf("the Reconcile preset for %s did not resolve, so there is nowhere to break", op)
	}
	// Clear them again: the stop this test wants is the source breakpoint below, and a
	// function breakpoint on Reconcile would race it.
	if _, err := cli.setFunctionBreakpoints(ctx, nil); err != nil {
		t.Fatalf("clear function breakpoints: %v", err)
	}

	// A source breakpoint on the line the panel would set it on — through the same path
	// mapping the session uses, which is the thing an IDE needs substitutePath for.
	line := 0
	for _, want := range []int{reconcileLine, reconcileLine + 1, reconcileLine + 2, reconcileLine + 3} {
		bps, err := cli.setBreakpoints(ctx, k3dDebugBuildPath(buildDir, controller),
			[]dapSourceBreakpoint{{Line: want}})
		if err != nil {
			t.Fatalf("setBreakpoints: %v", err)
		}
		if len(bps) == 1 && bps[0].Verified {
			line = bps[0].Line
			t.Logf("breakpoint at %s:%d (asked for %d)", controller, line, want)
			break
		}
		cli.setBreakpoints(ctx, k3dDebugBuildPath(buildDir, controller), nil)
	}
	if line == 0 {
		t.Fatal("no breakpoint could be set in Reconcile")
	}

	// Always leave the operator as it was found: nothing armed, and running.
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cli.setBreakpoints(cctx, k3dDebugBuildPath(buildDir, controller), nil)
		cli.setFunctionBreakpoints(cctx, nil)
		cli.cont(cctx, 1)
		if err := cli.dapDisconnect(cctx); err != nil {
			t.Logf("disconnect: %v", err)
		}
		t.Log("cleared the breakpoints and resumed the operator")
	}()

	if err := cli.configurationDone(ctx); err != nil {
		t.Logf("configurationDone: %v", err)
	}
	// The same belt-and-braces continue the session sends.
	if err := cli.cont(ctx, 1); err != nil {
		t.Logf("continue after the handshake: %v (expected when it is already running)", err)
	}

	// PXC and MongoDB requeue every five seconds, so Reconcile comes round on its own and the
	// stop below needs nothing from anyone. Percona Server and PostgreSQL only reconcile when
	// something changes, so on a settled cluster this waits forever unless the custom resource
	// is touched — which is what the page's "Force a reconcile" button does.
	var stop dapStoppedEvent
	select {
	case stop = <-stops:
	case <-time.After(90 * time.Second):
		t.Fatalf("the operator never hit the breakpoint in Reconcile. %s only reconciles when "+
			"something changes; run this again with `kubectl -n <ns> annotate %s <cr> "+
			"debug=$(date +%%s) --overwrite` in a loop beside it", k3dOperatorLabel(op),
			k3dDebugCRKind[op])
	}
	t.Logf("stopped: reason=%q thread=%d", stop.Reason, stop.ThreadID)

	frames, err := cli.stackTrace(ctx, stop.ThreadID, k3dDebugStackDepth)
	if err != nil {
		t.Fatalf("stackTrace: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("an empty stack")
	}
	mapped := k3dDebugMapFrames(buildDir, frames)
	for i, f := range mapped {
		if i > 5 {
			break
		}
		t.Logf("frame %d: %s at %s:%d (source: %v)", i, f.Name, f.File, f.Line, f.HasSource)
	}
	if !mapped[0].HasSource || mapped[0].File != controller {
		t.Errorf("top frame = %+v, want %s with source", mapped[0], controller)
	}
	if !strings.Contains(mapped[0].Name, "Reconcile") {
		t.Errorf("top frame is %q, want Reconcile", mapped[0].Name)
	}
	// The frames below are controller-runtime's, from the module cache: they must survive
	// the mapping without pretending to have source the node does not have.
	deps := 0
	for _, f := range mapped {
		if !f.HasSource {
			deps++
		}
	}
	t.Logf("%d of %d frames have no source on the node (dependencies)", deps, len(mapped))

	// Scopes and variables, as the panel's right-hand column asks for them.
	scopes, err := cli.scopes(ctx, frames[0].ID)
	if err != nil {
		t.Fatalf("scopes: %v", err)
	}
	if len(scopes) == 0 {
		t.Fatal("no scopes in the top frame")
	}
	found := 0
	for _, sc := range scopes {
		vars, err := cli.variables(ctx, sc.VariablesReference)
		if err != nil {
			t.Errorf("variables(%s): %v", sc.Name, err)
			continue
		}
		t.Logf("scope %s: %d variable(s)", sc.Name, len(vars))
		for _, v := range vars {
			if v.Name == "request" || v.Name == "r" || v.Name == "ctx" {
				found++
				t.Logf("  %s = %s (%s)", v.Name, truncate(v.Value, 90), v.Type)
			}
		}
	}
	if found == 0 {
		t.Error("neither the receiver nor the request is visible in Reconcile's scopes")
	}

	// Evaluating in a frame — the thing a println would otherwise be for.
	if v, err := cli.evaluate(ctx, "request.NamespacedName", frames[0].ID, dapEvalFull); err != nil {
		t.Errorf("evaluate: %v", err)
	} else {
		t.Logf("request.NamespacedName = %s", truncate(v.Value, 120))
	}

	// Stepping: one `next` must produce another stop, at a different line in the same frame.
	if err := cli.next(ctx, stop.ThreadID); err != nil {
		t.Fatalf("next: %v", err)
	}
	select {
	case s2 := <-stops:
		fr2, err := cli.stackTrace(ctx, s2.ThreadID, 3)
		if err != nil {
			t.Fatalf("stackTrace after next: %v", err)
		}
		t.Logf("after next: reason=%q at %s:%d", s2.Reason, controller, fr2[0].Line)
		if fr2[0].Line == frames[0].Line {
			t.Errorf("next did not move: still line %d", fr2[0].Line)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no stop after next")
	}
}

// TestDebuggerAPILive drives the whole feature through its own API, the way the page does:
// log in, find the target, read the source, open the session socket, set a breakpoint, wait
// for the operator to hit it, look at a variable, continue, and detach.
//
// It needs a running DBCanvas with a deployed K3D frame whose debugger is listening:
//
//	DBCANVAS_IT=http://127.0.0.1:8080 DBCANVAS_IT_USER=admin DBCANVAS_IT_PASS=… \
//	  go test -run TestDebuggerAPILive -v .
func TestDebuggerAPILive(t *testing.T) {
	base := os.Getenv("DBCANVAS_IT")
	if base == "" {
		t.Skip("integration test; set DBCANVAS_IT=<url> (plus DBCANVAS_IT_USER/PASS) to run against a running app")
	}
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 30 * time.Second}

	login, _ := json.Marshal(map[string]string{
		"username": os.Getenv("DBCANVAS_IT_USER"), "password": os.Getenv("DBCANVAS_IT_PASS")})
	res, err := hc.Post(base+"/api/auth/login", "application/json", bytes.NewReader(login))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("login: status %d", res.StatusCode)
	}

	// ---- what is there to debug
	var targets struct {
		Targets []struct {
			k3dDebugTarget
			StartFile string           `json:"startFile"`
			Presets   []k3dDebugPreset `json:"presets"`
		} `json:"targets"`
	}
	getJSON(t, hc, base+"/api/k3d/debug/targets", &targets)
	if len(targets.Targets) == 0 {
		t.Fatal("no debuggable frames — deploy a K3D frame with the operator under Delve first")
	}
	tgt := targets.Targets[0]
	t.Logf("target: stack %d frame %s (%s %s)", tgt.StackID, tgt.FrameID, tgt.Operator, tgt.OperatorVer)
	if tgt.StartFile == "" || len(tgt.Presets) == 0 {
		t.Fatalf("target has no start file or presets: %+v", tgt)
	}
	frameBase := fmt.Sprintf("%s/api/stacks/%d/frames/%s/k3d/debug", base, tgt.StackID, tgt.FrameID)

	// ---- the operator's own source, off the k3s node
	var sources struct {
		Files []string `json:"files"`
	}
	getJSON(t, hc, frameBase+"/sources", &sources)
	t.Logf("%d Go files in the operator source", len(sources.Files))
	if !slices.Contains(sources.Files, tgt.StartFile) {
		t.Fatalf("the start file %s is not in the source listing", tgt.StartFile)
	}
	var file struct {
		Path      string `json:"path"`
		BuildPath string `json:"buildPath"`
		Content   string `json:"content"`
	}
	getJSON(t, hc, frameBase+"/source?path="+url.QueryEscape(tgt.StartFile), &file)
	if !strings.Contains(file.Content, ") Reconcile(") {
		t.Fatalf("%s does not look like the controller (%d bytes)", tgt.StartFile, len(file.Content))
	}
	// The line the panel would break on: the first statement inside Reconcile. Delve refuses a
	// line with no statement on it ("please use a line with a statement"), which is exactly
	// what the panel surfaces as an unverified breakpoint — so pick a line that has one.
	line := 0
	src := strings.Split(file.Content, "\n")
	for i, ln := range src {
		if !strings.Contains(ln, ") Reconcile(ctx context.Context") {
			continue
		}
		for j := i + 1; j < len(src) && j < i+12; j++ {
			t := strings.TrimSpace(src[j])
			if t != "" && !strings.HasPrefix(t, "//") && !strings.HasPrefix(t, "}") {
				line = j + 1
				break
			}
		}
		break
	}
	if line == 0 {
		t.Fatal("could not find a statement in Reconcile")
	}
	t.Logf("%s → %s, breaking at line %d: %s", file.Path, file.BuildPath, line, strings.TrimSpace(src[line-1]))

	// ---- the session socket
	wsURL := strings.Replace(frameBase, "http", "ws", 1) + "/ws"
	// A client with no Timeout: coder/websocket turns HTTPClient.Timeout into a deadline on the
	// whole connection, so the 30s one above would hang up on the session mid-session.
	wsClient := &http.Client{Jar: jar}
	c, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPClient: wsClient})
	if err != nil {
		t.Fatalf("dial the session socket: %v", err)
	}
	defer c.CloseNow()

	states := make(chan k3dDebugState, 64)
	replies := make(chan map[string]any, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var msg struct {
				Type  string          `json:"type"`
				State k3dDebugState   `json:"state"`
				Line  k3dDebugLogLine `json:"line"`
				OK    bool            `json:"ok"`
				Error string          `json:"error"`
				Data  map[string]any  `json:"data"`
			}
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			switch msg.Type {
			case "state":
				select {
				case states <- msg.State:
				default:
				}
			case "log":
				t.Logf("log[%s] %s", msg.Line.Kind, msg.Line.Text)
			case "reply":
				select {
				case replies <- map[string]any{"ok": msg.OK, "error": msg.Error, "data": msg.Data}:
				default:
				}
			}
		}
	}()
	send := func(v any) {
		buf, _ := json.Marshal(v)
		if err := c.Write(ctx, websocket.MessageText, buf); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	await := func(what string, timeout time.Duration, ok func(k3dDebugState) bool) k3dDebugState {
		t.Helper()
		deadline := time.After(timeout)
		for {
			select {
			case st := <-states:
				if ok(st) {
					return st
				}
			case <-deadline:
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	// Attaching happens on connect.
	await("the session to attach", 60*time.Second, func(st k3dDebugState) bool { return st.Status == "running" })
	t.Log("attached, the operator is running")

	// Leave nothing behind, whatever happens next.
	defer func() {
		send(map[string]any{"cmd": "clearBreakpoints"})
		send(map[string]any{"cmd": "detach"})
		time.Sleep(2 * time.Second)
	}()

	send(map[string]any{"cmd": "breakpoint", "file": tgt.StartFile, "line": line, "on": true})
	st := await("the breakpoint to be armed", 30*time.Second, func(st k3dDebugState) bool {
		return len(st.Breakpoints) == 1
	})
	if !st.Breakpoints[0].Verified {
		t.Fatalf("breakpoint not verified: %+v", st.Breakpoints[0])
	}

	// A forced reconcile — the button no IDE has. (The operator also requeues every five
	// seconds, so this proves the annotation works rather than making the stop happen.)
	send(map[string]any{"cmd": "reconcile", "id": 1})
	select {
	case r := <-replies:
		if r["ok"] != true {
			t.Errorf("reconcile: %v", r["error"])
		}
	case <-time.After(30 * time.Second):
		t.Error("no reply to reconcile")
	}

	st = await("the operator to stop", 60*time.Second, func(st k3dDebugState) bool { return st.Status == "stopped" })
	if len(st.Frames) == 0 || !st.Frames[0].HasSource {
		t.Fatalf("stopped with no usable stack: %+v", st.Frames)
	}
	t.Logf("stopped in %s at %s:%d", st.Frames[0].Name, st.Frames[0].File, st.Frames[0].Line)

	// The panel's right-hand column: scopes, and an expression.
	send(map[string]any{"cmd": "scopes", "frameId": st.Frames[0].ID, "id": 2})
	if r := <-replies; r["ok"] != true {
		t.Fatalf("scopes: %v", r["error"])
	}
	send(map[string]any{"cmd": "evaluate", "expr": "request.NamespacedName", "frameId": st.Frames[0].ID, "id": 3})
	select {
	case r := <-replies:
		if r["ok"] != true {
			t.Errorf("evaluate: %v", r["error"])
		} else {
			t.Logf("evaluate → %v", r["data"])
		}
	case <-time.After(30 * time.Second):
		t.Error("no reply to evaluate")
	}
	// ...and a call is refused until it is allowed, because it would run in the operator.
	send(map[string]any{"cmd": "evaluate", "expr": "r.client.Get(ctx, request.NamespacedName, o)", "frameId": st.Frames[0].ID, "id": 4})
	select {
	case r := <-replies:
		if r["ok"] == true {
			t.Error("a function call should be refused until it is allowed")
		} else {
			t.Logf("a call was refused: %v", r["error"])
		}
	case <-time.After(30 * time.Second):
		t.Error("no reply to the call expression")
	}

	send(map[string]any{"cmd": "continue"})
	await("the operator to resume", 30*time.Second, func(st k3dDebugState) bool { return st.Status == "running" })
	t.Log("resumed")
}

func getJSON(t *testing.T, hc *http.Client, url string, out any) {
	t.Helper()
	res, err := hc.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", url, res.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// TestDebuggerLiveTeardownResumes is the safety property, against a real cluster: close the
// page while the operator is stopped at a breakpoint and the operator must end up running
// again with nothing armed. A breakpoint that outlives its session freezes the operator
// silently — no probe fails, nothing is logged — and that is the failure this whole feature
// has to be trusted not to cause.
//
//	DBCANVAS_IT=http://127.0.0.1:8080 DBCANVAS_IT_USER=admin DBCANVAS_IT_PASS=… \
//	  go test -run TestDebuggerLiveTeardown -v .
func TestDebuggerLiveTeardownResumes(t *testing.T) {
	base := os.Getenv("DBCANVAS_IT")
	if base == "" {
		t.Skip("integration test; set DBCANVAS_IT=<url> (plus DBCANVAS_IT_USER/PASS)")
	}
	hc, tgt := debugLiveTarget(t, base)
	wsURL := fmt.Sprintf("%s/api/stacks/%d/frames/%s/k3d/debug/ws",
		strings.Replace(base, "http", "ws", 1), tgt.StackID, tgt.FrameID)

	// ---- session one: stop the operator, then vanish.
	c1, states1, cancel1 := debugLiveSocket(t, hc, wsURL)
	awaitState(t, states1, "attached", 90*time.Second, func(st k3dDebugState) bool { return st.Status == "running" })

	fn := k3dDebugPresets[tgt.Operator][0].Func
	sendWS(t, c1, map[string]any{"cmd": "fnbreakpoint", "name": fn, "on": true})
	awaitState(t, states1, "the quick breakpoint to arm", 30*time.Second, func(st k3dDebugState) bool {
		return len(st.Functions) == 1 && st.Functions[0].Verified
	})
	awaitState(t, states1, "the operator to stop", 60*time.Second, func(st k3dDebugState) bool {
		return st.Status == "stopped"
	})
	t.Log("the operator is stopped at Reconcile; hanging up without detaching")
	c1.CloseNow()
	cancel1()

	// ---- session two: whatever the server did, the operator must be running.
	time.Sleep(3 * time.Second)
	c2, states2, cancel2 := debugLiveSocket(t, hc, wsURL)
	defer func() {
		sendWS(t, c2, map[string]any{"cmd": "clearBreakpoints"})
		sendWS(t, c2, map[string]any{"cmd": "detach"})
		time.Sleep(2 * time.Second)
		c2.CloseNow()
		cancel2()
	}()
	st := awaitState(t, states2, "the operator to be running again", 90*time.Second,
		func(st k3dDebugState) bool { return st.Status == "running" })
	t.Logf("the operator is running again; the session still remembers %d function breakpoint(s) to re-arm",
		len(st.Functions))
}

// debugLiveTarget logs in and returns the first debuggable frame.
func debugLiveTarget(t *testing.T, base string) (*http.Client, k3dDebugTarget) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar}
	login, _ := json.Marshal(map[string]string{
		"username": os.Getenv("DBCANVAS_IT_USER"), "password": os.Getenv("DBCANVAS_IT_PASS")})
	res, err := hc.Post(base+"/api/auth/login", "application/json", bytes.NewReader(login))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	res.Body.Close()
	var targets struct {
		Targets []k3dDebugTarget `json:"targets"`
	}
	getJSON(t, hc, base+"/api/k3d/debug/targets", &targets)
	if len(targets.Targets) == 0 {
		t.Fatal("no debuggable frames")
	}
	return hc, targets.Targets[0]
}

// debugLiveSocket opens a session socket and streams its states.
func debugLiveSocket(t *testing.T, hc *http.Client, wsURL string) (*websocket.Conn, chan k3dDebugState, context.CancelFunc) {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	states := make(chan k3dDebugState, 64)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var msg struct {
				Type  string          `json:"type"`
				State k3dDebugState   `json:"state"`
				Line  k3dDebugLogLine `json:"line"`
			}
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			if msg.Type == "state" {
				select {
				case states <- msg.State:
				default:
				}
			} else if msg.Type == "log" {
				t.Logf("log[%s] %s", msg.Line.Kind, msg.Line.Text)
			}
		}
	}()
	return c, states, cancel
}

func sendWS(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	buf, _ := json.Marshal(v)
	if err := c.Write(context.Background(), websocket.MessageText, buf); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func awaitState(t *testing.T, states chan k3dDebugState, what string, timeout time.Duration, ok func(k3dDebugState) bool) k3dDebugState {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case st := <-states:
			if ok(st) {
				return st
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
