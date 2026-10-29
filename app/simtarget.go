package main

// simtarget.go resolves the database a simulator node is linked to: a standalone
// node, a cluster frame, a proxy or pool in front of one, or an instance on an
// All-in-One node, to an address, credentials and the engine speaking there.
// Performance Sim, Ledger Sim, the Support Sims and compose share it; the
// Kubernetes frame is resolved in simtarget_k3d.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// simSecrets holds the credentials a manual-mode node was configured with,
// kept out of stockSimConfig so the node properties panel never renders them.
type simSecrets struct {
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	DSN      string `json:"dsn,omitempty"`
}

// simEngineForKind maps a linked canvas target's type to the engine the
// sim will actually speak to it. In linked mode this — not the node's own
// SSEngine field, which only manual mode fills in — is what the sim ends up
// running, so it decides both the driver and whether a size target is possible.
//
// The two routers are absent on purpose: HAProxy fronts MySQL-family *or*
// PostgreSQL clusters and ProxySQL only MySQL-family ones, so their engine is a
// property of the backend behind them. So is a K3D frame's — it is whichever of
// the six operators the frame runs. simEngineForTarget resolves all three.
func simEngineForKind(kind string) string {
	switch kind {
	case "ps", "mariadb", "mysqlce",
		"pxc", "mysql", "innodb", "mariadbrepl", "mariadbgalera", "mysqlcerepl", "mysqlceinnodb":
		return "mysql"
	case "pg", "patroni", "repmgr", "spock":
		return "postgres"
	case "psm", "psmrs", "psmdb":
		return "mongodb"
	case "valkey", "valkeycluster":
		return "valkey"
	}
	return ""
}

// simEngineForTarget is simEngineForKind with the router and
// Kubernetes cases resolved, which needs the design to see what the router
// fronts and which operator the frame runs. Returns "" if the target is a
// router with no single identifiable backend, or a K3D frame with no operator —
// design errors the validator reports in its own words rather than guessing an
// engine for.
func simEngineForTarget(doc designDoc, kind, targetID string) string {
	switch kind {
	case "haproxy":
		if _, backKind, ok := haproxyBackend(doc, targetID); ok {
			return simEngineForKind(backKind)
		}
		return ""
	case "pgbouncer":
		// PgBouncer speaks the PostgreSQL protocol and fronts nothing else — but it
		// still has to be linked to a backend, so an unlinked pool has no engine and
		// the validator says so rather than the sim failing at deploy.
		if _, _, _, ok := pgBouncerBackend(doc, targetID); ok {
			return "postgres"
		}
		return ""
	case "proxysql":
		// ProxySQL is a MySQL-protocol proxy and fronts nothing else.
		return "mysql"
	case "k3d":
		// A Kubernetes frame's engine is the one its operator manages: MySQL
		// under PXC or PS, MongoDB under PSMDB, PostgreSQL under any of the
		// three PostgreSQL operators. A frame with no operator has no database
		// in it at all, and returns "" for the validator to report.
		return k3dOperatorEngine(frameByID(doc, targetID).K3DOperator)
	}
	return simEngineForKind(kind)
}

// engineDisplayLabel mirrors engineDisplayName in the sim image.
func engineDisplayLabel(engine string) string {
	switch engine {
	case "mysql":
		return "MySQL"
	case "postgres":
		return "PostgreSQL"
	case "mongodb":
		return "MongoDB"
	case "valkey":
		return "Valkey"
	}
	return engine
}

// simStandaloneTargets are the standalone database node types a Stock
// Market Sim node can be linked to. The key is the node type, which is also the
// coarse kind, because for a standalone node the two coincide.
var simStandaloneTargets = map[string]bool{
	"ps": true, "mariadb": true, "mysqlce": true, // MySQL family
	"pg":     true, // PostgreSQL
	"psm":    true, // MongoDB
	"valkey": true, // Valkey
}

// simFrameTargets are the cluster frame types it can be linked to. Every
// one resolves to a single write endpoint in waitSimTarget — the primary,
// the leader, the router or the mongos, depending on what the cluster is.
var simFrameTargets = map[string]bool{
	// MySQL family.
	"pxc": true, "mysql": true, "innodb": true,
	"mariadbrepl": true, "mariadbgalera": true,
	"mysqlcerepl": true, "mysqlceinnodb": true,
	// PostgreSQL.
	"patroni": true, "repmgr": true, "spock": true,
	// Kubernetes. A K3D frame is any of the three engines above, depending on
	// which of the six operators it runs — see simtarget_k3d.go.
	"k3d": true,
	// MongoDB.
	"psmrs": true, "psmdb": true,
	// Valkey.
	"valkeycluster": true,
}

// simLinkedTarget resolves the coarse kind and target id a simulator node is
// linked to via a drawn edge: an undirected-edge walk over a wide set, covering
// every database node, cluster frame and router dbcanvas can deploy. A sim tied
// to one engine checks the kind it gets. The kind returned is the node or frame
// type itself, except for the two routers, which are named for what they are
// rather than what they front — waitSimTarget works that out from the backend
// they are attached to.
//
// A simulator node drives exactly one database; the singleOutgoing rule on the
// canvas is what enforces that.
func simLinkedTarget(doc designDoc, startID string) (kind, targetID string, ok bool) {
	for _, e := range doc.Edges {
		var other string
		switch startID {
		case e.From.Node:
			other = e.To.Node
		case e.To.Node:
			other = e.From.Node
		default:
			continue
		}
		for _, n := range doc.Nodes {
			if n.ID != other || n.FrameID != "" {
				continue
			}
			switch {
			case simStandaloneTargets[n.Type]:
				return n.Type, n.ID, true
			case n.Type == "haproxy", n.Type == "proxysql", n.Type == "pgbouncer":
				return n.Type, n.ID, true
			}
		}
		for _, f := range doc.Frames {
			if f.ID == other && simFrameTargets[f.Type] {
				return f.Type, f.ID, true
			}
		}
	}
	return "", "", false
}

// simSQLEnv builds the sim's connection environment for a SQL engine: the write DSN,
// and — when the target publishes a read endpoint this node asked to use — a read DSN
// beside it. One function for both engines, because they differ only in DSN dialect while
// the rule about when a second endpoint exists is identical, and because the pair is what
// the test can then check without a running stack.
//
// readPort 0 means no split: exactly the single-DSN environment every target produced
// before the option existed, which is what keeps an untouched node's deploy unchanged.
// extra is appended to the PostgreSQL DSN's query string verbatim ("" for none). It
// exists for one endpoint: a PgBouncer pool, where pgx must stop preparing statements
// it will not find again on the next transaction — see pgBouncerDSNExtra.
func simSQLEnv(engine, user, pass, host string, port, readPort int, extra string, failover ...string) []string {
	dsn := func(p int) string {
		if engine == "mysql" {
			return fmt.Sprintf("%s:%s@tcp(%s:%d)/?tls=false", user, pass, host, p)
		}
		// Every member, not just the one that is primary today. libpq's URI format takes a
		// comma-separated host list and `target_session_attrs=read-write` makes the client try
		// them in turn until one accepts a read-write session — which is the primary, whichever
		// member that is now. Without it the DSN names the member that happened to be primary
		// at deploy, and a switchover leaves the sim pointed at a read-only standby: writes fail
		// with SQLSTATE 25006 while reads keep working, so nothing looks broken.
		//
		// Built by hand rather than through url.URL, which percent-escapes the commas in a
		// multi-host authority and produces a DSN libpq reads as one absurd hostname.
		hostList := fmt.Sprintf("%s:%d", host, p)
		query := "sslmode=prefer&connect_timeout=10"
		if len(failover) > 0 {
			var b strings.Builder
			for i, h := range failover {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, "%s:%d", h, p)
			}
			hostList = b.String()
			// Only with somewhere to fail over to: on a single-host DSN this would merely add a
			// round trip, and on a standalone node it would refuse to connect at all.
			query += "&target_session_attrs=read-write"
		}
		if extra != "" {
			query += "&" + extra
		}
		return "postgres://" + url.UserPassword(user, pass).String() + "@" + hostList + "/postgres?" + query
	}
	prefix := "POSTGRES"
	if engine == "mysql" {
		prefix = "MYSQL"
	}
	env := []string{"DB_ENGINE=" + engine, prefix + "_DSN=" + dsn(port)}
	if readPort != 0 {
		env = append(env, prefix+"_RO_DSN="+dsn(readPort))
	}
	return env
}

// simResolved is what a linked target resolves to.
type simResolved struct {
	env         []string
	secrets     simSecrets
	engine      string
	kind        string
	displayName string
	host        string
	port        int
	// readPort is the endpoint the sim's display-only reads go to, 0 when every
	// statement goes to port. Only an HAProxy target with the split turned on sets
	// it: it is the one target that publishes a second port meaning "a replica".
	readPort int
	// noPrepare says this endpoint does not keep a client on one server connection
	// between transactions, so server-side prepared statements will not be there on
	// the next one. Only a PgBouncer pool in transaction or statement mode sets it.
	// Every client that goes through this resolver has to act on it in its own
	// dialect — pgx wants default_query_exec_mode=simple_protocol, pgjdbc wants
	// prepareThreshold=0 — which is why it is a fact here rather than a string.
	noPrepare bool
}

// waitSimTarget resolves a linked canvas target down to a connectable
// endpoint and credentials, blocking until it is actually running.
//
// It dispatches mostly on the *engine* rather than on the kind, because everything
// after the endpoint has been found — the DSN dialect, the driver, the
// credentials that exist — is a property of the engine, and everything before
// it is a property of the kind. The four family resolvers below each do the
// second half for one engine.
func (a *App) waitSimTarget(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, coarseKind, targetID string, timeout time.Duration, splitReads bool, logln func(string)) (simResolved, error) {
	// A K3D frame is dispatched on the kind rather than the engine: all three
	// engines share one resolver there, because what has to be found first — the
	// operator's own Services, and the Secret it keeps its passwords in — is the
	// same work whichever database came out the other end. See simtarget_k3d.go.
	//
	// It is also the only target that gets logln. Every other one is up in
	// under a minute; an operator building a database cluster inside Kubernetes
	// is minutes, and a deploy log that says nothing for that long reads as a
	// hang rather than as waiting.
	if coarseKind == "k3d" {
		return a.simK3DTarget(ctx, st, doc, targetID, timeout, logln)
	}
	switch simEngineForTarget(doc, coarseKind, targetID) {
	case "mysql":
		return a.simMySQLTarget(ctx, st, hosts, doc, domain, coarseKind, targetID, timeout, splitReads)
	case "postgres":
		return a.simPostgresTarget(ctx, st, hosts, doc, domain, coarseKind, targetID, timeout, splitReads)
	case "mongodb":
		return a.simMongoTarget(ctx, st, hosts, doc, domain, coarseKind, targetID, timeout)
	case "valkey":
		return a.simValkeyTarget(ctx, st, hosts, doc, domain, coarseKind, targetID, timeout)
	}
	if coarseKind == "haproxy" {
		return simResolved{}, fmt.Errorf(
			"the linked HAProxy node does not front exactly one database cluster, so there is nothing to connect to — link the cluster frame directly instead")
	}
	if coarseKind == "pgbouncer" {
		return simResolved{}, fmt.Errorf(
			"the linked PgBouncer node does not pool for exactly one PostgreSQL backend, so there is nothing to connect to — link the node or cluster to it, or link this sim to the backend directly")
	}
	return simResolved{}, fmt.Errorf("unresolved simulator target %q", coarseKind)
}

// simMySQLTarget resolves every MySQL-family shape to one write endpoint.
func (a *App) simMySQLTarget(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, kind, targetID string, timeout time.Duration, splitReads bool) (simResolved, error) {
	var (
		h    string
		port = pxcMySQLPort
		// readPort is set only by the HAProxy branch below: it is the only target with a
		// second port that means "a replica". ProxySQL splits reads inside itself, and
		// every other shape here is one endpoint.
		readPort int
		s        pxcSecrets
		name     = nodeLabel(doc, targetID)
		err      error
	)
	frame := frameByID(doc, targetID)
	if frame.ID != "" {
		name = frame.Label
	}

	switch kind {
	case "ps", "mariadb", "mysqlce":
		h, s, err = a.waitMySQLFamilyNode(ctx, st.ID, targetID, hosts, domain, kind, timeout)

	case "pxc":
		h, s, err = a.waitPXCRunning(ctx, st.ID, frame, doc, domain, timeout)

	case "mysql":
		h, _, s, err = a.waitMySQLRunning(ctx, st.ID, frame, doc, domain, timeout)

	case "mariadbrepl", "mysqlcerepl":
		// Asynchronous replication: only the primary takes writes.
		h, s, err = a.waitMySQLFamilyFrame(ctx, st.ID, frame, doc, domain, true, timeout)

	case "mariadbgalera":
		// Galera is multi-master — every member is a write endpoint.
		h, s, err = a.waitMySQLFamilyFrame(ctx, st.ID, frame, doc, domain, false, timeout)

	case "innodb", "mysqlceinnodb":
		// With MySQL Router on (the frame's mysqlRouter option), every member runs
		// one, and connecting to any member's read/write router port lands on the
		// current primary wherever it is, which is what an application wants and
		// what makes a failover invisible to the sim. Without it nothing listens on
		// that port, and the cluster has no endpoint of its own (see innodb.go), so
		// the sim is given a member: the first in label order, which is the one the
		// group was bootstrapped on and so its primary until a failover.
		h, s, err = a.waitMySQLFamilyFrame(ctx, st.ID, frame, doc, domain, false, timeout)
		if frame.MySQLRouter {
			port = routerRWPort
		}

	case "haproxy":
		back, backKind, ok := haproxyBackend(doc, targetID)
		if !ok {
			return simResolved{}, fmt.Errorf("the linked HAProxy node does not front exactly one cluster")
		}
		if !a.waitNodeRunning(st.ID, targetID, timeout) {
			return simResolved{}, fmt.Errorf("linked HAProxy node did not become ready within %s", timeout)
		}
		switch backKind {
		case "pxc":
			_, s, err = a.waitPXCRunning(ctx, st.ID, back, doc, domain, timeout)
		case "mysql":
			_, _, s, err = a.waitMySQLRunning(ctx, st.ID, back, doc, domain, timeout)
		default:
			_, s, err = a.waitMySQLFamilyFrame(ctx, st.ID, back, doc, domain, false, timeout)
		}
		h, port, kind = fqdnOf(hosts[targetID], domain), haproxyWritePort, "haproxy-"+backKind
		if splitReads {
			readPort = haproxyReadPort
		}

	case "proxysql":
		var backKind string
		h, backKind, s, name, err = a.waitProxySQLRunning(ctx, st.ID, doc, hosts, domain, targetID, timeout)
		port, kind = proxysqlMySQLPort, "proxysql-"+backKind

	default:
		return simResolved{}, fmt.Errorf("unresolved MySQL-family target %q", kind)
	}
	if err != nil {
		return simResolved{}, err
	}

	// Connect as the application user, never root: root@localhost cannot
	// connect over TCP, which is what every sibling sim learned the hard way.
	return simResolved{
		env:     simSQLEnv("mysql", s.AppUser, s.AppPassword, h, port, readPort, ""),
		secrets: simSecrets{User: s.AppUser, Password: s.AppPassword},
		engine:  "mysql", kind: kind, displayName: name,
		host: h, port: port, readPort: readPort,
	}, nil
}

// simPostgresTarget resolves every PostgreSQL shape to one write endpoint.
func (a *App) simPostgresTarget(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, kind, targetID string, timeout time.Duration, splitReads bool) (simResolved, error) {
	var (
		h    string
		port = patroniPGPort
		// See the same field in simMySQLTarget: only an HAProxy target has a second
		// port, and only when the node asked for the split.
		readPort int
		s        pgSecrets
		name     = nodeLabel(doc, targetID)
		err      error
		// failover is every member of a clustered target, for the multi-host DSN. Empty for a
		// standalone node and for either proxy — the proxy already is the indirection.
		failover []string
		// dsnExtra is connection-string parameters the endpoint itself demands, and
		// noPrepare the fact behind it. Only a PgBouncer pool sets either.
		dsnExtra  string
		noPrepare bool
	)
	frame := frameByID(doc, targetID)
	if frame.ID != "" {
		name = frame.Label
	}

	switch kind {
	case "pg":
		h, s, err = a.waitPgNodeRunning(ctx, st.ID, targetID, hosts, domain, timeout)

	// Patroni and repmgr both move their primary — by failover, or because somebody ran a
	// switchover, which is the whole point of having the cluster. So every member goes into the
	// DSN and the driver picks the one accepting writes; `h` stays the current primary because
	// it is what the panel reports and what the read-split uses.
	case "patroni":
		var fqdns []string
		fqdns, s, err = a.waitPatroniRunning(ctx, st.ID, frame, doc, domain, timeout)
		if err == nil {
			h = a.leaderOrFirst(ctx, st, frame, doc, hosts, domain, fqdns, a.patroniLeaderContainer(ctx, st, frame, doc))
			failover = fqdns
		}

	case "repmgr":
		var fqdns []string
		fqdns, s, err = a.waitRepmgrRunning(ctx, st.ID, frame, doc, domain, timeout)
		if err == nil {
			h = a.leaderOrFirst(ctx, st, frame, doc, hosts, domain, fqdns, a.repmgrPrimaryContainer(ctx, st, frame, doc))
			failover = fqdns
		}

	case "spock":
		var fqdns []string
		fqdns, s, err = a.waitSpockRunning(ctx, st.ID, frame, doc, domain, timeout)
		if err == nil {
			if len(fqdns) == 0 {
				return simResolved{}, fmt.Errorf("associated Spock cluster %s has no running member", frame.Label)
			}
			h = fqdns[0] // symmetric peers — any member is a valid write target
		}

	case "haproxy":
		back, backKind, ok := haproxyBackend(doc, targetID)
		if !ok {
			return simResolved{}, fmt.Errorf("the linked HAProxy node does not front exactly one cluster")
		}
		if !a.waitNodeRunning(st.ID, targetID, timeout) {
			return simResolved{}, fmt.Errorf("linked HAProxy node did not become ready within %s", timeout)
		}
		switch backKind {
		case "patroni":
			_, s, err = a.waitPatroniRunning(ctx, st.ID, back, doc, domain, timeout)
		case "repmgr":
			_, s, err = a.waitRepmgrRunning(ctx, st.ID, back, doc, domain, timeout)
		default:
			_, s, err = a.waitSpockRunning(ctx, st.ID, back, doc, domain, timeout)
		}
		h, port, kind = fqdnOf(hosts[targetID], domain), haproxyWritePort, "haproxy-"+backKind
		if splitReads {
			readPort = haproxyReadPort
		}

	// A PgBouncer pool. No failover list and no read port: the pool is already the
	// indirection, and its read/write split is a database name rather than a second
	// port, so a sim asking for the wildcard pool always lands on the member that can
	// take writes — through a failover, without reconnecting anywhere else.
	case "pgbouncer":
		h, port, s, kind, dsnExtra, name, err = a.pgBouncerSimEndpoint(ctx, st, hosts, doc, domain, targetID, timeout)
		noPrepare = dsnExtra != ""

	default:
		return simResolved{}, fmt.Errorf("unresolved PostgreSQL target %q", kind)
	}
	if err != nil {
		return simResolved{}, err
	}

	// The superuser, because a stack node provisions no other PostgreSQL role
	// and the app needs to create a database or a schema of its own.
	// Same credentials and same database on both endpoints: the sim rewrites the database
	// name onto the read DSN from the one it resolved on the writer (pgOpenRead), because
	// which database it ends up using is decided there.
	return simResolved{
		env:     simSQLEnv("postgres", s.SuperUser, s.SuperPassword, h, port, readPort, dsnExtra, failover...),
		secrets: simSecrets{User: s.SuperUser, Password: s.SuperPassword},
		engine:  "postgres", kind: kind, displayName: name,
		host: h, port: port, readPort: readPort, noPrepare: noPrepare,
	}, nil
}

// simMongoTarget resolves a standalone node, a replica set or a sharded
// cluster. The last two use waitMongoTarget outright — it already returns
// exactly the URI this app needs, including waiting for a replica-set majority
// and picking a running mongos.
func (a *App) simMongoTarget(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, kind, targetID string, timeout time.Duration) (simResolved, error) {
	if kind == "psm" {
		dep, werr := a.waitSimNode(ctx, st.ID, targetID, timeout, "MongoDB")
		if werr != nil {
			return simResolved{}, werr
		}
		user, pass := mongoAdminCreds(dep)
		h := fqdnOf(hosts[targetID], domain)
		uri := fmt.Sprintf("mongodb://%s:%s@%s:%d/?authSource=admin&directConnection=true",
			url.QueryEscape(user), url.QueryEscape(pass), h, mongoPort)
		return simResolved{
			env:     []string{"DB_ENGINE=mongodb", "MONGO_URI=" + uri},
			secrets: simSecrets{User: user, Password: pass},
			engine:  "mongodb", kind: "psm", displayName: nodeLabel(doc, targetID),
			host: h, port: mongoPort,
		}, nil
	}

	frame := frameByID(doc, targetID)
	// waitMongoTarget addresses members by bare hostname; this app's container resolves
	// those through the same Intranet DNS, so the URI is usable as returned.
	uri, werr := a.waitMongoTarget(ctx, st.ID, hosts, "", frame, kind, doc, timeout)
	if werr != nil {
		return simResolved{}, werr
	}
	user, pass := "", ""
	if u, uerr := url.Parse(uri); uerr == nil && u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return simResolved{
		env:     []string{"DB_ENGINE=mongodb", "MONGO_URI=" + uri},
		secrets: simSecrets{User: user, Password: pass},
		engine:  "mongodb", kind: kind, displayName: frame.Label,
		host: frame.Label, port: mongoPort,
	}, nil
}

// simValkeyTarget resolves a standalone node or a cluster frame. The
// cluster case uses waitValkeyTarget, and the sim's own store hands the
// address list to a UniversalClient, which picks cluster mode from the count —
// so nothing downstream needs to know which of the two this was.
func (a *App) simValkeyTarget(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, kind, targetID string, timeout time.Duration) (simResolved, error) {
	if kind == "valkey" {
		dep, werr := a.waitSimNode(ctx, st.ID, targetID, timeout, "Valkey")
		if werr != nil {
			return simResolved{}, werr
		}
		pw := valkeyPasswordFor(dep)
		h := fqdnOf(hosts[targetID], domain)
		return simResolved{
			env: []string{
				"DB_ENGINE=valkey",
				fmt.Sprintf("VALKEY_ADDRS=%s:%d", h, valkeyPort),
				"VALKEY_PASSWORD=" + pw,
			},
			secrets: simSecrets{Password: pw},
			engine:  "valkey", kind: "valkey", displayName: nodeLabel(doc, targetID),
			host: h, port: valkeyPort,
		}, nil
	}

	frame := frameByID(doc, targetID)
	addrs, pw, werr := a.waitValkeyTarget(ctx, st.ID, hosts, "", frame, true, timeout)
	if werr != nil {
		return simResolved{}, werr
	}
	if len(addrs) == 0 {
		return simResolved{}, fmt.Errorf("associated Valkey cluster %s has no running member", frame.Label)
	}
	// In Sentinel mode the members are one primary and its replicas, not a
	// cluster: a client given all of them would take them for one. Give it the
	// primary, which is the first member by label until a failover.
	if frame.ValkeyMode == "sentinel" {
		var first designNode
		for _, n := range doc.Nodes {
			if n.FrameID == frame.ID && n.Type == "valkeycluster" && (first.ID == "" || n.Label < first.Label) {
				first = n
			}
		}
		addrs = []string{fmt.Sprintf("%s:%d", hosts[first.ID], valkeyPort)}
	}
	return simResolved{
		env: []string{
			"DB_ENGINE=valkey",
			"VALKEY_ADDRS=" + strings.Join(addrs, ","),
			"VALKEY_PASSWORD=" + pw,
		},
		secrets: simSecrets{Password: pw},
		engine:  "valkey", kind: "valkeycluster", displayName: frame.Label,
		host: addrs[0], port: valkeyPort,
	}, nil
}

// simAIODeclared finds the instance the node's picker names, in the AIO
// node's *design*. Working from the design rather than from a deployment is
// what lets the engine — and so the size target and the validation — be known
// before anything is provisioned.
func simAIODeclared(doc designDoc, aioNode, instance string) (aioInstance, bool) {
	for _, other := range doc.Nodes {
		if other.ID != aioNode || other.Type != "aio" {
			continue
		}
		for _, in := range other.AIOInstances {
			if in.Name == instance {
				return in, true
			}
		}
	}
	return aioInstance{}, false
}

// simAIOMember picks the concrete running member to connect to for a
// declared instance name.
//
// The picker names an instance as the user declared it ("pxc-cluster-01"); a
// deployed cluster instance is several members with generated names
// ("pxc-cluster-01-n2"), tied back by aioInstanceRuntime.Group. A standalone
// instance is one member whose Inst is the declared name and whose Group is
// empty. Where the members are not interchangeable the write endpoint is
// preferred, for the same reason waitMySQLFamilyFrame prefers a primary.
func simAIOMember(dep Deployment, declared string) (aioInstanceRuntime, bool) {
	var fallback aioInstanceRuntime
	found := false
	for _, m := range aioTargetableInstances(dep) {
		if m.Inst != declared && m.Group != declared {
			continue
		}
		switch m.Role {
		case "primary", "bootstrap", "mongos":
			return m, true
		}
		if !found {
			fallback, found = m, true
		}
	}
	return fallback, found
}

// simAIOTarget resolves an All in One instance to a connection, reusing
// the same helpers the Query Runner, Benchmark and Data Generator use so an
// instance behaves identically wherever it is addressed from.
func (a *App) simAIOTarget(ctx context.Context, st Stack, doc designDoc, aioNode, instance string, timeout time.Duration) (simResolved, error) {
	label := nodeLabel(doc, aioNode)

	deadline := time.Now().Add(timeout)
	for {
		dep, err := a.store.GetDeployment(st.ID, aioNode)
		switch {
		case err == nil && dep.State == DeployError:
			return simResolved{}, fmt.Errorf("the selected All in One node %s failed to provision", label)
		case err == nil && dep.State == DeployRunning && dep.ContainerID != "":
			m, ok := simAIOMember(dep, instance)
			if !ok {
				return simResolved{}, fmt.Errorf(
					"All in One node %s has no ready instance called %q — check the instance is still declared on that node and finished starting",
					label, instance)
			}
			engine, port, user, pass := aioInstanceCreds(dep, m)
			host := m.FQDN
			display := aioTargetLabel(label, m)

			res := simResolved{
				secrets: simSecrets{User: user, Password: pass},
				engine:  engine, kind: "aio-" + engine, displayName: display,
				host: host, port: port,
			}
			switch engine {
			case "mysql":
				res.env = []string{"DB_ENGINE=mysql", fmt.Sprintf(
					"MYSQL_DSN=%s:%s@tcp(%s:%d)/?tls=false", user, pass, host, port)}
			case "postgres":
				res.env = []string{"DB_ENGINE=postgres", "POSTGRES_DSN=" + (&url.URL{
					Scheme: "postgres", User: url.UserPassword(user, pass),
					Host: fmt.Sprintf("%s:%d", host, port), Path: "/postgres",
					RawQuery: "sslmode=prefer&connect_timeout=10",
				}).String()}
			case "mongodb":
				res.env = []string{"DB_ENGINE=mongodb", fmt.Sprintf(
					"MONGO_URI=mongodb://%s:%s@%s:%d/?authSource=admin&directConnection=true",
					url.QueryEscape(user), url.QueryEscape(pass), host, port)}
			default:
				return simResolved{}, fmt.Errorf(
					"All in One instance %q is a %s instance, which this application cannot drive", instance, m.Kind)
			}
			return res, nil
		}
		if time.Now().After(deadline) {
			return simResolved{}, fmt.Errorf(
				"the selected All in One node %s did not become ready within %s", label, timeout)
		}
		select {
		case <-ctx.Done():
			return simResolved{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// k3dServerConfig returns the stored k3dConfig of a K3D frame's server node —
// the one that carries the cluster-wide record, and whose container is the one
// kubectl runs in — along with that container's id.
func (a *App) k3dServerConfig(stackID int64, doc designDoc, frameID string) (k3dConfig, string, bool) {
	for _, n := range doc.Nodes {
		if n.FrameID != frameID || n.Type != "k3d" {
			continue
		}
		dep, err := a.store.GetDeployment(stackID, n.ID)
		if err != nil || dep.State != DeployRunning || dep.ContainerID == "" || len(dep.Config) == 0 {
			continue
		}
		var cfg k3dConfig
		if json.Unmarshal(dep.Config, &cfg) != nil || cfg.Role != "server" {
			continue
		}
		return cfg, dep.ContainerID, true
	}
	return k3dConfig{}, "", false
}

// splitHostPortDefault splits "host:port", falling back to def when there is no
// port to read.
func splitHostPortDefault(addr string, def int) (string, int) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, def
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return h, def
	}
	return h, n
}

// waitMySQLFamilyNode blocks until a standalone MySQL-family node is running
// and returns its FQDN plus its stored credentials. Percona Server, MariaDB and
// MySQL CE all provision the same credential set (mysqlFamilySecrets), so one
// helper covers all three; only the wording of a timeout differs.
func (a *App) waitMySQLFamilyNode(ctx context.Context, stackID int64, nodeID string, hosts map[string]string, domain, kind string, timeout time.Duration) (string, pxcSecrets, error) {
	dep, err := a.waitSimNode(ctx, stackID, nodeID, timeout, engineDisplayLabel(simEngineForKind(kind))+" ("+kind+")")
	if err != nil {
		return "", pxcSecrets{}, err
	}
	var sec pxcSecrets
	json.Unmarshal(dep.Secrets, &sec)
	return fqdnOf(hosts[nodeID], domain), sec, nil
}

// waitMySQLFamilyFrame blocks until a MySQL-family cluster frame has a usable
// write endpoint, and returns that member's FQDN plus the frame's credentials.
//
// Members of every one of these frames are nodes whose own type equals the
// frame's, which is what lets one helper serve MariaDB replication, Galera,
// MySQL CE replication and both flavours of Group Replication. wantPrimary
// selects the member marked primary — required where writes only work there
// (asynchronous replication) and pointless where they work anywhere (Galera,
// and Group Replication reached through its router).
func (a *App) waitMySQLFamilyFrame(ctx context.Context, stackID int64, frame designFrame, doc designDoc, domain string, wantPrimary bool, timeout time.Duration) (string, pxcSecrets, error) {
	hosts := stackHostnames(doc)
	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == frame.Type {
			members = append(members, n)
		}
	}
	if len(members) == 0 {
		return "", pxcSecrets{}, fmt.Errorf("associated cluster %s has no members", frame.Label)
	}
	// Stable order so a redeploy picks the same member, matching how every
	// provisioner in this codebase orders a frame's nodes.
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })

	deadline := time.Now().Add(timeout)
	for {
		deps, err := a.store.ListDeployments(stackID)
		if err == nil {
			byNode := map[string]Deployment{}
			for _, d := range deps {
				byNode[d.NodeID] = d
			}
			othersUp := false
			// usable counts the members that could serve writes: the primary when
			// wantPrimary, else any member. Once all of them have failed, none will.
			usable, failed := 0, 0
			for _, n := range members {
				d, ok := byNode[n.ID]
				if !wantPrimary || n.Role == "primary" {
					usable++
					if ok && d.State == DeployError {
						failed++
					}
				}
				if !ok || d.State != DeployRunning || d.ContainerID == "" {
					continue
				}
				if !wantPrimary || n.Role == "primary" {
					var sec pxcSecrets
					json.Unmarshal(d.Secrets, &sec)
					return fqdnOf(hosts[n.ID], domain), sec, nil
				}
				othersUp = true
			}
			// A replication frame whose primary is down still has replicas up.
			// Writing to one would fail, so once the wait is over say what is
			// actually wrong rather than blaming readiness in general.
			if wantPrimary && othersUp && time.Now().After(deadline) {
				return "", pxcSecrets{}, fmt.Errorf(
					"associated cluster %s has running members but no primary — writes have nowhere to go", frame.Label)
			}
			if usable > 0 && failed == usable {
				return "", pxcSecrets{}, fmt.Errorf("associated cluster %s failed to provision", frame.Label)
			}
		}
		if time.Now().After(deadline) {
			return "", pxcSecrets{}, fmt.Errorf("associated cluster %s did not become ready within %s", frame.Label, timeout)
		}
		select {
		case <-ctx.Done():
			return "", pxcSecrets{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// waitSimNode blocks until a standalone node is running and returns its
// deployment, so the caller can read that node type's own stored secrets.
// Neither MongoDB nor Valkey standalone nodes have a dedicated wait helper —
// their sibling sims poll inline — so this is the shared one.
func (a *App) waitSimNode(ctx context.Context, stackID int64, nodeID string, timeout time.Duration, what string) (Deployment, error) {
	deadline := time.Now().Add(timeout)
	for {
		if dep, err := a.store.GetDeployment(stackID, nodeID); err == nil {
			if dep.State == DeployError {
				return Deployment{}, fmt.Errorf("linked %s node failed to provision", what)
			}
			if dep.State == DeployRunning && dep.ContainerID != "" {
				return dep, nil
			}
		}
		if time.Now().After(deadline) {
			return Deployment{}, fmt.Errorf("linked %s node did not become ready within %s", what, timeout)
		}
		select {
		case <-ctx.Done():
			return Deployment{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
