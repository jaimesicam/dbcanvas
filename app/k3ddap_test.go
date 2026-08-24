package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// k3ddap_test.go — the DAP client against a fake adapter.
//
// Everything here runs on a local listener with no Delve and no cluster: what is being checked is
// the client's half of the protocol — framing, correlating a response to its request while events
// arrive in between, the attach handshake's order, and the one argument that must never be sent
// with the wrong value (terminateDebuggee).

// fakeDAP is a one-connection DAP server. handle is called for every request with the decoded
// message and a writer for whatever it wants to send back — responses, events, or nothing.
type fakeDAP struct {
	ln   net.Listener
	t    *testing.T
	mu   sync.Mutex
	seen []dapMessage
}

type dapWriter struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func (w *dapWriter) send(v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	fmt.Fprintf(w.w, "Content-Length: %d\r\n\r\n", len(buf))
	w.w.Write(buf)
	w.w.Flush()
}

// respond writes a successful response to req with the given body.
func (w *dapWriter) respond(req dapMessage, body any) {
	w.send(map[string]any{
		"seq": 0, "type": "response", "request_seq": req.Seq,
		"success": true, "command": req.Command, "body": body,
	})
}

func (w *dapWriter) fail(req dapMessage, msg string) {
	w.send(map[string]any{
		"seq": 0, "type": "response", "request_seq": req.Seq,
		"success": false, "command": req.Command, "message": msg,
	})
}

func (w *dapWriter) event(name string, body any) {
	w.send(map[string]any{"seq": 0, "type": "event", "event": name, "body": body})
}

// startFakeDAP listens on loopback and serves one connection with handle.
func startFakeDAP(t *testing.T, handle func(req dapMessage, w *dapWriter)) *fakeDAP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeDAP{ln: ln, t: t}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := &dapWriter{w: bufio.NewWriter(conn)}
		for {
			raw, err := dapReadFrame(r)
			if err != nil {
				return
			}
			var m dapMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return
			}
			f.mu.Lock()
			f.seen = append(f.seen, m)
			f.mu.Unlock()
			handle(m, w)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeDAP) addr() string { return f.ln.Addr().String() }

func (f *fakeDAP) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.seen))
	for _, m := range f.seen {
		out = append(out, m.Command)
	}
	return out
}

func (f *fakeDAP) request(command string) (dapMessage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.seen {
		if m.Command == command {
			return m, true
		}
	}
	return dapMessage{}, false
}

func dialFake(t *testing.T, f *fakeDAP, onEvent func(string, json.RawMessage)) *dapClient {
	t.Helper()
	c, err := dapDial(context.Background(), f.addr(), onEvent)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// The handshake is initialize → attach{mode:remote} → (server: initialized) → breakpoints →
// configurationDone. Order matters: breakpoints sent before `initialized` are refused by a real
// adapter, and configurationDone before them arms nothing.
func TestDAPAttachHandshakeOrder(t *testing.T) {
	f := startFakeDAP(t, func(req dapMessage, w *dapWriter) {
		switch req.Command {
		case "initialize":
			w.respond(req, map[string]any{"supportsConfigurationDoneRequest": true})
		case "attach":
			w.respond(req, nil)
			w.event("initialized", nil) // as Delve does, after the attach is accepted
		case "setBreakpoints":
			w.respond(req, map[string]any{"breakpoints": []dapBreakpoint{{ID: 1, Verified: true, Line: 42}}})
		default:
			w.respond(req, nil)
		}
	})
	c := dialFake(t, f, nil)
	ctx := context.Background()

	if err := c.initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := c.attachRemote(ctx); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := c.waitInitialized(ctx); err != nil {
		t.Fatalf("waitInitialized: %v", err)
	}
	bps, err := c.setBreakpoints(ctx, "/go/src/x/controller.go", []dapSourceBreakpoint{{Line: 42}})
	if err != nil {
		t.Fatalf("setBreakpoints: %v", err)
	}
	if len(bps) != 1 || !bps[0].Verified || bps[0].Line != 42 {
		t.Fatalf("breakpoints = %+v", bps)
	}
	if err := c.configurationDone(ctx); err != nil {
		t.Fatalf("configurationDone: %v", err)
	}

	want := []string{"initialize", "attach", "setBreakpoints", "configurationDone"}
	got := f.commands()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("request order = %v, want %v", got, want)
	}

	// The attach must be the remote mode: any other mode makes Delve start a *new* debuggee
	// instead of adopting the operator that is already running.
	m, _ := f.request("attach")
	var args map[string]any
	json.Unmarshal(m.Arguments, &args)
	if args["mode"] != "remote" {
		t.Fatalf("attach arguments = %v, want mode remote", args)
	}
}

// Two requests in flight at once, answered out of order, with an event in between: each caller
// must get its own response. This is the whole reason the read loop is a demultiplexer.
func TestDAPConcurrentCallsCorrelateBySeq(t *testing.T) {
	var held dapMessage
	var once sync.Once
	release := make(chan struct{})
	f := startFakeDAP(t, func(req dapMessage, w *dapWriter) {
		switch req.Command {
		case "stackTrace":
			// Hold the first one back until the second has been answered.
			once.Do(func() { held = req })
			go func() {
				<-release
				w.respond(req, map[string]any{"stackFrames": []dapStackFrame{{ID: 1, Name: "held"}}})
			}()
		case "threads":
			w.event("output", dapOutputEvent{Category: "stdout", Output: "noise\n"})
			w.respond(req, map[string]any{"threads": []dapThread{{ID: 7, Name: "goroutine 7"}}})
			close(release)
		}
	})
	c := dialFake(t, f, nil)

	type result struct {
		frames []dapStackFrame
		err    error
	}
	done := make(chan result, 1)
	go func() {
		fr, err := c.stackTrace(context.Background(), 1, 20)
		done <- result{fr, err}
	}()

	// Give the first request time to be in flight before the second goes out.
	time.Sleep(20 * time.Millisecond)
	th, err := c.threads(context.Background())
	if err != nil {
		t.Fatalf("threads: %v", err)
	}
	if len(th) != 1 || th[0].ID != 7 {
		t.Fatalf("threads = %+v", th)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("stackTrace: %v", r.err)
	}
	if len(r.frames) != 1 || r.frames[0].Name != "held" {
		t.Fatalf("stackTrace = %+v", r.frames)
	}
	if held.Seq == 0 {
		t.Fatal("the held request was never seen")
	}
}

// A `stopped` event has to reach the session with its body intact — it is what every panel in the
// debugger is drawn from.
func TestDAPEventsReachTheHandler(t *testing.T) {
	f := startFakeDAP(t, func(req dapMessage, w *dapWriter) {
		if req.Command == "continue" {
			w.respond(req, map[string]any{"allThreadsContinued": true})
			w.event("stopped", dapStoppedEvent{Reason: "breakpoint", ThreadID: 42, AllThreadsStopped: true})
		}
	})

	type ev struct {
		name string
		body json.RawMessage
	}
	events := make(chan ev, 4)
	c := dialFake(t, f, func(name string, body json.RawMessage) { events <- ev{name, body} })

	if err := c.cont(context.Background(), 1); err != nil {
		t.Fatalf("continue: %v", err)
	}
	select {
	case e := <-events:
		if e.name != "stopped" {
			t.Fatalf("event = %q, want stopped", e.name)
		}
		var st dapStoppedEvent
		if err := json.Unmarshal(e.body, &st); err != nil {
			t.Fatalf("decode stopped: %v", err)
		}
		if st.Reason != "breakpoint" || st.ThreadID != 42 {
			t.Fatalf("stopped = %+v", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no stopped event")
	}
}

// Delve's own error text is the useful part of a failed request ("could not find symbol value for
// x"), so it must come back verbatim rather than wrapped in a sentence of ours.
func TestDAPFailedRequestKeepsTheServerMessage(t *testing.T) {
	f := startFakeDAP(t, func(req dapMessage, w *dapWriter) {
		w.fail(req, "could not find symbol value for nope")
	})
	c := dialFake(t, f, nil)

	_, err := c.evaluate(context.Background(), "nope", 3, dapEvalFull)
	if err == nil || err.Error() != "could not find symbol value for nope" {
		t.Fatalf("err = %v, want the server's own message", err)
	}
}

// terminateDebuggee:true kills dlv, which is the operator pod's process — the pod restarts and the
// cluster loses its operator. This test is the guard on that: disconnect must always say false.
func TestDAPDisconnectNeverTerminatesTheDebuggee(t *testing.T) {
	f := startFakeDAP(t, func(req dapMessage, w *dapWriter) { w.respond(req, nil) })
	c := dialFake(t, f, nil)

	if err := c.dapDisconnect(context.Background()); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	m, ok := f.request("disconnect")
	if !ok {
		t.Fatal("no disconnect request")
	}
	var args map[string]any
	if err := json.Unmarshal(m.Arguments, &args); err != nil {
		t.Fatalf("decode arguments: %v", err)
	}
	if args["terminateDebuggee"] != false {
		t.Fatalf("terminateDebuggee = %v, want false", args["terminateDebuggee"])
	}
	if args["restart"] != false {
		t.Fatalf("restart = %v, want false", args["restart"])
	}
}

// A connection that ends must unblock every caller waiting on it, rather than leaving a request
// hanging until its timeout — a debugger pod that restarts should surface as an error at once.
func TestDAPClosedConnectionUnblocksCallers(t *testing.T) {
	// The fake answers nothing at all, so the only way out is the connection ending.
	f := startFakeDAP(t, func(req dapMessage, w *dapWriter) {})
	c := dialFake(t, f, nil)
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.conn.Close()
	}()

	if _, err := c.threads(context.Background()); err == nil {
		t.Fatal("want an error when the connection ends")
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("Done() never closed")
	}
	// And a call made afterwards fails immediately rather than dialling into the void.
	if _, err := c.threads(context.Background()); err == nil {
		t.Fatal("want an error after the connection ended")
	}
}

// The framing parser: headers are case-insensitive, extra headers are ignored, and a message
// without a length (or with an absurd one) is rejected rather than trusted.
func TestDAPReadFrame(t *testing.T) {
	body := `{"seq":1,"type":"event","event":"initialized"}`
	raw := "content-length: " + fmt.Sprint(len(body)) + "\r\nContent-Type: application/vnd.dap+json\r\n\r\n" + body
	got, err := dapReadFrame(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q", got)
	}

	if _, err := dapReadFrame(bufio.NewReader(strings.NewReader("X: 1\r\n\r\n{}"))); err == nil {
		t.Fatal("want an error with no Content-Length")
	}
	if _, err := dapReadFrame(bufio.NewReader(strings.NewReader("Content-Length: 99999999999\r\n\r\n"))); err == nil {
		t.Fatal("want an error for an oversized message")
	}
	if _, err := dapReadFrame(bufio.NewReader(strings.NewReader("Content-Length: nope\r\n\r\n"))); err == nil {
		t.Fatal("want an error for a malformed Content-Length")
	}
}

// setBreakpoints with no lines is how a file's breakpoints are cleared, and it has to send an
// empty JSON array — `null` is a different request as far as an adapter is concerned.
func TestDAPClearBreakpointsSendsAnEmptyList(t *testing.T) {
	f := startFakeDAP(t, func(req dapMessage, w *dapWriter) {
		w.respond(req, map[string]any{"breakpoints": []dapBreakpoint{}})
	})
	c := dialFake(t, f, nil)

	if _, err := c.setBreakpoints(context.Background(), "/go/src/x/a.go", nil); err != nil {
		t.Fatalf("setBreakpoints: %v", err)
	}
	if _, err := c.setFunctionBreakpoints(context.Background(), nil); err != nil {
		t.Fatalf("setFunctionBreakpoints: %v", err)
	}
	for _, cmd := range []string{"setBreakpoints", "setFunctionBreakpoints"} {
		m, ok := f.request(cmd)
		if !ok {
			t.Fatalf("no %s request", cmd)
		}
		if !strings.Contains(string(m.Arguments), `"breakpoints":[]`) {
			t.Fatalf("%s arguments = %s, want an empty array", cmd, m.Arguments)
		}
	}
}

// The evaluation context is not a hint with Delve, it is how much of the value you get: the same
// receiver comes back summarised under "watch" and whole under "variables". Anything that reads a
// value for a person to look at must therefore ask for the full one.
func TestDAPEvaluateSendsTheRequestedContext(t *testing.T) {
	f := startFakeDAP(t, func(req dapMessage, w *dapWriter) {
		w.respond(req, map[string]any{"result": "ok", "type": "string"})
	})
	c := dialFake(t, f, nil)

	if _, err := c.evaluate(context.Background(), "x", 7, dapEvalFull); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	m, _ := f.request("evaluate")
	var args map[string]any
	json.Unmarshal(m.Arguments, &args)
	if args["context"] != "variables" {
		t.Fatalf("context = %v, want variables", args["context"])
	}
	if args["frameId"] != float64(7) {
		t.Fatalf("frameId = %v", args["frameId"])
	}
}

// setVariable addresses a variable by its container and name, which is the only shape Delve
// accepts — there is no setExpression on this server ("Not yet implemented").
func TestDAPSetVariable(t *testing.T) {
	f := startFakeDAP(t, func(req dapMessage, w *dapWriter) {
		w.respond(req, map[string]any{"value": `"probe"`, "type": "string"})
	})
	c := dialFake(t, f, nil)

	v, err := c.setVariable(context.Background(), 1003, "Name", `"probe"`)
	if err != nil {
		t.Fatalf("setVariable: %v", err)
	}
	if v.Value != `"probe"` || v.Type != "string" {
		t.Fatalf("result = %+v", v)
	}
	m, _ := f.request("setVariable")
	var args map[string]any
	json.Unmarshal(m.Arguments, &args)
	if args["variablesReference"] != float64(1003) || args["name"] != "Name" {
		t.Fatalf("arguments = %v", args)
	}
}
