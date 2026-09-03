package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/term"
)

// cmd_node.go — the per-node commands, including the console.
//
// The console is the one place the CLI does something the browser cannot do better,
// and it works for a specific reason: `Authorization` is a normal header on a
// WebSocket handshake from a Go client, even though a browser cannot set one. That
// is why the server takes the token from the header and offers no `?token=`
// parameter — a query string ends up in access logs and shell history, and the
// browser does not need one because it has a cookie.

func cmdNode(args []string) error {
	return sub("node", args, map[string]func([]string) error{
		"list":    nodeList,
		"start":   nodeAction("start"),
		"stop":    nodeAction("stop"),
		"restart": nodeAction("restart"),
		"console": nodeConsole,
		"exec":    nodeExec,
		"cp":      nodeCp,
		"tunnel":  nodeTunnel,
	}, []string{"list", "start", "stop", "restart", "console", "exec", "cp", "tunnel"})
}

func nodeList(args []string) error {
	fs := flagsFor("node list")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a stack name or id"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	st, err := resolveStack(c, arg(0))
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get(fmt.Sprintf("/api/stacks/%d", st.ID), &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	var d stackDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	if len(d.Deployments) == 0 {
		empty("deployed nodes in "+d.Name, "Deploy it first: `dbcanvas stack deploy "+d.Name+" --wait`.")
		return nil
	}
	// NAME first, because it is the thing every panel and the compose plan shows and
	// the thing every other node command now takes. The id stays visible so an old
	// script that uses one still has somewhere to read it from.
	var doc struct {
		Nodes []designNodeRef `json:"nodes"`
	}
	_ = json.Unmarshal(d.Design, &doc)
	labels := map[string]string{}
	for _, n := range doc.Nodes {
		labels[n.ID] = n.Label
	}
	t := newTable("name", "node id", "state", "container")
	for _, dep := range d.Deployments {
		cid := dep.ContainerID
		if len(cid) > 12 {
			cid = cid[:12]
		}
		name := labels[dep.NodeID]
		if name == "" {
			name = "-"
		}
		t.add(name, dep.NodeID, dep.State, cid)
	}
	t.print()
	return nil
}

// nodeAction builds start/stop/restart, which differ only in the verb.
func nodeAction(action string) func([]string) error {
	return func(args []string) error {
		fs := flagsFor("node " + action)
		if err := parse(fs, args); err != nil {
			return err
		}
		if err := need(2, "a stack and a node: dbcanvas node "+action+" <stack> <node>"); err != nil {
			return err
		}
		c, st, node, err := stackAndNode(arg(0), arg(1))
		if err != nil {
			return err
		}
		if err := c.post(fmt.Sprintf("/api/stacks/%d/nodes/%s/%s",
			st.ID, url.PathEscape(node), action), nil, nil); err != nil {
			return err
		}
		fmt.Printf("%s: %s %sed.\n", st.Name, arg(1), strings.TrimSuffix(action, "e"))
		if action == "restart" {
			// Docker hands out a new ephemeral host port on every start, so any
			// address printed before the restart is now wrong.
			fmt.Println("  Published ports may have moved — re-read the node to get them.")
		}
		return nil
	}
}

// nodeConsole bridges the terminal to a node's shell over the WebSocket.
func nodeConsole(args []string) error {
	fs := flagsFor("node console")
	user := fs.String("user", "", "exec as this uid or username (default: the image's user)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(2, "a stack and a node: dbcanvas node console <stack> <node>"); err != nil {
		return err
	}
	c, st, node, err := stackAndNode(arg(0), arg(1))
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/api/stacks/%d/nodes/%s/term", st.ID, url.PathEscape(node))
	if *user != "" {
		path += "?user=" + url.QueryEscape(*user)
	}
	return runConsole(c, path, fmt.Sprintf("%s/%s", st.Name, node))
}

// runConsole is the raw-mode bridge, shared by any endpoint that speaks the terminal
// protocol (binary frames are bytes, text frames are control messages).
func runConsole(c *Client, path, label string) error {
	target, err := c.wsURL(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, target, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Token}},
	})
	if err != nil {
		// The handshake failure carries the same JSON errors as any other request,
		// and reporting it as one is far more useful than "bad handshake".
		if resp != nil {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			msg := strings.TrimSpace(string(raw))
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(raw, &e) == nil && e.Error != "" {
				msg = e.Error
			}
			if msg == "" {
				msg = fmt.Sprintf("the server refused the connection (%d)", resp.StatusCode)
			}
			return &APIError{Status: resp.StatusCode, Message: msg, Method: "GET", Path: path}
		}
		return fmt.Errorf("cannot open a console on %s: %w", label, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		// Raw mode, or the remote shell never sees a keystroke until Enter and
		// Ctrl-C kills the CLI instead of the remote process.
		state, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("cannot put the terminal in raw mode: %w", err)
		}
		defer term.Restore(fd, state)

		sendSize := func() {
			w, h, err := term.GetSize(fd)
			if err != nil || w <= 0 || h <= 0 {
				return
			}
			msg, _ := json.Marshal(map[string]any{"type": "resize", "Cols": w, "Rows": h})
			conn.Write(ctx, websocket.MessageText, msg)
		}
		sendSize()
		// Keep the remote shell reflowing when the window is resized. How that is
		// noticed is platform-specific (see console_unix.go / console_windows.go),
		// which is not an implementation detail worth having here.
		stopResize := watchResize(fd, sendSize)
		defer stopResize()
	} else {
		fmt.Fprintln(os.Stderr, "dbcanvas: stdin is not a terminal; running non-interactively")
	}

	// container → terminal
	done := make(chan error, 1)
	go func() {
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				done <- err
				return
			}
			if typ == websocket.MessageBinary || typ == websocket.MessageText {
				if _, err := os.Stdout.Write(data); err != nil {
					done <- err
					return
				}
			}
		}
	}()

	// terminal → container
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					done <- werr
					return
				}
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()

	err = <-done
	cancel()
	// A closed connection is how a console ends — the user typed exit, or closed
	// the shell. Reporting that as a failure would make every normal session exit
	// non-zero.
	if err == nil || err == io.EOF ||
		websocket.CloseStatus(err) == websocket.StatusNormalClosure ||
		websocket.CloseStatus(err) == websocket.StatusGoingAway {
		return nil
	}
	if strings.Contains(err.Error(), "use of closed") || strings.Contains(err.Error(), "EOF") {
		return nil
	}
	return err
}

// nodeExec runs one command and prints its output, without a TTY.
func nodeExec(args []string) error {
	fs := flagsFor("node exec")
	if err := parse(fs, args); err != nil {
		return err
	}
	// parse() consumes the `--` and keeps everything after it verbatim, so the
	// remote command's own flags (`mysql -e 'SHOW …'`) arrive intact.
	if nargs() < 3 {
		fmt.Fprintln(os.Stderr,
			"dbcanvas: usage: dbcanvas node exec <stack> <node> -- <command> [args…]")
		return errUsage
	}
	cmdArgs := positional[2:]
	c, st, node, err := stackAndNode(arg(0), arg(1))
	if err != nil {
		return err
	}
	// There is no exec endpoint in the API: the console WebSocket is the exec
	// mechanism, and this is it with the command piped in and stdin closed after.
	// Said plainly rather than pretending otherwise, because the exit status of the
	// remote command is not available this way.
	fmt.Fprintln(os.Stderr, "dbcanvas: note — exec runs through the console, so the remote exit status is not reported")
	path := fmt.Sprintf("/api/stacks/%d/nodes/%s/term", st.ID, url.PathEscape(node))
	return execOverConsole(c, path, shellJoin(cmdArgs))
}

// shellJoin turns argv into one command line that a remote shell will split back into
// exactly the same argv.
//
// This has to re-quote, and the reason is the sentence above: exec has no endpoint of
// its own, so the command is TYPED INTO a login shell over the console. Joining with
// plain spaces threw every argument boundary away, and the remote shell then made up
// its own — `printf '[%s]\n' 'one two' three` arrived as five words with the
// backslash eaten, and, worse, an argument containing a metacharacter ran as a second
// command: `echo 'harmless; id -un'` printed "harmless" and then "root". A command
// that silently becomes a different command is the worst failure this tool could
// have, and it made both documented examples wrong.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return strings.Join(out, " ")
}

// shellQuote makes one argument survive a POSIX shell.
//
// Single quotes protect everything a shell would otherwise act on, and the one
// character they cannot contain is a single quote — which leaves the quoted run, is
// escaped, and re-enters it. Arguments that need no protection are returned as they
// are, so the command echoed back by the console stays readable instead of becoming a
// wall of quotes.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool { return !shellSafeRune(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellSafeRune reports whether a rune carries no meaning to a shell. Deliberately a
// small allow-list: anything not named here gets quoted, which is the safe direction
// to be wrong in.
func shellSafeRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("@%+=:,./-_", r)
}

// execOverConsole sends one command line, then EOF, and relays what comes back.
func execOverConsole(c *Client, path, line string) error {
	target, err := c.wsURL(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, target, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Token}},
	})
	if err != nil {
		if resp != nil {
			resp.Body.Close()
			return &APIError{Status: resp.StatusCode, Message: "the server refused the console connection", Path: path}
		}
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	// `exit` after the command so the shell closes and the read loop ends by
	// itself, rather than the CLI having to guess when the output has finished.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte(line+"\nexit\n")); err != nil {
		return err
	}
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return nil
		}
		if typ == websocket.MessageBinary || typ == websocket.MessageText {
			os.Stdout.Write(data)
		}
	}
}

// nodeCp copies a file in or out. The direction is inferred from which argument
// carries a stack:node: prefix.
func nodeCp(args []string) error {
	fs := flagsFor("node cp")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(2, "a source and a destination: dbcanvas node cp <src> <dst>\n"+
		"  where one side is <stack>:<node>:<path>"); err != nil {
		return err
	}
	src, dst := arg(0), arg(1)
	srcRemote, srcRef := parseNodeRef(src)
	dstRemote, dstRef := parseNodeRef(dst)

	c, err := mustClient()
	if err != nil {
		return err
	}
	switch {
	case srcRemote && dstRemote:
		// Node-to-node has a dedicated endpoint that never round-trips through
		// this machine, which for a multi-gigabyte file is the whole difference.
		return cpNodeToNode(c, srcRef, dstRef)
	case dstRemote:
		return cpUp(c, src, dstRef)
	case srcRemote:
		return cpDown(c, srcRef, dst)
	default:
		fmt.Fprintln(os.Stderr,
			"dbcanvas: one side must be <stack>:<node>:<path> — otherwise this is just `cp`")
		return errUsage
	}
}

type nodeRef struct{ stack, node, path string }

// parseNodeRef splits stack:node:/path.
//
// Two things are deliberately not node references: anything with fewer than three
// colon-separated parts, and a Windows drive path. The drive test is narrow — a
// single *letter* followed by a slash — because a stack id is often a single digit,
// and rejecting "1:node-1:/tmp" as a drive letter would break `cp` for exactly the
// stacks people address by id.
func parseNodeRef(s string) (bool, nodeRef) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return false, nodeRef{}
	}
	if len(parts[0]) == 1 && isASCIILetter(parts[0][0]) &&
		(strings.HasPrefix(parts[1], `\`) || strings.HasPrefix(parts[1], "/")) {
		return false, nodeRef{} // C:\Users\… or C:/Users/… — a drive, not a stack
	}
	return true, nodeRef{stack: parts[0], node: parts[1], path: parts[2]}
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func cpUp(c *Client, localPath string, dst nodeRef) error {
	st, err := resolveStack(c, dst.stack)
	if err != nil {
		return err
	}
	// The id goes in the path; dst.node stays as typed for what gets printed back.
	dstID, err := resolveNode(c, st.ID, dst.node)
	if err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", localPath, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory — copy files one at a time, or tar it first", localPath)
	}
	name := fi.Name()
	dir := dst.path
	// A trailing slash, or an existing directory, means "into here" — the same
	// convention cp uses.
	if strings.HasSuffix(dir, "/") {
		dir = strings.TrimSuffix(dir, "/")
	} else if base := pathBase(dir); base != "" && strings.Contains(base, ".") {
		name = base
		dir = pathDir(dir)
	}
	if err := uploadFile(c, st.ID, dstID, dir, name, f, fi.Size()); err != nil {
		return err
	}
	fmt.Printf("Copied %s to %s:%s/%s (%s).\n", localPath, dst.node, dir, name, humanBytes(fi.Size()))
	return nil
}

func cpDown(c *Client, src nodeRef, localPath string) error {
	st, err := resolveStack(c, src.stack)
	if err != nil {
		return err
	}
	srcID, err := resolveNode(c, st.ID, src.node)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/stacks/%d/nodes/%s/fs/download?path=%s",
		st.ID, url.PathEscape(srcID), url.QueryEscape(src.path))
	written, err := c.download(path, localPath)
	if err != nil {
		return err
	}
	fmt.Printf("Copied %s:%s to %s.\n", src.node, src.path, written)
	return nil
}

func cpNodeToNode(c *Client, src, dst nodeRef) error {
	if src.stack != dst.stack {
		return fmt.Errorf("both nodes have to be in the same stack (%s vs %s)", src.stack, dst.stack)
	}
	st, err := resolveStack(c, src.stack)
	if err != nil {
		return err
	}
	srcID, err := resolveNode(c, st.ID, src.node)
	if err != nil {
		return err
	}
	dstID, err := resolveNode(c, st.ID, dst.node)
	if err != nil {
		return err
	}
	if err := c.post(fmt.Sprintf("/api/stacks/%d/nodes/%s/fs/transfer", st.ID, url.PathEscape(srcID)),
		map[string]any{"path": src.path, "targetNodeId": dstID, "targetPath": dst.path}, nil); err != nil {
		return err
	}
	fmt.Printf("Copied %s:%s to %s:%s.\n", src.node, src.path, dst.node, dst.path)
	return nil
}

func nodeTunnel(args []string) error {
	fs := flagsFor("node tunnel")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(2, "a stack and a node: dbcanvas node tunnel <stack> <node>"); err != nil {
		return err
	}
	c, st, node, err := stackAndNode(arg(0), arg(1))
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get(fmt.Sprintf("/api/stacks/%d/nodes/%s/sshforward",
		st.ID, url.PathEscape(node)), &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	var res struct {
		Command string         `json:"command"`
		Ports   map[string]int `json:"ports"`
	}
	json.Unmarshal(raw, &res)
	if res.Command == "" {
		return fmt.Errorf("this installation has no SSH_FORWARDING_HOST configured, " +
			"or the node publishes no ports")
	}
	fmt.Println(res.Command)
	return nil
}

// ------------------------------------------------------------- small helpers

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func pathDir(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}

func humanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func readAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }

// designNodeRef is the part of a stack's design the node commands need: the id the
// API keys on, and the label everything a person looks at shows.
type designNodeRef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Type  string `json:"type"`
}

// stackNodes reads a stack's design nodes.
//
// The design, not the deployments, because a node exists as soon as it is drawn: a
// name should resolve on a draft too, and a node that exists but is not running then
// gets the server's own 409, which says something more accurate than "no such node".
func stackNodes(c *Client, stackID int64) ([]designNodeRef, error) {
	var raw []byte
	if err := c.get(fmt.Sprintf("/api/stacks/%d", stackID), &raw); err != nil {
		return nil, err
	}
	var d stackDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	var doc struct {
		Nodes []designNodeRef `json:"nodes"`
	}
	if len(d.Design) > 0 {
		if err := json.Unmarshal(d.Design, &doc); err != nil {
			return nil, fmt.Errorf("the stack's design is not readable: %w", err)
		}
	}
	return doc.Nodes, nil
}

// resolveNode turns what a person types into the node id the API wants.
//
// Every node command took the id and only the id, and the id is not a thing anyone
// has: the designer generates it from a timestamp, so a node whose hostname is
// "ps-01" everywhere in the UI is addressed as "ps-mt1kvaak-3". `node list` printed
// only the id, so the mapping was not visible anywhere either. Stacks already solved
// this — resolveStack takes a name or an id — and this is the same precedence, so an
// id that worked before still works.
func resolveNode(c *Client, stackID int64, ref string) (string, error) {
	nodes, err := stackNodes(c, stackID)
	if err != nil {
		return "", err
	}
	// Exact id first: a label is allowed to look like an id.
	for _, n := range nodes {
		if n.ID == ref {
			return n.ID, nil
		}
	}
	var exact, fuzzy []designNodeRef
	for _, n := range nodes {
		switch {
		case n.Label == ref:
			exact = append(exact, n)
		case strings.EqualFold(n.Label, ref):
			fuzzy = append(fuzzy, n)
		}
	}
	for _, set := range [][]designNodeRef{exact, fuzzy} {
		switch len(set) {
		case 1:
			return set[0].ID, nil
		case 0:
			continue
		default:
			// Labels are hostnames, so this should not happen — but guessing which
			// container to open a root shell on is not the place to find out.
			var ids []string
			for _, n := range set {
				ids = append(ids, n.ID)
			}
			return "", fmt.Errorf("%d nodes are called %q — use an id: %s",
				len(set), ref, strings.Join(ids, ", "))
		}
	}
	var labels []string
	for _, n := range nodes {
		labels = append(labels, n.Label)
	}
	if len(labels) == 0 {
		return "", fmt.Errorf("no node called %q — this stack has no nodes yet", ref)
	}
	return "", fmt.Errorf("no node called %q in this stack. It has: %s",
		ref, strings.Join(labels, ", "))
}

// resolveNodes resolves a comma-separated list, for the flags that take one.
func resolveNodes(c *Client, stackID int64, list string) (string, error) {
	parts := strings.Split(list, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := resolveNode(c, stackID, p)
		if err != nil {
			return "", err
		}
		out = append(out, id)
	}
	return strings.Join(out, ","), nil
}

// stackAndNode is the two-line preamble every single-node command shares.
func stackAndNode(stackRef, nodeRef string) (*Client, Stack, string, error) {
	c, err := mustClient()
	if err != nil {
		return nil, Stack{}, "", err
	}
	st, err := resolveStack(c, stackRef)
	if err != nil {
		return nil, Stack{}, "", err
	}
	id, err := resolveNode(c, st.ID, nodeRef)
	if err != nil {
		return nil, Stack{}, "", err
	}
	return c, st, id, nil
}
