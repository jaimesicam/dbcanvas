package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// samplecode_targets.go — the endpoints a sample can be generated against, read off the stack.
//
// This is the half of the feature that makes the generated code worth anything. A driver's
// quickstart has `localhost`, `root` and `CHANGEME` in it because the person who wrote it could
// not know anything else. DBCanvas deployed the thing: it knows the DNS name the Intranet
// publishes, the port the engine listens on, the account that was created for applications, the
// password that came out of .env, and whether the server's certificate was signed by the CA that
// is already in this Linux Client's trust store. None of that should ever appear in a generated
// sample as a placeholder.
//
// ------------------------------------------------------------------- what counts as an endpoint
//
// Not "every database node". An endpoint is *something an application would connect to*, which on
// a canvas is a wider and more interesting set than the node list:
//
//   - a standalone node, and every member of a cluster (connecting to one member on purpose is
//     most of what a lab is for);
//   - a replica set as a whole, with its member list and set name, which is what a MongoDB driver
//     actually wants;
//   - a Valkey cluster as a whole, for the same reason;
//   - HAProxy's write port and its read port, which are two different endpoints with two
//     different meanings and are the clearest demonstration in the app of why that matters;
//   - ProxySQL's client port;
//   - the MySQL Router ports that a Group Replication cluster installs on every member — 6446 goes
//     to the primary wherever it currently is, 6447 to a secondary;
//   - every instance inside an All-in-One node, which are several databases on one host.
//
// Nothing in the list is hard-coded to a stack shape: it is derived from the design document and
// the deployments, so a stack with two PXC clusters and a proxy in front of each produces the
// endpoints those clusters actually have.

// scTLSMode is what the generated client is told to do about TLS. Three values rather than each
// driver's own vocabulary, because every driver spells these three differently and the generator
// is the thing that knows the spelling.
const (
	scTLSOff     = "off"     // plaintext
	scTLSRequire = "require" // encrypt, but do not check who the server is
	scTLSVerify  = "verify"  // encrypt and verify the certificate chain and the hostname
)

// scTLS is the TLS posture for one endpoint.
type scTLS struct {
	Mode string `json:"mode"`
	CA   string `json:"ca,omitempty"` // the CA bundle path on the Linux Client, when verifying
	Why  string `json:"why"`          // why this is the default, in one sentence
}

// scTarget is one endpoint, resolved. It is what the generator reads and what the picker shows.
//
// Password carries no JSON tag of its own on purpose — see scTargetDTO, which is what the target
// *list* returns. A password reaches the browser only inside a sample the user asked to generate,
// the same way a node panel shows the credentials for the node it is describing.
type scTarget struct {
	ID      string // stable across reloads: "<nodeID>" or "<nodeID>@role" or "<frameID>#frame"
	Label   string // "ps-01 — Percona Server for MySQL 8.4"
	Engine  string
	Kind    string // the node/frame type, or "haproxy-rw"/"router-ro"/… for a derived endpoint
	Product string
	Major   string
	Host    string
	Port    int
	// Hosts is every address of a multi-host endpoint — a replica set's members, a Valkey
	// cluster's shards. Empty for a single-address endpoint; Host is always the first address.
	Hosts      []string
	ReplicaSet string
	User       string
	Password   string
	Database   string // the database the sample will use
	AuthDB     string // MongoDB authSource
	Role       string // primary | member | replica | router | proxy | instance
	Note       string // what a user should know before pointing an application here
	TLS        scTLS
	StackID    int64
	StackName  string
	NodeID     string // the deployment the credentials came from
}

// scTargetDTO is an endpoint as the picker sees it: everything except the password.
type scTargetDTO struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	Engine     string   `json:"engine"`
	Kind       string   `json:"kind"`
	Product    string   `json:"product"`
	Major      string   `json:"major,omitempty"`
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	Hosts      []string `json:"hosts,omitempty"`
	ReplicaSet string   `json:"replicaSet,omitempty"`
	User       string   `json:"user"`
	Database   string   `json:"database"`
	Role       string   `json:"role"`
	Note       string   `json:"note,omitempty"`
	TLS        scTLS    `json:"tls"`
}

func (t scTarget) dto() scTargetDTO {
	return scTargetDTO{
		ID: t.ID, Label: t.Label, Engine: t.Engine, Kind: t.Kind, Product: t.Product, Major: t.Major,
		Host: t.Host, Port: t.Port, Hosts: t.Hosts, ReplicaSet: t.ReplicaSet,
		User: t.User, Database: t.Database, Role: t.Role, Note: t.Note, TLS: t.TLS,
	}
}

// Addresses is every host:port for this endpoint, which is what the multi-host drivers want.
func (t scTarget) Addresses() []string {
	if len(t.Hosts) == 0 {
		return []string{fmt.Sprintf("%s:%d", t.Host, t.Port)}
	}
	out := make([]string, 0, len(t.Hosts))
	for _, h := range t.Hosts {
		if strings.Contains(h, ":") {
			out = append(out, h)
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d", h, t.Port))
	}
	return out
}

// scDemoDatabase is the database (or schema, or key prefix owner) every sample works in. One name
// across every engine so the same program can be compared side by side, and a name that is
// obviously not a system database so nothing important is in the way.
const scDemoDatabase = "dbcanvas"

// scCADir is where trustIntranetCA put the stack CA on this node — the file every generated client
// points at when it verifies a server certificate. Two paths because the two families' trust tools
// read from two different directories; both are the file the node already trusts, which is why the
// generated code can verify without anything being copied anywhere first.
func scCAPath(nodeOS string) string {
	if isDebianOS(nodeOS) {
		return "/usr/local/share/ca-certificates/dbcanvas-ca.crt"
	}
	return "/etc/pki/ca-trust/source/anchors/dbcanvas-ca.crt"
}

// scCfgProbe reads the handful of fields this feature wants out of any node's config, whatever
// engine wrote it. A loose struct rather than the engine's own type because the caller has a
// deployment and not yet an engine, and because every one of these fields is optional: a node
// deployed before a field existed simply does not have it, and the defaults below are what that
// means.
type scCfgProbe struct {
	FQDN         string `json:"fqdn"`
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	GenerateCert bool   `json:"generateCert"`
	ReplSet      string `json:"replSet"`
	Role         string `json:"role"`
	// The product series, spelled differently by every engine.
	PSMajor      string `json:"psMajor"`
	PGMajor      string `json:"pgMajor"`
	PSMDBMajor   string `json:"psmdbMajor"`
	MariaDBMajor string `json:"mariadbMajor"`
	MySQLCEMajor string `json:"mysqlceMajor"`
	ValkeyMajor  string `json:"valkeyMajor"`
	PSVersion    string `json:"psVersion"`
	PXCVersion   string `json:"pxcVersion"`
	Version      string `json:"version"`
}

// major is the product series this node runs, from whichever field its engine filled in. Falls
// back to the first two components of a full version ("8.0.36-28.1" → "8.0"), which is what the
// MySQL-family nodes record instead of a series.
func (p scCfgProbe) major() string {
	for _, m := range []string{p.PSMajor, p.PGMajor, p.PSMDBMajor, p.MariaDBMajor, p.MySQLCEMajor, p.ValkeyMajor} {
		if strings.TrimSpace(m) != "" {
			return strings.TrimSpace(m)
		}
	}
	for _, v := range []string{p.PSVersion, p.PXCVersion, p.Version} {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		parts := strings.Split(v, ".")
		if len(parts) >= 2 {
			return parts[0] + "." + parts[1]
		}
		return v
	}
	return ""
}

func scProbe(dep Deployment) scCfgProbe {
	var p scCfgProbe
	json.Unmarshal(dep.Config, &p)
	return p
}

// scProductLabel names what is actually running, for the picker. The node type alone is not it:
// "ps" is a Percona Server, and the person choosing an endpoint is choosing between a Percona
// Server and a MySQL Community server as much as between two hostnames.
func scProductLabel(nodeType string) string {
	switch nodeType {
	case "ps", "mysql":
		return "Percona Server for MySQL"
	case "pxc":
		return "Percona XtraDB Cluster"
	case "innodb":
		return "Percona Server (Group Replication)"
	case "mysqlce", "mysqlcerepl":
		return "MySQL Community Server"
	case "mysqlceinnodb":
		return "MySQL Community (Group Replication)"
	case "mariadb", "mariadbrepl":
		return "MariaDB"
	case "mariadbgalera":
		return "MariaDB Galera"
	case "pg":
		return "Percona Distribution for PostgreSQL"
	case "patroni":
		return "PostgreSQL (Patroni)"
	case "repmgr":
		return "PostgreSQL (repmgr)"
	case "spock":
		return "PostgreSQL (Spock)"
	case "psm", "psmrs", "psmdb":
		return "Percona Server for MongoDB"
	case "valkey", "valkeycluster":
		return "Valkey"
	case "haproxy":
		return "HAProxy"
	case "pgbouncer":
		return "PgBouncer"
	case "proxysql":
		return "ProxySQL"
	}
	return nodeType
}

// scEngineOf is engineForType widened by the one family it does not cover. Valkey has never been
// a "SQL target", so it is absent from that map; here it is a first-class database.
func scEngineOf(nodeType string) string {
	if e := engineForType(nodeType); e != "" {
		return e
	}
	if nodeType == "valkey" || nodeType == "valkeycluster" {
		return scValkey
	}
	return ""
}

// scDefaultPort is the port an engine listens on when nothing has moved it.
func scDefaultPort(engine string) int {
	switch engine {
	case scPostgres:
		return patroniPGPort
	case scMongoDB:
		return mongoPort
	case scValkey:
		return valkeyPort
	}
	return pxcMySQLPort
}

// scDeriveTLS is what DBCanvas knows about this endpoint's TLS, and it is deliberately not more
// than it knows.
//
// MySQL and PostgreSQL nodes deployed with a generated certificate are signed by the Intranet CA,
// and that CA is in this Linux Client's system trust store — so the generated client can verify
// the chain *and* the hostname against the DNS name the Intranet publishes, which is the only TLS
// configuration worth demonstrating. Without one:
//
//   - MySQL still speaks TLS (the server generates its own self-signed material at
//     initialization), but nothing can verify it, so the honest default is encrypt-without-verify.
//   - PostgreSQL has ssl off unless DBCanvas turned it on, so the default is plaintext.
//   - MongoDB gets a signed certificate when asked, but enabling TLS is an all-members-at-once
//     operator step that DBCanvas deliberately does not take (see mongoApplyCert) — so the default
//     is plaintext even when the material is on the node, and the note says so.
//   - Valkey's TLS is runtime configuration (CONFIG SET tls-*), off on a fresh node.
//
// The picker can override any of this: the user knows what they turned on after deploying, and
// the generated code changes with the choice.
func scDeriveTLS(engine string, generateCert bool, nodeOS string) scTLS {
	ca := scCAPath(nodeOS)
	switch engine {
	case scMySQL:
		if generateCert {
			return scTLS{Mode: scTLSVerify, CA: ca, Why: "this node's certificate is signed by the stack CA, which is already in this Linux Client's trust store"}
		}
		return scTLS{Mode: scTLSRequire, Why: "the server has only the self-signed certificate it generated for itself, so the connection can be encrypted but the server cannot be identified"}
	case scPostgres:
		if generateCert {
			return scTLS{Mode: scTLSVerify, CA: ca, Why: "this node's certificate is signed by the stack CA, which is already in this Linux Client's trust store"}
		}
		return scTLS{Mode: scTLSOff, Why: "PostgreSQL was deployed without a certificate, so it is not listening for TLS"}
	case scMongoDB:
		if generateCert {
			return scTLS{Mode: scTLSOff, Why: "the node carries a stack-CA certificate, but DBCanvas does not turn cluster TLS on for you — switch this to Verify once you have enabled it from the node's TLS tab"}
		}
		return scTLS{Mode: scTLSOff, Why: "no certificate was issued for this deployment"}
	}
	return scTLS{Mode: scTLSOff, Why: "Valkey TLS is runtime configuration and is off on a freshly deployed node"}
}

// scApplyTLSChoice overrides a target's derived TLS with what the user picked, keeping the CA path
// (which is a fact about the node, not a preference) and saying where the choice came from.
func scApplyTLSChoice(t scTarget, mode, nodeOS string) scTarget {
	switch mode {
	case scTLSOff, scTLSRequire, scTLSVerify:
	default:
		return t // "" or nonsense: keep what was derived
	}
	if mode == t.TLS.Mode {
		return t
	}
	t.TLS = scTLS{Mode: mode, Why: "chosen on the Sample Client Code page rather than derived from the deployment"}
	if mode == scTLSVerify {
		t.TLS.CA = scCAPath(nodeOS)
	}
	return t
}

// ------------------------------------------------------------------------------- enumeration

// scStackTargets is every endpoint in one stack, in the order the picker offers them: databases
// first, grouped by the cluster they belong to, then the things in front of them.
func (a *App) scStackTargets(st Stack, clientOS string) []scTarget {
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
		return d, ok && d.State == DeployRunning
	}

	var out []scTarget
	// --- the nodes themselves ------------------------------------------------------------
	for _, n := range doc.Nodes {
		dep, ok := running(n.ID)
		if !ok {
			continue
		}
		if n.Type == "aio" {
			out = append(out, a.scAIOTargets(st, n, dep, domain, clientOS)...)
			continue
		}
		engine := scEngineOf(n.Type)
		if engine == "" {
			continue
		}
		p := scProbe(dep)
		// A MongoDB config server is not something to point an application at, and a shard
		// member is only interesting as a direct connection — which is offered, with a note.
		if engine == scMongoDB && p.Role == "config" {
			continue
		}
		user, pass, ok := a.scCredentials(engine, dep)
		if !ok {
			continue
		}
		host := p.FQDN
		if host == "" {
			host = fqdnOf(hosts[n.ID], domain)
		}
		t := scTarget{
			ID: n.ID, Engine: engine, Kind: n.Type, Product: scProductLabel(n.Type), Major: p.major(),
			Host: host, Port: scDefaultPort(engine), User: user, Password: pass,
			Database: scDemoDatabase, AuthDB: "admin", Role: "member",
			StackID: st.ID, StackName: st.Name, NodeID: n.ID,
			TLS: scDeriveTLS(engine, p.GenerateCert, clientOS),
		}
		if frame := frameByID(doc, n.FrameID); frame.ID != "" {
			t.Label = frame.Label + " · " + n.Label
		} else {
			t.Label = n.Label
			t.Role = "primary"
		}
		switch {
		case engine == scMongoDB && p.Role == "mongos":
			t.Role, t.Note = "router", "a mongos router — the entry point to the sharded cluster"
		case engine == scMongoDB && p.ReplSet != "":
			t.Note = "a direct connection to one replica-set member; the set as a whole is offered separately"
			t.ReplicaSet = p.ReplSet
		case n.Type == "valkeycluster":
			t.Note = "one shard of a Valkey cluster; the cluster as a whole is offered separately"
		case p.Role == "secondary" || n.Role == "replica":
			t.Role, t.Note = "replica", "a replica — writes will be refused here"
		}
		out = append(out, t)
	}

	// --- clusters as a whole, where a driver wants the set and not a member ----------------
	for _, f := range doc.Frames {
		switch f.Type {
		case "psmrs":
			if t, ok := a.scReplicaSetTarget(st, doc, f, hosts, domain, clientOS, running); ok {
				out = append(out, t)
			}
		case "valkeycluster":
			if t, ok := a.scValkeyClusterTarget(st, doc, f, hosts, domain, clientOS, running); ok {
				out = append(out, t)
			}
		case "innodb", "mysqlceinnodb":
			out = append(out, a.scRouterTargets(st, doc, f, hosts, domain, clientOS, running)...)
		}
	}

	// --- what sits in front of them --------------------------------------------------------
	for _, n := range doc.Nodes {
		dep, ok := running(n.ID)
		if !ok {
			continue
		}
		switch n.Type {
		case "haproxy":
			out = append(out, a.scHAProxyTargets(st, doc, n, dep, hosts, domain, running)...)
		case "pgbouncer":
			out = append(out, a.scPgBouncerTargets(st, doc, n, hosts, domain, running)...)
		case "proxysql":
			if t, ok := a.scProxySQLTarget(st, doc, n, hosts, domain, clientOS, running); ok {
				out = append(out, t)
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// scCredentials is the account a sample connects as, per engine.
//
// MySQL uses the application account rather than root or admin: root@localhost cannot connect over
// TCP at all, and an application connecting as a superuser is the thing a sample should not be
// teaching. The app account is created on every MySQL-family node with privileges enough to create
// its own database, which is exactly what an application deploying its own schema needs.
//
// PostgreSQL has no such account on a DBCanvas node — the superuser is the only role provisioned —
// so that is what is used, and the generated code says so in a comment rather than pretending.
func (a *App) scCredentials(engine string, dep Deployment) (user, pass string, ok bool) {
	switch engine {
	case scMySQL:
		var s pxcSecrets
		json.Unmarshal(dep.Secrets, &s)
		user, pass = s.AppUser, s.AppPassword
		if user == "" {
			user, pass = s.AdminUser, s.AdminPassword
		}
		if user == "" {
			return "", "", false
		}
		return user, pass, true
	case scPostgres:
		var s pgSecrets
		json.Unmarshal(dep.Secrets, &s)
		if s.SuperPassword == "" {
			s = pgFamilySecrets()
		}
		return s.Super(), s.SuperPassword, true
	case scMongoDB:
		var s mongoSecrets
		json.Unmarshal(dep.Secrets, &s)
		if s.AdminUser == "" {
			s.AdminUser = "admin"
		}
		if s.AdminPassword == "" {
			return "", "", false
		}
		return s.AdminUser, s.AdminPassword, true
	case scValkey:
		return "default", valkeyPasswordFor(dep), true
	}
	return "", "", false
}

// scFrameMembers is the running member nodes of a frame, in design order.
func scFrameMembers(doc designDoc, frameID string, running func(string) (Deployment, bool)) []designNode {
	var out []designNode
	for _, n := range doc.Nodes {
		if n.FrameID != frameID {
			continue
		}
		if _, ok := running(n.ID); ok {
			out = append(out, n)
		}
	}
	return out
}

// scReplicaSetTarget is a MongoDB replica set as one endpoint: every member, plus the set name,
// which is what a driver needs to do its own primary discovery and failover. This is the endpoint
// an application should use; the individual members are offered too, for the times when connecting
// to one on purpose is the experiment.
func (a *App) scReplicaSetTarget(st Stack, doc designDoc, f designFrame, hosts map[string]string, domain, clientOS string, running func(string) (Deployment, bool)) (scTarget, bool) {
	members := scFrameMembers(doc, f.ID, running)
	if len(members) == 0 {
		return scTarget{}, false
	}
	dep, _ := running(members[0].ID)
	p := scProbe(dep)
	user, pass, ok := a.scCredentials(scMongoDB, dep)
	if !ok {
		return scTarget{}, false
	}
	var addrs []string
	for _, m := range members {
		addrs = append(addrs, fqdnOf(hosts[m.ID], domain))
	}
	return scTarget{
		ID: f.ID + "#frame", Label: f.Label + " (replica set)", Engine: scMongoDB, Kind: "psmrs",
		Product: scProductLabel("psmrs"), Major: p.major(),
		Host: addrs[0], Port: mongoPort, Hosts: addrs, ReplicaSet: p.ReplSet,
		User: user, Password: pass, Database: scDemoDatabase, AuthDB: "admin", Role: "primary",
		Note:    "the whole set — the driver finds the primary and follows it through a failover",
		StackID: st.ID, StackName: st.Name, NodeID: members[0].ID,
		TLS: scDeriveTLS(scMongoDB, p.GenerateCert, clientOS),
	}, true
}

// scValkeyClusterTarget is a Valkey cluster as one endpoint: every shard's address, which is what
// a cluster-aware client seeds itself from.
func (a *App) scValkeyClusterTarget(st Stack, doc designDoc, f designFrame, hosts map[string]string, domain, clientOS string, running func(string) (Deployment, bool)) (scTarget, bool) {
	members := scFrameMembers(doc, f.ID, running)
	if len(members) == 0 {
		return scTarget{}, false
	}
	dep, _ := running(members[0].ID)
	var addrs []string
	for _, m := range members {
		addrs = append(addrs, fqdnOf(hosts[m.ID], domain))
	}
	return scTarget{
		ID: f.ID + "#frame", Label: f.Label + " (cluster)", Engine: scValkey, Kind: "valkeycluster",
		Product: scProductLabel("valkeycluster"), Major: scProbe(dep).major(),
		Host: addrs[0], Port: valkeyPort, Hosts: addrs,
		User: "default", Password: valkeyPasswordFor(dep), Database: scDemoDatabase, Role: "primary",
		Note:    "every shard — the client discovers the slot map and routes each key itself",
		StackID: st.ID, StackName: st.Name, NodeID: members[0].ID,
		TLS: scDeriveTLS(scValkey, false, clientOS),
	}, true
}

// scRouterTargets are the MySQL Router ports a Group Replication cluster installs on every member.
// 6446 lands on the primary wherever it currently is — which is what an application wants and what
// makes a failover invisible — and 6447 on a secondary. Offered on the first running member,
// because the router on each member answers identically.
func (a *App) scRouterTargets(st Stack, doc designDoc, f designFrame, hosts map[string]string, domain, clientOS string, running func(string) (Deployment, bool)) []scTarget {
	members := scFrameMembers(doc, f.ID, running)
	if len(members) == 0 {
		return nil
	}
	dep, _ := running(members[0].ID)
	user, pass, ok := a.scCredentials(scMySQL, dep)
	if !ok {
		return nil
	}
	p := scProbe(dep)
	host := fqdnOf(hosts[members[0].ID], domain)
	base := scTarget{
		Engine: scMySQL, Product: scProductLabel(f.Type), Major: p.major(),
		Host: host, User: user, Password: pass, Database: scDemoDatabase,
		StackID: st.ID, StackName: st.Name, NodeID: members[0].ID,
		TLS: scDeriveTLS(scMySQL, p.GenerateCert, clientOS),
	}
	rw, ro := base, base
	rw.ID, rw.Kind, rw.Port, rw.Role = f.ID+"#router-rw", "router-rw", routerRWPort, "router"
	rw.Label = f.Label + " · MySQL Router (read/write)"
	rw.Note = "routed to whichever member is primary right now"
	ro.ID, ro.Kind, ro.Port, ro.Role = f.ID+"#router-ro", "router-ro", routerROPort, "replica"
	ro.Label = f.Label + " · MySQL Router (read-only)"
	ro.Note = "routed to a secondary — writes will be refused"
	return []scTarget{rw, ro}
}

// scHAProxyTargets are the two ports an HAProxy in front of a cluster publishes. They are separate
// endpoints because they mean different things, and generating a sample against the read port is
// the shortest way to see a write fail for a reason that is not a bug.
func (a *App) scHAProxyTargets(st Stack, doc designDoc, n designNode, dep Deployment, hosts map[string]string, domain string, running func(string) (Deployment, bool)) []scTarget {
	back, backKind, ok := haproxyBackend(doc, n.ID)
	if !ok {
		return nil
	}
	engine := scEngineOf(backKind)
	if engine == "" {
		return nil
	}
	members := scFrameMembers(doc, back.ID, running)
	if len(members) == 0 {
		return nil
	}
	mdep, _ := running(members[0].ID)
	user, pass, ok := a.scCredentials(engine, mdep)
	if !ok {
		return nil
	}
	p := scProbe(mdep)
	host := fqdnOf(hosts[n.ID], domain)
	base := scTarget{
		Engine: engine, Product: "HAProxy → " + scProductLabel(backKind), Major: p.major(),
		Host: host, User: user, Password: pass, Database: scDemoDatabase,
		StackID: st.ID, StackName: st.Name, NodeID: members[0].ID,
		// The certificate is the backend's, and HAProxy in DBCanvas passes TCP through
		// rather than terminating TLS — but it does so under its own hostname, so a client
		// verifying the name would be checking the wrong one.
		TLS: scTLS{Mode: scTLSOff, Why: "HAProxy passes the connection through under its own name, so the backend's certificate would not match it"},
	}
	w, r := base, base
	w.ID, w.Kind, w.Port, w.Role = n.ID+"@write", "haproxy-write", haproxyWritePort, "primary"
	w.Label = n.Label + " · write port"
	w.Note = "balanced onto the member that can take writes"
	r.ID, r.Kind, r.Port, r.Role = n.ID+"@read", "haproxy-read", haproxyReadPort, "replica"
	r.Label = n.Label + " · read port"
	r.Note = "balanced across the replicas — writes will be refused"
	return []scTarget{w, r}
}

// scPgBouncerTargets are the pools a PgBouncer node publishes. All of them are the same host and
// the same port — what differs is the database name a client asks for, which is exactly how
// PgBouncer routing works and is the thing a generated sample makes concrete:
//
//	the wildcard pool — any database name, always the member that can take writes
//	"<db>_ro"         — a standby, on a Patroni or repmgr backend. Writes fail here, on purpose.
//	"<db>_<member>"   — one per Spock member, because every one of them is a writer
//
// The credentials are the backend's own superuser, resolved the same way the provisioner resolves
// them; the pool authenticates the client itself and then reuses its own server connections.
func (a *App) scPgBouncerTargets(st Stack, doc designDoc, n designNode, hosts map[string]string, domain string, running func(string) (Deployment, bool)) []scTarget {
	kind, frame, backNode, ok := pgBouncerBackend(doc, n.ID)
	if !ok {
		return nil
	}
	// One running backend deployment is what the credentials come from: a frame's first
	// running member, or the standalone node itself.
	var mdep Deployment
	var srcNodeID string
	if kind == "pg" {
		d, up := running(backNode.ID)
		if !up {
			return nil
		}
		mdep, srcNodeID = d, backNode.ID
	} else {
		members := scFrameMembers(doc, frame.ID, running)
		if len(members) == 0 {
			return nil
		}
		mdep, _ = running(members[0].ID) // scFrameMembers only returns running members
		srcNodeID = members[0].ID
	}
	user, pass, ok := a.scCredentials(scPostgres, mdep)
	if !ok {
		return nil
	}
	opt := pgBouncerDefaults(n, kind)
	p := scProbe(mdep)
	base := scTarget{
		Engine: scPostgres, Product: "PgBouncer → " + scProductLabel(kind), Major: p.major(),
		Host: fqdnOf(hosts[n.ID], domain), Port: pgBouncerPort, User: user, Password: pass,
		StackID: st.ID, StackName: st.Name, NodeID: srcNodeID,
		// Unlike HAProxy, PgBouncer *terminates* the connection, so when the node was
		// deployed with a certificate it is the pool's own name on it and verifying is
		// the right thing to do.
		TLS: scDeriveTLS(scPostgres, n.GenerateCert, n.OS),
	}
	if !n.GenerateCert {
		base.TLS = scTLS{Mode: scTLSOff, Why: "this PgBouncer terminates the connection and was deployed without a certificate, so it is not listening for TLS"}
	}
	out := []scTarget{}
	w := base
	w.ID, w.Kind, w.Role, w.Database = n.ID+"@pool", "pgbouncer-rw", "primary", scDemoDatabase
	w.Label = n.Label + " · pool"
	w.Note = "the wildcard pool — any database name, pooled onto the member that can take writes"
	out = append(out, w)
	if opt.PgbRouting == "rw-ro" {
		r := base
		r.ID, r.Kind, r.Role, r.Database = n.ID+"@pool-ro", "pgbouncer-ro", "replica", opt.PgbDatabase+"_ro"
		r.Label = n.Label + " · read-only pool"
		r.Note = "the " + r.Database + " pool — a standby, so writes will be refused"
		out = append(out, r)
	}
	return out
}

// scProxySQLTarget is ProxySQL's client port. The read/write split happens inside it, so unlike
// HAProxy it is one endpoint — and the credentials are the backend cluster's own, resolved the
// same way the provisioner resolves them (backendFrameForProxySQL walks the association graph, so
// a ProxySQL chained behind another ProxySQL still finds the cluster).
func (a *App) scProxySQLTarget(st Stack, doc designDoc, n designNode, hosts map[string]string, domain, clientOS string, running func(string) (Deployment, bool)) (scTarget, bool) {
	back, backKind, ok := backendFrameForProxySQL(doc, n.ID)
	if !ok {
		return scTarget{}, false
	}
	members := scFrameMembers(doc, back.ID, running)
	if len(members) == 0 {
		return scTarget{}, false
	}
	mdep, _ := running(members[0].ID)
	user, pass, ok := a.scCredentials(scMySQL, mdep)
	if !ok {
		return scTarget{}, false
	}
	p := scProbe(mdep)
	return scTarget{
		ID: n.ID, Label: n.Label + " · client port", Engine: scMySQL, Kind: "proxysql",
		Product: "ProxySQL → " + scProductLabel(backKind), Major: p.major(),
		Host: fqdnOf(hosts[n.ID], domain), Port: proxysqlMySQLPort,
		User: user, Password: pass, Database: scDemoDatabase, Role: "proxy",
		Note:    "ProxySQL splits reads from writes itself, so this one port takes both",
		StackID: st.ID, StackName: st.Name, NodeID: mdep.NodeID,
		TLS: scTLS{Mode: scTLSOff, Why: "ProxySQL terminates the client connection under its own name, so the backend's certificate would not match it"},
	}, true
}

// scAIOTargets are the database instances inside one All-in-One node. Each is a separate endpoint
// with its own engine, port and credentials — which is exactly what the Query Runner and the
// Benchmark already treat them as, through the same three helpers.
func (a *App) scAIOTargets(st Stack, n designNode, dep Deployment, domain, clientOS string) []scTarget {
	var out []scTarget
	for _, m := range aioTargetableInstances(dep) {
		engine, port, user, pass := aioInstanceCreds(dep, m)
		if engine == "" {
			continue
		}
		out = append(out, scTarget{
			ID: aioJoinTarget(n.ID, m.Inst), Label: aioTargetLabel(n.Label, m),
			Engine: engine, Kind: m.Kind, Product: scProductLabel(m.Kind),
			Host: fqdnOf(m.Inst, domain), Port: port,
			User: user, Password: pass, Database: scDemoDatabase, AuthDB: "admin",
			Role: "instance", Note: "one instance inside an All-in-One node",
			StackID: st.ID, StackName: st.Name, NodeID: n.ID,
			TLS: scDeriveTLS(engine, false, clientOS),
		})
	}
	return out
}

// scFindTarget resolves one endpoint id within a stack, with the sentence a user should read when
// it is gone — a target list is a snapshot, and a node can be stopped between choosing one and
// pressing Run.
func (a *App) scFindTarget(st Stack, id, clientOS string) (scTarget, error) {
	for _, t := range a.scStackTargets(st, clientOS) {
		if t.ID == id {
			return t, nil
		}
	}
	return scTarget{}, fmt.Errorf("endpoint %q is not in this stack any more — it may have been stopped or removed", id)
}

// scClientCertNames lists the client certificates the stack's Intranet has issued, by username.
// They are what a mutual-TLS sample presents, and DBCanvas copies the chosen one onto the Linux
// Client when the project is saved (see scCopyClientCert). Best-effort: a stack whose Intranet is
// not running simply has none to offer.
func (a *App) scClientCertNames(ctx context.Context, st Stack) []string {
	id := a.intranetContainerID(ctx, st)
	if id == "" {
		return nil
	}
	out, err := a.execScript(ctx, id, dbCertListScript, nil)
	if err != nil {
		return nil
	}
	var names []string
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if ln = strings.TrimSpace(ln); ln == "" {
			continue
		}
		names = append(names, strings.SplitN(ln, "\t", 2)[0])
	}
	sort.Strings(names)
	return names
}

// scReadClientCert fetches one issued certificate and its key off the Intranet, for copying into
// the project directory. The key never reaches the browser: it goes from the Intranet to the Linux
// Client through the app, and the generated code refers to it by path.
func (a *App) scReadClientCert(ctx context.Context, st Stack, user string) (cert, key []byte, err error) {
	if !validCertUser(user) {
		return nil, nil, fmt.Errorf("invalid certificate username")
	}
	id := a.intranetContainerID(ctx, st)
	if id == "" {
		return nil, nil, fmt.Errorf("this stack has no running Intranet, so its certificate authority cannot be reached")
	}
	cert, err = a.readContainerFile(ctx, id, dbCertDir+"/"+user+".crt")
	if err != nil {
		return nil, nil, fmt.Errorf("no client certificate for %q — issue one from the Intranet node's Certificates tab", user)
	}
	key, err = a.readContainerFile(ctx, id, dbCertDir+"/"+user+".key")
	if err != nil {
		return nil, nil, fmt.Errorf("no private key for %q on the Intranet", user)
	}
	return cert, key, nil
}
