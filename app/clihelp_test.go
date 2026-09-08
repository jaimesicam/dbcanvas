package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The page asks the CLI what its commands are rather than carrying a copy, so the thing worth
// testing is that asking works — against the real binary when one has been built.
//
// `make cli` writes the cross-compiled matrix to dist/, which is where this looks; without it the
// test skips rather than failing, since a checkout that has never run `make cli` is normal.
func TestCLIHelpComesFromTheBinary(t *testing.T) {
	bin := filepath.Join("../dist", cliBinaryName(runtime.GOOS, runtime.GOARCH))
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no dist/" + filepath.Base(bin) + " — run `make cli` to exercise this")
	}
	t.Setenv("CLI_DIR", "../dist")
	resetCLIHelpCache()

	help, err := (&App{}).cliHelp(context.Background())
	if err != nil {
		t.Fatalf("asking the CLI for its help failed: %v", err)
	}
	// The shape of usage(): what to type, then the commands, then how to reach the rest.
	for _, want := range []string{"Usage:", "Commands:", "Global flags:", "Exit codes:"} {
		if !strings.Contains(help, want) {
			t.Errorf("the help has no %q section:\n%s", want, help)
		}
	}
	// And the commands themselves, which is the reason the page shows this at all: somebody who
	// has just downloaded the binary needs to know these exist.
	for _, cmd := range []string{"login", "stack", "node", "api", "endpoints"} {
		if !strings.Contains(help, "\n  "+cmd+" ") && !strings.Contains(help, "\n  "+cmd+"\t") {
			t.Errorf("%q is not listed as a command:\n%s", cmd, help)
		}
	}
	// It is the running installation's own binary, so no config or token of the server's may
	// leak into what a browser is shown.
	for _, secret := range []string{"dbc_", "DBCANVAS_TOKEN="} {
		if strings.Contains(help, secret) {
			t.Errorf("the help text carries %q", secret)
		}
	}
}

// A server with no bundled binary — a development checkout — has to say so in a way that names
// the fix, because "unavailable" on a page with download buttons above it reads as a bug.
func TestCLIHelpWithoutABinarySaysWhatToDo(t *testing.T) {
	t.Setenv("CLI_DIR", t.TempDir())
	resetCLIHelpCache()

	_, err := (&App{}).cliHelp(context.Background())
	if err == nil {
		t.Fatal("an empty CLI directory produced help")
	}
	if !strings.Contains(err.Error(), "make cli") {
		t.Errorf("the message does not name the fix: %v", err)
	}
}
