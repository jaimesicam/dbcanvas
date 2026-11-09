// dbcanvas-cli drives a DBCanvas installation from a terminal.
//
// It is a client of the same HTTP API the web UI uses (see docs/API.md) — there is
// no privileged back door and no second implementation of anything. That is what
// makes `dbcanvas-cli api METHOD PATH` a complete escape hatch: every one of the app's
// endpoints is reachable from the day it exists, and the curated subcommands below
// are ergonomics on top, not the boundary of what the tool can do.
//
//	dbcanvas-cli login --url http://localhost:8080
//	dbcanvas-cli stack deploy my-pxc-lab --wait
//	dbcanvas-cli node console my-pxc-lab pxc-01
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
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
		{"user", "Accounts (administrators): list, create", cmdUser},
		{"group", "Security groups (administrators): which pages each group may use", cmdGroup},
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
		{"updates", "Check GitHub for a newer DBCanvas server, and what it would bring", cmdUpdates},
		{"notifications", "Your notifications; --read-all to clear them", cmdNotifications},
		{"endpoints", "List the API endpoints this installation serves", cmdEndpoints},
		{"api", "Call any endpoint directly: dbcanvas-cli api POST /api/stacks/1/deploy", cmdAPI},
		{"version", "Print the CLI version, and the server's when signed in", cmdVersion},
		{"doctor", "Check this installation (administrators): Docker, images, catalogue, default passwords, exposure", cmdDoctor},
		{"commands", "Every command with its arguments and flags; --json (before it) for a program to read", cmdCommands},
		{"help", "What a command takes: dbcanvas-cli help node exec (the same as node exec -h)", cmdHelpCmd},
	}
}

func main() {
	fs := flag.NewFlagSet("dbcanvas-cli", flag.ContinueOnError)
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
				var rx *remoteExit
				if !errors.Is(err, errUsage) && !errors.Is(err, errHelp) && !errors.As(err, &rx) {
					fmt.Fprintln(os.Stderr, "dbcanvas-cli: "+err.Error())
				}
				os.Exit(exitCodeFor(err))
			}
			return
		}
	}
	fmt.Fprintf(os.Stderr, "dbcanvas-cli: unknown command %q\n\n", name)
	usage()
	os.Exit(2)
}

func usage() {
	fmt.Fprintf(os.Stderr, "dbcanvas-cli %s — drive a DBCanvas installation from your terminal\n\n", version)
	fmt.Fprintln(os.Stderr, "Usage:\n  dbcanvas-cli [global flags] <command> [arguments]\n\nCommands:")
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
  dbcanvas-cli endpoints              # the catalogue
  dbcanvas-cli api GET /api/stacks    # call any of them

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
	switch args[0] {
	case "-h", "-help", "--help", "help":
		fmt.Fprint(os.Stderr, subUsageText(group, order, table))
		return errHelp
	}
	fn, ok := table[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "dbcanvas-cli %s: unknown subcommand %q\n\n", group, args[0])
		return subUsage(group, order, table)
	}
	return fn(args[1:])
}

func subUsage(group string, order []string, table map[string]func([]string) error) error {
	fmt.Fprint(os.Stderr, subUsageText(group, order, table))
	return errUsage
}

// flagsFor builds a FlagSet that reports errors the way the rest of the tool does.
func flagsFor(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = usageFunc(name, fs)
	return fs
}

// positional holds the non-flag arguments of the command currently running, as
// collected by parse.
var positional []string

// parse runs a subcommand's flags and collects its positional arguments.
//
// It loops rather than calling Parse once because Go's flag package stops at the
// first non-flag argument: `dbcanvas-cli api POST /api/stacks --data '{}'` would leave
// --data unparsed and silently send no body, which is exactly the documented form.
// Re-parsing after each positional is the standard idiom for interspersed flags.
func parse(fs *flag.FlagSet, args []string) error {
	positional = nil
	// `dbcanvas-cli commands` (cmd_commands.go): record the flags and stop, before anything runs.
	if describing != nil {
		*describing = describeFlags(fs)
		return errHelp
	}
	for {
		if err := fs.Parse(args); err != nil {
			// -h / --help: the flag package has printed the usage already, and asking for it
			// is not a mistake.
			if errors.Is(err, flag.ErrHelp) {
				return errHelp
			}
			return errUsage
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return nil
		}
		// `--` ends flag parsing: everything after it is positional, verbatim.
		// node exec relies on this to pass a remote command through untouched. fs.Parse
		// consumes the `--` itself, so look at the argument just before what it left —
		// checking only args[0] missed a `--` that came after a flag
		// (`node exec s n --timeout 1m -- mysql -e …`), and the remote command's own
		// flags were then parsed as ours.
		if used := len(args) - len(rest); used > 0 && args[used-1] == "--" {
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
		fmt.Fprintf(os.Stderr, "dbcanvas-cli: expected %s\n", what)
		return errUsage
	}
	return nil
}

func cmdVersion(args []string) error {
	fs := flagsFor("version")
	verify := fs.Bool("verify", false, "check this binary's SHA-256 against the checksums the server publishes for its CLI")
	release := fs.Bool("release", false, "with --verify: check against the signed checksum list on this version's GitHub release instead")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *release && !*verify {
		fmt.Fprintln(os.Stderr, "dbcanvas-cli: --release goes with --verify")
		return errUsage
	}
	if *verify && *release {
		return verifyAgainstRelease(releaseChecksumsURL(version))
	}
	if *verify {
		return verifySelf()
	}
	// The server's version is worth having in the same breath, but not worth
	// failing over: `dbcanvas-cli version` has to work before anybody has signed in.
	server, url := "", ""
	if c, err := mustClient(); err == nil {
		var st struct {
			Version string `json:"version"`
		}
		if err := c.get("/api/setup/status", &st); err == nil {
			server, url = st.Version, c.BaseURL
		}
	}
	match := versionsMatch(version, server)
	if g.json {
		return printJSON(map[string]any{"cli": version, "server": server, "url": url, "match": match})
	}
	fmt.Printf("dbcanvas-cli %s\n", version)
	// The binary is downloaded on its own, so it says what it is licensed under itself.
	fmt.Println("Free software under the GPL-3.0-only, with NO WARRANTY.")
	fmt.Println("Source and licences: https://github.com/jaimesicam/dbcanvas")
	if server != "" {
		fmt.Printf("server      %s (%s)\n", server, url)
		if !match {
			fmt.Fprintf(os.Stderr, "warning: this CLI is %s and the server is %s — commands and flags may differ; "+
				"download the matching CLI from %s/api/cli/download\n", version, server, url)
		}
	}
	return nil
}

// versionsMatch reports whether a CLI and a server are the same release. A development
// build on either side matches anything (it is somebody working on DBCanvas itself), and
// an unknown server is not reported as a mismatch.
func versionsMatch(cli, server string) bool {
	if server == "" || cli == "dev" || server == "dev" {
		return true
	}
	return strings.TrimPrefix(cli, "v") == strings.TrimPrefix(server, "v")
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

// cliFileName is this platform's binary as the server and the release name it.
func cliFileName() string {
	name := "dbcanvas-cli_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// checksumFor finds a file's digest in a sha256sum listing ("<hex>  <name>").
func checksumFor(list, name string) string {
	for _, line := range strings.Split(list, "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return f[0]
		}
	}
	return ""
}

// selfSHA256 is the running binary's path and SHA-256.
func selfSHA256() (string, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	f, err := os.Open(exe)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", err
	}
	return exe, hex.EncodeToString(h.Sum(nil)), nil
}

// verifySelf checks the running binary against GET /api/cli/checksums: the server's SHA-256 of
// the binary it ships for this platform.
func verifySelf() error {
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/cli/checksums", &raw); err != nil {
		return err
	}
	name := cliFileName()
	want := checksumFor(string(raw), name)
	if want == "" {
		return fmt.Errorf("the server publishes no checksum for %s", name)
	}
	exe, got, err := selfSHA256()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s does not match the %s the server ships (sha256 %s, server %s) — "+
			"it is another build or release; download the server's from %s/api/cli/download", exe, name, got, want, c.BaseURL)
	}
	fmt.Printf("%s matches the %s this server ships (sha256 %s).\n", exe, name, got)
	return nil
}

// releaseRepo is where releases are published (.github/workflows/release.yml).
const releaseRepo = "https://github.com/jaimesicam/dbcanvas"

func releaseChecksumsURL(ver string) string {
	return releaseRepo + "/releases/download/v" + strings.TrimPrefix(ver, "v") + "/dbcanvas-cli_checksums.txt"
}

// verifyAgainstRelease checks the running binary against the checksum list on its version's
// GitHub release. That list is signed (Sigstore, keyless, by the release workflow); this compares
// digests over HTTPS, and cosign verify-blob checks the signature itself.
func verifyAgainstRelease(url string) error {
	if version == "dev" {
		return errors.New("this is a development build (version dev); only released versions are on GitHub")
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Get(url)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("release v%s has no signed checksum list (%s) — releases before signed binaries have none", strings.TrimPrefix(version, "v"), url)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: %s", url, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	name := cliFileName()
	want := checksumFor(string(raw), name)
	if want == "" {
		return fmt.Errorf("release v%s lists no %s", strings.TrimPrefix(version, "v"), name)
	}
	exe, got, err := selfSHA256()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s does not match the %s on release v%s (sha256 %s, release %s) — "+
			"it was built from other source, with another Go release, or changed since", exe, name, strings.TrimPrefix(version, "v"), got, want)
	}
	fmt.Printf("%s matches the %s on GitHub release v%s (sha256 %s).\n", exe, name, strings.TrimPrefix(version, "v"), got)
	fmt.Printf("To check the list's signature: download dbcanvas-cli_checksums.txt and its .sigstore.json from the release, then run cosign verify-blob (see docs/SECURITY.md).\n")
	return nil
}
