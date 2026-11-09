package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"
)

// cmd_commands.go — `dbcanvas-cli commands`: every command, its arguments and its flags, as
// text or (--json, before the command) as data.
//
// It exists so that a program driving the CLI can learn what this build accepts instead of
// keeping its own copy of the command groups and flags, which goes stale with every release
// that adds one. The flags are not written down anywhere: each command is run in describe mode,
// in which parse (main.go) records the FlagSet it was handed and stops — every command parses
// its flags before it reads an argument or makes a request, which is also what makes -h safe —
// so the list is exactly what the commands themselves would accept.
//
// Compatibility: a command or flag is not renamed or removed without the old spelling being
// kept as an alias for at least one release (docs/CLI.md, "Compatibility").

// flagDesc is one flag as a command accepts it.
type flagDesc struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default string `json:"default,omitempty"`
	Usage   string `json:"usage"`
}

// commandDesc is one command or subcommand.
type commandDesc struct {
	Name    string     `json:"name"` // "stack list", "whoami"
	Args    string     `json:"args,omitempty"`
	Summary string     `json:"summary"`
	Flags   []flagDesc `json:"flags"`
}

// describing, when set, makes parse record the FlagSet instead of parsing.
var describing *[]flagDesc

// describeFlags turns a FlagSet into flag descriptions.
func describeFlags(fs *flag.FlagSet) []flagDesc {
	out := []flagDesc{}
	fs.VisitAll(func(f *flag.Flag) {
		out = append(out, flagDesc{Name: f.Name, Type: flagType(f), Default: f.DefValue, Usage: f.Usage})
	})
	return out
}

// flagType names a flag's value type the way a caller needs it: bool flags take no value.
func flagType(f *flag.Flag) string {
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return "bool"
	}
	t := fmt.Sprintf("%T", f.Value)
	switch {
	case strings.HasPrefix(t, "*flag."):
		return strings.TrimSuffix(strings.TrimPrefix(t, "*flag."), "Value")
	case strings.Contains(t, "multiFlag"):
		return "string (repeatable)"
	}
	return "string"
}

// describeCommands runs every command and subcommand in describe mode.
func describeCommands() []commandDesc {
	groups := map[string][]string{} // command → its subcommands, from the help table
	for k := range helpText {
		if c, sub, ok := strings.Cut(k, " "); ok {
			groups[c] = append(groups[c], sub)
		}
	}
	var out []commandDesc
	run := func(c command, name string, args []string) {
		got := []flagDesc{}
		describing = &got
		defer func() { describing = nil }()
		_ = c.run(args)
		h := helpText[name]
		summary := h.summary
		if summary == "" && len(args) == 0 {
			summary = c.summary
		}
		out = append(out, commandDesc{Name: name, Args: h.args, Summary: summary, Flags: got})
	}
	for _, c := range commands {
		if c.name == "help" {
			continue // `help <command>` is `<command> -h`; it has nothing of its own
		}
		subs := groups[c.name]
		if len(subs) == 0 {
			run(c, c.name, nil)
			continue
		}
		sort.Strings(subs)
		for _, s := range subs {
			run(c, c.name+" "+s, []string{s})
		}
	}
	return out
}

// globalFlagDescs are the flags that go before the command (main.go).
func globalFlagDescs() []flagDesc {
	return []flagDesc{
		{Name: "profile", Type: "string", Usage: "which configured installation to use"},
		{Name: "url", Type: "string", Usage: "DBCanvas base URL (overrides the profile and DBCANVAS_URL)"},
		{Name: "token", Type: "string", Usage: "API token (overrides the profile and DBCANVAS_TOKEN)"},
		{Name: "json", Type: "bool", Usage: "print machine-readable JSON"},
	}
}

func cmdCommands(args []string) error {
	fs := flagsFor("commands")
	if err := parse(fs, args); err != nil {
		return err
	}
	cmds := describeCommands()
	if g.json {
		return printJSON(map[string]any{"cli": version, "globalFlags": globalFlagDescs(), "commands": cmds})
	}
	for _, c := range cmds {
		line := c.Name
		if c.Args != "" {
			line += " " + c.Args
		}
		fmt.Printf("%s\n    %s\n", line, c.Summary)
		for _, f := range c.Flags {
			v := ""
			if f.Type != "bool" {
				v = " <" + f.Type + ">"
			}
			fmt.Printf("    --%s%s  %s\n", f.Name, v, f.Usage)
		}
	}
	return nil
}
