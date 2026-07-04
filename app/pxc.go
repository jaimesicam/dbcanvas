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
	if major == "8.4" {
		return "pxb84lts"
	}
	return "pxb80"
}

func pxbPackage(major string) string {
	if major == "8.4" {
		return "percona-xtrabackup-84"
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

	go func() {
		ctx := context.Background()
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

		// ---- Phase 1 (parallel): container + install + base config per node ----
		var wg sync.WaitGroup
		failed := make(map[string]bool)
		var mu sync.Mutex
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.pxcPrepareNode(ctx, st, frame, n, hosts, domain, image, clusterAddr, intranetIP, sec); err != nil {
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

		// ---- Phase 3: monitoring + finalize ----
		for _, n := range members {
			pr := a.pxcNewProg(st.ID, n.ID)
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
func (a *App) pxcPrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, hosts map[string]string, domain, image, clusterAddr, intranetIP string, sec pxcSecrets) error {
	pr := a.pxcNewProg(st.ID, n.ID)
	host := hosts[n.ID]
	arbiter := n.Role == "arbitrator"

	pr.phase("Creating container", 15)
	name := containerName(st.ID, n.ID)
	if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
		a.docker.ContainerRemove(ctx, cid)
	}
	spec := ContainerSpec{
		Name: name, Image: image, Hostname: host, Privileged: true,
		Network: networkName(st.ID), Aliases: []string{host},
		DNS: []string{intranetIP}, DNSSearch: []string{domain},
	}
	// Regular nodes can publish 3306 to the host.
	if !arbiter && n.ExportEnabled {
		spec.PublishMap = []PortMap{{ContainerPort: 3306, HostPort: n.ExportHostPort}}
	}
	id, err := a.docker.ContainerCreate(ctx, spec)
	if err != nil {
		return pr.fail("create container: %v", err)
	}
	if err := a.docker.ContainerStart(ctx, id); err != nil {
		return pr.fail("start container: %v", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)

	// Record container id + export host port.
	var cfg pxcConfig
	if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil {
		json.Unmarshal(dep.Config, &cfg)
	}
	if !arbiter && n.ExportEnabled {
		if hp, e := a.docker.ContainerPort(ctx, id, "3306/tcp"); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.ExportPort = p
			}
		}
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

	pr.phase("Waiting for systemd", 25)
	if err := a.docker.WaitSystemd(ctx, id, 90*time.Second); err != nil {
		return pr.fail("systemd did not start: %v", err)
	}
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
	env := []string{"PRODUCT=" + pxcProduct(frame.PXCMajor), "PKG=" + pkg, "PROXY=" + proxy}
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
		xbEnv := []string{"PRODUCT=" + pxbProduct(frame.PXCMajor), "PKG=" + xbpkg}
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
		cnf := pxcMyCnf(frame, n, host, domain, clusterAddr)
		dir, base := pxcCnfDir(frame.OS)
		if err := a.docker.CopyFile(ctx, id, dir, base, 0o644, []byte(cnf)); err != nil {
			return pr.fail("write %s: %v", pxcCnfPath(frame.OS), err)
		}
		// On Debian, ensure our file is included last so it wins over the package
		// defaults (otherwise the empty cluster address bootstraps every node alone).
		if isDebianOS(frame.OS) {
			if err := a.runStep(ctx, id, pxcDebianIncludeCnf, nil, pr.logln); err != nil {
				return pr.fail("include my.cnf: %v", err)
			}
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

// pxcRootMyCnf renders /root/.my.cnf so the unix root user can run `mysql` without
// supplying the password. Written after the root password is established (so it
// doesn't interfere with the bootstrap auth_socket path), mode 0600.
func pxcRootMyCnf(sec pxcSecrets) []byte {
	return []byte("[client]\nuser=" + sec.RootUser + "\npassword=" + sec.RootPassword + "\nsocket=/var/lib/mysql/mysql.sock\n")
}

// pxcMyCnf renders /etc/my.cnf for a regular node (no SSL yet — certs are applied
// after the node is up so SST does not race the cert files).
func pxcMyCnf(frame designFrame, n designNode, host, domain, clusterAddr string) string {
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
	fmt.Fprintf(&b, "pxc_encrypt_cluster_traffic=OFF\n")
	fmt.Fprintf(&b, "wsrep_provider=%s\n", galeraProvider(frame.OS))
	fmt.Fprintf(&b, "wsrep_cluster_name=%s\n", frame.Label)
	fmt.Fprintf(&b, "wsrep_cluster_address=gcomm://%s\n", clusterAddr)
	fmt.Fprintf(&b, "wsrep_node_name=%s\n", host)
	fmt.Fprintf(&b, "wsrep_node_address=%s\n", fqdnOf(host, domain))
	fmt.Fprintf(&b, "wsrep_sst_method=xtrabackup-v2\n")
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
		"RESET_CMD=" + mysqlResetCmd(frame.PXCMajor),
		"LOGERR=" + pxcLogError(frame.OS),
	}
	if err := a.runStep(ctx, id, pxcBootstrapScript, env, pr.logln); err != nil {
		return pr.fail("bootstrap: %v", err)
	}
	pr.logln("cluster bootstrapped; root/admin passwords set; app/repl/monitor/cluster users created; GTID reset")
	// Let the unix root user run mysql without typing the password.
	if err := a.docker.CopyFile(ctx, id, "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec)); err != nil {
		return pr.fail("write /root/.my.cnf: %v", err)
	}
	if frame.GenerateCert {
		pr.phase("Issuing certificate", 80)
		if err := a.pxcApplyCert(ctx, id, intranetID, fqdnOf(host, domain), "mysql@bootstrap", frame.OS, frame.CertTTLValue, frame.CertTTLUnit, pr.logln); err != nil {
			return pr.fail("%v", err)
		}
	}
	return nil
}

// pxcJoin starts a joining regular node (SST), waits for sync, then optionally
// applies its certificate.
func (a *App) pxcJoin(ctx context.Context, st Stack, frame designFrame, n designNode, host, domain, intranetID string, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	if err := a.runStep(ctx, id, pxcJoinScript, []string{"LOGERR=" + pxcLogError(frame.OS)}, pr.logln); err != nil {
		return pr.fail("join: %v", err)
	}
	pr.logln("joined cluster (synced)")
	// Root password is replicated from the donor via SST; drop the same /root/.my.cnf.
	if err := a.docker.CopyFile(ctx, id, "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec)); err != nil {
		return pr.fail("write /root/.my.cnf: %v", err)
	}
	if frame.GenerateCert {
		pr.phase("Issuing certificate", 80)
		if err := a.pxcApplyCert(ctx, id, intranetID, fqdnOf(host, domain), "mysql", frame.OS, frame.CertTTLValue, frame.CertTTLUnit, pr.logln); err != nil {
			return pr.fail("%v", err)
		}
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
func (a *App) pxcApplyCert(ctx context.Context, containerID, intranetID, fqdn, unit, os string, ttlValue int, ttlUnit string, logln func(string)) error {
	if logln == nil {
		logln = func(string) {}
	}
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	caCrt, err := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}
	caKey, err := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if err != nil {
		return fmt.Errorf("read CA key: %w", err)
	}
	// In the systemd PXC images exec runs as root, so 0644 staging is fine.
	if err := a.docker.PutArchive(ctx, containerID, "/tmp", tarFiles(map[string]fileEntry{
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
	_, err := a.docker.Exec(ctx, containerID, []string{"bash", "-c", script}, env)
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

const pxcInstallRHEL = `set -e
if [ -n "$PROXY" ]; then grep -q '^proxy=' /etc/dnf/dnf.conf 2>/dev/null || echo "proxy=$PROXY" >> /etc/dnf/dnf.conf; fi
dnf -y -q module disable mysql >/dev/null 2>&1 || true
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
dnf -y -q install "$PKG" >/dev/null`

const pxcInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
if [ -n "$PROXY" ]; then echo "Acquire::http::Proxy \"$PROXY\";" > /etc/apt/apt.conf.d/01dbcanvas-proxy; fi
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq "$PKG" >/dev/null`

// pxcInstallXtrabackup{RHEL,Debian} enable the XtraBackup repo for the cluster's
// PXC series (percona-release setup pxb80 | pxb84lts) and install the matching
// percona-xtrabackup-80 | -84 package used for SST.
const pxcInstallXtrabackupRHEL = `set -e
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
dnf -y -q install "$PKG" >/dev/null`

const pxcInstallXtrabackupDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq "$PKG" >/dev/null`

// pxcDebianIncludeCnf appends a trailing `!include /etc/mysql/dbcanvas.cnf` to
// Debian's /etc/mysql/my.cnf so our settings are read last and win over the
// package's includedirs (whose empty wsrep_cluster_address would otherwise make
// every node bootstrap its own single-node cluster).
const pxcDebianIncludeCnf = `set -e
MYCNF=/etc/mysql/my.cnf
[ -e "$MYCNF" ] || : > "$MYCNF"
grep -q '/etc/mysql/dbcanvas.cnf' "$MYCNF" || printf '\n!include /etc/mysql/dbcanvas.cnf\n' >> "$MYCNF"`

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
const pxcJoinScript = `set -e
LOGERR=${LOGERR:-/var/log/mysqld.log}
systemctl reset-failed mysql 2>/dev/null || true
systemctl start mysql
systemctl is-active --quiet mysql || { echo "mysql failed to join:"; grep -iE 'ERROR|Aborting' "$LOGERR" 2>/dev/null | tail -8; exit 1; }`

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
openssl x509 -req -in /tmp/s.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/server-cert.pem" -not_after "$END" >/dev/null
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/client-key.pem" -out /tmp/c.csr -subj "/O=DBCanvas/CN=$FQDN-client" >/dev/null
openssl x509 -req -in /tmp/c.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/client-cert.pem" -not_after "$END" >/dev/null
chown mysql:mysql "$DIR/ca.pem" "$DIR/server-cert.pem" "$DIR/server-key.pem" "$DIR/client-cert.pem" "$DIR/client-key.pem"
chmod 600 "$DIR/server-key.pem" "$DIR/client-key.pem"
chmod 644 "$DIR/ca.pem" "$DIR/server-cert.pem" "$DIR/client-cert.pem"
rm -f /tmp/dbca-ca.crt /tmp/dbca-ca.key /tmp/s.csr /tmp/c.csr /tmp/dbca-ca.srl
CNF=${CNF:-/etc/my.cnf}
LOGERR=${LOGERR:-/var/log/mysqld.log}
grep -q '^ssl-ca=' "$CNF" 2>/dev/null || cat >> "$CNF" <<EOF
ssl-ca=/var/lib/mysql/ca.pem
ssl-cert=/var/lib/mysql/server-cert.pem
ssl-key=/var/lib/mysql/server-key.pem
EOF
systemctl restart "$UNITSVC"
systemctl is-active --quiet "$UNITSVC" || { echo "mysql failed to restart with TLS"; tail -8 "$LOGERR" 2>/dev/null; exit 1; }`

// pxcInstallPMMClient{RHEL,Debian} install the PMM client (percona-release setup
// pmm3-client → dnf/apt install pmm-client). Run on every data node at deploy,
// independent of whether monitoring is enabled, so it can be turned on later
// without an install. Fails loudly so a broken install surfaces.
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
