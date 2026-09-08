package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// cmd_stack.go — stacks, and the name resolution and waiting that make them usable
// from a script.
//
// Two things here are worth more than the commands themselves:
//
//   - Names, not just ids. Nobody remembers that the PXC lab is stack 47. resolveStack
//     accepts either, matches exactly before case-insensitively, and refuses rather
//     than guesses when two of your stacks share a name.
//   - `--wait`. POST /api/stacks/{id}/deploy returns as soon as provisioning has
//     *started*, which is right for a UI that then watches the node cards, and wrong
//     for a pipeline whose next step assumes a database. --wait polls to a terminal
//     state and exits 4 if it never got there, so a CI job fails where the problem is.

type Stack struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	TTL       string `json:"ttl"`
	CreatedAt string `json:"createdAt"`
	ExpiresAt string `json:"expiresAt"`
	OwnerID   int64  `json:"ownerId"`
}

type Deployment struct {
	StackID     int64  `json:"stackId"`
	NodeID      string `json:"nodeId"`
	ContainerID string `json:"containerId"`
	State       string `json:"state"`
}

type stackDetail struct {
	Stack
	Design      json.RawMessage `json:"design"`
	Deployments []Deployment    `json:"deployments"`
}

func cmdStack(args []string) error {
	return sub("stack", args, map[string]func([]string) error{
		"list":     stackList,
		"get":      stackGet,
		"create":   stackCreate,
		"compose":  stackCompose,
		"kinds":    stackKinds,
		"validate": stackValidate,
		"deploy":   stackDeploy,
		"destroy":  stackDestroy,
		"delete":   stackDelete,
		"export":   stackExport,
	}, []string{"list", "get", "create", "compose", "kinds", "validate", "deploy", "destroy", "delete", "export"})
}

// resolveStack turns a name or an id into a stack.
func resolveStack(c *Client, ref string) (Stack, error) {
	var stacks []Stack
	if err := c.get("/api/stacks", &stacks); err != nil {
		return Stack{}, err
	}
	// A pure number is an id. Names are allowed to look like numbers, so an id
	// match is checked first and a name match is still tried if it misses.
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		for _, s := range stacks {
			if s.ID == id {
				return s, nil
			}
		}
	}
	var exact, fuzzy []Stack
	for _, s := range stacks {
		if s.Name == ref {
			exact = append(exact, s)
		} else if strings.EqualFold(s.Name, ref) {
			fuzzy = append(fuzzy, s)
		}
	}
	for _, set := range [][]Stack{exact, fuzzy} {
		switch len(set) {
		case 1:
			return set[0], nil
		case 0:
			continue
		default:
			// Guessing which of two same-named stacks to deploy is not a risk worth
			// taking to save the user typing an id.
			var ids []string
			for _, s := range set {
				ids = append(ids, strconv.FormatInt(s.ID, 10))
			}
			return Stack{}, fmt.Errorf("%d stacks are called %q — use an id: %s",
				len(set), ref, strings.Join(ids, ", "))
		}
	}
	return Stack{}, fmt.Errorf("no stack called %q (try `dbcanvas stack list`)", ref)
}

func stackList(args []string) error {
	fs := flagsFor("stack list")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/stacks", &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	var stacks []Stack
	if err := json.Unmarshal(raw, &stacks); err != nil {
		return err
	}
	if len(stacks) == 0 {
		empty("stacks", "Create one with `dbcanvas stack create <name> --template <name>`.")
		return nil
	}
	t := newTable("id", "name", "status", "ttl", "expires", "created")
	for _, s := range stacks {
		t.add(strconv.FormatInt(s.ID, 10), truncate(s.Name, 32), s.Status, s.TTL,
			untilText(s.ExpiresAt), shortDate(s.CreatedAt))
	}
	t.print()
	return nil
}

func stackGet(args []string) error {
	fs := flagsFor("stack get")
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
	fmt.Printf("%-10s %s (id %d)\n", "stack", d.Name, d.ID)
	fmt.Printf("%-10s %s\n", "status", d.Status)
	fmt.Printf("%-10s %s%s\n", "ttl", d.TTL, ttlSuffix(d.ExpiresAt))
	fmt.Println()
	if len(d.Deployments) == 0 {
		empty("deployed nodes", "Deploy it with `dbcanvas stack deploy "+d.Name+" --wait`.")
		return nil
	}
	t := newTable("node", "state", "container")
	for _, dep := range d.Deployments {
		cid := dep.ContainerID
		if len(cid) > 12 {
			cid = cid[:12]
		}
		t.add(dep.NodeID, dep.State, cid)
	}
	t.print()
	return nil
}

// countSuffix renders " (2 warnings, 1 note)" for whatever non-error levels are
// present, or "" when there are none.
func countSuffix(byLevel map[string]int) string {
	var parts []string
	if n := byLevel["warning"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d warning%s", n, plural(n)))
	}
	if n := byLevel["info"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d note%s", n, plural(n)))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func ttlSuffix(expires string) string {
	if expires == "" {
		return ""
	}
	return " (" + untilText(expires) + " left, at " + shortTime(expires) + ")"
}

func stackCreate(args []string) error {
	fs := flagsFor("stack create")
	tpl := fs.String("template", "", "start from a template, by name or id")
	ttl := fs.String("ttl", "8h", "lifetime: 2h, 4h, 8h, 24h, 2w or infinity")
	design := fs.String("design", "", "path to a design JSON file, or - for stdin")
	deploy := fs.Bool("deploy", false, "deploy it immediately after creating it")
	wait := fs.Bool("wait", false, "with --deploy, wait for every node to come up")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a name for the stack"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}

	body := map[string]any{"name": arg(0), "ttl": *ttl}
	switch {
	case *design != "":
		raw, err := readFileOrStdin(*design)
		if err != nil {
			return err
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s is not valid JSON: %w", *design, err)
		}
		body["design"] = doc
	case *tpl != "":
		d, err := templateDesign(c, *tpl)
		if err != nil {
			return err
		}
		body["design"] = d
	}
	// With neither, the server creates an empty canvas, which is a reasonable
	// thing to want — `stack create scratch` then edit it in the UI.

	var created Stack
	if err := c.post("/api/stacks", body, &created); err != nil {
		return err
	}
	if g.json {
		return printJSON(created)
	}
	fmt.Printf("Created %q (id %d), %s.\n", created.Name, created.ID, created.TTL)
	if !*deploy {
		fmt.Printf("  Deploy it with `dbcanvas stack deploy %d --wait`.\n", created.ID)
		return nil
	}
	return deployStack(c, created, *wait, 0)
}

func stackValidate(args []string) error {
	fs := flagsFor("stack validate")
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
	if err := c.request("POST", fmt.Sprintf("/api/stacks/%d/validate", st.ID), nil, &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	// The server's field is "issues", not "problems" — an earlier version of this
	// decoded the wrong name, so every design "validated cleanly" whatever was wrong
	// with it. A validator that cannot fail is worse than no validator.
	var res struct {
		OK     bool `json:"ok"`
		Issues []struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		} `json:"issues"`
	}
	if err := jsonUnmarshal(raw, &res); err != nil {
		return err
	}
	if len(res.Issues) == 0 {
		fmt.Printf("%s validates cleanly.\n", st.Name)
		return nil
	}
	t := newTable("level", "problem")
	byLevel := map[string]int{}
	for _, p := range res.Issues {
		byLevel[p.Level]++
		t.add(p.Level, p.Message)
	}
	t.print()
	// Warnings and notes are worth printing and not worth failing over; an error is
	// what a script checking before a deploy needs to stop on. Counted per level,
	// because reporting an "info" line as a warning is a small lie that makes the
	// summary useless.
	if byLevel["error"] == 0 {
		fmt.Printf("\n%s has no errors%s.\n", st.Name, countSuffix(byLevel))
		return nil
	}
	return fmt.Errorf("%d error(s) in %s", byLevel["error"], st.Name)
}

func stackDeploy(args []string) error {
	fs := flagsFor("stack deploy")
	wait := fs.Bool("wait", false, "wait until every node is running, or something fails")
	timeout := fs.Duration("timeout", 60*time.Minute, "how long --wait will wait")
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
	return deployStack(c, st, *wait, *timeout)
}

func deployStack(c *Client, st Stack, wait bool, timeout time.Duration) error {
	if err := c.post(fmt.Sprintf("/api/stacks/%d/deploy", st.ID), nil, nil); err != nil {
		return err
	}
	if !wait {
		fmt.Printf("Deploying %s. Watch it with `dbcanvas stack get %d`.\n", st.Name, st.ID)
		return nil
	}
	if timeout == 0 {
		timeout = 60 * time.Minute
	}
	fmt.Printf("Deploying %s…\n", st.Name)
	return waitForNodes(c, st, timeout)
}

// waitForNodes polls the stack until every node reaches a terminal state, printing
// each change once.
//
// It reports per-node transitions rather than a spinner because the useful
// information during a five-minute deploy is *which* node is still provisioning —
// and because this output is often read later, in a CI log, where a spinner is noise.
func waitForNodes(c *Client, st Stack, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	seen := map[string]string{}
	for {
		var d stackDetail
		if err := c.get(fmt.Sprintf("/api/stacks/%d", st.ID), &d); err != nil {
			return err
		}
		running, failed, pending := 0, 0, 0
		for _, dep := range d.Deployments {
			if seen[dep.NodeID] != dep.State {
				fmt.Printf("  %-24s %s\n", dep.NodeID, dep.State)
				seen[dep.NodeID] = dep.State
			}
			switch dep.State {
			case "running":
				running++
			case "error":
				failed++
			default:
				pending++
			}
		}
		if len(d.Deployments) > 0 && pending == 0 {
			if failed > 0 {
				return fmt.Errorf("%w: %d of %d nodes failed in %s",
					errWaitFailed, failed, len(d.Deployments), st.Name)
			}
			fmt.Printf("%s is up: %d node(s) running.\n", st.Name, running)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s still had %d node(s) provisioning after %s",
				errWaitFailed, st.Name, pending, timeout)
		}
		time.Sleep(5 * time.Second)
	}
}

func stackDestroy(args []string) error {
	fs := flagsFor("stack destroy")
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
	if err := c.post(fmt.Sprintf("/api/stacks/%d/destroy", st.ID), nil, nil); err != nil {
		return err
	}
	fmt.Printf("Destroyed the nodes of %s. The design is kept — deploy it again any time.\n", st.Name)
	return nil
}

func stackDelete(args []string) error {
	fs := flagsFor("stack delete")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
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
	// Deleting takes the design with it, which is the part that cannot be rebuilt
	// from anything the CLI holds — so this is the one command that confirms.
	if !*yes {
		answer, err := prompt(fmt.Sprintf(
			"Delete %q (id %d) and its design? This cannot be undone. [y/N] ", st.Name, st.ID))
		if err != nil {
			return err
		}
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			fmt.Println("Left alone.")
			return nil
		}
	}
	if err := c.delete(fmt.Sprintf("/api/stacks/%d", st.ID), nil); err != nil {
		return err
	}
	fmt.Printf("Deleted %s.\n", st.Name)
	return nil
}

func stackExport(args []string) error {
	fs := flagsFor("stack export")
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
	var d stackDetail
	if err := c.get(fmt.Sprintf("/api/stacks/%d", st.ID), &d); err != nil {
		return err
	}
	return printJSON(map[string]any{"name": d.Name, "ttl": d.TTL, "design": d.Design})
}

// readFileOrStdin reads a path, or stdin when the path is "-".
func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		return readAll(os.Stdin)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return raw, nil
}
