package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// mongosearch.go — full-text and vector search for Percona Server for MongoDB, which
// is a second process: Percona Search for MongoDB, `mongot`.
//
// mongod does not search anything itself. It forwards `$search`, `$vectorSearch` and
// `$searchMeta` to mongot over gRPC, gets back document ids and scores, and loads the
// documents. mongot in turn keeps its Lucene indexes current by tailing a change stream
// on the replica set it serves, as a user with the `searchCoordinator` role. So turning
// search on is four things that have to agree with each other:
//
//   - mongod's setParameter block names mongot's address — `mongotHost` for queries and
//     `searchIndexManagementHostAndPort` for createSearchIndex & co. Both are startup
//     parameters, which is why this is a design-time choice: they are written into
//     mongod.conf before mongod's first start, and mongod is happy to start before the
//     mongot it names exists (it connects lazily).
//   - a `searchCoordinator` user exists on the replica set mongot syncs from.
//   - mongot itself, installed from Percona's `ps4m` repository, configured with that
//     user and the replica set's members, and running.
//   - on a sharded cluster, mongos carries the same parameters (index management issued
//     through the router has to reach a mongot), and each shard's mongot also opens a
//     connection to mongos.
//
// The layout is the Percona Operator's, verified against its 1.23.0 source
// (pkg/psmdb/vectorsearch): ONE mongot per data-bearing replica set — the operator
// allows no more — and none for the config servers. On a canvas there are no pods to
// give it, so it is co-located on the replica set's first member (in label order).
// That member's container is then the only place search runs: stop it and $search
// fails while every plain query keeps working, which is the honest picture of a
// single-mongot deployment and worth being able to show.
//
// mongot needs PSMDB 8.3 or later, and needs a replica set — it reads a change stream,
// which a standalone mongod does not have. Hence the option lives on the replica-set
// and sharded frames only; a one-member replica set is the smallest thing that works.

const (
	// mongotGRPCPort is where mongod (and mongos) reach mongot.
	mongotGRPCPort = 27028
	// mongotHealthPort answers GET /health with {"status":"SERVING"} once mongot is up.
	mongotHealthPort = 8080
	// mongotMetricsPort is mongot's Prometheus endpoint.
	mongotMetricsPort = 9946
	// mongoSearchUser is the account mongot authenticates to the replica set (and to mongos)
	// as. Not "mongotUser" like the package's sample config, so it reads as ours in a
	// listing of users — and not the operator's "searchCoordinator", which is a role name.
	mongoSearchUser = "mongot"
	// mongoSearchMinMajor is the first PSMDB series that can talk to mongot.
	mongoSearchMinMajor = "8.3"
	// mongoSearchRepo is the percona-release repository Percona Search for MongoDB ships in.
	mongoSearchRepo = "ps4m"
)

// mongoSearchOS lists the images Percona publishes percona-search-mongodb for (repo
// ps4m: el8, el9, bookworm, jammy, noble). Oracle Linux 10 and Debian 13 have PSMDB 8.3
// packages but no mongot — refusing them at validation is far kinder than failing a
// 290 MB install twenty minutes into a deploy.
var mongoSearchOS = map[string]bool{
	"oraclelinux/8": true, "oraclelinux/9": true,
	"ubuntu/22.04": true, "ubuntu/24.04": true,
	"debian/12": true,
}

// mongoSearchMajorOK reports whether a PSMDB series can run with mongot.
func mongoSearchMajorOK(major string) bool {
	m := strings.TrimSpace(major)
	return m != "" && compareVersions(m, mongoSearchMinMajor) >= 0
}

// mongoSearchIssues validates the Vector search option on a psmrs or psmdb frame.
func mongoSearchIssues(f designFrame) []issue {
	if !f.VectorSearch {
		return nil
	}
	kind := "replica set"
	if f.Type == "psmdb" {
		kind = "sharded cluster"
	}
	var out []issue
	if !mongoSearchMajorOK(f.PSMDBMajor) {
		out = append(out, issue{Level: "error", Message: "PS MongoDB " + kind + " " + f.Label +
			" has vector search on, which needs Percona Server for MongoDB " + mongoSearchMinMajor +
			" or later (mongot cannot attach to " + orDefault(f.PSMDBMajor, "8.0") + ") — choose the " +
			mongoSearchMinMajor + " series, or run `ONLY=percona make versions` if it is not offered"})
	}
	if !mongoSearchOS[f.OS+"/"+f.OSVersion] {
		out = append(out, issue{Level: "error", Message: "PS MongoDB " + kind + " " + f.Label +
			" has vector search on, but Percona Search for MongoDB (mongot) is not published for " +
			f.OS + " " + f.OSVersion + " — use Oracle Linux 8/9, Ubuntu 22.04/24.04 or Debian 12"})
	}
	return out
}

// mongoSearchSetParams is the setParameter body (no header) that points a mongod or
// mongos at a mongot. The keys and values are exactly the operator's
// buildSearchSetParameters for a cluster without TLS.
func mongoSearchSetParams(mongotAddr string) string {
	return fmt.Sprintf(`  mongotHost: %[1]s
  searchIndexManagementHostAndPort: %[1]s
  skipAuthenticationToSearchIndexManagementServer: false
  skipAuthenticationToMongot: false
  searchTLSMode: disabled
  useGrpcForSearch: true
`, mongotAddr)
}

// mergeSetParams folds several rendered "setParameter:" blocks into one. mongod.conf
// can hold only one setParameter key — a second is a duplicate YAML key and mongod
// refuses to start — so a node that needs both the MClusterAdmin mechanisms and the
// search parameters needs them under a single header. A block passed without the
// header is taken as body lines.
func mergeSetParams(blocks ...string) string {
	var body strings.Builder
	for _, b := range blocks {
		b = strings.TrimPrefix(b, "setParameter:\n")
		if strings.TrimSpace(b) == "" {
			continue
		}
		body.WriteString(strings.TrimRight(b, "\n") + "\n")
	}
	if body.Len() == 0 {
		return ""
	}
	return "setParameter:\n" + body.String()
}

// mongotConfigYAML renders /etc/mongot/mongot.yml for one replica set. rsHosts are the
// replica set's members (mongot picks the one to tail by itself); routerHosts are the
// mongos routers of a sharded cluster, empty for a plain replica set. Everything listens
// on all interfaces because the other members' mongod reach this mongot over the stack
// network — the package's default of localhost only works when mongot serves one mongod.
func mongotConfigYAML(rsHosts, routerHosts []string) string {
	var b strings.Builder
	b.WriteString("# Written by DBCanvas — Percona Search for MongoDB (mongot) for this replica set.\n")
	b.WriteString("syncSource:\n  replicaSet:\n    hostAndPort:\n")
	for _, h := range rsHosts {
		fmt.Fprintf(&b, "      - %q\n", h)
	}
	b.WriteString(mongotScramAuth("    "))
	if len(routerHosts) > 0 {
		b.WriteString("  router:\n    hostAndPort:\n")
		for _, h := range routerHosts {
			fmt.Fprintf(&b, "      - %q\n", h)
		}
		b.WriteString(mongotScramAuth("    "))
	}
	fmt.Fprintf(&b, `storage:
  dataPath: "/var/lib/mongot"
server:
  grpc:
    address: "0.0.0.0:%d"
    tls:
      mode: "disabled"
metrics:
  enabled: true
  address: "0.0.0.0:%d"
healthCheck:
  address: "0.0.0.0:%d"
logging:
  verbosity: INFO
  logPath: /var/log/mongot/mongot.log
`, mongotGRPCPort, mongotMetricsPort, mongotHealthPort)
	return b.String()
}

func mongotScramAuth(ind string) string {
	return ind + "scramAuth:\n" +
		ind + "  username: " + mongoSearchUser + "\n" +
		ind + "  passwordFile: \"/etc/mongot/secrets/passwordFile\"\n" +
		ind + "  authSource: admin\n" +
		ind + "  tls:\n" +
		ind + "    enabled: false\n"
}

// mongoSearchUserJS creates (or re-passwords) the account mongot uses. searchCoordinator
// is the built-in role PSMDB 8.3 ships for exactly this: read every collection, open
// change streams, and write the search catalog in __mdb_internal_search.
func mongoSearchUserJS(pass string) string {
	pw, _ := json.Marshal(pass)
	return fmt.Sprintf(`var a=db.getSiblingDB("admin");
if (a.getUser(%[1]q)) { a.updateUser(%[1]q,{pwd:%[2]s,roles:[{role:"searchCoordinator",db:"admin"}]}) }
else { a.createUser({user:%[1]q,pwd:%[2]s,roles:[{role:"searchCoordinator",db:"admin"}]}) }
print("search user ready")`, mongoSearchUser, string(pw))
}

// mongoEnsureSearchUser creates the mongot account on a replica-set primary — the
// replica set's own, or (sharded) a shard's or the config RS's, which is where mongos
// looks users up. Same admin-or-localhost-exception path as the PMM user.
func (a *App) mongoEnsureSearchUser(ctx context.Context, st Stack, node designNode, sec mongoSecrets, pr *pxcProg) error {
	dep, err := a.store.GetDeployment(st.ID, node.ID)
	if err != nil || dep.ContainerID == "" {
		return pr.fail("create search user: %s has no container", node.Label)
	}
	env := []string{"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword, "USER_JS=" + mongoSearchUserJS(sec.SearchPassword)}
	if err := a.runStep(ctx, dep.ContainerID, mongoAdminEvalScript, env, pr.logln); err != nil {
		return pr.fail("create search user: %v", err)
	}
	return nil
}

// mongoSetupSearch installs, configures and starts mongot on host — the replica set's
// first member — and waits for its health check to say SERVING. It runs after the
// replica set is initiated and the search user exists: mongot crash-loops until it can
// authenticate, and while systemd would eventually get it there, a deploy log that says
// "SERVING" is worth more than one that says "started".
func (a *App) mongoSetupSearch(ctx context.Context, st Stack, frame designFrame, host designNode, rsHosts, routerHosts []string, sec mongoSecrets, pr *pxcProg) error {
	dep, err := a.store.GetDeployment(st.ID, host.ID)
	if err != nil || dep.ContainerID == "" {
		return pr.fail("vector search: %s has no container", host.Label)
	}
	id := dep.ContainerID
	pr.phase("Installing Percona Search for MongoDB (mongot)", 90)
	script := mongotInstallRHEL
	if isDebianOS(frame.OS) {
		script = mongotInstallDebian
	}
	if err := a.runStep(ctx, id, script, []string{"REPO=" + mongoSearchRepo}, pr.logln); err != nil {
		return pr.fail("install percona-search-mongodb: %v", err)
	}
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/etc/mongot", "mongot.yml", 0o600, []byte(mongotConfigYAML(rsHosts, routerHosts))); err != nil {
		return pr.fail("write mongot.yml: %v", err)
	}
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/etc/mongot/secrets", "passwordFile", 0o400, []byte(sec.SearchPassword)); err != nil {
		return pr.fail("write mongot password file: %v", err)
	}
	pr.phase("Starting mongot", 93)
	if err := a.runStep(ctx, id, mongotStartScript, nil, pr.logln); err != nil {
		return pr.fail("start mongot: %v", err)
	}
	pr.logln(fmt.Sprintf("mongot SERVING on %s:%d (health :%d, metrics :%d), syncing from %s",
		host.Label, mongotGRPCPort, mongotHealthPort, mongotMetricsPort, strings.Join(rsHosts, ",")))
	return nil
}

// mongotInstall{RHEL,Debian} install the percona-search-mongodb package from the ps4m
// repository. It is about 290 MB — mongot is a Java service with its own JDK — which is
// the one slow step of a search-enabled deploy; the Intranet proxy and a Repository node
// both cache it for the next one.
const mongotInstallRHEL = `set -e
percona-release enable -y "$REPO" release >/dev/null 2>&1 || percona-release enable "$REPO" release >/dev/null 2>&1 || percona-release setup -y "$REPO" >/dev/null 2>&1
rpm -q percona-search-mongodb >/dev/null 2>&1 || dnf -y -q install percona-search-mongodb
mongot --version 2>/dev/null | head -1 || true`

const mongotInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release enable -y "$REPO" release >/dev/null 2>&1 || percona-release enable "$REPO" release >/dev/null 2>&1 || percona-release setup -y "$REPO" >/dev/null 2>&1
apt-get update -qq >/dev/null
dpkg -s percona-search-mongodb >/dev/null 2>&1 || apt-get install -y -qq percona-search-mongodb >/dev/null
mongot --version 2>/dev/null | head -1 || true`

// mongotStartScript fixes ownership (mongot refuses a password file anyone else can
// read), starts the packaged unit and waits for the health check. A first start pulls
// JNI libraries out of the bundle and replays the change stream, hence the minute.
const mongotStartScript = `set -e
install -d -o mongod -g mongod -m 0750 /var/lib/mongot /var/lib/mongot/tmp /var/log/mongot /etc/mongot /etc/mongot/secrets
chown mongod:mongod /etc/mongot/mongot.yml /etc/mongot/secrets/passwordFile
chmod 600 /etc/mongot/mongot.yml
chmod 400 /etc/mongot/secrets/passwordFile
command -v restorecon >/dev/null 2>&1 && restorecon -R /usr/lib/percona-search-mongodb /var/lib/mongot >/dev/null 2>&1 || true
systemctl daemon-reload
systemctl enable mongot >/dev/null 2>&1 || true
systemctl restart mongot
OK=0
for i in $(seq 1 90); do
  if curl -fsS http://127.0.0.1:8080/health 2>/dev/null | grep -q SERVING; then OK=1; break; fi
  sleep 2
done
[ "$OK" = 1 ] || { echo "mongot did not report SERVING:"; systemctl status mongot --no-pager 2>&1 | tail -5; tail -30 /var/log/mongot/mongot.log 2>/dev/null; journalctl -u mongot --no-pager -n 30 2>/dev/null; exit 1; }
echo "mongot health: $(curl -fsS http://127.0.0.1:8080/health)"`

// mongoProbeSearch is not a wait on mongot (mongotStartScript already did that) but
// on the round trip: a search-index command through mongod, which is what proves the
// setParameter half is right too. Best-effort and logged — an index command that fails
// here will fail the same way in the app, with the same message.
func (a *App) mongoProbeSearch(ctx context.Context, containerID string, sec mongoSecrets, logln func(string)) {
	js := `try { const r = db.getSiblingDB("admin").runCommand({listSearchIndexes: "dbcanvas_probe", cursor: {}}); print(r.ok ? "search index API reachable through mongod" : JSON.stringify(r)) } catch (e) { print("search probe: " + e.message) }`
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"mongosh", "--quiet", "--port", "27017",
			"-u", sec.AdminUser, "-p", sec.AdminPassword, "--authenticationDatabase", "admin", "--eval", js}, nil)
		out := ""
		if err == nil {
			out = strings.TrimSpace(res.Stdout)
		}
		if strings.Contains(out, "reachable") || time.Now().After(deadline) {
			if out != "" {
				logln(out)
			}
			return
		}
		time.Sleep(3 * time.Second)
	}
}

// depContainer is the container a node's deployment currently runs in ("" when none).
func (a *App) depContainer(stackID int64, nodeID string) string {
	if dep, err := a.store.GetDeployment(stackID, nodeID); err == nil {
		return dep.ContainerID
	}
	return ""
}
