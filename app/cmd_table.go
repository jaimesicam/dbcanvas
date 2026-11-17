package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// cmd_table.go — every `dbcanvas` command, its arguments and its flags, in one table. The help
// text (cliUsage), `dbcanvas commands` and each command's flag parsing (cmdFlags) are all built
// from it, so what the help says, what `commands --json` reports and what the binary accepts
// cannot drift apart.

// cliFlag is one flag. Kind is bool, string, int, duration or list (repeatable string).
type cliFlag struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Value   string `json:"value,omitempty"` // the placeholder in the usage: FILE, 8h, …
	Default string `json:"default,omitempty"`
	Help    string `json:"help"`
}

// cliCommand is one command. Name may be two words (token create). Stack is the position of
// the STACK argument among the positional arguments (1-based), 0 when there is none; Service
// is whether the command starts the service when it is not running.
type cliCommand struct {
	Name    string    `json:"name"`
	Args    string    `json:"args,omitempty"`
	Summary string    `json:"summary"`
	Flags   []cliFlag `json:"flags,omitempty"`
	Stack   int       `json:"stackArg,omitempty"`
	Service bool      `json:"startsService"`
	Hidden  bool      `json:"-"`
}

var jsonFlag = cliFlag{Name: "json", Kind: "bool", Help: "print JSON instead of text"}

var cliCommands = []cliCommand{
	{Name: "open", Service: true,
		Summary: "open dbcanvas in the browser, signed in (also what `dbcanvas` alone does)",
		Flags: []cliFlag{{Name: "print", Kind: "bool",
			Help: "print the one-time sign-in link instead (for another browser, or over an SSH tunnel)"}}},
	{Name: "ls", Service: true, Summary: "list your stacks", Flags: []cliFlag{jsonFlag}},
	{Name: "export", Args: "STACK", Stack: 1, Service: true,
		Summary: "write a stack as a stack file (YAML)",
		Flags:   []cliFlag{{Name: "o", Kind: "string", Value: "FILE", Help: "write to this file instead of standard output"}}},
	{Name: "up", Args: "[NAME]", Stack: 1, Service: true,
		Summary: "create or update a stack, from a stack file (-f) or from compose entries (NAME --node …), then deploy it",
		Flags: []cliFlag{
			{Name: "f", Kind: "string", Value: "FILE", Help: "the stack file (- for standard input)"},
			{Name: "node", Kind: "list", Value: "SPEC", Help: "a compose entry, e.g. 'pxc:3,version=8.4' (repeatable; with NAME instead of -f)"},
			{Name: "ttl", Kind: "string", Value: "8h", Help: "the stack's lifetime: 2h, 4h, 8h, 24h, 2w or 30d (with NAME)"},
			{Name: "label", Kind: "list", Value: "k=v", Help: "a label on the stack (repeatable; with NAME)"},
			{Name: "platform", Kind: "string", Value: "amd64|arm64", Help: "the architecture of the database nodes that do not choose their own"},
			{Name: "dry-run", Kind: "bool", Help: "show what would change, and change nothing"},
			{Name: "wait", Kind: "bool", Help: "wait until every node is running, or one fails"},
			{Name: "timeout", Kind: "duration", Value: "1h", Default: "1h", Help: "how long --wait waits"},
			jsonFlag,
		}},
	{Name: "down", Args: "STACK", Stack: 1, Service: true, Summary: "remove a stack's containers, keep its design"},
	{Name: "rm", Args: "STACK", Stack: 1, Service: true, Summary: "delete a stack and everything it deployed"},
	{Name: "exec", Args: "STACK NODE -- CMD [ARG...]", Stack: 1, Service: true,
		Summary: "run a command in a node; its output and exit status are the command's",
		Flags: []cliFlag{
			{Name: "user", Kind: "string", Value: "USER", Help: "run as this user (default: the node's, root)"},
			{Name: "timeout", Kind: "duration", Value: "10m", Help: "stop the command after this long (at most 1h)"},
			{Name: "i", Kind: "bool", Help: "pass standard input to the command (at most 16 MiB)"},
			jsonFlag,
		}},
	{Name: "cp", Args: "STACK SRC DST", Stack: 1, Service: true,
		Summary: "copy one file to or from a node: NODE:/path ./file, or ./file NODE:/path"},
	{Name: "token create", Args: "NAME", Service: true,
		Summary: "an API token for a script or another user of this computer",
		Flags: []cliFlag{
			{Name: "scope", Kind: "string", Value: "read|write|admin", Default: ScopeWrite, Help: "read (look only), write (create, deploy, change) or admin (also settings, tokens, images)"},
			{Name: "days", Kind: "int", Value: "30", Default: "30", Help: "days until it expires; 0 never expires"},
			{Name: "own-stacks", Kind: "bool", Help: "reach only the stacks this token creates"},
			{Name: "label", Kind: "list", Value: "k=v", Help: "reach only stacks with this label, which it puts on every stack it creates (repeatable)"},
			{Name: "max-ttl", Kind: "string", Value: "8h", Help: "the longest a stack it creates may live (2h, 4h, 8h, 24h, 2w, 30d); none extended"},
			{Name: "deny", Kind: "string", Value: "TYPE,...", Help: "node types it may not deploy, comma separated"},
		}},
	{Name: "token ls", Service: true, Summary: "list API tokens"},
	{Name: "token rm", Args: "ID", Service: true, Summary: "revoke an API token"},
	{Name: "passwords", Args: "[NAME]", Service: true,
		Summary: "the passwords of the accounts on nodes (root, admin, …): generated on first run, or set in config.yaml's env:"},
	{Name: "kinds", Summary: "the compose kinds `up --node` takes, with their options", Flags: []cliFlag{jsonFlag}},
	{Name: "commands", Summary: "every command with its arguments and flags", Flags: []cliFlag{jsonFlag}},
	{Name: "status", Summary: "whether the dbcanvas service runs, and what keeps it up", Flags: []cliFlag{jsonFlag}},
	{Name: "stop", Summary: "stop the service (stacks keep running; it starts again with the next command that needs it)"},
	{Name: "versions", Args: "[update|reset]",
		Summary: "show the version catalogue in use; update downloads the newest into the data folder, reset goes back to the built-in one"},
	{Name: "doctor", Summary: "check Docker, images, catalogues, data folder, networks, k3d, the bundled files and the service, and say how to fix what fails"},
	{Name: "network check", Summary: "where the next stack network would go, and what rules ranges out"},
	{Name: "rotate-key", Summary: "re-encrypt the database's secrets under a new key (stops the service while it runs; stacks keep running)"},
	{Name: "version", Summary: "print the version"},
	{Name: "help", Summary: "this help"},
}

func cliCommandByName(name string) cliCommand {
	for _, c := range cliCommands {
		if c.Name == name {
			return c
		}
	}
	panic("no command " + name + " in cliCommands")
}

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// cmdFlags is the flag set of a command, built from its table entry.
func cmdFlags(name string) *flag.FlagSet {
	c := cliCommandByName(name)
	fs := flag.NewFlagSet("dbcanvas "+name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s\n\n%s\n", usageLine(c), strings.ReplaceAll(c.Summary, "\n", " "))
		if len(c.Flags) > 0 {
			fmt.Fprintln(fs.Output())
			fs.PrintDefaults()
		}
	}
	for _, f := range c.Flags {
		switch f.Kind {
		case "bool":
			fs.Bool(f.Name, f.Default == "true", f.Help)
		case "string":
			fs.String(f.Name, f.Default, f.Help)
		case "int":
			n, _ := strconv.Atoi(f.Default)
			fs.Int(f.Name, n, f.Help)
		case "duration":
			d, _ := time.ParseDuration(f.Default)
			fs.Duration(f.Name, d, f.Help)
		case "list":
			fs.Var(&listFlag{}, f.Name, f.Help)
		default:
			panic("flag kind " + f.Kind)
		}
	}
	return fs
}

func flagString(fs *flag.FlagSet, name string) string { return fs.Lookup(name).Value.String() }
func flagBool(fs *flag.FlagSet, name string) bool     { return fs.Lookup(name).Value.String() == "true" }
func flagInt(fs *flag.FlagSet, name string) int {
	return fs.Lookup(name).Value.(flag.Getter).Get().(int)
}
func flagDuration(fs *flag.FlagSet, name string) time.Duration {
	return fs.Lookup(name).Value.(flag.Getter).Get().(time.Duration)
}
func flagList(fs *flag.FlagSet, name string) []string {
	return []string(*fs.Lookup(name).Value.(*listFlag))
}

// usageFlag is how a flag reads in the usage: [--wait], [-f FILE], [--node SPEC]...
func usageFlag(f cliFlag) string {
	dash := "--"
	if len(f.Name) == 1 {
		dash = "-"
	}
	s := dash + f.Name
	if f.Kind != "bool" {
		s += " " + f.Value
	}
	s = "[" + s + "]"
	if f.Kind == "list" {
		s += "..."
	}
	return s
}

// usageParts is a command's synopsis as unbreakable pieces: dbcanvas, its name, its
// arguments and each flag.
func usageParts(c cliCommand) []string {
	parts := []string{"dbcanvas " + c.Name}
	pre, post, dashed := strings.Cut(c.Args, " -- ")
	if !dashed {
		pre = c.Args
	}
	if pre != "" {
		parts = append(parts, pre)
	}
	// Flags go before a `--` command, after the other arguments.
	for _, f := range c.Flags {
		parts = append(parts, usageFlag(f))
	}
	if dashed {
		parts = append(parts, "-- "+post)
	}
	return parts
}

// usageLine is a command's synopsis: dbcanvas NAME ARGS [FLAGS].
func usageLine(c cliCommand) string { return strings.Join(usageParts(c), " ") }

// wrapParts joins pieces into lines of at most width, never breaking a piece.
func wrapParts(parts []string, width int, first, rest string) []string {
	var out []string
	line := first
	for _, w := range parts {
		if strings.TrimSpace(line) != "" && len(line)+1+len(w) > width {
			out = append(out, line)
			line = rest
		}
		if strings.TrimSpace(line) == "" {
			line += w
		} else {
			line += " " + w
		}
	}
	return append(out, line)
}

// wrapWords breaks s into lines of at most width, the first line indented by first, the rest by
// rest.
func wrapWords(s string, width int, first, rest string) []string {
	return wrapParts(strings.Fields(s), width, first, rest)
}

// usageText is the help, with one %s for the data folder.
func usageText() string {
	const col, width = 32, 96
	sumWidth := width - col
	var b strings.Builder
	b.WriteString("dbcanvas — database labs on Docker\n\nUsage:\n")
	pad := strings.Repeat(" ", col)
	for _, c := range cliCommands {
		if c.Hidden {
			continue
		}
		head := wrapParts(usageParts(c), width, "  ", "      ")
		var summary []string
		for _, para := range strings.Split(c.Summary, "\n") {
			for _, l := range wrapWords(para, sumWidth, "", "") {
				summary = append(summary, l)
			}
		}
		if len(head) == 1 && len(head[0]) < col-1 {
			b.WriteString(head[0] + strings.Repeat(" ", col-len(head[0])) + summary[0] + "\n")
			summary = summary[1:]
		} else {
			for _, h := range head {
				b.WriteString(h + "\n")
			}
		}
		for _, s := range summary {
			b.WriteString(pad + s + "\n")
		}
	}
	b.WriteString(`
Any command that needs the service starts it, in the background, and returns at once.
It stops by itself when no stack has containers and nothing has used it for 10 minutes.
STACK is a stack's name or id; NODE is a node's label (ps-01) or id.
Exit status: 0 on success, 1 on an error, 2 for a usage error, 3 when a deploy failed;
exec exits with the command's own status.

Data folder: %s (DBCANVAS_DATA overrides it). Settings are in its config.yaml;
the service's log is dbcanvas.log there.
`)
	return strings.ReplaceAll(strings.ReplaceAll(b.String(), "%", "%%"), "%%s (DBCANVAS_DATA", "%s (DBCANVAS_DATA")
}

var cliUsage = usageText()

// cliCommandsList is `dbcanvas commands`.
func cliCommandsList(args []string) error {
	fs := cmdFlags("commands")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErr("commands")
	}
	visible := []cliCommand{}
	for _, c := range cliCommands {
		if !c.Hidden {
			visible = append(visible, c)
		}
	}
	if flagBool(fs, "json") {
		return printJSON(map[string]any{"version": appVersion, "commands": visible})
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, c := range visible {
		fmt.Fprintf(tw, "%s\t%s\n", usageLine(c), strings.ReplaceAll(c.Summary, "\n", " "))
	}
	return tw.Flush()
}

// cliKinds is `dbcanvas kinds`: the compose language, read from this binary (no service).
func cliKinds(args []string) error {
	fs := cmdFlags("kinds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErr("kinds")
	}
	doc := composeKindsDoc()
	if flagBool(fs, "json") {
		return printJSON(doc)
	}
	b, _ := json.Marshal(doc)
	var d struct {
		Kinds []struct {
			Kind    string   `json:"kind"`
			Cluster bool     `json:"cluster"`
			Members string   `json:"members"`
			Options []string `json:"options"`
			Needs   []string `json:"needs"`
			About   string   `json:"about"`
		} `json:"kinds"`
		Unsupported []map[string]string `json:"unsupported"`
		OSAliases   []string            `json:"osAliases"`
		DefaultOS   string              `json:"defaultOS"`
		Notes       []string            `json:"notes"`
	}
	json.Unmarshal(b, &d)
	sort.Slice(d.Kinds, func(i, j int) bool { return d.Kinds[i].Kind < d.Kinds[j].Kind })
	for _, k := range d.Kinds {
		for _, l := range wrapWords(k.Kind+" — "+k.About, 96, "", "    ") {
			fmt.Println(l)
		}
		if k.Members != "" {
			fmt.Printf("    members: %s\n", k.Members)
		}
		for _, l := range wrapWords("options: "+strings.Join(k.Options, ", "), 96, "    ", "      ") {
			fmt.Println(l)
		}
		for _, n := range k.Needs {
			fmt.Printf("    needs: %s\n", n)
		}
	}
	fmt.Println()
	fmt.Println("An entry is KIND[:COUNT][,OPTION=VALUE|,FLAG]..., e.g. 'pxc:3,version=8.4,os=el9,monitor'.")
	fmt.Printf("OS: %s (default %s).\n", strings.Join(d.OSAliases, ", "), d.DefaultOS)
	for _, n := range d.Notes {
		fmt.Println(n)
	}
	if len(d.Unsupported) > 0 {
		fmt.Println("\nNot built by compose (start from a template in the browser instead):")
		for _, u := range d.Unsupported {
			fmt.Printf("  %s: %s\n", u["kind"], u["why"])
		}
	}
	return nil
}

// printJSON writes v as indented JSON on standard output.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// errUsage is a usage error: exit status 2.
type errUsage struct{ msg string }

func (e errUsage) Error() string { return e.msg }

// usageErr is the usage error of one command, its synopsis.
func usageErr(name string) error {
	return errUsage{"usage: " + usageLine(cliCommandByName(name))}
}

// flagErr is a flag parse error, which the flag package has already reported with the
// command's usage: -h asked for that usage (exit 0), anything else is a usage error (exit 2).
func flagErr(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return errExit{0}
	}
	return errExit{2}
}

// errExit ends the program with a command's own exit status, having already said everything.
type errExit struct{ code int }

func (e errExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }
