package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// clihelp.go — dbcanvas-cli's own help, served to the API page.
//
// The page could offer the binary and a link to the manual on GitHub, and for a while it did.
// That is a poor answer to "I have downloaded this, now what": the manual is somewhere else, an
// installation on a private network may not be able to reach it at all, and the three example
// lines on the page do not say which commands exist.
//
// So the page asks the CLI. The image already carries the cross-compiled binaries for the
// download (clidownload.go), one of which is for the platform this server is running on — so the
// help text shown in the browser is not a copy of anything: it is what that exact build prints,
// produced by the same table that dispatches its commands (cli/main.go). A command added to the
// CLI appears here on the next build, and one that is renamed cannot be left behind in a list
// somebody forgot to edit, because there is no list.

// cliHelpTimeout bounds the subprocess. Printing usage is immediate; this is only here so a
// broken binary cannot hold a request open.
const cliHelpTimeout = 5 * time.Second

var (
	cliHelpOnce sync.Once
	cliHelpText string
	cliHelpErr  error
)

// resetCLIHelpCache clears the memoized answer. Only the tests use it: the binary cannot change
// under a running server, which is the whole reason the answer is cached.
func resetCLIHelpCache() {
	cliHelpOnce = sync.Once{}
	cliHelpText, cliHelpErr = "", nil
}

// cliSelfBinary is the bundled binary for the platform this server is on — the only one of the
// five that can actually be run here.
func cliSelfBinary() string {
	return filepath.Join(cliDir(), cliBinaryName(runtime.GOOS, runtime.GOARCH))
}

// cliHelp runs the bundled CLI with no arguments and returns what it prints.
//
// No arguments is deliberately the "what is this" case: cli/main.go answers it with usage() —
// the command table, the global flags, the environment variables and the exit codes — and exits
// 2, which is why a non-zero status is not treated as failure here. Output goes to stderr, as
// usage does; stdout is read too so a future build that prints it the other way still works.
//
// Cached for the life of the process. The binary is baked into the image, so the answer cannot
// change between calls, and this is a page anyone may open.
func (a *App) cliHelp(ctx context.Context) (string, error) {
	cliHelpOnce.Do(func() {
		cliHelpText, cliHelpErr = runCLIHelp(ctx)
	})
	return cliHelpText, cliHelpErr
}

func runCLIHelp(ctx context.Context) (string, error) {
	bin := cliSelfBinary()
	st, err := os.Stat(bin)
	if err != nil {
		// The development case: a checkout run with `go run .` has no image directory. Say what
		// would put one there rather than reporting a mystery.
		return "", fmt.Errorf("this installation has no bundled dbcanvas-cli to ask (%s is not there) — `make cli` builds it, and CLI_DIR points here at the result", bin)
	}
	if st.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s is not executable", bin)
	}

	ctx, cancel := context.WithTimeout(ctx, cliHelpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	// A bare run must not pick up an operator's own profile or token from the server's
	// environment, and has no business reading a config file: this is a help string.
	cmd.Env = []string{"DBCANVAS_CONFIG=/dev/null"}
	err = cmd.Run()
	text := strings.TrimRight(errb.String()+out.String(), "\n")
	if text != "" {
		// Exit 2 is what "no command given" means here, so the text is the answer whatever the
		// status was.
		return text, nil
	}
	if err != nil && !errors.As(err, new(*exec.ExitError)) {
		return "", fmt.Errorf("run %s: %w", filepath.Base(bin), err)
	}
	return "", fmt.Errorf("%s printed no usage", filepath.Base(bin))
}

// handleCLIHelp serves it. Signed in only, like the download beside it.
func (a *App) handleCLIHelp(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	text, err := a.cliHelp(r.Context())
	if err != nil {
		// Not an error the caller can fix, and not a broken page either: the API page shows the
		// reason where the commands would have been, and its download links still work.
		writeJSON(w, http.StatusOK, map[string]any{"help": "", "unavailable": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"help": text, "version": appVersion})
}
