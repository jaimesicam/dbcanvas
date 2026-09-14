package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Ledger Sim (Type=="ledgersim"): an order-and-payment ledger driven through
// JDBC and HikariCP, against MySQL (Percona Server, PXC, MySQL Community) or
// PostgreSQL. Runs dbcanvas's own dbcanvas-ledgersim:latest image (built by
// `make ledgersim-image`) — a JVM on Eclipse Temurin rather than a Go binary,
// which is the point of it.
//
// It is the Stock Market Sim's sibling, and deliberately not its replacement.
// Where stocksim asks "what does this database do under load", Ledger Sim asks
// the question one layer up: *how the application is connected*. A customer
// reporting "it works from the CLI but not from the app" is almost always
// reporting a client fact — which driver, which URL properties, which pool
// settings, which isolation level — and none of those exist in a Go sim using
// database/sql. So this node:
//
//   - ships three JDBC drivers (MySQL Connector/J, MariaDB Connector/J, pgJDBC)
//     and lets the driver be chosen per node and swapped while it runs, which is
//     the only honest way to tell a driver problem from a server problem;
//   - composes the JDBC URL from whatever dbcanvas detected about the target,
//     shows it, explains every property it derived, and lets all of it be edited
//     and re-applied against the live workload; and
//   - exposes the HikariCP pool as a first-class thing to get wrong.
//
// Connection modes mirror stocksim's: "linked" resolves a drawn association
// line, "manual" takes a connection typed into the form and can point outside
// the stack. Linked-mode resolution is stocksim's own resolver (see
// waitStockSimTarget) rather than a second copy of it — every target it can
// reach, this node can reach, minus the two engines JDBC has no business
// speaking to.
const (
	ledgerSimImage = "dbcanvas-ledgersim:latest"
	ledgerSimPort  = 8094
)

// ledgerSimEngines is what JDBC covers here. Shorter than stocksim's list on
// purpose: MongoDB and Valkey have no JDBC driver worth shipping, and a node
// that offered them would be promising something the image cannot do.
var ledgerSimEngines = []string{"mysql", "postgres"}

// ledgerSimDrivers are the drivers on the image's classpath, per engine. This
// mirrors DriverKind in the image; adding one means changing both, and the node
// form offers exactly this set so a user can never configure a combination the
// JVM would refuse at startup.
var ledgerSimDrivers = map[string][]string{
	"mysql":    {"mysql-connector-j", "mariadb-connector-j"},
	"postgres": {"pgjdbc"},
}

// ledgerSimDriverLicenses is recorded here, not only in the image's NOTICE,
// because dbcanvas is GPL-3.0-only and which driver is shipped is a licence
// question before it is a technical one. Connector/J is GPLv2-only and is
// combinable with this project *because of* the Universal FOSS Exception; if
// that ever stops being true, MariaDB Connector/J is the LGPL route to the same
// servers and pgJDBC is BSD.
var ledgerSimDriverLicenses = map[string]string{
	"mysql-connector-j":   "GPL-2.0-only WITH Universal-FOSS-Exception-1.0",
	"mariadb-connector-j": "LGPL-2.1-or-later",
	"pgjdbc":              "BSD-2-Clause",
}

func ledgerSimEngineImplemented(engine string) bool {
	for _, e := range ledgerSimEngines {
		if e == engine {
			return true
		}
	}
	return false
}

func ledgerSimDriverFor(engine, driver string) string {
	list := ledgerSimDrivers[engine]
	if len(list) == 0 {
		return ""
	}
	for _, d := range list {
		if d == driver {
			return d
		}
	}
	// A driver that cannot speak the engine is a stale form value, not a
	// preference — the image would fall back anyway, so agree with it here.
	return list[0]
}

func ledgerSimDefaultPort(engine string) int {
	if engine == "postgres" {
		return patroniPGPort
	}
	return pxcMySQLPort
}

// ledgerSimMode normalises the node's connection mode. Unset means linked,
// which is what a node dropped on the canvas and wired up gets.
func ledgerSimMode(n designNode) string {
	if n.LSMode == "manual" {
		return "manual"
	}
	return "linked"
}

func ledgerSimEngine(n designNode) string {
	if ledgerSimEngineImplemented(n.LSEngine) {
		return n.LSEngine
	}
	return "mysql"
}

func ledgerSimDatabase(n designNode) string {
	if db := strings.TrimSpace(n.LSDatabase); db != "" {
		return db
	}
	return "ledgersim"
}

func ledgerSimTLS(n designNode) string {
	switch n.LSTLS {
	case "disable", "prefer", "require":
		return n.LSTLS
	}
	return "prefer"
}

// Pool and workload bounds, mirroring PoolSpec/Knobs.sane() in the image so a
// value the JVM would clamp is corrected on the canvas instead.
const (
	ledgerSimDefaultThreads = 8
	ledgerSimMaxThreads     = 256
	ledgerSimDefaultPoolMax = 10
	ledgerSimMaxPoolMax     = 500
	ledgerSimMaxShards      = 8
)

func ledgerSimThreads(n designNode) int {
	if n.LSThreads <= 0 {
		return ledgerSimDefaultThreads
	}
	if n.LSThreads > ledgerSimMaxThreads {
		return ledgerSimMaxThreads
	}
	return n.LSThreads
}

// ledgerSimPoolMode normalises the connection mode. Anything unrecognised is
// "pooled": a node must never end up silently running the other experiment.
func ledgerSimPoolMode(n designNode) string {
	if strings.EqualFold(strings.TrimSpace(n.LSPoolMode), "direct") {
		return "direct"
	}
	return "pooled"
}

func ledgerSimPoolMax(n designNode) int {
	if n.LSPoolMax <= 0 {
		return ledgerSimDefaultPoolMax
	}
	if n.LSPoolMax > ledgerSimMaxPoolMax {
		return ledgerSimMaxPoolMax
	}
	return n.LSPoolMax
}

func ledgerSimRevenueShards(n designNode) int {
	if n.LSRevenueShards <= 0 {
		return ledgerSimMaxShards
	}
	if n.LSRevenueShards > ledgerSimMaxShards {
		return ledgerSimMaxShards
	}
	return n.LSRevenueShards
}

// ledgerSimIsolationLevels mirrors Workload.isolationLevel in the image. The
// empty string means "whatever the driver defaults to", which is itself a thing
// worth being able to choose: it differs between MySQL (REPEATABLE READ) and
// PostgreSQL (READ COMMITTED), and that difference is a support case of its own.
var ledgerSimIsolationLevels = map[string]bool{
	"":                             true,
	"TRANSACTION_READ_UNCOMMITTED": true,
	"TRANSACTION_READ_COMMITTED":   true,
	"TRANSACTION_REPEATABLE_READ":  true,
	"TRANSACTION_SERIALIZABLE":     true,
}

// ledgerSimConfig is the non-secret profile shown for a deployed node.
// Credentials live in ledgerSimSecrets and never appear here.
type ledgerSimConfig struct {
	Image      string `json:"image"`
	Hostname   string `json:"hostname"`
	FQDN       string `json:"fqdn"`
	HTTPPort   int    `json:"httpPort"`
	Mode       string `json:"mode"`
	Engine     string `json:"engine"`
	Driver     string `json:"driver"`
	License    string `json:"driverLicense"`
	Database   string `json:"database"`
	TargetKind string `json:"targetKind"`
	TargetName string `json:"targetName"`
	// JDBCURL is what the node was deployed pointing at, with any password in a
	// pasted override masked. It is a record of the starting point only — the
	// dashboard can change it afterwards, which is the whole feature, so the
	// node panel says so rather than presenting this as current truth.
	JDBCURL       string `json:"jdbcUrl"`
	Threads       int    `json:"threads"`
	PoolMode      string `json:"poolMode"`
	PoolMax       int    `json:"poolMax"`
	Isolation     string `json:"isolation"`
	RevenueShards int    `json:"revenueShards"`
}

type ledgerSimSecrets struct {
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	JDBCURL  string `json:"jdbcUrl,omitempty"`
}

// ledgerSimEngineAndIssues resolves the engine a node will actually run against
// and reports every reason it might not resolve. In linked mode the engine is a
// property of the node on the other end of the line, not of this node's own
// form — so a link to a MongoDB or Valkey node is caught here, where it can be
// explained, rather than by a JVM that has no driver for it.
func ledgerSimEngineAndIssues(doc designDoc, n designNode) (string, []issue) {
	var out []issue
	if ledgerSimMode(n) == "manual" {
		engine := ledgerSimEngine(n)
		if strings.TrimSpace(n.LSHost) == "" && strings.TrimSpace(n.LSJdbcURL) == "" {
			out = append(out, issue{Level: "error", Message: "Ledger Sim node " + n.Label +
				" is set to a manual connection but has no host — enter one, or paste a full JDBC URL under Advanced"})
		}
		if n.LSPort < 0 || n.LSPort > 65535 {
			out = append(out, issue{Level: "error", Message: "Ledger Sim node " + n.Label +
				" has an invalid port — leave it blank for the engine default"})
		}
		out = append(out, issue{Level: "info", Message: "Ledger Sim node " + n.Label +
			" connects to a database outside this stack — dbcanvas cannot verify it before deploying, so use Test connection on the node to check it now"})
		return engine, out
	}

	kind, targetID, ok := stockSimTarget(doc, n.ID)
	if !ok {
		out = append(out, issue{Level: "error", Message: "Ledger Sim node " + n.Label +
			" is not connected to a database — draw a line to one, or switch it to a manual connection"})
		return "", out
	}
	engine := stockSimEngineForTarget(doc, kind, targetID)
	if engine == "" {
		out = append(out, issue{Level: "error", Message: "Ledger Sim node " + n.Label +
			" is linked to something whose database engine cannot be determined — link it to the database, or to a router that fronts exactly one cluster"})
		return "", out
	}
	if !ledgerSimEngineImplemented(engine) {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf(
			"Ledger Sim node %s is linked to a %s target, and there is no JDBC driver for it — Ledger Sim speaks %s. Use a Stock Market Sim node for %s",
			n.Label, engineDisplayLabel(engine), strings.Join(ledgerSimEngines, " or "), engineDisplayLabel(engine))})
		return "", out
	}
	return engine, out
}

// ledgerSimIssues is the node's full validation, called by the stack validator.
func ledgerSimIssues(doc designDoc, n designNode) []issue {
	engine, out := ledgerSimEngineAndIssues(doc, n)

	if stockSimReservedDatabases[strings.ToLower(ledgerSimDatabase(n))] {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf(
			"Ledger Sim node %s cannot use %q as its database — that is a reserved system database. Choose another name (the default is \"ledgersim\")",
			n.Label, ledgerSimDatabase(n))})
	}
	if engine != "" && n.LSDriver != "" && ledgerSimDriverFor(engine, n.LSDriver) != n.LSDriver {
		out = append(out, issue{Level: "warn", Message: fmt.Sprintf(
			"Ledger Sim node %s is set to the %s driver, which cannot speak to a %s target — %s will be used instead",
			n.Label, n.LSDriver, engineDisplayLabel(engine), ledgerSimDriverFor(engine, n.LSDriver))})
	}
	if !ledgerSimIsolationLevels[strings.TrimSpace(n.LSIsolation)] {
		out = append(out, issue{Level: "error", Message: "Ledger Sim node " + n.Label +
			" has an unknown isolation level " + strconv.Quote(n.LSIsolation) +
			" — use one of the TRANSACTION_* names, or leave it blank for the driver default"})
	}
	if n.LSThreads < 0 || n.LSThreads > ledgerSimMaxThreads {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf(
			"Ledger Sim node %s asks for %d worker threads — use 1..%d, or leave it blank for %d",
			n.Label, n.LSThreads, ledgerSimMaxThreads, ledgerSimDefaultThreads)})
	}
	if n.LSPoolMax < 0 || n.LSPoolMax > ledgerSimMaxPoolMax {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf(
			"Ledger Sim node %s asks for a maximum pool size of %d — use 1..%d, or leave it blank for %d",
			n.Label, n.LSPoolMax, ledgerSimMaxPoolMax, ledgerSimDefaultPoolMax)})
	}
	if m := strings.TrimSpace(n.LSPoolMode); m != "" && !strings.EqualFold(m, "pooled") && !strings.EqualFold(m, "direct") {
		out = append(out, issue{Level: "error", Message: "Ledger Sim node " + n.Label +
			" has an unknown connection mode " + strconv.Quote(n.LSPoolMode) + " — use \"pooled\" or \"direct\""})
	}
	// Direct mode opens a connection per transaction, so N workers means N
	// concurrent connections with no ceiling in front of them. Said plainly,
	// because on a server with a low max_connections that is a real outage rather
	// than a slow benchmark.
	if ledgerSimPoolMode(n) == "direct" && ledgerSimThreads(n) > 32 {
		out = append(out, issue{Level: "warn", Message: fmt.Sprintf(
			"Ledger Sim node %s runs %d workers with no pool, so it will hold up to %d connections open at once and churn one per transaction — check the server's max_connections before deploying",
			n.Label, ledgerSimThreads(n), ledgerSimThreads(n))})
	}
	// Worth saying rather than silently allowing: a pool smaller than the worker
	// count is a queue, and someone who has not chosen it on purpose will read
	// the resulting latency as the database being slow.
	if ledgerSimPoolMode(n) == "pooled" && n.LSPoolMax > 0 && n.LSThreads > 0 && n.LSPoolMax < n.LSThreads {
		out = append(out, issue{Level: "info", Message: fmt.Sprintf(
			"Ledger Sim node %s has %d workers sharing %d pooled connections — workers will queue for a connection, which is a fine thing to demonstrate but shows up as latency, not as a pool error",
			n.Label, n.LSThreads, n.LSPoolMax)})
	}
	return out
}

// ledgerSimJDBCURL composes the URL for display and for the deployment record.
// It mirrors JdbcUrl.build in the image closely enough to be recognisable, but
// deliberately does not reproduce its auto-derived driver properties: those are
// the image's business, they differ per driver, and a second implementation
// here would be one that drifts. The URL shown on the canvas is therefore the
// shape without them, and the dashboard shows the real thing.
func ledgerSimJDBCURL(engine, driver, host string, port int, database string) string {
	scheme := "jdbc:mysql"
	switch driver {
	case "mariadb-connector-j":
		scheme = "jdbc:mariadb"
	case "pgjdbc":
		scheme = "jdbc:postgresql"
	}
	if host == "" {
		return scheme + "://<host>:" + strconv.Itoa(port) + "/" + database
	}
	return fmt.Sprintf("%s://%s:%d/%s", scheme, host, port, database)
}

// ledgerSimManualEnv builds the container environment from the node's own
// connection fields. Nothing is contacted here — that is what manual mode
// means, and why the node form offers Test connection instead.
func ledgerSimManualEnv(n designNode) (env []string, sec ledgerSimSecrets, displayName string) {
	engine := ledgerSimEngine(n)
	driver := ledgerSimDriverFor(engine, n.LSDriver)
	port := n.LSPort
	if port == 0 {
		port = ledgerSimDefaultPort(engine)
	}
	host := strings.TrimSpace(n.LSHost)
	url := strings.TrimSpace(n.LSJdbcURL)

	env = []string{
		"DB_ENGINE=" + engine,
		"DB_DRIVER=" + driver,
		"DB_HOST=" + host,
		fmt.Sprintf("DB_PORT=%d", port),
		"DB_USER=" + n.LSUser,
		"DB_PASSWORD=" + n.LSPassword,
		"DB_TLS=" + ledgerSimTLS(n),
		"DB_PARAMS=" + strings.TrimSpace(n.LSParams),
	}
	if url != "" {
		env = append(env, "JDBC_URL="+url)
	}
	sec = ledgerSimSecrets{User: n.LSUser, Password: n.LSPassword, JDBCURL: url}

	switch {
	case strings.TrimSpace(n.LSLabel) != "":
		displayName = strings.TrimSpace(n.LSLabel)
	case host != "":
		displayName = fmt.Sprintf("%s:%d", host, port)
	default:
		displayName = "external " + engine
	}
	return env, sec, displayName
}

// ledgerSimLinkedEnv turns stocksim's resolved endpoint into JDBC environment.
//
// Reusing that resolver rather than writing a second one is the whole reason
// this node supports every cluster frame, router and Kubernetes operator that
// the Stock Market Sim does. What differs is only the last step: stocksim emits
// a Go DSN, and this emits the fields the JVM composes a JDBC URL from.
func ledgerSimLinkedEnv(n designNode, r stockSimResolved) (env []string, sec ledgerSimSecrets) {
	driver := ledgerSimDriverFor(r.engine, n.LSDriver)
	env = []string{
		"DB_ENGINE=" + r.engine,
		"DB_DRIVER=" + driver,
		"DB_HOST=" + r.host,
		fmt.Sprintf("DB_PORT=%d", r.port),
		"DB_USER=" + r.secrets.User,
		"DB_PASSWORD=" + r.secrets.Password,
		"DB_TLS=" + ledgerSimTLS(n),
		"DB_PARAMS=" + strings.TrimSpace(n.LSParams),
	}
	// A URL override still applies in linked mode: the line says which database,
	// and the override says how to reach it — useful for pointing the driver at
	// a specific member of the cluster it is linked to.
	if url := strings.TrimSpace(n.LSJdbcURL); url != "" {
		env = append(env, "JDBC_URL="+url)
	}
	return env, ledgerSimSecrets{User: r.secrets.User, Password: r.secrets.Password}
}

// provisionLedgerSim records the deployment then brings up the sim container.
func (a *App) provisionLedgerSim(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	host := hosts[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	// Reuse the previously published host port across a redeploy so the
	// dashboard URL in node properties stays stable, as stocksim and PMM do.
	httpPort := 0
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old ledgerSimConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			httpPort = old.HTTPPort
		}
	}
	if httpPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpPort = p
		}
	}

	mode := ledgerSimMode(n)
	cfg := ledgerSimConfig{
		Image: ledgerSimImage, Hostname: host, FQDN: fqdn, HTTPPort: httpPort,
		Mode: mode, Engine: ledgerSimEngine(n), Database: ledgerSimDatabase(n),
		Threads: ledgerSimThreads(n), PoolMode: ledgerSimPoolMode(n), PoolMax: ledgerSimPoolMax(n),
		Isolation: strings.TrimSpace(n.LSIsolation), RevenueShards: ledgerSimRevenueShards(n),
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

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, ledgerSimImage); !ok {
			pr.fail("image %s not found — run `make ledgersim-image` first", ledgerSimImage)
			return
		}

		pr.phase("Waiting for Intranet to be ready", 8)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		var env []string
		var sec ledgerSimSecrets
		var extraHosts []string
		targetHost, targetPort := "", ledgerSimDefaultPort(cfg.Engine)

		switch mode {
		case "linked":
			pr.phase("Waiting for linked database", 20)
			r, werr := a.waitStockSimTarget(ctx, st, hosts, doc, domain, coarseKind, targetID, deployTimeout(), false, pr.logln)
			if werr != nil {
				pr.fail("%v", werr)
				return
			}
			// The engine is only certain now. A Kubernetes frame in particular is
			// whichever of its operators turned out to be running, and JDBC cannot
			// speak to two of the six — refuse here with a sentence rather than
			// starting a JVM that has no driver to load.
			if !ledgerSimEngineImplemented(r.engine) {
				pr.fail("the linked target is %s, which Ledger Sim has no JDBC driver for — it speaks %s",
					engineDisplayLabel(r.engine), strings.Join(ledgerSimEngines, " or "))
				return
			}
			cfg.Engine, cfg.TargetKind, cfg.TargetName = r.engine, r.kind, r.displayName
			targetHost, targetPort = r.host, r.port
			env, sec = ledgerSimLinkedEnv(n, r)
			pr.logln(fmt.Sprintf("target: %s %s (%s:%d)", cfg.TargetKind, cfg.TargetName, r.host, r.port))

		default:
			pr.phase("Using the connection configured on this node", 20)
			env, sec, cfg.TargetName = ledgerSimManualEnv(n)
			cfg.TargetKind = "external-" + cfg.Engine
			targetHost = strings.TrimSpace(n.LSHost)
			if n.LSPort > 0 {
				targetPort = n.LSPort
			}
			// Manual mode is the one case where the database may be on the Docker
			// host rather than in the stack network, and the name does not resolve
			// without this.
			extraHosts = []string{"host.docker.internal:host-gateway"}
			pr.logln("target: " + cfg.TargetKind + " " + cfg.TargetName + " (not verified by dbcanvas)")
		}

		cfg.Driver = ledgerSimDriverFor(cfg.Engine, n.LSDriver)
		cfg.License = ledgerSimDriverLicenses[cfg.Driver]
		cfg.JDBCURL = ledgerSimJDBCURL(cfg.Engine, cfg.Driver, targetHost, targetPort, cfg.Database)
		if url := strings.TrimSpace(n.LSJdbcURL); url != "" {
			cfg.JDBCURL = maskURLPassword(url)
		}

		env = append(env,
			"DB_NAME="+cfg.Database,
			"LS_LABEL="+cfg.TargetName,
			fmt.Sprintf("LS_THREADS=%d", cfg.Threads),
			"POOL_MODE="+cfg.PoolMode,
			fmt.Sprintf("POOL_MAX=%d", cfg.PoolMax),
			"LS_ISOLATION="+cfg.Isolation,
			fmt.Sprintf("LS_REVENUE_SHARDS=%d", cfg.RevenueShards),
			fmt.Sprintf("LS_CUSTOMERS=%d", ledgerSimCustomers(n)),
			fmt.Sprintf("LS_DEADLOCK_SHARE=%g", ledgerSimShare(n.LSDeadlockShare)),
			fmt.Sprintf("LS_HOT_SHARE=%g", ledgerSimShare(n.LSHotShare)),
			"LS_AUTOSTART="+strconv.FormatBool(!n.LSStartPaused),
			fmt.Sprintf("PORT=%d", ledgerSimPort),
		)
		if cfg.PoolMode == "direct" {
			pr.logln(fmt.Sprintf("driver: %s (%s); NO POOL — one connection per transaction, %d workers",
				cfg.Driver, cfg.License, cfg.Threads))
		} else {
			pr.logln(fmt.Sprintf("driver: %s (%s); HikariCP pool max %d, %d workers",
				cfg.Driver, cfg.License, cfg.PoolMax, cfg.Threads))
		}

		pr.phase("Creating container", 45)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: ledgerSimImage, Hostname: host, Env: env,
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: ledgerSimPort, HostPort: httpPort}},
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
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", ledgerSimPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}
		a.store.UpsertDeployment(Deployment{
			StackID: st.ID, NodeID: n.ID, ContainerID: id,
			State: DeployProvisioning, Config: mustJSON(cfg), Secrets: mustJSON(sec),
		})

		// A JVM plus a driver plus schema creation is slower off the mark than a
		// Go binary, and the dashboard deliberately comes up before the database
		// connection does — so this waits on the dashboard, not on the database.
		pr.phase("Waiting for the application", 80)
		if err := a.waitLedgerSimHealthy(ctx, id, 120*time.Second); err != nil {
			pr.fail("ledgersim did not become ready: %v", err)
			return
		}

		a.store.UpsertDeployment(Deployment{
			StackID: st.ID, NodeID: n.ID, ContainerID: id,
			State: DeployRunning, Config: mustJSON(cfg), Secrets: mustJSON(sec),
		})
		a.reconcileStackDNS(ctx, st.ID)
		pr.logln(fmt.Sprintf("dashboard on http://localhost:%d — the JDBC URL and the pool can be changed there without redeploying", cfg.HTTPPort))
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
	}()
}

// waitLedgerSimHealthy polls until the container's own /healthz answers. The
// runtime image has a shell, but the check is Exec'd through the same
// `-healthcheck` flag the Go sims use so the contract is identical whatever the
// image is built from.
func (a *App) waitLedgerSimHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"/ledgersim", "-healthcheck"}, nil)
		if err == nil && res.Code == 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("healthz not ready within %s", timeout)
}

func ledgerSimCustomers(n designNode) int {
	if n.LSCustomers <= 0 {
		return 2000
	}
	if n.LSCustomers > 5_000_000 {
		return 5_000_000
	}
	return n.LSCustomers
}

// ledgerSimShare clamps a 0..1 share. A value outside it is a typo rather than
// an intent, and the image would clamp it anyway.
func ledgerSimShare(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// maskURLPassword hides a password in a pasted JDBC URL before it is stored in
// the non-secret config the node panel renders.
func maskURLPassword(url string) string {
	return jdbcPasswordPattern.ReplaceAllString(url, "${1}****")
}

// handleLedgerSimTest answers "can this connection work" before the stack is
// deployed, by running the sim image's own `-testconn` in a throwaway
// container. The answer has to come from the image: the drivers, the URL
// composition and the TLS mapping all live there, and a second implementation
// in Go would be answering a different question. Mirrors handleStockSimTest.
func (a *App) handleLedgerSimTest(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var body struct {
		Engine   string `json:"engine"`
		Driver   string `json:"driver"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		Database string `json:"database"`
		TLS      string `json:"tls"`
		Params   string `json:"params"`
		JDBCURL  string `json:"jdbcUrl"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	n := designNode{
		LSEngine: body.Engine, LSDriver: body.Driver, LSHost: body.Host, LSPort: body.Port,
		LSUser: body.User, LSPassword: body.Password, LSDatabase: body.Database,
		LSTLS: body.TLS, LSParams: body.Params, LSJdbcURL: body.JDBCURL,
	}
	if strings.TrimSpace(body.Host) == "" && strings.TrimSpace(body.JDBCURL) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "error: no host — enter one, or paste a full JDBC URL"})
		return
	}

	ctx := r.Context()
	eng := a.engCtx(ctx)
	if ok, _ := eng.ImageExists(ctx, ledgerSimImage); !ok {
		writeErr(w, http.StatusPreconditionFailed,
			"image "+ledgerSimImage+" not found — run `make ledgersim-image` first")
		return
	}

	env, _, _ := ledgerSimManualEnv(n)
	env = append(env, "DB_NAME="+ledgerSimDatabase(n), fmt.Sprintf("PORT=%d", ledgerSimPort))

	// Join the stack's own network when it exists so the test takes the path the
	// deployed node will; before a stack's first deploy there is no such network,
	// and testing then is exactly what this is for. Same reasoning as stocksim's.
	network := networkName(st.ID)
	if _, nerr := eng.NetworkSubnet(ctx, network); nerr != nil {
		network = ""
	}

	name := fmt.Sprintf("dbcanvas-%d-ledgersim-testconn-%d", st.ID, time.Now().UnixNano())
	id, err := eng.ContainerCreate(ctx, ContainerSpec{
		Name: name, Image: ledgerSimImage, Env: env,
		Network:    network,
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		// The entrypoint starts the dashboard, which stays up whether or not the
		// database is reachable — so the container is alive to be Exec'd into,
		// which is the same arrangement handleStockSimTest relies on.
		NoRestart: true,
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
	res, err := eng.Exec(ctx, id, []string{"/ledgersim", "-testconn"}, nil)
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

// jdbcPasswordPattern matches a password carried in a JDBC URL's query string,
// which is how a pasted override can smuggle one into a field the node panel
// renders. Both spellings are in the wild: pgJDBC uses `password`, and MySQL
// URLs are often written with it too.
var jdbcPasswordPattern = regexp.MustCompile(`(?i)([?&](?:password|pwd)=)[^&]*`)
