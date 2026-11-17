package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// cmd_exec.go — `dbcanvas exec` and `dbcanvas cp`: a command in a node, and a file to or from
// one, for scripts. exec runs without a TTY through the node exec API (nodeexec.go), so a
// command's output and exit status are its own: stdout to stdout, stderr to stderr, and
// dbcanvas exits with the command's status.

// stackNode is a node of a stack as `exec` and `cp` find it.
type stackNode struct {
	ID    string
	Label string
}

// findNode resolves NODE in a stack: its label, its id, or its label in any case.
func (c ctl) findNode(st Stack, ref string) (stackNode, error) {
	var d struct {
		Design struct {
			Nodes []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"nodes"`
		} `json:"design"`
	}
	if err := c.do("GET", fmt.Sprintf("/api/stacks/%d", st.ID), nil, &d); err != nil {
		return stackNode{}, err
	}
	for _, n := range d.Design.Nodes {
		if n.Label == ref || n.ID == ref {
			return stackNode{n.ID, n.Label}, nil
		}
	}
	for _, n := range d.Design.Nodes {
		if strings.EqualFold(n.Label, ref) {
			return stackNode{n.ID, n.Label}, nil
		}
	}
	var labels []string
	for _, n := range d.Design.Nodes {
		labels = append(labels, n.Label)
	}
	return stackNode{}, fmt.Errorf("%s has no node %q; its nodes: %s", st.Name, ref, strings.Join(labels, ", "))
}

// splitExecArgs separates exec's own arguments from the command: everything after the first
// `--`, or, without one, everything after STACK and NODE (flags then come first).
func splitExecArgs(args []string) (own, command []string, dashed bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

func cliExec(args []string) error {
	fs := cmdFlags("exec")
	own, command, dashed := splitExecArgs(args)
	var pos []string
	var err error
	if dashed {
		pos, err = parseInterspersed(fs, own)
	} else {
		err = fs.Parse(own)
		pos = fs.Args()
		if len(pos) > 2 {
			pos, command = pos[:2], pos[2:]
		}
	}
	if err != nil {
		return flagErr(err)
	}
	if len(pos) != 2 || len(command) == 0 {
		return usageErr("exec")
	}
	timeout := flagDuration(fs, "timeout")
	if timeout < 0 {
		return errUsage{"--timeout cannot be negative"}
	}
	body := map[string]any{"argv": command, "user": flagString(fs, "user")}
	if timeout > 0 {
		secs := int(timeout.Seconds())
		if secs < 1 {
			secs = 1
		}
		body["timeout"] = secs
	}
	if flagBool(fs, "i") {
		in, err := io.ReadAll(io.LimitReader(os.Stdin, nodeExecStdinLimit+1))
		if err != nil {
			return err
		}
		if len(in) > nodeExecStdinLimit {
			return fmt.Errorf("standard input is limited to %d MiB; copy larger files with dbcanvas cp", nodeExecStdinLimit>>20)
		}
		body["stdin"] = string(in)
	}
	asJSON := flagBool(fs, "json")
	return withDBCanvas(func(c ctl) error {
		st, err := c.findStack(pos[0])
		if err != nil {
			return err
		}
		n, err := c.findNode(st, pos[1])
		if err != nil {
			return err
		}
		var res nodeExecResponse
		if err := c.do("POST", fmt.Sprintf("/api/stacks/%d/nodes/%s/exec", st.ID, url.PathEscape(n.ID)), body, &res); err != nil {
			return err
		}
		code := res.ExitCode
		if asJSON {
			printJSON(res)
		} else {
			os.Stdout.Write(execOutput(res.Stdout, res.StdoutBase64))
			os.Stderr.Write(execOutput(res.Stderr, res.StderrBase64))
			if res.Truncated {
				fmt.Fprintln(os.Stderr, "dbcanvas: the output was cut short; redirect it to a file on the node and copy that with dbcanvas cp")
			}
			if res.TimedOut {
				fmt.Fprintf(os.Stderr, "dbcanvas: the command did not finish in %s and was stopped\n", firstNonEmpty(timeout.String(), "10m"))
			}
		}
		if res.TimedOut && code == 0 {
			code = 124 // as timeout(1) reports it
		}
		if code != 0 {
			return errExit{code}
		}
		return nil
	})
}

// execOutput is one of exec's output streams, as bytes.
func execOutput(text, b64 string) []byte {
	if b64 != "" {
		if b, err := base64.StdEncoding.DecodeString(b64); err == nil {
			return b
		}
	}
	return []byte(text)
}

// nodePath splits NODE:/path; ok is false for a local path.
func nodePath(s string) (node, p string, ok bool) {
	node, p, ok = strings.Cut(s, ":")
	if !ok || node == "" || !strings.HasPrefix(p, "/") || strings.ContainsAny(node, `/\`) {
		return "", "", false
	}
	return node, p, true
}

func cliCp(args []string) error {
	fs := cmdFlags("cp")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	if len(pos) != 3 {
		return usageErr("cp")
	}
	srcNode, srcPath, fromNode := nodePath(pos[1])
	dstNode, dstPath, toNode := nodePath(pos[2])
	switch {
	case fromNode && toNode:
		return errors.New("copy between two nodes in the browser's file manager, or through a local file")
	case !fromNode && !toNode:
		return errors.New("one side must be on a node, written NODE:/path")
	}
	return withDBCanvas(func(c ctl) error {
		st, err := c.findStack(pos[0])
		if err != nil {
			return err
		}
		if fromNode {
			n, err := c.findNode(st, srcNode)
			if err != nil {
				return err
			}
			return cpFromNode(c, st, n, srcPath, pos[2])
		}
		n, err := c.findNode(st, dstNode)
		if err != nil {
			return err
		}
		return cpToNode(c, st, n, pos[1], dstPath)
	})
}

// cpFromNode downloads one regular file.
func cpFromNode(c ctl, st Stack, n stackNode, src, dst string) error {
	resp, err := c.stream("GET", fmt.Sprintf("/api/stacks/%d/nodes/%s/fs/download?path=%s", st.ID, url.PathEscape(n.ID), url.QueryEscape(src)), nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") == "application/gzip" {
		return fmt.Errorf("%s:%s is a directory; dbcanvas cp copies one file", n.Label, src)
	}
	if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		dst = filepath.Join(dst, path.Base(src))
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(dst)
		return err
	}
	return f.Close()
}

// cpToNode uploads one regular file. dst names the file, or a directory that exists on the node
// (with or without a trailing /), which keeps the local file's name.
func cpToNode(c ctl, st Stack, n stackNode, src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; dbcanvas cp copies one file", src)
	}
	dir, name := path.Dir(dst), path.Base(dst)
	if strings.HasSuffix(dst, "/") || c.nodeIsDir(st, n, dst) {
		dir, name = strings.TrimSuffix(dst, "/"), filepath.Base(src)
		if dir == "" {
			dir = "/"
		}
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := mw.WriteField("dest", dir)
		if err == nil {
			var part io.Writer
			if part, err = mw.CreateFormFile(name, name); err == nil {
				_, err = io.Copy(part, f)
			}
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	resp, err := c.stream("POST", fmt.Sprintf("/api/stacks/%d/nodes/%s/fs/upload", st.ID, url.PathEscape(n.ID)), pr, mw.FormDataContentType())
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// nodeIsDir reports whether p is a directory on the node.
func (c ctl) nodeIsDir(st Stack, n stackNode, p string) bool {
	var res nodeExecResponse
	err := c.do("POST", fmt.Sprintf("/api/stacks/%d/nodes/%s/exec", st.ID, url.PathEscape(n.ID)),
		map[string]any{"argv": []string{"test", "-d", p}, "timeout": 30}, &res)
	return err == nil && res.ExitCode == 0
}

// stream sends a request with a raw body and returns the response for the caller to read; an
// error answer is turned into an error, as raw does.
func (c ctl) stream(method, p string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequest(method, "http://dbcanvas"+p, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(b, &e)
		return nil, &apiError{Status: resp.StatusCode, Msg: firstNonEmpty(e.Error, string(bytes.TrimSpace(b)), resp.Status)}
	}
	return resp, nil
}
