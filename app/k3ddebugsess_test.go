package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// k3ddebugsess_test.go — the session's rules, against the fake adapter from k3ddap_test.go.
//
// The interesting assertions are all about the same thing: the debugger must never leave the
// operator stopped or a breakpoint armed with nobody watching, because either one silently stops
// the cluster being reconciled.

// newTestSession wires a session to a fake adapter and attaches it. The target's BuildDir is the
// prefix the operator's binary was compiled with, which is what the path mapping turns on.
func newTestSession(t *testing.T, handle func(req dapMessage, w *dapWriter)) (*k3dDebugSession, *fakeDAP) {
	t.Helper()
	f := startFakeDAP(t, func(req dapMessage, w *dapWriter) {
		// The handshake every session needs; the caller's handler decides the rest.
		switch req.Command {
		case "initialize":
			w.respond(req, map[string]any{})
			return
		case "attach":
			w.respond(req, nil)
			w.event("initialized", nil)
			return
		}
		handle(req, w)
	})
	s := &k3dDebugSession{
		a: &App{}, key: "1/frame1", status: "detached",
		tgt: k3dDebugTarget{
			StackID: 1, FrameID: "frame1", Label: "k3d-01", Operator: "pxc",
			BuildDir: "/go/src/github.com/percona/percona-xtradb-cluster-operator",
			SrcDir:   "/root/percona-xtradb-cluster-operator-1.20.0",
		},
		bps: map[string]map[int]k3dDebugBP{}, fns: map[string]k3dDebugFnBP{},
		idleAfter: k3dDebugIdleDefault, subs: map[int]chan []byte{},
	}
	// attach() resolves the address through Docker; the tests dial the fake directly instead.
	s.attachTo(t, f)
	return s, f
}

// attachTo is attach() with the address already known — the dial is the only part of attach that
// needs a cluster.
func (s *k3dDebugSession) attachTo(t *testing.T, f *fakeDAP) {
	t.Helper()
	ctx := context.Background()
	events := make(chan dapRawEvent, 64)
	cli, err := dapDial(ctx, f.addr(), func(name string, body json.RawMessage) {
		select {
		case events <- dapRawEvent{name, body}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := cli.initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := cli.attachRemote(ctx); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := cli.waitInitialized(ctx); err != nil {
		t.Fatalf("initialized: %v", err)
	}
	s.mu.Lock()
	s.cli = cli
	s.status = "running"
	s.mu.Unlock()
	s.rearm(ctx, cli)
	go s.pump(events, cli)
	t.Cleanup(func() { cli.Close() })
}

func waitFor(t *testing.T, s *k3dDebugSession, want string) k3dDebugState {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		st := s.snapshot()
		if st.Status == want {
			return st
		}
		select {
		case <-deadline:
			t.Fatalf("status = %q, want %q", st.Status, want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A breakpoint is set on a repo-relative path, and must reach Delve as the path the binary was
// compiled with — that mapping is what an IDE needs substitutePath for, and doing it here is why
// the panel needs no configuration.
func TestSessionBreakpointUsesTheCompiledPath(t *testing.T) {
	s, f := newTestSession(t, func(req dapMessage, w *dapWriter) {
		if req.Command == "setBreakpoints" {
			w.respond(req, map[string]any{"breakpoints": []dapBreakpoint{{ID: 1, Verified: true, Line: 91}}})
			return
		}
		w.respond(req, nil)
	})

	if err := s.setBreakpoint(context.Background(), "pkg/controller/pxc/controller.go", 91, true); err != nil {
		t.Fatalf("setBreakpoint: %v", err)
	}
	m, ok := f.request("setBreakpoints")
	if !ok {
		t.Fatal("no setBreakpoints request")
	}
	var args struct {
		Source      dapSource             `json:"source"`
		Breakpoints []dapSourceBreakpoint `json:"breakpoints"`
	}
	json.Unmarshal(m.Arguments, &args)
	want := "/go/src/github.com/percona/percona-xtradb-cluster-operator/pkg/controller/pxc/controller.go"
	if args.Source.Path != want {
		t.Fatalf("source path = %q, want %q", args.Source.Path, want)
	}
	if len(args.Breakpoints) != 1 || args.Breakpoints[0].Line != 91 {
		t.Fatalf("breakpoints = %+v", args.Breakpoints)
	}

	st := s.snapshot()
	if len(st.Breakpoints) != 1 || !st.Breakpoints[0].Verified {
		t.Fatalf("state breakpoints = %+v", st.Breakpoints)
	}
	// ...and the panel keeps seeing it by its repo-relative name.
	if st.Breakpoints[0].File != "pkg/controller/pxc/controller.go" {
		t.Fatalf("breakpoint file = %q", st.Breakpoints[0].File)
	}
}

// Removing one breakpoint of two re-sends the file's remaining set, because that is the only way
// DAP expresses it.
func TestSessionRemovingOneResendsTheRest(t *testing.T) {
	s, f := newTestSession(t, func(req dapMessage, w *dapWriter) {
		w.respond(req, map[string]any{"breakpoints": []dapBreakpoint{{Verified: true}, {Verified: true}}})
	})
	ctx := context.Background()
	s.setBreakpoint(ctx, "pkg/a.go", 10, true)
	s.setBreakpoint(ctx, "pkg/a.go", 20, true)
	s.setBreakpoint(ctx, "pkg/a.go", 10, false)

	f.mu.Lock()
	last := f.seen[len(f.seen)-1]
	f.mu.Unlock()
	var args struct {
		Breakpoints []dapSourceBreakpoint `json:"breakpoints"`
	}
	json.Unmarshal(last.Arguments, &args)
	if len(args.Breakpoints) != 1 || args.Breakpoints[0].Line != 20 {
		t.Fatalf("last setBreakpoints = %+v, want only line 20", args.Breakpoints)
	}
	if st := s.snapshot(); len(st.Breakpoints) != 1 || st.Breakpoints[0].Line != 20 {
		t.Fatalf("state = %+v", st.Breakpoints)
	}
}

// A stop is turned into state the panel can draw: the stack, mapped back to repo-relative paths,
// with frames outside the operator's source kept but marked as having none.
func TestSessionStopMapsTheStack(t *testing.T) {
	s, _ := newTestSession(t, func(req dapMessage, w *dapWriter) {
		switch req.Command {
		case "continue":
			w.respond(req, nil)
			w.event("stopped", dapStoppedEvent{Reason: "breakpoint", ThreadID: 17})
		case "stackTrace":
			w.respond(req, map[string]any{"stackFrames": []dapStackFrame{
				{ID: 1000, Name: "(*ReconcilePerconaXtraDBCluster).Reconcile", Line: 91, Source: dapSource{
					Path: "/go/src/github.com/percona/percona-xtradb-cluster-operator/pkg/controller/pxc/controller.go"}},
				{ID: 1001, Name: "controller-runtime.(*Controller).Reconcile", Line: 114, Source: dapSource{
					Path: "/go/pkg/mod/sigs.k8s.io/controller-runtime@v0.19.0/pkg/internal/controller/controller.go"}},
			}})
		default:
			w.respond(req, nil)
		}
	})

	s.mu.Lock()
	s.status = "stopped" // so exec("continue") is allowed
	s.mu.Unlock()
	if err := s.exec(context.Background(), "continue"); err != nil {
		t.Fatalf("continue: %v", err)
	}
	st := waitFor(t, s, "stopped")
	if st.ThreadID != 17 || len(st.Frames) != 2 {
		t.Fatalf("state = %+v", st)
	}
	if got := st.Frames[0]; got.File != "pkg/controller/pxc/controller.go" || !got.HasSource || got.Line != 91 {
		t.Fatalf("frame 0 = %+v", got)
	}
	if got := st.Frames[1]; got.HasSource || !strings.HasPrefix(got.File, "/go/pkg/mod/") {
		t.Fatalf("frame 1 = %+v, want kept but without source", got)
	}
}

// Detaching must clear the breakpoints on the server and resume a stopped operator — in that
// order, or the resume walks straight back into the breakpoint it just left. This is the rule the
// watchdog sidecar exists to enforce when the app cannot.
func TestSessionTeardownClearsThenResumes(t *testing.T) {
	var order []string
	var mu sync.Mutex
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}
	s, _ := newTestSession(t, func(req dapMessage, w *dapWriter) {
		switch req.Command {
		case "setBreakpoints":
			var args struct {
				Breakpoints []dapSourceBreakpoint `json:"breakpoints"`
			}
			json.Unmarshal(req.Arguments, &args)
			if len(args.Breakpoints) == 0 {
				record("clear")
			}
			w.respond(req, map[string]any{"breakpoints": []dapBreakpoint{{Verified: true}}})
		case "continue":
			record("continue")
			w.respond(req, nil)
		case "disconnect":
			record("disconnect")
			w.respond(req, nil)
		default:
			w.respond(req, nil)
		}
	})

	s.setBreakpoint(context.Background(), "pkg/a.go", 10, true)
	s.mu.Lock()
	s.status, s.thread = "stopped", 3
	s.mu.Unlock()

	s.teardown("test")

	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	if got != "clear,continue,disconnect" {
		t.Fatalf("teardown order = %q, want clear,continue,disconnect", got)
	}
	if st := s.snapshot(); st.Status != "detached" {
		t.Fatalf("status = %q, want detached", st.Status)
	}
	// The breakpoint is kept in the session, so the next attach re-arms it — that is what makes
	// closing the page cheap rather than destructive.
	if st := s.snapshot(); len(st.Breakpoints) != 1 {
		t.Fatalf("breakpoints after teardown = %+v, want them kept", st.Breakpoints)
	}
}

// Re-attaching re-sends everything the session was holding.
func TestSessionReattachRearmsBreakpoints(t *testing.T) {
	s, _ := newTestSession(t, func(req dapMessage, w *dapWriter) {
		w.respond(req, map[string]any{"breakpoints": []dapBreakpoint{{Verified: true}}})
	})
	ctx := context.Background()
	s.setBreakpoint(ctx, "pkg/a.go", 10, true)
	s.setFunctionBreakpoint(ctx, "(*ReconcilePerconaXtraDBCluster).Reconcile", true)
	s.teardown("test")

	f2 := startFakeDAP(t, func(req dapMessage, w *dapWriter) {
		switch req.Command {
		case "initialize":
			w.respond(req, map[string]any{})
		case "attach":
			w.respond(req, nil)
			w.event("initialized", nil)
		default:
			w.respond(req, map[string]any{"breakpoints": []dapBreakpoint{{Verified: true}}})
		}
	})
	s.attachTo(t, f2)

	var files, fns int
	f2.mu.Lock()
	for _, m := range f2.seen {
		switch m.Command {
		case "setBreakpoints":
			var args struct {
				Breakpoints []dapSourceBreakpoint `json:"breakpoints"`
			}
			json.Unmarshal(m.Arguments, &args)
			if len(args.Breakpoints) > 0 {
				files++
			}
		case "setFunctionBreakpoints":
			var args struct {
				Breakpoints []dapFunctionBreakpoint `json:"breakpoints"`
			}
			json.Unmarshal(m.Arguments, &args)
			if len(args.Breakpoints) > 0 {
				fns++
			}
		}
	}
	f2.mu.Unlock()
	if files != 1 || fns != 1 {
		t.Fatalf("re-armed %d file(s) and %d function breakpoint(s), want 1 and 1", files, fns)
	}
}

// The idle guard: a stop nobody touches resumes itself, because the alternative is a cluster that
// silently stops being reconciled.
func TestSessionIdleStopResumesItself(t *testing.T) {
	resumed := make(chan struct{}, 1)
	s, _ := newTestSession(t, func(req dapMessage, w *dapWriter) {
		if req.Command == "continue" {
			select {
			case resumed <- struct{}{}:
			default:
			}
		}
		w.respond(req, nil)
	})

	s.mu.Lock()
	s.idleAfter = 60 * time.Millisecond
	s.status, s.thread = "stopped", 5
	s.mu.Unlock()
	s.armIdle()

	select {
	case <-resumed:
	case <-time.After(2 * time.Second):
		t.Fatal("the idle guard never resumed the operator")
	}
}

// ...and a command from the panel is proof somebody is still there, so the countdown restarts.
func TestSessionTouchResetsTheIdleGuard(t *testing.T) {
	resumed := make(chan struct{}, 1)
	s, _ := newTestSession(t, func(req dapMessage, w *dapWriter) {
		if req.Command == "continue" {
			select {
			case resumed <- struct{}{}:
			default:
			}
		}
		w.respond(req, map[string]any{"breakpoints": []dapBreakpoint{{Verified: true}}})
	})
	s.mu.Lock()
	s.idleAfter = 150 * time.Millisecond
	s.status, s.thread = "stopped", 5
	s.mu.Unlock()
	s.armIdle()

	// Keep touching it for longer than the idle window.
	for i := 0; i < 4; i++ {
		time.Sleep(60 * time.Millisecond)
		s.touch()
	}
	select {
	case <-resumed:
		t.Fatal("the idle guard fired while the session was being used")
	default:
	}
}

// An expression that calls a function runs real code inside the operator, so it needs to be
// turned on first.
func TestSessionEvaluateGatesFunctionCalls(t *testing.T) {
	s, _ := newTestSession(t, func(req dapMessage, w *dapWriter) {
		w.respond(req, map[string]any{"result": "ok", "type": "string"})
	})
	ctx := context.Background()

	if _, err := s.evaluate(ctx, "r.client.Get(ctx, req, o)", 1); err == nil {
		t.Fatal("want a call to be refused by default")
	}
	if _, err := s.evaluate(ctx, "request.NamespacedName", 1); err != nil {
		t.Fatalf("a plain expression should be allowed: %v", err)
	}
	s.setAllowCalls(true)
	if _, err := s.evaluate(ctx, "o.Status.State()", 1); err != nil {
		t.Fatalf("a call should be allowed once turned on: %v", err)
	}
}

func TestDebugLooksLikeCall(t *testing.T) {
	calls := []string{"f()", "r.client.Get(ctx)", "len(xs)", "pkg.Fn(1, 2)"}
	plain := []string{"request.NamespacedName", "o.Spec.PXC.Size", "xs[0]", "(a + b) * c", "*o"}
	for _, e := range calls {
		if !k3dDebugLooksLikeCall(e) {
			t.Errorf("%q should be treated as a call", e)
		}
	}
	for _, e := range plain {
		if k3dDebugLooksLikeCall(e) {
			t.Errorf("%q should not be treated as a call", e)
		}
	}
}

// Stepping while the operator is running is a mistake worth naming, rather than an opaque error
// from Delve.
func TestSessionStepRequiresAStop(t *testing.T) {
	s, _ := newTestSession(t, func(req dapMessage, w *dapWriter) { w.respond(req, nil) })
	err := s.exec(context.Background(), "next")
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("err = %v, want it to say the operator is running", err)
	}
	// Pause is the one that is meant to be used while it runs.
	if err := s.exec(context.Background(), "pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
}

// The path helpers, both ways, including the paths that must be refused.
func TestDebugPathMapping(t *testing.T) {
	build := "/go/src/github.com/percona/percona-xtradb-cluster-operator"
	if got := k3dDebugBuildPath(build, "pkg/controller/pxc/controller.go"); got != build+"/pkg/controller/pxc/controller.go" {
		t.Fatalf("build path = %q", got)
	}
	if got := k3dDebugBuildPath(build, "/pkg/a.go"); got != build+"/pkg/a.go" {
		t.Fatalf("a leading slash should not double up: %q", got)
	}
	if rel, ok := k3dDebugRelPath(build, build+"/cmd/manager/main.go"); !ok || rel != "cmd/manager/main.go" {
		t.Fatalf("rel = %q, %v", rel, ok)
	}
	for _, p := range []string{
		"/go/pkg/mod/sigs.k8s.io/controller-runtime@v0.19.0/pkg/x.go", // a dependency, not ours
		"/usr/local/go/src/runtime/proc.go",                           // the standard library
		"",
	} {
		if _, ok := k3dDebugRelPath(build, p); ok {
			t.Errorf("%q should not map into the operator's source", p)
		}
	}
}

// A subscriber that stops reading must not stall the session — the debugger cannot wait on a
// browser.
func TestSessionSlowSubscriberIsDropped(t *testing.T) {
	s, _ := newTestSession(t, func(req dapMessage, w *dapWriter) { w.respond(req, nil) })
	ch, _ := s.subscribe()
	// Never read from ch; fill it well past its buffer.
	for i := 0; i < 200; i++ {
		s.logf("info", "line %d", i)
	}
	s.submu.Lock()
	n := len(s.subs)
	s.submu.Unlock()
	if n != 0 {
		t.Fatalf("subscribers = %d, want the stalled one dropped", n)
	}
	_ = ch
}
