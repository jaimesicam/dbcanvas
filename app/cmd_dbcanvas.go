package main

import (
	"bytes"
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
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// cmd_dbcanvas.go — the dbcanvas command.
//
// Every command returns the prompt at once. One that needs dbcanvas's service starts it
// in the background when it is not running (soloservice.go) and hands it the work over
// the control socket (ctlsock.go); the service carries on — a deploy, expiry — after the
// command has returned, and stops by itself when nothing needs it.

// soloPort is the browser's port when APP_PORT does not set one; a free one is taken
// when it is busy.
const soloPort = "7480"

const cliUsage = `dbcanvas — database labs on Docker

Usage:
  dbcanvas                      open dbcanvas in the browser, signed in
  dbcanvas open [--print]       the same; --print prints the one-time sign-in link instead
                                (for another browser, or over an SSH tunnel)
  dbcanvas ls                   list your stacks
  dbcanvas export STACK [-o FILE]
                                write a stack as a stack file (YAML)
  dbcanvas up -f FILE [--platform amd64|arm64] [--dry-run] [--wait] [--timeout 1h]
                                create or update the stack in FILE, then deploy it;
                                --platform sets the database nodes' architecture
  dbcanvas down STACK           remove a stack's containers, keep its design
  dbcanvas rm STACK             delete a stack and everything it deployed
  dbcanvas token create NAME [--scope read|write] [--days 30] [--own-stacks]
                     [--label k=v]... [--max-ttl 8h] [--deny TYPE,...]
                                an API token for a script or another user of this computer
  dbcanvas token ls | token rm ID
  dbcanvas passwords [NAME]     the passwords of the accounts on nodes (root, admin, …):
                                generated on first run, or set in config.yaml's env:
  dbcanvas status               whether the dbcanvas service runs, and what keeps it up
  dbcanvas stop                 stop the service (stacks keep running; it starts again
                                with the next command that needs it)
  dbcanvas versions [update|reset]
                                show the version catalogue in use; update downloads the
                                newest into the data folder, reset goes back to the built-in one
  dbcanvas network check        where the next stack network would go, and what rules ranges out
  dbcanvas version              print the version

Any command that needs the service starts it, in the background, and returns at once.
It stops by itself when no stack has containers and nothing has used it for 10 minutes.

Data folder: %s (DBCANVAS_DATA overrides it). Settings are in its config.yaml;
the service's log is dbcanvas.log there.
`

func runCLI(args []string) int {
	log.SetFlags(log.LstdFlags)
	// ~/.dbcanvas/config.yaml, below the environment, before any default is chosen.
	switch firstArg(args) {
	case "help", "--help", "-h", "version", "--version", "-v":
	default:
		dir, err := ensureDataDir()
		if err == nil {
			err = applySoloConfig(dir)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "dbcanvas:", err)
			return 1
		}
	}
	// Everything this dbcanvas creates on the Docker host carries its instance name, so
	// several people can share one host (instance.go). It is the OS login unless set.
	// Read here, after config.yaml: instanceName was set when the program loaded, before it.
	if v := strings.TrimSpace(os.Getenv("DBCANVAS_INSTANCE")); v != "" {
		instanceName = v
	} else {
		instanceName = soloInstance()
	}
	// Node images for this machine's architecture, and the lab domain, unless set: the
	// container edition takes both from its .env. The service replaces the architecture with
	// the Docker host's own when it starts (detectPlatforms); this is the stand-in until then.
	platformsSet = soloPlatforms{docker: os.Getenv("DOCKER_PLATFORM") != "",
		k8s: os.Getenv("K8S_PLATFORM") != "" || os.Getenv("K3D_PLATFORM") != ""}
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
	case serviceArg:
		return runService()
	case "", "open":
		err = cliOpen(rest)
	case "status":
		err = cliStatus()
	case "stop":
		err = cliStop()
	case "passwords", "password":
		err = cliPasswords(rest)
	case "token", "tokens":
		err = cliToken(rest)
	case "serve":
		err = errors.New("dbcanvas has no serve command any more: any command starts it in the background (dbcanvas opens the browser)")
	case "ls", "list":
		err = withDBCanvas(func(c ctl) error { return cliList(c) })
	case "export":
		err = cliExport(rest)
	case "up":
		err = cliUp(rest)
	case "versions":
		err = cliVersions(rest)
	case "network":
		err = cliNetwork(rest)
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

// cliOpen opens dbcanvas in the browser, signed in, or prints the one-time link.
func cliOpen(args []string) error {
	fs := flag.NewFlagSet("dbcanvas open", flag.ContinueOnError)
	print := fs.Bool("print", false, "print the one-time sign-in link instead of opening a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := ensureDataDir()
	if err != nil {
		return err
	}
	if err := ensureService(dir); err != nil {
		return err
	}
	u, err := startLinkFrom(dir)
	if err != nil {
		return err
	}
	if *print {
		fmt.Println(u)
		fmt.Fprintln(os.Stderr, "It works once, for a minute.")
		return nil
	}
	openBrowser(u)
	return nil
}

func openBrowser(u string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	if err := exec.Command(cmd, u).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "could not open a browser (%v); open %s (it works once, for a minute)\n", err, u)
	}
}

// cliStatus says whether the service runs and what keeps it up. It never starts it.
func cliStatus() error {
	dir, err := ensureDataDir()
	if err != nil {
		return err
	}
	if !ctlReachable(dir) {
		fmt.Println("dbcanvas is not running. Any command that needs it starts it (dbcanvas opens the browser).")
		return nil
	}
	var st struct {
		PID       int           `json:"pid"`
		URL       string        `json:"url"`
		Version   string        `json:"version"`
		Instance  string        `json:"instance"`
		StartedAt string        `json:"startedAt"`
		Grace     int           `json:"idleGraceSeconds"`
		HostArch  string        `json:"hostArch"`
		Default   string        `json:"defaultArch"`
		K8s       string        `json:"kubernetesArch"`
		Keep      soloKeepAlive `json:"keepAlive"`
	}
	if err := (ctl{c: ctlClient(dir)}).do("GET", "/api/solo/status", nil, &st); err != nil {
		return err
	}
	fmt.Printf("dbcanvas %s is running (pid %d, since %s) at %s/\n", st.Version, st.PID, st.StartedAt, st.URL)
	fmt.Printf("Instance %s, data in %s\n", st.Instance, dir)
	fmt.Printf("Docker runs on %s; nodes default to %s, Kubernetes nodes run on %s\n", firstNonEmpty(st.HostArch, "an unknown architecture"), st.Default, st.K8s)
	var why []string
	if st.Keep.Containers > 0 {
		why = append(why, fmt.Sprintf("%d node(s) with containers", st.Keep.Containers))
	}
	if st.Keep.Deploying > 0 {
		why = append(why, fmt.Sprintf("%d deploy(s) running", st.Keep.Deploying))
	}
	if st.Keep.Connections > 0 {
		why = append(why, fmt.Sprintf("%d open connection(s): a browser, a guest or a waiting command", st.Keep.Connections))
	}
	if len(why) > 0 {
		fmt.Println("Kept up by:", strings.Join(why, "; "))
	} else {
		fmt.Printf("Nothing keeps it up: it stops %s after it was last used.\n", time.Duration(st.Grace)*time.Second)
	}
	return nil
}

// cliStop stops the service. It never starts it.
func cliStop() error {
	dir, err := ensureDataDir()
	if err != nil {
		return err
	}
	if !ctlReachable(dir) {
		fmt.Println("dbcanvas is not running.")
		return nil
	}
	var out struct {
		Keep soloKeepAlive `json:"keepAlive"`
	}
	if err := (ctl{c: ctlClient(dir)}).do("POST", "/api/solo/stop", nil, &out); err != nil {
		return err
	}
	for i := 0; i < 50 && ctlReachable(dir); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Println("dbcanvas stopped.")
	if out.Keep.Containers > 0 {
		fmt.Printf("Its %d node(s) keep running in Docker; nothing expires them until a dbcanvas command starts it again.\n", out.Keep.Containers)
	}
	return nil
}

// startLinkFrom asks the running service for a one-time sign-in link (soloweb.go).
func startLinkFrom(dir string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := (ctl{c: ctlClient(dir)}).do("POST", "/api/solo/start-link", nil, &out); err != nil {
		return "", err
	}
	return out.URL, nil
}

// withDBCanvas runs f against the dbcanvas service, starting it when it is not running.
func withDBCanvas(f func(c ctl) error) error {
	dir, err := ensureDataDir()
	if err != nil {
		return err
	}
	if err := ensureService(dir); err != nil {
		return err
	}
	return f(ctl{c: ctlClient(dir)})
}

// ctl calls the API over the control socket.
type ctl struct {
	c *http.Client
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
	platform := fs.String("platform", "", "amd64 or arm64 for the database nodes that do not choose their own (overrides the file's platform:)")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if *file == "" || len(rest) > 0 {
		return errors.New("usage: dbcanvas up -f FILE [--platform amd64|arm64] [--dry-run] [--wait] [--timeout 1h]")
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
	// Refuse a malformed file or platform before starting anything.
	if _, err := parseStackFile(raw); err != nil {
		return fmt.Errorf("%s: %v", *file, err)
	}
	if *platform != "" {
		if _, err := normalizePlatform(*platform); err != nil {
			return err
		}
	}
	return withDBCanvas(func(c ctl) error {
		var p stackPlan
		if err := c.do("POST", "/api/stacks/apply", map[string]any{"file": string(raw), "dryRun": *dry, "platform": *platform}, &p); err != nil {
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
		if !*wait {
			fmt.Printf("Deploying %s in the background. `dbcanvas ls` shows it; the browser shows each node.\n", p.Name)
			return nil
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
			var tags []string
			if it.Arch != "" {
				tags = append(tags, it.Arch)
			}
			if it.Emulated {
				tags = append(tags, "emulated")
			}
			if it.Running {
				tags = append(tags, "deployed")
			}
			extra := ""
			if len(tags) > 0 {
				extra = " (" + strings.Join(tags, ", ") + ")"
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

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// cliPasswords lists the node accounts' passwords, or prints one.
func cliPasswords(args []string) error {
	if len(args) > 1 {
		return errors.New("usage: dbcanvas passwords [NAME]")
	}
	return withDBCanvas(func(c ctl) error {
		var out struct {
			Passwords []nodePasswordInfo `json:"passwords"`
		}
		if err := c.do("GET", "/api/solo/passwords", nil, &out); err != nil {
			return err
		}
		if len(args) == 1 {
			want := strings.ToUpper(strings.TrimSpace(args[0]))
			if !strings.HasSuffix(want, "_PASSWORD") {
				want += "_PASSWORD"
			}
			for _, p := range out.Passwords {
				if p.Name == want {
					fmt.Println(p.Value)
					return nil
				}
			}
			return fmt.Errorf("no node password setting %s; `dbcanvas passwords` lists them", want)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "SETTING\tPASSWORD\tFROM")
		replaced := false
		for _, p := range out.Passwords {
			from := "generated"
			if p.Source == "config" {
				from = "config.yaml or the environment"
				if p.Replaced {
					from += " *"
					replaced = true
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.Value, from)
		}
		tw.Flush()
		if replaced {
			fmt.Println("\n* Replaces the generated one: nodes built before keep the generated password until they are rebuilt.")
		}
		fmt.Println("\nSet any of them under env: in ~/.dbcanvas/config.yaml; it applies to the nodes built after.")
		return nil
	})
}
