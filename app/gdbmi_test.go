package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixtures below are **real** records, captured from `gdb --interpreter=mi3` reading the
// 811 MB Percona Server 8.0.16 core this feature was built against. Hand-written MI is not worth
// testing: every mistake this parser made was in a shape nobody would have thought to invent.

// A list of *bare tuples*. The first version of the parser read `{id` as the name of the first
// result — the `=` it scanned to was the one inside the tuple — and produced a thread list of one
// nameless entry, for every -thread-info reply ever sent.
const miThreadsFixture = `^done,threads=[{id="1",target-id="Thread 0x7602d8112700 (LWP 9756)",` +
	`frame={level="0",addr="0x00007602eb1295de",func="_int_malloc",args=[],from="/lib64/libc.so.6"},state="stopped"},` +
	`{id="2",target-id="Thread 0x7602ed397380 (LWP 9712)",` +
	`frame={level="0",addr="0x00007602eb1c29b1",func="poll",args=[],from="/lib64/libc.so.6"},state="stopped"}],` +
	`current-thread-id="1"`

// A list of *named* results — MI does not distinguish [a,b] from [x=a,x=b], and -stack-list-frames
// uses the second form, so a repeated name has to become a list rather than overwrite.
const miStackFixture = `^done,stack=[` +
	`frame={level="0",addr="0x00007602eb1295de",func="_int_malloc",from="/lib64/libc.so.6"},` +
	`frame={level="1",addr="0x00007602eb12c0d6",func="calloc",from="/lib64/libc.so.6"},` +
	`frame={level="2",addr="0x0000000001f67add",func="ut_allocator<unsigned char>::allocate(unsigned long, unsigned char const*, unsigned int, bool, bool)"},` +
	`frame={level="3",addr="0x000000000216283f",func="rbt_create(unsigned long, int (*)(void const*, void const*))"},` +
	`frame={level="4",addr="0x00000000022f9c88",func="fts_query_visitor(fts_ast_oper_t, fts_ast_node_t*, void*)",file="fts0que.cc",fullname="/src/storage/innobase/fts/fts0que.cc",line="3707"}]`

func TestMIParsesThreadsFromRealGDB(t *testing.T) {
	class, payload := miClassAndPayload(miThreadsFixture[1:])
	if class != "done" {
		t.Fatalf("class = %q, want done", class)
	}
	if got := miStr(payload["current-thread-id"]); got != "1" {
		t.Errorf("current-thread-id = %q, want 1", got)
	}
	list := miList(payload["threads"])
	if len(list) != 2 {
		t.Fatalf("threads = %d, want 2 — a list of bare tuples must not collapse: %#v", len(list), list)
	}
	first, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("thread 0 is %T, want a tuple", list[0])
	}
	if miStr(first["id"]) != "1" {
		t.Errorf("thread 0 id = %q, want 1", miStr(first["id"]))
	}
	// The target-id carries spaces and parentheses inside its c-string; a parser that split on
	// commas without honouring quotes would lose the LWP.
	if want := "Thread 0x7602d8112700 (LWP 9756)"; miStr(first["target-id"]) != want {
		t.Errorf("target-id = %q, want %q", miStr(first["target-id"]), want)
	}
	frame, ok := first["frame"].(map[string]any)
	if !ok {
		t.Fatalf("thread 0 frame is %T, want a tuple", first["frame"])
	}
	if miStr(frame["func"]) != "_int_malloc" {
		t.Errorf("top frame = %q, want _int_malloc", miStr(frame["func"]))
	}
}

func TestMIParsesStackFromRealGDB(t *testing.T) {
	_, payload := miClassAndPayload(miStackFixture[1:])
	list := miList(payload["stack"])
	if len(list) != 5 {
		t.Fatalf("stack = %d frames, want 5 — repeated `frame=` results must become a list: %#v", len(list), list)
	}
	frames := make([]miFrame, 0, len(list))
	for _, item := range list {
		m := item.(map[string]any)
		if inner, ok := m["frame"].(map[string]any); ok {
			m = inner
		}
		frames = append(frames, miFrameOf(m))
	}
	if frames[0].Func != "_int_malloc" || frames[0].From != "/lib64/libc.so.6" {
		t.Errorf("frame 0 = %+v", frames[0])
	}
	// A C++ signature is full of commas and parentheses and lives inside one c-string. Getting
	// this wrong truncates every symbol in an InnoDB backtrace at its first argument.
	if want := "rbt_create(unsigned long, int (*)(void const*, void const*))"; frames[3].Func != want {
		t.Errorf("frame 3 func = %q, want %q", frames[3].Func, want)
	}
	if frames[3].Level != 3 {
		t.Errorf("frame 3 level = %d, want 3", frames[3].Level)
	}
	// fullname wins over file, and the line number comes back as an int.
	if frames[4].File != "/src/storage/innobase/fts/fts0que.cc" || frames[4].Line != 3707 {
		t.Errorf("frame 4 source = %s:%d", frames[4].File, frames[4].Line)
	}
}

// A frame with no debug information has no file and no line, and must not be reported as line 0 of
// a file called "" — the page shows `from <object>` for exactly these.
func TestMIFrameWithoutSymbols(t *testing.T) {
	f := miFrameOf(map[string]any{
		"level": "9", "addr": "0x1", "func": "??", "from": "/lib64/libc.so.6",
	})
	if f.File != "" || f.Line != 0 {
		t.Errorf("unsymbolised frame claims source %s:%d", f.File, f.Line)
	}
	if f.From != "/lib64/libc.so.6" {
		t.Errorf("from = %q", f.From)
	}
}

func TestMICStringUnescapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"plain"`, "plain"},
		{`"a\nb"`, "a\nb"},
		{`"say \"hi\""`, `say "hi"`},
		{`"back\\slash"`, `back\slash`},
		{`"has,comma and {brace}"`, "has,comma and {brace}"},
	}
	for _, c := range cases {
		if got, _ := miCString(c.in); got != c.want {
			t.Errorf("miCString(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMIQuoteRoundTrips(t *testing.T) {
	for _, s := range []string{`plain`, `has "quotes"`, "has\nnewline", `back\slash`, `p tab\t`} {
		got, _ := miCString(miQuote(s))
		if got != s {
			t.Errorf("round trip of %q gave %q", s, got)
		}
	}
}

// ---------------------------------------------------------------- the client

// miFake is a scripted gdb: it answers each command with the next canned record, and can emit
// unsolicited records the way a real one does.
type miFake struct {
	mu      sync.Mutex
	in      *io.PipeReader // what the client writes
	out     *io.PipeWriter // what the client reads
	written []string
}

type miPipe struct {
	io.Reader
	io.Writer
	closeOnce sync.Once
	onClose   func()
}

func (p *miPipe) Close() error { p.closeOnce.Do(p.onClose); return nil }

// newMIFake starts a fake gdb that replies to command n with replies[n].
func newMIFake(t *testing.T, replies ...string) (*miClient, *miFake, chan string) {
	t.Helper()
	toGDB, fromClient := io.Pipe() // client -> fake
	fromGDB, toClient := io.Pipe() // fake  -> client
	f := &miFake{in: toGDB, out: toClient}
	logs := make(chan string, 16)

	go func() {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 256)
		n := 0
		for {
			c, err := toGDB.Read(tmp)
			if c > 0 {
				buf = append(buf, tmp[:c]...)
			}
			for {
				i := strings.IndexByte(string(buf), '\n')
				if i < 0 {
					break
				}
				line := string(buf[:i])
				buf = buf[i+1:]
				f.mu.Lock()
				f.written = append(f.written, line)
				f.mu.Unlock()
				token := line[:strings.IndexFunc(line, func(r rune) bool { return r < '0' || r > '9' })]
				body := "^done"
				if n < len(replies) {
					body = replies[n]
				}
				n++
				toClient.Write([]byte(token + body + "\n(gdb)\n"))
			}
			if err != nil {
				return
			}
		}
	}()

	cli := newMIClient(&miPipe{Reader: fromGDB, Writer: fromClient, onClose: func() {
		fromClient.Close()
		toClient.Close()
	}}, miHandlers{
		Stream: func(kind byte, text string) {
			if kind == miLogOut {
				select {
				case logs <- text:
				default:
				}
			}
		},
	})
	t.Cleanup(func() { cli.Close() })
	return cli, f, logs
}

func TestMIClientMatchesRepliesToCommands(t *testing.T) {
	cli, fake, _ := newMIFake(t, miThreadsFixture, miStackFixture)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	threads, current, err := cli.threads(ctx)
	if err != nil {
		t.Fatalf("threads: %v", err)
	}
	if len(threads) != 2 || current != "1" {
		t.Fatalf("threads = %d, current = %q", len(threads), current)
	}
	if threads[0].Frame == nil || threads[0].Frame.Func != "_int_malloc" {
		t.Errorf("thread 1 top frame = %+v", threads[0].Frame)
	}

	frames, err := cli.frames(ctx, "1", 0, 200)
	if err != nil {
		t.Fatalf("frames: %v", err)
	}
	if len(frames) != 5 {
		t.Fatalf("frames = %d, want 5", len(frames))
	}

	// Every command carries its own token, and the reply is matched by it rather than by order —
	// which is what lets the session run two queries at once.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.written) != 2 {
		t.Fatalf("sent %d commands, want 2: %v", len(fake.written), fake.written)
	}
	if !strings.HasSuffix(fake.written[0], "-thread-info") {
		t.Errorf("first command = %q", fake.written[0])
	}
	if !strings.Contains(fake.written[1], "-stack-list-frames --thread 1 0 199") {
		t.Errorf("second command = %q — the window must be [offset, offset+limit-1]", fake.written[1])
	}
}

// gdb answers a bad command with ^error and a message worth showing; the client must surface that
// message rather than a generic failure, because it is usually the actual diagnosis.
func TestMIClientSurfacesGDBError(t *testing.T) {
	cli, _, _ := newMIFake(t, `^error,msg="No symbol \"nope\" in current context."`)
	_, err := cli.evaluate(context.Background(), "1", 0, "nope")
	if err == nil {
		t.Fatal("an ^error must be an error")
	}
	if !strings.Contains(err.Error(), `No symbol "nope" in current context.`) {
		t.Errorf("error = %q, want gdb's own message", err)
	}
}

// When gdb goes away, every caller waiting on a reply has to be released. A client that leaves them
// on a channel nobody will write to hangs the whole page.
func TestMIClientReleasesWaitersWhenGDBDies(t *testing.T) {
	cli, _, _ := newMIFake(t)
	cli.Close()
	select {
	case <-cli.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done was never closed")
	}
	if _, err := cli.exec(context.Background(), "-thread-info"); err == nil {
		t.Fatal("a command after close must fail")
	}
}

// The records below are **real**, captured from gdb reading the 819 MB Percona Server 8.4.5 core
// that crashed in the audit log filter component: `this` in frame 14, opened one level.
//
// Two shapes in them are the reason varChildren is not a thin wrapper around -var-list-children:
//
//   - `private` is not a member. C++ classes hand their children back grouped by access specifier,
//     as untyped nodes, and a panel that renders them shows the word "private" where a field should
//     be and hides six fields behind it.
//   - The base class IS a member, spelled as its own type name, and its path expression is a cast
//     rather than a field access. Both have to survive into something the page can ask for again.
const (
	miVarCreateFixture = `^done,frame={level="0",addr="0x000079019fb6b735",func="pthread_kill",args=[],` +
		`from="/lib64/libpthread.so.0"},name="var1",numchild="2",value="0x38f822e0",` +
		`type="audit_log_filter::log_writer::LogWriter<(audit_log_filter::log_writer::AuditLogHandlerType)0> * const",` +
		`thread-id="1",has_more="0"`
	miVarChildrenFixture = `^done,numchild="2",children=[` +
		`child={name="var1.audit_log_filter::log_writer::LogWriterBase",exp="audit_log_filter::log_writer::LogWriterBase",` +
		`numchild="1",value="{...}",type="audit_log_filter::log_writer::LogWriterBase",thread-id="1"},` +
		`child={name="var1.private",exp="private",numchild="6",value="",thread-id="1"}],has_more="0"`
	miVarBasePathFixture    = `^done,path_expr="(*(class audit_log_filter::log_writer::LogWriterBase*) this)"`
	miVarPrivateKidsFixture = `^done,numchild="6",children=[` +
		`child={name="var1.private.m_is_rotating",exp="m_is_rotating",numchild="0",value="false",type="bool",thread-id="1"},` +
		`child={name="var1.private.m_is_log_empty",exp="m_is_log_empty",numchild="0",value="true",type="bool",thread-id="1"},` +
		`child={name="var1.private.m_is_opened",exp="m_is_opened",numchild="0",value="false",type="bool",thread-id="1"},` +
		`child={name="var1.private.m_file_writer",exp="m_file_writer",numchild="1",value="{...}",` +
		`type="audit_log_filter::log_writer::FileWriterPtr",thread-id="1"},` +
		`child={name="var1.private.m_file_handle",exp="m_file_handle",numchild="1",value="{...}",` +
		`type="audit_log_filter::log_writer::FileHandle",thread-id="1"},` +
		`child={name="var1.private.m_write_lock",exp="m_write_lock",numchild="1",value="{...}",` +
		`type="std::mutex",thread-id="1"}],has_more="0"`
	miVarFieldPathFixture = `^done,path_expr="((this)->m_is_rotating)"`
)

func TestMIVarChildrenOpensAValue(t *testing.T) {
	cli, fake, _ := newMIFake(t,
		miVarCreateFixture, miVarChildrenFixture, miVarBasePathFixture,
		miVarPrivateKidsFixture, miVarFieldPathFixture)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	kids, more, err := cli.varChildren(ctx, "1", 14, "this")
	if err != nil {
		t.Fatalf("varChildren: %v", err)
	}
	if more {
		t.Errorf("seven children is not more than the cap")
	}
	// One base class plus six fields — and no node called "private", which is an access specifier
	// and not something anybody can click on.
	if len(kids) != 7 {
		t.Fatalf("children = %d, want 7: %+v", len(kids), kids)
	}
	for _, k := range kids {
		if k.Name == "private" || k.Name == "public" || k.Name == "protected" {
			t.Fatalf("an access specifier was rendered as a member: %+v", k)
		}
	}
	if kids[0].Name != "audit_log_filter::log_writer::LogWriterBase" {
		t.Errorf("first child = %q, want the base class", kids[0].Name)
	}
	if want := "(*(class audit_log_filter::log_writer::LogWriterBase*) this)"; kids[0].Expr != want {
		t.Errorf("base class expression = %q, want %q", kids[0].Expr, want)
	}
	field := kids[1]
	if field.Name != "m_is_rotating" || field.Value != "false" || field.Type != "bool" {
		t.Errorf("first field = %+v", field)
	}
	// The access it was found under is kept — it is worth showing, it is just not a row.
	if field.Access != "private" {
		t.Errorf("access = %q, want private", field.Access)
	}
	if want := "((this)->m_is_rotating)"; field.Expr != want {
		t.Errorf("field expression = %q, want %q — a member has to be re-readable to be openable",
			field.Expr, want)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !strings.Contains(fake.written[0], "-var-create --thread 1 --frame 14 - * \"this\"") {
		t.Errorf("create = %q — the frame has to be named, or gdb reads the wrong one", fake.written[0])
	}
	// The handle is given back. Without this a session leaks one variable object per click.
	deleted := false
	for _, line := range fake.written {
		if strings.Contains(line, "-var-delete var1") {
			deleted = true
		}
	}
	if !deleted {
		t.Errorf("the variable object was never deleted: %v", fake.written)
	}
}

// gdb's own refusal is the answer worth showing: "Cannot access memory at address 0x0", on a crash
// stack, is the diagnosis rather than a failed request.
func TestMIVarChildrenSurfacesGDBRefusal(t *testing.T) {
	cli, _, _ := newMIFake(t, `^error,msg="-var-create: unable to create variable object"`)
	_, _, err := cli.varChildren(context.Background(), "1", 13, "nosuchthing")
	if err == nil || !strings.Contains(err.Error(), "unable to create variable object") {
		t.Fatalf("error = %v, want gdb's own message", err)
	}
}

// $_siginfo is read out of the core's own note, and it is the one fact that says WHICH pointer
// faulted. The value comes back as a bare address because the expression casts it to void*.
func TestMISiginfoReadsTheFaultingAddress(t *testing.T) {
	cli, fake, _ := newMIFake(t, `^done,value="11"`, `^done,value="1"`, `^done,value="0x30"`)
	si := cli.siginfo(context.Background())
	if si == nil {
		t.Fatal("siginfo = nil")
	}
	if si.Signo != 11 || si.Code != 1 {
		t.Errorf("signo/code = %d/%d, want 11/1", si.Signo, si.Code)
	}
	if !si.HasAddr || si.Addr != 0x30 {
		t.Errorf("addr = %#x (has=%v), want 0x30", si.Addr, si.HasAddr)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	// No --thread/--frame: these belong to the inferior, and asking for them in a frame is how you
	// get "No frame selected" on a core whose threads have not been walked yet.
	if strings.Contains(fake.written[0], "--frame") {
		t.Errorf("siginfo was read in a frame: %q", fake.written[0])
	}
}

// A core with no siginfo note answers nothing rather than an error: plenty of cores (gcore's, for
// one) have no signal at all, and that is not a failure to report.
func TestMISiginfoAbsentIsNotAnError(t *testing.T) {
	cli, _, _ := newMIFake(t, `^error,msg="No symbol \"_siginfo\" in current context."`)
	if si := cli.siginfo(context.Background()); si != nil {
		t.Fatalf("siginfo = %+v, want nil", si)
	}
}
