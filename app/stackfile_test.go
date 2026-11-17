package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

// Every built-in template, exported and read back, is the same design.
func TestStackFileRoundTripsEveryTemplate(t *testing.T) {
	for _, tpl := range builtinTemplateDefs {
		st := Stack{Name: tpl.Name, TTL: "8h", Design: tpl.Design, Labels: map[string]string{"team": "example"}}
		b, err := encodeStackFile(st)
		if err != nil {
			t.Fatalf("%s: encode: %v", tpl.Name, err)
		}
		sf, err := parseStackFile(b)
		if err != nil {
			t.Fatalf("%s: parse: %v\n%s", tpl.Name, err, b)
		}
		if sf.Name != tpl.Name || sf.TTL == nil || *sf.TTL != "8h" || sf.Labels["team"] != "example" {
			t.Errorf("%s: header came back as %q %v %v", tpl.Name, sf.Name, sf.TTL, sf.Labels)
		}
		if !sameJSON(t, tpl.Design, sf.Design) {
			t.Errorf("%s: the design changed on the way through:\n%s", tpl.Name, b)
		}
	}
}

// Strings that would read as something else stay strings; key order is kept.
func TestStackFileKeepsTypesAndOrder(t *testing.T) {
	design := `{"nodes":[{"id":"n1","type":"ps","label":"ps-01","version":"8.0","osVersion":"9","port":3306,"on":"yes","empty":"","note":"two\nlines"}],"frames":[],"edges":[]}`
	b, err := encodeStackFile(Stack{Name: "lab", TTL: "infinity", Design: json.RawMessage(design)})
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.Contains(out, `"8.0"`) || !strings.Contains(out, `"9"`) || !strings.Contains(out, "port: 3306") {
		t.Errorf("types not kept:\n%s", out)
	}
	if strings.Index(out, "id: n1") > strings.Index(out, "type: ps") {
		t.Errorf("key order not kept:\n%s", out)
	}
	sf, err := parseStackFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, []byte(design), sf.Design) {
		t.Errorf("round trip:\n%s\n%s", out, sf.Design)
	}
}

// A 0.0.x `dbcanvas-cli stack export` file is read as it is.
func TestStackFileReadsTheOldJSONExport(t *testing.T) {
	old := `{"name":"pxc-lab","ttl":"4h","design":{"nodes":[{"id":"n1","type":"intranet","label":"intranet-01"}],"edges":[]}}`
	sf, err := parseStackFile([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if sf.Name != "pxc-lab" || *sf.TTL != "4h" || sf.LabelsSet || !strings.Contains(string(sf.Design), "intranet-01") {
		t.Errorf("got %+v", sf)
	}
}

func TestStackFileRefusals(t *testing.T) {
	cases := map[string]string{
		"apiVersion: dbcanvas/v9\nkind: Stack\nname: a\nnodes: [ps]": "apiVersion",
		"kind: Pod\nname: a\nnodes: [ps]":                            "kind Stack",
		"name: a":                                                    "needs a design",
		"name: a\nnodes: [ps]\ndesign: {nodes: []}":                  "not both",
		"name: a\nnodes: [ps]\nreplicas: 3":                          `unknown key "replicas"`,
		"nodes: [ps]":                                                "needs a name",
		"name: a\nnodes: ['ps,colour=blue']":                         `unknown option "colour"`,
		"name: a\ndesign:\n  nodes:\n    - {id: n1, type: pxc, label: x, osVersion: 9.0}": "quote it",
		"name: a\nname: b\nnodes: [ps]": "given twice",
	}
	for in, want := range cases {
		if _, err := parseStackFile([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want an error with %q", in, err, want)
		}
	}
}

// Compose entries, short and long, become compose specs.
func TestStackFileNodesEntries(t *testing.T) {
	sf, err := parseStackFile([]byte(`
apiVersion: dbcanvas/v1
kind: Stack
name: pxc-lab
nodes:
  - pxc:3,version=8.4,os=el9,monitor
  - {kind: pmm, version: "3"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Nodes) != 2 || sf.Nodes[0].Kind != "pxc" || sf.Nodes[0].Count != 3 || sf.Nodes[0].Version != "8.4" ||
		sf.Nodes[1].Kind != "pmm" || sf.Nodes[1].Version != "3" {
		t.Errorf("got %+v", sf.Nodes)
	}
}

// Applying creates the stack; applying an edited export updates it, and the plan lists
// exactly what changed — not a node that only moved.
func TestApplyStackFileCreatesThenUpdates(t *testing.T) {
	app := newTestApp(t)
	app.docker = NewDocker(filepath.Join(t.TempDir(), "no-docker.sock")) // validation asks it; nothing answers
	u, _ := app.store.CreateUser("owner", "x", RoleUser, StatusApproved)
	ctx := context.Background()
	design := `{"nodes":[` +
		`{"id":"i1","type":"intranet","label":"intranet-01","x":0,"y":0},` +
		`{"id":"p1","type":"ps","label":"ps-01","x":10,"y":10,"cpus":1},` +
		`{"id":"p2","type":"ps","label":"ps-02","x":20,"y":20}],"frames":[],"edges":[]}`
	file, _ := encodeStackFile(Stack{Name: "lab", TTL: "4h", Design: json.RawMessage(design)})
	sf, err := parseStackFile(file)
	if err != nil {
		t.Fatal(err)
	}
	p, err := app.applyStackFile(ctx, u, sf)
	if err != nil {
		t.Fatal(err)
	}
	if p.Action != "create" || len(p.Added) != 3 || p.Stack == nil || p.Stack.TTL != "4h" {
		t.Fatalf("create: %+v", p)
	}

	// Same file again: nothing to do.
	if p, err = app.applyStackFile(ctx, u, sf); err != nil || p.Action != "unchanged" {
		t.Fatalf("re-apply: %v %+v", err, p)
	}

	// Move ps-01, change ps-02, drop intranet-01's twin… and add ps-03.
	edited := `{"nodes":[` +
		`{"id":"i1","type":"intranet","label":"intranet-01","x":0,"y":0},` +
		`{"id":"p1","type":"ps","label":"ps-01","x":99,"y":99,"cpus":1},` +
		`{"id":"p3","type":"ps","label":"ps-03","x":30,"y":30}],"frames":[],"edges":[]}`
	file, _ = encodeStackFile(Stack{Name: "lab", TTL: "4h", Design: json.RawMessage(edited), Labels: map[string]string{"run": "2"}})
	sf, _ = parseStackFile(file)
	dry, _, err := app.planStackFile(ctx, u.ID, sf)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Action != "update" || len(dry.Added) != 1 || dry.Added[0].ID != "p3" ||
		len(dry.Removed) != 1 || dry.Removed[0].ID != "p2" || len(dry.Changed) != 0 || !dry.LabelsChange {
		t.Fatalf("plan: %+v", dry)
	}
	if st, _ := app.store.GetStack(dry.StackID); strings.Contains(string(st.Design), "ps-03") {
		t.Fatal("a dry run saved the design")
	}
	p, err = app.applyStackFile(ctx, u, sf)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := app.store.GetStack(p.StackID)
	if !strings.Contains(string(st.Design), "ps-03") || strings.Contains(string(st.Design), "ps-02") || st.Labels["run"] != "2" {
		t.Fatalf("after apply: %s %v", st.Design, st.Labels)
	}

	// Another owner's stack of the same name is theirs: this one creates its own.
	other, _ := app.store.CreateUser("other", "x", RoleUser, StatusApproved)
	if p, err := app.applyStackFile(ctx, other, sf); err != nil || p.Action != "create" {
		t.Fatalf("other owner: %v %+v", err, p)
	}
}
