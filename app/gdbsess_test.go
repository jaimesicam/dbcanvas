package main

import (
	"context"
	"testing"
)

// These are the exact console lines gdb emits while loading the real core this feature was built
// against — captured from the node, not invented. Getting the attribution wrong here is not a
// cosmetic bug: the page told the user there were no symbols for the executable while showing
// file, line and arguments that could only have come from them.
func TestGDBReadingAttribution(t *testing.T) {
	lines := []struct {
		in   string
		want string // "" when the line is not a "Reading symbols from"
	}{
		{"Reading symbols from /sysroot/mysqld...", "/sysroot/mysqld"},
		{"Reading symbols from /usr/lib/debug//sysroot/mysqld-8.0.16-7.1.el8.x86_64.debug...",
			"/usr/lib/debug//sysroot/mysqld-8.0.16-7.1.el8.x86_64.debug"},
		// Every one of these has a dot in it, which is what the first version choked on.
		{"Reading symbols from /usr/lib64/libpthread-2.28.so...", "/usr/lib64/libpthread-2.28.so"},
		{"Reading symbols from /usr/lib64/libaio.so.1.0.1...", "/usr/lib64/libaio.so.1.0.1"},
		{"Reading symbols from .gnu_debugdata for /usr/lib64/libnuma.so.1.0.0...",
			".gnu_debugdata for /usr/lib64/libnuma.so.1.0.0"},
		{"(no debugging symbols found)...done.", ""},
		{"Program terminated with signal SIGSEGV, Segmentation fault.", ""},
	}
	for _, c := range lines {
		m := gdbReadingRe.FindStringSubmatch(c.in)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != c.want {
			t.Errorf("%q -> %q, want %q", c.in, got, c.want)
		}
	}
}

// The signal is only ever said once, on the console stream, while the core is loading. `info
// program` cannot be asked afterwards: on a core file it answers "The program being debugged is
// not being run", which is what the first version of this shipped as the crash summary.
func TestGDBTerminatedLine(t *testing.T) {
	m := gdbTerminatedRe.FindStringSubmatch("Program terminated with signal SIGSEGV, Segmentation fault.")
	if m == nil {
		t.Fatal("the terminated line did not match")
	}
	if m[1] != "SIGSEGV" || m[2] != "Segmentation fault." {
		t.Errorf("signal = %q, meaning = %q", m[1], m[2])
	}
	if gdbTerminatedRe.MatchString("The program being debugged is not being run.") {
		t.Error("`info program`'s reply must not be read as a signal")
	}
}

// A whole session's console stream, replayed: only the executable's own verdict may set the
// symbol warning.
func TestGDBSymbolVerdictIsAttributed(t *testing.T) {
	sess := &gdbSession{tgt: gdbTarget{Binary: "/sysroot/mysqld"}, subs: map[int]chan []byte{}}
	for _, ln := range []string{
		"Reading symbols from /sysroot/mysqld...",
		"Reading symbols from /usr/lib/debug//sysroot/mysqld-8.0.16-7.1.el8.x86_64.debug...",
		"Program terminated with signal SIGSEGV, Segmentation fault.",
		"Reading symbols from /usr/lib64/libpthread-2.28.so...",
		"(no debugging symbols found)...done.",
		"Reading symbols from /usr/lib64/libaio.so.1.0.1...",
		"(no debugging symbols found)...done.",
	} {
		sess.onStream(miConsoleOut, ln)
	}
	if sess.symbols != "" {
		t.Errorf("symbols verdict = %q — those warnings were about the libraries", sess.symbols)
	}
	if sess.signal != "SIGSEGV" || sess.sigText != "Segmentation fault" {
		t.Errorf("signal = %q / %q", sess.signal, sess.sigText)
	}

	// And when it really is the executable, it has to be said.
	sess2 := &gdbSession{tgt: gdbTarget{Binary: "/usr/sbin/mysqld"}, subs: map[int]chan []byte{}}
	sess2.onStream(miConsoleOut, "Reading symbols from /usr/sbin/mysqld...")
	sess2.onStream(miConsoleOut, "(no debugging symbols found)...done.")
	if sess2.symbols == "" {
		t.Error("the executable having no symbols was not reported")
	}
}

// `thread apply all bt` is the command people run first, and this is what it becomes: every
// thread's stack in one reply, each carrying the stack's REAL depth rather than the number of
// frames printed. The depth is the part the command cannot give you — it prints what it prints,
// and a window of forty frames out of 1,085 looks exactly like a stack of forty.
func TestGDBThreadStacksCarryTheRealDepth(t *testing.T) {
	cli, fake, _ := newMIFake(t,
		`^done,stack=[frame={level="0",addr="0x1",func="_int_malloc",from="/lib64/libc.so.6"},`+
			`frame={level="1",addr="0x2",func="fts_query_visitor(fts_ast_oper_t, fts_ast_node_t*, void*)"}]`,
		`^done,depth="1085"`,
		`^done,stack=[frame={level="0",addr="0x3",func="poll",from="/lib64/libc.so.6"}]`,
		`^done,depth="4"`)
	sess := &gdbSession{
		subs:    map[int]chan []byte{},
		cli:     cli,
		thread:  "1",
		threads: []miThread{{ID: "1", Target: "Thread 0x76 (LWP 9756)"}, {ID: "2", Name: "ib_io_rd"}},
	}
	stacks, err := sess.threadStacks(context.Background())
	if err != nil {
		t.Fatalf("threadStacks: %v", err)
	}
	if len(stacks) != 2 {
		t.Fatalf("stacks = %d, want one per thread", len(stacks))
	}
	if !stacks[0].Signal {
		t.Error("the thread that took the signal is not marked")
	}
	if stacks[0].Depth != 1085 {
		t.Errorf("depth = %d, want the whole stack's 1085 rather than the frames returned", stacks[0].Depth)
	}
	if len(stacks[0].Frames) != 2 || stacks[0].Frames[0].Func != "_int_malloc" {
		t.Errorf("frames = %+v", stacks[0].Frames)
	}
	if stacks[1].Name != "ib_io_rd" {
		t.Errorf("thread 2 name = %q — a thread that named itself says more than its LWP", stacks[1].Name)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.written) != 4 {
		t.Fatalf("sent %d commands, want two per thread: %v", len(fake.written), fake.written)
	}
}

// One thread gdb cannot unwind is not a failed request: the other fifty-nine are still the answer,
// and the row says what happened to this one.
func TestGDBThreadStacksSurviveOneBadThread(t *testing.T) {
	cli, _, _ := newMIFake(t, `^error,msg="Invalid thread id: 2"`)
	sess := &gdbSession{subs: map[int]chan []byte{}, cli: cli, threads: []miThread{{ID: "2"}}}
	stacks, err := sess.threadStacks(context.Background())
	if err != nil {
		t.Fatalf("threadStacks: %v", err)
	}
	if len(stacks) != 1 || stacks[0].Error == "" {
		t.Fatalf("stacks = %+v, want one row carrying gdb's message", stacks)
	}
}

// `bt full` is per frame in MI, and the reply has to come back keyed by the frame LEVEL rather
// than by position: the pane it fills has collapsed recursion in it, so its rows are not levels.
func TestGDBFrameVarsAreKeyedByLevel(t *testing.T) {
	cli, _, _ := newMIFake(t,
		`^done,variables=[{name="node",arg="1",type="fts_ast_node_t *",value="0x7602d0001234"}]`,
		`^error,msg="No symbol table info available."`,
		`^done,variables=[{name="state",type="fts_query_t *",value="0x7602d0005678"}]`)
	sess := &gdbSession{subs: map[int]chan []byte{}, cli: cli}
	got, err := sess.frameVars(context.Background(), "1", []int{4, 5, 6})
	if err != nil {
		t.Fatalf("frameVars: %v", err)
	}
	if len(got[4]) != 1 || got[4][0].Name != "node" || !got[4][0].Arg {
		t.Errorf("frame 4 = %+v", got[4])
	}
	// A frame with no debug information has no locals, which is an answer rather than a failure —
	// it must not take the other frames down with it.
	if _, ok := got[5]; ok {
		t.Errorf("frame 5 = %+v, want nothing", got[5])
	}
	if len(got[6]) != 1 || got[6][0].Name != "state" {
		t.Errorf("frame 6 = %+v", got[6])
	}
}
