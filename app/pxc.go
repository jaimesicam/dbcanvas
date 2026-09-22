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

// Percona XtraDB Cluster (PXC) frame. A PXC cluster is a group of nodes deployed
// on the systemd OS images (built by `make images`) with the percona-xtradb-cluster
// packages installed at deploy time. Regular nodes are Galera data members; an
// "arbitrator" node runs garbd (votes for quorum, stores no data). The first
// regular node is bootstrapped and the rest join via xtrabackup SST. GTID and a
// per-node certificate (signed by the Intranet CA, in /var/lib/mysql) are
// optional. Each node exposes the four Galera ports on the stack network and can
// publish its 3306 to the host.

// The four ports a PXC node uses on the stack network.
//
//	3306 — MySQL client/SQL    4567 — Galera group communication
//	4444 — SST (state transfer) 4568 — IST (incremental state transfer)
var pxcPorts = []int{3306, 4567, 4444, 4568}

// pxcConfig is the non-secret profile shown for a deployed PXC node.
type pxcConfig struct {
	Cluster      string `json:"cluster"`
	Image        string `json:"image"`
	OS           string `json:"os"`   // os family (oraclelinux | ubuntu | …) — drives config paths
	Role         string `json:"role"` // regular | arbitrator
	Hostname     string `json:"hostname"`
	FQDN         string `json:"fqdn"`
	ServerID     int    `json:"serverId"`
	PXCVersion   string `json:"pxcVersion"`
	Bootstrap    bool   `json:"bootstrap"`
	GTID         bool   `json:"gtid"`
	GenerateCert bool   `json:"generateCert"`
	UseProxy     bool   `json:"useProxy"`
	MonitoredBy  string `json:"monitoredBy"` // PMM node FQDN, if any
	Ports        []int  `json:"ports"`
	ExportPort   int    `json:"exportPort"` // published host port for 3306 (0 = none)
}

// pxcSecrets holds a PXC node's credentials (root is cluster-wide; app/repl come
// from the environment).
type pxcSecrets struct {
	RootUser        string `json:"rootUser"`
	RootPassword    string `json:"rootPassword"`
	AdminUser       string `json:"adminUser"`     // remote superuser admin@'%' (MYSQL_ADMIN_PASSWORD)
	AdminPassword   string `json:"adminPassword"` // root@localhost can't connect over the network
	AppUser         string `json:"appUser"`
	AppPassword     string `json:"appPassword"`
	ReplUser        string `json:"replUser"`
	ReplPassword    string `json:"replPassword"`
	MonitorUser     string `json:"monitorUser"`
	MonitorPassword string `json:"monitorPassword"`
	ClusterUser     string `json:"clusterUser"`     // cluster admin user (used by ProxySQL)
	ClusterPassword string `json:"clusterPassword"` // from CLUSTER_PASSWORD env
	// clustercheck@'localhost' (PROCESS priv) — read by the PXC clustercheck / mysqlchk
	// health endpoint HAProxy polls. Created in every MySQL-family node's baseline (like
	// the users above) so it exists cluster-wide without a post-baseline write.
	ClusterCheckUser     string `json:"clusterCheckUser"`
	ClusterCheckPassword string `json:"clusterCheckPassword"` // from CLUSTERCHECK_PASSWORD env
	// orchestrator@'%' — the topology user Percona Orchestrator connects as to discover
	// and monitor a cluster (SUPER, PROCESS, REPLICATION SLAVE/CLIENT, RELOAD). Created
	// by every replication baseline whether or not an Orchestrator node is linked, but
	// NOT by PXC: Orchestrator manages async/semi-sync replication only, and a Galera
	// cluster elects its own primary, so there is nothing there for it to do. This type
	// is shared by the whole MySQL family (see mysqlFamilySecrets), which is why the
	// fields live here. See app/orchestrator.go.
	OrchestratorUser     string `json:"orchestratorUser"`
	OrchestratorPassword string `json:"orchestratorPassword"` // from ORCHESTRATOR_PASSWORD env
}

func pxcImage(os, osVersion, arch string) string {
	return "dbcanvas-systemd:" + os + "-" + osVersion + "-" + archOr(arch)
}

// mysqlFamilySecrets builds the credential set for any MySQL-family engine (PXC,
// MySQL replication, InnoDB / Group Replication, standalone Percona Server). Every
// password comes exclusively from the environment (.env) — node-property and
// stored-secret overrides were removed, so a redeploy re-reads .env. root@localhost
// is the local superuser; admin@'%' (MYSQL_ADMIN_PASSWORD) is the network-reachable
// superuser, since root@localhost cannot connect over TCP.
func mysqlFamilySecrets() pxcSecrets {
	return pxcSecrets{
		RootUser: "root", RootPassword: envOr("MYSQL_ROOT_PASSWORD", "root_password"),
		AdminUser: "admin", AdminPassword: envOr("MYSQL_ADMIN_PASSWORD", "admin_password"),
		AppUser: "app", AppPassword: envOr("APP_PASSWORD", "app_password"),
		ReplUser: "repl", ReplPassword: envOr("REPL_PASSWORD", "repl_password"),
		MonitorUser: "monitor", MonitorPassword: envOr("MONITOR_PASSWORD", "monitor_password"),
		ClusterUser: "cluster", ClusterPassword: envOr("CLUSTER_PASSWORD", "cluster_password"),
		ClusterCheckUser: "clustercheck", ClusterCheckPassword: envOr("CLUSTERCHECK_PASSWORD", "cluster_password"),
		OrchestratorUser: "orchestrator", OrchestratorPassword: envOr("ORCHESTRATOR_PASSWORD", "orchestrator_password"),
	}
}

// mysqlAdminUserSQL is the SQL fragment that creates/updates the admin@'%' remote
// superuser. Shared by every MySQL-family bootstrap so the account is identical
// across engines. Env: $ADMIN_USER, $ADMIN_PW.
const mysqlAdminUserSQL = `CREATE USER IF NOT EXISTS '$ADMIN_USER'@'%' IDENTIFIED BY '$ADMIN_PW';
ALTER USER '$ADMIN_USER'@'%' IDENTIFIED BY '$ADMIN_PW';
GRANT ALL PRIVILEGES ON *.* TO '$ADMIN_USER'@'%' WITH GRANT OPTION;`

// pxcProduct maps a PXC major series to its percona-release product name.
func pxcProduct(major string) string {
	if major == "8.4" {
		return "pxc84lts"
	}
	return "pxc80"
}

// pxbProduct / pxbPackage map a PXC major series to the matching Percona
// XtraBackup percona-release product and package. XtraBackup performs the SST
// (state transfer) that joins nodes to the cluster, so every data node needs it.
func pxbProduct(major string) string {
	switch major {
	case "9.7":
		// Same story as psClientProduct: the repository exists (pxb-97-lts) but
		// percona-release cannot enable it, so the script writes it by hand.
		return ""
	case "8.4":
		return "pxb84lts"
	case "5.7":
		// Percona Server 5.7 pairs with the legacy XtraBackup 2.4 series.
		return "pxb-24"
	}
	return "pxb80"
}

// pxbRepoName is the repository directory for a XtraBackup series on the
// hand-written path (see pxcInstallXtrabackupRHEL).
func pxbRepoName(major string) string {
	switch major {
	case "9.7":
		return "pxb-97-lts"
	case "8.4":
		return "pxb-84-lts"
	case "5.7":
		return "pxb-24"
	}
	return "pxb-80"
}

func pxbPackage(major string) string {
	switch major {
	case "9.7":
		return "percona-xtrabackup-97"
	case "8.4":
		return "percona-xtrabackup-84"
	case "5.7":
		return "percona-xtrabackup-24"
	}
	return "percona-xtrabackup-80"
}

func isDebianOS(os string) bool { return os == "ubuntu" || os == "debian" }

func galeraProvider(os string) string {
	if isDebianOS(os) {
		return "/usr/lib/galera4/libgalera_smm.so"
	}
	return "/usr/lib64/galera4/libgalera_smm.so"
}

// pxcCnfPath is where DBCanvas writes the node's mysqld config. On RHEL the
// global /etc/my.cnf is read directly. On Debian /etc/my.cnf is read *first* but
// the package's /etc/mysql includes are read *after* and would override it (with
// an empty wsrep_cluster_address → every node bootstraps standalone), so we write
// a dedicated file under /etc/mysql and `!include` it last (see pxcDebianIncludeCnf).
func pxcCnfPath(os string) string {
	if isDebianOS(os) {
		return "/etc/mysql/dbcanvas.cnf"
	}
	return "/etc/my.cnf"
}

// pxcLogError is the mysqld error-log path. Debian's apparmor/package layout only
// permits /var/log/mysql, so use that there (also where any temporary password
// would be logged); RHEL uses /var/log/mysqld.log.
func pxcLogError(os string) string {
	if isDebianOS(os) {
		return "/var/log/mysql/error.log"
	}
	return "/var/log/mysqld.log"
}

// logUpdatesOption returns the right "replica updates" option for the version:
// log_replica_updates on 8.4 and 8.0.26+, log_slave_updates on older 8.0.
func logUpdatesOption(major, version string) string {
	if major == "8.4" {
		return "log_replica_updates=ON"
	}
	// 5.7 predates the log_replica_updates rename; it only has log_slave_updates.
	if major == "5.7" {
		return "log_slave_updates=ON"
	}
	// version like "8.0.45-36.1"; extract the patch number.
	patch := 99
	if parts := strings.SplitN(version, ".", 3); len(parts) == 3 {
		p := parts[2]
		if i := strings.IndexAny(p, "-."); i >= 0 {
			p = p[:i]
		}
		if v, err := strconv.Atoi(p); err == nil {
			patch = v
		}
	}
	if patch >= 26 {
		return "log_replica_updates=ON"
	}
	return "log_slave_updates=ON"
}

// pxcServerID derives a stable, unique server-id from a PXC node name.
func pxcServerID(name string) int { return serverIDFor(name) }

func fnv32(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// serverIDFor derives a stable MySQL server-id from a node's stack-unique
// hostname. It hashes the *full* name — not just the trailing number — so ids
// stay unique across clusters (e.g. mysql01 vs pxc01, which otherwise both got
// server-id 1). Cross-cluster async replication requires distinct ids: MySQL
// stops the replica I/O thread when source and replica share a server-id.
// Range 1..~268M keeps it a valid, positive server-id with negligible collision
// probability (validateStack still warns on the astronomically rare clash).
func serverIDFor(host string) int {
	return int(fnv32(host)%0xFFFFFFF) + 1
}

// --- progress helper (shared shape with the other provisioners) ---

type pxcProg struct {
	a       *App
	stackID int64
	nodeID  string
	p       *provProgress
}

func (a *App) pxcNewProg(stackID int64, nodeID string) *pxcProg {
	return &pxcProg{a: a, stackID: stackID, nodeID: nodeID, p: &provProgress{Phase: "Starting", Log: []string{}}}
}
func (pr *pxcProg) save() {
	b, _ := json.Marshal(pr.p)
	pr.a.store.SetDeploymentProgress(pr.stackID, pr.nodeID, b)
}
func (pr *pxcProg) phase(s string, n int) { pr.p.Phase = s; pr.p.Percent = n; pr.save() }
func (pr *pxcProg) logln(s string) {
	pr.p.Log = append(pr.p.Log, s)
	if len(pr.p.Log) > 200 {
		pr.p.Log = pr.p.Log[len(pr.p.Log)-200:]
	}
	pr.save()
}
func (pr *pxcProg) fail(format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	// A destroy cancels the deploy out from under the provisioners. The errors
	// that follow are caused by the teardown itself, so don't record them as node
	// failures — the deployment rows are about to be deleted, and writing them
	// here would resurrect the rows and wedge the next deploy in "error".
	if pr.a.deployCancelled(pr.stackID) {
		log.Printf("stack %d %s: aborted (stack destroyed): %s", pr.stackID, pr.nodeID, msg)
		return fmt.Errorf("%s", msg)
	}
	log.Printf("stack %d pxc %s: %s", pr.stackID, pr.nodeID, msg)
	pr.p.Phase = "failed"
	pr.p.Message = msg
	pr.save()
	pr.a.store.SetDeploymentState(pr.stackID, pr.nodeID, DeployError)
	pr.a.notifyStack(pr.stackID, "node.error", "error", "Node deployment failed", pr.nodeID+": "+msg, pr.nodeID)
	return fmt.Errorf("%s", msg)
}

// --- frame orchestration ---

// provisionPXCFrame brings up an entire PXC cluster frame: it records each
// member's deployment, creates every container (in parallel), forms the cluster
// (bootstrap the first regular node, then join the rest, then start garbd for
// arbitrators), creates the app/repl users, and optionally issues per-node certs.
func (a *App) provisionPXCFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)

	var regulars, arbiters []designNode
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != "pxc" {
			continue
		}
		if n.Role == "arbitrator" {
			arbiters = append(arbiters, n)
		} else {
			regulars = append(regulars, n)
		}
	}
	byLabel := func(s []designNode) { sort.Slice(s, func(i, j int) bool { return s[i].Label < s[j].Label }) }
	byLabel(regulars)
	byLabel(arbiters)
	members := append(append([]designNode{}, regulars...), arbiters...)

	// All credentials come from .env (re-read on every deploy).
	sec := mysqlFamilySecrets()
	secJSON, _ := json.Marshal(sec)

	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	var gcommHosts []string
	for _, n := range regulars {
		gcommHosts = append(gcommHosts, fqdnOf(hosts[n.ID], domain))
	}
	clusterAddr := strings.Join(gcommHosts, ",")
	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, n := range doc.Nodes {
			if n.ID == frame.PMMNodeID {
				monitoredBy = fqdnOf(hosts[n.ID], domain)
			}
		}
	}

	// Record every member as pending with its profile.
	for i, n := range members {
		host := hosts[n.ID]
		cfg := pxcConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Role: roleOf(n), Hostname: host, FQDN: fqdnOf(host, domain),
			ServerID: pxcServerID(host), PXCVersion: frame.PXCVersion, Bootstrap: i == 0 && n.Role != "arbitrator",
			GTID: frame.GTID, GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy, MonitoredBy: monitoredBy,
			Ports: pxcPorts,
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		baseProg := a.pxcNewProg(st.ID, members[0].ID)
		// A PXC cluster reaches its reset baseline as it forms (bootstrap creates the
		// credentials and clears GTID; joiners inherit that clean state via SST). This
		// deferred drain releases the stack-wide barrier for every member no matter how
		// the goroutine exits; the success path arrives them explicitly after Phase 2.
		barrier := a.deployBarrierFor(st.ID)
		if barrier != nil {
			defer func() {
				for _, n := range members {
					barrier.arrive(n.ID)
				}
			}()
		}
		for _, n := range members {
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			a.pxcNewProg(st.ID, n.ID).phase("Waiting for Intranet to be ready", 5)
		}
		intranetID, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			for _, n := range members {
				a.pxcNewProg(st.ID, n.ID).fail("%v", err)
			}
			return
		}

		// An encrypted cluster needs one certificate that every member carries identically, and
		// it has to exist before the first member starts — see pxcMyCnf. Minted once, here,
		// rather than per node: per-node certificates are precisely what PXC cannot use for
		// cluster traffic.
		var clusterSSL []string
		if frame.EnableVault {
			baseProg.phase("Signing the cluster certificate", 8)
			clusterSSL, err = a.pxcClusterSSL(ctx, intranetID, frame, gcommHosts)
			if err != nil {
				for _, n := range members {
					a.pxcNewProg(st.ID, n.ID).fail("%v", err)
				}
				return
			}
			baseProg.logln("cluster certificate signed by the Intranet CA — every member carries the same one")
		}

		// ---- Phase 1 (parallel): container + install + base config per node ----
		var wg sync.WaitGroup
		failed := make(map[string]bool)
		var mu sync.Mutex
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.pxcPrepareNode(ctx, st, frame, n, hosts, domain, image, clusterAddr, intranetIP, sec, clusterSSL); err != nil {
					mu.Lock()
					failed[n.ID] = true
					mu.Unlock()
				}
			}(n)
		}
		wg.Wait()
		if len(failed) > 0 {
			return // each failed node already recorded its error
		}

		// All containers exist — publish DNS so every FQDN resolves for gcomm.
		a.reconcileStackDNS(ctx, st.ID)

		// ---- Phase 2 (sequential): form the cluster ----
		bootProg := a.pxcNewProg(st.ID, regulars[0].ID)
		bootProg.phase("Bootstrapping cluster", 60)
		if err := a.pxcBootstrap(ctx, st, frame, regulars[0], hosts[regulars[0].ID], domain, intranetID, sec, bootProg); err != nil {
			return
		}
		for _, n := range regulars[1:] {
			pr := a.pxcNewProg(st.ID, n.ID)
			pr.phase("Joining cluster (SST)", 65)
			if err := a.pxcJoin(ctx, st, frame, n, hosts[n.ID], domain, intranetID, sec, pr); err != nil {
				return
			}
		}
		for _, n := range arbiters {
			pr := a.pxcNewProg(st.ID, n.ID)
			pr.phase("Starting arbitrator (garbd)", 70)
			if err := a.pxcStartGarbd(ctx, st, n, frame, clusterAddr, pr); err != nil {
				return
			}
		}

		// Cluster is formed and at its reset baseline — release the barrier so cross-
		// cluster replication can proceed once every stack participant has arrived.
		if barrier != nil {
			for _, n := range members {
				barrier.arrive(n.ID)
			}
		}

		// ---- Phase 3: encryption check, monitoring, finalize ----
		for _, n := range members {
			pr := a.pxcNewProg(st.ID, n.ID)
			// The keyring was staged before each member started; confirm it loaded, and prove it
			// end to end on the node that bootstrapped — an encrypted table there is a master key
			// stored in OpenBao, and the write replicates to the rest of the cluster.
			if frame.EnableVault && n.Role != "arbitrator" {
				pr.phase("Verifying keyring (OpenBao)", 90)
				dep, _ := a.store.GetDeployment(st.ID, n.ID)
				mount, _, _ := mysqlVaultMount(frame.PXCMajor, hosts[n.ID])
				if err := a.verifyMySQLVault(ctx, dep.ContainerID, frame.OS, frame.PXCMajor, mount,
					sec.RootPassword, n.ID == regulars[0].ID, pr); err != nil {
					pr.fail("verify keyring_vault: %v", err)
					return
				}
			}
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 92)
				pmmUser, pmmPass := "", ""
				if _, u, p, ok := a.pmmServerFor(st, doc, frame.PMMNodeID); ok {
					pmmUser, pmmPass = u, p
				}
				a.pxcRegisterPMM(ctx, st, n, frame, monitoredBy, pmmUser, pmmPass, sec, pr) // best-effort
			}
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d cluster %s: provisioned (%d node(s))", st.ID, frame.Label, len(members))
		_ = baseProg
	}()
}

func roleOf(n designNode) string {
	if n.Role == "arbitrator" {
		return "arbitrator"
	}
	return "regular"
}

// pxcPrepareNode creates the node container, points it at the Intranet resolver,
// installs the PXC packages, and writes the base my.cnf (regular nodes only).
func (a *App) pxcPrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, hosts map[string]string, domain, image, clusterAddr, intranetIP string, sec pxcSecrets, clusterSSL []string) error {
	pr := a.pxcNewProg(st.ID, n.ID)
	host := hosts[n.ID]
	arbiter := n.Role == "arbitrator"

	pr.phase("Creating container", 15)
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
	// Regular nodes can publish 3306 to the host.
	if !arbiter && n.ExportEnabled {
		spec.PublishMap = []PortMap{{ContainerPort: 3306, HostPort: n.ExportHostPort}}
	}
	id, err := a.engCtx(ctx).ContainerCreate(ctx, spec)
	if err != nil {
		return pr.fail("create container: %v", err)
	}
	if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
		return pr.fail("start container: %v", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)

	// Record container id + export host port.
	var cfg pxcConfig
	if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil {
		json.Unmarshal(dep.Config, &cfg)
	}
	if !arbiter && n.ExportEnabled {
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, "3306/tcp"); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.ExportPort = p
			}
		}
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

	pr.phase("Waiting for systemd", 25)
	if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
		return pr.fail("systemd did not start: %v", err)
	}
	a.trustIntranetCA(ctx, st, id, frame.OS, pr.logln)
	a.ensureDNFIPv4(ctx, id, frame.OS, pr.logln)

	pr.phase("Installing Percona XtraDB Cluster", 35)
	pkg := "percona-xtradb-cluster"
	if arbiter {
		pkg = "percona-xtradb-cluster-garbd"
	}
	proxy := ""
	if frame.UseProxy {
		proxy = "http://intranet." + domain + ":3128"
	}
	env := []string{"PRODUCT=" + pxcProduct(frame.PXCMajor), "PKG=" + pkg, "PROXY=" + proxy, "VER=" + frame.PXCVersion}
	script := pxcInstallRHEL
	if isDebianOS(frame.OS) {
		script = pxcInstallDebian
	}
	if err := a.runStep(ctx, id, script, env, pr.logln); err != nil {
		return pr.fail("install %s: %v", pkg, err)
	}
	pr.logln(pkg + " installed")
	a.ensureRsyslog(ctx, id, frame.OS, pr.logln)

	// Data nodes need Percona XtraBackup for SST (wsrep_sst_method=xtrabackup-v2);
	// the percona-xtrabackup-80/84 package matches the PXC series. Arbitrators run
	// garbd only (no datadir, no SST), so they skip it.
	if !arbiter {
		pr.phase("Installing Percona XtraBackup", 40)
		xbpkg := pxbPackage(frame.PXCMajor)
		xbEnv := []string{"PRODUCT=" + pxbProduct(frame.PXCMajor), "REPO=" + pxbRepoName(frame.PXCMajor), "PKG=" + xbpkg,
			"VER=" + frame.PXCVersion}
		xbScript := pxcInstallXtrabackupRHEL
		if isDebianOS(frame.OS) {
			xbScript = pxcInstallXtrabackupDebian
		}
		if err := a.runStep(ctx, id, xbScript, xbEnv, pr.logln); err != nil {
			return pr.fail("install %s: %v", xbpkg, err)
		}
		pr.logln(xbpkg + " installed")

		// Install pmm-client only when the cluster is monitored by a PMM server.
		// Enabling monitoring later re-runs provisioning, which installs it then.
		if frame.PMMNodeID != "" {
			pr.phase("Installing PMM client", 45)
			pmmScript := pxcInstallPMMClientRHEL
			if isDebianOS(frame.OS) {
				pmmScript = pxcInstallPMMClientDebian
			}
			if err := a.runStep(ctx, id, pmmScript, nil, pr.logln); err != nil {
				return pr.fail("install pmm-client: %v", err)
			}
			pr.logln("pmm-client installed")
		}
	}

	// garbd nodes are configured later; regular nodes get their my.cnf now.
	if !arbiter {
		// Data-at-rest encryption, staged before this member has ever started mysqld — which for
		// a cluster member is not a convenience but the difference between configuring a keyring
		// and restarting a node out of the cluster to give it one. Every member gets its own KV
		// mount; see dbvault.go. An arbitrator runs garbd, which has no datadir and no keyring.
		vaultOpts := ""
		if frame.EnableVault {
			// The cluster's shared TLS material first: it is what the SST channel is encrypted
			// with, and PXC will not transfer state without it once a keyring is configured.
			if err := a.runStep(ctx, id, pxcClusterSSLScript, clusterSSL, pr.logln); err != nil {
				return pr.fail("stage cluster TLS material: %v", err)
			}
			prep, err := a.prepareMySQLVault(ctx, st, frame.OpenBaoNodeID, frame.OS, frame.PXCMajor, host, id, pr)
			if err != nil {
				return pr.fail("configure keyring_vault: %v", err)
			}
			vaultOpts = prep.Options
			a.persistConfigKey(st, n.ID, "vault", prep.Info)
		}
		if err := a.mysqlWriteNodeCnf(ctx, id, frame.OS, pxcMyCnf(frame, n, host, domain, clusterAddr, vaultOpts), pr); err != nil {
			return err
		}
	}
	return nil
}

// pxcCnfDir splits pxcCnfPath into the (dir, base) CopyFile expects.
func pxcCnfDir(os string) (string, string) {
	if isDebianOS(os) {
		return "/etc/mysql", "dbcanvas.cnf"
	}
	return "/etc", "my.cnf"
}

// mysqlWriteNodeCnf writes a rendered mysqld config to the OS-correct path and, on
// Debian/Ubuntu, makes it authoritative. Shared by every MySQL-family frame (PXC,
// Percona Server replication, MySQL Community, group replication / InnoDB Cluster)
// so they cannot drift apart.
//
// On RHEL the file *is* /etc/my.cnf and there is nothing else to do. Debian splits
// the config across `!includedir` drop-ins, and the percona-xtradb-cluster-server
// package ships a *populated* one — /etc/mysql/mysql.conf.d/mysqld.cnf with
// `server-id=1`, `wsrep_cluster_name=pxc-cluster`, `wsrep_node_name=pxc-cluster-node-1`
// and an empty `wsrep_cluster_address=gcomm://`. Two things then have to happen:
//
//   - the trailing `!include` (pxcDebianIncludeCnf), so our file is read last, and
//   - commenting the vendor's copy of every option we set (pxcDebianDisableVendorCnf).
//
// The second is not redundant. Ordering alone leaves two contradictory values on
// disk: anything that reads the config rather than the running server — an operator
// grepping /etc, a config collector, pt-config-diff — sees `pxc-cluster-node-1`, and
// if the include is ever lost (an alternatives switch, a hand edit) the vendor
// defaults take over silently and every node bootstraps its own one-node cluster
// with server-id 1. Only keys the config we just wrote actually sets are disabled,
// so nothing is left unset.
func (a *App) mysqlWriteNodeCnf(ctx context.Context, id, nodeOS, cnf string, pr *pxcProg) error {
	dir, base := pxcCnfDir(nodeOS)
	if err := a.engCtx(ctx).CopyFile(ctx, id, dir, base, 0o644, []byte(cnf)); err != nil {
		return pr.fail("write %s: %v", pxcCnfPath(nodeOS), err)
	}
	if !isDebianOS(nodeOS) {
		return nil
	}
	if err := a.runStep(ctx, id, pxcDebianIncludeCnf, nil, pr.logln); err != nil {
		return pr.fail("include my.cnf: %v", err)
	}
	keys := mysqlCnfOptionKeys(cnf)
	if len(keys) == 0 {
		return nil
	}
	if err := a.runStep(ctx, id, pxcDebianDisableVendorCnf, []string{"KEYS=" + strings.Join(keys, " ")}, pr.logln); err != nil {
		return pr.fail("disable vendor my.cnf settings: %v", err)
	}
	return nil
}

// mysqlCnfOptionKeys lists the option names an option file sets, deduplicated and
// in first-seen order (the same option can appear in both [client] and [mysqld]).
// Section headers, comments and blank lines are skipped, and only the part before
// the first '=' is taken — a value may itself contain '=' (wsrep_provider_options).
func mysqlCnfOptionKeys(cnf string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, line := range strings.Split(cnf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "!") {
			continue
		}
		i := strings.Index(line, "=")
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		// Anything exotic is left alone rather than interpolated into the sed
		// expression the disable script builds.
		if k == "" || seen[k] || strings.IndexFunc(k, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
		}) >= 0 {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	return keys
}

// pxcRootMyCnf renders /root/.my.cnf so the unix root user can run `mysql` without
// supplying the password. Written after the root password is established (so it
// doesn't interfere with the bootstrap auth_socket path), mode 0600.
func pxcRootMyCnf(sec pxcSecrets) []byte {
	return []byte("[client]\nuser=" + sec.RootUser + "\npassword=" + sec.RootPassword + "\nsocket=/var/lib/mysql/mysql.sock\n")
}

// pxcMyCnf renders /etc/my.cnf for a regular node (no SSL yet — certs are applied
// after the node is up so SST does not race the cert files).
func pxcMyCnf(frame designFrame, n designNode, host, domain, clusterAddr, vaultOptions string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[client]\nsocket=/var/lib/mysql/mysql.sock\n\n[mysqld]\n")
	fmt.Fprintf(&b, "server-id=%d\n", pxcServerID(host))
	fmt.Fprintf(&b, "datadir=/var/lib/mysql\nsocket=/var/lib/mysql/mysql.sock\n")
	fmt.Fprintf(&b, "log-error=%s\npid-file=/var/run/mysqld/mysqld.pid\n", pxcLogError(frame.OS))
	// Listen on all interfaces (Debian's package config defaults to 127.0.0.1,
	// which would block the published host port and cross-node client access).
	fmt.Fprintf(&b, "bind-address=0.0.0.0\n")
	// Slow query log on by default (file in the mysql-owned datadir so mysqld can
	// always create it).
	fmt.Fprintf(&b, "slow_query_log=ON\nslow_query_log_file=/var/lib/mysql/slow.log\nlong_query_time=2\n")
	if frame.GTID {
		fmt.Fprintf(&b, "gtid_mode=ON\nenforce_gtid_consistency=ON\n")
	}
	// Binary logging is on regardless of GTID (with replica-update logging) so a PXC
	// node can act as an async source or replica for a cross-cluster replication link
	// — GTID auto-position when both clusters use GTID, binlog file/position otherwise.
	fmt.Fprintf(&b, "log_bin=binlog\n%s\n", logUpdatesOption(frame.PXCMajor, frame.PXCVersion))
	fmt.Fprintf(&b, "binlog_format=ROW\ninnodb_autoinc_lock_mode=2\npxc_strict_mode=ENFORCING\n")
	// Cluster traffic runs unencrypted on the isolated stack network; client
	// (3306) TLS is provided separately by the per-node certs when enabled.
	//
	// UNLESS the cluster has a keyring, where it is not a choice. PXC's own SST script refuses
	// to run — "FATAL: keyring component is enabled but transit channel is unencrypted. Enable
	// encryption for SST traffic" — so a keyed cluster with this off bootstraps its first member
	// and can never add a second. The material is one certificate for the whole cluster
	// (pxcClusterSSL), staged before any member starts, because it is what the transfer itself
	// is encrypted with.
	if vaultOptions != "" {
		fmt.Fprintf(&b, "pxc_encrypt_cluster_traffic=ON\n")
		fmt.Fprintf(&b, "ssl-ca=%s/ca.pem\nssl-cert=%s/server-cert.pem\nssl-key=%s/server-key.pem\n",
			pxcSSLDir, pxcSSLDir, pxcSSLDir)
	} else {
		fmt.Fprintf(&b, "pxc_encrypt_cluster_traffic=OFF\n")
	}
	fmt.Fprintf(&b, "wsrep_provider=%s\n", galeraProvider(frame.OS))
	fmt.Fprintf(&b, "wsrep_cluster_name=%s\n", frame.Label)
	fmt.Fprintf(&b, "wsrep_cluster_address=gcomm://%s\n", clusterAddr)
	fmt.Fprintf(&b, "wsrep_node_name=%s\n", host)
	fmt.Fprintf(&b, "wsrep_node_address=%s\n", fqdnOf(host, domain))
	// xtrabackup-v2 rather than rsync, and with a keyring that is not a preference: PXC aborts
	// an rsync SST outright on a node configured with component_keyring_vault, because rsync
	// copies the tablespaces without the re-encryption step. XtraBackup re-encrypts the donor's
	// keys with a transition key, and the joiner re-encrypts them under a master key of its own —
	// which is also what makes a mount per member work.
	fmt.Fprintf(&b, "wsrep_sst_method=xtrabackup-v2\n")
	b.WriteString(vaultOptions)
	// The [sst] section is last because it ends [mysqld]: anything written after it would be an
	// SST option rather than a server one. `encrypt=4` is SSL with the server's own certificate
	// on both ends, which is what makes the keyring's tablespace keys — and the transition key
	// the joiner re-encrypts them with — private on the wire.
	if vaultOptions != "" {
		fmt.Fprintf(&b, "\n[sst]\nencrypt=4\nssl-ca=%s/ca.pem\nssl-cert=%s/server-cert.pem\nssl-key=%s/server-key.pem\n",
			pxcSSLDir, pxcSSLDir, pxcSSLDir)
	}
	return b.String()
}

// pxcBootstrap bootstraps the first regular node, sets the root password, creates
// the app/repl users, and (optionally) applies the node certificate.
func (a *App) pxcBootstrap(ctx context.Context, st Stack, frame designFrame, n designNode, host, domain, intranetID string, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	env := []string{
		"ROOT_PW=" + sec.RootPassword,
		"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword,
		"APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword,
		"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
		"MON_USER=" + sec.MonitorUser, "MON_PW=" + sec.MonitorPassword,
		"CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword,
		"CC_USER=" + sec.ClusterCheckUser, "CC_PW=" + sec.ClusterCheckPassword,
		"RESET_CMD=" + mysqlResetCmd(frame.PXCMajor),
		"LOGERR=" + pxcLogError(frame.OS),
	}
	if err := a.runStep(ctx, id, pxcBootstrapScript, env, pr.logln); err != nil {
		return pr.fail("bootstrap: %v", err)
	}
	pr.logln("cluster bootstrapped; root/admin passwords set; app/repl/monitor/cluster users created; GTID reset")
	// Let the unix root user run mysql without typing the password.
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec)); err != nil {
		return pr.fail("write /root/.my.cnf: %v", err)
	}
	if frame.GenerateCert && !frame.EnableVault {
		pr.phase("Issuing certificate", 80)
		if err := a.pxcApplyCert(ctx, id, intranetID, fqdnOf(host, domain), "mysql@bootstrap", frame.OS, frame.CertTTLValue, frame.CertTTLUnit, pr.logln, false); err != nil {
			return pr.fail("%v", err)
		}
	} else if frame.GenerateCert {
		// An encrypted cluster already has a certificate, and it has to be the SAME one on every
		// member (pxcClusterSSL). Issuing a per-node certificate on top would give each member a
		// different one, which is what PXC cannot use for cluster traffic — and this cluster's
		// traffic has to be encrypted, because its SST carries keys.
		pr.logln("per-node certificates skipped: this cluster uses one shared certificate, which is what encrypted cluster traffic requires")
	}
	return nil
}

// pxcJoin starts a joining regular node (SST), waits for sync, then optionally
// applies its certificate.
func (a *App) pxcJoin(ctx context.Context, st Stack, frame designFrame, n designNode, host, domain, intranetID string, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	// ROOT_PW lets the .cache recovery path shut its standalone mysqld down cleanly
	// (the donor's root password arrives with the SST).
	joinEnv := []string{"LOGERR=" + pxcLogError(frame.OS), "ROOT_PW=" + sec.RootPassword}
	if err := a.runStep(ctx, id, pxcJoinScript, joinEnv, pr.logln); err != nil {
		return pr.fail("join: %v", err)
	}
	pr.logln("joined cluster (synced)")
	// Root password is replicated from the donor via SST; drop the same /root/.my.cnf.
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec)); err != nil {
		return pr.fail("write /root/.my.cnf: %v", err)
	}
	if frame.GenerateCert && !frame.EnableVault {
		pr.phase("Issuing certificate", 80)
		if err := a.pxcApplyCert(ctx, id, intranetID, fqdnOf(host, domain), "mysql", frame.OS, frame.CertTTLValue, frame.CertTTLUnit, pr.logln, false); err != nil {
			return pr.fail("%v", err)
		}
	} else if frame.GenerateCert {
		pr.logln("per-node certificates skipped: this cluster uses one shared certificate (see the bootstrap node)")
	}
	return nil
}

// pxcStartGarbd configures and starts the Galera arbitrator daemon.
func (a *App) pxcStartGarbd(ctx context.Context, st Stack, n designFrameNode, frame designFrame, clusterAddr string, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	garbConf := "/etc/sysconfig/garb"
	if isDebianOS(frame.OS) {
		garbConf = "/etc/default/garb"
	}
	env := []string{"GROUP=" + frame.Label, "NODES=" + clusterAddr, "GARBCONF=" + garbConf}
	if err := a.runStep(ctx, id, pxcGarbdScript, env, pr.logln); err != nil {
		return pr.fail("start garbd: %v", err)
	}
	pr.logln("arbitrator joined the cluster")
	return nil
}

// pxcApplyCert stages the Intranet CA into the node container, signs server +
// client certs into /var/lib/mysql (owned by mysql) with the given TTL, points
// my.cnf at them, and restarts the given mysql unit. Returns an error (callers
// own progress reporting), so it is reusable for post-deploy regeneration.
func (a *App) pxcApplyCert(ctx context.Context, containerID, intranetID, fqdn, unit, os string, ttlValue int, ttlUnit string, logln func(string), noRestart bool) error {
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
	// In the systemd PXC images exec runs as root, so 0644 staging is fine.
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
	env := []string{
		"FQDN=" + fqdn,
		"VALUE=" + strconv.Itoa(ttlValue), "UNIT=" + ttlUnit,
		"UNITSVC=" + unit,
		"CNF=" + pxcCnfPath(os), "LOGERR=" + pxcLogError(os),
		"NORESTART=" + boolEnv(noRestart),
	}
	if err := a.runStep(ctx, containerID, pxcCertScript, env, logln); err != nil {
		return fmt.Errorf("generate certificate: %w", err)
	}
	logln("per-node certificate written to /var/lib/mysql (mysql-owned)")
	return nil
}

// pxcRegisterPMM installs pmm-client and registers the node's MySQL with the PMM
// server. Best-effort — failures are logged but do not fail the deployment.
func (a *App) pxcRegisterPMM(ctx context.Context, st Stack, n designNode, frame designFrame, pmmFQDN, pmmUser, pmmPass string, sec pxcSecrets, pr *pxcProg) {
	if pmmFQDN == "" {
		return
	}
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	if err := a.pxcPMMExec(ctx, dep.ContainerID, frame.OS, pxcPMMEnv(pmmFQDN, pmmUser, pmmPass, sec, n.Label)); err != nil {
		pr.logln("PMM registration skipped: " + err.Error())
	} else {
		pr.logln("registered with PMM at " + pmmFQDN)
	}
}

// pxcPMMEnv builds the exec environment for the PMM register script. PMM server
// creds default to admin/admin when unknown (best-effort deploy path). The MySQL
// service is added as root over the local socket (DB_USER/DB_PW): root won't
// authenticate over TCP (root@localhost doesn't match 127.0.0.1, and caching_sha2
// over plain TCP needs the server key), but root@localhost connects fine over the
// socket. (The monitor user is reserved for ProxySQL, not PMM.)
func pxcPMMEnv(pmmFQDN, pmmUser, pmmPass string, sec pxcSecrets, node string) []string {
	if pmmUser == "" {
		pmmUser = "admin"
	}
	if pmmPass == "" {
		pmmPass = "admin"
	}
	dbUser := sec.RootUser
	if dbUser == "" {
		dbUser = "root"
	}
	return []string{
		"PMM_FQDN=" + pmmFQDN, "PMM_USER=" + pmmUser, "PMM_PASS=" + pmmPass, "PMM_URL=" + pmmServerURL(pmmFQDN, pmmUser, pmmPass),
		// DB_USER/DB_PW are root, used to create the dedicated 'pmm' account;
		// PMM_PW is that account's password (PMM connects as 'pmm').
		"DB_USER=" + dbUser, "DB_PW=" + sec.RootPassword,
		"PMM_PW=" + envOr("PMM_PASSWORD", "pmm_password"),
		"NODE=" + node,
	}
}

// pxcPMMExec runs the OS-appropriate PMM register script in a node container.
func (a *App) pxcPMMExec(ctx context.Context, containerID, os string, env []string) error {
	script := pxcPMMRHEL
	if isDebianOS(os) {
		script = pxcPMMDebian
	}
	_, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", script}, env)
	return err
}

// pmmServerFor resolves a frame's PMM node into the FQDN + admin credentials of a
// running PMM server. ok is false when no PMM node is selected or it is not running.
func (a *App) pmmServerFor(st Stack, doc designDoc, pmmNodeID string) (fqdn, user, pass string, ok bool) {
	if pmmNodeID == "" {
		return "", "", "", false
	}
	hosts := stackHostnames(doc)
	domain := envOr("DOMAIN", "example.net")
	for _, n := range doc.Nodes {
		if n.ID != pmmNodeID || n.Type != "pmm" {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
			return "", "", "", false
		}
		var s pmmSecrets
		json.Unmarshal(dep.Secrets, &s)
		u := s.AdminUser
		if u == "" {
			u = "admin"
		}
		return fqdnOf(hosts[n.ID], domain), u, s.AdminPassword, true
	}
	return "", "", "", false
}

// designFrameNode is just designNode (alias for readability in garbd handling).
type designFrameNode = designNode

// ------------------------------------------------------------------ scripts

const pxcInstallRHEL = pinInstallRHEL + `set -e
if [ -n "$PROXY" ]; then grep -q '^proxy=' /etc/dnf/dnf.conf 2>/dev/null || echo "proxy=$PROXY" >> /etc/dnf/dnf.conf; fi
dnf -y -q module disable mysql >/dev/null 2>&1 || true
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
pin_install "$PKG"`

const pxcInstallDebian = pinInstallDebian + `set -e
export DEBIAN_FRONTEND=noninteractive
if [ -n "$PROXY" ]; then echo "Acquire::http::Proxy \"$PROXY\";" > /etc/apt/apt.conf.d/01dbcanvas-proxy; fi
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
apt-get update -qq >/dev/null
pin_install "$PKG"`

// pxcInstallXtrabackup{RHEL,Debian} enable the XtraBackup repo for the cluster's
// PXC series (percona-release setup pxb80 | pxb84lts) and install the matching
// percona-xtrabackup-80 | -84 package used for SST.
// $PRODUCT empty takes the hand-written path, for the series percona-release cannot
// enable — pxb-97-lts has the same problem as ps-97-lts. See psRepoRHEL in mysql.go.
const pxcInstallXtrabackupRHEL = pinInstallRHEL + `set -e
if [ -z "$PRODUCT" ]; then
  cat >/etc/yum.repos.d/dbcanvas-pxb.repo <<EOF
[dbcanvas-pxb]
name=Percona XtraBackup $REPO
baseurl=https://repo.percona.com/$REPO/yum/release/\$releasever/RPMS/\$basearch/
gpgkey=https://repo.percona.com/yum/PERCONA-PACKAGING-KEY
gpgcheck=1
enabled=1
skip_if_unavailable=1
EOF
else
  percona-release setup -y "$PRODUCT" >/dev/null 2>&1
fi
# pin_install rather than a bare dnf: XtraBackup has its own version series, but it shares
# libraries with the cluster packages, and an unpinned install can drag those up a minor.
pin_install "$PKG" >/dev/null`

const pxcInstallXtrabackupDebian = pinInstallDebian + `set -e
export DEBIAN_FRONTEND=noninteractive
if [ -z "$PRODUCT" ]; then
  apt-get install -y -qq curl gnupg ca-certificates >/dev/null 2>&1 || true
  install -d /etc/apt/keyrings
  curl -fsSL https://repo.percona.com/yum/PERCONA-PACKAGING-KEY | gpg --batch --yes --dearmor -o /etc/apt/keyrings/dbcanvas-percona.gpg
  CODE=$(. /etc/os-release; echo "$VERSION_CODENAME")
  echo "deb [signed-by=/etc/apt/keyrings/dbcanvas-percona.gpg] https://repo.percona.com/$REPO/apt $CODE main" \
    >/etc/apt/sources.list.d/dbcanvas-pxb.list
else
  percona-release setup -y "$PRODUCT" >/dev/null 2>&1
fi
apt-get update -qq >/dev/null
pin_install "$PKG" >/dev/null`

// pxcDebianIncludeCnf appends a trailing `!include /etc/mysql/dbcanvas.cnf` to
// Debian's /etc/mysql/my.cnf so our settings are read last and win over the
// package's includedirs (whose empty wsrep_cluster_address would otherwise make
// every node bootstrap its own single-node cluster).
//
// /etc/mysql/my.cnf is not a plain file: mysql-common registers it with
// update-alternatives (my.cnf.fallback at priority 100, the server package's
// /etc/mysql/mysql.cnf at 300), so the path is a symlink into /etc/alternatives.
// Appending through it edits whichever candidate is selected *today* — the include
// is therefore written into every candidate, so removing or downgrading the server
// package (which switches the alternative) cannot silently strip our config.
const pxcDebianIncludeCnf = `set -e
add_include() {
  [ -f "$1" ] || return 0
  grep -q '/etc/mysql/dbcanvas.cnf' "$1" || printf '\n!include /etc/mysql/dbcanvas.cnf\n' >> "$1"
}
MYCNF=/etc/mysql/my.cnf
if [ -e "$MYCNF" ]; then
  add_include "$(readlink -f "$MYCNF")"
else
  # No my.cnf at all (never seen with the Percona/MySQL packages, but a broken
  # alternatives link must not leave the node with an unread config): create it as
  # a plain file rather than writing through a dangling symlink.
  rm -f "$MYCNF"
  printf '!include /etc/mysql/dbcanvas.cnf\n' > "$MYCNF"
fi
for alt in $(update-alternatives --list my.cnf 2>/dev/null || true); do add_include "$alt"; done`

// pxcDebianDisableVendorCnf comments out, in the packages' own !includedir drop-ins,
// every option DBCanvas sets in /etc/mysql/dbcanvas.cnf (passed in as $KEYS).
//
// pxcDebianIncludeCnf already makes dbcanvas.cnf win at runtime, but PXC's
// /etc/mysql/mysql.conf.d/mysqld.cnf ships a full identity block — server-id=1,
// wsrep_cluster_name=pxc-cluster, wsrep_node_name=pxc-cluster-node-1,
// wsrep_cluster_address=gcomm:// — that stays on disk contradicting it. Leaving it
// there misleads anyone reading the config instead of the running server, and turns
// any future loss of the include into a silent split cluster. Percona Server and
// MySQL Community ship a benign drop-in (pid-file/socket/datadir/log-error) and are
// tidied by the same pass.
//
// Option names are matched with '-' and '_' interchangeable (MySQL treats them as
// the same option) and anchored on the '=', so server-id does not match
// server-id-bits. Re-running is a no-op: a commented line no longer matches.
const pxcDebianDisableVendorCnf = `set -e
MARK='# dbcanvas: set in /etc/mysql/dbcanvas.cnf --'
for f in /etc/mysql/conf.d/*.cnf /etc/mysql/mysql.conf.d/*.cnf /etc/mysql/percona-xtradb-cluster.conf.d/*.cnf; do
  [ -f "$f" ] || continue
  for k in $KEYS; do
    re=$(printf '%s' "$k" | sed -e 's/\./\\./g' -e 's/[-_]/[-_]/g')
    sed -i -E "s|^([[:space:]]*)(${re}[[:space:]]*=)|\1${MARK} \2|" "$f"
  done
  n=$(grep -Fc "$MARK" "$f" 2>/dev/null || true)
  # || true: a drop-in with nothing to disable must not trip set -e.
  [ "${n:-0}" -gt 0 ] && echo "$f: $n vendor setting(s) commented out (dbcanvas.cnf is authoritative)" || true
done`

// pxcBootstrapScript bootstraps the cluster on the first node and sets up users.
// (systemctl start blocks until mysqld signals ready, so no extra wait is needed.)
// Root auth differs by distro: RHEL/OL logs a temporary password; Debian/Ubuntu
// leaves root@localhost on auth_socket (no password). We handle both, plus the
// already-set case on redeploy.
const pxcBootstrapScript = `set -e
LOGERR=${LOGERR:-/var/log/mysqld.log}
rm -f "$LOGERR" 2>/dev/null || true
systemctl reset-failed mysql@bootstrap 2>/dev/null || true
systemctl start mysql@bootstrap
if mysql -uroot -p"$ROOT_PW" -e "SELECT 1" >/dev/null 2>&1; then
  : # root password already set (redeploy)
else
  TMP=$(grep -i 'temporary password' "$LOGERR" 2>/dev/null | tail -1 | sed 's/.*localhost: //')
  if [ -n "$TMP" ]; then
    # RHEL/OL: the temp password is EXPIRED — ALTER USER is allowed while expired,
    # but a SELECT is not, so set the password directly (no probe query first).
    # validate_password can't be relaxed while expired, so if it rejects a weak
    # $ROOT_PW set a strong interim password, relax the policy, then apply $ROOT_PW.
    if ! mysql -uroot --connect-expired-password -p"$TMP" -e "ALTER USER 'root'@'localhost' IDENTIFIED BY '$ROOT_PW';" 2>/dev/null; then
      mysql -uroot --connect-expired-password -p"$TMP" -e "ALTER USER 'root'@'localhost' IDENTIFIED BY 'Dbc#Interim7Pw';"
      mysql -uroot -p'Dbc#Interim7Pw' -e "SET GLOBAL validate_password.policy=LOW; SET GLOBAL validate_password.length=6;" 2>/dev/null || true
      mysql -uroot -p'Dbc#Interim7Pw' -e "ALTER USER 'root'@'localhost' IDENTIFIED BY '$ROOT_PW';"
    fi
  else
    # Debian: connect over the local socket as the root OS user (auth_socket). We're
    # a full (non-expired) root here, so relax validate_password before setting a pw.
    mysql -uroot -e "SET GLOBAL validate_password.policy=LOW; SET GLOBAL validate_password.length=6;" 2>/dev/null || true
    mysql -uroot -e "ALTER USER 'root'@'localhost' IDENTIFIED WITH caching_sha2_password BY '$ROOT_PW';"
  fi
fi
# Relax validate_password so the .env app/repl/monitor/cluster passwords are accepted
# (tolerated if the component isn't installed).
mysql -uroot -p"$ROOT_PW" -e "SET GLOBAL validate_password.policy=LOW; SET GLOBAL validate_password.length=6;" 2>/dev/null || true
mysql -uroot -p"$ROOT_PW" <<SQL
` + mysqlAdminUserSQL + `
CREATE USER IF NOT EXISTS '$APP_USER'@'%' IDENTIFIED BY '$APP_PW';
GRANT ALL PRIVILEGES ON *.* TO '$APP_USER'@'%';
CREATE USER IF NOT EXISTS '$REPL_USER'@'%' IDENTIFIED BY '$REPL_PW';
GRANT REPLICATION SLAVE ON *.* TO '$REPL_USER'@'%';
CREATE USER IF NOT EXISTS '$MON_USER'@'%' IDENTIFIED BY '$MON_PW' WITH MAX_USER_CONNECTIONS 10;
ALTER USER '$MON_USER'@'%' IDENTIFIED BY '$MON_PW';
GRANT SELECT, PROCESS, REPLICATION CLIENT, RELOAD, BACKUP_ADMIN ON *.* TO '$MON_USER'@'%';
GRANT SELECT ON performance_schema.* TO '$MON_USER'@'%';
CREATE USER IF NOT EXISTS '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
ALTER USER '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
GRANT ALL PRIVILEGES ON *.* TO '$CLUSTER_USER'@'%' WITH GRANT OPTION;
CREATE USER IF NOT EXISTS '$CC_USER'@'localhost' IDENTIFIED BY '$CC_PW';
ALTER USER '$CC_USER'@'localhost' IDENTIFIED BY '$CC_PW';
GRANT PROCESS ON *.* TO '$CC_USER'@'localhost';
FLUSH PRIVILEGES;
SQL
# Clear GTID/binlog history now that credentials exist — the joiners have not yet
# SST'd, so they inherit this clean, empty baseline from the donor. A shared empty
# GTID baseline across every cluster is what lets cross-cluster replication attach
# cleanly (AUTO_POSITION with nothing to backfill) once all servers are ready.
mysql -uroot -p"$ROOT_PW" -e "$RESET_CMD" 2>/dev/null || true
echo "wsrep_cluster_size: $(mysql -uroot -p"$ROOT_PW" -N -e "SHOW STATUS LIKE 'wsrep_cluster_size'" 2>/dev/null | awk '{print $2}')"`

// pxcJoinScript starts a joining node, which SSTs from the donor. The PXC
// mysql.service is Type=notify, so systemctl start blocks until mysqld is synced
// and ready — no separate (password-protected) status poll is needed.
//
// Emulated-host (Rosetta) workaround: a joiner can be left with a
// /var/lib/mysql/.cache directory that mysqld cannot remove ("Access denied"),
// so the join never completes. If the unit fails and that directory is present,
// clear it as root, bring mysqld up standalone once so it recovers the data dir,
// shut it down cleanly, then let systemd start it and join. Only joiners run this
// script — the bootstrap node uses mysql@bootstrap (pxcBootstrapScript) — so the
// recovery can never touch the node that seeds the cluster.
const pxcJoinScript = `set -e
LOGERR=${LOGERR:-/var/log/mysqld.log}
ROOT_PW=${ROOT_PW:-}
CACHE=/var/lib/mysql/.cache
RECOVERLOG=/tmp/pxc-cache-recover.log

start_mysql() {
  systemctl reset-failed mysql 2>/dev/null || true
  systemctl start mysql 2>/dev/null || true
  systemctl is-active --quiet mysql
}

# "Access denied" means the server is up but the credentials are stale (the root
# password arrives from the donor via SST) — for a liveness probe that is a yes.
mysqld_alive() {
  out=$(mysqladmin ping -uroot -p"$ROOT_PW" 2>&1) || true
  case "$out" in *"is alive"*|*"Access denied"*) return 0 ;; esac
  return 1
}

if start_mysql; then exit 0; fi

if [ -e "$CACHE" ]; then
  echo "join failed and $CACHE is present — clearing it and recovering"
  systemctl stop mysql 2>/dev/null || true
  rm -rf "$CACHE"
  mysqld --user=mysql >>"$RECOVERLOG" 2>&1 &
  MPID=$!
  UP=0
  for _ in $(seq 1 180); do
    if mysqld_alive; then UP=1; break; fi
    kill -0 "$MPID" 2>/dev/null || break
    sleep 2
  done
  if [ "$UP" = 1 ]; then
    echo "mysqld started standalone; shutting it down cleanly"
    mysqladmin shutdown -uroot -p"$ROOT_PW" >/dev/null 2>&1 \
      || mysqladmin shutdown >/dev/null 2>&1 \
      || kill "$MPID" 2>/dev/null || true
  else
    echo "mysqld did not come up standalone after clearing $CACHE:"
    tail -8 "$RECOVERLOG" 2>/dev/null
    kill "$MPID" 2>/dev/null || true
  fi
  wait "$MPID" 2>/dev/null || true
  if start_mysql; then
    echo "joined cluster after clearing $CACHE"
    exit 0
  fi
fi

echo "mysql failed to join:"; grep -iE 'ERROR|Aborting' "$LOGERR" 2>/dev/null | tail -8; exit 1`

// pxcGarbdScript configures and starts the arbitrator daemon. The config file is
// /etc/sysconfig/garb on RHEL and /etc/default/garb on Debian (passed as GARBCONF).
const pxcGarbdScript = `set -e
mkdir -p "$(dirname "$GARBCONF")"
cat > "$GARBCONF" <<EOF
GALERA_NODES="$(echo "$NODES" | sed 's/\([^,]*\)/\1:4567/g')"
GALERA_GROUP="$GROUP"
GALERA_OPTIONS=""
EOF
systemctl reset-failed garb 2>/dev/null || true
systemctl start garb
sleep 2
systemctl is-active --quiet garb || { echo "garbd failed:"; journalctl -u garb --no-pager 2>/dev/null | tail -10; exit 1; }`

// pxcCertScript signs a server + client certificate from the staged Intranet CA
// into /var/lib/mysql (mysql-owned), points my.cnf at them, and restarts mysqld.
const pxcCertScript = `set -e
case "$UNIT" in
  minutes) SECS=$((VALUE*60));;
  hours)   SECS=$((VALUE*3600));;
  *)       SECS=$((VALUE*86400));;
esac
END=$(date -u -d "+$SECS seconds" +%Y%m%d%H%M%SZ)
DIR=/var/lib/mysql
CA=/tmp/dbca-ca.crt; CAKEY=/tmp/dbca-ca.key
[ -f "$CA" ] && [ -f "$CAKEY" ] || { echo "CA material missing"; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "openssl not installed in this image"; exit 1; }
cp -f "$CA" "$DIR/ca.pem"
# Errors are intentionally NOT discarded so a failure surfaces in the deploy log.
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/server-key.pem" -out /tmp/s.csr -subj "/O=DBCanvas/CN=$FQDN" >/dev/null
` + serverCertExtScript + `openssl x509 -req -in /tmp/s.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/server-cert.pem" -extfile /tmp/dbca-san.ext -not_after "$END" >/dev/null
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/client-key.pem" -out /tmp/c.csr -subj "/O=DBCanvas/CN=$FQDN-client" >/dev/null
openssl x509 -req -in /tmp/c.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/client-cert.pem" -not_after "$END" >/dev/null
chown mysql:mysql "$DIR/ca.pem" "$DIR/server-cert.pem" "$DIR/server-key.pem" "$DIR/client-cert.pem" "$DIR/client-key.pem"
chmod 600 "$DIR/server-key.pem" "$DIR/client-key.pem"
chmod 644 "$DIR/ca.pem" "$DIR/server-cert.pem" "$DIR/client-cert.pem"
rm -f /tmp/dbca-ca.crt /tmp/dbca-ca.key /tmp/s.csr /tmp/c.csr /tmp/dbca-san.ext /tmp/dbca-ca.srl
CNF=${CNF:-/etc/my.cnf}
LOGERR=${LOGERR:-/var/log/mysqld.log}
grep -q '^ssl-ca=' "$CNF" 2>/dev/null || cat >> "$CNF" <<EOF
ssl-ca=/var/lib/mysql/ca.pem
ssl-cert=/var/lib/mysql/server-cert.pem
ssl-key=/var/lib/mysql/server-key.pem
EOF
# NORESTART=1 (management re-issue on an already-running node): only overwrite the cert
# files in place and leave mysqld untouched — restarting a PXC member would force it to
# rejoin the cluster (slow/blocking). The operator restarts the service to apply the new cert.
if [ "$NORESTART" = "1" ]; then
  echo "certificate overwritten in place — restart this node's mysqld to apply it"
  exit 0
fi
systemctl restart "$UNITSVC"
systemctl is-active --quiet "$UNITSVC" || { echo "mysql failed to restart with TLS"; tail -8 "$LOGERR" 2>/dev/null; exit 1; }`

// pxcInstallPMMClient{RHEL,Debian} install the PMM client (percona-release setup
// pmm3-client → dnf/apt install pmm-client). Only run when the cluster/node has
// monitoring enabled (a PMM node selected). Fails loudly so a broken install surfaces.
const pxcInstallPMMClientRHEL = `set -e
percona-release setup -y pmm3-client >/dev/null 2>&1
dnf -y -q install pmm-client >/dev/null`

const pxcInstallPMMClientDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release setup -y pmm3-client >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq pmm-client >/dev/null`

// pxcPMM{RHEL,Debian} point an already-installed pmm-client at the PMM server and
// register this node's MySQL. pmm-client is installed separately at deploy
// (pxcInstallPMMClient*); the install is re-run here too so the step is
// self-healing for clusters provisioned before that became unconditional. config
// + add fail loudly (no `|| true`); only the pre-add `remove` is tolerant.
// pmm-admin config talks to the *local* pmm-agent over its API (127.0.0.1:7777),
// so the agent must be enabled + running first. The RHEL package starts it at
// install; the Debian package leaves it disabled — hence `systemctl enable --now
// pmm-agent` before config (and again after, since config may restart it).
const pxcPMMRHEL = `set -e
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; dnf -y -q install pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove mysql "$NODE" >/dev/null 2>&1 || true
# Dedicated least-privilege PMM monitoring account (per the Percona PMM docs),
# created via root. On PXC the DDL replicates cluster-wide; IF NOT EXISTS keeps
# it idempotent when this runs on every node.
mysql --socket=/var/lib/mysql/mysql.sock -u"$DB_USER" -p"$DB_PW" 2>/dev/null <<SQL || true
CREATE USER IF NOT EXISTS 'pmm'@'%' IDENTIFIED BY '$PMM_PW' WITH MAX_USER_CONNECTIONS 10;
ALTER USER 'pmm'@'%' IDENTIFIED BY '$PMM_PW';
GRANT SELECT, PROCESS, REPLICATION CLIENT, RELOAD, BACKUP_ADMIN ON *.* TO 'pmm'@'%';
GRANT SELECT ON performance_schema.* TO 'pmm'@'%';
SQL
QS=perfschema
[ "$(mysql --socket=/var/lib/mysql/mysql.sock -upmm -p"$PMM_PW" -N -e 'SELECT @@global.slow_query_log' 2>/dev/null)" = "1" ] && QS=slowlog
pmm-admin add mysql --username=pmm --password="$PMM_PW" --socket=/var/lib/mysql/mysql.sock --query-source="$QS" "$NODE"`

const pxcPMMDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; apt-get update -qq >/dev/null; apt-get install -y -qq pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove mysql "$NODE" >/dev/null 2>&1 || true
# Dedicated least-privilege PMM monitoring account (per the Percona PMM docs),
# created via root. On PXC the DDL replicates cluster-wide; IF NOT EXISTS keeps
# it idempotent when this runs on every node.
mysql --socket=/var/lib/mysql/mysql.sock -u"$DB_USER" -p"$DB_PW" 2>/dev/null <<SQL || true
CREATE USER IF NOT EXISTS 'pmm'@'%' IDENTIFIED BY '$PMM_PW' WITH MAX_USER_CONNECTIONS 10;
ALTER USER 'pmm'@'%' IDENTIFIED BY '$PMM_PW';
GRANT SELECT, PROCESS, REPLICATION CLIENT, RELOAD, BACKUP_ADMIN ON *.* TO 'pmm'@'%';
GRANT SELECT ON performance_schema.* TO 'pmm'@'%';
SQL
QS=perfschema
[ "$(mysql --socket=/var/lib/mysql/mysql.sock -upmm -p"$PMM_PW" -N -e 'SELECT @@global.slow_query_log' 2>/dev/null)" = "1" ] && QS=slowlog
pmm-admin add mysql --username=pmm --password="$PMM_PW" --socket=/var/lib/mysql/mysql.sock --query-source="$QS" "$NODE"`

// pxcPMMRemoveScript deregisters a node's MySQL service from PMM and unregisters
// the node from the server (best-effort; used when monitoring is turned off).
const pxcPMMRemoveScript = `pmm-admin remove mysql "$NODE" >/dev/null 2>&1 || true
pmm-admin unregister --force >/dev/null 2>&1 || true`

// ---------------------------------------------------------------- cluster TLS, for an encrypted cluster

// pxcSSLDir is where an encrypted cluster's shared TLS material lives.
//
// Deliberately NOT the data directory, which is where a single node's certificates go
// (pxcCertScript). Two reasons, and the second is the one that matters: a joiner's datadir must be
// empty for its first start and is then replaced wholesale by SST, so anything staged there before
// the node joins is either in the way or about to be deleted — and these files have to exist
// before the node starts, because they are what the SST channel itself is encrypted with.
const pxcSSLDir = "/etc/mysql/pxc-ssl"

// pxcClusterSSLScript writes the shared material a cluster's members all carry, byte for byte.
const pxcClusterSSLScript = `set -e
install -d -o mysql -g mysql -m 0750 ` + pxcSSLDir + `
printf '%s' "$CA"    > ` + pxcSSLDir + `/ca.pem
printf '%s' "$SCERT" > ` + pxcSSLDir + `/server-cert.pem
printf '%s' "$SKEY"  > ` + pxcSSLDir + `/server-key.pem
printf '%s' "$CCERT" > ` + pxcSSLDir + `/client-cert.pem
printf '%s' "$CKEY"  > ` + pxcSSLDir + `/client-key.pem
chown mysql:mysql ` + pxcSSLDir + `/*.pem
chmod 0640 ` + pxcSSLDir + `/server-key.pem ` + pxcSSLDir + `/client-key.pem
chmod 0644 ` + pxcSSLDir + `/ca.pem ` + pxcSSLDir + `/server-cert.pem ` + pxcSSLDir + `/client-cert.pem
echo "cluster TLS material staged in ` + pxcSSLDir + `"`

// pxcMintClusterSSLScript signs ONE certificate for a whole cluster, on the Intranet node.
//
// It runs there rather than on a database node so the CA's private key never leaves the host that
// owns it — the per-node path (pxcCertScript) copies it to the node, signs, and deletes it, which
// is fine for one node and pointless to repeat per member when every member gets the same
// certificate anyway.
const pxcMintClusterSSLScript = `set -e
command -v openssl >/dev/null 2>&1 || { echo "openssl is not installed on the Intranet node"; exit 1; }
case "$UNIT" in
  minutes) SECS=$((VALUE*60));;
  hours)   SECS=$((VALUE*3600));;
  *)       SECS=$((VALUE*86400));;
esac
END=$(date -u -d "+$SECS seconds" +%Y%m%d%H%M%SZ)
rm -rf "$OUT"; mkdir -p "$OUT"
cp -f /etc/pki/dbcanvas/ca.crt "$OUT/ca.pem"
cat >"$OUT/san.ext" <<EXT
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth,clientAuth
subjectAltName=$SANS
EXT
openssl req -newkey rsa:2048 -nodes -keyout "$OUT/server-key.pem" -out "$OUT/s.csr" -subj "/O=DBCanvas/CN=$CN" >/dev/null
openssl x509 -req -in "$OUT/s.csr" -CA /etc/pki/dbcanvas/ca.crt -CAkey /etc/pki/dbcanvas/ca.key \
  -CAcreateserial -out "$OUT/server-cert.pem" -extfile "$OUT/san.ext" -not_after "$END" >/dev/null
openssl req -newkey rsa:2048 -nodes -keyout "$OUT/client-key.pem" -out "$OUT/c.csr" -subj "/O=DBCanvas/CN=$CN-client" >/dev/null
openssl x509 -req -in "$OUT/c.csr" -CA /etc/pki/dbcanvas/ca.crt -CAkey /etc/pki/dbcanvas/ca.key \
  -CAcreateserial -out "$OUT/client-cert.pem" -not_after "$END" >/dev/null
rm -f "$OUT/s.csr" "$OUT/c.csr" "$OUT/san.ext"
echo "cluster certificate signed for $CN"`

// pxcClusterSSL mints the certificate every member of an encrypted cluster shares, and returns the
// five files as environment for pxcClusterSSLScript.
//
// One certificate for the whole cluster, not one per node, because PXC says so: "all nodes must
// use identical key and certificate files to ensure a consistent security setup". A per-node
// certificate — even one signed by the same CA — is what the per-node path already produces, and
// it is exactly what a cluster cannot use.
func (a *App) pxcClusterSSL(ctx context.Context, intranetID string, frame designFrame, memberFQDNs []string) ([]string, error) {
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	ttlValue, ttlUnit := frame.CertTTLValue, frame.CertTTLUnit
	if ttlValue <= 0 {
		ttlValue, ttlUnit = 365, "days"
	}
	switch ttlUnit {
	case "minutes", "hours", "days":
	default:
		ttlUnit = "days"
	}
	// Every member's name is in the certificate, so a client that verifies the hostname reaches
	// any of them with it — the cluster's own traffic does not care, but somebody connecting with
	// --ssl-mode=VERIFY_IDENTITY does.
	sans := []string{}
	for _, f := range memberFQDNs {
		sans = append(sans, "DNS:"+f)
		if short, _, ok := strings.Cut(f, "."); ok {
			sans = append(sans, "DNS:"+short)
		}
	}
	cn := sanitizeName(frame.Label)
	out := "/tmp/dbcanvas-pxcssl-" + cn
	env := []string{
		"OUT=" + out, "CN=" + cn, "SANS=" + strings.Join(sans, ","),
		"VALUE=" + strconv.Itoa(ttlValue), "UNIT=" + ttlUnit,
	}
	ictx := withEngine(ctx, a.intranetEngine())
	if _, err := a.execScript(ictx, intranetID, pxcMintClusterSSLScript, env); err != nil {
		return nil, fmt.Errorf("sign cluster certificate: %w", err)
	}
	names := []string{"CA=ca.pem", "SCERT=server-cert.pem", "SKEY=server-key.pem", "CCERT=client-cert.pem", "CKEY=client-key.pem"}
	files := make([]string, 0, len(names))
	for _, n := range names {
		key, file, _ := strings.Cut(n, "=")
		b, err := a.readContainerFile(ictx, intranetID, out+"/"+file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		files = append(files, key+"="+string(b))
	}
	// The private key does not stay on the CA host a moment longer than it takes to read it.
	a.execScript(ictx, intranetID, "rm -rf \"$OUT\"", []string{"OUT=" + out})
	return files, nil
}
