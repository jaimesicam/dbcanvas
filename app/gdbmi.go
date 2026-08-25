package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// gdbmi.go — a client for GDB's machine interface.
//
// MI is to gdb what DAP is to Delve, and this file is the sibling of k3ddap.go: a request/reply
// client over a stream, with unsolicited records arriving in between. Driving gdb's *human*
// interface instead — sending "bt" and scraping the answer — is the obvious alternative and the
// wrong one: its output has no schema, changes between releases, and cannot be told apart from a
// program's own output. MI is a documented grammar with a machine on the other end of it.
//
// The transport is one exec in the node container (HijackExec, the same call the web terminal
// makes). That exec has a TTY, which MI does not want and cannot switch off itself, so gdb is
// started behind `stty -echo -onlcr`:
//
//   - **-echo**, or every command we send is handed straight back to us. MI echoes are in fact
//     distinguishable from output (a record's first character after the token is one of ^*+=~@&,
//     never `-`), so this is belt and braces — but a parser that has to know which lines are its
//     own reflection is a parser with a bug in it.
//   - **-onlcr**, or the terminal driver turns every \n in gdb's output into \r\n and every record
//     arrives with a trailing carriage return. The reader trims \r anyway, for the same reason.
//
// Everything here is about *reading* a core file: there is no run, no continue, no breakpoints.
// gdb against a core is a database of a dead process, and the whole API surface below is queries.

// miRecordClass is the leading character of an MI output record.
const (
	miResultRecord = '^' // <token>^done, ^error, ^exit …
	miExecAsync    = '*' // *stopped …
	miStatusAsync  = '+'
	miNotifyAsync  = '=' // =library-loaded, =thread-group-added …
	miConsoleOut   = '~' // what the CLI would have printed
	miTargetOut    = '@'
	miLogOut       = '&' // gdb's own log, which is where its warnings go
)

// miResult is one reply to one command.
type miResult struct {
	Class   string         // "done" | "running" | "connected" | "error" | "exit"
	Payload map[string]any // everything after the class
}

// miHandlers are the callbacks for records that are nobody's reply.
type miHandlers struct {
	// Async says something happened: "stopped", "library-loaded", "thread-created" …
	Async func(class string, payload map[string]any)
	// Stream is gdb talking. The log stream (&) is where the sentences that matter live —
	// "could not find debug symbols", "exec file does not match" — so it is not noise.
	Stream func(kind byte, text string)
}

// miClient is a live gdb speaking MI.
type miClient struct {
	w  io.Writer
	c  io.Closer
	on miHandlers

	mu      sync.Mutex
	next    int
	pending map[int]chan miResult
	err     error

	done chan struct{}
}

// newMIClient wraps a stream on which gdb --interpreter=mi3 is already running and starts reading.
func newMIClient(rw io.ReadWriteCloser, on miHandlers) *miClient {
	c := &miClient{w: rw, c: rw, on: on, pending: map[int]chan miResult{}, done: make(chan struct{})}
	go c.pump(rw)
	return c
}

// Close ends the session. Every caller waiting on a reply is released with an error rather than
// left on a channel nobody will ever write to.
func (c *miClient) Close() error {
	err := c.c.Close()
	c.fail(fmt.Errorf("the gdb session was closed"))
	return err
}

// Done is closed when gdb has gone away.
func (c *miClient) Done() <-chan struct{} { return c.done }

func (c *miClient) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
	c.mu.Unlock()
}

// pump reads records until the stream ends.
//
// The scanner's buffer is raised a long way past the default 64 KiB on purpose: a single MI record
// can be very large — `-stack-list-frames` on the recursive core this was built for is one line
// carrying several hundred frames — and a scanner that gives up on a long line drops exactly the
// backtrace you were reading.
func (c *miClient) pump(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		c.record(strings.TrimRight(sc.Text(), "\r"))
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	c.fail(fmt.Errorf("gdb ended: %w", err))
}

// record dispatches one line.
func (c *miClient) record(line string) {
	if line == "" || line == "(gdb)" || line == "(gdb) " {
		return
	}
	// A leading decimal token belongs to a reply.
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	token, hasToken := 0, i > 0
	if hasToken {
		token, _ = strconv.Atoi(line[:i])
	}
	if i >= len(line) {
		return
	}
	kind, rest := line[i], line[i+1:]

	switch kind {
	case miConsoleOut, miTargetOut, miLogOut:
		if c.on.Stream != nil {
			s, _ := miCString(rest)
			c.on.Stream(kind, s)
		}
	case miResultRecord:
		class, payload := miClassAndPayload(rest)
		c.mu.Lock()
		ch := c.pending[token]
		delete(c.pending, token)
		c.mu.Unlock()
		if ch != nil {
			ch <- miResult{Class: class, Payload: payload}
			close(ch)
		}
	case miExecAsync, miStatusAsync, miNotifyAsync:
		class, payload := miClassAndPayload(rest)
		if c.on.Async != nil {
			c.on.Async(class, payload)
		}
	}
}

// miClassAndPayload splits "done,threads=[...]" into its class and its results.
func miClassAndPayload(s string) (string, map[string]any) {
	class, rest, _ := strings.Cut(s, ",")
	payload := map[string]any{}
	p := &miParser{s: rest}
	for {
		p.ws()
		if p.eof() {
			break
		}
		k, v, ok := p.result()
		if !ok {
			break
		}
		payload[k] = v
		p.ws()
		if !p.accept(',') {
			break
		}
	}
	return strings.TrimSpace(class), payload
}

// ---------------------------------------------------------------- commands

// exec sends one MI command and waits for its reply.
//
// An ^error comes back as a Go error carrying gdb's own message, because gdb's message is always
// the more useful one: "No symbol table is loaded" and "No thread selected" are answers, not
// failures of this client.
func (c *miClient) exec(ctx context.Context, cmd string) (map[string]any, error) {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.next++
	id := c.next
	ch := make(chan miResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if _, err := fmt.Fprintf(c.w, "%d%s\n", id, cmd); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("write to gdb: %w", err)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case res, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("gdb ended before answering %s", cmd)
		}
		if res.Class == "error" {
			msg, _ := res.Payload["msg"].(string)
			if msg == "" {
				msg = "gdb refused the command"
			}
			return res.Payload, fmt.Errorf("%s", msg)
		}
		return res.Payload, nil
	}
}

// console runs a plain gdb command and returns what the CLI would have printed.
//
// Some things have no MI spelling at all — `info sharedlibrary`, `thread apply all bt`, the whole
// `set` family — and this is how they are reached. The console output arrives as ~ stream records
// while the command runs, so they are collected around it.
func (c *miClient) console(ctx context.Context, cmd string) (string, error) {
	var sb strings.Builder
	stop := c.captureConsole(&sb)
	defer stop()
	_, err := c.exec(ctx, `-interpreter-exec console `+miQuote(cmd))
	return sb.String(), err
}

// captureConsole tees the console stream into sb until the returned function is called. Only one
// capture can be active at a time, which is enough because the session serialises commands.
func (c *miClient) captureConsole(sb *strings.Builder) func() {
	c.mu.Lock()
	prev := c.on.Stream
	var mu sync.Mutex
	c.on.Stream = func(kind byte, text string) {
		if kind == miConsoleOut {
			mu.Lock()
			sb.WriteString(text)
			mu.Unlock()
		}
		if prev != nil {
			prev(kind, text)
		}
	}
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.on.Stream = prev
		c.mu.Unlock()
	}
}

// miQuote renders a Go string as an MI c-string argument.
func miQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ---------------------------------------------------------------- typed queries

// miThread is one thread in the core.
type miThread struct {
	ID     string   `json:"id"`
	Target string   `json:"target"`         // "Thread 0x7f… (LWP 9756)"
	Name   string   `json:"name,omitempty"` // the thread's own name, when it set one
	Frame  *miFrame `json:"frame,omitempty"`
	Signal string   `json:"signal,omitempty"` // set on the thread that took it
}

// miFrame is one stack frame.
type miFrame struct {
	Level  int     `json:"level"`
	Addr   string  `json:"addr"`
	Func   string  `json:"func"`
	File   string  `json:"file,omitempty"` // only with debug symbols
	Line   int     `json:"line,omitempty"`
	From   string  `json:"from,omitempty"` // the object, when there is no source info
	Args   []miVar `json:"args,omitempty"`
	Repeat int     `json:"repeat,omitempty"` // set by the caller when frames are collapsed
}

// miVar is one argument or local.
type miVar struct {
	Name  string `json:"name"`
	Type  string `json:"type,omitempty"`
	Value string `json:"value"`
	Arg   bool   `json:"arg,omitempty"`
}

func (c *miClient) threads(ctx context.Context) ([]miThread, string, error) {
	p, err := c.exec(ctx, "-thread-info")
	if err != nil {
		return nil, "", err
	}
	current, _ := p["current-thread-id"].(string)
	out := []miThread{}
	for _, item := range miList(p["threads"]) {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		t := miThread{
			ID:     miStr(m["id"]),
			Target: miStr(m["target-id"]),
			Name:   miStr(m["name"]),
		}
		if f, ok := m["frame"].(map[string]any); ok {
			fr := miFrameOf(f)
			t.Frame = &fr
		}
		out = append(out, t)
	}
	return out, current, nil
}

// frames reads a thread's backtrace, capped at limit frames from offset.
//
// The cap is not politeness either: the crash this was built for is unbounded recursion, several
// hundred frames of it, and a stack with no ceiling is a request that never returns on a deeper one.
func (c *miClient) frames(ctx context.Context, thread string, offset, limit int) ([]miFrame, error) {
	cmd := fmt.Sprintf("-stack-list-frames --thread %s %d %d", thread, offset, offset+limit-1)
	p, err := c.exec(ctx, cmd)
	if err != nil {
		return nil, err
	}
	out := []miFrame{}
	for _, item := range miList(p["stack"]) {
		// -stack-list-frames returns a list of frame=... results, which the parser hands back
		// as single-entry maps.
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if inner, ok := m["frame"].(map[string]any); ok {
			m = inner
		}
		out = append(out, miFrameOf(m))
	}
	return out, nil
}

// variables reads one frame's arguments and locals in a single call.
func (c *miClient) variables(ctx context.Context, thread string, frame int) ([]miVar, error) {
	cmd := fmt.Sprintf("-stack-list-variables --thread %s --frame %d --all-values", thread, frame)
	p, err := c.exec(ctx, cmd)
	if err != nil {
		return nil, err
	}
	out := []miVar{}
	for _, item := range miList(p["variables"]) {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, miVar{
			Name:  miStr(m["name"]),
			Type:  miStr(m["type"]),
			Value: miStr(m["value"]),
			Arg:   miStr(m["arg"]) == "1",
		})
	}
	return out, nil
}

// evaluate reads one expression in a frame.
func (c *miClient) evaluate(ctx context.Context, thread string, frame int, expr string) (string, error) {
	cmd := fmt.Sprintf("-data-evaluate-expression --thread %s --frame %d %s", thread, frame, miQuote(expr))
	p, err := c.exec(ctx, cmd)
	if err != nil {
		return "", err
	}
	return miStr(p["value"]), nil
}

// disassemble returns the selected frame's instructions. It is what the source pane shows when
// there is no source on the node to show instead — which, for a released server binary, is always.
func (c *miClient) disassemble(ctx context.Context, thread string, frame int) ([]string, error) {
	if _, err := c.exec(ctx, fmt.Sprintf("-stack-select-frame --thread %s %d", thread, frame)); err != nil {
		return nil, err
	}
	out, err := c.console(ctx, "disassemble")
	if err != nil {
		return nil, err
	}
	lines := []string{}
	for _, ln := range strings.Split(out, "\n") {
		if ln = strings.TrimRight(ln, " \t"); ln != "" {
			lines = append(lines, ln)
		}
	}
	return lines, nil
}

// miFrameOf converts one MI frame tuple.
func miFrameOf(m map[string]any) miFrame {
	f := miFrame{
		Addr: miStr(m["addr"]),
		Func: miStr(m["func"]),
		File: miStr(m["fullname"]),
		From: miStr(m["from"]),
	}
	if f.File == "" {
		f.File = miStr(m["file"])
	}
	f.Level, _ = strconv.Atoi(miStr(m["level"]))
	f.Line, _ = strconv.Atoi(miStr(m["line"]))
	for _, item := range miList(m["args"]) {
		am, ok := item.(map[string]any)
		if !ok {
			continue
		}
		f.Args = append(f.Args, miVar{Name: miStr(am["name"]), Value: miStr(am["value"]), Arg: true})
	}
	return f
}

// miStr reads a string out of a parsed value, tolerating the absent case.
func miStr(v any) string {
	s, _ := v.(string)
	return s
}

// miList reads a list out of a parsed value. A tuple is returned as a one-element list, because MI
// collapses a single-element list into a tuple in a few places and a caller iterating either way
// should not have to care.
func miList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		return []any{t}
	case nil:
		return nil
	}
	return nil
}

// ---------------------------------------------------------------- the value grammar

// miParser reads MI's value grammar: c-strings, {tuples} and [lists], nested arbitrarily.
type miParser struct {
	s string
	i int
}

func (p *miParser) eof() bool { return p.i >= len(p.s) }

func (p *miParser) ws() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *miParser) accept(c byte) bool {
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

// result reads "name=value", or a bare value where MI allows one.
//
// The bare case has to be recognised *first*, from the opening character. A list of tuples —
// threads=[{id="1",…},{id="2",…}], which is how -thread-info answers — otherwise has its first
// element read as a name: the scan below stops at the `=` inside the tuple and produces a result
// called `{id` whose value is `"1"`, silently, for every element. That was the shape of the first
// version of this parser, and the real records captured from gdb are what found it.
func (p *miParser) result() (string, any, bool) {
	p.ws()
	if !p.eof() {
		switch p.s[p.i] {
		case '{', '[', '"':
			v, ok := p.value()
			return "", v, ok
		}
	}
	start := p.i
	for p.i < len(p.s) && p.s[p.i] != '=' && p.s[p.i] != ',' && p.s[p.i] != '}' && p.s[p.i] != ']' {
		p.i++
	}
	name := strings.TrimSpace(p.s[start:p.i])
	if !p.accept('=') {
		// A bare value inside a list: rewind and read it as one.
		p.i = start
		v, ok := p.value()
		return "", v, ok
	}
	v, ok := p.value()
	return name, v, ok
}

func (p *miParser) value() (any, bool) {
	p.ws()
	if p.eof() {
		return nil, false
	}
	switch p.s[p.i] {
	case '"':
		s, n := miCString(p.s[p.i:])
		p.i += n
		return s, true
	case '{':
		p.i++
		return p.tuple('}')
	case '[':
		p.i++
		return p.tuple(']')
	}
	// An unquoted token — gdb emits these for a few enum-ish fields.
	start := p.i
	for p.i < len(p.s) && p.s[p.i] != ',' && p.s[p.i] != '}' && p.s[p.i] != ']' {
		p.i++
	}
	return strings.TrimSpace(p.s[start:p.i]), true
}

// tuple reads the body of a {…} or […] up to close.
//
// MI does not distinguish the two structurally: both can hold bare values *or* name=value results,
// and a list of results is how -stack-list-frames returns a stack. So the shape is decided by what
// is inside — all-named becomes a map, anything else a slice — and miList papers over the rest.
func (p *miParser) tuple(close byte) (any, bool) {
	named := map[string]any{}
	var items []any
	anyBare := false
	for {
		p.ws()
		if p.eof() {
			break
		}
		if p.accept(close) {
			break
		}
		name, v, ok := p.result()
		if !ok {
			break
		}
		if name == "" {
			anyBare = true
			items = append(items, v)
		} else {
			// Every named result is recorded twice over: once by name, for the tuple case, and
			// once positionally, for the list case. Which of the two is returned is decided at
			// the end — a repeated name (frame=…,frame=…, which is how a stack arrives) is a
			// list of five frames in `items` and a single last-one-wins entry in `named`.
			named[name] = v
			items = append(items, map[string]any{name: v})
		}
		p.ws()
		if !p.accept(',') {
			p.accept(close)
			break
		}
	}
	if close == ']' || anyBare {
		if items == nil {
			items = []any{}
		}
		return items, true
	}
	return named, true
}

// miCString reads a leading "…" and returns the unescaped contents and how many bytes it consumed.
func miCString(s string) (string, int) {
	if len(s) == 0 || s[0] != '"' {
		return "", 0
	}
	var b strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			default:
				b.WriteByte(s[i])
			}
			i++
			continue
		}
		if c == '"' {
			return b.String(), i + 1
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), i
}

// stackDepth is the real number of frames, which -stack-list-frames never tells you because it is
// always asked for a window. On a runaway recursion the difference between the window and the
// truth is the whole diagnosis.
func (c *miClient) stackDepth(ctx context.Context, thread string) (int, error) {
	p, err := c.exec(ctx, "-stack-info-depth --thread "+thread)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(miStr(p["depth"]))
}

// miFrameArgs is one frame's arguments, as -stack-list-arguments returns them for a range.
type miFrameArgs struct {
	Level int
	Args  []miVar
}

// stackArguments reads the arguments of a range of frames in one call. Used to go looking for the
// input a crash was working on, which lives near the bottom of the stack where nobody scrolls.
func (c *miClient) stackArguments(ctx context.Context, thread string, low, high int) ([]miFrameArgs, error) {
	// `1` is --all-values: names and printed values, which is what makes a query string readable.
	p, err := c.exec(ctx, fmt.Sprintf("-stack-list-arguments --thread %s 1 %d %d", thread, low, high))
	if err != nil {
		return nil, err
	}
	out := []miFrameArgs{}
	for _, item := range miList(p["stack-args"]) {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if inner, ok := m["frame"].(map[string]any); ok {
			m = inner
		}
		fa := miFrameArgs{}
		fa.Level, _ = strconv.Atoi(miStr(m["level"]))
		for _, a := range miList(m["args"]) {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			fa.Args = append(fa.Args, miVar{Name: miStr(am["name"]), Value: miStr(am["value"]), Arg: true})
		}
		out = append(out, fa)
	}
	return out, nil
}
