package main

import (
	"context"
	"strings"
	"testing"
)

// svc builds a Service the way `kubectl get svc -o json` would report one.
func svc(name, typ string, lbIP string, ports ...[3]any) kubeService {
	var s kubeService
	s.Metadata.Name = name
	s.Spec.Type = typ
	for _, p := range ports {
		s.Spec.Ports = append(s.Spec.Ports, struct {
			Name     string `json:"name"`
			Port     int    `json:"port"`
			NodePort int    `json:"nodePort"`
		}{Name: p[0].(string), Port: p[1].(int), NodePort: p[2].(int)})
	}
	if lbIP != "" {
		s.Status.LoadBalancer.Ingress = append(s.Status.LoadBalancer.Ingress, struct {
			IP       string `json:"ip"`
			Hostname string `json:"hostname"`
		}{IP: lbIP})
	}
	return s
}

// The port a Service is dialled on has to come from its *name* first. The PS
// operator's MySQL Router answers read/write on 6446 and also publishes 3306
// (rw-default), so picking "the one that looks like MySQL" lands on the wrong
// port — this is the case that makes the preference list worth having.
func TestServicePortPrefersNames(t *testing.T) {
	router := svc("ps-cluster1-router", "LoadBalancer", "10.0.0.5",
		[3]any{"http", 8443, 0}, [3]any{"rw-default", 3306, 0},
		[3]any{"read-write", 6446, 0}, [3]any{"rw", 6446, 0}, [3]any{"ro", 6447, 0})
	if p, _, ok := servicePort(router, pxcMySQLPort, "rw", "mysql"); !ok || p != 6446 {
		t.Errorf("router port = %d (ok=%v), want 6446", p, ok)
	}

	haproxy := svc("cluster1-haproxy", "LoadBalancer", "10.0.0.6",
		[3]any{"mysql", 3306, 0}, [3]any{"mysql-replicas", 3307, 0})
	if p, _, ok := servicePort(haproxy, pxcMySQLPort, "rw", "mysql"); !ok || p != 3306 {
		t.Errorf("haproxy port = %d (ok=%v), want 3306", p, ok)
	}

	// No name matches: the default number wins over the first port.
	odd := svc("odd", "NodePort", "", [3]any{"metrics", 9104, 30001}, [3]any{"", 3306, 30002})
	p, np, ok := servicePort(odd, pxcMySQLPort, "rw", "mysql")
	if !ok || p != 3306 || np != 30002 {
		t.Errorf("odd = (%d, %d, %v), want (3306, 30002, true)", p, np, ok)
	}

	// Neither: the first port, so a renamed port still yields something dialable.
	only := svc("only", "LoadBalancer", "10.0.0.7", [3]any{"weird", 15432, 0})
	if p, _, ok := servicePort(only, patroniPGPort, "postgres"); !ok || p != 15432 {
		t.Errorf("only = %d (ok=%v), want 15432", p, ok)
	}

	if _, _, ok := servicePort(svc("none", "ClusterIP", ""), 3306); ok {
		t.Error("a Service with no ports should not resolve one")
	}
}

// The per-pod Services of a PSMDB replica set are the seed list, and the driver
// is handed them in member order.
func TestServicesWithPrefix(t *testing.T) {
	all := []kubeService{
		svc("mdb-rs0-1", "LoadBalancer", "10.0.0.2"),
		svc("mdb-mongos", "ClusterIP", ""),
		svc("mdb-rs0-0", "LoadBalancer", "10.0.0.1"),
		svc("mdb-cfg", "ClusterIP", ""),
		svc("mdb-rs0-2", "LoadBalancer", "10.0.0.3"),
	}
	got := servicesWithPrefix(all, "mdb-rs0-")
	var names []string
	for _, s := range got {
		names = append(names, s.Metadata.Name)
	}
	want := "mdb-rs0-0,mdb-rs0-1,mdb-rs0-2"
	if strings.Join(names, ",") != want {
		t.Errorf("members = %v, want %s", names, want)
	}
	if len(servicesWithPrefix(all, "nothing-")) != 0 {
		t.Error("an unmatched prefix should select nothing")
	}
}

// Waiting can fix a LoadBalancer whose address has not landed. It can never fix
// a ClusterIP — that is a setting on the frame, and a user should be told now
// rather than at the deploy timeout. Keeping the two apart is also what stops a
// still-starting cluster from being reported as reachable: the first live run
// of this resolver collapsed "not yet" into "ok" and dialled `:0`.
func TestResolveServiceRetryability(t *testing.T) {
	var a *App // neither branch below touches the receiver

	pending := svc("cluster1-haproxy", "LoadBalancer", "", [3]any{"mysql", 3306, 0})
	ep, retry, err := a.resolveService(context.Background(), "", pending, pxcMySQLPort, "mysql")
	if err == nil || !retry {
		t.Errorf("a LoadBalancer with no address = (%+v, retry=%v, err=%v), want a retryable failure", ep, retry, err)
	}

	clusterIP := svc("cluster1-haproxy", "ClusterIP", "", [3]any{"mysql", 3306, 0})
	ep, retry, err = a.resolveService(context.Background(), "", clusterIP, pxcMySQLPort, "mysql")
	if err == nil || retry {
		t.Errorf("a ClusterIP = (%+v, retry=%v, err=%v), want a permanent failure", ep, retry, err)
	}

	ready := svc("cluster1-haproxy", "LoadBalancer", "172.26.255.246", [3]any{"mysql", 3306, 32305}, [3]any{"mysqlx", 33060, 0})
	ep, retry, err = a.resolveService(context.Background(), "", ready, pxcMySQLPort, "mysql")
	if err != nil || retry || ep.addr() != "172.26.255.246:3306" {
		t.Errorf("a ready LoadBalancer = (%+v, retry=%v, err=%v), want 172.26.255.246:3306", ep, retry, err)
	}
}

// The DSN each family builds is what the sim image parses, so the shape of it
// is part of the contract with the store.
func TestK3DResolvedDSNs(t *testing.T) {
	frame := designFrame{ID: "fr", Type: "k3d", Label: "k8s-01", K3DOperator: "pxc"}
	ep := k3dEndpoint{host: "10.90.0.11", port: 3306, svc: "cluster1-haproxy", typ: "LoadBalancer"}

	my := k3dMySQLResolved(frame, k3dConfig{Operator: "pxc", ClusterName: "cluster1"}, ep, "root", "s3cret")
	if my.engine != "mysql" || my.kind != "k3d-pxc" {
		t.Errorf("engine/kind = %q/%q, want mysql/k3d-pxc", my.engine, my.kind)
	}
	if want := "MYSQL_DSN=root:s3cret@tcp(10.90.0.11:3306)/?tls=false"; my.env[1] != want {
		t.Errorf("DSN = %q, want %q", my.env[1], want)
	}
	if my.displayName != "k8s-01 / cluster1 (cluster1-haproxy)" {
		t.Errorf("displayName = %q", my.displayName)
	}

	pgFrame := designFrame{ID: "fr", Type: "k3d", Label: "k8s-02", K3DOperator: "pgo"}
	pgEP := k3dEndpoint{host: "10.90.0.12", port: 5432, svc: "cluster1-pgbouncer", typ: "LoadBalancer"}
	pg := k3dPostgresResolved(pgFrame, k3dConfig{Operator: "pgo", ClusterName: "cluster1"}, pgEP, "cluster1", "p@ss word", "cluster1")
	if pg.engine != "postgres" || pg.kind != "k3d-pgo" {
		t.Errorf("engine/kind = %q/%q, want postgres/k3d-pgo", pg.engine, pg.kind)
	}
	// The password has characters a naive concatenation would break the URL on.
	dsn := strings.TrimPrefix(pg.env[1], "POSTGRES_DSN=")
	for _, want := range []string{"postgres://cluster1:", "@10.90.0.12:5432/cluster1", "sslmode=prefer"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSN %q is missing %q", dsn, want)
		}
	}
	if strings.Contains(dsn, "p@ss word") {
		t.Errorf("DSN %q carries an unescaped password", dsn)
	}
}

// Every one of the six operators has a label, because the error a user reads
// when their cluster is unreachable names the operator.
func TestK3DOperatorLabelsCoverEveryEngine(t *testing.T) {
	for op := range k3dDeployableOperator {
		if k3dOperatorEngine(op) == "" {
			continue
		}
		if l := k3dOperatorLabel(op); l == "" || l == op {
			t.Errorf("operator %q has no readable label (got %q)", op, l)
		}
	}
}

// ssK3DDoc is a Stock Market Sim node linked to one Kubernetes frame.
func ssK3DDoc(f designFrame) designDoc {
	f.ID, f.Type = "fr", "k3d"
	if f.Label == "" {
		f.Label = "k8s-01"
	}
	return designDoc{
		Nodes:  []designNode{{ID: "sim", Type: "stocksim", Label: "sim1"}},
		Frames: []designFrame{f},
		Edges:  []designEdge{{ID: "e", From: edgeEnd{Node: "fr"}, To: edgeEnd{Node: "sim"}}},
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

// Every operator's front end has to be checked, or a sim gets linked to a cluster that has no
// address reachable from the stack network and only says so twenty minutes later, at deploy.
// One tier being routable is enough — that is the tier the resolver will use.
func TestK3DExposedTiersCoversEveryOperator(t *testing.T) {
	for _, op := range []string{"pxc", "ps", "psmdb", "pg", "pgo", "cnpg"} {
		if got := k3dExposedTiers(designFrame{K3DOperator: op}); len(got) == 0 {
			t.Errorf("operator %q reports no exposable tier — its front end would never be validated", op)
		}
	}
	// A frame with no operator has nothing to say; stockSimEngineAndIssues covers that case.
	if got := k3dExposedTiers(designFrame{}); len(got) != 0 {
		t.Errorf("a frame with no operator should report no tiers, got %v", got)
	}
}

// CloudNativePG's PgBouncer pool is a front end in its own right, so exposing only the pool must
// satisfy the check — and the pool must not be listed at all when it was never enabled.
func TestK3DExposedTiersCNPGPooler(t *testing.T) {
	off := k3dExposedTiers(designFrame{K3DOperator: "cnpg"})
	if _, ok := off["the PgBouncer pool"]; ok {
		t.Error("the pool is listed on a frame that has no pool")
	}
	on := k3dExposedTiers(designFrame{K3DOperator: "cnpg", K3DCNPGPooler: true, K3DCNPGPoolerExpose: "loadbalancer"})
	if on["the PgBouncer pool"] != "LoadBalancer" {
		t.Errorf("pool tier = %q, want LoadBalancer", on["the PgBouncer pool"])
	}
	if on["the CloudNativePG primary"] != "ClusterIP" {
		t.Errorf("primary tier = %q, want ClusterIP — the two are independent", on["the CloudNativePG primary"])
	}
	// An exposed pool in front of an in-cluster primary is a reachable cluster, so no warning.
	doc := designDoc{
		Nodes: []designNode{{ID: "ss", Type: "stocksim", Label: "stocksim-01", SSMode: "linked"}},
		Frames: []designFrame{{ID: "f1", Type: "k3d", Label: "k3d-00", K3DOperator: "cnpg",
			K3DCNPGPooler: true, K3DCNPGPoolerExpose: "loadbalancer"}},
		Edges: []designEdge{{ID: "e1", From: edgeEnd{Node: "f1"}, To: edgeEnd{Node: "ss"}}},
	}
	if got := stockSimK3DExposeIssues(doc, doc.Nodes[0]); len(got) != 0 {
		t.Errorf("an exposed PgBouncer pool should satisfy the check, got %q", got[0].Message)
	}
}
