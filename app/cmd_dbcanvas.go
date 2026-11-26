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
const soloPort = "8080"

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
		err = cliStatus(rest)
	case "stop":
		err = cliStop()
	case "passwords", "password":
		err = cliPasswords(rest)
	case "token", "tokens":
		err = cliToken(rest)
	case "serve":
		err = errors.New("dbcanvas has no serve command any more: any command starts it in the background (dbcanvas opens the browser)")
	case "ls", "list":
		err = cliList(rest)
	case "exec":
		err = cliExec(rest)
	case "cp":
		err = cliCp(rest)
	case "kinds":
		err = cliKinds(rest)
	case "commands":
		err = cliCommandsList(rest)
	case "export":
		err = cliExport(rest)
	case "up":
		err = cliUp(rest)
	case "versions":
		err = cliVersions(rest)
	case "network":
		err = cliNetwork(rest)
	case "rotate-key":
		err = cliRotateKey(rest)
	case "doctor":
		err = cliDoctor(rest)
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
		var exit errExit
		if errors.As(err, &exit) {
			return exit.code
		}
		fmt.Fprintln(os.Stderr, "dbcanvas:", err)
		var usage errUsage
		switch {
		case errors.As(err, &usage):
			return 2
		case errors.Is(err, errDeployFailed):
			return 3
		}
		return 1
	}
	return 0
}

// cliOpen opens dbcanvas in the browser, signed in, or prints the one-time link.
func cliOpen(args []string) error {
	fs := cmdFlags("open")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr("open")
	}
	print := flagBool(fs, "print")
	dir, err := ensureDataDir()
	if err != nil {
		return err
	}
	if err := ensureService(dir); err != nil {
		return err
	}
	link, err := startLinkFrom(dir)
	if err != nil {
		return err
	}
	if print {
		// The 127.0.0.1 link first: a program reads the first line.
		fmt.Println(link.URL)
		if link.PublicURL != "" {
			fmt.Println(link.PublicURL)
		}
		fmt.Fprintln(os.Stderr, "It works once, for 3 minutes.")
		return nil
	}
	if why := noBrowserHere(); why != "" {
		fmt.Print(remoteSignIn(link, why))
		return nil
	}
	openBrowser(link.URL)
	return nil
}

// noBrowserHere says why this terminal has no browser to open, or "" when it may have one:
// an SSH session (the browser is on the other end), or Linux with no display (a server).
func noBrowserHere() string {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return "this is an SSH session"
	}
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return "this machine has no display"
	}
	return ""
}

// remoteSignIn is what `dbcanvas` prints where it cannot open a browser: how to reach the
// port from another computer through an SSH tunnel, and the one-time link to open there —
// also the publicUrl one when a tunnel or reverse proxy of the owner's own is configured.
// DBCanvas answers only on 127.0.0.1:PORT, so the tunnel keeps the same port on both ends.
func remoteSignIn(link startLink, why string) string {
	port := "8080"
	if u, err := url.Parse(link.URL); err == nil && u.Port() != "" {
		port = u.Port()
	}
	login := "you"
	if cu, err := user.Current(); err == nil && cu.Username != "" {
		login = cu.Username
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "this-machine"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "DBCanvas is running on 127.0.0.1:%s, but %s, so no browser was opened.\n\n", port, why)
	fmt.Fprintf(&b, "From your own computer, forward the port (the same number on both ends):\n\n")
	fmt.Fprintf(&b, "  ssh -N -L %s:127.0.0.1:%s %s@%s\n\n", port, port, login, host)
	fmt.Fprintf(&b, "then open this link there, within 3 minutes:\n\n  %s\n\n", link.URL)
	if link.PublicURL != "" {
		fmt.Fprintf(&b, "Or, through publicUrl (your own tunnel or reverse proxy):\n\n  %s\n\n", link.PublicURL)
	}
	fmt.Fprintf(&b, "A link works once. `dbcanvas open --print` prints a new one.\n")
	return b.String()
}

func openBrowser(u string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	if err := exec.Command(cmd, u).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "could not open a browser (%v); open %s (it works once, for 3 minutes)\n", err, u)
	}
}

// cliStatus says whether the service runs and what keeps it up. It never starts it.
func cliStatus(args []string) error {
	fs := cmdFlags("status")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr("status")
	}
	asJSON := flagBool(fs, "json")
	dir, err := ensureDataDir()
	if err != nil {
		return err
	}
	if !ctlReachable(dir) {
		if asJSON {
			return printJSON(map[string]any{"running": false, "version": appVersion, "dataDir": dir})
		}
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
		Updated   bool          `json:"binaryUpdated"`
		Keep      soloKeepAlive `json:"keepAlive"`
	}
	raw, err := (ctl{c: ctlClient(dir)}).raw("GET", "/api/solo/status", nil)
	if err != nil {
		return err
	}
	if asJSON {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		m["running"], m["dataDir"] = true, dir
		return printJSON(m)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	fmt.Printf("dbcanvas %s is running (pid %d, since %s) at %s/\n", st.Version, st.PID, st.StartedAt, st.URL)
	fmt.Printf("Instance %s, data in %s\n", st.Instance, dir)
	fmt.Printf("Docker runs on %s; nodes default to %s, Kubernetes nodes run on %s\n", firstNonEmpty(st.HostArch, "an unknown architecture"), st.Default, st.K8s)
	if st.Updated {
		fmt.Println("Its binary was updated since it started: it restarts as the new one once no deploy runs and nothing is connected (close browser tabs, or run dbcanvas stop).")
	}
	var why []string
	if st.Keep.Containers > 0 {
		why = append(why, fmt.Sprintf("%d node(s) with containers", st.Keep.Containers))
	}
	if st.Keep.VMs > 0 {
		why = append(why, fmt.Sprintf("%d of them VM(s), which reach their stack network only while dbcanvas runs", st.Keep.VMs))
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
		fmt.Printf("Its %d node(s) keep running; nothing expires them until a dbcanvas command starts it again.\n", out.Keep.Containers)
	}
	if out.Keep.VMs > 0 {
		fmt.Printf("%d of them run as VMs, whose link to their stack network is down until then: dbcanvas carries it.\n", out.Keep.VMs)
	}
	return nil
}

// startLink is a one-time sign-in link: on 127.0.0.1, and on publicUrl when one is set.
type startLink struct {
	URL       string `json:"url"`
	PublicURL string `json:"publicUrl,omitempty"`
}

// startLinkFrom asks the running service for a one-time sign-in link (soloweb.go).
func startLinkFrom(dir string) (startLink, error) {
	var out startLink
	err := (ctl{c: ctlClient(dir)}).do("POST", "/api/solo/start-link", nil, &out)
	return out, err
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

func cliList(args []string) error {
	fs := cmdFlags("ls")
	if err := fs.Parse(args); err != nil {
		return flagErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr("ls")
	}
	return withDBCanvas(func(c ctl) error { return listStacks(c, flagBool(fs, "json")) })
}

func listStacks(c ctl, asJSON bool) error {
	var stacks []stackSummary
	if err := c.do("GET", "/api/stacks?detail=1", nil, &stacks); err != nil {
		return err
	}
	if asJSON {
		type row struct {
			ID        int64             `json:"id"`
			Name      string            `json:"name"`
			State     string            `json:"state"`
			Status    string            `json:"status"`
			Nodes     stackNodeCount    `json:"nodes"`
			TTL       string            `json:"ttl"`
			ExpiresAt *string           `json:"expiresAt"`
			Labels    map[string]string `json:"labels,omitempty"`
		}
		out := []row{}
		for _, st := range stacks {
			out = append(out, row{st.ID, st.Name, stackState(st), st.Status, st.Nodes, st.TTL, st.ExpiresAt, st.Labels})
		}
		return printJSON(out)
	}
	if len(stacks) == 0 {
		fmt.Println("No stacks. Create one in the browser (dbcanvas) or from a file (dbcanvas up -f FILE).")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tNODES\tTTL\tEXPIRES")
	for _, st := range stacks {
		exp := "-"
		if st.ExpiresAt != nil {
			exp = *st.ExpiresAt
		}
		nodes := fmt.Sprintf("%d/%d", st.Nodes.Running, st.Nodes.Total)
		if st.Nodes.Error > 0 {
			nodes += fmt.Sprintf(", %d failed", st.Nodes.Error)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", st.ID, st.Name, stackState(st), nodes, st.TTL, exp)
	}
	return tw.Flush()
}

// stackState is what `ls` says a stack is doing, from its nodes rather than its record:
// a stack is marked deployed as soon as its deploy starts, while its nodes still come up.
//
//	running        every node is running
//	deploying      deployed, and some nodes are not running yet
//	failed         a node is in error
//	not deployed   nothing is running (never deployed, or taken down)
//	expired        its TTL ran out and it was torn down
func stackState(s stackSummary) string {
	switch {
	case s.Status == StackExpired:
		return "expired"
	case s.Nodes.Error > 0:
		return "failed"
	case s.Nodes.Total > 0 && s.Nodes.Running >= s.Nodes.Total:
		return "running"
	case s.Status == StackDeployed:
		return "deploying"
	}
	return "not deployed"
}

func cliExport(args []string) error {
	fs := cmdFlags("export")
	stack, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	if len(stack) != 1 {
		return usageErr("export")
	}
	out := new(string)
	*out = flagString(fs, "o")
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
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Printf("usage: %s\n\n%s\n", usageLine(cliCommandByName(name)), cliCommandByName(name).Summary)
		return nil
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return usageErr(name)
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
	fs := cmdFlags("up")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagErr(err)
	}
	file, nodes := flagString(fs, "f"), flagList(fs, "node")
	ttl, labels := flagString(fs, "ttl"), flagList(fs, "label")
	dry, wait, asJSON := flagBool(fs, "dry-run"), flagBool(fs, "wait"), flagBool(fs, "json")
	timeout, platform := flagDuration(fs, "timeout"), flagString(fs, "platform")
	var raw []byte
	switch {
	case file != "" && (len(pos) > 0 || len(nodes) > 0 || ttl != "" || len(labels) > 0):
		return errUsage{"up takes a stack file (-f) or a NAME with --node entries, not both; --ttl and --label go with NAME (a file has its own ttl: and labels:)"}
	case file != "":
		if file == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(file)
		}
		if err != nil {
			return err
		}
	case len(pos) == 1 && len(nodes) > 0:
		raw, err = composeStackFile(pos[0], nodes, ttl, labels)
		if err != nil {
			return flagErr(err)
		}
		file = "the command line"
	default:
		return usageErr("up")
	}
	// Refuse a malformed file or platform before starting anything.
	if _, err := parseStackFile(raw); err != nil {
		return fmt.Errorf("%s: %v", file, err)
	}
	if platform != "" {
		if _, err := normalizePlatform(platform); err != nil {
			return err
		}
	}
	return withDBCanvas(func(c ctl) error {
		var p stackPlan
		if err := c.do("POST", "/api/stacks/apply", map[string]any{"file": string(raw), "dryRun": dry, "platform": platform}, &p); err != nil {
			return err
		}
		report := map[string]any{"plan": p, "deployed": false}
		finish := func(err error) error {
			if asJSON {
				printJSON(report)
			}
			return err
		}
		if !asJSON {
			printPlan(p, dry)
		}
		if dry {
			if !p.OK {
				return finish(errors.New("the design has errors; nothing was changed"))
			}
			return finish(nil)
		}
		if !p.OK {
			return finish(fmt.Errorf("%s was saved, but its design has errors, so it was not deployed", p.Name))
		}
		if err := c.do("POST", fmt.Sprintf("/api/stacks/%d/deploy", p.StackID), nil, nil); err != nil {
			return finish(err)
		}
		report["deployed"] = true
		if !wait {
			if !asJSON {
				fmt.Printf("Deploying %s in the background. `dbcanvas ls` shows it; the browser shows each node.\n", p.Name)
			}
			return finish(nil)
		}
		out := io.Writer(os.Stdout)
		if asJSON {
			out = io.Discard
		}
		states, err := waitDeploy(c, p.StackID, p.Name, timeout, out)
		report["nodes"] = states
		if err != nil && !asJSON {
			for _, n := range states {
				if n.Error != "" {
					fmt.Fprintf(os.Stderr, "  %s: %s\n", firstNonEmpty(n.Label, n.NodeID), n.Error)
				}
			}
		}
		return finish(err)
	})
}

// composeStackFile is the stack file `up NAME --node …` stands for.
func composeStackFile(name string, nodes []string, ttl string, labels []string) ([]byte, error) {
	sf := map[string]any{"apiVersion": stackFileAPIVersion, "kind": "Stack", "name": name, "nodes": nodes}
	if ttl != "" {
		sf["ttl"] = ttl
	}
	if len(labels) > 0 {
		m := map[string]string{}
		for _, l := range labels {
			k, v, ok := strings.Cut(l, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return nil, fmt.Errorf("--label %q: want key=value", l)
			}
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		sf["labels"] = m
	}
	return json.Marshal(sf)
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
	for _, r := range p.Resolved {
		v := r.Version
		if v == "" && r.Major != "" {
			v = r.Major + " (its newest build at deploy)"
		}
		if v == "" {
			continue
		}
		fmt.Printf("  = %s: %s %s on %s %s (%s)\n", r.Name, r.Kind, v, r.OS, r.OSVersion, r.Arch)
	}
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
// nodeState is a node's state at the end of `up --wait`.
type nodeState struct {
	NodeID string `json:"nodeId"`
	Label  string `json:"label"`
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
}

func waitDeploy(c ctl, id int64, name string, timeout time.Duration, w io.Writer) ([]nodeState, error) {
	type progress struct {
		Configuring bool   `json:"configuring"`
		ConfigPhase string `json:"configPhase"`
		Message     string `json:"message"`
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
			Design struct {
				Nodes []struct {
					ID    string `json:"id"`
					Label string `json:"label"`
				} `json:"nodes"`
			} `json:"design"`
			Deployments []dep `json:"deployments"`
		}
		if err := c.do("GET", "/api/stacks/"+url.PathEscape(strconv.FormatInt(id, 10)), nil, &d); err != nil {
			return nil, err
		}
		labels := map[string]string{}
		for _, n := range d.Design.Nodes {
			labels[n.ID] = n.Label
		}
		states := []nodeState{}
		running, failed, pending := 0, 0, 0
		for _, x := range d.Deployments {
			state := x.State
			var pr progress
			if json.Unmarshal(x.Progress, &pr) == nil && state == DeployRunning && pr.Configuring {
				state = strings.TrimSuffix("running, configuring: "+pr.ConfigPhase, ": ")
			}
			if seen[x.NodeID] != state {
				fmt.Fprintf(w, "  %-24s %s\n", firstNonEmpty(labels[x.NodeID], x.NodeID), state)
				seen[x.NodeID] = state
			}
			ns := nodeState{NodeID: x.NodeID, Label: labels[x.NodeID], State: x.State}
			if x.State == DeployError {
				ns.Error = pr.Message
			}
			states = append(states, ns)
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
				return states, fmt.Errorf("%w: %d of %d nodes failed in %s", errDeployFailed, failed, len(d.Deployments), name)
			}
			fmt.Fprintf(w, "%s is up: %d node(s) running.\n", name, running)
			return states, nil
		}
		if time.Now().After(deadline) {
			return states, fmt.Errorf("%w: %s still had %d node(s) provisioning after %s", errDeployFailed, name, pending, timeout)
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
