package main

import (
	"strings"
	"testing"
)

// These cover the edge walk and the engine mapping that decide what a
// simulator node connects to. Both are pure, both are mirrored in
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

func TestSimLinkedTargetStandaloneNodes(t *testing.T) {
	for _, typ := range []string{"ps", "mariadb", "mysqlce", "pg", "psm", "valkey"} {
		doc := ssDoc([]designNode{{ID: "db", Type: typ, Label: "db1"}}, nil, "db")
		kind, id, ok := simLinkedTarget(doc, "sim")
		if !ok || kind != typ || id != "db" {
			t.Errorf("%s: got (%q, %q, %v), want (%q, \"db\", true)", typ, kind, id, ok, typ)
		}
	}
}

func TestSimLinkedTargetFrames(t *testing.T) {
	frames := []string{
		"pxc", "mysql", "innodb", "mariadbrepl", "mariadbgalera", "mysqlcerepl", "mysqlceinnodb",
		"patroni", "repmgr", "spock", "k3d", "psmrs", "psmdb", "valkeycluster",
	}
	for _, typ := range frames {
		doc := ssDoc(nil, []designFrame{{ID: "fr", Type: typ, Label: "cluster1"}}, "fr")
		kind, id, ok := simLinkedTarget(doc, "sim")
		if !ok || kind != typ || id != "fr" {
			t.Errorf("%s: got (%q, %q, %v), want (%q, \"fr\", true)", typ, kind, id, ok, typ)
		}
	}
}

func TestSimLinkedTargetRejectsNonDatabases(t *testing.T) {
	// A node inside a frame is a cluster member, not something this app can
	// address on its own — the frame is what it links to.
	doc := ssDoc([]designNode{{ID: "db", Type: "ps", Label: "member", FrameID: "fr"}}, nil, "db")
	if kind, _, ok := simLinkedTarget(doc, "sim"); ok {
		t.Errorf("a framed member should not be a target, got kind %q", kind)
	}
	// Types that are not databases at all.
	for _, typ := range []string{"pmm", "vnc", "intranet", "seaweedfs", "aio", "orchestrator"} {
		doc := ssDoc([]designNode{{ID: "db", Type: typ, Label: "x"}}, nil, "db")
		if kind, _, ok := simLinkedTarget(doc, "sim"); ok {
			t.Errorf("%s should not be a link target, got kind %q", typ, kind)
		}
	}
	// No edge at all.
	if _, _, ok := simLinkedTarget(designDoc{Nodes: []designNode{{ID: "sim", Type: "stocksim"}}}, "sim"); ok {
		t.Error("an unlinked node should resolve to no target")
	}
}

// The routers are matched, but their engine comes from what they front.
func TestSimLinkedTargetRouters(t *testing.T) {
	for _, typ := range []string{"haproxy", "proxysql"} {
		doc := ssDoc([]designNode{{ID: "r", Type: typ, Label: "router"}}, nil, "r")
		kind, id, ok := simLinkedTarget(doc, "sim")
		if !ok || kind != typ || id != "r" {
			t.Errorf("%s: got (%q, %q, %v)", typ, kind, id, ok)
		}
	}
}

func TestSimEngineForKind(t *testing.T) {
	want := map[string]string{
		"ps": "mysql", "mariadb": "mysql", "mysqlce": "mysql",
		"pxc": "mysql", "mysql": "mysql", "innodb": "mysql",
		"mariadbrepl": "mysql", "mariadbgalera": "mysql",
		"mysqlcerepl": "mysql", "mysqlceinnodb": "mysql",
		"pg": "postgres", "patroni": "postgres", "repmgr": "postgres",
		"spock": "postgres",
		"psm":   "mongodb", "psmrs": "mongodb", "psmdb": "mongodb",
		"valkey": "valkey", "valkeycluster": "valkey",
		// Routers and Kubernetes frames resolve through
		// simEngineForTarget, not here.
		"haproxy": "", "proxysql": "", "k3d": "", "nonsense": "",
	}
	for kind, engine := range want {
		if got := simEngineForKind(kind); got != engine {
			t.Errorf("simEngineForKind(%q) = %q, want %q", kind, got, engine)
		}
	}
	// Every kind simLinkedTarget can return must map to an engine, or the node
	// deploys into a switch with no branch for it.
	for kind := range simStandaloneTargets {
		if simEngineForKind(kind) == "" {
			t.Errorf("standalone target %q has no engine", kind)
		}
	}
	for kind := range simFrameTargets {
		if kind == "k3d" {
			continue // resolved from the frame's operator; see the test below
		}
		if simEngineForKind(kind) == "" {
			t.Errorf("frame target %q has no engine", kind)
		}
	}
}

// A K3D frame's engine is whichever of the six operators it runs, so it can
// only be resolved with the design in hand. All six must map to an engine the
// sim implements — a frame that deploys into a switch with no branch for it is
// the bug this guards.
func TestSimEngineForTargetK3D(t *testing.T) {
	want := map[string]string{
		"pxc": "mysql", "ps": "mysql",
		"psmdb": "mongodb",
		"pg":    "postgres", "cnpg": "postgres", "pgo": "postgres",
		// A frame with no operator has no database in it to drive, and neither does
		// OpenEverest's: its databases are created from its own UI.
		"": "", "nonsense": "", "everest": "",
	}
	for op, engine := range want {
		doc := designDoc{Frames: []designFrame{{ID: "fr", Type: "k3d", Label: "k8s", K3DOperator: op}}}
		if got := simEngineForTarget(doc, "k3d", "fr"); got != engine {
			t.Errorf("K3D frame running %q = %q, want %q", op, got, engine)
		}
		if engine != "" && !stockSimEngineImplemented(engine) {
			t.Errorf("K3D frame running %q resolves to %q, which the sim image does not implement", op, engine)
		}
	}
	// Every operator k3d.go can actually install must be one of them.
	for op, deployable := range k3dDeployableOperator {
		if deployable && !k3dNoDatabaseOperator[op] && k3dOperatorEngine(op) == "" {
			t.Errorf("operator %q is deployable but a Stock Market Sim node cannot use it", op)
		}
	}
}

// ProxySQL is MySQL-only; HAProxy takes its engine from the cluster it fronts,
// and reports none when it fronts no single cluster — which is a design error
// rather than a default worth guessing at.
func TestSimEngineForTargetRouters(t *testing.T) {
	pgCluster := designDoc{
		Nodes: []designNode{
			{ID: "hap", Type: "haproxy", Label: "hap1"},
			{ID: "m1", Type: "patroni", Label: "m1", FrameID: "fr"},
		},
		Frames: []designFrame{{ID: "fr", Type: "patroni", Label: "pgc"}},
		Edges:  []designEdge{{ID: "e", From: edgeEnd{Node: "fr"}, To: edgeEnd{Node: "hap"}}},
	}
	if got := simEngineForTarget(pgCluster, "haproxy", "hap"); got != "postgres" {
		t.Errorf("HAProxy fronting Patroni = %q, want postgres", got)
	}

	bare := designDoc{Nodes: []designNode{{ID: "hap", Type: "haproxy", Label: "hap1"}}}
	if got := simEngineForTarget(bare, "haproxy", "hap"); got != "" {
		t.Errorf("HAProxy fronting nothing = %q, want empty", got)
	}
	if got := simEngineForTarget(bare, "proxysql", "px"); got != "mysql" {
		t.Errorf("ProxySQL = %q, want mysql", got)
	}
}

// An All in One node draws no association lines, so its instance is named by
// the picker and resolved against the AIO node's own design.
func TestSimAIODeclared(t *testing.T) {
	doc := designDoc{Nodes: []designNode{
		{ID: "aio1", Type: "aio", Label: "box", AIOInstances: []aioInstance{
			{ID: "i1", Kind: "ps", Name: "ps01"},
			{ID: "i2", Kind: "patroni", Name: "pgc", Members: 3},
			{ID: "i3", Kind: "proxysql", Name: "px01"},
		}},
	}}
	sim := designNode{ID: "sim", Type: "stocksim", SSMode: "aio", SSAIONode: "aio1", SSAIOInstance: "pgc"}
	in, ok := simAIODeclared(doc, sim.SSAIONode, sim.SSAIOInstance)
	if !ok || in.Kind != "patroni" {
		t.Fatalf("got (%+v, %v), want the patroni instance", in, ok)
	}

	sim.SSAIOInstance = "missing"
	if _, ok := simAIODeclared(doc, sim.SSAIONode, sim.SSAIOInstance); ok {
		t.Error("an instance that is not declared should not resolve")
	}
	sim.SSAIONode, sim.SSAIOInstance = "nope", "ps01"
	if _, ok := simAIODeclared(doc, sim.SSAIONode, sim.SSAIOInstance); ok {
		t.Error("an instance on a node that does not exist should not resolve")
	}
}

// A declared cluster instance names several deployed members; the write
// endpoint is the one to connect to.
func TestSimAIOMember(t *testing.T) {
	dep := aioDepWith([]aioInstanceRuntime{
		{Inst: "ps01", Kind: "ps", Ready: true, FQDN: "ps01.example.net"},
		{Inst: "pgc-n1", Group: "pgc", Kind: "patroni", Role: "replica", Ready: true},
		{Inst: "pgc-n2", Group: "pgc", Kind: "patroni", Role: "primary", Ready: true},
		{Inst: "px01", Kind: "proxysql", Ready: true},
	})

	if m, ok := simAIOMember(dep, "ps01"); !ok || m.Inst != "ps01" {
		t.Errorf("standalone: got (%+v, %v)", m, ok)
	}
	if m, ok := simAIOMember(dep, "pgc"); !ok || m.Inst != "pgc-n2" {
		t.Errorf("cluster: got %+v, want the primary pgc-n2", m)
	}
	// ProxySQL is not a targetable instance, so naming it resolves to nothing.
	if m, ok := simAIOMember(dep, "px01"); ok {
		t.Errorf("proxysql should not resolve, got %+v", m)
	}
	if _, ok := simAIOMember(dep, "nope"); ok {
		t.Error("an unknown instance should not resolve")
	}
}

// aioDepWith builds a Deployment whose config carries these runtime instances,
// which is where aioRuntimeInstances reads them from.
func aioDepWith(instances []aioInstanceRuntime) Deployment {
	return Deployment{State: DeployRunning, ContainerID: "c1",
		Config: mustJSON(aioConfig{Instances: instances})}
}

// The read split is the only thing in DBCanvas that puts read load on a cluster's replicas,
// and it exists for exactly one target shape: an HAProxy node, whose :5001 means "a replica"
// where :5000 means "the primary". These check the environment the sim is handed, which is
// the whole contract between the two halves of the feature.
func TestSimSQLEnvSplitsReadsOnlyWhenAsked(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		single := simSQLEnv(engine, "app", "pw", "hap-01.example.net", haproxyWritePort, 0, "")
		if len(single) != 2 {
			t.Errorf("%s: no split should be two variables, got %v", engine, single)
		}
		for _, v := range single {
			if strings.Contains(v, "_RO_DSN=") {
				t.Errorf("%s: a target with no read endpoint was given one: %q", engine, v)
			}
		}

		split := simSQLEnv(engine, "app", "pw", "hap-01.example.net", haproxyWritePort, haproxyReadPort, "")
		if len(split) != 3 {
			t.Fatalf("%s: a split target should be three variables, got %v", engine, split)
		}
		var write, read string
		for _, v := range split {
			switch {
			case strings.Contains(v, "_RO_DSN="):
				read = v
			case strings.Contains(v, "_DSN="):
				write = v
			}
		}
		if !strings.Contains(write, "5000") || strings.Contains(write, "5001") {
			t.Errorf("%s: writes must go to the write port: %q", engine, write)
		}
		if !strings.Contains(read, "5001") {
			t.Errorf("%s: reads must go to the read port: %q", engine, read)
		}
		// Same host and same credentials on both — a read endpoint elsewhere, or one
		// authenticating differently, would be a different database as far as the sim
		// is concerned.
		if !strings.Contains(read, "hap-01.example.net") || !strings.Contains(read, "app") {
			t.Errorf("%s: the read DSN lost the host or the credentials: %q", engine, read)
		}
	}
}

// The engine decides the variable names, because that is what the sim reads (main.go's
// configFromEnv). Getting this wrong is a sim that silently ignores the read endpoint.
func TestSimSQLEnvUsesTheEngineVariableNames(t *testing.T) {
	my := strings.Join(simSQLEnv("mysql", "u", "p", "h", 5000, 5001, ""), " ")
	for _, want := range []string{"DB_ENGINE=mysql", "MYSQL_DSN=", "MYSQL_RO_DSN="} {
		if !strings.Contains(my, want) {
			t.Errorf("mysql env is missing %q: %s", want, my)
		}
	}
	pg := strings.Join(simSQLEnv("postgres", "u", "p", "h", 5000, 5001, ""), " ")
	for _, want := range []string{"DB_ENGINE=postgres", "POSTGRES_DSN=", "POSTGRES_RO_DSN="} {
		if !strings.Contains(pg, want) {
			t.Errorf("postgres env is missing %q: %s", want, pg)
		}
	}
}

// A clustered PostgreSQL target's primary moves — by failover, or because somebody ran the
// switchover this lab exists to rehearse — and the DSN has to survive it. Naming only the
// member that was primary at deploy leaves the sim on a read-only standby afterwards: reads
// keep working, so nothing looks broken, while every write fails with SQLSTATE 25006.
func TestSimPostgresDSNNamesEveryMember(t *testing.T) {
	members := []string{"repmgr-1.example.net", "repmgr-2.example.net", "repmgr-3.example.net"}
	env := simSQLEnv("postgres", "postgres", "pw", members[0], 5432, 0, "", members...)

	var dsn string
	for _, e := range env {
		if strings.HasPrefix(e, "POSTGRES_DSN=") {
			dsn = strings.TrimPrefix(e, "POSTGRES_DSN=")
		}
	}
	if dsn == "" {
		t.Fatalf("no POSTGRES_DSN in %v", env)
	}
	for _, h := range members {
		if !strings.Contains(dsn, h+":5432") {
			t.Errorf("DSN omits %s: %s", h, dsn)
		}
	}
	// The setting that makes the host list mean "find the writer" rather than "try these".
	if !strings.Contains(dsn, "target_session_attrs=read-write") {
		t.Errorf("a multi-host DSN must ask for the read-write member: %s", dsn)
	}
	// Commas must survive verbatim. url.URL percent-escapes them in an authority, which libpq
	// then reads as one absurd hostname — the bug this is built by hand to avoid.
	if strings.Contains(dsn, "%2C") || !strings.Contains(dsn, ",") {
		t.Errorf("the host list must stay comma-separated and unescaped: %s", dsn)
	}
	if !strings.HasPrefix(dsn, "postgres://") || !strings.Contains(dsn, "/postgres?") {
		t.Errorf("DSN is malformed: %s", dsn)
	}

	// A standalone target has nowhere to fail over to, and asking for read-write there would
	// only add a round trip — or refuse a perfectly good single server.
	single := simSQLEnv("postgres", "postgres", "pw", "pg-01.example.net", 5432, 0, "")
	for _, e := range single {
		if strings.HasPrefix(e, "POSTGRES_DSN=") {
			if strings.Contains(e, "target_session_attrs") || strings.Contains(e, ",") {
				t.Errorf("a single-host DSN must stay single-host: %s", e)
			}
		}
	}

	// MySQL is untouched: its DSN dialect has no equivalent, and its clustered targets front
	// themselves with a proxy instead.
	my := simSQLEnv("mysql", "root", "pw", "ps-01.example.net", 3306, 0, "", members...)
	for _, e := range my {
		if strings.HasPrefix(e, "MYSQL_DSN=") && strings.Contains(e, ",") {
			t.Errorf("the MySQL DSN must not grow a host list: %s", e)
		}
	}
}
