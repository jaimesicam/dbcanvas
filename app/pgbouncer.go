package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PgBouncer node (Type=="pgbouncer"). A lightweight PostgreSQL connection pooler —
// Percona's `percona-pgbouncer` package, out of the same ppg-NN repository the rest
// of the PostgreSQL family here installs from — placed in front of ONE PostgreSQL
// backend drawn on the canvas (mutually exclusive, exactly like HAProxy):
//
//	pg      — a standalone PostgreSQL node. One server, one pool, nothing to follow.
//	patroni — a Patroni HA cluster. The leader moves, so the pool has to move with it:
//	          a watcher polls each member's Patroni REST API (:8008 /primary,
//	          /replica) and rewrites the pool's host on failover.
//	repmgr  — streaming replication + repmgrd. No REST API of its own, so the same
//	          watcher asks each member `pg_is_in_recovery()` over psql instead.
//	spock   — multi-master logical replication. Every member is a writer, so there is
//	          no role to follow: the pool is pinned to one member at a time (writing
//	          the same rows from two nodes at once is the conflict storm, not the
//	          demo) and the watcher only moves it when that member stops answering.
//
// The difference from HAProxy matters and is the reason both exist. HAProxy is a TCP
// load balancer: it hands a client a connection to a member and gets out of the way,
// and a thousand clients are a thousand backend connections. PgBouncer terminates the
// PostgreSQL protocol, so N client connections share a much smaller pool of server
// connections — which is the actual answer to PostgreSQL's process-per-connection
// cost, and the thing you want in front of a Patroni cluster that an application
// server farm is about to open 2000 connections to.
//
// Everything a pool decides is a design option on the node (pool mode, sizes,
// auth, the read/write split, the failover watcher) — see designNode's Pgb* fields.

const (
	// pgBouncerPort is both the client port and the admin console's: the console is
	// the virtual database "pgbouncer" on the same listener.
	pgBouncerPort = 6432
	// pgBouncerConfDir holds pgbouncer.ini, the generated databases.ini, userlist.txt
	// and (with TLS on) the node's certificate.
	pgBouncerConfDir = "/etc/pgbouncer"
	// pgBouncerClientDir holds the client certificate minted for cert authentication,
	// kept apart from the pool's own server material so "the key I hand to a client"
	// and "the key the pool listens with" are not one directory listing apart.
	pgBouncerClientDir = "/etc/pgbouncer/client"
)

var pgBouncerPorts = []int{pgBouncerPort}

// pgBouncerConfig is the non-secret profile shown for a deployed PgBouncer node. It
// carries the resolved pool settings rather than the raw node properties, because
// what the panel has to explain is what the running pooler actually does — the
// defaults are filled in here, not in the UI.
type pgBouncerConfig struct {
	Image    string `json:"image"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`
	FQDN     string `json:"fqdn"`
	// Backend is the kind of PostgreSQL topology behind the pool ("pg" | "patroni" |
	// "repmgr" | "spock"); Cluster is its canvas label and Members its member FQDNs.
	Backend string   `json:"backend"`
	Cluster string   `json:"cluster"`
	Members []string `json:"members"`
	PGMajor string   `json:"pgMajor"` // ppg-NN repository the pooler came from
	// The pool, as configured.
	PoolMode         string `json:"poolMode"`
	AuthType         string `json:"authType"`
	AuthQuery        bool   `json:"authQuery"`
	MaxClientConn    int    `json:"maxClientConn"`
	DefaultPoolSize  int    `json:"defaultPoolSize"`
	MinPoolSize      int    `json:"minPoolSize"`
	ReservePoolSize  int    `json:"reservePoolSize"`
	MaxDBConnections int    `json:"maxDbConnections"`
	IgnoreStartup    string `json:"ignoreStartupParameters"`
	ServerTLSMode    string `json:"serverTlsMode"`
	// Routing is the shape of the [databases] section: "rw" (one pool at the writable
	// member), "rw-ro" (that plus a read-only alias on a standby) or "mesh" (one alias
	// per member, for Spock).
	Routing string `json:"routing"`
	// Database is the database the named aliases pool into, and ReadAlias/MemberAlias
	// are the alias names a client can ask for. The wildcard pool "*" always exists
	// and always points at the writable member.
	Database     string   `json:"database"`
	ReadAlias    string   `json:"readAlias,omitempty"`
	MemberAlias  []string `json:"memberAliases,omitempty"`
	FollowRole   bool     `json:"followRole"`
	WatchSeconds int      `json:"watchSeconds,omitempty"`
	GenerateCert bool     `json:"generateCert"`
	UseProxy     bool     `json:"useProxy"`
	// ClientCertUser is the role whose name is in the CN of the client certificate
	// minted for this pool, and ClientCertDir where that certificate and its key
	// were put. Both empty unless auth_type is "cert". PgBouncer takes the user name
	// from the CN, so a certificate is per role and the panel has to say which.
	ClientCertUser string `json:"clientCertUser,omitempty"`
	ClientCertDir  string `json:"clientCertDir,omitempty"`
	Ports          []int  `json:"ports"`
	ExportPort     int    `json:"exportPort"` // published host port for 6432 (0 = none)
}

// ---------------------------------------------------------------- design model

// pgBouncerDefaults fills in every unset Pgb* option on a node. One function, called
// by the provisioner, the validator and the design-time summary alike, so "what this
// pool will actually do" has exactly one answer and the UI never has to repeat it.
//
// backend is the resolved backend kind ("" when nothing is linked yet): it decides
// only the two defaults that genuinely differ per topology — the routing shape (a
// Spock mesh has no standbys to split reads onto) and whether the role watcher is
// worth running (a standalone server has no role that can move).
func pgBouncerDefaults(n designNode, backend string) designNode {
	if n.PgbPoolMode == "" {
		n.PgbPoolMode = "transaction"
	}
	if n.PgbAuthType == "" {
		n.PgbAuthType = "scram-sha-256"
	}
	if n.PgbMaxClientConn <= 0 {
		n.PgbMaxClientConn = 500
	}
	if n.PgbDefaultPoolSize <= 0 {
		n.PgbDefaultPoolSize = 20
	}
	if n.PgbIgnoreStartupParams == "" {
		// The three a PostgreSQL driver sends that PgBouncer rejects by default, and
		// every one of them is a real report: psycopg/asyncpg send extra_float_digits,
		// JDBC sends options, and a pooled app that sets search_path in its connection
		// string cannot connect at all without the third. See IMPLEMENTATION.md §297,
		// where the same three had to be added for the Kubernetes PgBouncer.
		n.PgbIgnoreStartupParams = "extra_float_digits,options,search_path"
	}
	if n.PgbServerTLS == "" {
		n.PgbServerTLS = "prefer"
	}
	if n.PgbRouting == "" {
		if backend == "spock" {
			n.PgbRouting = "mesh"
		} else if backend == "pg" {
			n.PgbRouting = "rw"
		} else {
			n.PgbRouting = "rw-ro"
		}
	}
	if n.PgbDatabase == "" {
		if backend == "spock" {
			n.PgbDatabase = spockDefaultDB
		} else {
			n.PgbDatabase = "postgres"
		}
	}
	if n.PgbWatchInterval <= 0 {
		n.PgbWatchInterval = 5
	}
	return n
}

// pgBouncerFollowsRole reports whether the role watcher runs for this node. A
// standalone PostgreSQL backend has no role that can move, so the option is ignored
// there rather than installing a timer that can only ever find the same answer.
func pgBouncerFollowsRole(n designNode, backend string) bool {
	return n.PgbFollowPrimary && backend != "pg" && backend != ""
}

// pgBouncerAlias derives the pgbouncer database-alias suffix for a member host:
// "pgnode-1.example.net" → "pgnode_1". PgBouncer's config is an ini file whose keys
// are the database names clients ask for, so the alias has to survive being typed
// into a connection string.
func pgBouncerAlias(fqdn string) string {
	h := fqdn
	if i := strings.Index(h, "."); i > 0 {
		h = h[:i]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '_'
		}
	}, h)
}

// pgBouncerBackends returns the distinct PostgreSQL backends directly associated
// with a PgBouncer node by an association line: a standalone PostgreSQL node, or a
// Patroni, repmgr or Spock cluster frame. A pool fronts exactly one, so 0 (unlinked)
// and >1 (ambiguous) are both validation errors — the same mutual-exclusivity rule
// HAProxy has, for the same reason: the four backends need four different
// [databases] sections and, for three of them, a different way of asking who can
// currently take writes.
//
// The return is parallel slices rather than a struct because a frame backend has a
// designFrame and a standalone backend a designNode, and only one of the two is ever
// set for a given entry. pgBouncerBackend below is what callers actually use.
func pgBouncerBackends(doc designDoc, startID string) (kinds []string, frames []designFrame, nodes []designNode) {
	frameByID := map[string]designFrame{}
	for _, f := range doc.Frames {
		if f.Type == "patroni" || f.Type == "repmgr" || f.Type == "spock" {
			frameByID[f.ID] = f
		}
	}
	nodeByID := map[string]designNode{}
	for _, n := range doc.Nodes {
		if n.Type == "pg" && n.FrameID == "" {
			nodeByID[n.ID] = n
		}
	}
	seen := map[string]bool{}
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
		if seen[other] {
			continue
		}
		if f, ok := frameByID[other]; ok {
			seen[other] = true
			kinds = append(kinds, f.Type)
			frames = append(frames, f)
			nodes = append(nodes, designNode{})
			continue
		}
		if n, ok := nodeByID[other]; ok {
			seen[other] = true
			kinds = append(kinds, "pg")
			frames = append(frames, designFrame{})
			nodes = append(nodes, n)
		}
	}
	return kinds, frames, nodes
}

// pgBouncerBackend returns the single backend a PgBouncer node pools for, ok only
// when exactly one is associated.
func pgBouncerBackend(doc designDoc, startID string) (kind string, frame designFrame, node designNode, ok bool) {
	kinds, frames, nodes := pgBouncerBackends(doc, startID)
	if len(kinds) != 1 {
		return "", designFrame{}, designNode{}, false
	}
	return kinds[0], frames[0], nodes[0], true
}

// pgBouncerPGMajor is the PostgreSQL major series whose percona-release repository
// (ppg-NN) the pooler and the psql client are installed from. It follows the backend
// rather than being chosen on the node: percona-pgbouncer is built against a series,
// and a pool in front of a PostgreSQL 17 cluster that was installed from ppg-13 is a
// mismatch nobody asked for. An explicit PGMajor on the node still wins, for the
// case where the point of the lab *is* the mismatch.
func pgBouncerPGMajor(n designNode, kind string, frame designFrame, back designNode) string {
	if m := strings.TrimSpace(n.PGMajor); m != "" {
		return m
	}
	if kind == "pg" {
		return ppgMajorOf(back.PGMajor)
	}
	return ppgMajorOf(frame.PGMajor)
}

// ---------------------------------------------------------------- provisioning

// provisionPgBouncer records + provisions a PgBouncer node in front of the single
// PostgreSQL backend it is linked to on the canvas.
func (a *App) provisionPgBouncer(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	if host == "" {
		host = "pgbouncer"
	}
	image := pxcImage(n.OS, n.OSVersion, n.Arch)
	fqdn := fqdnOf(host, domain)

	cfg := pgBouncerConfig{
		Image: image, OS: n.OS, Arch: archOr(n.Arch),
		Hostname: host, FQDN: fqdn,
		GenerateCert: n.GenerateCert, UseProxy: n.UseProxy, Ports: pgBouncerPorts,
	}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		pr.phase("Waiting for Intranet to be ready", 5)
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		// Resolve the single associated PostgreSQL backend and wait for it to be
		// running, so its members exist before the pool's [databases] section is
		// written. All four kinds yield the same two things — a label to show and a
		// list of member FQDNs — plus the superuser credentials the pool authenticates
		// to the backend with.
		kind, frame, backNode, ok := pgBouncerBackend(doc, n.ID)
		if !ok {
			pr.fail("PgBouncer must be linked to exactly one PostgreSQL node or Patroni, repmgr or Spock cluster")
			return
		}
		opt := pgBouncerDefaults(n, kind)
		var members []string
		var label string
		sec := pgFamilySecrets()
		switch kind {
		case "pg":
			pr.phase("Waiting for PostgreSQL", 15)
			// The standalone node keeps its own superuser secret, so this is also
			// where the pool's userlist credentials come from.
			m, s, cerr := a.waitPgNodeRunning(ctx, st.ID, backNode.ID, stackHostnames(doc), domain, deployTimeout())
			if cerr != nil {
				pr.fail("%v", cerr)
				return
			}
			members, sec, label = []string{m}, s, backNode.Label
		case "patroni":
			pr.phase("Waiting for Patroni cluster", 15)
			m, s, cerr := a.waitPatroniRunning(ctx, st.ID, frame, doc, domain, deployTimeout())
			if cerr != nil {
				pr.fail("%v", cerr)
				return
			}
			members, sec, label = m, s, frame.Label
		case "repmgr":
			pr.phase("Waiting for repmgr cluster", 15)
			ms, cerr := a.waitRepmgrMembers(ctx, st.ID, frame, doc, domain, deployTimeout())
			if cerr != nil {
				pr.fail("%v", cerr)
				return
			}
			for _, m := range ms {
				members = append(members, m.FQDN)
			}
			label = frame.Label
		case "spock":
			pr.phase("Waiting for Spock cluster", 15)
			m, s, cerr := a.waitSpockRunning(ctx, st.ID, frame, doc, domain, deployTimeout())
			if cerr != nil {
				pr.fail("%v", cerr)
				return
			}
			members, sec, label = m, s, frame.Label
		default:
			pr.fail("unsupported backend type %q", kind)
			return
		}
		if len(members) == 0 {
			pr.fail("backend %s has no running member to pool for", label)
			return
		}
		major := pgBouncerPGMajor(n, kind, frame, backNode)

		cfg.Backend, cfg.Cluster, cfg.Members, cfg.PGMajor = kind, label, members, major
		cfg.PoolMode, cfg.AuthType, cfg.AuthQuery = opt.PgbPoolMode, opt.PgbAuthType, opt.PgbAuthQuery
		cfg.MaxClientConn, cfg.DefaultPoolSize = opt.PgbMaxClientConn, opt.PgbDefaultPoolSize
		cfg.MinPoolSize, cfg.ReservePoolSize = opt.PgbMinPoolSize, opt.PgbReservePoolSize
		cfg.MaxDBConnections, cfg.IgnoreStartup = opt.PgbMaxDBConnections, opt.PgbIgnoreStartupParams
		cfg.ServerTLSMode, cfg.Routing, cfg.Database = opt.PgbServerTLS, opt.PgbRouting, opt.PgbDatabase
		cfg.FollowRole = pgBouncerFollowsRole(opt, kind)
		if cfg.FollowRole {
			cfg.WatchSeconds = opt.PgbWatchInterval
		}
		if cfg.Routing == "rw-ro" {
			cfg.ReadAlias = opt.PgbDatabase + "_ro"
		}
		if cfg.Routing == "mesh" {
			for _, m := range members {
				cfg.MemberAlias = append(cfg.MemberAlias, opt.PgbDatabase+"_"+pgBouncerAlias(m))
			}
		}
		pr.logln(fmt.Sprintf("%s backend %s is running (%d member(s))", kind, label, len(members)))

		pr.phase("Creating container", 25)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		spec := ContainerSpec{
			Name: name, Image: image, Hostname: host, Privileged: true,
			Network: networkName(st.ID), Aliases: []string{host},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		}
		applyVMSize(&spec, n.limits())
		if n.ExportEnabled {
			spec.PublishMap = []PortMap{{ContainerPort: pgBouncerPort, HostPort: n.ExportHostPort}}
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, spec)
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}
		a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
		if n.ExportEnabled {
			if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", pgBouncerPort)); e == nil {
				cfg.ExportPort, _ = strconv.Atoi(hp)
			}
		}
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON})

		pr.phase("Waiting for systemd", 32)
		if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
			pr.fail("systemd did not start: %v", err)
			return
		}
		a.trustIntranetCA(ctx, st, id, n.OS, pr.logln)
		a.ensureDNFIPv4(ctx, id, n.OS, pr.logln)

		debian := isDebianOS(n.OS)
		if n.UseProxy {
			proxyScript := pkgProxyRHEL
			if debian {
				proxyScript = pkgProxyDebian
			}
			if err := a.runStep(ctx, id, proxyScript, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
				pr.fail("configure package proxy: %v", err)
				return
			}
			pr.logln("package egress via Intranet proxy")
		}

		// percona-pgbouncer and the psql client both come from the backend's own
		// ppg-NN repository: one percona-release setup, two packages. psql is not
		// decoration — the role watcher asks repmgr and Spock members
		// pg_is_in_recovery() with it, and it is what a person opening a console on
		// this node reaches for.
		pr.phase("Installing PgBouncer", 45)
		instScript := pgBouncerInstallRHEL
		if debian {
			instScript = pgBouncerInstallDebian
		}
		instEnv := []string{
			"PRODUCT=" + ppgProduct(major),
			"PKGS=" + strings.Join(pgBouncerPackages(n.OS, major), " "),
		}
		if err := a.runStep(ctx, id, instScript, instEnv, pr.logln); err != nil {
			pr.fail("install pgbouncer: %v", err)
			return
		}
		pr.logln("percona-pgbouncer installed from " + ppgProduct(major))
		a.ensureRsyslog(ctx, id, n.OS, pr.logln)

		if err := a.runStep(ctx, id, pgBouncerPrepDirsScript, nil, pr.logln); err != nil {
			pr.fail("prepare pgbouncer directories: %v", err)
			return
		}

		// Optional TLS termination at the pool, signed by the Intranet CA — the same
		// material and the same TTL controls a PostgreSQL node gets, written into
		// /etc/pgbouncer instead of the data directory.
		if n.GenerateCert {
			pr.phase("Issuing certificate", 55)
			if err := a.pgBouncerApplyCert(ctx, id, intranetID, fqdn, n.CertTTLValue, n.CertTTLUnit, pr.logln); err != nil {
				pr.fail("%v", err)
				return
			}
			// A client certificate for the superuser, minted whenever this node has
			// a certificate at all — because it is needed by *both* legs and only
			// one of them is obvious:
			//
			//   client → pool: auth_type=cert has to have something to authenticate
			//     with, and PgBouncer takes the user name from the CN.
			//   pool → server: a backend whose pg_hba puts `hostssl … cert` first
			//     matches the pool's own connection, since the pool connects over
			//     TLS — and refuses it outright with "connection requires a valid
			//     client certificate" unless the pool presents one. That failure is
			//     on the *server* leg and says nothing about how the client
			//     authenticated, which is exactly why it is worth spending one
			//     openssl invocation to make impossible.
			pr.phase("Issuing client certificate", 58)
			if err := a.pgBouncerApplyClientCert(ctx, id, intranetID, sec.SuperUser, n.CertTTLValue, n.CertTTLUnit, pr.logln); err != nil {
				pr.fail("%v", err)
				return
			}
			cfg.ClientCertUser, cfg.ClientCertDir = sec.SuperUser, pgBouncerClientDir
		}

		pr.phase("Configuring PgBouncer", 65)
		files := map[string]fileEntry{
			"pgbouncer.ini":  {0o640, 0, []byte(pgBouncerINI(opt, kind, n.GenerateCert, sec))},
			"databases.ini":  {0o640, 0, []byte(pgBouncerDatabasesINI(opt, kind, members))},
			"userlist.txt":   {0o640, 0, []byte(pgBouncerUserlist(sec))},
			"dbcanvas-notes": {0o640, 0, []byte(pgBouncerNotes(kind, label, opt))},
		}
		if err := a.engCtx(ctx).PutArchive(ctx, id, pgBouncerConfDir, tarFiles(files)); err != nil {
			pr.fail("write pgbouncer config: %v", err)
			return
		}

		// The role watcher. It is installed for every non-standalone backend even when
		// the timer is off, because the provisioner runs it once here: that is what
		// makes the pool point at the *current* leader the moment PgBouncer starts,
		// instead of at whichever member happened to be first in the design.
		if kind != "pg" {
			if err := a.engCtx(ctx).PutArchive(ctx, id, "/usr/local/bin", tarFiles(map[string]fileEntry{
				"dbcanvas-pgbouncer-watch": {0o755, 0, []byte(pgBouncerWatchScript)},
			})); err != nil {
				pr.fail("write role watcher: %v", err)
				return
			}
			env := pgBouncerWatchEnv(opt, kind, members, sec)
			if err := a.engCtx(ctx).CopyFile(ctx, id, pgBouncerConfDir, "backend.env", 0o600, []byte(env)); err != nil {
				pr.fail("write backend.env: %v", err)
				return
			}
			if err := a.runStep(ctx, id, pgBouncerWatchOnceScript, nil, pr.logln); err != nil {
				// Not fatal: the databases.ini written above is already a valid
				// config, and the timer (or the next run) will correct it.
				pr.logln("initial role probe did not resolve a writable member yet: " + err.Error())
			}
		}

		pr.phase("Starting PgBouncer", 80)
		if err := a.runStep(ctx, id, pgBouncerStartScript, nil, pr.logln); err != nil {
			pr.fail("start pgbouncer: %v", err)
			return
		}
		pr.logln(fmt.Sprintf("pgbouncer listening on :%d (%s pooling, default_pool_size=%d, max_client_conn=%d)",
			pgBouncerPort, opt.PgbPoolMode, opt.PgbDefaultPoolSize, opt.PgbMaxClientConn))

		if cfg.FollowRole {
			pr.phase("Enabling role watcher", 92)
			if err := a.runStep(ctx, id, pgBouncerWatchTimerScript,
				[]string{"INTERVAL=" + strconv.Itoa(opt.PgbWatchInterval)}, pr.logln); err != nil {
				pr.logln("role watcher not enabled: " + err.Error())
			} else {
				pr.logln(fmt.Sprintf("role watcher polling every %ds — the pool follows the writable member", opt.PgbWatchInterval))
			}
		}

		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		log.Printf("stack %d pgbouncer %s: provisioned (%s backend %s)", st.ID, n.ID, kind, label)
	}()
}

// pgBouncerPackages is what gets installed from the ppg-NN repo: the pooler itself
// and the PostgreSQL client (psql), which the role watcher uses on repmgr and Spock
// backends. EL and Debian spell the client package differently, the same split
// pgServerPackages already has.
func pgBouncerPackages(os, major string) []string {
	major = ppgMajorOf(major)
	if isDebianOS(os) {
		return []string{"percona-pgbouncer", "percona-postgresql-client-" + major}
	}
	return []string{"percona-pgbouncer", "percona-postgresql" + major}
}

// pgBouncerApplyCert signs a server certificate + key from the Intranet CA into
// /etc/pgbouncer, pgbouncer-owned. Mirrors pgApplyCert, which does the same for a
// PostgreSQL data directory.
func (a *App) pgBouncerApplyCert(ctx context.Context, containerID, intranetID, fqdn string, ttlValue int, ttlUnit string, logln func(string)) error {
	if logln == nil {
		logln = func(string) {}
	}
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	caCrt, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}
	caKey, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if err != nil {
		return fmt.Errorf("read CA key: %w", err)
	}
	if err := a.engCtx(ctx).PutArchive(ctx, containerID, "/tmp", tarFiles(map[string]fileEntry{
		"dbca-ca.crt": {0o644, 0, caCrt},
		"dbca-ca.key": {0o644, 0, caKey},
	})); err != nil {
		return fmt.Errorf("stage CA: %w", err)
	}
	if ttlValue <= 0 {
		ttlValue, ttlUnit = 365, "days"
	}
	switch ttlUnit {
	case "minutes", "hours", "days":
	default:
		ttlUnit = "days"
	}
	env := []string{"FQDN=" + fqdn, "VALUE=" + strconv.Itoa(ttlValue), "UNIT=" + ttlUnit, "DIR=" + pgBouncerConfDir}
	if err := a.runStep(ctx, containerID, pgBouncerCertScript, env, logln); err != nil {
		return fmt.Errorf("generate certificate: %w", err)
	}
	logln("per-node certificate written to " + pgBouncerConfDir + " (pgbouncer-owned)")
	return nil
}

// pgBouncerApplyClientCert signs a *client* certificate for one role. Its CN is the
// role name, because that is where PgBouncer reads the user name from under
// auth_type=cert — a certificate is therefore per role, and one whose CN does not
// match the user in the connection string is refused (verified: "FATAL: certificate
// authentication failed").
//
// extendedKeyUsage is clientAuth alone. The pool's own certificate carries serverAuth
// as well, and handing a client something that would also work as a server
// certificate is not a distinction worth blurring in a lab that is about TLS.
func (a *App) pgBouncerApplyClientCert(ctx context.Context, containerID, intranetID, role string, ttlValue int, ttlUnit string, logln func(string)) error {
	if logln == nil {
		logln = func(string) {}
	}
	caCrt, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}
	caKey, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if err != nil {
		return fmt.Errorf("read CA key: %w", err)
	}
	if err := a.engCtx(ctx).PutArchive(ctx, containerID, "/tmp", tarFiles(map[string]fileEntry{
		"dbca-ca.crt": {0o644, 0, caCrt},
		"dbca-ca.key": {0o644, 0, caKey},
	})); err != nil {
		return fmt.Errorf("stage CA: %w", err)
	}
	if ttlValue <= 0 {
		ttlValue, ttlUnit = 365, "days"
	}
	switch ttlUnit {
	case "minutes", "hours", "days":
	default:
		ttlUnit = "days"
	}
	env := []string{"ROLE=" + role, "VALUE=" + strconv.Itoa(ttlValue), "UNIT=" + ttlUnit, "DIR=" + pgBouncerClientDir}
	if err := a.runStep(ctx, containerID, pgBouncerClientCertScript, env, logln); err != nil {
		return fmt.Errorf("generate client certificate: %w", err)
	}
	logln("client certificate for " + role + " written to " + pgBouncerClientDir)
	return nil
}

// --------------------------------------------------------------- config files

// pgBouncerINI renders /etc/pgbouncer/pgbouncer.ini. The [databases] section is not
// here: it is %include'd from databases.ini, which is the one file the role watcher
// rewrites — so a failover reloads a four-line file rather than regenerating the
// whole config from a script running inside the container.
func pgBouncerINI(n designNode, backend string, tls bool, sec pgSecrets) string {
	// clientCert is the certificate this pool can present — to a backend that asks
	// for one, and as the thing its own clients authenticate with under cert auth.
	// It exists exactly when the node was given a certificate.
	clientCert := tls
	var b strings.Builder
	b.WriteString(";; Generated by DBCanvas — do not edit by hand.\n")
	b.WriteString(";; The pool's backend list lives in databases.ini; on a Patroni, repmgr or\n")
	b.WriteString(";; Spock backend that file is rewritten by dbcanvas-pgbouncer-watch and\n")
	b.WriteString(";; picked up with SIGHUP, so a failover never needs this file changed.\n")
	fmt.Fprintf(&b, "%%include %s/databases.ini\n\n", pgBouncerConfDir)

	b.WriteString("[pgbouncer]\n")
	b.WriteString("listen_addr = *\n")
	fmt.Fprintf(&b, "listen_port = %d\n", pgBouncerPort)
	b.WriteString("unix_socket_dir = /var/run/pgbouncer\n")
	b.WriteString("logfile = /var/log/pgbouncer/pgbouncer.log\n")
	b.WriteString("pidfile = /var/run/pgbouncer/pgbouncer.pid\n\n")

	fmt.Fprintf(&b, "auth_type = %s\n", n.PgbAuthType)
	if n.PgbAuthType != "trust" {
		// Still needed under cert auth, and for a different leg: the certificate
		// authenticates the *client*, but PgBouncer must then authenticate itself to
		// PostgreSQL, and with no client password to forward the only secret it has
		// is the plain text in this file. See pgBouncerIssues.
		fmt.Fprintf(&b, "auth_file = %s/userlist.txt\n", pgBouncerConfDir)
	}
	// auth_query is deliberately not emitted under cert auth. pg_shadow returns a
	// SCRAM *verifier*, and a verifier cannot be used to authenticate as a client —
	// so with no client password to pass through, a role looked up that way could be
	// let in at the pool and then fail at the server. Only roles with a plain-text
	// secret in userlist.txt can complete both legs.
	if n.PgbAuthQuery && n.PgbAuthType != "trust" && n.PgbAuthType != "cert" {
		// Without this, every application role has to be copied into userlist.txt by
		// hand and re-copied whenever a password changes. With it, PgBouncer connects
		// as the superuser (whose own password *is* in userlist.txt, which is what
		// breaks the chicken-and-egg) and looks the client up in pg_shadow.
		fmt.Fprintf(&b, "auth_user = %s\n", sec.SuperUser)
		b.WriteString("auth_dbname = postgres\n")
		b.WriteString("auth_query = SELECT usename, passwd FROM pg_shadow WHERE usename = $1\n")
	}
	fmt.Fprintf(&b, "admin_users = %s\n", sec.SuperUser)
	fmt.Fprintf(&b, "stats_users = %s\n\n", sec.SuperUser)

	fmt.Fprintf(&b, "pool_mode = %s\n", n.PgbPoolMode)
	fmt.Fprintf(&b, "max_client_conn = %d\n", n.PgbMaxClientConn)
	fmt.Fprintf(&b, "default_pool_size = %d\n", n.PgbDefaultPoolSize)
	fmt.Fprintf(&b, "min_pool_size = %d\n", n.PgbMinPoolSize)
	fmt.Fprintf(&b, "reserve_pool_size = %d\n", n.PgbReservePoolSize)
	if n.PgbReservePoolSize > 0 {
		b.WriteString("reserve_pool_timeout = 5\n")
	}
	fmt.Fprintf(&b, "max_db_connections = %d\n", n.PgbMaxDBConnections)
	if n.PgbServerIdleTimeout > 0 {
		fmt.Fprintf(&b, "server_idle_timeout = %d\n", n.PgbServerIdleTimeout)
	}
	// DISCARD ALL is correct for session pooling and skipped in the other two modes
	// (server_reset_query_always = 0), where a client must not be relying on session
	// state in the first place. Spelling both out is what makes that explicit in the
	// file somebody will read after their prepared statement disappeared.
	b.WriteString("server_reset_query = DISCARD ALL\n")
	b.WriteString("server_reset_query_always = 0\n")
	fmt.Fprintf(&b, "ignore_startup_parameters = %s\n\n", n.PgbIgnoreStartupParams)

	fmt.Fprintf(&b, "server_tls_sslmode = %s\n", n.PgbServerTLS)
	if clientCert && n.PgbServerTLS != "disable" {
		// Presented only if the backend asks. A server that does not is unaffected;
		// one whose pg_hba starts with `hostssl … cert` accepts the pool instead of
		// refusing it, because the CN here is the role the pool connects as.
		fmt.Fprintf(&b, "server_tls_cert_file = %s/%s.crt\n", pgBouncerClientDir, sec.SuperUser)
		fmt.Fprintf(&b, "server_tls_key_file = %s/%s.key\n", pgBouncerClientDir, sec.SuperUser)
		fmt.Fprintf(&b, "server_tls_ca_file = %s/ca.crt\n", pgBouncerClientDir)
	}
	switch {
	case n.PgbAuthType == "cert":
		// Not a preference. PgBouncer refuses to start otherwise:
		//   ERROR auth_type=cert requires client_tls_sslmode=SSLMODE_VERIFY_FULL
		// which is also why cert auth is only offered on a node that was given a
		// certificate — there would be nothing to verify against.
		b.WriteString("client_tls_sslmode = verify-full\n")
		fmt.Fprintf(&b, "client_tls_cert_file = %s/server.crt\n", pgBouncerConfDir)
		fmt.Fprintf(&b, "client_tls_key_file = %s/server.key\n", pgBouncerConfDir)
		fmt.Fprintf(&b, "client_tls_ca_file = %s/ca.crt\n", pgBouncerConfDir)
	case tls:
		b.WriteString("client_tls_sslmode = require\n")
		fmt.Fprintf(&b, "client_tls_cert_file = %s/server.crt\n", pgBouncerConfDir)
		fmt.Fprintf(&b, "client_tls_key_file = %s/server.key\n", pgBouncerConfDir)
		fmt.Fprintf(&b, "client_tls_ca_file = %s/ca.crt\n", pgBouncerConfDir)
	default:
		b.WriteString("client_tls_sslmode = disable\n")
	}
	return b.String()
}

// pgBouncerDatabasesINI renders the [databases] section. The wildcard pool always
// exists and always points at the member that can take writes, so a client that asks
// for any database at all lands somewhere sensible; the named aliases are what make
// the topology's shape reachable:
//
//	rw     — wildcard only. A standalone server, or a cluster you only ever write to.
//	rw-ro  — plus "<db>_ro" on a standby, so an application can send its reports
//	         somewhere that is not the primary. Writes there are refused by PostgreSQL
//	         itself, which is the point.
//	mesh   — plus one alias per member ("<db>_<host>"), for Spock: every node is a
//	         writer, so the interesting thing to be able to do is aim at a *named*
//	         one and watch the write appear on the others.
//
// members[0] is the initial write target; on a Patroni, repmgr or Spock backend the
// role watcher replaces this file with the same shape and the real answer.
func pgBouncerDatabasesINI(n designNode, backend string, members []string) string {
	if len(members) == 0 {
		return "[databases]\n"
	}
	var b strings.Builder
	b.WriteString("[databases]\n")
	fmt.Fprintf(&b, "* = host=%s port=%d\n", members[0], patroniPGPort)
	switch n.PgbRouting {
	case "rw-ro":
		ro := members[0]
		if len(members) > 1 {
			ro = members[1]
		}
		fmt.Fprintf(&b, "%s_ro = host=%s port=%d dbname=%s\n", n.PgbDatabase, ro, patroniPGPort, n.PgbDatabase)
	case "mesh":
		for _, m := range members {
			fmt.Fprintf(&b, "%s_%s = host=%s port=%d dbname=%s\n",
				n.PgbDatabase, pgBouncerAlias(m), m, patroniPGPort, n.PgbDatabase)
		}
	}
	return b.String()
}

// pgBouncerUserlist is auth_file: the superuser (and the replication role, which is
// the other account that exists on every PostgreSQL node here) with plain-text
// secrets. PgBouncer derives a SCRAM verifier from a plain-text secret itself, so one
// file serves scram-sha-256 and md5 alike; with auth_query on, these two are the only
// entries that ever need to be here, because every other role is looked up live.
func pgBouncerUserlist(sec pgSecrets) string {
	var b strings.Builder
	b.WriteString("\"" + sec.SuperUser + "\" \"" + sec.SuperPassword + "\"\n")
	if sec.ReplUser != "" && sec.ReplUser != sec.SuperUser {
		b.WriteString("\"" + sec.ReplUser + "\" \"" + sec.ReplPassword + "\"\n")
	}
	return b.String()
}

// pgBouncerWatchEnv is the watcher's input: what topology it is looking at, which
// hosts to ask, and the credentials for the psql probes. Written 0600 — it carries
// the superuser password.
func pgBouncerWatchEnv(n designNode, backend string, members []string, sec pgSecrets) string {
	var b strings.Builder
	b.WriteString("# Generated by DBCanvas. Read by dbcanvas-pgbouncer-watch.\n")
	fmt.Fprintf(&b, "MODE=%s\n", backend)
	fmt.Fprintf(&b, "MEMBERS=%s\n", shQuote(strings.Join(members, " ")))
	fmt.Fprintf(&b, "ROUTING=%s\n", n.PgbRouting)
	fmt.Fprintf(&b, "DB=%s\n", shQuote(n.PgbDatabase))
	fmt.Fprintf(&b, "PGPORT=%d\n", patroniPGPort)
	fmt.Fprintf(&b, "RESTPORT=%d\n", patroniRESTPort)
	fmt.Fprintf(&b, "PGUSER=%s\n", shQuote(sec.SuperUser))
	fmt.Fprintf(&b, "PGPASSWORD=%s\n", shQuote(sec.SuperPassword))
	return b.String()
}

// pgBouncerNotes is a short human-readable crib left in /etc/pgbouncer, for whoever
// opens a console on this node rather than the panel.
func pgBouncerNotes(backend, cluster string, n designNode) string {
	var b strings.Builder
	fmt.Fprintf(&b, "DBCanvas PgBouncer — %s backend %q\n\n", backend, cluster)
	fmt.Fprintf(&b, "client port      : %d\n", pgBouncerPort)
	fmt.Fprintf(&b, "pool mode        : %s\n", n.PgbPoolMode)
	fmt.Fprintf(&b, "routing          : %s\n", n.PgbRouting)
	fmt.Fprintf(&b, "config           : %s/pgbouncer.ini (+ databases.ini)\n", pgBouncerConfDir)
	if n.PgbAuthType == "cert" {
		fmt.Fprintf(&b, "auth             : cert — the client certificate's CN is the role name\n")
		fmt.Fprintf(&b, "client cert      : %s/<role>.{crt,key} (+ ca.crt)\n", pgBouncerClientDir)
		b.WriteString("\nadmin console    : the unix socket does NOT work under cert auth (no TLS, so no\n")
		b.WriteString("                   certificate). Connect over TLS instead:\n")
		fmt.Fprintf(&b, "                   psql \"host=$(hostname -f) port=%d dbname=pgbouncer user=%s \\\n", pgBouncerPort, "postgres")
		fmt.Fprintf(&b, "                     sslmode=verify-full sslrootcert=%s/ca.crt \\\n", pgBouncerClientDir)
		fmt.Fprintf(&b, "                     sslcert=%s/postgres.crt sslkey=%s/postgres.key\"\n", pgBouncerClientDir, pgBouncerClientDir)
		b.WriteString("\nanother role     : openssl req -newkey rsa:2048 -nodes -keyout R.key -out R.csr \\\n")
		b.WriteString("                     -subj \"/O=DBCanvas/CN=<role>\"\n")
		b.WriteString("                   openssl x509 -req -in R.csr -CA ca.crt -CAkey <the Intranet CA key> \\\n")
		b.WriteString("                     -out R.crt -days 365\n")
		b.WriteString("                   The CN must equal the role in the connection string, and that role\n")
		b.WriteString("                   needs a plain-text secret in userlist.txt for the server leg.\n")
	} else {
		b.WriteString("\nadmin console    : psql -p 6432 -U postgres pgbouncer\n")
		b.WriteString("                   SHOW POOLS; SHOW SERVERS; SHOW CLIENTS; SHOW STATS;\n")
	}
	if backend != "pg" {
		b.WriteString("\nrole watcher     : systemctl status dbcanvas-pgbouncer-watch.timer\n")
		b.WriteString("                   /usr/local/bin/dbcanvas-pgbouncer-watch (run it by hand to re-probe)\n")
	}
	return b.String()
}

// ------------------------------------------------------------------ scripts

// pgBouncerInstallRHEL/Debian enable the backend's ppg-NN repository and install the
// pooler + psql from it. No version pin: percona-pgbouncer is not in the per-image
// version catalogue (that catalogue is server minors), so this takes what the series
// carries — which is also what a real Percona Distribution install would get.
const pgBouncerInstallRHEL = `set -e
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
dnf -y -q install $PKGS >/dev/null`

const pgBouncerInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq $PKGS >/dev/null`

// pgBouncerPrepDirsScript makes the runtime + log directories and the pgbouncer user
// exist before any config is written into them. The package creates most of this,
// but not on every OS and not always with the ownership a non-packaged unit needs,
// and a missing /etc/pgbouncer makes Docker's copy API 404 rather than create it.
const pgBouncerPrepDirsScript = `set -e
id pgbouncer >/dev/null 2>&1 || useradd -r -m -d /var/lib/pgbouncer -s /sbin/nologin pgbouncer
mkdir -p /etc/pgbouncer /var/log/pgbouncer /var/run/pgbouncer /var/lib/pgbouncer
chown -R pgbouncer:pgbouncer /etc/pgbouncer /var/log/pgbouncer /var/run/pgbouncer /var/lib/pgbouncer
# /var/run is a tmpfs in a systemd container, so the runtime dir has to be recreated
# on every boot — a tmpfiles rule rather than a mkdir the next restart forgets.
echo 'd /var/run/pgbouncer 0755 pgbouncer pgbouncer -' >/etc/tmpfiles.d/pgbouncer.conf
systemd-tmpfiles --create /etc/tmpfiles.d/pgbouncer.conf >/dev/null 2>&1 || true`

// pgBouncerCertScript signs a server cert + key from the staged Intranet CA into
// /etc/pgbouncer ($DIR, pgbouncer-owned). Same shape as pgCertScript; only the owner
// differs, because PgBouncer drops to its own user and must be able to read the key.
const pgBouncerCertScript = `set -e
case "$UNIT" in
  minutes) SECS=$((VALUE*60));;
  hours)   SECS=$((VALUE*3600));;
  *)       SECS=$((VALUE*86400));;
esac
END=$(date -u -d "+$SECS seconds" +%Y%m%d%H%M%SZ)
CA=/tmp/dbca-ca.crt; CAKEY=/tmp/dbca-ca.key
[ -f "$CA" ] && [ -f "$CAKEY" ] || { echo "CA material missing"; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "openssl not installed in this image"; exit 1; }
mkdir -p "$DIR"
cp -f "$CA" "$DIR/ca.crt"
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/server.key" -out /tmp/s.csr -subj "/O=DBCanvas/CN=$FQDN" >/dev/null
` + serverCertExtScript + `openssl x509 -req -in /tmp/s.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/server.crt" -extfile /tmp/dbca-san.ext -not_after "$END" >/dev/null
chown pgbouncer:pgbouncer "$DIR/ca.crt" "$DIR/server.crt" "$DIR/server.key"
chmod 600 "$DIR/server.key"
chmod 644 "$DIR/ca.crt" "$DIR/server.crt"
rm -f /tmp/dbca-ca.crt /tmp/dbca-ca.key /tmp/s.csr /tmp/dbca-san.ext /tmp/dbca-ca.srl`

// pgBouncerClientCertScript signs a client certificate + key for $ROLE from the
// staged Intranet CA. Root-owned: this is material to hand to somebody, not material
// the pool reads.
const pgBouncerClientCertScript = `set -e
case "$UNIT" in
  minutes) SECS=$((VALUE*60));;
  hours)   SECS=$((VALUE*3600));;
  *)       SECS=$((VALUE*86400));;
esac
END=$(date -u -d "+$SECS seconds" +%Y%m%d%H%M%SZ)
CA=/tmp/dbca-ca.crt; CAKEY=/tmp/dbca-ca.key
[ -f "$CA" ] && [ -f "$CAKEY" ] || { echo "CA material missing"; exit 1; }
mkdir -p "$DIR"
cp -f "$CA" "$DIR/ca.crt"
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/$ROLE.key" -out /tmp/c.csr -subj "/O=DBCanvas/CN=$ROLE" >/dev/null 2>&1
cat >/tmp/dbca-client.ext <<EXT
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=clientAuth
EXT
openssl x509 -req -in /tmp/c.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/$ROLE.crt" -extfile /tmp/dbca-client.ext -not_after "$END" >/dev/null 2>&1
chmod 700 "$DIR"
chmod 600 "$DIR/$ROLE.key"
chmod 644 "$DIR/$ROLE.crt" "$DIR/ca.crt"
rm -f /tmp/dbca-ca.crt /tmp/dbca-ca.key /tmp/c.csr /tmp/dbca-client.ext /tmp/dbca-ca.srl`

// pgBouncerStartScript validates the config, makes sure a unit exists, then enables
// and (re)starts the pooler. The unit is written only when the package did not ship
// one, so a packaged unit keeps whatever hardening it came with; the one written here
// is deliberately minimal and, crucially, has an ExecReload — SIGHUP is how the role
// watcher installs a new backend list without dropping client connections.
const pgBouncerStartScript = `set -e
chown -R pgbouncer:pgbouncer /etc/pgbouncer
chmod 640 /etc/pgbouncer/pgbouncer.ini /etc/pgbouncer/databases.ini
[ -f /etc/pgbouncer/userlist.txt ] && chmod 600 /etc/pgbouncer/userlist.txt
[ -f /etc/pgbouncer/backend.env ] && chmod 600 /etc/pgbouncer/backend.env
if ! systemctl cat pgbouncer >/dev/null 2>&1; then
cat >/etc/systemd/system/pgbouncer.service <<'UNIT'
[Unit]
Description=PgBouncer connection pooler for PostgreSQL
After=network-online.target
[Service]
Type=simple
User=pgbouncer
Group=pgbouncer
RuntimeDirectory=pgbouncer
ExecStart=/usr/bin/pgbouncer /etc/pgbouncer/pgbouncer.ini
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT
fi
systemctl daemon-reload
systemctl enable pgbouncer >/dev/null 2>&1 || true
systemctl reset-failed pgbouncer 2>/dev/null || true
systemctl restart pgbouncer
sleep 2
systemctl is-active --quiet pgbouncer || { echo "pgbouncer failed to start:"; journalctl -u pgbouncer --no-pager 2>/dev/null | tail -20; tail -20 /var/log/pgbouncer/pgbouncer.log 2>/dev/null; exit 1; }`

// pgBouncerWatchScript re-points the pool at whichever member currently holds the
// role, and reloads PgBouncer only when the answer changed.
//
// How "who can take writes" is asked differs by topology, and each question is the
// one that topology actually answers:
//
//	patroni — the Patroni REST API (:8008 /primary and /replica), which is the same
//	          endpoint the HAProxy node health-checks. It is authoritative in a way a
//	          SQL query is not: a leader that has lost its DCS lease stops answering
//	          /primary before PostgreSQL itself notices.
//	repmgr  — pg_is_in_recovery() over psql. repmgr has no REST API, and this is what
//	          repmgrCheckServiceScript asks on the member itself.
//	spock   — every member is a writer, so there is no role: the first member that
//	          answers at all keeps the wildcard pool, and it only moves when that one
//	          stops answering. Pinning one writer is not a limitation of Spock — it is
//	          how you avoid two nodes writing the same rows and spending the demo on
//	          conflict resolution.
//
// It never writes an empty backend list: with no member answering, the last
// known-good databases.ini is left exactly as it is, because a pool pointing at a
// server that is down recovers by itself when the server comes back, while a pool
// pointing at nothing needs a human.
const pgBouncerWatchScript = `#!/bin/bash
# DBCanvas — re-point PgBouncer at the member that can currently take writes.
set -u
CONFDIR=/etc/pgbouncer
. "$CONFDIR/backend.env"
DEST="$CONFDIR/databases.ini"
TMP=$(mktemp) || exit 1
trap 'rm -f "$TMP"' EXIT
export PGPASSWORD PGCONNECT_TIMEOUT=3

alive() { # $1 host — can we open a session at all?
  psql -qtAX -h "$1" -p "$PGPORT" -U "$PGUSER" -d postgres -c 'SELECT 1' >/dev/null 2>&1
}
in_recovery() { # $1 host — "t" standby, "f" primary, "" unreachable
  psql -qtAX -h "$1" -p "$PGPORT" -U "$PGUSER" -d postgres -c 'SELECT pg_is_in_recovery()' 2>/dev/null | tr -d '[:space:]'
}
rest() { # $1 host, $2 path — 0 when Patroni answers 200, 2 when there is no curl
  # -f (fail on >=400) without -S: a replica answering 503 to /primary is the API
  # working, not an error, and -S would write one to the journal every probe.
  if command -v curl >/dev/null 2>&1; then
    curl -fs -m 3 -o /dev/null "http://$1:$RESTPORT/$2"
  else
    return 2
  fi
}

PRIMARY=""
REPLICAS=""
case "$MODE" in
  patroni)
    for h in $MEMBERS; do
      rest "$h" primary; rc=$?
      if [ "$rc" -eq 0 ]; then
        [ -z "$PRIMARY" ] && PRIMARY="$h"
        continue
      fi
      # Exit 2 means curl is missing, not that the member is a replica: fall back to
      # asking PostgreSQL directly rather than silently treating the cluster as
      # leaderless and leaving the pool where it was.
      if [ "$rc" -eq 2 ]; then
        case "$(in_recovery "$h")" in
          f) [ -z "$PRIMARY" ] && PRIMARY="$h" ;;
          t) REPLICAS="$REPLICAS $h" ;;
        esac
        continue
      fi
      rest "$h" replica && REPLICAS="$REPLICAS $h"
    done
    ;;
  repmgr)
    for h in $MEMBERS; do
      case "$(in_recovery "$h")" in
        f) [ -z "$PRIMARY" ] && PRIMARY="$h" ;;
        t) REPLICAS="$REPLICAS $h" ;;
      esac
    done
    ;;
  spock)
    for h in $MEMBERS; do
      alive "$h" || continue
      if [ -z "$PRIMARY" ]; then PRIMARY="$h"; else REPLICAS="$REPLICAS $h"; fi
    done
    ;;
  *)
    exit 0 ;;
esac

if [ -z "$PRIMARY" ]; then
  echo "no writable member answered — leaving $DEST unchanged" >&2
  exit 1
fi

{
  echo "[databases]"
  echo "* = host=$PRIMARY port=$PGPORT"
  case "$ROUTING" in
    rw-ro)
      RO=$(echo $REPLICAS | awk '{print $1}')
      [ -n "$RO" ] || RO="$PRIMARY"
      echo "${DB}_ro = host=$RO port=$PGPORT dbname=$DB"
      ;;
    mesh)
      for h in $MEMBERS; do
        a=${h%%.*}; a=${a//-/_}
        echo "${DB}_${a} = host=$h port=$PGPORT dbname=$DB"
      done
      ;;
  esac
} >"$TMP"

if cmp -s "$TMP" "$DEST"; then
  exit 0
fi
install -o pgbouncer -g pgbouncer -m 0640 "$TMP" "$DEST"
PID=$(systemctl show -p MainPID --value pgbouncer 2>/dev/null)
if [ -n "$PID" ] && [ "$PID" != "0" ]; then
  kill -HUP "$PID" && echo "pool re-pointed at $PRIMARY (reloaded)"
else
  echo "pool re-pointed at $PRIMARY (pgbouncer not running yet)"
fi`

// pgBouncerWatchOnceScript runs the watcher a single time during provisioning, so
// PgBouncer starts already pointing at the current leader.
const pgBouncerWatchOnceScript = `set -e
/usr/local/bin/dbcanvas-pgbouncer-watch`

// pgBouncerWatchTimerScript installs and starts the periodic watcher. A timer rather
// than a daemon: the work is one probe per member and it must survive its own
// failures, which is exactly what a oneshot-plus-timer gives for free.
const pgBouncerWatchTimerScript = `set -e
cat >/etc/systemd/system/dbcanvas-pgbouncer-watch.service <<'UNIT'
[Unit]
Description=DBCanvas — re-point PgBouncer at the writable member
After=pgbouncer.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/dbcanvas-pgbouncer-watch
UNIT
cat >/etc/systemd/system/dbcanvas-pgbouncer-watch.timer <<UNIT
[Unit]
Description=DBCanvas — poll the PostgreSQL backend for a role change
[Timer]
OnBootSec=${INTERVAL}s
OnUnitActiveSec=${INTERVAL}s
AccuracySec=1s
Unit=dbcanvas-pgbouncer-watch.service
[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl reset-failed dbcanvas-pgbouncer-watch.timer 2>/dev/null || true
systemctl enable --now dbcanvas-pgbouncer-watch.timer >/dev/null`

// ------------------------------------------------------ application simulators

// pgBouncerDSNExtra is the libpq/pgx connection-string parameter an application has
// to add when it connects through a pool that is NOT session-pooling.
//
// Both PostgreSQL simulators here are pgx programs, and pgx's default execution mode
// prepares every statement on the server under a generated name and caches it per
// connection. In transaction (or statement) pooling that connection is a *different*
// server connection on the next transaction, so the cached name does not exist there
// and the query fails with "prepared statement \"stmtcache_1\" does not exist" —
// intermittently, under load, which is the worst way to find out. simple_protocol
// sends the SQL unprepared instead, which is exactly what a pooled client should do.
//
// Session pooling holds one server connection for the life of the client session, so
// prepared statements survive and nothing needs adding.
func pgBouncerDSNExtra(poolMode string) string {
	if poolMode == "session" {
		return ""
	}
	return "default_query_exec_mode=simple_protocol"
}

// pgBouncerSimEndpoint resolves a PgBouncer node that an application simulator is
// linked to, down to a connectable endpoint plus the backend's credentials — the
// PostgreSQL equivalent of the HAProxy branch each sim already has, shared by Car
// Rental Sim and Stock Market Sim so the two cannot disagree about what a pooled
// connection needs.
//
// The account is the backend's superuser (a pool authenticates the client and then
// reuses its own server connections, so it has no accounts of its own), and there is
// no read endpoint to return: unlike HAProxy, a pool's read/write split is a database
// name and not a second port, and a sim asking for the wildcard pool always lands on
// the member that can take writes — which is what a sim wants and what makes it
// survive a failover without reconnecting to a different address.
func (a *App) pgBouncerSimEndpoint(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, nodeID string, timeout time.Duration) (host string, port int, sec pgSecrets, kind, dsnExtra, name string, err error) {
	backKind, frame, backNode, ok := pgBouncerBackend(doc, nodeID)
	if !ok {
		return "", 0, pgSecrets{}, "", "", "", fmt.Errorf(
			"the linked PgBouncer node does not pool for exactly one PostgreSQL backend — draw the association from the node or cluster to it, or link this sim to the backend directly")
	}
	switch backKind {
	case "pg":
		_, sec, err = a.waitPgNodeRunning(ctx, st.ID, backNode.ID, hosts, domain, timeout)
	case "patroni":
		_, sec, err = a.waitPatroniRunning(ctx, st.ID, frame, doc, domain, timeout)
	case "repmgr":
		_, sec, err = a.waitRepmgrRunning(ctx, st.ID, frame, doc, domain, timeout)
	case "spock":
		_, sec, err = a.waitSpockRunning(ctx, st.ID, frame, doc, domain, timeout)
	default:
		err = fmt.Errorf("unsupported PgBouncer backend %q", backKind)
	}
	if err != nil {
		return "", 0, pgSecrets{}, "", "", "", err
	}
	if !a.waitNodeRunning(st.ID, nodeID, timeout) {
		return "", 0, pgSecrets{}, "", "", "", fmt.Errorf("linked PgBouncer node did not become ready within %s", timeout)
	}
	// The pool's own options decide the one thing the client has to know.
	var self designNode
	for _, n := range doc.Nodes {
		if n.ID == nodeID {
			self = n
		}
	}
	opt := pgBouncerDefaults(self, backKind)
	return fqdnOf(hosts[nodeID], domain), pgBouncerPort, sec,
		"pgbouncer-" + backKind, pgBouncerDSNExtra(opt.PgbPoolMode), nodeLabel(doc, nodeID), nil
}

// ------------------------------------------------------------------ validation

// pgBouncerLinkedSims are the application-simulator nodes driven through this pool.
// They are the one class of client that cannot be handed a certificate — each one
// builds its own connection string from what the provisioner resolved — so under
// cert authentication they are a refusal rather than a warning.
func pgBouncerLinkedSims(n designNode, doc designDoc) []string {
	sim := map[string]bool{"carsim": true, "stocksim": true, "ledgersim": true}
	var out []string
	for _, e := range doc.Edges {
		var other string
		switch n.ID {
		case e.From.Node:
			other = e.To.Node
		case e.To.Node:
			other = e.From.Node
		default:
			continue
		}
		for _, m := range doc.Nodes {
			if m.ID == other && sim[m.Type] {
				out = append(out, m.Label)
			}
		}
	}
	sort.Strings(out)
	return out
}

// pgBouncerIssues validates one PgBouncer node at Validate time: the association
// rule (exactly one backend), and the handful of options whose wrong value would
// otherwise only show up as a pooler that refuses to start.
func pgBouncerIssues(n designNode, doc designDoc) []issue {
	var out []issue
	who := "PgBouncer node " + n.Label
	kinds, _, _ := pgBouncerBackends(doc, n.ID)
	switch {
	case len(kinds) == 0:
		out = append(out, issue{Level: "error", Message: who +
			" must be linked to a PostgreSQL node or a Patroni, repmgr or Spock cluster — draw an association line from one to it"})
	case len(kinds) > 1:
		sorted := append([]string(nil), kinds...)
		sort.Strings(sorted)
		out = append(out, issue{Level: "error", Message: who +
			" can pool for only one backend — remove the extra association (linked to: " + strings.Join(sorted, ", ") + ")"})
	}
	backend := ""
	if len(kinds) == 1 {
		backend = kinds[0]
	}
	opt := pgBouncerDefaults(n, backend)
	switch opt.PgbPoolMode {
	case "session", "transaction", "statement":
	default:
		out = append(out, issue{Level: "error", Message: who + ": pool mode " + opt.PgbPoolMode + " is not one of session, transaction, statement"})
	}
	switch opt.PgbAuthType {
	case "scram-sha-256", "md5", "trust":
	case "cert":
		// PgBouncer refuses to start without a certificate to verify against —
		// "auth_type=cert requires client_tls_sslmode=SSLMODE_VERIFY_FULL" — so this
		// is a refusal at Validate rather than a node that fails at deploy.
		if !n.GenerateCert {
			out = append(out, issue{Level: "error", Message: who +
				": certificate authentication needs a certificate — tick \"Generate certificate from Intranet CA\", or choose another auth type"})
		}
		// Nothing without a client certificate can get in, and an application
		// simulator has no way to present one: it would fail at connect with
		// "tlsv13 alert certificate required", after deploying successfully.
		if sims := pgBouncerLinkedSims(n, doc); len(sims) > 0 {
			out = append(out, issue{Level: "error", Message: who +
				": certificate authentication refuses any client without a certificate, and " +
				strings.Join(sims, ", ") + " cannot present one — drive this backend directly, or choose another auth type"})
		}
		if opt.PgbAuthQuery {
			out = append(out, issue{Level: "warn", Message: who +
				": auth_query does nothing under certificate authentication (pg_shadow returns a SCRAM verifier, which cannot be used to authenticate to the server) — only roles in userlist.txt can connect"})
		}
	default:
		out = append(out, issue{Level: "error", Message: who + ": auth type " + opt.PgbAuthType + " is not one of scram-sha-256, md5, cert, trust"})
	}
	switch opt.PgbRouting {
	case "rw", "rw-ro", "mesh":
	default:
		out = append(out, issue{Level: "error", Message: who + ": routing " + opt.PgbRouting + " is not one of rw, rw-ro, mesh"})
	}
	if backend == "pg" && opt.PgbRouting != "rw" {
		out = append(out, issue{Level: "warn", Message: who +
			": a standalone PostgreSQL backend has one server, so the read-only alias and the per-member aliases all point back at it"})
	}
	if backend == "spock" && opt.PgbRouting == "rw-ro" {
		out = append(out, issue{Level: "warn", Message: who +
			": every Spock member is writable, so a read-only alias is not read-only — \"mesh\" is the routing that fits multi-master"})
	}
	if opt.PgbReservePoolSize > 0 && opt.PgbReservePoolSize > opt.PgbDefaultPoolSize {
		out = append(out, issue{Level: "warn", Message: who + ": the reserve pool is larger than the pool it backs up"})
	}
	if opt.PgbMaxDBConnections > 0 && opt.PgbMaxDBConnections < opt.PgbDefaultPoolSize {
		out = append(out, issue{Level: "warn", Message: who +
			": max_db_connections is below default_pool_size, so the pool can never fill"})
	}
	if opt.PgbAuthType == "trust" {
		out = append(out, issue{Level: "warn", Message: who +
			": trust authentication accepts any user name without a password — fine for a lab, never past one"})
	}
	if opt.PgbMinPoolSize > opt.PgbDefaultPoolSize {
		out = append(out, issue{Level: "warn", Message: who + ": min_pool_size is above default_pool_size"})
	}

	// The server leg. A backend whose pg_hba puts `hostssl … cert` first judges the
	// pool's own connection by certificate — the pool connects over TLS, so it
	// matches that rule — and refuses it unless the pool has one to present. The
	// pool's certificate is what provides it, so a pool without one cannot reach
	// such a backend at all. Reported here rather than discovered as
	// "server login failed: FATAL connection requires a valid client certificate"
	// in a log, which reads like a client problem and is not one.
	if kind, frame, backNode, ok := pgBouncerBackend(doc, n.ID); ok {
		backAuth, backWho := frame.PGHostAuth, "the "+kind+" backend"
		if kind == "pg" {
			backAuth = backNode.PGHostAuth
		}
		if pgHostAuthRequiresClientCert(backAuth) && !n.GenerateCert && opt.PgbServerTLS != "disable" {
			out = append(out, issue{Level: "error", Message: who +
				": " + backWho + " authenticates TLS clients by certificate, and this pool has none to present — tick \"Generate certificate from Intranet CA\" on it, or set server_tls_sslmode to disable so it connects without TLS and reaches the password rule"})
		}
	}
	return out
}
