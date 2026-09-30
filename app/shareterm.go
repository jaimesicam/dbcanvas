package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// shareterm.go — terminals everyone in a shared session sees.
//
// A node terminal (terminal.go) is one browser, one exec. A shared terminal is one
// exec and many viewers: the shell's output goes to every browser in the session,
// with a scrollback so somebody arriving late sees what they missed, and keystrokes
// are taken only from whoever drives at that moment. Control moving is therefore
// also the keyboard moving — the next keystroke from the old driver is dropped.

const shareTermScrollback = 256 << 10

type sharedTerm struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	StackID int64  `json:"stackId"`
	NodeID  string `json:"nodeId"`
	Opener  string `json:"opener"`

	hub     *shareHub
	app     *App
	stream  *ExecConn
	execCtx context.Context // carries the node's engine, for resize
	cancel  context.CancelFunc

	mu      sync.Mutex
	buf     []byte
	viewers map[*termViewer]bool
	closed  bool
}

type termViewer struct {
	guestID int64
	out     chan []byte
	cancel  context.CancelFunc
}

// termList is the open terminals, for a browser arriving. Caller holds h.mu.
func (h *shareHub) termList() []*sharedTerm {
	out := make([]*sharedTerm, 0, len(h.terms))
	for _, t := range h.terms {
		out = append(out, t)
	}
	return out
}

// pump copies the shell's output to the scrollback and every viewer, until the
// shell ends.
func (t *sharedTerm) pump() {
	defer t.close()
	buf := make([]byte, 4096)
	for {
		n, err := t.stream.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			t.mu.Lock()
			t.buf = append(t.buf, chunk...)
			if len(t.buf) > shareTermScrollback {
				t.buf = t.buf[len(t.buf)-shareTermScrollback:]
			}
			for v := range t.viewers {
				select {
				case v.out <- chunk:
				default:
					v.cancel() // too slow to follow; it can reconnect for the scrollback
				}
			}
			t.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// close ends the shell and tells everyone. Safe to call more than once.
func (t *sharedTerm) close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	for v := range t.viewers {
		v.cancel()
	}
	t.mu.Unlock()
	t.cancel()
	t.stream.Close()
	t.hub.mu.Lock()
	delete(t.hub.terms, t.ID)
	t.hub.mu.Unlock()
	t.hub.broadcast(map[string]any{"t": "terminal-close", "id": t.ID}, nil)
}

// handleShareTermOpen opens a shared terminal on a node: the driver, or the host. It
// is the same act as opening a terminal of your own, done in front of everyone.
func (a *App) handleShareTermOpen(w http.ResponseWriter, r *http.Request) {
	sess, c, ok := a.shareCaller(w, r)
	if !ok {
		return
	}
	h := a.hubFor(sess)
	h.mu.Lock()
	driving := h.controller == c.guestID
	h.mu.Unlock()
	// The host's terminals are always shared, driving or not: it is their session.
	if !driving && !c.isHost() {
		writeErr(w, http.StatusForbidden, "only whoever has control can open a terminal")
		return
	}
	var in struct {
		StackID   int64  `json:"stackId"`
		NodeID    string `json:"nodeId"`
		Title     string `json:"title"`
		User      string `json:"user"`
		Namespace string `json:"namespace"`
		Pod       string `json:"pod"`
		Container string `json:"container"`
		Shell     string `json:"shell"`
	}
	if err := decode(r, &in); err != nil || in.StackID == 0 || in.NodeID == "" {
		writeErr(w, http.StatusBadRequest, "stackId and nodeId are required")
		return
	}
	// Resolve the node exactly as the terminal socket does, through the same
	// ownership check and the same shell: a request shaped like the socket's.
	q := url.Values{}
	for k, v := range map[string]string{"user": in.User, "namespace": in.Namespace, "pod": in.Pod,
		"container": in.Container, "shell": in.Shell} {
		if v != "" {
			q.Set(k, v)
		}
	}
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = q.Encode()
	r2.SetPathValue("id", strconv.FormatInt(in.StackID, 10))
	r2.SetPathValue("nid", in.NodeID)
	dep, _, ok := a.loadRunningNode(w, r2)
	if !ok {
		return
	}
	cmd, env, user, status, err := nodeShellSpec(r2, dep)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	st, _ := a.store.GetStack(dep.StackID)
	ctx, cancel := context.WithCancel(withEngine(context.Background(), a.depEngine(st, dep.NodeID)))
	stream, err := a.engCtx(ctx).HijackExec(ctx, dep.ContainerID, cmd, env, user)
	if err != nil {
		cancel()
		writeErr(w, http.StatusBadGateway, "could not start a shell on that node")
		return
	}
	title := in.Title
	if title == "" {
		title = in.NodeID
	}
	h.mu.Lock()
	h.termSeq++
	t := &sharedTerm{ID: fmt.Sprintf("t%d", h.termSeq), Title: title, StackID: in.StackID, NodeID: in.NodeID,
		Opener: c.name, hub: h, app: a, stream: stream, execCtx: ctx, cancel: cancel, viewers: map[*termViewer]bool{}}
	h.terms[t.ID] = t
	h.mu.Unlock()
	go t.pump()
	h.event("terminal", c.guestID, c.name, fmt.Sprintf("%s opened a terminal on %s", c.name, title))
	h.broadcast(map[string]any{"t": "terminal-open", "terminal": t}, nil)
	writeJSON(w, http.StatusOK, t)
}

func (a *App) shareTerm(w http.ResponseWriter, r *http.Request) (*shareHub, *sharedTerm, *hubClient, bool) {
	sess, c, ok := a.shareCaller(w, r)
	if !ok {
		return nil, nil, nil, false
	}
	h := a.hubFor(sess)
	h.mu.Lock()
	t := h.terms[r.PathValue("tid")]
	h.mu.Unlock()
	if t == nil {
		writeErr(w, http.StatusNotFound, "that terminal is closed")
		return nil, nil, nil, false
	}
	return h, t, c, true
}

// handleShareTermClose closes a shared terminal for everyone. The driver's call alone.
func (a *App) handleShareTermClose(w http.ResponseWriter, r *http.Request) {
	h, t, c, ok := a.shareTerm(w, r)
	if !ok {
		return
	}
	h.mu.Lock()
	driving := h.controller == c.guestID
	h.mu.Unlock()
	// Only the driver: a viewer, the host included while someone else drives, cannot
	// take a terminal off everyone's screen.
	if !driving {
		writeErr(w, http.StatusForbidden, "only whoever has control can close a shared terminal")
		return
	}
	t.close()
	writeJSON(w, http.StatusOK, map[string]any{"status": "closed"})
}

// handleShareTermWS is one browser watching a shared terminal — and typing into it,
// while it is theirs to type into.
func (a *App) handleShareTermWS(w http.ResponseWriter, r *http.Request) {
	h, t, c, ok := a.shareTerm(w, r)
	if !ok {
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := &termViewer{guestID: c.guestID, out: make(chan []byte, 512), cancel: cancel}

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "closed")
		return
	}
	back := append([]byte(nil), t.buf...)
	t.viewers[v] = true
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.viewers, v)
		t.mu.Unlock()
	}()

	go func() {
		defer cancel()
		if len(back) > 0 {
			if conn.Write(ctx, websocket.MessageBinary, back) != nil {
				return
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-v.out:
				wctx, wc := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(wctx, websocket.MessageBinary, b)
				wc()
				if err != nil {
					return
				}
			}
		}
	}()

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		// Checked per message rather than once: control can move mid-session, and the
		// keyboard moves with it.
		h.mu.Lock()
		driving := h.controller == c.guestID
		h.mu.Unlock()
		if !driving {
			continue
		}
		if typ == websocket.MessageText {
			var msg struct {
				Type       string `json:"type"`
				Cols, Rows int
			}
			if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" {
				a.engCtx(t.execCtx).ResizeExec(t.execCtx, t.stream.ExecID, msg.Cols, msg.Rows)
				continue
			}
		}
		if _, err := t.stream.Write(data); err != nil {
			return
		}
	}
}
