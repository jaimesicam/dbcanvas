package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// gdbsess.go — one live gdb, reading one core file, shared by everyone looking at it.
//
// The shape is the Operator Debugger's (k3ddebugsess.go): a server-held session keyed by target,
// subscribers fed the whole state on every change, a bounded event log, and teardown when the last
// viewer leaves. It is markedly simpler than that one for a single reason — **a core file does not
// run**. There is no continue, no breakpoints, no lease to renew, and no way to leave a cluster
// halted by walking away. The failure mode the debugger needs a watchdog sidecar for does not
// exist here; the worst a forgotten session costs is a gdb process holding a file open.
//
// What it adds instead is the thing the one-line recipe has no room for: a verdict. gdb will
// happily print a backtrace from the wrong libraries or the wrong build, and it will look exactly
// like a right one. So the session keeps what gdb says about itself — the log stream is where
// "Missing separate debuginfo for /usr/sbin/mysqld" arrives — and hands it to the page next to the
// stack it produced.

const (
	// How long a session survives with nobody watching. Longer than the debugger's 20s: loading
	// an 800 MB core takes seconds, and paying that again because a page was reloaded is a poor
	// trade for a process that is doing nothing.
	gdbGrace = 3 * time.Minute
	// Frames fetched per request. The crash this was built for is unbounded recursion several
	// hundred frames deep; a window keeps the first request fast and lets the page ask for more.
	gdbFrameWindow = 200
	// The event log's ring, matching the debugger's.
	gdbEventLog = 200
	// How long any one gdb command may take. Loading the core is the slow one and has its own,
	// longer, budget.
	//
	// Five minutes rather than the two this started with, because of what opening a *value* costs.
	// A backtrace is cheap — gdb already has the frames — but `-var-list-children` on a C++ object
	// makes gdb read the full DWARF for that type and everything it is built from, and on a server
	// binary with a gigabyte of debug information that is tens of seconds the first time, measured
	// at 41s for one struct on the 8.4.5 core this was read against. A budget that cuts that off
	// turns a slow answer into no answer, which is the worse of the two.
	gdbCommandTimeout = 5 * time.Minute
	gdbLoadTimeout    = 10 * time.Minute
)

// gdbTarget is one Linux Client node that can analyse core dumps, resolved.
type gdbTarget struct {
	StackID     int64  `json:"stackId"`
	StackName   string `json:"stackName"`
	NodeID      string `json:"nodeId"`
	Label       string `json:"label"`
	Hostname    string `json:"hostname"`
	OS          string `json:"os"`
	OSVersion   string `json:"osVersion"`
	Product     string `json:"product"`
	Major       string `json:"major"`
	Version     string `json:"version"`
	Binary      string `json:"binary"`
	BinaryFrom  string `json:"binaryFrom"` // "mounted" (the copy that crashed) | "installed"
	BuildID     string `json:"buildId"`
	HasSymbols  bool   `json:"hasSymbols"`
	CoreDir     string `json:"coreDir"` // the host path, for display
	LibDir      string `json:"libDir"`
	Status      string `json:"status"`
	ContainerID string `json:"-"`
	// Container is the node's container NAME, which is what a person types. It is here for the
	// recipe (gdbRecipe) and nothing else: the id would work too and is unreadable in a ticket.
	Container string `json:"container,omitempty"`
}

func (t gdbTarget) key() string { return fmt.Sprintf("%d/%s", t.StackID, t.NodeID) }

// gdbLogLine is one line of the session log.
type gdbLogLine struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // info | warn | error
	Text string    `json:"text"`
}

// gdbState is everything the page draws, re-sent whole on every change.
type gdbState struct {
	Status      string      `json:"status"` // idle | loading | ready | error
	Detail      string      `json:"detail,omitempty"`
	Core        string      `json:"core,omitempty"` // the file being read
	Signal      string      `json:"signal,omitempty"`
	SignalText  string      `json:"signalText,omitempty"`
	Threads     []miThread  `json:"threads"`
	Thread      string      `json:"thread,omitempty"` // the selected one
	Total       int         `json:"totalThreads"`
	AllowShell  bool        `json:"allowShell"`
	Symbols     string      `json:"symbols,omitempty"` // gdb's own verdict on the symbol table
	Verdict     *gdbVerdict `json:"verdict,omitempty"` // what went wrong and why — see gdbdiag.go
	Subscribers int         `json:"subscribers"`
	Target      gdbTarget   `json:"target"`
	// Recipe is the command line that reproduces this session in a terminal — built when the core
	// is opened, because it carries the search path this node actually resolved (gdbRecipe).
	Recipe string `json:"recipe,omitempty"`
}

// gdbSession is a gdb process plus everyone watching it.
type gdbSession struct {
	a   *App
	key string

	mu      sync.Mutex
	tgt     gdbTarget
	cli     *miClient
	conn    *ExecConn
	core    string
	status  string
	detail  string
	signal  string
	sigText string
	symbols string
	verdict *gdbVerdict
	reading string // the object gdb last said it was reading symbols from
	recipe  string // the terminal equivalent of this session, for copying
	// Where source lives on this node, resolved once when the core is opened, and the answers
	// already worked out for the paths the debug information records. Both are per-core: a
	// different core can be a different build with a different tree unpacked next to it.
	srcRoots []string
	srcPaths map[string]string // DWARF path -> the file on this node, "" when there is none
	threads  []miThread
	thread   string
	allow    bool
	grace    *time.Timer

	submu   sync.Mutex
	subs    map[int]chan []byte
	nextSub int
	log     []gdbLogLine
}

// gdbSessionFor returns the session for a node, creating it if this is the first viewer.
func (a *App) gdbSessionFor(t gdbTarget) *gdbSession {
	s := &gdbSession{a: a, key: t.key(), tgt: t, status: "idle", subs: map[int]chan []byte{}}
	actual, loaded := a.gdbSessions.LoadOrStore(t.key(), s)
	sess := actual.(*gdbSession)
	if loaded {
		// A redeploy can change the container id or the installed version under a session that
		// is only holding the old target for display.
		sess.mu.Lock()
		if sess.cli == nil {
			sess.tgt = t
		}
		sess.mu.Unlock()
	}
	return sess
}

// gdbForget tears down every session belonging to a stack. Called when a stack is destroyed, for
// the same reason the debugger's equivalent is: the container is about to stop, and a session
// holding an exec into it would otherwise linger until its grace timer.
func (a *App) gdbForget(stackID int64) {
	prefix := fmt.Sprintf("%d/", stackID)
	a.gdbSessions.Range(func(k, v any) bool {
		if key, ok := k.(string); ok && strings.HasPrefix(key, prefix) {
			v.(*gdbSession).teardown("the stack was destroyed")
			a.gdbSessions.Delete(k)
		}
		return true
	})
}

// ---------------------------------------------------------------- subscribers

func (s *gdbSession) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 64)
	s.submu.Lock()
	s.nextSub++
	id := s.nextSub
	s.subs[id] = ch
	backlog := append([]gdbLogLine(nil), s.log...)
	s.submu.Unlock()

	s.mu.Lock()
	if s.grace != nil {
		s.grace.Stop()
		s.grace = nil
	}
	s.mu.Unlock()

	for _, ln := range backlog {
		ch <- mustJSON(map[string]any{"type": "log", "line": ln})
	}
	ch <- mustJSON(map[string]any{"type": "state", "state": s.snapshot()})
	return ch, func() { s.unsubscribe(id) }
}

func (s *gdbSession) unsubscribe(id int) {
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
	// Nobody is watching. Unlike the debugger there is nothing to heal, so the only question is
	// how long to keep an expensively loaded core around for someone who reloaded the page.
	s.mu.Lock()
	if s.grace != nil {
		s.grace.Stop()
	}
	if s.cli != nil {
		s.grace = time.AfterFunc(gdbGrace, func() {
			s.submu.Lock()
			none := len(s.subs) == 0
			s.submu.Unlock()
			if none {
				s.teardown("nobody is watching")
			}
		})
	}
	s.mu.Unlock()
}

func (s *gdbSession) broadcast(v any) {
	buf := mustJSON(v)
	s.submu.Lock()
	defer s.submu.Unlock()
	for id, ch := range s.subs {
		select {
		case ch <- buf:
		default:
			delete(s.subs, id)
			close(ch)
		}
	}
}

func (s *gdbSession) publishState() {
	s.broadcast(map[string]any{"type": "state", "state": s.snapshot()})
}

func (s *gdbSession) logf(kind, format string, args ...any) {
	ln := gdbLogLine{At: time.Now(), Kind: kind, Text: fmt.Sprintf(format, args...)}
	s.submu.Lock()
	s.log = append(s.log, ln)
	if len(s.log) > gdbEventLog {
		s.log = s.log[len(s.log)-gdbEventLog:]
	}
	s.submu.Unlock()
	s.broadcast(map[string]any{"type": "log", "line": ln})
}

func (s *gdbSession) snapshot() gdbState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := gdbState{
		Status: s.status, Detail: s.detail, Core: s.core,
		Signal: s.signal, SignalText: s.sigText, Symbols: s.symbols, Verdict: s.verdict,
		Threads: append([]miThread(nil), s.threads...), Thread: s.thread,
		Total: len(s.threads), AllowShell: s.allow, Target: s.tgt, Recipe: s.recipe,
	}
	if st.Threads == nil {
		st.Threads = []miThread{}
	}
	s.submu.Lock()
	st.Subscribers = len(s.subs)
	s.submu.Unlock()
	return st
}

func (s *gdbSession) client() *miClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cli
}

// ---------------------------------------------------------------- opening a core

// gdbStartScript is the command line gdb is started with.
//
// `stty -echo -onlcr` is not cosmetic — see the header of gdbmi.go. `-nx` skips every .gdbinit on
// the node, which is both reproducibility and one less place for a file to run code. The `set`s
// are the user's own recipe, plus two of our own:
//
//   - **auto-load off / startup-with-shell off** because gdb runs arbitrary Python out of
//     objfile-gdb.py files next to a binary, and a shell for its inferior. The inferior here is a
//     dead process that will never run, so neither buys anything, and both are ways for a mounted
//     directory to become code execution.
//   - **backtrace past-main / past-entry on** because a stack-exhaustion core often has its
//     deepest frames past what gdb considers the beginning, and cutting them off hides the bottom
//     of the recursion, which is the part that names the bug.
const gdbStartScript = `stty -echo -onlcr 2>/dev/null
exec gdb --interpreter=mi3 -nx \
  -ex "set auto-load off" \
  -ex "set startup-with-shell off" \
  -ex "set pagination off" \
  -ex "set confirm off" \
  -ex "set print pretty on" \
  -ex "set backtrace past-main on" \
  -ex "set backtrace past-entry on" \
  -ex "set sysroot $SYSROOT" \
  -ex "set solib-search-path $SOLIB" \
  -ex "set debug-file-directory /usr/lib/debug" \
  -ex "directory $SRCDIRS" \
  "$BINARY" "$CORE"
`

// open starts gdb on one core file.
//
// It runs on a context detached from the caller's: loading a large core takes tens of seconds, and
// a browser that navigates away mid-load would otherwise leave a half-started gdb behind. The page
// is told what is happening through the state's `loading` phase instead.
func (s *gdbSession) open(ctx context.Context, core string) error {
	s.mu.Lock()
	if s.status == "loading" {
		s.mu.Unlock()
		return fmt.Errorf("a core file is already being loaded")
	}
	tgt := s.tgt
	s.mu.Unlock()

	if err := gdbSafeCoreName(core); err != nil {
		return err
	}
	if tgt.Binary == "" {
		return fmt.Errorf("no server binary was found on this node — the debug symbols did not install")
	}

	s.closeClient()
	s.setStatus("loading", "reading "+core)
	s.logf("info", "loading %s with %s", core, tgt.Binary)

	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gdbLoadTimeout)
	defer cancel()

	// Anything left over from a previous session on this node goes first.
	//
	// gdb runs inside the node, on the far side of a docker exec, and an exec does not die when
	// the process that started it does: restart DBCanvas and the gdb it was driving keeps running,
	// holding an 800 MB core mapped and a gigabyte of symbols resident, with nothing left that can
	// talk to it. Three of them accumulated on the sample node in an afternoon, and the only
	// symptom is that everything gets slow — the next session's queries are competing with two
	// ghosts for the same page cache. One gdb per node is the design (the session is keyed by
	// node), so anything else speaking MI in there is by definition orphaned.
	//
	// Only the MI ones: a person who ran the recipe in their own terminal is running gdb without
	// --interpreter=mi3, and their session is not ours to end.
	s.a.gdbKillStrays(loadCtx, tgt.ContainerID)

	solib := s.a.gdbSolibPath(loadCtx, tgt.ContainerID)
	// Where debugsource unpacked the code. A recent build's DWARF is absolute and needs none of
	// this; an older one records a relative path that gdb resolves against exactly this list.
	srcRoots := s.a.gdbSourceRoots(loadCtx, tgt.ContainerID)
	conn, err := s.a.engCtx(loadCtx).HijackExec(loadCtx, tgt.ContainerID,
		[]string{"sh", "-c", gdbStartScript},
		[]string{
			"SYSROOT=" + gdbSysrootMount,
			"SOLIB=" + solib,
			"BINARY=" + tgt.Binary,
			"CORE=" + gdbCoreMount + "/" + core,
			"SRCDIRS=" + strings.Join(srcRoots, " "),
			"TERM=dumb",
		}, "")
	if err != nil {
		s.setStatus("error", err.Error())
		return fmt.Errorf("start gdb: %w", err)
	}

	cli := newMIClient(conn, miHandlers{
		Stream: func(kind byte, text string) { s.onStream(kind, text) },
		Async:  func(class string, payload map[string]any) { s.onAsync(class, payload) },
	})

	s.mu.Lock()
	s.cli, s.conn, s.core = cli, conn, core
	s.symbols, s.signal, s.sigText, s.reading, s.verdict = "", "", "", "", nil
	s.srcRoots, s.srcPaths = srcRoots, map[string]string{}
	// Built here rather than in the browser because this is where the truth is: the solib path was
	// just resolved by walking the mount, and a recipe that guessed it would send somebody off to
	// read a different set of libraries than the page did.
	s.recipe = gdbRecipe(tgt.Container, tgt.Binary, gdbCoreMount+"/"+core, gdbSysrootMount, solib, srcRoots)
	s.mu.Unlock()

	// The first command is also the proof that gdb came up and finished reading the core: MI
	// answers nothing until the -ex list has run.
	threads, current, err := cli.threads(loadCtx)
	if err != nil {
		s.closeClient()
		s.setStatus("error", err.Error())
		return fmt.Errorf("read the core's threads: %w", err)
	}
	// Which thread took the signal: gdb selects it, so "current" is the answer, and it is the one
	// thing about a core everybody wants first. *What* the signal was arrived on the console
	// stream while the core was loading — see onStream.
	s.mu.Lock()
	s.threads, s.thread = threads, current
	sig := s.signal
	s.status, s.detail = "ready", ""
	s.mu.Unlock()
	if sig == "" {
		sig = "no signal"
	}
	s.logf("info", "%d threads; thread %s took %s", len(threads), current, sig)
	s.publishState()
	go s.watch(cli)

	// Then work out what actually happened. It is deliberately *after* the state is published:
	// reading a 1,000-frame stack takes a moment, and the panes are usable while it runs. A
	// diagnosis that delayed the stack would be a worse trade than one that arrives a second late.
	s.mu.Lock()
	sigText := s.sigText
	s.mu.Unlock()
	if v := s.diagnose(loadCtx, cli, current, s.signalNow(), sigText); v != nil {
		s.mu.Lock()
		s.verdict = v
		s.mu.Unlock()
		s.logf("info", "%s", v.Headline)
		s.publishState()
	}
	return nil
}

// watch reports gdb going away on its own.
//
// Without it the session goes on saying "ready" while every command comes back "gdb ended: EOF",
// which reads as the page being broken rather than as the debugger being gone — and the two want
// completely different things from the reader. Killed from outside, out of memory, or crashed on a
// value it could not parse: whichever it was, the answer is to open the core again, and the page
// can only offer that if it knows.
//
// A client that is no longer the session's is one we replaced ourselves (closeClient clears the
// field before closing it), so that case is silence.
func (s *gdbSession) watch(cli *miClient) {
	<-cli.Done()
	s.mu.Lock()
	ours := s.cli == cli
	s.mu.Unlock()
	if !ours {
		return
	}
	s.closeClient()
	s.mu.Lock()
	s.status = "error"
	s.detail = "gdb is no longer running on the node — open the core again"
	s.threads, s.thread = nil, ""
	s.mu.Unlock()
	s.logf("error", "gdb exited on its own — the session is gone; open the core again")
	s.publishState()
}

func (s *gdbSession) signalNow() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signal
}

// onStream records what gdb says about itself while it reads the core.
//
// Two of those sentences are the whole verdict, and gdb says each exactly once, at load, where
// nobody reading a stack afterwards would ever see it:
//
//   - "Program terminated with signal SIGSEGV, Segmentation fault." — which is *the* answer to what
//     happened, and cannot be asked for afterwards. `info program` on a core file replies "The
//     program being debugged is not being run", which is true and useless. (That reply is what the
//     first version of this shipped as the crash summary, until it was read on a real core.)
//   - "(no debugging symbols found)" — but that arrives once per object, and on a flat dump of the
//     crashed host's libraries *most* objects have no symbols and never will. Attributing it to the
//     executable when it belonged to libcrypto is how a page ends up claiming there are no symbols
//     while showing file, line and arguments. So the preceding "Reading symbols from <path>" is
//     tracked and the verdict is attached to that path.
func (s *gdbSession) onStream(kind byte, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if m := gdbReadingRe.FindStringSubmatch(text); m != nil {
		s.mu.Lock()
		s.reading = m[1]
		s.mu.Unlock()
		return
	}
	if m := gdbTerminatedRe.FindStringSubmatch(text); m != nil {
		s.mu.Lock()
		s.signal = m[1]
		s.sigText = strings.TrimSuffix(strings.TrimSpace(m[2]), ".")
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	reading, binary := s.reading, s.tgt.Binary
	s.mu.Unlock()
	aboutTheExecutable := reading != "" && binary != "" && reading == binary

	switch {
	case strings.Contains(text, "Missing separate debuginfo for"):
		if strings.Contains(text, binary) || aboutTheExecutable {
			s.setSymbolVerdict("no separate debug symbols for the executable — frames will have " +
				"names but no arguments or line numbers")
			s.logf("warn", "%s", text)
		}
	case strings.Contains(text, "no debugging symbols found"):
		if aboutTheExecutable {
			s.setSymbolVerdict("no debug symbols for the executable — frames will have names but " +
				"no arguments or line numbers")
			s.logf("warn", "%s (%s)", text, reading)
		}
	case strings.Contains(text, "does not match"), strings.HasPrefix(text, "warning: exec file"):
		s.logf("warn", "%s", text)
	case kind == miLogOut && strings.Contains(text, "Can't open file"):
		// The tail of a split warning; see the case below for the head. One per unmapped object.
		return
	case kind == miLogOut && strings.HasPrefix(text, "warning:"):
		// gdb splits a warning across two log records — `&"\nwarning: "` and then the sentence —
		// so the first half arrives as the bare word and would fill the log with "warning:" and
		// nothing else. And one whole warning arrives per unmapped object, which the core
		// listing's coverage check already reports properly and in one line.
		if text == "warning:" || strings.Contains(text, "file-backed mapping note") {
			return
		}
		s.logf("warn", "%s", text)
	}
}

func (s *gdbSession) setSymbolVerdict(v string) {
	s.mu.Lock()
	s.symbols = v
	s.mu.Unlock()
}

var (
	gdbTerminatedRe = regexp.MustCompile(`Program terminated with signal (SIG[A-Z0-9]+),\s*(.*)`)
	// The path has to be captured *greedily* up to the trailing ellipsis. An earlier version
	// stopped at the first dot, which no library path survives — libpthread-2.28.so,
	// libaio.so.1.0.1 — so the pattern simply never matched a library, "the object being read"
	// stayed pinned to the executable for the rest of the load, and every library's
	// "(no debugging symbols found)" was reported against it. The page then said there were no
	// symbols for mysqld while showing file, line and arguments from them.
	gdbReadingRe = regexp.MustCompile(`^Reading symbols from (.+?)\.\.\.$`)
)

func (s *gdbSession) onAsync(class string, payload map[string]any) {
	if class == "thread-group-exited" || class == "thread-exited" {
		return
	}
	if class == "stopped" {
		if sig := miStr(payload["signal-name"]); sig != "" {
			s.mu.Lock()
			s.signal = sig
			s.sigText = miStr(payload["signal-meaning"])
			s.mu.Unlock()
			s.publishState()
		}
	}
}

func (s *gdbSession) setStatus(status, detail string) {
	s.mu.Lock()
	s.status, s.detail = status, detail
	s.mu.Unlock()
	s.publishState()
}

func (s *gdbSession) closeClient() {
	s.mu.Lock()
	cli, conn := s.cli, s.conn
	s.cli, s.conn = nil, nil
	s.mu.Unlock()
	if cli != nil {
		cli.Close()
	}
	if conn != nil {
		conn.Close()
	}
}

func (s *gdbSession) teardown(why string) {
	s.mu.Lock()
	running := s.cli != nil
	if s.grace != nil {
		s.grace.Stop()
		s.grace = nil
	}
	s.mu.Unlock()
	if !running {
		return
	}
	s.closeClient()
	s.mu.Lock()
	s.status, s.detail, s.core = "idle", "", ""
	s.threads, s.thread = nil, ""
	s.mu.Unlock()
	s.logf("info", "gdb closed — %s", why)
	s.publishState()
}

// ---------------------------------------------------------------- queries

// withClient runs f against the live gdb, with the standard command budget.
func (s *gdbSession) withClient(ctx context.Context, f func(context.Context, *miClient) error) error {
	cli := s.client()
	if cli == nil {
		return fmt.Errorf("no core file is open")
	}
	ctx, cancel := context.WithTimeout(ctx, gdbCommandTimeout)
	defer cancel()
	return f(ctx, cli)
}

// backtrace reads one thread's stack, collapsing runs of repeating frames.
func (s *gdbSession) backtrace(ctx context.Context, thread string, offset int) ([]miFrame, bool, error) {
	var frames []miFrame
	err := s.withClient(ctx, func(ctx context.Context, cli *miClient) error {
		var err error
		// One extra frame is asked for, purely to know whether there are more.
		frames, err = cli.frames(ctx, thread, offset, gdbFrameWindow+1)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	more := len(frames) > gdbFrameWindow
	if more {
		frames = frames[:gdbFrameWindow]
	}
	s.mu.Lock()
	s.thread = thread
	s.mu.Unlock()
	return gdbCollapse(frames), more, nil
}

// gdbCollapse folds a repeating cycle of frames into one entry carrying its repeat count.
//
// This is what makes a stack-exhaustion core readable. The crash this was built for recurses
// through fts_query_visitor → fts_ast_visit → fts_ast_visit_sub_exp several hundred times;
// rendered frame by frame it is a page of identical lines with the interesting parts — the bottom
// of the recursion and the top — pushed out of sight.
//
// The cycle ceiling is eight rather than the two or three that would seem enough. Read on the real
// core, the repeating unit turned out to be *five* frames; a four-frame ceiling found the inner
// three-frame run instead and reported "fts_ast_visit repeats 3 times", which is true, useless,
// and leaves the outer cycle spread across the whole pane. The longest period is preferred over
// the shortest for the same reason.
func gdbCollapse(frames []miFrame) []miFrame {
	out := make([]miFrame, 0, len(frames))
	for i := 0; i < len(frames); {
		best, bestReps := 0, 0
		for period := 1; period <= 8 && i+period*2 <= len(frames); period++ {
			reps := 0
			for {
				next := i + (reps+1)*period
				if next+period > len(frames) || !gdbSameCycle(frames[i:i+period], frames[next:next+period]) {
					break
				}
				reps++
			}
			// Three or more consecutive copies is a cycle; two is a coincidence worth showing.
			// A longer period that repeats as often wins: it is the real unit, and the shorter
			// one is a fragment of it.
			if reps >= 2 && reps >= bestReps {
				best, bestReps = period, reps
			}
		}
		if bestReps == 0 {
			out = append(out, frames[i])
			i++
			continue
		}
		group := frames[i : i+best]
		for j, f := range group {
			c := f
			if j == 0 {
				c.Repeat = bestReps + 1
			}
			out = append(out, c)
		}
		i += best * (bestReps + 1)
	}
	return out
}

func gdbSameCycle(a, b []miFrame) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		// Addresses differ between iterations (each is a distinct return site is not true — in a
		// direct recursion they are identical — but level always differs), so the identity of a
		// frame for this purpose is its function.
		if a[i].Func != b[i].Func || a[i].Func == "" {
			return false
		}
	}
	return true
}

func (s *gdbSession) variables(ctx context.Context, thread string, frame int) ([]miVar, error) {
	var vars []miVar
	err := s.withClient(ctx, func(ctx context.Context, cli *miClient) error {
		var err error
		vars, err = cli.variables(ctx, thread, frame)
		return err
	})
	return vars, err
}

func (s *gdbSession) evaluate(ctx context.Context, thread string, frame int, expr string) (string, error) {
	var val string
	err := s.withClient(ctx, func(ctx context.Context, cli *miClient) error {
		var err error
		val, err = cli.evaluate(ctx, thread, frame, expr)
		return err
	})
	return val, err
}

func (s *gdbSession) disassemble(ctx context.Context, thread string, frame int) ([]string, error) {
	var lines []string
	err := s.withClient(ctx, func(ctx context.Context, cli *miClient) error {
		var err error
		lines, err = cli.disassemble(ctx, thread, frame)
		return err
	})
	return lines, err
}

// console runs a raw gdb command, refusing the ones that are not analysis.
func (s *gdbSession) console(ctx context.Context, cmd string) (string, error) {
	s.mu.Lock()
	allow := s.allow
	s.mu.Unlock()
	if !allow {
		if why := gdbUnsafeCommand(cmd); why != "" {
			return "", fmt.Errorf("%s — tick “allow shell commands” first", why)
		}
	}
	var out string
	err := s.withClient(ctx, func(ctx context.Context, cli *miClient) error {
		var err error
		out, err = cli.console(ctx, cmd)
		return err
	})
	if err == nil && allow {
		s.logf("warn", "ran %q with shell commands allowed", cmd)
	}
	return out, err
}

func (s *gdbSession) setAllowShell(on bool) {
	s.mu.Lock()
	s.allow = on
	s.mu.Unlock()
	if on {
		s.logf("warn", "shell commands are now allowed — gdb's `shell` runs as root on this node")
	}
	s.publishState()
}

// gdbUnsafeCommand names the gdb commands that are not reading a core file.
//
// gdb is a programmable debugger: `shell` is a root shell on the node, `python` and `guile` are
// interpreters, `pipe` feeds output to one, and `file`/`core-file` would repoint the session at
// something the mount confinement never approved. The same reasoning as the Operator Debugger's
// function-call gate — the difference is that there a call is sometimes what you want, and here
// these are never part of reading a stack.
func gdbUnsafeCommand(cmd string) string {
	c := strings.ToLower(strings.TrimSpace(cmd))
	if c == "" {
		return ""
	}
	if strings.HasPrefix(c, "!") {
		return "that runs a shell command"
	}
	word := c
	if i := strings.IndexAny(word, " \t"); i >= 0 {
		word = word[:i]
	}
	switch word {
	case "shell", "sh", "pipe", "|", "python", "python-interactive", "pi", "guile", "guile-repl", "gi":
		return "that runs code outside gdb"
	case "file", "core-file", "exec-file", "add-symbol-file", "generate-core-file", "gcore", "dump", "append":
		return "that reads or writes a file this node was not given"
	case "set":
		// Most `set`s are fine and useful — print elements, backtrace limit, listsize. These two
		// are the ones the session turned off at startup to stop gdb running code.
		if strings.Contains(c, "startup-with-shell") || strings.Contains(c, "auto-load") {
			return "that re-enables a way for gdb to run code"
		}
	case "define", "document", "source":
		return "that defines or loads gdb scripting"
	}
	return ""
}

// gdbSafeCoreName confines the file to open to a plain name inside the mounted directory. The
// caller owns the stack and could open a terminal on the node, so this is not the security
// boundary — it is here so a path cannot turn a core viewer into a way to load /etc/shadow as an
// ELF file and get a confusing error.
func gdbSafeCoreName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("no core file named")
	}
	if strings.ContainsAny(name, "/\x00\n") || name == "." || name == ".." {
		return fmt.Errorf("%q is not a file in %s", name, gdbCoreMount)
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------- every thread at once

// gdbThreadStack is one thread's stack in the all-threads view.
type gdbThreadStack struct {
	Thread string    `json:"thread"`
	Name   string    `json:"name,omitempty"`
	Target string    `json:"target,omitempty"`
	Depth  int       `json:"depth"`
	Frames []miFrame `json:"frames"`
	More   bool      `json:"more"`             // the stack is deeper than the frames here
	Signal bool      `json:"signal,omitempty"` // this is the thread that took the signal
	Error  string    `json:"error,omitempty"`
}

// gdbAllThreadWindow is how many frames each thread contributes to the all-threads view.
//
// Not the whole stack, on purpose. The view answers "what was every thread doing", and for that a
// thread's top frames are the answer — the one stack anybody reads to the bottom is the one that
// crashed, and selecting a thread loads that properly. A 1,085-frame recursion rendered sixty-one
// times over is not a more complete answer, it is an unusable one.
const gdbAllThreadWindow = 40

// threadStacks is `thread apply all bt`, with the two things that command cannot do.
//
// The command exists and people run it first, so this is the view the tool owes them. What it adds
// is what makes the output readable at sixty threads:
//
//   - **The real depth of each stack**, from -stack-info-depth, not the number of frames printed.
//   - **Collapsed recursion**, the same folding the single-thread pane does.
//
// The grouping of identical stacks — twenty threads all parked in the same wait — is done on the
// page rather than here, because which stacks count as "the same" is a display decision and the
// frames are needed either way.
func (s *gdbSession) threadStacks(ctx context.Context) ([]gdbThreadStack, error) {
	s.mu.Lock()
	threads := append([]miThread(nil), s.threads...)
	signalled := s.thread
	s.mu.Unlock()
	if len(threads) == 0 {
		return nil, fmt.Errorf("no core file is open")
	}
	out := make([]gdbThreadStack, 0, len(threads))
	err := s.withClient(ctx, func(ctx context.Context, cli *miClient) error {
		for _, t := range threads {
			ts := gdbThreadStack{Thread: t.ID, Name: t.Name, Target: t.Target, Signal: t.ID == signalled}
			frames, err := cli.frames(ctx, t.ID, 0, gdbAllThreadWindow+1)
			if err != nil {
				// One unreadable thread is not a failed request: a core with a thread gdb cannot
				// unwind still has fifty-nine it can, and saying so per row is the honest shape.
				ts.Error = err.Error()
				out = append(out, ts)
				continue
			}
			if len(frames) > gdbAllThreadWindow {
				frames, ts.More = frames[:gdbAllThreadWindow], true
			}
			ts.Frames = gdbCollapse(frames)
			if d, err := cli.stackDepth(ctx, t.ID); err == nil {
				ts.Depth = d
			} else {
				ts.Depth = len(frames)
			}
			out = append(out, ts)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------- bt full

// gdbFullVarsMax is how many frames one `bt full` request reads locals for.
//
// Each frame is its own MI round trip — MI has no "locals for a range of frames" — and on a server
// binary each of those costs gdb a couple of seconds of DWARF reading. Forty is the most that can
// be asked for without the answer taking longer than anybody will wait for it, and it is the top
// forty, which is where a crash is. The page mirrors this number so that the rows it cannot fill
// say so rather than sitting on "reading…" forever.
const gdbFullVarsMax = 40

// frameVars reads the arguments and locals of a run of frames — the `full` in `bt full`.
//
// One frame at a time is the only way MI offers (-stack-list-variables takes a single frame), but
// the round trips are local to the node and the answer is what turns a stack into something you
// can read without clicking thirty times. The reply is keyed by frame level rather than by
// position, because the pane it fills has collapsed cycles in it and its rows are not levels.
func (s *gdbSession) frameVars(ctx context.Context, thread string, levels []int) (map[int][]miVar, error) {
	if len(levels) > gdbFullVarsMax {
		levels = levels[:gdbFullVarsMax]
	}
	out := map[int][]miVar{}
	err := s.withClient(ctx, func(ctx context.Context, cli *miClient) error {
		for _, lv := range levels {
			vars, err := cli.variables(ctx, thread, lv)
			if err != nil {
				continue // a frame with no debug information has no locals; that is not an error
			}
			if vars == nil {
				vars = []miVar{}
			}
			out[lv] = vars
		}
		return nil
	})
	return out, err
}

// children opens one value — see miClient.varChildren.
func (s *gdbSession) children(ctx context.Context, thread string, frame int, expr string) ([]miChild, bool, error) {
	var (
		kids []miChild
		more bool
	)
	err := s.withClient(ctx, func(ctx context.Context, cli *miClient) error {
		var err error
		kids, more, err = cli.varChildren(ctx, thread, frame, expr)
		return err
	})
	return kids, more, err
}

// ---------------------------------------------------------------- source

// sourcePath maps a path out of the debug information to a file on this node, remembering the
// answer.
//
// Worth a cache even though the lookup is one exec: the source pane asks on every frame you click,
// and a stack walked frame by frame asks for the same handful of files over and over.
func (s *gdbSession) sourcePath(ctx context.Context, file string) (string, error) {
	s.mu.Lock()
	roots := append([]string(nil), s.srcRoots...)
	hit, known := "", false
	if s.srcPaths != nil {
		hit, known = s.srcPaths[file]
	}
	tgt := s.tgt
	s.mu.Unlock()
	if known {
		if hit == "" {
			return "", gdbNoSourceError(file)
		}
		return hit, nil
	}
	found, err := s.a.gdbFindSource(ctx, tgt.ContainerID, file, roots)
	s.mu.Lock()
	if s.srcPaths != nil {
		s.srcPaths[file] = found // "" records the miss, which is the answer worth not asking twice
	}
	s.mu.Unlock()
	return found, err
}
