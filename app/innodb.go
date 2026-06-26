package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// InnoDB / Group Replication frame. A set of Percona Server nodes (installed from a
// PDPS repository) forming a single-primary MySQL Group Replication group, either
// raw ("groupreplication") or MySQL-Shell-managed ("innodbcluster"). MySQL Router
// is (by default) installed on every member, so the cluster is self-contained and
// exposes no canvas association endpoints — apps connect to a member's router port
// (6446 read/write, 6447 read-only).

// GR uses 3306 (SQL), 33061 (group comms), 6446/6447 (router RW/RO).
const (
	grCommPort    = 33061
	routerRWPort  = 6446
	routerROPort  = 6447
	mysqlBackPort = 3306
)

// innodbConfig is the non-secret profile shown for a deployed InnoDB/GR node.
type innodbConfig struct {
	Cluster      string `json:"cluster"`
	Image        string `json:"image"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	PDPSRepo     string `json:"pdpsRepo"`
	ReplMode     string `json:"replMode"` // innodbcluster | groupreplication
	Hostname     string `json:"hostname"`
	FQDN         string `json:"fqdn"`
	ServerID     int    `json:"serverId"`
	GroupName    string `json:"groupName"`
	Bootstrap    bool   `json:"bootstrap"`
	Router       bool   `json:"router"`
	RWPort       int    `json:"rwPort"` // host port mapped to router 6446 (0 = not published)
	ROPort       int    `json:"roPort"` // host port mapped to router 6447 (0 = not published)
	GenerateCert bool   `json:"generateCert"`
	UseProxy     bool   `json:"useProxy"`
	MonitoredBy  string `json:"monitoredBy"`
	Ports        []int  `json:"ports"`
}

func innodbReplMode(m string) string {
	if m == "groupreplication" {
		return "groupreplication"
	}
	return "innodbcluster"
}

// innodbServerID derives a stable server-id from an innodbNN node name.
func innodbServerID(name string) int {
	digits := strings.TrimLeft(strings.TrimPrefix(name, "innodb"), "0")
	if v, err := strconv.Atoi(digits); err == nil && v > 0 {
		return v
	}
	return int(fnv32(name)%100000) + 1
}

// genUUID returns a random RFC-4122 v4 UUID (used for the GR group name).
func genUUID() string {
	b := make([]byte, 16)
	crand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// provisionInnoDBFrame brings up an InnoDB/Group-Replication cluster: install every
// member, form the group (manual GR or MySQL-Shell InnoDB Cluster), bootstrap MySQL
// Router on each member, and optionally apply TLS/PMM.
func (a *App) provisionInnoDBFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	mode := innodbReplMode(frame.ReplMode)

	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "innodb" {
			members = append(members, n)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	if len(members) == 0 {
		return
	}

	// Cluster-wide root password + a stable group name: reuse across redeploys.
	root := strings.TrimSpace(frame.RootPassword)
	groupName := ""
	for _, n := range members {
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
			var s pxcSecrets
			if json.Unmarshal(dep.Secrets, &s) == nil && s.RootPassword != "" {
				root = s.RootPassword
			}
			var c innodbConfig
			if json.Unmarshal(dep.Config, &c) == nil && c.GroupName != "" {
				groupName = c.GroupName
			}
		}
	}
	if root == "" {
		root = genSecret("MyRoot!")
	}
	if groupName == "" {
		groupName = genUUID()
	}
	sec := pxcSecrets{
		RootUser: "root", RootPassword: root,
		AppUser: "app", AppPassword: envOr("APP_PASSWORD", "app_password"),
		ReplUser: "repl", ReplPassword: envOr("REPL_PASSWORD", "repl_password"),
		MonitorUser: "monitor", MonitorPassword: envOr("MONITOR_PASSWORD", "monitor_password"),
		ClusterUser: "cluster", ClusterPassword: envOr("CLUSTER_PASSWORD", "cluster_password"),
	}
	secJSON, _ := json.Marshal(sec)

	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	var seeds []string
	for _, n := range members {
		seeds = append(seeds, fqdnOf(hosts[n.ID], domain)+":"+strconv.Itoa(grCommPort))
	}
	seedList := strings.Join(seeds, ",")
	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, n := range doc.Nodes {
			if n.ID == frame.PMMNodeID && n.Type == "pmm" {
				monitoredBy = fqdnOf(hosts[n.ID], domain)
			}
		}
	}

	for i, n := range members {
		host := hosts[n.ID]
		cfg := innodbConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			PDPSRepo: frame.PDPSRepo, ReplMode: mode, Hostname: host, FQDN: fqdnOf(host, domain),
			ServerID: innodbServerID(host), GroupName: groupName, Bootstrap: i == 0,
			Router: frame.MySQLRouter, GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy,
			MonitoredBy: monitoredBy, Ports: []int{mysqlBackPort, grCommPort, routerRWPort, routerROPort},
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	go func() {
		ctx := context.Background()
		progs := map[string]*pxcProg{}
		for _, n := range members {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		failAll := func(format string, args ...any) {
			for _, n := range members {
				progs[n.ID].fail(format, args...)
			}
		}
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, 10*time.Minute)
		if werr != nil {
			failAll("%v", werr)
			return
		}

		// ---- Phase 1 (parallel): container + install + my.cnf + root pw + recovery user ----
		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := false
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.innodbPrepareNode(ctx, st, frame, n, hosts[n.ID], image, groupName, seedList, intranetIP, domain, sec, progs[n.ID]); err != nil {
					mu.Lock()
					failed = true
					mu.Unlock()
				}
			}(n)
		}
		wg.Wait()
		if failed {
			return
		}
		a.reconcileStackDNS(ctx, st.ID)

		// ---- Phase 2: form the group ----
		if mode == "innodbcluster" {
			if err := a.innodbCreateCluster(ctx, st, frame, members, hosts, domain, sec, progs); err != nil {
				return
			}
		} else {
			if err := a.innodbFormGroup(ctx, st, members, sec, progs); err != nil {
				return
			}
		}

		// ---- Phase 3: MySQL Router on each member ----
		if frame.MySQLRouter {
			for _, n := range members {
				pr := progs[n.ID]
				pr.phase("Bootstrapping MySQL Router", 85)
				if err := a.innodbSetupRouter(ctx, st, frame, n, hosts, domain, mode, sec, pr); err != nil {
					return
				}
			}
		}

		// ---- Phase 4: TLS + PMM + finalize ----
		for _, n := range members {
			pr := progs[n.ID]
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			// Republish router host ports into the config.
			cfg := innodbConfig{}
			json.Unmarshal(dep.Config, &cfg)
			if frame.MySQLRouter {
				cfg.RWPort, cfg.ROPort = a.readInnoDBRouterPorts(ctx, dep.ContainerID, n.ExportEnabled)
				cfgJSON, _ := json.Marshal(cfg)
				a.store.UpsertDeployment(Deployment{StackID: dep.StackID, NodeID: dep.NodeID, ContainerID: dep.ContainerID, State: dep.State, Config: cfgJSON, Secrets: dep.Secrets})
			}
			if frame.GenerateCert {
				pr.phase("Issuing certificate", 92)
				if err := a.pxcApplyCert(ctx, dep.ContainerID, intranetID, fqdnOf(hosts[n.ID], domain), mysqlUnit(frame.OS), frame.OS, frame.CertTTLValue, frame.CertTTLUnit, pr.logln); err != nil {
					pr.fail("%v", err)
					return
				}
			}
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 96)
				pmmUser, pmmPass := "", ""
				if _, u, p, ok := a.pmmServerFor(st, doc, frame.PMMNodeID); ok {
					pmmUser, pmmPass = u, p
				}
				a.pxcPMMExec(ctx, dep.ContainerID, frame.OS, pxcPMMEnv(monitoredBy, pmmUser, pmmPass, sec, n.Label)) // best-effort
			}
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d innodb %s: provisioned (%d node(s), %s)", st.ID, frame.Label, len(members), mode)
	}()
}

// readInnoDBRouterPorts reads the published host ports for the router (0 when not
// published).
func (a *App) readInnoDBRouterPorts(ctx context.Context, id string, exported bool) (rw, ro int) {
	if !exported {
		return 0, 0
	}
	if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", routerRWPort)); e == nil {
		if p, e2 := strconv.Atoi(hp); e2 == nil {
			rw = p
		}
	}
	if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", routerROPort)); e == nil {
		if p, e2 := strconv.Atoi(hp); e2 == nil {
			ro = p
		}
	}
	return rw, ro
}

// innodbPrepareNode creates a node container, installs percona-server-server (from
// the PDPS repo) + MySQL Router (+ MySQL Shell for InnoDB Cluster) + pmm-client,
// writes my.cnf, starts mysqld (GR not yet started), sets the root password, relaxes
// validate_password, creates the GR recovery user, clears GTID state, and drops
// /root/.my.cnf.
func (a *App) innodbPrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, host, image, groupName, seedList, intranetIP, domain string, sec pxcSecrets, pr *pxcProg) error {
	if host == "" {
		host = sanitizeName(n.Label)
	}
	mode := innodbReplMode(frame.ReplMode)
	pr.phase("Creating container", 12)
	name := containerName(st.ID, n.ID)
	if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
		a.docker.ContainerRemove(ctx, cid)
	}
	spec := ContainerSpec{
		Name: name, Image: image, Hostname: host, Privileged: true,
		Network: networkName(st.ID), Aliases: []string{host},
		DNS: []string{intranetIP}, DNSSearch: []string{domain},
	}
	if n.ExportEnabled && frame.MySQLRouter {
		spec.PublishMap = []PortMap{
			{ContainerPort: routerRWPort, HostPort: n.ExportHostPort},
			{ContainerPort: routerROPort, HostPort: 0},
		}
	}
	id, err := a.docker.ContainerCreate(ctx, spec)
	if err != nil {
		return pr.fail("create container: %v", err)
	}
	if err := a.docker.ContainerStart(ctx, id); err != nil {
		return pr.fail("start container: %v", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: a.depConfig(st.ID, n.ID), Secrets: a.depSecrets(st.ID, n.ID)})

	pr.phase("Waiting for systemd", 22)
	if err := a.docker.WaitSystemd(ctx, id, 90*time.Second); err != nil {
		return pr.fail("systemd did not start: %v", err)
	}
	a.ensureDNFIPv4(ctx, id, frame.OS, pr.logln)

	debian := isDebianOS(frame.OS)
	if frame.UseProxy {
		proxyScript := pkgProxyRHEL
		if debian {
			proxyScript = pkgProxyDebian
		}
		if err := a.runStep(ctx, id, proxyScript, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
			return pr.fail("configure package proxy: %v", err)
		}
	}

	pr.phase("Installing Percona Server + Router", 40)
	pkgs := "percona-server-server"
	if frame.MySQLRouter {
		pkgs += " percona-mysql-router"
	}
	if mode == "innodbcluster" {
		pkgs += " percona-mysql-shell"
	}
	instScript := innodbInstallRHEL
	pmmScript := pxcInstallPMMClientRHEL
	if debian {
		instScript = innodbInstallDebian
		pmmScript = pxcInstallPMMClientDebian
	}
	if err := a.runStep(ctx, id, instScript, []string{"PDPS_REPO=" + frame.PDPSRepo, "PKGS=" + pkgs}, pr.logln); err != nil {
		return pr.fail("install packages: %v", err)
	}
	pr.logln("installed: " + pkgs)
	if err := a.runStep(ctx, id, pmmScript, nil, pr.logln); err != nil {
		return pr.fail("install pmm-client: %v", err)
	}
	a.ensureRsyslog(ctx, id, frame.OS, pr.logln)

	// my.cnf: full GR block for raw group replication; base only for InnoDB Cluster
	// (MySQL Shell configures GR itself).
	cnf := innodbMyCnf(frame, host, domain, groupName, seedList, mode)
	dir, base := pxcCnfDir(frame.OS)
	if err := a.docker.CopyFile(ctx, id, dir, base, 0o644, []byte(cnf)); err != nil {
		return pr.fail("write %s: %v", pxcCnfPath(frame.OS), err)
	}
	if debian {
		if err := a.runStep(ctx, id, pxcDebianIncludeCnf, nil, pr.logln); err != nil {
			return pr.fail("include my.cnf: %v", err)
		}
	}

	pr.phase("Starting mysqld + base setup", 55)
	env := []string{
		"UNIT=" + mysqlUnit(frame.OS), "LOGERR=" + pxcLogError(frame.OS),
		"RESET_CMD=" + mysqlResetCmd(psMajorOfRepo(frame.PDPSRepo)),
		"ROOT_PW=" + sec.RootPassword,
		"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
		"CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword,
	}
	if err := a.runStep(ctx, id, innodbBaseScript, env, pr.logln); err != nil {
		return pr.fail("base setup: %v", err)
	}
	a.docker.CopyFile(ctx, id, "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec))
	return nil
}

// depConfig / depSecrets fetch a deployment's stored config/secrets blobs.
func (a *App) depConfig(stackID int64, nodeID string) []byte {
	if dep, err := a.store.GetDeployment(stackID, nodeID); err == nil {
		return dep.Config
	}
	return []byte("{}")
}
func (a *App) depSecrets(stackID int64, nodeID string) []byte {
	if dep, err := a.store.GetDeployment(stackID, nodeID); err == nil {
		return dep.Secrets
	}
	return []byte("{}")
}

// psMajorOfRepo maps a PDPS repo name to a major series for RESET-keyword selection
// (8.4 if the repo mentions 84/8.4, else 8.0).
func psMajorOfRepo(repo string) string {
	if strings.Contains(repo, "84") || strings.Contains(repo, "8.4") || strings.Contains(repo, "9") {
		return "8.4"
	}
	return "8.0"
}

// innodbMyCnf renders /etc/my.cnf. read_only is left to Group Replication (which
// makes secondaries super_read_only in single-primary mode).
func innodbMyCnf(frame designFrame, host, domain, groupName, seedList, mode string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[client]\nsocket=/var/lib/mysql/mysql.sock\n\n[mysqld]\n")
	fmt.Fprintf(&b, "server-id=%d\n", innodbServerID(host))
	fmt.Fprintf(&b, "datadir=/var/lib/mysql\nsocket=/var/lib/mysql/mysql.sock\n")
	fmt.Fprintf(&b, "log-error=%s\npid-file=/var/run/mysqld/mysqld.pid\n", pxcLogError(frame.OS))
	fmt.Fprintf(&b, "bind-address=0.0.0.0\n")
	fmt.Fprintf(&b, "slow_query_log=ON\nslow_query_log_file=/var/lib/mysql/slow.log\nlong_query_time=2\n")
	// GR prerequisites (required for both modes).
	fmt.Fprintf(&b, "gtid_mode=ON\nenforce_gtid_consistency=ON\nlog_bin=binlog\nlog_replica_updates=ON\nbinlog_format=ROW\n")
	if mode == "groupreplication" {
		fmt.Fprintf(&b, "plugin_load_add=group_replication.so\n")
		fmt.Fprintf(&b, "group_replication_group_name=%q\n", groupName)
		fmt.Fprintf(&b, "group_replication_start_on_boot=OFF\n")
		fmt.Fprintf(&b, "group_replication_local_address=%q\n", fqdnOf(host, domain)+":"+strconv.Itoa(grCommPort))
		fmt.Fprintf(&b, "group_replication_group_seeds=%q\n", seedList)
		fmt.Fprintf(&b, "group_replication_single_primary_mode=ON\n")
		fmt.Fprintf(&b, "group_replication_enforce_update_everywhere_checks=OFF\n")
		fmt.Fprintf(&b, "group_replication_ip_allowlist=%q\n", "AUTOMATIC")
		// The recovery channel auths to the donor as the repl user
		// (caching_sha2_password). Without TLS, caching_sha2 refuses unless it can
		// fetch the server's public key — so a joiner's distributed recovery fails
		// with "Authentication requires secure connection". Allow the key fetch.
		fmt.Fprintf(&b, "group_replication_recovery_get_public_key=ON\n")
	}
	return b.String()
}

// innodbFormGroup bootstraps the group on the first member and joins the rest (raw
// Group Replication mode).
func (a *App) innodbFormGroup(ctx context.Context, st Stack, members []designNode, sec pxcSecrets, progs map[string]*pxcProg) error {
	boot := members[0]
	bdep, _ := a.store.GetDeployment(st.ID, boot.ID)
	bp := progs[boot.ID]
	bp.phase("Bootstrapping group", 65)
	env := []string{
		"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
		"APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword,
		"MON_USER=" + sec.MonitorUser, "MON_PW=" + sec.MonitorPassword,
		"CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword,
	}
	if err := a.runStep(ctx, bdep.ContainerID, innodbBootstrapScript, env, bp.logln); err != nil {
		return bp.fail("bootstrap group: %v", err)
	}
	bp.logln("group bootstrapped (primary online); app/monitor/cluster users created")
	for _, n := range members[1:] {
		pr := progs[n.ID]
		dep, _ := a.store.GetDeployment(st.ID, n.ID)
		pr.phase("Joining group", 70)
		if err := a.runStep(ctx, dep.ContainerID, innodbJoinScript, []string{"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword}, pr.logln); err != nil {
			return pr.fail("join group: %v", err)
		}
		pr.logln("joined the group (ONLINE)")
	}
	return nil
}

// innodbCreateCluster builds an InnoDB Cluster via MySQL Shell: create the cluster
// on the first member, create the app/monitor/cluster users, then add the rest.
func (a *App) innodbCreateCluster(ctx context.Context, st Stack, frame designFrame, members []designNode, hosts map[string]string, domain string, sec pxcSecrets, progs map[string]*pxcProg) error {
	boot := members[0]
	bdep, _ := a.store.GetDeployment(st.ID, boot.ID)
	bp := progs[boot.ID]
	bp.phase("Creating InnoDB Cluster (MySQL Shell)", 65)
	// addInstance connects to peers as the 'cluster' admin user, so create the app/
	// monitor/cluster users first; the cluster name + member FQDNs are passed in.
	var others []string
	for _, n := range members[1:] {
		others = append(others, fqdnOf(hosts[n.ID], domain))
	}
	env := []string{
		"CLUSTER=" + sanitizeName(frame.Label),
		"ROOT_PW=" + sec.RootPassword,
		"APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword,
		"MON_USER=" + sec.MonitorUser, "MON_PW=" + sec.MonitorPassword,
		"CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword,
		"PRIMARY_FQDN=" + fqdnOf(hosts[boot.ID], domain),
		"MEMBERS=" + strings.Join(others, ","),
	}
	if err := a.runStep(ctx, bdep.ContainerID, innodbShellClusterScript, env, bp.logln); err != nil {
		return bp.fail("create InnoDB Cluster: %v", err)
	}
	bp.logln("InnoDB Cluster created and instances added")
	return nil
}

// innodbSetupRouter installs/bootstraps MySQL Router on a member. For InnoDB Cluster
// it bootstraps against the cluster metadata; for raw GR it writes a static config
// routing to the members (not primary-aware — a documented limitation).
func (a *App) innodbSetupRouter(ctx context.Context, st Stack, frame designFrame, n designNode, hosts map[string]string, domain, mode string, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	if mode == "innodbcluster" {
		env := []string{"CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword, "PRIMARY_FQDN=" + fqdnOf(hosts[n.ID], domain)}
		if err := a.runStep(ctx, dep.ContainerID, innodbRouterBootstrapScript, env, pr.logln); err != nil {
			return pr.fail("bootstrap MySQL Router: %v", err)
		}
	} else {
		var dests []string
		for _, m := range memberNodes(st, frame) {
			dests = append(dests, fqdnOf(hosts[m.ID], domain)+":3306")
		}
		env := []string{"DESTS=" + strings.Join(dests, ",")}
		if err := a.runStep(ctx, dep.ContainerID, innodbRouterStaticScript, env, pr.logln); err != nil {
			return pr.fail("configure MySQL Router: %v", err)
		}
	}
	pr.logln("MySQL Router running (6446 RW / 6447 RO)")
	return nil
}

// memberNodes returns the design nodes belonging to a frame.
func memberNodes(st Stack, frame designFrame) []designNode {
	var doc designDoc
	json.Unmarshal(st.Design, &doc)
	var out []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "innodb" {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// ------------------------------------------------------------------ scripts

const innodbInstallRHEL = `set -e
dnf -y -q module disable mysql >/dev/null 2>&1 || true
percona-release enable -y "$PDPS_REPO" >/dev/null 2>&1 || percona-release enable "$PDPS_REPO" >/dev/null 2>&1
dnf -y -q install $PKGS >/dev/null`

const innodbInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release enable -y "$PDPS_REPO" >/dev/null 2>&1 || percona-release enable "$PDPS_REPO" >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq $PKGS >/dev/null`

// innodbBaseScript initializes the datadir (if empty), starts mysqld (GR not started
// yet), sets the root password, relaxes validate_password, creates the GR recovery
// user (not binlogged), clears GTID state, and points the recovery channel at it.
//
// In GR mode /etc/my.cnf already carries plugin_load_add=group_replication.so + the
// group_replication_* block. The package's first-start auto-initialize loads that
// plugin and aborts, leaving an empty datadir (mysql.user et al. missing) so mysqld
// then fails to start. Initialize the datadir explicitly with a minimal, GR-free
// defaults file first; the subsequent normal start reads the full my.cnf with the
// system tables already in place. Guarded on an uninitialized datadir, so redeploys
// keep their data.
const innodbBaseScript = `set -e
LOGERR=${LOGERR:-/var/log/mysqld.log}
# Recreate the error log owned by mysql: we delete the package's copy, but /var/log
# is root-owned so the dropped-privilege mysqld (--initialize / normal start runs as
# user=mysql) can't recreate it there ("Could not open file ... Permission denied").
rm -f "$LOGERR" 2>/dev/null || true
install -m 0640 -o mysql -g mysql /dev/null "$LOGERR" 2>/dev/null || { touch "$LOGERR"; chown mysql:mysql "$LOGERR" 2>/dev/null || true; }
# Surface the real [ERROR] line (not the truncated "Shutdown complete" tail) when a
# step fails: mysqld logs are long and runStep only keeps the last 160 chars.
say_err() { echo "$1:"; grep -iE '\[ERROR\]|error' "$LOGERR" /tmp/mysql-init.log 2>/dev/null | grep -viE 'log-error|--log-error' | tail -4; }
install -d -m 0755 -o mysql -g mysql /var/run/mysqld 2>/dev/null || true
if [ ! -d /var/lib/mysql/mysql ]; then
  # Clear everything incl. dotfiles (mysqld --initialize refuses a non-empty datadir).
  find /var/lib/mysql -mindepth 1 -delete 2>/dev/null || true
  printf '[mysqld]\nuser=mysql\ndatadir=/var/lib/mysql\nsocket=/var/lib/mysql/mysql.sock\nlog-error=%s\npid-file=/var/run/mysqld/mysqld.pid\n' "$LOGERR" > /tmp/mysql-init.cnf
  mysqld --defaults-file=/tmp/mysql-init.cnf --initialize-insecure >/tmp/mysql-init.log 2>&1 || { say_err "datadir initialize failed"; exit 1; }
  chown -R mysql:mysql /var/lib/mysql
fi
systemctl reset-failed "$UNIT" 2>/dev/null || true
systemctl start "$UNIT" || { say_err "mysqld failed to start"; exit 1; }
` + mysqlSetRootPW + `
mysql -uroot -p"$ROOT_PW" -e "SET GLOBAL validate_password.policy=LOW; SET GLOBAL validate_password.length=6;" 2>/dev/null || true
mysql -uroot -p"$ROOT_PW" -e "SET sql_log_bin=0; CREATE USER IF NOT EXISTS '$REPL_USER'@'%' IDENTIFIED BY '$REPL_PW'; GRANT REPLICATION SLAVE, BACKUP_ADMIN, CONNECTION_ADMIN ON *.* TO '$REPL_USER'@'%'; GRANT GROUP_REPLICATION_STREAM ON *.* TO '$REPL_USER'@'%';" 2>/dev/null || \
mysql -uroot -p"$ROOT_PW" -e "SET sql_log_bin=0; CREATE USER IF NOT EXISTS '$REPL_USER'@'%' IDENTIFIED BY '$REPL_PW'; GRANT REPLICATION SLAVE, BACKUP_ADMIN ON *.* TO '$REPL_USER'@'%';"
# Cluster admin user on EVERY member (sql_log_bin=0, so it is local — not GTID/
# replicated). InnoDB Cluster mode connects to each joiner as this user for
# configureInstance/addInstance *before* the joiner is cloned, so it must exist
# locally up front (the primary-only copy arrives too late). Harmless for raw GR.
mysql -uroot -p"$ROOT_PW" -e "SET sql_log_bin=0; CREATE USER IF NOT EXISTS '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW'; GRANT ALL PRIVILEGES ON *.* TO '$CLUSTER_USER'@'%' WITH GRANT OPTION;"
mysql -uroot -p"$ROOT_PW" -e "$RESET_CMD" 2>/dev/null || true
mysql -uroot -p"$ROOT_PW" -e "CHANGE REPLICATION SOURCE TO SOURCE_USER='$REPL_USER', SOURCE_PASSWORD='$REPL_PW' FOR CHANNEL 'group_replication_recovery';" 2>/dev/null || true`

// innodbBootstrapScript bootstraps the group on the first member and creates the
// app/monitor/cluster users (which replicate to joiners via GR). Uses /root/.my.cnf.
const innodbBootstrapScript = `set -e
mysql -e "SET GLOBAL group_replication_bootstrap_group=ON; START GROUP_REPLICATION; SET GLOBAL group_replication_bootstrap_group=OFF;"
OK=0
for i in $(seq 1 30); do
  S=$(mysql -N -e "SELECT MEMBER_STATE FROM performance_schema.replication_group_members WHERE MEMBER_HOST=@@hostname" 2>/dev/null)
  [ "$S" = "ONLINE" ] && { OK=1; break; }
  sleep 2
done
[ "$OK" = 1 ] || { echo "group did not come ONLINE:"; mysql -e "SELECT * FROM performance_schema.replication_group_members\G" 2>/dev/null | head -20; exit 1; }
mysql <<SQL
CREATE USER IF NOT EXISTS '$APP_USER'@'%' IDENTIFIED BY '$APP_PW';
GRANT ALL PRIVILEGES ON *.* TO '$APP_USER'@'%';
CREATE USER IF NOT EXISTS '$MON_USER'@'%' IDENTIFIED BY '$MON_PW' WITH MAX_USER_CONNECTIONS 10;
ALTER USER '$MON_USER'@'%' IDENTIFIED BY '$MON_PW';
GRANT SELECT, PROCESS, REPLICATION CLIENT, RELOAD, BACKUP_ADMIN ON *.* TO '$MON_USER'@'%';
GRANT SELECT ON performance_schema.* TO '$MON_USER'@'%';
CREATE USER IF NOT EXISTS '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
ALTER USER '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
GRANT ALL PRIVILEGES ON *.* TO '$CLUSTER_USER'@'%' WITH GRANT OPTION;
FLUSH PRIVILEGES;
SQL`

// innodbJoinScript starts Group Replication on a joining member and waits for ONLINE.
const innodbJoinScript = `set -e
mysql -e "START GROUP_REPLICATION;"
OK=0
for i in $(seq 1 60); do
  S=$(mysql -N -e "SELECT MEMBER_STATE FROM performance_schema.replication_group_members WHERE MEMBER_HOST=@@hostname" 2>/dev/null)
  [ "$S" = "ONLINE" ] && { OK=1; break; }
  sleep 2
done
[ "$OK" = 1 ] || { echo "member did not reach ONLINE:"; mysql -e "SELECT * FROM performance_schema.replication_group_members\G" 2>/dev/null | head -20; exit 1; }`

// innodbShellClusterScript creates an InnoDB Cluster with MySQL Shell on the primary
// and adds the other members (clone recovery). Connects as the 'cluster' user.
// Every Shell call runs with interactive:false so it never blocks on the wizard's
// [y/n] prompt (the exec has no TTY/stdin, so a prompt would hang forever), and under
// `timeout` so a stalled clone/RESTART surfaces as an error instead of hanging the
// deploy. createCluster/addInstance are guarded so the runStep retry loop is
// idempotent (a re-run finds the cluster/member already there instead of erroring).
const innodbShellClusterScript = `set -e
mysql <<SQL
CREATE USER IF NOT EXISTS '$APP_USER'@'%' IDENTIFIED BY '$APP_PW';
GRANT ALL PRIVILEGES ON *.* TO '$APP_USER'@'%';
CREATE USER IF NOT EXISTS '$MON_USER'@'%' IDENTIFIED BY '$MON_PW' WITH MAX_USER_CONNECTIONS 10;
GRANT SELECT, PROCESS, REPLICATION CLIENT ON *.* TO '$MON_USER'@'%';
CREATE USER IF NOT EXISTS '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
GRANT ALL PRIVILEGES ON *.* TO '$CLUSTER_USER'@'%' WITH GRANT OPTION;
FLUSH PRIVILEGES;
SQL
ADMIN="$CLUSTER_USER:$CLUSTER_PW@localhost:3306"
# sh_run TIMEOUT JS: run a MySQL Shell snippet; on success echo its output, on failure
# surface the real error LAST (runStep keeps only the final 160 chars, and mysqlsh
# otherwise ends by echoing the statement, which hides the actual message).
sh_run() {
  if timeout "$1" mysqlsh --uri "$ADMIN" --js -e "$2" >/tmp/sh.log 2>&1; then cat /tmp/sh.log; return 0; fi
  echo "MySQL Shell step failed:"; grep -iE 'ERROR|exception|Dba\.|Cluster\.' /tmp/sh.log | tail -4 || true; return 1
}
# interactive:false makes configureInstance auto-apply required fixes (e.g.
# binlog_transaction_dependency_tracking=WRITESET); without it Shell prompts
# "perform changes? [y/n]" and hangs forever on the no-TTY exec.
sh_run 300 "dba.configureInstance('$ADMIN', {interactive:false, restart:false});"
# Reuse an existing cluster on redeploy; otherwise create fresh. MySQL Shell 8.0.46
# SEGFAULTS in createCluster's "adopt existing GR" path (when a prior attempt left
# Group Replication running with stale/invalid metadata), so first force a clean
# slate — stop any running GR and drop leftover metadata — and let createCluster take
# the working "new group" path. The clean-up is idempotent (errors tolerated).
if timeout 60 mysqlsh --uri "$ADMIN" --js -e "dba.getCluster('$CLUSTER')" >/dev/null 2>&1; then
  echo "cluster '$CLUSTER' already exists"
else
  mysql --force >/dev/null 2>&1 <<'CLEAN' || true
SET GLOBAL super_read_only=OFF;
STOP GROUP_REPLICATION;
SET GLOBAL super_read_only=OFF;
DROP SCHEMA IF EXISTS mysql_innodb_cluster_metadata;
RESET REPLICA ALL FOR CHANNEL 'group_replication_recovery';
CLEAN
  sh_run 300 "dba.createCluster('$CLUSTER');"
fi
IFS=','; for h in $MEMBERS; do
  [ -n "$h" ] || continue
  sh_run 300 "dba.configureInstance('$CLUSTER_USER:$CLUSTER_PW@$h:3306', {interactive:false, restart:false});"
  sh_run 600 "var c=dba.getCluster('$CLUSTER'); try { c.addInstance('$CLUSTER_USER:$CLUSTER_PW@$h:3306', {recoveryMethod:'clone'}); } catch (e) { if (String(e).indexOf('already') < 0) throw e; }"
done`

// innodbRouterBootstrapScript bootstraps MySQL Router against the InnoDB Cluster
// metadata and starts it (RW 6446 / RO 6447).
const innodbRouterBootstrapScript = `set -e
id -u mysqlrouter >/dev/null 2>&1 || useradd -r -s /sbin/nologin mysqlrouter 2>/dev/null || true
install -d -o mysqlrouter -g mysqlrouter /var/lib/mysqlrouter 2>/dev/null || true
mysqlrouter --bootstrap "$CLUSTER_USER:$CLUSTER_PW@$PRIMARY_FQDN:3306" --user=mysqlrouter --force --conf-use-sockets >/tmp/router.log 2>&1 || { echo "router bootstrap failed:"; tail -20 /tmp/router.log; exit 1; }
systemctl enable --now mysqlrouter >/dev/null 2>&1 || systemctl restart mysqlrouter`

// innodbRouterStaticScript writes a static MySQL Router config for raw Group
// Replication (no InnoDB Cluster metadata) routing to the members. Not primary-aware.
const innodbRouterStaticScript = `set -e
id -u mysqlrouter >/dev/null 2>&1 || useradd -r -s /sbin/nologin mysqlrouter 2>/dev/null || true
CONF=/etc/mysqlrouter/mysqlrouter.conf
install -d /etc/mysqlrouter 2>/dev/null || true
cat > "$CONF" <<EOF
[DEFAULT]
user=mysqlrouter
[logger]
level=INFO
[routing:rw]
bind_address=0.0.0.0:6446
destinations=$DESTS
routing_strategy=first-available
protocol=classic
[routing:ro]
bind_address=0.0.0.0:6447
destinations=$DESTS
routing_strategy=round-robin
protocol=classic
EOF
systemctl enable --now mysqlrouter >/dev/null 2>&1 || systemctl restart mysqlrouter`
