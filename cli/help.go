package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// help.go — what `-h`, `--help` and `dbcanvas-cli help …` print.
//
// Every command parses its flags first (flagsFor + parse), before it reads an argument or
// makes a request, so `-h` anywhere is safe: it prints the command's usage line, what it does
// and its flags, and exits 0. `dbcanvas-cli help node exec` is the same as
// `dbcanvas-cli node exec -h`, and `dbcanvas-cli help node` lists the group's subcommands.
//
// The flags come from the FlagSet itself, so they cannot drift; what is written here is only the
// positional arguments and one line about what the command is for.

type cmdHelp struct {
	args    string // the positional arguments, as they go after the command
	summary string
}

var helpText = map[string]cmdHelp{
	"login":         {"", "Sign in with a password (or --guest) and store an API token for this installation."},
	"logout":        {"", "Revoke the stored token and forget this installation."},
	"whoami":        {"", "Who you are signed in as, on which installation, and the token's scope and expiry."},
	"password":      {"", "Change your own password."},
	"api":           {"[METHOD] <path>", "Call any endpoint: GET by default, POST when --data is given. `endpoints` lists them."},
	"endpoints":     {"", "List the API endpoints this installation serves."},
	"dashboard":     {"", "The dashboard's counters; --live adds a CPU, memory and network sample per node."},
	"updates":       {"", "Whether a newer DBCanvas release is available, and what it brings."},
	"notifications": {"", "Your notifications; --read-all marks them read."},

	"stack list":     {"", "Your stacks: id, name, status, nodes, expiry."},
	"stack get":      {"<stack>", "A stack's design and every node's state."},
	"stack create":   {"<name>", "Create a stack from a template (--template) or a design file (--design)."},
	"stack compose":  {"<name>", "Build a stack from one --node per node, or a --spec file. `stack kinds` lists what each kind takes."},
	"stack kinds":    {"", "Every kind compose builds, with its options and relationships."},
	"stack validate": {"<stack>", "Check a stack's design without building anything."},
	"stack deploy":   {"<stack>", "Deploy a stack; --wait waits for every node."},
	"stack destroy":  {"<stack>", "Remove a stack's containers and keep its design."},
	"stack delete":   {"<stack>", "Remove a stack's containers and its design."},
	"stack export":   {"<stack>", "Print a stack's design as JSON."},

	"node list":    {"<stack>", "A stack's nodes: name, node id, state, container."},
	"node start":   {"<stack> <node>", "Start a node's container."},
	"node stop":    {"<stack> <node>", "Stop a node's container."},
	"node restart": {"<stack> <node>", "Restart a node's container."},
	"node console": {"<stack> <node>", "An interactive root shell in a node (or, with --pod, in a pod of the Kubernetes cluster it runs)."},
	"node pods":    {"<stack> <node>", "The pods and containers of the Kubernetes cluster a server node runs."},
	"node exec":    {"<stack> <node> -- <command> [args…]", "Run one command in a node and print its output. Exits with the command's own status."},
	"node cp":      {"<src> <dst>", "Copy a file to or from a node; the node side is <stack>:<node>:<path>."},
	"node tunnel":  {"<stack> <node>", "Print the ssh -L command that forwards a node's ports (when SSH forwarding is configured)."},

	"template list":   {"", "The deployment templates you can create a stack from."},
	"template export": {"<template>", "Print a template's design as JSON."},
	"template import": {"<file | ->", "Add a template from an exported file, or from stdin."},

	"token list":   {"", "Your API tokens."},
	"token create": {"", "Create an API token; --name is required."},
	"token revoke": {"<id>", "Revoke an API token; it stops working at once."},
	"user list":    {"", "Every account (administrators)."},
	"user create":  {"<username>", "Create an account (administrators)."},
	"group list":   {"", "Which pages each security group may use (administrators)."},
	"group set":    {"<admin|user|guest> <feature>=on|off …", "Allow or deny pages for a security group (administrators)."},

	"datagen connections": {"", "The servers the Data Generator can fill."},
	"datagen databases":   {"<stack> <node>", "A server's databases."},
	"datagen tables":      {"<stack> <node>", "A database's tables and their row counts."},
	"datagen run":         {"<stack> <node>", "Fill a table with generated rows; --table and --rows are required."},
	"query targets":       {"", "The servers the Query Runner can run SQL on."},
	"query run":           {"", "Run SQL on several nodes at once; --stack, --nodes and --sql are required."},
	"query history":       {"", "Past Query Runner runs."},
	"benchmark targets":   {"", "The servers a benchmark can run against."},
	"benchmark run":       {"<stack> <node>", "Run a workload and report throughput and latency."},
	"benchmark history":   {"", "Past benchmark runs."},

	"logs targets":     {"", "The nodes Log Summary can read."},
	"logs collect":     {"<stack>", "Collect nodes' logs into one bundle (every node with logs, or --nodes)."},
	"logs list":        {"", "The collected log bundles."},
	"logs get":         {"<bundle>", "A bundle's summary."},
	"logs events":      {"<bundle>", "A bundle's events as one timeline; --severity filters them."},
	"logs delete":      {"<bundle>", "Delete a log bundle."},
	"capture targets":  {"", "The nodes Packet Inspector can capture on."},
	"capture start":    {"<stack> <node>", "Capture a node's traffic and decode the database protocol."},
	"capture list":     {"", "Your captures."},
	"capture get":      {"<id>", "A capture's summary."},
	"capture stop":     {"<id>", "Stop a running capture."},
	"capture packets":  {"<id>", "A capture's decoded packets."},
	"capture download": {"<id>", "Save a capture as a .pcap file."},
	"stalk start":      {"<stack> <node>", "Run pt-summary, pt-mysql-summary and pt-stalk on a MySQL node."},
	"stalk status":     {"<stack> <node>", "Whether a pt-stalk capture is running, and what it has collected."},
	"stalk download":   {"<stack> <node>", "Save the node's current pt-stalk capture."},
	"stalk archives":   {"", "The kept pt-stalk archives."},
	"stalk analyse":    {"<archive>", "Analyse a kept pt-stalk archive."},
	"ftdc targets":     {"", "The MongoDB nodes FTDC Summary can read."},
	"ftdc node":        {"<stack> <node>", "Read a mongod's diagnostic data (FTDC)."},
}

// subUsageText is a group's subcommands with what each does.
func subUsageText(group string, order []string, table map[string]func([]string) error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: dbcanvas-cli %s <subcommand> [arguments]\n\nSubcommands:\n", group)
	w := 0
	for _, n := range order {
		if len(n) > w {
			w = len(n)
		}
	}
	for _, n := range order {
		if _, ok := table[n]; ok {
			fmt.Fprintf(&b, "  %-*s  %s\n", w, n, helpText[group+" "+n].summary)
		}
	}
	fmt.Fprintf(&b, "\n`dbcanvas-cli %s <subcommand> -h` shows a subcommand's arguments and flags.\n", group)
	return b.String()
}

// usageFunc is the -h text of one command: its usage line, what it does, and its flags.
func usageFunc(name string, fs *flag.FlagSet) func() {
	return func() {
		h := helpText[name]
		line := "dbcanvas-cli " + name
		var flags int
		fs.VisitAll(func(*flag.Flag) { flags++ })
		if flags > 0 {
			line += " [flags]"
		}
		if h.args != "" {
			line += " " + h.args
		}
		out := fs.Output()
		fmt.Fprintf(out, "Usage: %s\n", line)
		if h.summary != "" {
			fmt.Fprintf(out, "\n%s\n", h.summary)
		}
		if flags > 0 {
			fmt.Fprintln(out, "\nFlags:")
			fs.PrintDefaults()
		}
		fmt.Fprintln(out, "\nGlobal flags (--profile, --url, --token, --json) go before the command; `dbcanvas-cli -h` lists them.")
	}
}

// cmdHelpCmd is `dbcanvas-cli help [command [subcommand]]`.
func cmdHelpCmd(args []string) error {
	if len(args) == 0 {
		usage()
		return errHelp
	}
	for _, c := range commands {
		if c.name == args[0] {
			// Running the command with -h prints exactly what `<command> -h` prints, and runs
			// nothing: every command parses its flags before it does anything else.
			return c.run(append(append([]string{}, args[1:]...), "-h"))
		}
	}
	fmt.Fprintf(os.Stderr, "dbcanvas-cli: no command %q\n\n", args[0])
	usage()
	return errUsage
}
