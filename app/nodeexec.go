package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// nodeexec.go — POST /api/stacks/{id}/nodes/{nid}/exec: run one command in a node and return
// its output and exit status.
//
// Until this existed the console WebSocket (terminal.go) was the only way in, and
// `dbcanvas-cli node exec` typed the command into it: the answer came back with the shell's
// prompt, the echoed command, terminal-title escapes and a trailing `exit` mixed in, and with no
// exit status at all. Scripts and agents had to filter all of that out and could not tell a
// failed command from a working one. This runs the command the way the deploy steps do (the
// engine's exec, no TTY) and returns stdout, stderr and the exit code separately.
//
// It grants nothing the console does not: the same owner check, the same Nodes feature, and
// write scope for a token (it is a POST). The command runs as the container's default user —
// root on every node image — or as `user`, as the console's ?user= does.

const (
	nodeExecDefaultTimeout = 10 * time.Minute
	nodeExecMaxTimeout     = time.Hour
	// nodeExecOutputLimit caps each of stdout and stderr in the response. The whole output is
	// held in memory before it is sent, so this is what keeps a runaway `cat` of a large file
	// from taking the server with it; copy files with the file manager (…/fs/) instead.
	nodeExecOutputLimit = 16 << 20
	// nodeExecStdinLimit caps the stdin a caller may send.
	nodeExecStdinLimit = 16 << 20
)

// nodeExecWrapper runs argv ("$@" after the timeout) in a login bash where the image has bash,
// so PATH is the one the console has (/etc/profile and its profile.d), and under coreutils or
// busybox `timeout` where there is one, so a command that outlives its timeout is killed in the
// container rather than left running when the request gives up. Images without either (none of
// DBCanvas's own) still run the command, directly.
const nodeExecWrapper = `T=$1; shift
if command -v bash >/dev/null 2>&1; then set -- bash -lc 'exec "$@"' bash "$@"; fi
if command -v timeout >/dev/null 2>&1; then set -- timeout -k 5 "$T" "$@"; fi
exec "$@"`

type nodeExecRequest struct {
	// Argv is the command and its arguments, run as given — no shell splits or expands them.
	// For pipes, redirections or variables, run a shell: ["bash", "-c", "…"].
	Argv []string `json:"argv"`
	// Stdin, if set, is written to the command's standard input; otherwise it reads nothing.
	Stdin string `json:"stdin"`
	// Timeout is in seconds (default 600, at most 3600).
	Timeout int `json:"timeout"`
	// User runs the command as this user (a name or uid) instead of the container's default.
	User string `json:"user"`
}

// nodeExecResponse is the command's result. Output that is not valid UTF-8 comes back base64
// encoded in the …Base64 field instead, so a binary file read with `cat` arrives intact.
type nodeExecResponse struct {
	ExitCode     int    `json:"exitCode"`
	Stdout       string `json:"stdout"`
	Stderr       string `json:"stderr"`
	StdoutBase64 string `json:"stdoutBase64,omitempty"`
	StderrBase64 string `json:"stderrBase64,omitempty"`
	// Truncated is set when stdout or stderr was longer than the limit and was cut.
	Truncated bool `json:"truncated,omitempty"`
	// TimedOut is set when the command was killed for running past its timeout.
	TimedOut   bool  `json:"timedOut,omitempty"`
	DurationMs int64 `json:"durationMs"`
}

func (a *App) handleNodeExec(w http.ResponseWriter, r *http.Request) {
	var req nodeExecRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		writeErr(w, http.StatusBadRequest, `argv is required: the command and its arguments, e.g. {"argv":["mysql","-e","SELECT 1"]}`)
		return
	}
	if len(req.Stdin) > nodeExecStdinLimit {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("stdin is limited to %d MiB; copy larger files with …/upload", nodeExecStdinLimit>>20))
		return
	}
	timeout := nodeExecDefaultTimeout
	if req.Timeout < 0 {
		writeErr(w, http.StatusBadRequest, "timeout is in seconds and cannot be negative")
		return
	}
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}
	if timeout > nodeExecMaxTimeout {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("timeout is at most %d seconds", int(nodeExecMaxTimeout.Seconds())))
		return
	}

	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	st, _ := a.store.GetStack(dep.StackID)
	// The request's own context, so a caller that gives up stops waiting here too; the extra
	// margin lets `timeout` in the container fire first and report itself.
	ctx, cancel := context.WithTimeout(withEngine(r.Context(), a.depEngine(st, dep.NodeID)), timeout+30*time.Second)
	defer cancel()

	secs := strconv.Itoa(int(timeout.Seconds()))
	cmd := append([]string{"/bin/sh", "-c", nodeExecWrapper, "sh", secs}, req.Argv...)
	var stdin []byte
	if req.Stdin != "" {
		stdin = []byte(req.Stdin)
	}
	start := time.Now()
	res, err := a.engCtx(ctx).ExecInput(ctx, dep.ContainerID, req.User, cmd, nil, stdin)
	elapsed := time.Since(start)
	if err != nil {
		if ctx.Err() != nil {
			writeErr(w, http.StatusGatewayTimeout, fmt.Sprintf("the command did not finish within %s", timeout+30*time.Second))
			return
		}
		writeErr(w, http.StatusBadGateway, "exec failed: "+err.Error())
		return
	}
	out := nodeExecResponse{ExitCode: res.Code, DurationMs: elapsed.Milliseconds()}
	var cut bool
	out.Stdout, out.StdoutBase64, cut = nodeExecField(res.Stdout)
	out.Truncated = cut
	out.Stderr, out.StderrBase64, cut = nodeExecField(res.Stderr)
	out.Truncated = out.Truncated || cut
	// coreutils timeout(1) exits 124 when it stopped the command; busybox's reports the signal
	// (143, TERM); either exits 137 when it had to KILL it.
	out.TimedOut = (res.Code == 124 || res.Code == 137 || res.Code == 143) && elapsed >= timeout
	writeJSON(w, http.StatusOK, out)
}

// nodeExecField caps one stream and picks its encoding: the text itself when it is valid
// UTF-8, base64 otherwise.
func nodeExecField(s string) (text, b64 string, truncated bool) {
	if len(s) > nodeExecOutputLimit {
		s, truncated = s[:nodeExecOutputLimit], true
	}
	if utf8.ValidString(s) {
		return s, "", truncated
	}
	return "", base64.StdEncoding.EncodeToString([]byte(s)), truncated
}
