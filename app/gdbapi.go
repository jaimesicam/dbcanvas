package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/coder/websocket"
)

// gdbapi.go — the Core Dump Analyzer's HTTP surface: which nodes can analyse a core, what is in
// the mounted directory, and the live gdb over a WebSocket.
//
// Same two shapes as the Operator Debugger's API, for the same reasons. Plain JSON for the things
// a page loads once (the target list, the core listing); one WebSocket for the session, because
// reading a stack is a conversation and because the state a page draws is the session's, not the
// request's.
//
// Everything is gated on loadRunningNode — the same check the web terminal and the file manager
// use. It is the right boundary and worth saying why: gdb runs *in* the node, and gdb is a
// programmable debugger. Anyone who can reach this can already open a root shell on the same
// container, which is what makes the gate sufficient; it is also what makes it necessary.

// handleGDBTargets lists every Linux Client, across all the caller's stacks, that was deployed for
// core-dump analysis. The page is a tool of its own rather than a tab on one node, so — like the
// Operator Debugger and the Packet Inspector — it finds its own targets.
func (a *App) handleGDBTargets(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	type target struct {
		gdbTarget
		Open bool `json:"open"` // a core is loaded in a live session right now
	}
	out := []target{}
	stacks, _ := a.store.ListStacks(u.ID, u.Role == RoleAdmin)
	for _, s := range stacks {
		st, err := a.store.GetStack(s.ID)
		if err != nil {
			continue
		}
		var doc designDoc
		if json.Unmarshal(st.Design, &doc) != nil {
			continue
		}
		for _, n := range doc.Nodes {
			if n.Type != "linuxclient" || !n.GDBEnabled {
				continue
			}
			tgt, err := a.gdbResolveTarget(st, n)
			if err != nil {
				continue // not running, or it was not deployed for this
			}
			t := target{gdbTarget: tgt}
			if sess, ok := a.gdbSessions.Load(tgt.key()); ok {
				t.Open = sess.(*gdbSession).client() != nil
			}
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StackName != out[j].StackName {
			return out[i].StackName < out[j].StackName
		}
		return out[i].Label < out[j].Label
	})
	writeJSON(w, http.StatusOK, map[string]any{"targets": out})
}

// gdbResolveTarget turns a deployed Linux Client into a target, or says which of the ways it can
// be unavailable applies — they need different answers, so they are different sentences.
func (a *App) gdbResolveTarget(st Stack, n designNode) (gdbTarget, error) {
	dep, err := a.store.GetDeployment(st.ID, n.ID)
	if err != nil || dep.State != DeployRunning {
		return gdbTarget{}, fmt.Errorf("node %s is not running", n.Label)
	}
	var cfg struct {
		linuxClientConfig
		gdbNodeConfig
	}
	if json.Unmarshal(dep.Config, &cfg) != nil {
		return gdbTarget{}, fmt.Errorf("node %s has no readable config", n.Label)
	}
	if !cfg.gdbNodeConfig.Enabled {
		return gdbTarget{}, fmt.Errorf(
			"%s was not deployed for core-dump analysis — tick it on the node and deploy again", n.Label)
	}
	return gdbTarget{
		StackID: st.ID, StackName: st.Name, NodeID: n.ID, Label: n.Label,
		Hostname: cfg.Hostname, OS: cfg.OS, OSVersion: n.OSVersion,
		Product: cfg.Product, Major: cfg.Major, Version: cfg.Version,
		Binary: cfg.Binary, BinaryFrom: cfg.BinaryFrom, BuildID: cfg.BuildID, HasSymbols: cfg.HasSyms,
		CoreDir: cfg.CoreDir, LibDir: cfg.LibDir, Status: cfg.gdbNodeConfig.Status,
		ContainerID: dep.ContainerID,
	}, nil
}

// gdbContext resolves the node for a request, writing the HTTP error itself when anything is
// missing.
func (a *App) gdbContext(w http.ResponseWriter, r *http.Request) (gdbTarget, bool) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return gdbTarget{}, false
	}
	st, err := a.store.GetStack(dep.StackID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "stack not found")
		return gdbTarget{}, false
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return gdbTarget{}, false
	}
	for _, n := range doc.Nodes {
		if n.ID != dep.NodeID {
			continue
		}
		tgt, err := a.gdbResolveTarget(st, n)
		if err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return gdbTarget{}, false
		}
		return tgt, true
	}
	writeErr(w, http.StatusNotFound, "node not found in the design")
	return gdbTarget{}, false
}

// handleGDBCores lists the mounted core directory, with the verdict on each file.
//
// This is the step the one-line gdb recipe has nowhere to put, and it is the step that decides
// whether anything that follows is true. gdb prints a backtrace whether or not the libraries are
// the ones the process was running and whether or not the binary is the build that crashed; only
// the build-ids can tell the difference, and only before the stack is on screen is anybody going
// to read the answer.
func (a *App) handleGDBCores(w http.ResponseWriter, r *http.Request) {
	tgt, ok := a.gdbContext(w, r)
	if !ok {
		return
	}
	cores, err := a.gdbListCores(r.Context(), tgt.ContainerID, tgt.BuildID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cores": cores, "target": tgt,
		"coreMount": gdbCoreMount, "sysrootMount": gdbSysrootMount,
	})
}

// ---------------------------------------------------------------- the session socket

// gdbWSCmd is one command from the page. One shape covers all of them, as in the debugger.
type gdbWSCmd struct {
	ID     int    `json:"id"`
	Cmd    string `json:"cmd"`
	Core   string `json:"core,omitempty"`
	Thread string `json:"thread,omitempty"`
	Frame  int    `json:"frame,omitempty"`
	Offset int    `json:"offset,omitempty"`
	Expr   string `json:"expr,omitempty"`
	Text   string `json:"text,omitempty"`
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Span   int    `json:"span,omitempty"`
	On     bool   `json:"on,omitempty"`
}

func (a *App) handleGDBWS(w http.ResponseWriter, r *http.Request) {
	tgt, ok := a.gdbContext(w, r)
	if !ok {
		return
	}
	sess := a.gdbSessionFor(tgt)

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(withEngine(context.Background(), a.depEngine(Stack{ID: tgt.StackID}, tgt.NodeID)))
	defer cancel()

	events, unsub := sess.subscribe()
	defer unsub()

	// One writer, fed by both the session's broadcasts and this socket's command replies — two
	// goroutines writing to a WebSocket is a data race the library will not save you from.
	out := make(chan []byte, 64)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case b, ok := <-events:
				if !ok {
					return
				}
				select {
				case out <- b:
				default:
				}
			}
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-out:
				if err := c.Write(ctx, websocket.MessageText, b); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var cmd gdbWSCmd
		if json.Unmarshal(data, &cmd) != nil {
			continue
		}
		// Each command in its own goroutine: opening a core takes tens of seconds, and a page
		// that cannot ask anything else meanwhile is a page that looks broken.
		go func(cmd gdbWSCmd) {
			res, err := a.gdbRun(ctx, sess, tgt, cmd)
			if cmd.ID == 0 {
				return // fire-and-forget; the state broadcast is the answer
			}
			reply := map[string]any{"type": "reply", "id": cmd.ID, "ok": err == nil}
			if err != nil {
				reply["error"] = err.Error()
			} else if res != nil {
				reply["data"] = res
			}
			select {
			case out <- mustJSON(reply):
			case <-ctx.Done():
			}
		}(cmd)
	}
}

// gdbRun dispatches one command. An unknown one is an error rather than a silent no-op — a page
// and a server that disagree about the protocol should say so.
func (a *App) gdbRun(ctx context.Context, sess *gdbSession, tgt gdbTarget, cmd gdbWSCmd) (any, error) {
	switch cmd.Cmd {
	case "state":
		return map[string]any{"state": sess.snapshot()}, nil

	case "open":
		return nil, sess.open(ctx, cmd.Core)

	case "close":
		sess.teardown("closed from the page")
		return nil, nil

	case "backtrace":
		frames, more, err := sess.backtrace(ctx, cmd.Thread, cmd.Offset)
		if err != nil {
			return nil, err
		}
		sess.publishState()
		return map[string]any{"frames": frames, "more": more, "offset": cmd.Offset}, nil

	case "variables":
		vars, err := sess.variables(ctx, cmd.Thread, cmd.Frame)
		if err != nil {
			return nil, err
		}
		if vars == nil {
			vars = []miVar{}
		}
		return map[string]any{"variables": vars}, nil

	case "evaluate":
		val, err := sess.evaluate(ctx, cmd.Thread, cmd.Frame, cmd.Expr)
		if err != nil {
			return nil, err
		}
		return map[string]any{"value": val}, nil

	case "disassemble":
		lines, err := sess.disassemble(ctx, cmd.Thread, cmd.Frame)
		if err != nil {
			return nil, err
		}
		return map[string]any{"lines": lines}, nil

	case "source":
		span := cmd.Span
		if span <= 0 || span > 200 {
			span = 24
		}
		lines, from, err := a.gdbReadSource(ctx, tgt.ContainerID, cmd.File, cmd.Line, span)
		if err != nil {
			return nil, err
		}
		return map[string]any{"lines": lines, "from": from, "line": cmd.Line, "file": cmd.File}, nil

	case "console":
		out, err := sess.console(ctx, cmd.Text)
		if err != nil {
			return nil, err
		}
		return map[string]any{"output": out}, nil

	case "allowShell":
		sess.setAllowShell(cmd.On)
		return nil, nil

	case "cores":
		cores, err := a.gdbListCores(ctx, tgt.ContainerID, tgt.BuildID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"cores": cores}, nil
	}
	return nil, fmt.Errorf("unknown command %q", cmd.Cmd)
}

// gdbCrashSummary reduces a loaded core to the two or three sentences somebody actually wants:
// what the signal was, which thread took it, and the first frame that belongs to the program
// rather than to libc.
//
// "The top frame" is almost never the answer. A stack overflow surfaces inside _int_malloc, an
// assertion inside abort, a bad free inside libc's allocator — in every case the frame that names
// the bug is the first one below the C library. Picking it is a heuristic, so the page shows the
// whole stack next to it rather than instead of it.
func gdbCrashSummary(frames []miFrame) (culprit *miFrame, recursion string) {
	for i := range frames {
		f := frames[i]
		if f.Func == "" || f.Func == "??" {
			continue
		}
		if f.From != "" && gdbSystemObject(f.From) {
			continue
		}
		culprit = &frames[i]
		break
	}
	// A stack that is entirely C library — a crash inside malloc with no program frames above it —
	// still has to name something. The top frame is a poor answer, but it is an answer, and
	// silence would read as "the tool could not tell".
	if culprit == nil && len(frames) > 0 {
		culprit = &frames[0]
	}
	for _, f := range frames {
		if f.Repeat > 2 {
			recursion = fmt.Sprintf("%s repeats %d times", shortFuncName(f.Func), f.Repeat)
			break
		}
	}
	return culprit, recursion
}

// gdbSystemObject reports whether a shared object is part of the C runtime rather than the program.
//
// The names have to be matched on their *stem*, because both spellings turn up and which one you
// get depends on where the library came from. A distribution's own /lib64 holds the soname —
// libc.so.6 — while a flat copy taken off the crashed host holds the real file, libc-2.28.so.
// Matching only the first form is how the frame that surfaces a stack overflow (`_int_malloc`,
// from libc) gets reported as the program's own code, which is precisely backwards.
func gdbSystemObject(from string) bool {
	base := from
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	// libc-2.28.so -> libc; libc.so.6 -> libc; ld-linux-x86-64.so.2 -> ld
	stem := base
	if i := strings.IndexAny(stem, "-."); i > 0 {
		stem = stem[:i]
	}
	switch stem {
	case "libc", "libpthread", "ld", "libm", "libgcc_s", "libstdc++", "librt", "libdl":
		return true
	}
	return false
}

// shortFuncName drops a C++ signature's arguments, which are most of its length and none of its
// meaning in a one-line summary.
//
// The argument list is the first `(` at template-bracket depth zero, not just the first `(` in
// the string — a non-type template parameter of enum type prints as `<(some::Enum)0>` (seen on
// PS-9668's real core, on a template instantiated with an enum value: `LogRecordFormatter<(...)0>`),
// and the naive scan truncated the name mid-template, before the class name it was templated on.
func shortFuncName(fn string) string {
	// "(anonymous namespace)::" is C++'s own name for internal linkage — a real prefix, not an
	// argument list, but its parenthesis sits at position zero, before the depth tracking below
	// has scanned anything. PXC-3848's real core has exactly this: rewrite_query is declared in
	// an anonymous namespace, and the naive scan returned "" for the whole name — which then
	// rendered as a blank entry in the middle of an otherwise-readable recursion cycle.
	const anonNS = "(anonymous namespace)::"
	if strings.HasPrefix(fn, anonNS) {
		return anonNS + shortFuncName(fn[len(anonNS):])
	}
	depth := 0
	for i, r := range fn {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		case '(':
			if depth == 0 {
				return fn[:i]
			}
		}
	}
	return fn
}
