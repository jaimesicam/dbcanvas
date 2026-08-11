package main

import "testing"

// These cover the edge walk and the engine mapping that decide what a Stock
// Market Sim node connects to. Both are pure, both are mirrored in
// StackDesigner.jsx, and a drift in either shows up as a node that validates on
// the canvas and then fails to deploy.

// ssDoc builds a design with one stocksim node linked to `other`.
func ssDoc(nodes []designNode, frames []designFrame, other string) designDoc {
	nodes = append(nodes, designNode{ID: "sim", Type: "stocksim", Label: "sim1"})
	return designDoc{
		Nodes:  nodes,
		Frames: frames,
		Edges:  []designEdge{{ID: "e1", From: edgeEnd{Node: other}, To: edgeEnd{Node: "sim"}}},
	}
}

func TestStockSimTargetStandaloneNodes(t *testing.T) {
	for _, typ := range []string{"ps", "mariadb", "mysqlce", "pg", "psm", "valkey"} {
		doc := ssDoc([]designNode{{ID: "db", Type: typ, Label: "db1"}}, nil, "db")
		kind, id, ok := stockSimTarget(doc, "sim")
		if !ok || kind != typ || id != "db" {
			t.Errorf("%s: got (%q, %q, %v), want (%q, \"db\", true)", typ, kind, id, ok, typ)
		}
	}
}

func TestStockSimTargetFrames(t *testing.T) {
	frames := []string{
		"pxc", "mysql", "innodb", "mariadbrepl", "mariadbgalera", "mysqlcerepl", "mysqlceinnodb",
		"patroni", "repmgr", "spock", "k3d", "psmrs", "psmdb", "valkeycluster",
	}
	for _, typ := range frames {
		doc := ssDoc(nil, []designFrame{{ID: "fr", Type: typ, Label: "cluster1"}}, "fr")
		kind, id, ok := stockSimTarget(doc, "sim")
		if !ok || kind != typ || id != "fr" {
			t.Errorf("%s: got (%q, %q, %v), want (%q, \"fr\", true)", typ, kind, id, ok, typ)
		}
	}
}

func TestStockSimTargetRejectsNonDatabases(t *testing.T) {
	// A node inside a frame is a cluster member, not something this app can
	// address on its own — the frame is what it links to.
	doc := ssDoc([]designNode{{ID: "db", Type: "ps", Label: "member", FrameID: "fr"}}, nil, "db")
	if kind, _, ok := stockSimTarget(doc, "sim"); ok {
		t.Errorf("a framed member should not be a target, got kind %q", kind)
	}
	// Types that are not databases at all.
	for _, typ := range []string{"pmm", "vnc", "intranet", "seaweedfs", "aio", "orchestrator"} {
		doc := ssDoc([]designNode{{ID: "db", Type: typ, Label: "x"}}, nil, "db")
		if kind, _, ok := stockSimTarget(doc, "sim"); ok {
			t.Errorf("%s should not be a link target, got kind %q", typ, kind)
		}
	}
	// No edge at all.
	if _, _, ok := stockSimTarget(designDoc{Nodes: []designNode{{ID: "sim", Type: "stocksim"}}}, "sim"); ok {
		t.Error("an unlinked node should resolve to no target")
	}
}

// The routers are matched, but their engine comes from what they front.
func TestStockSimTargetRouters(t *testing.T) {
	for _, typ := range []string{"haproxy", "proxysql"} {
		doc := ssDoc([]designNode{{ID: "r", Type: typ, Label: "router"}}, nil, "r")
		kind, id, ok := stockSimTarget(doc, "sim")
		if !ok || kind != typ || id != "r" {
			t.Errorf("%s: got (%q, %q, %v)", typ, kind, id, ok)
		}
	}
}

func TestStockSimEngineForKind(t *testing.T) {
	want := map[string]string{
		"ps": "mysql", "mariadb": "mysql", "mysqlce": "mysql",
		"pxc": "mysql", "mysql": "mysql", "innodb": "mysql",
		"mariadbrepl": "mysql", "mariadbgalera": "mysql",
		"mysqlcerepl": "mysql", "mysqlceinnodb": "mysql",
		"pg": "postgres", "patroni": "postgres", "repmgr": "postgres",
		"spock": "postgres", "k3d": "postgres",
		"psm": "mongodb", "psmrs": "mongodb", "psmdb": "mongodb",
		"valkey": "valkey", "valkeycluster": "valkey",
		// Routers resolve through stockSimEngineForTarget, not here.
		"haproxy": "", "proxysql": "", "nonsense": "",
	}
	for kind, engine := range want {
		if got := stockSimEngineForKind(kind); got != engine {
			t.Errorf("stockSimEngineForKind(%q) = %q, want %q", kind, got, engine)
		}
	}
	// Every kind stockSimTarget can return must map to an engine, or the node
	// deploys into a switch with no branch for it.
	for kind := range stockSimStandaloneTargets {
		if stockSimEngineForKind(kind) == "" {
			t.Errorf("standalone target %q has no engine", kind)
		}
	}
	for kind := range stockSimFrameTargets {
		if stockSimEngineForKind(kind) == "" {
			t.Errorf("frame target %q has no engine", kind)
		}
	}
}

// ProxySQL is MySQL-only; HAProxy takes its engine from the cluster it fronts,
// and reports none when it fronts no single cluster — which is a design error
// rather than a default worth guessing at.
func TestStockSimEngineForTargetRouters(t *testing.T) {
	pgCluster := designDoc{
		Nodes: []designNode{
			{ID: "hap", Type: "haproxy", Label: "hap1"},
			{ID: "m1", Type: "patroni", Label: "m1", FrameID: "fr"},
		},
		Frames: []designFrame{{ID: "fr", Type: "patroni", Label: "pgc"}},
		Edges:  []designEdge{{ID: "e", From: edgeEnd{Node: "fr"}, To: edgeEnd{Node: "hap"}}},
	}
	if got := stockSimEngineForTarget(pgCluster, "haproxy", "hap"); got != "postgres" {
		t.Errorf("HAProxy fronting Patroni = %q, want postgres", got)
	}

	bare := designDoc{Nodes: []designNode{{ID: "hap", Type: "haproxy", Label: "hap1"}}}
	if got := stockSimEngineForTarget(bare, "haproxy", "hap"); got != "" {
		t.Errorf("HAProxy fronting nothing = %q, want empty", got)
	}
	if got := stockSimEngineForTarget(bare, "proxysql", "px"); got != "mysql" {
		t.Errorf("ProxySQL = %q, want mysql", got)
	}
}

func TestStockSimMode(t *testing.T) {
	for in, want := range map[string]string{
		"": "linked", "linked": "linked", "manual": "manual", "aio": "aio", "nonsense": "linked",
	} {
		if got := stockSimMode(designNode{SSMode: in}); got != want {
			t.Errorf("stockSimMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// An All in One node draws no association lines, so its instance is named by
// the picker and resolved against the AIO node's own design.
func TestStockSimAIODeclared(t *testing.T) {
	doc := designDoc{Nodes: []designNode{
		{ID: "aio1", Type: "aio", Label: "box", AIOInstances: []aioInstance{
			{ID: "i1", Kind: "ps", Name: "ps01"},
			{ID: "i2", Kind: "patroni", Name: "pgc", Members: 3},
			{ID: "i3", Kind: "proxysql", Name: "px01"},
		}},
	}}
	sim := designNode{ID: "sim", Type: "stocksim", SSMode: "aio", SSAIONode: "aio1", SSAIOInstance: "pgc"}
	in, ok := stockSimAIODeclared(doc, sim)
	if !ok || in.Kind != "patroni" {
		t.Fatalf("got (%+v, %v), want the patroni instance", in, ok)
	}

	sim.SSAIOInstance = "missing"
	if _, ok := stockSimAIODeclared(doc, sim); ok {
		t.Error("an instance that is not declared should not resolve")
	}
	sim.SSAIONode, sim.SSAIOInstance = "nope", "ps01"
	if _, ok := stockSimAIODeclared(doc, sim); ok {
		t.Error("an instance on a node that does not exist should not resolve")
	}
}

// The engine — and so the size target and the validation — has to be knowable
// from the design alone, before anything is deployed, in all three modes.
func TestStockSimEngineAndIssues(t *testing.T) {
	cases := []struct {
		name       string
		doc        designDoc
		sim        designNode
		wantEngine string
		wantIssues int
	}{
		{
			name: "linked to a Galera cluster",
			doc: designDoc{
				Frames: []designFrame{{ID: "fr", Type: "mariadbgalera", Label: "g"}},
				Edges:  []designEdge{{From: edgeEnd{Node: "fr"}, To: edgeEnd{Node: "sim"}}},
			},
			sim: designNode{ID: "sim", Label: "sim1"}, wantEngine: "mysql",
		},
		{
			name:       "linked to nothing",
			doc:        designDoc{},
			sim:        designNode{ID: "sim", Label: "sim1"},
			wantEngine: "", wantIssues: 1,
		},
		{
			name: "an All in One MongoDB instance",
			doc: designDoc{Nodes: []designNode{{ID: "aio1", Type: "aio", Label: "box",
				AIOInstances: []aioInstance{{Kind: "psmdb", Name: "mongo01"}}}}},
			sim:        designNode{ID: "sim", Label: "sim1", SSMode: "aio", SSAIONode: "aio1", SSAIOInstance: "mongo01"},
			wantEngine: "mongodb",
		},
		{
			name: "an All in One instance this app cannot drive",
			doc: designDoc{Nodes: []designNode{{ID: "aio1", Type: "aio", Label: "box",
				AIOInstances: []aioInstance{{Kind: "proxysql", Name: "px01"}}}}},
			sim:        designNode{ID: "sim", Label: "sim1", SSMode: "aio", SSAIONode: "aio1", SSAIOInstance: "px01"},
			wantEngine: "", wantIssues: 1,
		},
		{
			name:       "All in One mode with nothing chosen",
			doc:        designDoc{},
			sim:        designNode{ID: "sim", Label: "sim1", SSMode: "aio"},
			wantEngine: "", wantIssues: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := c.doc
			doc.Nodes = append(doc.Nodes, c.sim)
			engine, issues := stockSimEngineAndIssues(doc, c.sim)
			if engine != c.wantEngine {
				t.Errorf("engine = %q, want %q", engine, c.wantEngine)
			}
			if len(issues) != c.wantIssues {
				t.Errorf("got %d issues, want %d: %+v", len(issues), c.wantIssues, issues)
			}
		})
	}
}

// A declared cluster instance names several deployed members; the write
// endpoint is the one to connect to.
func TestStockSimAIOMember(t *testing.T) {
	dep := aioDepWith([]aioInstanceRuntime{
		{Inst: "ps01", Kind: "ps", Ready: true, FQDN: "ps01.example.net"},
		{Inst: "pgc-n1", Group: "pgc", Kind: "patroni", Role: "replica", Ready: true},
		{Inst: "pgc-n2", Group: "pgc", Kind: "patroni", Role: "primary", Ready: true},
		{Inst: "px01", Kind: "proxysql", Ready: true},
	})

	if m, ok := stockSimAIOMember(dep, "ps01"); !ok || m.Inst != "ps01" {
		t.Errorf("standalone: got (%+v, %v)", m, ok)
	}
	if m, ok := stockSimAIOMember(dep, "pgc"); !ok || m.Inst != "pgc-n2" {
		t.Errorf("cluster: got %+v, want the primary pgc-n2", m)
	}
	// ProxySQL is not a targetable instance, so naming it resolves to nothing.
	if m, ok := stockSimAIOMember(dep, "px01"); ok {
		t.Errorf("proxysql should not resolve, got %+v", m)
	}
	if _, ok := stockSimAIOMember(dep, "nope"); ok {
		t.Error("an unknown instance should not resolve")
	}
}

// aioDepWith builds a Deployment whose config carries these runtime instances,
// which is where aioRuntimeInstances reads them from.
func aioDepWith(instances []aioInstanceRuntime) Deployment {
	return Deployment{State: DeployRunning, ContainerID: "c1",
		Config: mustJSON(aioConfig{Instances: instances})}
}
