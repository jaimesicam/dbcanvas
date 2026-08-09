package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Stock Market Sim (Type=="stocksim"): a small stock-exchange application with
// full browser CRUD, a live dashboard and a printable report, running against
// whichever database it was pointed at. Runs dbcanvas's own first-party
// dbcanvas-stocksim:latest image (built by `make stocksim-image`), not a
// systemd OS image — no product installed on top, no PMM monitoring. Its
// dashboard port is published to the host on a fixed, auto-assigned port that's
// reused across a redeploy, so it's reachable directly from the host browser
// with no VNC desktop needed.
//
// Two things make it unlike its five sibling simulators.
//
// First, it is not tied to one database engine. The same image speaks MySQL,
// PostgreSQL, MongoDB and Valkey; which one is decided here, at deploy, and
// passed in as DB_ENGINE — either from the linked node's own type, or from the
// engine chosen on the node's form.
//
// Second, it does not have to be linked to anything on the canvas. In "linked"
// mode it resolves a drawn association line exactly as carsim/airlinesim do. In
// "manual" mode it takes connection options straight from the node's own form
// and waits for nothing, so it can drive a database that is not part of the
// stack — on the Docker host, on the LAN, or a managed cloud instance. That is
// why this is the only provisioner that sets ExtraHosts, and the only node type
// with a pre-deploy connection test (see handleStockSimTest).
const (
	stockSimImage = "dbcanvas-stocksim:latest"
	stockSimPort  = 8093
)

// stockSimImplementedEngines mirrors store.Implemented() inside the sim image.
// The node form offers exactly this set, so a user can never configure a target
// the binary would then refuse at startup. Adding an engine means changing both.
var stockSimImplementedEngines = []string{"mysql", "postgres", "mongodb", "valkey"}

func stockSimEngineImplemented(engine string) bool {
	for _, e := range stockSimImplementedEngines {
		if e == engine {
			return true
		}
	}
	return false
}

// stockSimDefaultPort mirrors store.DefaultPort in the sim image.
func stockSimDefaultPort(engine string) int {
	switch engine {
	case "mysql":
		return pxcMySQLPort
	case "postgres":
		return patroniPGPort
	case "mongodb":
		return mongoPort
	case "valkey":
		return valkeyPort
	}
	return 0
}

// stockSimReservedDatabases mirrors the sim image's own refusal list. Checking
// here too means the user is told at validation time rather than watching a
// container start and immediately fail.
var stockSimReservedDatabases = map[string]bool{
	"mysql": true, "sys": true, "information_schema": true, "performance_schema": true,
	"postgres": true, "template0": true, "template1": true,
	"admin": true, "local": true, "config": true,
}

// stockSimConfig is the non-secret profile shown for a deployed Stock Market
// Sim node. Credentials never appear here — see stockSimSecrets.
type stockSimConfig struct {
	Image      string `json:"image"`
	Hostname   string `json:"hostname"`
	FQDN       string `json:"fqdn"`
	Mode       string `json:"mode"`       // "linked" | "manual"
	Engine     string `json:"engine"`     // mysql | postgres | mongodb | valkey
	Database   string `json:"database"`   // schema / database / key prefix it owns
	TargetKind string `json:"targetKind"` // "ps" (linked) | "external-<engine>" (manual)
	TargetName string `json:"targetName"` // linked node label, or host:port
	HTTPPort   int    `json:"httpPort"`   // host port mapped to the container's dashboard port
}

// stockSimSecrets holds the credentials a manual-mode node was configured with,
// kept out of stockSimConfig so the node properties panel never renders them.
type stockSimSecrets struct {
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	DSN      string `json:"dsn,omitempty"`
}

// stockSimMode normalises the node's connection mode; an unset mode means
// linked, which is what every stack designed before manual mode existed gets.
func stockSimMode(n designNode) string {
	if n.SSMode == "manual" {
		return "manual"
	}
	return "linked"
}

func stockSimEngine(n designNode) string {
	if e := strings.ToLower(strings.TrimSpace(n.SSEngine)); e != "" {
		return e
	}
	return "mysql"
}

func stockSimDatabase(n designNode) string {
	if d := strings.TrimSpace(n.SSDatabase); d != "" {
		return d
	}
	return "stocksim"
}

// stockSimTarget resolves the coarse kind and target node id a stocksim node is
// linked to via a drawn edge. Mirrors carSimTarget's undirected-edge walk. Only
// standalone database nodes are matched — one per engine family: a Stock Market
// Sim node drives one database, and pointing it at a cluster's load balancer is
// what manual mode's host field is for.
func stockSimTarget(doc designDoc, startID string) (kind, targetID string, ok bool) {
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
			switch n.Type {
			case "ps", "pg", "psm", "valkey":
				return n.Type, n.ID, true
			}
		}
	}
	return "", "", false
}

// stockSimConfigError reports the first setting that the sim binary itself
// would refuse at startup, or "" if the configuration is at least startable.
// Mirrors store.Config.Validate inside the image — both this and
// stockSimIssues below are built on it so a node's validation, its
// Test-connection button and the container all agree.
func stockSimConfigError(n designNode) string {
	engine := stockSimEngine(n)
	if !stockSimEngineImplemented(engine) {
		return fmt.Sprintf("database engine %q is not implemented in this build yet — only %s available",
			engine, strings.Join(stockSimImplementedEngines, " or ")+" is")
	}
	if stockSimReservedDatabases[strings.ToLower(stockSimDatabase(n))] {
		return fmt.Sprintf(
			"refusing to use %q as this application's namespace: it is a reserved system database. Choose another name (the default is \"stocksim\")",
			stockSimDatabase(n))
	}
	if strings.TrimSpace(n.SSHost) == "" && strings.TrimSpace(n.SSDSN) == "" {
		return "no target: set a host to connect to, or a full connection string"
	}
	return ""
}

// stockSimIssues returns the validation problems for one stocksim node, in the
// user's own vocabulary. Split out of validateStack because manual mode has
// several distinct ways to be incomplete and inlining them all would bury the
// switch it lives in.
func stockSimIssues(n designNode) []issue {
	var out []issue
	engine := stockSimEngine(n)
	if !stockSimEngineImplemented(engine) {
		out = append(out, issue{"error", fmt.Sprintf(
			"Stock Market Sim node %s is set to %s, which this build does not implement yet — choose %s",
			n.Label, engine, strings.Join(stockSimImplementedEngines, " or "))})
	}
	if stockSimReservedDatabases[strings.ToLower(stockSimDatabase(n))] {
		out = append(out, issue{"error", fmt.Sprintf(
			"Stock Market Sim node %s cannot use %q as its database — that is a reserved system database. Choose another name (the default is \"stocksim\")",
			n.Label, stockSimDatabase(n))})
	}
	if strings.TrimSpace(n.SSHost) == "" && strings.TrimSpace(n.SSDSN) == "" {
		out = append(out, issue{"error",
			"Stock Market Sim node " + n.Label + " is set to a manual connection but has no host — enter one, or paste a full connection string under Advanced"})
	}
	if n.SSPort < 0 || n.SSPort > 65535 {
		out = append(out, issue{"error",
			"Stock Market Sim node " + n.Label + " has an invalid port — leave it blank for the engine default"})
	}
	// Not an error: dbcanvas cannot reach out and check an endpoint it does not
	// manage, so say so plainly rather than pretend the design is verified.
	out = append(out, issue{"info",
		"Stock Market Sim node " + n.Label + " connects to a database outside this stack — dbcanvas cannot verify it before deploying, so use Test connection on the node to check it now"})
	return out
}

// provisionStockSim records the deployment then brings up the sim container.
func (a *App) provisionStockSim(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	host := hosts[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	// Reuse the previously published host port across a redeploy so the
	// dashboard URL shown in node properties stays stable — mirrors
	// provisionPMM's own reused-host-port pattern.
	httpPort := 0
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old stockSimConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			httpPort = old.HTTPPort
		}
	}
	if httpPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpPort = p
		}
	}

	mode := stockSimMode(n)
	cfg := stockSimConfig{
		Image: stockSimImage, Hostname: host, FQDN: fqdn, HTTPPort: httpPort,
		Mode: mode, Engine: stockSimEngine(n), Database: stockSimDatabase(n),
	}

	coarseKind, targetID := "", ""
	if mode == "linked" {
		var ok bool
		coarseKind, targetID, ok = stockSimTarget(doc, n.ID)
		if !ok {
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployError, Config: mustJSON(cfg)})
			return
		}
	}
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: mustJSON(cfg)})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, stockSimImage); !ok {
			pr.fail("image %s not found — run `make stocksim-image` first", stockSimImage)
			return
		}

		pr.phase("Waiting for Intranet to be ready", 8)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		var env []string
		var sec stockSimSecrets
		var extraHosts []string

		if mode == "linked" {
			pr.phase("Waiting for linked database node", 20)
			e, werr := a.waitStockSimTarget(ctx, st, hosts, doc, domain, coarseKind, targetID, deployTimeout())
			if werr != nil {
				pr.fail("%v", werr)
				return
			}
			cfg.Engine, cfg.TargetKind, cfg.TargetName = e.engine, e.kind, e.displayName
			env, sec = e.env, e.secrets
			pr.logln(fmt.Sprintf("target: %s %s (%s:%d)", cfg.TargetKind, cfg.TargetName, e.host, e.port))
		} else {
			pr.phase("Using the connection configured on this node", 20)
			env, sec, cfg.TargetName = stockSimManualEnv(n)
			cfg.TargetKind = "external-" + cfg.Engine
			// Only manual mode gets host.docker.internal: it is the one case
			// where the database may be running on the Docker host rather than
			// inside the stack's own network, and without this the name does
			// not resolve at all.
			extraHosts = []string{"host.docker.internal:host-gateway"}
			pr.logln("target: " + cfg.TargetKind + " " + cfg.TargetName + " (not verified by dbcanvas)")
		}

		env = append(env,
			"DB_NAME="+cfg.Database,
			"TARGET_KIND="+cfg.TargetKind,
			"TARGET_LABEL="+cfg.TargetName,
			fmt.Sprintf("PORT=%d", stockSimPort),
		)

		pr.phase("Creating container", 45)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: stockSimImage, Hostname: host, Env: env,
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: stockSimPort, HostPort: httpPort}},
			DNS:        []string{intranetIP}, DNSSearch: []string{domain},
			ExtraHosts: extraHosts,
		})
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", stockSimPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}
		a.store.UpsertDeployment(Deployment{
			StackID: st.ID, NodeID: n.ID, ContainerID: id,
			State: DeployProvisioning, Config: mustJSON(cfg), Secrets: mustJSON(sec),
		})

		pr.phase("Waiting for the application", 80)
		if err := a.waitStockSimHealthy(ctx, id, 60*time.Second); err != nil {
			pr.fail("stocksim did not become ready: %v", err)
			return
		}

		a.store.UpsertDeployment(Deployment{
			StackID: st.ID, NodeID: n.ID, ContainerID: id,
			State: DeployRunning, Config: mustJSON(cfg), Secrets: mustJSON(sec),
		})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
	}()
}

// stockSimResolved is what a linked target resolves to.
type stockSimResolved struct {
	env         []string
	secrets     stockSimSecrets
	engine      string
	kind        string
	displayName string
	host        string
	port        int
}

// waitStockSimTarget resolves a linked canvas node down to a connectable
// endpoint and credentials, blocking until it is actually running.
func (a *App) waitStockSimTarget(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, coarseKind, targetID string, timeout time.Duration) (stockSimResolved, error) {
	label := nodeLabel(doc, targetID)
	switch coarseKind {
	case "ps":
		h, s, werr := a.waitPSNodeRunning(ctx, st.ID, targetID, hosts, domain, timeout)
		if werr != nil {
			return stockSimResolved{}, werr
		}
		// Connect as the application user, never root: root@localhost cannot
		// connect over TCP, which is what every sibling sim learned the hard way.
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/?tls=false", s.AppUser, s.AppPassword, h, pxcMySQLPort)
		return stockSimResolved{
			env:     []string{"DB_ENGINE=mysql", "MYSQL_DSN=" + dsn},
			secrets: stockSimSecrets{User: s.AppUser, Password: s.AppPassword},
			engine:  "mysql", kind: "ps", displayName: label,
			host: h, port: pxcMySQLPort,
		}, nil

	case "pg":
		h, s, werr := a.waitPgNodeRunning(ctx, st.ID, targetID, hosts, domain, timeout)
		if werr != nil {
			return stockSimResolved{}, werr
		}
		// The superuser, because the app claims a schema of its own inside the
		// "postgres" database and a stack node has no other role provisioned.
		dsn := (&url.URL{
			Scheme: "postgres", User: url.UserPassword(s.SuperUser, s.SuperPassword),
			Host: fmt.Sprintf("%s:%d", h, patroniPGPort), Path: "/postgres",
			RawQuery: "sslmode=prefer&connect_timeout=10",
		}).String()
		return stockSimResolved{
			env:     []string{"DB_ENGINE=postgres", "POSTGRES_DSN=" + dsn},
			secrets: stockSimSecrets{User: s.SuperUser, Password: s.SuperPassword},
			engine:  "postgres", kind: "pg", displayName: label,
			host: h, port: patroniPGPort,
		}, nil

	case "psm":
		dep, werr := a.waitStockSimNode(ctx, st.ID, targetID, timeout, "MongoDB")
		if werr != nil {
			return stockSimResolved{}, werr
		}
		user, pass := hotelSimCreds(dep)
		h := fqdnOf(hosts[targetID], domain)
		uri := fmt.Sprintf("mongodb://%s:%s@%s:%d/?authSource=admin&directConnection=true",
			url.QueryEscape(user), url.QueryEscape(pass), h, mongoPort)
		return stockSimResolved{
			env:     []string{"DB_ENGINE=mongodb", "MONGO_URI=" + uri},
			secrets: stockSimSecrets{User: user, Password: pass},
			engine:  "mongodb", kind: "psm", displayName: label,
			host: h, port: mongoPort,
		}, nil

	case "valkey":
		dep, werr := a.waitStockSimNode(ctx, st.ID, targetID, timeout, "Valkey")
		if werr != nil {
			return stockSimResolved{}, werr
		}
		pw := valkeyPasswordFor(dep)
		h := fqdnOf(hosts[targetID], domain)
		return stockSimResolved{
			env: []string{
				"DB_ENGINE=valkey",
				fmt.Sprintf("VALKEY_ADDRS=%s:%d", h, valkeyPort),
				"VALKEY_PASSWORD=" + pw,
			},
			secrets: stockSimSecrets{Password: pw},
			engine:  "valkey", kind: "valkey", displayName: label,
			host: h, port: valkeyPort,
		}, nil
	}
	return stockSimResolved{}, fmt.Errorf("unresolved Stock Market Sim target")
}

// waitStockSimNode blocks until a standalone node is running and returns its
// deployment, so the caller can read that node type's own stored secrets.
// Neither MongoDB nor Valkey standalone nodes have a dedicated wait helper —
// their sibling sims poll inline — so this is the shared one.
func (a *App) waitStockSimNode(ctx context.Context, stackID int64, nodeID string, timeout time.Duration, what string) (Deployment, error) {
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

// stockSimManualEnv builds the container environment from the node's own
// connection fields. Nothing is contacted here — that is the point of manual
// mode, and it is why the node form offers Test connection instead.
func stockSimManualEnv(n designNode) (env []string, sec stockSimSecrets, displayName string) {
	engine := stockSimEngine(n)
	port := n.SSPort
	if port == 0 {
		port = stockSimDefaultPort(engine)
	}
	host := strings.TrimSpace(n.SSHost)
	dsn := strings.TrimSpace(n.SSDSN)

	env = []string{
		"DB_ENGINE=" + engine,
		"DB_HOST=" + host,
		fmt.Sprintf("DB_PORT=%d", port),
		"DB_USER=" + n.SSUser,
		"DB_PASSWORD=" + n.SSPassword,
		"DB_TLS=" + stockSimTLS(n),
		"DB_PARAMS=" + n.SSParams,
	}
	if dsn != "" {
		env = append(env, "DB_DSN="+dsn)
	}
	sec = stockSimSecrets{User: n.SSUser, Password: n.SSPassword, DSN: dsn}

	switch {
	case strings.TrimSpace(n.SSLabel) != "":
		displayName = strings.TrimSpace(n.SSLabel)
	case host != "":
		displayName = fmt.Sprintf("%s:%d", host, port)
	default:
		displayName = "external " + engine
	}
	return env, sec, displayName
}

func stockSimTLS(n designNode) string {
	switch n.SSTLS {
	case "disable", "prefer", "require":
		return n.SSTLS
	}
	return "prefer"
}

// handleStockSimTest checks a connection *before* anything is deployed with
// it. An external endpoint is exactly the case where a typo should not be
// discovered via a failed deploy, so this starts a throwaway sim container with
// the candidate settings, execs `stocksim -testconn` inside it, and returns
// that one line of output verbatim.
//
// It runs the check inside a container rather than dialing from the dbcanvas
// server on purpose: the container is what will actually have to reach the
// database, and it sits on the stack's own bridge network with the stack's own
// DNS. Testing from anywhere else would be testing the wrong path.
func (a *App) handleStockSimTest(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var body struct {
		Engine   string `json:"engine"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		Database string `json:"database"`
		TLS      string `json:"tls"`
		Params   string `json:"params"`
		DSN      string `json:"dsn"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	n := designNode{
		SSEngine: body.Engine, SSHost: body.Host, SSPort: body.Port,
		SSUser: body.User, SSPassword: body.Password, SSDatabase: body.Database,
		SSTLS: body.TLS, SSParams: body.Params, SSDSN: body.DSN,
	}
	// Settings the sim binary would refuse outright are answered here rather
	// than by starting a container: those failures are fatal at startup, so the
	// container would exit and there would be nothing left to exec the check
	// in. Everything that remains is a real connection question, which only the
	// container can answer.
	if msg := stockSimConfigError(n); msg != "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "error: " + msg})
		return
	}

	ctx := r.Context()
	eng := a.engCtx(ctx)
	if ok, _ := eng.ImageExists(ctx, stockSimImage); !ok {
		writeErr(w, http.StatusPreconditionFailed,
			"image "+stockSimImage+" not found — run `make stocksim-image` first")
		return
	}

	env, _, _ := stockSimManualEnv(n)
	env = append(env, "DB_NAME="+stockSimDatabase(n), fmt.Sprintf("PORT=%d", stockSimPort))

	// Join the stack's own network when it exists, so the test takes the same
	// path the deployed node will. Before a stack's first deploy that network
	// has not been created yet — and testing a connection *before* deploying is
	// exactly what this endpoint is for — so fall back to Docker's default
	// bridge. An external database is reachable either way: egress and
	// host-gateway both work on the default bridge, and a target inside the
	// stack could not have been running before the stack was deployed anyway.
	network := networkName(st.ID)
	if _, nerr := eng.NetworkSubnet(ctx, network); nerr != nil {
		network = ""
	}

	// A distinct name per attempt, so two people testing at once cannot
	// collide, and a leftover from a crashed attempt never blocks a new one.
	name := fmt.Sprintf("dbcanvas-%d-stocksim-testconn-%d", st.ID, time.Now().UnixNano())
	id, err := eng.ContainerCreate(ctx, ContainerSpec{
		Name: name, Image: stockSimImage, Env: env,
		Network:    network,
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		NoRestart:  true,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create test container: "+err.Error())
		return
	}
	defer eng.ContainerRemove(context.Background(), id)

	if err := eng.ContainerStart(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "start test container: "+err.Error())
		return
	}
	res, err := eng.Exec(ctx, id, []string{"/stocksim", "-testconn"}, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "run connection test: "+err.Error())
		return
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		out = strings.TrimSpace(res.Stderr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.Code == 0, "message": out})
}

// waitStockSimHealthy polls until the container's own /healthz answers 200. The
// runtime image is distroless (no shell, no curl) — matching waitCarSimHealthy,
// this execs the check inside the container: `stocksim -healthcheck` exits 0
// only if /healthz answers 200.
func (a *App) waitStockSimHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"/stocksim", "-healthcheck"}, nil)
		if err == nil && res.Code == 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("healthz not ready within %s", timeout)
}
