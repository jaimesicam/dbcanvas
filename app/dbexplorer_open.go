package main

// Database Explorer — opening a connection, and cancelling a query.
//
// Everything above this point describes a database; this is where DBCanvas actually
// reaches one. Two routes, and which one a target takes is a fact about the
// deployment rather than a preference:
//
//	network   the app joins the stack's Docker network and dials the node's address
//	          on it, exactly as the Query Runner, the Benchmark and the Mongo Data
//	          Generator do. No port needs to be published to the host, which is the
//	          whole reason this works on a stack nobody exported anything from.
//	exec      the database listens on loopback inside its container and cannot be
//	          dialled at all, so its own client is run in there. PMM's two are the
//	          only ones today; see dbexplorer_pmm.go.
//
// The stack-network join is deliberately not done when listing connections — only
// when one is opened. Attaching the app's container to a network is disruptive enough
// that the Query Runner defers it off the request path for the same reason.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// dexUnlock decides whether a submission may write to a connection that is
// read-only by policy, and it is the only place that decision is made.
//
// Three things must all be true, and they are checked in this order because each is
// cheaper and more specific than the last: the connection has to be one whose
// read-only-ness is a policy at all (a replica is read-only because it is a replica,
// and no setting changes that); an administrator has to have unlocked writes for the
// installation; and this particular submission has to have asked. A tab, a saved
// query or a replayed request that predates the setting carries no AllowWrites, so
// none of them can write by accident.
func (a *App) dexUnlock(t dexTarget, allowWrites bool) bool {
	return t.Conn.Policy == dexPMMPolicy && allowWrites && a.internalWritesAllowed()
}

// dexOpenUnlocked builds an adapter with the read-only policy lifted, for a target
// that dexUnlock has approved.
func (a *App) dexOpenUnlocked(ctx context.Context, t dexTarget) (dexAdapter, error) {
	t.Conn.ReadOnly = false
	t.Conn.Caps = dexCapsFor(t.Conn.Engine, false)
	return a.dexOpen(ctx, t)
}

// dexOpen builds the adapter for a resolved target. The caller closes it.
func (a *App) dexOpen(ctx context.Context, t dexTarget) (dexAdapter, error) {
	switch t.Conn.Engine {
	case dexMySQL:
		return a.dexOpenMySQL(ctx, t)
	case dexPostgres:
		return a.dexOpenPostgres(ctx, t)
	case dexMongoDB:
		return a.dexOpenMongo(ctx, t)
	case dexValkey:
		return a.dexOpenValkey(ctx, t)
	case dexClickHouse:
		return a.dexOpenClickHouse(ctx, t)
	}
	return nil, fmt.Errorf("%s is not a supported engine", t.Conn.Engine)
}

// dexNodeAddr resolves the address to dial on the stack network, joining it first
// when the app is containerized. Shared by every network-transport adapter.
func (a *App) dexNodeAddr(ctx context.Context, t dexTarget) (string, error) {
	// A Kubernetes endpoint already carries its address: a MetalLB LoadBalancer IP or
	// a k3s node's address with a NodePort, both on the stack's own subnet. There is
	// no container of its own to resolve — the database is a pod behind a Service.
	if t.K8s != nil {
		if !t.K8s.Reachable() {
			return "", fmt.Errorf("%s", t.K8s.Why)
		}
		if err := a.joinStackForDial(ctx, a.docker, networkName(t.Conn.StackID)); err != nil {
			return "", fmt.Errorf("could not join the stack network: %v", err)
		}
		return fmt.Sprintf("%s:%d", t.K8s.Addr, t.K8s.Port), nil
	}
	netName := networkName(t.Conn.StackID)
	eng := a.dialEngine(t.Conn.StackID, t.ContainerID)
	if err := a.joinStackForDial(ctx, eng, netName); err != nil {
		return "", fmt.Errorf("could not join the stack network: %v", err)
	}
	ip, err := eng.ContainerIP(ctx, t.ContainerID, netName)
	if err != nil || ip == "" {
		return "", fmt.Errorf("could not resolve the node's address on the stack network")
	}
	return fmt.Sprintf("%s:%d", ip, t.Port), nil
}

func (a *App) dexOpenMySQL(ctx context.Context, t dexTarget) (dexAdapter, error) {
	addr, err := a.dexNodeAddr(ctx, t)
	if err != nil {
		return nil, err
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd = t.User, t.Pass
	cfg.Net, cfg.Addr = "tcp", addr
	cfg.Timeout = 15 * time.Second
	cfg.AllowNativePasswords = true
	cfg.ParseTime = true
	cfg.MultiStatements = false // statements are split here, so the server never needs to
	cfg.Params = map[string]string{"sql_mode": "''"}
	// TLS. "preferred" is opportunistic: encrypt if the server offers it, do not
	// claim to have identified it. A node deployed with a stack-CA certificate gets
	// the real thing instead — the chain verified against that CA, with the hostname
	// check left out because the app has to dial an address (dbexplorer_tls.go).
	//
	// The condition is the node's own certificate and not merely the existence of a
	// CA: a node deployed without one is not listening for TLS, and demanding it
	// there would not be stricter, it would fail to connect at all.
	cfg.TLSConfig = "preferred"
	if ca := a.dexStackCA(ctx, t.Stack); t.TLSVerify && len(ca) > 0 {
		if tc := dexChainOnlyTLS(ca); tc != nil {
			name := fmt.Sprintf("dbcanvas-dex-%d", t.Conn.StackID)
			if err := mysql.RegisterTLSConfig(name, tc); err == nil {
				cfg.TLSConfig = name
			}
		}
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, dexMySQLError(err, nil)
	}
	return &dexMySQLAdapter{db: db, caps: t.Conn.Caps}, nil
}

func (a *App) dexOpenPostgres(ctx context.Context, t dexTarget) (dexAdapter, error) {
	// A ClusterIP database has no address, so psql runs inside one of its own pods.
	// The runner wraps the command in `kubectl exec` and runs that on the k3s server
	// container, which is where the admin kubeconfig lives — so the transport below
	// is the same one PMM uses, pointed somewhere else.
	if t.K8s != nil && t.Conn.Transport == "exec" {
		db := t.K8s.Database
		if db == "" {
			db = "postgres"
		}
		return &dexPGAdapter{
			t: &dexPGExec{
				run: dexK8sExec{
					eng: a.docker, id: t.K8s.ServerID, ep: *t.K8s,
				},
				user: t.User, ro: t.Conn.ReadOnly,
			},
			defaultDB: db, caps: t.Conn.Caps,
		}, nil
	}
	if t.Conn.Transport == "exec" {
		return &dexPGAdapter{
			t: &dexPGExec{
				run:  dexContainerExec{eng: a.dialEngine(t.Conn.StackID, t.ExecContainer), id: t.ExecContainer},
				user: t.User, ro: t.Conn.ReadOnly,
			},
			// pmm-managed is the database worth opening on: it is the inventory.
			defaultDB: "pmm-managed",
			caps:      t.Conn.Caps,
		}, nil
	}
	addr, err := a.dexNodeAddr(ctx, t)
	if err != nil {
		return nil, err
	}
	caFile := ""
	if t.TLSVerify {
		caFile = a.dexStackCAFile(ctx, t.Stack)
	}
	// A PostgreSQL operator terminates TLS on every Service and refuses a plaintext
	// client outright ("SSL required"), so `prefer` is not good enough there. The
	// certificate is the cluster's own, issued by the operator rather than by the
	// stack CA, and it names an in-cluster Service — so this encrypts without
	// claiming to have identified the server, and the connection says so.
	k8sTLS := t.K8s != nil && t.K8s.TLS != ""
	mk := func(database string) string {
		if database == "" {
			database = "postgres"
		}
		q := "connect_timeout=15"
		if k8sTLS && caFile == "" {
			q += "&sslmode=" + t.K8s.TLS
		} else if caFile != "" {
			// verify-ca, not verify-full: the chain is checked against the stack CA,
			// and the hostname is not, because the app has to dial an address rather
			// than the name the certificate carries. This is a real check, not a
			// disabled one.
			//
			// It is only reachable for a node deployed with a certificate. libpq has
			// no "verify if TLS happens to be negotiated" mode, so asking for it
			// against a PostgreSQL with ssl off does not harden the connection — the
			// server refuses TLS and there is no connection at all.
			q += "&sslmode=verify-ca&sslrootcert=" + url.QueryEscape(caFile)
		} else {
			q += "&sslmode=prefer"
		}
		return (&url.URL{
			Scheme: "postgres", User: url.UserPassword(t.User, t.Pass),
			Host: addr, Path: "/" + database, RawQuery: q,
		}).String()
	}
	defaultDB := "postgres"
	if t.K8s != nil && t.K8s.Database != "" {
		// A pooler only routes to the databases it was configured for, so opening on
		// "postgres" would fail on the endpoint most likely to be picked.
		defaultDB = t.K8s.Database
	}
	ad := &dexPGAdapter{t: &dexPGNet{dsn: mk}, defaultDB: defaultDB, caps: t.Conn.Caps}
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := ad.TestConnection(pingCtx); err != nil {
		ad.Close()
		return nil, err
	}
	return ad, nil
}

func (a *App) dexOpenMongo(ctx context.Context, t dexTarget) (dexAdapter, error) {
	addr, err := a.dexNodeAddr(ctx, t)
	if err != nil {
		return nil, err
	}
	// directConnection=true for the same reason the Mongo Data Generator uses it:
	// Docker's embedded DNS does not resolve the Intranet's member hostnames, so the
	// driver's own replica-set discovery would find members it cannot reach. The
	// endpoint is still presented as the set — this is how it is reached, not what
	// it is.
	authDB := t.AuthDB
	if authDB == "" {
		authDB = "admin"
	}
	uri := fmt.Sprintf("mongodb://%s:%s@%s/?authSource=%s&directConnection=true",
		url.QueryEscape(t.User), url.QueryEscape(t.Pass), addr, url.QueryEscape(authDB))
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(cctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, dexMongoError(err, nil)
	}
	closer := func() { _ = client.Disconnect(context.Background()) }
	if err := client.Ping(cctx, nil); err != nil {
		closer()
		return nil, dexMongoError(err, nil)
	}
	return &dexMongoAdapter{client: client, closer: closer, caps: t.Conn.Caps}, nil
}

func (a *App) dexOpenValkey(ctx context.Context, t dexTarget) (dexAdapter, error) {
	addr, err := a.dexNodeAddr(ctx, t)
	if err != nil {
		return nil, err
	}
	ad := &dexValkeyAdapter{addr: addr, pass: t.Pass, caps: t.Conn.Caps}
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := ad.TestConnection(pingCtx); err != nil {
		return nil, err
	}
	return ad, nil
}

func (a *App) dexOpenClickHouse(ctx context.Context, t dexTarget) (dexAdapter, error) {
	if t.Conn.Transport == "exec" {
		eng := a.dialEngine(t.Conn.StackID, t.ExecContainer)
		creds := a.dexPMMClickHouseCreds(ctx, eng, t.ExecContainer)
		return &dexCHAdapter{
			t: &dexCHExec{
				run:  dexContainerExec{eng: eng, id: t.ExecContainer},
				user: creds.User, pass: creds.Pass,
			},
			defaultDB: creds.DB, readOnly: t.Conn.ReadOnly, caps: t.Conn.Caps,
		}, nil
	}
	// The network transport, for a ClickHouse node on the canvas. Nothing provisions
	// one today; the path exists so that when something does, the adapter is already
	// the generic one rather than PMM's.
	addr, err := a.dexNodeAddr(ctx, t)
	if err != nil {
		return nil, err
	}
	db := t.CHDB
	if db == "" {
		db = "default"
	}
	return &dexCHAdapter{
		t: &dexCHHTTP{
			base: "http://" + addr, user: t.User, pass: t.Pass,
			client: &http.Client{Timeout: 5 * time.Minute},
		},
		defaultDB: db, readOnly: t.Conn.ReadOnly, caps: t.Conn.Caps,
	}, nil
}

// ---------------------------------------------------------------- cancellation

// dexRun is one in-flight query, registered so Cancel has something to name. The
// owner is recorded because a cancel request is authorized the same way everything
// else here is: you may only cancel your own.
type dexRun struct {
	ownerID int64
	cancel  context.CancelFunc
	started time.Time
}

var dexRuns = struct {
	sync.Mutex
	m map[string]*dexRun
}{m: map[string]*dexRun{}}

// dexRegisterRun records an in-flight query under the id the client chose. The id is
// client-supplied on purpose: the browser needs to be able to cancel a query whose
// response has not arrived yet, so it cannot wait for the server to name it.
func dexRegisterRun(id string, ownerID int64, cancel context.CancelFunc) {
	if id == "" {
		return
	}
	dexRuns.Lock()
	defer dexRuns.Unlock()
	dexRuns.m[id] = &dexRun{ownerID: ownerID, cancel: cancel, started: time.Now()}
	// A run whose handler died without unregistering would leak a cancel func
	// forever; anything older than the maximum query timeout cannot still be live.
	cutoff := time.Now().Add(-2 * dexMaxTimeoutS * time.Second)
	for k, v := range dexRuns.m {
		if v.started.Before(cutoff) {
			delete(dexRuns.m, k)
		}
	}
}

func dexUnregisterRun(id string) {
	if id == "" {
		return
	}
	dexRuns.Lock()
	delete(dexRuns.m, id)
	dexRuns.Unlock()
}

// dexCancelRun cancels one in-flight query if the caller owns it. Cancelling the
// context is what actually stops the work: pgx sends a CancelRequest on its own
// connection, the MySQL driver closes the connection so the server reaps the
// statement, the MongoDB driver kills the cursor, and the exec transports kill the
// client process inside the container.
func dexCancelRun(id string, u User) bool {
	dexRuns.Lock()
	r := dexRuns.m[id]
	if r == nil || (r.ownerID != u.ID && u.Role != RoleAdmin) {
		dexRuns.Unlock()
		return false
	}
	delete(dexRuns.m, id)
	dexRuns.Unlock()
	r.cancel()
	return true
}

// dexSafeID bounds a client-supplied run id. It is only ever a map key, but a
// megabyte of it would still be a megabyte held until the query finished.
func dexSafeID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

// dexK8sExec runs a database client inside an operator's pod.
//
// It satisfies the same dexExecRunner seam the PMM transports use, which is the whole
// reason a ClusterIP-only database needs no new adapter: the PostgreSQL adapter cannot
// tell whether its psql is running in a PMM container or in a pod three layers inside
// a k3s cluster, and does not need to.
type dexK8sExec struct {
	eng Engine
	id  string // the k3s server container, where kubectl and the kubeconfig live
	ep  k8sEndpoint
}

func (d dexK8sExec) run(ctx context.Context, argv, env []string, stdin []byte) (ExecResult, error) {
	cmd := d.ep.k8sExecArgv(argv)
	// The kubeconfig is k3s's own, in the place k3s puts it — the same one every
	// other kubectl call in the app uses (see k3d.go).
	env = append(append([]string{}, env...), "KUBECONFIG="+k3dKubeconfig)
	if len(stdin) > 0 {
		return d.eng.ExecInput(ctx, d.id, "", cmd, env, stdin)
	}
	return d.eng.Exec(ctx, d.id, cmd, env)
}
