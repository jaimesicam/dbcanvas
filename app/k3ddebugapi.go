package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// k3ddebugapi.go — the Operator Debugger's HTTP surface: what there is to debug, the operator's
// source, the live session over a WebSocket, and the one button that makes a reconcile happen.
//
// The session (k3ddebugsess.go) is the state; this is only transport. Two shapes:
//
//   - **Plain JSON** for the things a page loads once — the list of debuggable frames, the source
//     tree, a file's contents.
//   - **One WebSocket** for the session itself, because a debugger is not request/response: the
//     operator stops when it stops, and the panel has to redraw when it does. Commands go up with
//     an id and come back as replies; state and log lines arrive unsolicited.
//
// Everything is scoped to a stack the caller owns (loadOwnedStack), which is the same gate the web
// terminal and the file manager use. It is worth being explicit about why that gate is enough and
// also strictly necessary: a debugger is remote code execution by design — it can read any memory
// in the operator process, and with function calls turned on, run its code. Anyone who can reach
// this can already open a root shell on the same node, so the boundary is the stack, not the
// feature.

// k3dDebugPreset is a "quick breakpoint": somewhere in this operator worth stopping, by name, so
// nobody has to go and find the line. The names are Delve function-breakpoint specs, and were
// taken from the 1.20.0 source rather than guessed.
type k3dDebugPreset struct {
	Label string `json:"label"`
	Func  string `json:"func"`
	Hint  string `json:"hint"`
}

// k3dDebugPresets is per operator, like everything else that knows what an operator's code looks
// like. Only PXC is debuggable today (k3dDebuggableOperator); the rest of the machinery is
// operator-agnostic, so the day another one is verified this is where its landmarks go.
var k3dDebugPresets = map[string][]k3dDebugPreset{
	"pxc": {
		{Label: "Reconcile", Func: "pxc.(*ReconcilePerconaXtraDBCluster).Reconcile",
			Hint: "the main loop — every change to the cluster comes through here"},
		{Label: "deploy", Func: "pxc.(*ReconcilePerconaXtraDBCluster).deploy",
			Hint: "creating or updating the PXC and proxy StatefulSets"},
		{Label: "smartUpdate", Func: "pxc.(*ReconcilePerconaXtraDBCluster).smartUpdate",
			Hint: "the rolling restart: which pod goes next, and why"},
		{Label: "reconcileUsers", Func: "pxc.(*ReconcilePerconaXtraDBCluster).reconcileUsers",
			Hint: "the system users' passwords and grants"},
		{Label: "Backup Reconcile", Func: "pxcbackup.(*ReconcilePerconaXtraDBClusterBackup).Reconcile",
			Hint: "a PerconaXtraDBClusterBackup being acted on"},
		{Label: "Restore Reconcile", Func: "pxcrestore.(*ReconcilePerconaXtraDBClusterRestore).Reconcile",
			Hint: "a PerconaXtraDBClusterRestore being acted on"},
	},
}

// k3dDebugStartFile is where the panel opens: the file whose Reconcile is the thing everyone comes
// here to see.
var k3dDebugStartFile = map[string]string{
	"pxc": "pkg/controller/pxc/controller.go",
}

// k3dDebugCRKind is the custom resource a forced reconcile annotates — the operator's own short
// name, which is what the CRD registers.
var k3dDebugCRKind = map[string]string{
	"pxc": "pxc", "ps": "ps", "psmdb": "psmdb", "pg": "pg",
}

// ---------------------------------------------------------------- context

// k3dDebugContext resolves the stack, the design and the frame's debugger for a request, writing
// the HTTP error itself when any of it is missing.
func (a *App) k3dDebugContext(w http.ResponseWriter, r *http.Request) (Stack, k3dDebugTarget, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return Stack{}, k3dDebugTarget{}, false
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return Stack{}, k3dDebugTarget{}, false
	}
	tgt, err := a.k3dDebugResolveTarget(st, doc, r.PathValue("fid"))
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return Stack{}, k3dDebugTarget{}, false
	}
	return st, tgt, true
}

// ---------------------------------------------------------------- targets

// handleK3DDebugTargets lists every frame the caller can debug, across all their stacks — the page
// is a tool of its own, not a tab on one cluster, so it has to find its own targets the way the
// Packet Inspector and the Query Runner do.
func (a *App) handleK3DDebugTargets(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	type target struct {
		k3dDebugTarget
		Presets   []k3dDebugPreset `json:"presets"`
		StartFile string           `json:"startFile"`
		Attached  bool             `json:"attached"`
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
		for _, f := range doc.Frames {
			if f.Type != "k3d" || !f.K3DDebug {
				continue
			}
			tgt, err := a.k3dDebugResolveTarget(st, doc, f.ID)
			if err != nil {
				continue // not running, or its debugger never came up — not a target
			}
			t := target{k3dDebugTarget: tgt, Presets: k3dDebugPresets[tgt.Operator],
				StartFile: k3dDebugStartFile[tgt.Operator]}
			if sess, ok := a.debugSessions.Load(tgt.key()); ok {
				t.Attached = sess.(*k3dDebugSession).client() != nil
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

// ---------------------------------------------------------------- source

// handleK3DDebugSources lists the operator's Go files. They are read off the k3s server node,
// where the release tarball was already unpacked for bundle.yaml and cr.yaml — the same source the
// binary was built from, so a line number in the panel is the line number Delve resolves.
//
// Nothing is cached: it is one exec, and a cache that outlives a redeploy would show the wrong
// release's source, which is the one failure mode that matters here.
func (a *App) handleK3DDebugSources(w http.ResponseWriter, r *http.Request) {
	_, tgt, ok := a.k3dDebugContext(w, r)
	if !ok {
		return
	}
	files, err := a.k3dDebugListSources(r.Context(), tgt)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files": files, "root": tgt.SrcDir, "buildDir": tgt.BuildDir,
		"startFile": k3dDebugStartFile[tgt.Operator],
	})
}

// k3dDebugListSources runs find on the node. busybox find has no -printf, so the paths are made
// relative by cd-ing first — and the k3s image does have an sh to cd with.
func (a *App) k3dDebugListSources(ctx context.Context, tgt k3dDebugTarget) ([]string, error) {
	if tgt.SrcDir == "" {
		return nil, fmt.Errorf("this frame did not record where the operator source is")
	}
	res, err := a.engCtx(ctx).Exec(ctx, tgt.ServerID, []string{"sh", "-c",
		"cd " + shellQuote(tgt.SrcDir) + " && find . -type f -name '*.go'"}, nil)
	if err != nil {
		return nil, fmt.Errorf("list the operator source: %w", err)
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("list the operator source: %s", strings.TrimSpace(res.Stderr))
	}
	out := []string{}
	for _, ln := range strings.Split(res.Stdout, "\n") {
		p := strings.TrimPrefix(strings.TrimSpace(ln), "./")
		if p == "" || strings.HasPrefix(p, "vendor/") || strings.Contains(p, "/vendor/") {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// handleK3DDebugSource returns one file, with its line count — the panel numbers the gutter from
// it and sets breakpoints by line.
func (a *App) handleK3DDebugSource(w http.ResponseWriter, r *http.Request) {
	_, tgt, ok := a.k3dDebugContext(w, r)
	if !ok {
		return
	}
	rel, err := k3dDebugCleanRel(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := a.k3dDebugReadSource(r.Context(), tgt, rel)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": rel, "buildPath": k3dDebugBuildPath(tgt.BuildDir, rel), "content": body,
	})
}

// k3dDebugSourceMax caps a file read. The operator's largest generated file is a few MiB of
// zz_generated deepcopy; anything past this is not source anybody is stepping through.
const k3dDebugSourceMax = 4 << 20

func (a *App) k3dDebugReadSource(ctx context.Context, tgt k3dDebugTarget, rel string) (string, error) {
	full := strings.TrimSuffix(tgt.SrcDir, "/") + "/" + rel
	res, err := a.engCtx(ctx).Exec(ctx, tgt.ServerID, []string{"sh", "-c",
		"head -c " + strconv.Itoa(k3dDebugSourceMax) + " " + shellQuote(full)}, nil)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", rel, err)
	}
	if res.Code != 0 {
		return "", fmt.Errorf("read %s: %s", rel, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// k3dDebugCleanRel confines a requested path to the operator's source tree. The caller already
// owns the stack (and could read the file through the file manager), so this is not the security
// boundary — it is here so a typo cannot turn a source view into a way to cat /etc/shadow through
// a path this feature had no business accepting.
func k3dDebugCleanRel(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("no path")
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("the path must be relative to the operator's source")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return "", fmt.Errorf("invalid path %q", p)
		}
	}
	return p, nil
}

// ---------------------------------------------------------------- force a reconcile

// handleK3DDebugReconcile annotates the custom resource, which is what makes the operator run
// Reconcile now rather than at its next resync. It is the other half of a breakpoint: without it
// you set one and wait.
func (a *App) handleK3DDebugReconcile(w http.ResponseWriter, r *http.Request) {
	_, tgt, ok := a.k3dDebugContext(w, r)
	if !ok {
		return
	}
	if err := a.k3dDebugForceReconcile(r.Context(), tgt); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cr": tgt.CR})
}

func (a *App) k3dDebugForceReconcile(ctx context.Context, tgt k3dDebugTarget) error {
	kind := k3dDebugCRKind[tgt.Operator]
	if kind == "" {
		return fmt.Errorf("no custom resource to annotate for %s", k3dOperatorLabel(tgt.Operator))
	}
	// The value only has to change; the timestamp makes it obvious in `kubectl get -o yaml` who
	// asked and when. --overwrite because it is set again every time.
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	if _, err := a.kubectl(ctx, tgt.ServerID, "-n", tgt.Namespace, "annotate", kind, tgt.CR,
		"dbcanvas-debug="+stamp, "--overwrite"); err != nil {
		return fmt.Errorf("annotate %s/%s: %w", kind, tgt.CR, err)
	}
	return nil
}

// ---------------------------------------------------------------- the session socket

// k3dDebugWSCmd is one command from the panel. Everything optional, because one shape covers all
// of them and a debugger's commands are small.
type k3dDebugWSCmd struct {
	ID      int    `json:"id"`
	Cmd     string `json:"cmd"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	On      bool   `json:"on,omitempty"`
	Name    string `json:"name,omitempty"`
	Ref     int    `json:"ref,omitempty"`
	FrameID int    `json:"frameId,omitempty"`
	Expr    string `json:"expr,omitempty"`
	Seconds int    `json:"seconds,omitempty"`
	Value   string `json:"value,omitempty"`
}

// handleK3DDebugWS is the session: it attaches on connect, forwards everything the session
// publishes, and runs the commands the panel sends.
//
// Several browsers may hold this open at once and they all drive the same session — Delve serves
// one DAP client at a time, so sharing one connection between viewers is what makes a second tab
// (or a colleague) work instead of erroring.
func (a *App) handleK3DDebugWS(w http.ResponseWriter, r *http.Request) {
	_, tgt, ok := a.k3dDebugContext(w, r)
	if !ok {
		return
	}
	sess := a.k3dDebugSessionFor(tgt)

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	events, unsubscribe := sess.subscribe()
	defer unsubscribe()

	// One writer. Session events and command replies both end up here, because a WebSocket
	// tolerates exactly one writer at a time.
	out := make(chan []byte, 128)
	go func() {
		for {
			select {
			case buf, live := <-events:
				if !live {
					cancel()
					return
				}
				select {
				case out <- buf:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Attaching can take a moment (a dial and a handshake), so it runs off the read loop and the
	// panel sees "attaching" in the meantime.
	go func() {
		if sess.client() == nil {
			sess.attach(ctx)
		}
	}()

	go func() {
		defer cancel()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var cmd k3dDebugWSCmd
			if json.Unmarshal(data, &cmd) != nil {
				continue
			}
			// Each command in its own goroutine: `variables` on a big struct must not hold up
			// the Continue the user pressed while waiting for it.
			go func() {
				data, err := a.k3dDebugRun(ctx, sess, tgt, cmd)
				if cmd.ID == 0 {
					return // fire-and-forget; the state broadcast is the answer
				}
				reply := map[string]any{"type": "reply", "id": cmd.ID, "ok": err == nil}
				if err != nil {
					reply["error"] = err.Error()
				} else if data != nil {
					reply["data"] = data
				}
				select {
				case out <- mustJSON(reply):
				case <-ctx.Done():
				}
			}()
		}
	}()

	for {
		select {
		case buf := <-out:
			if err := c.Write(ctx, websocket.MessageText, buf); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// k3dDebugRun executes one command. Returns the data for its reply, if it has any.
func (a *App) k3dDebugRun(ctx context.Context, sess *k3dDebugSession, tgt k3dDebugTarget, cmd k3dDebugWSCmd) (any, error) {
	switch cmd.Cmd {
	case "attach":
		return nil, sess.attach(ctx)
	case "detach":
		sess.teardown("you asked to detach")
		return nil, nil
	case "continue", "pause", "next", "stepIn", "stepOut":
		return nil, sess.exec(ctx, cmd.Cmd)
	case "breakpoint":
		return nil, sess.setBreakpoint(ctx, cmd.File, cmd.Line, cmd.On)
	case "fnbreakpoint":
		return nil, sess.setFunctionBreakpoint(ctx, cmd.Name, cmd.On)
	case "clearBreakpoints":
		sess.clearBreakpoints(ctx)
		return nil, nil
	case "scopes":
		scopes, err := sess.scopes(ctx, cmd.FrameID)
		return map[string]any{"scopes": scopes}, err
	case "variables":
		vars, err := sess.variables(ctx, cmd.Ref)
		return map[string]any{"variables": vars}, err
	case "evaluate":
		v, err := sess.evaluate(ctx, cmd.Expr, cmd.FrameID)
		return map[string]any{"result": v}, err
	case "setVariable":
		v, err := sess.setVariable(ctx, cmd.Ref, cmd.Name, cmd.Value)
		return map[string]any{"result": v}, err
	case "allowCalls":
		sess.setAllowCalls(cmd.On)
		return nil, nil
	case "idle":
		sess.setIdle(cmd.Seconds)
		return nil, nil
	case "reconcile":
		if err := a.k3dDebugForceReconcile(ctx, tgt); err != nil {
			return nil, err
		}
		sess.logf("info", "annotated %s/%s — the operator will reconcile now", k3dDebugCRKind[tgt.Operator], tgt.CR)
		return nil, nil
	case "state":
		return map[string]any{"state": sess.snapshot()}, nil
	}
	return nil, fmt.Errorf("unknown command %q", cmd.Cmd)
}
