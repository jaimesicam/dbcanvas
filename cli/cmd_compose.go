package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// cmd_compose.go — `dbcanvas stack compose`.
//
// The point is to make the common ask a single line:
//
//	dbcanvas stack compose repro-1234 --ttl 8h \
//	  --node 'pxc:3,version=8.4.5,os=el8,monitor' \
//	  --node 'ps,version=8.0.45,os=el9,ldap,monitor' \
//	  --node 'pmm,version=3' \
//	  --deploy --wait
//
// The `--node` syntax is `kind[:count][,key=value|,flag]...`, which is the shape
// people already know from `docker run --mount` and `-o` mount options. It is a thin
// front end: everything it accepts maps to one field of the JSON the API takes, and
// `--spec file.json` sends that JSON directly for anything the flag form cannot say.
//
// Deploying is a SECOND request to the existing deploy endpoint, not a flag on
// compose. Composing and provisioning are different operations and the deploy path
// already handles backend pinning, the one-at-a-time guard and cancellation; a
// convenience flag that duplicated any of that would be a second deploy path to keep
// correct.

// parseNodeSpec turns "pxc:3,version=8.4.5,os=el8,monitor" into a spec entry.
func parseNodeSpec(arg string) (map[string]any, error) {
	parts := strings.Split(arg, ",")
	head := strings.TrimSpace(parts[0])
	if head == "" {
		return nil, fmt.Errorf("empty --node")
	}
	out := map[string]any{}

	// kind[:count]
	if i := strings.Index(head, ":"); i > 0 {
		n, err := strconv.Atoi(strings.TrimSpace(head[i+1:]))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%q: the count after ':' must be a positive number", head)
		}
		out["kind"] = strings.TrimSpace(head[:i])
		out["count"] = n
	} else {
		out["kind"] = head
	}

	// Options: bare words are booleans, key=value is a value. Listed explicitly
	// rather than passed through, so a typo is caught here with the valid set
	// rather than becoming a field the server silently ignores.
	boolOpts := map[string]string{
		// The relationships: each wires this node to another in the spec.
		"monitor": "monitor", "ldap": "ldap", "oidc": "oidc",
		"kerberos": "kerberos", "vault": "vault", "backup": "backup",
		"orchestrator": "orchestrator",
		// And the plain switches.
		"export": "export", "cert": "cert", "proxy": "proxy", "gtid": "gtid",
		"mysqlrouter": "mysqlRouter", "tls": "tls",
		"netalltraffic": "netAllTraffic",
	}
	strOpts := map[string]string{
		"name": "name", "os": "os", "arch": "arch", "version": "version",
		// "…With" names which provider, when a spec has more than one.
		"monitorwith": "monitorWith", "ldapwith": "ldapWith", "oidcwith": "oidcWith",
		"kerberoswith": "kerberosWith", "vaultwith": "vaultWith",
		"backupwith": "backupWith", "orchestratorwith": "orchestratorWith",
		// "to" is the association line — the backend a proxy fronts, the database a
		// simulator drives. Omit it and the server picks the only legal target, or
		// refuses and names the choices.
		"to": "to",
		// Per-engine.
		"replmode": "replMode", "mode": "mode", "setup": "setup",
		"dataset": "dataset", "certttl": "certTtl",
	}
	intOpts := map[string]string{
		"count": "count", "cpus": "cpus", "memorygb": "memoryGb", "exportport": "exportPort",
		// Resource shaping.
		"netlatencyms": "netLatencyMs", "netjitterms": "netJitterMs",
		"netratembit":    "netRateMbit",
		"devicereadmbps": "deviceReadMbps", "devicewritembps": "deviceWriteMbps",
	}
	// Two that are neither a string nor a whole number.
	floatOpts := map[string]string{"netlosspct": "netLossPct"}
	listOpts := map[string]string{"buckets": "buckets"}

	for _, raw := range parts[1:] {
		opt := strings.TrimSpace(raw)
		if opt == "" {
			continue
		}
		key, val, hasVal := strings.Cut(opt, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)

		if field, ok := boolOpts[key]; ok {
			b := true
			if hasVal {
				var err error
				if b, err = strconv.ParseBool(val); err != nil {
					return nil, fmt.Errorf("%s=%s: want true or false", key, val)
				}
			}
			out[field] = b
			continue
		}
		if field, ok := strOpts[key]; ok {
			if !hasVal {
				return nil, fmt.Errorf("%s needs a value, as %s=…", key, key)
			}
			out[field] = val
			continue
		}
		if field, ok := intOpts[key]; ok {
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("%s=%s: want a number", key, val)
			}
			out[field] = n
			continue
		}
		if field, ok := floatOpts[key]; ok {
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return nil, fmt.Errorf("%s=%s: want a number", key, val)
			}
			out[field] = f
			continue
		}
		if field, ok := listOpts[key]; ok {
			// "buckets=backups+wal" rather than a comma: commas already separate
			// options, and quoting one inside a --node value is a trap.
			out[field] = strings.Split(val, "+")
			continue
		}
		return nil, fmt.Errorf("unknown option %q in --node %q.\n"+
			"  links:   monitor, ldap, oidc, kerberos, vault, backup, orchestrator\n"+
			"           (each takes an optional <link>With= naming which node)\n"+
			"  wire to: to=<name>  — the backend a proxy fronts, the database a sim drives\n"+
			"  flags:   export, cert, proxy, gtid, mysqlRouter, tls, netAllTraffic\n"+
			"  values:  name=, os=, arch=, version=, count=, cpus=, memoryGb=, exportPort=,\n"+
			"           replMode=, mode=, setup=, dataset=, certTtl=, buckets=a+b\n"+
			"  shaping: netLatencyMs=, netJitterMs=, netLossPct=, netRateMbit=,\n"+
			"           deviceReadMbps=, deviceWriteMbps=\n"+
			"  `dbcanvas stack kinds` lists what each kind supports.", key, arg)
	}
	return out, nil
}

func stackCompose(args []string) error {
	fs := flagsFor("stack compose")
	var nodes multiFlag
	fs.Var(&nodes, "node", "kind[:count][,opt...] — repeatable; see `dbcanvas stack kinds`")
	specFile := fs.String("spec", "", "a JSON spec file (or - for stdin) instead of --node flags")
	ttl := fs.String("ttl", "8h", "lifetime: 2h, 4h, 8h, 24h, 2w or infinity")
	dryRun := fs.Bool("dry-run", false, "resolve and print the plan without creating anything")
	deploy := fs.Bool("deploy", false, "deploy it once composed")
	wait := fs.Bool("wait", false, "with --deploy, wait for every node to come up")
	if err := parse(fs, args); err != nil {
		return err
	}

	spec := map[string]any{"ttl": *ttl}
	switch {
	case *specFile != "":
		raw, err := readFileOrStdin(*specFile)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &spec); err != nil {
			return fmt.Errorf("%s is not valid JSON: %w", *specFile, err)
		}
		if spec["ttl"] == nil {
			spec["ttl"] = *ttl
		}
	case len(nodes) > 0:
		list := make([]map[string]any, 0, len(nodes))
		for _, n := range nodes {
			entry, err := parseNodeSpec(n)
			if err != nil {
				return err
			}
			list = append(list, entry)
		}
		spec["nodes"] = list
	default:
		fmt.Fprintln(os.Stderr,
			"dbcanvas: give at least one --node, or --spec file.json\n\n"+
				"  dbcanvas stack compose my-lab \\\n"+
				"    --node 'pxc:3,version=8.4.5,os=el8,monitor' \\\n"+
				"    --node 'ps,version=8.0.45,os=el9,ldap,monitor' \\\n"+
				"    --node 'pmm,version=3'\n\n"+
				"  `dbcanvas stack kinds` lists every kind and its options.")
		return errUsage
	}

	name := arg(0)
	if name == "" && !*dryRun {
		fmt.Fprintln(os.Stderr, "dbcanvas: a stack needs a name (or use --dry-run)")
		return errUsage
	}
	spec["name"] = name
	spec["dryRun"] = *dryRun

	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.request("POST", "/api/stacks/compose", spec, &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}

	var res struct {
		Stack    *Stack `json:"stack"`
		Resolved []struct {
			Kind, Name, OS, OSVersion, Arch string
			Major, Version                  string
			NodeIDs                         []string `json:"nodeIds"`
			FrameID                         string   `json:"frameId"`
			Links                           []string `json:"links"`
		} `json:"resolved"`
		Added  []string `json:"added"`
		Issues []struct {
			Level, Message string
		} `json:"issues"`
		OK bool `json:"ok"`
	}
	if err := jsonUnmarshal(raw, &res); err != nil {
		return err
	}

	// The plan. What each entry resolved to is the answer to "which build am I
	// actually reproducing on", so it is the table rather than a footnote.
	t := newTable("kind", "name", "os", "version", "nodes", "wired to")
	for _, r := range res.Resolved {
		ver := r.Version
		if ver == "" {
			ver = r.Major
			if ver == "" {
				ver = "-"
			} else {
				ver += " (latest)"
			}
		}
		osCol := strings.TrimSpace(r.OS + " " + r.OSVersion)
		t.add(r.Kind, r.Name, orDash(osCol), ver,
			strconv.Itoa(len(r.NodeIDs)), orDash(strings.Join(r.Links, "  ")))
	}
	t.print()

	for _, a := range res.Added {
		fmt.Printf("\n  + %s\n", a)
	}
	for _, i := range res.Issues {
		fmt.Printf("  %-8s %s\n", i.Level, i.Message)
	}

	if *dryRun {
		fmt.Println("\nDry run — nothing was created. Drop --dry-run to build it.")
		return nil
	}
	if res.Stack == nil {
		return fmt.Errorf("the server composed a design but returned no stack")
	}
	fmt.Printf("\nCreated %q (id %d), %s.\n", res.Stack.Name, res.Stack.ID, res.Stack.TTL)
	if !res.OK {
		return fmt.Errorf("the design has problems (above) — fix them before deploying")
	}
	if !*deploy {
		fmt.Printf("  Deploy it with `dbcanvas stack deploy %d --wait`.\n", res.Stack.ID)
		return nil
	}
	return deployStack(c, *res.Stack, *wait, 0)
}

// stackKinds prints the spec language, from the server, for the same reason
// `dbcanvas endpoints` does: the installation is the authority on what it can build.
func stackKinds(args []string) error {
	fs := flagsFor("stack kinds")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/stacks/compose/kinds", &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	var res struct {
		Kinds []struct {
			Kind, NodeType, Members, Catalog, About string
			Cluster                                 bool
			Options                                 []string
			Needs                                   []string
		} `json:"kinds"`
		Unsupported []struct{ Kind, Why string } `json:"unsupported"`
		OSAliases   []string                     `json:"osAliases"`
		DefaultOS   string                       `json:"defaultOS"`
		Notes       []string                     `json:"notes"`
	}
	if err := jsonUnmarshal(raw, &res); err != nil {
		return err
	}

	t := newTable("kind", "members", "options", "links to")
	for _, k := range res.Kinds {
		members := "-"
		if k.Cluster {
			members = k.Members
		}
		t.add(k.Kind, members, strings.Join(k.Options, " "), orDash(strings.Join(k.Needs, "  ")))
	}
	t.print()

	fmt.Printf("\nOS aliases (default %s):\n  %s\n", res.DefaultOS, strings.Join(res.OSAliases, " "))
	if len(res.Unsupported) > 0 {
		fmt.Println("\nNot built by compose — start from a template and edit the design:")
		for _, u := range res.Unsupported {
			fmt.Printf("  %-12s %s\n", u.Kind, u.Why)
		}
	}
	if len(res.Notes) > 0 {
		fmt.Println()
		for _, n := range res.Notes {
			fmt.Printf("  · %s\n", n)
		}
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
