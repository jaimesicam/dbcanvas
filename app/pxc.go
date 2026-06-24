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
	RootUser     string `json:"rootUser"`
	RootPassword string `json:"rootPassword"`
	AppUser      string `json:"appUser"`
	AppPassword  string `json:"appPassword"`
	ReplUser     string `json:"replUser"`
	ReplPassword string `json:"replPassword"`
}

func pxcImage(os, osVersion, arch string) string {
	return "dbcanvas-systemd:" + os + "-" + osVersion + "-" + archOr(arch)
}

// pxcProduct maps a PXC major series to its percona-release product name.
func pxcProduct(major string) string {
	if major == "8.4" {
		return "pxc84lts"
	}
	return "pxc80"
}

func isDebianOS(os string) bool { return os == "ubuntu" || os == "debian" }

func galeraProvider(os string) string {
	if isDebianOS(os) {
		return "/usr/lib/galera4/libgalera_smm.so"
	}
	return "/usr/lib64/galera4/libgalera_smm.so"
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

// pxcServerID derives a stable, unique server-id from a pxcNN node name.
func pxcServerID(name string) int {
	digits := strings.TrimLeft(strings.TrimPrefix(name, "pxc"), "0")
	if v, err := strconv.Atoi(digits); err == nil && v > 0 {
		return v
	}
	return int(fnv32(name)%100000) + 1
}

func fnv32(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
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

	// Cluster-wide root password: reuse across redeploys, else the frame value,
	// else a generated one. App/repl come from the environment.
	root := strings.TrimSpace(frame.RootPassword)
	for _, n := range members {
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
			var s pxcSecrets
			if json.Unmarshal(dep.Secrets, &s) == nil && s.RootPassword != "" {
				root = s.RootPassword
				break
			}
		}
	}
	if root == "" {
		root = genSecret("PxcRoot!")
	}
	sec := pxcSecrets{
		RootUser: "root", RootPassword: root,
		AppUser: "app", AppPassword: envOr("APP_PASSWORD", "app_password"),
		ReplUser: "repl", ReplPassword: envOr("REPL_PASSWORD", "repl_password"),
	}
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
			Cluster: frame.Label, Image: image, Role: roleOf(n), Hostname: host, FQDN: fqdnOf(host, domain),
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
		for _, n := range members {
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			a.pxcNewProg(st.ID, n.ID).phase("Waiting for Intranet to be ready", 5)
		}
		intranetID, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, 10*time.Minute)
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

		// ---- Phase 3: monitoring + finalize ----
		for _, n := range members {
			pr := a.pxcNewProg(st.ID, n.ID)
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 92)
				a.pxcRegisterPMM(ctx, st, n, frame, monitoredBy, sec, pr) // best-effort
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

	// garbd nodes are configured later; regular nodes get their my.cnf now.
	if !arbiter {
		cnf := pxcMyCnf(frame, n, host, domain, clusterAddr)
		if err := a.docker.CopyFile(ctx, id, "/etc", "my.cnf", 0o644, []byte(cnf)); err != nil {
			return pr.fail("write my.cnf: %v", err)
		}
	}
	return nil
}

// pxcMyCnf renders /etc/my.cnf for a regular node (no SSL yet — certs are applied
// after the node is up so SST does not race the cert files).
func pxcMyCnf(frame designFrame, n designNode, host, domain, clusterAddr string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[client]\nsocket=/var/lib/mysql/mysql.sock\n\n[mysqld]\n")
	fmt.Fprintf(&b, "server-id=%d\n", pxcServerID(host))
	fmt.Fprintf(&b, "datadir=/var/lib/mysql\nsocket=/var/lib/mysql/mysql.sock\n")
	fmt.Fprintf(&b, "log-error=/var/log/mysqld.log\npid-file=/var/run/mysqld/mysqld.pid\n")
	if frame.GTID {
		fmt.Fprintf(&b, "gtid_mode=ON\nenforce_gtid_consistency=ON\nlog_bin=binlog\n%s\n", logUpdatesOption(frame.PXCMajor, frame.PXCVersion))
	}
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
	env := []string{"ROOT_PW=" + sec.RootPassword, "APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword, "REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword}
	if err := a.runStep(ctx, id, pxcBootstrapScript, env, pr.logln); err != nil {
		return pr.fail("bootstrap: %v", err)
	}
	pr.logln("cluster bootstrapped; root password set; app/repl users created")
	if frame.GenerateCert {
		pr.phase("Issuing certificate", 80)
		if err := a.pxcApplyCert(ctx, st, frame, n, host, domain, intranetID, "mysql@bootstrap", pr); err != nil {
			return err
		}
	}
	return nil
}

// pxcJoin starts a joining regular node (SST), waits for sync, then optionally
// applies its certificate.
func (a *App) pxcJoin(ctx context.Context, st Stack, frame designFrame, n designNode, host, domain, intranetID string, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	if err := a.runStep(ctx, id, pxcJoinScript, nil, pr.logln); err != nil {
		return pr.fail("join: %v", err)
	}
	pr.logln("joined cluster (synced)")
	if frame.GenerateCert {
		pr.phase("Issuing certificate", 80)
		if err := a.pxcApplyCert(ctx, st, frame, n, host, domain, intranetID, "mysql", pr); err != nil {
			return err
		}
	}
	return nil
}

// pxcStartGarbd configures and starts the Galera arbitrator daemon.
func (a *App) pxcStartGarbd(ctx context.Context, st Stack, n designFrameNode, frame designFrame, clusterAddr string, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	env := []string{"GROUP=" + frame.Label, "NODES=" + clusterAddr}
	if err := a.runStep(ctx, id, pxcGarbdScript, env, pr.logln); err != nil {
		return pr.fail("start garbd: %v", err)
	}
	pr.logln("arbitrator joined the cluster")
	return nil
}

// pxcApplyCert stages the Intranet CA into the node, signs server + client certs
// into /var/lib/mysql (owned by mysql) with the frame's TTL, points my.cnf at
// them, and restarts the given mysql unit.
func (a *App) pxcApplyCert(ctx context.Context, st Stack, frame designFrame, n designNode, host, domain, intranetID, unit string, pr *pxcProg) error {
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		return pr.fail("certificate: %v", err)
	}
	caCrt, err := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil {
		return pr.fail("read CA cert: %v", err)
	}
	caKey, err := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if err != nil {
		return pr.fail("read CA key: %v", err)
	}
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	// In the systemd PXC images exec runs as root, so 0644 staging is fine.
	if err := a.docker.PutArchive(ctx, id, "/tmp", tarFiles(map[string]fileEntry{
		"dbca-ca.crt": {0o644, 0, caCrt},
		"dbca-ca.key": {0o644, 0, caKey},
	})); err != nil {
		return pr.fail("stage CA: %v", err)
	}
	val, unitVal := frame.CertTTLValue, frame.CertTTLUnit
	if val <= 0 {
		val, unitVal = 365, "days"
	}
	switch unitVal {
	case "minutes", "hours", "days":
	default:
		unitVal = "days"
	}
	env := []string{
		"FQDN=" + fqdnOf(host, domain),
		"VALUE=" + strconv.Itoa(val), "UNIT=" + unitVal,
		"UNITSVC=" + unit,
	}
	if err := a.runStep(ctx, id, pxcCertScript, env, pr.logln); err != nil {
		return pr.fail("generate certificate: %v", err)
	}
	pr.logln("per-node certificate written to /var/lib/mysql (mysql-owned)")
	return nil
}

// pxcRegisterPMM installs pmm-client and registers the node's MySQL with the PMM
// server. Best-effort — failures are logged but do not fail the deployment.
func (a *App) pxcRegisterPMM(ctx context.Context, st Stack, n designNode, frame designFrame, pmmFQDN string, sec pxcSecrets, pr *pxcProg) {
	if pmmFQDN == "" {
		return
	}
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	env := []string{"PMM_FQDN=" + pmmFQDN, "ROOT_PW=" + sec.RootPassword, "NODE=" + n.Label}
	script := pxcPMMRHEL
	if isDebianOS(frame.OS) {
		script = pxcPMMDebian
	}
	if _, err := a.docker.Exec(ctx, id, []string{"bash", "-c", script}, env); err != nil {
		pr.logln("PMM registration skipped: " + err.Error())
	} else {
		pr.logln("registered with PMM at " + pmmFQDN)
	}
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

// pxcBootstrapScript bootstraps the cluster on the first node and sets up users.
// (systemctl start blocks until mysqld signals ready, so no extra wait is needed.)
const pxcBootstrapScript = `set -e
rm -f /var/log/mysqld.log 2>/dev/null || true
systemctl reset-failed mysql@bootstrap 2>/dev/null || true
systemctl start mysql@bootstrap
TMP=$(grep -i 'temporary password' /var/log/mysqld.log 2>/dev/null | tail -1 | sed 's/.*localhost: //')
if [ -n "$TMP" ]; then
  mysql -uroot --connect-expired-password -p"$TMP" -e "ALTER USER 'root'@'localhost' IDENTIFIED BY '$ROOT_PW';"
fi
mysql -uroot -p"$ROOT_PW" <<SQL
CREATE USER IF NOT EXISTS '$APP_USER'@'%' IDENTIFIED BY '$APP_PW';
GRANT ALL PRIVILEGES ON *.* TO '$APP_USER'@'%';
CREATE USER IF NOT EXISTS '$REPL_USER'@'%' IDENTIFIED BY '$REPL_PW';
GRANT REPLICATION SLAVE ON *.* TO '$REPL_USER'@'%';
FLUSH PRIVILEGES;
SQL
echo "wsrep_cluster_size: $(mysql -uroot -p"$ROOT_PW" -N -e "SHOW STATUS LIKE 'wsrep_cluster_size'" 2>/dev/null | awk '{print $2}')"`

// pxcJoinScript starts a joining node, which SSTs from the donor. The PXC
// mysql.service is Type=notify, so systemctl start blocks until mysqld is synced
// and ready — no separate (password-protected) status poll is needed.
const pxcJoinScript = `set -e
systemctl reset-failed mysql 2>/dev/null || true
systemctl start mysql
systemctl is-active --quiet mysql || { echo "mysql failed to join:"; grep -iE 'ERROR|Aborting' /var/log/mysqld.log | tail -8; exit 1; }`

// pxcGarbdScript configures and starts the arbitrator daemon.
const pxcGarbdScript = `set -e
cat > /etc/sysconfig/garb <<EOF
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
cp -f "$CA" "$DIR/ca.pem"
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/server-key.pem" -out /tmp/s.csr -subj "/O=DBCanvas/CN=$FQDN" >/dev/null 2>&1
openssl x509 -req -in /tmp/s.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/server-cert.pem" -not_after "$END" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/client-key.pem" -out /tmp/c.csr -subj "/O=DBCanvas/CN=$FQDN-client" >/dev/null 2>&1
openssl x509 -req -in /tmp/c.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/client-cert.pem" -not_after "$END" >/dev/null 2>&1
chown mysql:mysql "$DIR/ca.pem" "$DIR/server-cert.pem" "$DIR/server-key.pem" "$DIR/client-cert.pem" "$DIR/client-key.pem"
chmod 600 "$DIR/server-key.pem" "$DIR/client-key.pem"
chmod 644 "$DIR/ca.pem" "$DIR/server-cert.pem" "$DIR/client-cert.pem"
rm -f /tmp/dbca-ca.crt /tmp/dbca-ca.key /tmp/s.csr /tmp/c.csr /tmp/dbca-ca.srl
grep -q '^ssl-ca=' /etc/my.cnf || cat >> /etc/my.cnf <<EOF
ssl-ca=/var/lib/mysql/ca.pem
ssl-cert=/var/lib/mysql/server-cert.pem
ssl-key=/var/lib/mysql/server-key.pem
EOF
systemctl restart "$UNITSVC"
systemctl is-active --quiet "$UNITSVC" || { echo "mysql failed to restart with TLS"; tail -8 /var/log/mysqld.log; exit 1; }`

const pxcPMMRHEL = `set -e
percona-release enable -y pmm3-client >/dev/null 2>&1 || percona-release enable -y original >/dev/null 2>&1 || true
dnf -y -q install pmm-client >/dev/null 2>&1 || true
pmm-admin config --server-insecure-tls --server-url="https://admin:admin@$PMM_FQDN:8443" >/dev/null 2>&1 || true
pmm-admin add mysql --username=root --password="$ROOT_PW" --host=127.0.0.1 --port=3306 "$NODE" >/dev/null 2>&1 || true`

const pxcPMMDebian = `set -e
percona-release enable -y pmm3-client >/dev/null 2>&1 || true
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq pmm-client >/dev/null 2>&1 || true
pmm-admin config --server-insecure-tls --server-url="https://admin:admin@$PMM_FQDN:8443" >/dev/null 2>&1 || true
pmm-admin add mysql --username=root --password="$ROOT_PW" --host=127.0.0.1 --port=3306 "$NODE" >/dev/null 2>&1 || true`
