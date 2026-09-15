package main

// Database Explorer — connection discovery and, on every request, re-authorization.
//
// The point of the feature is that nobody types a hostname. DBCanvas deployed these
// databases, so the tree is built by walking the designs the caller owns (an admin
// owns the lot), asking each deployment whether it is running, and turning what is
// there into endpoints. It is the same reading of a stack that Sample Client Code
// does — members, HAProxy's two ports, mongos, a replica set as a whole, a Valkey
// cluster's seeds, MySQL Router, All-in-One instances — with two differences that
// matter here: the account is the administrative one (you cannot browse a schema as
// an application user), and PMM Server's own internal databases are included.
//
// Discovery is also the authorization boundary. A connection id is re-resolved here
// on every single request, against ListStacks for *this* caller, so an id belonging
// to somebody else's stack does not resolve — it is not checked against a cache, and
// it is never taken on trust because the browser sent it back.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The tree's headings.
const (
	dexGroupMySQL      = "MySQL / PXC"
	dexGroupPostgres   = "PostgreSQL"
	dexGroupMongo      = "MongoDB"
	dexGroupValkey     = "Valkey"
	dexGroupClickHouse = "ClickHouse"
	dexGroupPMM        = "PMM Server"
	dexGroupK8s        = "Kubernetes operators"
)

// dexPMMWarning is shown on both PMM connections, every time, because the databases
// behind them are not a lab's — they are the monitoring system's own, and a stray
// UPDATE in one is how a PMM installation stops working.
const dexPMMWarning = "These databases are used internally by PMM. DBCanvas exposes them for " +
	"inspection and troubleshooting. Modifying PMM internal data may corrupt or break the PMM installation."

const dexPMMPolicy = "pmm-internal"

// dexTarget is a resolved connection: the public half the browser sees, plus the
// private half it never does. Nothing in this struct is serialised as a whole —
// only Conn is, and Conn has no password field to leak.
type dexTarget struct {
	Conn  dexConnection
	Stack Stack
	// DialNodeID is the node whose container is actually dialled, which is not
	// always the node the credentials came from: an HAProxy endpoint dials HAProxy
	// and authenticates with the backend cluster's account.
	DialNodeID  string
	ContainerID string
	Port        int
	User        string
	Pass        string
	// AuthDB is MongoDB's authSource.
	AuthDB string
	// ReplicaSet and Hosts describe a multi-member endpoint.
	ReplicaSet string
	// TLSVerify says this endpoint has a certificate issued by the stack's CA, and
	// therefore something to verify. It is not "does a CA exist" — every stack with
	// an Intranet has one — but "was this node deployed with GenerateCert", which is
	// what decides whether the server is listening for TLS at all. A node without one
	// refuses a TLS connection outright, so demanding verification of it would not be
	// stricter, it would be broken. Same reading as scDeriveTLS.
	TLSVerify bool
	// Exec names the container a client is run inside when Transport is "exec".
	ExecContainer string
	// K8s is the operator endpoint this target came from, empty for everything else.
	// It carries the pod to exec a client in and the kubectl host to do it through,
	// which is what makes a ClusterIP-only database reachable at all.
	K8s *k8sEndpoint
	// CHUser/CHPass are ClickHouse's own credentials, read off the PMM container.
	CHUser string
	CHPass string
	CHDB   string
}

// ---------------------------------------------------------------- capabilities

func dexCapsFor(engine string, readOnly bool) dexCaps {
	c := dexCaps{SchemaBrowser: true, Charts: true, QueryCancel: true}
	switch engine {
	case dexMySQL:
		c.SQL, c.Explain, c.Transactions, c.MultiResult = true, true, true, true
		c.EditableRows = !readOnly
	case dexPostgres:
		c.SQL, c.Explain, c.Transactions, c.Schemas, c.MultiResult = true, true, true, true, true
		c.EditableRows = !readOnly
	case dexClickHouse:
		c.SQL, c.Explain = true, true
		// ClickHouse has no OLTP row identity, so the grid never offers an in-place
		// edit for it. Saying so through the capability is better than offering a
		// button that would have to explain itself afterwards.
		c.EditableRows, c.Transactions = false, false
	case dexMongoDB:
		c.Documents = true
		c.EditableRows = !readOnly
	case dexValkey:
		c.KeyValue = true
		c.EditableRows = !readOnly
		c.Charts = false
		c.QueryCancel = false
	}
	return c
}

// ---------------------------------------------------------------- discovery

// dexConnections is every endpoint the caller may reach, grouped by stack. The
// ordering is deliberate: the endpoint a client should normally use comes first
// within its group, then the members, so the obvious choice is the top one.
func (a *App) dexConnections(ctx context.Context, u User) []dexConnGroup {
	stacks, _ := a.store.ListStacks(u.ID, u.Role == RoleAdmin)
	out := []dexConnGroup{}
	for _, s := range stacks {
		st, err := a.store.GetStack(s.ID)
		if err != nil {
			continue
		}
		conns := a.dexStackConnections(ctx, st)
		if len(conns) == 0 {
			continue
		}
		byGroup := map[string][]dexConnection{}
		var order []string
		for _, c := range conns {
			if _, ok := byGroup[c.Group]; !ok {
				order = append(order, c.Group)
			}
			byGroup[c.Group] = append(byGroup[c.Group], c)
		}
		g := dexConnGroup{StackID: st.ID, StackName: st.Name, Connections: len(conns)}
		for _, name := range order {
			list := byGroup[name]
			sort.SliceStable(list, func(i, j int) bool {
				if list[i].Preferred != list[j].Preferred {
					return list[i].Preferred
				}
				return list[i].Label < list[j].Label
			})
			g.Groups = append(g.Groups, dexEngineList{Name: name, Engine: list[0].Engine, Connections: list})
		}
		out = append(out, g)
	}
	return out
}

// dexStackConnections enumerates one stack. It returns the DTOs; the credentials are
// resolved separately (dexResolve) so that listing connections never assembles a
// password it has no use for.
func (a *App) dexStackConnections(ctx context.Context, st Stack) []dexConnection {
	var out []dexConnection
	for _, t := range a.dexStackTargets(ctx, st) {
		out = append(out, t.Conn)
	}
	return out
}

// dexStackTargets is the real enumeration: every endpoint in one stack, resolved
// including credentials. Callers that only need the public half take .Conn.
func (a *App) dexStackTargets(ctx context.Context, st Stack) []dexTarget {
	doc := buildDoc(st)
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	deps := map[string]Deployment{}
	list, _ := a.store.ListDeployments(st.ID)
	for _, d := range list {
		deps[d.NodeID] = d
	}
	running := func(id string) (Deployment, bool) {
		d, ok := deps[id]
		return d, ok && d.State == DeployRunning && d.ContainerID != ""
	}

	var out []dexTarget
	// --- the nodes themselves -------------------------------------------------
	for _, n := range doc.Nodes {
		dep, ok := running(n.ID)
		if !ok {
			continue
		}
		switch {
		case n.Type == "aio":
			out = append(out, a.dexAIOTargets(st, n, dep, domain)...)
			continue
		case n.Type == "pmm":
			out = append(out, a.dexPMMTargets(ctx, st, n, dep, hosts, domain)...)
			continue
		}
		engine := scEngineOf(n.Type)
		if engine == "" {
			continue
		}
		p := scProbe(dep)
		// A MongoDB config server is a cluster's bookkeeping, not a database
		// anyone should be browsing; a shard member is, with a note.
		if engine == dexMongoDB && p.Role == "config" {
			continue
		}
		user, pass, ok := a.dexCredentials(engine, dep)
		if !ok {
			continue
		}
		host := p.FQDN
		if host == "" {
			host = fqdnOf(hosts[n.ID], domain)
		}
		t := dexTarget{
			Stack: st, DialNodeID: n.ID, ContainerID: dep.ContainerID,
			Port: scDefaultPort(engine), User: user, Pass: pass, AuthDB: "admin",
			ReplicaSet: p.ReplSet, TLSVerify: p.GenerateCert,
			Conn: dexConnection{
				ID:      dexRef{StackID: st.ID, Shape: dexShapeNode, Target: n.ID}.String(),
				StackID: st.ID, StackName: st.Name, NodeID: n.ID, Label: n.Label,
				Engine: engine, Kind: n.Type, Product: scProductLabel(n.Type), Version: p.major(),
				Group: dexGroupFor(engine), Role: "member", Host: host, Port: scDefaultPort(engine),
				Status: "running", User: user, Transport: "network", Caps: dexCapsFor(engine, false),
			},
		}
		if frame := frameByID(doc, n.FrameID); frame.ID != "" {
			t.Conn.Label = frame.Label + " · " + n.Label
		} else {
			t.Conn.Role = "primary"
			t.Conn.Preferred = true
		}
		switch {
		case engine == dexMongoDB && p.Role == "mongos":
			t.Conn.Role, t.Conn.Preferred = "router", true
			t.Conn.Note = "a mongos router — the entry point to the sharded cluster"
		case engine == dexMongoDB && p.ReplSet != "":
			t.Conn.Note = "one replica-set member; the set as a whole is offered above"
		case n.Type == "valkeycluster":
			t.Conn.Note = "one shard of a Valkey cluster; the cluster as a whole is offered above"
		case p.Role == "secondary" || n.Role == "replica":
			t.Conn.Role, t.Conn.Note = "replica", "a replica — writes will be refused here"
		}
		out = append(out, t)
	}

	// --- clusters as a whole, where the set and not the member is the endpoint --
	for _, f := range doc.Frames {
		switch f.Type {
		case "psmrs":
			if t, ok := a.dexReplicaSetTarget(st, doc, f, hosts, domain, running); ok {
				out = append(out, t)
			}
		case "valkeycluster":
			if t, ok := a.dexValkeyClusterTarget(st, doc, f, hosts, domain, running); ok {
				out = append(out, t)
			}
		case "innodb", "mysqlceinnodb":
			out = append(out, a.dexRouterTargets(st, doc, f, hosts, domain, running)...)
		}
	}

	// --- databases an operator deployed inside a K3D frame ----------------------
	// Discovered rather than read off the design: what a cluster actually has is the
	// operator's business, and it changes while the frame stays the same.
	for _, e := range a.k8sStackEndpoints(ctx, st) {
		if t, ok := dexK8sTarget(st, e); ok {
			out = append(out, t)
		}
	}

	// --- what sits in front of them --------------------------------------------
	for _, n := range doc.Nodes {
		dep, ok := running(n.ID)
		if !ok {
			continue
		}
		switch n.Type {
		case "haproxy":
			out = append(out, a.dexHAProxyTargets(st, doc, n, dep, hosts, domain, running)...)
		case "proxysql":
			if t, ok := a.dexProxySQLTarget(st, doc, n, dep, hosts, domain, running); ok {
				out = append(out, t)
			}
		}
	}
	return out
}

func dexGroupFor(engine string) string {
	switch engine {
	case dexMySQL:
		return dexGroupMySQL
	case dexPostgres:
		return dexGroupPostgres
	case dexMongoDB:
		return dexGroupMongo
	case dexValkey:
		return dexGroupValkey
	case dexClickHouse:
		return dexGroupClickHouse
	}
	return engine
}

// dexCredentials is the account the Explorer connects as. It is the administrative
// one rather than the application one Sample Client Code uses, and for a reason that
// is specific to this feature: browsing a schema means reading the server's own
// catalogue, and an application account provisioned to own one database cannot see
// past it. The password is resolved here and never leaves the process.
func (a *App) dexCredentials(engine string, dep Deployment) (user, pass string, ok bool) {
	switch engine {
	case dexMySQL:
		var s pxcSecrets
		json.Unmarshal(dep.Secrets, &s)
		user, pass = s.AdminUser, s.AdminPassword
		if user == "" {
			user = "admin"
		}
		if pass == "" {
			return "", "", false
		}
		return user, pass, true
	case dexPostgres:
		var s pgSecrets
		json.Unmarshal(dep.Secrets, &s)
		if s.SuperPassword == "" {
			s = pgFamilySecrets()
		}
		return s.Super(), s.SuperPassword, true
	case dexMongoDB:
		var s mongoSecrets
		json.Unmarshal(dep.Secrets, &s)
		if s.AdminUser == "" {
			s.AdminUser = "admin"
		}
		if s.AdminPassword == "" {
			return "", "", false
		}
		return s.AdminUser, s.AdminPassword, true
	case dexValkey:
		return "default", valkeyPasswordFor(dep), true
	}
	return "", "", false
}

// dexAIOTargets are the database instances inside one All-in-One node.
func (a *App) dexAIOTargets(st Stack, n designNode, dep Deployment, domain string) []dexTarget {
	var out []dexTarget
	for _, m := range aioTargetableInstances(dep) {
		engine, port, user, pass := aioInstanceCreds(dep, m)
		if engine == "" {
			continue
		}
		out = append(out, dexTarget{
			Stack: st, DialNodeID: n.ID, ContainerID: dep.ContainerID,
			Port: port, User: user, Pass: pass, AuthDB: "admin",
			Conn: dexConnection{
				ID:      dexRef{StackID: st.ID, Shape: dexShapeAIO, Target: n.ID, Extra: m.Inst}.String(),
				StackID: st.ID, StackName: st.Name, NodeID: n.ID,
				Label: aioTargetLabel(n.Label, m), Engine: engine, Kind: m.Kind,
				Product: scProductLabel(m.Kind), Group: dexGroupFor(engine), Role: "instance",
				Host: fqdnOf(m.Inst, domain), Port: port, Status: "running", User: user,
				Note: "one instance inside an All-in-One node", Transport: "network",
				Caps: dexCapsFor(engine, false),
			},
		})
	}
	return out
}

// dexReplicaSetTarget is a MongoDB replica set as one endpoint. DBCanvas dials the
// first running member directly (Docker's embedded DNS does not resolve the
// Intranet's member names, so driver auto-discovery would fail), but the endpoint is
// presented as the set because that is what it is.
func (a *App) dexReplicaSetTarget(st Stack, doc designDoc, f designFrame, hosts map[string]string, domain string, running func(string) (Deployment, bool)) (dexTarget, bool) {
	members := scFrameMembers(doc, f.ID, running)
	if len(members) == 0 {
		return dexTarget{}, false
	}
	dep, _ := running(members[0].ID)
	p := scProbe(dep)
	user, pass, ok := a.dexCredentials(dexMongoDB, dep)
	if !ok {
		return dexTarget{}, false
	}
	var addrs []string
	for _, m := range members {
		addrs = append(addrs, fqdnOf(hosts[m.ID], domain))
	}
	return dexTarget{
		Stack: st, DialNodeID: members[0].ID, ContainerID: dep.ContainerID,
		Port: mongoPort, User: user, Pass: pass, AuthDB: "admin", ReplicaSet: p.ReplSet,
		TLSVerify: p.GenerateCert,
		Conn: dexConnection{
			ID:      dexRef{StackID: st.ID, Shape: dexShapeReplSet, Target: f.ID}.String(),
			StackID: st.ID, StackName: st.Name, NodeID: members[0].ID,
			Label: f.Label + " (replica set)", Engine: dexMongoDB, Kind: "psmrs",
			Product: scProductLabel("psmrs"), Version: p.major(), Group: dexGroupMongo,
			Role: "primary", Preferred: true, Host: addrs[0], Port: mongoPort, Hosts: addrs,
			Status: "running", User: user, Transport: "network",
			Note: "the whole set — browse it here, or pick one member below to see that member's own view",
			Caps: dexCapsFor(dexMongoDB, false),
		},
	}, true
}

// dexValkeyClusterTarget is a Valkey cluster as one endpoint, seeded from the first
// shard. SCAN is per-node in a cluster, so the browser says which shard it is
// reading — see the Valkey adapter.
func (a *App) dexValkeyClusterTarget(st Stack, doc designDoc, f designFrame, hosts map[string]string, domain string, running func(string) (Deployment, bool)) (dexTarget, bool) {
	members := scFrameMembers(doc, f.ID, running)
	if len(members) == 0 {
		return dexTarget{}, false
	}
	dep, _ := running(members[0].ID)
	var addrs []string
	for _, m := range members {
		addrs = append(addrs, fqdnOf(hosts[m.ID], domain))
	}
	return dexTarget{
		Stack: st, DialNodeID: members[0].ID, ContainerID: dep.ContainerID,
		Port: valkeyPort, User: "default", Pass: valkeyPasswordFor(dep),
		Conn: dexConnection{
			ID:      dexRef{StackID: st.ID, Shape: dexShapeVKC, Target: f.ID}.String(),
			StackID: st.ID, StackName: st.Name, NodeID: members[0].ID,
			Label: f.Label + " (cluster)", Engine: dexValkey, Kind: "valkeycluster",
			Product: scProductLabel("valkeycluster"), Group: dexGroupValkey,
			Role: "primary", Preferred: true, Host: addrs[0], Port: valkeyPort, Hosts: addrs,
			Status: "running", User: "default", Transport: "network",
			Note: "seeded from the first shard — a cluster's keyspace is per-node, so each shard is browsed separately",
			Caps: dexCapsFor(dexValkey, false),
		},
	}, true
}

// dexRouterTargets are the MySQL Router ports a Group Replication cluster installs
// on every member: 6446 follows the primary, 6447 lands on a secondary.
func (a *App) dexRouterTargets(st Stack, doc designDoc, f designFrame, hosts map[string]string, domain string, running func(string) (Deployment, bool)) []dexTarget {
	members := scFrameMembers(doc, f.ID, running)
	if len(members) == 0 {
		return nil
	}
	dep, _ := running(members[0].ID)
	user, pass, ok := a.dexCredentials(dexMySQL, dep)
	if !ok {
		return nil
	}
	p := scProbe(dep)
	host := fqdnOf(hosts[members[0].ID], domain)
	mk := func(extra, label, note, role string, port int, preferred bool) dexTarget {
		return dexTarget{
			Stack: st, DialNodeID: members[0].ID, ContainerID: dep.ContainerID,
			Port: port, User: user, Pass: pass,
			Conn: dexConnection{
				ID:      dexRef{StackID: st.ID, Shape: dexShapeRouter, Target: f.ID, Extra: extra}.String(),
				StackID: st.ID, StackName: st.Name, NodeID: members[0].ID,
				Label: f.Label + " · " + label, Engine: dexMySQL, Kind: "router-" + extra,
				Product: scProductLabel(f.Type), Version: p.major(), Group: dexGroupMySQL,
				Role: role, Preferred: preferred, Host: host, Port: port, Status: "running",
				User: user, Note: note, Transport: "network", Caps: dexCapsFor(dexMySQL, false),
			},
		}
	}
	return []dexTarget{
		mk("rw", "MySQL Router (read/write)", "routed to whichever member is primary right now", "router", routerRWPort, true),
		mk("ro", "MySQL Router (read-only)", "routed to a secondary — writes will be refused", "replica", routerROPort, false),
	}
}

// dexHAProxyTargets are HAProxy's two ports. They are separate connections because
// they mean different things — and for a PXC cluster the write port is the endpoint
// a client is meant to use, which is why it is the preferred one.
func (a *App) dexHAProxyTargets(st Stack, doc designDoc, n designNode, dep Deployment, hosts map[string]string, domain string, running func(string) (Deployment, bool)) []dexTarget {
	back, backKind, ok := haproxyBackend(doc, n.ID)
	if !ok {
		return nil
	}
	engine := scEngineOf(backKind)
	if engine != dexMySQL && engine != dexPostgres {
		return nil
	}
	members := scFrameMembers(doc, back.ID, running)
	if len(members) == 0 {
		return nil
	}
	mdep, _ := running(members[0].ID)
	user, pass, ok := a.dexCredentials(engine, mdep)
	if !ok {
		return nil
	}
	p := scProbe(mdep)
	host := fqdnOf(hosts[n.ID], domain)
	mk := func(extra, label, note, role string, port int, preferred bool) dexTarget {
		return dexTarget{
			// The dialled container is HAProxy's; the account is the cluster's.
			Stack: st, DialNodeID: n.ID, ContainerID: dep.ContainerID,
			Port: port, User: user, Pass: pass,
			Conn: dexConnection{
				ID:      dexRef{StackID: st.ID, Shape: dexShapeHAProxy, Target: n.ID, Extra: extra}.String(),
				StackID: st.ID, StackName: st.Name, NodeID: n.ID,
				Label: n.Label + " · " + label, Engine: engine, Kind: "haproxy-" + extra,
				Product: "HAProxy → " + scProductLabel(backKind), Version: p.major(),
				Group: dexGroupFor(engine), Role: role, Preferred: preferred,
				Host: host, Port: port, Status: "running", User: user, Note: note,
				Transport: "network", Caps: dexCapsFor(engine, false),
			},
		}
	}
	return []dexTarget{
		mk("write", "write port", "balanced onto the member that can take writes — the endpoint a client should use", "primary", haproxyWritePort, true),
		mk("read", "read port", "balanced across the replicas — writes will be refused", "replica", haproxyReadPort, false),
	}
}

// dexProxySQLTarget is ProxySQL's client port; the read/write split happens inside
// it, so unlike HAProxy it is one endpoint.
func (a *App) dexProxySQLTarget(st Stack, doc designDoc, n designNode, dep Deployment, hosts map[string]string, domain string, running func(string) (Deployment, bool)) (dexTarget, bool) {
	back, backKind, ok := backendFrameForProxySQL(doc, n.ID)
	if !ok {
		return dexTarget{}, false
	}
	members := scFrameMembers(doc, back.ID, running)
	if len(members) == 0 {
		return dexTarget{}, false
	}
	mdep, _ := running(members[0].ID)
	user, pass, ok := a.dexCredentials(dexMySQL, mdep)
	if !ok {
		return dexTarget{}, false
	}
	p := scProbe(mdep)
	return dexTarget{
		Stack: st, DialNodeID: n.ID, ContainerID: dep.ContainerID,
		Port: proxysqlMySQLPort, User: user, Pass: pass,
		Conn: dexConnection{
			ID:      dexRef{StackID: st.ID, Shape: dexShapeProxySQL, Target: n.ID}.String(),
			StackID: st.ID, StackName: st.Name, NodeID: n.ID,
			Label: n.Label + " · client port", Engine: dexMySQL, Kind: "proxysql",
			Product: "ProxySQL → " + scProductLabel(backKind), Version: p.major(),
			Group: dexGroupMySQL, Role: "proxy", Preferred: true,
			Host: fqdnOf(hosts[n.ID], domain), Port: proxysqlMySQLPort, Status: "running",
			User: user, Note: "ProxySQL splits reads from writes itself, so this one port takes both",
			Transport: "network", Caps: dexCapsFor(dexMySQL, false),
		},
	}, true
}

// dexK8sTarget turns a discovered operator endpoint into an Explorer connection.
//
// The transport is decided here and stated in the DTO: an endpoint with a network
// address is dialled like any other database, and one without is reached by running a
// client in its pod. An endpoint that is neither — no address and no running pod —
// is not offered, because there is nothing behind it to open.
func dexK8sTarget(st Stack, e k8sEndpoint) (dexTarget, bool) {
	transport := ""
	switch {
	case e.Reachable():
		transport = "network"
	case e.Execable():
		transport = "exec"
	default:
		return dexTarget{}, false
	}
	// Only the engines with an exec transport can use the exec route; Valkey and the
	// network-only ones would have nothing to run.
	if transport == "exec" && e.Engine != dexPostgres {
		return dexTarget{}, false
	}
	caps := dexCapsFor(e.Engine, false)
	if transport == "exec" {
		// A statement is a process here, so there is nothing to cancel from another
		// request and nothing that would make a second connection.
		caps.QueryCancel = false
	}
	ep := e
	t := dexTarget{
		Stack: st, DialNodeID: e.FrameID, K8s: &ep,
		Port: e.Port, User: e.User, Pass: e.Pass, AuthDB: e.AuthDB,
		ExecContainer: e.ServerID,
		Conn: dexConnection{
			ID:      dexRef{StackID: st.ID, Shape: dexShapeK8s, Target: e.FrameID, Extra: e.Service}.String(),
			StackID: st.ID, StackName: st.Name, NodeID: e.FrameID,
			Label: e.Label, Engine: e.Engine, Kind: "k8s-" + e.Kind,
			Product: k3dOperatorLabel(e.Operator), Group: dexGroupK8s,
			Role: e.Role, Preferred: e.Preferred, Host: e.Addr, Port: e.Port,
			Status: "running", User: e.User, Note: dexK8sNote(e), Transport: transport,
			Caps: caps,
			// A ClusterIP endpoint with a selector can be given an address; one
			// whose endpoints the operator manages cannot (see k8sexpose.go).
			Exposable: !e.Reachable() && len(e.Selector) > 0,
			ExposedBy: e.ExposedBy,
		},
	}
	if transport == "exec" {
		t.Conn.Host = e.Service + "." + e.Namespace + ".svc"
	}
	return t, true
}

// dexK8sNote is the sentence under a Kubernetes connection: what the tier is, and —
// when there is no network address — why, because "ClusterIP" is the answer to a
// question the user did not know they had asked.
func dexK8sNote(e k8sEndpoint) string {
	parts := []string{}
	if e.Note != "" {
		parts = append(parts, e.Note)
	}
	parts = append(parts, fmt.Sprintf("Service %s/%s (%s)", e.Namespace, e.Service, e.SvcType))
	if !e.Reachable() && e.Why != "" {
		parts = append(parts, e.Why+" — DBCanvas reads it by running a client in its pod instead")
	}
	return strings.Join(parts, " · ")
}

// ---------------------------------------------------------------- resolution

// dexResolve turns a connection id from the browser into a target, enforcing that
// the caller may reach it. This is the function every handler calls first, and it is
// the only place that decides access:
//
//   - the stack is loaded through GetStack and checked against the caller's
//     ownership (an admin passes, anybody else must own it);
//   - the id is then matched against the endpoints that stack *currently* has, so a
//     node that has been stopped, removed or renamed stops resolving;
//   - and the credentials are attached here, not carried from the browser.
//
// A forged or stale id therefore fails the same way an unauthorized one does, and
// neither reveals whether the stack exists.
func (a *App) dexResolve(ctx context.Context, u User, id string) (dexTarget, error) {
	ref, err := dexParseID(id)
	if err != nil {
		return dexTarget{}, err
	}
	st, err := a.store.GetStack(ref.StackID)
	if err != nil {
		return dexTarget{}, fmt.Errorf("connection not found")
	}
	if st.OwnerID != u.ID && u.Role != RoleAdmin {
		// Deliberately the same sentence as a missing stack: whether a stack id
		// exists is not something an unauthorized caller should be able to probe.
		return dexTarget{}, fmt.Errorf("connection not found")
	}
	for _, t := range a.dexStackTargets(ctx, st) {
		if t.Conn.ID == id {
			return t, nil
		}
	}
	return dexTarget{}, fmt.Errorf("connection not found — the node may have been stopped or removed")
}

// dexSummaryLine is the one-line description of a connection used in history and in
// error messages. It never includes the account's password, and it is the only place
// a connection is turned into text for storage.
func (t dexTarget) dexSummaryLine() string {
	parts := []string{t.Conn.StackName, t.Conn.Label}
	return strings.Join(parts, " · ")
}
