package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Spock PostgreSQL cluster frame (Type=="spock"). A multi-master, active-active
// PostgreSQL cluster using pgEdge's Spock logical-replication extension
// (https://github.com/pgEdge/spock). Every member is a writable node; each is a Spock
// node and subscribes to every other node (full mesh), so a write on ANY member
// replicates to all the others. No primary/standby, no failover election. Conflicts are
// resolved last-update-wins (track_commit_timestamp=on).
//
// Spock requires a *patched* PostgreSQL — it ships PostgreSQL source patches
// (patches/<major>/pg<major>-*.diff, e.g. the logical commit clock) that stock PGDG
// binaries don't have. So this frame compiles PostgreSQL from source (postgresql.org
// REL_<major>_STABLE) with Spock's patches applied, then builds the Spock extension
// against it — all self-contained under /usr/pgsql-<major> (Oracle Linux only).

// spockDefaultDB is the demo database created on every node and set up for replication.
const spockDefaultDB = "spockdemo"

// spockService is the systemd unit for the source-built PostgreSQL on each member.
const spockService = "postgresql-spock"

// spockRef is the git ref of pgEdge/spock to build (a tag whose patches match the PG
// major). Overridable via SPOCK_REF. v5.0.10 is the validated default (PG 15/16/17).
func spockRef() string { return envOr("SPOCK_REF", "v5.0.10") }

// spockPGRef maps a selected Percona PostgreSQL minor (e.g. "18.1-2") to the
// matching postgresql.org git tag ("REL_18_1") so the source build honours the
// pinned minor. An empty version falls back to the major's stable branch tip
// (REL_<major>_STABLE = latest minor), preserving the previous "latest" default.
func spockPGRef(major, version string) string {
	v := version
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i] // drop Percona packaging suffix ("18.1-2" → "18.1")
	}
	if v = strings.TrimSpace(v); v == "" {
		return "REL_" + ppgMajorOf(major) + "_STABLE"
	}
	return "REL_" + strings.ReplaceAll(v, ".", "_")
}

func spockPrefix(major string) string  { return "/usr/pgsql-" + ppgMajorOf(major) }
func spockDataDir(major string) string { return "/var/lib/pgsql/" + ppgMajorOf(major) + "/data" }

// spockConfig is the non-secret profile shown for a deployed Spock member.
type spockConfig struct {
	Cluster      string   `json:"cluster"`
	Image        string   `json:"image"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Hostname     string   `json:"hostname"`
	FQDN         string   `json:"fqdn"`
	PGMajor      string   `json:"pgMajor"`
	PGVersion    string   `json:"pgVersion"`
	NodeName     string   `json:"nodeName"` // Spock node name
	Database     string   `json:"database"` // replicated demo database
	Members      []string `json:"members"`  // all member FQDNs (mesh peers)
	SpockRef     string   `json:"spockRef"`
	GenerateCert bool     `json:"generateCert"`
	UseProxy     bool     `json:"useProxy"`
	MonitoredBy  string   `json:"monitoredBy"`
	Ports        []int    `json:"ports"`
	ExportPort   int      `json:"exportPort"`
}

// spockNodeName derives a valid Spock node identifier from a member host.
func spockNodeName(host string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		s = "n" + s
	}
	return s
}

// spockDSN builds a libpq connection string a peer uses to reach `fqdn`'s database.
func spockDSN(fqdn, db string, sec pgSecrets) string {
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s", fqdn, patroniPGPort, db, sec.SuperUser, sec.SuperPassword)
}

// provisionSpockFrame brings up a full-mesh, active-active Spock cluster: compile
// patched PostgreSQL + Spock and configure logical replication per member (parallel),
// create the Spock node + demo schema + replication set on each, then create the full
// mesh of subscriptions so every node replicates to every other.
func (a *App) provisionSpockFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)

	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "spock" {
			members = append(members, n)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	if len(members) == 0 {
		return
	}

	sec := pgFamilySecrets()
	secJSON, _ := json.Marshal(sec)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	major := ppgMajorOf(frame.PGMajor)
	db := spockDefaultDB

	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, n := range doc.Nodes {
			if n.ID == frame.PMMNodeID && n.Type == "pmm" {
				monitoredBy = fqdnOf(hosts[n.ID], domain)
			}
		}
	}
	var memberFQDNs []string
	for _, n := range members {
		memberFQDNs = append(memberFQDNs, fqdnOf(hosts[n.ID], domain))
	}

	for _, n := range members {
		host := hosts[n.ID]
		cfg := spockConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			Hostname: host, FQDN: fqdnOf(host, domain),
			PGMajor: major, PGVersion: frame.PGVersion,
			NodeName: spockNodeName(host), Database: db, Members: memberFQDNs, SpockRef: spockRef(),
			GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy, MonitoredBy: monitoredBy,
			Ports: []int{patroniPGPort},
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID)
	go func() {
		defer endScope()
		for _, n := range members {
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			a.pxcNewProg(st.ID, n.ID).phase("Waiting for Intranet to be ready", 5)
		}
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			for _, n := range members {
				a.pxcNewProg(st.ID, n.ID).fail("%v", werr)
			}
			return
		}

		// ---- Phase 1 (parallel): container + compile patched PostgreSQL + Spock +
		// configure + start PostgreSQL with logical replication enabled. ----
		var wg sync.WaitGroup
		failed := make(map[string]bool)
		var mu sync.Mutex
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.spockPrepareNode(ctx, st, frame, n, major, image, intranetIP, domain, sec); err != nil {
					mu.Lock()
					failed[n.ID] = true
					mu.Unlock()
				}
			}(n)
		}
		wg.Wait()
		if len(failed) > 0 {
			return
		}
		a.reconcileStackDNS(ctx, st.ID)

		// ---- Phase 2 (parallel): create the Spock node + demo schema + replication set
		// on every member. All nodes must exist before any subscription is created. ----
		var wg2 sync.WaitGroup
		for _, n := range members {
			wg2.Add(1)
			go func(n designNode) {
				defer wg2.Done()
				pr := a.pxcNewProg(st.ID, n.ID)
				pr.phase("Creating Spock node + replication set", 88)
				host := hosts[n.ID]
				dep, _ := a.store.GetDeployment(st.ID, n.ID)
				env := []string{
					"DB=" + db, "NODE=" + spockNodeName(host),
					"DSN=" + spockDSN(fqdnOf(host, domain), db, sec),
				}
				if err := a.runStep(ctx, dep.ContainerID, spockNodeSetupScript, env, pr.logln); err != nil {
					mu.Lock()
					failed[n.ID] = true
					mu.Unlock()
					pr.fail("create spock node: %v", err)
				}
			}(n)
		}
		wg2.Wait()
		if len(failed) > 0 {
			return
		}

		// ---- Phase 3: full-mesh subscriptions — on each node, subscribe to every other
		// node. forward_origins='{}' makes each node forward only its own changes, so in
		// the mesh every change reaches every node exactly once (no loops). ----
		for _, n := range members {
			pr := a.pxcNewProg(st.ID, n.ID)
			pr.phase("Subscribing to peers", 92)
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			selfNode := spockNodeName(hosts[n.ID])
			for _, peer := range members {
				if peer.ID == n.ID {
					continue
				}
				peerNode := spockNodeName(hosts[peer.ID])
				env := []string{
					"DB=" + db,
					"SUB=sub_" + selfNode + "_" + peerNode,
					"DSN=" + spockDSN(fqdnOf(hosts[peer.ID], domain), db, sec),
				}
				if err := a.runStep(ctx, dep.ContainerID, spockSubCreateScript, env, pr.logln); err != nil {
					pr.logln("subscription to " + peerNode + " failed: " + err.Error())
				}
			}
			pr.logln("subscribed to " + strconv.Itoa(len(members)-1) + " peer(s)")
		}

		// ---- Phase 4: PMM + finalize ----
		for _, n := range members {
			pr := a.pxcNewProg(st.ID, n.ID)
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 96)
				a.patroniRegisterPMM(ctx, st, n, frame, doc, sec, pr) // generic postgres PMM register
			}
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: dep.ContainerID, State: DeployRunning, Config: dep.Config, Secrets: secJSON})
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d spock %s: provisioned (%d node(s), active-active mesh)", st.ID, frame.Label, len(members))
	}()
}

// spockPrepareNode creates the container, compiles a patched PostgreSQL + the Spock
// extension from source, initialises the data dir, configures postgresql.conf for
// logical replication (+ pg_hba + optional TLS), and starts PostgreSQL with the
// superuser password set. Oracle Linux only.
func (a *App) spockPrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, major, image, intranetIP, domain string, sec pgSecrets) error {
	pr := a.pxcNewProg(st.ID, n.ID)
	if isDebianOS(frame.OS) {
		return pr.fail("Spock frames are supported on Oracle Linux only (PostgreSQL is compiled from source)")
	}
	host := stackHostnames(buildDoc(st))[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)
	prefix := spockPrefix(major)
	dataDir := spockDataDir(major)

	pr.phase("Creating container", 12)
	name := containerName(st.ID, n.ID)
	if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
		a.engCtx(ctx).ContainerRemove(ctx, cid)
	}
	spec := ContainerSpec{
		Name: name, Image: image, Hostname: host, Privileged: true,
		Network: networkName(st.ID), Aliases: []string{host},
		DNS: []string{intranetIP}, DNSSearch: []string{domain},
	}
	if n.ExportEnabled {
		spec.PublishMap = []PortMap{{ContainerPort: patroniPGPort, HostPort: n.ExportHostPort}}
	}
	id, err := a.engCtx(ctx).ContainerCreate(ctx, spec)
	if err != nil {
		return pr.fail("create container: %v", err)
	}
	if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
		return pr.fail("start container: %v", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)

	var cfg spockConfig
	if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil {
		json.Unmarshal(dep.Config, &cfg)
	}
	if n.ExportEnabled {
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", patroniPGPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.ExportPort = p
			}
		}
	}
	secJSON, _ := json.Marshal(sec)
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

	pr.phase("Waiting for systemd", 18)
	if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
		return pr.fail("systemd did not start: %v", err)
	}
	a.trustIntranetCA(ctx, st, id, frame.OS, pr.logln)
	a.ensureDNFIPv4(ctx, id, frame.OS, pr.logln)

	if frame.UseProxy {
		if err := a.runStep(ctx, id, pkgProxyRHEL, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
			return pr.fail("configure package proxy: %v", err)
		}
		pr.logln("package egress via Intranet proxy")
	}

	pr.phase("Installing build toolchain", 22)
	if err := a.runStep(ctx, id, spockBuildDepsRHEL, []string{"EPELPKG=" + epelPackage(frame.OSVersion)}, pr.logln); err != nil {
		return pr.fail("install build dependencies: %v", err)
	}
	pr.logln("build toolchain + PostgreSQL build deps installed")

	pr.phase("Compiling patched PostgreSQL + Spock (this takes a while)", 30)
	pgRef := spockPGRef(major, frame.PGVersion)
	buildEnv := []string{
		"PGMAJOR=" + major, "PGREF=" + pgRef,
		"SPOCK_REF=" + spockRef(), "PREFIX=" + prefix,
	}
	if err := a.runStep(ctx, id, spockCompileScript, buildEnv, pr.logln); err != nil {
		return pr.fail("compile PostgreSQL/Spock: %v", err)
	}
	pr.logln("PostgreSQL " + pgRef + " (patched) + Spock " + spockRef() + " compiled + installed")
	a.ensureRsyslog(ctx, id, frame.OS, pr.logln)

	if frame.PMMNodeID != "" {
		pr.phase("Installing PMM client", 60)
		if err := a.runStep(ctx, id, pxcInstallPMMClientRHEL, nil, pr.logln); err != nil {
			pr.logln("pmm-client install skipped: " + err.Error())
		} else {
			pr.logln("pmm-client installed")
		}
	}

	pr.phase("Initialising PostgreSQL", 64)
	if err := a.runStep(ctx, id, spockInitScript, []string{"PREFIX=" + prefix, "DATADIR=" + dataDir}, pr.logln); err != nil {
		return pr.fail("initdb: %v", err)
	}
	if frame.GenerateCert {
		pr.phase("Issuing certificate", 66)
		if err := a.pgApplyCert(ctx, id, a.intranetContainerID(ctx, st), fqdn, dataDir, frame.CertTTLValue, frame.CertTTLUnit, pr.logln); err != nil {
			return pr.fail("%v", err)
		}
	}
	confEnv := []string{"CONFDIR=" + dataDir, "DATADIR=" + dataDir}
	if frame.GenerateCert {
		confEnv = append(confEnv, "TLS=1")
	}
	pr.phase("Configuring logical replication", 70)
	if err := a.runStep(ctx, id, spockConfigureScript, confEnv, pr.logln); err != nil {
		return pr.fail("configure postgresql.conf: %v", err)
	}
	if err := a.runStep(ctx, id, spockStartScript, []string{"SERVICE=" + spockService, "PREFIX=" + prefix, "DATADIR=" + dataDir}, pr.logln); err != nil {
		return pr.fail("start PostgreSQL: %v", err)
	}
	if err := a.runStep(ctx, id, pgSetPasswordScript, []string{"SUPERPW=" + sec.SuperPassword}, pr.logln); err != nil {
		return pr.fail("set superuser password: %v", err)
	}
	pr.logln("PostgreSQL running with wal_level=logical + spock preloaded")
	return nil
}

// waitSpockRunning blocks until every member of a Spock frame is running, then returns
// the member FQDNs + shared secrets (for a future round-robin HAProxy association).
func (a *App) waitSpockRunning(ctx context.Context, stackID int64, frame designFrame, doc designDoc, domain string, timeout time.Duration) ([]string, pgSecrets, error) {
	hosts := stackHostnames(doc)
	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "spock" {
			members = append(members, n)
		}
	}
	if len(members) == 0 {
		return nil, pgSecrets{}, fmt.Errorf("Spock cluster %s has no members", frame.Label)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allRunning := true
		var sec pgSecrets
		for _, n := range members {
			dep, err := a.store.GetDeployment(stackID, n.ID)
			if err != nil {
				allRunning = false
				break
			}
			if dep.State == DeployError {
				return nil, pgSecrets{}, fmt.Errorf("Spock cluster %s failed to provision", frame.Label)
			}
			if dep.State != DeployRunning {
				allRunning = false
				break
			}
			json.Unmarshal(dep.Secrets, &sec)
		}
		if allRunning {
			var fqdns []string
			for _, n := range members {
				fqdns = append(fqdns, fqdnOf(hosts[n.ID], domain))
			}
			return fqdns, sec, nil
		}
		time.Sleep(3 * time.Second)
	}
	return nil, pgSecrets{}, fmt.Errorf("Spock cluster %s did not become ready within %s", frame.Label, timeout)
}

// ------------------------------------------------------------------ scripts

// spockBuildDepsRHEL installs the build toolchain + PostgreSQL build dependencies from
// the base repos + CodeReady Builder (CRB) + EPEL (perl(IPC::Run), jansson-devel etc.).
const spockBuildDepsRHEL = `set -e
dnf -y -q install dnf-plugins-core >/dev/null 2>&1 || true
elver=$(rpm -E %rhel)
dnf config-manager --set-enabled "ol${elver}_codeready_builder" >/dev/null 2>&1 || true
dnf config-manager --set-enabled crb >/dev/null 2>&1 || true
dnf config-manager --set-enabled powertools >/dev/null 2>&1 || true
dnf -y -q install "$EPELPKG" >/dev/null 2>&1 || dnf -y -q install epel-release >/dev/null 2>&1 || true
dnf -y -q install gcc make git bison flex patch redhat-rpm-config \
  readline-devel zlib-devel openssl-devel libxml2-devel libicu-devel lz4-devel libzstd-devel krb5-devel \
  perl-devel perl-IPC-Run 'perl(FindBin)' jansson-devel >/dev/null`

// spockCompileScript compiles PostgreSQL from source (postgresql.org $PGREF) with Spock's
// patches applied, installs it to $PREFIX, then builds + installs the Spock extension
// against it, and symlinks the client tools onto PATH. Idempotent: a redeploy where the
// build already exists is a no-op.
const spockCompileScript = `set -e
PREFIX=${PREFIX:-/usr/pgsql-$PGMAJOR}
link_tools() { for b in psql createdb pg_isready pg_dump pg_ctl pg_config; do ln -sf "$PREFIX/bin/$b" "/usr/local/bin/$b"; done; }
if [ -f "$PREFIX/lib/spock.so" ] && [ -x "$PREFIX/bin/postgres" ]; then link_tools; echo "PostgreSQL + Spock already built at $PREFIX"; exit 0; fi
rm -rf /usr/src/postgres /usr/src/spock
git clone --depth 1 --branch "$PGREF" https://github.com/postgres/postgres /usr/src/postgres >/tmp/pg-clone.log 2>&1 || { echo "clone postgres ($PGREF) failed:"; tail -6 /tmp/pg-clone.log; exit 1; }
git clone --depth 1 --branch "$SPOCK_REF" https://github.com/pgEdge/spock /usr/src/spock >/tmp/sp-clone.log 2>&1 || { echo "clone spock ($SPOCK_REF) failed:"; tail -6 /tmp/sp-clone.log; exit 1; }
cd /usr/src/postgres
for p in /usr/src/spock/patches/$PGMAJOR/pg$PGMAJOR-*.diff; do
  [ -e "$p" ] || { echo "no Spock patches for PG $PGMAJOR"; exit 1; }
  patch -p1 --forward --fuzz=3 < "$p" >/tmp/patch.log 2>&1 || { echo "apply $(basename "$p") failed:"; tail -8 /tmp/patch.log; exit 1; }
done
./configure --prefix="$PREFIX" --with-openssl --with-libxml --with-icu --with-lz4 --with-zstd --with-gssapi >/tmp/pg-configure.log 2>&1 || { echo "configure failed:"; tail -15 /tmp/pg-configure.log; exit 1; }
NPROC=$(nproc 2>/dev/null || echo 2)
make -j"$NPROC" >/tmp/pg-make.log 2>&1 || { echo "make failed:"; tail -20 /tmp/pg-make.log; exit 1; }
make install >>/tmp/pg-make.log 2>&1 || { echo "make install failed:"; tail -10 /tmp/pg-make.log; exit 1; }
( cd contrib && make -j"$NPROC" install >>/tmp/pg-make.log 2>&1 ) || { echo "contrib install failed"; exit 1; }
cd /usr/src/spock
make USE_PGXS=1 with_llvm=no PG_CONFIG="$PREFIX/bin/pg_config" install >/tmp/spock-build.log 2>&1 || { echo "spock build failed:"; grep -iE "error:|fatal" /tmp/spock-build.log | head -8; exit 1; }
[ -f "$PREFIX/lib/spock.so" ] || { echo "spock.so not installed"; exit 1; }
link_tools
echo "PostgreSQL (patched) + Spock installed to $PREFIX"`

// spockInitScript creates the postgres OS user (if missing) and initialises the data
// directory. Idempotent: an already-initialised datadir is left alone.
const spockInitScript = `set -e
id postgres >/dev/null 2>&1 || useradd --system --home-dir /var/lib/pgsql --create-home --shell /bin/bash postgres
install -d -m 700 -o postgres -g postgres "$DATADIR"
if [ ! -s "$DATADIR/PG_VERSION" ]; then
  runuser -u postgres -- "$PREFIX/bin/initdb" -D "$DATADIR" -E UTF8 -k --auth-local=peer --auth-host=scram-sha-256 >/tmp/initdb.log 2>&1 || { echo "initdb failed:"; tail -8 /tmp/initdb.log; exit 1; }
fi`

// spockConfigureScript enables remote access + logical replication in postgresql.conf,
// preloads the spock library, enables commit-timestamp tracking (last-update-wins
// conflict resolution), and opens pg_hba for remote + replication connections (+ TLS).
// Appended last so these win over the defaults. $CONFDIR == the data dir for a source build.
const spockConfigureScript = `set -e
CONF="$CONFDIR/postgresql.conf"
HBA="$CONFDIR/pg_hba.conf"
[ -f "$CONF" ] || { echo "postgresql.conf not found at $CONF"; exit 1; }
grep -q "dbcanvas spock" "$CONF" 2>/dev/null || {
{
  echo ""
  echo "# --- dbcanvas spock ---"
  echo "listen_addresses = '*'"
  echo "port = 5432"
  echo "password_encryption = scram-sha-256"
  echo "wal_level = logical"
  echo "shared_preload_libraries = 'spock'"
  echo "track_commit_timestamp = on"
  echo "max_worker_processes = 16"
  echo "max_replication_slots = 16"
  echo "max_wal_senders = 16"
} >> "$CONF"
}
if [ -n "$TLS" ] && ! grep -q "^ssl = on" "$CONF"; then
  {
    echo "ssl = on"
    echo "ssl_cert_file = '$DATADIR/server.crt'"
    echo "ssl_key_file = '$DATADIR/server.key'"
    echo "ssl_ca_file = '$DATADIR/ca.crt'"
  } >> "$CONF"
fi
grep -q "dbcanvas-remote" "$HBA" 2>/dev/null || {
  {
    echo "# dbcanvas-remote"
    echo "host all all 0.0.0.0/0 scram-sha-256"
    echo "host replication all 0.0.0.0/0 scram-sha-256"
  } >> "$HBA"
}
chown -R postgres:postgres "$CONFDIR" 2>/dev/null || true`

// spockStartScript writes a systemd unit for the source-built PostgreSQL, (re)starts it,
// and waits for readiness.
const spockStartScript = `set -e
cat >/etc/systemd/system/$SERVICE.service <<UNIT
[Unit]
Description=PostgreSQL (Spock source build)
After=network.target
[Service]
Type=simple
User=postgres
ExecStart=$PREFIX/bin/postgres -D $DATADIR
ExecReload=/bin/kill -HUP \$MAINPID
KillMode=mixed
KillSignal=SIGINT
TimeoutStartSec=120
Restart=on-failure
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1 || true
systemctl reset-failed "$SERVICE" 2>/dev/null || true
systemctl restart "$SERVICE"
OK=0
for i in $(seq 1 30); do
  runuser -u postgres -- "$PREFIX/bin/pg_isready" -q && { OK=1; break; }
  sleep 1
done
[ "$OK" = 1 ] || { echo "postgres not ready:"; journalctl -u "$SERVICE" --no-pager 2>/dev/null | tail -15; exit 1; }`

// spockNodeSetupScript creates the demo database, the spock extension + local node, a
// demo table, and adds all public tables to the 'default' replication set. Idempotent:
// each step is guarded so a redeploy is a no-op. $DSN is how peers reach THIS node.
const spockNodeSetupScript = `set -e
PSQLDB() { runuser -u postgres -- psql -v ON_ERROR_STOP=1 -d "$DB" "$@"; }
runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_database WHERE datname='$DB'" | grep -q 1 || runuser -u postgres -- createdb "$DB"
PSQLDB -c "CREATE EXTENSION IF NOT EXISTS spock;"
EXISTS=$(PSQLDB -tAc "SELECT count(*) FROM spock.node WHERE node_name = '$NODE'")
if [ "$EXISTS" = 0 ]; then
  printf '%s\n' "SELECT spock.node_create(node_name := :'node', dsn := :'dsn');" | PSQLDB -v node="$NODE" -v dsn="$DSN"
fi
PSQLDB -c "CREATE TABLE IF NOT EXISTS public.spock_demo (id bigint PRIMARY KEY, note text, updated_at timestamptz DEFAULT now());"
PSQLDB -c "SELECT spock.repset_add_all_tables('default', ARRAY['public']);" 2>/dev/null || true
echo "spock node $NODE ready in database $DB"`

// spockSubCreateScript creates one subscription from THIS node to a peer ($DSN), for the
// full mesh. forward_origins='{}' → forward only local-origin changes (no loops). Guarded
// so a redeploy is a no-op. Structure/data are not synchronised (every node starts with
// the same empty schema).
const spockSubCreateScript = `set -e
PSQLDB() { runuser -u postgres -- psql -v ON_ERROR_STOP=1 -d "$DB" "$@"; }
EXISTS=$(PSQLDB -tAc "SELECT count(*) FROM spock.subscription WHERE sub_name = '$SUB'")
if [ "$EXISTS" = 0 ]; then
  printf '%s\n' "SELECT spock.sub_create(subscription_name := :'sub', provider_dsn := :'dsn', replication_sets := ARRAY['default','default_insert_only','ddl_sql'], synchronize_structure := false, synchronize_data := false, forward_origins := '{}');" | PSQLDB -v sub="$SUB" -v dsn="$DSN"
fi
echo "subscription $SUB ready"`
