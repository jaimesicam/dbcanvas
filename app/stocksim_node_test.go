package main

import (
	"testing"
)

// What only the Stock Market Sim node decides: its connection mode, the
// engine and issues it reports, and the warning for a Kubernetes target
// exposed only inside the cluster.

func TestStockSimMode(t *testing.T) {
	for in, want := range map[string]string{
		"": "linked", "linked": "linked", "manual": "manual", "aio": "aio", "nonsense": "linked",
	} {
		if got := stockSimMode(designNode{SSMode: in}); got != want {
			t.Errorf("stockSimMode(%q) = %q, want %q", in, got, want)
		}
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

// The whole point of the design-time check is to say "change this setting"
// before a twenty-minute deploy says it, so it has to fire on exactly the
// frames whose database has no address outside Kubernetes — and stay quiet the
// moment any tier the sim could use has one.
func TestStockSimK3DExposeIssues(t *testing.T) {
	sim := designNode{ID: "sim", Type: "stocksim", Label: "sim1"}

	cases := []struct {
		name  string
		frame designFrame
		warn  bool
	}{
		{"PXC behind a ClusterIP HAProxy", designFrame{K3DOperator: "pxc",
			K3DExposeHAProxy: "clusterip", K3DExposePXC: "clusterip"}, true},
		{"PXC with a LoadBalancer HAProxy", designFrame{K3DOperator: "pxc",
			K3DExposeHAProxy: "loadbalancer", K3DExposePXC: "clusterip"}, false},
		{"PXC with NodePort pods and no proxy address", designFrame{K3DOperator: "pxc",
			K3DExposeHAProxy: "clusterip", K3DExposePXC: "nodeport"}, false},
		// The proxy that counts is the one the frame actually runs: exposing
		// ProxySQL does nothing for a cluster fronted by HAProxy.
		{"PXC on HAProxy with only ProxySQL exposed", designFrame{K3DOperator: "pxc",
			K3DProxy: "haproxy", K3DExposeHAProxy: "clusterip", K3DExposeProxySQL: "loadbalancer",
			K3DExposePXC: "clusterip"}, true},
		{"PXC on ProxySQL with ProxySQL exposed", designFrame{K3DOperator: "pxc",
			K3DProxy: "proxysql", K3DExposeHAProxy: "clusterip", K3DExposeProxySQL: "loadbalancer",
			K3DExposePXC: "clusterip"}, false},
		{"PS group replication behind a ClusterIP router", designFrame{K3DOperator: "ps",
			K3DProxy: "router", K3DExposeRouter: "clusterip", K3DExposeMySQL: "clusterip"}, true},
		{"PS with an exposed primary", designFrame{K3DOperator: "ps",
			K3DProxy: "router", K3DExposeRouter: "clusterip", K3DExposeMySQL: "loadbalancer"}, false},
		// A replica set has no router: exposing mongos is meaningless when
		// sharding is off, and the members are what the driver has to reach.
		{"PSMDB replica set with only mongos exposed", designFrame{K3DOperator: "psmdb",
			K3DSharding: false, K3DExposeReplset: "clusterip", K3DExposeMongos: "loadbalancer"}, true},
		{"PSMDB replica set exposed", designFrame{K3DOperator: "psmdb",
			K3DExposeReplset: "loadbalancer"}, false},
		{"PSMDB sharded with mongos exposed", designFrame{K3DOperator: "psmdb",
			K3DSharding: true, K3DExposeReplset: "clusterip", K3DExposeMongos: "loadbalancer"}, false},
		{"Percona PG in-cluster only", designFrame{K3DOperator: "pg",
			K3DExposePG: "clusterip", K3DExposePGBouncer: "clusterip"}, true},
		// Either tier is a usable endpoint: the pooler is the cluster's front
		// door, the primary Service is the fallback behind it.
		{"Percona PG through pgBouncer", designFrame{K3DOperator: "pg",
			K3DExposePG: "clusterip", K3DExposePGBouncer: "loadbalancer"}, false},
		{"Percona PG with an exposed primary", designFrame{K3DOperator: "pg",
			K3DExposePG: "loadbalancer", K3DExposePGBouncer: "clusterip"}, false},
		{"Crunchy PGO in-cluster only", designFrame{K3DOperator: "pgo",
			K3DExposePG: "clusterip", K3DExposePGBouncer: "clusterip"}, true},
		{"Crunchy PGO with an exposed primary", designFrame{K3DOperator: "pgo",
			K3DExposePG: "nodeport", K3DExposePGBouncer: "clusterip"}, false},
		{"CNPG on ClusterIP", designFrame{K3DOperator: "cnpg", K3DCNPGExpose: "clusterip"}, true},
		{"CNPG on a LoadBalancer", designFrame{K3DOperator: "cnpg", K3DCNPGExpose: "loadbalancer"}, false},
		// No operator is a different complaint, raised by the engine check.
		{"no operator", designFrame{K3DOperator: ""}, false},
	}
	for _, c := range cases {
		got := stockSimK3DExposeIssues(ssK3DDoc(c.frame), sim)
		if c.warn && len(got) == 0 {
			t.Errorf("%s: expected a warning, got none", c.name)
		}
		if !c.warn && len(got) != 0 {
			t.Errorf("%s: expected no warning, got %q", c.name, got[0].Message)
		}
		for _, i := range got {
			if i.Level != "warning" {
				t.Errorf("%s: level = %q, want warning", c.name, i.Level)
			}
		}
	}

	// The legacy single-value expose field still counts: a design saved before
	// the per-section split says LoadBalancer once and means it everywhere.
	if got := stockSimK3DExposeIssues(ssK3DDoc(designFrame{K3DOperator: "pxc", K3DExpose: "loadbalancer"}), sim); len(got) != 0 {
		t.Errorf("legacy expose=loadbalancer should be reachable, got %q", got[0].Message)
	}

	// A node that is not linked to Kubernetes at all has nothing to say.
	manual := designNode{ID: "sim", Type: "stocksim", Label: "sim1", SSMode: "manual"}
	if got := stockSimK3DExposeIssues(ssK3DDoc(designFrame{K3DOperator: "pxc"}), manual); len(got) != 0 {
		t.Errorf("a manual node should raise nothing, got %q", got[0].Message)
	}
}
