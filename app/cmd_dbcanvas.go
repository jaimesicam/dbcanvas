package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

// cmd_dbcanvas.go — the dbcanvas command.
//
//	dbcanvas                      start dbcanvas and open it in the browser
//	dbcanvas serve                start it without opening the browser
//	dbcanvas ls                   list stacks
//	dbcanvas export STACK         write a stack as a stack file
//	dbcanvas up -f FILE           create or update a stack from a file, and deploy it
//
// A command hands its work to the running dbcanvas over the control socket
// (ctlsock.go). With none running it runs the engine itself, in this process and under
// the same lock, for as long as the command takes.

// soloPort is the browser's port when APP_PORT does not set one; a free one is taken
// when it is busy.
const soloPort = "7480"

const cliUsage = `dbcanvas — database labs on Docker

Usage:
  dbcanvas                      start dbcanvas and open it in the browser
  dbcanvas serve                start dbcanvas without opening the browser
  dbcanvas ls                   list your stacks
  dbcanvas export STACK [-o FILE]
                                write a stack as a stack file (YAML)
  dbcanvas up -f FILE [--dry-run] [--wait] [--timeout 1h]
                                create or update the stack in FILE, then deploy it
  dbcanvas down STACK           remove a stack's containers, keep its design
  dbcanvas rm STACK             delete a stack and everything it deployed
  dbcanvas version              print the version

Data folder: %s (DBCANVAS_DATA overrides it)
`

func runCLI(args []string) int {
	log.SetFlags(log.LstdFlags)
	// Everything this dbcanvas creates on the Docker host carries its instance name, so
	// several people can share one host (instance.go). It is the OS login unless set.
	if strings.TrimSpace(os.Getenv("DBCANVAS_INSTANCE")) == "" {
		instanceName = soloInstance()
	}
	// Node images for this machine's architecture, and the lab domain, unless set: the
	// container edition takes both from its .env.
	if os.Getenv("DOCKER_PLATFORM") == "" {
		os.Setenv("DOCKER_PLATFORM", "linux/"+runtime.GOARCH)
	}
	if os.Getenv("DOMAIN") == "" {
		os.Setenv("DOMAIN", "example.net")
	}
	cmd, rest := "", []string(nil)
	if len(args) > 0 {
		cmd, rest = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "":
		err = cliServe(true)
	case "serve":
		err = cliServe(false)
	case "ls", "list":
		err = withDBCanvas(func(c ctl) error { return cliList(c) })
	case "export":
		err = cliExport(rest)
	case "up":
		err = cliUp(rest)
	case "down":
		err = cliStackAction(rest, "down", "POST", "/destroy", "Removed the containers of %s; its design is kept.")
	case "rm":
		err = cliStackAction(rest, "rm", "DELETE", "", "Deleted %s and everything it deployed.")
	case "version", "--version", "-v":
		fmt.Println("dbcanvas", appVersion)
	case "help", "--help", "-h":
		dir, _ := dataDir()
		fmt.Printf(cliUsage, dir)
	default:
		dir, _ := dataDir()
		fmt.Fprintf(os.Stderr, "dbcanvas: unknown command %q\n\n"+cliUsage, cmd, dir)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dbcanvas:", err)
		if errors.Is(err, errDeployFailed) {
			return 3
		}
		return 1
	}
	return 0
}

// cliServe runs dbcanvas until interrupted. With one already running, it opens that one.
func cliServe(open bool) error {
	dir, err := ensureDataDir()
	if err != nil {
		return err
	}
	lock, err := lockEngine(dir)
	if errors.Is(err, errEngineRunning) {
		addr, _ := os.ReadFile(filepath.Join(dir, "url"))
		u := strings.TrimSpace(string(addr))
		if u == "" {
			return errors.New("dbcanvas is already running (it is not serving a browser: a command is using it)")
		}
		fmt.Println("dbcanvas is already running at", u)
		if open {
			openBrowser(u)
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- runServer(serveOpts{ctx: ctx, tcp: true, dataDir: dir, ready: ready}) }()
	select {
	case err := <-done:
		return err
	case addr := <-ready:
		u := "http://" + addr + "/"
		os.WriteFile(filepath.Join(dir, "url"), []byte(u+"\n"), 0o600)
		defer os.Remove(filepath.Join(dir, "url"))
		fmt.Printf("dbcanvas is running at %s — Ctrl-C stops it.\n", u)
		if open {
			openBrowser(u)
		}
	}
	return <-done
}

func openBrowser(u string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	if err := exec.Command(cmd, u).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "could not open a browser (%v); open %s\n", err, u)
	}
}

// withDBCanvas runs f against the running dbcanvas, or, with none running, against an
// engine started in this process for the duration (ctl.inProcess).
func withDBCanvas(f func(c ctl) error) error {
	dir, err := ensureDataDir()
	if err != nil {
		return err
	}
	if ctlReachable(dir) {
		return f(ctl{c: ctlClient(dir)})
	}
	lock, err := lockEngine(dir)
	if errors.Is(err, errEngineRunning) {
		// Another dbcanvas is starting: give its socket a moment.
		for i := 0; i < 50 && !ctlReachable(dir); i++ {
			time.Sleep(200 * time.Millisecond)
		}
		if ctlReachable(dir) {
			return f(ctl{c: ctlClient(dir)})
		}
		return errors.New("another dbcanvas holds the engine but does not answer; is it stuck?")
	}
	if err != nil {
		return err
	}
	defer lock.release()
	// The engine logs to the server log; a command prints only its own output.
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- runServer(serveOpts{ctx: ctx, dataDir: dir, ready: ready}) }()
	select {
	case err := <-done:
		cancel()
		return err
	case <-ready:
	}
	ferr := f(ctl{c: ctlClient(dir), inProcess: true})
	cancel()
	<-done
	return ferr
}

// ctl calls the API over the control socket.
type ctl struct {
	c *http.Client
	// inProcess is set when the engine runs in this process and stops with the command.
	inProcess bool
}

// apiError is an error answer from the API.
type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return e.Msg }

func (c ctl) do(method, path string, body, out any) error {
	b, err := c.raw(method, path, body)
	if err != nil {
		return err
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c ctl) raw(method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		j, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, "http://dbcanvas"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error  string  `json:"error"`
			Issues []issue `json:"issues"`
		}
		json.Unmarshal(b, &e)
		msg := firstNonEmpty(e.Error, strings.TrimSpace(string(b)), resp.Status)
		for _, is := range e.Issues {
			if is.Level == "error" {
				msg += "\n  error: " + is.Message
			}
		}
		return nil, &apiError{Status: resp.StatusCode, Msg: msg}
	}
	return b, nil
}

// findStack resolves a stack by id or name.
func (c ctl) findStack(ref string) (Stack, error) {
	var stacks []Stack
	if err := c.do("GET", "/api/stacks", nil, &stacks); err != nil {
		return Stack{}, err
	}
	id, _ := strconv.ParseInt(ref, 10, 64)
	for _, st := range stacks {
		if st.Name == ref || (id != 0 && st.ID == id) {
			return st, nil
		}
	}
	for _, st := range stacks {
		if strings.EqualFold(st.Name, ref) {
			return st, nil
		}
	}
	return Stack{}, fmt.Errorf("no stack called %q", ref)
}

func cliList(c ctl) error {
	var stacks []Stack
	if err := c.do("GET", "/api/stacks", nil, &stacks); err != nil {
		return err
	}
	if len(stacks) == 0 {
		fmt.Println("No stacks. Create one in the browser (dbcanvas) or from a file (dbcanvas up -f FILE).")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tTTL\tEXPIRES")
	for _, st := range stacks {
		exp := "-"
		if st.ExpiresAt != nil {
			exp = *st.ExpiresAt
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", st.ID, st.Name, st.Status, st.TTL, exp)
	}
	return tw.Flush()
}

func cliExport(args []string) error {
	fs := flag.NewFlagSet("dbcanvas export", flag.ContinueOnError)
	out := fs.String("o", "", "write to this file instead of standard output")
	stack, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(stack) != 1 {
		return errors.New("usage: dbcanvas export STACK [-o FILE]")
	}
	return withDBCanvas(func(c ctl) error {
		st, err := c.findStack(stack[0])
		if err != nil {
			return err
		}
		b, err := c.raw("GET", fmt.Sprintf("/api/stacks/%d/stackfile", st.ID), nil)
		if err != nil {
			return err
		}
		if *out == "" {
			_, err = os.Stdout.Write(b)
			return err
		}
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Wrote %s to %s.\n", st.Name, *out)
		return nil
	})
}

// cliStackAction runs one request against a named stack.
func cliStackAction(args []string, name, method, suffix, done string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: dbcanvas %s STACK", name)
	}
	return withDBCanvas(func(c ctl) error {
		st, err := c.findStack(args[0])
		if err != nil {
			return err
		}
		if err := c.do(method, fmt.Sprintf("/api/stacks/%d%s", st.ID, suffix), nil, nil); err != nil {
			return err
		}
		fmt.Printf(done+"\n", st.Name)
		return nil
	})
}

// errDeployFailed is a deploy that ended with a node failed or not up in time.
var errDeployFailed = errors.New("deploy failed")

func cliUp(args []string) error {
	fs := flag.NewFlagSet("dbcanvas up", flag.ContinueOnError)
	file := fs.String("f", "", "the stack file (- for standard input)")
	dry := fs.Bool("dry-run", false, "show what would change, and change nothing")
	wait := fs.Bool("wait", false, "wait until every node is running, or one fails")
	timeout := fs.Duration("timeout", time.Hour, "how long --wait waits")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if *file == "" || len(rest) > 0 {
		return errors.New("usage: dbcanvas up -f FILE [--dry-run] [--wait] [--timeout 1h]")
	}
	var raw []byte
	if *file == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*file)
	}
	if err != nil {
		return err
	}
	// Refuse a malformed file before starting anything.
	if _, err := parseStackFile(raw); err != nil {
		return fmt.Errorf("%s: %v", *file, err)
	}
	return withDBCanvas(func(c ctl) error {
		var p stackPlan
		if err := c.do("POST", "/api/stacks/apply", map[string]any{"file": string(raw), "dryRun": *dry}, &p); err != nil {
			return err
		}
		printPlan(p, *dry)
		if *dry {
			if !p.OK {
				return errors.New("the design has errors; nothing was changed")
			}
			return nil
		}
		if !p.OK {
			return fmt.Errorf("%s was saved, but its design has errors, so it was not deployed", p.Name)
		}
		if err := c.do("POST", fmt.Sprintf("/api/stacks/%d/deploy", p.StackID), nil, nil); err != nil {
			return err
		}
		if !*wait && !c.inProcess {
			fmt.Printf("Deploying %s. `dbcanvas ls` shows it; the browser shows each node.\n", p.Name)
			return nil
		}
		if !*wait {
			fmt.Println("No dbcanvas is running, so this command runs the deploy and waits for it.")
		}
		return waitDeploy(c, p.StackID, p.Name, *timeout)
	})
}

// parseInterspersed parses flags that may come before or after the arguments.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return rest, nil
		}
		rest = append(rest, args[0])
		args = args[1:]
	}
}

func printPlan(p stackPlan, dry bool) {
	verb := map[string]string{"create": "Created", "update": "Updated", "unchanged": "Unchanged"}[p.Action]
	if dry {
		verb = map[string]string{"create": "Would create", "update": "Would update", "unchanged": "No change to"}[p.Action]
	}
	fmt.Printf("%s %s\n", verb, p.Name)
	list := func(sign string, items []stackPlanItem) {
		for _, it := range items {
			extra := ""
			if it.Running {
				extra = " (deployed)"
			}
			fmt.Printf("  %s %-5s %s%s\n", sign, it.What, firstNonEmpty(it.Label, it.ID), extra)
		}
	}
	list("+", p.Added)
	list("-", p.Removed)
	list("~", p.Changed)
	if p.TTL != "" && p.Action != "create" {
		fmt.Printf("  ttl → %s (counted from now)\n", p.TTL)
	}
	if p.LabelsChange {
		fmt.Printf("  labels → %v\n", p.Labels)
	}
	for _, n := range p.Notes {
		fmt.Println("  note:", n)
	}
	for _, is := range p.Issues {
		if is.Level != "info" {
			fmt.Printf("  %s: %s\n", is.Level, is.Message)
		}
	}
}

// waitDeploy polls the stack until every node is running or one failed, printing each
// node's state as it changes. A node still being configured after its container started
// (a cluster forming, a replica seeding) is not done yet.
func waitDeploy(c ctl, id int64, name string, timeout time.Duration) error {
	type progress struct {
		Configuring bool   `json:"configuring"`
		ConfigPhase string `json:"configPhase"`
	}
	type dep struct {
		NodeID   string          `json:"nodeId"`
		State    string          `json:"state"`
		Progress json.RawMessage `json:"progress"`
	}
	deadline := time.Now().Add(timeout)
	seen := map[string]string{}
	for {
		var d struct {
			Deployments []dep `json:"deployments"`
		}
		if err := c.do("GET", "/api/stacks/"+url.PathEscape(strconv.FormatInt(id, 10)), nil, &d); err != nil {
			return err
		}
		running, failed, pending := 0, 0, 0
		for _, x := range d.Deployments {
			state := x.State
			var pr progress
			if json.Unmarshal(x.Progress, &pr) == nil && state == DeployRunning && pr.Configuring {
				state = strings.TrimSuffix("running, configuring: "+pr.ConfigPhase, ": ")
			}
			if seen[x.NodeID] != state {
				fmt.Printf("  %-24s %s\n", x.NodeID, state)
				seen[x.NodeID] = state
			}
			switch state {
			case DeployRunning:
				running++
			case DeployError:
				failed++
			default:
				pending++
			}
		}
		if len(d.Deployments) > 0 && pending == 0 {
			if failed > 0 {
				return fmt.Errorf("%w: %d of %d nodes failed in %s", errDeployFailed, failed, len(d.Deployments), name)
			}
			fmt.Printf("%s is up: %d node(s) running.\n", name, running)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s still had %d node(s) provisioning after %s", errDeployFailed, name, pending, timeout)
		}
		time.Sleep(3 * time.Second)
	}
}

// soloInstance is the instance name for this OS login: dbcanvas-<login>, or plain
// "dbcanvas-solo" when the login does not make a valid one (instance.go).
func soloInstance() string {
	if cu, err := user.Current(); err == nil {
		login := strings.Trim(ownerNameRe.ReplaceAllString(strings.ToLower(cu.Username), "-"), "-.")
		login = strings.ReplaceAll(login, ".", "-")
		if name := legacyInstance + "-" + login; login != "" && validInstance(name) == "" {
			return name
		}
	}
	return legacyInstance + "-solo"
}
