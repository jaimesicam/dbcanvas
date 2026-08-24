package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// k3ddap.go — a small Debug Adapter Protocol client, enough of one to drive Delve.
//
// This is what lets DBCanvas *be* the debugger instead of handing out a port and a launch.json.
// k3ddebug.go already puts the operator under `dlv exec --headless`; everything here talks to that
// listener the way an IDE would.
//
// Why DAP and not Delve's own JSON-RPC API, which is the more obvious choice for a Go program:
//
//   - Delve serves both on the same port — it peeks the first byte of a connection and picks the
//     protocol — so there is nothing extra to deploy either way.
//   - The JSON-RPC `CreateBreakpoint` **blocks while the target is running**: the in-flight
//     `Continue` holds Delve's target mutex, and a raw RPC probe just hangs. Every client that
//     works (the dlv terminal, the DAP server) halts the target first, sets the breakpoint and
//     resumes. DAP does that dance internally; on the JSON-RPC path it is ours to reimplement, and
//     to get right under a debuggee that may stop on its own at any moment.
//   - DAP is what VS Code drives against this exact deployment, so it is the path that has already
//     been proven live here — including the Windows→Linux source mapping.
//
// The protocol itself is Content-Length-framed JSON, one message per frame, in three shapes:
// requests (client→server, carrying a monotonic `seq`), responses (server→client, quoting the
// request's `seq` in `request_seq`), and events (server→client, unsolicited: `stopped`, `output`,
// `terminated`). Responses and events arrive interleaved on one connection, which is why the read
// loop below is a demultiplexer rather than a read-after-write.
//
// The client is deliberately dumb: it knows about framing, correlation and the handshake, and
// nothing about breakpoints, sessions or operators. What to do with a `stopped` event is
// k3ddebugsess.go's business.

// dapMaxMessage caps a single frame. Delve does not send anything remotely this large — a
// stackTrace of a deep goroutine is tens of KiB — so the limit only exists to keep a garbled or
// hostile Content-Length from making us allocate the machine's memory.
const dapMaxMessage = 32 << 20

// dapCallTimeout is how long a request waits for its response when the caller's context has no
// deadline of its own. Delve answers every request this client sends promptly — including the
// stepping ones, which reply immediately and report the stop later as an event — so a request
// still outstanding after this is a stuck adapter, not a slow one.
const dapCallTimeout = 30 * time.Second

// ---------------------------------------------------------------- wire types

// dapMessage is the envelope every message shares, plus the union of the three shapes' fields.
// One struct rather than three: the frame has to be decoded before its `type` is known, and the
// alternative is decoding twice.
type dapMessage struct {
	Seq  int    `json:"seq"`
	Type string `json:"type"` // "request" | "response" | "event"

	// response
	RequestSeq int             `json:"request_seq,omitempty"`
	Success    bool            `json:"success,omitempty"`
	Command    string          `json:"command,omitempty"`
	Message    string          `json:"message,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`

	// request
	Arguments json.RawMessage `json:"arguments,omitempty"`

	// event
	Event string `json:"event,omitempty"`
}

// The subset of DAP structures this client exchanges. Field names are the protocol's, so the
// json tags are the documentation; only what is actually read is declared.

type dapSource struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"`
}

type dapSourceBreakpoint struct {
	Line      int    `json:"line"`
	Condition string `json:"condition,omitempty"`
}

type dapFunctionBreakpoint struct {
	Name      string `json:"name"`
	Condition string `json:"condition,omitempty"`
}

// dapBreakpoint is what the server says about a breakpoint it was asked to set. Verified=false
// with a Message is the interesting case: the line held no code, or a breakpoint was already
// there (the leftover-breakpoint problem k3ddebug.go's watchdog exists to prevent).
type dapBreakpoint struct {
	ID       int       `json:"id"`
	Verified bool      `json:"verified"`
	Message  string    `json:"message,omitempty"`
	Source   dapSource `json:"source"`
	Line     int       `json:"line"`
}

type dapThread struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type dapStackFrame struct {
	ID     int       `json:"id"`
	Name   string    `json:"name"`
	Source dapSource `json:"source"`
	Line   int       `json:"line"`
	Column int       `json:"column"`
}

type dapScope struct {
	Name               string `json:"name"`
	VariablesReference int    `json:"variablesReference"`
	Expensive          bool   `json:"expensive"`
}

// dapVariable is one value in a scope. VariablesReference != 0 means it has children (a struct's
// fields, a slice's elements) which are fetched with another `variables` request — the protocol's
// way of not serialising an object graph nobody asked for.
type dapVariable struct {
	Name               string `json:"name"`
	Value              string `json:"value"`
	Type               string `json:"type,omitempty"`
	VariablesReference int    `json:"variablesReference"`
	EvaluateName       string `json:"evaluateName,omitempty"`
	IndexedVariables   int    `json:"indexedVariables,omitempty"`
	NamedVariables     int    `json:"namedVariables,omitempty"`
}

// dapStoppedEvent is the one that matters: the debuggee has halted, and everything the UI shows
// (stack, scopes, variables) is fetched in response to it. ThreadID is the goroutine that stopped.
type dapStoppedEvent struct {
	Reason            string `json:"reason"` // breakpoint | step | pause | exception | ...
	Description       string `json:"description,omitempty"`
	Text              string `json:"text,omitempty"`
	ThreadID          int    `json:"threadId"`
	AllThreadsStopped bool   `json:"allThreadsStopped"`
	HitBreakpointIDs  []int  `json:"hitBreakpointIds,omitempty"`
}

type dapOutputEvent struct {
	Category string `json:"category,omitempty"` // stdout | stderr | console
	Output   string `json:"output"`
}

// ---------------------------------------------------------------- the client

// dapClient is one connection to a DAP server. Safe for concurrent use: requests are serialised
// on the write mutex and correlated by seq, so the UI can ask for variables while a `continue` is
// in flight.
type dapClient struct {
	conn net.Conn

	wmu sync.Mutex // guards the writer and seq — one frame at a time on the wire
	w   *bufio.Writer
	seq int

	pmu     sync.Mutex
	pending map[int]chan dapMessage

	onEvent func(name string, body json.RawMessage)

	initOnce sync.Once
	inited   chan struct{} // closed when the server's `initialized` event arrives

	doneOnce sync.Once
	done     chan struct{}
	errMu    sync.Mutex
	err      error
}

// dapDial connects and starts the read loop. onEvent is called from that loop for every event,
// in order, so it must not block for long — the session hands work off to its own goroutine.
func dapDial(ctx context.Context, addr string, onEvent func(name string, body json.RawMessage)) (*dapClient, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to the debugger at %s: %w", addr, err)
	}
	c := &dapClient{
		conn:    conn,
		w:       bufio.NewWriter(conn),
		pending: map[int]chan dapMessage{},
		onEvent: onEvent,
		inited:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	go c.read()
	return c, nil
}

// Done is closed when the connection ends, for whatever reason. Err says why.
func (c *dapClient) Done() <-chan struct{} { return c.done }

func (c *dapClient) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// Close ends the connection. It does not send `disconnect` — that is a protocol decision (and one
// with a dangerous argument; see dapDisconnect), so the session makes it explicitly.
func (c *dapClient) Close() error {
	c.finish(nil)
	return c.conn.Close()
}

// finish records the first reason the connection ended and wakes everything waiting on it.
func (c *dapClient) finish(err error) {
	c.doneOnce.Do(func() {
		c.errMu.Lock()
		if c.err == nil {
			c.err = err
		}
		c.errMu.Unlock()
		close(c.done)
		// Anything still waiting for a response never gets one.
		c.pmu.Lock()
		for seq, ch := range c.pending {
			delete(c.pending, seq)
			close(ch)
		}
		c.pmu.Unlock()
	})
}

// read is the demultiplexer: responses go to whoever is waiting on that seq, events to onEvent.
func (c *dapClient) read() {
	r := bufio.NewReader(c.conn)
	for {
		raw, err := dapReadFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				err = nil // an ordinary end of session, not a failure to report
			}
			c.finish(err)
			return
		}
		var m dapMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			c.finish(fmt.Errorf("the debugger sent a message that is not JSON: %w", err))
			return
		}
		switch m.Type {
		case "response":
			c.pmu.Lock()
			ch, ok := c.pending[m.RequestSeq]
			delete(c.pending, m.RequestSeq)
			c.pmu.Unlock()
			if ok {
				ch <- m
				close(ch)
			}
		case "event":
			// `initialized` is part of the handshake, so it is answered here as well as passed
			// on: the caller of dapAttachRemote waits for it before sending breakpoints.
			if m.Event == "initialized" {
				c.initOnce.Do(func() { close(c.inited) })
			}
			if c.onEvent != nil {
				c.onEvent(m.Event, m.Body)
			}
		}
		// A "request" from the server (reverse request, e.g. runInTerminal) is not something
		// this client offers, and Delve does not send one for an attach. Ignored on purpose:
		// leaving it unanswered is better than answering it wrongly.
	}
}

// dapReadFrame reads one Content-Length-framed message. The header block is CRLF-delimited and
// ends with a blank line; only Content-Length is meaningful (the spec's optional Content-Type is
// accepted and ignored).
func dapReadFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("malformed header %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("malformed Content-Length %q", value)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("a message with no Content-Length")
	}
	if length > dapMaxMessage {
		return nil, fmt.Errorf("a %d-byte message, over the %d-byte limit", length, dapMaxMessage)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// call sends a request and waits for its response. out, when non-nil, receives the response body.
//
// A request that fails carries the server's own message, which is worth surfacing verbatim: Delve
// says things like "unable to find file" or "Unable to evaluate expression: could not find symbol
// value for x", and a wrapper sentence adds nothing to that.
func (c *dapClient) call(ctx context.Context, command string, args, out any) error {
	select {
	case <-c.done:
		return c.closedErr()
	default:
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dapCallTimeout)
		defer cancel()
	}

	ch := make(chan dapMessage, 1)
	c.wmu.Lock()
	c.seq++
	seq := c.seq
	c.pmu.Lock()
	c.pending[seq] = ch
	c.pmu.Unlock()
	err := c.write(seq, command, args)
	c.wmu.Unlock()
	if err != nil {
		c.pmu.Lock()
		delete(c.pending, seq)
		c.pmu.Unlock()
		c.finish(err)
		return err
	}

	select {
	case m, ok := <-ch:
		if !ok {
			return c.closedErr()
		}
		if !m.Success {
			return errors.New(dapErrorText(m, command))
		}
		if out != nil && len(m.Body) > 0 {
			return json.Unmarshal(m.Body, out)
		}
		return nil
	case <-ctx.Done():
		c.pmu.Lock()
		delete(c.pending, seq)
		c.pmu.Unlock()
		return fmt.Errorf("%s: %w", command, ctx.Err())
	case <-c.done:
		return c.closedErr()
	}
}

// dapErrorText is the most useful sentence in a failed response. DAP splits an error in two:
// `message` is a summary ("Unable to evaluate expression") and body.error.format carries what
// actually went wrong ("could not find symbol value for o") — which is the half worth reading,
// so it wins when both are present.
func dapErrorText(m dapMessage, command string) string {
	summary := strings.TrimSpace(m.Message)
	var body struct {
		Error struct {
			Format string `json:"format"`
		} `json:"error"`
	}
	if len(m.Body) > 0 {
		json.Unmarshal(m.Body, &body)
	}
	detail := strings.TrimSpace(body.Error.Format)
	switch {
	case detail != "" && summary != "" && !strings.Contains(detail, summary):
		return summary + ": " + detail
	case detail != "":
		return detail
	case summary != "":
		return summary
	}
	return fmt.Sprintf("the debugger refused %s", command)
}

func (c *dapClient) closedErr() error {
	if err := c.Err(); err != nil {
		return fmt.Errorf("the debugger connection ended: %w", err)
	}
	return errors.New("the debugger connection ended")
}

// write frames one request onto the connection. Called with wmu held.
func (c *dapClient) write(seq int, command string, args any) error {
	body := map[string]any{"seq": seq, "type": "request", "command": command}
	if args != nil {
		body["arguments"] = args
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(buf)); err != nil {
		return err
	}
	if _, err := c.w.Write(buf); err != nil {
		return err
	}
	return c.w.Flush()
}

// waitInitialized blocks until the server has sent `initialized`, which is its way of saying it is
// ready to be configured (breakpoints, then configurationDone).
func (c *dapClient) waitInitialized(ctx context.Context) error {
	select {
	case <-c.inited:
		return nil
	case <-ctx.Done():
		return errors.New("the debugger never reported itself initialized")
	case <-c.done:
		return c.closedErr()
	}
}

// ---------------------------------------------------------------- the requests

// dapInitialize opens the session. The capabilities in the reply are not acted on: Delve's are
// fixed for a given version, and this client only uses requests it implements.
func (c *dapClient) initialize(ctx context.Context) error {
	return c.call(ctx, "initialize", map[string]any{
		"clientID":        "dbcanvas",
		"clientName":      "DBCanvas",
		"adapterID":       "go",
		"pathFormat":      "path",
		"linesStartAt1":   true,
		"columnsStartAt1": true,

		"supportsVariableType": true,
	}, nil)
}

// attachRemote attaches to a debuggee the server is already running — `dlv exec --headless`,
// started by k3ddebug.go's patch. This is the same request VS Code's Go extension sends for a
// remote attach, and the mode Delve requires for a headless instance it did not start for us.
func (c *dapClient) attachRemote(ctx context.Context) error {
	return c.call(ctx, "attach", map[string]any{"mode": "remote"}, nil)
}

// configurationDone ends the configuration phase. Sent after the breakpoints, per the protocol's
// order — an operator under `--continue` is already running, so this changes nothing about whether
// it runs; it is what makes the breakpoints live.
func (c *dapClient) configurationDone(ctx context.Context) error {
	return c.call(ctx, "configurationDone", map[string]any{}, nil)
}

// setBreakpoints replaces *all* breakpoints in one source file. That is the protocol's model, not
// a convenience: there is no "add one" request, so the caller keeps the file's full set and sends
// it every time — and an empty list is how a file's breakpoints are cleared.
func (c *dapClient) setBreakpoints(ctx context.Context, path string, lines []dapSourceBreakpoint) ([]dapBreakpoint, error) {
	if lines == nil {
		lines = []dapSourceBreakpoint{}
	}
	var out struct {
		Breakpoints []dapBreakpoint `json:"breakpoints"`
	}
	err := c.call(ctx, "setBreakpoints", map[string]any{
		"source":      dapSource{Name: pathBase(path), Path: path},
		"breakpoints": lines,
	}, &out)
	return out.Breakpoints, err
}

// setFunctionBreakpoints is the same idea by function name — how the "quick breakpoints" work,
// so nobody has to find the line `Reconcile` starts on. Delve accepts a package-qualified name
// with a receiver, e.g. (*ReconcilePerconaXtraDBCluster).Reconcile.
func (c *dapClient) setFunctionBreakpoints(ctx context.Context, fns []dapFunctionBreakpoint) ([]dapBreakpoint, error) {
	if fns == nil {
		fns = []dapFunctionBreakpoint{}
	}
	var out struct {
		Breakpoints []dapBreakpoint `json:"breakpoints"`
	}
	err := c.call(ctx, "setFunctionBreakpoints", map[string]any{"breakpoints": fns}, &out)
	return out.Breakpoints, err
}

// threads lists the debuggee's goroutines (Delve maps goroutines onto DAP threads).
func (c *dapClient) threads(ctx context.Context) ([]dapThread, error) {
	var out struct {
		Threads []dapThread `json:"threads"`
	}
	err := c.call(ctx, "threads", map[string]any{}, &out)
	return out.Threads, err
}

func (c *dapClient) stackTrace(ctx context.Context, threadID, levels int) ([]dapStackFrame, error) {
	var out struct {
		StackFrames []dapStackFrame `json:"stackFrames"`
	}
	err := c.call(ctx, "stackTrace", map[string]any{
		"threadId":   threadID,
		"startFrame": 0,
		"levels":     levels,
	}, &out)
	return out.StackFrames, err
}

func (c *dapClient) scopes(ctx context.Context, frameID int) ([]dapScope, error) {
	var out struct {
		Scopes []dapScope `json:"scopes"`
	}
	err := c.call(ctx, "scopes", map[string]any{"frameId": frameID}, &out)
	return out.Scopes, err
}

func (c *dapClient) variables(ctx context.Context, ref int) ([]dapVariable, error) {
	var out struct {
		Variables []dapVariable `json:"variables"`
	}
	err := c.call(ctx, "variables", map[string]any{"variablesReference": ref}, &out)
	return out.Variables, err
}

// evaluate runs an expression in the given frame. Context is the protocol's hint for what the
// result is for; "watch" is the read-only one, and it is what this client sends.
func (c *dapClient) evaluate(ctx context.Context, expr string, frameID int) (dapVariable, error) {
	var out struct {
		Result             string `json:"result"`
		Type               string `json:"type"`
		VariablesReference int    `json:"variablesReference"`
	}
	args := map[string]any{"expression": expr, "context": "watch"}
	if frameID > 0 {
		args["frameId"] = frameID
	}
	err := c.call(ctx, "evaluate", args, &out)
	return dapVariable{
		Name:               expr,
		Value:              out.Result,
		Type:               out.Type,
		VariablesReference: out.VariablesReference,
	}, err
}

// The execution commands. Each replies immediately; the debuggee's new state arrives later as a
// `stopped` event (or does not, for continue, until something stops it).
func (c *dapClient) cont(ctx context.Context, threadID int) error {
	return c.call(ctx, "continue", map[string]any{"threadId": threadID}, nil)
}

func (c *dapClient) next(ctx context.Context, threadID int) error {
	return c.call(ctx, "next", map[string]any{"threadId": threadID}, nil)
}

func (c *dapClient) stepIn(ctx context.Context, threadID int) error {
	return c.call(ctx, "stepIn", map[string]any{"threadId": threadID}, nil)
}

func (c *dapClient) stepOut(ctx context.Context, threadID int) error {
	return c.call(ctx, "stepOut", map[string]any{"threadId": threadID}, nil)
}

func (c *dapClient) pause(ctx context.Context, threadID int) error {
	return c.call(ctx, "pause", map[string]any{"threadId": threadID}, nil)
}

// dapDisconnect ends the session and leaves the debuggee running.
//
// terminateDebuggee is the field to be careful with, and it is why this is a named function rather
// than an inline map: `true` kills the process Delve is debugging — which here means dlv exits,
// and dlv *is* the container's process, so the operator pod restarts. VS Code's Stop button sends
// exactly that. Nothing in DBCanvas ever should.
func (c *dapClient) dapDisconnect(ctx context.Context) error {
	return c.call(ctx, "disconnect", map[string]any{
		"restart":           false,
		"terminateDebuggee": false,
		"suspendDebuggee":   false,
	}, nil)
}

// pathBase is filepath.Base for the debuggee's paths, which are always slash-separated (the
// binary was cross-compiled on Linux) whatever the app happens to be running on.
func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
