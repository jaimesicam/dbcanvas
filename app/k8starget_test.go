package main

// k8starget_test.go — the classification of an operator's Services, pinned against
// what a real one actually creates.
//
// The fixture is the Service list of a live Percona PostgreSQL Operator cluster,
// captured from a running k3s rather than written by hand. That matters: every guess
// this file would otherwise encode — which Service is the primary, which is headless,
// which carries the port — is a guess about somebody else's operator, and the only way
// to be right about it is to look.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func loadSvcFixture(t *testing.T, name string) []k8sTargetSvc {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Skipf("fixture %s is not present: %v", name, err)
	}
	var list struct {
		Items []k8sTargetSvc `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return list.Items
}

func TestK8sClassifiesAPerconaPGClusterTheWayItIsBuilt(t *testing.T) {
	svcs := loadSvcFixture(t, "k8s-svc-percona-pg.json")
	cfg := k3dConfig{Operator: "pg", ClusterName: "k3d-01", Namespace: "pg"}

	got := map[string]k8sEndpoint{}
	for _, s := range svcs {
		if e, ok := k8sClassifyService(s, cfg); ok {
			got[s.Metadata.Name] = e
		}
	}

	// The two that are real endpoints.
	pgb, ok := got["k3d-01-pgbouncer"]
	if !ok {
		t.Fatalf("the pooler was not recognised; classified: %v", keysOf(got))
	}
	if pgb.Engine != dexPostgres || pgb.Port != 5432 {
		t.Errorf("pgbouncer: engine=%q port=%d", pgb.Engine, pgb.Port)
	}
	if !pgb.Preferred {
		t.Error("the pooler is the endpoint an application should use, so it sorts first")
	}
	if pgb.credKey() != "app" {
		// pgBouncer only knows the application roles the operator registered with
		// it; offering the superuser there produces "no such user" and looks like a
		// DBCanvas bug rather than the pooler doing its job.
		t.Errorf("the pooler must authenticate as the application user, got %q", pgb.credKey())
	}
	if pgb.TLS != "require" {
		t.Errorf("a PostgreSQL operator refuses plaintext, so TLS must be required: %q", pgb.TLS)
	}

	ha, ok := got["k3d-01-ha"]
	if !ok {
		t.Fatal("the primary Service was not recognised")
	}
	if ha.Role != "primary" || !ha.Preferred {
		t.Errorf("-ha is the primary: role=%q preferred=%v", ha.Role, ha.Preferred)
	}
	if ha.credKey() != "admin" {
		t.Errorf("a direct endpoint uses the administrative account, got %q", ha.credKey())
	}

	if rep, ok := got["k3d-01-replicas"]; !ok || rep.Role != "replica" {
		t.Errorf("-replicas should classify as a replica endpoint: %+v", rep)
	}

	// The ones that must NOT become endpoints.
	for _, name := range []string{
		"k3d-01-pods",    // headless, no ports — pod DNS only
		"k3d-01-primary", // headless; its Endpoints are managed by Patroni
		"k3d-01-ha-config",
	} {
		if e, ok := got[name]; ok {
			t.Errorf("%s is not a database endpoint but was classified as %+v", name, e)
		}
	}

	// Longest-suffix matching is what keeps "-ha-config" from reading as "-ha".
	if _, ok := got["k3d-01-ha-config"]; ok {
		t.Error("-ha-config matched the -ha tier")
	}
}

func keysOf(m map[string]k8sEndpoint) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestK8sIgnoresServicesThatBelongToSomethingElse(t *testing.T) {
	cfg := k3dConfig{Operator: "pg", ClusterName: "mycluster"}
	mk := func(name string, typ string, port int, portName string) k8sTargetSvc {
		var s k8sTargetSvc
		s.Metadata.Name = name
		s.Spec.Type = typ
		s.Spec.ClusterIP = "10.43.0.5"
		s.Spec.Ports = append(s.Spec.Ports, struct {
			Name     string `json:"name"`
			Port     int    `json:"port"`
			NodePort int    `json:"nodePort"`
			Protocol string `json:"protocol"`
		}{Name: portName, Port: port})
		return s
	}
	// Another workload's Service in the same namespace.
	if _, ok := k8sClassifyService(mk("kubernetes", "ClusterIP", 443, "https"), cfg); ok {
		t.Error("the Kubernetes API Service was classified as a database")
	}
	if _, ok := k8sClassifyService(mk("percona-postgresql-operator", "ClusterIP", 443, ""), cfg); ok {
		t.Error("the operator's own Service was classified as a database")
	}
	// A Service of this cluster but on a port nothing speaks a database on.
	if _, ok := k8sClassifyService(mk("mycluster-ha", "ClusterIP", 8008, "patroni"), cfg); ok {
		t.Error("Patroni's REST port is not a database endpoint")
	}
	// And the one that should work.
	if e, ok := k8sClassifyService(mk("mycluster-ha", "ClusterIP", 5432, "postgres"), cfg); !ok || e.Engine != dexPostgres {
		t.Errorf("a real endpoint was refused: %+v %v", e, ok)
	}
}

func TestK8sPortEngineCoversTheThreeFamilies(t *testing.T) {
	cases := []struct {
		name string
		port int
		want string
	}{
		{"postgres", 5432, dexPostgres},
		{"pgbouncer", 5432, dexPostgres},
		{"", 6432, dexPostgres},
		{"mysql", 3306, dexMySQL},
		{"", 3306, dexMySQL},
		{"", 6033, dexMySQL}, // ProxySQL
		{"", 6446, dexMySQL}, // MySQL Router read/write
		{"mongodb", 27017, dexMongoDB},
		{"", 27017, dexMongoDB},
		{"patroni", 8008, ""},
		{"https", 443, ""},
		{"metrics", 9187, ""},
	}
	for _, c := range cases {
		if got := k8sPortEngine(c.name, c.port); got != c.want {
			t.Errorf("k8sPortEngine(%q, %d) = %q, want %q", c.name, c.port, got, c.want)
		}
	}
}

func TestK8sAddressResolutionExplainsItself(t *testing.T) {
	mk := func(typ, lbIP string, port, nodePort int) k8sTargetSvc {
		var s k8sTargetSvc
		s.Spec.Type = typ
		s.Spec.Ports = append(s.Spec.Ports, struct {
			Name     string `json:"name"`
			Port     int    `json:"port"`
			NodePort int    `json:"nodePort"`
			Protocol string `json:"protocol"`
		}{Name: "postgres", Port: port, NodePort: nodePort})
		if lbIP != "" {
			s.Status.LoadBalancer.Ingress = append(s.Status.LoadBalancer.Ingress, struct {
				IP       string `json:"ip"`
				Hostname string `json:"hostname"`
			}{IP: lbIP})
		}
		return s
	}
	// A LoadBalancer with a MetalLB address is dialable at the Service port.
	addr, port, why := k8sAddressOf(mk("LoadBalancer", "172.20.255.238", 5432, 32215), 5432, "172.20.0.9")
	if addr != "172.20.255.238" || port != 5432 || why != "" {
		t.Errorf("LoadBalancer: %s:%d (%s)", addr, port, why)
	}
	// A NodePort is dialable at any k3s node's address, on the node port.
	addr, port, why = k8sAddressOf(mk("NodePort", "", 5432, 32215), 5432, "172.20.0.9")
	if addr != "172.20.0.9" || port != 32215 || why != "" {
		t.Errorf("NodePort: %s:%d (%s)", addr, port, why)
	}
	// ClusterIP is not an address outside the cluster, and the reason is the useful
	// half — "nothing to connect to" is the wrong thing to tell somebody whose
	// Service is simply the operator default.
	addr, _, why = k8sAddressOf(mk("ClusterIP", "", 5432, 0), 5432, "172.20.0.9")
	if addr != "" {
		t.Error("a ClusterIP Service must not report an address")
	}
	if why == "" {
		t.Error("the absence of an address has to be explained")
	}
	// A LoadBalancer MetalLB has not served yet is not silently a ClusterIP.
	addr, _, why = k8sAddressOf(mk("LoadBalancer", "", 5432, 0), 5432, "172.20.0.9")
	if addr != "" || why == "" {
		t.Errorf("a pending LoadBalancer: %q / %q", addr, why)
	}
}

func TestK8sPodSelectionSkipsHeadlessAndPicksTheClientContainer(t *testing.T) {
	mkPod := func(name, phase string, labels map[string]string, containers ...string) k8sTargetPod {
		var p k8sTargetPod
		p.Metadata.Name = name
		p.Metadata.Labels = labels
		p.Status.Phase = phase
		for _, c := range containers {
			p.Spec.Containers = append(p.Spec.Containers, struct {
				Name string `json:"name"`
			}{Name: c})
		}
		return p
	}
	sel := map[string]string{"cluster": "k3d-01", "role": "pgbouncer"}
	pods := []k8sTargetPod{
		mkPod("other", "Running", map[string]string{"cluster": "k3d-01"}, "database"),
		mkPod("stopped", "Succeeded", sel, "pgbouncer"),
		mkPod("k3d-01-pgbouncer-x", "Running", sel, "pgbouncer-config", "pgbouncer"),
	}
	var svc k8sTargetSvc
	svc.Spec.Selector = sel
	pod, container := k8sPodFor(pods, svc, dexPostgres)
	if pod != "k3d-01-pgbouncer-x" {
		t.Errorf("pod = %q, want the running one whose labels match", pod)
	}
	// An operator pod is several containers and the client is in one of them; picking
	// the first would land on a config sidecar.
	if container != "pgbouncer" {
		t.Errorf("container = %q, want the one with the client", container)
	}

	// A Service with no selector (the operators' headless primary, whose Endpoints
	// Patroni manages) matches nothing, and must not be paired with an unrelated pod.
	var headless k8sTargetSvc
	if pod, _ := k8sPodFor(pods, headless, dexPostgres); pod != "" {
		t.Errorf("a selectorless Service matched pod %q", pod)
	}
}

func TestK8sExecArgvWrapsTheClientForKubectl(t *testing.T) {
	e := k8sEndpoint{Namespace: "pg", Pod: "k3d-01-instance1-qwhc-0", Container: "database"}
	got := e.k8sExecArgv([]string{"psql", "-U", "postgres", "-f", "-"})
	want := []string{"kubectl", "-n", "pg", "exec", "k3d-01-instance1-qwhc-0", "-c", "database", "-i", "--", "psql", "-U", "postgres", "-f", "-"}
	if len(got) != len(want) {
		t.Fatalf("argv = %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %q, want %q", got, want)
		}
	}
	// A pod with one container needs no -c, and adding an empty one would break it.
	single := k8sEndpoint{Namespace: "pg", Pod: "p"}
	for _, a := range single.k8sExecArgv([]string{"psql"}) {
		if a == "-c" {
			t.Error("no container was named, so -c must not be passed")
		}
	}
}

func TestK8sEndpointIDIsStableAndParseable(t *testing.T) {
	e := k8sEndpoint{FrameID: "frame-mu2awv5r-1", Service: "k3d-01-pgbouncer"}
	if e.ID() != "frame-mu2awv5r-1/k3d-01-pgbouncer" {
		t.Errorf("id = %q", e.ID())
	}
	// The Explorer carries the two halves separately in its own connection id, so
	// the Service name must survive that encoding.
	ref := dexRef{StackID: 3, Shape: dexShapeK8s, Target: e.FrameID, Extra: e.Service}
	back, err := dexParseID(ref.String())
	if err != nil {
		t.Fatalf("a Kubernetes connection id must parse: %v", err)
	}
	if back.Target != e.FrameID || back.Extra != e.Service {
		t.Errorf("round trip lost the endpoint: %+v", back)
	}
}

// ---------------------------------------------------------------- exposing

func TestK8sExposeManifestMirrorsWithoutTouching(t *testing.T) {
	e := k8sEndpoint{
		Namespace: "pg", Service: "k3d-01-replicas", Engine: dexPostgres,
		Port: 5432, TargetPort: 5432,
		Selector: map[string]string{
			"postgres-operator.crunchydata.com/cluster": "k3d-01",
			"postgres-operator.crunchydata.com/role":    "replica",
		},
	}
	m := k8sExposeManifest(e.Service+k8sExposeSuffix, e, "LoadBalancer")

	for _, want := range []string{
		"name: k3d-01-replicas-dbcanvas", // beside the operator's, never over it
		"namespace: pg",
		"type: LoadBalancer",
		k8sExposeLabel + `: "true"`, // what makes removal safe
		k8sExposeSourceAnnotation + ": k3d-01-replicas",
		"port: 5432",
		"targetPort: 5432",
		"postgres-operator.crunchydata.com/role",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("the manifest is missing %q:\n%s", want, m)
		}
	}
	// The name must not be the operator's own — that is the whole design.
	if strings.Contains(m, "name: k3d-01-replicas\n") {
		t.Error("the companion Service must not be named the same as the operator's")
	}
	// A Kubernetes key has at most one slash, the prefix. Building the annotation by
	// appending a path to the label produced an invalid key and the API rejected it.
	if strings.Count(k8sExposeSourceAnnotation, "/") > 1 {
		t.Errorf("%q is not a valid Kubernetes key", k8sExposeSourceAnnotation)
	}
}

func TestK8sExposeRefusesAServiceItCannotMirror(t *testing.T) {
	// The PostgreSQL operators publish the primary as a Service with no selector, so
	// that a failover moves it. There is nothing to copy, and guessing a selector
	// would pin the mirror to whichever pod is primary right now — which is exactly
	// the thing the operator's arrangement exists to avoid.
	app := newTestApp(t)
	e := k8sEndpoint{Service: "k3d-01-ha", Namespace: "pg"}
	_, err := app.k8sExposeEndpoint(context.Background(), e)
	if err == nil {
		t.Fatal("a selectorless Service must not be mirrored")
	}
	if !strings.Contains(err.Error(), "pooler") {
		t.Errorf("the refusal should say what to do instead: %v", err)
	}
}

func TestK8sExposedServiceIsNotItsOwnEndpoint(t *testing.T) {
	// A companion Service is the same database as the one it mirrors. Listing it as
	// well would put one endpoint in the tree twice, with two different addresses.
	var s k8sTargetSvc
	s.Metadata.Name = "k3d-01-replicas" + k8sExposeSuffix
	s.Spec.Type = "LoadBalancer"
	s.Spec.ClusterIP = "10.43.1.1"
	s.Spec.Ports = append(s.Spec.Ports, struct {
		Name     string `json:"name"`
		Port     int    `json:"port"`
		NodePort int    `json:"nodePort"`
		Protocol string `json:"protocol"`
	}{Name: "postgres", Port: 5432})
	if _, ok := k8sClassifyService(s, k3dConfig{Operator: "pg", ClusterName: "k3d-01"}); ok {
		t.Error("the Service DBCanvas created was classified as an endpoint of its own")
	}
}

// ---------------------------------------------------------------- tool wiring

func TestK8sTargetIDsAreDistinctFromNodeIDs(t *testing.T) {
	// The load tools take a node id from a dropdown. A Kubernetes endpoint is not a
	// node, so its id has to be recognisable as something else — and a canvas node id
	// must never be mistaken for one.
	id := k8sTargetID(k8sEndpoint{FrameID: "frame-abc", Service: "cl-pgbouncer"})
	if got, ok := k8sSplitTarget(id); !ok || got != "frame-abc/cl-pgbouncer" {
		t.Fatalf("split(%q) = %q, %v", id, got, ok)
	}
	for _, nodeID := range []string{"pg-mu285xp2-1", "aio1#ps01", "frame-abc", ""} {
		if _, ok := k8sSplitTarget(nodeID); ok {
			t.Errorf("%q was read as a Kubernetes target", nodeID)
		}
	}
}

func TestK8sLoadToolsOnlyOfferWhatTheyCanDial(t *testing.T) {
	// The Query Runner and the Benchmark open many connections and time them, so an
	// endpoint reachable only by `kubectl exec` is no use to them — a process per
	// statement would measure the process. listK8sSQLTargets filters on Reachable(),
	// and the refusal in k8sResolveTarget says what to do about it.
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	// A stack with no Kubernetes frames contributes nothing rather than failing.
	if got := app.listK8sSQLTargets(context.Background(), u); len(got) != 0 {
		t.Errorf("a user with no clusters should have no Kubernetes targets, got %v", got)
	}
	// The resolver refuses an id that is not one of ours at all.
	if _, err := app.k8sResolveTarget(context.Background(), u, 1, "pg-node-1"); err == nil {
		t.Error("a plain node id must not resolve as a Kubernetes target")
	}
}

func TestK8sTargetsStayOutOfTheSharedSQLTargetList(t *testing.T) {
	// listSQLTargets is also the Packet Inspector's, and the Packet Inspector runs
	// tcpdump inside a node's container. A Kubernetes Service has no such container,
	// so an endpoint in that list would offer a capture that could never start.
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	seedStack(t, app, u, "lab")
	for _, tgt := range app.listSQLTargets(u) {
		if _, ok := k8sSplitTarget(tgt.NodeID); ok {
			t.Errorf("a Kubernetes endpoint reached listSQLTargets: %s", tgt.NodeID)
		}
	}
}

func TestK8sDataGenOffersOnlyTheEndpointThatTakesWrites(t *testing.T) {
	// The generator writes. A replica or a pooler would fail in a way that reads like
	// a DBCanvas fault rather than like pointing an INSERT at a read-only endpoint.
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	st := seedStack(t, app, u, "lab")
	if got := app.k8sDataGenConnections(context.Background(), st); len(got) != 0 {
		t.Errorf("a stack with no clusters should contribute nothing, got %v", got)
	}
}
