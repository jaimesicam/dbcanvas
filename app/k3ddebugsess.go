package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// k3ddebugsess.go — one debug session per K3D frame: the connection to Delve, the breakpoints,
// and the rules that keep a debugger from quietly breaking the cluster it is attached to.
//
// k3ddebug.go put the operator under `dlv exec --headless`; k3ddap.go can talk to it. This is the
// part in between — the thing a browser panel drives.
//
// # How it gets there
//
// Not through the port published to the host. That port exists for an external IDE, and the app
// cannot reach the host's loopback from inside its container anyway. The debugger is *also* a
// NodePort Service (k3dDebugService), and a k3d cluster is created on the stack network, so a k3s
// node's InternalIP on :30400 is an address a sibling container can route to — the same route
// Stock Market Sim takes to reach a database in the cluster. The app joins the stack network for
// the dial exactly as the Query Runner does. This is also why `--only-same-user=false` is in the
// patch: Delve refuses a peer it cannot prove is the same OS user unless the peer is loopback,
// and through a NodePort it never is.
//
// # What the session owes the cluster
//
// A breakpoint stops the operator, and a stopped operator reconciles nothing — no probe fails
// (the liveness probe is deliberately gone) and nothing is logged. The cluster just stops being
// managed. So the session is responsible for never leaving one armed with nobody watching:
//
//   - **Detach clears the server's breakpoints and resumes**, before disconnecting. They are kept
//     in the session so the next attach re-arms them, which is what makes closing the page cheap.
//   - **An idle stop resumes itself.** Sitting at a breakpoint for k3dDebugIdleDefault with nobody
//     touching anything is treated as "walked away", and the operator is continued with a line in
//     the event log saying so.
//   - **A page reload does not tear the session down.** The last subscriber leaving starts a short
//     grace period instead — unless the operator is *stopped*, in which case there is no grace: a
//     frozen operator is not something to hold open on the chance someone comes back.
//
// The watchdog sidecar from k3ddebug.go stays the backstop for what this cannot cover — the app
// being killed mid-session. While a session is live its socket is ESTABLISHED, which is exactly
// the condition the watchdog treats as "someone is attached", so the two never fight.

const (
	// k3dDebugIdleDefault is how long the operator may sit at a breakpoint with nobody issuing a
	// command before the session resumes it by itself.
	k3dDebugIdleDefault = 5 * time.Minute
	// k3dDebugGrace is how long a running session survives its last subscriber leaving, so that
	// reloading the page does not clear and re-arm every breakpoint.
	k3dDebugGrace = 20 * time.Second
	// k3dDebugStackDepth is how many frames are fetched for a stop. Deep enough to reach through
	// controller-runtime into the operator's own code, short of paying for a whole goroutine dump.
	k3dDebugStackDepth = 60
	// k3dDebugEventLog is how many event-log lines are kept for a subscriber that joins late.
	k3dDebugEventLog = 200
)

// ---------------------------------------------------------------- the target

// k3dDebugTarget is one frame's debugger, resolved: where to dial it, what was compiled into it,
// and what to say to the cluster it manages.
type k3dDebugTarget struct {
	StackID     int64  `json:"stackId"`
	StackName   string `json:"stackName"`
	FrameID     string `json:"frameId"`
	Label       string `json:"label"`
	Operator    string `json:"operator"`
	OperatorVer string `json:"operatorVer"`
	Cluster     string `json:"cluster"`
	Namespace   string `json:"namespace"`
	CR          string `json:"cr"`       // the custom resource a reconcile is forced on
	Deployment  string `json:"-"`        // the operator Deployment, = the repo name
	SrcDir      string `json:"-"`        // /root/<repo>-<ver> on the server node
	BuildDir    string `json:"buildDir"` // /go/src/github.com/percona/<repo>, the DWARF prefix
	ServerID    string `json:"-"`        // the k3s server node's container
	HostPort    int    `json:"hostPort"`
	NodePort    int    `json:"nodePort"`
	Status      string `json:"debugStatus"` // k3dConfig.DebugStatus: "listening" when it is up
}

func (t k3dDebugTarget) key() string { return fmt.Sprintf("%d/%s", t.StackID, t.FrameID) }

// k3dDebugResolveTarget reads a frame's recorded k3dConfig and turns it into a target. The error
// says which of the three ways it can be unavailable applies, because they need different
// answers: a cluster that is not running, a frame deployed without the debugger, and a debugger
// that failed to attach at deploy time.
func (a *App) k3dDebugResolveTarget(st Stack, doc designDoc, frameID string) (k3dDebugTarget, error) {
	var frame designFrame
	for _, f := range doc.Frames {
		if f.ID == frameID && f.Type == "k3d" {
			frame = f
		}
	}
	if frame.ID == "" {
		return k3dDebugTarget{}, fmt.Errorf("K3D cluster not found")
	}
	cfg, serverID, ok := a.k3dServerConfig(st.ID, doc, frameID)
	if !ok {
		return k3dDebugTarget{}, fmt.Errorf("the K3D cluster %s is not running", frame.Label)
	}
	if cfg.DebugStatus == "" {
		return k3dDebugTarget{}, fmt.Errorf(
			"%s was not deployed with the operator under Delve — tick it on the frame and deploy again", frame.Label)
	}
	if cfg.DebugStatus != "listening" {
		return k3dDebugTarget{}, fmt.Errorf("the debugger on %s is not listening: %s", frame.Label, cfg.DebugStatus)
	}
	repo := k3dOperatorRepos[cfg.Operator]
	return k3dDebugTarget{
		StackID: st.ID, StackName: st.Name, FrameID: frameID, Label: frame.Label,
		Operator: cfg.Operator, OperatorVer: cfg.OperatorVer,
		Cluster: cfg.Cluster, Namespace: cfg.Namespace, CR: k3dCRName(frame),
		Deployment: repo, SrcDir: cfg.OperatorSrc, BuildDir: cfg.DebugBuildDir,
		ServerID: serverID, HostPort: cfg.DebugPort, NodePort: cfg.DebugNodePort,
		Status: cfg.DebugStatus,
	}, nil
}

// k3dDebugDialAddr is the address of the frame's Delve listener from inside the app: a k3s node's
// own address on the stack network, on the NodePort in front of the operator pod.
func (a *App) k3dDebugDialAddr(ctx context.Context, t k3dDebugTarget) (string, error) {
	eng := a.engCtx(ctx)
	if err := a.joinStackForDial(ctx, eng, networkName(t.StackID)); err != nil {
		return "", fmt.Errorf("join the stack network: %w", err)
	}
	ip := a.k3dNodeIP(ctx, t.ServerID)
	if ip == "" {
		return "", fmt.Errorf("could not read the k3s node's address")
	}
	port := t.NodePort
	if port == 0 {
		port = k3dDebugNodePort
	}
	return net.JoinHostPort(ip, strconv.Itoa(port)), nil
}

// ---------------------------------------------------------------- state, as the panel sees it

type k3dDebugFrameInfo struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	// File is repo-relative when the frame is in the operator's own source (so the panel can open
	// it) and the raw compiled path otherwise — a frame in controller-runtime or the standard
	// library is real and worth showing, it just has no source on the node to display.
	File      string `json:"file"`
	Line      int    `json:"line"`
	HasSource bool   `json:"hasSource"`
}

type k3dDebugBP struct {
	File     string `json:"file"` // repo-relative
	Line     int    `json:"line"`
	Verified bool   `json:"verified"`
	Message  string `json:"message,omitempty"`
}

type k3dDebugFnBP struct {
	Name     string `json:"name"`
	Verified bool   `json:"verified"`
	Message  string `json:"message,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// k3dDebugState is the whole of what the panel draws, sent on every change. Small enough to
// re-send in full, which removes a class of bug where a delta and the UI disagree.
type k3dDebugState struct {
	Status      string              `json:"status"` // detached | attaching | running | stopped
	Reason      string              `json:"reason,omitempty"`
	Detail      string              `json:"detail,omitempty"`
	ThreadID    int                 `json:"threadId,omitempty"`
	Frames      []k3dDebugFrameInfo `json:"frames,omitempty"`
	Breakpoints []k3dDebugBP        `json:"breakpoints"`
	Functions   []k3dDebugFnBP      `json:"functions"`
	AllowCalls  bool                `json:"allowCalls"`
	IdleSeconds int                 `json:"idleSeconds"`
	Subscribers int                 `json:"subscribers"`
	Target      k3dDebugTarget      `json:"target"`
}

type k3dDebugLogLine struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // info | warn | error | output
	Text string    `json:"text"`
}

// ---------------------------------------------------------------- the session

type k3dDebugSession struct {
	a   *App
	key string

	mu     sync.Mutex
	tgt    k3dDebugTarget
	cli    *dapClient
	status string
	reason string
	detail string
	thread int
	frames []k3dDebugFrameInfo
	// bps is file (repo-relative) → line → what the server said about it. Kept across a detach
	// so the next attach re-arms exactly what was there.
	bps        map[string]map[int]k3dDebugBP
	fns        map[string]k3dDebugFnBP
	allowCalls bool
	idleAfter  time.Duration
	idleTimer  *time.Timer
	graceTimer *time.Timer

	submu   sync.Mutex
	subs    map[int]chan []byte
	nextSub int
	log     []k3dDebugLogLine
}

// k3dDebugSessionFor returns the frame's session, creating it on first use. Sessions outlive
// their subscribers on purpose — the breakpoint set is the thing worth keeping.
func (a *App) k3dDebugSessionFor(t k3dDebugTarget) *k3dDebugSession {
	s := &k3dDebugSession{
		a: a, key: t.key(), tgt: t, status: "detached",
		bps: map[string]map[int]k3dDebugBP{}, fns: map[string]k3dDebugFnBP{},
		idleAfter: k3dDebugIdleDefault, subs: map[int]chan []byte{},
	}
	actual, loaded := a.debugSessions.LoadOrStore(t.key(), s)
	sess := actual.(*k3dDebugSession)
	if loaded {
		// The recorded target can change under a redeploy (a new cluster, a new source path).
		sess.mu.Lock()
		sess.tgt = t
		sess.mu.Unlock()
	}
	return sess
}

// k3dDebugForget drops every session belonging to a stack. Called when the stack's clusters are
// destroyed: the breakpoints refer to source that is going away with them.
func (a *App) k3dDebugForget(stackID int64) {
	prefix := strconv.FormatInt(stackID, 10) + "/"
	a.debugSessions.Range(func(k, v any) bool {
		if key, ok := k.(string); ok && strings.HasPrefix(key, prefix) {
			if s, ok := v.(*k3dDebugSession); ok {
				s.teardown("the stack was destroyed")
			}
			a.debugSessions.Delete(k)
		}
		return true
	})
}

// ---------------------------------------------------------------- subscribers

// subscribe adds a listener and returns its channel plus the unsubscribe. The caller gets the
// current state and the event log immediately, so a browser that joins a session in progress
// draws the same thing as one that started it.
func (s *k3dDebugSession) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 64)
	s.submu.Lock()
	s.nextSub++
	id := s.nextSub
	s.subs[id] = ch
	backlog := append([]k3dDebugLogLine(nil), s.log...)
	s.submu.Unlock()

	s.mu.Lock()
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}
	s.mu.Unlock()

	// Seed the new subscriber on its own channel rather than broadcasting.
	for _, ln := range backlog {
		ch <- mustJSON(map[string]any{"type": "log", "line": ln})
	}
	ch <- mustJSON(map[string]any{"type": "state", "state": s.snapshot()})

	return ch, func() { s.unsubscribe(id) }
}

func (s *k3dDebugSession) unsubscribe(id int) {
	s.submu.Lock()
	if ch, ok := s.subs[id]; ok {
		delete(s.subs, id)
		close(ch)
	}
	left := len(s.subs)
	s.submu.Unlock()
	if left > 0 {
		return
	}

	// The last one left. A stopped operator is not left stopped on the chance the user comes
	// back; a running one gets a grace period, so a page reload costs nothing.
	s.mu.Lock()
	stopped := s.status == "stopped"
	attached := s.cli != nil
	if s.graceTimer != nil {
		s.graceTimer.Stop()
	}
	if attached && !stopped {
		s.graceTimer = time.AfterFunc(k3dDebugGrace, func() {
			s.submu.Lock()
			none := len(s.subs) == 0
			s.submu.Unlock()
			if none {
				s.teardown("nobody is watching")
			}
		})
	}
	s.mu.Unlock()
	if attached && stopped {
		s.teardown("the last viewer left while the operator was stopped")
	}
}

func (s *k3dDebugSession) broadcast(v any) {
	buf := mustJSON(v)
	s.submu.Lock()
	defer s.submu.Unlock()
	for id, ch := range s.subs {
		select {
		case ch <- buf:
		default:
			// A subscriber that cannot keep up is dropped rather than allowed to stall the
			// session — the debugger must not wait on a browser.
			delete(s.subs, id)
			close(ch)
		}
	}
}

func (s *k3dDebugSession) publishState() {
	s.broadcast(map[string]any{"type": "state", "state": s.snapshot()})
}

func (s *k3dDebugSession) logf(kind, format string, args ...any) {
	ln := k3dDebugLogLine{At: time.Now(), Kind: kind, Text: fmt.Sprintf(format, args...)}
	s.submu.Lock()
	s.log = append(s.log, ln)
	if len(s.log) > k3dDebugEventLog {
		s.log = s.log[len(s.log)-k3dDebugEventLog:]
	}
	s.submu.Unlock()
	s.broadcast(map[string]any{"type": "log", "line": ln})
}

// snapshot copies the state out under the lock. Breakpoints are sorted so the panel's list does
// not reshuffle itself on every update.
func (s *k3dDebugSession) snapshot() k3dDebugState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := k3dDebugState{
		Status: s.status, Reason: s.reason, Detail: s.detail, ThreadID: s.thread,
		Frames:      append([]k3dDebugFrameInfo(nil), s.frames...),
		Breakpoints: []k3dDebugBP{}, Functions: []k3dDebugFnBP{},
		AllowCalls: s.allowCalls, IdleSeconds: int(s.idleAfter / time.Second),
		Target: s.tgt,
	}
	for _, lines := range s.bps {
		for _, bp := range lines {
			st.Breakpoints = append(st.Breakpoints, bp)
		}
	}
	sort.Slice(st.Breakpoints, func(i, j int) bool {
		if st.Breakpoints[i].File != st.Breakpoints[j].File {
			return st.Breakpoints[i].File < st.Breakpoints[j].File
		}
		return st.Breakpoints[i].Line < st.Breakpoints[j].Line
	})
	for _, fn := range s.fns {
		st.Functions = append(st.Functions, fn)
	}
	sort.Slice(st.Functions, func(i, j int) bool { return st.Functions[i].Name < st.Functions[j].Name })
	s.submu.Lock()
	st.Subscribers = len(s.subs)
	s.submu.Unlock()
	return st
}

// ---------------------------------------------------------------- attach / detach

// attach opens the session: dial, handshake, re-arm whatever breakpoints the session holds, and
// then make sure the operator is running.
//
// That last step is the one worth explaining. DAP's attach leaves it open whether the adapter
// halts the debuggee, and an operator halted by our own attach — with the user not yet having
// asked for anything — is exactly the silent freeze this whole file exists to prevent. So the
// handshake ends with a `continue`, and an error from it (the debuggee was already running) is
// not a failure.
func (s *k3dDebugSession) attach(ctx context.Context) error {
	// On the session's own context, not the caller's. Attaching takes a dial, a handshake and
	// a re-arm, and the caller is a browser: a page closed (or a socket timed out) halfway
	// through would otherwise abandon the attach with Delve part-configured. WithoutCancel
	// keeps what the context carries — the engine — and drops only the cancellation.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	s.mu.Lock()
	if s.cli != nil {
		s.mu.Unlock()
		return nil
	}
	tgt := s.tgt
	s.status, s.reason, s.detail = "attaching", "", ""
	s.mu.Unlock()
	s.publishState()

	addr, err := s.a.k3dDebugDialAddr(ctx, tgt)
	if err != nil {
		s.fail(err)
		return err
	}
	events := make(chan dapRawEvent, 64)
	cli, err := dapDial(ctx, addr, func(name string, body json.RawMessage) {
		// Never work in the read loop: fetching a stack trace here would wait for a response
		// the read loop itself has to deliver. Hand off and return.
		select {
		case events <- dapRawEvent{name, body}:
		default:
		}
	})
	if err != nil {
		s.fail(err)
		return err
	}
	if err := cli.initialize(ctx); err != nil {
		cli.Close()
		s.fail(fmt.Errorf("initialize: %w", err))
		return err
	}
	if err := cli.attachRemote(ctx); err != nil {
		cli.Close()
		s.fail(fmt.Errorf("attach: %w", err))
		return err
	}
	if err := cli.waitInitialized(ctx); err != nil {
		cli.Close()
		s.fail(err)
		return err
	}

	s.mu.Lock()
	s.cli = cli
	s.status = "running"
	s.thread, s.frames = 0, nil
	s.mu.Unlock()

	s.logf("info", "attached to the operator's debugger at %s", addr)
	if n := s.rearm(ctx, cli); n > 0 {
		s.logf("info", "re-armed %d breakpoint(s) from the last session", n)
	}
	if err := cli.configurationDone(ctx); err != nil {
		s.logf("warn", "configurationDone: %v", err)
	}
	// Belt and braces: whatever the attach did to the debuggee, it is running when we are done.
	if err := cli.cont(ctx, 1); err != nil {
		s.logf("info", "the operator was already running (%v)", err)
	}

	go s.pump(events, cli)
	s.publishState()
	return nil
}

// rearm re-sends the session's breakpoints to a freshly attached server and returns how many.
func (s *k3dDebugSession) rearm(ctx context.Context, cli *dapClient) int {
	s.mu.Lock()
	files := map[string][]int{}
	for file, lines := range s.bps {
		for line := range lines {
			files[file] = append(files[file], line)
		}
	}
	fns := make([]string, 0, len(s.fns))
	for name := range s.fns {
		fns = append(fns, name)
	}
	s.mu.Unlock()

	n := 0
	for file, lines := range files {
		sort.Ints(lines)
		s.sendFile(ctx, cli, file, lines)
		n += len(lines)
	}
	if len(fns) > 0 {
		sort.Strings(fns)
		s.sendFns(ctx, cli, fns)
		n += len(fns)
	}
	return n
}

// fail records an attach failure as state the panel can render, rather than only as an error on
// one request.
func (s *k3dDebugSession) fail(err error) {
	s.mu.Lock()
	s.status, s.reason, s.detail = "detached", "error", err.Error()
	s.cli = nil
	s.mu.Unlock()
	s.logf("error", "%v", err)
	s.publishState()
}

// teardown ends the session and leaves the operator running with nothing armed.
//
// The order is the same one the watchdog sidecar uses, for the same reason: clear first, then
// resume, or the resume walks straight back into the breakpoint it just left.
func (s *k3dDebugSession) teardown(why string) {
	s.mu.Lock()
	cli := s.cli
	if cli == nil {
		s.mu.Unlock()
		return
	}
	s.cli = nil
	stopped := s.status == "stopped"
	thread := s.thread
	files := make([]string, 0, len(s.bps))
	for file := range s.bps {
		files = append(files, file)
	}
	hasFns := len(s.fns) > 0
	buildDir := s.tgt.BuildDir
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	if s.graceTimer != nil {
		s.graceTimer.Stop()
		s.graceTimer = nil
	}
	s.status, s.reason, s.detail = "detached", "", ""
	s.thread, s.frames = 0, nil
	s.mu.Unlock()

	// A context of its own: teardown runs on a WebSocket that has already gone away.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, file := range files {
		cli.setBreakpoints(ctx, k3dDebugBuildPath(buildDir, file), nil)
	}
	if hasFns {
		cli.setFunctionBreakpoints(ctx, nil)
	}
	if stopped {
		if err := cli.cont(ctx, thread); err != nil {
			s.logf("warn", "could not resume the operator on detach: %v", err)
		}
	}
	cli.dapDisconnect(ctx)
	cli.Close()
	s.logf("info", "detached — %s; breakpoints cleared and the operator is running", why)
	s.publishState()
}

// dapRawEvent is one event on its way from the read loop to the session's own goroutine.
type dapRawEvent struct {
	name string
	body json.RawMessage
}

// pump turns Delve's events into session state. One goroutine, so events are handled in the order
// they arrived, and it exits with the connection.
func (s *k3dDebugSession) pump(events <-chan dapRawEvent, cli *dapClient) {
	for {
		select {
		case ev := <-events:
			s.onEvent(ev, cli)
		case <-cli.Done():
			s.mu.Lock()
			live := s.cli == cli
			if live {
				s.cli = nil
				s.status, s.reason = "detached", "connection lost"
				s.thread, s.frames = 0, nil
				if s.idleTimer != nil {
					s.idleTimer.Stop()
					s.idleTimer = nil
				}
			}
			s.mu.Unlock()
			if live {
				if err := cli.Err(); err != nil {
					s.logf("error", "the debugger connection ended: %v", err)
				} else {
					s.logf("warn", "the debugger connection ended — the operator pod may have restarted")
				}
				s.publishState()
			}
			return
		}
	}
}

func (s *k3dDebugSession) onEvent(ev dapRawEvent, cli *dapClient) {
	// Events from a connection the session has already let go of change nothing. Delve sends
	// `terminated` when a *client* session ends — its own log line says "leaving multi-client
	// DAP server ... with debuggee running" — so without this check an ordinary detach reports
	// the operator as gone.
	s.mu.Lock()
	current := s.cli == cli
	s.mu.Unlock()
	if !current {
		return
	}
	switch ev.name {
	case "stopped":
		var st dapStoppedEvent
		json.Unmarshal(ev.body, &st)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		frames, err := cli.stackTrace(ctx, st.ThreadID, k3dDebugStackDepth)
		cancel()
		if err != nil {
			s.logf("warn", "could not read the stack: %v", err)
		}
		s.mu.Lock()
		s.status, s.reason, s.detail = "stopped", st.Reason, strings.TrimSpace(st.Description+" "+st.Text)
		s.thread = st.ThreadID
		s.frames = k3dDebugMapFrames(s.tgt.BuildDir, frames)
		s.mu.Unlock()
		where := "somewhere with no source"
		if f := s.topFrame(); f != nil {
			where = fmt.Sprintf("%s (%s:%d)", f.Name, f.File, f.Line)
		}
		s.logf("info", "stopped: %s at %s", st.Reason, where)
		s.armIdle()
		s.publishState()

	case "continued":
		s.mu.Lock()
		s.status, s.reason, s.detail = "running", "", ""
		s.thread, s.frames = 0, nil
		s.mu.Unlock()
		s.stopIdle()
		s.publishState()

	case "output":
		var out dapOutputEvent
		json.Unmarshal(ev.body, &out)
		if txt := strings.TrimRight(out.Output, "\n"); txt != "" {
			s.logf("output", "%s", txt)
		}

	case "terminated", "exited":
		// Reached only while this is still the live connection, i.e. the debuggee really did
		// end under us — the pod is being replaced, or somebody stopped it from elsewhere.
		s.logf("warn", "the debuggee ended — the operator pod is being replaced")
		s.mu.Lock()
		s.status, s.reason = "detached", "the debuggee ended"
		s.mu.Unlock()
		s.publishState()
	}
}

func (s *k3dDebugSession) topFrame() *k3dDebugFrameInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.frames) == 0 {
		return nil
	}
	f := s.frames[0]
	return &f
}

// ---------------------------------------------------------------- the idle guard

// armIdle starts the countdown that resumes an operator nobody is stepping through any more.
func (s *k3dDebugSession) armIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	if s.idleAfter <= 0 { // the user turned the guard off
		s.idleTimer = nil
		return
	}
	after := s.idleAfter
	s.idleTimer = time.AfterFunc(after, func() {
		s.mu.Lock()
		cli, stopped, thread := s.cli, s.status == "stopped", s.thread
		s.mu.Unlock()
		if cli == nil || !stopped {
			return
		}
		s.logf("warn", "the operator sat at a breakpoint for %s with nobody stepping — resuming it, "+
			"because a stopped operator reconciles nothing", after)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := cli.cont(ctx, thread); err != nil {
			s.logf("error", "could not resume: %v", err)
		}
	})
}

// touch resets the idle countdown: any command from the panel means somebody is still there.
func (s *k3dDebugSession) touch() {
	s.mu.Lock()
	stopped := s.status == "stopped"
	s.mu.Unlock()
	if stopped {
		s.armIdle()
	}
}

func (s *k3dDebugSession) stopIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

// ---------------------------------------------------------------- breakpoints

// setBreakpoint adds or removes one source breakpoint. DAP has no "add one" request — a file's
// breakpoints are replaced wholesale — so the session keeps the file's set and sends all of it.
func (s *k3dDebugSession) setBreakpoint(ctx context.Context, file string, line int, on bool) error {
	file = strings.TrimPrefix(strings.TrimSpace(file), "/")
	if file == "" || line <= 0 {
		return fmt.Errorf("a breakpoint needs a file and a line")
	}
	s.mu.Lock()
	if s.bps[file] == nil {
		s.bps[file] = map[int]k3dDebugBP{}
	}
	if on {
		s.bps[file][line] = k3dDebugBP{File: file, Line: line}
	} else {
		delete(s.bps[file], line)
	}
	lines := make([]int, 0, len(s.bps[file]))
	for ln := range s.bps[file] {
		lines = append(lines, ln)
	}
	if len(lines) == 0 {
		delete(s.bps, file)
	}
	cli := s.cli
	s.mu.Unlock()
	sort.Ints(lines)

	if cli != nil {
		s.sendFile(ctx, cli, file, lines)
	}
	s.touch()
	s.publishState()
	return nil
}

// sendFile pushes one file's breakpoint set and records what the server made of each line — an
// unverified breakpoint (a line with no code) is a thing the panel must show rather than hide.
func (s *k3dDebugSession) sendFile(ctx context.Context, cli *dapClient, file string, lines []int) {
	s.mu.Lock()
	buildDir := s.tgt.BuildDir
	s.mu.Unlock()

	req := make([]dapSourceBreakpoint, 0, len(lines))
	for _, ln := range lines {
		req = append(req, dapSourceBreakpoint{Line: ln})
	}
	got, err := cli.setBreakpoints(ctx, k3dDebugBuildPath(buildDir, file), req)
	if err != nil {
		s.logf("error", "setting breakpoints in %s: %v", file, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, ln := range lines {
		if s.bps[file] == nil {
			return
		}
		bp := k3dDebugBP{File: file, Line: ln}
		if i < len(got) {
			bp.Verified = got[i].Verified
			bp.Message = got[i].Message
			// Delve moves a breakpoint to the next line that has code; take its word for where
			// it ended up so the panel's marker is where the operator will actually stop.
			if got[i].Line > 0 && got[i].Line != ln {
				bp.Message = strings.TrimSpace(fmt.Sprintf("moved to line %d %s", got[i].Line, bp.Message))
			}
		}
		s.bps[file][ln] = bp
	}
}

// setFunctionBreakpoint is the same for a breakpoint by name — what the panel's quick breakpoints
// use, so nobody has to find the line Reconcile starts on.
func (s *k3dDebugSession) setFunctionBreakpoint(ctx context.Context, name string, on bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("a function breakpoint needs a name")
	}
	s.mu.Lock()
	if on {
		s.fns[name] = k3dDebugFnBP{Name: name}
	} else {
		delete(s.fns, name)
	}
	names := make([]string, 0, len(s.fns))
	for n := range s.fns {
		names = append(names, n)
	}
	cli := s.cli
	s.mu.Unlock()
	sort.Strings(names)

	if cli != nil {
		s.sendFns(ctx, cli, names)
	}
	s.touch()
	s.publishState()
	return nil
}

func (s *k3dDebugSession) sendFns(ctx context.Context, cli *dapClient, names []string) {
	req := make([]dapFunctionBreakpoint, 0, len(names))
	for _, n := range names {
		req = append(req, dapFunctionBreakpoint{Name: n})
	}
	got, err := cli.setFunctionBreakpoints(ctx, req)
	if err != nil {
		s.logf("error", "setting function breakpoints: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, n := range names {
		fn := k3dDebugFnBP{Name: n}
		if i < len(got) {
			fn.Verified, fn.Message, fn.Line = got[i].Verified, got[i].Message, got[i].Line
			if rel, ok := k3dDebugRelPath(s.tgt.BuildDir, got[i].Source.Path); ok {
				fn.File = rel
			}
		}
		s.fns[n] = fn
	}
}

// clearBreakpoints removes everything, on the server and in the session.
func (s *k3dDebugSession) clearBreakpoints(ctx context.Context) {
	s.mu.Lock()
	files := make([]string, 0, len(s.bps))
	for f := range s.bps {
		files = append(files, f)
	}
	hadFns := len(s.fns) > 0
	s.bps = map[string]map[int]k3dDebugBP{}
	s.fns = map[string]k3dDebugFnBP{}
	cli := s.cli
	buildDir := s.tgt.BuildDir
	s.mu.Unlock()

	if cli != nil {
		for _, f := range files {
			cli.setBreakpoints(ctx, k3dDebugBuildPath(buildDir, f), nil)
		}
		if hadFns {
			cli.setFunctionBreakpoints(ctx, nil)
		}
	}
	s.logf("info", "cleared every breakpoint")
	s.publishState()
}

// ---------------------------------------------------------------- stepping and inspection

// exec runs one of the execution commands. They reply at once; where the operator ends up arrives
// later as a `stopped` event, which is what updates the state.
func (s *k3dDebugSession) exec(ctx context.Context, cmd string) error {
	s.mu.Lock()
	cli, thread, status := s.cli, s.thread, s.status
	s.mu.Unlock()
	if cli == nil {
		return fmt.Errorf("not attached")
	}
	if thread == 0 {
		thread = 1
	}
	if cmd != "pause" && status != "stopped" {
		return fmt.Errorf("the operator is running — pause it or wait for a breakpoint")
	}
	var err error
	switch cmd {
	case "continue":
		err = cli.cont(ctx, thread)
	case "next":
		err = cli.next(ctx, thread)
	case "stepIn":
		err = cli.stepIn(ctx, thread)
	case "stepOut":
		err = cli.stepOut(ctx, thread)
	case "pause":
		err = cli.pause(ctx, thread)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		return err
	}
	if cmd == "continue" {
		// Delve does not always announce a resume it was asked for, and a panel that still
		// says "stopped" after Continue reads as a dead debugger.
		s.mu.Lock()
		if s.status == "stopped" {
			s.status, s.reason, s.detail = "running", "", ""
			s.thread, s.frames = 0, nil
		}
		s.mu.Unlock()
		s.stopIdle()
		s.publishState()
	} else {
		s.touch()
	}
	return nil
}

func (s *k3dDebugSession) scopes(ctx context.Context, frameID int) ([]dapScope, error) {
	cli := s.client()
	if cli == nil {
		return nil, fmt.Errorf("not attached")
	}
	s.touch()
	return cli.scopes(ctx, frameID)
}

func (s *k3dDebugSession) variables(ctx context.Context, ref int) ([]dapVariable, error) {
	cli := s.client()
	if cli == nil {
		return nil, fmt.Errorf("not attached")
	}
	s.touch()
	return cli.variables(ctx, ref)
}

// evaluate runs an expression in a frame.
//
// An expression is not only a read: Delve will happily *call a method* on the live operator if
// asked to, which is real code running in a process managing a real cluster. That is a legitimate
// thing to want, and a terrible default, so a call has to be turned on for the session first.
func (s *k3dDebugSession) evaluate(ctx context.Context, expr string, frameID int) (dapVariable, error) {
	cli := s.client()
	if cli == nil {
		return dapVariable{}, fmt.Errorf("not attached")
	}
	s.mu.Lock()
	allow := s.allowCalls
	s.mu.Unlock()
	if !allow && k3dDebugLooksLikeCall(expr) {
		return dapVariable{}, fmt.Errorf(
			"that expression calls a function, which runs real code in the operator — tick “allow function calls” first")
	}
	s.touch()
	return cli.evaluate(ctx, expr, frameID)
}

func (s *k3dDebugSession) client() *dapClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cli
}

func (s *k3dDebugSession) setAllowCalls(on bool) {
	s.mu.Lock()
	s.allowCalls = on
	s.mu.Unlock()
	if on {
		s.logf("warn", "function calls in expressions are now allowed — they run inside the operator")
	}
	s.publishState()
}

// setIdle changes the auto-resume guard. Zero turns it off, which is a deliberate choice the panel
// makes the user confirm.
func (s *k3dDebugSession) setIdle(seconds int) {
	s.mu.Lock()
	s.idleAfter = time.Duration(seconds) * time.Second
	stopped := s.status == "stopped"
	s.mu.Unlock()
	if stopped {
		s.armIdle()
	} else {
		s.stopIdle()
	}
	s.publishState()
}

// ---------------------------------------------------------------- paths

// k3dDebugBuildPath turns a repo-relative path into the one compiled into the binary. The
// operator was built in a container at /go/src/github.com/percona/<repo>, so that is the prefix
// in its DWARF and the only path Delve resolves a breakpoint against.
//
// This is the same mapping an IDE does with substitutePath — done here instead, which is why the
// panel needs no configuration at all.
func k3dDebugBuildPath(buildDir, rel string) string {
	rel = strings.TrimPrefix(strings.TrimSpace(rel), "/")
	if buildDir == "" {
		return rel
	}
	return strings.TrimSuffix(buildDir, "/") + "/" + rel
}

// k3dDebugRelPath is the inverse, and reports false for a path outside the operator's own source:
// a frame in controller-runtime or the standard library comes from the module cache, which is in
// the build container that no longer exists. Those frames are shown without source rather than
// hidden — the call stack is worth reading either way.
func k3dDebugRelPath(buildDir, p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || buildDir == "" {
		return "", false
	}
	prefix := strings.TrimSuffix(buildDir, "/") + "/"
	if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(p, prefix)
	if rel == "" || strings.Contains(rel, "..") {
		return "", false
	}
	return rel, true
}

func k3dDebugMapFrames(buildDir string, frames []dapStackFrame) []k3dDebugFrameInfo {
	out := make([]k3dDebugFrameInfo, 0, len(frames))
	for _, f := range frames {
		info := k3dDebugFrameInfo{ID: f.ID, Name: f.Name, Line: f.Line, File: f.Source.Path}
		if rel, ok := k3dDebugRelPath(buildDir, f.Source.Path); ok {
			info.File, info.HasSource = rel, true
		}
		out = append(out, info)
	}
	return out
}

// k3dDebugLooksLikeCall reports whether an expression would make Delve run code. Deliberately
// crude — it errs towards asking — and it only gates the default; the session can allow calls.
func k3dDebugLooksLikeCall(expr string) bool {
	depth := 0
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '(':
			// A cast — (*foo)(x) or (int)(y) — also has parentheses, but so does a call, and
			// telling them apart needs the parser Delve already has. Ask instead of guessing.
			if depth == 0 && i > 0 && !strings.ContainsRune(" \t+-*/<>=!&|,([{", rune(expr[i-1])) {
				return true
			}
			depth++
		case ')':
			depth--
		}
	}
	return false
}
