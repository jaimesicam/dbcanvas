package main

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Every command in the table is one the dispatcher runs, and every command the dispatcher runs
// is in the table (aliases and the service's own hidden command aside), so `dbcanvas commands`
// and the help list exactly what the binary does.
func TestCommandTableMatchesTheDispatcher(t *testing.T) {
	src, err := os.ReadFile("cmd_dbcanvas.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src[strings.Index(string(src), "func runCLI("):])
	body = body[:strings.Index(body, "\n}\n")]
	cases := map[string]bool{}
	for _, m := range regexp.MustCompile(`case ([^:]+):`).FindAllStringSubmatch(body, -1) {
		for _, q := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(m[1], -1) {
			cases[q[1]] = true
		}
	}
	aliases := map[string]bool{"": true, "list": true, "password": true, "tokens": true, "serve": true,
		"--version": true, "-v": true, "--help": true, "-h": true}
	inTable := map[string]bool{}
	for _, c := range cliCommands {
		word := strings.Fields(c.Name)[0]
		inTable[word] = true
		if !cases[word] {
			t.Errorf("%q is in the command table but runCLI has no case for it", c.Name)
		}
	}
	for c := range cases {
		if !inTable[c] && !aliases[c] && c != serviceArg {
			t.Errorf("runCLI runs %q but the command table does not list it", c)
		}
	}
}

func TestUsageListsEveryCommandAndFormats(t *testing.T) {
	out := fmt.Sprintf(cliUsage, "/home/example/.dbcanvas")
	if strings.Contains(out, "%!") || !strings.Contains(out, "Data folder: /home/example/.dbcanvas") {
		t.Fatalf("the usage does not format: %s", out)
	}
	flat := strings.Join(strings.Fields(out), " ")
	for _, c := range cliCommands {
		if !strings.Contains(flat, usageLine(c)) {
			t.Errorf("the help does not show %q", usageLine(c))
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > 100 {
			t.Errorf("help line too long (%d): %s", len(line), line)
		}
	}
}

func TestEveryFlagParsesAndReads(t *testing.T) {
	for _, c := range cliCommands {
		fs := cmdFlags(c.Name)
		for _, f := range c.Flags {
			if fs.Lookup(f.Name) == nil {
				t.Errorf("%s: --%s not registered", c.Name, f.Name)
			}
			switch f.Kind {
			case "bool":
				flagBool(fs, f.Name)
			case "string":
				flagString(fs, f.Name)
			case "int":
				flagInt(fs, f.Name)
			case "duration":
				flagDuration(fs, f.Name)
			case "list":
				flagList(fs, f.Name)
			}
		}
	}
	fs := cmdFlags("up")
	if _, err := parseInterspersed(fs, []string{"lab", "--node", "pxc:3", "--node", "vnc", "--label", "a=b", "--wait"}); err != nil {
		t.Fatal(err)
	}
	if got := flagList(fs, "node"); !reflect.DeepEqual(got, []string{"pxc:3", "vnc"}) || !flagBool(fs, "wait") {
		t.Fatalf("nodes %v", got)
	}
}

func TestComposeStackFileIsAStackFile(t *testing.T) {
	raw, err := composeStackFile("lab", []string{"pxc:3,version=8.4", "vnc"}, "8h", []string{"run=12", "team = qa"})
	if err != nil {
		t.Fatal(err)
	}
	sf, err := parseStackFile(raw)
	if err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if sf.Name != "lab" || sf.TTL == nil || *sf.TTL != "8h" || len(sf.Nodes) != 2 ||
		!reflect.DeepEqual(sf.Labels, map[string]string{"run": "12", "team": "qa"}) {
		t.Fatalf("parsed %+v from %s", sf, raw)
	}
	if _, err := composeStackFile("lab", []string{"ps"}, "", []string{"nokey"}); err == nil {
		t.Error("a label without = was accepted")
	}
	if raw, _ := composeStackFile("lab", []string{"ps"}, "", nil); strings.Contains(string(raw), "ttl") || strings.Contains(string(raw), "labels") {
		t.Errorf("no --ttl or --label must leave the stack's own: %s", raw)
	}
}

func TestExecArguments(t *testing.T) {
	for _, c := range []struct {
		args     []string
		own, cmd []string
		dashed   bool
	}{
		{[]string{"lab", "ps-01", "--", "mysql", "-e", "select 1"}, []string{"lab", "ps-01"}, []string{"mysql", "-e", "select 1"}, true},
		{[]string{"--user", "mysql", "lab", "ps-01", "--", "id", "--", "x"}, []string{"--user", "mysql", "lab", "ps-01"}, []string{"id", "--", "x"}, true},
		{[]string{"lab", "ps-01", "hostname"}, []string{"lab", "ps-01", "hostname"}, nil, false},
	} {
		own, cmd, dashed := splitExecArgs(c.args)
		if !reflect.DeepEqual(own, c.own) || !reflect.DeepEqual(cmd, c.cmd) || dashed != c.dashed {
			t.Errorf("%v: %v %v %v", c.args, own, cmd, dashed)
		}
	}
}

func TestNodePath(t *testing.T) {
	for in, want := range map[string][2]string{
		"ps-01:/etc/my.cnf": {"ps-01", "/etc/my.cnf"},
		"./my.cnf":          {"", ""},
		"C:/x":              {"C", "/x"},
		"ps-01:relative":    {"", ""},
		"dir/ps:/x":         {"", ""},
	} {
		n, p, _ := nodePath(in)
		if n != want[0] || p != want[1] {
			t.Errorf("%s: %q %q", in, n, p)
		}
	}
}
