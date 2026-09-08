// dbcanvas-cli drives a DBCanvas installation from a terminal.
//
// It is a client of the same HTTP API the web UI uses (see docs/API.md) — there is
// no privileged back door and no second implementation of anything. That is what
// makes `dbcanvas api METHOD PATH` a complete escape hatch: every one of the app's
// endpoints is reachable from the day it exists, and the curated subcommands below
// are ergonomics on top, not the boundary of what the tool can do.
//
//	dbcanvas login --url http://localhost:8080
//	dbcanvas stack deploy my-pxc-lab --wait
//	dbcanvas node console my-pxc-lab pxc-01
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

// version is stamped at link time from the repo's VERSION file
// (-ldflags "-X main.version=…"); "dev" in a plain `go build`.
var version = "dev"

// global flags, shared by every subcommand.
type globals struct {
	profile string
	url     string
	token   string
	json    bool
}

var g globals

// command is one subcommand.
type command struct {
	name    string
	summary string
	// run receives the arguments after the command name. Commands that need a
	// server build their own client with mustClient().
	run func(args []string) error
}

var commands []command

func init() {
	commands = []command{
		{"login", "Sign in and store an API token for this installation", cmdLogin},
		{"logout", "Revoke the stored token and forget this installation", cmdLogout},
		{"whoami", "Show who you are signed in as, and on which installation", cmdWhoami},
		{"password", "Change your own password", cmdPassword},
		{"token", "Manage API tokens: list, create, revoke", cmdToken},
		{"stack", "Stacks: compose or create, deploy, destroy, delete, export", cmdStack},
		{"node", "Nodes: list, start, stop, restart, console, exec, cp, tunnel", cmdNode},
		{"template", "Deployment templates: list, export, import", cmdTemplate},
		{"datagen", "Data Generator: connections, tables, run", cmdDatagen},
		{"query", "Query Runner: run SQL across nodes", cmdQuery},
		{"benchmark", "Benchmark: run a workload, read the history", cmdBenchmark},
		{"logs", "Log Summary: collect and read several nodes' logs as one timeline", cmdLogs},
		{"capture", "Packet Inspector: capture on a node and read the decoded packets", cmdCapture},
		{"stalk", "Stalk Summary: run pt-stalk on a node, download and analyse it", cmdStalk},
		{"ftdc", "FTDC Summary: read a mongod's own diagnostic data", cmdFTDC},
		{"dashboard", "The dashboard's counters, and the live resource sample", cmdDashboard},
		{"notifications", "Your notifications; --read-all to clear them", cmdNotifications},
		{"endpoints", "List the API endpoints this installation serves", cmdEndpoints},
		{"api", "Call any endpoint directly: dbcanvas api POST /api/stacks/1/deploy", cmdAPI},
		{"version", "Print the CLI version, and the server's when signed in", cmdVersion},
	}
}

func main() {
	fs := flag.NewFlagSet("dbcanvas", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&g.profile, "profile", "", "which configured installation to use")
	fs.StringVar(&g.url, "url", "", "DBCanvas base URL (overrides the profile and DBCANVAS_URL)")
	fs.StringVar(&g.token, "token", "", "API token (overrides the profile and DBCANVAS_TOKEN)")
	fs.BoolVar(&g.json, "json", false, "print the server's JSON verbatim instead of a table")
	fs.Usage = usage

	// Global flags are accepted before the subcommand; a subcommand's own flags are
	// parsed by the subcommand, so `stack deploy x --wait` works as written.
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	args := fs.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	name := args[0]
	for _, c := range commands {
		if c.name == name {
			if err := c.run(args[1:]); err != nil {
				if !errors.Is(err, errUsage) {
					fmt.Fprintln(os.Stderr, "dbcanvas: "+err.Error())
				}
				os.Exit(exitCodeFor(err))
			}
			return
		}
	}
	fmt.Fprintf(os.Stderr, "dbcanvas: unknown command %q\n\n", name)
	usage()
	os.Exit(2)
}

func usage() {
	fmt.Fprintf(os.Stderr, "dbcanvas %s — drive a DBCanvas installation from your terminal\n\n", version)
	fmt.Fprintln(os.Stderr, "Usage:\n  dbcanvas [global flags] <command> [arguments]\n\nCommands:")
	w := 0
	for _, c := range commands {
		if len(c.name) > w {
			w = len(c.name)
		}
	}
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-*s  %s\n", w, c.name, c.summary)
	}
	fmt.Fprint(os.Stderr, `
Global flags:
  --profile NAME   which configured installation to use
  --url URL        base URL, overriding the profile and DBCANVAS_URL
  --token TOKEN    API token, overriding the profile and DBCANVAS_TOKEN
  --json           print the server's JSON verbatim instead of a table

Environment:
  DBCANVAS_URL     base URL; what CI should use, so nothing is written to disk
  DBCANVAS_TOKEN   API token
  DBCANVAS_CONFIG  path to the config file (default ~/.config/dbcanvas/config.json)

Exit codes:
  0 success   1 request failed   2 bad usage   3 not signed in   4 --wait gave up

Every endpoint is reachable whether or not it has a subcommand:
  dbcanvas endpoints              # the catalogue
  dbcanvas api GET /api/stacks    # call any of them

Docs: docs/API.md and docs/CLI.md in the DBCanvas repository.
`)
}

// ------------------------------------------------------------- helpers

// mustClient resolves the installation and returns a bearer-authenticated client.
func mustClient() (*Client, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	p, _, err := cfg.resolve(g.profile, g.url, g.token)
	if err != nil {
		return nil, err
	}
	if p.Token == "" {
		return nil, &APIError{Status: 401, Message: "no token for " + p.URL}
	}
	return newClient(p.URL, p.Token), nil
}

// sub dispatches a subcommand's own subcommands, and prints what it accepts when
// given something it does not recognise. Every group command goes through this so
// the error is the same shape everywhere.
func sub(group string, args []string, table map[string]func([]string) error, order []string) error {
	if len(args) == 0 {
		return subUsage(group, order, table)
	}
	fn, ok := table[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "dbcanvas %s: unknown subcommand %q\n\n", group, args[0])
		return subUsage(group, order, table)
	}
	return fn(args[1:])
}

func subUsage(group string, order []string, table map[string]func([]string) error) error {
	fmt.Fprintf(os.Stderr, "Usage: dbcanvas %s <subcommand> [arguments]\n\nSubcommands:\n", group)
	for _, n := range order {
		if _, ok := table[n]; ok {
			fmt.Fprintf(os.Stderr, "  %s\n", n)
		}
	}
	return errUsage
}

// flagsFor builds a FlagSet that reports errors the way the rest of the tool does.
func flagsFor(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// positional holds the non-flag arguments of the command currently running, as
// collected by parse.
var positional []string

// parse runs a subcommand's flags and collects its positional arguments.
//
// It loops rather than calling Parse once because Go's flag package stops at the
// first non-flag argument: `dbcanvas api POST /api/stacks --data '{}'` would leave
// --data unparsed and silently send no body, which is exactly the documented form.
// Re-parsing after each positional is the standard idiom for interspersed flags.
func parse(fs *flag.FlagSet, args []string) error {
	positional = nil
	for {
		if err := fs.Parse(args); err != nil {
			return errUsage
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return nil
		}
		// `--` ends flag parsing: everything after it is positional, verbatim.
		// node exec relies on this to pass a remote command through untouched.
		if len(args) > 0 && args[0] == "--" {
			positional = append(positional, rest...)
			return nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// arg is the i'th positional argument, or "".
func arg(i int) string {
	if i < len(positional) {
		return positional[i]
	}
	return ""
}

// nargs is how many positional arguments there were.
func nargs() int { return len(positional) }

// need enforces a minimum number of positional arguments.
func need(n int, what string) error {
	if nargs() < n {
		fmt.Fprintf(os.Stderr, "dbcanvas: expected %s\n", what)
		return errUsage
	}
	return nil
}

func cmdVersion(args []string) error {
	fmt.Printf("dbcanvas-cli %s\n", version)
	// The server's version is worth having in the same breath, but not worth
	// failing over: `dbcanvas version` has to work before anybody has signed in.
	c, err := mustClient()
	if err != nil {
		return nil
	}
	var st struct {
		Version string `json:"version"`
	}
	if err := c.get("/api/setup/status", &st); err == nil && st.Version != "" {
		fmt.Printf("server      %s (%s)\n", st.Version, c.BaseURL)
	}
	return nil
}

// truncate keeps a table column from wrapping. Counted and cut in runes, so a
// multi-byte character is never sliced in half and the limit means what it says.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// yn renders a boolean the way a table column wants it.
func yn(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

// joinNonEmpty is strings.Join over only the non-empty parts.
func joinNonEmpty(sep string, parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
