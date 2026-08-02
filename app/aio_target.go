package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// aio_target.go — making an All-in-One node's databases usable from the rest of
// the app (Query Runner, Benchmark, Data Generator).
//
// Every one of those tools resolves a *node* to one connection: it looks up the
// node's type, infers the engine, and assumes the product's default port. An
// All-in-One node breaks all three assumptions at once — one node, many engines,
// no default ports.
//
// Rather than teach each tool about instances, a target id becomes composite:
//
//	"<nodeId>"           an ordinary node, exactly as before
//	"<nodeId>#<inst>"    one instance inside an All-in-One node
//
// Everything downstream splits the id once (aioSplitTarget) and then works with
// a plain node id plus, when present, the instance's engine/port/credentials.
// Ordinary nodes contain no "#", so their path is untouched.

// aioTargetSep separates a node id from an instance name in a composite target.
// "#" is safe: node ids are uuids and instance names are DNS labels, so neither
// can contain it.
const aioTargetSep = "#"

// aioSplitTarget splits a composite target id. inst is "" for an ordinary node.
func aioSplitTarget(target string) (nodeID, inst string) {
	if base, i, ok := strings.Cut(target, aioTargetSep); ok {
		return base, i
	}
	return target, ""
}

// aioJoinTarget builds a composite target id for one instance.
func aioJoinTarget(nodeID, inst string) string { return nodeID + aioTargetSep + inst }

// aioEngineForKind maps an All-in-One feature kind to the engine name the app's
// tools use. Mirrors engineForType, which keys on node types instead.
func aioEngineForKind(kind string) string {
	switch aioFamilyOf(kind) {
	case famMySQL:
		return "mysql"
	case famPG:
		return "postgres"
	case famMongo:
		return "mongodb"
	}
	return "" // valkey, proxies and orchestrator are not SQL/Mongo query targets
}

// aioPMMSupported reports whether an instance kind can actually be registered as a
// PMM *service*.
//
// Only the three database engines are wired here. Orchestrator has no PMM service
// type at all; Valkey and the proxies do have one on their dedicated nodes, but no
// All-in-One provisioner adds it. Either way the control must not be offered — a
// picker that silently does nothing is worse than an absent one, which is the same
// rule aioTLSSupported enforces for certificates.
//
// The node's OS metrics are still collected for every instance once any one of them
// registers the agent, so "not monitored" here means "no per-service dashboard".
func aioPMMSupported(kind string) bool { return aioEngineForKind(kind) != "" }

// aioFindInstance returns one instance's runtime row from a node's deployment.
func aioFindInstance(dep Deployment, inst string) (aioInstanceRuntime, bool) {
	for _, m := range aioRuntimeInstances(dep) {
		if m.Inst == inst {
			return m, true
		}
	}
	return aioInstanceRuntime{}, false
}

// aioTargetableInstances is the subset of a node's instances the query/benchmark/
// datagen tools can actually connect to: a database, and one that is meant to be
// reached directly.
//
// Cluster members are all listed rather than just the primary — running a query
// against a specific replica is exactly the sort of thing these tools are for.
// Non-database instances (proxies, Orchestrator) are excluded because they have
// no schema of their own; a proxy is reachable through its own port anyway.
func aioTargetableInstances(dep Deployment) []aioInstanceRuntime {
	var out []aioInstanceRuntime
	for _, m := range aioRuntimeInstances(dep) {
		if !m.Ready || aioEngineForKind(m.Kind) == "" {
			continue
		}
		// A mongos router is a valid target; a config server is not something a
		// user should be writing to.
		if m.Role == "config" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// aioInstanceCreds resolves one instance's engine, port and network credentials.
//
// The credentials come from the node's stored secrets, which are shared across
// the node's instances of a family — the same .env-derived accounts every
// classic node uses, which is what makes an All-in-One instance behave like an
// ordinary target once it has been resolved.
func aioInstanceCreds(dep Deployment, m aioInstanceRuntime) (engine string, port int, user, pass string) {
	engine = aioEngineForKind(m.Kind)
	port = m.Ports.Client
	switch engine {
	case "mysql":
		var s pxcSecrets
		json.Unmarshal(dep.Secrets, &s)
		user, pass = s.AdminUser, s.AdminPassword
		if user == "" {
			user = "admin"
		}
	case "postgres":
		// PostgreSQL instances are provisioned with the shared pgFamilySecrets
		// superuser rather than the MySQL-shaped node secrets.
		sec := pgFamilySecrets()
		user, pass = sec.Super(), sec.SuperPassword
	case "mongodb":
		user, pass = "admin", envOr("MONGO_ADMIN_PASSWORD", envOr("MYSQL_ADMIN_PASSWORD", "admin_password"))
	}
	return engine, port, user, pass
}

// aioTargetLabel is how an instance is named in a target list: the node's label
// plus the instance, so two All-in-One nodes with a "ps01" stay distinguishable.
func aioTargetLabel(nodeLabel string, m aioInstanceRuntime) string {
	return nodeLabel + " / " + m.Inst
}

// aioDBConn builds the Data Generator's connection for one All-in-One instance.
//
// The Data Generator reaches a database by exec'ing the product's CLI inside the
// container rather than dialing it. On a classic node the client's defaults find
// the only server there is; here the container holds several, so the connection
// carries the arguments that select this one — and, for PostgreSQL, an absolute
// binary path, since a source-built or PGDG server may not be the psql on PATH.
func (a *App) aioDBConn(st Stack, dep Deployment, inst string) (dbConn, bool) {
	m, ok := aioFindInstance(dep, inst)
	if !ok {
		return dbConn{}, false
	}
	engine, port, user, pass := aioInstanceCreds(dep, m)
	if engine == "" {
		return dbConn{}, false
	}
	l := aioLayout(m.Inst, m.Kind, m.Ports)
	eng := a.nodeEngine(st, "aio")
	c := dbConn{
		ContainerID: dep.ContainerID, Engine: engine, StackID: st.ID, eng: eng,
		Super: user, Password: pass,
	}
	switch engine {
	case "mysql":
		// The socket is unambiguous; a TCP port would also work but the socket
		// matches how root@localhost is granted.
		c.Args = []string{"--socket=" + l.Sock}
		// The Data Generator's MySQL path authenticates as root via MYSQL_PWD.
		var s pxcSecrets
		json.Unmarshal(dep.Secrets, &s)
		c.Super, c.Password = s.RootUser, s.RootPassword
		if c.Super == "" {
			c.Super = "root"
		}
	case "postgres":
		c.Args = []string{"-h", l.RunDir, "-p", strconv.Itoa(port)}
		c.Bin = aioPGBinForInstance(dep, m)
	case "mongodb":
		// MongoDB is dialed over the network rather than exec'd, so the port goes
		// on the connection instead of into client arguments.
		c.Port = port
	}
	return c, true
}

// aioPGBinForInstance is the bin directory whose psql matches this instance's
// server. All three PostgreSQL distributions install to /usr/pgsql-<major>, so
// the major recorded in the plan locates the right client — which matters
// because a container may hold several majors and PATH points at only one.
func aioPGBinForInstance(dep Deployment, m aioInstanceRuntime) string {
	var cfg aioConfig
	if json.Unmarshal(dep.Config, &cfg) != nil {
		return ""
	}
	// Every PostgreSQL flavor installs to /usr/pgsql-<major>/bin; the instance's
	// own psql is the one that certainly speaks its protocol version.
	if maj := aioPGMajorOfRuntime(cfg, m); maj != "" {
		return "/usr/pgsql-" + maj + "/bin"
	}
	return ""
}

// aioPGMajorOfRuntime recovers an instance's PostgreSQL major from the stored
// plan. Returns "" when it cannot be determined, in which case the caller falls
// back to whatever psql is on PATH.
func aioPGMajorOfRuntime(cfg aioConfig, m aioInstanceRuntime) string {
	for _, x := range cfg.Instances {
		if x.Inst == m.Inst {
			return x.PGMajor
		}
	}
	return ""
}

// ---------------------------------------------------------------- PMM

// aioPMMRegisterScript registers ONE instance with a PMM server.
//
// pxcPMMRHEL cannot be reused: it hardcodes /var/lib/mysql/mysql.sock and names
// the service after the node. Here several servers share the container, so the
// socket and the service name both have to be the instance's — otherwise every
// instance would overwrite the same PMM service and only the last would report.
// pmm-agent itself is per container and configured once; `pmm-admin add` is what
// runs per instance.
// aioPMMSetupScript installs pmm-client and points the agent at the PMM server.
//
// It runs ONCE per node, deliberately. `pmm-admin config` re-registers the node and
// DROPS every service already added to it — so running it per instance (as this used
// to) meant each instance silently deleted the ones before it, and only the last of
// eleven survived while the deploy reported all eleven registered. The config is also
// skipped when the agent is already connected, so a redeploy that adds one instance
// does not wipe the rest.
const aioPMMSetupScript = `set -e
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; dnf -y -q install pmm-client >/dev/null 2>&1 || { apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq pmm-client >/dev/null; }; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
if pmm-admin status >/dev/null 2>&1; then
  echo "pmm-agent already registered — keeping existing services"
else
  pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null 2>&1 || true
  systemctl enable --now pmm-agent >/dev/null 2>&1 || true
  for i in $(seq 1 15); do pmm-admin status >/dev/null 2>&1 && break; sleep 2; done
  pmm-admin status >/dev/null 2>&1 || { echo "pmm-agent did not register with $PMM_URL"; exit 1; }
  echo "pmm-agent registered"
fi`

// aioPMMAddScript adds ONE instance as a PMM service. Idempotent: the remove-then-add
// pair means a redeploy re-points an existing service rather than erroring on it.
const aioPMMAddScript = `set -e
case "$ENGINE" in
mysql)
  pmm-admin remove mysql "$SVC" >/dev/null 2>&1 || true
  mysql --no-defaults --socket="$SOCK" -u"$DB_USER" -p"$DB_PW" 2>/dev/null <<SQL || true
CREATE USER IF NOT EXISTS 'pmm'@'%' IDENTIFIED BY '$PMM_PW' WITH MAX_USER_CONNECTIONS 10;
ALTER USER 'pmm'@'%' IDENTIFIED BY '$PMM_PW';
GRANT SELECT, PROCESS, REPLICATION CLIENT, RELOAD, BACKUP_ADMIN ON *.* TO 'pmm'@'%';
GRANT SELECT ON performance_schema.* TO 'pmm'@'%';
SQL
  QS=perfschema
  [ "$(mysql --no-defaults --socket="$SOCK" -upmm -p"$PMM_PW" -N -e 'SELECT @@global.slow_query_log' 2>/dev/null)" = "1" ] && QS=slowlog
  pmm-admin add mysql --username=pmm --password="$PMM_PW" --socket="$SOCK" --query-source="$QS" "$SVC"
  ;;
postgres)
  pmm-admin remove postgresql "$SVC" >/dev/null 2>&1 || true
  pmm-admin add postgresql --username="$DB_USER" --password="$DB_PW" --host=127.0.0.1 --port="$PORT" "$SVC"
  ;;
mongodb)
  pmm-admin remove mongodb "$SVC" >/dev/null 2>&1 || true
  pmm-admin add mongodb --username="$DB_USER" --password="$DB_PW" --host=127.0.0.1 --port="$PORT" "$SVC"
  ;;
*) echo "no PMM integration for engine $ENGINE"; exit 0 ;;
esac
exit 0`

// aioRegisterPMM registers every instance that named a PMM node.
//
// Best-effort per instance, matching how the classic provisioners treat PMM: a
// monitoring failure should not fail a node whose databases are all up. Each
// failure is reported in the deploy log rather than swallowed.
func (a *App) aioRegisterPMM(ctx context.Context, st Stack, n designNode, doc designDoc, id string, cfg aioConfig, fresh map[string]bool, sec pxcSecrets, pr *pxcProg) {
	byInst := map[string]aioInstance{}
	for _, in := range n.AIOInstances {
		if in.PMMNodeID == "" {
			continue
		}
		key := aioSanitizeInst(in.Name)
		byInst[key] = in
	}
	if len(byInst) == 0 {
		return
	}
	dep, _ := a.store.GetDeployment(st.ID, n.ID)

	// The agent is configured once for the whole node, before any service is added.
	// Doing it per instance dropped every service added so far (see
	// aioPMMSetupScript). Any PMM-enabled instance can supply the server URL — they
	// all point at the same node in practice, and the first one that resolves wins.
	configured := false
	for _, in := range byInst {
		pmmFQDN, pmmUser, pmmPass, ok := a.pmmServerFor(st, doc, in.PMMNodeID)
		if !ok {
			continue
		}
		if err := a.runStep(ctx, id, aioPMMSetupScript, []string{"PMM_URL=" + pmmServerURL(pmmFQDN, pmmUser, pmmPass)}, pr.logln); err != nil {
			pr.logln("PMM agent setup failed: " + err.Error())
			return
		}
		configured = true
		break
	}
	if !configured {
		pr.logln("PMM server not available yet — monitoring not registered")
		return
	}

	registered := 0
	var unsupported []string
	// Every PMM-enabled instance is (re)added, not just the fresh ones: the adds are
	// idempotent, and re-adding is what makes the node converge if a service was ever
	// dropped — which is exactly the state this function used to leave behind.
	for _, m := range cfg.Instances {
		key := m.Group
		if key == "" {
			key = m.Inst
		}
		in, ok := byInst[key]
		if !ok {
			continue
		}
		engine := aioEngineForKind(m.Kind)
		if engine == "" {
			// Valkey, the proxies and Orchestrator have no PMM service exporter here.
			// Say so rather than skipping in silence: the instance asked to be
			// monitored, and its OS metrics ARE collected by the node exporter.
			unsupported = append(unsupported, m.Inst+" ("+m.Kind+")")
			continue
		}
		pmmFQDN, pmmUser, pmmPass, ok := a.pmmServerFor(st, doc, in.PMMNodeID)
		if !ok {
			pr.logln(m.Inst + ": PMM server not available yet — monitoring not registered")
			continue
		}
		_, port, dbUser, dbPass := aioInstanceCreds(dep, m)
		l := aioLayout(m.Inst, m.Kind, m.Ports)
		env := []string{
			"ENGINE=" + engine,
			"SVC=" + n.Label + "-" + m.Inst, // unique per instance, not per node
			"SOCK=" + l.Sock,
			fmt.Sprintf("PORT=%d", port),
			"PMM_URL=" + pmmServerURL(pmmFQDN, pmmUser, pmmPass),
			"PMM_PW=" + sec.MonitorPassword,
			"DB_USER=" + dbUser, "DB_PW=" + dbPass,
		}
		if engine == "mysql" {
			// The MySQL path creates the pmm account as root over the socket.
			env = append(env, "DB_USER="+sec.RootUser, "DB_PW="+sec.RootPassword)
		}
		if err := a.runStep(ctx, id, aioPMMAddScript, env, pr.logln); err != nil {
			pr.logln(m.Inst + ": PMM registration failed: " + err.Error())
			continue
		}
		registered++
	}
	if registered > 0 {
		pr.logln(fmt.Sprintf("%d instance(s) registered with PMM", registered))
	}
	if len(unsupported) > 0 {
		pr.logln("no PMM service exporter for " + strings.Join(unsupported, ", ") +
			" — the node's OS metrics are still collected, but there is no per-service dashboard")
	}
}
