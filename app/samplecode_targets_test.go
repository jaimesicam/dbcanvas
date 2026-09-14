package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// samplecode_targets_test.go — the endpoints are derived from the canvas, not from a list.
//
// This is the half of the feature that would rot silently: a new cluster type, a new proxy, a
// renamed config field, and the picker quietly stops offering something that is sitting right
// there on the canvas. So the test builds a stack with one of nearly everything, deploys it in the
// store, and asserts on what comes back.

// scTestStack writes a design and its deployments into a throwaway store and returns the stack.
func scTestStack(t *testing.T, app *App, doc designDoc, deps []Deployment) Stack {
	t.Helper()
	u, err := app.store.CreateUser("owner", "x", RoleAdmin, StatusApproved)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	design, _ := json.Marshal(doc)
	st, err := app.store.CreateStack("lab", u.ID, ttlInfinity, nil, design)
	if err != nil {
		t.Fatalf("create stack: %v", err)
	}
	for _, d := range deps {
		d.StackID = st.ID
		if d.State == "" {
			d.State = DeployRunning
		}
		if d.ContainerID == "" {
			d.ContainerID = "c-" + d.NodeID
		}
		if err := app.store.UpsertDeployment(d); err != nil {
			t.Fatalf("deploy %s: %v", d.NodeID, err)
		}
	}
	got, err := app.store.GetStack(st.ID)
	if err != nil {
		t.Fatalf("re-read stack: %v", err)
	}
	return got
}

func scJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// scTargetByID is the endpoint with that id, or a failure naming what was on offer instead.
func scTargetByID(t *testing.T, targets []scTarget, id string) scTarget {
	t.Helper()
	var have []string
	for _, tg := range targets {
		if tg.ID == id {
			return tg
		}
		have = append(have, tg.ID)
	}
	t.Fatalf("no endpoint %q; the stack offered %s", id, strings.Join(have, ", "))
	return scTarget{}
}

// A stack with a standalone Percona Server, a PXC cluster behind HAProxy, a Patroni cluster, a
// MongoDB replica set and a Valkey cluster — and every endpoint shape those imply.
func TestSampleCodeTargetsFromTopology(t *testing.T) {
	app := newTestApp(t)
	doc := designDoc{
		Nodes: []designNode{
			{ID: "intra", Type: "intranet", Label: "intranet"},
			{ID: "ps1", Type: "ps", Label: "ps-01"},
			{ID: "pxc1", Type: "pxc", Label: "pxc-01", FrameID: "fpxc"},
			{ID: "pxc2", Type: "pxc", Label: "pxc-02", FrameID: "fpxc"},
			{ID: "hap", Type: "haproxy", Label: "haproxy-01"},
			{ID: "pg1", Type: "patroni", Label: "patroni-01", FrameID: "fpat"},
			{ID: "mongo1", Type: "psmrs", Label: "psmrs-01", FrameID: "frs"},
			{ID: "mongo2", Type: "psmrs", Label: "psmrs-02", FrameID: "frs"},
			{ID: "vk1", Type: "valkeycluster", Label: "valkey-01", FrameID: "fvk"},
			{ID: "vk2", Type: "valkeycluster", Label: "valkey-02", FrameID: "fvk"},
			{ID: "vk3", Type: "valkeycluster", Label: "valkey-03", FrameID: "fvk"},
			{ID: "lc", Type: "linuxclient", Label: "linuxclient1"},
		},
		Frames: []designFrame{
			{ID: "fpxc", Type: "pxc", Label: "PXC Cluster"},
			{ID: "fpat", Type: "patroni", Label: "Patroni Cluster"},
			{ID: "frs", Type: "psmrs", Label: "Replica Set"},
			{ID: "fvk", Type: "valkeycluster", Label: "Valkey Cluster"},
		},
		// The HAProxy is associated with the PXC frame, which is how haproxyBackend
		// resolves what it fronts.
		Edges: []designEdge{{ID: "e1", From: edgeEnd{Node: "hap"}, To: edgeEnd{Node: "fpxc"}}},
	}
	mysqlSec := scJSON(t, pxcSecrets{AppUser: "app", AppPassword: "app_pw", AdminUser: "admin", AdminPassword: "admin_pw"})
	pgSec := scJSON(t, pgSecrets{SuperUser: "postgres", SuperPassword: "pg_pw"})
	mongoSec := scJSON(t, mongoSecrets{AdminUser: "admin", AdminPassword: "mongo_pw"})
	vkSec := scJSON(t, valkeySecrets{Password: "valkey_pw"})
	st := scTestStack(t, app, doc, []Deployment{
		{NodeID: "intra"},
		{NodeID: "ps1", Config: scJSON(t, pxcConfig{FQDN: "ps-01.example.net", GenerateCert: true}), Secrets: mysqlSec},
		{NodeID: "pxc1", Config: scJSON(t, pxcConfig{FQDN: "pxc-01.example.net", PXCVersion: "8.0.36-28.1"}), Secrets: mysqlSec},
		{NodeID: "pxc2", Config: scJSON(t, pxcConfig{FQDN: "pxc-02.example.net"}), Secrets: mysqlSec},
		{NodeID: "hap"},
		{NodeID: "pg1", Config: scJSON(t, pgConfig{FQDN: "patroni-01.example.net", PGMajor: "17", GenerateCert: true}), Secrets: pgSec},
		{NodeID: "mongo1", Config: scJSON(t, mongoConfig{FQDN: "psmrs-01.example.net", ReplSet: "rs0", PSMDBMajor: "8.0"}), Secrets: mongoSec},
		{NodeID: "mongo2", Config: scJSON(t, mongoConfig{FQDN: "psmrs-02.example.net", ReplSet: "rs0"}), Secrets: mongoSec},
		{NodeID: "vk1", Secrets: vkSec},
		{NodeID: "vk2", Secrets: vkSec},
		{NodeID: "vk3", Secrets: vkSec},
		{NodeID: "lc", Config: scJSON(t, linuxClientConfig{OS: "oraclelinux", FQDN: "linuxclient1.example.net"})},
	})

	targets := app.scStackTargets(st, "oraclelinux")

	// The standalone node: its own FQDN, the application account, and — because it was
	// deployed with a generated certificate — a verified TLS posture.
	ps := scTargetByID(t, targets, "ps1")
	if ps.Engine != scMySQL || ps.Host != "ps-01.example.net" || ps.Port != pxcMySQLPort {
		t.Errorf("standalone Percona Server resolved to %+v", ps)
	}
	if ps.User != "app" || ps.Password != "app_pw" {
		t.Errorf("expected the application account, got %q/%q", ps.User, ps.Password)
	}
	if ps.TLS.Mode != scTLSVerify || ps.TLS.CA != scCAPath("oraclelinux") {
		t.Errorf("a certificate-generating node should verify: %+v", ps.TLS)
	}

	// A cluster member is offered individually, named for the cluster it is in, and carries
	// the series the deployment recorded (so the native client comes from the right repo).
	m := scTargetByID(t, targets, "pxc1")
	if !strings.Contains(m.Label, "PXC Cluster") || !strings.Contains(m.Label, "pxc-01") {
		t.Errorf("a cluster member should be named for its cluster: %q", m.Label)
	}
	if m.Major != "8.0" {
		t.Errorf("major from pxcVersion 8.0.36-28.1 was %q", m.Major)
	}

	// HAProxy is two endpoints, not one, and they are not interchangeable.
	w := scTargetByID(t, targets, "hap@write")
	r := scTargetByID(t, targets, "hap@read")
	if w.Port != haproxyWritePort || r.Port != haproxyReadPort {
		t.Errorf("HAProxy ports resolved to %d and %d", w.Port, r.Port)
	}
	if w.Engine != scMySQL || r.Role != "replica" {
		t.Errorf("HAProxy endpoints: %+v / %+v", w, r)
	}
	if w.User != "app" {
		t.Errorf("HAProxy should carry the backend's credentials, got %q", w.User)
	}
	if !strings.Contains(r.Note, "writes will be refused") {
		t.Errorf("the read port should say what it is: %q", r.Note)
	}

	// The replica set is an endpoint of its own, with every member and the set name — which
	// is what a MongoDB driver needs to follow a failover.
	rs := scTargetByID(t, targets, "frs#frame")
	if rs.ReplicaSet != "rs0" || len(rs.Hosts) != 2 {
		t.Errorf("replica set endpoint: %+v", rs)
	}
	if rs.User != "admin" || rs.Password != "mongo_pw" {
		t.Errorf("replica set credentials: %q/%q", rs.User, rs.Password)
	}

	// So is the Valkey cluster.
	vk := scTargetByID(t, targets, "fvk#frame")
	if len(vk.Hosts) != 3 || vk.Password != "valkey_pw" || vk.Engine != scValkey {
		t.Errorf("valkey cluster endpoint: %+v", vk)
	}

	// PostgreSQL uses the superuser, because a DBCanvas node provisions no other role.
	pg := scTargetByID(t, targets, "pg1")
	if pg.User != "postgres" || pg.Password != "pg_pw" || pg.Port != patroniPGPort {
		t.Errorf("patroni member: %+v", pg)
	}
	if pg.Major != "17" {
		t.Errorf("PostgreSQL major was %q", pg.Major)
	}

	// The Intranet and the Linux Client are not database endpoints.
	for _, tg := range targets {
		if tg.NodeID == "intra" || tg.ID == "lc" {
			t.Errorf("%s should not be offered as a database endpoint", tg.ID)
		}
	}

	// A password never leaves in the list the picker reads.
	dto := scJSON(t, ps.dto())
	if strings.Contains(string(dto), "app_pw") {
		t.Error("the target list carries a password")
	}
}

// A node that is not running is not an endpoint: a picker offering a stopped database is offering
// a connection error.
func TestSampleCodeTargetsSkipStoppedNodes(t *testing.T) {
	app := newTestApp(t)
	doc := designDoc{Nodes: []designNode{
		{ID: "ps1", Type: "ps", Label: "ps-01"},
		{ID: "ps2", Type: "ps", Label: "ps-02"},
	}}
	sec := scJSON(t, pxcSecrets{AppUser: "app", AppPassword: "pw"})
	st := scTestStack(t, app, doc, []Deployment{
		{NodeID: "ps1", Config: scJSON(t, pxcConfig{FQDN: "ps-01.example.net"}), Secrets: sec},
		{NodeID: "ps2", State: DeployStopped, Config: scJSON(t, pxcConfig{FQDN: "ps-02.example.net"}), Secrets: sec},
	})
	targets := app.scStackTargets(st, "ubuntu")
	if len(targets) != 1 || targets[0].ID != "ps1" {
		t.Fatalf("expected only the running node, got %d: %+v", len(targets), targets)
	}
	// And the CA path follows the *client's* OS, not the server's.
	if targets[0].TLS.Mode == scTLSVerify && targets[0].TLS.CA != scCAPath("ubuntu") {
		t.Errorf("CA path %q is not the Debian one", targets[0].TLS.CA)
	}
	if _, err := app.scFindTarget(st, "ps2", "ubuntu"); err == nil {
		t.Error("a stopped node resolved as an endpoint")
	} else if !strings.Contains(err.Error(), "stopped or removed") {
		t.Errorf("the error should say why: %v", err)
	}
}

// Group Replication exposes no endpoint of its own — the router on each member is the endpoint,
// and its two ports mean different things.
func TestSampleCodeTargetsGroupReplicationRouter(t *testing.T) {
	app := newTestApp(t)
	doc := designDoc{
		Nodes: []designNode{
			{ID: "n1", Type: "innodb", Label: "gr-01", FrameID: "f"},
			{ID: "n2", Type: "innodb", Label: "gr-02", FrameID: "f"},
		},
		Frames: []designFrame{{ID: "f", Type: "innodb", Label: "GR Cluster"}},
	}
	sec := scJSON(t, pxcSecrets{AppUser: "app", AppPassword: "pw"})
	st := scTestStack(t, app, doc, []Deployment{
		{NodeID: "n1", Config: scJSON(t, innodbConfig{FQDN: "gr-01.example.net"}), Secrets: sec},
		{NodeID: "n2", Config: scJSON(t, innodbConfig{FQDN: "gr-02.example.net"}), Secrets: sec},
	})
	targets := app.scStackTargets(st, "oraclelinux")
	rw := scTargetByID(t, targets, "f#router-rw")
	ro := scTargetByID(t, targets, "f#router-ro")
	if rw.Port != routerRWPort || ro.Port != routerROPort {
		t.Errorf("router ports: %d / %d", rw.Port, ro.Port)
	}
	if !strings.Contains(rw.Note, "primary") || !strings.Contains(ro.Note, "refused") {
		t.Errorf("the router endpoints do not explain themselves: %q / %q", rw.Note, ro.Note)
	}
}

// The generated sample must refuse a client that cannot speak the endpoint's engine — the picker
// makes that unreachable, and the API is not the picker.
func TestSampleCodeRefusesMismatchedClient(t *testing.T) {
	app := newTestApp(t)
	doc := designDoc{Nodes: []designNode{
		{ID: "pg1", Type: "pg", Label: "pg-01"},
		{ID: "lc", Type: "linuxclient", Label: "linuxclient1"},
	}}
	st := scTestStack(t, app, doc, []Deployment{
		{NodeID: "pg1", Config: scJSON(t, pgConfig{FQDN: "pg-01.example.net", PGMajor: "17"}), Secrets: scJSON(t, pgSecrets{SuperUser: "postgres", SuperPassword: "pw"})},
		{NodeID: "lc", Config: scJSON(t, linuxClientConfig{OS: "oraclelinux"})},
	})
	lc, err := app.store.GetDeployment(st.ID, "lc")
	if err != nil {
		t.Fatalf("read the Linux Client: %v", err)
	}

	if _, _, _, err := app.scBuild(t.Context(), st, lc, scRequest{
		Sample: "mysql/python/pymysql/crud", Target: "pg1",
	}); err == nil || !strings.Contains(err.Error(), "postgres endpoint") {
		t.Errorf("a MySQL driver against a PostgreSQL endpoint gave %v", err)
	}

	// And the matching one resolves, with the endpoint's own coordinates in the plan.
	_, g, plan, err := app.scBuild(t.Context(), st, lc, scRequest{
		Sample: "postgres/python/psycopg/crud", Target: "pg1",
	})
	if err != nil {
		t.Fatalf("psycopg against PostgreSQL: %v", err)
	}
	if g.Target.Host != "pg-01.example.net" {
		t.Errorf("host is %q", g.Target.Host)
	}
	if len(plan.Files) == 0 || plan.Run.Cmd == "" {
		t.Error("the plan has no project or no run command")
	}
	// A client certificate on a plaintext connection is a contradiction, and saying so is
	// more useful than generating code that quietly ignores it.
	if _, _, _, err := app.scBuild(t.Context(), st, lc, scRequest{
		Sample: "postgres/python/psycopg/crud", Target: "pg1", TLS: scTLSOff, ClientCert: "alice",
	}); err == nil || !strings.Contains(err.Error(), "only presented on a TLS connection") {
		t.Errorf("mutual TLS without TLS gave %v", err)
	}
}
